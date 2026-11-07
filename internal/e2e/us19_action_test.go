//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlow_WriteActionCrashKeepsOriginalIdentityAndNeverReplays(t *testing.T) {
	for _, window := range []string{"known-audit-before-flow-result", "effect-before-audit-terminal"} {
		t.Run(window, func(t *testing.T) {
			model := newFakeModel(t, usageResponse(`{"answer":"warm"}`, 5, 3))
			u := newUS19Harness(t, model.URL(), us19HarnessOptions{tools: []string{"write_file"}})
			node := u.definition["nodes"].([]any)[0].(map[string]any)
			node["kind"], node["tool"] = "tool", "write_file"
			node["inputs"] = []any{map[string]any{"target": "path", "literal": "gft-output.txt"}, map[string]any{"target": "content", "literal": "controlled binary write bytes"}}
			schema := map[string]any{"type": "object", "properties": map[string]any{"content": map[string]any{"type": "string"}, "retryable": map[string]any{"type": "boolean"}, "truncated": map[string]any{"type": "boolean"}}, "required": []string{"content"}, "additionalProperties": false}
			node["output_schema"] = schema
			contract := u.definition["contract"].(map[string]any)
			contract["output_schema"] = schema
			contract["acceptance"].([]any)[0].(map[string]any)["schema"] = schema
			uid := u.publish(t)
			fault := us19Fault{"flow_artifacts", "INSERT", "true"}
			if window == "effect-before-audit-terminal" {
				fault = us19Fault{"tool_invocations", "UPDATE", "NEW.status='success'"}
			}
			wait, remove := us19InstallFault(t, u, fault)
			response := u.start(t, uid, "fixed-write-once")
			requireWebStatus(t, response, 202, "OK", "", "accept controlled write")
			var task struct {
				ID string `json:"task_id"`
			}
			decodeWebData(t, response, &task, "accept controlled write")
			pid := wait()
			path := filepath.Join(u.h.Root, ".basalt", "agents", "flow-reader", "gft-output.txt")
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "controlled binary write bytes", string(body))
			before, err := os.Stat(path)
			require.NoError(t, err)
			db := u.h.openDB(t)
			var actionID, attemptID, runID string
			require.NoError(t, db.QueryRow(`SELECT a.action_id,a.action_attempt_id,a.run_id FROM flow_action_attempts a WHERE task_id=$1`, task.ID).Scan(&actionID, &attemptID, &runID))
			u.p.kill()
			u.p.waitExit(t, 10*time.Second)
			var terminated bool
			require.NoError(t, db.QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
			require.True(t, terminated)
			remove()
			u.p = startServe(t, u.h, u.addr)
			// US-24：启动不再做全局扫描；A1 按**过期租约**收尾叶子 Run（FR-24-02）。之后
			// 两种窗口分道：
			//   - 未决动作存在 → `Recover` 在写库前返回 ErrActionUnknown（R4–R7 / FR-24-37），
			//     根保持非终态、动作守卫原样保留，只能由管理面查证（resume 409 / cancel 200）；
			//   - 动作与审计都已落定（known）→ A3 按 worker_lease_expired 把 blocked 根重入
			//     queued，普通 Worker 复用已完成节点的运行证据，无需人工 resume（FR-24-37）。
			// The crash window now resolves through lease-driven reclamation; a known action
			// converges without an administrative resume, an unresolved one never does.
			u.waitRunOver(t, task.ID)
			if window == "effect-before-audit-terminal" {
				require.Equal(t, 1, scalarInt(t, db, `SELECT count(*) FROM flow_actions WHERE task_id=$1 AND outcome IN ('dispatching','unknown')`, task.ID), "未决动作必须保持守卫直到人工查证")
				var rootStatus string
				require.NoError(t, db.QueryRow(`SELECT status FROM flow_tasks WHERE task_id=$1`, task.ID).Scan(&rootStatus))
				require.False(t, flowStatusTerminal(rootStatus), "有未决动作的根不得被自动收为终态")
				resume := u.control(t, task.ID, "resume", "resume-controlled-write")
				requireWebStatus(t, resume, 409, "FLOW_ACTION_UNKNOWN", "", "unknown write must not replay")
				var held int64
				require.NoError(t, db.QueryRow(`SELECT COALESCE(sum(reserved_tool_calls),0) FROM flow_budget_allocations WHERE task_id=$1 AND state IN ('pending','unknown')`, task.ID).Scan(&held))
				require.EqualValues(t, 1, held)
				closed := u.control(t, task.ID, "cancel", "close-controlled-write")
				requireCloseAccepted(t, closed, "close unknown write without clearing its effect")
				// 202 只表示关闭意图已受理；收敛由 sweepClosures 在可收敛后推进，
				// 这里以精确终态证明它真的收敛，且不改变下面"零重放"的判据。
				u.waitStatus(t, task.ID, "cancelled")
			} else {
				u.waitStatus(t, task.ID, "succeeded")
			}
			after, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, before.ModTime(), after.ModTime(), "the controlled file must not be rewritten")
			require.Equal(t, 1, scalarInt(t, db, `SELECT count(*) FROM tool_invocations WHERE run_id=$1`, runID))
			require.Equal(t, 1, scalarInt(t, db, `SELECT count(*) FROM flow_node_attempts WHERE task_id=$1`, task.ID))
			require.Equal(t, 1, scalarInt(t, db, `SELECT count(*) FROM runs WHERE flow_task_id=$1`, task.ID))
			require.Equal(t, 0, scalarInt(t, db, `SELECT count(*) FROM llm_calls WHERE run_id=$1`, runID))
			require.Len(t, model.Requests(), 1, "FixedTool must not make any model call after warm-up")
			var actualAction, actualAttempt string
			require.NoError(t, db.QueryRow(`SELECT action_id,action_attempt_id FROM flow_action_attempts WHERE task_id=$1`, task.ID).Scan(&actualAction, &actualAttempt))
			require.Equal(t, actionID, actualAction)
			require.Equal(t, attemptID, actualAttempt)
			t.Logf("real_process_killed=true window=%s task=%s run=%s action=%s attempt=%s file_write_audits=1 model_calls=0", window, task.ID, runID, actionID, attemptID)
		})
	}
}
