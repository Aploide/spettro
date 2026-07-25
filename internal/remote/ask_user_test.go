package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// waitForEvent reads the replay buffer for the newest event of a kind, which is
// how these tests observe a publish without standing up an SSE subscriber.
func waitForEvent(s *Server, kind string, within time.Duration) (Event, bool) {
	deadline := time.Now().Add(within)
	for {
		s.mu.RLock()
		var found Event
		var ok bool
		for _, ev := range s.recent {
			if ev.Kind == kind {
				found, ok = ev, true
			}
		}
		s.mu.RUnlock()
		if ok {
			return found, true
		}
		if time.Now().After(deadline) {
			return Event{}, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// askUserReply drives one ask-user interaction: the event body the caller
// published is returned along with whatever the client answered.
func askUserReply(t *testing.T, s *Server, data map[string]any, body string) (Event, AskUserReply, error) {
	t.Helper()
	type result struct {
		reply AskUserReply
		err   error
	}
	done := make(chan result, 1)
	go func() {
		reply, err := s.RequestAskUser(context.Background(), "q-1", data)
		done <- result{reply, err}
	}()

	published, ok := waitForEvent(s, "ask_user", 2*time.Second)
	if !ok {
		t.Fatal("no ask_user event was published")
	}

	resp := doReq(t, s, http.MethodPost, "/ask-user", "test-token", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ask-user = %d", resp.StatusCode)
	}
	select {
	case got := <-done:
		return published, got.reply, got.err
	case <-time.After(2 * time.Second):
		t.Fatal("RequestAskUser did not return after the answer")
		return Event{}, AskUserReply{}, nil
	}
}

// The caller's payload is published verbatim with the question id added, and a
// form-aware client's per-question answers come back keyed by header.
func TestRequestAskUser_FormAnswers(t *testing.T) {
	s := startTestServer(t)
	data := map[string]any{
		"version":   2,
		"count":     2,
		"questions": []map[string]any{{"header": "Database"}, {"header": "Checks"}},
		"question":  "Which database?",
	}
	body := `{"question_id":"q-1","answers":{"Database":"SQLite","Checks":"go vet, gofmt"}}`

	published, reply, err := askUserReply(t, s, data, body)
	if err != nil {
		t.Fatalf("RequestAskUser: %v", err)
	}
	if published.Data["question_id"] != "q-1" {
		t.Fatalf("the event must carry the question id: %+v", published.Data)
	}
	if published.Data["version"] != 2 || published.Data["question"] != "Which database?" {
		t.Fatalf("the caller's payload must be published as given: %+v", published.Data)
	}
	if reply.Answers["Database"] != "SQLite" || reply.Answers["Checks"] != "go vet, gofmt" {
		t.Fatalf("per-question answers lost: %+v", reply.Answers)
	}
}

// A client that only understands the flat shape answers as it always did; the
// caller is the one that decides which question that answers.
func TestRequestAskUser_FlatAnswerStillWorks(t *testing.T) {
	s := startTestServer(t)
	_, reply, err := askUserReply(t, s, map[string]any{"question": "Which database?"},
		`{"question_id":"q-1","answer":"SQLite"}`)
	if err != nil {
		t.Fatalf("RequestAskUser: %v", err)
	}
	if reply.Answer != "SQLite" || len(reply.Answers) != 0 {
		t.Fatalf("reply = %+v", reply)
	}
}

// A run cancelled or interrupted mid-form resolves the waiting call exactly
// once, with the context's error — never a hang, and never an empty answer the
// model could read as a decision.
func TestRequestAskUser_CancelResolvesOnce(t *testing.T) {
	s := startTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := s.RequestAskUser(ctx, "q-cancel", map[string]any{"question": "Which database?"})
		done <- err
	}()
	// Give the call time to register before cancelling it.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled ask-user must fail rather than answer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RequestAskUser did not return after cancellation")
	}

	// The pending answer is gone with it, so a late client is told so instead
	// of blocking on a channel nobody reads.
	resp := doReq(t, s, http.MethodPost, "/ask-user", "test-token", `{"question_id":"q-cancel","answer":"SQLite"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("late answer = %d, want 404", resp.StatusCode)
	}
}

// Answering twice is a conflict, not a second delivery to a call that already
// returned.
func TestRequestAskUser_SecondAnswerConflicts(t *testing.T) {
	s := startTestServer(t)
	done := make(chan struct{})
	go func() {
		_, _ = s.RequestAskUser(context.Background(), "q-dup", map[string]any{"question": "Which database?"})
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)

	if resp := doReq(t, s, http.MethodPost, "/ask-user", "test-token", `{"question_id":"q-dup","answer":"SQLite"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("first answer = %d", resp.StatusCode)
	}
	<-done

	resp := doReq(t, s, http.MethodPost, "/ask-user", "test-token", `{"question_id":"q-dup","answer":"Postgres"}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a second answer must not be accepted")
	}
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
}
