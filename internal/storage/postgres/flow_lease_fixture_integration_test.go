//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// acquireFlowRootLease 为测试根领取一枚真实的 flow_root 租约（US-24 §3.1.6 / P-04）：
// `ClaimNode` / `CommitNode` / `Revise` / `ParkTask` 等推进类写入都按目标行的
// `root_task_id` 校验父根租约，夹具不能再退化成"无租约写入"。租约走真实
// LeaseStore（数据库时钟 + 行锁），不是替身。
// The fixture must hold a real flow_root lease: advance-class writes are fenced by
// it, and using the real store keeps the check identical to production.
func acquireFlowRootLease(t *testing.T, db *DB, rootIDs ...string) {
	t.Helper()
	for _, root := range rootIDs {
		if root == "" {
			continue
		}
		flowRootLeaseFor(t, db, root)
	}
}

// flowRootLeaseFor 领取并返回一枚根租约 token，供夹具在单次调用后主动释放——
// 管理面 Revise / Recover 会自取租约，夹具长期占着会变成 ErrFlowRootBusy。
// flowRootLeaseFor returns the token so a fixture can release it before a
// management call self-acquires the same root.
func flowRootLeaseFor(t *testing.T, db *DB, root string) core.LeaseToken {
	t.Helper()
	lease, err := NewLeaseStore(db).Acquire(t.Context(), core.LeaseFlowRoot, root,
		core.WorkerIdentity{InstanceID: "flow-store-fixture", Name: "fixture"}, time.Hour)
	require.NoError(t, err)
	return lease.LeaseToken
}

// parkFlowWithLease 以一次性根租约执行 ParkTask：ParkTask 先写 running（推进类），
// 随后必须释放，避免挡住同测试里的管理面 Revise（自取租约）。
// ParkTask writes an advance-class running state, so the fixture leases the root for
// that call only and releases it immediately.
func parkFlowWithLease(t *testing.T, s *FlowStore, p core.Principal, id string, expected flow.TaskVersion, reason string) {
	t.Helper()
	lease := flowRootLeaseFor(t, s.db, id)
	defer func() {
		require.NoError(t, NewLeaseStore(s.db).Release(t.Context(), lease))
	}()
	require.NoError(t, s.ParkTask(t.Context(), p, id, expected, reason))
}
