// Package budget holds the chars/4 token heuristic and the token budget
// checks built on it.
package budget

import (
	"fmt"
	"unicode/utf8"
)

// EstimateTokens approximates the tokens of parts: their total rune count
// divided by four, plus one for any non-empty text (0 for no text at all).
func EstimateTokens(parts ...string) int {
	totalChars := 0
	for _, p := range parts {
		// The same count as len([]rune(p)), invalid bytes included, written
		// so it never depends on the compiler eliding the rune slice.
		totalChars += utf8.RuneCountInString(p)
	}

	if totalChars == 0 {
		return 0
	}

	return (totalChars / 4) + 1
}

// Validate returns an error if the combined token estimate of parts exceeds
// maxTokens. Pass 0 (or a negative number) to disable budgeting.
func Validate(maxTokens int, parts ...string) error {
	if maxTokens <= 0 {
		return nil
	}
	estimated := EstimateTokens(parts...)
	if estimated >= maxTokens {
		return fmt.Errorf("token budget exceeded: estimated=%d max=%d", estimated, maxTokens)
	}
	return nil
}

// CheckTokens returns an error if an already-computed token estimate is at or
// above limit. Pass 0 (or a negative number) to disable the check.
func CheckTokens(limit, estimated int) error {
	if limit <= 0 {
		return nil
	}
	if estimated >= limit {
		return fmt.Errorf("token budget exceeded: estimated=%d max=%d", estimated, limit)
	}
	return nil
}
