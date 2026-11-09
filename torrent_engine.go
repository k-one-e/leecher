package main

// torrent_engine.go — built-in BitTorrent engine (anacrolix/torrent).
//
// Torrents run as ordinary leecher jobs: they appear in the activity list,
// finish into the same work area, and use Save / Keep / Delete like any other
// download. After completing they seed until the configured ratio or time is
// reached, or until the user keeps or deletes the files.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/iplist"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

const (
	engineBuiltin      = "builtin"
	engineTransmission = "transmission"
	engineOff          = "off"

	torrentMetadataTimeout = 30 * time.Minute
)

func torrentEngine() string {
	switch e := cfg().TorrentEngine; e {
	case engineTransmission, engineOff:
		return e
	}
	return engineBuiltin
}

// torrentsEnabled reports whether magnets / .torrent files are accepted.
func torrentsEnabled() bool {
	switch torrentEngine() {
	case engineBuiltin:
		return true
	case engineTransmission:
		return cfg().TransmissionURL != ""
	}
	return false
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// privateRanger stops the engine from dialling peers on private, loopback or
// link-local addresses — the BitTorrent equivalent of the direct-download
// SSRF guard (a hostile tracker could otherwise point us at the LAN).
type privateRanger struct{}

func (privateRanger) Lookup(ip net.IP) (iplist.Range, bool) {
	if cfg().AllowPrivateURLs {
		return iplist.Range{}, false
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok || !isPublicAddr(a) {
		return iplist.Range{Description: "private or local address"}, true
	}
	return iplist.Range{}, false
}

func (privateRanger) NumRanges() int { return 1 }

// btNoDHT disables DHT bootstrapping (tests run offline).
var btNoDHT bool

var (
	btMu     sync.Mutex
	btClient *torrent.Client
	btJobs   = map[string]*btJob{} // job ID -> running torrent
)

type btJob struct {
	job    *Job
	t      *torrent.Torrent
	stopCh chan struct{} // closed to end seeding early (Keep / Delete)
	once   sync.Once
	paused atomic.Bool // set by the kill switch before it cancels the job
}

func (b *btJob) stopSeeding() { b.once.Do(func() { close(b.stopCh) }) }

func getBTClient() (*torrent.Client, error) {
	btMu.Lock()
	defer btMu.Unlock()
	if btClient != nil {
		return btClient, nil
	}
	dataDir := filepath.Join(stateDir(), "torrent")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	newCfg := func(port int) *torrent.ClientConfig {
		c := torrent.NewDefaultClientConfig()
		c.DataDir = dataDir
		c.ListenPort = port
		c.Seed = true
		c.IPBlocklist = privateRanger{}
		c.NoDHT = btNoDHT
		// The library is chatty; only surface real problems.
		c.Slogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
		return c
	}
	cl, err := torrent.NewClient(newCfg(cfg().TorrentPort))
	if err != nil && cfg().TorrentPort != 0 {
		logger.Warn("Torrent port unavailable, using a random one", "port", cfg().TorrentPort, "error", err)
		cl, err = torrent.NewClient(newCfg(0))
	}
	if err != nil {
		return nil, fmt.Errorf("cannot start torrent engine: %w", err)
	}
	btClient = cl
	logger.Info("Torrent engine started", "port", cfg().TorrentPort)
	return cl, nil
}

// jobStorage stores a job's files as <workDir>/<jobID>_<torrent name>[/...],
// so the finished download already sits where Save / Keep expect it.
func jobStorage(workDir, jobID string) storage.ClientImplCloser {
	return storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir: workDir,
		FilePathMaker: func(o storage.FilePathMakerOpts) string {
			parts := append([]string{jobID + "_" + sanitizeDirectName(o.Info.BestName())}, o.File.BestPath()...)
			for i := 1; i < len(parts); i++ {
				parts[i] = sanitizeDirectName(parts[i])
			}
			return filepath.Join(parts...)
		},
	})
}

// specFor builds a torrent spec from a magnet link or .torrent bytes.
func specFor(magnet string, meta []byte) (*torrent.TorrentSpec, error) {
	if magnet != "" {
		spec, err := torrent.TorrentSpecFromMagnetUri(magnet)
		if err != nil {
			return nil, errors.New("That magnet link could not be read.")
		}
		return spec, nil
	}
	mi, err := metainfo.Load(bytes.NewReader(meta))
	if err != nil {
		return nil, errors.New("That is not a valid .torrent file.")
	}
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, errors.New("That .torrent file is invalid.")
	}
	return spec, nil
}

// ---------------------------------------------------------------------------
// Resume after restart
// ---------------------------------------------------------------------------

type btResume struct {
	ID          string    `json:"id"`
	Owner       string    `json:"owner"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	Magnet      string    `json:"magnet,omitempty"`
	Meta        string    `json:"meta,omitempty"` // base64 .torrent
	Destination string    `json:"destination"`
	WorkDir     string    `json:"work_dir"`
	CreatedAt   time.Time `json:"created_at"`
	Paused      bool      `json:"paused,omitempty"` // paused by its user; stays paused across restarts
}

var btResumeMu sync.Mutex

func btResumePath() string { return filepath.Join(stateDir(), "torrent-jobs.json") }

func loadBTResume() map[string]btResume {
	out := map[string]btResume{}
	if data, err := os.ReadFile(btResumePath()); err == nil {
		json.Unmarshal(data, &out)
	}
	return out
}

func updateBTResume(fn func(map[string]btResume)) {
	btResumeMu.Lock()
	defer btResumeMu.Unlock()
	m := loadBTResume()
	fn(m)
	data, _ := json.MarshalIndent(m, "", "  ")
	os.MkdirAll(stateDir(), 0700)
	tmp := btResumePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err == nil {
		os.Rename(tmp, btResumePath())
	}
}

// resumeTorrents restarts downloads that were running at shutdown or were
// paused by the kill switch. Pieces already on disk are verified and kept.
// While the kill switch blocks downloads they are listed as paused instead;
// torrents their user paused stay paused.
func resumeTorrents() {
	if torrentEngine() != engineBuiltin {
		return
	}
	blocked, reason := killSwitchBlocked()
	for _, r := range loadBTResume() {
		resumeTorrentEntry(r, blocked, reason)
	}
}

func resumeTorrentEntry(r btResume, blocked bool, reason string) error {
	if torrentJobBusy(r.ID) {
		return nil
	}
	jobsMu.RLock()
	job := jobs[r.ID]
	jobsMu.RUnlock()
	if job != nil {
		job.mu.Lock()
		status := job.Status
		job.mu.Unlock()
		if status == "cancelled" {
			updateBTResume(func(m map[string]btResume) { delete(m, r.ID) })
			removeJobFiles(r.WorkDir, r.ID)
			return nil
		}
		if status != "paused" {
			return nil
		}
	} else {
		job = newTorrentJob(r.ID, r.Owner, r.Title, r.URL, r.Destination, r.CreatedAt)
		job.workDir = r.WorkDir
	}
	job.mu.Lock()
	job.UserPaused = r.Paused
	job.mu.Unlock()
	if r.Paused {
		pauseTorrentJob(job, "Paused")
		return nil
	}
	if blocked {
		pauseTorrentJob(job, "Paused by the kill switch: "+reason+".")
		return killSwitchErr{reason}
	}
	job.mu.Lock()
	job.Status, job.Message = "queued", "Resuming..."
	job.notifySubscribers()
	job.mu.Unlock()
	meta, _ := base64.StdEncoding.DecodeString(r.Meta)
	if _, err := launchTorrent(job, r.Magnet, meta); err != nil {
		if errors.Is(err, errKillSwitch) {
			pauseTorrentJob(job, "Paused: "+err.Error())
			return err
		}
		logger.Warn("Could not resume torrent", "job", r.ID, "error", err)
		updateBTResume(func(m map[string]btResume) { delete(m, r.ID) })
		jobsMu.RLock()
		_, listed := jobs[r.ID]
		jobsMu.RUnlock()
		if listed {
			setJobError(job, err.Error())
		}
		return err
	}
	logger.Info("Resumed torrent", "job", r.ID, "title", r.Title)
	return nil
}

var errNotPausable = errors.New("Only a downloading torrent can be paused.")

// pauseTorrent stops a built-in torrent at its user's request. Like a kill
// switch pause, its files and resume entry stay; unlike it, it is not resumed
// automatically — not by the kill switch, nor after a restart.
func pauseTorrent(jobID string) error {
	jobsMu.RLock()
	job := jobs[jobID]
	jobsMu.RUnlock()
	if job == nil || job.Kind != kindTorrent {
		return errNotPausable
	}
	if _, ok := loadBTResume()[jobID]; !ok {
		return errNotPausable // finished (seeding) or never started
	}
	btMu.Lock()
	b := btJobs[jobID]
	btMu.Unlock()
	msg := "Paused"
	if b != nil && b.t.Info() != nil {
		msg = fmt.Sprintf("Paused · %s / %s", humanBytes(b.t.BytesCompleted()), humanBytes(b.t.Length()))
	}

	job.mu.Lock()
	defer job.mu.Unlock()
	switch job.Status {
	case "queued", "downloading":
		if b == nil {
			// Between resume and registration; cancelling now would delete its files.
			return errors.New("The torrent is just starting. Try again in a moment.")
		}
		b.paused.Store(true)
		job.Status, job.Message, job.Peers, job.UserPaused = "paused", msg, 0, true
		job.notifySubscribers()
		job.cancel()
	case "paused": // already held by the kill switch: keep it paused afterwards
		job.UserPaused = true
		job.Message = "Paused"
		job.notifySubscribers()
	default:
		return errNotPausable
	}
	updateBTResume(func(m map[string]btResume) {
		if r, ok := m[jobID]; ok {
			r.Paused = true
			m[jobID] = r
		}
	})
	return nil
}

// resumePausedTorrent undoes pauseTorrent.
func resumePausedTorrent(jobID string) error {
	jobsMu.RLock()
	job := jobs[jobID]
	jobsMu.RUnlock()
	if job == nil || job.Kind != kindTorrent {
		return errors.New("Only a paused torrent can be resumed.")
	}
	job.mu.Lock()
	paused := job.Status == "paused"
	job.mu.Unlock()
	r, ok := loadBTResume()[jobID]
	if !paused || !ok {
		return errors.New("Only a paused torrent can be resumed.")
	}
	if n := cfg().MaxActiveTorrents; n > 0 && activeTorrentCount() >= n {
		return fmt.Errorf("%d torrents are already downloading. Try again when one finishes.", n)
	}
	r.Paused = false
	updateBTResume(func(m map[string]btResume) {
		if e, ok := m[jobID]; ok {
			e.Paused = false
			m[jobID] = e
		}
	})
	job.mu.Lock()
	job.UserPaused = false
	job.mu.Unlock()
	blocked, reason := killSwitchBlocked()
	// While the kill switch holds downloads, the torrent now waits for it and
	// resumes with everything else; the returned error says so.
	return resumeTorrentEntry(r, blocked, reason)
}

// pauseTorrentJob lists a torrent that is not running as paused.
func pauseTorrentJob(job *Job, msg string) {
	job.mu.Lock()
	job.Status, job.Message, job.Peers = "paused", msg, 0
	job.notifySubscribers()
	job.mu.Unlock()
	jobsMu.Lock()
	jobs[job.ID] = job
	jobsMu.Unlock()
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

func newTorrentJob(id, owner, title, url, destination string, created time.Time) *Job {
	_, cancel := context.WithCancel(context.Background())
	return &Job{
		ID: id, Status: "queued", Message: "Starting torrent...", Owner: owner, Title: cleanJobTitle(title),
		URL: url, Kind: kindTorrent, Destination: destination, Token: generateSessionToken(),
		CreatedAt: created, cancel: cancel, subscribers: make(map[chan struct{}]struct{}),
	}
}

func activeTorrentCount() int {
	btMu.Lock()
	defer btMu.Unlock()
	n := 0
	for _, b := range btJobs {
		b.job.mu.Lock()
		if b.job.Status == "queued" || b.job.Status == "downloading" {
			n++
		}
		b.job.mu.Unlock()
	}
	return n
}

// launchTorrent registers the job and starts its goroutine.
func launchTorrent(job *Job, magnet string, meta []byte) (*btJob, error) {
	if blocked, reason := killSwitchBlocked(); blocked {
		return nil, killSwitchErr{reason}
	}
	spec, err := specFor(magnet, meta)
	if err != nil {
		return nil, err
	}
	btMu.Lock()
	for _, b := range btJobs {
		if b.t != nil && b.t.InfoHash() == spec.InfoHash {
			btMu.Unlock()
			return nil, fmt.Errorf("This torrent is already running (%s).", jobTitle(b.job))
		}
	}
	btMu.Unlock()

	cl, err := getBTClient()
	if err != nil {
		return nil, err
	}
	if job.workDir == "" {
		job.workDir = tempDir()
	}
	if err := os.MkdirAll(job.workDir, 0775); err != nil {
		return nil, fmt.Errorf("Cannot prepare the download folder: %v", err)
	}
	spec.Storage = jobStorage(job.workDir, job.ID)
	t, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("Could not add the torrent: %v", err)
	}
	if job.Title == "" {
		job.Title = cleanJobTitle(spec.DisplayName)
	}

	ctx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	b := &btJob{job: job, t: t, stopCh: make(chan struct{})}
	btMu.Lock()
	btJobs[job.ID] = b
	btMu.Unlock()
	jobsMu.Lock()
	jobs[job.ID] = job
	jobsMu.Unlock()

	updateBTResume(func(m map[string]btResume) {
		m[job.ID] = btResume{ID: job.ID, Owner: job.Owner, Title: job.Title, URL: job.URL, Magnet: magnet,
			Meta: base64.StdEncoding.EncodeToString(meta), Destination: job.Destination, WorkDir: job.workDir, CreatedAt: job.CreatedAt}
	})
	go runTorrent(ctx, b)
	return b, nil
}

// removeJobFiles deletes everything a torrent job wrote (complete or .part).
func removeJobFiles(workDir, jobID string) {
	matches, _ := filepath.Glob(filepath.Join(workDir, jobID+"_*"))
	for _, m := range matches {
		os.RemoveAll(m)
	}
}

func finishBT(b *btJob) {
	b.t.Drop()
	btMu.Lock()
	delete(btJobs, b.job.ID)
	btMu.Unlock()
	updateBTResume(func(m map[string]btResume) { delete(m, b.job.ID) })
}

// detachBT stops a torrent paused by the kill switch but keeps its resume
// entry and files, so resumeTorrents can pick it up again.
func detachBT(b *btJob) {
	b.t.Drop()
	btMu.Lock()
	delete(btJobs, b.job.ID)
	btMu.Unlock()
}

func runTorrent(ctx context.Context, b *btJob) {
	job, t := b.job, b.t
	stopped := func() {
		if b.paused.Load() {
			detachBT(b)
			return
		}
		finishBT(b)
		removeJobFiles(job.workDir, job.ID)
	}
	fail := func(msg string) {
		finishBT(b)
		removeJobFiles(job.workDir, job.ID)
		setJobError(job, msg)
	}
	setMsg := func(status, msg string) {
		job.mu.Lock()
		if job.Status != "cancelled" && job.Status != "paused" {
			job.Status, job.Message = status, msg
		}
		job.notifySubscribers()
		job.mu.Unlock()
	}

	setMsg("queued", "Looking for peers...")
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		stopped()
		return
	case <-time.After(torrentMetadataTimeout):
		fail("No peers sent this torrent's details within 30 minutes. It may be dead.")
		return
	}

	name := sanitizeDirectName(t.Info().BestName())
	size := t.Length()
	job.mu.Lock()
	if job.Title == "" || strings.HasPrefix(job.Title, "magnet:") {
		job.Title = cleanJobTitle(t.Info().BestName())
	}
	job.mu.Unlock()
	if limit := int64(cfg().MaxFileSizeMB) << 20; limit > 0 && size > limit {
		fail(fmt.Sprintf("Torrent is %s, larger than the %d MB limit.", humanBytes(size), cfg().MaxFileSizeMB))
		return
	}
	if minFree := int64(cfg().MinDiskFreeMB); minFree >= 0 {
		if free, err := diskFreeMBAt(job.workDir); err == nil && free-(size-t.BytesCompleted())>>20 < minFree {
			fail(fmt.Sprintf("Not enough disk space: the torrent needs %s and only %d MB are free.", humanBytes(size), free))
			return
		}
	}

	setMsg("downloading", "Downloading...")
	t.DownloadAll()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	// Speed comes from piece data received from peers. BytesCompleted also grows
	// while pieces already on disk are re-verified after a resume, which would
	// show the disk's read rate as download speed.
	received := func() int64 { st := t.Stats(); return st.BytesReadUsefulData.Int64() }
	var lastBytes int64 = received()
	lastAt := time.Now()
	var speed float64
	for t.BytesMissing() > 0 {
		select {
		case <-ctx.Done():
			stopped()
			return
		case <-tick.C:
		}
		done := t.BytesCompleted()
		got := received()
		now := time.Now()
		if dt := now.Sub(lastAt).Seconds(); dt > 0 {
			inst := float64(got-lastBytes) / dt
			speed = 0.7*speed + 0.3*inst
		}
		lastBytes, lastAt = got, now
		stats := t.Stats()
		msg := fmt.Sprintf("%s / %s", humanBytes(done), humanBytes(size))
		if speed > 1024 {
			msg += fmt.Sprintf(" · %s/s", humanBytes(int64(speed)))
			if left := float64(size-done) / speed; left > 0 {
				msg += " · " + (time.Duration(left) * time.Second).Round(time.Second).String() + " left"
			}
		}
		msg += fmt.Sprintf(" · %d peer", stats.ActivePeers)
		if stats.ActivePeers != 1 {
			msg += "s"
		}
		job.mu.Lock()
		if size > 0 {
			job.Progress = float64(done) / float64(size) * 99
		}
		if job.Status != "paused" {
			job.Message = msg
			job.Peers = stats.ActivePeers
		}
		job.notifySubscribers()
		job.mu.Unlock()
	}

	// Part files are renamed once complete; give the storage a moment.
	final := job.ID + "_" + name
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(filepath.Join(job.workDir, final)); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	updateBTResume(func(m map[string]btResume) { delete(m, job.ID) })

	job.mu.Lock()
	job.Status, job.Progress, job.Message = "done", 100, "Complete!"
	job.Filename, job.DisplayName, job.FinishedAt = final, name, time.Now()
	job.notifySubscribers()
	dest := job.Destination
	job.mu.Unlock()

	if dest == "library" {
		finishBT(b)
		if _, err := keepJobFile(jobRef{ID: job.ID, Owner: job.Owner, Status: "done", Filename: final, DisplayName: name}); err != nil {
			logger.Warn("Auto-keep of torrent failed", "job", job.ID, "error", err)
		}
	}
	job.mu.Lock()
	appendJobHistory(JobRecord{ID: job.ID, URL: job.URL, Status: "done", Filename: job.Filename, DisplayName: name,
		Owner: job.Owner, Title: job.Title, Kind: kindTorrent, Destination: job.Destination, LibraryPath: job.LibraryPath,
		CreatedAt: job.CreatedAt, FinishedAt: job.FinishedAt})
	job.mu.Unlock()
	notifyFileChange()
	if dest == "library" {
		return
	}
	seed(ctx, b, size)
}

// seed shares the finished torrent until the ratio or time limit is met, or
// until the user keeps, deletes or stops it.
func seed(ctx context.Context, b *btJob, size int64) {
	defer finishBT(b)
	job, t := b.job, b.t
	limitMin, ratio := cfg().SeedMinutes, cfg().SeedRatio
	if limitMin <= 0 || size <= 0 {
		return
	}
	uploaded := func() int64 { st := t.Stats(); return st.BytesWrittenData.Int64() }
	base := uploaded()
	deadline := time.Now().Add(time.Duration(limitMin) * time.Minute)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var last int64
	lastAt := time.Now()
	setSeeding := func(on bool, up int64, r float64) {
		job.mu.Lock()
		job.Seeding, job.RateUp, job.Ratio = on, up, r
		job.notifySubscribers()
		job.mu.Unlock()
	}
	setSeeding(true, 0, 0)
	defer setSeeding(false, 0, 0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case <-tick.C:
		}
		up := uploaded() - base
		r := float64(up) / float64(size)
		rate := int64(float64(up-last) / time.Since(lastAt).Seconds())
		last, lastAt = up, time.Now()
		setSeeding(true, rate, r)
		if (ratio > 0 && r >= ratio) || time.Now().After(deadline) {
			logger.Info("Seeding finished", "job", job.ID, "ratio", fmt.Sprintf("%.2f", r))
			return
		}
	}
}

// stopTorrentSeeding ends seeding before files are kept or deleted, so the
// engine no longer holds them open.
func stopTorrentSeeding(jobID string) {
	btMu.Lock()
	b := btJobs[jobID]
	btMu.Unlock()
	if b == nil {
		return
	}
	b.stopSeeding()
	for i := 0; i < 50; i++ {
		btMu.Lock()
		_, still := btJobs[jobID]
		btMu.Unlock()
		if !still {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startTorrentForRequest is the built-in engine's /api/download path.
func startTorrentForRequest(w http.ResponseWriter, r *http.Request, raw, owner, title, destination string) bool {
	if n := cfg().MaxActiveTorrents; n > 0 && activeTorrentCount() >= n {
		jsonError(w, fmt.Sprintf("%d torrents are already downloading. Try again when one finishes.", n), http.StatusServiceUnavailable)
		return false
	}
	var magnet string
	var meta []byte
	if strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		magnet = raw
		if title == "" || title == "Magnet link" {
			title = magnetName(raw)
		}
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		data, err := fetchTorrentFile(ctx, raw)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return false
		}
		meta = data
	}
	return startTorrentJob(w, owner, title, raw, destination, magnet, meta)
}

func startTorrentJob(w http.ResponseWriter, owner, title, source, destination, magnet string, meta []byte) bool {
	if len(source) > 2000 {
		source = source[:2000]
	}
	job := newTorrentJob(generateID(), owner, title, source, destination, time.Now())
	if _, err := launchTorrent(job, magnet, meta); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return false
	}
	logger.Info("Torrent started", "user", owner, "job", job.ID, "title", job.Title)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID, "token": job.Token})
	return true
}

// handleBuiltinTorrentUpload accepts a .torrent file upload.
func handleBuiltinTorrentUpload(w http.ResponseWriter, r *http.Request, a *authInfo) {
	if err := killSwitchGate(r.Context()); err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
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
	if n := cfg().MaxActiveTorrents; n > 0 && activeTorrentCount() >= n {
		jsonError(w, fmt.Sprintf("%d torrents are already downloading. Try again when one finishes.", n), http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimSuffix(sanitizeDirectName(header.Filename), ".torrent")
	startTorrentJob(w, a.User.Username, name, "upload: "+header.Filename, "device", "", data)
}

// jobIDNameRe matches a running job's own working folder (16 hex chars).
var jobIDNameRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// torrentJobBusy reports whether files of jobID are in use by the engine.
func torrentJobBusy(jobID string) bool {
	btMu.Lock()
	defer btMu.Unlock()
	_, ok := btJobs[jobID]
	return ok
}
