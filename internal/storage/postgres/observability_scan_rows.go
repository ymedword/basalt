package postgres

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

func obsDecodeLLM(r rowScanner) (any, error) {
	var f observability.LLMCallFact
	var start, duration, prompt, completion int64
	var recovered sql.NullInt64
	err := r.Scan(&f.ID, &f.RunID, &f.Status, &f.Purpose, &f.ErrorKind, &f.Model, &f.Round, &start, &duration, &recovered, &prompt, &completion)
	f.CreatedAt, f.RecoveredAt = time.UnixMilli(start).UTC(), obsTime(recovered)
	f.ErrorKind = obsReason(f.ErrorKind)
	if f.Status != "unknown" && f.Status != "pending" {
		f.DurationMS, f.PromptTokens, f.CompletionTokens = &duration, &prompt, &completion
	}
	return f, err
}

func obsDecodeTool(r rowScanner) (any, error) {
	var f observability.ToolInvocationFact
	var start int64
	var finish, recovered sql.NullInt64
	err := r.Scan(&f.ID, &f.RunID, &f.ToolCallID, &f.Tool, &f.Status, &f.ErrorKind, &start, &finish, &recovered)
	f.StartedAt, f.FinishedAt, f.RecoveredAt = time.UnixMilli(start).UTC(), obsTime(finish), obsTime(recovered)
	f.ErrorKind = obsReason(f.ErrorKind)
	return f, err
}

func obsDecodeEmbedding(r rowScanner) (any, error) {
	var f observability.EmbeddingCallFact
	var start int64
	var finish sql.NullInt64
	err := r.Scan(&f.ID, &f.RunID, &f.IngestJobID, &f.Status, &f.ErrorKind, &f.Model, &start, &finish)
	f.CreatedAt, f.FinishedAt = time.UnixMilli(start).UTC(), obsTime(finish)
	f.ErrorKind = obsReason(f.ErrorKind)
	return f, err
}

func obsDecodeTask(r rowScanner) (any, error) {
	var f observability.TaskExecutionFact
	var start int64
	var finish sql.NullInt64
	err := r.Scan(&f.ID, &f.TaskID, &f.RunID, &f.Agent, &f.Status, &start, &finish)
	f.StartedAt, f.FinishedAt = time.UnixMilli(start).UTC(), obsTime(finish)
	return f, err
}

func obsDecodeOperation(r rowScanner) (any, error) {
	var f observability.ManagementOperationFact
	var start int64
	var finish, recovered sql.NullInt64
	err := r.Scan(&f.OperationID, &f.Action, &f.Status, &f.Verification, &f.ResultCode, &start, &finish, &recovered)
	f.CreatedAt, f.FinishedAt, f.RecoveredAt = time.UnixMilli(start).UTC(), obsTime(finish), obsTime(recovered)
	return f, err
}

func obsDecodeIngest(r rowScanner) (any, error) {
	var f observability.IngestJobFact
	var start int64
	var finish sql.NullInt64
	err := r.Scan(&f.IngestJobID, &f.Status, &f.Stage, &f.ErrorKind, &f.Attempt, &start, &finish)
	f.StartedAt, f.FinishedAt = time.UnixMilli(start).UTC(), obsTime(finish)
	f.ErrorKind = obsIngestReason(f.ErrorKind)
	return f, err
}

// 只列诊断允许列；即使旧数据attrs含敏感字段，重读后仍走同源脱敏。
// Only diagnostic allowlisted columns are selected, and legacy attributes are redacted again.
const obsDiagnosticSafeColumns = `event_id,schema_version,kind,occurred_at,COALESCE(request_id,''),COALESCE(run_id,''),
	COALESCE(task_id,''),COALESCE(operation_id,''),COALESCE(ingest_job_id,''),COALESCE(actor,''),COALESCE(step_id,''),
	COALESCE(attempt_id,''),COALESCE(parent_step_id,''),COALESCE(seq,0),COALESCE(reason_code,''),COALESCE(decision,''),attrs,COALESCE(audit_ref,'')`

func obsDecodeDiagnostic(r rowScanner) (any, error) {
	var f diagnostics.DiagnosticEvent
	var raw []byte
	var ref string
	if err := r.Scan(&f.EventID, &f.SchemaVersion, &f.Kind, &f.OccurredAt, &f.Correlation.RequestID, &f.Correlation.RunID,
		&f.Correlation.TaskID, &f.Correlation.OperationID, &f.Correlation.IngestJobID, &f.Actor, &f.StepID, &f.AttemptID,
		&f.ParentStepID, &f.Seq, &f.ReasonCode, &f.Decision, &raw, &ref); err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f.Attrs); err != nil {
		return f, err
	}
	if ref != "" {
		value, err := diagnostics.ParseAuditRef(ref)
		if err != nil {
			return f, err
		}
		f.AuditRef = &value
	}
	return f.Redacted(diagnostics.NewWhitelistRedactor()), nil
}
