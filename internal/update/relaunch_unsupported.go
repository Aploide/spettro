//go:build !darwin && !linux

package update

import "errors"

// Relaunch is not implemented on this platform; the caller falls back to
// telling the user to restart manually.
func Relaunch(binaryPath string) error {
	return errors.New("auto-relaunch is not supported on this platform")
}
