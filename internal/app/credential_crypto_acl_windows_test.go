//go:build windows

package app

// 本文件是主密钥测试夹具的 Windows 半边：用 `icacls` 把目录 / 文件的 DACL 收成
// 「当前用户 + SYSTEM + Administrators」，并给负向用例提供追加 ACE 的工具。
// 临时目录默认继承 AppContainer 包 SID 等宽松 ACE，不收口的话正向用例会被 DACL
// 校验（正确地）拒绝。
// Windows half of the master-key test fixture: icacls tightens or loosens the DACL so
// both the positive and the negative DACL cases are deterministic.

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// 白名单 SID 的文本形态（与生产校验使用同一组常量；这里写成字面量便于 icacls）。
// The allowed well-known SIDs, written out for icacls.
const (
	testLocalSystemSID     = "S-1-5-18"
	testAdministratorsSID  = "S-1-5-32-544"
	testEveryoneSID        = "S-1-1-0"
	testBuiltinUsersSID    = "S-1-5-32-545"
	testDirInheritFullPerm = "(OI)(CI)F"
)

// tightenMasterKeyACLForTest 用 `/inheritance:r` 清掉继承来的宽松 ACE，再只授予
// 当前用户、SYSTEM 与 Administrators 完全控制（目录带继承标志）。
// tightenMasterKeyACLForTest removes inherited ACEs and grants only the three trustees.
func tightenMasterKeyACLForTest(t *testing.T, path string, isDir bool) {
	t.Helper()
	user, err := currentProcessUserSID()
	require.NoError(t, err)
	perms := "F"
	if isDir {
		perms = testDirInheritFullPerm
	}
	runICACLS(t, path, "/inheritance:r", "/grant:r",
		"*"+user.String()+":"+perms,
		"*"+testLocalSystemSID+":"+perms,
		"*"+testAdministratorsSID+":"+perms)
}

// grantMasterKeyACLForTest 追加一条 allow ACE，用于「未授权主体获得读 / 写」的负向用例。
// grantMasterKeyACLForTest adds one allow ACE for the negative cases.
func grantMasterKeyACLForTest(t *testing.T, path, sid, perms string) {
	t.Helper()
	runICACLS(t, path, "/grant", "*"+sid+":"+perms)
}

// runICACLS 执行 icacls 并要求成功；输出按系统代码页原样带入失败信息。
// runICACLS runs icacls and requires success.
func runICACLS(t *testing.T, path string, args ...string) {
	t.Helper()
	commandArgs := append([]string{path}, args...)
	out, err := exec.Command("icacls", commandArgs...).CombinedOutput()
	require.NoErrorf(t, err, "icacls 失败（输出为系统代码页）: %s", out)
}
