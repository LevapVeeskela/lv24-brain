package daemon

import (
	"os"

	"golang.org/x/sys/windows"
)

func securePath(path string, _ os.FileMode, directory bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	inheritance := uint32(0)
	if directory {
		inheritance = windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.ACCESS_MASK(windows.GENERIC_ALL),
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       inheritance,
		Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid)},
	}}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}
