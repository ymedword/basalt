package callers

// 本文件实现后台执行的**原授权 Key 复核**（C-32 / 设计卡 §2.14）：调度与恢复在
// 发起任何运行之前，按任务 / 运行行上记的 `(caller_id, key_id)` 重新读**那一把**
// Key，核对它此刻仍然有效且仍覆盖该 Agent。它不是认证路径——失败原因必须可分辨
// （`owner_reason` 进审计），因此这里**不**复用 `ErrUnauthorized` 的模糊口径。
// This file implements the authorizing-key recheck for background execution. Unlike
// the authentication path it reports a distinguishable owner reason for the audit.

import (
	"context"
	"errors"
	"fmt"

	"github.com/ymedword/basalt/internal/core"
)

// KeyVerifier 是 `core.OwnerKeyVerifier` 的存储侧实现：只读 `KeyStore`，不签发、
// 不轮换、**不**在检查期间改变任何状态。
// KeyVerifier implements core.OwnerKeyVerifier on top of the read side of KeyStore.
type KeyVerifier struct {
	keys KeyStore
}

// NewKeyVerifier 构造复核端口；`keys` 为 nil 时返回 nil——装配层据此把端口留空，
// 调度器随即对 caller-key 任务 fail-closed。
// NewKeyVerifier returns nil for a nil store so the scheduler keeps failing closed.
func NewKeyVerifier(keys KeyStore) *KeyVerifier {
	if keys == nil {
		return nil
	}
	return &KeyVerifier{keys: keys}
}

// KeyVerifier 必须满足调度器的复核端口。
// KeyVerifier satisfies the scheduler's verification port.
var _ core.OwnerKeyVerifier = (*KeyVerifier)(nil)

// VerifyOwnerKey 复核 `(callerID, keyID)` 这一把 Key 当前是否仍授权该 Agent 的
// 调用，成立时返回它**现在**的 scope 快照。
//
// 判定顺序是刻意的：调用方停用 → Key 不存在 → 吊销 → 过期 → scope 不覆盖。
// 吊销优先于过期，因为吊销是显式动作（`schema.sql` 的状态投影同口径）；调用方
// 停用先于 Key 状态，因为它使整把 Key 失去意义。任一不成立都返回带原因码的
// `*core.PrincipalError`——**不得**返回空 scope 让上层"当作没有权限"，
// 也**不得**改用同一调用方的其他 Key。
// VerifyOwnerKey re-reads that exact key and returns its current scopes, or a
// *core.PrincipalError carrying the owner reason.
func (v *KeyVerifier) VerifyOwnerKey(ctx context.Context, callerID, keyID, agent string) ([]string, error) {
	if v == nil || v.keys == nil {
		return nil, &core.PrincipalError{
			Reason: core.OwnerReasonKeyMissing,
			Detail: "owner key verifier is not wired",
		}
	}

	rec, err := v.keys.LookupByID(ctx, callerID, keyID)
	if err != nil {
		return nil, v.classifyLookupFailure(ctx, callerID, keyID, err)
	}
	if rec.CallerID != callerID || rec.KeyID != keyID {
		// 存储层已按 (caller_id, key_id) 过滤；这里再挡一次归属漂移。
		// The store already filters by (caller_id, key_id); this rejects drift anyway.
		return nil, &core.PrincipalError{
			Reason: core.OwnerReasonKindMismatch,
			Detail: fmt.Sprintf("key lookup returned another owner: key=%s caller=%s", rec.KeyID, rec.CallerID),
		}
	}

	switch rec.Status {
	case KeyStatusActive:
		// 继续核对 scope。
	case KeyStatusRevoked:
		return nil, &core.PrincipalError{
			Reason: core.OwnerReasonKeyRevoked,
			Detail: fmt.Sprintf("authorizing key %s is revoked", keyID),
		}
	case KeyStatusExpired:
		return nil, &core.PrincipalError{
			Reason: core.OwnerReasonKeyExpired,
			Detail: fmt.Sprintf("authorizing key %s is expired", keyID),
		}
	default:
		return nil, &core.PrincipalError{
			Reason: core.OwnerReasonKeyMissing,
			Detail: fmt.Sprintf("authorizing key %s has unknown status %q", keyID, rec.Status),
		}
	}

	if !agentScopeCovers(rec.Scopes, agent) {
		return nil, &core.PrincipalError{
			Reason: core.OwnerReasonScopeMissing,
			Detail: fmt.Sprintf("authorizing key %s does not cover agent %q", keyID, agent),
		}
	}
	return append([]string(nil), rec.Scopes...), nil
}

// classifyLookupFailure 把存储层的「未找到」拆成可分辨的原因：调用方已停用时
// `caller_disabled`，其余一律 `key_missing`。存储层**故意**不区分这几种情形
// （认证面不该成为探测面），后台执行侧的审计却必须指名原因。
// classifyLookupFailure splits the store's "not found" into a distinguishable reason
// for the audit; the authentication path deliberately keeps them merged.
func (v *KeyVerifier) classifyLookupFailure(ctx context.Context, callerID, keyID string, err error) error {
	if !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("callers: verify owner key %s: %w", keyID, err)
	}
	caller, callerErr := v.keys.LookupCaller(ctx, callerID)
	if callerErr == nil && caller.Status == CallerStatusDisabled {
		return &core.PrincipalError{
			Reason: core.OwnerReasonCallerDisabled,
			Detail: fmt.Sprintf("caller %s is disabled", callerID),
		}
	}
	return &core.PrincipalError{
		Reason: core.OwnerReasonKeyMissing,
		Detail: fmt.Sprintf("authorizing key %s is absent for caller %s", keyID, callerID),
	}
}

// agentScopeCovers 判定 scope 集合是否含 `agents:invoke:<agent>`。空 Agent 名不命中
// 任何 scope（与 `core.Principal.HasAgentScope` 同口径）。
// agentScopeCovers mirrors core.Principal.HasAgentScope for a bare scope set.
func agentScopeCovers(scopes []string, agent string) bool {
	if agent == "" {
		return false
	}
	want := core.AgentInvokeScopePrefix + agent
	for _, held := range scopes {
		if held == want {
			return true
		}
	}
	return false
}
