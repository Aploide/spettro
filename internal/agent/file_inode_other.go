//go:build !unix

package agent

import "os"

// fileInode is 0 where os.FileInfo carries no inode (Windows): the shell
// re-stamp then compares size and mtime only (file_stamps.go).
func fileInode(os.FileInfo) uint64 { return 0 }
