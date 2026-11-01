//go:build integration

package dbmigrate

// 本文件判卷**三维回退判据**与**停止自动回退**（颗粒度文档 §2.15.1–§2.15.3，
// C-43 / C-48；T067）：数据库业务写入、工作区文件、外部副作用各要单独取证；
// 任一维"脏"或"无法证明"⇒ `ErrAutoRollbackBlocked`，转人工核查；
// C-48 的维护写入（白名单）不关闭窗口，管理变更（集合非空）一律停自动回退。
// 全部用例走真实 SQLite v6 夹具 → 真实 `Exporter` / `Importer` → 临时 PG；
// 工作区维度用真实临时目录与真实文件哈希，不用手写假快照。
// Every case runs the real fixture chain into a throwaway PostgreSQL schema; the
// workspace dimension hashes real files in a temp directory.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// cutoverAt 是回退用例统一使用的切换时刻（2026-10-28T14:00Z，Unix 毫秒）。
var cutoverAt = time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)

// backfillUID 是补记用例写入的 MCP server 资源 UID（26 位 Crockford 载荷）。
// backfillUID is the server identity the real backfill writes rows for.
const backfillUID = core.ResourceUID("rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C")

// rollbackFixture 是回退用例的公共前置：真实 SQLite v6 夹具 → 导出 → 导入临时 PG →
// 写切换记录。返回目标库、切换记录与导入报告（报告里有逐表行数，作为数据库维度的基线）。
func rollbackFixture(t *testing.T) (*postgres.DB, CutoverRecord, ImportReport) {
	t.Helper()
	source := migrationFixture(t)
	seedMigrationRows(t, source)
	dump, manifest := exportFixture(t, source)

	db := pgtest.NewTestDB(t)
	report, err := NewImporter(db, dump, manifest).Import(importCtx)
	require.NoError(t, err)
	require.False(t, report.Failed(), "夹具导入必须整体成功，否则基线不可信")

	record, err := WriteCutover(importCtx, db, CutoverRequest{
		ManifestDir: dump,
		Operator:    "cli:test-operator",
		CutoverAt:   cutoverAt,
	})
	require.NoError(t, err)
	return db, record, report
}

// cleanWorkspacePair 采集两份**未变化**的工作区快照（切换时 / 现在），内容一致。
func cleanWorkspacePair(t *testing.T) (WorkspaceSnapshot, WorkspaceSnapshot) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "state"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state", "seen.json"),
		[]byte(`{"seen":["sample-1"]}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "assets.json"),
		[]byte(`{"assets":[]}`), 0o644))

	atCutover, err := CaptureWorkspaceTree(root, "assets.json", "state", "runs")
	require.NoError(t, err, "切换时 runs/ 尚未创建，按空处理")
	now, err := CaptureWorkspaceTree(root, "assets.json", "state", "runs")
	require.NoError(t, err)
	return atCutover, now
}

// dimension 取指定维度的取证结果（用例按名断言，避免按下标依赖顺序）。
func dimension(t *testing.T, assessment RollbackAssessment, name string) DimensionEvidence {
	t.Helper()
	for _, candidate := range assessment.Dimensions {
		if candidate.Dimension == name {
			return candidate
		}
	}
	t.Fatalf("取证结果里没有维度 %q", name)
	return DimensionEvidence{}
}

// sourceCounts 取源 SQLite 的三张代表性表行数，用于证明"源库没有被写"。
func sourceCounts(t *testing.T, source *sql.DB) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"sessions", "messages", "management_operations"} {
		var count int
		require.NoError(t, source.QueryRowContext(importCtx, `SELECT count(*) FROM `+quoteIdent(table)).Scan(&count))
		counts[table] = count
	}
	return counts
}

// TestRollback_BeforeCutoverReturnsToSource 切换前失败**无损**回退：导出只读源库快照，
// 源库行数不变；目标端没有 `storage_cutover` 行时源库仍是权威，回退（丢弃目标）天然允许。
// 逆改点：`AssessRollback` 在无切换行时按"三维都要取证"处理 → `BeforeCutover` 断言红；
// 或把它判成不允许 → `AutoRollbackAllowed` 断言红。
func TestRollback_BeforeCutoverReturnsToSource(t *testing.T) {
	source := migrationFixture(t)
	seedMigrationRows(t, source)
	before := sourceCounts(t, source)

	_, _ = exportFixture(t, source) // 导出 = VACUUM INTO 只读快照
	require.Equal(t, before, sourceCounts(t, source), "导出不得写源库——切换前回退无损的前提")

	// 目标库只有 schema（什么都没导入），更没有切换记录。
	db := pgtest.NewTestDB(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{})
	require.NoError(t, err)
	require.True(t, assessment.BeforeCutover, "没有 storage_cutover 行就不是已切换状态")
	require.True(t, assessment.AutoRollbackAllowed(), "未切换时源库仍是权威，回退无损")
	require.NoError(t, assessment.RequireAutoRollbackAllowed())
	require.Contains(t, assessment.Report(), "尚未切换")
	require.Equal(t, before, sourceCounts(t, source), "回退判据本身不得写源库")
}

// TestRollback_EmptyBaselineIsUnprovable 空导入基线不是"没有差异"，是**没有测量**：
// 逐表比对会一行都不跑，直接把数据库维度判成干净——那等于把"漏传报告"读成"可以
// 自动回退"。C-43 对工作区维度已经执行同一条（缺快照即无法证明），数据库维度不得例外。
// 逆改点：删掉空基线守卫 → 数据库维度回到 `clean`，`AutoRollbackAllowed` 断言红。
func TestRollback_EmptyBaselineIsUnprovable(t *testing.T) {
	db, _, _ := rollbackFixture(t)
	atCutover, now := cleanWorkspacePair(t)

	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           ImportReport{},
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)

	var database DimensionEvidence
	for _, dimension := range assessment.Dimensions {
		if dimension.Dimension == DimensionDatabase {
			database = dimension
		}
	}
	require.Equal(t, EvidenceUnprovable, database.Verdict,
		"空基线是未测量，不得判干净：%v", database.Findings)
	require.Contains(t, strings.Join(database.Findings, "\n"), "缺少导入基线行数")
	require.False(t, assessment.AutoRollbackAllowed(), "无法证明时必须停止自动回退")
}

// TestRollback_BackfillWritesDoNotCloseWindow C-48 ①：BackfillEnvSources 的补记属白名单
// 维护写入——**不**关闭回退窗口、**不**算业务写入；回退时随目标库丢弃或按原序重放。
// 逆改点：把 mcp_credential_sources 的多出行按普通业务写入处理 → 数据库维度判脏、用例红；
// 或补记顺带写 first_write_at → `RollbackWindowOpen` 断言红。
func TestRollback_BackfillWritesDoNotCloseWindow(t *testing.T) {
	db, record, report := rollbackFixture(t)
	_, err := db.ExecContext(importCtx, `INSERT INTO mcp_credential_sources
		(server_uid, binding_key, mode, revision, updated_at, operation_id)
		VALUES ('mcp-1', 'API_KEY', 'env', 1, $1, 'op-backfill-1')`, record.CutoverAt+1000)
	require.NoError(t, err)

	atCutover, now := cleanWorkspacePair(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)
	require.False(t, assessment.BeforeCutover)
	require.Len(t, assessment.Dimensions, 3, "三维必须都在场")
	for _, dimension := range assessment.Dimensions {
		require.Equal(t, EvidenceClean, dimension.Verdict, "维度 %s 应判干净：%v", dimension.Dimension, dimension.Findings)
	}
	require.True(t, assessment.AutoRollbackAllowed(), "补记不关闭回退窗口")
	require.NoError(t, assessment.RequireAutoRollbackAllowed())

	stored, err := RequireCutover(importCtx, db)
	require.NoError(t, err)
	require.True(t, stored.RollbackWindowOpen(), "维护写入不得写 first_write_at")

	require.Len(t, assessment.MaintenanceWrites, 2, "切换记录 + 补记各一项")
	require.Equal(t, "storage_cutover", assessment.MaintenanceWrites[0].Kind)
	backfill := assessment.MaintenanceWrites[1]
	require.Equal(t, "backfill_env_sources", backfill.Kind)
	require.Contains(t, backfill.Object, "mcp_credential_sources")
	require.Contains(t, backfill.Disposition, "丢弃")
	require.Contains(t, assessment.Report(), "backfill_env_sources")
}

// TestBackfill_DoesNotSetFirstWrite —— 设计 §2.16.1 ⑤：启动期**真实**补记走的是初始化
// 写入，不经 `MarkFirstWrite`、不写 `first_write_at`、不关闭回退窗口。与
// `TestRollback_BackfillWritesDoNotCloseWindow` 的分工：那条用手写 INSERT 验**判定面**
// （白名单识别），这条用真实 `BackfillEnvSources` 验**写入面**（补记自己不清窗）。
// 逆改点：补记路径接上 MarkFirstWrite / 任何切面写 first_write_at → 本用例在
// `RollbackWindowOpen` 或 `AutoRollbackAllowed` 红。
// TestBackfill_DoesNotSetFirstWrite runs the real backfill and proves the window stays open.
func TestBackfill_DoesNotSetFirstWrite(t *testing.T) {
	db, record, report := rollbackFixture(t)
	store := postgres.NewMCPCredentialSourceStore(db)
	backfilled, err := store.BackfillEnvSources(importCtx, backfillUID,
		[]string{"API_KEY"}, "system.backfill", time.UnixMilli(record.CutoverAt+1000))
	require.NoError(t, err)
	require.Equal(t, []string{"API_KEY"}, backfilled.Filled)

	stored, err := RequireCutover(importCtx, db)
	require.NoError(t, err)
	require.True(t, stored.RollbackWindowOpen(), "补记不得写 first_write_at")

	atCutover, now := cleanWorkspacePair(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)
	require.True(t, assessment.AutoRollbackAllowed(), "补记不清空自动回退许可")
}

// TestRollback_ManagementWriteBlocksAutoRollback C-48 ②：切换后的管理变更（Key /
// 配额 / 凭据 / 资源发布 / Agent 生命周期 / 通知渠道 / 执行策略）**不**写
// `first_write_at`，但旧实例没有这些事实——集合非空即不得自动回退，转人工核查。
// 逆改点：只看 `first_write_at` 不看 management_operations → 用例在 `AutoRollbackAllowed` 红；
// 或不列对象与时间 → 报告断言红。
func TestRollback_ManagementWriteBlocksAutoRollback(t *testing.T) {
	db, record, report := rollbackFixture(t)
	_, err := db.ExecContext(importCtx, `INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
		 status, detail_json, created_at)
		VALUES ('op-after-cutover', 'credential.rotate', 'model_connection', 'mc-1',
		 'local_trust', 'local_trust', 'cli', 'success', '{"k":"v"}', $1)`, record.CutoverAt+2000)
	require.NoError(t, err)

	atCutover, now := cleanWorkspacePair(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)

	database := dimension(t, assessment, DimensionDatabase)
	require.Equal(t, EvidenceDirty, database.Verdict, "管理变更集合非空即不得自动回退")
	require.Len(t, assessment.ManagementChanges, 1)
	change := assessment.ManagementChanges[0]
	require.Equal(t, "op-after-cutover", change.OperationID)
	require.Equal(t, "credential.rotate", change.Action)
	require.Equal(t, "mc-1", change.TargetID)
	require.Equal(t, record.CutoverAt+2000, change.CreatedAt)

	require.False(t, assessment.AutoRollbackAllowed())
	require.ErrorIs(t, assessment.RequireAutoRollbackAllowed(), ErrAutoRollbackBlocked)
	require.Contains(t, assessment.BlockReasons(), "管理变更 op-after-cutover（credential.rotate model_connection/mc-1，success）")
	reportText := assessment.Report()
	require.Contains(t, reportText, "管理变更")
	require.Contains(t, reportText, "op-after-cutover")
	require.Contains(t, reportText, "停止自动回退")
}

// seedForeignRun 在切换后造一次真实的通知投递：Run + 工具调用 + 投递行，归属按建库
// 种子的内置 `local-admin`（`callers` 不在导入面，外键指向它）。返回工具调用 id，
// 供 notify_deliveries 的外键使用。
func seedForeignRun(t *testing.T, db *postgres.DB, after int64) int64 {
	t.Helper()
	_, err := db.ExecContext(importCtx, `INSERT INTO runs
		(run_id, agent, channel, user_id, trigger, status, started_at,
		 caller_id, authorizing_kind, actor)
		VALUES ('run-notify-1', 'demo', 'cli', 'u1', 'user', 'completed', $1,
		 'local-admin', 'admin-builtin', 'admin-cli')`, after)
	require.NoError(t, err)
	var invocationID int64
	require.NoError(t, db.QueryRowContext(importCtx, `INSERT INTO tool_invocations
		(run_id, tool_name, status, started_at, finished_at)
		VALUES ('run-notify-1', 'notify', 'success', $1, $2) RETURNING id`,
		after, after+1000).Scan(&invocationID))
	_, err = db.ExecContext(importCtx, `INSERT INTO notify_deliveries
		(tool_invocation_id, run_id, agent_uid, channel_uid, channel_name, resolved_at)
		VALUES ($1, 'run-notify-1', 'demo', 'ops-alert', 'ops', $2)`, invocationID, after+1000)
	require.NoError(t, err)
	return invocationID
}

// TestRollback_NotificationBlocksAutoRollback 外部副作用维度：切换后已发出的通知无法
// 撤销——即使数据库与文件两维干净，外部维判脏即停止自动回退。
// 逆改点：外部维恒判干净（不查 notify_deliveries）→ `EvidenceDirty` 断言红；
// 或 `AutoRollbackAllowed` 忽略外部维 → 允许断言红。
func TestRollback_NotificationBlocksAutoRollback(t *testing.T) {
	db, record, report := rollbackFixture(t)
	seedForeignRun(t, db, record.CutoverAt+3000)

	atCutover, now := cleanWorkspacePair(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)

	external := dimension(t, assessment, DimensionExternal)
	require.Equal(t, EvidenceDirty, external.Verdict, "已发出的通知必须阻止自动回退")
	require.False(t, assessment.AutoRollbackAllowed())
	require.ErrorIs(t, assessment.RequireAutoRollbackAllowed(), ErrAutoRollbackBlocked)

	var sawNotification bool
	for _, action := range assessment.ExternalActions {
		if action.Kind == "notification" {
			sawNotification = true
			require.Contains(t, action.Object, "ops-alert")
			require.NotEmpty(t, action.Disposition)
		}
	}
	require.True(t, sawNotification, "通知必须进补偿清单，不能只报计数")
}

// TestRollback_NewExternalActionsListed 前滚处置清单必须逐条列出对象、时间与处置：
// 通知（`resolved_at`）与切换后落定结果的工具调用（`COALESCE(finished_at, started_at)`）。
// 逆改点：只给计数不给对象 → `Object` 断言红；时间取错（用导入时间 / 零值）→
// `OccurredAt` 断言红。
func TestRollback_NewExternalActionsListed(t *testing.T) {
	db, record, report := rollbackFixture(t)
	seedForeignRun(t, db, record.CutoverAt+3000)

	atCutover, now := cleanWorkspacePair(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)
	require.Len(t, assessment.ExternalActions, 2, "通知与工具调用各一条")

	byKind := map[string]ExternalAction{}
	for _, action := range assessment.ExternalActions {
		byKind[action.Kind] = action
	}
	notification, ok := byKind["notification"]
	require.True(t, ok)
	require.Equal(t, record.CutoverAt+4000, notification.OccurredAt, "通知按 resolved_at 记时间")
	require.Contains(t, notification.Disposition, "无法撤销")

	invocation, ok := byKind["tool_invocation"]
	require.True(t, ok)
	require.Contains(t, invocation.Object, "tool=notify")
	require.Equal(t, record.CutoverAt+4000, invocation.OccurredAt, "调用按完成时刻记时间")
	require.Contains(t, invocation.Disposition, "核对外部系统状态")

	reportText := assessment.Report()
	require.Contains(t, reportText, "前滚补偿清单")
	require.Contains(t, reportText, "channel=ops-alert run=run-notify-1")
	require.Contains(t, reportText, "tool=notify run=run-notify-1 status=success")
}

// TestRollback_WorkspaceChangeBlocksAutoRollback 工作区文件维度：切换后新增的 runs/
// 产物与改动的 state/*.json 都算脏——数据库两维（窗口 + 行数）干净也不得自动回退。
// 逆改点：工作区维恒判干净（不比快照）→ `EvidenceDirty` 断言红；
// 只比 hash 不比新增文件 → 新增 `runs/r1/report.md` 的 finding 断言红。
func TestRollback_WorkspaceChangeBlocksAutoRollback(t *testing.T) {
	db, _, report := rollbackFixture(t)

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "state"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state", "seen.json"),
		[]byte(`{"seen":[]}`), 0o644))
	atCutover, err := CaptureWorkspaceTree(root, "state", "runs")
	require.NoError(t, err)

	// 切换后：改 seen.json + 新写一条 runs/ 产物。
	require.NoError(t, os.WriteFile(filepath.Join(root, "state", "seen.json"),
		[]byte(`{"seen":["sample-2"]}`), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "runs", "r1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "runs", "r1", "report.md"),
		[]byte("# report\n"), 0o644))
	now, err := CaptureWorkspaceTree(root, "state", "runs")
	require.NoError(t, err)

	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)

	database := dimension(t, assessment, DimensionDatabase)
	require.Equal(t, EvidenceClean, database.Verdict, "本用例只让文件维变脏")
	workspace := dimension(t, assessment, DimensionWorkspace)
	require.Equal(t, EvidenceDirty, workspace.Verdict)
	findings := strings.Join(workspace.Findings, "\n")
	require.Contains(t, findings, "state/seen.json", "内容变化必须点名文件")
	require.Contains(t, findings, "runs/r1/report.md", "新增产物必须点名文件")
	require.False(t, assessment.AutoRollbackAllowed())
	require.ErrorIs(t, assessment.RequireAutoRollbackAllowed(), ErrAutoRollbackBlocked)
}

// TestRollback_UnknownSideEffectRequiresManual 结果未知 / 无法证明一律转人工：
// 其余三维都干净也不行（C-34 的提交未知协议同理）。
// 逆改点：忽略 UnknownSideEffects → 外部维判干净、允许自动回退，用例红。
func TestRollback_UnknownSideEffectRequiresManual(t *testing.T) {
	db, _, report := rollbackFixture(t)

	atCutover, now := cleanWorkspacePair(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
		UnknownSideEffects: []string{"外部连接测试结果未知（无回执）"},
	})
	require.NoError(t, err)

	external := dimension(t, assessment, DimensionExternal)
	require.Equal(t, EvidenceUnprovable, external.Verdict, "未知结果必须按'无法证明'处理")
	require.Contains(t, strings.Join(external.Findings, "\n"), "外部连接测试结果未知")
	require.False(t, assessment.AutoRollbackAllowed())
	require.ErrorIs(t, assessment.RequireAutoRollbackAllowed(), ErrAutoRollbackBlocked)
}

// TestRollback_MissingWorkspaceEvidenceIsUnprovable 缺工作区快照不是"干净"：
// 没有测量就没有证据，判"无法证明"并阻止自动回退。
// 逆改点：nil 快照按空集合比对 → `EvidenceUnprovable` 断言红。
func TestRollback_MissingWorkspaceEvidenceIsUnprovable(t *testing.T) {
	db, _, report := rollbackFixture(t)

	assessment, err := AssessRollback(importCtx, db, RollbackRequest{Baseline: report})
	require.NoError(t, err)
	workspace := dimension(t, assessment, DimensionWorkspace)
	require.Equal(t, EvidenceUnprovable, workspace.Verdict)
	require.False(t, assessment.AutoRollbackAllowed())
}

// TestRollback_BusinessRowDeltaBlocksAutoRollback 数据库行数维度：即使
// `first_write_at` 还空着，业务表比导入基线多行也说明有写路径绕过了 `MarkFirstWrite`
// ——按脏处理，不得自动回退。
// 逆改点：只信 `first_write_at`、不比行数 → 用例红。
func TestRollback_BusinessRowDeltaBlocksAutoRollback(t *testing.T) {
	db, _, report := rollbackFixture(t)
	_, err := db.ExecContext(importCtx, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at)
		VALUES ('session-bypass', 'demo', 'cli', 'u1', 'active', 0, 2000, 2000)`)
	require.NoError(t, err)

	atCutover, now := cleanWorkspacePair(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)
	database := dimension(t, assessment, DimensionDatabase)
	require.Equal(t, EvidenceDirty, database.Verdict)
	require.Contains(t, strings.Join(database.Findings, "\n"), "表 sessions 比导入基线多 1 行")
	require.False(t, assessment.AutoRollbackAllowed())
}

// TestRollback_ClosedWindowBlocksAutoRollback 回退窗口关闭本身就是数据库维度的脏证据：
// `first_write_at` 已写表示目标库已有业务事务，重启旧库会丢新数据（C-13 / C-43）。
// 本用例只写首写标记、不加任何业务行——行数维度仍与基线一致，能变脏的只有窗口这一条。
// 逆改点：`assessDatabaseDimension` 不看 `RequireOpenRollbackWindow` → 维度断言与
// `AutoRollbackAllowed` 断言红；报告不写窗口状态 → `Contains(..., "已关闭")` 红。
func TestRollback_ClosedWindowBlocksAutoRollback(t *testing.T) {
	db, record, report := rollbackFixture(t)
	firstWrite := time.UnixMilli(record.CutoverAt + 60_000).UTC()

	outcome, err := db.Tx(importCtx, func(ctx context.Context, tx *sql.Tx) error {
		return db.MarkFirstWrite(ctx, tx, firstWrite)
	})
	require.NoError(t, err)
	require.Equal(t, postgres.TxCommitted, outcome)

	atCutover, now := cleanWorkspacePair(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)

	database := dimension(t, assessment, DimensionDatabase)
	require.Equal(t, EvidenceDirty, database.Verdict, "窗口关闭后只允许前滚")
	require.Contains(t, strings.Join(database.Findings, "\n"), record.CutoverID,
		"拒绝必须指出是哪一次切换")
	require.False(t, assessment.AutoRollbackAllowed())
	require.ErrorIs(t, assessment.RequireAutoRollbackAllowed(), ErrAutoRollbackBlocked)
	require.Contains(t, assessment.Report(), "回退窗口：已关闭")
}

// insertLegacyMemoryRows 在目标库补 n 条"旧记忆导入"形态的行（与 `ImportLegacy` 的落库
// 形态一致：state=active、source=legacy_import）——模拟 `db switch` 在导入与切换记录
// 之间写进去的那批行。返回表的当前行数。
// insertLegacyMemoryRows simulates the controlled legacy-memory batch written between the
// dump import and the cutover row.
func insertLegacyMemoryRows(t *testing.T, db *postgres.DB, prefix string, n int) int64 {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := db.ExecContext(importCtx, `INSERT INTO memory_entries
			(scope, section, content, created_at, state, source, valid_from, content_hash)
			VALUES ('global', 'core', $1, $2, 'active', 'legacy_import', $2, $3)`,
			fmt.Sprintf("%s-%d", prefix, i), cutoverAt.UnixMilli(), fmt.Sprintf("hash-%s-%d", prefix, i))
		require.NoError(t, err)
	}
	var count int64
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM memory_entries`).Scan(&count))
	return count
}

// rollbackLegacyMemoryFixture 按真实切换顺序搭夹具：dump 导入 → 受控旧记忆导入 →
// 对账（`CheckLegacyMemoryImport`，与 `db switch` 同一处调用）→ 写切换记录。
// rollbackLegacyMemoryFixture follows the real switch order: import, legacy memory batch,
// reconciliation, then the cutover row.
func rollbackLegacyMemoryFixture(t *testing.T) (*postgres.DB, ImportReport, LegacyMemoryEvidence) {
	t.Helper()
	source := migrationFixture(t)
	seedMigrationRows(t, source)
	dump, manifest := exportFixture(t, source)

	db := pgtest.NewTestDB(t)
	report, err := NewImporter(db, dump, manifest).Import(importCtx)
	require.NoError(t, err)
	require.False(t, report.Failed(), "夹具导入必须整体成功，否则基线不可信")

	beforeImport, err := CountLegacyMemoryRows(importCtx, db)
	require.NoError(t, err)
	require.Equal(t, report.Rows[LegacyMemoryTable], beforeImport,
		"导入前实测必须与导入报告的该表行数一致——switch 侧取的就是这个数")

	require.Equal(t, int64(3), insertLegacyMemoryRows(t, db, "legacy", 3))
	evidence, err := CheckLegacyMemoryImport(importCtx, db, beforeImport, 3)
	require.NoError(t, err, "导入前计数 + 导入条数与实测相等，对账必须成立")
	require.Equal(t, int64(3), evidence.ObservedRows)

	_, err = WriteCutover(importCtx, db, CutoverRequest{
		ManifestDir: dump,
		Operator:    "cli:test-operator",
		CutoverAt:   cutoverAt,
	})
	require.NoError(t, err)
	return db, report, evidence
}

// TestRollback_LegacyMemoryImportBaselineAllowsClean S-F04 主路径：受控旧记忆导入写在
// 导入报告之后，回退判据的 `memory_entries` 基线取导入后实测——切换后**没有**业务写入时
// 数据库维度仍是干净，且报告写明三个数，自动回退不被这三维外的行数差挡住。
//
// 逆改点：不认 `LegacyMemory` 证据（memory_entries 永远按导入报告基线）→ 本用例维度
// 判脏、`AutoRollbackAllowed` 断言红。
func TestRollback_LegacyMemoryImportBaselineAllowsClean(t *testing.T) {
	db, report, evidence := rollbackLegacyMemoryFixture(t)
	atCutover, now := cleanWorkspacePair(t)

	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		LegacyMemory:       evidence,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)

	database := dimension(t, assessment, DimensionDatabase)
	require.Equal(t, EvidenceClean, database.Verdict,
		"受控导入的行不得被读成业务写入：%v", database.Findings)
	require.Contains(t, strings.Join(database.Findings, "\n"),
		"旧记忆导入对账：导入前 0 行 + 本次导入 3 条 = 导入后 3 行")
	require.True(t, assessment.AutoRollbackAllowed(), "三维干净时自动回退必须放行")
}

// TestRollback_LegacyMemoryImportThenBusinessWriteBlocks S-F04 反向：导入完成后再写
// `memory_entries` 就是业务写入——推迟基线**不得**把这段窗口里的写入一起吸收。
//
// 逆改点：把基线固定成导入后实际值（或不再与当前行数比对）→ 维度回到 clean，用例红。
func TestRollback_LegacyMemoryImportThenBusinessWriteBlocks(t *testing.T) {
	db, report, evidence := rollbackLegacyMemoryFixture(t)
	require.Equal(t, int64(4), insertLegacyMemoryRows(t, db, "business", 1))

	atCutover, now := cleanWorkspacePair(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		LegacyMemory:       evidence,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)

	database := dimension(t, assessment, DimensionDatabase)
	require.Equal(t, EvidenceDirty, database.Verdict)
	require.Contains(t, strings.Join(database.Findings, "\n"),
		"表 memory_entries 比导入后基线多 1 行——受控导入完成后出现了业务写入")
	require.False(t, assessment.AutoRollbackAllowed())
}

// TestRollback_LegacyMemoryImportWithoutEvidenceIsDirty S-F04 结果未知之一：导入发生过
// 却没申报对账证据——那批行与业务写入无法区分，不得因为"看起来像导入"就豁免。
//
// 逆改点：把 `memory_entries` 无条件当作可推迟基线（不要求 Checked）→ 维度回到
// clean，用例红。
func TestRollback_LegacyMemoryImportWithoutEvidenceIsDirty(t *testing.T) {
	db, report, _ := rollbackLegacyMemoryFixture(t)
	atCutover, now := cleanWorkspacePair(t)

	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)

	database := dimension(t, assessment, DimensionDatabase)
	require.Equal(t, EvidenceDirty, database.Verdict)
	require.Contains(t, strings.Join(database.Findings, "\n"),
		"受控旧记忆导入必须随 LegacyMemory 申报「导入前 / 导入条数 / 导入后实测」三个数")
	require.False(t, assessment.AutoRollbackAllowed())
}

// TestRollback_LegacyMemoryEvidenceInconsistentIsUnprovable S-F04 结果未知之二：证据
// 在场但对不上（这里模拟导入报告比实际少 1 行）——既不能判干净，也不按"业务写入"
// 冤枉，只按无法证明停自动回退。
//
// 逆改点：只校验 Checked、不校验三个数是否相等 → 维度判干净，用例红。
func TestRollback_LegacyMemoryEvidenceInconsistentIsUnprovable(t *testing.T) {
	db, report, evidence := rollbackLegacyMemoryFixture(t)
	evidence.ObservedRows--

	atCutover, now := cleanWorkspacePair(t)
	assessment, err := AssessRollback(importCtx, db, RollbackRequest{
		Baseline:           report,
		LegacyMemory:       evidence,
		WorkspaceAtCutover: atCutover,
		WorkspaceNow:       now,
	})
	require.NoError(t, err)

	database := dimension(t, assessment, DimensionDatabase)
	require.Equal(t, EvidenceUnprovable, database.Verdict)
	require.Contains(t, strings.Join(database.Findings, "\n"),
		"少 1 行——导入未全部落库，结果未知")
	require.False(t, assessment.AutoRollbackAllowed())
}

// TestCheckLegacyMemoryImport_RejectsMismatch 对账本身的两个方向都拒绝：多出 = 报告
// 之外有写入，少掉 = 导入未全部落库。负数输入是调用错误，不与"行数不符"混为一类。
// 逆改点：只比对 `observed >= expected`（放过多出）或只比 `<=`（放过少掉）→ 对应
// `ErrorIs` 断言红。
func TestCheckLegacyMemoryImport_RejectsMismatch(t *testing.T) {
	db, _, _ := rollbackLegacyMemoryFixture(t)

	_, err := CheckLegacyMemoryImport(importCtx, db, 0, 2)
	require.ErrorIs(t, err, ErrLegacyMemoryImportMismatch)
	require.Contains(t, err.Error(), "多出 1 行")

	_, err = CheckLegacyMemoryImport(importCtx, db, 0, 4)
	require.ErrorIs(t, err, ErrLegacyMemoryImportMismatch)
	require.Contains(t, err.Error(), "少 1 行")

	_, err = CheckLegacyMemoryImport(importCtx, db, -1, 1)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrLegacyMemoryImportMismatch)
}
