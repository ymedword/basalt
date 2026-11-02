package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObservability_SendResultClassification(t *testing.T) {
	batch := ExportBatch{RecordCount: 3}
	for _, c := range []struct {
		name   string
		result SendResult
		err    error
		state  ExportState
		known  bool
	}{
		{"sent", SendResult{Accepted: 3}, nil, ExportSent, true},
		{"partial", SendResult{Accepted: 2, Rejected: 1, PartialSuccess: true}, nil, ExportUnconfirmed, true},
		{"warning", SendResult{Accepted: 3, PartialSuccess: true, Warning: "CANARY_RECEIVER_MESSAGE"}, nil, ExportSent, true},
		{"invalid", SendResult{Rejected: 4, PartialSuccess: true}, nil, ExportUnconfirmed, false},
		{"negative", SendResult{Accepted: 4, Rejected: -1, PartialSuccess: true}, nil, ExportUnconfirmed, false},
		{"partial_transport", SendResult{Accepted: 2, Rejected: 1, PartialSuccess: true, Retryable: true}, context.DeadlineExceeded, ExportUnconfirmed, false},
		{"retry", SendResult{Retryable: true}, context.DeadlineExceeded, ExportRetry, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveSendResult(batch, c.result, c.err)
			require.Equal(t, c.state, got.State)
			require.Equal(t, c.known, got.Accepted != nil)
			require.Equal(t, c.known, got.Rejected != nil)
			require.NotContains(t, got.Warning, "CANARY")
		})
	}
}
