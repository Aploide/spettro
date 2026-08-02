package jobs_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"spettro/internal/jobs"
	"spettro/internal/shell/shelltest"
)

// helperAddrEnv makes the test binary serve HTTP on the named address instead
// of running tests, giving TestBackgroundHTTPServer a real long-lived listener
// to drive.
const helperAddrEnv = "SPETTRO_TEST_HTTP_ADDR"

// TestMain routes the helper mode before the test framework starts.
//
// The obvious server, "python -m http.server", is not usable here.
// http.server.HTTPServer.server_bind does a reverse-DNS lookup
// (socket.getfqdn) between bind() and listen(), and on the macOS CI runners
// that lookup regularly outlasts the start-up budget. The failure is opaque:
// nothing has been printed yet because the banner comes after listen(), and a
// socket that is bound but not yet listening drops SYNs silently on BSD rather
// than refusing them, so the connect times out instead of failing fast.
// Serving from this binary also drops the interpreter dependency, so the case
// stops silently skipping on hosts without python.
func TestMain(m *testing.M) {
	if addr := os.Getenv(helperAddrEnv); addr != "" {
		serveForever(addr)
	}
	os.Exit(m.Run())
}

// serveForever runs the helper HTTP server until the process is killed. It
// reports to stderr, which is unbuffered, so a job that failed to bind is
// distinguishable from one that never got that far.
func serveForever(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper listen %s: %v\n", addr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "serving on %s\n", ln.Addr())
	err = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(os.Stderr, "%s %s\n", r.Method, r.URL.Path)
		io.WriteString(w, "ok\n")
	}))
	fmt.Fprintf(os.Stderr, "helper serve: %v\n", err)
	os.Exit(1)
}

func TestBackgroundJobLifecycle(t *testing.T) {
	m := jobs.NewManager()
	cmd := shelltest.Command(shelltest.Join(
		shelltest.Repeat(5, 50*time.Millisecond, func(i string) string { return shelltest.EchoVar("tick-", i) }),
		shelltest.Sleep(30*time.Second),
	))
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

// TestBackgroundHTTPServer acceptance flow: start a server
// detached, hit it from the foreground, then kill it.
func TestBackgroundHTTPServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	addr := ln.Addr().String()
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	m := jobs.NewManager()
	cmd := shelltest.Command(shelltest.Exec(exe))
	cmd.Env = append(os.Environ(), helperAddrEnv+"="+addr)
	job, err := m.Start(cmd, "http helper")
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
		if out, _, running, exitInfo := job.Output(0); !running {
			t.Fatalf("helper exited before serving: %s (output: %q)", exitInfo, out)
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
