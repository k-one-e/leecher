//go:build linux

package main

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// readDiskStats parses /proc/diskstats into cumulative byte counters keyed by
// device number. The file is not namespaced, so this works inside containers
// for host-path volumes too.
func readDiskStats() (map[uint64]diskIOCount, error) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseDiskStats(f)
}

func parseDiskStats(r io.Reader) (map[uint64]diskIOCount, error) {
	out := make(map[uint64]diskIOCount)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// major minor name reads merged sectors_read ms writes merged sectors_written ...
		fs := strings.Fields(sc.Text())
		if len(fs) < 10 {
			continue
		}
		maj, err1 := strconv.ParseUint(fs[0], 10, 32)
		min, err2 := strconv.ParseUint(fs[1], 10, 32)
		rd, err3 := strconv.ParseUint(fs[5], 10, 64)
		wr, err4 := strconv.ParseUint(fs[9], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		// diskstats always counts 512-byte sectors, whatever the hardware uses.
		out[unix.Mkdev(uint32(maj), uint32(min))] = diskIOCount{name: fs[2], read: rd * 512, written: wr * 512}
	}
	return out, sc.Err()
}

// readNetDev sums received and sent bytes over all interfaces except
// loopback. Inside a pod this is the pod's own network namespace, so it only
// counts leecher's traffic.
func readNetDev() (rx, tx uint64, err error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	return parseNetDev(f)
}

func parseNetDev(r io.Reader) (rx, tx uint64, err error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// "  eth0: rx_bytes rx_packets ... (8 rx fields) tx_bytes ..."
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		fs := strings.Fields(rest)
		if len(fs) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(fs[0], 10, 64)
		t, err2 := strconv.ParseUint(fs[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx += r
		tx += t
	}
	return rx, tx, sc.Err()
}

// dropPageCache evicts a file from the page cache so the benchmark's read
// pass measures the disk rather than RAM.
func dropPageCache(f *os.File) bool {
	return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED) == nil
}
