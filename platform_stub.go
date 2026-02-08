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

func sameInode(_, _ string) (bool, error) {
	return false, errUnsupported
}

func restoreMetadata(_ string, _ os.FileInfo) error {
	return errUnsupported
}
