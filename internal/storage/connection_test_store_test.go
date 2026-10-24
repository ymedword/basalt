package storage

// 本文件是 `resource_connection_tests` 投影表的判卷（US-12 R7 范围修订，check
// `012-storage-admin`）：每资源只保留最后一次结果、upsert 覆盖旧值、非法形状在 SQL
// 之前拒绝。历史明细不归本表——那是 `management_operations`。
// These tests pin the last-test projection: one row per resource, upsert overwrites,
// invalid shapes rejected before touching SQL.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestConnectionTestStore_UpsertKeepsLastOnly：同一资源二次测试覆盖首条（本表按定义
// 只回答「上回测得怎样」），且不新增第二行；不同 UID 互不干扰并按 UID 升序返回。
// 中文验收点：只留最后一条，不追加历史。
func TestConnectionTestStore_UpsertKeepsLastOnly(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	store := NewConnectionTestStore(db)

	first := core.ResourceConnectionTest{
		Kind:        core.ResourceNotifyChannel,
		ResourceUID: testResourceUID('A'),
		TestedAt:    opNow,
		Status:      core.ConnectionTestSuccess,
		Detail:      "通知渠道测试消息已发送",
		OperationID: "op-1759999999999-aaaaaa",
	}
	require.NoError(t, store.RecordConnectionTest(ctx, first))

	second := core.ResourceConnectionTest{
		Kind:        core.ResourceNotifyChannel,
		ResourceUID: testResourceUID('A'),
		TestedAt:    opNow.Add(2 * time.Minute),
		Status:      core.ConnectionTestFailed,
		Detail:      "对端返回 400",
		OperationID: "op-1760000000119-bbbbbb",
	}
	require.NoError(t, store.RecordConnectionTest(ctx, second))

	other := core.ResourceConnectionTest{
		Kind:        core.ResourceNotifyChannel,
		ResourceUID: testResourceUID('B'),
		TestedAt:    opNow,
		Status:      core.ConnectionTestUnknown,
		Detail:      "请求已发出但结果不明",
		OperationID: "op-1759999999999-cccccc",
	}
	require.NoError(t, store.RecordConnectionTest(ctx, other))

	rows, err := store.ListConnectionTests(ctx, core.ResourceNotifyChannel)
	require.NoError(t, err)
	require.Len(t, rows, 2, "同资源重测必须覆盖，不得追加历史行")
	require.Equal(t, testResourceUID('A'), rows[0].ResourceUID)
	require.Equal(t, core.ConnectionTestFailed, rows[0].Status)
	require.Equal(t, second.Detail, rows[0].Detail)
	require.Equal(t, second.OperationID, rows[0].OperationID)
	require.Equal(t, second.TestedAt, rows[0].TestedAt, "覆盖必须刷新时间戳")
	require.Equal(t, testResourceUID('B'), rows[1].ResourceUID)
	require.Equal(t, core.ConnectionTestUnknown, rows[1].Status)
}

// TestConnectionTestStore_KindIsolation：模型连接与通知渠道的 UID 即使字面相同也各自
// 成行——读取按种类过滤，不得把另一类的最后结果串进来。
// 中文验收点：两类资源不串味。
func TestConnectionTestStore_KindIsolation(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	store := NewConnectionTestStore(db)

	uid := testResourceUID('C')
	require.NoError(t, store.RecordConnectionTest(ctx, core.ResourceConnectionTest{
		Kind:        core.ResourceModelConnection,
		ResourceUID: uid,
		TestedAt:    opNow,
		Status:      core.ConnectionTestSuccess,
		Detail:      "模型调用成功（12 ms）",
		OperationID: "op-1759999999999-dddddd",
	}))

	models, err := store.ListConnectionTests(ctx, core.ResourceModelConnection)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, core.ResourceModelConnection, models[0].Kind)

	channels, err := store.ListConnectionTests(ctx, core.ResourceNotifyChannel)
	require.NoError(t, err)
	require.Empty(t, channels, "另一类资源的读取不得串行")
}

// TestConnectionTestStore_RejectsInvalidShapes：非法种类、空 UID、非法状态在 SQL 之前
// 拒绝；两者都不落行。
// 中文验收点：脏数据不进表。
func TestConnectionTestStore_RejectsInvalidShapes(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()
	store := NewConnectionTestStore(db)

	cases := []struct {
		name string
		rec  core.ResourceConnectionTest
	}{
		{"非法种类", core.ResourceConnectionTest{
			Kind: core.ResourceSkill, ResourceUID: testResourceUID('D'), TestedAt: opNow,
			Status: core.ConnectionTestSuccess,
		}},
		{"空 UID", core.ResourceConnectionTest{
			Kind: core.ResourceNotifyChannel, TestedAt: opNow, Status: core.ConnectionTestSuccess,
		}},
		{"非法状态", core.ResourceConnectionTest{
			Kind: core.ResourceNotifyChannel, ResourceUID: testResourceUID('D'), TestedAt: opNow,
			Status: "ok",
		}},
	}
	for _, tc := range cases {
		require.Error(t, store.RecordConnectionTest(ctx, tc.rec), tc.name)
	}

	rows, err := store.ListConnectionTests(ctx, core.ResourceNotifyChannel)
	require.NoError(t, err)
	require.Empty(t, rows, "被拒绝的记录不得落行")

	_, err = store.ListConnectionTests(ctx, core.ResourceSkill)
	require.Error(t, err, "技能不是连接测试的资源种类")
}
