//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no Landlock or Seatbelt: nothing lets a process declare "confine
// this child to these paths" without touching the objects themselves. What it
// does have is Mandatory Integrity Control. Every securable object carries an
// integrity label, and a process may not write to an object labelled above it.
// Running the child on a Low-integrity token therefore denies writes to the
// entire filesystem — every ordinary file and directory sits at Medium — and
// the roots the policy does allow are opened back up by labelling them Low.
//
// This is a narrower guarantee than the Unix backends provide, in two ways
// that capabilities() reports rather than papers over:
//
//   - Reads are not confined. MIC is no-write-up only; a Low process still
//     reads whatever its user can read. FSReadOnly here means "cannot modify
//     project or user files", not "cannot see them".
//   - Network is not confined at all. Sockets are outside MIC's scope, so a
//     requested network policy fails closed rather than running unconfined.
//
// Confining both would require launching into an AppContainer, which needs a
// process attribute list that os/exec cannot pass.

// sandboxFailureExit matches the Linux backend's "could not confine" status.
const sandboxFailureExit = 126

// failureEnvVar carries the diagnostic into the reporting command's
// environment. Interpolating it into the command line instead would let a
// path or policy string containing a cmd metacharacter be re-parsed as
// syntax, and cmd expands %VAR% before it parses.
const failureEnvVar = "SPETTRO_SANDBOX_ERROR"

func available() bool { return true }

func capabilities() Capabilities {
	return Capabilities{
		Mechanism: "integrity-level",
		FS:        true,
		Net:       false,
		Detail:    "Low-integrity token: writes confined, reads unconfined; network confinement needs AppContainer and is unavailable",
	}
}

// runChildIfRequested is a no-op: only the Linux backend re-execs a child.
// The restricted token here is applied by the parent as it builds the command.
func runChildIfRequested() {}

// wrap builds the command to run under confinement. Any failure to set the
// policy up produces a command that reports the reason and exits 126, so the
// caller can never end up running the model's command unconfined.
func wrap(ctx context.Context, p Policy, workspaceDir, name string, args ...string) *exec.Cmd {
	if p.netRestricted() {
		return failCommand(ctx, fmt.Sprintf(
			"network confinement (%s) is not supported on Windows; it requires AppContainer. Remove --sandbox-net or set it to \"all\".", p.Net))
	}

	scratch, err := lowIntegrityScratchDir()
	if err != nil {
		return failCommand(ctx, "prepare sandbox temp directory: "+err.Error())
	}
	// Every root the policy calls writable has to be reachable by a Low
	// process, which means labelling it. The scratch dir stands in for the
	// system temp dirs, which must not be relabelled: they are shared by every
	// application on the machine.
	writable := []string{scratch}
	if p.FS == FSWorkspaceWrite && workspaceDir != "" {
		writable = append(writable, workspaceDir)
	}
	writable = append(writable, p.ExtraWritable...)
	for _, dir := range writable {
		if err := allowLowIntegrityWrites(dir); err != nil {
			return failCommand(ctx, err.Error())
		}
	}

	token, err := lowIntegrityToken()
	if err != nil {
		return failCommand(ctx, "create low-integrity token: "+err.Error())
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(token)}
	// A Low process cannot write to the ordinary per-user temp directory, so
	// point the child at the labelled scratch dir instead. Without this,
	// compilers and anything using a temp file fail in the sandbox for reasons
	// that look nothing like a policy decision.
	cmd.Env = append(environWithout(os.Environ(), "TEMP", "TMP"),
		"TEMP="+scratch,
		"TMP="+scratch,
	)
	return cmd
}

// confineParent reports that the spettro process cannot confine itself here.
// The Unix backends apply an in-process restriction; a Windows process cannot
// lower its own integrity level and keep working — it would immediately lose
// the ability to write its own config, session store and conversation history.
// Callers treat the error as a warning, and the model's own surface is still
// confined at the shell and file-tool layers.
func confineParent(writableRoots []string) error {
	return fmt.Errorf("parent write-confinement is not available on Windows (a process cannot lower its own integrity level and keep writing its config)")
}

// failCommand returns a command that reports message on stderr and exits 126.
//
// wrap has no way to return an error, and refusing to confine has to be
// louder than running unconfined would be. cmd.exe does the reporting rather
// than a re-exec of this binary: os.Executable() under `go test` is the test
// binary, which has no main() to intercept a sentinel argument and would
// simply run the whole suite again.
func failCommand(ctx context.Context, message string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe", "/d", "/s", "/c",
		"echo spettro sandbox: %"+failureEnvVar+"% 1>&2 & exit "+strconv.Itoa(sandboxFailureExit))
	cmd.Env = append(environWithout(os.Environ(), failureEnvVar),
		failureEnvVar+"="+sanitizeForCmdEcho(message))
	return cmd
}

// sanitizeForCmdEcho strips the characters cmd would treat as syntax after it
// expands the variable, so a diagnostic can never become a command.
func sanitizeForCmdEcho(message string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '&', '|', '<', '>', '^', '%', '"', '\r', '\n':
			return ' '
		}
		if r < 0x20 {
			return ' '
		}
		return r
	}, message)
}

// lowIntegrityToken duplicates the current process token and lowers its
// integrity to Low. Lowering a token's own integrity needs no privilege, and a
// token derived this way can be passed to CreateProcessAsUser — which is what
// exec.Cmd does when SysProcAttr.Token is set.
func lowIntegrityToken() (windows.Token, error) {
	var current windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_ADJUST_DEFAULT,
		&current,
	); err != nil {
		return 0, fmt.Errorf("open process token: %w", err)
	}
	defer current.Close()

	var duplicated windows.Token
	if err := windows.DuplicateTokenEx(
		current,
		windows.TOKEN_ALL_ACCESS,
		nil,
		windows.SecurityImpersonation,
		windows.TokenPrimary,
		&duplicated,
	); err != nil {
		return 0, fmt.Errorf("duplicate token: %w", err)
	}

	low, err := windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		duplicated.Close()
		return 0, fmt.Errorf("resolve low integrity SID: %w", err)
	}
	label := tokenMandatoryLabel{Label: windows.SIDAndAttributes{
		Sid:        low,
		Attributes: windows.SE_GROUP_INTEGRITY,
	}}
	if err := windows.SetTokenInformation(
		duplicated,
		windows.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(&label)),
		uint32(unsafe.Sizeof(label))+windows.GetLengthSid(low),
	); err != nil {
		duplicated.Close()
		return 0, fmt.Errorf("set integrity level: %w", err)
	}
	return duplicated, nil
}

// tokenMandatoryLabel mirrors TOKEN_MANDATORY_LABEL, which x/sys/windows does
// not declare.
type tokenMandatoryLabel struct {
	Label windows.SIDAndAttributes
}

// labelled remembers the directories already opened up this run. Labelling is
// a security-descriptor write; repeating it for every shell command would cost
// a process spawn each time and churn the ACL for no benefit.
var labelled sync.Map

// allowLowIntegrityWrites labels dir (and, by inheritance, everything created
// inside it) Low so the sandboxed child can write there.
//
// icacls does the work rather than a hand-built SACL: the mandatory label ACE
// has no helper in x/sys/windows, and getting its layout subtly wrong would
// silently produce a directory the sandbox cannot write instead of a visible
// error. Note this *lowers* the label, which grants nothing to other users —
// it only stops MIC from blocking the Low child.
func allowLowIntegrityWrites(dir string) error {
	dir = filepath.Clean(dir)
	if dir == "" {
		return nil
	}
	if _, done := labelled.Load(dir); done {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("sandbox writable root %s: %w", dir, err)
	}
	out, err := exec.Command("icacls", dir, "/setintegritylevel", "(OI)(CI)L").CombinedOutput()
	if err != nil {
		return fmt.Errorf("label %s writable by the sandbox: %v: %s", dir, err, strings.TrimSpace(string(out)))
	}
	labelled.Store(dir, struct{}{})
	return nil
}

// lowIntegrityScratchDir returns a spettro-owned temp directory the sandboxed
// child may write to, creating and labelling it on first use.
func lowIntegrityScratchDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "spettro", "sandbox-temp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := allowLowIntegrityWrites(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// environWithout returns env with the named variables removed, so callers can
// append authoritative replacements.
func environWithout(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			out = append(out, entry)
			continue
		}
		drop := false
		for _, name := range names {
			if strings.EqualFold(key, name) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return out
}
