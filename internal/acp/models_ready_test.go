package acp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// lateModelHarness starts an agent whose model discovery is still running:
// ModelsReady stays open until the returned release function runs, which
// first adds a "late-model" endpoint to the provider manager.
func lateModelHarness(t *testing.T) (*acpHarness, func()) {
	t.Helper()
	llm := newScriptedLLM(t)
	ready := make(chan struct{})
	var pm *provider.Manager
	h := newACPHarnessWith(t, llm, config.PermissionYOLO, func(o *Options) {
		o.ModelsReady = ready
		pm = o.Providers
	})
	release := func() {
		pm.AddLocalModels([]provider.Model{{Provider: "http://127.0.0.1:1", Name: "late-model", Local: true}})
		close(ready)
	}
	return h, release
}

func shortModelsWait(t *testing.T, d time.Duration) {
	t.Helper()
	orig := modelsReadyWait
	modelsReadyWait = d
	t.Cleanup(func() { modelsReadyWait = orig })
}

func mentions(t *testing.T, v any, needle string) bool {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(raw), needle)
}

// initialize never waits for model discovery.
func TestInitializeDoesNotWaitForModels(t *testing.T) {
	shortModelsWait(t, 5*time.Second)
	h, release := lateModelHarness(t)
	defer release()
	start := time.Now()
	h.initialize()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("initialize took %v with discovery pending", d)
	}
}

// session/new gives up after modelsReadyWait, and the late models reach the
// session as a config_option_update once discovery finishes.
func TestLateModelsArriveAsConfigOptionUpdate(t *testing.T) {
	shortModelsWait(t, 50*time.Millisecond)
	h, release := lateModelHarness(t)
	h.initialize()

	start := time.Now()
	resp, err := h.conn.NewSession(h.ctx(), acpsdk.NewSessionRequest{Cwd: h.cwd, McpServers: []acpsdk.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("session/new took %v, want about modelsReadyWait", d)
	}
	if mentions(t, resp.ConfigOptions, "late-model") {
		t.Fatal("late model listed before discovery finished")
	}

	release()
	update := h.waitForUpdate(resp.SessionId, "config_option_update")
	if !mentions(t, update, "late-model") {
		t.Fatalf("config_option_update lacks the late model: %v", update)
	}
}

// Discovery that finishes within the wait is in the session/new response.
func TestSessionNewWaitsForPromptDiscovery(t *testing.T) {
	shortModelsWait(t, 5*time.Second)
	h, release := lateModelHarness(t)
	h.initialize()
	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()
	resp, err := h.conn.NewSession(h.ctx(), acpsdk.NewSessionRequest{Cwd: h.cwd, McpServers: []acpsdk.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if !mentions(t, resp.ConfigOptions, "late-model") {
		t.Fatal("session/new did not wait for discovery that finished in time")
	}
}
