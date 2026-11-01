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
	"os"
	"path/filepath"
	"strings"
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

// TestSecrets_CommandVerifyFailsOnCurrentUnusable：(b) 当前可用性失败同样必须让命令
// 非零退出，并打出分类明细——判卷者要能一眼区分"历史密文坏了"与"当前凭据不可用"。
// 逆改点：成功判据只数 (a) 的 `Failed` → 本用例无错误、无明细行，红。
// TestSecrets_CommandVerifyFailsOnCurrentUnusable fails on the current-usability group.
func TestSecrets_CommandVerifyFailsOnCurrentUnusable(t *testing.T) {
	fake := &fakeSecrets{verify: SecretsVerifyReport{
		Checked: 2, CurrentChecked: 1, CurrentFailed: 1, TargetMismatch: 1,
	}}

	out, err := executeSecrets(t, Deps{Secrets: fake}, "secrets", "verify")
	require.Error(t, err)
	require.Contains(t, err.Error(), "1 条托管凭据校验失败")
	require.Contains(t, out, "托管凭据检查 2 条，失败 0 条")
	require.Contains(t, out, "当前可用性：检查 1 / 失败 1")
	require.Contains(t, out, "目标地址不一致 1")
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

// fakeSecretsRecovery 是离线记录恢复端口的判卷替身：记录调用次数与请求——命令面若有
// 校验被跳过，`calls` 会先变红。
// fakeSecretsRecovery counts port calls so "refused before the port" stays decidable.
type fakeSecretsRecovery struct {
	calls int
	req   RecoverRecordRequest
}

func (f *fakeSecretsRecovery) RecoverSecretsRecord(_ context.Context, req RecoverRecordRequest) (RecoverRecordResult, error) {
	f.calls++
	f.req = req
	return RecoverRecordResult{}, nil
}

// fakeLegacyAdopt 是受控升级端口的判卷替身，语义同上。
// fakeLegacyAdopt counts adopt-legacy port calls.
type fakeLegacyAdopt struct {
	calls int
	req   AdoptLegacyRequest
}

func (f *fakeLegacyAdopt) AdoptLegacySecrets(_ context.Context, req AdoptLegacyRequest) (AdoptLegacyResult, error) {
	f.calls++
	f.req = req
	return AdoptLegacyResult{}, nil
}

// writeAnchorBackup 造一个只含清单文件的备份目录，返回目录与其清单字节哈希。
// writeAnchorBackup builds a manifest-only backup dir and returns its byte digest.
func writeAnchorBackup(t *testing.T) (dir, digest string) {
	t.Helper()
	dir = t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"format_version":1}`), 0o600))
	sum, err := sha256FileHex(path)
	require.NoError(t, err)
	return dir, sum
}

// TestSecretsRecoverRecord_RefusesUnsafeInvocation：只要有一项前置不信（确认 / 形态 /
// 锚点），命令面就必须在**调用端口之前**拒绝，并按 §3.1.8b 给对应退出码。
// 逆改点：先调端口再判确认 / 锚点 → 零调用断言红；把 backup_unverified 归为推动 2 →
// 退出码断言红。
// TestSecretsRecoverRecord_RefusesUnsafeInvocation keeps every refusal ahead of the port.
func TestSecretsRecoverRecord_RefusesUnsafeInvocation(t *testing.T) {
	port := &fakeSecretsRecovery{}
	deps := Deps{SecretsRecovery: port}
	ws := t.TempDir()
	zeros := strings.Repeat("0", 64)

	// 缺 --confirm：先拒绝、零调用、退出 2。
	out, err := executeSecrets(t, deps, "secrets", "recover-record",
		"--workspace", ws, "--backup", ws, "--expect-backup-sha256", zeros)
	require.Error(t, err)
	require.Contains(t, out, "--confirm")
	require.Zero(t, port.calls, "缺确认时不得调用恢复端口")
	require.Equal(t, 2, ExitStatusOf(err))

	// 空目录两条路线都不像：backup_invalid、退出 1、零调用。
	out, err = executeSecrets(t, deps, "secrets", "recover-record",
		"--workspace", ws, "--backup", ws, "--expect-backup-sha256", zeros, "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "backup_invalid")
	require.Equal(t, 1, ExitStatusOf(err))
	require.Zero(t, port.calls)

	// manifest.json 与 basalt.db + instance.json 同时成立：形态二义，同样拒绝。
	mixed := t.TempDir()
	for _, name := range []string{"manifest.json", "basalt.db", "instance.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(mixed, name), []byte("{}"), 0o600))
	}
	out, err = executeSecrets(t, deps, "secrets", "recover-record",
		"--workspace", ws, "--backup", mixed, "--expect-backup-sha256", zeros, "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "backup_invalid")
	require.Zero(t, port.calls, "备份形态二义时不得调用恢复端口")

	// 形态明确但缺外部锚点 / 锚点与清单不符：backup_unverified、退出 1、零调用。
	exported, digest := writeAnchorBackup(t)
	out, err = executeSecrets(t, deps, "secrets", "recover-record",
		"--workspace", ws, "--backup", exported, "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "backup_unverified")
	require.Equal(t, 1, ExitStatusOf(err))
	require.Zero(t, port.calls, "缺锚点时不得调用恢复端口")

	out, err = executeSecrets(t, deps, "secrets", "recover-record",
		"--workspace", ws, "--backup", exported, "--expect-backup-sha256", zeros, "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "backup_unverified")
	require.Zero(t, port.calls, "锚点不符时不得调用恢复端口")

	// 正面路径：锚点逐字相符才放行，且交给端口的是命令面重算过的哈希（恰一次）。
	_, err = executeSecrets(t, deps, "secrets", "recover-record",
		"--workspace", ws, "--backup", exported, "--expect-backup-sha256", digest, "--confirm")
	require.NoError(t, err)
	require.Equal(t, 1, port.calls)
	require.Equal(t, digest, port.req.ManifestSHA256)
	require.Equal(t, BackupKindExportPackage, port.req.Kind)

	// 端口缺件与参数错误可区分：前者报「未装配」，不冒充备份问题。
	out, err = executeSecrets(t, Deps{}, "secrets", "recover-record", "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "未装配")
	require.Contains(t, out, "recover-record")
}

// TestSecretsAdoptLegacy_RefusesUnsafeInvocation：见证、取钥锚点、备份形态与清单锚点
// 四项前置任一不信即拒绝，全部发生在端口调用之前（§3.1.8c）。
// 逆改点：允许见证缺失时默认相信 → attestation_missing 子例变绿（应红）；空备份的
// 文案丢掉 env-only / 未批准 → 处置提示断言红。
// TestSecretsAdoptLegacy_RefusesUnsafeInvocation keeps every refusal ahead of the port.
func TestSecretsAdoptLegacy_RefusesUnsafeInvocation(t *testing.T) {
	port := &fakeLegacyAdopt{}
	deps := Deps{LegacySecretsAdopt: port}
	ws := t.TempDir()
	zeros := strings.Repeat("0", 64)

	// 缺 --confirm：退出 2、零调用。
	out, err := executeSecrets(t, deps, "secrets", "adopt-legacy",
		"--workspace", ws, "--backup", ws, "--expect-backup-sha256", zeros, "--source", ws)
	require.Error(t, err)
	require.Contains(t, out, "--confirm")
	require.Zero(t, port.calls, "缺确认时不得调用升级端口")
	require.Equal(t, 2, ExitStatusOf(err))

	// 见证二选一：同时给两种 → source_witness_conflict；都不给 → attestation_missing。
	out, err = executeSecrets(t, deps, "secrets", "adopt-legacy",
		"--workspace", ws, "--backup", ws, "--expect-backup-sha256", zeros,
		"--source", ws, "--attest-source-unchanged", "ticket-1", "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "source_witness_conflict")
	require.Equal(t, 2, ExitStatusOf(err))
	require.Zero(t, port.calls)

	out, err = executeSecrets(t, deps, "secrets", "adopt-legacy",
		"--workspace", ws, "--backup", ws, "--expect-backup-sha256", zeros, "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "attestation_missing")
	require.Equal(t, 2, ExitStatusOf(err), "缺见证按前置不满足收场")
	require.Zero(t, port.calls, "缺见证时不得调用升级端口")

	// 覆盖取钥锚点只能配覆盖路径：只给锚点 → 退出 2、零调用。
	out, err = executeSecrets(t, deps, "secrets", "adopt-legacy",
		"--workspace", ws, "--backup", ws, "--expect-backup-sha256", zeros,
		"--source", ws, "--expect-key-sha256", zeros, "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "--master-key-file")
	require.Equal(t, 2, ExitStatusOf(err))
	require.Zero(t, port.calls)

	// 备份不可用（空目录缺 backup.json）：backup_invalid、退出 1，并给出可操作处置——
	// env-only 降级与「破坏性重置未批准」，不得暗示可以就地迁移或重置。
	out, err = executeSecrets(t, deps, "secrets", "adopt-legacy",
		"--workspace", ws, "--backup", t.TempDir(), "--expect-backup-sha256", zeros,
		"--source", ws, "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "backup_invalid")
	require.Contains(t, out, "env-only", "无备份时给出可操作降级路径")
	require.Contains(t, out, "未批准", "破坏性重置只能标为候选，不得宣称可用")
	require.Equal(t, 1, ExitStatusOf(err))
	require.Zero(t, port.calls, "备份不可用时不得调用升级端口")

	// 清单在但锚点不符：backup_unverified、退出 1、零调用。
	good := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(good, "backup.json"), []byte(`{"format_version":1}`), 0o600))
	sum, err := sha256FileHex(filepath.Join(good, "backup.json"))
	require.NoError(t, err)
	out, err = executeSecrets(t, deps, "secrets", "adopt-legacy",
		"--workspace", ws, "--backup", good, "--expect-backup-sha256", zeros,
		"--source", ws, "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "backup_unverified")
	require.Zero(t, port.calls, "锚点不符时不得调用升级端口")

	// 正面路径：四项前置全过才放行，交给端口的是命令面重算的清单哈希（恰一次）。
	_, err = executeSecrets(t, deps, "secrets", "adopt-legacy",
		"--workspace", ws, "--backup", good, "--expect-backup-sha256", sum,
		"--source", ws, "--confirm")
	require.NoError(t, err)
	require.Equal(t, 1, port.calls)
	require.Equal(t, sum, port.req.ManifestSHA256)
	require.Equal(t, ws, port.req.Source)
	require.True(t, port.req.Confirm)

	// 端口缺件与参数错误可区分。
	out, err = executeSecrets(t, Deps{}, "secrets", "adopt-legacy", "--confirm")
	require.Error(t, err)
	require.Contains(t, out, "未装配")
	require.Contains(t, out, "adopt-legacy")
}
