package cli

// 本文件是 `secrets` 命令组的展示层判卷（颗粒度文档 R4.8 / T135）：
//   - init 只打印公开标识与托管依赖计数，**不**打印密钥、密文或地址；
//   - status 未初始化只影响文案，不影响退出码；
//   - verify 有失败条数即非零退出（脚本判卷面），明细留给结构化日志；
//   - revoke-managed 缺 `--confirm` 时零副作用、零调用；
//   - 端口缺件报「未装配」，不冒充「没有密钥」。
//
// 判卷替身只记录"命令层把什么传给了端口"与"调了几次"——实现语义在
// `internal/app/credential_ops_test.go` 的 `TestSecrets_*` 里判。
// The fake port records calls only; the service semantics live in internal/app.

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeSecrets 是 Secrets 端口的判卷替身：预置结果、记录调用次数。
// fakeSecrets returns canned results and counts calls.
type fakeSecrets struct {
	initStatus   SecretsStatus
	initErr      error
	status       SecretsStatus
	statusErr    error
	verify       SecretsVerifyReport
	verifyErr    error
	revokeErr    error
	initCalls    int
	statusCalls  int
	verifyCalls  int
	revokeCalls  int
	revokedCount int
}

func (f *fakeSecrets) Init(context.Context) (SecretsStatus, error) {
	f.initCalls++
	return f.initStatus, f.initErr
}

func (f *fakeSecrets) Status(context.Context) (SecretsStatus, error) {
	f.statusCalls++
	return f.status, f.statusErr
}

func (f *fakeSecrets) Verify(context.Context) (SecretsVerifyReport, error) {
	f.verifyCalls++
	return f.verify, f.verifyErr
}

func (f *fakeSecrets) RevokeManaged(context.Context) (int, error) {
	f.revokeCalls++
	return f.revokedCount, f.revokeErr
}

// executeSecrets 在注入依赖上跑一次**真根命令**：注册、参数校验与退出码都在被测面内。
// executeSecrets runs the real root command so registration and the exit code stay
// under test.
func executeSecrets(t *testing.T, deps Deps, args ...string) (string, error) {
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

// TestSecrets_CommandInitPrintsPublicIdentifiers：init 的成功输出只有 key_id、指纹与
// 托管依赖计数；调用恰好一次。
// TestSecrets_CommandInitPrintsPublicIdentifiers pins the public-only init output.
func TestSecrets_CommandInitPrintsPublicIdentifiers(t *testing.T) {
	fake := &fakeSecrets{initStatus: SecretsStatus{
		Initialized:             true,
		KeyID:                   "k_test",
		Fingerprint:             "9f2c1d34",
		ManagedModelConnections: 1,
		ManagedNotifyChannels:   2,
	}}

	out, err := executeSecrets(t, Deps{Secrets: fake}, "secrets", "init")
	require.NoError(t, err)
	require.Contains(t, out, "主密钥已生成：key_id=k_test fingerprint=9f2c1d34")
	require.Contains(t, out, "模型连接 1 个", "init 必须报托管依赖计数")
	require.Contains(t, out, "通知渠道 2 个")
	require.Equal(t, 1, fake.initCalls)
}

// TestSecrets_CommandStatusUninitialised：未初始化是部署状态而非命令失败——文案明确，
// 退出码为零，且不调用 init / verify。
// TestSecrets_CommandStatusUninitialised keeps an uninitialised key a state, not a failure.
func TestSecrets_CommandStatusUninitialised(t *testing.T) {
	fake := &fakeSecrets{status: SecretsStatus{Initialized: false}}

	out, err := executeSecrets(t, Deps{Secrets: fake}, "secrets", "status")
	require.NoError(t, err, "未初始化不得以非零退出码冒充故障")
	require.Contains(t, out, "主密钥：未初始化")
	require.Contains(t, out, "模型连接 0 个")
	require.Equal(t, 1, fake.statusCalls)
	require.Zero(t, fake.initCalls)
	require.Zero(t, fake.verifyCalls)
}

// TestSecrets_CommandVerifyFailsOnFailure：失败条数大于零即命令失败（脚本可判卷），
// 计数行照常打印——判卷者既看得见数字也拿得到非零退出码。
// TestSecrets_CommandVerifyFailsOnFailure makes a non-zero failure count a command failure.
func TestSecrets_CommandVerifyFailsOnFailure(t *testing.T) {
	fake := &fakeSecrets{verify: SecretsVerifyReport{Checked: 3, Failed: 1}}

	out, err := executeSecrets(t, Deps{Secrets: fake}, "secrets", "verify")
	require.Error(t, err)
	require.Contains(t, err.Error(), "1 条托管凭据校验失败")
	require.Contains(t, out, "托管凭据检查 3 条，失败 1 条")
	require.Equal(t, 1, fake.verifyCalls)
}

// TestSecrets_CommandRevokeRequiresConfirm：缺 `--confirm` 时**零调用**；带确认才执行。
// 逆改点：先调端口再判确认 → 零调用断言红。
// TestSecrets_CommandRevokeRequiresConfirm refuses before touching the service.
func TestSecrets_CommandRevokeRequiresConfirm(t *testing.T) {
	fake := &fakeSecrets{revokedCount: 2}

	out, err := executeSecrets(t, Deps{Secrets: fake}, "secrets", "revoke-managed")
	require.Error(t, err)
	require.Contains(t, err.Error(), "--confirm")
	require.Zero(t, fake.revokeCalls, "缺确认时不得调用服务面")
	require.NotContains(t, out, "已撤销")

	out, err = executeSecrets(t, Deps{Secrets: fake}, "secrets", "revoke-managed", "--confirm")
	require.NoError(t, err)
	require.Equal(t, 1, fake.revokeCalls)
	require.Contains(t, out, "已撤销 2 条托管凭据")
}

// TestSecrets_CommandMissingPort：四路子命令在端口缺件时都报「未装配」——不把装配缺口
// 说成「没有密钥」（两者的处置完全不同）。
// TestSecrets_CommandMissingPort keeps a wiring gap distinct from a deployment state.
func TestSecrets_CommandMissingPort(t *testing.T) {
	for _, args := range [][]string{
		{"secrets", "init"},
		{"secrets", "status"},
		{"secrets", "verify"},
		{"secrets", "revoke-managed", "--confirm"},
	} {
		out, err := executeSecrets(t, Deps{}, args...)
		require.Error(t, err, "子命令 %v 必须报缺件", args)
		require.Contains(t, out, "未装配")
	}
}

// TestSecrets_CommandUnknownSubcommand：未知子命令非零退出，不被当成「请求帮助」。
// TestSecrets_CommandUnknownSubcommand rejects unknown subcommands with a non-zero exit.
func TestSecrets_CommandUnknownSubcommand(t *testing.T) {
	out, err := executeSecrets(t, Deps{}, "secrets", "rotate")
	require.Error(t, err)
	require.Contains(t, out, "未知命令")
}
