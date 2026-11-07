//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type us19Harness struct {
	h          *harness
	addr, key  string
	p          *process
	definition map[string]any
	env        []string
}

type us19HarnessOptions struct {
	tools       []string
	beforeServe func(*harness)
	afterServe  func(*harness, string)
	env         []string
	config      func(string) string
}

// Each case owns a real candidate binary, workspace, PG schema, caller key and
// localhost service. The warm-up ordinary Run obtains the actual pinned hashes.
func newUS19Harness(t *testing.T, modelURL string, options ...us19HarnessOptions) *us19Harness {
	t.Helper()
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	var option us19HarnessOptions
	if len(options) > 0 {
		option = options[0]
	}
	// US-24 起崩溃恢复由**过期租约的回收**触发，不再有启动全局扫描：接管发生在
	// TTL 到期后的回收轮次。夹具把执行权时效压到用例可等待的量级（默认 30s TTL
	// 会让每个崩溃窗口白等半分钟），判据仍是终态列，不放宽任何断言。
	// Since US-24 takeover comes from reclaiming expired leases, the fixture shortens the
	// lease timing so crash cases still converge inside their wait budgets.
	config := configWithAddr(modelURL, addr) + fastWorkerSection() +
		"flows:\n  enabled: true\n  max_active: 2\n  poll_interval_ms: 100\n"
	if option.config != nil {
		config = option.config(config)
	}
	h.writeConfig(t, config)
	h.writeAgent(t, "flow-reader", agentFrontmatter("flow-reader", option.tools), "Return only the JSON object required by output_schema. Follow immutable hard constraints.")
	if option.beforeServe != nil {
		option.beforeServe(h)
	}
	p := startServeWithEnv(t, h, addr, append(fixtureEnv(), option.env...))
	if option.afterServe != nil {
		option.afterServe(h, addr)
	}
	warm, err := postInvoke(addr, "flow-reader", "Warm up the ordinary path; return a JSON answer.")
	require.NoError(t, err)
	require.Equal(t, "completed", warm.Status)
	db := h.openDB(t)
	var configHash, manifestHash, raw string
	require.NoError(t, db.QueryRow(`SELECT r.config_hash,m.manifest_hash,m.snapshot_json FROM runs r JOIN run_resource_manifests m ON m.run_id=r.run_id WHERE r.run_id=$1`, warm.RunID).Scan(&configHash, &manifestHash, &raw))
	var manifest struct {
		AgentUID string `json:"agent_uid"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &manifest))
	require.NotEmpty(t, manifest.AgentUID)
	life := readLifecycleStateFile(t, h, "flow-reader")
	require.Equal(t, manifest.AgentUID, life.AgentUID)
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	budget := map[string]any{"max_total_tokens": 100000, "max_tool_calls": 10, "max_duration_seconds": 60, "max_depth": 3, "max_children": 16, "max_attempts_per_node": 3, "max_revisions": 2}
	definition := map[string]any{"format_version": 1, "name": "us19-binary", "entry": "draft", "task_contract": "TASK.json", "limits": budget, "nodes": []any{map[string]any{"id": "draft", "kind": "agent", "agent": map[string]any{"uid": manifest.AgentUID, "name": "flow-reader", "version": life.Generation, "config_hash": configHash, "manifest_hash": manifestHash}, "output_schema": schema, "failure": map[string]any{"max_attempts": 3}}}, "contract": map[string]any{"contract_version": 1, "goal": "produce a JSON answer", "constraints": []string{"keep this hard constraint"}, "input_schema": schema, "output_schema": schema, "budget": budget, "stop_conditions": []string{"budget", "deadline", "unknown"}, "escalation": "show unresolved", "artifact_spec": []any{map[string]any{"id": "report", "format": "json", "required": true}}, "acceptance": []any{map[string]any{"id": "shape", "version": 1, "kind": "deterministic", "target_kind": "artifact", "target_id": "report", "required": true, "schema": schema}}}}
	u := &us19Harness{h: h, addr: addr, p: p, definition: definition, env: append(fixtureEnv(), option.env...)}
	u.key = us19IssueKey(t, addr)
	h.registerSecret("BASALT_US19_CALLER_KEY", u.key)
	return u
}

func us19IssueKey(t *testing.T, addr string, concurrency ...int) string {
	t.Helper()
	maximum := 8
	if len(concurrency) > 0 {
		maximum = concurrency[0]
	}
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/callers", mustJSON(map[string]any{"name": fmt.Sprint("us19-caller-", maximum), "limits": map[string]any{"qps": 100, "burst": 100, "max_concurrent": maximum, "tasks_per_period": 10000, "tokens_per_period": 1000000, "period": "day", "timezone": "UTC"}}), nil)
	requireWebStatus(t, resp, 200, "OK", "", "create Flow caller")
	var caller struct {
		ID string `json:"caller_id"`
	}
	decodeWebData(t, resp, &caller, "create Flow caller")
	resp = webRequest(t, addr, http.MethodPost, "/api/v1/callers/"+caller.ID+"/keys", mustJSON(map[string]any{"scopes": []string{"flows:read", "flows:write", "agents:invoke:flow-reader"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}), nil)
	requireWebStatus(t, resp, 200, "OK", "", "issue Flow key")
	var issued struct {
		Key string `json:"key"`
	}
	decodeWebData(t, resp, &issued, "issue Flow key")
	require.NotEmpty(t, issued.Key)
	return issued.Key
}

func (u *us19Harness) publish(t *testing.T) string {
	t.Helper()
	resp := webRequest(t, u.addr, http.MethodPost, "/api/v1/admin/flows/publish", mustJSON(map[string]any{"operation_id": "publish-us19", "expected_version": 0, "definition": u.definition}), nil)
	requireWebStatus(t, resp, 200, "OK", "", "publish Flow")
	var value struct {
		UID string `json:"definition_uid"`
	}
	decodeWebData(t, resp, &value, "publish Flow")
	require.NotEmpty(t, value.UID)
	return value.UID
}

func (u *us19Harness) headers(key string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + u.key, "Idempotency-Key": key}
}

func (u *us19Harness) start(t *testing.T, uid, key string) *webResponse {
	t.Helper()
	return webRequest(t, u.addr, http.MethodPost, "/api/v1/flow-tasks", mustJSON(map[string]any{"definition_uid": uid, "input": map[string]any{"answer": "immutable input"}}), u.headers(key))
}

// waitStatus 等到根任务到达期望状态。US-24 起接管由**过期租约的回收轮次**驱动
// （FR-24-02 / 卡片 §7 Q2：恢复最多等一个 TTL），不再是启动即扫描；预算因此覆盖
// 夹具压缩后的 TTL（6s）+ 回收间隔（1s）+ 收尾耗时，判据仍是精确的终态值。
// The budget now covers the fixture's shortened lease TTL plus a reclaim round; the
// verdict is still the exact terminal status.
func (u *us19Harness) waitStatus(t *testing.T, id, status string) {
	t.Helper()
	db := u.h.openDB(t)
	var last string
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT status FROM flow_tasks WHERE task_id=$1`, id).Scan(&last) == nil && last == status
	}, 30*time.Second, 20*time.Millisecond, "task %s expected %s, last %s", id, status, last)
}

func (u *us19Harness) restart(t *testing.T) {
	t.Helper()
	u.p.kill()
	u.p.waitExit(t, 10*time.Second)
	u.p = startServeWithEnv(t, u.h, u.addr, u.env)
}

// waitRunOver 等到某个根的叶子 Run 被接管方按 A1 收尾（进入终态）。US-24 起崩溃恢复
// 只作用于**合法领取的过期租约**（FR-24-02），因此这个等待同时是"接管真的发生过"
// 的证据：它不依赖启动扫描，也不靠心跳推断死亡。
// It waits until the root's leaf run was closed by a lease-driven takeover.
func (u *us19Harness) waitRunOver(t *testing.T, taskID string) {
	t.Helper()
	db := u.h.openDB(t)
	require.Eventually(t, func() bool {
		var total, closed int
		if err := db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE status IN ('failed','completed')) FROM runs WHERE flow_task_id=$1`, taskID).Scan(&total, &closed); err != nil {
			return false
		}
		return total > 0 && total == closed
	}, 30*time.Second, 20*time.Millisecond, "task %s expected its leaf run closed by the takeover reclaimer", taskID)
}

// requireCloseAccepted 接受关闭控制的两种合法回应：已收敛的 200，或关闭意图已受理、
// 本地执行仍在收束的 202（`flowWriteView` 对 accepted 返回 202，见
// internal/web/handlers_flow.go:101-108；US-19 起的既定契约）。两者 code 都是 OK。
// 202 只证明受理，不证明收敛——调用方必须另以精确终态等待证明结果，不得把 202
// 当失败，也不得把它当成功。
// Accepts either the converged 200 or the accepted-but-still-joining 202; callers
// must still wait for the exact terminal status.
func requireCloseAccepted(t *testing.T, resp *webResponse, label string) {
	t.Helper()
	if resp.Status != 200 && resp.Status != 202 {
		t.Fatalf("%s：HTTP = %d，期望 200 或 202（信封 %s）", label, resp.Status, resp.Raw)
	}
	if resp.Env.Code != "OK" {
		t.Fatalf("%s：code = %q，期望 %q（信封 %s）", label, resp.Env.Code, "OK", resp.Raw)
	}
}

// flowStatusTerminal 给出 Flow 任务状态的终态集合（与 `TaskStatus.Terminal` 同一口径，
// e2e 不能 import internal/flow 的内部判定，只能复制字面量并在此标注来源）。
// Terminal Flow task statuses, mirroring internal/flow/state.go.
func flowStatusTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "stopped", "cancelled", "abandoned":
		return true
	}
	return false
}

func (u *us19Harness) control(t *testing.T, id, action, op string) *webResponse {
	t.Helper()
	current := webRequest(t, u.addr, http.MethodGet, "/api/v1/flow-tasks/"+id, "", u.headers(""))
	requireWebStatus(t, current, 200, "OK", "", "read before control")
	headers := u.headers("")
	headers["If-Match"] = current.Header.Get("ETag")
	return webRequest(t, u.addr, http.MethodPost, fmt.Sprintf("/api/v1/flow-tasks/%s/%s", id, action), mustJSON(map[string]any{"operation_id": op, "reason": "reconcile the original persisted outcome"}), headers)
}
