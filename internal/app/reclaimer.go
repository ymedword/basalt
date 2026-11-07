package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// 本文件是**回收器**（US-24 T017 / 模块卡 §2.3.5 图 5）：把"已失去合法执行者"的
// 三类资源按既有恢复规则收尾，另做一轮无租约的幂等残留对账。它是一段固定有界的
// 维护循环（条数上限 + 单轮超时），独立于 cron，**不执行业务推进、不调模型、不调工具**
// （FR-24-17 / C-12）。
//
// 四条边界：
//
//  1. **只碰合法领取的租约**。领取谓词是 `expires_at <= db_ms OR released_at IS NOT NULL`
//     加业务行未终态；心跳、启动时间、"看起来很久"都**不是**死亡判据（C-06）。
//  2. **A 类顺序固定** run → schedule_execution → flow_root：先收回 Run，执行行才能读到
//     它的真实终态；最后处理根时子树输入已经稳定。
//  3. **回收器不判 Flow 根的状态**。A3 只取证（三个判据）→ 调 `flow.Service.Recover`
//     → 按"答复是否确定"决定释放还是放弃；`Queued` / `Blocked` 由服务层算。
//  4. **错误分档**：单条资源收尾失败只计数 + WARN（下一轮自然重试），领取与残留对账
//     这类基础设施失败才上抛——启动期首轮失败即启动失败（图 1 fail-closed）。
//
// The reclaimer closes only what was legitimately claimed: expired or released leases over
// non-terminal business rows. It never decides a Flow status, and per-resource failures are
// counted rather than fatal.

// reclaimRoundTimeout 是单轮回收的**总预算**（FR-24-17 的"超时"一半）。它有界而非
// 恒定值：一轮扫得再慢也必须结束，否则下一拍会和它叠在同一进程里，回收从"有界维护"
// 退化成"永远在跑的第二个执行路径"。超时只中止**本轮**，不取消进程。
// reclaimRoundTimeout bounds one pass; a slow pass ends instead of stacking on the next tick.
const reclaimRoundTimeout = 30 * time.Second

// 接管 Flow 根时写进 OperationID / Reason 的固定字面量。它们进管理操作台账，
// 运维据此把"这条根是被回收器接管的"与正常推进、管理面 resume 分开。
// Frozen literals stamped by the flow-root takeover.
const (
	reclaimOperationPrefix = "reclaim:"
	reclaimResumeReason    = "worker_lease_expired"
)

// boundedReworkReason 是允许"再等一轮"的 blocked 原因（`worker.go` 的 rework 路径）。
// 它**不是** A3 该接住的形态：那条路径由 Flow Worker 自己重新排队，A3 插手只会把
// 一次重做判定改写成"接管"。
// boundedReworkReason belongs to the rework path, not to takeover.
const boundedReworkReason = "bounded_rework_required"

// reclaimerDeps 是回收器的装配输入。全部显式传入，不用包级变量或迟绑定。
// reclaimerDeps is the explicit assembly input of the reclaimer.
type reclaimerDeps struct {
	DB       *postgres.DB
	Flows    *flow.Service
	Leases   core.LeaseStore
	Worker   core.WorkerIdentity
	Logger   *slog.Logger
	Now      func() time.Time
	LeaseTTL time.Duration
	// Learning 是 B3 孤儿抽取查证的选取与写入端口；与启动恢复共用同一个仓储实例，
	// 因此两处走的是同一对函数（P-18），不会出现第二套判定。
	// Learning is the B3 orphan-extraction port; it is the same repo the startup scan uses.
	Learning memory.LearningAdminStore
	// Diagnostics 是 A1 未重放动作的诊断事件出口（§2.3.5 A1 的"写诊断事件"）。
	// nil = 不写事件：诊断可丢弃、可关闭，回收语义一字不依赖它。
	// Diagnostics is the A1 event outlet; nil simply records nothing.
	Diagnostics diagnosticRecorder
}

// leaseReclaimer 实现 `core.Reclaimer`。
//
// 它持有 `ReclaimerStore`（领取面 + 残留对账）、`LeaseStore`（交还）与 `flow.Service`
// （唯一允许判 Flow 状态的地方）。没有 `SubtreeGate` 字段：A3 用
// `postgres.RecoverFlowRoot` 在**一个快照**里同时取到三个判据，再调本服务——分四次
// 独立查询会读到自相矛盾的中间态（§2.3.6）。
// leaseReclaimer implements core.Reclaimer over the storage primitives and the flow service.
type leaseReclaimer struct {
	db          *postgres.DB
	store       *postgres.ReclaimerStore
	flows       *flow.Service
	leases      core.LeaseStore
	worker      core.WorkerIdentity
	logger      *slog.Logger
	now         func() time.Time
	ttl         time.Duration
	learning    memory.LearningAdminStore
	diagnostics diagnosticRecorder
}

// newLeaseReclaimer 校验装配输入。缺任何一项都是装配缺陷——回收器要么完整参与协调，
// 要么不装配；半套装配会让"领到了却交还不了"变成静默的租约泄漏。
// newLeaseReclaimer refuses a partial assembly: a reclaimer must be able to release.
func newLeaseReclaimer(deps reclaimerDeps) (*leaseReclaimer, error) {
	if deps.DB == nil {
		return nil, fmt.Errorf("回收器装配缺少数据库句柄")
	}
	if deps.Flows == nil {
		return nil, fmt.Errorf("回收器装配缺少 Flow 服务（A3 的状态只能由它决定）")
	}
	if deps.Leases == nil {
		return nil, fmt.Errorf("回收器装配缺少租约端口（无法交还领取的租约）")
	}
	if deps.Worker.InstanceID == "" {
		return nil, fmt.Errorf("回收器装配缺少 Worker 实例标识")
	}
	if deps.LeaseTTL <= 0 {
		return nil, fmt.Errorf("回收器装配的执行权有效期必须为正（收到 %s）", deps.LeaseTTL)
	}
	if deps.Learning == nil {
		return nil, fmt.Errorf("回收器装配缺少学习抽取查证端口（B3 无法查证孤儿行）")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &leaseReclaimer{
		db:          deps.DB,
		store:       postgres.NewReclaimerStore(deps.DB),
		flows:       deps.Flows,
		leases:      deps.Leases,
		worker:      deps.Worker,
		logger:      logger,
		now:         now,
		ttl:         deps.LeaseTTL,
		learning:    deps.Learning,
		diagnostics: deps.Diagnostics,
	}, nil
}

// ReclaimOnce 跑一轮：A1 → A2 → A3 → B1 → B2 → B3。
//
// 返回的错误只表示**本轮没跑完**（领取或残留对账这类基础设施失败）；单条资源收尾
// 失败进 `Errors` 并记 WARN——它不该拖垮整个循环，下一轮领取会自然重试（租约仍未
// 释放则在下一轮再被领到）。
// ReclaimOnce runs one pass; the error reports an incomplete pass, not a failed resource.
func (r *leaseReclaimer) ReclaimOnce(ctx context.Context, limit int) (core.ReclaimReport, error) {
	var report core.ReclaimReport
	if limit <= 0 {
		return report, fmt.Errorf("回收轮次的条数上限必须为正（收到 %d）", limit)
	}

	runs, audits, runErrs, err := r.reclaimRuns(ctx, limit)
	if err != nil {
		return report, err
	}
	report.Runs, report.PendingAudit = runs, audits
	report.Errors += runErrs

	executions, execErrs, err := r.reclaimExecutions(ctx, limit)
	if err != nil {
		return report, err
	}
	report.Executions = executions
	report.Errors += execErrs

	roots, rootErrs, err := r.reclaimFlowRoots(ctx, limit)
	if err != nil {
		return report, err
	}
	report.FlowRoots = roots
	report.Errors += rootErrs

	residue, err := r.store.ReclaimTerminalResidue(ctx, limit, r.now())
	if err != nil {
		return report, fmt.Errorf("终态残留对账: %w", err)
	}
	report.PendingAudit += residue.Unknowns
	if residue.Runs > 0 || residue.RefsReleased > 0 || residue.DeliveriesSettled > 0 {
		// 正常路径下这四项都是 0；>0 说明上一次收尾在"终态已写、引用 / 投递未结算"
		// 之间断过。它不是缺陷，但必须有人看得见（否则"总有一点点残留"会被当成常态）。
		r.logger.Warn("回收器修复了终态残留",
			"reason", "terminal_residue", "runs", residue.Runs,
			"unknowns", residue.Unknowns, "refs_released", residue.RefsReleased,
			"deliveries_settled", residue.DeliveriesSettled)
	}

	// B2：Run 创建前预写、属主已失联的 pending 投递（模块卡 §2.3.5 B2）。它不领租约，
	// 靠 `execution_id` 对应的执行租约证明所有权；没有 Run 行 ⇒ 从未尝试投递 ⇒ skipped。
	// 顺序放在 A 类之后：存在执行租约的那条会先被 A2 合法收尾并释放，再轮到 B2 判。
	orphanDeliveries, err := postgres.SettleOrphanDeliveries(ctx, r.db, r.now(), limit)
	if err != nil {
		return report, fmt.Errorf("运行创建前投递结算: %w", err)
	}
	if orphanDeliveries > 0 {
		r.logger.Warn("回收器结算了运行创建前的投递",
			"reason", "pre_run_delivery", "deliveries_settled", orphanDeliveries)
	}

	// B3：孤儿学习抽取（模块卡 §2.3.5 B3 / FR-24-39）。只查证、不重放；它与启动恢复
	// 共用 `verifyPendingExtractions` 与 `RecoverExtraction`，区别只在选取入口。放在 A 类
	// 之后：Run 已经收尾（`runs.status` 稳定），扫描到的孤儿才是真的无人收尾。
	// 进程不重启也能解阻——这正是 B3 存在的理由。
	// B3 verifies orphan extractions so a crashed owner's row unblocks without a restart.
	orphanExtractions, err := r.learning.OrphanPendingExtractions(ctx, limit)
	if err != nil {
		return report, fmt.Errorf("孤儿学习抽取扫描: %w", err)
	}
	if len(orphanExtractions) > 0 {
		result, err := verifyPendingExtractions(ctx, r.learning, orphanExtractions, r.now().UTC())
		if err != nil {
			return report, fmt.Errorf("孤儿学习抽取查证: %w", err)
		}
		// 正常路径下没有孤儿；>0 说明有属主失联留下的 pending 行，必须有人看得见。
		if result.Verified > 0 {
			r.logger.Warn("回收器查证了孤儿学习抽取",
				"reason", "orphan_extraction", "verified", result.Verified,
				"skipped", result.Skipped, "success", result.Success, "unknown", result.Unknown)
		}
	}
	return report, nil
}

// reclaimRuns 是 A1：领取过期 / 已释放的 run 租约，逐条按作用域收尾。
// 返回"真收尾的 Run 数"、"归一化为 unknown 的审计行数"与单条失败数。
// reclaimRuns is A1: claim expired run leases and close each in its own scope.
func (r *leaseReclaimer) reclaimRuns(ctx context.Context, limit int) (int, int, int, error) {
	leases, err := r.store.ClaimExpired(ctx, core.LeaseRun, r.worker, limit, r.ttl)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("领取过期 Run 租约: %w", err)
	}
	var closed, audits, failed int
	for _, lease := range leases {
		part, err := postgres.RecoverRun(ctx, r.db, r.logger, lease.LeaseToken, r.now())
		if err != nil {
			// 整笔回滚：Run 保持 running、租约保持未释放，到期后下一轮再领再试。
			reportReclaimError(r.logger, "Run", lease.ResourceID, lease.OwnershipEpoch, err)
			failed++
			continue
		}
		closed += part.ClosedRuns
		audits += part.Unknowns
		// A1 的"写诊断事件"：事务已提交，现在把每条**未重放**的动作挂回 Run 时间线。
		// 放在提交之后是判据的一部分——回滚的接管不得留下"已恢复"的证据。
		r.recordUnreplayedActions(ctx, lease.ResourceID, part.Unreplayed)
	}
	return closed, audits, failed, nil
}

// recordUnreplayedActions 为本次接管未重放的每条审计行写一条诊断事件（§2.3.5 A1）。
//
// 事件形状与运行期生产者同口径：`llm_call` 事件用轮号作 `step_id`，`tool_call` 事件
// 把工具名与调用标识放进 attrs；`audit_ref` 逐行回指审计表 + 主键。归属两项
// （`worker_instance_id` / `ownership_epoch`）由诊断存储按租约行回填——本进程刚领取过
// 这条租约，回填值是本次接管的代次，不是旧属主的。
//
// 三条边界：
//   - **只在提交之后写**：事务回滚时不留"已恢复"的半截证据；
//   - **失败不改变回收结论**：`Recorder.Record` 不返回错误，诊断可丢弃；
//   - **不重放**：事件只作解释，绝不表示再次发起模型或工具调用。
//
// The events annotate, never replay, and are written only after the A1 transaction
// commits; lease attribution is backfilled by the diagnostics store from the lease row.
func (r *leaseReclaimer) recordUnreplayedActions(ctx context.Context, runID string, actions []postgres.UnreplayedAction) {
	if r.diagnostics == nil || runID == "" || len(actions) == 0 {
		return
	}
	at := r.now().UnixMilli()
	for _, action := range actions {
		ev := diagnostics.DiagnosticEvent{
			Correlation: diagnostics.Correlation{RunID: runID},
			OccurredAt:  at,
			Actor:       diagnostics.ActorRecovery,
			ReasonCode:  diagnostics.ReasonRecoveryUnknown,
			AuditRef: &diagnostics.AuditRef{
				Table: action.Table,
				ID:    strconv.FormatInt(action.ID, 10),
			},
			Attrs: map[string]any{},
		}
		switch action.Table {
		case postgres.UnreplayedTableLLMCalls:
			ev.Kind = diagnostics.KindLLMCall
			if action.Round > 0 {
				ev.StepID = strconv.FormatInt(action.Round, 10)
			}
		case postgres.UnreplayedTableToolInvocations:
			ev.Kind = diagnostics.KindToolCall
			if action.ToolName != "" {
				ev.Attrs["tool"] = action.ToolName
			}
			if action.ToolCallID != "" {
				ev.Attrs["tool_call_id"] = action.ToolCallID
			}
		default:
			// 归一化只写这两张表；未知表名是代码与 SQL 漂移。事件丢弃并留痕，
			// 不把没见过的表名塞进契约枚举（诊断可丢弃，误报更糟）。
			r.logger.Warn("恢复诊断：未知审计表，事件丢弃",
				"run_id", runID, "table", action.Table)
			continue
		}
		r.diagnostics.Record(ctx, ev)
	}
}

// reclaimExecutions 是 A2：领取过期 / 已释放的执行行租约，把执行行收尾到四值域。
// `RecoverExecution` 返回空状态表示"行已不在 started"（已被合法收尾或根本没开始），
// 那一条不计入——回收器的读数只统计真落盘的收尾。
// reclaimExecutions is A2: close execution rows whose lease expired.
func (r *leaseReclaimer) reclaimExecutions(ctx context.Context, limit int) (int, int, error) {
	leases, err := r.store.ClaimExpired(ctx, core.LeaseScheduleExecution, r.worker, limit, r.ttl)
	if err != nil {
		return 0, 0, fmt.Errorf("领取过期执行行租约: %w", err)
	}
	var closed, failed int
	for _, lease := range leases {
		status, err := postgres.RecoverExecution(ctx, r.db, lease.LeaseToken, r.now())
		if err != nil {
			reportReclaimError(r.logger, "schedule_execution", lease.ResourceID, lease.OwnershipEpoch, err)
			failed++
			continue
		}
		if status != "" {
			closed++
		}
	}
	return closed, failed, nil
}

// reclaimFlowRoots 是 A3：领取过期 / 已释放的 flow_root 租约，取证后交 `Service.Recover`
// 查证。**A3 自己不推进、不判状态**（§2.3.5 A3）。
// reclaimFlowRoots is A3: gather evidence and let the flow service reconcile.
func (r *leaseReclaimer) reclaimFlowRoots(ctx context.Context, limit int) (int, int, error) {
	leases, err := r.store.ClaimExpired(ctx, core.LeaseFlowRoot, r.worker, limit, r.ttl)
	if err != nil {
		return 0, 0, fmt.Errorf("领取过期 Flow 根租约: %w", err)
	}
	var reconciled, failed int
	for _, lease := range leases {
		if err := r.reclaimFlowRoot(ctx, lease); err != nil {
			reportReclaimError(r.logger, "flow_root", lease.ResourceID, lease.OwnershipEpoch, err)
			failed++
			continue
		}
		reconciled++
	}
	return reconciled, failed, nil
}

// reclaimFlowRoot 处理一条已领取的 Flow 根租约。返回值只表示"这一条处理完了"；
// 是否改写了根由 `Service.Recover` 决定。
//
// 分路逐字对应模块卡 §2.3.5 A3：
//
//	门 ① 非 Isolated          → 交还（有人仍能提交，先 Recover 会打断在跑的子树）
//	无事可做                   → 交还（queued / running 归 ReadyRoots；rework 归 Flow Worker）
//	Recover 成功               → 交还（Queued 由 ReadyRoots 拾起；Blocked 由下一轮 A3 自己接住）
//	ErrActionUnknown           → 交还（结论是"有未决动作"，它是持久状态，不用租约记住）
//	ErrTaskClosed / ErrDeadline → 交还（关闭意图已落库，sweepClosures 是推进者）
//	其余错误                    → **不交还**（"根本没查完"：保住 TookOver=true 这条信号）
//
// The branches mirror the card exactly; only "no answer" keeps the lease, so the next round
// still sees TookOver=true instead of mistaking an unfinished takeover for a clean handoff.
func (r *leaseReclaimer) reclaimFlowRoot(ctx context.Context, lease core.Lease) error {
	// 三个判据必须来自**同一快照**：分开查会读到"没人持有租约却还有在途 attempt"这种
	// 自相矛盾的中间态，据此下的结论不可复现。
	evidence, err := postgres.RecoverFlowRoot(ctx, r.db, lease.ResourceID)
	if err != nil {
		return fmt.Errorf("读取 Flow 根恢复输入 %s: %w", lease.ResourceID, err)
	}
	if !evidence.Isolated {
		return r.releaseFlowRoot(ctx, lease)
	}
	if evidence.Joined &&
		(evidence.Task.Status != flow.Blocked || evidence.Task.Reason == boundedReworkReason) {
		return r.releaseFlowRoot(ctx, lease)
	}
	principal, err := flow.StoredPrincipal(evidence.Task)
	if err != nil {
		// 契约与任务行不一致说明归属被改写过，不能拿一个重建出来的身份去推进别人的根。
		// 不交还：交还会让下一轮看起来像正常交棒。
		return fmt.Errorf("重建 Flow 根 %s 的原授权身份: %w", evidence.Task.ID, err)
	}
	task, err := r.flows.Recover(ctx, flow.ResumeRequest{
		Principal: principal,
		TaskID:    evidence.Task.ID,
		// Expected 必须传当前版本：A3 不猜版本，重建它会触发 ErrPreconditionFailed。
		Expected:    evidence.Task.Version,
		Lease:       lease.LeaseToken,
		OperationID: reclaimOperationID(evidence.Task.ID, lease.OwnershipEpoch),
		Reason:      reclaimResumeReason,
	})
	if err != nil {
		switch {
		case errors.Is(err, flow.ErrActionUnknown):
			// 答复是确定的：子树里有未决动作。释放后 A 类谓词仍命中它
			// （released_at IS NOT NULL），下一轮还会再判一次——保护不依赖租约。
			return r.releaseFlowRoot(ctx, lease)
		case errors.Is(err, flow.ErrTaskClosed), errors.Is(err, flow.ErrDeadline):
			// 领取之后、Recover 之前被并发关闭 / 过 deadline：关闭意图已持久，
			// 推进者是 sweepClosures，不需要 TookOver 信号。
			return r.releaseFlowRoot(ctx, lease)
		default:
			return fmt.Errorf("接管 Flow 根 %s: %w", evidence.Task.ID, err)
		}
	}
	r.logger.Info("接管 Flow 根完成", "task_id", task.ID, "status", string(task.Status),
		"reason", task.Reason, "ownership_epoch", lease.OwnershipEpoch)
	return r.releaseFlowRoot(ctx, lease)
}

// releaseFlowRoot 交还一条已领取的根租约。交还失败是基础设施错误（上抛 → 计数），
// 但已完成的 Recover 不会被回滚：下一轮最多再查证一次，`FindRecovery` 幂等。
// releaseFlowRoot hands the claimed root lease back.
func (r *leaseReclaimer) releaseFlowRoot(ctx context.Context, lease core.Lease) error {
	if err := r.leases.Release(ctx, lease.LeaseToken); err != nil {
		return fmt.Errorf("交还 Flow 根租约 %s: %w", lease.ResourceID, err)
	}
	return nil
}

// reclaimOperationID 生成接管操作的幂等键：同一根、同一代次只对应一次接管。
// 更换代次（再次被领取）才是新的一次操作。
// reclaimOperationID keys one takeover by root and ownership epoch.
func reclaimOperationID(taskID string, epoch int64) string {
	return fmt.Sprintf("%s%s:%d", reclaimOperationPrefix, taskID, epoch)
}

// reportReclaimError 记录单条资源的收尾失败。WARN 而不是 ERROR：这是回收器的常态
// 分支（下一轮会重试），ERROR 会把它误报成需要人工介入的故障。
// reportReclaimError logs a per-resource failure; the next round retries it.
func reportReclaimError(logger *slog.Logger, kind, resourceID string, epoch int64, err error) {
	logger.Warn("回收单条资源失败（下一轮重试）",
		"lease_kind", kind, "resource_id", resourceID,
		"ownership_epoch", epoch, "error", err)
}

// StartReclaimer 启动回收循环：每 interval 跑一轮 `ReclaimOnce`，每轮独立超时。
// 循环与心跳分开：心跳只写诊断行，回收会改业务行，两者的失败语义因此不同。
// StartReclaimer runs the bounded maintenance loop independently of cron.
func (r *workerRuntime) StartReclaimer(ctx context.Context, interval time.Duration, limit int) {
	if r == nil || r.reclaimer == nil {
		return
	}
	if interval <= 0 {
		interval = defaultReclaimInterval
	}
	if limit <= 0 {
		limit = defaultReclaimBatch
	}
	reclaimCtx, cancel := context.WithCancel(ctx)
	r.reclaimCancel = cancel
	r.reclaimDone = make(chan struct{})

	go func() {
		defer close(r.reclaimDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-reclaimCtx.Done():
				return
			case <-ticker.C:
				roundCtx, roundCancel := context.WithTimeout(reclaimCtx, reclaimRoundTimeout)
				report, err := r.reclaimer.ReclaimOnce(roundCtx, limit)
				roundCancel()
				// 停机导致的取消不是回收失败：父 ctx 已结束就不再刷 WARN。
				if err != nil {
					if reclaimCtx.Err() == nil {
						r.logger.Warn("回收轮次未完成（有界维护循环继续）",
							"instance_id", r.worker.InstanceID, "error", err)
					}
					continue
				}
				if report.Runs > 0 || report.Executions > 0 || report.FlowRoots > 0 ||
					report.PendingAudit > 0 {
					r.logger.Info("回收轮次完成", "instance_id", r.worker.InstanceID,
						"runs", report.Runs, "executions", report.Executions,
						"flow_roots", report.FlowRoots, "pending_audit", report.PendingAudit,
						"errors", report.Errors)
				}
			}
		}
	}()
}

// 回收循环在缺省配置缺失时的兜底节奏（正常路径的缺省值来自 `cli.DefaultWorkerConfig`，
// 这里只是让零值不再意味着"疯狂空转"或"从不运行"）。
const (
	defaultReclaimInterval = 5 * time.Second
	defaultReclaimBatch    = 32
)
