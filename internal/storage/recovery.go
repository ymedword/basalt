package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是崩溃恢复扫描（docs/us/us4-react-loop.md §2.3 流程 G 六步里的 ①②③④⑥；
// 步骤 ⑤ 的工作区残留归 internal/workspace）。调用方是**持锁的重命令**，启动时跑一次
// （契约 §4.3.1：只读命令不取锁、不做恢复、不清理锁文件）。
// This file is the crash-recovery scan (flow G steps ①-④ and ⑥); the locked heavy path
// invokes it once at startup.
//
// 三条纪律。① **组看消息、Run 看表**：半截组的判据只落在 `messages` 上，且判组**按位置**
// （同一会话内 assistant 之后紧邻的 tool 段），**不**按 `tool_call_id` 跨 Run 查——跨 Run 的
// 同 id 会把"这次没执行"误配成"上次执行过的结果"。② **同一短事务**：一个 Run 的组修复与
// 其收尾要么一起提交、要么一起回滚——契约明写"Run 已翻 failed 而组仍半截"的中间态不可能
// 持久化。③ **只改仍未结束的行**：`WHERE status = 'pending'` 与 `WHERE status = 'running'`
// 是两条语句的全部意义，恢复可能在任何时刻重跑，幂等性靠它们而不是靠"只调一次"。
// Three rules: a purely positional group criterion over messages, one short transaction per
// run covering repair and closure, and a status guard on every statement so a rerun is a no-op.
//
// 冻结签名（§3.1）收的是裸 `*sql.DB`——本文件因此不复用需要 `*DB` 句柄的仓储方法，改为直接
// 对连接执行；判据与仓储逐字同源，差别只在收尾面（按 run_id 收窄）与执行面（事务连接）。
// **消息写入是例外**：`appendMessage`（`session_store.go`）按执行面取参，两处共用同一份
// 语句——`seq` 分配与计数列这两条纪律不允许有第二个版本。
// The frozen signature hands recovery a bare *sql.DB, so the statements here mirror the
// repositories' criteria instead of reusing methods that need the *DB handle. Message
// writes are the deliberate exception: both paths share the one INSERT statement.

// recoveryCloseReason 是崩溃收尾写进 `runs.reason` 的逐字文本（契约 §4.3.2 步骤 ③④）。
// The verbatim reason stamped on crash closure.
const recoveryCloseReason = "进程中断（启动时发现未完成的运行）"

// RecoveryReport 是恢复扫描的三个读数（契约 §4.3.2 步骤 ⑥ 的启动摘要）。
// RecoveryReport carries the three counters of the startup summary.
type RecoveryReport struct {
	// Unknowns 是 ① 归一化为 `unknown` 的审计行数（`llm_calls` 与 `tool_invocations` 合计）。
	Unknowns int

	// RepairedGroups 是被补齐的**半截组数**（按 assistant 消息计，不按缺失的调用计）。
	RepairedGroups int

	// ClosedRuns 是被收尾为 `failed` 的运行数（含没有任何半截组、纯靠账本判定者）。
	ClosedRuns int
}

// Recover 执行一次崩溃恢复扫描。返回的 error 是**尽力而为**的汇总（`errors.Join`）：
// 某个 Run 修复失败不影响其余 Run，但启动方必须看见它（据此决定要不要开跑）。
// Recover performs one crash-recovery scan; the error aggregates per-run failures without
// hiding any of them from the caller.
func Recover(ctx context.Context, db *sql.DB, logger *slog.Logger, now time.Time) (RecoveryReport, error) {
	var report RecoveryReport
	if logger == nil {
		// 冻结签名收 `*slog.Logger`；调用方图省事传 nil 不该让恢复崩在日志上。
		logger = slog.Default()
	}

	// ① `pending` → `unknown`（两表）。放在最前：③ 的分类要读审计行，而"还在 pending"
	// 这个假象一旦开机就不该再留在库里。
	unknowns, err := normalizePendingAuditRows(ctx, db)
	if err != nil {
		return report, err
	}
	report.Unknowns = unknowns

	// ② 半截组扫描（只读，按会话 + seq 走一遍消息）。
	groups, err := scanHalfGroups(ctx, db)
	if err != nil {
		return report, err
	}

	// ③ 每个含半截组的 Run 一个短事务。**失败也要记账为"已处理"**：它留着 running 等下次
	// 启动重扫，不能被 ④ 顺手收尾（否则就成了"Run 已翻 failed 而组仍半截"——契约禁止的中间态）。
	byRun, runOrder := halfGroupsByRun(groups)
	handled := make(map[string]struct{}, len(runOrder))
	var errs []error
	for _, runID := range runOrder {
		handled[runID] = struct{}{}
		repaired, closed, err := repairRunGroups(ctx, db, logger, runID, byRun[runID], now)
		if err != nil {
			errs = append(errs, err)
			logger.Error("崩溃恢复：半截组修复失败（该 Run 保持 running，下次启动重扫）",
				"run_id", runID, "error", err)
			continue
		}
		report.RepairedGroups += repaired
		if closed {
			report.ClosedRuns++
		}
	}

	// ④ 另扫 runs：仍为 running 且**不在** ③ 里的行，同样收尾。这一类包含"一条消息都没写就
	// 崩在首次模型响应上""最终回答已落库但终态没写""无状态 invoke"——它们的共同点是：
	// 消息侧看不出任何异常，只有账本知道上次没跑完。
	// 每个 Run 一个短事务：收尾与资源引用释放一起提交（约束 19）——分开写会留下
	// "Run 已终态、版本却仍被引用"的中间态。
	runningIDs, err := listRunningRunIDs(ctx, db)
	if err != nil {
		return report, errors.Join(append(errs, err)...)
	}
	for _, runID := range runningIDs {
		if _, done := handled[runID]; done {
			continue
		}
		closed, err := closeRunInTransaction(ctx, db, runID, now)
		if err != nil {
			errs = append(errs, err)
			logger.Error("崩溃恢复：运行收尾失败（该 Run 保持 running，下次启动重扫）",
				"run_id", runID, "error", err)
			continue
		}
		if closed {
			report.ClosedRuns++
		}
	}

	// ⑤ 终态 Run 的残留引用补齐（约束 19 末句 / 契约 DDL 注释）：它们早已不是 running，
	// ④ 的扫描面看不到；一条幂等语句按"Run 已终态"补齐。正常路径（FinishRun / ③ / ④）
	// 都是同事务释放，这一步只兜版本间残留与人工修库留下的行——**不**补造任何历史。
	if err := releaseTerminalRefs(ctx, db, now); err != nil {
		errs = append(errs, err)
		logger.Error("崩溃恢复：终态运行的残留资源引用补齐失败", "error", err)
	}

	// ⑥ 启动摘要。三键与返回值里的三个读数**同源**（一处算、两处读），日志与报告的差异
	// 因此只可能是"有人看的是另一次运行"，不会出现口径差。
	logger.Info("崩溃恢复扫描完成",
		"unknowns", report.Unknowns,
		"repaired_groups", report.RepairedGroups,
		"closed_runs", report.ClosedRuns)

	return report, errors.Join(errs...)
}

// Recover 是机制层 Recover 的 *DB 形态（**装配面**）：持锁重路径在取锁之后、开跑之前
// 调用它（US-4 起），此后机制函数的直接调用方只在本包内。
// DB.Recover is the assembly-facing form of the recovery scan: the locked heavy path
// calls it between taking the lock and serving, and it is the only production caller.
//
// 为什么不把写池交出去：`DB.write` 与迁移用的嵌入源都不出包——写权、SQL 文本与
// schema.sql 的读法由本包保管，不交给调用方自觉（与"只读句柄干脆不承载写路径"同一
// 纪律，见 db.go 的句柄边界说明）。只读句柄（write 为 nil）在这里**显式拒绝**，而不是
// 让它崩在一句 nil 解引用上。
// The write pool stays inside this package: the handle carries the boundary. A
// read-only handle is refused explicitly instead of panicking on a nil pool.
func (db *DB) Recover(ctx context.Context, logger *slog.Logger, now time.Time) (RecoveryReport, error) {
	if db == nil || db.write == nil {
		return RecoveryReport{}, errors.New("storage: 只读句柄没有写池，不得做恢复扫描（先经 OpenWritable 取得写模式句柄）")
	}
	return Recover(ctx, db.write, logger, now)
}

// normalizePendingAuditRows 把两张审计表里仍为 `pending` 的行归一化为 `unknown` 并写
// `error_kind = 'unknown'`（契约 §4.3.2 步骤 ①）。`finished_at` 与 `duration_ms` 保持 NULL：
// 这两列说的都是"进程没走到那一步"，编一个结束时刻或时长比留空更误导。
// Tables are normalized in one transaction so the two audit surfaces never disagree about
// "what was still in flight when the process died".
func normalizePendingAuditRows(ctx context.Context, db *sql.DB) (int, error) {
	var total int
	err := withWriteLock(ctx, db, func(ctx context.Context, conn *sql.Conn) error {
		total = 0
		updated, err := normalizePendingRows(ctx, conn,
			`UPDATE llm_calls SET status = ?, error_kind = ? WHERE status = ?`)
		if err != nil {
			return fmt.Errorf("归一化 llm_calls 的 pending 行: %w", err)
		}
		total += updated

		updated, err = normalizePendingRows(ctx, conn,
			`UPDATE tool_invocations SET status = ?, error_kind = ? WHERE status = ?`)
		if err != nil {
			return fmt.Errorf("归一化 tool_invocations 的 pending 行: %w", err)
		}
		total += updated
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// normalizePendingRows 执行一条归一化语句并返回影响行数（两条语句共用，参数相同）。
// Runs one normalization statement and reports the affected row count.
func normalizePendingRows(ctx context.Context, ex sqlExecutor, stmt string) (int, error) {
	res, err := ex.ExecContext(ctx, stmt,
		string(core.LLMCallUnknown), string(core.ErrorKindUnknown), string(core.LLMCallPending))
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// halfGroup 是一个"半截工具组"：某条 assistant 消息声明了工具调用，紧随其后的 tool 消息段
// 没能把这组调用答全。`seq` 只用于日志点名，判组与补齐都不看它。
// halfGroup is one incomplete tool group; seq is carried only for log attribution.
type halfGroup struct {
	sessionID string
	runID     string
	seq       int64
	missing   []core.ToolCall
}

// scanHalfGroups 扫出全部半截组（契约 §4.3.2 步骤 ②）。判组**按位置**：同一个会话内按 seq
// 顺序走，遇到 `role=assistant` 且 `tool_calls` 非空就开一个组，随后**紧邻**的 `role=tool`
// 消息段就是这组的答卷；中间出现任何别的角色即封组。**不得**按 `tool_call_id` 全会话查——
// 跨 Run 的同 id 会互相误配（判卷用例 `TestRecovery_RepairsHalfGroupByPosition`）。
// Groups are located positionally inside each session; the shared judgement itself comes from
// core.MissingToolResults, so the crash path and the cancel path always agree.
func scanHalfGroups(ctx context.Context, db *sql.DB) ([]halfGroup, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT session_id, COALESCE(run_id, ''), seq, role, COALESCE(tool_calls, ''),
		        COALESCE(tool_call_id, '')
		   FROM messages
		  WHERE session_id IS NOT NULL
		  ORDER BY session_id, seq`)
	if err != nil {
		return nil, fmt.Errorf("扫描半截工具组: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		groups     []halfGroup
		sessionID  string
		open       bool
		calls      []core.ToolCall
		window     []core.Message
		groupRunID string
		groupSeq   int64
	)

	// flush 结算当前组：缺了结果的才算半截组，完整的组连计数都不进（`RepairedGroups`
	// 只数"真补过"的组）。
	flush := func() {
		if !open {
			return
		}
		if missing := core.MissingToolResults(calls, window); len(missing) > 0 {
			groups = append(groups, halfGroup{
				sessionID: sessionID, runID: groupRunID, seq: groupSeq, missing: missing,
			})
		}
		open, calls, window = false, nil, nil
	}

	for rows.Next() {
		var (
			rowSession, role, toolCalls, toolCallID, rowRunID string
			seq                                               int64
		)
		if err := rows.Scan(&rowSession, &rowRunID, &seq, &role, &toolCalls, &toolCallID); err != nil {
			return nil, fmt.Errorf("读消息行: %w", err)
		}
		if rowSession != sessionID {
			flush() // 会话边界即组边界：跨会话的 tool 行不属于任何组
			sessionID = rowSession
		}

		switch role {
		case core.RoleAssistant:
			flush() // 上一条 assistant 到此封组（下一个 assistant 就是新的一组）
			parsed, err := parseToolCalls(toolCalls)
			if err != nil {
				return nil, fmt.Errorf("解析 messages.tool_calls（session=%s, seq=%d）: %w", rowSession, seq, err)
			}
			if len(parsed) > 0 {
				open, calls, window, groupRunID, groupSeq = true, parsed, nil, rowRunID, seq
			}
		case core.RoleTool:
			// 只有紧邻的 tool 段算组内答卷；open 为假说明这条 tool 行前面隔着别的角色，
			// 它答的是更早的组（或根本没有组）——按位置判组就不该把它借给谁。
			if open {
				window = append(window, core.Message{Role: core.RoleTool, ToolCallID: toolCallID})
			}
		default:
			flush()
		}
	}
	flush()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描半截工具组: %w", err)
	}
	return groups, nil
}

// parseToolCalls 解析 `messages.tool_calls` 列（schema 注释里的形态：`[{id,name,arguments}]`）。
// 解不开**不猜**：这一列只由本仓写入，解不开就是真损坏；按"没有工具调用"跳过会让一个半截组
// 永远补不上，而按"猜一个组"补齐会往历史里塞从未发生过的消息——两者都比停下更糟。
// A malformed column aborts the scan: skipping it would silently leave a group unrepaired.
func parseToolCalls(raw string) ([]core.ToolCall, error) {
	if raw == "" {
		return nil, nil
	}
	var calls []core.ToolCall
	if err := json.Unmarshal([]byte(raw), &calls); err != nil {
		return nil, fmt.Errorf("decode tool_calls: %w", err)
	}
	return calls, nil
}

// halfGroupsByRun 把半截组按 Run 归并，并返回**首次出现序**（扫描序）——每个 Run 一个事务，
// 处理的先后因此与消息顺序一致，判卷与日志都可复现。
// Groups are bucketed per run in first-appearance order so each run gets exactly one
// transaction and the processing order stays reproducible.
func halfGroupsByRun(groups []halfGroup) (map[string][]halfGroup, []string) {
	byRun := make(map[string][]halfGroup, len(groups))
	order := make([]string, 0, len(groups))
	for _, g := range groups {
		if _, seen := byRun[g.runID]; !seen {
			order = append(order, g.runID)
		}
		byRun[g.runID] = append(byRun[g.runID], g)
	}
	return byRun, order
}

// repairRunGroups 在**一个短事务**里补齐该 Run 的全部半截组、并把仍为 `running` 的它收尾
// （契约 §4.3.2 步骤 ③）。返回的读数是"这一趟真落了盘"的数量——事务回滚时两者都为零。
// Repairs every half group of one run and closes it if still running, all inside a single
// short transaction; a rolled-back transaction reports zero on both counters.
func repairRunGroups(ctx context.Context, db *sql.DB, logger *slog.Logger, runID string,
	groups []halfGroup, now time.Time,
) (int, bool, error) {
	var (
		repairedGroups int
		closed         bool
	)
	err := withWriteLock(ctx, db, func(ctx context.Context, conn *sql.Conn) error {
		repairedGroups = 0
		closed = false

		// 分类要看该 Run **当前**的状态：cancelled → "已取消…"、limit_reached → "未执行
		// （工具调用次数超限）"、其余（含仍 running 与已终结的其它态）→ "未执行（进程中断）"。
		status, found, err := lookupRunStatus(ctx, conn, runID)
		if err != nil {
			return err
		}
		reason := recoveryReasonFor(status)

		for _, g := range groups {
			if !found {
				// 契约 §4.3.2 步骤 ③ 的明文：run_id 为空或找不到 runs 行时**仍然补组**——
				// 历史里少一条 tool 结果会让整段会话对供应商不可用（400），比"给一个查不到
				// Run 的组留白"严重得多；但这件事要有人看见，故 WARN 点名 run_id 与会话。
				logger.Warn("崩溃恢复：半截组找不到对应的运行行（仍按进程中断补齐）",
					"run_id", runID, "session_id", g.sessionID, "seq", g.seq)
			}
			for _, call := range g.missing {
				content, err := core.SynthesizeMissingResult(ctx, recoveryAuditReader{conn: conn}, runID, call, reason)
				if err != nil {
					// 查审计失败**不**写兜底文本（与取消路径的差别在时机）：这里还是开机，
					// 整笔回滚、该 Run 留 running，下次启动会重扫一遍——不必在拿不准时落笔。
					return fmt.Errorf("补齐半截工具组（session=%s, seq=%d, tool_call_id=%s）: %w",
						g.sessionID, g.seq, call.ID, err)
				}
				// 写入与正常运行路径**共用同一份语句**（含 `seq` 分配与计数列递增）——
				// 两处各写一份同义 SQL 就会有第二处漂移点。
				m := core.Message{Role: core.RoleTool, Content: content, ToolCallID: call.ID}
				if err := appendMessage(ctx, conn, g.sessionID, g.runID, m, now); err != nil {
					return err
				}
			}
			repairedGroups++
		}

		closedNow, err := closeAndReleaseRun(ctx, conn, runID, now)
		if err != nil {
			return err
		}
		closed = closedNow
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return repairedGroups, closed, nil
}

// lookupRunStatus 读该 Run 当前的状态（分类用）。行不存在返回 found=false——**不是错误**：
// 半截组的 run_id 可能为空，也可能指向一个没有账本行的 Run（契约 §4.3.2 步骤 ③）。
// A missing ledger row is not an error here: it is one of the documented cases.
func lookupRunStatus(ctx context.Context, q queryer, runID string) (string, bool, error) {
	var status string
	err := q.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id = ?`, runID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读运行状态（run_id=%s）: %w", runID, err)
	}
	return status, true, nil
}

// recoveryReasonFor 把 Run 的当前状态映射为补齐分类的原因（契约 §4.4 五分支）。
// 只影响"**无**审计行"的兜底文本：有终态行就还原真实结果，与原因无关。
// Maps the run state onto the completion reason; it only shapes the no-audit-row fallback.
func recoveryReasonFor(status string) string {
	switch core.RunStatus(status) {
	case core.StatusCancelled:
		return core.ReasonCancelled
	case core.StatusLimitReached:
		return core.ReasonToolBudget
	default:
		return core.ReasonRunFailed
	}
}

// closeAndReleaseRun 收尾**指定的、仍为 `running`** 的行，并在**同一事务**释放它的资源
// 引用（步骤 ③④，约束 19 / 注 8）。两个动作必须一起提交：只关不加锁引用会让版本永远
// 显示"被引用"，只释放不关行则是把 Running 的账静默变成"没有引用"。
// 已终态的行不会被覆盖（`status = 'running'` 守卫），但它的残留引用照样释放——这正是
// "终态 Run 残留补齐"在单 Run 面上的落点；行根本不存在时释放是 0 行 no-op。
// closeAndReleaseRun closes the run and releases its references in the caller's
// transaction; an already-terminal row keeps its status but still gets its residual
// references released.
//
// `ex` 收执行面而非库句柄：步骤 ③ 在组修复的短事务里调用，步骤 ④ 由
// closeRunInTransaction 开事务；两步的事务边界都在调用方，本函数不自开事务。
func closeAndReleaseRun(ctx context.Context, ex sqlExecutor, runID string, now time.Time) (bool, error) {
	closed, err := closeRunningRun(ctx, ex, runID, now)
	if err != nil {
		return false, err
	}
	if _, err := releaseRunRefs(ctx, ex, runID, now); err != nil {
		return false, err
	}
	return closed, nil
}

// closeRunningRun 把**指定的、仍为 `running`** 的行收尾为 `failed`（步骤 ③④ 的状态转换，
// 引用释放由 closeAndReleaseRun 在同一事务内追加）。`WHERE status = 'running'` 是这条
// 语句的全部意义：恢复扫描可能在任何时刻重跑，幂等靠它。
// closeRunningRun performs the guarded status flip; closeAndReleaseRun adds the release.
func closeRunningRun(ctx context.Context, ex sqlExecutor, runID string, now time.Time) (bool, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE runs
		    SET status = ?, reason = ?, finished_at = ?
		  WHERE run_id = ? AND status = ?`,
		string(core.StatusFailed), recoveryCloseReason, now.UnixMilli(), runID, string(core.StatusRunning))
	if err != nil {
		return false, fmt.Errorf("收尾仍为 running 的运行行（run_id=%s）: %w", runID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("确认运行收尾（run_id=%s）: %w", runID, err)
	}
	return affected > 0, nil
}

// closeRunInTransaction 为步骤 ④ 的单个 Run 开一个短事务，把收尾与引用释放一起提交
// （约束 19）。步骤 ③ 已在组修复事务里，不能重复开事务——两条路径因此共用
// closeAndReleaseRun，事务边界各自给。
// closeRunInTransaction wraps closeAndReleaseRun in one short transaction for step ④;
// step ③ already sits inside the group-repair transaction.
func closeRunInTransaction(ctx context.Context, db *sql.DB, runID string, now time.Time) (bool, error) {
	var closed bool
	err := withWriteLock(ctx, db, func(ctx context.Context, conn *sql.Conn) error {
		var err error
		closed, err = closeAndReleaseRun(ctx, conn, runID, now)
		return err
	})
	if err != nil {
		return false, err
	}
	return closed, nil
}

// releaseTerminalRefs 补齐**已终态** Run 仍挂着的未释放引用（约束 19 末句）：步骤 ④ 只
// 扫得到 running 行，而版本间残留 / 人工修库留下的行可能早就不是 running 了。一条语句
// 按"Run 已终态"收口，幂等；**不**碰 running 行，也**不**补造清单或历史。
// releaseTerminalRefs releases residual references on already-terminal runs; running rows
// are untouched and nothing historical is fabricated.
func releaseTerminalRefs(ctx context.Context, db *sql.DB, now time.Time) error {
	return withWriteLock(ctx, db, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx,
			`UPDATE run_resource_refs
			    SET released_at = ?
			  WHERE released_at IS NULL
			    AND run_id IN (SELECT run_id FROM runs WHERE status <> ?)`,
			now.UnixMilli(), string(core.StatusRunning))
		if err != nil {
			return fmt.Errorf("补齐终态运行的残留资源引用: %w", err)
		}
		return nil
	})
}

// listRunningRunIDs 扫出仍为 `running` 的 run_id（步骤 ④ 的取数面——"组看消息、Run 看表"
// 里的表侧判据）。与 `RunStore.listRunning` 同一条判据、不同返回面：恢复只要 id，
// 那具方法要 `*DB` 句柄而冻结签名只有裸 `*sql.DB`。
// The table-side scan of step ④; it only needs the ids, unlike RunStore.listRunning.
func listRunningRunIDs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT run_id FROM runs WHERE status = ? ORDER BY started_at, run_id`, string(core.StatusRunning))
	if err != nil {
		return nil, fmt.Errorf("扫描仍为 running 的运行行: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读仍为 running 的运行行: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描仍为 running 的运行行: %w", err)
	}
	return ids, nil
}

// recoveryAuditReader 是 `core.ToolInvocationReader` 在**恢复事务内**的适配器：判据与
// `ToolInvocationRepo.Lookup` **共用同一份实现**（`lookupToolInvocation`），只是执行面换成
// 事务连接。两条硬道理：① 写池恒一条连接（SQLite 单写者），事务持着它时再向池要连接就是
// 自己等自己；② 查询与写入同处一个快照，同一个 Run 的判组与补齐不会看到互相矛盾的两份数据。
// The reader shares the repository's criterion but runs on the transaction connection:
// asking the single-connection write pool for another connection while the transaction
// holds it would deadlock.
type recoveryAuditReader struct{ conn *sql.Conn }

// Lookup 见 `lookupToolInvocation`（本类型只换执行面，不换判据）。
// Lookup delegates to the shared implementation.
func (r recoveryAuditReader) Lookup(ctx context.Context, runID, toolCallID string) (core.ToolInvocationView, bool, error) {
	return lookupToolInvocation(ctx, r.conn, runID, toolCallID)
}
