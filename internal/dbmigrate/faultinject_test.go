//go:build integration

package dbmigrate

// 本文件是 §2.11.3 失败矩阵的**故障注入**用例（T081）：三个场景各取一个既有用例没覆盖的
// 侧面，故意不重复已有的"回滚为空表""二次导入被拒""源端漂移进报告"：
//
//	① 中途失败（外键拒绝）整笔回滚之后，**换成修好的 dump 重试必须成功**——既有
//	   `TestMigration_FailedImportLeavesEmptyTables` 只证明"回滚为空表"，不证明空表
//	   真的可重试（序列、advisory 锁、schema 状态都可能留下残骸）；
//	② 源库文件**同尺寸单字节**改动也必须判成 `DBFileDrift`——证明判据是 SHA-256 而
//	   不是文件尺寸；
//	③ **部分迁移副本**（崩溃残骸：目标库只有一行）重试必须 `ErrTargetNotEmpty`，残骸
//	   行不被合并、其余表不被补写——既有 `TestImport_RequiresEmptyBusinessTables` 的
//	   目标库是"完整导过一遍"，断言分不出"整体拒绝"与"按主键 upsert"。
//
// Fault-injection cases for the §2.11.3 failure matrix; each targets a side an existing
// case cannot distinguish (retryability, hash-vs-size, partial-residue refusal).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// TestFaultInject_RetryAfterMidwayFailure ① 数据事务中途失败后，修好的 dump 必须能在
// 同一目标库上原样重试成功，且底座干净（无 cutover 行、first_write_at 为 NULL）。
// 逆改点：把单事务拆成分段提交，或失败路径漏回收某张表 → 重试要么撞上
// ErrTargetNotEmpty，要么写出重复行 / 残留半行，下面的计数断言红。
func TestFaultInject_RetryAfterMidwayFailure(t *testing.T) {
	orphanDir, orphanManifest := fixtureDumpWithOrphanMessage(t)
	db := pgtest.NewTestDB(t)

	_, err := NewImporter(db, orphanDir, orphanManifest).Import(importCtx)
	require.Error(t, err, "外键违例的 dump 必须整笔失败")

	var sessions, messages int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM sessions`).Scan(&sessions))
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM messages`).Scan(&messages))
	require.Zero(t, sessions+messages, "失败导入必须先回滚成空表——重试的前提是空底座")

	// 修好的 dump（同结构、无孤儿行），模拟操作者修数据后重跑。
	fixedDir, fixedManifest := fixtureDumpWithManifest(t)
	report, err := NewImporter(db, fixedDir, fixedManifest).Import(importCtx)
	require.NoError(t, err, "回滚后的空底座必须允许重试，不得要求重新建库")
	require.False(t, report.Failed(), "重试不得留下失败表：%v", report.Failures)
	require.Equal(t, int64(1), report.Rows["sessions"])
	require.Equal(t, int64(1), report.Rows["messages"])

	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM sessions`).Scan(&sessions))
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM messages`).Scan(&messages))
	require.Equal(t, 1, sessions, "重试只能导入一份数据，不得残留失败批次的半行")
	require.Equal(t, 1, messages, "重试只能导入一份数据，不得残留失败批次的半行")

	var cutoverRows, firstWrites int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM storage_cutover`).Scan(&cutoverRows))
	require.Zero(t, cutoverRows, "重试成功也不得写 cutover 行")
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT count(*) FROM storage_cutover WHERE first_write_at IS NOT NULL`).Scan(&firstWrites))
	require.Zero(t, firstWrites, "first_write_at 归切换后的首个业务事务，导入重试不得代写")
}

// TestFaultInject_SameSizeSourceDrift ② 源库文件**同尺寸、单字节**改动必须被判成
// `DBFileDrift`：判据是 SHA-256，不是尺寸（尺寸相等时若只比尺寸就会放行被改写的库）。
// 逆改点：把 `compareDBFiles` 的 `want.Size != got.Size || want.SHA256 != got.SHA256`
// 缩成只比尺寸 → 本例如实读回 OK()==true、DBFileDrift 为空，断言红。
func TestFaultInject_SameSizeSourceDrift(t *testing.T) {
	root, specs := newAssetFixture(t)
	manifest := assetBuild(t, root, specs)

	report, err := VerifyAssets(context.Background(), root, manifest)
	require.NoError(t, err)
	require.True(t, report.OK(), "刚采集完必须零差异：%v", report)

	// 单字节翻转：改最后一个字节，尺寸逐字节不变。
	dbPath := filepath.Join(root, "basalt.db")
	original, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	require.NotEmpty(t, original)
	mutated := append([]byte(nil), original...)
	mutated[len(mutated)-1] ^= 0xff
	require.NotEqual(t, original, mutated)
	require.NoError(t, os.WriteFile(dbPath, mutated, 0o644))

	after, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	require.Equal(t, len(original), len(after), "故障注入必须保持文件尺寸不变，才证明判据是哈希")

	report, err = VerifyAssets(context.Background(), root, manifest)
	require.NoError(t, err)
	require.False(t, report.OK(), "同尺寸内容改动必须让复核不通过")
	require.Len(t, report.DBFileDrift, 1, "只应报 basalt.db 一处漂移：%v", report.DBFileDrift)
	require.Contains(t, strings.Join(report.DBFileDrift, "；"), "basalt.db")

	before, present := assetDBFile(manifest.SourceDBFiles, "basalt.db")
	require.True(t, present, "清单必须记有 basalt.db 的身份")
	got, present := assetDBFile(report.DBFiles, "basalt.db")
	require.True(t, present, "复核必须读回 basalt.db 的身份")
	require.Equal(t, before.Size, got.Size, "尺寸相同")
	require.NotEqual(t, before.SHA256, got.SHA256, "SHA-256 必须不同——漂移就靠它判出来")
}

// TestFaultInject_RefusesPartiallyMigratedTarget ③ 部分迁移副本（上一次迁移中途崩溃、
// 只落下 sessions 一行）重试必须整体拒绝：残骸行原样保留，其余表不被补写，不写
// cutover / first_write_at——"部分成功"的副本只能整体重来，不能按主键合并。
// 逆改点：把 `assertTarget` 的空表断言改成 upsert / 跳过已存在行 → 本例如实把
// messages 补成 1 行（或把 sessions 改成 2 行），下面的零值断言红。
func TestFaultInject_RefusesPartiallyMigratedTarget(t *testing.T) {
	dir, manifest := fixtureDumpWithManifest(t)
	db := pgtest.NewTestDB(t)

	// 崩溃残骸：只有 sessions 一行，messages / management_operations 都没落。
	// （`execFixture` 收的是 SQLite 夹具的 *sql.DB，这里直接对目标库执行。）
	_, err := db.ExecContext(importCtx, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at)
		VALUES ('s1', 'demo', 'cli', 'u1', 'active', 1, 1000, 1000)`)
	require.NoError(t, err, "写崩溃残骸")

	_, err = NewImporter(db, dir, manifest).Import(importCtx)
	require.ErrorIs(t, err, ErrTargetNotEmpty, "非空目标库必须整体拒绝，不得续跑")
	require.Contains(t, err.Error(), "sessions", "拒绝必须点名非空的表")

	var sessions, messages, operations, cutoverRows int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM sessions`).Scan(&sessions))
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM messages`).Scan(&messages))
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT count(*) FROM management_operations`).Scan(&operations))
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM storage_cutover`).Scan(&cutoverRows))
	require.Equal(t, 1, sessions, "残骸行必须原样保留：既不删也不合并")
	require.Zero(t, messages, "被拒的导入不得补写其余表")
	require.Zero(t, operations, "被拒的导入不得补写其余表")
	require.Zero(t, cutoverRows, "被拒的导入不得留下 cutover 行")
}
