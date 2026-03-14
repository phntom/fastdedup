package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCachePath(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		dir := t.TempDir()
		a, err := cachePath(dir)
		if err != nil {
			t.Fatal(err)
		}
		b, err := cachePath(dir)
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Errorf("cachePath not deterministic: %q != %q", a, b)
		}
	})

	t.Run("different roots differ", func(t *testing.T) {
		d1 := t.TempDir()
		d2 := t.TempDir()
		a, _ := cachePath(d1)
		b, _ := cachePath(d2)
		if a == b {
			t.Error("different roots should produce different cache paths")
		}
	})

	t.Run("ends with .gob", func(t *testing.T) {
		dir := t.TempDir()
		p, _ := cachePath(dir)
		if !strings.HasSuffix(p, ".gob") {
			t.Errorf("cache path should end with .gob: %q", p)
		}
	})

	t.Run("contains fastdedup dir", func(t *testing.T) {
		dir := t.TempDir()
		p, _ := cachePath(dir)
		if !strings.Contains(p, "fastdedup") {
			t.Errorf("cache path should contain 'fastdedup': %q", p)
		}
	})

	t.Run("symlink resolves to same path", func(t *testing.T) {
		dir := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Skip("symlinks not supported")
		}
		a, _ := cachePath(dir)
		b, _ := cachePath(link)
		if a != b {
			t.Errorf("symlink should resolve to same cache path: %q != %q", a, b)
		}
	})
}

func TestCacheRoundTrip(t *testing.T) {
	t.Run("save and load", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "cache.gob")
		data := map[int64]uint64{100: 42, 200: 99, 300: 1}
		if err := saveCache(p, data); err != nil {
			t.Fatal(err)
		}
		loaded := loadCache(p)
		if len(loaded) != len(data) {
			t.Fatalf("loaded %d entries, want %d", len(loaded), len(data))
		}
		for k, v := range data {
			if loaded[k] != v {
				t.Errorf("loaded[%d] = %d, want %d", k, loaded[k], v)
			}
		}
	})

	t.Run("nonexistent returns empty", func(t *testing.T) {
		loaded := loadCache("/nonexistent/cache.gob")
		if len(loaded) != 0 {
			t.Errorf("expected empty map, got %d entries", len(loaded))
		}
	})

	t.Run("corrupt file returns empty", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "cache.gob")
		os.WriteFile(p, []byte("not gob data"), 0644)
		loaded := loadCache(p)
		if len(loaded) != 0 {
			t.Errorf("expected empty map for corrupt file, got %d entries", len(loaded))
		}
	})

	t.Run("empty file returns empty", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "cache.gob")
		os.WriteFile(p, []byte(""), 0644)
		loaded := loadCache(p)
		if len(loaded) != 0 {
			t.Errorf("expected empty map for empty file, got %d entries", len(loaded))
		}
	})

	t.Run("empty map round trip", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "cache.gob")
		if err := saveCache(p, map[int64]uint64{}); err != nil {
			t.Fatal(err)
		}
		loaded := loadCache(p)
		if len(loaded) != 0 {
			t.Errorf("expected empty map, got %d entries", len(loaded))
		}
	})

	t.Run("creates parent dirs", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "a", "b", "cache.gob")
		err := saveCache(p, map[int64]uint64{1: 2})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("no tmp file left", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "cache.gob")
		saveCache(p, map[int64]uint64{1: 2})
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Error("temp file should be cleaned up")
			}
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "cache.gob")
		saveCache(p, map[int64]uint64{1: 10})
		saveCache(p, map[int64]uint64{2: 20})
		loaded := loadCache(p)
		if _, ok := loaded[1]; ok {
			t.Error("old data should be overwritten")
		}
		if loaded[2] != 20 {
			t.Errorf("loaded[2] = %d, want 20", loaded[2])
		}
	})
}

func TestMetaPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/cache/abc123.gob", "/cache/abc123.meta"},
		{"/cache/a.b.c.gob", "/cache/a.b.c.meta"},
	}
	for _, tt := range tests {
		got := metaPath(tt.input)
		if got != tt.want {
			t.Errorf("metaPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMetaRoundTrip(t *testing.T) {
	t.Run("save and load", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "scan.meta")
		meta := &ScanMeta{FileCount: 12345}
		if err := saveMeta(p, meta); err != nil {
			t.Fatal(err)
		}
		loaded := loadMeta(p)
		if loaded == nil {
			t.Fatal("loadMeta returned nil")
		}
		if loaded.FileCount != 12345 {
			t.Errorf("FileCount = %d, want 12345", loaded.FileCount)
		}
	})

	t.Run("nonexistent returns nil", func(t *testing.T) {
		if loadMeta("/nonexistent/scan.meta") != nil {
			t.Error("expected nil for nonexistent file")
		}
	})

	t.Run("corrupt returns nil", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "scan.meta")
		os.WriteFile(p, []byte("garbage"), 0644)
		if loadMeta(p) != nil {
			t.Error("expected nil for corrupt file")
		}
	})
}

func TestHashFilename(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		a := hashFilename("test.txt")
		b := hashFilename("test.txt")
		if a != b {
			t.Error("hashFilename not deterministic")
		}
	})

	t.Run("different names differ", func(t *testing.T) {
		a := hashFilename("a.txt")
		b := hashFilename("b.txt")
		if a == b {
			t.Error("different names should produce different hashes")
		}
	})

	t.Run("empty string", func(t *testing.T) {
		h := hashFilename("")
		if h == 0 {
			t.Error("empty string should have non-zero FNV hash (offset basis)")
		}
	})
}
