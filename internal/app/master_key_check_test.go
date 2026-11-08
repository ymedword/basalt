package app

// 本文件判 `secrets check-key` 的装配侧实现是否**原样复用** `LoadMasterKey` 的判据
// （US-25 FR-25-13 / §2.3.5）：Worker 预检的两侧证据就是这里——
// `0700` 目录 + `0400` 文件通过，同一把密钥放进 `0755` 父目录被拒。
//
// 为什么不在 `internal/cli` 判：`cli` 不得 import `app`（端口报告类型分开正是为了这条
// 依赖方向），真实 `LoadMasterKey` 语义只能在这一侧执行；命令面契约见
// `internal/cli/secrets_check_key_test.go`。
// This file pins the real LoadMasterKey reuse; the command surface is pinned in the cli
// package, which must never import app.

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMasterKeyCheck_ReusesLoadKeyPermissions 覆盖四个面：通过路径、父目录过宽拒绝、
// 工作区内拒绝、错误与报告都不含密钥材料。
//
// 逆改点：把 `masterKeyCheckPort.Check` 改成只 `os.Stat` 文件（不判父目录）→ `0755`
// 断言红；去掉 `requireOutsideWorkspace` 的复用 → 工作区断言红；把 `LoadMasterKey` 换成
// 自带解析 → `0400` / JSON 形态断言随之漂移。
// Reverting any of the reused checks turns the matching sub-assertion red.
func TestMasterKeyCheck_ReusesLoadKeyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 权限位为合成值，判卷走 DACL（见 credential_crypto_windows_test.go）；
		// 这里的两侧判据（0700 / 0755）只在 Unix 上有意义。
		t.Skip("Windows 权限位为合成值，DACL 校验见 credential_crypto_windows_test.go")
	}

	key := testMasterKey()
	t.Cleanup(func() { key.Zero() })
	encoded := base64.StdEncoding.EncodeToString(key.Key)

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, ".basalt"), 0o700))

	// 镜像内预建的 `/run/basalt-secrets`：0700 目录、密钥 0400（只读共享）。
	secretDir := filepath.Join(root, "run-basalt-secrets")
	keyPath := writeMasterKeyFile(t, secretDir, "k_check", key.Key)
	require.NoError(t, os.Chmod(keyPath, 0o400))

	port := masterKeyCheckPort{}
	report, err := port.Check(keyPath, workspace)
	require.NoError(t, err, "0700 目录 + 0400 文件必须通过（FR-25-13 正面判据）")
	require.Equal(t, "k_check", report.KeyID)
	require.Len(t, report.Fingerprint, masterKeyFingerprintLen,
		"只回公开指纹，长度与 secrets status 同一口径")
	require.NotContains(t, report.Fingerprint, encoded,
		"报告不得携带密钥材料")

	// 同一把正确的密钥放进 0755 父目录（典型：默认 /run/secrets）必须被拒。
	require.NoError(t, os.Chmod(secretDir, 0o755))
	report, err = port.Check(keyPath, workspace)
	require.Error(t, err, "父目录 0755 必须拒绝——Worker 预检据此 exit 78")
	require.Contains(t, err.Error(), "0700", "错误必须点名目录权限要求")
	require.Empty(t, report.KeyID, "失败路径不得交出任何摘要")
	require.NotContains(t, err.Error(), encoded, "错误文案不得含密钥材料")

	// 工作区内的密钥路径同样被拒（与 secrets init 同一纪律）。
	insidePath := writeMasterKeyFile(t, filepath.Join(workspace, ".basalt", "keys"), "k_inside", key.Key)
	_, err = port.Check(insidePath, workspace)
	require.Error(t, err, "工作区内的密钥不得被接受")
	require.Contains(t, err.Error(), "工作区")
}
