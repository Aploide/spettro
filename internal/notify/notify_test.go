package notify

import (
	"strings"
	"testing"
	"time"
)

// Send is best-effort by design: with no notification daemon (or no binary on
// PATH at all) it must return without panicking or reporting an error.
func TestSendIsBestEffort(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	Send("title", "body")
	Send("", "")
}

func TestSupportsOSC9(t *testing.T) {
	cases := []struct {
		termProgram, term string
		want              bool
	}{
		{"iTerm.app", "xterm-256color", true},
		{"WezTerm", "xterm-256color", true},
		{"ghostty", "xterm-ghostty", true},
		{"", "xterm-kitty", true},
		{"", "wezterm", true},
		{"Apple_Terminal", "xterm-256color", false},
		{"", "dumb", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := SupportsOSC9(c.termProgram, c.term); got != c.want {
			t.Errorf("SupportsOSC9(%q, %q) = %v, want %v", c.termProgram, c.term, got, c.want)
		}
	}
}

func newTestNotifier(osc9 bool) (*Notifier, *strings.Builder) {
	var out strings.Builder
	n := &Notifier{
		Enabled: true,
		Quiet:   time.Hour,
		Out:     &out,
		OSC9:    osc9,
		desktop: func(string, string) {},
	}
	return n, &out
}

func TestNotifyEmitsOSC9(t *testing.T) {
	n, out := newTestNotifier(true)
	n.Notify("Spettro", "Agent finished")
	if got, want := out.String(), "\x1b]9;Spettro: Agent finished\x07"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNotifyFallsBackToBEL(t *testing.T) {
	n, out := newTestNotifier(false)
	n.Notify("Spettro", "Agent finished")
	if got := out.String(); got != "\a" {
		t.Errorf("got %q, want BEL", got)
	}
}

func TestNotifyDisabled(t *testing.T) {
	n, out := newTestNotifier(true)
	n.Enabled = false
	n.Notify("Spettro", "Agent finished")
	if out.Len() != 0 {
		t.Errorf("disabled notifier wrote %q", out.String())
	}
}

func TestNotifyQuietPeriodSuppressesBursts(t *testing.T) {
	n, out := newTestNotifier(false)
	n.Notify("a", "1")
	n.Notify("a", "2")
	n.Notify("a", "3")
	if got := out.String(); got != "\a" {
		t.Errorf("expected a single BEL inside the quiet period, got %q", got)
	}
}

func TestNotifyStripsControlCharactersFromOSC9(t *testing.T) {
	n, out := newTestNotifier(true)
	n.Notify("Spet\x07tro", "line\x1bbreak")
	if got, want := out.String(), "\x1b]9;Spettro: linebreak\x07"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNotifyNilNotifierIsNoop(t *testing.T) {
	var n *Notifier
	n.Notify("Spettro", "no crash")
}
