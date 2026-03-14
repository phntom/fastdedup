//go:build unix

package main

import (
	"testing"
)

func TestAcquireLock(t *testing.T) {
	t.Run("basic acquire and release", func(t *testing.T) {
		dir := t.TempDir()
		f, err := acquireLock(dir)
		if err != nil {
			t.Fatalf("acquireLock failed: %v", err)
		}
		if f == nil {
			t.Fatal("acquireLock returned nil file")
		}
		releaseLock(f)
	})

	t.Run("double lock same root fails", func(t *testing.T) {
		dir := t.TempDir()
		f1, err := acquireLock(dir)
		if err != nil {
			t.Fatalf("first acquireLock failed: %v", err)
		}
		defer releaseLock(f1)

		f2, err := acquireLock(dir)
		if err == nil {
			releaseLock(f2)
			t.Fatal("second acquireLock on same root should fail")
		}
	})

	t.Run("different roots lock independently", func(t *testing.T) {
		d1 := t.TempDir()
		d2 := t.TempDir()
		f1, err := acquireLock(d1)
		if err != nil {
			t.Fatalf("acquireLock d1 failed: %v", err)
		}
		defer releaseLock(f1)

		f2, err := acquireLock(d2)
		if err != nil {
			t.Fatalf("acquireLock d2 failed: %v", err)
		}
		defer releaseLock(f2)
	})

	t.Run("release then reacquire", func(t *testing.T) {
		dir := t.TempDir()
		f1, err := acquireLock(dir)
		if err != nil {
			t.Fatalf("first acquireLock failed: %v", err)
		}
		releaseLock(f1)

		f2, err := acquireLock(dir)
		if err != nil {
			t.Fatalf("reacquire after release failed: %v", err)
		}
		releaseLock(f2)
	})

	t.Run("release nil is safe", func(t *testing.T) {
		releaseLock(nil) // should not panic
	})

	t.Run("deterministic lock path", func(t *testing.T) {
		// Same root should always use the same lock file.
		dir := t.TempDir()
		f1, err := acquireLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		name1 := f1.Name()
		releaseLock(f1)

		f2, err := acquireLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		name2 := f2.Name()
		releaseLock(f2)

		if name1 != name2 {
			t.Errorf("lock paths differ for same root: %q vs %q", name1, name2)
		}
	})
}
