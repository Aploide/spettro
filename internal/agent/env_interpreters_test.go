package agent

import (
	"reflect"
	"runtime"
	"testing"
)

// A hint is given only when the bare command is missing and the versioned
// one exists; both present, or neither, says nothing.
func TestInterpreterHintLines(t *testing.T) {
	cases := []struct {
		name      string
		installed []string
		want      []string
	}{
		{"only versioned", []string{"python3", "pip3"}, []string{
			"- Use `python3` (`python` is not installed)",
			"- Use `pip3` (`pip` is not installed)",
		}},
		{"python3 only, pip present", []string{"python3", "pip", "pip3"}, []string{
			"- Use `python3` (`python` is not installed)",
		}},
		{"both names", []string{"python", "python3", "pip", "pip3"}, nil},
		{"bare only", []string{"python", "pip"}, nil},
		{"nothing", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := map[string]bool{}
			for _, n := range tc.installed {
				set[n] = true
			}
			got := interpreterHintLines(func(n string) bool { return set[n] })
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// probeCommands asks the bash tool's own shell, so it finds a command every
// POSIX host has and misses one that cannot exist.
func TestProbeCommandsUsesToolShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the POSIX probe path is exercised on Unix hosts")
	}
	got := probeCommands([]string{"sh", "spettro-no-such-command-xyz"})
	if !got["sh"] || got["spettro-no-such-command-xyz"] {
		t.Fatalf("probe = %v, want sh only", got)
	}
}

// The hint lines are cached for the process, so the Environment section,
// part of the cached system prompt, is identical on every call.
func TestEnvironmentBriefIsStable(t *testing.T) {
	if a, b := environmentBrief(), environmentBrief(); a != b {
		t.Fatalf("environmentBrief changed between calls:\n%s\n---\n%s", a, b)
	}
}
