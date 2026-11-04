package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/flow"
)

func TestFlow_ClosePreservesDrainFailureJoinedWithCancellation(t *testing.T) {
	for _, mode := range []string{"cancel-only", "drain-only", "cancel-and-drain"} {
		t.Run(mode, func(t *testing.T) {
			var result error
			switch mode {
			case "cancel-only":
				result = context.Canceled
			case "drain-only":
				result = fmt.Errorf("%w: flow drain exceeded 35s", flow.ErrDrainTimeout)
			case "cancel-and-drain":
				result = errors.Join(context.Canceled, fmt.Errorf("%w: flow drain exceeded 35s", flow.ErrDrainTimeout))
			}
			done := make(chan struct{})
			close(done)
			r := &flowRuntime{cancel: func() {}, done: done, runErr: result}
			for range 2 {
				err := r.Close()
				if mode == "cancel-only" {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, flow.ErrDrainTimeout)
					require.ErrorContains(t, err, "flow drain exceeded 35s")
				}
			}
		})
	}
}

func TestFlow_ClosePreservesDatabaseErrorJoinedWithCancellation(t *testing.T) {
	dbErr := errors.New("database unavailable")
	for _, result := range []error{
		dbErr,
		errors.Join(context.Canceled, dbErr),
		fmt.Errorf("worker stopped: %w", errors.Join(context.Canceled, dbErr)),
		errors.Join(fmt.Errorf("query: %w", dbErr), context.Canceled),
	} {
		done := make(chan struct{})
		close(done)
		r := &flowRuntime{cancel: func() {}, done: done, runErr: result}
		for range 2 {
			require.ErrorIs(t, r.Close(), dbErr, "normal cancellation must not swallow a database failure")
		}
	}
}
