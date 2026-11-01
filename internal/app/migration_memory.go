package app

// 本文件是 `db switch` 里的**旧记忆一次性只读导入**（D-18 / D-22，2026-10-30 用户裁决
// V-2）。它只做三件事：按包内副本建只读源 → 调 `memory.ImportLegacy` 逐条对账 → 把
// 报告投影成命令面能打印的普通数据。
//
// **调用位置是判据的一部分**：必须在 `VerifyTarget` 之后、`WriteCutover` 之前。
// `db verify --target` 的判据是"目标库逐表行数与清单严格相等"（`compareTargetRows`），
// 而旧记忆导入必然给 `memory_entries` 增加行——排在它前面会让目标复核当场红，且重跑
// 无法自愈（多出来的行不会自己消失）。排在切换记录之前则保住 D-18 的「导入不完整不得
// 切换」：任何一条没导进去，本次调用返回错误，`storage_cutover` 不落行，实例仍停在
// 切换前。
//
// **读的是包内副本**（`<dump>/package/memory/**`），不是源工作区：包已按清单逐字节
// 复核过（`VerifyPackage`），导入行因此与切换证据绑定同一份快照；`--workspace` 只是
// 可选的重盘门禁，不能假定它存在。源工作区的原文件一个字节都不动，留作回退层（D-22）。
// The legacy memory import runs between VerifyTarget and WriteCutover: target verification
// compares exact row counts, so imported memory rows must come after it, while the cutover
// row must come after a complete import.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/dbmigrate"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// importLegacyMemory 把包内 `memory/` 下的旧 markdown 记忆导入目标库并逐条对账。
//
// 四种判定（D-18）：
//   - 读源 / 写库失败 → 返回错误（切换不发生）；
//   - 有 `Unreadable` → 返回错误：读不出来的旧条目此刻还存在于旧载体，放过它等于
//     切换后静默丢失；
//   - 有 `Conflicts` → 返回错误：权威库里的同一条已不在生效态，谁对要人裁决，
//     导入不得替人决定；
//   - 零条 → **不是**失败，但也**不是**成功证据：报告照常返回，渲染层必须显式打出
//     "源里没有可导入的旧记忆条目"。
//
// A failure, an unreadable entry or a conflict all stop the switch; an empty source is
// reported as such rather than dressed up as a successful migration.
func (p migrationPort) importLegacyMemory(ctx context.Context, db *postgres.DB, dumpDir string) (memory.ImportReport, error) {
	if db == nil {
		return memory.ImportReport{}, errors.New("db switch：导入旧记忆需要已打开的目标库句柄")
	}
	src := memory.NewMarkdownLegacySource(dbmigrate.PackageRoot(dumpDir))
	store := postgres.NewMemoryStore(db, func() time.Time { return p.clock() })
	// 只导条目、不生成向量：嵌入回填是**下一次正常启动**的独立作业（V-5 的启动期回填，
	// D-18 的"回填向量"），因此这里不装配 Embedder——缺它只影响语义召回，不影响逐条
	// 对账；迁移事务里也**不得**出现外部嵌入调用。
	semantic := memory.NewSemanticStore(memory.SemanticDeps{Store: store})

	report, err := memory.ImportLegacy(ctx, src, semantic)
	if err != nil {
		return report, fmt.Errorf("db switch：旧记忆导入失败（切换未发生，storage_cutover 未写入）: %w", err)
	}
	if len(report.Unreadable) > 0 || len(report.Conflicts) > 0 {
		// 恢复路径**不是**"处置完再重跑 `db switch`"：本次可能已经导入了一部分行，
		// 而 `db import` 的空库前置（`assertTarget`）与 `db verify --target` 的逐表
		// 行数都不接受带了导入行的目标库，重跑会在预检 / 校验处再红一次。正确路径是
		// 处置完报告后**重建目标库**，从 `db migrate` 起重走；`storage_cutover` 未写入，
		// 旧实例仍是权威，重建目标库不动旧实例的数据。
		// Recovery is: fix what the report names, recreate the target, then rerun the
		// migrate → import → switch chain; a plain re-run hits the exact-row-count and
		// empty-target preconditions.
		return report, fmt.Errorf(
			"db switch：旧记忆导入不完整（不可读 %d 条 / 冲突 %d 条）——按 D-18 不得切换："+
				"先按报告逐条处置旧载体或权威库，再重建目标库并从 db migrate 起重走 "+
				"（本次可能已写入部分导入行，直接重跑会在空库前置或 db verify --target 处再失败）；"+
				"本次 storage_cutover 未写入，旧实例不受影响",
			len(report.Unreadable), len(report.Conflicts))
	}
	return report, nil
}

// projectMemoryImport 把导入报告投影成命令面数据。**只投影、不判定**：判定已在
// `importLegacyMemory` 里做完，渲染层不得再解释一遍结论。
//
// 报告里**不带条目正文**：冲突与不可读只给分层 / 分区 / 状态 / 主键与原因，正文可能
// 是业务敏感内容，运维要看得按主键去查，不随终端输出与日志扩散。
// projectMemoryImport projects the report without re-judging it and without carrying any
// entry body: scope, section, state and id are enough to act on.
func projectMemoryImport(report memory.ImportReport) cli.MigrationMemoryImport {
	out := cli.MigrationMemoryImport{
		Imported: len(report.Imported),
		Skipped:  len(report.Skipped),
		Total:    report.Total(),
	}
	for _, conflict := range report.Conflicts {
		out.Conflicts = append(out.Conflicts, fmt.Sprintf("%s/%s 条目 #%d 当前状态 %s",
			memoryScopeLabel(conflict.Scope), memorySectionLabel(conflict.Section), conflict.EntryID, conflict.State))
	}
	for _, unreadable := range report.Unreadable {
		out.Unreadable = append(out.Unreadable, fmt.Sprintf("%s/%s %s",
			memoryScopeLabel(unreadable.Scope), memorySectionLabel(unreadable.Section), unreadable.Reason))
	}
	return out
}

// projectMemoryBaseline 把导入后的行数对账投影成命令面数据（S-F04）。只投影不判定：
// 三个数不相等时 `Switch` 根本不会产出报告。
// projectMemoryBaseline projects the reconciled post-import baseline; the switch only
// returns a report when the numbers already matched.
func projectMemoryBaseline(evidence dbmigrate.LegacyMemoryEvidence) cli.MigrationMemoryBaseline {
	return cli.MigrationMemoryBaseline{
		Checked:      evidence.Checked,
		ImportRows:   evidence.BaselineRows,
		Imported:     evidence.Imported,
		ObservedRows: evidence.ObservedRows,
	}
}

// memoryScopeLabel 是报告里的分层标签：空 Agent 名 = 全局层。
// memoryScopeLabel renders the layer; an empty agent name is the global layer.
func memoryScopeLabel(scope core.Scope) string {
	if scope.Agent == "" {
		return "global"
	}
	return "agent:" + scope.Agent
}

// memorySectionLabel 是报告里的分区标签；文件级不可读条目没有分区，显式写出来，
// 不让空字段看起来像"分区为空"。
// memorySectionLabel renders the section; a file-level unreadable entry has none.
func memorySectionLabel(section core.Section) string {
	if section == "" {
		return "（未定位分区）"
	}
	return string(section)
}
