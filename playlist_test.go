//go:build !windows

package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMissingPlaylistItems(t *testing.T) {
	names := []string{
		"1. Mosafere Shahre Baran.jpg",
		"2. Rayhan (Azari).mp3",
		"3. Khaar.mp3",
		"9. Shirin Jaan.jpg",
		"4. Gole Yakh.mp3.part",
		"cover.jpg",
	}
	missing, labels := missingPlaylistItems(names, 5, "mp3")
	if !reflect.DeepEqual(missing, []int{1, 4, 5}) {
		t.Fatalf("missing = %v, want [1 4 5]", missing)
	}
	if labels[1] != "1. Mosafere Shahre Baran" || labels[4] != "4" || labels[5] != "5" {
		t.Fatalf("labels = %v", labels)
	}

	// Unknown playlist size: only positions that left something behind.
	missing, labels = missingPlaylistItems(names, 0, "mp3")
	if !reflect.DeepEqual(missing, []int{1, 9}) || labels[9] != "9. Shirin Jaan" {
		t.Fatalf("unknown size: missing = %v, labels = %v", missing, labels)
	}

	if m, _ := missingPlaylistItems([]string{"1. A.mp3", "2. B.MP3"}, 2, "mp3"); len(m) != 0 {
		t.Fatalf("complete album reported missing %v", m)
	}
	if got := joinInts([]int{1, 9, 12}); got != "1,9,12" {
		t.Fatalf("joinInts = %q", got)
	}
}

// fakeYtdlp writes a yt-dlp stand-in: a probe lists three tracks; a download
// writes "N. TrackN.mp3" per requested item, except that item 2 only leaves its
// cover (as YouTube's HTTP 403 does) for the first failFirst calls.
func fakeYtdlp(t *testing.T, failFirst int) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
state="` + dir + `/calls"
case "$*" in *--flat-playlist*) printf '{"id":"a"}\n{"id":"b"}\n{"id":"c"}\n'; exit 0;; esac
out=""; items=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift;;
    --playlist-items) items="$2"; shift;;
  esac
  shift
done
n=$(cat "$state" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "$state"
dir=$(dirname "$out")
[ -n "$items" ] || items="1-3"
for i in $(echo "$items" | tr ',' ' '); do
  case "$i" in *-*) i=$(seq "${i%-*}" "${i#*-}");; esac
  for k in $i; do
    if [ "$k" = 2 ] && [ "$n" -le ` + strconv.Itoa(failFirst) + ` ]; then
      printf 'cover' > "$dir/$k. Track$k.jpg"
      echo "ERROR: unable to download video data: HTTP Error 403: Forbidden"
    else
      printf 'ID3audio' > "$dir/$k. Track$k.mp3"
    fi
  done
done
`
	path := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRetryPlaylistAfterRestart(t *testing.T, formatID string) {
	t.Helper()
	withUsers(t)
	withRetryJobState(t)
	ytdlp := fakeYtdlp(t, 0)
	lib := t.TempDir()
	withConfig(t, func(c *Config) {
		c.YtdlpPath = ytdlp
		c.LibraryDir = lib
		c.CookiesFile = ""
		c.CookiesFromBrowser = ""
	})
	original := &Job{ID: "failed-playlist", Owner: "bob", Kind: kindYtdlp,
		URL: "https://music.youtube.com/playlist?list=test", FormatID: formatID,
		Title: "Original album", Destination: "library", CreatedAt: time.Now()}
	setJobError(original, "cancelled")
	jobHistory = nil
	loadJobHistory()

	w := httptest.NewRecorder()
	handleJobAction(w, asUser(t, httptest.NewRequest("POST", "/api/jobs/failed-playlist/retry", nil), "bob"))
	if w.Code != http.StatusOK {
		t.Fatalf("retry failed: %d %s", w.Code, w.Body)
	}
	var resp struct {
		ID string `json:"job_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	j := waitForRetryJob(t, resp.ID)
	if j.Status != "done" || j.FormatID != "playlist:mp3" || j.Destination != "library" || !insideLibrary(j.LibraryPath) {
		t.Fatalf("retry lost playlist format or library destination: %+v", j)
	}
	names := zipNames(t, j.LibraryPath)
	if len(names) != 3 {
		t.Fatalf("retried playlist should contain all tracks: %v", names)
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".mp3") {
			t.Fatalf("album retry produced a non-MP3 entry: %q", name)
		}
	}
}

func TestRetryPlaylist_PreservesFormatAndDestinationAfterRestart(t *testing.T) {
	testRetryPlaylistAfterRestart(t, "playlist:mp3")
}

func TestRetryPlaylist_LegacyAlbumIsMP3ZipNotVideo(t *testing.T) {
	testRetryPlaylistAfterRestart(t, "")
}

func zipNames(t *testing.T, path string) []string {
	t.Helper()
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	var names []string
	for _, f := range z.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}

func runFakePlaylist(t *testing.T, failFirst int) (*Job, []string) {
	t.Helper()
	ytdlp := fakeYtdlp(t, failFirst)
	withConfig(t, func(c *Config) { c.YtdlpPath = ytdlp; c.JsRuntimes = ""; c.CookiesFile = ""; c.CookiesFromBrowser = "" })
	orig := playlistRetryWaits
	playlistRetryWaits = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { playlistRetryWaits = orig })

	job := &Job{ID: "pltest", Status: "downloading", workDir: t.TempDir()}
	name, _, err := tryDownloadPlaylist(context.Background(), job, "https://music.youtube.com/playlist?list=x", "mp3")
	if err != nil {
		t.Fatalf("tryDownloadPlaylist: %v", err)
	}
	return job, zipNames(t, filepath.Join(job.workDir, name))
}

func TestPlaylistRetriesRefusedTrack(t *testing.T) {
	job, names := runFakePlaylist(t, 1) // track 2 refused once, then fine
	if want := []string{"1. Track1.mp3", "2. Track2.mp3", "3. Track3.mp3"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("zip = %v, want %v", names, want)
	}
	if job.Warning != "" {
		t.Fatalf("unexpected warning %q", job.Warning)
	}
}

func TestPlaylistReportsTrackThatStaysMissing(t *testing.T) {
	job, names := runFakePlaylist(t, 99) // track 2 never downloads
	if want := []string{"1. Track1.mp3", "3. Track3.mp3"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("zip = %v, want %v (no leftover cover)", names, want)
	}
	if !strings.Contains(job.Warning, "1 track(s) could not be downloaded: 2. Track2") {
		t.Fatalf("warning = %q", job.Warning)
	}
}
