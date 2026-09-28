package tui

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"spettro/internal/config"
	"spettro/internal/theme"
)

// newThemeTestModel isolates everything /theme reads or writes: a temp HOME,
// because the command persists through config.Update and would otherwise
// rewrite the developer's real ~/.spettro/config.json, and the two environment
// variables that outrank the config. The palette is process-global and outlives
// any single test, so it is pinned on the way in and restored on the way out.
func newThemeTestModel(t *testing.T) Model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(theme.EnvVar, "")
	t.Setenv("COLORFGBG", "")
	theme.SetPalette(theme.Dark())
	t.Cleanup(func() { theme.SetPalette(theme.Dark()) })
	return NewModelForTesting()
}

// sgrFg and sgrBg are the SGR parameter runs lipgloss emits for a truecolor
// foreground and background. Tests match the numbers rather than a whole
// escape sequence because lipgloss merges several attributes into one CSI, so
// the colour is not always the only parameter in its sequence.
func sgrFg(c color.Color) string {
	r, g, b := theme.RGB(c)
	return fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
}

func sgrBg(c color.Color) string {
	r, g, b := theme.RGB(c)
	return fmt.Sprintf("48;2;%d;%d;%d", r, g, b)
}

func TestThemeCommandReportsSelection(t *testing.T) {
	m := newThemeTestModel(t)
	nm, _ := m.handleCommand("/theme")
	got := nm.(Model)

	// The three facts a bare /theme owes the user: what is selected, what
	// that resolved to, and who decided. "auto" alone answers none of them.
	for _, want := range []string{
		"theme: auto",
		"rendering: dark",
		"selected by: default",
		"resolved by:",
		"/theme <dark|light|auto>",
	} {
		if !hasSystemMsg(got, want) {
			t.Errorf("/theme report is missing %q", want)
		}
	}
	if got.cfg.Theme != "" {
		t.Errorf("a bare /theme must not persist anything, cfg.Theme = %q", got.cfg.Theme)
	}
}

func TestThemeSummaryNamesTheDecidingSource(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		m := newThemeTestModel(t)
		t.Setenv(theme.EnvVar, "light")
		m.cfg.Theme = "dark"
		got := m.themeSummary()
		if !strings.Contains(got, theme.EnvVar+"=light") {
			t.Errorf("summary = %q, want it to credit the environment variable", got)
		}
		if !strings.Contains(got, "theme: light") {
			t.Errorf("summary = %q, want the env var to outrank the config's dark", got)
		}
	})

	t.Run("config", func(t *testing.T) {
		m := newThemeTestModel(t)
		m.cfg.Theme = "light"
		got := m.themeSummary()
		if !strings.Contains(got, `the "theme" key in your user config`) {
			t.Errorf("summary = %q, want it to credit the config key", got)
		}
	})

	t.Run("detected", func(t *testing.T) {
		m := newThemeTestModel(t)
		m.themeAuto = true
		m.themeDetected = true
		got := m.themeSummary()
		if !strings.Contains(got, "terminal's reported background") {
			t.Errorf("summary = %q, want it to credit terminal detection", got)
		}
	})

	// COLORFGBG only decides on a terminal that could have been asked and did
	// not answer — theme.Seed refuses to let an inherited variable decide
	// otherwise — and a test binary is never such a terminal, so the branch is
	// exercised through themeResolutionText rather than through the model.
	t.Run("colorfgbg", func(t *testing.T) {
		got := themeResolutionText(theme.AutoKind, false, true, "0;15")
		if !strings.Contains(got, "COLORFGBG=0;15") {
			t.Errorf("resolution = %q, want it to credit COLORFGBG", got)
		}
	})

	t.Run("colorfgbg is not credited on an unqueryable terminal", func(t *testing.T) {
		m := newThemeTestModel(t)
		t.Setenv("COLORFGBG", "0;15")
		got := m.themeSummary()
		if strings.Contains(got, "COLORFGBG") && !strings.Contains(got, "not trusted") {
			t.Errorf("summary = %q, want it to say the palette came from the dark fallback; startup never consulted COLORFGBG here", got)
		}
		// The report and the resolver have to agree about the same inputs.
		if want, got := theme.Seed(theme.AutoKind), theme.CurrentKind(); want != got {
			t.Errorf("Seed(auto) = %q but the palette in force is %q", want, got)
		}
	})
}

func TestThemeCommandSetsAndPersistsEachValue(t *testing.T) {
	cases := []struct {
		arg      string
		resolved theme.Kind
		auto     bool
	}{
		// "auto" resolves to dark here: the test binary's stdout is a pipe,
		// so there is no terminal to query and COLORFGBG is blanked.
		{"dark", theme.DarkKind, false},
		{"light", theme.LightKind, false},
		{"auto", theme.DarkKind, true},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			m := newThemeTestModel(t)
			nm, _ := m.handleCommand("/theme " + tc.arg)
			got := nm.(Model)

			if theme.CurrentKind() != tc.resolved {
				t.Errorf("palette = %s, want %s", theme.CurrentKind(), tc.resolved)
			}
			if got.themeAuto != tc.auto {
				t.Errorf("themeAuto = %v, want %v", got.themeAuto, tc.auto)
			}
			if !strings.Contains(got.banner, "theme set to "+tc.arg) {
				t.Errorf("banner = %q, want it to confirm %q", got.banner, tc.arg)
			}
			if got.cfg.Theme != tc.arg {
				t.Errorf("in-memory cfg.Theme = %q, want %q", got.cfg.Theme, tc.arg)
			}

			// The round trip that matters is through the file, not the
			// struct: a value the config's normalize() rejects is silently
			// cleared on the next load rather than reported here.
			reloaded, err := config.Load()
			if err != nil {
				t.Fatalf("reload config: %v", err)
			}
			if reloaded.Theme != tc.arg {
				t.Errorf("persisted theme = %q, want %q", reloaded.Theme, tc.arg)
			}
			if k := theme.Preferred(reloaded.Theme); k != theme.Kind(tc.arg) {
				t.Errorf("theme.Preferred(%q) = %s, want %s", reloaded.Theme, k, tc.arg)
			}
		})
	}
}

func TestThemeCommandRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"/theme solarized", "invalid theme"},
		{"/theme darkk", "invalid theme"},
		{"/theme dark light", "usage"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			m := newThemeTestModel(t)
			nm, _ := m.handleCommand(tc.input)
			got := nm.(Model)

			if !strings.Contains(got.banner, tc.want) {
				t.Errorf("banner = %q, want it to contain %q", got.banner, tc.want)
			}
			for _, valid := range []string{"dark", "light", "auto"} {
				if !strings.Contains(got.banner, valid) {
					t.Errorf("banner = %q, want it to list %q as a valid value", got.banner, valid)
				}
			}
			if got.cfg.Theme != "" {
				t.Errorf("a rejected theme must not be persisted, cfg.Theme = %q", got.cfg.Theme)
			}
			if theme.CurrentKind() != theme.DarkKind {
				t.Errorf("a rejected theme must not move the palette, now %s", theme.CurrentKind())
			}
		})
	}
}

// TestThemeCommandRepaintsToLight is the end-to-end claim the feature rests
// on: a light terminal user runs one command and the frame they are looking at
// stops painting dark slabs.
func TestThemeCommandRepaintsToLight(t *testing.T) {
	m := newThemeTestModel(t)
	m.SetDimensionsForTesting(120, 40)
	m.MarkReadyAndTrustedForTesting()
	m = m.RecalcLayoutForTesting()

	dark := m.ViewForTesting()
	if !strings.Contains(dark, sgrBg(theme.Dark().BgHeader)) {
		t.Fatalf("the dark frame does not paint the dark header band; the assertion below would prove nothing")
	}

	nm, _ := m.handleCommand("/theme light")
	got := nm.(Model)
	light := got.ViewForTesting()

	if light == dark {
		t.Fatal("switching to light left the rendered frame byte-identical")
	}
	if !strings.Contains(light, sgrBg(theme.Light().BgHeader)) {
		t.Error("the light frame does not paint the light header band")
	}
	if strings.Contains(light, sgrBg(theme.Dark().BgHeader)) {
		t.Error("the light frame still paints the dark header band")
	}
	// The render cache is keyed on width and accent but not on the palette,
	// so a stale cache would leave already-rendered blocks in the old theme.
	if got.RenderCacheSizeForTesting() > 0 {
		t.Error("applyTheme must drop the message render cache")
	}
}

// TestDarkRenderingIsUnchangedByTheming pins the tui layer rather than the
// palette: internal/theme has its own test that Dark() holds the pre-theme
// hexes, but this asserts that the roles, modeColor and the glare sweep still
// emit exactly the escape sequences the hardcoded literals emitted before
// themes existed. Dark mode is the status quo given names, not a redesign.
func TestDarkRenderingIsUnchangedByTheming(t *testing.T) {
	theme.SetPalette(theme.Dark())
	t.Cleanup(func() { theme.SetPalette(theme.Dark()) })

	roles := map[string]struct {
		got  color.Color
		want string
	}{
		"colorText":     {colorText, "#F9FAFB"},
		"colorMuted":    {colorMuted, "#6B7280"},
		"colorDim":      {colorDim, "#374151"},
		"colorBorder":   {colorBorder, "#4B5563"},
		"colorSuccess":  {colorSuccess, "#10B981"},
		"colorError":    {colorError, "#EF4444"},
		"colorWarn":     {colorWarn, "#F59E0B"},
		"colorToolPend": {colorToolPend, "#F59E0B"},
		"colorToolRun":  {colorToolRun, "#60A5FA"},
		"colorToolOK":   {colorToolOK, "#10B981"},
		"colorToolErr":  {colorToolErr, "#EF4444"},
		"colorRule":     {colorRule, "#374151"},
	}
	for name, tc := range roles {
		want := lipgloss.NewStyle().Foreground(lipgloss.Color(tc.want)).Render("x")
		if got := lipgloss.NewStyle().Foreground(tc.got).Render("x"); got != want {
			t.Errorf("%s renders %q, want %q (the pre-theme literal %s)", name, got, want, tc.want)
		}
	}

	// modeColor's 11 cases plus the unknown-name fallback, which is what
	// roughly forty call sites reach the agent accents through.
	modes := map[string]string{
		"blue":     "#A78BFA",
		"green":    "#34D399",
		"cyan":     "#60A5FA",
		"yellow":   "#F59E0B",
		"magenta":  "#C084FC",
		"purple":   "#BD93F9",
		"red":      "#EF4444",
		"plan":     "#BD93F9",
		"planning": "#A78BFA",
		"coding":   "#34D399",
		"chat":     "#60A5FA",
		"":         "#60A5FA",
	}
	for name, want := range modes {
		if got := hexOfColor(modeColor(name)); got != want {
			t.Errorf("modeColor(%q) = %s, want %s", name, got, want)
		}
	}

	// The glare sweep used to lerp toward a hardcoded 255 at four inline
	// weights. Both the target and the weights are palette roles now, so the
	// old arithmetic is reproduced here and compared stop for stop.
	oldLerpToWhite := func(c color.Color, t float64) color.Color {
		r, g, b := theme.RGB(c)
		mix := func(v uint8) uint8 { return uint8(float64(v) + t*(255-float64(v))) }
		return color.RGBA{R: mix(r), G: mix(g), B: mix(b), A: 0xFF}
	}
	for _, name := range []string{"blue", "green", "cyan", "yellow", "magenta", "purple", "red"} {
		base := modeColor(name)
		got := glareGradient(base)
		want := [5]color.Color{
			oldLerpToWhite(base, 0.88),
			oldLerpToWhite(base, 0.60),
			oldLerpToWhite(base, 0.30),
			oldLerpToWhite(base, 0.12),
			base,
		}
		for i := range want {
			if hexOfColor(got[i]) != hexOfColor(want[i]) {
				t.Errorf("glareGradient(%s)[%d] = %s, want %s", name, i, hexOfColor(got[i]), hexOfColor(want[i]))
			}
		}
	}
}

// TestLightRenderingChangesTheNeutrals guards the other direction: a role that
// silently kept its dark value would leave unreadable ink on a light terminal
// while every dark-parity assertion above still passed.
func TestLightRenderingChangesTheNeutrals(t *testing.T) {
	theme.SetPalette(theme.Light())
	t.Cleanup(func() { theme.SetPalette(theme.Dark()) })

	for name, r := range map[string]role{
		"colorText":   colorText,
		"colorMuted":  colorMuted,
		"colorDim":    colorDim,
		"colorRule":   colorRule,
		"colorBorder": colorBorder,
	} {
		if got := lipgloss.NewStyle().Foreground(r).Render("x"); strings.Contains(got, sgrFg(theme.Dark().Text)) {
			t.Errorf("%s still resolves through the dark palette", name)
		}
	}
	if hexOfColor(colorText) != "#111827" {
		t.Errorf("light colorText = %s, want the light theme's ink", hexOfColor(colorText))
	}
	if theme.Light().IsDark() {
		t.Fatal("the light palette must not report itself as dark")
	}
}

func hexOfColor(c color.Color) string {
	r, g, b := theme.RGB(c)
	return fmt.Sprintf("#%02X%02X%02X", r, g, b)
}

// TestThemeCommandIsDiscoverable covers the three places a command has to be
// registered to actually be reachable: the completion catalog, the /help text,
// and the instant-command list that lets it run while an agent is mid-turn.
func TestThemeCommandIsDiscoverable(t *testing.T) {
	m := newThemeTestModel(t)

	var found bool
	for _, c := range m.filterCommands("theme") {
		if c.name == "/theme" {
			found = true
		}
	}
	if !found {
		t.Error("/theme is missing from the command catalog")
	}
	if !strings.Contains(helpText, "/theme") {
		t.Error("/theme is missing from /help")
	}
	if !isInstantCommand("/theme light") {
		t.Error("/theme only touches local config and display state, so it must be instant")
	}
	// /theme must NOT offer a second-level menu. It used to: selecting
	// "/theme" from the completion list inserted "/theme ", which re-opened
	// the sub-menu, whose own selection inserted "/theme dark", which matched
	// the sub-menu prefix again. Every enter inserted and none dispatched, so
	// the command could never actually run and appeared to do nothing.
	m.ta.SetValue("/theme ")
	m.syncInputSuggestions()
	for _, it := range m.cmdItems {
		if strings.Contains(strings.TrimPrefix(it.name, "/"), " ") {
			t.Errorf("/theme offered the sub-command %q; enter must dispatch /theme itself", it.name)
		}
	}
	// And enter on it must actually reach the handler.
	nm, _ := m.updateMain(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got, ok := nm.(Model); !ok || !got.showThemePicker {
		t.Error("enter on /theme did not dispatch the command")
	}
	// Bare /theme opens the picker, so it must not be gated behind a required
	// parameter the way /permission and /thinking are.
	if requiresParam("/theme") {
		t.Error("bare /theme is a valid command and must not require a parameter")
	}
}

// TestBareThemeOpensThePicker pins the entry point: the whole reason the
// command exists is choosing a palette by looking at it.
func TestBareThemeOpensThePicker(t *testing.T) {
	m := newThemeTestModel(t)
	nm, _ := m.handleCommand("/theme")
	got, ok := nm.(Model)
	if !ok {
		t.Fatalf("handleCommand returned %T, want Model", nm)
	}
	if !got.showThemePicker {
		t.Fatal("bare /theme did not open the theme picker")
	}
	if got.activeModal() != modalThemePicker {
		t.Errorf("activeModal = %v, want modalThemePicker", got.activeModal())
	}
	if view := got.viewThemePicker(); !strings.Contains(view, "select theme") {
		t.Error("the picker did not render its title")
	}
	// An argument still applies directly, so the command stays scriptable.
	nm, _ = m.handleCommand("/theme light")
	if got, ok := nm.(Model); ok && got.showThemePicker {
		t.Error("/theme light opened the picker instead of applying the theme")
	}
}

// TestThemePickerRendersThroughView goes through the real View() rather than
// calling viewThemePicker directly. Registering a modal takes three separate
// edits — the const, activeModal, and the handler table — and a dialog missing
// only the third one still reports the right activeModal and still renders
// correctly when its view method is called by hand. It just never appears on
// screen, which is exactly how it shipped the first time.
func TestThemePickerRendersThroughView(t *testing.T) {
	m := newThemeTestModel(t)
	m.SetDimensionsForTesting(120, 40)
	m.MarkReadyAndTrustedForTesting()
	m = m.RecalcLayoutForTesting()

	nm, _ := m.handleCommand("/theme")
	got := nm.(Model)
	if v := got.ViewForTesting(); !strings.Contains(v, "select theme") {
		t.Errorf("the picker is open (activeModal=%v) but View() does not render it", got.activeModal())
	}
}

// TestThemePickerPreviewsWithoutInstalling covers the design the picker
// settled on: the panel must show the candidate theme's own inks, and moving
// the cursor must not touch the palette actually in force. Installing the
// candidate instead would repaint the dialog itself, and previewing a dark
// theme on a light terminal then leaves the chrome unreadable — the picker
// would break the screen the user is choosing on.
func TestThemePickerPreviewsWithoutInstalling(t *testing.T) {
	m := newThemeTestModel(t)
	m.SetDimensionsForTesting(120, 40)
	m.MarkReadyAndTrustedForTesting()
	m = m.RecalcLayoutForTesting()
	m = m.applyTheme(theme.DarkKind)

	m = m.openThemePicker()
	for themePickerOrder[m.themeCursor] != theme.LightKind {
		nm, _ := m.updateThemePicker(tea.KeyPressMsg{Code: tea.KeyDown})
		m = nm.(Model)
	}

	if theme.CurrentKind() != theme.DarkKind {
		t.Errorf("navigating to light installed %s; the picker must not move the live palette", theme.CurrentKind())
	}
	view := m.viewThemePicker()
	if !strings.Contains(view, sgrBg(theme.Light().BgCode)) {
		t.Error("the preview panel does not paint the light theme's code background")
	}
	if !strings.Contains(view, sgrBg(theme.Light().BgBase)) {
		t.Error("the preview panel does not paint the light theme's page background; its inks would sit on the terminal's own colour")
	}

	// Escape has nothing to undo, but it must still leave the palette alone.
	nm, _ := m.updateThemePicker(tea.KeyPressMsg{Code: tea.KeyEscape})
	got := nm.(Model)
	if got.showThemePicker {
		t.Error("escape left the picker open")
	}
	if theme.CurrentKind() != theme.DarkKind {
		t.Errorf("escape moved the palette to %s", theme.CurrentKind())
	}
	if got.cfg.Theme != "" {
		t.Errorf("escape persisted %q", got.cfg.Theme)
	}
}

// TestThemePickerEnterApplies pins the other half: confirming does install and
// persist, through the same path as /theme <kind>.
func TestThemePickerEnterApplies(t *testing.T) {
	m := newThemeTestModel(t)
	m = m.openThemePicker()
	for themePickerOrder[m.themeCursor] != theme.LightKind {
		nm, _ := m.updateThemePicker(tea.KeyPressMsg{Code: tea.KeyDown})
		m = nm.(Model)
	}
	nm, _ := m.updateThemePicker(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := nm.(Model)

	if got.showThemePicker {
		t.Error("enter left the picker open")
	}
	if theme.CurrentKind() != theme.LightKind {
		t.Errorf("palette = %s, want light", theme.CurrentKind())
	}
	if got.cfg.Theme != "light" {
		t.Errorf("cfg.Theme = %q, want light", got.cfg.Theme)
	}
}

// TestThemeSelectionMirrorsPreferred pins the report to the resolver. Bare
// /theme is what docs/troubleshooting.md sends a user to when the palette is
// wrong, so a selection it names that theme.Preferred would not have chosen
// makes the diagnostic worse than silence — it points away from the source
// that actually decided.
func TestThemeSelectionMirrorsPreferred(t *testing.T) {
	values := []string{"", "dark", "light", "auto", "solarized", "  LIGHT  "}
	for _, env := range values {
		for _, cfgTheme := range values {
			m := newThemeTestModel(t)
			t.Setenv(theme.EnvVar, env)
			m.cfg.Theme = cfgTheme

			want := theme.Preferred(m.cfg.Theme)
			got, source := m.themeSelection()
			if got != want {
				t.Errorf("themeSelection() = %q with SPETTRO_THEME=%q cfg=%q, but theme.Preferred = %q",
					got, env, cfgTheme, want)
			}
			// A value that was skipped has to survive into the report, or the
			// user never learns their typo was the reason nothing happened.
			if strings.TrimSpace(env) != "" {
				if _, ok := theme.Parse(env); !ok && !strings.Contains(source, env) {
					t.Errorf("source %q with an invalid SPETTRO_THEME=%q never mentions it", source, env)
				}
			}
		}
	}
}

// TestUnparseableBackgroundReplyIsNotADetection guards the provenance flag
// rather than the palette. ansi.XParseColor returns a nil colour for an OSC 11
// body it cannot decode, and Resolve already treats that as "nothing
// detected"; the flag /theme reads to explain itself has to agree.
func TestUnparseableBackgroundReplyIsNotADetection(t *testing.T) {
	m := newThemeTestModel(t)
	m.themeAuto = true

	nm, _ := m.Update(tea.BackgroundColorMsg{Color: nil})
	got := nm.(Model)
	if got.themeDetected {
		t.Error("a nil background reply must not be recorded as a detection")
	}
	if s := got.themeSummary(); strings.Contains(s, "terminal's reported background") {
		t.Errorf("summary = %q, want it to credit COLORFGBG or the fallback, not a reply that could not be decoded", s)
	}

	nm, _ = m.Update(tea.BackgroundColorMsg{Color: lipgloss.Color("#FFFFFF")})
	if got := nm.(Model); !got.themeDetected {
		t.Error("a decodable background reply must be recorded as a detection")
	}
}

// TestThemeSwitchInvalidatesTheQuestionPreview covers the second memoised
// render in the TUI. Its "… N more lines" footer is styled at cache-fill time,
// so a switch that only drops renderCache leaves that one line in the old
// palette until the user happens to move the cursor.
func TestThemeSwitchInvalidatesTheQuestionPreview(t *testing.T) {
	m := newThemeTestModel(t)
	m.pendingQuestion = &questionForm{cursor: []int{0}}
	preview := strings.Repeat("a preview line\n", 20)

	dark := m.questionPreviewLines(preview, 40, 5)
	if len(dark) == 0 || !strings.Contains(dark[len(dark)-1], sgrFg(theme.Dark().TextMuted)) {
		t.Fatalf("the dark footer is not painted with TextMuted; the assertion below would prove nothing: %q", dark)
	}

	m = m.applyTheme(theme.LightKind)
	light := m.questionPreviewLines(preview, 40, 5)
	if strings.Contains(light[len(light)-1], sgrFg(theme.Dark().TextMuted)) {
		t.Errorf("the preview footer is still painted in the dark palette after a theme switch: %q", light[len(light)-1])
	}
	if !strings.Contains(light[len(light)-1], sgrFg(theme.Light().TextMuted)) {
		t.Errorf("the preview footer did not repaint into the light palette: %q", light[len(light)-1])
	}
}

// TestLightCursorIsNotInvisible is the one widget the theme reaches into that
// bubbles does not vary by polarity: DefaultStyles hardcodes Cursor.Color to
// ANSI 7 for both, and the virtual cursor renders it reversed — a silver block
// whose glyph is painted in the terminal's own background, i.e. white on white.
func TestLightCursorIsNotInvisible(t *testing.T) {
	m := newThemeTestModel(t)
	m.SetDimensionsForTesting(120, 40)
	m = m.applyTheme(theme.LightKind)

	if got := m.ta.Styles().Cursor.Color; hexOfColor(got) != hexOfColor(theme.Light().Text) {
		t.Errorf("light cursor ink = %s, want the light theme's body text %s",
			hexOfColor(got), hexOfColor(theme.Light().Text))
	}
	if !strings.Contains(m.viewInput(120), "\x1b[7;"+sgrFg(theme.Light().Text)) {
		t.Error("the light input box does not paint a reversed body-text caret")
	}

	// The dark branch keeps bubbles' own chrome, byte for byte.
	m = m.applyTheme(theme.DarkKind)
	if !strings.Contains(m.viewInput(120), "\x1b[7;37m") {
		t.Error("the dark input box no longer emits bubbles' reverse-video ANSI-7 caret")
	}
}

// Found in a VHS screenshot of the theme picker: "light" is a cell longer
// than "auto" and "dark", so its description started one column right of
// the others. Every option's description now starts in the same column.
func TestThemePickerDescriptionsLineUp(t *testing.T) {
	m := newThemeTestModel(t)
	m.SetDimensionsForTesting(80, 24)
	m.MarkReadyAndTrustedForTesting()
	m = m.RecalcLayoutForTesting()
	for cursor := range themePickerOrder {
		m = m.openThemePicker()
		m.themeCursor = cursor
		cols := map[int]bool{}
		for _, row := range strings.Split(ansi.Strip(m.viewThemePicker()), "\n") {
			for _, desc := range []string{"follow the terminal", "palette tuned for a dark", "palette tuned for a light"} {
				if i := strings.Index(row, desc); i >= 0 {
					cols[ansi.StringWidth(row[:i])] = true
				}
			}
		}
		if len(cols) != 1 {
			t.Errorf("cursor on %v: the descriptions start in columns %v, want one column", themePickerOrder[cursor], cols)
		}
	}
}
