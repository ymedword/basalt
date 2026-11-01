//go:build integration

package postgres

// 本文件是 **C-14 的 PG 侧判据**（US-16 §4 的 `TestPostgres_TimeColumnsAreUnixMillisUTC`）：
// 契约 D1 规定两个谱系的时间列一律 **Unix 毫秒 UTC 整数**（PG 存 `bigint`），**不得**改用
// `timestamptz` / `now()`。用例分三段判同一个决定：
//
//  1. **类型面**：扫当前 schema 的全部列，任何一列的 `data_type` 落在日期 / 时间类型里
//     即失败，失败信息逐条点名 `表.列 类型`（契约 D1 原文：扫描全库时间列的列类型，
//     出现 `timestamptz` 即失败）；
//  2. **正向锚**：名字以 `_at` 结尾的时间列**一律** `bigint`，且契约逐字点名的
//     `storage_cutover.first_write_at` 必须在内——没有这一段，空 schema 或把时间列
//     改窄成 `integer` 都能让第 1 段假绿；
//  3. **值面**：用 **UTC+8** 时区的时钟走一次真实写入（`MemoryStore.Append`），库里的
//     `created_at` / `valid_from` 必须恰等于同一瞬时在 UTC 下的 Unix 毫秒——"毫秒而非
//     秒"与"时区不参与换算"两条同时押在这一条断言上。
//
// 连接参数那半边（`TimeZone=UTC`）由 `TestStartupVerification_OpenPinsUTCAndPrimarySession`
// 判卷；两半合起来才是模块卡 §4 的「`TestPostgres_TimeColumnsAreUnixMillisUTC` +
// 连接参数断言」。
// This file is the PG half of C-14: a whole-schema scan that forbids temporal column
// types, the positive `bigint` anchor named by the contract, and one real write whose
// stored millis must equal the same instant expressed in UTC while the clock runs in
// UTC+8. The connection-side half lives in the startup-verification case.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// temporalDataTypes 是 `information_schema.columns.data_type` 里日期 / 时间类型的全部
// 归一化字面量。契约 D1 要求全库一列都不许出现。
// temporalDataTypes lists every temporal literal PostgreSQL normalises date/time columns to.
var temporalDataTypes = map[string]bool{
	"timestamp with time zone":    true, // timestamptz
	"timestamp without time zone": true, // timestamp
	"time with time zone":         true, // timetz
	"time without time zone":      true, // time
	"date":                        true,
	"interval":                    true,
}

// TestPostgres_TimeColumnsAreUnixMillisUTC：全库不出现日期 / 时间类型列，`_at` 时间列
// 一律 `bigint`，且一次非 UTC 时钟的真实写入落库为同一瞬时的 Unix 毫秒。
// 逆改点：任一列改用 `timestamptz` / `date` → 第 1 段红；把 `_at` 列收窄成 `integer`
// → 第 2 段红；写入侧改存秒 / 纳秒或按本地时区重算 → 第 3 段红。
// 验收点：C-14 / FR-026 / FR-076 / 契约 D1。
func TestPostgres_TimeColumnsAreUnixMillisUTC(t *testing.T) {
	t.Parallel()

	db := knTestDB(t)
	ctx := context.Background()

	// ① + ② 一遍扫完：类型面收集违例，正向锚同时确认 `_at` 列确实存在且是 bigint。
	rows, err := db.pool.QueryContext(ctx, `
		SELECT table_name, column_name, data_type
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		ORDER BY table_name, column_name`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	var (
		scanned  int
		temporal []string
		nonMilli []string
		anchored bool
	)
	for rows.Next() {
		var table, column, dataType string
		require.NoError(t, rows.Scan(&table, &column, &dataType))
		scanned++
		if temporalDataTypes[dataType] {
			temporal = append(temporal, table+"."+column+" "+dataType)
			continue
		}
		if !strings.HasSuffix(column, "_at") {
			continue
		}
		if dataType != "bigint" {
			nonMilli = append(nonMilli, table+"."+column+" "+dataType)
			continue
		}
		if table == "storage_cutover" && column == "first_write_at" {
			anchored = true
		}
	}
	require.NoError(t, rows.Err())

	// 扫描面本身要可证：一列都没看到不是"合规"，是扫错了 schema。
	require.Positive(t, scanned, "扫描必须真的看到列（当前 schema 空 = 用例失去判别力）")
	require.Empty(t, temporal,
		"时间列必须是 bigint Unix 毫秒（契约 D1）：出现 timestamptz / date 即失败，漂移列见上")
	require.Empty(t, nonMilli,
		"以 `_at` 结尾的时间列必须是 bigint（Unix 毫秒），不得收窄成 integer 等其它整数类型")
	require.True(t, anchored,
		"契约 D1 逐字点名的 storage_cutover.first_write_at 必须作为 bigint 时间列存在")

	// ③ 值面：时钟给的是 UTC+8 的 17:00，与夹具的 09:00 UTC 是同一瞬时；落库值必须
	// 恰为该瞬时的 Unix 毫秒——秒 / 纳秒 / 本地时区重算三种漂移都会让等式不成立。
	zoned := time.Date(2026, 10, 28, 17, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	require.True(t, zoned.Equal(knNow), "夹具前提：UTC+8 的 17:00 与 knNow 是同一瞬时")

	store := NewMemoryStore(db, func() time.Time { return zoned })
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "时间列判据"))

	var createdAt, validFrom int64
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		SELECT created_at, valid_from FROM memory_entries`).Scan(&createdAt, &validFrom))
	require.Equal(t, knNow.UnixMilli(), createdAt,
		"写入时刻必须落成 Unix 毫秒：本地时区参与换算或改用秒 / 纳秒都到此为止")
	require.Equal(t, knNow.UnixMilli(), validFrom, "valid_from 与 created_at 同一口径")
}
