//go:build darwin || linux

package update

import (
	"os"
	"syscall"
)

// Relaunch replaces the current process image with binaryPath, preserving
// argv/envp so the CLI restarts exactly as it was invoked. It only returns on
// failure.
func Relaunch(binaryPath string) error {
	return syscall.Exec(binaryPath, os.Args, os.Environ())
}
