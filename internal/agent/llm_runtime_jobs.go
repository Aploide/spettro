package agent

import (
	"fmt"
	"strings"

	"spettro/internal/jobs"
)

// runJobOutput fetches accumulated output for a background job started via the
// bash/shell-exec run_in_background parameter. offset lets repeated polls read
// incrementally: pass the next_offset from the previous call.
func (r *toolRuntime) runJobOutput(rawArgs []byte) (string, error) {
	var args struct {
		JobID  string `json:"job_id"`
		Offset int    `json:"offset"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("job-output args: %w", err)
	}
	if strings.TrimSpace(args.JobID) == "" {
		return "", fmt.Errorf("job-output: job_id is required")
	}
	job, ok := jobs.Default().Get(args.JobID)
	if !ok {
		return "", fmt.Errorf("job-output: unknown job %q", args.JobID)
	}
	out, next, running, exitInfo := job.Output(args.Offset)
	status := "running"
	if !running {
		status = "exited (" + exitInfo + ")"
	}
	header := fmt.Sprintf("job=%s status=%s next_offset=%d", job.ID, status, next)
	if strings.TrimSpace(out) == "" {
		return header + "\n(no new output)", nil
	}
	return header + "\n" + truncate(out, 12000), nil
}

// runJobKill terminates a background job's whole process group.
func (r *toolRuntime) runJobKill(rawArgs []byte) (string, error) {
	var args struct {
		JobID string `json:"job_id"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("job-kill args: %w", err)
	}
	if strings.TrimSpace(args.JobID) == "" {
		return "", fmt.Errorf("job-kill: job_id is required")
	}
	if err := jobs.Default().Kill(args.JobID); err != nil {
		return "", fmt.Errorf("job-kill: %w", err)
	}
	return fmt.Sprintf("killed job %s", strings.TrimSpace(args.JobID)), nil
}
