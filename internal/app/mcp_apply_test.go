package app

// 本文件是受控重建（`mcp_server.apply`）的验收用例 C1–C8（颗粒度文档 §4 冻结的八个
// `TestMCPApply_*` 名字；契约 `/mcp-servers/{name}/apply`；设计 §2.3.3 的八步算法）。
//
// 判别力来自四处：
//   - 夹具用**真仓储**（SQLite + 资源指针 + 凭据版本）与**假连接器**，两者之间只有设计
//     允许的接缝（`NewConnector` / `ReplaceTools` / `MarkApplied`）；
//   - 候选连接在建连时刻执行注入钩子——并发写（publish / 另一次 apply / revoke）恰好发生在
//     "快照之后、提交段之前"，这正是 C6 要判的窗口；
//   - 计数工具记录 `Execute` 次数：门的拒绝必须发生在**实际执行之前**（inner 计数为 0）；
//   - 每条用例同时检查指针、注册池、连接器生命周期与操作行 detail——只看返回码会把
//     "指针没换但工具已换"判成通过。
// The C1–C8 acceptance cases for a controlled apply: real stores, a fake connector, and
// assertions that span pointers, the registry, connector lifecycles and the audit detail.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// mcpApplyCountTool 是计数工具：`Execute` 次数与可选阻塞通道让用例判断"调用是否真的
// 发出过"（C4 / C8 的拒绝必须发生在执行之前）和"在途调用是否被排空窗口覆盖"（C3）。
// mcpApplyCountTool counts executions and can hold one call open for the drain window.
type mcpApplyCountTool struct {
	name string

	mu    sync.Mutex
	calls int
	hold  chan struct{}
}

// Name 返回工具名（连接结果里的名字就是它）。
func (t *mcpApplyCountTool) Name() string { return t.name }

// Description 返回固定描述：本文件不判描述措辞。
func (t *mcpApplyCountTool) Description() string { return "apply 夹具工具：" + t.name }

// InputSchema 返回最小对象 schema。
func (t *mcpApplyCountTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

// Execute 计数后返回固定结果；设置了 hold 时等它关闭（排空用例要的就是"在途"）。
func (t *mcpApplyCountTool) Execute(ctx context.Context, _ core.ToolEnv, _ json.RawMessage,
) (core.Result, error) {
	t.mu.Lock()
	t.calls++
	hold := t.hold
	t.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return core.Result{}, ctx.Err()
		}
	}
	return core.Result{Content: t.name + " ok"}, nil
}

// callCount 读取已执行的次数。
func (t *mcpApplyCountTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// holdUntil 让后续调用阻塞到 hold 关闭（在发起调用之前设置）。
func (t *mcpApplyCountTool) holdUntil(hold chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hold = hold
}

// mcpApplyConnector 是受控重建夹具的连接器：每次建连按 FIFO 取一组工具名，并在建连时刻
// 执行注入钩子（并发写就发生在这一刻）。`Connect` 委托 `ConnectDetailed`，使 Start 与
// Replace 走同一个注入点。
// mcpApplyConnector hands out FIFO tool sets and runs the injected hook at connect time.
type mcpApplyConnector struct {
	env *mcpApplyEnv

	mu     sync.Mutex
	tools  map[string]*mcpApplyCountTool
	closed bool
}

// Connect 满足 `mcpConnector`：启动路径只关心工具，丢弃结构化失败清单。
func (c *mcpApplyConnector) Connect(ctx context.Context, specs []core.MCPServerConfig,
) ([]tool.MCPConnection, error) {
	conns, _, err := c.ConnectDetailed(ctx, specs)
	return conns, err
}

// ConnectDetailed 按注入表产出连接结果或一条结构化失败。
func (c *mcpApplyConnector) ConnectDetailed(ctx context.Context, specs []core.MCPServerConfig,
) ([]tool.MCPConnection, []tool.MCPConnectFailure, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	server := ""
	if len(specs) > 0 {
		server = specs[0].Name
	}
	index, names, hook, failure, failed := c.env.nextConnect()
	if hook != nil {
		hook(index)
	}
	if failed {
		return nil, []tool.MCPConnectFailure{{
			Server: server, Stage: failure.Stage, Class: failure.Class, Err: failure.Err,
		}}, nil
	}
	c.mu.Lock()
	c.tools = make(map[string]*mcpApplyCountTool, len(names))
	tools := make([]core.Tool, 0, len(names))
	for _, name := range names {
		counted := &mcpApplyCountTool{name: name}
		c.tools[name] = counted
		tools = append(tools, counted)
	}
	c.mu.Unlock()
	return []tool.MCPConnection{{Server: server, Tools: tools}}, nil, nil
}

// Close 记录一次关闭（用例据 closed 与总次数断言"候选被关闭、旧连接仍活着"）。
func (c *mcpApplyConnector) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.env.countClose()
	return nil
}

// isClosed 报告该连接器是否已被关闭。
func (c *mcpApplyConnector) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// innerTool 取本次连接产出的计数工具（未过门的那一层：门的拒绝不会让它计数）。
func (c *mcpApplyConnector) innerTool(name string) *mcpApplyCountTool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tools[name]
}

// mcpApplyEnv 在凭据夹具之上装配受控重建：真仓储 + 假连接器 + 真注册池 / 来源表。
// mcpApplyEnv wires the controlled apply on top of the credential fixture.
type mcpApplyEnv struct {
	*mcpCredentialEnv
	supervisor   *MCPSupervisor
	registry     *tool.Registry
	dispositions *mcpDispositionTable

	mu         sync.Mutex
	toolSets   [][]string
	connectors []*mcpApplyConnector
	failures   map[int]tool.MCPConnectFailure
	onConnect  func(n int)
	connects   int
	closes     int
}

// newMCPApplyEnv 装配夹具；mutate 在资源写面构造前生效（C5 用它换依赖探针）。
// newMCPApplyEnv wires the fixture; mutate runs before the resource admin is built.
func newMCPApplyEnv(t *testing.T, mutate ...func(*ResourceAdminDeps)) *mcpApplyEnv {
	t.Helper()
	env := &mcpApplyEnv{
		mcpCredentialEnv: newMCPCredentialEnv(t, mutate...),
		registry:         tool.NewRegistry(slog.New(slog.DiscardHandler)),
		dispositions:     newMCPDispositionTable(),
		failures:         make(map[int]tool.MCPConnectFailure),
	}
	env.supervisor = NewMCPSupervisor(MCPSupervisorDeps{
		Policy: tool.NewWhitelistPolicy(tool.PolicyOptions{
			Root:            t.TempDir(),
			AllowedCommands: []string{"allowed-mcp"},
			AllowedDomains:  []string{"mcp.example.com"},
		}),
		Gate:   env.gate,
		Logger: slog.New(slog.DiscardHandler),
		Now:    baselineClock,
		MarkApplied: func(ctx context.Context, name string, versionID int64) error {
			return markMCPApplied(ctx, env.store, name, versionID, baselineClock)
		},
		OnConnected: func(conns []tool.MCPConnection) {
			mcpRegisterTools(env.registry, env.dispositions, conns, env.logger)
		},
		ReplaceTools: func(server string, tools []core.Tool) {
			mcpReplaceTools(env.registry, env.dispositions, server, tools)
		},
		NewConnector: env.newConnector,
		TestTimeout:  5 * time.Second,
	})
	// 与 wire 的装配顺序一致：admin 在 supervisor 之后才拿到运行时句柄。
	env.admin.supervisor = env.supervisor
	return env
}

// newConnector 为每次连接登记一个假连接器。
func (e *mcpApplyEnv) newConnector() mcpConnector {
	e.mu.Lock()
	defer e.mu.Unlock()
	connector := &mcpApplyConnector{env: e, tools: make(map[string]*mcpApplyCountTool)}
	e.connectors = append(e.connectors, connector)
	return connector
}

// nextConnect 取第 n 次建连的工具集、钩子与失败注入；钩子在建连线程上执行。
func (e *mcpApplyEnv) nextConnect() (int, []string, func(int), tool.MCPConnectFailure, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	index := e.connects
	e.connects++
	names := []string{"apply_default_tool"}
	if index < len(e.toolSets) {
		names = e.toolSets[index]
	}
	failure, failed := e.failures[index]
	return index, names, e.onConnect, failure, failed
}

// countClose 记一次连接器关闭。
func (e *mcpApplyEnv) countClose() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closes++
}

// pushTools 为下一次（按 FIFO）建连预置工具名。
func (e *mcpApplyEnv) pushTools(names ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolSets = append(e.toolSets, names)
}

// failConnectAt 让第 index 次建连返回一条结构化失败（stage × class）。
func (e *mcpApplyEnv) failConnectAt(index int, stage, class string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures[index] = tool.MCPConnectFailure{
		Stage: stage, Class: class, Err: errors.New("apply 夹具注入的连接失败"),
	}
}

// setOnConnect 注册建连钩子（并发写注入点；n 为建连序号）。
func (e *mcpApplyEnv) setOnConnect(hook func(n int)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onConnect = hook
}

// connector 取第 index 个连接器。
func (e *mcpApplyEnv) connector(index int) *mcpApplyConnector {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Less(e.t, index, len(e.connectors), "第 %d 个连接器尚未创建", index)
	return e.connectors[index]
}

// connectTotal 读取建连总次数（预检拒绝时它必须不增长）。
func (e *mcpApplyEnv) connectTotal() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.connects
}

// configOfVersion 解析指定版本的已发布条目（Start / Replace 的输入）。
func (e *mcpApplyEnv) configOfVersion(t *testing.T, versionID int64) core.MCPServerConfig {
	t.Helper()
	version, err := e.store.VersionByID(context.Background(), versionID)
	require.NoError(t, err)
	cfg, err := workspace.ParseMCPServerBlock([]byte(version.DefinitionYAML))
	require.NoError(t, err)
	return cfg
}

// startVersion 让一个已发布版本生效（初次装配 / 重启路径）。
func (e *mcpApplyEnv) startVersion(t *testing.T, name string, versionID int64) {
	t.Helper()
	require.NoError(t, e.supervisor.Start(context.Background(), core.PublishedMCPServer{
		VersionID: versionID, Config: e.configOfVersion(t, versionID),
	}))
}

// publishMCPArgs 用不同的参数发布新版本（内容不同 = 新定义版本），返回新版本行。
func (e *mcpApplyEnv) publishMCPArgs(t *testing.T, name string, args []string,
	envValues map[string]string, credentials []MCPCredentialRequest,
) core.ResourceVersion {
	t.Helper()
	version, err := e.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:        name,
		Transport:   "stdio",
		Command:     "allowed-mcp",
		Args:        args,
		Env:         envValues,
		Credentials: credentials,
		IfMatch:     e.currentETagOf(t, core.ResourceMCPServer, name),
	})
	require.NoError(t, err)
	return version
}

// createManagedMCP 建一个托管凭据槽位的 MCP server（占位符同时也是 env 键）。
func (e *mcpApplyEnv) createManagedMCP(t *testing.T, name string) core.ResourceVersion {
	t.Helper()
	version, err := e.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:        name,
		Transport:   "stdio",
		Command:     "allowed-mcp",
		Args:        []string{"--root", "./workspace"},
		Env:         map[string]string{"MCP_ALPHA_TOKEN": "${MCP_ALPHA_TOKEN}"},
		Credentials: []MCPCredentialRequest{mcpManagedValue("MCP_ALPHA_TOKEN", mcpCredAlphaOne)},
		Create:      true,
	})
	require.NoError(t, err)
	return version
}

// applyInput 组装连接层的受控重建输入；目标配置取目标版本的真实发布字节。
func (e *mcpApplyEnv) applyInput(t *testing.T, name string, targetID, previousID int64,
	window time.Duration, commit func(context.Context) (core.MCPApplySupersededBy, error),
	legal func(context.Context) bool,
) MCPApplyInput {
	t.Helper()
	return MCPApplyInput{
		Target:            core.PublishedMCPServer{VersionID: targetID, Config: e.configOfVersion(t, targetID)},
		PreviousVersionID: previousID,
		DrainTimeout:      window,
		Commit:            commit,
		OldLegal:          legal,
	}
}

// operationRow 按标识读操作行。
func (e *mcpApplyEnv) operationRow(t *testing.T, operationID string) core.OperationRecord {
	t.Helper()
	row, err := e.ops.GetOperation(context.Background(), operationID)
	require.NoError(t, err)
	return row
}

// mcpApplyDetailOf 解析操作行 detail 并钉住固定 7 键集（audit 契约）。
func mcpApplyDetailOf(t *testing.T, row core.OperationRecord) map[string]any {
	t.Helper()
	var detail map[string]any
	require.NoError(t, json.Unmarshal(row.DetailJSON, &detail))
	require.Len(t, detail, 7, "detail_json 固定 7 键（audit 契约）")
	return detail
}

// requireExternalRefusal 断言一次调用被门拒绝：错误是自产短语的 `*core.ExternalError`。
func requireExternalRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	require.Error(t, err)
	var external *core.ExternalError
	require.ErrorAs(t, err, &external)
	require.Equal(t, reason, external.Reason)
}

// TestMCPApply_OnlyTargetReplaced 覆盖 C1：只有目标 server 的一代连接被替换——指针、
// 注册池、来源表与连接器生命周期全部指向新版本，另一个 server 的实例逐一不动。
func TestMCPApply_OnlyTargetReplaced(t *testing.T) {
	t.Parallel()
	env := newMCPApplyEnv(t)
	ctx := context.Background()

	alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
	betaV1 := env.createMCPStdio(t, "beta", "allowed-mcp")
	env.pushTools("alpha_v1_read")
	env.pushTools("beta_read")
	env.startVersion(t, "alpha", alphaV1.ID)
	env.startVersion(t, "beta", betaV1.ID)

	oldAlphaTool, ok := env.registry.Resolve("alpha_v1_read")
	require.True(t, ok)
	betaTool, ok := env.registry.Resolve("beta_read")
	require.True(t, ok)
	oldAlphaConn := env.connector(0)
	betaConn := env.connector(1)

	alphaV2 := env.publishMCPArgs(t, "alpha",
		[]string{"--root", "./workspace", "--v2"},
		map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"},
		envOnlyCredentials(map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"}))
	require.Equal(t, 2, alphaV2.VersionNo)
	require.Equal(t, alphaV1.ID, derefVersionID(env.pointerOf(core.ResourceMCPServer, "alpha").AppliedVersionID),
		"发布只推进 published，applied 保持旧值")
	env.pushTools("alpha_v2_read")

	result, err := env.admin.ApplyMCPServer(ctx, resourceActor(), "alpha",
		env.currentETagOf(t, core.ResourceMCPServer, "alpha"), 0)
	require.NoError(t, err)
	require.Equal(t, core.MCPApplyApplied, result.Outcome)
	require.Equal(t, alphaV2.ID, result.TargetVersionID)
	require.Equal(t, alphaV1.ID, result.PreviousVersionID)
	require.Equal(t, alphaV2.ID, result.AppliedVersionID)
	require.Empty(t, result.FailureClass)
	require.Equal(t, core.MCPApplySupersededByNone, result.SupersededBy)
	require.Equal(t, 30, result.DrainTimeoutSeconds)
	require.Equal(t, baselineClock().Add(defaultMCPDrainTimeout).UTC().Format(time.RFC3339),
		result.DrainDeadline)
	require.NotEmpty(t, result.OperationID)

	pointer := env.pointerOf(core.ResourceMCPServer, "alpha")
	require.Equal(t, alphaV2.ID, pointer.PublishedVersionID)
	require.Equal(t, alphaV2.ID, derefVersionID(pointer.AppliedVersionID))

	_, stale := env.registry.Resolve("alpha_v1_read")
	require.False(t, stale, "旧版本的工具名必须随整组替换撤下")
	fresh, ok := env.registry.Resolve("alpha_v2_read")
	require.True(t, ok)
	require.NotSame(t, oldAlphaTool, fresh, "新 Run 必须拿到新一代实例")
	betaNow, ok := env.registry.Resolve("beta_read")
	require.True(t, ok)
	require.Same(t, betaTool, betaNow, "另一个 server 的实例不得被替换")

	status, ok := env.supervisor.Status("alpha")
	require.True(t, ok)
	require.True(t, status.Connected)
	require.Equal(t, []string{"alpha_v2_read"}, status.Tools)
	require.Equal(t, alphaV2.ID, status.AppliedVersionID)
	betaStatus, ok := env.supervisor.Status("beta")
	require.True(t, ok)
	require.True(t, betaStatus.Connected, "另一个 server 的连接不得被触碰")

	require.True(t, oldAlphaConn.isClosed(), "旧代连接在排空后关闭")
	require.False(t, betaConn.isClosed())

	disposition := env.dispositions.view("alpha")
	require.NotNil(t, disposition)
	require.Equal(t, 1, disposition.Offered)
	require.Equal(t, 1, disposition.Accepted)
	require.Empty(t, disposition.Skipped)
	require.Equal(t, "alpha", env.dispositions.source("alpha_v2_read"))
	require.Empty(t, env.dispositions.source("alpha_v1_read"))
	require.Equal(t, "beta", env.dispositions.source("beta_read"))

	row := env.operationRow(t, result.OperationID)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, lifecycleResultOK, row.ResultCode)
	require.Equal(t, core.ActionMCPServerApply, row.Action)
	require.Equal(t, string(pointer.ResourceUID), row.TargetID)
	detail := mcpApplyDetailOf(t, row)
	require.Equal(t, string(core.MCPApplyApplied), detail["outcome"])
	require.Equal(t, float64(alphaV2.ID), detail["target_version_id"])
	require.Equal(t, float64(alphaV1.ID), detail["previous_version_id"])
	require.Equal(t, float64(alphaV2.ID), detail["applied_version_id"])
	require.Equal(t, "", detail["failure_class"])
	require.Equal(t, "", detail["superseded_by"])
	require.Equal(t, result.DrainDeadline, detail["drain_deadline"])
}

// TestMCPApply_FailureKeepsApplied 覆盖 C2：建连失败时 `applied` 不变、旧连接保留；
// 若冻结的旧版本已不合法（凭据已撤销），旧连接一并关闭并标固定短语——绝不把撤销
// "顺延"给旧连接。
func TestMCPApply_FailureKeepsApplied(t *testing.T) {
	t.Parallel()

	t.Run("建连失败保留旧连接与应用版本", func(t *testing.T) {
		t.Parallel()
		env := newMCPApplyEnv(t)
		ctx := context.Background()

		alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
		env.pushTools("alpha_v1_read")
		env.startVersion(t, "alpha", alphaV1.ID)
		oldConn := env.connector(0)
		oldTool, ok := env.registry.Resolve("alpha_v1_read")
		require.True(t, ok)

		alphaV2 := env.publishMCPArgs(t, "alpha",
			[]string{"--root", "./workspace", "--v2"},
			map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"},
			envOnlyCredentials(map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"}))
		env.pushTools("alpha_v2_read")
		env.failConnectAt(1, "initialize", "handshake_failed")

		result, err := env.admin.ApplyMCPServer(ctx, resourceActor(), "alpha",
			env.currentETagOf(t, core.ResourceMCPServer, "alpha"), 0)
		require.Error(t, err)
		require.ErrorIs(t, err, core.ErrMCPApplyFailed)
		require.Equal(t, core.MCPApplyFailed, result.Outcome)
		require.Equal(t, "handshake_failed", result.FailureClass)
		require.Equal(t, alphaV1.ID, result.AppliedVersionID)
		require.Empty(t, result.DrainDeadline, "失败没有排空窗口")

		pointer := env.pointerOf(core.ResourceMCPServer, "alpha")
		require.Equal(t, alphaV2.ID, pointer.PublishedVersionID, "发布指针不动")
		require.Equal(t, alphaV1.ID, derefVersionID(pointer.AppliedVersionID), "应用版本不变")

		require.False(t, oldConn.isClosed(), "旧连接仍合法——失败不得中断服务")
		require.True(t, env.connector(1).isClosed(), "候选连接必须关闭")
		now, ok := env.registry.Resolve("alpha_v1_read")
		require.True(t, ok)
		require.Same(t, oldTool, now, "注册池保持旧一代")
		_, leaked := env.registry.Resolve("alpha_v2_read")
		require.False(t, leaked)

		status, ok := env.supervisor.Status("alpha")
		require.True(t, ok)
		require.True(t, status.Connected)
		require.Equal(t, alphaV1.ID, status.AppliedVersionID)

		row := env.operationRow(t, result.OperationID)
		require.Equal(t, core.OperationFailed, row.Status)
		require.Equal(t, mcpApplyResultFailed, row.ResultCode)
		detail := mcpApplyDetailOf(t, row)
		require.Equal(t, string(core.MCPApplyFailed), detail["outcome"])
		require.Equal(t, "handshake_failed", detail["failure_class"])
		require.Equal(t, float64(alphaV1.ID), detail["applied_version_id"])
	})

	t.Run("旧版本已不合法时关闭旧连接", func(t *testing.T) {
		t.Parallel()
		env := newMCPApplyEnv(t)
		ctx := context.Background()

		alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
		env.pushTools("alpha_v1_read")
		env.startVersion(t, "alpha", alphaV1.ID)
		oldConn := env.connector(0)
		oldTool, ok := env.registry.Resolve("alpha_v1_read")
		require.True(t, ok)

		alphaV2 := env.publishMCPArgs(t, "alpha",
			[]string{"--root", "./workspace", "--v2"},
			map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"},
			envOnlyCredentials(map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"}))
		env.pushTools("alpha_v2_read")
		env.failConnectAt(1, mcpTestStageSpawn, "command_not_found")

		result := env.supervisor.Replace(ctx, env.applyInput(t, "alpha", alphaV2.ID, alphaV1.ID,
			time.Second,
			func(context.Context) (core.MCPApplySupersededBy, error) {
				return core.MCPApplySupersededByNone, nil
			},
			func(context.Context) bool { return false }))
		require.Equal(t, core.MCPApplyFailed, result.Outcome)
		require.Equal(t, "command_not_found", result.FailureClass)
		require.Equal(t, alphaV1.ID, result.AppliedVersionID)

		require.True(t, oldConn.isClosed(), "旧版本已不合法：连接必须关闭")
		status, ok := env.supervisor.Status("alpha")
		require.True(t, ok)
		require.False(t, status.Connected)
		require.Empty(t, status.Tools)
		require.Equal(t, mcpLinkRevokedReason, status.Error)
		require.Equal(t, alphaV1.ID, status.AppliedVersionID, "版本事实不变")

		_, err := oldTool.Execute(ctx, core.ToolEnv{}, nil)
		requireExternalRefusal(t, err, mcpLinkRevokedReason)
	})
}

// TestMCPApply_DrainWindow 覆盖 C3：排空窗口覆盖已发出的调用——窗口内自然完成不记失败；
// 窗口到点仍未归零则强制关闭并显式记 `drain_timeout`，之后的新调用立刻被拒绝。
func TestMCPApply_DrainWindow(t *testing.T) {
	t.Parallel()

	t.Run("在途调用在窗口内完成", func(t *testing.T) {
		t.Parallel()
		env := newMCPApplyEnv(t)
		ctx := context.Background()

		alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
		env.pushTools("alpha_v1_read")
		env.startVersion(t, "alpha", alphaV1.ID)
		oldTool, ok := env.registry.Resolve("alpha_v1_read")
		require.True(t, ok)
		inner := env.connector(0).innerTool("alpha_v1_read")
		require.NotNil(t, inner)

		alphaV2 := env.publishMCPArgs(t, "alpha",
			[]string{"--root", "./workspace", "--v2"},
			map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"},
			envOnlyCredentials(map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"}))
		env.pushTools("alpha_v2_read")

		hold := make(chan struct{})
		inner.holdUntil(hold)
		type execution struct {
			result core.Result
			err    error
		}
		done := make(chan execution, 1)
		go func() {
			result, err := oldTool.Execute(ctx, core.ToolEnv{}, nil)
			done <- execution{result: result, err: err}
		}()
		require.Eventually(t, func() bool { return inner.callCount() == 1 },
			2*time.Second, 5*time.Millisecond, "调用必须先真正发出")
		go func() {
			time.Sleep(100 * time.Millisecond)
			close(hold)
		}()

		result := env.supervisor.Replace(ctx, env.applyInput(t, "alpha", alphaV2.ID, alphaV1.ID,
			2*time.Second,
			func(context.Context) (core.MCPApplySupersededBy, error) {
				return core.MCPApplySupersededByNone, nil
			}, nil))
		require.Equal(t, core.MCPApplyApplied, result.Outcome)
		require.Empty(t, result.FailureClass, "窗口内自然归零不记失败分类")
		require.Equal(t, alphaV2.ID, result.AppliedVersionID)

		got := <-done
		require.NoError(t, got.err)
		require.Contains(t, got.result.Content, "alpha_v1_read")
		require.True(t, env.connector(0).isClosed(), "排空完成后旧连接关闭")
	})

	t.Run("窗口到点强制关闭并记 drain_timeout", func(t *testing.T) {
		t.Parallel()
		env := newMCPApplyEnv(t)
		ctx := context.Background()

		alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
		env.pushTools("alpha_v1_read")
		env.startVersion(t, "alpha", alphaV1.ID)
		oldTool, ok := env.registry.Resolve("alpha_v1_read")
		require.True(t, ok)
		inner := env.connector(0).innerTool("alpha_v1_read")
		require.NotNil(t, inner)

		alphaV2 := env.publishMCPArgs(t, "alpha",
			[]string{"--root", "./workspace", "--v2"},
			map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"},
			envOnlyCredentials(map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"}))
		env.pushTools("alpha_v2_read")

		hold := make(chan struct{})
		inner.holdUntil(hold)
		type execution struct {
			result core.Result
			err    error
		}
		done := make(chan execution, 1)
		go func() {
			result, err := oldTool.Execute(ctx, core.ToolEnv{}, nil)
			done <- execution{result: result, err: err}
		}()
		require.Eventually(t, func() bool { return inner.callCount() == 1 },
			2*time.Second, 5*time.Millisecond)

		window := 120 * time.Millisecond
		result := env.supervisor.Replace(ctx, env.applyInput(t, "alpha", alphaV2.ID, alphaV1.ID,
			window,
			func(context.Context) (core.MCPApplySupersededBy, error) {
				return core.MCPApplySupersededByNone, nil
			}, nil))
		require.Equal(t, core.MCPApplyApplied, result.Outcome, "切换已提交，失败分类只描述排空")
		require.Equal(t, core.MCPApplyFailureDrainTimeout, result.FailureClass)
		require.Equal(t, baselineClock().Add(window), result.DrainDeadline)
		require.True(t, env.connector(0).isClosed(), "到点必须强制关闭")

		_, err := oldTool.Execute(ctx, core.ToolEnv{}, nil)
		requireExternalRefusal(t, err, mcpLinkClosedReason)

		close(hold)
		got := <-done
		require.NoError(t, got.err, "已发出的调用自然返回，不被取消")
		require.Equal(t, 1, inner.callCount(), "被拒绝的新调用不得触达工具本体")
	})
}

// TestMCPApply_OldRunSnapshotUnchanged 覆盖 C4：准入时钉住的工具实例在受控重建后仍是
// 同一对象、仍连着旧一代；注册池里的同名条目已换成新实例。旧 Run 执行被明确拒绝，
// 工具本体一次都没有被调用。
func TestMCPApply_OldRunSnapshotUnchanged(t *testing.T) {
	t.Parallel()
	env := newMCPApplyEnv(t)
	ctx := context.Background()

	alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
	env.pushTools("alpha_v1_read")
	env.startVersion(t, "alpha", alphaV1.ID)

	pinned, specs := env.registry.Pin([]string{"alpha_v1_read"})
	require.Len(t, specs, 1)
	require.Equal(t, "alpha_v1_read", specs[0].Name)
	pinnedTool, ok := pinned["alpha_v1_read"]
	require.True(t, ok)
	inner := env.connector(0).innerTool("alpha_v1_read")
	require.NotNil(t, inner)

	alphaV2 := env.publishMCPArgs(t, "alpha",
		[]string{"--root", "./workspace", "--v2"},
		map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"},
		envOnlyCredentials(map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"}))
	env.pushTools("alpha_v2_read")

	result, err := env.admin.ApplyMCPServer(ctx, resourceActor(), "alpha",
		env.currentETagOf(t, core.ResourceMCPServer, "alpha"), 0)
	require.NoError(t, err)
	require.Equal(t, core.MCPApplyApplied, result.Outcome)
	require.Equal(t, alphaV2.ID, result.TargetVersionID)

	_, present := env.registry.Resolve("alpha_v1_read")
	require.False(t, present, "注册池里的旧名字已撤下")
	fresh, ok := env.registry.Resolve("alpha_v2_read")
	require.True(t, ok)
	require.NotSame(t, pinnedTool, fresh)

	_, err = pinnedTool.Execute(ctx, core.ToolEnv{}, nil)
	requireExternalRefusal(t, err, mcpLinkClosedReason)
	require.Equal(t, 0, inner.callCount(), "旧 Run 的快照不得被悄悄转到新连接，也不得触达旧工具本体")
}

// TestMCPApply_PreflightRefuses 覆盖 C5：就绪预检在**任何进程启动之前**拒绝——依赖未就绪
// 与托管凭据不可用走两个不同的业务码与错误分类，且都不建候选连接。
func TestMCPApply_PreflightRefuses(t *testing.T) {
	t.Parallel()

	t.Run("依赖未就绪", func(t *testing.T) {
		t.Parallel()
		env := newMCPApplyEnv(t, func(deps *ResourceAdminDeps) {
			deps.MCPDependencies = func(core.MCPServerConfig) *core.MCPDependencyDiagnosis {
				return &core.MCPDependencyDiagnosis{
					Component:       "dbhub",
					State:           core.MCPDependencyMissing,
					ExpectedVersion: "1.5.0",
				}
			}
		})
		ctx := context.Background()

		alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
		env.pushTools("alpha_v1_read")
		env.startVersion(t, "alpha", alphaV1.ID)
		alphaV2 := env.publishMCPArgs(t, "alpha",
			[]string{"--root", "./workspace", "--v2"},
			map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"},
			envOnlyCredentials(map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"}))
		connectsBefore := env.connectTotal()

		result, err := env.admin.ApplyMCPServer(ctx, resourceActor(), "alpha",
			env.currentETagOf(t, core.ResourceMCPServer, "alpha"), 0)
		require.Error(t, err)
		require.ErrorIs(t, err, core.ErrProviderNotConfigured)
		require.Equal(t, core.MCPApplyFailed, result.Outcome)
		require.Equal(t, mcpTestClassDependencyUnconfirmed, result.FailureClass)
		require.Equal(t, alphaV1.ID, result.AppliedVersionID)
		require.Equal(t, connectsBefore, env.connectTotal(), "预检拒绝不得启动任何连接")

		pointer := env.pointerOf(core.ResourceMCPServer, "alpha")
		require.Equal(t, alphaV2.ID, pointer.PublishedVersionID)
		require.Equal(t, alphaV1.ID, derefVersionID(pointer.AppliedVersionID))

		row := env.operationRow(t, result.OperationID)
		require.Equal(t, core.OperationFailed, row.Status)
		require.Equal(t, resourceResultNotConfigured, row.ResultCode)
		detail := mcpApplyDetailOf(t, row)
		require.Equal(t, mcpTestClassDependencyUnconfirmed, detail["failure_class"])
		require.Equal(t, float64(alphaV1.ID), detail["applied_version_id"])
	})

	t.Run("托管凭据已撤销", func(t *testing.T) {
		t.Parallel()
		env := newMCPApplyEnv(t)
		ctx := context.Background()

		alphaV1 := env.createManagedMCP(t, "alpha")
		env.pushTools("alpha_v1_read")
		env.startVersion(t, "alpha", alphaV1.ID)
		env.revoke(t, "alpha", "MCP_ALPHA_TOKEN")
		connectsBefore := env.connectTotal()

		result, err := env.admin.ApplyMCPServer(ctx, resourceActor(), "alpha",
			env.currentETagOf(t, core.ResourceMCPServer, "alpha"), 0)
		require.Error(t, err)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Equal(t, core.MCPApplyFailed, result.Outcome)
		require.Equal(t, mcpGateClassCredentialUnavailable, result.FailureClass)
		require.Equal(t, alphaV1.ID, result.AppliedVersionID)
		require.Equal(t, connectsBefore, env.connectTotal(), "凭据不可用不得启动连接")

		require.Equal(t, alphaV1.ID,
			derefVersionID(env.pointerOf(core.ResourceMCPServer, "alpha").AppliedVersionID))

		row := env.operationRow(t, result.OperationID)
		require.Equal(t, core.OperationFailed, row.Status)
		require.Equal(t, mcpApplyResultCredentialUnavailable, row.ResultCode)
		detail := mcpApplyDetailOf(t, row)
		require.Equal(t, mcpGateClassCredentialUnavailable, detail["failure_class"])
	})
}

// TestMCPApply_SupersededByConcurrent 覆盖 C6：快照之后、提交之前的并发写让本次切换
// 整体丢弃——候选连接关闭、注册池与旧连接原样、`applied` 不变，操作行记 `superseded`
// 并点名取代者（publish / apply / revoke 三档）。
func TestMCPApply_SupersededByConcurrent(t *testing.T) {
	t.Parallel()

	envOnly := map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"}

	// prepare 建出一个已连接 v1、已发布 v2 的 server，并为三次建连预置工具集
	// （Start=v1、候选=v2）。返回 v1 与 v2 的版本号。
	prepare := func(t *testing.T) (*mcpApplyEnv, int64, int64) {
		t.Helper()
		env := newMCPApplyEnv(t)
		alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
		env.pushTools("alpha_v1_read")
		env.startVersion(t, "alpha", alphaV1.ID)
		alphaV2 := env.publishMCPArgs(t, "alpha",
			[]string{"--root", "./workspace", "--v2"}, envOnly, envOnlyCredentials(envOnly))
		env.pushTools("alpha_v2_read")
		return env, alphaV1.ID, alphaV2.ID
	}

	assertSuperseded := func(t *testing.T, env *mcpApplyEnv, result core.MCPServerApplyResult,
		err error, wantBy core.MCPApplySupersededBy,
	) {
		t.Helper()
		require.Error(t, err)
		require.ErrorIs(t, err, core.ErrMCPApplySuperseded)
		require.Equal(t, core.MCPApplySuperseded, result.Outcome)
		require.Equal(t, wantBy, result.SupersededBy)
		require.Empty(t, result.DrainDeadline, "被取代没有排空窗口")
		require.True(t, env.connector(1).isClosed(), "候选连接必须丢弃")
		require.False(t, env.connector(0).isClosed(), "旧连接仍在服务")
		_, present := env.registry.Resolve("alpha_v1_read")
		require.True(t, present, "被取代的切换不得动注册池")
		_, leaked := env.registry.Resolve("alpha_v2_read")
		require.False(t, leaked)
		status, ok := env.supervisor.Status("alpha")
		require.True(t, ok)
		require.True(t, status.Connected)
		require.Equal(t, []string{"alpha_v1_read"}, status.Tools)

		row := env.operationRow(t, result.OperationID)
		require.Equal(t, core.OperationFailed, row.Status)
		require.Equal(t, mcpApplyResultSuperseded, row.ResultCode)
		detail := mcpApplyDetailOf(t, row)
		require.Equal(t, string(core.MCPApplySuperseded), detail["outcome"])
		require.Equal(t, string(wantBy), detail["superseded_by"])
	}

	t.Run("发布推进指针", func(t *testing.T) {
		t.Parallel()
		env, alphaV1, _ := prepare(t)
		env.setOnConnect(func(n int) {
			if n != 1 {
				return
			}
			// 快照之后把 published 挪回 v1：CAS 的期望不再成立。指针的操作标识必须指向
			// 既有操作行（外键），因此沿用当前指针上的那个——分类只看指针值。
			pointer := env.pointerOf(core.ResourceMCPServer, "alpha")
			require.NoError(t, env.store.Publish(context.Background(), core.ResourceMCPServer,
				"alpha", alphaV1, false, pointer.OperationID, baselineClock()))
		})
		result, err := env.admin.ApplyMCPServer(context.Background(), resourceActor(), "alpha",
			env.currentETagOf(t, core.ResourceMCPServer, "alpha"), 0)
		assertSuperseded(t, env, result, err, core.MCPApplySupersededByPublish)
	})

	t.Run("另一次 apply 先完成", func(t *testing.T) {
		t.Parallel()
		env, alphaV1, alphaV2 := prepare(t)
		env.setOnConnect(func(n int) {
			if n != 1 {
				return
			}
			// 另一次 apply 已把 applied 推到目标版本：本次的期望（applied=v1）不再成立。
			// 操作标识同样沿用指针上的既有行（外键）；分类只看指针值。
			pointer := env.pointerOf(core.ResourceMCPServer, "alpha")
			swapped, err := env.store.CASApplied(context.Background(), core.ResourceMCPServer,
				"alpha", alphaV2, alphaV1, alphaV2, pointer.OperationID, baselineClock())
			require.NoError(t, err)
			require.True(t, swapped)
		})
		result, err := env.admin.ApplyMCPServer(context.Background(), resourceActor(), "alpha",
			env.currentETagOf(t, core.ResourceMCPServer, "alpha"), 0)
		assertSuperseded(t, env, result, err, core.MCPApplySupersededByApply)
	})

	t.Run("托管凭据被撤销", func(t *testing.T) {
		t.Parallel()
		env := newMCPApplyEnv(t)
		alphaV1 := env.createManagedMCP(t, "alpha")
		env.pushTools("alpha_v1_read")
		env.startVersion(t, "alpha", alphaV1.ID)
		env.pushTools("alpha_v2_read")
		env.setOnConnect(func(n int) {
			if n == 1 {
				env.revoke(t, "alpha", "MCP_ALPHA_TOKEN")
			}
		})
		result, err := env.admin.ApplyMCPServer(context.Background(), resourceActor(), "alpha",
			env.currentETagOf(t, core.ResourceMCPServer, "alpha"), 0)
		require.Error(t, err)
		require.ErrorIs(t, err, core.ErrMCPApplySuperseded)
		require.Equal(t, core.MCPApplySuperseded, result.Outcome)
		require.Equal(t, core.MCPApplySupersededByRevoke, result.SupersededBy)
		require.True(t, env.connector(1).isClosed(), "候选连接必须丢弃")
		row := env.operationRow(t, result.OperationID)
		require.Equal(t, core.OperationFailed, row.Status)
		require.Equal(t, mcpApplyResultSuperseded, row.ResultCode)
		detail := mcpApplyDetailOf(t, row)
		require.Equal(t, string(core.MCPApplySupersededByRevoke), detail["superseded_by"])
	})
}

// TestMCPApply_CASRollback 覆盖 C7：提交段失败（CAS 写不进去）走与建连失败同一段回滚——
// 候选关闭、注册池不变、旧连接保留、`applied` 不变。
func TestMCPApply_CASRollback(t *testing.T) {
	t.Parallel()
	env := newMCPApplyEnv(t)
	ctx := context.Background()

	alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
	env.pushTools("alpha_v1_read")
	env.startVersion(t, "alpha", alphaV1.ID)
	oldConn := env.connector(0)
	oldTool, ok := env.registry.Resolve("alpha_v1_read")
	require.True(t, ok)

	alphaV2 := env.publishMCPArgs(t, "alpha",
		[]string{"--root", "./workspace", "--v2"},
		map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"},
		envOnlyCredentials(map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"}))
	env.pushTools("alpha_v2_read")

	result := env.supervisor.Replace(ctx, env.applyInput(t, "alpha", alphaV2.ID, alphaV1.ID,
		time.Second,
		func(context.Context) (core.MCPApplySupersededBy, error) {
			return core.MCPApplySupersededByNone, errors.New("夹具注入：CAS 写失败")
		}, nil))
	require.Equal(t, core.MCPApplyFailed, result.Outcome)
	require.Equal(t, mcpTestClassDependencyUnconfirmed, result.FailureClass)
	require.Equal(t, alphaV1.ID, result.AppliedVersionID)

	require.True(t, env.connector(1).isClosed(), "候选连接必须关闭")
	require.False(t, oldConn.isClosed(), "旧连接在失败后保留")
	now, ok := env.registry.Resolve("alpha_v1_read")
	require.True(t, ok)
	require.Same(t, oldTool, now, "注册池未被改动")
	_, leaked := env.registry.Resolve("alpha_v2_read")
	require.False(t, leaked)
	status, ok := env.supervisor.Status("alpha")
	require.True(t, ok)
	require.True(t, status.Connected)
	require.Equal(t, alphaV1.ID, status.AppliedVersionID)
	require.Equal(t, alphaV1.ID,
		derefVersionID(env.pointerOf(core.ResourceMCPServer, "alpha").AppliedVersionID))
}

// TestMCPApply_RevokeImmediate 覆盖 C8：凭据撤销**立即**生效，不等一次 apply——
// 门先关死（之后的调用拒绝发出，而不是发出去再收 401），连接随后回收；`applied`
// 保留，因为运行记录里那次运行确实用的是这个已撤销版本。
func TestMCPApply_RevokeImmediate(t *testing.T) {
	t.Parallel()

	t.Run("撤销回收连接并拒绝新调用", func(t *testing.T) {
		t.Parallel()
		env := newMCPApplyEnv(t)
		ctx := context.Background()

		alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
		env.pushTools("alpha_v1_read")
		env.startVersion(t, "alpha", alphaV1.ID)
		pinned, ok := env.registry.Resolve("alpha_v1_read")
		require.True(t, ok)
		inner := env.connector(0).innerTool("alpha_v1_read")
		require.NotNil(t, inner)

		require.NoError(t, env.supervisor.RevokeServer(ctx, "alpha"))

		_, err := pinned.Execute(ctx, core.ToolEnv{}, nil)
		requireExternalRefusal(t, err, mcpLinkRevokedReason)
		require.Equal(t, 0, inner.callCount(), "拒绝必须发生在调用真正发出之前")
		require.True(t, env.connector(0).isClosed(), "连接随后关闭")

		status, ok := env.supervisor.Status("alpha")
		require.True(t, ok)
		require.False(t, status.Connected)
		require.Empty(t, status.Tools)
		require.Equal(t, mcpLinkRevokedReason, status.Error)
		require.Equal(t, alphaV1.ID, status.AppliedVersionID, "版本事实不变")

		require.Equal(t, alphaV1.ID,
			derefVersionID(env.pointerOf(core.ResourceMCPServer, "alpha").AppliedVersionID))

		// 运行记录标明使用了已撤销版本：调用经**真实执行器**落审计行，失败原文就是
		// 撤销短语（不是"发出后 401"的模糊错误），Run 记录据此可事后追溯。
		// The run record marks the revoked credential: the real executor writes a failed
		// audit row whose message is the fixed revoke phrase.
		repo := storage.NewToolInvocationRepo(env.db)
		executor := core.NewToolExecutor(env.registry, repo, slog.New(slog.DiscardHandler))
		res := executor.Execute(ctx, core.ToolEnv{
			RunID: "run-c8-revoked", SessionID: "sess-c8", RunBound: true,
			Tools: map[string]core.Tool{"alpha_v1_read": pinned},
		}, []string{"alpha_v1_read"}, core.ToolCall{
			ID: "call-c8-revoked", Name: "alpha_v1_read", Arguments: `{}`,
		})
		require.Contains(t, res.Content, mcpLinkRevokedReason)
		view, ok, err := repo.Lookup(ctx, "run-c8-revoked", "call-c8-revoked")
		require.NoError(t, err)
		require.True(t, ok, "执行器必须为这次拒绝留下审计行")
		require.Equal(t, core.LLMCallFailed, view.Status)
		require.Equal(t, core.ErrorKindExternalError, view.ErrorKind)
		require.Contains(t, view.ErrorMessage, mcpLinkRevokedReason)
		require.Equal(t, 0, inner.callCount(), "审计里的失败来自拒绝，不是发出后的响应")
	})

	t.Run("槽位撤销触发即时回收", func(t *testing.T) {
		t.Parallel()
		env := newMCPApplyEnv(t)
		ctx := context.Background()

		alphaV1 := env.createManagedMCP(t, "alpha")
		env.pushTools("alpha_v1_read")
		env.startVersion(t, "alpha", alphaV1.ID)
		pinned, ok := env.registry.Resolve("alpha_v1_read")
		require.True(t, ok)

		service, err := NewResourceStateService(ResourceStateDeps{
			Resources:     env.store,
			Credentials:   env.creds,
			Operations:    env.ops,
			Admin:         env.admin,
			Scopes:        &ScopeService{Resources: env.store, Agents: env.store, WorkspaceRoot: env.root, Now: baselineClock},
			CredentialOps: env.revoker,
			Channels: newNotifyChannelReadPort(storage.NewNotifyChannelRepo(env.db),
				env.creds, env.store, notifyReadDeps{}),
			Logger:     env.logger,
			Now:        baselineClock,
			MCPRevoker: env.supervisor.RevokeServer,
		})
		require.NoError(t, err)

		out, err := service.RevokeCredential(ctx, resourceActor(), CredentialRevokeCommand{
			Kind:        "mcp_server",
			Name:        "alpha",
			ResourceUID: string(env.uid(t, "alpha")),
			BindingKey:  "MCP_ALPHA_TOKEN",
			Confirm:     true,
			IfMatch:     env.currentETagOf(t, core.ResourceMCPServer, "alpha"),
		})
		require.NoError(t, err)
		require.Equal(t, "MCP_ALPHA_TOKEN", out.BindingKey)

		require.True(t, env.connector(0).isClosed(), "撤销不等 apply：连接立即回收")
		status, ok := env.supervisor.Status("alpha")
		require.True(t, ok)
		require.False(t, status.Connected)
		require.Equal(t, mcpLinkRevokedReason, status.Error)
		require.Equal(t, alphaV1.ID, status.AppliedVersionID)
		_, err = pinned.Execute(ctx, core.ToolEnv{}, nil)
		requireExternalRefusal(t, err, mcpLinkRevokedReason)
		require.Empty(t, env.listOperations(core.OperationFilter{Action: core.ActionMCPServerApply}),
			"撤销路径不得伪造一条受控发布记录")
	})
}

// TestMCPApply_PendingRowVerifiedNotReplayed 覆盖契约 §9.4 的恢复表：重启后对 pending 的
// apply 行只比较 `applied` 指针与 detail 里的目标 / 前一版本——指针 = 目标 → applied、
// = 前一版本 → not_applied、第三个版本 / detail 缺键 / 身份读不懂 → unknown；查证
// **不得重放**切换（不建连接、不改指针、不动注册池）。
// The recovery verdict compares the applied pointer with the frozen versions and never
// replays the apply.
func TestMCPApply_PendingRowVerifiedNotReplayed(t *testing.T) {
	t.Parallel()
	env := newMCPApplyEnv(t)
	ctx := context.Background()

	alphaV1 := env.createMCPStdio(t, "alpha", "allowed-mcp")
	env.pushTools("alpha_v1_read")
	env.startVersion(t, "alpha", alphaV1.ID)
	alphaV2 := env.publishMCPArgs(t, "alpha", []string{"--read"}, nil, nil)

	opID := "op-pending-apply"
	require.NoError(t, env.ops.BeginOperation(ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       core.ActionMCPServerApply,
		TargetKind:   core.TargetKindMCPServer,
		TargetID:     string(env.uid(t, "alpha")),
		ActorSubject: resourceActor().Subject,
		AuthMethod:   resourceActor().Method,
		Source:       resourceActor().Source,
		RequestHash:  mcpApplyRequestHash("alpha", alphaV2.ID),
		Status:       core.OperationPending,
		// 预写形态与生产路径同源（mcpApplyDetail）：崩溃时 detail 里已有两个版本号。
		DetailJSON: mcpApplyDetail(core.MCPServerApplyResult{
			TargetVersionID:   alphaV2.ID,
			PreviousVersionID: alphaV1.ID,
			AppliedVersionID:  alphaV1.ID,
		}),
		CreatedAt: baselineClock(),
	}))
	deps := RecoveryDeps{Resources: env.store}

	before := env.pointerOf(core.ResourceMCPServer, "alpha")
	require.Equal(t, alphaV1.ID, derefVersionID(before.AppliedVersionID))
	require.Equal(t, core.VerificationNotApplied,
		verifyOperation(ctx, deps, env.operationRow(t, opID)), "指针仍是操作前版本")
	require.Equal(t, core.OperationFailed, verifyOutcome(core.VerificationNotApplied))

	switched, err := env.store.CASApplied(ctx, core.ResourceMCPServer, "alpha",
		alphaV2.ID, alphaV1.ID, alphaV2.ID, opID, baselineClock())
	require.NoError(t, err)
	require.True(t, switched)
	require.Equal(t, core.VerificationApplied,
		verifyOperation(ctx, deps, env.operationRow(t, opID)), "指针 = 目标版本")
	require.Equal(t, core.OperationSuccess, verifyOutcome(core.VerificationApplied))

	// 第三版本：另一次 apply 已把指针挪到 v3，本操作到底有没有生效无法判定。
	alphaV3 := env.publishMCPArgs(t, "alpha", []string{"--read", "--v3"}, nil, nil)
	switched, err = env.store.CASApplied(ctx, core.ResourceMCPServer, "alpha",
		alphaV3.ID, alphaV2.ID, alphaV3.ID, opID, baselineClock())
	require.NoError(t, err)
	require.True(t, switched)
	require.Equal(t, core.VerificationIndeterminate,
		verifyOperation(ctx, deps, env.operationRow(t, opID)), "指针是第三个版本")
	require.Equal(t, core.OperationUnknown, verifyOutcome(core.VerificationIndeterminate))

	// detail 缺键（旧形态 `{}`）与身份读不懂：都不猜，交 unknown。
	blankID := "op-pending-apply-blank"
	require.NoError(t, env.ops.BeginOperation(ctx, core.OperationRecord{
		OperationID:  blankID,
		Action:       core.ActionMCPServerApply,
		TargetKind:   core.TargetKindMCPServer,
		TargetID:     string(env.uid(t, "alpha")),
		ActorSubject: resourceActor().Subject,
		AuthMethod:   resourceActor().Method,
		Source:       resourceActor().Source,
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    baselineClock(),
	}))
	require.Equal(t, core.VerificationIndeterminate,
		verifyOperation(ctx, deps, env.operationRow(t, blankID)))
	require.Equal(t, core.VerificationIndeterminate, verifyMCPApply(ctx, deps, core.OperationRecord{
		Action:     core.ActionMCPServerApply,
		TargetKind: core.TargetKindMCPServer,
		TargetID:   "not-a-resource-uid",
		DetailJSON: mcpApplyDetail(core.MCPServerApplyResult{
			TargetVersionID: alphaV2.ID, PreviousVersionID: alphaV1.ID,
		}),
	}))

	require.Equal(t, 1, env.connectTotal(), "查证不得重放 apply（不建新连接）")
	after := env.pointerOf(core.ResourceMCPServer, "alpha")
	require.Equal(t, alphaV3.ID, derefVersionID(after.AppliedVersionID), "查证不改指针")
	status, ok := env.supervisor.Status("alpha")
	require.True(t, ok)
	require.Equal(t, alphaV1.ID, status.AppliedVersionID, "运行态仍停在恢复前的版本")
}
