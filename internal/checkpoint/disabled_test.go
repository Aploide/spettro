package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

// A platform without subprocess execution has no git, so OpenWith short-circuits
// to the disabled Checkpointer (see the platform.CanExec() branch there). That
// branch is a build-time constant and cannot be flipped from a desktop test, so
// this test drives the identical state through Options{Disabled: true} — the two
// paths differ only in the message on the returned error. What matters, and what
// is asserted here, is that "disabled" is a fully inert Checkpointer rather than
// a half-built one: nothing panics, nothing blocks, and no operation reaches git.
func TestDisabledCheckpointerIsInert(t *testing.T) {
	globalDir := t.TempDir()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := OpenWith(globalDir, project, Options{Disabled: true})
	if err == nil {
		t.Fatal("disabled OpenWith must report why it is disabled")
	}
	if c == nil {
		t.Fatal("disabled OpenWith must still return a usable Checkpointer, not nil: callers store it unconditionally and would panic")
	}

	// Every operation must fail closed and return, never exec and never hang.
	if _, err := c.Snapshot("file-write", "prompt", []byte("{}")); err == nil {
		t.Error("Snapshot on a disabled checkpointer must return an error")
	}
	if err := c.RestoreFiles("whatever"); err == nil {
		t.Error("RestoreFiles on a disabled checkpointer must return an error")
	}
	if n := c.ChangesSince("whatever"); n != 0 {
		t.Errorf("ChangesSince on a disabled checkpointer = %d, want 0", n)
	}
	if list, err := c.List(); err != nil || len(list) != 0 {
		t.Errorf("List on a disabled checkpointer = (%v, %v), want (empty, nil)", list, err)
	}
	if w := c.Warning(); w != "" {
		t.Errorf("Warning on a disabled checkpointer = %q, want empty", w)
	}

	// And it must not have created a shadow repository on the way out: the
	// early return happens before any filesystem work.
	if _, err := os.Stat(filepath.Join(Dir(globalDir, project), "repo.git")); !os.IsNotExist(err) {
		t.Errorf("disabled OpenWith created a shadow repo: stat err = %v", err)
	}
}
