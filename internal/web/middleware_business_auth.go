package web

// 本文件是业务面认证中间件（模块卡 §3.1.7 / C-28 / T025）：业务端点接受调用方 Key
// 或 admin token（映射 `core.LocalAdminPrincipal`），管理面只接受 admin token。
// 认证结果 `core.Principal` 以**显式参数**交给处理函数——**不得**放进
// `context.WithValue`（宪法 VII / C-06）。
//
// 图 6 的准入顺序在 HTTP 边界的落位（C-F03 裁决）：[0] 认证（本文件）→ [1] 入口
// 速率（`server.go` 的 `business`，`admitIngress`）→ [2] 作用域（本文件的
// `requiredScope`）→ 其余由服务层承担。三段**不得**合并：本文件不做限流与配额。
// This file is the business-surface authentication middleware: business endpoints take
// a caller key or the admin token, the management surface takes the admin token only,
// and the principal reaches the handler as an explicit parameter.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// PrincipalAuthenticator 是调用方 Key 解析的消费方端口（实现放 `internal/callers`）：
// 只按凭据解析身份并校验状态 / 过期 / scope，失败返回错误且**不**区分原因。
// 端口声明在消费方是包纪律（技术方案 §7.1.2），装配层负责把真实实现注入。
// PrincipalAuthenticator is the caller-key resolution port; the consumer declares it
// and the assembly layer injects the implementation.
type PrincipalAuthenticator interface {
	// Authenticate 把请求携带的字符串凭据解析为调用方或内置管理员身份。
	// Authenticate resolves a presented credential into a principal.
	Authenticate(ctx context.Context, presented string) (core.Principal, error)
}

// 业务面拒绝的固定文案（契约 §8 允许 403 说明被拒边界；401 / 429 与管理面共用）。
// 401 不区分"缺失"与"无效"，403 不解释命中哪条规则。
// Fixed business-surface copy: 401 never distinguishes missing from invalid, and 403
// never explains which rule matched.
const messageScopeDenied = "调用方无权执行该操作"

// requiredScope 是业务行声明的作用域要求。固定 scope 逐字匹配；Agent 调用按路径
// 参数 `name` 组装 `agents:invoke:<name>`——通配 `agents:invoke:*` 只随管理员生效，
// 调用方身份里出现它时 `core.Principal.Valid()` 已经拒绝。
// requiredScope pins the scope of a business row; agent invocation derives the literal
// from the path name, and the wildcard never honours a caller principal.
type requiredScope struct {
	// literal 是固定 scope 字面量（`agent` 为真时忽略）。
	literal string
	// agent 表示按路径参数组装 `agents:invoke:<name>`。
	agent bool
}

// fixedScope 声明一个固定 scope。
// fixedScope declares one concrete scope literal.
func fixedScope(literal string) requiredScope { return requiredScope{literal: literal} }

// agentInvokeScope 声明按路径参数组装的 Agent 调用 scope。
// agentInvokeScope declares the per-agent invoke scope.
func agentInvokeScope() requiredScope { return requiredScope{agent: true} }

// declared 判定该行是否声明了一个可识别的要求：未声明的业务行在启动自检失败。
// declared reports whether the row names a known scope; an undeclared business row
// fails the startup self-check.
func (s requiredScope) declared() bool {
	return s.agent || core.KnownScope(s.literal)
}

// allows 判定身份是否满足本行要求：内置管理员恒真；调用方逐字匹配。
// allows reports whether the principal satisfies the row; the admin always passes.
func (s requiredScope) allows(p core.Principal, r *http.Request) bool {
	if p.Kind == core.PrincipalLocalAdmin {
		return true
	}
	if s.agent {
		return p.HasAgentScope(r.PathValue("name"))
	}
	return s.literal != "" && p.HasScope(s.literal)
}

// NewAuthGuardWithPrincipals 在管理闸门之上接入调用方 Key 解析端口：业务端点因此
// 同时接受 admin token 与 caller key，管理面也能把"有效 caller key"识别为 403 而
// 不是 401（契约 §8）。端口为 nil 时 caller key 一律认证失败（失败关闭，不回落成
// 管理员）。
// NewAuthGuardWithPrincipals attaches the caller-key port to the guard; a nil port
// fails closed for every caller key instead of promoting it to the admin.
func NewAuthGuardWithPrincipals(
	cfg AdminAuthConfig,
	logger *slog.Logger,
	now func() time.Time,
	principals PrincipalAuthenticator,
) (*authGuard, error) {
	guard, err := NewAuthGuard(cfg, logger, now)
	if err != nil {
		return nil, err
	}
	guard.principals = principals
	return guard, nil
}

// WrapPrincipal 把业务处理函数包装成标准 http.Handler：Host → Origin → 来源归因
// → 认证，全部通过后以显式 principal 调用 next。作用域判定**不在**这里——它属于
// 路由行（`requiredScope`），由 server.go 的业务包装在认证之后执行。
// WrapPrincipal runs host, origin, and authentication in that order, then calls next
// with the authenticated principal; scope enforcement belongs to the route row.
func (g *authGuard) WrapPrincipal(next PrincipalHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := requestIDFrom(r.Context())
		source := sourceOf(r, g.proxies)
		limitKey, ok := g.gate(w, r, requestID, source, surfaceBusiness)
		if !ok {
			return
		}
		principal, failure := g.authenticatePrincipal(r)
		if failure != nil {
			g.recordFailure(requestID, source, limitKey, *failure)
			g.reject(w, requestID, r, source, surfaceBusiness, *failure)
			return
		}
		next(w, r, principal)
	})
}

// authenticatePrincipal 按配置模式解析业务面身份：token 模式先比 admin token、
// 再按 caller key 查表；local_trust 模式**也不**把"本机"当成业务凭据——业务端点
// 仍要求显式凭据，只有管理面的信任三元组成立时才映射成内置管理员。
// authenticatePrincipal resolves the business credential: the admin token first, then
// the caller key. Local trust never substitutes for a business credential by itself.
func (g *authGuard) authenticatePrincipal(r *http.Request) (core.Principal, *authFailure) {
	switch g.cfg.Mode {
	case adminAuthModeToken:
		return g.businessPrincipalToken(r)
	case adminAuthModeLocalTrust:
		return g.businessPrincipalLocalTrust(r)
	default:
		// 构造函数已拦截未知模式；手工拼装的闸门一律拒绝，不把零值 tokenHash
		// 当成有效凭据。
		// A hand-built guard with an unknown mode denies by default.
		return core.Principal{}, &authFailure{
			rule: "unknown_mode", status: http.StatusUnauthorized,
			code: codeUnauthorized, message: messageUnauthorized, counted: true,
		}
	}
}

// businessPrincipalToken 判定 token 模式下的业务身份。
// businessPrincipalToken resolves a business request in token mode.
func (g *authGuard) businessPrincipalToken(r *http.Request) (core.Principal, *authFailure) {
	token, ok := bearerToken(r)
	if !ok {
		return core.Principal{}, &authFailure{
			rule: "credential_missing", status: http.StatusUnauthorized,
			code: codeUnauthorized, message: messageUnauthorized, counted: true,
		}
	}
	sum := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(sum[:], g.tokenHash[:]) == 1 {
		// admin token 是业务面的合法凭据：映射成内置管理员（契约 §8 / C-28）。
		// The admin token is a valid business credential and maps to the built-in admin.
		return core.LocalAdminPrincipal(), nil
	}
	if principal, ok := g.resolveCallerKey(r.Context(), token); ok {
		return principal, nil
	}
	return core.Principal{}, &authFailure{
		rule: "credential_invalid", status: http.StatusUnauthorized,
		code: codeUnauthorized, message: messageUnauthorized, counted: true,
	}
}

// businessPrincipalLocalTrust 判定 local_trust 模式下的业务身份：携带凭据时只认
// caller key（坏凭据一律 401，**不得**因对端回环而升级）；不带凭据时，只有管理面
// 的信任三元组成立才映射成内置管理员，其余一律 401。
// businessPrincipalLocalTrust never promotes a bad credential because the peer is
// loopback; without a credential only the local-trust triple maps to the admin.
func (g *authGuard) businessPrincipalLocalTrust(r *http.Request) (core.Principal, *authFailure) {
	if token, ok := bearerToken(r); ok {
		if principal, resolved := g.resolveCallerKey(r.Context(), token); resolved {
			return principal, nil
		}
		return core.Principal{}, &authFailure{
			rule: "credential_invalid", status: http.StatusUnauthorized,
			code: codeUnauthorized, message: messageUnauthorized, counted: true,
		}
	}
	peer, ok := peerIP(r)
	if !ok || !peer.IsLoopback() || hasForwardedHeader(r.Header) ||
		r.Header.Get(localTrustHeader) != "1" {
		return core.Principal{}, &authFailure{
			rule: "credential_missing", status: http.StatusUnauthorized,
			code: codeUnauthorized, message: messageUnauthorized, counted: true,
		}
	}
	return core.LocalAdminPrincipal(), nil
}

// resolveCallerKey 经端口解析调用方 Key：实现返回错误、返回零值或返回未通过
// `Valid()` 的身份一律当认证失败（**不得**信任端口"说成功就算成功"）。
// resolveCallerKey fails closed on any port error, the zero value, or an invalid
// principal—the caller never trusts "no error" alone.
func (g *authGuard) resolveCallerKey(ctx context.Context, presented string) (core.Principal, bool) {
	if g.principals == nil {
		return core.Principal{}, false
	}
	principal, err := g.principals.Authenticate(ctx, presented)
	if err != nil || !principal.Valid() {
		return core.Principal{}, false
	}
	return principal, true
}

// validCallerKey 判定凭据是否是**有效**调用方 Key：管理面据此把"拿 caller key 调
// 管理端点"映射成 403 而不是 401（契约 §8 行 493），两条凭据面都不接受它。
// validCallerKey lets the management surface answer 403 for a valid caller key.
func (g *authGuard) validCallerKey(ctx context.Context, presented string) bool {
	_, ok := g.resolveCallerKey(ctx, presented)
	return ok
}
