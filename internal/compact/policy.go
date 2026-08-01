package compact

import "math"

const (
	defaultAutoThresholdPct = 85
	defaultMaxFailures      = 3
	warningBufferTokens     = 20000
	errorBufferTokens       = 12000
	autoBufferTokens        = 13000
	blockingBufferTokens    = 3000
	reservedOutputTokens    = 20000
	// keepRecentTokens is how much of the conversation tail stays verbatim
	// after a compaction (Pi-style). Older turns are summarized.
	defaultKeepRecentTokens = 20000
	// keepRecentTokensForce is the smaller tail kept on explicit /compact or
	// budget-recovery force so a mid-sized history still shrinks.
	defaultKeepRecentTokensForce = 8000
)

type Config struct {
	AutoEnabled      bool
	AutoThresholdPct int
	MaxFailures      int
}

type State struct {
	TokensUsed          int
	ConsecutiveFailures int
}

type Evaluation struct {
	EffectiveWindow      int
	WarningThreshold     int
	ErrorThreshold       int
	AutoCompactThreshold int
	BlockingLimit        int
	IsWarning            bool
	IsError              bool
	ShouldAutoCompact    bool
	IsBlocking           bool
	AutoDisabledReason   string
}

func Evaluate(contextWindow int, cfg Config, state State) Evaluation {
	effective := EffectiveContextWindow(contextWindow)
	if effective <= 0 {
		effective = 1
	}
	warn := clampThreshold(effective-warningBufferTokens, effective)
	err := clampThreshold(effective-errorBufferTokens, effective)
	autoCompactThreshold := autoThreshold(effective, cfg.AutoThresholdPct)
	blocking := clampThreshold(effective-blockingBufferTokens, effective)

	maxFailures := cfg.MaxFailures
	if maxFailures <= 0 {
		maxFailures = defaultMaxFailures
	}

	autoEnabled := cfg.AutoEnabled
	autoDisabledReason := ""
	if !autoEnabled {
		autoDisabledReason = "auto compact disabled"
	} else if state.ConsecutiveFailures >= maxFailures {
		autoEnabled = false
		autoDisabledReason = "auto compact paused after repeated failures"
	}

	return Evaluation{
		EffectiveWindow:      effective,
		WarningThreshold:     warn,
		ErrorThreshold:       err,
		AutoCompactThreshold: autoCompactThreshold,
		BlockingLimit:        blocking,
		IsWarning:            state.TokensUsed >= warn,
		IsError:              state.TokensUsed >= err,
		ShouldAutoCompact:    autoEnabled && state.TokensUsed >= autoCompactThreshold,
		IsBlocking:           state.TokensUsed >= blocking,
		AutoDisabledReason:   autoDisabledReason,
	}
}

func EffectiveContextWindow(contextWindow int) int {
	if contextWindow <= 0 {
		return 0
	}
	reserved := minInt(reservedOutputTokens, maxInt(contextWindow/2, 1))
	effective := contextWindow - reserved
	if effective <= 0 {
		return contextWindow
	}
	return effective
}

func autoThreshold(effective, pct int) int {
	if pct <= 0 {
		pct = defaultAutoThresholdPct
	}
	if pct > 99 {
		pct = 99
	}
	pctThreshold := int(math.Floor(float64(effective) * (float64(pct) / 100.0)))
	bufThreshold := effective - autoBufferTokens
	if bufThreshold <= 0 {
		bufThreshold = pctThreshold
	}
	if pctThreshold <= 0 {
		pctThreshold = bufThreshold
	}
	if pctThreshold <= 0 {
		pctThreshold = effective
	}
	if bufThreshold <= 0 || pctThreshold < bufThreshold {
		return clampThreshold(pctThreshold, effective)
	}
	return clampThreshold(bufThreshold, effective)
}

func clampThreshold(v, effective int) int {
	if v < 1 {
		return 1
	}
	if v > effective {
		return effective
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
