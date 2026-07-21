package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"spettro/internal/config"
	"spettro/internal/jobs"
)

func TestShellExecSpoolsAndJobOutputPages(t *testing.T) {
	t.Cleanup(jobs.Spool().Cleanup)
	r := &toolRuntime{cwd: t.TempDir(), permission: config.PermissionYOLO, readSet: map[string]struct{}{}}
	out, err := r.runShellTool(context.Background(), "shell-exec", []byte(`{"command":"seq 1 20000 | awk '{print \"log line \" $1}'"}`), "shell-exec")
	if err != nil {
		t.Fatalf("shell-exec: %v", err)
	}
	m := regexp.MustCompile(`"job_id":"(spool:\d+)","offset":(\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no spool footer in shell output; tail: %q", out[len(out)-200:])
	}
	page, err := r.runJobOutput([]byte(`{"job_id":"` + m[1] + `","offset":` + m[2] + `}`))
	if err != nil {
		t.Fatalf("job-output on %s: %v", m[1], err)
	}
	if !strings.Contains(page, "spool="+m[1]) || !strings.Contains(page, "log line") {
		t.Fatalf("paging failed: %q", page[:150])
	}
}

func TestBashOutputWithJobIDRoutesToJobOutput(t *testing.T) {
	t.Cleanup(jobs.Spool().Cleanup)
	id, err := jobs.Spool().Add(strings.Repeat("spooled line\n", 100))
	if err != nil {
		t.Fatal(err)
	}
	r := &toolRuntime{cwd: t.TempDir(), permission: config.PermissionYOLO, readSet: map[string]struct{}{}}
	out, err := r.execute(context.Background(), toolCall{Tool: "bash-output", Args: []byte(`{"job_id":"` + id + `","offset":0}`)}, map[string]struct{}{"bash-output": {}})
	if err != nil {
		t.Fatalf("bash-output with job_id: %v", err)
	}
	if !strings.Contains(out, "spool="+id) || !strings.Contains(out, "spooled line") {
		t.Fatalf("expected spool paging result, got %q", out[:min(150, len(out))])
	}
}

func TestJobOutputPaginationNeverSkipsBytes(t *testing.T) {
	// A job whose pending output exceeds the history budget must return
	// next_offset pointing at the end of the shown chunk, not the buffer end.
	job := &jobs.Job{ID: "job-test"}
	big := strings.Repeat("background job line\n", 1000) // ~20KB > 8000 budget
	if _, err := job.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	jobs.Default().Register(job)
	r := &toolRuntime{}
	var got strings.Builder
	offset := 0
	for i := 0; i < 20; i++ {
		out, err := r.runJobOutput([]byte(fmt.Sprintf(`{"job_id":"job-test","offset":%d}`, offset)))
		if err != nil {
			t.Fatal(err)
		}
		var next int
		if _, err := fmt.Sscanf(out[strings.Index(out, "next_offset="):], "next_offset=%d", &next); err != nil {
			t.Fatalf("no next_offset in %q", out[:80])
		}
		if strings.Contains(out, "(no new output)") {
			break
		}
		body := out[strings.Index(out, "\n")+1:]
		if i := strings.Index(body, "\n[truncated:"); i >= 0 {
			body = body[:i]
		}
		got.WriteString(body)
		if next == offset {
			break
		}
		offset = next
	}
	if got.String() != big {
		t.Fatalf("paged job output diverges: got %d bytes, want %d", got.Len(), len(big))
	}
}
