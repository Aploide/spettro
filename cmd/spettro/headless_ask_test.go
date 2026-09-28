package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"spettro/internal/agent"
	"spettro/internal/remote"
)

// fakeAskServer stands in for the remote server: subs connected clients that
// may answer, when the last one left, and an optional canned reply (nil: the
// client never answers).
type fakeAskServer struct {
	mu       sync.Mutex
	subs     int
	lastLeft time.Time
	reply    *remote.AskUserReply
	asked    bool
}

func (f *fakeAskServer) Answerers() (int, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs, f.lastLeft
}

func (f *fakeAskServer) setSubs(n int) {
	f.mu.Lock()
	f.subs = n
	f.mu.Unlock()
}

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

// A client that dropped off moments ago (a phone backgrounding the app) is
// waited for: once it is back the question goes out instead of being given up.
func TestHeadlessAskUserWaitsForReconnectingClient(t *testing.T) {
	orig := askUserPresencePoll
	askUserPresencePoll = 10 * time.Millisecond
	t.Cleanup(func() { askUserPresencePoll = orig })
	srv := &fakeAskServer{lastLeft: time.Now(), reply: &remote.AskUserReply{Answer: "SQLite"}}
	time.AfterFunc(100*time.Millisecond, func() { srv.setSubs(1) })
	answers, err := headlessAskUser(context.Background(), srv, "q-1", testForm, time.Hour)
	if err != nil {
		t.Fatalf("reconnecting client not waited for: %v", err)
	}
	if !srv.asked || len(answers) != 1 {
		t.Fatalf("asked=%v answers=%+v", srv.asked, answers)
	}
}

// The reconnect wait is bounded by the grace since the client left, and a
// client that left long ago is not waited for at all.
func TestHeadlessAskUserReconnectGraceIsBounded(t *testing.T) {
	orig := askUserPresencePoll
	askUserPresencePoll = 10 * time.Millisecond
	t.Cleanup(func() { askUserPresencePoll = orig })
	for name, left := range map[string]time.Time{
		"grace running out": time.Now().Add(-askUserReconnectGrace + 100*time.Millisecond),
		"left long ago":     time.Now().Add(-time.Hour),
	} {
		srv := &fakeAskServer{lastLeft: left}
		start := time.Now()
		_, err := headlessAskUser(context.Background(), srv, "q-1", testForm, time.Hour)
		if !errors.Is(err, agent.ErrNoUserAvailable) {
			t.Fatalf("%s: err = %v, want ErrNoUserAvailable", name, err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("%s: waited %s", name, elapsed)
		}
		if srv.asked {
			t.Fatalf("%s: question published with nobody connected", name)
		}
	}
}

// Each question gets its own id, so a late answer to an expired question
// cannot be delivered to the next one.
func TestNextQuestionIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := nextQuestionID()
		if seen[id] {
			t.Fatalf("question id %s reused", id)
		}
		seen[id] = true
	}
}
