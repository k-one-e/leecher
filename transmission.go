package main

// transmission.go — hands magnet links and .torrent files to Transmission
// over its RPC API and tracks which leecher user added which torrent.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	urlpkg "net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxTorrentFileBytes = 8 << 20

var btihRe = regexp.MustCompile(`(?i)^urn:(btih:([0-9a-f]{40}|[a-z2-7]{32})|btmh:[0-9a-f]{68})$`)

func isValidMagnet(raw string) bool {
	u, err := urlpkg.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "magnet") {
		return false
	}
	for _, xt := range u.Query()["xt"] {
		if btihRe.MatchString(xt) {
			return true
		}
	}
	return false
}

func magnetName(raw string) string {
	if u, err := urlpkg.Parse(raw); err == nil {
		if dn := strings.TrimSpace(u.Query().Get("dn")); dn != "" {
			return dn
		}
	}
	return "Magnet link"
}

func transmissionEnabled() bool {
	return torrentEngine() == engineTransmission && cfg().TransmissionURL != ""
}

func transmissionCreds() (string, string) {
	c := cfg()
	user, pass := c.TransmissionUser, c.TransmissionPassword
	if v := os.Getenv("TRANSMISSION_USER"); v != "" {
		user = v
	}
	if v := os.Getenv("TRANSMISSION_PASS"); v != "" {
		pass = v
	}
	return user, pass
}

// validateTransmissionSettings normalises the admin-editable fields.
func validateTransmissionSettings(rpcURL, downloadDir *string) error {
	*rpcURL = strings.TrimSpace(*rpcURL)
	*downloadDir = strings.TrimSpace(*downloadDir)
	if *rpcURL != "" {
		u, err := urlpkg.Parse(*rpcURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("transmission_url must be an http(s) URL, e.g. http://localhost:9091/transmission/rpc")
		}
		if u.Path == "" || u.Path == "/" {
			u.Path = "/transmission/rpc"
			*rpcURL = u.String()
		}
	}
	if *downloadDir != "" {
		if !strings.HasPrefix(*downloadDir, "/") {
			return errors.New("transmission_download_dir must be an absolute path inside the Transmission container")
		}
		*downloadDir = path.Clean(*downloadDir)
	}
	return nil
}

// ---------------------------------------------------------------------------
// RPC client
// ---------------------------------------------------------------------------

type transmissionClient struct {
	mu        sync.Mutex
	sessionID string
	hc        *http.Client
}

// The RPC endpoint is admin-configured and normally cluster-internal, so it
// deliberately does not use the SSRF-guarded client.
var trClient = &transmissionClient{hc: &http.Client{Timeout: 20 * time.Second}}

func (c *transmissionClient) call(ctx context.Context, method string, args, out any) error {
	endpoint := cfg().TransmissionURL
	if endpoint == "" {
		return errors.New("Torrent support is not configured.")
	}
	body, err := json.Marshal(map[string]any{"method": method, "arguments": args})
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		c.mu.Lock()
		if c.sessionID != "" {
			req.Header.Set("X-Transmission-Session-Id", c.sessionID)
		}
		c.mu.Unlock()
		if u, p := transmissionCreds(); u != "" || p != "" {
			req.SetBasicAuth(u, p)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return fmt.Errorf("Transmission is unreachable: %w", err)
		}
		if resp.StatusCode == http.StatusConflict {
			// CSRF handshake: Transmission hands out the session id on 409.
			c.mu.Lock()
			c.sessionID = resp.Header.Get("X-Transmission-Session-Id")
			c.mu.Unlock()
			resp.Body.Close()
			continue
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			return errors.New("Transmission rejected leecher's credentials.")
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("Transmission answered HTTP %d.", resp.StatusCode)
		}
		var env struct {
			Result    string          `json:"result"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&env); err != nil {
			return fmt.Errorf("Unexpected Transmission response: %w", err)
		}
		if env.Result != "success" {
			return fmt.Errorf("Transmission: %s", env.Result)
		}
		if out != nil && len(env.Arguments) > 0 {
			return json.Unmarshal(env.Arguments, out)
		}
		return nil
	}
	return errors.New("Could not negotiate a Transmission session.")
}

type trAdded struct {
	HashString string `json:"hashString"`
	Name       string `json:"name"`
}

// addTorrent submits either a magnet link or raw .torrent bytes.
func addTorrent(ctx context.Context, magnet string, metainfo []byte) (trAdded, bool, error) {
	args := map[string]any{"paused": false}
	if magnet != "" {
		args["filename"] = magnet
	} else {
		args["metainfo"] = base64.StdEncoding.EncodeToString(metainfo)
	}
	if dir := cfg().TransmissionDownloadDir; dir != "" {
		args["download-dir"] = dir
	}
	var out struct {
		Added     *trAdded `json:"torrent-added"`
		Duplicate *trAdded `json:"torrent-duplicate"`
	}
	if err := trClient.call(ctx, "torrent-add", args, &out); err != nil {
		return trAdded{}, false, err
	}
	switch {
	case out.Added != nil:
		return *out.Added, false, nil
	case out.Duplicate != nil:
		return *out.Duplicate, true, nil
	}
	return trAdded{}, false, errors.New("Transmission did not confirm the torrent.")
}

type trTorrent struct {
	HashString     string  `json:"hashString"`
	Name           string  `json:"name"`
	Status         int     `json:"status"`
	PercentDone    float64 `json:"percentDone"`
	RateDownload   int64   `json:"rateDownload"`
	RateUpload     int64   `json:"rateUpload"`
	ETA            int64   `json:"eta"`
	SizeWhenDone   int64   `json:"sizeWhenDone"`
	Error          int     `json:"error"`
	ErrorString    string  `json:"errorString"`
	PeersConnected int     `json:"peersConnected"`
	UploadRatio    float64 `json:"uploadRatio"`
	AddedDate      int64   `json:"addedDate"`
	DownloadDir    string  `json:"downloadDir"`
}

var trFields = []string{"hashString", "name", "status", "percentDone", "rateDownload", "rateUpload", "eta",
	"sizeWhenDone", "error", "errorString", "peersConnected", "uploadRatio", "addedDate", "downloadDir"}

// getTorrents fetches the given hashes, or every torrent when hashes is nil.
func getTorrents(ctx context.Context, hashes []string) ([]trTorrent, error) {
	args := map[string]any{"fields": trFields}
	if hashes != nil {
		if len(hashes) == 0 {
			return nil, nil
		}
		args["ids"] = hashes
	}
	var out struct {
		Torrents []trTorrent `json:"torrents"`
	}
	if err := trClient.call(ctx, "torrent-get", args, &out); err != nil {
		return nil, err
	}
	return out.Torrents, nil
}

// ---------------------------------------------------------------------------
// Ownership
// ---------------------------------------------------------------------------

type torrentOwner struct {
	Owner   string    `json:"owner"`
	Name    string    `json:"name"`
	AddedAt time.Time `json:"added_at"`
}

var (
	torrentOwners   = map[string]torrentOwner{} // lower-case info hash -> owner
	torrentOwnersMu sync.Mutex
)

func torrentOwnersPath() string { return filepath.Join(stateDir(), "torrents.json") }

func loadTorrentOwners() {
	data, err := os.ReadFile(torrentOwnersPath())
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("Could not read torrent owners", "error", err)
		}
		return
	}
	torrentOwnersMu.Lock()
	defer torrentOwnersMu.Unlock()
	if err := json.Unmarshal(data, &torrentOwners); err != nil {
		logger.Warn("Corrupt torrents.json, starting fresh", "error", err)
		torrentOwners = map[string]torrentOwner{}
	}
}

// saveTorrentOwnersLocked must be called with torrentOwnersMu held.
func saveTorrentOwnersLocked() {
	data, err := json.MarshalIndent(torrentOwners, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(stateDir(), 0700); err != nil {
		logger.Warn("Could not create state dir", "error", err)
		return
	}
	tmp := torrentOwnersPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err == nil {
		err = os.Rename(tmp, torrentOwnersPath())
		if err != nil {
			logger.Warn("Could not save torrent owners", "error", err)
		}
	}
}

func ownedHashes(username string) []string {
	k := userKey(username)
	torrentOwnersMu.Lock()
	defer torrentOwnersMu.Unlock()
	out := []string{}
	for h, o := range torrentOwners {
		if userKey(o.Owner) == k {
			out = append(out, h)
		}
	}
	return out
}

func canManageTorrent(a *authInfo, hash string) bool {
	if a == nil {
		return false
	}
	if a.User.Role == roleAdmin {
		return true
	}
	torrentOwnersMu.Lock()
	o, ok := torrentOwners[strings.ToLower(hash)]
	torrentOwnersMu.Unlock()
	return ok && userKey(o.Owner) == userKey(a.User.Username)
}

// forgetMissingTorrents drops ownership records for torrents that were
// removed from Transmission directly.
func forgetMissingTorrents(asked []string, present []trTorrent) {
	seen := map[string]bool{}
	for _, t := range present {
		seen[strings.ToLower(t.HashString)] = true
	}
	torrentOwnersMu.Lock()
	defer torrentOwnersMu.Unlock()
	changed := false
	for _, h := range asked {
		if !seen[h] {
			delete(torrentOwners, h)
			changed = true
		}
	}
	if changed {
		saveTorrentOwnersLocked()
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func handleTorrentFormats(w http.ResponseWriter, r *http.Request, raw string) {
	name := magnetName(raw)
	if !strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		if u, err := urlpkg.Parse(raw); err == nil {
			name = sanitizeDirectName(path.Base(u.Path))
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	where := "Torrent · downloads on the server"
	if torrentEngine() == engineTransmission {
		where = "Torrent · sent to Transmission"
	}
	writeSSEJSON(w, "media", map[string]any{"title": name, "uploader": where})
	writeSSEJSON(w, "formats", map[string]any{"formats": []Format{{FormatID: "torrent", Label: "Download torrent"}}})
}

// fetchTorrentFile downloads a .torrent through the SSRF-guarded client.
func fetchTorrentFile(ctx context.Context, raw string) ([]byte, error) {
	req, err := newDirectRequest(ctx, http.MethodGet, raw)
	if err != nil {
		return nil, errors.New("invalid URL")
	}
	resp, err := directClient.Do(req)
	if err != nil {
		return nil, errors.New(friendlyNetError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("The server answered HTTP %d.", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentFileBytes+1))
	if err != nil {
		return nil, errors.New("Could not read the .torrent file.")
	}
	if len(data) > maxTorrentFileBytes {
		return nil, errors.New("The .torrent file is too large.")
	}
	if err := checkTorrentBytes(data); err != nil {
		return nil, err
	}
	return data, nil
}

// checkTorrentBytes is a cheap sanity check (bencoded dict with an info key);
// Transmission does the real parsing.
func checkTorrentBytes(data []byte) error {
	if len(data) < 2 || data[0] != 'd' || !bytes.Contains(data, []byte("4:info")) {
		return errors.New("That is not a valid .torrent file.")
	}
	return nil
}

// registerTorrent records ownership and builds the API response.
func registerTorrent(w http.ResponseWriter, a *authInfo, added trAdded, duplicate bool, source string) bool {
	hash := strings.ToLower(added.HashString)
	owner := ""
	if a != nil {
		owner = a.User.Username
	}
	torrentOwnersMu.Lock()
	existing, known := torrentOwners[hash]
	if duplicate && known && userKey(existing.Owner) != userKey(owner) && (a == nil || a.User.Role != roleAdmin) {
		torrentOwnersMu.Unlock()
		jsonError(w, "This torrent is already being downloaded by another user.", http.StatusConflict)
		return false
	}
	if !known {
		torrentOwners[hash] = torrentOwner{Owner: owner, Name: added.Name, AddedAt: time.Now()}
		saveTorrentOwnersLocked()
	}
	torrentOwnersMu.Unlock()

	logger.Info("Torrent added", "user", owner, "hash", hash, "name", added.Name, "duplicate", duplicate)
	appendJobHistory(JobRecord{
		ID: hash[:min(16, len(hash))], URL: source, Status: "added", DisplayName: added.Name,
		Owner: owner, Kind: kindTorrent, CreatedAt: time.Now(), FinishedAt: time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"torrent": map[string]any{"hash": hash, "name": added.Name, "duplicate": duplicate}})
	return true
}

// addTorrentForRequest is the /api/download path for magnets and .torrent URLs.
func addTorrentForRequest(w http.ResponseWriter, r *http.Request, raw, _ string) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var added trAdded
	var dup bool
	var err error
	if strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		added, dup, err = addTorrent(ctx, raw, nil)
	} else {
		var data []byte
		if data, err = fetchTorrentFile(ctx, raw); err == nil {
			added, dup, err = addTorrent(ctx, "", data)
		}
	}
	if err != nil {
		logger.Warn("Adding torrent failed", "error", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return false
	}
	source := raw
	if len(source) > 300 {
		source = source[:300] + "…"
	}
	return registerTorrent(w, currentAuth(r), added, dup, source)
}

type torrentView struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	Owner    string  `json:"owner,omitempty"`
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	RateDown int64   `json:"rate_down"`
	RateUp   int64   `json:"rate_up"`
	ETA      int64   `json:"eta"`
	Size     int64   `json:"size"`
	Peers    int     `json:"peers"`
	Ratio    float64 `json:"ratio"`
	Error    string  `json:"error,omitempty"`
	Added    int64   `json:"added"`
	Dir      string  `json:"dir,omitempty"`
}

var trStatusNames = map[int]string{0: "stopped", 1: "check-wait", 2: "checking", 3: "download-wait", 4: "downloading", 5: "seed-wait", 6: "seeding"}

func handleTorrents(w http.ResponseWriter, r *http.Request) {
	a := currentAuth(r)
	if a == nil {
		jsonError(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	if !transmissionEnabled() {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "torrents": []torrentView{}})
			return
		}
		if r.Method == http.MethodPost && strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/torrents"), "/") == "upload" && torrentEngine() == engineBuiltin {
			if !limiter.allow(requestActor(r)+":torrent", 30.0/60.0, 30) {
				jsonError(w, "Too many requests. Please slow down.", http.StatusTooManyRequests)
				return
			}
			handleBuiltinTorrentUpload(w, r, a)
			return
		}
		jsonError(w, "Torrent support is not configured.", http.StatusNotFound)
		return
	}
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/torrents"), "/")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if r.Method != http.MethodGet && !limiter.allow(requestActor(r)+":torrent", 30.0/60.0, 30) {
		jsonError(w, "Too many requests. Please slow down.", http.StatusTooManyRequests)
		return
	}
	// Adding or starting makes Transmission connect out; stopping and removing stay allowed.
	if r.Method == http.MethodPost && (sub == "upload" || strings.HasSuffix(sub, "/start")) {
		if err := killSwitchGate(r.Context()); err != nil {
			jsonError(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}

	switch {
	case sub == "" && r.Method == http.MethodGet:
		isAdmin := a.User.Role == roleAdmin
		var hashes []string
		if !isAdmin {
			hashes = ownedHashes(a.User.Username)
		}
		list, err := getTorrents(ctx, hashes)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		if hashes != nil {
			forgetMissingTorrents(hashes, list)
		}
		torrentOwnersMu.Lock()
		views := make([]torrentView, 0, len(list))
		for _, t := range list {
			v := torrentView{
				Hash: strings.ToLower(t.HashString), Name: t.Name, Status: trStatusNames[t.Status],
				Percent: t.PercentDone * 100, RateDown: t.RateDownload, RateUp: t.RateUpload, ETA: t.ETA,
				Size: t.SizeWhenDone, Peers: t.PeersConnected, Ratio: t.UploadRatio, Error: t.ErrorString, Added: t.AddedDate,
			}
			if isAdmin {
				v.Owner = torrentOwners[v.Hash].Owner
				v.Dir = t.DownloadDir
			}
			views = append(views, v)
		}
		torrentOwnersMu.Unlock()
		sort.Slice(views, func(i, j int) bool { return views[i].Added > views[j].Added })
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "torrents": views, "all": isAdmin})

	case sub == "upload" && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxTorrentFileBytes+64<<10)
		if err := r.ParseMultipartForm(maxTorrentFileBytes); err != nil {
			jsonError(w, "Upload too large or invalid", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			jsonError(w, "Missing file field", http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxTorrentFileBytes+1))
		if err != nil || len(data) > maxTorrentFileBytes {
			jsonError(w, "The .torrent file is too large.", http.StatusBadRequest)
			return
		}
		if err := checkTorrentBytes(data); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		added, dup, err := addTorrent(ctx, "", data)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		registerTorrent(w, a, added, dup, "upload: "+sanitizeDirectName(header.Filename))

	case sub != "" && r.Method == http.MethodPost:
		parts := strings.SplitN(sub, "/", 2)
		if len(parts) != 2 {
			jsonError(w, "Not found", http.StatusNotFound)
			return
		}
		hash, action := strings.ToLower(parts[0]), parts[1]
		if !canManageTorrent(a, hash) {
			jsonError(w, "Torrent not found", http.StatusNotFound)
			return
		}
		var err error
		switch action {
		case "start":
			err = trClient.call(ctx, "torrent-start", map[string]any{"ids": []string{hash}}, nil)
		case "stop":
			err = trClient.call(ctx, "torrent-stop", map[string]any{"ids": []string{hash}}, nil)
		case "remove", "remove-data":
			err = trClient.call(ctx, "torrent-remove", map[string]any{"ids": []string{hash}, "delete-local-data": action == "remove-data"}, nil)
			if err == nil {
				torrentOwnersMu.Lock()
				delete(torrentOwners, hash)
				saveTorrentOwnersLocked()
				torrentOwnersMu.Unlock()
				logger.Info("Torrent removed", "user", a.User.Username, "hash", hash, "delete_data", action == "remove-data")
			}
		default:
			jsonError(w, "Unknown action", http.StatusNotFound)
			return
		}
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleAdminTransmissionTest checks connectivity and credentials.
func handleAdminTransmissionTest(w http.ResponseWriter, r *http.Request) {
	if !isAdminAuthenticated(r) {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var out struct {
		Version     string `json:"version"`
		DownloadDir string `json:"download-dir"`
	}
	if err := trClient.call(ctx, "session-get", map[string]any{"fields": []string{"version", "download-dir"}}, &out); err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": out.Version, "download_dir": out.DownloadDir})
}
