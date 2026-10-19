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

// RunService 是唯一运行入口与归档入口（适配 *core.AgentService）：归档与在途运行
// 的互斥在服务端同一把锁内判定（约束 40），web 不再登记任何在途计数。
// RunService is the only run and archive entry point; admission lives in the service.
type RunService interface {
	Run(ctx context.Context, req core.RunRequest) (core.RunResult, error)
	Archive(ctx context.Context, id string) error
}

// SessionStore 是会话读面与建行写面（适配 *storage.SessionStore）；归档写面已收归
// RunService.Archive，本端口不再暴露归档。
// SessionStore is the session read/create face; archiving moved to RunService.
type SessionStore interface {
	GetOrCreateSession(ctx context.Context, rec core.SessionRecord) (core.SessionRecord, bool, error)
	GetSession(ctx context.Context, id string) (core.SessionRecord, error)
	MessagesTail(ctx context.Context, id string, limit int) ([]core.Message, error)
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
	if opts.Runs == nil || opts.Sessions == nil || opts.Agents == nil ||
		opts.Tools == nil || opts.Memory == nil {
		return nil, errors.New("web: 装配缺少必需端口（Runs / Sessions / Agents / Tools / Memory）")
	}

	s := &Server{opts: opts}
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

// routes 注册固定端点集与一条方法无关 catch-all（注 3）：未匹配的路径与方法
// 一律落到 404 信封，不暴露 ServeMux 的纯文本 404 与自动 405。
// routes registers the fixed endpoint set plus a method-agnostic catch-all.
func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/info", s.handleInfo)
	mux.HandleFunc("GET /api/v1/profiles", s.handleProfiles)
	mux.HandleFunc("GET /api/v1/tools", s.handleTools)
	mux.HandleFunc("GET /api/v1/memory", s.handleMemory)
	mux.HandleFunc("POST /api/v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.handleArchiveSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/messages", s.handlePostMessage)
	mux.HandleFunc("POST /api/v1/agents/{name}/invoke", s.handleInvoke)
	mux.HandleFunc("GET /api/v1/schedules", s.handleListSchedules)
	mux.HandleFunc("PUT /api/v1/schedules/{id}", s.handleSetScheduleEnabled)
	mux.HandleFunc("GET /api/v1/schedules/{id}/executions", s.handleListExecutions)
	mux.HandleFunc("POST /api/v1/schedules/{id}/run", s.handleRunSchedule)
	mux.HandleFunc("/", s.handleNotFound)
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
