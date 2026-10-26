package app

// 本文件是两个**资源状态动作**端点的装配层编排（模块卡 R4.4 / R4.8 / T139）：
//   - `POST /resources/{kind}/{name}/credential/revoke`：按名称解析活动资源 → 校验
//     `resource_uid` 与当前身份一致 → 校验 `If-Match` 与**当前条件 ETag**一致 →
//     委托 `CredentialOps.Revoke`（它自己预写 `*.credential.revoke` 操作行）→
//     回读撤销后的条件 ETag 与凭据版本数；
//   - `POST /resources/{kind}/{name}/scope`：同一段身份 / ETag 前置校验 → 解析属主
//     （不存在 404 / 已归档 409，都排在预写之前）→ 预写 `*.scope` 操作行（`detail_json`
//     带四证：revision 前后值、范围与属主前后值）→ 委托 `ScopeService.Convert`
//     （占用检查与写入在同一事务）→ 回填终态 → 回读 revision 与 ETag。
//
// 为什么需要本文件：两个端点的手工动作各自缺一半——
//   - `ScopeService.Convert` 要求调用方给出操作标识，却**不写**操作行（它被保存路径
//     复用，那里的行由 `ResourceAdmin` 预写）；直接把端点接到上面会留下"没有 pending
//     行"的写，违反 FR-001；
//   - `CredentialOps.Revoke` 既不识别条件写，也不知道渠道 ETag 配方。
//
// 因此「校验 → 预写 → 执行 → 回填 → 回读」在本层收成一处，web 只解码与投影。
//
// 纪律：
//   - 纯请求校验（类别 / 名称 / UID / 范围字面量 / 属主存在性）与条件 ETag 比较都在
//     预写之前：412 / 409 / 404 / 400 都不留操作行（FR-001 / FR-003）；
//   - 预写之后任何失败都回填 failed 并带 `operation_id`（FR-001）；
//   - 锁序：取资源写面的**同一张**按键互斥表（`ResourceAdmin.LockResource`），不新建
//     第二套互斥（FR-041）；
//   - `detail_json` 只记元数据（名称 / UID / revision 前后值 / 范围与属主 / 凭据 ID 与
//     版本号），不含明文与完整地址。
//
// This file orchestrates the revoke and scope endpoints. The scope service needs an
// operation id without writing the row, and the revoke path ignores conditional writes,
// so validation, the pending row, execution, the terminal state and the read-back all
// live here. Pure request checks and the ETag comparison precede the pending row.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// resourceStateTool 是请求形状错误（类别 / 名称 / UID / 范围 / confirm）的工具名：
// 两个动作各有自己的操作字面量，但形状错误发生在知道类别之前，统一挂这个标签。
// resourceStateTool labels shape errors raised before the action literal is known.
const resourceStateTool = "resource.state"

// ResourceStateDeps 是状态动作编排的装配输入；六个依赖缺一即拒绝装配。
// ResourceStateDeps wires the state orchestrator; every dependency is required.
type ResourceStateDeps struct {
	Resources     *storage.ResourceStore
	Credentials   *storage.CredentialStore
	Operations    core.OperationStore
	Admin         *ResourceAdmin
	Scopes        *ScopeService
	CredentialOps *CredentialOps
	// Channels 是通知渠道的管理读面（`notify_read.go`）：渠道 ETag 的**唯一**配方，
	// 与列表 / 写后回显 / 条件写中间件读同一个值。
	// Channels is the one channel ETag recipe shared with the console and the routes.
	Channels *notifyChannelReadPort
	// Agents 给出注册表快照（影响提示用）；读失败只降级为空列表并记 WARN——写入已经
	// 提交，提示不该把它变成失败。
	// Agents supplies the registry snapshot for the impact hint; a failure degrades it.
	Agents func() ([]core.AgentEntry, error)
	// MCPRevoker 在 MCP 槽位凭据撤销后立即回收该 server 的连接（14D C8）。nil = 本进程
	// 没有 MCP 运行时（只读装配 / 单元测试），撤销照常记账，只是没有连接可回收。
	// MCPRevoker tears down the server's connection after an MCP slot revoke; nil means
	// there is no MCP runtime in this process.
	MCPRevoker func(ctx context.Context, name string) error
	Logger     *slog.Logger
	Now        func() time.Time
}

// ResourceStateService 是两个状态动作的编排实现；字段构造后不再变化。
// ResourceStateService is the only implementation of the two state actions.
type ResourceStateService struct {
	deps ResourceStateDeps
}

// NewResourceStateService 组装状态动作编排；缺必需依赖即拒绝构造。
// NewResourceStateService refuses to construct without its required stores.
func NewResourceStateService(deps ResourceStateDeps) (*ResourceStateService, error) {
	if deps.Resources == nil || deps.Credentials == nil || deps.Operations == nil ||
		deps.Admin == nil || deps.Scopes == nil || deps.CredentialOps == nil ||
		deps.Channels == nil {
		return nil, errors.New("资源状态动作装配缺少必需依赖")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &ResourceStateService{deps: deps}, nil
}

// CredentialRevokeCommand 是一次撤销的线上请求（`resource_uid` 必填、`confirm` 必须为
// true、`If-Match` 是中间件已校验过形状的强 ETag 原件）。`BindingKey` 只在
// `kind=mcp_server` 时必填：MCP 的托管凭据按槽位寻址，一个 server 可以有多个槽位。
// CredentialRevokeCommand is one revoke request; BindingKey addresses the MCP slot.

type CredentialRevokeCommand struct {
	Kind        string
	Name        string
	ResourceUID string
	BindingKey  string
	Confirm     bool
	IfMatch     string
}

// CredentialRevokeOutcome 是撤销后的公开事实；`ETag` 只进响应头（契约的
// `CredentialRevokeResult` 没有 etag 字段）。
// CredentialRevokeOutcome carries the post-revoke facts; ETag is header-only.
type CredentialRevokeOutcome struct {
	ResourceUID core.ResourceUID
	// BindingKey 是本次撤销的槽位名（仅 `kind=mcp_server` 非空）：撤销的粒度是单个
	// 占位符，回执必须点名它，否则页面无法判断"撤的是哪一个"。
	BindingKey      string
	CredentialID    string
	RevocationEpoch int
	RevokedVersions int
	OperationID     string
	ETag            string
}

// ResourceScopeCommand 是一次范围切换的线上请求。
// ResourceScopeCommand is one scope conversion request.
type ResourceScopeCommand struct {
	Kind        string
	Name        string
	ResourceUID string
	Scope       string
	OwnerAgent  string
	IfMatch     string
}

// ResourceScopeOutcome 是切换后的公开事实：`Revision` 与 `ETag` 来自写后回读（回读
// 失败时 `Revision` 退回可证明的 `写前 + 1`，`ETag` 为空——页面随后重新 GET）。
// ResourceScopeOutcome is the post-conversion state re-read after the write.
type ResourceScopeOutcome struct {
	ResourceUID            core.ResourceUID
	Scope                  core.ResourceScope
	OwnerAgent             string
	Revision               int
	ETag                   string
	AffectedEligibleAgents []string
}

// resourceStateRow 是目标行上本动作需要的提交事实与条件 ETag。
// resourceStateRow carries the commit facts and the conditional ETag of one target.
type resourceStateRow struct {
	kind          core.ResourceKind
	name          string
	uid           core.ResourceUID
	revision      int
	scope         core.ResourceScope
	ownerAgentUID core.AgentUID
	etag          string
}

// CurrentETag 解析两个状态动作端点的当前条件 ETag（形状级 412 的 `data.current_etag`）。
// 只读：不写操作行、不触达定义文件。类别非法 / 资源不存在 / 读面缺失一律返回 false。
// CurrentETag resolves the conditional ETag for the shape-level 412; it is read-only.
func (s *ResourceStateService) CurrentETag(ctx context.Context, rawKind, name string) (string, bool) {
	kind, err := stateKind(rawKind, resourceStateTool)
	if err != nil {
		return "", false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	state, err := s.loadState(ctx, kind, name, resourceStateTool)
	if err != nil || state.etag == "" {
		return "", false
	}
	return state.etag, true
}

// RevokeCredential 执行一次撤销：前置校验 → 委托凭据写面 → 回读结论。
// RevokeCredential performs one revoke; every pure check precedes the pending row.
func (s *ResourceStateService) RevokeCredential(ctx context.Context, actor core.Actor,
	cmd CredentialRevokeCommand,
) (CredentialRevokeOutcome, error) {
	kind, err := stateKind(cmd.Kind, resourceStateTool)
	if err != nil {
		return CredentialRevokeOutcome{}, err
	}
	action := credentialRevokeAction(core.BindingKind(kind))
	name, err := stateName(cmd.Name, action)
	if err != nil {
		return CredentialRevokeOutcome{}, err
	}
	if !cmd.Confirm {
		return CredentialRevokeOutcome{}, &core.InvalidInputError{
			Tool:   action,
			Reason: "撤销必须显式确认：请求体须带 confirm: true",
		}
	}

	unlock := s.deps.Admin.LockResource(kind, name)
	defer unlock()

	state, err := s.loadState(ctx, kind, name, action)
	if err != nil {
		return CredentialRevokeOutcome{}, err
	}
	if err := s.checkPrecondition(state, cmd.ResourceUID, cmd.IfMatch, action); err != nil {
		return CredentialRevokeOutcome{}, err
	}

	result, err := s.deps.CredentialOps.Revoke(ctx, CredentialRevokeRequest{
		Actor:      actor,
		Kind:       core.BindingKind(kind),
		Name:       name,
		UID:        state.uid,
		BindingKey: cmd.BindingKey,
		IfMatch:    cmd.IfMatch,
	})
	if err != nil {
		return CredentialRevokeOutcome{}, err
	}

	// MCP 撤销**立即**生效（C8）：不等一次 `apply`。门先关死（之后的调用拒绝发出，而不
	// 是发出去再收 401），连接随后回收。凭据事实已经提交，因此回收失败只记 ERROR、不把
	// 撤销说成失败——拒绝面已经由封门保证；进程残留要能被排障看见。
	// An MCP revocation takes effect immediately: the gate closes before the connection,
	// and a close failure never turns the committed revocation into a failure.
	if kind == core.ResourceMCPServer && s.deps.MCPRevoker != nil {
		if revokeErr := s.deps.MCPRevoker(ctx, name); revokeErr != nil {
			s.log().Error("MCP 凭据已撤销，但回收连接失败（门已关闭，调用不会再发出）",
				"resource", name, "operation_id", result.OperationID, "error", revokeErr)
		}
	}

	out := CredentialRevokeOutcome{
		ResourceUID:     state.uid,
		BindingKey:      strings.TrimSpace(cmd.BindingKey),
		CredentialID:    result.CredentialID,
		RevocationEpoch: result.RevocationEpoch,
		OperationID:     result.OperationID,
		RevokedVersions: s.countCredentialVersions(ctx, result.CredentialID),
		ETag:            s.readBackETag(ctx, kind, name),
	}
	return out, nil
}

// ConvertScope 执行一次范围切换：前置校验 → 属主解析 → 预写 → 原子转换 → 回填。
// ConvertScope performs one scope conversion with the pending row written before the
// transactional switch.
func (s *ResourceStateService) ConvertScope(ctx context.Context, actor core.Actor,
	cmd ResourceScopeCommand,
) (ResourceScopeOutcome, error) {
	kind, err := stateKind(cmd.Kind, resourceStateTool)
	if err != nil {
		return ResourceScopeOutcome{}, err
	}
	if kind == core.ResourceMCPServer {
		// MCP server 可持有托管凭据，但使用范围切换不在本期范围：与其把动作写成
		// `model_connection.scope` 留下一条错的审计行，不如在预写之前明确拒绝。
		return ResourceScopeOutcome{}, &core.InvalidInputError{
			Tool:   resourceStateTool,
			Reason: "MCP server 不支持使用范围切换（只支持凭据撤销）",
		}
	}
	action := scopeAction(core.BindingKind(kind))
	name, err := stateName(cmd.Name, action)
	if err != nil {
		return ResourceScopeOutcome{}, err
	}
	scope, owner, err := parseScopeCommand(cmd, action)
	if err != nil {
		return ResourceScopeOutcome{}, err
	}

	unlock := s.deps.Admin.LockResource(kind, name)
	defer unlock()

	state, err := s.loadState(ctx, kind, name, action)
	if err != nil {
		return ResourceScopeOutcome{}, err
	}
	if err := s.checkPrecondition(state, cmd.ResourceUID, cmd.IfMatch, action); err != nil {
		return ResourceScopeOutcome{}, err
	}

	// 属主解析排在预写之前：不存在（404）与已归档（409）是纯目标检查，不得留操作行。
	var ownerUID core.AgentUID
	if scope == core.ScopeExclusive {
		ownerUID, err = s.deps.Scopes.ResolveOwner(ctx, owner)
		if err != nil {
			return ResourceScopeOutcome{}, err
		}
	}

	// 影响名单只算一次：响应提示与操作记录必须是同一份（M-3）。它读的是 Agent 目录，
	// 写在事务外——与本动作是否真正落库无关。
	affected := s.affectedEligibleAgents(kind, scope, owner)

	// 幂等重放：目标已经是请求的范围与属主时不新开操作行、不推进 revision——
	// `writeScopeTx` 无条件 `revision + 1`，对着同一目标重放只会让 ETag 无谓漂移。
	if normalizedScope(state.scope) == scope && state.ownerAgentUID == ownerUID {
		return ResourceScopeOutcome{
			ResourceUID:            state.uid,
			Scope:                  scope,
			OwnerAgent:             owner,
			Revision:               state.revision,
			ETag:                   state.etag,
			AffectedEligibleAgents: affected,
		}, nil
	}

	now := s.deps.Now()
	operationID := core.NewOperationID(now)
	detailJSON, err := json.Marshal(scopeActionDetail(state, scope, ownerUID, affected))
	if err != nil {
		// 结构只有字符串与整数，此分支不可达；仍不吞错误，按预写失败处理。
		return ResourceScopeOutcome{}, fmt.Errorf("%s 序列化预写详情失败，拒绝执行: %w",
			action, err)
	}
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   string(kind),
		TargetID:     string(state.uid),
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  scopeRequestHash(kind, name, state.uid, scope, ownerUID),
		ExpectedETag: cmd.IfMatch,
		Status:       core.OperationPending,
		DetailJSON:   detailJSON,
		CreatedAt:    now,
	}
	if beginErr := s.deps.Operations.BeginOperation(ctx, record); beginErr != nil {
		s.log().Error("范围切换预写操作记录失败，拒绝执行",
			"kind", string(kind), "resource", name, "action", action, "error", beginErr)
		return ResourceScopeOutcome{}, fmt.Errorf("%s 预写操作记录失败，拒绝执行: %w",
			action, beginErr)
	}

	result, err := s.deps.Scopes.Convert(ctx, ScopeRequest{
		Kind:        kind,
		Name:        name,
		ResourceUID: state.uid,
		Scope:       scope,
		OwnerAgent:  owner,
		OperationID: operationID,
	})
	if err != nil {
		s.log().Error("资源范围切换失败",
			"kind", string(kind), "resource", name, "operation_id", operationID, "error", err)
		s.finishOperation(ctx, operationID, core.OperationFailed, "切换资源使用范围失败", nil)
		return ResourceScopeOutcome{}, scopeConversionFailure(operationID, err)
	}
	// 成功终态带同一份 detail（含影响名单）：操作记录里的"告诉过谁"与响应一致。
	s.finishOperation(ctx, operationID, core.OperationSuccess, "", detailJSON)

	out := ResourceScopeOutcome{
		ResourceUID:            result.ResourceUID,
		Scope:                  result.Scope,
		OwnerAgent:             owner,
		Revision:               state.revision + 1,
		AffectedEligibleAgents: affected,
	}
	// 回读权威 revision 与 ETag：`usageRevision` 对渠道恒为 0，只有重读读面才能给出
	// 与下一次条件写一致的值。读失败不改写已提交的结果，只降级（ETag 为空）。
	if after, readErr := s.loadState(ctx, kind, name, action); readErr == nil {
		out.Revision = after.revision
		out.ETag = after.etag
	} else {
		s.log().Error("范围切换成功但回读目标失败",
			"kind", string(kind), "resource", name, "operation_id", operationID, "error", readErr)
	}
	return out, nil
}

// stateKind 解析路径里的资源类别：接受**凭据属主**三类（`mcp_server` 自 v6 起能持有
// 托管凭据，因此撤销面必须认它）。它比绑定类别宽一档，但仍不等于"任意资源"。
// `mcp_server` 只在本文件的撤销分支可用；使用范围切换的闭集仍是两类，由调用方再判。
// stateKind accepts the three credential owner kinds; the scope surface still refuses MCP.
func stateKind(raw, action string) (core.ResourceKind, error) {
	raw = strings.TrimSpace(raw)
	kind, err := core.ParseCredentialOwnerKind(raw)
	if err != nil {
		return "", &core.InvalidInputError{
			Tool:   action,
			Reason: "资源类别只接受 model_connection / notify_channel / mcp_server，收到 " + raw,
		}
	}
	return core.ResourceKind(kind), nil
}

// stateName 校验路径资源名非空：名称是本次动作的唯一寻址入口。
// stateName requires a non-empty path name.
func stateName(raw, action string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", &core.InvalidInputError{Tool: action, Reason: "资源名不得为空"}
	}
	return name, nil
}

// parseScopeCommand 校验范围请求的形状：字面量逐字、专属必须带属主、共享不得带属主。
// 迁移 / 归属判定留给 `ScopeService.Convert` 与仓储事务。
// parseScopeCommand validates the scope body shape; ownership rules stay upstream.
func parseScopeCommand(cmd ResourceScopeCommand, action string) (core.ResourceScope, string, error) {
	scope, err := core.ParseScope(strings.TrimSpace(cmd.Scope))
	if err != nil {
		return "", "", &core.InvalidInputError{Tool: action, Reason: err.Error()}
	}
	owner := strings.TrimSpace(cmd.OwnerAgent)
	switch scope {
	case core.ScopeExclusive:
		if owner == "" {
			return "", "", &core.InvalidInputError{
				Tool:   action,
				Reason: "转专属必须给出 owner_agent",
			}
		}
	case core.ScopeShared:
		if owner != "" {
			return "", "", &core.InvalidInputError{
				Tool:   action,
				Reason: "共享范围不得带 owner_agent",
			}
		}
	}
	return scope, owner, nil
}

// loadState 读取目标行的提交事实与条件 ETag：模型连接读发布指针 + 资源写面的
// `CurrentETag`，通知渠道读管理读面（它补齐凭据段并给出渠道 ETag）。墓碑按不存在处理。
// loadState reads the commit facts and conditional ETag; a tombstone counts as absent.
func (s *ResourceStateService) loadState(ctx context.Context, kind core.ResourceKind,
	name, action string,
) (resourceStateRow, error) {
	switch kind {
	case core.ResourceModelConnection, core.ResourceMCPServer:
		// 两类共用同一配方：指针 + 资源写面的条件 ETag（MCP 的 ETag 由定义版本、凭据摘要
		// 与 scope / revision 共同决定，与 GET 单条读的是同一个值）。
		pointer, err := s.deps.Resources.Pointer(ctx, kind, name)
		if errors.Is(err, core.ErrResourceNotFound) {
			return resourceStateRow{}, fmt.Errorf("%w: %s %s", core.ErrResourceNotFound, kind, name)
		}
		if err != nil {
			return resourceStateRow{}, err
		}
		if pointer.DeletedAt != nil {
			return resourceStateRow{}, fmt.Errorf("%w: %s %s（已删除）",
				core.ErrResourceNotFound, kind, name)
		}
		etag, ok := s.deps.Admin.CurrentETag(ctx, kind, name)
		if !ok {
			return resourceStateRow{}, fmt.Errorf("读取 %s %s 的条件 ETag 失败", kind, name)
		}
		return resourceStateRow{
			kind: kind, name: name, uid: pointer.ResourceUID, revision: pointer.Revision,
			scope: pointer.Scope, ownerAgentUID: pointer.OwnerAgentUID, etag: etag,
		}, nil
	case core.ResourceNotifyChannel:
		channel, err := s.deps.Channels.Channel(ctx, name)
		if err != nil {
			return resourceStateRow{}, err
		}
		return resourceStateRow{
			kind: kind, name: name, uid: channel.ResourceUID, revision: channel.Revision,
			scope: channel.Scope, ownerAgentUID: channel.OwnerAgentUID, etag: channel.ETag,
		}, nil
	default:
		return resourceStateRow{}, &core.InvalidInputError{
			Tool:   action,
			Reason: "资源类别只接受 model_connection / notify_channel / mcp_server",
		}
	}
}

// checkPrecondition 先比身份、再比条件 ETag：`resource_uid` 与当前实例不一致是 409
// `RESOURCE_UID_MISMATCH`（不按名猜目标），If-Match 不匹配是 412 并给出当前值。
// checkPrecondition compares the identity first, then the If-Match.
func (s *ResourceStateService) checkPrecondition(state resourceStateRow, rawUID, ifMatch,
	action string,
) error {
	uid, err := core.ParseResourceUID(strings.TrimSpace(rawUID))
	if err != nil {
		return &core.InvalidInputError{
			Tool:   action,
			Reason: "必须显式给出合法的 resource_uid: " + err.Error(),
		}
	}
	if uid != state.uid {
		return &core.AdminError{
			Kind: core.ErrResourceUIDMismatch,
			Message: fmt.Sprintf(
				"resource_uid 与 %s %s 的当前身份不一致：请重新读取资源后重试",
				state.kind, state.name),
			Data: map[string]any{"resource_uid": string(state.uid)},
		}
	}
	if ifMatch != state.etag {
		return preconditionError(state.etag)
	}
	return nil
}

// scopeActionDetail 组装范围动作的预写详情：范围与属主的前后值、revision 前后值，
// 以及本次切换的影响名单（M-3：与响应同源）。
// `Revision` 必须是**写后**值——`writeScopeTx` 无条件 `revision + 1`，恢复据此判
// applied / not_applied。
// scopeActionDetail builds the pending detail; Revision is the post-write value.
func scopeActionDetail(state resourceStateRow, scope core.ResourceScope,
	ownerUID core.AgentUID, affected []string,
) credentialActionDetail {
	return credentialActionDetail{
		Kind:                   string(state.kind),
		Name:                   state.name,
		ResourceUID:            string(state.uid),
		Revision:               state.revision + 1,
		PreviousRevision:       state.revision,
		Scope:                  string(scope),
		PreviousScope:          string(normalizedScope(state.scope)),
		OwnerAgentUID:          string(ownerUID),
		PreviousOwnerAgentUID:  string(state.ownerAgentUID),
		AffectedEligibleAgents: affected,
	}
}

// scopeAction 给出范围动作的固定字面量（契约 §9.3）。
// scopeAction returns the fixed scope action literal.
func scopeAction(kind core.BindingKind) string {
	if kind == core.BindingNotifyChannel {
		return actionNotifyChannelScope
	}
	return actionModelConnectionScope
}

// scopeRequestHash 摘要一次范围切换：材料只含类别、名称、UID、目标范围与属主 UID。
// scopeRequestHash digests one conversion without any definition body.
func scopeRequestHash(kind core.ResourceKind, name string, uid core.ResourceUID,
	scope core.ResourceScope, ownerUID core.AgentUID,
) string {
	return agentRequestHash(strings.Join([]string{
		string(kind), name, string(uid), string(scope), string(ownerUID),
	}, "\n"))
}

// scopeConversionFailure 把范围转换的失败并成"终态已回填"的对外错误：可分类的保留
// 原码（409 占用 / 409 身份 / 404 目标），其余按内部错误 500；`operation_id` 一并出站。
// scopeConversionFailure keeps the classifiable sentinels and tags the operation id.
func scopeConversionFailure(operationID string, cause error) error {
	data := map[string]any{"operation_id": operationID}
	switch {
	case errors.Is(cause, core.ErrResourceScopeInUse):
		return &core.AdminError{
			Kind:    core.ErrResourceScopeInUse,
			Message: trimSentinel(cause, core.ErrResourceScopeInUse),
			Data:    data,
		}
	case errors.Is(cause, core.ErrResourceUIDMismatch):
		return &core.AdminError{
			Kind:    core.ErrResourceUIDMismatch,
			Message: trimSentinel(cause, core.ErrResourceUIDMismatch),
			Data:    data,
		}
	case errors.Is(cause, core.ErrResourceNotFound):
		return &core.AdminError{
			Kind:    core.ErrResourceNotFound,
			Message: trimSentinel(cause, core.ErrResourceNotFound),
			Data:    data,
		}
	default:
		return operationFailedError(operationID, "切换资源使用范围失败")
	}
}

// trimSentinel 去掉错误文本里的哨兵前缀：哨兵是英文分类语（`errors.Is` 用），
// 不应出现在对外文案里。
// trimSentinel strips the sentinel prefix so the public message stays readable.
func trimSentinel(err error, sentinel error) string {
	text := err.Error()
	prefix := sentinel.Error() + ": "
	if after, ok := strings.CutPrefix(text, prefix); ok {
		return after
	}
	return text
}

// finishOperation 回填范围动作终态：脱离请求取消，失败只记 ERROR（启动恢复查证）。
// `detail` 非空时覆盖预写详情（成功路径带上影响名单）；失败路径传 nil，保留预写的
// 期望值供恢复查证。
// finishOperation records the terminal state detached from request cancellation;
// a non-nil detail replaces the pending one on success.
func (s *ResourceStateService) finishOperation(ctx context.Context, operationID string,
	status core.OperationStatus, message string, detail []byte,
) {
	code := lifecycleResultOK
	if status != core.OperationSuccess {
		code = lifecycleResultInternal
	}
	if err := s.deps.Operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		status, code, message, detail, s.deps.Now()); err != nil {
		s.log().Error("范围切换回填操作终态失败",
			"operation_id", operationID, "status", string(status), "error", err)
	}
}

// countCredentialVersions 回读身份的全部版本数（契约 `revoked_versions`）。写已经成功，
// 读失败只记 ERROR 并给 0——不得把成功改写成失败。
// countCredentialVersions reads the version count back; a read failure is log-only.
func (s *ResourceStateService) countCredentialVersions(ctx context.Context,
	credentialID string,
) int {
	if credentialID == "" {
		return 0
	}
	versions, err := s.deps.Credentials.Versions(ctx, credentialID)
	if err != nil {
		s.log().Error("回读托管凭据版本数失败",
			"credential_id", credentialID, "error", err)
		return 0
	}
	return len(versions)
}

// readBackETag 在成功后重读条件 ETag；读失败记 ERROR 并返回空串（响应头省略）。
// readBackETag re-reads the conditional ETag; a failure degrades to an empty header.
func (s *ResourceStateService) readBackETag(ctx context.Context, kind core.ResourceKind,
	name string,
) string {
	after, err := s.loadState(ctx, kind, name, resourceStateTool)
	if err != nil {
		s.log().Error("状态动作成功但回读条件 ETag 失败",
			"kind", string(kind), "resource", name, "error", err)
		return ""
	}
	return after.etag
}

// affectedEligibleAgents 给出"会被这次收缩影响、但不会被自动修改"的 Agent 名（契约
// `affected_eligible_agents` 的影响提示）：只有通知渠道转专属时才有影响——`all_eligible`
// 模式的已注册 Agent 依赖"共享 ∪ 本人专属"，资源变专属后它们不再有资格；新属主本身
// 不算受影响。模型连接没有逐 Agent 的通知模式，返回空数组。
// affectedEligibleAgents reports all_eligible agents that lose eligibility on a
// shared→exclusive switch; the new owner is excluded and other kinds have none.
func (s *ResourceStateService) affectedEligibleAgents(kind core.ResourceKind,
	scope core.ResourceScope, owner string,
) []string {
	if kind != core.ResourceNotifyChannel || scope != core.ScopeExclusive ||
		s.deps.Agents == nil {
		return []string{}
	}
	entries, err := s.deps.Agents()
	if err != nil {
		s.log().Warn("计算受影响 Agent 失败，影响提示降级为空", "error", err)
		return []string{}
	}
	affected := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.Registered || entry.Profile == nil || entry.Name == owner {
			continue
		}
		if core.EffectiveNotifyMode(entry.Profile.NotifyMode,
			entry.Profile.NotifyChannels) == core.NotifyModeAllEligible {
			affected = append(affected, entry.Name)
		}
	}
	sort.Strings(affected)
	return affected
}

// log 返回装配的 logger，缺省用 slog.Default。
// log returns the assembled logger.
func (s *ResourceStateService) log() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.Default()
	}
	return s.deps.Logger
}
