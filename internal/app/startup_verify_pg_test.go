//go:build integration

package app

// 本文件守 S-F01（2026-10-31 裁决第 1 条）的**真实库**那一半：验证窗口的探针必须
// 从 `storage_cutover` 读出真实的 `first_write_at` 与逐表行数，而不是永远返回一个
// "空标记 = 干净"的假读数。
//
// 判据四条：没有切换行时如实报 `ErrNoCutoverMarker`（不降级成"窗口开着"）；空标记
// 原样读出；`first_write_at` 落库后读得到；行数快照覆盖真实用户表并在新增行时给出
// 正的差值。
// The real-database half of the window's database dimension: the marker and the row
// counts must come from PostgreSQL itself.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/dbmigrate"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// TestStartupVerification_ReadsRealCutoverMarker 判窗口探针的真实读数与行数快照。
//
// 逆改点：探针改写 `Record.FirstWriteAt` 不读（永远返回 nil）→ 第三条断言红；
// 行数快照按空 map 返回 → 覆盖与差值两条断言红；没有切换行时返回空状态而非
// `ErrNoCutoverMarker` → 第一条断言红。
// Mutation: a probe that ignores first_write_at, an empty row-count snapshot, or a
// missing-marker path that reports "clean" instead of ErrNoCutoverMarker.
func TestStartupVerification_ReadsRealCutoverMarker(t *testing.T) {
	db := pgtest.NewTestDB(t)
	ctx := context.Background()

	// 一、没有切换行的库不是本窗口的对象：必须带哨兵报错，不能读成"窗口开着"。
	_, err := readWindowDatabaseState(ctx, db)
	require.ErrorIs(t, err, dbmigrate.ErrNoCutoverMarker)

	// 二、写一条切换行（夹具直接落行：`WriteCutover` 需要完整清单目录，本用例判的是
	// 读侧；写侧的判据由 dbmigrate 的切换用例守）。
	cutoverAt := int64(1_700_000_000_000)
	_, err = db.ExecContext(ctx, `INSERT INTO storage_cutover
		(cutover_id, source_schema_version, manifest_sha256, cutover_at, operator)
		VALUES ($1, 6, $2, $3, 'fixture')`,
		"cut_1700000000000", strings.Repeat("a", 64), cutoverAt)
	require.NoError(t, err)

	state, err := readWindowDatabaseState(ctx, db)
	require.NoError(t, err)
	require.Equal(t, postgres.SupportedSchemaVersion, state.SchemaVersion)
	require.Equal(t, "cut_1700000000000", state.CutoverID)
	require.Equal(t, cutoverAt, state.CutoverAt)
	require.Nil(t, state.FirstWriteAt, "刚写入的切换行 first_write_at 必须仍为 NULL")

	// 行数快照覆盖真实用户表：业务表在，切换表自己的读数与库里一致。
	require.Contains(t, state.RowCounts, "runs")
	require.Contains(t, state.RowCounts, "messages")
	require.Equal(t, int64(1), state.RowCounts["storage_cutover"])

	// 三、首个业务写入落库后（这里直接更新同一列），探针必须读得到。
	written := int64(1_700_000_100_000)
	_, err = db.ExecContext(ctx, `UPDATE storage_cutover SET first_write_at = $1
		WHERE cutover_id = 'cut_1700000000000'`, written)
	require.NoError(t, err)

	state, err = readWindowDatabaseState(ctx, db)
	require.NoError(t, err)
	require.NotNil(t, state.FirstWriteAt)
	require.Equal(t, written, *state.FirstWriteAt)

	// 四、窗口起止对比：再插一条切换行，行数差必须为正且指向 storage_cutover。
	_, err = db.ExecContext(ctx, `INSERT INTO storage_cutover
		(cutover_id, source_schema_version, manifest_sha256, cutover_at, operator)
		VALUES ($1, 6, $2, $3, 'fixture')`,
		"cut_1700000200000", strings.Repeat("b", 64), cutoverAt+200_000)
	require.NoError(t, err)

	after, err := readWindowDatabaseState(ctx, db)
	require.NoError(t, err)
	deltas := dbmigrate.CompareRowCounts(state.RowCounts, after.RowCounts)
	require.Len(t, deltas, 1)
	require.Equal(t, "storage_cutover", deltas[0].Table)
	require.Equal(t, int64(1), deltas[0].Delta)
	require.Equal(t, "cut_1700000200000", after.CutoverID, "当前切换行随之后移")
}
