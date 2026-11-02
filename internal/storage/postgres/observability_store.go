package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/observability"
)

// ObservabilityStore 实现只读事实投影与独立遥测账本；不执行业务任务。
// ObservabilityStore adapts evidence and telemetry ledgers without executing business work.
type ObservabilityStore struct {
	now func() time.Time
	db  *DB
}

// NewObservabilityStore 复用既有句柄，不新开第二套数据库或连接配置。
// NewObservabilityStore reuses the runtime database handle.
func NewObservabilityStore(db *DB) *ObservabilityStore { return &ObservabilityStore{db: db} }

// WithClock 只绑定观测派生时间，返回同一DB的只读适配副本，不改业务时钟。
// WithClock binds observation time on a copy of the same DB adapter without changing business time.
func (s *ObservabilityStore) WithClock(clock func() time.Time) *ObservabilityStore {
	if s == nil {
		return nil
	}
	copy := *s
	copy.now = clock
	return &copy
}

func (s *ObservabilityStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return s.db.clock()
}

var (
	_ observability.RunFactsReader        = (*ObservabilityStore)(nil)
	_ observability.ExecutionFactsReader  = (*ObservabilityStore)(nil)
	_ observability.TaskFactsReader       = (*ObservabilityStore)(nil)
	_ observability.OperationFactsReader  = (*ObservabilityStore)(nil)
	_ observability.IngestFactsReader     = (*ObservabilityStore)(nil)
	_ observability.AssemblyFactsReader   = (*ObservabilityStore)(nil)
	_ observability.DiagnosticFactsReader = (*ObservabilityStore)(nil)
	_ observability.CleanupFactsReader    = (*ObservabilityStore)(nil)
)

// obsRows 所有读集合都有SQL固定上限，读取错误不降级为空结果。
// obsRows preserves read errors and closes bounded row sets.
func obsRows[T any](ctx context.Context, db *DB, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	if db == nil || db.pool == nil {
		return nil, errors.New("observability database unavailable")
	}
	rows, err := db.pool.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query observability facts: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.WarnContext(ctx, "observability rows close failed", "error_kind", "query_failed")
		}
	}()
	out := make([]T, 0)
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("decode observability facts: %w", err)
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read observability rows: %w", err)
	}
	return out, nil
}

func obsTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	instant := time.UnixMilli(value.Int64).UTC()
	return &instant
}

const obsRunColumns = `run_id,COALESCE(request_id,''),COALESCE(session_id,''),agent,status,
	COALESCE(reason,''),trigger,started_at,finished_at,iterations,tool_calls`

func obsScanRun(row rowScanner) (observability.RunFacts, error) {
	var out observability.RunFacts
	var start int64
	var finish sql.NullInt64
	err := row.Scan(&out.RunID, &out.RequestID, &out.SessionID, &out.Agent, &out.Status,
		&out.Reason, &out.Trigger, &start, &finish, &out.Rounds, &out.ToolCalls)
	out.StartedAt = time.UnixMilli(start).UTC()
	out.FinishedAt = obsTime(finish)
	// runs.reason可含内部原文，OBS只公开固定原因分类。
	// Run reasons may contain raw errors; expose only fixed classifications.
	out.ContextBudget = obsContextBudget(out.Reason)
	out.Reason = obsRunReason(out.Reason)
	return out, err
}

func obsReason(value string) string {
	switch value {
	case "", "timeout", "cancelled", "unknown", "external_error", "runtime_error", "notify_disabled", "invalid_input", "sandbox_denied", "not_found", "audit_write_failed", "llm_call_failed", "iteration_limit", "tool_call_limit", "run_timeout", "quota_exhausted", "concurrency_limited":
		return value
	default:
		return "unclassified"
	}
}

func obsRunReason(value string) string {
	if obsContextBudget(value) != nil {
		return "context_budget_exceeded"
	}
	switch value {
	case core.ReasonToolBudget:
		return "tool_call_limit"
	case core.ReasonIterationLimit:
		return "iteration_limit"
	case "超过运行时限":
		return "run_timeout"
	case "context canceled":
		return "cancelled"
	default:
		return obsReason(value)
	}
}

// Match only the exact message emitted by core/prompt.go. Historical arbitrary
// errors remain unclassified; numeric captures do not disclose raw error text.
var obsContextBudgetPattern = regexp.MustCompile(`^上下文超限：不可裁部分估算 ([0-9]{1,12}) tokens 超过 budget\.context_tokens=([0-9]{1,12})（含余量 ([0-9]{1,12})）$`)

func obsContextBudget(value string) *observability.ContextBudgetFailure {
	matches := obsContextBudgetPattern.FindStringSubmatch(value)
	if matches == nil {
		return nil
	}
	required, _ := strconv.ParseInt(matches[1], 10, 64)
	limit, _ := strconv.ParseInt(matches[2], 10, 64)
	reserve, _ := strconv.ParseInt(matches[3], 10, 64)
	if limit <= 0 || required+reserve <= limit {
		return nil
	}
	return &observability.ContextBudgetFailure{Required: required, Limit: limit, Reserve: reserve}
}

func obsIngestReason(value string) string {
	if value == "" || slices.Contains(knowledge.ErrorKinds(), value) {
		return value
	}
	return "unclassified"
}

// RunFacts 读取一个运行的允许列，不读取user/caller身份或消息。
// RunFacts reads safe columns without user identities or message bodies.
func (s *ObservabilityStore) RunFacts(ctx context.Context, runID string) (observability.RunFacts, error) {
	out, err := obsRows(ctx, s.db, "SELECT "+obsRunColumns+" FROM runs WHERE run_id=$1", []any{runID}, func(rows *sql.Rows) (observability.RunFacts, error) { return obsScanRun(rows) })
	if err != nil {
		return observability.RunFacts{}, err
	}
	if len(out) == 0 {
		return observability.RunFacts{}, core.ErrRunNotFound
	}
	return out[0], nil
}

func (s *ObservabilityStore) runIDs(ctx context.Context, query, value string) ([]string, error) {
	return obsRows(ctx, s.db, query, []any{value}, func(rows *sql.Rows) (string, error) {
		var id string
		err := rows.Scan(&id)
		return id, err
	})
}

// RunsByRequestID 只返回真实持久化请求桥；无历史反推。
// RunsByRequestID returns only recorded request associations.
func (s *ObservabilityStore) RunsByRequestID(ctx context.Context, requestID string) ([]string, error) {
	return s.runIDs(ctx, `SELECT run_id FROM runs WHERE request_id=$1 ORDER BY started_at,run_id LIMIT 10001`, requestID)
}

// RunsBySessionID 有界展开会话，后续时间线仍逐Run编号。
// RunsBySessionID bounds session expansion without merging run identity.
func (s *ObservabilityStore) RunsBySessionID(ctx context.Context, sessionID string) ([]string, error) {
	return s.runIDs(ctx, `SELECT run_id FROM runs WHERE session_id=$1 ORDER BY started_at,run_id LIMIT 10001`, sessionID)
}

// RunsByAgentWindow 使用明确时间窗和固定上限，不提供无界列表。
// RunsByAgentWindow requires an explicit window and applies a fixed bound.
func (s *ObservabilityStore) RunsByAgentWindow(ctx context.Context, agent string, from, to time.Time) ([]observability.RunFacts, error) {
	if from.IsZero() || to.IsZero() || from.After(to) {
		return nil, observability.ErrInvalidQuery
	}
	return obsRows(ctx, s.db, "SELECT "+obsRunColumns+` FROM runs WHERE started_at BETWEEN $1 AND $2
		AND ($3='' OR agent=$3) ORDER BY started_at DESC,run_id DESC LIMIT 10001`, []any{from.UnixMilli(), to.UnixMilli(), agent}, func(rows *sql.Rows) (observability.RunFacts, error) { return obsScanRun(rows) })
}

// LLMCallsByRun 不读取供应商原始错误或提示词。
// LLMCallsByRun excludes provider error text and prompts.
func (s *ObservabilityStore) LLMCallsByRun(ctx context.Context, runID string) ([]observability.LLMCallFact, error) {
	return obsRows(ctx, s.db, `SELECT id,run_id,status,purpose,COALESCE(error_kind,''),model,round,
		created_at,duration_ms,recovered_at,prompt_tokens,completion_tokens FROM llm_calls
		WHERE run_id=$1 ORDER BY created_at,id LIMIT 10001`, []any{runID}, func(rows *sql.Rows) (observability.LLMCallFact, error) {
		var out observability.LLMCallFact
		var created, duration, prompt, completion int64
		var recovered sql.NullInt64
		err := rows.Scan(&out.ID, &out.RunID, &out.Status, &out.Purpose, &out.ErrorKind, &out.Model, &out.Round, &created, &duration, &recovered, &prompt, &completion)
		out.CreatedAt, out.RecoveredAt = time.UnixMilli(created).UTC(), obsTime(recovered)
		out.ErrorKind = obsReason(out.ErrorKind)
		if out.Status != "pending" && out.Status != "unknown" {
			out.DurationMS, out.PromptTokens, out.CompletionTokens = &duration, &prompt, &completion
		}
		return out, err
	})
}

// ToolInvocationsByRun 不读取工具入参、结果或原始错误。
// ToolInvocationsByRun excludes tool arguments, results, and raw errors.
func (s *ObservabilityStore) ToolInvocationsByRun(ctx context.Context, runID string) ([]observability.ToolInvocationFact, error) {
	return obsRows(ctx, s.db, `SELECT id,run_id,COALESCE(tool_call_id,''),tool_name,status,
		COALESCE(error_kind,''),started_at,finished_at,recovered_at,duration_ms FROM tool_invocations
		WHERE run_id=$1 ORDER BY started_at,id LIMIT 10001`, []any{runID}, func(rows *sql.Rows) (observability.ToolInvocationFact, error) {
		var out observability.ToolInvocationFact
		var start int64
		var finish, recovered sql.NullInt64
		var duration sql.NullInt64
		err := rows.Scan(&out.ID, &out.RunID, &out.ToolCallID, &out.Tool, &out.Status, &out.ErrorKind, &start, &finish, &recovered, &duration)
		out.StartedAt, out.FinishedAt, out.RecoveredAt = time.UnixMilli(start).UTC(), obsTime(finish), obsTime(recovered)
		if duration.Valid && (out.Status == "success" || out.Status == "failed") {
			out.DurationMS = &duration.Int64
		}
		out.ErrorKind = obsReason(out.ErrorKind)
		return out, err
	})
}

const obsEmbeddingColumns = `id,COALESCE(run_id,''),COALESCE(ingest_job_id,''),status,
	COALESCE(error_kind,''),model,created_at,finished_at`

func obsScanEmbedding(rows *sql.Rows) (observability.EmbeddingCallFact, error) {
	var out observability.EmbeddingCallFact
	var start int64
	var finish sql.NullInt64
	err := rows.Scan(&out.ID, &out.RunID, &out.IngestJobID, &out.Status, &out.ErrorKind, &out.Model, &start, &finish)
	out.CreatedAt, out.FinishedAt = time.UnixMilli(start).UTC(), obsTime(finish)
	out.ErrorKind = obsReason(out.ErrorKind)
	return out, err
}

// EmbeddingCallsByRun 读取Run锚点的向量调用。
// EmbeddingCallsByRun reads embedding calls anchored to a run.
func (s *ObservabilityStore) EmbeddingCallsByRun(ctx context.Context, runID string) ([]observability.EmbeddingCallFact, error) {
	return obsRows(ctx, s.db, "SELECT "+obsEmbeddingColumns+` FROM embedding_calls WHERE run_id=$1 ORDER BY created_at,id LIMIT 10001`, []any{runID}, obsScanEmbedding)
}

// EmbeddingCallsByIngest 读取摄取锚点，不伪造Run。
// EmbeddingCallsByIngest reads ingestion calls without inventing a run.
func (s *ObservabilityStore) EmbeddingCallsByIngest(ctx context.Context, ingestID string) ([]observability.EmbeddingCallFact, error) {
	return obsRows(ctx, s.db, "SELECT "+obsEmbeddingColumns+` FROM embedding_calls WHERE ingest_job_id=$1 ORDER BY created_at,id LIMIT 10001`, []any{ingestID}, obsScanEmbedding)
}

const obsTaskColumns = `e.id,e.task_id,COALESCE(e.run_id,''),t.agent_name,e.status,e.started_at,e.finished_at`

// UI explanations may reference a Run; export keeps source-only facts and hashes.
const obsTaskReadColumns = obsTaskColumns + `,COALESCE((SELECT r.reason FROM runs r WHERE r.run_id=e.run_id),'')`

func obsScanTask(rows *sql.Rows) (observability.TaskExecutionFact, error) {
	var out observability.TaskExecutionFact
	var start int64
	var finish sql.NullInt64
	var runReason string
	err := rows.Scan(&out.ID, &out.TaskID, &out.RunID, &out.Agent, &out.Status, &start, &finish, &runReason)
	out.StartedAt, out.FinishedAt = time.UnixMilli(start).UTC(), obsTime(finish)
	if out.Status == "failed" && runReason != "" {
		out.ErrorKind, out.ReasonFromRun = obsRunReason(runReason), true
	}
	return out, err
}

// TaskExecutionFacts 只读任务执行及关联，不读取error_message。
// TaskExecutionFacts reads task execution without raw errors.
func (s *ObservabilityStore) TaskExecutionFacts(ctx context.Context, taskID string, from, to time.Time) ([]observability.TaskExecutionFact, error) {
	return obsRows(ctx, s.db, "SELECT "+obsTaskReadColumns+` FROM task_executions e JOIN scheduled_tasks t USING(task_id)
		WHERE e.task_id=$1 AND ($2::bigint=0 OR e.started_at >= $2) AND ($3::bigint=0 OR e.started_at <= $3)
		ORDER BY e.started_at,e.id LIMIT 10001`, []any{taskID, obsMillis(from), obsMillis(to)}, obsScanTask)
}

// TaskExecutionByRun 保留真实任务归属。
// TaskExecutionByRun preserves recorded task ownership.
func (s *ObservabilityStore) TaskExecutionByRun(ctx context.Context, runID string) ([]observability.TaskExecutionFact, error) {
	return obsRows(ctx, s.db, "SELECT "+obsTaskReadColumns+` FROM task_executions e JOIN scheduled_tasks t USING(task_id)
		WHERE e.run_id=$1 ORDER BY e.started_at,e.id LIMIT 10001`, []any{runID}, obsScanTask)
}

func obsMillis(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixMilli()
}

// OperationFacts 不读取detail_json、actor或原始错误。
// OperationFacts excludes operation details, actors, and raw errors.
func (s *ObservabilityStore) OperationFacts(ctx context.Context, id string) (observability.ManagementOperationFact, error) {
	values, err := obsRows(ctx, s.db, `SELECT operation_id,action,status,COALESCE(verification,''),
		COALESCE(result_code,''),created_at,finished_at,recovered_at FROM management_operations WHERE operation_id=$1`, []any{id}, func(rows *sql.Rows) (observability.ManagementOperationFact, error) {
		var out observability.ManagementOperationFact
		var start int64
		var finish, recovered sql.NullInt64
		err := rows.Scan(&out.OperationID, &out.Action, &out.Status, &out.Verification, &out.ResultCode, &start, &finish, &recovered)
		out.CreatedAt, out.FinishedAt, out.RecoveredAt = time.UnixMilli(start).UTC(), obsTime(finish), obsTime(recovered)
		return out, err
	})
	if err != nil {
		return observability.ManagementOperationFact{}, err
	}
	if len(values) == 0 {
		return observability.ManagementOperationFact{}, core.ErrOperationNotFound
	}
	return values[0], nil
}

// IngestJobFacts 不读取文档/版本标题和正文。
// IngestJobFacts excludes document titles and contents.
func (s *ObservabilityStore) IngestJobFacts(ctx context.Context, id string) (observability.IngestJobFact, error) {
	values, err := obsRows(ctx, s.db, `SELECT ingest_job_id,status,stage,COALESCE(error_kind,''),attempt,started_at,finished_at
		FROM kb_ingest_jobs WHERE ingest_job_id=$1`, []any{id}, func(rows *sql.Rows) (observability.IngestJobFact, error) {
		var out observability.IngestJobFact
		var start int64
		var finish sql.NullInt64
		err := rows.Scan(&out.IngestJobID, &out.Status, &out.Stage, &out.ErrorKind, &out.Attempt, &start, &finish)
		out.StartedAt, out.FinishedAt = time.UnixMilli(start).UTC(), obsTime(finish)
		out.ErrorKind = obsIngestReason(out.ErrorKind)
		return out, err
	})
	if err != nil {
		return observability.IngestJobFact{}, err
	}
	if len(values) == 0 {
		return observability.IngestJobFact{}, fs.ErrNotExist
	}
	return values[0], nil
}

// ListEvents 复用既有读取后重脱敏，不改变旧诊断查询的固定排序。
// ListEvents reuses the redacting reader and preserves existing query order.
func (s *ObservabilityStore) ListEvents(ctx context.Context, q diagnostics.Query) ([]diagnostics.DiagnosticEvent, error) {
	return diagnostics.NewReader(NewDiagnosticStore(s.db), diagnostics.DefaultConfig(), nil).List(ctx, q)
}

// ListObservationEvents 在Reader边界脱敏并只携带隐藏数量；不扩展v0事件契约。
// ListObservationEvents redacts at the reader boundary and carries only an omission count.
func (s *ObservabilityStore) ListObservationEvents(ctx context.Context, q diagnostics.Query) ([]observability.DiagnosticReadEvent, error) {
	rows, err := NewDiagnosticStore(s.db).ListEvents(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]observability.DiagnosticReadEvent, 0, len(rows))
	for _, raw := range rows {
		e := raw.Redacted(nil)
		out = append(out, observability.DiagnosticReadEvent{Event: e, HiddenFields: max(0, len(raw.Attrs)-len(e.Attrs))})
	}
	return out, nil
}

// AssemblyByRun 读取ID、分数与决定；正文不在投影列中。
// AssemblyByRun reads identifiers and decisions, never content bodies.
func (s *ObservabilityStore) AssemblyByRun(ctx context.Context, runID string) ([]observability.AssemblyRecordFact, error) {
	return obsRows(ctx, s.db, "SELECT "+assemblyRecordColumns+` FROM context_assembly_records
		WHERE run_id=$1 ORDER BY created_at,record_id LIMIT 10001`, []any{runID}, obsScanAssembly)
}

func obsScanAssembly(rows *sql.Rows) (observability.AssemblyRecordFact, error) {
	var out observability.AssemblyRecordFact
	var budget, candidates, hits, decisions, reasons, compression, trim, segments, retrieval []byte
	var call sql.NullInt64
	var created int64
	err := rows.Scan(&out.RecordID, &out.RunID, &out.StepID, &call, &out.Purpose, &out.Agent,
		&out.PromptVersion, &out.ModelVersion, &budget, &out.EstimatorVersion, &candidates, &hits,
		&decisions, &reasons, &compression, &trim, &segments, &created, &retrieval)
	if err != nil {
		return out, err
	}
	out.LLMCallID, out.CreatedAt = call.Int64, time.UnixMilli(created).UTC()
	var b assemblyBudgetJSON
	var cs []assemblyCandidateJSON
	var cp assemblyCompressionJSON
	var ht assemblyHistoryTrimJSON
	for _, decode := range []struct {
		raw    []byte
		target any
	}{
		{budget, &b},
		{candidates, &cs},
		{hits, &out.Hits},
		{decisions, &out.Decisions},
		{reasons, &out.ReasonCodes},
		{compression, &cp},
		{trim, &ht},
		{segments, &out.FinalSegments},
		{retrieval, &out.Retrieval},
	} {
		if len(decode.raw) > 0 {
			if err := json.Unmarshal(decode.raw, decode.target); err != nil {
				return out, fmt.Errorf("decode assembly evidence: %w", err)
			}
		}
	}
	out.Budget = core.ContextBudget{ContextTokens: b.ContextTokens, ProtocolReserve: b.ProtocolReserve, OutputReserve: b.OutputReserve, EstimatorVersion: b.EstimatorVersion}
	out.BudgetRecorded = out.Purpose == core.PurposeChat && b.ContextTokens > 0
	for _, c := range cs {
		out.Candidates = append(out.Candidates, core.AssemblyCandidate{
			ChunkID: c.ChunkID, VersionID: c.VersionID, ContentHash: c.ContentHash,
			TokenEstimate: c.TokenEstimate, RetrievalScore: c.RetrievalScore, RerankScore: c.RerankScore, Rank: c.Rank, ACLDecision: c.ACLDecision, Decision: c.Decision,
		})
	}
	out.Compression = core.CompressionRecord{
		Enabled: cp.Enabled, Decision: core.CompressionDecision(cp.Decision), CompactionID: cp.CompactionID,
		CoveredFromMessageID: cp.CoveredFromMessageID, CutMessageID: cp.CutMessageID, FirstKeptMessageID: cp.FirstKeptMessageID,
		StrategyVersion: cp.StrategyVersion, PromptVersion: cp.PromptVersion, TokensBefore: cp.TokensBefore, TokensAfter: cp.TokensAfter, ReasonCode: cp.ReasonCode,
	}
	out.HistoryTrim = core.HistoryTrimRecord{
		DroppedGroups: ht.DroppedGroups, FirstDroppedMessageID: ht.FirstDroppedMessageID,
		CutMessageID: ht.CutMessageID, ReasonCode: ht.ReasonCode, TokensBefore: ht.TokensBefore, TokensAfter: ht.TokensAfter, StrategyVersion: ht.StrategyVersion,
	}
	return out, nil
}

// CleanupRuns 只返回有界窗口账本，不声称有逐条删除墓碑。
// CleanupRuns returns bounded window evidence without claiming per-row tombstones.
func (s *ObservabilityStore) CleanupRuns(ctx context.Context, from, to time.Time) ([]observability.CleanupRun, error) {
	return obsRows(ctx, s.db, `SELECT cleanup_id,table_kind,window_start,window_end,scan_covered_from,scan_covered_through,
		deleted_count,skipped_count,COALESCE(skip_reason,''),status,COALESCE(error_kind,''),started_at,finished_at
		FROM observability_cleanup_runs WHERE ($1::bigint=0 OR window_end >= $1) AND ($2::bigint=0 OR window_start <= $2)
		ORDER BY started_at,cleanup_id LIMIT 10001`, []any{obsMillis(from), obsMillis(to)}, func(rows *sql.Rows) (observability.CleanupRun, error) {
		var out observability.CleanupRun
		var start, end, started int64
		var coveredFrom, coveredTo, finished sql.NullInt64
		err := rows.Scan(&out.CleanupID, &out.TableKind, &start, &end, &coveredFrom, &coveredTo,
			&out.Deleted, &out.Skipped, &out.SkipReason, &out.Status, &out.ErrorKind, &started, &finished)
		out.WindowStart, out.WindowEnd, out.StartedAt = time.UnixMilli(start).UTC(), time.UnixMilli(end).UTC(), time.UnixMilli(started).UTC()
		out.ScanCoveredFrom, out.ScanCoveredThrough, out.FinishedAt = obsTime(coveredFrom), obsTime(coveredTo), obsTime(finished)
		out.ErrorKind = obsCleanupErrorKind(out.ErrorKind)
		return out, err
	})
}
