package spettro

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newStubBackend returns a server mimicking the Go backend's auth + v1 routes.
func newStubBackend(t *testing.T) *httptest.Server {
	t.Helper()
	const validKey = "ep_testkey"
	mux := http.NewServeMux()

	mux.HandleFunc("/auth/initiate", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.SessionID == "" {
			http.Error(w, "session_id required", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"browser_url": "http://frontend.example/auth/cli?session=" + req.SessionID,
		})
	})

	// Poll returns pending on the first call, then complete with the key once.
	calls := map[string]int{}
	mux.HandleFunc("/auth/poll/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/auth/poll/")
		calls[id]++
		switch calls[id] {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "complete", "api_key": validKey})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "complete"})
		}
	})

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+validKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "deepseek-v4-flash", "object": "model", "owned_by": "deepseek"},
				{"id": "qwen3.7-plus", "object": "model", "owned_by": "alibaba"},
			},
		})
	})

	mux.HandleFunc("/v1/account", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+validKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email":        "user@example.com",
			"plan":         "pro",
			"plan_status":  "active",
			"credits_used": 1.5,
			"credit_limit": 16.0,
		})
	})

	srv := httptest.NewServer(mux)
	t.Setenv("SPETTRO_API_URL", srv.URL)
	t.Cleanup(srv.Close)
	return srv
}

func TestDeviceFlow(t *testing.T) {
	newStubBackend(t)
	ctx := context.Background()

	sessionID := NewSessionID()
	browserURL, err := Initiate(ctx, sessionID)
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	if !strings.Contains(browserURL, sessionID) {
		t.Fatalf("browser URL %q missing session id", browserURL)
	}

	// First poll: pending.
	res, err := Poll(ctx, sessionID)
	if err != nil {
		t.Fatalf("Poll #1: %v", err)
	}
	if res.Status != "pending" {
		t.Fatalf("expected pending, got %q", res.Status)
	}

	// Second poll: complete with key.
	res, err = Poll(ctx, sessionID)
	if err != nil {
		t.Fatalf("Poll #2: %v", err)
	}
	if res.Status != "complete" || res.APIKey == "" {
		t.Fatalf("expected complete+key, got status=%q key=%q", res.Status, res.APIKey)
	}

	apiKey := res.APIKey
	models, err := ListModels(ctx, apiKey)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "deepseek-v4-flash" {
		t.Fatalf("unexpected models: %+v", models)
	}

	acc, err := GetAccount(ctx, apiKey)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if acc.Plan != "pro" || acc.Email != "user@example.com" {
		t.Fatalf("unexpected account: %+v", acc)
	}
}

func TestUnauthorizedModels(t *testing.T) {
	newStubBackend(t)
	if _, err := ListModels(context.Background(), "ep_wrong"); err == nil {
		t.Fatal("expected error for invalid key")
	}
}

func TestInferenceBaseURL(t *testing.T) {
	t.Setenv("SPETTRO_API_URL", "https://api.example.com/")
	if got := InferenceBaseURL(); got != "https://api.example.com/v1" {
		t.Fatalf("InferenceBaseURL = %q", got)
	}
}
