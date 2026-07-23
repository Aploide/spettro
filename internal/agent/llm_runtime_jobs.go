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
	if strings.HasPrefix(strings.TrimSpace(args.JobID), "spool:") {
		return r.readSpoolOutput(strings.TrimSpace(args.JobID), args.Offset)
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
	// Cap the chunk to the history budget, but move next_offset back to the
	// end of what was actually shown so repeated polls never skip bytes.
	footer := ""
	if limit := r.historyLimit("job-output") - spoolFooterReserve; limit > 0 && len(out) > limit {
		start := next - len(out)
		cut := out[:limit]
		if i := strings.LastIndexByte(cut, '\n'); i > 0 {
			cut = cut[:i+1]
		}
		next = start + len(cut)
		out = cut
		footer = fmt.Sprintf("\n[truncated: call job-output again with offset %d to continue]", next)
	}
	header := fmt.Sprintf("job=%s status=%s next_offset=%d", job.ID, status, next)
	if strings.TrimSpace(out) == "" {
		return header + "\n(no new output)", nil
	}
	return header + "\n" + out + footer, nil
}

// readSpoolOutput pages through a spooled (truncated) tool result. The chunk
// size is capped by the job-output history budget, leaving room for the header
// so history truncation never cuts the paging hint.
func (r *toolRuntime) readSpoolOutput(spoolID string, offset int) (string, error) {
	maxChunk := max(r.historyLimit("job-output")-200, 1000)
	chunk, next, size, err := jobs.Spool().Read(spoolID, offset, maxChunk)
	if err != nil {
		return "", fmt.Errorf("job-output: %w", err)
	}
	status := "more available"
	if next >= size {
		status = "end of output"
	}
	header := fmt.Sprintf("spool=%s size=%d next_offset=%d (%s)", spoolID, size, next, status)
	if chunk == "" {
		return header + "\n(no more output)", nil
	}
	return header + "\n" + chunk, nil
}

// runToolOutput reads back a tool result that was offloaded to the spool
// (either by execution-time truncation or by compaction stage 1). It is the
// stable read path the compaction stubs point at; job-output with a spool ID
// remains as an alias.
func (r *toolRuntime) runToolOutput(rawArgs []byte) (string, error) {
	var args struct {
		ID     string `json:"id"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeJSONStrict(rawArgs, &args); err != nil {
		return "", fmt.Errorf("tool-output args: %w", err)
	}
	id := strings.TrimSpace(args.ID)
	if id == "" {
		return "", fmt.Errorf("tool-output: id is required")
	}
	// Accept a bare number for convenience ("7" -> "spool:7").
	if !strings.Contains(id, ":") {
		id = "spool:" + id
	}
	maxChunk := max(r.historyLimit("tool-output")-200, 1000)
	if args.Limit > 0 && args.Limit < maxChunk {
		maxChunk = args.Limit
	}
	chunk, next, size, err := jobs.Spool().Read(id, args.Offset, maxChunk)
	if err != nil {
		return "", fmt.Errorf("tool-output: %w", err)
	}
	status := "more available"
	if next >= size {
		status = "end of output"
	}
	header := fmt.Sprintf("output=%s size=%d next_offset=%d (%s)", id, size, next, status)
	if chunk == "" {
		return header + "\n(no more output)", nil
	}
	return header + "\n" + chunk, nil
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
