package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// SettleMs overrides how long post-edit diagnostics keep listening
	// after the server's first publish about the edit (see settlePolicy):
	// the quiet window, or for a staged server the ceiling. nil keeps the
	// built-in value.
	SettleMs *int `json:"settle_ms,omitempty"`
}

func (sc ServerConfig) enabled() bool { return sc.Enabled == nil || *sc.Enabled }

// Config is the on-disk shape of .spettro/lsp.json.
type Config struct {
	Servers map[string]ServerConfig `json:"servers"`
}

// builtinServer lists candidate commands for a server key; the first one found
// on PATH wins, so e.g. python works with either pyright or pylsp installed.
// stages is set for servers that publish several sets per change (see
// settlePolicy).
type builtinServer struct {
	candidates []ServerConfig
	filetypes  []string
	stages     int
}

// fastSettleMs is the quiet window of servers that publish one complete set
// per change: a follow-up publish, if any, comes within milliseconds. (gopls,
// pyright and clangd also version their publishes, which ends the wait at
// once; the window covers the unversioned case.) Measured with gopls: a Go
// file-edit spent 300 ms of its 344 ms in the old one-size window.
var fastSettleMs = 30

var builtinServers = map[string]builtinServer{
	"go": {candidates: []ServerConfig{{Command: "gopls", SettleMs: &fastSettleMs}}, filetypes: []string{".go"}},
	// typescript-language-server publishes syntactic diagnostics first and
	// the semantic (type) errors in a later publish: wait for the second.
	"typescript": {candidates: []ServerConfig{{Command: "typescript-language-server", Args: []string{"--stdio"}}}, filetypes: []string{".ts", ".tsx", ".js", ".jsx"}, stages: 2},
	"python":     {candidates: []ServerConfig{{Command: "pyright-langserver", Args: []string{"--stdio"}, SettleMs: &fastSettleMs}, {Command: "pylsp"}}, filetypes: []string{".py"}},
	"rust":       {candidates: []ServerConfig{{Command: "rust-analyzer"}}, filetypes: []string{".rs"}},
	"c":          {candidates: []ServerConfig{{Command: "clangd", SettleMs: &fastSettleMs}}, filetypes: []string{".c", ".h"}},
	"cpp":        {candidates: []ServerConfig{{Command: "clangd", SettleMs: &fastSettleMs}}, filetypes: []string{".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx"}},
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
				// no command in the override: keep the detected one (and
				// whatever an earlier file set), overriding only the fields
				// this entry sets. So {"python":{"settle_ms":200}} in the
				// project keeps {"python":{"enabled":false}} from home.
				if base, ok := cfg.Servers[key]; ok {
					if sc.Enabled != nil {
						base.Enabled = sc.Enabled
					}
					if len(sc.Filetypes) > 0 {
						base.Filetypes = sc.Filetypes
					}
					if sc.SettleMs != nil {
						base.SettleMs = sc.SettleMs
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

// errShutDown is returned by a Manager after Shutdown: it starts nothing more.
var errShutDown = errors.New("lsp servers for this workspace were shut down")

// Manager owns the lazily started language servers for one workspace root.
type Manager struct {
	root string
	cfg  Config

	mu       sync.Mutex
	clients  map[string]*Client      // server key → running client
	broken   map[string]string       // server key → start failure (until an lsp restart)
	starting map[string]*serverStart // server key → the start in flight
	// shutDown is set by Shutdown. The manager is out of the registry by
	// then, so a server it started afterwards would never be closed.
	shutDown bool
}

// serverStart is one background server start. Restart and Shutdown abandon it
// by removing it from Manager.starting and cancelling it; done closes once
// the start has finished and, if abandoned, closed whatever it spawned.
type serverStart struct {
	done   chan struct{}
	cancel context.CancelFunc
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
		starting: map[string]*serverStart{},
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
// an lsp restart (Restart) clears the mark.
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
	if m.shutDown {
		return nil, nil, errShutDown
	}
	if c, ok := m.clients[key]; ok && c.alive() {
		return c, nil, nil
	}
	if reason, bad := m.broken[key]; bad {
		return nil, nil, fmt.Errorf("lsp server %q unavailable: %s (use the lsp tool, op restart, to retry)", key, reason)
	}
	if st, ok := m.starting[key]; ok {
		return nil, st.done, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), serverStartTimeout)
	st := &serverStart{done: make(chan struct{}), cancel: cancel}
	m.starting[key] = st
	go m.start(ctx, key, m.cfg.Servers[key], st)
	return nil, st.done, nil
}

// start spawns one server and registers the outcome, unless the start was
// abandoned meanwhile (Restart, Shutdown): then the new server is closed
// before done is, so whoever waits on done knows it is gone.
func (m *Manager) start(ctx context.Context, key string, sc ServerConfig, st *serverStart) {
	defer close(st.done)
	defer st.cancel()
	// A user-configured command gets the same install-location fallback as
	// the auto-detected ones.
	command := sc.Command
	if found, ok := FindServerBinary(command, m.root); ok {
		command = found
	}
	c, err := startClient(ctx, m.root, command, sc.Args, serverSettings(command))
	m.mu.Lock()
	abandoned := m.starting[key] != st
	if !abandoned {
		delete(m.starting, key)
		if err != nil {
			m.broken[key] = err.Error()
		} else {
			m.clients[key] = c
		}
	}
	m.mu.Unlock()
	if abandoned && c != nil {
		c.Close()
	}
}

// serverSettings returns the initializationOptions for a server command.
//
// gopls diagnoses a change in two passes by default: the changed package at
// once, and its reverse dependencies only after diagnosticsDelay (1s). That
// suits a person typing, but a post-edit check that stops listening after the
// first pass would report a signature change as clean while the callers in
// other packages no longer compile, and keep reporting callers as broken
// after the signature is restored. With no delay gopls does one full pass
// per change, so the edited file's publish comes with every other file's.
func serverSettings(command string) map[string]any {
	base := filepath.Base(command)
	if strings.TrimSuffix(base, filepath.Ext(base)) == "gopls" {
		return map[string]any{"diagnosticsDelay": "0s"}
	}
	return nil
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

// syncFromDisk hands the server absPath's current on-disk content, after first
// bringing the documents it already holds open back in line with the disk and
// sending the also files (other files the caller just changed, such as the
// rest of a rename, that the server may not have open yet), so whatever it
// reports next describes the workspace as it is. It returns the synced
// document and the content sent. ctx bounds every write to the server: a
// server that stopped reading its input is abandoned at the deadline
// instead of blocking the caller (see Client.write).
func (m *Manager) syncFromDisk(ctx context.Context, c *Client, key, absPath string, also ...string) (doc, string, error) {
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return doc{}, "", err
	}
	c.resyncOpen(ctx, fileURI(absPath))
	for _, p := range also {
		p = realPath(p)
		if p == absPath {
			continue
		}
		if k, ok := m.serverKeyFor(p); !ok || k != key {
			continue
		}
		if other, err := os.ReadFile(p); err == nil {
			if d, err := c.syncFile(ctx, p, languageIDForPath(p, key), string(other)); err == nil {
				_ = c.didSave(ctx, d, string(other))
			}
		}
	}
	content := string(raw)
	d, err := c.syncFile(ctx, absPath, languageIDForPath(absPath, key), content)
	return d, content, err
}

// DiagnosticsForFile syncs the file's current on-disk content to the server
// and waits (bounded by ctx) for fresh diagnostics. Empty string means clean.
func (m *Manager) DiagnosticsForFile(ctx context.Context, absPath string) (string, error) {
	absPath = realPath(absPath)
	c, key, err := m.clientFor(ctx, absPath)
	if err != nil {
		return "", err
	}
	d, _, err := m.syncFromDisk(ctx, c, key, absPath)
	if err != nil {
		return "", err
	}
	ds := c.waitDiagnostics(ctx, d)
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
	d, content, err := m.syncFromDisk(ctx, c, key, absPath)
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
	d, content, err := m.syncFromDisk(ctx, c, key, absPath)
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
	d, content, err := m.syncFromDisk(ctx, c, key, absPath)
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
	var closing []*Client
	pending, stopped := m.abandonStartsLocked(name)
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
	// The abandoned starts were cancelled, so their wait is short too.
	closeAll(closing)
	waitAll(pending)
	sort.Strings(stopped)
	stopped = slices.Compact(stopped) // a dead client and its replacement's start
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
	m.shutDown = true
	closing := make([]*Client, 0, len(m.clients))
	for key, c := range m.clients {
		closing = append(closing, c)
		delete(m.clients, key)
	}
	pending, _ := m.abandonStartsLocked("")
	root := m.root
	m.mu.Unlock()

	regMu.Lock()
	if reg, ok := registry[root]; ok && reg == m {
		delete(registry, root)
	}
	regMu.Unlock()

	closeAll(closing)
	// A start still in flight would leave a live server behind (holding the
	// workspace open on Windows). It was cancelled and closes whatever it
	// spawned on its way out, so this wait is short.
	waitAll(pending)
}

// abandonStartsLocked cancels the in-flight start of server name (every
// server's when name is empty); each closes whatever it spawned and does not
// register it. It returns their done channels and server keys. Callers hold
// m.mu.
func (m *Manager) abandonStartsLocked(name string) ([]chan struct{}, []string) {
	var pending []chan struct{}
	var keys []string
	for key, st := range m.starting {
		if name != "" && key != name {
			continue
		}
		st.cancel()
		pending = append(pending, st.done)
		keys = append(keys, key)
		delete(m.starting, key)
	}
	return pending, keys
}

func waitAll(chans []chan struct{}) {
	for _, ch := range chans {
		<-ch
	}
}

// ShutdownUnder stops the servers of every workspace at or below dir, such as
// a sub-agent's worktree that is about to be removed: nothing else would stop
// them before the process exits, and a live server keeps the directory busy.
func ShutdownUnder(dir string) {
	dir = realPath(dir)
	regMu.Lock()
	var managers []*Manager
	for root, m := range registry {
		if _, ok := relTo(dir, root); !ok {
			continue
		}
		if m != nil {
			managers = append(managers, m)
		}
		delete(registry, root)
	}
	regMu.Unlock()

	for _, m := range managers {
		m.Shutdown()
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
