package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 runs 的**唯一 SQL 入口**。表 / 列 / 状态字面量以 docs/contracts/schema.sql 为准。
// 三条纪律。① **起始态不由调用方给**：`StartRun` 收的 `RunRecord` 没有状态字段，`running`
// 由本文件的 SQL 固定写——与审计预写同一手法，调用方也就没有传错的可能。② **不接收
// finalText**：`runs` 表没有该列（§3.4 列级口径），最终回答只存在 `RunResult` 与会话的
// assistant 消息里。③ **终态只覆盖仍未结束的行**：正常收口走 `FinishRun`（同一进程内恒
// 一次），崩溃收尾走 `closeRunning`（`WHERE status = 'running'`），两条路都不会把已判定的
// 终态改口。
// This file is the only SQL entry point for runs: the row starts as running by SQL literal,
// no final text anywhere, and terminal state is written by exactly one of two paths —
// FinishRun in-process, closeRunning under a status guard for crash recovery.

// 四个 `trigger` 字面量（契约 §5 / §6.2 固定口径 8）。列名与表名一律内联在 SQL 字面量里
// （与 llm_calls.go 同风格）——不另立列清单常量，免得出现"第二份列定义"与 DDL 漂移。
// The four trigger literals; column names stay inline in the SQL for the same reason the
// rest of the package does: no second column list to drift from the DDL.
const (
	triggerUser   = "user"
	triggerAPI    = "api"
	triggerCron   = "cron"
	triggerManual = "manual"
)

// RunStore 是 runs 的唯一 SQL 入口（core.RunStore 的实现）。
// RunStore is the only SQL entry point for runs.
type RunStore struct{ db *DB }

// 编译期断言：RunStore 满足消费方（core）声明的运行账本接口。
// Compile-time assertion that RunStore satisfies the consumer-declared interface.
var _ core.RunStore = (*RunStore)(nil)

// NewRunStore 绑定一个已打开的写模式库句柄。
// NewRunStore binds the store to an opened writable database handle.
func NewRunStore(db *DB) *RunStore { return &RunStore{db: db} }

// StartRun 建 `status = 'running'` 的账本行；失败 = 调用方**拒绝启动**该次运行。
// StartRun inserts the running ledger row; a failure means the caller refuses to start.
//
// `trigger` 按渠道映射（§6 固定口径 8：cli → user、web → api、scheduler → cron、
// admin → manual）。映射是**显式表**而非推断：不认识的渠道返回错误，不落一行猜出来的
// 记账——`trigger` 列承担"区分定时与手动补跑"的审计职责（契约 §5），猜错比拒绝启动更糟。
// The trigger mapping is an explicit table; an unknown channel is refused rather than
// guessed, because the column carries audit meaning.
func (s *RunStore) StartRun(ctx context.Context, rec core.RunRecord) error {
	trigger, err := triggerFor(rec.Channel)
	if err != nil {
		return err
	}

	// 单条 INSERT 即一个短事务；reason / finished_at 留 NULL（终态才写），
	// iterations / tool_calls 取列缺省 0（还没跑，不是"跑了零次"）。
	_, err = s.db.write.ExecContext(ctx,
		`INSERT INTO runs
		   (run_id, session_id, agent, channel, user_id, trigger, status,
		    reason, iterations, tool_calls, config_hash, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, 0, 0, ?, ?, NULL)`,
		rec.RunID, nullIfEmpty(rec.SessionID), rec.Agent, rec.Channel, rec.UserID,
		trigger, string(core.StatusRunning), nullIfEmpty(rec.ConfigHash), rec.StartedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("预写运行账本行（run_id=%s）: %w", rec.RunID, err)
	}
	return nil
}

// FinishRun 把终态写回同一行（status / reason / iterations / tool_calls / finished_at）。
// FinishRun writes the terminal state onto the same row.
//
// 与审计回填不同，这里**不**加 `status = 'running'` 前置条件：本路径的调用方是运行入口，
// 一次运行只收口一次（防重入由调用形状保证）；崩溃收尾是另一条带条件的路径（`closeRunning`），
// 两条职责分明的语句比一条兼两者的语句更好审。行不存在（影响 0 行）**如实报错**——账本行
// 缺失说明启动纪律被绕过，这种状态不该被"更新成功"掩盖。
// A missing row is an error: it means the ledger row was never written, and that must not
// be masked by a silent success.
func (s *RunStore) FinishRun(ctx context.Context, runID string, status core.RunStatus, reason string,
	iterations, toolCalls int, now time.Time,
) error {
	res, err := s.db.write.ExecContext(ctx,
		`UPDATE runs
		    SET status = ?, reason = ?, iterations = ?, tool_calls = ?, finished_at = ?
		  WHERE run_id = ?`,
		string(status), nullIfEmpty(reason), iterations, toolCalls, now.UnixMilli(), runID)
	if err != nil {
		return fmt.Errorf("回填运行终态（run_id=%s）: %w", runID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认运行终态写入（run_id=%s）: %w", runID, err)
	}
	if affected == 0 {
		return fmt.Errorf("回填运行终态（run_id=%s）: 账本行不存在", runID)
	}
	return nil
}

// listRunning 扫出仍为 `running` 的行（崩溃恢复步骤 ④ 的取数面，§2.3 流程 G）。
// **读的是状态列**——"组看消息、Run 看表"里的表侧判据就在这里。
// listRunning scans the rows still in the running state, the table-side judgement of
// crash recovery.
func (s *RunStore) listRunning(ctx context.Context) ([]core.RunRecord, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT run_id, session_id, agent, channel, user_id, config_hash, started_at
		   FROM runs WHERE status = ? ORDER BY started_at, run_id`, string(core.StatusRunning))
	if err != nil {
		return nil, fmt.Errorf("扫描仍为 running 的运行行: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.RunRecord
	for rows.Next() {
		var (
			rec       core.RunRecord
			sessionID sql.NullString
			configHsh sql.NullString
			startedAt int64
		)
		if err := rows.Scan(&rec.RunID, &sessionID, &rec.Agent, &rec.Channel, &rec.UserID,
			&configHsh, &startedAt); err != nil {
			return nil, fmt.Errorf("读仍为 running 的运行行: %w", err)
		}
		rec.SessionID = sessionID.String
		rec.ConfigHash = configHsh.String
		rec.StartedAt = time.UnixMilli(startedAt).UTC()
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描仍为 running 的运行行: %w", err)
	}
	return out, nil
}

// closeRunning 把**仍为 `running`** 的行收尾为 `failed`（崩溃恢复步骤 ③/④，§2.3 流程 G）：
// 进程没了而账本还写着 running，就是"进程中断"。
// closeRunning closes the rows that are *still* running as failed.
//
// `WHERE status = 'running'` 是这条语句的全部意义：已结束的行（completed / limit_reached /
// cancelled / failed）**不得**被覆盖——恢复扫描可能在任何时刻重跑，幂等性靠它保证。
// `ex` 收执行面而非库句柄：恢复步骤 ③ 要在**同一个短事务**里连组修复一起提交。
// The status guard is the whole point: rerunning recovery must find nothing left to close,
// and terminal rows must never be re-judged. The executor parameter lets recovery step ③
// close runs inside the same short transaction as the group repair.
func (s *RunStore) closeRunning(ctx context.Context, ex sqlExecutor, reason string, now time.Time) (int, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE runs
		    SET status = ?, reason = ?, finished_at = ?
		  WHERE status = ?`,
		string(core.StatusFailed), reason, now.UnixMilli(), string(core.StatusRunning))
	if err != nil {
		return 0, fmt.Errorf("收尾仍为 running 的运行行: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("确认仍为 running 的运行行收尾: %w", err)
	}
	return int(affected), nil
}

// sqlExecutor 是 `*sql.DB` 与 `*sql.Tx` 的公共执行面：恢复路径要在短事务内复用仓储的
// 单写语句（**不**给事务另写第二份 SQL——两份同义 SQL 就是第二处漂移点）。
// sqlExecutor is the shared execution surface of *sql.DB and *sql.Tx so recovery can reuse
// repository statements inside its transaction instead of duplicating them.
type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// triggerFor 按渠道映射 `runs.trigger`（§6 固定口径 8）。未知渠道 → 错误（见 StartRun 注释）。
// triggerFor maps a channel onto the trigger column; unknown channels are refused.
func triggerFor(channel string) (string, error) {
	switch channel {
	case "cli":
		return triggerUser, nil
	case "web":
		return triggerAPI, nil
	case "scheduler":
		return triggerCron, nil
	case "admin":
		return triggerManual, nil
	default:
		return "", fmt.Errorf("预写运行账本行: 不认识的渠道 %q（无法映射 runs.trigger，§6 固定口径 8）", channel)
	}
}
