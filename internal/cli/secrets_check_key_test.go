package cli_test

// 本文件判 `secrets check-key`（US-25 FR-25-13 / §3.1.4）的命令面契约：离线、无数据库、
// 无实例锁；缺省工作区推导为 `<WorkDir>/.basalt`；通过只打印公开摘要，失败只点名原因。
//
// 逆改点（每条对应一种真实事故）：
//   - 缺省工作区没推导、或推导成 `<WorkDir>` 本身 → 第一个子测试的 workspace 断言红
//     （密钥“在工作区外”的判据会跟着错）；
//   - 失败路径把密钥字节 / base64 原文带进输出 → 不泄露断言红（CI 日志里出现可用凭据）；
//   - nil 端口退回“没有密钥”的文案 → 装配缺件断言红（两种原因的处置完全不同）；
//   - 缺 `--path` 仍调用端口 → 零调用断言红（会用空路径污染错误来源）。
//
// 权限语义的实测（`0700` 目录 + `0600` 文件通过、`0755` 父目录拒绝）落在
// `internal/app/master_key_check_test.go`：`cli` 包不得 import `app`（报告类型分开正是
// 为了这条依赖方向），真实 `LoadMasterKey` 判据在那一侧执行。
// This file pins the command-surface contract; the real permission semantics live in the
// app package, which the cli package must never import.

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
)

// fakeMasterKeyCheck 记录入参并回报预置结果；密钥材料的"泄露"只能由报告字段带来，
// 而报告类型本身没有密钥字段——断言因此判的是"命令面没有别的输出通道"。
// fakeMasterKeyCheck records its inputs and returns canned results.
type fakeMasterKeyCheck struct {
	calls        int
	gotPath      string
	gotWorkspace string
	report       cli.MasterKeyCheckReport
	err          error
}

func (f *fakeMasterKeyCheck) Check(path, workspaceRoot string) (cli.MasterKeyCheckReport, error) {
	f.calls++
	f.gotPath = path
	f.gotWorkspace = workspaceRoot
	return f.report, f.err
}

// runCheckKey 执行一次 `secrets check-key`，返回输出与错误。
// runCheckKey executes one check-key invocation and returns its output and error.
func runCheckKey(t *testing.T, deps cli.Deps, args ...string) (string, error) {
	t.Helper()
	root := cli.NewRootCommand(deps)
	root.SetArgs(append([]string{"secrets", "check-key"}, args...))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(context.Background())
	return stdout.String(), err
}

// TestCLI_SecretsCheckKeyOffline 覆盖命令面的四件事：缺省工作区、显式覆盖、失败非零退出
// 且不泄露、装配缺件失败关闭。
// TestCLI_SecretsCheckKeyOffline covers defaults, overrides, failure status and wiring.
func TestCLI_SecretsCheckKeyOffline(t *testing.T) {
	// 判"输出不含密钥内容"用的哨兵：真实密钥是 32 字节 base64（44 字符），任何形态都
	// 不该出现在命令输出里。
	const keyMaterialSentinel = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

	t.Run("成功：缺省工作区为 <WorkDir>/.basalt 且只打印公开摘要", func(t *testing.T) {
		workDir := t.TempDir()
		port := &fakeMasterKeyCheck{report: cli.MasterKeyCheckReport{
			KeyID:       "k_01j9test0000000000000000",
			Fingerprint: "0123456789abcdef",
		}}
		deps := cli.Deps{WorkDir: workDir, MasterKeyCheck: port}

		stdout, err := runCheckKey(t, deps, "--path", "/run/basalt-secrets/master.key")
		require.NoError(t, err)
		require.Equal(t, 1, port.calls)
		require.Equal(t, "/run/basalt-secrets/master.key", port.gotPath)
		require.Equal(t, filepath.Join(workDir, ".basalt"), port.gotWorkspace,
			"缺省工作区必须是 <WorkDir>/.basalt——它是“密钥在工作区外”判据的输入")
		require.Contains(t, stdout, "k_01j9test0000000000000000")
		require.Contains(t, stdout, "0123456789abcdef")
		require.NotContains(t, stdout, keyMaterialSentinel)
		require.False(t, strings.Contains(strings.ToLower(stdout), "key="),
			"输出只回 key_id 与指纹，不回密钥字段")
	})

	t.Run("显式 --workspace 覆盖缺省", func(t *testing.T) {
		port := &fakeMasterKeyCheck{report: cli.MasterKeyCheckReport{KeyID: "k_x", Fingerprint: "ffffffffffffffff"}}
		deps := cli.Deps{WorkDir: t.TempDir(), MasterKeyCheck: port}

		_, err := runCheckKey(t, deps, "--path", "/keys/master.key", "--workspace", "/data/.basalt")
		require.NoError(t, err)
		require.Equal(t, "/data/.basalt", port.gotWorkspace)
	})

	t.Run("端口失败：错误上抛（退出码 1）且不回显密钥内容", func(t *testing.T) {
		port := &fakeMasterKeyCheck{err: errors.New(
			"凭据不可用: 主密钥目录权限为 0755，必须为 0700")}
		deps := cli.Deps{WorkDir: t.TempDir(), MasterKeyCheck: port}

		stdout, err := runCheckKey(t, deps, "--path", "/run/secrets/master.key")
		require.Error(t, err, "父目录过宽必须让命令失败（Worker 预检据此 exit 78）")
		require.Equal(t, 1, cli.ExitStatusOf(err), "check-key 只有 0 / 1 两档")
		require.Contains(t, err.Error(), "必须为 0700", "错误必须点名权限要求")
		require.NotContains(t, err.Error(), keyMaterialSentinel)
		require.NotContains(t, stdout, keyMaterialSentinel)
	})

	t.Run("缺 --path 被拒绝且零调用", func(t *testing.T) {
		port := &fakeMasterKeyCheck{report: cli.MasterKeyCheckReport{KeyID: "k_x"}}
		deps := cli.Deps{WorkDir: t.TempDir(), MasterKeyCheck: port}

		_, err := runCheckKey(t, deps)
		require.Error(t, err)
		require.Contains(t, err.Error(), "--path")
		require.Zero(t, port.calls, "空路径不得交给端口——否则错误来源被污染")
	})

	t.Run("nil 端口是装配错误，不冒充“没有密钥”", func(t *testing.T) {
		_, err := runCheckKey(t, cli.Deps{WorkDir: t.TempDir()}, "--path", "/keys/master.key")
		require.Error(t, err)
		require.Contains(t, err.Error(), "离线端口未装配")
		require.NotContains(t, err.Error(), "主密钥文件不存在",
			"装配缺件与部署状态是两回事，文案不得混用")
	})
}
