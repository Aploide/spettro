package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"spettro/internal/homedir"
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
// settle overrides defaultSettle for servers known to publish in stages.
type builtinServer struct {
	candidates []ServerConfig
	filetypes  []string
	settle     time.Duration
}

var builtinServers = map[string]builtinServer{
	"go": {candidates: []ServerConfig{{Command: "gopls"}}, filetypes: []string{".go"}},
	// typescript-language-server publishes syntactic diagnostics first and
	// the semantic (type) errors in a later publish, so listen for longer.
	"typescript": {candidates: []ServerConfig{{Command: "typescript-language-server", Args: []string{"--stdio"}}}, filetypes: []string{".ts", ".tsx", ".js", ".jsx"}, settle: time.Second},
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

// extraBinDirs lists the well-known install locations searched after PATH.
// Toolchains install language servers into directories many users never put
// on PATH — `go install` into $GOBIN, $GOPATH/bin or ~/go/bin, `pip --user`
// and pipx into ~/.local/bin, cargo into ~/.cargo/bin — and JS projects keep
// theirs in node_modules/.bin. (The workspace's .spettro/lsp.json can already
// name any command to run, so looking in the workspace grants it nothing new.)
// Swappable in tests.
var extraBinDirs = func(root string) []string {
	dirs := []string{filepath.Join(root, "node_modules", ".bin")}
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	for _, gp := range filepath.SplitList(os.Getenv("GOPATH")) {
		if gp != "" {
			dirs = append(dirs, filepath.Join(gp, "bin"))
		}
	}
	if home, err := homedir.Dir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, "go", "bin"),
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".cargo", "bin"),
		)
	}
	return dirs
}

// FindServerBinary resolves a server command: PATH first, then the
// extraBinDirs install locations. A PATH hit keeps the bare command name; a
// fallback hit returns the absolute path, since the spawn would not find it.
func FindServerBinary(command, root string) (string, bool) {
	if _, err := lookPath(command); err == nil {
		return command, true
	}
	if strings.ContainsAny(command, `/\`) {
		return "", false // an explicit path either exists or it does not
	}
	for _, dir := range extraBinDirs(root) {
		// LookPath on a path with a separator checks just that file (and, on
		// Windows, tries the PATHEXT extensions).
		if p, err := lookPath(filepath.Join(dir, command)); err == nil {
			return p, true
		}
	}
	return "", false
}

// detectBuiltinServers returns the built-in servers whose binary is installed.
func detectBuiltinServers(root string) map[string]ServerConfig {
	servers := map[string]ServerConfig{}
	for key, b := range builtinServers {
		for _, cand := range b.candidates {
			if cmd, ok := FindServerBinary(cand.Command, root); ok {
				cand.Command = cmd
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
	cfg := Config{Servers: detectBuiltinServers(root)}
	var paths []string
	if home, err := homedir.Dir(); err == nil {
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

// serverStartTimeout bounds a server's spawn plus initialize handshake. The
// start runs in the background, so this is not what a tool call waits for:
// callers wait only as long as their own context allows.
const serverStartTimeout = 30 * time.Second

// ErrServerStarting is returned when the caller's context ran out while the
// server was still starting. The start carries on in the background, so a
// later call finds it ready.
var ErrServerStarting = errors.New("lsp server still starting")

// Manager owns the lazily started language servers for one workspace root.
type Manager struct {
	root string
	cfg  Config

	mu       sync.Mutex
	clients  map[string]*Client       // server key → running client
	broken   map[string]string        // server key → start failure (until lsp-restart)
	starting map[string]chan struct{} // server key → closed when its start ends
	// epoch invalidates starts begun before a Restart or Shutdown: they
	// close the server they started instead of registering it.
	epoch int
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
	// The root is resolved (not just cleaned) so the rootUri servers get, the
	// paths we send them and the paths they answer with are all the same
	// spelling of the same directory.
	root = realPath(root)
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
	m := newManager(root, cfg)
	registry[root] = m
	return m
}

func newManager(root string, cfg Config) *Manager {
	return &Manager{
		root:     root,
		cfg:      cfg,
		clients:  map[string]*Client{},
		broken:   map[string]string{},
		starting: map[string]chan struct{}{},
	}
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

// clientFor returns the server responsible for path, starting it on first use.
// The start runs in the background and is shared by every caller, so a
// server is paid for once per session; each caller waits for it only as
// long as its own ctx allows (ErrServerStarting otherwise). A failed start
// is remembered so a missing binary is not retried on every edit;
// lsp-restart clears the mark.
func (m *Manager) clientFor(ctx context.Context, path string) (*Client, string, error) {
	key, ok := m.serverKeyFor(path)
	if !ok {
		return nil, "", fmt.Errorf("no lsp server configured for %s files", filepath.Ext(path))
	}
	for {
		c, done, err := m.ensureStarted(key)
		if c != nil || err != nil {
			return c, key, err
		}
		select {
		case <-done:
			// loop: pick up the client, the failure, or (when a restart
			// raced the start) begin a fresh one
		case <-ctx.Done():
			return nil, key, fmt.Errorf("%w: %s", ErrServerStarting, m.serverName(key))
		}
	}
}

// Warm starts the server for path's file type in the background, if there is
// one and it is not already running, without waiting for it. Call it when a
// file is first looked at, so the server is up by the time it is edited.
func (m *Manager) Warm(path string) {
	if key, ok := m.serverKeyFor(path); ok {
		_, _, _ = m.ensureStarted(key)
	}
}

// ensureStarted returns the running client for key, or the channel that closes
// when its (possibly just begun) start finishes, or the remembered failure.
func (m *Manager) ensureStarted(key string) (*Client, <-chan struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[key]; ok && c.alive() {
		return c, nil, nil
	}
	if reason, bad := m.broken[key]; bad {
		return nil, nil, fmt.Errorf("lsp server %q unavailable: %s (use lsp-restart to retry)", key, reason)
	}
	if done, ok := m.starting[key]; ok {
		return nil, done, nil
	}
	done := make(chan struct{})
	m.starting[key] = done
	go m.start(key, m.cfg.Servers[key], m.epoch, done)
	return nil, done, nil
}

// start spawns one server and registers the outcome, unless a Restart or
// Shutdown happened meanwhile: then the new server is closed before done is,
// so whoever waits on done knows it is gone.
func (m *Manager) start(key string, sc ServerConfig, epoch int, done chan struct{}) {
	defer close(done)
	// A user-configured command gets the same install-location fallback as
	// the auto-detected ones.
	command := sc.Command
	if found, ok := FindServerBinary(command, m.root); ok {
		command = found
	}
	ctx, cancel := context.WithTimeout(context.Background(), serverStartTimeout)
	c, err := startClient(ctx, m.root, command, sc.Args)
	cancel()
	m.mu.Lock()
	stale := m.epoch != epoch
	if !stale {
		if err != nil {
			m.broken[key] = err.Error()
		} else {
			m.clients[key] = c
		}
	}
	if m.starting[key] == done {
		delete(m.starting, key)
	}
	m.mu.Unlock()
	if stale && c != nil {
		c.Close()
	}
}

// serverName is the human name of a server key: its command's base name
// ("gopls"), which is what users recognise, falling back to the key.
func (m *Manager) serverName(key string) string {
	m.mu.Lock()
	cmd := m.cfg.Servers[key].Command
	m.mu.Unlock()
	if cmd == "" {
		return key
	}
	base := filepath.Base(cmd)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func (m *Manager) relPath(path string) string {
	if rel, ok := relTo(m.root, path); ok {
		return rel
	}
	// A path spelled differently than the resolved root — a symlinked or short
	// Windows form a server answered with — only lines up once resolved, so
	// pay for that lookup, but only when the plain comparison came up short.
	if resolved := realPath(path); resolved != path {
		if rel, ok := relTo(m.root, resolved); ok {
			return rel
		}
	}
	return path
}

func relTo(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func (m *Manager) formatDiagnostics(path string, ds []Diagnostic) string {
	var sb strings.Builder
	rel := m.relPath(path)
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
	absPath = realPath(absPath)
	c, key, err := m.clientFor(ctx, absPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	d, err := c.syncFile(absPath, languageIDForPath(absPath, key), string(raw))
	if err != nil {
		return "", err
	}
	ds := c.waitDiagnostics(ctx, d.key, d.sinceGen)
	return strings.TrimRight(m.formatDiagnostics(absPath, ds), "\n"), nil
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
		paths := make([]string, 0, len(all))
		for path, ds := range all {
			if len(ds) > 0 {
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		for _, path := range paths {
			parts = append(parts, strings.TrimRight(m.formatDiagnostics(path, all[path]), "\n"))
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
	absPath = realPath(absPath)
	c, key, err := m.clientFor(ctx, absPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	content := string(raw)
	d, err := c.syncFile(absPath, languageIDForPath(absPath, key), content)
	if err != nil {
		return "", err
	}
	pos, err := resolvePosition(content, absPath, symbol, line, character)
	if err != nil {
		return "", err
	}
	var locs []Location
	if kind == "definition" {
		locs, err = c.definition(ctx, d.uri, pos)
	} else {
		locs, err = c.references(ctx, d.uri, pos)
	}
	if err != nil {
		return "", err
	}
	if len(locs) == 0 {
		return "no results", nil
	}
	var sb strings.Builder
	for _, l := range locs {
		fmt.Fprintf(&sb, "%s:%d:%d\n", m.relPath(uriToPath(l.URI)), l.Range.Start.Line+1, l.Range.Start.Character+1)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// resolvePosition turns the tool's addressing (1-based line/character, or a
// symbol name to search for) into a zero-based LSP position.
func resolvePosition(content, absPath, symbol string, line, character int) (Position, error) {
	if line > 0 {
		pos := Position{Line: line - 1}
		if character > 0 {
			pos.Character = character - 1
		}
		return pos, nil
	}
	pos, ok := positionOfSymbol(content, symbol)
	if !ok {
		return Position{}, fmt.Errorf("symbol %q not found in %s", symbol, absPath)
	}
	return pos, nil
}

// Hover returns the language server's hover text (type signature + docs) for
// a symbol in absPath, positioned like Lookup. Empty string means the server
// had nothing to say.
func (m *Manager) Hover(ctx context.Context, absPath, symbol string, line, character int) (string, error) {
	absPath = realPath(absPath)
	c, key, err := m.clientFor(ctx, absPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	content := string(raw)
	d, err := c.syncFile(absPath, languageIDForPath(absPath, key), content)
	if err != nil {
		return "", err
	}
	pos, err := resolvePosition(content, absPath, symbol, line, character)
	if err != nil {
		return "", err
	}
	out, err := c.hover(ctx, d.uri, pos)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// FileChange is one file's proposed content after a rename. The caller (the
// agent runtime) owns applying it, so permission prompts and checkpointing
// wrap the write exactly like file-write.
type FileChange struct {
	Path string // absolute
	Rel  string // workspace-relative (or absolute when outside the root)
	Old  string
	New  string
}

// RenameEdits asks the server to rename the symbol at the given position and
// returns the resulting per-file content changes without touching the disk.
func (m *Manager) RenameEdits(ctx context.Context, absPath, symbol string, line, character int, newName string) ([]FileChange, error) {
	absPath = realPath(absPath)
	c, key, err := m.clientFor(ctx, absPath)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	content := string(raw)
	d, err := c.syncFile(absPath, languageIDForPath(absPath, key), content)
	if err != nil {
		return nil, err
	}
	pos, err := resolvePosition(content, absPath, symbol, line, character)
	if err != nil {
		return nil, err
	}
	edits, err := c.rename(ctx, d.uri, pos, newName)
	if err != nil {
		return nil, err
	}
	var changes []FileChange
	for u, es := range edits {
		if len(es) == 0 {
			continue
		}
		p := uriToPath(u)
		old, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("rename touches unreadable file %s: %w", p, err)
		}
		updated := applyTextEdits(string(old), es)
		if updated == string(old) {
			continue
		}
		changes = append(changes, FileChange{Path: p, Rel: m.relPath(p), Old: string(old), New: updated})
	}
	if len(changes) == 0 {
		return nil, fmt.Errorf("rename produced no edits")
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Rel < changes[j].Rel })
	return changes, nil
}

// applyTextEdits applies non-overlapping LSP text edits to content. Edits are
// applied bottom-up so earlier offsets stay valid.
func applyTextEdits(content string, edits []TextEdit) string {
	type span struct {
		start, end int
		text       string
	}
	spans := make([]span, 0, len(edits))
	for _, e := range edits {
		spans = append(spans, span{
			start: offsetOfPosition(content, e.Range.Start),
			end:   offsetOfPosition(content, e.Range.End),
			text:  e.NewText,
		})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	for _, s := range spans {
		if s.start < 0 || s.end > len(content) || s.start > s.end {
			continue
		}
		content = content[:s.start] + s.text + content[s.end:]
	}
	return content
}

// offsetOfPosition converts an LSP position (zero-based line, UTF-16 column)
// to a byte offset into content. Positions past the end clamp to len(content).
func offsetOfPosition(content string, pos Position) int {
	offset := 0
	for line := 0; line < pos.Line; line++ {
		nl := strings.IndexByte(content[offset:], '\n')
		if nl < 0 {
			return len(content)
		}
		offset += nl + 1
	}
	col := 0
	for i, r := range content[offset:] {
		if col >= pos.Character || r == '\n' {
			return offset + i
		}
		if r > 0xFFFF {
			col += 2 // surrogate pair in UTF-16
		} else {
			col++
		}
	}
	return len(content)
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
	var stopped []string
	var closing []*Client
	pending := m.abandonStartsLocked()
	for key, c := range m.clients {
		if name != "" && key != name {
			continue
		}
		closing = append(closing, c)
		delete(m.clients, key)
		stopped = append(stopped, key)
	}
	if name == "" {
		m.broken = map[string]string{}
	} else {
		delete(m.broken, name)
	}
	m.mu.Unlock()

	// Close waits for the server to exit, which a wedged server can drag out
	// to its full timeout. The clients are already unregistered, so do that
	// waiting outside the lock rather than stalling every other LSP call.
	closeAll(closing)
	waitAll(pending)
	sort.Strings(stopped)
	if len(stopped) == 0 {
		return "no matching running lsp server; it will start on next use"
	}
	return fmt.Sprintf("restarted lsp server(s): %s (respawn on next use)", strings.Join(stopped, ", "))
}

// Shutdown stops every server owned by this workspace and drops the manager
// from the registry, releasing the workspace files the servers hold open. A
// later ForWorkspace(root) builds a fresh manager.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	closing := make([]*Client, 0, len(m.clients))
	for key, c := range m.clients {
		closing = append(closing, c)
		delete(m.clients, key)
	}
	pending := m.abandonStartsLocked()
	root := m.root
	m.mu.Unlock()

	regMu.Lock()
	if reg, ok := registry[root]; ok && reg == m {
		delete(registry, root)
	}
	regMu.Unlock()

	closeAll(closing)
	// A start still in flight would leave a live server behind (holding the
	// workspace open on Windows); it closes its own server once it notices
	// the epoch moved, so wait for that.
	waitAll(pending)
}

// abandonStartsLocked invalidates every in-flight start (each will close the
// server it spawns) and returns their done channels. Callers hold m.mu.
func (m *Manager) abandonStartsLocked() []chan struct{} {
	m.epoch++
	pending := make([]chan struct{}, 0, len(m.starting))
	for key, done := range m.starting {
		pending = append(pending, done)
		delete(m.starting, key)
	}
	return pending
}

func waitAll(chans []chan struct{}) {
	for _, ch := range chans {
		<-ch
	}
}

// ShutdownAll stops every server started in this process. Call it on session
// exit: a live server holds handles on workspace files, which on Windows
// blocks the delete-or-replace done by /update, checkpoint restore and any
// later run in the same directory.
func ShutdownAll() {
	regMu.Lock()
	managers := make([]*Manager, 0, len(registry))
	for _, m := range registry {
		if m != nil {
			managers = append(managers, m)
		}
	}
	regMu.Unlock()

	for _, m := range managers {
		m.Shutdown()
	}
}

// closeAll shuts down clients concurrently: each Close waits on its own
// server, and one slow exit should not be paid serially by the rest.
func closeAll(clients []*Client) {
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Go(c.Close)
	}
	wg.Wait()
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
