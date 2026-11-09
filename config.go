package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// cfg returns a lock-free snapshot of the configuration. Fields that the admin
// panel can change at runtime must be read through this, never off the `config`
// global, which is only safe while holding configMu.
func cfg() *Config {
	if c := configSnapshot.Load(); c != nil {
		return c
	}
	return &Config{}
}

// publishConfig republishes the snapshot. Call it after every mutation of
// `config`, while still holding configMu.
func publishConfig() {
	c := config
	configSnapshot.Store(&c)
}

type Config struct {
	Port                   int    `json:"port"`
	Host                   string `json:"host"`
	DownloadsDir           string `json:"downloads_dir"`
	MaxFileAgeMinutes      int    `json:"max_file_age_minutes"`
	DownloadTimeoutMinutes int    `json:"download_timeout_minutes"`
	MaxConcurrentDownloads int    `json:"max_concurrent_downloads"`
	MaxDownloadsPerIP      int    `json:"max_downloads_per_ip"`
	YtdlpPath              string `json:"ytdlp_path"`
	CookiesFromBrowser     string `json:"cookies_from_browser"`
	CookiesFile            string `json:"cookies_file"`
	AdminUser              string `json:"admin_user"`
	AdminHash              string `json:"admin_hash"`
	SessionHours           int    `json:"session_hours"`
	MaxFileSizeMB          int    `json:"max_file_size_mb"`
	MaxStorageMB           int    `json:"max_storage_mb"`
	MinDiskFreeMB          int    `json:"min_disk_free_mb"`
	RateLimitFormats       int    `json:"rate_limit_formats"`
	RateLimitDownloads     int    `json:"rate_limit_downloads"`
	TimeZone               string `json:"time_zone"`
	DebugMode              bool   `json:"debug_mode"`
	JsRuntimes             string `json:"js_runtimes"`
	ProbeConcurrency       int    `json:"probe_concurrency"`
	ProbeQueueMax          int    `json:"probe_queue_max"`
	ProbeTimeoutSecs       int    `json:"probe_timeout_secs"`
	DownloadQueueMax       int    `json:"download_queue_max"`
	// TrustedProxies lists CIDRs/IPs whose X-Forwarded-For header may be
	// believed. Empty (the default) means the header is ignored entirely.
	TrustedProxies []string `json:"trusted_proxies"`
	// AllowedOrigins lists extra WebSocket origins; empty means same-origin only.
	AllowedOrigins []string `json:"allowed_origins"`

	// SSOUserHeader enables single sign-on behind a forward-auth proxy such
	// as Traefik + Authelia: the header (e.g. "Remote-User") names the
	// signed-in user. Only honoured from trusted_proxies. Empty disables SSO.
	SSOUserHeader string `json:"sso_user_header"`
	// SSOGroupsHeader carries the user's comma-separated groups.
	SSOGroupsHeader string `json:"sso_groups_header"`
	// SSOAdminGroup makes members leecher admins (and everyone else a
	// regular user). Empty leaves roles to the admin panel.
	SSOAdminGroup string `json:"sso_admin_group"`
	// SSOLogoutURL is where "Sign out" goes after ending the leecher session,
	// so the SSO session ends too. "{domain}" is the request host minus its
	// first label, e.g. https://auth.{domain}/logout.
	SSOLogoutURL string `json:"sso_logout_url"`

	// LibraryDir is where "Save to server" downloads end up. Empty disables
	// the option. Must live outside DownloadsDir, which is auto-cleaned.
	LibraryDir string `json:"library_dir"`
	// DirectDownloads enables plain http(s) file downloads for URLs that
	// yt-dlp does not handle.
	DirectDownloads      bool `json:"direct_downloads"`
	DirectTimeoutMinutes int  `json:"direct_timeout_minutes"`
	// AllowPrivateURLs lifts the SSRF guard on direct downloads. Leave it off
	// unless every user is fully trusted with access to the internal network.
	AllowPrivateURLs bool `json:"allow_private_urls"`

	// TransmissionURL is the RPC endpoint (…/transmission/rpc). Empty disables
	// torrent support. Credentials can also come from TRANSMISSION_USER /
	// TRANSMISSION_PASS, which take precedence.
	// TorrentEngine: "builtin" (default), "transmission" or "off".
	TorrentEngine     string  `json:"torrent_engine"`
	SeedRatio         float64 `json:"seed_ratio"`          // stop seeding at this upload ratio (0 = no ratio limit)
	SeedMinutes       int     `json:"seed_minutes"`        // stop seeding after this long (0 = don't seed)
	MaxActiveTorrents int     `json:"max_active_torrents"` // concurrent torrent downloads (0 = unlimited)
	TorrentPort       int     `json:"torrent_port"`        // peer listen port (0 = random)

	TransmissionURL         string `json:"transmission_url"`
	TransmissionDownloadDir string `json:"transmission_download_dir"`
	TransmissionUser        string `json:"transmission_user,omitempty"`
	TransmissionPassword    string `json:"transmission_password,omitempty"`

	// KillSwitch stops every download while the outbound IP is not one of
	// KillSwitchIPs (addresses or CIDR ranges); see killswitch.go.
	KillSwitch             bool     `json:"kill_switch"`
	KillSwitchIPs          []string `json:"kill_switch_ips"`
	KillSwitchIntervalSecs int      `json:"kill_switch_interval_secs"`
}

// stateDir holds users.json and torrents.json. It is a dedicated directory
// because atomic writes need a writable parent, and the systemd unit only
// opens up specific paths under the otherwise read-only install dir.
func stateDir() string { return filepath.Join(appHome(), "state") }

// appHome is the directory for leecher's own files (state, managed binaries,
// caches). LEECHER_HOME overrides the default of the binary's dir, which is
// read-only inside a container.
func appHome() string {
	if h := os.Getenv("LEECHER_HOME"); h != "" {
		return h
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func loadConfig() {
	// Defaults — loaded from the embedded config.default.json (single source of truth)
	defaultData, err := embedded.ReadFile("config.default.json")
	if err != nil {
		logger.Error("Missing embedded config.default.json", "error", err)
		os.Exit(1)
	}
	if err := json.Unmarshal(defaultData, &config); err != nil {
		logger.Error("Invalid config.default.json", "error", err)
		os.Exit(1)
	}

	// Try to find config.json next to the binary, or only in LEECHER_HOME
	// when that is set (container deployments keep it on the data volume).
	exePath, _ := os.Executable()
	configPaths := []string{
		"config.json",
		filepath.Join(filepath.Dir(exePath), "config.json"),
	}
	if home := os.Getenv("LEECHER_HOME"); home != "" {
		configPaths = []string{filepath.Join(home, "config.json")}
	}

	for _, path := range configPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(data, &config); err != nil {
			logger.Warn("Invalid config.json", "error", err)
		} else {
			logger.Info("Loaded config", "path", path)
		}
		configPath = path
		break
	}
	if configPath == "" {
		configPath = configPaths[0]
	}

	// Apply minimums for fields that may be missing from older config.json files.
	// json.Unmarshal sets missing fields to zero, overwriting the defaults above.
	if config.ProbeConcurrency < 1 {
		config.ProbeConcurrency = 2
	}
	if config.ProbeQueueMax < 1 {
		config.ProbeQueueMax = 50
	}
	if config.MaxConcurrentDownloads < 1 {
		config.MaxConcurrentDownloads = 4
	}
	if config.MaxDownloadsPerIP < 1 {
		config.MaxDownloadsPerIP = 1
	}
	if config.ProbeTimeoutSecs < 5 {
		config.ProbeTimeoutSecs = 600
	}
	if config.DownloadQueueMax < 1 {
		config.DownloadQueueMax = 20
	}
	if config.MaxActiveTorrents < 0 {
		config.MaxActiveTorrents = 0
	}
	if config.DirectTimeoutMinutes < 1 {
		config.DirectTimeoutMinutes = 240
	}
	if config.KillSwitchIntervalSecs < 1 {
		config.KillSwitchIntervalSecs = 3
	}
	if ips, err := normalizeAllowedIPs(config.KillSwitchIPs); err != nil {
		// Fail closed: an unreadable list allows nothing, so an armed switch stays tripped.
		logger.Warn("Invalid kill_switch_ips in config", "error", err)
	} else {
		config.KillSwitchIPs = ips
	}

	trustedProxies = parseTrustedProxies(config.TrustedProxies)
	if len(trustedProxies) > 0 {
		logger.Info("Trusting X-Forwarded-For", "proxies", strings.Join(config.TrustedProxies, ","))
	}

	publishConfig()
}
