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

// saveCache atomically writes the filename-hash cache to disk.
// It writes to a temporary file first, then renames, so a Ctrl+C
// mid-write never corrupts the existing cache.
func saveCache(path string, hashes map[int64]uint64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(hashes); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// metaPath returns the path for the scan metadata cache file.
func metaPath(cacheFile string) string {
	return cacheFile[:len(cacheFile)-len(filepath.Ext(cacheFile))] + ".meta"
}

// ScanMeta holds metadata from a previous scan for progress estimation.
type ScanMeta struct {
	FileCount int64
}

// loadMeta reads scan metadata from disk. Returns nil on any error.
func loadMeta(path string) *ScanMeta {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var meta ScanMeta
	if err := gob.NewDecoder(f).Decode(&meta); err != nil {
		return nil
	}
	return &meta
}

// saveMeta atomically writes scan metadata to disk.
func saveMeta(path string, meta *ScanMeta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(meta); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// hashFilename returns a 64-bit FNV-1a hash of a filename.
func hashFilename(name string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(name))
	return h.Sum64()
}
