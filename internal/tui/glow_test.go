package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"spettro/internal/config"
	"spettro/internal/theme"
)

func TestHighlightUltracodePreservesVisibleText(t *testing.T) {
	cases := []string{
		"",
		"nothing to see here",
		"ultracode",
		"please ultracode this refactor",
		"ULTRACODE and ultracode twice",
		"UltraCode mixed case",
		"an ultracoded word must not light up",
		lipgloss.NewStyle().Foreground(lipgloss.Color("#FF0000")).Render("ultracode inside a styled run"),
	}
	for _, in := range cases {
		got := highlightUltracode(in, 0)
		if stripANSIForTest(got) != stripANSIForTest(in) {
			t.Fatalf("visible text changed:\n in: %q\nout: %q", stripANSIForTest(in), stripANSIForTest(got))
		}
	}
}

func TestHighlightUltracodeOnlyFiresOnTheRealKeyword(t *testing.T) {
	// The highlight must agree with the runtime: a word that lights up is a
	// word that will actually inject the tool.
	if got := highlightUltracode("an ultracoded word", 0); got != "an ultracoded word" {
		t.Fatalf("a non-activating word must be left alone: %q", got)
	}
	if got := highlightUltracode("plain text", 0); got != "plain text" {
		t.Fatalf("untouched text must be returned verbatim: %q", got)
	}
	lit := highlightUltracode("go ultracode now", 0)
	if lit == "go ultracode now" {
		t.Fatal("the keyword was not highlighted")
	}
	if !strings.Contains(lit, "\x1b[") {
		t.Fatalf("no styling emitted: %q", lit)
	}
	// The words around it keep their plain form.
	if !strings.HasPrefix(lit, "go ") || !strings.HasSuffix(lit, " now") {
		t.Fatalf("surrounding text was disturbed: %q", lit)
	}
}

// The textarea draws its cursor as a reverse-video cell. If the cursor lands
// inside the keyword, the glow must step over it rather than swallow it.
func TestHighlightUltracodeKeepsTheCursorCell(t *testing.T) {
	// "ultr" + reversed "a" + "code"
	in := "ultr\x1b[7ma\x1b[27mcode"
	got := highlightUltracode(in, 0)
	if stripANSIForTest(got) != "ultracode" {
		t.Fatalf("visible text = %q", stripANSIForTest(got))
	}
	if !strings.Contains(got, "\x1b[7ma") {
		t.Fatalf("the reversed cursor cell was overwritten: %q", got)
	}
}

func TestHighlightUltracodeRestoresSurroundingStyle(t *testing.T) {
	// A styled run that contains the keyword must still be styled after it.
	in := "\x1b[38;2;255;0;0mred ultracode red\x1b[0m"
	got := highlightUltracode(in, 0)
	if stripANSIForTest(got) != "red ultracode red" {
		t.Fatalf("visible text = %q", stripANSIForTest(got))
	}
	tail := got[strings.LastIndex(got, "e"):]
	_ = tail
	// After the lit run the original foreground is re-asserted, so the trailing
	// " red" is not left wearing the glow's colour.
	idx := strings.Index(got, "\x1b[0m\x1b[38;2;255;0;0m")
	if idx < 0 {
		t.Fatalf("surrounding style was not restored after the keyword: %q", got)
	}
}

func TestUltracodeAnimates(t *testing.T) {
	seen := map[string]bool{}
	for frame := range 200 {
		seen[highlightUltracode("ultracode", frame)] = true
	}
	if len(seen) < 20 {
		t.Fatalf("only %d distinct frames — the effect is barely animating", len(seen))
	}
	// It must loop rather than drift forever: the colour ramp and the sweep
	// each repeat, so the whole effect repeats on their common multiple.
	period := lcm(int(ultracodeDriftFrames), int(ultracodeSweepFrames))
	if a, b := highlightUltracode("ultracode", 3), highlightUltracode("ultracode", 3+period); a != b {
		t.Fatalf("the effect does not repeat after %d frames", period)
	}
}

func lcm(a, b int) int {
	g := a
	h := b
	for h != 0 {
		g, h = h, g%h
	}
	return a / g * b
}

// The sweep is what makes it read as a glow rather than a colour cycle: at any
// moment a couple of cells must be markedly brighter than the rest, and over a
// full pass every cell must get its turn.
func TestUltracodeSweepProducesAHotSpot(t *testing.T) {
	const crest = 0.45
	n := len("ultracode")
	reached := map[int]bool{}
	for frame := range int(ultracodeSweepFrames) {
		for i := range n {
			if _, _, shine := ultracodeCellColor(i, n, frame); shine > crest {
				reached[i] = true
			}
		}
	}
	if len(reached) != n {
		t.Fatalf("the highlight only ever reached %d of %d cells", len(reached), n)
	}
	// And it must not be hot everywhere at once, or it is just "bold".
	lit := 0
	for i := range n {
		if _, _, shine := ultracodeCellColor(i, n, 0); shine > crest {
			lit++
		}
	}
	if lit > 4 {
		t.Fatalf("%d cells lit at once — that is a flash, not a sweep", lit)
	}
}

// Every cell carries a tint dark enough to keep the text legible on top of it.
func TestUltracodeTintStaysDark(t *testing.T) {
	pinTheme(t, theme.Dark())
	n := len("ultracode")
	for frame := range int(ultracodeSweepFrames) {
		for i := range n {
			_, bg, _ := ultracodeCellColor(i, n, frame)
			if lum := lumaOfHex(t, bg); lum > 96 {
				t.Fatalf("tint at cell %d frame %d is too bright (%s, luma %.0f)", i, frame, bg, lum)
			}
		}
	}
}

// The glow reads as a lit object on the page, so it is anchored to the theme's
// own ground rather than to a fixed near-black: switching themes has to reach
// an animation that is already running, and on a light terminal the tint must
// be a pale wash rather than the dark slab the dark theme paints.
func TestUltracodeGlowFollowsTheTheme(t *testing.T) {
	pinTheme(t, theme.Dark())
	dark := highlightUltracode("ultracode", 0)

	pinTheme(t, theme.Light())
	light := highlightUltracode("ultracode", 0)
	if light == dark {
		t.Fatal("the glow did not repaint when the theme changed")
	}
	if stripANSIForTest(light) != "ultracode" {
		t.Fatalf("visible text changed under the light theme: %q", stripANSIForTest(light))
	}

	n := len("ultracode")
	for frame := range int(ultracodeSweepFrames) {
		for i := range n {
			_, bg, _ := ultracodeCellColor(i, n, frame)
			// Comfortably clear of the dark theme's ceiling above: any value
			// in between would mean the tint had stopped tracking the ground.
			if lum := lumaOfHex(t, bg); lum < 150 {
				t.Fatalf("light tint at cell %d frame %d is too dark (%s, luma %.0f)", i, frame, bg, lum)
			}
		}
	}
}

// pinTheme installs a palette for one test and restores the dark default
// afterwards, so a theme-sensitive test cannot leak its palette into the rest
// of the package.
func pinTheme(t *testing.T, p theme.Palette) {
	t.Helper()
	theme.SetPalette(p)
	t.Cleanup(func() { theme.SetPalette(theme.Dark()) })
}

func lumaOfHex(t *testing.T, hex string) float64 {
	t.Helper()
	var r, g, b int
	if _, err := fmt.Sscanf(hex, "#%02X%02X%02X", &r, &g, &b); err != nil {
		t.Fatalf("bad colour %q: %v", hex, err)
	}
	return 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
}

func TestPlanLabelMaxIsSlow(t *testing.T) {
	// The MAX rainbow interpolates its hue on every frame, so consecutive
	// ticks no longer render identically; "slow" is the period instead. One
	// full trip around the rainbow takes planLabelFrameDivisor frames per
	// colour stop — the ~150 ms per step the label has always had.
	changes := 0
	prev := renderPlanLabel("max", 0)
	for frame := 1; frame < planLabelFrameDivisor*4; frame++ {
		cur := renderPlanLabel("max", frame)
		if cur != prev {
			changes++
		}
		prev = cur
	}
	if changes == 0 {
		t.Fatal("MAX stopped animating entirely")
	}
	period := planLabelFrameDivisor * len(theme.Current().RampRainbow)
	if renderPlanLabel("max", 0) != renderPlanLabel("max", period) {
		t.Fatalf("MAX should complete one rainbow cycle in %d frames, not more", period)
	}
	if renderPlanLabel("max", 0) == renderPlanLabel("max", period-1) {
		t.Fatalf("MAX rainbow should take the full %d frames, not fewer", period)
	}
	// Non-animated tiers are unaffected.
	if renderPlanLabel("pro", 0) != renderPlanLabel("pro", 99) {
		t.Fatal("only MAX should animate")
	}
}

func TestSampleRampLoops(t *testing.T) {
	ramp := ultracodeRamp(theme.Dark())
	if a, b := sampleRamp(ramp, 0), sampleRamp(ramp, 1); a != b {
		t.Fatalf("ramp is not seamless: %v vs %v", a, b)
	}
	if a, b := sampleRamp(ramp, 0.25), sampleRamp(ramp, 1.25); a != b {
		t.Fatalf("ramp does not repeat: %v vs %v", a, b)
	}
}

func TestRGBHex(t *testing.T) {
	for in, want := range map[rgbColor]string{
		{0, 0, 0}:          "#000000",
		{255, 255, 255}:    "#FFFFFF",
		{0x7C, 0x3A, 0xED}: "#7C3AED",
		{-10, 300, 127.6}:  "#00FF80",
	} {
		if got := in.hex(); got != want {
			t.Fatalf("%v.hex() = %s, want %s", in, got, want)
		}
	}
}

// The input lights up a plain-English request the same way it lights the
// keyword, because both activate the tool.
func TestHighlightUltracodeLightsPlainEnglishRequests(t *testing.T) {
	lit := highlightUltracode("please use a workflow for this", 0)
	if lit == "please use a workflow for this" {
		t.Fatal("a plain-English request was not highlighted")
	}
	if stripANSIForTest(lit) != "please use a workflow for this" {
		t.Fatalf("visible text changed: %q", stripANSIForTest(lit))
	}
	// An ordinary mention of the word stays plain, matching the runtime.
	for _, in := range []string{"our deploy workflow is broken", "fix .github/workflows/ci.yml"} {
		if got := highlightUltracode(in, 0); got != in {
			t.Fatalf("%q should not light up: %q", in, got)
		}
	}
}

// A "+500k" budget directive glows only when it will be honoured — when
// workflows are on for the message — and then exactly the directive does.
func TestHighlightBudgetDirectives(t *testing.T) {
	const in = "audit the parser +500k please"
	if got := highlightWorkflowInput(in, 0, false); got != in {
		t.Fatalf("a directive must stay plain while workflows are off: %q", got)
	}
	lit := highlightWorkflowInput(in, 0, true)
	if lit == in || stripANSIForTest(lit) != in {
		t.Fatalf("the directive should glow without changing the text: %q", lit)
	}
	if !strings.HasPrefix(lit, "audit the parser ") || !strings.HasSuffix(lit, " please") {
		t.Fatalf("only the directive may be styled: %q", lit)
	}
	// Not directives: arithmetic, a glued "+", a bare number.
	for _, plain := range []string{"x = a+500k", "add +5 to it", "C++ code"} {
		if got := highlightWorkflowInput(plain, 0, true); got != plain {
			t.Fatalf("%q is not a directive and must stay plain: %q", plain, got)
		}
	}
	// With the keyword in the message, both light up.
	both := highlightWorkflowInput("ultracode +1.5m", 0, true)
	if both == "ultracode +1.5m" || stripANSIForTest(both) != "ultracode +1.5m" {
		t.Fatalf("keyword and directive should both glow: %q", both)
	}
}

func TestBudgetDirectivesLiveOnlyWithWorkflows(t *testing.T) {
	cases := []struct {
		input string
		ultra bool
		perm  config.PermissionLevel
		want  bool
	}{
		{"audit this +500k", false, config.PermissionRestricted, false},
		{"ultracode: audit this +500k", false, config.PermissionRestricted, true},
		{"use a workflow for this +2M", false, config.PermissionRestricted, true},
		{"audit this +500k", true, config.PermissionRestricted, true},
		{"audit this", false, config.PermissionRestricted, false},
		// A suspended ultra (ask-first, where workflows cannot run) does
		// not make a bare directive live; the keyword still does.
		{"audit this +500k", true, config.PermissionAskFirst, false},
		{"ultracode: audit this +500k", true, config.PermissionAskFirst, true},
	}
	for _, c := range cases {
		m := NewModelForTesting()
		m.cfg.Ultra = c.ultra
		m.cfg.Permission = c.perm
		m.SetTextareaValueForTesting(c.input)
		if got := m.budgetDirectivesLive(); got != c.want {
			t.Errorf("budgetDirectivesLive(%q, ultra=%v, %s) = %v, want %v", c.input, c.ultra, c.perm, got, c.want)
		}
	}
	// The animation gate agrees: with ultra live, a directive alone needs
	// frames; without it, a directive without the keyword never glows.
	if !inputMayGlow("audit +500k", true) || inputMayGlow("audit +500k", false) {
		t.Fatal("inputMayGlow disagrees with when a directive can glow")
	}
}
