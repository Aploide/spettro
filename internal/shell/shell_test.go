package shell_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"spettro/internal/shell"
)

// run executes cmdline through the host interpreter exactly the way the shell
// tools do, and reports its combined output and exit status.
func run(t *testing.T, cmdline string) (string, int) {
	t.Helper()
	name, args := shell.CommandLine(cmdline)
	out, err := exec.Command(name, args...).CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		t.Fatalf("run %q: %v", cmdline, err)
		return "", 0
	}
}

// echoOf renders a command line that prints s verbatim in the host dialect.
func echoOf(s string) string {
	if shell.Dialect() == shell.KindPowerShell {
		// Single quotes are PowerShell's literal string; '' escapes one.
		return "Write-Output '" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "printf '%s\\n' " + "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestSuccessfulCommandExitsZero(t *testing.T) {
	out, code := run(t, echoOf("hello"))
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (output %q)", code, out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output = %q, want it to contain %q", out, "hello")
	}
}

// A native program's exit status must survive the interpreter. PowerShell
// normally discards it and reports its own 0/1, which would make every failing
// build or test run look identical to the agent.
func TestNativeExitCodePropagates(t *testing.T) {
	cmdline := "exit 7"
	if shell.Dialect() == shell.KindPowerShell {
		cmdline = "cmd /c exit 7"
	}
	if _, code := run(t, cmdline); code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}

func TestShellBuiltinExitCodePropagates(t *testing.T) {
	if _, code := run(t, "exit 3"); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestUnknownCommandIsNonZero(t *testing.T) {
	out, code := run(t, "spettro-no-such-command-zzz")
	if code == 0 {
		t.Errorf("exit code = 0 for an unknown command, want non-zero (output %q)", out)
	}
}

// The command line reaches the interpreter as a single opaque argument, so
// quotes, spaces and metacharacters an LLM writes must not reshape the
// argument vector.
func TestQuotingIsPreserved(t *testing.T) {
	for _, payload := range []string{
		`a "quoted" word`,
		`single 'quoted' word`,
		`back\slash and $dollar`,
		`semi; colon && amp | pipe`,
		`trailing backslash \`,
	} {
		out, code := run(t, echoOf(payload))
		if code != 0 {
			t.Errorf("payload %q: exit %d (output %q)", payload, code, out)
			continue
		}
		if !strings.Contains(out, payload) {
			t.Errorf("payload %q: output = %q, want it echoed verbatim", payload, out)
		}
	}
}

// Piped output must be UTF-8. Windows PowerShell writes the legacy OEM code
// page to a redirected stdout unless the encoding is forced, which corrupts
// every non-ASCII byte of tool output the agent reads back.
func TestNonASCIIOutputIsUTF8(t *testing.T) {
	const payload = "caffè — 日本語 — ✅"
	out, code := run(t, echoOf(payload))
	if code != 0 {
		t.Fatalf("exit %d (output %q)", code, out)
	}
	if !strings.Contains(out, payload) {
		t.Errorf("output = %q, want it to contain %q", out, payload)
	}
}

func TestMultilineCommandRuns(t *testing.T) {
	cmdline := echoOf("first") + "\n" + echoOf("second")
	out, code := run(t, cmdline)
	if code != 0 {
		t.Fatalf("exit %d (output %q)", code, out)
	}
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
}

// runSplit is run, but keeping stdout and stderr apart.
func runSplit(t *testing.T, cmdline string) (stdout, stderr string, code int) {
	t.Helper()
	name, args := shell.CommandLine(cmdline)
	cmd := exec.Command(name, args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("run %q: %v", cmdline, err)
	}
	return out.String(), errb.String(), code
}

// A program that merely writes to stderr has not failed. git, go and npm all
// report progress there on success, so treating stderr traffic as failure
// would mark healthy commands as broken.
func TestNativeStderrIsNotFailure(t *testing.T) {
	cmdline := `sh -c 'echo warn >&2; exit 0'`
	if shell.Dialect() == shell.KindPowerShell {
		cmdline = `cmd /c "echo warn 1>&2 & exit 0"`
	}
	stdout, stderr, code := runSplit(t, cmdline)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (stdout %q, stderr %q)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "warn") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "warn")
	}
}

// Windows PowerShell serialises its error stream as a CLIXML document when
// stderr is a pipe. The agent must read a message, not XML.
func TestErrorsAreDeliveredAsPlainText(t *testing.T) {
	_, stderr, code := runSplit(t, "spettro-no-such-command-zzz")
	if code == 0 {
		t.Errorf("exit code = 0 for an unknown command, want non-zero")
	}
	if strings.Contains(stderr, "CLIXML") || strings.Contains(stderr, "<Objs") {
		t.Errorf("stderr carries CLIXML serialisation, want plain text: %q", stderr)
	}
	if !strings.Contains(strings.ToLower(stderr), "spettro-no-such-command-zzz") {
		t.Errorf("stderr = %q, want it to name the missing command", stderr)
	}
}

// A command the interpreter cannot even parse — an LLM writing a POSIX
// pipeline on a PowerShell host — must still report a readable diagnostic.
// PowerShell fails such a script before any in-script error handling exists
// and would otherwise emit a CLIXML document.
func TestSyntaxErrorReportsPlainDiagnostic(t *testing.T) {
	if shell.Dialect() != shell.KindPowerShell {
		t.Skip("PowerShell-specific parse behaviour")
	}
	_, stderr, code := runSplit(t, "echo oops >&2; exit 3")
	if code == 0 {
		t.Errorf("exit code = 0 for an unparseable command, want non-zero")
	}
	if strings.Contains(stderr, "CLIXML") || strings.Contains(stderr, "<Objs") {
		t.Errorf("stderr carries CLIXML serialisation, want plain text: %q", stderr)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("stderr is empty, want a parse diagnostic")
	}
}

// Nothing may be written to stderr by a command that succeeded quietly; a
// stray CLIXML preamble there would be misread as a diagnostic.
func TestSuccessfulCommandWritesNothingToStderr(t *testing.T) {
	stdout, stderr, code := runSplit(t, echoOf("quiet"))
	if code != 0 {
		t.Fatalf("exit %d (stdout %q, stderr %q)", code, stdout, stderr)
	}
	if strings.TrimSpace(stderr) != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// Output that survives to stdout must include the rows a command printed,
// which regressed once when the interception pipeline swallowed formatted
// object output.
func TestFormattedOutputReachesStdout(t *testing.T) {
	cmdline := "ls -d ."
	want := "."
	if shell.Dialect() == shell.KindPowerShell {
		cmdline = `Get-Item $env:SystemRoot | Select-Object -ExpandProperty Name`
		want = "Windows"
	}
	stdout, stderr, code := runSplit(t, cmdline)
	if code != 0 {
		t.Fatalf("exit %d (stderr %q)", code, stderr)
	}
	// Case-insensitive: %SystemRoot% is spelled C:\WINDOWS on some installs.
	if !strings.Contains(strings.ToLower(stdout), strings.ToLower(want)) {
		t.Errorf("stdout = %q, want it to contain %q", stdout, want)
	}
}

func TestEnvOverrideSelectsInterpreter(t *testing.T) {
	cases := []struct {
		spec string
		want shell.Kind
		name string
	}{
		{spec: "bash", want: shell.KindPOSIX, name: "bash"},
		{spec: `C:\Program Files\Git\bin\bash.exe`, want: shell.KindPOSIX, name: "bash"},
		{spec: "pwsh", want: shell.KindPowerShell, name: "pwsh"},
		{spec: `C:\Program Files\PowerShell\7\pwsh.exe`, want: shell.KindPowerShell, name: "pwsh"},
		{spec: "powershell.exe", want: shell.KindPowerShell, name: "powershell"},
		{spec: "cmd.exe", want: shell.KindCmd, name: "cmd"},
	}
	for _, tc := range cases {
		t.Setenv(shell.EnvOverride, tc.spec)
		if got := shell.Dialect(); got != tc.want {
			t.Errorf("SPETTRO_SHELL=%q: dialect = %q, want %q", tc.spec, got, tc.want)
		}
		if got := shell.Name(); got != tc.name {
			t.Errorf("SPETTRO_SHELL=%q: name = %q, want %q", tc.spec, got, tc.name)
		}
		if gotPath, _ := shell.CommandLine("echo hi"); gotPath != tc.spec {
			t.Errorf("SPETTRO_SHELL=%q: interpreter = %q, want the override verbatim", tc.spec, gotPath)
		}
	}
}
