package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// toolMessageContents returns the content of every tool-result message of a
// recorded chat-completions request, in order.
func toolMessageContents(body map[string]any) []string {
	msgs, _ := body["messages"].([]any)
	var out []string
	for _, m := range msgs {
		if mm, _ := m.(map[string]any); mm["role"] == "tool" {
			out = append(out, fmt.Sprint(mm["content"]))
		}
	}
	return out
}

func todoLoopCfg(t *testing.T, replies ...loopReply) (toolLoopConfig, *loopServer) {
	t.Helper()
	pm, url, ls := newLoopServer(t, replies...)
	cfg := loopCfg(t, pm, url)
	cfg.AllowedTools = append(cfg.AllowedTools, "todo-write", "glob")
	cfg.SessionDir = filepath.Join(t.TempDir(), "sessions", "sess-1")
	return cfg, ls
}

// A step whose only call is todo-write gets its normal result plus the note,
// once per turn: the second such step does not.
func TestTodoOnlyStepGetsNoteOncePerTurn(t *testing.T) {
	plan := `{"todos":[{"id":"1","content":"fix the parser"}]}`
	cfg, ls := todoLoopCfg(t,
		loopReply{toolName: "todo-write", toolArgs: plan},
		loopReply{toolName: "todo-write", toolArgs: `{"todos":[{"id":"1","content":"fix the parser","status":"in_progress"}],"merge":true}`},
		loopReply{content: "done"})
	if _, err := runToolLoop(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	reqs := ls.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	first := toolMessageContents(reqs[1])
	if len(first) != 1 || !strings.HasSuffix(first[0], "\n"+todoOnlyStepNote) || !strings.Contains(first[0], "fix the parser") {
		t.Fatalf("first todo-only step result = %q", first)
	}
	second := toolMessageContents(reqs[2])
	if len(second) != 2 || strings.Contains(second[1], todoOnlyStepNote) {
		t.Fatalf("second todo-only step must carry no note: %q", second)
	}
}

// todo-write sent together with a real call is the intended use: no note.
func TestTodoWithRealCallGetsNoNote(t *testing.T) {
	res := []parallelResult{{name: "todo-write", status: "success"}, {name: "glob", status: "success"}}
	if todoOnlyStep(res) {
		t.Fatal("todo-write next to glob counted as a todo-only step")
	}
	for _, c := range []struct {
		res  []parallelResult
		want bool
	}{
		{nil, false},
		{[]parallelResult{{name: "todo-write", status: "success"}}, true},
		{[]parallelResult{{name: "todo-write", status: "success"}, {name: "todo-write", status: "success"}}, true},
		{[]parallelResult{{name: "todo-write", status: "error"}}, false},
	} {
		if got := todoOnlyStep(c.res); got != c.want {
			t.Errorf("todoOnlyStep(%+v) = %v, want %v", c.res, got, c.want)
		}
	}
}
