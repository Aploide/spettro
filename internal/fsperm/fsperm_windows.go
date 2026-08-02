//go:build windows

package fsperm

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// restrictToOwner replaces the path's DACL with a single entry granting the
// current user full control.
//
// PROTECTED_DACL_SECURITY_INFORMATION is the part that matters: without it the
// ACEs inherited from the parent directory stay in force, so a secrets file
// created inside a world-readable project folder would remain world-readable
// no matter what the new DACL says. Administrators and SYSTEM are deliberately
// not listed — they can always take ownership, so naming them would weaken the
// stated guarantee without changing who can actually read the file.
func restrictToOwner(path string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		// On a directory this propagates to anything created inside it, so
		// secrets written later start out owner-only too. It is ignored for a
		// file, which has nothing to inherit it.
		Inheritance: windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("build DACL for %s: %w", path, err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("restrict %s to owner: %w", path, err)
	}
	return nil
}

// isOwnerOnly reports whether every entry in the path's effective DACL names
// the current user.
//
// Inheritance is deliberately not consulted. GetNamedSecurityInfo returns the
// DACL that is actually enforced, inherited entries included, so enumerating
// it answers the real question. Requiring a protected DACL instead would call
// a file "not owner-only" precisely when it correctly inherited owner-only
// access from a directory SecureMkdirAll had locked down.
func isOwnerOnly(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, fmt.Errorf("read security info for %s: %w", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, fmt.Errorf("read DACL for %s: %w", path, err)
	}
	// A nil DACL is not "no access" — it grants everyone full control.
	if dacl == nil {
		return false, nil
	}

	self, err := currentUserSID()
	if err != nil {
		return false, err
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, fmt.Errorf("read ACE %d of %s: %w", i, path, err)
		}
		if !aceSID(ace).Equals(self) {
			return false, nil
		}
	}
	return true, nil
}

// aceSID locates the SID stored inline after an ACE's fixed header. The
// Windows layout appends the SID directly to the structure, which is why
// x/sys/windows exposes only its leading uint32 as SidStart.
func aceSID(ace *windows.ACCESS_ALLOWED_ACE) *windows.SID {
	return (*windows.SID)(unsafe.Pointer(&ace.SidStart))
}

// currentUserSID returns the SID of the account this process runs as.
func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("resolve current user SID: %w", err)
	}
	return user.User.Sid, nil
}
