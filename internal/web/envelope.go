package web

// 本文件是统一响应信封与唯一写信封出口（约束 2 / 3 / 23）：成功含 data、失败省略
// data，时间戳固定 RFC3339 UTC；错误文案只出固定公开文案，底层原因永不进入响应。
// The envelope and its only write exits: success carries data, failure omits it, and
// internal reasons never reach the response.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// Envelope 是唯一响应信封（契约 components.schemas.Envelope）。
// data 在失败时省略——契约口径「失败时通常为空」。
// Envelope is the single response shape; data is omitted on failure.
type Envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data,omitempty"`
	Timestamp string `json:"timestamp"` // RFC3339 UTC
}

// 错误码固定字面量（契约 §8 错误码表）。
const (
	codeOK              = "OK"
	codeBadRequest      = "BAD_REQUEST"
	codeUnsupportedType = "UNSUPPORTED_TYPE" // 契约保留；本 US 不产生
	codeNotFound        = "NOT_FOUND"
	codeSessionBusy     = "SESSION_BUSY"
	codeAgentBusy       = "AGENT_BUSY"
	codeTooManyRuns     = "TOO_MANY_RUNS"
	codeLimitReached    = "LIMIT_REACHED"
	codeTimeout         = "TIMEOUT"
	codeNotConfigured   = "NOT_CONFIGURED" // 契约保留；US-12 作者模型
	codeInternal        = "INTERNAL"

	// US-11 新增（模块卡 §6.3 固定集合；`IMPORT_REJECTED` 属 US-12，本 US 不产生）。
	// US-11 additions; IMPORT_REJECTED belongs to US-12.
	codeUnauthorized       = "UNAUTHORIZED"
	codeForbidden          = "FORBIDDEN"
	codeEditUnsupported    = "EDIT_UNSUPPORTED"
	codeAgentUnavailable   = "AGENT_UNAVAILABLE"
	codeConflict           = "CONFLICT"
	codePreconditionFailed = "PRECONDITION_FAILED"
	codeAuthRateLimited    = "AUTH_RATE_LIMITED"
)

// messageSuccess / messageBusy / messageInternal 是固定文案；messageBusy 逐字取自
// run-and-errors §3.1，**不得**改写。
const (
	messageSuccess  = "success"
	messageBusy     = "该会话有正在执行的任务，请等待完成或稍后重试"
	messageInternal = "服务器内部错误"
)

// 运行原因对外投影文案（约束 23 / 注 15）。
const (
	publicReasonFailed    = "运行失败"
	publicReasonCancelled = "运行已取消"
	publicReasonLimit     = "运行达到上限"
)

// limitReachedMessage 按 RunResult.Reason 组装 LIMIT_REACHED 的 message——契约
// （run-and-errors §8）要求「说明撞了哪条上限 + 可调配置键」：
//
//	core.ReasonIterationLimit → 「运行达到上限：超过最大迭代次数（可调配置键：budget.max_iterations）」
//	core.ReasonToolBudget     → 「运行达到上限：工具调用次数超限（可调配置键：budget.max_tool_calls）」
//
// 未知 reason 只回固定公开文案「运行达到上限」——**不得**回 reason 原文（约束 23）。
// Unknown reasons fall back to the fixed public text, never the raw reason.
func limitReachedMessage(reason string) string {
	switch reason {
	case core.ReasonIterationLimit:
		return "运行达到上限：超过最大迭代次数（可调配置键：budget.max_iterations）"
	case core.ReasonToolBudget:
		return "运行达到上限：工具调用次数超限（可调配置键：budget.max_tool_calls）"
	default:
		return publicReasonLimit
	}
}

// publicRunReason 把 RunResult 的原始原因投影为对外固定文案（约束 23 / 注 15）：
//
//	cancelled → publicReasonCancelled；failed → publicReasonFailed；
//	limit_reached → 仅 core.ReasonIterationLimit / core.ReasonToolBudget 原样保留，
//	其余 → publicReasonLimit。
//
// 原始原因只进 runs.reason 与日志，**永不**进入响应。
// Only the two whitelisted limit reasons survive; everything else is projected.
func publicRunReason(status core.RunStatus, reason string) string {
	switch status {
	case core.StatusCancelled:
		return publicReasonCancelled
	case core.StatusFailed:
		return publicReasonFailed
	case core.StatusLimitReached:
		if reason == core.ReasonIterationLimit || reason == core.ReasonToolBudget {
			return reason
		}
		return publicReasonLimit
	default:
		return ""
	}
}

// 内部诊断脱敏（约束 23 的日志面）：failed 的原始原因可能含绝对路径、SQL 正文、
// 引号内的用户数据与凭证键值。这些内容允许进日志用于排障，但必须先经投影——
// 原始哨兵、路径与 SQL 正文不得落盘（R15 的 200 路径日志断言依赖此保证）。
// Internal diagnostics are projected before logging: paths, quoted literals, URLs,
// credential pairs, and SQL bodies are replaced.
var (
	// authorization 头单独一条规则：连同 scheme 与凭证一起吞掉。若交给通用键值
	// 规则，`\S+` 只吃到 scheme（Bearer / Basic），把凭证原样留在后面——独立复核
	// R5R-01 已实测该绕过。
	runReasonAuth = regexp.MustCompile(`(?i)\bauthorization\b\s*[:=]\s*\S+(?:\s+\S+)?`)
	// 凭证值优先整体识别带引号内容（允许空格与反斜杠转义），无引号值再回落到
	// 非空白匹配；未闭合引号按「过度遮蔽」吞到串尾。若沿用 `\S+`，它只吃到
	// 首段（含左引号），后续引号规则无法识别剩余串，凭证尾部会落盘——人工
	// review ② 以 `password="prefix SECRET-TAIL-P41"` 复现（R7）。
	// Quoted credential values are consumed as a whole; unquoted ones fall back to
	// a non-space run, and an unterminated quote over-redacts to end of text.
	runReasonSecret = regexp.MustCompile(
		`(?i)\b(api[_-]?key|token|secret|password|passwd)\b\s*[:=]\s*(?:"(?:\\.|[^"\\])*(?:"|$)|'(?:\\.|[^'\\])*(?:'|$)|\S+)`)
	// 引号规则支持反斜杠转义：`'it\'s ...'` 整体投影，不留下后半截（R5R-04）。
	runReasonQuoted = regexp.MustCompile(`'(?:\\.|[^'\\])*'|"(?:\\.|[^"\\])*"`)
	runReasonBearer = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)
	runReasonURL    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	// 路径允许空格（到行尾或冒号为止），并覆盖 Windows 盘符与 UNC 前缀（R5R-04）。
	runReasonPath = regexp.MustCompile(`(?:[A-Za-z]:\\|/|\\\\)[^\n:]*`)
	// SQL 正文跨行投影到分号或串尾：失败原因可能是多行语句（R5R-04）。宁可在
	// 文本里过度遮蔽，也不让表名 / 列名落盘。
	runReasonSQL = regexp.MustCompile(
		`(?is)\b(?:select|insert|update|delete|replace|pragma|attach|detach|create|drop|alter)\b[^;]*`)
)

// maxRunReasonDiagnostic 限制诊断长度，避免异常原因把日志行撑爆。
// maxRunReasonDiagnostic bounds one diagnostic field.
const maxRunReasonDiagnostic = 256

// sanitizeRunReason 把原始失败原因投影为可落盘的内部诊断：Authorization 头（含
// scheme 与凭证）、凭证键值、Bearer 令牌、引号字面量、URL、绝对 / UNC 路径与 SQL
// 正文依次替换为固定占位，最后按字符截断。
// 投影是单向的：调用方**不得**把结果写进响应（响应只走 publicRunReason）。
// sanitizeRunReason projects a raw failure reason into a log-safe diagnostic.
func sanitizeRunReason(reason string) string {
	out := runReasonAuth.ReplaceAllString(reason, "Authorization=<redacted>")
	out = runReasonSecret.ReplaceAllString(out, `${1}=<redacted>`)
	out = runReasonBearer.ReplaceAllString(out, "Bearer <redacted>")
	out = runReasonQuoted.ReplaceAllString(out, "<literal>")
	out = runReasonURL.ReplaceAllString(out, "<url>")
	out = runReasonPath.ReplaceAllString(out, "<path>")
	out = runReasonSQL.ReplaceAllString(out, "<sql>")
	out = strings.TrimSpace(out)
	if runes := []rune(out); len(runes) > maxRunReasonDiagnostic {
		out = string(runes[:maxRunReasonDiagnostic]) + "…"
	}
	return out
}

// writeOK 写 200 成功信封；data 为 nil 即无载荷成功（归档的唯一例外）。
// writeOK writes the 200 success envelope; nil data omits the field.
func (s *Server) writeOK(w http.ResponseWriter, data any) {
	s.writeEnvelope(w, http.StatusOK, Envelope{
		Code: codeOK, Message: messageSuccess, Data: data, Timestamp: s.timestamp(),
	})
}

// writeOKMessage 写 200 成功信封并替换 message——两个使用方：取消端点（契约
// openapi /runs/{run_id}/cancel 要求用 message 说明"尚未结束 / 无需取消"）与
// unknown 操作详情（提示"结果未查证、不得盲目重做"）。两者都只改提示，不改 data。
// writeOKMessage is the 200-with-custom-message exit: cancellation and the unknown
// operation hint both change the message only, never the data.
func (s *Server) writeOKMessage(w http.ResponseWriter, data any, message string) {
	s.writeEnvelope(w, http.StatusOK, Envelope{
		Code: codeOK, Message: message, Data: data, Timestamp: s.timestamp(),
	})
}

// writeResult 写 200 但 code 非 OK 的唯一场景（LIMIT_REACHED）：message 由 RunView
// 里保留的原始 reason 组装，点名可调配置键；data 原样作为载荷。
// writeResult is the only 200-with-non-OK-code exit; the message names the budget key.
func (s *Server) writeResult(w http.ResponseWriter, code string, data any) {
	message := messageSuccess
	if code == codeLimitReached {
		reason := ""
		if view, ok := data.(RunView); ok {
			reason = view.Reason
		}
		message = limitReachedMessage(reason)
	}
	s.writeEnvelope(w, http.StatusOK, Envelope{
		Code: code, Message: message, Data: data, Timestamp: s.timestamp(),
	})
}

// writeError 写失败信封：失败路径**永不**设置 data（契约：失败时通常为空）。
// writeError writes the failure envelope; data is never set on failures.
func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	s.writeEnvelope(w, status, Envelope{
		Code: code, Message: message, Timestamp: s.timestamp(),
	})
}

// writeEnvelope 是 Server 侧的落盘入口；认证闸门没有 *Server，也走下面的包级
// writeEnvelope——**不得**出现第二份信封序列化或第二套 Content-Type 约定。
// The Server-side entry delegates to the package-level serializer so the auth guard
// can share it; a second envelope writer would be a second contract.
func (s *Server) writeEnvelope(w http.ResponseWriter, status int, env Envelope) {
	writeEnvelope(w, status, env, s.logger())
}

// writeEnvelope 是唯一落盘点：Content-Type 固定、编码失败记 ERROR 并退回 500 信封。
// writeEnvelope is the only serialization point; an encoding failure logs and falls
// back to a 500 envelope rather than writing a half-formed body.
func writeEnvelope(w http.ResponseWriter, status int, env Envelope, logger *slog.Logger) {
	raw, err := json.Marshal(env)
	if err != nil {
		logger.Error("写响应信封：编码失败", "error", err, "status", status)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"INTERNAL","message":"服务器内部错误"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(raw); err != nil {
		// 连接已断是客户端行为，不是服务端故障；留 WARN 便于排障（不写第二份响应）。
		logger.Warn("写响应体失败", "error", err)
	}
}

// timestamp 是信封时间戳的唯一来源：固定 RFC3339 UTC。
// timestamp is the single source of the envelope timestamp, always RFC3339 UTC.
func (s *Server) timestamp() string {
	return s.now().UTC().Format(time.RFC3339)
}

// now 取装配注入的时钟；未注入时退回 time.Now（NewServer 已补缺省，此处兜底）。
// now returns the injected clock, falling back to time.Now for hand-built servers.
func (s *Server) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

// logger 取装配注入的日志器；未注入时退回进程缺省。
// logger returns the injected logger, falling back to slog.Default.
func (s *Server) logger() *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	return slog.Default()
}
