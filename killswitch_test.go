package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withKillSwitchIP arms the kill switch for allowed and serves *ip from fake
// IP services; an empty *ip makes every service fail.
func withKillSwitchIP(t *testing.T, allowed []string) *atomic.Value {
	t.Helper()
	var ip atomic.Value
	ip.Store("")
	handler := func(w http.ResponseWriter, r *http.Request) {
		v := ip.Load().(string)
		if v == "" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if v == "hang" {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		w.Write([]byte(v + "\n"))
	}
	var urls []string
	for i := 0; i < 2; i++ {
		s := httptest.NewServer(http.HandlerFunc(handler))
		t.Cleanup(s.Close)
		urls = append(urls, s.URL)
	}
	orig := killSwitchProviders
	killSwitchProviders = urls
	withConfig(t, func(c *Config) {
		c.AllowPrivateURLs = true // httptest is on loopback
		c.KillSwitch = true
		c.KillSwitchIPs = allowed
		c.KillSwitchIntervalSecs = 1
	})
	resetKillSwitch := func() {
		ks.mu.Lock()
		ks.armed, ks.verified, ks.tripped = false, false, false
		ks.ip, ks.reason, ks.checkedAt = "", "", time.Time{}
		ks.stopCtx, ks.stopFn = context.WithCancel(context.Background())
		ks.mu.Unlock()
		ksTrMu.Lock()
		ksTrStopped = nil
		ksTrMu.Unlock()
	}
	resetKillSwitch()
	t.Cleanup(func() {
		killSwitchProviders = orig
		resetKillSwitch()
	})
	return &ip
}

func TestNormalizeAllowedIPs(t *testing.T) {
	got, err := normalizeAllowedIPs([]string{" 203.0.113.7 ", "198.51.100.9/24", "203.0.113.7", "", "::ffff:192.0.2.1", "2001:db8::1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.7", "198.51.100.0/24", "192.0.2.1", "2001:db8::1"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"vpn.example.com", "1.2.3.4/40", "300.1.1.1"} {
		if _, err := normalizeAllowedIPs([]string{bad}); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	allowed, _ := parseAllowedIPs([]string{"198.51.100.0/24", "203.0.113.7"})
	for ip, want := range map[string]bool{"198.51.100.200": true, "203.0.113.7": true, "203.0.113.8": false, "::ffff:203.0.113.7": true, "garbage": false} {
		if got := ipAllowed(ip, allowed); got != want {
			t.Errorf("ipAllowed(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestKillSwitch_DisabledNeverBlocks(t *testing.T) {
	ip := withKillSwitchIP(t, []string{"203.0.113.7"})
	withConfig(t, func(c *Config) { c.KillSwitch = false })
	ip.Store("198.51.100.1")
	if err := killSwitchGate(context.Background()); err != nil {
		t.Errorf("a disabled kill switch must not block: %v", err)
	}
}

func TestKillSwitch_GateFollowsTheOutboundIP(t *testing.T) {
	ip := withKillSwitchIP(t, []string{"203.0.113.7"})

	if blocked, _ := killSwitchBlocked(); !blocked {
		t.Error("an armed switch must hold downloads until the first check passes")
	}
	ip.Store("203.0.113.7")
	if err := killSwitchGate(context.Background()); err != nil {
		t.Fatalf("allowed IP should pass: %v", err)
	}
	if st := currentKillSwitchStatus(); !st.Enabled || !st.Verified || st.Tripped || st.IP != "203.0.113.7" {
		t.Errorf("unexpected status %+v", st)
	}

	ip.Store("198.51.100.1")
	ksResetChecked()
	err := killSwitchGate(context.Background())
	if !errors.Is(err, errKillSwitch) {
		t.Fatalf("a foreign IP must block, got %v", err)
	}
	if st := currentKillSwitchStatus(); !st.Tripped || st.IP != "198.51.100.1" || st.TrippedAt.IsZero() {
		t.Errorf("unexpected status %+v", st)
	}

	ip.Store("203.0.113.7")
	ksResetChecked()
	if err := killSwitchGate(context.Background()); err != nil {
		t.Errorf("should recover once the allowed IP is back: %v", err)
	}
}

func TestKillSwitch_FailsClosedWhenTheIPIsUnknown(t *testing.T) {
	ip := withKillSwitchIP(t, []string{"203.0.113.7"})
	ip.Store("203.0.113.7")
	killSwitchCheck(context.Background())
	ip.Store("") // every lookup service fails
	ksResetChecked()
	if err := killSwitchGate(context.Background()); !errors.Is(err, errKillSwitch) {
		t.Fatalf("an unverifiable IP must block, got %v", err)
	}
	if st := currentKillSwitchStatus(); !st.Tripped || st.Reason == "" {
		t.Errorf("unexpected status %+v", st)
	}
}

func TestKillSwitch_TripStopsDownloadsAndProbes(t *testing.T) {
	ip := withKillSwitchIP(t, []string{"203.0.113.7"})
	ip.Store("203.0.113.7")
	killSwitchCheck(context.Background())

	job := newTestJob(t)
	job.Kind = kindDirect
	job.Status = "downloading"
	var cancelled atomic.Bool
	job.cancel = func() { cancelled.Store(true) }
	probeCtx, done := withKillSwitch(context.Background())
	defer done()

	ip.Store("198.51.100.1")
	killSwitchCheck(context.Background())

	if !cancelled.Load() {
		t.Error("the running download must be cancelled")
	}
	job.mu.Lock()
	resp := buildJobStatusResponse(job, false)
	job.mu.Unlock()
	if resp["status"] != "cancelled" || resp["error"] == nil || resp["error"] == "" {
		t.Errorf("the job should say why it stopped, got %v", resp)
	}
	select {
	case <-probeCtx.Done():
	case <-time.After(time.Second):
		t.Error("in-flight link checks must be cancelled too")
	}
	w := httptest.NewRecorder()
	handleDownload(w, httptest.NewRequest("POST", "/api/download", strings.NewReader(`{"url":"https://www.youtube.com/watch?v=dQw4w9WgXcQ"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("new downloads must be refused while tripped, got %d %s", w.Code, w.Body)
	}
}

func TestKillSwitch_PausesAndResumesBuiltinTorrents(t *testing.T) {
	lib := setupEngine(t)
	ip := withKillSwitchIP(t, []string{"203.0.113.7"})
	ip.Store("203.0.113.7")
	killSwitchCheck(context.Background())
	seeder, meta, content := newSeeder(t, "linux.iso", 1<<20)

	job := newTorrentJob(generateID(), "bob", "", "upload: linux.torrent", "device", time.Now())
	if _, err := launchTorrent(job, "", meta); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, job.ID); jobsMu.Unlock() })

	ip.Store("198.51.100.1")
	killSwitchCheck(context.Background())
	waitJob(t, job, func() bool { return job.Status == "paused" }, "torrent to pause")
	deadline := time.Now().Add(10 * time.Second)
	for torrentJobBusy(job.ID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if torrentJobBusy(job.ID) {
		t.Fatal("a paused torrent must leave the engine")
	}
	btMu.Lock()
	engineUp := btClient != nil
	btMu.Unlock()
	if engineUp {
		t.Error("the torrent engine must be shut down while tripped")
	}
	if _, ok := loadBTResume()[job.ID]; !ok {
		t.Fatal("a paused torrent must stay on the resume list")
	}
	if !entryInUse(job.ID + "_linux.iso") {
		t.Error("cleanup must not delete a paused torrent's partial files")
	}
	if _, err := launchTorrent(newTorrentJob(generateID(), "bob", "", "x", "device", time.Now()), "", meta); !errors.Is(err, errKillSwitch) {
		t.Errorf("starting a torrent while tripped must fail, got %v", err)
	}

	ip.Store("203.0.113.7")
	killSwitchCheck(context.Background())
	if !torrentJobBusy(job.ID) {
		t.Fatal("the torrent should resume once the IP is allowed again")
	}
	btMu.Lock()
	b := btJobs[job.ID]
	btMu.Unlock()
	b.t.AddClientPeer(seeder)
	waitJob(t, job, func() bool { return job.Status == "done" }, "resumed download to finish")
	if got, err := os.ReadFile(filepath.Join(lib, workDirName, job.ID+"_linux.iso")); err != nil || len(got) != len(content) {
		t.Errorf("resumed download incomplete: %v", err)
	}
}

func TestKillSwitch_CancellingAPausedTorrentCleansUp(t *testing.T) {
	lib := setupEngine(t)
	ip := withKillSwitchIP(t, []string{"203.0.113.7"})
	ip.Store("203.0.113.7")
	killSwitchCheck(context.Background())
	_, meta, _ := newSeeder(t, "big.iso", 1<<20)
	job := newTorrentJob(generateID(), "bob", "", "upload: big.torrent", "device", time.Now())
	if _, err := launchTorrent(job, "", meta); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, job.ID); jobsMu.Unlock() })
	os.WriteFile(filepath.Join(lib, workDirName, job.ID+"_big.iso"), []byte("partial"), 0644)

	ip.Store("")
	killSwitchCheck(context.Background())
	waitJob(t, job, func() bool { return job.Status == "paused" }, "torrent to pause")

	w := httptest.NewRecorder()
	handleCancel(w, httptest.NewRequest("POST", "/api/cancel/"+job.ID+"?token="+job.Token, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("cancel failed: %d %s", w.Code, w.Body)
	}
	if _, ok := loadBTResume()[job.ID]; ok {
		t.Error("a cancelled paused torrent must not be resumed")
	}
	if m, _ := filepath.Glob(filepath.Join(lib, workDirName, job.ID+"_*")); len(m) != 0 {
		t.Errorf("its partial data should be removed, found %v", m)
	}
	ip.Store("203.0.113.7")
	killSwitchCheck(context.Background())
	if torrentJobBusy(job.ID) {
		t.Error("a cancelled torrent must not come back on recovery")
	}
}

func TestKillSwitch_HangingLookupsTrip(t *testing.T) {
	ip := withKillSwitchIP(t, []string{"203.0.113.7"})
	origTimeout := killSwitchLookupTimeout
	killSwitchLookupTimeout = 150 * time.Millisecond
	t.Cleanup(func() { killSwitchLookupTimeout = origTimeout })
	ip.Store("203.0.113.7")
	killSwitchCheck(context.Background())

	ip.Store("hang")                                                              // e.g. DNS or firewall silently dropping the lookups
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond) // an impatient caller
	defer cancel()
	ksResetChecked()
	if err := killSwitchGate(ctx); !errors.Is(err, errKillSwitch) {
		t.Fatalf("lookups that never answer must trip the switch, got %v", err)
	}
}

func TestKillSwitch_TurningOffWhileTrippedResumesTorrents(t *testing.T) {
	setupEngine(t)
	ip := withKillSwitchIP(t, []string{"203.0.113.7"})
	ip.Store("203.0.113.7")
	killSwitchCheck(context.Background())
	_, meta, _ := newSeeder(t, "data.bin", 1<<20)
	job := newTorrentJob(generateID(), "bob", "", "upload: data.torrent", "device", time.Now())
	if _, err := launchTorrent(job, "", meta); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, job.ID); jobsMu.Unlock() })

	ip.Store("198.51.100.1")
	killSwitchCheck(context.Background())
	waitJob(t, job, func() bool { return job.Status == "paused" }, "torrent to pause")
	for deadline := time.Now().Add(10 * time.Second); torrentJobBusy(job.ID) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}

	withConfig(t, func(c *Config) { c.KillSwitch = false })
	ksResetChecked()
	killSwitchCheck(context.Background()) // what the monitor does on its next tick
	if !torrentJobBusy(job.ID) {
		t.Error("turning the switch off must resume paused torrents")
	}
	if err := killSwitchGate(context.Background()); err != nil {
		t.Errorf("a disabled switch must not block: %v", err)
	}
}

func TestKillSwitch_StopsAndRestartsTransmission(t *testing.T) {
	t.Setenv("LEECHER_HOME", t.TempDir())
	var mu sync.Mutex
	var stopped, started []string
	failStop := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method    string         `json:"method"`
			Arguments map[string]any `json:"arguments"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		ids := func() (out []string) {
			for _, id := range req.Arguments["ids"].([]any) {
				out = append(out, id.(string))
			}
			return
		}
		mu.Lock()
		defer mu.Unlock()
		args := map[string]any{}
		switch req.Method {
		case "torrent-get":
			status := 4
			if slices.Contains(stopped, "aaa") && len(started) == 0 {
				status = 0
			}
			args["torrents"] = []map[string]any{{"hashString": "aaa", "status": status}, {"hashString": "bbb", "status": 0}}
		case "torrent-stop":
			if failStop {
				failStop = false
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			stopped = append(stopped, ids()...)
		case "torrent-start":
			started = append(started, ids()...)
		}
		json.NewEncoder(w).Encode(map[string]any{"result": "success", "arguments": args})
	}))
	t.Cleanup(srv.Close)
	ip := withKillSwitchIP(t, []string{"203.0.113.7"})
	withConfig(t, func(c *Config) { c.TorrentEngine = engineTransmission; c.TransmissionURL = srv.URL })

	ip.Store("203.0.113.7")
	killSwitchCheck(context.Background())
	ip.Store("198.51.100.1")
	killSwitchCheck(context.Background()) // Transmission errors on this first stop
	ksResetChecked()
	killSwitchCheck(context.Background()) // still tripped: retries
	ksResetChecked()
	killSwitchCheck(context.Background()) // already stopped: nothing to do
	mu.Lock()
	if !slices.Equal(stopped, []string{"aaa"}) {
		t.Errorf("the running torrent should be stopped once a retry succeeds, and only once; got %v", stopped)
	}
	mu.Unlock()

	ip.Store("203.0.113.7")
	killSwitchCheck(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(started, []string{"aaa"}) {
		t.Errorf("the stopped torrent should be started again, got %v", started)
	}
}
