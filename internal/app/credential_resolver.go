package app

// 本文件实现 `CredentialResolver` 端口（模块卡 R4.8 / T130）：按资源 UID 解析**当前**
// active 版本的托管凭据，并在三个消费面给出同一份判定：
//   - `ResolveModelCredential`：模型连接 → 明文 Key（发布校验 / 连接测试 / 作者模型）；
//   - `ResolveNotifyCredential`：通知渠道 → 完整投递地址（每次发送重新解析）；
//   - `CheckBinding`：准入期的**只读**状态检查（不解密，只回答"还能不能用"）；
//   - `VerifyStartup`：启动期只校验「当前托管模型连接」，其余托管资源不阻断启动。
//
// 五条纪律：
//   - **不缓存**：每次解析都重读权威行与主密钥文件，撤销在下一发 / 下一调用即生效；
//   - **不回落**：托管失败就是失败，**不得**退回 env、旧版本或自动生成新密钥；
//   - **启动依赖最小**：只有"当前发布 + 被活动区已注册 Agent 绑定"的托管模型连接
//     参与启动校验；纯 env 实例、托管通知渠道、仅归档引用都不读主密钥；
//   - **目标地址绑定**：模型连接的 `base_url` 变了就必须重新录入 Key——解析时用当前
//     发布定义重算目标绑定并与凭据版本比对，不一致即不可用；
//   - **错误不回显材料**：只点名资源、凭据 ID 与 `key_id`；Key、密文与完整地址一律
//     不进错误文案与日志。
//
// This file implements the credential resolver: current-version resolution for model
// connections and notification channels, the read-only admission check, and the
// startup dependency scan limited to bound managed model connections.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// 编译期断言：解析器同时满足凭据解析端口与准入的只读检查面。
// Compile-time assertions that the resolver satisfies both consumer faces.
var (
	_ CredentialResolver     = (*credentialResolver)(nil)
	_ BindingCredentialCheck = (*credentialResolver)(nil)
	_ CredentialPinSource    = (*credentialResolver)(nil)
)

// CredentialResolverDeps 是解析器的只读依赖集合：四个仓储与主密钥文件位置。
// 主密钥**不**在这里读取——每次解析都重新 `LoadMasterKey`。
// CredentialResolverDeps carries the read-only dependencies; the master key is loaded
// on every resolution and never held here.
type CredentialResolverDeps struct {
	Credentials *storage.CredentialStore
	Resources   *storage.ResourceStore
	Notify      *storage.NotifyAdminStore
	Bindings    *storage.BindingStore
	// MasterKeyFile 是静态初值；KeyPath 是进程内共享的生效路径（用户显式初始化后
	// 立即切换）。有 KeyPath 时以它为准。
	// MasterKeyFile is the static seed; KeyPath is the live shared path when present.
	MasterKeyFile string
	KeyPath       *MasterKeyLocator
	WorkspaceRoot string
	Logger        *slog.Logger
}

// credentialResolver 是 `CredentialResolver` 的唯一实现。
// credentialResolver is the only implementation of the resolver port.
type credentialResolver struct {
	deps CredentialResolverDeps
}

// NewCredentialResolver 构造解析器；缺任一必需依赖即拒绝构造（不是"运行到再说"）。
// NewCredentialResolver refuses to construct without its required stores.
func NewCredentialResolver(deps CredentialResolverDeps) (CredentialResolver, error) {
	return newCredentialResolver(deps)
}

// newCredentialResolver 返回**具体类型**：装配层要用它的启动校验（`VerifyStartup`）与
// `secrets verify` 的逐条校验，而端口只声明了运行期消费的两件事。两个入口共用同一份
// 校验，具体类型不外泄到包外。
// newCredentialResolver keeps the concrete type for the assembly's startup and secrets
// checks; both constructors share one validation path.
func newCredentialResolver(deps CredentialResolverDeps) (*credentialResolver, error) {
	if deps.Credentials == nil || deps.Resources == nil || deps.Notify == nil {
		return nil, errors.New("凭据解析器装配缺少仓储依赖")
	}
	return &credentialResolver{deps: deps}, nil
}

// ResolveModelCredential 解析模型连接当前 active 版本的明文 Key。
// 先核对当前指针仍指向该 UID（同名重建不自动接管），再用当前发布定义重算目标绑定，
// 最后解密；任一步不成立都返回固定类别的错误，**不**回落到 env。
// ResolveModelCredential decrypts the current active key of one model connection after
// verifying the pointer still points at that uid and the target binding is unchanged.
func (r *credentialResolver) ResolveModelCredential(ctx context.Context,
	connection core.ResourceVersionRef,
) (string, error) {
	uid, err := core.ParseResourceUID(string(connection.ResourceUID))
	if err != nil {
		return "", fmt.Errorf("解析模型连接凭据: %w", err)
	}
	version, err := r.publishedVersion(ctx, core.ResourceModelConnection, connection)
	if err != nil {
		return "", err
	}
	definition, err := parseProviderDefinition(connection.Name, version.DefinitionYAML)
	if err != nil {
		return "", err
	}
	mode, err := credentialModeOf(connection.Name, definition)
	if err != nil {
		return "", err
	}
	if mode != core.CredentialModeManaged {
		return "", fmt.Errorf("%w: 模型连接 %s 不是托管模式，不能按托管凭据解析",
			core.ErrCredentialUnavailable, connection.Name)
	}
	target, err := CredentialTargetBinding(core.BindingModelConnection, definition.BaseURL, "")
	if err != nil {
		return "", err
	}
	if err := r.requireCurrentPointer(ctx, core.ResourceModelConnection, connection.Name, uid); err != nil {
		return "", err
	}
	plaintext, _, err := r.openManaged(ctx, core.BindingModelConnection, uid, target)
	if err != nil {
		return "", err
	}
	r.logResolved(ctx, string(core.BindingModelConnection), connection.Name)
	return string(plaintext), nil
}

// ResolveNotifyCredential 解析通知渠道当前 active 版本的完整投递地址。
// 渠道行只带 host 与凭据引用；地址本身是密文材料，只在本次投递中使用。返回值同时带上
// 实际使用的凭据身份与版本号——通知不固定版本，投递记录必须能追溯“这一次发的是哪一版”。
// ResolveNotifyCredential decrypts the full destination of a managed channel for one
// delivery and reports which credential version was actually used; the address never
// leaves this call.
func (r *credentialResolver) ResolveNotifyCredential(ctx context.Context,
	channel core.NotifyChannel,
) (core.NotifyCredentialResolution, error) {
	if channel.CredentialMode != core.CredentialModeManaged {
		return core.NotifyCredentialResolution{}, fmt.Errorf("%w: 通知渠道 %s 不是托管模式，不能按托管凭据解析",
			core.ErrCredentialUnavailable, channel.Name)
	}
	if strings.TrimSpace(channel.CredentialID) == "" {
		return core.NotifyCredentialResolution{}, fmt.Errorf("%w: 通知渠道 %s 缺少凭据引用",
			core.ErrCredentialUnavailable, channel.Name)
	}
	uid, err := core.ParseResourceUID(string(channel.ResourceUID))
	if err != nil {
		return core.NotifyCredentialResolution{}, fmt.Errorf("解析通知渠道凭据: %w", err)
	}
	identity, err := r.deps.Credentials.CredentialByOwner(ctx, core.BindingNotifyChannel, uid)
	if err != nil {
		return core.NotifyCredentialResolution{}, r.identityError(ctx, core.BindingNotifyChannel, uid, err)
	}
	if identity.ID != channel.CredentialID {
		return core.NotifyCredentialResolution{}, fmt.Errorf("%w: 通知渠道 %s 的凭据引用与当前身份不一致（重新录入后再试）",
			core.ErrCredentialOwnerMismatch, channel.Name)
	}
	plaintext, version, err := r.openManaged(ctx, core.BindingNotifyChannel, uid, "")
	if err != nil {
		return core.NotifyCredentialResolution{}, err
	}
	r.logResolved(ctx, string(core.BindingNotifyChannel), channel.Name)
	return core.NotifyCredentialResolution{
		Target:            string(plaintext),
		CredentialID:      identity.ID,
		CredentialVersion: version.VersionNo,
	}, nil
}

// PinModelCredential 为运行清单固定托管凭据的**当前**版本（准入期，不解密、不读主
// 密钥）：身份与当前版本都必须 active，且版本的目标绑定必须与当前发布定义重算出的
// 结果一致。撤销、状态非法或地址漂移一律失败关闭，**不得**改钉另一个版本、也不得
// 降级为 env 继续跑。
// PinModelCredential pins the current managed version at admission; revocation, an
// illegal state or a drifted target address all fail closed.
func (r *credentialResolver) PinModelCredential(ctx context.Context, uid core.ResourceUID,
	targetBinding string,
) (core.CredentialPin, error) {
	identity, err := r.deps.Credentials.CredentialByOwner(ctx, core.BindingModelConnection, uid)
	if err != nil {
		return core.CredentialPin{}, r.identityError(ctx, core.BindingModelConnection, uid, err)
	}
	if err := r.checkManagedState(ctx, core.BindingModelConnection, uid, targetBinding); err != nil {
		return core.CredentialPin{}, err
	}
	version, err := r.deps.Credentials.Version(ctx, identity.ID, identity.CurrentVersion)
	if err != nil {
		return core.CredentialPin{}, fmt.Errorf("%w: 凭据 %s 的当前版本不可读",
			core.ErrCredentialUnavailable, identity.ID)
	}
	return core.CredentialPin{
		Mode:          core.CredentialModeManaged,
		ID:            identity.ID,
		Version:       version.VersionNo,
		TargetBinding: version.TargetBinding,
	}, nil
}

// CheckBinding 是准入期的只读检查：只回答"该资源的凭据还能不能用"，**不解密**、
// 不读主密钥。env 模式恒可用；managed 模式要求身份与当前版本均 active，且模型连接的
// 目标绑定与当前发布定义一致。
// CheckBinding answers whether the credential is still usable at admission; it never
// decrypts and never reads the master key.
func (r *credentialResolver) CheckBinding(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) error {
	bindingKind, ok := credentialBindingKind(kind)
	if !ok {
		return fmt.Errorf("%w: 绑定类别 %q 不支持凭据检查", core.ErrAgentResourceBindingInvalid, kind)
	}
	switch bindingKind {
	case core.BindingModelConnection:
		pointer, err := r.deps.Resources.PointerByUID(ctx, core.ResourceModelConnection, uid)
		if err != nil {
			return fmt.Errorf("%w: 模型连接 %s 没有当前发布指针", core.ErrAgentResourceUnavailable, uid)
		}
		version, err := r.deps.Resources.VersionByID(ctx, pointer.PublishedVersionID)
		if err != nil {
			return fmt.Errorf("%w: 模型连接 %s 的发布版本不可读", core.ErrAgentResourceUnavailable, pointer.ResourceID)
		}
		definition, err := parseProviderDefinition(pointer.ResourceID, version.DefinitionYAML)
		if err != nil {
			return err
		}
		mode, err := credentialModeOf(pointer.ResourceID, definition)
		if err != nil {
			return err
		}
		if mode == core.CredentialModeEnv {
			return nil
		}
		target, err := CredentialTargetBinding(core.BindingModelConnection, definition.BaseURL, "")
		if err != nil {
			return err
		}
		return r.checkManagedState(ctx, bindingKind, uid, target)
	case core.BindingNotifyChannel:
		channel, err := r.deps.Notify.GetByUID(ctx, uid)
		if err != nil {
			return fmt.Errorf("%w: 通知渠道 %s 不可读", core.ErrAgentResourceUnavailable, uid)
		}
		if channel.CredentialMode == core.CredentialModeEnv {
			return nil
		}
		if channel.CredentialMode != core.CredentialModeManaged {
			return fmt.Errorf("%w: 通知渠道 %s 的凭据模式非法", core.ErrCredentialUnavailable, channel.Name)
		}
		return r.checkManagedState(ctx, bindingKind, uid, "")
	}
	return fmt.Errorf("%w: 绑定类别 %q 不支持凭据检查", core.ErrAgentResourceBindingInvalid, kind)
}

// VerifyStartup 只校验「当前托管模型连接」：当前发布指针 → managed → 被至少一个
// **活动区已注册** Agent 的绑定指向。无依赖时**不读主密钥**（纯 env 实例、只有托管
// 通知渠道、只有归档引用都属此类）。校验失败拒绝启动并点名资源。
// VerifyStartup validates only bound managed model connections and never reads the
// master key when none exist.
func (r *credentialResolver) VerifyStartup(ctx context.Context, activeAgents []string) error {
	if r.deps.Bindings == nil {
		return errors.New("凭据启动校验缺少绑定投影依赖")
	}
	active := make(map[string]struct{}, len(activeAgents))
	for _, name := range activeAgents {
		active[name] = struct{}{}
	}
	pointers, err := r.deps.Resources.ListPointers(ctx, core.ResourceModelConnection)
	if err != nil {
		return fmt.Errorf("启动校验: 列出已发布模型连接: %w", err)
	}
	for _, pointer := range pointers {
		version, versionErr := r.deps.Resources.VersionByID(ctx, pointer.PublishedVersionID)
		if versionErr != nil {
			return fmt.Errorf("启动校验: 读取模型连接 %s 的发布版本: %w", pointer.ResourceID, versionErr)
		}
		definition, defErr := parseProviderDefinition(pointer.ResourceID, version.DefinitionYAML)
		if defErr != nil {
			return fmt.Errorf("启动校验: %w", defErr)
		}
		mode, modeErr := credentialModeOf(pointer.ResourceID, definition)
		if modeErr != nil {
			return fmt.Errorf("启动校验: %w", modeErr)
		}
		if mode != core.CredentialModeManaged {
			continue
		}
		bound, boundErr := r.boundByActiveAgent(ctx, pointer.ResourceUID, active)
		if boundErr != nil {
			return boundErr
		}
		if !bound {
			continue
		}
		ref := core.ResourceVersionRef{
			Name:        pointer.ResourceID,
			ResourceUID: pointer.ResourceUID,
			VersionID:   version.ID,
			VersionNo:   version.VersionNo,
			ContentHash: version.ContentHash,
		}
		if _, resolveErr := r.ResolveModelCredential(ctx, ref); resolveErr != nil {
			return fmt.Errorf("当前托管模型连接 %s 的凭据不可用: %w", pointer.ResourceID, resolveErr)
		}
	}
	return nil
}

// verifyIdentity 校验一条 active 身份能否在当前主密钥下解密，且（模型连接）目标绑定
// 仍与当前发布定义一致。**只**供 `secrets verify` 计数使用：不返回材料，失败原因只
// 点名资源与凭据 ID。
// verifyIdentity checks one active identity for the secrets verify count; it never
// returns decrypted material.
func (r *credentialResolver) verifyIdentity(ctx context.Context, record core.CredentialRecord) error {
	if record.State != core.CredentialIdentityActive {
		return fmt.Errorf("%w: 凭据 %s 状态为 %s", core.ErrCredentialUnavailable, record.ID, record.State)
	}
	expectedTarget := ""
	if record.OwnerKind == core.BindingModelConnection {
		pointer, err := r.deps.Resources.PointerByUID(ctx, core.ResourceModelConnection, record.OwnerUID)
		if err != nil {
			return fmt.Errorf("%w: 模型连接 %s 没有当前发布指针", core.ErrAgentResourceUnavailable, record.OwnerName)
		}
		version, err := r.deps.Resources.VersionByID(ctx, pointer.PublishedVersionID)
		if err != nil {
			return fmt.Errorf("%w: 模型连接 %s 的发布版本不可读", core.ErrAgentResourceUnavailable, record.OwnerName)
		}
		definition, err := parseProviderDefinition(pointer.ResourceID, version.DefinitionYAML)
		if err != nil {
			return err
		}
		expectedTarget, err = CredentialTargetBinding(core.BindingModelConnection, definition.BaseURL, "")
		if err != nil {
			return err
		}
	}
	plaintext, _, err := r.openManaged(ctx, record.OwnerKind, record.OwnerUID, expectedTarget)
	if err != nil {
		return err
	}
	zeroCredentialValue(plaintext)
	return nil
}

// publishedVersion 取连接引用的发布版本：优先用调用方冻结的版本 ID，缺省时按名称
// 查当前发布指针。版本必须确实属于该资源 UID——否则就是凭据被搬到别的资源上。
// publishedVersion resolves the pinned version id, falling back to the current pointer.
func (r *credentialResolver) publishedVersion(ctx context.Context, kind core.ResourceKind,
	ref core.ResourceVersionRef,
) (core.ResourceVersion, error) {
	versionID := ref.VersionID
	if versionID == 0 {
		pointer, err := r.deps.Resources.Pointer(ctx, kind, ref.Name)
		if err != nil {
			return core.ResourceVersion{}, fmt.Errorf("%w: 资源 %s 没有发布指针", core.ErrCredentialUnavailable, ref.Name)
		}
		versionID = pointer.PublishedVersionID
	}
	version, err := r.deps.Resources.VersionByID(ctx, versionID)
	if err != nil {
		return core.ResourceVersion{}, fmt.Errorf("%w: 资源 %s 的发布版本不可读", core.ErrCredentialUnavailable, ref.Name)
	}
	if version.Kind != kind || version.ResourceUID != ref.ResourceUID {
		return core.ResourceVersion{}, fmt.Errorf("%w: 资源 %s 的发布版本归属不符", core.ErrCredentialOwnerMismatch, ref.Name)
	}
	return version, nil
}

// requireCurrentPointer 核对名称的当前指针仍是该 UID（同名重建不自动接管旧凭据）。
// requireCurrentPointer refuses when the name no longer points at this uid.
func (r *credentialResolver) requireCurrentPointer(ctx context.Context, kind core.ResourceKind,
	name string, uid core.ResourceUID,
) error {
	pointer, err := r.deps.Resources.Pointer(ctx, kind, name)
	if err != nil || pointer.DeletedAt != nil {
		return fmt.Errorf("%w: 资源 %s 已删除或没有当前指针", core.ErrCredentialUnavailable, name)
	}
	if pointer.ResourceUID != uid {
		return fmt.Errorf("%w: 资源 %s 的当前指针已换成另一个资源实例",
			core.ErrCredentialOwnerMismatch, name)
	}
	return nil
}

// checkManagedState 判定托管凭据的权威状态（不解密）：身份 active、当前版本 active、
// 目标绑定（模型连接）与当前定义一致。撤销 → `ErrCredentialRevoked`（准入 409）。
// checkManagedState reads the authoritative state without touching ciphertext.
func (r *credentialResolver) checkManagedState(ctx context.Context, kind core.BindingKind,
	uid core.ResourceUID, expectedTarget string,
) error {
	identity, err := r.deps.Credentials.CredentialByOwner(ctx, kind, uid)
	if err != nil {
		return r.identityError(ctx, kind, uid, err)
	}
	if identity.OwnerKind != kind || identity.OwnerUID != uid {
		return fmt.Errorf("%w: 凭据 %s 的属主与资源不一致", core.ErrCredentialOwnerMismatch, identity.ID)
	}
	switch identity.State {
	case core.CredentialIdentityRevoked:
		return fmt.Errorf("%w: 凭据 %s 已撤销", core.ErrCredentialRevoked, identity.ID)
	case core.CredentialIdentityActive:
	default:
		return fmt.Errorf("%w: 凭据 %s 状态为 %s", core.ErrCredentialUnavailable, identity.ID, identity.State)
	}
	version, err := r.deps.Credentials.Version(ctx, identity.ID, identity.CurrentVersion)
	if err != nil {
		return fmt.Errorf("%w: 凭据 %s 的当前版本不可读", core.ErrCredentialUnavailable, identity.ID)
	}
	switch version.State {
	case core.CredentialVersionRevoked:
		return fmt.Errorf("%w: 凭据 %s 版本 %d 已撤销", core.ErrCredentialRevoked, identity.ID, version.VersionNo)
	case core.CredentialVersionActive:
	default:
		return fmt.Errorf("%w: 凭据 %s 当前版本 %d 状态为 %s",
			core.ErrCredentialUnavailable, identity.ID, version.VersionNo, version.State)
	}
	if expectedTarget != "" && version.TargetBinding != expectedTarget {
		return fmt.Errorf("%w: 凭据 %s 的目标地址与当前定义不一致，必须重新录入 Key",
			core.ErrCredentialUnavailable, identity.ID)
	}
	return nil
}

// openManaged 取当前 active 版本并解密；失败按固定类别返回，**不**缓存主密钥。
// 同时返回实际使用的版本记录——通知投递记录要写明“这一次发的是哪一版”（R4.8）。
// openManaged decrypts the current active version with a freshly loaded master key and
// reports the version record it used.
func (r *credentialResolver) openManaged(ctx context.Context, kind core.BindingKind,
	uid core.ResourceUID, expectedTarget string,
) ([]byte, core.CredentialVersionRecord, error) {
	identity, err := r.deps.Credentials.CredentialByOwner(ctx, kind, uid)
	if err != nil {
		return nil, core.CredentialVersionRecord{}, r.identityError(ctx, kind, uid, err)
	}
	if err := r.checkManagedState(ctx, kind, uid, expectedTarget); err != nil {
		return nil, core.CredentialVersionRecord{}, err
	}
	version, err := r.deps.Credentials.Version(ctx, identity.ID, identity.CurrentVersion)
	if err != nil {
		return nil, core.CredentialVersionRecord{}, fmt.Errorf("%w: 凭据 %s 的当前版本不可读",
			core.ErrCredentialUnavailable, identity.ID)
	}
	masterKey, err := LoadMasterKey(effectiveMasterKeyFile(r.deps.KeyPath, r.deps.MasterKeyFile), r.deps.WorkspaceRoot)
	if err != nil {
		return nil, core.CredentialVersionRecord{}, err
	}
	defer masterKey.Zero()
	if masterKey.KeyID != version.KeyID {
		return nil, core.CredentialVersionRecord{}, fmt.Errorf("%w: 主密钥 key_id %s 与凭据 %s 版本 %d 的 key_id 不匹配",
			core.ErrCredentialUnavailable, masterKey.KeyID, identity.ID, version.VersionNo)
	}
	aad := CredentialAAD{
		OwnerKind:     kind,
		OwnerUID:      uid,
		CredentialID:  identity.ID,
		VersionNo:     version.VersionNo,
		TargetBinding: version.TargetBinding,
	}
	plaintext, err := OpenCredential(masterKey, aad, version.Nonce, version.Ciphertext)
	if err != nil {
		return nil, core.CredentialVersionRecord{}, err
	}
	return plaintext, version, nil
}

// identityError 把"按属主查不到凭据"收敛成固定类别：managed 模式下缺身份是数据问题，
// 不是"没配凭据"。
// identityError collapses a missing identity into the fixed unavailable class.
func (r *credentialResolver) identityError(_ context.Context, kind core.BindingKind,
	uid core.ResourceUID, err error,
) error {
	if errors.Is(err, core.ErrResourceNotFound) {
		return fmt.Errorf("%w: 资源 %s 的托管凭据身份缺失", core.ErrCredentialUnavailable, uid)
	}
	return fmt.Errorf("读取资源 %s 的凭据身份: %w", uid, err)
}

// boundByActiveAgent 判定资源是否被至少一个活动区已注册 Agent 的投影行引用。
// 归档副本也会出现在投影里，因此必须用活动区名单过滤——仅归档引用不算启动依赖。
// boundByActiveAgent filters projection rows by the active-area agent name set; archived
// copies are projected too and must not count as a startup dependency.
func (r *credentialResolver) boundByActiveAgent(ctx context.Context, uid core.ResourceUID,
	active map[string]struct{},
) (bool, error) {
	rows, err := r.deps.Bindings.ReferencingAgents(ctx, uid)
	if err != nil {
		return false, fmt.Errorf("启动校验: 读取资源 %s 的绑定引用: %w", uid, err)
	}
	for _, row := range rows {
		if _, ok := active[row.AgentName]; ok {
			return true, nil
		}
	}
	return false, nil
}

// logResolved 记一条 Debug：资源名与类别可见，凭据材料不可见。
// logResolved logs the resource kind and name only.
func (r *credentialResolver) logResolved(ctx context.Context, kind, name string) {
	if r.deps.Logger == nil {
		return
	}
	r.deps.Logger.DebugContext(ctx, "托管凭据按需解析成功", "kind", kind, "resource", name)
}

// credentialModeOf 把定义里的 `credential_mode` 归一为两值模式：未声明 = env
// （兼容 v2 定义），非法值拒绝。模式只由字段判定，**不**按值的形态推断。
// credentialModeOf defaults an undeclared mode to env and refuses unknown literals.
func credentialModeOf(name string, definition resourceProviderDefinition) (core.CredentialMode, error) {
	raw := strings.TrimSpace(definition.CredentialMode)
	if raw == "" {
		return core.CredentialModeEnv, nil
	}
	mode, err := core.ParseCredentialMode(raw)
	if err != nil {
		return "", fmt.Errorf("模型连接 %s 的凭据模式非法: %w", name, err)
	}
	return mode, nil
}
