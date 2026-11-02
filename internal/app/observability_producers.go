package app

import (
	"context"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/knowledge"
)

// 摄取装饰器只在权威动作成功后发诊断，不接管业务事务或审计。
// The ingest decorator emits evidence after authoritative success, outside business transactions.
type diagnosticIngestStore struct {
	knowledge.IngestStore
	recorder *diagnostics.Recorder
	mu       sync.Mutex
	jobs     map[string]string
}

func (s *diagnosticIngestStore) stage(ctx context.Context, job, stage string, at time.Time) {
	if job != "" {
		s.recorder.Record(ctx, diagnostics.DiagnosticEvent{
			Kind:        diagnostics.KindIngestStage,
			Correlation: diagnostics.Correlation{IngestJobID: job}, StepID: stage, OccurredAt: at.UnixMilli(),
		})
	}
}

func (s *diagnosticIngestStore) StartJob(ctx context.Context, req knowledge.StartJobRequest) (knowledge.JobRecord, error) {
	job, err := s.IngestStore.StartJob(ctx, req)
	if err == nil {
		s.mu.Lock()
		if s.jobs == nil {
			s.jobs = make(map[string]string)
		}
		s.jobs[job.VersionID] = job.JobID
		s.mu.Unlock()
		s.stage(ctx, job.JobID, knowledge.JobStageParse, req.At)
	}
	return job, err
}

func (s *diagnosticIngestStore) job(version string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[version]
}

func (s *diagnosticIngestStore) WriteChunks(ctx context.Context, version string, chunks []knowledge.ChunkDraft) ([]knowledge.ChunkRef, error) {
	refs, err := s.IngestStore.WriteChunks(ctx, version, chunks)
	if err == nil {
		s.stage(ctx, s.job(version), knowledge.JobStageChunk, time.Now())
	}
	return refs, err
}

func (s *diagnosticIngestStore) MarkIndexed(ctx context.Context, version string, at time.Time) error {
	err := s.IngestStore.MarkIndexed(ctx, version, at)
	if err == nil {
		s.stage(ctx, s.job(version), knowledge.JobStageIndex, at)
	}
	return err
}

func (s *diagnosticIngestStore) FinishJob(ctx context.Context, job, stage, status, errorKind string, at time.Time) error {
	err := s.IngestStore.FinishJob(ctx, job, stage, status, errorKind, at)
	if err == nil {
		s.stage(ctx, job, stage, at)
		s.mu.Lock()
		for version, id := range s.jobs {
			if id == job {
				delete(s.jobs, version)
			}
		}
		s.mu.Unlock()
	}
	return err
}

func (s *diagnosticIngestStore) AuthorizeReindex(ctx context.Context, p core.Principal, document string) error {
	guard, ok := s.IngestStore.(knowledge.ReindexAuthorization)
	if !ok {
		return knowledge.ErrIngestUnavailable
	}
	return guard.AuthorizeReindex(ctx, p, document)
}

func (s *diagnosticIngestStore) ActivateReindex(ctx context.Context, p core.Principal, version string, generation int, at time.Time) (string, error) {
	guard, ok := s.IngestStore.(knowledge.ReindexAuthorization)
	if !ok {
		return "", knowledge.ErrIngestUnavailable
	}
	return guard.ActivateReindex(ctx, p, version, generation, at)
}

type diagnosticIngestAudit struct {
	knowledge.EmbeddingAudit
	recorder *diagnostics.Recorder
}

func (s *diagnosticIngestAudit) BeginEmbedding(ctx context.Context, job, model string, count int) (string, error) {
	id, err := s.EmbeddingAudit.BeginEmbedding(ctx, job, model, count)
	if err == nil && job != "" {
		s.recorder.Record(ctx, diagnostics.DiagnosticEvent{
			Kind:        diagnostics.KindIngestStage,
			Correlation: diagnostics.Correlation{IngestJobID: job}, StepID: knowledge.JobStageEmbed,
		})
	}
	return id, err
}

type diagnosticGenerationStore struct {
	knowledge.GenerationStore
	recorder    *diagnostics.Recorder
	correlation diagnostics.Correlation
}

func (s *diagnosticGenerationStore) ActivateGeneration(ctx context.Context, generation int, at time.Time) (int, error) {
	retired, err := s.GenerationStore.ActivateGeneration(ctx, generation, at)
	// 启动期通常没有请求/操作锚点；不得以代际号伪造operation_id。
	// Startup normally has no request/operation anchor; never invent one from a generation.
	if err == nil && s.correlation.Validate() == nil {
		s.recorder.Record(ctx, diagnostics.DiagnosticEvent{
			Kind:        diagnostics.KindIndexSwitch,
			Correlation: s.correlation, OccurredAt: at.UnixMilli(), Attrs: map[string]any{"count": 1},
		})
	}
	return retired, err
}
