//go:build e2e && windows

package e2e

// 本文件是 e2e 主密钥夹具的 Windows 半边：`t.TempDir()` 默认继承 AppContainer 包 SID
// 与历史账户等宽松 ACE，`secrets init` 的 DACL 校验会（正确地）拒绝。部署侧按
// `docs/Operations.md` 用 `icacls` 把目录收成最小权限，夹具照同一形态收口。
// Windows half of the e2e master-key fixture: temp dirs inherit loose ACEs, so the DACL
// check refuses them; the fixture tightens the directory exactly like the deployment step.

import (
	"os/exec"
	"os/user"
	"testing"

	"github.com/stretchr/testify/require"
)

// 白名单受托人（与生产校验同一组主体，写成 SID 便于 icacls）。
// The allowed trustees, written as SIDs for icacls.
const (
	e2eLocalSystemSID    = "S-1-5-18"
	e2eAdministratorsSID = "S-1-5-32-544"
	e2eDirInheritFull    = "(OI)(CI)F"
)

// tightenMasterKeyDirACL 清掉继承 ACE，只授予当前用户、SYSTEM 与 Administrators；
// 目录带 OI/CI，随后由 `secrets init` 创建的主密钥文件继承同一组受托人。
// tightenMasterKeyDirACL removes inherited ACEs and grants only the three trustees.
func tightenMasterKeyDirACL(t *testing.T, dir string) {
	t.Helper()
	runE2EICACLS(t, dir,
		"/inheritance:r", "/grant:r",
		currentUserName(t)+":"+e2eDirInheritFull,
		"*"+e2eLocalSystemSID+":"+e2eDirInheritFull,
		"*"+e2eAdministratorsSID+":"+e2eDirInheritFull)
}

// currentUserName 返回 `DOMAIN\user`：`os/user` 的 Windows 实现直接读令牌，
// 不走 PATH——Git for Windows 的 `whoami` 也在 PATH 上，按名调用会拿错二进制。
// currentUserName returns DOMAIN\user from the token; PATH is not consulted because
// Git for Windows ships its own whoami.
func currentUserName(t *testing.T) string {
	t.Helper()
	current, err := user.Current()
	require.NoErrorf(t, err, "读取当前用户失败: %v", err)
	return current.Username
}

// runE2EICACLS 执行 icacls 并要求成功。
// runE2EICACLS runs icacls and requires success.
func runE2EICACLS(t *testing.T, path string, args ...string) {
	t.Helper()
	out, err := exec.Command("icacls", append([]string{path}, args...)...).CombinedOutput()
	require.NoErrorf(t, err, "icacls 失败（输出为系统代码页）: %s", out)
}
