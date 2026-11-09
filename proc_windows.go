//go:build windows

package main

import (
	"context"
	"os/exec"
)

// lowPriorityCmd on Windows returns a normal exec.Cmd.
func lowPriorityCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// lowerPriority is a no-op on Windows.
func lowerPriority(cmd *exec.Cmd) {}

// ensureClassicTorrentIO is a no-op on Windows (no exec-in-place); set
// TORRENT_STORAGE_DEFAULT_FILE_IO=classic in the environment instead.
func ensureClassicTorrentIO() {}
