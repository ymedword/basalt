package app

// 本文件是凭据写面编排的判卷（模块卡 R4.8 / R4.9 / T132、T157）：
//   - 写入即追加版本：第 1 版新建身份，第 2 版只把旧版本转 `superseded`（密文保留）；
//   - 撤销是终态：密文清空、纪元 +1；重复撤销幂等；**重新录入**只追加新版本；
//   - managed → env 退役保留密文（在途可解密），不追加版本；
//   - 预写失败零写入（T157）；
//   - 四证恢复三档：applied / not_applied / indeterminate，含退役分支（T132）；
//   - 明文不落库、不进日志。
// These tests pin the credential write surface: append-only versions, terminal revoke,
// ciphertext-preserving retire, pre-write failure with zero writes, and the three-way
// four-evidence recovery including the retire branch.

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// credentialOpsFixture 在解析器夹具之上装配凭据写面：同一个工作区、同一份主密钥、
// 同一个库句柄。
// credentialOpsFixture assembles the write surface on the resolver fixture.
type credentialOpsFixture struct {
	*resolverFixture
	credOps *CredentialOps
	logs    *strings.Builder
}

// newCredentialOpsFixture 构造写面夹具；默认操作台账就是夹具库里的管理操作表。
// newCredentialOpsFixture wires the write surface onto the fixture stores.
func newCredentialOpsFixture(t *testing.T) *credentialOpsFixture {
	t.Helper()
	env := newResolverFixture(t)
	f := &credentialOpsFixture{resolverFixture: env, logs: &strings.Builder{}}
	f.credOps = newTestCredentialOps(t, env, env.opStore(), f.logs)
	return f
}

// opStore 暴露夹具的管理操作表（写面默认用它预写 pending）。
// opStore exposes the fixture management store.
func (f *resolverFixture) opStore() core.OperationStore { return f.ops }

// newTestCredentialOps 按生产依赖形状构造写面。
// newTestCredentialOps builds the write surface with production-shaped dependencies.
func newTestCredentialOps(t *testing.T, env *resolverFixture, store core.OperationStore,
	logs *strings.Builder,
) *CredentialOps {
	t.Helper()
	ops, err := NewCredentialOps(CredentialOpsDeps{
		Operations:    store,
		Credentials:   env.credentials,
		Resources:     env.versions,
		Notify:        env.admission.NotifyAdmin,
		MasterKeyFile: env.keyPath,
		WorkspaceRoot: env.root,
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:           baselineClock,
	})
	require.NoError(t, err)
	return ops
}

// connectionUID 给出夹具里某个模型连接的稳定身份。
// connectionUID returns the fixture uid of one model connection.
func connectionUID(t *testing.T, name string) core.ResourceUID {
	t.Helper()
	return fixtureResourceUID(t, core.ResourceModelConnection, name)
}

// writeConnection 按托管模式写一份 Key。
// writeConnection writes one managed key for the fixture connection.
func writeConnection(t *testing.T, f *credentialOpsFixture, name, secret string) CredentialWriteResult {
	t.Helper()
	result, err := f.credOps.Write(context.Background(), CredentialWriteRequest{
		Actor: resourceActor(),
		Kind:  core.BindingModelConnection,
		Name:  name,
		UID:   connectionUID(t, name),
		Mode:  core.CredentialModeManaged,
		Value: []byte(secret),
	})
	require.NoError(t, err)
	return result
}

// TestCredentialOps_WriteAppendsVersionAndBindsTarget：写 Key = 新建身份 + 第 1 版；
// 换 Key = 追加第 2 版并把第 1 版转 superseded（密文保留，在途 Run 仍可解密）。
// 逆改点：原地改密文 → 第 1 版密文断言红；不推进 revision → 指针断言红；
// 目标绑定算错 → 解析器按目标绑定拒绝（应用侧断言红）。
// TestCredentialOps_WriteAppendsVersionAndBindsTarget pins append-only rotation.
func TestCredentialOps_WriteAppendsVersionAndBindsTarget(t *testing.T) {
	ctx := context.Background()
	f := newCredentialOpsFixture(t)
	f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	uid := connectionUID(t, "alpha")

	before, err := f.versions.PointerByUID(ctx, core.ResourceModelConnection, uid)
	require.NoError(t, err)

	first := writeConnection(t, f, "alpha", "sk-alpha-secret-1")
	require.True(t, first.Changed)
	require.Equal(t, 1, first.VersionNo)
	require.Equal(t, before.Revision+1, first.Revision, "凭据写入推进一次资源 revision")
	require.Equal(t, last4Of("sk-alpha-secret-1"), first.Last4)
	require.Equal(t, core.CredentialIdentityActive, first.State)

	identity, err := f.credentials.CredentialByOwner(ctx, core.BindingModelConnection, uid)
	require.NoError(t, err)
	require.Equal(t, first.CredentialID, identity.ID)
	require.Equal(t, 1, identity.CurrentVersion)

	stored, err := f.credentials.Version(ctx, identity.ID, 1)
	require.NoError(t, err)
	require.NotContains(t, string(stored.Ciphertext), "sk-alpha-secret-1", "明文不得落库")
	expectedBinding, err := CredentialTargetBinding(core.BindingModelConnection,
		"https://api.example.com/v1", "")
	require.NoError(t, err)
	require.Equal(t, expectedBinding, stored.TargetBinding)

	// 解析器按当前版本解出同一份 Key：目标绑定与 AAD 逐项对得上。
	plaintext, err := f.resolver.ResolveModelCredential(ctx, core.ResourceVersionRef{
		Name: "alpha", ResourceUID: uid,
	})
	require.NoError(t, err)
	require.Equal(t, "sk-alpha-secret-1", plaintext)

	// 换 Key：新版本可用，旧版本保留密文供在途使用。
	second := writeConnection(t, f, "alpha", "sk-alpha-secret-2")
	require.Equal(t, 2, second.VersionNo)
	require.Equal(t, first.Revision+1, second.Revision)
	rotated, err := f.credentials.Version(ctx, identity.ID, 1)
	require.NoError(t, err)
	require.Equal(t, core.CredentialVersionSuperseded, rotated.State)
	require.NotEmpty(t, rotated.Ciphertext, "换 Key 不得清空旧版本密文")

	// 操作台账：两次写入各留一行，动作字面量与目标都是契约口径。
	list, err := f.ops.ListOperations(ctx, core.OperationFilter{
		Action: actionModelConnectionCredentialWrite, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, row := range list {
		require.Equal(t, core.OperationSuccess, row.Status)
		require.Equal(t, string(core.ResourceModelConnection), row.TargetKind)
		require.Equal(t, string(uid), row.TargetID)
	}

	require.NotContains(t, f.logs.String(), "sk-alpha-secret", "日志不得出现明文")
}

// TestCredentialOps_RevokeIsTerminal 固定 2026-10-23 判据裁决后的矩阵：撤销清空密文、
// 纪元 +1、重复撤销幂等；重新录入把身份迁回 active 并追加新版本，但**在途 Run 固定的
// 旧版本**仍失败关闭，纪元不重置。
// TestCredentialOps_RevokeIsTerminal pins terminal versions with identity re-entry.
func TestCredentialOps_RevokeIsTerminal(t *testing.T) {
	ctx := context.Background()
	f := newCredentialOpsFixture(t)
	f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	uid := connectionUID(t, "alpha")

	written := writeConnection(t, f, "alpha", "sk-alpha-secret-1")
	identity, err := f.credentials.CredentialByOwner(ctx, core.BindingModelConnection, uid)
	require.NoError(t, err)

	revoked, err := f.credOps.Revoke(ctx, CredentialRevokeRequest{
		Actor: resourceActor(), Kind: core.BindingModelConnection, Name: "alpha", UID: uid,
	})
	require.NoError(t, err)
	require.True(t, revoked.Changed)
	require.Equal(t, 1, revoked.RevocationEpoch)
	require.Equal(t, written.Revision+1, revoked.Revision)

	after, err := f.credentials.Credential(ctx, identity.ID)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityRevoked, after.State)
	versions, err := f.credentials.Versions(ctx, identity.ID)
	require.NoError(t, err)
	for _, rec := range versions {
		require.Equal(t, core.CredentialVersionRevoked, rec.State)
		require.Nil(t, rec.Ciphertext, "撤销后不得留下可解密材料")
	}

	// 重复撤销：幂等，不新开操作行、不再升纪元。
	again, err := f.credOps.Revoke(ctx, CredentialRevokeRequest{
		Actor: resourceActor(), Kind: core.BindingModelConnection, Name: "alpha", UID: uid,
	})
	require.NoError(t, err)
	require.False(t, again.Changed)
	require.Empty(t, again.OperationID)
	require.Equal(t, 1, again.RevocationEpoch)
	rows, err := f.ops.ListOperations(ctx, core.OperationFilter{
		Action: actionModelConnectionCredentialRevoke, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, rows, 1, "幂等撤销不得写第二条操作行")

	// 重新录入：身份迁回 active、追加第 2 版；纪元不重置、旧版本永不复活。
	reentry := writeConnection(t, f, "alpha", "sk-alpha-secret-2")
	require.Equal(t, 2, reentry.VersionNo)
	require.Equal(t, core.CredentialIdentityActive, reentry.State)
	identity, err = f.credentials.Credential(ctx, identity.ID)
	require.NoError(t, err)
	require.Equal(t, 1, identity.RevocationEpoch, "重新录入不重置撤销纪元")

	old, err := f.credentials.Version(ctx, identity.ID, 1)
	require.NoError(t, err)
	require.Equal(t, core.CredentialVersionRevoked, old.State)
	require.Nil(t, old.Ciphertext)

	// 在途 Run 固定的第 1 版：重新录入之后仍然失败关闭。
	guard, err := NewCredentialGuard(CredentialGuardDeps{Credentials: f.credentials})
	require.NoError(t, err)
	err = guard.Check(ctx, core.RunCredentialRef{
		RunID:             "run_01",
		CredentialID:      identity.ID,
		CredentialVersion: 1,
		ResourceKind:      core.BindingModelConnection,
		ResourceUID:       uid,
		ResourceName:      "alpha",
	})
	require.ErrorIs(t, err, core.ErrCredentialRevoked, "重新录入不得复活旧 Run 固定的版本")

	// 新 Run 用第 2 版：解析器按当前版本给出新 Key。
	current, err := f.resolver.ResolveModelCredential(ctx, core.ResourceVersionRef{
		Name: "alpha", ResourceUID: uid,
	})
	require.NoError(t, err)
	require.Equal(t, "sk-alpha-secret-2", current)

	// 再撤销：纪元继续前进。
	second, err := f.credOps.Revoke(ctx, CredentialRevokeRequest{
		Actor: resourceActor(), Kind: core.BindingModelConnection, Name: "alpha", UID: uid,
	})
	require.NoError(t, err)
	require.True(t, second.Changed)
	require.Equal(t, 2, second.RevocationEpoch)
}

// TestCredentialOps_EnvRetireKeepsCiphertext：managed → env 退役身份与活动版本，
// 密文**保留**（在途 Run 可能还持有该版本），不追加版本；重复退役是无改动结果。
// TestCredentialOps_EnvRetireKeepsCiphertext pins the ciphertext-preserving retire.
func TestCredentialOps_EnvRetireKeepsCiphertext(t *testing.T) {
	ctx := context.Background()
	f := newCredentialOpsFixture(t)
	f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	uid := connectionUID(t, "alpha")
	written := writeConnection(t, f, "alpha", "sk-alpha-secret-1")
	identity, err := f.credentials.CredentialByOwner(ctx, core.BindingModelConnection, uid)
	require.NoError(t, err)

	// 定义文件改回 env 并发布：发布事实先落库，凭据动作只退役密文身份。
	envVersion := f.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "https://api.example.com/v1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	f.publish(t, core.ResourceModelConnection, "alpha", envVersion.ID, true)
	before, err := f.versions.PointerByUID(ctx, core.ResourceModelConnection, uid)
	require.NoError(t, err)

	retired, err := f.credOps.Write(ctx, CredentialWriteRequest{
		Actor: resourceActor(),
		Kind:  core.BindingModelConnection,
		Name:  "alpha",
		UID:   uid,
		Mode:  core.CredentialModeEnv,
		Value: []byte("${BASALT_ALPHA_KEY}"),
	})
	require.NoError(t, err)
	require.True(t, retired.Changed)
	require.Equal(t, core.CredentialIdentityRetired, retired.State)
	require.Equal(t, before.Revision+1, retired.Revision)

	after, err := f.credentials.Credential(ctx, identity.ID)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityRetired, after.State)
	require.Equal(t, 1, after.CurrentVersion, "退役不追加版本")
	version, err := f.credentials.Version(ctx, identity.ID, 1)
	require.NoError(t, err)
	require.Equal(t, core.CredentialVersionRetired, version.State)
	require.NotEmpty(t, version.Ciphertext, "退役保留密文供在途使用")
	require.NotEqual(t, 0, written.VersionNo)

	// 再切一次 env：没有可退役的东西，是无改动结果，不新开操作行。
	again, err := f.credOps.Write(ctx, CredentialWriteRequest{
		Actor: resourceActor(),
		Kind:  core.BindingModelConnection,
		Name:  "alpha",
		UID:   uid,
		Mode:  core.CredentialModeEnv,
	})
	require.NoError(t, err)
	require.False(t, again.Changed)
	rows, err := f.ops.ListOperations(ctx, core.OperationFilter{
		Action: actionModelConnectionCredentialWrite, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, rows, 2, "无改动的退役不得写操作行（首次写入 + 本次退役）")
}

// TestCredentialOps_NotifyChannelWriteAndRevoke：托管通知渠道的完整地址进密文，
// 行上只留 scheme + host；撤销后渠道行仍指向该凭据（历史可查证），地址列不回填明文。
// TestCredentialOps_NotifyChannelWriteAndRevoke pins the managed channel row shape.
func TestCredentialOps_NotifyChannelWriteAndRevoke(t *testing.T) {
	ctx := context.Background()
	f := newCredentialOpsFixture(t)
	uid := fixtureResourceUID(t, core.ResourceNotifyChannel, "ops")
	require.NoError(t, f.admission.NotifyAdmin.Create(ctx, core.NotifyChannel{
		ResourceUID:    uid,
		Name:           "ops",
		Type:           "webhook",
		URL:            "${BASALT_WEBHOOK}",
		URLHost:        "https://hooks.example.com",
		CredentialMode: core.CredentialModeEnv,
		OperationID:    f.opID,
	}))

	const address = "https://hooks.example.com/t/v1/token-abc"
	written, err := f.credOps.Write(ctx, CredentialWriteRequest{
		Actor: resourceActor(),
		Kind:  core.BindingNotifyChannel,
		Name:  "ops",
		UID:   uid,
		Mode:  core.CredentialModeManaged,
		Value: []byte(address),
	})
	require.NoError(t, err)
	require.True(t, written.Changed)
	require.Equal(t, 1, written.VersionNo)
	require.Equal(t, 2, written.Revision, "渠道行首次写入后 revision = 2")

	channel, err := f.admission.NotifyAdmin.GetByUID(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, core.CredentialModeManaged, channel.CredentialMode)
	require.Equal(t, written.CredentialID, channel.CredentialID)
	require.Empty(t, channel.URL, "托管渠道行不得留地址明文")
	require.Equal(t, "https://hooks.example.com", channel.URLHost)

	// 解析器按渠道行解出完整地址（每次发送都重读）。
	resolved, err := f.resolver.ResolveNotifyCredential(ctx, channel)
	require.NoError(t, err)
	require.Equal(t, address, resolved.Target)
	require.Equal(t, written.CredentialID, resolved.CredentialID)
	require.Equal(t, written.VersionNo, resolved.CredentialVersion)

	revoked, err := f.credOps.Revoke(ctx, CredentialRevokeRequest{
		Actor: resourceActor(), Kind: core.BindingNotifyChannel, Name: "ops", UID: uid,
	})
	require.NoError(t, err)
	require.True(t, revoked.Changed)
	require.Equal(t, 3, revoked.Revision)
	channel, err = f.admission.NotifyAdmin.GetByUID(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, written.CredentialID, channel.CredentialID, "撤销不改渠道行的凭据引用")
}

// TestCredentialOps_PreWriteFailureWritesNothing（T157）：预写失败 → 拒绝执行，
// 凭据与资源指针逐项不变；撤销同样如此。
// TestCredentialOps_PreWriteFailureWritesNothing pins the fail-closed pre-write.
func TestCredentialOps_PreWriteFailureWritesNothing(t *testing.T) {
	ctx := context.Background()
	env := newResolverFixture(t)
	logs := &strings.Builder{}
	ops := newTestCredentialOps(t, env, &failingOperationStore{OperationStore: env.ops}, logs)
	env.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	uid := connectionUID(t, "alpha")

	before, err := env.versions.PointerByUID(ctx, core.ResourceModelConnection, uid)
	require.NoError(t, err)

	_, err = ops.Write(ctx, CredentialWriteRequest{
		Actor: resourceActor(),
		Kind:  core.BindingModelConnection,
		Name:  "alpha",
		UID:   uid,
		Mode:  core.CredentialModeManaged,
		Value: []byte("sk-alpha-secret-1"),
	})
	require.Error(t, err, "预写失败必须拒绝执行")

	_, err = env.credentials.CredentialByOwner(ctx, core.BindingModelConnection, uid)
	require.ErrorIs(t, err, core.ErrResourceNotFound, "预写失败不得留下凭据身份")
	after, err := env.versions.PointerByUID(ctx, core.ResourceModelConnection, uid)
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision, "预写失败不得推进 revision")

	// 撤销路径同理：身份先由正常台账写入，再用失败台账撤销。
	require.NoError(t, env.credentials.Create(ctx, core.CredentialRecord{
		ID:          credentialIDFor(uid),
		OwnerKind:   core.BindingModelConnection,
		OwnerUID:    uid,
		OwnerName:   "alpha",
		KeyID:       env.key.KeyID,
		OperationID: env.opID,
	}, core.CredentialVersionRecord{
		KeyID:         env.key.KeyID,
		TargetBinding: "sha256:" + strings.Repeat("b", 64),
		Nonce:         make([]byte, 12),
		Ciphertext:    []byte("sealed"),
		OperationID:   env.opID,
	}))
	_, err = ops.Revoke(ctx, CredentialRevokeRequest{
		Actor: resourceActor(), Kind: core.BindingModelConnection, Name: "alpha", UID: uid,
	})
	require.Error(t, err, "预写失败必须拒绝撤销")
	identity, err := env.credentials.CredentialByOwner(ctx, core.BindingModelConnection, uid)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityActive, identity.State, "预写失败不得撤销凭据")
	require.Equal(t, 0, identity.RevocationEpoch)

	require.NotContains(t, logs.String(), "sk-alpha-secret", "日志不得出现明文")
}

// TestCredentialRecovery_FourEvidenceOutcomes 用**写面自己生成的 detail** 判三档：
// 写入 applied / not_applied / indeterminate，以及退役 applied / not_applied /
// indeterminate。逆改点：恢复时重放加密或补写版本 → not_applied / indeterminate 断言红；
// 只看身份不看资源 revision → 半提交被误判成 applied 的断言红。
// TestCredentialRecovery_FourEvidenceOutcomes drives recovery from the surface's own
// detail projection, covering the write and retire branches.
func TestCredentialRecovery_FourEvidenceOutcomes(t *testing.T) {
	ctx := context.Background()

	t.Run("write_applied", func(t *testing.T) {
		env := newRecoveryEnv(t)
		ptr := seedModelConnection(t, env, "primary")
		operationID := core.NewOperationID(baselineClock())
		target := "sha256:" + strings.Repeat("b", 64)
		detail := credentialWritePlan{
			kind: core.BindingModelConnection, name: "primary", uid: ptr.ResourceUID,
			mode:             core.CredentialModeManaged,
			previousRevision: ptr.Revision, revision: ptr.Revision + 1,
			credentialID: credentialIDFor(ptr.ResourceUID), versionNo: 1, previousVersion: 0,
			target: target,
		}.writeDetail()
		beginPendingOperation(t, env.store, core.OperationRecord{
			OperationID: operationID,
			Action:      actionModelConnectionCredentialWrite,
			TargetKind:  string(core.ResourceModelConnection),
			TargetID:    string(ptr.ResourceUID),
			DetailJSON:  mustDetailJSON(t, detail),
		})
		seedCredential(t, env, "primary", ptr.ResourceUID, operationID)
		require.NoError(t, env.deps.Resources.Publish(ctx, core.ResourceModelConnection,
			"primary", ptr.PublishedVersionID, true, operationID, baselineClock()))
		require.NoError(t, recoverPendingOperations(ctx, env.deps))

		op := getOperation(t, env.store, operationID)
		require.Equal(t, core.OperationSuccess, op.Status)
		require.Equal(t, core.VerificationApplied, op.Verification)
	})

	t.Run("write_not_applied", func(t *testing.T) {
		env := newRecoveryEnv(t)
		ptr := seedModelConnection(t, env, "primary")
		operationID := core.NewOperationID(baselineClock())
		detail := credentialWritePlan{
			kind: core.BindingModelConnection, name: "primary", uid: ptr.ResourceUID,
			mode:             core.CredentialModeManaged,
			previousRevision: ptr.Revision, revision: ptr.Revision + 1,
			credentialID: credentialIDFor(ptr.ResourceUID), versionNo: 1, previousVersion: 0,
			target: "sha256:" + strings.Repeat("b", 64),
		}.writeDetail()
		beginPendingOperation(t, env.store, core.OperationRecord{
			OperationID: operationID,
			Action:      actionModelConnectionCredentialWrite,
			TargetKind:  string(core.ResourceModelConnection),
			TargetID:    string(ptr.ResourceUID),
			DetailJSON:  mustDetailJSON(t, detail),
		})
		require.NoError(t, recoverPendingOperations(ctx, env.deps))

		op := getOperation(t, env.store, operationID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)
		_, err := env.credentials.CredentialByOwner(ctx, core.BindingModelConnection, ptr.ResourceUID)
		require.ErrorIs(t, err, core.ErrResourceNotFound, "恢复不得补写凭据")
	})

	t.Run("write_indeterminate", func(t *testing.T) {
		env := newRecoveryEnv(t)
		ptr := seedModelConnection(t, env, "primary")
		otherOp := seedOperation(t, env, actionModelConnectionCredentialWrite,
			string(core.ResourceModelConnection), string(ptr.ResourceUID))
		seedCredential(t, env, "primary", ptr.ResourceUID, otherOp)
		operationID := core.NewOperationID(baselineClock())
		detail := credentialWritePlan{
			kind: core.BindingModelConnection, name: "primary", uid: ptr.ResourceUID,
			mode:             core.CredentialModeManaged,
			previousRevision: ptr.Revision, revision: ptr.Revision + 1,
			credentialID: credentialIDFor(ptr.ResourceUID), versionNo: 1, previousVersion: 0,
			target: "sha256:" + strings.Repeat("b", 64),
		}.writeDetail()
		beginPendingOperation(t, env.store, core.OperationRecord{
			OperationID: operationID,
			Action:      actionModelConnectionCredentialWrite,
			TargetKind:  string(core.ResourceModelConnection),
			TargetID:    string(ptr.ResourceUID),
			DetailJSON:  mustDetailJSON(t, detail),
		})
		require.NoError(t, recoverPendingOperations(ctx, env.deps))

		op := getOperation(t, env.store, operationID)
		require.Equal(t, core.OperationUnknown, op.Status)
		require.Equal(t, core.VerificationIndeterminate, op.Verification)
	})

	t.Run("retire_applied", func(t *testing.T) {
		env := newRecoveryEnv(t)
		ptr := seedModelConnection(t, env, "primary")
		seedOp := seedOperation(t, env, actionModelConnectionCredentialWrite,
			string(core.ResourceModelConnection), string(ptr.ResourceUID))
		identity, _, target := seedCredential(t, env, "primary", ptr.ResourceUID, seedOp)
		operationID := core.NewOperationID(baselineClock())
		detail := credentialWritePlan{
			kind: core.BindingModelConnection, name: "primary", uid: ptr.ResourceUID,
			mode:             core.CredentialModeEnv,
			previousRevision: ptr.Revision, revision: ptr.Revision + 1,
			credentialID: identity.ID, versionNo: 1, previousVersion: 1, target: target,
		}.writeDetail()
		beginPendingOperation(t, env.store, core.OperationRecord{
			OperationID: operationID,
			Action:      actionModelConnectionCredentialWrite,
			TargetKind:  string(core.ResourceModelConnection),
			TargetID:    string(ptr.ResourceUID),
			DetailJSON:  mustDetailJSON(t, detail),
		})
		require.NoError(t, env.credentials.Retire(ctx, identity.ID, operationID, baselineClock()))
		require.NoError(t, env.deps.Resources.Publish(ctx, core.ResourceModelConnection,
			"primary", ptr.PublishedVersionID, true, operationID, baselineClock()))
		require.NoError(t, recoverPendingOperations(ctx, env.deps))

		op := getOperation(t, env.store, operationID)
		require.Equal(t, core.OperationSuccess, op.Status)
		require.Equal(t, core.VerificationApplied, op.Verification)
	})

	t.Run("retire_not_applied", func(t *testing.T) {
		env := newRecoveryEnv(t)
		ptr := seedModelConnection(t, env, "primary")
		seedOp := seedOperation(t, env, actionModelConnectionCredentialWrite,
			string(core.ResourceModelConnection), string(ptr.ResourceUID))
		identity, _, target := seedCredential(t, env, "primary", ptr.ResourceUID, seedOp)
		operationID := core.NewOperationID(baselineClock())
		detail := credentialWritePlan{
			kind: core.BindingModelConnection, name: "primary", uid: ptr.ResourceUID,
			mode:             core.CredentialModeEnv,
			previousRevision: ptr.Revision, revision: ptr.Revision + 1,
			credentialID: identity.ID, versionNo: 1, previousVersion: 1, target: target,
		}.writeDetail()
		beginPendingOperation(t, env.store, core.OperationRecord{
			OperationID: operationID,
			Action:      actionModelConnectionCredentialWrite,
			TargetKind:  string(core.ResourceModelConnection),
			TargetID:    string(ptr.ResourceUID),
			DetailJSON:  mustDetailJSON(t, detail),
		})
		require.NoError(t, recoverPendingOperations(ctx, env.deps))

		op := getOperation(t, env.store, operationID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)
		after, err := env.credentials.Credential(ctx, identity.ID)
		require.NoError(t, err)
		require.Equal(t, core.CredentialIdentityActive, after.State, "恢复不得补做退役")
	})

	t.Run("retire_indeterminate", func(t *testing.T) {
		env := newRecoveryEnv(t)
		ptr := seedModelConnection(t, env, "primary")
		seedOp := seedOperation(t, env, actionModelConnectionCredentialWrite,
			string(core.ResourceModelConnection), string(ptr.ResourceUID))
		identity, _, target := seedCredential(t, env, "primary", ptr.ResourceUID, seedOp)
		operationID := core.NewOperationID(baselineClock())
		detail := credentialWritePlan{
			kind: core.BindingModelConnection, name: "primary", uid: ptr.ResourceUID,
			mode:             core.CredentialModeEnv,
			previousRevision: ptr.Revision, revision: ptr.Revision + 1,
			credentialID: identity.ID, versionNo: 1, previousVersion: 1, target: target,
		}.writeDetail()
		beginPendingOperation(t, env.store, core.OperationRecord{
			OperationID: operationID,
			Action:      actionModelConnectionCredentialWrite,
			TargetKind:  string(core.ResourceModelConnection),
			TargetID:    string(ptr.ResourceUID),
			DetailJSON:  mustDetailJSON(t, detail),
		})
		// 身份已退役、资源 revision 未推进：两类证据矛盾 → unknown，不得猜成成功。
		require.NoError(t, env.credentials.Retire(ctx, identity.ID, operationID, baselineClock()))
		require.NoError(t, recoverPendingOperations(ctx, env.deps))

		op := getOperation(t, env.store, operationID)
		require.Equal(t, core.OperationUnknown, op.Status)
		require.Equal(t, core.VerificationIndeterminate, op.Verification)
	})
}

// ============================================================
// secrets 服务面（T135）：init / status / verify / revoke-managed
// ============================================================

// newTestSecretsService 按生产依赖形状构造 secrets 服务面；主密钥路径显式传入，
// 便于造"密钥不存在 / 指向别处"的部署现场。
// newTestSecretsService builds the secrets surface with an explicit key path.
func newTestSecretsService(t *testing.T, f *credentialOpsFixture, keyPath string,
	logs *strings.Builder,
) SecretsService {
	t.Helper()
	service, err := NewSecretsService(SecretsServiceDeps{
		Credentials:   f.credentials,
		Channels:      f.admission.Channels,
		Ops:           f.credOps,
		Resolver:      f.resolver,
		MasterKeyFile: keyPath,
		WorkspaceRoot: f.root,
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	require.NoError(t, err)
	return service
}

// seedCredentialUnderOtherKeyID 写一条 key_id 与当前主密钥**不一致**的凭据：形态合法
// （身份与密文都可读），但 `secrets verify` 必须把它判为失败——模拟换过钥匙或恢复了
// 错密钥的部署现场。
// seedCredentialUnderOtherKeyID writes a well-formed credential whose key_id does not
// match the current master key; verify must classify it as failed.
func seedCredentialUnderOtherKeyID(t *testing.T, f *resolverFixture, kind core.ResourceKind,
	name, target, plaintext string,
) {
	t.Helper()
	binding, ok := credentialBindingKind(kind)
	require.True(t, ok)
	uid := fixtureResourceUID(t, kind, name)
	credID := fixtureCredentialID(t, string(kind)+"/"+name)
	aad := CredentialAAD{
		OwnerKind:     binding,
		OwnerUID:      uid,
		CredentialID:  credID,
		VersionNo:     1,
		TargetBinding: target,
	}
	nonce, ciphertext, err := SealCredential(f.key, aad, []byte(plaintext))
	require.NoError(t, err)
	require.NoError(t, f.credentials.Create(context.Background(), core.CredentialRecord{
		ID:          credID,
		OwnerKind:   binding,
		OwnerUID:    uid,
		OwnerName:   name,
		KeyID:       "k_other",
		OperationID: f.opID,
		CreatedAt:   baselineClock().UnixMilli(),
	}, core.CredentialVersionRecord{
		KeyID:         "k_other",
		TargetBinding: target,
		Last4:         last4Of(plaintext),
		Nonce:         nonce,
		Ciphertext:    ciphertext,
		OperationID:   f.opID,
	}))
}

// TestSecrets_InitRefusesWhenKeyExists：密钥文件已存在 → 拒绝，且文件逐字节不变。
// 逆改点：先 stat 再写（有窗口）→ 并发用例红；写成覆盖 → 文件哈希断言红。
// TestSecrets_InitRefusesWhenKeyExists pins the never-overwrite rule.
func TestSecrets_InitRefusesWhenKeyExists(t *testing.T) {
	ctx := context.Background()
	f := newCredentialOpsFixture(t)
	logs := &strings.Builder{}
	service := newTestSecretsService(t, f, f.keyPath, logs)

	before, err := os.ReadFile(f.keyPath)
	require.NoError(t, err)

	_, err = service.Init(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrCredentialUnavailable)
	require.Contains(t, err.Error(), "已存在", "拒绝原因必须点名：文件已存在")

	after, err := os.ReadFile(f.keyPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "拒绝覆盖时主密钥文件必须逐字节不变")
	require.NotContains(t, logs.String(), "主密钥已生成", "拒绝路径不得记录生成成功")
}

// TestSecrets_InitRefusesWithManagedDependency：当前仍有托管凭据依赖（模型连接 + 通知
// 渠道各一条）→ 拒绝生成新密钥，且不落盘任何文件。
// TestSecrets_InitRefusesWithManagedDependency pins the orphan-prevention rule.
func TestSecrets_InitRefusesWithManagedDependency(t *testing.T) {
	ctx := context.Background()
	f := newCredentialOpsFixture(t)
	f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	writeConnection(t, f, "alpha", "sk-alpha-secret-1")
	f.seedManagedChannel(t, "ops", "https://hooks.example.com/services/abc")

	keyPath := filepath.Join(t.TempDir(), "master.key")
	logs := &strings.Builder{}
	service := newTestSecretsService(t, f, keyPath, logs)

	_, err := service.Init(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "1 个托管模型连接", "拒绝原因必须点名托管依赖计数")
	require.Contains(t, err.Error(), "1 个托管通知渠道", "托管渠道同样是拒绝条件")
	require.NoFileExists(t, keyPath, "拒绝时不得生成新密钥文件")
	require.NotContains(t, logs.String(), "sk-alpha-secret", "日志不得出现明文")
}

// TestSecrets_VerifyReportsFingerprint：status 回主密钥指纹（与密钥材料不可互推、
// 多次读取稳定），verify 只回检查与失败计数——key_id 不匹配的那条必须计入失败。
// TestSecrets_VerifyReportsFingerprint pins the public fingerprint and count-only verify.
func TestSecrets_VerifyReportsFingerprint(t *testing.T) {
	ctx := context.Background()
	f := newCredentialOpsFixture(t)
	logs := &strings.Builder{}
	service := newTestSecretsService(t, f, f.keyPath, logs)

	wantFingerprint, err := MasterKeyFingerprint(f.key)
	require.NoError(t, err)

	status, err := service.Status(ctx)
	require.NoError(t, err)
	require.True(t, status.Initialized)
	require.Equal(t, f.key.KeyID, status.KeyID)
	require.Equal(t, wantFingerprint, status.Fingerprint, "status 必须给出可核对的主密钥指纹")
	require.Zero(t, status.ManagedModelConnections)
	require.Zero(t, status.ManagedNotifyChannels)

	empty, err := service.Verify(ctx)
	require.NoError(t, err)
	require.Equal(t, SecretsVerifyReport{}, empty, "没有托管凭据时计数为零")

	f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	writeConnection(t, f, "alpha", "sk-alpha-secret-1")
	f.seedManagedModelConnection(t, "beta", "https://api.example.com/v1")
	betaTarget, err := CredentialTargetBinding(core.BindingModelConnection,
		"https://api.example.com/v1", "")
	require.NoError(t, err)
	seedCredentialUnderOtherKeyID(t, f.resolverFixture, core.ResourceModelConnection,
		"beta", betaTarget, "sk-beta-secret-1")

	report, err := service.Verify(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, report.Checked)
	require.Equal(t, 1, report.Failed, "key_id 与当前主密钥不一致的凭据必须计入失败")

	again, err := service.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, wantFingerprint, again.Fingerprint, "指纹在多次读取间必须稳定")
	require.NotEqual(t, base64.StdEncoding.EncodeToString(f.key.Key), again.Fingerprint,
		"指纹不得是密钥材料本身")
	require.NotContains(t, logs.String(), base64.StdEncoding.EncodeToString(f.key.Key),
		"日志不得出现密钥材料")
	require.NotContains(t, logs.String(), "sk-alpha-secret", "日志不得出现明文")
	require.NotContains(t, logs.String(), "sk-beta-secret", "日志不得出现明文")
}

// TestSecrets_RevokeManaged：撤销全部托管凭据（模型连接 + 通知渠道），不需要解密；
// 密文清空、状态与纪元推进、每条写撤销操作行；重复执行幂等（0 条）。
// TestSecrets_RevokeManaged pins the destructive recovery path.
func TestSecrets_RevokeManaged(t *testing.T) {
	ctx := context.Background()
	f := newCredentialOpsFixture(t)
	f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	written := writeConnection(t, f, "alpha", "sk-alpha-secret-1")
	channel := f.seedManagedChannel(t, "ops", "https://hooks.example.com/services/abc")
	logs := &strings.Builder{}
	service := newTestSecretsService(t, f, f.keyPath, logs)

	revoked, err := service.RevokeManaged(ctx, resourceActor())
	require.NoError(t, err)
	require.Equal(t, 2, revoked, "模型连接与通知渠道各撤销一条")

	modelIdentity, err := f.credentials.Credential(ctx, written.CredentialID)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityRevoked, modelIdentity.State)
	require.Equal(t, 1, modelIdentity.RevocationEpoch, "撤销推进纪元")
	modelVersion, err := f.credentials.Version(ctx, written.CredentialID, 1)
	require.NoError(t, err)
	require.Equal(t, core.CredentialVersionRevoked, modelVersion.State)
	require.Empty(t, modelVersion.Ciphertext, "撤销必须清空密文")

	channelIdentity, err := f.credentials.Credential(ctx, channel.CredentialID)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityRevoked, channelIdentity.State)
	require.Equal(t, 1, channelIdentity.RevocationEpoch)

	// 操作台账：两条撤销各留一行，动作字面量按资源类别区分，状态为成功。
	for _, tc := range []struct {
		action string
		uid    core.ResourceUID
	}{
		{actionModelConnectionCredentialRevoke, connectionUID(t, "alpha")},
		{actionNotifyChannelCredentialRevoke, channel.ResourceUID},
	} {
		rows, err := f.ops.ListOperations(ctx, core.OperationFilter{Action: tc.action, Limit: 10})
		require.NoError(t, err)
		require.Len(t, rows, 1, "动作 %s 必须恰有一行", tc.action)
		require.Equal(t, core.OperationSuccess, rows[0].Status)
		require.Equal(t, string(tc.uid), rows[0].TargetID)
	}

	// 幂等：没有 active 凭据时再跑一次是零条，不报错。
	again, err := service.RevokeManaged(ctx, resourceActor())
	require.NoError(t, err)
	require.Zero(t, again, "重复撤销必须幂等（0 条）")

	require.NotContains(t, logs.String(), "sk-alpha-secret", "日志不得出现明文")
	require.NotContains(t, logs.String(), "hooks.example.com/services", "日志不得出现渠道地址")
}
