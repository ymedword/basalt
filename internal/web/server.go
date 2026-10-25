package web

// 本文件是 HTTP 面的装配与生命周期（模块卡 §3.1）：Options 是唯一装配输入、端口由
// app 层适配；Server 只持有这些端口与标准库 HTTP 服务——会话准入（忙集、实例上限、
// 归档互斥）的唯一归属是 core.AgentService，web 不另建互斥（约束 40）。停机按五阶段
// 协议（约束 20）：停止接入 → 宽限等待 → 取消并断开 → 有界收尾 → 由调用方释放共享资源。
// This file assembles the HTTP face and owns its lifecycle; runtime dependencies arrive
// only through Options, and session admission belongs to core.AgentService alone.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// Options 是 web 面的装配输入。所有端口由 app 层适配；web 只依赖 core 与标准库。
// Options is the assembly surface; every port is adapted by the app layer.
type Options struct {
	Addr            string        // 空 → defaultAddr
	MaxRequestBytes int64         // ≤0 → defaultMaxRequestBytes
	RunTimeout      time.Duration // 0 → defaultRunTimeout
	RunBudget       core.Budget   // REST / invoke 运行预算（约束 35）
	ShutdownGrace   time.Duration // 0 → defaultShutdownGrace
	Version         string        // /info.version
	MemoryBackend   string        // markdown | sqlite（归一化后）
	MCPConnected    int           // 已连接 MCP server 数（/info.mcp_servers）
	Providers       []ProviderInfo
	Logger          *slog.Logger     // nil → slog.Default()
	Now             func() time.Time // nil → time.Now
	StartedAt       time.Time        // /info uptime 起点；零值 → NewServer 时刻
	Listener        net.Listener     // 测试接缝；nil → Run 内 net.Listen(Addr)

	Runs      RunService
	Sessions  SessionStore
	Agents    AgentProfiles
	Tools     ToolCatalog
	Memory    MemorySections
	Schedules ScheduleService // 可选：nil 时四个 /schedules 端点按 500 返回（装配缺失是错误）

	// 管理面端口（US-11；全部由 app 层适配，web 只依赖 core 与标准库）。
	// AdminAuth 是认证闸门：公开面闭集以外的路由一律经它取得 actor。
	// 其余端口各自对应一类观测 / 管理路由；缺失端口的必需性随对应路由落地收紧。
	// Management-surface ports, adapted by the app layer; each port gates one route
	// family, and the requirement to supply it tightens as those routes land.
	AdminAuth  AdminAuth
	Registry   AgentRegistry
	Lifecycle  core.LifecyclePort
	AgentAdmin core.AgentAdmin
	Operations core.OperationStore

	// System 是 /system/overview 的数据库侧读数（运行 / 资源计数与库版本）；
	// Sampler 是 /system/sample 的采样器，nil 时按注入时钟构造。
	// System supplies the overview's database readings; a nil Sampler is built from
	// the injected clock at construction.
	System  SystemReader
	Sampler *Sampler

	// Resources 是技能 / MCP / 模型连接只读面的数据源（§3.1.8）；六个资源路由
	// 落地后为必需端口，缺端口按 500 INTERNAL 拒绝（不是空列表）。
	// Resources backs the six read-only resource routes; a missing port is an
	// assembly error answered with a fixed 500, never an empty list.
	Resources ResourceReader

	// Console 是管理台内嵌产物根（`assets.Console()`，§3.1.22）：装配期必需——缺它
	// 就没有 `/console/*` 与根重定向的落点，NewServer 直接拒绝（约束 41 / 43）。
	// Console is the embedded dist root; it is required at assembly time.
	Console fs.FS

	// Sandbox 是沙箱白名单的投影输入（§3.1.28）：路径一律是相对展示名，绝对
	// 路径由投影层再挡一次；零值 = 四类白名单全空（不是"读配置"的失败）。其中
	// `HTTPAllowedDomains` 只在未装配 NetworkAllowlist 时作只读兜底。
	// Sandbox carries the sandbox allowlists as relative display names; the zero
	// value means four empty allowlists, not a configuration failure.
	Sandbox SandboxConfig

	// NetworkAllowlist 是出网域名白名单的管理端口（R8 裁决 2）：读**当前生效**
	// 值、执行带 If-Match 的条件写。装配时必须与运行期消费者共用同一策略实例，
	// 否则"热生效"只改了副本。nil = 只读装配（GET 退回配置投影、PUT 按装配错误
	// 500）：生产装配由 app 层提供且不可缺省。
	// NetworkAllowlist is the managed outbound allowlist port; a nil port is the
	// read-only assembly seam and makes PUT fail closed with a fixed 500.
	NetworkAllowlist core.NetworkAllowlistAdmin

	// Notify 是通知渠道的只读注册表（§3.1.29）；NotifyTypes 是装配层显式注册的有序
	// 类型名（单一真值，US-12 写面复用同一映射表）。Notify 为 nil 时列表端点按 500
	// INTERNAL 拒绝——缺端口是装配错误，不是空列表。
	// Notify is the read-only channel registry; NotifyTypes is the assembly layer's
	// single ordered type list.
	Notify      core.NotifyChannelReader
	NotifyTypes []string

	// NotifyReferences 是「哪些 Agent 文档引用了某渠道」的只读端口（US-12 R3.6）：
	// 按渠道名分组返回排序去重的 Agent 名（归档项带 `archive:` 前缀）。它是**文档
	// 引用扫描**，不是精确绑定；精确的可用范围在 Agent 的 notify_channels 声明。
	// nil = 列表照常返回、引用集合为空（测试与无工作区嵌入）；生产装配必须提供，
	// 扫描失败按 500 失败关闭——**不得**把失败降级成"没有 Agent 引用"。
	// NotifyReferences groups document references by channel name. It is a textual
	// scan, not the exact binding. A scan failure fails closed, never an empty result.
	NotifyReferences NotifyReferenceReader

	// 写面端口（US-12；同样由 app 层适配）。五个端口与 US-11 的 `AgentAdmin`
	// **互不替代**：后者是内容读写与启停（`GET/PUT /content`、enable / disable），
	// 这里承载创建 / 草稿 / 多文件提交 / 复制 / 归档 / 恢复，模型连接、MCP、
	// 技能、通知渠道与工作区的读写，以及结构化配置读面。端口为 nil 时对应 handler
	// 按固定 500 失败关闭——**不得**用空实现冒充已装配，也不得把它们合并成一个
	// "大接口"。
	// The US-12 write ports; they never replace the US-11 content editor in
	// AgentAdmin. A nil port fails closed with a fixed 500.
	AgentAdminService    AgentAdminService
	ResourceAdminService ResourceAdminService
	NotifyAdminService   NotifyAdminService
	WorkspaceService     WorkspaceService
	AgentConfigService   AgentConfigService
	// ResourceStateService 是撤销托管凭据与切换使用范围两条状态动作的端口
	// （`ports_admin.go`）；它与 `ResourceAdminService` 分开，是因为两条端点各要
	// "条件写 + 显式身份 + 操作行"的完整编排，而资源写面不承担这层组合。
	// ResourceStateService carries the revoke and scope orchestrations; it is separate
	// from the resource write port because both need the full conditional-write
	// orchestration that the write port does not carry.
	ResourceStateService ResourceStateService

	// MCPCatalog 是只读 MCP 目录端口（US-14 D-01）：构建期内嵌的静态快照，读取不启动
	// 进程、不安装、不写配置。nil = 装配缺失，目录端点按固定 500 失败关闭——**不得**
	// 用空目录充数（空数组会让页面显示"没有推荐配方"，与装配故障不可区分）。
	// MCPCatalog is the read-only catalog port; nil fails closed with a fixed 500 rather
	// than serving an empty catalog, which would be indistinguishable from a real gap.
	MCPCatalog MCPCatalogReader

	// 测试接缝（仅包内测试可设）：「慢 body + 长运行」用例在秒级内触达写预算边界；
	// 「收尾不收敛」用例在毫秒级触达等待上限。生产装配不设置，一律走缺省值。
	readTimeoutOverride  time.Duration
	writeMarginOverride  time.Duration
	writeTimeoutOverride time.Duration
	drainTimeoutOverride time.Duration
}

// ProviderInfo 是 /info 的 provider 摘要：**没有**凭证与 base_url 字段（约束 25）。
// ProviderInfo never carries credentials or base URLs.
type ProviderInfo struct {
	Name       string
	Models     []string
	Configured bool
}

// SandboxConfig 是 `/sandbox` 的投影输入（§3.1.28）：四类白名单。`FileAllowedPaths`
// 是**相对工作区根的展示名**（如 `.basalt/agents/<agent>/`），装配层负责把固定目录
// 规则翻成这些名字；web 层不再拼路径——两处各拼一次必有第三处漂移。
// SandboxConfig carries the four allowlists; file paths are workspace-relative display
// names filled by the assembly layer, never host paths.
type SandboxConfig struct {
	FileAllowedPaths     []string
	ShellAllowedCommands []string
	ShellInterpreters    []string
	HTTPAllowedDomains   []string
}

// RunService 是运行入口、归档入口与运行账本读面的唯一组合端口：Run / Archive /
// Cancel 归 `core.AgentService`（准入、归档互斥与取消等待都在 core），ListRuns /
// GetRun 归 `storage.RunStore`（固定排序、窗口与过滤在 SQL 层）。生产实现是
// `app.NewRunService`——`*core.AgentService` 的方法集不含 ListRuns / GetRun，
// **不得**直接当本接口用；本层也不实现第二套账本读取或取消语义。
// RunService joins the run entry point, archiving, cancellation and the ledger read
// face. Delegation lives in app.runService; web never re-implements any of it.
type RunService interface {
	Run(ctx context.Context, req core.RunRequest) (core.RunResult, error)
	Archive(ctx context.Context, agent string) error
	ListRuns(ctx context.Context, filter core.RunFilter) ([]core.RunDetail, error)
	GetRun(ctx context.Context, runID string) (core.RunDetail, error)
	Cancel(ctx context.Context, runID string) (core.RunDetail, error)
}

// NotifyReferenceReader 是通知渠道反向引用的消费方端口：输入一组渠道名，
// 返回 `渠道名 → Agent 名列表`（排序去重）。实现扫描工作区的 Agent 文档与归档；
// 扫描失败必须返回错误——空映射会被页面读成"没有任何 Agent 引用"。
// NotifyReferenceReader resolves channel names to the agents that mention them;
// a failed scan is an error, never an empty map.
type NotifyReferenceReader interface {
	NotifyReferences(ctx context.Context, channels []string) (map[string][]string, error)
}

// AdminHandler 是受认证保护的处理函数：actor 由认证层以显式参数传入，
// **不得**写入 context（宪法 VII）；未通过认证时不得调用本函数。
// AdminHandler receives the authenticated actor as an explicit parameter, never via
// context; it must never be invoked before authentication succeeds.
type AdminHandler func(http.ResponseWriter, *http.Request, core.Actor)

// AdminAuth 是管理面认证闸门的装配端口（实现在 auth.go）：公开面闭集以外的
// 全部路由经 Wrap 包装，认证成功后以显式 actor 调用处理函数。
// AdminAuth wraps every non-public route and hands the actor to the handler.
type AdminAuth interface {
	Wrap(next AdminHandler) http.Handler
}

// AgentRegistry 是注册表快照的只读端口（实现在 internal/app）：Entries 返回按
// Agent 名升序的副本，Lookup 只查当前快照。管理台、准入与调度器看到同一份
// 不可变快照（FR22）——web **不得**另建一份 Agent 清单。
// AgentRegistry is the read-only view of the single immutable registry snapshot;
// web must never keep a second agent list.
type AgentRegistry interface {
	Entries() []core.AgentEntry
	Lookup(name string) (core.AgentEntry, bool)
}

// SessionStore 是会话读面与建行写面（适配 *storage.SessionStore）；归档写面已收归
// RunService.Archive，本端口不再暴露归档。
// SessionStore is the session read/create face; archiving moved to RunService.
type SessionStore interface {
	GetOrCreateSession(ctx context.Context, rec core.SessionRecord) (core.SessionRecord, bool, error)
	GetSession(ctx context.Context, id string) (core.SessionRecord, error)
	MessagesTail(ctx context.Context, id string, limit int) ([]core.Message, error)
	// ListSessions 是 `GET /sessions` 的读面：过滤只有 agent / status 两维，排序由
	// 实现固定（`last_active_at DESC, session_id`），Status 按 `archived_at` 派生。
	// ListSessions backs the console list; ordering and status derivation stay in
	// the implementation.
	ListSessions(ctx context.Context, filter core.SessionFilter) ([]core.SessionRecord, error)
}

// AgentProfiles 是装载结果（适配 app 保留的 entries）。
// AgentProfiles exposes the loader result, unregistered entries included.
type AgentProfiles interface {
	Profiles() []core.AgentEntry
}

// ToolDescriptor 把「工具规格 + 来源」绑在一起；来源枚举 builtin | mcp | code。
// ToolDescriptor binds a tool spec to its source.
type ToolDescriptor struct {
	Spec   core.ToolSpec
	Source string
}

// ToolCatalog 是工具清单（适配 tool.Registry + MCP 连接结果）。
// ToolCatalog is the tool inventory port.
type ToolCatalog interface {
	Tools() []ToolDescriptor
}

// MemorySections 是记忆两分区读面（适配 *memory.Service）。
// MemorySections is the two-partition memory read port.
type MemorySections interface {
	Sections(ctx context.Context, scope core.Scope) (coreText, archiveText string, err error)
}

// Server 是 HTTP 面的运行时状态。跨源保护实例在装配期构造一次；会话准入状态
// 不在 web 持有——它唯一归属 core.AgentService（约束 40）。
// Server holds the runtime state; session admission state is owned by
// core.AgentService alone.
type Server struct {
	opts Options

	// console 是构造期校验过的管理台静态壳（§3.1.3）；装配失败由 NewServer 拒绝，
	// 路由表因此可以无条件登记 `/console/*`（约束 41）。
	// console is the validated static shell; NewServer refuses to build without it.
	console *consoleHandler

	crossOrigin *http.CrossOriginProtection
	baseCtx     context.Context
	baseCancel  context.CancelFunc

	// lifecycleMu 让「进入 handler」与「切换到收尾」互斥：收尾标记一旦置位，
	// 不再有新的 handler 登记进 inflight，等待组因此不会在 Wait 之后又增长。
	// lifecycleMu serializes handler admission with the switch to draining, so the
	// wait group can never grow after the final Wait starts.
	lifecycleMu sync.Mutex
	draining    bool
	inflight    sync.WaitGroup
}

// 缺省值与超时常量（约束 18 / 19 / 20；字面量以模块卡 §3.1 为准）。
const (
	defaultAddr            = "127.0.0.1:8080"
	defaultMaxRequestBytes = 1 << 20
	defaultRunTimeout      = 60 * time.Second
	defaultShutdownGrace   = 70 * time.Second
	readHeaderTimeout      = 5 * time.Second
	readTimeout            = 60 * time.Second
	idleTimeout            = 120 * time.Second
	writeMargin            = 10 * time.Second // 写预算余量（约束 18）
	drainTimeout           = 5 * time.Second  // 停机后等 handler 收敛的上限（约束 20）
)

// denyMessage 是跨源写被拒绝时的固定公开文案（约束 21）。
// denyMessage is the fixed public text for rejected cross-origin writes.
const denyMessage = "跨源请求被拒绝"

// consoleRootPath 是管理台的挂载根：`GET /` 只做 302 到这里，不携带任何状态。
// consoleRootPath is the console mount point; the root route only redirects here.
const consoleRootPath = "/console/"

// NewServer 补默认值并校验必需端口。缺端口是装配错误而非运行时故障——当场拒绝，
// 避免进程起来后每个请求都 500。
// NewServer fills defaults and rejects a missing port up front.
func NewServer(opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = defaultAddr
	}
	if opts.MaxRequestBytes <= 0 {
		opts.MaxRequestBytes = defaultMaxRequestBytes
	}
	if opts.RunTimeout <= 0 {
		opts.RunTimeout = defaultRunTimeout
	}
	if opts.ShutdownGrace <= 0 {
		opts.ShutdownGrace = defaultShutdownGrace
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = opts.Now()
	}
	if opts.Sampler == nil {
		opts.Sampler = NewSampler(opts.Now)
	}
	if opts.Runs == nil || opts.Sessions == nil || opts.Agents == nil ||
		opts.Tools == nil || opts.Memory == nil {
		return nil, errors.New("web: 装配缺少必需端口（Runs / Sessions / Agents / Tools / Memory）")
	}
	if opts.Console == nil {
		return nil, errors.New("web: 装配缺少管理台资源（Console）")
	}
	console, err := NewConsoleHandler(opts.Console)
	if err != nil {
		return nil, err
	}

	s := &Server{opts: opts, console: console}
	s.crossOrigin = http.NewCrossOriginProtection()
	s.crossOrigin.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 拒绝发生在读 body 之前：跨源请求零副作用（约束 21）。
		s.writeError(w, http.StatusForbidden, codeBadRequest, denyMessage)
	}))
	return s, nil
}

// Run 监听并服务，直到 ctx 取消后按五阶段协议收尾（约束 20）。**异常退出与信号
// 触发走同一条收尾路径**：本函数返回即代表已取消在途请求、关闭活动连接并等待
// 全部 handler 退出，调用方随后才可释放 MCP / 数据库 / 锁。
// Run serves until ctx is cancelled and then drains per the five-phase protocol; an
// abnormal Serve error takes the very same cleanup path.
func (s *Server) Run(ctx context.Context) error {
	ln := s.opts.Listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", s.opts.Addr)
		if err != nil {
			return fmt.Errorf("监听 %s: %w", s.opts.Addr, err)
		}
	}
	srv := s.newHTTPServer(ctx)
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("HTTP 服务: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		// 监听 / 服务异常退出：直接返回会让在途 handler 在调用方释放共享资源后
		// 继续运行；因此同样经过 ③④⑤ 的统一收尾（R21）。
		s.stopServing(srv)
		return err
	case <-ctx.Done():
		// ① 停止接入 + ② 宽限等待：Shutdown 先关监听器，再等活动连接空闲。
		// 基上下文与信号 ctx 解耦，宽限期内运行不受信号影响（约束 20）。
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownGrace)
		err := srv.Shutdown(shutdownCtx)
		cancel()
		if err != nil {
			// ③ 宽限到期：与异常退出共用收尾（等待超时不得视为已退出，R22）。
			s.stopServing(srv)
			// ⑤ 共享资源由 app.Run 的 defer rt.Close() 在本函数返回后释放。
			return fmt.Errorf("优雅停机宽限 %s 到期: %w", s.opts.ShutdownGrace, err)
		}
		return nil
	}
}

// stopServing 是异常退出与宽限耗尽共用的 ③④⑤ 收尾：先切到 draining（此后不再
// 接受新的 handler），再取消在途请求并关闭活动连接，最后等待在途计数归零。
//
// **等待超时的终止策略：记 ERROR 后继续等待，不返回。** 安全优先于活性——若在
// handler 仍存活时返回，调用方会释放数据库 / MCP 连接，handler 随后触碰的是已
// 释放的资源；因此 `drainTimeout` 只是「发现不收敛」的报告点，不是放弃点。进程
// 需要被强制终止时由外部监管（systemd / k8s）发 SIGKILL，那等于放弃清理，而不是
// 把等待超时当作收尾完成。
// stopServing is the shared teardown. A drain timeout is reported but never treated
// as completion: after logging it the function keeps waiting, because returning while
// a handler is alive would let the caller release resources still in use.
func (s *Server) stopServing(srv *http.Server) {
	s.enterDraining()
	s.cancelBase()
	// Close 的错误在收尾阶段没有可行动作：监听器已在 Shutdown / Serve 失败时关闭。
	_ = srv.Close()
	limit := s.drainTimeoutValue()
	if !s.waitInflight(limit) {
		s.logger().Error("停机后等待在途请求收敛超过上限，继续等待至全部退出",
			"drain_timeout", limit)
		s.inflight.Wait()
	}
}

// beginRequest 登记一个在途 handler；收尾已开始时返回 false，调用方必须直接返回
// （不再触达路由与共享资源）。登记与收尾切换共用 lifecycleMu，保证 Wait 之后不会
// 再出现新的 Add。
// beginRequest admits an in-flight handler unless teardown has begun.
func (s *Server) beginRequest() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.draining {
		return false
	}
	s.inflight.Add(1)
	return true
}

// endRequest 与 beginRequest 成对出现（唯一合法配对是 requestLogMiddleware 的 defer）。
// endRequest pairs with a successful beginRequest.
func (s *Server) endRequest() {
	s.inflight.Done()
}

// enterDraining 是收尾开关：置位后 beginRequest 一律拒绝，在途计数只减不增。
// enterDraining flips the switch so the in-flight count only decreases afterwards.
func (s *Server) enterDraining() {
	s.lifecycleMu.Lock()
	s.draining = true
	s.lifecycleMu.Unlock()
}

// handler 组装中间件链：recover → 请求日志 → 跨源保护 → 路由（约束 21）。
// handler builds the fixed middleware chain.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return s.recoverMiddleware(s.requestLogMiddleware(s.crossOriginMiddleware(mux)))
}

// routeVisibility 区分三类端点：公开（闭集：健康检查 / 管理台静态壳 / 根重定向）、
// 受保护（一律认证）与兜底 404（不参与认证判定，方法无关）。
// routeVisibility separates public, protected, and fallback endpoints.
type routeVisibility int

const (
	routePublic routeVisibility = iota
	routeProtected
	routeFallback
)

// route 是路由表的一行。兜底项方法为空串（方法无关）；其余项必须显式给出方法。
// 处理函数二选一：`handler` 是既有不消费认证主体的处理函数；`adminHandler` 是
// US-11 起需要 actor 的处理函数——认证成功的 actor 由 `s.admin` 直接传入，**不经**
// context（宪法 VII / 约束 9）。两者恰一非 nil，路由表仍是唯一枚举。
// A route row; the fallback row carries an empty method. Exactly one of handler and
// adminHandler is set: adminHandler receives the authenticated actor as a parameter.
type route struct {
	method       string
	pattern      string
	visibility   routeVisibility
	handler      http.HandlerFunc
	adminHandler AdminHandler
}

// routeTable 是端点集的唯一枚举：注册（routes）与认证遍历测试共用同一份表，
// **不得**另写第二份清单（约束 8）。公开面是闭集（健康检查、管理台静态壳、根
// 重定向），新增公开入口必须同时修订契约与本表。管理台两条静态壳路由随 console
// 处理器（T049）一并登记：`/console` 精确路径单独成行，避免 ServeMux 对子树模式
// 自动发 301（图 H 要求二者都直接得到 index.html）。
// 表由两段组成：US-11 及以前的字面行 + `adminRoutes()` 返回的 US-12 写面行，
// 兜底恒为最后一行。两段都只是**同一张表**的组成部分，注册与遍历看到的行一致。
// routeTable is the single enumeration shared by registration and the auth matrix;
// the console rows are exact-path plus subtree, and the US-12 rows are appended from
// admin_routes.go ahead of the fallback, which always stays last.
func (s *Server) routeTable() []route {
	rows := []route{
		{method: http.MethodGet, pattern: "/{$}", visibility: routePublic, handler: s.handleRoot},
		{method: http.MethodGet, pattern: "/api/v1/health", visibility: routePublic, handler: s.handleHealth},
		{method: http.MethodGet, pattern: "/api/v1/info", visibility: routeProtected, handler: s.handleInfo},
		{method: http.MethodGet, pattern: "/api/v1/profiles", visibility: routeProtected, handler: s.handleProfiles},
		{method: http.MethodGet, pattern: "/api/v1/tools", visibility: routeProtected, handler: s.handleTools},
		{method: http.MethodGet, pattern: "/api/v1/memory", visibility: routeProtected, handler: s.handleMemory},
		{method: http.MethodPost, pattern: "/api/v1/sessions", visibility: routeProtected, handler: s.handleCreateSession},
		{method: http.MethodGet, pattern: "/api/v1/sessions", visibility: routeProtected, adminHandler: s.handleListSessions},
		{method: http.MethodGet, pattern: "/api/v1/sessions/{id}", visibility: routeProtected, handler: s.handleGetSession},
		{method: http.MethodDelete, pattern: "/api/v1/sessions/{id}", visibility: routeProtected, handler: s.handleArchiveSession},
		{method: http.MethodPost, pattern: "/api/v1/sessions/{id}/messages", visibility: routeProtected, handler: s.handlePostMessage},
		{method: http.MethodPost, pattern: "/api/v1/agents/{name}/invoke", visibility: routeProtected, handler: s.handleInvoke},
		{method: http.MethodGet, pattern: "/api/v1/agents/{name}/memory", visibility: routeProtected, adminHandler: s.handleAgentMemory},
		{method: http.MethodGet, pattern: "/api/v1/sandbox", visibility: routeProtected, adminHandler: s.handleSandbox},
		{method: http.MethodGet, pattern: "/api/v1/notify-channels", visibility: routeProtected, adminHandler: s.handleListNotifyChannels},
		{method: http.MethodGet, pattern: "/api/v1/notify-channels/types", visibility: routeProtected, adminHandler: s.handleNotifyTypes},
		{method: http.MethodGet, pattern: "/api/v1/agents/{name}/config", visibility: routeProtected, adminHandler: s.handleGetAgentConfig},
		{method: http.MethodGet, pattern: "/api/v1/agents/{name}/content", visibility: routeProtected, adminHandler: s.handleGetAgentContent},
		{method: http.MethodPut, pattern: "/api/v1/agents/{name}/content", visibility: routeProtected, adminHandler: s.handlePutAgentContent},
		{method: http.MethodPost, pattern: "/api/v1/agents/{name}/disable", visibility: routeProtected, adminHandler: s.handleDisableAgent},
		{method: http.MethodPost, pattern: "/api/v1/agents/{name}/enable", visibility: routeProtected, adminHandler: s.handleEnableAgent},
		{method: http.MethodGet, pattern: "/api/v1/runs", visibility: routeProtected, adminHandler: s.handleListRuns},
		{method: http.MethodGet, pattern: "/api/v1/runs/{id}", visibility: routeProtected, adminHandler: s.handleRunDetail},
		{method: http.MethodPost, pattern: "/api/v1/runs/{id}/cancel", visibility: routeProtected, adminHandler: s.handleCancelRun},
		{method: http.MethodGet, pattern: "/api/v1/operations", visibility: routeProtected, adminHandler: s.handleListOperations},
		{method: http.MethodGet, pattern: "/api/v1/operations/{id}", visibility: routeProtected, adminHandler: s.handleOperationDetail},
		{method: http.MethodGet, pattern: "/api/v1/system/overview", visibility: routeProtected, adminHandler: s.handleSystemOverview},
		{method: http.MethodGet, pattern: "/api/v1/system/sample", visibility: routeProtected, adminHandler: s.handleSystemSample},
		{method: http.MethodGet, pattern: "/api/v1/skills", visibility: routeProtected, adminHandler: s.handleListSkills},
		{method: http.MethodGet, pattern: "/api/v1/skills/{name}", visibility: routeProtected, adminHandler: s.handleGetSkill},
		{method: http.MethodGet, pattern: "/api/v1/mcp-servers", visibility: routeProtected, adminHandler: s.handleListMCPServers},
		{method: http.MethodGet, pattern: "/api/v1/mcp-servers/{name}", visibility: routeProtected, adminHandler: s.handleGetMCPServer},
		{method: http.MethodGet, pattern: "/api/v1/model-connections", visibility: routeProtected, adminHandler: s.handleListModelConnections},
		{method: http.MethodGet, pattern: "/api/v1/model-connections/{name}", visibility: routeProtected, adminHandler: s.handleGetModelConnection},
		{method: http.MethodGet, pattern: "/api/v1/schedules", visibility: routeProtected, handler: s.handleListSchedules},
		{method: http.MethodPut, pattern: "/api/v1/schedules/{id}", visibility: routeProtected, handler: s.handleSetScheduleEnabled},
		{method: http.MethodGet, pattern: "/api/v1/schedules/{id}/executions", visibility: routeProtected, handler: s.handleListExecutions},
		{method: http.MethodPost, pattern: "/api/v1/schedules/{id}/run", visibility: routeProtected, handler: s.handleRunSchedule},
		// 管理台静态壳（图 H、约束 41）：两条都是公开路由，由 consoleHandler 自行
		// 区分真实资源与 SPA 回退——服务端不解析前端路由。
		// The console shell is public; the handler decides asset versus SPA fallback.
		{method: http.MethodGet, pattern: "/console", visibility: routePublic, handler: s.console.ServeHTTP},
		{method: http.MethodGet, pattern: "/console/{path...}", visibility: routePublic, handler: s.console.ServeHTTP},
	}
	// US-12 写面 30 条由 admin_routes.go 唯一枚举（§3.7.1）：逐行标记受保护，
	// 条件写在认证之后套 If-Match。并入点固定在兜底之前——兜底必须仍是最后一行，
	// 否则未匹配路径会先撞上兜底以外的行。
	// The US-12 admin rows join before the method-agnostic fallback, which must stay
	// last; the rows themselves are enumerated only in admin_routes.go.
	rows = append(rows, s.adminRoutes()...)
	// 方法无关 catch-all（注 3）：未匹配的路径与方法一律落到 404 信封，
	// 不暴露 ServeMux 的纯文本 404 与自动 405。
	return append(rows, route{pattern: "/", visibility: routeFallback, handler: s.handleNotFound})
}

// routes 按路由表注册：每条非兜底项按 `方法 模式` 注册，兜底项方法无关。受保护
// 路由在这里统一套上认证闸门——新增受保护端点只要进表就自动获得认证，不依赖
// 逐条手工挂载（约束 8）。
// routes registers every table entry; protected rows are wrapped with the auth guard
// centrally, so a new protected endpoint cannot be registered without authentication.
func (s *Server) routes(mux *http.ServeMux) {
	for _, rt := range s.routeTable() {
		pattern := rt.pattern
		if rt.method != "" {
			pattern = rt.method + " " + rt.pattern
		}
		mux.Handle(pattern, s.routeHandler(rt))
	}
}

// routeHandler 按可见性决定是否过认证闸门：受保护路由经 s.admin 包装，公开路由
// 与兜底 404 **不经过**闸门（公开面是闭集，兜底不参与认证判定）。测试装配可以
// 不提供 AdminAuth（新路由落地前的旧用例只关心处理函数本身）：此时需要 actor 的
// 处理函数收到零值 actor，生产装配由 app 层保证非 nil——serve 在装配失败时拒绝启动。
// routeHandler wraps protected rows with the auth guard; public and fallback rows
// bypass it. Without AdminAuth (tests) an adminHandler receives the zero actor, and
// the production assembly always supplies the guard.
func (s *Server) routeHandler(rt route) http.Handler {
	plain := rt.handler
	if plain == nil && rt.adminHandler != nil {
		admin := rt.adminHandler
		plain = func(w http.ResponseWriter, r *http.Request) {
			admin(w, r, core.Actor{})
		}
	}
	if rt.visibility != routeProtected || s.opts.AdminAuth == nil {
		return plain
	}
	if rt.adminHandler != nil {
		return s.admin(rt.adminHandler)
	}
	return s.admin(func(w http.ResponseWriter, r *http.Request, _ core.Actor) {
		plain(w, r)
	})
}

// admin 是约束 9 的显式 actor 入口：认证成功后 actor 以参数交给处理函数，**不得**
// 写入 context。当前既有处理函数不消费 actor，仍走同一包装并显式丢弃；生命周期 /
// 内容写等需要 actor 的处理函数后续直接复用本包装。
// admin is the explicit-actor entry point (constraint 9): the actor arrives as a
// parameter, never through context; handlers that ignore it still use this wrapper.
func (s *Server) admin(h AdminHandler) http.Handler {
	return s.opts.AdminAuth.Wrap(h)
}

// handleRoot 是根路径重定向（图 H）：`GET /` → 302 `/console/`。根路径属于公开
// 闭集——它只把浏览器送到管理台壳，不暴露任何状态。用 `GET /{$}` 精确匹配，
// 未匹配的路径仍落到兜底 404，不被重定向吞掉。
// handleRoot redirects the exact root path; unmatched paths still reach the 404
// fallback because the route is registered as the exact pattern `GET /{$}`.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, consoleRootPath, http.StatusFound)
}

// newHTTPServer 以 WithoutCancel + WithCancel 派生基上下文：信号取消不直接传递给
// 在途请求，只有 ③ 停机阶段调用 cancelBase 才取消（约束 20）。四超时按约束 18 设置。
// newHTTPServer derives a signal-decoupled base context and applies all four timeouts.
func (s *Server) newHTTPServer(ctx context.Context) *http.Server {
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.baseCtx, s.baseCancel = base, cancel
	return &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       s.readTimeoutValue(),
		WriteTimeout:      s.writeTimeout(),
		IdleTimeout:       idleTimeout,
		BaseContext:       func(net.Listener) context.Context { return base },
	}
}

// cancelBase 是 ③ 停机阶段：取消全部在途请求的上下文。
// cancelBase cancels every in-flight request context at stage ③.
func (s *Server) cancelBase() {
	if s.baseCancel != nil {
		s.baseCancel()
	}
}

// waitInflight 有界等待在途 handler 收敛（④ 阶段）；超时返回 false。
// waitInflight waits for in-flight handlers up to limit; false means it timed out.
func (s *Server) waitInflight(limit time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// writeTimeout 派生写预算：读超时 + 运行时限 + 余量（约束 18）。写 deadline 覆盖
// 「读 body + 运行 + 写响应」全段，只比运行时限大不能证明预算充足。
// writeTimeout derives the write budget from read timeout, run timeout, and margin.
func (s *Server) writeTimeout() time.Duration {
	if s.opts.writeTimeoutOverride > 0 {
		return s.opts.writeTimeoutOverride
	}
	return s.readTimeoutValue() + s.opts.RunTimeout + s.writeMarginValue()
}

// readTimeoutValue / writeMarginValue 支持测试接缝；生产装配恒为固定常量。
// readTimeoutValue / writeMarginValue honor the test seams.
func (s *Server) readTimeoutValue() time.Duration {
	if s.opts.readTimeoutOverride > 0 {
		return s.opts.readTimeoutOverride
	}
	return readTimeout
}

func (s *Server) writeMarginValue() time.Duration {
	if s.opts.writeMarginOverride > 0 {
		return s.opts.writeMarginOverride
	}
	return writeMargin
}

// drainTimeoutValue 支持「收尾不收敛」用例；生产装配恒为 drainTimeout。
// drainTimeoutValue honors the test seam; production always uses drainTimeout.
func (s *Server) drainTimeoutValue() time.Duration {
	if s.opts.drainTimeoutOverride > 0 {
		return s.opts.drainTimeoutOverride
	}
	return drainTimeout
}
