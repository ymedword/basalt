package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// insertNotifyChannel 直接向 notify_channels 落一行夹具：本 US 对注册表**只读**，
// 生产侧零写入——夹具是唯一写入口，时刻显式传入（仓储不自取业务时间）。
// insertNotifyChannel writes one fixture row; this US has no production write path,
// the fixture is the only writer and the clock is explicit.
func insertNotifyChannel(t *testing.T, db *DB, name, typ, url string) {
	t.Helper()
	createdAt := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	_, err := db.write.ExecContext(context.Background(),
		`INSERT INTO notify_channels (name, type, url, description, created_at)
		 VALUES (?, ?, ?, NULL, ?)`, name, typ, url, createdAt.UnixMilli())
	require.NoError(t, err)
}

// TestNotifyChannelRepo_ListsRowsOrderedByName —— S1（约束 4）：读取按 `name` 升序，
// 三列原样返回；插入顺序与之无关，排序由 SQL 固定（可用渠道名文案与测试可复现）。
// S1: rows come back ordered by name regardless of insertion order, with the three
// consumed columns returned verbatim.
func TestNotifyChannelRepo_ListsRowsOrderedByName(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	insertNotifyChannel(t, db, "weekly", "webhook", "${WEEKLY_HOOK}")
	insertNotifyChannel(t, db, "alerts", "feishu", "${ALERTS_HOOK}")
	insertNotifyChannel(t, db, "ops", "feishu", "${OPS_HOOK}")

	repo := NewNotifyChannelRepo(db)
	got, err := repo.ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []core.NotifyChannel{
		{Name: "alerts", Type: "feishu", URL: "${ALERTS_HOOK}"},
		{Name: "ops", Type: "feishu", URL: "${OPS_HOOK}"},
		{Name: "weekly", Type: "webhook", URL: "${WEEKLY_HOOK}"},
	}, got)
}

// TestNotifyChannelRepo_EmptyRegistryReturnsEmptySlice —— S2（约束 2 / 4）：空表返回
// 空切片且不是错误——「没有渠道可发」与「读取失败」是两条不同的诊断路径。
// S2: an empty registry yields an empty slice, never an error.
func TestNotifyChannelRepo_EmptyRegistryReturnsEmptySlice(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)

	repo := NewNotifyChannelRepo(db)
	got, err := repo.ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.NotNil(t, got, "空表返回空切片（不是 nil）")
	require.Empty(t, got)
}

// TestNotifyChannelRepo_ContextErrorPropagates —— S3：ctx 已取消时读取直接失败，
// 错误链保留 `context.Canceled`（不吞、不换文案——分类由调用方按身份判定）。
// S3: a canceled context propagates with its identity intact.
func TestNotifyChannelRepo_ContextErrorPropagates(t *testing.T) {
	db := openTempDB(t)
	repo := NewNotifyChannelRepo(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.ListNotifyChannels(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// TestNotifyChannelRepo_SeesRowsInsertedAfterConstruction —— S4（约束 4 / 注 4）：
// 仓储构造后新增的行下一发即可见——零缓存，US-12 / 手工 SQL 的改动不要求重建仓储。
// S4: rows inserted after construction are visible on the next read — zero caching.
func TestNotifyChannelRepo_SeesRowsInsertedAfterConstruction(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewNotifyChannelRepo(db)

	before, err := repo.ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, before)

	insertNotifyChannel(t, db, "ops", "feishu", "${OPS_HOOK}")
	after, err := repo.ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []core.NotifyChannel{{Name: "ops", Type: "feishu", URL: "${OPS_HOOK}"}}, after)
}
