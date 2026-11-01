package cli

// 本文件是 `instance` 命令组的展示层判卷（颗粒度文档 §3.1.8a / T071）：
//   - 未知子命令非零退出，不被 cobra 当成「请求帮助」吞掉；
//   - 端口缺件报「未注入」，命令面**不**自己开库或写名册补第二条路径；
//   - `--workspace` 缺省派生 `<WorkDir>/.basalt`，配置取**同级**的 `<WorkDir>/basalt.yaml`
//     （§3.1.8c 路径口径：配置不属于工作区资产）；
//   - `--master-key-file` 去空白后原样透传（只做互证，不覆盖配置）；
//   - 报告只打印公开标识，**不含**密钥材料 / 密文 / 明文；拒绝时也打印已采集事实。
//
// 判卷替身只记录"命令层把什么交给了端口"；采纳语义在
// `internal/app/instance_adopt_test.go` 的 `TestInstanceAdopt_*` 里判。
// The fake port records calls only; the adoption semantics live in internal/app.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeInstanceAdoption 是 InstanceAdoption 端口的判卷替身：预置结果、记录实参。
type fakeInstanceAdoption struct {
	report InstanceAdoptReport
	err    error

	calls int
	req   InstanceAdoptRequest
}

// Adopt 记录实参并返回预置结果。
func (f *fakeInstanceAdoption) Adopt(_ context.Context, req InstanceAdoptRequest) (InstanceAdoptReport, error) {
	f.calls++
	f.req = req
	return f.report, f.err
}

// executeInstance 在注入依赖上跑一次**真根命令**：注册、参数校验与退出码都在被测面内。
func executeInstance(t *testing.T, deps Deps, args ...string) (string, error) {
	t.Helper()

	var logs bytes.Buffer
	deps.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	root := NewRootCommand(deps)
	root.SetArgs(args)

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.ExecuteContext(context.Background())
	return stdout.String() + stderr.String(), err
}

// TestInstanceCommand_UnknownSubcommand：未知子命令必须非零退出，不得静默成功。
// 逆改点：把 `instance` 组的 RunE 改回 `cmd.Help()` → 本用例拿到 nil 错误即红。
func TestInstanceCommand_UnknownSubcommand(t *testing.T) {
	out, err := executeInstance(t, Deps{}, "instance", "bogus")
	require.Error(t, err)
	require.Contains(t, err.Error(), "未知子命令")
	require.NotContains(t, out, "Usage:", "未知子命令不该被当成请求帮助")
}

// TestInstanceCommand_RequiresAdoptPort：端口缺件如实报「未注入」，且不产生任何调用。
// 逆改点：删掉 requireInstanceAdoptPort → 空指针 panic 或静默 0 退出。
func TestInstanceCommand_RequiresAdoptPort(t *testing.T) {
	out, err := executeInstance(t, Deps{}, "instance", "adopt")
	require.Error(t, err)
	require.Contains(t, err.Error(), "实例采纳端口未注入")
	require.NotContains(t, out, "写实例记录：", "缺件时命令面还没走到采纳，不得打印伪造的空报告")
}

// TestInstanceCommand_AdoptDerivesWorkspaceAndConfig：缺省路径口径——工作区取
// `<WorkDir>/.basalt`，配置取**同级**的 `<WorkDir>/basalt.yaml`。
func TestInstanceCommand_AdoptDerivesWorkspaceAndConfig(t *testing.T) {
	fake := &fakeInstanceAdoption{}
	workDir := t.TempDir()

	_, err := executeInstance(t, Deps{WorkDir: workDir, InstanceAdopt: fake}, "instance", "adopt")
	require.NoError(t, err)
	require.Equal(t, 1, fake.calls)
	require.Equal(t, filepath.Join(workDir, workspaceDirName), fake.req.Workspace)
	require.Equal(t, filepath.Join(workDir, configFileName), fake.req.ConfigPath)
	require.Empty(t, fake.req.MasterKeyFile)
}

// TestInstanceCommand_AdoptUsesExplicitWorkspaceAndKeyFile：`--workspace` 原样使用
// （配置跟着工作区走），`--master-key-file` 去空白后透传。
func TestInstanceCommand_AdoptUsesExplicitWorkspaceAndKeyFile(t *testing.T) {
	fake := &fakeInstanceAdoption{}
	ws := filepath.Join(t.TempDir(), "legacy", workspaceDirName)
	keyFile := filepath.Join(t.TempDir(), "private", "master.key")

	_, err := executeInstance(t, Deps{WorkDir: t.TempDir(), InstanceAdopt: fake},
		"instance", "adopt", "--workspace", ws, "--master-key-file", "  "+keyFile+"  ")
	require.NoError(t, err)
	require.Equal(t, ws, fake.req.Workspace)
	require.Equal(t, filepath.Join(filepath.Dir(ws), configFileName), fake.req.ConfigPath)
	require.Equal(t, keyFile, fake.req.MasterKeyFile, "参数必须去空白后再交给端口")
}

// TestInstanceCommand_AdoptPrintsReportWithoutSecrets：报告只打印公开标识。
// 断言的是**反向**判据：把 key_id / 指纹放进输出可以，把密钥材料、密文或明文放进去
// 不行——所以这里显式搜敏感字面量，出现即红。
func TestInstanceCommand_AdoptPrintsReportWithoutSecrets(t *testing.T) {
	fake := &fakeInstanceAdoption{report: InstanceAdoptReport{
		Workspace:           "/ws/.basalt",
		ConfigPath:          "/ws/basalt.yaml",
		RosterPath:          "/ws/.basalt/instance.json",
		AdoptedAt:           time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC),
		Case:                1,
		InstanceID:          "inst_0123456789abcdef",
		KeyFile:             "/private/master.key",
		KeyPresent:          true,
		KeyID:               "k_test",
		Fingerprint:         "9f2c1d34",
		RecordKeyID:         "k_test",
		RecordFingerprint:   "9f2c1d34",
		ManagedCiphertexts:  2,
		CiphertextsVerified: true,
		State:               "ready",
		Action:              "ready",
		AuditOperationID:    "op-1761700000000-abc123",
		NewPrivateDir:       "/data/basalt/instances/inst_0123456789abcdef",
	}}

	out, err := executeInstance(t, Deps{InstanceAdopt: fake}, "instance", "adopt")
	require.NoError(t, err)

	require.Contains(t, out, "命中判定：案例 1")
	require.Contains(t, out, "实例标识：inst_0123456789abcdef")
	require.Contains(t, out, "主密钥：/private/master.key（key_id=k_test 指纹=9f2c1d34）")
	require.Contains(t, out, "托管密文：2 条（逐条可解 true，不可解 0 条）")
	require.Contains(t, out, "审计操作：op-1761700000000-abc123")
	require.Contains(t, out, "写实例记录：false（采纳只写名册）")
	require.Contains(t, out, "重启复验：未验证（由随后的 serve 启动判定）")

	// 反向判据：报告里不得出现任何密钥材料 / 密文 / 明文标识。
	for _, secret := range []string{"sk-", "BEGIN", "ciphertext", "nonce", "plaintext"} {
		require.NotContains(t, out, secret, "报告不得带出凭据内容")
	}
}

// TestInstanceCommand_AdoptFailureStillPrintsReport：拒绝时同样打印已采集事实——
// "停在哪一步"比一句错误更有用；错误仍要上抛（退出码由装配层定）。
func TestInstanceCommand_AdoptFailureStillPrintsReport(t *testing.T) {
	refusal := errors.New("recovery_required: 1 / 2 条托管密文无法用当前主密钥解开")
	fake := &fakeInstanceAdoption{
		report: InstanceAdoptReport{
			Workspace:             "/ws/.basalt",
			Case:                  1,
			RecordKeyID:           "k_test",
			RecordFingerprint:     "9f2c1d34",
			ManagedCiphertexts:    2,
			CiphertextsUnreadable: 1,
			FirstFailure:          "凭据 cred_00000000000000000000000012 v1 无法用当前主密钥解密",
			State:                 "recovery_required",
			Action:                "recover",
			Reason:                "unavailable",
		},
		err: refusal,
	}

	out, err := executeInstance(t, Deps{InstanceAdopt: fake}, "instance", "adopt")
	require.ErrorIs(t, err, refusal, "错误必须上抛，命令面不得吞掉")
	require.Contains(t, out, "命中判定：案例 1")
	require.Contains(t, out, "不可解 1 条")
	require.Contains(t, out, "首个失败：凭据")
	require.Contains(t, out, "启动状态：recovery_required / recover（原因 unavailable）")
	require.NotContains(t, out, "实例标识：", "拒绝路径没有实例标识可打印")
}
