package tui

// The gitignore matching logic moved to internal/ignore so the repo symbol
// indexer can share it; these aliases keep the TUI call sites unchanged.

import "spettro/internal/ignore"

type gitignoreMatcher = ignore.Matcher

func newGitignoreMatcher(root string) *gitignoreMatcher {
	return ignore.NewMatcher(root)
}
