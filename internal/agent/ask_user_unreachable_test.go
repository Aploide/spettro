package agent

import (
	"context"
	"fmt"
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
