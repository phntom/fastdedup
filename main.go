package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var version = "dev"

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
		memBudgetMB  = flag.Int64("mem-budget", 256, "memory budget in MiB for path cache in default mode")
		noCache      = flag.Bool("no-cache", false, "ignore saved state — reprocess all file sizes even if unchanged since last run")
		hardlink     = flag.Bool("hardlink", false, "use hard links instead of reflinks (works on any filesystem, but linked files share all changes)")
		fixPerms     = flag.Bool("fix-perms", false, "temporarily add write permission to read-only directories during dedup, then restore")
		rawSizes     = flag.Bool("raw-sizes", false, "show raw byte counts instead of human-readable")
		snapshots    = flag.Bool("snapshots", false, "include .snapshots directories (skipped by default)")
		showVersion  = flag.Bool("version", false, "print version and exit")
	)

	//goland:noinspection GoUnhandledErrorResult
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] [directory]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Deduplicate files using reflinks (btrfs, XFS, ZFS).\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("fastdedup %s\n", version)
		return
	}

	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}

	// Resolve to canonical absolute path for display and cache keying.
	if absRoot, err := filepath.Abs(root); err == nil {
		root = absRoot
	}
	if canonical, err := filepath.EvalSymlinks(root); err == nil {
		root = canonical
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

	startTime := time.Now()

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
	scanStart := time.Now()
	fileCount, err := WalkSizes(root, sm, *snapshots, *minSize, func(path string, size int64) {
		if filenameHashes != nil {
			filenameHashes[size] += hashFilename(filepath.Base(path))
		}
		scanCount++
		if scanCount%10_000 == 0 {
			rate := int64(float64(scanCount) / time.Since(scanStart).Seconds())
			printStatus(fmt.Sprintf("  Scanned: %s (%s/s)", formatCount(scanCount), formatCount(rate)))
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
		var totalTargetFiles int64
		var totalTargetSavings int64
		for _, t := range targets {
			totalTargetFiles += t.Count
			totalTargetSavings += t.Savings()
		}
		fmt.Fprintf(os.Stderr, "  %4s  %10s  %8s  %10s\n",
			"", "", formatCount(totalTargetFiles), fmtSize(totalTargetSavings))
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
	errorSizes := make(map[int64]bool) // track which size groups had errors
	dirPool := NewDirIntern()          // shared directory string interner for compact paths

	// Compute expected totals from pass 1 for overall progress.
	var expectedFiles int64
	var expectedSavings int64
	for _, t := range targets {
		expectedFiles += t.Count
		expectedSavings += t.Savings()
	}

	var filesProcessed int64 // cumulative files across all groups
	var noDupGroups int64    // groups where no action was taken

	// processGroup deduplicates one size group and accumulates stats.
	processGroup := func(idx, total int, size int64, paths []string) {
		numWidth := len(fmt.Sprintf("%d", total))
		prefix := fmt.Sprintf("  [%*d/%d] %10s \u00d7 %-8s",
			numWidth, idx+1, total,
			fmtSize(size), formatCount(int64(len(paths))))

		step := max(1, len(paths)/200)
		groupBase := filesProcessed
		stats := ProcessSizeGroup(paths, size, *dryRun, *verbose, *rawSizes, *hardlink, *fixPerms, func(current int) {
			if current%step == 0 || current == len(paths) {
				overallPct := int((groupBase + int64(current)) * 100 / expectedFiles)
				suffix := fmt.Sprintf("(%d%%)", overallPct)
				printProgressBar(prefix, current, len(paths), suffix)
			}
		})
		filesProcessed += int64(len(paths))

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
		if len(parts) == 0 {
			noDupGroups++
			// Clear progress bar but don't print a line for no-action groups.
			printStatus("")
		} else {
			finishLine(fmt.Sprintf("%s  \u2713 %s", prefix, strings.Join(parts, ", ")))
		}

		totalStats.BytesSaved += stats.BytesSaved
		totalStats.FilesDeduped += stats.FilesDeduped
		totalStats.AlreadyDeduped += stats.AlreadyDeduped
		totalStats.Errors += stats.Errors
		if stats.Errors > 0 {
			errorSizes[size] = true
		}

		// Incrementally save cache after each completed group so Ctrl+C doesn't lose progress.
		if cacheFile != "" && !*dryRun && !errorSizes[size] {
			cached[size] = filenameHashes[size]
			if err := saveCache(cacheFile, cached); err != nil {
				slog.Debug("failed to save cache", "error", err)
			}
		}
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
		collected, err := CollectFiles(root, targetSet, *snapshots, *minSize, dirPool, func() {
			collectCount++
			if collectCount%10_000 == 0 {
				printProgressBar("  Collecting:", int(collectCount), int(expectedFiles), "")
			}
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: collection failed: %v\n", err)
			os.Exit(1)
		}

		type processEntry struct {
			size  int64
			paths []CompactPath
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
			dryLabel := ""
			if *dryRun {
				dryLabel = " (dry run)"
			}
			fmt.Fprintf(os.Stderr, "\nDeduplicating%s: %s groups, %s files, up to %s potential savings\n",
				dryLabel, formatCount(int64(len(toProcess))), formatCount(expectedFiles), fmtSize(expectedSavings))
		}

		for i, entry := range toProcess {
			processGroup(i, len(toProcess), entry.size, ExpandPaths(entry.paths))
		}
	} else if *lowMemory {
		// Low-memory mode: scan for each file size separately.
		if !*quiet {
			dryLabel := ""
			if *dryRun {
				dryLabel = " (dry run)"
			}
			fmt.Fprintf(os.Stderr, "\nPass 2: Deduplicating%s: %s groups, %s files, up to %s potential savings\n",
				dryLabel, formatCount(int64(len(targets))), formatCount(expectedFiles), fmtSize(expectedSavings))
		}

		for i, t := range targets {
			singleSet := map[int64]struct{}{t.Size: {}}
			collected, err := CollectFiles(root, singleSet, *snapshots, *minSize, dirPool, nil)
			if err != nil {
				slog.Debug("collection failed", "size", t.Size, "error", err)
				continue
			}
			paths := collected[t.Size]
			if len(paths) < 2 {
				continue
			}
			processGroup(i, len(targets), t.Size, ExpandPaths(paths))
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
			paths   []CompactPath
			memUsed int64
		}
		cache := make(map[int64]*cachedGroup)
		evicted := make(map[int64]bool)
		var totalMem int64
		memBudget := *memBudgetMB * 1024 * 1024

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
				printProgressBar("  Collecting:", int(collectCount), int(expectedFiles), "")
			}

			dir, name := filepath.Dir(path), filepath.Base(path)
			iDir, dirCost := dirPool.Intern(dir)
			cp := CompactPath{Dir: iDir, Name: name}
			pathMem := cp.MemCost() // per-entry cost (shared Dir excluded)
			totalMem += dirCost     // count new directory strings against budget

			g, ok := cache[size]
			if !ok {
				g = &cachedGroup{}
				cache[size] = g
			}
			g.paths = append(g.paths, cp)
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
			dryLabel := ""
			if *dryRun {
				dryLabel = " (dry run)"
			}
			fmt.Fprintf(os.Stderr, "\nDeduplicating%s: %s groups, %s files, up to %s potential savings\n",
				dryLabel, formatCount(int64(len(targets))), formatCount(expectedFiles), fmtSize(expectedSavings))
		}

		for i, t := range targets {
			var compact []CompactPath
			if g, ok := cache[t.Size]; ok {
				compact = g.paths
				delete(cache, t.Size) // free memory as we go
			} else {
				// Evicted or not found — do a per-size walk.
				singleSet := map[int64]struct{}{t.Size: {}}
				c, err := CollectFiles(root, singleSet, *snapshots, *minSize, dirPool, nil)
				if err != nil {
					slog.Debug("collection failed", "size", t.Size, "error", err)
					continue
				}
				compact = c[t.Size]
			}
			if len(compact) < 2 {
				continue
			}
			processGroup(i, len(targets), t.Size, ExpandPaths(compact))
		}
	}

	// Update dedup cache (skip on dry-run; exclude size groups that had errors).
	if cacheFile != "" && !*dryRun {
		for _, t := range originalTargets {
			if !errorSizes[t.Size] {
				cached[t.Size] = filenameHashes[t.Size]
			}
		}
		if err := saveCache(cacheFile, cached); err != nil {
			slog.Debug("failed to save cache", "error", err)
		}
	}

	// Final summary.
	elapsed := time.Since(startTime).Truncate(time.Millisecond)
	if *quiet {
		if totalStats.FilesDeduped > 0 || totalStats.Errors > 0 {
			fmt.Fprintf(os.Stderr, "fastdedup: %s: %s deduped, %s saved, %s already, %s errors (%s)\n",
				root,
				formatCount(totalStats.FilesDeduped), fmtSize(totalStats.BytesSaved),
				formatCount(totalStats.AlreadyDeduped), formatCount(totalStats.Errors),
				elapsed)
		}
	} else {
		fmt.Fprintf(os.Stderr, "\nDone in %s!\n", elapsed)
		fmt.Fprintf(os.Stderr, "  Files deduped:    %s\n", formatCount(totalStats.FilesDeduped))
		fmt.Fprintf(os.Stderr, "  Space saved:      %s\n", fmtSize(totalStats.BytesSaved))
		fmt.Fprintf(os.Stderr, "  Already deduped:  %s\n", formatCount(totalStats.AlreadyDeduped))
		if noDupGroups > 0 {
			fmt.Fprintf(os.Stderr, "  No duplicates:    %s groups\n", formatCount(noDupGroups))
		}
		fmt.Fprintf(os.Stderr, "  Errors:           %s\n", formatCount(totalStats.Errors))
	}

	if totalStats.Errors > 0 {
		os.Exit(1)
	}
}
