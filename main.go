package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	urlpkg "net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/crypto/bcrypt"
)

//go:embed static templates config.default.json
var embedded embed.FS

// ---------------------------------------------------------------------------
// SSE broadcaster for admin file updates
// ---------------------------------------------------------------------------

var (
	sseClients   = make(map[chan struct{}]struct{})
	sseClientsMu sync.Mutex
)

func notifyFileChange() {
	invalidateDirSizeCache()
	sseClientsMu.Lock()
	defer sseClientsMu.Unlock()
	for ch := range sseClients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// ---------------------------------------------------------------------------
// Job
// ---------------------------------------------------------------------------

type Job struct {
	ID          string      `json:"id"`
	Status      string      `json:"status"` // downloading, done, error, cancelled
	Progress    float64     `json:"progress"`
	Message     string      `json:"message"`
	Filename    string      `json:"filename,omitempty"`
	DisplayName string      `json:"display_name,omitempty"`
	Error       string      `json:"error,omitempty"`
	Warning     string      `json:"warning,omitempty"` // finished, but incomplete (e.g. playlist tracks missing)
	URL         string      `json:"url,omitempty"`
	FormatID    string      `json:"-"` // original selection, before format fallbacks are expanded
	CreatedAt   time.Time   `json:"created_at"`
	Logs        []string    `json:"-"`
	Token       string      `json:"-"` // bearer secret returned once to the submitter
	Owner       string      `json:"owner,omitempty"`
	Title       string      `json:"title,omitempty"`       // as shown in the UI when started
	Kind        string      `json:"kind,omitempty"`        // ytdlp | direct
	Destination string      `json:"destination,omitempty"` // device | library
	LibraryPath string      `json:"library_path,omitempty"`
	FinishedAt  time.Time   `json:"finished_at,omitempty"`
	Hidden      bool        `json:"-"` // dismissed from the user's activity list
	StopReason  string      `json:"-"` // why a "cancelled" job was stopped when not by its user
	UserPaused  bool        `json:"-"` // torrent: paused by its user (not by the kill switch)
	Peers       int         `json:"-"` // torrent: connected peers
	Seeding     bool        `json:"-"` // torrent: finished and still uploading
	RateUp      int64       `json:"-"`
	Ratio       float64     `json:"-"`
	workDir     string      // fixed when the job starts; see jobWorkDir
	Ticket      int32       // display-only ticket number
	IP          string      // client IP for per-IP concurrency gating
	ipWaiter    *fifoWaiter // per-IP FIFO waiter; non-nil while waiting for per-IP slot
	waiter      *fifoWaiter // global FIFO semaphore waiter; non-nil while in global queue
	cancel      context.CancelFunc
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{} // WebSocket notification channels
}

// notifySubscribers wakes all WebSocket listeners for this job.
func (j *Job) notifySubscribers() {
	for ch := range j.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type Format struct {
	FormatID string `json:"format_id"`
	Label    string `json:"label"`
	Height   int    `json:"height"`
	HasVideo bool   `json:"has_video"`
	HasAudio bool   `json:"has_audio"`
	Ext      string `json:"ext"`
	Filesize int64  `json:"filesize"`
}

// ---------------------------------------------------------------------------
// Job history (persistent)
// ---------------------------------------------------------------------------

type JobRecord struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	FormatID    string    `json:"format_id,omitempty"`
	Status      string    `json:"status"`
	Filename    string    `json:"filename,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	Error       string    `json:"error,omitempty"`
	Warning     string    `json:"warning,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	Title       string    `json:"title,omitempty"`
	Kind        string    `json:"kind,omitempty"`
	Destination string    `json:"destination,omitempty"`
	LibraryPath string    `json:"library_path,omitempty"`
	Hidden      bool      `json:"hidden,omitempty"` // dismissed from the activity list; admin history keeps it
	CreatedAt   time.Time `json:"created_at"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
}

var (
	jobHistory   []JobRecord
	jobHistoryMu sync.Mutex
)

func jobHistoryPath() string {
	return filepath.Join(cfg().DownloadsDir, ".job_history.json")
}

func loadJobHistory() {
	data, err := os.ReadFile(jobHistoryPath())
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("Could not read job history", "error", err)
		}
		return
	}
	jobHistoryMu.Lock()
	defer jobHistoryMu.Unlock()
	if err := json.Unmarshal(data, &jobHistory); err != nil {
		logger.Warn("Corrupt job history, starting fresh", "error", err)
		jobHistory = nil
	}
}

// persistJobHistoryLocked marshals the current jobHistory and writes it to
// disk. Must be called with jobHistoryMu held; the mutex is released before
// the disk write so the I/O does not block other history operations.
func persistJobHistoryLocked() {
	data, err := json.Marshal(jobHistory)
	jobHistoryMu.Unlock()
	if err != nil {
		logger.Warn("Could not marshal job history", "error", err)
		return
	}
	if err := os.WriteFile(jobHistoryPath(), data, 0644); err != nil {
		logger.Warn("Could not write job history", "error", err)
	}
}

func saveJobHistory() {
	jobHistoryMu.Lock()
	persistJobHistoryLocked() // unlocks before disk write
}

func appendJobHistory(rec JobRecord) {
	jobHistoryMu.Lock()
	jobHistory = append(jobHistory, rec)
	// Keep last 200 entries (lighter for SBCs)
	if len(jobHistory) > 200 {
		jobHistory = jobHistory[len(jobHistory)-200:]
	}
	persistJobHistoryLocked() // unlocks before disk write
}

// ---------------------------------------------------------------------------
// Security headers middleware
// ---------------------------------------------------------------------------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				// No inline scripts anywhere: every handler lives in /static/*.js.
				"script-src 'self'; "+
				// Inline style attributes are still used for layout in the admin page.
				"style-src 'self' 'unsafe-inline'; "+
				"font-src 'self'; "+
				"img-src 'self' data:; "+
				"connect-src 'self' ws: wss:; "+
				"base-uri 'none'; "+
				"form-action 'self'; "+
				"object-src 'none'; "+
				"frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Globals
// ---------------------------------------------------------------------------

var (
	config     Config
	configPath string
	configMu   sync.RWMutex
	// configSnapshot is a read-only copy of config, republished on every
	// mutation. Readers outside a configMu section must go through cfg().
	configSnapshot  atomic.Pointer[Config]
	jobs            = make(map[string]*Job)
	jobsMu          sync.RWMutex
	dlSem           *fifoSem                       // FIFO-ordered concurrency semaphore for downloads
	probeSem        *fifoSem                       // FIFO-ordered concurrency semaphore for format probes
	probeWaiting    atomic.Int32                   // probe requests in queue (waiting + active)
	downloadWaiting atomic.Int32                   // download requests in queue (waiting + active)
	downloadTicket  atomic.Int32                   // monotonic ticket counter for display
	ipDlSems        = make(map[string]*ipSemEntry) // per-IP download concurrency semaphores
	ipDlSemsMu      sync.Mutex
	trustedProxies  []*net.IPNet // parsed once at startup from config.TrustedProxies
	progressRe      = regexp.MustCompile(`(\d+\.?\d*)%`)
	allowedRe       = regexp.MustCompile(`^https?://([a-z0-9-]+\.)*(` + videoHostPattern() + `)(/|$)`)

	// playlistZipHosts are the hosts for which we offer "download entire
	// album/playlist as a ZIP of MP3 files". Kept to audio-first platforms
	// on purpose: zipping arbitrary YouTube playlists would trivially blow
	// past any disk budget. music.youtube.com is matched exactly, so plain
	// youtube.com video playlists stay refused.
	playlistZipHosts = []string{"soundcloud.com", "music.youtube.com"}

	// playlistMaxEntries caps the number of tracks we will pull in one
	// playlist-zip job — abuse/disk-usage guard.
	playlistMaxEntries = 100

	// supportedPlatforms is the user-facing list shown when an unsupported
	// URL is rejected, derived from the same table the footer badges use.
	supportedPlatforms = platformNames()

	// drmBlocklist maps host substrings to a human-readable reason. These
	// services either serve DRM-protected streams (Widevine/FairPlay) or
	// only expose metadata, so yt-dlp cannot produce usable audio/video.
	// Returned as a specific, actionable error instead of the generic
	// "unsupported" message.
	drmBlocklist = map[string]string{
		"spotify.com":      "Spotify streams are DRM-protected and cannot be downloaded. For music, try SoundCloud or YouTube Music.",
		"music.apple.com":  "Apple Music is DRM-protected and cannot be downloaded.",
		"tidal.com":        "Tidal is DRM-protected and cannot be downloaded.",
		"deezer.com":       "Deezer streams are DRM-protected (only 30-second previews are public).",
		"amazon.com/music": "Amazon Music is DRM-protected and cannot be downloaded.",
		"music.amazon":     "Amazon Music is DRM-protected and cannot be downloaded.",
		"netflix.com":      "Netflix is DRM-protected and cannot be downloaded.",
		"disneyplus.com":   "Disney+ is DRM-protected and cannot be downloaded.",
		"hbomax.com":       "HBO Max is DRM-protected and cannot be downloaded.",
		"max.com":          "Max is DRM-protected and cannot be downloaded.",
		"primevideo.com":   "Prime Video is DRM-protected and cannot be downloaded.",
	}
	limiter    *rateLimiter
	logger     *slog.Logger
	shutdownCh = make(chan struct{}) // closed on graceful shutdown to stop background goroutines

	// Pre-rendered HTML pages (populated once at startup to avoid per-request allocations)
	indexHTML  []byte
	loginHTML  string
	adminHTML  string
	faviconSVG []byte
)

func main() {
	ensureClassicTorrentIO()

	// Structured logging
	logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Handle --hash-password before anything else
	if len(os.Args) == 3 && os.Args[1] == "--hash-password" {
		hash, err := bcrypt.GenerateFromPassword([]byte(os.Args[2]), bcrypt.DefaultCost)
		if err != nil {
			logger.Error("Failed to hash password", "error", err)
			os.Exit(1)
		}
		fmt.Println(string(hash))
		os.Exit(0)
	}
	runUserCLI(os.Args)

	loadConfig()
	initUsers()
	loadSessions()
	loadTorrentOwners()
	defer func() {
		if btClient != nil {
			btClient.Close()
		}
	}()
	ensureYtdlp()
	ensureNodejs()
	detectJsRuntimes()
	loadJobHistory()
	loadTransmissionStopped()
	// Pre-render index page once (avoid string replacement per request)
	if data, err := embedded.ReadFile("templates/index.html"); err == nil {
		page := strings.ReplaceAll(string(data), "<!--APP_VERSION-->", versionHTML())
		indexHTML = []byte(strings.ReplaceAll(page, "<!--PLATFORMS-->", platformSummaryHTML()))
	}
	if data, err := embedded.ReadFile("templates/login.html"); err == nil {
		loginHTML = strings.ReplaceAll(string(data), "<!--APP_VERSION-->", versionHTML())
	}
	if data, err := embedded.ReadFile("templates/admin.html"); err == nil {
		adminHTML = strings.ReplaceAll(string(data), "<!--APP_VERSION-->", versionHTML())
	}
	if data, err := embedded.ReadFile("static/favicon.svg"); err == nil {
		faviconSVG = data
	}

	dlSem = newFifoSem(config.MaxConcurrentDownloads)
	probeSem = newFifoSem(config.ProbeConcurrency)
	limiter = newRateLimiter()

	removeLegacyStaging()
	if err := os.MkdirAll(tempDir(), 0775); err != nil {
		logger.Warn("Cannot create working dir", "dir", tempDir(), "error", err)
	}
	if err := os.MkdirAll(config.DownloadsDir, 0755); err != nil {
		logger.Error("Cannot create downloads dir", "error", err)
		os.Exit(1)
	}

	// Start background goroutines
	if cfg().KillSwitch {
		// Verify before resuming torrents, so nothing reconnects from a wrong IP.
		killSwitchCheck(context.Background())
	}
	resumeTorrents()
	go killSwitchLoop()
	go cleanupLoop()
	go diskIOLoop()
	go cleanupDiskBenchFiles()
	go currentEgress(context.Background(), false) // warm the cache for the first page load
	go ytdlpAutoUpdateLoop()

	mux := http.NewServeMux()

	// Serve embedded static files
	staticFS, _ := fs.Sub(embedded, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Serve favicon
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		if len(faviconSVG) > 0 {
			w.Write(faviconSVG)
			return
		}
		// Fallback for tests or if startup preload failed
		data, _ := embedded.ReadFile("static/favicon.svg")
		w.Write(data)
	})

	// Serve downloads directory. Non-admins only get files from their own jobs.
	mux.Handle("/downloads/", http.StripPrefix("/downloads/",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clean := filepath.Clean(r.URL.Path)
			if clean != filepath.Base(clean) || clean == ".." || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, ".") {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if !canAccessFile(r, clean) {
				http.NotFound(w, r)
				return
			}
			path := findTempFile(clean)
			if path == "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(clean)+"\"")
			http.ServeFile(w, r, path)
		}),
	))

	// Auth, health and account routes
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", handleHealthz)
	mux.HandleFunc("/login", handleLogin)
	mux.HandleFunc("/logout", handleLogout)
	mux.HandleFunc("/change-password", handleChangePassword)
	mux.HandleFunc("/account", handleAccountPage)
	mux.HandleFunc("/api/account", handleAccount)
	mux.HandleFunc("/api/account/", handleAccount)

	// API routes
	mux.HandleFunc("/api/formats", handleFormats)
	mux.HandleFunc("/api/thumbnail/", handleThumbnail)
	mux.HandleFunc("/api/download", handleDownload)
	mux.HandleFunc("/api/status/", handleStatus)
	mux.HandleFunc("/api/cancel/", handleCancel)
	mux.HandleFunc("/ws/status/", handleWSStatus)
	mux.HandleFunc("/api/egress", handleEgress)
	mux.HandleFunc("/api/disk", handleDisk)
	mux.HandleFunc("/api/jobs", handleJobs)
	mux.HandleFunc("/api/jobs/", handleJobAction)
	mux.HandleFunc("/api/files/", handleJobFile)
	mux.HandleFunc("/api/torrents", handleTorrents)
	mux.HandleFunc("/api/torrents/", handleTorrents)

	// Admin routes
	mux.HandleFunc("/admin", handleAdmin)
	mux.HandleFunc("/admin/login", handleLegacyAdminLogin)
	mux.HandleFunc("/admin/logout", handleLogout)
	mux.HandleFunc("/api/admin/files", handleAdminFiles)
	mux.HandleFunc("/api/admin/delete/", handleAdminDelete)
	mux.HandleFunc("/api/admin/settings", handleAdminSettings)
	mux.HandleFunc("/api/admin/users", handleAdminUsers)
	mux.HandleFunc("/api/admin/users/", handleAdminUsers)
	mux.HandleFunc("/api/admin/transmission-test", handleAdminTransmissionTest)
	mux.HandleFunc("/api/admin/killswitch", handleAdminKillSwitch)
	mux.HandleFunc("/api/admin/disk-bench", handleAdminDiskBench)
	mux.HandleFunc("/api/admin/cookies", handleAdminCookies)
	mux.HandleFunc("/api/admin/history", handleAdminHistory)
	mux.HandleFunc("/api/admin/update-ytdlp", handleAdminUpdateYtdlp)
	mux.HandleFunc("/api/admin/events", handleAdminEvents)

	// Index
	mux.HandleFunc("/", handleIndex)

	addr := fmt.Sprintf("%s:%d", config.Host, config.Port)
	server := &http.Server{
		Addr:           addr,
		Handler:        securityHeaders(requireAuth(mux)),
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   5 * time.Minute,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 16, // 64KB — protect against oversized headers
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		logger.Info("Shutting down...")
		close(shutdownCh) // signal background goroutines to stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	logger.Info("Leecher running", "addr", "http://"+addr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		logger.Error("Server error", "error", err)
		os.Exit(1)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page := indexHTML
	if len(page) == 0 {
		// Fallback: render on the fly (only during tests or if pre-render failed)
		data, err := embedded.ReadFile("templates/index.html")
		if err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		page = []byte(strings.ReplaceAll(string(data), "<!--APP_VERSION-->", versionHTML()))
	}

	var csrf, username, role string
	if a := currentAuth(r); a != nil {
		username, role = a.User.Username, a.User.Role
		if !a.ViaToken {
			csrf = csrfTokenForSession(a.SessionToken)
		}
	}
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return ""
	}
	c := cfg()
	out := strings.NewReplacer(
		"<!--CSRF_TOKEN-->", htmlpkg.EscapeString(csrf),
		"<!--USERNAME-->", htmlpkg.EscapeString(username),
		"<!--IS_ADMIN-->", flag(role == roleAdmin),
		"<!--LIBRARY-->", flag(c.LibraryDir != ""),
		"<!--TORRENTS-->", flag(torrentsEnabled()),
		"<!--TRANSMISSION-->", flag(transmissionEnabled()),
		"<!--DIRECT-->", flag(c.DirectDownloads),
		"<!--LIBRARY_PATH-->", htmlpkg.EscapeString(c.LibraryDir),
		"<!--FILE_AGE-->", strconv.Itoa(c.MaxFileAgeMinutes),
		"<!--VIDEO_HOSTS-->", strings.Join(videoHosts, " "),
		"<!--ROLE-->", htmlpkg.EscapeString(role),
	).Replace(string(page))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(out))
}

// parseTrustedProxies converts the configured CIDRs / bare IPs into networks.
// Anything unparseable is logged and skipped rather than failing startup.
func parseTrustedProxies(entries []string) []*net.IPNet {
	var nets []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(e); err == nil {
			nets = append(nets, n)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		logger.Warn("Ignoring invalid trusted_proxies entry", "entry", e)
	}
	return nets
}

func isTrustedProxy(ip string) bool {
	if len(trustedProxies) == 0 {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range trustedProxies {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// clientIP resolves the caller's address. X-Forwarded-For is only consulted
// when the immediate peer is a configured trusted proxy — otherwise any client
// could forge the header and bypass every per-IP limit in the service. The
// right-most entry that is not itself a trusted proxy is the real client.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isTrustedProxy(host) {
		return host
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		if candidate == "" {
			continue
		}
		if net.ParseIP(candidate) == nil {
			continue
		}
		if isTrustedProxy(candidate) {
			continue
		}
		return candidate
	}
	return host
}

// isPlaylistZipHost reports whether the given URL's host is one we allow
// "download entire playlist as a ZIP of MP3s" for. Kept narrow to audio-
// focused platforms to prevent accidental multi-gigabyte video zips.
func isPlaylistZipHost(raw string) bool {
	u, err := urlpkg.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Host)
	for _, h := range playlistZipHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// URL kinds decide which engine handles a download.
const (
	kindYtdlp   = "ytdlp"
	kindDirect  = "direct"
	kindTorrent = "torrent"
)

// classifyURL checks a user-supplied URL and picks the engine for it, or
// returns a specific, actionable error:
//
//   - magnet link / .torrent URL -> Transmission (when configured)
//   - known-DRM host             -> a specific reason ("Spotify is DRM-protected …")
//   - supported platform         -> yt-dlp
//   - any other http(s) URL      -> direct file download (when enabled)
//   - otherwise                  -> the full list of supported platforms
func classifyURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("No URL provided.")
	}
	if len(raw) > maxURLLen {
		return "", fmt.Errorf("URL is too long.")
	}
	if strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		if !isValidMagnet(raw) {
			return "", fmt.Errorf("That magnet link has no valid info hash (xt=urn:btih:…).")
		}
		if !torrentsEnabled() {
			return "", fmt.Errorf("Torrent downloads are not configured on this server.")
		}
		return kindTorrent, nil
	}
	u, err := urlpkg.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("Please enter a valid http(s) URL.")
	}
	host := strings.ToLower(u.Host)
	pathLower := strings.ToLower(u.Path)
	hostAndPath := host + pathLower

	// DRM / known-unsupported services get a specific explanation.
	for needle, reason := range drmBlocklist {
		if strings.Contains(hostAndPath, needle) {
			return "", fmt.Errorf("%s", reason)
		}
	}

	if allowedRe.MatchString(raw) {
		return kindYtdlp, nil
	}
	if strings.HasSuffix(pathLower, ".torrent") && torrentsEnabled() {
		return kindTorrent, nil
	}
	if cfg().DirectDownloads {
		return kindDirect, nil
	}
	return "", fmt.Errorf("%s isn't supported. Supported platforms: %s.", host, supportedPlatforms)
}

// validateSourceURL reports whether raw is acceptable for any engine.
func validateSourceURL(raw string) error {
	_, err := classifyURL(raw)
	return err
}

func handleFormats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := requestActor(r)
	// Rate limit format probes per minute per user (or IP).
	// Burst allows queuing up to probe_queue_max at once; the per-minute
	// rate still protects against sustained abuse.
	configMu.RLock()
	rlFormats := config.RateLimitFormats
	pqMax := config.ProbeQueueMax
	configMu.RUnlock()
	if rlFormats <= 0 {
		rlFormats = 5
	}
	if pqMax < rlFormats {
		pqMax = rlFormats
	}
	if !limiter.allow(ip+":formats", float64(rlFormats)/60.0, float64(pqMax)) {
		logger.Warn("Rate limited", "ip", ip, "endpoint", "formats")
		jsonError(w, "Too many requests. Please wait a moment.", http.StatusTooManyRequests)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxURLLen+4096)
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		jsonError(w, "No URL provided", http.StatusBadRequest)
		return
	}

	url := strings.TrimSpace(req.URL)
	kind, err := classifyURL(url)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := killSwitchGate(r.Context()); err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	ksCtx, ksCancel := withKillSwitch(r.Context())
	defer ksCancel()
	r = r.WithContext(ksCtx)
	switch kind {
	case kindTorrent:
		handleTorrentFormats(w, r, url)
		return
	case kindDirect:
		handleDirectFormats(w, r, url)
		return
	}

	configMu.RLock()
	probeTimeout := time.Duration(config.ProbeTimeoutSecs) * time.Second
	configMu.RUnlock()
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	// SSE streaming for real-time probe logs
	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	clearWriteDeadline(w)

	sendEvent := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if canFlush {
			flusher.Flush()
		}
	}

	configMu.RLock()
	debugProbe := config.DebugMode
	configMu.RUnlock()

	// Queue with cap: reject if probe_queue_max requests already in flight (active + waiting).
	configMu.RLock()
	probeMaxTotal := int32(config.ProbeQueueMax)
	configMu.RUnlock()
	for {
		cur := probeWaiting.Load()
		if cur >= probeMaxTotal {
			sendEvent("error", `{"error":"Server is overloaded. Please try again in a moment."}`)
			return
		}
		if probeWaiting.CompareAndSwap(cur, cur+1) {
			break
		}
	}
	// FIFO-enqueue: assigns a ticket and queues atomically under one lock,
	// so serve order is strictly determined by HTTP arrival order.
	probeWaiter, myTicket := probeSem.enqueue()
	defer func() {
		probeWaiting.Add(-1)
	}()

	// Wait for slot, sending live queue position every second via SSE.
	queueTicker := time.NewTicker(time.Second)
	defer queueTicker.Stop()
waitLoop:
	for {
		select {
		case <-probeWaiter.ch:
			// Slot granted — proceed to probe.
			break waitLoop
		case <-queueTicker.C:
			pos := probeSem.position(probeWaiter)
			if pos < 1 {
				pos = 1
			}
			sendEvent("queue", fmt.Sprintf(`{"position":%d}`, pos))
		case <-ctx.Done():
			if probeSem.cancel(probeWaiter) {
				// Removed from queue before slot was granted.
				if ctx.Err() == context.DeadlineExceeded {
					sendEvent("error", `{"error":"Timed out waiting for a format-probe slot. The server is busy — try again in a moment."}`)
				} else {
					sendEvent("error", `{"error":"Connection closed by client while waiting in queue."}`)
				}
			} else {
				// Slot was granted just as ctx expired; give it back.
				probeSem.release()
				sendEvent("error", `{"error":"Timed out waiting for a format-probe slot. The server is busy — try again in a moment."}`)
			}
			return
		}
	}
	defer probeSem.release()
	_ = myTicket // ticket assigned for ordering; position tracked via probeSem.position()

	if debugProbe {
		sendEvent("log", "Fetching formats...")
	}

	// Try format probe with fallback strategies.
	//
	// We pass a comma-list of YouTube player clients in a single yt-dlp
	// invocation (see youtubeClientArgs); yt-dlp queries each and unions
	// their format catalogs. If that primary attempt still fails (e.g.
	// non-YouTube URL where player_client is irrelevant, or a transient
	// network hiccup), we fall back to a bare retry without the client
	// override.
	type probeStrategy struct {
		name              string
		url               string
		skipPlayerClients bool
	}
	strategies := []probeStrategy{
		{"multi_client", url, false},
		{"default", url, true},
	}
	// Vimeo refuses anonymous web-client requests; its embed player does not.
	if player, ok := vimeoPlayerURL(url); ok {
		strategies = append(strategies, probeStrategy{"vimeo_player", player, false})
	}

	var out []byte
	var probeErr error
	var lastStderr string
	resolvedURL := url
	for i, strat := range strategies {
		if i > 0 {
			if debugProbe {
				sendEvent("log", fmt.Sprintf("Retrying with strategy: %s", strat.name))
			}
		}
		args := buildYtdlpArgs(ytdlpRequest{
			Mode:              modeProbe,
			URL:               strat.url,
			SkipPlayerClients: strat.skipPlayerClients,
		})
		cmd := lowPriorityCmd(ctx, cfg().YtdlpPath, args...)

		// Capture stderr for real-time logging
		stderrPipe, _ := cmd.StderrPipe()
		cmd.Stdout = nil // we'll read stdout after
		var stdout bytes.Buffer
		cmd.Stdout = &stdout

		if err := cmd.Start(); err != nil {
			probeErr = err
			logger.Warn("Format probe failed to start", "strategy", strat.name, "error", err)
			continue
		}
		lowerPriority(cmd)

		// Stream stderr lines as log events (only when debug mode is on)
		// Always capture stderr to include in error messages (capped at 64KB)
		const maxStderrBytes = 64 * 1024
		var stderrBuf strings.Builder
		if stderrPipe != nil {
			scanner := bufio.NewScanner(stderrPipe)
			for scanner.Scan() {
				line := scanner.Text()
				if line != "" {
					if stderrBuf.Len() < maxStderrBytes {
						stderrBuf.WriteString(line)
						stderrBuf.WriteByte('\n')
					}
					if debugProbe {
						sendEvent("log", line)
					}
				}
			}
		}

		probeErr = cmd.Wait()
		if probeErr == nil {
			out = stdout.Bytes()
			resolvedURL = strat.url
			if i > 0 {
				logger.Info("Format probe succeeded with fallback", "strategy", strat.name, "url", strat.url)
			}
			break
		}
		lastStderr = stderrBuf.String()
		logger.Warn("Format probe failed", "strategy", strat.name, "url", strat.url, "error", probeErr, "stderr", lastStderr)
	}
	if probeErr != nil {
		// Collect all ERROR: lines from yt-dlp stderr for a useful message.
		// Fall back to the last 2 non-empty stderr lines if none are found.
		errMsg := "Could not retrieve formats for this URL."
		var errLines []string
		var nonEmpty []string
		for _, line := range strings.Split(lastStderr, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			nonEmpty = append(nonEmpty, line)
			if strings.HasPrefix(line, "ERROR:") {
				errLines = append(errLines, strings.TrimSpace(strings.TrimPrefix(line, "ERROR:")))
			}
		}
		if len(errLines) > 0 {
			errMsg = strings.Join(errLines, " | ")
		} else if len(nonEmpty) > 0 {
			start := len(nonEmpty) - 2
			if start < 0 {
				start = 0
			}
			errMsg = "yt-dlp: " + strings.Join(nonEmpty[start:], " | ")
		}
		errJSON, _ := json.Marshal(map[string]string{"error": errMsg})
		sendEvent("error", string(errJSON))
		return
	}

	var info struct {
		Title      string  `json:"title"`
		Uploader   string  `json:"uploader"`
		Channel    string  `json:"channel"`
		Duration   float64 `json:"duration"`
		Thumbnail  string  `json:"thumbnail"`
		Thumbnails []struct {
			URL        string `json:"url"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			Preference int    `json:"preference"`
		} `json:"thumbnails"`
		Formats []struct {
			FormatID    string  `json:"format_id"`
			Ext         string  `json:"ext"`
			VCodec      string  `json:"vcodec"`
			ACodec      string  `json:"acodec"`
			Height      int     `json:"height"`
			FPS         float64 `json:"fps"`
			ABR         float64 `json:"abr"`
			Filesize    int64   `json:"filesize"`
			FilesizeApp int64   `json:"filesize_approx"`
		} `json:"formats"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		// yt-dlp emits one JSON object per entry on its own line for
		// playlist URLs (e.g. SoundCloud /sets/, YouTube /playlist?list=).
		// In that case `out` is NDJSON, which json.Unmarshal rejects.
		// Detect that and — for audio-first hosts — offer to download the
		// whole playlist as a single ZIP of MP3s. Everything else gets an
		// actionable "paste an individual track" error.
		dec := json.NewDecoder(bytes.NewReader(out))
		entries := 0
		var firstEntry struct {
			Playlist      string `json:"playlist"`
			PlaylistTitle string `json:"playlist_title"`
		}
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				break
			}
			if entries == 0 {
				_ = json.Unmarshal(raw, &firstEntry)
			}
			entries++
		}
		if entries > 1 {
			if isPlaylistZipHost(url) {
				if entries > playlistMaxEntries {
					sendEvent("error", fmt.Sprintf(`{"error":"Playlist has %d tracks — more than the %d-track limit. Please download in smaller chunks."}`, entries, playlistMaxEntries))
					return
				}
				title := firstEntry.Playlist
				if title == "" {
					title = firstEntry.PlaylistTitle
				}
				if title == "" {
					title = "playlist"
				}
				preset := Format{
					FormatID: "playlist:mp3",
					Label:    fmt.Sprintf("All %d tracks as ZIP (mp3) — %s", entries, title),
					Ext:      "zip",
					HasAudio: true,
				}
				formatsJSON, _ := json.Marshal(map[string]any{"formats": []Format{preset}})
				sendEvent("formats", string(formatsJSON))
				return
			}
			sendEvent("error", `{"error":"This URL points to a playlist. Please paste a link to an individual track or video instead."}`)
			return
		}
		sendEvent("error", `{"error":"Failed to parse format info."}`)
		return
	}

	seen := make(map[string]bool)
	var formats []Format

	for _, f := range info.Formats {
		hasVideo := f.VCodec != "" && f.VCodec != "none"
		hasAudio := f.ACodec != "" && f.ACodec != "none"
		filesize := f.Filesize
		if filesize == 0 {
			filesize = f.FilesizeApp
		}

		var label string
		if hasVideo && f.Height > 0 {
			label = fmt.Sprintf("%dp", f.Height)
			if f.FPS > 30 {
				label += fmt.Sprintf(" %.0ffps", f.FPS)
			}
			if hasAudio {
				label += " (video+audio)"
				label += fmt.Sprintf(" .%s", f.Ext)
			} else {
				// Video-only streams get merged and delivered as mp4, so the
				// source extension would be misleading — show the codec instead.
				label += " (video+audio merged)"
				if c := codecLabel(f.VCodec); c != "" {
					label += " " + c
				}
				label += " .mp4"
			}
		} else if hasAudio && !hasVideo {
			label = "Audio only"
			if f.ABR > 0 {
				label += fmt.Sprintf(" %dkbps", int(f.ABR))
			}
			label += fmt.Sprintf(" .%s", f.Ext)
		} else {
			continue
		}

		if seen[label] {
			continue
		}
		seen[label] = true

		if filesize > 0 {
			mb := float64(filesize) / (1024 * 1024)
			if mb >= 1 {
				label += fmt.Sprintf(" (~%.0fMB)", mb)
			} else {
				label += fmt.Sprintf(" (~%.0fKB)", float64(filesize)/1024)
			}
		}

		formats = append(formats, Format{
			FormatID: f.FormatID,
			Label:    label,
			Height:   f.Height,
			HasVideo: hasVideo,
			HasAudio: hasAudio,
			Ext:      f.Ext,
			Filesize: filesize,
		})
	}

	// Sort: video desc by height, then audio
	sortFormats(formats)

	// Prepend presets. Only offer video presets when the source actually
	// exposes at least one video stream — otherwise (e.g. SoundCloud,
	// Bandcamp, podcasts) offering "Best quality .mp4" produces an mp4
	// container wrapping an audio track, which is never what the user wants.
	hasAnyVideo := false
	for _, f := range formats {
		if f.HasVideo {
			hasAnyVideo = true
			break
		}
	}
	var presets []Format
	if hasAnyVideo {
		presets = append(presets,
			Format{FormatID: "bv*+ba/b", Label: "Best quality (highest available, mp4)", Height: 99999, HasVideo: true, HasAudio: true, Ext: "mp4"},
			Format{FormatID: "bv*[height<=1080]+ba/b[height<=1080]/bv*+ba/b", Label: "Best quality (up to 1080p, mp4)", Height: 9999, HasVideo: true, HasAudio: true, Ext: "mp4"},
		)
	}
	presets = append(presets, Format{FormatID: "ba", Label: "Best audio only (mp3)", Height: 0, HasVideo: false, HasAudio: true, Ext: "mp3"})
	formats = append(presets, formats...)

	// The format IDs below belong to whichever URL actually answered, so the
	// client must download from that one rather than what the user pasted.
	if resolvedURL != url {
		resolvedJSON, _ := json.Marshal(map[string]string{"url": resolvedURL})
		sendEvent("resolved_url", string(resolvedJSON))
	}

	preview := map[string]any{
		"title":    info.Title,
		"uploader": firstNonEmpty(info.Uploader, info.Channel),
		"duration": info.Duration,
	}
	if id := rememberThumbnail(pickThumbnail(info.Thumbnail, info.Thumbnails)); id != "" {
		preview["thumbnail"] = "/api/thumbnail/" + id
	}
	if previewJSON, err := json.Marshal(preview); err == nil {
		sendEvent("media", string(previewJSON))
	}

	formatsJSON, _ := json.Marshal(map[string]any{"formats": formats})
	sendEvent("formats", string(formatsJSON))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

type downloadRequest struct {
	URL         string `json:"url"`
	FormatID    string `json:"format_id"`
	Destination string `json:"destination"`
	Title       string `json:"title"`
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxURLLen+4096)
	var req downloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		jsonError(w, "No URL provided", http.StatusBadRequest)
		return
	}
	owner := ""
	if a := currentAuth(r); a != nil {
		owner = a.User.Username
	}
	startDownload(w, r, req, owner)
}

func startDownload(w http.ResponseWriter, r *http.Request, req downloadRequest, owner string) bool {
	ip := requestActor(r)
	// Rate limit downloads per minute per user (or IP).
	// Burst allows queuing up to download_queue_max at once; the per-minute
	// rate still protects against sustained abuse.
	configMu.RLock()
	rlDownloads := config.RateLimitDownloads
	dlQueueMax := config.DownloadQueueMax
	configMu.RUnlock()
	if rlDownloads <= 0 {
		rlDownloads = 3
	}
	if dlQueueMax < rlDownloads {
		dlQueueMax = rlDownloads
	}
	if !limiter.allow(ip+":download", float64(rlDownloads)/60.0, float64(dlQueueMax)) {
		logger.Warn("Rate limited", "ip", ip, "endpoint", "download")
		jsonError(w, "Too many download requests. Please wait.", http.StatusTooManyRequests)
		return false
	}

	url := strings.TrimSpace(req.URL)
	kind, err := classifyURL(url)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return false
	}
	if err := killSwitchGate(r.Context()); err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return false
	}
	if kind == kindTorrent {
		if torrentEngine() == engineBuiltin {
			dest := "device"
			if req.Destination == "library" && cfg().LibraryDir != "" {
				dest = "library"
			}
			return startTorrentForRequest(w, r, url, owner, cleanJobTitle(req.Title), dest)
		}
		return addTorrentForRequest(w, r, url, owner)
	}

	destination := strings.TrimSpace(req.Destination)
	switch destination {
	case "", "device":
		destination = "device"
	case "library":
		if cfg().LibraryDir == "" {
			jsonError(w, "Saving to the server is not configured.", http.StatusBadRequest)
			return false
		}
	default:
		jsonError(w, "Invalid destination", http.StatusBadRequest)
		return false
	}

	formatID := strings.TrimSpace(req.FormatID)
	if kind == kindDirect {
		formatID = "direct"
	} else if formatID == "direct" || formatID == "torrent" {
		jsonError(w, "That format does not apply to this URL.", http.StatusBadRequest)
		return false
	}
	if formatID == "" {
		formatID = "bv*[vcodec~='^(avc|h264)'][height<=1080]+ba/b[height<=1080]/bv*[height<=1080]+ba/b[height<=1080]/bv*+ba/b"
	}

	// If a simple format ID (numeric), it may be video-only.
	// Append +bestaudio with a fallback to handle video-only formats gracefully.
	// m4a is tried first: plain "bestaudio" is usually Opus/WebM on YouTube, which
	// cannot be muxed into mp4 and would force the merge into an mkv container.
	userPickedSpecific := isSimpleFormatID(formatID)
	if userPickedSpecific {
		formatID = formatID + "+bestaudio[ext=m4a]/" + formatID + "+bestaudio/" + formatID
	}

	// Reject immediately if the download queue is already full.
	for {
		cur := downloadWaiting.Load()
		configMu.RLock()
		qmax := int32(config.DownloadQueueMax)
		configMu.RUnlock()
		if cur >= qmax {
			jsonError(w, "Download queue is full. Please try again shortly.", http.StatusServiceUnavailable)
			return false
		}
		if downloadWaiting.CompareAndSwap(cur, cur+1) {
			break
		}
	}

	// Assign a display ticket from a monotonic counter. The actual FIFO
	// enqueue happens inside downloadMedia (after per-IP gating) so that
	// one IP cannot monopolise all global slots.
	ticket := downloadTicket.Add(1)

	jobID := generateID()
	jobToken := generateSessionToken()
	timeout := time.Duration(cfg().DownloadTimeoutMinutes) * time.Minute
	if kind == kindDirect {
		// Plain files can be many GB; the yt-dlp timeout is sized for clips.
		timeout = time.Duration(cfg().DirectTimeoutMinutes) * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	job := &Job{
		ID:          jobID,
		Status:      "queued",
		Progress:    0,
		Message:     "Waiting in queue...",
		Ticket:      ticket,
		IP:          ip,
		URL:         url,
		FormatID:    strings.TrimSpace(req.FormatID),
		Token:       jobToken,
		Owner:       owner,
		Title:       cleanJobTitle(req.Title),
		Kind:        kind,
		Destination: destination,
		CreatedAt:   time.Now(),
		cancel:      cancel,
		subscribers: make(map[chan struct{}]struct{}),
	}

	jobsMu.Lock()
	jobs[jobID] = job
	jobsMu.Unlock()

	go func() {
		defer cancel() // release the timer as soon as the job settles
		downloadMedia(ctx, job, url, formatID, userPickedSpecific)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"job_id": jobID, "token": jobToken})
	return true
}

// authorizeJob checks the caller presented the token handed out when the job
// was created. Job IDs alone must never be sufficient to observe or cancel a
// download that belongs to somebody else.
// The owning account (or an admin) may also see it without the token, e.g.
// when polling a job started through the API from another client.
func authorizeJob(r *http.Request, job *Job) bool {
	token := r.URL.Query().Get("token")
	if token == "" {
		token = r.Header.Get("X-Job-Token")
	}
	if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(job.Token)) == 1 {
		return true
	}
	a := currentAuth(r)
	if a == nil {
		return false
	}
	return a.User.Role == roleAdmin || (job.Owner != "" && userKey(a.User.Username) == userKey(job.Owner))
}

// buildJobStatusResponse serialises the job state into a map shared by the
// polling and WebSocket status endpoints. Must be called with job.mu held.
func buildJobStatusResponse(job *Job, includeLogs bool) map[string]any {
	resp := map[string]any{
		"status":   job.Status,
		"progress": job.Progress,
		"message":  job.Message,
	}
	if includeLogs {
		resp["logs"] = job.Logs
	}
	switch job.Status {
	case "queued":
		var pos int32
		if job.ipWaiter != nil {
			pos = getIPDownloadSem(job.IP).position(job.ipWaiter)
		} else if job.waiter != nil {
			pos = dlSem.position(job.waiter)
		}
		if pos < 1 {
			pos = 1
		}
		resp["queue_position"] = pos
	case "done":
		resp["display_name"] = job.DisplayName
		if job.Warning != "" {
			resp["warning"] = job.Warning
		}
		addFileFields(resp, job.ID, job.Filename, job.LibraryPath, job.FinishedAt)
	case "error":
		resp["error"] = job.Error
	case "cancelled":
		if job.StopReason != "" {
			resp["error"] = job.StopReason
		}
	}
	if job.Kind == kindTorrent {
		resp["peers"] = job.Peers
		if job.UserPaused {
			resp["user_paused"] = true
		}
		if job.Seeding {
			resp["seeding"] = true
			resp["rate_up"] = job.RateUp
			resp["ratio"] = job.Ratio
		}
	}
	return resp
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/api/status/")
	if jobID == "" {
		jsonError(w, "No job ID", http.StatusBadRequest)
		return
	}

	jobsMu.RLock()
	job, ok := jobs[jobID]
	jobsMu.RUnlock()

	if !ok {
		jsonError(w, "Job not found", http.StatusNotFound)
		return
	}
	if !authorizeJob(r, job) {
		jsonError(w, "Job not found", http.StatusNotFound)
		return
	}

	configMu.RLock()
	debug := config.DebugMode
	configMu.RUnlock()

	job.mu.Lock()
	resp := buildJobStatusResponse(job, debug)
	job.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobID := strings.TrimPrefix(r.URL.Path, "/api/cancel/")
	if jobID == "" {
		jsonError(w, "No job ID", http.StatusBadRequest)
		return
	}

	jobsMu.RLock()
	job, ok := jobs[jobID]
	jobsMu.RUnlock()

	if !ok {
		jsonError(w, "Job not found", http.StatusNotFound)
		return
	}
	if !authorizeJob(r, job) {
		jsonError(w, "Job not found", http.StatusNotFound)
		return
	}

	job.mu.Lock()
	wasPaused := job.Status == "paused"
	if job.Status == "downloading" || job.Status == "queued" || wasPaused {
		job.Status = "cancelled"
		job.notifySubscribers()
		job.cancel()
	}
	job.mu.Unlock()

	// A torrent paused by the kill switch has no goroutine left to clean up.
	if wasPaused && job.Kind == kindTorrent {
		updateBTResume(func(m map[string]btResume) { delete(m, jobID) })
		removeJobFiles(jobWorkDir(job), jobID)
	}

	// Cleanup job directory
	os.RemoveAll(filepath.Join(jobWorkDir(job), jobID))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
}

// ---------------------------------------------------------------------------
// WebSocket: real-time progress
// ---------------------------------------------------------------------------

// maxJobSubscribers caps concurrent WebSocket listeners per job so a single
// client cannot amplify every progress update.
const maxJobSubscribers = 4

func handleWSStatus(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/ws/status/")
	if jobID == "" {
		http.Error(w, "No job ID", http.StatusBadRequest)
		return
	}

	jobsMu.RLock()
	job, ok := jobs[jobID]
	jobsMu.RUnlock()
	if !ok {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}
	if !authorizeJob(r, job) {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}

	configMu.RLock()
	origins := append([]string(nil), config.AllowedOrigins...)
	configMu.RUnlock()

	// Nil OriginPatterns keeps the library's same-origin default.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: origins,
	})
	if err != nil {
		logger.Error("WebSocket accept failed", "error", err)
		return
	}
	defer c.CloseNow()

	ctx := c.CloseRead(r.Context())

	// Subscribe to job updates
	ch := make(chan struct{}, 1)
	job.mu.Lock()
	if len(job.subscribers) >= maxJobSubscribers {
		job.mu.Unlock()
		c.Close(websocket.StatusTryAgainLater, "too many subscribers")
		return
	}
	job.subscribers[ch] = struct{}{}
	job.mu.Unlock()

	defer func() {
		job.mu.Lock()
		delete(job.subscribers, ch)
		job.mu.Unlock()
	}()
	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()

	// Send initial state + updates until done
	for {
		configMu.RLock()
		debugWS := config.DebugMode
		configMu.RUnlock()

		job.mu.Lock()
		msg := buildJobStatusResponse(job, debugWS)
		done := job.Status != "downloading" && job.Status != "queued"
		job.mu.Unlock()

		if err := wsjson.Write(ctx, c, msg); err != nil {
			return
		}

		if done {
			c.Close(websocket.StatusNormalClosure, "done")
			return
		}

		// Wait for update or context cancel
		select {
		case <-ch:
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			// Heartbeat - resend current state
		}
	}
}

func downloadMedia(ctx context.Context, job *Job, url, formatID string, userPickedSpecific bool) {
	defer downloadWaiting.Add(-1)

	positionTicker := time.NewTicker(time.Second)
	defer positionTicker.Stop()

	// ---------------------------------------------------------------
	// Phase 1: Per-IP gate — only N downloads per IP may be active at
	// once (default 1). This prevents one user from hogging all global
	// slots while still letting them queue multiple downloads.
	// ---------------------------------------------------------------
	ipSem := getIPDownloadSem(job.IP)
	ipWaiter, _ := ipSem.enqueue()

	job.mu.Lock()
	job.ipWaiter = ipWaiter
	job.mu.Unlock()

ipWaitLoop:
	for {
		select {
		case <-ipWaiter.ch:
			break ipWaitLoop

		case <-positionTicker.C:
			pos := ipSem.position(ipWaiter)
			job.mu.Lock()
			if job.Status == "cancelled" {
				job.mu.Unlock()
				if !ipSem.cancel(ipWaiter) {
					ipSem.release()
				}
				return
			}
			if pos > 0 {
				job.Message = fmt.Sprintf("Waiting for your download to finish (#%d in your queue)", pos)
				job.notifySubscribers()
			}
			job.mu.Unlock()

		case <-ctx.Done():
			if ipSem.cancel(ipWaiter) {
				job.mu.Lock()
				if job.Status != "cancelled" {
					configMu.RLock()
					timeoutMin := config.DownloadTimeoutMinutes
					configMu.RUnlock()
					job.Status = "error"
					job.Error = fmt.Sprintf("Timed out after %d minute(s) while waiting in queue.", timeoutMin)
					job.notifySubscribers()
				}
				job.mu.Unlock()
			} else {
				ipSem.release()
			}
			return
		}
	}
	defer ipSem.release()

	// Clear per-IP waiter and check for cancellation between phases.
	job.mu.Lock()
	job.ipWaiter = nil
	if job.Status == "cancelled" {
		job.mu.Unlock()
		return
	}
	job.mu.Unlock()

	// ---------------------------------------------------------------
	// Phase 2: Global FIFO gate — limits total server-wide concurrent
	// downloads (default 4). Only requests that passed the per-IP gate
	// compete here, so one IP cannot monopolise all slots.
	// ---------------------------------------------------------------
	waiter, _ := dlSem.enqueue()

	job.mu.Lock()
	job.waiter = waiter
	job.mu.Unlock()

globalWaitLoop:
	for {
		select {
		case <-waiter.ch:
			break globalWaitLoop

		case <-positionTicker.C:
			pos := dlSem.position(waiter)
			job.mu.Lock()
			if job.Status == "cancelled" {
				job.mu.Unlock()
				if !dlSem.cancel(waiter) {
					dlSem.release()
				}
				return
			}
			if pos > 0 {
				job.Message = fmt.Sprintf("Queued — position #%d", pos)
				job.notifySubscribers()
			}
			job.mu.Unlock()

		case <-ctx.Done():
			configMu.RLock()
			timeoutMin := config.DownloadTimeoutMinutes
			configMu.RUnlock()
			if dlSem.cancel(waiter) {
				job.mu.Lock()
				if job.Status != "cancelled" {
					job.Status = "error"
					job.Error = fmt.Sprintf("Timed out after %d minute(s) while waiting in the download queue. The server is busy — try again shortly.", timeoutMin)
					job.notifySubscribers()
				}
				job.mu.Unlock()
			} else {
				dlSem.release()
			}
			return
		}
	}
	defer dlSem.release()

	// Slot acquired — clear waiter reference and transition to active download.
	ksErr := killSwitchGate(ctx)
	job.mu.Lock()
	job.waiter = nil
	if job.Status == "cancelled" {
		job.mu.Unlock()
		return
	}
	if ksErr != nil {
		msg := ksErr.Error()
		job.Status, job.Message, job.StopReason = "cancelled", msg, msg
		job.notifySubscribers()
		job.mu.Unlock()
		return
	}
	job.Status = "downloading"
	job.Message = "Starting download..."
	job.notifySubscribers()
	job.mu.Unlock()

	// Pre-download: check disk space where this job will actually write.
	job.mu.Lock()
	job.workDir = tempDir()
	job.mu.Unlock()
	workDir := jobWorkDir(job)
	if err := os.MkdirAll(workDir, 0775); err != nil {
		setJobError(job, "Cannot prepare the download folder: "+err.Error())
		return
	}
	if minFree := cfg().MinDiskFreeMB; minFree > 0 {
		free, err := diskFreeMBAt(workDir)
		if err == nil && free < int64(minFree) {
			setJobError(job, fmt.Sprintf("Insufficient disk space: %d MB free, need at least %d MB", free, minFree))
			return
		}
	}

	// Pre-download: check storage limit (only the temporary downloads folder is capped)
	if maxStorage := cfg().MaxStorageMB; maxStorage > 0 && job.Destination != "library" {
		used := dirSizeMB()
		if used >= int64(maxStorage) {
			// Try to free space first
			cleanupFiles()
			used = dirSizeMB()
			if used >= int64(maxStorage) {
				setJobError(job, fmt.Sprintf("Storage limit reached: %d MB used (max %d MB)", used, maxStorage))
				return
			}
		}
	}

	// Build fallback chain: try progressively simpler formats on failure.
	// Playlist-zip jobs are single-attempt — their yt-dlp invocation already
	// handles per-track failures internally, and we don't have a meaningful
	// fallback for "download N tracks and zip them".
	type attemptPlan struct {
		url      string
		formatID string
	}
	var attempts []attemptPlan
	if strings.HasPrefix(formatID, "playlist:") || formatID == "direct" {
		attempts = []attemptPlan{{url, formatID}}
	} else {
		attempts = []attemptPlan{{url, formatID}}
		if formatID != "ba" { // no video fallbacks for audio-only
			if formatID != "bv*+ba/b" {
				attempts = append(attempts, attemptPlan{url, "bv*+ba/b"})
			}
			if formatID != "b" {
				attempts = append(attempts, attemptPlan{url, "b"})
			}
		}
		// Last resort for Vimeo page links, whose web client now demands a login.
		if player, ok := vimeoPlayerURL(url); ok {
			attempts = append(attempts, attemptPlan{player, "bv*+ba/b"})
		}
	}

	var lastErr string
	for attempt, plan := range attempts {
		currentFormat := plan.formatID
		if attempt > 0 {
			job.mu.Lock()
			if job.Status == "cancelled" {
				job.mu.Unlock()
				return
			}
			job.Progress = 0
			job.Message = fmt.Sprintf("Retrying with fallback format (%d/%d)...", attempt+1, len(attempts))
			job.Logs = append(job.Logs, fmt.Sprintf("--- Retry %d/%d with format: %s ---", attempt+1, len(attempts), currentFormat))
			job.notifySubscribers()
			job.mu.Unlock()
			logger.Info("Retrying download", "job", job.ID, "attempt", attempt+1, "format", currentFormat, "url", plan.url)
		}

		var finalName, displayName string
		var err error
		if currentFormat == "direct" {
			finalName, displayName, err = tryDownloadDirect(ctx, job, plan.url)
		} else if strings.HasPrefix(currentFormat, "playlist:") {
			finalName, displayName, err = tryDownloadPlaylist(ctx, job, plan.url, strings.TrimPrefix(currentFormat, "playlist:"))
		} else {
			// Only the first attempt honours the user's specific pick.
			// Fallbacks are preset selectors and always prefer h264/mp4 for
			// maximum compatibility and a cheap stream-copy remux.
			preferCompat := !(userPickedSpecific && attempt == 0)
			finalName, displayName, err = tryDownloadFormat(ctx, job, plan.url, currentFormat, preferCompat)
		}
		if err == nil && job.Destination == "library" {
			job.mu.Lock()
			job.Progress = 99
			job.Message = "Saving to server..."
			job.notifySubscribers()
			job.mu.Unlock()
			libPath, mvErr := moveToLibrary(filepath.Join(workDir, finalName), displayName)
			if mvErr != nil {
				os.Remove(filepath.Join(workDir, finalName))
				setJobError(job, "Downloaded, but saving to the server failed: "+mvErr.Error())
				return
			}
			job.mu.Lock()
			job.LibraryPath = libPath
			job.mu.Unlock()
			finalName = ""
		}
		if err == nil {
			job.mu.Lock()
			job.Status = "done"
			job.Progress = 100
			job.Message = "Complete!"
			job.Filename = finalName
			job.DisplayName = displayName
			job.FinishedAt = time.Now()
			job.Logs = nil // free log memory on completion
			job.notifySubscribers()
			job.mu.Unlock()

			notifyFileChange()

			appendJobHistory(JobRecord{
				ID:          job.ID,
				URL:         job.URL,
				FormatID:    job.FormatID,
				Status:      "done",
				Filename:    finalName,
				DisplayName: displayName,
				Warning:     job.Warning,
				Owner:       job.Owner,
				Title:       job.Title,
				Kind:        job.Kind,
				Destination: job.Destination,
				LibraryPath: job.LibraryPath,
				CreatedAt:   job.CreatedAt,
				FinishedAt:  job.FinishedAt,
			})
			return
		}

		lastErr = err.Error()
		logger.Warn("Download attempt failed", "job", job.ID, "attempt", attempt+1, "format", currentFormat, "error", err)

		// Stop retrying if cancelled
		job.mu.Lock()
		cancelled := job.Status == "cancelled"
		job.mu.Unlock()
		if cancelled {
			return
		}
	}

	setJobError(job, lastErr)
}

// tryDownloadFormat attempts a single yt-dlp download with the given format.
// Returns (finalName, displayName, nil) on success, or ("", "", error) on failure.
// Updates job progress/message during execution. Cleans up jobDir on failure.
// When preferCompat is true, yt-dlp is biased toward h264 for broad
// compatibility. When false (the user picked a specific format such as a 2160p
// VP9/AV1 stream) the chosen codecs are kept as-is. Either way the target
// container is mp4; ensureMP4 normalises anything yt-dlp had to write elsewhere.
func tryDownloadFormat(ctx context.Context, job *Job, url, formatID string, preferCompat bool) (string, string, error) {
	jobDir := filepath.Join(jobWorkDir(job), job.ID)
	os.MkdirAll(jobDir, 0755)

	outputTemplate := filepath.Join(jobDir, "%(title).80s.%(ext)s")
	isAudioOnly := formatID == "ba"

	args := buildYtdlpArgs(ytdlpRequest{
		Mode:           modeDownload,
		URL:            url,
		FormatID:       formatID,
		OutputTemplate: outputTemplate,
		AudioOnly:      isAudioOnly,
		PreferH264:     preferCompat,
		MaxFileSizeMB:  cfg().MaxFileSizeMB,
	})

	logger.Info("Starting yt-dlp", "job", job.ID, "format", formatID, "args", strings.Join(args, " "))

	cmd := lowPriorityCmd(ctx, cfg().YtdlpPath, args...)

	// Merge stdout and stderr into a single pipe
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("yt-dlp failed to start: %w", err)
	}
	lowerPriority(cmd)

	// Close write end when process exits so scanner doesn't block
	go func() {
		cmd.Wait()
		pw.Close()
	}()

	scanner := bufio.NewScanner(pr)
	downloadPass := 0
	totalPasses := 0 // detected from [download] Destination lines
	var lastError string

	for scanner.Scan() {
		line := scanner.Text()
		logger.Debug("yt-dlp output", "job", job.ID, "line", line)

		// Single lock per line: append log, check cancel, update state
		job.mu.Lock()

		// Append to job logs (keep last 50 lines — most are duplicate progress)
		job.Logs = append(job.Logs, line)
		if len(job.Logs) > 50 {
			job.Logs = job.Logs[len(job.Logs)-50:]
		}

		// Check cancellation
		if job.Status == "cancelled" {
			job.mu.Unlock()
			cmd.Process.Kill()
			pr.Close()
			os.RemoveAll(jobDir)
			return "", "", fmt.Errorf("cancelled")
		}

		// Parse line content — fast prefix check before expensive Contains
		if pct, ok := parseProgressPercent(line); ok {
			// Enforce monotonic progress — never decrease.
			var newProgress float64
			if downloadPass <= 1 && totalPasses <= 1 {
				newProgress = pct * 0.98
			} else if downloadPass <= 1 {
				newProgress = pct * 0.80
			} else {
				newProgress = 80 + pct*0.18
			}
			prevProgress := job.Progress
			prevMessage := job.Message
			if job.Message == "Starting download..." || job.Message == "Fetching metadata..." || strings.HasPrefix(job.Message, "Retrying") {
				job.Message = "Downloading..."
			}
			if newProgress > job.Progress {
				job.Progress = newProgress
			}
			// Only push updates when visible progress changed meaningfully.
			if (job.Progress-prevProgress) >= 0.5 || job.Message != prevMessage {
				job.notifySubscribers()
			}
		} else if len(line) > 0 && line[0] == '[' {
			// Track download passes
			if strings.Contains(line, "[download] Destination:") {
				downloadPass++
				totalPasses = downloadPass
				if downloadPass == 1 {
					job.Message = "Downloading..."
				} else {
					job.Message = "Downloading audio..."
					if job.Progress > 80 {
						job.Progress = 80
					}
				}
				job.notifySubscribers()
			} else if strings.Contains(line, "[Merger]") || strings.Contains(line, "[ExtractAudio]") {
				// Detect merging/processing
				if job.Message != "Processing..." || job.Progress < 99 {
					job.Message = "Processing..."
					job.Progress = 99
					job.notifySubscribers()
				}
			} else if strings.Contains(line, "[info]") || strings.Contains(line, "[youtube]") {
				// Metadata/extraction phase
				if job.Message != "Fetching metadata..." {
					job.Message = "Fetching metadata..."
					job.notifySubscribers()
				}
			}
		} else if strings.Contains(line, "Merging") || strings.Contains(line, "Extracting") {
			// Non-bracketed processing lines
			if strings.Contains(line, "Merging") {
				if job.Message != "Processing..." || job.Progress < 99 {
					job.Message = "Processing..."
					job.Progress = 99
					job.notifySubscribers()
				}
			} else {
				if job.Message != "Fetching metadata..." {
					job.Message = "Fetching metadata..."
					job.notifySubscribers()
				}
			}
		}

		job.mu.Unlock()

		// Track errors (outside lock — just a local variable)
		if strings.Contains(line, "ERROR") {
			lastError = line
		}
	}

	pr.Close()

	// Wait for cmd.Wait() goroutine to finish
	// cmd.Wait() already called in goroutine, check process state
	exitErr := cmd.ProcessState

	// Check if cancelled during wait
	job.mu.Lock()
	if job.Status == "cancelled" {
		job.mu.Unlock()
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("cancelled")
	}
	job.mu.Unlock()

	if exitErr == nil || !exitErr.Success() {
		errMsg := lastError
		if errMsg == "" {
			errMsg = "Download failed"
			if exitErr != nil {
				errMsg += ": " + exitErr.String()
			}
		}
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("%s", errMsg)
	}

	// Find the downloaded file
	entries, _ := os.ReadDir(jobDir)
	var bestFile string
	var bestSize int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") || strings.HasSuffix(name, ".temp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Size() > bestSize {
			bestSize = info.Size()
			bestFile = name
		}
	}

	if bestFile == "" {
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("no file was downloaded")
	}

	src := filepath.Join(jobDir, bestFile)
	if !isAudioOnly {
		src = ensureMP4(ctx, job, src, preferCompat)
		bestFile = filepath.Base(src)
	}

	// Move file to downloads root
	finalName := job.ID + "_" + bestFile
	dst := filepath.Join(jobWorkDir(job), finalName)
	if err := os.Rename(src, dst); err != nil {
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("failed to move file: %w", err)
	}
	// Touch the file so cleanup uses download time, not the source's original modtime
	now := time.Now()
	os.Chtimes(dst, now, now)
	os.RemoveAll(jobDir)

	return finalName, bestFile, nil
}

// playlistItemRe matches lines like "[download] Downloading item 3 of 12"
// emitted by yt-dlp in playlist mode. Used to derive overall progress.
var playlistItemRe = regexp.MustCompile(`\[download\]\s+Downloading item (\d+) of (\d+)`)

// playlistWorkers is how many yt-dlp processes share one album. Each track
// spends ~8s resolving with YouTube and ~8s single-core MP3 encoding, so a
// few in parallel roughly divide the wall time; more risks YouTube's bot check.
const playlistWorkers = 3

// countPlaylist returns the number of entries via the flat probe, or 0 if unknown.
func countPlaylist(ctx context.Context, url string) int {
	out, err := lowPriorityCmd(ctx, cfg().YtdlpPath, buildYtdlpArgs(ytdlpRequest{Mode: modeProbe, URL: url})...).Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, l := range bytes.Split(out, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(l), []byte("{")) {
			n++
		}
	}
	return n
}

// playlistRanges splits n tracks (capped) into contiguous "a-b" slices for up
// to workers processes. An unknown size gives one worker for the whole list.
func playlistRanges(n, workers int) []string {
	if n > playlistMaxEntries {
		n = playlistMaxEntries
	}
	if n < 1 {
		return []string{""}
	}
	if workers > n {
		workers = n
	}
	var out []string
	start := 1
	for i := 0; i < workers; i++ {
		size := n / workers
		if i < n%workers {
			size++
		}
		out = append(out, fmt.Sprintf("%d-%d", start, start+size-1))
		start += size
	}
	return out
}

// tryDownloadPlaylist runs yt-dlp in playlist mode, saves each track to the
// per-job directory as an MP3, then packs them into a single stored-only
// (uncompressed) ZIP file in the downloads root. Audio is already heavily
// compressed; deflate would waste CPU for no size win.
//
// audioFormat is currently unused but threaded through for future variants
// (e.g. flac, opus). Only "mp3" is supported today.
func tryDownloadPlaylist(ctx context.Context, job *Job, url, audioFormat string) (string, string, error) {
	if audioFormat == "" {
		audioFormat = "mp3"
	}
	jobDir := filepath.Join(jobWorkDir(job), job.ID)
	os.MkdirAll(jobDir, 0755)

	// "1. Intro.mp3": album position, then the song title as the album lists it.
	outputTemplate := filepath.Join(jobDir, "%(playlist_index)d. %(track,title).80s.%(ext)s")

	total := countPlaylist(ctx, url)
	if total > playlistMaxEntries {
		total = playlistMaxEntries
	}
	ranges := playlistRanges(total, playlistWorkers)
	logger.Info("Starting yt-dlp playlist download", "job", job.ID, "url", url, "workers", len(ranges))

	// Each worker takes a slice of the album; yt-dlp keeps the original
	// playlist_index, so file names and numbering are unaffected.
	type workerLine struct {
		w    int
		line string
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	lines := make(chan workerLine)
	var wg sync.WaitGroup
	var exitMu sync.Mutex
	var exitNotes []string
	started := 0
	for i, items := range ranges {
		args := buildYtdlpArgs(ytdlpRequest{
			Mode:           modePlaylist,
			URL:            url,
			OutputTemplate: outputTemplate,
			AudioFormat:    audioFormat,
			MaxFileSizeMB:  cfg().MaxFileSizeMB,
			PlaylistItems:  items,
		})
		cmd := lowPriorityCmd(runCtx, cfg().YtdlpPath, args...)
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		cmd.Stderr = pw
		if err := cmd.Start(); err != nil {
			pw.Close()
			pr.Close()
			exitMu.Lock()
			exitNotes = append(exitNotes, err.Error())
			exitMu.Unlock()
			continue
		}
		lowerPriority(cmd)
		started++
		go func() {
			if err := cmd.Wait(); err != nil {
				exitMu.Lock()
				exitNotes = append(exitNotes, err.Error())
				exitMu.Unlock()
			}
			pw.Close()
		}()
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			sc := bufio.NewScanner(pr)
			for sc.Scan() {
				lines <- workerLine{w, sc.Text()}
			}
			pr.Close()
		}(i)
	}
	if started == 0 {
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("yt-dlp failed to start: %s", strings.Join(exitNotes, "; "))
	}
	go func() {
		wg.Wait()
		close(lines)
	}()

	var lastError string
	var playlistTitle string
	cancelled := false
	done := make([]int, len(ranges))  // finished tracks per worker
	sizes := make([]int, len(ranges)) // tracks per worker, from "Downloading item k of n"
	current := make([]float64, len(ranges))

	// Keep draining after a cancel so the reader goroutines can exit.
	for wl := range lines {
		if cancelled {
			continue
		}
		line, w := wl.line, wl.w

		job.mu.Lock()
		job.Logs = append(job.Logs, line)
		if len(job.Logs) > 50 {
			job.Logs = job.Logs[len(job.Logs)-50:]
		}
		if job.Status == "cancelled" {
			job.mu.Unlock()
			cancelled = true
			stop()
			continue
		}

		progressed := false
		if m := playlistItemRe.FindStringSubmatch(line); len(m) == 3 {
			k, _ := strconv.Atoi(m[1])
			sizes[w], _ = strconv.Atoi(m[2])
			done[w], current[w] = k-1, 0
			progressed = true
		} else if pct, ok := parseProgressPercent(line); ok {
			current[w] = pct / 100
			progressed = true
		} else if playlistTitle == "" {
			playlistTitle = playlistTitleFromLine(line)
		}
		if progressed {
			total, finished := 0, 0.0
			for i := range sizes {
				total += sizes[i]
				finished += float64(done[i]) + current[i]
			}
			if total > 0 {
				job.Message = fmt.Sprintf("Downloading tracks... %d of %d done", int(finished), total)
				if p := finished / float64(total) * 95.0; p > job.Progress {
					job.Progress = p
				}
				job.notifySubscribers()
			}
		}
		job.mu.Unlock()

		if strings.Contains(line, "ERROR") {
			lastError = line
		}
	}

	if cancelled {
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("cancelled")
	}

	job.mu.Lock()
	if job.Status == "cancelled" {
		job.mu.Unlock()
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("cancelled")
	}
	job.mu.Unlock()

	// YouTube refuses some stream URLs with HTTP 403 for a while (seen on
	// YouTube Music albums); the same track usually downloads a minute later.
	// yt-dlp has written the cover by then, so only the .jpg is left.
	for round, wait := range playlistRetryWaits {
		missing, _ := missingPlaylistItems(dirNames(jobDir), total, audioFormat)
		if len(missing) == 0 {
			break
		}
		job.mu.Lock()
		job.Message = fmt.Sprintf("Retrying %d track(s) YouTube refused...", len(missing))
		job.notifySubscribers()
		job.mu.Unlock()
		logger.Info("Retrying playlist tracks", "job", job.ID, "round", round+1, "items", joinInts(missing))
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		if ctx.Err() != nil {
			break
		}
		args := buildYtdlpArgs(ytdlpRequest{
			Mode:           modePlaylist,
			URL:            url,
			OutputTemplate: outputTemplate,
			AudioFormat:    audioFormat,
			MaxFileSizeMB:  cfg().MaxFileSizeMB,
			PlaylistItems:  joinInts(missing),
		})
		if e := runYtdlpLogged(ctx, job, args); e != "" {
			lastError = e
		}
	}
	job.mu.Lock()
	cancelled = job.Status == "cancelled" || ctx.Err() != nil
	job.mu.Unlock()
	if cancelled {
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("cancelled")
	}

	// yt-dlp may exit non-zero when --ignore-errors was triggered; only
	// treat it as a total failure if nothing was produced. Only audio files
	// are tracks: a cover image left by a failed track is not.
	names := dirNames(jobDir)
	missing, labels := missingPlaylistItems(names, total, audioFormat)
	var tracks []os.DirEntry
	entries, _ := os.ReadDir(jobDir)
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), "."+audioFormat) {
			tracks = append(tracks, e)
		}
	}
	if len(missing) > 0 && len(tracks) > 0 {
		var parts []string
		for _, i := range missing {
			parts = append(parts, labels[i])
		}
		warning := fmt.Sprintf("%d track(s) could not be downloaded: %s", len(missing), strings.Join(parts, ", "))
		logger.Warn("Playlist incomplete", "job", job.ID, "missing", joinInts(missing), "last_error", lastError)
		job.mu.Lock()
		job.Warning = warning
		job.mu.Unlock()
	}
	if len(tracks) == 0 {
		msg := lastError
		if msg == "" {
			msg = "No tracks could be downloaded from this playlist."
			if len(exitNotes) > 0 {
				msg += " (" + strings.Join(exitNotes, "; ") + ")"
			}
		}
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("%s", msg)
	}

	// Zip phase (uncompressed — Store method). Audio is already compressed;
	// deflate would burn CPU on a Pi for negligible gains.
	job.mu.Lock()
	job.Message = fmt.Sprintf("Packaging %d tracks into ZIP...", len(tracks))
	if job.Progress < 95 {
		job.Progress = 95
	}
	job.notifySubscribers()
	job.mu.Unlock()

	if playlistTitle == "" {
		playlistTitle = "playlist"
	}
	zipBase := sanitizeFilename(albumArchiveName(readTrackTags(ctx, jobDir, tracks), playlistTitle)) + ".zip"
	finalName := job.ID + "_" + zipBase
	dst := filepath.Join(jobWorkDir(job), finalName)

	if err := createStoredZip(dst, jobDir, tracks); err != nil {
		os.Remove(dst)
		os.RemoveAll(jobDir)
		return "", "", fmt.Errorf("failed to package zip: %w", err)
	}

	now := time.Now()
	os.Chtimes(dst, now, now)
	os.RemoveAll(jobDir)

	return finalName, zipBase, nil
}

// playlistRetryWaits are the pauses before each retry of refused tracks.
var playlistRetryWaits = []time.Duration{30 * time.Second, 90 * time.Second}

// runYtdlpLogged runs one yt-dlp process, keeps its output in the job log and
// returns the last ERROR line ("" if none). The context ends it on cancel.
func runYtdlpLogged(ctx context.Context, job *Job, args []string) string {
	cmd := lowPriorityCmd(ctx, cfg().YtdlpPath, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err.Error()
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err.Error()
	}
	lowerPriority(cmd)
	var lastError string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		job.mu.Lock()
		job.Logs = append(job.Logs, line)
		if len(job.Logs) > 50 {
			job.Logs = job.Logs[len(job.Logs)-50:]
		}
		job.mu.Unlock()
		if strings.Contains(line, "ERROR") {
			lastError = line
		}
	}
	cmd.Wait()
	return lastError
}

// dirNames lists the regular files in dir.
func dirNames(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// playlistIndex reads the album position from a "12. Title.ext" file name.
func playlistIndex(name string) (int, bool) {
	dot := strings.Index(name, ". ")
	if dot < 1 {
		return 0, false
	}
	n, err := strconv.Atoi(name[:dot])
	return n, err == nil && n > 0
}

// missingPlaylistItems returns the album positions without an audio file, in
// order, with a label for each ("9. Shirin Jaan" when a leftover file names
// the track, else "9"). total is the playlist size; 0 (unknown) limits the
// check to positions that left other files (e.g. a cover) behind.
func missingPlaylistItems(names []string, total int, audioFormat string) ([]int, map[int]string) {
	audio := map[int]bool{}
	labels := map[int]string{}
	for _, name := range names {
		i, ok := playlistIndex(name)
		if !ok {
			continue
		}
		ext := filepath.Ext(name)
		if strings.EqualFold(ext, "."+audioFormat) {
			audio[i] = true
		} else if labels[i] == "" && !strings.HasSuffix(name, ".part") && !strings.HasSuffix(name, ".ytdl") {
			labels[i] = strings.TrimSuffix(name, ext)
		}
	}
	var missing []int
	if total > 0 {
		for i := 1; i <= total; i++ {
			if !audio[i] {
				missing = append(missing, i)
			}
		}
	} else {
		for i := range labels {
			if !audio[i] {
				missing = append(missing, i)
			}
		}
		sort.Ints(missing)
	}
	out := map[int]string{}
	for _, i := range missing {
		if labels[i] != "" {
			out[i] = labels[i]
		} else {
			out[i] = strconv.Itoa(i)
		}
	}
	return missing, out
}

// joinInts renders positions as yt-dlp --playlist-items ("1,9,12").
func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

// playlistTitleFromLine extracts the title from yt-dlp's extractor banner,
// e.g. "[bandcamp] Playlist Best Of Foo:" or
// "[youtube:tab] Playlist Foo: Downloading 9 items of 9". Returns "" otherwise.
func playlistTitleFromLine(line string) string {
	idx := strings.Index(line, "Playlist ")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(line[idx+len("Playlist "):])
	if i := strings.LastIndex(rest, ": Downloading "); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(strings.TrimSuffix(rest, ":"))
}

// createStoredZip writes a ZIP archive at dst containing the given entries
// from srcDir, using the Store method (no compression).
func createStoredZip(dst, srcDir string, entries []os.DirEntry) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	for _, e := range entries {
		src := filepath.Join(srcDir, e.Name())
		info, err := e.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = e.Name()
		header.Method = zip.Store
		w, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, in); err != nil {
			in.Close()
			return err
		}
		in.Close()
	}
	return nil
}

// sanitizeFilename strips path separators and other awkward characters so
// `name` is safe to use as a single filesystem component.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f:
			// drop control chars
		case r == '/', r == '\\', r == ':', r == '*', r == '?', r == '"', r == '<', r == '>', r == '|':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "playlist"
	}
	if len(out) > 80 {
		cut := 80
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = strings.TrimSpace(out[:cut])
	}
	return out
}

func setJobError(job *Job, msg string) {
	job.mu.Lock()
	job.Status = "error"
	job.Error = msg
	job.Logs = nil // free log memory on error
	job.notifySubscribers()
	jobURL := job.URL
	kind := job.Kind
	job.mu.Unlock()

	// Passively flag cookies as suspect if the failure message looks auth-related.
	// Direct downloads never use the cookie file, so their 403s say nothing about it.
	if kind != kindDirect && kind != kindTorrent {
		recordAuthFailure(jobURL, msg)
	}

	appendJobHistory(JobRecord{
		ID:          job.ID,
		URL:         job.URL,
		FormatID:    job.FormatID,
		Status:      "error",
		Error:       msg,
		Owner:       job.Owner,
		Title:       job.Title,
		Kind:        job.Kind,
		Destination: job.Destination,
		CreatedAt:   job.CreatedAt,
		FinishedAt:  time.Now(),
	})
}

// ---------------------------------------------------------------------------
// Admin: page rendering
// ---------------------------------------------------------------------------

func generateSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func renderAdminHTML(csrf, adminUser string) []byte {
	html := adminHTML
	if html == "" {
		data, err := embedded.ReadFile("templates/admin.html")
		if err != nil {
			return nil
		}
		html = strings.ReplaceAll(string(data), "<!--APP_VERSION-->", versionHTML())
	}
	html = strings.ReplaceAll(html, "<!--CSRF_TOKEN-->", htmlpkg.EscapeString(csrf))
	html = strings.ReplaceAll(html, "<!--ADMIN_USER-->", htmlpkg.EscapeString(adminUser))
	// Injected into a data- attribute, so it must be attribute-escaped.
	defaultData, _ := embedded.ReadFile("config.default.json")
	html = strings.ReplaceAll(html, "<!--DEFAULTS_JSON-->", htmlpkg.EscapeString(string(defaultData)))
	return []byte(html)
}

// ---------------------------------------------------------------------------
// Admin: handlers
// ---------------------------------------------------------------------------

// minAdminPasswordLen is the single source of truth for the password policy —
// the HTML form and the JSON API must not disagree about it.
const minAdminPasswordLen = 8

func validateAdminPassword(pw string) error {
	if len(pw) < minAdminPasswordLen {
		return fmt.Errorf("Password must be at least %d characters.", minAdminPasswordLen)
	}
	switch strings.ToLower(pw) {
	case "admin", "password", "changeme", "leecher":
		return fmt.Errorf("Please choose a stronger password.")
	}
	return nil
}

// adminWriteAllowed throttles account and admin mutations to 10/min per caller.
// A stolen session should not grant unlimited deletes, uploads or updates.
func adminWriteAllowed(w http.ResponseWriter, r *http.Request) bool {
	ip := clientIP(r)
	if limiter.allow(requestActor(r)+":admin-write", 10.0/60.0, 20) {
		return true
	}
	logger.Warn("Admin write rate limited", "ip", ip, "path", r.URL.Path)
	jsonError(w, "Too many admin requests. Please slow down.", http.StatusTooManyRequests)
	return false
}

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin" {
		http.NotFound(w, r)
		return
	}
	if !isAdminAuthenticated(r) {
		http.Redirect(w, r, "/login?next=%2Fadmin", http.StatusFound)
		return
	}
	a := currentAuth(r)
	html := renderAdminHTML(csrfTokenForSession(a.SessionToken), a.User.Username)
	if html == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(html)
}

// AdminFile represents a file in the downloads directory for the admin API.
type AdminFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SizeMB   string `json:"size_mb"`
	Modified string `json:"modified"`
	Path     string `json:"path"`
}

func handleAdminFiles(w http.ResponseWriter, r *http.Request) {
	if !isAdminAuthenticated(r) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}

	var entries []os.DirEntry
	for _, dir := range fileAreas() {
		es, err := os.ReadDir(dir)
		if err == nil {
			entries = append(entries, es...)
		}
	}

	// Load timezone once for all files
	configMu.RLock()
	tz := config.TimeZone
	configMu.RUnlock()
	loc, _ := time.LoadLocation(tz)

	type adminFileEntry struct {
		name    string
		size    int64
		modTime time.Time
	}

	var rows []adminFileEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rows = append(rows, adminFileEntry{name: name, size: info.Size(), modTime: info.ModTime()})
	}

	// Sort newest first
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].modTime.After(rows[j].modTime)
	})

	files := make([]AdminFile, 0, len(rows))
	for _, row := range rows {
		mb := float64(row.size) / (1024 * 1024)
		modTime := row.modTime
		if loc != nil {
			modTime = modTime.In(loc)
		}
		files = append(files, AdminFile{
			Name:     row.name,
			Size:     row.size,
			SizeMB:   fmt.Sprintf("%.1f MB", mb),
			Modified: modTime.Format("2006-01-02 15:04:05"),
			Path:     "/downloads/" + row.name,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"files": files})
}

func handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isAdminAuthenticated(r) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}
	if !validateCSRF(r) {
		jsonError(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}
	if !adminWriteAllowed(w, r) {
		return
	}

	filename := strings.TrimPrefix(r.URL.Path, "/api/admin/delete/")
	if filename == "" {
		jsonError(w, "No filename", http.StatusBadRequest)
		return
	}

	// Sanitize: only allow base filenames, no path traversal
	clean := filepath.Base(filepath.Clean(filename))
	if clean != filename || strings.Contains(clean, "..") {
		jsonError(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	fullPath := findTempFile(clean)
	if fullPath == "" {
		jsonError(w, "File not found", http.StatusNotFound)
		return
	}

	if err := os.Remove(fullPath); err != nil {
		jsonError(w, "Failed to delete: "+err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Info("Admin deleted file", "file", clean)
	notifyFileChange()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// settingsPayload is the subset of Config exposed via the admin settings API.
type settingsPayload struct {
	MaxFileAgeMinutes      int    `json:"max_file_age_minutes"`
	DownloadTimeoutMinutes int    `json:"download_timeout_minutes"`
	MaxConcurrentDownloads int    `json:"max_concurrent_downloads"`
	MaxDownloadsPerIP      int    `json:"max_downloads_per_ip"`
	MaxFileSizeMB          int    `json:"max_file_size_mb"`
	MaxStorageMB           int    `json:"max_storage_mb"`
	MinDiskFreeMB          int    `json:"min_disk_free_mb"`
	SessionHours           int    `json:"session_hours"`
	RateLimitFormats       int    `json:"rate_limit_formats"`
	RateLimitDownloads     int    `json:"rate_limit_downloads"`
	TimeZone               string `json:"time_zone"`
	DebugMode              bool   `json:"debug_mode"`
	ProbeConcurrency       int    `json:"probe_concurrency"`
	ProbeQueueMax          int    `json:"probe_queue_max"`
	ProbeTimeoutSecs       int    `json:"probe_timeout_secs"`
	DownloadQueueMax       int    `json:"download_queue_max"`
	CookiesFile            string `json:"cookies_file"`

	LibraryDir              string `json:"library_dir"`
	DirectDownloads         bool   `json:"direct_downloads"`
	DirectTimeoutMinutes    int    `json:"direct_timeout_minutes"`
	TransmissionURL         string `json:"transmission_url"`
	TransmissionDownloadDir string `json:"transmission_download_dir"`

	TorrentEngine     string  `json:"torrent_engine"`
	SeedRatio         float64 `json:"seed_ratio"`
	SeedMinutes       int     `json:"seed_minutes"`
	MaxActiveTorrents int     `json:"max_active_torrents"`

	KillSwitch             bool     `json:"kill_switch"`
	KillSwitchIPs          []string `json:"kill_switch_ips"`
	KillSwitchIntervalSecs int      `json:"kill_switch_interval_secs"`
}

func handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if !isAdminAuthenticated(r) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		configMu.RLock()
		payload := settingsPayload{
			MaxFileAgeMinutes:      config.MaxFileAgeMinutes,
			DownloadTimeoutMinutes: config.DownloadTimeoutMinutes,
			MaxConcurrentDownloads: config.MaxConcurrentDownloads,
			MaxDownloadsPerIP:      config.MaxDownloadsPerIP,
			MaxFileSizeMB:          config.MaxFileSizeMB,
			MaxStorageMB:           config.MaxStorageMB,
			MinDiskFreeMB:          config.MinDiskFreeMB,
			SessionHours:           config.SessionHours,
			RateLimitFormats:       config.RateLimitFormats,
			RateLimitDownloads:     config.RateLimitDownloads,
			TimeZone:               config.TimeZone,
			DebugMode:              config.DebugMode,
			ProbeConcurrency:       config.ProbeConcurrency,
			ProbeQueueMax:          config.ProbeQueueMax,
			ProbeTimeoutSecs:       config.ProbeTimeoutSecs,
			DownloadQueueMax:       config.DownloadQueueMax,
			CookiesFile:            config.CookiesFile,

			LibraryDir:              config.LibraryDir,
			DirectDownloads:         config.DirectDownloads,
			DirectTimeoutMinutes:    config.DirectTimeoutMinutes,
			TransmissionURL:         config.TransmissionURL,
			TransmissionDownloadDir: config.TransmissionDownloadDir,

			TorrentEngine:     torrentEngine(),
			SeedRatio:         config.SeedRatio,
			SeedMinutes:       config.SeedMinutes,
			MaxActiveTorrents: config.MaxActiveTorrents,

			KillSwitch:             config.KillSwitch,
			KillSwitchIPs:          append([]string{}, config.KillSwitchIPs...),
			KillSwitchIntervalSecs: config.KillSwitchIntervalSecs,
		}
		configMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)

	case http.MethodPost:
		if !validateCSRF(r) {
			jsonError(w, "Invalid CSRF token", http.StatusForbidden)
			return
		}
		if !adminWriteAllowed(w, r) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		var payload settingsPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			jsonError(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Validate ranges
		if payload.MaxFileAgeMinutes < 1 {
			jsonError(w, "max_file_age_minutes must be >= 1", http.StatusBadRequest)
			return
		}
		if payload.DownloadTimeoutMinutes < 1 {
			jsonError(w, "download_timeout_minutes must be >= 1", http.StatusBadRequest)
			return
		}
		if payload.MaxConcurrentDownloads < 1 || payload.MaxConcurrentDownloads > 10 {
			jsonError(w, "max_concurrent_downloads must be 1-10", http.StatusBadRequest)
			return
		}
		if payload.MaxDownloadsPerIP < 1 || payload.MaxDownloadsPerIP > 10 {
			jsonError(w, "max_downloads_per_ip must be 1-10", http.StatusBadRequest)
			return
		}
		if payload.MaxFileSizeMB < 0 {
			jsonError(w, "max_file_size_mb must be >= 0", http.StatusBadRequest)
			return
		}
		if payload.MaxStorageMB < 0 {
			jsonError(w, "max_storage_mb must be >= 0", http.StatusBadRequest)
			return
		}
		if payload.MinDiskFreeMB < 0 {
			jsonError(w, "min_disk_free_mb must be >= 0", http.StatusBadRequest)
			return
		}
		if payload.SessionHours < 1 {
			jsonError(w, "session_hours must be >= 1", http.StatusBadRequest)
			return
		}
		if payload.RateLimitFormats < 1 || payload.RateLimitFormats > 60 {
			jsonError(w, "rate_limit_formats must be 1-60", http.StatusBadRequest)
			return
		}
		if payload.RateLimitDownloads < 1 || payload.RateLimitDownloads > 60 {
			jsonError(w, "rate_limit_downloads must be 1-60", http.StatusBadRequest)
			return
		}
		if payload.ProbeConcurrency < 1 || payload.ProbeConcurrency > 20 {
			jsonError(w, "probe_concurrency must be 1-20", http.StatusBadRequest)
			return
		}
		if payload.ProbeQueueMax < 1 || payload.ProbeQueueMax > 500 {
			jsonError(w, "probe_queue_max must be 1-500", http.StatusBadRequest)
			return
		}
		if payload.DownloadQueueMax < 1 || payload.DownloadQueueMax > 200 {
			jsonError(w, "download_queue_max must be 1-200", http.StatusBadRequest)
			return
		}
		if payload.ProbeTimeoutSecs < 5 || payload.ProbeTimeoutSecs > 600 {
			jsonError(w, "probe_timeout_secs must be 5-600", http.StatusBadRequest)
			return
		}
		if payload.DirectTimeoutMinutes < 1 || payload.DirectTimeoutMinutes > 1440 {
			jsonError(w, "direct_timeout_minutes must be 1-1440", http.StatusBadRequest)
			return
		}
		libDir, err := validateLibraryDir(payload.LibraryDir)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		payload.LibraryDir = libDir
		if err := validateTransmissionSettings(&payload.TransmissionURL, &payload.TransmissionDownloadDir); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch payload.TorrentEngine {
		case engineBuiltin, engineTransmission, engineOff:
		default:
			jsonError(w, "torrent_engine must be builtin, transmission or off", http.StatusBadRequest)
			return
		}
		if payload.SeedRatio < 0 || payload.SeedRatio > 100 {
			jsonError(w, "seed_ratio must be 0-100", http.StatusBadRequest)
			return
		}
		if payload.SeedMinutes < 0 || payload.SeedMinutes > 100000 {
			jsonError(w, "seed_minutes must be 0-100000", http.StatusBadRequest)
			return
		}
		if payload.MaxActiveTorrents < 0 || payload.MaxActiveTorrents > 50 {
			jsonError(w, "max_active_torrents must be 0-50", http.StatusBadRequest)
			return
		}
		ksIPs, err := normalizeAllowedIPs(payload.KillSwitchIPs)
		if err != nil {
			jsonError(w, "Kill switch: "+err.Error(), http.StatusBadRequest)
			return
		}
		payload.KillSwitchIPs = ksIPs
		if payload.KillSwitch && len(ksIPs) == 0 {
			jsonError(w, "Kill switch: add at least one allowed IP (use Detect current IP).", http.StatusBadRequest)
			return
		}
		if payload.KillSwitchIntervalSecs < 1 || payload.KillSwitchIntervalSecs > 300 {
			jsonError(w, "kill_switch_interval_secs must be 1-300", http.StatusBadRequest)
			return
		}
		if payload.TimeZone != "" {
			if _, err := time.LoadLocation(payload.TimeZone); err != nil {
				jsonError(w, "Invalid time_zone: "+payload.TimeZone, http.StatusBadRequest)
				return
			}
		} else {
			payload.TimeZone = "UTC"
		}

		configMu.Lock()
		config.MaxFileAgeMinutes = payload.MaxFileAgeMinutes
		config.DownloadTimeoutMinutes = payload.DownloadTimeoutMinutes
		config.MaxConcurrentDownloads = payload.MaxConcurrentDownloads
		config.MaxDownloadsPerIP = payload.MaxDownloadsPerIP
		config.MaxFileSizeMB = payload.MaxFileSizeMB
		config.MaxStorageMB = payload.MaxStorageMB
		config.MinDiskFreeMB = payload.MinDiskFreeMB
		config.SessionHours = payload.SessionHours
		config.RateLimitFormats = payload.RateLimitFormats
		config.RateLimitDownloads = payload.RateLimitDownloads
		config.TimeZone = payload.TimeZone
		config.DebugMode = payload.DebugMode
		config.ProbeConcurrency = payload.ProbeConcurrency
		config.ProbeQueueMax = payload.ProbeQueueMax
		config.ProbeTimeoutSecs = payload.ProbeTimeoutSecs
		config.DownloadQueueMax = payload.DownloadQueueMax
		config.LibraryDir = payload.LibraryDir
		config.DirectDownloads = payload.DirectDownloads
		config.DirectTimeoutMinutes = payload.DirectTimeoutMinutes
		config.TransmissionURL = payload.TransmissionURL
		config.TransmissionDownloadDir = payload.TransmissionDownloadDir
		config.TorrentEngine = payload.TorrentEngine
		config.SeedRatio = payload.SeedRatio
		config.SeedMinutes = payload.SeedMinutes
		config.MaxActiveTorrents = payload.MaxActiveTorrents
		ksChanged := config.KillSwitch != payload.KillSwitch || !slices.Equal(config.KillSwitchIPs, payload.KillSwitchIPs)
		config.KillSwitch = payload.KillSwitch
		config.KillSwitchIPs = payload.KillSwitchIPs
		config.KillSwitchIntervalSecs = payload.KillSwitchIntervalSecs
		// Note: CookiesFile is managed exclusively by handleAdminCookies (upload/clear).
		// Do NOT mutate it here — the settings UI doesn't include it in its payload,
		// so writing payload.CookiesFile would wipe an uploaded cookies.txt on any save.
		publishConfig()
		newDlConcurrency := payload.MaxConcurrentDownloads
		newProbeConcurrency := payload.ProbeConcurrency
		newIPMax := payload.MaxDownloadsPerIP

		// Persist to disk
		data, err := json.MarshalIndent(config, "", "  ")
		configMu.Unlock()

		// Resize semaphores live — immediately grants extra slots to queued waiters.
		dlSem.resize(newDlConcurrency)
		probeSem.resize(newProbeConcurrency)
		resizeIPSems(newIPMax)
		if ksChanged {
			// Re-check right away; otherwise a changed list only applies at the next tick.
			ksResetChecked()
			kickKillSwitch()
		}

		if err != nil {
			jsonError(w, "Failed to serialize config", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(configPath, data, 0600); err != nil {
			logger.Warn("Settings applied in memory but failed to save", "error", err)
		} else {
			logger.Info("Settings saved", "path", configPath)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Cookie file health checking
// ---------------------------------------------------------------------------
//
// Rather than pinging the target sites to validate credentials (which would
// burn the account's reputation with bot-detection), we do two cheap things:
//
//   1. Parse the Netscape cookies.txt file and report, per platform, whether
//      the session cookies are present and when the earliest of them expires.
//   2. Whenever a download fails with a message that strongly indicates an
//      auth problem, we record the host+timestamp so the admin page can show
//      "last auth failure" as a passive hint that the cookies may be stale.
//
// Neither step makes any outbound request.

// cookiePlatform describes the session cookies we look for per platform.
type cookiePlatform struct {
	Name     string   // display label
	Domains  []string // substrings to match against the Netscape "domain" column
	Required []string // cookie names that must all be present
}

var cookiePlatforms = []cookiePlatform{
	{"Instagram", []string{"instagram.com"}, []string{"sessionid", "ds_user_id"}},
	{"YouTube", []string{"youtube.com", ".google.com"}, []string{"SAPISID"}},
	{"Twitter/X", []string{"twitter.com", "x.com"}, []string{"auth_token", "ct0"}},
	{"TikTok", []string{"tiktok.com"}, []string{"sessionid"}},
	{"Facebook", []string{"facebook.com"}, []string{"c_user", "xs"}},
	{"Reddit", []string{"reddit.com"}, []string{"reddit_session"}},
}

// PlatformCookieStatus is the per-platform report for the admin UI.
type PlatformCookieStatus struct {
	Name           string   `json:"name"`
	Present        bool     `json:"present"`
	Missing        []string `json:"missing,omitempty"`
	EarliestExpiry int64    `json:"earliest_expiry,omitempty"` // unix seconds, 0 = session / unknown
	ExpiresInDays  int      `json:"expires_in_days,omitempty"`
	Expired        bool     `json:"expired,omitempty"`
}

// AuthFailureRecord captures the most recent suspected-auth failure.
type AuthFailureRecord struct {
	Host  string    `json:"host"`
	URL   string    `json:"url"`
	At    time.Time `json:"at"`
	Error string    `json:"error"`
}

// CookieStatus is the payload returned by GET /api/admin/cookies.
type CookieStatus struct {
	Path            string                 `json:"path"`
	Present         bool                   `json:"present"`
	Size            int64                  `json:"size,omitempty"`
	ModifiedAt      time.Time              `json:"modified_at,omitempty"`
	TotalCookies    int                    `json:"total_cookies,omitempty"`
	Platforms       []PlatformCookieStatus `json:"platforms,omitempty"`
	LastAuthFailure *AuthFailureRecord     `json:"last_auth_failure,omitempty"`
}

var (
	lastAuthFailure   *AuthFailureRecord
	lastAuthFailureMu sync.RWMutex
)

// authFailureIndicators are case-insensitive substrings that strongly suggest
// the cookies are no longer valid (as opposed to a transient network error).
// We keep this conservative to avoid false positives.
var authFailureIndicators = []string{
	"login required",
	"this video is private",
	"please log in",
	"sign in to confirm",
	"http error 401",
	"challenge_required",
	"cookies are no longer valid",
	"requires authentication",
	"only available for registered users",
	"members-only content",
	"restricted video",
}

// recordAuthFailure inspects a failure message and, if it looks
// auth-related, stores it for later display on the admin page.
func recordAuthFailure(jobURL, errMsg string) {
	lower := strings.ToLower(errMsg)
	matched := false
	for _, ind := range authFailureIndicators {
		if strings.Contains(lower, ind) {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	host := ""
	if u, err := urlpkg.Parse(jobURL); err == nil {
		host = u.Host
	}
	// Keep the recorded error short to avoid a huge payload on the admin page.
	trimmed := strings.TrimSpace(errMsg)
	if len(trimmed) > 500 {
		trimmed = trimmed[:500] + "…"
	}
	lastAuthFailureMu.Lock()
	lastAuthFailure = &AuthFailureRecord{
		Host:  host,
		URL:   jobURL,
		At:    time.Now(),
		Error: trimmed,
	}
	lastAuthFailureMu.Unlock()
}

// loadCookieStatus parses the currently-configured cookies.txt and returns
// a CookieStatus describing its health. Never performs network I/O.
func loadCookieStatus() CookieStatus {
	configMu.RLock()
	path := config.CookiesFile
	configMu.RUnlock()

	status := CookieStatus{Path: path}

	lastAuthFailureMu.RLock()
	if lastAuthFailure != nil {
		cp := *lastAuthFailure
		status.LastAuthFailure = &cp
	}
	lastAuthFailureMu.RUnlock()

	if path == "" {
		return status
	}
	info, err := os.Stat(path)
	if err != nil {
		return status
	}
	status.Present = true
	status.Size = info.Size()
	status.ModifiedAt = info.ModTime()

	data, err := os.ReadFile(path)
	if err != nil {
		return status
	}

	// Parse Netscape format: domain \t flag \t path \t secure \t expiry \t name \t value
	// Track earliest expiry per platform and required-cookie presence.
	type platformState struct {
		have           map[string]bool
		earliestExpiry int64 // 0 == session/unset, else earliest non-zero expiry seen
	}
	platforms := make(map[int]*platformState, len(cookiePlatforms))
	for i := range cookiePlatforms {
		platforms[i] = &platformState{have: make(map[string]bool)}
	}

	total := 0
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(rawLine, "\t")
		if len(parts) < 7 {
			continue
		}
		domain := parts[0]
		expiryStr := parts[4]
		name := parts[5]
		total++

		expiry, _ := strconv.ParseInt(expiryStr, 10, 64)

		for i, p := range cookiePlatforms {
			matches := false
			for _, d := range p.Domains {
				if strings.Contains(domain, d) {
					matches = true
					break
				}
			}
			if !matches {
				continue
			}
			// Is this one of the required cookies?
			for _, req := range p.Required {
				if req == name {
					ps := platforms[i]
					ps.have[name] = true
					if expiry > 0 && (ps.earliestExpiry == 0 || expiry < ps.earliestExpiry) {
						ps.earliestExpiry = expiry
					}
				}
			}
		}
	}
	status.TotalCookies = total

	now := time.Now().Unix()
	status.Platforms = make([]PlatformCookieStatus, 0, len(cookiePlatforms))
	for i, p := range cookiePlatforms {
		ps := platforms[i]
		out := PlatformCookieStatus{Name: p.Name}
		missing := make([]string, 0)
		for _, req := range p.Required {
			if !ps.have[req] {
				missing = append(missing, req)
			}
		}
		out.Present = len(missing) == 0
		if !out.Present {
			out.Missing = missing
		}
		if ps.earliestExpiry > 0 {
			out.EarliestExpiry = ps.earliestExpiry
			if ps.earliestExpiry < now {
				out.Expired = true
			} else {
				out.ExpiresInDays = int((ps.earliestExpiry - now) / 86400)
			}
		}
		// Only include platforms that the user has at least one matching
		// cookie for, to keep the UI focused; a platform with zero cookies
		// at all for its domain is just noise.
		hasAnyCookie := len(ps.have) > 0 || len(missing) < len(p.Required)
		if hasAnyCookie {
			status.Platforms = append(status.Platforms, out)
		}
	}
	return status
}

// handleAdminCookies accepts cookies.txt uploads and clears them.
// POST multipart/form-data with "file" field -> saves to <exe_dir>/.cache/cookies.txt and sets config.CookiesFile.
// DELETE -> removes the file and clears config.CookiesFile.
// GET -> returns a CookieStatus describing parsed health + last auth failure.
func handleAdminCookies(w http.ResponseWriter, r *http.Request) {
	if !isAdminAuthenticated(r) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}
	// GET is safe and intentionally excluded from CSRF (no state change).
	if r.Method != http.MethodGet && !validateCSRF(r) {
		jsonError(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && !adminWriteAllowed(w, r) {
		return
	}

	cookieDir := filepath.Join(appHome(), ".cache")
	cookiePath := filepath.Join(cookieDir, "cookies.txt")

	switch r.Method {
	case http.MethodGet:
		// The cookie status includes "expires in N days" which is time-sensitive;
		// without no-store browsers will happily serve a stale cached payload for
		// days and the chip will appear frozen.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(loadCookieStatus())
		return
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1024*1024) // 1MB max
		if err := r.ParseMultipartForm(1024 * 1024); err != nil {
			jsonError(w, "Upload too large or invalid", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			jsonError(w, "Missing file field", http.StatusBadRequest)
			return
		}
		defer file.Close()
		if header.Size > 1024*1024 {
			jsonError(w, "File too large", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(file)
		if err != nil {
			jsonError(w, "Failed to read upload", http.StatusInternalServerError)
			return
		}
		// Basic sanity check: must look like a Netscape cookies file
		content := string(data)
		if !strings.Contains(content, "\t") {
			jsonError(w, "Not a valid cookies.txt file (expected tab-separated Netscape format)", http.StatusBadRequest)
			return
		}

		if err := os.MkdirAll(cookieDir, 0700); err != nil {
			jsonError(w, "Failed to create cache dir", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(cookiePath, data, 0600); err != nil {
			jsonError(w, "Failed to save cookies", http.StatusInternalServerError)
			return
		}

		configMu.Lock()
		config.CookiesFile = cookiePath
		publishConfig()
		cfgData, _ := json.MarshalIndent(config, "", "  ")
		configMu.Unlock()

		if err := os.WriteFile(configPath, cfgData, 0600); err != nil {
			logger.Warn("Cookies saved but config update failed", "error", err)
		}

		logger.Info("Cookies uploaded", "path", cookiePath, "size", len(data))
		lastAuthFailureMu.Lock()
		lastAuthFailure = nil
		lastAuthFailureMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "path": cookiePath})

	case http.MethodDelete:
		os.Remove(cookiePath)

		configMu.Lock()
		config.CookiesFile = ""
		publishConfig()
		cfgData, _ := json.MarshalIndent(config, "", "  ")
		configMu.Unlock()

		if err := os.WriteFile(configPath, cfgData, 0600); err != nil {
			logger.Warn("Cookies cleared but config update failed", "error", err)
		}

		logger.Info("Cookies cleared")
		lastAuthFailureMu.Lock()
		lastAuthFailure = nil
		lastAuthFailureMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func cleanupLoop() {
	// On startup, do an immediate cleanup pass
	cleanupOrphanDirs()
	cleanupFiles()

	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cleanupOrphanDirs()
			cleanupFiles()
			pruneFinishedJobs()
			pruneExpiredSessions()
			pruneIPSems(ipSemTTL)
			pruneThumbnails()
		case <-shutdownCh:
			return
		}
	}
}

// pruneFinishedJobs removes completed/errored jobs from memory after 10 minutes
// to free RAM on resource-constrained devices.
func pruneFinishedJobs() {
	cutoff := time.Now().Add(-10 * time.Minute)
	jobsMu.Lock()
	for id, job := range jobs {
		job.mu.Lock()
		done := job.Status == "done" || job.Status == "error" || job.Status == "cancelled"
		old := job.CreatedAt.Before(cutoff)
		job.mu.Unlock()
		if done && old {
			delete(jobs, id)
		}
	}
	jobsMu.Unlock()
}

// cleanupOrphanDirs removes leftover job subdirectories (from crashes).
func cleanupOrphanDirs() {
	for _, dir := range fileAreas() {
		cleanupOrphanDirsIn(dir)
	}
}

func cleanupOrphanDirsIn(downloadsDir string) {
	entries, err := os.ReadDir(downloadsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		// Finished torrent folders are named <id>_<name>; only bare <id>
		// folders are per-job scratch space.
		if !e.IsDir() || !jobIDNameRe.MatchString(e.Name()) {
			continue
		}
		dirPath := filepath.Join(downloadsDir, e.Name())
		// If there's an active job with this ID, skip it
		jobsMu.RLock()
		_, active := jobs[e.Name()]
		jobsMu.RUnlock()
		if active {
			continue
		}
		logger.Info("Removing orphan job dir", "dir", e.Name())
		os.RemoveAll(dirPath)
	}
}

// cleanupFiles handles both age-based cleanup and storage limit enforcement
// in a single directory scan to minimize I/O on slow storage.
func cleanupFiles() {
	c := cfg()
	maxAge := time.Duration(c.MaxFileAgeMinutes) * time.Minute
	maxBytes := int64(0)
	if c.MaxStorageMB > 0 {
		maxBytes = int64(c.MaxStorageMB) * 1024 * 1024
	}
	now := time.Now()

	type fileEntry struct {
		name    string // full path
		size    int64
		modTime time.Time
	}
	type dirEntry struct {
		dir string
		e   os.DirEntry
	}
	var entries []dirEntry
	for _, dir := range fileAreas() {
		es, _ := os.ReadDir(dir)
		for _, e := range es {
			entries = append(entries, dirEntry{dir, e})
		}
	}

	var surviving []fileEntry
	var totalSize int64
	removed := false

	// Pass 1: remove expired files by age
	for _, de := range entries {
		e := de.e
		// Dot-files are leecher's own state (e.g. .job_history.json); bare
		// <id> folders belong to running jobs.
		if strings.HasPrefix(e.Name(), ".") || (e.IsDir() && !downloadEntryRe.MatchString(e.Name())) || entryInUse(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(de.dir, e.Name())
		size := info.Size()
		if e.IsDir() {
			size = treeSize(full)
		}
		if now.Sub(info.ModTime()) > maxAge {
			logger.Info("Cleanup: removing expired download", "entry", e.Name(), "age", now.Sub(info.ModTime()).Round(time.Second))
			os.RemoveAll(full)
			removed = true
		} else {
			surviving = append(surviving, fileEntry{name: full, size: size, modTime: info.ModTime()})
			totalSize += info.Size()
		}
	}

	// Pass 2: enforce storage limit on surviving files
	if maxBytes > 0 && totalSize > maxBytes && len(surviving) > 0 {
		sort.Slice(surviving, func(i, j int) bool { return surviving[i].modTime.Before(surviving[j].modTime) })
		for _, f := range surviving {
			if totalSize <= maxBytes {
				break
			}
			logger.Info("Storage limit: removing oldest file", "file", filepath.Base(f.name), "size_mb", f.size/(1024*1024))
			if err := os.RemoveAll(f.name); err == nil {
				totalSize -= f.size
				removed = true
			}
		}
	}

	if removed {
		notifyFileChange()
	}

	// Also prune stale finished jobs from memory (history keeps them). Running
	// jobs must stay, however long a big direct download takes.
	jobsMu.Lock()
	for id, job := range jobs {
		job.mu.Lock()
		finished := job.Status == "done" || job.Status == "error" || job.Status == "cancelled"
		job.mu.Unlock()
		if finished && now.Sub(job.CreatedAt) > maxAge {
			delete(jobs, id)
		}
	}
	jobsMu.Unlock()
}

// Cached directory size to avoid repeated FS scans on slow storage (SD cards).
var (
	cachedDirSizeMB int64
	cachedDirSizeAt time.Time
	cachedDirSizeMu sync.Mutex
)

// dirSizeMB returns the total size of all files in the downloads directory in MB.
// Results are cached for 60 seconds to reduce I/O on slow storage.
func dirSizeMB() int64 {
	cachedDirSizeMu.Lock()
	defer cachedDirSizeMu.Unlock()
	if time.Since(cachedDirSizeAt) < 60*time.Second {
		return cachedDirSizeMB
	}
	var entries []os.DirEntry
	for _, dir := range fileAreas() {
		es, _ := os.ReadDir(dir)
		entries = append(entries, es...)
	}
	var total int64
	for _, dir := range fileAreas() {
		es, _ := os.ReadDir(dir)
		for _, e := range es {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if e.IsDir() {
				if downloadEntryRe.MatchString(e.Name()) {
					total += treeSize(filepath.Join(dir, e.Name()))
				}
				continue
			}
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	_ = entries
	cachedDirSizeMB = total / (1024 * 1024)
	cachedDirSizeAt = time.Now()
	return cachedDirSizeMB
}

// invalidateDirSizeCache forces the next dirSizeMB() call to re-scan.
func invalidateDirSizeCache() {
	cachedDirSizeMu.Lock()
	cachedDirSizeAt = time.Time{}
	cachedDirSizeMu.Unlock()
}

func sortFormats(formats []Format) {
	sort.SliceStable(formats, func(i, j int) bool {
		a, b := formats[i], formats[j]
		if a.HasVideo != b.HasVideo {
			return a.HasVideo && !b.HasVideo
		}
		if a.Height != b.Height {
			return a.Height > b.Height
		}
		return a.Filesize > b.Filesize
	})
}

// generateID returns a random job identifier. It must be unguessable: job IDs
// address the status, cancel and WebSocket endpoints.
func generateID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// cookieArgs returns yt-dlp arguments for cookie authentication.
func cookieArgs() []string {
	c := cfg()
	if c.CookiesFile != "" {
		return []string{"--cookies", c.CookiesFile}
	}
	if c.CookiesFromBrowser != "" {
		return []string{"--cookies-from-browser", c.CookiesFromBrowser}
	}
	return nil
}

// cacheArgs returns --cache-dir pointing to a persistent writable location next to the binary.
// Using a persistent dir (not /tmp tmpfs) means yt-dlp's player JS and API caches survive
// across probes and restarts, avoiding redundant network fetches on every run.
func cacheArgs() []string {
	cacheDir := filepath.Join(appHome(), ".cache", "ytdlp")
	return []string{"--cache-dir", cacheDir}
}

// youtubeClientArgs asks yt-dlp to query several YouTube player clients in a
// single invocation and union their format lists. This maximizes the chance
// of getting high-quality formats (e.g. 4K from `web`) even when some clients
// are captcha-walled or rate-limited. yt-dlp silently skips clients that fail.
//
// Strategy depends on whether we have cookies:
//   - With cookies: use cookie-aware web clients. They expose the full 4K/8K
//     DASH catalog and authentication bypasses YouTube's captcha walls.
//     yt-dlp would skip non-cookie clients with a warning ("does not support
//     cookies") so listing them just adds log noise.
//   - Without cookies: use tv_simply/android/ios which work anonymously.
//     `tv` is currently in a YouTube DRM A/B experiment (yt-dlp/yt-dlp#12563)
//     so we exclude it. `tv_embedded` was removed upstream.
//
// The arg is namespaced to `youtube:`, so it is safely ignored by other
// extractors and may be passed unconditionally.
func youtubeClientArgs() []string {
	configMu.RLock()
	hasCookies := config.CookiesFile != "" || config.CookiesFromBrowser != ""
	configMu.RUnlock()

	var clients string
	if hasCookies {
		clients = "default,web,web_safari,web_creator,web_embedded,mweb"
	} else {
		clients = "default,tv_simply,web_safari,web_creator,web_embedded,android,ios,mweb"
	}
	return []string{"--extractor-args", "youtube:player_client=" + clients}
}

// jsRuntimeArgs returns --js-runtimes flag if a runtime was detected or configured.
func jsRuntimeArgs() []string {
	configMu.RLock()
	rt := config.JsRuntimes
	configMu.RUnlock()
	if rt != "" {
		return []string{"--js-runtimes", rt}
	}
	return nil
}

// detectJsRuntimes auto-detects available JS runtimes for yt-dlp if not configured.
// Prefers the managed node binary in <exe>/bin/ (using node:PATH syntax).
func detectJsRuntimes() {
	configMu.RLock()
	already := config.JsRuntimes
	configMu.RUnlock()
	if already != "" {
		// Re-detect if the value contains duplicate node entries (stale config
		// written by a previous binary version that added both "node" and
		// "node:/path" when both the "node" and "nodejs" binaries were present).
		nodeCount := 0
		for _, entry := range strings.Split(already, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "node" || strings.HasPrefix(entry, "node:") {
				nodeCount++
			}
		}
		if nodeCount <= 1 {
			logger.Info("JS runtimes configured", "runtimes", already)
			return
		}
		logger.Info("Stale js_runtimes detected (duplicate node entries), re-detecting", "old", already)
		configMu.Lock()
		config.JsRuntimes = ""
		publishConfig()
		configMu.Unlock()
	}

	var found []string

	// Check system runtimes.
	// yt-dlp only accepts: deno, node, bun, quickjs.
	// Use else-if for node variants to ensure only one "node" entry is emitted.
	for _, rt := range []string{"deno", "bun"} {
		if _, err := exec.LookPath(rt); err == nil {
			found = append(found, rt)
		}
	}
	// Prefer the managed node binary in <exe>/bin/node (self-contained install).
	managedNode := filepath.Join(appHome(), "bin", "node")
	if _, err := os.Stat(managedNode); err == nil {
		found = append(found, "node:"+managedNode)
	} else if _, err := exec.LookPath("node"); err == nil {
		found = append(found, "node")
	} else if path, err := exec.LookPath("nodejs"); err == nil {
		found = append(found, "node:"+path)
	}

	if len(found) > 0 {
		configMu.Lock()
		config.JsRuntimes = strings.Join(found, ",")
		publishConfig()
		configMu.Unlock()
		logger.Info("Auto-detected JS runtimes", "runtimes", strings.Join(found, ","))
	} else {
		logger.Warn("No JS runtime found (deno/node/bun). YouTube extraction may be limited. See https://github.com/yt-dlp/yt-dlp/wiki/EJS")
	}
}

// isSimpleFormatID returns true for plain numeric format IDs (e.g. "401", "140")
// which may be video-only or audio-only and need bestaudio merging.
func isSimpleFormatID(id string) bool {
	if id == "" || id == "ba" {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// clearWriteDeadline lifts the server's WriteTimeout for a single response.
// Long-lived streams (SSE) outlive it and would otherwise be cut mid-stream.
func clearWriteDeadline(w http.ResponseWriter) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		logger.Debug("Cannot clear write deadline", "error", err)
	}
}

// ensureYtdlp ensures a self-managed yt-dlp binary exists in <exe>/bin/.
// We never rely on a system-installed version — always use our own.
func ensureYtdlp() {
	binDir := filepath.Join(appHome(), "bin")
	localName := "yt-dlp"
	if runtime.GOOS == "windows" {
		localName = "yt-dlp.exe"
	}
	localPath := filepath.Join(binDir, localName)

	// If we already have a local binary, use it
	if _, err := os.Stat(localPath); err == nil {
		config.YtdlpPath = localPath
		publishConfig()
		logger.Info("Using managed yt-dlp", "path", localPath)
		return
	}

	// Download fresh from GitHub releases
	logger.Info("yt-dlp not found, downloading from GitHub...")

	var asset string
	switch runtime.GOOS {
	case "darwin":
		asset = "yt-dlp_macos"
	case "windows":
		asset = "yt-dlp.exe"
	case "linux":
		switch runtime.GOARCH {
		case "arm64":
			asset = "yt-dlp_linux_aarch64"
		case "arm":
			asset = "yt-dlp_linux_armv7l"
		default:
			asset = "yt-dlp_linux"
		}
	default:
		logger.Error("Unsupported OS for yt-dlp download", "os", runtime.GOOS)
		return
	}

	downloadURL := "https://github.com/yt-dlp/yt-dlp/releases/latest/download/" + asset
	sumsURL := "https://github.com/yt-dlp/yt-dlp/releases/latest/download/SHA2-256SUMS"

	if err := os.MkdirAll(binDir, 0755); err != nil {
		logger.Error("Cannot create bin dir", "error", err)
		return
	}

	if err := downloadToFile(downloadURL, localPath); err != nil {
		logger.Error("Failed to download yt-dlp", "error", err)
		os.Remove(localPath)
		return
	}

	if err := verifyChecksum(localPath, sumsURL, asset); err != nil {
		logger.Error("yt-dlp checksum verification failed, refusing to use it", "error", err)
		os.Remove(localPath)
		return
	}

	if err := os.Chmod(localPath, 0755); err != nil {
		logger.Error("Failed to chmod yt-dlp", "error", err)
		return
	}

	config.YtdlpPath = localPath
	publishConfig()
	logger.Info("Downloaded yt-dlp", "path", localPath)
}

// verifyChecksum fetches a `sha256␠␠filename` manifest and compares the entry
// for assetName against the file on disk. A release published with a bad or
// missing checksum is treated as a failure — we exec this binary.
func verifyChecksum(path, sumsURL, assetName string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(sumsURL)
	if err != nil {
		return fmt.Errorf("cannot fetch checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum list returned status %d", resp.StatusCode)
	}
	sums, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("cannot read checksums: %w", err)
	}

	var want string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName {
			want = strings.ToLower(fields[0])
			break
		}
	}
	if want == "" {
		return fmt.Errorf("no checksum published for %s", assetName)
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", assetName, got, want)
	}
	logger.Info("Checksum verified", "asset", assetName)
	return nil
}

func downloadToFile(url, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	tmpFile := dest + ".tmp"
	f, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("cannot create file: %w", err)
	}

	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("download write failed: %w", err)
	}

	if err := os.Rename(tmpFile, dest); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("rename failed: %w", err)
	}

	return nil
}

// ensureNodejs checks for an available JS runtime and installs one via the
// system package manager if none is found. No binary is downloaded.
func ensureNodejs() {
	// Prefer managed node binary in <exe>/bin/node (self-contained install).
	managedNode := filepath.Join(appHome(), "bin", "node")
	if _, err := os.Stat(managedNode); err == nil {
		logger.Info("Using managed Node.js", "path", managedNode)
		return
	}
	// Fall back to any system JS runtime.
	for _, rt := range []string{"node", "nodejs", "deno", "bun"} {
		if _, err := exec.LookPath(rt); err == nil {
			logger.Info("Using system JS runtime", "runtime", rt)
			return
		}
	}

	if tryInstallNodeViaPackageManager() {
		return
	}

	logger.Warn("No JS runtime found and automatic install failed. Install Node.js manually: https://nodejs.org")
}

// tryInstallNodeViaPackageManager attempts to install Node.js using the first
// available system package manager. Returns true if a JS runtime is reachable
// after the attempt.
func tryInstallNodeViaPackageManager() bool {
	type pmEntry struct {
		binary   string
		args     []string
		pkgName  string
		userMode bool // true = no root required (brew, winget, scoop, choco)
	}
	managers := []pmEntry{
		// Windows — user-mode
		{"winget", []string{"install", "--silent", "--accept-package-agreements", "--accept-source-agreements", "--id"}, "OpenJS.NodeJS.LTS", true},
		{"choco", []string{"install", "-y"}, "nodejs-lts", true},
		{"scoop", []string{"install"}, "nodejs-lts", true},
		// macOS — user-mode
		{"brew", []string{"install"}, "node", true},
		// Linux — system (require root)
		{"apt-get", []string{"install", "-y"}, "nodejs", false},
		{"apt", []string{"install", "-y"}, "nodejs", false},
		{"dnf", []string{"install", "-y"}, "nodejs", false},
		{"yum", []string{"install", "-y"}, "nodejs", false},
		{"apk", []string{"add", "--no-cache"}, "nodejs", false},
		{"pacman", []string{"-S", "--noconfirm"}, "nodejs", false},
		{"zypper", []string{"install", "-y"}, "nodejs", false},
	}

	// System package managers require root. User-mode managers (brew, winget,
	// scoop, choco) work without root. os.Getuid() returns -1 on Windows.
	isRoot := os.Getuid() == 0

	for _, pm := range managers {
		if _, err := exec.LookPath(pm.binary); err != nil {
			continue
		}
		if !pm.userMode && !isRoot {
			// Skip system package managers when not running as root.
			continue
		}
		logger.Info("Attempting to install Node.js", "package_manager", pm.binary)
		args := append(append([]string{}, pm.args...), pm.pkgName)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, pm.binary, args...)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			logger.Warn("Package manager install failed", "pm", pm.binary, "error", err, "output", strings.TrimSpace(string(out)))
			continue
		}
		for _, rt := range []string{"node", "nodejs"} {
			if _, err := exec.LookPath(rt); err == nil {
				logger.Info("Installed Node.js", "package_manager", pm.binary)
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// yt-dlp auto-update
// ---------------------------------------------------------------------------

var (
	ytdlpUpdateMu sync.Mutex
	ytdlpVersion  string
)

func getYtdlpVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := lowPriorityCmd(ctx, cfg().YtdlpPath, "--version")
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func ytdlpAutoUpdateLoop() {
	ytdlpVersion = getYtdlpVersion()
	logger.Info("yt-dlp version", "version", ytdlpVersion)

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if blocked, _ := killSwitchBlocked(); blocked {
				continue
			}
			updateYtdlp(nil)
		case <-shutdownCh:
			return
		}
	}
}

func updateYtdlp(logFn func(string)) error {
	ytdlpUpdateMu.Lock()
	defer ytdlpUpdateMu.Unlock()

	log := func(msg string) {
		logger.Info(msg)
		if logFn != nil {
			logFn(msg)
		}
	}

	log("Checking for yt-dlp updates...")

	// Self-update — works because we own the binary (downloaded from GitHub)
	updateCtx, updateCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer updateCancel()
	cmd := lowPriorityCmd(updateCtx, cfg().YtdlpPath, "-U")
	out, err := cmd.CombinedOutput()
	result := strings.TrimSpace(string(out))

	// Stream each line of output
	for _, line := range strings.Split(result, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && logFn != nil {
			logFn(line)
		}
	}

	if err != nil {
		return fmt.Errorf("yt-dlp self-update failed: %s", result)
	}

	newVer := getYtdlpVersion()
	if newVer != ytdlpVersion {
		log(fmt.Sprintf("yt-dlp updated: %s -> %s", ytdlpVersion, newVer))
		ytdlpVersion = newVer
	} else {
		log("yt-dlp already up to date (" + ytdlpVersion + ")")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Admin: history + update endpoints
// ---------------------------------------------------------------------------

func handleAdminHistory(w http.ResponseWriter, r *http.Request) {
	if !isAdminAuthenticated(r) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}
	jobHistoryMu.Lock()
	history := make([]JobRecord, len(jobHistory))
	copy(history, jobHistory)
	jobHistoryMu.Unlock()

	// Reverse (newest first)
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"history": history})
}

func handleAdminUpdateYtdlp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isAdminAuthenticated(r) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}
	if !validateCSRF(r) {
		jsonError(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}
	if !adminWriteAllowed(w, r) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Fallback: non-streaming response
		if err := updateYtdlp(nil); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": ytdlpVersion})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	clearWriteDeadline(w)

	logFn := func(line string) {
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
	}

	if err := updateYtdlp(logFn); err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}

	fmt.Fprintf(w, "event: done\ndata: %s\n\n", ytdlpVersion)
	flusher.Flush()
}

func handleAdminEvents(w http.ResponseWriter, r *http.Request) {
	if !isAdminAuthenticated(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	clearWriteDeadline(w)

	ch := make(chan struct{}, 1)
	sseClientsMu.Lock()
	sseClients[ch] = struct{}{}
	sseClientsMu.Unlock()

	defer func() {
		sseClientsMu.Lock()
		delete(sseClients, ch)
		sseClientsMu.Unlock()
	}()

	ctx := r.Context()

	// Send initial keepalive
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ch:
			fmt.Fprintf(w, "event: files-changed\ndata: {}\n\n")
			flusher.Flush()
		case <-ctx.Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
