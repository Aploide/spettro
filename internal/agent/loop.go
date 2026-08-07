package agent

import (
	"fmt"
	"strings"
	"time"
)

// MinLoopInterval bounds /loop schedules so a typo cannot hammer the provider
// with back-to-back runs.
const MinLoopInterval = 10 * time.Second

// ParseLoopInterval parses the <time> argument of /loop <time> <prompt>.
// Accepted forms are Go durations with units: "30s", "5m", "1h", "1h30m".
//
// Shared between the TUI and ACP loop runners.
func ParseLoopInterval(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("missing interval — use forms like 30s, 5m, 1h30m")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid interval %q — use forms like 30s, 5m, 1h30m", s)
	}
	if d < MinLoopInterval {
		return 0, fmt.Errorf("interval %s is below the minimum %s", d, MinLoopInterval)
	}
	return d, nil
}

// SplitLoopArgs splits the text after "/loop" into the interval token and the
// prompt to repeat. ok is false when either part is missing.
func SplitLoopArgs(rest string) (interval, prompt string, ok bool) {
	interval, prompt, _ = strings.Cut(strings.TrimSpace(rest), " ")
	prompt = strings.TrimSpace(prompt)
	return interval, prompt, interval != "" && prompt != ""
}
