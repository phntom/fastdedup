//go:build !unix

package main

import "os"

// acquireLock is a no-op on non-Unix platforms.
// Actual dedup operations will fail with platform-specific errors.
func acquireLock(_ string) (*os.File, error) {
	return nil, nil
}

// releaseLock is a no-op on non-Unix platforms.
func releaseLock(_ *os.File) {}
