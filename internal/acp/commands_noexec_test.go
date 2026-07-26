package acp

import (
	"slices"
	"testing"
)

// Build-tag-independent: availableCommands takes the capability explicitly so
// the iOS command surface is checkable from a desktop test run.

func TestAvailableCommandsDropsExecOnlyOnesWithoutExec(t *testing.T) {
	names := func(canExec bool) []string {
		out := make([]string, 0, len(acpAvailableCommands))
		for _, c := range availableCommands(canExec) {
			out = append(out, c.Name)
		}
		return out
	}

	withExec := names(true)
	for want := range execDependentCommands {
		if !slices.Contains(withExec, want) {
			t.Fatalf("%q is gated but not advertised on desktop — stale gate?", want)
		}
	}

	noExec := names(false)
	for bad := range execDependentCommands {
		if slices.Contains(noExec, bad) {
			t.Errorf("%q advertised with exec unavailable", bad)
		}
	}

	// Everything else must survive untouched and in order: these commands are
	// the app's only in-chat control surface, and losing one silently would
	// be worse than losing the shell.
	var want []string
	for _, n := range withExec {
		if _, blocked := execDependentCommands[n]; !blocked {
			want = append(want, n)
		}
	}
	if !slices.Equal(noExec, want) {
		t.Errorf("surviving command list changed:\n got %v\nwant %v", noExec, want)
	}
	for _, must := range []string{"help", "mode", "models", "clear", "compact", "memory", "tasks", "plan"} {
		if !slices.Contains(noExec, must) {
			t.Errorf("%q must remain available without exec", must)
		}
	}
}
