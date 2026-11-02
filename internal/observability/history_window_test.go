package observability

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestObservability_HistoryWindowIsBoundedAtThirtyDays(t *testing.T) {
	now := time.Unix(1700000000, 0)
	f := &overviewFacts{testFacts: sameMillisecondFacts()}
	ports := f.ports()
	ports.Runs, ports.Assembly = f, f
	p := NewProjector(ports, ProjectorConfig{})
	for _, days := range []int{1, 7, 30} {
		q := ReadWindow{From: now.Add(-time.Duration(days) * 24 * time.Hour), To: now, Limit: 100}
		require.NoError(t, q.Validate())
		_, err := p.Overview(t.Context(), OverviewQuery{From: q.From, To: now, Limit: 100})
		require.NoError(t, err)
	}
	q := ReadWindow{From: now.Add(-MaxReadWindow - time.Nanosecond), To: now, Limit: 100}
	require.ErrorIs(t, q.Validate(), ErrInvalidQuery)
	_, err := p.Overview(t.Context(), OverviewQuery{From: q.From, To: now, Limit: 100})
	require.ErrorIs(t, err, ErrInvalidQuery)
	q.From = now.Add(-time.Hour)
	q.Limit = 1001
	require.ErrorIs(t, q.Validate(), ErrInvalidQuery, "wider history does not remove row limits")
}
