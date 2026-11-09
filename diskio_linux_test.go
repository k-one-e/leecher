//go:build linux

package main

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseDiskStats(t *testing.T) {
	in := `   8       0 sda 1000 0 2048 10 500 0 4096 20 0 30 30 0 0 0 0 0 0
   8       1 sda1 900 0 1024 10 400 0 2048 20 0 30 30 0 0 0 0 0 0
 253       0 dm-0 1 2 3
`
	got, err := parseDiskStats(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 devices (short line skipped), got %d", len(got))
	}
	sda1 := got[unix.Mkdev(8, 1)]
	if sda1.name != "sda1" || sda1.read != 1024*512 || sda1.written != 2048*512 {
		t.Fatalf("bad sda1 counters %+v", sda1)
	}
}

func TestReadDiskStats_FindsFolderDevice(t *testing.T) {
	_, dev, err := diskDevice(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if unix.Major(dev) == 0 {
		t.Skip("temp dir is on a virtual filesystem (tmpfs/overlay); no block device to match")
	}
	stats, err := readDiskStats()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stats[dev]; !ok {
		t.Fatalf("device %d:%d not found in /proc/diskstats", unix.Major(dev), unix.Minor(dev))
	}
}

func TestParseNetDev(t *testing.T) {
	in := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 99999     10    0    0    0     0          0         0    99999      10    0    0    0     0       0          0
  eth0: 1000      5    0    0    0     0          0         0     200       2    0    0    0     0       0          0
  wg0:  500       1    0    0    0     0          0         0      50       1    0    0    0     0       0          0
`
	rx, tx, err := parseNetDev(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if rx != 1500 || tx != 250 {
		t.Fatalf("rx=%d tx=%d, want 1500/250 (loopback excluded)", rx, tx)
	}
}
