package app

import (
	"context"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

type phaseAdapter struct{ recorder *diagnostics.Recorder }

func newPhaseAdapter(recorder *diagnostics.Recorder) core.PhaseRecorder {
	if recorder == nil {
		return nil
	}
	return &phaseAdapter{recorder: recorder}
}

func (p *phaseAdapter) RecordPhase(ctx context.Context, timing core.PhaseTiming) {
	kind := diagnostics.KindAssembly
	switch timing.Phase {
	case core.PhaseKnowledge, core.PhaseQueryEmbedding, core.PhaseScope, core.PhaseKeyword, core.PhaseVector, core.PhaseFusion, core.PhaseSourceRead:
		kind = diagnostics.KindRetrieval
	case core.PhaseFirstResponse, core.PhaseResponseRead:
		kind = diagnostics.KindLLMCall
	}
	attrs := map[string]any{"phase": timing.Phase, "phase_parent": timing.Parent, "phase_status": timing.Status, "duration_ms": float64(timing.Duration) / float64(time.Millisecond), "phase_end": timing.FinishedAt.UTC().Format(time.RFC3339Nano)}
	if _, valid := diagnostics.ReadPhase(kind, attrs); !valid {
		return
	}
	event := diagnostics.DiagnosticEvent{Kind: kind, Correlation: diagnostics.Correlation{RunID: timing.RunID}, StepID: timing.StepID, OccurredAt: timing.StartedAt.UnixMilli(), Attrs: attrs}
	if timing.CallID > 0 {
		event.AuditRef = &diagnostics.AuditRef{Table: "llm_calls", ID: strconv.FormatInt(timing.CallID, 10)}
	}
	p.recorder.Record(ctx, event)
}
