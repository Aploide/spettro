package provider_test

import (
	"testing"

	"spettro/internal/provider"
)

func TestThinkingBudgetTokens_Mapping(t *testing.T) {
	cases := []struct {
		level provider.ThinkingLevel
		want  int
	}{
		{provider.ThinkingOff, 0},
		{"", 0},
		{provider.ThinkingLow, 2048},
		{provider.ThinkingMedium, 5120},
		{provider.ThinkingHigh, 16384},
		{provider.ThinkingXHigh, 32768},
		{"unknown", 0},
	}
	for _, c := range cases {
		if got := provider.ThinkingBudgetTokens(c.level); got != c.want {
			t.Errorf("ThinkingBudgetTokens(%q) = %d, want %d", c.level, got, c.want)
		}
	}
}

func TestIsValidThinkingLevel(t *testing.T) {
	valid := []string{"", "off", "low", "medium", "high", "x-high"}
	for _, v := range valid {
		if !provider.IsValidThinkingLevel(v) {
			t.Errorf("IsValidThinkingLevel(%q) = false, want true", v)
		}
	}
	invalid := []string{"OFF", "x-large", "extreme", "  high", "high  "}
	for _, v := range invalid {
		if provider.IsValidThinkingLevel(v) {
			t.Errorf("IsValidThinkingLevel(%q) = true, want false", v)
		}
	}
}
