package tui

import (
	"fmt"
	"sort"
	"strings"

	"spettro/internal/provider"
)

// defaultInputPricePerMTok is the coarse input price (USD per million tokens)
// used for the "estimated cost saved" line when the model's real price is
// unknown. Cache reads bill at ~10% of the input rate and cache writes at
// ~125%, so savings ≈ 0.9·read − 0.25·write, valued at this rate.
const defaultInputPricePerMTok = 3.0

func cacheSavedUSD(u provider.Usage) float64 {
	saved := 0.9*float64(u.CacheReadTokens) - 0.25*float64(u.CacheWriteTokens)
	return saved * defaultInputPricePerMTok / 1_000_000
}

func formatHitRate(u provider.Usage) string {
	r := u.CacheHitRate()
	if r < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", r*100)
}

// hitRateBar renders rate (0..1) as a fixed-width block bar. Negative rates
// (usage unavailable) render as an empty track.
func hitRateBar(rate float64, width int) string {
	if rate < 0 {
		rate = 0
	}
	filled := int(rate*float64(width) + 0.5)
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// renderStats builds the /stats report from the provider manager's
// accumulated session usage.
func (m Model) renderStats() string {
	s := m.providers.UsageSnapshot()
	if s.Totals.Requests == 0 {
		return "no LLM requests recorded in this session yet"
	}
	t := s.Totals

	var b strings.Builder
	fmt.Fprintf(&b, "session usage — %s\n\n", plural(t.Requests, "request"))

	fmt.Fprintf(&b, "  input        %7s\n", formatTokenCount(t.InputTokens))
	fmt.Fprintf(&b, "  output       %7s\n", formatTokenCount(t.OutputTokens))
	fmt.Fprintf(&b, "  cache read   %7s\n", formatTokenCount(t.CacheReadTokens))
	fmt.Fprintf(&b, "  cache write  %7s\n\n", formatTokenCount(t.CacheWriteTokens))

	const barW = 20
	fmt.Fprintf(&b, "  cache hits   %5s  %s  session\n", formatHitRate(t.Usage), hitRateBar(t.CacheHitRate(), barW))
	fmt.Fprintf(&b, "               %5s  %s  last request\n", formatHitRate(s.Last), hitRateBar(s.Last.CacheHitRate(), barW))
	if saved := cacheSavedUSD(t.Usage); saved > 0 {
		fmt.Fprintf(&b, "  saved       ~$%.2f  (est., cache reads bill at ~10%% of input)\n", saved)
	}

	if len(s.ByModel) > 0 {
		keys := make([]string, 0, len(s.ByModel))
		w := 0
		for k := range s.ByModel {
			keys = append(keys, k)
			if len(k) > w {
				w = len(k)
			}
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "\n  by model\n")
		for _, k := range keys {
			u := s.ByModel[k]
			fmt.Fprintf(&b, "  %-*s  %s · %s hit · in %s · out %s\n",
				w, k, plural(u.Requests, "req"), formatHitRate(u.Usage),
				formatTokenCount(u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens),
				formatTokenCount(u.OutputTokens))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// cacheIndicator is the compact status-bar cue: cache-hit % of the LAST
// request, so a broken prompt prefix is visible immediately mid-session.
// Empty when no request has reported usage yet.
func (m Model) cacheIndicator() (label string, healthy bool) {
	last := m.providers.UsageSnapshot().Last
	r := last.CacheHitRate()
	if r < 0 {
		return "", false
	}
	return fmt.Sprintf("cache %.0f%%", r*100), r >= 0.5
}
