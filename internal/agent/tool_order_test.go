package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlanToolBatches(t *testing.T) {
	names := []string{"file-read", "grep", "file-edit", "glob", "agent", "agent", "bash", "bash", "file-read", "some-mcp-tool"}
	calls := make([]toolCall, len(names))
	indices := make([]int, len(names))
	for i, n := range names {
		calls[i] = toolCall{Tool: n}
		indices[i] = i
	}
	got := planToolBatches(calls, indices)
	want := [][]int{{0, 1}, {2}, {3, 4, 5}, {6}, {7}, {8}, {9}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batches = %v, want %v", got, want)
	}
	// Skipped calls (not in indices) leave no gap in the plan.
	if got := planToolBatches(calls, []int{0, 3}); !reflect.DeepEqual(got, [][]int{{0, 3}}) {
		t.Fatalf("filtered batches = %v", got)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Mutating calls run in the model's order: an edit followed by the command
// that tests it must never race, and results come back in call order.
func TestParallelExecRunsMutationsInOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell syntax")
	}
	r := newShellTestRuntime(t)
	calls := []toolCall{
		{Tool: "bash", Args: mustJSON(t, map[string]string{"command": "sleep 0.2; echo 1 >> log.txt"})},
		{Tool: "bash", Args: mustJSON(t, map[string]string{"command": "echo 2 >> log.txt"})},
		{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "a.txt", "content": "written"})},
		{Tool: "bash", Args: mustJSON(t, map[string]string{"command": "cat a.txt; echo; cat log.txt"})},
		{Tool: "file-read", Args: mustJSON(t, map[string]string{"path": "log.txt"})},
	}
	allowed := map[string]struct{}{"bash": {}, "file-write": {}, "file-read": {}}
	res := r.parallelExec(context.Background(), calls, allowed, nil)
	for i, c := range calls {
		if res[i].name != c.Tool {
			t.Fatalf("result %d is for %q, want %q", i, res[i].name, c.Tool)
		}
		if res[i].status != "success" {
			t.Fatalf("call %d (%s) failed: %s", i, c.Tool, res[i].output)
		}
	}
	if got := res[3].output; got != "written\n1\n2\n" {
		t.Fatalf("shell saw out-of-order effects: %q", got)
	}
	if !strings.Contains(res[4].output, "1") || !strings.Contains(res[4].output, "2") {
		t.Fatalf("read after the writes missed them: %q", res[4].output)
	}
}

// A cancelled run must still give every call a result, without starting the
// calls that had not begun.
func TestParallelExecCancelledStepStartsNothing(t *testing.T) {
	r := newShellTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := []toolCall{{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "never.txt", "content": "x"})}}
	res := r.parallelExec(ctx, calls, map[string]struct{}{"file-write": {}}, nil)
	if res[0].status != "error" || !strings.Contains(res[0].output, "not executed") {
		t.Fatalf("result = %+v", res[0])
	}
	if _, err := os.Stat(filepath.Join(r.cwd, "never.txt")); err == nil {
		t.Fatal("cancelled call still ran")
	}
}

func TestFileMutationLockIsPerResolvedPath(t *testing.T) {
	r := newShellTestRuntime(t)
	if err := os.WriteFile(filepath.Join(r.cwd, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock := r.lockFileForMutation([]byte(`{"path":"a.txt"}`))
	acquired := make(chan struct{})
	go func() {
		// Same file, different spelling and alias: must wait.
		release := r.lockFileForMutation([]byte(`{"file_path":"./a.txt"}`))
		close(acquired)
		release()
	}()
	// A different file is independent.
	r.lockFileForMutation([]byte(`{"path":"b.txt"}`))()
	select {
	case <-acquired:
		t.Fatal("second lock on the same file did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("lock never released")
	}
	// Unresolvable arguments lock nothing and do not panic.
	r.lockFileForMutation([]byte(`not json`))()
	r.lockFileForMutation([]byte(`{"path":"../outside"}`))()
}

// Sibling runtimes (sub-agents sharing a checkout) editing one file at once
// must not lose each other's edits.
func TestConcurrentFileEditsAcrossRuntimesAreSerialized(t *testing.T) {
	dir := t.TempDir()
	const perWorker = 15
	var b strings.Builder
	for w := range 2 {
		for i := range perWorker {
			fmt.Fprintf(&b, "slot-%d-%d\n", w, i)
		}
	}
	path := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := range 2 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := newShellTestRuntime(t)
			r.cwd = dir
			allowed := map[string]struct{}{"file-edit": {}}
			for i := range perWorker {
				args := mustJSON(t, map[string]string{"path": "shared.txt", "old_string": fmt.Sprintf("slot-%d-%d\n", w, i), "new_string": fmt.Sprintf("done-%d-%d\n", w, i)})
				if _, err := r.execute(context.Background(), toolCall{Tool: "file-edit", Args: args}, allowed); err != nil {
					t.Errorf("edit %d/%d: %v", w, i, err)
				}
			}
		}(w)
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "slot-") {
		t.Fatalf("edits lost to a race:\n%s", data)
	}
}
