package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/agent"
)

func TestSlugifyPrompt(t *testing.T) {
	cases := map[string]string{
		"A simple logo!":                   "a-simple-logo",
		"  Lots   of   spaces  ":           "lots-of-spaces",
		"!!!@@@###":                        "",
		strings.Repeat("a", 200):           strings.Repeat("a", 60),
		"Mix of UPPER, lower & 123 digits": "mix-of-upper-lower-123-digits",
	}
	for in, want := range cases {
		if got := agent.SlugifyPromptForTesting(in); got != want {
			t.Errorf("slugifyPrompt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickExtension(t *testing.T) {
	cases := []struct {
		mime, kind, want string
	}{
		{"image/png", "image", ".png"},
		{"image/jpeg", "image", ".jpg"},
		{"image/jpg", "image", ".jpg"},
		{"image/webp", "image", ".webp"},
		{"video/mp4", "video", ".mp4"},
		{"video/webm", "video", ".webm"},
		{"", "image", ".png"},
		{"", "video", ".mp4"},
		{"application/octet-stream", "image", ".png"},
		{"application/octet-stream", "video", ".mp4"},
	}
	for _, tc := range cases {
		if got := agent.PickExtensionForTesting(tc.mime, tc.kind); got != tc.want {
			t.Errorf("pickExtension(%q, %q) = %q, want %q", tc.mime, tc.kind, got, tc.want)
		}
	}
}

func TestResolveMediaPath_DefaultsToAssetsWhenNoPath(t *testing.T) {
	cwd := t.TempDir()
	dir, base, ext, hasExt, dirOnly := agent.ResolveMediaPathForTesting(cwd, "", "Hello World", "image")
	if !strings.HasSuffix(dir, filepath.Join(cwd, "assets")) {
		t.Fatalf("expected default assets/ folder, got %q", dir)
	}
	if base != "hello-world" {
		t.Fatalf("expected slug base, got %q", base)
	}
	if hasExt {
		t.Fatalf("expected hasExt=false for auto-named file")
	}
	if !dirOnly {
		t.Fatalf("expected dirOnly=true for auto-named file")
	}
	if ext != "" {
		t.Fatalf("expected empty fixed ext, got %q", ext)
	}
}

func TestResolveMediaPath_PreservesExplicitFilename(t *testing.T) {
	cwd := t.TempDir()
	dir, base, ext, hasExt, dirOnly := agent.ResolveMediaPathForTesting(cwd, "public/logo.png", "A round logo", "image")
	if !strings.HasSuffix(dir, filepath.Join(cwd, "public")) {
		t.Fatalf("expected public dir, got %q", dir)
	}
	if base != "logo" {
		t.Fatalf("expected base=logo, got %q", base)
	}
	if ext != ".png" {
		t.Fatalf("expected fixed .png, got %q", ext)
	}
	if !hasExt {
		t.Fatalf("expected hasExt=true")
	}
	if dirOnly {
		t.Fatalf("expected dirOnly=false")
	}
}

func TestResolveMediaPath_TrailingSlashIsDirectory(t *testing.T) {
	cwd := t.TempDir()
	dir, base, _, hasExt, dirOnly := agent.ResolveMediaPathForTesting(cwd, "static/icons/", "ICON for app", "image")
	if !strings.HasSuffix(dir, filepath.Join(cwd, "static/icons")) {
		t.Fatalf("expected static/icons dir, got %q", dir)
	}
	if base != "icon-for-app" {
		t.Fatalf("expected slugged base, got %q", base)
	}
	if hasExt {
		t.Fatalf("expected hasExt=false for directory path")
	}
	if !dirOnly {
		t.Fatalf("expected dirOnly=true for directory path")
	}
}

func TestResolveMediaPath_ExistingDirectoryTreatedAsDir(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "media"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir, _, _, hasExt, dirOnly := agent.ResolveMediaPathForTesting(cwd, "media", "Test", "image")
	if !strings.HasSuffix(dir, filepath.Join(cwd, "media")) {
		t.Fatalf("expected existing dir resolved as dir, got %q", dir)
	}
	if hasExt || !dirOnly {
		t.Fatalf("expected (hasExt=false, dirOnly=true), got (%v, %v)", hasExt, dirOnly)
	}
}

func TestResolveMediaPath_FallsBackToTimestampWhenSlugEmpty(t *testing.T) {
	cwd := t.TempDir()
	_, base, _, _, _ := agent.ResolveMediaPathForTesting(cwd, "", "!!!@@@", "image")
	if !strings.HasPrefix(base, "image-") {
		t.Fatalf("expected fallback base starting with image-, got %q", base)
	}
}

func TestDefaultMediaDirFor_DetectsNextJSViaConfig(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "next.config.js"), []byte("module.exports = {}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if dir := agent.DefaultMediaDirForTesting(cwd); dir != "public" {
		t.Fatalf("expected public, got %q", dir)
	}
}

func TestDefaultMediaDirFor_DetectsNextJSViaPackageJSON(t *testing.T) {
	cwd := t.TempDir()
	pkg := `{
		"name": "site",
		"dependencies": {"next": "^15.0.0"}
	}`
	if err := os.WriteFile(filepath.Join(cwd, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if dir := agent.DefaultMediaDirForTesting(cwd); dir != "public" {
		t.Fatalf("expected public, got %q", dir)
	}
}

func TestDefaultMediaDirFor_DefaultsToAssetsOtherwise(t *testing.T) {
	cwd := t.TempDir()
	if dir := agent.DefaultMediaDirForTesting(cwd); dir != "assets" {
		t.Fatalf("expected assets, got %q", dir)
	}
}

func TestIsNextJSProject_FalseForRegularGoProject(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "go.mod"), []byte("module foo"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if agent.IsNextJSProjectForTesting(cwd) {
		t.Fatal("Go project must not be detected as Next.js")
	}
}

func TestIsNextJSProject_TrueWhenInDevDependencies(t *testing.T) {
	cwd := t.TempDir()
	pkg := `{"devDependencies": {"next": "^14.0.0"}}`
	if err := os.WriteFile(filepath.Join(cwd, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !agent.IsNextJSProjectForTesting(cwd) {
		t.Fatal("expected Next.js detection from devDependencies")
	}
}
