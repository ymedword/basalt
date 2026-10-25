package app

// 本文件是颗粒度文档 §4.5.1–§4.5.3 的验收面：保存期 env 闸门（创建 / 修改 / 发布 /
// 回滚四个入口）+ 幂等发布短路 + 错误与 detail 脱敏。判据是"**预写之后、第一次写文件
// 之前** fail-closed"：503（app 层为 `core.ErrProviderNotConfigured`）+ `operation_id`、
// 操作行 failed / NOT_CONFIGURED、定义文件字节 / 版本 / 指针零变化，且错误与
// `detail_json` 不含任何 env 值。
// These cases pin the save-time env gate: refuse after the pending row and before the
// first write, audit the failure, and never echo a value anywhere.
//
// 说明：卡片里的 `env.mcpPointer(t, name)` 按"实现后以实际签名为准修正"的约定落在
// 既有 `pointerOf(kind, name)` 之上（薄包装，避免误用只查模型连接的 `pointer`）。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// mcpAbsentTokenEnv 是本文件的"缺失变量"：用例显式置空（空值 = 缺失，与
	// resolveMCPEnv 同判据），不依赖开发机是否恰好设置了同名变量。
	mcpAbsentTokenEnv = "MCP_ABSENT_TOKEN"
	// mcpSecondTokenEnv 是回滚用例 v2 的变量：它已就绪，而回滚目标 v1 的变量缺失——
	// 这样"闸门校验的是目标版本条目"才有判别力。
	mcpSecondTokenEnv = "MCP_SECOND_TOKEN"
	// mcpEnvSecretSentinel 是"已就绪变量"的值：它只允许存在于进程环境里，不得出现在
	// 错误消息、操作详情或日志中。
	mcpEnvSecretSentinel = "mcp-env-secret-sentinel-not-a-credential"
)

// mcpPointer 取 MCP server 的发布指针（`pointer` 只查模型连接，不能混用）。
// mcpPointer reads the MCP publish pointer; the model-connection helper cannot be reused.
func (e *resourceEnv) mcpPointer(t *testing.T, name string) core.ResourcePointer {
	e.t.Helper()
	return e.pointerOf(core.ResourceMCPServer, name)
}

// envGateDetail 是 §2.3 图 2-2 的 `detail_json` 形态：只有分类与点名字段，
// 没有任何 env 值字段——脱敏是结构性质，不靠调用方自觉。
// envGateDetail mirrors the gate's detail payload: classification only, never values.
type envGateDetail struct {
	ErrorCode    string `json:"error_code"`
	Stage        string `json:"stage"`
	FailureClass string `json:"failure_class"`
	ErrorMessage string `json:"error_message"`
}

// requireEnvGateOperation 断言操作行是 failed / NOT_CONFIGURED，并返回解析后的
// detail（`stage=preflight`、`failure_class=env_missing`）。
// requireEnvGateOperation asserts the failed audit row and returns its detail.
func requireEnvGateOperation(t *testing.T, env *resourceEnv, operationID string) envGateDetail {
	t.Helper()
	row, err := env.ops.GetOperation(context.Background(), operationID)
	require.NoError(t, err, "503 必须能按 operation_id 查到操作行")
	require.Equal(t, core.OperationFailed, row.Status)
	require.Equal(t, resourceResultNotConfigured, row.ResultCode)

	var detail envGateDetail
	require.NoError(t, json.Unmarshal(row.DetailJSON, &detail))
	require.Equal(t, resourceResultNotConfigured, detail.ErrorCode)
	require.Equal(t, "preflight", detail.Stage)
	require.Equal(t, "env_missing", detail.FailureClass)
	return detail
}

// TestMCPEnvGate_CreateMissingEnvNoWrite 覆盖 §4.5.1：创建条目引用的变量缺失 / 为空
// → 503 + operation_id、操作行 failed/NOT_CONFIGURED、文件 / 版本 / 指针零变化。
// 逆改点：把校验放到 CommitMCPServersDefinition 之后、先建指针再校验、detail 写入
// env 值、不填 operation_id → 对应断言红。
// TestMCPEnvGate_CreateMissingEnvNoWrite covers §4.5.1 (create path, zero write).
func TestMCPEnvGate_CreateMissingEnvNoWrite(t *testing.T) {
	// 不入 t.Parallel：本文件改写进程环境（t.Setenv），并行会与其它用例互相干扰。
	env := newResourceEnv(t)
	env.writeMCPConfig("servers:\n  - name: sibling\n    transport: stdio\n    command: allowed-mcp\n")
	t.Setenv(mcpAbsentTokenEnv, "")

	before := env.readMCPConfig()
	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      "demo",
		Transport: "stdio",
		Command:   "mcp-helper",
		Env:       map[string]string{mcpAbsentTokenEnv: "${" + mcpAbsentTokenEnv + "}"},
		Create:    true,
	})
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, core.ErrProviderNotConfigured, adminErr.Kind, "缺 env 必须按 NOT_CONFIGURED 拒绝（web 层映射 503）")
	require.Contains(t, err.Error(), "demo", "错误必须点名 server")
	require.Contains(t, err.Error(), mcpAbsentTokenEnv, "错误必须点名变量")

	operationID, ok := adminErr.Data["operation_id"].(string)
	require.True(t, ok, "503 必须带 operation_id")
	require.NotEmpty(t, operationID)

	require.Equal(t, before, env.readMCPConfig(), "闸门失败不得写定义文件")
	require.Empty(t, env.versionsOfKind(core.ResourceMCPServer, "demo"), "闸门失败不得追加版本")
	_, err = env.store.Pointer(context.Background(), core.ResourceMCPServer, "demo")
	require.ErrorIs(t, err, core.ErrResourceNotFound, "闸门失败不得留下发布指针")

	detail := requireEnvGateOperation(t, env, operationID)
	require.Contains(t, detail.ErrorMessage, mcpAbsentTokenEnv)
	require.NotContains(t, detail.ErrorMessage, mcpEnvSecretSentinel)
}

// TestMCPEnvGate_UpdateMissingEnvNoWrite 覆盖 §4.5.1 的修改入口：已发布 v1 之后，
// 编辑出引用缺失变量的 v2 → 503、文件仍是 v1 字节、指针仍指 v1、不追加版本。
// 逆改点：修改路径漏接闸门（只接创建）→ 文件被改写 / 出现 v2 → 红。
// TestMCPEnvGate_UpdateMissingEnvNoWrite covers the update entry point.
func TestMCPEnvGate_UpdateMissingEnvNoWrite(t *testing.T) {
	env := newResourceEnv(t)
	v1 := env.createMCPStdio(t, "demo", "allowed-mcp")
	t.Setenv(mcpAbsentTokenEnv, "")

	before := env.readMCPConfig()
	beforePointer := env.mcpPointer(t, "demo")

	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      "demo",
		Transport: "stdio",
		Command:   "other-mcp",
		Env:       map[string]string{mcpAbsentTokenEnv: "${" + mcpAbsentTokenEnv + "}"},
		IfMatch:   env.currentETagOf(t, core.ResourceMCPServer, "demo"),
	})
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, core.ErrProviderNotConfigured, adminErr.Kind)

	operationID, ok := adminErr.Data["operation_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, operationID)

	require.Equal(t, before, env.readMCPConfig(), "闸门失败不得改写定义文件")
	require.Equal(t, beforePointer, env.mcpPointer(t, "demo"), "闸门失败不得推进指针")
	versions := env.versionsOfKind(core.ResourceMCPServer, "demo")
	require.Len(t, versions, 1, "闸门失败不得追加版本")
	require.Equal(t, v1.ID, versions[0].ID)

	requireEnvGateOperation(t, env, operationID)
}

// TestMCPEnvGate_PublishMissingEnvNoWrite 覆盖 §4.5.2：**显式传版本**（即使等于当前
// published）不是幂等分支，必须走闸门；缺变量 → 503、文件 / published / applied 零变化。
// 逆改点：Publish 路径漏接闸门、把显式版本当幂等 → 文件被改写 / 指针前进 → 红。
// TestMCPEnvGate_PublishMissingEnvNoWrite covers the explicit republish path.
func TestMCPEnvGate_PublishMissingEnvNoWrite(t *testing.T) {
	env := newResourceEnv(t)
	env.createMCPStdio(t, "demo", "allowed-mcp")
	t.Setenv(mcpTestTokenEnv, "") // 空值 = 缺失（与 resolveMCPEnv 同判据）

	before := env.readMCPConfig()
	beforePointer := env.mcpPointer(t, "demo")
	versionID := beforePointer.PublishedVersionID

	_, err := env.admin.Publish(context.Background(), resourceActor(), core.ResourcePublishRequest{
		Kind:      core.ResourceMCPServer,
		ID:        "demo",
		VersionID: &versionID,
	})
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, core.ErrProviderNotConfigured, adminErr.Kind, "发布缺 env 必须 503")

	operationID, ok := adminErr.Data["operation_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, operationID)

	afterPointer := env.mcpPointer(t, "demo")
	require.Equal(t, before, env.readMCPConfig(), "发布失败不得改写定义文件")
	require.Equal(t, beforePointer.PublishedVersionID, afterPointer.PublishedVersionID, "发布失败不得推进 published")
	require.Equal(t, beforePointer.AppliedVersionID, afterPointer.AppliedVersionID, "发布失败不得改变 applied")
	require.Len(t, env.versionsOfKind(core.ResourceMCPServer, "demo"), 1, "发布失败不得追加版本")

	requireEnvGateOperation(t, env, operationID)
}

// TestMCPEnvGate_RollbackMissingEnvNoWrite 覆盖 §4.5.1 / §4.5.2 的回滚入口：v2 已发布
// 且 v2 的变量就绪，回滚目标 v1 的变量缺失 → 仍必须 503。闸门校验的是**目标版本**的
// 条目，不是当前文件 / 当前发布版本的环境。
// 逆改点：回滚走"当前文件"校验、或按 published 版本校验 → 本用例变绿（错），必须红。
// TestMCPEnvGate_RollbackMissingEnvNoWrite proves the gate validates the target version.
func TestMCPEnvGate_RollbackMissingEnvNoWrite(t *testing.T) {
	env := newResourceEnv(t)
	v1 := env.createMCPStdio(t, "demo", "allowed-mcp") // v1 引用 MCP_TEST_TOKEN
	t.Setenv(mcpSecondTokenEnv, "second-token-present")

	v2, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      "demo",
		Transport: "stdio",
		Command:   "other-mcp",
		Env:       map[string]string{mcpSecondTokenEnv: "${" + mcpSecondTokenEnv + "}"},
		IfMatch:   env.currentETagOf(t, core.ResourceMCPServer, "demo"),
	})
	require.NoError(t, err)
	require.Equal(t, 2, v2.VersionNo)

	before := env.readMCPConfig()
	beforePointer := env.mcpPointer(t, "demo")
	t.Setenv(mcpTestTokenEnv, "") // 只有回滚目标 v1 的变量缺失

	rollbackID := v1.ID
	_, err = env.admin.Publish(context.Background(), resourceActor(), core.ResourcePublishRequest{
		Kind:      core.ResourceMCPServer,
		ID:        "demo",
		VersionID: &rollbackID,
	})
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, core.ErrProviderNotConfigured, adminErr.Kind, "回滚目标的 env 缺失必须 503")

	operationID, ok := adminErr.Data["operation_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, operationID)

	require.Equal(t, before, env.readMCPConfig(), "回滚失败不得把文件改写成目标版本字节")
	require.Equal(t, beforePointer, env.mcpPointer(t, "demo"), "回滚失败不得改指针")
	require.Len(t, env.versionsOfKind(core.ResourceMCPServer, "demo"), 2, "回滚失败不得追加版本")

	requireEnvGateOperation(t, env, operationID)
}

// TestMCPEnvGate_IdempotentPublishSkips 覆盖 §4.5.3：幂等发布（无 VersionID 且最新即
// published）在闸门**之前**短路——不重新校验、不改指针、不新增版本；且成功不等于
// "环境已满足"：同一版本显式重发仍必须被闸门拒绝，幂等操作行也不写任何 env 状态字段。
// 逆改点：把闸门放在幂等分支之前（幂等发布被 503）、幂等分支里写指针、detail 里写
// "env_satisfied" 之类字段 → 对应断言红。
// TestMCPEnvGate_IdempotentPublishSkips covers §4.5.3 (idempotent short-circuit).
func TestMCPEnvGate_IdempotentPublishSkips(t *testing.T) {
	env := newResourceEnv(t)
	env.createMCPStdio(t, "demo", "allowed-mcp")
	before := env.readMCPConfig()
	beforePointer := env.mcpPointer(t, "demo")
	t.Setenv(mcpTestTokenEnv, "") // 空值 = 缺失；幂等分支不得重新校验

	result, err := env.admin.Publish(context.Background(), resourceActor(), core.ResourcePublishRequest{
		Kind: core.ResourceMCPServer,
		ID:   "demo",
	})
	require.NoError(t, err, "幂等发布不得重新校验 env")
	require.Equal(t, beforePointer.PublishedVersionID, result.Version.ID)

	require.Equal(t, beforePointer, env.mcpPointer(t, "demo"), "幂等发布不得改指针")
	require.Len(t, env.versionsOfKind(core.ResourceMCPServer, "demo"), 1, "幂等发布不得新增版本")
	require.Equal(t, before, env.readMCPConfig(), "幂等发布不得改写定义文件")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerPublish})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	var detail map[string]any
	require.NoError(t, json.Unmarshal(rows[0].DetailJSON, &detail))
	require.Empty(t, detail, "幂等成功不得写入任何 env 状态字段（发布成功 ≠ 环境满足）")

	// 幂等成功没有让环境"通过"：同一版本显式重发必须被闸门拒绝。
	versionID := beforePointer.PublishedVersionID
	_, err = env.admin.Publish(context.Background(), resourceActor(), core.ResourcePublishRequest{
		Kind:      core.ResourceMCPServer,
		ID:        "demo",
		VersionID: &versionID,
	})
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, core.ErrProviderNotConfigured, adminErr.Kind, "幂等成功不得被当成环境已验证")
}

// TestMCPEnvGate_ErrorOmitsValues 覆盖脱敏判据：条目里一个变量已就绪（值为哨兵）、
// 另一个缺失——错误、操作详情与日志只点名 server / 键 / 变量名，**不得**出现已就绪
// 变量的值，也不得回显占位表达式。
// 逆改点：把解析后的 env 值写进错误或 detail、把整个 Env map 原样序列化 → 红。
// TestMCPEnvGate_ErrorOmitsValues asserts neither the message nor the detail leaks a value.
func TestMCPEnvGate_ErrorOmitsValues(t *testing.T) {
	env := newResourceEnv(t)
	t.Setenv(mcpTestTokenEnv, mcpEnvSecretSentinel)
	t.Setenv(mcpAbsentTokenEnv, "")

	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      "demo",
		Transport: "stdio",
		Command:   "mcp-helper",
		Env: map[string]string{
			mcpTestTokenEnv:   "${" + mcpTestTokenEnv + "}",
			mcpAbsentTokenEnv: "${" + mcpAbsentTokenEnv + "}",
		},
		Create: true,
	})
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, core.ErrProviderNotConfigured, adminErr.Kind)
	require.Contains(t, err.Error(), mcpAbsentTokenEnv, "错误只点名变量")
	require.NotContains(t, err.Error(), mcpEnvSecretSentinel, "错误不得回显 env 值")

	operationID, _ := adminErr.Data["operation_id"].(string)
	require.NotEmpty(t, operationID)
	detail := requireEnvGateOperation(t, env, operationID)
	require.NotContains(t, detail.ErrorMessage, mcpEnvSecretSentinel, "detail 不得回显 env 值")

	row, err := env.ops.GetOperation(context.Background(), operationID)
	require.NoError(t, err)
	require.NotContains(t, string(row.DetailJSON), mcpEnvSecretSentinel, "detail_json 不得含 env 值")
	require.NotContains(t, string(row.DetailJSON), "${", "detail_json 不得回显占位表达式")
	require.NotContains(t, env.logs.String(), mcpEnvSecretSentinel, "日志不得含 env 值")
}
