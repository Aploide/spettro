//go:build unix

package tui

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"spettro/internal/pty"
)

// TestRenderPtyLiveTail verifies a running pty tool call renders the live
// scrollback tail of the session it drives.
func TestRenderPtyLiveTail(t *testing.T) {
	sess, err := pty.Default().Start(exec.Command("sh", "-c", "echo tail-marker; sleep 5"), "echo tail-marker", 0, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer func() { _ = pty.Default().Kill(sess.ID) }()

	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(sess.Scrollback(), "tail-marker") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	args := fmt.Sprintf(`{"id":%q,"input":""}`, sess.ID)
	tail := renderPtyLiveTail("pty-write", args, false)
	if !strings.Contains(strings.Join(tail, "\n"), "tail-marker") {
		t.Fatalf("live tail missing session output, got %q", tail)
	}

	out := renderToolGroups([]ToolItem{{Name: "pty-write", Status: "running", Args: args}}, false, false, colorHeaderBg)
	if !strings.Contains(out, "tail-marker") {
		t.Fatalf("renderToolGroups missing live tail, got %q", out)
	}

	if tail := renderPtyLiveTail("pty-write", `{"id":"pty-none"}`, false); tail != nil {
		t.Fatalf("unknown session should render no tail, got %q", tail)
	}
	if tail := renderPtyLiveTail("shell-exec", args, false); tail != nil {
		t.Fatalf("non-pty tool should render no tail, got %q", tail)
	}
}
