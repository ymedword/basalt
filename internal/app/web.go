package app

// 本文件把重路径装配态适配成 HTTP 面（颗粒度文档 §3.1 的 `internal/app/web.go` 行）：
// `serve` 在重路径装配成功之后、交出端口之前，经这里把配置、运行入口、仓储与只读
// 装配态投影成 `web.Options`。**契约方向不因此改变**：web 仍只认自己声明的端口，
// 装配层是唯一知道"端口背后是哪个实例"的地方（与 ports.go 同一套适配纪律）。
// This file projects the heavy-path assembly state onto web.Options; web keeps
// depending only on its own consumer-side ports.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/assets"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/web"
	"github.com/ymedword/basalt/internal/workspace"
)

// defaultListenAddr 是 serve 未配置 `http.addr` 时的监听地址，与 web.Options 的
// 缺省一致：安全默认只**听本机**。app 在 web 之前创建监听，因此必须在这里显式给出
// 同一取值；装配后以**实际**监听地址为准（端口可能变化）。
// defaultListenAddr mirrors web's default so the loopback safety default holds before
// the listener exists; the actual bound address wins afterwards.
const defaultListenAddr = "127.0.0.1:8080"

// newWebServer 把重路径装配态适配成 web.Options、创建监听并构造 HTTP 服务（图 A ⑥）：
// 密码来源先解析（首启生成不能因为端口被占而丢失），**再绑定监听、用真实地址校验管理
// 认证配置**——缺 mode / 非法 mode / token 模式缺凭据 / local_trust 绑定非回环，都在
// 交出端口之前拒绝启动（约束 1 / 3 / 4）。
// newWebServer resolves the token source first, then binds the listener and validates
// the management-auth configuration against the real address before serving.
//
// 配置一律读 `rt.cfg`（newRuntime 解出的**生效值**，绝不为 nil）——**不读**可能为 nil
// 的 `App.cfg`：配置缺失时重路径用的是 `&cli.Config{}`，两处读法的差异正是"配置了
// 缺省却按另一套缺省跑"的来源。时钟用 `a.nowOr()`，与运行入口、恢复扫描同源。
// Configuration comes from rt.cfg (the effective value) and the clock from a.nowOr(),
// so the HTTP face can never diverge from the run entry point.
func (a *App) newWebServer(rt *heavyRuntime) (*web.Server, error) {
	if rt == nil {
		return nil, fmt.Errorf("HTTP 面装配失败：重路径运行期为空")
	}
	// 访问密码的第三个来源（T090）在**绑定监听之前**解析：`mode: token` 且配置没给
	// token 时，从实例私有目录的 admin-token 读取，只有新实例首次启动才生成。首启生成
	// 不能等到端口就绪——端口被占（另一个 HTTP 服务、上一次没退干净的实例）会让首次
	// 启动失败，而重试时库与名册都已存在、不再算新实例，密码就再也没有机会生成。
	// 三处都没有时**不**回落 local_trust——校验失败即拒绝启动，与配置缺 token 同一处
	// 拒绝点（校验本身仍在绑定之后，用真实地址判 Host 三元组）。
	// The token file is resolved before binding: a first start that loses the port must
	// not lose the one chance to mint the password, or the retry has no new-instance
	// verdict left and only a hand-written file can recover it.
	token, tokenErr := a.resolveAdminToken(rt.cfg)
	if tokenErr != nil {
		return nil, fmt.Errorf("管理认证配置校验失败: %w", tokenErr)
	}

	addr := listenAddr(rt.cfg)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("监听 %s: %w", addr, err)
	}
	logger := a.loggerOr()
	bound := ln.Addr().String()

	opts := a.webOptions(rt)
	opts.Addr = bound
	opts.Listener = ln

	authCfg := adminAuthConfig(rt.cfg, bound)
	if token.Source != "" {
		authCfg.Token = token.Token
	}
	if err := web.ValidateAdminAuth(authCfg, []string{bound}); err != nil {
		closeListener(logger, ln)
		return nil, fmt.Errorf("管理认证配置校验失败: %w", err)
	}
	guard, err := web.NewAuthGuard(authCfg, logger, a.nowOr())
	if err != nil {
		closeListener(logger, ln)
		return nil, fmt.Errorf("管理认证闸门装配失败: %w", err)
	}
	opts.AdminAuth = guard

	srv, err := web.NewServer(opts)
	if err != nil {
		closeListener(logger, ln)
		return nil, err
	}
	// 生成提示放在服务构造成功之后：只有真的能开服务，才值得把密码出示一次。
	// The one-time hint comes after the server exists, so a failed start prints nothing.
	announceAdminToken(os.Stdout, token, isInteractiveWriter(os.Stdout))
	return srv, nil
}

// listenAddr 取生效监听地址：空配置落回环缺省，绝不落到"全接口监听"。
// listenAddr resolves the effective listen address; an empty value stays loopback.
func listenAddr(cfg *cli.Config) string {
	if cfg == nil || strings.TrimSpace(cfg.HTTP.Addr) == "" {
		return defaultListenAddr
	}
	return cfg.HTTP.Addr
}

// closeListener 在装配失败时释放已绑定的监听器。关闭失败只影响资源回收，记 WARN
// 而不掩盖首错（错误路径双轨：首错上抛、清理异常落日志）。
// closeListener releases the bound listener on assembly failure; a close error is
// logged but never masks the original failure.
func closeListener(logger *slog.Logger, ln net.Listener) {
	if err := ln.Close(); err != nil {
		logger.Warn("HTTP 监听器关闭失败", "error", err)
	}
}

// adminAuthConfig 把 `http.admin_auth` 段投影成 web.AdminAuthConfig，并按实际监听
// 地址补齐缺省白名单（约束 4 / 5）：allowed_hosts 缺省只在**回环监听**上派生三元组
// （127.0.0.1 / localhost / [::1]）；非回环与通配监听**不派生**——必须显式列出，
// 否则 ValidateAdminAuth 拒绝启动（D-10）。allowed_origins 缺省取有效 Host 列表的
// `http://` 形态；显式配置一律原样保留。
// adminAuthConfig projects the section and fills defaults from the bound address;
// non-loopback listeners never get derived hosts, so they must be explicit.
func adminAuthConfig(cfg *cli.Config, listener string) web.AdminAuthConfig {
	auth := web.AdminAuthConfig{}
	if cfg != nil {
		auth.Mode = cfg.HTTP.AdminAuth.Mode
		auth.Token = cfg.HTTP.AdminAuth.Token
		auth.AllowedHosts = append([]string(nil), cfg.HTTP.AdminAuth.AllowedHosts...)
		auth.AllowedOrigins = append([]string(nil), cfg.HTTP.AdminAuth.AllowedOrigins...)
		auth.TrustedProxies = append([]string(nil), cfg.HTTP.AdminAuth.TrustedProxies...)
	}
	if len(auth.AllowedHosts) == 0 {
		auth.AllowedHosts = deriveDefaultHosts(listener)
	}
	if len(auth.AllowedOrigins) == 0 {
		auth.AllowedOrigins = deriveDefaultOrigins(auth.AllowedHosts)
	}
	return auth
}

// deriveDefaultHosts 在回环监听上派生缺省 Host 三元组；非回环 / 通配 / 非法地址
// 返回 nil（下游 ValidateAdminAuth 据此拒绝启动）。
// deriveDefaultHosts returns the loopback trio or nil for anything else.
func deriveDefaultHosts(listener string) []string {
	host, port, err := net.SplitHostPort(listener)
	if err != nil || !loopbackHost(host) {
		return nil
	}
	return []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port}
}

// deriveDefaultOrigins 把有效 Host 列表投影成 `http://` 形态（约束 5 的缺省 origins）。
// deriveDefaultOrigins maps the effective hosts to their http:// origin forms.
func deriveDefaultOrigins(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	origins := make([]string, 0, len(hosts))
	for _, host := range hosts {
		origins = append(origins, "http://"+host)
	}
	return origins
}

// loopbackHost 与 web.ValidateAdminAuth 的回环判定同口径：localhost、127.0.0.0/8、
// ::1（含 IPv4-mapped）。两处必须一起改。
// loopbackHost mirrors web's loopback rule; the two must move together.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// webOptions 是 newWebServer 的**纯投影**（拆成独立函数是可判卷前提：包外看不到
// `web.Server.opts`，投影错了只能靠行为用例间接发现）。生产装配只经 newWebServer
// 调用它；用例直接断言投影值。
// webOptions is a pure projection split out so tests can assert it directly; the
// server keeps its options private and production only reaches them via newWebServer.
func (a *App) webOptions(rt *heavyRuntime) web.Options {
	cfg := rt.cfg
	if cfg == nil {
		// 纵深防御：newRuntime 保证非 nil；这里兜底的是"未来某处构造了半截运行期"。
		// Defensive only: newRuntime guarantees a non-nil effective configuration.
		cfg = &cli.Config{}
	}
	opts := web.Options{
		Addr:            cfg.HTTP.Addr,
		MaxRequestBytes: cfg.HTTP.MaxRequestBytes,
		// 手动补跑与定时触发共用同一条执行链，预算取同一个常量（scheduleRunTimeout）：
		// 两处写死不同数字时，同一个任务定时能跑完、手动点一下却在 60s 处被截断。
		// The manual re-run shares the scheduled budget through one constant.
		ScheduleRunTimeout: scheduleRunTimeout,
		RunBudget:          webBudget(cfg),
		Version:            Version,
		MemoryBackend:      memoryBackendName(cfg),
		// MCP 连接数取**真实连接结果**（约束 25）：配置了几个与连上几个是两回事。
		// The connected count comes from the actual connections, not from configuration.
		MCPConnected: len(rt.mcpConns),
		Providers:    webProviders(cfg),
		Logger:       a.loggerOr(),
		Now:          a.nowOr(),

		// 运行端口必须是适配器：web 需要账本读面（ListRuns / GetRun）与取消，方法集
		// 比 *core.AgentService 多出三项；装配层组合、web 只依赖接口（§3.1.31）。
		// The run port is the adapter; web never receives the bare AgentService.
		Runs:     NewRunService(rt.service, rt.runs),
		Sessions: rt.sessions,
		Agents:   agentProfilesPort{registry: rt.agents, entries: rt.entries},
		Tools:    newToolCatalogPort(rt.registry, rt.mcpRT),
		Memory:   rt.mem,
		// Schedules 在重路径中**恒非 nil**（约束 21）：chat / tool 只是不 Start，
		// 调度器本身始终构造，四个 /schedules 端点因此不会因模式而缺端口。
		// The scheduler port is always present on the heavy path; only Start is
		// deferred to serve / gateway.
		Schedules: rt.scheduler,

		// 沙箱页与通知渠道页（§3.1.28 / §3.1.29）：白名单是配置面的**展示投影**，
		// 渠道仓储只读且与发送路径共用同一实现——页面读到的就是下一发会用的。
		// Sandbox is a display projection of the configuration; the notify repo is the
		// very same read face the send path uses.
		Sandbox: sandboxConfigView(cfg),
		// 出网域名白名单的管理端口（R8 裁决 2）：与运行期策略、MCP supervisor 同一批
		// 实例，保存成功即热生效；`http.allowed_domains` 因此不再只来源于配置投影。
		// The managed allowlist port is the very instance the live policy uses.
		NetworkAllowlist: rt.networkAllowlist,
		// 禁止域名表（14D）：同一张表的第二行、同一套条件写；页面 `denied_domains`
		// 与 `network_denylist` 读它，保存成功即对下一次判定生效。
		// The denylist row and its conditional write share the same implementation.
		NetworkDenylist: rt.networkDenylist,
		// 访问例外（14D）：列表与集合 ETag、创建 / 删除。服务与守门器快照同源，
		// 删除后下一次拨号即失效。
		// The exception set is shared with the dial guard; deletion binds immediately.
		NetworkExceptions: rt.networkExceptions,
		// 执行档位端口（14D D-03）：与运行期策略实例同源，`PUT /execution-policy`
		// 保存成功即改下一次判定；读端点另外给出两组待确认项（只读扫描）。
		// The profile port shares the live policy instance; the read endpoint also
		// carries the two read-only pending axes.
		ExecutionPolicy: rt.execPolicy,
		// 实例加密端口（14D / E9）：投影**启动 `Ensure` 判定表本身**的实例——页面报出
		// 的状态就是下一次启动会采取的动作。这里刻意不接命令面的 `Status`（未配置主
		// 密钥路径时它短路成错误），恢复态因此是状态而**不是**读失败。
		// The instance-encryption port projects the very decision table Ensure uses; the
		// command-surface Status is not reused because it short-circuits to an error.
		InstanceSecrets: instanceSecretsPortOf(rt.instanceSecrets),
		// 管理读面 = 发送读面 + 属主名 / 凭据状态 / 条件 ETag 三件事（notify_read.go）。
		// 写后下一发即可见：两者共用同一个零缓存仓储实例。
		// The admin read face wraps the very same send-path repository.
		Notify: newNotifyChannelReadPort(storage.NewNotifyChannelRepo(rt.db), rt.credentials, rt.versions,
			notifyReadDeps{ConnectionTests: rt.connectionTests}),
		NotifyTypes: notifyTypeNames(),
		// 反向引用与删除闸门同源：扫描活动与归档 AGENT.md 的整词提及。
		// Reverse references share the delete gate's document scan.
		NotifyReferences: notifyReferencePort{root: rt.root},

		// 管理台产物是编译期常量（go:embed，约束 43）：装配期直接给出，运行时不读磁盘，
		// 也不依赖 Node——单二进制复制到目标机后页面照常打开。
		// The console artifact is embedded at compile time; no disk or Node at runtime.
		Console: assets.Console(),
		// 实例上限投影（14D / E8）：就绪视图要把 Agent 声明的 `settings.max_iterations`
		// 与**生效上限**对照、把计划数与会**实际拦截**的并发上限对照。0 值原样交给
		// `web.NewServer` 补 core 的同一对缺省常量——装配层**不**在这里再抄一份 32 / 10。
		// Instance limits are projected as configured; the zero value is normalized by
		// web.NewServer against the same core defaults the run path uses.
		InstanceLimits: web.InstanceLimits{
			MaxIterations:     cfg.Budget.MaxIterations,
			MaxConcurrentRuns: cfg.Server.MaxConcurrentRuns,
		},
	}
	a.wireManagementPorts(&opts, rt)
	wireWritePorts(&opts, rt)
	return opts
}

// writeServices 是 US-12 写面适配器的装配槽：六个字段与 `web` 包的六组消费方接口
// （ports_admin.go）一一对应。构造任务逐字段填入；本文件只做逐字段投影，空槽保持
// nil——handler 对缺端口按固定 500 失败关闭，**不得**在装配层换成空实现冒充"可用"。
// writeServices holds the US-12 write adapters; an empty slot stays nil so a handler
// fails closed with a fixed 500 instead of pretending the port is assembled.
type writeServices struct {
	agentAdmin    web.AgentAdminService
	resourceAdmin web.ResourceAdminService
	notifyAdmin   web.NotifyAdminService
	workspace     web.WorkspaceService
	agentConfig   web.AgentConfigService
	resourceState web.ResourceStateService
}

// wireWritePorts 把运行期的写面适配器逐个投影到 `web.Options`。**刻意不并入
// wireManagementPorts**：后者在依赖不全时整体提前返回，而六个写面端口彼此独立——
// 缺一个不影响其余，缺的那个由对应 handler 按 500 拒绝。六个端口与 US-11 的
// `Options.AgentAdmin`（内容读写与启停）互不替代。
// wireWritePorts projects each write port independently; it is deliberately separate
// from wireManagementPorts, whose early return covers a different port family.
func wireWritePorts(opts *web.Options, rt *heavyRuntime) {
	if rt == nil {
		return
	}
	opts.AgentAdminService = rt.writeServices.agentAdmin
	opts.ResourceAdminService = rt.writeServices.resourceAdmin
	opts.NotifyAdminService = rt.writeServices.notifyAdmin
	opts.WorkspaceService = rt.writeServices.workspace
	opts.AgentConfigService = rt.writeServices.agentConfig
	opts.ResourceStateService = rt.writeServices.resourceState
}

// agentAdminPort 把 `*AgentAdmin` 适配成 `web.AgentAdminService`（`writeServices.agentAdmin`
// 的唯一实现，§3.1.8 第 1 个端口）：两处转换只做形状投影、不改语义——`AgentFilesView`
// 投影成 `web.AgentFileSet`，`AgentDraft` 取出 `[]core.AuthorFile`。作者配置在装配期
// **一次冻结**（`author:` 段由命令面装载一次），端口自己不读配置、不补默认值；错误一律
// 原样上抛，分类（412 / 409 / 503 …）与文案映射归 web 层（约束 43）。
// agentAdminPort adapts the write orchestrator to the web port. Both conversions are
// shape-only, the author config is frozen at assembly, and errors pass through verbatim.
type agentAdminPort struct {
	admin  *AgentAdmin
	author core.AuthorConfig
}

// 编译期断言：端口方法一旦与端口形状对不上，在这里先红（与 ports.go 同一纪律）。
// Compile-time assertion: every method must match the consumer-side port exactly.
var _ web.AgentAdminService = agentAdminPort{}

// Create 转调创建编排并投影受管文件集合。
// Create forwards to the create orchestration and projects the managed file set.
func (p agentAdminPort) Create(ctx context.Context, actor core.Actor, spec core.AgentSpec) (web.AgentFileSet, error) {
	view, err := p.admin.Create(ctx, actor, spec)
	if err != nil {
		return web.AgentFileSet{}, err
	}
	return agentFileSet(view), nil
}

// GenerateFiles 用装配期冻结的作者配置生成内存草稿或澄清问题（不落盘、不注册）。
// GenerateFiles drafts in memory with the frozen author config; nothing is written.
func (p agentAdminPort) GenerateFiles(ctx context.Context, name, description string,
	answers []core.AuthorAnswer,
) (web.AgentDraftResult, error) {
	draft, err := p.admin.GenerateFiles(ctx, p.author, name, description, answers)
	if err != nil {
		return web.AgentDraftResult{}, err
	}
	return web.AgentDraftResult{Agent: draft.Agent, Files: draft.Files, Questions: draft.Questions}, nil
}

// ScheduleDraft 用装配期冻结的作者配置生成未保存的定时候选与服务端计算的触发时间。
// ScheduleDraft returns the unsaved candidate and the server-computed triggers.
func (p agentAdminPort) ScheduleDraft(ctx context.Context, name, instruction string) (web.ScheduleDraftResult, error) {
	draft, err := p.admin.ScheduleDraft(ctx, p.author, name, instruction)
	if err != nil {
		return web.ScheduleDraftResult{}, err
	}
	return web.ScheduleDraftResult{Agent: name, Candidate: draft.Candidate, NextRuns: draft.NextRuns}, nil
}

// Files 转调受管文件读取并投影集合 ETag。
// Files forwards the managed-file read including the collection ETag.
func (p agentAdminPort) Files(ctx context.Context, name string) (web.AgentFileSet, error) {
	view, err := p.admin.Files(ctx, name)
	if err != nil {
		return web.AgentFileSet{}, err
	}
	return agentFileSet(view), nil
}

// SubmitFiles 转调条件多文件提交（If-Match 原样传入，不在本层归一化）。
// SubmitFiles forwards the conditional submit; If-Match stays verbatim.
func (p agentAdminPort) SubmitFiles(ctx context.Context, actor core.Actor, name, ifMatch string,
	files []core.AuthorFile,
) (web.AgentFileSet, error) {
	view, err := p.admin.SubmitFiles(ctx, actor, name, ifMatch, files)
	if err != nil {
		return web.AgentFileSet{}, err
	}
	return agentFileSet(view), nil
}

// Copy 转调复制编排并投影受管文件集合；`resource_mapping` 原样透传（显式映射的
// 解析归编排层，本层不按名称补映射）。
// Copy forwards the copy orchestration; the explicit mapping passes through verbatim.
func (p agentAdminPort) Copy(ctx context.Context, actor core.Actor, source, target string,
	mapping []core.ResourceMapping,
) (web.AgentFileSet, error) {
	view, err := p.admin.Copy(ctx, actor, source, target, mapping)
	if err != nil {
		return web.AgentFileSet{}, err
	}
	return agentFileSet(view), nil
}

// Archive 转调归档编排（注销 / 等待 / 回滚矩阵全在编排层，本层不补步骤）。
// Archive forwards the archive orchestration.
func (p agentAdminPort) Archive(ctx context.Context, actor core.Actor, name string) (core.AgentArchiveRecord, error) {
	return p.admin.Archive(ctx, actor, name)
}

// Restore 转调恢复编排并投影受管文件集合。
// Restore forwards the restore orchestration.
func (p agentAdminPort) Restore(ctx context.Context, actor core.Actor, archiveID, target string) (web.AgentFileSet, error) {
	view, err := p.admin.Restore(ctx, actor, archiveID, target)
	if err != nil {
		return web.AgentFileSet{}, err
	}
	return agentFileSet(view), nil
}

// Archived 转调归档列表：CLI 与 HTTP 写下的行同在 `agent_archives` 一张表，本层不区分
// 写入来源（约束 27 的「CLI 归档在管理台可见」就落在这条读路径上）。
// Archived forwards the archive list; rows written by the CLI and by HTTP share one table.
func (p agentAdminPort) Archived(ctx context.Context) ([]core.AgentArchiveRecord, error) {
	return p.admin.Archived(ctx)
}

// agentFileSet 是写面视图到 web 端口的形状转换：字段一一对应，不重算 ETag、不重读磁盘。
// agentFileSet converts the write-surface view into the web port shape without recomputing.
func agentFileSet(view AgentFilesView) web.AgentFileSet {
	return web.AgentFileSet{
		Files: core.AgentFiles{
			Agent:     view.Agent,
			Lifecycle: view.Lifecycle,
			Files:     view.Files,
		},
		ETag: view.ETag,
	}
}

// mcpCatalogPort 把装配期解析好的只读 MCP 目录快照适配成 `web.MCPCatalogReader`
// （US-14 D-01）。它只有一份内存数据：请求期不重读文件、不联网、不启动进程、不写
// 操作行——目录是"配方建议"，不是运行配置。解析与校验已在装配期完成，因此这里也
// 没有"读失败"的分支；快照一旦不可解析，启动就已经失败关闭。
// 调用方**不得**改写返回值里的切片：`Items` 是共享快照，视图层只做投影。
// mcpCatalogPort serves the snapshot parsed at assembly. Callers must not mutate the
// returned slices: the items are shared and the view layer only projects them.
type mcpCatalogPort struct {
	catalog core.MCPCatalog
}

// 编译期断言：目录端口形状一旦变化，在这里先红。
// Compile-time assertion: the catalog port shape is fixed at this line.
var _ web.MCPCatalogReader = mcpCatalogPort{}

// ListMCPCatalog 返回整份快照（已按 id 升序，排序由解析层保证，本层不再排序）。
// ListMCPCatalog returns the whole snapshot; ordering is already guaranteed upstream.
func (p mcpCatalogPort) ListMCPCatalog(context.Context) (core.MCPCatalog, error) {
	return p.catalog, nil
}

// resourceAdminPort 把 `*ResourceAdmin` 适配成 `web.ResourceAdminService`（`writeServices.resourceAdmin`
// 的唯一实现，§3.1.8 第 2 个端口）。全部方法只做形状投影与转调：`Versions`
// 把版本行补上指针状态与撤回来源（与 `VersionDetail` 同一派生口径，**不**在 web 层
// 重算），模型连接与 MCP 写入后经只读投影回读一次视图（写后重读，不做乐观更新）；
// 技能四方法把线上文件草稿投影成作者文件并回投影 `SkillDetail`，校验只转调只读裁决。
// resourceAdminPort adapts the resource write surface to the web port. Every method
// is a shape-only forward; skill drafts are projected into author files here.
type resourceAdminPort struct {
	admin *ResourceAdmin
	// read 是 `Options.Resources` 的同一只读投影（同一构造、同一批存储）：写入成功后
	// 经它回读视图，保证响应与随后 GET 读到的是同一口径。
	// read is the same stateless read projection used by Options.Resources.
	read web.ResourceReader
}

// 编译期断言：15 个方法一旦与端口形状对不上，在这里先红（与 agentAdminPort 同一纪律）。
// Compile-time assertion: all fifteen methods must match the consumer-side port.
var _ web.ResourceAdminService = resourceAdminPort{}

// UpsertModelConnection 把线上请求投影成编排层请求：空 `ifMatch` 表示创建（POST），
// 非空表示条件修改（PUT）；发布成功后按资源标识回读一次发布视图。
// UpsertModelConnection maps the wire body to the orchestration request; an empty
// ifMatch means create, and the published view is re-read afterwards.
func (p resourceAdminPort) UpsertModelConnection(ctx context.Context, actor core.Actor,
	write web.ModelConnectionWrite, ifMatch string,
) (web.ModelConnectionView, error) {
	req := ModelConnectionRequest{
		Name:           write.Name,
		BaseURL:        write.BaseURL,
		DefaultModel:   write.DefaultModel,
		Create:         ifMatch == "",
		IfMatch:        ifMatch,
		CredentialMode: write.CredentialMode,
		Scope:          write.Scope,
		OwnerAgent:     write.OwnerAgent,
	}
	// 字段省略（指针 nil）= 保留；出现即原样下传，"空串既不是保留也不是清除"由编排层判。
	if write.APIKey != nil {
		req.APIKey = *write.APIKey
		req.APIKeySet = true
	}
	version, err := p.admin.UpsertModelConnection(ctx, actor, req)
	if err != nil {
		return web.ModelConnectionView{}, err
	}
	return p.read.ModelConnection(ctx, version.ResourceID)
}

// UpsertMCPServer 与模型连接同一形状：空 `ifMatch` 表示创建，非空表示条件修改；
// 发布成功后按资源标识回读一次视图。回读走详情投影并只取 `View`——写响应与随后
// `GET /mcp-servers/{name}` 的响应体同形（ETag 只走响应头）。
// UpsertMCPServer follows the model-connection shape; the detail projection is re-read
// after the publish and only its view is returned, so the body matches the GET.
func (p resourceAdminPort) UpsertMCPServer(ctx context.Context, actor core.Actor,
	write web.MCPServerWrite, ifMatch string,
) (web.McpServerView, error) {
	credentials := make([]MCPCredentialRequest, len(write.Credentials))
	for i, decl := range write.Credentials {
		credentials[i] = MCPCredentialRequest{Key: decl.Key, Mode: decl.Mode, Value: decl.Value}
	}
	version, err := p.admin.UpsertMCPServer(ctx, actor, MCPServerRequest{
		Name:        write.Name,
		Transport:   write.Transport,
		Command:     write.Command,
		Args:        write.Args,
		URL:         write.URL,
		Env:         write.Env,
		Headers:     write.Headers,
		Credentials: credentials,
		Create:      ifMatch == "",
		IfMatch:     ifMatch,
	})
	if err != nil {
		return web.McpServerView{}, err
	}
	detail, err := p.read.McpServer(ctx, version.ResourceID)
	if err != nil {
		return web.McpServerView{}, err
	}
	return detail.View, nil
}

// skillFilesFromDrafts 把线上文件草稿投影成作者文件：路径与正文逐字保留，边界
// （相对路径、隐藏段、大小、数量、大小写重复）由工作空间的同一校验函数裁决。
// skillFilesFromDrafts projects wire drafts verbatim; the workspace gate decides
// every boundary.
func skillFilesFromDrafts(drafts []web.FileDraftWrite) []core.AuthorFile {
	files := make([]core.AuthorFile, len(drafts))
	for i, draft := range drafts {
		files[i] = core.AuthorFile{Path: draft.Path, Content: draft.Content}
	}
	return files
}

// skillDetailView 把编排层的写结果投影成契约 `SkillDetail`：文件清单给出路径 /
// 字节数 / sha256 / 正文，`file_count` 取清单长度（与只读面同一派生口径，不在 web
// 层重算哈希或 ETag）。
// skillDetailView projects the write result into the contract shape; file_count is
// the manifest length and nothing is recomputed here.
func skillDetailView(detail SkillDetail) web.SkillDetailView {
	files := make([]web.SkillFileView, len(detail.Files))
	for i, file := range detail.Files {
		files[i] = web.SkillFileView{
			Path:    file.Path,
			Size:    file.Size,
			SHA256:  file.SHA256,
			Content: string(file.Content),
		}
	}
	return web.SkillDetailView{
		SkillView: web.SkillView{
			Name:         detail.Name,
			Description:  detail.Description,
			Path:         detail.Path,
			VersionID:    detail.VersionID,
			VersionNo:    detail.VersionNo,
			ContentHash:  detail.ContentHash,
			ETag:         detail.ETag,
			FileCount:    len(detail.Files),
			ReferencedBy: detail.ReferencedBy,
		},
		Files: files,
	}
}

// CreateSkill 转调骨架创建（最小 SKILL.md 由编排层渲染，不信任调用方正文）。
// CreateSkill forwards skeleton creation; the orchestration renders the card.
func (p resourceAdminPort) CreateSkill(ctx context.Context, actor core.Actor,
	write web.SkillCreateWrite,
) (web.SkillDetailView, error) {
	detail, err := p.admin.CreateSkill(ctx, actor, SkillCreateRequest{
		Name:        write.Name,
		Description: write.Description,
	})
	if err != nil {
		return web.SkillDetailView{}, err
	}
	return skillDetailView(detail), nil
}

// ImportSkill 转调目录导入（文件集合原样交边界校验）。
// ImportSkill forwards the directory import; the file set stays verbatim.
func (p resourceAdminPort) ImportSkill(ctx context.Context, actor core.Actor,
	write web.SkillImportWrite,
) (web.SkillDetailView, error) {
	detail, err := p.admin.ImportSkill(ctx, actor, SkillImportRequest{
		Name:  write.Name,
		Files: skillFilesFromDrafts(write.Files),
	})
	if err != nil {
		return web.SkillDetailView{}, err
	}
	return skillDetailView(detail), nil
}

// UpdateSkill 转调全量替换：名字取路径、If-Match 逐字传入（缺失由编排层按 412 拒绝）。
// UpdateSkill forwards the full replacement; name and If-Match stay verbatim.
func (p resourceAdminPort) UpdateSkill(ctx context.Context, actor core.Actor, name string,
	write web.SkillUpdateWrite, ifMatch string,
) (web.SkillDetailView, error) {
	detail, err := p.admin.UpdateSkill(ctx, actor, SkillUpdateRequest{
		Name:    name,
		Files:   skillFilesFromDrafts(write.Files),
		IfMatch: ifMatch,
	})
	if err != nil {
		return web.SkillDetailView{}, err
	}
	return skillDetailView(detail), nil
}

// ValidateSkill 转调只读校验：与导入共用同一校验函数，不落盘、不写操作行、不执行脚本。
// ValidateSkill forwards the read-only validation sharing the import gate.
func (p resourceAdminPort) ValidateSkill(ctx context.Context, name string,
	write web.SkillUpdateWrite,
) (core.SkillValidateResult, error) {
	return p.admin.ValidateSkill(ctx, name, SkillUpdateRequest{
		Name:  name,
		Files: skillFilesFromDrafts(write.Files),
	})
}

// Delete 转调删除编排：引用闸门、块移除与事务都在编排层，本层不补步骤。
// Delete forwards to the orchestration; the gate and transaction stay there.
func (p resourceAdminPort) Delete(ctx context.Context, actor core.Actor, kind core.ResourceKind, id, ifMatch,
	resourceUID string,
) error {
	return p.admin.Delete(ctx, actor, kind, id, ifMatch, resourceUID)
}

// Publish 转调发布编排（发布最新 / 显式撤回两种语义都在编排层）。
// Publish forwards the publish orchestration, including explicit revert.
func (p resourceAdminPort) Publish(ctx context.Context, actor core.Actor,
	req core.ResourcePublishRequest,
) (core.ResourcePublishResult, error) {
	return p.admin.Publish(ctx, actor, req)
}

// CurrentETag 转调条件 ETag 的唯一服务端读取点（形状级 412 用它给出当前值）。
// CurrentETag forwards the single server-side conditional-ETag reader.
func (p resourceAdminPort) CurrentETag(ctx context.Context, kind core.ResourceKind, id string) (string, bool) {
	return p.admin.CurrentETag(ctx, kind, id)
}

// Versions 返回带指针状态与撤回来源的版本列表（与 `VersionDetail` 同一派生口径）。
// Versions returns the version list with the same derived fields as VersionDetail.
func (p resourceAdminPort) Versions(ctx context.Context, kind core.ResourceKind, id string) ([]core.ResourceVersionDetail, error) {
	return p.admin.versionDetails(ctx, kind, id)
}

// VersionDetail 转调只读版本详情（脱敏与 fail-closed 在编排层完成）。
// VersionDetail forwards the read-only detail; redaction already happened upstream.
func (p resourceAdminPort) VersionDetail(ctx context.Context, versionID int64) (core.ResourceVersionDetail, error) {
	return p.admin.VersionDetail(ctx, versionID)
}

// References 转调引用联合视图（活动 Agent 快照 + 归档扫描 + 未释放运行引用）。
// References forwards the reference union view.
func (p resourceAdminPort) References(ctx context.Context, kind core.ResourceKind, id string) (core.ResourceReferences, error) {
	return p.admin.References(ctx, kind, id)
}

// TestModelConnection 转调一次性连接测试（不建 Run、不换运行时映射）。
// TestModelConnection forwards the one-shot connection test.
func (p resourceAdminPort) TestModelConnection(ctx context.Context, actor core.Actor, name string) (core.ResourceTestResult, error) {
	return p.admin.TestModelConnection(ctx, actor, name)
}

// TestMCPServer 转调一次性连接测试（按已发布版本、不建 Run、不写 llm_calls）。
// TestMCPServer forwards the one-shot connection test against the published version.
func (p resourceAdminPort) TestMCPServer(ctx context.Context, actor core.Actor, name string) (core.ResourceTestResult, error) {
	return p.admin.TestMCPServer(ctx, actor, name)
}

// ApplyMCPServer 转调受控发布（14D）；预写、冻结、预检与回滚都在编排层，本层只转手。
// ApplyMCPServer forwards the controlled apply; every guarantee lives in the orchestrator.
func (p resourceAdminPort) ApplyMCPServer(ctx context.Context, actor core.Actor, name, ifMatch string,
	versionID int64,
) (core.MCPServerApplyResult, error) {
	return p.admin.ApplyMCPServer(ctx, actor, name, ifMatch, versionID)
}

// DiagnoseMCPServer 转调只读诊断（US-14 D-02）；本层不做任何补写或缓存——诊断的
// "零进程、零操作行"由编排层保证，适配器只转手。
// DiagnoseMCPServer forwards the read-only diagnosis untouched.
func (p resourceAdminPort) DiagnoseMCPServer(ctx context.Context, actor core.Actor,
	name string,
) (core.MCPServerDiagnosis, error) {
	return p.admin.DiagnoseMCPServer(ctx, actor, name)
}

// notifyAdminPort 把 `*NotifyAdmin` 适配成 `web.NotifyAdminService`（`writeServices.notifyAdmin`
// 的唯一实现，§3.1.8 第 3 个端口）：三个方法只做形状投影与转调——请求投影成
// `NotifyChannelRequest`，成功后回传**存储行**并经管理读面补齐（见 `echo`），URL 保持
// **库里原样值**（精确 `${VAR}` 占位或空串）；解析后的地址不进入本层，host 投影仍只在
// web 层 `notifyChannelView` 做一次（约束 38 的凭证红线）。错误一律原样上抛，分类与
// 文案映射归 web 层（约束 43）。
// notifyAdminPort adapts the channel write surface to the web port: the request maps to
// NotifyChannelRequest, and the success echo is the stored row enriched by the admin read
// face — the host projection stays in web.
type notifyAdminPort struct {
	admin *NotifyAdmin
	// read 是写后回显用的管理读面：补凭据状态 / 属主名 / 条件 ETag。写成功后它读失败
	// **不得**把写入说成失败——退回编排层已回读的行（缺 ETag，页面随后重新 GET）。
	// read enriches the post-write echo; a read failure never turns a successful write
	// into an error.
	read *notifyChannelReadPort
}

// 编译期断言：三方法一旦与端口形状对不上，在这里先红（与其余三个端口同一纪律）。
// Compile-time assertion: all three methods must match the consumer-side port.
var _ web.NotifyAdminService = notifyAdminPort{}

// Create 转调创建编排；同名 400、占位与白名单预检都在编排层完成。
// Create forwards to the create orchestration; duplicate-name 400s and placeholder
// checks stay upstream.
func (p notifyAdminPort) Create(ctx context.Context, actor core.Actor,
	write web.NotifyChannelWrite,
) (core.NotifyChannel, error) {
	stored, err := p.admin.Create(ctx, actor, notifyChannelRequest(write))
	if err != nil {
		return core.NotifyChannel{}, err
	}
	return p.echo(ctx, stored), nil
}

// Update 转调更新编排：空 `url` 原样下传，是否保留原值由编排层在写前解释；
// 成功后的回显同样走 `echo`。
// Update forwards the update; a blank url stays verbatim and the echo uses the same path.
func (p notifyAdminPort) Update(ctx context.Context, actor core.Actor, name string,
	write web.NotifyChannelWrite,
) (core.NotifyChannel, error) {
	stored, err := p.admin.Update(ctx, actor, name, notifyChannelRequest(write))
	if err != nil {
		return core.NotifyChannel{}, err
	}
	return p.echo(ctx, stored), nil
}

// Delete 转调整词引用闸门后的删除（`force` 由 web 层按字面量解析后传入，
// `resourceUID` 原样下传，由编排层在预写前与当前指针比较）。
// Delete forwards the delete gate; force was parsed in web and resourceUID is
// compared against the current pointer in the adapter.
func (p notifyAdminPort) Delete(ctx context.Context, actor core.Actor, name, resourceUID string, force bool) error {
	return p.admin.Delete(ctx, actor, name, resourceUID, force)
}

// TestChannel 转调渠道连通性测试：预写管理操作、真实发送与三值结果都在编排层，
// 本层只透传；测试结果同时写「最后测试」投影，列表页随后即可读到。
// TestChannel forwards to the connectivity test; the pre-write, the real send and the
// last-test projection all stay in the orchestrator.
func (p notifyAdminPort) TestChannel(ctx context.Context, actor core.Actor,
	name string,
) (core.NotifyChannelTestResult, error) {
	return p.admin.TestChannel(ctx, actor, name)
}

// notifyChannelRequest 把线上请求投影成编排层请求：字段逐字保留，名称 / 类型 / 占位
// 的合法性与默认停用语义都由编排层裁决。`url` 省略（指针 nil）= 保留原值，出现即
// 原样下传（空串 → 编排层 400）。
// notifyChannelRequest projects the wire body field by field; validation stays upstream.
func notifyChannelRequest(write web.NotifyChannelWrite) NotifyChannelRequest {
	req := NotifyChannelRequest{
		Name:           write.Name,
		Type:           write.Type,
		Description:    write.Description,
		CredentialMode: write.CredentialMode,
		Scope:          write.Scope,
		OwnerAgent:     write.OwnerAgent,
	}
	if write.URL != nil {
		req.URL = *write.URL
		req.URLSet = true
	}
	return req
}

// echo 是写成功后的回显：管理读面补齐属主名 / 凭据状态 / 条件 ETag，页面拿到的字段与
// 随后 GET 完全一致。读面失败**只记日志**并退回编排层已回读的存储行（缺 ETag）——
// 写入已生效，把读失败说成写失败会让页面重试一次已经成功的创建（触发同名 400）。
// echo enriches a successful write's stored row; a read failure is logged and falls back
// to the stored row so a committed write is never reported as failed.
func (p notifyAdminPort) echo(ctx context.Context, stored core.NotifyChannel) core.NotifyChannel {
	if p.read == nil {
		return stored
	}
	enriched, err := p.read.Channel(ctx, stored.Name)
	if err != nil {
		p.admin.logger.Error("通知渠道写后补齐失败（写入已生效）",
			"channel", stored.Name, "error", err)
		return stored
	}
	return enriched
}

// resourceStatePort 把 `*ResourceStateService` 适配成 `web.ResourceStateService`
// （`writeServices.resourceState` 的唯一实现）：三个方法只做形状投影——线上请求体
// 投影成领域命令、领域结论投影成契约视图。`credential_state` 在成功路径上恒为
// `revoked`（撤销端点只会在撤销成功后返回）；`affected_eligible_agents` 在这里
// 兜底成非 nil，保证契约数组形状。身份 / ETag 比较、预写与终态回填都在编排层，
// 本适配器不重做校验；错误一律原样上抛，分类与文案映射归 web 层（约束 43）。
// resourceStatePort adapts the state orchestrator to the web port. It only projects
// shapes: the wire body becomes a command, the outcome becomes a contract view.
type resourceStatePort struct {
	state *ResourceStateService
}

// 编译期断言：三方法一旦与端口形状对不上，在这里先红（与其余端口同一纪律）。
// Compile-time assertion: all three methods must match the consumer-side port.
var _ web.ResourceStateService = resourceStatePort{}

// CurrentETag 转调条件 ETag 的只读解析（形状级 412 的 `data.current_etag`）。
// CurrentETag forwards the read-only conditional ETag lookup.
func (p resourceStatePort) CurrentETag(ctx context.Context, kind, name string) (string, bool) {
	return p.state.CurrentETag(ctx, kind, name)
}

// RevokeCredential 转调撤销编排并投影契约结果：`credential_ref` 是服务端生成的
// 凭据身份引用（不是凭据材料），`revoked_versions` 是撤销后的全部版本数。
// RevokeCredential forwards the revoke and projects the contract result.
func (p resourceStatePort) RevokeCredential(ctx context.Context, actor core.Actor,
	kind, name string, write web.CredentialRevokeWrite, ifMatch string,
) (web.CredentialRevokeView, error) {
	out, err := p.state.RevokeCredential(ctx, actor, CredentialRevokeCommand{
		Kind:        kind,
		Name:        name,
		ResourceUID: write.ResourceUID,
		BindingKey:  write.BindingKey,
		Confirm:     write.Confirm,
		IfMatch:     ifMatch,
	})
	if err != nil {
		return web.CredentialRevokeView{}, err
	}
	return web.CredentialRevokeView{
		ResourceUID:     string(out.ResourceUID),
		BindingKey:      out.BindingKey,
		CredentialRef:   out.CredentialID,
		CredentialState: string(core.CredentialIdentityRevoked),
		RevokedVersions: out.RevokedVersions,
		RevocationEpoch: out.RevocationEpoch,
		ETag:            out.ETag,
	}, nil
}

// ConvertScope 转调范围切换编排并投影契约结果：`etag` 同时进响应体与响应头；
// `affected_eligible_agents` 空集合也输出 `[]`。
// ConvertScope forwards the conversion; an empty impact hint still renders as [].
func (p resourceStatePort) ConvertScope(ctx context.Context, actor core.Actor,
	kind, name string, write web.ResourceScopeWrite, ifMatch string,
) (web.ResourceScopeView, error) {
	out, err := p.state.ConvertScope(ctx, actor, ResourceScopeCommand{
		Kind:        kind,
		Name:        name,
		ResourceUID: write.ResourceUID,
		Scope:       write.Scope,
		OwnerAgent:  write.OwnerAgent,
		IfMatch:     ifMatch,
	})
	if err != nil {
		return web.ResourceScopeView{}, err
	}
	affected := out.AffectedEligibleAgents
	if affected == nil {
		affected = []string{}
	}
	return web.ResourceScopeView{
		ResourceUID:            string(out.ResourceUID),
		Scope:                  string(out.Scope),
		OwnerAgent:             out.OwnerAgent,
		Revision:               out.Revision,
		ETag:                   out.ETag,
		AffectedEligibleAgents: affected,
	}, nil
}

// workspaceBrowsePort 把 `*WorkspaceBrowse` 适配成 `web.WorkspaceService`
// （`writeServices.workspace` 的唯一实现，§3.1.8 第 4 个端口）：两个方法只做形状
// 投影——领域条目投影成契约视图，`truncated` 恒 false（超限是错误、不是截断）。
// 路径规则、真实路径投影、残骸隐藏与 256 KiB 上限都在 `WorkspaceBrowse` 里，
// 本适配器不重做校验；错误一律原样上抛，分类与文案映射归 web 层（约束 43）。
// 名字带 Browse 以区别于 `ports.go` 里适配 `cli.Workspace` 的 `workspacePort`。
// workspaceBrowsePort adapts the read-only browser to the web port. Both conversions
// are shape-only: truncated is always false because oversized files fail instead of cut.
type workspaceBrowsePort struct {
	browse *WorkspaceBrowse
}

// 编译期断言：两方法一旦与端口形状对不上，在这里先红（与其余三个端口同一纪律）。
// Compile-time assertion: both methods must match the consumer-side port.
var _ web.WorkspaceService = workspaceBrowsePort{}

// Tree 转调目录树并投影条目：路径原样下传，空树保持 nil（web 层用
// `nonNilWorkspaceTree` 输出 `[]`）。
// Tree forwards the tree request and projects the entries verbatim.
func (p workspaceBrowsePort) Tree(ctx context.Context, path string) (web.WorkspaceTreeView, error) {
	entries, err := p.browse.Tree(ctx, path)
	if err != nil {
		return nil, err
	}
	views := make(web.WorkspaceTreeView, len(entries))
	for i, entry := range entries {
		views[i] = web.WorkspaceEntryView{Path: entry.Path, Type: entry.Type, Size: entry.Size}
	}
	return views, nil
}

// File 转调单文件预览；`truncated` 由契约固定 false。
// File forwards one preview; the contract fixes truncated at false.
func (p workspaceBrowsePort) File(ctx context.Context, path string) (web.WorkspaceFileView, error) {
	file, err := p.browse.File(ctx, path)
	if err != nil {
		return web.WorkspaceFileView{}, err
	}
	return web.WorkspaceFileView{
		Path:      file.Path,
		Content:   file.Content,
		Size:      file.Size,
		Truncated: false,
	}, nil
}

// instanceSecretsPort 把 `*InstanceSecretsService` 适配成 `web.InstanceSecretsService`
// （14D / E9）：只做形状投影，判定的语义全在服务层。
//
// 两处不是手误、而是**必须**的差异：
//   - `CanInitialize` 直接取服务层的完整前置条件（判定允许 **且** 已有路径或能在
//     本次动作里建立身份与路径）。存量无名册实例因此**可以**从页面初始化；显式
//     `auto_init: false` 的纯 env 部署仍是 false，处理器在**预写之前**给出 409。
//   - `Init` 的第二个返回值取 `SecretsInitError.OperationID`：只有预写之后的失败才
//     带标识，处理器据此决定 500 是否携带 `data.operation_id`；预写之前的拒绝
//     （ErrCredentialUnavailable 等）保持空串，因而不会出现"有标识却没有操作行"。
//
// instanceSecretsPort is shape-only. CanInitialize is the service's full precondition,
// and the Init operation id comes only from SecretsInitError.
type instanceSecretsPort struct {
	service *InstanceSecretsService
}

// 编译期断言：两方法一旦与端口形状对不上，在这里先红。
// Compile-time assertion: both methods must match the consumer-side port.
var _ web.InstanceSecretsService = instanceSecretsPort{}

// instanceSecretsPortOf 把可空的装配产物投影成端口：服务缺席时返回 nil 接口
// （不是带 nil 指针的非 nil 接口——那会让处理器的缺端口判定失效）。
// instanceSecretsPortOf returns a nil interface when the service is absent.
func instanceSecretsPortOf(service *InstanceSecretsService) web.InstanceSecretsService {
	if service == nil {
		return nil
	}
	return instanceSecretsPort{service: service}
}

// Status 投影判定结论、公开密钥身份与三件只读事实；不含指纹与密钥材料。
// Status projects the verdict and the public identity summary, never key material.
func (p instanceSecretsPort) Status(ctx context.Context) (web.InstanceSecretsStatus, error) {
	status, err := p.service.Status(ctx)
	if err != nil {
		return web.InstanceSecretsStatus{}, err
	}
	return web.InstanceSecretsStatus{
		State:         string(status.Decision.State),
		Reason:        string(status.Decision.Reason),
		KeyID:         status.KeyID,
		KeyFile:       status.KeyFile,
		InitializedAt: status.InitializedAt,
		CanInitialize: status.CanInitialize,
	}, nil
}

// Init 转调显式初始化，并把预写之后的失败标识交还 web 层。
// Init forwards the explicit initialization and surfaces the post-pre-write id.
func (p instanceSecretsPort) Init(ctx context.Context, actor core.Actor) (string, error) {
	// 成功时标识不需要回传：响应体是重新读出的投影，操作标识只服务于失败查证。
	// The operation id only matters on failure; success returns the re-read projection.
	if _, err := p.service.Init(ctx, actor); err != nil {
		var initErr *SecretsInitError
		if errors.As(err, &initErr) {
			return initErr.OperationID, initErr
		}
		return "", err
	}
	return "", nil
}

// wireManagementPorts 装配管理面七个端口（模块卡 §3.1.15 的 webOptions 行）：Registry /
// Lifecycle / Operations 各取装配期共享实例，Resources 与 System 是只读适配，Sampler 按
// 注入时钟构造，AgentAdmin 由内容编辑器与组合适配器组成。生产重路径上七个端口全部非
// nil；投影用例会构造半截运行期，缺句柄的端口不装配——handler 对缺端口按固定 500 拒绝，
// 而不是在装配期悄悄换成空实现（失败关闭，不伪造"可用"）。
// wireManagementPorts assembles the seven management-surface ports. The production
// heavy path supplies every handle; projection cases may pass a partial runtime, and a
// missing handle leaves its port unset so the handler fails closed with a fixed 500.
func (a *App) wireManagementPorts(opts *web.Options, rt *heavyRuntime) {
	if rt.agents != nil {
		opts.Registry = rt.agents
	}
	if rt.lifecycle != nil {
		opts.Lifecycle = rt.lifecycle
	}
	if rt.ops != nil {
		opts.Operations = rt.ops
	}
	if rt.versions != nil && rt.lifecycle != nil {
		opts.Resources = NewResourceViews(rt.versions, rt.lifecycle, rt.mcpRT, rt.credentials,
			ResourceViewDeps{ConnectionTests: rt.connectionTests})
	}
	if rt.db != nil && rt.runs != nil && rt.versions != nil {
		opts.System = newSystemReader(rt.db, rt.runs, rt.versions, a.nowOr())
	}
	opts.Sampler = web.NewSampler(a.nowOr())

	// 只读 MCP 目录与的写路径**无关**（它是快照，不是资源写面的一部分），因此在这道
	// 依赖完整性闸门之前注入：写面缺件不应连带把目录页打成 500。裸 nil 指针装进接口会
	// 变成"非 nil 接口 + nil 接收者"（与下方 scheduler 同一纪律），所以显式判空。
	// The read-only catalog is independent of the write surface, so it is projected
	// before the completeness gate; a nil pointer must not become a non-nil interface.
	if rt.mcpCatalog != nil {
		opts.MCPCatalog = rt.mcpCatalog
	}

	// AgentAdmin 需要完整写路径（编辑器 → 快照 / 操作 / 注册表 / 调度 + 装载期校验器）。
	// 缺任一依赖时不装配：内容端点按 500 拒绝比"写了一半再失败"安全。
	if rt.loader == nil || rt.root == "" || rt.agents == nil || rt.ops == nil ||
		rt.db == nil || rt.agentGate == nil || rt.lifecycle == nil || rt.versions == nil {
		return
	}
	// Scheduler 是按 Agent 对账的可选面：裸 nil 指针装进接口会变成"非 nil 接口 + nil
	// 接收者"，编辑器会误以为调度同步可用——这里显式只在非 nil 时注入。
	var scheduler workspace.ScheduleReconciler
	if rt.scheduler != nil {
		scheduler = rt.scheduler
	}
	now := a.nowOr()
	editor := workspace.NewAgentEditor(workspace.AgentEditorDeps{
		Root:      rt.root,
		Gate:      rt.agentGate,
		Ops:       rt.ops,
		Snapshots: storage.NewSnapshotStore(rt.db),
		Registry:  rt.agents,
		Scheduler: scheduler,
		// 候选校验走装载器的同一道规则；调度占用基线取**当前**注册表快照——热加载
		// 之后不能还拿装配时的 entries 判冲突。
		Validate: func(_ context.Context, agent string, candidate []byte) (core.AgentEntry, string, []core.ManifestEntry, error) {
			return rt.loader.ValidateCandidate(rt.root, rt.agents.Entries(), agent, candidate, now())
		},
		// 保存期绑定校验（卡 R4.3 的校验顺序：校验引用与绑定 → 原子写文件）：装载期
		// 只保证结构，资源状态在仓储里，因此由装配层注入与准入**同一份**实现。
		// The save-time binding check reuses the admission implementation.
		ValidateBindings: SaveBindingCheck(rt.lifecycle, BindingCheckDeps{
			Resources:      rt.versions,
			Notify:         rt.notifyAdminStore,
			NotifyChannels: storage.NewNotifyChannelRepo(rt.db),
			Credentials:    rt.resolver,
		}),
		// 受控工具装配（R7 / 宪法约束 11）：与准入读同一份渠道目录与同一份生命周期
		// 状态文件；系统只会增删 `notify`，且变更随保存结果回显。
		// The controlled tool assembly reads the same catalog and lifecycle store the
		// admission path uses; notify is the only name it may change.
		AssembleTools: assembleNotifyToolsFor(rt),
		Now:           now,
	})
	opts.AgentAdmin = NewAgentConsole(AgentConsoleDeps{
		Root:       rt.root,
		Projection: rt.projection,
		Editor:     editor,
		Lifecycle:  rt.lifecycle,
		Index:      rt.versions,
		Gate:       rt.agentGate,
		Operations: rt.ops,
		Logger:     a.loggerOr(),
		Now:        now,
	})
}

// systemReader 是 web.SystemReader 的生产适配：`storage/db.go` 的 Ping / SchemaVersion、
// `RunStore.CountRuns` 与三类发布指针计数。任一读失败整体返回错误——handler 据此把
// `database.status` 标 unavailable 并省略计数，**不得**把"读不到"填成 0。
// systemReader adapts the database and repositories behind the overview; any read
// failure fails the whole reading instead of fabricating zeros.
type systemReader struct {
	db       *storage.DB
	runs     *storage.RunStore
	versions *storage.ResourceStore
	now      func() time.Time
}

// 编译期断言：概览适配器满足 web 的消费方端口。
// Compile-time assertion that the adapter satisfies the consumer-side port.
var _ web.SystemReader = systemReader{}

// newSystemReader 组装概览读数适配器；now 为 nil 时退回 time.Now（生产恒传注入时钟）。
// newSystemReader builds the overview reader; a nil clock falls back to time.Now.
func newSystemReader(db *storage.DB, runs *storage.RunStore, versions *storage.ResourceStore, now func() time.Time) systemReader {
	if now == nil {
		now = time.Now
	}
	return systemReader{db: db, runs: runs, versions: versions, now: now}
}

// ReadOverview 汇总一次概览读数：探活 → 库版本 → 三个运行计数 → 三类发布指针计数。
// 顺序固定，任一失败即整体失败（部分读数会在页面上伪装成"其余为 0"）。
// ReadOverview reads the overview in a fixed order and fails as a whole on any error.
func (r systemReader) ReadOverview(ctx context.Context) (web.SystemReadings, error) {
	if r.db == nil || r.runs == nil || r.versions == nil {
		return web.SystemReadings{}, errors.New("系统概览读数缺少数据库 / 运行账本 / 资源仓储")
	}
	if err := r.db.Ping(ctx); err != nil {
		return web.SystemReadings{}, fmt.Errorf("系统概览探活: %w", err)
	}
	schemaVersion, err := r.db.SchemaVersion(ctx)
	if err != nil {
		return web.SystemReadings{}, fmt.Errorf("系统概览读取库版本: %w", err)
	}
	running, last24h, failedLast24h, err := r.runs.CountRuns(ctx, r.now())
	if err != nil {
		return web.SystemReadings{}, err
	}
	modelConnections, err := r.countPointers(ctx, core.ResourceModelConnection)
	if err != nil {
		return web.SystemReadings{}, err
	}
	mcpServers, err := r.countPointers(ctx, core.ResourceMCPServer)
	if err != nil {
		return web.SystemReadings{}, err
	}
	skills, err := r.countPointers(ctx, core.ResourceSkill)
	if err != nil {
		return web.SystemReadings{}, err
	}
	return web.SystemReadings{
		RunsRunning:       running,
		RunsLast24h:       last24h,
		RunsFailedLast24h: failedLast24h,
		ModelConnections:  modelConnections,
		MCPServers:        mcpServers,
		Skills:            skills,
		SchemaVersion:     schemaVersion,
	}, nil
}

// countPointers 统计一类资源的**未删除发布指针**数（与只读资源路由同一处数据源）。
// countPointers counts the live published pointers of one kind.
func (r systemReader) countPointers(ctx context.Context, kind core.ResourceKind) (int64, error) {
	pointers, err := r.versions.ListPointers(ctx, kind)
	if err != nil {
		return 0, fmt.Errorf("系统概览统计 %s: %w", kind, err)
	}
	return int64(len(pointers)), nil
}

// 沙箱页固定目录的**展示名**（相对 `.basalt` 根）：文件工具的可读根由代码常量决定
// （`tool/policy.go` 的 checkFileRead：本 Agent 目录 → 技能库 → 摘录经授权的跨
// Agent），这里不新增配置键、不改变策略，只把同一组规则翻成相对展示形态——绝对
// 路径不出站（约束 25 的口径）。
// Display names of the fixed read roots; the policy stays the single source of truth.
const (
	sandboxAgentDirDisplay    = ".basalt/agents/<agent>/"
	sandboxSkillsDirDisplay   = ".basalt/skills/"
	sandboxVersionsDirDisplay = ".basalt/versions/skills/"
)

// sandboxConfigView 把配置段与固定目录规则投影成 `/sandbox` 的白名单视图输入：
// 命令 / 解释器 / 域名原样搬运（空列表 = 全拒），路径先列固定三项，再逐个追加
// `sandbox.cross_agent_read` 授权的 Agent 目录。
// sandboxConfigView projects the configuration sections plus the fixed directory rules.
func sandboxConfigView(cfg *cli.Config) web.SandboxConfig {
	out := web.SandboxConfig{
		FileAllowedPaths: []string{
			sandboxAgentDirDisplay,
			sandboxSkillsDirDisplay,
			sandboxVersionsDirDisplay,
		},
		ShellAllowedCommands: append([]string(nil), cfg.Shell.AllowedCommands...),
		ShellInterpreters:    append([]string(nil), cfg.Shell.Interpreters...),
		HTTPAllowedDomains:   append([]string(nil), cfg.HTTP.AllowedDomains...),
	}
	for _, name := range cfg.Sandbox.CrossAgentRead {
		out.FileAllowedPaths = append(out.FileAllowedPaths, ".basalt/agents/"+name+"/")
	}
	return out
}

// notifyTypeNames 取装配层显式注册的渠道类型名（`newNotifyAdapters` 的键集），
// 排序后作为 `/notify-channels/types` 的固定顺序——**一处真值**：US-12 的写侧
// 校验必须复用同一张映射表，不得另建类型清单。
// notifyTypeNames returns the sorted key set of the single adapter registry.
func notifyTypeNames() []string {
	return slices.Sorted(maps.Keys(newNotifyAdapters()))
}

// webBudget 把配置的预算段映射成运行预算（约束 35）——与 `cli.chatBudget` 同一口径：
// 缺省值**不在本层补**（运行入口的 mergeBudget 负责），`max_output_bytes` 仅当
// 0 < v ≤ math.MaxInt 时搬运；cfg 为 nil → 全零预算。
// webBudget mirrors cli.chatBudget: no defaults are filled here, and an out-of-range
// max_output_bytes is dropped rather than truncated.
func webBudget(cfg *cli.Config) core.Budget {
	if cfg == nil {
		return core.Budget{}
	}
	budget := core.Budget{
		MaxIterations: cfg.Budget.MaxIterations,
		MaxToolCalls:  cfg.Budget.MaxToolCalls,
		ContextTokens: cfg.Budget.ContextTokens,
	}
	if v := cfg.Budget.MaxOutputBytes; v > 0 && v <= math.MaxInt {
		budget.MaxOutputBytes = int(v)
	}
	return budget
}

// ensureWorkspaceForServe 是 serve 的幂等兜底初始化（技术方案 §3.1.1）：`basalt init`
// 不再是启动前的人工前置——serve 先建齐骨架再装配，二次启动逐字节不动既有文件。
// ensureWorkspaceForServe makes `basalt init` unnecessary before serve; a second
// start leaves every existing file byte-for-byte untouched.
func ensureWorkspaceForServe(root string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	report, err := workspace.Init(root)
	if err != nil {
		return fmt.Errorf("serve 兜底初始化工作区: %w", err)
	}
	// 一条 INFO 说清"这次启动建了什么、保留了什么"（巡检要对得上磁盘）。
	// One INFO answers what this start created and what it left untouched.
	logger.Info("serve 兜底初始化完成", "root", root,
		"created", report.Created, "kept", report.Kept)
	return nil
}

// webProviders 按配置顺序产出 /info 的 provider 摘要（已决 #4）：`models` 只含
// `default_model`、空则不列；`configured` 只看 base_url 与 api_key 是否**都非空**
// （约束 25）——**不得**回显凭证或 base_url 本身。
// webProviders lists one summary per configured provider, in configuration order.
func webProviders(cfg *cli.Config) []web.ProviderInfo {
	if cfg == nil {
		return []web.ProviderInfo{}
	}
	out := make([]web.ProviderInfo, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		models := []string{}
		if p.DefaultModel != "" {
			models = append(models, p.DefaultModel)
		}
		out = append(out, web.ProviderInfo{
			Name:       p.Name,
			Models:     models,
			Configured: p.BaseURL != "" && p.APIKey != "",
		})
	}
	return out
}

// memoryBackendName 归一化 memory.backend（约束 28）：空 → markdown。装载期已把空串
// 写回缺省，这里是装配层读**直接构造的** Config 时的第二处同口径落点。
// memoryBackendName normalizes an empty backend to markdown.
func memoryBackendName(cfg *cli.Config) string {
	if cfg == nil || cfg.Memory.Backend == "" {
		return "markdown"
	}
	return cfg.Memory.Backend
}

// agentProfilesPort 是 `/profiles` 的适配器（约束 39）：**已注册条目每次读都取自
// 注册表当前快照**——热加载、内容写与启停之后，页面与准入、调度器看到的是同一份
// 数据；`entries` 只补装配期装载结果里的**未注册项**，它们的拒因是 problems 的唯一
// 来源（约束 26），而注册表按注册不变量不持有它们。同名时注册表优先：注册一旦成立，
// 旧拒因不得再展示；被删除的 Agent 因注册表已收回而**不得**从装配期快照里复活。
// agentProfilesPort serves registered entries from the live registry on every read and
// only appends assembly-time rejects; once registered, a stale reject disappears.
type agentProfilesPort struct {
	registry *agentRegistry
	entries  []core.AgentEntry
}

// Profiles 返回按 name 升序的合并视图：注册表条目在前（其派生顺序即 name 序），
// 未注册且未后来注册成功的装配期条目补入后统一重排。registry 为 nil 时（只读投影
// 或直接构造的用例）退回装载结果本身。
// Profiles merges live registered entries with still-unregistered assembly-time entries,
// sorted by name; a nil registry falls back to the raw loader result.
func (p agentProfilesPort) Profiles() []core.AgentEntry {
	if p.registry == nil {
		return p.entries
	}
	registered := p.registry.Entries()
	merged := make([]core.AgentEntry, 0, len(registered)+len(p.entries))
	merged = append(merged, registered...)
	for _, entry := range p.entries {
		if entry.Registered {
			// 注册条目的权威来源是注册表：快照可能已被热加载替换或被移除。
			// Registered entries are owned by the registry, which may have replaced or
			// removed them since assembly.
			continue
		}
		if _, ok := p.registry.Lookup(entry.Name); ok {
			// 启动时被拒、后来校验通过：旧拒因不得再展示。
			// A formerly rejected agent that has since registered loses its stale reason.
			continue
		}
		merged = append(merged, entry)
	}
	slices.SortFunc(merged, func(a, b core.AgentEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	return merged
}

// toolCatalogPort 把注册池与 MCP 连接结果适配成工具清单（约束 27）：顺序 =
// `Registry.Names()`（排序序），来源判定 builtin 集 → builtin、MCP 连接池 → mcp、
// 其余 → code。builtin 集**必须**取自 `declaredToolNames`（十一件内置的唯一声明），
// 另写一份名单必然漂移。
// 14D 起来源优先取**运行期来源表**（受控重建会整组换掉某个 server 的工具名，装配期
// 快照会过期）；没有来源表时退回连接快照扫描——两条路径的判定口径一致。
// The source is read from the live disposition table (an apply rewrites it); the static
// connection scan remains the fallback for fixtures without a source table.
type toolCatalogPort struct {
	registry     *tool.Registry
	mcp          []tool.MCPConnection
	dispositions *mcpDispositionTable
	builtin      map[string]struct{}
}

// newToolCatalogPort 从注册池与运行期句柄构建清单端口；builtin 集在构造期一次成型
// （数据来源是本包的声明名集，不读注册池——"注册了"不等于"内置"）。runtime 可为
// nil（只读装配的用例），此时来源退回连接快照。
// newToolCatalogPort builds the catalog port; the builtin set comes from the declared
// names, not from the registry (registered is not the same as builtin).
func newToolCatalogPort(registry *tool.Registry, runtime *mcpRuntime) toolCatalogPort {
	builtin := make(map[string]struct{}, len(declaredToolNames))
	for _, name := range declaredToolNames {
		builtin[name] = struct{}{}
	}
	port := toolCatalogPort{registry: registry, builtin: builtin}
	if runtime != nil {
		port.mcp = runtime.conns
		port.dispositions = runtime.dispositions
	}
	return port
}

// Tools 按注册表顺序投影工具清单，来源按约束 27 分类。
// Tools projects the registry in name order with each tool's source.
func (p toolCatalogPort) Tools() []web.ToolDescriptor {
	if p.registry == nil {
		return []web.ToolDescriptor{}
	}
	specs := p.registry.Specs(p.registry.Names())
	out := make([]web.ToolDescriptor, 0, len(specs))
	for _, spec := range specs {
		out = append(out, web.ToolDescriptor{Spec: spec, Source: p.source(spec.Name)})
	}
	return out
}

// source 判一个已注册工具名的来源：内置声明集优先（内置赢家即使在来源表里被同名
// MCP 工具标注过也必须显示 builtin），其次来源表的赢家，最后退回连接快照扫描。
// source decides one name's source; the declared builtin set wins outright.
func (p toolCatalogPort) source(name string) string {
	if _, ok := p.builtin[name]; ok {
		return "builtin"
	}
	if p.dispositions != nil {
		if winner := p.dispositions.source(name); winner != "" && winner != mcpDispositionBuiltin {
			return "mcp"
		}
		return "code"
	}
	for _, conn := range p.mcp {
		for _, t := range conn.Tools {
			if t.Name() == name {
				return "mcp"
			}
		}
	}
	return "code"
}
