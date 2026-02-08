package main

import "sort"

// SizeEntry holds a file size and how many times it was encountered.
type SizeEntry struct {
	Size  int64
	Count int64
}

// Impact returns the total byte impact: size * count.
func (e SizeEntry) Impact() int64 {
	return e.Size * e.Count
}

// SizeMap is a bounded map from file size to occurrence count.
// When capacity is exceeded, the least impactful entries (lowest size*count)
// are evicted in batches of 10% to amortize the cost.
type SizeMap struct {
	m       map[int64]int64
	maxSize int
}

// NewSizeMap creates a SizeMap that holds at most maxSize unique entries.
func NewSizeMap(maxSize int) *SizeMap {
	return &SizeMap{
		m:       make(map[int64]int64, maxSize),
		maxSize: maxSize,
	}
}

// Add records one occurrence of a file with the given size.
func (sm *SizeMap) Add(size int64) {
	sm.m[size]++
	if len(sm.m) > sm.maxSize {
		sm.evict()
	}
}

// Len returns the number of distinct sizes tracked.
func (sm *SizeMap) Len() int {
	return len(sm.m)
}

// TopN returns the top n entries with count >= 2, ranked by impact descending.
func (sm *SizeMap) TopN(n int) []SizeEntry {
	entries := make([]SizeEntry, 0, len(sm.m))
	for size, count := range sm.m {
		if count >= 2 {
			entries = append(entries, SizeEntry{Size: size, Count: count})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Impact() > entries[j].Impact()
	})

	return entries[:min(n, len(entries))]
}

// evict removes the bottom 10% of entries by impact.
func (sm *SizeMap) evict() {
	evictCount := sm.maxSize / 10
	if evictCount < 1 {
		evictCount = 1
	}

	type entry struct {
		size   int64
		impact int64
	}
	all := make([]entry, 0, len(sm.m))
	for size, count := range sm.m {
		all = append(all, entry{size, size * count})
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].impact < all[j].impact
	})

	for i := range min(evictCount, len(all)) {
		delete(sm.m, all[i].size)
	}
}
