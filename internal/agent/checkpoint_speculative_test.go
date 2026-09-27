package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"spettro/internal/checkpoint"
	"spettro/internal/hooks"
	"spettro/internal/jobs"
)

// fakePrepared records how the runtime used a prepared snapshot.
type fakePrepared struct {
	mu        sync.Mutex
	claims    []string
	refreshed []string
	ok        bool
}

func (f *fakePrepared) Claim(tool string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, tool)
	return f.ok
}

func (f *fakePrepared) ClaimRefreshed(tool string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, tool)
	return f.ok
}

// speculativeTestRuntime is a runtime whose host counts synchronous
// snapshots and preparations; every preparation returns prepared. The run
// has already taken its first checkpoint, so it prepares ahead.
func speculativeTestRuntime(t *testing.T, prepared *fakePrepared) (r *toolRuntime, syncSnaps, preparations *int) {
	t.Helper()
	r = newShellTestRuntime(t)
	r.runCheckpointed = true
	var mu sync.Mutex
	syncSnaps, preparations = new(int), new(int)
	r.checkpoint = func(string) {
		mu.Lock()
		defer mu.Unlock()
		*syncSnaps++
	}
	r.checkpointPrepare = func() PreparedCheckpoint {
		mu.Lock()
		defer mu.Unlock()
		*preparations++
		return prepared
	}
	return r, syncSnaps, preparations
}

// prepareAndWait is prepareStepCheckpoint followed by waiting for the
// preparation (if one started), as a slow model would.
func prepareAndWait(r *toolRuntime) {
	r.prepareStepCheckpoint()
	r.stepCheckpointMu.Lock()
	sc := r.speculative
	r.stepCheckpointMu.Unlock()
	if sc != nil {
		<-sc.done
	}
}

var speculativeAllowed = map[string]struct{}{"file-write": {}, "file-read": {}, "file-edit": {}, "bash": {}}

func runStep(t *testing.T, r *toolRuntime, calls ...toolCall) {
	t.Helper()
	for i, res := range r.parallelExec(context.Background(), calls, speculativeAllowed, nil) {
		if res.status != "success" {
			t.Fatalf("call %d (%s) failed: %s", i, calls[i].Tool, res.output)
		}
	}
}

// The step's first mutating call claims the prepared snapshot; no
// synchronous snapshot runs.
func TestSpeculativeCheckpointIsClaimed(t *testing.T) {
	prepared := &fakePrepared{ok: true}
	r, syncSnaps, _ := speculativeTestRuntime(t, prepared)
	prepareAndWait(r)
	runStep(t, r,
		toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "a.txt", "content": "one"})},
		toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "b.txt", "content": "two"})},
	)
	if len(prepared.claims) != 1 || prepared.claims[0] != "file-write" || len(prepared.refreshed) != 0 {
		t.Fatalf("claims = %v, refreshed = %v; want one plain claim for the first file-write", prepared.claims, prepared.refreshed)
	}
	if *syncSnaps != 0 {
		t.Fatalf("%d synchronous snapshots after a successful claim, want 0", *syncSnaps)
	}
	// Claimed once: the next step without a new preparation snapshots
	// synchronously.
	runStep(t, r, toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "c.txt", "content": "3"})})
	if len(prepared.claims) != 1 || *syncSnaps != 1 {
		t.Fatalf("second step: claims = %v, sync snapshots = %d; want the synchronous path", prepared.claims, *syncSnaps)
	}
}

// A claim the host refuses (something else was snapshotted since) falls
// back to the synchronous snapshot.
func TestSpeculativeCheckpointRefusedFallsBack(t *testing.T) {
	prepared := &fakePrepared{ok: false}
	r, syncSnaps, _ := speculativeTestRuntime(t, prepared)
	prepareAndWait(r)
	runStep(t, r, toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "a.txt", "content": "one"})})
	if len(prepared.claims) != 1 || *syncSnaps != 1 {
		t.Fatalf("claims = %v, sync snapshots = %d; want a refused claim then one synchronous snapshot", prepared.claims, *syncSnaps)
	}
}

// A preparation that failed (nil) falls back too.
func TestSpeculativeCheckpointFailedPreparationFallsBack(t *testing.T) {
	r, syncSnaps, _ := speculativeTestRuntime(t, nil)
	r.checkpointPrepare = func() PreparedCheckpoint { return nil }
	prepareAndWait(r)
	runStep(t, r, toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "a.txt", "content": "one"})})
	if *syncSnaps != 1 {
		t.Fatalf("sync snapshots = %d after a failed preparation, want 1", *syncSnaps)
	}
}

// An edit to a file the agent knows, made while the model generated, is
// detected: the claim re-stages tracked files instead.
func TestSpeculativeCheckpointDetectsKnownFileEdit(t *testing.T) {
	prepared := &fakePrepared{ok: true}
	r, _, _ := speculativeTestRuntime(t, prepared)
	path := filepath.Join(r.cwd, "known.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	runStep(t, r, toolCall{Tool: "file-read", Args: mustJSON(t, map[string]string{"path": "known.txt"})})
	prepareAndWait(r)
	if err := os.WriteFile(path, []byte("v2 from the user's editor"), 0o644); err != nil {
		t.Fatal(err)
	}
	runStep(t, r, toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "other.txt", "content": "x"})})
	if len(prepared.refreshed) != 1 || len(prepared.claims) != 0 {
		t.Fatalf("claims = %v, refreshed = %v; want the refreshed claim", prepared.claims, prepared.refreshed)
	}
}

// An unclaimed preparation serves the next step when only read-only tools
// ran; any other tool makes the next step prepare anew.
func TestSpeculativeCheckpointReuse(t *testing.T) {
	prepared := &fakePrepared{ok: true}
	r, _, preparations := speculativeTestRuntime(t, prepared)
	if err := os.WriteFile(filepath.Join(r.cwd, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepareAndWait(r)
	runStep(t, r,
		toolCall{Tool: "file-read", Args: mustJSON(t, map[string]string{"path": "a.txt"})},
		toolCall{Tool: "bash", Args: mustJSON(t, map[string]string{"command": "ls"})},
	)
	prepareAndWait(r)
	if *preparations != 1 {
		t.Fatalf("preparations after a read-only step = %d, want the first one reused", *preparations)
	}
	r.noteTreeUse(toolCall{Tool: "download"})
	prepareAndWait(r)
	if *preparations != 2 {
		t.Fatalf("preparations after a tool that may write = %d, want a fresh one", *preparations)
	}
	r.stepCheckpointMu.Lock()
	r.speculative.started = time.Now().Add(-speculativeMaxAge)
	r.stepCheckpointMu.Unlock()
	prepareAndWait(r)
	if *preparations != 3 {
		t.Fatalf("preparations after the reuse window = %d, want a fresh one", *preparations)
	}
}

// While a background job runs nothing is prepared: it could change files at
// any moment.
func TestSpeculativeCheckpointSkippedWithBackgroundJob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	prepared := &fakePrepared{ok: true}
	r, syncSnaps, preparations := speculativeTestRuntime(t, prepared)
	job, err := jobs.Default().Start(exec.Command("sleep", "30"), "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jobs.Default().Kill(job.ID) })
	prepareAndWait(r)
	runStep(t, r, toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "a.txt", "content": "one"})})
	if *preparations != 0 || *syncSnaps != 1 {
		t.Fatalf("preparations = %d, sync snapshots = %d; want none prepared and a synchronous snapshot", *preparations, *syncSnaps)
	}
}

func TestMayCheckpointAndHooks(t *testing.T) {
	if mayCheckpoint(map[string]struct{}{"file-read": {}, "grep": {}, "glob": {}}) {
		t.Error("a read-only agent must not prepare snapshots")
	}
	if !mayCheckpoint(map[string]struct{}{"file-read": {}, "file-edit": {}}) || !mayCheckpoint(map[string]struct{}{"agent": {}}) {
		t.Error("an agent that edits or delegates must prepare snapshots")
	}
	on := true
	pre := hooks.EffectiveRule{Rule: hooks.Rule{Event: hooks.EventPreToolUse, Enabled: &on}, Enabled: true}
	start := hooks.EffectiveRule{Rule: hooks.Rule{Event: hooks.EventSessionStart}, Enabled: true}
	if !hooksMayWriteFiles(hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{start, pre}}) {
		t.Error("a PreToolUse hook must disable preparing ahead")
	}
	pre.Enabled = false
	if hooksMayWriteFiles(hooks.EffectiveConfig{Rules: []hooks.EffectiveRule{start, pre}}) {
		t.Error("session-start and disabled hooks must not disable preparing ahead")
	}
}

// hostCheckpoints wires a real checkpointer the way the TUI does.
type hostCheckpoints struct{ cp *checkpoint.Checkpointer }

func (h hostCheckpoints) snapshot(tool string) { _, _ = h.cp.Snapshot(tool, "p", nil) }

func (h hostCheckpoints) prepare() PreparedCheckpoint {
	p, err := h.cp.Prepare("step")
	if err != nil {
		return nil
	}
	return hostPrepared{h.cp, p}
}

type hostPrepared struct {
	cp *checkpoint.Checkpointer
	p  *checkpoint.Prepared
}

func (h hostPrepared) Claim(tool string) bool {
	_, err := h.cp.Commit(h.p, tool, "p", nil)
	return err == nil
}

func (h hostPrepared) ClaimRefreshed(tool string) bool {
	_, err := h.cp.CommitTracked(h.p, tool, "p", nil)
	return err == nil
}

// End to end with the real checkpointer: prepared steps rewind exactly like
// synchronous ones, and a user edit to a known file during generation is in
// the step's checkpoint.
func TestSpeculativeCheckpointRewindEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	r := newShellTestRuntime(t)
	cp, err := checkpoint.Open(t.TempDir(), r.cwd)
	if err != nil {
		t.Fatal(err)
	}
	host := hostCheckpoints{cp}
	r.checkpoint = host.snapshot
	r.checkpointPrepare = host.prepare
	read := func(rel string) string {
		data, _ := os.ReadFile(filepath.Join(r.cwd, rel))
		return string(data)
	}
	write := func(rel, content string) toolCall {
		return toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": rel, "content": content})}
	}

	prepareAndWait(r) // the run's first step: nothing is prepared yet
	runStep(t, r, write("a.txt", "a1"))
	prepareAndWait(r)
	runStep(t, r, toolCall{Tool: "file-read", Args: mustJSON(t, map[string]string{"path": "a.txt"})}, write("b.txt", "b1"))
	prepareAndWait(r)
	if err := os.WriteFile(filepath.Join(r.cwd, "a.txt"), []byte("a2 by the user"), 0o644); err != nil {
		t.Fatal(err)
	}
	runStep(t, r, write("c.txt", "c1"))

	list, err := cp.List()
	if err != nil || len(list) != 3 {
		t.Fatalf("checkpoints = %d (%v), want one per mutating step", len(list), err)
	}
	for _, c := range list {
		if c.Tool != "file-write" {
			t.Fatalf("checkpoint tool = %q, want file-write", c.Tool)
		}
	}
	if err := cp.RestoreFiles(list[2].ID); err != nil {
		t.Fatal(err)
	}
	if read("a.txt") != "a2 by the user" || read("b.txt") != "b1" || read("c.txt") != "" {
		t.Fatalf("rewind to step 3: a=%q b=%q c=%q; want the user's edit kept, c.txt gone", read("a.txt"), read("b.txt"), read("c.txt"))
	}
	if err := cp.RestoreFiles(list[1].ID); err != nil {
		t.Fatal(err)
	}
	if read("a.txt") != "a1" || read("b.txt") != "" {
		t.Fatalf("rewind to step 2: a=%q b=%q", read("a.txt"), read("b.txt"))
	}
	if err := cp.RestoreFiles(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if read("a.txt") != "" {
		t.Fatalf("rewind to step 1: a=%q, want no file", read("a.txt"))
	}
}

// A run prepares ahead only after its first checkpoint: a run that never
// edits (a question, a review) never stages the tree.
func TestSpeculativeCheckpointWaitsForTheRunsFirstCheckpoint(t *testing.T) {
	prepared := &fakePrepared{ok: true}
	r, syncSnaps, preparations := speculativeTestRuntime(t, prepared)
	r.runCheckpointed = false
	if err := os.WriteFile(filepath.Join(r.cwd, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepareAndWait(r)
	runStep(t, r, toolCall{Tool: "file-read", Args: mustJSON(t, map[string]string{"path": "a.txt"})})
	prepareAndWait(r)
	if *preparations != 0 {
		t.Fatalf("preparations in a run without a checkpoint = %d, want 0", *preparations)
	}
	runStep(t, r, toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "b.txt", "content": "b"})})
	if *syncSnaps != 1 {
		t.Fatalf("sync snapshots for the run's first mutating step = %d, want 1", *syncSnaps)
	}
	prepareAndWait(r)
	runStep(t, r, toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "c.txt", "content": "c"})})
	if *preparations != 1 || len(prepared.claims) != 1 || *syncSnaps != 1 {
		t.Fatalf("after the first checkpoint: preparations = %d, claims = %v, sync = %d; want the next step prepared and claimed", *preparations, prepared.claims, *syncSnaps)
	}
}

// A preparation claimed speculativeMaxAge or more after it started (a slow
// generation, an approval wait) falls back to the synchronous snapshot.
func TestSpeculativeCheckpointExpiredClaimFallsBack(t *testing.T) {
	prepared := &fakePrepared{ok: true}
	r, syncSnaps, _ := speculativeTestRuntime(t, prepared)
	prepareAndWait(r)
	r.stepCheckpointMu.Lock()
	r.speculative.started = time.Now().Add(-speculativeMaxAge)
	r.stepCheckpointMu.Unlock()
	runStep(t, r, toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "a.txt", "content": "one"})})
	if len(prepared.claims)+len(prepared.refreshed) != 0 || *syncSnaps != 1 {
		t.Fatalf("claims = %v, refreshed = %v, sync = %d; want the synchronous path", prepared.claims, prepared.refreshed, *syncSnaps)
	}
}

// Review finding: a call that writes the tree without a checkpoint of its
// own (download, MCP tools) earlier in the same step is not in the prepared
// tree, so the step's claim must fall back. End to end: rewinding to the
// checkpoint before the file-write keeps the downloaded file, as the
// synchronous path does.
func TestSpeculativeCheckpointSameStepWriterFallsBack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, speculative := range []bool{false, true} {
		t.Run(fmt.Sprintf("speculative=%v", speculative), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("payload")) }))
			defer srv.Close()
			r := newShellTestRuntime(t)
			r.httpClient = &http.Client{}
			cp, err := checkpoint.Open(t.TempDir(), r.cwd)
			if err != nil {
				t.Fatal(err)
			}
			host := hostCheckpoints{cp}
			r.checkpoint = host.snapshot
			if speculative {
				r.checkpointPrepare = host.prepare
				r.runCheckpointed = true
				prepareAndWait(r)
			}
			allowed := map[string]struct{}{"file-write": {}, "download": {}}
			calls := []toolCall{
				{Tool: "download", Args: mustJSON(t, map[string]string{"url": srv.URL, "path": "vendor/lib.bin"})},
				{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "a.txt", "content": "a"})},
			}
			for i, res := range r.parallelExec(context.Background(), calls, allowed, nil) {
				if res.status != "success" {
					t.Fatalf("call %d: %s", i, res.output)
				}
			}
			list, _ := cp.List()
			if len(list) == 0 {
				t.Fatal("no checkpoint recorded")
			}
			if err := cp.RestoreFiles(list[len(list)-1].ID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(r.cwd, "vendor", "lib.bin")); err != nil {
				t.Fatalf("downloaded file after rewinding to the checkpoint before file-write: %v, want it kept", err)
			}
		})
	}
}

// Review finding: a file first read after the preparation started (in a
// step that reused it) and then edited by the user is detected too.
func TestSpeculativeCheckpointDetectsEditToFileReadAfterPreparation(t *testing.T) {
	prepared := &fakePrepared{ok: true}
	r, _, preparations := speculativeTestRuntime(t, prepared)
	path := filepath.Join(r.cwd, "later.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepareAndWait(r)
	runStep(t, r, toolCall{Tool: "file-read", Args: mustJSON(t, map[string]string{"path": "later.txt"})})
	prepareAndWait(r) // reused: only a read ran
	if *preparations != 1 {
		t.Fatalf("preparations = %d, want the first one reused", *preparations)
	}
	if err := os.WriteFile(path, []byte("v2 from the user's editor"), 0o644); err != nil {
		t.Fatal(err)
	}
	runStep(t, r, toolCall{Tool: "file-write", Args: mustJSON(t, map[string]string{"path": "other.txt", "content": "x"})})
	if len(prepared.refreshed) != 1 || len(prepared.claims) != 0 {
		t.Fatalf("claims = %v, refreshed = %v; want the refreshed claim", prepared.claims, prepared.refreshed)
	}
}

// A file stamped after the preparation started counts as unchanged only
// when its last change (by ctime) is well before the preparation.
func TestLastChangedBefore(t *testing.T) {
	now := time.Now().UnixNano()
	old := now - 10*int64(time.Second)
	for _, tc := range []struct {
		id   fileIdentity
		want bool
	}{
		{fileIdentity{mtime: old, ctime: old}, true},
		{fileIdentity{mtime: old, ctime: now - int64(time.Second)}, false}, // changed within the racy window
		{fileIdentity{mtime: now, ctime: old}, false},                      // mtime moved forward
		{fileIdentity{mtime: old}, false},                                  // no ctime (Windows)
	} {
		if got := lastChangedBefore(tc.id, now); got != tc.want {
			t.Errorf("lastChangedBefore(%+v) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
