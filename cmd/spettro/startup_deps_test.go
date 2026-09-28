package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// eagerInitPackages load a native library in their package initializer, so
// linking one into the macOS binary costs every process (even --version) its
// load time and memory. The clipboard library loaded AppKit this way: 1.7 to
// 2.3 ms and about 8 MB RSS per process (perf/startup, spettro-noclip).
var eagerInitPackages = []string{
	"github.com/aymanbagabas/go-nativeclipboard",
	"github.com/ebitengine/purego/objc",
}

// TestDarwinBinaryHasNoEagerNativeLoads is a work-count guard: the darwin
// build of cmd/spettro must not link a package that dlopens a system
// framework at init. internal/clipboard loads AppKit on first paste instead.
func TestDarwinBinaryHasNoEagerNativeLoads(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not found")
	}
	cmd := exec.Command(goTool, "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=arm64", "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		deps[strings.TrimSpace(line)] = true
	}
	for _, pkg := range eagerInitPackages {
		if deps[pkg] {
			t.Errorf("darwin build links %s, which loads a native library at startup", pkg)
		}
	}
}
