//go:build linux

// Package unmount provides FUSE mount detach and abort helpers (linux only).
package unmount

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// ConnectionsDir is the base directory for fusectl connections; overridable
// in tests.
var ConnectionsDir = "/sys/fs/fuse/connections"

// AbortConnection writes "1" to the abort file of the given FUSE connection
// minor number. A missing connection (already gone) is not an error.
func AbortConnection(minor int) error {
	path := filepath.Join(ConnectionsDir, strconv.Itoa(minor), "abort")
	err := os.WriteFile(path, []byte("1"), 0)
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("abort fuse connection %d: %w", minor, err)
}

// Detach performs a lazy unmount (MNT_DETACH) of path.
func Detach(path string) error {
	return classify(syscall.Unmount(path, syscall.MNT_DETACH), path)
}

// classify translates a raw Unmount error into a package error, treating
// "already unmounted" conditions as success.
func classify(err error, path string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("detach %s: permission denied (root required): %w", path, err)
	}
	return fmt.Errorf("detach %s: %w", path, err)
}
