package tui_test

import (
	"path/filepath"
	"strings"
	"testing"

	"spettro/internal/tui"
)

// TestParseMediaTraceOutput_ExtractsFilesAndPrompt covers the JSON envelope
// the dispatcher receives from grok-image / grok-video on success. The
// shape is `{"tool":"...","prompt":"...","files":[...]}` — the parser
// pulls out the file list and (truncated) prompt and ignores everything
// else.
func TestParseMediaTraceOutput_ExtractsFilesAndPrompt(t *testing.T) {
	out := `{"tool":"grok-image","model":"grok-imagine-image-quality","prompt":"A blue cat","count":2,"files":["assets/cat-1.png","assets/cat-2.png"]}`
	files, prompt := tui.ParseMediaTraceOutputForTesting(out)
	if len(files) != 2 || files[0] != "assets/cat-1.png" || files[1] != "assets/cat-2.png" {
		t.Fatalf("file list mismatch: %v", files)
	}
	if prompt != "A blue cat" {
		t.Fatalf("prompt mismatch: %q", prompt)
	}
}

func TestParseMediaTraceOutput_HandlesEmptyOrInvalid(t *testing.T) {
	cases := []string{
		"",
		"  ",
		"not json",
		`{"tool":"grok-image"}`,            // no files
		`{"tool":"grok-image","files":[]}`, // empty files
		`{"tool":"grok-image","files":["","   "]}`, // all blank entries
		`{"files":["x"`, // truncated JSON
	}
	for _, c := range cases {
		files, prompt := tui.ParseMediaTraceOutputForTesting(c)
		if len(files) != 0 {
			t.Errorf("expected empty files for %q, got %v", c, files)
		}
		_ = prompt
	}
}

// TestMediaCaption_FormatsByTool checks the per-tool emoji prefix and the
// truncation behaviour on long prompts.
func TestMediaCaption_FormatsByTool(t *testing.T) {
	if got := tui.MediaCaptionForTesting("grok-image", "  a cute logo  "); got != "▣ a cute logo" {
		t.Fatalf("image caption: %q", got)
	}
	if got := tui.MediaCaptionForTesting("grok-video", "a panning shot"); got != "▶ a panning shot" {
		t.Fatalf("video caption: %q", got)
	}
	if got := tui.MediaCaptionForTesting("grok-image", ""); got != "" {
		t.Fatalf("empty prompt should produce no caption, got %q", got)
	}
	long := strings.Repeat("p", 1024)
	caption := tui.MediaCaptionForTesting("grok-image", long)
	if !strings.HasPrefix(caption, "▣") {
		t.Fatal("missing emoji prefix on long caption")
	}
	if len([]rune(caption)) > 500 {
		t.Fatalf("caption preview too long: %d runes", len([]rune(caption)))
	}
}

// TestMediaAbsolutePath_ResolvesRelativeAgainstCwd asserts the same path
// rules the dispatcher uses to find generated assets on disk.
func TestMediaAbsolutePath_ResolvesRelativeAgainstCwd(t *testing.T) {
	cwd := "/tmp/project"
	if got := tui.MediaAbsolutePathForTesting(cwd, "assets/foo.png"); got != filepath.Join(cwd, "assets", "foo.png") {
		t.Fatalf("relative resolution: %q", got)
	}
	if got := tui.MediaAbsolutePathForTesting(cwd, "/abs/path/foo.png"); got != "/abs/path/foo.png" {
		t.Fatalf("absolute path should pass through: %q", got)
	}
	if got := tui.MediaAbsolutePathForTesting("", "x.png"); got != "x.png" {
		t.Fatalf("empty cwd should return path as-is: %q", got)
	}
	if got := tui.MediaAbsolutePathForTesting(cwd, "   "); got != "" {
		t.Fatalf("blank path should return empty: %q", got)
	}
}
