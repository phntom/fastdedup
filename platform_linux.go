//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	_FS_IOC_FIEMAP      = 0xC020660B
	_FIEMAP_FLAG_SYNC   = 0x00000001
	_FIEMAP_EXTENT_LAST = 0x00000001
	_MAX_FIEMAP_EXTENTS = 512
	_FICLONE            = 0x40049409
)

// Raw kernel structs for FIEMAP ioctl. Field order and sizes must match
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
	extents       [_MAX_FIEMAP_EXTENTS]fiemapExtent
}

// getExtents returns the physical extent map of a file using the FIEMAP ioctl.
func getExtents(path string) ([]Extent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var all []Extent
	var start uint64

	for {
		req := fiemapReq{
			start:       start,
			length:      ^uint64(0),
			flags:       _FIEMAP_FLAG_SYNC,
			extentCount: _MAX_FIEMAP_EXTENTS,
		}

		_, _, errno := unix.Syscall(
			unix.SYS_IOCTL,
			f.Fd(),
			uintptr(_FS_IOC_FIEMAP),
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
		if last.flags&_FIEMAP_EXTENT_LAST != 0 {
			break
		}
		start = last.logical + last.length
	}

	return all, nil
}

// reflinkCopy creates a reflink (CoW) copy of src at dst. The new file shares
// the same physical data blocks as src. Only works on btrfs/XFS with reflink.
func reflinkCopy(src, dst string, perm os.FileMode) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	defer dstFile.Close()

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		dstFile.Fd(),
		uintptr(_FICLONE),
		srcFile.Fd(),
	)
	if errno != 0 {
		return fmt.Errorf("FICLONE ioctl: %w", errno)
	}

	return nil
}

// sameInode reports whether two paths refer to the same inode on the same device.
func sameInode(a, b string) (bool, error) {
	var statA, statB syscall.Stat_t
	if err := syscall.Stat(a, &statA); err != nil {
		return false, err
	}
	if err := syscall.Stat(b, &statB); err != nil {
		return false, err
	}
	return statA.Dev == statB.Dev && statA.Ino == statB.Ino, nil
}

// restoreMetadata copies ownership, permissions, and timestamps from the
// original file info onto the new file at path.
func restoreMetadata(path string, orig os.FileInfo) error {
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
	atime := time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
	mtime := time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
	if err := os.Chtimes(path, atime, mtime); err != nil {
		return fmt.Errorf("chtimes: %w", err)
	}

	return nil
}
