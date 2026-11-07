//go:build integration

package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// 本文件是**跨 Worker 取消的服务层接线**验收（US-24 T024 / FR-24-22 / C-16）。
// 仓储级判据（标志只写不改状态、续约带回、终态幂等）在
// `internal/storage/postgres/run_lease_test.go` 的 `TestCancel_RemoteWorkerObservesFlag`
// （模块卡 §4.4.6）；这里补的是**服务层是否真的走了那条路**——否则
// `AgentService.Cancel` 的"未命中本地在途表"分支只会记一条日志，取消请求永远到不了
// 属主（这正是 C-16 的违反症状：UI 显示已取消但仍在跑）。
// Service-level wiring for cross-worker cancel; the storage-level criteria live in
// postgres.TestCancel_RemoteWorkerObservesFlag.

// TestCancel_NonOwnerRoutesThroughRequestCancel 判非属主取消：`AgentService.Cancel` 在
// 本地没有该 Run 时经 `RequestCancel` 只写 `runs.cancel_requested`，返回真实状态
// （仍是 running，不假装成功）；属主下一次续约看到标志并能自行收敛；已终态再取消幂等。
func TestCancel_NonOwnerRoutesThroughRequestCancel(t *testing.T) {
	ctx := context.Background()
	db := pgtest.NewTestDB(t)
	runs := postgres.NewRunStore(db)
	const (
		runID     = "run-cancel-remote"
		sessionID = "s-cancel-remote"
		ownerID   = "worker-owner"
	)

	// 属主是另一个 Worker：服务实例（所谓"非属主进程"）里没有这条在途登记。
	appSeedRun(t, db, runID, sessionID, ownerID)
	owner, err := runs.LookupAdmittedRun(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, ownerID, owner.OwnerWorkerID, "回查到的属主必须是种下的那个 Worker")

	svc := core.NewAgentService(core.ServiceOptions{Runs: runs})

	detail, err := svc.Cancel(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, core.StatusRunning, detail.Status, "非属主只写标志，不声称已取消")

	renewed, err := postgres.NewLeaseStore(db).Renew(ctx, owner, 30*time.Second)
	require.NoError(t, err)
	require.True(t, renewed.CancelRequested, "属主必须在续约响应里看到取消请求")

	// 属主自行收敛；已终态之后再次取消必须幂等。
	applied, err := runs.FinishRun(ctx, runID, owner,
		core.StatusCancelled, "owner observed the flag", 0, 0, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, applied)
	detail, err = svc.Cancel(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, core.StatusCancelled, detail.Status, "已终态再取消：幂等成功")
}
