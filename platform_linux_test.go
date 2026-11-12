//go:build linux

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSameInode(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, []byte("same inode test"), 0644); err != nil {
		t.Fatalf("write a: %v", err)
	}

	// A hard link shares the inode.
	link := filepath.Join(dir, "link")
	if err := os.Link(a, link); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	same, err := sameInode(a, link)
	if err != nil {
		t.Fatalf("sameInode on hard link: %v", err)
	}
	if !same {
		t.Error("hard-linked paths should resolve to the same inode")
	}

	// Two separate files do not.
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(b, []byte("same inode test"), 0644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	same, err = sameInode(a, b)
	if err != nil {
		t.Fatalf("sameInode on different files: %v", err)
	}
	if same {
		t.Error("two different files should not resolve to the same inode")
	}

	// A missing path is an error, in either argument position.
	if _, err := sameInode(a, filepath.Join(dir, "missing")); err == nil {
		t.Error("sameInode with a missing second path should error")
	}
	if _, err := sameInode(filepath.Join(dir, "missing"), a); err == nil {
		t.Error("sameInode with a missing first path should error")
	}
}

func TestRestoreMetadata(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "file")
	if err := os.WriteFile(p, []byte("metadata"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	orig, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Change both mode and mtime.
	if err := os.Chmod(p, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	shifted := orig.ModTime().Add(-2 * time.Hour)
	if err := os.Chtimes(p, shifted, shifted); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := restoreMetadata(p, orig); err != nil {
		t.Fatalf("restoreMetadata: %v", err)
	}

	after, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat after restore: %v", err)
	}
	if after.Mode() != orig.Mode() {
		t.Errorf("mode not restored: got %v, want %v", after.Mode(), orig.Mode())
	}
	if !after.ModTime().Equal(orig.ModTime()) {
		t.Errorf("mtime not restored: got %v, want %v", after.ModTime(), orig.ModTime())
	}
}

func TestIsMountPoint(t *testing.T) {
	if !isMountPoint("/") {
		t.Error("filesystem root should be a mount point")
	}
	if isMountPoint(t.TempDir()) {
		t.Error("a fresh t.TempDir() should not be a mount point")
	}
}

// probeFICLONE issues a raw FICLONE between two scratch files in dir and
// returns nil when the filesystem supports it, or the errno it failed with.
func probeFICLONE(t *testing.T, dir string) error {
	t.Helper()
	src := filepath.Join(dir, "probe-src")
	dst := filepath.Join(dir, "probe-dst")
	if err := os.WriteFile(src, []byte("ficlone probe"), 0644); err != nil {
		t.Fatalf("write probe source: %v", err)
	}
	s, err := os.Open(src)
	if err != nil {
		t.Fatalf("open probe source: %v", err)
	}
	defer func() { _ = s.Close() }()
	d, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("create probe destination: %v", err)
	}
	defer func() { _ = d.Close() }()
	return unix.IoctlFileClone(int(d.Fd()), int(s.Fd()))
}

func TestReflink(t *testing.T) {
	dir := t.TempDir()
	content := []byte("reflink payload that must survive the clone")
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	probeErr := probeFICLONE(t, dir)
	if probeErr != nil {
		t.Logf("FICLONE unsupported on %s (%v); asserting the error contract", dir, probeErr)
		// Unsupported filesystem: both wrappers must surface the probe's
		// errno behind the FICLONE error text.
		err := reflinkCopy(src, filepath.Join(dir, "copy.bin"), 0644)
		if err == nil {
			t.Fatal("reflinkCopy should fail where FICLONE is unsupported")
		}
		if !strings.Contains(err.Error(), "FICLONE ioctl") {
			t.Errorf("reflinkCopy error %q should contain \"FICLONE ioctl\"", err)
		}
		if !errors.Is(err, probeErr) {
			t.Errorf("reflinkCopy error %v should wrap probe errno %v", err, probeErr)
		}

		inPlace := filepath.Join(dir, "inplace.bin")
		if err := os.WriteFile(inPlace, []byte("old content"), 0644); err != nil {
			t.Fatalf("write in-place destination: %v", err)
		}
		err = reflinkInPlace(src, inPlace)
		if err == nil {
			t.Fatal("reflinkInPlace should fail where FICLONE is unsupported")
		}
		if !strings.Contains(err.Error(), "FICLONE ioctl") {
			t.Errorf("reflinkInPlace error %q should contain \"FICLONE ioctl\"", err)
		}
		if !errors.Is(err, probeErr) {
			t.Errorf("reflinkInPlace error %v should wrap probe errno %v", err, probeErr)
		}
		return
	}

	// Supported filesystem: the clone must carry the content over, and
	// reflinkCopy must create the destination with the requested permission
	// bits (umask cleared so the assertion is exact).
	const perm os.FileMode = 0640
	copyPath := filepath.Join(dir, "copy.bin")
	oldMask := unix.Umask(0)
	err := reflinkCopy(src, copyPath, perm)
	unix.Umask(oldMask)
	if err != nil {
		t.Fatalf("reflinkCopy: %v", err)
	}
	got, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("reflinkCopy content: got %q, want %q", got, content)
	}
	info, err := os.Stat(copyPath)
	if err != nil {
		t.Fatalf("stat copy: %v", err)
	}
	if info.Mode().Perm() != perm {
		t.Errorf("reflinkCopy permission bits: got %o, want %o", info.Mode().Perm(), perm)
	}

	inPlace := filepath.Join(dir, "inplace.bin")
	if err := os.WriteFile(inPlace, []byte("stale content that the clone replaces entirely"), 0644); err != nil {
		t.Fatalf("write in-place destination: %v", err)
	}
	if err := reflinkInPlace(src, inPlace); err != nil {
		t.Fatalf("reflinkInPlace: %v", err)
	}
	got, err = os.ReadFile(inPlace)
	if err != nil {
		t.Fatalf("read in-place destination: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("reflinkInPlace content: got %q, want %q", got, content)
	}
}

func TestGetExtents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mapped.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte("e"), 4096), 0644); err != nil {
		t.Fatalf("write mapped file: %v", err)
	}

	extents, err := getExtents(p)
	if err != nil {
		// Some filesystems do not implement FIEMAP (e.g. ZFS); the failure
		// must still be reported with the FIEMAP error prefix.
		if strings.HasPrefix(err.Error(), "FIEMAP ioctl on") {
			t.Logf("FIEMAP unavailable on %s (%v); asserting the error contract", dir, err)
			return
		}
		t.Fatalf("getExtents: %v", err)
	}
	if len(extents) == 0 {
		t.Error("a freshly written non-empty file should have at least one extent")
	}
	t.Logf("getExtents returned %d extent(s)", len(extents))
}
