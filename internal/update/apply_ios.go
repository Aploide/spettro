//go:build ios

package update

import (
	"context"
	"errors"
	"fmt"
)

// ErrUnsupported reports that in-place self-update is not available on this
// platform. On iOS the engine is linked into the host app and shipped with it;
// replacing the running code is both impossible (the app bundle is read-only
// and code-signature enforced) and against App Store guideline 2.5.2 —
// updates arrive through the App Store with a new app build.
var ErrUnsupported = fmt.Errorf("self-update: %w on iOS; update the app instead", errors.ErrUnsupported)

// Apply is not implemented on iOS. LatestRelease (update.go) still works, so
// callers can keep reporting that a newer engine exists without offering to
// install it.
func Apply(_ context.Context, _ *Release) (string, error) {
	return "", ErrUnsupported
}
