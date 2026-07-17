package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ServerConfig describes one language server. LSP works with zero config:
// well-known servers are auto-detected on PATH for the built-in server keys
// (go, typescript, python, rust, c, cpp, csharp, swift). .spettro/lsp.json is
// an optional override: entries replace the built-in defaults per key, and
// `"enabled": false` turns a server off. A missing `enabled` means enabled.
type ServerConfig struct {
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	Enabled   *bool    `json:"enabled,omitempty"`
	Filetypes []string `json:"filetypes,omitempty"` // extensions like ".go"
}

func (sc ServerConfig) enabled() bool { return sc.Enabled == nil || *sc.Enabled }

// Config is the on-disk shape of .spettro/lsp.json.
type Config struct {
	Servers map[string]ServerConfig `json:"servers"`
}

// builtinServer lists candidate commands for a server key; the first one found
// on PATH wins, so e.g. python works with either pyright or pylsp installed.
type builtinServer struct {
	candidates []ServerConfig
	filetypes  []string
}

var builtinServers = map[string]builtinServer{
	"go":         {candidates: []ServerConfig{{Command: "gopls"}}, filetypes: []string{".go"}},
	"typescript": {candidates: []ServerConfig{{Command: "typescript-language-server", Args: []string{"--stdio"}}}, filetypes: []string{".ts", ".tsx", ".js", ".jsx"}},
	"python":     {candidates: []ServerConfig{{Command: "pyright-langserver", Args: []string{"--stdio"}}, {Command: "pylsp"}}, filetypes: []string{".py"}},
	"rust":       {candidates: []ServerConfig{{Command: "rust-analyzer"}}, filetypes: []string{".rs"}},
	"c":          {candidates: []ServerConfig{{Command: "clangd"}}, filetypes: []string{".c", ".h"}},
	"cpp":        {candidates: []ServerConfig{{Command: "clangd"}}, filetypes: []string{".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx"}},
	"csharp":     {candidates: []ServerConfig{{Command: "csharp-ls"}, {Command: "OmniSharp", Args: []string{"-lsp"}}, {Command: "omnisharp", Args: []string{"-lsp"}}}, filetypes: []string{".cs"}},
	"swift":      {candidates: []ServerConfig{{Command: "sourcekit-lsp"}}, filetypes: []string{".swift"}},
}

var defaultFiletypes = func() map[string][]string {
	m := make(map[string][]string, len(builtinServers))
	for k, b := range builtinServers {
		m[k] = b.filetypes
	}
	return m
}()

var extLanguageID = map[string]string{
	".go":    "go",
	".ts":    "typescript",
	".tsx":   "typescriptreact",
	".js":    "javascript",
	".jsx":   "javascriptreact",
	".py":    "python",
	".rs":    "rust",
	".c":     "c",
	".h":     "c",
	".cpp":   "cpp",
	".cc":    "cpp",
	".cxx":   "cpp",
	".hpp":   "cpp",
	".hh":    "cpp",
	".hxx":   "cpp",
	".cs":    "csharp",
	".swift": "swift",
}

func languageIDForPath(path, serverKey string) string {
	if id, ok := extLanguageID[strings.ToLower(filepath.Ext(path))]; ok {
		return id
	}
	return serverKey
}

// lookPath is exec.LookPath, swappable in tests.
var lookPath = exec.LookPath

// detectBuiltinServers returns the built-in servers whose binary is on PATH.
func detectBuiltinServers() map[string]ServerConfig {
	servers := map[string]ServerConfig{}
	for key, b := range builtinServers {
		for _, cand := range b.candidates {
			if _, err := lookPath(cand.Command); err == nil {
				cand.Filetypes = b.filetypes
				servers[key] = cand
				break
			}
		}
	}
	return servers
}

// loadConfig builds the effective config: built-in servers auto-detected on
// PATH, overlaid by the optional user configs (~/.spettro/lsp.json first, then
// the project's .spettro/lsp.json, so the project wins per server key). false
// means no usable server, and LSP silently degrades for the workspace.
func loadConfig(root string) (Config, bool) {
	cfg := Config{Servers: detectBuiltinServers()}
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".spettro", "lsp.json"))
	}
	paths = append(paths, filepath.Join(root, ".spettro", "lsp.json"))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var user Config
		if json.Unmarshal(raw, &user) != nil {
			continue
		}
		for key, sc := range user.Servers {
			if strings.TrimSpace(sc.Command) == "" {
				// no command in the override: keep the detected one, but let
				// the entry toggle it (e.g. {"enabled": false})
				if base, ok := cfg.Servers[key]; ok {
					base.Enabled = sc.Enabled
					if len(sc.Filetypes) > 0 {
						base.Filetypes = sc.Filetypes
					}
					cfg.Servers[key] = base
				}
				continue
			}
			if len(sc.Filetypes) == 0 {
				sc.Filetypes = defaultFiletypes[key]
			}
			cfg.Servers[key] = sc
		}
	}
	enabled := false
	for _, sc := range cfg.Servers {
		if sc.enabled() && strings.TrimSpace(sc.Command) != "" && len(sc.Filetypes) > 0 {
			enabled = true
		}
	}
	return cfg, enabled
}

// Manager owns the lazily started language servers for one workspace root.
type Manager struct {
	root string
	cfg  Config

	mu      sync.Mutex
	clients map[string]*Client // server key → running client
	broken  map[string]string  // server key → start failure (until lsp-restart)
}

var (
	regMu    sync.Mutex
	registry = map[string]*Manager{}
)

// ForWorkspace returns the shared Manager for root, or nil when no LSP server
// is configured — the nil return is the "silently degrade" path callers rely
// on. The nil result is also cached, so unconfigured workspaces pay one stat
// per lookup at most.
func ForWorkspace(root string) *Manager {
	root = filepath.Clean(root)
	regMu.Lock()
	defer regMu.Unlock()
	if m, ok := registry[root]; ok {
		return m
	}
	cfg, ok := loadConfig(root)
	if !ok {
		registry[root] = nil
		return nil
	}
	m := &Manager{root: root, cfg: cfg, clients: map[string]*Client{}, broken: map[string]string{}}
	registry[root] = m
	return m
}

// serverKeyFor returns the enabled server key matching the file's extension.
func (m *Manager) serverKeyFor(path string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ext := strings.ToLower(filepath.Ext(path))
	keys := make([]string, 0, len(m.cfg.Servers))
	for k := range m.cfg.Servers {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic pick if two servers claim an extension
	for _, key := range keys {
		sc := m.cfg.Servers[key]
		if !sc.enabled() || strings.TrimSpace(sc.Command) == "" {
			continue
		}
		for _, ft := range sc.Filetypes {
			if strings.EqualFold(ft, ext) {
				return key, true
			}
		}
	}
	return "", false
}

// clientFor lazily starts (and caches) the server responsible for path. A
// failed start is remembered so a missing binary is not retried on every edit;
// lsp-restart clears the mark.
func (m *Manager) clientFor(ctx context.Context, path string) (*Client, string, error) {
	key, ok := m.serverKeyFor(path)
	if !ok {
		return nil, "", fmt.Errorf("no lsp server configured for %s files", filepath.Ext(path))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[key]; ok && c.alive() {
		return c, key, nil
	}
	if reason, bad := m.broken[key]; bad {
		return nil, key, fmt.Errorf("lsp server %q unavailable: %s (use lsp-restart to retry)", key, reason)
	}
	sc := m.cfg.Servers[key]
	initCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = ctx // startup deliberately uses its own budget: the server outlives the call
	c, err := startClient(initCtx, m.root, sc.Command, sc.Args)
	if err != nil {
		m.broken[key] = err.Error()
		return nil, key, err
	}
	m.clients[key] = c
	return c, key, nil
}

func (m *Manager) relPath(uri string) string {
	p := uriToPath(uri)
	if rel, err := filepath.Rel(m.root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return p
}

func (m *Manager) formatDiagnostics(uri string, ds []Diagnostic) string {
	var sb strings.Builder
	rel := m.relPath(uri)
	for _, d := range ds {
		src := ""
		if d.Source != "" {
			src = " (" + d.Source + ")"
		}
		fmt.Fprintf(&sb, "%s:%d:%d [%s] %s%s\n", rel, d.Range.Start.Line+1, d.Range.Start.Character+1,
			severityLabel(d.Severity), strings.ReplaceAll(d.Message, "\n", " "), src)
	}
	return sb.String()
}

// DiagnosticsForFile syncs the file's current on-disk content to the server
// and waits (bounded by ctx) for fresh diagnostics. Empty string means clean.
func (m *Manager) DiagnosticsForFile(ctx context.Context, absPath string) (string, error) {
	c, key, err := m.clientFor(ctx, absPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	uri, sinceGen, err := c.syncFile(absPath, languageIDForPath(absPath, key), string(raw))
	if err != nil {
		return "", err
	}
	ds := c.waitDiagnostics(ctx, uri, sinceGen)
	return strings.TrimRight(m.formatDiagnostics(uri, ds), "\n"), nil
}

// WorkspaceDiagnostics reports the latest published diagnostics across all
// running servers. It never starts a server.
func (m *Manager) WorkspaceDiagnostics() string {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.mu.Unlock()
	var parts []string
	for _, c := range clients {
		all := c.allDiagnostics()
		uris := make([]string, 0, len(all))
		for uri, ds := range all {
			if len(ds) > 0 {
				uris = append(uris, uri)
			}
		}
		sort.Strings(uris)
		for _, uri := range uris {
			parts = append(parts, strings.TrimRight(m.formatDiagnostics(uri, all[uri]), "\n"))
		}
	}
	return strings.Join(parts, "\n")
}

// positionOfSymbol finds the first occurrence of symbol in content, preferring
// whole-identifier matches, and returns its zero-based LSP position.
func positionOfSymbol(content, symbol string) (Position, bool) {
	isWord := func(b byte) bool {
		return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}
	lines := strings.Split(content, "\n")
	var fallback *Position
	for li, line := range lines {
		from := 0
		for {
			idx := strings.Index(line[from:], symbol)
			if idx < 0 {
				break
			}
			col := from + idx
			if fallback == nil {
				fallback = &Position{Line: li, Character: col}
			}
			beforeOK := col == 0 || !isWord(line[col-1])
			after := col + len(symbol)
			afterOK := after >= len(line) || !isWord(line[after])
			if beforeOK && afterOK {
				return Position{Line: li, Character: col}, true
			}
			from = col + len(symbol)
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return Position{}, false
}

// Lookup resolves references (kind "references", the default) or the
// definition (kind "definition") for a symbol in absPath. The position comes
// from line/character when given (1-based), otherwise from the first
// occurrence of symbol in the file.
func (m *Manager) Lookup(ctx context.Context, absPath, symbol, kind string, line, character int) (string, error) {
	c, key, err := m.clientFor(ctx, absPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	content := string(raw)
	uri, _, err := c.syncFile(absPath, languageIDForPath(absPath, key), content)
	if err != nil {
		return "", err
	}
	var pos Position
	if line > 0 {
		pos = Position{Line: line - 1}
		if character > 0 {
			pos.Character = character - 1
		}
	} else {
		p, ok := positionOfSymbol(content, symbol)
		if !ok {
			return "", fmt.Errorf("symbol %q not found in %s", symbol, absPath)
		}
		pos = p
	}
	var locs []Location
	if kind == "definition" {
		locs, err = c.definition(ctx, uri, pos)
	} else {
		locs, err = c.references(ctx, uri, pos)
	}
	if err != nil {
		return "", err
	}
	if len(locs) == 0 {
		return "no results", nil
	}
	var sb strings.Builder
	for _, l := range locs {
		fmt.Fprintf(&sb, "%s:%d:%d\n", m.relPath(l.URI), l.Range.Start.Line+1, l.Range.Start.Character+1)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// Restart stops the named server (or all servers when name is empty), clears
// any start-failure marks, and reloads the config so edits to lsp.json apply.
func (m *Manager) Restart(name string) string {
	if cfg, ok := loadConfig(m.root); ok {
		m.mu.Lock()
		m.cfg = cfg
		m.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var stopped []string
	for key, c := range m.clients {
		if name != "" && key != name {
			continue
		}
		c.Close()
		delete(m.clients, key)
		stopped = append(stopped, key)
	}
	if name == "" {
		m.broken = map[string]string{}
	} else {
		delete(m.broken, name)
	}
	sort.Strings(stopped)
	if len(stopped) == 0 {
		return "no matching running lsp server; it will start on next use"
	}
	return fmt.Sprintf("restarted lsp server(s): %s (respawn on next use)", strings.Join(stopped, ", "))
}

// ServerKeys lists configured servers for error messages / discoverability.
func (m *Manager) ServerKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.cfg.Servers))
	for k, sc := range m.cfg.Servers {
		if sc.enabled() {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
