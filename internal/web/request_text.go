package web

// 本文件是入口的可存储性校验（US-16 人工走查发现，2026-10-31）：路径段与查询参数
// 里出现非法 UTF-8 或 NUL 时，这些字节会原样流进 SQL 文本参数，PostgreSQL 报
// `SQLSTATE 22021`，端点返回 500——把"客户端请求形状非法"错报成"服务端故障"。
// 校验只覆盖**受保护路由**，位置在认证 → 入口速率 → 作用域之后、处理函数之前：
// 公开面不读参数，兜底只回显路径；未认证请求仍先拿 401，被限流仍先拿 429，
// 缺作用域仍先拿 403（C-F03 的准入顺序不被本校验改写）。
// The guard rejects unstorable request text on protected routes, after auth, rate, and
// scope, so the C-F03 admission order stays intact.

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// messageInvalidRequestText 是形状拒绝的固定文案（契约 §8：400 只说明请求形状非法，
// 不回显哪一段命中、也不解释存储层的限制）。
// Fixed copy for the shape denial; the storage-level reason never reaches the client.
const messageInvalidRequestText = "请求参数必须是合法 UTF-8 且不含 NUL 字符"

// requireValidRequestText 给受保护路由的处理函数套上形状守卫（认证之后的调用点）。
// requireValidRequestText wraps a protected handler with the shape guard.
func (s *Server) requireValidRequestText(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.requestTextValid(w, r) {
			next(w, r)
		}
	}
}

// requestTextValid 校验路径与全部查询参数的可存储性；不合法时由本方法写固定 400
// （调用方据此直接返回，**不得**继续处理）。
// requestTextValid writes the fixed 400 itself; callers must stop on false.
func (s *Server) requestTextValid(w http.ResponseWriter, r *http.Request) bool {
	if requestTextStorable(r.URL.Path) && queryTextStorable(r.URL.Query()) {
		return true
	}
	s.writeError(w, http.StatusBadRequest, codeBadRequest, messageInvalidRequestText)
	return false
}

// queryTextStorable 判全部键与值；`r.URL.Query()` 已丢弃无法解析的键值对，
// 这里只管"能解析但不能存储"的那一类。
// queryTextStorable checks every decoded key and value.
func queryTextStorable(query url.Values) bool {
	for key, values := range query {
		if !requestTextStorable(key) {
			return false
		}
		for _, value := range values {
			if !requestTextStorable(value) {
				return false
			}
		}
	}
	return true
}

// requestTextStorable 判一段请求文本能否安全进入存储与协议：合法 UTF-8 且不含 NUL。
// 与 `core.sanitizeMessageText` 的判据同源（同两类字节），但这里的处置是**拒绝**：
// 请求参数是调用方当场给出的形状，客户端可以改正；工具结果正文来自外部、改不了，
// 所以那一侧清洗、这一侧 400。
// Same two byte classes as the message wash; a request may be refused, while external
// tool output cannot be.
func requestTextStorable(text string) bool {
	return !strings.Contains(text, "\x00") && utf8.ValidString(text)
}
