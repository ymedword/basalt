package web

// 本文件是「离能跑还差什么」的唯一判定处（14D 设计 §2.5.1 / §2.5.2 / §2.6.4，验收
// E6 / E8）：`GET /api/v1/agents/{name}/readiness` 的投影，与启用前置校验共用一个
// 评估器——两处各写一套判定，页面说"可以启用"、按钮却 409，是这类功能最坏的假成功。
//
// **两类结果，语义不同**：
//   - `pending` 非空 = **不可启用**：缺模型连接、缺该 Agent 所需的服务凭据、资源引用
//     没有已发布版本。启用端点据此 409 并逐项点名 + 给跳转。
//   - `notices` 只提示、**不**阻止启用：通知渠道缺失（可选）、`settings.max_iterations`
//     被实例上限压住、计划数可能超过并发上限。E8 明确要求"只点名不改配置"。
//
// 判定是**纯读**：不启动进程、不连接、不写操作行、不改任何配置；读不到资源视图时
// 返回错误（启用路径据此按不可确认失败关闭），**不**静默降级成"没有待补项"。
// The readiness evaluator is the single verdict behind both the read endpoint and the
// enable precondition. Pending blocks; notices never do. Every check is read-only, and an
// unreadable resource view is an error rather than an empty pending list.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// 就绪项的 kind 闭集（契约 AgentReadinessItem.kind）。前三个属 pending，后三个属
// notices；两组各自固定，**不**合并枚举。
// The frozen kind set: the first three block, the last three only advise.
const (
	readinessModelConnection = "model_connection"
	readinessCredential      = "credential"
	readinessResource        = "resource"
	readinessNotifyChannel   = "notify_channel"
	readinessBudget          = "budget"
	readinessConcurrency     = "concurrency"
)

// 控制台跳转路径（与 `assets/console/src/router.ts` 的既有路由逐字对应；新增跳转
// **必须**指向已存在的页面，否则页面给的是点不通的链接）。
// Console jump paths; every one of them must exist in the console router.
const (
	consoleAgentConfigPath = "/console/agents/%s/config"
	consoleModelConnsPath  = "/console/resources/model-connections"
	consoleMCPServersPath  = "/console/resources/mcp-servers"
	consoleMCPServerPath   = "/console/resources/mcp-servers/%s"
	consoleSkillsPath      = "/console/resources/skills"
	consoleNotifyPath      = "/console/settings/notifications"
)

// 固定中文原因文案：一行说明"差什么"，**不**回显英文库文案与路径（约束 45）。
// Fixed Chinese reasons; raw library text and host paths never reach the response.
// #nosec G101 -- 固定公开文案：谈论凭据状态，不含任何材料（变量名命中）。
const (
	readinessMissingModelDetail  = "引用的模型连接未发布或不存在"
	readinessModelCredDetail     = "模型连接的凭据不可用（托管已撤销或环境变量缺失）"
	readinessMCPServerDetail     = "MCP server 未发布或不存在"
	readinessSkillDetail         = "技能没有已发布版本"
	readinessCredRevokedDetail   = "托管凭据已撤销"
	readinessCredMissingDetail   = "声明了托管来源但没有可用凭据"
	readinessCredUnknownDetail   = "凭据来源状态未知"
	readinessNotifyMissingDetail = "尚未配置通知渠道（可选，不阻止启用）"
	readinessNotifyUnknownDetail = "通知渠道不存在"
)

// agentReadiness 评估一只 Agent 的就绪状态。`Registered == false` 时装载期已经拒绝，
// 后续检查没有意义：只回装载拒因一项，避免页面列出"补了也仍不可用"的清单。
// 资源视图读不到时返回错误——调用方必须按"不可确认"处理，而不是当作通过。
// agentReadiness evaluates one agent; an unregistered entry yields the loader's refusal
// alone, and an unreadable resource view is an error, never a pass.
func (s *Server) agentReadiness(ctx context.Context, entry core.AgentEntry) (AgentReadinessView, error) {
	view := AgentReadinessView{
		Agent:   entry.Name,
		Pending: []AgentReadinessItem{},
		Notices: []AgentReadinessItem{},
	}
	configFix := fmt.Sprintf(consoleAgentConfigPath, url.PathEscape(entry.Name))
	if !entry.Registered {
		detail := enableRejectedReason
		if entry.RejectKind != "" {
			detail = entryRejectMessage(entry)
		}
		view.Pending = append(view.Pending, AgentReadinessItem{
			Kind:   readinessKindOfReject(entry.RejectKind),
			Detail: detail,
			Fix:    readinessFixForReject(entry.RejectKind, configFix),
		})
		return view, nil
	}
	if entry.Profile == nil {
		// 已注册却没有 profile 只可能来自人工构造的条目；仍按"不可确认"拒绝。
		view.Pending = append(view.Pending, AgentReadinessItem{
			Kind:   readinessResource,
			Detail: enableRejectedReason,
			Fix:    configFix,
		})
		return view, nil
	}

	profile := entry.Profile
	index, err := s.publishedResourceIndex(ctx)
	if err != nil {
		return view, err
	}
	view.Pending = append(view.Pending, s.modelConnectionPending(profile, index)...)
	view.Pending = append(view.Pending, s.mcpPending(profile, index)...)
	view.Pending = append(view.Pending, s.skillPending(profile, index)...)
	view.Notices = append(view.Notices, s.scheduleNotices(ctx, profile, entry.Name)...)
	view.Ready = len(view.Pending) == 0
	return view, nil
}

// readinessKindOfReject 把装载拒因分类映射到就绪 kind：模型侧两项归
// `model_connection`，其余（含 MCP / 技能 / 绑定 / 计划）归 `resource`。
// readinessKindOfReject maps the loader's category onto a readiness kind.
func readinessKindOfReject(kind core.AgentRejectKind) string {
	switch kind {
	case core.AgentRejectBlankModel, core.AgentRejectProviderNotRegistered:
		return readinessModelConnection
	default:
		return readinessResource
	}
}

// readinessFixForReject 把装载拒因指到**能修它**的页面：缺模型连接去模型连接页、
// 缺技能去技能页、MCP 不可用去 MCP 页，其余（名字 / frontmatter / 绑定 / 计划）只能改
// Agent 自己的文件。跳转必须点得通——控制台没有的路由**不**写。
// readinessFixForReject points each loader refusal at the page that can actually fix it.
func readinessFixForReject(kind core.AgentRejectKind, configFix string) string {
	switch kind {
	case core.AgentRejectProviderNotRegistered:
		return consoleModelConnsPath
	case core.AgentRejectSkillNotFound, core.AgentRejectSkillVersionMissing:
		return consoleSkillsPath
	case core.AgentRejectMCPServerSkipped:
		return consoleMCPServersPath
	default:
		return configFix
	}
}

// modelConnectionPending 检查模型连接：未发布 / 凭据不可用是**阻断项**
// （设计 §2.5.1：模型连接是必需项）。`Availability` 只认显式的 `unavailable`——
// 空串表示该端口没有投影这一项，本层不据此拒绝（不是"可用"的断言，只是"不判"）。
//
// **空 Provider 名在本层不判**（T114-R3）：装载规则 2 / 3 已把未注册 provider 与空
// 模型整只拒载（`Registered=false`），启用路径根本走不到这里；能走到这里的空名只可能
// 来自人工构造的条目。与 `missingPublishedResources` 保持同一口径，两处判定不漂移。
// modelConnectionPending blocks on an unpublished or credential-unavailable model
// connection; a blank provider name is not judged here because the loader already
// rejects it (rules 2/3), matching the runnable hint's rule.
func (s *Server) modelConnectionPending(profile *core.Profile, index resourcePublicationIndex) []AgentReadinessItem {
	name := profile.Provider.Name
	if name == "" {
		return nil
	}
	view, found := s.modelConnectionView(profile)
	if !found || !index.providers[name] {
		return []AgentReadinessItem{{
			Kind:   readinessModelConnection,
			Item:   "model_connection/" + name,
			Detail: readinessMissingModelDetail,
			Fix:    consoleModelConnsPath,
		}}
	}
	if view.Availability == core.AvailabilityUnavailable {
		return []AgentReadinessItem{{
			Kind:   readinessCredential,
			Item:   "model_connection/" + name,
			Detail: readinessModelCredDetail,
			Fix:    consoleModelConnsPath,
		}}
	}
	return nil
}

// mcpPending 检查 MCP 依赖：条目未发布是资源项；声明了托管来源却不可用是凭据项。
//
// **`env` 来源按就绪处理**（T114-R2）：保存 / 发布期的 env 闸门已在发布那一刻证明
// 变量存在，事后被移除会在运行期以分类失败暴露（设计 §2.5.1 末段），启用闸门不
// 重复猜一份进程环境快照。未声明来源的槽位不出现在 `CredentialKeys` 里（视图只列
// 已声明槽位），其来源由装配期按事实补记，同样不在这里阻断。
// mcpPending blocks on unpublished entries and unusable managed credentials. An `env`
// source counts as ready: the save-time gate proved it, and later removal surfaces as a
// classified run failure.
func (s *Server) mcpPending(profile *core.Profile, index resourcePublicationIndex) []AgentReadinessItem {
	var pending []AgentReadinessItem
	for _, name := range profile.MCPServers {
		view, found := s.mcpServerView(name)
		if !found || !index.mcpServers[name] {
			pending = append(pending, AgentReadinessItem{
				Kind:   readinessResource,
				Item:   "mcp_server/" + name,
				Detail: readinessMCPServerDetail,
				Fix:    consoleMCPServersPath,
			})
			continue
		}
		for _, key := range view.CredentialKeys {
			detail := ""
			switch core.MCPCredentialKeyState(key.State) {
			case core.MCPCredentialManaged, core.MCPCredentialEnv:
				continue
			case core.MCPCredentialManagedRevoked:
				detail = readinessCredRevokedDetail
			case core.MCPCredentialMissing:
				detail = readinessCredMissingDetail
			default:
				detail = readinessCredUnknownDetail
			}
			pending = append(pending, AgentReadinessItem{
				Kind:   readinessCredential,
				Item:   "mcp_server/" + name + "#" + key.Key,
				Detail: detail,
				Fix:    fmt.Sprintf(consoleMCPServerPath, url.PathEscape(name)),
			})
		}
	}
	return pending
}

// skillPending 检查技能引用是否有已发布版本（未发布 = 资源项）。
// skillPending blocks on a skill without a published version.
func (s *Server) skillPending(profile *core.Profile, index resourcePublicationIndex) []AgentReadinessItem {
	var pending []AgentReadinessItem
	for _, name := range profile.Skills {
		if index.skills[name] {
			continue
		}
		pending = append(pending, AgentReadinessItem{
			Kind:   readinessResource,
			Item:   "skill/" + name,
			Detail: readinessSkillDetail,
			Fix:    consoleSkillsPath,
		})
	}
	return pending
}

// scheduleNotices 组装两条**只提示**的项：
//
//  1. 预算：Agent 声明的 `settings.max_iterations` 大于实例生效上限时点名两边数字，
//     说明"超出部分不会生效"——`mergeBudget` 取小，**不**改任何一侧的配置（E8）。
//  2. 并发：实例里启用了计划的不同 Agent 数超过 `server.max_concurrent_runs` 时提示
//     "到点可能被拒绝（429 TOO_MANY_RUNS）"。这是**可能性**提示而非精确排期推断：
//     不解析 cron 求交集，只看计划数量级（设计 §2.5.1 第 2 条的"是否可能"）。
//
// 两个端口（Schedules / Notify）都可缺省：缺端口或读失败只跳过提示并记 WARN，
// **不**阻断启用——它们是可选项，读不到不等于不可用。
// scheduleNotices advises on the budget cap and on a possible concurrency overflow. Both
// ports are optional: a missing port or read failure only skips the notice.
func (s *Server) scheduleNotices(ctx context.Context, profile *core.Profile, agent string) []AgentReadinessItem {
	var notices []AgentReadinessItem
	limits := s.opts.InstanceLimits
	if profile.Settings.MaxIterations > limits.MaxIterations {
		notices = append(notices, AgentReadinessItem{
			Kind: readinessBudget,
			Item: "settings.max_iterations",
			Detail: fmt.Sprintf("声明 %d 超过实例上限 %d，超出部分不会生效（实例配置键 budget.max_iterations）",
				profile.Settings.MaxIterations, limits.MaxIterations),
		})
	}
	notices = append(notices, s.concurrencyNotice(ctx, agent)...)
	return notices
}

// concurrencyNotice 统计启用计划涉及的 Agent 数并与并发上限对照。已退役的计划不计；
// 调度端口未装配或读失败时不出提示（WARN 留痕）。
// concurrencyNotice compares the number of agents with an enabled schedule against the
// instance cap; a missing or failing schedule port only skips the notice.
func (s *Server) concurrencyNotice(ctx context.Context, agent string) []AgentReadinessItem {
	if s.opts.Schedules == nil {
		return nil
	}
	tasks, err := s.opts.Schedules.List(ctx)
	if err != nil {
		s.logger().Warn("读取计划列表失败，并发提示按不可判定处理", "agent", agent, "error", err)
		return nil
	}
	agents := map[string]struct{}{}
	for _, task := range tasks {
		if !task.Enabled || !task.RetiredAt.IsZero() {
			continue
		}
		agents[task.Agent] = struct{}{}
	}
	limit := s.opts.InstanceLimits.MaxConcurrentRuns
	if len(agents) <= limit {
		return nil
	}
	return []AgentReadinessItem{{
		Kind: readinessConcurrency,
		Item: "server.max_concurrent_runs",
		Detail: fmt.Sprintf("有 %d 只 Agent 启用了计划，超过并发上限 %d，到点可能被拒绝（429 TOO_MANY_RUNS）；请错开计划或调高上限",
			len(agents), limit),
	}}
}

// notifyNotice 检查通知渠道（可选，**不**阻止启用）：声明的渠道不存在、或该 Agent
// 需要通知却一个渠道都没有时提示。`none` 意图不提示。
// notifyNotice advises on missing notify channels; it never blocks enable.
func (s *Server) notifyNotice(ctx context.Context, profile *core.Profile, configFix string) []AgentReadinessItem {
	if effectiveNotifyMode(profile) == core.NotifyModeNone {
		return nil
	}
	if s.opts.Notify == nil {
		return nil
	}
	channels, err := s.opts.Notify.ListNotifyChannels(ctx)
	if err != nil {
		s.logger().Warn("读取通知渠道失败，通知就绪提示跳过", "error", err)
		return nil
	}
	known := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		known[channel.Name] = struct{}{}
	}
	if len(profile.NotifyChannels) == 0 {
		if len(known) == 0 {
			return []AgentReadinessItem{{
				Kind:   readinessNotifyChannel,
				Detail: readinessNotifyMissingDetail,
				Fix:    consoleNotifyPath,
			}}
		}
		return nil
	}
	var notices []AgentReadinessItem
	for _, name := range profile.NotifyChannels {
		if _, ok := known[name]; ok {
			continue
		}
		notices = append(notices, AgentReadinessItem{
			Kind:   readinessNotifyChannel,
			Item:   name,
			Detail: readinessNotifyUnknownDetail,
			Fix:    configFix,
		})
	}
	return notices
}

// modelConnectionView 在一次评估内按名字取模型连接视图；资源端口缺省时返回未找到。
// modelConnectionView looks one connection up; a missing port reads as not found.
func (s *Server) modelConnectionView(profile *core.Profile) (ModelConnectionView, bool) {
	if s.opts.Resources == nil {
		return ModelConnectionView{}, false
	}
	connections, err := s.opts.Resources.ModelConnections(context.Background())
	if err != nil {
		return ModelConnectionView{}, false
	}
	for _, connection := range connections {
		if connection.Name == profile.Provider.Name {
			return connection, true
		}
	}
	return ModelConnectionView{}, false
}

// mcpServerView 在一次评估内按名字取 MCP server 视图；资源端口缺省时返回未找到。
// mcpServerView looks one MCP server up; a missing port reads as not found.
func (s *Server) mcpServerView(name string) (McpServerView, bool) {
	if s.opts.Resources == nil {
		return McpServerView{}, false
	}
	servers, err := s.opts.Resources.MCPServers(context.Background())
	if err != nil {
		return McpServerView{}, false
	}
	for _, server := range servers {
		if server.Name == name {
			return server, true
		}
	}
	return McpServerView{}, false
}

// readinessPendingMessage 把待补项拼成 409 的一行中文：逐项点名 + 跳转。`item` 为空的
// 项（装载拒因、未声明模型）只有一句话，不拼括号；有名字的项给 `名字（原因，前往 路径）`。
// 名字用 `model_connection/x` / `mcp_server/x#KEY` / `skill/x`，与 /profiles 的问题项同一
// 命名空间——两处看到的是同一个东西。
// readinessPendingMessage joins the pending items into the single 409 sentence.
func readinessPendingMessage(items []AgentReadinessItem) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if item.Item == "" {
			parts = append(parts, item.Detail)
			continue
		}
		part := item.Item
		inner := item.Detail
		if item.Fix != "" {
			if inner != "" {
				inner += "，"
			}
			inner += "前往 " + item.Fix
		}
		if inner != "" {
			part += "（" + inner + "）"
		}
		parts = append(parts, part)
	}
	return pendingConfigPrefix + strings.Join(parts, "、")
}

// pendingConfigPrefix 是启用拒绝里待补项清单的引导语。
// pendingConfigPrefix introduces the pending list in the 409 message.
const pendingConfigPrefix = "缺少必需配置："

// handleAdminAgentReadiness 是 `GET /agents/{name}/readiness`：只读投影，复用启用
// 前置校验的同一份评估器。不存在 → 404；资源视图读不到 → 固定 500（读面不写操作行，
// 也不把"读不到"伪装成"就绪"）。
// handleAdminAgentReadiness serves the read-only readiness view; existence is the loader
// result and an unreadable resource view is a fixed 500.
func (s *Server) handleAdminAgentReadiness(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	name := r.PathValue("name")
	entry, exists := s.lifecycleEntry(name)
	if !exists {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到 Agent："+name)
		return
	}
	view, err := s.agentReadiness(r.Context(), entry)
	if err != nil {
		s.logger().Warn("读取已发布资源视图失败，就绪检查不可确认", "agent", name, "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, resourceViewsProblem)
		return
	}
	view.Notices = append(view.Notices, s.notifyNotice(r.Context(), entry.Profile, fmt.Sprintf(consoleAgentConfigPath, url.PathEscape(name)))...)
	s.writeOK(w, view)
}
