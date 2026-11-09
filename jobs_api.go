package main

// jobs_api.go — the user's download list (live jobs merged with history), and
// per-download file actions: save to device, keep in the library, delete.

import (
	"archive/zip"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxJobTitleLen = 200
	maxListedJobs  = 60
	// Unusable entries (failed, or file deleted/expired) leave the activity
	// list on their own after this long.
	staleEntryAge = 24 * time.Hour
)

var (
	jobRetryMu sync.Mutex
	jobRetries = make(map[string]bool)
)

// isDeadEntry reports whether a finished entry has nothing left to act on.
func isDeadEntry(status, filename, libraryPath string) bool {
	switch status {
	case "error", "cancelled":
		return true
	case "done":
		return jobFilePath(filename, libraryPath) == ""
	}
	return false
}

// jobTitle prefers the title shown in the UI when the job was started and
// falls back to the URL.
func jobTitle(job *Job) string {
	if job.Title != "" {
		return job.Title
	}
	if job.DisplayName != "" {
		return job.DisplayName
	}
	return job.URL
}

func cleanJobTitle(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	if len(t) > maxJobTitleLen {
		t = t[:maxJobTitleLen]
		for len(t) > 0 && t[len(t)-1]&0xC0 == 0x80 {
			t = t[:len(t)-1]
		}
	}
	return t
}

// jobFilePath resolves where a finished download lives now, or "".
func jobFilePath(filename, libraryPath string) string {
	if libraryPath != "" {
		if fi, err := os.Stat(libraryPath); err == nil && (fi.Mode().IsRegular() || fi.IsDir()) && insideLibrary(libraryPath) {
			return libraryPath
		}
		return ""
	}
	return findTempFile(filename)
}

// insideLibrary guards against serving anything outside the configured
// library, e.g. if library_dir was changed after a file was kept.
func insideLibrary(p string) bool {
	lib := cfg().LibraryDir
	if lib == "" {
		return false
	}
	rel, err := filepath.Rel(lib, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !strings.HasPrefix(rel, workDirName)
}

// addFileFields describes a finished download's file for the UI.
func addFileFields(resp map[string]any, id, filename, libraryPath string, finishedAt time.Time) {
	if !finishedAt.IsZero() {
		resp["finished_at"] = finishedAt
	}
	if libraryPath != "" {
		resp["saved_to"] = "library"
		resp["library_path"] = libraryPath
	}
	if jobFilePath(filename, libraryPath) == "" {
		// Deleted by a user/admin, by the expiry cleanup, or moved away.
		resp["file_removed"] = true
		return
	}
	resp["download_url"] = "/api/files/" + id
	if libraryPath == "" && !finishedAt.IsZero() {
		if age := cfg().MaxFileAgeMinutes; age > 0 {
			resp["expires_at"] = finishedAt.Add(time.Duration(age) * time.Minute)
		}
	}
}

// jobRef is a download as far as activity actions are concerned, whether it is
// still in memory or only in the persistent history.
type jobRef struct {
	ID, Owner, Status, Filename, DisplayName, LibraryPath string
	URL, FormatID, Title, Destination                     string
	Hidden                                                bool
	FinishedAt                                            time.Time
}

func findJobRef(id string) (jobRef, bool) {
	jobsMu.RLock()
	j, ok := jobs[id]
	jobsMu.RUnlock()
	if ok {
		j.mu.Lock()
		defer j.mu.Unlock()
		return jobRef{ID: j.ID, Owner: j.Owner, Status: j.Status, Filename: j.Filename, DisplayName: j.DisplayName,
			LibraryPath: j.LibraryPath, FinishedAt: j.FinishedAt,
			URL: j.URL, FormatID: j.FormatID, Title: j.Title, Destination: j.Destination, Hidden: j.Hidden}, true
	}
	jobHistoryMu.Lock()
	defer jobHistoryMu.Unlock()
	for i := len(jobHistory) - 1; i >= 0; i-- {
		r := jobHistory[i]
		if r.ID == id && r.Status != "added" { // "added" = handed to Transmission
			return jobRef{ID: r.ID, Owner: r.Owner, Status: r.Status, Filename: r.Filename, DisplayName: r.DisplayName,
				LibraryPath: r.LibraryPath, FinishedAt: r.FinishedAt,
				URL: r.URL, FormatID: r.FormatID, Title: r.Title, Destination: r.Destination, Hidden: r.Hidden}, true
		}
	}
	return jobRef{}, false
}

func canUseJob(a *authInfo, ref jobRef) bool {
	return a != nil && (a.User.Role == roleAdmin || (ref.Owner != "" && userKey(ref.Owner) == userKey(a.User.Username)))
}

func canRetryJob(status, url string) bool {
	url = strings.ToLower(strings.TrimSpace(url))
	return (status == "error" || status == "cancelled") &&
		(strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "magnet:"))
}

// Older history has no format selection. Recover audio-playlist mode from
// unambiguous playlist URLs rather than silently downloading one video.
func retryFormatID(formatID, rawURL string) string {
	if formatID != "" || !isPlaylistZipHost(rawURL) {
		return formatID
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return formatID
	}
	if (strings.TrimSuffix(u.Path, "/") == "/playlist" && u.Query().Get("list") != "") ||
		strings.Contains(u.Path, "/sets/") {
		return "playlist:mp3"
	}
	return formatID
}

// updateJobFile records where a download's file now lives on the live job
// and in the history, so it survives restarts.
func updateJobFile(id string, filename, libraryPath string) {
	jobsMu.RLock()
	j, ok := jobs[id]
	jobsMu.RUnlock()
	if ok {
		j.mu.Lock()
		j.Filename, j.LibraryPath = filename, libraryPath
		if libraryPath != "" {
			j.Destination = "library"
		}
		j.mu.Unlock()
	}
	jobHistoryMu.Lock()
	changed := false
	for i := range jobHistory {
		if jobHistory[i].ID == id {
			jobHistory[i].Filename, jobHistory[i].LibraryPath = filename, libraryPath
			if libraryPath != "" {
				jobHistory[i].Destination = "library"
			}
			changed = true
		}
	}
	if changed {
		persistJobHistoryLocked() // unlocks
	} else {
		jobHistoryMu.Unlock()
	}
	notifyFileChange()
}

// keepJobFile moves an unkept download into the library.
func keepJobFile(ref jobRef) (string, error) {
	if cfg().LibraryDir == "" {
		return "", errors.New("No library folder is configured.")
	}
	if ref.LibraryPath != "" {
		return ref.LibraryPath, nil
	}
	stopTorrentSeeding(ref.ID)
	src := findTempFile(ref.Filename)
	if src == "" {
		return "", errors.New("The file is no longer available.")
	}
	name := ref.DisplayName
	if name == "" {
		name = strings.TrimPrefix(ref.Filename, ref.ID+"_")
	}
	dst, err := moveToLibrary(src, name)
	if err != nil {
		return "", err
	}
	updateJobFile(ref.ID, "", dst)
	return dst, nil
}

func handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	a := currentAuth(r)
	if a == nil {
		jsonError(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	isAdmin := a.User.Role == roleAdmin
	me := userKey(a.User.Username)
	mine := func(owner string) bool { return isAdmin || userKey(owner) == me }

	type view struct {
		resp    map[string]any
		created time.Time
	}
	var views []view
	seen := map[string]bool{}

	jobsMu.RLock()
	live := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		live = append(live, j)
	}
	jobsMu.RUnlock()
	for _, j := range live {
		j.mu.Lock()
		if !mine(j.Owner) || j.Hidden {
			if j.Hidden {
				seen[j.ID] = true
			}
			j.mu.Unlock()
			continue
		}
		resp := buildJobStatusResponse(j, cfg().DebugMode)
		resp["id"] = j.ID
		resp["title"] = jobTitle(j)
		resp["url"] = j.URL
		resp["kind"] = j.Kind
		resp["created_at"] = j.CreatedAt
		resp["can_retry"] = canRetryJob(j.Status, j.URL)
		if isAdmin {
			resp["owner"] = j.Owner
		}
		seen[j.ID] = true
		views = append(views, view{resp, j.CreatedAt})
		j.mu.Unlock()
	}

	jobHistoryMu.Lock()
	hist := append([]JobRecord(nil), jobHistory...)
	jobHistoryMu.Unlock()
	for i := len(hist) - 1; i >= 0; i-- {
		h := hist[i]
		if seen[h.ID] || !mine(h.Owner) || (h.Status != "done" && h.Status != "error") {
			continue
		}
		seen[h.ID] = true
		if h.Hidden || (time.Since(h.FinishedAt) > staleEntryAge && isDeadEntry(h.Status, h.Filename, h.LibraryPath)) {
			continue
		}
		title := h.Title
		if title == "" {
			title = firstNonEmpty(h.DisplayName, h.URL)
		}
		resp := map[string]any{
			"id": h.ID, "status": h.Status, "title": title, "display_name": h.DisplayName, "url": h.URL,
			"kind": h.Kind, "created_at": h.CreatedAt, "progress": 100,
			"can_retry": canRetryJob(h.Status, h.URL),
		}
		if h.Status == "error" {
			resp["error"] = h.Error
		} else {
			if h.Warning != "" {
				resp["warning"] = h.Warning
			}
			addFileFields(resp, h.ID, h.Filename, h.LibraryPath, h.FinishedAt)
		}
		if isAdmin {
			resp["owner"] = h.Owner
		}
		views = append(views, view{resp, h.CreatedAt})
	}

	sort.Slice(views, func(i, k int) bool { return views[i].created.After(views[k].created) })
	if len(views) > maxListedJobs {
		views = views[:maxListedJobs]
	}
	out := make([]map[string]any, 0, len(views))
	for _, v := range views {
		out = append(out, v.resp)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out, "all": isAdmin, "library": cfg().LibraryDir != ""})
}

// hideJobs marks entries as dismissed on the live jobs and in the history.
func hideJobs(ids map[string]bool) {
	if len(ids) == 0 {
		return
	}
	jobsMu.RLock()
	for id := range ids {
		if j, ok := jobs[id]; ok {
			j.mu.Lock()
			j.Hidden = true
			j.mu.Unlock()
		}
	}
	jobsMu.RUnlock()
	jobHistoryMu.Lock()
	for i := range jobHistory {
		if ids[jobHistory[i].ID] {
			jobHistory[i].Hidden = true
		}
	}
	persistJobHistoryLocked() // unlocks
}

// clearDeadJobs dismisses every unusable entry the caller can see.
func clearDeadJobs(a *authInfo) int {
	ids := map[string]bool{}
	visible := func(owner string) bool {
		return a.User.Role == roleAdmin || userKey(owner) == userKey(a.User.Username)
	}
	jobsMu.RLock()
	for _, j := range jobs {
		j.mu.Lock()
		if visible(j.Owner) && !j.Hidden && isDeadEntry(j.Status, j.Filename, j.LibraryPath) {
			ids[j.ID] = true
		}
		j.mu.Unlock()
	}
	jobsMu.RUnlock()
	jobHistoryMu.Lock()
	for _, h := range jobHistory {
		// Entries past staleEntryAge are already off the list; don't count them.
		if h.Status != "added" && !h.Hidden && visible(h.Owner) && time.Since(h.FinishedAt) <= staleEntryAge &&
			isDeadEntry(h.Status, h.Filename, h.LibraryPath) {
			ids[h.ID] = true
		}
	}
	jobHistoryMu.Unlock()
	hideJobs(ids)
	return len(ids)
}

// handleJobAction: POST /api/jobs/{id}/keep or /retry, DELETE /api/jobs/{id}/file,
// DELETE /api/jobs/{id} (dismiss from the list), POST /api/jobs/clear.
func handleJobAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/jobs/"), "/"), "/")
	a := currentAuth(r)
	if a == nil {
		jsonError(w, "Authentication required", http.StatusUnauthorized)
		return
	}
	if len(parts) == 1 && parts[0] == "clear" && r.Method == http.MethodPost {
		n := clearDeadJobs(a)
		writeJSON(w, http.StatusOK, map[string]int{"cleared": n})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		ref, ok := findJobRef(parts[0])
		if !ok || !canUseJob(a, ref) {
			jsonError(w, "Download not found", http.StatusNotFound)
			return
		}
		if ref.Status == "queued" || ref.Status == "downloading" || ref.Status == "paused" {
			jsonError(w, "Stop the download first.", http.StatusConflict)
			return
		}
		hideJobs(map[string]bool{ref.ID: true})
		writeJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
		return
	}
	if len(parts) != 2 {
		jsonError(w, "Not found", http.StatusNotFound)
		return
	}
	ref, ok := findJobRef(parts[0])
	if !ok || !canUseJob(a, ref) {
		jsonError(w, "Download not found", http.StatusNotFound)
		return
	}
	if parts[1] == "retry" {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		jobRetryMu.Lock()
		ref, ok = findJobRef(parts[0])
		if !ok || !canUseJob(a, ref) {
			jobRetryMu.Unlock()
			jsonError(w, "Download not found", http.StatusNotFound)
			return
		}
		if jobRetries[ref.ID] || ref.Hidden {
			jobRetryMu.Unlock()
			jsonError(w, "This download has already been retried or dismissed.", http.StatusConflict)
			return
		}
		if !canRetryJob(ref.Status, ref.URL) {
			jobRetryMu.Unlock()
			jsonError(w, "Only failed or stopped downloads with a source link can be retried.", http.StatusConflict)
			return
		}
		jobRetries[ref.ID] = true
		jobRetryMu.Unlock()
		defer func() {
			jobRetryMu.Lock()
			delete(jobRetries, ref.ID)
			jobRetryMu.Unlock()
		}()
		if startDownload(w, r, downloadRequest{
			URL: ref.URL, FormatID: retryFormatID(ref.FormatID, ref.URL), Title: ref.Title, Destination: ref.Destination,
		}, ref.Owner) {
			hideJobs(map[string]bool{ref.ID: true})
		}
		return
	}
	if (parts[1] == "pause" || parts[1] == "resume") && r.Method == http.MethodPost {
		if !limiter.allow(requestActor(r)+":file-action", 30.0/60.0, 30) {
			jsonError(w, "Too many requests. Please slow down.", http.StatusTooManyRequests)
			return
		}
		var err error
		if parts[1] == "pause" {
			err = pauseTorrent(ref.ID)
		} else {
			err = resumePausedTorrent(ref.ID)
		}
		switch {
		case errors.Is(err, errKillSwitch):
			jsonError(w, err.Error()+" It will resume automatically once the IP is allowed again.", http.StatusServiceUnavailable)
		case err != nil:
			jsonError(w, err.Error(), http.StatusConflict)
		default:
			logger.Info("Torrent "+parts[1]+"d by user", "user", a.User.Username, "job", ref.ID)
			writeJSON(w, http.StatusOK, map[string]string{"status": parts[1] + "d"})
		}
		return
	}
	if ref.Status != "done" {
		jsonError(w, "The download has not finished.", http.StatusConflict)
		return
	}
	if !limiter.allow(requestActor(r)+":file-action", 30.0/60.0, 30) {
		jsonError(w, "Too many requests. Please slow down.", http.StatusTooManyRequests)
		return
	}

	switch {
	case parts[1] == "keep" && r.Method == http.MethodPost:
		dst, err := keepJobFile(ref)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		logger.Info("Download kept", "user", a.User.Username, "job", ref.ID, "path", dst)
		writeJSON(w, http.StatusOK, map[string]string{"status": "kept", "library_path": dst})

	case parts[1] == "file" && r.Method == http.MethodDelete:
		stopTorrentSeeding(ref.ID)
		path := jobFilePath(ref.Filename, ref.LibraryPath)
		if path == "" {
			jsonError(w, "The file is already gone.", http.StatusNotFound)
			return
		}
		if err := os.RemoveAll(path); err != nil {
			jsonError(w, "Could not delete the file: "+err.Error(), http.StatusInternalServerError)
			return
		}
		notifyFileChange()
		logger.Info("Download file deleted", "user", a.User.Username, "job", ref.ID, "path", path)
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleJobFile streams a finished download (kept or not) to its owner.
// http.ServeFile handles Range requests, so big files can resume.
func handleJobFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	ref, ok := findJobRef(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/files/"), "/"))
	if !ok || !canUseJob(currentAuth(r), ref) {
		http.NotFound(w, r)
		return
	}
	path := jobFilePath(ref.Filename, ref.LibraryPath)
	if path == "" {
		http.Error(w, "This file is no longer available.", http.StatusGone)
		return
	}
	name := filepath.Base(path)
	if ref.LibraryPath == "" {
		name = firstNonEmpty(ref.DisplayName, strings.TrimPrefix(name, ref.ID+"_"))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	clearWriteDeadline(w) // big files outlive the server's write timeout
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		// Multi-file torrent: stream the folder as an uncompressed ZIP.
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
		if err := streamZip(w, path, name); err != nil {
			logger.Warn("Streaming folder failed", "job", ref.ID, "error", err)
		}
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeFile(w, r, path)
}

// streamZip writes root's files as a stored (uncompressed) ZIP; media is
// already compressed, so deflate would only burn CPU.
func streamZip(w io.Writer, root, top string) error {
	zw := zip.NewWriter(w)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(filepath.Join(top, rel))
		hdr.Method = zip.Store
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(fw, f)
		f.Close()
		return err
	})
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	return err
}
