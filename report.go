package main

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DedupError captures details of a failed dedup attempt for error reporting.
type DedupError struct {
	Size    int64
	Mode    string // "reflink" or "hardlink"
	Err     string
	SrcPath string
	DstPath string
}

// pathPattern returns an anonymized representation of a file path,
// preserving length, depth, extension, segment lengths, and character
// classes — enough to reproduce path-related bugs without revealing
// the actual content.
func pathPattern(p string) string {
	depth := strings.Count(p, "/")
	ext := filepath.Ext(p)
	parts := strings.Split(p, "/")
	segs := make([]string, len(parts))
	for i, s := range parts {
		segs[i] = fmt.Sprintf("%d", len(s))
	}
	return fmt.Sprintf("d=%d,l=%d,ext=%s,c=%s,segs=%s",
		depth, len(p), ext, charClasses(p), strings.Join(segs, "."))
}

// charClasses returns a compact string describing which character classes
// are present in s (excluding path separators which are always expected).
func charClasses(s string) string {
	var lower, upper, digit, space, dot, dash, underscore, other, nonASCII bool
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		case r == ' ':
			space = true
		case r == '.':
			dot = true
		case r == '-':
			dash = true
		case r == '_':
			underscore = true
		case r == '/' || r == '\\':
			// path separators — expected, don't flag
		case r > 127:
			nonASCII = true
		default:
			other = true
		}
	}
	var b strings.Builder
	if lower {
		b.WriteByte('a')
	}
	if upper {
		b.WriteByte('A')
	}
	if digit {
		b.WriteByte('0')
	}
	if space {
		b.WriteString("SP")
	}
	if dot {
		b.WriteByte('.')
	}
	if dash {
		b.WriteByte('-')
	}
	if underscore {
		b.WriteByte('_')
	}
	if other {
		b.WriteByte('?')
	}
	if nonASCII {
		b.WriteByte('~')
	}
	return b.String()
}

// sanitizeError removes file paths from error strings to avoid leaking
// personally identifiable information.
func sanitizeError(s string) string {
	var result strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '/' && (i == 0 || s[i-1] == ' ' || s[i-1] == ':') {
			j := i + 1
			for j < len(s) && s[j] != ' ' && s[j] != ':' && s[j] != '\n' && s[j] != '\t' {
				j++
			}
			if j > i+1 {
				result.WriteString("<path>")
				i = j
				continue
			}
		}
		result.WriteByte(s[i])
		i++
	}
	return result.String()
}

// reportKey returns a dedup key for an error report entry, based on the
// sanitized error and anonymized path patterns.
func reportKey(de *DedupError) string {
	h := fnv.New64a()
	h.Write([]byte(sanitizeError(de.Err)))
	h.Write([]byte{0})
	h.Write([]byte(pathPattern(de.SrcPath)))
	h.Write([]byte{0})
	h.Write([]byte(pathPattern(de.DstPath)))
	return fmt.Sprintf("%016x", h.Sum64())
}

// reportFilePath returns the path to the global error report file
// in the user's cache directory.
func reportFilePath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "fastdedup", "report.txt"), nil
}

// loadReportKeys reads existing dedup keys from the report file.
func loadReportKeys(path string) map[string]bool {
	keys := make(map[string]bool)
	f, err := os.Open(path)
	if err != nil {
		return keys
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || len(line) < 16 {
			continue
		}
		keys[line[:16]] = true
	}
	return keys
}

// appendReport writes new (not previously reported) errors to the report file.
// Errors whose dedup key already exists in the file are suppressed.
func appendReport(reportFile string, errors []DedupError, ver string) error {
	if len(errors) == 0 {
		return nil
	}

	existing := loadReportKeys(reportFile)

	var newEntries []string
	for i := range errors {
		key := reportKey(&errors[i])
		if existing[key] {
			continue
		}
		existing[key] = true
		entry := fmt.Sprintf("%s %s v=%s size=%d mode=%s err=%q src={%s} dst={%s}",
			key,
			time.Now().UTC().Format(time.RFC3339),
			ver,
			errors[i].Size,
			errors[i].Mode,
			sanitizeError(errors[i].Err),
			pathPattern(errors[i].SrcPath),
			pathPattern(errors[i].DstPath),
		)
		newEntries = append(newEntries, entry)
	}

	if len(newEntries) == 0 {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(reportFile), 0755); err != nil {
		return err
	}

	needHeader := false
	if _, err := os.Stat(reportFile); os.IsNotExist(err) {
		needHeader = true
	}

	f, err := os.OpenFile(reportFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if needHeader {
		fmt.Fprintln(f, "# fastdedup error report — anonymized, no personally identifiable information")
		fmt.Fprintln(f, "# Each line: KEY TIMESTAMP VERSION SIZE MODE ERROR SRC_PATTERN DST_PATTERN")
		fmt.Fprintln(f, "# Duplicate entries (same KEY) are suppressed automatically")
	}

	for _, entry := range newEntries {
		fmt.Fprintln(f, entry)
	}

	return nil
}
