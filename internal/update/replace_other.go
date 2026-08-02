//go:build !windows

package update

import (
	"io"
	"os"
)

// replaceExecutable installs newPath as target. Renaming is atomic and,
// crucially, safe to do onto a currently-running executable on Unix (the
// running process keeps its old inode open until it exits); a same-directory
// temp file (see downloadToTemp/extractBinary) keeps this a same-filesystem
// rename in the common case. If rename isn't possible we fall back to a
// copy onto a fresh inode: the target is unlinked first, never truncated in
// place — on macOS, rewriting an existing executable's inode leaves the
// kernel's cached code signature stale and the binary is SIGKILLed at launch.
func replaceExecutable(newPath, target string) error {
	if err := os.Rename(newPath, target); err == nil {
		return nil
	}
	src, err := os.Open(newPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	os.Remove(newPath)
	return nil
}
