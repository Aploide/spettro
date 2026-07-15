package config

import (
	"strings"
	"testing"
)

// minimalManifestTOML builds a decodable manifest with the given header lines
// injected into the [runtime] table.
func minimalManifestTOML(version int, runtimeLines string) string {
	return `
version = ` + itoa(version) + `
default_agent = "plan"

[runtime]
default_permission = "ask-first"
default_timeout_sec = 120
` + runtimeLines + `

[[tools]]
id = "shell-exec"
name = "Shell"
kind = "builtin"
enabled = true
timeout_sec = 120
permitted_actions = ["execute"]

[[agents]]
id = "plan"
name = "Plan"
mode = "orchestrator"
allowed_tools = ["shell-exec"]
permission = "ask-first"
max_steps = 10
enabled = true
`
}

func itoa(n int) string {
	return string(rune('0' + n))
}

func TestMigrationRewritesInertWorkspaceWrite(t *testing.T) {
	// Pre-v3 manifests carry a tool-injected, never-enforced
	// sandbox_mode = "workspace-write"; v3 must neutralize it.
	m, _, changed, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(
		minimalManifestTOML(2, `sandbox_mode = "workspace-write"`)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !changed {
		t.Fatal("v2 manifest must be marked changed by the v3 migration")
	}
	if m.Version != 5 {
		t.Fatalf("version = %d, want 5 (v5 vision-tools migration)", m.Version)
	}
	if m.Runtime.SandboxMode != SandboxFullAccess {
		t.Fatalf("sandbox_mode = %q, want %q (inert pre-v3 value must be neutralized)", m.Runtime.SandboxMode, SandboxFullAccess)
	}
}

func TestMigrationKeepsExplicitPreV3ReadOnly(t *testing.T) {
	// read-only could never have been injected by the tool, so it is an
	// explicit user choice and survives migration.
	m, _, _, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(
		minimalManifestTOML(2, `sandbox_mode = "read-only"`)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Runtime.SandboxMode != SandboxReadOnly {
		t.Fatalf("sandbox_mode = %q, want read-only preserved", m.Runtime.SandboxMode)
	}
}

func TestExplicitV3WorkspaceWriteSurvives(t *testing.T) {
	m, _, _, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(
		minimalManifestTOML(3, `sandbox_mode = "workspace-write"`)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Runtime.SandboxMode != SandboxWorkspaceWrite {
		t.Fatalf("sandbox_mode = %q, want workspace-write preserved at v3", m.Runtime.SandboxMode)
	}
}

func TestSandboxModeOffAccepted(t *testing.T) {
	m, _, _, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(
		minimalManifestTOML(3, `sandbox_mode = "off"`)))
	if err != nil {
		t.Fatalf("sandbox_mode = \"off\" must validate: %v", err)
	}
	if m.Runtime.SandboxMode != SandboxOff {
		t.Fatalf("sandbox_mode = %q, want off", m.Runtime.SandboxMode)
	}
}

func TestSandboxNetValidation(t *testing.T) {
	if _, _, _, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(
		minimalManifestTOML(3, `sandbox_net = "ports:443,8080"`))); err != nil {
		t.Fatalf("valid sandbox_net must pass: %v", err)
	}
	if _, _, _, err := DecodeAgentManifestWithMigrationInfo(strings.NewReader(
		minimalManifestTOML(3, `sandbox_net = "bogus"`))); err == nil {
		t.Fatal("invalid sandbox_net must fail validation")
	}
}

func TestDefaultManifestSandboxSetup(t *testing.T) {
	m := DefaultAgentManifest()
	if m.Runtime.SandboxMode != SandboxFullAccess {
		t.Fatalf("default sandbox_mode = %q, want full-access (opt-in feature)", m.Runtime.SandboxMode)
	}
	if m.Version != 5 {
		t.Fatalf("default manifest version = %d, want 5", m.Version)
	}
	// The OS sandbox is enforced transparently; it must not surface as a tool
	// the model can see or call.
	for _, tool := range m.Tools {
		if tool.ID == "sandbox" {
			t.Fatal("the sandbox must not be exposed to the model as a tool")
		}
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("default manifest must validate: %v", err)
	}
}
