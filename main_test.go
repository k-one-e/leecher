package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() {
	// Initialize globals needed by handlers
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slog.SetDefault(logger)
	config = Config{
		Port:                   9090,
		Host:                   "0.0.0.0",
		DownloadsDir:           "./test_downloads",
		MaxFileAgeMinutes:      60,
		DownloadTimeoutMinutes: 10,
		MaxConcurrentDownloads: 4,
		MaxDownloadsPerIP:      1,
		YtdlpPath:              "yt-dlp",
		MinDiskFreeMB:          500,
		RateLimitFormats:       5,
		RateLimitDownloads:     3,
		ProbeConcurrency:       2,
		ProbeQueueMax:          50,
		ProbeTimeoutSecs:       600,
		DownloadQueueMax:       20,
	}
	publishConfig()
	dlSem = newFifoSem(config.MaxConcurrentDownloads)
	probeSem = newFifoSem(2)
	limiter = newRateLimiter()
}

// ---------------------------------------------------------------------------
// Rate limiter tests
// ---------------------------------------------------------------------------

func TestRateLimiter_AllowsBurst(t *testing.T) {
	rl := newRateLimiter()
	for i := 0; i < 5; i++ {
		if !rl.allow("1.2.3.4:test", 1.0/60, 5) {
			t.Fatalf("request %d should have been allowed within burst", i+1)
		}
	}
	// 6th should be denied
	if rl.allow("1.2.3.4:test", 1.0/60, 5) {
		t.Fatal("request beyond burst should be denied")
	}
}

func TestRateLimiter_RefillsOverTime(t *testing.T) {
	rl := newRateLimiter()
	// Exhaust burst
	for i := 0; i < 3; i++ {
		rl.allow("ip:test", 100, 3) // rate=100/s
	}
	if rl.allow("ip:test", 100, 3) {
		t.Fatal("should be denied after burst")
	}
	// Manually advance time by modifying lastCheck
	rl.mu.Lock()
	rl.clients["ip:test"].lastCheck = time.Now().Add(-1 * time.Second)
	rl.mu.Unlock()
	if !rl.allow("ip:test", 100, 3) {
		t.Fatal("should be allowed after refill")
	}
}

func TestRateLimiter_IsolatesIPs(t *testing.T) {
	rl := newRateLimiter()
	for i := 0; i < 3; i++ {
		rl.allow("a:test", 0, 3)
	}
	// IP "a" is exhausted, but "b" should still work
	if !rl.allow("b:test", 0, 3) {
		t.Fatal("different IP should have its own bucket")
	}
}

// ---------------------------------------------------------------------------
// URL validation tests
// ---------------------------------------------------------------------------

func TestAllowedURLs(t *testing.T) {
	valid := []string{
		"https://www.youtube.com/watch?v=abc",
		"https://youtu.be/abc",
		"https://twitter.com/user/status/123",
		"https://www.instagram.com/p/abc",
		"https://www.tiktok.com/@user/video/123",
		"https://www.reddit.com/r/sub/comments/abc",
		"https://soundcloud.com/artist/track",
		"https://vimeo.com/123456",
		"https://x.com/user/status/123",
		// Subdomain variants used natively by these platforms.
		"https://m.youtube.com/watch?v=abc",
		"https://on.soundcloud.com/xyz",
		"https://old.reddit.com/r/sub/comments/abc",
		"https://player.vimeo.com/video/123456",
	}
	for _, u := range valid {
		if !allowedRe.MatchString(u) {
			t.Errorf("URL should be allowed: %s", u)
		}
	}
}

func TestDisallowedURLs(t *testing.T) {
	invalid := []string{
		"https://evil.com/malware",
		"ftp://youtube.com/watch",
		"https://notyoutube.com/video",
		"javascript:alert(1)",
		"",
		// Bandcamp was removed from the allowlist.
		"https://exodus.bandcamp.com/track/3111",
		"https://bandcamp.com/",
		// Host-confusion attempts that must not match.
		"https://youtube.com.attacker.com/",
		"https://evil.com/youtube.com/",
	}
	for _, u := range invalid {
		if allowedRe.MatchString(u) {
			t.Errorf("URL should be rejected: %s", u)
		}
	}
}

func TestValidateSourceURL_DRM(t *testing.T) {
	cases := []struct {
		url    string
		substr string // required substring in the error message
	}{
		{"https://open.spotify.com/track/abc", "Spotify"},
		{"https://music.apple.com/us/album/x", "Apple Music"},
		{"https://tidal.com/browse/track/1", "Tidal"},
		{"https://www.netflix.com/watch/123", "Netflix"},
	}
	for _, c := range cases {
		err := validateSourceURL(c.url)
		if err == nil {
			t.Errorf("expected error for %s", c.url)
			continue
		}
		if !strings.Contains(err.Error(), c.substr) {
			t.Errorf("error for %s should mention %q, got %q", c.url, c.substr, err.Error())
		}
	}
}

func TestValidateSourceURL_Malformed(t *testing.T) {
	cases := []string{"", "   ", "not a url", "javascript:alert(1)", "ftp://youtube.com/x"}
	for _, u := range cases {
		if err := validateSourceURL(u); err == nil {
			t.Errorf("expected error for %q", u)
		}
	}
}

func TestValidateSourceURL_Unsupported(t *testing.T) {
	err := validateSourceURL("https://example.com/foo")
	if err == nil {
		t.Fatal("expected error for unsupported host")
	}
	if !strings.Contains(err.Error(), "Supported platforms") {
		t.Errorf("error should list supported platforms, got: %s", err.Error())
	}
}

func TestValidateSourceURL_Allowed(t *testing.T) {
	ok := []string{
		"https://www.youtube.com/watch?v=abc",
		"https://soundcloud.com/artist/track",
	}
	for _, u := range ok {
		if err := validateSourceURL(u); err != nil {
			t.Errorf("should accept %s, got: %s", u, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Format ID tests
// ---------------------------------------------------------------------------

func TestIsSimpleFormatID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"401", true},
		{"140", true},
		{"0", true},
		{"bv*+ba/b", false},
		{"ba", false},
		{"", false},
		{"abc", false},
		{"140+bestaudio", false},
	}
	for _, c := range cases {
		if got := isSimpleFormatID(c.id); got != c.want {
			t.Errorf("isSimpleFormatID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Progress regex tests
// ---------------------------------------------------------------------------

func TestProgressRegex(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"[download]  45.3% of 100MiB", "45.3"},
		{"[download] 100% of 50MiB", "100"},
		{"[download]   0.5% of 1GiB", "0.5"},
		{"no progress here", ""},
	}
	for _, c := range cases {
		matches := progressRe.FindStringSubmatch(c.line)
		got := ""
		if len(matches) > 1 {
			got = matches[1]
		}
		if got != c.want {
			t.Errorf("progressRe(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Format sorting tests
// ---------------------------------------------------------------------------

func TestSortFormats(t *testing.T) {
	formats := []Format{
		{FormatID: "1", Label: "Audio", Height: 0, HasVideo: false, HasAudio: true},
		{FormatID: "2", Label: "480p", Height: 480, HasVideo: true, HasAudio: true},
		{FormatID: "3", Label: "1080p", Height: 1080, HasVideo: true, HasAudio: true},
		{FormatID: "4", Label: "720p", Height: 720, HasVideo: true, HasAudio: false},
	}
	sortFormats(formats)

	if formats[0].Height != 1080 {
		t.Errorf("first format should be 1080p, got %dp", formats[0].Height)
	}
	if formats[1].Height != 720 {
		t.Errorf("second format should be 720p, got %dp", formats[1].Height)
	}
}

// ---------------------------------------------------------------------------
// HTTP handler tests
// ---------------------------------------------------------------------------

func TestHandleIndex_ServesHTML(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handleIndex(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("expected text/html content type, got %s", ct)
	}
	if !strings.Contains(w.Body.String(), "Leecher") {
		t.Error("index page should contain 'Leecher'")
	}
}

func TestHandleIndex_404ForOtherPaths(t *testing.T) {
	req := httptest.NewRequest("GET", "/nonexistent", nil)
	w := httptest.NewRecorder()
	handleIndex(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleFormats_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/formats", nil)
	w := httptest.NewRecorder()
	handleFormats(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHandleFormats_EmptyURL(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/formats", strings.NewReader(`{"url":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleFormats(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleFormats_DisallowedURL(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/formats", strings.NewReader(`{"url":"https://evil.com/video"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleFormats(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if !strings.Contains(resp["error"], "isn't supported") {
		t.Errorf("expected unsupported URL error, got: %s", resp["error"])
	}
}

func TestHandleDownload_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/download", nil)
	w := httptest.NewRecorder()
	handleDownload(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHandleDownload_DisallowedURL(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/download", strings.NewReader(`{"url":"https://evil.com/file"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleDownload(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleStatus_MissingJob(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/status/nonexistent", nil)
	w := httptest.NewRecorder()
	handleStatus(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleStatus_ExistingJob(t *testing.T) {
	job := &Job{
		ID:          "testjob1",
		Status:      "downloading",
		Progress:    42.5,
		Message:     "Downloading...",
		Token:       "tok1",
		CreatedAt:   time.Now(),
		subscribers: make(map[chan struct{}]struct{}),
	}
	jobsMu.Lock()
	jobs["testjob1"] = job
	jobsMu.Unlock()
	defer func() {
		jobsMu.Lock()
		delete(jobs, "testjob1")
		jobsMu.Unlock()
	}()

	req := httptest.NewRequest("GET", "/api/status/testjob1?token=tok1", nil)
	w := httptest.NewRecorder()
	handleStatus(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "downloading" {
		t.Errorf("expected status downloading, got %v", resp["status"])
	}
	if resp["progress"].(float64) != 42.5 {
		t.Errorf("expected progress 42.5, got %v", resp["progress"])
	}
}

func TestHandleCancel_MissingJob(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/cancel/nonexistent", nil)
	w := httptest.NewRecorder()
	handleCancel(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Security headers tests
// ---------------------------------------------------------------------------

func TestSecurityHeaders(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	checks := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for header, want := range checks {
		got := w.Header().Get(header)
		if got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") {
		t.Error("CSP header missing or malformed")
	}
}

// ---------------------------------------------------------------------------
// CSRF tests
// ---------------------------------------------------------------------------

func TestCSRFValidation_RejectsEmpty(t *testing.T) {
	req := httptest.NewRequest("POST", "/admin/logout", nil)
	if validateCSRF(req) {
		t.Error("CSRF validation should fail without cookie")
	}
}

func TestCSRFValidation_RejectsWrongToken(t *testing.T) {
	// Create a session
	token := generateSessionToken()
	sessionsMu.Lock()
	sessions[sessionKey(token)] = &session{
		User:      "admin",
		Expires:   time.Now().Add(1 * time.Hour),
		CSRFToken: "correct-csrf-token",
	}
	sessionsMu.Unlock()
	defer func() {
		sessionsMu.Lock()
		delete(sessions, sessionKey(token))
		sessionsMu.Unlock()
	}()

	req := httptest.NewRequest("POST", "/admin/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.Header.Set("X-CSRF-Token", "wrong-token")
	if validateCSRF(req) {
		t.Error("CSRF validation should fail with wrong token")
	}
}

func TestCSRFValidation_AcceptsCorrectToken(t *testing.T) {
	token := generateSessionToken()
	sessionsMu.Lock()
	sessions[sessionKey(token)] = &session{
		User:      "admin",
		Expires:   time.Now().Add(1 * time.Hour),
		CSRFToken: "my-csrf-token",
	}
	sessionsMu.Unlock()
	defer func() {
		sessionsMu.Lock()
		delete(sessions, sessionKey(token))
		sessionsMu.Unlock()
	}()

	req := httptest.NewRequest("POST", "/admin/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.Header.Set("X-CSRF-Token", "my-csrf-token")
	if !validateCSRF(req) {
		t.Error("CSRF validation should pass with correct token")
	}
}

// ---------------------------------------------------------------------------
// Admin auth tests
// ---------------------------------------------------------------------------

// withConfig mutates the global config for the duration of one test and
// republishes the snapshot that cfg() serves.
func withConfig(t *testing.T, fn func(c *Config)) {
	t.Helper()
	orig := config
	fn(&config)
	publishConfig()
	t.Cleanup(func() {
		config = orig
		publishConfig()
	})
}

func TestAdminRedirectsUnauthenticated(t *testing.T) {
	req := httptest.NewRequest("GET", "/admin", nil)
	w := httptest.NewRecorder()
	handleAdmin(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/login?next=%2Fadmin" {
		t.Errorf("expected redirect to the login page, got %s", loc)
	}
}

// ---------------------------------------------------------------------------
// Rate limiting integration test
// ---------------------------------------------------------------------------

func TestFormatsEndpoint_RateLimited(t *testing.T) {
	rl := newRateLimiter()
	oldLimiter := limiter
	limiter = rl
	withConfig(t, func(c *Config) {
		c.ProbeQueueMax = 1
		c.RateLimitFormats = 1 // burst = max(pqMax, rlFormats) = 1
	})
	defer func() { limiter = oldLimiter }()

	// First request consumes the only burst token
	req := httptest.NewRequest("POST", "/api/formats", strings.NewReader(`{"url":"https://youtube.com/watch?v=test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	handleFormats(w, req)

	// Second request should be rate limited
	req = httptest.NewRequest("POST", "/api/formats", strings.NewReader(`{"url":"https://youtube.com/watch?v=test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:12345"
	w = httptest.NewRecorder()
	handleFormats(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Job history tests
// ---------------------------------------------------------------------------

func TestAppendJobHistory(t *testing.T) {
	// Save and restore
	origHistory := jobHistory
	withConfig(t, func(c *Config) { c.DownloadsDir = t.TempDir() })
	jobHistory = nil
	defer func() {
		jobHistory = origHistory
	}()

	rec := JobRecord{
		ID:     "test1",
		URL:    "https://youtube.com/watch?v=test",
		Status: "done",
	}
	appendJobHistory(rec)

	jobHistoryMu.Lock()
	l := len(jobHistory)
	jobHistoryMu.Unlock()
	if l != 1 {
		t.Fatalf("expected 1 history entry, got %d", l)
	}

	// Verify it was persisted
	loadJobHistory()
	jobHistoryMu.Lock()
	l = len(jobHistory)
	jobHistoryMu.Unlock()
	if l != 1 {
		t.Fatalf("expected 1 history entry after reload, got %d", l)
	}
}

// ---------------------------------------------------------------------------
// Client IP extraction tests
// ---------------------------------------------------------------------------

func TestClientIP_XForwardedForIgnoredFromUntrustedPeer(t *testing.T) {
	trustedProxies = nil
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	req.RemoteAddr = "127.0.0.1:1234"
	if ip := clientIP(req); ip != "127.0.0.1" {
		t.Errorf("expected 127.0.0.1 (spoofed header must be ignored), got %s", ip)
	}
}

func TestClientIP_XForwardedForFromTrustedProxy(t *testing.T) {
	trustedProxies = parseTrustedProxies([]string{"127.0.0.1"})
	defer func() { trustedProxies = nil }()

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	req.RemoteAddr = "127.0.0.1:1234"
	// Right-most entry wins: everything left of it is attacker-controlled.
	if ip := clientIP(req); ip != "5.6.7.8" {
		t.Errorf("expected 5.6.7.8, got %s", ip)
	}
}

func TestClientIP_SkipsChainedTrustedProxies(t *testing.T) {
	trustedProxies = parseTrustedProxies([]string{"127.0.0.1", "5.6.7.0/24"})
	defer func() { trustedProxies = nil }()

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	req.RemoteAddr = "127.0.0.1:1234"
	if ip := clientIP(req); ip != "1.2.3.4" {
		t.Errorf("expected 1.2.3.4, got %s", ip)
	}
}

func TestClientIP_RemoteAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:9999"
	if ip := clientIP(req); ip != "10.0.0.5" {
		t.Errorf("expected 10.0.0.5, got %s", ip)
	}
}

// ---------------------------------------------------------------------------
// Job notify subscribers test
// ---------------------------------------------------------------------------

func TestJobNotifySubscribers(t *testing.T) {
	job := &Job{
		subscribers: make(map[chan struct{}]struct{}),
	}
	ch := make(chan struct{}, 1)
	job.subscribers[ch] = struct{}{}

	job.notifySubscribers()

	select {
	case <-ch:
		// ok
	default:
		t.Error("subscriber should have been notified")
	}
}

// ---------------------------------------------------------------------------
// fifoSem tests
// ---------------------------------------------------------------------------

func TestFifoSem_ImmediateGrant(t *testing.T) {
	s := newFifoSem(2)
	w1, t1 := s.enqueue()
	w2, t2 := s.enqueue()
	if t1 != 1 || t2 != 2 {
		t.Fatalf("expected tickets 1,2 got %d,%d", t1, t2)
	}
	// Both should be granted immediately (2 slots)
	select {
	case <-w1.ch:
	default:
		t.Fatal("w1 should be granted immediately")
	}
	select {
	case <-w2.ch:
	default:
		t.Fatal("w2 should be granted immediately")
	}
	// Third should queue
	w3, _ := s.enqueue()
	select {
	case <-w3.ch:
		t.Fatal("w3 should NOT be granted immediately")
	default:
	}
	// Position should be 1
	if pos := s.position(w3); pos != 1 {
		t.Fatalf("expected position 1, got %d", pos)
	}
	// Release one slot — w3 should be granted
	s.release()
	select {
	case <-w3.ch:
	default:
		t.Fatal("w3 should be granted after release")
	}
	s.release()
	s.release()
}

func TestFifoSem_FIFOOrder(t *testing.T) {
	s := newFifoSem(1)
	w1, _ := s.enqueue() // gets the slot immediately
	<-w1.ch

	// Queue 3 more
	w2, _ := s.enqueue()
	w3, _ := s.enqueue()
	w4, _ := s.enqueue()

	if s.position(w2) != 1 || s.position(w3) != 2 || s.position(w4) != 3 {
		t.Fatalf("expected positions 1,2,3 got %d,%d,%d", s.position(w2), s.position(w3), s.position(w4))
	}

	// Release should grant to w2 (first in queue)
	s.release()
	select {
	case <-w2.ch:
	default:
		t.Fatal("w2 should be granted")
	}
	select {
	case <-w3.ch:
		t.Fatal("w3 should NOT be granted yet")
	default:
	}

	s.release()
	<-w3.ch
	s.release()
	<-w4.ch
	s.release()
}

func TestFifoSem_Cancel(t *testing.T) {
	s := newFifoSem(1)
	w1, _ := s.enqueue()
	<-w1.ch // w1 occupies the slot

	w2, _ := s.enqueue() // queued
	w3, _ := s.enqueue() // queued

	// Cancel w2 (should succeed — still in queue)
	if !s.cancel(w2) {
		t.Fatal("cancel(w2) should return true")
	}
	// w3 should now be at position 1
	if pos := s.position(w3); pos != 1 {
		t.Fatalf("expected w3 at position 1, got %d", pos)
	}
	// Cancelling w2 again should return false (already removed)
	if s.cancel(w2) {
		t.Fatal("cancel(w2) again should return false")
	}

	// Release slot — should go to w3 (w2 was cancelled)
	s.release()
	select {
	case <-w3.ch:
	default:
		t.Fatal("w3 should be granted after w2 cancelled")
	}
	select {
	case <-w2.ch:
		t.Fatal("w2 should NOT receive after cancel")
	default:
	}
	s.release()
}

func TestFifoSem_CancelAlreadyGranted(t *testing.T) {
	s := newFifoSem(2)
	w1, _ := s.enqueue()
	<-w1.ch // granted immediately
	// cancel returns false because w1 was never in the waiters list
	if s.cancel(w1) {
		t.Fatal("cancel should return false for an already-granted waiter")
	}
	s.release()
}

func TestFifoSem_ResizeUp(t *testing.T) {
	s := newFifoSem(1)
	w1, _ := s.enqueue()
	<-w1.ch

	w2, _ := s.enqueue() // queued
	w3, _ := s.enqueue() // queued

	// Resize from 1 to 3 — should grant w2 and w3 immediately
	s.resize(3)
	select {
	case <-w2.ch:
	default:
		t.Fatal("w2 should be granted after resize up")
	}
	select {
	case <-w3.ch:
	default:
		t.Fatal("w3 should be granted after resize up")
	}
	s.release()
	s.release()
	s.release()
}

func TestFifoSem_ResizeDownAbsorbsExcessSlots(t *testing.T) {
	s := newFifoSem(3)
	// Acquire all 3 slots
	w1, _ := s.enqueue()
	w2, _ := s.enqueue()
	w3, _ := s.enqueue()
	<-w1.ch
	<-w2.ch
	<-w3.ch

	// Resize down to 1 while all 3 are active
	s.resize(1)

	// Release all 3 — only 1 free slot should result, not 3
	s.release()
	s.release()
	s.release()

	// Now enqueue 2 — only the first should get a slot
	w4, _ := s.enqueue()
	w5, _ := s.enqueue()
	select {
	case <-w4.ch:
	default:
		t.Fatal("w4 should be granted (1 free slot)")
	}
	select {
	case <-w5.ch:
		t.Fatal("w5 should NOT be granted (max is 1, slot occupied by w4)")
	default:
	}
	if pos := s.position(w5); pos != 1 {
		t.Fatalf("w5 should be at position 1, got %d", pos)
	}
	s.release() // release w4's slot, should grant w5
	<-w5.ch
	s.release()
}

func TestFifoSem_ReleaseUnderLockPreventsDoubleRelease(t *testing.T) {
	// This test verifies that cancel() correctly returns false when a waiter
	// has been popped from the queue by release(), because the channel send
	// now happens under the lock (no race window).
	s := newFifoSem(1)
	w1, _ := s.enqueue()
	<-w1.ch

	w2, _ := s.enqueue() // queued at position 1

	// Release grants to w2 under the lock
	s.release()
	// Now cancel(w2) should return false (already granted)
	if s.cancel(w2) {
		t.Fatal("cancel should return false — waiter was already granted by release")
	}
	// w2 should have received the grant
	select {
	case <-w2.ch:
	default:
		t.Fatal("w2 should have been granted")
	}
	s.release()
}

// ---------------------------------------------------------------------------
// Job authorization tests
// ---------------------------------------------------------------------------

func newTestJob(t *testing.T) *Job {
	t.Helper()
	job := &Job{
		ID:          generateID(),
		Status:      "queued",
		Token:       generateSessionToken(),
		CreatedAt:   time.Now(),
		cancel:      func() {},
		subscribers: make(map[chan struct{}]struct{}),
	}
	jobsMu.Lock()
	jobs[job.ID] = job
	jobsMu.Unlock()
	t.Cleanup(func() {
		jobsMu.Lock()
		delete(jobs, job.ID)
		jobsMu.Unlock()
	})
	return job
}

func TestHandleStatus_RequiresToken(t *testing.T) {
	job := newTestJob(t)

	w := httptest.NewRecorder()
	handleStatus(w, httptest.NewRequest("GET", "/api/status/"+job.ID, nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status without token: expected 404, got %d", w.Code)
	}

	w = httptest.NewRecorder()
	handleStatus(w, httptest.NewRequest("GET", "/api/status/"+job.ID+"?token="+job.Token, nil))
	if w.Code != http.StatusOK {
		t.Errorf("status with token: expected 200, got %d", w.Code)
	}
}

func TestHandleCancel_RequiresToken(t *testing.T) {
	job := newTestJob(t)

	w := httptest.NewRecorder()
	handleCancel(w, httptest.NewRequest("POST", "/api/cancel/"+job.ID, nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("cancel without token: expected 404, got %d", w.Code)
	}
	job.mu.Lock()
	status := job.Status
	job.mu.Unlock()
	if status == "cancelled" {
		t.Fatal("job was cancelled by an unauthenticated request")
	}

	req := httptest.NewRequest("POST", "/api/cancel/"+job.ID, nil)
	req.Header.Set("X-Job-Token", job.Token)
	w = httptest.NewRecorder()
	handleCancel(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("cancel with token: expected 200, got %d", w.Code)
	}
}

func TestGenerateIDIsRandom(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := generateID()
		if len(id) != 16 {
			t.Fatalf("unexpected id length %d for %q", len(id), id)
		}
		if seen[id] {
			t.Fatalf("duplicate job id generated: %s", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// Per-IP semaphore pruning
// ---------------------------------------------------------------------------

func TestPruneIPSems(t *testing.T) {
	ipDlSemsMu.Lock()
	ipDlSems = make(map[string]*ipSemEntry)
	ipDlSemsMu.Unlock()

	idle := getIPDownloadSem("10.0.0.1")
	_ = idle
	busy := getIPDownloadSem("10.0.0.2")
	w, _ := busy.enqueue() // occupies a slot
	<-w.ch

	pruneIPSems(0)

	ipDlSemsMu.Lock()
	_, idleKept := ipDlSems["10.0.0.1"]
	_, busyKept := ipDlSems["10.0.0.2"]
	ipDlSemsMu.Unlock()

	if idleKept {
		t.Error("idle per-IP semaphore should have been pruned")
	}
	if !busyKept {
		t.Error("in-use per-IP semaphore must not be pruned")
	}
	busy.release()
}

// ---------------------------------------------------------------------------
// Config snapshot
// ---------------------------------------------------------------------------

// TestConfigSnapshotIsRaceFree fails under -race if a reader touches the
// `config` global instead of the published snapshot.
func TestConfigSnapshotIsRaceFree(t *testing.T) {
	withConfig(t, func(c *Config) { c.MaxFileSizeMB = 0 })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = cfg().MaxFileSizeMB
				_ = cookieArgs()
			}
		}
	}()

	for i := 1; i <= 200; i++ {
		configMu.Lock()
		config.MaxFileSizeMB = i
		config.CookiesFile = ""
		publishConfig()
		configMu.Unlock()
	}
	close(stop)
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Admin policy tests
// ---------------------------------------------------------------------------

func TestValidateAdminPassword(t *testing.T) {
	for _, bad := range []string{"", "short", "1234567", "admin", "password", "ChangeMe"} {
		if err := validateAdminPassword(bad); err == nil {
			t.Errorf("password %q should have been rejected", bad)
		}
	}
	for _, good := range []string{"correct-horse", "8charsss"} {
		if err := validateAdminPassword(good); err != nil {
			t.Errorf("password %q should have been accepted: %v", good, err)
		}
	}
}

func TestInvalidateUserSessions(t *testing.T) {
	sessionsMu.Lock()
	sessions = map[string]*session{
		sessionKey("keep"):  {User: "a", Expires: time.Now().Add(time.Hour)},
		sessionKey("drop"):  {User: "A", Expires: time.Now().Add(time.Hour)},
		sessionKey("other"): {User: "b", Expires: time.Now().Add(time.Hour)},
	}
	sessionsMu.Unlock()

	invalidateUserSessions("a", "keep")

	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	if _, ok := sessions[sessionKey("drop")]; ok {
		t.Error("other sessions of the same user must be dropped after a password change")
	}
	if _, ok := sessions[sessionKey("keep")]; !ok {
		t.Error("the caller's own session should survive")
	}
	if _, ok := sessions[sessionKey("other")]; !ok {
		t.Error("sessions of other users must not be touched")
	}
}

func TestAdminWriteRateLimited(t *testing.T) {
	oldLimiter := limiter
	limiter = newRateLimiter()
	defer func() { limiter = oldLimiter }()

	req := httptest.NewRequest("POST", "/api/admin/delete/x", nil)
	req.RemoteAddr = "10.9.9.9:1111"

	allowed := 0
	for i := 0; i < 40; i++ {
		w := httptest.NewRecorder()
		if adminWriteAllowed(w, req) {
			allowed++
		} else if w.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 when throttled, got %d", w.Code)
		}
	}
	if allowed == 0 || allowed >= 40 {
		t.Fatalf("expected a burst then throttling, got %d/40 allowed", allowed)
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asset.bin")
	payload := []byte("pretend this is yt-dlp")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	good := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/good":
			fmt.Fprintf(w, "%s  other_asset\n%s  asset.bin\n", strings.Repeat("0", 64), good)
		case "/bad":
			fmt.Fprintf(w, "%s  asset.bin\n", strings.Repeat("f", 64))
		default:
			fmt.Fprintf(w, "%s  unrelated\n", good)
		}
	}))
	defer srv.Close()

	if err := verifyChecksum(path, srv.URL+"/good", "asset.bin"); err != nil {
		t.Errorf("matching checksum should verify: %v", err)
	}
	if err := verifyChecksum(path, srv.URL+"/bad", "asset.bin"); err == nil {
		t.Error("mismatched checksum must fail")
	}
	if err := verifyChecksum(path, srv.URL+"/missing", "asset.bin"); err == nil {
		t.Error("missing checksum entry must fail")
	}
}

func TestIsPlaylistZipHost(t *testing.T) {
	cases := map[string]bool{
		"https://soundcloud.com/artist/sets/album":                         true,
		"https://music.youtube.com/playlist?list=OLAK5uy_mvacNSxINvJ0muaQ": true,
		"https://music.youtube.com/playlist?list=PLabc":                    true,
		"https://www.youtube.com/playlist?list=PLabc":                      false,
		"https://youtube.com/playlist?list=OLAK5uy_abc":                    false,
		"https://notmusic.youtube.com.evil.example/playlist?list=x":        false,
	}
	for u, want := range cases {
		if got := isPlaylistZipHost(u); got != want {
			t.Errorf("isPlaylistZipHost(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestPlaylistTitleFromLine(t *testing.T) {
	cases := map[string]string{
		"[youtube:tab] Playlist Album - Roozhaye Taraneho Andooh - Persian Music: Downloading 9 items of 9": "Album - Roozhaye Taraneho Andooh - Persian Music",
		"[bandcamp] Playlist Best Of Foo:":                                "Best Of Foo",
		"[soundcloud:set] Playlist Live: Tokyo: Downloading 3 items of 3": "Live: Tokyo",
		"[download] Downloading item 1 of 9":                              "",
	}
	for line, want := range cases {
		if got := playlistTitleFromLine(line); got != want {
			t.Errorf("playlistTitleFromLine(%q) = %q, want %q", line, got, want)
		}
	}
}
