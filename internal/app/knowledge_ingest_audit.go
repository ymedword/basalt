package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/web"
)

type auditedKnowledgeIngest struct {
	inner      web.KnowledgeIngest
	guard      knowledge.ReindexAuthorization
	operations core.OperationStore
	now        func() time.Time
	logger     *slog.Logger
}

func (a *auditedKnowledgeIngest) Ingest(ctx context.Context, req knowledge.IngestRequest) (knowledge.IngestResult, error) {
	return a.inner.Ingest(ctx, req)
}

func (a *auditedKnowledgeIngest) Delete(ctx context.Context, req knowledge.DeleteRequest) error {
	return a.inner.Delete(ctx, req)
}

// Reindex writes a pending management operation before any source is read or
// embedding request is sent. A caller that loses authorization is rejected again
// at the domain's activation point.
func (a *auditedKnowledgeIngest) Reindex(ctx context.Context, req knowledge.ReindexRequest) (knowledge.IngestResult, error) {
	if a == nil || a.inner == nil || a.guard == nil || a.operations == nil || a.now == nil || a.logger == nil {
		return knowledge.IngestResult{}, knowledge.ErrIngestUnavailable
	}
	id := strings.TrimSpace(req.DocumentID)
	if id == "" {
		return knowledge.IngestResult{}, knowledge.ErrInvalidRequest
	}
	if req.AuditActor.Subject == "" || req.AuditActor.Source == "" ||
		(req.AuditActor.Method != core.AuthMethodBearer && req.AuditActor.Method != core.AuthMethodLocalTrust) {
		return knowledge.IngestResult{}, fmt.Errorf("重建索引缺少经过认证的审计身份")
	}
	if err := a.guard.AuthorizeReindex(ctx, req.Principal, id); err != nil {
		return knowledge.IngestResult{}, err
	}
	detail, err := json.Marshal(struct {
		CallerID string `json:"caller_id"`
		KeyID    string `json:"key_id,omitempty"`
	}{req.Principal.CallerID, req.Principal.KeyID})
	if err != nil {
		return knowledge.IngestResult{}, err
	}
	now := a.now()
	opID := core.NewOperationID(now)
	if err := a.operations.BeginOperation(ctx, core.OperationRecord{
		OperationID: opID, Action: core.ActionKnowledgeReindex, TargetKind: core.TargetKindKnowledgeDocument, TargetID: id,
		ActorSubject: req.AuditActor.Subject, AuthMethod: req.AuditActor.Method, Source: req.AuditActor.Source,
		Status: core.OperationPending, DetailJSON: detail, CreatedAt: now,
	}); err != nil {
		return knowledge.IngestResult{}, fmt.Errorf("预写重建操作失败: %w", err)
	}
	result, runErr := a.inner.Reindex(ctx, req)
	status, code := core.OperationSuccess, "OK"
	if runErr != nil || result.Status == knowledge.VersionFailed {
		status, code = core.OperationFailed, "REINDEX_FAILED"
	}
	if ctx.Err() != nil && (runErr != nil || result.Status != knowledge.VersionActive) {
		status, code = core.OperationUnknown, "OUTCOME_UNKNOWN"
	}
	detail, err = json.Marshal(struct {
		CallerID  string                  `json:"caller_id"`
		KeyID     string                  `json:"key_id,omitempty"`
		JobID     string                  `json:"ingest_job_id,omitempty"`
		VersionID string                  `json:"version_id,omitempty"`
		Status    knowledge.VersionStatus `json:"version_status,omitempty"`
	}{req.Principal.CallerID, req.Principal.KeyID, result.IngestJobID, result.VersionID, result.Status})
	if err != nil {
		detail = nil
	}
	if finishErr := a.operations.FinishOperation(context.WithoutCancel(ctx), opID, status, code, "", detail, a.now()); finishErr != nil {
		a.logger.Error("重建操作终态写入失败", "operation_id", opID, "error", finishErr)
	}
	return result, runErr
}
