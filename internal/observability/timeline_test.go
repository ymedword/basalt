package observability

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

func TestObservability_TimelineLocalOrder(t *testing.T) {
	// 验收点：同毫秒的固定来源顺序不依赖ID字典序；按ID排序时llm会跑到run之前。
	f := sameMillisecondFacts()
	p := NewProjector(f.ports(), ProjectorConfig{SampleRate: 1})
	got, err := p.Timeline(context.Background(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Limit: 100})
	require.NoError(t, err, "valid anchor must project evidence, not return an unimplemented error")
	require.Equal(t, "anchor-local", got.OrderScope)
	var ids []string
	for _, entry := range got.Entries {
		ids = append(ids, string(entry.Source)+":"+entry.SourceID)
	}
	require.Equal(t, []string{"runs:run_1", "llm_calls:1", "tool_invocations:1"}, ids)
	// 不同时间的顺序先于来源优先级，删除排序会让tool错误留在run后。
	f.tools[0].StartedAt = f.run.StartedAt.Add(-time.Second)
	got, err = p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, SourceTool, got.Entries[0].Source)
}

type anchorFacts struct {
	*testFacts
	runs     []RunFacts
	tasks    []TaskExecutionFact
	llmByRun map[string][]LLMCallFact
}

func (f *anchorFacts) LLMCallsByRun(_ context.Context, run string) ([]LLMCallFact, error) {
	return f.llmByRun[run], nil
}

func (f *anchorFacts) RunFacts(_ context.Context, id string) (RunFacts, error) {
	for _, run := range f.runs {
		if run.RunID == id {
			return run, nil
		}
	}
	return RunFacts{}, core.ErrRunNotFound
}

func (f *anchorFacts) RunsBySessionID(context.Context, string) ([]string, error) {
	var ids []string
	for _, run := range f.runs {
		ids = append(ids, run.RunID)
	}
	return ids, nil
}

func (f *anchorFacts) TaskExecutionFacts(context.Context, string, time.Time, time.Time) ([]TaskExecutionFact, error) {
	return f.tasks, nil
}

func (f *anchorFacts) TaskExecutionByRun(context.Context, string) ([]TaskExecutionFact, error) {
	return nil, nil
}

func (f *anchorFacts) OperationFacts(context.Context, string) (ManagementOperationFact, error) {
	return ManagementOperationFact{OperationID: "op-real", Status: "success", CreatedAt: f.run.StartedAt}, nil
}

func (f *anchorFacts) IngestJobFacts(context.Context, string) (IngestJobFact, error) {
	return IngestJobFact{IngestJobID: "ingest-real", Status: "success", StartedAt: f.run.StartedAt}, nil
}

func (f *anchorFacts) EmbeddingCallsByIngest(context.Context, string) ([]EmbeddingCallFact, error) {
	return []EmbeddingCallFact{{ID: 19, IngestJobID: "ingest-real", Status: "success", CreatedAt: f.run.StartedAt}}, nil
}

func TestObservability_TimelineAnchors(t *testing.T) {
	f := &anchorFacts{testFacts: sameMillisecondFacts()}
	f.llm, f.tools = nil, nil
	f.runs = []RunFacts{f.run, {RunID: "run-2", StartedAt: f.run.StartedAt.Add(time.Second), Status: "completed"}}
	f.tasks = []TaskExecutionFact{{ID: 7, TaskID: "task-real", Status: "skipped"}}
	ports := f.ports()
	ports.Runs, ports.Tasks, ports.Ops, ports.Ingest = f, f, f, f
	p := NewProjector(ports, ProjectorConfig{SampleRate: 1})
	for _, c := range []struct {
		kind    AnchorKind
		id      string
		sources []SourceKind
	}{
		{AnchorRun, "run_1", []SourceKind{SourceRun}},
		{AnchorRequest, "request-real", []SourceKind{SourceRun}},
		{AnchorTask, "task-real", []SourceKind{SourceTask}},
		{AnchorOperation, "op-real", []SourceKind{SourceOperation}},
		{AnchorIngest, "ingest-real", []SourceKind{SourceIngest, SourceEmbedding}},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			got, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: c.kind, Value: c.id}})
			require.NoError(t, err)
			var sources []SourceKind
			for _, entry := range got.Entries {
				sources = append(sources, entry.Source)
			}
			require.Equal(t, c.sources, sources)
		})
	}
	session, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorSession, Value: "session-real"}})
	require.NoError(t, err)
	require.Len(t, session.Entries, 2)
	require.Equal(t, "R1-S1", session.Entries[0].DisplayStepID)
	require.Equal(t, "R2-S1", session.Entries[1].DisplayStepID)
	missing, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "missing"}})
	require.NoError(t, err)
	require.Contains(t, missing.Gaps, factGap(SourceRun, GapNotRecorded))
	require.NotContains(t, missing.Gaps, factGap(SourceRun, GapQueryFailed))
}

func TestObservability_TimelineOverlappingRunsPrefix(t *testing.T) {
	f := &anchorFacts{testFacts: sameMillisecondFacts(), llmByRun: map[string][]LLMCallFact{}}
	f.tools = nil
	f.runs = []RunFacts{{RunID: "A", StartedAt: f.run.StartedAt}, {RunID: "B", StartedAt: f.run.StartedAt.Add(time.Second)}}
	f.llmByRun["A"] = []LLMCallFact{{ID: 1, RunID: "A", CreatedAt: f.run.StartedAt.Add(2 * time.Second)}, {ID: 2, RunID: "A", CreatedAt: f.run.StartedAt.Add(3 * time.Second)}}
	ports := f.ports()
	ports.Runs, ports.Exec = f, f
	got, err := NewProjector(ports, ProjectorConfig{MaxTimelineEntries: 2, SampleRate: 1}).Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorSession, Value: "session"}})
	require.NoError(t, err)
	require.True(t, got.Truncated)
	require.Len(t, got.Entries, 2)
	require.Equal(t, "A", got.Entries[0].SourceID)
	require.Equal(t, "B", got.Entries[1].SourceID, "later run must displace a later call from the first run")
}

func TestObservability_TimelineAssemblyCleanupGap(t *testing.T) {
	f := sameMillisecondFacts()
	ports := f.ports()
	ports.Assembly = f
	p := NewProjector(ports, ProjectorConfig{SampleRate: 1})
	q := TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}}
	got, err := p.Timeline(t.Context(), q)
	require.NoError(t, err)
	require.Contains(t, got.Gaps, factGap(SourceAssembly, GapNotRecorded))
	f.cleanup = []CleanupRun{{TableKind: string(SourceAssembly), WindowStart: f.run.StartedAt, WindowEnd: f.run.StartedAt, Deleted: 1, Status: "succeeded"}}
	got, err = p.Timeline(t.Context(), q)
	require.NoError(t, err)
	require.Contains(t, got.Gaps, EvidenceGap{Source: SourceAssembly, Reason: GapRetentionWindowCleaned, Detail: "该时间窗发生过装配记录清理；当前记录是否因此缺失，无法确认"})
}

type testFacts struct {
	run           RunFacts
	llm           []LLMCallFact
	tools         []ToolInvocationFact
	assembly      []AssemblyRecordFact
	events        []diagnostics.DiagnosticEvent
	cleanup       []CleanupRun
	diagnosticErr error
	cleanupErr    error
	assemblyErr   error
}

func sameMillisecondFacts() *testFacts {
	now := time.UnixMilli(1700000000000).UTC()
	return &testFacts{
		run:   RunFacts{RunID: "run_1", Status: "completed", StartedAt: now, FinishedAt: &now},
		llm:   []LLMCallFact{{ID: 1, RunID: "run_1", Status: "success", Round: 1, CreatedAt: now}},
		tools: []ToolInvocationFact{{ID: 1, RunID: "run_1", Status: "success", StartedAt: now, FinishedAt: &now}},
	}
}

func (f *testFacts) ports() Ports                                       { return Ports{Runs: f, Exec: f, Diagnostics: f, Cleanup: f} }
func (f *testFacts) RunFacts(context.Context, string) (RunFacts, error) { return f.run, nil }
func (f *testFacts) RunsByRequestID(context.Context, string) ([]string, error) {
	return []string{f.run.RunID}, nil
}

func (f *testFacts) RunsBySessionID(context.Context, string) ([]string, error) {
	return []string{f.run.RunID}, nil
}

func (f *testFacts) RunsByAgentWindow(context.Context, string, time.Time, time.Time) ([]RunFacts, error) {
	return []RunFacts{f.run}, nil
}

func (f *testFacts) LLMCallsByRun(context.Context, string) ([]LLMCallFact, error) { return f.llm, nil }

func (f *testFacts) ToolInvocationsByRun(context.Context, string) ([]ToolInvocationFact, error) {
	return f.tools, nil
}

func (f *testFacts) EmbeddingCallsByRun(context.Context, string) ([]EmbeddingCallFact, error) {
	return nil, nil
}

func (f *testFacts) AssemblyByRun(context.Context, string) ([]AssemblyRecordFact, error) {
	return f.assembly, f.assemblyErr
}

func (f *testFacts) ListEvents(context.Context, diagnostics.Query) ([]diagnostics.DiagnosticEvent, error) {
	return f.events, f.diagnosticErr
}

func (f *testFacts) CleanupRuns(context.Context, time.Time, time.Time) ([]CleanupRun, error) {
	return f.cleanup, f.cleanupErr
}

func TestObservability_DisplayStepIDsStable(t *testing.T) {
	f := sameMillisecondFacts()
	p := NewProjector(f.ports(), ProjectorConfig{SampleRate: 1})
	all, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Limit: 100})
	require.NoError(t, err)
	filtered, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Sources: []SourceKind{SourceLLM}, Limit: 1})
	require.NoError(t, err)
	require.Len(t, filtered.Entries, 1)
	require.Equal(t, all.Entries[1].DisplayStepID, filtered.Entries[0].DisplayStepID)
	require.Equal(t, "S2", filtered.Entries[0].DisplayStepID)
}

func TestObservability_TimelineTruncated(t *testing.T) {
	f := sameMillisecondFacts()
	p := NewProjector(f.ports(), ProjectorConfig{SampleRate: 1, MaxTimelineEntries: 2})
	got, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, got.Entries, 2)
	require.True(t, got.Truncated)
}

func TestObservability_MissingReasonsOnlyWhenProven(t *testing.T) {
	for _, rate := range []float64{.5, 1} {
		f := sameMillisecondFacts()
		p := NewProjector(f.ports(), ProjectorConfig{SampleRate: rate})
		got, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Limit: 100})
		require.NoError(t, err)
		var reasons []GapReason
		for _, gap := range got.Gaps {
			if gap.Source == SourceDiagnostic {
				reasons = append(reasons, gap.Reason)
			}
		}
		if rate < 1 {
			require.Contains(t, reasons, GapSampledOut)
			require.NotContains(t, reasons, GapNotRecorded)
		} else {
			require.Contains(t, reasons, GapNotRecorded)
			require.NotContains(t, reasons, GapSampledOut)
		}
	}
}

func TestObservability_SampledOutNotNormal(t *testing.T) {
	f := sameMillisecondFacts()
	f.diagnosticErr = errors.New("CANARY_OBS_RAW_ERROR")
	p := NewProjector(f.ports(), ProjectorConfig{SampleRate: .5})
	got, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Limit: 100})
	require.NoError(t, err)
	require.Contains(t, got.Gaps, EvidenceGap{Source: SourceDiagnostic, Reason: GapQueryFailed, Detail: "读取失败"})
	for _, gap := range got.Gaps {
		require.NotContains(t, gap.Detail, "CANARY")
	}
}

func TestObservability_CleanedWindowAttribution(t *testing.T) {
	f := sameMillisecondFacts()
	f.cleanup = []CleanupRun{{TableKind: "diagnostic_events", WindowStart: f.run.StartedAt.Add(-time.Second), WindowEnd: f.run.StartedAt.Add(time.Second), Deleted: 1, Status: "succeeded"}}
	p := NewProjector(f.ports(), ProjectorConfig{SampleRate: 1})
	got, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Limit: 100})
	require.NoError(t, err)
	var found bool
	for _, gap := range got.Gaps {
		if gap.Reason == GapRetentionWindowCleaned {
			found = true
			require.Contains(t, gap.Detail, "发生过")
			require.Contains(t, gap.Detail, "无法确认")
			require.NotContains(t, gap.Detail, "该记录已被清理")
		}
	}
	require.True(t, found)
}
