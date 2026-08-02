// Package safeio provides file primitives that behave the same on Windows as
// they do on Unix.
package safeio

import (
	"errors"
	"os"
	"runtime"
	"time"
)

// replaceAttempts and replaceBackoff bound the retry window at roughly 100ms,
// which is far longer than a reader needs to finish a small JSON file and
// short enough that a genuinely stuck writer still reports an error promptly.
const (
	replaceAttempts = 20
	replaceBackoff  = 5 * time.Millisecond
)

// Replace moves tmp onto dst, replacing dst if it exists.
//
// On Unix this is a plain rename: the operation is atomic and unaffected by
// anyone holding dst open, because the old inode simply lives on until its
// last reader closes it.
//
// Windows has no such guarantee. Go opens files without FILE_SHARE_DELETE, so
// while any reader has dst open the rename fails outright with "Access is
// denied" — and every one of these call sites is a read-modify-write store
// (session tasks, config, secrets, the MCP server list) that concurrent
// readers poll. The failure is transient by nature, so it is retried briefly
// rather than surfaced; the alternative is a save that randomly loses data
// whenever a reader happens to overlap.
func Replace(tmp, dst string) error {
	err := os.Rename(tmp, dst)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	for range replaceAttempts {
		if !isTransientReplaceError(err) {
			return err
		}
		time.Sleep(replaceBackoff)
		if err = os.Rename(tmp, dst); err == nil {
			return nil
		}
	}
	return err
}

// ReadFile reads path, tolerating the moment a concurrent Replace is swapping
// it in.
//
// This is the reader half of the same Windows limitation Replace works around.
// A rename onto an open destination is not atomic from a reader's point of
// view there: while the swap is in flight an open fails outright with a
// sharing violation, so a poller reading a store that another goroutine is
// saving sees a spurious error rather than either the old or the new contents.
// On Unix the rename is atomic and the first read always succeeds.
func ReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil || runtime.GOOS != "windows" {
		return data, err
	}
	for range replaceAttempts {
		// A genuinely missing file is not contention; report it immediately so
		// callers testing for os.ErrNotExist are not delayed.
		if !isTransientReplaceError(err) {
			return data, err
		}
		time.Sleep(replaceBackoff)
		if data, err = os.ReadFile(path); err == nil {
			return data, nil
		}
	}
	return data, err
}

// isTransientReplaceError reports whether err is the kind of contention that
// retrying can clear: another handle on the destination, or the brief window
// where an antivirus or indexer has the file open.
func isTransientReplaceError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, errSharingViolation)
}
