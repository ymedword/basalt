package observability

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestObservability_AnchorValidation(t *testing.T) {
	for _, kind := range []AnchorKind{AnchorRun, AnchorRequest, AnchorTask, AnchorOperation, AnchorIngest, AnchorSession} {
		require.NoError(t, (Anchor{Kind: kind, Value: "id-1"}).Validate())
	}
	for _, a := range []Anchor{{Kind: AnchorRun}, {Kind: "other", Value: "x"}, {Kind: AnchorRun, Value: "x\n"}, {Kind: AnchorRequest, Value: "has space"}} {
		require.ErrorIs(t, a.Validate(), ErrInvalidQuery)
	}
	q := TimelineQuery{Anchor: Anchor{Kind: AnchorTask, Value: "task-1"}, Sources: []SourceKind{SourceMetricSnapshot}}
	require.ErrorIs(t, q.Validate(), ErrInvalidQuery, "metric snapshots are not timeline facts")
	q.Sources = []SourceKind{SourceAssembly}
	require.NoError(t, q.Validate())
	q.Limit = 1001
	require.ErrorIs(t, q.Validate(), ErrInvalidQuery)
	q.Limit, q.From, q.To = 100, time.Unix(2, 0), time.Unix(1, 0)
	require.ErrorIs(t, q.Validate(), ErrInvalidQuery)
}

func ExampleAnchor_Validate() {
	anchor := Anchor{Kind: AnchorRun, Value: "run_1"}
	if err := anchor.Validate(); err != nil {
		panic(err)
	}
	// Output:
}
