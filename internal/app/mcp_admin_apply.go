package app

// 本文件是 `mcp_server.apply` 的**应用层适配**：把 HTTP 意图（资源名、`If-Match`、目标版本）
// 变成设计 §2.3.3 前五步的落地调用，再把连接层的结论写成操作行与契约结果对象。
//
// 分工是刻意的：
//   - 第 1–3 步（读指针 / 冻结定义与凭据版本 / 只读预检）在这里——它们都要读库、读环境，
//     且**不得**启动任何进程；
//   - 第 4–8 步（授权、建候选连接、并发校验与 CAS、排空）在 `mcp_apply.go`——那里只管
//     一个 server 的一代连接，不碰操作台账；
//   - 注册池的整组替换由装配层通过 `ReplaceTools` 在同一段临界区里完成，本文件不碰注册池。
// The HTTP-side adapter of a controlled apply: steps 1–3 live here, steps 4–8 live in the
// supervisor, and neither touches the other's state.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// apply 操作行与响应共用的两个业务码（与 `docs/contracts/run-and-errors.md` 的 409 / 500
// 两行逐字一致；Web 层的常量化在同名 code 上，两边同源）。
// The two business codes shared by the operation row and the HTTP response.
const (
	mcpApplyResultSuperseded = "MCP_APPLY_SUPERSEDED"
	mcpApplyResultFailed     = "MCP_APPLY_FAILED"
	// mcpApplyResultCredentialUnavailable 是预检"托管凭据取不到"的 503 业务码。
	// #nosec G101 -- 业务码字面量，不是凭据（变量名命中）。
	mcpApplyResultCredentialUnavailable = "CREDENTIAL_UNAVAILABLE"
)

// ApplyMCPServer 受控地让某个已发布版本**运行生效**（契约 `/mcp-servers/{name}/apply`）。
// `versionID` 为 0 表示取当前 `published_version_id`；显式给出版本号时必须是同一 MCP server
// 的既有版本（回滚即"应用一个更早的版本"），没有草稿分支。
//
// 失败面按契约分成四档，互不吞并：预检拒绝 503（未启动任何进程）、策略拒绝 403（先于
// 建连）、被并发写取代 409、建连 / 注册失败 500。四档都先写 `pending` 操作行，再置终态，
// 响应/操作详情都带 `operation_id`。
// ApplyMCPServer makes a published version the applied one. The four failure faces stay
// distinct, and every one of them is written to the prewritten operation row.
func (s *ResourceAdmin) ApplyMCPServer(ctx context.Context, actor core.Actor, name string,
	ifMatch string, versionID int64,
) (core.MCPServerApplyResult, error) {
	if err := validateResourceID(name); err != nil {
		return core.MCPServerApplyResult{}, &core.InvalidInputError{Tool: core.ActionMCPServerApply, Reason: err.Error()}
	}
	if versionID < 0 {
		return core.MCPServerApplyResult{}, &core.InvalidInputError{
			Tool: core.ActionMCPServerApply, Reason: "version_id 不能为负数",
		}
	}
	if s.supervisor == nil {
		return core.MCPServerApplyResult{}, &core.AdminError{
			Kind:    core.ErrProviderNotConfigured,
			Message: "MCP 运行时尚未装配，无法执行受控发布",
		}
	}

	unlockResource := s.resourceLocks.lock(resourceLockKey(core.ResourceMCPServer, name))
	defer unlockResource()

	// 第 1 步：读指针与目标版本。指针无行 = 资源不存在（404），没有"草稿"分支。
	// Step 1: the pointer and the target version; a missing pointer is a 404.
	pointer, err := s.activePointer(ctx, core.ResourceMCPServer, name)
	if err != nil {
		return core.MCPServerApplyResult{}, err
	}
	published, err := s.store.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return core.MCPServerApplyResult{}, fmt.Errorf("读取 MCP server %s 的发布版本: %w", name, err)
	}
	current, err := publishedResourceETag(ctx, s.credentials, pointer, published)
	if err != nil {
		return core.MCPServerApplyResult{}, err
	}
	if ifMatch != current {
		// 412 零写入、零操作行：条件写没通过就还没"开始做"。
		return core.MCPServerApplyResult{}, preconditionError(current)
	}

	targetID := pointer.PublishedVersionID
	if versionID > 0 {
		targetID = versionID
	}
	targetDefinition := published.DefinitionYAML
	if targetID != pointer.PublishedVersionID {
		target, versionErr := s.store.VersionByID(ctx, targetID)
		if errors.Is(versionErr, core.ErrResourceNotFound) {
			return core.MCPServerApplyResult{}, versionErr
		}
		if versionErr != nil {
			return core.MCPServerApplyResult{}, fmt.Errorf(
				"读取 MCP server %s 的目标版本 %d: %w", name, targetID, versionErr)
		}
		if target.Kind != core.ResourceMCPServer || target.ResourceUID != pointer.ResourceUID {
			return core.MCPServerApplyResult{}, &core.InvalidInputError{
				Tool:   core.ActionMCPServerApply,
				Reason: fmt.Sprintf("版本 %d 不属于 MCP server %s", targetID, name),
			}
		}
		targetDefinition = target.DefinitionYAML
	}

	cfg, err := workspace.ParseMCPServerBlock([]byte(targetDefinition))
	if err != nil {
		return core.MCPServerApplyResult{}, fmt.Errorf("读取 MCP server %s 的目标版本定义: %w", name, err)
	}
	if cfg.Name != name {
		return core.MCPServerApplyResult{}, fmt.Errorf(
			"MCP server %s 的目标版本条目名 %q 与资源标识不一致", name, cfg.Name)
	}

	previous := derefVersionID(pointer.AppliedVersionID)
	window := defaultMCPDrainTimeout
	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID: operationID,
		Action:      core.ActionMCPServerApply,
		TargetKind:  core.TargetKindMCPServer,
		// 凭据与范围动作的 target_id 是资源稳定身份，不是名称（audit 契约逐字规定）。
		TargetID:           string(pointer.ResourceUID),
		ActorSubject:       actor.Subject,
		AuthMethod:         actor.Method,
		Source:             actor.Source,
		RequestHash:        mcpApplyRequestHash(name, targetID),
		RequestedVersionID: targetID,
		Status:             core.OperationPending,
		// 预写就冻结两个版本号（设计 §2.3.3 第 2 步）：崩溃后恢复只能依据这份 detail
		// 判断切换是否真的发生，写 `{}` 会让三态全部落到 unknown。`outcome` 此时为空，
		// 表示"尚无结论"——终态回填会覆盖成 applied / failed / superseded。
		DetailJSON: mcpApplyDetail(core.MCPServerApplyResult{
			TargetVersionID:   targetID,
			PreviousVersionID: previous,
			AppliedVersionID:  previous,
		}),
		CreatedAt: now,
	}
	if err := s.operations.BeginOperation(ctx, record); err != nil {
		// 预写失败不执行、不留行：没有审计就不发动作（原则 5）。
		s.logger.Error("MCP 受控发布预写操作失败，拒绝执行", "resource", name, "error", err)
		return core.MCPServerApplyResult{}, fmt.Errorf("预写管理操作失败: %w", err)
	}

	// 第 2–3 步：冻结定义版本与凭据版本，再做只读预检。两处失败都在任何进程启动之前。
	// Steps 2–3: freeze the credential versions and refuse before any process starts.
	frozen, class, err := s.mcpApplyPreflight(ctx, name, pointer.ResourceUID, cfg)
	if err != nil {
		return s.refuseMCPApply(ctx, operationID, name, targetID, previous, window, class, err)
	}
	resolved := cfg
	if s.mcpCredentials != nil {
		values, resolveErr := s.mcpCredentials.Resolve(ctx, name, pointer.ResourceUID, cfg)
		if resolveErr != nil {
			class = mcpGateFailureClassOf(resolveErr)
			return s.refuseMCPApply(ctx, operationID, name, targetID, previous, window, class, resolveErr)
		}
		resolved = ApplyResolvedMCP(cfg, values)
		ZeroMCPCredentialValues(values)
	}

	// 第 4–8 步交给连接层。`Commit` 只做并发校验与 CAS：注册池的整组替换由 supervisor
	// 在同一段临界区里紧跟它完成。
	// Steps 4–8 belong to the supervisor; Commit does the concurrent check and the CAS only.
	in := MCPApplyInput{
		Target:            core.PublishedMCPServer{VersionID: targetID, Config: resolved},
		PreviousVersionID: previous,
		DrainTimeout:      window,
		Commit: func(commitCtx context.Context) (core.MCPApplySupersededBy, error) {
			return s.commitMCPApply(commitCtx, name, pointer, frozen, targetID, operationID)
		},
		OldLegal: func(legalCtx context.Context) bool {
			stillLegal, legalErr := s.mcpCredentials.StillCurrent(legalCtx, pointer.ResourceUID, frozen)
			return legalErr == nil && stillLegal
		},
	}
	replaced := s.supervisor.Replace(ctx, in)
	result := core.MCPServerApplyResult{
		Outcome:             replaced.Outcome,
		OperationID:         operationID,
		TargetVersionID:     targetID,
		PreviousVersionID:   previous,
		AppliedVersionID:    replaced.AppliedVersionID,
		DrainDeadline:       mcpApplyDeadline(replaced.DrainDeadline),
		DrainTimeoutSeconds: int(window.Seconds()),
		FailureClass:        replaced.FailureClass,
		SupersededBy:        replaced.SupersededBy,
	}
	payload := mcpApplyDetail(result)

	switch result.Outcome {
	case core.MCPApplyApplied:
		s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", payload)
		return result, nil
	case core.MCPApplySuperseded:
		s.finish(ctx, operationID, core.OperationFailed, mcpApplyResultSuperseded,
			"MCP 受控发布被并发写取代", payload)
		return result, &core.AdminError{
			Kind:    core.ErrMCPApplySuperseded,
			Message: fmt.Sprintf("MCP server %s 的受控发布被并发写取代，本次未切换", name),
			Data:    map[string]any{"operation_id": operationID},
		}
	default:
		// 策略拒绝先于建连，且本次没有切换：旧连接仍在合法运行，错误面单独一档（403）。
		if result.FailureClass == mcpResultPolicyDenied {
			s.finish(ctx, operationID, core.OperationFailed, mcpResultPolicyDenied,
				"MCP 受控发布被策略拒绝", payload)
			return result, &core.AdminError{
				Kind:    core.ErrPolicyDenied,
				Message: fmt.Sprintf("MCP server %s 的受控发布被策略拒绝（未建立任何连接）", name),
				Data:    map[string]any{"operation_id": operationID},
			}
		}
		s.finish(ctx, operationID, core.OperationFailed, mcpApplyResultFailed,
			"MCP 受控发布失败", payload)
		return result, &core.AdminError{
			Kind:    core.ErrMCPApplyFailed,
			Message: fmt.Sprintf("MCP server %s 的受控发布失败，运行中的版本未改变", name),
			Data:    map[string]any{"operation_id": operationID},
		}
	}
}

// mcpApplyPreflight 冻结本次依赖的凭据版本并做只读预检（依赖定位 + 声明来源可取）。
// 返回的失败分类用于 503 的 detail：依赖缺失 / 版本不符与"取不到值"都落在既有枚举内，
// 不新造字面量。
// mcpApplyPreflight freezes the credential versions and refuses read-only; the class stays
// inside the frozen failure enum.
func (s *ResourceAdmin) mcpApplyPreflight(ctx context.Context, name string,
	uid core.ResourceUID, cfg core.MCPServerConfig,
) ([]mcpCredentialFreeze, string, error) {
	if s.mcpDependencies != nil {
		if dependency := s.mcpDependencies(cfg); dependency != nil &&
			dependency.State != "" && dependency.State != core.MCPDependencyReady {
			return nil, mcpTestClassDependencyUnconfirmed, fmt.Errorf(
				"MCP server %s 的依赖 %s 未就绪（状态 %s，期望版本 %q，实际版本 %q）",
				name, dependency.Component, dependency.State,
				dependency.ExpectedVersion, dependency.FoundVersion)
		}
	}
	if s.mcpCredentials == nil {
		return nil, "", nil
	}
	frozen, err := s.mcpCredentials.Freeze(ctx, name, uid, cfg)
	if err != nil {
		return nil, mcpGateFailureClassOf(err), err
	}
	return frozen, "", nil
}

// refuseMCPApply 统一收口预检拒绝：先写终态（`failed`，detail 与成功响应同形），再回
// 503。`applied` 保持不变；错误只点名 server / 键 / 变量名，不含值（约束 7 / 22）。
// refuseMCPApply finishes the operation as failed and answers 503 with the same shape.
func (s *ResourceAdmin) refuseMCPApply(ctx context.Context, operationID, name string,
	targetID, previous int64, window time.Duration, class string, cause error,
) (core.MCPServerApplyResult, error) {
	result := core.MCPServerApplyResult{
		Outcome:             core.MCPApplyFailed,
		OperationID:         operationID,
		TargetVersionID:     targetID,
		PreviousVersionID:   previous,
		AppliedVersionID:    previous,
		DrainTimeoutSeconds: int(window.Seconds()),
		FailureClass:        class,
	}
	code := resourceResultNotConfigured
	kind := core.ErrProviderNotConfigured
	if class == mcpGateClassCredentialUnavailable {
		// 托管凭据取不到值走 CREDENTIAL_UNAVAILABLE：该去补录 / 恢复主密钥，而不是补环境变量。
		code = mcpApplyResultCredentialUnavailable
		kind = core.ErrCredentialUnavailable
	}
	s.finish(ctx, operationID, core.OperationFailed, code, "受控发布的就绪预检未通过", mcpApplyDetail(result))
	s.logger.Warn("MCP 受控发布预检拒绝", "resource", name, "target_version_id", targetID,
		"failure_class", class)
	return result, &core.AdminError{
		Kind:    kind,
		Message: cause.Error(),
		Data:    map[string]any{"operation_id": operationID},
	}
}

// commitMCPApply 是第 6 步的并发校验：冻结的托管凭据必须仍是当前版本，且 `applied`
// 的 CAS 必须仍以第 1 步读到的 `published` / `applied` 为前提。两种失败都只是"被别人
// 取代"——本函数不写库以外的任何状态，回滚由调用方在同一段临界区里完成。
//
// CAS 失败时再读一次指针来**分类**取代者：发布推进了指针 → `publish`；只有 `applied`
// 变了 → 另一次 `apply`。读不到（行已删）也记 `apply`：撤销会先递增 epoch，走不到这里。
// commitMCPApply is step 6: credential currency plus the applied CAS, classified on miss.
func (s *ResourceAdmin) commitMCPApply(ctx context.Context, name string, pointer core.ResourcePointer,
	frozen []mcpCredentialFreeze, targetID int64, operationID string,
) (core.MCPApplySupersededBy, error) {
	if len(frozen) > 0 {
		still, err := s.mcpCredentials.StillCurrent(ctx, pointer.ResourceUID, frozen)
		if err != nil {
			return core.MCPApplySupersededByNone, err
		}
		if !still {
			return core.MCPApplySupersededByRevoke, nil
		}
	}
	applied := derefVersionID(pointer.AppliedVersionID)
	swapped, err := s.store.CASApplied(ctx, core.ResourceMCPServer, name,
		pointer.PublishedVersionID, applied, targetID, operationID, s.now())
	if err != nil {
		return core.MCPApplySupersededByNone, err
	}
	if swapped {
		return core.MCPApplySupersededByNone, nil
	}
	latest, err := s.store.Pointer(ctx, core.ResourceMCPServer, name)
	if err != nil {
		if errors.Is(err, core.ErrResourceNotFound) {
			return core.MCPApplySupersededByApply, nil
		}
		return core.MCPApplySupersededByNone, err
	}
	if latest.PublishedVersionID != pointer.PublishedVersionID {
		return core.MCPApplySupersededByPublish, nil
	}
	return core.MCPApplySupersededByApply, nil
}

// mcpApplyDetail 序列化操作行 detail；序列化失败退回 `{}`（审计行照写，不因 detail 丢行）。
// mcpApplyDetail serializes the fixed seven-key audit detail.
func mcpApplyDetail(result core.MCPServerApplyResult) []byte {
	payload, err := json.Marshal(result.AuditDetail())
	if err != nil {
		return []byte("{}")
	}
	return payload
}

// mcpApplyDeadline 把排空截止时刻按契约格式化成 RFC 3339 UTC；零值表示"没有窗口"
// （`superseded` / `failed`），输出空串而不是零时刻。
// mcpApplyDeadline renders the deadline as RFC 3339 UTC; a zero time renders empty.
func mcpApplyDeadline(deadline time.Time) string {
	if deadline.IsZero() {
		return ""
	}
	return deadline.UTC().Format(time.RFC3339)
}

// mcpApplyRequestHash 是操作行的请求指纹：动作 + 资源名 + 目标版本。同一次 apply 重放
// 得到同一个哈希，不同目标版本可区分。
// mcpApplyRequestHash fingerprints the action, the server and the target version.
func mcpApplyRequestHash(name string, targetVersionID int64) string {
	return agentRequestHash(fmt.Sprintf("%s\n%s\n%d", core.ActionMCPServerApply, name, targetVersionID))
}
