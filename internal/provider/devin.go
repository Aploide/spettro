package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DevinSessionsBaseURL is the public Cognition / Devin API host.
const DevinSessionsBaseURL = "https://api.devin.ai"

// devinBaseURLOverride lets tests redirect the Manager.CallDevin path to a
// scripted httptest server. Production code leaves this empty and the
// adapter uses DevinSessionsBaseURL. Override is package-level so both
// DevinAdapter.Send (when BaseURL is unset) and the manager helper see it.
var devinBaseURLOverride string

// devinPollOverride lets tests shrink the adapter's poll cadence so
// scripted test servers don't sit idle for seconds. Production stays at
// devinDefaultPollInterval.
var devinPollOverride time.Duration

// OverrideDevinBaseURLForTesting points subsequent Manager.CallDevin invocations
// at a custom base URL (e.g. an httptest server) and shrinks the poll
// interval to 5ms so the test runs in milliseconds rather than seconds.
// It returns a restore function that resets both overrides when the test
// cleans up. Not safe for concurrent tests; intended for single-goroutine
// integration tests only.
func OverrideDevinBaseURLForTesting(url string) (restore func()) {
	prevURL := devinBaseURLOverride
	prevPoll := devinPollOverride
	devinBaseURLOverride = url
	devinPollOverride = 5 * time.Millisecond
	return func() {
		devinBaseURLOverride = prevURL
		devinPollOverride = prevPoll
	}
}

// Default polling cadence and overall deadline for DevinAdapter. Devin
// sessions can take many minutes; the adapter blocks until completion or
// until ctx is cancelled, whichever comes first.
const (
	devinDefaultPollInterval = 5 * time.Second
	devinDefaultMaxWait      = 30 * time.Minute
)

// DevinAdapter forwards a chat-completion-shaped Request to the Devin
// Sessions API and synchronously polls until the session reaches a
// terminal status.
//
// Devin is an agent runner, not a chat-completion endpoint: the model
// chosen by Spettro and the request's Thinking level are NOT honoured by
// the Devin backend (the active model and thinking config are owned by
// the user's Devin account). Both are accepted but quietly ignored so
// callers can keep a uniform Request type. The user_agent ends up as a
// session "tag" so sessions launched through Spettro are easy to spot in
// the Devin dashboard.
//
// Authentication & API version
//
//   - v3 keys (prefix "cog_") require OrgID and target
//     POST /v3/organizations/{org_id}/sessions
//   - v1 keys (prefix "apk_" or anything else) target the legacy
//     POST /v1/sessions endpoint, which doesn't take an org id.
//
// Both endpoints poll on GET /<version>/sessions/<id> for v1 or
// GET /v3/organizations/{org_id}/sessions/{devin_id} for v3 and finish
// when the session reports a terminal status.
type DevinAdapter struct {
	APIKey       string
	OrgID        string
	BaseURL      string
	PollInterval time.Duration
	MaxWait      time.Duration
	HTTPClient   *http.Client
}

func (a DevinAdapter) Send(ctx context.Context, model string, req Request) (Response, error) {
	apiKey := strings.TrimSpace(a.APIKey)
	if apiKey == "" {
		return Response{}, fmt.Errorf("devin: API key is required (set api_keys.devin)")
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return Response{}, fmt.Errorf("devin: prompt is required")
	}
	baseURL := strings.TrimRight(a.BaseURL, "/")
	if baseURL == "" {
		baseURL = devinBaseURLOverride
	}
	if baseURL == "" {
		baseURL = DevinSessionsBaseURL
	}
	pollInterval := a.PollInterval
	if pollInterval <= 0 {
		pollInterval = devinPollOverride
	}
	if pollInterval <= 0 {
		pollInterval = devinDefaultPollInterval
	}
	maxWait := a.MaxWait
	if maxWait <= 0 {
		maxWait = devinDefaultMaxWait
	}
	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	useV3 := strings.HasPrefix(apiKey, "cog_")
	if useV3 && strings.TrimSpace(a.OrgID) == "" {
		return Response{}, fmt.Errorf("devin: cog_ API keys require an organization id (set devin_org_id)")
	}

	deadline := time.Now().Add(maxWait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	// 1. Create the session.
	sessionID, sessionURL, err := a.createSession(ctx, client, baseURL, apiKey, prompt, useV3)
	if err != nil {
		return Response{}, err
	}

	// 2. Poll until the session reaches a terminal state.
	finalContent, terminalStatus, err := a.pollUntilTerminal(ctx, client, baseURL, apiKey, sessionID, useV3, pollInterval, deadline)
	if err != nil {
		// Surface the session URL so users can check the run in the dashboard.
		return Response{}, fmt.Errorf("devin session %s (%s): %w", sessionID, sessionURL, err)
	}

	// Append a short footer so users know which Devin session generated
	// the output and where to inspect it.
	body := strings.TrimSpace(finalContent)
	if body == "" {
		body = fmt.Sprintf("(devin session %s ended with status %q and produced no agent message)", sessionID, terminalStatus)
	} else if sessionURL != "" {
		body = body + "\n\n— Devin session: " + sessionURL
	}

	return Response{
		Content:         body,
		EstimatedTokens: 0, // Devin reports ACUs, not tokens; map at TUI surface if useful later.
	}, nil
}

// createSession POSTs the prompt to the right endpoint and returns
// (session_id, session_url).
func (a DevinAdapter) createSession(ctx context.Context, client *http.Client, baseURL, apiKey, prompt string, useV3 bool) (string, string, error) {
	body, _ := json.Marshal(map[string]any{
		"prompt":   prompt,
		"unlisted": true,
		"tags":     []string{"spettro"},
	})
	url := baseURL + "/v1/sessions"
	if useV3 {
		url = fmt.Sprintf("%s/v3/organizations/%s/sessions", baseURL, a.OrgID)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("devin: build request: %w", err)
	}
	r.Header.Set("Authorization", "Bearer "+apiKey)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	resp, err := client.Do(r)
	if err != nil {
		return "", "", fmt.Errorf("devin: create session: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("devin: create session: HTTP %d: %s", resp.StatusCode, truncateForError(string(raw), 600))
	}
	var parsed struct {
		SessionID string `json:"session_id"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", "", fmt.Errorf("devin: decode create response: %w", err)
	}
	if parsed.SessionID == "" {
		return "", "", fmt.Errorf("devin: create session returned no session_id (HTTP %d)", resp.StatusCode)
	}
	return parsed.SessionID, parsed.URL, nil
}

// pollUntilTerminal blocks until the session reports a terminal status,
// then returns the latest agent / devin message body and the terminal
// status string. It honours ctx and the supplied wall-clock deadline.
func (a DevinAdapter) pollUntilTerminal(ctx context.Context, client *http.Client, baseURL, apiKey, sessionID string, useV3 bool, pollInterval time.Duration, deadline time.Time) (string, string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		default:
		}
		if !time.Now().Before(deadline) {
			return "", "", fmt.Errorf("polling deadline exceeded after %s", time.Until(deadline).Truncate(time.Second))
		}

		status, statusDetail, content, terminal, err := a.fetchSession(ctx, client, baseURL, apiKey, sessionID, useV3)
		if err != nil {
			return "", "", err
		}
		if terminal {
			label := status
			if statusDetail != "" {
				label = status + "/" + statusDetail
			}
			return content, label, nil
		}

		// Sleep with ctx-aware wakeup so /interrupt cancels the poll.
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// fetchSession GETs the session, decides whether it has reached a terminal
// state, and (for terminal v1 sessions) extracts the latest devin/agent
// message body. v3's GET returns status + status_detail; v1 returns
// status_enum.
func (a DevinAdapter) fetchSession(ctx context.Context, client *http.Client, baseURL, apiKey, sessionID string, useV3 bool) (status, statusDetail, content string, terminal bool, err error) {
	url := baseURL + "/v1/sessions/" + sessionID
	if useV3 {
		url = fmt.Sprintf("%s/v3/organizations/%s/sessions/%s", baseURL, a.OrgID, sessionID)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", "", false, fmt.Errorf("devin: build poll request: %w", err)
	}
	r.Header.Set("Authorization", "Bearer "+apiKey)
	r.Header.Set("Accept", "application/json")
	resp, err := client.Do(r)
	if err != nil {
		return "", "", "", false, fmt.Errorf("devin: poll: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", false, fmt.Errorf("devin: poll HTTP %d: %s", resp.StatusCode, truncateForError(string(raw), 600))
	}

	if useV3 {
		var parsed struct {
			Status       string                 `json:"status"`
			StatusDetail string                 `json:"status_detail"`
			Structured   map[string]interface{} `json:"structured_output"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return "", "", "", false, fmt.Errorf("devin: decode v3 session: %w", err)
		}
		terminal = isV3Terminal(parsed.Status, parsed.StatusDetail)
		if !terminal {
			return parsed.Status, parsed.StatusDetail, "", false, nil
		}
		// On terminal, fetch messages so we can return the last devin
		// message. Structured output, when present, is preferred since
		// it's the explicit final answer.
		if len(parsed.Structured) > 0 {
			if b, mErr := json.MarshalIndent(parsed.Structured, "", "  "); mErr == nil {
				return parsed.Status, parsed.StatusDetail, string(b), true, nil
			}
		}
		msgs, mErr := a.fetchV3Messages(ctx, client, baseURL, apiKey, sessionID)
		if mErr != nil {
			return parsed.Status, parsed.StatusDetail, "", true, mErr
		}
		return parsed.Status, parsed.StatusDetail, msgs, true, nil
	}

	// v1 path
	var parsed struct {
		StatusEnum string `json:"status_enum"`
		Status     string `json:"status"`
		Messages   []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"messages"`
		Structured map[string]interface{} `json:"structured_output"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", "", "", false, fmt.Errorf("devin: decode v1 session: %w", err)
	}
	terminal = isV1Terminal(parsed.StatusEnum)
	if !terminal {
		return parsed.StatusEnum, "", "", false, nil
	}
	if len(parsed.Structured) > 0 {
		if b, mErr := json.MarshalIndent(parsed.Structured, "", "  "); mErr == nil {
			return parsed.StatusEnum, "", string(b), true, nil
		}
	}
	body := lastV1AgentMessage(parsed.Messages)
	return parsed.StatusEnum, "", body, true, nil
}

// fetchV3Messages pulls the conversation log for a v3 session and returns
// the last "devin" (i.e. assistant) message body.
func (a DevinAdapter) fetchV3Messages(ctx context.Context, client *http.Client, baseURL, apiKey, sessionID string) (string, error) {
	url := fmt.Sprintf("%s/v3/organizations/%s/sessions/%s/messages?first=200", baseURL, a.OrgID, sessionID)
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("devin: build messages request: %w", err)
	}
	r.Header.Set("Authorization", "Bearer "+apiKey)
	r.Header.Set("Accept", "application/json")
	resp, err := client.Do(r)
	if err != nil {
		return "", fmt.Errorf("devin: list messages: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("devin: list messages HTTP %d: %s", resp.StatusCode, truncateForError(string(raw), 600))
	}
	var parsed struct {
		Items []struct {
			Source  string `json:"source"`
			Message string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("devin: decode messages: %w", err)
	}
	for i := len(parsed.Items) - 1; i >= 0; i-- {
		if parsed.Items[i].Source == "devin" && strings.TrimSpace(parsed.Items[i].Message) != "" {
			return parsed.Items[i].Message, nil
		}
	}
	return "", nil
}

// isV3Terminal reports whether a v3 session has reached a state where no
// further progress is expected. We treat suspended/error/exit as terminal,
// plus running+finished (the agent has answered the prompt without
// suspending the session).
func isV3Terminal(status, detail string) bool {
	switch status {
	case "exit", "error":
		return true
	case "suspended":
		return true
	}
	if status == "running" && detail == "finished" {
		return true
	}
	return false
}

// isV1Terminal reports whether a v1 session is finished. The blocked state
// means Devin is waiting for the user; we treat that as terminal too so
// Spettro can surface the question and let the user resume manually.
func isV1Terminal(status string) bool {
	switch status {
	case "finished", "expired", "blocked":
		return true
	}
	return false
}

// lastV1AgentMessage returns the body of the most recent message Devin
// (rather than the user) emitted. v1 message types are not strictly
// enumerated; we accept any non-user message tagged with a "devin" or
// "agent" prefix and fall back to the last non-user message we see.
func lastV1AgentMessage(msgs []struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		t := strings.ToLower(msgs[i].Type)
		body := strings.TrimSpace(msgs[i].Message)
		if body == "" {
			continue
		}
		if strings.Contains(t, "user") {
			continue
		}
		return msgs[i].Message
	}
	return ""
}

func truncateForError(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
