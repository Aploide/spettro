//go:build !windows

package safeio

import "errors"

// errSharingViolation has no Unix equivalent; a sentinel that matches nothing
// keeps Replace's error test uniform across platforms.
var errSharingViolation = errors.New("sharing violation")
