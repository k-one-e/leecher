package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func withRetryJobState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	withConfig(t, func(c *Config) {
		c.DownloadsDir = dir
		c.LibraryDir = ""
		c.KillSwitch = false
		c.MinDiskFreeMB = 0
		c.MaxStorageMB = 0
		c.DirectDownloads = true
		c.AllowPrivateURLs = true
		c.DirectTimeoutMinutes = 1
	})
	oldJobs, oldHistory, oldLimiter := jobs, jobHistory, limiter
	jobs, jobHistory, limiter = map[string]*Job{}, nil, newRateLimiter()
	t.Cleanup(func() { jobs, jobHistory, limiter = oldJobs, oldHistory, oldLimiter })
}

func waitForRetryJob(t *testing.T, id string) *Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Workers release this counter only after writing their history.
		if downloadWaiting.Load() == 0 {
			jobsMu.RLock()
			j := jobs[id]
			jobsMu.RUnlock()
			if j == nil {
				t.Fatalf("new job %q not found", id)
			}
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("retry worker did not finish")
	return nil
}

func TestRetryDownload_LiveAndHistory(t *testing.T) {
	withUsers(t)
	for _, tc := range []struct {
		name, caller, status string
		history              bool
	}{
		{"live failure", "bob", "error", false},
		{"stopped download", "bob", "cancelled", false},
		{"history after restart", "bob", "error", true},
		{"admin preserves owner", "alice", "error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRetryJobState(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Write([]byte("retried file"))
			}))
			t.Cleanup(srv.Close)
			rec := JobRecord{ID: "original", URL: srv.URL + "/file.bin", Status: tc.status,
				Owner: "bob", Title: "Original title", Kind: kindDirect, Destination: "device",
				CreatedAt: time.Now(), FinishedAt: time.Now(), Error: "old failure"}
			appendJobHistory(rec)
			if tc.history {
				jobHistory = nil
				loadJobHistory()
			} else {
				jobs[rec.ID] = &Job{ID: rec.ID, URL: rec.URL, Status: rec.Status, Owner: rec.Owner,
					Title: rec.Title, Kind: rec.Kind, Destination: rec.Destination, Error: rec.Error}
			}
			w := httptest.NewRecorder()
			handleJobAction(w, asUser(t, httptest.NewRequest("POST", "/api/jobs/original/retry", nil), tc.caller))
			if w.Code != http.StatusOK {
				t.Fatalf("retry failed: %d %s", w.Code, w.Body)
			}
			var resp struct {
				ID    string `json:"job_id"`
				Token string `json:"token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.ID == "" || resp.ID == rec.ID || resp.Token == "" {
				t.Fatalf("retry must create a new job and token: %+v", resp)
			}
			j := waitForRetryJob(t, resp.ID)
			if j.Status != "done" || j.Owner != "bob" || j.Title != rec.Title || j.URL != rec.URL || j.Destination != "device" {
				t.Fatalf("retry did not preserve the request or finish: %+v", j)
			}
			data, err := os.ReadFile(jobFilePath(j.Filename, j.LibraryPath))
			if err != nil || string(data) != "retried file" {
				t.Fatalf("retried file = %q, err = %v", data, err)
			}
			ref, ok := findJobRef(rec.ID)
			if !ok || ref.Status != rec.Status {
				t.Fatal("retry must preserve the original attempt")
			}
			if !ref.Hidden {
				t.Fatal("successful retry must replace the original activity row")
			}
			for _, h := range jobHistory {
				if h.ID == rec.ID && (!h.Hidden || h.Status != rec.Status || h.Error != rec.Error) {
					t.Fatal("original history must be hidden but otherwise unchanged")
				}
			}
			if tc.history {
				jobs = map[string]*Job{}
				jobHistory = nil
				loadJobHistory()
			}
			again := httptest.NewRecorder()
			handleJobAction(again, asUser(t, httptest.NewRequest("POST", "/api/jobs/original/retry", nil), tc.caller))
			if again.Code != http.StatusConflict {
				t.Fatalf("repeated retry must be rejected, got %d %s", again.Code, again.Body)
			}
		})
	}
}

func TestRetryDownload_RejectsInvalidRequests(t *testing.T) {
	withUsers(t)
	withRetryJobState(t)
	jobs["failed"] = &Job{ID: "failed", Owner: "bob", Status: "error", URL: "https://example.com/file.zip"}
	jobs["active"] = &Job{ID: "active", Owner: "bob", Status: "downloading", URL: "https://example.com/file.zip"}
	jobs["done"] = &Job{ID: "done", Owner: "bob", Status: "done", URL: "https://example.com/file.zip"}
	jobs["upload"] = &Job{ID: "upload", Owner: "bob", Status: "error", URL: "upload: movie.torrent"}
	for _, tc := range []struct {
		id, method, user string
		want             int
	}{
		{"failed", "POST", "", http.StatusUnauthorized},
		{"failed", "POST", "carol", http.StatusNotFound},
		{"missing", "POST", "bob", http.StatusNotFound},
		{"failed", "GET", "bob", http.StatusMethodNotAllowed},
		{"active", "POST", "bob", http.StatusConflict},
		{"done", "POST", "bob", http.StatusConflict},
		{"upload", "POST", "bob", http.StatusConflict},
	} {
		t.Run(tc.id+"/"+tc.method+"/"+tc.user, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/api/jobs/"+tc.id+"/retry", nil)
			if tc.user != "" {
				// A real other user is needed for the ownership check.
				if tc.user == "carol" {
					if err := userDB.create("carol", "carol-password", roleUser, false); err != nil {
						t.Fatal(err)
					}
				}
				r = asUser(t, r, tc.user)
			}
			w := httptest.NewRecorder()
			handleJobAction(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body, tc.want)
			}
		})
	}
	if len(jobs) != 4 {
		t.Fatal("rejected retries must not create jobs")
	}
}

func TestRetryDownload_UsesNormalDownloadGuards(t *testing.T) {
	withUsers(t)
	for _, guard := range []string{"rate limit", "queue full", "kill switch", "invalid destination", "disabled direct downloads"} {
		t.Run(guard, func(t *testing.T) {
			withRetryJobState(t)
			j := &Job{ID: "failed", Owner: "bob", Status: "error", URL: "https://example.com/file.zip"}
			jobs[j.ID] = j
			r := asUser(t, httptest.NewRequest("POST", "/api/jobs/failed/retry", nil), "bob")
			want := http.StatusServiceUnavailable
			switch guard {
			case "rate limit":
				for i := 0; i < cfg().DownloadQueueMax; i++ {
					limiter.allow(requestActor(r)+":download", 0, float64(cfg().DownloadQueueMax))
				}
				want = http.StatusTooManyRequests
			case "queue full":
				withConfig(t, func(c *Config) { c.DownloadQueueMax = 0 })
			case "kill switch":
				ip := withKillSwitchIP(t, []string{"203.0.113.7"})
				ip.Store("198.51.100.1")
			case "invalid destination":
				j.Destination = "library"
				want = http.StatusBadRequest
			case "disabled direct downloads":
				withConfig(t, func(c *Config) { c.DirectDownloads = false })
				want = http.StatusBadRequest
			}
			w := httptest.NewRecorder()
			handleJobAction(w, r)
			if w.Code != want {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body, want)
			}
			if len(jobs) != 1 {
				t.Fatal("blocked retries must not create jobs")
			}
			if j.Hidden {
				t.Fatal("failed retry must leave the original visible and retryable")
			}
		})
	}
}

type blockingRetryWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
}

func (w *blockingRetryWriter) Write(data []byte) (int, error) {
	close(w.started)
	<-w.release
	return w.ResponseRecorder.Write(data)
}

func TestRetryDownload_ConcurrentRequestsAndUnrelatedJobs(t *testing.T) {
	withUsers(t)
	withRetryJobState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("retried file"))
	}))
	defer srv.Close()
	jobs["original"] = &Job{ID: "original", Owner: "bob", Status: "error", URL: srv.URL + "/file.bin"}
	jobs["previous"] = &Job{ID: "previous", Owner: "bob", Status: "done", Filename: "previous_file.zip"}
	w := &blockingRetryWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	first := asUser(t, httptest.NewRequest("POST", "/api/jobs/original/retry", nil), "bob")
	second := asUser(t, httptest.NewRequest("POST", "/api/jobs/original/retry", nil), "bob")
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handleJobAction(w, first)
	}()
	<-w.started
	duplicate := httptest.NewRecorder()
	handleJobAction(duplicate, second)
	close(w.release)
	<-finished
	var resp struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	waitForRetryJob(t, resp.ID)
	if duplicate.Code != http.StatusConflict || len(jobs) != 3 {
		t.Fatalf("concurrent retry created duplicates: status=%d jobs=%d", duplicate.Code, len(jobs))
	}
	previous := jobs["previous"]
	if previous.Status != "done" || previous.Hidden || previous.Filename != "previous_file.zip" {
		t.Fatal("retry must not change earlier downloads")
	}
	list := httptest.NewRecorder()
	handleJobs(list, asUser(t, httptest.NewRequest("GET", "/api/jobs", nil), "bob"))
	var activity struct {
		Jobs []struct {
			ID string `json:"id"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &activity); err != nil {
		t.Fatal(err)
	}
	if len(activity.Jobs) != 2 {
		t.Fatalf("activity should contain only the new attempt and previous download: %s", list.Body)
	}
	for _, j := range activity.Jobs {
		if j.ID == "original" {
			t.Fatal("retried failure is still visible")
		}
	}
}

func TestJobs_RetryAvailability(t *testing.T) {
	withUsers(t)
	withRetryJobState(t)
	now := time.Now()
	jobHistory = []JobRecord{
		{ID: "historical", Owner: "bob", Status: "error", URL: "https://example.com/file.zip", FinishedAt: now},
	}
	for id, status := range map[string]string{"failed": "error", "stopped": "cancelled", "active": "downloading", "done": "done"} {
		jobs[id] = &Job{ID: id, Status: status, URL: "https://example.com/file.zip", Owner: "bob"}
	}
	jobs["upload"] = &Job{ID: "upload", Status: "error", URL: "upload: x.torrent", Owner: "bob"}
	w := httptest.NewRecorder()
	handleJobs(w, asUser(t, httptest.NewRequest("GET", "/api/jobs", nil), "bob"))
	var resp struct {
		Jobs []struct {
			ID       string `json:"id"`
			CanRetry bool   `json:"can_retry"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Jobs) != 6 {
		t.Fatalf("expected 6 entries, got %d", len(resp.Jobs))
	}
	for _, j := range resp.Jobs {
		want := j.ID == "failed" || j.ID == "stopped" || j.ID == "historical"
		if j.CanRetry != want {
			t.Errorf("%s can_retry=%v, want %v", j.ID, j.CanRetry, want)
		}
	}
}

func TestRetryFormatID_LegacyAudioPlaylists(t *testing.T) {
	for _, tc := range []struct {
		url, format, want string
	}{
		{"https://music.youtube.com/playlist?list=OLAK5uy_test", "", "playlist:mp3"},
		{"https://music.youtube.com/playlist/?list=PLtest", "", "playlist:mp3"},
		{"https://soundcloud.com/artist/sets/album", "", "playlist:mp3"},
		{"https://music.youtube.com/watch?v=track", "", ""},
		{"https://music.youtube.com/watch?v=track&list=PLtest", "", ""},
		{"https://music.youtube.com/playlist", "", ""},
		{"https://www.youtube.com/playlist?list=PLtest", "", ""},
		{"https://evil.example/playlist?list=PLtest", "", ""},
		{"https://music.youtube.com.evil.example/playlist?list=PLtest", "", ""},
		{"https://music.youtube.com/playlist?list=PLtest", "ba", "ba"},
		{"https://music.youtube.com/playlist?list=PLtest", "playlist:mp3", "playlist:mp3"},
		{"https://soundcloud.com/artist/track", "", ""},
	} {
		if got := retryFormatID(tc.format, tc.url); got != tc.want {
			t.Errorf("retryFormatID(%q, %q) = %q, want %q", tc.format, tc.url, got, tc.want)
		}
	}
}

func TestHandleJobs_OwnersAndAdmins(t *testing.T) {
	withUsers(t)
	if err := userDB.create("carol", "carol-password", roleUser, false); err != nil {
		t.Fatal(err)
	}
	add := func(id, owner, status string) {
		j := &Job{ID: id, Owner: owner, Status: status, Title: "title " + id, URL: "https://x.test/" + id,
			Destination: "library", CreatedAt: time.Now(), subscribers: map[chan struct{}]struct{}{}}
		jobsMu.Lock()
		jobs[id] = j
		jobsMu.Unlock()
		t.Cleanup(func() { jobsMu.Lock(); delete(jobs, id); jobsMu.Unlock() })
	}
	add("jb1", "bob", "downloading")
	add("jb2", "carol", "downloading")

	list := func(name string) []map[string]any {
		w := httptest.NewRecorder()
		handleJobs(w, asUser(t, httptest.NewRequest("GET", "/api/jobs", nil), name))
		var resp struct {
			Jobs []map[string]any `json:"jobs"`
		}
		json.NewDecoder(w.Body).Decode(&resp)
		return resp.Jobs
	}
	got := list("bob")
	if len(got) != 1 || got[0]["id"] != "jb1" || got[0]["title"] != "title jb1" || got[0]["owner"] != nil {
		t.Errorf("bob should see only his own job, without owner info: %+v", got)
	}
	if got := list("alice"); len(got) != 2 || got[0]["owner"] == nil {
		t.Errorf("admins should see all jobs with owners: %+v", got)
	}

	// Stopping works by ownership alone (no job token after a page reload).
	jobsMu.RLock()
	job := jobs["jb1"]
	jobsMu.RUnlock()
	job.cancel = func() {}
	w := httptest.NewRecorder()
	handleCancel(w, asUser(t, httptest.NewRequest("POST", "/api/cancel/jb1", nil), "carol"))
	if w.Code != http.StatusNotFound {
		t.Errorf("carol must not stop bob's job, got %d", w.Code)
	}
	w = httptest.NewRecorder()
	handleCancel(w, asUser(t, httptest.NewRequest("POST", "/api/cancel/jb1", nil), "bob"))
	if w.Code != 200 || job.Status != "cancelled" {
		t.Errorf("bob should stop his own job, got %d / %s", w.Code, job.Status)
	}
}

func TestJobStatus_DeletedFileHasNoLink(t *testing.T) {
	dir := t.TempDir()
	withConfig(t, func(c *Config) { c.DownloadsDir = dir })
	job := &Job{ID: "gone1", Status: "done", Filename: "gone1_x.mp4", Destination: "device"}
	os.WriteFile(filepath.Join(dir, job.Filename), []byte("x"), 0644)
	if resp := buildJobStatusResponse(job, false); resp["download_url"] != "/api/files/gone1" {
		t.Fatalf("existing file should have a link: %v", resp)
	}
	os.Remove(filepath.Join(dir, job.Filename))
	resp := buildJobStatusResponse(job, false)
	if _, ok := resp["download_url"]; ok || resp["file_removed"] != true {
		t.Errorf("deleted file must not be offered for download: %v", resp)
	}
}

func TestKeepDeleteAndServeFiles(t *testing.T) {
	withUsers(t)
	if err := userDB.create("carol", "carol-password", roleUser, false); err != nil {
		t.Fatal(err)
	}
	dl, lib := t.TempDir(), t.TempDir()
	withConfig(t, func(c *Config) { c.DownloadsDir = dl; c.LibraryDir = lib; c.MaxFileAgeMinutes = 60 })
	oldLimiter := limiter
	limiter = newRateLimiter()
	t.Cleanup(func() { limiter = oldLimiter })
	jobHistoryMu.Lock()
	oldHist := jobHistory
	jobHistory = nil
	jobHistoryMu.Unlock()
	t.Cleanup(func() { jobHistoryMu.Lock(); jobHistory = oldHist; jobHistoryMu.Unlock() })

	// A finished download that only exists in history (e.g. after a restart).
	os.MkdirAll(filepath.Join(lib, workDirName), 0755)
	os.WriteFile(filepath.Join(lib, workDirName, "hjob_movie.mkv"), []byte("movie-bytes"), 0644)
	jobHistory = []JobRecord{{ID: "hjob", Status: "done", Owner: "bob", Filename: "hjob_movie.mkv", DisplayName: "movie.mkv",
		Title: "My Movie", Kind: kindDirect, CreatedAt: time.Now(), FinishedAt: time.Now()}}

	list := func(name string) []map[string]any {
		w := httptest.NewRecorder()
		handleJobs(w, asUser(t, httptest.NewRequest("GET", "/api/jobs", nil), name))
		var resp struct{ Jobs []map[string]any }
		json.NewDecoder(w.Body).Decode(&resp)
		return resp.Jobs
	}
	got := list("bob")
	if len(got) != 1 || got[0]["download_url"] != "/api/files/hjob" || got[0]["expires_at"] == nil || got[0]["title"] != "My Movie" {
		t.Fatalf("history download should be listed with a link and expiry: %+v", got)
	}
	if len(list("carol")) != 0 {
		t.Error("carol must not see bob's downloads")
	}

	fetch := func(name string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handleJobFile(w, asUser(t, httptest.NewRequest("GET", "/api/files/hjob", nil), name))
		return w
	}
	if w := fetch("carol"); w.Code != http.StatusNotFound {
		t.Errorf("carol must not download bob's file, got %d", w.Code)
	}
	if w := fetch("bob"); w.Code != 200 || w.Body.String() != "movie-bytes" || !strings.Contains(w.Header().Get("Content-Disposition"), "movie.mkv") {
		t.Fatalf("bob should get his file, got %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Disposition"))
	}

	act := func(method, path, name string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handleJobAction(w, asUser(t, httptest.NewRequest(method, path, nil), name))
		return w
	}
	if w := act("POST", "/api/jobs/hjob/keep", "carol"); w.Code != http.StatusNotFound {
		t.Errorf("carol must not keep bob's file, got %d", w.Code)
	}
	if w := act("POST", "/api/jobs/hjob/keep", "bob"); w.Code != 200 {
		t.Fatalf("keep failed: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(lib, "movie.mkv")); err != nil {
		t.Fatalf("kept file should be in the library: %v", err)
	}
	got = list("bob")
	if got[0]["saved_to"] != "library" || got[0]["expires_at"] != nil || got[0]["download_url"] != "/api/files/hjob" {
		t.Errorf("kept file should still be downloadable and never expire: %+v", got[0])
	}
	if w := fetch("bob"); w.Code != 200 || w.Body.String() != "movie-bytes" {
		t.Errorf("kept file should be served from the library, got %d", w.Code)
	}

	if w := act("DELETE", "/api/jobs/hjob/file", "bob"); w.Code != 200 {
		t.Fatalf("delete failed: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(lib, "movie.mkv")); !os.IsNotExist(err) {
		t.Error("file should be deleted")
	}
	if got = list("bob"); got[0]["file_removed"] != true || got[0]["download_url"] != nil {
		t.Errorf("deleted file must show as removed: %+v", got[0])
	}
	if w := fetch("bob"); w.Code != http.StatusGone {
		t.Errorf("deleted file should answer 410, got %d", w.Code)
	}
}

func TestInsideLibraryRejectsWorkAreaAndOutside(t *testing.T) {
	lib := t.TempDir()
	withConfig(t, func(c *Config) { c.LibraryDir = lib })
	for p, want := range map[string]bool{
		filepath.Join(lib, "a.mkv"):               true,
		filepath.Join(lib, "Movies", "b.mkv"):     true,
		filepath.Join(lib, workDirName, "c.mkv"):  false,
		filepath.Join(lib, "..", "etc", "passwd"): false,
		"/etc/passwd": false,
	} {
		if got := insideLibrary(p); got != want {
			t.Errorf("insideLibrary(%s) = %v, want %v", p, got, want)
		}
	}
}

func TestClearAndDismissDeadEntries(t *testing.T) {
	withUsers(t)
	dir := t.TempDir()
	withConfig(t, func(c *Config) { c.DownloadsDir = dir; c.LibraryDir = "" })
	oldLimiter := limiter
	limiter = newRateLimiter()
	t.Cleanup(func() { limiter = oldLimiter })
	jobHistoryMu.Lock()
	oldHist := jobHistory
	jobHistoryMu.Unlock()
	t.Cleanup(func() { jobHistoryMu.Lock(); jobHistory = oldHist; jobHistoryMu.Unlock() })

	os.WriteFile(filepath.Join(dir, "alive_a.mp4"), []byte("x"), 0644)
	now := time.Now()
	jobHistoryMu.Lock()
	jobHistory = []JobRecord{
		{ID: "alive", Owner: "bob", Status: "done", Filename: "alive_a.mp4", CreatedAt: now, FinishedAt: now},
		{ID: "gone", Owner: "bob", Status: "done", Filename: "gone_b.mp4", CreatedAt: now, FinishedAt: now},
		{ID: "failed", Owner: "bob", Status: "error", Error: "boom", CreatedAt: now, FinishedAt: now},
		{ID: "oldgone", Owner: "bob", Status: "done", Filename: "x.mp4", CreatedAt: now.Add(-48 * time.Hour), FinishedAt: now.Add(-48 * time.Hour)},
		{ID: "alices", Owner: "alice", Status: "error", Error: "boom", CreatedAt: now, FinishedAt: now},
	}
	jobHistoryMu.Unlock()

	ids := func(name string) []string {
		w := httptest.NewRecorder()
		handleJobs(w, asUser(t, httptest.NewRequest("GET", "/api/jobs", nil), name))
		var resp struct{ Jobs []map[string]any }
		json.NewDecoder(w.Body).Decode(&resp)
		var out []string
		for _, j := range resp.Jobs {
			out = append(out, j["id"].(string))
		}
		sort.Strings(out)
		return out
	}
	if got := strings.Join(ids("bob"), ","); got != "alive,failed,gone" {
		t.Fatalf("stale dead entries must drop out on their own; got %s", got)
	}

	w := httptest.NewRecorder()
	handleJobAction(w, asUser(t, httptest.NewRequest("DELETE", "/api/jobs/failed", nil), "bob"))
	if w.Code != 200 || strings.Join(ids("bob"), ",") != "alive,gone" {
		t.Fatalf("dismiss failed: %d %s %v", w.Code, w.Body, ids("bob"))
	}

	w = httptest.NewRecorder()
	handleJobAction(w, asUser(t, httptest.NewRequest("POST", "/api/jobs/clear", nil), "bob"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"cleared":1`) {
		t.Fatalf("clear should hide exactly the one remaining dead entry: %d %s", w.Code, w.Body)
	}
	if got := strings.Join(ids("bob"), ","); got != "alive" {
		t.Errorf("only usable entries should remain, got %s", got)
	}
	if got := ids("alice"); len(got) != 2 || got[0] != "alices" {
		t.Errorf("bob's clear must not touch alice's entries; admin sees %v", got)
	}

	w = httptest.NewRecorder()
	handleJobAction(w, asUser(t, httptest.NewRequest("DELETE", "/api/jobs/alices", nil), "bob"))
	if w.Code != http.StatusNotFound {
		t.Errorf("bob must not dismiss alice's entry, got %d", w.Code)
	}
	jobHistoryMu.Lock()
	n := len(jobHistory)
	jobHistoryMu.Unlock()
	if n != 5 {
		t.Errorf("clearing must only hide entries; history should keep all 5, has %d", n)
	}
}
