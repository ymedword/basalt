package app

// 本文件把重路径装配态适配成 HTTP 面（颗粒度文档 §3.1 的 `internal/app/web.go` 行）：
// `serve` 在重路径装配成功之后、交出端口之前，经这里把配置、运行入口、仓储与只读
// 装配态投影成 `web.Options`。**契约方向不因此改变**：web 仍只认自己声明的端口，
// 装配层是唯一知道"端口背后是哪个实例"的地方（与 ports.go 同一套适配纪律）。
// This file projects the heavy-path assembly state onto web.Options; web keeps
// depending only on its own consumer-side ports.

import (
	"fmt"
	"log/slog"
	"math"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/web"
	"github.com/ymedword/basalt/internal/workspace"
)

// newWebServer 把重路径装配态适配成 web.Options 并构造 HTTP 服务。
// newWebServer projects the runtime state onto web.Options and builds the server.
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
	return web.NewServer(a.webOptions(rt))
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
	return web.Options{
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

		Runs:     rt.service,
		Sessions: rt.sessions,
		Agents:   agentProfilesPort{entries: rt.entries},
		Tools:    newToolCatalogPort(rt.registry, rt.mcpConns),
		Memory:   rt.mem,
	}
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

// agentProfilesPort 是 `/profiles` 的适配器：**全部**装载结果（含未注册项）按装载
// 顺序交出——未注册原因正是 problems 的数据来源，过滤掉它们会让拒绝静默消失。
// agentProfilesPort keeps every loader entry, rejects included, in loader order.
type agentProfilesPort struct{ entries []core.AgentEntry }

// Profiles 返回装载结果（保持装载器给出的顺序，web 侧零重排）。
// Profiles returns the entries in loader order.
func (p agentProfilesPort) Profiles() []core.AgentEntry { return p.entries }

// toolCatalogPort 把注册池与 MCP 连接结果适配成工具清单（约束 27）：顺序 =
// `Registry.Names()`（排序序），来源判定 builtin 集 → builtin、MCP 连接池 → mcp、
// 其余 → code。builtin 集**必须**取自 `declaredToolNames`（九件内置的唯一声明），
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
