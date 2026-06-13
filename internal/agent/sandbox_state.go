package agent

import "spettro/internal/sandbox"

// SandboxState carries the session's OS sandbox policy. It is resolved once at
// startup from the operator's settings (CLI flags / manifest) and is immutable
// for the lifetime of the session: the model can neither observe it nor request
// changes to it. A nil *SandboxState means the sandbox is disabled.
type SandboxState struct {
	policy sandbox.Policy
}

func NewSandboxState(p sandbox.Policy) *SandboxState {
	return &SandboxState{policy: p}
}

// Policy returns the active policy (the zero Policy when state is nil).
func (s *SandboxState) Policy() sandbox.Policy {
	if s == nil {
		return sandbox.Policy{}
	}
	return s.policy
}

// sandboxPolicy returns the runtime's effective policy, or the disabled zero
// value when no sandbox is attached.
func (r *toolRuntime) sandboxPolicy() sandbox.Policy {
	return r.sandboxState.Policy()
}
