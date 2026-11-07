package flow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
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

// parkingRepository 记录 ParkTask 调用；未覆盖的 Repository 方法经内嵌接口转发，
// 测试里真走到它们会直接 panic——正好暴露意外路径。
// parkingRepository records ParkTask calls; any other repository use panics.
type parkingRepository struct {
	Repository
	task   Task
	parked []string
}

func (r *parkingRepository) Read(context.Context, core.Principal, string) (Task, error) {
	return r.task, nil
}

func (r *parkingRepository) ParkTask(_ context.Context, _ core.Principal, _ string, _ TaskVersion, reason string) error {
	r.parked = append(r.parked, reason)
	return nil
}

// TestFlow_WorkerReworkContentionKeepsRoutingReason —— 两个 Worker 同时拿到同一个
// blocked + bounded_rework_required 根时，落败方的 rework → Revise 自取根租约撞忙
// （ErrFlowRootBusy）。这不是推进失败：不得停靠、不得把 reason 改写成
// advance_unavailable——A3 靠 reason 分路，持有者的 AcceptRevision 还会因
// state_version CAS 失败丢掉这次返工（评审 IDX-14）。
// Losing the root-lease race is contention, not an advance failure: the row keeps its
// routing reason and the pending bounded rework.
func TestFlow_WorkerReworkContentionKeepsRoutingReason(t *testing.T) {
	task := Task{ID: "root-1", RootTaskID: "root-1", Status: Blocked, Reason: "bounded_rework_required", CloseIntent: CloseNone}
	repository := &parkingRepository{task: task}
	worker := &Worker{Service: &Service{Repository: repository}}
	item := workItem{task: task, principal: core.LocalAdminCLIPrincipal()}
	ctx := context.Background()

	for _, ignored := range []error{core.ErrPreconditionFailed, ErrTaskClosed, ErrCapacity, ErrFlowRootBusy} {
		worker.handleJobError(ctx, item, ignored)
	}
	require.Empty(t, repository.parked, "竞争 / 会话语义错误不得停靠改写 reason")

	worker.handleJobError(ctx, item, errors.New("database unavailable"))
	require.Equal(t, []string{"advance_unavailable"}, repository.parked, "真实失败仍须停靠保留检查点")
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
