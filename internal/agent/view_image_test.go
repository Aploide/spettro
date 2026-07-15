package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/config"
)

// newVisionRuntime builds a minimal runtime for the view-image tool with a
// visionCheck seam simulating a vision-capable model.
func newVisionRuntime(t *testing.T) *toolRuntime {
	t.Helper()
	return &toolRuntime{
		cwd:          t.TempDir(),
		permission:   config.PermissionYOLO,
		toolPolicies: map[string]config.ToolSpec{},
		readSet:      map[string]struct{}{},
		visionCheck:  func() bool { return true },
	}
}

func TestViewImageAttaches(t *testing.T) {
	r := newVisionRuntime(t)
	img := filepath.Join(r.cwd, "shots", "home.png")
	if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, sink := withImageSink(context.Background())
	out, err := r.runViewImage(ctx, []byte(`{"path":"shots/home.png"}`))
	if err != nil {
		t.Fatalf("runViewImage: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if parsed["attached"] != true || parsed["media_type"] != "image/png" || parsed["file"] != "shots/home.png" {
		t.Fatalf("unexpected output: %v", parsed)
	}
	if imgs := sink.list(); len(imgs) != 1 || imgs[0] != img {
		t.Fatalf("sink = %v, want [%s]", imgs, img)
	}
	if _, ok := r.readSet["shots/home.png"]; !ok {
		t.Fatal("view-image should mark the file as read")
	}
}

func TestViewImageErrors(t *testing.T) {
	r := newVisionRuntime(t)
	if err := os.WriteFile(filepath.Join(r.cwd, "big.png"), make([]byte, maxAttachBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.cwd, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.cwd, "empty.png"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, args := range map[string]string{
		"missing path":   `{}`,
		"missing file":   `{"path":"nope.png"}`,
		"not an image":   `{"path":"notes.txt"}`,
		"empty image":    `{"path":"empty.png"}`,
		"unknown field":  `{"path":"big.png","zoom":2}`,
		"path escape":    `{"path":"../../etc/passwd.png"}`,
		"oversize image": `{"path":"big.png"}`,
	} {
		if _, err := r.runViewImage(ctx, []byte(args)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}

	// Without vision the tool refuses outright: there is nothing useful it
	// could do, and the error tells the model why.
	img := filepath.Join(r.cwd, "ok.png")
	if err := os.WriteFile(img, []byte{1}, 0o644); err != nil {
		t.Fatal(err)
	}
	r.visionCheck = func() bool { return false }
	if _, err := r.runViewImage(ctx, []byte(`{"path":"ok.png"}`)); err == nil || !strings.Contains(err.Error(), "vision") {
		t.Fatalf("expected vision error, got %v", err)
	}
}

func TestImageSinkIsPerCall(t *testing.T) {
	ctx1, s1 := withImageSink(context.Background())
	ctx2, s2 := withImageSink(context.Background())
	attachResultImage(ctx1, "/a.png")
	attachResultImage(ctx2, "/b.png")
	if got := s1.list(); len(got) != 1 || got[0] != "/a.png" {
		t.Fatalf("sink1 = %v", got)
	}
	if got := s2.list(); len(got) != 1 || got[0] != "/b.png" {
		t.Fatalf("sink2 = %v", got)
	}
	// Attaching without a sink must be a silent no-op.
	attachResultImage(context.Background(), "/c.png")
}
