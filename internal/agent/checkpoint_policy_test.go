package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// isolateClassifierEnv clears the inherited settings the classifier reads,
// so the verdicts below do not depend on the developer's environment.
func isolateClassifierEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GOFLAGS", "")
	for _, name := range []string{"GIT_EXTERNAL_DIFF", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		t.Setenv(name, "")
	}
}

func TestReadOnlyShellCommandClassifier(t *testing.T) {
	isolateClassifierEnv(t)
	readOnly := []string{
		"ls -la",
		"pwd",
		"cat go.mod",
		"head -n 20 main.go | nl",
		"grep -rn 'TODO' internal/ | head -50",
		"rg --files -g '*.go'",
		"git status",
		"git status --porcelain && git diff --stat",
		"git -C sub log --oneline -5",
		"git --no-pager diff HEAD~1 -- internal/",
		"git branch -a",
		"git stash list",
		"find . -name '*.go' -type f",
		"sed -n '10,40p' internal/agent/agent.go",
		"sed -n '1p;$p' file",
		"go vet ./...",
		"go list -m all",
		"go env GOPATH",
		"cd internal && ls",
		"wc -l *.go 2>/dev/null",
		"ls missing 2>&1 | head",
		"LC_ALL=C sort -u names.txt",
		"LANG=C TZ=UTC git log -1",
		"echo done",
		"tree -L 2",
	}
	for _, cmd := range readOnly {
		if !isReadOnlyShellCommand(cmd) {
			t.Errorf("%q: classified as mutating, want read-only", cmd)
		}
	}
	mutating := []string{
		"",
		"go test ./...",
		"go build ./...",
		"go vet -vettool=/tmp/x ./...",
		"go env -w GOFLAGS=-mod=mod",
		"make",
		"npm test",
		"rm -rf build",
		"touch a.txt",
		"echo hi > a.txt",
		"echo hi >> a.txt",
		"cat a | tee b",
		"ls; rm x",
		"ls && mv a b",
		"ls & rm x",
		"echo $(rm x)",
		"echo `rm x`",
		"find . -name '*.tmp' -delete",
		"find . -exec rm {} \\;",
		"sed -i 's/a/b/' f.go",
		"sed -n 'w out.txt' f.go",
		"sed 's/a/b/' f.go",
		"sort -o out.txt in.txt",
		"sort --output=out.txt in.txt",
		"tree -o listing.txt",
		"rg --pre ./script pattern",
		"git checkout main",
		"git stash",
		"git reset --hard",
		"git diff --output=patch.diff",
		"git -c diff.external=./x diff",
		"git branch feature",
		"git apply fix.patch",
		"git",
		"python3 -c 'print(1)'",
		"bash -c ls",
		"xargs rm < list",
		"cat <<EOF > f\nx\nEOF",
		"(cd sub; ls)",
		"env -i rm x",
		"FOO=1 rm x",
		// Env prefixes are the environment form of flags the classifier
		// refuses (git -c, go -toolexec): they run programs or write files.
		"GIT_EXTERNAL_DIFF=./x.sh git diff",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.external GIT_CONFIG_VALUE_0=./x git diff",
		"GIT_CONFIG_PARAMETERS=\"'diff.external'='./x'\" git diff",
		"GOFLAGS=-toolexec=./x go vet ./...",
		"GOFLAGS=-mod=mod go list ./...",
		"LC_ALL=C GIT_EXTERNAL_DIFF=./x git diff",
		"env git status",
		"env GIT_EXTERNAL_DIFF=./x git diff",
		"LC_ALL=C",
		// -mod=mod and -modfile rewrite go.mod/go.sum.
		"go vet -mod=mod ./...",
		"go list -mod mod ./...",
		"go list --mod=mod -m all",
		"go vet -modfile=alt.mod ./...",
		"go vet -mod=readonly ./...",
	}
	for _, cmd := range mutating {
		if isReadOnlyShellCommand(cmd) {
			t.Errorf("%q: classified as read-only, want mutating", cmd)
		}
	}
}

// Inherited settings that make read-only git and go commands run programs or
// rewrite go.mod turn them mutating too.
func TestReadOnlyClassifierInheritedEnv(t *testing.T) {
	isolateClassifierEnv(t)
	t.Setenv("GOENV", filepath.Join(t.TempDir(), "missing"))
	if !isReadOnlyShellCommand("git diff") || !isReadOnlyShellCommand("go vet ./...") {
		t.Fatal("baseline: git diff / go vet should be read-only in a clean environment")
	}

	for _, name := range []string{"GIT_EXTERNAL_DIFF", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "1")
			if isReadOnlyShellCommand("git diff") {
				t.Errorf("git diff with %s set: classified read-only", name)
			}
		})
	}
	for _, flags := range []string{"-mod=mod", "-modfile=x.mod", "-trimpath -toolexec=./x"} {
		t.Run("GOFLAGS "+flags, func(t *testing.T) {
			t.Setenv("GOFLAGS", flags)
			for _, cmd := range []string{"go vet ./...", "go list ./..."} {
				if isReadOnlyShellCommand(cmd) {
					t.Errorf("%q with GOFLAGS=%q: classified read-only", cmd, flags)
				}
			}
		})
	}
	t.Run("GOFLAGS harmless", func(t *testing.T) {
		t.Setenv("GOFLAGS", "-trimpath")
		if !isReadOnlyShellCommand("go vet ./...") {
			t.Error("go vet with GOFLAGS=-trimpath: classified mutating")
		}
	})

	// `go env -w GOFLAGS=-mod=mod` persists in the go env file, which
	// applies when the variable is not set in the environment.
	t.Run("go env file", func(t *testing.T) {
		envFile := filepath.Join(t.TempDir(), "env")
		if err := os.WriteFile(envFile, []byte("GOPROXY=direct\nGOFLAGS=-mod=mod\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOENV", envFile)
		t.Setenv("GOFLAGS", "") // registers the restore for the Unsetenv below
		os.Unsetenv("GOFLAGS")
		if isReadOnlyShellCommand("go vet ./...") {
			t.Error("go vet with GOFLAGS=-mod=mod in the go env file: classified read-only")
		}
		t.Setenv("GOFLAGS", "")
		if !isReadOnlyShellCommand("go vet ./...") {
			t.Error("an explicit empty GOFLAGS overrides the go env file")
		}
	})
}

func TestNeedsCheckpoint(t *testing.T) {
	isolateClassifierEnv(t)
	if runtime.GOOS == "windows" {
		t.Skip("the read-only classifier covers POSIX shells only")
	}
	cases := []struct {
		tool, args string
		want       bool
	}{
		{"file-read", `{"path":"a"}`, false},
		{"grep", `{"pattern":"x"}`, false},
		{"file-edit", `{"path":"a"}`, true},
		{"file-write", `{"path":"a"}`, true},
		{"file-edit", `{"path":"a","edits":[{"old_string":"x","new_string":"y"}]}`, true},
		{"bash", `{"command":"git status"}`, false},
		{"shell-exec", `{"cmd":"ls -la"}`, false},
		{"bash", `{"command":"go test ./..."}`, true},
		{"bash", `{"command":"ls","run_in_background":true}`, true},
		{"bash", `not json`, true},
		{"bash", `{"job_id":"job-1","offset":0}`, false},
		{"pty-start", `{"command":"ls"}`, true},
	}
	for _, c := range cases {
		if got := needsCheckpoint(toolCall{Tool: c.tool, Args: []byte(c.args)}); got != c.want {
			t.Errorf("needsCheckpoint(%s %s) = %v, want %v", c.tool, c.args, got, c.want)
		}
	}
}

// A step snapshots once, before its first mutating call, and a step of
// read-only shell commands does not snapshot at all.
func TestParallelExecCheckpointsOncePerStep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell syntax")
	}
	r := newShellTestRuntime(t)
	var mu sync.Mutex
	var snaps []string
	var aExisted []bool
	r.checkpoint = func(tool string) {
		mu.Lock()
		defer mu.Unlock()
		snaps = append(snaps, tool)
		_, err := os.Stat(filepath.Join(r.cwd, "a.txt"))
		aExisted = append(aExisted, err == nil)
	}
	allowed := map[string]struct{}{"bash": {}, "file-write": {}, "file-read": {}}
	run := func(calls ...toolCall) {
		t.Helper()
		for i, res := range r.parallelExec(context.Background(), calls, allowed, nil) {
			if res.status != "success" {
				t.Fatalf("call %d (%s) failed: %s", i, calls[i].Tool, res.output)
			}
		}
	}

	run(
		toolCall{Tool: "bash", Args: mustJSON(t, map[string]string{"command": "ls"})},
		toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "a.txt", "content": "one"})},
		toolCall{Tool: "bash", Args: mustJSON(t, map[string]string{"command": "echo two > b.txt"})},
		toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "c.txt", "content": "three"})},
	)
	if len(snaps) != 1 || snaps[0] != "file-write" {
		t.Fatalf("snapshots after a 3-mutation step = %v, want exactly one before the first file-write", snaps)
	}
	if aExisted[0] {
		t.Fatalf("the step's snapshot ran after its first mutation")
	}

	run(
		toolCall{Tool: "bash", Args: mustJSON(t, map[string]string{"command": "cat a.txt b.txt"})},
		toolCall{Tool: "bash", Args: mustJSON(t, map[string]string{"command": "ls -la && wc -l c.txt"})},
	)
	if len(snaps) != 1 {
		t.Fatalf("a read-only step took a snapshot: %v", snaps)
	}

	run(toolCall{Tool: "bash", Args: mustJSON(t, map[string]string{"command": "touch d.txt"})})
	if len(snaps) != 2 || snaps[1] != "bash" {
		t.Fatalf("snapshots = %v, want a second one for the new mutating step", snaps)
	}
}

// A sub-agent in its own worktree must not snapshot the main checkout; one
// sharing the parent's directory keeps the hook.
func TestSubagentCheckpointScope(t *testing.T) {
	r := &toolRuntime{cwd: "/repo", checkpoint: func(string) {}}
	if r.subagentCheckpoint("/repo") == nil {
		t.Fatal("same-directory sub-agent lost its checkpoint hook")
	}
	if r.subagentCheckpoint("/repo/.spettro/worktrees/code-1") != nil {
		t.Fatal("worktree sub-agent kept a checkpoint hook for the main checkout")
	}
}
