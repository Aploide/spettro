package session

import (
	"fmt"
	"sort"
	"strings"
)

// Task graph semantics for the session task store. Tasks form a DAG through
// their Dependencies edges; these helpers validate the graph and derive the
// scheduling state (ready / blocked) the task tools and UIs report.

const (
	TaskStatusPending    = "pending"
	TaskStatusInProgress = "in_progress"
	TaskStatusCompleted  = "completed"
	TaskStatusBlocked    = "blocked"
	TaskStatusCancelled  = "cancelled"
)

// NormalizeTaskStatus canonicalizes a task status, accepting common aliases.
// An empty status defaults to pending.
func NormalizeTaskStatus(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", TaskStatusPending, "todo":
		return TaskStatusPending, nil
	case TaskStatusInProgress, "in-progress", "active", "running":
		return TaskStatusInProgress, nil
	case TaskStatusCompleted, "done":
		return TaskStatusCompleted, nil
	case TaskStatusBlocked:
		return TaskStatusBlocked, nil
	case TaskStatusCancelled, "canceled":
		return TaskStatusCancelled, nil
	default:
		return "", fmt.Errorf("invalid task status %q (want pending|in_progress|completed|blocked|cancelled)", s)
	}
}

// taskDone reports whether a task no longer gates its dependents.
func taskDone(status string) bool {
	return status == TaskStatusCompleted || status == TaskStatusCancelled
}

// ValidateTaskGraph checks the whole task list for structural problems:
// duplicate IDs, dependencies on unknown tasks, self-dependencies, and
// dependency cycles.
func ValidateTaskGraph(todos []Todo) error {
	byID := make(map[string]*Todo, len(todos))
	for i := range todos {
		id := todos[i].ID
		if _, dup := byID[id]; dup {
			return fmt.Errorf("duplicate task id %q", id)
		}
		byID[id] = &todos[i]
	}
	for _, t := range todos {
		for _, dep := range t.Dependencies {
			if dep == t.ID {
				return fmt.Errorf("task %q depends on itself", t.ID)
			}
			if _, ok := byID[dep]; !ok {
				return fmt.Errorf("task %q depends on unknown task %q", t.ID, dep)
			}
		}
	}
	// Cycle detection: iterative DFS with three-color marking.
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(todos))
	var visit func(id string, path []string) error
	visit = func(id string, path []string) error {
		switch color[id] {
		case gray:
			return fmt.Errorf("dependency cycle: %s", strings.Join(append(path, id), " -> "))
		case black:
			return nil
		}
		color[id] = gray
		for _, dep := range byID[id].Dependencies {
			if err := visit(dep, append(path, id)); err != nil {
				return err
			}
		}
		color[id] = black
		return nil
	}
	for _, t := range todos {
		if err := visit(t.ID, nil); err != nil {
			return err
		}
	}
	return nil
}

// IncompleteDeps returns the IDs of t's dependencies that are not yet
// completed (or cancelled), i.e. the tasks currently gating t.
func IncompleteDeps(t Todo, all []Todo) []string {
	if len(t.Dependencies) == 0 {
		return nil
	}
	status := make(map[string]string, len(all))
	for _, o := range all {
		status[o.ID] = o.Status
	}
	var out []string
	for _, dep := range t.Dependencies {
		st, known := status[dep]
		if !known || !taskDone(st) {
			out = append(out, dep)
		}
	}
	return out
}

// BlockedIDs returns the set of task IDs that are effectively blocked: not
// yet done, with at least one incomplete dependency.
func BlockedIDs(all []Todo) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range all {
		if taskDone(t.Status) {
			continue
		}
		if len(IncompleteDeps(t, all)) > 0 {
			out[t.ID] = struct{}{}
		}
	}
	return out
}

// ReadyTasks returns pending tasks whose dependencies are all satisfied —
// the tasks an agent can start next, in stored order.
func ReadyTasks(all []Todo) []Todo {
	var out []Todo
	for _, t := range all {
		if t.Status != TaskStatusPending {
			continue
		}
		if len(IncompleteDeps(t, all)) == 0 {
			out = append(out, t)
		}
	}
	return out
}

// TopoOrder returns the task IDs in a dependency-respecting order
// (dependencies before dependents; ties broken by stored order). It assumes
// the graph already passed ValidateTaskGraph; on a cycle it returns the
// stored order.
func TopoOrder(all []Todo) []string {
	indegree := make(map[string]int, len(all))
	dependents := make(map[string][]string, len(all))
	pos := make(map[string]int, len(all))
	for i, t := range all {
		pos[t.ID] = i
		indegree[t.ID] += 0
	}
	for _, t := range all {
		for _, dep := range t.Dependencies {
			if _, ok := pos[dep]; !ok {
				// unknown dep: ignore edge; validation reports it separately
				continue
			}
			indegree[t.ID]++
			dependents[dep] = append(dependents[dep], t.ID)
		}
	}
	var ready []string
	for _, t := range all {
		if indegree[t.ID] == 0 {
			ready = append(ready, t.ID)
		}
	}
	var out []string
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return pos[ready[i]] < pos[ready[j]] })
		id := ready[0]
		ready = ready[1:]
		out = append(out, id)
		for _, d := range dependents[id] {
			indegree[d]--
			if indegree[d] == 0 {
				ready = append(ready, d)
			}
		}
	}
	if len(out) != len(all) {
		out = out[:0]
		for _, t := range all {
			out = append(out, t.ID)
		}
	}
	return out
}
