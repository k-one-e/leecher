package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskGroups_MergesFoldersOnSameDisk(t *testing.T) {
	lib := t.TempDir()
	withConfig(t, func(c *Config) { c.LibraryDir = lib })
	if err := os.MkdirAll(tempDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	groups := diskGroups()
	if len(groups) != 1 {
		t.Fatalf("want 1 group for downloads inside the library, got %d: %+v", len(groups), groups)
	}
	g := groups[0]
	if g.id != "downloads" || len(g.labels) != 2 || g.labels[1] != "Library" {
		t.Fatalf("unexpected group %+v", g)
	}
}

func TestDiskGroups_MissingFolderReportsError(t *testing.T) {
	withConfig(t, func(c *Config) {
		c.LibraryDir = ""
		c.DownloadsDir = filepath.Join(t.TempDir(), "missing")
	})
	groups := diskGroups()
	if len(groups) != 1 || groups[0].err == nil {
		t.Fatalf("want one group with an error, got %+v", groups)
	}
}

func TestDiskIsLow(t *testing.T) {
	withConfig(t, func(c *Config) { c.MinDiskFreeMB = 500 })
	cases := []struct {
		total, free uint64
		low         bool
	}{
		{100 << 30, 50 << 30, false},
		{100 << 30, 4 << 30, true}, // under 5%
		{2 << 40, 900 << 20, true}, // under 2× min_disk_free_mb
		{2 << 40, 200 << 30, false},
	}
	for _, c := range cases {
		if got := diskIsLow(c.total, c.free); got != c.low {
			t.Errorf("diskIsLow(%d, %d) = %v, want %v", c.total, c.free, got, c.low)
		}
	}
}

func TestHandleDisk_HidesPathsFromUsers(t *testing.T) {
	withUsers(t)
	dir := t.TempDir()
	withConfig(t, func(c *Config) { c.DownloadsDir = dir; c.LibraryDir = "" })

	get := func(name string) map[string]any {
		rec := httptest.NewRecorder()
		handleDisk(rec, asUser(t, httptest.NewRequest("GET", "/api/disk", nil), name))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", name, rec.Code)
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	user := get("bob")
	disks := user["disks"].([]any)
	if len(disks) != 1 {
		t.Fatalf("want 1 disk, got %v", disks)
	}
	d := disks[0].(map[string]any)
	if d["total"].(float64) <= 0 || d["free"].(float64) <= 0 {
		t.Fatalf("missing sizes: %v", d)
	}
	if _, ok := d["paths"]; ok {
		t.Fatal("users must not see server paths")
	}
	if _, ok := user["bench_running"]; ok {
		t.Fatal("users must not see benchmark state")
	}

	admin := get("alice")
	ad := admin["disks"].([]any)[0].(map[string]any)
	if ps, ok := ad["paths"].([]any); !ok || ps[0] != dir {
		t.Fatalf("admin should see paths, got %v", ad["paths"])
	}
}

func TestHandleAdminDiskBench(t *testing.T) {
	withUsers(t)
	dir := t.TempDir()
	withConfig(t, func(c *Config) { c.DownloadsDir = dir; c.LibraryDir = ""; c.MinDiskFreeMB = 0 })

	rec := httptest.NewRecorder()
	handleAdminDiskBench(rec, asUser(t, httptest.NewRequest("POST", "/api/admin/disk-bench?disk=downloads", nil), "bob"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-admin: want 401, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	handleAdminDiskBench(rec, asUser(t, httptest.NewRequest("POST", "/api/admin/disk-bench?disk=nope", nil), "alice"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown disk: want 400, got %d", rec.Code)
	}

	diskBenchRunning.Store(true)
	rec = httptest.NewRecorder()
	handleAdminDiskBench(rec, asUser(t, httptest.NewRequest("POST", "/api/admin/disk-bench?disk=downloads", nil), "alice"))
	diskBenchRunning.Store(false)
	if rec.Code != http.StatusConflict {
		t.Fatalf("concurrent run: want 409, got %d", rec.Code)
	}
}

func TestRunDiskBench(t *testing.T) {
	dir := t.TempDir()
	res, err := runDiskBench(context.Background(), dir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != 8<<20 || res.WriteBps <= 0 || res.ReadBps <= 0 {
		t.Fatalf("bad result %+v", res)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".leecher-diskbench-*")); len(left) != 0 {
		t.Fatalf("test file not removed: %v", left)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runDiskBench(ctx, dir, 8<<20); err == nil {
		t.Fatal("want error for cancelled context")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".leecher-diskbench-*")); len(left) != 0 {
		t.Fatalf("test file not removed after cancel: %v", left)
	}
}

func TestIORates(t *testing.T) {
	t0 := time.Unix(1000, 0)
	old := ioSample{at: t0, disks: map[uint64]diskIOCount{1: {name: "sda", read: 0, written: 0}, 2: {name: "sdb", read: 500, written: 500}}, rx: 1000, tx: 100, netOK: true}
	cur := ioSample{at: t0.Add(10 * time.Second), disks: map[uint64]diskIOCount{1: {name: "sda", read: 10_000, written: 50_000}, 2: {name: "sdb", read: 0, written: 0}}, rx: 101_000, tx: 1100, netOK: true}
	rates, n := ioRates(old, cur)
	if r := rates[1]; r.readBps != 1000 || r.writeBps != 5000 {
		t.Fatalf("sda rates %+v", r)
	}
	if _, ok := rates[2]; ok {
		t.Fatal("counters that went backwards must be skipped")
	}
	if n == nil || n.rxBps != 10_000 || n.txBps != 100 {
		t.Fatalf("net rates %+v", n)
	}

	cur.netOK = false
	if _, n := ioRates(old, cur); n != nil {
		t.Fatal("no net rate without counters")
	}
}
