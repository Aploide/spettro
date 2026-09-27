package agent

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"

	"spettro/internal/shell"
)

// Interpreter hints for the Environment section.
//
// Models type `python` and `pip` by habit. Many hosts (current macOS, Debian
// and Ubuntu without python-is-python3, slim container images) install only
// `python3` and `pip3`, so the first attempt fails with "command not found"
// and costs a turn; the round-5 bench counted 11 such turns for Kimi, 5 for
// GLM and 6 for DeepSeek. When the bare name is missing but the versioned one
// is present, the Environment section says so up front.
//
// The probe runs once per process and its answer is reused: the Environment
// section must stay byte-stable for the whole conversation (it is part of the
// cached system prompt), and a tool installed mid-session is rare enough not
// to be worth a prompt-cache miss.

// interpreterAlias pairs the bare command a model tends to type with the
// versioned command some hosts install instead.
type interpreterAlias struct {
	bare      string // e.g. "python"
	versioned string // e.g. "python3"
}

// interpreterAliases lists the pairs the Environment section checks, in the
// order their hints are rendered.
var interpreterAliases = []interpreterAlias{
	{bare: "python", versioned: "python3"},
	{bare: "pip", versioned: "pip3"},
}

// interpreterProbeTimeout bounds the one-time probe. A login shell whose
// profile hangs must not stall the first request; on timeout no hint is
// given, which is the behaviour from before hints existed.
const interpreterProbeTimeout = 5 * time.Second

// interpreterHintLines returns one Environment line per alias whose bare
// command is missing while its versioned command is available, e.g.
// "- Use `python3` (`python` is not installed)". available reports whether a
// command can be run by name. Nothing is said when both or neither exist:
// the hint only corrects a wrong guess the model is known to make.
func interpreterHintLines(available func(name string) bool) []string {
	var lines []string
	for _, a := range interpreterAliases {
		if !available(a.bare) && available(a.versioned) {
			lines = append(lines, "- Use `"+a.versioned+"` (`"+a.bare+"` is not installed)")
		}
	}
	return lines
}

// cachedInterpreterHints is interpreterHintLines over the real host, probed
// once per process (see the comment at the top of this file for why it is not
// refreshed).
var cachedInterpreterHints = sync.OnceValue(func() []string {
	names := make([]string, 0, 2*len(interpreterAliases))
	for _, a := range interpreterAliases {
		names = append(names, a.bare, a.versioned)
	}
	found := probeCommands(names)
	return interpreterHintLines(func(name string) bool { return found[name] })
})

// probeCommands reports which of names can be run from the bash tool.
//
// On a POSIX host the bash tool runs `bash -lc`, whose login profile may add
// PATH entries (pyenv, conda, Homebrew) that this process does not see, so
// the probe asks that same shell with `command -v` instead of searching this
// process's PATH. Any failure (no shell, timeout) returns what was found so
// far, possibly nothing, and so at worst omits a hint.
//
// PowerShell and cmd.exe inherit this process's PATH (PowerShell is started
// with -NoProfile), so there exec.LookPath gives the same answer without
// spawning anything.
func probeCommands(names []string) map[string]bool {
	found := make(map[string]bool, len(names))
	if shell.Dialect() != shell.KindPOSIX {
		for _, name := range names {
			if _, err := exec.LookPath(name); err == nil {
				found[name] = true
			}
		}
		return found
	}
	// The names are fixed identifiers from interpreterAliases (and tests), so
	// quoting them in single quotes is enough.
	var script strings.Builder
	script.WriteString("for c in")
	for _, name := range names {
		script.WriteString(" '" + name + "'")
	}
	script.WriteString(`; do command -v "$c" >/dev/null 2>&1 && echo "$c"; done`)

	ctx, cancel := context.WithTimeout(context.Background(), interpreterProbeTimeout)
	defer cancel()
	bin, args := shell.CommandLine(script.String())
	cmd := exec.CommandContext(ctx, bin, args...)
	// A profile may leave a background child holding stdout open; without a
	// WaitDelay, Output would wait for it even after the timeout kills bash.
	cmd.WaitDelay = time.Second
	out, _ := cmd.Output()
	// A login profile may print its own lines; only exact requested names
	// count.
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); wanted[line] {
			found[line] = true
		}
	}
	return found
}
