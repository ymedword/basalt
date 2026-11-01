//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// 本文件是**提交结局三态协议**的具名回归（颗粒度文档 §3.1.1 / C-34 / C-13，T122 十二例）。
//
// 每例都在隔离 schema 上跑真库、真 xid：故障钩子只改连接层行为（提交 / 回滚 / 阻塞），
// 核查仍走 `pg_xact_status` 与业务键。逆改点写在每个用例头部——实现改回去必然变红。
//
// Twelve named cases for the tri-state commit protocol. Every case runs against a real
// server with a real xid on an isolated schema; the fault hooks only change connection
// behaviour, never the verification path.

// newTxTestDB 建一个隔离 schema 并建到 v1，再种一个外部调用方 `c1`（预留与用量表的
// 外键目标）。search_path 含扩展 schema 供 `vector(n)` 解析；同时守住"迁移空转"——
// 路径上若已能解析到 `schema_version`，说明会有第二个 Basalt schema 被穿透读到。
//
// newTxTestDB opens an isolated schema migrated to v1 and seeds caller c1.
func newTxTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	base := testPool(t)
	schema := createSchema(t, base)

	conn, err := base.Conn(ctx)
	require.NoError(t, err)
	paths := []string{schema}
	seen := map[string]bool{schema: true}
	for _, ext := range []string{"vector", "pg_trgm"} {
		extSchema := extensionSchema(t, conn, ext)
		if seen[extSchema] {
			continue
		}
		seen[extSchema] = true
		paths = append(paths, extSchema)
	}
	searchPath := strings.Join(paths, ", ")
	_, err = conn.ExecContext(ctx, "SET search_path TO "+searchPath)
	require.NoError(t, err)
	var shadow sql.NullString
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT to_regclass('schema_version')::text`).Scan(&shadow))
	require.False(t, shadow.Valid,
		"search_path 已能解析到 schema_version（%s）——迁移会在别的 schema 上空转", shadow.String)
	require.NoError(t, conn.Close())

	db, err := Open(ctx, Config{DSN: withSearchPath(t, testDSN(t), paths...), ConnectTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = db.pool.ExecContext(ctx, `INSERT INTO callers
		(caller_id, kind, name, project, status, created_at)
		VALUES ('c1', 'service', 'c1', '', 'active', $1)`, time.Now().UTC().UnixMilli())
	require.NoError(t, err)
	return db
}

// reopenDB 用同一份配置重开句柄，模拟进程重启（内存标记、连接全部重来）。
// reopenDB reopens the same schema, simulating a process restart.
func reopenDB(t *testing.T, db *DB) *DB {
	t.Helper()
	clone, err := Open(context.Background(), db.cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, clone.Close()) })
	return clone
}

// seedCutover 插入一条切换记录；seedCutoverAt 返回其 cutover_id。
// seedCutoverAt inserts one cutover row and returns its id.
func seedCutoverAt(t *testing.T, db *DB, at time.Time) string {
	t.Helper()
	id := fmt.Sprintf("cut_%d", at.UnixMilli())
	_, err := db.pool.ExecContext(context.Background(), `INSERT INTO storage_cutover
		(cutover_id, source_schema_version, manifest_sha256, cutover_at, operator)
		VALUES ($1, 6, $2, $3, 'pgtest')`, id, "sha256:"+id, at.UnixMilli())
	require.NoError(t, err)
	return id
}

// seedCutover 用固定时刻插一条切换记录。
// seedCutover inserts a single cutover row at a fixed instant.
func seedCutover(t *testing.T, db *DB) {
	t.Helper()
	seedCutoverAt(t, db, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
}

// firstWriteAt 读**当前** cutover 行的 first_write_at（空值 → nil）。
// firstWriteAt reads first_write_at of the newest cutover row.
func firstWriteAt(t *testing.T, db *DB) *int64 {
	t.Helper()
	var value sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(context.Background(), `
		SELECT first_write_at FROM storage_cutover
		ORDER BY cutover_at DESC, cutover_id DESC LIMIT 1`).Scan(&value))
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

// cutoverFirstWrite 读指定 cutover 行的 first_write_at。
// cutoverFirstWrite reads first_write_at for one cutover row.
func cutoverFirstWrite(t *testing.T, db *DB, cutoverID string) *int64 {
	t.Helper()
	var value sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT first_write_at FROM storage_cutover WHERE cutover_id = $1`,
		cutoverID).Scan(&value))
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

// firstWriteSet 是"当前 cutover 行是否已标记首写"的核查函数。
// firstWriteSet is the verification callback for the first-write marker.
func firstWriteSet() func(context.Context, Queryer) (bool, error) {
	return func(ctx context.Context, q Queryer) (bool, error) {
		var value sql.NullInt64
		if err := q.QueryRowContext(ctx, `
			SELECT first_write_at FROM storage_cutover
			ORDER BY cutover_at DESC, cutover_id DESC LIMIT 1`).Scan(&value); err != nil {
			return false, err
		}
		return value.Valid, nil
	}
}

// hasReservation 是"该调用方的幂等键是否已落预留行"的核查函数。
// hasReservation is the verification callback for a quota reservation row.
func hasReservation(callerID, idempotencyKey string) func(context.Context, Queryer) (bool, error) {
	return func(ctx context.Context, q Queryer) (bool, error) {
		var count int
		if err := q.QueryRowContext(ctx, `
			SELECT count(*) FROM quota_reservations
			WHERE caller_id = $1 AND idempotency_key = $2`,
			callerID, idempotencyKey).Scan(&count); err != nil {
			return false, err
		}
		return count > 0, nil
	}
}

// mustVerifyTx 从未知结局的错误里取出 TxAttempt 并核查，要求核查本身不报错。
// mustVerifyTx extracts the attempt from the unknown-outcome error and verifies it.
func mustVerifyTx(t *testing.T, db *DB, err error,
	check func(context.Context, Queryer) (bool, error),
) TxVerdict {
	t.Helper()
	var unknown *TxUnknownError
	require.ErrorAs(t, err, &unknown, "未知结局必须带回 TxAttempt，核查才有原事务的 xid")
	verdict, verifyErr := db.VerifyTx(context.Background(), unknown.Attempt, check)
	require.NoError(t, verifyErr)
	return verdict
}

// withRuntimeParam 给连接串追加一个会话级 GUC（只读副本用例用它构造
// `default_transaction_read_only=on`）。pgx 把连接串里的未知键当启动包里的
// runtime parameter 下发，值里因此**不得**带空格——`options='-c …=…'` 经 URL
// 编码后空格变 `+`，服务端会把参数名读成 `+default_transaction_read_only`。
// withRuntimeParam appends a session GUC as a startup runtime parameter; the value
// must not contain spaces because the URL encoder turns them into '+'.
func withRuntimeParam(t *testing.T, dsn, key, value string) string {
	t.Helper()
	if !strings.Contains(dsn, "://") {
		return dsn + " " + key + "=" + value
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// TestMarkFirstWrite_RolledBackWithBusinessTx：业务事务报错回滚后**不得**留下首写标记。
// 逆改点：MarkFirstWrite 另开连接（不接 tx）→ 回滚后标记仍在，断言红。
func TestMarkFirstWrite_RolledBackWithBusinessTx(t *testing.T) {
	db := newTxTestDB(t)
	seedCutover(t, db)
	errRollback := errors.New("业务写入失败")
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	outcome, err := db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		if err := db.MarkFirstWrite(ctx, tx, now); err != nil {
			return err
		}
		return errRollback // 同一事务内业务写入失败
	})
	require.ErrorIs(t, err, errRollback)
	require.Equal(t, TxRolledBack, outcome, "fn 报错 + 回滚完成 = 确认回滚（C-34）")
	require.Nil(t, firstWriteAt(t, db), "业务事务回滚不得留下 first_write_at")
}

// TestMarkFirstWrite_UnixMillis：显式时刻必须按 **BIGINT 毫秒** 写入。
// 逆改点：改用 now() / timestamptz → 列类型不符或值语义漂移，断言红。
func TestMarkFirstWrite_UnixMillis(t *testing.T) {
	db := newTxTestDB(t)
	cutoverID := seedCutoverAt(t, db, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	outcome, err := db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		return db.MarkFirstWrite(ctx, tx, now) // 显式时刻 → BIGINT 毫秒
	})
	require.NoError(t, err)
	require.Equal(t, TxCommitted, outcome)
	got := cutoverFirstWrite(t, db, cutoverID)
	require.NotNil(t, got)
	require.Equal(t, now.UnixMilli(), *got, "必须是 BIGINT 毫秒，不得写 now() / timestamptz")
}

// TestMarkFirstWrite_OnlyCurrentCutover：只命中**当前** cutover 行，历史行不得被写。
// 逆改点：WHERE 不带 cutover_id 子查询 → 历史行被改写，断言红。
func TestMarkFirstWrite_OnlyCurrentCutover(t *testing.T) {
	db := newTxTestDB(t)
	oldID := seedCutoverAt(t, db, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	curID := seedCutoverAt(t, db, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))

	outcome, err := db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		return db.MarkFirstWrite(ctx, tx, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	})
	require.NoError(t, err)
	require.Equal(t, TxCommitted, outcome)
	require.Nil(t, cutoverFirstWrite(t, db, oldID), "历史 cutover 行不得被写")
	require.NotNil(t, cutoverFirstWrite(t, db, curID), "只命中当前 cutover 行")
}

// TestMarkFirstWrite_IdempotentAfterRestart：崩溃在提交后、内存标记前，重启后重跑同一
// 幂等语句——已有的 first_write_at **不得**被覆盖。
// 逆改点：去掉 `first_write_at IS NULL` 或改成直接赋值 → 时间戳被后写覆盖，断言红。
func TestMarkFirstWrite_IdempotentAfterRestart(t *testing.T) {
	db := newTxTestDB(t)
	cutoverID := seedCutoverAt(t, db, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC))
	first := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)

	outcome, err := db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		return db.MarkFirstWrite(ctx, tx, first)
	})
	require.NoError(t, err)
	require.Equal(t, TxCommitted, outcome)

	restarted := reopenDB(t, db)
	outcome, err = restarted.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		return restarted.MarkFirstWrite(ctx, tx, second)
	})
	require.NoError(t, err, "重跑同一幂等语句不得报唯一冲突")
	require.Equal(t, TxCommitted, outcome)

	got := cutoverFirstWrite(t, db, cutoverID)
	require.NotNil(t, got)
	require.Equal(t, first.UnixMilli(), *got, "重启后不得覆盖已有的 first_write_at")
}

// TestMarkFirstWrite_ProvablyNotSentRetries：可证明未发送才判确认回滚，并由下一次
// 业务事务补写标记。
// 逆改点：SafeToRetry 也归 TxUnknown → 第一次调用只能走核查，"可安全重试"的判定消失。
func TestMarkFirstWrite_ProvablyNotSentRetries(t *testing.T) {
	db := newTxTestDB(t)
	seedCutover(t, db)
	db.faults.FailNextCommitBeforeSend() // 满足 pgconn.SafeToRetry：请求确定未发出

	outcome, err := db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		return db.MarkFirstWrite(ctx, tx, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	})
	require.Equal(t, TxRolledBack, outcome, "可证明未发送才允许判确认回滚")
	require.Error(t, err)
	require.Nil(t, firstWriteAt(t, db))

	// 未落 → 下一次业务事务补写；这一条才是原"提交失败可重试"的正确形态
	outcome, err = db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		return db.MarkFirstWrite(ctx, tx, time.Date(2026, 10, 26, 13, 0, 0, 0, time.UTC))
	})
	require.NoError(t, err)
	require.Equal(t, TxCommitted, outcome)
	require.NotNil(t, firstWriteAt(t, db))
}

// TestMarkFirstWrite_AckLostIsUnknown：提交已成功但确认丢失，**不得**当已回滚。
// 逆改点：把任何 Commit 错误都当 TxRolledBack → 报告"未落"而标记其实已写；
// 把 TxUnknown 当 TxCommitted → 真正未提交时按成功返回、切换证据缺失。
func TestMarkFirstWrite_AckLostIsUnknown(t *testing.T) {
	db := newTxTestDB(t)
	seedCutover(t, db)
	db.faults.LoseNextCommitAck() // 服务端 COMMIT 成功，客户端收到连接错误

	outcome, err := db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		return db.MarkFirstWrite(ctx, tx, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	})
	require.Equal(t, TxUnknown, outcome, "确认丢失不得判成 TxRolledBack")
	require.ErrorIs(t, err, ErrTxOutcomeUnknown)
	// 服务端其实已提交；Tx 本身不下结论，只上报 Unknown，交给 VerifyTx 核查
	require.Equal(t, TxVerifyCommitted, mustVerifyTx(t, db, err, firstWriteSet()))
	require.NotNil(t, firstWriteAt(t, db), "核查为 committed 后按已提交处理，不得回退或重放")
}

// TestTx_ServerCommitErrorIsRolledBack：服务端在 COMMIT 时明确拒绝（延迟唯一约束），
// 属于**确认回滚**，不必走核查，且必须保留原始 PgError。
// 逆改点：只用 SafeToRetry 二分 → 服务端拒绝被误判成 TxUnknown，正常错误路径白跑核查。
//
// 与设计卡的一处适配：`caller_usage_periods.kind` 的冻结 CHECK 只允许
// `tasks` / `tokens` / `ingest_bytes`（契约 `schema.pg.sql`），故用例插入 `tasks`。
func TestTx_ServerCommitErrorIsRolledBack(t *testing.T) {
	db := newTxTestDB(t)
	seedCutover(t, db)
	db.faults.SeedDeferredUniqueConflict(t) // 延迟唯一约束在 COMMIT 时被服务端拒绝

	outcome, err := db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `INSERT INTO caller_usage_periods
			(caller_id, period_start, kind, updated_at) VALUES ('c1', 0, 'tasks', $1)`,
			time.Now().UTC().UnixMilli())
		return execErr
	})
	require.Equal(t, TxRolledBack, outcome, "服务端明确拒绝即确认回滚，不必走核查")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "服务端错误必须保留原始 PgError，不得包成未知")
	require.Nil(t, firstWriteAt(t, db))
}

// TestTx_CommitAckLostVerifyReadsCommittedRow：先按 xid 定结局，再用业务键复核行。
// 逆改点：用普通 SELECT 查无行直接判未提交 → 提交仍在执行时会被判成"可重试"。
func TestTx_CommitAckLostVerifyReadsCommittedRow(t *testing.T) {
	db := newTxTestDB(t)
	db.faults.LoseNextCommitAck()

	outcome, err := db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `INSERT INTO quota_reservations
			(reservation_id, caller_id, idempotency_key, class, period_start, amount, state, created_at)
			VALUES ('res-r1', 'c1', 'run:r1:admit', 'run-chat', 0, 1, 'pending', $1)`,
			time.Now().UTC().UnixMilli())
		return execErr
	})
	require.Equal(t, TxUnknown, outcome)
	require.ErrorIs(t, err, ErrTxOutcomeUnknown)
	var unknown *TxUnknownError
	require.ErrorAs(t, err, &unknown, "必须带回 TxAttempt，核查才有原事务的 xid")

	// 先按 xid 定结局（committed），再用业务键复核行——两步都做，缺一不可
	verdict, err := db.VerifyTx(context.Background(), unknown.Attempt,
		hasReservation("c1", "run:r1:admit"))
	require.NoError(t, err)
	require.Equal(t, TxVerifyCommitted, verdict,
		"核查必须看到已提交的业务行，而不是凭错误类型猜")
}

// TestTx_VerifyBeforeCommitCompletesIsPending：核查早于原 COMMIT 完成时，"新连接查不到行"
// 不代表未提交，必须停在 Pending。
// 逆改点：跳过 `pg_xact_status` 直接看业务行 → 本用例被判成 Absent 并触发重放。
func TestTx_VerifyBeforeCommitCompletesIsPending(t *testing.T) {
	db := newTxTestDB(t)
	release := db.faults.HoldNextCommit(t) // 延迟唯一约束 + 另一会话占冲突键，COMMIT 卡在服务端
	t.Cleanup(release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 这次调用要等 COMMIT 放行后才返回，因此不能靠它的返回值取 TxAttempt
		_, _ = db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
			_, execErr := tx.ExecContext(ctx, `INSERT INTO quota_reservations
				(reservation_id, caller_id, idempotency_key, class, period_start, amount, state, created_at)
				VALUES ('res-r1', 'c1', 'run:r1:admit', 'run-chat', 0, 1, 'pending', $1)`,
				time.Now().UTC().UnixMilli())
			return execErr
		})
	}()

	// 从故障钩子拿到"正在提交"的那次事务的 TxAttempt（真实实现里它随 TxUnknownError 返回）
	attempt := db.faults.WaitCommitInFlight(t)
	verdict, err := db.VerifyTx(context.Background(), attempt,
		hasReservation("c1", "run:r1:admit"))
	require.Equal(t, TxVerifyPending, verdict,
		"原事务仍在执行：查无行不是未提交的证据，必须保持 Pending")
	require.Error(t, err, "Pending 必须带原因返回，调用方限时等待后冻结前滚并告警")

	release() // 放行 COMMIT
	<-done
	verdict, err = db.VerifyTx(context.Background(), attempt,
		hasReservation("c1", "run:r1:admit"))
	require.NoError(t, err)
	require.Equal(t, TxVerifyCommitted, verdict, "原事务结束后才可定论")
}

// TestTx_VerifyAbortedIsAbsent：只有"服务端已回滚 + 业务键查无行"才可重试。
// 逆改点：把 aborted 也当 committed → 本用例返回 Inconsistent 而不是 Absent。
func TestTx_VerifyAbortedIsAbsent(t *testing.T) {
	db := newTxTestDB(t)
	db.faults.LoseNextCommitAck()
	db.faults.AbortNextCommitAfterAckLoss() // 服务端最终回滚

	_, err := db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `INSERT INTO quota_reservations
			(reservation_id, caller_id, idempotency_key, class, period_start, amount, state, created_at)
			VALUES ('res-r1', 'c1', 'run:r1:admit', 'run-chat', 0, 1, 'pending', $1)`,
			time.Now().UTC().UnixMilli())
		return execErr
	})
	var unknown *TxUnknownError
	require.ErrorAs(t, err, &unknown)

	verdict, err := db.VerifyTx(context.Background(), unknown.Attempt,
		hasReservation("c1", "run:r1:admit"))
	require.NoError(t, err)
	require.Equal(t, TxVerifyAbsent, verdict, "只有 aborted 且查无行才可重试")
}

// TestTx_VerifyCommittedButRowMissingIsInconsistent：已提交却查无业务行 = 冻结并告警，
// **不得**当成未提交去重放。
// 逆改点：committed + 查无行归 Absent → 本用例变成"可重试"，重复副作用随之出现。
func TestTx_VerifyCommittedButRowMissingIsInconsistent(t *testing.T) {
	db := newTxTestDB(t)
	db.faults.LoseNextCommitAck()

	_, err := db.Tx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, execErr := tx.ExecContext(ctx, `SELECT 1`) // 提交成功但没写业务键
		return execErr
	})
	var unknown *TxUnknownError
	require.ErrorAs(t, err, &unknown)

	verdict, err := db.VerifyTx(context.Background(), unknown.Attempt,
		hasReservation("c1", "run:r1:admit"))
	require.ErrorIs(t, err, ErrTxInconsistent)
	require.Equal(t, TxVerifyInconsistent, verdict,
		"已提交却查无业务行：冻结并告警，不得当成未提交去重放")
}

// TestTx_VerifyReadsPrimaryOnly：核查打到只读副本一律报错，不得拿滞后快照下结论。
// 逆改点：去掉主库判定或改用 normalizeDSN 补 target_session_attrs → 本用例退回
// "快照里查无行"而被误判成 Absent。
func TestTx_VerifyReadsPrimaryOnly(t *testing.T) {
	db := newTxTestDB(t) // DSN 已带 target_session_attrs=read-write
	// 指向只读会话：副本 DSN 的典型缺陷正是没声明 target_session_attrs=read-write，
	// SetReplicaDSN 因此故意不经 normalizeDSN。
	db.faults.SetReplicaDSN(t, withRuntimeParam(t, db.cfg.DSN, "default_transaction_read_only", "on"))

	_, err := db.VerifyTx(context.Background(), TxAttempt{XID: 1},
		hasReservation("c1", "run:r1:admit"))
	require.ErrorIs(t, err, ErrPrimaryRequired,
		"核查打到只读副本一律报错，不得拿滞后快照下结论")
}
