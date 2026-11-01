package dbmigrate

// 本文件是**三维回退判据**与**停止自动回退**的唯一判定点（颗粒度文档 §2.15.1–§2.15.3，
// C-43 / C-48）。三条纪律：
//  1. `first_write_at` 只覆盖"数据库业务写入"一个维度（C-13）——空标记本身**不**构成
//     "可以无损回退"的证据，工作区文件与外部副作用各自要单独取证；
//  2. 三维都在场且都判"干净"才允许自动回退；任一维"脏"或"无法证明"即
//     `ErrAutoRollbackBlocked`，转人工核查，**不得**因为"哪一维看起来不重要"而跳过；
//  3. 管理写入按 C-48 二分：白名单维护写入选 `MaintenanceWrites`（可随回退丢弃 / 重放），
//     不可忽略的管理变更选 `ManagementChanges`——**集合非空即不得自动回退**，
//     两类都**不**写 `first_write_at`，也**不得**用"窗口还开着"互相顶替。
//
// This file is the single decision point for the three-dimensional rollback judgement.
// An empty first_write_at proves the database business-write dimension only; workspace
// files and external side effects need their own evidence. Any dirty or unprovable
// dimension blocks the automatic rollback and hands the case to manual review.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/storage/postgres"
)

// 三维判据的维度名（报告与断言的稳定字面量）。
// Dimension names are stable literals shared by the report and the assertions.
const (
	DimensionDatabase  = "database"
	DimensionWorkspace = "workspace_files"
	DimensionExternal  = "external_side_effects"
)

// EvidenceVerdict 是一维取证的三值：干净 / 脏 / 无法证明。后两者都阻止自动回退，
// 区别只在"已经确认有问题"与"证明不了没问题"——报告要能分开读。
// EvidenceVerdict is the three-valued result of one dimension: clean, dirty, unprovable.
type EvidenceVerdict string

const (
	// EvidenceClean 表示该维度已取证且未发现切换后的新增。
	EvidenceClean EvidenceVerdict = "clean"
	// EvidenceDirty 表示该维度已确认存在切换后的新增 / 变化，自动回退必须停止。
	EvidenceDirty EvidenceVerdict = "dirty"
	// EvidenceUnprovable 表示无法证明该维度干净（缺证据 / 结果未知 / 无法归因）。
	EvidenceUnprovable EvidenceVerdict = "unprovable"
)

// ErrAutoRollbackBlocked 表示三维判据未全部干净，回退只能转人工核查（C-43 / C-48）。
// ErrAutoRollbackBlocked means at least one dimension is dirty or unprovable.
var ErrAutoRollbackBlocked = errors.New("dbmigrate: 回退三维判据未全部干净，不得自动回退，转人工核查")

// DimensionEvidence 是一维的取证结果与读数：Findings 里逐条写对象与处置依据，
// 报告据此列人工核查清单，**不得**只给一个结论词。
// DimensionEvidence carries one dimension's verdict plus the findings behind it.
type DimensionEvidence struct {
	Dimension string
	Verdict   EvidenceVerdict
	Findings  []string
}

// Clean 判断该维是否干净（只有 clean 才算）。
func (e DimensionEvidence) Clean() bool { return e.Verdict == EvidenceClean }

// MaintenanceWrite 是 C-48 ① 的白名单维护写入：不关闭回退窗口，回退时随目标库丢弃
// 或按原序重放；报告逐项写"已丢弃 / 已重放"的处置，**不得**与业务写入混记。
// MaintenanceWrite is one class-① whitelisted maintenance write (C-48).
type MaintenanceWrite struct {
	Kind        string // storage_cutover / backfill_env_sources
	Object      string
	Disposition string
}

// ManagementChange 是 C-48 ② 的不可忽略管理变更：不写 `first_write_at`，但改变授权与
// 资源状态，旧实例没有这些事实——集合非空即不得自动回退。
// ManagementChange is one class-② management change; any entry blocks auto rollback.
type ManagementChange struct {
	OperationID  string
	Action       string
	TargetKind   string
	TargetID     string
	Status       string
	ActorSubject string
	Source       string
	CreatedAt    int64 // Unix 毫秒
}

// ExternalAction 是切换后已经发出的通知 / 工具调用：无法撤销，只能前滚对账或人工补偿，
// 因此逐条进前滚处置清单（对象 / 时间 / 处置），**不得**只报一个计数。
// ExternalAction is one already-emitted external action listed for forward-fix handling.
type ExternalAction struct {
	Kind        string // notification / tool_invocation
	Object      string
	OccurredAt  int64 // Unix 毫秒（通知取 resolved_at，调用取完成或开始时刻）
	Disposition string
}

// WorkspaceSnapshot 是工作区文件维度的一份读数：相对路径（`/` 分隔）→ 文件内容 sha256。
// WorkspaceSnapshot maps workspace-relative paths to the sha256 of their bytes.
type WorkspaceSnapshot map[string]string

// RollbackRequest 是评估所需的全部输入。基线取导入报告的逐表行数；工作区两份快照
// 由调用方按"切换时 / 现在"分别采集（**不**由本函数自行猜路径）；UnknownSideEffects
// 是操作者显式申报的"无法证明无副作用"项（如连接测试结果未知）。
//
// `LegacyMemory` 是旧记忆导入完成后的行数对账（S-F04）：切换会在这份导入报告之后
// 再写 `memory_entries`，如果把那批行读成业务写入，回退判据就会永远判脏。豁免的
// 条件不是"推迟取基线"本身，而是三个数**对得上**——没申报或对不上都按原基线判定。
//
// RollbackRequest carries everything the assessment may not assume: the import baseline,
// both workspace snapshots, the operator-declared unknown side effects, and the post-import
// legacy-memory reconciliation that re-bases memory_entries.
type RollbackRequest struct {
	Baseline           ImportReport
	LegacyMemory       LegacyMemoryEvidence
	WorkspaceAtCutover WorkspaceSnapshot
	WorkspaceNow       WorkspaceSnapshot
	UnknownSideEffects []string
}

// LegacyMemoryTable 是旧记忆导入落库的表名（稳定字面量，对账与判据共用）。
// LegacyMemoryTable is the table the legacy memory import writes into.
const LegacyMemoryTable = "memory_entries"

// LegacyMemoryEvidence 是旧记忆导入完成后的 `memory_entries` 行数对账（S-F04）：
// `BaselineRows`（导入前，`VerifyTarget` 已确认与清单相等）+ `Imported`（本次导入
// 条数）= `ObservedRows`（导入后实测）。三者相等才证明这批行属于本次受控迁移且与
// 导入报告一致，判据才把该表基线推迟到 `ObservedRows`。
//
// `Checked` 为假 = 没有对账证据：不享受豁免，`memory_entries` 多出来的行仍按业务
// 写入处理——没有证据就没有推迟基线的资格。
//
// LegacyMemoryEvidence re-bases memory_entries only when all three numbers reconcile.
// Without it the table keeps the original import baseline and any excess row is a
// business write.
type LegacyMemoryEvidence struct {
	Checked      bool
	BaselineRows int64
	Imported     int64
	ObservedRows int64
}

// ErrLegacyMemoryImportMismatch 表示导入后的 `memory_entries` 行数与「导入前基线 +
// 导入报告条数」不符：可能是报告之外的写入，也可能是导入没全部落库。两种都不得
// 写入切换记录。
// ErrLegacyMemoryImportMismatch blocks the cutover row when the import does not reconcile.
var ErrLegacyMemoryImportMismatch = errors.New(
	"dbmigrate: 旧记忆导入后的 memory_entries 行数与导入报告不符，不得写入切换记录")

// CountLegacyMemoryRows 读当前 `memory_entries` 行数：切换侧在导入**之前**用它取
// 基线（与清单无关的实测值——目标复核已经保证清单内的表与库一致）。
// CountLegacyMemoryRows reads the pre-import baseline for the reconciliation.
func CountLegacyMemoryRows(ctx context.Context, db *postgres.DB) (int64, error) {
	if db == nil {
		return 0, errors.New("dbmigrate: 读取 memory_entries 行数需要已装配的目标库句柄")
	}
	return countTableRows(ctx, db, LegacyMemoryTable)
}

// CheckLegacyMemoryImport 在旧记忆导入之后、写切换记录之前点名 `memory_entries`：
// 期望行数 = 导入前实测行数（`CountLegacyMemoryRows`）+ 本次导入条数，实测不等即
// `ErrLegacyMemoryImportMismatch`——切换不发生，`storage_cutover` 不落行，实例停在
// 切换前。
//
// 成功时返回的证据就是 `RollbackRequest.LegacyMemory` 该填的值：切换与回退判据因此
// 绑在同一份读数上，不靠事后推断。
// CheckLegacyMemoryImport reconciles memory_entries right after the import; the returned
// evidence is exactly what a later RollbackRequest must carry.
func CheckLegacyMemoryImport(ctx context.Context, db *postgres.DB, baselineRows, imported int64) (LegacyMemoryEvidence, error) {
	if db == nil {
		return LegacyMemoryEvidence{}, errors.New("dbmigrate: 旧记忆导入对账需要已装配的目标库句柄")
	}
	if baselineRows < 0 || imported < 0 {
		return LegacyMemoryEvidence{}, fmt.Errorf(
			"dbmigrate: 旧记忆导入对账的输入不能为负（导入前 %d / 导入 %d）", baselineRows, imported)
	}
	observed, err := countTableRows(ctx, db, LegacyMemoryTable)
	if err != nil {
		return LegacyMemoryEvidence{}, err
	}
	evidence := LegacyMemoryEvidence{
		Checked:      true,
		BaselineRows: baselineRows,
		Imported:     imported,
		ObservedRows: observed,
	}
	expected := baselineRows + imported
	switch {
	case observed > expected:
		return evidence, fmt.Errorf(
			"%w: 期望 %d 行（导入前 %d + 本次导入 %d），实测 %d 行——多出 %d 行，导入报告之外出现了写入",
			ErrLegacyMemoryImportMismatch, expected, baselineRows, imported, observed, observed-expected)
	case observed < expected:
		return evidence, fmt.Errorf(
			"%w: 期望 %d 行（导入前 %d + 本次导入 %d），实测 %d 行——少 %d 行，导入未全部落库",
			ErrLegacyMemoryImportMismatch, expected, baselineRows, imported, observed, expected-observed)
	}
	return evidence, nil
}

// RollbackAssessment 是一次完整取证的结果。`BeforeCutover` 为真时其余字段为空——
// 未切换即源库仍是权威，目标端数据不构成回退对象。
// RollbackAssessment is the outcome of one full evidence pass.
type RollbackAssessment struct {
	BeforeCutover     bool
	Cutover           CutoverRecord
	Dimensions        []DimensionEvidence
	MaintenanceWrites []MaintenanceWrite
	ManagementChanges []ManagementChange
	ExternalActions   []ExternalAction
}

// AssessRollback 按三维判据取证并给出**是否允许自动回退**。它只读不写：不改
// `storage_cutover`、不动目标库、不动工作区——回退动作本身由 CLI / 演练脚本执行，
// 执行前必须先过 `RequireAutoRollbackAllowed`，执行后另记回退报告（§2.15.2 第 5 条）。
//
// AssessRollback gathers evidence only. It never writes; the rollback itself runs after
// RequireAutoRollbackAllowed and must leave its own report.
func AssessRollback(ctx context.Context, db *postgres.DB, req RollbackRequest) (RollbackAssessment, error) {
	if db == nil {
		return RollbackAssessment{}, errors.New("dbmigrate: 回退判据需要已装配的目标库句柄")
	}
	record, err := RequireCutover(ctx, db)
	if errors.Is(err, ErrNoCutoverMarker) {
		return RollbackAssessment{BeforeCutover: true}, nil
	}
	if err != nil {
		return RollbackAssessment{}, err
	}

	assessment := RollbackAssessment{Cutover: record}

	database, maintenance, changes, err := assessDatabaseDimension(ctx, db, record, req.Baseline, req.LegacyMemory)
	if err != nil {
		return RollbackAssessment{}, err
	}
	assessment.Dimensions = append(assessment.Dimensions, database)
	assessment.MaintenanceWrites = maintenance
	assessment.ManagementChanges = changes

	assessment.Dimensions = append(assessment.Dimensions, assessWorkspaceDimension(req.WorkspaceAtCutover, req.WorkspaceNow))

	external, actions, err := assessExternalDimension(ctx, db, record.CutoverAt, req.UnknownSideEffects)
	if err != nil {
		return RollbackAssessment{}, err
	}
	assessment.Dimensions = append(assessment.Dimensions, external)
	assessment.ExternalActions = actions

	sort.SliceStable(assessment.ExternalActions, func(i, j int) bool {
		if assessment.ExternalActions[i].OccurredAt != assessment.ExternalActions[j].OccurredAt {
			return assessment.ExternalActions[i].OccurredAt < assessment.ExternalActions[j].OccurredAt
		}
		return assessment.ExternalActions[i].Object < assessment.ExternalActions[j].Object
	})
	return assessment, nil
}

// AutoRollbackAllowed 判是否允许自动回退：未切换直接允许；否则三维必须**都在场**
// 且都干净，并且不可忽略的管理变更集合为空。少一维 = 没取证，按不允许处理。
// AutoRollbackAllowed requires all three dimensions present and clean, and no class-②
// management change.
func (a RollbackAssessment) AutoRollbackAllowed() bool {
	if a.BeforeCutover {
		return true
	}
	if len(a.ManagementChanges) > 0 {
		return false
	}
	if len(a.Dimensions) != 3 {
		return false
	}
	for _, dimension := range a.Dimensions {
		if !dimension.Clean() {
			return false
		}
	}
	return true
}

// RequireAutoRollbackAllowed 是**所有**自动回退入口的闸门：不通过即
// `ErrAutoRollbackBlocked`，文案带上逐维理由与管理变更对象，直接可作人工核查清单。
// RequireAutoRollbackAllowed gates every automatic rollback entry point.
func (a RollbackAssessment) RequireAutoRollbackAllowed() error {
	if a.AutoRollbackAllowed() {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrAutoRollbackBlocked, strings.Join(a.BlockReasons(), "；"))
}

// BlockReasons 返回阻止自动回退的逐条理由（维度读数 + 管理变更对象）。
// BlockReasons lists every reason that blocks the automatic rollback.
func (a RollbackAssessment) BlockReasons() []string {
	if a.BeforeCutover {
		return nil
	}
	var reasons []string
	for _, dimension := range a.Dimensions {
		if dimension.Clean() {
			continue
		}
		if len(dimension.Findings) == 0 {
			reasons = append(reasons, fmt.Sprintf("%s: %s", dimension.Dimension, dimension.Verdict))
			continue
		}
		for _, finding := range dimension.Findings {
			reasons = append(reasons, fmt.Sprintf("%s: %s", dimension.Dimension, finding))
		}
	}
	for _, change := range a.ManagementChanges {
		reasons = append(reasons, fmt.Sprintf("管理变更 %s（%s %s/%s，%s）",
			change.OperationID, change.Action, change.TargetKind, change.TargetID, change.Status))
	}
	if len(a.Dimensions) != 3 {
		reasons = append(reasons, fmt.Sprintf("三维判据不完整（%d/3）", len(a.Dimensions)))
	}
	return reasons
}

// Report 渲染回退报告正文（§2.15.2 第 5 条：回退动作另记报告）。报告只含对象、时间与
// 处置，不含密文 / 令牌 / 密钥值。
// Report renders the rollback report: objects, times and dispositions, never secrets.
func (a RollbackAssessment) Report() string {
	var b strings.Builder
	if a.BeforeCutover {
		b.WriteString("回退判据：尚未切换——源库仍是权威，目标端数据不构成回退对象（无损）。\n")
		return b.String()
	}
	fmt.Fprintf(&b, "回退判据：cutover_id=%s source_schema_version=%d cutover_at=%s\n",
		a.Cutover.CutoverID, a.Cutover.SourceSchemaVersion, formatMillis(a.Cutover.CutoverAt))
	if a.Cutover.RollbackWindowOpen() {
		b.WriteString("回退窗口：开（first_write_at IS NULL）\n")
	} else {
		fmt.Fprintf(&b, "回退窗口：已关闭（first_write_at=%s），只允许前滚\n", formatMillis(*a.Cutover.FirstWriteAt))
	}
	for _, dimension := range a.Dimensions {
		fmt.Fprintf(&b, "- %s：%s\n", dimension.Dimension, dimension.Verdict)
		for _, finding := range dimension.Findings {
			fmt.Fprintf(&b, "    %s\n", finding)
		}
	}
	if len(a.MaintenanceWrites) > 0 {
		b.WriteString("维护写入（C-48 ①，可丢弃 / 重放）：\n")
		for _, write := range a.MaintenanceWrites {
			fmt.Fprintf(&b, "  - %s %s：%s\n", write.Kind, write.Object, write.Disposition)
		}
	}
	if len(a.ManagementChanges) > 0 {
		b.WriteString("管理变更（C-48 ②，非空即不得自动回退）：\n")
		for _, change := range a.ManagementChanges {
			fmt.Fprintf(&b, "  - %s %s %s/%s status=%s actor=%s source=%s at=%s\n",
				change.OperationID, change.Action, change.TargetKind, change.TargetID,
				change.Status, change.ActorSubject, change.Source, formatMillis(change.CreatedAt))
		}
	}
	if len(a.ExternalActions) > 0 {
		b.WriteString("前滚补偿清单：\n")
		for _, action := range a.ExternalActions {
			fmt.Fprintf(&b, "  - %s %s at=%s：%s\n", action.Kind, action.Object, formatMillis(action.OccurredAt), action.Disposition)
		}
	}
	if a.AutoRollbackAllowed() {
		b.WriteString("结论：三维干净，允许自动回退（回退动作另记，不得改写 storage_cutover 历史行）。\n")
	} else {
		fmt.Fprintf(&b, "结论：停止自动回退，转人工核查——%s\n", strings.Join(a.BlockReasons(), "；"))
	}
	return b.String()
}

// assessDatabaseDimension 取证"数据库业务写入"维度：回退窗口 + 导入基线逐表行数 +
// C-48 ② 管理变更。白名单维护写入（C-48 ①）单列进 MaintenanceWrites，**不**关闭窗口。
//
// 行数比对的口径：基线取导入报告的 `Rows`（清单里的每张表都有计数，含 0 行表）。
// 除 `mcp_credential_sources`（启动期 BackfillEnvSources 补记，幂等可重放）外，任何
// 表的行数与基线不符都按业务写入处理——哪怕 `first_write_at` 还空着，那只能说明
// 有写路径绕过了 `MarkFirstWrite`，不是"干净"。
//
// `memory_entries` 是唯一例外，且例外**不是**推迟基线本身：只有 `legacy.Checked` 且
// 「导入前 + 导入报告 = 导入后实测」三个数对得上时，才把该表基线换成导入后实测
// （S-F04）。对不上按方向分档——多出 = 报告之外的写入（脏），少掉 = 导入未全部落库
// （无法证明）；没申报则不享受豁免，多出来的行照常按业务写入处理。
//
// 已知不可见项：值级改动（如校验期 `scheduled_tasks.enabled` 临时开关）不改变行数，
// 属白名单但本判据看不见，由操作者按 C-48 逐项申报；报告不声称覆盖它。
func assessDatabaseDimension(ctx context.Context, db *postgres.DB, record CutoverRecord, baseline ImportReport, legacy LegacyMemoryEvidence) (DimensionEvidence, []MaintenanceWrite, []ManagementChange, error) {
	evidence := DimensionEvidence{Dimension: DimensionDatabase, Verdict: EvidenceClean}
	maintenance := []MaintenanceWrite{{
		Kind:        "storage_cutover",
		Object:      record.CutoverID,
		Disposition: "保留作证据（不得改写历史行）；目标库如需保留作只读快照",
	}}

	if err := RequireOpenRollbackWindow(record); err != nil {
		evidence.Verdict = EvidenceDirty
		evidence.Findings = append(evidence.Findings, err.Error())
	}

	if len(baseline.Rows) == 0 {
		// 空基线不是"没有差异"，是**没有测量**：逐表比对会一行都不跑，直接给出干净
		// 结论。C-43 的同一句话对工作区维度已经执行（缺快照即无法证明），数据库维度
		// 不得例外——否则调用方漏传导入报告就会被读成"可以自动回退"。
		// An empty baseline is no measurement, not zero delta; it must never read clean.
		// 已确认的脏维不因"同时无法证明"降级——报告要先说不可撤销的那一条。
		if evidence.Verdict != EvidenceDirty {
			evidence.Verdict = EvidenceUnprovable
		}
		evidence.Findings = append(evidence.Findings,
			"缺少导入基线行数（基线为空）——没有测量就不能记为干净（C-43）")
	}

	rows, legacyChecked := reconcileLegacyMemoryBaseline(&evidence, baseline, legacy)

	changes, err := readManagementChanges(ctx, db, record.CutoverAt)
	if err != nil {
		return evidence, nil, nil, err
	}

	for _, table := range sortedTables(rows) {
		count, err := countTableRows(ctx, db, table)
		if err != nil {
			return evidence, nil, nil, err
		}
		delta := count - rows[table]
		if delta == 0 {
			continue
		}
		switch table {
		case "mcp_credential_sources":
			if delta > 0 {
				maintenance = append(maintenance, MaintenanceWrite{
					Kind:        "backfill_env_sources",
					Object:      fmt.Sprintf("mcp_credential_sources +%d 行", delta),
					Disposition: "随回退丢弃或按原序重放（BackfillEnvSources 幂等）",
				})
				continue
			}
			evidence.Verdict = EvidenceDirty
			evidence.Findings = append(evidence.Findings,
				fmt.Sprintf("表 mcp_credential_sources 比导入基线少 %d 行——维护写入不会删行", -delta))
		case "management_operations":
			if delta != int64(len(changes)) {
				evidence.Verdict = EvidenceUnprovable
				evidence.Findings = append(evidence.Findings,
					fmt.Sprintf("表 management_operations 比导入基线多 %d 行，但切换后操作只有 %d 条——无法归因，按 C-48 转人工",
						delta, len(changes)))
			}
		case LegacyMemoryTable:
			if legacy.Checked && !legacyChecked {
				// 对账证据在场但对不上：该表已经在对账里定过性（脏 / 无法证明），
				// 不再拿原基线重复判一次——否则"少掉"会被这里改写成另一个方向的误报。
				continue
			}
			evidence.Verdict = EvidenceDirty
			switch {
			case delta > 0 && legacyChecked:
				evidence.Findings = append(evidence.Findings,
					fmt.Sprintf("表 memory_entries 比导入后基线多 %d 行——受控导入完成后出现了业务写入", delta))
			case delta > 0:
				evidence.Findings = append(evidence.Findings,
					fmt.Sprintf("表 memory_entries 比基线多 %d 行——受控旧记忆导入必须随 LegacyMemory 申报"+
						"「导入前 / 导入条数 / 导入后实测」三个数，否则无法与业务写入区分（S-F04）", delta))
			default:
				evidence.Findings = append(evidence.Findings,
					fmt.Sprintf("表 memory_entries 比基线少 %d 行——旧记忆导入不会删行", -delta))
			}
		default:
			evidence.Verdict = EvidenceDirty
			if delta > 0 {
				evidence.Findings = append(evidence.Findings,
					fmt.Sprintf("表 %s 比导入基线多 %d 行——first_write_at 为空却出现业务写入，或写路径绕过了 MarkFirstWrite", table, delta))
			} else {
				evidence.Findings = append(evidence.Findings,
					fmt.Sprintf("表 %s 比导入基线少 %d 行——业务行被删除", table, -delta))
			}
		}
	}

	if len(changes) > 0 {
		evidence.Verdict = EvidenceDirty
		evidence.Findings = append(evidence.Findings,
			fmt.Sprintf("切换后存在不可忽略的管理变更 %d 项（C-48 ②：集合非空即不得自动回退）", len(changes)))
	}
	return evidence, maintenance, changes, nil
}

// reconcileLegacyMemoryBaseline 决定 `memory_entries` 的比对基线（S-F04）并返回逐表
// 有效基线（原基线的副本，不改调用方的导入报告）。`legacyChecked` 只在三个数对得上、
// 基线换成导入后实测时为真——用例靠它区分"导入完成后无写入"与"没有对账证据"。
//
// 三个数不对账时的分档：多出 = 报告之外有写入（脏，**不得**被"推迟基线"吸收）；
// 少掉 = 导入报告与库不符、结果未知（无法证明）；证据自相矛盾（不是针对这份导入
// 报告 / 数值为负）= 无法证明。已确认的脏不因同时无法证明降级。
//
// reconcileLegacyMemoryBaseline returns the effective per-table baseline and whether the
// post-import re-base took effect. Excess rows beyond the report are dirty, shortfall is
// unprovable, and contradictory evidence never counts as clean.
func reconcileLegacyMemoryBaseline(evidence *DimensionEvidence, baseline ImportReport, legacy LegacyMemoryEvidence) (map[string]int64, bool) {
	rows := make(map[string]int64, len(baseline.Rows)+1)
	for table, count := range baseline.Rows {
		rows[table] = count
	}
	if !legacy.Checked {
		return rows, false
	}

	dumpRows, found := baseline.Rows[LegacyMemoryTable]
	escalate := func(verdict EvidenceVerdict) {
		if verdict == EvidenceDirty || evidence.Verdict != EvidenceDirty {
			evidence.Verdict = verdict
		}
	}
	switch {
	case !found:
		escalate(EvidenceUnprovable)
		evidence.Findings = append(evidence.Findings,
			"提供了旧记忆导入对账，但导入基线里没有 memory_entries 行数——无法对账（C-43）")
	case legacy.BaselineRows != dumpRows:
		escalate(EvidenceUnprovable)
		evidence.Findings = append(evidence.Findings, fmt.Sprintf(
			"旧记忆导入对账的导入前行数 %d 与导入基线 %d 不一致——证据不是针对这份导入报告",
			legacy.BaselineRows, dumpRows))
	case legacy.Imported < 0 || legacy.ObservedRows < 0:
		escalate(EvidenceUnprovable)
		evidence.Findings = append(evidence.Findings, fmt.Sprintf(
			"旧记忆导入对账的数值为负（导入 %d / 导入后实测 %d）", legacy.Imported, legacy.ObservedRows))
	case legacy.ObservedRows > dumpRows+legacy.Imported:
		escalate(EvidenceDirty)
		evidence.Findings = append(evidence.Findings, fmt.Sprintf(
			"导入后实测 %d 行，比「导入基线 %d + 导入报告 %d」多 %d 行——导入报告之外出现了写入",
			legacy.ObservedRows, dumpRows, legacy.Imported, legacy.ObservedRows-dumpRows-legacy.Imported))
	case legacy.ObservedRows < dumpRows+legacy.Imported:
		escalate(EvidenceUnprovable)
		evidence.Findings = append(evidence.Findings, fmt.Sprintf(
			"导入后实测 %d 行，比「导入基线 %d + 导入报告 %d」少 %d 行——导入未全部落库，结果未知",
			legacy.ObservedRows, dumpRows, legacy.Imported, dumpRows+legacy.Imported-legacy.ObservedRows))
	default:
		rows[LegacyMemoryTable] = legacy.ObservedRows
		evidence.Findings = append(evidence.Findings, fmt.Sprintf(
			"旧记忆导入对账：导入前 %d 行 + 本次导入 %d 条 = 导入后 %d 行（memory_entries 基线取导入后实测）",
			dumpRows, legacy.Imported, legacy.ObservedRows))
		return rows, true
	}
	return rows, false
}

// readManagementChanges 读切换后的管理操作审计行：这些写入不写 `first_write_at`，
// 但改变授权与资源状态，旧实例没有这些事实。
// 写来源不明 / 无法区分一律按 ② 处理——**不**因为"看起来像维护写入"就放行。
func readManagementChanges(ctx context.Context, db *postgres.DB, cutoverAt int64) ([]ManagementChange, error) {
	rows, err := db.QueryContext(ctx, `SELECT operation_id, action, target_kind, target_id,
			status, actor_subject, source, created_at
		FROM management_operations
		WHERE created_at > $1
		ORDER BY created_at, operation_id`, cutoverAt)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 读切换后管理操作: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var changes []ManagementChange
	for rows.Next() {
		var change ManagementChange
		if err := rows.Scan(&change.OperationID, &change.Action, &change.TargetKind, &change.TargetID,
			&change.Status, &change.ActorSubject, &change.Source, &change.CreatedAt); err != nil {
			return nil, fmt.Errorf("dbmigrate: 读切换后管理操作: %w", err)
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dbmigrate: 读切换后管理操作: %w", err)
	}
	return changes, nil
}

// countTableRows 统计一张表的当前行数；表名来自本仓清单，经 quoteIdent 转义后拼入。
func countTableRows(ctx context.Context, db *postgres.DB, table string) (int64, error) {
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+quoteIdent(table)).Scan(&count); err != nil {
		return 0, fmt.Errorf("dbmigrate: 统计表 %s 行数: %w", table, err)
	}
	return count, nil
}

// sortedTables 让行数比对的顺序稳定（报告逐次可 diff），不依赖 map 遍历顺序。
func sortedTables(counts map[string]int64) []string {
	tables := make([]string, 0, len(counts))
	for table := range counts {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables
}

// SnapshotRowCounts 读当前 schema 全部用户表的行数。它是**验证窗口**的起止对比基线
// （`serve --verification-window`）。
//
// 与 `assessDatabaseDimension` 的分工必须分清，两者不能互相顶替：
//   - 那份判据拿**导入报告**当基线，能发现"切换之后、取证之前"的任何业务写入，并按
//     C-48 ① 区分白名单维护写入；
//   - 窗口没有导入报告，只能比较**窗口起止**——起止之间出现的新行一律按业务写入处理
//     （窗口自己不写库、不启 MCP / 调度，没有需要豁免的维护写入）。
//
// 窗口一开始读到的快照因此**不能**当作"切换后从未写入"的证据：切换与窗口之间的写入
// 只能由 `first_write_at` 与 `AssessRollback` 的导入基线发现。
// SnapshotRowCounts is the window's start/end baseline; it never substitutes for the
// import-baseline comparison that AssessRollback performs.
func SnapshotRowCounts(ctx context.Context, db *postgres.DB) (map[string]int64, error) {
	if db == nil {
		return nil, errors.New("dbmigrate: 行数快照需要已装配的目标库句柄")
	}
	tables, err := postgres.TableNames(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 清点目标库用户表: %w", err)
	}
	counts := make(map[string]int64, len(tables))
	for _, table := range tables {
		count, err := countTableRows(ctx, db, table)
		if err != nil {
			return nil, err
		}
		counts[table] = count
	}
	return counts, nil
}

// RowCountDelta 是一条逐表行数变化（`Delta > 0` 新增，`Delta < 0` 减少）。
// RowCountDelta is one table's change between two snapshots.
type RowCountDelta struct {
	Table string
	Delta int64
}

// CompareRowCounts 比对两份行数快照：一张表在一侧缺失时按整表行数计（表消失本身就是
// 异常，报告不能把它折成 0）。返回顺序按表名稳定排序，报告逐次可 diff。
// CompareRowCounts lists every table whose row count changed, in table-name order.
func CompareRowCounts(before, after map[string]int64) []RowCountDelta {
	tables := make(map[string]struct{}, len(before)+len(after))
	for table := range before {
		tables[table] = struct{}{}
	}
	for table := range after {
		tables[table] = struct{}{}
	}
	deltas := make([]RowCountDelta, 0, len(tables))
	for table := range tables {
		if delta := after[table] - before[table]; delta != 0 {
			deltas = append(deltas, RowCountDelta{Table: table, Delta: delta})
		}
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i].Table < deltas[j].Table })
	return deltas
}

// assessWorkspaceDimension 取证"工作区文件"维度：切换时快照与现在的快照逐文件比对——
// 新增文件（runs/ 产物、手工编辑）、内容变化、文件消失都算脏。缺任一份快照即
// "无法证明"，**不**默认干净。
func assessWorkspaceDimension(atCutover, now WorkspaceSnapshot) DimensionEvidence {
	evidence := DimensionEvidence{Dimension: DimensionWorkspace, Verdict: EvidenceClean}
	if atCutover == nil || now == nil {
		evidence.Verdict = EvidenceUnprovable
		evidence.Findings = append(evidence.Findings,
			"缺少工作区快照（切换时 / 现在）——没有测量就不能记为干净（C-43）")
		return evidence
	}

	paths := make([]string, 0, len(atCutover)+len(now))
	seen := make(map[string]struct{}, len(atCutover)+len(now))
	for path := range atCutover {
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for path := range now {
		if _, ok := seen[path]; ok {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		before, wasThere := atCutover[path]
		after, isThere := now[path]
		switch {
		case !wasThere:
			evidence.Verdict = EvidenceDirty
			evidence.Findings = append(evidence.Findings, fmt.Sprintf("切换后新增文件 %s", path))
		case !isThere:
			evidence.Verdict = EvidenceDirty
			evidence.Findings = append(evidence.Findings, fmt.Sprintf("文件 %s 在切换后消失", path))
		case before != after:
			evidence.Verdict = EvidenceDirty
			evidence.Findings = append(evidence.Findings,
				fmt.Sprintf("文件 %s 内容变化（切换时 %s，现在 %s）", path, shortHash(before), shortHash(after)))
		}
	}
	if len(evidence.Findings) == 0 {
		evidence.Findings = append(evidence.Findings, fmt.Sprintf("已比对 %d 个申报文件，未发现变化", len(paths)))
	}
	return evidence
}

// assessExternalDimension 取证"外部副作用"维度：切换后新发出的通知与工具调用都
// **无法撤销**——已发出的通知逐条进补偿清单；工具调用无法证明只读，同样算脏。
// 操作者申报的未知结果按"无法证明"处理，一律转人工。
func assessExternalDimension(ctx context.Context, db *postgres.DB, cutoverAt int64, unknown []string) (DimensionEvidence, []ExternalAction, error) {
	evidence := DimensionEvidence{Dimension: DimensionExternal, Verdict: EvidenceClean}
	var actions []ExternalAction

	notifications, err := readNotifications(ctx, db, cutoverAt)
	if err != nil {
		return evidence, nil, err
	}
	if len(notifications) > 0 {
		evidence.Verdict = EvidenceDirty
		evidence.Findings = append(evidence.Findings,
			fmt.Sprintf("切换后已发出 %d 条通知——无法撤销，只能前滚对账 / 人工补偿", len(notifications)))
		actions = append(actions, notifications...)
	}

	invocations, err := readNewInvocations(ctx, db, cutoverAt)
	if err != nil {
		return evidence, nil, err
	}
	if len(invocations) > 0 {
		evidence.Verdict = EvidenceDirty
		evidence.Findings = append(evidence.Findings,
			fmt.Sprintf("切换后有 %d 条工具调用记录——无法证明只读，按外部副作用处理", len(invocations)))
		actions = append(actions, invocations...)
	}

	for _, item := range unknown {
		evidence.Verdict = EvidenceUnprovable
		evidence.Findings = append(evidence.Findings,
			fmt.Sprintf("操作者申报的未知外部结果：%s——无法证明无副作用，转人工核查", item))
	}
	if evidence.Verdict == EvidenceClean {
		evidence.Findings = append(evidence.Findings, "切换后无通知、无工具调用、无申报的未知结果")
	}
	return evidence, actions, nil
}

// readNotifications 读切换后 resolve 的通知投递（`notify_deliveries.resolved_at`）。
// 这些行在源端没有外键边，只能在这里按时间取证。
func readNotifications(ctx context.Context, db *postgres.DB, cutoverAt int64) ([]ExternalAction, error) {
	rows, err := db.QueryContext(ctx, `SELECT channel_uid, run_id, resolved_at
		FROM notify_deliveries
		WHERE resolved_at > $1
		ORDER BY resolved_at, tool_invocation_id`, cutoverAt)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 读切换后通知投递: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var actions []ExternalAction
	for rows.Next() {
		var channelUID, runID string
		var resolvedAt int64
		if err := rows.Scan(&channelUID, &runID, &resolvedAt); err != nil {
			return nil, fmt.Errorf("dbmigrate: 读切换后通知投递: %w", err)
		}
		actions = append(actions, ExternalAction{
			Kind:        "notification",
			Object:      fmt.Sprintf("channel=%s run=%s", channelUID, runID),
			OccurredAt:  resolvedAt,
			Disposition: "已发出，无法撤销；前滚对账，必要时人工补偿",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dbmigrate: 读切换后通知投递: %w", err)
	}
	return actions, nil
}

// readNewInvocations 读切换后开始或完成的工具调用：`started_at > cutover` 是新调用，
// `finished_at > cutover` 是切换时尚未结束、切换后才落定结果的调用——两者都算切换后
// 的外部动作。导入时本就存在的历史行（含 pending）不在此列。
func readNewInvocations(ctx context.Context, db *postgres.DB, cutoverAt int64) ([]ExternalAction, error) {
	rows, err := db.QueryContext(ctx, `SELECT tool_name, run_id, status,
			COALESCE(finished_at, started_at) AS occurred_at
		FROM tool_invocations
		WHERE started_at > $1 OR finished_at > $1
		ORDER BY COALESCE(finished_at, started_at), id`, cutoverAt)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 读切换后工具调用: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var actions []ExternalAction
	for rows.Next() {
		var toolName, runID, status string
		var occurredAt int64
		if err := rows.Scan(&toolName, &runID, &status, &occurredAt); err != nil {
			return nil, fmt.Errorf("dbmigrate: 读切换后工具调用: %w", err)
		}
		actions = append(actions, ExternalAction{
			Kind:        "tool_invocation",
			Object:      fmt.Sprintf("tool=%s run=%s status=%s", toolName, runID, status),
			OccurredAt:  occurredAt,
			Disposition: "调用已发出，结果无法撤回；核对外部系统状态后前滚对账或人工补偿",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dbmigrate: 读切换后工具调用: %w", err)
	}
	return actions, nil
}

// CaptureWorkspaceTree 采集工作区快照：对申报的每个文件或目录逐文件取内容 sha256，
// 目录递归展开。缺失的条目按"此刻为空"处理（如切换时 `runs/` 尚未创建）——之后的
// 新增会在比对时显形；符号链接与非常规文件一律拒绝，**不**跟随。
// CaptureWorkspaceTree hashes every regular file under the declared entries; symlinks
// and special files are refused, missing entries count as empty at capture time.
func CaptureWorkspaceTree(root string, entries ...string) (WorkspaceSnapshot, error) {
	rootDir := strings.TrimSpace(root)
	if rootDir == "" {
		return nil, errors.New("dbmigrate: 工作区快照必须给出根目录")
	}
	if len(entries) == 0 {
		return nil, errors.New("dbmigrate: 工作区快照至少要申报一个文件或目录")
	}
	handle, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("dbmigrate: 打开工作区: %w", err)
	}
	defer func() { _ = handle.Close() }()
	fsys := handle.FS()

	snapshot := WorkspaceSnapshot{}
	for _, entry := range entries {
		name := strings.TrimSpace(filepath.ToSlash(entry))
		if name == "" {
			continue
		}
		walkErr := fs.WalkDir(fsys, name, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == name && errors.Is(err, fs.ErrNotExist) {
					// 申报的条目此刻不存在 = 空；切换后才出现的文件由比对发现。
					return nil
				}
				return fmt.Errorf("dbmigrate: 遍历工作区 %s: %w", path, err)
			}
			if d.IsDir() {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("dbmigrate: 工作区快照拒绝符号链接：%s", path)
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("dbmigrate: 工作区快照只接受普通文件：%s", path)
			}
			raw, readErr := fs.ReadFile(fsys, path)
			if readErr != nil {
				return fmt.Errorf("dbmigrate: 读工作区文件 %s: %w", path, readErr)
			}
			sum := sha256.Sum256(raw)
			snapshot[path] = hex.EncodeToString(sum[:])
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	return snapshot, nil
}

// shortHash 把摘要截成前 12 位用于报告；完整值留在快照里，报告不展开。
func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

// formatMillis 把 Unix 毫秒渲染成 UTC RFC3339，报告里统一用可读时间。
func formatMillis(millis int64) string {
	return time.UnixMilli(millis).UTC().Format(time.RFC3339)
}
