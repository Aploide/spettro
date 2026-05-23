package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"spettro/internal/budget"
	"spettro/internal/provider"
)

// coAuthor is the canonical commit trailer Spettro stamps onto every commit
// it writes. The exact string is also auto-injected by EnforceCommitCoAuthor
// when an LLM agent issues `git commit` through shell-exec/bash, so keep both
// callers in sync via the single shared spettroCoAuthorTrailer constant.
const coAuthor = spettroCoAuthorTrailer

const commitSystemPrompt = `You are a git commit message writer.
Given a git diff, write a concise conventional commit message.
Format: type(scope): short description (max 72 chars)
Valid types: feat, fix, refactor, chore, docs, test, style, perf, ci
Use imperative mood ("add" not "added").
Output ONLY the commit message subject line — no markdown, no explanation, no quotes.`

// CommitAgent generates a commit message via the LLM and commits the changes.
type CommitAgent interface {
	Commit(ctx context.Context, cwd string) (string, error)
}

// LLMCommitter uses the active provider to write the commit message.
type LLMCommitter struct {
	ProviderManager *provider.Manager
	ProviderName    func() string
	ModelName       func() string
}

func (c LLMCommitter) Commit(ctx context.Context, cwd string) (string, error) {
	statusOut, err := gitCmd(cwd, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(statusOut) == "" {
		return "", fmt.Errorf("nothing to commit")
	}

	// Prefer unstaged diff, then staged, then fall back to status.
	diffOut, _ := gitCmd(cwd, "diff", "HEAD")
	if strings.TrimSpace(diffOut) == "" {
		diffOut, _ = gitCmd(cwd, "diff", "--cached")
	}
	if strings.TrimSpace(diffOut) == "" {
		diffOut = statusOut
	}
	if len(diffOut) > 8000 {
		diffOut = diffOut[:8000] + "\n... (truncated)"
	}

	prompt := commitSystemPrompt + "\n\n" + diffOut
	if err := budget.Validate(0, prompt); err != nil {
		return "", fmt.Errorf("diff too large: %w", err)
	}

	resp, err := c.ProviderManager.Send(ctx, c.ProviderName(), c.ModelName(), provider.Request{
		Prompt: prompt,
	})
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}

	msg := strings.TrimSpace(resp.Content)
	msg = strings.Trim(msg, "`")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "", fmt.Errorf("LLM returned empty commit message")
	}

	if _, err := gitCmd(cwd, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}

	if out, err := gitCmd(cwd, "commit", "-m", msg, "--trailer", coAuthor); err != nil {
		return "", fmt.Errorf("git commit: %s: %w", strings.TrimSpace(out), err)
	}

	return msg, nil
}

func gitCmd(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	return string(out), err
}
