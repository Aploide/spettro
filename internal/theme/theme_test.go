package theme

import (
	"image/color"
	"math"
	"reflect"
	"strconv"
	"testing"

	"charm.land/lipgloss/v2"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Kind
		ok   bool
	}{
		{"dark", DarkKind, true},
		{"light", LightKind, true},
		{"auto", AutoKind, true},
		{"", AutoKind, true},
		{"  Dark  ", DarkKind, true},
		{"LIGHT", LightKind, true},
		{"solarized", AutoKind, false},
		{"darker", AutoKind, false},
		{"1", AutoKind, false},
	}
	for _, tc := range cases {
		got, ok := Parse(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Parse(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPreferredPrecedence(t *testing.T) {
	t.Run("env beats config", func(t *testing.T) {
		t.Setenv(EnvVar, "light")
		if got := Preferred("dark"); got != LightKind {
			t.Errorf("Preferred = %q, want light", got)
		}
	})
	t.Run("invalid env falls through to config", func(t *testing.T) {
		t.Setenv(EnvVar, "chartreuse")
		if got := Preferred("light"); got != LightKind {
			t.Errorf("Preferred = %q, want light", got)
		}
	})
	t.Run("empty env falls through to config", func(t *testing.T) {
		t.Setenv(EnvVar, "")
		if got := Preferred("dark"); got != DarkKind {
			t.Errorf("Preferred = %q, want dark", got)
		}
	})
	t.Run("no env and no config is auto", func(t *testing.T) {
		t.Setenv(EnvVar, "")
		if got := Preferred(""); got != AutoKind {
			t.Errorf("Preferred = %q, want auto", got)
		}
	})
	t.Run("invalid config is auto", func(t *testing.T) {
		t.Setenv(EnvVar, "")
		if got := Preferred("neon"); got != AutoKind {
			t.Errorf("Preferred = %q, want auto", got)
		}
	})
}

func TestResolve(t *testing.T) {
	white := lipgloss.Color("#FFFFFF")
	black := lipgloss.Color("#000000")

	t.Run("explicit selection ignores detection", func(t *testing.T) {
		t.Setenv("COLORFGBG", "0;15")
		if got := Resolve(DarkKind, white); got != DarkKind {
			t.Errorf("Resolve(dark, white) = %q, want dark", got)
		}
		if got := Resolve(LightKind, black); got != LightKind {
			t.Errorf("Resolve(light, black) = %q, want light", got)
		}
	})
	t.Run("auto uses the detected background", func(t *testing.T) {
		t.Setenv("COLORFGBG", "15;0") // says dark; the detected colour must win
		if got := Resolve(AutoKind, white); got != LightKind {
			t.Errorf("Resolve(auto, white) = %q, want light", got)
		}
		if got := Resolve(AutoKind, black); got != DarkKind {
			t.Errorf("Resolve(auto, black) = %q, want dark", got)
		}
	})
	t.Run("auto falls back to COLORFGBG", func(t *testing.T) {
		t.Setenv("COLORFGBG", "0;15")
		if got := Resolve(AutoKind, nil); got != LightKind {
			t.Errorf("Resolve(auto, nil) = %q, want light", got)
		}
		t.Setenv("COLORFGBG", "15;0")
		if got := Resolve(AutoKind, nil); got != DarkKind {
			t.Errorf("Resolve(auto, nil) = %q, want dark", got)
		}
	})
	t.Run("auto degrades to dark when nothing is known", func(t *testing.T) {
		t.Setenv("COLORFGBG", "")
		if got := Resolve(AutoKind, nil); got != DarkKind {
			t.Errorf("Resolve(auto, nil) = %q, want dark", got)
		}
		if got := Resolve(Kind("bogus"), nil); got != DarkKind {
			t.Errorf("Resolve(bogus, nil) = %q, want dark", got)
		}
	})
}

func TestIsDarkColor(t *testing.T) {
	cases := []struct {
		name string
		c    color.Color
		want bool
	}{
		{"nil degrades to dark", nil, true},
		{"black", lipgloss.Color("#000000"), true},
		{"white", lipgloss.Color("#FFFFFF"), false},
		{"vscode dark", lipgloss.Color("#1E1E1E"), true},
		{"solarized dark", lipgloss.Color("#002B36"), true},
		{"solarized light", lipgloss.Color("#FDF6E3"), false},
		// L is exactly 0.5019…, just over the threshold: mid grey counts as
		// light, matching Bubble Tea and Lip Gloss rather than intuition.
		{"mid grey is light", lipgloss.Color("#808080"), false},
		{"one step below mid grey is dark", lipgloss.Color("#7F7F7F"), true},
		{"saturated mid green is light by HSL", lipgloss.Color("#00FF00"), false},
		{"dark saturated green", lipgloss.Color("#008000"), true},
	}
	for _, tc := range cases {
		if got := IsDarkColor(tc.c); got != tc.want {
			t.Errorf("%s: IsDarkColor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestColorFGBG(t *testing.T) {
	cases := []struct {
		in   string
		dark bool
		ok   bool
	}{
		{"15;0", true, true},
		{"0;15", false, true},
		{"15;default;0", true, true},  // rxvt three-field form
		{"0;default;15", false, true}, // ditto, light
		{"7;8", true, true},           // bright black background
		{"0;7", false, true},          // silver background
		{"0;6", true, true},           // cyan, still the dark half
		{"0;9", false, true},          // bright red, the light half
		{" 15 ; 0 ", true, true},      // whitespace tolerated
		{"", false, false},            // unset
		{"15", false, false},          // single field
		{"15;default", false, false},  // background field is not a number
		{"15;16", false, false},       // outside the 16-colour range
		{"15;-1", false, false},       // negative index
		{"15;abc", false, false},      // garbage
		{"15;0;", false, false},       // trailing separator, empty background
	}
	for _, tc := range cases {
		dark, ok := ColorFGBG(tc.in)
		if dark != tc.dark || ok != tc.ok {
			t.Errorf("ColorFGBG(%q) = %v, %v; want %v, %v", tc.in, dark, ok, tc.dark, tc.ok)
		}
	}
}

func TestCurrentAndSet(t *testing.T) {
	restore := Current()
	t.Cleanup(func() { SetPalette(restore) })

	if got := Set(LightKind); got.Kind != LightKind {
		t.Fatalf("Set(light).Kind = %q, want light", got.Kind)
	}
	if CurrentKind() != LightKind {
		t.Errorf("CurrentKind = %q, want light", CurrentKind())
	}
	if Current().IsDark() {
		t.Error("light palette reports IsDark")
	}

	Set(DarkKind)
	if CurrentKind() != DarkKind || !Current().IsDark() {
		t.Errorf("CurrentKind = %q, IsDark = %v; want dark, true", CurrentKind(), Current().IsDark())
	}

	// Set never leaves AutoKind installed: the render path must always have a
	// concrete palette to read.
	t.Setenv("COLORFGBG", "")
	if got := Set(AutoKind); got.Kind == AutoKind {
		t.Error("Set(auto) left an unresolved palette installed")
	}
}

// TestPaletteCompleteness walks both palettes by reflection so a role added to
// the struct cannot be silently left unset in one theme. The default branch is
// the point: a field of an unhandled kind fails rather than passing by
// omission.
func TestPaletteCompleteness(t *testing.T) {
	for _, p := range []Palette{Dark(), Light()} {
		v := reflect.ValueOf(p)
		typ := v.Type()
		for i := range typ.NumField() {
			checkField(t, string(p.Kind)+"."+typ.Field(i).Name, v.Field(i))
		}
	}
}

func checkField(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.Interface: // color.Color
		if v.IsNil() {
			t.Errorf("%s is nil", path)
			return
		}
		if _, ok := v.Interface().(lipgloss.NoColor); ok {
			t.Errorf("%s is NoColor (malformed literal?)", path)
		}
	case reflect.Slice, reflect.Array:
		if v.Len() == 0 {
			t.Errorf("%s is empty", path)
			return
		}
		for i := range v.Len() {
			checkField(t, path+"["+strconv.Itoa(i)+"]", v.Index(i))
		}
	case reflect.Float64:
		if v.Float() <= 0 {
			t.Errorf("%s = %v, want a positive weight", path, v.Float())
		}
	case reflect.String: // Kind
		if v.String() == "" {
			t.Errorf("%s is empty", path)
		}
	default:
		t.Errorf("%s: unhandled field kind %s — extend checkField for it", path, v.Kind())
	}
}

// TestDarkPaletteIsUnchanged pins the dark theme to the literals the TUI used
// before themes existed. Dark mode is meant to be the status quo given names,
// so any drift here is a regression rather than a redesign.
func TestDarkPaletteIsUnchanged(t *testing.T) {
	p := Dark()
	cases := map[string]struct {
		got  color.Color
		want string
	}{
		"Text":          {p.Text, "#F9FAFB"},
		"TextMuted":     {p.TextMuted, "#6B7280"},
		"TextSubtle":    {p.TextSubtle, "#9CA3AF"},
		"TextDim":       {p.TextDim, "#374151"},
		"TextFaint":     {p.TextFaint, "#4B5563"},
		"TextOnAccent":  {p.TextOnAccent, "#0D0D0D"},
		"Border":        {p.Border, "#4B5563"},
		"Rule":          {p.Rule, "#374151"},
		"BgHeader":      {p.BgHeader, "#0D0D0D"},
		"BgSelection":   {p.BgSelection, "#1F2937"},
		"BgCode":        {p.BgCode, "#111827"},
		"CodeFg":        {p.CodeFg, "#E5E7EB"},
		"BgCodeInline":  {p.BgCodeInline, "#1F2937"},
		"CodeInlineFg":  {p.CodeInlineFg, "#D1D5DB"},
		"Success":       {p.Success, "#10B981"},
		"SuccessBright": {p.SuccessBright, "#22C55E"},
		"Error":         {p.Error, "#EF4444"},
		"Warning":       {p.Warning, "#F59E0B"},
		"WarningSoft":   {p.WarningSoft, "#FBBF24"},
		"Info":          {p.Info, "#60A5FA"},
		"Danger":        {p.Danger, "#FF5555"},
		"AccentViolet":  {p.AccentViolet, "#A78BFA"},
		"AccentGreen":   {p.AccentGreen, "#34D399"},
		"AccentBlue":    {p.AccentBlue, "#60A5FA"},
		"AccentAmber":   {p.AccentAmber, "#F59E0B"},
		"AccentMagenta": {p.AccentMagenta, "#C084FC"},
		"AccentPurple":  {p.AccentPurple, "#BD93F9"},
		"AccentRed":     {p.AccentRed, "#EF4444"},
		"AccentCyan":    {p.AccentCyan, "#00D7FF"},
		"DiffAddHiFg":   {p.DiffAddHiFg, "#6EE7B7"},
		"DiffAddHiBg":   {p.DiffAddHiBg, "#064E3B"},
		"DiffDelHiFg":   {p.DiffDelHiFg, "#FCA5A5"},
		"DiffDelHiBg":   {p.DiffDelHiBg, "#7F1D1D"},
		"DiffLineNo":    {p.DiffLineNo, "#4B5563"},
		"DiffDivider":   {p.DiffDivider, "#374151"},
		"PlanPlus":      {p.PlanPlus, "#86EFAC"},
		"PlanPro":       {p.PlanPro, "#C4B5FD"},
		"BgBase":        {p.BgBase, "#0B0B0D"},
		"FgHighlight":   {p.FgHighlight, "#FFFFFF"},
		"GlowSpecular":  {p.GlowSpecular, "#FFFFFF"},
	}
	for name, tc := range cases {
		if got := hexOf(tc.got); got != tc.want {
			t.Errorf("Dark().%s = %s, want %s", name, got, tc.want)
		}
	}

	wantGlow := []string{"#7C3AED", "#A855F7", "#E879F9", "#38BDF8", "#22D3EE"}
	assertRamp(t, "RampGlow", p.RampGlow, wantGlow)
	wantRainbow := []string{"#FF6B6B", "#FF9E4F", "#FFD93D", "#6BCB77", "#4D96FF", "#C77DFF"}
	assertRamp(t, "RampRainbow", p.RampRainbow, wantRainbow)
	assertRamp(t, "EyesScan", p.EyesScan[:], []string{"#888888", "#555555", "#2A2A2A"})
	assertRamp(t, "EyesBlink", p.EyesBlink[:], []string{"#6B6B8A", "#3D3D5C", "#1A1A2E"})

	if p.GlareStops != [4]float64{0.88, 0.60, 0.30, 0.12} {
		t.Errorf("Dark().GlareStops = %v, want the pre-theme glare weights", p.GlareStops)
	}
	if p.GlowShine != 0.90 || p.GlowTintBias != 0.16 || p.GlowTintGain != 0.26 {
		t.Errorf("Dark() glow weights = %v/%v/%v, want 0.9/0.16/0.26", p.GlowShine, p.GlowTintBias, p.GlowTintGain)
	}
}

func assertRamp(t *testing.T, name string, got []color.Color, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s has %d anchors, want %d", name, len(got), len(want))
	}
	for i := range want {
		if h := hexOf(got[i]); h != want[i] {
			t.Errorf("%s[%d] = %s, want %s", name, i, h, want[i])
		}
	}
}

// TestLightPaletteInvertsPolarity guards the two roles that must not merely
// change value but change direction: text painted on an accent chip, and the
// anchors every fade interpolates toward.
func TestLightPaletteInvertsPolarity(t *testing.T) {
	dark, light := Dark(), Light()

	if !IsDarkColor(dark.TextOnAccent) {
		t.Error("dark TextOnAccent should be near-black, painted on a bright accent")
	}
	if IsDarkColor(light.TextOnAccent) {
		t.Error("light TextOnAccent should be near-white, painted on a dark accent")
	}
	if IsDarkColor(dark.FgHighlight) || !IsDarkColor(light.FgHighlight) {
		t.Error("FgHighlight must lead away from the page: white on dark, ink on light")
	}
	if !IsDarkColor(dark.BgBase) || IsDarkColor(light.BgBase) {
		t.Error("BgBase must sit just past the terminal's own ground in each theme")
	}
	if !IsDarkColor(light.Text) || IsDarkColor(light.BgHeader) {
		t.Error("light theme must render dark ink on a light header band")
	}
	// Every accent has to survive TextOnAccent painted on top of it, since
	// that is exactly what the active agent tab does. Contrast here is WCAG
	// relative luminance rather than the HSL rule IsDarkColor applies: HSL
	// answers "is this a dark terminal background", which is a different
	// question from "is this ink legible on that chip", and a saturated
	// violet like #6D28D9 lands on opposite sides of the two.
	for _, p := range []Palette{dark, light} {
		accents := map[string]color.Color{
			"AccentViolet":  p.AccentViolet,
			"AccentGreen":   p.AccentGreen,
			"AccentBlue":    p.AccentBlue,
			"AccentAmber":   p.AccentAmber,
			"AccentMagenta": p.AccentMagenta,
			"AccentPurple":  p.AccentPurple,
			"AccentRed":     p.AccentRed,
		}
		for name, c := range accents {
			if got := contrast(p.TextOnAccent, c); got < 4.5 {
				t.Errorf("%s theme: TextOnAccent on %s = %.2f:1, want at least 4.5:1", p.Kind, name, got)
			}
		}
	}
}

// contrast is the WCAG 2.x contrast ratio between two colours.
func contrast(a, b color.Color) float64 {
	la, lb := relLuminance(a), relLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func relLuminance(c color.Color) float64 {
	r, g, b := RGB(c)
	lin := func(v uint8) float64 {
		f := float64(v) / 255
		if f <= 0.03928 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func TestAccentMapping(t *testing.T) {
	p := Dark()
	cases := map[string]color.Color{
		"blue":      p.AccentViolet,
		"green":     p.AccentGreen,
		"cyan":      p.AccentBlue,
		"yellow":    p.AccentAmber,
		"magenta":   p.AccentMagenta,
		"purple":    p.AccentPurple,
		"red":       p.AccentRed,
		"plan":      p.AccentPurple,
		"planning":  p.AccentViolet,
		"coding":    p.AccentGreen,
		"chat":      p.AccentBlue,
		"":          p.AccentBlue,
		"not-a-hue": p.AccentBlue,
	}
	for name, want := range cases {
		if got := p.Accent(name); hexOf(got) != hexOf(want) {
			t.Errorf("Accent(%q) = %s, want %s", name, hexOf(got), hexOf(want))
		}
	}
}

func TestLerp(t *testing.T) {
	base := lipgloss.Color("#000000")
	white := lipgloss.Color("#FFFFFF")

	if got := hexOf(Lerp(base, white, 0)); got != "#000000" {
		t.Errorf("Lerp at t=0 = %s, want the base", got)
	}
	if got := hexOf(Lerp(base, white, 1)); got != "#FFFFFF" {
		t.Errorf("Lerp at t=1 = %s, want the target", got)
	}
	// Truncating, not rounding: this is the arithmetic the glare gradient has
	// always used, and dark output must stay byte-identical.
	if got := hexOf(Lerp(base, white, 0.88)); got != "#E0E0E0" {
		t.Errorf("Lerp(black, white, 0.88) = %s, want #E0E0E0", got)
	}
	// Out-of-range weights clamp rather than overflow the channel.
	if got := hexOf(Lerp(base, white, 2)); got != "#FFFFFF" {
		t.Errorf("Lerp at t=2 = %s, want the target", got)
	}
	if got := hexOf(Lerp(base, white, -1)); got != "#000000" {
		t.Errorf("Lerp at t=-1 = %s, want the base", got)
	}
}

func hexOf(c color.Color) string {
	r, g, b := RGB(c)
	const digits = "0123456789ABCDEF"
	out := []byte("#000000")
	for i, v := range []uint8{r, g, b} {
		out[1+i*2] = digits[v>>4]
		out[2+i*2] = digits[v&0xF]
	}
	return string(out)
}

// TestLightTextRolesClearContrastFloors checks the roles whose whole reason to
// exist is that a dark-theme value would be illegible on white. Text roles are
// held to WCAG AA (4.5:1) and purely structural ones to the 3:1 decorative
// floor, both measured against #F5F5F5 rather than pure white — an off-white
// terminal has less headroom, so it is the harder of the two grounds the brief
// names.
func TestLightTextRolesClearContrastFloors(t *testing.T) {
	p := Light()
	ground := hex("#F5F5F5")
	cases := []struct {
		name  string
		c     color.Color
		floor float64
	}{
		{"Text", p.Text, 4.5},
		{"TextMuted", p.TextMuted, 4.5},
		{"TextSubtle", p.TextSubtle, 4.5},
		// TextFaint is italic tool output: receded, but still read.
		{"TextFaint", p.TextFaint, 4.5},
		// TextDim carries the thinking-block transcript, mined memory facts
		// and session previews. It is the faintest prose in the UI, not
		// decoration, so it is held to AA like the rest.
		{"TextDim", p.TextDim, 4.5},
		// Rule is only ever drawn: separators, fills, empty progress cells.
		{"Rule", p.Rule, 3},
		{"Border", p.Border, 3},
		// The diff gutter and column rule are glanced at, not read.
		{"DiffLineNo", p.DiffLineNo, 3},
		{"DiffDivider", p.DiffDivider, 3},
		{"AccentCyan", p.AccentCyan, 4.5},
	}
	for _, tc := range cases {
		if got := contrast(tc.c, ground); got < tc.floor {
			t.Errorf("Light().%s = %.2f:1 on #F5F5F5, want at least %.1f:1", tc.name, got, tc.floor)
		}
	}
}

// TestLightNeutralsStayOrdered checks the recede order rather than the values.
// Each of these roles is a step further back than the last, and the light theme
// inverts the direction — "further back" becomes lighter, not darker — so a
// value ported across without re-tuning shows up here as an inversion long
// before anyone notices it on screen.
func TestLightNeutralsStayOrdered(t *testing.T) {
	p := Light()
	steps := []struct {
		name string
		c    color.Color
	}{
		{"Text", p.Text},
		{"TextMuted", p.TextMuted},
		{"TextFaint", p.TextFaint},
		{"TextDim", p.TextDim},
		{"Border", p.Border},
		{"Rule", p.Rule},
	}
	for i := 1; i < len(steps); i++ {
		prev, cur := contrast(steps[i-1].c, hex("#F5F5F5")), contrast(steps[i].c, hex("#F5F5F5"))
		if cur >= prev {
			t.Errorf("Light().%s (%.2f:1) does not recede behind %s (%.2f:1)",
				steps[i].name, cur, steps[i-1].name, prev)
		}
	}
	// The diff's own ramp: context text sits forward of the gutter, which
	// sits forward of the column rule.
	diff := []struct {
		name string
		c    color.Color
	}{
		{"TextSubtle", p.TextSubtle},
		{"DiffLineNo", p.DiffLineNo},
		{"DiffDivider", p.DiffDivider},
	}
	for i := 1; i < len(diff); i++ {
		prev, cur := contrast(diff[i-1].c, hex("#F5F5F5")), contrast(diff[i].c, hex("#F5F5F5"))
		if cur >= prev {
			t.Errorf("Light().%s (%.2f:1) does not recede behind %s (%.2f:1)",
				diff[i].name, cur, diff[i-1].name, prev)
		}
	}
	// AccentCyan exists only to stay distinguishable from AccentBlue in the
	// legacy renderer; if they converge the role has no purpose.
	if hexOf(p.AccentCyan) == hexOf(p.AccentBlue) {
		t.Error("Light() AccentCyan and AccentBlue collapsed onto one value")
	}
}

// TestRuleSharesTextDimInDark pins the split that lets TextDim be darkened for
// the light theme: the two roles are one colour in dark, so moving a call site
// between them cannot change a byte of dark output.
func TestRuleSharesTextDimInDark(t *testing.T) {
	if d := Dark(); hexOf(d.Rule) != hexOf(d.TextDim) {
		t.Errorf("Dark().Rule = %s, want TextDim's %s", hexOf(d.Rule), hexOf(d.TextDim))
	}
	if l := Light(); hexOf(l.Rule) == hexOf(l.TextDim) {
		t.Error("Light().Rule and TextDim collapsed onto one value, which is the collision the split exists to break")
	}
}

// TestLightPaintedBandsStayApart covers the two surfaces that abut: the
// command overlay's selected row sits directly on the status band, and
// BgSelection is the only mark on the current row in every list and dialog —
// none of them draw a cursor glyph. If the two bands converge, or the
// selection stops standing off the page, the user loses track of what enter
// will select.
func TestLightPaintedBandsStayApart(t *testing.T) {
	for _, p := range []Palette{Dark(), Light()} {
		// away is +1 in the direction the theme's painted surfaces run: down
		// from a white page, up from a black one.
		page, away := hex("#FFFFFF"), 1.0
		if p.IsDark() {
			page, away = hex("#000000"), -1.0
		}
		selStep := away * (lstar(page) - lstar(p.BgSelection))
		hdrStep := away * (lstar(page) - lstar(p.BgHeader))
		if selStep < 8 {
			t.Errorf("%s theme: BgSelection is %.1f L* off the page, too faint to read as a selection", p.Kind, selStep)
		}
		if hdrStep < 3 {
			t.Errorf("%s theme: BgHeader is %.1f L* off the page, too faint to read as a band", p.Kind, hdrStep)
		}
		if gap := away * (lstar(p.BgHeader) - lstar(p.BgSelection)); gap < 4 {
			t.Errorf("%s theme: BgSelection and BgHeader are %.2f L* apart; a selected row on the status band would merge into it", p.Kind, gap)
		}
		// Every list paints Text on the selected row.
		if got := contrast(p.Text, p.BgSelection); got < 4.5 {
			t.Errorf("%s theme: Text on BgSelection = %.2f:1, want at least 4.5:1", p.Kind, got)
		}
	}
}

// TestLightEyeRampsMatchDarkStepForStep pins the claim EyesBlink's comment
// makes. The eyes are on screen in every idle and thinking frame, so a light
// ramp that fades faster than its dark counterpart is a light-only regression
// in the app's most-seen animation.
func TestLightEyeRampsMatchDarkStepForStep(t *testing.T) {
	d, l := Dark(), Light()
	black, white := hex("#000000"), hex("#FFFFFF")
	for _, ramp := range []struct {
		name string
		d, l [3]color.Color
	}{
		{"EyesScan", d.EyesScan, l.EyesScan},
		{"EyesBlink", d.EyesBlink, l.EyesBlink},
	} {
		for i := range ramp.d {
			want, got := contrast(ramp.d[i], black), contrast(ramp.l[i], white)
			// A tenth of a ratio is below anything an eye resolves; the point
			// is to catch a step that fades half as far, not to pin a hex.
			if diff := want - got; diff > 0.15 || diff < -0.15 {
				t.Errorf("Light().%s[%d] = %.2f:1 on white, but Dark()'s is %.2f:1 on black", ramp.name, i, got, want)
			}
		}
	}
}

// TestSeed covers the guard Resolve cannot apply for itself: COLORFGBG is
// inherited, so on a terminal that cannot be asked for its background the
// variable describes whatever launched us. Every caller that installs a
// palette without a detected background goes through Seed, so startup and a
// runtime /theme cannot disagree about the same inputs.
func TestSeed(t *testing.T) {
	// Under `go test` stdout is a pipe, so CanQueryTerminal is false.
	if CanQueryTerminal() {
		t.Skip("test binary unexpectedly attached to a terminal")
	}
	t.Setenv("COLORFGBG", "0;15")
	if got := Resolve(AutoKind, nil); got != LightKind {
		t.Fatalf("Resolve(auto) = %q with COLORFGBG=0;15, want light; the assertion below would prove nothing", got)
	}
	if got := Seed(AutoKind); got != DarkKind {
		t.Errorf("Seed(auto) = %q on an unqueryable terminal, want the dark fallback rather than an inherited COLORFGBG", got)
	}
	// An explicit selection is never second-guessed, guard or no guard.
	for _, k := range []Kind{DarkKind, LightKind} {
		if got := Seed(k); got != k {
			t.Errorf("Seed(%q) = %q, want it untouched", k, got)
		}
	}
}

// lstar is CIE L*, the perceptual lightness the painted-band steps above are
// measured in. Contrast ratio answers "is this ink legible"; two large flat
// surfaces a user has to tell apart is a different question, and WCAG ratios
// compress badly at the pale end where the light theme's bands live.
func lstar(c color.Color) float64 {
	y := relLuminance(c)
	if y > 216.0/24389.0 {
		return 116*math.Cbrt(y) - 16
	}
	return y * 24389 / 27
}
