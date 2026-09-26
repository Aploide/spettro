package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"spettro/internal/agent"
	"spettro/internal/remote"
)

// fakeAskServer stands in for the remote server: subs connected clients, and
// an optional canned reply (nil: the client never answers).
type fakeAskServer struct {
	subs  int
	reply *remote.AskUserReply
	asked bool
}

func (f *fakeAskServer) SubscriberCount() int { return f.subs }

func (f *fakeAskServer) RequestAskUser(ctx context.Context, _ string, _ map[string]any) (remote.AskUserReply, error) {
	f.asked = true
	if f.reply != nil {
		return *f.reply, nil
	}
	<-ctx.Done()
	return remote.AskUserReply{}, ctx.Err()
}

var testForm = agent.AskUserForm{Questions: []agent.AskUserQuestion{{
	Header:   "Database",
	Question: "Which database?",
	Options:  []agent.AskUserOption{{Label: "Postgres"}, {Label: "SQLite"}},
}}}

func TestHeadlessAskUserWithoutClientFailsFast(t *testing.T) {
	srv := &fakeAskServer{}
	_, err := headlessAskUser(context.Background(), srv, "q-1", testForm, time.Hour)
	if !errors.Is(err, agent.ErrNoUserAvailable) {
		t.Fatalf("err = %v, want ErrNoUserAvailable", err)
	}
	if srv.asked {
		t.Fatal("question published with nobody connected")
	}
}

// A connected client that never answers (a script following the event
// stream) must not stall the run forever.
func TestHeadlessAskUserUnansweredTimesOut(t *testing.T) {
	srv := &fakeAskServer{subs: 1}
	start := time.Now()
	_, err := headlessAskUser(context.Background(), srv, "q-1", testForm, 50*time.Millisecond)
	if !errors.Is(err, agent.ErrNoUserAvailable) {
		t.Fatalf("err = %v, want ErrNoUserAvailable", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("wait limit not applied")
	}
}

// Interrupting the run is not "nobody answered": the cancellation propagates.
func TestHeadlessAskUserCancelIsNotNoUser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := headlessAskUser(ctx, &fakeAskServer{subs: 1}, "q-1", testForm, time.Hour)
	if err == nil || errors.Is(err, agent.ErrNoUserAvailable) {
		t.Fatalf("err = %v, want the context error", err)
	}
}

func TestHeadlessAskUserAnswered(t *testing.T) {
	srv := &fakeAskServer{subs: 1, reply: &remote.AskUserReply{Answers: map[string]string{"Database": "SQLite"}}}
	answers, err := headlessAskUser(context.Background(), srv, "q-1", testForm, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || len(answers[0].Selected) != 1 || answers[0].Selected[0] != "SQLite" {
		t.Fatalf("answers = %+v", answers)
	}
}

func TestHeadlessAskUserWaitFromEnv(t *testing.T) {
	cases := map[string]time.Duration{"": defaultHeadlessAskUserWait, "0": 0, "30": 30 * time.Second, "x": defaultHeadlessAskUserWait, "-1": defaultHeadlessAskUserWait}
	for raw, want := range cases {
		t.Setenv(askUserWaitEnv, raw)
		if got := headlessAskUserWait(); got != want {
			t.Errorf("%s=%q: got %s, want %s", askUserWaitEnv, raw, got, want)
		}
	}
}
