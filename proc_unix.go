//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// lowPriorityCmd wraps exec.CommandContext so the child process (yt-dlp, node, ffmpeg)
// runs at reduced CPU priority on Unix systems. This is critical for SBCs where a
// yt-dlp format probe or download can otherwise starve the HTTP server.
//
// Uses Setpgid so the child gets its own process group — allows us to renice the
// entire group (yt-dlp may spawn ffmpeg/node children that inherit the group).
// After cmd.Start(), call lowerPriority(cmd) to apply the nice value.
func lowPriorityCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // new process group so children inherit priority
	}
	// PyInstaller (used by yt-dlp) extracts its bundle to TMPDIR on each run.
	// On Raspberry Pi /tmp is a small tmpfs; redirect to a persistent cache dir
	// next to the binary so extraction never fails due to tmpfs space limits.
	exePath, _ := os.Executable()
	pyiTmp := filepath.Join(filepath.Dir(exePath), ".pyi-tmp")
	os.MkdirAll(pyiTmp, 0755)
	cmd.Env = append(os.Environ(), "TMPDIR="+pyiTmp)
	return cmd
}

// lowerPriority sets the nice value of a running process to +15 (low priority).
// Call this immediately after cmd.Start(). Safe to call on any platform — no-op if
// the process is nil or already exited.
func lowerPriority(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// PRIO_PROCESS = 0 on Linux/macOS
	_ = syscall.Setpriority(0, cmd.Process.Pid, 15)
}

// torrentFileIOEnv selects how the torrent library reads files. It defaults to
// mmap, where every page fault on a slow disk (USB HDD) blocks a thread while
// it holds one of the Go scheduler's Ps; piece hashing then freezes the whole
// process — HTTP, health checks, the kill switch. "classic" uses pread, which
// releases the P while waiting.
const torrentFileIOEnv = "TORRENT_STORAGE_DEFAULT_FILE_IO"

// ensureClassicTorrentIO re-executes leecher with torrentFileIOEnv set when it
// is missing. The library reads it in its package init, before main runs, so
// setting it from Go code here would be too late.
func ensureClassicTorrentIO() {
	if _, ok := os.LookupEnv(torrentFileIOEnv); ok {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	env := append(os.Environ(), torrentFileIOEnv+"=classic")
	_ = syscall.Exec(exe, os.Args, env) // only returns on failure; then carry on with mmap
}
