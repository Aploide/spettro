// Package agents embeds the built-in agent prompt pack (the *.md files in this
// directory) into the binary, so every agent gets its full system prompt no
// matter which directory Spettro is started in. A project can still override a
// prompt with .spettro/<prompt_file>, or with the plain prompt_file path when it
// ships its own spettro.agents.toml (see loadPromptOrFallback).
package agents

import (
	"embed"
	"path"
	"strings"
)

//go:embed *.md
var files embed.FS

// Prompt returns the embedded prompt for a manifest prompt_file such as
// "agents/coding.md" (a bare "coding.md" is accepted too). Only paths inside
// the agents/ pack resolve; anything else reports false.
func Prompt(promptFile string) (string, bool) {
	p := path.Clean(strings.ReplaceAll(strings.TrimSpace(promptFile), `\`, "/"))
	p = strings.TrimPrefix(p, "./")
	if dir, name := path.Split(p); dir == "agents/" {
		p = name
	}
	if p == "" || strings.Contains(p, "/") || !strings.HasSuffix(p, ".md") {
		return "", false
	}
	data, err := files.ReadFile(p)
	if err != nil {
		return "", false
	}
	return string(data), true
}
