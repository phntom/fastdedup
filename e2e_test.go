//go:build linux

// Black-box end-to-end regression tests for the fastdedup CLI.
//
// These are characterization tests: the current observable behaviour of the
// binary IS the specification. Every test drives the compiled binary as an
// external process (never in-process) against a purpose-built fixture tree
// and verifies the exit code, the output and the resulting filesystem state
// using independent oracles — byte comparison of contents, os.SameFile for
// hard links, a FICLONE probe for reflink capability — not package internals.
//
// The directory walk order is randomised on every run, so no test may depend
// on which duplicate becomes the kept reference. The suite passes repeatedly
// under -count=5 -shuffle=on.
//
// Environment knobs:
//
//	FASTDEDUP_E2E_BIN     absolute path of a prebuilt binary to test
//	FASTDEDUP_E2E_DIR     directory to build fixture trees in (CI points this
//	                      at a btrfs/XFS mount so reflinks are exercisable)
//	FASTDEDUP_E2E_REQUIRE_REFLINK=1   fail (not skip) when FICLONE is missing
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// Binary under test
// ---------------------------------------------------------------------------

var (
	e2eBinOnce  sync.Once
	e2eBin      string
	e2eBinErr   error
	e2eBuildDir string
)

// TestMain removes the lazily created binary build directory when the test
// process ends. It defines no test itself; no other *_test.go file in this
// package defines TestMain.
func TestMain(m *testing.M) {
	code := m.Run()
	if e2eBuildDir != "" {
		// Best-effort teardown: nothing can act on a failure at this point.
		_ = os.RemoveAll(e2eBuildDir)
	}
	os.Exit(code)
}

// e2eBinary returns the path of the binary under test. With
// FASTDEDUP_E2E_BIN set it must point at an existing absolute path;
// otherwise the binary is built once per test process, lazily on first use,
// into a temp directory outside the repository.
func e2eBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("FASTDEDUP_E2E_BIN"); bin != "" {
		if !filepath.IsAbs(bin) {
			t.Fatalf("FASTDEDUP_E2E_BIN must be an absolute path, got %q", bin)
		}
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("FASTDEDUP_E2E_BIN binary not usable: %v", err)
		}
		return bin
	}
	e2eBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fastdedup-e2e-bin-")
		if err != nil {
			e2eBinErr = fmt.Errorf("create build dir: %w", err)
			return
		}
		bin := filepath.Join(dir, "fastdedup")
		out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
		if err != nil {
			_ = os.RemoveAll(dir)
			e2eBinErr = fmt.Errorf("go build -o %s . : %v\n%s", bin, err, out)
			return
		}
		e2eBin, e2eBuildDir = bin, dir
	})
	if e2eBinErr != nil {
		t.Fatalf("could not prepare fastdedup binary: %v", e2eBinErr)
	}
	return e2eBin
}

// requireE2E is the first line of every e2e test: -short skips them all.
func requireE2E(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping e2e test in -short mode")
	}
}

// ---------------------------------------------------------------------------
// Hermetic runner
// ---------------------------------------------------------------------------

// runResult captures one invocation of the binary.
type runResult struct {
	argv     []string
	exitCode int
	stdout   string
	stderr   string
}

// dump renders the full invocation details so that any assertion failure
// makes the cause obvious (R9).
func (r *runResult) dump() string {
	var b strings.Builder
	fmt.Fprintf(&b, "argv: %q\n", r.argv)
	fmt.Fprintf(&b, "exit code: %d\n", r.exitCode)
	fmt.Fprintf(&b, "--- stdout ---\n%s", r.stdout)
	if r.stdout != "" && !strings.HasSuffix(r.stdout, "\n") {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "--- stderr ---\n%s", r.stderr)
	if r.stderr != "" && !strings.HasSuffix(r.stderr, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// e2eRunner runs the binary with a hermetic environment: only PATH, HOME and
// XDG_CACHE_HOME (both pointing at per-test temp dirs outside the deduped
// tree) plus variables the test sets on purpose. No FASTDEDUP_* variable can
// leak in from the developer's environment, and the real user cache/home is
// never touched.
type e2eRunner struct {
	bin      string
	homeDir  string
	cacheDir string
	extra    map[string]string
}

func newRunner(t *testing.T) *e2eRunner {
	t.Helper()
	home, err := os.MkdirTemp("", "fastdedup-e2e-home-")
	if err != nil {
		t.Fatalf("create HOME dir: %v", err)
	}
	cache, err := os.MkdirTemp("", "fastdedup-e2e-cache-")
	if err != nil {
		t.Fatalf("create XDG_CACHE_HOME dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(home)
		_ = os.RemoveAll(cache)
	})
	return &e2eRunner{
		bin:      e2eBinary(t),
		homeDir:  home,
		cacheDir: cache,
		extra:    map[string]string{},
	}
}

// setenv registers a deliberate environment variable for the child (used for
// FASTDEDUP_* variables the test wants to exercise).
func (e *e2eRunner) setenv(key, value string) {
	e.extra[key] = value
}

func (e *e2eRunner) env(extra map[string]string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.homeDir,
		"XDG_CACHE_HOME=" + e.cacheDir,
	}
	for k, v := range e.extra {
		env = append(env, k+"="+v)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}

// run executes the binary with the given arguments and captures everything.
func (e *e2eRunner) run(t *testing.T, args ...string) *runResult {
	return e.runEnv(t, nil, args...)
}

// runEnv is run with additional per-invocation environment variables.
func (e *e2eRunner) runEnv(t *testing.T, extra map[string]string, args ...string) *runResult {
	t.Helper()
	argv := append([]string{e.bin}, args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = e.env(extra)
	err := cmd.Run()
	r := &runResult{argv: argv, stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return r
	}
	if ee, ok := err.(*exec.ExitError); ok {
		r.exitCode = ee.ExitCode()
		return r
	}
	t.Fatalf("fastdedup could not be executed: %v\n%s", err, r.dump())
	return nil // unreachable
}

// ---------------------------------------------------------------------------
// Assertion helpers
// ---------------------------------------------------------------------------

func requireExitCode(t *testing.T, r *runResult, want int) {
	t.Helper()
	if r.exitCode != want {
		t.Fatalf("exit code = %d, want %d\n%s", r.exitCode, want, r.dump())
	}
}

func requireStderrMatch(t *testing.T, r *runResult, re *regexp.Regexp, what string) {
	t.Helper()
	if !re.MatchString(r.stderr) {
		t.Fatalf("stderr does not contain %s (pattern %q)\n%s", what, re, r.dump())
	}
}

func requireNoStderrMatch(t *testing.T, r *runResult, re *regexp.Regexp, what string) {
	t.Helper()
	if re.MatchString(r.stderr) {
		t.Fatalf("stderr unexpectedly contains %s (pattern %q)\n%s", what, re, r.dump())
	}
}

func requireStdoutMatch(t *testing.T, r *runResult, re *regexp.Regexp, what string) {
	t.Helper()
	if !re.MatchString(r.stdout) {
		t.Fatalf("stdout does not contain %s (pattern %q)\n%s", what, re, r.dump())
	}
}

func requireStdoutEmpty(t *testing.T, r *runResult) {
	t.Helper()
	if r.stdout != "" {
		t.Fatalf("stdout not empty:\n%s", r.dump())
	}
}

// Whitespace-tolerant matchers for the summary numbers (R6: match numbers
// with tolerant regular expressions).
var (
	reHardlinkWarning = regexp.MustCompile(`WARNING: --hardlink mode`)
	reDeduped0        = regexp.MustCompile(`Files deduped:\s+0\b`)
	reDeduped1        = regexp.MustCompile(`Files deduped:\s+1\b`)
	reDeduped5        = regexp.MustCompile(`Files deduped:\s+5\b`)
	reDeduped7        = regexp.MustCompile(`Files deduped:\s+7\b`)
	reErrors0         = regexp.MustCompile(`Errors:\s+0\b`)
	reErrors1         = regexp.MustCompile(`Errors:\s+1\b`)
	reErrors5         = regexp.MustCompile(`Errors:\s+5\b`)
	reAlready5        = regexp.MustCompile(`Already deduped:\s+5\b`)
	reNoDuplicates    = regexp.MustCompile(`No duplicate file sizes found\.`)
	reNoNewDupCached  = regexp.MustCompile(`No new duplicate file sizes found \(3 cached\)\.`)
	reSkipped2Cached  = regexp.MustCompile(`Skipped 2 unchanged size groups \(cached\)`)
	reTimeLimit       = regexp.MustCompile(`Time limit reached, stopping gracefully`)
	reFICLONE         = regexp.MustCompile(`FICLONE ioctl`)
	reInvalidMaxTime  = regexp.MustCompile(`invalid --max-time`)
	reRequireRoot     = regexp.MustCompile(`--scrub and --defrag require root permissions`)
	reDryRunLine      = regexp.MustCompile(`^\[dry-run\] dedup: (.+) -> (.+) \((.+)\)$`)
	reVersionOutput   = regexp.MustCompile(`\Afastdedup \S+\n\z`)
	reUsage           = regexp.MustCompile(`Usage:`)
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// Fixture file sizes: distinct and a few KiB so the suite stays fast (R7).
const (
	sizeA = 4096 // groups A and C
	sizeB = 6144 // group B
	sizeD = 8192 // group D
	sizeE = 3072 // lone file E
)

// Relative layout of the standard fixture. Groups are deliberately spread
// over three subdirectories that each mix several sizes: the walk order is
// random, and this mixing keeps every observable outcome order-independent.
const (
	relA1 = "d1/A1.bin"
	relA2 = "d2/A2.bin"
	relA3 = "d3/A3.bin"
	relC1 = "d1/C1.bin"
	relC2 = "d2/C2.bin"
	relC3 = "d3/C3.bin"
	relB1 = "d1/B1.bin"
	relB2 = "d2/B2.bin"
	relD1 = "d1/D1.bin"
	relD3 = "d3/D3.bin" // same content as D1
	relD2 = "d2/D2.bin"
	relD4 = "d3/D4.bin" // same content as D2
	relE  = "E.bin"
	// relEmpty, relLink and the .snapshots copies complete the fixture.
	relEmpty = "empty.bin"
	relLink  = "link"
	relSnap1 = ".snapshots/S1.bin"
	relSnap2 = ".snapshots/S2.bin"
)

// stdFixture is the standard fixture tree: groups A (3 identical), C (3
// distinct contents of the same size), B (2 identical), D (two identical
// pairs), a lone file E, one empty file, one symlink and a .snapshots
// directory holding two more copies of A's content.
type stdFixture struct {
	root    string            // canonical absolute path
	content map[string][]byte // relative path -> exact bytes written
	links   map[string]string // relative path -> symlink target
	inodes  map[string]uint64 // relative path -> inode number at build time
}

func (f *stdFixture) abs(rel string) string {
	return filepath.Join(f.root, rel)
}

// newFixtureRoot creates the directory the fixture is built in, honouring
// FASTDEDUP_E2E_DIR (R4) so CI can place fixtures on btrfs/XFS.
func newFixtureRoot(t *testing.T) string {
	t.Helper()
	if base := os.Getenv("FASTDEDUP_E2E_DIR"); base != "" {
		root, err := os.MkdirTemp(base, "fastdedup-e2e-fixture-")
		if err != nil {
			t.Fatalf("create fixture dir under FASTDEDUP_E2E_DIR %s: %v", base, err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		return root
	}
	return t.TempDir()
}

// canonicalPath mirrors the binary's own root canonicalisation (Abs +
// EvalSymlinks) so assertions can compare against printed paths.
func canonicalPath(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs %s: %v", p, err)
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", abs, err)
	}
	return canon
}

func newStdFixture(t *testing.T) *stdFixture {
	t.Helper()
	root := newFixtureRoot(t)
	f := &stdFixture{
		root:    canonicalPath(t, root),
		content: map[string][]byte{},
		links:   map[string]string{},
		inodes:  map[string]uint64{},
	}

	dataA := detBytes(1, sizeA)
	dataB := detBytes(2, sizeB)
	dataD1 := detBytes(3, sizeD)
	dataD2 := detBytes(4, sizeD)
	dataC1 := detBytes(5, sizeA)
	dataC2 := detBytes(6, sizeA)
	dataC3 := detBytes(7, sizeA)
	dataE := detBytes(8, sizeE)

	for _, dir := range []string{"d1", "d2", "d3", ".snapshots"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatalf("create fixture dir %s: %v", dir, err)
		}
	}
	write := func(rel string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel), data, 0644); err != nil {
			t.Fatalf("write fixture file %s: %v", rel, err)
		}
		f.content[rel] = data
	}

	// Group A: three identical files of size a, in three different dirs.
	write(relA1, dataA)
	write(relA2, dataA)
	write(relA3, dataA)
	// Group C: three files of size a, all contents distinct (and distinct
	// from A) — must never dedup.
	write(relC1, dataC1)
	write(relC2, dataC2)
	write(relC3, dataC3)
	// Group B: two identical files of size b.
	write(relB1, dataB)
	write(relB2, dataB)
	// Group D: four files of size d forming two identical pairs.
	write(relD1, dataD1)
	write(relD3, dataD1)
	write(relD2, dataD2)
	write(relD4, dataD2)
	// Lone file E: a size nothing else has.
	write(relE, dataE)
	// Empty file, symlink to an A file, .snapshots copies of A's content.
	write(relEmpty, nil)
	if err := os.Symlink("d1/A1.bin", filepath.Join(root, relLink)); err != nil {
		t.Fatalf("create fixture symlink: %v", err)
	}
	f.links[relLink] = "d1/A1.bin"
	write(relSnap1, dataA)
	write(relSnap2, dataA)

	// Record build-time inodes so tests can prove files were untouched.
	for rel := range f.content {
		fi, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("stat fixture file %s: %v", rel, err)
		}
		f.inodes[rel] = inoOf(fi)
	}
	fi, err := os.Lstat(filepath.Join(root, relLink))
	if err != nil {
		t.Fatalf("stat fixture symlink: %v", err)
	}
	f.inodes[relLink] = inoOf(fi)
	return f
}

func (f *stdFixture) groupA() []string { return []string{relA1, relA2, relA3} }

func (f *stdFixture) groupAabs() []string {
	paths := make([]string, 0, len(f.groupA()))
	for _, rel := range f.groupA() {
		paths = append(paths, f.abs(rel))
	}
	return paths
}

// allRegularRels lists every regular file of the fixture in sorted order.
func (f *stdFixture) allRegularRels() []string {
	rels := make([]string, 0, len(f.content))
	for rel := range f.content {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	return rels
}

// detBytes returns n deterministic pseudo-random bytes derived from seed
// (splitmix64). Deterministic content keeps the suite reproducible while
// differing between groups, so nothing dedups by accident.
func detBytes(seed uint64, n int) []byte {
	b := make([]byte, n)
	x := seed
	for i := 0; i < n; i += 8 {
		x += 0x9e3779b97f4a7c15
		z := x
		z ^= z >> 30
		z *= 0xbf58476d1ce4e5b9
		z ^= z >> 27
		z *= 0x94d049bb133111eb
		z ^= z >> 31
		for j := 0; j < 8 && i+j < n; j++ {
			b[i+j] = byte(z >> (8 * uint(j)))
		}
	}
	return b
}

func inoOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Filesystem oracles
// ---------------------------------------------------------------------------

func lstat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return fi
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	return os.SameFile(lstat(t, a), lstat(t, b))
}

func requireOneInode(t *testing.T, r *runResult, what string, absPaths ...string) {
	t.Helper()
	if len(absPaths) < 2 {
		t.Fatalf("requireOneInode(%s): need at least two paths", what)
	}
	for _, p := range absPaths[1:] {
		if !sameFile(t, absPaths[0], p) {
			t.Fatalf("%s files do not share one inode:\n  %s\n  %s\n%s", what, absPaths[0], p, r.dump())
		}
	}
}

func requireDistinctInodes(t *testing.T, r *runResult, what string, absPaths ...string) {
	t.Helper()
	for i := 0; i < len(absPaths); i++ {
		for j := i + 1; j < len(absPaths); j++ {
			if sameFile(t, absPaths[i], absPaths[j]) {
				t.Fatalf("%s: %s and %s unexpectedly share an inode\n%s", what, absPaths[i], absPaths[j], r.dump())
			}
		}
	}
}

func requireSameInode(t *testing.T, r *runResult, what, a, b string) {
	t.Helper()
	if !sameFile(t, a, b) {
		t.Fatalf("%s: %s and %s do not share an inode\n%s", what, a, b, r.dump())
	}
}

func requireDistinctInode(t *testing.T, r *runResult, what, a, b string) {
	t.Helper()
	if sameFile(t, a, b) {
		t.Fatalf("%s: %s and %s unexpectedly share an inode\n%s", what, a, b, r.dump())
	}
}

// requireContentsIntact verifies every fixture file still has exactly the
// bytes the test itself wrote (R8).
func requireContentsIntact(t *testing.T, f *stdFixture, r *runResult, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		got, err := os.ReadFile(f.abs(rel))
		if err != nil {
			t.Fatalf("read %s: %v\n%s", rel, err, r.dump())
		}
		if !bytes.Equal(got, f.content[rel]) {
			t.Fatalf("content of %s changed\n%s", rel, r.dump())
		}
	}
}

// requireUntouchedPaths asserts the given entries were not modified at all:
// same inode as at build time, same content, symlinks still symlinks pointing
// at the same target.
func requireUntouchedPaths(t *testing.T, f *stdFixture, r *runResult, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		p := f.abs(rel)
		fi := lstat(t, p)
		if inoOf(fi) != f.inodes[rel] {
			t.Fatalf("%s was touched (inode changed)\n%s", p, r.dump())
		}
		if want, ok := f.content[rel]; ok {
			got, err := os.ReadFile(p)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("%s was touched (content changed)\n%s", p, r.dump())
			}
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			got, err := os.Readlink(p)
			if err != nil || got != f.links[rel] {
				t.Fatalf("%s was touched (symlink target changed)\n%s", p, r.dump())
			}
		}
	}
}

// requireNoDedupTmp asserts no leftover temporary files exist under root.
func requireNoDedupTmp(t *testing.T, root string, r *runResult) {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".dedup-tmp") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v\n%s", root, err, r.dump())
	}
	if len(found) > 0 {
		t.Fatalf("leftover .dedup-tmp files: %v\n%s", found, r.dump())
	}
}

// treeEntry is a snapshot of one filesystem path.
type treeEntry struct {
	relPath string
	inode   uint64
	mode    os.FileMode
	mtime   time.Time
	content []byte // regular files
	link    string // symlink target
}

// snapshotTree records the full state of every path under root: inode,
// mode, mtime, content and symlink targets.
func snapshotTree(t *testing.T, root string) []treeEntry {
	t.Helper()
	var entries []treeEntry
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		e := treeEntry{
			relPath: relPath(root, path),
			inode:   inoOf(fi),
			mode:    fi.Mode(),
			mtime:   fi.ModTime(),
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			e.link = target
		case fi.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			e.content = data
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return entries
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

// requireTreeMatches asserts the tree still matches a previous snapshot.
// checkInode also compares inode numbers: true for cases where nothing may
// have been replaced at all (dry runs, failed runs); false where the binary
// legitimately replaces files with fresh inodes (successful reflinks).
// ignoreDirMtime tolerates parent-directory mtime changes caused by the
// binary's rename-to-tmp-and-back dance, which touches directory mtimes
// even when every file ends up byte-identical.
func requireTreeMatches(t *testing.T, root string, before []treeEntry, r *runResult, checkInode, ignoreDirMtime bool) {
	t.Helper()
	after := snapshotTree(t, root)
	if len(after) != len(before) {
		t.Fatalf("path count changed: before %d, after %d\n%s", len(before), len(after), r.dump())
	}
	byRel := make(map[string]treeEntry, len(after))
	for _, e := range after {
		byRel[e.relPath] = e
	}
	for _, e := range before {
		cur, ok := byRel[e.relPath]
		if !ok {
			t.Fatalf("path %s disappeared\n%s", e.relPath, r.dump())
		}
		if checkInode && cur.inode != e.inode {
			t.Fatalf("inode of %s changed: before %d, after %d\n%s", e.relPath, e.inode, cur.inode, r.dump())
		}
		if cur.mode != e.mode {
			t.Fatalf("mode of %s changed: before %v, after %v\n%s", e.relPath, e.mode, cur.mode, r.dump())
		}
		if !ignoreDirMtime || cur.mode&os.ModeDir == 0 {
			if !cur.mtime.Equal(e.mtime) {
				t.Fatalf("mtime of %s changed: before %v, after %v\n%s", e.relPath, e.mtime, cur.mtime, r.dump())
			}
		}
		if cur.link != e.link {
			t.Fatalf("symlink target of %s changed: before %q, after %q\n%s", e.relPath, e.link, cur.link, r.dump())
		}
		if !bytes.Equal(cur.content, e.content) {
			t.Fatalf("content of %s changed\n%s", e.relPath, r.dump())
		}
	}
}

// reflinkSupported probes FICLONE support on the filesystem that hosts the
// fixtures, using two scratch files in the fixture base directory (R8).
func reflinkSupported(t *testing.T, baseDir string) bool {
	t.Helper()
	probeDir, err := os.MkdirTemp(baseDir, "fastdedup-e2e-probe-")
	if err != nil {
		t.Fatalf("create reflink probe dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(probeDir) })
	src := filepath.Join(probeDir, "src.bin")
	dst := filepath.Join(probeDir, "dst.bin")
	if err := os.WriteFile(src, detBytes(999, 4096), 0644); err != nil {
		t.Fatalf("write probe source: %v", err)
	}
	s, err := os.Open(src)
	if err != nil {
		t.Fatalf("open probe source: %v", err)
	}
	defer func() { _ = s.Close() }()
	d, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("create probe destination: %v", err)
	}
	defer func() { _ = d.Close() }()
	return unix.IoctlFileClone(int(d.Fd()), int(s.Fd())) == nil
}

// ---------------------------------------------------------------------------
// Standard end-state verification (hard-link mode)
// ---------------------------------------------------------------------------

// assertHardlinkedState verifies the post-run state of the standard fixture
// after a full hard-link dedup: identical groups share one inode, distinct
// content keeps separate inodes, the untouched entries are really untouched,
// every file has its original content and no temporary files remain.
// snapshotsIncluded adjusts the expectation for the .snapshots copies.
func assertHardlinkedState(t *testing.T, f *stdFixture, r *runResult, snapshotsIncluded bool) {
	t.Helper()
	a := f.groupAabs()
	requireOneInode(t, r, "group A", a...)
	requireOneInode(t, r, "group B", f.abs(relB1), f.abs(relB2))
	requireDistinctInodes(t, r, "group C", f.abs(relC1), f.abs(relC2), f.abs(relC3))
	requireOneInode(t, r, "D pair 1", f.abs(relD1), f.abs(relD3))
	requireOneInode(t, r, "D pair 2", f.abs(relD2), f.abs(relD4))
	requireDistinctInode(t, r, "D pair 1 vs pair 2", f.abs(relD1), f.abs(relD2))

	requireUntouchedPaths(t, f, r, relE, relEmpty, relLink)
	if snapshotsIncluded {
		// With --snapshots the two extra copies of A's content join the
		// A inode family (order-independently: whichever file becomes the
		// reference, all identical files link to it).
		requireOneInode(t, r, "group A + .snapshots", append(a, f.abs(relSnap1), f.abs(relSnap2))...)
	} else {
		requireUntouchedPaths(t, f, r, relSnap1, relSnap2)
		requireDistinctInode(t, r, ".snapshots copy vs A", f.abs(relSnap1), f.abs(relA1))
		requireDistinctInode(t, r, ".snapshots copy vs A", f.abs(relSnap2), f.abs(relA1))
	}

	rels := f.allRegularRels()
	requireContentsIntact(t, f, r, rels...)
	requireNoDedupTmp(t, f.root, r)
}

// ---------------------------------------------------------------------------
// Scenarios
// ---------------------------------------------------------------------------

// 1. --version prints the version to stdout and exits 0.
func TestE2EVersion(t *testing.T) {
	requireE2E(t)
	e := newRunner(t)
	r := e.run(t, "--version")
	requireExitCode(t, r, 0)
	requireStdoutMatch(t, r, reVersionOutput, "exactly one version line")
}

// 2. An unknown flag exits 2 and prints the usage (standard flag behaviour).
func TestE2EUsageError(t *testing.T) {
	requireE2E(t)
	e := newRunner(t)
	r := e.run(t, "--definitely-not-a-flag")
	requireExitCode(t, r, 2)
	requireStderrMatch(t, r, reUsage, "usage text")
}

// 3. Hard-link mode dedups exactly 5 files of the standard fixture and
// leaves the expected inode/content state behind.
func TestE2EHardlink(t *testing.T) {
	requireE2E(t)
	f := newStdFixture(t)
	e := newRunner(t)
	r := e.run(t, "--hardlink", "--no-cache", "--min-size", "1", f.root)
	requireExitCode(t, r, 0)
	requireStderrMatch(t, r, reHardlinkWarning, "hard-link warning banner")
	requireStderrMatch(t, r, reDeduped5, "5 files deduped")
	requireStderrMatch(t, r, reErrors0, "no errors")
	assertHardlinkedState(t, f, r, false)
}

// 4. --snapshots additionally includes the .snapshots copies (7 files).
func TestE2ESnapshotsFlag(t *testing.T) {
	requireE2E(t)
	f := newStdFixture(t)
	e := newRunner(t)
	r := e.run(t, "--hardlink", "--no-cache", "--min-size", "1", "--snapshots", f.root)
	requireExitCode(t, r, 0)
	requireStderrMatch(t, r, reHardlinkWarning, "hard-link warning banner")
	requireStderrMatch(t, r, reDeduped7, "7 files deduped")
	requireStderrMatch(t, r, reErrors0, "no errors")
	assertHardlinkedState(t, f, r, true)
}

// 5. Dry runs report what would be deduped without touching anything, and
// a dry run must not poison the cache for a subsequent real run.
func TestE2EDryRun(t *testing.T) {
	requireE2E(t)
	f := newStdFixture(t)
	e := newRunner(t)
	before := snapshotTree(t, f.root)

	// Reflink-mode dry run (works on any filesystem: no ioctl happens).
	r1 := e.run(t, "--dry-run", "--no-cache", "--min-size", "1", f.root)
	requireExitCode(t, r1, 0)
	requireDryRunReport(t, r1, f.root, 5)
	requireStderrMatch(t, r1, reDeduped5, "5 files deduped")
	requireNoStderrMatch(t, r1, reHardlinkWarning, "no hard-link warning in dry run")

	// Hard-link dry run with the cache enabled (same XDG_CACHE_HOME as the
	// real run below).
	r2 := e.run(t, "--dry-run", "--hardlink", "--min-size", "1", f.root)
	requireExitCode(t, r2, 0)
	requireDryRunReport(t, r2, f.root, 5)
	requireStderrMatch(t, r2, reDeduped5, "5 files deduped")
	requireNoStderrMatch(t, r2, reHardlinkWarning, "no hard-link warning in dry run")

	// The dry runs must not have changed a single path.
	requireTreeMatches(t, f.root, before, r2, true, false)

	// A real hard-link run afterwards still dedups everything: the dry
	// runs did not poison the cache.
	r3 := e.run(t, "--hardlink", "--min-size", "1", f.root)
	requireExitCode(t, r3, 0)
	requireStderrMatch(t, r3, reDeduped5, "5 files deduped after preceding dry runs")
}

// requireDryRunReport checks stdout consists of exactly n [dry-run] lines
// naming two distinct existing files under root with identical content.
func requireDryRunReport(t *testing.T, r *runResult, root string, n int) {
	t.Helper()
	var lines []string
	if r.stdout != "" {
		lines = strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")
	}
	if len(lines) != n {
		t.Fatalf("stdout has %d [dry-run] lines, want %d\n%s", len(lines), n, r.dump())
	}
	for _, line := range lines {
		m := reDryRunLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("stdout line does not match the dry-run format: %q\n%s", line, r.dump())
		}
		src, dst := m[1], m[2]
		for _, p := range []string{src, dst} {
			if !strings.HasPrefix(p, root+string(os.PathSeparator)) {
				t.Fatalf("dry-run path %q is not under the root %q\n%s", p, root, r.dump())
			}
			if _, err := os.Lstat(p); err != nil {
				t.Fatalf("dry-run path %q does not exist: %v\n%s", p, err, r.dump())
			}
		}
		if src == dst {
			t.Fatalf("dry-run line dedups a path onto itself: %q\n%s", line, r.dump())
		}
		a, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v\n%s", src, err, r.dump())
		}
		b, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read %s: %v\n%s", dst, err, r.dump())
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("dry-run line pairs files with different content: %q\n%s", line, r.dump())
		}
	}
}

// 6. All collection modes produce the same result on a fresh fixture.
func TestE2ECollectionModesEquivalent(t *testing.T) {
	requireE2E(t)
	modes := []struct {
		name string
		args []string
	}{
		{"default", nil},
		{"batch", []string{"--batch"}},
		{"low-memory", []string{"--low-memory"}},
		{"mem-budget-1", []string{"--mem-budget", "1"}},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			f := newStdFixture(t)
			e := newRunner(t)
			args := append([]string{"--hardlink", "--no-cache", "--min-size", "1"}, m.args...)
			args = append(args, f.root)
			r := e.run(t, args...)
			requireExitCode(t, r, 0)
			requireStderrMatch(t, r, reDeduped5, "5 files deduped")
			requireStderrMatch(t, r, reErrors0, "no errors")
			assertHardlinkedState(t, f, r, false)
		})
	}
}

// 7. --mem-budget 0 must behave like every other collection mode.
func TestE2ETinyMemBudget(t *testing.T) {
	requireE2E(t)
	f := newStdFixture(t)
	e := newRunner(t)
	r := e.run(t, "--hardlink", "--no-cache", "--min-size", "1", "--mem-budget", "0", f.root)
	requireExitCode(t, r, 0)
	requireStderrMatch(t, r, reDeduped5, "5 files deduped")
	requireStderrMatch(t, r, reErrors0, "no errors")
	assertHardlinkedState(t, f, r, false)
}

// 8. The size-group cache skips unchanged groups across runs.
func TestE2ECache(t *testing.T) {
	requireE2E(t)
	f := newStdFixture(t)
	e := newRunner(t) // one XDG_CACHE_HOME for the whole scenario

	// Run 1: cold cache, everything is deduped and the cache is written.
	r1 := e.run(t, "--hardlink", "--min-size", "1", f.root)
	requireExitCode(t, r1, 0)
	requireStderrMatch(t, r1, reDeduped5, "run 1 dedups 5")
	gob, err := filepath.Glob(filepath.Join(e.cacheDir, "fastdedup", "*.gob"))
	if err != nil || len(gob) == 0 {
		t.Fatalf("no *.gob cache file under %s (glob err: %v)\n%s", e.cacheDir, err, r1.dump())
	}

	// Run 2: identical tree — all three size groups are skipped via cache.
	r2 := e.run(t, "--hardlink", "--min-size", "1", f.root)
	requireExitCode(t, r2, 0)
	requireStderrMatch(t, r2, reNoNewDupCached, "run 2 reports all sizes cached")

	// Run 3: --no-cache reprocesses and finds everything already linked.
	r3 := e.run(t, "--hardlink", "--no-cache", "--min-size", "1", f.root)
	requireExitCode(t, r3, 0)
	requireStderrMatch(t, r3, reDeduped0, "run 3 dedups nothing")
	requireStderrMatch(t, r3, reAlready5, "run 3 finds 5 already deduped")

	// Run 4: the FASTDEDUP_NO_CACHE env var behaves like --no-cache.
	r4 := e.runEnv(t, map[string]string{"FASTDEDUP_NO_CACHE": "1"}, "--hardlink", "--min-size", "1", f.root)
	requireExitCode(t, r4, 0)
	requireStderrMatch(t, r4, reDeduped0, "run 4 dedups nothing")
	requireStderrMatch(t, r4, reAlready5, "run 4 finds 5 already deduped")

	// Run 5: one new file with A's content appears; the cache skips the
	// two unchanged size groups and only the changed group is processed.
	// The new duplicate joins the existing A-family inode, so exactly one
	// file is deduped no matter which order the random walk visits the
	// four files in.
	relA4 := "d3/A4.bin"
	if err := os.WriteFile(f.abs(relA4), f.content[relA1], 0644); err != nil {
		t.Fatalf("create extra A copy: %v", err)
	}
	f.content[relA4] = f.content[relA1]
	r5 := e.run(t, "--hardlink", "--min-size", "1", f.root)
	requireExitCode(t, r5, 0)
	requireStderrMatch(t, r5, reSkipped2Cached, "run 5 skips the 2 unchanged size groups")
	requireStderrMatch(t, r5, reDeduped1, "run 5 dedups only the new file")
	// The new file shares the A family inode either way.
	a4, err := os.Lstat(f.abs(relA4))
	if err != nil {
		t.Fatalf("lstat new A copy: %v\n%s", err, r5.dump())
	}
	a1 := lstat(t, f.abs(relA1))
	if !os.SameFile(a1, a4) {
		t.Fatalf("new A copy does not share A's inode\n%s", r5.dump())
	}
}

// 9. The default --min-size (524288) and explicit --min-size values gate
// which duplicate pairs are processed.
func TestE2EMinSize(t *testing.T) {
	requireE2E(t)

	t.Run("default ignores 4 KiB pair", func(t *testing.T) {
		root := newFixtureRoot(t)
		data := detBytes(21, 4096)
		p1, p2 := filepath.Join(root, "p1.bin"), filepath.Join(root, "p2.bin")
		if err := os.WriteFile(p1, data, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p2, data, 0644); err != nil {
			t.Fatal(err)
		}
		e := newRunner(t)
		r := e.run(t, "--hardlink", "--no-cache", root)
		requireExitCode(t, r, 0)
		requireStderrMatch(t, r, reNoDuplicates, "no duplicate sizes above the default minimum")
		if sameFile(t, p1, p2) {
			t.Fatalf("files below the default --min-size were deduplicated\n%s", r.dump())
		}
		for _, p := range []string{p1, p2} {
			got, err := os.ReadFile(p)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("content of %s changed\n%s", p, r.dump())
			}
		}
	})

	t.Run("default dedups 600 KiB pair", func(t *testing.T) {
		root := newFixtureRoot(t)
		data := detBytes(22, 600*1024)
		p1, p2 := filepath.Join(root, "p1.bin"), filepath.Join(root, "p2.bin")
		if err := os.WriteFile(p1, data, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p2, data, 0644); err != nil {
			t.Fatal(err)
		}
		e := newRunner(t)
		r := e.run(t, "--hardlink", "--no-cache", root)
		requireExitCode(t, r, 0)
		requireStderrMatch(t, r, reDeduped1, "600 KiB pair deduped")
		requireSameInode(t, r, "600 KiB pair", p1, p2)
	})

	t.Run("min-size 5000 splits pairs", func(t *testing.T) {
		root := newFixtureRoot(t)
		small := detBytes(23, 4096)
		big := detBytes(24, 6144)
		files := map[string][]byte{
			"s1.bin": small, "s2.bin": small,
			"b1.bin": big, "b2.bin": big,
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(root, name), data, 0644); err != nil {
				t.Fatal(err)
			}
		}
		e := newRunner(t)
		r := e.run(t, "--hardlink", "--no-cache", "--min-size", "5000", root)
		requireExitCode(t, r, 0)
		requireStderrMatch(t, r, reDeduped1, "only the 6 KiB pair deduped")
		requireDistinctInode(t, r, "4 KiB pair below --min-size 5000", filepath.Join(root, "s1.bin"), filepath.Join(root, "s2.bin"))
		requireSameInode(t, r, "6 KiB pair above --min-size 5000", filepath.Join(root, "b1.bin"), filepath.Join(root, "b2.bin"))
	})
}

// 10. Quiet mode prints exactly one summary line on stderr when work was
// done, and nothing when there was nothing left to do.
func TestE2EQuiet(t *testing.T) {
	requireE2E(t)
	f := newStdFixture(t)
	e := newRunner(t)
	args := []string{"-q", "--hardlink", "--no-cache", "--min-size", "1", f.root}

	r1 := e.run(t, args...)
	requireExitCode(t, r1, 0)
	requireStdoutEmpty(t, r1)
	quietLines := countLinesWithPrefix(r1.stderr, "fastdedup: ")
	if quietLines != 1 {
		t.Fatalf("quiet run printed %d lines starting with 'fastdedup: ', want 1\n%s", quietLines, r1.dump())
	}
	quietLineRe := regexp.MustCompile(`(?m)^fastdedup: ` + regexp.QuoteMeta(f.root) + `: 5 deduped, .+ saved, 0 already, 0 errors \(.+\)$`)
	if !quietLineRe.MatchString(r1.stderr) {
		t.Fatalf("quiet summary line does not match\n%s", r1.dump())
	}

	// Nothing left to dedup: no quiet line at all.
	r2 := e.run(t, args...)
	requireExitCode(t, r2, 0)
	if n := countLinesWithPrefix(r2.stderr, "fastdedup: "); n != 0 {
		t.Fatalf("second quiet run printed %d lines starting with 'fastdedup: ', want 0\n%s", n, r2.dump())
	}
}

func countLinesWithPrefix(s, prefix string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// 11. --raw-sizes switches the dry-run size column from human-readable to
// exact decimal byte counts.
func TestE2ERawSizes(t *testing.T) {
	requireE2E(t)
	cases := []struct {
		name    string
		raw     bool
		want    *regexp.Regexp // size column of each dry-run line
		notWant *regexp.Regexp
	}{
		{"human readable", false, regexp.MustCompile(`\(4\.0 KiB\)`), regexp.MustCompile(`\(4096\)`)},
		{"raw", true, regexp.MustCompile(`\(4096\)`), regexp.MustCompile(`KiB\)`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newStdFixture(t)
			e := newRunner(t)
			args := []string{"--dry-run", "--no-cache", "--min-size", "1"}
			if c.raw {
				args = append(args, "--raw-sizes")
			}
			args = append(args, f.root)
			r := e.run(t, args...)
			requireExitCode(t, r, 0)
			requireStdoutMatch(t, r, c.want, "the wanted size format")
			if c.notWant.MatchString(r.stdout) {
				t.Fatalf("stdout unexpectedly contains %q\n%s", c.notWant, r.dump())
			}
			requireDryRunReport(t, r, f.root, 5)
		})
	}
}

// 12. --top 1 processes only the most impactful size group.
func TestE2ETop(t *testing.T) {
	requireE2E(t)
	root := newFixtureRoot(t)
	big := detBytes(31, 32*1024)
	small := detBytes(32, 2*1024)
	files := map[string][]byte{
		"big1.bin": big, "big2.bin": big,
		"small1.bin": small, "small2.bin": small,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	e := newRunner(t)
	r := e.run(t, "--hardlink", "--no-cache", "--min-size", "1", "--top", "1", root)
	requireExitCode(t, r, 0)
	requireStderrMatch(t, r, reDeduped1, "only one pair deduped")
	// Only the pair with the larger potential savings is linked.
	requireSameInode(t, r, "big pair", filepath.Join(root, "big1.bin"), filepath.Join(root, "big2.bin"))
	requireDistinctInode(t, r, "small pair", filepath.Join(root, "small1.bin"), filepath.Join(root, "small2.bin"))
}

// 13. The per-root lock rejects concurrent runs.
func TestE2ELock(t *testing.T) {
	requireE2E(t)
	f := newStdFixture(t)
	e := newRunner(t)
	// acquireLock reads XDG_CACHE_HOME from this process's environment;
	// the child gets the same directory explicitly.
	t.Setenv("XDG_CACHE_HOME", e.cacheDir)
	lock, err := acquireLock(f.root)
	if err != nil {
		t.Fatalf("acquireLock(%s): %v", f.root, err)
	}

	args := []string{"--hardlink", "--no-cache", "--min-size", "1", f.root}
	r := e.run(t, args...)
	requireExitCode(t, r, 1)
	lockRe := regexp.MustCompile(`another fastdedup instance is already running on ` + regexp.QuoteMeta(f.root))
	requireStderrMatch(t, r, lockRe, "lock conflict message")
	requireDistinctInode(t, r, "group A after blocked run", f.abs(relA1), f.abs(relA2))

	releaseLock(lock)
	r2 := e.run(t, args...)
	requireExitCode(t, r2, 0)
	requireStderrMatch(t, r2, reDeduped5, "dedup works after the lock is released")
	requireOneInode(t, r2, "group A after successful run", f.groupAabs()...)
}

// 14. --max-time validation and graceful deadline handling.
func TestE2EMaxTime(t *testing.T) {
	requireE2E(t)

	t.Run("invalid duration", func(t *testing.T) {
		root := newFixtureRoot(t)
		e := newRunner(t)
		r := e.run(t, "--max-time", "bogus", root)
		requireExitCode(t, r, 1)
		requireStderrMatch(t, r, reInvalidMaxTime, "invalid --max-time error")
	})

	t.Run("1ns deadline stops before any dedup", func(t *testing.T) {
		f := newStdFixture(t)
		e := newRunner(t)
		before := snapshotTree(t, f.root)
		r := e.run(t, "--hardlink", "--no-cache", "--min-size", "1", "--max-time", "1ns", f.root)
		requireExitCode(t, r, 0)
		requireStderrMatch(t, r, reTimeLimit, "graceful time-limit stop")
		requireStderrMatch(t, r, reDeduped0, "nothing deduped")
		requireTreeMatches(t, f.root, before, r, true, false)
	})
}

// 15. --scrub and --defrag refuse to run without root.
func TestE2EScrubDefragRequireRoot(t *testing.T) {
	requireE2E(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root: --scrub/--defrag would proceed to the btrfs check")
	}
	root := newFixtureRoot(t)
	e := newRunner(t)
	for _, flag := range []string{"--scrub", "--defrag"} {
		r := e.run(t, flag, root)
		requireExitCode(t, r, 1)
		requireStderrMatch(t, r, reRequireRoot, "root permission requirement message")
	}
}

// 16. Dedup into read-only directories fails cleanly, writes an anonymised
// error report, and succeeds with --fix-perms.
func TestE2EReadOnlyDir(t *testing.T) {
	requireE2E(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root: read-only directories do not stop root")
	}
	root, file1, file2 := newReadOnlyFixture(t)
	data, err := os.ReadFile(file1)
	if err != nil {
		t.Fatalf("read fixture file: %v", err)
	}

	// (a) Without --fix-perms the dedup fails and an anonymised report is
	// written to the cache directory.
	e1 := newRunner(t)
	r1 := e1.run(t, "--hardlink", "--no-cache", "--min-size", "1", root)
	requireExitCode(t, r1, 1)
	requireStderrMatch(t, r1, reErrors1, "exactly one dedup error")
	requireStderrMatch(t, r1, reDeduped0, "nothing deduped")
	requireDistinctInode(t, r1, "files after failed dedup", file1, file2)
	for _, p := range []string{file1, file2} {
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("content of %s changed\n%s", p, r1.dump())
		}
	}
	reportPath := filepath.Join(e1.cacheDir, "fastdedup", "report.txt")
	reportData, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("error report %s missing: %v\n%s", reportPath, err, r1.dump())
	}
	reportText := string(reportData)
	for _, leak := range []string{root, filepath.Base(file1), filepath.Base(file2)} {
		if strings.Contains(reportText, leak) {
			t.Fatalf("error report leaks %q:\n%s", leak, reportText)
		}
	}

	// (b) FASTDEDUP_NO_REPORT_FILE=1 suppresses the report entirely.
	e2 := newRunner(t)
	e2.setenv("FASTDEDUP_NO_REPORT_FILE", "1")
	r2 := e2.run(t, "--hardlink", "--no-cache", "--min-size", "1", root)
	requireExitCode(t, r2, 1)
	requireStderrMatch(t, r2, reErrors1, "exactly one dedup error")
	if _, err := os.Lstat(filepath.Join(e2.cacheDir, "fastdedup", "report.txt")); !os.IsNotExist(err) {
		t.Fatalf("report.txt was created despite FASTDEDUP_NO_REPORT_FILE=1 (err: %v)\n%s", err, r2.dump())
	}

	// (c) --fix-perms temporarily opens the directories and restores them.
	e3 := newRunner(t)
	r3 := e3.run(t, "--hardlink", "--no-cache", "--min-size", "1", "--fix-perms", root)
	requireExitCode(t, r3, 0)
	requireStderrMatch(t, r3, reDeduped1, "files deduped with --fix-perms")
	requireSameInode(t, r3, "files after --fix-perms", file1, file2)
	for _, dir := range []string{filepath.Dir(file1), filepath.Dir(file2)} {
		fi := lstat(t, dir)
		if fi.Mode().Perm() != 0555 {
			t.Fatalf("directory %s has mode %v, want 0555 after --fix-perms\n%s", dir, fi.Mode().Perm(), r3.dump())
		}
	}
}

// newReadOnlyFixture builds the scenario-16 fixture: two identical files,
// each alone in its own 0555 directory. Because each file lives in its own
// directory the outcome is independent of the random walk order: one file
// becomes the reference, the other one always fails.
func newReadOnlyFixture(t *testing.T) (root, file1, file2 string) {
	t.Helper()
	root = newFixtureRoot(t)
	dir1 := filepath.Join(root, "ro-alpha")
	dir2 := filepath.Join(root, "ro-beta")
	for _, dir := range []string{dir1, dir2} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("create read-only fixture dir: %v", err)
		}
	}
	data := detBytes(101, 4096)
	file1 = filepath.Join(dir1, "dup-alpha.bin")
	file2 = filepath.Join(dir2, "dup-beta.bin")
	if err := os.WriteFile(file1, data, 0644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	if err := os.WriteFile(file2, data, 0644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	for _, dir := range []string{dir1, dir2} {
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatalf("chmod %s: %v", dir, err)
		}
	}
	// Restore write permission so the temp dir can be removed again.
	t.Cleanup(func() {
		_ = os.Chmod(dir1, 0755)
		_ = os.Chmod(dir2, 0755)
	})
	return root, file1, file2
}

// 17. Webhook notifications on success and failure, and the healthcheck
// ping on success.
func TestE2EWebhooks(t *testing.T) {
	requireE2E(t)
	log := &hookLog{}
	srv := httptest.NewServer(log)
	t.Cleanup(srv.Close)

	// Successful run: one update, one healthcheck ping, no alert.
	f := newStdFixture(t)
	e := newRunner(t)
	e.setenv("FASTDEDUP_WEBHOOK_UPDATES", srv.URL+"/updates")
	e.setenv("FASTDEDUP_WEBHOOK_ALERTS", srv.URL+"/alerts")
	e.setenv("FASTDEDUP_HEALTHCHECK_URL", srv.URL+"/ping")
	e.setenv("FASTDEDUP_HOST_ID", "e2e-host")
	r := e.run(t, "--hardlink", "--no-cache", "--min-size", "1", f.root)
	requireExitCode(t, r, 0)
	requireStderrMatch(t, r, reDeduped5, "5 files deduped")

	updates := log.requests("POST", "/updates")
	if len(updates) != 1 {
		t.Fatalf("got %d POST /updates, want 1\n%s", len(updates), r.dump())
	}
	if updates[0].contentType != "application/json" {
		t.Fatalf("update webhook Content-Type = %q, want application/json\n%s", updates[0].contentType, r.dump())
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(updates[0].body), &payload); err != nil {
		t.Fatalf("update webhook body is not JSON: %v\nbody: %s\n%s", err, updates[0].body, r.dump())
	}
	if !strings.Contains(payload.Text, "e2e-host") || !strings.Contains(payload.Text, f.root) {
		t.Fatalf("update webhook text lacks host id or root:\n%q\n%s", payload.Text, r.dump())
	}
	if n := len(log.requests("POST", "/alerts")); n != 0 {
		t.Fatalf("got %d POST /alerts on a successful run, want 0\n%s", n, r.dump())
	}
	if n := len(log.requests("GET", "/ping")); n != 1 {
		t.Fatalf("got %d GET /ping, want 1\n%s", n, r.dump())
	}

	// Failing run (read-only directories, no --fix-perms): one alert and
	// one update. Nothing is asserted about /ping here.
	if os.Geteuid() == 0 {
		t.Skip("read-only directory part of the webhook test cannot run as root")
	}
	before := len(log.all())
	root, _, _ := newReadOnlyFixture(t)
	e2 := newRunner(t)
	e2.setenv("FASTDEDUP_WEBHOOK_UPDATES", srv.URL+"/updates")
	e2.setenv("FASTDEDUP_WEBHOOK_ALERTS", srv.URL+"/alerts")
	e2.setenv("FASTDEDUP_HEALTHCHECK_URL", srv.URL+"/ping")
	e2.setenv("FASTDEDUP_HOST_ID", "e2e-host")
	r2 := e2.run(t, "--hardlink", "--no-cache", "--min-size", "1", root)
	requireExitCode(t, r2, 1)
	requireStderrMatch(t, r2, reErrors1, "exactly one dedup error")

	since := log.all()[before:]
	alerts := filterRequests(since, "POST", "/alerts")
	if len(alerts) != 1 {
		t.Fatalf("got %d POST /alerts after the failing run, want 1\n%s", len(alerts), r2.dump())
	}
	if err := json.Unmarshal([]byte(alerts[0].body), &payload); err != nil {
		t.Fatalf("alert webhook body is not JSON: %v\nbody: %s\n%s", err, alerts[0].body, r2.dump())
	}
	if !strings.Contains(payload.Text, "e2e-host") {
		t.Fatalf("alert webhook text lacks host id:\n%q\n%s", payload.Text, r2.dump())
	}
	if n := len(filterRequests(since, "POST", "/updates")); n != 1 {
		t.Fatalf("got %d POST /updates after the failing run, want 1\n%s", n, r2.dump())
	}
}

// hookLog records method, path, Content-Type and body of every request.
type hookLog struct {
	mu   sync.Mutex
	reqs []hookRecord
}

type hookRecord struct {
	method      string
	path        string
	contentType string
	body        string
}

func (h *hookLog) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	h.mu.Lock()
	h.reqs = append(h.reqs, hookRecord{
		method:      req.Method,
		path:        req.URL.Path,
		contentType: req.Header.Get("Content-Type"),
		body:        string(body),
	})
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (h *hookLog) all() []hookRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]hookRecord, len(h.reqs))
	copy(out, h.reqs)
	return out
}

func (h *hookLog) requests(method, path string) []hookRecord {
	return filterRequests(h.all(), method, path)
}

func filterRequests(reqs []hookRecord, method, path string) []hookRecord {
	var out []hookRecord
	for _, r := range reqs {
		if r.method == method && r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// 18. Default reflink mode: dedups on reflink-capable filesystems, fails
// cleanly with FICLONE errors elsewhere.
func TestE2EReflink(t *testing.T) {
	requireE2E(t)
	f := newStdFixture(t)
	applyDistinctiveMetadata(t, f)
	before := snapshotTree(t, f.root)

	// Probe FICLONE support on the filesystem hosting the fixtures.
	supported := reflinkSupported(t, filepath.Dir(f.root))
	if !supported && os.Getenv("FASTDEDUP_E2E_REQUIRE_REFLINK") == "1" {
		t.Fatalf("FASTDEDUP_E2E_REQUIRE_REFLINK=1 but FICLONE is not supported on %s — the reflink code path is not being exercised", filepath.Dir(f.root))
	}

	e := newRunner(t)
	args := []string{"-v", "--no-cache", "--min-size", "1", f.root}
	r := e.run(t, args...)

	if !supported {
		// Unsupported filesystem: every dedup attempt fails with a
		// FICLONE ioctl error, nothing changes on disk.
		requireExitCode(t, r, 1)
		requireStderrMatch(t, r, reDeduped0, "nothing deduped")
		requireStderrMatch(t, r, reErrors5, "5 dedup errors (2 group A, 1 group B, 2 group D)")
		requireStderrMatch(t, r, reFICLONE, "FICLONE ioctl error text")
		requireTreeMatches(t, f.root, before, r, true, true) // inode numbers too
		requireNoDedupTmp(t, f.root, r)
		return
	}

	// Reflink-capable filesystem.
	requireExitCode(t, r, 0)
	requireStderrMatch(t, r, reDeduped5, "5 files deduped")
	requireStderrMatch(t, r, reErrors0, "no errors")
	// Reflinks preserve per-file metadata but give every file its own
	// inode, so inode numbers are intentionally not compared here.
	requireTreeMatches(t, f.root, before, r, false, true)
	var absRels []string
	for _, rel := range f.allRegularRels() {
		absRels = append(absRels, f.abs(rel))
	}
	requireDistinctInodes(t, r, "every regular file", absRels...)
	requireNoDedupTmp(t, f.root, r)

	// Reflinked A files share physical extents (only checkable where the
	// FIEMAP ioctl works; must happen before the CoW overwrite below).
	e1, err1 := getExtents(f.abs(relA1))
	e2, err2 := getExtents(f.abs(relA2))
	e3, err3 := getExtents(f.abs(relA3))
	if err1 == nil && err2 == nil && err3 == nil {
		if !SameExtents(e1, e2) || !SameExtents(e1, e3) {
			t.Fatalf("A files do not share extents after reflink\n%s", r.dump())
		}
	}

	// A second run recognises the reflinks as already deduped.
	r2 := e.run(t, args...)
	requireExitCode(t, r2, 0)
	requireStderrMatch(t, r2, reDeduped0, "second run dedups nothing")
	requireStderrMatch(t, r2, reAlready5, "second run finds 5 already deduped")

	// Copy-on-write isolation: overwriting one A file must not change
	// the others.
	newHead := detBytes(4242, 32)
	a1, err := os.OpenFile(f.abs(relA1), os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s for writing: %v", relA1, err)
	}
	if _, err := a1.Write(newHead); err != nil {
		t.Fatalf("overwrite %s: %v", relA1, err)
	}
	if err := a1.Close(); err != nil {
		t.Fatalf("close %s after overwrite: %v", relA1, err)
	}
	origA := f.content[relA1]
	for _, rel := range []string{relA2, relA3} {
		got, err := os.ReadFile(f.abs(rel))
		if err != nil || !bytes.Equal(got, origA) {
			t.Fatalf("CoW isolation broken: %s changed when %s was overwritten\n%s", rel, relA1, r.dump())
		}
	}
}

// applyDistinctiveMetadata gives every regular fixture file a repeating
// pattern of permission bits and a unique mtime, so tests can detect any
// metadata loss across a dedup.
func applyDistinctiveMetadata(t *testing.T, f *stdFixture) {
	t.Helper()
	modes := []os.FileMode{0600, 0640, 0644}
	base := time.Date(2018, 5, 4, 3, 2, 1, 0, time.UTC)
	for i, rel := range f.allRegularRels() {
		p := f.abs(rel)
		if err := os.Chmod(p, modes[i%len(modes)]); err != nil {
			t.Fatalf("chmod %s: %v", rel, err)
		}
		ts := base.Add(time.Duration(i) * 13 * time.Minute)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatalf("chtimes %s: %v", rel, err)
		}
	}
}
