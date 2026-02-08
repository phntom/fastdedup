package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

func main() {
	var (
		maxSizes = flag.Int("max-sizes", 1_000_000, "maximum unique file sizes to track in pass 1")
		topN     = flag.Int("top", 10_000, "number of most impactful file sizes to dedup in pass 2")
		dryRun   = flag.Bool("dry-run", false, "report what would be deduped without making changes")
		verbose  = flag.Bool("v", false, "verbose output")
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

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Pass 1: survey file sizes.
	slog.Info("pass 1: surveying file sizes", "root", root, "max_sizes", *maxSizes)
	sm := NewSizeMap(*maxSizes)
	fileCount, err := WalkSizes(root, sm)
	if err != nil {
		slog.Error("pass 1 failed", "error", err)
		os.Exit(1)
	}
	slog.Info("pass 1 complete", "files_scanned", fileCount, "unique_sizes", sm.Len())

	// Select top N most impactful sizes (need count >= 2 to dedup).
	targets := sm.TopN(*topN)
	if len(targets) == 0 {
		slog.Info("no candidate file sizes found for deduplication")
		return
	}
	slog.Info("selected target sizes",
		"count", len(targets),
		"top_size", targets[0].Size,
		"top_count", targets[0].Count,
		"top_impact", targets[0].Impact(),
	)

	// Pass 2: deduplicate.
	slog.Info("pass 2: deduplicating", "dry_run", *dryRun)
	stats, err := Dedup(root, targets, *dryRun)
	if err != nil {
		slog.Error("pass 2 failed", "error", err)
		os.Exit(1)
	}
	slog.Info("done",
		"bytes_saved", stats.BytesSaved,
		"files_deduped", stats.FilesDeduped,
		"already_deduped", stats.AlreadyDeduped,
		"errors", stats.Errors,
	)
}
