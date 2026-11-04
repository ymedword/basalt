package flow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type stoppingQueryRepository struct {
	WorkerRepository
	query  string
	stop   func(context.Context) error
	called bool
}

func (r *stoppingQueryRepository) OpenRoots(ctx context.Context, _ int) ([]Task, error) {
	if r.query == "open" {
		r.called = true
		return nil, r.stop(ctx)
	}
	return nil, nil
}

func (r *stoppingQueryRepository) ReadyRoots(ctx context.Context, _ int64, _ int) ([]Task, error) {
	r.called = true
	return nil, r.stop(ctx)
}

func TestFlow_WorkerCancellationDuringQueryPreservesOtherErrors(t *testing.T) {
	dbErr := errors.New("database unavailable")
	for _, query := range []string{"open", "ready"} {
		for _, mode := range []string{"cancel", "wrapped-cancel", "joined-cancel", "live-cancel", "live-deadline", "database", "cancel-database", "wrapped-cancel-database", "cancel-joined-database"} {
			t.Run(query+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var returned error
				repository := &stoppingQueryRepository{query: query, stop: func(queryCtx context.Context) error {
					switch mode {
					case "cancel", "wrapped-cancel", "joined-cancel", "cancel-database", "wrapped-cancel-database", "cancel-joined-database":
						cancel()
					}
					switch mode {
					case "cancel":
						returned = queryCtx.Err()
					case "wrapped-cancel":
						returned = fmt.Errorf("query interrupted: %w", queryCtx.Err())
					case "joined-cancel":
						// 与 readFlowRoots 的 errors.Join(rows.Err(), rows.Close()) 同形：
						// 只有一个非空子错误，但节点本身实现 Unwrap() []error。
						returned = errors.Join(queryCtx.Err())
					case "live-cancel":
						returned = context.Canceled
					case "live-deadline":
						returned = context.DeadlineExceeded
					case "database", "cancel-database":
						returned = dbErr
					case "wrapped-cancel-database":
						returned = fmt.Errorf("query: %w", errors.Join(queryCtx.Err(), dbErr))
					case "cancel-joined-database":
						returned = errors.Join(queryCtx.Err(), dbErr)
					}
					return returned
				}}
				cfg := DefaultConfig()
				cfg.Enabled = true
				worker := &Worker{Service: &Service{Config: cfg}, Repository: repository, Startup: func(context.Context) error { return nil }}
				err := worker.Run(ctx)
				require.True(t, repository.called, "cancellation must occur inside the repository query")
				if mode == "cancel" || mode == "wrapped-cancel" || mode == "joined-cancel" {
					require.NoError(t, err, "normal parent cancellation must join and stop successfully")
				} else {
					require.ErrorIs(t, err, returned, "query failure must survive worker shutdown")
				}
				require.False(t, worker.running, "Run must finish its lifecycle before returning")
			})
		}
	}
}
