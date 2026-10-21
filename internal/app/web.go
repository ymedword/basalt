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
// **先绑定监听，再用真实地址校验管理认证配置**——缺 mode / 非法 mode / token 模式
// 缺凭据 / local_trust 绑定非回环，都在交出端口之前拒绝启动（约束 1 / 3 / 4）。
// newWebServer binds the listener first, then validates the management-auth
// configuration against the real address before any request can be served.
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
		RunBudget:       webBudget(cfg),
		Version:         Version,
		MemoryBackend:   memoryBackendName(cfg),
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
		Tools:    newToolCatalogPort(rt.registry, rt.mcpConns),
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
		Sandbox:     sandboxConfigView(cfg),
		Notify:      storage.NewNotifyChannelRepo(rt.db),
		NotifyTypes: notifyTypeNames(),

		// 管理台产物是编译期常量（go:embed，约束 43）：装配期直接给出，运行时不读磁盘，
		// 也不依赖 Node——单二进制复制到目标机后页面照常打开。
		// The console artifact is embedded at compile time; no disk or Node at runtime.
		Console: assets.Console(),
	}
	a.wireManagementPorts(&opts, rt)
	return opts
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
		opts.Resources = NewResourceViews(rt.versions, rt.lifecycle, rt.mcpRT)
	}
	if rt.db != nil && rt.runs != nil && rt.versions != nil {
		opts.System = newSystemReader(rt.db, rt.runs, rt.versions, a.nowOr())
	}
	opts.Sampler = web.NewSampler(a.nowOr())

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
		Now: now,
	})
	opts.AgentAdmin = NewAgentConsole(AgentConsoleDeps{
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
// 其余 → code。builtin 集**必须**取自 `declaredToolNames`（十件内置的唯一声明），
// 另写一份名单必然漂移。
// toolCatalogPort adapts the registry plus MCP connections; the builtin set is the
// single declared name set, so a second hand-maintained list can never drift.
type toolCatalogPort struct {
	registry *tool.Registry
	mcp      []tool.MCPConnection
	builtin  map[string]struct{}
}

// newToolCatalogPort 从注册池与连接结果构建清单端口；builtin 集在构造期一次成型
// （数据来源是本包的声明名集，不读注册池——"注册了"不等于"内置"）。
// newToolCatalogPort builds the catalog port; the builtin set comes from the declared
// names, not from the registry (registered is not the same as builtin).
func newToolCatalogPort(registry *tool.Registry, mcp []tool.MCPConnection) toolCatalogPort {
	builtin := make(map[string]struct{}, len(declaredToolNames))
	for _, name := range declaredToolNames {
		builtin[name] = struct{}{}
	}
	return toolCatalogPort{registry: registry, mcp: mcp, builtin: builtin}
}

// Tools 按注册表顺序投影工具清单，来源按约束 27 分类。
// Tools projects the registry in name order with each tool's source.
func (p toolCatalogPort) Tools() []web.ToolDescriptor {
	if p.registry == nil {
		return []web.ToolDescriptor{}
	}
	mcpNames := make(map[string]struct{})
	for _, conn := range p.mcp {
		for _, t := range conn.Tools {
			mcpNames[t.Name()] = struct{}{}
		}
	}
	specs := p.registry.Specs(p.registry.Names())
	out := make([]web.ToolDescriptor, 0, len(specs))
	for _, spec := range specs {
		source := "code"
		if _, ok := p.builtin[spec.Name]; ok {
			source = "builtin"
		} else if _, ok := mcpNames[spec.Name]; ok {
			source = "mcp"
		}
		out = append(out, web.ToolDescriptor{Spec: spec, Source: source})
	}
	return out
}
