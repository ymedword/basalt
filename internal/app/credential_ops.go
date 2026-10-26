package app

// 本文件是托管凭据管理写面的编排（模块卡 R4.8 / R4.9 / T132）：
//   - `Write`：managed 写入（新建身份 / 追加版本 / 撤销后重新录入）与 managed → env 的退役；
//   - `Revoke`：撤销（身份与全部版本 → `revoked`、密文清空、`revocation_epoch + 1`）；
//   - 四证恢复：预写 pending 时先把**候选终态**落进 `detail_json`（资源 `revision` 前后值、
//     凭据 ID 与版本号、目标绑定哈希、撤销纪元前后值），启动恢复据此判
//     applied / not_applied / indeterminate，**不**重新加密、**不**重放撤销。
//
// 六条纪律：
//   - **先预写再动目标**：加密在主密钥装载之后、操作行之前完成——装载失败不留操作行；
//     一旦写了 pending，后续失败必须回填终态并带上操作标识（契约 §9.3 / FR-001）；
//   - **凭据只追加**：换 Key 是新版本，重新录入是**新编号**的版本；被撤销的版本永不复活，
//     `revocation_epoch` 只增不减（2026-10-23 判据裁决）；
//   - **撤销不需要主密钥**：只清密文与升纪元，不做任何解密；资源已删除（墓碑）时仍可撤销；
//   - **目标绑定跟着地址走**：模型连接取当前发布定义的 `base_url`，通知渠道取本次写入的
//     完整地址；地址变了就必须录新 Key；
//   - **明文不落盘、不进日志、不进请求摘要**：`Value` 用完清零，`detail_json` 只记元数据，
//     错误文案只点名资源与凭据 ID；
//   - **资源行与凭据分开写**：本服务在凭据动作成功后推进一次目标资源 `revision`（模型连接
//     走发布指针，通知渠道走渠道行），调用方不得再写同一组凭据列；目标已删除时不推进。
//
// This file orchestrates managed-credential writes and revokes. The pending row carries
// the predicted post-change evidence before anything moves; plaintext never reaches the
// database, the logs or the request digest, and recovery verifies rather than replays.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// credentialIDPrefix 是凭据标识的固定前缀（`core.ValidateCredentialID` 同口径）。
// credentialIDPrefix mirrors the literal checked by the domain validator.
const credentialIDPrefix = "cred_"

// CredentialOpsDeps 是凭据写面的装配输入；四个仓储缺一即拒绝装配。
// CredentialOpsDeps wires the write surface; every store is required.
type CredentialOpsDeps struct {
	Operations  core.OperationStore
	Credentials *storage.CredentialStore
	Resources   *storage.ResourceStore
	Notify      *storage.NotifyAdminStore
	// Instance 是实例加密三态与首次初始化的唯一实现（R2-1）：首次保存托管凭据时由
	// `PrepareManagedWrite` 在同一次保存里完成必要初始化。nil 表示半截装配或测试，
	// 此时退回「只校验主密钥可装载」的旧行为，不伪造一个能初始化的实例。
	// Instance lets a first managed save initialize the instance; nil falls back to the
	// plain loadability check.
	Instance *InstanceSecretsService
	// MasterKeyFile 是静态初值；KeyPath 是共享的生效路径（用户显式初始化后立即切换）。
	// MasterKeyFile is the static seed; KeyPath is the live shared path when present.
	MasterKeyFile string
	KeyPath       *MasterKeyLocator
	WorkspaceRoot string
	Logger        *slog.Logger
	Now           func() time.Time
}

// CredentialOps 是托管凭据写面的编排实现；字段构造后不再变化。
// CredentialOps is the only implementation of the credential write surface.
type CredentialOps struct {
	deps CredentialOpsDeps
}

// NewCredentialOps 组装凭据写面；缺必需仓储即拒绝构造。
// NewCredentialOps refuses to construct without its required stores.
func NewCredentialOps(deps CredentialOpsDeps) (*CredentialOps, error) {
	if deps.Operations == nil || deps.Credentials == nil || deps.Resources == nil || deps.Notify == nil {
		return nil, errors.New("凭据写面装配缺少仓储依赖")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &CredentialOps{deps: deps}, nil
}

// CredentialSaveWriter 是**资源保存路径**消费的凭据写入面（`POST` / `PUT` 模型连接与
// 通知渠道）：只需要预检与写入两个动作。保存不新开端点（R4.9）——凭据与定义属于同一次
// 保存动作，动作字面量仍记 `*.credential.write`，由 `Write` 自己预写操作行。
// CredentialSaveWriter is the subset the save endpoints need; saving never opens a new
// endpoint, so the credential half still records its own `*.credential.write` action.
type CredentialSaveWriter interface {
	// PrepareManagedWrite 让托管写入前置条件就绪：必要时在**同一次保存**里完成实例
	// 初始化（可证明从未启用托管的存量实例），随后确认主密钥此刻可装载。失败即 503，
	// 调用方**不得**继续写定义。
	PrepareManagedWrite(ctx context.Context, actor core.Actor) error
	Write(ctx context.Context, req CredentialWriteRequest) (CredentialWriteResult, error)
}

// PrepareManagedWrite 是保存路径在写盘之前的唯一前置动作（R2-1）：
//
//  1. 实例加密尚未启用时，把「检查 → 建立实例身份与密钥路径 → 生成密钥与记录」并入
//     这次受控保存——管理员只填 API Key 点保存，不需要先点「初始化」也不需要重启；
//  2. 随后校验主密钥当前可装载（路径 / 权限 / 格式 / key_id）。主密钥不可用是唯一能
//     在写盘前发现的凭据失败，提前拦下可让「定义已发布、凭据没写成」的窗口只剩数据库
//     错误。
//
// 已经就绪的实例只做第 2 步，不写任何东西；恢复态与显式纯 env 部署在第 1 步就拒绝。
// PrepareManagedWrite folds the first managed save into instance initialization and then
// checks the key is loadable; an already-ready instance is a pure read.
func (s *CredentialOps) PrepareManagedWrite(ctx context.Context, actor core.Actor) error {
	if s.deps.Instance != nil {
		if err := s.deps.Instance.EnsureForManagedWrite(ctx, actor); err != nil {
			return err
		}
	}
	key, err := LoadMasterKey(effectiveMasterKeyFile(s.deps.KeyPath, s.deps.MasterKeyFile), s.deps.WorkspaceRoot)
	if err != nil {
		// LoadMasterKey 已包装 ErrCredentialUnavailable（503）；原样上抛即可。
		return err
	}
	key.Zero()
	return nil
}

// credentialSavePlan 是一次保存里「凭据段」的解析结果（R4.8）：`Mode` 是保存后的模式，
// `Value` 只在 managed 且本次确实带值时非空（nil = 保留），`Retire` 表示 env 保存要把
// 已存在的托管身份置 `retired`（managed → env 的切换）。
// credentialSavePlan is the resolved credential half of one save.
type credentialSavePlan struct {
	Mode   core.CredentialMode
	Value  []byte
	Retire bool
}

// needsWrite 报告本次保存是否需要调用凭据写面：带新值（managed）或要退役身份。
// needsWrite reports whether the credential write surface must be called.
func (p credentialSavePlan) needsWrite() bool { return p.Retire || p.Value != nil }

// planCredentialSave 按契约 R4.8 解析一次保存声明的模式与值：
//
//	创建（`creating`）：省略模式时按值形态判定——精确 `${VAR}` → env；managed 必须显式；
//	更新：省略模式保持当前模式；给值则形态必须与最终模式一致；
//	切换（最终模式 ≠ 当前模式）：必须显式并给出目标模式所需的值；
//	字段省略：保留（managed 不写新版本，env 不碰身份）；带空串 / 纯空白 → 400，既不
//	当保留也不当清除。
//
// 返回值只描述「凭据段该做什么」；定义文件里 `credential_mode` / `api_key` 的渲染由
// 调用方按同一 `Mode` 决定，两处不会各说各话。
// planCredentialSave resolves the credential mode and value of one save. A blank value
// is neither "keep" nor "clear": it is the contract's 400.
func planCredentialSave(action, resource, declared, value string, valueSet bool,
	current core.CredentialMode, creating bool,
) (credentialSavePlan, error) {
	mode := current
	if creating {
		// 创建没有「当前模式」：未声明时只能按值形态落到 env（空值 = 暂不可用）。
		mode = core.CredentialModeEnv
	}
	if declared != "" {
		parsed, err := core.ParseCredentialMode(declared)
		if err != nil {
			return credentialSavePlan{}, err
		}
		mode = parsed
	}

	if valueSet {
		if strings.TrimSpace(value) == "" {
			return credentialSavePlan{}, &core.InvalidInputError{
				Tool:   action,
				Reason: resource + ": 凭据值不得为空；保留请省略字段",
			}
		}
		_, placeholder := core.EnvPlaceholderName(value)
		switch mode {
		case core.CredentialModeEnv:
			if !placeholder {
				return credentialSavePlan{}, credentialModeError(resource,
					"env 模式的凭据字段必须是精确 ${VAR} 占位")
			}
		case core.CredentialModeManaged:
			if placeholder {
				return credentialSavePlan{}, credentialModeError(resource,
					"托管模式不接受 ${VAR} 占位：请给出明文凭据")
			}
		}
		plan := credentialSavePlan{Mode: mode}
		switch mode {
		case core.CredentialModeManaged:
			plan.Value = []byte(value)
		case core.CredentialModeEnv:
			// managed → env：地址换成 `${VAR}` 占位的同时退役旧身份。版本仍可解密，
			// 在途 Run 固定的旧版本不受影响；身份不回滚、revocation_epoch 不重置。
			if current == core.CredentialModeManaged {
				plan.Retire = true
			}
		}
		return plan, nil
	}

	// 字段省略 = 保留。模式发生变化即一次切换，切换必须带值（R4.8）。
	if mode != current {
		return credentialSavePlan{}, credentialModeError(resource,
			"切换凭据模式必须显式给出目标模式所需的值")
	}
	return credentialSavePlan{Mode: mode}, nil
}

// credentialModeError 是保存期模式 / 值不匹配的固定载体：400 `CREDENTIAL_MODE_REQUIRED`，
// 文案点名资源与要求，不含任何值。
// credentialModeError is the fixed 400 CREDENTIAL_MODE_REQUIRED carrier.
func credentialModeError(resource, message string) error {
	return &core.AdminError{
		Kind:    core.ErrCredentialModeRequired,
		Message: resource + ": " + message,
	}
}

// writeSavedCredential 在保存路径上执行凭据段：只在需要时调用写面（managed 带值 →
// 新版本；managed → env → 退役身份），不需要时返回 `Changed=false`。
// 失败原样上抛——`Write` 已按契约分类（503 / 400 / 500 + 操作标识）。
// writeSavedCredential runs the credential half of a save and only writes when needed.
func writeSavedCredential(ctx context.Context, writer CredentialSaveWriter, actor core.Actor,
	kind core.BindingKind, name string, uid core.ResourceUID, plan credentialSavePlan,
) (CredentialWriteResult, error) {
	if writer == nil || !plan.needsWrite() {
		return CredentialWriteResult{}, nil
	}
	return writer.Write(ctx, CredentialWriteRequest{
		Actor: actor,
		Kind:  kind,
		Name:  name,
		UID:   uid,
		Mode:  plan.Mode,
		Value: plan.Value,
	})
}

// CredentialWriteRequest 是一次凭据写入请求：`Value` 是请求里的凭据字段原文，
// 按模式解释——env 必须是精确 `${VAR}` 占位（空 = 资源暂不可用），managed 是待加密
// 的明文 Key（模型连接）或完整投递地址（通知渠道）。**本服务在返回前清零 `Value` 的
// 底层字节**，调用方不必也不得再读。
// CredentialWriteRequest interprets Value by mode and zeroes it before returning.
type CredentialWriteRequest struct {
	Actor core.Actor
	Kind  core.BindingKind
	Name  string
	UID   core.ResourceUID
	Mode  core.CredentialMode
	Value []byte
}

// CredentialWriteResult 是一次写入的权威结论：`Changed=false` 表示本动作没有改动任何行
// （例如 env 模式下本就没有托管身份），调用方按自己的普通写路径落地址列。
// A false Changed means this action touched nothing and the caller keeps its own write path.
type CredentialWriteResult struct {
	OperationID  string
	CredentialID string
	VersionNo    int
	Revision     int
	Last4        string
	State        core.CredentialIdentityState
	Changed      bool
}

// CredentialRevokeRequest 是一次撤销请求；按资源 UID 寻址，不按名称猜测目标。
// CredentialRevokeRequest addresses the resource by uid, never by name.
type CredentialRevokeRequest struct {
	Actor core.Actor
	Kind  core.BindingKind
	Name  string
	UID   core.ResourceUID
	// BindingKey 是 `kind=mcp_server` 时的槽位名（env 键名或请求头名）：一个 server 可以
	// 有多个槽位，各走独立版本与撤销。其它类别必须留空。
	// BindingKey addresses the MCP slot; other kinds must leave it empty.
	BindingKey string
	// IfMatch 是调用方带来的条件写期望值：比较在调用方（`ResourceStateService`）的
	// 资源锁内完成，本字段只落进操作行 `expected_etag` 供审计与查证，**不**在这里
	// 二次判定——保存路径的子动作（`Write`）留空。
	// IfMatch is only recorded on the operation row; the caller compares it under the
	// resource lock, and the save-path sub-action leaves it empty.
	IfMatch string
}

// CredentialRevokeResult 是一次撤销的结论；已撤销的凭据重放同请求 → `Changed=false`，
// 不新开操作行、不再升纪元。
// A repeated revoke is an idempotent no-change result with no new operation row.
type CredentialRevokeResult struct {
	OperationID     string
	CredentialID    string
	RevocationEpoch int
	Revision        int
	Changed         bool
}

// credentialTarget 是目标资源在**本次动作之前**的提交事实：`revision` 参与四证恢复，
// `deleted` 决定是否推进 revision（墓碑必须保持墓碑）。
// credentialTarget is the pre-change commit state of the target row.
type credentialTarget struct {
	revision           int
	deleted            bool
	publishedVersionID int64
}

// credentialWritePlan 是一次写动作在预写之前算好的全部事实：目标行的前后 revision、
// 身份与版本号、目标绑定哈希，以及要不要退役 / 撤销。
// credentialWritePlan holds everything the pending row must predict before the change.
type credentialWritePlan struct {
	kind core.BindingKind
	name string
	uid  core.ResourceUID
	mode core.CredentialMode
	now  time.Time
	// bindingKey 只在 MCP 上非空：一个 server 可以有多个槽位，各走独立的版本与撤销。
	// bindingKey is the slot of an MCP credential; other kinds leave it empty.
	bindingKey string
	// expectedETag 只用于**撤销端点**的条件写审计（保存路径的子动作留空）。
	// expectedETag is the conditional-write value recorded on a revoke operation row.
	expectedETag string

	previousRevision   int
	revision           int
	deleted            bool
	publishedVersionID int64

	identity        core.CredentialRecord
	identityFound   bool
	credentialID    string
	previousVersion int
	versionNo       int
	target          string
	retire          bool
	revoke          bool
	changed         bool

	keyID   string
	version core.CredentialVersionRecord
}

// Write 执行一次凭据写入：managed 新建 / 追加 / 重新录入，或 managed → env 的退役。
// 请求校验全部在预写之前；加密也在预写之前（主密钥不可用 → 不落操作行）。
// Write runs one credential write; validation and sealing both happen before the
// pending row, so a missing master key never leaves an operation row.
func (s *CredentialOps) Write(ctx context.Context, req CredentialWriteRequest) (CredentialWriteResult, error) {
	defer zeroCredentialValue(req.Value)

	action := credentialWriteAction(req.Kind)
	kind, name, uid, err := s.validateTarget(req.Kind, req.Name, req.UID, action)
	if err != nil {
		return CredentialWriteResult{}, err
	}
	mode, err := core.ParseCredentialMode(string(req.Mode))
	if err != nil {
		return CredentialWriteResult{}, fmt.Errorf("%s: %w", action, err)
	}
	value, err := normaliseCredentialValue(mode, action, req.Value)
	if err != nil {
		return CredentialWriteResult{}, err
	}
	plan, err := s.planWrite(ctx, kind, name, uid, mode, value)
	if err != nil {
		return CredentialWriteResult{}, err
	}
	if !plan.changed {
		return CredentialWriteResult{
			CredentialID: plan.credentialID,
			VersionNo:    plan.versionNo,
			Revision:     plan.previousRevision,
			State:        plan.identity.State,
		}, nil
	}
	return s.applyWrite(ctx, req.Actor, plan)
}

// Revoke 撤销一个资源的托管凭据：不需要主密钥、不解密；资源已删除（墓碑）时仍可撤销，
// 只是不再推进已删除资源的 `revision`。
// Revoke blanks the credential without the master key and still works on a tombstoned
// resource, where it simply does not advance the resource revision.
func (s *CredentialOps) Revoke(ctx context.Context, req CredentialRevokeRequest) (CredentialRevokeResult, error) {
	action := credentialRevokeAction(req.Kind)
	kind, name, uid, err := s.validateTarget(req.Kind, req.Name, req.UID, action)
	if err != nil {
		return CredentialRevokeResult{}, err
	}
	// MCP 的凭据按槽位寻址：`binding_key` 必填且必须合法，其它类别不得携带它——
	// 多一个寻址维度就多一种"撤错对象"的可能。
	bindingKey, err := credentialBindingKey(kind, req.BindingKey, action)
	if err != nil {
		return CredentialRevokeResult{}, err
	}
	target, err := s.readTarget(ctx, kind, name, uid)
	if err != nil {
		return CredentialRevokeResult{}, err
	}
	identity, err := s.credentialIdentity(ctx, kind, uid, bindingKey)
	if err != nil {
		if errors.Is(err, core.ErrResourceNotFound) {
			return CredentialRevokeResult{}, &core.InvalidInputError{
				Tool:   action,
				Reason: fmt.Sprintf("资源 %s 没有托管凭据，无需撤销", name),
			}
		}
		return CredentialRevokeResult{}, fmt.Errorf("%s: %w", action, err)
	}
	if identity.State == core.CredentialIdentityRevoked {
		return CredentialRevokeResult{
			CredentialID:    identity.ID,
			RevocationEpoch: identity.RevocationEpoch,
			Revision:        target.revision,
		}, nil
	}
	if !identity.State.CanTransitionTo(core.CredentialIdentityRevoked) {
		return CredentialRevokeResult{}, fmt.Errorf("%w: 凭据 %s 状态为 %s，不能撤销",
			core.ErrCredentialUnavailable, identity.ID, identity.State)
	}
	current, err := s.deps.Credentials.Version(ctx, identity.ID, identity.CurrentVersion)
	if err != nil {
		return CredentialRevokeResult{}, fmt.Errorf("%w: 凭据 %s 的当前版本不可读",
			core.ErrCredentialUnavailable, identity.ID)
	}
	revision := target.revision
	if !target.deleted {
		revision = target.revision + 1
	}
	plan := credentialWritePlan{
		kind: kind, name: name, uid: uid, mode: core.CredentialModeManaged, now: s.deps.Now(),
		bindingKey:         bindingKey,
		expectedETag:       req.IfMatch,
		previousRevision:   target.revision,
		revision:           revision,
		deleted:            target.deleted,
		publishedVersionID: target.publishedVersionID,
		identity:           identity,
		identityFound:      true,
		credentialID:       identity.ID,
		previousVersion:    identity.CurrentVersion,
		versionNo:          identity.CurrentVersion,
		target:             current.TargetBinding,
		revoke:             true,
		changed:            true,
	}
	return s.applyRevoke(ctx, req.Actor, plan)
}

// validateTarget 校验类别 / 名称 / 资源 UID 三件寻址输入；只接受带凭据的三类资源。
// MCP server 另有 `binding_key`（槽位名）这一维寻址，由调用方单独校验。
// validateTarget accepts the three credential owner kinds; MCP adds a binding key
// that the caller validates separately.
func (s *CredentialOps) validateTarget(kind core.BindingKind, name string,
	uid core.ResourceUID, action string,
) (core.BindingKind, string, core.ResourceUID, error) {
	parsed, err := core.ParseCredentialOwnerKind(string(kind))
	if err != nil {
		return "", "", "", &core.InvalidInputError{Tool: action, Reason: err.Error()}
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", "", "", &core.InvalidInputError{Tool: action, Reason: "资源名不得为空"}
	}
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return "", "", "", &core.InvalidInputError{Tool: action, Reason: err.Error()}
	}
	return parsed, trimmed, canonical, nil
}

// credentialBindingKey 归一化撤销请求的槽位维度：MCP 必填且必须是合法槽位名，其它
// 类别必须留空（携带即 400——多一个寻址维度就多一种撤错对象的可能）。
// credentialBindingKey requires a slot for MCP and forbids one elsewhere.
func credentialBindingKey(kind core.BindingKind, raw, action string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if kind != core.BindingMCPServer {
		if trimmed != "" {
			return "", &core.InvalidInputError{
				Tool:   action,
				Reason: fmt.Sprintf("资源类别 %q 的凭据不按槽位寻址，不得携带 binding_key", kind),
			}
		}
		return "", nil
	}
	key, ok := core.MCPBindingKey(trimmed)
	if !ok {
		return "", &core.InvalidInputError{
			Tool:   action,
			Reason: "撤销 MCP 凭据必须给出合法的槽位名（env 键名或请求头名）",
		}
	}
	return key, nil
}

// credentialIdentity 按类别读托管身份：MCP 按 `(server, 槽位)`，其余按属主。
// credentialIdentity reads the managed identity of one owner or one MCP slot.
func (s *CredentialOps) credentialIdentity(ctx context.Context, kind core.BindingKind,
	uid core.ResourceUID, bindingKey string,
) (core.CredentialRecord, error) {
	if kind == core.BindingMCPServer {
		return s.deps.Credentials.CredentialByOwnerKey(ctx, kind, uid, bindingKey)
	}
	return s.deps.Credentials.CredentialByOwner(ctx, kind, uid)
}

// normaliseCredentialValue 按模式解释请求里的凭据字段：managed 拒绝 `${VAR}` 占位与
// 空值，env 只接受精确占位或空串；跨形态值一律是 CREDENTIAL_MODE_REQUIRED（R4.8）。
// normaliseCredentialValue enforces the mode/value cross-shape rule; a mismatch is the
// contract's CREDENTIAL_MODE_REQUIRED class.
func normaliseCredentialValue(mode core.CredentialMode, action string, value []byte) ([]byte, error) {
	raw := string(value)
	trimmed := strings.TrimSpace(raw)
	switch mode {
	case core.CredentialModeManaged:
		if trimmed == "" {
			return nil, &core.InvalidInputError{
				Tool:   action,
				Reason: "凭据值不得为空；保留原 Key 请省略该字段",
			}
		}
		if _, ok := core.EnvPlaceholderName(trimmed); ok {
			return nil, fmt.Errorf("%w: 托管模式不接受 ${VAR} 占位（凭据值是明文 Key 或完整地址）",
				core.ErrCredentialModeRequired)
		}
		if raw != trimmed {
			return nil, &core.InvalidInputError{Tool: action, Reason: "凭据值首尾不得含空白"}
		}
		return []byte(trimmed), nil
	case core.CredentialModeEnv:
		if trimmed == "" {
			return nil, nil
		}
		if _, ok := core.EnvPlaceholderName(trimmed); !ok {
			return nil, fmt.Errorf("%w: env 模式的凭据字段必须是精确 ${VAR} 占位",
				core.ErrCredentialModeRequired)
		}
		return []byte(trimmed), nil
	default:
		return nil, fmt.Errorf("%w: 非法的凭据模式 %q", core.ErrCredentialModeRequired, mode)
	}
}

// planWrite 读目标资源与凭据身份，算出候选终态；managed 在返回前完成加密与目标绑定。
// planWrite computes the predicted terminal state; managed sealing happens here, before
// the pending row.
func (s *CredentialOps) planWrite(ctx context.Context, kind core.BindingKind, name string,
	uid core.ResourceUID, mode core.CredentialMode, value []byte,
) (credentialWritePlan, error) {
	target, err := s.readTarget(ctx, kind, name, uid)
	if err != nil {
		return credentialWritePlan{}, err
	}
	if target.deleted {
		return credentialWritePlan{}, fmt.Errorf("%w: 资源 %s 已删除，不能再写凭据",
			core.ErrResourceNotFound, name)
	}
	plan := credentialWritePlan{
		kind: kind, name: name, uid: uid, mode: mode, now: s.deps.Now(),
		previousRevision:   target.revision,
		revision:           target.revision + 1,
		publishedVersionID: target.publishedVersionID,
	}
	if mode == core.CredentialModeManaged {
		binding, bindErr := s.bindManagedTarget(ctx, kind, name, target, value)
		if bindErr != nil {
			return credentialWritePlan{}, bindErr
		}
		plan.target = binding
	}

	identity, identityErr := s.deps.Credentials.CredentialByOwner(ctx, kind, uid)
	switch {
	case identityErr == nil:
		plan.identity = identity
		plan.identityFound = true
		plan.previousVersion = identity.CurrentVersion
	case errors.Is(identityErr, core.ErrResourceNotFound):
		plan.identityFound = false
	default:
		return credentialWritePlan{}, fmt.Errorf("读取资源 %s 的凭据身份: %w", uid, identityErr)
	}

	switch mode {
	case core.CredentialModeManaged:
		plan.changed = true
		if plan.identityFound {
			plan.credentialID = identity.ID
			plan.versionNo = identity.CurrentVersion + 1
		} else {
			credentialID, mintErr := newCredentialID()
			if mintErr != nil {
				return credentialWritePlan{}, mintErr
			}
			plan.credentialID = credentialID
			plan.versionNo = 1
			plan.previousVersion = 0
		}
		if err := s.sealWrite(&plan, value); err != nil {
			return credentialWritePlan{}, err
		}
	case core.CredentialModeEnv:
		// 没有托管身份、或身份已撤销 / 已退役：本动作不碰任何行，调用方按普通写落地址列。
		if !plan.identityFound || identity.State == core.CredentialIdentityRevoked ||
			identity.State == core.CredentialIdentityRetired {
			return plan, nil
		}
		current, versionErr := s.deps.Credentials.Version(ctx, identity.ID, identity.CurrentVersion)
		if versionErr != nil {
			return credentialWritePlan{}, fmt.Errorf("%w: 凭据 %s 的当前版本不可读",
				core.ErrCredentialUnavailable, identity.ID)
		}
		plan.changed = true
		plan.retire = true
		plan.credentialID = identity.ID
		plan.versionNo = identity.CurrentVersion
		plan.target = current.TargetBinding
	}
	return plan, nil
}

// bindManagedTarget 计算 managed 写入的目标绑定：模型连接以当前发布定义的 `base_url`
// 绑定并核对定义声明的模式；通知渠道以本次写入的完整地址绑定。展示主机由调用方在自己
// 那次地址写入里落库（`managedAddressHost`），不从这里回流——凭据动作只碰凭据列。
// bindManagedTarget returns the binding a managed write seals into the AAD; the display
// host belongs to the caller's own address write.
func (s *CredentialOps) bindManagedTarget(ctx context.Context, kind core.BindingKind, name string,
	target credentialTarget, value []byte,
) (string, error) {
	switch kind {
	case core.BindingModelConnection:
		version, err := s.deps.Resources.VersionByID(ctx, target.publishedVersionID)
		if err != nil {
			return "", fmt.Errorf("%w: 模型连接 %s 的当前发布版本不可读",
				core.ErrCredentialUnavailable, name)
		}
		definition, err := parseProviderDefinition(name, version.DefinitionYAML)
		if err != nil {
			return "", err
		}
		declared, err := credentialModeOf(name, definition)
		if err != nil {
			return "", err
		}
		if declared != core.CredentialModeManaged {
			return "", fmt.Errorf("%w: 模型连接 %s 的定义声明为 env，不能写入托管凭据",
				core.ErrCredentialModeRequired, name)
		}
		return CredentialTargetBinding(core.BindingModelConnection, definition.BaseURL, "")
	case core.BindingNotifyChannel:
		address := string(value)
		if err := validateManagedAddress(address); err != nil {
			return "", err
		}
		return CredentialTargetBinding(core.BindingNotifyChannel, "", address)
	default:
		return "", fmt.Errorf("%w: 资源类别 %q 不支持托管凭据",
			core.ErrCredentialUnavailable, kind)
	}
}

// readTarget 读目标资源的提交事实：模型连接读发布指针（含墓碑），通知渠道读渠道行
// （含墓碑）。同名重建后模型连接的旧指针行已被覆盖，此时按 UID 找不到 → 404；
// 通知渠道的每个 UID 各占一行，墓碑长期可寻址。
// readTarget reads the commit facts of the target row, tombstones included.
func (s *CredentialOps) readTarget(ctx context.Context, kind core.BindingKind, name string,
	uid core.ResourceUID,
) (credentialTarget, error) {
	switch kind {
	case core.BindingModelConnection:
		pointer, err := s.deps.Resources.PointerByUID(ctx, core.ResourceModelConnection, uid)
		if err != nil {
			return credentialTarget{}, fmt.Errorf("%w: 模型连接 %s 没有当前发布指针",
				core.ErrResourceNotFound, name)
		}
		if pointer.ResourceID != name {
			return credentialTarget{}, fmt.Errorf("%w: 模型连接 %s 当前指向 %s",
				core.ErrResourceUIDMismatch, name, pointer.ResourceUID)
		}
		return credentialTarget{
			revision:           pointer.Revision,
			deleted:            pointer.DeletedAt != nil,
			publishedVersionID: pointer.PublishedVersionID,
		}, nil
	case core.BindingNotifyChannel:
		channel, err := s.deps.Notify.GetByUID(ctx, uid)
		if err != nil {
			return credentialTarget{}, fmt.Errorf("%w: 通知渠道 %s", core.ErrResourceNotFound, name)
		}
		return credentialTarget{
			revision: channel.Revision,
			deleted:  channel.DeletedAt != nil,
		}, nil
	case core.BindingMCPServer:
		pointer, err := s.deps.Resources.PointerByUID(ctx, core.ResourceMCPServer, uid)
		if err != nil {
			return credentialTarget{}, fmt.Errorf("%w: MCP server %s 没有当前发布指针",
				core.ErrResourceNotFound, name)
		}
		if pointer.ResourceID != name {
			return credentialTarget{}, fmt.Errorf("%w: MCP server %s 当前指向 %s",
				core.ErrResourceUIDMismatch, name, pointer.ResourceUID)
		}
		return credentialTarget{
			revision:           pointer.Revision,
			deleted:            pointer.DeletedAt != nil,
			publishedVersionID: pointer.PublishedVersionID,
		}, nil
	default:
		return credentialTarget{}, fmt.Errorf("%w: 资源类别 %q 不支持托管凭据",
			core.ErrCredentialUnavailable, kind)
	}
}

// applyWrite 预写 pending → 写凭据 → 推进资源 revision → 回填终态。
// applyWrite pre-writes, applies the credential, bumps the revision and finishes the row.
func (s *CredentialOps) applyWrite(ctx context.Context, actor core.Actor,
	plan credentialWritePlan,
) (CredentialWriteResult, error) {
	detail := plan.writeDetail()
	operationID, err := s.beginOperation(ctx, actor, credentialWriteAction(plan.kind), plan,
		credentialWriteRequestHash(plan), detail)
	if err != nil {
		return CredentialWriteResult{}, err
	}
	if applyErr := s.applyCredential(ctx, plan, operationID); applyErr != nil {
		s.logError("托管凭据写入失败", plan, operationID, applyErr)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "托管凭据写入失败")
		return CredentialWriteResult{}, operationFailedError(operationID, "写入托管凭据失败")
	}
	if bumpErr := s.bumpRevision(ctx, plan, operationID); bumpErr != nil {
		// 凭据已生效、资源 revision 未动：恢复路径按四证判 indeterminate → unknown，
		// **不**自动补做，由运维核对操作记录后决定。
		s.logError("凭据写入后推进资源 revision 失败", plan, operationID, bumpErr)
		s.finish(ctx, operationID, core.OperationUnknown, lifecycleResultInternal,
			"凭据已写入但资源 revision 未推进")
		return CredentialWriteResult{}, operationFailedError(operationID,
			"托管凭据已写入，但资源 revision 未能推进；请核对操作记录后再继续")
	}
	s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "")
	return s.writeResult(ctx, plan, operationID), nil
}

// applyRevoke 预写 pending → 撤销 → 推进资源 revision → 回填终态。
// applyRevoke pre-writes, revokes, bumps the revision and finishes the row.
func (s *CredentialOps) applyRevoke(ctx context.Context, actor core.Actor, plan credentialWritePlan,
) (CredentialRevokeResult, error) {
	detail := plan.revokeDetail()
	operationID, err := s.beginOperation(ctx, actor, credentialRevokeAction(plan.kind), plan,
		credentialRevokeRequestHash(plan), detail)
	if err != nil {
		return CredentialRevokeResult{}, err
	}
	if applyErr := s.applyCredential(ctx, plan, operationID); applyErr != nil {
		s.logError("撤销托管凭据失败", plan, operationID, applyErr)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "撤销托管凭据失败")
		return CredentialRevokeResult{}, operationFailedError(operationID, "撤销托管凭据失败")
	}
	if bumpErr := s.bumpRevision(ctx, plan, operationID); bumpErr != nil {
		s.logError("撤销托管凭据后推进资源 revision 失败", plan, operationID, bumpErr)
		s.finish(ctx, operationID, core.OperationUnknown, lifecycleResultInternal,
			"凭据已撤销但资源 revision 未推进")
		return CredentialRevokeResult{}, operationFailedError(operationID,
			"凭据已撤销，但资源 revision 未能推进；请核对操作记录后再继续")
	}
	s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "")
	return CredentialRevokeResult{
		OperationID:     operationID,
		CredentialID:    plan.credentialID,
		RevocationEpoch: detail.RevocationEpoch,
		Revision:        plan.revision,
		Changed:         true,
	}, nil
}

// writeResult 回读权威行给出结论；回读失败只记 ERROR——写已经成功，不得改写成失败。
// writeResult reads the authoritative rows back; a read-back failure is logged, never
// surfaced as a failed write.
func (s *CredentialOps) writeResult(ctx context.Context, plan credentialWritePlan,
	operationID string,
) CredentialWriteResult {
	result := CredentialWriteResult{
		OperationID:  operationID,
		CredentialID: plan.credentialID,
		VersionNo:    plan.versionNo,
		Revision:     plan.revision,
		Last4:        plan.version.Last4,
		State:        core.CredentialIdentityActive,
		Changed:      true,
	}
	identity, err := s.deps.Credentials.Credential(ctx, plan.credentialID)
	if err != nil {
		s.logError("回读托管凭据失败", plan, operationID, err)
		return result
	}
	result.State = identity.State
	if plan.retire {
		return result
	}
	if stored, versionErr := s.deps.Credentials.Version(ctx, plan.credentialID,
		identity.CurrentVersion); versionErr == nil {
		result.Last4 = stored.Last4
	}
	return result
}

// applyCredential 执行仓储层的三选一：撤销 / 退役 / 新建或追加版本。
// applyCredential performs exactly one repository change.
func (s *CredentialOps) applyCredential(ctx context.Context, plan credentialWritePlan,
	operationID string,
) error {
	switch {
	case plan.revoke:
		return s.deps.Credentials.Revoke(ctx, plan.credentialID, operationID, plan.now)
	case plan.retire:
		return s.deps.Credentials.Retire(ctx, plan.credentialID, operationID, plan.now)
	case plan.identityFound:
		return s.deps.Credentials.AppendVersion(ctx, plan.credentialID, plan.keyID, operationID,
			plan.version, plan.now)
	default:
		return s.deps.Credentials.Create(ctx, core.CredentialRecord{
			ID:          plan.credentialID,
			OwnerKind:   plan.kind,
			OwnerUID:    plan.uid,
			OwnerName:   plan.name,
			State:       core.CredentialIdentityActive,
			KeyID:       plan.keyID,
			OperationID: operationID,
		}, plan.version)
	}
}

// bumpRevision 让目标资源行前进一格 revision 并记上本次凭据动作：模型连接走发布指针
// （同一发布版本重发布，只推进 revision 与 operation_id），通知渠道**只**覆盖凭据列——
// 地址列属于调用方自己那次写入，不得被这里读到的旧快照回写覆盖（managed 的地址在密文里，
// 退役后地址是调用方刚写入的 `${VAR}` 占位）。目标已删除时**不**推进——撤销不得让墓碑复活。
// bumpRevision advances the resource revision once; for a channel it only touches the
// credential columns so the caller's own address write survives.
func (s *CredentialOps) bumpRevision(ctx context.Context, plan credentialWritePlan,
	operationID string,
) error {
	if plan.deleted {
		return nil
	}
	switch plan.kind {
	case core.BindingModelConnection:
		return s.deps.Resources.Publish(ctx, core.ResourceModelConnection, plan.name,
			plan.publishedVersionID, true, operationID, plan.now)
	case core.BindingNotifyChannel:
		credentialID := plan.credentialID
		if plan.mode != core.CredentialModeManaged {
			// 退役 / 撤销后行上不得再挂着凭据身份（env 模式与 credential_id 互斥）。
			credentialID = ""
		}
		return s.deps.Notify.UpdateCredentialColumns(ctx, plan.uid, plan.mode, credentialID,
			operationID)
	case core.BindingMCPServer:
		// MCP 的生效不走凭据写路径（受控重建是 `mcp_server.apply`），这里只推进发布
		// 指针的 revision：撤销因此对下一次准入可见，不需要重启。
		return s.deps.Resources.Publish(ctx, core.ResourceMCPServer, plan.name,
			plan.publishedVersionID, false, operationID, plan.now)
	default:
		return fmt.Errorf("%w: 资源类别 %q 不支持托管凭据",
			core.ErrCredentialUnavailable, plan.kind)
	}
}

// beginOperation 预写 pending 行并返回操作标识；失败即拒绝执行（目标零写入）。
// beginOperation pre-writes the pending row; a failure refuses the change.
func (s *CredentialOps) beginOperation(ctx context.Context, actor core.Actor, action string,
	plan credentialWritePlan, requestHash string, detail credentialActionDetail,
) (string, error) {
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		// 结构只有字符串与整数，此分支不可达；仍不吞错误，按预写失败处理。
		return "", fmt.Errorf("%s 序列化预写详情失败，拒绝执行: %w", action, err)
	}
	operationID := core.NewOperationID(plan.now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   string(plan.kind),
		TargetID:     string(plan.uid),
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  requestHash,
		ExpectedETag: plan.expectedETag,
		DetailJSON:   detailJSON,
		Status:       core.OperationPending,
		CreatedAt:    plan.now,
	}
	if beginErr := s.deps.Operations.BeginOperation(ctx, record); beginErr != nil {
		s.logError("凭据动作预写操作记录失败，拒绝执行", plan, "", beginErr)
		return "", fmt.Errorf("%s 预写操作记录失败，拒绝执行: %w", action, beginErr)
	}
	return operationID, nil
}

// finish 回填终态：脱离请求取消（`context.WithoutCancel`），失败只记 ERROR。
// finish records the terminal state detached from request cancellation.
func (s *CredentialOps) finish(ctx context.Context, operationID string, status core.OperationStatus,
	resultCode, message string,
) {
	if err := s.deps.Operations.FinishOperation(context.WithoutCancel(ctx), operationID, status,
		resultCode, message, nil, s.deps.Now()); err != nil {
		s.log().Error("凭据管理操作回填终态失败",
			"operation_id", operationID, "status", string(status), "error", err)
	}
}

// logError 记一条不含凭据材料的 ERROR。
// logError never carries credential material.
func (s *CredentialOps) logError(message string, plan credentialWritePlan, operationID string,
	err error,
) {
	s.log().Error(message,
		"kind", string(plan.kind), "resource", plan.name, "operation_id", operationID,
		"credential_id", plan.credentialID, "error", err)
}

// log 返回装配的 logger，缺省用 slog.Default。
func (s *CredentialOps) log() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.Default()
	}
	return s.deps.Logger
}

// sealWrite 用主密钥给明文加密并填好候选版本行；主密钥每次重新装载、用完清零。
// sealWrite seals the plaintext with a freshly loaded master key and zeroes it.
func (s *CredentialOps) sealWrite(plan *credentialWritePlan, value []byte) error {
	key, err := LoadMasterKey(effectiveMasterKeyFile(s.deps.KeyPath, s.deps.MasterKeyFile), s.deps.WorkspaceRoot)
	if err != nil {
		return err
	}
	defer key.Zero()
	aad := CredentialAAD{
		OwnerKind:     plan.kind,
		OwnerUID:      plan.uid,
		CredentialID:  plan.credentialID,
		VersionNo:     plan.versionNo,
		TargetBinding: plan.target,
	}
	nonce, ciphertext, err := SealCredential(key, aad, value)
	if err != nil {
		return err
	}
	plan.keyID = key.KeyID
	plan.version = core.CredentialVersionRecord{
		KeyID:         key.KeyID,
		TargetBinding: plan.target,
		Last4:         credentialLast4(value),
		Nonce:         nonce,
		Ciphertext:    ciphertext,
	}
	return nil
}

// writeDetail 是写入动作的恢复投影：写后 revision 是**预判**值，写前值来自当前行。
// writeDetail projects the write action; Revision is the predicted post-change value.
func (p credentialWritePlan) writeDetail() credentialActionDetail {
	return credentialActionDetail{
		Kind:             string(p.kind),
		Name:             p.name,
		ResourceUID:      string(p.uid),
		Revision:         p.revision,
		PreviousRevision: p.previousRevision,
		Mode:             string(p.mode),
		CredentialID:     p.credentialID,
		VersionNo:        p.versionNo,
		PreviousVersion:  p.previousVersion,
		TargetBinding:    p.target,
	}
}

// revokeDetail 是撤销动作的恢复投影：纪元候选值 = 写前 + 1，版本号停在当前版本。
// revokeDetail projects a revoke; the epoch candidate is the pre-change value plus one.
func (p credentialWritePlan) revokeDetail() credentialActionDetail {
	return credentialActionDetail{
		Kind:             string(p.kind),
		Name:             p.name,
		ResourceUID:      string(p.uid),
		Revision:         p.revision,
		PreviousRevision: p.previousRevision,
		CredentialID:     p.credentialID,
		VersionNo:        p.versionNo,
		PreviousVersion:  p.previousVersion,
		TargetBinding:    p.target,
		BindingKey:       p.bindingKey,
		RevocationEpoch:  p.identity.RevocationEpoch + 1,
		PreviousEpoch:    p.identity.RevocationEpoch,
	}
}

// credentialWriteAction / credentialRevokeAction 给出契约 §9.3 的固定动作字面量。
// The fixed action literals of the credential write and revoke action groups.
func credentialWriteAction(kind core.BindingKind) string {
	if kind == core.BindingNotifyChannel {
		return actionNotifyChannelCredentialWrite
	}
	return actionModelConnectionCredentialWrite
}

func credentialRevokeAction(kind core.BindingKind) string {
	switch kind {
	case core.BindingNotifyChannel:
		return actionNotifyChannelCredentialRevoke
	case core.BindingMCPServer:
		return core.ActionMCPServerCredentialRevoke
	default:
		return actionModelConnectionCredentialRevoke
	}
}

// credentialWriteRequestHash / credentialRevokeRequestHash 摘要一次请求：材料只含类别、
// 名称、UID、模式与目标绑定哈希，**不含**明文与完整地址。
// The request digests never carry plaintext or the full address.
func credentialWriteRequestHash(plan credentialWritePlan) string {
	return agentRequestHash(strings.Join([]string{
		string(plan.kind), plan.name, string(plan.uid), string(plan.mode),
		plan.credentialID, strconv.Itoa(plan.versionNo), plan.target,
	}, "\n"))
}

func credentialRevokeRequestHash(plan credentialWritePlan) string {
	return agentRequestHash(strings.Join([]string{
		string(plan.kind), plan.name, string(plan.uid), plan.credentialID, plan.bindingKey,
	}, "\n"))
}

// newCredentialID 生成 `cred_` + 26 位 Crockford 载荷的身份：载荷与资源 UID 同源，
// 换前缀不换生成器（领域只有一个随机源）。
// newCredentialID mints a cred_ identity from the same Crockford generator as resource uids.
func newCredentialID() (string, error) {
	uid, err := core.NewResourceUID()
	if err != nil {
		return "", fmt.Errorf("生成凭据标识: %w", err)
	}
	return credentialIDPrefix + strings.TrimPrefix(string(uid), core.ResourceUIDPrefix), nil
}

// credentialLast4 只保留可安全展示的尾号：原文短于 8 个字符时给空串（不泄露大半内容）。
// credentialLast4 keeps the display tail only when the value is long enough to hide.
func credentialLast4(value []byte) string {
	if len(value) < 8 {
		return ""
	}
	return string(value[len(value)-4:])
}

// zeroCredentialValue 清零调用方传入的明文切片（best-effort）。
// zeroCredentialValue clears the caller-supplied plaintext in place.
func zeroCredentialValue(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

// ============================================================
// secrets 子命令服务面（T134 装配 / T135 CLI 消费）
// ============================================================
//
// `SecretsService` 只做四件事：`init` 生成主密钥（已存在或存在托管依赖即拒绝）、
// `status` / `verify` 只读报告、`revoke-managed` 在服务停止时撤销全部托管凭据。
// 三条纪律：
//   - **不自动轮换**：生成新密钥不会迁移旧密文；已有密钥就拒绝，换了就是永久丢数据；
//   - **只回指纹**：`status` / `verify` 的输出里没有密钥、密文或地址，指纹只是比对值；
//   - **撤销不需要解密**：`revoke-managed` 走既有 `CredentialOps.Revoke`，清空密文、
//     升纪元、写 `*.credential.revoke` 操作行——与 HTTP 面的撤销是同一条路径。
//
// The secrets service mints the master key once, reports status and verify counts, and
// revokes every managed credential through the very path the HTTP surface uses.

// 编译期断言：本类型就是 `SecretsService` 端口的实现。
// Compile-time assertion that the type below is the port's implementation.
var _ SecretsService = (*secretsService)(nil)

// SecretsServiceDeps 是 secrets 服务面的装配输入：凭据仓储、活动渠道只读面、撤销编排、
// 校验用的解析器与主密钥文件位置。缺任一必需依赖即拒绝构造。
// SecretsServiceDeps wires the secrets surface; every required dependency is explicit.
type SecretsServiceDeps struct {
	Credentials *storage.CredentialStore
	Channels    NotifyChannelCatalog
	Ops         *CredentialOps
	Resolver    *credentialResolver
	// Instance 是实例加密三态与首次初始化的唯一实现（T091）：`secrets init` 的**生成**
	// 动作走它，命令面因此与启动路径共用同一套"先预写审计、排他发布、失败转 adopt"的
	// 语义，不出现第二个生成入口。
	// Instance is the one minting implementation shared with the startup path.
	Instance *InstanceSecretsService
	// MasterKeyFile 是静态初值；KeyPath 是共享的生效路径（实例在运行中完成显式
	// 初始化后立即切换，命令面与页面读到同一事实）。
	// MasterKeyFile is the static seed; KeyPath is the live shared path.
	MasterKeyFile string
	KeyPath       *MasterKeyLocator
	WorkspaceRoot string
	Logger        *slog.Logger
}

// secretsService 是 `SecretsService` 的唯一实现。
// secretsService is the only implementation of the secrets port.
type secretsService struct {
	deps SecretsServiceDeps
}

// masterKeyFile 返回本服务当前生效的主密钥路径。
// masterKeyFile returns the currently effective key path.
func (s *secretsService) masterKeyFile() string {
	return effectiveMasterKeyFile(s.deps.KeyPath, s.deps.MasterKeyFile)
}

// NewSecretsService 组装 secrets 服务面；缺依赖即拒绝构造（不降级成"什么都没做"）。
// NewSecretsService refuses to construct without its required dependencies.
func NewSecretsService(deps SecretsServiceDeps) (SecretsService, error) {
	if deps.Credentials == nil || deps.Channels == nil || deps.Ops == nil || deps.Resolver == nil {
		return nil, errors.New("secrets 服务面装配缺少仓储、撤销编排或解析器依赖")
	}
	return &secretsService{deps: deps}, nil
}

// Init 生成主密钥并落盘。拒绝次序固定：**先**看文件是否已存在（最常见的误操作，且换
// 密钥会让既有密文永久不可解），**再**看是否存在当前托管依赖（同样会变孤儿）。
// 纯 env 部署可以不跑本命令；未配置 `secrets.master_key_file` 时显式拒绝。
// Init mints the master key; an existing file or any current managed dependency refuses.
func (s *secretsService) Init(ctx context.Context, actor core.Actor) (SecretsStatus, error) {
	if strings.TrimSpace(s.masterKeyFile()) == "" {
		return SecretsStatus{}, fmt.Errorf(
			"%w: 未配置主密钥文件（secrets.master_key_file）——纯 env 部署可跳过 secrets init",
			core.ErrCredentialUnavailable)
	}
	exists, err := masterKeyFileExists(s.masterKeyFile())
	if err != nil {
		return SecretsStatus{}, err
	}
	if exists {
		return SecretsStatus{}, fmt.Errorf(
			"%w: 主密钥文件已存在，拒绝覆盖（不提供自动轮换；确需更换请先离线备份并 revoke-managed）",
			core.ErrCredentialUnavailable)
	}
	models, channels, err := s.managedDependencyCounts(ctx)
	if err != nil {
		return SecretsStatus{}, err
	}
	if models+channels > 0 {
		return SecretsStatus{}, fmt.Errorf(
			"当前存在 %d 个托管模型连接凭据与 %d 个托管通知渠道：生成新主密钥会让既有密文永久不可解——先执行 secrets revoke-managed --confirm",
			models, channels)
	}
	if s.deps.Instance == nil {
		// 装配缺口如实报：命令面不另走一条生成路径（那会绕开排他发布与审计预写）。
		// A wiring gap is reported as such; there is no second minting path.
		return SecretsStatus{}, errors.New("secrets 服务面装配缺少实例加密服务")
	}
	generated, err := s.deps.Instance.Init(ctx, actor)
	if err != nil {
		return SecretsStatus{}, err
	}
	return SecretsStatus{
		Initialized:             true,
		State:                   generated.Decision.State,
		Reason:                  generated.Decision.Reason,
		KeyID:                   generated.KeyID,
		Fingerprint:             generated.Fingerprint,
		ManagedModelConnections: models,
		ManagedNotifyChannels:   channels,
	}, nil
}

// Status 报告主密钥是否就绪与当前托管依赖计数。未配置路径 = 纯 env（`Initialized=false`
// 且不报错）；配置了却读不回来 = 部署错误，如实上抛（CLI 打印并取非零退出码）。
// Status reports key readiness and dependency counts; a configured-but-unreadable key
// returns the error instead of a quiet false.
func (s *secretsService) Status(ctx context.Context) (SecretsStatus, error) {
	models, channels, err := s.managedDependencyCounts(ctx)
	if err != nil {
		return SecretsStatus{}, err
	}
	status := SecretsStatus{ManagedModelConnections: models, ManagedNotifyChannels: channels}
	if strings.TrimSpace(s.masterKeyFile()) == "" {
		// 未配置路径 = 纯 env 实例：三态是"未初始化"，命令面据此提示而不是报故障。
		status.State = core.SecretStateUninitialized
		return status, nil
	}
	if s.deps.Instance != nil {
		// 三态判定是**唯一**的状态来源（T085 / T091）：`Initialized` 保持既有语义
		// （密钥文件可读），恢复态与待 adopt 由 State / Reason 表达。
		// The decision table is the only source of the state; Initialized keeps its
		// long-standing "key file is readable" meaning.
		verdict, err := s.deps.Instance.Status(ctx)
		if err != nil {
			return SecretsStatus{}, err
		}
		status.State = verdict.Decision.State
		status.Reason = verdict.Decision.Reason
		switch verdict.Decision.Action {
		case SecretsActionReady, SecretsActionAdopt:
			status.Initialized = true
			status.KeyID = verdict.KeyID
			status.Fingerprint = verdict.Fingerprint
			return status, nil
		case SecretsActionRecover:
			// 恢复态：如实上抛（命令面取非零退出码并打印原因），不静默报成"未初始化"。
			return status, fmt.Errorf(
				"%w: 主密钥处于恢复态（原因 %q）——不生成替代密钥、不回落 env；按 docs/Operations.md 的主密钥恢复步骤处置",
				core.ErrCredentialUnavailable, verdict.Decision.Reason)
		default:
			return status, nil
		}
	}
	key, err := LoadMasterKey(s.masterKeyFile(), s.deps.WorkspaceRoot)
	if err != nil {
		return SecretsStatus{}, err
	}
	defer key.Zero()
	fingerprint, err := MasterKeyFingerprint(key)
	if err != nil {
		return SecretsStatus{}, err
	}
	status.Initialized = true
	status.State = core.SecretStateReady
	status.KeyID = key.KeyID
	status.Fingerprint = fingerprint
	return status, nil
}

// Verify 逐条校验**当前 active 的托管凭据**能否在当前主密钥下解密、目标绑定是否仍与
// 当前定义一致，只回检查与失败计数；失败逐条记 WARN（点名资源与凭据 ID，不含材料）。
// Verify checks every active managed credential and reports counts only.
func (s *secretsService) Verify(ctx context.Context) (SecretsVerifyReport, error) {
	active, err := s.deps.Credentials.ListActive(ctx)
	if err != nil {
		return SecretsVerifyReport{}, fmt.Errorf("列举托管凭据: %w", err)
	}
	report := SecretsVerifyReport{Checked: len(active)}
	for _, record := range active {
		if verifyErr := s.deps.Resolver.verifyIdentity(ctx, record); verifyErr != nil {
			report.Failed++
			s.logger().Warn("secrets verify：凭据校验失败",
				"kind", string(record.OwnerKind), "resource", record.OwnerName,
				"credential", record.ID, "error", verifyErr)
		}
	}
	return report, nil
}

// RevokeManaged 在服务停止时撤销全部 active 托管凭据：逐个走 `CredentialOps.Revoke`
// （清空密文、升纪元、写操作行），不需要解密。中途失败即返回已撤销条数与错误——
// 撤销是幂等的，重跑会跳过已撤销的身份。
// RevokeManaged revokes every active managed credential without decrypting anything.
func (s *secretsService) RevokeManaged(ctx context.Context, actor core.Actor) (int, error) {
	active, err := s.deps.Credentials.ListActive(ctx)
	if err != nil {
		return 0, fmt.Errorf("列举托管凭据: %w", err)
	}
	revoked := 0
	for _, record := range active {
		result, revokeErr := s.deps.Ops.Revoke(ctx, CredentialRevokeRequest{
			Actor: actor,
			Kind:  record.OwnerKind,
			Name:  record.OwnerName,
			UID:   record.OwnerUID,
		})
		if revokeErr != nil {
			return revoked, fmt.Errorf("撤销凭据 %s（资源 %s）: %w", record.ID, record.OwnerName, revokeErr)
		}
		if result.Changed {
			revoked++
		}
	}
	s.logger().Info("托管凭据已全部撤销", "revoked", revoked)
	return revoked, nil
}

// managedDependencyCounts 统计"当前托管依赖"：active 的模型连接凭据身份，加上活动且
// `managed` 的通知渠道。两者都是 `secrets init` 的拒绝条件——新建密钥会让它们变孤儿。
// managedDependencyCounts counts the refuse conditions of secrets init.
func (s *secretsService) managedDependencyCounts(ctx context.Context) (int, int, error) {
	active, err := s.deps.Credentials.ListActive(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("统计托管模型连接凭据: %w", err)
	}
	models := 0
	for _, record := range active {
		if record.OwnerKind == core.BindingModelConnection {
			models++
		}
	}
	channels, err := s.deps.Channels.ListNotifyChannels(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("统计托管通知渠道: %w", err)
	}
	managed := 0
	for _, channel := range channels {
		if channel.CredentialMode == core.CredentialModeManaged {
			managed++
		}
	}
	return models, managed, nil
}

// logger 返回生效的日志器（nil 回落默认）。
// logger returns the effective logger.
func (s *secretsService) logger() *slog.Logger {
	if s.deps.Logger != nil {
		return s.deps.Logger
	}
	return slog.Default()
}

// validateManagedAddress 校验托管通知渠道的完整地址：绝对 URL、http / https、host 非空、
// 不带 userinfo。错误只给固定短语，**不**回显地址。
// validateManagedAddress checks scheme, host and userinfo without echoing the address.
func validateManagedAddress(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return &core.InvalidInputError{Tool: "凭据地址", Reason: "通知渠道地址不是合法的绝对 URL"}
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return &core.InvalidInputError{Tool: "凭据地址", Reason: "通知渠道地址只接受 http / https"}
	}
	if parsed.User != nil {
		return &core.InvalidInputError{
			Tool:   "凭据地址",
			Reason: "通知渠道地址不得携带 userinfo（凭证进 Key，不进 URL）",
		}
	}
	return nil
}
