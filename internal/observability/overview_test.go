package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestObservability_OverviewOtherRunsRecentFirst(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	f := &overviewFacts{testFacts: sameMillisecondFacts()}
	for i := range 21 {
		f.runs = append(f.runs, RunFacts{RunID: fmt.Sprintf("top-%02d", i), Agent: "a", Status: "failed", StartedAt: now.Add(-time.Minute)})
	}
	for i := range 20 {
		f.runs = append(f.runs, RunFacts{RunID: fmt.Sprintf("old-%02d", i), Agent: "b", Status: "failed", StartedAt: now.Add(-time.Hour)})
	}
	f.runs = append(f.runs, RunFacts{RunID: "newest", Agent: "c", Status: "failed", StartedAt: now})
	ports := f.ports()
	ports.Runs = f
	ports.Assembly = f
	got, err := NewProjector(ports, ProjectorConfig{SampleRate: 1}).Overview(t.Context(), OverviewQuery{From: now.Add(-2 * time.Hour), To: now.Add(time.Second), Limit: 1})
	require.NoError(t, err)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	var view struct {
		Other *TaskGroup `json:"other_group"`
	}
	require.NoError(t, json.Unmarshal(raw, &view))
	require.NotNil(t, view.Other)
	require.Len(t, view.Other.RunIDs, 20)
	require.Equal(t, "newest", view.Other.RunIDs[0], "overflow groups must merge their newest runs before applying the global bound")
}

type overviewFacts struct {
	*testFacts
	runs []RunFacts
}

func (f *overviewFacts) RunsByAgentWindow(context.Context, string, time.Time, time.Time) ([]RunFacts, error) {
	return f.runs, nil
}

func TestObservability_OverviewGroupsAndEmpty(t *testing.T) {
	f := &overviewFacts{testFacts: sameMillisecondFacts(), runs: []RunFacts{{RunID: "A", Agent: "agent-a", Status: "failed", Reason: "timeout"}, {RunID: "B", Agent: "agent-b", Status: "completed"}}}
	ports := f.ports()
	ports.Runs = f
	ports.Assembly = f
	p := NewProjector(ports, ProjectorConfig{SampleRate: 1})
	q := OverviewQuery{From: time.UnixMilli(1), To: time.UnixMilli(1001), Limit: 1}
	got, err := p.Overview(t.Context(), q)
	require.NoError(t, err)
	require.Equal(t, 2, got.Runs)
	require.Equal(t, 1, got.Failed)
	require.Equal(t, .5, *got.FailedRatio)
	require.Len(t, got.Groups, 1)
	require.Equal(t, []string{"A"}, got.Groups[0].RunIDs)
	require.Equal(t, 1, got.merged)
	b, err := json.Marshal(got)
	require.NoError(t, err)
	var view struct {
		Other *TaskGroup `json:"other_group"`
	}
	require.NoError(t, json.Unmarshal(b, &view))
	require.NotNil(t, view.Other, "超出Top-N不能只丢弃组，必须汇入其他")
	require.Equal(t, 1, view.Other.Total)
	require.Equal(t, []string{"B"}, view.Other.RunIDs)
	f.runs = nil
	got, err = p.Overview(t.Context(), q)
	require.NoError(t, err)
	require.Nil(t, got.FailedRatio)
	require.Empty(t, got.Groups)
}
