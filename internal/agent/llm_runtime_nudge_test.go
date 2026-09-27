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
