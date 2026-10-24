//go:build e2e && !windows

package e2e

// 本文件是 e2e 主密钥夹具的 Unix 半边：`t.TempDir()` 已是 0700，无需额外动作。
// Unix half of the e2e master-key fixture: temp dirs are already 0700.

import "testing"

// tightenMasterKeyDirACL 在 Unix 上不做事；签名与 Windows 分支保持一致。
// tightenMasterKeyDirACL is a no-op on Unix; it mirrors the Windows helper.
func tightenMasterKeyDirACL(_ *testing.T, _ string) {}
