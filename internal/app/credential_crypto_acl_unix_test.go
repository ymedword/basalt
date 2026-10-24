//go:build !windows

package app

// 本文件是主密钥测试夹具的 Unix 半边：权限已由 `os.Chmod` 收口，无需额外动作。
// Unix half of the master-key test fixture: modes are already restricted by os.Chmod.

import "testing"

// tightenMasterKeyACLForTest 在 Unix 上不做事；签名与 Windows 分支保持一致。
// tightenMasterKeyACLForTest is a no-op on Unix; it exists to match the Windows helper.
func tightenMasterKeyACLForTest(_ *testing.T, _ string, _ bool) {}
