package acp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"spettro/internal/agent"
	"spettro/internal/config"
	"spettro/internal/provider"
	"spettro/internal/skills"
)

// skillWorkspace returns a workspace with three project skills (a normal
// one, one named like the built-in /help, and a model-only one), HOME
// isolated and the shared skill cache cleared.
func skillWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	cwd := t.TempDir()
	write := func(name, front, body string) {
		dir := filepath.Join(cwd, ".spettro", "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: " + name + "\n" + front + "---\n" + body
		if err := os.WriteFile(filepath.Join(dir, skills.SkillFilename), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("greet", "description: Greets a person\nargument-hint: <name>\n", "Say hello to $ARGUMENTS.\n")
	write("help", "description: collides with /help\n", "never\n")
	write("background", "description: model only\nuser-invocable: false\n", "context\n")
	agent.ReloadSkills()
	t.Cleanup(agent.ReloadSkills)
	return cwd
}

func TestSessionCommandsAdvertiseSkills(t *testing.T) {
	cwd := skillWorkspace(t)
	b := &bridge{sessions: map[string]*acpSession{"s1": {id: "s1", cwd: cwd}}}
	cmds := b.sessionCommands(acpsdk.SessionId("s1"))
	byName := map[string]acpsdk.AvailableCommand{}
	count := map[string]int{}
	for _, c := range cmds {
		byName[c.Name] = c
		count[c.Name]++
	}
	greet, ok := byName["greet"]
	if !ok || !strings.Contains(greet.Description, "Greets a person") {
		t.Fatalf("greet not advertised: %+v", cmds)
	}
	if greet.Input == nil || greet.Input.Unstructured == nil || greet.Input.Unstructured.Hint != "<name>" {
		t.Errorf("greet input hint = %+v", greet.Input)
	}
	if count["help"] != 1 || strings.Contains(byName["help"].Description, "collides") {
		t.Errorf("the built-in /help must win: %+v", byName["help"])
	}
	if _, ok := byName["background"]; ok {
		t.Error("a model-only skill must not be advertised")
	}
	if _, ok := byName["skills"]; !ok {
		t.Error("/skills must be advertised")
	}
	// An unknown session gets the built-ins only.
	if got := b.sessionCommands(acpsdk.SessionId("gone")); len(got) != len(acpAvailableCommands) {
		t.Errorf("unknown session: %d commands, want %d", len(got), len(acpAvailableCommands))
	}
}

func TestResolveSkillCommand(t *testing.T) {
	cwd := skillWorkspace(t)
	cfg := config.UserConfig{}
	prompt, reply, ok := resolveSkillCommand(cwd, cfg, "/greet Ada Lovelace")
	if !ok || reply != "" {
		t.Fatalf("resolve /greet: ok=%v reply=%q", ok, reply)
	}
	if !strings.Contains(prompt, "Say hello to Ada Lovelace.") || !strings.Contains(prompt, "/greet Ada Lovelace") {
		t.Errorf("prompt:\n%s", prompt)
	}
	for _, input := range []string{"/help", "/background", "/nope", "greet"} {
		if _, _, ok := resolveSkillCommand(cwd, cfg, input); ok {
			t.Errorf("%q must not resolve to a skill", input)
		}
	}
	if _, _, ok := resolveSkillCommand(cwd, config.UserConfig{DisabledSkills: []string{"greet"}}, "/greet"); ok {
		t.Error("a disabled skill must not run")
	}
}

// End to end through Prompt: "/greet Ada" runs a turn whose request carries
// the skill's instructions, while the transcript keeps what the user typed;
// a $greet mention in an ordinary prompt does the same.
func TestPromptRunsSkillCommandAndMention(t *testing.T) {
	cwd := skillWorkspace(t)
	home := os.Getenv("HOME")

	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"x","object":"chat.completion.chunk","model":"fake-model","choices":[{"index":0,"delta":{"role":"assistant","content":"done"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"x","object":"chat.completion.chunk","model":"fake-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	if err := os.MkdirAll(filepath.Join(home, ".spettro"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON := `{"active_provider":` + strconvQuote(srv.URL) + `,"active_model":"fake-model","permission":"yolo"}`
	if err := os.WriteFile(filepath.Join(home, ".spettro", "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	pm := provider.NewManager()
	pm.AddLocalModels([]provider.Model{{Provider: srv.URL, Name: "fake-model", Local: true}})
	manifest := config.AgentManifest{Agents: []config.AgentSpec{{
		ID: "coding", Mode: "worker", AllowedTools: []string{"comment"}, Permission: config.PermissionYOLO, Enabled: true,
	}}}
	b := newBridge(Options{CWD: cwd, GlobalDir: t.TempDir(), Providers: pm, Manifest: manifest})
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	b.conn = acpsdk.NewAgentSideConnection(b, io.Discard, pr)
	s := &acpSession{
		id: "sess-skill", cwd: cwd, agentID: "coding", manifest: manifest,
		mediaDir: t.TempDir(), startedAt: time.Now(), commandsAnnounced: true,
	}
	b.sessions[s.id] = s

	for i, tc := range []struct{ input, wantInRequest string }{
		{"/greet Ada", "Say hello to Ada."},
		{"please use $greet", "Say hello to ."},
	} {
		if _, err := b.Prompt(context.Background(), acpsdk.PromptRequest{
			SessionId: acpsdk.SessionId(s.id),
			Prompt:    []acpsdk.ContentBlock{acpsdk.TextBlock(tc.input)},
		}); err != nil {
			t.Fatalf("%q: %v", tc.input, err)
		}
		mu.Lock()
		last := bodies[len(bodies)-1]
		mu.Unlock()
		if !strings.Contains(last, tc.wantInRequest) {
			t.Errorf("%q: request lacks %q:\n%s", tc.input, tc.wantInRequest, last)
		}
		if got := s.transcript[2*i].Content; got != tc.input {
			t.Errorf("%q: transcript records %q, want what the user typed", tc.input, got)
		}
	}
}

func TestSkillsSlashCommand(t *testing.T) {
	cwd := skillWorkspace(t)
	s := &acpSession{cwd: cwd}
	cfg := config.UserConfig{}
	reply, _, handled := handleExtendedSlashCommand(nil, s, &cfg, provider.NewManager(), "/skills")
	if !handled {
		t.Fatal("/skills must be handled")
	}
	for _, want := range []string{"greet [/greet]", "background [agent only]", filepath.Join(".spettro", "skills", "greet", "SKILL.md")} {
		if !strings.Contains(reply, want) {
			t.Errorf("/skills reply missing %q:\n%s", want, reply)
		}
	}
}
