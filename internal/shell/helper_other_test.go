//go:build !unix

package shell

// runTestHelperIfRequested is a no-op where the hangup helper does not exist.
func runTestHelperIfRequested() {}
