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
		maxTime      = flag.String("max-time", "", "stop gracefully after duration (e.g. 30m, 2h, 1h30m)")
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
		scrub        = flag.Bool("scrub", false, "run btrfs scrub after dedup completes (requires root, btrfs only)")
		defrag       = flag.Bool("defrag", false, "run btrfs defragment after dedup/scrub (requires root, btrfs only)")
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

	// Parse --max-time deadline.
	var deadline time.Time
	if *maxTime != "" {
		d, err := time.ParseDuration(*maxTime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid --max-time %q: %v\n", *maxTime, err)
			os.Exit(1)
		}
		deadline = time.Now().Add(d)
	}

	// Validate --scrub / --defrag requirements early.
	if *scrub || *defrag {
		if os.Geteuid() != 0 {
			fmt.Fprintf(os.Stderr, "error: --scrub and --defrag require root permissions\n")
			os.Exit(1)
		}
		if !isBtrfs(root) {
			fmt.Fprintf(os.Stderr, "error: --scrub and --defrag require a btrfs filesystem (detected non-btrfs at %s)\n", root)
			os.Exit(1)
		}
	}

	startTime := time.Now()

	if *hardlink && !*dryRun {
		fmt.Fprintf(os.Stderr, "WARNING: --hardlink mode creates hard links instead of reflinks.\n")
		fmt.Fprintf(os.Stderr, "  Hard-linked files share the same inode — editing one file changes ALL copies.\n")
		fmt.Fprintf(os.Stderr, "  Metadata (permissions, timestamps) is also shared. Use with caution.\n\n")
	}

	fmtSize := func(b int64) string {
		return formatSize(b, *rawSizes)
	}

	// Acquire per-root lock to prevent concurrent runs.
	lockFile, lockErr := acquireLock(root)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "error: another fastdedup instance is already running on %s\n", root)
		os.Exit(1)
	}
	defer releaseLock(lockFile)

	// Load dedup cache.
	var cacheFile string
	var cached map[int64]uint64
	if !*noCache && os.Getenv("FASTDEDUP_NO_CACHE") == "" {
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

	// Estimate progress: try metadata cache for file count, then statfs for used bytes.
	var mFile string
	var estimatedFiles int64
	if cacheFile != "" {
		mFile = metaPath(cacheFile)
		if meta := loadMeta(mFile); meta != nil {
			estimatedFiles = meta.FileCount
		}
	}
	if estimatedFiles == 0 && isMountPoint(root) {
		estimatedFiles = fsFileEstimate(root)
	}
	var estimatedBytes int64
	if estimatedFiles == 0 {
		estimatedBytes = fsUsedBytes(root)
	}

	var scanCount int64
	var scanBytes int64
	scanStart := time.Now()
	lastUpdate := scanStart
	fileCount, err := WalkSizes(root, sm, *snapshots, *minSize, func(path string, size int64) {
		if filenameHashes != nil {
			filenameHashes[size] += hashFilename(filepath.Base(path))
		}
		scanCount++
		scanBytes += size
		if scanCount%100 == 0 {
			now := time.Now()
			if now.Sub(lastUpdate) >= 200*time.Millisecond {
				lastUpdate = now
				elapsed := now.Sub(scanStart)
				rate := int64(float64(scanCount) / elapsed.Seconds())
				if estimatedFiles > 0 {
					eta := formatETA(elapsed, scanCount, estimatedFiles)
					suffix := fmt.Sprintf("%s/s %s", formatCount(rate), eta)
					printProgressBar("  Scanning:", scanCount, estimatedFiles, suffix)
				} else if estimatedBytes > 0 {
					eta := formatETA(elapsed, scanBytes, estimatedBytes)
					suffix := fmt.Sprintf("%s / %s %s", formatSize(scanBytes, false), formatSize(estimatedBytes, false), eta)
					printProgressBar("  Scanning:", scanBytes, estimatedBytes, suffix)
				} else {
					printStatus(fmt.Sprintf("  Scanned: %s (%s/s)", formatCount(scanCount), formatCount(rate)))
				}
			}
		}
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: pass 1 failed: %v\n", err)
		os.Exit(1)
	}
	finishLine(fmt.Sprintf("  Scanned %s files, %s unique sizes",
		formatCount(fileCount), formatCount(int64(sm.Len()))))

	// Save scan metadata for future progress estimation.
	if mFile != "" {
		_ = saveMeta(mFile, &ScanMeta{FileCount: fileCount})
	}

	// Select top N most impactful sizes, excluding cached (unchanged) groups.
	// Cached sizes are filtered before applying the -top limit so that
	// subsequent runs still process the requested number of entries.
	allCandidates := sm.TopN(sm.Len())
	var targets []SizeEntry
	var skippedCached int64
	for _, t := range allCandidates {
		if cached != nil {
			if h, ok := cached[t.Size]; ok && h == filenameHashes[t.Size] {
				skippedCached++
				continue
			}
		}
		if len(targets) < *topN {
			targets = append(targets, t)
		}
	}

	if len(targets) == 0 {
		if !*quiet {
			if skippedCached > 0 {
				fmt.Fprintf(os.Stderr, "\nNo new duplicate file sizes found (%s cached).\n", formatCount(skippedCached))
			} else {
				fmt.Fprintf(os.Stderr, "\nNo duplicate file sizes found.\n")
			}
		}
		return
	}

	// Display top sizes table.
	if !*quiet {
		if skippedCached > 0 {
			fmt.Fprintf(os.Stderr, "  Skipped %s unchanged size groups (cached)\n", formatCount(skippedCached))
		}
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
	var timeLimitHit bool    // set when --max-time deadline is reached
	dedupStart := time.Now()

	timeExpired := func() bool {
		return !deadline.IsZero() && time.Now().After(deadline)
	}

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
				overall := groupBase + int64(current)
				eta := formatETA(time.Since(dedupStart), overall, expectedFiles)
				overallPct := overall * 100 / expectedFiles
				suffix := fmt.Sprintf("(%d%%) %s", overallPct, eta)
				printProgressBar(prefix, int64(current), int64(len(paths)), suffix)
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
		totalStats.ErrorDetails = append(totalStats.ErrorDetails, stats.ErrorDetails...)
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
		collectStart := time.Now()
		lastCollectUpdate := collectStart
		collected, err := CollectFiles(root, targetSet, *snapshots, *minSize, dirPool, func() {
			collectCount++
			if collectCount%100 == 0 {
				now := time.Now()
				if now.Sub(lastCollectUpdate) >= 200*time.Millisecond {
					lastCollectUpdate = now
					eta := formatETA(now.Sub(collectStart), collectCount, expectedFiles)
					printProgressBar("  Collecting:", collectCount, expectedFiles, eta)
				}
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
			} else if cacheFile != "" && !*dryRun {
				// No duplicates for this size — cache to skip on next run.
				cached[t.Size] = filenameHashes[t.Size]
			}
		}
		finishLine(fmt.Sprintf("  Collected %s files in %s size groups",
			formatCount(totalFiles), formatCount(int64(len(toProcess)))))

		if len(toProcess) == 0 {
			if cacheFile != "" && !*dryRun {
				if err := saveCache(cacheFile, cached); err != nil {
					slog.Debug("failed to save cache", "error", err)
				}
			}
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
			if timeExpired() {
				timeLimitHit = true
				finishLine("  Time limit reached, stopping gracefully")
				break
			}
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
			if timeExpired() {
				timeLimitHit = true
				finishLine("  Time limit reached, stopping gracefully")
				break
			}
			singleSet := map[int64]struct{}{t.Size: {}}
			collected, err := CollectFiles(root, singleSet, *snapshots, *minSize, dirPool, nil)
			if err != nil {
				slog.Debug("collection failed", "size", t.Size, "error", err)
				continue
			}
			paths := collected[t.Size]
			if len(paths) < 2 {
				if cacheFile != "" && !*dryRun {
					cached[t.Size] = filenameHashes[t.Size]
				}
				continue
			}
			processGroup(i, len(targets), t.Size, ExpandPaths(paths))
		}
	} else {
		// Default: wave-based collection. Fill memory up to budget, process
		// cached groups, free memory, refill with remaining groups, repeat.
		// Groups too large to ever fit get processed last via per-size scan.
		type cachedGroup struct {
			paths   []CompactPath
			memUsed int64
		}
		memBudget := *memBudgetMB * 1024 * 1024
		processed := make(map[int64]bool)
		oversized := make(map[int64]bool)
		groupsDone := 0
		totalGroups := len(targets)

		if !*quiet {
			dryLabel := ""
			if *dryRun {
				dryLabel = " (dry run)"
			}
			fmt.Fprintf(os.Stderr, "\nPass 2: Deduplicating%s: %s groups, %s files, up to %s potential savings\n",
				dryLabel, formatCount(int64(totalGroups)), formatCount(expectedFiles), fmtSize(expectedSavings))
		}

		for wave := 1; ; wave++ {
			if timeExpired() {
				timeLimitHit = true
				finishLine("  Time limit reached, stopping gracefully")
				break
			}

			// Build set of sizes to collect this wave.
			collectSet := make(map[int64]struct{})
			for _, t := range targets {
				if !processed[t.Size] && !oversized[t.Size] {
					collectSet[t.Size] = struct{}{}
				}
			}
			if len(collectSet) == 0 {
				break
			}

			if wave > 1 && !*quiet {
				fmt.Fprintf(os.Stderr, "  Wave %d: collecting %s remaining groups...\n", wave, formatCount(int64(len(collectSet))))
			} else if !*quiet {
				fmt.Fprintf(os.Stderr, "  Collecting target files...\n")
			}

			// Collect into memory with eviction.
			cache := make(map[int64]*cachedGroup)
			evicted := make(map[int64]bool)
			var totalMem int64
			var collectCount int64
			waveStart := time.Now()
			lastWaveUpdate := waveStart

			_ = walkRandom(root, *snapshots, *minSize, func(path string, size int64) {
				if _, ok := collectSet[size]; !ok {
					return
				}
				if evicted[size] {
					return
				}

				collectCount++
				if collectCount%100 == 0 {
					now := time.Now()
					if now.Sub(lastWaveUpdate) >= 200*time.Millisecond {
						lastWaveUpdate = now
						eta := formatETA(now.Sub(waveStart), collectCount, expectedFiles)
						printProgressBar("  Collecting:", collectCount, expectedFiles, eta)
					}
				}

				dir, name := filepath.Dir(path), filepath.Base(path)
				iDir, dirCost := dirPool.Intern(dir)
				cp := CompactPath{Dir: iDir, Name: name}
				pathMem := cp.MemCost()
				totalMem += dirCost

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

					// If cache is empty after eviction, this group is too large to ever fit.
					if len(cache) == 0 && totalMem > memBudget {
						oversized[maxSize] = true
						delete(evicted, maxSize)
						totalMem = 0
						slog.Debug("marked oversized group", "size", maxSize)
					}
				}
			})

			if len(cache) == 0 {
				finishLine(fmt.Sprintf("  Wave %d: no groups fit in memory", wave))
				break
			}
			finishLine(fmt.Sprintf("  Collected %s groups (%s deferred)",
				formatCount(int64(len(cache))), formatCount(int64(len(evicted)))))

			// Process cached groups in original priority order.
			for _, t := range targets {
				if timeExpired() {
					timeLimitHit = true
					finishLine("  Time limit reached, stopping gracefully")
					break
				}
				g, ok := cache[t.Size]
				if !ok {
					continue
				}
				delete(cache, t.Size)
				processed[t.Size] = true
				if len(g.paths) < 2 {
					if cacheFile != "" && !*dryRun {
						cached[t.Size] = filenameHashes[t.Size]
					}
					groupsDone++
					continue
				}
				processGroup(groupsDone, totalGroups, t.Size, ExpandPaths(g.paths))
				groupsDone++
			}
			if timeLimitHit {
				break
			}

			// Check if all non-oversized groups are done.
			allDone := true
			for _, t := range targets {
				if !processed[t.Size] && !oversized[t.Size] {
					allDone = false
					break
				}
			}
			if allDone {
				break
			}
		}

		// Process oversized groups last via per-size scan.
		for _, t := range targets {
			if !oversized[t.Size] {
				continue
			}
			if timeLimitHit || timeExpired() {
				timeLimitHit = true
				break
			}
			slog.Debug("processing oversized group via per-size scan", "size", t.Size)
			singleSet := map[int64]struct{}{t.Size: {}}
			c, err := CollectFiles(root, singleSet, *snapshots, *minSize, dirPool, nil)
			if err != nil {
				slog.Debug("collection failed", "size", t.Size, "error", err)
				groupsDone++
				continue
			}
			paths := c[t.Size]
			if len(paths) < 2 {
				if cacheFile != "" && !*dryRun {
					cached[t.Size] = filenameHashes[t.Size]
				}
				groupsDone++
				continue
			}
			processGroup(groupsDone, totalGroups, t.Size, ExpandPaths(paths))
			groupsDone++
		}
	}

	// Save dedup cache (skip on dry-run).
	// Individual groups are cached incrementally inside processGroup and at
	// <2-paths skip points above, so this block only prunes stale entries
	// and does a final save.
	if cacheFile != "" && !*dryRun {
		// Prune stale cache entries for sizes no longer present on disk.
		if filenameHashes != nil {
			for size := range cached {
				if _, exists := filenameHashes[size]; !exists {
					delete(cached, size)
				}
			}
		}
		if err := saveCache(cacheFile, cached); err != nil {
			slog.Debug("failed to save cache", "error", err)
		}
	}

	// Write anonymized error report (unless disabled).
	if os.Getenv("FASTDEDUP_NO_REPORT_FILE") == "" && len(totalStats.ErrorDetails) > 0 && !*dryRun {
		if rf, err := reportFilePath(); err == nil {
			if err := appendReport(rf, totalStats.ErrorDetails, version); err != nil {
				slog.Debug("failed to write error report", "error", err)
			}
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

	// Send webhook notifications.
	if url := os.Getenv("FASTDEDUP_WEBHOOK_UPDATES"); url != "" {
		notifyUpdate(url, root, totalStats, elapsed, *dryRun)
	}
	if url := os.Getenv("FASTDEDUP_WEBHOOK_ALERTS"); url != "" && totalStats.Errors > 0 {
		notifyAlert(url, root, totalStats)
	}
	if url := os.Getenv("FASTDEDUP_HEALTHCHECK_URL"); url != "" {
		pingHealthcheck(url)
	}

	// Post-dedup btrfs maintenance (order: scrub first, then defrag).
	if *scrub && !*dryRun {
		if err := runScrub(root); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	if *defrag && !*dryRun {
		if err := runDefrag(root, fileCount); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	if totalStats.Errors > 0 {
		os.Exit(1)
	}
}
