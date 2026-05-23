package agent_test

import (
	"strings"
	"testing"

	"spettro/internal/agent"
)

func TestEnforceCommitCoAuthor_InjectsOnSimpleCommit(t *testing.T) {
	cmd := `git commit -m "feat: add foo"`
	got := agent.EnforceCommitCoAuthorForTesting(cmd)
	if !strings.Contains(got, "--trailer 'Co-Authored-By: Spettro <spettro@eyed.to>'") {
		t.Fatalf("expected trailer to be injected, got: %q", got)
	}
	if !strings.HasPrefix(got, `git commit -m "feat: add foo"`) {
		t.Fatalf("expected original command preserved, got: %q", got)
	}
}

func TestEnforceCommitCoAuthor_HandlesAmShortFlag(t *testing.T) {
	cmd := `git commit -am 'wip'`
	got := agent.EnforceCommitCoAuthorForTesting(cmd)
	if !strings.Contains(got, agent.SpettroCoAuthorTrailerForTesting()) {
		t.Fatalf("expected trailer for -am, got: %q", got)
	}
}

func TestEnforceCommitCoAuthor_HandlesFileForm(t *testing.T) {
	cmd := `git commit -F /tmp/msg`
	got := agent.EnforceCommitCoAuthorForTesting(cmd)
	if !strings.Contains(got, agent.SpettroCoAuthorTrailerForTesting()) {
		t.Fatalf("expected trailer for -F form, got: %q", got)
	}
}

func TestEnforceCommitCoAuthor_IdempotentWhenAlreadyPresent(t *testing.T) {
	cmd := `git commit -m "fix: x" --trailer 'Co-Authored-By: Spettro <spettro@eyed.to>'`
	got := agent.EnforceCommitCoAuthorForTesting(cmd)
	if got != cmd {
		t.Fatalf("expected idempotent rewrite, got: %q (orig %q)", got, cmd)
	}
	if strings.Count(got, "Co-Authored-By: Spettro") != 1 {
		t.Fatalf("expected exactly one trailer copy, got %d in %q", strings.Count(got, "Co-Authored-By: Spettro"), got)
	}
}

func TestEnforceCommitCoAuthor_IdempotentWhenTrailerInMessage(t *testing.T) {
	cmd := `git commit -m "fix: x" -m "Co-Authored-By: Spettro <spettro@eyed.to>"`
	got := agent.EnforceCommitCoAuthorForTesting(cmd)
	if got != cmd {
		t.Fatalf("expected idempotent rewrite when trailer is in body, got: %q", got)
	}
}

func TestEnforceCommitCoAuthor_OnlyTouchesCommitSegment(t *testing.T) {
	cmd := `git add . && git commit -m "x" && git push`
	got := agent.EnforceCommitCoAuthorForTesting(cmd)
	if !strings.Contains(got, `git commit -m "x" --trailer '`+agent.SpettroCoAuthorTrailerForTesting()+`'`) {
		t.Fatalf("expected trailer attached only to commit segment, got: %q", got)
	}
	if !strings.Contains(got, "git add .") {
		t.Fatalf("expected git add segment preserved: %q", got)
	}
	if strings.Contains(got, `git add . --trailer`) {
		t.Fatalf("trailer must not be appended to non-commit segment: %q", got)
	}
}

func TestEnforceCommitCoAuthor_SkipsCommitTreeAndCommitGraph(t *testing.T) {
	for _, cmd := range []string{
		`git commit-tree HEAD`,
		`git commit-graph write`,
	} {
		got := agent.EnforceCommitCoAuthorForTesting(cmd)
		if got != cmd {
			t.Fatalf("plumbing command must NOT be rewritten: input=%q output=%q", cmd, got)
		}
	}
}

func TestEnforceCommitCoAuthor_HandlesGlobalOptions(t *testing.T) {
	cases := []string{
		`git -C /tmp/repo commit -m "x"`,
		`git --git-dir=/tmp/repo/.git --work-tree=/tmp/repo commit -m "x"`,
		`git -c user.name=foo commit -m "x"`,
	}
	for _, cmd := range cases {
		got := agent.EnforceCommitCoAuthorForTesting(cmd)
		if !strings.Contains(got, agent.SpettroCoAuthorTrailerForTesting()) {
			t.Fatalf("expected trailer for %q, got: %q", cmd, got)
		}
	}
}

func TestEnforceCommitCoAuthor_RespectsQuotedCommitWord(t *testing.T) {
	// "git commit" embedded in a quoted echo arg must NOT trigger rewrite.
	cmd := `echo "git commit -m hello"`
	got := agent.EnforceCommitCoAuthorForTesting(cmd)
	if got != cmd {
		t.Fatalf("quoted git commit text must not be rewritten: %q -> %q", cmd, got)
	}
}

func TestEnforceCommitCoAuthor_RespectsSubshell(t *testing.T) {
	// `git commit` inside $(...) is opaque — we conservatively skip it.
	cmd := `echo $(git status); git commit -m "x"`
	got := agent.EnforceCommitCoAuthorForTesting(cmd)
	if !strings.Contains(got, `git commit -m "x" --trailer '`+agent.SpettroCoAuthorTrailerForTesting()+`'`) {
		t.Fatalf("expected trailer on outer commit: %q", got)
	}
}

func TestEnforceCommitCoAuthor_NoCommitNoChange(t *testing.T) {
	cmd := `git status --porcelain && git diff HEAD`
	got := agent.EnforceCommitCoAuthorForTesting(cmd)
	if got != cmd {
		t.Fatalf("non-commit pipeline must be unchanged: %q -> %q", cmd, got)
	}
}

func TestEnforceCommitCoAuthor_EmptyString(t *testing.T) {
	if got := agent.EnforceCommitCoAuthorForTesting(""); got != "" {
		t.Fatalf("expected empty passthrough, got %q", got)
	}
}

func TestIsGitCommitInvocation(t *testing.T) {
	yes := []string{
		`git commit`,
		`git commit -m 'x'`,
		`git  commit  --amend`,
		`/usr/bin/git commit -m x`,
		`git -C dir commit -m x`,
		`git -c key=val commit`,
	}
	for _, c := range yes {
		if !agent.IsGitCommitInvocationForTesting(c) {
			t.Errorf("expected %q to be a git commit invocation", c)
		}
	}
	no := []string{
		`git status`,
		`git commit-tree`,
		`git commit-graph write`,
		`gitcommit -m x`,
		`echo git commit`,
		``,
	}
	for _, c := range no {
		if agent.IsGitCommitInvocationForTesting(c) {
			t.Errorf("expected %q NOT to match", c)
		}
	}
}

func TestLexShellTokens_HandlesQuotesAndEscapes(t *testing.T) {
	got := agent.LexShellTokensForTesting(`git commit -m "feat: with spaces" --trailer 'a b'`)
	want := []string{"git", "commit", "-m", "feat: with spaces", "--trailer", "a b"}
	if len(got) != len(want) {
		t.Fatalf("token count mismatch: got %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d: got %q want %q", i, got[i], want[i])
		}
	}
}
