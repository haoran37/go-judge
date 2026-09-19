//go:build windows

package securefile

import (
	"os"

	"golang.org/x/sys/windows"
)

// hardenPath 在 Windows 上用 current-user-only DACL 保护敏感文件/目录，
// owner 为当前用户，禁止其他用户访问，而不是仅依赖继承 ACL。
func hardenPath(path string, _ os.FileMode) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	)
}

// SyncDir 在 Windows 上不做 POSIX 目录 fsync；文件权限由 ACL 加固覆盖。
func SyncDir(_ string) error {
	return nil
}
