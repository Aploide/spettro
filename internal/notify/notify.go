package notify

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Send fires a desktop notification with the given title and body.
// It is best-effort: errors are silently swallowed so a missing
// notification daemon never crashes the TUI.
func Send(title, body string) {
	switch runtime.GOOS {
	case "linux":
		_ = exec.Command("notify-send", "--urgency=low", "--expire-time=5000", title, body).Start()
	case "darwin":
		script := fmt.Sprintf(`display notification %q with title %q`, body, title)
		_ = exec.Command("osascript", "-e", script).Start()
	}
}

// Notifier emits user alerts over two channels at once: an OSC 9 terminal
// escape sequence (rendered as a system notification by iTerm2, WezTerm,
// Ghostty and Kitty; degraded to a BEL elsewhere) and a best-effort desktop
// notification via Send. A quiet period rate-limits bursts so rapid
// successive events don't spam the user.
type Notifier struct {
	Enabled bool
	// Quiet is the minimum interval between emitted notifications;
	// events arriving inside the window are dropped.
	Quiet time.Duration
	// Out is the terminal the escape sequence is written to. Defaults to
	// stderr, which shares the tty with the renderer without interleaving
	// into its buffered frames.
	Out io.Writer
	// OSC9 reports whether the terminal understands OSC 9; when false a
	// plain BEL is written instead.
	OSC9 bool

	// desktop is Send, injectable for tests.
	desktop func(title, body string)

	mu       sync.Mutex
	lastSent time.Time
}

// New builds a Notifier writing to stderr, with OSC 9 support detected from
// the TERM/TERM_PROGRAM environment.
func New(enabled bool, quiet time.Duration) *Notifier {
	return &Notifier{
		Enabled: enabled,
		Quiet:   quiet,
		Out:     os.Stderr,
		OSC9:    SupportsOSC9(os.Getenv("TERM_PROGRAM"), os.Getenv("TERM")),
		desktop: Send,
	}
}

// SupportsOSC9 reports whether the terminal identified by the TERM_PROGRAM
// and TERM environment values understands OSC 9 notifications.
func SupportsOSC9(termProgram, term string) bool {
	switch termProgram {
	case "iTerm.app", "WezTerm", "ghostty":
		return true
	}
	term = strings.ToLower(term)
	for _, t := range []string{"kitty", "wezterm", "ghostty"} {
		if strings.Contains(term, t) {
			return true
		}
	}
	return false
}

// Notify emits a notification on both channels, honouring the enabled flag
// and the quiet period. Safe to call on a nil Notifier (no-op).
func (n *Notifier) Notify(title, body string) {
	if n == nil || !n.Enabled {
		return
	}
	n.mu.Lock()
	now := time.Now()
	if !n.lastSent.IsZero() && now.Sub(n.lastSent) < n.Quiet {
		n.mu.Unlock()
		return
	}
	n.lastSent = now
	n.mu.Unlock()

	if n.Out != nil {
		if n.OSC9 {
			// OSC 9 ; message BEL — message must not itself contain the
			// terminator, so strip control characters defensively.
			msg := sanitize(title)
			if b := sanitize(body); b != "" {
				msg += ": " + b
			}
			fmt.Fprintf(n.Out, "\x1b]9;%s\x07", msg)
		} else {
			fmt.Fprint(n.Out, "\a")
		}
	}
	if n.desktop != nil {
		n.desktop(title, body)
	}
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
