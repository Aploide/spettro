package jobs

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestJobOutputRingBuffer(t *testing.T) {
	j := &Job{ID: "job-x"}
	if _, err := j.Write([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}

	out, next, running, _ := j.Output(0)
	if out != "hello world" || next != 11 || !running {
		t.Errorf("Output(0) = (%q, %d, %v)", out, next, running)
	}
	// Incremental poll from previous offset.
	out, next, _, _ = j.Output(6)
	if out != "world" || next != 11 {
		t.Errorf("Output(6) = (%q, %d)", out, next)
	}
	// Offset past the end yields empty, same next.
	out, next, _, _ = j.Output(100)
	if out != "" || next != 11 {
		t.Errorf("Output(100) = (%q, %d)", out, next)
	}
}

func TestJobOutputCapDropsOldest(t *testing.T) {
	j := &Job{ID: "job-big"}
	big := strings.Repeat("a", outputCap)
	if _, err := j.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Write([]byte("TAIL")); err != nil {
		t.Fatal(err)
	}
	out, next, _, _ := j.Output(0)
	if len(out) != outputCap || !strings.HasSuffix(out, "TAIL") {
		t.Errorf("capped output wrong: len=%d suffix=%q", len(out), out[len(out)-4:])
	}
	// Absolute offsets keep counting past the drop.
	if next != outputCap+4 {
		t.Errorf("next = %d, want %d", next, outputCap+4)
	}
	// An offset inside the dropped region is clamped forward.
	out, _, _, _ = j.Output(1)
	if len(out) != outputCap {
		t.Errorf("dropped-region offset not clamped: len=%d", len(out))
	}
}

func waitDone(t *testing.T, j *Job) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for j.Running() {
		select {
		case <-deadline:
			t.Fatal("job did not finish in time")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestManagerStartAndReap(t *testing.T) {
	m := NewManager()
	job, err := m.Start(exec.Command("sh", "-c", "echo out; echo err >&2"), "echo test")
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "job-1" {
		t.Errorf("first job ID = %q", job.ID)
	}
	waitDone(t, job)
	out, _, running, exitInfo := job.Output(0)
	if running {
		t.Error("job should be done")
	}
	if !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Errorf("combined output = %q", out)
	}
	if exitInfo != "exit status 0" {
		t.Errorf("exitInfo = %q", exitInfo)
	}

	got, ok := m.Get(" job-1 ")
	if !ok || got != job {
		t.Error("Get with padded ID failed")
	}
	if _, ok := m.Get("job-99"); ok {
		t.Error("unknown ID should not be found")
	}
	if m.RunningCount() != 0 {
		t.Errorf("RunningCount = %d", m.RunningCount())
	}
}

func TestManagerKill(t *testing.T) {
	m := NewManager()
	job, err := m.Start(exec.Command("sleep", "30"), "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	if !job.Running() {
		t.Fatal("job should be running")
	}
	if err := m.Kill(job.ID); err != nil {
		t.Fatal(err)
	}
	waitDone(t, job)
	if err := m.Kill(job.ID); err != nil {
		t.Errorf("killing a finished job must be a no-op, got %v", err)
	}
	if err := m.Kill("nope"); err == nil {
		t.Error("killing unknown job must error")
	}
}

func TestSpoolStore(t *testing.T) {
	s := NewSpoolStore()
	defer s.Cleanup()

	id, err := s.Add("0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if id != "spool:1" {
		t.Errorf("id = %q", id)
	}

	chunk, next, size, err := s.Read(id, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if chunk != "0123" || next != 4 || size != 10 {
		t.Errorf("Read = (%q, %d, %d)", chunk, next, size)
	}
	// Continue from next; max<=0 means read to the end.
	chunk, next, _, err = s.Read(id, next, 0)
	if err != nil {
		t.Fatal(err)
	}
	if chunk != "456789" || next != 10 {
		t.Errorf("second Read = (%q, %d)", chunk, next)
	}
	// Negative and past-end offsets are clamped.
	if chunk, _, _, _ := s.Read(id, -5, 2); chunk != "01" {
		t.Errorf("negative offset chunk = %q", chunk)
	}
	if chunk, next, _, _ := s.Read(id, 99, 0); chunk != "" || next != 10 {
		t.Errorf("past-end Read = (%q, %d)", chunk, next)
	}
	if _, _, _, err := s.Read("spool:404", 0, 0); err == nil {
		t.Error("unknown spool must error")
	}

	s.Cleanup()
	if _, _, _, err := s.Read(id, 0, 0); err == nil {
		t.Error("Read after Cleanup must error")
	}
}
