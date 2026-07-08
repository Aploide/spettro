package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ServerConfig describes one language server. Users configure `command` and
// `enabled` in .spettro/lsp.json; args/filetypes/language ids fall back to
// built-in defaults for well-known server keys (go, typescript, python, rust).
type ServerConfig struct {
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	Enabled   bool     `json:"enabled"`
	Filetypes []string `json:"filetypes,omitempty"` // extensions like ".go"
}

// Config is the on-disk shape of .spettro/lsp.json.
type Config struct {
	Servers map[string]ServerConfig `json:"servers"`
}

var defaultFiletypes = map[string][]string{
	"go":         {".go"},
	"typescript": {".ts", ".tsx", ".js", ".jsx"},
	"python":     {".py"},
	"rust":       {".rs"},
}

var extLanguageID = map[string]string{
	".go":  "go",
	".ts":  "typescript",
	".tsx": "typescriptreact",
	".js":  "javascript",
	".jsx": "javascriptreact",
	".py":  "python",
	".rs":  "rust",
}

func languageIDForPath(path, serverKey string) string {
	if id, ok := extLanguageID[strings.ToLower(filepath.Ext(path))]; ok {
		return id
	}
	return serverKey
}

// loadConfig reads the project config, falling back to the user-global one.
// A missing config means LSP is disabled for the workspace.
func loadConfig(root string) (Config, bool) {
	paths := []string{filepath.Join(root, ".spettro", "lsp.json")}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".spettro", "lsp.json"))
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg Config
		if json.Unmarshal(raw, &cfg) != nil {
			continue
		}
		enabled := false
		for key, sc := range cfg.Servers {
			if len(sc.Filetypes) == 0 {
				sc.Filetypes = defaultFiletypes[key]
				cfg.Servers[key] = sc
			}
			if sc.Enabled && strings.TrimSpace(sc.Command) != "" && len(sc.Filetypes) > 0 {
				enabled = true
			}
		}
		if enabled {
			return cfg, true
		}
	}
	return Config{}, false
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
		if !sc.Enabled || strings.TrimSpace(sc.Command) == "" {
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
		if sc.Enabled {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
