package main

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// newSeeder serves a freshly generated file over BitTorrent and returns the
// client, the .torrent bytes and the file content.
func newSeeder(t *testing.T, name string, size int) (*torrent.Client, []byte, []byte) {
	t.Helper()
	dir := t.TempDir()
	content := make([]byte, size)
	rand.Read(content)
	if err := os.WriteFile(filepath.Join(dir, name), content, 0644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 64 << 10}
	if err := info.BuildFromFilePath(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dir
	cfg.Seed = true
	cfg.NoDHT = true
	cfg.ListenPort = 0
	cfg.DisableIPv6 = true
	seeder, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seeder.Close() })
	st, err := seeder.AddTorrent(&mi)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.VerifyData(); err != nil {
		t.Fatal(err)
	}
	return seeder, buf.Bytes(), content
}

// setupEngine points the built-in engine at temp dirs and a fresh client.
func setupEngine(t *testing.T) (lib string) {
	t.Helper()
	t.Setenv("LEECHER_HOME", t.TempDir())
	lib = t.TempDir()
	withConfig(t, func(c *Config) {
		c.TorrentEngine = engineBuiltin
		c.LibraryDir = lib
		c.DownloadsDir = t.TempDir()
		c.AllowPrivateURLs = true // the seeder is on loopback
		c.MinDiskFreeMB = 0
		c.MaxFileSizeMB = 0
		c.TorrentPort = 0
		c.SeedMinutes = 60
		c.SeedRatio = 1
	})
	btNoDHT = true
	btMu.Lock()
	btClient, btJobs = nil, map[string]*btJob{}
	btMu.Unlock()
	jobHistoryMu.Lock()
	oldHist := jobHistory
	jobHistory = nil
	jobHistoryMu.Unlock()
	t.Cleanup(func() {
		btMu.Lock()
		if btClient != nil {
			btClient.Close()
		}
		btClient, btJobs = nil, map[string]*btJob{}
		btMu.Unlock()
		jobHistoryMu.Lock()
		jobHistory = oldHist
		jobHistoryMu.Unlock()
	})
	return lib
}

func waitJob(t *testing.T, job *Job, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		job.mu.Lock()
		ok := cond()
		job.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	t.Fatalf("timed out waiting for %s (status=%s msg=%q err=%q)", what, job.Status, job.Message, job.Error)
}

func TestBuiltinTorrent_DownloadSeedAndKeep(t *testing.T) {
	lib := setupEngine(t)
	seeder, meta, content := newSeeder(t, "movie.mkv", 1<<20)

	job := newTorrentJob(generateID(), "bob", "", "upload: movie.torrent", "device", time.Now())
	b, err := launchTorrent(job, "", meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, job.ID); jobsMu.Unlock() })
	b.t.AddClientPeer(seeder)

	waitJob(t, job, func() bool { return job.Status == "done" }, "download to finish")
	got, err := os.ReadFile(filepath.Join(lib, workDirName, job.ID+"_movie.mkv"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("downloaded file missing or different: %v", err)
	}
	if job.Title != "movie.mkv" || job.DisplayName != "movie.mkv" {
		t.Errorf("title should come from the torrent, got %q / %q", job.Title, job.DisplayName)
	}
	waitJob(t, job, func() bool { return job.Seeding }, "seeding to start")
	if !torrentJobBusy(job.ID) || !entryInUse(job.ID+"_movie.mkv") {
		t.Error("a seeding torrent's files must be protected from cleanup")
	}

	// Keep stops seeding and moves the file into the library.
	dst, err := keepJobFile(jobRef{ID: job.ID, Owner: "bob", Status: "done", Filename: job.ID + "_movie.mkv", DisplayName: "movie.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if dst != filepath.Join(lib, "movie.mkv") {
		t.Errorf("kept to %s", dst)
	}
	if torrentJobBusy(job.ID) {
		t.Error("keeping must end seeding")
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, content) {
		t.Error("kept file content differs")
	}
	if len(loadBTResume()) != 0 {
		t.Error("finished torrents must not be resumed after a restart")
	}
}

func TestBuiltinTorrent_CancelRemovesPartialData(t *testing.T) {
	lib := setupEngine(t)
	_, meta, _ := newSeeder(t, "big.iso", 1<<20) // never connected: stays waiting

	job := newTorrentJob(generateID(), "bob", "", "upload: big.torrent", "device", time.Now())
	if _, err := launchTorrent(job, "", meta); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, job.ID); jobsMu.Unlock() })
	if len(loadBTResume()) != 1 {
		t.Fatal("a running torrent should be recorded for resume")
	}
	if _, err := launchTorrent(newTorrentJob(generateID(), "carol", "", "x", "device", time.Now()), "", meta); err == nil {
		t.Error("the same torrent must not run twice")
	}
	job.mu.Lock()
	job.Status = "cancelled"
	job.mu.Unlock()
	job.cancel()
	deadline := time.Now().Add(10 * time.Second)
	for torrentJobBusy(job.ID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if torrentJobBusy(job.ID) {
		t.Fatal("cancelled torrent should be dropped")
	}
	if m, _ := filepath.Glob(filepath.Join(lib, workDirName, job.ID+"_*")); len(m) != 0 {
		t.Errorf("partial data should be removed, found %v", m)
	}
	if len(loadBTResume()) != 0 {
		t.Error("a cancelled torrent must not be resumed")
	}
}

func TestPrivateRangerBlocksLAN(t *testing.T) {
	withConfig(t, func(c *Config) { c.AllowPrivateURLs = false })
	r := privateRanger{}
	for _, ip := range []string{"192.168.0.66", "10.43.0.1", "127.0.0.1", "::1", "169.254.1.1"} {
		if _, blocked := r.Lookup(net.ParseIP(ip)); !blocked {
			t.Errorf("%s must be blocked for peers", ip)
		}
	}
	if _, blocked := r.Lookup(net.ParseIP("1.1.1.1")); blocked {
		t.Error("public peers must be allowed")
	}
}

func TestTorrentEngineSelection(t *testing.T) {
	withConfig(t, func(c *Config) { c.TorrentEngine = ""; c.TransmissionURL = "http://x:9091/transmission/rpc" })
	if torrentEngine() != engineBuiltin || !torrentsEnabled() || transmissionEnabled() {
		t.Error("the built-in engine is the default, even if a Transmission URL is configured")
	}
	withConfig(t, func(c *Config) { c.TorrentEngine = engineTransmission })
	if !transmissionEnabled() {
		t.Error("transmission engine should use the configured URL")
	}
	withConfig(t, func(c *Config) { c.TorrentEngine = engineOff })
	if torrentsEnabled() {
		t.Error("off disables torrents")
	}
}

func TestBuiltinTorrent_MultiFileSavesAsZip(t *testing.T) {
	setupEngine(t)
	withUsers(t)
	src := filepath.Join(t.TempDir(), "Album")
	os.MkdirAll(filepath.Join(src, "CD1"), 0755)
	a, b := make([]byte, 200<<10), make([]byte, 300<<10)
	rand.Read(a)
	rand.Read(b)
	os.WriteFile(filepath.Join(src, "CD1", "01.flac"), a, 0644)
	os.WriteFile(filepath.Join(src, "cover.jpg"), b, 0644)
	info := metainfo.Info{PieceLength: 64 << 10}
	if err := info.BuildFromFilePath(src); err != nil {
		t.Fatal(err)
	}
	infoBytes, _ := bencode.Marshal(info)
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	var meta bytes.Buffer
	mi.Write(&meta)
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir, cfg.Seed, cfg.NoDHT, cfg.ListenPort, cfg.DisableIPv6 = filepath.Dir(src), true, true, 0, true
	seeder, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer seeder.Close()
	st, _ := seeder.AddTorrent(&mi)
	st.VerifyData()

	job := newTorrentJob(generateID(), "bob", "", "upload: album.torrent", "device", time.Now())
	bt, err := launchTorrent(job, "", meta.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, job.ID); jobsMu.Unlock() })
	bt.t.AddClientPeer(seeder)
	waitJob(t, job, func() bool { return job.Status == "done" }, "multi-file download")

	if p := findTempFile(job.ID + "_Album"); p == "" {
		t.Fatal("finished folder should be found in the work area")
	}
	w := httptest.NewRecorder()
	handleJobFile(w, asUser(t, httptest.NewRequest("GET", "/api/files/"+job.ID, nil), "bob"))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" || !strings.Contains(w.Header().Get("Content-Disposition"), "Album.zip") {
		t.Fatalf("folder should be served as a ZIP: %d %v", w.Code, w.Header())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["Album/CD1/01.flac"] || !names["Album/cover.jpg"] {
		t.Errorf("zip should contain the torrent's files, got %v", names)
	}
}

func TestBuiltinTorrent_PauseAndResume(t *testing.T) {
	lib := setupEngine(t)
	seeder, meta, content := newSeeder(t, "show.mkv", 1<<20)
	job := newTorrentJob(generateID(), "bob", "", "upload: show.torrent", "device", time.Now())
	if _, err := launchTorrent(job, "", meta); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, job.ID); jobsMu.Unlock() })

	if err := pauseTorrent(job.ID); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); torrentJobBusy(job.ID) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if torrentJobBusy(job.ID) {
		t.Fatal("a paused torrent must leave the engine")
	}
	if r, ok := loadBTResume()[job.ID]; !ok || !r.Paused {
		t.Fatal("the pause must be remembered for restarts")
	}
	resp := buildJobStatusResponse(job, false)
	if resp["status"] != "paused" || resp["user_paused"] != true {
		t.Errorf("unexpected status %v", resp)
	}

	// Neither the kill switch's automatic resume nor a restart may undo it.
	resumeTorrents()
	jobsMu.Lock()
	delete(jobs, job.ID) // as after a restart
	jobsMu.Unlock()
	resumeTorrents()
	if torrentJobBusy(job.ID) {
		t.Fatal("a torrent paused by its user must stay paused")
	}
	jobsMu.RLock()
	restored := jobs[job.ID]
	jobsMu.RUnlock()
	if restored == nil || restored.Status != "paused" || !restored.UserPaused {
		t.Fatalf("after a restart the torrent should be listed as paused, got %+v", restored)
	}

	if err := resumePausedTorrent(job.ID); err != nil {
		t.Fatal(err)
	}
	btMu.Lock()
	b := btJobs[job.ID]
	btMu.Unlock()
	if b == nil {
		t.Fatal("resume should restart the torrent")
	}
	b.t.AddClientPeer(seeder)
	waitJob(t, restored, func() bool { return restored.Status == "done" }, "resumed download to finish")
	if got, _ := os.ReadFile(filepath.Join(lib, workDirName, job.ID+"_show.mkv")); !bytes.Equal(got, content) {
		t.Error("resumed download incomplete")
	}
	if err := pauseTorrent(job.ID); err == nil {
		t.Error("a finished torrent cannot be paused")
	}
}
