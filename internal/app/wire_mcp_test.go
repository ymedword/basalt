package app

// 本文件是 US-7 装配面 MCP 接线的验收用例（颗粒度文档 §3.2 的 app/wire_mcp_test.go 七例，
// 当前已落 US1 三例 + US2 三例 + 夹具）。夹具 `fakeMCPConnector` / `fakeMCPTool` /
// `writeMCPServersYAML` 是 R3 / R8 / R10 判别力的来源：连接器可注入连接结果与关闭回调，
// 工具替身可判"注册池里是谁赢了"。
// Acceptance tests for the MCP wiring; the fake connector and tool are the discriminators
// for R3 / R8 / R10.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// fakeMCPTool 是 `core.Tool` 替身：**指针类型** + 指针接收者——R8 用类型断言
// `got.(*fakeMCPTool)` 判"谁赢了"。`Execute` 返回固定内容；R8 **不执行**内置工具
// （空 `ToolEnv` 下执行会被沙箱合法拒绝，测的就不是注册语义了）。
// fakeMCPTool is a pointer-typed tool double whose identity tells R8 which registration won.
type fakeMCPTool struct {
	name    string
	content string
}

// Name 返回替身工具名。
// Name returns the double's tool name.
func (f *fakeMCPTool) Name() string { return f.name }

// Description 返回固定描述。
// Description returns a fixed description.
func (f *fakeMCPTool) Description() string { return "MCP 替身工具" }

// InputSchema 返回最小对象 schema。
// InputSchema returns a minimal object schema.
func (f *fakeMCPTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

// Execute 返回构造时给定的固定内容。
// Execute returns the fixed content the double was built with.
func (f *fakeMCPTool) Execute(context.Context, core.ToolEnv, json.RawMessage) (core.Result, error) {
	return core.Result{Content: f.content}, nil
}

// fakeMCPConnector 实现 `mcpConnector`：记录收到的 specs、返回构造好的连接或注入错误、
// `Close` 记调用并可执行回调（R3 / R10 的判别力来源）。
// fakeMCPConnector records the specs it was handed, returns canned connections or an
// injected error, and reports Close calls.
type fakeMCPConnector struct {
	specs   []core.MCPServerConfig
	conns   []tool.MCPConnection
	err     error
	closed  bool
	onClose func() error
}

// 编译期断言：夹具与装配层接口保持同形。
// Compile-time assertion that the fixture matches the assembly-layer interface.
var _ mcpConnector = (*fakeMCPConnector)(nil)

// Connect 记录 specs 并返回构造好的连接或注入错误。
// Connect records the specs and returns the canned result.
func (f *fakeMCPConnector) Connect(_ context.Context, specs []core.MCPServerConfig) ([]tool.MCPConnection, error) {
	f.specs = specs
	if f.err != nil {
		return nil, f.err
	}
	return f.conns, nil
}

// Close 记录关闭并执行可选回调；回调的错误原样上抛（R10 后半靠它观察停机顺序）。
// Close records the call and returns the optional callback's error.
func (f *fakeMCPConnector) Close() error {
	f.closed = true
	if f.onClose != nil {
		return f.onClose()
	}
	return nil
}

// writeMCPServersYAML 在工作区根写 `servers:` 列表形态的最小配置（app 测试不能复用
// `workspace_test` 的包内夹具）。
// writeMCPServersYAML writes a minimal servers-list configuration into the workspace root.
func writeMCPServersYAML(t *testing.T, root string, names []string) {
	t.Helper()
	content := "servers:\n"
	for _, name := range names {
		content += "  - name: " + name + "\n    command: unused-mcp\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "mcp_servers.yaml"), []byte(content), 0o644))
}

// TestWire_MCPToolsRegisteredAfterEqualityGate（**R3 前半**）：池含 MCP 工具，且装配成功
// 本身证明等值门禁先于 MCP 注册——把注册挪到门禁之前 → 装配失败红。
// R3 front half: the pool contains MCP tools, and successful assembly proves the equality
// gate ran before MCP registration.
func TestWire_MCPToolsRegisteredAfterEqualityGate(t *testing.T) {
	fx := newWireFixture(t)
	writeMCPServersYAML(t, fx.root, []string{"search"})
	connector := &fakeMCPConnector{conns: []tool.MCPConnection{{
		Server: "search",
		Tools: []core.Tool{
			&fakeMCPTool{name: "search_docs", content: "hit"},
			&fakeMCPTool{name: "fetch_page", content: "page"},
		},
	}}}

	app := fx.app()
	app.mcpConnect = connector
	rt, err := app.newRuntime(context.Background())
	require.NoError(t, err, "等值门禁必须先于 MCP 注册，否则装配直接失败")
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	require.Contains(t, rt.registry.Names(), "search_docs")
	require.Contains(t, rt.registry.Names(), "fetch_page")
}

// TestWire_DuplicateMCPToolKeepsBuiltin（**R8**）：MCP 也提供 `read_file` → 保留内置工具 +
// 其余 MCP 工具照常注册 + WARN `拒绝重复注册的工具`。判别方式 = 身份断言（类型断言），
// **不执行**内置工具（空 `ToolEnv` 下执行会被沙箱合法拒绝）。
// R8: a duplicate MCP tool name keeps the builtin registration and warns; the remaining
// MCP tools still register.
func TestWire_DuplicateMCPToolKeepsBuiltin(t *testing.T) {
	fx := newWireFixture(t)
	writeMCPServersYAML(t, fx.root, []string{"shadow"})
	connector := &fakeMCPConnector{conns: []tool.MCPConnection{{
		Server: "shadow",
		Tools: []core.Tool{
			&fakeMCPTool{name: "read_file", content: "shadowed"},
			&fakeMCPTool{name: "search_docs", content: "hit"},
		},
	}}}

	app := fx.app()
	app.mcpConnect = connector
	rt, err := app.newRuntime(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	got, ok := rt.registry.Resolve("read_file")
	require.True(t, ok)
	require.Equal(t, "read_file", got.Name())
	_, isFake := got.(*fakeMCPTool)
	require.False(t, isFake, "内置 read_file 必须保留（keep-first）——MCP 替身赢了这里就红")
	require.Contains(t, rt.registry.Names(), "search_docs", "其余 MCP 工具照常注册")
	require.Contains(t, fx.logs.String(), "拒绝重复注册的工具")
}

// TestWire_AgentDeclaringFailedMCPToolIsNotRegistered（**R3 后半**）：连接失败后 server 的
// 工具不在池里，声明该工具的 Agent 命中**规则 4**——日志里必须出现契约字面量
// `tool not registered: <tool>`（装配层把「已配置 + 已连接」两名单都注入后才会走到拒绝，
// 缺任一名单都会退化成第三态，这里判的就是那条退化不发生）。
// R3 back half: a failed server's tool never enters the pool, so the declaring agent is
// rejected by rule 4 with the contract literal.
func TestWire_AgentDeclaringFailedMCPToolIsNotRegistered(t *testing.T) {
	fx := newWireFixture(t)
	writeMCPServersYAML(t, fx.root, []string{"broken"})
	fx.writeAgent(t, "scribe", "broken_probe")
	connector := &fakeMCPConnector{conns: nil}
	app := fx.app()
	app.mcpConnect = connector
	rt, err := app.newRuntime(context.Background())
	require.NoError(t, err, "连接失败不得中断启动")
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	require.NotContains(t, rt.registry.Names(), "broken_probe")
	require.Contains(t, fx.logs.String(), `"reason":"tool not registered: broken_probe"`)
	require.Contains(t, fx.logs.String(), "Agent 未注册")
}

// TestWire_AgentDeclaringFailedServerOnlyIsNotRegistered（规则 6）：Agent 只声明 server 名、
// 不声明工具——连接失败后 server 不在「已连接」集，整只不注册，reason 为契约字面量
// `mcp server skipped`、detail 点名 server；不得误报成第三态「待确认」。
// Rule 6: an agent whose only declaration is a failed server is rejected outright, not
// downgraded to the unverified third state.
func TestWire_AgentDeclaringFailedServerOnlyIsNotRegistered(t *testing.T) {
	fx := newWireFixture(t)
	writeMCPServersYAML(t, fx.root, []string{"broken"})

	// 只声明 server、不声明工具：规则 4 无输入，判定全部落在规则 6。
	dir := filepath.Join(fx.root, agentsDirName, "courier")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := "---\n" +
		"name: courier\n" +
		"description: 只声明 server 的探针\n" +
		"provider:\n  name: probe\n  model: probe-model\n" +
		"tools: []\n" +
		"mcp_servers:\n  - broken\n" +
		"---\n\n你是「courier」。\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(content), 0o644))

	connector := &fakeMCPConnector{} // 零连接：broken 已配置但未连接
	app := fx.app()
	app.mcpConnect = connector
	rt, err := app.newRuntime(context.Background())
	require.NoError(t, err, "连接失败不得中断启动")
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	require.Contains(t, fx.logs.String(), `"reason":"mcp server skipped"`)
	require.Contains(t, fx.logs.String(), `"detail":"broken"`)
	require.Contains(t, fx.logs.String(), "Agent 未注册")
	require.NotContains(t, fx.logs.String(), "Agent 注册待确认", "重路径两名单齐备时不得退化成第三态")
}

// TestWire_ConnectCanceledAbortsAndClosesPool（R10 前哨）：连接器只在**整体取消 / 截止**时
// 返回错误；此时装配失败，且连接池必须在这一步关闭——半开会话与子进程不得留在失败路径上。
// A canceled connect aborts assembly and the pool is closed on the failure path.
func TestWire_ConnectCanceledAbortsAndClosesPool(t *testing.T) {
	fx := newWireFixture(t)
	writeMCPServersYAML(t, fx.root, []string{"search"})

	connector := &fakeMCPConnector{err: context.Canceled}
	app := fx.app()
	app.mcpConnect = connector
	rt, err := app.newRuntime(context.Background())
	require.Error(t, err, "整体取消必须中断装配（其余失败才吸收为跳过）")
	require.Nil(t, rt)
	require.True(t, connector.closed, "装配失败必须关闭连接池——半开会话不得留过启动")
}

// TestWire_StartupSummaryCountsConnectedServers（FR-027 / 约束 23）：`mcp_connected` =
// **实际连接数**而不是配置数——配置两条、连接器只返回一条，摘要必须是 1。
// The startup summary reports the number of servers actually connected, not the number
// configured.
func TestWire_StartupSummaryCountsConnectedServers(t *testing.T) {
	fx := newWireFixture(t)
	writeMCPServersYAML(t, fx.root, []string{"search", "broken"})
	connector := &fakeMCPConnector{conns: []tool.MCPConnection{{
		Server: "search",
		Tools:  []core.Tool{&fakeMCPTool{name: "search_docs", content: "hit"}},
	}}}

	app := fx.app()
	app.mcpConnect = connector
	rt, err := app.newRuntime(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	require.Contains(t, fx.logs.String(), `"mcp_connected":1`, "摘要必须报实际连接数")
	require.NotContains(t, fx.logs.String(), `"mcp_connected":2`, "配置数不得冒充连接数")
}

// TestWire_MCPConfigChangesNeedRestart（FR-011）：当前 runtime 只认装配时读到的配置——
// 运行中改 `mcp_servers.yaml` 不影响已建 runtime 的注册池；重建 runtime（同工作区需先
// 释放实例锁）才读到新配置。逆改点：若装配接上配置监听 / 每次调用重读配置，
// 「旧 runtime 不得出现新工具」断言红。
// FR-011: the live runtime is unaffected by configuration edits; a rebuilt runtime sees
// the new configuration.
func TestWire_MCPConfigChangesNeedRestart(t *testing.T) {
	fx := newWireFixture(t)
	writeMCPServersYAML(t, fx.root, []string{"alpha"})
	first := &fakeMCPConnector{conns: []tool.MCPConnection{{
		Server: "alpha",
		Tools:  []core.Tool{&fakeMCPTool{name: "alpha_tool", content: "a"}},
	}}}
	app := fx.app()
	app.mcpConnect = first
	rt1, err := app.newRuntime(context.Background())
	require.NoError(t, err)
	require.Contains(t, rt1.registry.Names(), "alpha_tool")

	// 运行中改配置：当前 runtime 的注册池不因文件变化而变化（不热加载）。
	writeMCPServersYAML(t, fx.root, []string{"beta"})
	require.Contains(t, rt1.registry.Names(), "alpha_tool", "配置修改不得影响当前 runtime")
	require.NotContains(t, rt1.registry.Names(), "beta_tool")

	// 同工作区的第二个 runtime 需要先释放实例锁。
	require.NoError(t, rt1.Close())

	second := &fakeMCPConnector{conns: []tool.MCPConnection{{
		Server: "beta",
		Tools:  []core.Tool{&fakeMCPTool{name: "beta_tool", content: "b"}},
	}}}
	app2 := fx.app()
	app2.mcpConnect = second
	rt2, err := app2.newRuntime(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt2.Close()) })

	require.Equal(t, []string{"beta"}, mcpSpecNames(second.specs), "重建后才读新配置")
	require.Contains(t, rt2.registry.Names(), "beta_tool")
	require.NotContains(t, rt2.registry.Names(), "alpha_tool")
}

// mcpSpecNames 取连接器收到的 server 名序列（判"读到的是哪一份配置"）。
// mcpSpecNames returns the server names the connector was handed.
func mcpSpecNames(specs []core.MCPServerConfig) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// TestWire_CloseClosesMCPBeforeDatabase（**R10 后半**）：停机顺序（app 层）：池先于库关闭。
// 前提：工作区必须有一条 MCP 配置——零 server 时装配不建池，Close 自然不会碰连接器。
// 库存活性用真实 API 观察：rt.db.SchemaVersion(ctx)（storage.DB 没有 PingContext）。
// 逆改点：把 Close 顺序改成「库 → 池」→ 连接器 Close 里 SchemaVersion 返回错误，
// rt.Close() 经 errors.Join 上抛，NoError 断言红；忘了关池 → closed 断言红。
// R10 back half: the MCP pool must close before the database, observed via a live query
// from the connector's Close callback.
func TestWire_CloseClosesMCPBeforeDatabase(t *testing.T) {
	fx := newWireFixture(t)
	writeMCPServersYAML(t, fx.root, []string{"probe"}) // 建池前提：至少一个 server
	var rt *heavyRuntime
	connector := &fakeMCPConnector{onClose: func() error {
		_, err := rt.db.SchemaVersion(context.Background()) // 库还活着 = 顺序正确
		return err
	}}
	app := fx.app()
	app.mcpConnect = connector
	got, err := app.newRuntime(context.Background())
	require.NoError(t, err)
	rt = got
	require.NoError(t, rt.Close())
	require.True(t, connector.closed, "heavyRuntime.Close 必须先关闭 MCP 池")
}
