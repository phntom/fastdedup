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

// CollectFiles walks the tree once and returns file paths grouped by target size.
// The optional onMatch callback is called for each file matching a target size.
func CollectFiles(root string, targetSet map[int64]struct{}, includeSnapshots bool, minSize int64, onMatch func()) (map[int64][]string, error) {
	result := make(map[int64][]string)
	err := walkRandom(root, includeSnapshots, minSize, func(path string, size int64) {
		if _, ok := targetSet[size]; ok {
			result[size] = append(result[size], path)
			if onMatch != nil {
				onMatch()
			}
		}
	})
	return result, err
}

// ProcessSizeGroup deduplicates all files of a single size, returning stats.
// The optional onProgress callback is called with the 1-based index of each file processed.
func ProcessSizeGroup(paths []string, size int64, dryRun bool, verbose bool, rawSizes bool, hardlink bool, onProgress func(current int)) *DedupStats {
	stats := &DedupStats{}
	var refs []*fileRef

	for i, path := range paths {
		if onProgress != nil {
			onProgress(i + 1)
		}

		extents, err := getExtents(path)
		if err != nil {
			slog.Debug("cannot get extents (will use content comparison)", "path", path, "error", err)
		}

		// First file — establish as reference.
		if len(refs) == 0 {
			refs = append(refs, &fileRef{path: path, extents: extents})
			continue
		}

		matched := false
		for _, ref := range refs {
			// Same inode (hard link) — already sharing storage.
			if same, _ := sameInode(ref.path, path); same {
				if len(path) < len(ref.path) {
					ref.path = path
					ref.extents = extents
				}
				stats.AlreadyDeduped++
				matched = true
				break
			}

			// Same extents (existing reflink) — already sharing storage.
			// Only check when both sides have valid extents.
			if extents != nil && ref.extents != nil && SameExtents(ref.extents, extents) {
				if len(path) < len(ref.path) {
					ref.path = path
					ref.extents = extents
				}
				stats.AlreadyDeduped++
				matched = true
				break
			}

			// Compare file content byte-by-byte.
			equal, err := filesEqual(ref.path, path)
			if err != nil {
				slog.Debug("content comparison failed", "a", ref.path, "b", path, "error", err)
				continue
			}
			if !equal {
				continue
			}

			// Identical content — deduplicate!
			if dryRun {
				fmt.Printf("[dry-run] dedup: %s -> %s (%s)\n", path, ref.path, formatSize(size, rawSizes))
				stats.BytesSaved += size
				stats.FilesDeduped++
				matched = true
				break
			}

			var dedupErr error
			if hardlink {
				dedupErr = hardlinkFile(ref.path, path)
			} else {
				dedupErr = dedupFile(ref.path, path)
			}
			if dedupErr != nil {
				slog.Debug("dedup failed", "src", ref.path, "dst", path, "error", dedupErr)
				stats.Errors++
				matched = true
				break
			}

			if verbose {
				fmt.Fprintf(os.Stderr, "    %s -> %s\n", path, ref.path)
			}
			slog.Debug("deduped", "file", path, "ref", ref.path, "size", size)
			stats.BytesSaved += size
			stats.FilesDeduped++
			matched = true
			break
		}

		if !matched {
			// New content group for this size.
			refs = append(refs, &fileRef{path: path, extents: extents})
		}
	}

	return stats
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

// hardlinkFile replaces dst with a hard link to src.
// On failure, the original file is restored from a temporary backup.
func hardlinkFile(src, dst string) error {
	tmpPath := dst + ".dedup-tmp"

	// Step 1: move dst out of the way.
	if err := os.Rename(dst, tmpPath); err != nil {
		return fmt.Errorf("rename to tmp: %w", err)
	}

	//goland:noinspection GoUnhandledErrorResult
	rollback := func() {
		os.Remove(dst)
		os.Rename(tmpPath, dst)
	}

	// Step 2: create hard link from src to dst.
	if err := os.Link(src, dst); err != nil {
		rollback()
		return fmt.Errorf("hard link: %w", err)
	}

	// Step 3: verify they share the same inode.
	if same, err := sameInode(src, dst); err != nil || !same {
		rollback()
		return fmt.Errorf("hard link verification failed")
	}

	// Step 4: success — remove the backup.
	//goland:noinspection GoUnhandledErrorResult
	os.Remove(tmpPath)
	return nil
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

	// Step 3: verify the new file shares extents with src (when FIEMAP is available).
	srcExtents, errSrc := getExtents(src)
	dstExtents, errDst := getExtents(dst)
	if errSrc == nil && errDst == nil {
		if !SameExtents(srcExtents, dstExtents) {
			rollback()
			return fmt.Errorf("extents mismatch after reflink (filesystem may not support reflinks)")
		}
	} else {
		// FIEMAP not available (e.g. ZFS) — verify content instead.
		equal, err := filesEqual(src, dst)
		if err != nil {
			rollback()
			return fmt.Errorf("verify content after reflink: %w", err)
		}
		if !equal {
			rollback()
			return fmt.Errorf("content mismatch after reflink")
		}
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
