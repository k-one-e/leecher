//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// diskFreeMB returns the available disk space in MB for the downloads directory.
func diskFreeMB() (int64, error) { return diskFreeMBAt(tempDir()) }

// diskFreeMBAt returns the available space in MB on the volume holding path.
func diskFreeMBAt(path string) (int64, error) {
	_, avail, err := diskSpaceAt(path)
	if err != nil {
		return 0, err
	}
	return int64(avail / (1024 * 1024)), nil
}

// diskSpaceAt returns the total size and the space available to this process
// on the volume holding path.
func diskSpaceAt(path string) (total, avail uint64, err error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx := kernel32.NewProc("GetDiskFreeSpaceExW")

	dir, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}

	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	r1, _, err := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(dir)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if r1 == 0 {
		return 0, 0, err
	}
	return totalBytes, freeBytesAvailable, nil
}

// diskDevice identifies the volume holding path. Windows has no live I/O
// counters here, so dev is always 0.
func diskDevice(path string) (key string, dev uint64, err error) {
	if _, err := os.Stat(path); err != nil {
		return "", 0, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", 0, err
	}
	return "vol:" + strings.ToUpper(filepath.VolumeName(abs)), 0, nil
}
