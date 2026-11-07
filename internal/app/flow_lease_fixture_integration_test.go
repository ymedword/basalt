//go:build integration

package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// 直接调 Service.Advance / Claim / ParkTask 的夹具没有 Worker 的领取环节，按生产
// 同形补一次真实的 `flow_root` 租约：推进类写入（节点领取、提交、ParkTask、写
// Queued）与子 Run 创建都在**同一事务**里校验目标根自己的租约（US-24 §3.1.6 /
// P-04 / FR-24-18）。生产由 `flow.Worker` 按真实身份领取，夹具显式持有，让用例走
// 与生产同形的路径，而不是绕过执行权。
//
// 领取必须**每次推进前**领、推进返回后放：管理面 Recover / Revise 会自己领取同一行
// 租约，行上已有活租约时 Acquire 返回 ErrLeaseHeld，管理面据此 409 FLOW_ROOT_BUSY
// （§2.3.6 / FR-24-37）——夹具跨调用持有会把管理面路径误判成冲突。
// Fixtures that call Service.Advance / Claim / ParkTask directly take a real
// flow_root lease for the duration of one call only: management Recover / Revise
// acquire the same row themselves and must not see a lease held by the fixture.

const flowLeaseFixtureInstance = "flow-lease-fixture"

// flowLeaseFixtureIdentity 是夹具的执行者身份；非零 Identity 才写租约行。
// A non-zero identity is what makes Acquire write the lease row.
func flowLeaseFixtureIdentity() core.WorkerIdentity {
	return core.WorkerIdentity{InstanceID: flowLeaseFixtureInstance, Name: flowLeaseFixtureInstance}
}

// acquireFlowRootForTest 领取目标根的 flow_root 租约，返回可释放的凭证。
func acquireFlowRootForTest(t *testing.T, db *postgres.DB, rootTaskID string) core.LeaseToken {
	t.Helper()
	lease, err := postgres.NewLeaseStore(db).Acquire(t.Context(), core.LeaseFlowRoot, rootTaskID,
		flowLeaseFixtureIdentity(), 5*time.Minute)
	require.NoError(t, err)
	return lease.LeaseToken
}

// releaseFlowRootForTest 释放夹具持有的根租约（Release 本身幂等）。
//
// 上下文用 `WithoutCancel(t.Context())`：`t.Context()` 在 Cleanup 函数执行**之前**
// 就被取消，直接用它会让 `t.Cleanup(func(){ releaseFlowRootForTest(...) })` 必然
// 报 "context canceled"。释放租约是收尾动作，不应随测试上下文结束而放弃。
// The context survives cancellation because t.Context() dies before Cleanup runs.
func releaseFlowRootForTest(t *testing.T, db *postgres.DB, token core.LeaseToken) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancel()
	require.NoError(t, postgres.NewLeaseStore(db).Release(ctx, token))
}

// advanceUnderFlowRoot 以真实执行者身份包一次 Service.Advance：领租约 → 带 token
// 推进 → 放租约。返回值与错误原样透传，调用方的既有断言不变。
func advanceUnderFlowRoot(t *testing.T, db *postgres.DB, service *flow.Service,
	rootTaskID string, req flow.AdvanceRequest,
) (flow.Task, error) {
	t.Helper()
	token := acquireFlowRootForTest(t, db, rootTaskID)
	defer releaseFlowRootForTest(t, db, token)
	req.Lease = token
	return service.Advance(t.Context(), req)
}

// expireFlowRootLeaseForTest 把目标根的租约改成已过期：模拟原持有者崩溃 / 暂停后
// TTL 到期。回收只认"已过期或已释放"，不得按心跳或启动时间推断死亡（FR-24-15）；
// 随后以合法领取（TookOver=true）接手才允许写库。
// Expiring the row models a crashed owner; recovery must come from a lawful claim.
func expireFlowRootLeaseForTest(t *testing.T, db *postgres.DB, rootTaskID string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `UPDATE execution_leases
   SET expires_at = (extract(epoch from clock_timestamp()) * 1000)::bigint - 1000
 WHERE kind = 'flow_root' AND resource_id = $1 AND released_at IS NULL`, rootTaskID)
	require.NoError(t, err)
}
