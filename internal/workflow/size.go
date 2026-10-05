package workflow

import "strings"

// Size is a run's size guideline: how many agents a workflow of this tier is
// expected to start, and how wide one fan-out should be.
//
// It is a guideline, not a limit. A model writing a script has no feel for
// what "a reasonable number of agents" costs, and left alone it either runs
// three where thirty were warranted or two hundred where ten would do. The
// tier gives it a number to aim at — exposed to the script as the `size`
// global — while the hard runaway cap stays Options.MaxAgents.
type Size struct {
	// Tier is the tier's name: small, medium, large or unbounded.
	Tier string `json:"tier"`
	// Agents is the guideline for the total number of agents in one run.
	// 0 means no guideline (the script sees Infinity).
	Agents int `json:"agents"`
	// Fanout is the suggested maximum width of a single fan-out — the cap
	// plan() applies to its work-list unless told otherwise.
	Fanout int `json:"fanout"`
	// Concurrency is the default MaxConcurrency for the tier. 0 keeps the
	// engine's own default (min(16, NumCPU-2)).
	Concurrency int `json:"concurrency,omitempty"`
}

// Size tier names.
const (
	SizeSmall     = "small"
	SizeMedium    = "medium"
	SizeLarge     = "large"
	SizeUnbounded = "unbounded"
)

// SizeTierNames lists the tiers smallest first, for hosts that offer a choice.
var SizeTierNames = []string{SizeSmall, SizeMedium, SizeLarge, SizeUnbounded}

// SizeTiers is the single source of truth for what each tier means. Hosts
// (prompt guidance, the TUI's /workflows size, ACP's config option) read it
// rather than restating the numbers, so a retune happens in one place.
//
// small also lowers the default concurrency: a small run is usually a quick
// look the user is waiting on, and four agents at once is plenty for five.
var SizeTiers = map[string]Size{
	SizeSmall:     {Tier: SizeSmall, Agents: 5, Fanout: 3, Concurrency: 4},
	SizeMedium:    {Tier: SizeMedium, Agents: 10, Fanout: 6},
	SizeLarge:     {Tier: SizeLarge, Agents: 30, Fanout: 16},
	SizeUnbounded: {Tier: SizeUnbounded, Agents: 0, Fanout: 64},
}

// ResolveSize returns the named tier. An empty or unknown name resolves to
// medium, the default: a typo in a config file must not silently remove the
// guideline (unbounded) or starve the run (small).
func ResolveSize(tier string) Size {
	if s, ok := SizeTiers[strings.ToLower(strings.TrimSpace(tier))]; ok {
		return s
	}
	return SizeTiers[SizeMedium]
}
