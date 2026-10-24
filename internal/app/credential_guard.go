package app

// 本文件实现 `CredentialGuard` 端口（模块卡 R4.8 / T131，FR-078）：Run 内**每次模型
// 调用前**的检查点。Run 在准入时固定了"哪个凭据的哪一版"，此后每次调用都回来问一次
// 权威行——撤销必须在下一发即生效，**不得**等到 Run 结束。
//
// 四条纪律：
//   - **只读**：检查不写库、不升纪元、不改状态；它只回答"还能不能继续用这一版"；
//   - **固定版本优先**：查的是 Run 固定下来的 `credential_version`，不是当前活动版本——
//     换 Key 不会把在途 Run 悄悄换到新版本（新版本只服务新 Run）；
//   - **撤销终态**：身份或版本 `revoked` → `ErrCredentialRevoked`（Run 终止）；
//     `retired`（删除资源 / 模式切换）**不**腰斩在途 Run——密文保留到最后一个引用释放，
//     新 Run 在准入处已失败关闭；版本缺失 / 密文已被清空 → `ErrCredentialUnavailable`
//     （固定错误，**不**回落到 env 或新版本）；
//   - **并发安全**：本类型无内部状态，每次调用只读仓储；同一次 Run 的并发检查得到
//     同一结论（仓储读事务各自独立）。
//
// 关于撤销纪元：`run_credential_refs` 只有版本号、没有纪元列，而撤销在**同一事务**里
// 把身份与全部版本置 `revoked` 并把密文置 NULL——于是"版本状态 + 密文是否还在"就是
// 纪元的完整证据，无需新增列或第二套比对。
//
// This file implements the pre-call credential checkpoint: it re-reads the pinned version
// on every model call, never writes, and fails closed on revocation.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// 编译期断言：本类型就是端口声明的检查点实现。
// Compile-time assertion that this type is the port's only implementation.
var _ CredentialGuard = (*credentialGuard)(nil)

// CredentialGuardDeps 是检查点的只读依赖。
// CredentialGuardDeps carries the guard's read-only dependencies.
type CredentialGuardDeps struct {
	Credentials *storage.CredentialStore
	Logger      *slog.Logger
}

// credentialGuard 是 `CredentialGuard` 的唯一实现。
// credentialGuard is the only implementation of the guard port.
type credentialGuard struct {
	deps CredentialGuardDeps
}

// NewCredentialGuard 构造检查点；缺仓储即拒绝构造（检查点不得退化成空放行）。
// NewCredentialGuard refuses to construct without the credential store.
func NewCredentialGuard(deps CredentialGuardDeps) (CredentialGuard, error) {
	if deps.Credentials == nil {
		return nil, errors.New("凭据检查点装配缺少凭据仓储")
	}
	return &credentialGuard{deps: deps}, nil
}

// Check 校验 Run 固定的凭据版本仍可继续使用：
//   - 身份 `active` 可继续；`revoked` → `ErrCredentialRevoked`；`retired`（资源已删除 /
//     模式已切换）可继续——在途 Run 用完即止，新 Run 准入时已被拒；
//   - 固定版本的属主必须是引用里的资源（同名重建不接管旧凭据）；
//   - 版本 `active` / `superseded` 可继续（换 Key 之后在途 Run 仍用旧版）；
//     `revoked` → `ErrCredentialRevoked`，`retired` 同样可继续；
//   - 非撤销版本却没有密文 = 材料被清空 → 不可用。
//
// Check verifies the pinned credential version is still usable; it never decrypts and
// never writes.
func (g *credentialGuard) Check(ctx context.Context, ref core.RunCredentialRef) error {
	if err := core.ValidateCredentialID(ref.CredentialID); err != nil {
		return fmt.Errorf("%w: 运行固定的凭据引用非法", core.ErrCredentialUnavailable)
	}
	if ref.CredentialVersion < 1 {
		return fmt.Errorf("%w: 运行固定的凭据版本号必须 >= 1", core.ErrCredentialUnavailable)
	}
	if ref.ReleasedAt != nil {
		return fmt.Errorf("%w: 运行 %s 已释放凭据 %s", core.ErrCredentialUnavailable, ref.RunID, ref.CredentialID)
	}

	identity, err := g.deps.Credentials.CredentialByOwner(ctx, ref.ResourceKind, ref.ResourceUID)
	if err != nil {
		if errors.Is(err, core.ErrResourceNotFound) {
			return fmt.Errorf("%w: 资源 %s 的凭据身份已不存在", core.ErrCredentialUnavailable, ref.ResourceName)
		}
		return fmt.Errorf("检查运行凭据 %s: 读取身份: %w", ref.CredentialID, err)
	}
	if identity.ID != ref.CredentialID {
		return fmt.Errorf("%w: 运行 %s 固定的凭据 %s 不属于资源 %s（当前身份为 %s）",
			core.ErrCredentialOwnerMismatch, ref.RunID, ref.CredentialID, ref.ResourceName, identity.ID)
	}
	switch identity.State {
	case core.CredentialIdentityRevoked:
		return g.revoked(ctx, ref, "凭据身份", 0)
	case core.CredentialIdentityActive, core.CredentialIdentityRetired:
	default:
		return fmt.Errorf("%w: 凭据 %s 状态为 %s", core.ErrCredentialUnavailable, identity.ID, identity.State)
	}

	version, err := g.deps.Credentials.Version(ctx, identity.ID, ref.CredentialVersion)
	if err != nil {
		if errors.Is(err, core.ErrResourceNotFound) {
			return fmt.Errorf("%w: 凭据 %s 没有版本 %d",
				core.ErrCredentialUnavailable, identity.ID, ref.CredentialVersion)
		}
		return fmt.Errorf("检查运行凭据 %s: 读取版本 %d: %w", identity.ID, ref.CredentialVersion, err)
	}
	switch version.State {
	case core.CredentialVersionRevoked:
		return g.revoked(ctx, ref, "凭据版本", version.VersionNo)
	case core.CredentialVersionActive, core.CredentialVersionSuperseded, core.CredentialVersionRetired:
		// superseded 是"已被新版本替换但尚未撤销"：在途 Run 仍可继续用固定版本；
		// retired 是删除资源 / 模式切换的终态，密文保留给在途 Run 用完（新 Run 准入即拒）。
	default:
		return fmt.Errorf("%w: 凭据 %s 版本 %d 状态为 %s",
			core.ErrCredentialUnavailable, identity.ID, version.VersionNo, version.State)
	}
	if len(version.Ciphertext) == 0 {
		return fmt.Errorf("%w: 凭据 %s 版本 %d 的密文已被清空",
			core.ErrCredentialUnavailable, identity.ID, version.VersionNo)
	}
	return nil
}

// RunCredentialCheckpoint 是 Run 内模型调用前的检查点端口（FR-078 / T134 接线）：
// 按运行标识读它固定的凭据版本并逐条复核。装配缺失时不存在"漏检的 managed Run"——
// 托管定义在准入的 credentialPinOf 处已经失败关闭，没有固定版本就产生不了托管 Run。
// RunCredentialCheckpoint re-checks the credential versions a run pinned, before every
// model call; a nil checkpoint implies no managed run can exist (admission fails closed).
type RunCredentialCheckpoint interface {
	CheckRun(ctx context.Context, runID string) error
}

// RunCredentialPinStore 是检查点读固定版本的只读面（`*storage.RunCredentialStore`）。
// RunCredentialPinStore is the read-only side of run_credential_refs.
type RunCredentialPinStore interface {
	Pins(ctx context.Context, runID string) ([]core.RunCredentialRef, error)
}

// RunCredentialCheckpointDeps 是检查点的两个必需依赖。
// RunCredentialCheckpointDeps carries the checkpoint's two required dependencies.
type RunCredentialCheckpointDeps struct {
	Pins  RunCredentialPinStore
	Guard CredentialGuard
}

// runCredentialCheckpoint 是 `RunCredentialCheckpoint` 的唯一实现。
// runCredentialCheckpoint is the only implementation of the checkpoint port.
type runCredentialCheckpoint struct {
	deps RunCredentialCheckpointDeps
}

// 编译期断言：本类型就是端口声明的实现。
// Compile-time assertion that the checkpoint type satisfies its port.
var _ RunCredentialCheckpoint = runCredentialCheckpoint{}

// NewRunCredentialCheckpoint 构造检查点；缺任一依赖即拒绝构造（检查点不得退化成空放行）。
// NewRunCredentialCheckpoint refuses to construct without both dependencies.
func NewRunCredentialCheckpoint(deps RunCredentialCheckpointDeps) (RunCredentialCheckpoint, error) {
	if deps.Pins == nil || deps.Guard == nil {
		return nil, errors.New("凭据检查点装配缺少固定版本读面或守卫")
	}
	return runCredentialCheckpoint{deps: deps}, nil
}

// CheckRun 读这次 Run 固定的凭据版本并逐条检查。空运行标识 = 不是 Run（作者草稿等
// 无固定版本的调用），检查是空操作；env 模式的 Run 没有固定行，同样是空操作。
// CheckRun re-checks every pinned model-connection credential; an empty run id or a run
// without pins (env mode) is a no-op.
func (c runCredentialCheckpoint) CheckRun(ctx context.Context, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	pins, err := c.deps.Pins.Pins(ctx, runID)
	if err != nil {
		return fmt.Errorf("%w: 读取运行 %s 的凭据固定: %w", core.ErrCredentialUnavailable, runID, err)
	}
	for _, pin := range pins {
		if pin.ResourceKind != core.BindingModelConnection {
			continue
		}
		if err := c.deps.Guard.Check(ctx, pin); err != nil {
			return err
		}
	}
	return nil
}

// revoked 统一"撤销"这一类固定错误并记一条 WARN（Run 会因此终止，运维必须能看见）。
// revoked reports the terminal revoke and logs it once per failed check.
func (g *credentialGuard) revoked(_ context.Context, ref core.RunCredentialRef, scope string, versionNo int) error {
	err := fmt.Errorf("%w: %s %s 已撤销（运行 %s 固定版本 %d）",
		core.ErrCredentialRevoked, scope, ref.CredentialID, ref.RunID, ref.CredentialVersion)
	if g.deps.Logger != nil {
		g.deps.Logger.Warn("运行凭据已撤销——本次运行终止",
			"run_id", ref.RunID, "credential", ref.CredentialID, "version", versionNo)
	}
	return err
}
