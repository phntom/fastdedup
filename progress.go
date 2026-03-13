package main

import (
	"fmt"
	"os"
	"strings"
)

var (
	isTTY = func() bool {
		fi, err := os.Stderr.Stat()
		if err != nil {
			return false
		}
		return fi.Mode()&os.ModeCharDevice != 0
	}()
	quietMode bool
)

const barWidth = 30

// formatSize formats a byte count as a human-readable string.
// If raw is true, returns the raw byte count.
func formatSize(b int64, raw bool) string {
	if raw {
		return fmt.Sprintf("%d", b)
	}
	const (
		kiB = 1024
		miB = 1024 * kiB
		giB = 1024 * miB
		tiB = 1024 * giB
	)
	switch {
	case b >= tiB:
		return fmt.Sprintf("%.1f TiB", float64(b)/float64(tiB))
	case b >= giB:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(giB))
	case b >= miB:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(miB))
	case b >= kiB:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(kiB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// formatCount formats an integer with comma separators.
func formatCount(n int64) string {
	if n < 0 {
		return "-" + formatCount(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// printProgressBar renders a progress bar on stderr, overwriting the current line.
func printProgressBar(prefix string, current, total int, suffix string) {
	if quietMode || !isTTY || total <= 0 {
		return
	}
	pct := current * 100 / total
	filled := barWidth * current / total
	if filled > barWidth {
		filled = barWidth
	}
	bar := strings.Repeat("\u2588", filled) + strings.Repeat("\u2591", barWidth-filled)
	fmt.Fprintf(os.Stderr, "\r\033[K%s [%s] %3d%% %s", prefix, bar, pct, suffix)
}

// printCounter renders a counter on stderr, overwriting the current line.
func printCounter(prefix string, count int64) {
	if quietMode || !isTTY {
		return
	}
	fmt.Fprintf(os.Stderr, "\r\033[K%s %s", prefix, formatCount(count))
}

// finishLine completes the current line and moves to the next.
func finishLine(text string) {
	if quietMode {
		return
	}
	if isTTY {
		fmt.Fprintf(os.Stderr, "\r\033[K%s\n", text)
	} else {
		fmt.Fprintf(os.Stderr, "%s\n", text)
	}
}
