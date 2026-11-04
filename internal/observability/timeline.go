package observability

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// SourceKind 覆盖时间线与导出的不同来源集合。
// SourceKind covers the distinct timeline and export source sets.
type SourceKind string

// 来源集合按语义冻结：时间线九种，八种执行源可扫描导出。
// Timeline has nine sources; eight execution sources support export scanning.
const (
	SourceRun            SourceKind = "runs"
	SourceTask           SourceKind = "task_executions"
	SourceOperation      SourceKind = "management_operations"
	SourceIngest         SourceKind = "kb_ingest_jobs"
	SourceEmbedding      SourceKind = "embedding_calls"
	SourceLLM            SourceKind = "llm_calls"
	SourceTool           SourceKind = "tool_invocations"
	SourceAssembly       SourceKind = "context_assembly_records"
	SourceDiagnostic     SourceKind = "diagnostic_events"
	SourceMetricSnapshot SourceKind = "metric_snapshot"
)

var timelineSources = []SourceKind{SourceRun, SourceTask, SourceOperation, SourceIngest, SourceEmbedding, SourceLLM, SourceTool, SourceAssembly, SourceDiagnostic}

// EndTimeSource 区别独立记录与派生/未知结束时刻。
// EndTimeSource distinguishes recorded, derived, and unknown end times.
type EndTimeSource string

// 结束时间来源不改变权威记录。
// End-time provenance never changes authoritative records.
const (
	EndTimeRecorded        EndTimeSource = "recorded"
	EndTimeDerivedRecorded EndTimeSource = "derived_recorded"
	EndTimeUnknown         EndTimeSource = "unknown"
)

// TimelineQuery 只接受明确锚点与有界范围。
// TimelineQuery requires an anchor and bounded limits.
type TimelineQuery struct {
	Anchor  Anchor
	From    time.Time
	To      time.Time
	Limit   int
	Sources []SourceKind
}

// Validate 拒绝指标快照作为时间线来源，避免把遥测当执行事实。
// Validate rejects metric snapshots as timeline facts.
func (q TimelineQuery) Validate() error {
	if err := q.Anchor.Validate(); err != nil {
		return err
	}
	if q.Limit < 0 || q.Limit > 1000 || !q.From.IsZero() && !q.To.IsZero() && q.From.After(q.To) {
		return ErrInvalidQuery
	}
	for _, source := range q.Sources {
		if !slices.Contains(timelineSources, source) {
			return ErrInvalidQuery
		}
	}
	return nil
}

// TimelineEntry 把展示编号与真实标识分开，Attrs只含允许字段。
// TimelineEntry separates display numbers from persistent identities.
type TimelineEntry struct {
	Flow           *FlowAssociation `json:"flow,omitempty"`
	SchemaVersion  *int             `json:"schema_version,omitempty"`
	Decision       string           `json:"decision,omitempty"`
	RedactedFields int              `json:"redacted_fields"`
	DisplayStepID  string           `json:"display_step_id"`
	EventID        string           `json:"event_id,omitempty"`
	Source         SourceKind       `json:"source"`
	SourceID       string           `json:"source_id"`
	Kind           string           `json:"kind"`
	OccurredAt     time.Time        `json:"occurred_at"`
	EndedAt        *time.Time       `json:"ended_at"`
	EndSource      EndTimeSource    `json:"end_source"`
	Status         string           `json:"status"`
	ReasonCode     string           `json:"reason_code,omitempty"`
	StepID         string           `json:"step_id,omitempty"`
	AttemptID      string           `json:"attempt_id,omitempty"`
	ParentStepID   string           `json:"parent_step_id,omitempty"`
	Seq            int64            `json:"seq,omitempty"`
	AuditRef       string           `json:"audit_ref,omitempty"`
	Evidence       EvidenceGrade    `json:"evidence"`
	Attrs          map[string]any   `json:"attrs"`
	runID          string
}

// Timeline 表达有界前缀与缺口，不承诺跨节点因果顺序。
// Timeline reports bounded evidence without implying distributed causality.
type Timeline struct {
	Anchor      Anchor          `json:"anchor"`
	Entries     []TimelineEntry `json:"entries"`
	Gaps        []EvidenceGap   `json:"gaps"`
	OrderScope  string          `json:"order_scope"`
	Truncated   bool            `json:"truncated"`
	ProjectedAt time.Time       `json:"projected_at"`
}

// ProjectorConfig 固定装配时采样率与读取上限。
// ProjectorConfig fixes the effective sample rate and read bounds.
type ProjectorConfig struct {
	MaxTimelineEntries int
	MaxEvents          int
	SampleRate         float64
	Now                func() time.Time
}

// Projector 没有业务写入口，也不启动后台任务。
// Projector has no business mutation or background execution entry point.
type Projector struct {
	ports Ports
	cfg   ProjectorConfig
}

// NewProjector 显式注入读取端口与时钟，不产生副作用。
// NewProjector injects ports and clock without starting work.
func NewProjector(ports Ports, cfg ProjectorConfig) *Projector {
	if cfg.MaxTimelineEntries <= 0 {
		cfg.MaxTimelineEntries = 10000
	}
	if cfg.MaxEvents <= 0 {
		cfg.MaxEvents = 1000
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Projector{ports: ports, cfg: cfg}
}

// Timeline 在完整有界前缀上编号后才过滤，展示顺序不表示因果。
// Timeline numbers the full bounded prefix before filtering; order is not causality.
func (p *Projector) Timeline(ctx context.Context, q TimelineQuery) (Timeline, error) {
	if err := q.Validate(); err != nil {
		return Timeline{}, err
	}
	out := Timeline{Anchor: q.Anchor, Entries: []TimelineEntry{}, Gaps: []EvidenceGap{}, OrderScope: "anchor-local", ProjectedAt: p.cfg.Now().UTC()}
	ids, extras, gaps := p.resolveAnchor(ctx, q.Anchor)
	out.Entries, out.Gaps = append(out.Entries, extras...), append(out.Gaps, gaps...)
	for _, runID := range ids {
		entries, missing := p.loadRunFacts(ctx, runID)
		out.Entries, out.Gaps = append(out.Entries, entries...), append(out.Gaps, missing...)
		// 每次仅保留全锚点的最早有界集合，但仍检查后续Run，不能在排序前退出。
		// Keep a bounded merge prefix while still examining every resolved run.
		out.Entries = uniqueEntries(out.Entries)
		sortEntries(out.Entries)
		if len(out.Entries) > p.cfg.MaxTimelineEntries {
			out.Truncated = true
			out.Entries = out.Entries[:p.cfg.MaxTimelineEntries]
		}
	}
	if q.Anchor.Kind == AnchorRequest || q.Anchor.Kind == AnchorOperation || q.Anchor.Kind == AnchorIngest {
		dq := diagnostics.Query{Limit: min(p.cfg.MaxEvents, diagnostics.MaxQueryLimit)}
		switch q.Anchor.Kind {
		case AnchorRequest:
			dq.RequestID = q.Anchor.Value
		case AnchorOperation:
			dq.OperationID = q.Anchor.Value
		case AnchorIngest:
			dq.IngestJobID = q.Anchor.Value
		}
		entries, missing := p.loadDiagnostics(ctx, dq, q.From, q.To)
		out.Entries, out.Gaps = append(out.Entries, entries...), append(out.Gaps, missing...)
	}
	out.Entries = uniqueEntries(out.Entries)
	// 旧诊断端口只有一页；达到读取上限即诚实报告可能截断。
	// The diagnostic port exposes one page; hitting its bound is not proof of completeness.
	diagnosticCounts := make(map[string]int)
	for _, entry := range out.Entries {
		if entry.Source == SourceDiagnostic {
			diagnosticCounts[entry.runID]++
			if diagnosticCounts[entry.runID] >= min(p.cfg.MaxEvents, diagnostics.MaxQueryLimit) {
				out.Truncated = true
			}
		}
	}
	sortEntries(out.Entries)
	if len(out.Entries) > p.cfg.MaxTimelineEntries {
		out.Entries, out.Truncated = out.Entries[:p.cfg.MaxTimelineEntries], true
	}
	assignDisplayStepIDs(out.Entries, q.Anchor.Kind == AnchorSession)
	for i := range out.Entries {
		if out.Entries[i].runID != "" {
			attrs := maps.Clone(out.Entries[i].Attrs)
			if attrs == nil {
				attrs = make(map[string]any)
			}
			attrs["run_id"] = out.Entries[i].runID
			out.Entries[i].Attrs = attrs
		}
		out.Entries[i] = redactTimelineEntry(out.Entries[i])
	}
	limit := q.Limit
	if limit == 0 {
		limit = 200
	}
	selected := make([]TimelineEntry, 0, min(limit, len(out.Entries)))
	for _, entry := range out.Entries {
		if len(q.Sources) > 0 && !slices.Contains(q.Sources, entry.Source) || !q.From.IsZero() && entry.OccurredAt.Before(q.From) || !q.To.IsZero() && entry.OccurredAt.After(q.To) {
			continue
		}
		if len(selected) == limit {
			out.Truncated = true
			break
		}
		selected = append(selected, entry)
	}
	out.Entries, out.Gaps = selected, uniqueGaps(out.Gaps)
	return out, nil
}

func factGap(source SourceKind, reason GapReason) EvidenceGap {
	detail := "证据未记录"
	switch reason {
	case GapQueryFailed:
		detail = "读取失败"
	case GapSampledOut:
		detail = "可能被采样，无法确认是否发生"
	case GapNotSupported:
		detail = "该来源未接线或无生产端"
	case GapUnknown:
		detail = "证据不足，无法确认"
	}
	return EvidenceGap{Source: source, Reason: reason, Detail: detail}
}

func queryGap(source SourceKind, err error) EvidenceGap {
	if errors.Is(err, core.ErrRunNotFound) || errors.Is(err, core.ErrOperationNotFound) || errors.Is(err, fs.ErrNotExist) {
		return factGap(source, GapNotRecorded)
	}
	return factGap(source, GapQueryFailed)
}

func (p *Projector) resolveAnchor(ctx context.Context, anchor Anchor) ([]string, []TimelineEntry, []EvidenceGap) {
	var ids []string
	var entries []TimelineEntry
	var gaps []EvidenceGap
	failed := func(source SourceKind, err error) bool {
		if err != nil {
			gaps = append(gaps, queryGap(source, err))
			return true
		}
		return false
	}
	switch anchor.Kind {
	case AnchorFlowTask:
		reader, ok := p.ports.Runs.(FlowFactsReader)
		if !ok {
			return nil, nil, []EvidenceGap{factGap(SourceRun, GapNotSupported)}
		}
		var err error
		ids, err = reader.RunsByFlowTaskID(ctx, anchor.Value)
		failed(SourceRun, err)
	case AnchorRun:
		ids = []string{anchor.Value}
	case AnchorRequest, AnchorSession:
		if p.ports.Runs == nil {
			return nil, nil, []EvidenceGap{factGap(SourceRun, GapNotSupported)}
		}
		var err error
		if anchor.Kind == AnchorRequest {
			ids, err = p.ports.Runs.RunsByRequestID(ctx, anchor.Value)
		} else {
			ids, err = p.ports.Runs.RunsBySessionID(ctx, anchor.Value)
		}
		failed(SourceRun, err)
	case AnchorTask:
		if p.ports.Tasks == nil {
			return nil, nil, []EvidenceGap{factGap(SourceTask, GapNotSupported)}
		}
		facts, err := p.ports.Tasks.TaskExecutionFacts(ctx, anchor.Value, time.Time{}, time.Time{})
		if !failed(SourceTask, err) {
			for _, f := range facts {
				entries = append(entries, taskEntry(f))
				if f.RunID != "" && !slices.Contains(ids, f.RunID) {
					ids = append(ids, f.RunID)
				}
			}
		}
	case AnchorOperation:
		if p.ports.Ops == nil {
			return nil, nil, []EvidenceGap{factGap(SourceOperation, GapNotSupported)}
		}
		f, err := p.ports.Ops.OperationFacts(ctx, anchor.Value)
		if !failed(SourceOperation, err) {
			entries = append(entries, TimelineEntry{Source: SourceOperation, SourceID: f.OperationID, Kind: "management_operation", OccurredAt: f.CreatedAt, EndedAt: f.FinishedAt, EndSource: endSource(f.FinishedAt), Status: f.Status, Evidence: EvidenceDirect, Attrs: map[string]any{"action": f.Action, "verification": f.Verification, "result_code": f.ResultCode}})
		}
	case AnchorIngest:
		if p.ports.Ingest == nil {
			return nil, nil, []EvidenceGap{factGap(SourceIngest, GapNotSupported)}
		}
		f, err := p.ports.Ingest.IngestJobFacts(ctx, anchor.Value)
		if !failed(SourceIngest, err) {
			entries = append(entries, TimelineEntry{Source: SourceIngest, SourceID: f.IngestJobID, Kind: "ingest_job", OccurredAt: f.StartedAt, EndedAt: f.FinishedAt, EndSource: endSource(f.FinishedAt), Status: f.Status, ReasonCode: f.ErrorKind, Evidence: EvidenceDirect, Attrs: map[string]any{"stage": f.Stage, "attempt": f.Attempt}})
		}
		embeddings, err := p.ports.Ingest.EmbeddingCallsByIngest(ctx, anchor.Value)
		if !failed(SourceEmbedding, err) {
			for _, fact := range embeddings {
				entries = append(entries, embeddingEntry(fact))
			}
		}
	}
	return ids, entries, gaps
}

func endSource(end *time.Time) EndTimeSource {
	if end == nil {
		return EndTimeUnknown
	}
	return EndTimeRecorded
}

func (p *Projector) loadRunFacts(ctx context.Context, runID string) ([]TimelineEntry, []EvidenceGap) {
	var entries []TimelineEntry
	var gaps []EvidenceGap
	from, to := time.Time{}, p.cfg.Now()
	if p.ports.Runs == nil {
		gaps = append(gaps, factGap(SourceRun, GapNotSupported))
	} else if f, err := p.ports.Runs.RunFacts(ctx, runID); err != nil {
		gaps = append(gaps, queryGap(SourceRun, err))
	} else {
		from = f.StartedAt
		if f.FinishedAt != nil {
			to = *f.FinishedAt
		}
		entries = append(entries, TimelineEntry{Source: SourceRun, SourceID: f.RunID, Kind: "run", OccurredAt: f.StartedAt, EndedAt: f.FinishedAt, EndSource: endSource(f.FinishedAt), Status: f.Status, ReasonCode: f.Reason, Evidence: EvidenceDirect, Attrs: map[string]any{"request_id": f.RequestID, "rounds": f.Rounds, "tool_calls": f.ToolCalls}, runID: runID})
		if f.ContextBudget != nil {
			attrs := entries[len(entries)-1].Attrs
			attrs["context_required_tokens"] = f.ContextBudget.Required
			attrs["context_limit_tokens"] = f.ContextBudget.Limit
			attrs["context_reserve_tokens"] = f.ContextBudget.Reserve
		}
	}
	if p.ports.Exec == nil {
		for _, source := range []SourceKind{SourceLLM, SourceTool, SourceEmbedding} {
			gaps = append(gaps, factGap(source, GapNotSupported))
		}
	} else {
		llm, err := p.ports.Exec.LLMCallsByRun(ctx, runID)
		if err != nil {
			gaps = append(gaps, factGap(SourceLLM, GapQueryFailed))
		} else {
			for _, f := range llm {
				attrs := map[string]any{"purpose": f.Purpose, "model": f.Model}
				if f.PromptTokens != nil {
					attrs["prompt_tokens"] = *f.PromptTokens
				}
				if f.CompletionTokens != nil {
					attrs["completion_tokens"] = *f.CompletionTokens
				}
				if f.RecoveredAt != nil {
					attrs["recovered_at"] = f.RecoveredAt.Format(time.RFC3339Nano)
				}
				if f.DurationMS != nil {
					attrs["duration_ms"] = *f.DurationMS
				}
				entries = append(entries, TimelineEntry{Source: SourceLLM, SourceID: strconv.FormatInt(f.ID, 10), Kind: "llm_call", OccurredAt: f.CreatedAt, EndSource: EndTimeUnknown, Status: f.Status, ReasonCode: f.ErrorKind, StepID: optionalRound(f.Round), AuditRef: "llm_calls:" + strconv.FormatInt(f.ID, 10), Evidence: EvidenceDirect, Attrs: attrs, runID: runID})
			}
		}
		tools, err := p.ports.Exec.ToolInvocationsByRun(ctx, runID)
		if err != nil {
			gaps = append(gaps, factGap(SourceTool, GapQueryFailed))
		} else {
			for _, f := range tools {
				entry := TimelineEntry{Source: SourceTool, SourceID: strconv.FormatInt(f.ID, 10), Kind: "tool_call", OccurredAt: f.StartedAt, EndedAt: f.FinishedAt, EndSource: EndTimeUnknown, Status: f.Status, ReasonCode: f.ErrorKind, AuditRef: "tool_invocations:" + strconv.FormatInt(f.ID, 10), Evidence: EvidenceDirect, Attrs: map[string]any{"tool": f.Tool, "tool_call_id": f.ToolCallID}, runID: runID}
				if f.DurationMS != nil {
					entry.Attrs["duration_ms"] = *f.DurationMS
				}
				if f.RecoveredAt != nil {
					entry.Attrs["recovered_at"] = f.RecoveredAt.Format(time.RFC3339Nano)
				}
				if f.FinishedAt != nil {
					entry.EndSource = EndTimeDerivedRecorded
				}
				entries = append(entries, entry)
			}
		}
		embeddings, err := p.ports.Exec.EmbeddingCallsByRun(ctx, runID)
		if err != nil {
			gaps = append(gaps, factGap(SourceEmbedding, GapQueryFailed))
		} else {
			for _, f := range embeddings {
				entries = append(entries, embeddingEntry(f))
			}
		}
	}
	if p.ports.Assembly == nil {
		gaps = append(gaps, factGap(SourceAssembly, GapNotSupported))
	} else {
		facts, err := p.ports.Assembly.AssemblyByRun(ctx, runID)
		if err != nil {
			gaps = append(gaps, factGap(SourceAssembly, GapQueryFailed))
		} else {
			if len(facts) == 0 {
				gaps = append(gaps, p.missingEvidence(ctx, SourceAssembly, from, to, GapNotRecorded)...)
			}
			for _, f := range facts {
				entries = append(entries, TimelineEntry{Source: SourceAssembly, SourceID: f.RecordID, Kind: "assembly", OccurredAt: f.CreatedAt, EndSource: EndTimeUnknown, StepID: f.StepID, Evidence: EvidenceDirect, Attrs: map[string]any{"purpose": string(f.Purpose)}, runID: runID})
			}
		}
	}
	if p.ports.Tasks == nil {
		gaps = append(gaps, factGap(SourceTask, GapNotSupported))
	} else {
		facts, err := p.ports.Tasks.TaskExecutionByRun(ctx, runID)
		if err != nil {
			gaps = append(gaps, factGap(SourceTask, GapQueryFailed))
		} else {
			for _, f := range facts {
				entries = append(entries, taskEntry(f))
			}
		}
	}
	diag, diagGaps := p.loadDiagnostics(ctx, diagnostics.Query{RunID: runID, Limit: min(p.cfg.MaxEvents, diagnostics.MaxQueryLimit)}, from, to)
	entries, err := p.associateFlow(ctx, runID, append(entries, diag...))
	if err != nil {
		gaps = append(gaps, queryGap(SourceRun, err))
	}
	return entries, append(gaps, diagGaps...)
}

func optionalRound(round int) string {
	if round <= 0 {
		return ""
	}
	return strconv.Itoa(round)
}

func taskEntry(f TaskExecutionFact) TimelineEntry {
	entry := TimelineEntry{Source: SourceTask, SourceID: strconv.FormatInt(f.ID, 10), Kind: "task_execution", OccurredAt: f.StartedAt, EndedAt: f.FinishedAt, EndSource: endSource(f.FinishedAt), Status: f.Status, ReasonCode: f.ErrorKind, Evidence: EvidenceDirect, Attrs: map[string]any{"task_id": f.TaskID, "run_id": f.RunID}, runID: f.RunID}
	if f.ReasonFromRun {
		entry.Attrs["reason_source"] = "associated_run"
	}
	return entry
}

func embeddingEntry(f EmbeddingCallFact) TimelineEntry {
	return TimelineEntry{Source: SourceEmbedding, SourceID: strconv.FormatInt(f.ID, 10), Kind: "embedding", OccurredAt: f.CreatedAt, EndedAt: f.FinishedAt, EndSource: endSource(f.FinishedAt), Status: f.Status, ReasonCode: f.ErrorKind, Evidence: EvidenceDirect, Attrs: map[string]any{"model": f.Model}, runID: f.RunID}
}

func (p *Projector) loadDiagnostics(ctx context.Context, q diagnostics.Query, from, to time.Time) ([]TimelineEntry, []EvidenceGap) {
	if p.ports.Diagnostics == nil {
		return nil, []EvidenceGap{factGap(SourceDiagnostic, GapNotSupported)}
	}
	var events []DiagnosticReadEvent
	var err error
	if reader, ok := p.ports.Diagnostics.(diagnosticProjectionReader); ok {
		events, err = reader.ListObservationEvents(ctx, q)
	} else {
		var raw []diagnostics.DiagnosticEvent
		raw, err = p.ports.Diagnostics.ListEvents(ctx, q)
		for _, e := range raw {
			events = append(events, DiagnosticReadEvent{Event: e})
		}
	}
	if err != nil {
		return nil, []EvidenceGap{factGap(SourceDiagnostic, GapQueryFailed)}
	}
	if len(events) == 0 {
		reason := GapNotRecorded
		if p.cfg.SampleRate < 1 {
			reason = GapSampledOut
		}
		return nil, p.missingEvidence(ctx, SourceDiagnostic, from, to, reason)
	}
	out := make([]TimelineEntry, 0, len(events))
	for _, raw := range events {
		e := raw.Event.Redacted(nil)
		hidden := raw.HiddenFields + max(0, len(raw.Event.Attrs)-len(e.Attrs))
		ref := ""
		if e.AuditRef != nil {
			ref = e.AuditRef.String()
		}
		out = append(out, TimelineEntry{SchemaVersion: &e.SchemaVersion, Decision: string(e.Decision), RedactedFields: hidden, Source: SourceDiagnostic, SourceID: e.EventID, EventID: e.EventID, Kind: string(e.Kind), OccurredAt: time.UnixMilli(e.OccurredAt).UTC(), EndSource: EndTimeUnknown, ReasonCode: string(e.ReasonCode), StepID: e.StepID, AttemptID: e.AttemptID, ParentStepID: e.ParentStepID, Seq: e.Seq, AuditRef: ref, Evidence: EvidenceDirect, Attrs: e.Attrs, runID: e.Correlation.RunID})
		if phase, ok := diagnostics.ReadPhase(e.Kind, e.Attrs); ok {
			entry := &out[len(out)-1]
			entry.EndedAt, entry.EndSource, entry.Status = &phase.End, EndTimeRecorded, phase.Status
		}
	}
	return out, nil
}

func (p *Projector) missingEvidence(ctx context.Context, source SourceKind, from, to time.Time, reason GapReason) []EvidenceGap {
	if p.ports.Cleanup != nil && !from.IsZero() && !to.IsZero() {
		cleanup, err := p.ports.Cleanup.CleanupRuns(ctx, from, to)
		if err != nil {
			return []EvidenceGap{factGap(source, reason), factGap(source, GapQueryFailed)}
		}
		for _, run := range cleanup {
			if run.TableKind == string(source) && run.Status == "succeeded" && run.Deleted > 0 && !run.WindowEnd.Before(from) && !run.WindowStart.After(to) {
				label := "诊断记录"
				if source == SourceAssembly {
					label = "装配记录"
				}
				return []EvidenceGap{{Source: source, Reason: GapRetentionWindowCleaned, Detail: "该时间窗发生过" + label + "清理；当前记录是否因此缺失，无法确认"}}
			}
		}
	}
	return []EvidenceGap{factGap(source, reason)}
}

func sortEntries(entries []TimelineEntry) {
	slices.SortStableFunc(entries, func(a, b TimelineEntry) int {
		if a.OccurredAt.IsZero() != b.OccurredAt.IsZero() {
			if a.OccurredAt.IsZero() {
				return 1
			}
			return -1
		}
		if order := a.OccurredAt.Compare(b.OccurredAt); order != 0 {
			return order
		}
		ar, br := slices.Index(timelineSources, a.Source), slices.Index(timelineSources, b.Source)
		if ar != br {
			return ar - br
		}
		if a.SourceID < b.SourceID {
			return -1
		}
		if a.SourceID > b.SourceID {
			return 1
		}
		return 0
	})
}

func assignDisplayStepIDs(entries []TimelineEntry, session bool) {
	runs := make(map[string]int)
	steps := make(map[string]int)
	for i := range entries {
		if !session {
			entries[i].DisplayStepID = fmt.Sprintf("S%d", i+1)
			continue
		}
		id := entries[i].runID
		if runs[id] == 0 {
			runs[id] = len(runs) + 1
		}
		steps[id]++
		entries[i].DisplayStepID = fmt.Sprintf("R%d-S%d", runs[id], steps[id])
	}
}

func uniqueEntries(entries []TimelineEntry) []TimelineEntry {
	seen := make(map[string]bool)
	out := make([]TimelineEntry, 0, len(entries))
	for _, entry := range entries {
		key := string(entry.Source) + ":" + entry.SourceID
		if !seen[key] {
			out, seen[key] = append(out, entry), true
		}
	}
	return out
}

func uniqueGaps(gaps []EvidenceGap) []EvidenceGap {
	seen := make(map[EvidenceGap]bool)
	out := make([]EvidenceGap, 0, len(gaps))
	for _, gap := range gaps {
		if !seen[gap] {
			out, seen[gap] = append(out, gap), true
		}
	}
	return out
}
