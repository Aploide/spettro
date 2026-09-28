//go:build !darwin && !linux

package agent

import "os"

// fileCtime is 0 where Spettro does not read a change time from
// os.FileInfo (Windows has none there). An identity without one is never
// trusted on its own, so the shell re-stamp hashes the file instead
// (identityTrusted in file_stamps.go).
func fileCtime(os.FileInfo) int64 { return 0 }
