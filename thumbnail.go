package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Thumbnails are proxied rather than hot-linked. Two reasons: the CSP stays at
// `img-src 'self'`, and the browser never talks to YouTube's CDN — which is the
// whole point of a self-hosted downloader.
//
// The client can only ask for an opaque id minted during a format probe, never
// a URL, so this cannot be turned into an SSRF primitive.

const (
	thumbTTL          = 30 * time.Minute
	thumbMaxBytes     = 5 << 20 // 5 MB
	thumbFetchTimeout = 15 * time.Second
	maxThumbEntries   = 500
)

type thumbEntry struct {
	url     string
	expires time.Time
}

var (
	thumbStore = make(map[string]thumbEntry)
	thumbMu    sync.Mutex
)

// rememberThumbnail stores a source URL and returns the id the client may ask
// for. Returns "" when the URL is unusable.
func rememberThumbnail(rawURL string) string {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return ""
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	id := hex.EncodeToString(b)

	thumbMu.Lock()
	defer thumbMu.Unlock()
	if len(thumbStore) >= maxThumbEntries {
		pruneThumbnailsLocked()
	}
	thumbStore[id] = thumbEntry{url: rawURL, expires: time.Now().Add(thumbTTL)}
	return id
}

func lookupThumbnail(id string) (string, bool) {
	thumbMu.Lock()
	defer thumbMu.Unlock()
	e, ok := thumbStore[id]
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.url, true
}

func pruneThumbnails() {
	thumbMu.Lock()
	defer thumbMu.Unlock()
	pruneThumbnailsLocked()
}

func pruneThumbnailsLocked() {
	now := time.Now()
	for id, e := range thumbStore {
		if now.After(e.expires) {
			delete(thumbStore, id)
		}
	}
}

// pickThumbnail chooses the image to preview: yt-dlp's own pick when it offers
// one, otherwise the widest candidate that isn't absurdly large.
func pickThumbnail(primary string, candidates []struct {
	URL        string `json:"url"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Preference int    `json:"preference"`
}) string {
	if primary != "" {
		return primary
	}
	best, bestWidth := "", -1
	for _, c := range candidates {
		if c.URL == "" || c.Width > 1920 {
			continue
		}
		if c.Width > bestWidth {
			best, bestWidth = c.URL, c.Width
		}
	}
	if best == "" && len(candidates) > 0 {
		best = candidates[len(candidates)-1].URL
	}
	return best
}

func handleThumbnail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/thumbnail/")
	if id == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ip := clientIP(r)
	if !limiter.allow(ip+":thumb", 1.0, 60) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	src, ok := lookupThumbnail(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if blocked, _ := killSwitchBlocked(); blocked {
		http.Error(w, "paused by the kill switch", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), thumbFetchTimeout)
	defer cancel()
	ctx, ksCancel := withKillSwitch(ctx)
	defer ksCancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		http.Error(w, "bad thumbnail", http.StatusBadGateway)
		return
	}
	resp, err := thumbClient.Do(req)
	if err != nil {
		logger.Warn("Thumbnail fetch failed", "error", err)
		http.Error(w, "thumbnail unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") {
		http.Error(w, "thumbnail unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=1800")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	io.Copy(w, io.LimitReader(resp.Body, thumbMaxBytes))
}

var thumbClient = &http.Client{
	Timeout: thumbFetchTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		return nil
	},
}
