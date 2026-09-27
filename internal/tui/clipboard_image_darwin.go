//go:build darwin && !ios && !arm && !386

package tui

import "spettro/internal/clipboard"

// readClipboardImage returns the PNG image on the macOS clipboard. AppKit is
// loaded on the first call, not at process start (see internal/clipboard).
func readClipboardImage() ([]byte, error) {
	return clipboard.ReadImage()
}
