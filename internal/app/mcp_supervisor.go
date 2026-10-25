package app

// 本文件是 MCP supervisor（US-12，约束 19–23）：stdio / http 两种传输的连接生命周期
// 唯一归属地。连接前按传输分支过 `core.Policy`（stdio = 命令授权，http = 出网授权），
// 连接成功才把 applied 版本写进 `ResourceGate` 写锁内的提交段；`CommandRunner` 只执行
// 单次命令，**不得**被当作长驻连接的持有者。
// This file owns the MCP connection lifecycle (US-12): policy per transport before any
// process or socket, and `applied` written only after a successful connect, inside the
// ResourceGate write lock.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

const (
	// defaultMCPTestTimeout 是 `Test` 的固定预算（15 秒，约束 22；代码常量、零新增配置键）。
	// deps.TestTimeout 只在用例里缩短等待，生产装配不传（零值取本常量）。
	// The fixed 15s test budget; the deps override exists for tests only.
	defaultMCPTestTimeout = 15 * time.Second

	// 测试结果的固定 error_code 字面量：策略拒绝按卡片 D-13 的 `policy_denied`；连接失败
	// 与超时分开，供上层把超时映射为 504（复用 resourceResultTimeout）。
	// Fixed test result codes: policy denial, connect failure, and timeout.
	mcpResultPolicyDenied  = "policy_denied"
	mcpResultConnectFailed = "connect_failed"

	// 状态里的固定短语（约束 23：不放命令原文 / URL 原文 / env 值）。
	// Fixed status phrases; raw commands, URLs and env values never appear.
	mcpStatusPolicyDenied  = "策略拒绝"
	mcpStatusConnectFailed = "连接失败"
	mcpStatusApplyFailed   = "记录应用版本失败"
	// mcpStatusAllowlistRevoked 是收紧域名白名单后对已建立 http 连接的撤销短语
	// （R8.2.3 第 2 条）：连接被关闭、工具名清空，已应用版本保留。
	// The revocation phrase used when the allowlist is tightened under a live http link.
	mcpStatusAllowlistRevoked = "域名白名单已收紧"

	// mcpTransportHTTP 是 http 传输的解析字面量（stdio 复用 resources.go 的同名字面量）。
	// The http transport literal; stdio reuses the constant declared in resources.go.
	mcpTransportHTTP = "http"

	// 测试记录的 stage / failure_class 里由**装配层**判定的固定字面量（契约 D-05）：
	// 连接池只产出 spawn / initialize / tools_list，`complete` 与 `preflight` 之外的
	// `env_missing` 分别由本文件与 env 闸门判定。分类只来自结构化失败或本文件的确定
	// 分支，**不**解析 stderr / 日志文案（C-06）。
	// Fixed stage/class literals the assembly layer decides; never parsed from output.
	mcpTestStageComplete = "complete"
	mcpTestStageSpawn    = "spawn"
	// mcpTestClassTimeout 来自预算到点（含父 ctx 取消折成的超时）；连接池把单 server 的
	// 预算超时吸收成 `class=timeout` 的失败项时同样落这里。
	mcpTestClassTimeout = "timeout"
	// mcpTestClassDependencyUnconfirmed 是"失败但无法归因"的唯一出口：不得把"没有成功
	// 连接"推断成"未安装"。
	mcpTestClassDependencyUnconfirmed = "dependency_unconfirmed"
)

// MCPAuditEvent 是一条 MCP 授权 / 连接审计事件（固定键，结构化日志同形）。它不是
// `management_operations` 行：`run-and-errors.md` §9.3 的动作字面量是闭集、且禁止把
// 管理审计与调用审计混写，故按结构化事件落日志与可选回调。
// MCPAuditEvent is one structured authorization/connection event; it is deliberately not
// a management_operations row (the action literal set is closed).
type MCPAuditEvent struct {
	Event     string // `mcp.start` / `mcp.test`
	Server    string
	Transport string
	VersionID int64
	Decision  string // `allow` / `deny`
	Reason    string
	At        int64 // Unix 毫秒
}

// MCPServerStatus 是一个 server 的 supervisor 侧状态：`AppliedVersionID` 只在连接成功
// 并接管后推进；`Connected` 描述**当前**是否有连接（旧版本连接也算连接）。
// MCPServerStatus is the supervisor-side view; applied moves only after a successful
// takeover and Connected describes the live connection, old version included.
type MCPServerStatus struct {
	Name             string
	Transport        string
	Connected        bool
	Tools            []string
	AppliedVersionID int64
	Error            string
}

// MCPSupervisorDeps 是 supervisor 的装配参数。`NewConnector` / `MarkApplied` /
// `Gate` / `TestTimeout` 是测试与装配的接缝；生产装配由 wire.go 提供真实实现。
// `OnConnected` 是连接成功后的通知（装配层用它把工具适配器放进注册池与视图）；
// `Test` 的一次性连接**不**触发它。
// MCPSupervisorDeps wires the supervisor; NewConnector, MarkApplied, Gate and
// TestTimeout are the assembly/test seams. OnConnected publishes the connected tool
// adapters to the assembly layer; one-off Test connections never trigger it.
type MCPSupervisorDeps struct {
	Policy         core.Policy
	Gate           *core.ResourceGate
	Logger         *slog.Logger
	Now            func() time.Time
	Audit          func(MCPAuditEvent)
	MarkApplied    func(ctx context.Context, name string, versionID int64) error
	OnConnected    func(conns []tool.MCPConnection)
	NewConnector   func() mcpConnector
	TestTimeout    time.Duration
	Root           string
	Version        string
	EnvPassthrough []string
}

// MCPSupervisor 持有每个 server 的连接与状态。锁分两层：`mu` 只护 map，`entry.mu`
// 负责该 server 的启停串行——**不得**在持有 `entry.mu` 时等待 `ResourceGate`（约束 42）。
// The supervisor keeps one entry per server; entry.mu serializes start/stop and is never
// held while waiting for the gate.
type MCPSupervisor struct {
	deps MCPSupervisorDeps

	mu      sync.Mutex
	servers map[string]*mcpServerEntry
}

// mcpServerEntry 是一个 server 的可变状态。`epoch` 每次 Start / Stop 递增：连接可能在
// 锁外（等待 ResourceGate）期间被 Stop 或新的 Start 顶替，epoch 让旧连接放弃写 applied。
// mcpServerEntry holds one server's state; epoch invalidates a connect superseded while
// waiting for the gate.
type mcpServerEntry struct {
	mu     sync.Mutex
	epoch  uint64
	conn   mcpConnector
	status MCPServerStatus
	// httpHost 是已建立 http 连接的目标主机（小写；stdio 为空）。撤销判定只看它：
	// `status` 里没有 URL（约束 23 的脱敏面），而白名单收紧后必须能按 host 重新判定。
	// httpHost is the connected http host, kept solely for re-judging after the allowlist
	// is tightened. The status view never carries it.
	httpHost string
}

// NewMCPSupervisor 装配 supervisor：缺省 logger 丢弃、缺省时钟取系统时间。
// NewMCPSupervisor wires the supervisor with a discard logger and the wall clock.
func NewMCPSupervisor(deps MCPSupervisorDeps) *MCPSupervisor {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &MCPSupervisor{deps: deps, servers: make(map[string]*mcpServerEntry)}
}

// ApplyPublished 按配置顺序逐个启动已发布 server；单个失败（策略拒绝 / 连接失败）只
// 记录并继续——**只有**整体取消 / 截止才作为返回错误（与连接池的失败吸收同口径）。
// ApplyPublished starts each published server in order, absorbing per-server failures;
// only outer cancellation/deadline is an error.
func (s *MCPSupervisor) ApplyPublished(ctx context.Context, servers []core.PublishedMCPServer) error {
	for _, published := range servers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.Start(ctx, published); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			s.deps.Logger.Error("MCP server 启动失败——继续其余 server",
				"server", published.Config.Name, "detail", mcpErrorSummary(err))
		}
	}
	return nil
}

// Start 让一个已发布版本生效：① 按传输分支过策略与审计（拒绝即不连接）；② 关闭旧连接后
// 建立新连接并发现工具；③ 释放 supervisor 锁，在 `ResourceGate` 写锁内确认未被顶替后写
// `applied`（约束 17 / 21 / 42）。连接失败 → 保留旧 applied、connected=false。
// Start authorizes, connects, and only then records applied under the gate.
func (s *MCPSupervisor) Start(ctx context.Context, published core.PublishedMCPServer) error {
	cfg := published.Config
	if err := validateSupervisorConfig(cfg); err != nil {
		return err
	}
	entry := s.entry(cfg.Name)

	entry.mu.Lock()
	entry.epoch++
	epoch := entry.epoch
	if err := s.authorize(ctx, "mcp.start", cfg, published.VersionID); err != nil {
		// 拒绝不碰旧连接：已连接旧版本时仍可声明该 server（注 13 真值表第二行）。
		if entry.conn == nil {
			entry.status = MCPServerStatus{
				Name: cfg.Name, Transport: mcpTransport(cfg.Transport),
				Connected: false, AppliedVersionID: entry.status.AppliedVersionID,
				Error: mcpStatusPolicyDenied,
			}
		} else {
			entry.status.Error = mcpStatusPolicyDenied
		}
		entry.mu.Unlock()
		return fmt.Errorf("%w: %w", core.ErrPolicyDenied, err)
	}

	// 重启语义：旧连接先收（仍在启动 / 连接期，锁是单独持有的，不涉及 gate）。
	if entry.conn != nil {
		if err := entry.conn.Close(); err != nil {
			s.deps.Logger.Error("关闭旧 MCP 连接失败", "server", cfg.Name, "detail", mcpErrorSummary(err))
		}
		entry.conn = nil
	}

	connector := s.newConnector()
	conns, err := connector.Connect(ctx, []core.MCPServerConfig{cfg})
	if err == nil && len(conns) == 0 {
		// 连接池把单 server 失败吸收为"零连接"，这里必须显式转成失败。
		err = errors.New(mcpStatusConnectFailed)
	}
	if err != nil {
		closeErr := connector.Close()
		if closeErr != nil {
			s.deps.Logger.Error("清理失败的 MCP 连接失败", "server", cfg.Name, "detail", mcpErrorSummary(closeErr))
		}
		entry.status = MCPServerStatus{
			Name: cfg.Name, Transport: mcpTransport(cfg.Transport),
			Connected: false, AppliedVersionID: entry.status.AppliedVersionID,
			Error: mcpStatusConnectFailed,
		}
		entry.mu.Unlock()
		s.deps.Logger.Error("MCP server 连接失败", "server", cfg.Name,
			"transport", mcpTransport(cfg.Transport), "detail", mcpErrorSummary(err))
		return err
	}

	entry.conn = connector
	// 只记 http 目标主机供白名单收紧后重新判定（R8.2.3）；stdio 留空。
	// Only the http host is remembered for later revocation; stdio leaves it empty.
	entry.httpHost = ""
	if mcpTransport(cfg.Transport) == mcpTransportHTTP {
		entry.httpHost = mcpHost(cfg.URL)
	}
	entry.status = MCPServerStatus{
		Name: cfg.Name, Transport: mcpTransport(cfg.Transport),
		Connected: true, Tools: toolNamesOf(conns),
		AppliedVersionID: entry.status.AppliedVersionID,
	}
	toolCount := len(entry.status.Tools)
	entry.mu.Unlock()

	// 先落 applied、**再**发布工具适配器：记录失败时连接必须失败关闭（connected=false、
	// 工具撤回），否则一组未记为已应用的工具会留在注册池里被 Agent 引用，违背约束 21
	// 的"applied = 已连接且接管"真值。被顶替（epoch 已变）同样不得发布工具——那条连接
	// 已不属于本次启动。
	// Applied is recorded before tools are published; a failed record fails the
	// connection closed, and a superseded connect never publishes tools.
	applied, applyErr := s.markApplied(ctx, entry, epoch, published)
	if applyErr != nil {
		s.abandonApply(entry, epoch, connector, cfg)
		return applyErr
	}
	if !applied {
		// 被顶替时连接已由顶替者关闭：Start / Stop 都在 entry.mu 内递增 epoch 并关闭
		// `entry.conn`，因此 epoch 变化必然意味着本次连接已被收走，这里不重复关闭。
		// The superseder already closed this connector while holding entry.mu.
		return nil
	}
	if s.deps.OnConnected != nil {
		s.deps.OnConnected(conns)
	}
	s.deps.Logger.Info("MCP server 已连接", "server", cfg.Name,
		"transport", mcpTransport(cfg.Transport), "version_id", published.VersionID,
		"tools", toolCount)
	return nil
}

// abandonApply 处理"连接成功但 applied 落库失败"：失效关闭本次连接、把状态退回未连接、
// 清空工具名并保留旧 applied 与固定错误短语。epoch 已变（期间被新 Start / Stop 顶替）时
// 不再改状态——当前槽位已归属新的启动，只做幂等的连接关闭。
// abandonApply fails the connection closed when applied cannot be recorded, keeping
// the previous applied version.
func (s *MCPSupervisor) abandonApply(entry *mcpServerEntry, epoch uint64,
	connector mcpConnector, cfg core.MCPServerConfig,
) {
	entry.mu.Lock()
	stillOurs := entry.epoch == epoch
	if stillOurs {
		entry.conn = nil
		entry.status.Connected = false
		entry.status.Tools = nil
		entry.status.Error = mcpStatusApplyFailed
	}
	entry.mu.Unlock()
	if stillOurs {
		if closeErr := connector.Close(); closeErr != nil {
			s.deps.Logger.Error("关闭 applied 落库失败的 MCP 连接失败", "server", cfg.Name,
				"detail", mcpErrorSummary(closeErr))
		}
	}
	s.deps.Logger.Error("MCP server applied 落库失败——已断开连接并保留旧版本",
		"server", cfg.Name, "transport", mcpTransport(cfg.Transport))
}

// Stop 关闭一个 server 的连接（幂等：没有连接时直接成功）。删除路径在 `ResourceGate`
// 写锁内调用它（gate → supervisor 是允许的锁序，约束 42）；**不**回写 applied。
// Stop closes a server's connection; repeated calls succeed and applied is untouched.
func (s *MCPSupervisor) Stop(ctx context.Context, name string) error {
	_ = ctx // 契约签名统一携带 ctx；关闭本身没有可取消的远端等待
	entry, ok := s.lookup(name)
	if !ok {
		return nil
	}
	entry.mu.Lock()
	entry.epoch++
	conn := entry.conn
	entry.conn = nil
	entry.httpHost = ""
	entry.status.Connected = false
	entry.status.Tools = nil
	entry.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

// RevokeDisallowedHTTP 在域名白名单收紧后撤销不再被允许的 http 连接：关闭连接、
// `connected=false`、清空工具名，**保留** `applied`（版本事实不变）。`stillAllowed`
// 按 host 回答"新规则下还允许吗"。返回被撤销的 server 名（供日志与页面提示）。
// 放宽白名单不会自动重连——重新连接要等重启或重新发布（R8.2.3 第 2 条，如实边界）。
// RevokeDisallowedHTTP closes http links whose host the tightened allowlist no longer
// permits. Applied versions stay; loosening never reconnects automatically.
func (s *MCPSupervisor) RevokeDisallowedHTTP(stillAllowed func(host string) bool) []string {
	s.mu.Lock()
	entries := make([]*mcpServerEntry, 0, len(s.servers))
	for _, entry := range s.servers {
		entries = append(entries, entry)
	}
	s.mu.Unlock()

	revoked := []string{}
	for _, entry := range entries {
		entry.mu.Lock()
		// 只处理"当前确有 http 连接"的槽位；stdio 与未连接的槽位不碰。
		// Only live http links are considered; stdio and idle slots are untouched.
		if entry.conn == nil || entry.status.Transport != mcpTransportHTTP ||
			entry.httpHost == "" || stillAllowed(entry.httpHost) {
			entry.mu.Unlock()
			continue
		}
		entry.epoch++
		conn := entry.conn
		name := entry.status.Name
		host := entry.httpHost
		entry.conn = nil
		entry.httpHost = ""
		entry.status.Connected = false
		entry.status.Tools = nil
		entry.status.Error = mcpStatusAllowlistRevoked
		entry.mu.Unlock()

		if closeErr := conn.Close(); closeErr != nil {
			// 关闭失败只记 ERROR：策略已持久化，数据库是权威（R8.2.4 第 7 条）。
			s.deps.Logger.Error("failed to close revoked MCP http link; policy is already in effect",
				"server", name, "host", host, "detail", mcpErrorSummary(closeErr))
		}
		s.deps.Logger.Info("network domain allowlist tightened; revoked MCP http link",
			"server", name, "host", host)
		revoked = append(revoked, name)
	}
	return revoked
}

// mcpHost 取出 URL 的主机名（小写）；解析失败或没有主机时给空串（stdio 与坏 URL 都
// 走"不可判定"分支，不会被撤销逻辑误判为命中）。
// mcpHost returns the lower-cased hostname, or "" when it cannot be determined.
func mcpHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// Close 关闭全部 server 连接（幂等）：停机路径在数据库关闭之前调用它，避免每 server
// 的连接池把子进程留到进程退出。它不改 applied——版本事实仍以数据库为准。
// Close stops every server connection; it never rewrites applied.
func (s *MCPSupervisor) Close() error {
	s.mu.Lock()
	names := make([]string, 0, len(s.servers))
	for name := range s.servers {
		names = append(names, name)
	}
	s.mu.Unlock()

	var errs []error
	for _, name := range names {
		if err := s.Stop(context.Background(), name); err != nil {
			errs = append(errs, fmt.Errorf("关闭 MCP server %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// mcpDetailedConnector 是连接池的结构化失败能力（D-05 / C-06）：实现它时按 server 逐个
// 拿到 `stage` / `class`；只有 `Connect` 的旧实现（含测试假连接器）走回退分支。`Connect`
// 的启动语义保持不变——启动路径不因这条测试接缝改变。
// mcpDetailedConnector is the structured-failure capability; connectors that only
// implement Connect keep working through the fallback.
type mcpDetailedConnector interface {
	ConnectDetailed(ctx context.Context, specs []core.MCPServerConfig) ([]tool.MCPConnection, []tool.MCPConnectFailure, error)
}

// connectDetailed 走结构化失败通道；连接器没有该能力时退回 `Connect`，并把"零连接但
// 没有错误"标成 `dependency_unconfirmed`——那是连接池吸收了单 server 失败的结果，
// 没有结构化信息就不猜分类（C-06：不得把"没有成功连接"统一推断为"未安装"）。
// connectDetailed prefers ConnectDetailed and falls back to Connect with an
// unconfirmed classification when the connector cannot report structured failures.
func connectDetailed(ctx context.Context, connector mcpConnector, specs []core.MCPServerConfig,
) ([]tool.MCPConnection, []tool.MCPConnectFailure, error) {
	if detailed, ok := connector.(mcpDetailedConnector); ok {
		return detailed.ConnectDetailed(ctx, specs)
	}
	conns, err := connector.Connect(ctx, specs)
	if err == nil && len(conns) == 0 {
		return conns, []tool.MCPConnectFailure{{
			Server: specs[0].Name,
			Stage:  mcpTestStageSpawn,
			Class:  mcpTestClassDependencyUnconfirmed,
		}}, nil
	}
	return conns, nil, err
}

// Test 按**给定配置**（调用方取已发布版本）建立一次性连接：先过策略与审计，再在固定
// 15 秒预算内连接并发现工具；结束（成功 / 失败 / 超时）一律关闭连接，stdio 的进程树由
// 连接池在 Close 内清理。**不**创建 Run、**不**写 `llm_calls`、**不**改变 applied 或状态。
// 结论带 `stage` / `failure_class`（D-05）：分类只来自结构化失败与本文件的确定分支。
// Test opens a one-off connection under the fixed budget, never touching applied or
// creating a run; the verdict carries the D-05 stage and failure class.
func (s *MCPSupervisor) Test(ctx context.Context, cfg core.MCPServerConfig) core.ResourceTestResult {
	if err := validateSupervisorConfig(cfg); err != nil {
		return core.ResourceTestResult{
			OK: false, ErrorCode: mcpResultConnectFailed, ErrorMessage: mcpStatusConnectFailed,
			Stage: mcpTestStageSpawn, FailureClass: mcpTestClassDependencyUnconfirmed,
		}
	}
	if err := s.authorize(ctx, "mcp.test", cfg, 0); err != nil {
		return core.ResourceTestResult{
			OK: false, ErrorCode: mcpResultPolicyDenied, ErrorMessage: mcpStatusPolicyDenied,
			Stage: mcpGateStagePreflight, FailureClass: mcpResultPolicyDenied,
		}
	}

	timeout := s.deps.TestTimeout
	if timeout <= 0 {
		timeout = defaultMCPTestTimeout
	}
	connector := s.newConnector()
	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	conns, failures, err := connectDetailed(testCtx, connector, []core.MCPServerConfig{cfg})
	latency := time.Since(started).Milliseconds()
	// 收尾与分支无关：一次性连接不留给任何人（含超时 / 失败）。
	if closeErr := connector.Close(); closeErr != nil {
		s.deps.Logger.Warn("MCP 测试连接收尾失败", "server", cfg.Name, "detail", mcpErrorSummary(closeErr))
	}

	if err != nil || len(conns) == 0 {
		stage, class := mcpTestStageSpawn, mcpTestClassDependencyUnconfirmed
		if len(failures) > 0 {
			if failures[0].Stage != "" {
				stage = failures[0].Stage
			}
			if failures[0].Class != "" {
				class = failures[0].Class
			}
		}
		// 超时的判定要同时看两处：整体预算到点（err / testCtx），以及连接池把单 server
		// 预算超时吸收成 `class=timeout` 的失败项（此时 err 仍为 nil）。
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			testCtx.Err() != nil || class == mcpTestClassTimeout {
			return core.ResourceTestResult{
				OK: false, LatencyMS: latency,
				ErrorCode: resourceResultTimeout, ErrorMessage: "测试超时",
				Stage: stage, FailureClass: mcpTestClassTimeout,
			}
		}
		return core.ResourceTestResult{
			OK: false, LatencyMS: latency,
			ErrorCode: mcpResultConnectFailed, ErrorMessage: mcpStatusConnectFailed,
			Stage: stage, FailureClass: class,
		}
	}
	// `tools/list` 成功但为空是合法读数（ok=true、tools=[]）：不得改写成 tools_list_failed。
	return core.ResourceTestResult{
		OK: true, LatencyMS: latency, Tools: toolNamesOf(conns),
		Stage: mcpTestStageComplete,
	}
}

// Status 返回一次性拷贝；工具名切片同样复制，调用方改动不影响内部状态。
// Status returns a copy, including the tool-name slice.
func (s *MCPSupervisor) Status(name string) (MCPServerStatus, bool) {
	entry, ok := s.lookup(name)
	if !ok {
		return MCPServerStatus{}, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return cloneStatus(entry.status), true
}

// Snapshot 返回全部 server 状态的独立拷贝（map 与切片都不共享）。
// Snapshot returns an independent copy of every server status.
func (s *MCPSupervisor) Snapshot() map[string]MCPServerStatus {
	s.mu.Lock()
	entries := make(map[string]*mcpServerEntry, len(s.servers))
	for name, entry := range s.servers {
		entries[name] = entry
	}
	s.mu.Unlock()

	snapshot := make(map[string]MCPServerStatus, len(entries))
	for name, entry := range entries {
		entry.mu.Lock()
		snapshot[name] = cloneStatus(entry.status)
		entry.mu.Unlock()
	}
	return snapshot
}

// markApplied 在 `ResourceGate` 写锁内记录 applied：先取 gate、再取 entry.mu（gate →
// supervisor 是唯一允许的组合，约束 42）。返回值：`true` = 本次启动仍持有该槽位且
// applied 已记录（调用方可以发布工具）；`false` = 已被 Stop / 新 Start 顶替，调用方
// **不得**发布工具；非 nil 错误 = 记录失败，调用方必须失败关闭连接。
// markApplied records applied inside the gate. It reports whether this start still
// owns the slot (only then may tools be published) and any recording failure.
func (s *MCPSupervisor) markApplied(ctx context.Context, entry *mcpServerEntry, epoch uint64, published core.PublishedMCPServer) (bool, error) {
	if s.deps.Gate != nil {
		s.deps.Gate.Lock()
		defer s.deps.Gate.Unlock()
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.epoch != epoch || !entry.status.Connected {
		return false, nil
	}
	if s.deps.MarkApplied != nil {
		// 连接已经建立：记录 applied 的写入不得因调用方取消而丢失（取消只废止"还没做
		// 的事"，不废止"已经做完的事"）。
		if err := s.deps.MarkApplied(context.WithoutCancel(ctx), published.Config.Name, published.VersionID); err != nil {
			entry.status.Error = mcpStatusApplyFailed
			return true, fmt.Errorf("记录 MCP server %s 的应用版本: %w", published.Config.Name, err)
		}
	}
	entry.status.AppliedVersionID = published.VersionID
	entry.status.Error = ""
	return true, nil
}

// authorize 按传输分支过策略并落审计事件：stdio 是命令授权（`SHELL_COMMAND`），http 是
// 出网授权（`ActionHTTPRequest`）——**不得**让 http 借命令通道绕过域名白名单。
// authorize checks the per-transport policy and records one structured audit event.
func (s *MCPSupervisor) authorize(ctx context.Context, event string, cfg core.MCPServerConfig, versionID int64) error {
	transport := mcpTransport(cfg.Transport)
	action := core.Action{Type: core.ActionShellCommand, Target: cfg.Command}
	if transport == mcpTransportHTTP {
		action = core.Action{Type: core.ActionHTTPRequest, Target: cfg.URL}
	}
	var checkErr error
	if s.deps.Policy == nil {
		// 装配缺陷按拒绝处理（fail-closed）：没有策略就没有放行依据。
		checkErr = errors.New("策略未装配")
	} else {
		checkErr = s.deps.Policy.Check(ctx, action)
	}
	audit := MCPAuditEvent{
		Event: event, Server: cfg.Name, Transport: transport, VersionID: versionID,
		Decision: "allow", Reason: "allowed", At: s.deps.Now().UnixMilli(),
	}
	if checkErr != nil {
		audit.Decision = "deny"
		audit.Reason = mcpResultPolicyDenied
	}
	s.deps.Logger.Info("MCP 授权审计", "event", audit.Event, "server", audit.Server,
		"transport", audit.Transport, "version_id", audit.VersionID,
		"decision", audit.Decision, "reason", audit.Reason, "at", audit.At)
	if s.deps.Audit != nil {
		s.deps.Audit(audit)
	}
	return checkErr
}

// entry 取或建一个 server 状态槽；map 锁只在这里与 Snapshot 使用。
// entry returns the state slot for a server, creating it on first use.
func (s *MCPSupervisor) entry(name string) *mcpServerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.servers[name]; ok {
		return existing
	}
	entry := &mcpServerEntry{status: MCPServerStatus{Name: name, Transport: mcpTransportStdio}}
	s.servers[name] = entry
	return entry
}

// lookup 只读地取状态槽。
// lookup returns the state slot without creating one.
func (s *MCPSupervisor) lookup(name string) (*mcpServerEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.servers[name]
	return entry, ok
}

// newConnector 给出该次连接使用的连接器：生产为每 server 一个独立连接池（supervisor
// 自己持有生命周期），测试用注入的替身。
// newConnector returns a fresh connector; production is one pool per server.
func (s *MCPSupervisor) newConnector() mcpConnector {
	if s.deps.NewConnector != nil {
		return s.deps.NewConnector()
	}
	return tool.NewMCPPool(tool.MCPPoolOptions{
		Logger:         s.deps.Logger,
		EnvPassthrough: slices.Clone(s.deps.EnvPassthrough),
		Version:        s.deps.Version,
		Root:           s.deps.Root,
	})
}

// validateSupervisorConfig 是 supervisor 侧的 fail-closed 兜底：候选校验在解析层已完成，
// 这里只拒绝"没有足够信息决定传输"的形态（名字 / 传输 / 必需字段）。
// validateSupervisorConfig is the supervisor's fail-closed backstop for malformed input.
func validateSupervisorConfig(cfg core.MCPServerConfig) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return errors.New("MCP server 名为空")
	}
	switch mcpTransport(cfg.Transport) {
	case mcpTransportStdio:
		if strings.TrimSpace(cfg.Command) == "" {
			return errors.New("MCP stdio 缺少 command")
		}
	case mcpTransportHTTP:
		if strings.TrimSpace(cfg.URL) == "" {
			return errors.New("MCP http 缺少 url")
		}
	default:
		return fmt.Errorf("MCP server %s 的传输类型不支持", cfg.Name)
	}
	return nil
}

// toolNamesOf 按连接顺序收集工具名。
// toolNamesOf lists connected tool names in connection order.
func toolNamesOf(conns []tool.MCPConnection) []string {
	names := make([]string, 0, len(conns))
	for _, conn := range conns {
		for _, mcpTool := range conn.Tools {
			names = append(names, mcpTool.Name())
		}
	}
	return names
}

// cloneStatus 复制状态与工具名切片。
// cloneStatus copies a status value including its tool-name slice.
func cloneStatus(status MCPServerStatus) MCPServerStatus {
	clone := status
	clone.Tools = slices.Clone(status.Tools)
	return clone
}

// mcpErrorSummary 把 supervisor 侧错误收敛到固定短语：原始错误可能含路径 / SQL 细节，
// 只留"拒绝 / 取消 / 超时 / 其他"四档（约束 23 的日志脱敏面）。
// mcpErrorSummary reduces errors to fixed phrases for logs and statuses.
func mcpErrorSummary(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, core.ErrPolicyDenied) || core.IsSandboxViolation(err):
		return mcpStatusPolicyDenied
	case errors.Is(err, context.Canceled):
		return "已取消"
	case errors.Is(err, context.DeadlineExceeded):
		return "超时"
	}
	return mcpStatusConnectFailed
}
