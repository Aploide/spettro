package agent

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"spettro/internal/shell"
)

// Checkpoint policy: when the runtime asks the host to snapshot the working
// tree for /rewind.
//
// A snapshot is a handful of git processes over the whole tree, so taking one
// before every mutating call made shell-heavy runs pay for it on every `ls`
// and `git status`. Two rules keep the cost down without weakening rewind:
//
//   - At most one snapshot per step. parallelExec runs a step's mutating calls
//     serially in the model's order, so a snapshot taken before the step's
//     first mutating call captures the tree as it was before any of them.
//     Rewind granularity becomes the step (one model reply), which is also
//     the unit the user sees.
//   - Shell commands that provably cannot write to the working tree take no
//     snapshot. The classifier is deliberately narrow: anything it does not
//     recognise — interpreters, build and test runners, redirections, command
//     substitution, background jobs — still counts as mutating, because a
//     spurious snapshot costs milliseconds while a missed one is
//     unrecoverable.
//
// A third keeps the remaining cost off the mutating call's path: the host
// prepares the step's snapshot while the model generates and the first
// mutating call claims it (checkpoint_speculative.go). A preparation races
// outside edits made during generation; the claim re-stages tracked files
// when a file the agent knows changed, falls back to a synchronous snapshot
// whenever the preparation cannot be trusted, and the residual risk (an
// outside edit to a file the agent never touched) is documented there and
// in docs/checkpointing.md.

// needsCheckpoint reports whether a tool call can modify the working tree
// and so must be preceded by a snapshot.
func needsCheckpoint(call toolCall) bool {
	if !isMutatingTool(call.Tool) {
		return false
	}
	switch call.Tool {
	case "bash":
		// bash with a job_id polls a background job's output (see execute).
		var probe struct {
			JobID string `json:"job_id"`
		}
		if json.Unmarshal(call.Args, &probe) == nil && strings.TrimSpace(probe.JobID) != "" {
			return false
		}
		return !isReadOnlyShellCall(call.Args)
	case "shell-exec":
		return !isReadOnlyShellCall(call.Args)
	}
	return true
}

// resetStepCheckpoint marks the start of a new step: the next mutating call
// takes a fresh snapshot.
func (r *toolRuntime) resetStepCheckpoint() {
	r.stepCheckpointMu.Lock()
	r.stepCheckpointed = false
	r.stepCheckpointMu.Unlock()
}

// checkpointStep snapshots the working tree unless this step already has a
// snapshot. The lock is held across the host callback, so a concurrent
// caller in the same step (parallel sub-agent merges) waits for the snapshot
// instead of racing ahead of it.
func (r *toolRuntime) checkpointStep(tool string) {
	if r.checkpoint == nil {
		return
	}
	r.stepCheckpointMu.Lock()
	defer r.stepCheckpointMu.Unlock()
	if r.stepCheckpointed {
		return
	}
	if !r.claimSpeculativeLocked(tool) {
		r.checkpoint(tool)
	}
	r.stepCheckpointed = true
}

// subagentCheckpoint is the Checkpoint hook for a sub-agent working in
// subCWD. The host snapshots this runtime's checkpoint scope, the main
// checkout, so a sub-agent in its own worktree gets none: its edits land
// outside that tree, and snapshotting an untouched checkout before each of
// them is pure overhead. The merge that brings its work back is covered by
// the checkpointStep taken before it.
func (r *toolRuntime) subagentCheckpoint(subCWD string) func(string) {
	if subCWD != r.cwd {
		return nil
	}
	return r.checkpoint
}

// isReadOnlyShellCall reports whether a shell-exec/bash call runs, in the
// foreground, a command line that cannot modify the working tree.
func isReadOnlyShellCall(raw json.RawMessage) bool {
	var args struct {
		Command         string   `json:"command"`
		Cmd             string   `json:"cmd"`
		RunInBackground flexBool `json:"run_in_background"`
	}
	if json.Unmarshal(raw, &args) != nil || bool(args.RunInBackground) {
		return false
	}
	if shell.Dialect() != shell.KindPOSIX {
		// The classifier parses POSIX shell syntax only.
		return false
	}
	return isReadOnlyShellCommand(firstNonEmpty(args.Command, args.Cmd))
}

// harmlessRedirect matches redirections that write nowhere in the tree:
// merging stderr into stdout, or discarding a stream into /dev/null.
var harmlessRedirect = regexp.MustCompile(`(^|\s)(\d*|&)>>?\s*(&\d+|/dev/null)(\s|$)`)

// isReadOnlyShellCommand reports whether every command in a POSIX command
// line (segments split on ;, &&, ||, | and newlines) is a known read-only
// command whose arguments cannot make it write.
func isReadOnlyShellCommand(command string) bool {
	segments := splitShellCommandSegments(command)
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		// Apply twice: adjacent matches share the whitespace between them.
		seg = harmlessRedirect.ReplaceAllString(seg, " ")
		seg = harmlessRedirect.ReplaceAllString(seg, " ")
		if segmentHasUnsafeShellFeatures(seg) {
			return false
		}
		tokens := slices.DeleteFunc(lexShellTokens(seg), func(t string) bool { return t == "" })
		if len(tokens) == 0 {
			return false
		}
		// A leading VAR=value or `env` can change what the command does
		// (GIT_EXTERNAL_DIFF, GIT_CONFIG_*, GOFLAGS=-toolexec=… run programs
		// or rewrite files), and the classifier only reads argv: only
		// locale and display settings pass.
		for len(tokens) > 0 && looksLikeEnvAssignment(tokens[0]) {
			if !harmlessEnvAssignment(tokens[0]) {
				return false
			}
			tokens = tokens[1:]
		}
		if len(tokens) == 0 || tokens[0] == "env" {
			return false
		}
		if !readOnlyCommand(tokens) {
			return false
		}
	}
	return true
}

// harmlessEnvAssignment reports whether a leading VAR=value only affects
// formatting (locale, time zone, colour, width), never which programs run or
// which files are written.
func harmlessEnvAssignment(t string) bool {
	name, _, _ := strings.Cut(t, "=")
	switch name {
	case "LANG", "LANGUAGE", "TZ", "NO_COLOR", "COLUMNS":
		return true
	}
	return strings.HasPrefix(name, "LC_")
}

// readOnlyCommands never write files, whatever their arguments (output only
// goes to stdout, and redirections were rejected by the caller).
var readOnlyCommands = map[string]bool{
	"ls": true, "pwd": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "egrep": true, "fgrep": true, "stat": true, "which": true, "echo": true, "printf": true, "true": true, "basename": true,
	"dirname": true, "realpath": true, "readlink": true, "du": true, "df": true,
	"nl": true, "cmp": true, "diff": true, "cd": true, "whoami": true,
	"uname": true, "jq": true,
}

// readOnlyGitCommands are git subcommands that leave the working tree alone.
var readOnlyGitCommands = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "blame": true,
	"rev-parse": true, "ls-files": true, "ls-tree": true, "cat-file": true,
	"shortlog": true, "describe": true, "grep": true, "rev-list": true,
	"merge-base": true, "whatchanged": true,
}

// sedPrintScript is a sed -n script that only prints line ranges ("5,20p",
// "1p;$p"): the form the prompt recommends for reading part of a file.
var sedPrintScript = regexp.MustCompile(`^((\d+|\$)(,(\d+|\$))?p;?)+$`)

// readOnlyCommand classifies one command's tokens (the caller has rejected
// leading env assignments).
func readOnlyCommand(tokens []string) bool {
	name := path.Base(tokens[0])
	args := tokens[1:]
	if readOnlyCommands[name] {
		return true
	}
	switch name {
	case "rg":
		// --pre runs an arbitrary preprocessor command per file.
		return !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--pre") })
	case "tree", "sort":
		// -o (sort also --output) writes the result to a file.
		return !hasShortFlag(args, 'o') && !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--output") })
	case "find":
		for _, a := range args {
			switch a {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls":
				return false
			}
		}
		return true
	case "sed":
		return readOnlySed(args)
	case "git":
		return readOnlyGit(args)
	case "go":
		return readOnlyGo(args)
	}
	return false
}

// hasShortFlag reports whether any short-option cluster ("-o", "-ao",
// "-ofile") in args contains flag.
func hasShortFlag(args []string, flag byte) bool {
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.IndexByte(a[1:], flag) >= 0 {
			return true
		}
	}
	return false
}

// readOnlySed accepts only `sed -n '<ranges>p' [files]`.
func readOnlySed(args []string) bool {
	quiet, script := false, ""
	for _, a := range args {
		switch {
		case a == "-n" || a == "--quiet" || a == "--silent":
			quiet = true
		case strings.HasPrefix(a, "-"):
			// -i, -e, -f, --in-place, … : anything else is out.
			return false
		case script == "":
			script = a
		}
	}
	return quiet && sedPrintScript.MatchString(script)
}

// readOnlyGit accepts read-only git subcommands, after the -C/--no-pager
// global options. `-c` is refused: it can point diff.external or a pager at
// an arbitrary program.
func readOnlyGit(args []string) bool {
	if gitEnvRunsPrograms() {
		return false
	}
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		switch args[i] {
		case "-C":
			i += 2
		case "--no-pager", "-P":
			i++
		default:
			return false
		}
	}
	if i >= len(args) {
		return false
	}
	sub, rest := args[i], args[i+1:]
	for _, a := range rest {
		// --output writes the diff to a file; -O/--open-files-in-pager
		// (git grep) runs a program.
		if strings.HasPrefix(a, "--output") || a == "-O" || strings.HasPrefix(a, "--open-files-in-pager") || strings.HasPrefix(a, "--ext-diff") {
			return false
		}
	}
	if readOnlyGitCommands[sub] {
		return true
	}
	switch sub {
	case "branch":
		// Listing only: every argument a listing flag.
		for _, a := range rest {
			switch a {
			case "-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose", "--list", "-l", "--show-current", "--no-color":
			default:
				return false
			}
		}
		return true
	case "remote":
		return len(rest) == 0 || len(rest) == 1 && (rest[0] == "-v" || rest[0] == "--verbose")
	case "stash", "worktree":
		return len(rest) >= 1 && rest[0] == "list"
	}
	return false
}

// gitEnvRunsPrograms reports whether the inherited environment configures
// git to run an external program on read-only commands (the environment
// forms of the `-c` readOnlyGit refuses).
func gitEnvRunsPrograms() bool {
	for _, name := range []string{"GIT_EXTERNAL_DIFF", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// unsafeGoFlag reports whether a go flag can run a program (-toolexec,
// -vettool) or write the module files: -mod=mod rewrites go.mod and go.sum,
// -modfile points them elsewhere. Every -mod* spelling is refused, including
// the harmless -mod=readonly, to keep the check simple.
func unsafeGoFlag(a string) bool {
	if !strings.HasPrefix(a, "-") {
		return false
	}
	a = strings.TrimLeft(a, "-")
	return strings.HasPrefix(a, "toolexec") || strings.HasPrefix(a, "vettool") || strings.HasPrefix(a, "mod")
}

// goEnvFlags returns GOFLAGS as the go command would see it from the
// environment or, when unset there, from the `go env -w` config file.
func goEnvFlags() string {
	if v, ok := os.LookupEnv("GOFLAGS"); ok {
		return v
	}
	file := os.Getenv("GOENV")
	if file == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		file = filepath.Join(dir, "go", "env")
	}
	if file == "off" {
		return ""
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "GOFLAGS="); ok {
			return v
		}
	}
	return ""
}

// readOnlyGo accepts go subcommands that only read the module. go build and
// go test are out: build drops a binary in the package directory and tests
// may write fixtures and golden files.
func readOnlyGo(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if slices.ContainsFunc(args, unsafeGoFlag) {
		return false
	}
	// GOFLAGS applies its flags as if typed on the command line
	// (GOFLAGS=-mod=mod makes go list and go vet rewrite go.mod).
	if slices.ContainsFunc(strings.Fields(goEnvFlags()), unsafeGoFlag) {
		return false
	}
	switch args[0] {
	case "version", "list", "doc", "vet":
		return true
	case "env":
		return !slices.ContainsFunc(args[1:], func(a string) bool { return a == "-w" || a == "-u" })
	}
	return false
}
