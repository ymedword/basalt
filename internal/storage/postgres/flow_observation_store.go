package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// ObserveNode projects original Run/messages/call audit facts after the real
// synchronous Run returns. It cannot create or complete any of those records.
func (s *FlowStore) ObserveNode(ctx context.Context, p core.Principal, id, attemptID string) (flow.NodeRunResult, error) {
	var result flow.NodeRunResult
	if _, err := s.Read(ctx, p, id); err != nil {
		return result, err
	}
	var runID string
	err := s.db.pool.QueryRowContext(ctx, `SELECT COALESCE(n.run_id,'') FROM flow_node_attempts n JOIN runs r ON r.run_id=n.run_id AND r.flow_task_id=n.task_id AND r.flow_node_attempt_id=n.node_attempt_id WHERE n.task_id=$1 AND n.node_attempt_id=$2 AND r.caller_id=$3`, id, attemptID, p.CallerID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return result, core.ErrRunNotFound
	}
	if err != nil {
		return result, err
	}
	detail, err := NewRunStore(s.db).GetRun(ctx, runID)
	if err != nil {
		return result, err
	}
	result.Run = core.RunResult{RunID: runID, Status: detail.Status, FinalText: detail.FinalText, Reason: detail.Reason, Iterations: detail.Iterations, ToolCalls: detail.ToolCalls, ConfigHash: detail.ConfigHash, ManifestHash: detail.ManifestHash}
	result.Unknown = detail.Status == core.StatusRunning
	unknown, err := s.HasUnknownActions(ctx, id)
	if err != nil {
		return result, err
	}
	result.Unknown = result.Unknown || unknown
	result.Usage = core.UsageActual{Known: true, ToolCalls: int64(detail.ToolCalls)}
	calls, err := NewLLMCallRepo(s.db).CallsByRun(ctx, runID)
	if err != nil {
		return result, err
	}
	for _, call := range calls {
		usage := core.Usage{PromptTokens: call.PromptTokens, CompletionTokens: call.CompletionTokens, TotalTokens: call.TotalTokens}
		tokens, known, usageErr := core.FlowUsageTotal(usage)
		if usageErr != nil || call.Status != core.LLMCallSuccess || !known {
			result.Usage.Known = false
		}
		if known && usageErr == nil {
			if tokens > 200000-result.Usage.Tokens {
				result.Usage.Known = false
			} else {
				result.Usage.Tokens += tokens
			}
		}
	}
	rows, err := s.db.pool.QueryContext(ctx, `SELECT DISTINCT e.kind,e.ref_id,e.version_id,e.content_hash FROM message_evidence_refs e JOIN messages m ON m.id=e.message_id WHERE m.run_id=$1 ORDER BY e.kind,e.ref_id,e.version_id LIMIT 1025`, runID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		ref := flow.EvidenceRef{TaskID: id, RunID: runID, NodeAttemptID: attemptID}
		if err := rows.Scan(&ref.SourceKind, &ref.SourceUID, &ref.SourceVersion, &ref.ContentHash); err != nil {
			return result, errors.Join(err, rows.Close())
		}
		ref.Location = "source:" + ref.SourceUID
		result.Evidence = append(result.Evidence, ref)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return result, err
	}
	if len(result.Evidence) > 1024 {
		return result, flow.ErrCapacity
	}
	// FixedTool output is the original executor's structured Result, never text
	// guessed from a diagnostic message or a fabricated assistant answer.
	var raw sql.NullString
	var outputHash, auditOutput string
	err = s.db.pool.QueryRowContext(ctx, `SELECT a.output,a.output_hash,COALESCE(i.result_json,'') FROM flow_action_attempts a JOIN tool_invocations i ON i.id=a.tool_invocation_id WHERE a.task_id=$1 AND a.node_attempt_id=$2 AND a.outcome='succeeded' AND i.status='success' AND a.output_hash IS NOT NULL ORDER BY i.id DESC LIMIT 1`, id, attemptID).Scan(&raw, &outputHash, &auditOutput)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if raw.Valid {
		if flowDataHash([]byte(raw.String)) != outputHash || raw.String != auditOutput {
			return result, flow.ErrStateConflict
		}
		var value core.Result
		if err := json.Unmarshal([]byte(raw.String), &value); err != nil {
			return result, err
		}
		result.Output = json.RawMessage(raw.String)
	}
	return result, nil
}

// KnowledgeEvidenceCollection supplies identity only after app's SourceViewer
// has rechecked current ACL/version. It does not return source text.
func (s *FlowStore) KnowledgeEvidenceCollection(ctx context.Context, documentID, versionID string) (string, error) {
	var id string
	err := s.db.pool.QueryRowContext(ctx, `SELECT d.collection_id FROM kb_documents d JOIN kb_document_versions v ON v.document_id=d.document_id WHERE d.document_id=$1 AND v.version_id=$2`, documentID, versionID).Scan(&id)
	return id, err
}
