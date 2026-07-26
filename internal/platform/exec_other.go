//go:build !ios

package platform

// Every platform Spettro targets other than iOS — macOS, Linux, Windows —
// lets the process spawn children. Whether a particular binary exists is a
// separate question, answered by exec.LookPath at the call site.
const (
	canExec               = true
	execUnavailableReason = ""
)
