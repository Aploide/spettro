package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"spettro/internal/config"
	"spettro/internal/provider"
)

// Helpers shared by the runners that fan work out to many sub-agents at once
// (today the workflow tool): which manifest agent a fan-out member runs as,
// whether a failed member is worth starting over, and a cancellable wait
// for the backoff in between.

// resolveFanOutTarget picks the sub-agent spec a fan-out member runs: the
// explicit agentType when given, otherwise the "code" worker, otherwise the
// first enabled worker/subagent in the manifest.
//
// Only workers and subagents qualify, never an orchestrator or a primary:
// a member that could itself fan out would let one call nest a fan-out
// inside another, multiplying the agents a single request starts. The errors
// carry no prefix; the caller names itself.
func resolveFanOutTarget(manifest *config.AgentManifest, agentType string) (config.AgentSpec, error) {
	target := strings.TrimSpace(agentType)
	if target == "" {
		target = "code"
		if _, ok := manifest.AgentByID(target); !ok {
			target = ""
			for _, a := range manifest.Agents {
				if a.Enabled && (a.Role == config.AgentRoleWorker || a.Role == config.AgentRoleSubagent) {
					target = a.ID
					break
				}
			}
		}
	}
	if target == "" {
		return config.AgentSpec{}, fmt.Errorf("no worker agent available in manifest")
	}
	spec, ok := manifest.AgentByID(target)
	if !ok {
		return config.AgentSpec{}, fmt.Errorf("unknown agent type %q", target)
	}
	if !spec.Enabled {
		return config.AgentSpec{}, fmt.Errorf("agent type %q is disabled", target)
	}
	if spec.Mode == "orchestrator" || (spec.Role != config.AgentRoleWorker && spec.Role != config.AgentRoleSubagent) {
		return config.AgentSpec{}, fmt.Errorf("agent type %q must be a worker/subagent, got role %q", target, spec.Role)
	}
	return spec, nil
}

// rerunSubagentAfter reports whether a sub-agent run that failed with err is
// worth starting over: a transient provider failure, except a rate limit the
// provider manager already waited out for minutes (a re-run would restart
// the work from scratch only to queue on the same bucket).
func rerunSubagentAfter(err error) bool {
	return provider.Classify(err).Transient() && !errors.Is(err, provider.ErrRateLimitRetriesExhausted)
}

// sleepCtx waits for d or until ctx is cancelled; false means cancelled, so
// a backoff between sub-agent attempts never outlives the run it serves.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
