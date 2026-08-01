// Package shell resolves the interpreter that agent-, hook- and
// custom-command-authored command lines are handed to.
//
// Unix hosts run "bash -lc" as they always have. Windows has no bash in a
// default install, so command lines run under PowerShell — pwsh when it is on
// PATH, otherwise the powershell.exe that ships with every supported Windows
// release. Operators who do keep a POSIX shell around (Git for Windows, MSYS2,
// WSL interop) can point SPETTRO_SHELL at it and get the Unix behaviour back.
//
// Which dialect won is not an implementation detail the model can be left to
// guess: Kind is reported in the system prompt so the agent writes PowerShell
// on a PowerShell host instead of emitting POSIX pipelines that silently do
// the wrong thing.
package shell

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
)

// EnvOverride names the environment variable that forces a specific
// interpreter, e.g. SPETTRO_SHELL=bash or SPETTRO_SHELL=C:\Program Files\Git\bin\bash.exe.
const EnvOverride = "SPETTRO_SHELL"

// Kind identifies the dialect a command line must be written in.
type Kind string

const (
	// KindPOSIX is a Bourne-compatible shell: bash, sh, zsh, dash.
	KindPOSIX Kind = "posix"
	// KindPowerShell is Windows PowerShell or PowerShell 7+ (pwsh).
	KindPowerShell Kind = "powershell"
	// KindCmd is the legacy Windows command processor, used only as a
	// last-resort fallback when no PowerShell can be found.
	KindCmd Kind = "cmd"
)

// resolved is a fully-resolved interpreter choice.
type resolved struct {
	path string // executable to spawn, as passed to exec
	name string // base name without extension, e.g. "bash", "pwsh"
	kind Kind
}

// resolve picks the interpreter for this host. It is deliberately not cached:
// resolution costs a handful of stat calls against a process spawn that costs
// milliseconds, and recomputing keeps SPETTRO_SHELL changes observable to
// tests and to a running session.
func resolve() resolved {
	if override := strings.TrimSpace(os.Getenv(EnvOverride)); override != "" {
		return classify(override)
	}
	if runtime.GOOS != "windows" {
		return resolved{path: "bash", name: "bash", kind: KindPOSIX}
	}
	// pwsh first: PowerShell 7+ is the actively developed line and is what a
	// developer who installed a shell on purpose will have.
	for _, candidate := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return resolved{path: p, name: candidate, kind: KindPowerShell}
		}
	}
	return resolved{path: "cmd", name: "cmd", kind: KindCmd}
}

// classify derives the dialect of an explicitly configured interpreter from
// its file name, so both "pwsh" and "C:\Program Files\PowerShell\7\pwsh.exe"
// are understood.
func classify(spec string) resolved {
	base := strings.ToLower(filepath.Base(spec))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	switch base {
	case "pwsh", "powershell":
		return resolved{path: spec, name: base, kind: KindPowerShell}
	case "cmd":
		return resolved{path: spec, name: base, kind: KindCmd}
	default:
		return resolved{path: spec, name: base, kind: KindPOSIX}
	}
}

// Dialect reports the dialect command lines must be written in on this host.
func Dialect() Kind { return resolve().kind }

// Name reports the interpreter's base name, e.g. "bash" or "pwsh".
func Name() string { return resolve().name }

// Describe renders the interpreter for the system prompt and for diagnostics,
// e.g. "bash" or "PowerShell (pwsh)".
func Describe() string {
	r := resolve()
	switch r.kind {
	case KindPowerShell:
		return "PowerShell (" + r.name + ")"
	case KindCmd:
		return "cmd.exe"
	default:
		return r.name
	}
}

// CommandLine returns the executable and arguments that run cmdline under this
// host's interpreter. The result is meant to be handed to exec.Command or to
// sandbox.Command, which applies OS confinement around it.
func CommandLine(cmdline string) (name string, args []string) {
	r := resolve()
	switch r.kind {
	case KindPowerShell:
		return r.path, powerShellArgs(cmdline)
	case KindCmd:
		return r.path, []string{"/d", "/s", "/c", cmdline}
	default:
		// -l so the user's profile (PATH additions, toolchain managers like
		// nvm and pyenv) is in effect, matching an interactive terminal.
		return r.path, []string{"-lc", cmdline}
	}
}

// InteractiveCommandLine returns the executable and arguments for a command
// run under a PTY. The child owns a real console there, so none of the
// redirected-pipe corrections CommandLine applies are needed — and applying
// them would actively hurt, since routing output through a pipeline breaks the
// prompts and progress redraws a PTY session exists to drive. The command is
// still passed as an encoded blob so quoting stays exact.
func InteractiveCommandLine(cmdline string) (name string, args []string) {
	r := resolve()
	switch r.kind {
	case KindPowerShell:
		return r.path, []string{"-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encodeUTF16LE(cmdline)}
	case KindCmd:
		return r.path, []string{"/d", "/s", "/c", cmdline}
	default:
		return r.path, []string{"-lc", cmdline}
	}
}

// powerShellArgs builds the non-interactive argument vector. -EncodedCommand
// carries the script as base64 UTF-16LE, which removes the entire quoting
// problem: no metacharacter, embedded quote or newline in an LLM-authored
// command line can alter the argument vector, and powershell.exe's famously
// idiosyncratic -Command re-parsing never runs.
func powerShellArgs(cmdline string) []string {
	return []string{
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodeUTF16LE(powerShellScript(cmdline)),
	}
}

// powerShellScript wraps a command line so its output and status reach the
// caller the way a POSIX shell would report them. Four PowerShell behaviours
// have to be corrected, each verified against powershell.exe 5.1 in
// shell_test.go:
//
//   - Exit status. powershell.exe reports its own 0/1 and discards the exit
//     code of any native program it ran, so a build failing with 2 would be
//     indistinguishable from any other failure. The epilogue republishes
//     $LASTEXITCODE.
//   - Error visibility. $? stays true after `& { ... }` even when the block
//     raised a CommandNotFoundException, so a misspelled command would report
//     success. Errors are counted as they stream past instead.
//   - Native stderr is not failure. With `2>&1` every stderr line from a
//     native program arrives as an ErrorRecord, and git, go and npm all write
//     progress there on success; counting those would fail healthy commands.
//     NativeCommand* records are passed through without counting, since the
//     program's own exit code already carries its verdict.
//   - Stream encoding. When stderr is a pipe, powershell.exe serialises its
//     error and progress streams as CLIXML — the caller gets an XML document
//     instead of a message. Writing through [Console]::Error bypasses that
//     serialiser, and silencing the progress stream stops the CLIXML preamble
//     from being emitted at all. Piped stdout defaults to the legacy OEM code
//     page, so the prelude forces UTF-8.
func powerShellScript(cmdline string) string {
	var sb strings.Builder
	sb.WriteString("$ErrorActionPreference = 'Continue'\n")
	sb.WriteString("$ProgressPreference = 'SilentlyContinue'\n")
	// Assignment fails when the process has no attached console; output is
	// still UTF-8 in that case, so a failure here is not worth reporting.
	sb.WriteString("try { $OutputEncoding = [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false } catch {}\n")
	// Zero it first so a stale code from an earlier native call cannot be
	// mistaken for this command's status.
	sb.WriteString("$global:LASTEXITCODE = 0\n")
	sb.WriteString("$script:__spettroErrors = 0\n")
	// The command is carried as base64 and compiled at runtime rather than
	// pasted in as a literal script block. Two reasons: no byte of an
	// LLM-authored command line can terminate the surrounding script, and a
	// syntax error becomes a catchable runtime exception instead of a parse
	// failure of this whole script — which would abort before the error
	// handling below exists and emit a raw CLIXML document to stderr. Models
	// do write POSIX pipelines on a PowerShell host, and the resulting message
	// is what lets them notice and correct course.
	sb.WriteString("$__spettroCmd = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('")
	sb.WriteString(base64.StdEncoding.EncodeToString([]byte(cmdline)))
	sb.WriteString("'))\n")
	sb.WriteString("try { $__spettroBlock = [ScriptBlock]::Create($__spettroCmd) }\n")
	sb.WriteString("catch { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }\n")
	sb.WriteString("& $__spettroBlock 2>&1 | ForEach-Object {\n")
	sb.WriteString("  if ($_ -is [System.Management.Automation.ErrorRecord]) {\n")
	sb.WriteString("    if ($_.FullyQualifiedErrorId -notlike 'NativeCommand*') { $script:__spettroErrors++ }\n")
	sb.WriteString("    [Console]::Error.WriteLine($_.ToString())\n")
	sb.WriteString("  } else { $_ }\n")
	// Out-Default must be explicit: the interception pipeline is the last
	// statement's output, and without it formatted object output is dropped.
	sb.WriteString("} | Out-Default\n")
	sb.WriteString("$__spettroCode = $LASTEXITCODE\n")
	sb.WriteString("if ($__spettroCode) { exit $__spettroCode }\n")
	sb.WriteString("if ($script:__spettroErrors) { exit 1 }\n")
	sb.WriteString("exit 0\n")
	return sb.String()
}

// encodeUTF16LE renders a script as the base64 UTF-16LE blob -EncodedCommand
// expects.
func encodeUTF16LE(s string) string {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(units)*2)
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}
