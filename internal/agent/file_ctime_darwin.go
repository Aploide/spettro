//go:build darwin

package agent

import (
	"os"
	"syscall"
)

// fileCtime returns fi's inode change time (UnixNano), part of the identity
// the shell re-stamp compares (file_stamps.go): unlike the mtime, no
// user-space call can set it back, so it moves on every write even when
// the writer restores the old mtime. 0 when unknown.
func fileCtime(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctimespec.Nano()
	}
	return 0
}
