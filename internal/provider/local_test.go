package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newAuthedModelsServer(t *testing.T, wantKey string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantKey != "" && r.Header.Get("Authorization") != "Bearer "+wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeLocalServerWithAPIKey(t *testing.T) {
	srv := newAuthedModelsServer(t, "sk-local")

	models, err := ProbeLocalServer(context.Background(), srv.URL, "sk-local")
	if err != nil {
		t.Fatalf("probe with key: %v", err)
	}
	if len(models) != 1 || models[0].Name != "test-model" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

func TestProbeLocalServerMissingKey(t *testing.T) {
	srv := newAuthedModelsServer(t, "sk-local")

	_, err := ProbeLocalServer(context.Background(), srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "requires an API key") {
		t.Fatalf("expected requires-key error, got %v", err)
	}
}

func TestProbeLocalServerWrongKey(t *testing.T) {
	srv := newAuthedModelsServer(t, "sk-local")

	_, err := ProbeLocalServer(context.Background(), srv.URL, "wrong")
	if err == nil || !strings.Contains(err.Error(), "rejected the API key") {
		t.Fatalf("expected rejected-key error, got %v", err)
	}
}

func TestProbeLocalServerNoAuth(t *testing.T) {
	srv := newAuthedModelsServer(t, "")

	models, err := ProbeLocalServer(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("probe without key: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("unexpected models: %+v", models)
	}
}
