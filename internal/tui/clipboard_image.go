package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
)

var errClipboardPlatformUnsupported = errors.New("clipboard image paste is not supported on this platform")

type pasteImageMsg struct {
	path    string
	counter int
	err     error
}

// detectImageFormat inspects the leading bytes to identify PNG, JPEG, or WebP.
// Returns the file extension (e.g. ".png") and MIME type, or empty strings if
// the format is not recognised / not supported.
func detectImageFormat(data []byte) (ext, mediaType string) {
	if len(data) < 4 {
		return "", ""
	}
	// PNG: 89 50 4E 47
	if data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' {
		return ".png", "image/png"
	}
	// JPEG: FF D8
	if data[0] == 0xFF && data[1] == 0xD8 {
		return ".jpg", "image/jpeg"
	}
	// WebP: RIFF....WEBP
	if len(data) >= 12 &&
		data[0] == 'R' && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' &&
		data[8] == 'W' && data[9] == 'E' && data[10] == 'B' && data[11] == 'P' {
		return ".webp", "image/webp"
	}
	return "", ""
}

// readClipboardImageCmd returns a tea.Cmd that reads an image from the system
// clipboard and saves it to tempDir as clipboard-{counter}.{ext}.
func readClipboardImageCmd(tempDir string, counter int) tea.Cmd {
	return func() tea.Msg {
		data, err := readClipboardImage()
		if err != nil {
			return pasteImageMsg{counter: counter, err: err}
		}
		ext, _ := detectImageFormat(data)
		if ext == "" {
			return pasteImageMsg{counter: counter, err: fmt.Errorf("clipboard does not contain a supported image (jpg, png, webp)")}
		}
		path := filepath.Join(tempDir, fmt.Sprintf("clipboard-%d%s", counter, ext))
		if err := os.WriteFile(path, data, 0600); err != nil {
			return pasteImageMsg{counter: counter, err: fmt.Errorf("save clipboard image: %w", err)}
		}
		return pasteImageMsg{path: path, counter: counter}
	}
}
