package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// Extent represents a contiguous physical region of a file on disk.
type Extent struct {
	Logical  uint64
	Physical uint64
	Length   uint64
	Flags    uint32
}

// SameExtents reports whether two extent lists have identical physical mappings.
func SameExtents(a, b []Extent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Physical != b[i].Physical || a[i].Length != b[i].Length {
			return false
		}
	}
	return true
}

// DedupStats tracks deduplication results.
type DedupStats struct {
	BytesSaved     int64
	FilesDeduped   int64
	AlreadyDeduped int64
	Errors         int64
}

// fileRef is a reference file representing a unique content group within a size class.
type fileRef struct {
	path    string
	extents []Extent
}

// Dedup performs pass 2: walks the directory tree, finds files matching
// the target sizes, and deduplicates identical files using reflinks.
// For each target size, files are grouped by content. Within each group
// the shortest path is kept as the reference and other files are relinked
// to share the same physical extents.
func Dedup(root string, targets []SizeEntry, dryRun bool) (*DedupStats, error) {
	targetSet := make(map[int64]struct{}, len(targets))
	for _, t := range targets {
		targetSet[t.Size] = struct{}{}
	}

	// groups maps file size to known content groups (one ref per unique content).
	groups := make(map[int64][]*fileRef)
	stats := &DedupStats{}
	var processed int64

	err := walkRandom(root, func(path string, size int64) {
		if _, ok := targetSet[size]; !ok {
			return
		}

		processed++
		if processed%100_000 == 0 {
			slog.Debug("pass 2 progress",
				"files_processed", processed,
				"deduped", stats.FilesDeduped,
				"saved_bytes", stats.BytesSaved,
			)
		}

		processFile(path, size, groups, stats, dryRun)
	})

	slog.Info("pass 2 scan complete", "files_checked", processed)
	return stats, err
}

// processFile compares a file against known references for its size class.
// If it matches an existing reference (same inode, same extents, or same content),
// the appropriate action is taken (skip, update ref, or dedup).
// If no match is found, a new content group is created.
func processFile(path string, size int64, groups map[int64][]*fileRef, stats *DedupStats, dryRun bool) {
	refs := groups[size]

	// First file of this size — establish as reference.
	if len(refs) == 0 {
		extents, err := getExtents(path)
		if err != nil {
			slog.Debug("cannot get extents, skipping", "path", path, "error", err)
			return
		}
		groups[size] = []*fileRef{{path: path, extents: extents}}
		return
	}

	extents, err := getExtents(path)
	if err != nil {
		slog.Debug("cannot get extents, skipping", "path", path, "error", err)
		return
	}

	for _, ref := range refs {
		// Same inode (hard link) — already sharing storage.
		if same, _ := sameInode(ref.path, path); same {
			if len(path) < len(ref.path) {
				ref.path = path
				ref.extents = extents
			}
			stats.AlreadyDeduped++
			return
		}

		// Same extents (existing reflink) — already sharing storage.
		if SameExtents(ref.extents, extents) {
			if len(path) < len(ref.path) {
				ref.path = path
				ref.extents = extents
			}
			stats.AlreadyDeduped++
			return
		}

		// Different extents — compare file content byte-by-byte.
		equal, err := filesEqual(ref.path, path)
		if err != nil {
			slog.Debug("content comparison failed", "a", ref.path, "b", path, "error", err)
			continue
		}
		if !equal {
			continue
		}

		// Identical content, different extents — deduplicate!
		if dryRun {
			fmt.Printf("[dry-run] dedup: %s -> %s (%d bytes)\n", path, ref.path, size)
			stats.BytesSaved += size
			stats.FilesDeduped++
			return
		}

		if err := dedupFile(ref.path, path); err != nil {
			slog.Warn("dedup failed", "src", ref.path, "dst", path, "error", err)
			stats.Errors++
			return
		}

		slog.Info("deduped", "file", path, "ref", ref.path, "size", size)
		stats.BytesSaved += size
		stats.FilesDeduped++
		return
	}

	// No matching reference — new content group for this size.
	groups[size] = append(refs, &fileRef{path: path, extents: extents})
}

// filesEqual reports whether two files have identical content.
// Both files are assumed to have the same size.
func filesEqual(pathA, pathB string) (bool, error) {
	fa, err := os.Open(pathA)
	if err != nil {
		return false, err
	}
	//goland:noinspection GoUnhandledErrorResult
	defer fa.Close()

	fb, err := os.Open(pathB)
	if err != nil {
		return false, err
	}
	//goland:noinspection GoUnhandledErrorResult
	defer fb.Close()

	const chunkSize = 256 * 1024
	bufA := make([]byte, chunkSize)
	bufB := make([]byte, chunkSize)

	for {
		nA, errA := io.ReadFull(fa, bufA)
		nB, errB := io.ReadFull(fb, bufB)

		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return false, nil
		}

		eofA := isEOF(errA)
		eofB := isEOF(errB)

		if eofA && eofB {
			return true, nil
		}
		if eofA != eofB {
			return false, nil
		}
		if errA != nil {
			return false, errA
		}
		if errB != nil {
			return false, errB
		}
	}
}

func isEOF(err error) bool {
	return err == io.EOF || err == io.ErrUnexpectedEOF
}

// dedupFile replaces dst with a reflink copy of src, preserving dst's metadata.
// On failure, the original file is restored from a temporary backup.
func dedupFile(src, dst string) error {
	tmpPath := dst + ".dedup-tmp"

	// Capture dst metadata before touching anything.
	dstInfo, err := os.Lstat(dst)
	if err != nil {
		return fmt.Errorf("stat dst: %w", err)
	}

	// Step 1: move dst out of the way.
	if err := os.Rename(dst, tmpPath); err != nil {
		return fmt.Errorf("rename to tmp: %w", err)
	}

	//goland:noinspection GoUnhandledErrorResult
	rollback := func() {
		os.Remove(dst)
		os.Rename(tmpPath, dst)
	}

	// Step 2: create reflink copy of src at dst.
	if err := reflinkCopy(src, dst, dstInfo.Mode()); err != nil {
		rollback()
		return fmt.Errorf("reflink copy: %w", err)
	}

	// Step 3: verify the new file shares extents with src.
	srcExtents, err := getExtents(src)
	if err != nil {
		rollback()
		return fmt.Errorf("verify src extents: %w", err)
	}
	dstExtents, err := getExtents(dst)
	if err != nil {
		rollback()
		return fmt.Errorf("verify dst extents: %w", err)
	}
	if !SameExtents(srcExtents, dstExtents) {
		rollback()
		return fmt.Errorf("extents mismatch after reflink (filesystem may not support reflinks)")
	}

	// Step 4: restore original file metadata on the new file.
	if err := restoreMetadata(dst, dstInfo); err != nil {
		slog.Debug("metadata restoration partial", "path", dst, "error", err)
	}

	// Step 5: success — remove the backup.
	//goland:noinspection GoUnhandledErrorResult
	os.Remove(tmpPath)
	return nil
}
