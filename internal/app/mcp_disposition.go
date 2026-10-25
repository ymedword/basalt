package app

// 本文件是装配层的工具来源表（颗粒度文档 §2.5.7 / §3.1.4、契约 D-06、约束 C-07）：
// 注册池（`internal/tool.Registry`）只存 `core.Tool`、不带来源，本表在装配期记录
// "谁注册成功 / 谁被跳过 / 赢家是谁"，供 `McpServerView.tool_disposition` 投影。
// **不**改注册池的重名语义（拒绝重名、保留原注册、WARN 照旧），也**不**把本表变成
// 第二套事实源：`tools` 仍是该 server 提供的工具名（offered）。
// The assembly-side source table: who registered, who was skipped and why. The
// registry's duplicate semantics stay untouched; this only projects the outcome.

import (
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// 来源与跳过原因字面量逐字对齐契约 D-06。
// Source and reason literals mirror contract D-06.
const (
	mcpDispositionBuiltin     = "builtin"
	mcpDispositionUnknown     = "unknown"
	mcpDispositionDuplicate   = "duplicate_name"
	mcpDispositionInvalidTool = "invalid_tool"
)

// mcpValidToolName 判定一个 MCP 工具名能否出现在 `AGENT.md` 的 `tools:` 声明里：
// 声明按空白切分、按名逐字比对，空白名与含空白 / 控制字符的名字永远对不上，也无法
// 作为模型侧工具名暴露。判据只依赖名字本身，不查注册池。
// A blank or whitespace/control-bearing name can never appear in a tools declaration.
func mcpValidToolName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// mcpDispositionTable 是装配期一次写入、之后只读的来源表：`sources` 记每个已注册
// 工具名的赢家来源（`builtin` 或 server 名），`byServer` 记每个 server 的计数与跳过
// 明细。两者都只在装配期（单 goroutine 的连接回调）写入，运行期只读。
// mcpDispositionTable is written once at assembly and only read afterwards.
type mcpDispositionTable struct {
	sources  map[string]string
	byServer map[string]core.MCPToolDisposition
}

// newMCPDispositionTable 建一张空表。
// newMCPDispositionTable builds the empty table.
func newMCPDispositionTable() *mcpDispositionTable {
	return &mcpDispositionTable{
		sources:  map[string]string{},
		byServer: map[string]core.MCPToolDisposition{},
	}
}

// recordBuiltin 记一个内置工具的来源。内置先于 MCP 注册（装配序即判据），因此它一旦
// 登记就是同名工具的赢家；已登记过的名字保持原样（同一内置名不会重复成功注册）。
// recordBuiltin marks a registered builtin as the winner for its name.
func (t *mcpDispositionTable) recordBuiltin(name string) {
	if _, ok := t.sources[name]; !ok {
		t.sources[name] = mcpDispositionBuiltin
	}
}

// recordMCP 记一条 MCP 工具的注册结果：offered 恒 +1；空白名记 `invalid_tool`（不注册，
// 无赢家）、注册报错记 `duplicate_name`（赢家取来源表，查不到才写 `unknown`，不猜）、
// 注册成功则计入 accepted 并把来源记成该 server（先到先得）。
// registerErr 只区分"成功 / 失败"，原因不解析错误文案（D-05 同类纪律）。
// recordMCP records one MCP tool: offered always grows; blank names and duplicate
// rejections become skips; a success claims the name for its server.
func (t *mcpDispositionTable) recordMCP(server, tool string, registerErr error) {
	entry, ok := t.byServer[server]
	if !ok {
		entry = core.MCPToolDisposition{Skipped: []core.MCPToolSkip{}}
	}
	entry.Offered++
	switch {
	case !mcpValidToolName(tool):
		entry.Skipped = append(entry.Skipped, core.MCPToolSkip{
			Name:         tool,
			WinnerSource: mcpDispositionUnknown,
			Reason:       mcpDispositionInvalidTool,
		})
	case registerErr != nil:
		winner := t.sources[tool]
		if winner == "" {
			winner = mcpDispositionUnknown
		}
		entry.Skipped = append(entry.Skipped, core.MCPToolSkip{
			Name:         tool,
			WinnerSource: winner,
			Reason:       mcpDispositionDuplicate,
		})
	default:
		entry.Accepted++
		if _, claimed := t.sources[tool]; !claimed {
			t.sources[tool] = server
		}
	}
	t.byServer[server] = entry
}

// view 返回该 server 的注册投影副本；未知 server 返回 nil（D-06：未知为 null）。
// 复制 `Skipped`，调用方改写返回值不会污染来源表。
// view returns a defensive copy; an unknown server yields nil.
func (t *mcpDispositionTable) view(server string) *core.MCPToolDisposition {
	entry, ok := t.byServer[server]
	if !ok {
		return nil
	}
	entry.Skipped = slices.Clone(entry.Skipped)
	return &entry
}
