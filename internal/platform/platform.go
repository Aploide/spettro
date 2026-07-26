// Package platform answers, in one place, the process-level questions about
// the host OS that Spettro's feature set depends on.
//
// It exists so the rest of the tree asks a capability ("can I spawn a
// subprocess?") instead of testing an operating system (`runtime.GOOS ==
// "ios"`) at a dozen unrelated call sites. The answers are build-time
// constants, so the compiler folds the branches away and an unsupported
// backend is never linked into a build that cannot use it.
//
// Adding a capability: give it a constant in every per-platform file below
// and an accessor here. Never let a capability be answered by a runtime OS
// string — that is the pattern this package replaces.
package platform

// CanExec reports whether this process may spawn subprocesses (fork/exec).
//
// It is false on iOS, where the kernel denies fork/exec to sandboxed
// applications outright: there is no shell, no git, no compiler, no test
// runner and no language server, regardless of what is present on disk.
// Callers must treat false as permanent — it is a property of the platform,
// not of the environment or of the user's PATH — and degrade by *removing*
// the feature rather than by letting it fail. A feature that errors is one
// the model retries; a feature that is absent is one it plans around.
func CanExec() bool { return canExec }

// ExecUnavailableReason is a single sentence, fit for a log line or a model's
// context, explaining why CanExec is false. It is empty when CanExec is true.
func ExecUnavailableReason() string { return execUnavailableReason }
