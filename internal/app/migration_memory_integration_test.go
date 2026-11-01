//go:build integration

package app

// 本文件是 V-2「旧 markdown 记忆随 `db switch` 一次性只读导入」在**真链路**上的验收面
// （D-18 / D-22）：真 SQLite v6 源库 → 真导出包 → 真 PostgreSQL 目标库。单元层判的是
// 导入源本身的读法（`internal/memory/legacy_markdown_test.go`），这里判的是**挂点**：
// 导入排在 `VerifyTarget` 之后、`WriteCutover` 之前，导入不完整时 `storage_cutover`
// 一行都不落。
// The end-to-end acceptance of the one-shot legacy memory import inside `db switch`.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/dbmigrate"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// 两层旧载体的固定正文：全局层两分区各一条、Agent 层只有核心区一条——条数（3）与分层
// 分布因此可以逐条对号。
const (
	legacyGlobalLayer = "# 记忆\n\n## 核心记忆\n\n- 全局核心一条\n\n## 归档记忆\n\n- 全局归档一条\n"
	legacyAgentLayer  = "# 记忆\n\n## 核心记忆\n\n- ops 核心一条\n\n## 归档记忆\n"
)

// writeWorkspaceMemoryLayer 按工作区布局落一层旧记忆文件（agent 为空 = 全局层）。
// writeWorkspaceMemoryLayer writes one legacy layer at its workspace-relative path.
func writeWorkspaceMemoryLayer(t *testing.T, workspace, agent, body string) string {
	t.Helper()

	rel := filepath.Join("memory", "MEMORY.md")
	if agent != "" {
		rel = filepath.Join("memory", "agents", agent, "MEMORY.md")
	}
	path := filepath.Join(workspace, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// exportWithMemory 在可导出工作区里落好旧记忆层后跑一次导出，返回工作区与包目录。
// 只多了 `memory/` 下文件时导出必须直接封包——`memory` 是内置资产目录，里面的文件
// 属于已归类资产，不是"待核查"。
func exportWithMemory(t *testing.T, layers map[string]string) (workspace, out string) {
	t.Helper()

	port, _ := newMigrationPort(t)
	workspace = seedReadyWorkspace(t)
	for agent, body := range layers {
		writeWorkspaceMemoryLayer(t, workspace, agent, body)
	}
	out = newOutDir(t)
	report, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: workspace, Out: out})
	require.NoError(t, err, "可导出的工作区必须导出成功")
	require.True(t, report.Deliverable, "memory/ 是内置资产目录：只多它不该产生待核查文件")
	return workspace, out
}

// TestSwitch_ImportsLegacyMemoryBeforeCutover 主路径：切换成功、旧记忆逐条进目标库并标
// `source=legacy_import`、`storage_cutover` 记下这次切换、源工作区文件逐字节不变（D-22）。
//
// 逆改点：把导入挪到 `WriteCutover` 之后 → 报告条数为零 / 库行数为零；导入写错来源字面量
// → `source` 断言红；导入时顺手改写源文件 → 字节比较红。
func TestSwitch_ImportsLegacyMemoryBeforeCutover(t *testing.T) {
	workspace, out := exportWithMemory(t, map[string]string{
		"":          legacyGlobalLayer,
		"ops-agent": legacyAgentLayer,
	})
	globalPath := filepath.Join(workspace, "memory", "MEMORY.md")
	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)

	port, _ := newMigrationPort(t)
	dsn := pgtest.NewTestSchema(t)
	report, err := port.Switch(context.Background(),
		cli.DatabaseConfig{DSN: dsn},
		cli.MigrationSwitchRequest{Dump: out, Workspace: workspace})
	require.NoError(t, err, "旧记忆完好时切换必须成功")
	require.NotEmpty(t, report.CutoverID)
	require.Equal(t, 3, report.Memory.Imported, "全局层两条 + Agent 层一条")
	require.Zero(t, report.Memory.Skipped, "首次导入不该有跳过条目")
	require.Equal(t, 3, report.Memory.Total)
	require.Empty(t, report.Memory.Conflicts)
	require.Empty(t, report.Memory.Unreadable)
	require.True(t, report.MemoryBaseline.Checked, "切换必须产出导入后的行数对账（S-F04）")
	require.Equal(t, int64(0), report.MemoryBaseline.ImportRows, "空库导入前 memory_entries 为 0 行")
	require.Equal(t, int64(3), report.MemoryBaseline.Imported)
	require.Equal(t, int64(3), report.MemoryBaseline.ObservedRows, "导入后实测行数就是回退判据取的基线")

	db := openRawWrite(t, dsn)
	require.Equal(t, 3, countRows(t, db,
		`SELECT count(*) FROM memory_entries WHERE source = 'legacy_import'`))
	require.Equal(t, 2, countRows(t, db,
		`SELECT count(*) FROM memory_entries WHERE scope = 'global'`), "全局层两条")
	require.Equal(t, 1, countRows(t, db,
		`SELECT count(*) FROM memory_entries WHERE scope = 'agent' AND agent_name = 'ops-agent'`))
	require.Equal(t, 1, countRows(t, db,
		`SELECT count(*) FROM storage_cutover WHERE cutover_id = $1`, report.CutoverID),
		"导入成功之后才写切换事实")

	after, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "原文件保留作回退层（D-22：不改写）")
}

// TestSwitch_SucceedsWithEmptyLegacyMemory 空载体是**合法**情形：源里没有旧记忆时切换照常
// 完成、报告四类全零——空载体不等于失败；但"空"也**不得**被读成"迁移成功"的证据，那是
// 命令面打印（`printMemoryImportReport`）负责说清的事。
// 逆改点：把 Total == 0 当错误拒绝切换 → 本用例红（存量实例里没写过记忆的很常见）。
func TestSwitch_SucceedsWithEmptyLegacyMemory(t *testing.T) {
	workspace, out := exportWithMemory(t, nil)

	port, _ := newMigrationPort(t)
	dsn := pgtest.NewTestSchema(t)
	report, err := port.Switch(context.Background(),
		cli.DatabaseConfig{DSN: dsn},
		cli.MigrationSwitchRequest{Dump: out, Workspace: workspace})
	require.NoError(t, err, "没有旧记忆不是错误")
	require.NotEmpty(t, report.CutoverID, "空载体同样要留下切换事实")
	require.Zero(t, report.Memory.Total)
	require.True(t, report.MemoryBaseline.Checked, "零条导入也要有对账读数（0 + 0 = 0）")
	require.Zero(t, report.MemoryBaseline.ObservedRows)

	db := openRawWrite(t, dsn)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM memory_entries`))
}

// TestSwitch_RefusesWhenLegacyMemoryUnreadable 导入不完整**不得切换**：损坏的旧载体让
// `Switch` 在写切换事实之前返回错误，目标库**没有 `storage_cutover` 行**——实例停在切换前，
// 修好源文件后整条重跑（完好条目按幂等键记 skipped，损坏条目重读后补进来）。
//
// 逐条对账而不是整批回滚：完好条目照常落库，这是 `ImportLegacy` 的既定口径；本用例因此
// 同时钉住"写得进去的写进去"与"切换事实一行不落"两件事。
//
// 逆改点：把"不可读 / 冲突即拒绝"降级成 WARN（继续切换）→ `storage_cutover` 断言红；
// 把导入挪到写切换事实之后 → 同一条断言也红。
func TestSwitch_RefusesWhenLegacyMemoryUnreadable(t *testing.T) {
	// 缺少归档分区标题 = 结构损坏；同目录的 Agent 层完好，证明拒绝不是"整个目录读不了"。
	workspace, out := exportWithMemory(t, map[string]string{
		"":          "# 记忆\n\n## 核心记忆\n\n- 只有核心区\n",
		"ops-agent": legacyAgentLayer,
	})

	port, _ := newMigrationPort(t)
	dsn := pgtest.NewTestSchema(t)
	report, err := port.Switch(context.Background(),
		cli.DatabaseConfig{DSN: dsn},
		cli.MigrationSwitchRequest{Dump: out, Workspace: workspace})
	require.Error(t, err, "旧记忆导入不完整必须拒绝切换")
	require.Contains(t, err.Error(), "旧记忆导入不完整", "错误要点名是哪一步挡住的")
	require.Contains(t, err.Error(), "不得切换", "错误要说明这是 D-18 的硬判据")
	require.Empty(t, report.CutoverID, "被拒时不得留下切换事实")

	db := openRawWrite(t, dsn)
	// 逐条对账的口径：完好条目照常写入（重跑靠幂等键只补差集），损坏条目一条不写。
	// 拒绝的是**切换**，不是"一行都不许落"——把导入做成整批回滚会与 D-18 的重跑口径冲突。
	require.Equal(t, 1, countRows(t, db,
		`SELECT count(*) FROM memory_entries WHERE scope = 'agent' AND agent_name = 'ops-agent'`),
		"完好的 Agent 层条目照常导入")
	require.Zero(t, countRows(t, db,
		`SELECT count(*) FROM memory_entries WHERE scope = 'global'`),
		"损坏的全局层一条都不写")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM storage_cutover`),
		"切换事实未写入——实例停在切换前")
}

// TestImportLegacyMemory_SecondRunSkipsWithoutDuplicating 幂等：同一个包导入第二次，全部
// 记 skipped 且不新增行（D-18 的"重复导入幂等"）。这条走适配器方法而不是第二遍 `Switch`：
// 第二遍 `Switch` 会先在 `VerifyTarget` 撞上"目标库多出导入行"（已知遗留限制，见 run-state
// R139），那是另一个判据，不该把幂等判据一起拖红。
//
// 逆改点：去掉 `FindEntryByContent` 的命中分支 → 第二次导入变成 duplicated 行，行数断言红。
func TestImportLegacyMemory_SecondRunSkipsWithoutDuplicating(t *testing.T) {
	_, out := exportWithMemory(t, map[string]string{"": legacyGlobalLayer})
	dsn := pgtest.NewTestSchema(t)
	port, _ := newMigrationPort(t)
	db := openRawWrite(t, dsn)

	first, err := port.importLegacyMemory(context.Background(), db, out)
	require.NoError(t, err)
	require.Len(t, first.Imported, 2)

	second, err := port.importLegacyMemory(context.Background(), db, out)
	require.NoError(t, err, "重复导入必须幂等成功，而不是报错")
	require.Empty(t, second.Imported, "第二次不得新增任何行")
	require.Len(t, second.Skipped, 2, "已存在的两条全部记 skipped")
	require.Empty(t, second.Conflicts)
	require.Empty(t, second.Unreadable)
	require.Equal(t, 2, countRows(t, db, `SELECT count(*) FROM memory_entries`))
}

// TestSwitch_RefusesWhenMemoryEntriesDoNotReconcile S-F04：导入报告说 2 条、库里却有 4 行
// （触发器模拟"导入报告之外的写入"）——行数对账不成立，切换必须停在写 storage_cutover
// 之前。这条判据是"推迟基线"能成立的前提：没有它，导入后基线会把真实写入一起吸收。
//
// 逆改点：删掉 `Switch` 里的 `CheckLegacyMemoryImport` 调用 → 切换成功、本用例的
// `ErrorIs` 与 storage_cutover 零行断言同时红。
func TestSwitch_RefusesWhenMemoryEntriesDoNotReconcile(t *testing.T) {
	workspace, out := exportWithMemory(t, map[string]string{"": legacyGlobalLayer})
	port, _ := newMigrationPort(t)
	dsn := pgtest.NewTestSchema(t)

	// 先迁移（`postgres.Open` 与 `db switch` 用的是同一个 `DB.Migrate`），再挂触发器：
	// 每条导入行落库后多写一行，`ImportReport` 与实际行数从此对不上。
	db := openRawWrite(t, dsn)
	_, err := db.ExecContext(context.Background(), `
		CREATE FUNCTION inject_extra_memory() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			INSERT INTO memory_entries
				(scope, section, content, created_at, state, source, valid_from, content_hash)
			VALUES ('global', 'core', 'injected-by-trigger', 1000, 'active', 'legacy_import', 1000, 'injected-hash');
			RETURN NEW;
		END $$`)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `
		CREATE TRIGGER inject_extra_memory AFTER INSERT ON memory_entries
			FOR EACH ROW WHEN (pg_trigger_depth() = 0) EXECUTE FUNCTION inject_extra_memory()`)
	require.NoError(t, err)

	report, err := port.Switch(context.Background(),
		cli.DatabaseConfig{DSN: dsn},
		cli.MigrationSwitchRequest{Dump: out, Workspace: workspace})
	require.ErrorIs(t, err, dbmigrate.ErrLegacyMemoryImportMismatch,
		"导入后行数与「导入前 + 导入报告」不符必须拒绝切换")
	require.Contains(t, err.Error(), "多出", "错误要指明方向（报告之外出现了写入）")
	require.Empty(t, report.CutoverID, "对账不成立时不得留下切换事实")
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM storage_cutover`),
		"storage_cutover 一行不落——实例停在切换前")
}
