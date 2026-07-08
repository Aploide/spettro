// Package jobs tracks detached background shell commands started by the agent
// (dev servers, watch builds). Jobs are process-wide session state: they
// outlive individual agent turns and are killed when the session ends.
package jobs

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// outputCap bounds the retained output per job. Once exceeded the oldest
// bytes are dropped; offsets keep counting absolute bytes written so
// incremental polls stay consistent.
const outputCap = 1 << 20 // 1 MiB

type Job struct {
	ID      string
	Command string
	Started time.Time

	mu       sync.Mutex
	buf      []byte
	dropped  int // absolute offset of buf[0]
	done     bool
	exitInfo string
	cmd      *exec.Cmd
}

// Write implements io.Writer; the command's combined stdout/stderr streams
// into the ring buffer.
func (j *Job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.buf = append(j.buf, p...)
	if over := len(j.buf) - outputCap; over > 0 {
		j.buf = j.buf[over:]
		j.dropped += over
	}
	return len(p), nil
}

// Output returns the job's output from absolute byte offset onward, the next
// offset to poll from, and whether the job is still running.
func (j *Job) Output(offset int) (out string, next int, running bool, exitInfo string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if offset < j.dropped {
		offset = j.dropped
	}
	rel := offset - j.dropped
	if rel > len(j.buf) {
		rel = len(j.buf)
	}
	return string(j.buf[rel:]), j.dropped + len(j.buf), !j.done, j.exitInfo
}

func (j *Job) Running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return !j.done
}

// Manager tracks the session's background jobs.
type Manager struct {
	mu   sync.Mutex
	jobs map[string]*Job
	seq  int
}

func NewManager() *Manager {
	return &Manager{jobs: map[string]*Job{}}
}

var defaultManager = NewManager()

// Default returns the process-wide manager; the spettro process is one
// session, so session-scoped jobs live here.
func Default() *Manager { return defaultManager }

// Start launches cmd detached in its own process group, registers it, and
// reaps it in the background. The cmd must not have been started yet and must
// not have Stdout/Stderr set.
func (m *Manager) Start(cmd *exec.Cmd, command string) (*Job, error) {
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("job-%d", m.seq)
	m.mu.Unlock()

	job := &Job{ID: id, Command: command, Started: time.Now(), cmd: cmd}
	cmd.Stdout = job
	cmd.Stderr = job
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.jobs[id] = job
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		job.mu.Lock()
		job.done = true
		if err != nil {
			job.exitInfo = err.Error()
		} else {
			job.exitInfo = "exit status 0"
		}
		job.mu.Unlock()
	}()
	return job, nil
}

func (m *Manager) Get(id string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[strings.TrimSpace(id)]
	return j, ok
}

// Kill terminates the job's whole process group.
func (m *Manager) Kill(id string) error {
	j, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("unknown job %q", id)
	}
	if !j.Running() {
		return nil
	}
	return kill(j.cmd)
}

// KillAll terminates every running job; call on session exit.
func (m *Manager) KillAll() {
	m.mu.Lock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	for _, j := range jobs {
		if j.Running() {
			_ = kill(j.cmd)
		}
	}
}

// List returns all jobs ordered by ID.
func (m *Manager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Started.Before(out[b].Started) })
	return out
}

// RunningCount reports how many jobs are still running.
func (m *Manager) RunningCount() int {
	n := 0
	for _, j := range m.List() {
		if j.Running() {
			n++
		}
	}
	return n
}
