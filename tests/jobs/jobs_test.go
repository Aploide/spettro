package jobs_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"spettro/internal/jobs"
)

func TestBackgroundJobLifecycle(t *testing.T) {
	m := jobs.NewManager()
	cmd := exec.Command("bash", "-c", "for i in 1 2 3 4 5; do echo tick-$i; sleep 0.05; done; sleep 30")
	job, err := m.Start(cmd, "tick loop")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if job.ID != "job-1" {
		t.Fatalf("unexpected id %q", job.ID)
	}
	if m.RunningCount() != 1 {
		t.Fatalf("running count = %d, want 1", m.RunningCount())
	}

	// Poll incrementally: the second read from next offset must not repeat.
	deadline := time.Now().Add(5 * time.Second)
	var out string
	var next int
	for time.Now().Before(deadline) {
		var running bool
		out, next, running, _ = job.Output(0)
		if strings.Contains(out, "tick-5") {
			break
		}
		if !running {
			t.Fatalf("job exited early, output: %q", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(out, "tick-1") || !strings.Contains(out, "tick-5") {
		t.Fatalf("missing ticks in output: %q", out)
	}
	if again, _, _, _ := job.Output(next); again != "" {
		t.Fatalf("incremental read repeated output: %q", again)
	}

	if err := m.Kill(job.ID); err != nil {
		t.Fatalf("kill: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for job.Running() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if job.Running() {
		t.Fatal("job still running after kill")
	}
	if m.RunningCount() != 0 {
		t.Fatalf("running count = %d after kill, want 0", m.RunningCount())
	}
}

// TestBackgroundHTTPServer mirrors the TODO acceptance flow: start a server
// detached, hit it from the foreground, then kill it.
func TestBackgroundHTTPServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	m := jobs.NewManager()
	cmd := exec.Command("bash", "-c", fmt.Sprintf("python3 -m http.server %d --bind 127.0.0.1", port))
	job, err := m.Start(cmd, "http.server")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.KillAll()

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	var resp *http.Response
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err = http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		out, _, _, _ := job.Output(0)
		t.Fatalf("server never came up: %v (job output: %q)", err, out)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// The request must show up in the job's captured output.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if out, _, _, _ := job.Output(0); strings.Contains(out, "GET /") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	out, _, _, _ := job.Output(0)
	if !strings.Contains(out, "GET /") {
		t.Fatalf("request not visible in job output: %q", out)
	}

	if err := m.Kill(job.ID); err != nil {
		t.Fatalf("kill: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for job.Running() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if job.Running() {
		t.Fatal("server still running after kill")
	}
}
