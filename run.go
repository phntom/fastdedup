package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dedupRun holds the shared state of pass 2: the target size groups, the
// accumulated statistics, the dedup cache and the progress bookkeeping
// common to all three collection modes (batch, low-memory and wave).
type dedupRun struct {
	cfg             *config
	cacheFile       string
	cached          map[int64]uint64
	filenameHashes  map[int64]uint64
	targets         []SizeEntry
	dirPool         *DirIntern
	totalStats      *DedupStats
	errorSizes      map[int64]bool // size groups that had errors
	expectedFiles   int64
	expectedSavings int64
	filesProcessed  int64 // cumulative files across all groups
	noDupGroups     int64 // groups where no action was taken
	timeLimitHit    bool  // set when the --max-time deadline is reached
	dedupStart      time.Time
}

func newDedupRun(cfg *config, sv *surveyResult, cacheFile string, cached map[int64]uint64) *dedupRun {
	r := &dedupRun{
		cfg:            cfg,
		cacheFile:      cacheFile,
		cached:         cached,
		filenameHashes: sv.filenameHashes,
		targets:        sv.targets,
		dirPool:        NewDirIntern(),
		totalStats:     &DedupStats{},
		errorSizes:     make(map[int64]bool),
		dedupStart:     time.Now(),
	}
	// Compute expected totals from pass 1 for overall progress.
	for _, t := range sv.targets {
		r.expectedFiles += t.Count
		r.expectedSavings += t.Savings()
	}
	return r
}

// timeExpired reports whether the --max-time deadline has passed.
func (r *dedupRun) timeExpired() bool {
	return !r.cfg.deadline.IsZero() && time.Now().After(r.cfg.deadline)
}

// execute runs pass 2 in the selected collection mode. It reports whether
// the run reached the final stage: batch mode ends early, with its own
// report, when no group collected enough files to deduplicate.
func (r *dedupRun) execute() bool {
	switch {
	case r.cfg.batch:
		return r.runBatch()
	case r.cfg.lowMemory:
		r.runLowMemory()
	default:
		r.runWave()
	}
	return true
}

// processGroup deduplicates one size group and accumulates stats.
func (r *dedupRun) processGroup(idx, total int, size int64, paths []string) {
	cfg := r.cfg
	numWidth := len(fmt.Sprintf("%d", total))
	prefix := fmt.Sprintf("  [%*d/%d] %10s × %-8s",
		numWidth, idx+1, total,
		cfg.fmtSize(size), formatCount(int64(len(paths))))

	step := max(1, len(paths)/200)
	groupBase := r.filesProcessed
	stats := ProcessSizeGroup(paths, size, cfg.dryRun, cfg.verbose, cfg.rawSizes, cfg.hardlink, cfg.fixPerms, func(current int) {
		if current%step == 0 || current == len(paths) {
			overall := groupBase + int64(current)
			eta := formatETA(time.Since(r.dedupStart), overall, r.expectedFiles)
			overallPct := overall * 100 / r.expectedFiles
			suffix := fmt.Sprintf("(%d%%) %s", overallPct, eta)
			printProgressBar(prefix, int64(current), int64(len(paths)), suffix)
		}
	})
	r.filesProcessed += int64(len(paths))

	var parts []string
	if stats.FilesDeduped > 0 {
		parts = append(parts, fmt.Sprintf("%s deduped, %s saved",
			formatCount(stats.FilesDeduped), cfg.fmtSize(stats.BytesSaved)))
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
		r.noDupGroups++
		// Clear progress bar but don't print a line for no-action groups.
		printStatus("")
	} else {
		finishLine(fmt.Sprintf("%s  ✓ %s", prefix, strings.Join(parts, ", ")))
	}

	r.totalStats.BytesSaved += stats.BytesSaved
	r.totalStats.FilesDeduped += stats.FilesDeduped
	r.totalStats.AlreadyDeduped += stats.AlreadyDeduped
	r.totalStats.Errors += stats.Errors
	r.totalStats.ErrorDetails = append(r.totalStats.ErrorDetails, stats.ErrorDetails...)
	if stats.Errors > 0 {
		r.errorSizes[size] = true
	}

	// Incrementally save cache after each completed group so Ctrl+C doesn't lose progress.
	if r.cacheFile != "" && !cfg.dryRun && !r.errorSizes[size] {
		r.cached[size] = r.filenameHashes[size]
		if err := saveCache(r.cacheFile, r.cached); err != nil {
			slog.Debug("failed to save cache", "error", err)
		}
	}
}

// runBatch implements --batch: collect all target files in a single pass,
// then deduplicate every group in memory. It reports false when no group
// collected enough files to deduplicate, ending the run early.
func (r *dedupRun) runBatch() bool {
	cfg := r.cfg
	if !cfg.quiet {
		fmt.Fprintf(os.Stderr, "\nPass 2: Collecting target files...\n")
	}
	targetSet := make(map[int64]struct{}, len(r.targets))
	for _, t := range r.targets {
		targetSet[t.Size] = struct{}{}
	}

	var collectCount int64
	collectStart := time.Now()
	lastCollectUpdate := collectStart
	collected, err := CollectFiles(cfg.root, targetSet, cfg.snapshots, cfg.minSize, r.dirPool, func() {
		collectCount++
		if collectCount%100 == 0 {
			now := time.Now()
			if now.Sub(lastCollectUpdate) >= 200*time.Millisecond {
				lastCollectUpdate = now
				eta := formatETA(now.Sub(collectStart), collectCount, r.expectedFiles)
				printProgressBar("  Collecting:", collectCount, r.expectedFiles, eta)
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
	for _, t := range r.targets {
		paths := collected[t.Size]
		if len(paths) >= 2 {
			toProcess = append(toProcess, processEntry{t.Size, paths})
			totalFiles += int64(len(paths))
		} else if r.cacheFile != "" && !cfg.dryRun {
			// No duplicates for this size — cache to skip on next run.
			r.cached[t.Size] = r.filenameHashes[t.Size]
		}
	}
	finishLine(fmt.Sprintf("  Collected %s files in %s size groups",
		formatCount(totalFiles), formatCount(int64(len(toProcess)))))

	if len(toProcess) == 0 {
		if r.cacheFile != "" && !cfg.dryRun {
			if err := saveCache(r.cacheFile, r.cached); err != nil {
				slog.Debug("failed to save cache", "error", err)
			}
		}
		if !cfg.quiet {
			fmt.Fprintf(os.Stderr, "\nNo files to deduplicate.\n")
		}
		return false
	}

	if !cfg.quiet {
		dryLabel := ""
		if cfg.dryRun {
			dryLabel = " (dry run)"
		}
		fmt.Fprintf(os.Stderr, "\nDeduplicating%s: %s groups, %s files, up to %s potential savings\n",
			dryLabel, formatCount(int64(len(toProcess))), formatCount(r.expectedFiles), cfg.fmtSize(r.expectedSavings))
	}

	for i, entry := range toProcess {
		if r.timeExpired() {
			r.timeLimitHit = true
			finishLine("  Time limit reached, stopping gracefully")
			break
		}
		r.processGroup(i, len(toProcess), entry.size, ExpandPaths(entry.paths))
	}
	return true
}

// runLowMemory implements --low-memory: scan the tree separately for each
// target size, so only one group is ever held in memory.
func (r *dedupRun) runLowMemory() {
	cfg := r.cfg
	if !cfg.quiet {
		dryLabel := ""
		if cfg.dryRun {
			dryLabel = " (dry run)"
		}
		fmt.Fprintf(os.Stderr, "\nPass 2: Deduplicating%s: %s groups, %s files, up to %s potential savings\n",
			dryLabel, formatCount(int64(len(r.targets))), formatCount(r.expectedFiles), cfg.fmtSize(r.expectedSavings))
	}

	for i, t := range r.targets {
		if r.timeExpired() {
			r.timeLimitHit = true
			finishLine("  Time limit reached, stopping gracefully")
			break
		}
		singleSet := map[int64]struct{}{t.Size: {}}
		collected, err := CollectFiles(cfg.root, singleSet, cfg.snapshots, cfg.minSize, r.dirPool, nil)
		if err != nil {
			slog.Debug("collection failed", "size", t.Size, "error", err)
			continue
		}
		paths := collected[t.Size]
		if len(paths) < 2 {
			if r.cacheFile != "" && !cfg.dryRun {
				r.cached[t.Size] = r.filenameHashes[t.Size]
			}
			continue
		}
		r.processGroup(i, len(r.targets), t.Size, ExpandPaths(paths))
	}
}

// runWave implements the default collection mode: fill memory up to the
// budget, process the cached groups, free memory, refill with the
// remaining groups, repeat. Groups that cannot fit the memory budget —
// alone or together with the rest of a wave — are finished via a per-size
// scan, which handles any group size.
func (r *dedupRun) runWave() {
	cfg := r.cfg
	type cachedGroup struct {
		paths   []CompactPath
		memUsed int64
	}
	memBudget := cfg.memBudgetMB * 1024 * 1024
	processed := make(map[int64]bool)
	oversized := make(map[int64]bool)
	groupsDone := 0
	totalGroups := len(r.targets)

	if !cfg.quiet {
		dryLabel := ""
		if cfg.dryRun {
			dryLabel = " (dry run)"
		}
		fmt.Fprintf(os.Stderr, "\nPass 2: Deduplicating%s: %s groups, %s files, up to %s potential savings\n",
			dryLabel, formatCount(int64(totalGroups)), formatCount(r.expectedFiles), cfg.fmtSize(r.expectedSavings))
	}

	for wave := 1; ; wave++ {
		if r.timeExpired() {
			r.timeLimitHit = true
			finishLine("  Time limit reached, stopping gracefully")
			break
		}

		// Build set of sizes to collect this wave.
		collectSet := make(map[int64]struct{})
		for _, t := range r.targets {
			if !processed[t.Size] && !oversized[t.Size] {
				collectSet[t.Size] = struct{}{}
			}
		}
		if len(collectSet) == 0 {
			break
		}

		if wave > 1 && !cfg.quiet {
			fmt.Fprintf(os.Stderr, "  Wave %d: collecting %s remaining groups...\n", wave, formatCount(int64(len(collectSet))))
		} else if !cfg.quiet {
			fmt.Fprintf(os.Stderr, "  Collecting target files...\n")
		}

		// Collect into memory with eviction.
		cache := make(map[int64]*cachedGroup)
		evicted := make(map[int64]bool)
		var totalMem int64
		var collectCount int64
		waveStart := time.Now()
		lastWaveUpdate := waveStart

		_ = walkRandom(cfg.root, cfg.snapshots, cfg.minSize, func(path string, size int64) {
			if _, ok := collectSet[size]; !ok {
				return
			}
			if evicted[size] || oversized[size] {
				return
			}

			collectCount++
			if collectCount%100 == 0 {
				now := time.Now()
				if now.Sub(lastWaveUpdate) >= 200*time.Millisecond {
					lastWaveUpdate = now
					eta := formatETA(now.Sub(waveStart), collectCount, r.expectedFiles)
					printProgressBar("  Collecting:", collectCount, r.expectedFiles, eta)
				}
			}

			dir, name := filepath.Dir(path), filepath.Base(path)
			iDir, dirCost := r.dirPool.Intern(dir)
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
				totalMem -= maxMem
				delete(cache, maxSize)
				slog.Debug("evicted size group from cache", "size", maxSize, "freed", maxMem)

				// A group whose own footprint already exceeds the budget
				// can never fit alongside anything else — and if the
				// eviction emptied the cache while the walk still busts
				// the budget, no group fits at all. Both kinds are routed
				// to the per-size scan, which handles any group size.
				if maxMem > memBudget || (len(cache) == 0 && totalMem > memBudget) {
					oversized[maxSize] = true
					delete(evicted, maxSize)
					if len(cache) == 0 {
						totalMem = 0
					}
					slog.Debug("marked oversized group", "size", maxSize)
				}
			}
		})

		if len(cache) == 0 {
			// Nothing survived collection, so no wave will ever make progress:
			// every remaining group is, in effect, too large for the budget.
			// Escalate them all to the per-size scan below instead of
			// dropping them. This also guarantees the loop terminates: after
			// the break, no group is left neither processed nor oversized.
			individual := 0
			for _, t := range r.targets {
				if !processed[t.Size] && !oversized[t.Size] {
					oversized[t.Size] = true
				}
				if oversized[t.Size] {
					individual++
				}
			}
			finishLine(fmt.Sprintf("  Wave %d: %s groups exceed the memory budget — processing individually",
				wave, formatCount(int64(individual))))
			break
		}
		finishLine(fmt.Sprintf("  Collected %s groups (%s deferred)",
			formatCount(int64(len(cache))), formatCount(int64(len(evicted)))))

		// Process cached groups in original priority order.
		for _, t := range r.targets {
			if r.timeExpired() {
				r.timeLimitHit = true
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
				if r.cacheFile != "" && !cfg.dryRun {
					r.cached[t.Size] = r.filenameHashes[t.Size]
				}
				groupsDone++
				continue
			}
			r.processGroup(groupsDone, totalGroups, t.Size, ExpandPaths(g.paths))
			groupsDone++
		}
		if r.timeLimitHit {
			break
		}

		// Check if all non-oversized groups are done.
		allDone := true
		for _, t := range r.targets {
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
	for _, t := range r.targets {
		if !oversized[t.Size] {
			continue
		}
		if r.timeLimitHit || r.timeExpired() {
			r.timeLimitHit = true
			break
		}
		slog.Debug("processing oversized group via per-size scan", "size", t.Size)
		singleSet := map[int64]struct{}{t.Size: {}}
		c, err := CollectFiles(cfg.root, singleSet, cfg.snapshots, cfg.minSize, r.dirPool, nil)
		if err != nil {
			slog.Debug("collection failed", "size", t.Size, "error", err)
			groupsDone++
			continue
		}
		paths := c[t.Size]
		if len(paths) < 2 {
			if r.cacheFile != "" && !cfg.dryRun {
				r.cached[t.Size] = r.filenameHashes[t.Size]
			}
			groupsDone++
			continue
		}
		r.processGroup(groupsDone, totalGroups, t.Size, ExpandPaths(paths))
		groupsDone++
	}
}

// finishRun performs the final stage: cache maintenance, the anonymized
// error report, the run summary, webhook notifications, btrfs maintenance
// and the process exit code.
func finishRun(cfg *config, r *dedupRun, fileCount int64, startTime time.Time) {
	// Save dedup cache (skip on dry-run).
	// Individual groups are cached incrementally inside processGroup and at
	// <2-paths skip points above, so this block only prunes stale entries
	// and does a final save.
	if r.cacheFile != "" && !cfg.dryRun {
		// Prune stale cache entries for sizes no longer present on disk.
		if r.filenameHashes != nil {
			for size := range r.cached {
				if _, exists := r.filenameHashes[size]; !exists {
					delete(r.cached, size)
				}
			}
		}
		if err := saveCache(r.cacheFile, r.cached); err != nil {
			slog.Debug("failed to save cache", "error", err)
		}
	}

	// Write anonymized error report (unless disabled).
	if os.Getenv("FASTDEDUP_NO_REPORT_FILE") == "" && len(r.totalStats.ErrorDetails) > 0 && !cfg.dryRun {
		if rf, err := reportFilePath(); err == nil {
			if err := appendReport(rf, r.totalStats.ErrorDetails, version); err != nil {
				slog.Debug("failed to write error report", "error", err)
			}
		}
	}

	// Final summary.
	elapsed := time.Since(startTime).Truncate(time.Millisecond)
	if cfg.quiet {
		if r.totalStats.FilesDeduped > 0 || r.totalStats.Errors > 0 {
			fmt.Fprintf(os.Stderr, "fastdedup: %s: %s deduped, %s saved, %s already, %s errors (%s)\n",
				cfg.root,
				formatCount(r.totalStats.FilesDeduped), cfg.fmtSize(r.totalStats.BytesSaved),
				formatCount(r.totalStats.AlreadyDeduped), formatCount(r.totalStats.Errors),
				elapsed)
		}
	} else {
		fmt.Fprintf(os.Stderr, "\nDone in %s!\n", elapsed)
		fmt.Fprintf(os.Stderr, "  Files deduped:    %s\n", formatCount(r.totalStats.FilesDeduped))
		fmt.Fprintf(os.Stderr, "  Space saved:      %s\n", cfg.fmtSize(r.totalStats.BytesSaved))
		fmt.Fprintf(os.Stderr, "  Already deduped:  %s\n", formatCount(r.totalStats.AlreadyDeduped))
		if r.noDupGroups > 0 {
			fmt.Fprintf(os.Stderr, "  No duplicates:    %s groups\n", formatCount(r.noDupGroups))
		}
		fmt.Fprintf(os.Stderr, "  Errors:           %s\n", formatCount(r.totalStats.Errors))
	}

	// Send webhook notifications.
	if url := os.Getenv("FASTDEDUP_WEBHOOK_UPDATES"); url != "" {
		notifyUpdate(url, cfg.root, r.totalStats, elapsed, cfg.dryRun)
	}
	if url := os.Getenv("FASTDEDUP_WEBHOOK_ALERTS"); url != "" && r.totalStats.Errors > 0 {
		notifyAlert(url, cfg.root, r.totalStats)
	}
	if url := os.Getenv("FASTDEDUP_HEALTHCHECK_URL"); url != "" {
		pingHealthcheck(url)
	}

	// Post-dedup btrfs maintenance (order: scrub first, then defrag).
	if cfg.scrub && !cfg.dryRun {
		if err := runScrub(cfg.root); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	if cfg.defrag && !cfg.dryRun {
		if err := runDefrag(cfg.root, fileCount); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	if r.totalStats.Errors > 0 {
		os.Exit(1)
	}
}
