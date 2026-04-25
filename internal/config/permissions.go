//go:build !windows

package config

import (
	"fmt"
	"io/fs"
	"os"
)

const (
	// BeadsDirPerm is the permission mode for .beads/ directories (owner-only).
	BeadsDirPerm fs.FileMode = 0700
	// BeadsFilePerm is the permission mode for state files inside .beads/ (owner-only).
	BeadsFilePerm fs.FileMode = 0600
)

// EnsureBeadsDir creates the .beads directory with secure permissions.
func EnsureBeadsDir(path string) error {
	return os.MkdirAll(path, BeadsDirPerm)
}

// CheckBeadsDirPermissions warns to stderr if the .beads directory has
// group or world-accessible permissions. The check is non-fatal.
func CheckBeadsDirPermissions(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return // directory doesn't exist yet
	}
	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		fmt.Fprintf(os.Stderr, "Warning: %s has permissions %04o (recommended: 0700). Run: chmod 700 %s\n", path, perm, path)
	}
}

// FixBeadsDirPermissions strips world-accessible bits from the .beads
// directory. Group bits (0070) are left untouched — users may
// intentionally set 0750 for shared access. Returns true if permissions
// were changed.
func FixBeadsDirPermissions(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, nil // directory doesn't exist yet
	}
	perm := info.Mode().Perm()
	if perm&0007 == 0 {
		return false, nil // no world-accessible bits
	}
	fixed := perm &^ 0007 // strip other bits, preserve owner+group
	if err := os.Chmod(path, fixed); err != nil {
		return false, fmt.Errorf("failed to chmod %s to %04o: %w", path, fixed, err)
	}
	return true, nil
}
