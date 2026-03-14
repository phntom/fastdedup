package main

import (
	"testing"
	"time"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name string
		b    int64
		raw  bool
		want string
	}{
		{"zero", 0, false, "0 B"},
		{"one byte", 1, false, "1 B"},
		{"1023 bytes", 1023, false, "1023 B"},
		{"exactly 1 KiB", 1024, false, "1.0 KiB"},
		{"1.5 KiB", 1536, false, "1.5 KiB"},
		{"exactly 1 MiB", 1048576, false, "1.0 MiB"},
		{"1.5 GiB", 1610612736, false, "1.5 GiB"},
		{"exactly 1 GiB", 1073741824, false, "1.0 GiB"},
		{"exactly 1 TiB", 1099511627776, false, "1.0 TiB"},
		{"5 TiB", 5 * 1099511627776, false, "5.0 TiB"},
		{"raw zero", 0, true, "0"},
		{"raw large", 1048576, true, "1048576"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatSize(tt.b, tt.raw)
			if got != tt.want {
				t.Errorf("formatSize(%d, %v) = %q, want %q", tt.b, tt.raw, got, tt.want)
			}
		})
	}
}

func TestFormatCount(t *testing.T) {
	tests := []struct {
		name string
		n    int64
		want string
	}{
		{"zero", 0, "0"},
		{"single digit", 5, "5"},
		{"three digits", 999, "999"},
		{"four digits", 1000, "1,000"},
		{"six digits", 123456, "123,456"},
		{"seven digits", 1234567, "1,234,567"},
		{"ten digits", 1234567890, "1,234,567,890"},
		{"negative single", -5, "-5"},
		{"negative thousands", -1000, "-1,000"},
		{"negative millions", -1234567, "-1,234,567"},
		{"exact boundary", 1000, "1,000"},
		{"max int64", 9223372036854775807, "9,223,372,036,854,775,807"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatCount(tt.n)
			if got != tt.want {
				t.Errorf("formatCount(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestFormatETA(t *testing.T) {
	tests := []struct {
		name    string
		elapsed time.Duration
		current int64
		total   int64
		want    string
	}{
		{"half done 10s", 10 * time.Second, 50, 100, "~10s"},
		{"quarter done 1m", time.Minute, 25, 100, "~3.0m"},
		{"90% done 9m", 9 * time.Minute, 90, 100, "~1.0m"},
		{"half done 2h", 2 * time.Hour, 50, 100, "~2.0h"},
		{"zero current", time.Second, 0, 100, ""},
		{"zero total", time.Second, 50, 0, ""},
		{"current exceeds total", time.Second, 101, 100, ""},
		{"done", time.Second, 100, 100, "~0s"},
		{"large values 32bit safe", 10 * time.Second, 5_000_000_000, 10_000_000_000, "~10s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatETA(tt.elapsed, tt.current, tt.total)
			if got != tt.want {
				t.Errorf("formatETA(%v, %d, %d) = %q, want %q", tt.elapsed, tt.current, tt.total, got, tt.want)
			}
		})
	}
}
