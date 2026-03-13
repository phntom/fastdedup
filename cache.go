package main

import (
	"encoding/gob"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
)

// cachePath returns the OS-appropriate cache file for the given root directory.
func cachePath(root string) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		// Fall back to Abs if symlink resolution fails (e.g. broken symlink).
		absRoot, err = filepath.Abs(root)
		if err != nil {
			return "", err
		}
	}
	h := fnv.New64a()
	h.Write([]byte(absRoot))
	return filepath.Join(dir, "fastdedup", fmt.Sprintf("%016x.gob", h.Sum64())), nil
}

// loadCache reads a filename-hash cache from disk. Returns an empty map on any error.
func loadCache(path string) map[int64]uint64 {
	f, err := os.Open(path)
	if err != nil {
		return make(map[int64]uint64)
	}
	defer f.Close()
	var hashes map[int64]uint64
	if err := gob.NewDecoder(f).Decode(&hashes); err != nil {
		return make(map[int64]uint64)
	}
	return hashes
}

// saveCache writes the filename-hash cache to disk.
func saveCache(path string, hashes map[int64]uint64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return gob.NewEncoder(f).Encode(hashes)
}

// hashFilename returns a 64-bit FNV-1a hash of a filename.
func hashFilename(name string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(name))
	return h.Sum64()
}
