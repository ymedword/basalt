//go:build integration

package postgres

// 本文件是 US-22 迁移 `0011_memory_learning.sql` 的**有库**用例（T003）：
//
//  1. **两个闭集加宽来自迁移本身**——v10 上 `auto_extract` / `memory_extract` 被旧 CHECK
//     拒绝，迁到 v11 后同一行可写，且加宽不是放开（闭集外仍拒、约束只有一条）；
//  2. **失败分类与状态的闭集**——`window_gone` 等六个分类、五个状态可写，闭集外被拒，
//     结构性 CHECK（尝试次数 1..3、窗口右界不小于左界）在约束定义里可见；
//  3. **旧库前向升级**——v10 的五表尚不存在、版本停在 10；迁到 v11 后五表就位、版本
//     推到 11，既有记忆 / 调用行原样保留（只追加，不改写历史数据）；
//  4. **候选来源关系表的结构**——自引用、重复对、外键指向不存在的条目 / 抽取都被库层
//     拒绝，`source_entry_id` 上有召回复核要用的索引。
//
// 逆改点：删掉 0011 的两条 CHECK 扩展 → 用例 1 迁移后的写入仍被旧闭集拒绝而红；把
// `window_gone` 从分类闭集里去掉 → 用例 2 红；0011 漏建任一新表 / 版本记录不推进 →
// 用例 3 红；去掉 `entry_id <> source_entry_id` 或外键 / 索引 → 用例 4 红。
//
// Database-backed cases for migration 0011: the two widened closed sets, the
// extraction closed sets, the v10 -> v11 forward path, and the candidate-sources DDL.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// lmBase 是迁移夹具的时间基线（Unix 毫秒，全部时间列都是 bigint）。
// lmBase is the fixture's millisecond time origin.
var lmBase int64 = 1_762_419_600_000

// lmMemoryInsert 写一条记忆行；source 由参数给出，用来对照两个版本的闭集宽度。
// lmMemoryInsert inserts one memory row with the source parameterized.
const lmMemoryInsert = `INSERT INTO memory_entries
	(scope, agent_name, section, content, created_at, state, source, valid_from, content_hash)
	VALUES ('agent', 'demo', 'archive', 'content', $1, 'active', $2, $1, 'hash')`

// lmLLMCallInsert 写一条模型调用行；purpose 由参数给出（本表无外键，run_id 可独立给）。
// lmLLMCallInsert inserts one llm_calls row with the purpose parameterized.
const lmLLMCallInsert = `INSERT INTO llm_calls
	(run_id, session_id, round, provider, model, duration_ms, status, created_at, purpose)
	VALUES ('lm-run', 'lm-s1', 0, 'prov', 'model', 0, 'success', $1, $2)`

// lmExtractionInsert 写一条抽取账本行；failure_class 与 status 由参数给出。
// lmExtractionInsert inserts one extraction row with class and status parameterized.
const lmExtractionInsert = `INSERT INTO memory_extractions
	(extraction_id, agent_name, session_id, run_id, last_attempt_run_id, origin_caller_id,
	 status, window_from_message_id, window_to_message_id, window_messages,
	 prompt_version, rule_version, provider, model, failure_class, created_at, last_attempt_at)
	VALUES ($1, 'demo', 'lm-s1', $2, $2, 'c1', $3, 1, 2, 2, 'pv1', 'rv1', 'prov', 'model', $4, $5, $5)`

// TestUS22_Migration_SourcePurposeExtended 判两个闭集的加宽**来自迁移**：v10 上
// `auto_extract` / `memory_extract` 被旧 CHECK 拒绝，迁到 v11 后同一行可写；闭集之外
// 的值继续被拒，且每个约束只剩一条（旧约束被替换而不是叠加）。
// 逆改点：只 ADD 新约束不 DROP 旧约束 → 迁移后写入仍被旧闭集拒绝；把新值从闭集里
// 去掉 → 迁移后的写入报约束错误，两个"迁移后可写"断言红。
// TestUS22_Migration_SourcePurposeExtended pins the widened source/purpose closed sets.
func TestUS22_Migration_SourcePurposeExtended(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()

	require.NoError(t, ensureSchema(ctx, conn, 10), "先只建到 v10：闭集仍是旧宽度")
	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, 10, version)

	_, err = conn.ExecContext(ctx, lmMemoryInsert, lmBase, "auto_extract")
	require.Error(t, err, "v10 闭集必须拒绝 auto_extract")
	require.Contains(t, err.Error(), "memory_entries_source_check",
		"拒绝必须来自 source 闭集，不是别的失败")

	_, err = conn.ExecContext(ctx, lmLLMCallInsert, lmBase, "memory_extract")
	require.Error(t, err, "v10 闭集必须拒绝 memory_extract")
	require.Contains(t, err.Error(), "llm_calls_purpose_check",
		"拒绝必须来自 purpose 闭集，不是别的失败")

	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion), "v10 库必须能前向迁到 v11")
	version, err = schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version, "迁移终点必须是当前支持版本（US-24 起为 12）")

	_, err = conn.ExecContext(ctx, lmMemoryInsert, lmBase, "auto_extract")
	require.NoError(t, err, "v11 闭集必须接受 auto_extract")
	_, err = conn.ExecContext(ctx, lmLLMCallInsert, lmBase, "memory_extract")
	require.NoError(t, err, "v11 闭集必须接受 memory_extract")

	// 加宽不等于放开：闭集之外的值仍然被拒。
	_, err = conn.ExecContext(ctx, lmMemoryInsert, lmBase, "not_a_source")
	require.Error(t, err, "source 闭集之外的值必须继续被拒")
	require.Contains(t, err.Error(), "memory_entries_source_check")
	_, err = conn.ExecContext(ctx, lmLLMCallInsert, lmBase, "not_a_purpose")
	require.Error(t, err, "purpose 闭集之外的值必须继续被拒")
	require.Contains(t, err.Error(), "llm_calls_purpose_check")

	// 每个约束只存在一条，且定义里出现新字面量——DROP/ADD 是替换，不是叠加。
	for table, check := range map[string]struct{ name, want string }{
		"memory_entries": {name: "memory_entries_source_check", want: "auto_extract"},
		"llm_calls":      {name: "llm_calls_purpose_check", want: "memory_extract"},
	} {
		var count int
		var def string
		require.NoError(t, conn.QueryRowContext(ctx, `
			SELECT count(*), COALESCE(min(pg_get_constraintdef(oid)), '')
			  FROM pg_constraint
			 WHERE conrelid = $1::regclass AND conname = $2`,
			table, check.name).Scan(&count, &def))
		require.Equal(t, 1, count, "%s 必须恰好一条（旧约束被替换，不是叠加）", check.name)
		require.Contains(t, def, check.want, "%s 的定义必须包含新字面量", check.name)
	}
}

// TestUS22_Migration_FailureClassWindowGone 判抽取账本的状态 / 失败分类闭集与结构性
// CHECK：`window_gone` 等六个分类与五个状态可写，闭集外被拒；尝试次数上下界与窗口
// 边界写在约束定义里。
// 逆改点：把 window_gone 从分类闭集删掉 → 分类逐值写入红；放宽 status 闭集 → 闭集外
// 写入不再报错而红。
// TestUS22_Migration_FailureClassWindowGone pins the extraction status/failure-class DDL.
func TestUS22_Migration_FailureClassWindowGone(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion))
	// US-24 把支持版本推进到 12；本用例的判据是「v11 学习 DDL 在场」，不是版本号等于 11。
	var learningMigration string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT description FROM schema_version WHERE version = 11`).Scan(&learningMigration),
		"版本历史中必须有 v11 记录")
	require.Contains(t, learningMigration, "0011_memory_learning", "本用例的夹具必须包含 v11 学习 DDL")

	// 夹具提取行的提取器与版本列取固定值；状态与分类逐值参数化。
	insert := func(id, status string, class any) error {
		_, err := conn.ExecContext(ctx, lmExtractionInsert, id, "lm-"+id, status, class, lmBase)
		return err
	}

	classes := []string{
		"model_error", "invalid_output", "timeout",
		"audit_unavailable", "extractor_unavailable", "window_gone",
	}
	for _, class := range classes {
		require.NoError(t, insert("ex-"+class, "failed", class), "分类 %s 必须可写", class)
	}
	for _, status := range []string{"pending", "success", "failed", "skipped", "unknown"} {
		require.NoError(t, insert("st-"+status, status, nil), "状态 %s 必须可写", status)
	}

	err := insert("ex-bad-class", "failed", "not_a_class")
	require.Error(t, err, "分类闭集之外的值必须被拒")
	require.Contains(t, err.Error(), "memory_extractions_failure_class_check")
	err = insert("st-bad", "not_a_status", nil)
	require.Error(t, err, "状态闭集之外的值必须被拒")
	require.Contains(t, err.Error(), "memory_extractions_status_check")

	// 结构性 CHECK 在定义里可见：尝试次数 1..3、窗口右界不小于左界、错误消息有长度上限。
	rows, err := conn.QueryContext(ctx, `
		SELECT pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE conrelid = 'memory_extractions'::regclass AND contype = 'c'`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var defs []string
	for rows.Next() {
		var def string
		require.NoError(t, rows.Scan(&def))
		defs = append(defs, def)
	}
	require.NoError(t, rows.Err())
	joined := ""
	for _, def := range defs {
		joined += def + "\n"
	}
	require.Contains(t, joined, "window_to_message_id >= window_from_message_id")
	require.Contains(t, joined, "attempts >= 1")
	require.Contains(t, joined, "attempts <= 3")
	require.Contains(t, joined, "length(error_message) <= 1024")
}

// TestUS22_Migration_ForwardFromV10 判旧库前向升级的**实际路径**：v10 上五张学习表
// 尚不存在、版本停在 10；迁到 v11 后五表全部就位、版本记录追加到 11，且既有记忆行与
// 调用行的来源 / 用途原样保留（迁移只追加，不改写历史数据）。
// 逆改点：0011 用 CREATE OR REPLACE / UPDATE 改写既有行 → 迁移后旧行被改而红；漏建
// 任一新表 → `to_regclass` 空值断言红；版本记录不追加 → 版本断言红。
// TestUS22_Migration_ForwardFromV10 pins the append-only v10 -> v11 upgrade path.
func TestUS22_Migration_ForwardFromV10(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()

	require.NoError(t, ensureSchema(ctx, conn, 10), "先只建到 v10")
	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, 10, version)

	for _, table := range []string{
		"memory_extractions", "memory_extraction_cursors", "memory_candidate_meta",
		"memory_candidate_sources", "memory_method_drafts",
	} {
		var reg sql.NullString
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, table).Scan(&reg))
		require.False(t, reg.Valid, "v10 上不得存在学习表 %s", table)
	}

	// 迁移前的存量数据：显式保存的记忆与一次聊天调用，迁移后必须原样可读。
	_, err = conn.ExecContext(ctx, lmMemoryInsert, lmBase, "save_memory")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, lmLLMCallInsert, lmBase, "chat")
	require.NoError(t, err)

	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion), "v10 库必须能前向迁到 v11")
	version, err = schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version, "迁移终点必须是当前支持版本（US-24 起为 12）")

	for _, table := range []string{
		"memory_extractions", "memory_extraction_cursors", "memory_candidate_meta",
		"memory_candidate_sources", "memory_method_drafts",
	} {
		var reg sql.NullString
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, table).Scan(&reg))
		require.True(t, reg.Valid, "迁移后学习表 %s 必须存在", table)
	}

	// 版本记录逐条追加（1..11），没有重跑或改写。
	var versions, maxVersion int
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT count(*), max(version) FROM schema_version`).Scan(&versions, &maxVersion))
	require.Equal(t, SupportedSchemaVersion, versions, "版本记录必须与当前支持版本同数")
	require.Equal(t, SupportedSchemaVersion, maxVersion)
	var desc string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT description FROM schema_version WHERE version = 11`).Scan(&desc))
	require.Contains(t, desc, "0011_memory_learning")

	// 存量行原样保留。
	var source string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT source FROM memory_entries WHERE content = 'content'`).Scan(&source))
	require.Equal(t, "save_memory", source, "迁移不得改写既有记忆行的来源")
	var purpose string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT purpose FROM llm_calls WHERE run_id = 'lm-run'`).Scan(&purpose))
	require.Equal(t, "chat", purpose, "迁移不得改写既有调用行的用途")
}

// TestUS22_Migration_CandidateSourcesTable 判候选来源关系表的库层约束：自引用、重复
// 对、指向不存在的条目或抽取都被拒；`source_entry_id` 上的索引存在（召回闭包复核按它
// 反向查找）。
// 逆改点：去掉 `entry_id <> source_entry_id` → 自引用插入成功而红；去掉任一外键 →
// 悬空引用插入成功而红；删掉索引 → 索引存在断言红。
// TestUS22_Migration_CandidateSourcesTable pins the candidate-sources DDL constraints.
func TestUS22_Migration_CandidateSourcesTable(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion))

	var entryA, entryB int64
	require.NoError(t, conn.QueryRowContext(ctx,
		lmMemoryInsert+` RETURNING id`, lmBase, "save_memory").Scan(&entryA))
	require.NoError(t, conn.QueryRowContext(ctx,
		lmMemoryInsert+` RETURNING id`, lmBase, "save_memory").Scan(&entryB))

	_, err := conn.ExecContext(ctx, lmExtractionInsert,
		"ex-src", "lm-run-src", "pending", nil, lmBase)
	require.NoError(t, err)

	insertSource := func(entryID, sourceID int64, extractionID string) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO memory_candidate_sources
			(entry_id, source_entry_id, extraction_id, created_at) VALUES ($1, $2, $3, $4)`,
			entryID, sourceID, extractionID, lmBase)
		return err
	}

	require.NoError(t, insertSource(entryA, entryB, "ex-src"), "普通来源关系必须可写")

	err = insertSource(entryA, entryB, "ex-src")
	require.Error(t, err, "同一对 (entry_id, source_entry_id) 只能有一行")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "23505", pgErr.Code)

	err = insertSource(entryA, entryA, "ex-src")
	require.Error(t, err, "自引用必须被拒")
	require.Contains(t, err.Error(), "memory_candidate_sources")

	err = insertSource(entryA, 9_999_999, "ex-src")
	require.Error(t, err, "来源条目必须真实存在")
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "23503", pgErr.Code)
	require.Contains(t, err.Error(), "memory_candidate_sources_source_entry_id_fkey")

	err = insertSource(entryB, entryA, "ex-missing")
	require.Error(t, err, "抽取行必须真实存在")
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "23503", pgErr.Code)
	require.Contains(t, err.Error(), "memory_candidate_sources_extraction_id_fkey")

	var indexName string
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT indexname FROM pg_indexes
		 WHERE schemaname = current_schema()
		   AND tablename = 'memory_candidate_sources'
		   AND indexname = 'idx_memory_candidate_sources_source'`).Scan(&indexName))
	require.Equal(t, "idx_memory_candidate_sources_source", indexName)
}
