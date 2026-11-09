//go:build !linux

package main

import (
	"errors"
	"os"
)

func readDiskStats() (map[uint64]diskIOCount, error) {
	return nil, errors.New("live disk I/O is only available on Linux")
}

func readNetDev() (rx, tx uint64, err error) {
	return 0, 0, errors.New("network counters are only available on Linux")
}

func dropPageCache(*os.File) bool { return false }
