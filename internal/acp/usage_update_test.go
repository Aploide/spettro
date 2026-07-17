package acp

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// syncBuffer collects everything the agent-side connection writes to its peer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestPrompt_EmitsUsageUpdate drives a full prompt turn against a fake model
// and asserts a live usage_update session notification goes out on the wire.
func TestPrompt_EmitsUsageUpdate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cs := newSteeringCaptureServer(t, []string{
		"FINAL\nAll done.",
	})

	if err := os.MkdirAll(filepath.Join(home, ".spettro"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON := `{"active_provider":` + strconvQuote(cs.srv.URL) + `,"active_model":"fake-model","permission":"yolo"}`
	if err := os.WriteFile(filepath.Join(home, ".spettro", "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: cs.srv.URL, Name: "fake-model", Local: true}})

	manifest := config.AgentManifest{Agents: []config.AgentSpec{{
		ID:         "coding",
		Mode:       "worker",
		Permission: config.PermissionYOLO,
		Enabled:    true,
	}}}

	b := newBridge(Options{
		CWD:       t.TempDir(),
		GlobalDir: t.TempDir(),
		Providers: pm,
		Manifest:  manifest,
	})
	out := &syncBuffer{}
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	b.conn = acpsdk.NewAgentSideConnection(b, out, pr)

	s := &acpSession{
		id:                "sess-usage",
		cwd:               t.TempDir(),
		agentID:           "coding",
		manifest:          manifest,
		mediaDir:          t.TempDir(),
		startedAt:         time.Now(),
		commandsAnnounced: true,
	}
	b.sessions[s.id] = s

	resp, err := b.Prompt(context.Background(), acpsdk.PromptRequest{
		SessionId: acpsdk.SessionId(s.id),
		Prompt:    []acpsdk.ContentBlock{acpsdk.TextBlock("Say hi.")},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if resp.StopReason != acpsdk.StopReasonEndTurn {
		t.Fatalf("stop reason = %v", resp.StopReason)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens == 0 {
		t.Fatalf("PromptResponse.Usage missing or zero: %+v", resp.Usage)
	}

	// Notifications are written asynchronously by the connection; wait
	// briefly for the usage_update to land on the wire.
	deadline := time.After(3 * time.Second)
	for !strings.Contains(out.String(), `"sessionUpdate":"usage_update"`) {
		select {
		case <-deadline:
			t.Fatalf("no usage_update notification on the wire; output:\n%s", out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
