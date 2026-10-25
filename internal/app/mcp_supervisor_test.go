package app

// 本文件是 MCP supervisor 的验收用例（颗粒度文档 §3.2 的 app/mcp_supervisor_test.go 十七例）。
// 判别力来源：策略在**连接器创建之前**判定（被拒时连接器计数为 0）、applied 只在连接
// 成功后推进、http 分支零进程、测试端点不建运行也不换 applied。
// Acceptance tests for the MCP supervisor: policy precedes connector creation, applied
// only follows a successful connect, http never owns a process, and Test never touches
// applied.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// mcpTestEnv 是 supervisor 用例的共享夹具：白名单策略（`allowed-mcp` 命令 +
// `mcp.example.com` 域名）、记录型审计回调、内存 applied 表与可注入的假连接器。
// mcpTestEnv is the shared fixture: a whitelist policy, recording audit, in-memory
// applied table, and injectable fake connectors.
type mcpTestEnv struct {
	supervisor *MCPSupervisor
	gate       *core.ResourceGate

	mu             sync.Mutex
	processStarted bool
	connectors     []*supervisorFakeConnector
	created        int
	closed         int
	applied        map[string]int64
	events         []string
	audits         []MCPAuditEvent
	connected      []string
	connectErr     error
	closeErr       error
	applyErr       error
	blockConnect   bool
	toolNames      []string
	lastSpecs      []core.MCPServerConfig

	// US-14：结构化失败（D-05）与"没有 ConnectDetailed 的实现"两个注入位。
	// US-14 injection points: one structured failure and a connector without
	// ConnectDetailed (the fallback branch).
	failStage       string
	failClass       string
	failDeadline    bool
	legacyConnector bool
}

// newMCPEnv 装配默认夹具：只有 `allowed-mcp` 命令与 `mcp.example.com` 域名被放行。
// newMCPEnv wires the default fixture; only allowed-mcp and mcp.example.com pass policy.
func newMCPEnv(t *testing.T) *mcpTestEnv {
	t.Helper()
	policy := tool.NewWhitelistPolicy(tool.PolicyOptions{
		Root:            t.TempDir(),
		AllowedCommands: []string{"allowed-mcp"},
		AllowedDomains:  []string{"mcp.example.com"},
	})
	return newMCPEnvWithPolicy(t, policy)
}

// newMCPEnvWithPolicy 允许用例替换策略实现（HTTP 动作判定的记录型策略）。
// newMCPEnvWithPolicy lets a case replace the policy implementation.
func newMCPEnvWithPolicy(t *testing.T, policy core.Policy) *mcpTestEnv {
	t.Helper()
	env := &mcpTestEnv{
		gate:    &core.ResourceGate{},
		applied: make(map[string]int64),
	}
	env.supervisor = NewMCPSupervisor(MCPSupervisorDeps{
		Policy: policy,
		Gate:   env.gate,
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return time.UnixMilli(1_700_000_000_000) },
		Audit: func(event MCPAuditEvent) {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.audits = append(env.audits, event)
		},
		MarkApplied: func(_ context.Context, name string, versionID int64) error {
			env.mu.Lock()
			defer env.mu.Unlock()
			if env.applyErr != nil {
				env.events = append(env.events, fmt.Sprintf("apply-failed:%s:%d", name, versionID))
				return env.applyErr
			}
			env.applied[name] = versionID
			env.events = append(env.events, fmt.Sprintf("apply:%s:%d", name, versionID))
			return nil
		},
		OnConnected: func(conns []tool.MCPConnection) {
			env.mu.Lock()
			defer env.mu.Unlock()
			for _, conn := range conns {
				env.connected = append(env.connected, conn.Server)
				env.events = append(env.events, "connected:"+conn.Server)
			}
		},
		NewConnector: env.newConnector,
		TestTimeout:  5 * time.Second,
	})
	return env
}

// newConnector 为每次连接登记一个新的假连接器（形如"每 server 一个连接池"）。
// newConnector registers a fresh fake connector per call, mirroring one pool per server.
func (env *mcpTestEnv) newConnector() mcpConnector {
	env.mu.Lock()
	defer env.mu.Unlock()
	fake := &supervisorFakeConnector{env: env}
	env.connectors = append(env.connectors, fake)
	env.created++
	if env.legacyConnector {
		// 只满足 `mcpConnector`：用来判"没有 ConnectDetailed 时的回退分类"。
		// Satisfies only mcpConnector, exercising the Connect fallback.
		return legacyMCPConnector{fake: fake}
	}
	return fake
}

// setConnectFailure 让假连接器的 `ConnectDetailed` 报告一条结构化失败（stage × class）。
// setConnectFailure injects one structured connect failure (stage × class).
func (env *mcpTestEnv) setConnectFailure(stage, class string) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.failStage = stage
	env.failClass = class
}

// setConnectFailureDeadline 让结构化失败与 ctx 终止同时发生（超时路径取失败发生的阶段）。
// setConnectFailureDeadline makes the structured failure arrive together with ctx end.
func (env *mcpTestEnv) setConnectFailureDeadline(deadline bool) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.failDeadline = deadline
}

// setLegacyConnector 让 newConnector 返回**没有** `ConnectDetailed` 的实现。
// setLegacyConnector makes newConnector return an implementation without ConnectDetailed.
func (env *mcpTestEnv) setLegacyConnector(legacy bool) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.legacyConnector = legacy
}

// setConnectErr 注入连接失败。
// setConnectErr injects a connect failure.
func (env *mcpTestEnv) setConnectErr(err error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.connectErr = err
}

// setCloseErr 注入关闭失败（删除路径的 Stop 诚实上报）。
// setCloseErr injects a close failure so the delete path sees an honest Stop error.
func (env *mcpTestEnv) setCloseErr(err error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.closeErr = err
}

// setBlockConnect 让连接挂起到 ctx 结束（模拟超时）。
// setBlockConnect makes Connect block until the context ends.
func (env *mcpTestEnv) setBlockConnect(block bool) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.blockConnect = block
}

// setToolNames 覆盖假连接器发现的工具名。
// setToolNames overrides the tool names the fake connector discovers.
func (env *mcpTestEnv) setToolNames(names ...string) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.toolNames = names
}

// setApplyErr 注入 applied 落库失败（约束 21 的失败关闭判据）。
// setApplyErr injects an applied-recording failure.
func (env *mcpTestEnv) setApplyErr(err error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	env.applyErr = err
}

// connectedCount 返回 OnConnected 被调用的 server 次数（工具是否被发布的判据）。
// connectedCount reports how many times OnConnected published a server's tools.
func (env *mcpTestEnv) connectedCount() int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return len(env.connected)
}

// lastConnectConfigs 返回最近一次拨号收到的配置（判 env 占位是否已解析成终值）。
// lastConnectConfigs returns the specs of the most recent dial so env resolution is
// observable.
func (env *mcpTestEnv) lastConnectConfigs() []core.MCPServerConfig {
	env.mu.Lock()
	defer env.mu.Unlock()
	return slices.Clone(env.lastSpecs)
}

// appliedVersion 读取内存 applied 表中某 server 的版本。
// appliedVersion reads the in-memory applied version for a server.
func (env *mcpTestEnv) appliedVersion(name string) (int64, bool) {
	env.mu.Lock()
	defer env.mu.Unlock()
	versionID, ok := env.applied[name]
	return versionID, ok
}

// auditEvents 返回审计事件拷贝。
// auditEvents returns a copy of recorded audit events.
func (env *mcpTestEnv) auditEvents() []MCPAuditEvent {
	env.mu.Lock()
	defer env.mu.Unlock()
	return slices.Clone(env.audits)
}

// createdCount / closedCount 返回连接器创建与关闭计数。
func (env *mcpTestEnv) createdCount() int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.created
}

// closedCount 返回连接器关闭次数。
func (env *mcpTestEnv) closedCount() int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.closed
}

// eventLog 返回事件序拷贝（判别 connect 与 apply 的先后）。
// eventLog returns the event sequence used to judge connect-before-apply.
func (env *mcpTestEnv) eventLog() []string {
	env.mu.Lock()
	defer env.mu.Unlock()
	return slices.Clone(env.events)
}

// startedProcess 返回假连接器是否模拟过 stdio 进程。
// startedProcess reports whether the fake ever simulated a stdio process.
func (env *mcpTestEnv) startedProcess() bool {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.processStarted
}

// supervisorFakeConnector 是 `mcpConnector` 的假实现：stdio 记为"起了进程"、http 不记；
// 可注入失败或挂起到 ctx 结束；Close 记数并置位。
// supervisorFakeConnector simulates a pool: stdio marks a process, http never does;
// failures and blocking can be injected.
type supervisorFakeConnector struct {
	env     *mcpTestEnv
	failed  error
	blocked bool
	closed  bool
}

// Connect 记录调用、模拟进程并按注入条件失败 / 挂起 / 返回连接。
// Connect records the call, simulates a process, and honors injected failure or block.
func (f *supervisorFakeConnector) Connect(ctx context.Context, specs []core.MCPServerConfig) ([]tool.MCPConnection, error) {
	f.env.mu.Lock()
	f.env.events = append(f.env.events, "connect:"+specs[0].Name)
	for _, spec := range specs {
		if mcpTransport(spec.Transport) == mcpTransportStdio {
			f.env.processStarted = true
		}
	}
	connectErr := f.env.connectErr
	block := f.env.blockConnect
	toolNames := slices.Clone(f.env.toolNames)
	f.env.lastSpecs = slices.Clone(specs)
	f.env.mu.Unlock()

	f.failed = connectErr
	f.blocked = block
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if connectErr != nil {
		return nil, connectErr
	}
	conns := make([]tool.MCPConnection, 0, len(specs))
	for _, spec := range specs {
		tools := make([]core.Tool, 0, len(toolNames))
		for _, name := range toolNames {
			tools = append(tools, &fakeMCPTool{name: name})
		}
		conns = append(conns, tool.MCPConnection{Server: spec.Name, Tools: tools})
	}
	return conns, nil
}

// ConnectDetailed 是 `mcpDetailedConnector` 的假实现（US-14，契约 D-05）：注入结构化
// 失败时直接返回那一条（deadline 变体同时以 ctx 终止收尾）；其余情况复用 `Connect` 的
// 同一套模拟，只是没有失败项——真实池在没有可证明的失败时也不编造分类。
// ConnectDetailed is the fake's structured-failure path; without an injection it
// delegates to Connect and reports no failure items.
func (f *supervisorFakeConnector) ConnectDetailed(ctx context.Context, specs []core.MCPServerConfig,
) ([]tool.MCPConnection, []tool.MCPConnectFailure, error) {
	f.env.mu.Lock()
	stage, class, deadline := f.env.failStage, f.env.failClass, f.env.failDeadline
	f.env.mu.Unlock()

	if class != "" {
		failure := tool.MCPConnectFailure{Server: specs[0].Name, Stage: stage, Class: class}
		if deadline {
			<-ctx.Done()
			return nil, []tool.MCPConnectFailure{failure}, ctx.Err()
		}
		f.env.mu.Lock()
		f.env.events = append(f.env.events, "connect:"+specs[0].Name)
		for _, spec := range specs {
			if mcpTransport(spec.Transport) == mcpTransportStdio {
				f.env.processStarted = true
			}
		}
		f.env.lastSpecs = slices.Clone(specs)
		f.env.mu.Unlock()
		return nil, []tool.MCPConnectFailure{failure}, nil
	}
	conns, err := f.Connect(ctx, specs)
	return conns, nil, err
}

// legacyMCPConnector 只实现 `mcpConnector`（刻意**不**暴露 `ConnectDetailed`）：判回退
// 分支用的是"能力断言失败"这一真实条件，而不是给假实现加开关。
// legacyMCPConnector deliberately hides ConnectDetailed so the fallback branch is real.
type legacyMCPConnector struct{ fake *supervisorFakeConnector }

// Connect 转发到内部假连接器。
// Connect delegates to the wrapped fake.
func (l legacyMCPConnector) Connect(ctx context.Context, specs []core.MCPServerConfig) ([]tool.MCPConnection, error) {
	return l.fake.Connect(ctx, specs)
}

// Close 转发到内部假连接器。
// Close delegates to the wrapped fake.
func (l legacyMCPConnector) Close() error { return l.fake.Close() }

// Close 记录关闭次数：supervisor 的停止 / 失败清理 / 测试收尾都以它判卷。
// Close records the call; stop, failure cleanup and test teardown all judge on it.
func (f *supervisorFakeConnector) Close() error {
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	f.closed = true
	f.env.closed++
	f.env.events = append(f.env.events, "close")
	return f.env.closeErr
}

// isClosed 返回该连接器是否被关闭。
// isClosed reports whether this connector was closed.
func (f *supervisorFakeConnector) isClosed() bool {
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	return f.closed
}

// recordingPolicy 记录收到的每个 Action，供"按传输分支"判定（http 不得借命令通道）。
// recordingPolicy records every Action so transport branching is observable.
type recordingPolicy struct {
	mu      sync.Mutex
	actions []core.Action
}

// Check 记录动作后放行。
// Check records the action and allows it.
func (p *recordingPolicy) Check(_ context.Context, action core.Action) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, action)
	return nil
}

// actionsCopy 返回记录到的动作拷贝。
// actionsCopy returns a copy of the recorded actions.
func (p *recordingPolicy) actionsCopy() []core.Action {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.actions)
}

// allowedStdio 构造一个策略可放行的 stdio 版本。
// allowedStdio builds a policy-allowed stdio version.
func allowedStdio(name string, versionID int64) core.PublishedMCPServer {
	return core.PublishedMCPServer{
		VersionID: versionID,
		Config:    core.MCPServerConfig{Name: name, Transport: "stdio", Command: "allowed-mcp"},
	}
}

// TestMCPSupervisor_PolicyDenied 是颗粒度文档 §4.3 的**逐字**回归：策略拒绝时返回
// ErrPolicyDenied、状态未连接、且零进程。逆改点：把 authorize 挪到 Connect 之后 →
// processStarted 变 true 而红。
// Verbatim regression from the module card: denial returns ErrPolicyDenied, leaves the
// server disconnected, and starts no process.
func TestMCPSupervisor_PolicyDenied(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	pub := core.PublishedMCPServer{VersionID: 1, Config: core.MCPServerConfig{Name: "fs", Transport: "stdio", Command: "outside-allowlist"}}
	err := env.supervisor.Start(context.Background(), pub)
	if !errors.Is(err, core.ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
	if status, ok := env.supervisor.Status("fs"); ok && status.Connected {
		t.Fatalf("status = %+v, want not connected", status)
	}
	if env.processStarted {
		t.Fatal("process started despite policy denial")
	}
}

// TestMCPSupervisor_PolicyDeniedDoesNotStart：拒绝发生在连接器创建之前（计数 0），
// 且不写 applied、审计记 deny。
// Denial precedes connector creation: zero connectors, no applied write, deny audited.
func TestMCPSupervisor_PolicyDeniedDoesNotStart(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	pub := core.PublishedMCPServer{VersionID: 3, Config: core.MCPServerConfig{
		Name: "fs", Transport: "stdio", Command: "outside-allowlist",
	}}
	require.ErrorIs(t, env.supervisor.Start(context.Background(), pub), core.ErrPolicyDenied)
	require.Equal(t, 0, env.createdCount(), "被拒的连接不得创建连接器")
	_, applied := env.appliedVersion("fs")
	require.False(t, applied, "被拒的连接不得写 applied")
	audits := env.auditEvents()
	require.Len(t, audits, 1)
	require.Equal(t, "deny", audits[0].Decision)
	require.Equal(t, "mcp.start", audits[0].Event)
}

// TestMCPSupervisor_HTTPPolicyUsesHTTPAction：stdio 判 SHELL_COMMAND（目标是可执行名），
// http 判 HTTP_REQUEST（目标是 URL）——http **不得**借命令通道绕过域名白名单。
// Transport branches the checked action: stdio uses SHELL_COMMAND, http uses
// HTTP_REQUEST with the URL as target.
func TestMCPSupervisor_HTTPPolicyUsesHTTPAction(t *testing.T) {
	t.Parallel()
	policy := &recordingPolicy{}
	env := newMCPEnvWithPolicy(t, policy)

	require.NoError(t, env.supervisor.Start(context.Background(), allowedStdio("local", 1)))
	httpPub := core.PublishedMCPServer{VersionID: 1, Config: core.MCPServerConfig{
		Name: "remote", Transport: "http", URL: "https://mcp.example.com/mcp",
	}}
	require.NoError(t, env.supervisor.Start(context.Background(), httpPub))

	actions := policy.actionsCopy()
	require.Len(t, actions, 2)
	require.Equal(t, core.ActionShellCommand, actions[0].Type)
	require.Equal(t, "allowed-mcp", actions[0].Target)
	require.Equal(t, core.ActionHTTPRequest, actions[1].Type)
	require.Equal(t, "https://mcp.example.com/mcp", actions[1].Target)
}

// TestMCPSupervisor_HTTPRejectsIPLiteralAndNonDefaultPort：真实白名单策略下，IP 字面量与
// 非默认端口一律拒绝且零连接器、零进程；同主机的默认端口放行。
// Under the real whitelist policy, IP literals and non-default ports are refused before
// any connection, while the same host on the default port passes.
func TestMCPSupervisor_HTTPRejectsIPLiteralAndNonDefaultPort(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	ctx := context.Background()

	ipLiteral := core.PublishedMCPServer{VersionID: 1, Config: core.MCPServerConfig{
		Name: "ip", Transport: "http", URL: "http://127.0.0.1:8080/mcp",
	}}
	require.ErrorIs(t, env.supervisor.Start(ctx, ipLiteral), core.ErrPolicyDenied)

	oddPort := core.PublishedMCPServer{VersionID: 1, Config: core.MCPServerConfig{
		Name: "port", Transport: "http", URL: "https://mcp.example.com:3000/mcp",
	}}
	require.ErrorIs(t, env.supervisor.Start(ctx, oddPort), core.ErrPolicyDenied)
	require.Equal(t, 0, env.createdCount(), "被拒的 http 连接不得创建连接器")
	require.False(t, env.startedProcess())

	allowed := core.PublishedMCPServer{VersionID: 1, Config: core.MCPServerConfig{
		Name: "ok", Transport: "http", URL: "https://mcp.example.com/mcp",
	}}
	require.NoError(t, env.supervisor.Start(ctx, allowed))
	status, ok := env.supervisor.Status("ok")
	require.True(t, ok)
	require.True(t, status.Connected)
	require.False(t, env.startedProcess(), "http 分支零进程")
}

// TestMCPSupervisor_IndependentLifecycle：各 server 的连接互不影响——停一个不波及另一个。
// Server lifecycles are independent: stopping one leaves the other connected.
func TestMCPSupervisor_IndependentLifecycle(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	ctx := context.Background()
	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("alpha", 1)))
	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("beta", 1)))

	require.NoError(t, env.supervisor.Stop(ctx, "alpha"))
	alpha, _ := env.supervisor.Status("alpha")
	beta, _ := env.supervisor.Status("beta")
	require.False(t, alpha.Connected)
	require.True(t, beta.Connected, "停 alpha 不得波及 beta")
}

// TestMCPSupervisor_StdioOwnsProcessTree：stdio 启动即模拟起进程，停止时关闭连接器
// （连接池在 Close 内回收整棵进程树，tool 包的用例另测树杀本身）。
// Stdio owns the process; Stop closes the connector that reaps the tree.
func TestMCPSupervisor_StdioOwnsProcessTree(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	ctx := context.Background()
	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("fs", 1)))
	require.True(t, env.startedProcess())
	require.Equal(t, 1, env.createdCount())

	require.NoError(t, env.supervisor.Stop(ctx, "fs"))
	require.Equal(t, 1, env.closedCount(), "停止必须关闭持有进程树的连接器")
	require.True(t, env.connectors[0].isClosed())
}

// TestMCPSupervisor_HTTPNoProcess：http 版本连接时不产生任何本地进程。
// HTTP transports never own a local process.
func TestMCPSupervisor_HTTPNoProcess(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	pub := core.PublishedMCPServer{VersionID: 1, Config: core.MCPServerConfig{
		Name: "remote", Transport: "http", URL: "https://mcp.example.com/mcp",
	}}
	require.NoError(t, env.supervisor.Start(context.Background(), pub))
	require.Equal(t, 1, env.createdCount())
	require.False(t, env.startedProcess(), "http 分支零进程")
	status, _ := env.supervisor.Status("remote")
	require.True(t, status.Connected)
	require.Equal(t, "http", status.Transport)
}

// TestCommandRunner_NotUsedForMCP：supervisor 源码的**代码标识符**里不得出现
// 命令执行器——连接生命周期与单次命令执行分离（FR-018 / 约束 19）。按 AST 判卷而不是
// 比对全文：注释里"不得委托 CommandRunner"正是规范表述，全文匹配会把说明判成违规。
// The supervisor's code identifiers must never reference the command runner; judging
// the AST keeps explanatory comments from tripping a text match.
func TestCommandRunner_NotUsedForMCP(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("mcp_supervisor.go")
	require.NoError(t, err)
	parsed, err := parser.ParseFile(token.NewFileSet(), "mcp_supervisor.go", source, 0)
	require.NoError(t, err)

	identifiers := make(map[string]bool)
	ast.Inspect(parsed, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok {
			identifiers[ident.Name] = true
		}
		return true
	})
	require.False(t, identifiers["CommandRunner"], "MCP 连接生命周期不得委托命令执行器")
	require.False(t, identifiers["LocalRunner"])
}

// TestMCPSupervisor_AppliedOnlyAfterConnect：连接失败时 applied 不动、状态标未连接；
// 成功时 connect 事件先于 apply 事件。
// applied moves only after a successful connect, and connect precedes apply.
func TestMCPSupervisor_AppliedOnlyAfterConnect(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	ctx := context.Background()

	env.setConnectErr(errors.New("boom"))
	require.Error(t, env.supervisor.Start(ctx, allowedStdio("fs", 1)))
	_, applied := env.appliedVersion("fs")
	require.False(t, applied, "连接失败不得写 applied")
	status, ok := env.supervisor.Status("fs")
	require.True(t, ok)
	require.False(t, status.Connected)
	require.Equal(t, mcpStatusConnectFailed, status.Error)

	env.setConnectErr(nil)
	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("fs", 2)))
	versionID, applied := env.appliedVersion("fs")
	require.True(t, applied)
	require.Equal(t, int64(2), versionID)

	events := env.eventLog()
	connectIndex := slices.Index(events, "connect:fs")
	applyIndex := slices.Index(events, "apply:fs:2")
	require.NotEqual(t, -1, connectIndex)
	require.NotEqual(t, -1, applyIndex)
	require.Less(t, connectIndex, applyIndex, "apply 必须晚于 connect")
}

// TestMCPSupervisor_ApplyFailureFailsClosed：连接成功但 applied 落库失败 → Start 返回
// 错误、本次连接被关闭、状态退回未连接且工具清空、旧 applied 保留；`OnConnected` 不得
// 被调用——未记为已应用的工具不得进入注册池（约束 21 的真值）。
// 中文验收点：applied 失败不是"连上就算"，而是失败关闭。
// A failed applied record fails the connection closed and never publishes tools.
func TestMCPSupervisor_ApplyFailureFailsClosed(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	env.setToolNames("echo")
	ctx := context.Background()

	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("fs", 1)))
	require.Equal(t, 1, env.connectedCount(), "第一次成功启动发布一次工具")
	before := env.connectedCount()

	env.setApplyErr(errors.New("db down"))
	err := env.supervisor.Start(ctx, allowedStdio("fs", 2))
	require.Error(t, err, "applied 落库失败必须上报")

	status, ok := env.supervisor.Status("fs")
	require.True(t, ok)
	require.False(t, status.Connected, "applied 失败后不得声明已连接")
	require.Empty(t, status.Tools, "applied 失败后不得保留工具列表")
	require.Equal(t, mcpStatusApplyFailed, status.Error)
	versionID, applied := env.appliedVersion("fs")
	require.True(t, applied)
	require.Equal(t, int64(1), versionID, "旧 applied 必须保留")
	require.Equal(t, before, env.connectedCount(), "applied 失败不得发布工具")
	require.Equal(t, 2, env.closedCount(), "旧连接与失败关闭各关闭一次")
}

// TestMCPSupervisor_MarkAppliedSuperseded：epoch 已变（期间被 Stop / 新 Start 顶替）
// 时 markApplied 返回 false 且**不写** applied——调用方据此不发布工具。
// A superseded start records nothing and reports that it no longer owns the slot.
func TestMCPSupervisor_MarkAppliedSuperseded(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	entry := env.supervisor.entry("fs")
	entry.mu.Lock()
	entry.status.Connected = true
	entry.mu.Unlock()

	applied, err := env.supervisor.markApplied(context.Background(), entry, entry.epoch+1, allowedStdio("fs", 3))
	require.NoError(t, err)
	require.False(t, applied, "epoch 不匹配时不得宣称 applied")
	_, recorded := env.appliedVersion("fs")
	require.False(t, recorded, "被顶替的启动不得写 applied")
}

// TestMCPSupervisor_PendingRestartFlag：新版本被拒时旧连接与旧 applied 保留——待重启
// = 发布版本 ≠ 应用版本（注 13 真值表第二行）。
// A denied upgrade keeps the old connection and applied; pending_restart derives from
// published != applied.
func TestMCPSupervisor_PendingRestartFlag(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	ctx := context.Background()
	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("fs", 1)))

	denied := core.PublishedMCPServer{VersionID: 2, Config: core.MCPServerConfig{
		Name: "fs", Transport: "stdio", Command: "outside-allowlist",
	}}
	require.ErrorIs(t, env.supervisor.Start(ctx, denied), core.ErrPolicyDenied)

	status, ok := env.supervisor.Status("fs")
	require.True(t, ok)
	require.True(t, status.Connected, "旧连接继续服务")
	require.Equal(t, int64(1), status.AppliedVersionID)
	published := int64(2)
	require.True(t, published != status.AppliedVersionID, "pending_restart = published != applied")
}

// TestMCPTest_NoRunNoLLMCall：测试端点不建运行、不写 applied、不改状态。
// Test creates no run, writes no applied and leaves the status map untouched.
func TestMCPTest_NoRunNoLLMCall(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	result := env.supervisor.Test(context.Background(), core.MCPServerConfig{
		Name: "fs", Transport: "stdio", Command: "allowed-mcp",
	})
	require.True(t, result.OK)
	_, applied := env.appliedVersion("fs")
	require.False(t, applied, "测试端点不得写 applied")
	_, exists := env.supervisor.Status("fs")
	require.False(t, exists, "测试端点不得在状态表里留条目")
	audits := env.auditEvents()
	require.Len(t, audits, 1)
	require.Equal(t, "mcp.test", audits[0].Event)
	require.Equal(t, "allow", audits[0].Decision)
}

// TestMCPTest_StdioCleansProcessTree：测试结束一律关闭一次性连接器（stdio 的
// 进程树由连接池在 Close 内回收）。
// Test always closes the one-off connector, which reaps the stdio tree.
func TestMCPTest_StdioCleansProcessTree(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	result := env.supervisor.Test(context.Background(), core.MCPServerConfig{
		Name: "fs", Transport: "stdio", Command: "allowed-mcp",
	})
	require.True(t, result.OK)
	require.Equal(t, 1, env.createdCount())
	require.Equal(t, 1, env.closedCount(), "测试结束必须收尾连接（含进程树）")
}

// TestMCPTest_HTTPNoProcess：http 测试同样零进程，且连接器照常收尾。
// HTTP tests start no process and still close their connector.
func TestMCPTest_HTTPNoProcess(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	result := env.supervisor.Test(context.Background(), core.MCPServerConfig{
		Name: "remote", Transport: "http", URL: "https://mcp.example.com/mcp",
	})
	require.True(t, result.OK)
	require.False(t, env.startedProcess())
	require.Equal(t, 1, env.closedCount())
}

// TestMCPTest_Timeout：握手挂起时按固定预算返回 TIMEOUT，并收尾连接器。
// A hanging handshake yields TIMEOUT under the fixed budget and still closes the
// connector.
func TestMCPTest_Timeout(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	env.supervisor.deps.TestTimeout = 20 * time.Millisecond
	env.setBlockConnect(true)

	result := env.supervisor.Test(context.Background(), core.MCPServerConfig{
		Name: "fs", Transport: "stdio", Command: "allowed-mcp",
	})
	require.False(t, result.OK)
	require.Equal(t, resourceResultTimeout, result.ErrorCode)
	require.Equal(t, 1, env.closedCount(), "超时后也必须收尾连接")
}

// TestMCPSupervisor_TestPolicyDeniedAudited：测试端点的策略拒绝同样先审后拒、零连接器，
// 且结果 code 是卡片固定的 policy_denied。
// Test denial is audited before refusing, creates no connector, and uses policy_denied.
func TestMCPSupervisor_TestPolicyDeniedAudited(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	result := env.supervisor.Test(context.Background(), core.MCPServerConfig{
		Name: "fs", Transport: "stdio", Command: "outside-allowlist",
	})
	require.False(t, result.OK)
	require.Equal(t, mcpResultPolicyDenied, result.ErrorCode)
	require.Equal(t, 0, env.createdCount())
	audits := env.auditEvents()
	require.Len(t, audits, 1)
	require.Equal(t, "deny", audits[0].Decision)
	require.Equal(t, "mcp.test", audits[0].Event)
}

// TestMCPSupervisor_ConcurrentRestart：并发重启同一 server 时最终只有一个连接的赢家——
// 旧连接全部关闭、applied 落在赢家版本上、无死锁（锁序 gate → entry.mu，约束 42）。
// Concurrent restarts serialize per server: all older connectors close, applied lands
// on the winning version, and the gate/supervisor lock order never deadlocks.
func TestMCPSupervisor_ConcurrentRestart(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	const restarts = 8

	var wg sync.WaitGroup
	errs := make(chan error, restarts)
	for i := 1; i <= restarts; i++ {
		wg.Add(1)
		go func(versionID int64) {
			defer wg.Done()
			errs <- env.supervisor.Start(context.Background(), allowedStdio("fs", versionID))
		}(int64(i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	status, ok := env.supervisor.Status("fs")
	require.True(t, ok)
	require.True(t, status.Connected)
	require.GreaterOrEqual(t, status.AppliedVersionID, int64(1))
	require.LessOrEqual(t, status.AppliedVersionID, int64(restarts))
	require.Equal(t, restarts, env.createdCount())
	require.Equal(t, restarts-1, env.closedCount(), "每次重启都关掉上一个连接")
	require.Len(t, env.auditEvents(), restarts)
}

// TestMCPSupervisor_StopIdempotent：重复停止成功且 applied 不变；未启动的 server 停止也是空操作。
// Repeated stops succeed without touching applied; stopping an unknown server is a no-op.
func TestMCPSupervisor_StopIdempotent(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	ctx := context.Background()
	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("fs", 1)))

	require.NoError(t, env.supervisor.Stop(ctx, "fs"))
	require.NoError(t, env.supervisor.Stop(ctx, "fs"))
	require.NoError(t, env.supervisor.Stop(ctx, "never-started"))

	status, _ := env.supervisor.Status("fs")
	require.False(t, status.Connected)
	versionID, applied := env.appliedVersion("fs")
	require.True(t, applied)
	require.Equal(t, int64(1), versionID, "停止不回写 applied")
	require.Equal(t, 1, env.closedCount(), "第二次停止不得重复关闭")
}
