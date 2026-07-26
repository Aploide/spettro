//go:build ios

package platform

// iOS applications run under a sandbox that denies fork/exec unconditionally.
// This is not a missing-binary problem and cannot be worked around by
// shipping one: the process is simply not permitted to create another.
const (
	canExec               = false
	execUnavailableReason = "this device cannot run programs: iOS does not permit an application to start another process"
)
