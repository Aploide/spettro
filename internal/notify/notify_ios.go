//go:build ios

package notify

// Send is a no-op on iOS: there is no subprocess to shell out to, and user
// alerts belong to the host app (UNUserNotificationCenter), which owns the
// notification permission prompt. The OSC 9 / BEL channel in Notify is
// likewise inert because there is no terminal attached.
func Send(title, body string) {}
