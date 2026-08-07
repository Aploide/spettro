package agent

import (
	"testing"
	"time"
)

func TestParseLoopInterval(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"30s", 30 * time.Second, false},
		{"5m", 5 * time.Minute, false},
		{"1h", time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{" 10S ", 10 * time.Second, false}, // trimmed and case-folded
		{"", 0, true},                      // missing
		{"5", 0, true},                     // bare number, no unit
		{"abc", 0, true},                   // not a duration
		{"5s", 0, true},                    // below MinLoopInterval
		{"-5m", 0, true},                   // negative
	}
	for _, tc := range cases {
		got, err := ParseLoopInterval(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseLoopInterval(%q): expected error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseLoopInterval(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseLoopInterval(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSplitLoopArgs(t *testing.T) {
	if iv, p, ok := SplitLoopArgs("5m check CI status"); !ok || iv != "5m" || p != "check CI status" {
		t.Fatalf("SplitLoopArgs full form = (%q, %q, %v)", iv, p, ok)
	}
	if _, _, ok := SplitLoopArgs("5m"); ok {
		t.Fatal("interval without prompt must not be ok")
	}
	if _, _, ok := SplitLoopArgs("   "); ok {
		t.Fatal("blank input must not be ok")
	}
	if iv, p, ok := SplitLoopArgs("  30s   run the tests  "); !ok || iv != "30s" || p != "run the tests" {
		t.Fatalf("SplitLoopArgs trimming = (%q, %q, %v)", iv, p, ok)
	}
}
