package app

// 本文件是 MCP 连接的运行期门（14D / 设计 §2.3.3、§2.3.3.1）：受控重建（apply）与凭据撤销
// 都要在**不重启整站**的前提下换掉或收掉一代连接，而已经发出的调用必须被覆盖到。门因此
// 只做三件事：
//
//   - **在途计数**：调用进入时登记、返回时销号；排空窗口等的就是这张计数表归零；
//   - **状态闸门**：`live` 放行、`draining` 与 `closed` 一律**拒绝发出**——拒绝发生在调用
//     目标之前，不是"发出去再 401"；
//   - **固定短语**：拒绝理由是自产短语（经 `*core.ExternalError` 分类为 `external_error`），
//     每次拒绝照旧落一条工具审计 `failed`——不静默挂起，也不静默转到新连接。
//
// 组合方式是包装而不是改造：`mcpLinkTool` 包住连接池产出的 `core.Tool`，名字 / 描述 /
// schema 原样透传。旧 Run 在准入时钉住的是这些包装实例，因此"旧 Run 不会被悄悄换到新连接"
// 由对象身份本身保证，不需要额外登记表。
// The link is a per-generation gate: in-flight accounting, a state gate that refuses new
// calls before they are issued, and fixed refusal phrases. Wrapping the adapter (rather
// than changing it) means an admitted run's pinned instance can never be retargeted.

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// 拒绝新调用的固定短语：三条覆盖排空、关闭与撤销三种终态。短语进工具结果与审计
// `failed`，因此必须自产、可读、不含任何连接参数。
// Fixed refusal phrases for the three non-live states.
const (
	mcpLinkDrainingReason = "MCP 连接正在切换（排空窗口内不再接受新调用）：该调用未发出"
	mcpLinkClosedReason   = "MCP 连接已关闭：该调用未发出"
	mcpLinkRevokedReason  = "MCP 凭据已撤销，连接已关闭：该调用未发出"
)

// mcpLinkState 是一代连接的门状态。
// mcpLinkState is one generation's gate state.
type mcpLinkState int

const (
	// mcpLinkLive：正常服务，新调用放行。
	mcpLinkLive mcpLinkState = iota
	// mcpLinkDraining：切换后进入排空——已发出的调用继续，新调用拒绝。
	mcpLinkDraining
	// mcpLinkClosed：连接已关闭（撤销 / 强制排空到点），一切调用拒绝。
	mcpLinkClosed
)

// mcpLink 是一代 MCP 连接的在途计数与状态闸门。零值不可用，必须经 `newMCPLink`。
// mcpLink counts in-flight calls and gates new ones for one connection generation.
type mcpLink struct {
	server string

	mu       sync.Mutex
	state    mcpLinkState
	reason   string
	inflight int
	// drained 在 inflight 归零时关闭并置 nil；只在持有 mu 时创建。
	// drained closes when inflight reaches zero and is reset to nil under mu.
	drained chan struct{}
}

// newMCPLink 建一代连接的门。
// newMCPLink builds one generation's gate.
func newMCPLink(server string) *mcpLink {
	return &mcpLink{server: server}
}

// enter 登记一次在途调用；非 live 时返回固定短语的 `*core.ExternalError`。
// enter registers one call or refuses it with a fixed-phrase external error.
func (l *mcpLink) enter() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == mcpLinkLive {
		l.inflight++
		return nil
	}
	reason := l.reason
	if reason == "" {
		reason = mcpLinkClosedReason
	}
	return &core.ExternalError{Target: "mcp:" + l.server, Reason: reason}
}

// leave 销号一次在途调用；归零时唤醒排空等待方。
// leave releases one call and wakes the drain waiter at zero.
func (l *mcpLink) leave() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight > 0 {
		l.inflight--
	}
	if l.inflight == 0 && l.drained != nil {
		close(l.drained)
		l.drained = nil
	}
}

// beginDrain 把门切到排空态并返回"在途归零"信号：原本 live 时切态，已经是排空 / 关闭时
// 保持原状态（幂等）。返回值在 inflight 为 0 时已关闭。
// beginDrain switches a live gate to draining and returns the idle signal (idempotent).
func (l *mcpLink) beginDrain(reason string) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == mcpLinkLive {
		l.state = mcpLinkDraining
		l.reason = reason
	}
	if l.inflight == 0 {
		idle := make(chan struct{})
		close(idle)
		return idle
	}
	if l.drained == nil {
		l.drained = make(chan struct{})
	}
	return l.drained
}

// seal 把门永久关死（撤销 / 强制排空到点）：之后的调用一律拒绝，在途调用自然返回
// （连接关闭会让它们收到读取错误）。
// seal closes the gate for good; in-flight calls simply observe the closed connection.
func (l *mcpLink) seal(reason string) {
	l.mu.Lock()
	l.state = mcpLinkClosed
	l.reason = reason
	l.mu.Unlock()
}

// mcpLinkTool 是一代连接上的一件工具：名字 / 描述 / schema 原样透传，执行前后过门。
// mcpLinkTool wraps one adapter of a connection generation behind its gate.
type mcpLinkTool struct {
	link *mcpLink
	tool core.Tool
}

// Name 透传工具名。
// Name forwards the adapter's name.
func (t *mcpLinkTool) Name() string { return t.tool.Name() }

// Description 透传描述。
// Description forwards the adapter's description.
func (t *mcpLinkTool) Description() string { return t.tool.Description() }

// InputSchema 透传参数 schema（原样，不重序列化）。
// InputSchema forwards the adapter's schema verbatim.
func (t *mcpLinkTool) InputSchema() json.RawMessage { return t.tool.InputSchema() }

// Execute 过门后转调：排空 / 关闭态拒绝**在调用实际发出之前**，返回固定短语错误。
// Execute refuses before the call leaves the process once the gate is not live.
func (t *mcpLinkTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	if err := t.link.enter(); err != nil {
		return core.Result{}, err
	}
	defer t.link.leave()
	return t.tool.Execute(ctx, env, input)
}

// mcpWrapConnections 把一次连接结果里的工具全部套上该代的门；连接结果随后交给装配层
// 注册与投影，`Tools` 里的实例即准入钉住的同一代对象。
// mcpWrapConnections wraps every connected server's tools behind its generation gate.
func mcpWrapConnections(link *mcpLink, conns []tool.MCPConnection) []tool.MCPConnection {
	if conns == nil {
		return nil
	}
	wrapped := make([]tool.MCPConnection, 0, len(conns))
	for _, conn := range conns {
		tools := make([]core.Tool, 0, len(conn.Tools))
		for _, adapter := range conn.Tools {
			tools = append(tools, &mcpLinkTool{link: link, tool: adapter})
		}
		wrapped = append(wrapped, tool.MCPConnection{Server: conn.Server, Tools: tools})
	}
	return wrapped
}
