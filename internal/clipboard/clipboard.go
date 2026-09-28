// Package clipboard reads images from the macOS system clipboard.
//
// It exists for startup cost, not features: the third-party clipboard
// library it replaces on macOS loads AppKit in a package initializer, which
// cost every spettro process (including --version, ACP and headless) 1.7 to
// 2.3 ms and about 8 MB of resident memory, although only a paste into the
// TUI ever needs it. Here AppKit is loaded on the first ReadImage call.
// Linux and Windows keep the library (see internal/tui/clipboard_image_*.go).
package clipboard

import "errors"

// ErrUnavailable is returned when the clipboard holds no image, or the
// pasteboard cannot be reached.
var ErrUnavailable = errors.New("clipboard unavailable")
