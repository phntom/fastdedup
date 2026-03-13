package main

import "path/filepath"

// DirIntern deduplicates directory strings so files in the same directory
// share a single backing string allocation.
type DirIntern struct {
	dirs map[string]string
}

// NewDirIntern creates a new directory string interner.
func NewDirIntern() *DirIntern {
	return &DirIntern{dirs: make(map[string]string)}
}

// Intern returns a shared copy of the directory string.
// The returned cost is the memory of the backing string data, positive
// only when the directory is seen for the first time.
func (d *DirIntern) Intern(dir string) (interned string, cost int64) {
	if existing, ok := d.dirs[dir]; ok {
		return existing, 0
	}
	d.dirs[dir] = dir
	return dir, int64(len(dir)) + 64 // string data + map entry overhead
}

// CompactPath stores a file path with an interned directory component.
// Multiple CompactPaths in the same directory share the Dir allocation.
type CompactPath struct {
	Dir  string // interned via DirIntern
	Name string // base filename
}

// String returns the full file path.
func (p CompactPath) String() string {
	return filepath.Join(p.Dir, p.Name)
}

// MemCost returns the estimated per-entry memory cost, excluding the shared Dir.
func (p CompactPath) MemCost() int64 {
	return 32 + int64(len(p.Name)) // two string headers (16 each) + Name backing data
}

// ExpandPaths converts a slice of CompactPaths back to full path strings.
func ExpandPaths(compact []CompactPath) []string {
	paths := make([]string, len(compact))
	for i, cp := range compact {
		paths[i] = cp.String()
	}
	return paths
}
