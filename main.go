package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

func main() {
	var (
		maxSizes = flag.Int("max-sizes", 1_000_000, "maximum unique file sizes to track in pass 1")
		topN     = flag.Int("top", 10_000, "number of most impactful file sizes to dedup in pass 2")
		dryRun   = flag.Bool("dry-run", false, "report what would be deduped without making changes")
		verbose  = flag.Bool("v", false, "verbose output")
		rawSizes = flag.Bool("raw-sizes", false, "show raw byte counts instead of human-readable")
		chDir    = flag.String("C", "", "change to directory before running")
	)

	//goland:noinspection GoUnhandledErrorResult
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] [directory]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Deduplicate files on btrfs using reflinks.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	// Change directory if requested.
	if *chDir != "" {
		if err := os.Chdir(*chDir); err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot change directory to %s: %v\n", *chDir, err)
			os.Exit(1)
		}
	}

	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}

	// Quiet by default; verbose with -v.
	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	fmtSize := func(b int64) string {
		return formatSize(b, *rawSizes)
	}

	// === Pass 1: Survey file sizes ===
	fmt.Fprintf(os.Stderr, "Pass 1: Scanning file sizes...\n")
	sm := NewSizeMap(*maxSizes)
	var scanCount int64
	fileCount, err := WalkSizes(root, sm, func() {
		scanCount++
		if scanCount%10_000 == 0 {
			printCounter("  Scanned:", scanCount)
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: pass 1 failed: %v\n", err)
		os.Exit(1)
	}
	finishLine(fmt.Sprintf("  Scanned %s files, %s unique sizes",
		formatCount(fileCount), formatCount(int64(sm.Len()))))

	// Select top N most impactful sizes.
	targets := sm.TopN(*topN)
	if len(targets) == 0 {
		fmt.Fprintf(os.Stderr, "\nNo duplicate file sizes found.\n")
		return
	}

	// Display top sizes table.
	fmt.Fprintf(os.Stderr, "\nTop file sizes by dedup impact:\n")
	tableCount := min(20, len(targets))
	fmt.Fprintf(os.Stderr, "  %4s  %10s  %8s  %10s\n", "#", "Size", "Count", "Impact")
	for i := range tableCount {
		t := targets[i]
		fmt.Fprintf(os.Stderr, "  %4d  %10s  %8s  %10s\n",
			i+1, fmtSize(t.Size), formatCount(t.Count), fmtSize(t.Impact()))
	}
	if len(targets) > tableCount {
		fmt.Fprintf(os.Stderr, "  ... and %s more sizes\n", formatCount(int64(len(targets)-tableCount)))
	}

	// === Pass 2: Collect target files, then deduplicate per size ===
	fmt.Fprintf(os.Stderr, "\nPass 2: Collecting target files...\n")
	targetSet := make(map[int64]struct{}, len(targets))
	for _, t := range targets {
		targetSet[t.Size] = struct{}{}
	}

	var collectCount int64
	collected, err := CollectFiles(root, targetSet, func() {
		collectCount++
		if collectCount%10_000 == 0 {
			printCounter("  Found:", collectCount)
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: collection failed: %v\n", err)
		os.Exit(1)
	}

	// Build processing list (only sizes with >= 2 files).
	type processEntry struct {
		size  int64
		paths []string
	}
	var toProcess []processEntry
	var totalFiles int64
	for _, t := range targets {
		paths := collected[t.Size]
		if len(paths) >= 2 {
			toProcess = append(toProcess, processEntry{t.Size, paths})
			totalFiles += int64(len(paths))
		}
	}
	finishLine(fmt.Sprintf("  Collected %s files in %s size groups",
		formatCount(totalFiles), formatCount(int64(len(toProcess)))))

	if len(toProcess) == 0 {
		fmt.Fprintf(os.Stderr, "\nNo files to deduplicate.\n")
		return
	}

	// Process each size group with progress bar.
	header := "\nDeduplicating:"
	if *dryRun {
		header = "\nDeduplicating (dry run):"
	}
	fmt.Fprintf(os.Stderr, "%s\n", header)

	totalStats := &DedupStats{}
	numWidth := len(fmt.Sprintf("%d", len(toProcess)))

	for i, entry := range toProcess {
		prefix := fmt.Sprintf("  [%*d/%d] %10s \u00d7 %-8s",
			numWidth, i+1, len(toProcess),
			fmtSize(entry.size), formatCount(int64(len(entry.paths))))

		step := max(1, len(entry.paths)/200)
		stats := ProcessSizeGroup(entry.paths, entry.size, *dryRun, *rawSizes, func(current int) {
			if current%step == 0 || current == len(entry.paths) {
				printProgressBar(prefix, current, len(entry.paths), "")
			}
		})

		// Build summary for this size.
		var parts []string
		if stats.FilesDeduped > 0 {
			parts = append(parts, fmt.Sprintf("%s deduped, %s saved",
				formatCount(stats.FilesDeduped), fmtSize(stats.BytesSaved)))
		}
		if stats.AlreadyDeduped > 0 {
			parts = append(parts, fmt.Sprintf("%s already",
				formatCount(stats.AlreadyDeduped)))
		}
		if stats.Errors > 0 {
			parts = append(parts, fmt.Sprintf("%s errors",
				formatCount(stats.Errors)))
		}
		summary := strings.Join(parts, ", ")
		if summary == "" {
			summary = "no duplicates"
		}
		finishLine(fmt.Sprintf("%s  \u2713 %s", prefix, summary))

		totalStats.BytesSaved += stats.BytesSaved
		totalStats.FilesDeduped += stats.FilesDeduped
		totalStats.AlreadyDeduped += stats.AlreadyDeduped
		totalStats.Errors += stats.Errors
	}

	// Final summary.
	fmt.Fprintf(os.Stderr, "\nDone!\n")
	fmt.Fprintf(os.Stderr, "  Files deduped:    %s\n", formatCount(totalStats.FilesDeduped))
	fmt.Fprintf(os.Stderr, "  Space saved:      %s\n", fmtSize(totalStats.BytesSaved))
	fmt.Fprintf(os.Stderr, "  Already deduped:  %s\n", formatCount(totalStats.AlreadyDeduped))
	fmt.Fprintf(os.Stderr, "  Errors:           %s\n", formatCount(totalStats.Errors))
}
