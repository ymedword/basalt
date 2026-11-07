//go:build integration

package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func flowParentForTest(t *testing.T, s *FlowStore, child flow.DefinitionVersion) flow.DefinitionVersion {
	t.Helper()
	def := flowStoreDefinition()
	def.Name = "parent"
	def.Entry = "child"
	def.Nodes = []flow.Node{{ID: "child", Kind: "subflow", Subflow: &flow.DefinitionRef{UID: child.DefinitionUID, Version: child.Version}, OutputSchema: def.Contract.OutputSchema, Failure: flow.FailurePolicy{MaxAttempts: 1}}}
	require.NoError(t, NewManagementStore(s.db).BeginOperation(t.Context(), core.OperationRecord{OperationID: "publish-parent", Action: "flow.publish", TargetKind: "flow_definition", TargetID: "fd_parent", ActorSubject: "admin", AuthMethod: core.AuthMethodCLI, Source: "cli", RequestedVersionID: 1, CreatedAt: time.Now()}))
	version, err := s.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: "fd_parent", ExpectedVersion: 0, OperationID: "publish-parent", Definition: def})
	require.NoError(t, err)
	return version
}

func TestFlow_SubflowUsesOnlyLeafRunAndFrozenIdentity(t *testing.T) {
	for _, deferOnce := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "capacity-yield"}[deferOnce], func(t *testing.T) {
			s, child := newFlowStoreTest(t)
			parent := flowParentForTest(t, s, child)
			service := flowServiceForStore(s)
			runner := &flowAdvanceRunner{store: s, output: json.RawMessage(`{"answer":"child verified output"}`), capacity: deferOnce}
			service.Runner = runner
			task, err := service.Start(t.Context(), startFlowRequest(parent, "parent-start"))
			require.NoError(t, err)
			// 子任务行按自身 root_task_id 查父根租约：预领整棵子树的根。
			// 凭证必须原样下传：子 Run 创建（createRunForNode）以它证明父根归属。
			root := flowRootLeaseFor(t, s.db, task.RootTaskID)
			task, err = service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Lease: root})
			require.NoError(t, err)
			if deferOnce {
				require.Equal(t, flow.Running, task.Status)
				require.Equal(t, 1, task.ChildrenCreated)
				require.Equal(t, 1, task.Revision.Attempts["child"])
				runner.capacity = false
				service.Now = func() time.Time { return time.Now().Add(time.Second) }
				task, err = service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Lease: root})
				require.NoError(t, err)
			}
			require.Equal(t, flow.Succeeded, task.Status)
			require.Equal(t, 1, task.ChildrenCreated)
			require.Equal(t, 2, task.Revision.ExpandedSteps, "root counts both static delegation and the real child leaf")
			var tasks, runs, rootRuns int
			require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_tasks`).Scan(&tasks))
			require.Equal(t, 2, tasks)
			require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&runs))
			require.Equal(t, 1, runs, "static root consumes no Run quota")
			require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM runs WHERE flow_task_id=$1`, task.ID).Scan(&rootRuns))
			require.Zero(t, rootRuns)
			var childID, owner, kind, key string
			var version, deadline int64
			require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT task_id,caller_id,authorizing_kind,COALESCE(authorizing_key_id,''),definition_version,deadline FROM flow_tasks WHERE parent_task_id=$1`, task.ID).Scan(&childID, &owner, &kind, &key, &version, &deadline))
			require.NotEmpty(t, childID)
			require.Equal(t, task.CallerID, owner)
			require.Equal(t, task.AuthorizingKind, kind)
			require.Equal(t, task.AuthorizingKeyID, key)
			require.Equal(t, child.Version, version)
			require.LessOrEqual(t, deadline, task.Deadline)
		})
	}
}
