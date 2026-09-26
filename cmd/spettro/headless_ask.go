package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
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

// askUserServer is the part of the remote server a headless ask-user needs.
type askUserServer interface {
	SubscriberCount() int
	RequestAskUser(ctx context.Context, questionID string, data map[string]any) (remote.AskUserReply, error)
}

// headlessAskUser puts a form to the remote client. With no client on the
// event stream nobody could see the question, so it fails fast with
// agent.ErrNoUserAvailable; otherwise it waits up to wait (0: no limit) and
// reports an unanswered question the same way. The whole form goes out in one
// versioned event: a client that only understands the flat v1 shape answers
// its first question, and the rest come back skipped rather than defaulted.
func headlessAskUser(ctx context.Context, srv askUserServer, questionID string, form agent.AskUserForm, wait time.Duration) ([]agent.AskUserAnswer, error) {
	if srv.SubscriberCount() == 0 {
		return nil, fmt.Errorf("%w: no client is connected to the event stream", agent.ErrNoUserAvailable)
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
