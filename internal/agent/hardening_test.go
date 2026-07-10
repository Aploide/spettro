package agent

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spettro/internal/config"
)

func TestIsPublicIP(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":         true,
		"1.1.1.1":         true,
		"127.0.0.1":       false,
		"::1":             false,
		"169.254.169.254": false, // cloud metadata
		"10.0.0.1":        false,
		"192.168.1.1":     false,
		"172.16.0.1":      false,
		"0.0.0.0":         false,
		"fe80::1":         false,
	}
	for s, want := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if got := isPublicIP(ip); got != want {
			t.Errorf("isPublicIP(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestValidatePublicURL(t *testing.T) {
	for _, ok := range []string{"http://example.com", "https://example.com/path?q=1"} {
		if err := validatePublicURL(ok); err != nil {
			t.Errorf("validatePublicURL(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "ftp://host/x", "", "://nohost", "https://"} {
		if err := validatePublicURL(bad); err == nil {
			t.Errorf("validatePublicURL(%q) = nil, want error", bad)
		}
	}
}

func TestSafeHTTPClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// srv.URL is http://127.0.0.1:<port>; the safe client must refuse to dial it.
	if _, err := newSafeHTTPClient(5 * time.Second).Get(srv.URL); err == nil {
		t.Fatalf("expected loopback dial to be blocked, got nil error")
	}
}

func TestWriteMediaFileContainment(t *testing.T) {
	cwd := t.TempDir()
	r := &toolRuntime{cwd: cwd, readSet: map[string]struct{}{}}
	item := grokImageData{B64JSON: base64.StdEncoding.EncodeToString([]byte("payload"))}

	// Escaping the workspace must be refused.
	outside := filepath.Join(filepath.Dir(cwd), "evil.png")
	if _, err := r.writeMediaFile(context.Background(), outside, item); err == nil ||
		!strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("expected containment error, got %v", err)
	}

	// A path inside the workspace succeeds and returns a relative path.
	rel, err := r.writeMediaFile(context.Background(), filepath.Join(cwd, "assets", "ok.png"), item)
	if err != nil {
		t.Fatalf("contained write failed: %v", err)
	}
	if rel != "assets/ok.png" {
		t.Fatalf("unexpected rel path %q", rel)
	}
}

func TestAuthorizeWriteAccess(t *testing.T) {
	ctx := context.Background()
	policy := map[string]config.ToolSpec{"file-write": {RequiresApproval: true}}

	// Policy requires approval and the user denies → error.
	denied := &toolRuntime{
		permission:   config.PermissionAskFirst,
		toolPolicies: policy,
		shellApproval: func(context.Context, ShellApprovalRequest) (ShellApprovalDecision, error) {
			return ShellApprovalDeny, nil
		},
	}
	if err := denied.authorizeWriteAccess(ctx, "file-write", "x.go", ""); err == nil {
		t.Fatal("expected denial error")
	}

	// Policy requires approval and the user allows → nil.
	allowed := &toolRuntime{
		permission:   config.PermissionAskFirst,
		toolPolicies: policy,
		shellApproval: func(context.Context, ShellApprovalRequest) (ShellApprovalDecision, error) {
			return ShellApprovalAllowOnce, nil
		},
	}
	if err := allowed.authorizeWriteAccess(ctx, "file-write", "x.go", ""); err != nil {
		t.Fatalf("expected approval to pass, got %v", err)
	}

	// No policy / approval not required → never prompts (callback must not run).
	open := &toolRuntime{
		permission:   config.PermissionAskFirst,
		toolPolicies: map[string]config.ToolSpec{},
		shellApproval: func(context.Context, ShellApprovalRequest) (ShellApprovalDecision, error) {
			t.Fatal("approval should not be requested when policy does not require it")
			return ShellApprovalDeny, nil
		},
	}
	if err := open.authorizeWriteAccess(ctx, "file-write", "x.go", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// YOLO bypasses approval even when the policy requires it.
	yolo := &toolRuntime{permission: config.PermissionYOLO, toolPolicies: policy}
	if err := yolo.authorizeWriteAccess(ctx, "file-write", "x.go", ""); err != nil {
		t.Fatalf("yolo should bypass write approval, got %v", err)
	}
}
