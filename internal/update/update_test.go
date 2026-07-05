package update

import "testing"

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v1.2.3", "v1.2.4", true},
		{"v1.2.3", "v1.3.0", true},
		{"v1.2.3", "v2.0.0", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.4", "v1.2.3", false},
		{"v2.4", "v2.4.0", false},  // missing patch treated as 0
		{"v2.4.0", "v2.4.1", true}, // patch-only bump
		{"1.2.3", "1.2.4", true},   // no "v" prefix on either side
		{"v1.2.3", "v1.3.0-rc1", true},
		{"dev", "v1.0.0", false}, // from-source build never flagged
		{"v1.0.0", "not-a-version", false},
	}
	for _, tc := range cases {
		if got := IsNewer(tc.current, tc.latest); got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}
