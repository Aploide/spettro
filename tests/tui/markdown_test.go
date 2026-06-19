package tui_test

import (
	"strings"
	"testing"

	"spettro/internal/tui"
)

func TestRenderMarkdown_StripsBasicMarkdownMarkers(t *testing.T) {
	md := "# Title\n- **bold** item\n1. `code`\n[site](https://example.com)"
	out := tui.RenderMarkdownForTesting(md, 80)

	if strings.Contains(out, "**") {
		t.Fatalf("expected bold markers to be rendered, got %q", out)
	}
	if strings.Contains(out, "`code`") {
		t.Fatalf("expected code span markers to be rendered, got %q", out)
	}
	if strings.Contains(out, "[site](https://example.com)") {
		t.Fatalf("expected link markdown to be rendered, got %q", out)
	}
	if !strings.Contains(out, "•") {
		t.Fatalf("expected list bullet in output, got %q", out)
	}
}

func TestRenderMarkdown_RendersCodeFenceContent(t *testing.T) {
	md := "before\n```go\nfmt.Println(\"hi\")\n```\nafter"
	out := tui.RenderMarkdownForTesting(md, 80)

	if !strings.Contains(out, "fmt.Println(\"hi\")") {
		t.Fatalf("expected fenced code content in output, got %q", out)
	}
	if strings.Contains(out, "```") {
		t.Fatalf("expected fence markers removed, got %q", out)
	}
}

func TestRenderMarkdown_RendersTable(t *testing.T) {
	md := "| Name | Age |\n|------|-----|\n| Alice | 30 |\n| Bob | 25 |"
	out := tui.RenderMarkdownForTesting(md, 80)

	if strings.Contains(out, "|---") {
		t.Fatalf("expected separator row to be consumed, got %q", out)
	}
	if !strings.Contains(out, "Name") || !strings.Contains(out, "Age") {
		t.Fatalf("expected header cells in output, got %q", out)
	}
	if !strings.Contains(out, "Alice") || !strings.Contains(out, "Bob") {
		t.Fatalf("expected data cells in output, got %q", out)
	}
	if !strings.Contains(out, "┌") || !strings.Contains(out, "┘") {
		t.Fatalf("expected box-drawing borders in output, got %q", out)
	}
}

func TestPrefixBlockWithBullet_IndentsFollowingLines(t *testing.T) {
	out := tui.PrefixBlockWithBulletForTesting("  ●", "line one\nline two")
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], "  ● ") {
		t.Fatalf("expected first line prefixed with bullet, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "    ") {
		t.Fatalf("expected second line indented, got %q", lines[1])
	}
}
