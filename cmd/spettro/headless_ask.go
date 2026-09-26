package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"spettro/internal/agent"
	"spettro/internal/remote"
)

// askUserWaitEnv overrides how long a headless run waits for a remote client
// to answer an ask-user form: whole seconds, 0 to wait indefinitely.
const askUserWaitEnv = "SPETTRO_ASK_USER_TIMEOUT_SEC"

// defaultHeadlessAskUserWait bounds the wait for a remote answer. A connected
// client is not necessarily one that answers questions — a script following
// the event stream never will — and an unbounded wait would stall the run for
// good; past the limit the agent is told nobody answered and carries on.
const defaultHeadlessAskUserWait = 5 * time.Minute

// headlessAskUserWait resolves the ask-user wait from the environment.
func headlessAskUserWait() time.Duration {
	raw := strings.TrimSpace(os.Getenv(askUserWaitEnv))
	if raw == "" {
		return defaultHeadlessAskUserWait
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 0 {
		return defaultHeadlessAskUserWait
	}
	return time.Duration(secs) * time.Second
}

// askUserReconnectGrace is how long after an answering client dropped off the
// event stream a question still waits for it to come back: a phone that
// backgrounded the app or a proxy recycling the stream reconnects within
// seconds, and the question then goes out live instead of being given up on.
const askUserReconnectGrace = 30 * time.Second

// askUserPresencePoll is how often a question waiting on a reconnect checks
// for the client. A variable so tests can shorten it.
var askUserPresencePoll = 250 * time.Millisecond

// askUserSeq numbers headless questions. Every ask gets a fresh id, so a late
// answer to a question that already expired can never be taken as the answer
// to a later one in the same run.
var askUserSeq atomic.Uint64

// nextQuestionID returns a question id unique within this process.
func nextQuestionID() string {
	return fmt.Sprintf("q-%d", askUserSeq.Add(1))
}

// askUserServer is the part of the remote server a headless ask-user needs.
type askUserServer interface {
	Answerers() (connected int, lastLeft time.Time)
	RequestAskUser(ctx context.Context, questionID string, data map[string]any) (remote.AskUserReply, error)
}

// awaitAnswerer reports whether a client that might answer is on the event
// stream. Observers (/events?observe=1) do not count. With none connected it
// waits only if one dropped off within askUserReconnectGrace, until it is back
// or the grace runs out; a client that never connected, or left long ago, is
// not waited for.
func awaitAnswerer(ctx context.Context, srv askUserServer) (bool, error) {
	connected, lastLeft := srv.Answerers()
	if connected > 0 {
		return true, nil
	}
	if lastLeft.IsZero() {
		return false, nil
	}
	deadline := lastLeft.Add(askUserReconnectGrace)
	tick := time.NewTicker(askUserPresencePoll)
	defer tick.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-tick.C:
		}
		if connected, _ = srv.Answerers(); connected > 0 {
			return true, nil
		}
	}
	return false, nil
}

// headlessAskUser puts a form to the remote client. With no client on the
// event stream that could answer (see awaitAnswerer) nobody could see the
// question, so it fails fast with agent.ErrNoUserAvailable; otherwise it waits
// up to wait (0: no limit) and reports an unanswered question the same way.
// The whole form goes out in one versioned event: a client that only
// understands the flat v1 shape answers its first question, and the rest come
// back skipped rather than defaulted.
func headlessAskUser(ctx context.Context, srv askUserServer, questionID string, form agent.AskUserForm, wait time.Duration) ([]agent.AskUserAnswer, error) {
	present, err := awaitAnswerer(ctx, srv)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fmt.Errorf("%w: no client that can answer is connected to the event stream", agent.ErrNoUserAvailable)
	}
	askCtx := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		askCtx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	reply, err := srv.RequestAskUser(askCtx, questionID, agent.RemoteAskUserPayload(form, 0))
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: no answer within %s", agent.ErrNoUserAvailable, wait)
		}
		return nil, err
	}
	return agent.AnswersFromRemote(form, reply.Answer, reply.Answers), nil
}
