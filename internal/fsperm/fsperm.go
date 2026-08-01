// Package fsperm restricts files and directories to their owner on every
// platform.
//
// spettro keeps real secrets on disk — provider API keys in keys.enc, the
// master key that decrypts them, MCP auth tokens, Telegram bot tokens — and
// protects them with 0600/0700 modes. Those modes are close to meaningless on
// Windows: os.Chmod there only toggles the read-only attribute, and a new file
// simply inherits its parent directory's ACL. A project checkout on a shared
// drive or outside the user profile therefore gets no protection at all from a
// mode argument.
//
// RestrictToOwner expresses the intent directly: chmod on Unix, an explicit
// non-inherited DACL naming only the current user on Windows.
package fsperm

import "os"

// RestrictToOwner makes path readable and writable only by the current user,
// replacing any access inherited from the parent directory.
func RestrictToOwner(path string) error { return restrictToOwner(path) }

// IsOwnerOnly reports whether path grants access to nobody but its owner. It
// exists so tests can assert the guarantee in the platform's own terms rather
// than comparing Unix mode bits that Windows does not implement.
func IsOwnerOnly(path string) (bool, error) { return isOwnerOnly(path) }

// SecureMkdirAll creates dir along with any missing parents and restricts dir
// itself to the current user. Restricting the directory rather than each file
// is what makes this tractable: on Windows the entry is inheritable, so every
// secret written inside afterwards starts out owner-only without each write
// site having to remember.
//
// The restriction is reapplied even when dir already exists, so a store
// created by an older version — or by a Windows build that predates this —
// is tightened on the next run rather than staying open forever.
func SecureMkdirAll(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return RestrictToOwner(dir)
}
