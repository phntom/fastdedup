//go:build !linux

package main

import (
	"fmt"
	"os"
)

var errUnsupported = fmt.Errorf("fastdedup requires Linux (btrfs is Linux-only)")

func getExtents(_ string) ([]Extent, error) {
	return nil, errUnsupported
}

func reflinkCopy(_, _ string, _ os.FileMode) error {
	return errUnsupported
}

func reflinkInPlace(_, _ string) error {
	return errUnsupported
}

func isMountPoint(_ string) bool {
	return false
}

func fsFileEstimate(_ string) int64 {
	return 0
}

func sameInode(_, _ string) (bool, error) {
	return false, errUnsupported
}

func restoreMetadata(_ string, _ os.FileInfo) error {
	return errUnsupported
}

func fsUsedBytes(_ string) int64 {
	return 0
}

func isBtrfs(_ string) bool {
	return false
}

func runScrub(_ string) error {
	return errUnsupported
}

func runDefrag(_ string, _ int64) error {
	return errUnsupported
}
