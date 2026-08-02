// Package shelltest builds command lines that mean the same thing under every
// interpreter the shell package can select.
//
// Tests that hardcode "sh -c" or POSIX syntax do not merely fail on Windows —
// they stop testing the thing under test, because the production code paths
// run whatever shell.CommandLine returns. Building test commands through the
// same resolver keeps job control, hooks and spooling covered on every host.
package shelltest

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"spettro/internal/shell"
)

// Command builds an *exec.Cmd running cmdline under the host interpreter,
// exactly as the shell tools do.
func Command(cmdline string) *exec.Cmd {
	name, args := shell.CommandLine(cmdline)
	return exec.Command(name, args...)
}

// Echo is a command line that writes s and a newline to stdout.
func Echo(s string) string {
	if shell.Dialect() == shell.KindPowerShell {
		return "Write-Output " + psQuote(s)
	}
	return "printf '%s\\n' " + shQuote(s)
}

// EchoVar is a command line that writes prefix immediately followed by the
// value of varExpr — an expression in the host dialect, such as a loop
// variable — and a newline.
func EchoVar(prefix, varExpr string) string {
	if shell.Dialect() == shell.KindPowerShell {
		return "Write-Output (" + psQuote(prefix) + " + " + varExpr + ")"
	}
	return `printf '%s%s\n' ` + shQuote(prefix) + ` "` + varExpr + `"`
}

// EchoStderr is a command line that writes s and a newline to stderr.
//
// The PowerShell form writes through [Console]::Error rather than Write-Error
// so the bytes land on the process's stderr as plain text, the way a native
// program's diagnostics do — Write-Error would produce an ErrorRecord and mark
// the command as failed.
func EchoStderr(s string) string {
	if shell.Dialect() == shell.KindPowerShell {
		return "[Console]::Error.WriteLine(" + psQuote(s) + ")"
	}
	return "printf '%s\\n' " + shQuote(s) + " >&2"
}

// Sleep is a command line that blocks for roughly d.
func Sleep(d time.Duration) string {
	secs := d.Seconds()
	if shell.Dialect() == shell.KindPowerShell {
		return fmt.Sprintf("Start-Sleep -Milliseconds %d", int64(d/time.Millisecond))
	}
	return fmt.Sprintf("sleep %g", secs)
}

// CatStdin is a command line that copies stdin to stdout, used to check that
// hooks receive their JSON payload.
func CatStdin() string {
	if shell.Dialect() == shell.KindPowerShell {
		return "Write-Output ([Console]::In.ReadToEnd())"
	}
	return "cat"
}

// DiscardStdin is a command line that drains stdin and prints nothing.
func DiscardStdin() string {
	if shell.Dialect() == shell.KindPowerShell {
		return "[void][Console]::In.ReadToEnd()"
	}
	return "cat > /dev/null"
}

// EchoEnvJoined is a command line that prints the named environment
// variables' values, separated by sep.
func EchoEnvJoined(sep string, names ...string) string {
	if shell.Dialect() == shell.KindPowerShell {
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, "$env:"+n)
		}
		return "Write-Output (" + strings.Join(parts, " + "+psQuote(sep)+" + ") + ")"
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, "$"+n)
	}
	return `printf '%s\n' "` + strings.Join(parts, sep) + `"`
}

// Exit is a command line that terminates with the given status.
func Exit(code int) string {
	return fmt.Sprintf("exit %d", code)
}

// Exec is a command line that runs the program at path with args, quoted for
// the host dialect. PowerShell needs the call operator: a quoted path on its
// own is an expression, and the shell would echo it instead of running it.
func Exec(path string, args ...string) string {
	quote, parts := shQuote, []string{}
	if shell.Dialect() == shell.KindPowerShell {
		quote, parts = psQuote, []string{"&"}
	}
	parts = append(parts, quote(path))
	for _, a := range args {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " ")
}

// Repeat is a command line that runs body for each integer in [1,n], pausing
// by gap between iterations. Body is produced by fn, which receives the
// interpreter's expression for the loop variable.
func Repeat(n int, gap time.Duration, fn func(loopVar string) string) string {
	if shell.Dialect() == shell.KindPowerShell {
		return fmt.Sprintf("foreach ($i in 1..%d) { %s; %s }", n, fn("$i"), Sleep(gap))
	}
	return fmt.Sprintf("for i in $(seq 1 %d); do %s; %s; done", n, fn("$i"), Sleep(gap))
}

// ManyLines is a command line that writes n lines, each being prefix followed
// by the line's 1-based index. Used to produce output large enough to exercise
// spooling and paging.
func ManyLines(prefix string, n int) string {
	if shell.Dialect() == shell.KindPowerShell {
		return fmt.Sprintf("1..%d | ForEach-Object { %s + $_ }", n, psQuote(prefix))
	}
	return fmt.Sprintf(`seq 1 %d | awk '{print %s $1}'`, n, `"`+prefix+`"`)
}

// Join sequences command lines so each runs after the previous one. Windows
// PowerShell 5.1 has no && operator, and ; is the portable separator in both
// dialects.
func Join(cmdlines ...string) string { return strings.Join(cmdlines, "\n") }

// psQuote renders s as a PowerShell single-quoted literal, where a doubled
// single quote is the only escape.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// shQuote renders s as a POSIX single-quoted literal.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
