package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

type testSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]map[string]any `json:"properties"`
	Required   []string                  `json:"required"`
}

func decodeToolSchema(t *testing.T, name string) testSchema {
	t.Helper()
	raw, ok := builtinNativeToolSchemas[name]
	if !ok {
		t.Fatalf("no schema for %s", name)
	}
	var s testSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s schema is not valid JSON: %v", name, err)
	}
	return s
}

func TestBuiltinToolSchemasAreValidObjects(t *testing.T) {
	for name := range builtinNativeToolSchemas {
		s := decodeToolSchema(t, name)
		if s.Type != "object" {
			t.Errorf("%s: schema type %q, want object", name, s.Type)
		}
		for _, req := range s.Required {
			if _, ok := s.Properties[req]; !ok {
				t.Errorf("%s: required %q is not a declared property", name, req)
			}
		}
		if _, ok := builtinNativeToolDescs[name]; !ok {
			t.Errorf("%s: schema without description", name)
		}
	}
}

// TestCoreToolParamsDocumented requires every parameter of the core file,
// search and shell tools to carry a description, and pins grep path and the
// shell timeout in seconds (tool_path_timeout_test.go covers their behavior).
func TestCoreToolParamsDocumented(t *testing.T) {
	core := []string{"file-read", "file-write", "file-edit", "glob", "grep", "bash", "todo-write"}
	for _, name := range core {
		for prop, def := range decodeToolSchema(t, name).Properties {
			if d, _ := def["description"].(string); strings.TrimSpace(d) == "" {
				t.Errorf("%s.%s has no description", name, prop)
			}
		}
	}
	if _, ok := decodeToolSchema(t, "grep").Properties["path"]; !ok {
		t.Error("grep schema must declare path")
	}
	if p, ok := decodeToolSchema(t, "bash").Properties["timeout"]; !ok || p["type"] != "integer" {
		t.Errorf("bash schema must declare an integer timeout, got %v", p)
	}
}

func TestCoreToolDescriptionsStateTheirContracts(t *testing.T) {
	cases := map[string][]string{
		"file-edit":  {"read the file first", "refused if the file changed", "except by your own foreground bash commands", "byte for byte", "exactly one location", "replace_all", "line-number prefix", "edits[]", "Cannot create files"},
		"file-write": {"refused unless you read it", "prefer file-edit"},
		"file-read":  {"line number", "cat -n", "offset", "2000 lines", "60,000", "before editing or overwriting"},
		"grep":       {"RE2", "path", "max_results", "default 200", "symbol", "case-insensitive literal", "don't apply", "file names at any depth"},
		"glob":       {"**/*.go", "only top-level files", "at most 1000", "Without a pattern"},
		"bash":       {"timeout", "max 600", "run_in_background", "fresh process", "file-read", "[exit status N]", "30,000", "tool-output", "pty-start"},
		"todo-write": {"merge=true", "delete", "clear_completed", "no arguments", "unknown ids are dropped", "cannot start or complete before"},
	}
	for name, needles := range cases {
		desc := builtinNativeToolDescs[name]
		for _, n := range needles {
			if !strings.Contains(desc, n) {
				t.Errorf("%s description should mention %q", name, n)
			}
		}
	}
	// Long tool descriptions measurably inflated the model's reasoning in a
	// benchmark replay; keep the core ones down to their contracts.
	for _, name := range []string{"file-read", "file-write", "file-edit", "glob", "grep", "bash", "todo-write", "ask-user"} {
		if desc, _ := toolDescription(name); len(desc) > 900 {
			t.Errorf("%s description is %d characters; keep it under 900", name, len(desc))
		}
	}
	if !strings.HasPrefix(builtinNativeToolDescs["comment"], "Optional") {
		t.Error("comment must read as optional so models don't spend steps narrating")
	}
}
