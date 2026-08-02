// Package homedir resolves the user's home directory with consistent
// semantics on every platform.
package homedir

import (
	"os"
	"path/filepath"
	"runtime"
)

// Dir returns the directory that holds the user's ~/.spettro tree.
//
// os.UserHomeDir reads $HOME on Unix but %USERPROFILE% on Windows, so a
// Windows process cannot relocate its home by exporting $HOME the way it can
// everywhere else. That asymmetry is not academic: it silently pointed the
// test suite — and any tool that drives spettro with an explicit HOME — at the
// real ~/.spettro, so tests read and overwrote live credentials.
//
// Dir honours $HOME on every platform. On Windows the value must be a genuine
// absolute path (drive- or UNC-rooted), which rejects the POSIX-style HOME
// that Git Bash and MSYS2 export to native children ("/c/Users/alice") instead
// of building paths from it that no Windows API can open.
func Dir() (string, error) {
	if h := os.Getenv("HOME"); h != "" {
		if runtime.GOOS != "windows" || filepath.IsAbs(h) {
			return h, nil
		}
	}
	return os.UserHomeDir()
}
