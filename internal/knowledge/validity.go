package knowledge

// 本文件实现引用有效性判定（US-16 §3.1.3 / C-09 / C-10 / T041）：一个 `EvidenceRef`
// 在**被使用之前**按**当前**身份重查 ACL、删除与版本状态，返回冻结契约
// `ValidityView.reason` 的四值结论。四条硬约束：
//
//  1. **身份显式必填**：`Check` 的 Principal 是参数，零值 → `core.ErrUnauthorized`
//     且在读取任何版本行之前拒绝；没有 `knowledge:read` → `ErrForbidden`
//     （fail-closed，与 `SourceView` 同一道身份闸）；
//  2. **每次重查、不缓存**（C-10 "删除与撤权必须立即影响后续查询"）：ACL 与删除
//     状态按本次调用重算——上一轮有效的引用在本轮可能已失效，**不得**沿用旧结论；
//  3. **失效是判定，不是错误**：不存在 → `not_found`；软删 → `deleted`；因撤权退出
//     → `revoked`；已不是当前版本 → `version_changed`。只有存储违约（回读版本不符、
//     状态不在闭集内、端口报错）才返回 error——调用方（`core.SourceFilter`）据此
//     把整条消息替换为"内容不可用"，而不是让一次判定失败冒充"引用有效"；
//  4. **不得用新原文冒充旧引用**：判定只描述引用点名的那个版本，读回其它版本即
//     违约报错（与 `SourceView` 共用 `load` 的回读校验）。
//
// 与 `SourceView` 的关系：两者共用同一组端口、同一条 ACL 重查与同一张状态判定表
// （`judgeValidity`），区别只在出口——`SourceView` 是读取端点（ACL 拒绝 → 403），
// `Check` 是判定端口（ACL 拒绝 → `revoked` 结论），**不**各自实现一套判定。
// 本方法**不**写操作审计：它按消息逐条调用（注入前校验），操作级审计由调用方的
// 那次操作（检索 / 压缩 / 来源视图）自己落库；失败面只记 WARN 日志（可观测性双轨）。
//
// This file implements evidence validity: a reference is re-checked against the current
// ACL, deletion state and version status immediately before use. Reuse is deliberate —
// Check shares the ports, the ACL re-check and the status table with SourceView; only
// the exit differs (a verdict here, a 403 there). Check writes no operation audit
// because it runs once per message; failures are logged at WARN.

import (
	"context"
	"errors"
	"fmt"

	"github.com/ymedword/basalt/internal/core"
)

// Validity 是引用有效性的判定端口（设计 §3.1.3）：供压缩注入（`core.SourceFilter`）
// 与输出前引用的核对（K-01）复用。实现**必须**每次按传入的 Principal 重算当前
// ACL / 删除 / 版本，**不得**缓存上一轮结论。
// Validity reports whether a reference can still be used as-is. Every call re-checks
// the current ACL, deletion state and version; nothing is cached.
type Validity interface {
	// Check 判定引用点名的版本现在是否仍可按原样使用。
	// Check reports whether the referenced version is still usable as-is.
	Check(ctx context.Context, p core.Principal, ref EvidenceRef) (ValidityResult, error)
}

// `*SourceViewer` 同时实现读取端点与判定端口：两者共用端口与判定表。
// The source viewer is also the validity port, sharing ports and the status table.
var _ Validity = (*SourceViewer)(nil)

// Check 判定一个引用当前是否有效。返回策略：
//   - 版本不存在 → `{Valid:false, Reason:"not_found"}`，nil error（与 `SourceView`
//     的 200 语义同源：不存在是读取结论，不是服务错误）；
//   - 集合 ACL 已拒绝 → `{Valid:false, Reason:"revoked"}`，nil error——调用方要把
//     整条消息替换为"内容不可用"，**不得**因为一次撤权让整个 Run 报错；
//   - 软删 → `deleted`（带 `DeletedAt`）；因撤权退役 → `revoked`；不是当前版本
//     （被取代 / 从未生效）→ `version_changed`；仍是当前版本 → `Valid=true`；
//   - 存储违约（回读版本不符、未知状态、端口报错）→ error，**不得**降级成"有效"。
//
// Check returns a verdict for every soft outcome (missing, revoked, deleted,
// version_changed) and only errors on a store contract violation; it never reports an
// unreadable reference as valid.
func (v *SourceViewer) Check(ctx context.Context, p core.Principal, ref EvidenceRef) (ValidityResult, error) {
	if err := v.ready(); err != nil {
		return ValidityResult{}, err
	}
	if err := v.authorize(p); err != nil {
		return ValidityResult{}, err
	}
	if !ref.Valid() {
		return ValidityResult{}, fmt.Errorf("%w: 有效性判定缺少文档或版本标识", ErrInvalidRequest)
	}
	record, err := v.load(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ValidityResult{Reason: ReasonNotFound}, nil
		}
		v.logger.WarnContext(ctx, "来源有效性判定读取失败", "error", err)
		return ValidityResult{}, err
	}
	if err := v.visible(ctx, p, record.CollectionID); err != nil {
		if errors.Is(err, ErrForbidden) {
			// 撤权是判定结论（`revoked`），不是读取错误：调用方按"内容不可用"处理。
			// C-08：被拒集合的标识与来源路径**不得**进日志——这里连日志都不写。
			return ValidityResult{Reason: ReasonRevoked, VersionID: record.VersionID}, nil
		}
		v.logger.WarnContext(ctx, "来源有效性判定解析可见范围失败", "error", err)
		return ValidityResult{}, err
	}
	validity, _, _, err := judgeValidity(record)
	if err != nil {
		v.logger.WarnContext(ctx, "来源有效性判定的版本状态违约", "error", err)
		return ValidityResult{}, err
	}
	return validity, nil
}
