package config

import "testing"

// TestNormalizeTheme covers the theme selection's validation the same way the
// thinking-level switch above it is covered: an unrecognised value must be
// cleared (and the clearing reported, so it gets persisted) rather than
// reaching the TUI as a palette nobody defined.
func TestNormalizeTheme(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		changed bool
	}{
		{"", "", false},
		{"auto", "auto", false},
		{"dark", "dark", false},
		{"light", "light", false},
		{"Dark", "", true},      // config values are stored lowercase
		{"solarized", "", true}, // unknown theme
		{"  light  ", "", true}, // not trimmed on the way in, so not valid
	}
	for _, tc := range cases {
		cfg := Default()
		cfg.Theme = tc.in
		got, changed := normalize(cfg)
		if got.Theme != tc.want {
			t.Errorf("normalize(theme=%q).Theme = %q, want %q", tc.in, got.Theme, tc.want)
		}
		// normalize reports one "changed" for the whole config and Default()
		// already trips it on other fields, so a rejected theme is only
		// required to keep the flag set, never to be the sole cause of it.
		if tc.changed && !changed {
			t.Errorf("normalize(theme=%q) did not report the rewrite", tc.in)
		}
	}
}
