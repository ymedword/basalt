package postgres

// 本文件是 `diagnostic_events` 的**唯一 SQL 入口**（颗粒度文档 §2.9 / §3.4.1、
// tasks.md T096）。四条纪律：
//
//  1. **写入不做重试、不做事务**：诊断可丢弃，一条事件一行；把它塞进业务事务就等于
//     让"观测失败"回滚"业务成功"（与审计的两阶段写在语义上正好相反）。
//  2. **读取必须有锚点**：`Query.Validate` 先行（消费方端口也在 `diagnostics.Reader`
//     里判一次），存储层再判一次——两层都判是因为本包也可能被别的装配直接调用。
//  3. **次序固定**：`ORDER BY occurred_at, event_id`——同毫秒内靠事件标识定序，
//     两次读取拿到同一份顺序。诊断只承诺**局部有序**，不设跨节点全局序号。
//  4. **`audit_ref` 以 `table:id` 落一列**：三张审计表的 id 都没有表名前缀，单看数字
//     无法回指；读侧经 `diagnostics.ParseAuditRef` 还原成对象。
//  5. **归属两项由写入路径回填**（US-24 C-23）：`worker_instance_id` / `ownership_epoch`
//     取自 `execution_leases` 的当前行，且只在本进程是该行属主时写入——旧属主被接管后
//     迟到的诊断不得被标成新属主。
//
// This file is the only SQL entry point for diagnostic_events: one row per event and no
// business transaction, an anchor enforced twice, a fixed order, and audit_ref carried
// as `table:id` so a bare id can still be traced back to its table. Lease attributes are
// backfilled on the write path and only while this process owns the run lease.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// defaultDiagnosticQueryLimit 是契约 `limit` 的 default（200）。存储层补缺省值只为
// 防"直接调用存储"的路径漏填；端点自己也会补，两处取值以契约为准。
// defaultDiagnosticQueryLimit mirrors the contract default for callers that hit the
// store directly; the endpoint fills the same value from the same source.
const defaultDiagnosticQueryLimit = 200

// diagnosticColumns 是列清单的唯一一份：插入与读取共用，两处各写一份就是两处漂移点。
// The column list has exactly one copy, shared by insert and every read.
const diagnosticColumns = `event_id, schema_version, kind, occurred_at, request_id, run_id,
	task_id, operation_id, ingest_job_id, actor, step_id, attempt_id, parent_step_id, seq,
	reason_code, decision, attrs, audit_ref`

// diagnosticInsert 是单行插入：19 个占位符逐列对应 `diagnosticColumns`。
// diagnosticInsert writes one row; the placeholders line up with diagnosticColumns.
const diagnosticInsert = `INSERT INTO diagnostic_events (` + diagnosticColumns + `)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`

// DiagnosticStore 是 `diagnostic_events` 的仓储实现（消费方端口在 `internal/diagnostics`
// 与 `internal/web`，两者都以结构化方法满足）。
// DiagnosticStore implements the diagnostics write and read ports structurally.
type DiagnosticStore struct {
	db *DB
	// worker 是本进程的 Worker 实例标识（US-24 T032 / C-23）。只在**本进程确实持有该
	// Run 的租约**时给 attrs 补归属两项；零值 = 不补（非 Worker 进程与既有装配）。
	// worker is this process's instance id; the lease attributes are written only while
	// this very process holds the run lease, and a zero value disables the backfill.
	worker string
}

// 编译期断言：本仓储满足诊断包的读写两个端口。
// Compile-time assertions that the store satisfies both diagnostics ports.
var (
	_ diagnostics.EventStore  = (*DiagnosticStore)(nil)
	_ diagnostics.EventSource = (*DiagnosticStore)(nil)
)

// NewDiagnosticStore 绑定库句柄。
// NewDiagnosticStore binds the database handle.
func NewDiagnosticStore(db *DB) *DiagnosticStore { return &DiagnosticStore{db: db} }

// WithWorker 绑定本进程的 Worker 实例标识并返回自身（链式，与其他装配修饰器同形）。
// 只影响写入路径：读面不按它过滤，也不改写历史行。
// WithWorker binds this process's worker instance id; only the write path is affected.
func (s *DiagnosticStore) WithWorker(instanceID string) *DiagnosticStore {
	s.worker = instanceID
	return s
}

// leaseAttrs 读该 Run 的租约归属，并**只在归属者就是本进程时**返回代次。
//
// 为什么必须比对归属者：attrs 里的 `worker_instance_id` 断言的是"本进程写下这条事件"。
// 只按租约行回填会在接管后把旧属主迟到的诊断标成新属主——错误归因比缺属性更糟。
// 归属不符或根本没有租约时 attrs 原样不动，读侧因此看到的是"没有归属"而不是猜测。
// 不看 `released_at`：本进程释放租约之后写下的迟到事件仍由本进程产生，代次也仍是事实。
// leaseAttrs returns the run's lease epoch only when this process owns the lease row.
// Backfilling from the row alone would relabel a stale owner's late events as the new
// owner's; missing attribution is honest, wrong attribution is not. Release state is
// deliberately ignored: a late event from this process is still this process's event.
func (s *DiagnosticStore) leaseAttrs(ctx context.Context, runID string) (int64, bool, error) {
	if s.worker == "" || runID == "" {
		return 0, false, nil
	}
	var owner string
	var epoch int64
	err := s.db.pool.QueryRowContext(ctx, `
SELECT owner_worker_id, ownership_epoch FROM execution_leases
 WHERE kind = $1 AND resource_id = $2`, string(core.LeaseRun), runID).Scan(&owner, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("读取诊断事件的 Run 租约归属: %w", err)
	}
	return epoch, owner == s.worker, nil
}

// InsertEvent 落一条事件。`attrs` 以 JSON 文本写 `jsonb`（脱敏已在写入路径完成，
// 本层不再二次过滤——但也不接受 nil：`jsonb NOT NULL` 要求 `{}`）。
// InsertEvent writes one row; attrs travel as JSON text and a nil map still becomes `{}`
// because the column is NOT NULL.
func (s *DiagnosticStore) InsertEvent(ctx context.Context, ev diagnostics.DiagnosticEvent) error {
	attrs := ev.Attrs
	// 归属两项在这里补齐（C-23）：以**写入时刻**的租约行为准，且必须归属本进程。
	// 查租约失败即放弃这条事件（Recorder 记 WARN + 计数）：诊断可丢弃，但**不得**带着
	// 没有依据的归属落库。
	// The lease attributes are stamped from the row as it stands at write time, and only
	// when this process owns it. A failed lookup drops the event: diagnostics may be
	// dropped, but they must never be stored with unsupported attribution.
	epoch, owned, err := s.leaseAttrs(ctx, ev.Correlation.RunID)
	if err != nil {
		return err
	}
	if owned {
		stamped := make(map[string]any, len(attrs)+2)
		maps.Copy(stamped, attrs)
		stamped["worker_instance_id"] = s.worker
		stamped["ownership_epoch"] = epoch
		attrs = stamped
	}
	if attrs == nil {
		attrs = map[string]any{}
	}
	raw, err := json.Marshal(attrs)
	if err != nil {
		return fmt.Errorf("诊断事件 attrs 序列化失败: %w", err)
	}
	var auditRef any
	if ev.AuditRef != nil {
		auditRef = ev.AuditRef.String()
	}
	var seq any
	if ev.Seq > 0 {
		seq = ev.Seq
	}
	if _, err := s.db.pool.ExecContext(ctx, diagnosticInsert,
		ev.EventID, ev.SchemaVersion, string(ev.Kind), ev.OccurredAt,
		nullIfEmpty(ev.Correlation.RequestID), nullIfEmpty(ev.Correlation.RunID),
		nullIfEmpty(ev.Correlation.TaskID), nullIfEmpty(ev.Correlation.OperationID),
		nullIfEmpty(ev.Correlation.IngestJobID), nullIfEmpty(string(ev.Actor)),
		nullIfEmpty(ev.StepID), nullIfEmpty(ev.AttemptID), nullIfEmpty(ev.ParentStepID),
		seq, nullIfEmpty(string(ev.ReasonCode)), nullIfEmpty(string(ev.Decision)),
		string(raw), auditRef,
	); err != nil {
		return fmt.Errorf("写入诊断事件: %w", err)
	}
	return nil
}

// ListEvents 按锚点读取一页事件：`occurred_at` 升序，同毫秒按 `event_id` 定序。
// 无命中返回空切片而非错误——"没有记录"由端点用 `missing` 显式表达，不自造错误。
// ListEvents reads one page in fixed order; no rows is an empty slice, not an error.
func (s *DiagnosticStore) ListEvents(ctx context.Context, q diagnostics.Query) ([]diagnostics.DiagnosticEvent, error) {
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("读取诊断事件: %w", err)
	}
	limit := q.Limit
	if limit == 0 {
		limit = defaultDiagnosticQueryLimit
	}
	clauses := make([]string, 0, 8)
	args := make([]any, 0, 8)
	add := func(clause, value string) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if q.RequestID != "" {
		add("request_id = $%d", q.RequestID)
	}
	if q.RunID != "" {
		add("run_id = $%d", q.RunID)
	}
	if q.OperationID != "" {
		add("operation_id = $%d", q.OperationID)
	}
	if q.IngestJobID != "" {
		add("ingest_job_id = $%d", q.IngestJobID)
	}
	if q.TaskID != "" {
		add("task_id = $%d", q.TaskID)
	}
	// 会话锚点经 `runs.session_id` 解析：关联不到的记录**不**计入结果（契约逐字）。
	// The session anchor resolves through runs.session_id; unlinked rows never match.
	if q.SessionID != "" {
		add("run_id IN (SELECT run_id FROM runs WHERE session_id = $%d)", q.SessionID)
	}
	if q.Kind != "" {
		add("kind = $%d", string(q.Kind))
	}
	if !q.From.IsZero() {
		args = append(args, q.From.UTC().UnixMilli())
		clauses = append(clauses, fmt.Sprintf("occurred_at >= $%d", len(args)))
	}
	if !q.To.IsZero() {
		args = append(args, q.To.UTC().UnixMilli())
		clauses = append(clauses, fmt.Sprintf("occurred_at <= $%d", len(args)))
	}
	// 锚点由 `Query.Validate` 保证非空，因此 `clauses` 不会为空——`WHERE ` 后面永远
	// 是一条真实条件，不存在"无条件全表扫"的拼装分支。
	// Validate guarantees at least one clause, so there is no anchor-free branch.
	//
	// #nosec G202 -- 拼接的每一段都可追到固定代码，没有一处取值被插进语句文本：
	//   ① `clauses` 只由本函数内八处**固定字面量**（`request_id = $%d` 等）经
	//      `fmt.Sprintf` 填入占位符序号而成，序号就是 `len(args)`，不是调用方数据；
	//   ② 全部用户取值（含会话锚点的子查询参数）都走 `args` 走位置参数；
	//   ③ 唯一非占位符插值是 `LIMIT`，`strconv.Itoa` 生成的整数，且 `Query.Validate`
	//      在此之前已把 `Limit` 限在 1..`diagnostics.MaxQueryLimit`（1000），越界即返回
	//      `ErrQueryLimit`；
	//   ④ 反证用例 `TestDiagnostics_ReadFiltersByCorrelation` 第 ⑧ 组：带引号 / 注释符
	//      的取值只当字面量匹配、返回空页，恒真式不会把别人的事件捞出来。
	// Every fragment is a fixed literal in this function; all values travel as parameters;
	// LIMIT is a validated integer. The hostile-value case is asserted in the integration test.
	stmt := `SELECT ` + diagnosticColumns + ` FROM diagnostic_events WHERE ` +
		strings.Join(clauses, " AND ") + ` ORDER BY occurred_at, event_id LIMIT ` + strconv.Itoa(limit)
	rows, err := s.db.pool.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("读取诊断事件: %w", err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]diagnostics.DiagnosticEvent, 0, 16)
	for rows.Next() {
		var (
			event      diagnostics.DiagnosticEvent
			requestID  sql.NullString
			runID      sql.NullString
			taskID     sql.NullString
			opID       sql.NullString
			ingestID   sql.NullString
			actor      sql.NullString
			stepID     sql.NullString
			attemptID  sql.NullString
			parentStep sql.NullString
			seq        sql.NullInt64
			reasonCode sql.NullString
			decision   sql.NullString
			attrs      []byte
			auditRef   sql.NullString
		)
		if err := rows.Scan(
			&event.EventID, &event.SchemaVersion, &event.Kind, &event.OccurredAt,
			&requestID, &runID, &taskID, &opID, &ingestID, &actor,
			&stepID, &attemptID, &parentStep, &seq, &reasonCode, &decision,
			&attrs, &auditRef,
		); err != nil {
			return nil, fmt.Errorf("读取诊断事件: %w", err)
		}
		event.Correlation = diagnostics.Correlation{
			RequestID: requestID.String, RunID: runID.String, TaskID: taskID.String,
			OperationID: opID.String, IngestJobID: ingestID.String,
		}
		event.Actor = diagnostics.Actor(actor.String)
		event.StepID = stepID.String
		event.AttemptID = attemptID.String
		event.ParentStepID = parentStep.String
		if seq.Valid {
			event.Seq = seq.Int64
		}
		event.ReasonCode = diagnostics.ReasonCode(reasonCode.String)
		event.Decision = diagnostics.Decision(decision.String)
		if len(attrs) > 0 {
			if err := json.Unmarshal(attrs, &event.Attrs); err != nil {
				return nil, fmt.Errorf("读取诊断事件 attrs: %w", err)
			}
		}
		if auditRef.Valid && auditRef.String != "" {
			ref, err := diagnostics.ParseAuditRef(auditRef.String)
			if err != nil {
				return nil, fmt.Errorf("读取诊断事件 audit_ref: %w", err)
			}
			event.AuditRef = &ref
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取诊断事件: %w", err)
	}
	return events, nil
}
