//go:build !unix

package agent

import "os"

// fileInode is 0 where os.FileInfo carries no inode (Windows). There is no
// ctime either (file_ctime_other.go), so the shell re-stamp hashes a file
// before trusting it (file_stamps.go).
func fileInode(os.FileInfo) uint64 { return 0 }
