package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	var (
		maxSizes     = flag.Int("max-sizes", 1_000_000, "maximum unique file sizes to track in pass 1")
		topN         = flag.Int("top", 10_000, "number of most impactful file sizes to dedup in pass 2")
		minSize      = flag.Int64("min-size", 524288, "minimum file size to process in bytes")
		dryRun       = flag.Bool("dry-run", false, "report what would be deduped without making changes")
		verbose      = flag.Bool("v", false, "show file paths of deduped files and detailed diagnostics")
		quiet        = flag.Bool("q", false, "quiet mode — only print final summary (for cronjobs)")
		batch        = flag.Bool("batch", false, "collect all target files in one pass (faster, uses more memory)")
		lowMemory    = flag.Bool("low-memory", false, "scan separately for each file size (lowest memory, slower)")
		noCache      = flag.Bool("no-cache", false, "ignore saved state — reprocess all file sizes even if unchanged since last run")
		hardlink     = flag.Bool("hardlink", false, "use hard links instead of reflinks (works on any filesystem, but linked files share all changes)")
		rawSizes     = flag.Bool("raw-sizes", false, "show raw byte counts instead of human-readable")
		snapshots    = flag.Bool("snapshots", false, "include .snapshots directories (skipped by default)")
	)

	//goland:noinspection GoUnhandledErrorResult
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] [directory]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Deduplicate files on btrfs using reflinks.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}

	// Resolve to absolute path for display and cache keying.
	if absRoot, err := filepath.Abs(root); err == nil {
		root = absRoot
	}

	// Set log level and quiet mode.
	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	if *quiet {
		level = slog.LevelError
		quietMode = true
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if *hardlink && !*dryRun {
		fmt.Fprintf(os.Stderr, "WARNING: --hardlink mode creates hard links instead of reflinks.\n")
		fmt.Fprintf(os.Stderr, "  Hard-linked files share the same inode — editing one file changes ALL copies.\n")
		fmt.Fprintf(os.Stderr, "  Metadata (permissions, timestamps) is also shared. Use with caution.\n\n")
	}

	fmtSize := func(b int64) string {
		return formatSize(b, *rawSizes)
	}

	// Load dedup cache.
	var cacheFile string
	var cached map[int64]uint64
	if !*noCache {
		if cf, err := cachePath(root); err == nil {
			cacheFile = cf
			cached = loadCache(cf)
		}
	}

	// === Pass 1: Survey file sizes ===
	if !*quiet {
		fmt.Fprintf(os.Stderr, "Pass 1: Scanning file sizes in %s\n", root)
	}
	sm := NewSizeMap(*maxSizes)
	var filenameHashes map[int64]uint64
	if cacheFile != "" {
		filenameHashes = make(map[int64]uint64)
	}
	var scanCount int64
	fileCount, err := WalkSizes(root, sm, *snapshots, *minSize, func(path string, size int64) {
		if filenameHashes != nil {
			filenameHashes[size] += hashFilename(filepath.Base(path))
		}
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
		if !*quiet {
			fmt.Fprintf(os.Stderr, "\nNo duplicate file sizes found.\n")
		}
		return
	}

	// Display top sizes table.
	if !*quiet {
		fmt.Fprintf(os.Stderr, "\nTop file sizes by potential savings:\n")
		tableCount := min(20, len(targets))
		fmt.Fprintf(os.Stderr, "  %4s  %10s  %8s  %10s\n", "#", "Size", "Count", "Savings")
		for i := range tableCount {
			t := targets[i]
			fmt.Fprintf(os.Stderr, "  %4d  %10s  %8s  %10s\n",
				i+1, fmtSize(t.Size), formatCount(t.Count), fmtSize(t.Savings()))
		}
		if len(targets) > tableCount {
			fmt.Fprintf(os.Stderr, "  ... and %s more sizes\n", formatCount(int64(len(targets)-tableCount)))
		}
	}

	// Filter out size groups unchanged since last run.
	originalTargets := targets
	if cached != nil {
		var active []SizeEntry
		var skipped int64
		for _, t := range targets {
			if h, ok := cached[t.Size]; ok && h == filenameHashes[t.Size] {
				skipped++
			} else {
				active = append(active, t)
			}
		}
		if skipped > 0 && !*quiet {
			fmt.Fprintf(os.Stderr, "  Skipped %s unchanged size groups (cached)\n", formatCount(skipped))
		}
		targets = active
	}

	// === Pass 2: Deduplicate ===
	totalStats := &DedupStats{}

	// processGroup deduplicates one size group and accumulates stats.
	processGroup := func(idx, total int, size int64, paths []string) {
		numWidth := len(fmt.Sprintf("%d", total))
		prefix := fmt.Sprintf("  [%*d/%d] %10s \u00d7 %-8s",
			numWidth, idx+1, total,
			fmtSize(size), formatCount(int64(len(paths))))

		step := max(1, len(paths)/200)
		stats := ProcessSizeGroup(paths, size, *dryRun, *verbose, *rawSizes, *hardlink, func(current int) {
			if current%step == 0 || current == len(paths) {
				printProgressBar(prefix, current, len(paths), "")
			}
		})

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

	if *batch {
		// Batch mode: collect all target files in a single pass, then deduplicate.
		if !*quiet {
			fmt.Fprintf(os.Stderr, "\nPass 2: Collecting target files...\n")
		}
		targetSet := make(map[int64]struct{}, len(targets))
		for _, t := range targets {
			targetSet[t.Size] = struct{}{}
		}

		var collectCount int64
		collected, err := CollectFiles(root, targetSet, *snapshots, *minSize, func() {
			collectCount++
			if collectCount%10_000 == 0 {
				printCounter("  Found:", collectCount)
			}
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: collection failed: %v\n", err)
			os.Exit(1)
		}

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
			if !*quiet {
				fmt.Fprintf(os.Stderr, "\nNo files to deduplicate.\n")
			}
			return
		}

		if !*quiet {
			header := "\nDeduplicating:"
			if *dryRun {
				header = "\nDeduplicating (dry run):"
			}
			fmt.Fprintf(os.Stderr, "%s\n", header)
		}

		for i, entry := range toProcess {
			processGroup(i, len(toProcess), entry.size, entry.paths)
		}
	} else if *lowMemory {
		// Low-memory mode: scan for each file size separately.
		if !*quiet {
			header := "\nPass 2: Deduplicating:"
			if *dryRun {
				header = "\nPass 2: Deduplicating (dry run):"
			}
			fmt.Fprintf(os.Stderr, "%s\n", header)
		}

		for i, t := range targets {
			singleSet := map[int64]struct{}{t.Size: {}}
			collected, err := CollectFiles(root, singleSet, *snapshots, *minSize, nil)
			if err != nil {
				slog.Debug("collection failed", "size", t.Size, "error", err)
				continue
			}
			paths := collected[t.Size]
			if len(paths) < 2 {
				continue
			}
			processGroup(i, len(targets), t.Size, paths)
		}
	} else {
		// Default: bounded collection with automatic eviction for large groups.
		if !*quiet {
			fmt.Fprintf(os.Stderr, "\nPass 2: Collecting target files...\n")
		}
		targetSet := make(map[int64]struct{}, len(targets))
		for _, t := range targets {
			targetSet[t.Size] = struct{}{}
		}

		type cachedGroup struct {
			paths   []string
			memUsed int64
		}
		cache := make(map[int64]*cachedGroup)
		evicted := make(map[int64]bool)
		var totalMem int64
		const memBudget int64 = 256 * 1024 * 1024 // 256 MiB for path cache

		var collectCount int64
		_ = walkRandom(root, *snapshots, *minSize, func(path string, size int64) {
			if _, ok := targetSet[size]; !ok {
				return
			}
			if evicted[size] {
				return
			}

			collectCount++
			if collectCount%10_000 == 0 {
				printCounter("  Found:", collectCount)
			}

			pathMem := int64(len(path)) + 16 // string content + header
			g, ok := cache[size]
			if !ok {
				g = &cachedGroup{}
				cache[size] = g
			}
			g.paths = append(g.paths, path)
			g.memUsed += pathMem
			totalMem += pathMem

			// Evict the most memory-costly group when over budget.
			for totalMem > memBudget {
				var maxSize int64
				var maxMem int64
				for sz, grp := range cache {
					if grp.memUsed > maxMem {
						maxSize = sz
						maxMem = grp.memUsed
					}
				}
				if maxMem == 0 {
					break
				}
				evicted[maxSize] = true
				totalMem -= cache[maxSize].memUsed
				delete(cache, maxSize)
				slog.Debug("evicted size group from cache", "size", maxSize, "freed", maxMem)
			}
		})

		if len(evicted) > 0 {
			finishLine(fmt.Sprintf("  Collected %s files, %s groups deferred to rescan",
				formatCount(collectCount), formatCount(int64(len(evicted)))))
		} else {
			finishLine(fmt.Sprintf("  Collected %s files", formatCount(collectCount)))
		}

		if !*quiet {
			header := "\nDeduplicating:"
			if *dryRun {
				header = "\nDeduplicating (dry run):"
			}
			fmt.Fprintf(os.Stderr, "%s\n", header)
		}

		for i, t := range targets {
			var paths []string
			if g, ok := cache[t.Size]; ok {
				paths = g.paths
				delete(cache, t.Size) // free memory as we go
			} else {
				// Evicted or not found — do a per-size walk.
				singleSet := map[int64]struct{}{t.Size: {}}
				c, err := CollectFiles(root, singleSet, *snapshots, *minSize, nil)
				if err != nil {
					slog.Debug("collection failed", "size", t.Size, "error", err)
					continue
				}
				paths = c[t.Size]
			}
			if len(paths) < 2 {
				continue
			}
			processGroup(i, len(targets), t.Size, paths)
		}
	}

	// Update dedup cache (skip on dry-run and when errors occurred).
	if cacheFile != "" && !*dryRun && totalStats.Errors == 0 {
		for _, t := range originalTargets {
			cached[t.Size] = filenameHashes[t.Size]
		}
		if err := saveCache(cacheFile, cached); err != nil {
			slog.Debug("failed to save cache", "error", err)
		}
	}

	// Final summary.
	if *quiet {
		if totalStats.FilesDeduped > 0 || totalStats.Errors > 0 {
			fmt.Fprintf(os.Stderr, "fastdedup: %s deduped, %s saved, %s already, %s errors\n",
				formatCount(totalStats.FilesDeduped), fmtSize(totalStats.BytesSaved),
				formatCount(totalStats.AlreadyDeduped), formatCount(totalStats.Errors))
		}
	} else {
		fmt.Fprintf(os.Stderr, "\nDone!\n")
		fmt.Fprintf(os.Stderr, "  Files deduped:    %s\n", formatCount(totalStats.FilesDeduped))
		fmt.Fprintf(os.Stderr, "  Space saved:      %s\n", fmtSize(totalStats.BytesSaved))
		fmt.Fprintf(os.Stderr, "  Already deduped:  %s\n", formatCount(totalStats.AlreadyDeduped))
		fmt.Fprintf(os.Stderr, "  Errors:           %s\n", formatCount(totalStats.Errors))
	}
}
