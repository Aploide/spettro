package agent

import "strings"

// This file is the agent's half of the platform capability gate (the other
// half is internal/platform). Everything here is a pure function of an
// explicit `canExec bool` rather than of platform.CanExec() directly, so the
// no-exec surface is exercised by the normal test suite on a desktop host
// instead of only on an iOS build. runToolLoop is the one place that reads
// the real capability and threads it in.

// execDependentTools lists the built-in tool IDs whose implementation cannot
// work without spawning a subprocess. On a platform where exec is impossible
// they are dropped from the toolset entirely — not left in place to return an
// error — because a tool that errors is a tool the model retries, while a
// tool that is absent is one it plans around.
//
// Deliberately NOT in this list:
//
//   - job-output: it doubles as the pager for spooled tool results
//     ("spool:N"), which is how the model reads back a truncated file-read or
//     grep. The spool truncation footer names it by hand (see spool.go), so
//     removing it would strand large outputs. Its background-job half is
//     simply unreachable with no shell tool to start a job, and its
//     description is rewritten below to say so.
//   - tool-output, agent, ultra: pure in-process work.
//   - web-fetch, download, web-search, grok-image, grok-video: network only.
//   - the file, search, task, memory, skill and plan-mode tools: pure Go.
var execDependentTools = []string{
	// Shell. bash-output is here too: its schema accepts a `command`, so it
	// is a second door to the same subprocess.
	"shell-exec",
	"bash",
	"bash-output",
	// Background jobs — only ever created by the shell tools above.
	"job-kill",
	// Interactive pseudo-terminals: no /dev/ptmx, no child to attach to it.
	"pty-start",
	"pty-write",
	"pty-kill",
	// Git worktrees: every operation is a `git` invocation.
	"enter-worktree",
	"exit-worktree",
	// Language servers: each one is a spawned process, so every LSP-backed
	// lookup is dead without exec.
	"diagnostics",
	"references",
	"hover",
	"rename-symbol",
	"lsp-restart",
}

// execDependentToolSet is the lookup form of execDependentTools.
var execDependentToolSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(execDependentTools))
	for _, id := range execDependentTools {
		m[id] = struct{}{}
	}
	return m
}()

// filterExecTools returns ids with the exec-dependent tools removed when
// canExec is false. With canExec true it returns ids unchanged (same backing
// array), so the desktop path allocates nothing and cannot reorder anything.
func filterExecTools(ids []string, canExec bool) []string {
	if canExec {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, blocked := execDependentToolSet[strings.TrimSpace(id)]; blocked {
			continue
		}
		out = append(out, id)
	}
	return out
}

// noExecToolDescs overrides the description of tools that survive on a
// no-exec platform but whose shipped wording advertises capabilities the
// model does not have. Sending the desktop wording would invite calls that
// can only fail.
var noExecToolDescs = map[string]string{
	"job-output": "Page through a spooled truncated tool result (spool:N) — the id printed in a [truncated: ...] footer. Pass the next_offset from the previous call to read incrementally. There are no background jobs on this platform.",
	"view-image": "Attach an image file from the workspace so you can SEE it (vision models): screenshots, diagrams, mockups, or any png/jpg/webp/gif already in the project. This platform cannot run a browser or a screenshot tool, so the file must already exist — ask the user to add one if you need a view you do not have.",
}

// noExecSystemPrompt is appended to the system prompt when the platform
// cannot exec. It is a constant on purpose: buildSystemString's output must be
// byte-identical across every step of a run or the provider's prompt cache
// misses on the whole request.
//
// It is written to pre-empt the specific failure modes of a model that has
// been trained on shells — hunting for a bash tool, promising to run the
// tests, claiming a fix is verified — rather than to merely state the fact.
const noExecSystemPrompt = `

## Platform constraints: no command execution

You are running on a device that cannot start programs. There is no shell, no
terminal, no git, no compiler, no package manager, no test runner and no
language server. Those tools are not in your tool list, and no phrasing will
make them appear — do not look for them, and do not ask the user to paste
command output as a substitute for a tool call you cannot make.

Work within that:

- Change code with file-read, file-write, file-edit and multi-edit. These are
  full-strength and operate anywhere inside the project directory.
- Navigate with glob, grep, repo-search and ls. They are pure in-process
  implementations, not wrappers around find or ripgrep, so they work normally.
- You cannot build, run, test, lint, format or execute anything, and you
  cannot observe a program's behaviour. Verify your work by re-reading what
  you wrote and by reasoning about the code — check that identifiers you
  referenced exist, that call signatures line up, that imports resolve.
- You cannot use git in any form: no status, diff, log, branch, stash, commit
  or worktree. You cannot tell what is already committed. Never claim to have
  committed anything, and do not offer to.
- Prefer edits you can be confident in from reading alone. Where a change
  genuinely needs to be exercised to be trusted, make it anyway and say so.

When you report back, state plainly that the change is unverified because this
device cannot build or test it, and name the specific command the user should
run on a machine that can.`
