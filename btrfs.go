//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const btrfsSuperMagic uint32 = 0x9123683E

// isBtrfs checks whether the filesystem at path is btrfs.
func isBtrfs(path string) bool {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return false
	}
	// Compare as uint32 — Statfs_t.Type is int32 on 32-bit architectures
	// and the magic value exceeds MaxInt32.
	return uint32(stat.Type) == btrfsSuperMagic
}

// runScrub starts a btrfs scrub and polls status until completion.
func runScrub(path string) error {
	fmt.Fprintf(os.Stderr, "\nStarting btrfs scrub on %s...\n", path)

	cmd := exec.Command("btrfs", "scrub", "start", path)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("btrfs scrub start: %w", err)
	}

	// Poll scrub status until finished.
	var statusFailures int
	for {
		time.Sleep(5 * time.Second)
		out, err := exec.Command("btrfs", "scrub", "status", "-R", path).CombinedOutput()
		status := string(out)

		done := strings.Contains(status, "finished") || strings.Contains(status, "aborted") || strings.Contains(status, "canceled")
		if err != nil && !done {
			statusFailures++
			if statusFailures > 12 { // 1 minute of consecutive failures
				return fmt.Errorf("btrfs scrub status failed %d times: %w", statusFailures, err)
			}
			continue
		}
		statusFailures = 0

		if done {
			// Display final scrub stats.
			for _, line := range strings.Split(status, "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "UUID:") && !strings.HasPrefix(line, "Scrub started:") {
					finishLine(fmt.Sprintf("  %s", line))
				}
			}
			finishLine(fmt.Sprintf("  btrfs scrub completed on %s", path))
			break
		}

		// Show progress from status output.
		for _, line := range strings.Split(status, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "data_bytes_scrubbed") || strings.Contains(line, "running") {
				printStatus(fmt.Sprintf("  Scrubbing: %s", line))
				break
			}
		}
	}
	return nil
}

// runDefrag runs btrfs filesystem defragment recursively and shows progress.
func runDefrag(path string, estimatedFiles int64) error {
	fmt.Fprintf(os.Stderr, "\nStarting btrfs defragment on %s...\n", path)

	cmd := exec.Command("btrfs", "filesystem", "defragment", "-r", "-v", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("defrag pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("btrfs defragment: %w", err)
	}

	var defragCount int64
	defragStart := time.Now()
	lastUpdate := defragStart
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		defragCount++
		if defragCount%100 == 0 {
			now := time.Now()
			if now.Sub(lastUpdate) >= 200*time.Millisecond {
				lastUpdate = now
				if estimatedFiles > 0 {
					eta := formatETA(time.Since(defragStart), defragCount, estimatedFiles)
					printProgressBar("  Defragmenting:", defragCount, estimatedFiles, eta)
				} else {
					printStatus(fmt.Sprintf("  Defragmented: %s files", formatCount(defragCount)))
				}
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("btrfs defragment: %w", err)
	}
	finishLine(fmt.Sprintf("  Defragmented %s files", formatCount(defragCount)))
	return nil
}
