package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

var version = "dev"

// config holds every command-line flag plus the values derived from them:
// the canonical root, the log level / quiet mode, the --max-time deadline
// and the pass-2 collection mode selection.
type config struct {
	root        string    // canonical absolute path of the directory to dedup
	maxSizes    int       // --max-sizes: unique sizes tracked in pass 1
	topN        int       // --top: most impactful sizes deduped in pass 2
	minSize     int64     // --min-size: files below this size are ignored
	memBudgetMB int64     // --mem-budget: wave-mode path cache budget in MiB
	deadline    time.Time // --max-time: zero when unset
	dryRun      bool
	verbose     bool
	quiet       bool
	batch       bool
	lowMemory   bool
	noCache     bool
	hardlink    bool
	fixPerms    bool
	rawSizes    bool
	snapshots   bool
	scrub       bool
	defrag      bool
}

// parseFlags defines the command line, parses it and applies the early
// global setup: canonical root path, log level, quiet mode, --max-time
// deadline and the --scrub/--defrag requirement checks. It returns
// (nil, false) when the program should simply exit (version output);
// fatal flag errors exit directly with code 1.
func parseFlags() (*config, bool) {
	var (
		maxSizes    = flag.Int("max-sizes", 1_000_000, "maximum unique file sizes to track in pass 1")
		topN        = flag.Int("top", 10_000, "number of most impactful file sizes to dedup in pass 2")
		minSize     = flag.Int64("min-size", 524288, "minimum file size to process in bytes")
		maxTime     = flag.String("max-time", "", "stop gracefully after duration (e.g. 30m, 2h, 1h30m)")
		dryRun      = flag.Bool("dry-run", false, "report what would be deduped without making changes")
		verbose     = flag.Bool("v", false, "show file paths of deduped files and detailed diagnostics")
		quiet       = flag.Bool("q", false, "quiet mode — only print final summary (for cronjobs)")
		batch       = flag.Bool("batch", false, "collect all target files in one pass (faster, uses more memory)")
		lowMemory   = flag.Bool("low-memory", false, "scan separately for each file size (lowest memory, slower)")
		memBudgetMB = flag.Int64("mem-budget", 256, "memory budget in MiB for path cache in default mode")
		noCache     = flag.Bool("no-cache", false, "ignore saved state — reprocess all file sizes even if unchanged since last run")
		hardlink    = flag.Bool("hardlink", false, "use hard links instead of reflinks (works on any filesystem, but linked files share all changes)")
		fixPerms    = flag.Bool("fix-perms", false, "temporarily add write permission to read-only directories during dedup, then restore")
		rawSizes    = flag.Bool("raw-sizes", false, "show raw byte counts instead of human-readable")
		snapshots   = flag.Bool("snapshots", false, "include .snapshots directories (skipped by default)")
		scrub       = flag.Bool("scrub", false, "run btrfs scrub after dedup completes (requires root, btrfs only)")
		defrag      = flag.Bool("defrag", false, "run btrfs defragment after dedup/scrub (requires root, btrfs only)")
		showVersion = flag.Bool("version", false, "print version and exit")
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
		return nil, false
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

	cfg := &config{
		root:        root,
		maxSizes:    *maxSizes,
		topN:        *topN,
		minSize:     *minSize,
		memBudgetMB: *memBudgetMB,
		dryRun:      *dryRun,
		verbose:     *verbose,
		quiet:       *quiet,
		batch:       *batch,
		lowMemory:   *lowMemory,
		noCache:     *noCache,
		hardlink:    *hardlink,
		fixPerms:    *fixPerms,
		rawSizes:    *rawSizes,
		snapshots:   *snapshots,
		scrub:       *scrub,
		defrag:      *defrag,
	}

	// Set log level and quiet mode.
	level := slog.LevelWarn
	if cfg.verbose {
		level = slog.LevelDebug
	}
	if cfg.quiet {
		level = slog.LevelError
		quietMode = true
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Parse --max-time deadline.
	if *maxTime != "" {
		d, err := time.ParseDuration(*maxTime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid --max-time %q: %v\n", *maxTime, err)
			os.Exit(1)
		}
		cfg.deadline = time.Now().Add(d)
	}

	// Validate --scrub / --defrag requirements early.
	if cfg.scrub || cfg.defrag {
		if os.Geteuid() != 0 {
			fmt.Fprintf(os.Stderr, "error: --scrub and --defrag require root permissions\n")
			os.Exit(1)
		}
		if !isBtrfs(cfg.root) {
			fmt.Fprintf(os.Stderr, "error: --scrub and --defrag require a btrfs filesystem (detected non-btrfs at %s)\n", cfg.root)
			os.Exit(1)
		}
	}

	return cfg, true
}

// fmtSize formats a byte count honouring --raw-sizes.
func (c *config) fmtSize(b int64) string {
	return formatSize(b, c.rawSizes)
}

func main() {
	cfg, ok := parseFlags()
	if !ok {
		return
	}

	startTime := time.Now()

	if cfg.hardlink && !cfg.dryRun {
		fmt.Fprintf(os.Stderr, "WARNING: --hardlink mode creates hard links instead of reflinks.\n")
		fmt.Fprintf(os.Stderr, "  Hard-linked files share the same inode — editing one file changes ALL copies.\n")
		fmt.Fprintf(os.Stderr, "  Metadata (permissions, timestamps) is also shared. Use with caution.\n\n")
	}

	// Acquire per-root lock to prevent concurrent runs.
	lockFile, lockErr := acquireLock(cfg.root)
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "error: another fastdedup instance is already running on %s\n", cfg.root)
		os.Exit(1)
	}
	defer releaseLock(lockFile)

	// Load dedup cache.
	var cacheFile string
	var cached map[int64]uint64
	if !cfg.noCache && os.Getenv("FASTDEDUP_NO_CACHE") == "" {
		if cf, err := cachePath(cfg.root); err == nil {
			cacheFile = cf
			cached = loadCache(cf)
		}
	}

	// === Pass 1: survey file sizes ===
	sv := runSurvey(cfg, cacheFile, cached)
	if sv == nil {
		return // nothing to deduplicate; the survey already reported
	}

	// === Pass 2: deduplicate ===
	run := newDedupRun(cfg, sv, cacheFile, cached)
	if !run.execute() {
		return // batch mode found no deduplicatable group and already reported
	}

	finishRun(cfg, run, sv.fileCount, startTime)
}

// surveyResult carries the outcome of pass 1 into pass 2.
type surveyResult struct {
	fileCount      int64            // total regular files seen
	targets        []SizeEntry      // size groups to dedup, best savings first
	skippedCached  int64            // groups skipped via the dedup cache
	filenameHashes map[int64]uint64 // per-size filename hashes (nil when the cache is disabled)
}

// runSurvey performs pass 1: scan the tree and record file sizes, save scan
// metadata for future progress estimation, filter out unchanged cached size
// groups, select the top-N most impactful targets and print the survey
// report. It returns nil when there is nothing to deduplicate (the report
// has already been printed and the run should end successfully).
func runSurvey(cfg *config, cacheFile string, cached map[int64]uint64) *surveyResult {
	root := cfg.root
	if !cfg.quiet {
		fmt.Fprintf(os.Stderr, "Pass 1: Scanning file sizes in %s\n", root)
	}
	sm := NewSizeMap(cfg.maxSizes)
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
	fileCount, err := WalkSizes(root, sm, cfg.snapshots, cfg.minSize, func(path string, size int64) {
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
		if len(targets) < cfg.topN {
			targets = append(targets, t)
		}
	}

	if len(targets) == 0 {
		if !cfg.quiet {
			if skippedCached > 0 {
				fmt.Fprintf(os.Stderr, "\nNo new duplicate file sizes found (%s cached).\n", formatCount(skippedCached))
			} else {
				fmt.Fprintf(os.Stderr, "\nNo duplicate file sizes found.\n")
			}
		}
		return nil
	}

	// Display top sizes table.
	if !cfg.quiet {
		if skippedCached > 0 {
			fmt.Fprintf(os.Stderr, "  Skipped %s unchanged size groups (cached)\n", formatCount(skippedCached))
		}
		fmt.Fprintf(os.Stderr, "\nTop file sizes by potential savings:\n")
		tableCount := min(20, len(targets))
		fmt.Fprintf(os.Stderr, "  %4s  %10s  %8s  %10s\n", "#", "Size", "Count", "Savings")
		for i := range tableCount {
			t := targets[i]
			fmt.Fprintf(os.Stderr, "  %4d  %10s  %8s  %10s\n",
				i+1, cfg.fmtSize(t.Size), formatCount(t.Count), cfg.fmtSize(t.Savings()))
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
			"", "", formatCount(totalTargetFiles), cfg.fmtSize(totalTargetSavings))
	}

	return &surveyResult{
		fileCount:      fileCount,
		targets:        targets,
		skippedCached:  skippedCached,
		filenameHashes: filenameHashes,
	}
}
