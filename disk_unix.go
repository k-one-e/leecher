//go:build !windows

package main

import (
	"fmt"
	"syscall"
)

// diskFreeMB returns the available disk space in MB for the downloads directory.
func diskFreeMB() (int64, error) { return diskFreeMBAt(tempDir()) }

// diskFreeMBAt returns the available space in MB on the filesystem holding path.
func diskFreeMBAt(path string) (int64, error) {
	_, avail, err := diskSpaceAt(path)
	if err != nil {
		return 0, err
	}
	return int64(avail / (1024 * 1024)), nil
}

// diskSpaceAt returns the total size and the space available to this process
// (excluding root-reserved blocks) of the filesystem holding path.
func diskSpaceAt(path string) (total, avail uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	bs := uint64(stat.Bsize)
	return stat.Blocks * bs, stat.Bavail * bs, nil
}

// diskDevice identifies the filesystem holding path, so folders on the same
// disk are reported once. dev is the raw device number for I/O counters.
func diskDevice(path string) (key string, dev uint64, err error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", 0, err
	}
	dev = uint64(st.Dev)
	return fmt.Sprintf("dev:%d", dev), dev, nil
}
