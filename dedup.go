package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
// Paths are stored compactly with interned directory strings via the provided DirIntern.
// If pool is nil, a temporary pool is created (no cross-call sharing).
// The optional onMatch callback is called for each file matching a target size.
func CollectFiles(root string, targetSet map[int64]struct{}, includeSnapshots bool, minSize int64, pool *DirIntern, onMatch func()) (map[int64][]CompactPath, error) {
	if pool == nil {
		pool = NewDirIntern()
	}
	result := make(map[int64][]CompactPath)
	err := walkRandom(root, includeSnapshots, minSize, func(path string, size int64) {
		if _, ok := targetSet[size]; ok {
			dir, name := filepath.Dir(path), filepath.Base(path)
			iDir, _ := pool.Intern(dir)
			result[size] = append(result[size], CompactPath{Dir: iDir, Name: name})
			if onMatch != nil {
				onMatch()
			}
		}
	})
	return result, err
}

// ProcessSizeGroup deduplicates all files of a single size, returning stats.
// The optional onProgress callback is called with the 1-based index of each file processed.
//
// Files are compared against a growing list of reference files (one per unique
// content group). When content matches but the dedup operation fails (e.g.
// permissions, cross-device), the file is tried against remaining refs. If all
// matching refs fail, the file is added as an alternative ref so future files
// can dedup against it instead.
func ProcessSizeGroup(paths []string, size int64, dryRun bool, verbose bool, rawSizes bool, hardlink bool, fixPerms bool, onProgress func(current int)) *DedupStats {
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

		deduped := false
		contentMatch := false
		dedupErrors := 0
		for _, ref := range refs {
			// Same inode (hard link) — already sharing storage.
			if same, _ := sameInode(ref.path, path); same {
				if len(path) < len(ref.path) {
					ref.path = path
					ref.extents = extents
				}
				stats.AlreadyDeduped++
				deduped = true
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
				deduped = true
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

			// Identical content found.
			contentMatch = true

			if dryRun {
				fmt.Printf("[dry-run] dedup: %s -> %s (%s)\n", path, ref.path, formatSize(size, rawSizes))
				stats.BytesSaved += size
				stats.FilesDeduped++
				deduped = true
				break
			}

			var dedupErr error
			if hardlink {
				dedupErr = hardlinkFile(ref.path, path, fixPerms)
			} else {
				dedupErr = dedupFile(ref.path, path, fixPerms)
			}
			if dedupErr != nil {
				slog.Debug("dedup failed, trying next ref", "src", ref.path, "dst", path, "error", dedupErr)
				dedupErrors++
				continue // try next ref — another ref with same content may work
			}

			if verbose {
				fmt.Fprintf(os.Stderr, "    %s -> %s\n", path, ref.path)
			}
			slog.Debug("deduped", "file", path, "ref", ref.path, "size", size)
			stats.BytesSaved += size
			stats.FilesDeduped++
			deduped = true
			break
		}

		if !deduped {
			if contentMatch {
				// Content matched a ref but all dedup attempts failed.
				// Add this file as an alternative ref — it may succeed as
				// source where the original ref could not.
				stats.Errors++
				slog.Debug("all dedup attempts failed for content match, adding as alternative ref",
					"path", path, "attempts", dedupErrors)
			}
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
func hardlinkFile(src, dst string, fixPerms bool) error {
	tmpPath := dst + ".dedup-tmp"

	// Step 1: move dst out of the way, temporarily fixing directory permissions if needed.
	renameErr := os.Rename(dst, tmpPath)
	var restoreDir func()
	if renameErr != nil && fixPerms {
		if restore, chErr := addDirWrite(filepath.Dir(dst)); chErr == nil {
			if os.Rename(dst, tmpPath) == nil {
				renameErr = nil
				restoreDir = restore
				slog.Debug("temporarily added write permission to directory", "dir", filepath.Dir(dst))
			} else {
				restore()
			}
		}
	}
	if renameErr != nil {
		return fmt.Errorf("rename to tmp: %w", renameErr)
	}
	if restoreDir != nil {
		defer restoreDir()
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
// If the directory is write-protected, it falls back to an in-place reflink
// with a backup in the system temp directory.
func dedupFile(src, dst string, fixPerms bool) error {
	tmpPath := dst + ".dedup-tmp"

	// Capture dst metadata before touching anything.
	dstInfo, err := os.Lstat(dst)
	if err != nil {
		return fmt.Errorf("stat dst: %w", err)
	}

	// Step 1: move dst out of the way, temporarily fixing directory permissions if needed.
	renameErr := os.Rename(dst, tmpPath)
	var restoreDir func()
	if renameErr != nil && fixPerms {
		if restore, chErr := addDirWrite(filepath.Dir(dst)); chErr == nil {
			if os.Rename(dst, tmpPath) == nil {
				renameErr = nil
				restoreDir = restore
				slog.Debug("temporarily added write permission to directory", "dir", filepath.Dir(dst))
			} else {
				restore()
			}
		}
	}
	if renameErr != nil {
		// Directory may be write-protected; fall back to in-place reflink.
		slog.Debug("rename failed, trying in-place reflink", "dst", dst, "error", renameErr)
		return dedupFileInPlace(src, dst, dstInfo, fixPerms)
	}
	if restoreDir != nil {
		defer restoreDir()
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
	if err := verifyReflink(src, dst); err != nil {
		rollback()
		return err
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

// dedupFileInPlace performs a reflink by truncating and cloning into the existing
// dst inode, avoiding any directory entry changes. A content backup is kept in
// the system temp directory for rollback on failure.
func dedupFileInPlace(src, dst string, dstInfo os.FileInfo, fixPerms bool) error {
	// Back up dst content to a temp file.
	backupPath, err := backupToTemp(dst)
	if err != nil {
		return fmt.Errorf("backup for in-place dedup: %w", err)
	}
	//goland:noinspection GoUnhandledErrorResult
	defer os.Remove(backupPath)

	// If the file is read-only and --fix-perms is set, temporarily make it writable.
	origMode := dstInfo.Mode().Perm()
	if fixPerms && origMode&0200 == 0 {
		if err := os.Chmod(dst, origMode|0200); err != nil {
			slog.Debug("cannot chmod file writable", "path", dst, "error", err)
		} else {
			defer os.Chmod(dst, origMode)
		}
	}

	// Reflink in-place: truncate dst and FICLONE from src.
	if err := reflinkInPlace(src, dst); err != nil {
		restoreFromTemp(backupPath, dst)
		return fmt.Errorf("in-place reflink: %w", err)
	}

	// Verify.
	if err := verifyReflink(src, dst); err != nil {
		restoreFromTemp(backupPath, dst)
		return err
	}

	// Restore metadata (ownership, permissions, timestamps).
	if err := restoreMetadata(dst, dstInfo); err != nil {
		slog.Debug("metadata restoration partial", "path", dst, "error", err)
	}

	return nil
}

// verifyReflink checks that src and dst share the same data after a reflink.
func verifyReflink(src, dst string) error {
	srcExtents, errSrc := getExtents(src)
	dstExtents, errDst := getExtents(dst)
	if errSrc == nil && errDst == nil {
		if !SameExtents(srcExtents, dstExtents) {
			return fmt.Errorf("extents mismatch after reflink (filesystem may not support reflinks)")
		}
		return nil
	}
	// FIEMAP not available (e.g. ZFS) — verify content instead.
	equal, err := filesEqual(src, dst)
	if err != nil {
		return fmt.Errorf("verify content after reflink: %w", err)
	}
	if !equal {
		return fmt.Errorf("content mismatch after reflink")
	}
	return nil
}

// backupToTemp copies the content of path into a temporary file and returns
// the temp file path. The caller must remove the temp file when done.
func backupToTemp(path string) (string, error) {
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()

	tmp, err := os.CreateTemp("", "dedup-backup-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

// restoreFromTemp copies content from a temp backup back into dst (in-place).
//
//goland:noinspection GoUnhandledErrorResult
func restoreFromTemp(backupPath, dst string) {
	src, err := os.Open(backupPath)
	if err != nil {
		return
	}
	defer src.Close()

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return
	}
	defer dstFile.Close()

	io.Copy(dstFile, src)
}

// addDirWrite temporarily adds owner-write permission to a directory.
// It returns a restore function that restores the original permissions.
func addDirWrite(dir string) (restore func(), err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	origMode := info.Mode().Perm()
	if origMode&0200 != 0 {
		// Already writable — nothing to do.
		return func() {}, nil
	}
	if err := os.Chmod(dir, origMode|0200); err != nil {
		return nil, err
	}
	return func() {
		//goland:noinspection GoUnhandledErrorResult
		os.Chmod(dir, origMode)
	}, nil
}
