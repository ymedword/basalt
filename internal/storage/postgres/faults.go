//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// 本文件是**仅测试构建可见**的提交故障钩子（设计 §3.1.1 `DB` 行的 `faults` 组）。
//
// 它们存在的唯一理由：`Tx` 的三态协议必须能在"确认丢失""可证明未发送""服务端拒绝"
// "核查早于 COMMIT 完成"这四种真实故障下被验证。伪造返回值测不出协议——这些钩子只
// 改变**连接层的行为**（提交/回滚/阻塞），事务与核查仍然打真库、用真 xid。
//
// 生产构建里同一类型由 `faults_off.go` 提供空壳，`newCommitFaults` 返回 nil：发布二进制
// **不存在**注入面，事务路径也不经过任何 `faults` 分支。
//
// This file is the commit-fault harness available only in integration builds. The hooks
// change connection-level behaviour (commit/rollback/blocking) so the tri-state protocol
// can be exercised against a real server with real xids; the production build ships an
// empty stub and therefore carries no injection surface.

// commitAction 是一次装载的故障种类；`faultNone` 表示未装载。
// commitAction selects the injected behaviour for the next COMMIT.
type commitAction int

const (
	faultNone commitAction = iota
	// faultLoseCommitAck：真实提交成功，客户端只看到连接错误（确认丢失）。
	faultLoseCommitAck
	// faultFailBeforeSend：COMMIT 请求确定未发出（满足 pgconn.SafeToRetry）。
	faultFailBeforeSend
	// faultHoldCommit：真实 COMMIT 卡在服务端执行中（延迟唯一约束 + 另一会话占键）。
	faultHoldCommit
	// faultAbortAfterAckLoss：服务端最终回滚，但客户端同样只看到"确认丢失"。
	faultAbortAfterAckLoss
)

// provablyUnsentError 满足 `pgconn.SafeToRetry` 的接口断言（`SafeToRetry() bool`），
// 语义就是"请求确定没发出去"。**不得**让其他错误类型实现它——那会把未知结局误判成可重试。
// provablyUnsentError is the only error type here that claims SafeToRetry.
type provablyUnsentError struct{}

func (provablyUnsentError) Error() string { return "pgtest: 模拟 COMMIT 请求未发出" }

func (provablyUnsentError) SafeToRetry() bool { return true }

var (
	errFaultNotSent = provablyUnsentError{}
	// errFaultAckLost 表示"服务端可能已提交、确认丢了"：既不是 PgError，也不是 SafeToRetry，
	// 所以三态协议只能把它归为 TxUnknown。
	errFaultAckLost = errors.New("pgtest: 模拟提交确认丢失（连接在 COMMIT 之后断开）")
)

// commitFaults 挂在 `DB` 上；未装载时所有方法都是无操作。
// commitFaults is inert until an arm method is called.
type commitFaults struct {
	mu     sync.Mutex
	action commitAction
	// admission 是"已持生命周期行锁"的一次性暂停点；nil 表示未装载。
	// admission is the one-shot admission hold; nil means not armed.
	admission *admissionHold
	// started 把"某次事务开始提交"的 TxAttempt 交给 WaitCommitInFlight；容量 1，
	// 没有等待者时写入被丢弃（不阻塞事务路径）。
	started chan TxAttempt
	replica *sql.DB
	db      *DB
}

// newCommitFaults 装配钩子。生产构建的同名函数返回 nil。
// newCommitFaults wires the hooks; the production build returns nil instead.
func newCommitFaults(db *DB) *commitFaults {
	return &commitFaults{db: db, started: make(chan TxAttempt, 1)}
}

// LoseNextCommitAck 让下一次提交真实落库，但把确认换成连接错误。
// LoseNextCommitAck makes the next COMMIT really commit while the client sees an error.
func (f *commitFaults) LoseNextCommitAck() { f.arm(faultLoseCommitAck) }

// FailNextCommitBeforeSend 让下一次提交返回"可证明未发送"的错误。
// FailNextCommitBeforeSend makes the next COMMIT provably unsent.
func (f *commitFaults) FailNextCommitBeforeSend() { f.arm(faultFailBeforeSend) }

// ArmLoseNextCommitAck 是 `LoseNextCommitAck` 的**跨包**入口：`internal/app` 的端到端
// 用例需要用真实句柄驱动真实运行入口，而 `faults` 字段只在包内可见。
//
// 它与包内用例用的是**同一个**钩子、同一条连接层行为（真实提交、真实 xid、只丢确认），
// 因此不构成第二种注入面；伪造一个 `TxUnknownError` 返回给上层是另一回事——那种写法
// 只能测"上层认不认这个错误"，测不出协议本身。
//
// **仅集成构建可见**：`faults_off.go`（`!integration`）不提供同名符号，发布二进制里
// 调用方连编译都过不了。句柄未经装配时 `t.Fatal`，不静默当成"已装载"。
//
// ArmLoseNextCommitAck is the cross-package entry to the very same connection-level hook
// (real commit, real xid, lost ack). It exists only under the integration build tag; a
// handle without the harness is fatal rather than a silent no-op.
func ArmLoseNextCommitAck(t *testing.T, db *DB) {
	t.Helper()
	if db == nil || db.faults == nil {
		t.Fatal("提交故障钩子不可用：句柄未经 newCommitFaults 装配（集成构建里应恒为非 nil）")
	}
	db.faults.LoseNextCommitAck()
}

// AbortNextCommitAfterAckLoss 让下一次提交在服务端回滚，但同样只报"确认丢失"。
// AbortNextCommitAfterAckLoss rolls back server-side while reporting a lost ack.
func (f *commitFaults) AbortNextCommitAfterAckLoss() { f.arm(faultAbortAfterAckLoss) }

// ArmAbortNextCommitAfterAckLoss 是 `AbortNextCommitAfterAckLoss` 的**跨包**入口，
// 与 `ArmLoseNextCommitAck` 同一形态：真实回滚、真实 xid、客户端只看到"确认丢失"。
//
// 它守的是"回查查不到"这一支：只读快照看不见任何行，既可能是真回滚、也可能是原
// COMMIT 仍在服务端执行（C-34），因此上层只能按未知结局失败关闭——不得判未落、不得
// 退款、不得重试。仅集成构建可见。
// The cross-package entry for the abort-after-ack-loss shape: it exercises the "lookup
// finds nothing" branch, which must fail closed rather than conclude a rollback.
func ArmAbortNextCommitAfterAckLoss(t *testing.T, db *DB) {
	t.Helper()
	if db == nil || db.faults == nil {
		t.Fatal("提交故障钩子不可用：句柄未经 newCommitFaults 装配（集成构建里应恒为非 nil）")
	}
	db.faults.AbortNextCommitAfterAckLoss()
}

// SeedDeferredUniqueConflict 在隔离 schema 上把 `caller_usage_periods` 的主键改成
// **延迟唯一约束**，再用一条已提交的冲突行占位：业务事务的重复插入直到 COMMIT 才被
// 服务端拒绝（23505）——这是"服务端明确拒绝 COMMIT"的真实形态，不是伪造的 PgError。
//
// SeedDeferredUniqueConflict turns the caller_usage_periods primary key into a deferred
// unique constraint and seeds one committed conflicting row, so the duplicate insert is
// rejected by the server at COMMIT time. Only valid on an isolated test schema.
func (f *commitFaults) SeedDeferredUniqueConflict(t *testing.T) {
	t.Helper()
	mustExec(t, f.db.pool, `ALTER TABLE caller_usage_periods
		DROP CONSTRAINT IF EXISTS caller_usage_periods_pkey`)
	mustExec(t, f.db.pool, `ALTER TABLE caller_usage_periods
		ADD CONSTRAINT caller_usage_periods_pkey
		PRIMARY KEY (caller_id, period_start, kind) DEFERRABLE INITIALLY DEFERRED`)
	mustExec(t, f.db.pool, `INSERT INTO caller_usage_periods
		(caller_id, period_start, kind, updated_at) VALUES ('c1', 0, 'tasks', $1)`,
		time.Now().UTC().UnixMilli())
}

// HoldNextCommit 让下一次提交**卡在服务端执行中**：另开一条会话插入同一条
// `(caller_id, idempotency_key)` 但不提交，延迟唯一约束把冲突检查推到 COMMIT，
// 于是 COMMIT 在服务端等待那条未提交的事务。返回的 release 回滚占位事务（因此放行），
// 可重复调用。
//
// HoldNextCommit blocks the next COMMIT server-side with a deferred unique constraint
// held open by another session; release() rolls the holder back and unblocks it.
func (f *commitFaults) HoldNextCommit(t *testing.T) func() {
	t.Helper()
	ctx := context.Background()
	mustExec(t, f.db.pool, `ALTER TABLE quota_reservations
		DROP CONSTRAINT IF EXISTS quota_reservations_caller_id_idempotency_key_key`)
	mustExec(t, f.db.pool, `ALTER TABLE quota_reservations
		ADD CONSTRAINT quota_reservations_caller_id_idempotency_key_key
		UNIQUE (caller_id, idempotency_key) DEFERRABLE INITIALLY DEFERRED`)

	conn, err := f.db.pool.Conn(ctx)
	if err != nil {
		t.Fatalf("占用提交冲突键：获取独立会话失败: %v", err)
	}
	holder, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("占用提交冲突键：开启占位事务失败: %v", err)
	}
	if _, err := holder.ExecContext(ctx, `INSERT INTO quota_reservations
		(reservation_id, caller_id, idempotency_key, class, period_start, amount, state, created_at)
		VALUES ('hold-r1', 'c1', 'run:r1:admit', 'run-chat', 0, 1, 'pending', $1)`,
		time.Now().UTC().UnixMilli()); err != nil {
		_ = holder.Rollback()
		_ = conn.Close()
		t.Fatalf("占用提交冲突键：写入占位预留失败: %v", err)
	}
	f.arm(faultHoldCommit)

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = holder.Rollback()
			_ = conn.Close()
		})
	}
}

// WaitCommitInFlight 是"确实卡住了"的同步点：等被钩住的那次事务开始提交，并确认它的
// 后端真的进入锁等待，然后把该次事务的 `TxAttempt` 交给调用方去核查。
//
// WaitCommitInFlight waits until the hooked transaction is committing and its backend is
// actually lock-waiting, then returns the attempt for verification.
func (f *commitFaults) WaitCommitInFlight(t *testing.T) TxAttempt {
	t.Helper()
	select {
	case attempt := <-f.started:
		f.waitBackendLocked(t, attempt.BackendPID)
		return attempt
	case <-time.After(15 * time.Second):
		t.Fatal("等待 COMMIT 进入执行中失败：15s 内没有事务开始提交")
		return TxAttempt{}
	}
}

// SetReplicaDSN 把核查连接改到给定 DSN，用于验"核查只打主库"。这里**故意**不经
// `normalizeDSN`：副本 DSN 的典型错误正是没声明 `target_session_attrs=read-write`，
// 补上它，"核查必须打主库"的分支就永远走不到了。
//
// SetReplicaDSN redirects verification to the given DSN and deliberately skips
// normalizeDSN — a replica DSN's classic defect is exactly the missing
// target_session_attrs=read-write, and repairing it would make the guard untestable.
func (f *commitFaults) SetReplicaDSN(t *testing.T, dsn string) {
	t.Helper()
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("解析只读副本连接串: %v", err)
	}
	pool := stdlib.OpenDB(*connCfg)
	f.mu.Lock()
	f.replica = pool
	f.mu.Unlock()
	t.Cleanup(func() { _ = pool.Close() })
}

// replicaPool 返回核查连接要用的池；未设置时为 nil（生产构建恒为 nil）。
// replicaPool is nil unless SetReplicaDSN installed one.
func (f *commitFaults) replicaPool() *sql.DB {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replica
}

// commitWithFaults 执行被钩住的一次 COMMIT。第三个返回值 false 表示未装载故障，
// 调用方按正常路径提交；true 表示本函数已给出最终结局。
//
// commitWithFaults returns ok=false when no fault is armed, letting the caller commit
// normally; otherwise the returned outcome and error are final.
func (f *commitFaults) commitWithFaults(tx *sql.Tx, attempt TxAttempt,
	classify func(error, TxAttempt) (TxOutcome, error),
) (TxOutcome, error, bool) {
	switch action := f.takeAction(); action {
	case faultFailBeforeSend:
		// 请求确定未发出：服务端从未见过 COMMIT，事务必然回滚。回滚只为释放连接。
		rbErr := rollback(tx)
		outcome, err := classify(errFaultNotSent, attempt)
		return outcome, errors.Join(err, rbErr), true
	case faultAbortAfterAckLoss:
		// 服务端最终回滚，客户端只看得到"确认丢失"——核查必须能分辨这两种形态。
		rbErr := rollback(tx)
		return TxUnknown, &TxUnknownError{
			Attempt: attempt,
			Cause:   errors.Join(errFaultAckLost, rbErr),
		}, true
	case faultLoseCommitAck:
		if cerr := tx.Commit(); cerr != nil {
			// 真实提交也失败了：保留原始错误，按正常判定并标注确认丢失。
			outcome, err := classify(errors.Join(cerr, errFaultAckLost), attempt)
			return outcome, err, true
		}
		return TxUnknown, &TxUnknownError{Attempt: attempt, Cause: errFaultAckLost}, true
	case faultHoldCommit:
		f.markCommitStarted(attempt)
		outcome, err := classify(tx.Commit(), attempt)
		return outcome, err, true
	default:
		return "", nil, false
	}
}

// arm 装载一次故障；takeAction 取走并复位——故障只作用一次，重放路径不会被二次注入。
// A fault is armed once and consumed by the next COMMIT.
func (f *commitFaults) arm(action commitAction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.action = action
}

func (f *commitFaults) takeAction() commitAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	action := f.action
	f.action = faultNone
	return action
}

// markCommitStarted 把此事务的 TxAttempt 交给 WaitCommitInFlight；无等待者时丢弃。
// markCommitStarted hands the attempt over without blocking the commit path.
func (f *commitFaults) markCommitStarted(attempt TxAttempt) {
	select {
	case f.started <- attempt:
	default:
	}
}

// waitBackendLocked 轮询 `pg_stat_activity`，直到该后端确实停在锁等待上。
// waitBackendLocked polls until the backend is genuinely lock-waiting.
func (f *commitFaults) waitBackendLocked(t *testing.T, pid int32) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var waitType sql.NullString
		err := f.db.pool.QueryRowContext(context.Background(),
			`SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waitType)
		if err == nil && waitType.Valid && waitType.String == "Lock" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("事务后端 pid=%d 未进入锁等待：无法证明 COMMIT 正在服务端执行", pid)
}

// admissionHold 是"StartRun 事务已持生命周期行锁"的暂停点（FR-24-41 的获锁顺序①）。
// 它是一个可装载一次的门：装载后**下一次**走到该点的准入会阻塞在 release 上，其余准入
// 直接通过——测试只需一次真实的锁持有，不改变其他用例的时序。
// admissionHold lets exactly one admission stop after taking the lifecycle row lock.
type admissionHold struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

// HoldStartRunAfterLifecycleLock 装载一次暂停点，返回"已到达"信号与放行函数。
// 放行函数幂等；用例应在 t.Cleanup 里调用，避免失败路径把连接一直挂住。
//
// HoldStartRunAfterLifecycleLock arms the pause once and returns reached/release.
func (f *commitFaults) HoldStartRunAfterLifecycleLock() (<-chan struct{}, func()) {
	hold := &admissionHold{reached: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	f.admission = hold
	f.mu.Unlock()
	return hold.reached, func() { hold.once.Do(func() { close(hold.release) }) }
}

// holdAdmissionLock 是 RunStore 在生命周期复查之后调用的暂停点；未装载即无操作。
// holdAdmissionLock blocks once when armed; otherwise it is a no-op.
func (f *commitFaults) holdAdmissionLock(ctx context.Context) error {
	f.mu.Lock()
	hold := f.admission
	f.admission = nil
	f.mu.Unlock()
	if hold == nil {
		return nil
	}
	close(hold.reached)
	select {
	case <-hold.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ArmStartRunLockHold 是 `HoldStartRunAfterLifecycleLock` 的**跨包**入口（`internal/app`
// 的 FR-24-41 用例用真实句柄驱动真实运行入口）。仅集成构建可见；句柄未经装配时 `t.Fatal`，
// 不静默当成"已装载"。返回的 release 幂等，务必挂到 `t.Cleanup`。
//
// ArmStartRunLockHold is the cross-package entry to the same hold point; integration-only.
func ArmStartRunLockHold(t *testing.T, db *DB) (<-chan struct{}, func()) {
	t.Helper()
	if db == nil || db.faults == nil {
		t.Fatal("准入锁暂停点不可用：句柄未经 newCommitFaults 装配（集成构建里应恒为非 nil）")
	}
	return db.faults.HoldStartRunAfterLifecycleLock()
}

// mustExec 是钩子自身的装配语句，失败即让用例立刻停下（装配错误不该表现成断言失败）。
func mustExec(t *testing.T, pool *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("故障钩子装配失败（%s）: %v", firstLine(query), err)
	}
}

// firstLine 取语句首行，避免多行 SQL 把错误信息撑爆。
func firstLine(query string) string {
	for i := 0; i < len(query); i++ {
		if query[i] == '\n' {
			return query[:i]
		}
	}
	return query
}
