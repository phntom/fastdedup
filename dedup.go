package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
	ErrorDetails   []DedupError
}

// fileRef is a reference file representing a unique content group within a size class.
type fileRef struct {
	path    string
	extents []Extent
	links   int // files deduped onto this reference during the current run
}

// storageStat returns a hashable identity of the file's storage (device and
// inode numbers) together with its hard-link count. ok is false when the
// platform's stat wrapper exposes no inode fields (e.g. windows) or the file
// cannot be stat'ed; callers must then treat the file as a lone file.
//
// The raw stat data is read through reflection because the concrete types
// behind os.FileInfo.Sys() (syscall.Stat_t and friends) are platform-specific
// and absent on some platforms, so a direct type assertion would not compile
// everywhere fastdedup builds.
func storageStat(path string) (key string, nlink int, ok bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", 0, false
	}
	v := reflect.Indirect(reflect.ValueOf(fi.Sys()))
	if v.Kind() != reflect.Struct {
		return "", 0, false
	}
	ino := statField(v.FieldByName("Ino"))
	dev := statField(v.FieldByName("Dev"))
	if ino == nil || dev == nil {
		return "", 0, false
	}
	key = fmt.Sprintf("%d:%d", *dev, *ino)
	nl := statField(v.FieldByName("Nlink"))
	if nl == nil || *nl > uint64(^uint(0)>>1) {
		return key, 0, false
	}
	return key, int(*nl), true
}

// statField reads a numeric stat field as a uint64, or returns nil when the
// field is missing or not a plain integer.
func statField(f reflect.Value) *uint64 {
	switch f.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v := f.Uint()
		return &v
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v := f.Int()
		if v < 0 {
			return nil
		}
		u := uint64(v)
		return &u
	default:
		return nil
	}
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
//
// A file that is already part of an established storage family (several hard
// links already share its inode) is never treated as a fresh duplicate of a
// lone reference: instead the reference is linked onto the family, so a new
// duplicate joins the existing family inode and exactly one file is counted
// as deduped, regardless of the order files are visited in.
func ProcessSizeGroup(paths []string, size int64, dryRun bool, verbose bool, rawSizes bool, hardlink bool, fixPerms bool, onProgress func(current int)) *DedupStats {
	stats := &DedupStats{}
	var refs []*fileRef
	// family maps the inode identity of a storage family that a reference
	// has been linked onto (see tryJoinFamily) back to that reference, so
	// later members of the family are recognised as already deduped —
	// including in a dry run, where the reference has not physically
	// moved onto the family inode yet.
	family := make(map[string]*fileRef)

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

		// Member of a storage family that a reference already joined:
		// already sharing storage with that reference, nothing to do.
		// (Real runs usually catch this through the sameInode / sameExtents
		// fast paths below; the map is what makes it visible in dry runs.)
		if len(family) > 0 {
			if key, _, ok := storageStat(path); ok {
				if _, joined := family[key]; joined {
					stats.AlreadyDeduped++
					continue
				}
			}
		}

		// tryJoinFamily links the reference file onto the established
		// storage family of path when that is the honest direction: the
		// family is already sharing storage (multiple links) while the
		// reference is a lone file nothing has been deduped onto yet.
		// Linking the family onto the reference instead would migrate
		// every family member onto the new file's inode and over-report
		// the dedup count. It reports whether the reversal was applied
		// (or, in a dry run, simulated) and the error of a failed attempt,
		// in which case the caller falls back to the normal direction.
		tryJoinFamily := func(ref *fileRef) (bool, error) {
			// Only a reference with no dedups of its own can be moved
			// without stranding files already linked onto it.
			if ref.links > 0 {
				return false, nil
			}
			key, fileLinks, ok := storageStat(path)
			if !ok || fileLinks <= 1 {
				return false, nil
			}
			refKey, refLinks, ok := storageStat(ref.path)
			if !ok || refKey == key || refLinks != 1 {
				return false, nil
			}

			if dryRun {
				fmt.Printf("[dry-run] dedup: %s -> %s (%s)\n", ref.path, path, formatSize(size, rawSizes))
			} else {
				var err error
				if hardlink {
					err = hardlinkFile(path, ref.path, fixPerms)
				} else {
					err = dedupFile(path, ref.path, fixPerms)
				}
				if err != nil {
					return false, err
				}
				if verbose {
					fmt.Fprintf(os.Stderr, "    %s -> %s\n", ref.path, path)
				}
			}

			// The reference now (or, in a dry run, would) share the
			// family's storage.
			ref.links++
			if extents != nil {
				ref.extents = extents
			}
			family[key] = ref
			return true, nil
		}

		deduped := false
		contentMatch := false
		dedupErrors := 0
		var firstDedupErr error
		var firstRefPath string
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

			// If the file is already part of an established family,
			// have the reference join it instead of the other way round.
			joined, joinErr := tryJoinFamily(ref)
			if joined {
				slog.Debug("deduped new duplicate onto existing family", "file", ref.path, "family", path, "size", size)
				stats.BytesSaved += size
				stats.FilesDeduped++
				stats.AlreadyDeduped++
				deduped = true
				break
			}
			if joinErr != nil {
				if firstDedupErr == nil {
					firstDedupErr = joinErr
					firstRefPath = ref.path
				}
				slog.Debug("joining existing family failed, trying normal dedup", "src", path, "dst", ref.path, "error", joinErr)
			}

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
				if firstDedupErr == nil {
					firstDedupErr = dedupErr
					firstRefPath = ref.path
				}
				slog.Debug("dedup failed, trying next ref", "src", ref.path, "dst", path, "error", dedupErr)
				dedupErrors++
				continue // try next ref — another ref with same content may work
			}

			if verbose {
				fmt.Fprintf(os.Stderr, "    %s -> %s\n", path, ref.path)
			}
			slog.Debug("deduped", "file", path, "ref", ref.path, "size", size)
			ref.links++
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
				if firstDedupErr != nil {
					mode := "reflink"
					if hardlink {
						mode = "hardlink"
					}
					stats.ErrorDetails = append(stats.ErrorDetails, DedupError{
						Size:    size,
						Mode:    mode,
						Err:     firstDedupErr.Error(),
						SrcPath: firstRefPath,
						DstPath: path,
					})
				}
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
	defer fa.Close() //nolint:errcheck // read-only file

	fb, err := os.Open(pathB)
	if err != nil {
		return false, err
	}
	//goland:noinspection GoUnhandledErrorResult
	defer fb.Close() //nolint:errcheck // read-only file

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
		// Best-effort crash recovery: if restoring the backup fails too,
		// there is nothing further this process could do about it.
		_ = os.Remove(dst)
		_ = os.Rename(tmpPath, dst)
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
	_ = os.Remove(tmpPath)
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
		// Best-effort crash recovery: if restoring the backup fails too,
		// there is nothing further this process could do about it.
		_ = os.Remove(dst)
		_ = os.Rename(tmpPath, dst)
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
	_ = os.Remove(tmpPath)
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
	defer os.Remove(backupPath) //nolint:errcheck // best-effort temp cleanup

	// If the file is read-only and --fix-perms is set, temporarily make it writable.
	origMode := dstInfo.Mode().Perm()
	if fixPerms && origMode&0200 == 0 {
		if err := os.Chmod(dst, origMode|0200); err != nil {
			slog.Debug("cannot chmod file writable", "path", dst, "error", err)
		} else {
			//goland:noinspection GoUnhandledErrorResult
			defer os.Chmod(dst, origMode) //nolint:errcheck // best-effort permission restore
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
	defer src.Close() //nolint:errcheck // read-only file

	tmp, err := os.CreateTemp("", "dedup-backup-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

// restoreFromTemp copies content from a temp backup back into dst (in-place).
// It is crash-recovery code: every step is deliberately best-effort — there
// is no useful error handling left once the original operation already
// failed, so failures are explicitly discarded rather than plumbed through.
//
//goland:noinspection GoUnhandledErrorResult
func restoreFromTemp(backupPath, dst string) {
	src, err := os.Open(backupPath)
	if err != nil {
		return
	}
	defer src.Close() //nolint:errcheck // read-only file

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return
	}
	defer dstFile.Close() //nolint:errcheck // best-effort rollback

	_, _ = io.Copy(dstFile, src)
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
		_ = os.Chmod(dir, origMode)
	}, nil
}
