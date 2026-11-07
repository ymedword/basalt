package flow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// Worker is a fixed bounded pool. Construction starts nothing. Startup must
// reconcile the existing Run ledger and prove the old local process has exited;
// it is an app lifecycle prerequisite, never inferred from an empty Go map.
//
// US-24 起 Worker 自己领取 `flow_root` 租约再推进根：Identity / Leases / Gate /
// LeaseTTL / RenewInterval 是执行权装配面，ReleaseCtx 给出交还租约时的剩余停机
// 预算。缺任一（除 ReleaseCtx 有有界兜底）时 `advanceRoot` fail-closed，不推进。
// Since US-24 the worker acquires the root lease itself; missing execution-right
// fields make advanceRoot fail closed instead of advancing unleased.
type Worker struct {
	Service    *Service
	Repository WorkerRepository
	Identity   core.WorkerIdentity // 根租约的领取者 / who acquires the root lease
	Leases     core.LeaseStore
	// Gate 是 §3.1.8 的三个子树判据（R4 两判据 + R7 门 ③）。@advanceRoot 用它
	// 分走"允许查证"与"允许推进"；**不得**再用单个 Joined 兼顾两者。
	// Gate carries the three subtree criteria; one Joined cannot answer both.
	Gate core.SubtreeGate
	// ReleaseCtx 返回**剩余停机预算**的 ctx（§2.3.11）：
	// min(now + flows.cancel_wait + 5s, signal + worker.shutdown_grace_seconds)。
	// 只用于 KeepLease 的 stop()；实现为注入函数是因为停机截止时刻归 app 的停机
	// 编排所有，Worker 不自己算时间。**不得**退化成 context.WithoutCancel——那会把
	// 预算与 deadline 一并剥掉。
	// ReleaseCtx supplies the remaining shutdown budget for the release step only.
	ReleaseCtx    func() context.Context
	LeaseTTL      time.Duration
	RenewInterval time.Duration
	Startup       func(context.Context) error
	Logger        *slog.Logger
	mu            sync.Mutex
	running       bool
}

type (
	parkedRepository interface {
		ParkTask(context.Context, core.Principal, string, TaskVersion, string) error
	}
	workItem struct {
		task      Task
		principal core.Principal
	}
)

// Run owns exactly MaxActive workers and at most MaxActive active root identities.
// A root is released only after Advance really returns. No per-task goroutine or
// in-memory unbounded queue is created, and shutdown waits for actual joins.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil || w.Service == nil || w.Repository == nil {
		return ErrStateConflict
	}
	c := w.Service.Config
	if !c.Enabled {
		return nil
	}
	if c.MaxActive < 1 || c.MaxActive > 8 || c.PollIntervalMS < 100 || c.PollIntervalMS > 5000 || c.ShutdownGraceSeconds < 1 || c.ShutdownGraceSeconds > 70 || w.Startup == nil {
		return ErrStateConflict
	}
	if err := c.Validate(32, 70*time.Second); err != nil {
		return err
	}
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return ErrStateConflict
	}
	w.running = true
	w.mu.Unlock()
	defer func() { w.mu.Lock(); w.running = false; w.mu.Unlock() }()
	if err := w.Startup(ctx); err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan workItem)
	finished := make(chan string, c.MaxActive)
	var group sync.WaitGroup
	for range c.MaxActive {
		group.Go(func() {
			for item := range jobs {
				var err error
				if item.task.Status == Blocked && item.task.Reason == "bounded_rework_required" {
					_, err = w.Service.rework(workerCtx, item.principal, item.task)
				} else {
					err = w.advanceRoot(workerCtx, item.task)
					if err == nil {
						err = w.reworkIfBounded(workerCtx, item)
					}
				}
				w.handleJobError(workerCtx, item, err)
				finished <- item.task.RootTaskID
			}
		})
	}
	active := make(map[string]bool, c.MaxActive)
	ticker := time.NewTicker(time.Duration(c.PollIntervalMS) * time.Millisecond)
	defer ticker.Stop()
	var runErr error
	for ctx.Err() == nil {
		if err := w.Service.sweepClosures(ctx, w.Repository); err != nil {
			runErr = workerQueryError(ctx, err)
			break
		}
		for draining := true; draining; {
			select {
			case root := <-finished:
				delete(active, root)
			default:
				draining = false
			}
		}
		if len(active) < c.MaxActive {
			ready, err := w.Repository.ReadyRoots(ctx, w.Service.clock().UnixMilli(), 32)
			if err != nil {
				runErr = workerQueryError(ctx, err)
				break
			}
			for _, task := range ready {
				if active[task.RootTaskID] {
					continue
				}
				principal, err := StoredPrincipal(task)
				if err != nil {
					runErr = err
					break
				}
				select {
				case jobs <- workItem{task: task, principal: principal}:
					active[task.RootTaskID] = true
				case <-ctx.Done():
				}
				if len(active) >= c.MaxActive || ctx.Err() != nil {
					break
				}
			}
			if runErr != nil {
				break
			}
		}
		select {
		case root := <-finished:
			delete(active, root)
		case <-ticker.C:
		case <-ctx.Done():
		}
	}
	cancel()
	close(jobs)
	joined := make(chan struct{})
	go func() { group.Wait(); close(joined) }()
	grace := time.NewTimer(time.Duration(c.ShutdownGraceSeconds) * time.Second)
	defer grace.Stop()
	select {
	case <-joined:
		return runErr
	case <-grace.C:
		// The app must keep shared PG alive until Run returns. This failure is not
		// a join; the outer process shutdown deadline remains the hard last resort.
		logger := w.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("Flow shutdown exceeded grace; waiting for real join", "reason", "flow_drain_timeout")
		<-joined
		return errors.Join(runErr, fmt.Errorf("%w: flow drain exceeded %ds", ErrDrainTimeout, c.ShutdownGraceSeconds))
	}
}

// advanceRoot 推进一颗根，区分正常交棒与接管：接管必须先 Recover 再 Advance。
//
// 租约只在**结论稳定**时交还；结论未定时 Abandon（不写 released_at），让 TTL 到期
// 后仍以 TookOver 重新进入 Recover 分支。三道门各挡一种"还有人在动"的情形：
//   - ① Isolated 反面 `HasLiveRunLease`：子树仍有有效 run 租约 → 有人能提交。
//   - ③ 未决动作 `HasUnresolvedAction`：子树仍有 dispatching / unknown 动作 →
//     在未知结果之上再派发一次。命中必须 Abandon，把下一轮逼成 TookOver=true。
//   - ② Joined 反面 `HasInFlightNodeAttempt`：子树仍有 prepared / running attempt
//     → 子节点可能仍在提交。interrupted / unknown 不在此列（只能由 Recover 清理）。
//
// **③ 必须排在 ② 之前**（C-13「命中即 Abandon，不得 Release」）。崩溃留下的现场
// 往往同时有 `running` attempt 与 `dispatching` 动作：Recover 因为未决动作在写库前
// 就返回，attempt 永远不会被收敛；若 ② 先判，它每轮都给出 Release，下一轮就是
// 正常交棒、再也不进 Recover，根被永久锁在 running 且按轮询频率空转。先判 ③ 才能
// 把每一轮都送回 Recover 重判。
//
// Gate ③ must run before gate ②: a crash leaves both a running attempt and a
// dispatching action, and Recover returns before writing while the action is
// unresolved. Letting gate ② release would turn every later round into a normal
// handoff that never re-enters Recover.
//
// advanceRoot advances one root, recovering first when the lease was taken over;
// the lease is released only on a stable conclusion and abandoned otherwise.
func (w *Worker) advanceRoot(ctx context.Context, task Task) error {
	if w == nil || w.Service == nil || w.Leases == nil || w.Gate == nil || w.Identity.IsZero() {
		return ErrStateConflict
	}
	held, err := w.Leases.Acquire(ctx, core.LeaseFlowRoot, task.ID, w.Identity, w.LeaseTTL)
	if errors.Is(err, core.ErrLeaseHeld) {
		return nil
	}
	if err != nil {
		return err
	}
	// keepLease 收 LeaseToken；Acquire 返回的是 Lease，必须取内嵌的凭证。
	leaseCtx, stop, abandon := core.KeepLease(ctx, w.Leases, held.LeaseToken, w.RenewInterval, w.LeaseTTL, w.Logger)
	stable := false
	defer func() {
		if stable {
			releaseCtx, done := w.releaseContext()
			stop(releaseCtx) // 停续约、等 keeper 退出、再用剩余预算 Release
			done()
			return
		}
		abandon() // 停续约、**不** Release；TTL 到期后仍以 TookOver 重试
	}()
	principal, err := StoredPrincipal(task)
	if err != nil {
		return err
	}

	// ① Isolated = !HasLiveRunLease：仍有有效 run 租约 → 有人能提交，本轮不查证也不推进。
	live, err := w.Gate.HasLiveRunLease(leaseCtx, task.ID)
	if err != nil {
		return err
	}
	if live {
		stable = true // 明确的稳定结论：等子 Run 的租约失效，不是错误
		return nil
	}

	expected := task.Version // 推进用的期望版本；Recover 会把它刷新
	if held.TookOver {
		recovered, err := w.Service.Recover(leaseCtx, ResumeRequest{
			Principal:   principal,
			TaskID:      task.ID,
			Expected:    expected, // 必须传：否则无法拒绝基于旧版本的恢复请求
			Lease:       held.LeaseToken,
			OperationID: fmt.Sprintf("flow:%s:recover:%d", task.ID, held.OwnershipEpoch),
			Reason:      "worker_lease_expired",
		})
		switch {
		case errors.Is(err, ErrActionUnknown):
			stable = true // 稳定结论：保持 blocked，不推进、不解除阻断
			return nil
		case errors.Is(err, ErrTaskClosed), errors.Is(err, ErrDeadline):
			// 领取之后被并发关闭 / 越过 deadline：Recover 没写库，关闭意图已经
			// 在持久状态里，推进者是 sweepClosures → 交还租约即可。
			stable = true
			return nil
		case err != nil:
			return err // **没查完**：stable=false → Abandon → 下一轮重跑 Recover
		}
		expected = recovered.Version // 用 Recover 返回的新版本，不得沿用进来时的版本
	}

	// ③ 未决动作（**先于门 ②**，C-13）：正常交棒（TookOver=false）不走 Recover
	//    分支，这是"释放后仍受保护"唯一能失守的地方——上一轮把动作写成 dispatching
	//    后崩溃 / 主动释放，本轮以正常交棒领到同一根，直接 Advance 就会在未知结果
	//    之上再派发一次。命中 → **Abandon**（不 Release）：释放会让下一轮继续是正常
	//    交棒、永远绕开 Recover；不释放则租约到期后必为 TookOver=true，由 Recover
	//    给出确定答复。
	//
	//    本门必须先判：崩溃现场同时留着 running attempt 时，门 ② 会给出 Release，
	//    而 Recover 又因未决动作在写库前返回、永不收敛该 attempt——先判 ② 会让根
	//    永远回不到 Recover，并按轮询频率反复领取 / 释放。
	pendingActions, err := w.Gate.HasUnresolvedAction(leaseCtx, task.ID)
	if err != nil {
		return err
	}
	if pendingActions {
		return nil // stable=false → Abandon
	}

	// ② Joined = !HasInFlightNodeAttempt：还有 prepared / running attempt
	//    → 子节点可能仍在提交，不得推进。interrupted / unknown 不在此列。
	unresolved, err := w.Gate.HasInFlightNodeAttempt(leaseCtx, task.ID)
	if err != nil {
		return err
	}
	if unresolved {
		stable = true // 稳定结论：等 Recover / 子 Run 收敛，本轮不推进
		// 释放后根会停在 blocked，而 ReadyRoots 不取 blocked（除非
		// reason=bounded_rework_required）——下一轮由回收器 A3 的 blocked 分支接住
		// 再做一次 Recover，不是无人推进。
		return nil
	}

	if _, err := w.Service.Advance(leaseCtx, AdvanceRequest{
		Principal: principal,
		TaskID:    task.ID,
		Expected:  expected, // 已被 Recover 刷新过
		Lease:     held.LeaseToken,
	}); err != nil {
		return err // 结论未定：不交还，交 TTL 回收
	}
	stable = true
	return nil
}

// handleJobError 把一次工作项的推进失败分成两类：竞争 / 会话语义错误一律不动行，
// 其余记为 `advance_unavailable` 并保留检查点供下一轮重试。
//
// `ErrFlowRootBusy` 必须归入"不动行"：受控返工 `rework → Revise` 会自取根租约，
// 两个 Worker 同时拿到同一个 blocked + bounded_rework_required 根时，落败方拿到的是
// "另一工作者持有该根"，不是推进失败。此时停靠会把 reason 改写成
// advance_unavailable——A3 靠 reason 分路，持有者随后的 AcceptRevision 也会因
// state_version CAS 失败丢掉这次返工（评审 IDX-14）。
// handleJobError classifies a job failure: contention and conversational state errors
// leave the row untouched; anything else parks as advance_unavailable. A busy root
// lease means another worker owns the rework, not that this advance failed.
func (w *Worker) handleJobError(ctx context.Context, item workItem, err error) {
	if err == nil || errors.Is(err, core.ErrPreconditionFailed) || errors.Is(err, ErrTaskClosed) || errors.Is(err, ErrCapacity) || errors.Is(err, ErrFlowRootBusy) {
		return
	}
	logger := w.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("Flow advance stopped", "task_id", item.task.ID, "reason", "advance_unavailable")
	repository, ok := w.Service.Repository.(parkedRepository)
	if !ok {
		return
	}
	finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	current, readErr := w.Service.Repository.Read(finish, item.principal, item.task.ID)
	if readErr != nil || current.Status.Terminal() || (current.Status == Blocked && current.Reason != "bounded_rework_required") || current.CloseIntent != CloseNone {
		return
	}
	if parkErr := repository.ParkTask(finish, item.principal, current.ID, current.Version, "advance_unavailable"); parkErr != nil {
		logger.Warn("Flow checkpoint retained", "task_id", current.ID, "reason", "park_unconfirmed")
	}
}

// reworkIfBounded 承接 Advance 提交出的 `blocked + bounded_rework_required`
// 结论：Worker 继续做一次受控返工。必须在 advanceRoot 交还租约**之后**执行——
// rework → Revise 会自取根租约，持租约调用只会被拒为 ErrFlowRootBusy。
// reworkIfBounded performs the bounded rework after the advance, outside the lease.
func (w *Worker) reworkIfBounded(ctx context.Context, item workItem) error {
	current, err := w.Service.Repository.Read(ctx, item.principal, item.task.ID)
	if err != nil {
		return workerQueryError(ctx, err)
	}
	if current.Status != Blocked || current.Reason != "bounded_rework_required" {
		return nil
	}
	_, err = w.Service.rework(ctx, item.principal, current)
	return err
}

// releaseContext 返回交还租约可用的预算 ctx 与其释放函数。注入的停机编排优先；
// 未注入时退回有界后台预算（请求路径同款 2s），**不得**用 WithoutCancel 剥掉 deadline。
// releaseContext prefers the injected shutdown budget and otherwise falls back to
// a bounded background deadline; the returned func releases that fallback timer.
func (w *Worker) releaseContext() (context.Context, func()) {
	if w != nil && w.ReleaseCtx != nil {
		if ctx := w.ReleaseCtx(); ctx != nil {
			return ctx, func() {}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), rootLeaseReleaseTimeout)
	return ctx, cancel
}

// workerQueryError 仅消除已结束父上下文的纯取消错误，复合错误保守保留。
// workerQueryError normalizes only errors made purely of the parent's cancellation;
// joined errors carrying any other failure stay intact for the caller's real join.
func workerQueryError(ctx context.Context, err error) error {
	if CancellationOnly(err, ctx.Err()) {
		return nil
	}
	return err
}

// CancellationOnly 判定 err 是否为"纯取消"：沿单链包装与 errors.Join 树逐叶下钻，
// 仅当至少有一个叶子且每个叶子都等于 cancelled 时才为真。该函数存在的理由：
// `errors.Join(rows.Err(), rows.Close())` 即便只有一个非空子错误也会产生带
// `Unwrap() []error` 的 join 节点，单链 `errors.Unwrap` 到它就停住；若只按单链
// 判断，取消期间一句普通查询的 context.Canceled 会被误报为数据库故障，把正常
// 停机变成 rt.Close() 失败（CI `-race` 的 010-race-distributed 即此症状）。携带任何真实
// 失败叶子（例如数据库错误、drain 超时）的 join 一律保留，交给调用方报告。
// CancellationOnly reports whether err consists solely of the cancellation sentinel.
// It descends through both single-chain wrappers and errors.Join trees because a
// join node stops errors.Unwrap even when it contains only one non-nil child; a
// pure cancellation wrapped that way must not be mistaken for a database failure.
// Any join carrying a real failure leaf is retained and reported by the caller.
func CancellationOnly(err, cancelled error) bool {
	if err == nil || cancelled == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		found := false
		for _, child := range children {
			if child == nil {
				continue
			}
			found = true
			if !CancellationOnly(child, cancelled) {
				return false
			}
		}
		return found
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if child := wrapped.Unwrap(); child != nil {
			return CancellationOnly(child, cancelled)
		}
	}
	return err == cancelled
}

// StoredPrincipal 从任务的不可变契约重建**最初授权**的执行身份：契约里的调用方 /
// Key / 授权类型必须与任务行逐字一致，任一不符即拒绝——那说明行的归属被改写过。
// US-24 起它被导出（P-05，只是可见性变化）：回收器在 `internal/app` 层，跨 Worker
// 接管时要按**原授权**调用 `Service.Recover`，不能调包内私有函数，也不能另造一份
// 身份重建逻辑（两份实现迟早分叉，接管方就会用错身份推进别人的根）。
// StoredPrincipal rebuilds the original authorizing identity from the immutable task
// contract; exported for the app-level reclaimer, with no behaviour change.
func StoredPrincipal(task Task) (core.Principal, error) {
	if task.Contract.Scope.CallerID != task.CallerID || task.Contract.Scope.KeyID != task.AuthorizingKeyID || string(task.Contract.Scope.AuthorizingKind) != task.AuthorizingKind {
		return core.Principal{}, core.ErrUnauthorized
	}
	scopes := task.Contract.Scope.Scopes
	if task.AuthorizingKind == string(core.AuthorizingKindAdminBuiltin) {
		// The existing admin principal deliberately has no caller-key scopes.
		// Its explicit Flow ceiling remains in the immutable task contract.
		scopes = nil
	}
	return core.OwnedPrincipal(task.CallerID, core.AuthorizingKind(task.AuthorizingKind), task.AuthorizingKeyID, scopes, core.ActorRecovery)
}
