package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// 本文件是**作用域化的接管收尾**（US-24 / 模块卡 §2.3.5 A1～A3）：只有在回收器
// **合法领取**了某个资源的执行权之后，才对这个资源做收尾。签名因此收 token 与
// 显式的作用域标识，而不是"扫全库找坏的"。
//
// 与 US-4 版的三处差别：
//
//  1. **没有全局扫描**（FR-24-02）：`RecoverRun` 只处理调用方持有的那条 run 租约，
//     审计归一化与半截工具组补齐全按 run_id 过滤（FR-24-16）。库里的 pending 行若
//     没有人持有其租约，只报告不改写——改写它等于替一个可能还活着的 Worker 作结论。
//  2. **整笔一个事务**（§2.3.5 A1 末条）：归一化 → 补组 → 释放引用 → 投递结算 →
//     终态 → 释放租约一起提交。只有接管路径可以这样合并：原属主已失联，学习抽取
//     钩子已经丢失（记 skipped: worker_lost，不重放），"引用留给钩子用"的约束在
//     这里不存在。**正常收尾仍是五步**，不并进一个事务。
//  3. **归属由租约保证**：事务外壳是 `runFencedTx`——先锁租约行、再校验 owner +
//     epoch + 未释放 + 未过期，然后才动业务行。旧持有者即使恢复，它的写入也会在
//     这里被 `ErrLeaseLost` 挡下（FR-24-15 / FR-24-36）。
//
// Three differences from the single-process version: no global scan, one transaction for
// the whole takeover closeout, and ownership guaranteed by the leased fence rather than
// by "we started first".

// recoveryOwnerUnauthorized 是原授权 Key 已失效时写进 `runs.reason` 的固定文案
// （C-32）：与调度侧的 `owner_unauthorized` 同一字面量，运维不必为两条路径记两套。
// recoveryOwnerUnauthorized is the same frozen literal the scheduler uses when the
// authorizing key no longer holds.
const recoveryOwnerUnauthorized = "owner_unauthorized"

// UnreplayedAction 是一条本次接管**没有重放**、结果永远不可知的审计行
// （模块卡 §2.3.5 A1："未完成的工具副作用：不重放，记 unknown 并写诊断事件"）。
//
// 它只描述"哪一行被归一化了"，不携带诊断事件的翻译：事件种类、原因码与脱敏都属于
// 装配层（与 `tool_diagnostics.go` 同一套纪律），存储层不 import 诊断包去做翻译。
// UnreplayedAction names one audit row that was normalized to unknown and will never be
// replayed; translating it into a diagnostic event belongs to the assembly layer.
type UnreplayedAction struct {
	// Table 是承载该动作的审计表；取值只来自下面两个常量。
	Table string

	// ID 是该表的 bigint 主键，诊断的 `audit_ref` 用它回指审计行。
	ID int64

	// Round 是 `llm_calls.round`（工具行为 0）：运行期 `llm_call` 事件用轮号作
	// `step_id`，恢复事件据此与同一轮的原事件对齐；**不**另造全局序号（C-23）。
	Round int64

	// ToolCallID 是 `tool_invocations.tool_call_id`（可空）：工具事件的关联标识。
	ToolCallID string

	// ToolName 是 `tool_invocations.tool_name`：工具事件 attrs 的 `tool` 键。
	ToolName string
}

// 两张被归一化的审计表名（`UnreplayedAction.Table` 的取值域）。
// The two normalized audit tables; these are the only Table values.
const (
	// UnreplayedTableLLMCalls 是模型调用审计表。
	UnreplayedTableLLMCalls = "llm_calls"
	// UnreplayedTableToolInvocations 是工具调用审计表。
	UnreplayedTableToolInvocations = "tool_invocations"
)

// RecoveryReport 是**单个资源**接管收尾的读数（不再是全库扫描的汇总）。
// 四个计数都指"这一趟真落了盘"的数量：事务回滚时全部为零。
// RecoveryReport counts what one takeover closeout actually committed.
type RecoveryReport struct {
	// Unknowns 是 ① 归一化为 `unknown` 的审计行数（`llm_calls` 与 `tool_invocations` 合计）。
	Unknowns int

	// Unreplayed 与 Unknowns 同源：每条被归一化的审计行一项，供装配层逐条写诊断事件。
	// 诊断可丢弃，本报告不依赖它是否写成功；事务回滚时本字段为空。
	Unreplayed []UnreplayedAction

	// RepairedGroups 是被补齐的**半截组数**（按 assistant 消息计，不按缺失的调用计）。
	RepairedGroups int

	// ClosedRuns 是被收尾为 `failed` 的运行数（0 或 1）。
	ClosedRuns int

	// DeliveriesSkipped 是本次收尾判为 `skipped` 的 pending 投递行数。
	// 终态为 failed 的 Run 的投递**永不重发**，只能落 skipped（FR-24-21）。
	DeliveriesSkipped int
}

// RecoverRun 按 A1 收尾**一个已合法领取的 Run**。
//
// 顺序固定（模块卡 §2.3.5 A1）：审计 pending 归一化 → 半截工具组补齐 → 释放三类
// 引用 → 投递结算 → 终态写入 → 释放租约，全部在**同一事务**里。任何一步失败都整笔
// 回滚：Run 保持 running、租约保持"我持有但未释放"，到期后下一轮重新领取重试——
// **不交还**在这里表达"根本没查完"（与 A3 的错误分类同一原则）。
// RecoverRun closes one claimed run in a single transaction; a rollback keeps the lease
// unreleased so the next round retries instead of pretending the work was done.
func RecoverRun(ctx context.Context, db *DB, logger *slog.Logger, token core.LeaseToken,
	now time.Time,
) (RecoveryReport, error) {
	var report RecoveryReport
	if logger == nil {
		logger = slog.Default()
	}
	if db == nil || db.pool == nil {
		return report, errors.New("postgres: 句柄未装配连接池，不得做接管收尾")
	}
	if token.Kind != core.LeaseRun {
		return report, fmt.Errorf("postgres: 接管收尾的执行权类型 %q 不是 run", token.Kind)
	}
	if token.ResourceID == "" {
		return report, fmt.Errorf("postgres: 接管收尾缺少 Run 标识")
	}
	runID := token.ResourceID

	err := runFencedTx(ctx, db, token, nil, func(ctx context.Context, tx *sql.Tx) error {
		report = RecoveryReport{}

		// ① `pending` → `unknown`，**只限本次领取的 run_id**。放在最前：后续的
		// 补组要按审计行还原真实结果，"还在 pending"这个假象必须先消掉。
		unreplayed, err := normalizePendingAuditRows(ctx, tx, []string{runID}, now)
		if err != nil {
			return err
		}
		report.Unknowns = len(unreplayed)
		report.Unreplayed = unreplayed

		// ② 半截组扫描（只读，只扫本次领取的 Run 的消息）。
		groups, err := scanHalfGroups(ctx, tx, []string{runID})
		if err != nil {
			return err
		}

		// ③ 补组：分类要看该 Run **当前**的状态——cancelled → "已取消…"、
		// limit_reached → "未执行（工具调用次数超限）"、其余 → "未执行（进程中断）"。
		status, _, err := lookupRunStatus(ctx, tx, runID)
		if err != nil {
			return err
		}
		reason := recoveryReasonFor(status)
		for _, g := range groups {
			for _, call := range g.missing {
				content, err := core.SynthesizeMissingResult(ctx, recoveryAuditReader{q: tx}, runID, call, reason)
				if err != nil {
					// 查审计失败**不**写兜底文本：整笔回滚、该 Run 留 running，
					// 下一轮接管会重扫——不必在拿不准时落笔。
					return fmt.Errorf("补齐半截工具组（session=%s, seq=%d, tool_call_id=%s）: %w",
						g.sessionID, g.seq, call.ID, err)
				}
				// 写入与正常运行路径**共用同一份语句**（含 `seq` 分配与计数列递增）。
				m := core.Message{Role: core.RoleTool, Content: content, ToolCallID: call.ID}
				if _, err := appendMessage(ctx, tx, g.sessionID, g.runID, m, now); err != nil {
					return err
				}
			}
			report.RepairedGroups++
		}

		// ④ 释放三类引用。与正常收尾 `FinishRun` 同一口径：漏掉通知冻结会让渠道
		// 删除永远点名一个已失败的 Run。
		if _, err := releaseRunRefs(ctx, tx, runID, now); err != nil {
			return err
		}
		if _, err := releaseRunCredentialRefs(ctx, tx, runID, now); err != nil {
			return err
		}
		if _, err := releaseRunNotifyRefs(ctx, tx, runID, now); err != nil {
			return err
		}

		// ⑤ 投递结算：本次收尾为 failed → 该 Run 的 pending 投递置 skipped。
		// **不重发**：投递结果无法证明，重发就是重复通知。
		skipped, err := settleDeliveriesInTx(ctx, tx, runID, core.DeliverySkipped,
			"运行未完成，未投递", now)
		if err != nil {
			return err
		}
		report.DeliveriesSkipped = skipped

		// ⑥ 终态写入。reason 默认 `worker_lost`（FR-24 的机器可读字面量）；
		// 绑定的调用方 Key 已失效时改记 `owner_unauthorized`——同一行的授权已经不在，
		// 这条事实比"进程没了"更该被运维看见，且它是既有验收判据（C-32）。
		closed, err := closeRunningRun(ctx, tx, runID, now)
		if err != nil {
			return err
		}
		if closed {
			report.ClosedRuns = 1
		}

		// ⑦ 释放租约（同一事务）。提交之后"该 Run 无有效租约"，B1 的入选谓词因此
		// 可信："收尾已完成，或属主已经不在了"。
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		return releaseLeaseInTx(ctx, tx, token, nowMS)
	})
	if err != nil {
		return RecoveryReport{}, err
	}
	logger.Info("接管收尾完成", "run_id", runID,
		"ownership_epoch", token.OwnershipEpoch,
		"unknowns", report.Unknowns,
		"repaired_groups", report.RepairedGroups,
		"deliveries_skipped", report.DeliveriesSkipped,
		"closed_runs", report.ClosedRuns)
	return report, nil
}

// RecoverExecution 按 A2 收尾**一个已合法领取的执行行**，把它映射回执行行的四值域
// （模块卡 §2.3.5 A2）。
//
// 映射不是"照抄 Run 状态"：`task_executions.status` 只有 started / completed /
// failed / skipped 四值，没有 limit_reached 或 cancelled。写约束外的值会被 CHECK
// 拒绝，把 Run 状态原样搬过去还会把"Run 达标完成、仅执行行未收尾"记成故障，污染
// 调度成功率。
//
// 读取 run 状态与写入分属两个事务：读是快照，写入的权威判据在 `FinishExecution`
// 内部——它在租约 fence 下再查一次 `status='started'`，0 行按"已终态"幂等返回。
// RecoverExecution maps the run state onto the execution row's four-value domain and
// leaves the authoritative check to FinishExecution's fenced write.
func RecoverExecution(ctx context.Context, db *DB, token core.LeaseToken, now time.Time) (core.ExecutionStatus, error) {
	executionID, err := executionIDOf(token)
	if err != nil {
		return "", err
	}
	store := NewTaskStore(db)
	row, found, err := store.readExecutionForRecovery(ctx, executionID)
	if err != nil {
		return "", err
	}
	if !found || row.status != string(core.ExecutionStarted) {
		// 行已不在 started（已被合法收尾或从未开始）——按幂等处理，不写任何东西。
		return "", nil
	}

	status, message := executionOutcomeFor(row.runStatus)
	duration := time.Duration(0)
	if row.startedAt > 0 {
		duration = now.Sub(time.UnixMilli(row.startedAt))
		if duration < 0 {
			duration = 0
		}
	}
	if err := store.FinishExecution(ctx, executionID, token, status, duration, message, now); err != nil {
		return "", err
	}
	return status, nil
}

// executionRecoveryRow 是 A2 需要的执行行读数（含关联 Run 的状态）。
type executionRecoveryRow struct {
	status    string
	startedAt int64
	runStatus sql.NullString
}

// executionIDOf 从 `schedule_execution` 域的凭证里解出执行行主键。
func executionIDOf(token core.LeaseToken) (int64, error) {
	if token.Kind != core.LeaseScheduleExecution {
		return 0, fmt.Errorf("postgres: 执行行收尾的执行权类型 %q 不是 schedule_execution", token.Kind)
	}
	id, err := parseExecutionResourceID(token.ResourceID)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// executionOutcomeFor 把 Run 状态映射为执行行的四值域（模块卡 §2.3.5 A2）。
//
//	completed / limit_reached → completed（达标完成，不是故障）
//	failed / cancelled        → failed，错误文案取 Run 自己的 reason
//	无 Run / running          → failed + worker_lost（执行者已失联，无从查证）
//
// 无 Run / running 一律带 `worker_lost`：这一支的共同点是"没有任何证据说明这次执行
// 正常结束过"，留空会让故障在审计里与成功难以区分。
// executionOutcomeFor maps the run state onto the execution row's four-value domain.
func executionOutcomeFor(runStatus sql.NullString) (core.ExecutionStatus, string) {
	if !runStatus.Valid {
		return core.ExecutionFailed, workerLostReason
	}
	switch core.RunStatus(runStatus.String) {
	case core.StatusCompleted, core.StatusLimitReached:
		return core.ExecutionCompleted, ""
	case core.StatusFailed, core.StatusCancelled:
		return core.ExecutionFailed, ""
	default:
		// 仍 running（A1 因活租约放过）或未知状态：没有证据说明它正常结束过。
		return core.ExecutionFailed, workerLostReason
	}
}

// FlowRootRecovery 是 A3 恢复所需的**全部输入**（只读快照）。
//
// 三个判据各自独立（模块卡 §2.3.6）：`Isolated` 决定能否 Recover，`Joined` 决定
// Recover 之后落到 Queued 还是 Blocked，`Unresolved` 命中即**不得**推进。
// Three independent predicates; none may be derived from another.
type FlowRootRecovery struct {
	// Task 是根行的当前快照（含 Version——A3 必须把 Expected 传下去，不猜版本）。
	Task flow.Task

	// Isolated 为真表示子树里没有任何**仍有效**的 run 租约：没人还能提交。
	Isolated bool

	// Joined 为真表示子树里没有 prepared / running 的节点尝试。
	Joined bool

	// Unresolved 为真表示子树里还有 outcome ∈ (dispatching, unknown) 的动作。
	Unresolved bool
}

// RecoverFlowRoot 读出 A3 的四项输入，**在一个事务里**取同一快照：分四次独立查询
// 会读到一个既"没人持有租约"又"还有在途 attempt"的中间态，据此下的结论不可复现。
//
// **状态不由这里决定**：唯一的决定者是 `flow.Service.Recover`。本函数只回答"输入是
// 什么"，所以它是只读的——名字里的 Recover 指"为恢复取证"，不指"执行恢复"。
// RecoverFlowRoot only gathers A3's inputs in one snapshot; the only thing allowed to
// decide the resulting status is flow.Service.Recover.
func RecoverFlowRoot(ctx context.Context, db *DB, rootTaskID string) (FlowRootRecovery, error) {
	var out FlowRootRecovery
	if db == nil || db.pool == nil {
		return out, errors.New("postgres: 句柄未装配连接池，不得读取 Flow 根恢复输入")
	}
	if rootTaskID == "" {
		return out, errors.New("postgres: Flow 根恢复输入缺少根任务标识")
	}
	err := db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		task, err := readFlowTask(ctx, tx, rootTaskID)
		if err != nil {
			return err
		}
		if task.RootTaskID != task.ID || task.ParentTaskID != "" || task.Depth != 0 {
			// A3 只服务根行：把子任务当根去 Recover 会推进整棵树的版本。
			return fmt.Errorf("postgres: 任务 %s 不是 Flow 根（root=%q parent=%q depth=%d）",
				rootTaskID, task.RootTaskID, task.ParentTaskID, task.Depth)
		}
		out.Task = task
		live, err := querySubtreeExists(ctx, tx, subtreeLiveRunLeaseQuery, rootTaskID, "HasLiveRunLease")
		if err != nil {
			return err
		}
		out.Isolated = !live
		inflight, err := querySubtreeExists(ctx, tx, subtreeInFlightAttemptQuery, rootTaskID, "HasInFlightNodeAttempt")
		if err != nil {
			return err
		}
		out.Joined = !inflight
		unresolved, err := querySubtreeExists(ctx, tx, subtreeUnresolvedActionQuery, rootTaskID, "HasUnresolvedAction")
		if err != nil {
			return err
		}
		out.Unresolved = unresolved
		return nil
	})
	if err != nil {
		return FlowRootRecovery{}, err
	}
	return out, nil
}

// normalizePendingAuditRows 把两张审计表里仍为 `pending` 的行归一化为 `unknown`，
// 写 `error_kind = 'unknown'`（契约 §4.3.2 步骤 ①）。`finished_at` 与 `duration_ms`
// 保持 NULL：这两列说的都是"进程没走到那一步"，编一个结束时刻或时长比留空更误导。
//
// **必须按 runIDs 过滤**（FR-24-16）：无租约的 pending 行只报告不改写——它可能正被
// 一个还活着的 Worker 持有。
//
// 返回值逐行给出被归一化的动作（表名 + 主键 + 关联列），供 A1 的装配层写诊断事件
// （评审 IDX-28）。**只有这里能拿到准确的 id**：`RowsAffected` 说"改了几行"，却指不出
// 是哪几行，事后重查又会被并发收尾带走。`RETURNING` 让"改了"与"是哪一行"来自同一条
// 语句、同一个事务快照。
// The rows are returned so the assembly layer can annotate each unreplayed action; the
// ids come from the very UPDATE, not from a later query that a concurrent closeout
// could invalidate.
func normalizePendingAuditRows(ctx context.Context, ex sqlExecutor, runIDs []string, now time.Time) ([]UnreplayedAction, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	var out []UnreplayedAction

	llmRows, err := ex.QueryContext(ctx,
		`UPDATE llm_calls SET status = $1, error_kind = $2, recovered_at = $4
		  WHERE status = $3 AND run_id = ANY($5)
		  RETURNING id, round`,
		string(core.LLMCallUnknown), string(core.ErrorKindUnknown),
		string(core.LLMCallPending), now.UnixMilli(), runIDs)
	if err != nil {
		return nil, fmt.Errorf("归一化 %s 的 pending 行: %w", UnreplayedTableLLMCalls, err)
	}
	for llmRows.Next() {
		action := UnreplayedAction{Table: UnreplayedTableLLMCalls}
		if err := llmRows.Scan(&action.ID, &action.Round); err != nil {
			_ = llmRows.Close()
			return nil, fmt.Errorf("读取 %s 的归一化行: %w", UnreplayedTableLLMCalls, err)
		}
		out = append(out, action)
	}
	llmErr := llmRows.Err()
	_ = llmRows.Close()
	if llmErr != nil {
		return nil, fmt.Errorf("归一化 %s 的 pending 行: %w", UnreplayedTableLLMCalls, llmErr)
	}

	toolRows, err := ex.QueryContext(ctx,
		`UPDATE tool_invocations SET status = $1, error_kind = $2, recovered_at = $4
		  WHERE status = $3 AND run_id = ANY($5)
		  RETURNING id, COALESCE(tool_call_id, ''), tool_name`,
		string(core.LLMCallUnknown), string(core.ErrorKindUnknown),
		string(core.LLMCallPending), now.UnixMilli(), runIDs)
	if err != nil {
		return nil, fmt.Errorf("归一化 %s 的 pending 行: %w", UnreplayedTableToolInvocations, err)
	}
	for toolRows.Next() {
		action := UnreplayedAction{Table: UnreplayedTableToolInvocations}
		if err := toolRows.Scan(&action.ID, &action.ToolCallID, &action.ToolName); err != nil {
			_ = toolRows.Close()
			return nil, fmt.Errorf("读取 %s 的归一化行: %w", UnreplayedTableToolInvocations, err)
		}
		out = append(out, action)
	}
	toolErr := toolRows.Err()
	_ = toolRows.Close()
	if toolErr != nil {
		return nil, fmt.Errorf("归一化 %s 的 pending 行: %w", UnreplayedTableToolInvocations, toolErr)
	}
	return out, nil
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

// scanHalfGroups 扫出**指定 Run 集合**内的半截组（契约 §4.3.2 步骤 ②）。
// 判组**按位置**：同一个会话内按 seq 顺序走，遇到 `role=assistant` 且 `tool_calls`
// 非空就开一个组，随后**紧邻**的 `role=tool` 消息段就是这组的答卷；中间出现任何别的
// 角色即封组。**不得**按 `tool_call_id` 全会话查——跨 Run 的同 id 会互相误配
// （判卷用例 `TestRecovery_RepairsHalfGroupByPosition`）。
//
// 作用域过滤落在 `run_id = ANY($1)` 上：只扫本次合法领取的 Run。落在集合外的组
// （包括 run_id 为空的历史行）**不改写**——它们不属于任何我们持有执行权的 Run。
// Groups are located positionally inside each session, restricted to the claimed runs.
func scanHalfGroups(ctx context.Context, ex Queryer, runIDs []string) ([]halfGroup, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	rows, err := ex.QueryContext(ctx,
		`SELECT session_id, COALESCE(run_id, ''), seq, role, COALESCE(tool_calls, ''),
		        COALESCE(tool_call_id, '')
		   FROM messages
		  WHERE session_id IS NOT NULL AND run_id = ANY($1)
		  ORDER BY session_id, seq`, runIDs)
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

// lookupRunStatus 读该 Run 当前的状态（分类用）。行不存在返回 found=false——**不是错误**：
// 半截组的 run_id 可能指向一个没有账本行的 Run（契约 §4.3.2 步骤 ③）。
// A missing ledger row is not an error here: it is one of the documented cases.
func lookupRunStatus(ctx context.Context, q Queryer, runID string) (string, bool, error) {
	var status string
	err := q.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id = $1`, runID).Scan(&status)
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
//
// 注意它与 A2 的 `executionOutcomeFor` **不是同一件事**：本函数产出的是**文案**，
// 那个产出的是执行行的**状态值**。混用会写出约束外的取值（模块卡 §2.3.5 A2 注）。
// Maps the run state onto the completion reason text — not onto an execution status.
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

// closeRunningRun 把**指定的、仍为 `running`** 的行收尾为 `failed`（A1 步骤 ⑥）。
// `WHERE status = 'running'` 是这条语句的全部意义：并发接管与重跑都可能发生，幂等靠它。
//
// 同一条语句做另外两件事（C-32 / §3.4.2）：
//   - `actor` 改写为 `recovery`——执行者与归属分开记，`caller_id` / `authorizing_kind`
//     / `key_id` **一律不动**，恢复**不得**改写归属、**不得**提升成管理员；
//   - caller-key 运行在收尾前复核**那一把**原授权 Key（未吊销、未过期、调用方仍
//     启用、scope 仍覆盖该 Agent）。失效时 reason 记 `owner_unauthorized`——恢复
//     不会自动重放，但"该 Run 的授权已经不在"必须留在同一行上。
//
// closeRunningRun performs the guarded status flip, marks the executor as recovery and
// rechecks the bound caller key in the same statement; the owner columns never change.
func closeRunningRun(ctx context.Context, ex sqlExecutor, runID string, now time.Time) (bool, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE runs AS r
		    SET status = $1,
		        reason = CASE
		            WHEN r.authorizing_kind = $6 AND NOT EXISTS (
		                SELECT 1
		                  FROM caller_keys k
		                  JOIN callers c ON c.caller_id = k.caller_id
		                 WHERE k.key_id = r.key_id
		                   AND k.caller_id = r.caller_id
		                   AND k.revoked_at IS NULL
		                   AND (k.expires_at IS NULL OR k.expires_at > $7)
		                   AND c.status = 'active'
		                   AND k.scopes @> ARRAY['agents:invoke:' || r.agent]
		            ) THEN $8
		            ELSE $2
		        END,
		        actor = $9,
		        finished_at = $3
		  WHERE run_id = $4 AND status = $5`,
		string(core.StatusFailed), workerLostReason, now.UnixMilli(), runID, string(core.StatusRunning),
		string(core.AuthorizingKindCallerKey), now.UnixMilli(),
		recoveryOwnerUnauthorized, string(core.ActorRecovery))
	if err != nil {
		return false, fmt.Errorf("收尾仍为 running 的运行行（run_id=%s）: %w", runID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("确认运行收尾（run_id=%s）: %w", runID, err)
	}
	return affected > 0, nil
}

// releaseLeaseInTx 在调用方的事务里释放租约并记 `released` 事件。
// 0 行即失权（我们已不是该代次的持有者）——整笔回滚，不带着错误认知提交。
// releaseLeaseInTx releases the lease inside the caller's transaction.
func releaseLeaseInTx(ctx context.Context, tx *sql.Tx, token core.LeaseToken, nowMS int64) error {
	res, err := tx.ExecContext(ctx, `
UPDATE execution_leases
   SET released_at = $5
 WHERE kind = $1 AND resource_id = $2 AND owner_worker_id = $3 AND ownership_epoch = $4
   AND released_at IS NULL`,
		string(token.Kind), token.ResourceID, token.OwnerWorkerID, token.OwnershipEpoch, nowMS)
	if err != nil {
		return fmt.Errorf("释放接管租约（%s/%s）: %w", token.Kind, token.ResourceID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认接管租约释放: %w", err)
	}
	if affected != 1 {
		return core.ErrLeaseLost
	}
	return appendLeaseEvent(ctx, tx, token.Kind, token.ResourceID, "released",
		token.OwnerWorkerID, token.OwnershipEpoch, "", sql.NullInt64{}, "接管收尾后释放", nowMS)
}

// recoveryAuditReader 是 `core.ToolInvocationReader` 在**恢复事务内**的适配器：判据与
// `ToolInvocationRepo.Lookup` **共用同一份实现**（`lookupToolInvocation`），只是执行面换成
// 事务。硬道理：查询与写入同处一个快照，同一个 Run 的判组与补齐不会看到互相矛盾的两份数据。
// The reader shares the repository's criterion but runs on the transaction handle, so the
// judgement and the repair see one snapshot.
type recoveryAuditReader struct{ q Queryer }

// Lookup 见 `lookupToolInvocation`（本类型只换执行面，不换判据）。
// Lookup delegates to the shared implementation.
func (r recoveryAuditReader) Lookup(ctx context.Context, runID, toolCallID string) (core.ToolInvocationView, bool, error) {
	return lookupToolInvocation(ctx, r.q, runID, toolCallID)
}
