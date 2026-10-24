package storage

// 本文件是 Run 凭据固定仓储的判卷（颗粒度文档 R4.8 / T116，check `012-storage-admin`）：
// 准入冻结、终态释放、行保留为历史。冻结与释放都要在**准入 / 终态事务**里发生，
// 所以用例同时验证 StartRun 与 FinishRun 的接线段——只测独立方法会漏掉接线。
// These tests pin the run credential repository, including the StartRun/FinishRun wiring
// that freezes and releases the pin inside the admission and terminal transactions.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// managedManifest 把清单夹具的 Provider 凭据换成托管模式并重算哈希：
// 只有托管模式才会写 `run_credential_refs`（env 模式没有凭据身份）。
// managedManifest swaps the provider pin to managed mode and rehashes.
func managedManifest(t *testing.T, fx runManifestFixture, credentialID string) core.RunResourceManifest {
	t.Helper()
	managed := fx.resolved
	managed.ProviderCredential = core.CredentialPin{
		Mode:          core.CredentialModeManaged,
		ID:            credentialID,
		Version:       1,
		TargetBinding: testTargetBinding,
	}
	manifest, err := core.BuildRunResourceManifest(testAgentUID, core.AgentActive, managed)
	require.NoError(t, err)
	return manifest
}

// TestRunCredentialStore_FreezeAndRelease：准入把托管凭据版本冻进 `run_credential_refs`，
// 终态在同一事务释放；行保留（`released_at` 非空）供"当时用哪版 Key"事后追溯。
// 中文验收点：冻结写准入事务、释放写终态事务、行不删除。
func TestRunCredentialStore_FreezeAndRelease(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	runs := NewRunStore(db)
	pins := NewRunCredentialStore(db)

	credentialID := testCredentialID('A')

	// 独立冻结：先有一条 Run 行（零清单只写运行行，不伪造引用）。
	standalone := startRow("cli")
	standalone.RunID = "cli-1776000000123-pin001"
	require.NoError(t, runs.StartRun(ctx, standalone, core.RunResourceManifest{}))

	ref := core.RunCredentialRef{
		RunID:             standalone.RunID,
		CredentialID:      credentialID,
		CredentialVersion: 1,
		ResourceKind:      core.BindingModelConnection,
		ResourceUID:       testResourceUID('A'),
		ResourceName:      "openai",
	}
	require.NoError(t, pins.Freeze(ctx, ref))

	stored, err := pins.Pins(ctx, standalone.RunID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, credentialID, stored[0].CredentialID)
	require.Equal(t, 1, stored[0].CredentialVersion)
	require.Nil(t, stored[0].ReleasedAt, "准入时未释放")

	// 重复冻结同一 `(run, credential, kind, uid)` → 主键拒绝（同一 Run 只固定一次）。
	require.Error(t, pins.Freeze(ctx, ref))

	released, err := pins.Release(ctx, standalone.RunID, fixedNow)
	require.NoError(t, err)
	require.Equal(t, 1, released)

	stored, err = pins.Pins(ctx, standalone.RunID)
	require.NoError(t, err)
	require.Len(t, stored, 1, "释放不删行")
	require.NotNil(t, stored[0].ReleasedAt)
	require.Equal(t, fixedNow.UnixMilli(), *stored[0].ReleasedAt)

	again, err := pins.Release(ctx, standalone.RunID, fixedNow.Add(time.Hour))
	require.NoError(t, err)
	require.Zero(t, again, "重复释放是 no-op，第一次的释放时间不被覆盖")

	// 非法输入在写库前拒绝：只固定模型连接、版本从 1 起、身份合法。
	bad := ref
	bad.RunID = "cli-1776000000123-pin002"
	bad.ResourceKind = core.BindingNotifyChannel
	require.Error(t, pins.Freeze(ctx, bad))
	bad = ref
	bad.CredentialVersion = 0
	require.Error(t, pins.Freeze(ctx, bad))
	bad = ref
	bad.CredentialID = "cred_bogus"
	require.Error(t, pins.Freeze(ctx, bad))
}

// TestRunCredentialStore_AdmissionFreezesAndFinishReleases：接线判卷——托管模式的
// 准入清单经 StartRun 自动冻结，FinishRun 在同一事务释放；env 模式不写行。
// 中文验收点：凭据固定在准入时生效，不靠调用方补写。
func TestRunCredentialStore_AdmissionFreezesAndFinishReleases(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	runs := NewRunStore(db)
	pins := NewRunCredentialStore(db)

	fx := seedRunManifest(t, db)
	managed := managedManifest(t, fx, testCredentialID('A'))

	rec := startRow("cli")
	rec.RunID = "cli-1776000000123-pin003"
	require.NoError(t, runs.StartRun(ctx, rec, managed))

	stored, err := pins.Pins(ctx, rec.RunID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, managed.Provider.CredentialID, stored[0].CredentialID)
	require.Equal(t, managed.Provider.CredentialVersion, stored[0].CredentialVersion)
	require.Equal(t, managed.Provider.ResourceUID, stored[0].ResourceUID)
	require.Equal(t, managed.Provider.Name, stored[0].ResourceName)

	require.NoError(t, runs.FinishRun(ctx, rec.RunID, core.StatusCompleted, "", 1, 0, fixedNow))
	stored, err = pins.Pins(ctx, rec.RunID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.NotNil(t, stored[0].ReleasedAt, "终态与释放同事务")

	// env 模式的清单不写固定行（没有凭据身份可固定）。
	envRec := startRow("web")
	envRec.RunID = "web-1776000000123-pin004"
	require.NoError(t, runs.StartRun(ctx, envRec, fx.manifest))
	stored, err = pins.Pins(ctx, envRec.RunID)
	require.NoError(t, err)
	require.Empty(t, stored)
}
