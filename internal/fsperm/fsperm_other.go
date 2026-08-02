//go:build !windows

package fsperm

import "os"

// restrictToOwner applies the mode that matches the path's kind: 0700 for a
// directory (which needs the execute bit to be traversable), 0600 for a file.
func restrictToOwner(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info.IsDir() {
		mode = 0o700
	}
	return os.Chmod(path, mode)
}

// isOwnerOnly reports whether the group and other permission bits are clear.
func isOwnerOnly(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o077 == 0, nil
}
