// testdedup generates synthetic test files, runs fastdedup, and verifies results.
//
// Usage:
//
//	go run ./cmd/testdedup [-dir /path/to/btrfs] [fastdedup-binary]
//
// If no binary path is given, it builds ./fastdedup first.
// If -dir is given, the temp directory is created inside that path.
package main

import (
	"bytes"
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func main() {
	baseDir := flag.String("dir", "", "base directory for temp files (e.g. a btrfs mount)")
	flag.Parse()

	binary := findBinary()

	dir, err := os.MkdirTemp(*baseDir, "fastdedup-test.*")
	if err != nil {
		fatal("create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	fmt.Printf("==> Test directory: %s\n", dir)

	// --- Generate test files ---
	// All files >= 512 KiB (default --min-size).

	// Group A: 3 identical 1 MiB files — should dedup 2.
	fmt.Println("==> Group A: 3 identical 1 MiB files")
	dataA := randBytes(1 << 20)
	writeFile(dir, "groupA_1.bin", dataA)
	writeFile(dir, "groupA_2.bin", dataA)
	writeFile(dir, "groupA_3.bin", dataA)

	// Group B: 2 identical 2 MiB files — should dedup 1.
	fmt.Println("==> Group B: 2 identical 2 MiB files")
	dataB := randBytes(2 << 20)
	writeFile(dir, "groupB_1.bin", dataB)
	writeFile(dir, "groupB_2.bin", dataB)

	// Group C: 3 files, same size as A (1 MiB) but DIFFERENT content — should NOT dedup.
	fmt.Println("==> Group C: 3 different 1 MiB files (same size as A)")
	writeFile(dir, "groupC_1.bin", randBytes(1<<20))
	writeFile(dir, "groupC_2.bin", randBytes(1<<20))
	writeFile(dir, "groupC_3.bin", randBytes(1<<20))

	// Group D: 4 files (512 KiB), two pairs of identical content — should dedup 2.
	fmt.Println("==> Group D: 2 pairs of identical 512 KiB files")
	dataD1 := randBytes(512 << 10)
	dataD2 := randBytes(512 << 10)
	writeFile(dir, "groupD_pair1a.bin", dataD1)
	writeFile(dir, "groupD_pair1b.bin", dataD1)
	writeFile(dir, "groupD_pair2a.bin", dataD2)
	writeFile(dir, "groupD_pair2b.bin", dataD2)

	// Group E: 1 lone 1 MiB file — no pair, should be ignored.
	fmt.Println("==> Group E: 1 lone 1 MiB file (no duplicate)")
	writeFile(dir, "groupE_lone.bin", randBytes(1<<20))

	// --- Dry run ---
	fmt.Println("\n==> Running fastdedup --dry-run...")
	runCmd(binary, "--no-cache", "--min-size", "0", "--dry-run", "-v", dir)

	// --- Real run ---
	fmt.Println("\n==> Running fastdedup...")
	output := runCmd(binary, "--no-cache", "--min-size", "0", "-v", dir)

	// --- Verify ---
	fmt.Println("\n==> Verifying results...")
	pass := true

	check := func(label string, ok bool) {
		if ok {
			fmt.Printf("  PASS %s\n", label)
		} else {
			fmt.Printf("  FAIL %s\n", label)
			pass = false
		}
	}

	// Content integrity.
	fmt.Println("\n--- Content integrity ---")
	check("A1==A2", filesEqual(dir, "groupA_1.bin", "groupA_2.bin"))
	check("A1==A3", filesEqual(dir, "groupA_1.bin", "groupA_3.bin"))
	check("B1==B2", filesEqual(dir, "groupB_1.bin", "groupB_2.bin"))
	check("C1!=C2", !filesEqual(dir, "groupC_1.bin", "groupC_2.bin"))
	check("C1!=C3", !filesEqual(dir, "groupC_1.bin", "groupC_3.bin"))
	check("D1a==D1b", filesEqual(dir, "groupD_pair1a.bin", "groupD_pair1b.bin"))
	check("D2a==D2b", filesEqual(dir, "groupD_pair2a.bin", "groupD_pair2b.bin"))
	check("D1a!=D2a", !filesEqual(dir, "groupD_pair1a.bin", "groupD_pair2a.bin"))

	// Dedup count from output.
	// Group A: 2, Group B: 1, Group D: 2 = 5 total.
	fmt.Println("\n--- Output checks ---")
	noBtrfs := strings.Contains(output, "FICLONE ioctl: operation not supported")
	if noBtrfs {
		fmt.Println("  SKIP dedup count (filesystem does not support reflinks)")
	} else {
		re := regexp.MustCompile(`Files deduped:\s*([\d,]+)`)
		m := re.FindStringSubmatch(output)
		if m != nil {
			countStr := strings.ReplaceAll(m[1], ",", "")
			count, _ := strconv.Atoi(countStr)
			check(fmt.Sprintf("dedup count = %d (expected 5)", count), count == 5)
		} else {
			fmt.Println("  FAIL could not parse dedup count from output")
			pass = false
		}
	}

	// --- Cache test ---
	// Run with cache enabled, then run again — second run should skip all groups
	// (only if the first run had no errors, since errors prevent cache saving).
	fmt.Println("\n--- Cache test ---")
	fmt.Println("==> Running fastdedup with cache (first run)...")
	firstCacheOutput := runCmd(binary, "--min-size", "0", "-v", dir)

	if strings.Contains(firstCacheOutput, "Errors:") && !strings.Contains(firstCacheOutput, "Errors:           0") {
		fmt.Println("\n  SKIP cache test (first run had errors — cache not saved on unsupported filesystem)")
	} else {
		fmt.Println("\n==> Running fastdedup with cache (second run — should skip all)...")
		cacheOutput := runCmd(binary, "--min-size", "0", "-v", dir)
		check("second run skips cached groups",
			strings.Contains(cacheOutput, "Skipped") && strings.Contains(cacheOutput, "unchanged"))
	}

	fmt.Println()
	if pass {
		fmt.Println("==> ALL TESTS PASSED")
	} else {
		fmt.Println("==> SOME TESTS FAILED")
		os.Exit(1)
	}
}

func findBinary() string {
	if flag.NArg() > 0 {
		return flag.Arg(0)
	}
	// Build from source.
	fmt.Println("==> Building fastdedup...")
	cmd := exec.Command("go", "build", "-o", "fastdedup", ".")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("build failed: %v", err)
	}
	abs, err := filepath.Abs("fastdedup")
	if err != nil {
		fatal("abs path: %v", err)
	}
	return abs
}

func randBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		fatal("rand read: %v", err)
	}
	return buf
}

func writeFile(dir, name string, data []byte) {
	if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
		fatal("write %s: %v", name, err)
	}
}

func filesEqual(dir, a, b string) bool {
	da, err := os.ReadFile(filepath.Join(dir, a))
	if err != nil {
		return false
	}
	db, err := os.ReadFile(filepath.Join(dir, b))
	if err != nil {
		return false
	}
	return bytes.Equal(da, db)
}

func runCmd(binary string, args ...string) string {
	cmd := exec.Command(binary, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	output := buf.String()
	fmt.Print(output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: fastdedup exited with: %v\n", err)
	}
	return output
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+format+"\n", args...)
	os.Exit(1)
}
