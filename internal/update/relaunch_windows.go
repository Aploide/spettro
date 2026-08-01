//go:build windows

package update

import (
	"errors"
	"os"
	"os/exec"
)

// Relaunch runs binaryPath in place of the current process, preserving argv
// and the environment so the CLI restarts exactly as it was invoked. It only
// returns on failure.
//
// Windows has no execve that replaces a process image, so the new build is
// started as a child that inherits this console and the standard streams. The
// parent then waits for it and exits with its status rather than returning
// immediately: letting the parent exit first would hand the console back to
// the launching shell, which would print its prompt over the restarted TUI.
func Relaunch(binaryPath string) error {
	cmd := exec.Command(binaryPath, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}

	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		os.Exit(0)
	case errors.As(err, &exitErr):
		os.Exit(exitErr.ExitCode())
	}
	return err
}
