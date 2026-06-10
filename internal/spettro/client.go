// Package spettro implements the client for the Spettro Subscription backend:
// the CLI device-flow login and the OpenAI-compatible inference proxy.
//
// The backend exposes:
//   - POST /auth/initiate        — register a login session, returns a browser URL
//   - GET  /auth/poll/:session   — poll until the user signs in, returns an ep_ key
//   - GET  /v1/models            — list the models available on the user's plan
//   - GET  /v1/account           — the user's plan, status, and credit usage
//   - POST /v1/chat/completions  — OpenAI-compatible inference (handled by the
//     provider manager via the "spettro" provider, not this file)
package spettro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// ProviderID is the provider key used throughout the CLI (config, manager).
	ProviderID = "spettro"
	// ProviderName is the human-facing label shown in the UI.
	ProviderName = "Spettro"
	// PricingURL is where users upgrade their plan.
	PricingURL = "https://spettro.eyed.to/pricing"

	defaultBaseURL = "http://localhost:42099"
)

// BaseURL returns the backend's public base URL. Overridable with SPETTRO_API_URL.
func BaseURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("SPETTRO_API_URL")), "/"); v != "" {
		return v
	}
	return defaultBaseURL
}

// InferenceBaseURL is the OpenAI-compatible /v1 base passed to the provider manager.
func InferenceBaseURL() string { return BaseURL() + "/v1" }

func httpClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// NewSessionID returns a fresh UUID for a login session.
func NewSessionID() string { return uuid.NewString() }

// Initiate registers a login session and returns the browser URL the user must
// open to sign in. The caller polls Poll(sessionID) until completion.
func Initiate(ctx context.Context, sessionID string) (browserURL string, err error) {
	body, _ := json.Marshal(map[string]string{"session_id": sessionID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL()+"/auth/initiate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach the Spettro server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("login could not be started (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		BrowserURL string `json:"browser_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.BrowserURL == "" {
		return "", fmt.Errorf("server returned no browser URL")
	}
	return out.BrowserURL, nil
}

// PollResult is the outcome of one Poll call.
type PollResult struct {
	Status string // "pending" | "complete" | "expired"
	APIKey string // non-empty only on the first "complete" poll
}

// Poll checks a login session once. On completion it returns the ep_ API key
// exactly once (the backend clears it server-side afterwards).
func Poll(ctx context.Context, sessionID string) (PollResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL()+"/auth/poll/"+sessionID, nil)
	if err != nil {
		return PollResult{}, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return PollResult{}, fmt.Errorf("could not reach the Spettro server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return PollResult{}, fmt.Errorf("login session not found — please try again")
	}
	if resp.StatusCode != http.StatusOK {
		return PollResult{}, fmt.Errorf("login check failed (HTTP %d)", resp.StatusCode)
	}
	var out struct {
		Status string `json:"status"`
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return PollResult{}, err
	}
	return PollResult{Status: out.Status, APIKey: out.APIKey}, nil
}

// ModelInfo is one entry from GET /v1/models.
type ModelInfo struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
}

// ListModels returns the models available on the authenticated user's plan.
func ListModels(ctx context.Context, apiKey string) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL()+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the Spettro server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("your Spettro session is no longer valid — please sign in again")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("could not list models (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// Account is the response from GET /v1/account.
type Account struct {
	Email            string  `json:"email"`
	Plan             string  `json:"plan"`
	PlanStatus       string  `json:"plan_status"`
	CreditsUsed      float64 `json:"credits_used"`
	CreditLimit      float64 `json:"credit_limit"`
	RemainingCredits float64 `json:"remaining_credits"`
}

// GetAccount returns the user's plan and credit usage.
func GetAccount(ctx context.Context, apiKey string) (*Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL()+"/v1/account", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the Spettro server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("your Spettro session is no longer valid — please sign in again")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not load account (HTTP %d)", resp.StatusCode)
	}
	var acc Account
	if err := json.NewDecoder(resp.Body).Decode(&acc); err != nil {
		return nil, err
	}
	return &acc, nil
}
