//go:build !windows

package notify

// sendPlatform has no non-Windows work to do: Send handles the linux and
// darwin channels directly.
func sendPlatform(title, body string) {}
