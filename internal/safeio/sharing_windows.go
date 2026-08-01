//go:build windows

package safeio

import "golang.org/x/sys/windows"

// errSharingViolation is ERROR_SHARING_VIOLATION, which Windows returns when
// the destination is open in a mode that forbids replacement. It is distinct
// from the access-denied case and neither maps onto os.ErrPermission reliably,
// so both are matched.
var errSharingViolation error = windows.ERROR_SHARING_VIOLATION
