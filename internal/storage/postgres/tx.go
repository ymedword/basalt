package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// 本文件是**提交结局三态协议**（设计 §3.1.1 / C-34 / D-12 / D-39）。
//
// 一条纪律贯穿全文件：**提交返回错误 ≠ 事务已回滚**。连接在 COMMIT 发出后断开时，服务端
// 可能已经提交成功、只是确认丢了——把它当"未落"去重放，就会产生重复 Run、重复预留。
// 因此 `Tx` 返回 `(TxOutcome, error)` 三态；只有三种情形能判 `TxRolledBack`：业务回调报错
// 且回滚完成、`pgconn.SafeToRetry` 证明提交**未发出**、服务端对 COMMIT 明确返回
// `*pgconn.PgError`（PG 对 COMMIT 返回 ErrorResponse 即已回滚该事务）。其余一律
// `TxUnknown`，调用方**必须**走 `VerifyTx` 的两步核查，**不得**判未落、**不得**重放。
//
// This file implements the tri-state commit protocol: a COMMIT error is never taken as
// "rolled back" unless it is provably unsent or the server explicitly rejected it.
// Everything else is TxUnknown and must go through VerifyTx.

// TxOutcome 是事务提交结局的三态。**三态不得压缩成布尔**——`TxUnknown` 既不能当已提交，
// 也不能当未提交；`Valid()` 拒绝零值，表示"还没有结论"。
// TxOutcome is the three-state commit result; the zero value is invalid.
type TxOutcome string

const (
	// TxCommitted 表示服务端确认提交。
	// TxCommitted means the server acknowledged the commit.
	TxCommitted TxOutcome = "committed"
	// TxRolledBack 表示**确认回滚**：业务回调报错且回滚完成、提交可证明未发出、或服务端
	// 明确拒绝 COMMIT。只有这一态允许直接重试业务事务。
	// TxRolledBack is a proven rollback — the only state that may be retried directly.
	TxRolledBack TxOutcome = "rolled-back"
	// TxUnknown 表示提交已发出但确认丢失：可能已提交。**不得**据以判定未落、**不得**重放，
	// 必须用 `TxUnknownError.Attempt` 调 `VerifyTx` 定结局。
	// TxUnknown means the commit may or may not have happened; verify before acting.
	TxUnknown TxOutcome = "unknown"
)

// Valid 判定结局是否是闭集内的取值；零值与未知值一律拒绝。
// Valid accepts exactly the three literals.
func (o TxOutcome) Valid() bool {
	return o == TxCommitted || o == TxRolledBack || o == TxUnknown
}

// TxVerdict 是 `VerifyTx` 的四态结论。四态**不得**合并：`TxVerifyPending` 与
// `TxVerifyInconsistent` 都不允许重试，但前者是"等结局"，后者是"结局与业务行矛盾"。
// TxVerdict is the four-state outcome of VerifyTx; the states are never merged.
type TxVerdict string

const (
	// TxVerifyCommitted 表示原事务已提交且业务键命中——按已提交处理，不复写。
	// TxVerifyCommitted means the transaction committed and the business row is present.
	TxVerifyCommitted TxVerdict = "committed"
	// TxVerifyAbsent 表示原事务已回滚且业务键查无行——**唯一**可重试的结论。
	// TxVerifyAbsent is the only verdict that permits a retry.
	TxVerifyAbsent TxVerdict = "absent"
	// TxVerifyPending 表示原事务仍在执行、结局读不到（XID 已回收返回 NULL）或核查不可用
	// ——**不得**重试、**不得**判未落；调用方限时等待后冻结前滚并告警。
	// TxVerifyPending forbids both retrying and declaring the write absent.
	TxVerifyPending TxVerdict = "pending"
	// TxVerifyInconsistent 表示结局与业务行矛盾（已提交却查无行 / 已回滚却有行）——
	// 冻结当前前滚路径并告警，**不得**重放。
	// TxVerifyInconsistent means verdict and business rows contradict each other.
	TxVerifyInconsistent TxVerdict = "inconsistent"
)

// Valid 判定结论是否是闭集内的取值；零值与未知值一律拒绝。
// Valid accepts exactly the four literals.
func (v TxVerdict) Valid() bool {
	switch v {
	case TxVerifyCommitted, TxVerifyAbsent, TxVerifyPending, TxVerifyInconsistent:
		return true
	default:
		return false
	}
}

// TxAttempt 是一次业务事务的身份证据：`XID` 来自事务内 `pg_current_xact_id()`（`xid8`，
// **无符号 64 位**——不得用 `int64` 承载，超过 `MaxInt64` 会溢出），`BackendPID` 来自
// `pg_backend_pid()`，`StartedAt` 取句柄时钟。`TxUnknown` 时**只能**从
// `TxUnknownError.Attempt` 取它，**不得**经日志或包级变量传递。
// TxAttempt identifies the original transaction so VerifyTx can look up its fate.
type TxAttempt struct {
	// XID 是 `pg_current_xact_id()` 的 `xid8` 值。
	XID uint64
	// BackendPID 是执行该事务的后端进程号。
	BackendPID int32
	// StartedAt 是事务开始时刻（句柄时钟）。
	StartedAt time.Time
}

// TxUnknownError 是 `TxUnknown` 的错误载体：`Unwrap` 到 `ErrTxOutcomeUnknown`，
// 调用方既能 `errors.Is` 判类，又能 `errors.As` 取出 `TxAttempt` 去核查。
// `Cause` 保留原始提交错误，供日志与审计读取——**不得**吞掉。
// TxUnknownError carries the attempt; Cause keeps the original commit error observable.
type TxUnknownError struct {
	// Attempt 是原事务的身份证据，核查时必须原样交给 `VerifyTx`。
	Attempt TxAttempt
	// Cause 是原始提交错误（可能为空）。
	Cause error
}

// Error 渲染结局与事务标识（不含任何凭据值）。
// Error renders the unknown outcome with its transaction identity.
func (e *TxUnknownError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("事务提交结果未知（xid=%d, backend_pid=%d）：可能已提交，必须经 VerifyTx 核查，不得重放: %v",
			e.Attempt.XID, e.Attempt.BackendPID, e.Cause)
	}
	return fmt.Sprintf("事务提交结果未知（xid=%d, backend_pid=%d）：可能已提交，必须经 VerifyTx 核查，不得重放",
		e.Attempt.XID, e.Attempt.BackendPID)
}

// Unwrap 暴露 `ErrTxOutcomeUnknown`。
// Unwrap exposes the sentinel so callers can classify with errors.Is.
func (e *TxUnknownError) Unwrap() error { return ErrTxOutcomeUnknown }

var (
	// ErrTxOutcomeUnknown 在提交结果未知时随 `Tx` 返回。上层**不得**吞掉、**不得**当成
	// 业务失败静默处理、**不得**映射成 4xx（会让客户端以为没有副作用而重放）。
	// ErrTxOutcomeUnknown is returned when the commit outcome cannot be proven.
	ErrTxOutcomeUnknown = errors.New("postgres: 事务提交结果未知")
	// ErrTxInconsistent 表示核查结论与业务行矛盾：冻结前滚并告警，**不得**重放。
	// ErrTxInconsistent marks a contradiction between verdict and business rows.
	ErrTxInconsistent = errors.New("postgres: 事务结局与业务行矛盾")
	// ErrPrimaryRequired 表示核查连接不是主库（只读 / 恢复中）：其快照不能用于定结局。
	// ErrPrimaryRequired means verification did not run against a primary.
	ErrPrimaryRequired = errors.New("postgres: 事务核查必须打在主库")
)

// Tx 执行业务事务并返回**提交结局三态**（C-34）。
//
// 事务开始后立刻记录 `pg_current_xact_id()` 与 `pg_backend_pid()`——没有 XID 就无法证明
// 原事务结局，核查只能退化成"查无行即重试"（禁止）。
//
// `fn` **不得**自行提交或回滚；它返回错误时本函数负责回滚并返回 `TxRolledBack`。
// The callback must not commit or roll back on its own.
func (db *DB) Tx(ctx context.Context, fn func(context.Context, *sql.Tx) error) (TxOutcome, error) {
	if db == nil || db.pool == nil {
		return TxRolledBack, errors.New("postgres: 句柄未装配连接池，无法开启事务")
	}
	return txOn(ctx, db.pool, db.clock(), db.faults, fn)
}

// txOn 是 `Tx` 的自由函数形态：崩溃恢复的冻结签名收的是裸 `*sql.DB`（`Recover`），
// 它同样要走三态协议，**不得**另写一套提交处理。`faults` 只由持有它的句柄传入，
// 裸池调用方传 nil。
// txOn lets callers holding a bare *sql.DB use the same tri-state protocol; only a
// handle carrying the integration fault hook passes a non-nil faults value.
func txOn(ctx context.Context, pool *sql.DB, now time.Time, faults *commitFaults,
	fn func(context.Context, *sql.Tx) error,
) (TxOutcome, error) {
	if pool == nil {
		return TxRolledBack, errors.New("postgres: 连接池为空，无法开启事务")
	}
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		// 事务尚未开始：没有任何写入可能落库，可按确认回滚处理。
		return TxRolledBack, fmt.Errorf("开启 PostgreSQL 事务: %w", err)
	}
	attempt, err := readTxAttempt(ctx, tx, now)
	if err != nil {
		return TxRolledBack, errors.Join(err, rollback(tx))
	}

	if ferr := fn(ctx, tx); ferr != nil {
		return TxRolledBack, errors.Join(ferr, rollback(tx))
	}

	// 故障钩子只在测试构建里非 nil；它要么给出最终结局（ok=true），要么把控制权交回。
	if faults != nil {
		if outcome, err, ok := faults.commitWithFaults(tx, attempt, classifyCommitError); ok {
			return outcome, err
		}
	}
	return classifyCommitError(tx.Commit(), attempt)
}

// classifyCommitError 把 COMMIT 的返回错误映射成三态（C-34）。判定抽成独立函数，
// 是为让故障钩子对**合成错误**复用同一套判定——分类顺序全仓只有这一处。
// classifyCommitError is the single place where a COMMIT error becomes a TxOutcome.
func classifyCommitError(cerr error, attempt TxAttempt) (TxOutcome, error) {
	if cerr == nil {
		return TxCommitted, nil
	}
	switch {
	case pgconn.SafeToRetry(cerr):
		// 请求确定**未发出**：服务端没有收到 COMMIT，事务必然回滚。可安全重试。
		return TxRolledBack, fmt.Errorf("提交事务失败（可证明未发送，已回滚）: %w", cerr)
	case isServerRejection(cerr):
		// PG 对 COMMIT 返回 ErrorResponse（如延迟约束冲突）时已回滚该事务。
		return TxRolledBack, fmt.Errorf("服务端拒绝提交（事务已回滚）: %w", cerr)
	default:
		// 提交已发出、确认丢失：可能已提交。只上报未知，不下任何结论。
		return TxUnknown, &TxUnknownError{Attempt: attempt, Cause: cerr}
	}
}

// rollback 回滚并吞掉"事务已结束"的重复回滚噪声；其他回滚错误并入错误链。
// rollback ignores the already-finished sentinel but keeps real rollback failures.
func rollback(tx *sql.Tx) error {
	err := tx.Rollback()
	if err == nil || errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return fmt.Errorf("回滚 PostgreSQL 事务: %w", err)
}

// isServerRejection 判定提交错误是否是服务端对 COMMIT 的明确拒绝。
// isServerRejection reports whether the server rejected the COMMIT itself.
func isServerRejection(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr)
}

// readTxAttempt 在事务内取 `xid8` 与后端 PID。扫描用 `pgtype.Uint64`（`xid8` 的无符号
// 承载类型），**不得**用 `int64`。
// readTxAttempt records the xid8 and backend pid inside the transaction.
func readTxAttempt(ctx context.Context, tx *sql.Tx, now time.Time) (TxAttempt, error) {
	var xid pgtype.Uint64
	if err := tx.QueryRowContext(ctx, `SELECT pg_current_xact_id()`).Scan(&xid); err != nil {
		return TxAttempt{}, fmt.Errorf("读取事务 XID: %w", err)
	}
	if !xid.Valid {
		return TxAttempt{}, errors.New("读取事务 XID: pg_current_xact_id() 返回空值")
	}
	var pid int32
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return TxAttempt{}, fmt.Errorf("读取事务后端 PID: %w", err)
	}
	return TxAttempt{XID: xid.Uint64, BackendPID: pid, StartedAt: now.UTC()}, nil
}

// VerifyTx 是 `TxUnknown` 之后的**唯一**核查入口（C-34），两步缺一不可：
//
//  1. 在**主库**求 `pg_xact_status(xid8)` 定原事务结局——`in progress`、NULL（XID 已回收）
//     或查询失败 → `TxVerifyPending`（带原因返回），**不得**重试、**不得**判未落；
//  2. 结局已定（`committed` / `aborted`）后，在**新连接的新快照**上按业务键跑 `check`：
//     committed+命中 → `TxVerifyCommitted`；committed+查无 → `TxVerifyInconsistent`；
//     aborted+查无 → `TxVerifyAbsent`；aborted+命中 → `TxVerifyInconsistent`。
//
// **普通 SELECT 查无行本身不是"未提交"的证据**——新快照看不到尚在提交中的事务。`check`
// **不得**跑在只读副本上：副本滞后会把已提交读成未提交。
//
// The check callback must run against a primary; a replica's lagging snapshot can read a
// committed write as missing.
func (db *DB) VerifyTx(ctx context.Context, attempt TxAttempt, check func(context.Context, Queryer) (bool, error)) (TxVerdict, error) {
	if db == nil || db.pool == nil {
		return "", errors.New("postgres: 句柄未装配连接池，无法核查事务结局")
	}
	if check == nil {
		return "", errors.New("postgres: 核查缺少业务键检查函数")
	}
	if attempt.XID == 0 {
		return "", errors.New("postgres: TxAttempt 缺少 XID，无法核查原事务结局")
	}

	conn, err := db.verifyConn(ctx)
	if err != nil {
		return TxVerifyPending, fmt.Errorf("获取核查连接: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// 先证明核查连接是主库：只读副本的滞后快照会把已提交读成未提交。
	var inRecovery bool
	var readOnly string
	if err := conn.QueryRowContext(ctx,
		`SELECT pg_is_in_recovery(), current_setting('transaction_read_only')`).Scan(&inRecovery, &readOnly); err != nil {
		return TxVerifyPending, fmt.Errorf("核查连接主库状态: %w", err)
	}
	if inRecovery || readOnly != "off" {
		return "", fmt.Errorf("%w: pg_is_in_recovery=%t transaction_read_only=%s",
			ErrPrimaryRequired, inRecovery, readOnly)
	}

	fate, err := readXactStatus(ctx, conn, attempt.XID)
	if err != nil {
		return TxVerifyPending, err
	}
	if fate == xactStatusInProgress {
		return TxVerifyPending, fmt.Errorf("事务 xid=%d 仍在执行：查无行不是未提交的证据", attempt.XID)
	}

	hit, err := check(ctx, conn)
	if err != nil {
		return TxVerifyPending, fmt.Errorf("按业务键核查事务 xid=%d: %w", attempt.XID, err)
	}

	switch {
	case fate == xactStatusCommitted && hit:
		return TxVerifyCommitted, nil
	case fate == xactStatusCommitted && !hit:
		// 已提交却查无业务行：冻结并告警，**不得**当成未提交去重放。
		return TxVerifyInconsistent, fmt.Errorf("%w: 事务 xid=%d 已提交但业务行不存在", ErrTxInconsistent, attempt.XID)
	case fate == xactStatusAborted && !hit:
		return TxVerifyAbsent, nil
	case fate == xactStatusAborted && hit:
		// 已回滚却有业务行：与结局矛盾，同样冻结。
		return TxVerifyInconsistent, fmt.Errorf("%w: 事务 xid=%d 已回滚但业务行存在", ErrTxInconsistent, attempt.XID)
	default:
		return TxVerifyPending, fmt.Errorf("事务 xid=%d 结局 %q 无法定论", attempt.XID, fate)
	}
}

// verifyConn 取核查连接：默认打主库池；测试构建可用故障钩子把它改到给定 DSN，
// 用来验"核查只打主库"的拒绝分支。生产构建里钩子恒为 nil，恒走主库池。
// verifyConn returns the verification connection from the primary pool unless the
// integration fault hook redirects it.
func (db *DB) verifyConn(ctx context.Context) (*sql.Conn, error) {
	pool := db.pool
	if db.faults != nil {
		if replica := db.faults.replicaPool(); replica != nil {
			pool = replica
		}
	}
	return pool.Conn(ctx)
}

// xactStatus 是先归类后的 `pg_xact_status` 读数。
// xactStatus is the classified reading of pg_xact_status.
type xactStatus string

const (
	xactStatusCommitted  xactStatus = "committed"
	xactStatusAborted    xactStatus = "aborted"
	xactStatusInProgress xactStatus = "in progress"
)

// readXactStatus 求原事务结局。XID 以十进制文本传入并显式转 `text` 再转 `xid8`：参数类型因此
// 固定是 text，不受驱动对 `xid8` 编码计划的影响，也没有 `int64` 溢出面。
// XID 已回收（函数返回 NULL）或返回值不在已知集合时按"查不到结局"处理——一律 pending。
// xactStatus passes the xid as decimal text and casts through text, so no driver-side
// xid8 encoding or int64 overflow is involved.
func readXactStatus(ctx context.Context, q Queryer, xid uint64) (xactStatus, error) {
	var status sql.NullString
	if err := q.QueryRowContext(ctx,
		`SELECT pg_xact_status($1::text::xid8)`, strconv.FormatUint(xid, 10)).Scan(&status); err != nil {
		return "", fmt.Errorf("查询事务 xid=%d 结局: %w", xid, err)
	}
	if !status.Valid {
		// XID 超出保留窗口已被回收：读不到结局一律按 pending，不得推断。
		return xactStatusInProgress, fmt.Errorf("事务 xid=%d 结局不可读（XID 已回收），按 pending 处理", xid)
	}
	switch xactStatus(status.String) {
	case xactStatusCommitted, xactStatusAborted, xactStatusInProgress:
		return xactStatus(status.String), nil
	default:
		return "", fmt.Errorf("事务 xid=%d 返回未知结局 %q，按 pending 处理", xid, status.String)
	}
}

// MarkFirstWrite 把首次业务写入时刻写进**当前** cutover 行（C-13）。签名强制接收事务
// 执行器（与业务写入同事务）与**显式时刻**（不读 `now()`，测试可注入）。
//
// 三条纪律：只命中最新 cutover 行且仅当其 `first_write_at IS NULL`（幂等，历史行不得被写）；
// 写入 `at.UTC().UnixMilli()` 的 **BIGINT 毫秒**（列是 BIGINT 毫秒，**不得**用 `now()` /
// `timestamptz`）；无 cutover 行（全新安装）视为无操作。
//
// 提交结果未知时**不得**据"标记为空"或"查询查无行"推断未提交——必须用 `TxAttempt` 走
// `VerifyTx`；`first_write_at IS NOT NULL` 可作已提交的正证据。
// MarkFirstWrite stamps the first business write on the current cutover row only; an
// unknown commit must be resolved through VerifyTx rather than by inspecting the column.
func (db *DB) MarkFirstWrite(ctx context.Context, tx *sql.Tx, at time.Time) error {
	if tx == nil {
		return errors.New("postgres: MarkFirstWrite 必须与业务写入同一事务——事务执行器为空")
	}
	if at.IsZero() {
		return errors.New("postgres: MarkFirstWrite 要求显式时刻，空时刻会让切换证据失真")
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE storage_cutover SET first_write_at = $1
		WHERE cutover_id = (
			SELECT cutover_id FROM storage_cutover
			ORDER BY cutover_at DESC, cutover_id DESC
			LIMIT 1
		) AND first_write_at IS NULL`, at.UTC().UnixMilli()); err != nil {
		return fmt.Errorf("标记 first_write_at: %w", err)
	}
	return nil
}

// withTx 是仓储写路径的统一入口：走三态协议，并把"回调没报错却没提交"这种实现缺陷
// 显式上抛——**不得**静默当作成功。
// withTx is the repositories' write entry: it never reports a silent non-commit as success.
func (db *DB) withTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	outcome, err := db.Tx(ctx, fn)
	if err != nil {
		return err
	}
	if outcome != TxCommitted {
		return fmt.Errorf("%w: 业务回调未报错但事务结局为 %q", ErrTxOutcomeUnknown, outcome)
	}
	return nil
}
