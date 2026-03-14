package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSameExtents(t *testing.T) {
	tests := []struct {
		name string
		a, b []Extent
		want bool
	}{
		{"both nil", nil, nil, true},
		{"both empty", []Extent{}, []Extent{}, true},
		{"nil vs empty", nil, []Extent{}, true},
		{"identical single", []Extent{{Physical: 100, Length: 200}}, []Extent{{Physical: 100, Length: 200}}, true},
		{"different lengths", []Extent{{Physical: 1, Length: 2}}, []Extent{{Physical: 1, Length: 2}, {Physical: 3, Length: 4}}, false},
		{"different physical", []Extent{{Physical: 1, Length: 2}}, []Extent{{Physical: 9, Length: 2}}, false},
		{"different length", []Extent{{Physical: 1, Length: 2}}, []Extent{{Physical: 1, Length: 9}}, false},
		{"logical ignored", []Extent{{Logical: 0, Physical: 1, Length: 2}}, []Extent{{Logical: 99, Physical: 1, Length: 2}}, true},
		{"flags ignored", []Extent{{Physical: 1, Length: 2, Flags: 0}}, []Extent{{Physical: 1, Length: 2, Flags: 1}}, true},
		{"multiple matching", []Extent{{Physical: 1, Length: 2}, {Physical: 3, Length: 4}, {Physical: 5, Length: 6}},
			[]Extent{{Physical: 1, Length: 2}, {Physical: 3, Length: 4}, {Physical: 5, Length: 6}}, true},
		{"last differs", []Extent{{Physical: 1, Length: 2}, {Physical: 3, Length: 4}},
			[]Extent{{Physical: 1, Length: 2}, {Physical: 3, Length: 99}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SameExtents(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("SameExtents = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsEOF(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"io.EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"nil", nil, false},
		{"other error", fmt.Errorf("other"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEOF(tt.err); got != tt.want {
				t.Errorf("isEOF(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func createTempFile(t *testing.T, dir string, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFilesEqual(t *testing.T) {
	t.Run("identical", func(t *testing.T) {
		dir := t.TempDir()
		a := createTempFile(t, dir, "a", []byte("hello world"))
		b := createTempFile(t, dir, "b", []byte("hello world"))
		eq, err := filesEqual(a, b)
		if err != nil {
			t.Fatal(err)
		}
		if !eq {
			t.Error("identical files should be equal")
		}
	})

	t.Run("different same size", func(t *testing.T) {
		dir := t.TempDir()
		a := createTempFile(t, dir, "a", []byte("hello"))
		b := createTempFile(t, dir, "b", []byte("world"))
		eq, err := filesEqual(a, b)
		if err != nil {
			t.Fatal(err)
		}
		if eq {
			t.Error("different files should not be equal")
		}
	})

	t.Run("empty files", func(t *testing.T) {
		dir := t.TempDir()
		a := createTempFile(t, dir, "a", []byte{})
		b := createTempFile(t, dir, "b", []byte{})
		eq, err := filesEqual(a, b)
		if err != nil {
			t.Fatal(err)
		}
		if !eq {
			t.Error("empty files should be equal")
		}
	})

	t.Run("file A missing", func(t *testing.T) {
		dir := t.TempDir()
		b := createTempFile(t, dir, "b", []byte("data"))
		_, err := filesEqual("/nonexistent", b)
		if err == nil {
			t.Error("expected error for missing file A")
		}
	})

	t.Run("file B missing", func(t *testing.T) {
		dir := t.TempDir()
		a := createTempFile(t, dir, "a", []byte("data"))
		_, err := filesEqual(a, "/nonexistent")
		if err == nil {
			t.Error("expected error for missing file B")
		}
	})

	t.Run("large identical files", func(t *testing.T) {
		dir := t.TempDir()
		// Create files larger than the 256KiB chunk size.
		data := make([]byte, 300*1024)
		for i := range data {
			data[i] = byte(i % 251)
		}
		a := createTempFile(t, dir, "a", data)
		b := createTempFile(t, dir, "b", data)
		eq, err := filesEqual(a, b)
		if err != nil {
			t.Fatal(err)
		}
		if !eq {
			t.Error("large identical files should be equal")
		}
	})

	t.Run("large files differ last byte", func(t *testing.T) {
		dir := t.TempDir()
		data := make([]byte, 300*1024)
		for i := range data {
			data[i] = byte(i % 251)
		}
		a := createTempFile(t, dir, "a", data)
		data[len(data)-1] ^= 0xFF
		b := createTempFile(t, dir, "b", data)
		eq, err := filesEqual(a, b)
		if err != nil {
			t.Fatal(err)
		}
		if eq {
			t.Error("files differing in last byte should not be equal")
		}
	})

	t.Run("same file path", func(t *testing.T) {
		dir := t.TempDir()
		a := createTempFile(t, dir, "a", []byte("test content"))
		eq, err := filesEqual(a, a)
		if err != nil {
			t.Fatal(err)
		}
		if !eq {
			t.Error("file compared to itself should be equal")
		}
	})
}

func TestProcessSizeGroupDryRun(t *testing.T) {
	t.Run("single file", func(t *testing.T) {
		dir := t.TempDir()
		a := createTempFile(t, dir, "a", []byte("data"))
		stats := ProcessSizeGroup([]string{a}, 4, true, false, false, false, false, nil)
		if stats.FilesDeduped != 0 || stats.BytesSaved != 0 || stats.Errors != 0 {
			t.Errorf("single file should have no action, got %+v", stats)
		}
	})

	t.Run("two identical files", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("duplicate content here")
		a := createTempFile(t, dir, "a", content)
		b := createTempFile(t, dir, "b", content)
		stats := ProcessSizeGroup([]string{a, b}, int64(len(content)), true, false, false, false, false, nil)
		if stats.FilesDeduped != 1 {
			t.Errorf("FilesDeduped = %d, want 1", stats.FilesDeduped)
		}
		if stats.BytesSaved != int64(len(content)) {
			t.Errorf("BytesSaved = %d, want %d", stats.BytesSaved, len(content))
		}
	})

	t.Run("three identical files", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("triple content")
		a := createTempFile(t, dir, "a", content)
		b := createTempFile(t, dir, "b", content)
		c := createTempFile(t, dir, "c", content)
		stats := ProcessSizeGroup([]string{a, b, c}, int64(len(content)), true, false, false, false, false, nil)
		if stats.FilesDeduped != 2 {
			t.Errorf("FilesDeduped = %d, want 2", stats.FilesDeduped)
		}
	})

	t.Run("two different files", func(t *testing.T) {
		dir := t.TempDir()
		a := createTempFile(t, dir, "a", []byte("aaaaa"))
		b := createTempFile(t, dir, "b", []byte("bbbbb"))
		stats := ProcessSizeGroup([]string{a, b}, 5, true, false, false, false, false, nil)
		if stats.FilesDeduped != 0 {
			t.Errorf("different files should not be deduped, got %d", stats.FilesDeduped)
		}
	})

	t.Run("mixed identical and different", func(t *testing.T) {
		dir := t.TempDir()
		a := createTempFile(t, dir, "a", []byte("same!"))
		b := createTempFile(t, dir, "b", []byte("same!"))
		c := createTempFile(t, dir, "c", []byte("diff!"))
		stats := ProcessSizeGroup([]string{a, b, c}, 5, true, false, false, false, false, nil)
		if stats.FilesDeduped != 1 {
			t.Errorf("FilesDeduped = %d, want 1", stats.FilesDeduped)
		}
	})

	t.Run("empty paths", func(t *testing.T) {
		stats := ProcessSizeGroup([]string{}, 0, true, false, false, false, false, nil)
		if stats.FilesDeduped != 0 || stats.Errors != 0 {
			t.Errorf("empty paths should have no action, got %+v", stats)
		}
	})

	t.Run("progress callback", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("test")
		a := createTempFile(t, dir, "a", content)
		b := createTempFile(t, dir, "b", content)
		c := createTempFile(t, dir, "c", content)

		var calls []int
		ProcessSizeGroup([]string{a, b, c}, int64(len(content)), true, false, false, false, false, func(current int) {
			calls = append(calls, current)
		})
		if len(calls) != 3 {
			t.Errorf("expected 3 progress calls, got %d", len(calls))
		}
		for i, v := range calls {
			if v != i+1 {
				t.Errorf("call %d: got %d, want %d", i, v, i+1)
			}
		}
	})

	t.Run("no error details in dry run", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("content")
		a := createTempFile(t, dir, "a", content)
		b := createTempFile(t, dir, "b", content)
		stats := ProcessSizeGroup([]string{a, b}, int64(len(content)), true, false, false, false, false, nil)
		if len(stats.ErrorDetails) != 0 {
			t.Errorf("dry-run should have no error details, got %d", len(stats.ErrorDetails))
		}
	})
}

func TestAddDirWrite(t *testing.T) {
	t.Run("already writable", func(t *testing.T) {
		dir := t.TempDir()
		os.Chmod(dir, 0755)
		restore, err := addDirWrite(dir)
		if err != nil {
			t.Fatal(err)
		}
		restore() // should be a no-op
		info, _ := os.Stat(dir)
		if info.Mode().Perm() != 0755 {
			t.Errorf("perm = %o, want 0755", info.Mode().Perm())
		}
	})

	t.Run("read-only dir", func(t *testing.T) {
		dir := t.TempDir()
		os.Chmod(dir, 0555)
		defer os.Chmod(dir, 0755) // cleanup safety

		restore, err := addDirWrite(dir)
		if err != nil {
			t.Fatal(err)
		}
		// Should be writable now.
		info, _ := os.Stat(dir)
		if info.Mode().Perm()&0200 == 0 {
			t.Error("dir should be writable after addDirWrite")
		}
		restore()
		// Should be read-only again.
		info, _ = os.Stat(dir)
		if info.Mode().Perm()&0200 != 0 {
			t.Error("dir should be read-only after restore")
		}
	})

	t.Run("nonexistent dir", func(t *testing.T) {
		_, err := addDirWrite("/nonexistent/dir")
		if err == nil {
			t.Error("expected error for nonexistent dir")
		}
	})
}

func TestBackupToTemp(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		dir := t.TempDir()
		content := []byte("backup content here")
		src := createTempFile(t, dir, "src", content)
		backup, err := backupToTemp(src)
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(backup)
		got, _ := os.ReadFile(backup)
		if string(got) != string(content) {
			t.Errorf("backup content = %q, want %q", got, content)
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		_, err := backupToTemp("/nonexistent")
		if err == nil {
			t.Error("expected error for nonexistent file")
		}
	})
}

func TestRestoreFromTemp(t *testing.T) {
	t.Run("successful restore", func(t *testing.T) {
		dir := t.TempDir()
		original := []byte("original")
		backup := createTempFile(t, dir, "backup", original)
		dst := createTempFile(t, dir, "dst", []byte("modified"))
		restoreFromTemp(backup, dst)
		got, _ := os.ReadFile(dst)
		if string(got) != string(original) {
			t.Errorf("restored content = %q, want %q", got, original)
		}
	})

	t.Run("missing backup no panic", func(t *testing.T) {
		dir := t.TempDir()
		dst := createTempFile(t, dir, "dst", []byte("data"))
		restoreFromTemp("/nonexistent", dst) // should not panic
	})
}
