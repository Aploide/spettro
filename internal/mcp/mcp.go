package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxFileResourceBytes bounds how much a single file-type MCP resource read may
// return, matching the HTTP read cap. It prevents a huge file — or a symlink to
// an unbounded source such as /dev/zero — from exhausting memory.
const maxFileResourceBytes = 2 * 1024 * 1024

type Server struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type"` // "file" or "http"
	EntryPoint  string `json:"entry_point"`
}

type Resource struct {
	ServerID string `json:"server_id"`
	ID       string `json:"id"`
	Title    string `json:"title,omitempty"`
	URI      string `json:"uri,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

type AuthState struct {
	ServerID    string    `json:"server_id"`
	Token       string    `json:"token"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Description string    `json:"description,omitempty"`
}

type serverConfigFile struct {
	Servers []Server `json:"servers"`
}

type authFile struct {
	Auth []AuthState `json:"auth"`
}

func serversPath(cwd string) string {
	return filepath.Join(cwd, ".spettro", "mcp_servers.json")
}

func authPath(cwd string) string {
	return filepath.Join(cwd, ".spettro", "mcp_auth.json")
}

func LoadServers(cwd string) ([]Server, error) {
	raw, err := os.ReadFile(serversPath(cwd))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mcp servers: %w", err)
	}
	var cfg serverConfigFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("decode mcp servers: %w", err)
	}
	out := make([]Server, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		s.ID = strings.TrimSpace(s.ID)
		s.Type = strings.ToLower(strings.TrimSpace(s.Type))
		if s.ID == "" || s.EntryPoint == "" {
			continue
		}
		if s.Type == "" {
			s.Type = "file"
		}
		out = append(out, s)
	}
	return out, nil
}

func SaveAuth(cwd string, state AuthState) error {
	state.ServerID = strings.TrimSpace(state.ServerID)
	state.Token = strings.TrimSpace(state.Token)
	if state.ServerID == "" {
		return fmt.Errorf("server_id is required")
	}
	if state.Token == "" {
		return fmt.Errorf("token is required")
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now()
	}

	path := authPath(cwd)
	// The auth file holds bearer tokens, so keep its directory owner-only
	// (0o700) like the other ~/.spettro secret stores.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	var f authFile
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	replaced := false
	for i := range f.Auth {
		if f.Auth[i].ServerID == state.ServerID {
			f.Auth[i] = state
			replaced = true
			break
		}
	}
	if !replaced {
		f.Auth = append(f.Auth, state)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LoadAuth(cwd string, serverID string) (AuthState, bool, error) {
	raw, err := os.ReadFile(authPath(cwd))
	if err != nil {
		if os.IsNotExist(err) {
			return AuthState{}, false, nil
		}
		return AuthState{}, false, fmt.Errorf("read mcp auth: %w", err)
	}
	var f authFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return AuthState{}, false, fmt.Errorf("decode mcp auth: %w", err)
	}
	for _, a := range f.Auth {
		if a.ServerID == serverID {
			return a, true, nil
		}
	}
	return AuthState{}, false, nil
}

func ListResources(cwd string, serverID string) ([]Resource, error) {
	servers, err := LoadServers(cwd)
	if err != nil {
		return nil, err
	}
	serverID = strings.TrimSpace(serverID)
	var out []Resource
	for _, srv := range servers {
		if serverID != "" && srv.ID != serverID {
			continue
		}
		res, err := listServerResources(cwd, srv)
		if err != nil {
			return nil, err
		}
		out = append(out, res...)
	}
	return out, nil
}

func ReadResource(cwd, serverID, resourceID string) (string, error) {
	servers, err := LoadServers(cwd)
	if err != nil {
		return "", err
	}
	for _, srv := range servers {
		if srv.ID != serverID {
			continue
		}
		switch srv.Type {
		case "http":
			return readHTTPResource(cwd, srv, resourceID)
		default:
			return readFileResource(cwd, srv, resourceID)
		}
	}
	return "", fmt.Errorf("unknown mcp server %q", serverID)
}

func listServerResources(cwd string, srv Server) ([]Resource, error) {
	switch srv.Type {
	case "http":
		return listHTTPResources(cwd, srv)
	default:
		return listFileResources(cwd, srv)
	}
}

func listFileResources(cwd string, srv Server) ([]Resource, error) {
	abs := srv.EntryPoint
	if !filepath.IsAbs(abs) {
		abs = filepath.Clean(filepath.Join(cwd, abs))
	}
	var rows []Resource
	err := filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			return err
		}
		rows = append(rows, Resource{
			ServerID: srv.ID,
			ID:       filepath.ToSlash(rel),
			Title:    d.Name(),
			URI:      path,
			Kind:     "file",
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("list file resources: %w", err)
	}
	return rows, nil
}

func readFileResource(cwd string, srv Server, id string) (string, error) {
	base := srv.EntryPoint
	if !filepath.IsAbs(base) {
		base = filepath.Clean(filepath.Join(cwd, base))
	}
	abs := filepath.Clean(filepath.Join(base, filepath.FromSlash(id)))
	if err := ensureWithinRoot(base, abs); err != nil {
		return "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", fmt.Errorf("read file resource: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxFileResourceBytes+1))
	if err != nil {
		return "", fmt.Errorf("read file resource: %w", err)
	}
	if len(raw) > maxFileResourceBytes {
		return "", fmt.Errorf("resource %q exceeds the %d byte limit", id, maxFileResourceBytes)
	}
	return string(raw), nil
}

// ensureWithinRoot verifies target stays inside root both lexically and after
// resolving symlinks, so a symlink planted inside an MCP file root cannot be
// followed to read files outside it.
func ensureWithinRoot(root, target string) error {
	if !withinRoot(root, target) {
		return fmt.Errorf("resource path outside MCP server root")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil // root not resolvable; lexical check already passed
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return nil // target missing/dangling; os.Open will report it
	}
	if !withinRoot(resolvedRoot, resolvedTarget) {
		return fmt.Errorf("resource path escapes MCP server root via symlink")
	}
	return nil
}

func withinRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func listHTTPResources(cwd string, srv Server) ([]Resource, error) {
	url := strings.TrimRight(srv.EntryPoint, "/") + "/resources"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	addAuthHeader(cwd, srv.ID, req)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp list failed: %s", resp.Status)
	}
	var payload struct {
		Resources []Resource `json:"resources"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512*1024)).Decode(&payload); err != nil {
		return nil, err
	}
	for i := range payload.Resources {
		if payload.Resources[i].ServerID == "" {
			payload.Resources[i].ServerID = srv.ID
		}
	}
	return payload.Resources, nil
}

func readHTTPResource(cwd string, srv Server, id string) (string, error) {
	base, err := url.Parse(strings.TrimRight(srv.EntryPoint, "/"))
	if err != nil {
		return "", fmt.Errorf("mcp: invalid entry point: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", fmt.Errorf("mcp: unsupported entry-point scheme %q", base.Scheme)
	}
	// JoinPath escapes each element and collapses ./ and ../, so a resource id
	// cannot inject a new host, scheme, query, or path-traversal into the URL.
	target := base.JoinPath("resource", id)
	req, err := http.NewRequest(http.MethodGet, target.String(), nil)
	if err != nil {
		return "", err
	}
	addAuthHeader(cwd, srv.ID, req)
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("mcp read failed: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func addAuthHeader(cwd, serverID string, req *http.Request) {
	state, ok, err := LoadAuth(cwd, serverID)
	if err != nil || !ok || strings.TrimSpace(state.Token) == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+state.Token)
}
