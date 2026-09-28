package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const askUserTestArgs = `{"question":"Which database?","options":["Postgres","SQLite"]}`

// When nobody can answer, ask-user must return at once with guidance to carry
// on — never block the run (the go-08 benchmark hang) and never fail it.
func TestRunAskUserWithoutReachableUserReturnsImmediately(t *testing.T) {
	blocking := func(ctx context.Context, _ AskUserForm) ([]AskUserAnswer, error) {
		<-ctx.Done() // a callback nobody will ever answer
		return nil, ctx.Err()
	}
	cases := map[string]*toolRuntime{
		"no callback": {},
		"goal mode":   {askUser: blocking, goalMode: true},
		"sub-agent":   {askUser: blocking, delegationDepth: 1},
	}
	for name, rt := range cases {
		done := make(chan struct{})
		var out string
		var err error
		go func() {
			out, err = rt.runAskUser(context.Background(), []byte(askUserTestArgs))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: ask-user blocked", name)
		}
		if err != nil || out != noUserAvailableResult {
			t.Fatalf("%s: got %q, %v", name, out, err)
		}
	}
}

// A host that knows nobody is there (headless with no client, an unanswered
// question past its limit) reports ErrNoUserAvailable, possibly wrapped; the
// model gets the same guidance as a successful result.
func TestRunAskUserMapsNoUserAvailable(t *testing.T) {
	rt := &toolRuntime{askUser: func(context.Context, AskUserForm) ([]AskUserAnswer, error) {
		return nil, fmt.Errorf("%w: no answer within 5m0s", ErrNoUserAvailable)
	}}
	out, err := rt.runAskUser(context.Background(), []byte(askUserTestArgs))
	if err != nil || out != noUserAvailableResult {
		t.Fatalf("got %q, %v", out, err)
	}
	if rt.shouldStop() {
		t.Fatal("an unreachable user must not end the turn")
	}
}

func TestParallelExecAskUserWithoutUserSucceeds(t *testing.T) {
	rt := newShellTestRuntime(t)
	rt.goalMode = true
	res := rt.parallelExec(context.Background(), []toolCall{{Tool: "ask-user", Args: []byte(askUserTestArgs)}}, map[string]struct{}{"ask-user": {}}, nil)
	if res[0].status != "success" || res[0].output != noUserAvailableResult {
		t.Fatalf("result = %+v", res[0])
	}
}

// When the user picks "reply in chat", the turn ends: calls the model queued
// after ask-user in the same step must not run on a guess.
func TestParallelExecStopsAfterAskUserChatExit(t *testing.T) {
	rt := newShellTestRuntime(t)
	rt.askUser = func(context.Context, AskUserForm) ([]AskUserAnswer, error) {
		return nil, ErrAskUserReplyInChat
	}
	calls := []toolCall{
		{Tool: "ask-user", Args: []byte(askUserTestArgs)},
		{Tool: "bash", Args: []byte(`{"command":"touch ran.txt"}`)},
	}
	allowed := map[string]struct{}{"ask-user": {}, "bash": {}}
	res := rt.parallelExec(context.Background(), calls, allowed, nil)
	if !rt.shouldStop() {
		t.Fatal("chat exit must request a stop")
	}
	if res[0].status != "success" {
		t.Fatalf("ask-user result = %+v", res[0])
	}
	if res[1].status != "error" || !strings.Contains(res[1].output, "not executed") {
		t.Fatalf("bash after the chat exit must not run, got %+v", res[1])
	}
	if _, err := os.Stat(filepath.Join(rt.cwd, "ran.txt")); err == nil {
		t.Fatal("bash ran after the user chose to reply in chat")
	}
}
