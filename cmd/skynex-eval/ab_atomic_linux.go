//go:build linux

package main

import "golang.org/x/sys/unix"

// exchangeABFiles atomically swaps two existing paths. A/B OAuth execution is
// Linux-only in v1, and RENAME_EXCHANGE lets checkpoint replacement preserve a
// raced-in file instead of overwriting it.
func exchangeABFiles(left, right string) error {
	return unix.Renameat2(unix.AT_FDCWD, left, unix.AT_FDCWD, right, unix.RENAME_EXCHANGE)
}
