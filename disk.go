package main

// disk.go — free space, live throughput and an on-demand speed test for the
// disks leecher writes to.

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	diskIOInterval    = 2 * time.Second
	diskIOWindow      = 5 // samples: rates are averaged over the last 10s
	diskBenchSize     = 256 << 20
	diskBenchChunk    = 4 << 20
	diskBenchHeadroom = 64 << 20
	diskBenchTimeout  = 3 * time.Minute
)

type diskIOCount struct {
	name          string
	read, written uint64
}

type diskRate struct {
	name              string
	readBps, writeBps float64
}

type netRate struct {
	rxBps, txBps float64
}

// ioSample is one reading of the kernel's cumulative counters.
type ioSample struct {
	at     time.Time
	disks  map[uint64]diskIOCount
	rx, tx uint64
	netOK  bool
}

var diskIO struct {
	mu    sync.Mutex
	rates map[uint64]diskRate
	net   *netRate
}

// ioRates turns two samples into per-second rates. Counters that went
// backwards (device re-attached, interface reset) are skipped.
func ioRates(old, cur ioSample) (map[uint64]diskRate, *netRate) {
	secs := cur.at.Sub(old.at).Seconds()
	if secs <= 0 {
		return nil, nil
	}
	rates := make(map[uint64]diskRate, len(cur.disks))
	for dev, c := range cur.disks {
		p, ok := old.disks[dev]
		if !ok || c.read < p.read || c.written < p.written {
			continue
		}
		rates[dev] = diskRate{
			name:     c.name,
			readBps:  float64(c.read-p.read) / secs,
			writeBps: float64(c.written-p.written) / secs,
		}
	}
	var n *netRate
	if old.netOK && cur.netOK && cur.rx >= old.rx && cur.tx >= old.tx {
		n = &netRate{rxBps: float64(cur.rx-old.rx) / secs, txBps: float64(cur.tx-old.tx) / secs}
	}
	return rates, n
}

func takeIOSample() (ioSample, bool) {
	s := ioSample{at: time.Now()}
	disks, derr := readDiskStats()
	s.disks = disks
	rx, tx, nerr := readNetDev()
	s.rx, s.tx, s.netOK = rx, tx, nerr == nil
	return s, derr == nil || s.netOK
}

// diskIOLoop samples kernel disk and network counters so the UI can show
// live throughput. Rates span a sliding window: the kernel flushes writes to
// disk in bursts, and a single 2s sample would flicker between 0 and peaks.
// It exits immediately where counters are unavailable (non-Linux).
func diskIOLoop() {
	first, ok := takeIOSample()
	if !ok {
		return
	}
	window := []ioSample{first}
	t := time.NewTicker(diskIOInterval)
	defer t.Stop()
	for {
		select {
		case <-shutdownCh:
			return
		case <-t.C:
		}
		cur, ok := takeIOSample()
		if !ok {
			continue
		}
		rates, n := ioRates(window[0], cur)
		diskIO.mu.Lock()
		diskIO.rates, diskIO.net = rates, n
		diskIO.mu.Unlock()
		window = append(window, cur)
		if len(window) > diskIOWindow {
			window = window[1:]
		}
	}
}

func diskRateFor(dev uint64) (diskRate, bool) {
	diskIO.mu.Lock()
	defer diskIO.mu.Unlock()
	r, ok := diskIO.rates[dev]
	return r, ok
}

func currentNetRate() *netRate {
	diskIO.mu.Lock()
	defer diskIO.mu.Unlock()
	return diskIO.net
}

type diskGroup struct {
	id     string
	labels []string
	paths  []string
	key    string
	dev    uint64
	err    error
}

// diskGroups lists the folders leecher writes to, merged by filesystem so a
// disk shared by several folders is reported once.
func diskGroups() []diskGroup {
	type target struct{ label, path string }
	targets := []target{{"Downloads", tempDir()}}
	if lib := cfg().LibraryDir; lib != "" {
		targets = append(targets, target{"Library", lib})
	}

	var groups []diskGroup
	byKey := map[string]int{}
	for _, t := range targets {
		if t.path == "" {
			continue
		}
		key, dev, err := diskDevice(t.path)
		if err != nil {
			groups = append(groups, diskGroup{id: strings.ToLower(t.label), labels: []string{t.label}, paths: []string{t.path}, err: err})
			continue
		}
		if i, ok := byKey[key]; ok {
			groups[i].labels = append(groups[i].labels, t.label)
			groups[i].paths = append(groups[i].paths, t.path)
			continue
		}
		byKey[key] = len(groups)
		groups = append(groups, diskGroup{id: strings.ToLower(t.label), labels: []string{t.label}, paths: []string{t.path}, key: key, dev: dev})
	}
	return groups
}

type diskIOJSON struct {
	ReadBps  float64 `json:"read_bps"`
	WriteBps float64 `json:"write_bps"`
}

type diskBenchResult struct {
	Size       int64     `json:"size"`
	WriteBps   float64   `json:"write_bps"`
	ReadBps    float64   `json:"read_bps"`
	ReadCached bool      `json:"read_cached,omitempty"`
	At         time.Time `json:"at"`
}

type diskInfo struct {
	ID     string           `json:"id"`
	Label  string           `json:"label"`
	Total  uint64           `json:"total"`
	Free   uint64           `json:"free"`
	Used   uint64           `json:"used"`
	Low    bool             `json:"low"`
	IO     *diskIOJSON      `json:"io,omitempty"`
	Paths  []string         `json:"paths,omitempty"`
	Device string           `json:"device,omitempty"`
	Bench  *diskBenchResult `json:"bench,omitempty"`
	Error  string           `json:"error,omitempty"`
}

var (
	diskBenchRunning atomic.Bool
	diskBenchMu      sync.Mutex
	diskBenchLast    = map[string]diskBenchResult{}
)

// diskIsLow flags a disk that is close to blocking new downloads: under twice
// min_disk_free_mb, or under 5% of its size.
func diskIsLow(total, free uint64) bool {
	if minFree := cfg().MinDiskFreeMB; minFree > 0 && free < uint64(minFree)*2<<20 {
		return true
	}
	return total > 0 && free < total/20
}

// collectDisks reports every disk. Paths, device names, errors and benchmark
// results are admin-only; users just see how much room is left.
func collectDisks(admin bool) []diskInfo {
	var out []diskInfo
	for _, g := range diskGroups() {
		d := diskInfo{ID: g.id, Label: strings.Join(g.labels, " · ")}
		if admin {
			d.Paths = g.paths
		}
		total, avail, err := uint64(0), uint64(0), g.err
		if err == nil {
			total, avail, err = diskSpaceAt(g.paths[0])
		}
		if err != nil {
			if admin {
				d.Error = err.Error()
			}
			out = append(out, d)
			continue
		}
		d.Total, d.Free = total, avail
		if total > avail {
			d.Used = total - avail
		}
		d.Low = diskIsLow(total, avail)
		if r, ok := diskRateFor(g.dev); ok {
			d.IO = &diskIOJSON{ReadBps: r.readBps, WriteBps: r.writeBps}
			if admin {
				d.Device = r.name
			}
		}
		if admin {
			diskBenchMu.Lock()
			if b, ok := diskBenchLast[g.id]; ok {
				d.Bench = &b
			}
			diskBenchMu.Unlock()
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID == "downloads" && out[j].ID != "downloads" })
	return out
}

func handleDisk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	admin := isAdminAuthenticated(r)
	resp := map[string]any{"disks": collectDisks(admin)}
	// Leecher's own network receive rate: the steadiest "downloading now"
	// signal, since every engine (yt-dlp, direct, torrents) shows up here.
	if n := currentNetRate(); n != nil {
		resp["net"] = map[string]float64{"rx_bps": n.rxBps, "tx_bps": n.txBps}
	}
	if admin {
		resp["bench_running"] = diskBenchRunning.Load()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAdminDiskBench writes and reads back a 256 MB file on one disk to
// measure its sequential speed. One test runs at a time.
func handleAdminDiskBench(w http.ResponseWriter, r *http.Request) {
	if !isAdminAuthenticated(r) {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("disk")
	var target *diskGroup
	for _, g := range diskGroups() {
		if g.id == id && g.err == nil {
			target = &g
			break
		}
	}
	if target == nil {
		jsonError(w, "Unknown disk", http.StatusBadRequest)
		return
	}
	dir := target.paths[0]
	_, avail, err := diskSpaceAt(dir)
	if err != nil {
		jsonError(w, "Could not read free space: "+err.Error(), http.StatusInternalServerError)
		return
	}
	need := uint64(diskBenchSize+diskBenchHeadroom) + uint64(max(cfg().MinDiskFreeMB, 0))<<20
	if avail < need {
		jsonError(w, "Not enough free space for a speed test (needs "+humanBytes(int64(need))+")", http.StatusConflict)
		return
	}
	if !diskBenchRunning.CompareAndSwap(false, true) {
		jsonError(w, "A speed test is already running", http.StatusConflict)
		return
	}
	defer diskBenchRunning.Store(false)

	ctx, cancel := context.WithTimeout(r.Context(), diskBenchTimeout)
	defer cancel()
	res, err := runDiskBench(ctx, dir, diskBenchSize)
	if err != nil {
		logger.Warn("disk speed test failed", "disk", id, "error", err)
		jsonError(w, "Speed test failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	logger.Info("disk speed test", "disk", id, "write_mbps", int(res.WriteBps/1e6), "read_mbps", int(res.ReadBps/1e6))
	diskBenchMu.Lock()
	diskBenchLast[id] = res
	diskBenchMu.Unlock()
	writeJSON(w, http.StatusOK, res)
}

// runDiskBench measures sequential write (including fsync) and read speed with
// a temporary file in dir. Random data defeats compressing filesystems.
func runDiskBench(ctx context.Context, dir string, size int64) (diskBenchResult, error) {
	f, err := os.CreateTemp(dir, ".leecher-diskbench-*")
	if err != nil {
		return diskBenchResult{}, err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()

	buf := make([]byte, diskBenchChunk)
	if _, err := rand.Read(buf); err != nil {
		return diskBenchResult{}, err
	}

	start := time.Now()
	for written := int64(0); written < size; {
		if err := ctx.Err(); err != nil {
			return diskBenchResult{}, err
		}
		n, err := f.Write(buf[:min(int64(len(buf)), size-written)])
		if err != nil {
			return diskBenchResult{}, err
		}
		written += int64(n)
	}
	if err := f.Sync(); err != nil {
		return diskBenchResult{}, err
	}
	writeDur := time.Since(start)
	cached := !dropPageCache(f)
	f.Close()

	rf, err := os.Open(name)
	if err != nil {
		return diskBenchResult{}, err
	}
	defer rf.Close()
	start = time.Now()
	var read int64
	for {
		if err := ctx.Err(); err != nil {
			return diskBenchResult{}, err
		}
		n, err := rf.Read(buf)
		read += int64(n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return diskBenchResult{}, err
		}
	}
	readDur := time.Since(start)
	if read != size {
		return diskBenchResult{}, errors.New("read back " + humanBytes(read) + " of " + humanBytes(size))
	}

	return diskBenchResult{
		Size:       size,
		WriteBps:   float64(size) / max(writeDur.Seconds(), 1e-6),
		ReadBps:    float64(size) / max(readDur.Seconds(), 1e-6),
		ReadCached: cached,
		At:         time.Now().UTC(),
	}, nil
}

// cleanupDiskBenchFiles removes test files left behind by a crash mid-test.
func cleanupDiskBenchFiles() {
	for _, g := range diskGroups() {
		for _, p := range g.paths {
			matches, _ := filepath.Glob(filepath.Join(p, ".leecher-diskbench-*"))
			for _, m := range matches {
				os.Remove(m)
			}
		}
	}
}
