package app

// 本文件是工具来源投影的验收面（模块卡 §4.5.8 / 契约 D-06 / 约束 C-07）：回答"这条工具
// 最终是谁注册成功的、被跳过的为什么被跳过"。断言三条不变量：`tools` 仍是 offered 列表
// （`McpServerView` 语义不动）、每个 server 恒有 `accepted + len(skipped) == offered`、
// 赢家来源具体（内置记 `builtin`，被别的 server 抢走时记那个 server 名，不猜 `unknown`）。
// 注册池的重名语义由 `TestRegistry_DuplicateSemanticsUnchanged` 在 `internal/tool` 侧钉住；
// 这里的重名错误用**真实注册池**制造，不伪造错误文案。
// These cases pin the disposition projection: offered semantics untouched, the
// accepted/skipped invariant, and a concrete winner source. The web-level JSON shape is
// covered by TestMCPContracts_ToolDisposition (T017/T029).

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// newDispositionFixture 建一张空白来源表与一个运行期句柄：`helper` / `other` 都已连接，
// 工具清单即 offered 列表。表注入方式与 wire 装配同形（句柄持有同一张表的指针）。
// newDispositionFixture wires the same table shape wire injects into the runtime handle.
func newDispositionFixture() (*mcpDispositionTable, *mcpRuntime) {
	dispositions := newMCPDispositionTable()
	runtime := newMCPRuntime(
		[]core.MCPServerConfig{{Name: "helper"}, {Name: "other"}},
		[]tool.MCPConnection{
			{Server: "helper", Tools: []core.Tool{fakeTool{name: "write_file"}, fakeTool{name: "echo"}}},
			{Server: "other", Tools: []core.Tool{fakeTool{name: "echo"}}},
		},
	)
	runtime.dispositions = dispositions
	return dispositions, runtime
}

// duplicateRegisterErr 用真实注册池制造一次重名拒绝：先登记原名，再登记同名。
// duplicateRegisterErr produces the registry's real duplicate rejection error.
func duplicateRegisterErr(t *testing.T, name string) error {
	t.Helper()
	logger, _ := newCapturedLogger()
	registry := tool.NewRegistry(logger)
	require.NoError(t, registry.Register(fakeTool{name: name}))
	err := registry.Register(fakeTool{name: name})
	require.Error(t, err, "注册池必须拒绝重名")
	return err
}

// requireDispositionInvariant 逐条核对 `accepted + len(skipped) == offered`。
// requireDispositionInvariant pins the per-server count invariant.
func requireDispositionInvariant(t *testing.T, disposition *core.MCPToolDisposition) {
	t.Helper()
	require.NotNil(t, disposition)
	require.Equal(t, disposition.Offered, disposition.Accepted+len(disposition.Skipped),
		"accepted + len(skipped) 必须等于 offered：%+v", disposition)
}

// TestMCPToolDisposition_BuiltinWins：内置先注册、同名 MCP 工具被拒；来源记 `builtin`，
// `tools` 仍是该 server 提供的两个工具名（offered，不是 accepted）。
// 逆改点：把 `tools` 改成 accepted 列表 → 红；赢家写成 MCP server / `unknown` → 红。
func TestMCPToolDisposition_BuiltinWins(t *testing.T) {
	t.Parallel()
	dispositions, runtime := newDispositionFixture()

	dispositions.recordBuiltin("write_file")
	dispositions.recordMCP("helper", "write_file", duplicateRegisterErr(t, "write_file"))
	dispositions.recordMCP("helper", "echo", nil)

	disposition := runtime.ToolDisposition("helper")
	requireDispositionInvariant(t, disposition)
	require.Equal(t, 2, disposition.Offered)
	require.Equal(t, 1, disposition.Accepted)
	require.Equal(t, []core.MCPToolSkip{{
		Name:         "write_file",
		WinnerSource: "builtin",
		Reason:       "duplicate_name",
	}}, disposition.Skipped)
	require.Equal(t, []string{"write_file", "echo"}, runtime.Tools("helper"),
		"tools 仍应是 offered 列表（含被跳过的 write_file）")
}

// TestMCPToolDisposition_TwoServers：两个 server 提供同名工具时先注册者赢，后被拒者的
// 跳过条目点名**具体**赢家 server（不是 `unknown`）；两个 server 的计数各自成立。
// 逆改点：按名字查全局列表推断归属 / 两个 server 共享一份计数 → 红。
func TestMCPToolDisposition_TwoServers(t *testing.T) {
	t.Parallel()
	dispositions, runtime := newDispositionFixture()

	dispositions.recordMCP("helper", "echo", nil)
	dispositions.recordMCP("other", "echo", duplicateRegisterErr(t, "echo"))

	helper := runtime.ToolDisposition("helper")
	requireDispositionInvariant(t, helper)
	require.Equal(t, 1, helper.Accepted)
	require.Empty(t, helper.Skipped, "先注册者不应有跳过条目")

	other := runtime.ToolDisposition("other")
	requireDispositionInvariant(t, other)
	require.Equal(t, 0, other.Accepted)
	require.Equal(t, []core.MCPToolSkip{{
		Name:         "echo",
		WinnerSource: "helper",
		Reason:       "duplicate_name",
	}}, other.Skipped, "赢家必须是具体 server 名，不得写 unknown")
}

// TestMCPToolDisposition_InvalidTool：空白工具名不能出现在任何 `AGENT.md tools:` 声明里，
// 也无法作为模型工具暴露——记 `invalid_tool` 跳过，不计 accepted；没有赢家，来源为
// `unknown`（不猜）。合法名字不受影响。
// 逆改点：把空白名记 accepted、或跳过原因写成 duplicate_name → 红。
func TestMCPToolDisposition_InvalidTool(t *testing.T) {
	t.Parallel()
	dispositions, runtime := newDispositionFixture()

	dispositions.recordMCP("helper", "   ", nil)
	dispositions.recordMCP("helper", "", nil)
	dispositions.recordMCP("helper", "echo", nil)

	disposition := runtime.ToolDisposition("helper")
	requireDispositionInvariant(t, disposition)
	require.Equal(t, 3, disposition.Offered)
	require.Equal(t, 1, disposition.Accepted)
	require.Equal(t, []core.MCPToolSkip{
		{Name: "   ", WinnerSource: "unknown", Reason: "invalid_tool"},
		{Name: "", WinnerSource: "unknown", Reason: "invalid_tool"},
	}, disposition.Skipped)

	require.False(t, mcpValidToolName("   "))
	require.False(t, mcpValidToolName(""))
	require.True(t, mcpValidToolName("echo"), "合法工具名不得被误判")
}

// TestMCPToolDisposition_ConnectedWithNoTools：连上了但一个工具都没提供的 server 必须给出
// 零计数对象（offered = accepted = 0、skipped 为空切片），不是 null——D-06 要求连接后
// `offered = len(tools)`，null 没有计数可比。（2026-10-25 控制台走查发现：fake-empty
// 已连接却在页面上显示"未连接，暂无注册记录"。）
// 逆改点：已连接零工具时返回 nil → 红；返回不带空切片的零值结构（JSON 出 null）→ 红。
func TestMCPToolDisposition_ConnectedWithNoTools(t *testing.T) {
	t.Parallel()
	dispositions := newMCPDispositionTable()
	runtime := newMCPRuntime(
		[]core.MCPServerConfig{{Name: "empty"}},
		[]tool.MCPConnection{{Server: "empty"}},
	)
	runtime.dispositions = dispositions

	disposition := runtime.ToolDisposition("empty")
	requireDispositionInvariant(t, disposition)
	require.Equal(t, 0, disposition.Offered)
	require.Equal(t, 0, disposition.Accepted)
	require.NotNil(t, disposition.Skipped, "空跳过必须是 []，出站不得成 null")
	require.Empty(t, disposition.Skipped)
	require.Empty(t, runtime.Tools("empty"), "零工具 server 的 offered 列表为空")
}

// TestMCPToolDisposition_NilWhenUnknown：没有来源表、未知 server、以及**未连接**的 server
// 一律返回 nil（D-06：未连接 / 未知为 null）；已连接且有记录时返回非 nil。
// 逆改点：给未连接 server 编一份空对象、或未知 server 返回零值结构 → 红。
func TestMCPToolDisposition_NilWhenUnknown(t *testing.T) {
	t.Parallel()
	dispositions, runtime := newDispositionFixture()

	dispositions.recordMCP("helper", "echo", nil)

	require.NotNil(t, runtime.ToolDisposition("helper"))
	require.Nil(t, runtime.ToolDisposition("ghost"), "未知 server 必须为 nil")

	disconnected := newMCPRuntime([]core.MCPServerConfig{{Name: "helper"}}, nil)
	disconnected.dispositions = dispositions
	require.Nil(t, disconnected.ToolDisposition("helper"), "未连接 server 必须为 nil")

	noTable := newMCPRuntime([]core.MCPServerConfig{{Name: "helper"}}, nil)
	require.Nil(t, noTable.ToolDisposition("helper"), "没有来源表时必须为 nil")
}
