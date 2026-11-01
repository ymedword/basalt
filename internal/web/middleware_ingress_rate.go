package web

// 本文件是入口速率中间件（模块卡 §3.1.7 / 图 6 [1] / C-35 / T126）：认证之后、
// 作用域判定与路由处理之前，对**每个已认证业务请求**调用一次 `AdmitIngress`。三类
// 入口按路由行映射（run / ingest / read），业务行漏映射在启动自检失败（D-30）；
// 管理面路由与调度器**不进**调用方桶——admin token 解析出的内置管理员在中间件里
// 直接放行（§3.1.5）。本层只扣速率：不占并发名额、不预留配额，三段**不得**合并。
// 顺序按 C-F03 裁决：速率先于作用域 —— 桶键与目标资源无关，先扣桶不新增存在性
// 侧信道；作用域被拒的请求同样扣自己的桶（自伤，不波及其他调用方）。
// This file is the ingress-rate middleware: once per authenticated business request,
// after authentication and before the scope check (C-F03). Management traffic never
// enters a caller bucket, and this layer never touches concurrency slots or quotas.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// IngressGate 是入口速率的消费方端口（实现放 `internal/callers` 的 `*Limiter`）：
// 只判速率，超限时返回 `callers.ErrRateLimited`。端口声明在消费方是包纪律
// （技术方案 §7.1.2）；`*callers.Limiter` 结构化满足本接口，装配层直接注入，
// 不另写适配器。
// IngressGate is the consumer-side rate port. *callers.Limiter satisfies it
// structurally, so the assembly layer injects the limiter without an adapter.
type IngressGate interface {
	// AdmitIngress 判定一次已认证业务请求的入口速率（图 6 [1]），消耗不可返还。
	// AdmitIngress charges one authenticated business request; it is never refunded.
	AdmitIngress(ctx context.Context, principal core.Principal, class callers.IngressClass) error
}

// ingressRetryAfter 是 429 的 Retry-After 取值：令牌桶按 qps 连续回填，契约只要求
// 给出该头、不承诺精确等待，取 1 秒下界（客户端下一拍即可重试）。
// The bucket refills continuously and the contract only requires the header, so one
// second is the floor handed back to the client.
const ingressRetryAfter = time.Second

// checkRouteIngress 是入口速率的启动自检（D-30）：每条业务行**必须**映射到闭集内
// 的入口类别；管理行（含兜底 404）**不得**映射——管理面与调度器不受调用方桶约束。
// 未映射的业务行在启动期失败，而不是在运行期按"不限流"放行。
// Every business row must map to a known ingress class; management rows must not,
// and an unmapped business row fails the assembly instead of running unlimited.
func checkRouteIngress(rows []route) error {
	for _, rt := range rows {
		name := rt.method + " " + rt.pattern
		if rt.surface != surfaceRouteBusiness {
			if rt.ingress != "" {
				return fmt.Errorf("web: 管理面路由 %s 不得映射调用方入口类别", name)
			}
			continue
		}
		if !rt.ingress.Valid() {
			return fmt.Errorf("web: 业务面路由 %s 未映射入口类别（run / ingest / read）", name)
		}
	}
	return nil
}

// admitIngress 承担图 6 的 [1]：认证之后、作用域判定与处理函数之前判一次入口速率；
// 返回 false 表示已写拒绝响应，调用方必须直接返回。内置管理员**不**进任何调用方桶
// （§3.1.5）——管理凭据不是调用方配额的一部分。端口缺失按失败关闭处理：`NewServer`
// 已拒绝"有认证闸门却没有速率端口"的装配，这里是半截构造的兜底。
// admitIngress returns false after writing the refusal; the built-in admin is never
// charged, and a missing port fails closed.
func (s *Server) admitIngress(
	w http.ResponseWriter, r *http.Request, principal core.Principal, class callers.IngressClass,
) bool {
	if principal.Kind == core.PrincipalLocalAdmin {
		return true
	}
	gate := s.opts.Ingress
	if gate == nil {
		s.logger().Error("入口速率端口缺失，拒绝业务请求（装配错误）",
			"request_id", requestIDFrom(r.Context()),
			"method", r.Method,
			"path", safeLogValue(r.URL.Path),
			"class", string(class))
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return false
	}
	err := gate.AdmitIngress(r.Context(), principal, class)
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, callers.ErrRateLimited):
		// 契约 §8：速率拒绝自带 Retry-After，且此时**尚未**进入并发与配额判定。
		// The rate refusal carries Retry-After and precedes stages [3] and [5].
		//
		// 诊断（V-8）：拒绝按 `request_id` 记一条 `quota_decision`——请求还没变成 Run，
		// 没有 `run_id` 可用；这是入口类判定唯一的主锚点。只记拒绝（见端口注释）。
		// A denied request has no run id yet; request_id is the only anchor it has.
		s.recordIngressDenial(r, class)
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(ingressRetryAfter), 10))
		s.writeError(w, http.StatusTooManyRequests, codeCallerRateLimited,
			errorCodeMessage(codeCallerRateLimited, ""))
	case errors.Is(err, core.ErrUnauthorized):
		// 认证已通过（本层在认证之后），这里再出现未授权只能是端口缺陷：失败关闭。
		// Unauthorized here means the port disagrees with the guard: deny.
		s.writeError(w, http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// 客户端已放弃：不再写响应，也不记告警——断连是常态而非故障。
		// The client is gone; nothing to write and nothing to warn about.
	default:
		s.logger().Warn("入口速率判定失败",
			"request_id", requestIDFrom(r.Context()),
			"method", r.Method,
			"path", safeLogValue(r.URL.Path),
			"class", string(class),
			"error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	}
	return false
}

// recordIngressDenial 为一次入口速率拒绝记一条诊断事件（V-8）。三条短路：写端口未接、
// 请求没有 `request_id`（诊断不伪造锚点）、派发上下文中没有取值。
// `class` 取值加 `ingress_` 前缀：与 Run 路径上的 `run` / `chat` / `compaction` 共用一个
// 属性键，不加前缀会让"run 类"在两类判定里指向不同东西。
// recordIngressDenial records one denial; the class is prefixed so it never collides
// with the run path's class values under the same attribute key.
func (s *Server) recordIngressDenial(r *http.Request, class callers.IngressClass) {
	writer := s.opts.DiagnosticWriter
	if writer == nil {
		return
	}
	requestID := requestIDFrom(r.Context())
	if requestID == "" {
		return
	}
	writer.Record(r.Context(), diagnostics.DiagnosticEvent{
		Kind:        diagnostics.KindQuotaDecision,
		Correlation: diagnostics.Correlation{RequestID: requestID},
		Decision:    diagnostics.DecisionDeny,
		ReasonCode:  diagnostics.ReasonRateLimited,
		Attrs:       map[string]any{"class": "ingress_" + string(class)},
	})
}
