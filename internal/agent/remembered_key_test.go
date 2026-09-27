package agent

import "testing"

// A remembered command is keyed the way a shell splits words: runs of ASCII
// spaces and tabs fold to one space, but a no-break space (or any other
// Unicode space) stays, since the shell keeps it inside a word. Folding it
// would make "always allow" on a harmless one-argument command remember the
// command that deletes the home directory.
func TestRememberedCommandKeyFoldsOnlyShellWhitespace(t *testing.T) {
	if got := RememberedCommandKey("  go\t test   ./... "); got != "go test ./..." {
		t.Fatalf("ASCII whitespace: %q", got)
	}
	harmless := "rm -rf ./build/\u00a0~/"
	if got := RememberedCommandKey(harmless); got != harmless || got == "rm -rf ./build/ ~/" {
		t.Fatalf("no-break space folded: %q", got)
	}
}
