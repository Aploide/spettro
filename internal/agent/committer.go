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

const commitSystemPrompt = `You are a git commit message writer for the Spettro project.
Given a git diff, write a single Conventional Commits subject line.

Format: <type>(<scope>): <imperative summary>
- <type> is exactly one of: feat, fix, perf, refactor, docs, test, chore, ci, build, style, revert.
- <scope> is the most specific subsystem touched. For this repo, prefer:
  agent, tui, provider, config, telegram, remote, mcp, skills, session, hooks, agents, cli.
  When the diff genuinely spans subsystems, omit the scope (just "<type>: ...").
- <summary> is imperative ("add", "fix", "remove", "wire"), lowercase after the type
  prefix, no trailing period, ≤72 chars total including the type/scope prefix
  (~50 chars is ideal).

Pick the type from the actual change:
- feat: new user-visible capability
- fix: bug fix or correctness regression
- perf: same behavior, measurably faster/lighter
- refactor: code restructure with no behavior change
- docs/test/chore/ci/build/style/revert: per the matching conventional meaning

Output ONLY the subject line. No markdown, no explanation, no quotes,
no body, no trailers (Spettro's runtime adds the Co-Authored-By trailer
automatically when committing).`

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
