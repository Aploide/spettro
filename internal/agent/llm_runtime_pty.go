package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"spettro/internal/pty"
	"spettro/internal/sandbox"
)

// ptyDefaultWait is how long pty-start and pty-write pause for the process to
// produce output before returning what accumulated.
const ptyDefaultWait = 700 * time.Millisecond

// runPtyStart allocates an interactive pseudo-terminal session. The command
// goes through the same approval path as shell-exec; that single approval
// covers subsequent pty-write input into the session (stated in the approval
// reason), so under ask-first the user decides once, at start.
func (r *toolRuntime) runPtyStart(ctx context.Context, toolID string, rawArgs []byte) (string, error) {
	var args struct {
		Command string `json:"command"`
		Cols    int    `json:"cols"`
		Rows    int    `json:"rows"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("pty-start args: %w", err)
	}
	cmdText := strings.TrimSpace(args.Command)
	if cmdText == "" {
		return "", fmt.Errorf("pty-start: command is required")
	}
	if !pty.Supported() {
		return "", fmt.Errorf("pty-start: pty sessions are unsupported on this platform")
	}
	if err := r.authorizeShellCommand(ctx, toolID, cmdText); err != nil {
		return "", err
	}
	cmdText = EnforceCommitCoAuthor(cmdText)
	// Sessions must outlive this tool call: build the command on a background
	// context so the per-tool timeout doesn't kill it. The same sandbox policy
	// still wraps the process — the PTY is not a sandbox escape.
	cmd := sandbox.Command(context.Background(), r.sandboxPolicy(), r.cwd, "bash", "-lc", cmdText)
	cmd.Dir = r.cwd
	sess, err := pty.Default().Start(cmd, cmdText, clampWinDim(args.Cols), clampWinDim(args.Rows))
	if err != nil {
		return "", fmt.Errorf("pty-start: %w", err)
	}
	time.Sleep(ptyDefaultWait)
	out, running, exitInfo := sess.ReadNew()
	header := fmt.Sprintf("started pty session %s (send input with pty-write, terminate with pty-kill)", sess.ID)
	if !running {
		header = fmt.Sprintf("pty session %s exited immediately (%s)", sess.ID, exitInfo)
	}
	return header + "\n" + r.spoolResult(toolID, normalizePtyOutput(out)), nil
}

// ptyWaitForDefault caps how long pty-write blocks for a wait_for pattern
// when the model gave no explicit wait_ms.
const ptyWaitForDefault = 10 * time.Second

// runPtyWrite sends input to a live session and returns the output produced
// since the last read. Input is decoded for backslash escapes (models emit
// them as literal text more often than as real JSON control bytes), so
// "\\r" submits a line and "\\x03" is Ctrl-C regardless of how the
// escaping survived. submit appends \r; wait_for polls until a pattern
// appears instead of guessing wait_ms. Empty input just polls.
func (r *toolRuntime) runPtyWrite(rawArgs []byte) (string, error) {
	var args struct {
		ID      string `json:"id"`
		Input   string `json:"input"`
		Submit  bool   `json:"submit"`
		WaitMs  int    `json:"wait_ms"`
		WaitFor string `json:"wait_for"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("pty-write args: %w", err)
	}
	if strings.TrimSpace(args.ID) == "" {
		return "", fmt.Errorf("pty-write: id is required")
	}
	sess, ok := pty.Default().Get(args.ID)
	if !ok {
		return "", fmt.Errorf("pty-write: unknown pty session %q", args.ID)
	}
	input := decodePtyInput(args.Input)
	if args.Submit && !strings.HasSuffix(input, "\r") && !strings.HasSuffix(input, "\n") {
		input += "\r"
	}
	if input != "" {
		if err := sess.Write(input); err != nil {
			return "", fmt.Errorf("pty-write: %w", err)
		}
	}

	wait := ptyDefaultWait
	if args.WaitMs > 0 {
		wait = time.Duration(min(args.WaitMs, 30000)) * time.Millisecond
	} else if args.WaitFor != "" {
		wait = ptyWaitForDefault
	}

	var raw strings.Builder
	running, exitInfo := true, ""
	matched := false
	deadline := time.Now().Add(wait)
	for {
		var out string
		out, running, exitInfo = sess.ReadNew()
		raw.WriteString(out)
		if args.WaitFor != "" && strings.Contains(normalizePtyOutput(raw.String()), args.WaitFor) {
			matched = true
			break
		}
		if !running || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	status := "running"
	if !running {
		status = "exited (" + exitInfo + ")"
	}
	header := fmt.Sprintf("pty=%s status=%s", sess.ID, status)
	if args.WaitFor != "" {
		if matched {
			header += fmt.Sprintf(" wait_for=%q matched", args.WaitFor)
		} else {
			header += fmt.Sprintf(" wait_for=%q NOT seen before timeout", args.WaitFor)
		}
	}
	body := normalizePtyOutput(raw.String())
	if strings.TrimSpace(body) == "" {
		return header + "\n(no new output)", nil
	}
	return header + "\n" + r.spoolResult("pty-write", body), nil
}

// runPtyKill terminates a session's process group and frees the terminal.
func (r *toolRuntime) runPtyKill(rawArgs []byte) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("pty-kill args: %w", err)
	}
	if strings.TrimSpace(args.ID) == "" {
		return "", fmt.Errorf("pty-kill: id is required")
	}
	if err := pty.Default().Kill(args.ID); err != nil {
		return "", fmt.Errorf("pty-kill: %w", err)
	}
	return fmt.Sprintf("killed pty session %s", strings.TrimSpace(args.ID)), nil
}

// decodePtyInput turns backslash escape sequences that arrived as literal
// text (`\r`, `\n`, `\t`, `\e`, `\xHH`, `\uHHHH`, `\\`) into the real bytes.
// Models frequently double-escape control characters in JSON, and a REPL fed
// the six characters `\x03` (as text) instead of Ctrl-C is unrecoverable, so decoding
// here is the reliable surface. Unknown escapes pass through unchanged.
func decodePtyInput(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case 'r':
			b.WriteByte('\r')
			i++
		case 'n':
			b.WriteByte('\n')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'e':
			b.WriteByte(0x1b)
			i++
		case '\\':
			b.WriteByte('\\')
			i++
		case 'x':
			if i+3 < len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 3
					continue
				}
			}
			b.WriteByte(s[i])
		case 'u':
			if i+5 < len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+6], 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += 5
					continue
				}
			}
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// normalizePtyOutput renders raw terminal bytes as settled plain text for the
// model (see pty.Settle); the raw bytes stay in the session scrollback for
// the TUI live-tail view.
func normalizePtyOutput(out string) string {
	return pty.Settle(out)
}

// clampWinDim bounds a model-supplied terminal dimension to sane values;
// zero means "use the default".
func clampWinDim(v int) uint16 {
	if v <= 0 {
		return 0
	}
	if v > 500 {
		return 500
	}
	return uint16(v)
}
