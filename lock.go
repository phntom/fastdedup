package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"syscall"
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
	os.MkdirAll(lockDir, 0755)

	h := fnv.New64a()
	h.Write([]byte(root))
	lockPath := filepath.Join(lockDir, fmt.Sprintf("%016x.lock", h.Sum64()))

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// releaseLock releases the flock and closes the file.
func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}
