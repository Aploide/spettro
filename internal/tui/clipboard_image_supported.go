//go:build (linux || windows) && !arm && !386 && !android

// Linux and Windows read clipboard images through go-nativeclipboard, which
// loads the platform library (libX11, user32) in its package initializer.
// macOS uses internal/clipboard instead, which loads AppKit on first use
// (see clipboard_image_darwin.go).

package tui

import (
	"os"
	"os/exec"

	"github.com/aymanbagabas/go-nativeclipboard"
)

func readClipboardImage() ([]byte, error) {
	// On Wayland the native clipboard lives in the compositor, not in X11.
	// Use wl-paste to read directly from the Wayland selection.
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if path, err := exec.LookPath("wl-paste"); err == nil {
			data, err := exec.Command(path, "--no-newline", "--type", "image/png").Output()
			if err == nil && len(data) > 0 {
				return data, nil
			}
		}
	}
	return nativeclipboard.Image.Read()
}
