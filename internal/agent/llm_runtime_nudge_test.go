package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLooksLikeAnnouncement(t *testing.T) {
	yes := []string{
		"I'll start by exploring the repository structure to understand the codebase.",
		"I\u2019ll start by exploring…",
		"Let me look at the failing test first.",
		"let me check the config...",
		"I'm going to read the parser and then fix the bug.",
		"First, let me inspect the Makefile.",
		"Now I'll run the tests.",
		"I'll take a look at the parser.",
		"I'll first read the Makefile, then run the tests.",
		"I'll start with reading the failing test.",
		"I'm starting by reading the config loader.",
		"I'll need to check the lock file first.",
		"Let me look at internal/agent/foo.go:42 first.",
	}
	for _, s := range yes {
		if !looksLikeAnnouncement(s) {
			t.Errorf("looksLikeAnnouncement(%q) = false, want true", s)
		}
	}
	no := []string{
		"",
		"Done. The parser now rejects a double space; tests pass.",
		"Let me explain: the function returns early when the list is empty.",
		"I'll leave it as is; the code is already correct.",
		"Let me know if you want me to run the full test suite.",
		"I'll keep it. Running the tests showed nothing new.",
		"The bug is in parse.go: I'll note that the fix is a one-liner.",
		"I'll start by exploring " + strings.Repeat("the repository ", 30),
		// Questions and replies waiting on the user: nudging them would push
		// the model to act without the answer it asked for.
		"I'll help you fix that. Could you paste the full error message?",
		"I'll run the migration once you confirm which environment to target: staging or production?",
		"I'll need to look at the config file. Where is it?",
		"I need to know which branch you want me to review before I start.",
		"I need to look at the actual error output to diagnose this. Could you paste it?",
		"Let me check one thing first: do you want the v1 or the v2 endpoint changed?",
		"I'll run the migration once you confirm.",
		// Short final answers that open like an announcement.
		"Let me explain how to run the tests: use `make test`.",
		"Let me start with the short answer: no, the cache is never invalidated.",
		"Let me start with the short answer, which is no.",
		"Let's look at it differently: the function is O(n), which is fine here.",
		"Let me check... Actually no: the config already sets the timeout to 30s.",
		"I'm ready to run the tests whenever needed.",
	}
	for _, s := range no {
		if looksLikeAnnouncement(s) {
			t.Errorf("looksLikeAnnouncement(%q) = true, want false", s)
		}
	}
}

// An announce-only first reply gets one nudge, and the next reply is the
// answer.
func TestRunToolLoopAnnounceOnlyIsNudgedOnce(t *testing.T) {
	pm, url, ls := newLoopServer(t,
		loopReply{content: "I'll start by exploring the repository."},
		loopReply{content: "done"})
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil {
		t.Fatal(err)
	}
	if res.content != "done" {
		t.Fatalf("content = %q, want the reply after the nudge", res.content)
	}
	reqs := ls.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if got := lastUserText(reqs[1]); got != announceOnlyNudge {
		t.Fatalf("second request must carry the announce nudge, last user text = %q", got)
	}
}

// The nudge is spent once per turn: a second announcement ends the turn as
// it did before nudges existed.
func TestRunToolLoopAnnounceOnlyNudgeIsBounded(t *testing.T) {
	pm, url, ls := newLoopServer(t,
		loopReply{content: "I'll start by exploring the repository."},
		loopReply{content: "Let me look at the code."},
		loopReply{content: "never reached"})
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil {
		t.Fatal(err)
	}
	if res.content != "Let me look at the code." {
		t.Fatalf("content = %q", res.content)
	}
	if n := len(ls.requests()); n != 2 {
		t.Fatalf("requests = %d, want 2 (one nudge only)", n)
	}
}

// A clarifying question is a legitimate end of turn: the user must answer
// it, so the loop returns it instead of nudging the model to go on alone.
func TestRunToolLoopQuestionIsNotNudged(t *testing.T) {
	question := "I'll run the migration once you confirm which environment to target: staging or production?"
	pm, url, ls := newLoopServer(t,
		loopReply{content: question},
		loopReply{content: "never reached"})
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(ls.requests()); n != 1 {
		t.Fatalf("requests = %d, want 1 (no nudge)", n)
	}
	if res.content != question {
		t.Fatalf("content = %q, want the question", res.content)
	}
}

// No nudge once the turn has made a tool call, or when the agent has no
// tools to call, or when the host's step budget is spent.
func TestRunToolLoopAnnounceNudgeConditions(t *testing.T) {
	t.Run("after a tool call", func(t *testing.T) {
		pm, url, ls := newLoopServer(t,
			loopReply{toolName: "file-read", toolArgs: `{"path":"missing.txt"}`},
			loopReply{content: "Let me check the other file."},
			loopReply{content: "never reached"})
		if _, err := runToolLoop(context.Background(), loopCfg(t, pm, url)); err != nil {
			t.Fatal(err)
		}
		if n := len(ls.requests()); n != 2 {
			t.Fatalf("requests = %d, want 2", n)
		}
	})
	t.Run("no tools", func(t *testing.T) {
		pm, url, ls := newLoopServer(t,
			loopReply{content: "Let me check the code."},
			loopReply{content: "never reached"})
		cfg := loopCfg(t, pm, url)
		cfg.AllowedTools = nil
		if _, err := runToolLoop(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if n := len(ls.requests()); n != 1 {
			t.Fatalf("requests = %d, want 1", n)
		}
	})
	t.Run("step cap", func(t *testing.T) {
		pm, url, ls := newLoopServer(t,
			loopReply{content: "Let me check the code."},
			loopReply{content: "never reached"})
		cfg := loopCfg(t, pm, url)
		cfg.MaxSteps = 1
		if _, err := runToolLoop(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if n := len(ls.requests()); n != 1 {
			t.Fatalf("requests = %d, want 1", n)
		}
	})
}

// A reply whose finish reason is tool-calls but that carries no tool call
// gets one nudge asking for the call again; the text it did carry stays in
// the history ahead of the nudge.
func TestRunToolLoopDroppedToolCallIsNudgedOnce(t *testing.T) {
	pm, url, ls := newLoopServer(t,
		loopReply{content: "Reading the file now.", finish: "tool_calls"},
		loopReply{content: "Still nothing.", finish: "tool_calls"},
		loopReply{content: "never reached"})
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil {
		t.Fatal(err)
	}
	reqs := ls.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (one nudge only)", len(reqs))
	}
	if got := lastUserText(reqs[1]); got != droppedToolCallNudge {
		t.Fatalf("second request must carry the dropped-call nudge, last user text = %q", got)
	}
	if res.content != "Still nothing." {
		t.Fatalf("content = %q, want the second reply", res.content)
	}
	var sawFirst bool
	for _, m := range res.messages {
		if m.Content == "Reading the file now." {
			sawFirst = true
		}
	}
	if !sawFirst {
		t.Fatal("the first reply's text must stay in the history ahead of the nudge")
	}
}

// A second dropped-call reply with no text is an empty reply: it is not
// ended on, but handed to the bounded empty-reply nudges.
func TestRunToolLoopTextlessDroppedCallFallsBackToEmptyReply(t *testing.T) {
	pm, url, ls := newLoopServer(t,
		loopReply{finish: "tool_calls"},
		loopReply{finish: "tool_calls"},
		loopReply{content: "done"})
	res, err := runToolLoop(context.Background(), loopCfg(t, pm, url))
	if err != nil {
		t.Fatal(err)
	}
	reqs := ls.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	if got := lastUserText(reqs[1]); got != droppedToolCallNudge {
		t.Fatalf("second request: last user text = %q, want the dropped-call nudge", got)
	}
	if got := lastUserText(reqs[2]); got != emptyReplyNudge {
		t.Fatalf("third request: last user text = %q, want the empty-reply nudge", got)
	}
	if res.content != "done" {
		t.Fatalf("content = %q, want done", res.content)
	}
}

// Every reply is logged at debug level with its finish reason and the raw
// details, so a dropped tool call can be diagnosed from the log.
func TestRunToolLoopLogsReplyAtDebugLevel(t *testing.T) {
	var buf bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(saved) })

	pm, url, _ := newLoopServer(t, loopReply{content: "Nothing to do here.", finish: "tool_calls"}, loopReply{content: "done"})
	if _, err := runToolLoop(context.Background(), loopCfg(t, pm, url)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`msg="agent reply"`, "finish=tool-calls", "raw_finish=tool-calls", "tool_calls=0",
		`text_head="Nothing to do here."`, "finish=stop",
		"stopped for tool calls but none arrived",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("debug log missing %q:\n%s", want, out)
		}
	}
}
