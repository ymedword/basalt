package web

// 本文件是只读元数据端点（模块卡 §3.1 / 约束 24）：/health /info /profiles /tools
// /memory 不触达任何写面；/memory 只读全局层。`registeredAgent` 同时服务建会话与
// invoke 两条运行入口——「注册」的判定只此一处，避免两处口径漂移。
// Read-only metadata endpoints; the registeredAgent lookup is the single definition
// shared by session creation and stateless invoke.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// memoryScopeGlobal 是 /memory 的固定 scope 字面量（契约 MemoryView.scope 枚举）。
// memoryScopeGlobal is the fixed /memory scope literal.
const memoryScopeGlobal = "global"

// 生命周期与可运行性的固定问题文案（约束 45）：只给可行动的原因类别，绝不回绝对
// 路径；原始错误（可能带工作区根路径）只进 WARN 日志。
// Fixed problem texts for lifecycle and runnability: a category only, never a path.
// The raw error, which may name the workspace root, stays in the WARN log.
const (
	lifecycleMissingProblem = "生命周期状态文件缺失"
	lifecycleCorruptProblem = "生命周期状态文件损坏或与目录名不符"
	lifecycleUnwiredProblem = "生命周期状态不可用（读取端口未装配）"
	resourceViewsProblem    = "资源视图不可用，无法确认可运行性"
	missingPublishedPrefix  = "资源引用无已发布版本："
)

// defaultMemoryBackend 是归一化配置为空时的后端字面量（约束 28：空 → markdown）。
// defaultMemoryBackend is the fallback backend literal when config is empty.
const defaultMemoryBackend = "markdown"

// registeredAgent 按名字查装载结果；未注册（含被拒）返回 false。
// 供 POST /sessions 与 POST /agents/{name}/invoke 共用。
// registeredAgent looks a name up in the loader result; unregistered entries,
// including rejected ones, report false.
func (s *Server) registeredAgent(name string) (core.AgentEntry, bool) {
	for _, entry := range s.opts.Agents.Profiles() {
		if entry.Name == name && entry.Registered {
			return entry, true
		}
	}
	return core.AgentEntry{}, false
}

// handleHealth 是存活探针：200 即健康，载荷固定 `{"status":"ok"}`（契约 /health）。
// handleHealth is the liveness probe; 200 alone means healthy.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeOK(w, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

// handleInfo 汇总进程读面（约束 25）：agents 只数已注册项、MCP 数为真实连接数、
// provider 摘要不含任何凭证字段。
// handleInfo serves the process read face with no credential fields.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	s.writeOK(w, infoView(s.opts, s.now()))
}

// handleProfiles 按端口给出的顺序（装配层为 name 升序）投影全部条目（含未注册项，
// 约束 26），并补齐契约要求的 lifecycle 与 runnable：前者是状态文件现状，后者是
// 「已注册 + active + 资源引用全部有已发布版本」的只读判断——展示提示，不是准入
// 闸门本身（准入在 core 内按发布指针解析并固定运行时）。
// handleProfiles projects every entry the port yields, enriched with the contract's
// lifecycle and runnable fields. runnable is a read-only display hint, never the
// admission gate itself.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	entries := s.opts.Agents.Profiles()
	index, indexErr := s.publishedResourceIndex(r.Context())
	if indexErr != nil {
		// 资源清单读不到时，runnable 按「不可确认」处理并给出问题项——**不得**
		// 静默当作可运行，也不把整个 /profiles 变成 500（列表本身仍可读）。
		// A resource read failure makes runnable unconfirmable; it never turns the
		// whole listing into a 500 and never defaults to true.
		s.logger().Warn("读取已发布资源视图失败，可运行性按不可确认处理", "error", indexErr)
	}
	views := make([]ProfileView, 0, len(entries))
	for _, entry := range entries {
		view := profileView(entry)
		view.Lifecycle = s.profileLifecycle(r.Context(), entry.Name)
		runnable, problems := profileRunnable(entry, view.Lifecycle, index, indexErr)
		view.Runnable = runnable
		view.Problems = append(view.Problems, problems...)
		views = append(views, view)
	}
	s.writeOK(w, views)
}

// resourcePublicationIndex 是一次 /profiles 请求内共享的「名字 → 是否有已发布版本」
// 快照：三份清单各读一次，不按 Agent 重复查询。true 表示存在已发布版本指针
// （id > 0）；名字缺失或指针为 0 都记 false。
// resourcePublicationIndex snapshots "name → has a published version" once per
// request; a missing name and a zero pointer both mean false.
type resourcePublicationIndex struct {
	providers  map[string]bool
	mcpServers map[string]bool
	skills     map[string]bool
}

// publishedResourceIndex 组装快照；Resources 端口未装配或任一清单读取失败都返回错误，
// 调用方据此把 runnable 标成不可确认。
// publishedResourceIndex builds the snapshot; an unwired port or any read failure is
// reported so the caller can mark runnability unconfirmable.
func (s *Server) publishedResourceIndex(ctx context.Context) (resourcePublicationIndex, error) {
	index := resourcePublicationIndex{
		providers:  map[string]bool{},
		mcpServers: map[string]bool{},
		skills:     map[string]bool{},
	}
	if s.opts.Resources == nil {
		return index, errors.New("装配缺少 Resources 端口")
	}
	connections, err := s.opts.Resources.ModelConnections(ctx)
	if err != nil {
		return index, err
	}
	for _, connection := range connections {
		index.providers[connection.Name] = connection.PublishedVersionID > 0
	}
	servers, err := s.opts.Resources.MCPServers(ctx)
	if err != nil {
		return index, err
	}
	for _, server := range servers {
		index.mcpServers[server.Name] = server.PublishedVersionID > 0
	}
	skills, err := s.opts.Resources.Skills(ctx)
	if err != nil {
		return index, err
	}
	for _, skill := range skills {
		index.skills[skill.Name] = skill.VersionID > 0
	}
	return index, nil
}

// profileLifecycle 读一个 Agent 的状态文件并投影；缺失 / 损坏 / 端口未装配都返回
// valid=false 加固定原因，**不得**把缺失读成可用。原始错误只记 WARN。
// profileLifecycle projects one agent's state file; every failure path is
// valid=false with a fixed reason, never "available".
func (s *Server) profileLifecycle(ctx context.Context, agent string) AgentLifecycleView {
	view := AgentLifecycleView{Agent: agent}
	if s.opts.Lifecycle == nil {
		view.LifecycleError = lifecycleUnwiredProblem
		return view
	}
	rec, exists, err := s.opts.Lifecycle.Read(ctx, agent)
	switch {
	case err != nil:
		s.logger().Warn("读取生命周期状态失败", "agent", agent, "error", err)
		view.LifecycleError = lifecycleCorruptProblem
		return view
	case !exists:
		view.LifecycleError = lifecycleMissingProblem
		return view
	}
	return agentLifecycleView(rec)
}

// profileRunnable 计算只读可运行判断：已注册 + 状态为 active + Provider / MCP / 技能
// 引用全部有已发布版本。未通过时返回具体的固定问题项；判定本身**不**产生副作用，
// 也不调用准入解析（那会固定运行时句柄，必须由 Run 自己释放）。
// profileRunnable computes the read-only runnability hint and the concrete problem
// items; it never calls admission resolution, which fixes runtime handles.
func profileRunnable(entry core.AgentEntry, lifecycle AgentLifecycleView, index resourcePublicationIndex, indexErr error) (bool, []string) {
	if !entry.Registered || entry.Profile == nil {
		return false, nil
	}
	if !lifecycle.Valid || lifecycle.State == nil || *lifecycle.State != string(core.AgentActive) {
		return false, nil
	}
	if indexErr != nil {
		return false, []string{resourceViewsProblem}
	}
	missing := missingPublishedResources(entry, index)
	if len(missing) == 0 {
		return true, nil
	}
	return false, []string{missingPublishedPrefix + strings.Join(missing, "、")}
}

// missingPublishedResources 列出 profile 引用、却在已发布快照中缺席的资源
// （`model_connection/x` / `mcp_server/x` / `skill/x`），顺序固定为 Provider → MCP →
// 技能、各自照 profile 声明序。/profiles 的 runnable 与 enable 前置校验共用这一份
// 判定，避免两处口径漂移。profile 为空视为不可判定，返回 nil。
// missingPublishedResources lists referenced resources absent from the publication
// snapshot in a fixed order; the runnable hint and the enable precondition share it.
func missingPublishedResources(entry core.AgentEntry, index resourcePublicationIndex) []string {
	if entry.Profile == nil {
		return nil
	}
	var missing []string
	if name := entry.Profile.Provider.Name; name != "" && !index.providers[name] {
		missing = append(missing, "model_connection/"+name)
	}
	for _, name := range entry.Profile.MCPServers {
		if !index.mcpServers[name] {
			missing = append(missing, "mcp_server/"+name)
		}
	}
	for _, name := range entry.Profile.Skills {
		if !index.skills[name] {
			missing = append(missing, "skill/"+name)
		}
	}
	return missing
}

// handleTools 按注册表顺序投影工具清单（来源由装配层分类，约束 27）。
// handleTools projects the tool inventory in registry order.
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	tools := s.opts.Tools.Tools()
	views := make([]ToolView, 0, len(tools))
	for _, d := range tools {
		views = append(views, toolView(d))
	}
	s.writeOK(w, views)
}

// handleMemory 只读全局层两分区（约束 28）：分区原样透传（含标题行、空分区为空串），
// backend 取归一化配置；读取失败记 ERROR 并回固定 500 文案。
// handleMemory serves the global layer's two partitions; read failures are 500.
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	coreText, archiveText, err := s.opts.Memory.Sections(r.Context(), core.GlobalScope)
	if err != nil {
		s.logger().Error("读取全局记忆失败", "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	backend := s.opts.MemoryBackend
	if backend == "" {
		backend = defaultMemoryBackend
	}
	s.writeOK(w, MemoryView{
		Scope:   memoryScopeGlobal,
		Agent:   "",
		Backend: backend,
		Core:    coreText,
		Archive: archiveText,
	})
}
