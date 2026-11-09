package main

// direct.go — plain http(s) file downloads for URLs yt-dlp does not handle.
//
// Every connection goes through ssrfDialControl, which inspects the address
// actually being dialled (after DNS, on every redirect hop), so a user cannot
// make leecher fetch the cluster network, the node, or anything on the LAN.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	urlpkg "net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxURLLen       = 10 * 1024 // magnets carry long tracker lists
	directUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 leecher"
	directMaxTries  = 5 // consecutive attempts without progress
)

var directRetryBase = 2 * time.Second

var (
	errBlockedAddress = errors.New("destination is on a private or local network")
	errDirectTooLarge = errors.New("file exceeds the size limit")
	errDirectCanceled = errors.New("cancelled")
)

// blockedPrefixes covers special-purpose ranges that netip's Is* helpers do
// not, including IPv6 transition prefixes that embed an IPv4 address.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32",
		"2002::/16", "fec0::/10",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func isPublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// ssrfDialControl runs after name resolution, right before connect(), so it
// sees the real destination even for DNS-rebinding hosts and redirects.
func ssrfDialControl(_, address string, _ syscall.RawConn) error {
	if cfg().AllowPrivateURLs {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errBlockedAddress
	}
	a, err := netip.ParseAddr(host)
	if err != nil || !isPublicAddr(a) {
		return fmt.Errorf("%w (%s)", errBlockedAddress, host)
	}
	return nil
}

func newDirectClient() *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: ssrfDialControl}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil, // a proxy would dial on our behalf and bypass the guard
			DialContext:           dialer.DialContext,
			// HTTP/1.1 only: hosts that RST_STREAM a long HTTP/2 transfer poison the shared connection.
			TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       60 * time.Second,
			MaxIdleConns:          8,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

var directClient = newDirectClient()

// friendlyNetError turns transport errors into something a user can act on.
func friendlyNetError(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, errBlockedAddress):
		return "That address points to a private or local network, which is not allowed."
	case errors.As(err, &dnsErr):
		return "Could not resolve host " + dnsErr.Name + "."
	case errors.Is(err, context.DeadlineExceeded):
		return "The server took too long to respond."
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && len(msg)-i < 120 {
		msg = msg[i+2:]
	}
	return "Could not reach the server: " + msg
}

type directInfo struct {
	URL         string // after redirects
	Filename    string
	Size        int64 // -1 when unknown
	ContentType string
}

func isHTMLContentType(ct string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	return mt == "text/html" || mt == "application/xhtml+xml"
}

var extByType = map[string]string{
	"application/zip": ".zip", "application/x-7z-compressed": ".7z", "application/x-rar-compressed": ".rar",
	"application/vnd.rar": ".rar", "application/gzip": ".gz", "application/x-tar": ".tar",
	"application/pdf": ".pdf", "application/x-iso9660-image": ".iso", "application/x-bittorrent": ".torrent",
	"video/mp4": ".mp4", "video/x-matroska": ".mkv", "video/webm": ".webm", "video/quicktime": ".mov",
	"audio/mpeg": ".mp3", "audio/flac": ".flac", "audio/ogg": ".ogg", "audio/mp4": ".m4a",
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
	"text/plain": ".txt", "application/json": ".json",
}

// sanitizeDirectName makes a server-supplied name safe as a single path
// component while keeping its extension intact.
func sanitizeDirectName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
		case strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(strings.TrimSpace(b.String()), ".")
	if out == "" {
		out = "download"
	}
	const maxLen = 150
	if len(out) > maxLen {
		ext := filepath.Ext(out)
		if len(ext) > 16 {
			ext = ""
		}
		base := out[:maxLen-len(ext)]
		for !utf8.ValidString(base) {
			base = base[:len(base)-1]
		}
		out = base + ext
	}
	return out
}

func filenameFromResponse(resp *http.Response) string {
	name := ""
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			name = params["filename"] // ParseMediaType also decodes filename*
		}
	}
	if name == "" && resp.Request != nil && resp.Request.URL != nil {
		if base := path.Base(resp.Request.URL.Path); base != "/" && base != "." {
			if un, err := urlpkg.PathUnescape(base); err == nil {
				base = un
			}
			name = base
		}
	}
	name = sanitizeDirectName(path.Base(strings.ReplaceAll(name, `\`, "/")))
	if filepath.Ext(name) == "" {
		mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		name += extByType[mt]
	}
	return name
}

// totalSizeOf reads the full size from a 200 or 206 response.
func totalSizeOf(resp *http.Response) int64 {
	if resp.StatusCode == http.StatusPartialContent {
		cr := resp.Header.Get("Content-Range") // bytes 0-0/12345
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				return n
			}
		}
		return -1
	}
	return resp.ContentLength
}

func newDirectRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", directUserAgent)
	req.Header.Set("Accept", "*/*")
	return req, nil
}

// probeDirect learns name, size and type without downloading the body. Many
// servers mishandle HEAD, so a one-byte ranged GET is the fallback.
func probeDirect(ctx context.Context, rawURL string) (*directInfo, error) {
	req, err := newDirectRequest(ctx, http.MethodHead, rawURL)
	if err != nil {
		return nil, errors.New("Please enter a valid http(s) URL.")
	}
	resp, err := directClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if err != nil || resp.StatusCode >= 400 {
		if err != nil && errors.Is(err, errBlockedAddress) {
			return nil, errors.New(friendlyNetError(err))
		}
		req, _ = newDirectRequest(ctx, http.MethodGet, rawURL)
		req.Header.Set("Range", "bytes=0-0")
		resp, err = directClient.Do(req)
		if err != nil {
			return nil, errors.New(friendlyNetError(err))
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("The server answered HTTP %d.", resp.StatusCode)
		}
	}
	return &directInfo{
		URL:         resp.Request.URL.String(),
		Filename:    filenameFromResponse(resp),
		Size:        totalSizeOf(resp),
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

func humanBytes(n int64) string {
	switch {
	case n < 0:
		return "unknown size"
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

const directPageError = "This link opens a web page, not a file. Leecher can download direct file links and videos from supported sites."

// handleDirectFormats answers the format probe for a direct URL with a single
// "original file" option, using the same SSE protocol as the yt-dlp probe.
func handleDirectFormats(w http.ResponseWriter, r *http.Request, rawURL string) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	info, err := probeDirect(ctx, rawURL)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMLContentType(info.ContentType) {
		jsonError(w, directPageError, http.StatusBadRequest)
		return
	}
	if limit := int64(cfg().MaxFileSizeMB) << 20; limit > 0 && info.Size > limit {
		jsonError(w, fmt.Sprintf("File is %s, larger than the %d MB limit.", humanBytes(info.Size), cfg().MaxFileSizeMB), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	host := ""
	if u, err := urlpkg.Parse(info.URL); err == nil {
		host = u.Hostname()
	}
	ext := strings.TrimPrefix(filepath.Ext(info.Filename), ".")
	label := "Original file"
	if ext != "" {
		label += " · ." + ext
	}
	label += " · " + humanBytes(info.Size)
	writeSSEJSON(w, "media", map[string]any{"title": info.Filename, "uploader": host + " · direct download"})
	writeSSEJSON(w, "formats", map[string]any{"formats": []Format{{FormatID: "direct", Label: label, Ext: ext, Filesize: info.Size}}})
}

func writeSSEJSON(w http.ResponseWriter, event string, v any) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// directProgress throttles job updates to twice a second.
type directProgress struct {
	job       *Job
	total     int64
	lastAt    time.Time
	lastBytes int64
	speed     float64
}

func (p *directProgress) update(written int64, force bool) error {
	now := time.Now()
	if !force && now.Sub(p.lastAt) < 500*time.Millisecond {
		return nil
	}
	if dt := now.Sub(p.lastAt).Seconds(); !p.lastAt.IsZero() && dt > 0 {
		inst := float64(written-p.lastBytes) / dt
		if p.speed == 0 {
			p.speed = inst
		} else {
			p.speed = 0.7*p.speed + 0.3*inst
		}
	}
	p.lastAt, p.lastBytes = now, written

	p.job.mu.Lock()
	defer p.job.mu.Unlock()
	if p.job.Status == "cancelled" {
		return errDirectCanceled
	}
	msg := "Downloading... " + humanBytes(written)
	if p.total > 0 {
		msg += " / " + humanBytes(p.total)
		if pct := float64(written) / float64(p.total) * 98; pct > p.job.Progress {
			p.job.Progress = pct
		}
	}
	if p.speed > 0 {
		msg += fmt.Sprintf(" · %s/s", humanBytes(int64(p.speed)))
		if p.total > 0 && written < p.total {
			eta := time.Duration(float64(p.total-written)/p.speed) * time.Second
			msg += " · " + eta.Round(time.Second).String() + " left"
		}
	}
	p.job.Message = msg
	p.job.notifySubscribers()
	return nil
}

func copyWithProgress(ctx context.Context, dst io.Writer, src io.Reader, written *int64, limit int64, prog *directProgress) error {
	buf := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if limit > 0 && *written+int64(n) > limit {
				return errDirectTooLarge
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			*written += int64(n)
			if err := prog.update(*written, false); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// tryDownloadDirect fetches rawURL into the downloads root, resuming with
// Range requests after dropped connections when the server supports it.
func tryDownloadDirect(ctx context.Context, job *Job, rawURL string) (string, string, error) {
	base := jobWorkDir(job)
	jobDir := filepath.Join(base, job.ID)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return "", "", fmt.Errorf("cannot create job dir: %w", err)
	}
	fail := func(err error) (string, string, error) {
		os.RemoveAll(jobDir)
		return "", "", err
	}
	partPath := filepath.Join(jobDir, "download.part")
	limit := int64(cfg().MaxFileSizeMB) << 20
	prog := &directProgress{job: job, total: -1}
	var name string
	var written int64
	stalls := 0

	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			directClient.CloseIdleConnections()
		}
		before := written
		req, err := newDirectRequest(ctx, http.MethodGet, rawURL)
		if err != nil {
			return fail(errors.New("invalid URL"))
		}
		if written > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", written))
		}
		resp, err := directClient.Do(req)
		if err != nil {
			stalls++
			if ctx.Err() != nil || errors.Is(err, errBlockedAddress) || stalls >= directMaxTries {
				if ctx.Err() == context.DeadlineExceeded {
					return fail(fmt.Errorf("Timed out after %d minute(s).", cfg().DirectTimeoutMinutes))
				}
				if ctx.Err() != nil {
					return fail(errDirectCanceled)
				}
				return fail(errors.New(friendlyNetError(err)))
			}
			if !sleepCtx(ctx, time.Duration(stalls)*directRetryBase) {
				return fail(errDirectCanceled)
			}
			continue
		}

		flags := os.O_CREATE | os.O_WRONLY
		switch {
		case written > 0 && resp.StatusCode == http.StatusPartialContent:
			flags |= os.O_APPEND
		case resp.StatusCode == http.StatusOK:
			flags |= os.O_TRUNC // fresh start, or the server ignored our Range
			written = 0
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && prog.total > 0 && written >= prog.total:
			resp.Body.Close()
			goto finished
		default:
			resp.Body.Close()
			stalls++
			if resp.StatusCode >= 500 && stalls < directMaxTries {
				if !sleepCtx(ctx, time.Duration(stalls)*directRetryBase) {
					return fail(errDirectCanceled)
				}
				continue
			}
			return fail(fmt.Errorf("The server answered HTTP %d.", resp.StatusCode))
		}

		if name == "" {
			if isHTMLContentType(resp.Header.Get("Content-Type")) {
				resp.Body.Close()
				return fail(errors.New(directPageError))
			}
			name = filenameFromResponse(resp)
		}
		if prog.total < 0 {
			if t := totalSizeOf(resp); t >= 0 {
				if resp.StatusCode == http.StatusOK {
					prog.total = t
				} else if resp.Header.Get("Content-Range") != "" {
					prog.total = t
				}
			}
			if limit > 0 && prog.total > limit {
				resp.Body.Close()
				return fail(fmt.Errorf("File is %s, larger than the %d MB limit.", humanBytes(prog.total), cfg().MaxFileSizeMB))
			}
			if minFree := int64(cfg().MinDiskFreeMB); prog.total > 0 && minFree > 0 {
				if free, err := diskFreeMBAt(base); err == nil && free-prog.total>>20 < minFree {
					resp.Body.Close()
					return fail(fmt.Errorf("Not enough disk space: the file needs %s and only %d MB are free.", humanBytes(prog.total), free))
				}
			}
		}

		f, err := os.OpenFile(partPath, flags, 0644)
		if err != nil {
			resp.Body.Close()
			return fail(fmt.Errorf("cannot write file: %w", err))
		}
		copyErr := copyWithProgress(ctx, f, resp.Body, &written, limit, prog)
		resp.Body.Close()
		if cerr := f.Close(); copyErr == nil && cerr != nil {
			copyErr = cerr
		}

		switch {
		case copyErr == nil && (prog.total <= 0 || written >= prog.total):
			goto finished
		case errors.Is(copyErr, errDirectTooLarge):
			return fail(fmt.Errorf("File is larger than the %d MB limit.", cfg().MaxFileSizeMB))
		case errors.Is(copyErr, errDirectCanceled) || errors.Is(copyErr, context.Canceled):
			return fail(errDirectCanceled)
		case errors.Is(copyErr, context.DeadlineExceeded):
			return fail(fmt.Errorf("Timed out after %d minute(s).", cfg().DirectTimeoutMinutes))
		}
		// written can shrink if the server ignored Range, so only a real gain resets the budget.
		if written > before {
			stalls = 0
		}
		stalls++
		if stalls >= directMaxTries {
			if copyErr == nil {
				copyErr = io.ErrUnexpectedEOF
			}
			return fail(fmt.Errorf("Download kept getting interrupted (%v).", copyErr))
		}
		job.mu.Lock()
		job.Message = fmt.Sprintf("Connection dropped at %s, resuming (attempt %d)...", humanBytes(written), attempt+1)
		job.notifySubscribers()
		job.mu.Unlock()
		if !sleepCtx(ctx, time.Duration(stalls)*directRetryBase) {
			return fail(errDirectCanceled)
		}
	}

finished:
	prog.update(written, true)
	finalName := job.ID + "_" + name
	dst := filepath.Join(base, finalName)
	if err := os.Rename(partPath, dst); err != nil {
		return fail(fmt.Errorf("failed to move file: %w", err))
	}
	now := time.Now()
	os.Chtimes(dst, now, now)
	os.RemoveAll(jobDir)
	return finalName, name, nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
