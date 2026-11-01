//go:build integration

package dbmigrate

// 本文件判卷导入后的**目标侧复核**（颗粒度文档 §2.15.3 / §3.1.2 的 `VerifyTarget`）：
// 导入前后两侧都必须与清单相符，而**关键引用**不能只靠外键——把
// `run_resource_refs.version_id` 改指另一个同样存在的版本时，外键仍然成立、行数不变，
// 只有「content_hash 与被引用版本一致」这条判据能发现错位。
// Every case runs the real SQLite fixture through the real Exporter into a throwaway
// PostgreSQL schema, then tampers with a reference that still satisfies every foreign key.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// TestVerify_ComparesRefsNotJustRowCounts 校验必须比对关键引用，不能只比行数：
// 把 `run_resource_refs.version_id` 从版本 1 改指版本 2（版本 2 同样存在，外键照样成立，
// 行数是 1 对 1），复核必须失败，且失败项点名 `content_hash` 与被引用版本不一致。
// 逆改点：把判据退化成"只比行数" → 行数相等、报告 OK 为真，本用例在
// `ErrorIs(ErrVerifyFailed)` 与 `ReferenceFailures` 两处断言同时红。
func TestVerify_ComparesRefsNotJustRowCounts(t *testing.T) {
	dump, manifest := migrationDumpWithRefs(t)
	db := pgtest.NewTestDB(t)

	report, err := VerifyDump(importCtx, dump, manifest)
	require.NoError(t, err, "真实导出器产出的 dump 必须通过离线复核")
	require.True(t, report.OK)
	require.Empty(t, report.ReferenceFailures, "源侧引用自洽")

	_, err = NewImporter(db, dump, manifest).Import(importCtx)
	require.NoError(t, err)

	report, err = VerifyTarget(importCtx, db, manifest)
	require.NoError(t, err, "刚导入完的目标库必须与清单一致")
	require.True(t, report.OK)
	require.Contains(t, report.Notes, "management_operations",
		"含 jsonb 列的表跳过了行摘要，缺口必须在报告里显式披露")

	// 篡改：版本 2 存在，外键成立；行数不变；被引用版本的 content_hash 变了。
	_, err = db.ExecContext(importCtx,
		`UPDATE run_resource_refs SET version_id = 2 WHERE run_id = 'run-ref-1'`)
	require.NoError(t, err)

	report, err = VerifyTarget(importCtx, db, manifest)
	require.ErrorIs(t, err, ErrVerifyFailed)
	require.False(t, report.OK)
	require.Equal(t, report.SourceRows["run_resource_refs"], report.TargetRows["run_resource_refs"],
		"行数没变——只比行数的校验在这份数据上会判一致")
	require.NotEmpty(t, report.ReferenceFailures["run_resource_refs"])
	require.Contains(t, err.Error(), "content_hash", "失败文案必须点名脱钩的判据")
	require.Contains(t, err.Error(), "run-ref-1", "失败文案必须点名出问题的引用行")
}

// migrationDumpWithRefs 在公共夹具上加两个资源版本与一条指向版本 1 的 Run 引用。
//
// 版本 1 / 2 各自存在且 content_hash 不同，正是"改指另一个也存在版本"的场景：
// 外键（`version_id REFERENCES resource_versions(id)`）在篡改后依然满足。
func migrationDumpWithRefs(t *testing.T) (string, Manifest) {
	t.Helper()
	source := migrationFixture(t)
	seedMigrationRows(t, source)
	execFixture(t, source, `INSERT INTO runs
		(run_id, session_id, agent, channel, user_id, trigger, status, started_at)
		VALUES ('run-ref-1', 's1', 'demo', 'cli', 'u1', 'user', 'completed', 1000)`)
	execFixture(t, source, `INSERT INTO resource_versions
		(id, resource_kind, resource_uid, resource_id, version_no, content_hash,
		 definition_yaml, created_at, operation_id)
		VALUES (1, 'model_connection', 'mc-uid', 'primary', 1, 'hash-one',
		 'model: primary', 1000, 'op-1000-abcdef')`)
	execFixture(t, source, `INSERT INTO resource_versions
		(id, resource_kind, resource_uid, resource_id, version_no, content_hash,
		 definition_yaml, created_at, operation_id)
		VALUES (2, 'model_connection', 'mc-uid', 'primary', 2, 'hash-two',
		 'model: primary', 1001, 'op-1000-abcdef')`)
	execFixture(t, source, `INSERT INTO run_resource_refs
		(run_id, resource_kind, resource_id, resource_uid, resource_name,
		 version_id, content_hash, released_at)
		VALUES ('run-ref-1', 'model_connection', 'primary', 'mc-uid', 'primary',
		 1, 'hash-one', NULL)`)
	return exportFixture(t, source)
}
