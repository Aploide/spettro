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
	core := []string{"file-read", "file-write", "file-edit", "glob", "grep", "repo-search", "shell-exec", "bash", "ls"}
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
	for _, name := range []string{"shell-exec", "bash", "bash-output"} {
		p, ok := decodeToolSchema(t, name).Properties["timeout"]
		if !ok || p["type"] != "integer" {
			t.Errorf("%s schema must declare an integer timeout, got %v", name, p)
		}
	}
}

func TestCoreToolDescriptionsStateTheirContracts(t *testing.T) {
	cases := map[string][]string{
		"file-edit":  {"read the file first", "exactly one location", "replace_all", "line-number prefix"},
		"file-write": {"read it", "prefer file-edit"},
		"file-read":  {"line number", "cat -n", "offset", "2000 lines", "40,000"},
		"grep":       {"RE2", "path", "max_results"},
		"glob":       {"**/*.go"},
		"shell-exec": {"timeout", "run_in_background", "fresh process", "file-read"},
	}
	for name, needles := range cases {
		desc := builtinNativeToolDescs[name]
		for _, n := range needles {
			if !strings.Contains(desc, n) {
				t.Errorf("%s description should mention %q", name, n)
			}
		}
	}
	if builtinNativeToolDescs["bash"] != builtinNativeToolDescs["shell-exec"] {
		t.Error("bash and shell-exec are the same tool and should share a description")
	}
	if !strings.HasPrefix(builtinNativeToolDescs["comment"], "Optional") {
		t.Error("comment must read as optional so models don't spend steps narrating")
	}
}
