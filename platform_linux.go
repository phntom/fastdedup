//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// FIEMAP constants (linux/fiemap.h). golang.org/x/sys/unix has no FIEMAP
// wrapper, constants or types, so the ioctl below is issued as a raw syscall
// against these hand-written values and structs.
const (
	// fsIocFiemap is the request number of FS_IOC_FIEMAP,
	// _IOWR('f', 11, struct fiemap) = 0xC020660B. Unlike FICLONE, this
	// request number is identical on every architecture we ship, so a single
	// hard-coded value is safe here.
	fsIocFiemap = 0xC020660B
	// fiemapFlagSync is FIEMAP_FLAG_SYNC: sync the file to disk before
	// mapping its extents.
	fiemapFlagSync = 0x00000001
	// fiemapExtentLast is FIEMAP_EXTENT_LAST: this is the final extent in
	// the file.
	fiemapExtentLast = 0x00000001
	// maxFiemapExtents is how many extents a single FIEMAP request fetches
	// before the loop must continue from the last extent. Not a kernel
	// constant — our buffer size.
	maxFiemapExtents = 512
)

// Raw kernel structs for the FIEMAP ioctl. Field order and sizes must match
// the C definitions exactly (linux/fiemap.h).

type fiemapExtent struct {
	logical    uint64
	physical   uint64
	length     uint64
	reserved64 [2]uint64
	flags      uint32
	reserved32 [3]uint32
}

type fiemapReq struct {
	start         uint64
	length        uint64
	flags         uint32
	mappedExtents uint32
	extentCount   uint32
	reserved      uint32
	extents       [maxFiemapExtents]fiemapExtent
}

// Compile-time layout assertions. unsafe.Sizeof and unsafe.Offsetof are
// constant expressions, so the array types below only match when the Go
// struct layout equals the C ABI — a layout mistake breaks the build instead
// of corrupting data at run time.
var (
	// struct fiemap_extent is exactly 56 bytes.
	_ [unsafe.Sizeof(fiemapExtent{})]struct{} = [56]struct{}{}
	// The request header before the extents array (start, length, flags,
	// mapped_extents, extent_count, reserved) is 32 bytes — the C
	// offsetof(struct fiemap, fm_extents).
	_ [unsafe.Offsetof(fiemapReq{}.extents)]struct{} = [32]struct{}{}
)

// getExtents returns the physical extent map of a file using the FIEMAP ioctl.
func getExtents(path string) ([]Extent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	var all []Extent
	var start uint64

	for {
		req := fiemapReq{
			start:       start,
			length:      ^uint64(0),
			flags:       fiemapFlagSync,
			extentCount: maxFiemapExtents,
		}

		_, _, errno := unix.Syscall(
			unix.SYS_IOCTL,
			f.Fd(),
			uintptr(fsIocFiemap),
			uintptr(unsafe.Pointer(&req)),
		)
		if errno != 0 {
			return nil, fmt.Errorf("FIEMAP ioctl on %s: %w", path, errno)
		}

		if req.mappedExtents == 0 {
			break
		}

		for i := uint32(0); i < req.mappedExtents; i++ {
			e := req.extents[i]
			all = append(all, Extent{
				Logical:  e.logical,
				Physical: e.physical,
				Length:   e.length,
				Flags:    e.flags,
			})
		}

		last := req.extents[req.mappedExtents-1]
		if last.flags&fiemapExtentLast != 0 {
			break
		}
		start = last.logical + last.length
	}

	return all, nil
}

// reflinkCopy creates a reflink (CoW) copy of src at dst. The new file shares
// the same physical data blocks as src. Only works on btrfs/XFS with reflink.
func reflinkCopy(src, dst string, perm os.FileMode) error {
	return reflinkClone(src, func() (*os.File, error) {
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
		if err != nil {
			return nil, fmt.Errorf("create destination: %w", err)
		}
		return f, nil
	})
}

// reflinkInPlace replaces the content of dst with a reflink clone of src,
// without creating or removing directory entries. The existing dst inode is
// truncated and FICLONE'd in place, so this works on write-protected directories.
func reflinkInPlace(src, dst string) error {
	return reflinkClone(src, func() (*os.File, error) {
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return nil, fmt.Errorf("open destination: %w", err)
		}
		return f, nil
	})
}

// reflinkClone clones the entire file at src into the destination opened by
// openDst. reflinkCopy creates a fresh destination file with the requested
// permissions; reflinkInPlace truncates the existing one in place.
//
// The FICLONE ioctl goes through unix.IoctlFileClone because the ioctl
// request number is architecture-dependent: the direction bit that the old
// hand-rolled constant baked in is flipped on mips64le and ppc64le, where
// ioctl request numbers encode the transfer direction differently, so
// reflinking could never have worked there. The x/sys package carries the
// correct per-architecture value.
func reflinkClone(src string, openDst func() (*os.File, error)) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer srcFile.Close() //nolint:errcheck // read-only file

	dstFile, err := openDst()
	if err != nil {
		return err
	}

	if err := unix.IoctlFileClone(int(dstFile.Fd()), int(srcFile.Fd())); err != nil {
		// The ioctl error is the meaningful one; closing is best-effort.
		_ = dstFile.Close()
		return fmt.Errorf("FICLONE ioctl: %w", err)
	}

	// The clone succeeded, so the destination must be closed cleanly; a
	// failure there (e.g. NFS commit errors) would otherwise be dropped.
	if err := dstFile.Close(); err != nil {
		return fmt.Errorf("close destination: %w", err)
	}

	return nil
}

// isMountPoint checks whether path is a filesystem mount point by comparing
// device IDs with the parent directory.
func isMountPoint(path string) bool {
	var pathStat, parentStat unix.Stat_t
	if err := unix.Stat(path, &pathStat); err != nil {
		return false
	}
	parent := filepath.Dir(path)
	if parent == path {
		return true // filesystem root
	}
	if err := unix.Stat(parent, &parentStat); err != nil {
		return false
	}
	return pathStat.Dev != parentStat.Dev
}

// fsFileEstimate returns the approximate number of files on the filesystem
// containing path, using statfs (total inodes - free inodes).
func fsFileEstimate(path string) int64 {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0
	}
	used := int64(stat.Files) - int64(stat.Ffree)
	if used <= 0 {
		return 0
	}
	return used
}

// fsUsedBytes returns the number of bytes used on the filesystem containing path.
func fsUsedBytes(path string) int64 {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0
	}
	total := int64(stat.Blocks) * int64(stat.Bsize)
	free := int64(stat.Bfree) * int64(stat.Bsize)
	used := total - free
	if used <= 0 {
		return 0
	}
	return used
}

// sameInode reports whether two paths refer to the same inode on the same device.
func sameInode(a, b string) (bool, error) {
	infoA, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(infoA, infoB), nil
}

// restoreMetadata copies ownership, permissions, and timestamps from the
// original file info onto the new file at path.
func restoreMetadata(path string, orig os.FileInfo) error {
	// The os package on Linux exposes the raw stat from the frozen syscall
	// package; x/sys defines its own Stat_t type, so this assertion is the
	// one place the syscall package must stay.
	stat, ok := orig.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("unexpected stat type for metadata restoration")
	}

	// Ownership (best-effort; may require root).
	_ = os.Chown(path, int(stat.Uid), int(stat.Gid))

	// Permissions.
	if err := os.Chmod(path, orig.Mode()); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// Timestamps.
	atime := time.Unix(int64(stat.Atim.Sec), int64(stat.Atim.Nsec))
	mtime := time.Unix(int64(stat.Mtim.Sec), int64(stat.Mtim.Nsec))
	if err := os.Chtimes(path, atime, mtime); err != nil {
		return fmt.Errorf("chtimes: %w", err)
	}

	return nil
}
