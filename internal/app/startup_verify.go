package app

// 本文件实现 `serve --verification-window`：切换后的**启动验证窗口**（§2.12.0b 步骤 7 /
// C-51「启动验证必须离线优先」/ §2.15.2 三维判据，任务 T143）。
//
// 窗口的边界是**结构性**的，不是散落的开关：本路径**不调用** `newRuntime`——不取实例锁、
// 不迁移、不落恢复、不 `mcpSupervisor.ApplyPublished`、不构造 provider 客户端、不启调度器、
// 不启配置监听、不挂 Run 准入。因此"禁调度 / 通知 / MCP / 模型 / 业务写入"不是一组
// flag 判断，而是"这些组件在本路径里根本不存在"。HTTP 面也另起一套最小监听：只回环、
// 只有 `/api/v1/health` 与 `/api/v1/ready` 两条路由。
//
// 失败处置按三维判据取证（数据库业务写入 / 工作区文件 / 外部副作用），回退结论只有五值：
//
//	allowed                    三维都干净——才允许自动回退（恢复原工作区）
//	blocked:external_action    已确认发生外部副作用（MCP 已启动 / 模型已外呼 / 通知已发出）
//	blocked:database_written   已确认数据库出现业务写入（first_write_at 非空，或窗口内行数变化）
//	blocked:workspace_changed  工作区文件相对窗口开始时发生了变化
//	blocked:unknown            有一维**无法证明**干净（探针缺失、快照失败、外部结果未知）
//
// 前四个都**不得**自动恢复原工作区；`allowed` 也只授权回退，回退动作本身不在本文件执行。
// The verification window never assembles the runtime: no scheduler, MCP, provider client,
// config watcher or Run admission exists on this path. Failures are judged against the
// three-dimensional rollback criteria and yield exactly one of five stable decisions.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/dbmigrate"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

const (
	// verificationWindowFlag 是 `serve` 的验证窗口开关。它由装配层在根命令构造**之前**
	// 解析：普通 serve 的装配顺序（锁 → 库 → 迁移 → MCP → 调度器）在这里一条都不该跑。
	// verificationWindowFlag is consumed before the root command exists.
	verificationWindowFlag = "--verification-window"
	// verificationAssetPackageFlag 指向导出包目录（含 assets.json）；缺省即"资产未核对"。
	// 导出包允许在切换前删除（§2.12.0b 步骤 5），所以它**不能**是必填项。
	// The export package may be deleted before the switch, so this stays optional.
	verificationAssetPackageFlag = "--asset-package"
	// verificationWindowDefaultAddr 是窗口监听地址的缺省值：只回环、端口由内核分配。
	// The default is loopback with a kernel-assigned port.
	verificationWindowDefaultAddr = "127.0.0.1:0"
	// verificationWindowFinishProbeTimeout 是窗口结束重读的独立上限。停机信号已经取消
	// serve 上下文，重读必须像 serve 停机的宽限期一样用 `context.WithoutCancel` 脱掉
	// 取消，但不得无界——超时按"无法证明"处理，不阻塞进程退出。
	// The post-window re-read must outlive the cancellation that triggers it, but stay
	// bounded; a timeout is recorded as "unprovable" rather than delaying exit forever.
	verificationWindowFinishProbeTimeout = 5 * time.Second
)

// 回退判定五值（§2.15.2 / C-43 / S-F01 2026-10-31 裁决）。只有第一个允许自动回退。
// The five stable rollback decisions; only the first authorises an automatic rollback.
const (
	windowDecisionAllowed          = "allowed"
	windowDecisionBlockedExternal  = "blocked:external_action"
	windowDecisionBlockedDatabase  = "blocked:database_written"
	windowDecisionBlockedWorkspace = "blocked:workspace_changed"
	windowDecisionBlockedUnknown   = "blocked:unknown"
)

// 报告面六个稳定码（与 `db verify --handoff` 的三个码同一惯例：给脚本判卷的稳定
// fail-closed 码，不是 `docs/contracts/` 的字面量）。
// Six stable report codes for the window's own refusals.
const (
	windowCodeAssetsMismatch   = "assets_mismatch"
	windowCodeDatabaseMissing  = "database_unavailable"
	windowCodeCutoverMissing   = "cutover_marker_missing"
	windowCodeDatabaseWritten  = "database_written"
	windowCodeProbeMissing     = "verification_window_incomplete"
	windowCodeListenerRejected = "listener_not_loopback"
)

// verificationWindowOptions 是窗口的输入面。
// verificationWindowOptions is the window's input face.
type verificationWindowOptions struct {
	// Workspace 是迁移准备副本的**工作区根**（`<copy-root>/.basalt`），与
	// `db verify --handoff <dir>` 的 `<dir>` 同一口径。
	Workspace string
	// AssetPackage 是导出包目录；空表示本次不核对资产（报告会明写"未核对"）。
	AssetPackage string
	// Addr 是 health / ready 的监听地址；必须是回环地址，空取缺省。
	Addr string
	// OnReady 在监听真正建立后被调用一次（测试用它取实际地址并结束窗口）；nil 不回调。
	OnReady func(addr string)
}

// windowEvidence 是窗口内三维取证的结果（§2.15.2）。字段只记录**实际发生**的事：
// 三个副作用计数在正常路径上恒为零——本路径不装配能产生它们的组件；它们存在是为了让
// "窗口内禁 MCP / 模型 / 通知"这条判据可被断言，而不是靠阅读代码相信。
// windowEvidence records what actually happened; the three counters stay zero because the
// components that could raise them are never assembled here.
type windowEvidence struct {
	// MCPStarts / ModelCalls / Notifications 是外部副作用维度的正面证据。
	MCPStarts     int
	ModelCalls    int
	Notifications int
	// UnknownExternal 表示"外部结果未知 / 无法证明无副作用"——按 C-34 / §2.15.3 一律转人工。
	UnknownExternal bool
	// WorkspaceChanged 表示工作区文件相对窗口开始时发生了变化。
	WorkspaceChanged bool
	// WorkspaceUnprovable 表示工作区快照采不到——无法证明没变。
	WorkspaceUnprovable bool
	// DatabaseWritten 表示**已确认**数据库出现业务写入：窗口开始时 `first_write_at`
	// 已非空（切换后经 `MarkFirstWrite` 的写入，C-13），或窗口结束重读发现行数变化
	// （含绕过 `MarkFirstWrite` 的写路径）。
	DatabaseWritten bool
	// DatabaseUnprovable 表示只读库探针缺席或失败——数据库业务写入维度无法证明。
	DatabaseUnprovable bool
}

// externalDirty 判外部副作用维度是否**已确认**脏。
// externalDirty reports a confirmed external side effect.
func (e windowEvidence) externalDirty() bool {
	return e.MCPStarts > 0 || e.ModelCalls > 0 || e.Notifications > 0
}

// windowVerdict 把"脏 / 无法证明"两个布尔折成三维判据的三值。
// windowVerdict folds the two flags into the shared three-valued verdict.
func windowVerdict(dirty, unprovable bool) dbmigrate.EvidenceVerdict {
	switch {
	case dirty:
		return dbmigrate.EvidenceDirty
	case unprovable:
		return dbmigrate.EvidenceUnprovable
	default:
		return dbmigrate.EvidenceClean
	}
}

// decideWindowRollback 按三维判据给回退结论。
//
// 判定核心复用 `dbmigrate.RollbackAssessment.AutoRollbackAllowed`（"三维都在场且都干净"
// 只有一份实现，本文件不再写第二份）；这里只把"哪一维不干净"投影成稳定字面量：
// 已确认的外部副作用优先（不可撤销，报告要先说这一条），其次是已确认的数据库业务
// 写入（`first_write_at` 非空即只允许前滚，C-13），再次是工作区变化，最后是"无法证明"
// ——后者在没有脏维时统一转人工（C-34 / §2.15.3）。
// The decision core is the shared AutoRollbackAllowed; this only projects the first
// non-clean dimension onto a stable literal.
func decideWindowRollback(ev windowEvidence) string {
	assessment := dbmigrate.RollbackAssessment{
		Dimensions: []dbmigrate.DimensionEvidence{
			{
				Dimension: dbmigrate.DimensionExternal,
				Verdict:   windowVerdict(ev.externalDirty(), ev.UnknownExternal),
			},
			{
				Dimension: dbmigrate.DimensionDatabase,
				Verdict:   windowVerdict(ev.DatabaseWritten, ev.DatabaseUnprovable),
			},
			{
				Dimension: dbmigrate.DimensionWorkspace,
				Verdict:   windowVerdict(ev.WorkspaceChanged, ev.WorkspaceUnprovable),
			},
		},
	}
	if assessment.AutoRollbackAllowed() {
		return windowDecisionAllowed
	}
	for _, dimension := range assessment.Dimensions {
		if dimension.Verdict != dbmigrate.EvidenceDirty {
			// 先找**已确认**的脏维；"无法证明"只在没有脏维时兜底。
			continue
		}
		switch dimension.Dimension {
		case dbmigrate.DimensionExternal:
			return windowDecisionBlockedExternal
		case dbmigrate.DimensionDatabase:
			return windowDecisionBlockedDatabase
		case dbmigrate.DimensionWorkspace:
			return windowDecisionBlockedWorkspace
		}
	}
	return windowDecisionBlockedUnknown
}

// verificationWindowReport 是一次验证窗口的事实清单：**拒绝与失败也填**。
// verificationWindowReport is the fact sheet of one window; refusals fill it too.
type verificationWindowReport struct {
	Workspace string

	// OfflineGate 是离线门（身份 / 密钥 / 全部密文 / 配置）的报告。
	OfflineGate cli.HandoffVerifyReport
	// GateReady 为真表示离线门通过；GateCode / GateDetail 是拒绝时的事实。
	GateReady  bool
	GateCode   string
	GateDetail string

	// AssetsChecked 只在真的按清单重盘且逐项一致时为真；否则 AssetsNote 写明"未核对"。
	AssetsChecked bool
	Assets        dbmigrate.AssetReport
	AssetsNote    string

	// DatabaseChecked 只在只读库探针成功时为真（探针读到了切换行，哪怕随后因
	// `first_write_at` 非空而拒绝启动，这一项也是真——库确实被读过）。
	DatabaseChecked bool
	DatabaseNote    string
	SchemaVersion   int
	// CutoverID / FirstWriteAt 是窗口**开始时**的切换事实；FirstWriteAt 为 nil 表示
	// 回退窗口仍开（C-13）。报告据此可独立复算"窗口开始时能不能回退"。
	CutoverID    string
	FirstWriteAt *int64
	// DatabaseFindings 是窗口结束重读时发现的数据库侧变化（只写表名与行数差，不写业务值）。
	DatabaseFindings []string

	// ListenerOpened 只在 `net.Listen` **真正成功**后为真——离线门失败时它必须是 false。
	ListenerOpened bool
	ListenerAddr   string

	Evidence windowEvidence
	Decision string
}

// windowDatabaseState 是一次只读库探针的完整读数。
//
//   - SchemaVersion 判"连上的是不是这个版本的目标库"；
//   - CutoverID / CutoverAt 是**投影键**：窗口结束重读若换了行，说明窗口内又发生了
//     一次切换，起止两份读数不可比（新行的 `first_write_at` 又是空的，单看空标记会
//     把一次重新切换读成"干净"）；
//   - FirstWriteAt 非空 = 已出现经 `MarkFirstWrite` 的业务写入（C-13），此后只允许前滚；
//   - RowCounts 是窗口起止对比的行数基线——覆盖绕过 `MarkFirstWrite` 的写路径。
//
// 空的 FirstWriteAt 只证明"该机制覆盖的写路径没写过"，**不**证明文件与外部副作用
// 干净（§2.15.1 的同一句）；三维判据仍要逐维成立。
// The probe returns everything the window judges: schema version, the current cutover
// row, the first-write marker and the row-count baseline.
type windowDatabaseState struct {
	SchemaVersion int
	CutoverID     string
	CutoverAt     int64
	FirstWriteAt  *int64
	RowCounts     map[string]int64
}

// windowDatabaseProbe 是窗口里**唯一**碰数据库的动作：只读打开 + 上述读数。
// 它不迁移、不写业务表；失败即"数据库维度无法证明"，窗口不启动。
// windowDatabaseProbe is the window's only database touch: a read-only open plus those
// readings. It never migrates and never writes.
type windowDatabaseProbe func(ctx context.Context) (windowDatabaseState, error)

// verificationWindow 是窗口的执行体；依赖显式注入，测试因此能在不连 PostgreSQL 的前提下
// 覆盖"离线门失败 ⇒ 端口未开"这条顺序判据。
// verificationWindow holds the injected seams so the ordering can be tested without PostgreSQL.
type verificationWindow struct {
	logger *slog.Logger
	gate   cli.HandoffVerification
	probe  windowDatabaseProbe
	now    func() time.Time
}

// loggerOr / clock 保证零值可用（与其余端口适配器同形）。
// loggerOr and clock keep a zero-valued window usable.
func (w *verificationWindow) loggerOr() *slog.Logger {
	if w.logger == nil {
		return slog.Default()
	}
	return w.logger
}

func (w *verificationWindow) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// Run 跑一次验证窗口，直到 ctx 结束或某一步拒绝。
//
// 顺序是判据的一部分：**离线门 → 只读库探针 → 才监听**。任何一步失败都不打开端口。
// The ordering is the judgement: gate, then probe, then listen. No refusal opens a port.
func (w *verificationWindow) Run(ctx context.Context, opts verificationWindowOptions) (verificationWindowReport, error) {
	start := w.clock()
	report := verificationWindowReport{Workspace: opts.Workspace}

	err := w.run(ctx, opts, &report)
	report.Decision = decideWindowRollback(report.Evidence)
	if err != nil {
		w.loggerOr().Warn("验证窗口：未通过", "workspace", report.Workspace,
			"code", windowRefusalCode(err), "decision", report.Decision,
			"listener_opened", report.ListenerOpened, "error", err, "duration", w.clock().Sub(start))
		return report, err
	}
	w.loggerOr().Info("验证窗口：结束", "workspace", report.Workspace,
		"decision", report.Decision, "schema_version", report.SchemaVersion,
		"assets_checked", report.AssetsChecked, "duration", w.clock().Sub(start))
	return report, nil
}

// run 是窗口主体：只负责拒绝，走到底就是"窗口正常开过又正常停下"。
// run refuses at the first unmet criterion; falling through is a normal window.
func (w *verificationWindow) run(ctx context.Context, opts verificationWindowOptions, report *verificationWindowReport) error {
	if err := w.offlineGate(ctx, opts, report); err != nil {
		return err
	}
	report.GateReady = true

	// 基线快照取在**离线门之后**：门自己会只读打开 SQLite，那一步允许留下 `-shm` 之类的
	// 痕迹；窗口要判的是"从这里开始有没有再动工作区"。
	// The baseline is taken after the gate: the gate's own read-only SQLite open may leave
	// lock artifacts, and the window only judges what happens from here on.
	before, snapshotErr := dbmigrate.CaptureWorkspaceTree(opts.Workspace, ".")
	if snapshotErr != nil {
		report.Evidence.WorkspaceUnprovable = true
		w.loggerOr().Warn("验证窗口：工作区基线快照失败——工作区维度将按无法证明处理",
			"workspace", opts.Workspace, "error", snapshotErr)
	}

	var start windowDatabaseState
	if err := w.probeDatabaseStart(ctx, report, &start); err != nil {
		return err
	}

	if err := w.serve(ctx, opts, report); err != nil {
		return err
	}

	// 数据库维度的第二次读数紧跟在窗口结束后：`first_write_at` 在窗口内转为非空、
	// 或某张表行数变化，都在这里变成 `blocked:database_written`。它**不**返回拒绝
	// 错误——端口已经开过，能做的只有如实记录（结论由 Run 统一投影）。
	w.finishDatabase(ctx, report, start)

	after, afterErr := dbmigrate.CaptureWorkspaceTree(opts.Workspace, ".")
	switch {
	case snapshotErr != nil || afterErr != nil:
		report.Evidence.WorkspaceUnprovable = true
		if afterErr != nil {
			w.loggerOr().Warn("验证窗口：结束快照失败——工作区维度按无法证明处理",
				"workspace", opts.Workspace, "error", afterErr)
		}
	default:
		report.Evidence.WorkspaceChanged = !maps.Equal(before, after)
		if report.Evidence.WorkspaceChanged {
			w.loggerOr().Warn("验证窗口：工作区文件在窗口内发生了变化",
				"workspace", opts.Workspace, "changed", changedPaths(before, after))
		}
	}
	return nil
}

// offlineGate 跑离线门：身份 / 密钥 / 全部密文 / 配置（T142 的同一实现）与资产（可选）。
// offlineGate runs the offline gate: identity, key, every ciphertext and the configuration
// through the very verifier T142 delivered, plus the asset manifest when one is supplied.
func (w *verificationWindow) offlineGate(ctx context.Context, opts verificationWindowOptions, report *verificationWindowReport) error {
	if w.gate == nil {
		report.GateCode = windowCodeProbeMissing
		report.GateDetail = "验证窗口缺少离线门端口——装配缺陷"
		return refuseOffline(windowCodeProbeMissing, 2, "验证窗口：%s", report.GateDetail)
	}
	gate, gateErr := w.gate.VerifyHandoff(ctx, cli.HandoffVerifyRequest{Dir: opts.Workspace})
	report.OfflineGate = gate
	if gateErr != nil {
		report.GateCode = gate.Code
		report.GateDetail = gate.Detail
		return gateErr
	}

	packageDir := strings.TrimSpace(opts.AssetPackage)
	if packageDir == "" {
		// 导出包可在切换前删除（§2.12.0b 步骤 5），所以缺省不阻断；但**不静默通过**：
		// 报告与日志都写"未核对"，读的人才不会把没查当查过。
		// A missing package does not block, but the report never claims the assets were checked.
		report.AssetsNote = "未给出 " + verificationAssetPackageFlag +
			"——资产维度本次**未核对**（导出包可在切换前删除，给出时才会逐项重盘）"
		w.loggerOr().Warn("验证窗口：资产维度未核对", "reason", report.AssetsNote)
		return nil
	}
	manifest, err := dbmigrate.ReadAssetManifest(packageDir)
	if err != nil {
		report.AssetsNote = fmt.Sprintf("读资产清单失败: %v", err)
		return refuseOffline(windowCodeAssetsMismatch, 1, "验证窗口：%s", report.AssetsNote)
	}
	assets, err := dbmigrate.VerifyAssets(ctx, opts.Workspace, manifest)
	if err != nil {
		report.AssetsNote = fmt.Sprintf("重盘工作区失败: %v", err)
		return refuseOffline(windowCodeAssetsMismatch, 1, "验证窗口：%s", report.AssetsNote)
	}
	report.Assets = assets
	if problems := assetProblems(assets); len(problems) > 0 {
		report.AssetsNote = strings.Join(problems, "；")
		return refuseOffline(windowCodeAssetsMismatch, 1, "验证窗口：工作区资产与清单不符：%s", report.AssetsNote)
	}
	report.AssetsChecked = true
	report.AssetsNote = fmt.Sprintf("工作区资产与清单逐项一致（%d 项）", len(manifest.Entries))
	return nil
}

// probeDatabaseStart 在监听之前读一次库，把读数存作窗口基线。四种情况分开处理：
//
//  1. 探针缺席 → 装配缺陷（退出码 2）；
//  2. 读失败 / 结果未知 → 数据库维度无法证明（退出码 1）；
//  3. 没有切换行 → 这个窗口不属于"切换后的实例"（退出码 1，稳定码单独给）；
//  4. **`first_write_at` 已非空** → 回退窗口已关闭，只允许前滚（C-13）→ 拒绝启动。
//
// 第 4 条是 S-F01（2026-10-31 裁决第 1 条）的核心：在这条判据落地之前，窗口从不读
// `first_write_at`，一个已经越过回退点的实例照样能开出一个报 "ready" 的窗口，报告里的
// 数据库维度于是永远是"干净"——这正是"配了但不生效"的形态。
// probeDatabaseStart refuses before listening when the marker is missing or the rollback
// window is already closed.
func (w *verificationWindow) probeDatabaseStart(ctx context.Context, report *verificationWindowReport, start *windowDatabaseState) error {
	if w.probe == nil {
		report.Evidence.DatabaseUnprovable = true
		report.DatabaseNote = "验证窗口缺少只读库探针——装配缺陷，不启动 serve"
		return refuseOffline(windowCodeProbeMissing, 2, "验证窗口：%s", report.DatabaseNote)
	}
	state, err := w.probe(ctx)
	if err != nil {
		report.Evidence.DatabaseUnprovable = true
		if errors.Is(err, dbmigrate.ErrNoCutoverMarker) {
			report.DatabaseNote = fmt.Sprintf("目标库没有切换记录（storage_cutover 无行）: %v", err)
			return refuseOffline(windowCodeCutoverMissing, 1,
				"验证窗口：%s——本窗口只用于切换后的实例", report.DatabaseNote)
		}
		report.DatabaseNote = fmt.Sprintf("只读库探针失败: %v", err)
		return refuseOffline(windowCodeDatabaseMissing, 1, "验证窗口：%s", report.DatabaseNote)
	}
	report.DatabaseChecked = true
	report.SchemaVersion = state.SchemaVersion
	report.CutoverID = state.CutoverID
	report.FirstWriteAt = state.FirstWriteAt
	*start = state

	if state.FirstWriteAt != nil {
		report.Evidence.DatabaseWritten = true
		report.DatabaseNote = fmt.Sprintf(
			"只读打开成功，schema_version=%d，cutover_id=%s；但 first_write_at=%d 非空——"+
				"回退窗口已关闭，只允许前滚（C-13），验证窗口不启动",
			state.SchemaVersion, state.CutoverID, *state.FirstWriteAt)
		return refuseOffline(windowCodeDatabaseWritten, 1, "验证窗口：%s", report.DatabaseNote)
	}

	report.DatabaseNote = fmt.Sprintf(
		"只读打开成功，schema_version=%d，cutover_id=%s，first_write_at 为空"+
			"（该标记只覆盖经 MarkFirstWrite 的写路径），行数基线 %d 张表（不迁移、不写业务表）",
		state.SchemaVersion, state.CutoverID, len(state.RowCounts))
	return nil
}

// finishDatabase 在窗口结束后重读一次库并记录变化。三类变化各自落到不同的三值：
//
//   - 重读失败 → 无法证明（读失败与"提交结果未知"同一处置：不知道就不算干净）；
//   - 切换行换了 / `storage_cutover` 行数变了 → 无法归因（可能是 C-48 ① 的维护写入，
//     但本次窗口的证据随之作废，转人工）；
//   - `first_write_at` 转为非空，或其它表行数变化 → 已确认写入。
//
// "提交结果未知"在这里闭合：窗口开始读到空标记、结束后读到的状态就是数据库的最终
// 裁决——落库则标记非空或行数增加（判脏），未落库则两份读数一致（标记覆盖的路径
// 确实没写）。仍在漂移中的事务由重读失败分支兜住。
// finishDatabase re-reads after the window; it records rather than refuses.
func (w *verificationWindow) finishDatabase(ctx context.Context, report *verificationWindowReport, start windowDatabaseState) {
	if w.probe == nil {
		// 走不到：probeDatabaseStart 已经在同一 nil 探针上拒绝过。
		report.Evidence.DatabaseUnprovable = true
		return
	}
	// 重读**不得**继承 serve 的取消：停机信号恰恰是它的触发条件，继承取消会把每一次
	// 正常停机都记成"无法证明"，`allowed` 永远拿不到。脱掉取消、保留追踪，并加独立
	// 上限——超时落到与读失败同一处置（C-34：不知道就不算干净）。
	// The re-read is triggered BY the cancellation, so it must not inherit it.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verificationWindowFinishProbeTimeout)
	defer cancel()
	end, err := w.probe(probeCtx)
	if err != nil {
		report.Evidence.DatabaseUnprovable = true
		report.DatabaseFindings = append(report.DatabaseFindings,
			fmt.Sprintf("窗口结束重读数据库失败——数据库维度无法证明: %v", err))
		w.loggerOr().Warn("验证窗口：结束重读数据库失败——数据库维度按无法证明处理", "error", err)
		return
	}

	if end.CutoverID != start.CutoverID || end.CutoverAt != start.CutoverAt {
		report.Evidence.DatabaseUnprovable = true
		report.DatabaseFindings = append(report.DatabaseFindings,
			fmt.Sprintf("切换行在窗口内发生变化（%s → %s）——起止读数不可比，转人工核查",
				start.CutoverID, end.CutoverID))
		w.loggerOr().Warn("验证窗口：切换行在窗口内发生变化",
			"before", start.CutoverID, "after", end.CutoverID)
	}

	if end.FirstWriteAt != nil {
		report.Evidence.DatabaseWritten = true
		report.DatabaseFindings = append(report.DatabaseFindings,
			fmt.Sprintf("窗口内 first_write_at 由空转为 %d——首个业务写入已提交，回退窗口已关闭", *end.FirstWriteAt))
		w.loggerOr().Warn("验证窗口：窗口内出现首个业务写入", "first_write_at", *end.FirstWriteAt)
	}

	for _, delta := range dbmigrate.CompareRowCounts(start.RowCounts, end.RowCounts) {
		if delta.Table == "storage_cutover" {
			// 直接落进"无法归因"，不重复算业务写入：新切换行属 C-48 ① 维护写入，
			// 但它同样推翻本次窗口的起止可比性。
			report.Evidence.DatabaseUnprovable = true
			report.DatabaseFindings = append(report.DatabaseFindings,
				fmt.Sprintf("表 storage_cutover 在窗口内变化 %+d 行——切换事实被改动，无法归因，转人工核查", delta.Delta))
			continue
		}
		report.Evidence.DatabaseWritten = true
		report.DatabaseFindings = append(report.DatabaseFindings,
			fmt.Sprintf("表 %s 在窗口内变化 %+d 行——窗口内出现了写入", delta.Table, delta.Delta))
	}
	if len(report.DatabaseFindings) > 0 {
		w.loggerOr().Warn("验证窗口：数据库维度在窗口内发生变化",
			"findings", report.DatabaseFindings)
	}
}

// serve 起**只回环**的 health / ready 面并阻塞到 ctx 结束。
// serve opens the loopback-only health/ready face and blocks until the context ends.
func (w *verificationWindow) serve(ctx context.Context, opts verificationWindowOptions, report *verificationWindowReport) error {
	addr := strings.TrimSpace(opts.Addr)
	if addr == "" {
		addr = verificationWindowDefaultAddr
	}
	listener, err := listenVerificationWindow(addr)
	if err != nil {
		return err
	}
	// 只有真的 listen 成功才置真——离线门失败时这里根本走不到，判据因此可断言。
	report.ListenerOpened = true
	report.ListenerAddr = listener.Addr().String()

	server := &http.Server{
		Handler:           verificationWindowMux(report),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	w.loggerOr().Info("验证窗口：health / ready 已就绪（仅回环）",
		"addr", report.ListenerAddr, "schema_version", report.SchemaVersion)

	if opts.OnReady != nil {
		opts.OnReady(report.ListenerAddr)
	}

	select {
	case <-ctx.Done():
		// 停机用不受取消影响的上下文：信号已经取消 runCtx，宽限期还要跑完（约束 20）。
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if shutErr := server.Shutdown(shutdownCtx); shutErr != nil {
			return fmt.Errorf("验证窗口：停机失败: %w", shutErr)
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("验证窗口：服务失败: %w", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("验证窗口：服务失败: %w", err)
		}
		return nil
	}
}

// listenVerificationWindow 只接受回环地址：验证窗口不得对外暴露，配错即拒绝而不是"照样监听"。
// listenVerificationWindow accepts loopback literals only; a non-loopback address refuses.
func listenVerificationWindow(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, refuseOffline(windowCodeListenerRejected, 2,
			"验证窗口：监听地址 %q 不是 host:port 形态: %v", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, refuseOffline(windowCodeListenerRejected, 2,
			"验证窗口：只监听回环地址（127.0.0.0/8 或 ::1），拒绝 %q——验证窗口不得对外暴露", addr)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("验证窗口：监听 %s 失败: %w", addr, err)
	}
	return listener, nil
}

// verificationWindowMux 只有两条路由：产品既有的 `/api/v1/health` 与窗口专用的
// `/api/v1/ready`。没有任何业务端点——窗口不做业务。
// The window exposes the product's health route plus one ready route; no business endpoint.
func verificationWindowMux(report *verificationWindowReport) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(out http.ResponseWriter, _ *http.Request) {
		writeWindowJSON(out, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/api/v1/ready", func(out http.ResponseWriter, _ *http.Request) {
		writeWindowJSON(out, http.StatusOK, map[string]any{
			"status":              "ready",
			"verification_window": true,
			"schema_version":      report.SchemaVersion,
			"assets_checked":      report.AssetsChecked,
			"decision_pending":    true,
		})
	})
	return mux
}

// writeWindowJSON 写一条最小 JSON 响应；编码失败只记一次 WARN（响应头已经发出，改不了状态码）。
// writeWindowJSON emits a minimal JSON body; a late encode failure can only be logged.
func writeWindowJSON(out http.ResponseWriter, status int, payload map[string]any) {
	out.Header().Set("Content-Type", "application/json; charset=utf-8")
	out.WriteHeader(status)
	if err := json.NewEncoder(out).Encode(payload); err != nil {
		slog.Default().Warn("验证窗口：写响应失败", "error", err)
	}
}

// changedPaths 列出两次快照之间新增 / 变更 / 删除的相对路径（报告只写路径，不写内容）。
// changedPaths lists what differs between the two snapshots.
func changedPaths(before, after dbmigrate.WorkspaceSnapshot) []string {
	paths := make([]string, 0, len(after))
	for path, sum := range after {
		if before[path] != sum {
			paths = append(paths, path)
		}
	}
	for path := range before {
		if _, kept := after[path]; !kept {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths
}

// windowRefusalCode 取错误的稳定码（无码时给空串）——日志字段用。
// windowRefusalCode extracts the stable code for the log line.
func windowRefusalCode(err error) string {
	var refusal *offlineRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

// parseVerificationWindowArgs 从 argv 认出 `serve --verification-window [--asset-package <dir>]`。
//
// matched=false 表示这次调用不是验证窗口，参数原样留给根命令；matched=true 且 err 非空
// 表示窗口参数本身不成立（按前置不满足给退出码 2）。`--asset-package` 单独出现也算
// matched：它只在窗口下有意义，静默忽略会让操作者以为资产被核过。
// parseVerificationWindowArgs recognises the window invocation; a lone --asset-package is
// a usage error rather than a silently ignored flag.
func parseVerificationWindowArgs(args []string) (verificationWindowOptions, bool, error) {
	name, ok := commandName(args)
	if !ok || name != "serve" {
		return verificationWindowOptions{}, false, nil
	}
	var opts verificationWindowOptions
	enabled := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == verificationWindowFlag:
			enabled = true
		case strings.HasPrefix(arg, verificationAssetPackageFlag+"="):
			opts.AssetPackage = strings.TrimSpace(strings.TrimPrefix(arg, verificationAssetPackageFlag+"="))
		case arg == verificationAssetPackageFlag:
			if i+1 >= len(args) {
				return opts, true, cli.NewExitError(2,
					"serve %s：缺少导出包目录（用法：%s <dir>）", verificationAssetPackageFlag, verificationAssetPackageFlag)
			}
			i++
			opts.AssetPackage = strings.TrimSpace(args[i])
		}
	}
	if !enabled {
		if opts.AssetPackage != "" {
			return opts, true, cli.NewExitError(2,
				"serve %s 只在 %s 下有效——普通 serve 不核对资产",
				verificationAssetPackageFlag, verificationWindowFlag)
		}
		return verificationWindowOptions{}, false, nil
	}
	if strings.HasPrefix(opts.AssetPackage, "-") {
		return opts, true, cli.NewExitError(2,
			"serve %s：%q 看起来是另一个选项，不是导出包目录", verificationAssetPackageFlag, opts.AssetPackage)
	}
	return opts, true, nil
}

// windowDatabaseProbe 用轻路径的**只读**打开读窗口需要的全部数据库事实（不迁移、
// 不取锁、不恢复）：schema 版本、当前切换行（含 first_write_at）与行数基线。
// The probe reuses the light path's read-only open and reads every fact the window judges.
func (a *App) windowDatabaseProbe(ctx context.Context) (windowDatabaseState, error) {
	db, err := a.readStore(ctx)
	if err != nil {
		return windowDatabaseState{}, err
	}
	state, probeErr := readWindowDatabaseState(ctx, db)
	if closeErr := db.Close(); closeErr != nil {
		probeErr = errors.Join(probeErr, closeErr)
	}
	return state, probeErr
}

// readWindowDatabaseState 是探针的可测内核：给一个已打开的句柄，返回窗口读数。
// 顺序是判据的一部分——先判 schema 版本（连错了库就停），再读切换行（没切换过就停），
// 最后才做逐表行数清点（最贵的一步，前面已经拒绝时不做）。
// readWindowDatabaseState is the probe's testable core: version, cutover row, then counts.
func readWindowDatabaseState(ctx context.Context, db *postgres.DB) (windowDatabaseState, error) {
	version, err := db.SchemaVersion(ctx)
	if err != nil {
		return windowDatabaseState{}, err
	}
	record, err := dbmigrate.RequireCutover(ctx, db)
	if err != nil {
		return windowDatabaseState{}, err
	}
	counts, err := dbmigrate.SnapshotRowCounts(ctx, db)
	if err != nil {
		return windowDatabaseState{}, err
	}
	return windowDatabaseState{
		SchemaVersion: version,
		CutoverID:     record.CutoverID,
		CutoverAt:     record.CutoverAt,
		FirstWriteAt:  record.FirstWriteAt,
		RowCounts:     counts,
	}, nil
}

// runVerificationWindowMode 是 `serve --verification-window` 的进程面：读一次配置只为
// 日志档位与只读 DSN，然后派生唯一信号上下文并跑窗口。
// runVerificationWindowMode loads the configuration once for the log profile and the
// read-only DSN, derives the single signal context, then runs the window.
func runVerificationWindowMode(ctx context.Context, workDir string, opts verificationWindowOptions) int {
	configPath := filepath.Join(workDir, configFileName)
	cfg, cfgErr := cli.LoadConfig(configPath, os.LookupEnv)
	logger := bootstrapLogger(cfg, cfgErr, configPath)

	a := &App{
		workDir:          workDir,
		supportedVersion: postgres.SupportedSchemaVersion,
		logger:           logger,
		cfg:              cfg,
		cfgErr:           cfgErr,
	}
	window := &verificationWindow{
		logger: logger,
		gate:   handoffVerifyPort{Logger: logger},
		probe:  a.windowDatabaseProbe,
		now:    time.Now,
	}

	runCtx, stopSignals := a.signalContext(ctx)
	defer stopSignals()

	report, err := window.Run(runCtx, opts)
	if err != nil {
		logger.Error("验证窗口未通过", "code", windowRefusalCode(err),
			"decision", report.Decision, "error", err)
		return cli.ExitStatusOf(err)
	}
	logger.Info("验证窗口：serve 已在验证姿态下正常停止",
		"listener", report.ListenerAddr, "decision", report.Decision)
	return 0
}
