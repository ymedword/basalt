package web

// 本文件是 US-12 管理写面的**路由表唯一枚举**与条件写中间件（颗粒度文档
// §3.1.8）：所有新增端点先在这里登记方法 / 模式 / 是否需要 If-Match，再按统一
// 路径进入 `s.admin` 认证闸门（宪法 VII / 约束 9 的显式 actor）。表内每一行都
// 标记受保护——公开面仍是 US-11 的闭集，本 US 不新增公开入口。
// 条件写中间件只判 `If-Match` 的**形状**（逐字强 ETag，`etag.go`）；与当前版本
// 是否相等由服务层在同一写锁内判定（不匹配 412 且零写入）。认证在中间件之外，
// 因此缺凭据永远先得到 401，而不是 412。
// This file is the single route enumeration for the US-12 admin surface and the
// conditional-write middleware. Every row is protected; the middleware checks the
// If-Match shape only, while the service layer compares it inside the write lock.
// 表内三十五行全部指向已落地的 handler——占位函数在最后一条 handler 任务（工作区浏览）
// 落地时删除，没有"装配未完成"的中间态残留。R8 追加第 35 行（出网域名白名单条件写）。
// All thirty-five rows point at landed handlers; the placeholder was removed with the
// last handler task, so no "unassembled" state survives.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
)

// adminCurrentETagFunc 为一条条件写路由解析"当前 ETag"：返回 false 表示解析不了
// （资源不存在、端口未装配、读失败）——此时 412 仍照发，只是不带 data。
// adminCurrentETagFunc resolves the current ETag for a conditional route; false means
// it cannot be resolved and the 412 carries no data.
type adminCurrentETagFunc func(*http.Request) (string, bool)

// adminRouteSpec 是写面路由的一行：方法、模式、是否需要条件写以及处理函数。
// 条件写行在装配 `route` 时被 `requireIfMatch` 包装（顺序：认证 → If-Match → 处理）。
// `currentETag` 只服务于形状级 412 的 `data.current_etag`（约束 3）：它必须是**只读**
// 解析，不写操作行、不触达定义文件。
// adminRouteSpec is one admin route; conditional rows are wrapped by requireIfMatch
// after the auth guard, so missing credentials always win over a missing If-Match.
// currentETag only feeds data.current_etag on a shape-level 412 and stays read-only.
type adminRouteSpec struct {
	method      string
	pattern     string
	conditional bool
	currentETag adminCurrentETagFunc
	handler     AdminHandler
}

// adminRouteSpecs 返回全部新增端点的静态描述（35 条：Agent 9 / 资源 19 / 通知 4 /
// 工作区 2 / 执行环境 1）。顺序按契约 §3.7.1 排列，便于逐行对号；**不**依赖 map 遍历。
// adminRouteSpecs enumerates all 35 new endpoints in contract order.
func (s *Server) adminRouteSpecs() []adminRouteSpec {
	return []adminRouteSpec{
		// —— Agent 写面（handlers_admin_agents.go / handlers_admin_archived.go）——
		{method: http.MethodPost, pattern: "/api/v1/agents", handler: s.handleAdminCreateAgent},
		{method: http.MethodPost, pattern: "/api/v1/agents/{name}/generate-files", handler: s.handleAdminGenerateAgentFiles},
		// 定时候选（R6.4）：模型只产候选，确认保存仍走 `POST /agents/{name}/files`
		// 的 `changes.schedules` 条件写——本端点不写操作行、不落盘、不改调度器。
		{method: http.MethodPost, pattern: "/api/v1/agents/{name}/schedule-draft", handler: s.handleAdminScheduleDraft},
		{method: http.MethodGet, pattern: "/api/v1/agents/{name}/files", handler: s.handleAdminAgentFiles},
		{
			method: http.MethodPost, pattern: "/api/v1/agents/{name}/files", conditional: true,
			currentETag: s.agentFilesCurrentETag, handler: s.handleAdminSubmitAgentFiles,
		},
		{method: http.MethodPost, pattern: "/api/v1/agents/{name}/copy", handler: s.handleAdminCopyAgent},
		{method: http.MethodDelete, pattern: "/api/v1/agents/{name}", handler: s.handleAdminArchiveAgent},
		{method: http.MethodGet, pattern: "/api/v1/archived-agents", handler: s.handleAdminArchivedAgents},
		{method: http.MethodPost, pattern: "/api/v1/archived-agents/{archive_id}/restore", handler: s.handleAdminRestoreAgent},

		// —— 模型连接（handlers_admin_resources.go）——
		{method: http.MethodPost, pattern: "/api/v1/model-connections", handler: s.handleAdminCreateModelConnection},
		{
			method: http.MethodPut, pattern: "/api/v1/model-connections/{name}", conditional: true,
			currentETag: s.resourceCurrentETag(core.ResourceModelConnection), handler: s.handleAdminUpdateModelConnection,
		},
		{
			method: http.MethodDelete, pattern: "/api/v1/model-connections/{name}", conditional: true,
			currentETag: s.resourceCurrentETag(core.ResourceModelConnection), handler: s.handleAdminDeleteModelConnection,
		},
		{method: http.MethodPost, pattern: "/api/v1/model-connections/{name}/test", handler: s.handleAdminTestModelConnection},

		// —— MCP server（handlers_admin_resources.go）——
		{method: http.MethodPost, pattern: "/api/v1/mcp-servers", handler: s.handleAdminCreateMCPServer},
		{
			method: http.MethodPut, pattern: "/api/v1/mcp-servers/{name}", conditional: true,
			currentETag: s.resourceCurrentETag(core.ResourceMCPServer), handler: s.handleAdminUpdateMCPServer,
		},
		{
			method: http.MethodDelete, pattern: "/api/v1/mcp-servers/{name}", conditional: true,
			currentETag: s.resourceCurrentETag(core.ResourceMCPServer), handler: s.handleAdminDeleteMCPServer,
		},
		{method: http.MethodPost, pattern: "/api/v1/mcp-servers/{name}/test", handler: s.handleAdminTestMCPServer},

		// —— 技能（handlers_admin_resources.go；POST 创建 / 导入按契约只回 SkillView）——
		{method: http.MethodPost, pattern: "/api/v1/skills", handler: s.handleAdminCreateSkill},
		{method: http.MethodPost, pattern: "/api/v1/skills/import", handler: s.handleAdminImportSkill},
		{
			method: http.MethodPut, pattern: "/api/v1/skills/{name}", conditional: true,
			currentETag: s.resourceCurrentETag(core.ResourceSkill), handler: s.handleAdminUpdateSkill,
		},
		{
			method: http.MethodDelete, pattern: "/api/v1/skills/{name}", conditional: true,
			currentETag: s.resourceCurrentETag(core.ResourceSkill), handler: s.handleAdminDeleteSkill,
		},
		{method: http.MethodPost, pattern: "/api/v1/skills/{name}/validate", handler: s.handleAdminValidateSkill},

		// —— 版本 / 发布 / 引用（handlers_admin_resources.go）——
		{method: http.MethodGet, pattern: "/api/v1/resources/{kind}/{id}/versions", handler: s.handleAdminResourceVersions},
		{method: http.MethodGet, pattern: "/api/v1/resource-versions/{version_id}", handler: s.handleAdminResourceVersionDetail},
		{
			method: http.MethodPost, pattern: "/api/v1/resources/{kind}/{id}/publish", conditional: true,
			currentETag: s.publishCurrentETag, handler: s.handleAdminResourcePublish,
		},
		{method: http.MethodGet, pattern: "/api/v1/resources/{kind}/{id}/references", handler: s.handleAdminResourceReferences},
		{
			method: http.MethodPost, pattern: "/api/v1/resources/{kind}/{name}/credential/revoke", conditional: true,
			currentETag: s.resourceStateCurrentETag, handler: s.handleAdminRevokeCredential,
		},
		{
			method: http.MethodPost, pattern: "/api/v1/resources/{kind}/{name}/scope", conditional: true,
			currentETag: s.resourceStateCurrentETag, handler: s.handleAdminConvertScope,
		},

		// —— 通知渠道写面（handlers_admin_notify.go；GET 列表 / 类型沿用 US-11）——
		{method: http.MethodPost, pattern: "/api/v1/notify-channels", handler: s.handleAdminCreateNotifyChannel},
		{method: http.MethodPut, pattern: "/api/v1/notify-channels/{name}", handler: s.handleAdminUpdateNotifyChannel},
		{method: http.MethodDelete, pattern: "/api/v1/notify-channels/{name}", handler: s.handleAdminDeleteNotifyChannel},
		{method: http.MethodPost, pattern: "/api/v1/notify-channels/{name}/test", handler: s.handleAdminTestNotifyChannel},

		// —— 工作区只读（handlers_admin_workspace.go）——
		{method: http.MethodGet, pattern: "/api/v1/workspace/tree", handler: s.handleAdminWorkspaceTree},
		{method: http.MethodGet, pattern: "/api/v1/workspace/file", handler: s.handleAdminWorkspaceFile},

		// —— 执行环境：出网域名白名单条件写（handlers_sandbox.go；R8 裁决 2）——
		{
			method: http.MethodPut, pattern: "/api/v1/sandbox/network-domains", conditional: true,
			currentETag: s.networkAllowlistCurrentETag, handler: s.handleAdminUpdateNetworkDomains,
		},
	}
}

// networkAllowlistCurrentETag 解析出网白名单条件写的当前 ETag：只读内存快照
// （`Current` 不触库、不写操作行）。管理端口未装配时按"解析不了"处理，412 不带 data。
// networkAllowlistCurrentETag resolves the live ETag from the in-memory snapshot.
func (s *Server) networkAllowlistCurrentETag(*http.Request) (string, bool) {
	if s.opts.NetworkAllowlist == nil {
		return "", false
	}
	return core.NetworkDomainETag(s.opts.NetworkAllowlist.Current()), true
}

// adminRoutes 把静态描述装配成可与 US-11 路由表合并的 `route` 行：统一标记
// 受保护，条件写行套上 If-Match 中间件。`server.routes` 与本表共同构成唯一枚举。
// adminRoutes builds protected route rows, wrapping conditional writes with the
// If-Match middleware.
func (s *Server) adminRoutes() []route {
	specs := s.adminRouteSpecs()
	rows := make([]route, 0, len(specs))
	for _, spec := range specs {
		handler := spec.handler
		if spec.conditional {
			handler = s.requireIfMatch(handler, spec.currentETag)
		}
		rows = append(rows, route{
			method:       spec.method,
			pattern:      spec.pattern,
			visibility:   routeProtected,
			adminHandler: handler,
		})
	}
	return rows
}

// ifMatchRequiredMessage 是条件写拒绝的固定公开文案（412）；不区分"缺失 / 弱 /
// 通配 / 格式非法"的具体原因，避免给探测者额外信号。
// ifMatchRequiredMessage is the fixed public text for every rejected If-Match shape.
const ifMatchRequiredMessage = "条件写要求 If-Match 为当前 ETag 的逐字强 ETag"

// requireIfMatch 是条件写中间件：`If-Match` 缺失、弱 ETag、`*`、逗号列表或形状
// 非法时直接 412，**不**进入处理函数（因此不写管理操作行、不触达磁盘）。形状
// 合法时处理函数仍需按请求头重读原值，服务层负责与当前版本比较。412 在能解析时
// 带上 `data.current_etag`（约束 3）：形状错误与陈旧值给出同一组字段，页面才能按
// 同一条路径重新取 ETag。
// requireIfMatch rejects any non-literal strong ETag before the handler runs; a
// well-formed header is re-read by the handler and compared in the service. When
// resolvable, the shape-level 412 also carries data.current_etag (constraint 3).
func (s *Server) requireIfMatch(next AdminHandler, currentETag adminCurrentETagFunc) AdminHandler {
	return func(w http.ResponseWriter, r *http.Request, actor core.Actor) {
		if _, ok := parseIfMatch(r.Header.Get("If-Match")); ok {
			next(w, r, actor)
			return
		}
		var failure error
		if currentETag != nil {
			if current, resolved := currentETag(r); resolved {
				failure = &core.AdminError{
					Kind:    core.ErrPreconditionFailed,
					Message: ifMatchRequiredMessage,
					Data:    map[string]any{"current_etag": current},
				}
			}
		}
		s.writeAdminDataError(w, http.StatusPreconditionFailed, codePreconditionFailed,
			ifMatchRequiredMessage, failure)
	}
}

// resourceCurrentETag 解析模型连接 / MCP / 技能条件写的当前 ETag：读发布指针与版本
// 列表，取**已发布**那一行的 content_hash（约束 4 的字节定义）。只读、不写操作行。
// resourceCurrentETag resolves the published version's strong ETag for a resource.
func (s *Server) resourceCurrentETag(kind core.ResourceKind) adminCurrentETagFunc {
	return func(r *http.Request) (string, bool) {
		return s.resourceIDCurrentETag(r.Context(), kind, r.PathValue("name"))
	}
}

// publishCurrentETag 解析 `POST /resources/{kind}/{id}/publish` 的当前 ETag：kind 与 id
// 都来自路径；`kind` 非法时按"解析不了"处理（handler 仍会给出分类错误）。
// publishCurrentETag resolves the ETag for the kind/id pair on the publish route.
func (s *Server) publishCurrentETag(r *http.Request) (string, bool) {
	kind, err := core.ParseResourceKind(r.PathValue("kind"))
	if err != nil {
		return "", false
	}
	return s.resourceIDCurrentETag(r.Context(), kind, r.PathValue("id"))
}

// resourceIDCurrentETag 是资源类条件写 ETag 的唯一读取点：**必须**转调服务端的
// `CurrentETag`，由适配器用「指针 + 发布版本 + 凭据状态」组成当前值（FR-081）。
// 这里**不得**用版本列表的 `content_hash` 自行拼装——那会漏掉范围与凭据段，
// 形状级 412 给出的 `current_etag` 就不能用于重试（服务层会再判一次 412）。
// resourceIDCurrentETag forwards to the service's CurrentETag; composing the value
// from the version list here would omit the scope and credential segments.
func (s *Server) resourceIDCurrentETag(ctx context.Context, kind core.ResourceKind, id string) (string, bool) {
	if s.opts.ResourceAdminService == nil || id == "" {
		return "", false
	}
	return s.opts.ResourceAdminService.CurrentETag(ctx, kind, id)
}

// resourceStateCurrentETag 解析两个状态动作端点（撤销 / 范围切换）的当前条件 ETag：
// 类别只接受 model_connection / notify_channel，渠道侧必须与写路径用**同一份**配方
// （适配器转调管理读面）。解析不了一律返回 false，形状级 412 因而省略 `data`。
// resourceStateCurrentETag resolves the conditional ETag for both state actions; the
// adapter delegates to the same recipe the write paths compare against.
func (s *Server) resourceStateCurrentETag(r *http.Request) (string, bool) {
	if s.opts.ResourceStateService == nil {
		return "", false
	}
	return s.opts.ResourceStateService.CurrentETag(r.Context(),
		r.PathValue("kind"), r.PathValue("name"))
}

// agentFilesCurrentETag 解析 Agent 多文件提交的受管文件集合 ETag（约束 30）：由适配器
// 在同一把 Agent 写锁内算出，web 层只转交。
// agentFilesCurrentETag forwards the managed-file-set ETag computed by the adapter.
func (s *Server) agentFilesCurrentETag(r *http.Request) (string, bool) {
	if s.opts.AgentAdminService == nil {
		return "", false
	}
	name := r.PathValue("name")
	if name == "" {
		return "", false
	}
	set, err := s.opts.AgentAdminService.Files(r.Context(), name)
	if err != nil || set.ETag == "" {
		return "", false
	}
	return set.ETag, true
}

// parseVersionID 解析 `GET /resource-versions/{version_id}` 的路径参数：只接受
// ASCII 十进制数字组成的正整数；符号、小数、十六进制、前后空白、溢出与 ≤0 一律
// 不合法。不合法与不存在在 handler 上统一映射 404 NOT_FOUND——**不**做 400 区分，
// 避免按状态码枚举版本是否存在。
// parseVersionID accepts ASCII decimal digits forming a positive integer; handlers
// map every rejected shape to 404, so the status cannot be used to enumerate versions.
func parseVersionID(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c < '0' || c > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
