//go:build unix

package agent

import (
	"os"
	"syscall"
)

// fileInode returns fi's inode number, part of the identity the shell
// re-stamp compares (file_stamps.go): a formatter that replaces a file by
// renaming a new one over it changes the inode even when size and mtime
// happen to match.
func fileInode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
