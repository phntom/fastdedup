//go:build linux

package main

import (
	"testing"
)

func TestIsBtrfs(t *testing.T) {
	t.Run("nonexistent path returns false", func(t *testing.T) {
		if isBtrfs("/nonexistent/path/that/does/not/exist") {
			t.Error("nonexistent path should not be btrfs")
		}
	})

	t.Run("proc is not btrfs", func(t *testing.T) {
		if isBtrfs("/proc") {
			t.Error("/proc should not be btrfs")
		}
	})

	t.Run("sys is not btrfs", func(t *testing.T) {
		if isBtrfs("/sys") {
			t.Error("/sys should not be btrfs")
		}
	})
}

func TestRunScrubFailsWithoutBtrfs(t *testing.T) {
	// /proc is never btrfs, so scrub should fail.
	err := runScrub("/proc")
	if err == nil {
		t.Error("runScrub on /proc should fail")
	}
}

func TestRunDefragFailsWithoutBtrfs(t *testing.T) {
	err := runDefrag("/proc", 0)
	if err == nil {
		t.Error("runDefrag on /proc should fail")
	}
}
