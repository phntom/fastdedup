//go:build unix

package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// acquireLock takes a non-blocking exclusive flock on a per-root lock file.
// Returns the open file (caller must defer releaseLock) or an error if locked.
func acquireLock(root string) (*os.File, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		// Fall back to /tmp if no cache dir.
		dir = os.TempDir()
	}
	lockDir := filepath.Join(dir, "fastdedup")
	// Best-effort: if creating the directory fails, the OpenFile below
	// reports the real problem.
	_ = os.MkdirAll(lockDir, 0755)

	h := fnv.New64a()
	h.Write([]byte(root))
	lockPath := filepath.Join(lockDir, fmt.Sprintf("%016x.lock", h.Sum64()))

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// releaseLock releases the flock and closes the file. Both steps are
// best-effort: the process is on its way out and nothing could act on a
// failure here anyway.
func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	_ = f.Close()
}
