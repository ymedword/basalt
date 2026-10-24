package app

// 本文件是资源使用范围的转换编排（颗粒度文档 R4.4 / T121）。
//
// 两个方向不对称：
//   - **专属 → 共享**永远放行：收缩的是别人的可用面，放开的是全部有资格者，没有
//     需要保护的既有引用——保留 UID / 版本 / 凭据 / 绑定，只改范围。
//   - **共享 → 专属**必须先把"谁在用"查清楚，按卡片 R4.4 的四道证据：① 其他 Agent
//     的绑定投影（含归档）、② 定义文件整词扫描（解析失败按可能引用）、③ 未释放的
//     运行引用、④ 消息路由。任一命中即 409 `RESOURCE_SCOPE_IN_USE` 并点名，
//     **不提供 force、不自动解绑**——静默收缩等于在别人的运行里抽掉资源。
//
// ② 是**无条件**执行的防御性复核（R4.6 末条）：投影是派生视图，在
// `projection_error`、`archive_legacy_unrecorded` 或写后重建失败时都可能是空 / 陈旧
// 的；正文扫描不依赖投影，因此不需要先判断"投影可信吗"再决定扫不扫——扫描失败即
// fail-closed 拒绝转换。
//
// 检查与写入在 `ResourceStore.ConvertScope` 的同一事务里完成；本文件做请求校验、
// 目标 Agent 解析、定义文件扫描与端口取证，不自己写 SQL。
// This file orchestrates scope conversion. Narrowing a shared resource must prove
// nobody else still uses it through four evidence classes; the document scan runs
// unconditionally because a projection can be empty or stale, and the database checks
// and the write share one transaction.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// MessageRoutePort 报告把某资源挂在消息路由上的接入点（当前实现返回空：
// 消息接入属扩展阶段，本轮只留端口边界）。端口报错按"可能引用"处理，转换拒绝。
// MessageRoutePort reports message routes that reference one resource; this revision
// has no routes yet, but a port error still fails the conversion closed.
type MessageRoutePort interface {
	RoutesBlocking(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) ([]string, error)
}

// AgentDirectory 解析 Agent 名 → 稳定身份与生命周期状态（索引是状态文件的可重建镜像）。
// `*storage.ResourceStore` 满足本接口。
// AgentDirectory resolves an agent name to its identity and lifecycle state.
type AgentDirectory interface {
	AgentIdentity(ctx context.Context, agent string) (core.AgentUID, string, bool, error)
	LifecycleIndex(ctx context.Context, agent string) (core.LifecycleRecord, bool, string, error)
}

// ScopeRequest 是一次范围转换请求（HTTP 体的领域形态）。
// ResourceUID 必填：按名称猜测目标会把动作落到同名重建的新实例上。
// ScopeRequest is one scope conversion; the resource uid is mandatory.
type ScopeRequest struct {
	Kind        core.ResourceKind
	Name        string
	ResourceUID core.ResourceUID
	Scope       core.ResourceScope
	OwnerAgent  string // Scope=exclusive 时必填
	OperationID string
}

// ScopeResult 是转换后的事实（web 层据此重新读取视图与 ETag）。
// ScopeResult is the post-conversion state.
type ScopeResult struct {
	ResourceUID   core.ResourceUID
	Scope         core.ResourceScope
	OwnerAgent    string
	OwnerAgentUID core.AgentUID
	Revision      int
}

// ScopeService 编排范围转换：请求校验、目标 Agent 解析、定义文件扫描、路由取证、
// 原子执行。
// ScopeService validates, resolves the target agent, scans definitions, gathers route
// evidence and performs the atomic switch.
type ScopeService struct {
	Resources *storage.ResourceStore
	Agents    AgentDirectory
	// WorkspaceRoot 是工作区根（`ScanAgentDocuments` 以它为界扫活动与归档定义）。
	// 为空时转专属直接拒绝：证明不了"没人引用"就不得收缩。
	WorkspaceRoot string
	Routes        MessageRoutePort // nil = 本轮没有消息路由
	Now           func() time.Time
}

// Convert 执行一次范围转换。失败零写入：校验、解析与取证都在事务之前，事务内
// 任一阻断即整笔回滚。
// Convert performs one scope conversion; a failure writes nothing.
func (s *ScopeService) Convert(ctx context.Context, req ScopeRequest) (ScopeResult, error) {
	req, err := normalizeScopeRequest(req)
	if err != nil {
		return ScopeResult{}, err
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}

	var ownerUID core.AgentUID
	var documentRefs []string
	if req.Scope == core.ScopeExclusive {
		ownerUID, err = s.resolveOwner(ctx, req.OwnerAgent)
		if err != nil {
			return ScopeResult{}, err
		}
		documentRefs, err = s.documentRefs(req.Name, req.OwnerAgent)
		if err != nil {
			return ScopeResult{}, err
		}
	}

	blockers, err := s.routeBlockers(ctx, req.Kind, req.ResourceUID)
	if err != nil {
		return ScopeResult{}, err
	}

	usage, err := s.Resources.ConvertScope(ctx, storage.ScopeConversion{
		Kind:          req.Kind,
		Name:          req.Name,
		ExpectUID:     req.ResourceUID,
		Target:        req.Scope,
		OwnerAgentUID: ownerUID,
		DocumentRefs:  documentRefs,
		RouteBlockers: blockers,
		OperationID:   req.OperationID,
		At:            now,
	})
	if errors.Is(err, core.ErrResourceScopeInUse) {
		return ScopeResult{}, inUseError(req, usage)
	}
	if err != nil {
		return ScopeResult{}, err
	}

	return ScopeResult{
		ResourceUID:   req.ResourceUID,
		Scope:         req.Scope,
		OwnerAgent:    req.OwnerAgent,
		OwnerAgentUID: ownerUID,
		Revision:      usageRevision(ctx, s.Resources, req),
	}, nil
}

// normalizeScopeRequest 校验请求形状：只覆盖带范围的两类资源、名称与身份必填、
// 范围字面量逐字、专属必须带属主、共享不得带属主、操作标识必填。
// normalizeScopeRequest validates the request shape before any lookup.
func normalizeScopeRequest(req ScopeRequest) (ScopeRequest, error) {
	switch req.Kind {
	case core.ResourceModelConnection, core.ResourceNotifyChannel:
	default:
		return ScopeRequest{}, fmt.Errorf("范围转换只覆盖 model_connection / notify_channel，收到 %q",
			req.Kind)
	}
	if req.Name == "" {
		return ScopeRequest{}, errors.New("范围转换: 名称不能为空")
	}
	uid, err := core.ParseResourceUID(string(req.ResourceUID))
	if err != nil {
		return ScopeRequest{}, fmt.Errorf("范围转换 %s/%s: 必须显式给出 resource_uid: %w",
			req.Kind, req.Name, err)
	}
	req.ResourceUID = uid

	scope, err := core.ParseScope(string(req.Scope))
	if err != nil {
		return ScopeRequest{}, fmt.Errorf("范围转换 %s/%s: %w", req.Kind, req.Name, err)
	}
	req.Scope = scope
	switch scope {
	case core.ScopeExclusive:
		if req.OwnerAgent == "" {
			return ScopeRequest{}, fmt.Errorf("范围转换 %s/%s: 转专属必须给出 owner_agent",
				req.Kind, req.Name)
		}
	case core.ScopeShared:
		if req.OwnerAgent != "" {
			return ScopeRequest{}, fmt.Errorf("范围转换 %s/%s: 共享范围不得带 owner_agent",
				req.Kind, req.Name)
		}
	}
	if req.OperationID == "" {
		return ScopeRequest{}, fmt.Errorf("范围转换 %s/%s: 操作标识不能为空", req.Kind, req.Name)
	}
	return req, nil
}

// RequestedScope 解析**创建**请求声明的使用范围：缺省 shared；exclusive 必须给属主，
// shared 不得带属主；属主按范围转换的同一口径解析（存在、可证明身份、未归档）。
// 形状错误由 `action` 归属成 400，属主解析失败沿用 404 / 409。
// RequestedScope validates a create-time scope request with the same owner rules the
// conversion uses. Shape errors are attributable 400s; owner lookups keep their status.
func (s *ScopeService) RequestedScope(ctx context.Context, action, resource, rawScope,
	owner string,
) (core.ResourceScope, core.AgentUID, error) {
	rawScope = strings.TrimSpace(rawScope)
	owner = strings.TrimSpace(owner)
	scope := core.ScopeShared
	if rawScope != "" {
		parsed, err := core.ParseScope(rawScope)
		if err != nil {
			return "", "", &core.InvalidInputError{Tool: action, Reason: resource + ": " + err.Error()}
		}
		scope = parsed
	}
	switch scope {
	case core.ScopeShared:
		if owner != "" {
			return "", "", &core.InvalidInputError{Tool: action, Reason: resource + ": 共享范围不得带 owner_agent"}
		}
		return core.ScopeShared, "", nil
	default:
		if owner == "" {
			return "", "", &core.InvalidInputError{Tool: action, Reason: resource + ": 专属范围必须给出 owner_agent"}
		}
		uid, err := s.resolveOwner(ctx, owner)
		if err != nil {
			return "", "", err
		}
		return core.ScopeExclusive, uid, nil
	}
}

// checkScopeUpdate 拒绝在**更新**里切换使用范围：契约把 `scope` 定为创建时可选，
// 后续切换必须走 `/resources/{kind}/{name}/scope`（同一道校验与操作字面量）。
// 与当前状态一致的重复声明按幂等接受——"读出来再整份提交"不该被无谓拒绝。
// checkScopeUpdate rejects a scope switch on update unless it matches the current
// state exactly; the dedicated endpoint owns every real transition.
func checkScopeUpdate(ctx context.Context, scopes *ScopeService, action, rawScope, owner string,
	current core.ResourceScope, currentOwner core.AgentUID,
) error {
	rawScope = strings.TrimSpace(rawScope)
	owner = strings.TrimSpace(owner)
	if rawScope == "" && owner == "" {
		return nil
	}
	declared := core.ScopeShared
	if rawScope != "" {
		parsed, err := core.ParseScope(rawScope)
		if err != nil {
			return &core.InvalidInputError{Tool: action, Reason: err.Error()}
		}
		declared = parsed
	}
	if declared != current {
		return scopeOnUpdateError(action)
	}
	if declared == core.ScopeShared {
		if owner != "" {
			return &core.InvalidInputError{Tool: action, Reason: "共享范围不得带 owner_agent"}
		}
		return nil
	}
	if owner == "" || scopes == nil {
		return nil
	}
	uid, err := scopes.ResolveOwner(ctx, owner)
	if err != nil {
		return err
	}
	if uid != currentOwner {
		return scopeOnUpdateError(action)
	}
	return nil
}

// scopeOnUpdateError 是更新里切换范围的固定 400：指向专用端点，不回显当前属主。
// scopeOnUpdateError points callers at the dedicated scope endpoint.
func scopeOnUpdateError(action string) error {
	return &core.InvalidInputError{
		Tool: action,
		Reason: "使用范围只能在创建时声明；切换请调用 " +
			"POST /resources/{kind}/{name}/scope",
	}
}

// ResolveOwner 是 `resolveOwner` 的导出形态：资源**创建**时的专属属主在写行之前解析
// （创建失败零残余，不能先建共享行再转换）。判定口径与范围转换完全一致——存在、
// 可证明身份、未归档。
// ResolveOwner exposes the same owner resolution the scope conversion uses, so a
// create-with-exclusive resolves the owner before any row is written.
func (s *ScopeService) ResolveOwner(ctx context.Context, agent string) (core.AgentUID, error) {
	return s.resolveOwner(ctx, agent)
}

// resolveOwner 解析目标 Agent：必须存在、有可证明的稳定身份，且**未归档**——
// 归档 Agent 不可能再运行，把共享资源划给它等于让资源对所有人不可用（停用可接受：
// 停用是暂时状态，恢复启用后资源仍然可用）。
// resolveOwner resolves the target agent; an archived agent may not own a resource.
func (s *ScopeService) resolveOwner(ctx context.Context, agent string) (core.AgentUID, error) {
	if s.Agents == nil {
		return "", errors.New("范围转换: 缺少 Agent 目录，无法解析属主")
	}
	uid, _, found, err := s.Agents.AgentIdentity(ctx, agent)
	if err != nil {
		return "", fmt.Errorf("范围转换: 解析属主 %s: %w", agent, err)
	}
	if !found || uid == "" {
		return "", fmt.Errorf("%w: 属主 Agent %s 不存在或没有可证明的身份",
			core.ErrResourceNotFound, agent)
	}

	rec, valid, errMsg, err := s.Agents.LifecycleIndex(ctx, agent)
	if err != nil {
		return "", fmt.Errorf("范围转换: 读取属主 %s 的生命周期: %w", agent, err)
	}
	if !valid {
		// 索引不可用 → 无法证明"未归档"，按失败关闭拒绝（宁可要求先修索引）。
		return "", fmt.Errorf("%w: 属主 Agent %s 的状态不可用（%s）",
			core.ErrConflict, agent, errMsg)
	}
	if rec.State == core.AgentArchived {
		return "", fmt.Errorf("%w: 属主 Agent %s 已归档，不能接收专属资源", core.ErrConflict, agent)
	}
	return uid, nil
}

// documentRefs 按整词扫描工作区里的全部 `AGENT.md`（活动 + 归档），返回引用资源名
// 的文档点名。扫描失败即拒绝转换：证明不了"没人引用"时不得收缩。
// documentRefs scans every AGENT.md and returns the documents that name the resource;
// a scan failure refuses the conversion.
func (s *ScopeService) documentRefs(name, ownerAgent string) ([]string, error) {
	if strings.TrimSpace(s.WorkspaceRoot) == "" {
		return nil, errors.New("范围转换: 缺少工作区根目录，无法扫描 Agent 定义")
	}
	documents, err := workspace.ScanAgentDocuments(s.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("范围转换: 扫描 Agent 定义失败: %w", err)
	}
	return scopeDocumentRefs(documents, name, ownerAgent), nil
}

// scopeDocumentRefs 判定哪些文档在正文里整词提到了资源名：
//   - 属主自己的**活动**定义不是阻断项——转换完成后它仍是合法引用者；归档的
//     同名文档照算（归档身份不可信时宁可多挡一次）；
//   - 归档文档沿用扫描给出的 `archive:<id>` 点名，不自行编造 Agent 名；
//   - 结果升序去重，同一份输入两次调用得到同一顺序。
//
// 正文匹配不看 frontmatter 是否可解析：解析失败的文档照样参与（fail-closed）。
// scopeDocumentRefs names every document whose body mentions the resource as a whole
// word; the owner's own active definition is excluded, archives keep their id label.
func scopeDocumentRefs(documents []workspace.AgentDocument, name, ownerAgent string) []string {
	refs := make([]string, 0, len(documents))
	for _, doc := range documents {
		if !doc.Archived && ownerAgent != "" && doc.Name == ownerAgent {
			continue
		}
		if !workspace.WholeWordMatch(doc.Text, name) {
			continue
		}
		if doc.Archived {
			refs = append(refs, "archive:"+doc.ArchiveID)
			continue
		}
		refs = append(refs, doc.Name)
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

// routeBlockers 调消息路由端口取证；端口报错即拒绝（"可能引用"不得当作没有引用）。
// routeBlockers asks the message-route port; a port error fails the conversion closed.
func (s *ScopeService) routeBlockers(ctx context.Context, kind core.ResourceKind,
	uid core.ResourceUID,
) ([]string, error) {
	if s.Routes == nil {
		return nil, nil
	}
	routes, err := s.Routes.RoutesBlocking(ctx, kind, uid)
	if err != nil {
		return nil, fmt.Errorf("范围转换: 读取消息路由引用: %w", err)
	}
	sort.Strings(routes)
	return routes, nil
}

// inUseError 把占用证据拼成可点名的 409 错误：绑定者、定义文件引用者、未释放运行、
// 消息路由分组列出。
// inUseError renders the blockers into one addressable error.
func inUseError(req ScopeRequest, usage core.ScopeUsage) error {
	parts := make([]string, 0, 4)
	if len(usage.BoundAgents) > 0 {
		uids := make([]string, 0, len(usage.BoundAgents))
		for _, uid := range usage.BoundAgents {
			uids = append(uids, string(uid))
		}
		parts = append(parts, "绑定的 Agent: "+strings.Join(uids, "、"))
	}
	if len(usage.DocumentRefs) > 0 {
		parts = append(parts, "定义文件的引用: "+strings.Join(usage.DocumentRefs, "、"))
	}
	if len(usage.ActiveRuns) > 0 {
		parts = append(parts, "未释放的运行: "+strings.Join(usage.ActiveRuns, "、"))
	}
	if len(usage.MessageRoutes) > 0 {
		parts = append(parts, "消息路由: "+strings.Join(usage.MessageRoutes, "、"))
	}
	return fmt.Errorf("%w: %s/%s 转专属被拒绝（%s）",
		core.ErrResourceScopeInUse, req.Kind, req.Name, strings.Join(parts, "；"))
}

// usageRevision 在转换成功后重读指针拿到新的 revision（web 层据此算 ETag）。
// 读失败不改变已提交的转换结果：返回 0，由调用方重新读取资源视图补齐。
// usageRevision re-reads the pointer revision after a successful conversion.
func usageRevision(ctx context.Context, resources *storage.ResourceStore,
	req ScopeRequest,
) int {
	if req.Kind == core.ResourceNotifyChannel || resources == nil {
		return 0
	}
	ptr, err := resources.Pointer(ctx, req.Kind, req.Name)
	if err != nil {
		return 0
	}
	return ptr.Revision
}
