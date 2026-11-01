package history

// 本文件是历史回查的**唯一授权结算处**（US-16 §3.1.10 / C-28 / T055）：把
// `core.Principal` 与请求里可选的目标调用方结算成存储端口认的 `Scope`。
//
// 结算顺序固定，任一步失败都在**访问存储之前**返回：
//
//	零值 / 非法身份        → core.ErrUnauthorized（401，不得补默认身份）
//	缺 sessions:read       → ErrForbidden（403）
//	调用方指定他人 caller  → ErrForbidden（403）——请求参数与身份冲突是"越权"，
//	                         不是"没找到"
//	其余                   → Scope{CallerID: 自己的归属} / Scope{Admin, CallerID}
//
// 跨调用方的**读取结果**反过来一律 `ErrNotFound`（404）：403 会泄露"这个会话
// 存在但不属于你"。两条错误码的分界线是"请求本身是否越权"——越权 403，越权
// 探测他人的具体数据 404。
//
// 另有一条不可退让的规则：调用方范围的 `Scope.CallerID` **不得**为空。空归属
// 在存储层等于"不过滤"，迁移遗留的空 `caller_id` 会话因此只对管理员可见，绝
// 不会被匹配到任意调用方。
//
// This file is the single place that turns a principal into a store-level scope.
// A caller scope can never be empty, because an empty owner filter means "all
// callers" in the store; legacy rows with a NULL caller_id stay admin-only.

import (
	"github.com/ymedword/basalt/internal/core"
)

// resolveScope 结算本次请求的归属范围。
//
// `requestedCallerID` 是请求里显式给出的 `caller_id` 过滤（管理员可跨调用方时
// 才允许出现）；调用方给出自己的归属是允许的，给出他人的归属直接 403。
//
// resolveScope resolves the ownership scope; a caller may only name itself.
func resolveScope(p core.Principal, requestedCallerID string) (Scope, error) {
	if !p.Valid() {
		// 零值拒绝：**不得**把缺失身份补成内置管理员。
		// The zero value is rejected and never promoted to the built-in admin.
		return Scope{}, core.ErrUnauthorized
	}
	if !p.HasScope(core.ScopeSessionsRead) {
		// 历史是归属数据，读取前提是 sessions:read；后来撤销 agents:invoke
		// 不影响对既有会话的读取（那不是当前执行授权）。
		// sessions:read is the precondition; a revoked invoke scope does not matter.
		return Scope{}, ErrForbidden
	}
	if p.Kind == core.PrincipalLocalAdmin {
		// 管理员可跨调用方；空 CallerID 即"全部"，含迁移遗留的无归属会话。
		// Admins may span callers; an empty CallerID means all of them.
		return Scope{Admin: true, CallerID: requestedCallerID}, nil
	}
	if requestedCallerID != "" && requestedCallerID != p.CallerID {
		// 请求参数与身份冲突属于越权，返回 403 而不是 404。
		// Naming someone else's caller id is a permission error, not a lookup miss.
		return Scope{}, ErrForbidden
	}
	return Scope{CallerID: p.CallerID}, nil
}

// effectiveCallerFilter 返回查询里真正生效的调用方过滤值：调用方身份恒为自己的
// 归属（**不得**被请求参数放大），管理员按请求给值。它只用于回填 `HistoryQuery`，
// 存储端口仍以 `Scope` 为准。
// effectiveCallerFilter narrows the query to what the scope already allows.
func effectiveCallerFilter(scope Scope, requestedCallerID string) string {
	if scope.Admin {
		return requestedCallerID
	}
	return scope.CallerID
}
