package main

// library.go — "Save to server" destination and per-user access to the
// temporary downloads folder.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// workDirName is the hidden working area inside the library folder. Every
// download lands there, so it uses the library's (big) disk; "Keep" is then a
// cheap same-filesystem rename into the library itself.
const workDirName = ".leecher"

// legacyPartialDir is the staging folder used by earlier versions.
const legacyPartialDir = ".leecher-partial"

// tempDir holds in-progress and finished-but-not-kept downloads. Without a
// library it falls back to downloads_dir (small installs, e.g. a Pi).
func tempDir() string {
	if lib := cfg().LibraryDir; lib != "" {
		return filepath.Join(lib, workDirName)
	}
	return cfg().DownloadsDir
}

// fileAreas lists every folder that may hold unkept files: the current
// working area plus downloads_dir (files from before a library was set).
func fileAreas() []string {
	t, d := tempDir(), cfg().DownloadsDir
	if filepath.Clean(t) == filepath.Clean(d) {
		return []string{t}
	}
	return []string{t, d}
}

// jobWorkDir is where a job writes. It is fixed when the job starts so a
// settings change cannot split one download across two folders.
func jobWorkDir(job *Job) string {
	if job.workDir != "" {
		return job.workDir
	}
	return tempDir()
}

// findTempFile locates an unkept download (a file, or a folder for
// multi-file torrents) by its stored name.
func findTempFile(name string) string {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return ""
	}
	for _, dir := range fileAreas() {
		p := filepath.Join(dir, name)
		if fi, err := os.Lstat(p); err == nil && (fi.Mode().IsRegular() || (fi.IsDir() && downloadEntryRe.MatchString(name))) {
			return p
		}
	}
	return ""
}

// downloadEntryRe matches finished downloads in the work area: <id>_<name>.
var downloadEntryRe = regexp.MustCompile(`^[0-9a-f]{16}_.`)

// entryInUse reports whether a work-area entry belongs to a running job or a
// seeding torrent, so cleanup must leave it alone.
func entryInUse(name string) bool {
	id := name
	if i := strings.IndexByte(name, '_'); i == 16 {
		id = name[:i]
	}
	if torrentJobBusy(id) {
		return true
	}
	jobsMu.RLock()
	j, ok := jobs[id]
	jobsMu.RUnlock()
	if !ok {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	// Paused (kill switch) torrents are out of the engine but will resume into these files.
	return j.Status == "queued" || j.Status == "downloading" || j.Status == "paused"
}

func treeSize(root string) int64 {
	var total int64
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// removeLegacyStaging deletes the old .leecher-partial folder (only
// leftovers of interrupted downloads can be in it).
func removeLegacyStaging() {
	if lib := cfg().LibraryDir; lib != "" {
		os.RemoveAll(filepath.Join(lib, legacyPartialDir))
	}
}

// validateLibraryDir checks an admin-supplied library path. It must be
// absolute, writable and outside the auto-cleaned downloads directory.
func validateLibraryDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("library_dir must be an absolute path")
	}
	dir = filepath.Clean(dir)
	dl, err := filepath.Abs(cfg().DownloadsDir)
	if err == nil {
		if rel, err := filepath.Rel(dl, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("library_dir must be outside the downloads folder, which is cleaned automatically")
		}
	}
	if err := os.MkdirAll(dir, 0775); err != nil {
		return "", fmt.Errorf("cannot create library_dir: %v", err)
	}
	probe, err := os.CreateTemp(dir, ".leecher-write-test-*")
	if err != nil {
		return "", fmt.Errorf("library_dir is not writable: %v", err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return dir, nil
}

// uniquePath appends " (n)" before the extension until the name is free.
func uniquePath(p string) string {
	if _, err := os.Lstat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 1000; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Lstat(c); os.IsNotExist(err) {
			return c
		}
	}
	return fmt.Sprintf("%s (%s)%s", base, generateID(), ext)
}

// moveToLibrary moves a finished file into the library and returns its path.
// The library usually sits on another disk, so rename falls back to copy.
func moveToLibrary(src, displayName string) (string, error) {
	dir := cfg().LibraryDir
	if dir == "" {
		return "", errors.New("no library folder configured")
	}
	if err := os.MkdirAll(dir, 0775); err != nil {
		return "", err
	}
	name := sanitizeDirectName(displayName)
	dst := uniquePath(filepath.Join(dir, name))
	err := os.Rename(src, dst)
	if err == nil {
		return dst, nil
	}
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) || !errors.Is(linkErr.Err, syscall.EXDEV) {
		return "", err
	}
	if fi, _ := os.Stat(src); fi != nil && fi.IsDir() {
		return "", errors.New("folders can only be kept on the same drive as the work area")
	}
	if err := copyFile(src, dst); err != nil {
		os.Remove(dst)
		return "", err
	}
	os.Remove(src)
	return dst, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0664)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// fileOwner finds which account produced a file in the downloads folder.
func fileOwner(name string) string {
	jobsMu.RLock()
	for _, j := range jobs {
		j.mu.Lock()
		fn, owner := j.Filename, j.Owner
		j.mu.Unlock()
		if fn == name {
			jobsMu.RUnlock()
			return owner
		}
	}
	jobsMu.RUnlock()

	jobHistoryMu.Lock()
	defer jobHistoryMu.Unlock()
	for i := len(jobHistory) - 1; i >= 0; i-- {
		if jobHistory[i].Filename == name {
			return jobHistory[i].Owner
		}
	}
	return ""
}

// canAccessFile lets admins fetch anything and users only their own files.
func canAccessFile(r *http.Request, name string) bool {
	a := currentAuth(r)
	if a == nil {
		return false
	}
	if a.User.Role == roleAdmin {
		return true
	}
	owner := fileOwner(name)
	return owner != "" && userKey(owner) == userKey(a.User.Username)
}
