package notify

import (
	"fmt"
	"os/exec"
	"runtime"
)

// Send fires a desktop notification with the given title and body.
// It is best-effort: errors are silently swallowed so a missing
// notification daemon never crashes the TUI.
func Send(title, body string) {
	switch runtime.GOOS {
	case "linux":
		_ = exec.Command("notify-send", "--urgency=low", "--expire-time=5000", title, body).Start()
	case "darwin":
		script := fmt.Sprintf(`display notification %q with title %q`, body, title)
		_ = exec.Command("osascript", "-e", script).Start()
	}
}
