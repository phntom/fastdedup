package main

import (
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
)

// WalkSizes traverses the directory tree rooted at root, recording each
// regular file's size in the SizeMap. Symlinks are ignored. Directory
// entry order is randomized so repeated runs explore different parts of
// the tree before the bounded map fills up.
func WalkSizes(root string, sm *SizeMap) (int64, error) {
	var count int64
	err := walkRandom(root, func(_ string, size int64) {
		sm.Add(size)
		count++
		if count%1_000_000 == 0 {
			slog.Debug("pass 1 progress", "files", count, "unique_sizes", sm.Len())
		}
	})
	return count, err
}

// walkRandom recursively walks the directory tree at dir, calling fn for
// each regular file found. Directory entries are shuffled to randomize
// traversal order. Symlinks, special files, and empty files are skipped.
// Errors reading individual directories are logged and skipped.
func walkRandom(dir string, fn func(path string, size int64)) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Debug("skipping unreadable directory", "path", dir, "error", err)
		return nil
	}

	rand.Shuffle(len(entries), func(i, j int) {
		entries[i], entries[j] = entries[j], entries[i]
	})

	for _, entry := range entries {
		// Skip symlinks entirely.
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		if entry.IsDir() {
			_ = walkRandom(path, fn)
			continue
		}

		if !entry.Type().IsRegular() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			slog.Debug("skipping unreadable file", "path", path, "error", err)
			continue
		}

		if info.Size() == 0 {
			continue
		}

		fn(path, info.Size())
	}

	return nil
}
