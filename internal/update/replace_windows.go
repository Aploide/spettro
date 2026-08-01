//go:build windows

package update

import (
	"fmt"
	"os"
)

// backupSuffix marks the displaced previous build. It stays on disk until
// something can delete it, which is never during the run that created it.
const backupSuffix = ".old"

// replaceExecutable installs newPath as target while target is the image of
// the running process.
//
// The Unix approach — rename over the target, or unlink and rewrite — cannot
// work here: Windows holds an executing image open and refuses both deletion
// and overwrite with ERROR_ACCESS_DENIED. It does allow the running image to
// be *renamed*, because a rename leaves the file object intact. So the current
// build is moved aside, the new one takes its name, and the displaced file is
// deleted on a best-effort basis — that delete fails while the old build is
// still running and succeeds on the next update, which is why the stale
// backup is cleared before starting rather than only after.
//
// If the second rename fails the first is undone, so a failed update leaves a
// working executable rather than a missing one.
func replaceExecutable(newPath, target string) error {
	backup := target + backupSuffix
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale backup %s: %w", backup, err)
	}
	if err := os.Rename(target, backup); err != nil {
		return fmt.Errorf("move running executable aside: %w", err)
	}
	if err := os.Rename(newPath, target); err != nil {
		// Restore the previous build; without this the CLI would be gone.
		if restoreErr := os.Rename(backup, target); restoreErr != nil {
			return fmt.Errorf("install update: %w (and the previous build could not be restored from %s: %v)", err, backup, restoreErr)
		}
		return fmt.Errorf("install update: %w", err)
	}
	// Expected to fail while this process is still running the old image.
	_ = os.Remove(backup)
	return nil
}
