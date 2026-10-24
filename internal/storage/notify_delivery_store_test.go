package storage

// 本文件是通知冻结 / 投递记录仓储的判卷（颗粒度文档 R4.8 / T116，check
// `012-storage-admin`）：一次工具调用至多一条投递记录，渠道集合准入冻结、终态释放。
// These tests pin the notify side: one delivery row per tool invocation, channel sets
// frozen at admission and released with the terminal state.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestNotifyDeliveryStore_OnePerInvocation：投递记录的主键是工具调用 id——
// 第二次记录同一调用是冲突而不是静默覆盖；首条记录保留，审计读到的永远是第一次。
// 中文验收点：一次调用一条账，重复记录不覆盖。
func TestNotifyDeliveryStore_OnePerInvocation(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	store := NewNotifyDeliveryStore(db)

	seedSession(t, db.write, recoverySession)
	seedRun(t, db.write, "run-notify-1", "running")
	invocation := seedToolInvocation(t, db.write, "run-notify-1", "call-1", "succeeded", "ok")

	record := core.NotifyDeliveryRecord{
		ToolInvocationID:  invocation,
		RunID:             "run-notify-1",
		AgentUID:          testAgentUID,
		ChannelUID:        testResourceUID('A'),
		ChannelName:       "ops",
		CredentialID:      testCredentialID('A'),
		CredentialVersion: 2,
		TargetHost:        "open.feishu.cn",
		ResolvedAt:        opNow.UnixMilli(),
	}
	require.NoError(t, store.Record(ctx, record))

	stored, ok, err := store.Delivery(ctx, invocation)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, record.RunID, stored.RunID)
	require.Equal(t, record.AgentUID, stored.AgentUID)
	require.Equal(t, record.ChannelUID, stored.ChannelUID)
	require.Equal(t, record.ChannelName, stored.ChannelName)
	require.Equal(t, record.CredentialID, stored.CredentialID)
	require.Equal(t, record.CredentialVersion, stored.CredentialVersion)
	require.Equal(t, record.TargetHost, stored.TargetHost)
	require.Equal(t, record.ResolvedAt, stored.ResolvedAt)

	// 重复投递：冲突，且首条记录不被改写。
	duplicate := record
	duplicate.ChannelName = "other"
	duplicate.CredentialVersion = 3
	require.ErrorIs(t, store.Record(ctx, duplicate), core.ErrConflict)

	stored, ok, err = store.Delivery(ctx, invocation)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ops", stored.ChannelName)
	require.Equal(t, 2, stored.CredentialVersion)

	deliveries, err := store.Deliveries(ctx, "run-notify-1")
	require.NoError(t, err)
	require.Len(t, deliveries, 1)

	// 没记录过的调用：ok=false（"还没发"与"发过没记"要能分辨）。
	_, ok, err = store.Delivery(ctx, invocation+1)
	require.NoError(t, err)
	require.False(t, ok)

	// env 模式（无凭据身份）：凭据列留空，不得带版本；带版本即拒绝。
	envInvocation := seedToolInvocation(t, db.write, "run-notify-1", "call-2", "succeeded", "ok")
	envRecord := core.NotifyDeliveryRecord{
		ToolInvocationID: envInvocation,
		RunID:            "run-notify-1",
		AgentUID:         testAgentUID,
		ChannelUID:       testResourceUID('B'),
		ChannelName:      "mail",
		TargetHost:       "hooks.example.com",
	}
	require.NoError(t, store.Record(ctx, envRecord))
	stored, ok, err = store.Delivery(ctx, envInvocation)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, stored.CredentialID)
	require.Zero(t, stored.CredentialVersion)

	envRecord.ToolInvocationID = seedToolInvocation(t, db.write, "run-notify-1", "call-3", "succeeded", "ok")
	envRecord.CredentialVersion = 1
	require.Error(t, store.Record(ctx, envRecord), "env 模式不得带凭据版本")
}

// TestNotifyDeliveryStore_AdmissionFreezesChannels：准入把清单里的渠道集合冻进
// `run_notify_refs`，终态释放；通知**不**固定凭据版本（run_credential_refs 只记模型连接）。
// 中文验收点：渠道集合准入冻结、随终态释放、不产生凭据固定行。
func TestNotifyDeliveryStore_AdmissionFreezesChannels(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	runs := NewRunStore(db)
	store := NewNotifyDeliveryStore(db)
	pins := NewRunCredentialStore(db)

	fx := seedRunManifest(t, db)
	resolved := fx.resolved
	resolved.Notify = core.NotifyManifest{
		Mode: core.NotifyModeAllEligible,
		Channels: []core.NotifyChannelRef{
			{Name: "ops", ResourceUID: testResourceUID('A')},
			{Name: "mail", ResourceUID: testResourceUID('B')},
		},
	}
	manifest, err := core.BuildRunResourceManifest(testAgentUID, core.AgentActive, resolved)
	require.NoError(t, err)

	rec := startRow("scheduler")
	rec.RunID = "scheduler-1776000000123-notify1"
	require.NoError(t, runs.StartRun(ctx, rec, manifest))

	refs, err := store.Refs(ctx, rec.RunID)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	require.Equal(t, "mail", refs[0].ChannelName, "按名称升序")
	require.Equal(t, testResourceUID('B'), refs[0].ChannelUID)
	require.Equal(t, testAgentUID, refs[0].AgentUID)
	require.Nil(t, refs[0].ReleasedAt)

	pinned, err := pins.Pins(ctx, rec.RunID)
	require.NoError(t, err)
	require.Empty(t, pinned, "通知不固定凭据版本")

	require.NoError(t, runs.FinishRun(ctx, rec.RunID, core.StatusCompleted, "", 1, 1, fixedNow))
	refs, err = store.Refs(ctx, rec.RunID)
	require.NoError(t, err)
	require.Len(t, refs, 2, "释放不删行")
	for _, ref := range refs {
		require.NotNil(t, ref.ReleasedAt)
	}

	// 独立释放：只动未释放的行，重复释放是 no-op。
	released, err := store.ReleaseRefs(ctx, rec.RunID, fixedNow)
	require.NoError(t, err)
	require.Zero(t, released)
}
