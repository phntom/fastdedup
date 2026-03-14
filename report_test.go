package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCharClasses(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"lowercase", "abc", "a"},
		{"uppercase", "ABC", "A"},
		{"digits", "123", "0"},
		{"space", " ", "SP"},
		{"dot", ".", "."},
		{"dash", "-", "-"},
		{"underscore", "_", "_"},
		{"slashes excluded", "///", ""},
		{"backslash excluded", `\`, ""},
		{"non-ascii", "café", "a~"},
		{"all unicode", "日本語", "~"},
		{"typical path", "/home/user/file.txt", "a."},
		{"other chars", "foo[1](2)", "a0?"},
		{"all classes", "aA0 .-_!日/", "aA0SP.-_?~"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := charClasses(tt.input)
			if got != tt.want {
				t.Errorf("charClasses(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPathPattern(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantDepth int
		wantLen   int
		wantExt   string
	}{
		{"simple absolute", "/home/user/doc.txt", 3, 18, ".txt"},
		{"root", "/", 1, 1, ""},
		{"relative no slash", "file.txt", 0, 8, ".txt"},
		{"empty", "", 0, 0, ""},
		{"no extension", "/usr/bin/fastdedup", 3, 18, ""},
		{"multiple dots", "/data/archive.tar.gz", 2, 20, ".gz"},
		{"deep path", "/a/b/c/d/e/f/g.h", 7, 16, ".h"},
		{"dotfile", "/home/user/.bashrc", 3, 18, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pathPattern(tt.input)
			// Verify key fields are present in the pattern string.
			if !strings.Contains(got, "d=") || !strings.Contains(got, "l=") || !strings.Contains(got, "ext=") {
				t.Errorf("pathPattern(%q) = %q, missing expected fields", tt.input, got)
			}
			// Verify depth.
			wantD := "d=" + itoa(tt.wantDepth)
			if !strings.HasPrefix(got, wantD+",") {
				t.Errorf("pathPattern(%q) starts with %q, want prefix %q", tt.input, got, wantD)
			}
			// Verify length.
			wantL := "l=" + itoa(tt.wantLen)
			if !strings.Contains(got, wantL) {
				t.Errorf("pathPattern(%q) = %q, want %q", tt.input, got, wantL)
			}
			// Verify extension.
			wantE := "ext=" + tt.wantExt
			if !strings.Contains(got, wantE) {
				t.Errorf("pathPattern(%q) = %q, want %q", tt.input, got, wantE)
			}
		})
	}
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Replace(formatCount(int64(n)), ",", "", -1))
}

func TestPathPatternSegments(t *testing.T) {
	// /home/user/doc.txt => segments: "" "home" "user" "doc.txt" => segs=0.4.4.7
	got := pathPattern("/home/user/doc.txt")
	if !strings.Contains(got, "segs=0.4.4.7") {
		t.Errorf("pathPattern segments wrong: %q", got)
	}
}

func TestPathPatternDeterministic(t *testing.T) {
	p := "/some/test/path.bin"
	a := pathPattern(p)
	b := pathPattern(p)
	if a != b {
		t.Errorf("pathPattern not deterministic: %q != %q", a, b)
	}
}

func TestPathPatternSameStructure(t *testing.T) {
	// Two paths with same structure but different content should produce same pattern.
	a := pathPattern("/aaa/bbb.txt")
	b := pathPattern("/xxx/yyy.txt")
	if a != b {
		t.Errorf("same-structure paths produced different patterns: %q vs %q", a, b)
	}
}

func TestSanitizeError(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no paths", "permission denied", "permission denied"},
		{"path at start", "/usr/bin/foo: error", "<path>: error"},
		{"path after space", "open /tmp/secret.txt: no such file", "open <path>: no such file"},
		{"path after colon", "error:/etc/passwd", "error:<path>"},
		{"multiple paths", "link /src/a to /dst/b failed", "link <path> to <path> failed"},
		{"mid-word slash", "a/b/c", "a/b/c"},
		{"empty", "", ""},
		{"bare slash only", "/", "/"},
		{"path at end", "error /tmp/x", "error <path>"},
		{"path with newline", "/tmp/a\nmore", "<path>\nmore"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeError(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeError(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestReportKeyDeterministic(t *testing.T) {
	de := &DedupError{
		Size:    1024,
		Mode:    "reflink",
		Err:     "FICLONE ioctl: cross-device link",
		SrcPath: "/mnt/data/file1.txt",
		DstPath: "/mnt/data/file2.txt",
	}
	a := reportKey(de)
	b := reportKey(de)
	if a != b {
		t.Errorf("reportKey not deterministic: %q != %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("reportKey length = %d, want 16", len(a))
	}
}

func TestReportKeyDiffers(t *testing.T) {
	base := DedupError{
		Size:    1024,
		Mode:    "reflink",
		Err:     "error A",
		SrcPath: "/src/file.txt",
		DstPath: "/dst/file.txt",
	}

	// Different error text.
	diff := base
	diff.Err = "error B"
	if reportKey(&base) == reportKey(&diff) {
		t.Error("different errors should produce different keys")
	}

	// Different src pattern (different depth).
	diff = base
	diff.SrcPath = "/a/b/c/file.txt"
	if reportKey(&base) == reportKey(&diff) {
		t.Error("different src patterns should produce different keys")
	}

	// Different dst pattern.
	diff = base
	diff.DstPath = "/a/b/c/d/file.txt"
	if reportKey(&base) == reportKey(&diff) {
		t.Error("different dst patterns should produce different keys")
	}
}

func TestReportKeySamePattern(t *testing.T) {
	// Same structure paths should produce same key.
	a := &DedupError{Err: "err", SrcPath: "/aaa/bbb.txt", DstPath: "/ccc/ddd.txt"}
	b := &DedupError{Err: "err", SrcPath: "/xxx/yyy.txt", DstPath: "/zzz/www.txt"}
	if reportKey(a) != reportKey(b) {
		t.Error("same-pattern paths should produce same key")
	}
}

func TestLoadReportKeys(t *testing.T) {
	t.Run("nonexistent", func(t *testing.T) {
		keys := loadReportKeys("/nonexistent/report.txt")
		if len(keys) != 0 {
			t.Errorf("expected empty map, got %d keys", len(keys))
		}
	})

	t.Run("empty file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "report.txt")
		os.WriteFile(f, []byte(""), 0644)
		keys := loadReportKeys(f)
		if len(keys) != 0 {
			t.Errorf("expected empty map, got %d keys", len(keys))
		}
	})

	t.Run("comments and short lines", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "report.txt")
		os.WriteFile(f, []byte("# comment\nshort\n# another\n"), 0644)
		keys := loadReportKeys(f)
		if len(keys) != 0 {
			t.Errorf("expected empty map, got %d keys", len(keys))
		}
	})

	t.Run("valid entries", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "report.txt")
		content := "0123456789abcdef rest of line\nfedcba9876543210 another line\n"
		os.WriteFile(f, []byte(content), 0644)
		keys := loadReportKeys(f)
		if len(keys) != 2 {
			t.Errorf("expected 2 keys, got %d", len(keys))
		}
		if !keys["0123456789abcdef"] {
			t.Error("missing key 0123456789abcdef")
		}
		if !keys["fedcba9876543210"] {
			t.Error("missing key fedcba9876543210")
		}
	})
}

func TestAppendReport(t *testing.T) {
	t.Run("empty errors", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "report.txt")
		err := appendReport(f, nil, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Error("file should not be created for empty errors")
		}
	})

	t.Run("creates new file with header", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "report.txt")
		errors := []DedupError{{
			Size: 1024, Mode: "reflink",
			Err: "test error", SrcPath: "/src/a.txt", DstPath: "/dst/b.txt",
		}}
		err := appendReport(f, errors, "0.0.8")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(f)
		content := string(data)
		if !strings.Contains(content, "# fastdedup error report") {
			t.Error("missing header")
		}
		if !strings.Contains(content, "v=0.0.8") {
			t.Error("missing version")
		}
		if !strings.Contains(content, "size=1024") {
			t.Error("missing size")
		}
		if !strings.Contains(content, "mode=reflink") {
			t.Error("missing mode")
		}
	})

	t.Run("dedup suppression", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "report.txt")
		errors := []DedupError{{
			Size: 1024, Mode: "reflink",
			Err: "test error", SrcPath: "/src/a.txt", DstPath: "/dst/b.txt",
		}}
		// First write.
		appendReport(f, errors, "0.0.8")
		data1, _ := os.ReadFile(f)
		// Second write with same error — should be suppressed.
		appendReport(f, errors, "0.0.8")
		data2, _ := os.ReadFile(f)
		if string(data1) != string(data2) {
			t.Error("duplicate error was not suppressed")
		}
	})

	t.Run("different errors appended", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "report.txt")
		e1 := []DedupError{{Size: 1024, Mode: "reflink", Err: "error A", SrcPath: "/a/x.txt", DstPath: "/a/y.txt"}}
		e2 := []DedupError{{Size: 2048, Mode: "hardlink", Err: "error B", SrcPath: "/b/x.txt", DstPath: "/b/y.txt"}}
		appendReport(f, e1, "0.0.8")
		appendReport(f, e2, "0.0.8")
		data, _ := os.ReadFile(f)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		nonComment := 0
		for _, l := range lines {
			if !strings.HasPrefix(l, "#") {
				nonComment++
			}
		}
		if nonComment != 2 {
			t.Errorf("expected 2 data lines, got %d", nonComment)
		}
	})

	t.Run("creates intermediate dirs", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "sub", "dir", "report.txt")
		errors := []DedupError{{Size: 1, Mode: "reflink", Err: "e", SrcPath: "/a", DstPath: "/b"}}
		err := appendReport(f, errors, "test")
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("error paths sanitized in output", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "report.txt")
		errors := []DedupError{{
			Size: 1024, Mode: "reflink",
			Err:     "open /secret/data.txt: permission denied",
			SrcPath: "/src/a.txt", DstPath: "/dst/b.txt",
		}}
		appendReport(f, errors, "test")
		data, _ := os.ReadFile(f)
		if strings.Contains(string(data), "/secret/data.txt") {
			t.Error("raw path leaked into report")
		}
		if !strings.Contains(string(data), "<path>") {
			t.Error("path should be sanitized to <path>")
		}
	})
}
