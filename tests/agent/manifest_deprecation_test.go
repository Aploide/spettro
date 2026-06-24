package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"spettro/internal/config"
)

// TestManifestParsing_MaxStepsDeprecated verifies that manifests with the
// deprecated max_steps field still parse successfully under
// DisallowUnknownFields(), as the field is kept for backward compatibility.
func TestManifestParsing_MaxStepsDeprecated(t *testing.T) {
	manifestContent := `
version = "1.0"

[[agents]]
id = "test-agent"
name = "Test Agent"
mode = "worker"
max_steps = 32
enabled = true
`
	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "spettro.agents.toml")
	if err := writeFile(manifestPath, manifestContent); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	manifest, err := config.LoadManifest(tmpDir)
	if err != nil {
		t.Fatalf("manifest with deprecated max_steps should parse: %v", err)
	}

	if len(manifest.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(manifest.Agents))
	}

	spec := manifest.Agents[0]
	if spec.ID != "test-agent" {
		t.Errorf("expected agent ID 'test-agent', got %q", spec.ID)
	}
	// MaxSteps field should be populated but ignored by runtime
	if spec.MaxSteps != 32 {
		t.Errorf("expected MaxSteps=32 in parsed struct, got %d", spec.MaxSteps)
	}
}

// TestManifestParsing_NoMaxSteps verifies that manifests without max_steps
// parse successfully and MaxSteps defaults to 0.
func TestManifestParsing_NoMaxSteps(t *testing.T) {
	manifestContent := `
version = "1.0"

[[agents]]
id = "test-agent"
name = "Test Agent"
mode = "worker"
enabled = true
`
	tmpDir := t.TempDir()
	manifestPath := filepath.Join(tmpDir, "spettro.agents.toml")
	if err := writeFile(manifestPath, manifestContent); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	manifest, err := config.LoadManifest(tmpDir)
	if err != nil {
		t.Fatalf("manifest without max_steps should parse: %v", err)
	}

	spec := manifest.Agents[0]
	if spec.MaxSteps != 0 {
		t.Errorf("expected MaxSteps=0 when not specified, got %d", spec.MaxSteps)
	}
}

// writeFile is a helper to write test files.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}
