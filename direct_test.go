package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsPublicAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.43.0.1", "10.42.1.7", "172.16.0.1", "192.168.0.66", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "255.255.255.255", "198.18.0.1",
		"::1", "::", "fe80::1", "fc00::1", "fd12:3456::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1",
		"64:ff9b::a00:1", "2002:a00:1::", "2001:db8::1",
	}
	for _, s := range blocked {
		if isPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"} {
		if !isPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestProbeDirect_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the guarded client must never reach a loopback server")
	}))
	defer srv.Close()
	withConfig(t, func(c *Config) { c.AllowPrivateURLs = false })

	_, err := probeDirect(context.Background(), srv.URL+"/file.zip")
	if err == nil || !strings.Contains(err.Error(), "private or local network") {
		t.Fatalf("expected a private-network error, got %v", err)
	}
}

func TestDirectClient_BlocksRedirectToPrivate(t *testing.T) {
	// The redirect target is loopback; the first hop is only reachable
	// because the test lifts the guard for it via a custom dial check.
	withConfig(t, func(c *Config) { c.AllowPrivateURLs = false })
	if err := ssrfDialControl("tcp", "127.0.0.1:80", nil); err == nil {
		t.Fatal("dial control must reject loopback")
	}
	if err := ssrfDialControl("tcp", "[::ffff:192.168.1.1]:443", nil); err == nil {
		t.Fatal("dial control must reject IPv4-mapped private addresses")
	}
	if err := ssrfDialControl("tcp", "1.1.1.1:443", nil); err != nil {
		t.Fatalf("public address should pass: %v", err)
	}
}

func TestFilenameFromResponse(t *testing.T) {
	mk := func(rawURL, cd, ct string) *http.Response {
		req, _ := http.NewRequest("GET", rawURL, nil)
		h := http.Header{}
		if cd != "" {
			h.Set("Content-Disposition", cd)
		}
		if ct != "" {
			h.Set("Content-Type", ct)
		}
		return &http.Response{Request: req, Header: h}
	}
	cases := []struct{ url, cd, ct, want string }{
		{"https://x.test/dl?id=1", `attachment; filename="report.pdf"`, "", "report.pdf"},
		{"https://x.test/dl", `attachment; filename*=UTF-8''%C3%BCber%20file.zip`, "", "über file.zip"},
		{"https://x.test/files/My%20Movie.mkv", "", "", "My Movie.mkv"},
		{"https://x.test/files/archive", "", "application/zip", "archive.zip"},
		{"https://x.test/", "", "", "download"},
		{"https://x.test/a", `attachment; filename="../../etc/passwd"`, "", "passwd"},
		{"https://x.test/a", `attachment; filename="..\\..\\boot.ini"`, "", "boot.ini"},
		{"https://x.test/a", `attachment; filename=".hidden"`, "", "hidden"},
	}
	for _, c := range cases {
		if got := filenameFromResponse(mk(c.url, c.cd, c.ct)); got != c.want {
			t.Errorf("url=%s cd=%s: got %q, want %q", c.url, c.cd, got, c.want)
		}
	}
}

func TestClassifyURL(t *testing.T) {
	withConfig(t, func(c *Config) {
		c.DirectDownloads = true
		c.TransmissionURL = "http://transmission:9091/transmission/rpc"
	})
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Ubuntu"
	cases := map[string]string{
		"https://www.youtube.com/watch?v=abc":                  kindYtdlp,
		"https://example.com/file.iso":                         kindDirect,
		"https://example.com/linux.torrent":                    kindTorrent,
		magnet:                                                 kindTorrent,
		"magnet:?xt=urn:btih:ABCDEFGHIJKLMNOPQRSTUVWXYZ234567": kindTorrent,
	}
	for u, want := range cases {
		got, err := classifyURL(u)
		if err != nil || got != want {
			t.Errorf("classifyURL(%q) = %q, %v; want %q", u, got, err, want)
		}
	}
	for _, bad := range []string{"magnet:?dn=nohash", "magnet:?xt=urn:btih:xyz", "ftp://example.com/x", "file:///etc/passwd", "https://open.spotify.com/track/1"} {
		if _, err := classifyURL(bad); err == nil {
			t.Errorf("classifyURL(%q) should fail", bad)
		}
	}

	withConfig(t, func(c *Config) { c.DirectDownloads = false; c.TorrentEngine = engineOff })
	if _, err := classifyURL("https://example.com/file.iso"); err == nil {
		t.Error("direct URLs must be rejected when direct downloads are off")
	}
	if _, err := classifyURL(magnet); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("magnets need Transmission configured, got %v", err)
	}
}

func newDirectTestJob(id string) *Job {
	return &Job{ID: id, Status: "downloading", Kind: kindDirect, subscribers: map[chan struct{}]struct{}{}}
}

func TestTryDownloadDirect_ResumesAfterDrop(t *testing.T) {
	payload := bytes.Repeat([]byte("leecher!"), 64*1024) // 512 KiB
	var hits atomic.Int32
	var sawRange atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		w.Header().Set("Content-Disposition", `attachment; filename="data.bin"`)
		if n == 1 {
			// First attempt: promise the whole file, send half, drop the connection.
			w.Header().Set("Content-Length", "524288")
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
			w.Write(payload[:len(payload)/2])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		if r.Header.Get("Range") != "" {
			sawRange.Store(true)
		}
		http.ServeContent(w, r, "data.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()

	dir := t.TempDir()
	withConfig(t, func(c *Config) {
		c.AllowPrivateURLs = true // httptest listens on loopback
		c.DownloadsDir = dir
		c.MinDiskFreeMB = 0
		c.MaxFileSizeMB = 0
	})
	job := newDirectTestJob("resume1")
	name, display, err := tryDownloadDirect(context.Background(), job, srv.URL+"/data.bin")
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if display != "data.bin" || name != "resume1_data.bin" {
		t.Errorf("unexpected names %q %q", name, display)
	}
	got, _ := os.ReadFile(filepath.Join(dir, name))
	if !bytes.Equal(got, payload) {
		t.Fatalf("file content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if !sawRange.Load() {
		t.Error("the retry should resume with a Range request")
	}
	if _, err := os.Stat(filepath.Join(dir, "resume1")); !os.IsNotExist(err) {
		t.Error("job directory should be cleaned up")
	}
}

func TestTryDownloadDirect_KeepsResumingWhileProgressing(t *testing.T) {
	payload := bytes.Repeat([]byte("leecher!"), 64*1024) // 512 KiB
	const chunk = 64 * 1024                              // 8 drops, more than directMaxTries
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var start int
		if rg := r.Header.Get("Range"); rg != "" {
			fmt.Sscanf(rg, "bytes=%d-", &start)
		}
		end := min(start+chunk, len(payload))
		w.Header().Set("Content-Disposition", `attachment; filename="data.bin"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
		if start > 0 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Write(payload[start:end])
		if end < len(payload) {
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	withConfig(t, func(c *Config) {
		c.AllowPrivateURLs = true
		c.DownloadsDir = dir
		c.MinDiskFreeMB = 0
		c.MaxFileSizeMB = 0
	})
	old := directRetryBase
	directRetryBase = time.Millisecond
	t.Cleanup(func() { directRetryBase = old })

	name, _, err := tryDownloadDirect(context.Background(), newDirectTestJob("prog1"), srv.URL+"/data.bin")
	if err != nil {
		t.Fatalf("download failed after %d requests: %v", hits.Load(), err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, name))
	if !bytes.Equal(got, payload) {
		t.Fatalf("file content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if n := hits.Load(); n <= directMaxTries {
		t.Errorf("expected more than %d requests, got %d", directMaxTries, n)
	}
}

func TestTryDownloadDirect_RejectsPagesAndOversize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/page" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<html></html>"))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(bytes.Repeat([]byte{1}, 3<<20))
	}))
	defer srv.Close()
	dir := t.TempDir()
	withConfig(t, func(c *Config) {
		c.AllowPrivateURLs = true
		c.DownloadsDir = dir
		c.MinDiskFreeMB = 0
		c.MaxFileSizeMB = 1
	})
	if _, _, err := tryDownloadDirect(context.Background(), newDirectTestJob("page1"), srv.URL+"/page"); err == nil || !strings.Contains(err.Error(), "web page") {
		t.Errorf("HTML pages should be rejected, got %v", err)
	}
	if _, _, err := tryDownloadDirect(context.Background(), newDirectTestJob("big1"), srv.URL+"/big.bin"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("oversize files should be rejected, got %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("failed downloads must leave nothing behind, found %d entries", len(entries))
	}
}

func TestTryDownloadDirect_Cancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000000")
		for i := 0; i < 1000; i++ {
			if _, err := w.Write(make([]byte, 64<<10)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	withConfig(t, func(c *Config) {
		c.AllowPrivateURLs = true
		c.DownloadsDir = dir
		c.MinDiskFreeMB = 0
		c.MaxFileSizeMB = 0
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	_, _, err := tryDownloadDirect(ctx, newDirectTestJob("cancel1"), srv.URL+"/slow.bin")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "cancel1")); !os.IsNotExist(statErr) {
		t.Error("cancelled download should remove its job dir")
	}
}

func TestLibraryDirValidationAndMove(t *testing.T) {
	dl := t.TempDir()
	withConfig(t, func(c *Config) { c.DownloadsDir = dl })
	if _, err := validateLibraryDir("relative/path"); err == nil {
		t.Error("relative library paths must be rejected")
	}
	if _, err := validateLibraryDir(filepath.Join(dl, "lib")); err == nil {
		t.Error("a library inside the auto-cleaned downloads dir must be rejected")
	}
	if _, err := validateLibraryDir(dl); err == nil {
		t.Error("the downloads dir itself must be rejected")
	}
	lib := filepath.Join(t.TempDir(), "library")
	got, err := validateLibraryDir(lib + "/")
	if err != nil || got != lib {
		t.Fatalf("valid library dir rejected: %v (%q)", err, got)
	}
	withConfig(t, func(c *Config) { c.LibraryDir = lib })

	for i := 0; i < 2; i++ {
		src := filepath.Join(dl, "job_x.mp4")
		os.WriteFile(src, []byte("video"), 0644)
		dst, err := moveToLibrary(src, "x.mp4")
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(lib, "x.mp4")
		if i == 1 {
			want = filepath.Join(lib, "x (1).mp4")
		}
		if dst != want {
			t.Errorf("move %d: got %q, want %q (existing files must not be overwritten)", i, dst, want)
		}
	}
}

func TestTryDownloadDirect_UsesLibraryDiskWorkArea(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="big.iso"`)
		w.Write(bytes.Repeat([]byte{7}, 1<<20))
	}))
	defer srv.Close()
	dl, lib := t.TempDir(), t.TempDir()
	withConfig(t, func(c *Config) {
		c.AllowPrivateURLs = true
		c.DownloadsDir = dl
		c.LibraryDir = lib
		c.MinDiskFreeMB = 0
		c.MaxFileSizeMB = 0
	})
	job := newDirectTestJob("libjob1")
	name, _, err := tryDownloadDirect(context.Background(), job, srv.URL+"/big.iso")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(lib, workDirName, name)); err != nil {
		t.Fatalf("downloads should land in the library disk's work area: %v", err)
	}
	if entries, _ := os.ReadDir(dl); len(entries) != 0 {
		t.Errorf("the small downloads_dir must not be used when a library exists (found %d entries)", len(entries))
	}
	if findTempFile(name) == "" {
		t.Error("finished file should be found in the work area")
	}

	// Orphaned job folders in the work area are cleaned up.
	os.MkdirAll(filepath.Join(lib, workDirName, "0123456789abcdef"), 0755)
	os.MkdirAll(filepath.Join(lib, workDirName, "0123456789abcdef_Some Album"), 0755)
	cleanupOrphanDirs()
	if _, err := os.Stat(filepath.Join(lib, workDirName, "0123456789abcdef")); !os.IsNotExist(err) {
		t.Error("stale job folder should be removed")
	}
	if _, err := os.Stat(filepath.Join(lib, workDirName, "0123456789abcdef_Some Album")); err != nil {
		t.Error("a finished torrent folder is a download, not an orphan")
	}
}
