package core

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Phase names describe measured boundaries, never inferred gaps between events.
const (
	PhaseKnowledge      = "knowledge_retrieval"
	PhaseQueryEmbedding = "query_embedding"
	PhaseScope          = "retrieval_scope"
	PhaseKeyword        = "lexical_search"
	PhaseVector         = "vector_search"
	PhaseFusion         = "retrieval_fusion"
	PhaseSourceRead     = "knowledge_source_read"
	PhaseContext        = "context_prepare"
	PhaseRevalidate     = "knowledge_revalidate"
	PhaseCompaction     = "context_compaction"
	PhaseAssembly       = "context_assembly"
	PhaseFirstResponse  = "model_first_response"
	PhaseResponseRead   = "model_response_read"
)

// PhaseTiming contains only a bounded stage identity, correlation and clocks.
// Duration uses the monotonic clock; wall-clock endpoints remain separately recorded.
type PhaseTiming struct {
	RunID, StepID, Phase, Parent, Status string
	CallID                               int64
	StartedAt, FinishedAt                time.Time
	Duration                             time.Duration
}

// PhaseRecorder is a droppable observation port; it never owns business state.
type PhaseRecorder interface {
	RecordPhase(context.Context, PhaseTiming)
}

// ModelTiming is a per-call, in-process capture. The transport fills it without
// observation I/O; the audited caller publishes it after measuring call duration.
type ModelTiming struct{ Wait, Read *PhaseTiming }

// ErrPhaseSkipped marks an expected gate outcome for observation only.
// Callers keep returning their original business error or legal empty result.
var ErrPhaseSkipped = errors.New("phase skipped")

func RecordPhaseTiming(ctx context.Context, recorder PhaseRecorder, measurement PhaseTiming) {
	if recorder == nil || measurement.RunID == "" {
		return
	}
	defer func() { _ = recover() }()
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	recorder.RecordPhase(writeCtx, measurement)
}

// BeginPhase records once at the actual end of a stage, before observation I/O.
// Cancellation is retained using a detached bounded recording context. A faulty
// observer cannot change the business result. No prompt, response or error text is sent.
func BeginPhase(ctx context.Context, recorder PhaseRecorder, phase, parent, runID, stepID string, callID int64) func(error) {
	if recorder == nil || runID == "" {
		return func(error) {}
	}
	start := time.Now()
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			end := time.Now()
			status := "completed"
			if err != nil {
				status = "failed"
			}
			if errors.Is(err, ErrPhaseSkipped) {
				status = "skipped"
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = "cancelled"
			}
			measurement := PhaseTiming{RunID: runID, StepID: stepID, Phase: phase, Parent: parent, CallID: callID, StartedAt: start, FinishedAt: end, Duration: end.Sub(start), Status: status}
			RecordPhaseTiming(ctx, recorder, measurement)
		})
	}
}
