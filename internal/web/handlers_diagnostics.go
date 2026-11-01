package web

// 本文件是 `GET /api/v1/diagnostics` 的 HTTP 层（US-16 / T097）：诊断事件的**只读**
// 查询面。判据逐条取自冻结契约 `docs/contracts/openapi.yaml`：
//
//  1. **管理面只读**：根 security 只有 `bearerAuth` / `localTrust`，没有 `callerKey`
//     ——本行是管理行（`adminHandler`），不声明 scope、不映射 ingress。读取面**不写**
//     管理操作审计（run-and-errors §9.3）。
//  2. **至少一个主锚点**：`run_id` / `request_id` / `operation_id` / `ingest_job_id` /
//     `session_id` 至少给一个（`request_id` 为 A-F2 / C-F02 补入，无 Run 的准入拒绝按
//     它定位）；`task_id` 只作限定符，**单独给出 → 400**。形状非法在触达端口之前
//     返回，不去读库（契约：不允许无条件全表扫）。
//  3. **`limit` 边界**：minimum 1 / maximum 1000 / default 200；越界或非整数 → 400。
//  4. **装配缺失按失败关闭**：端口 nil 固定 500 + ERROR 日志，**不得**回 200 + 空集
//     ——空集在契约里带 `missing=true`，是有含义的读数。
//  5. **取消不算失败**：`context.Canceled` 只记 WARN、不写响应（连接已断）。
//  6. **`missing` 与 `sampled_out` 互斥**：无行时按采样口径二选一（R23 裁决 4）——
//     被采样丢弃的事件不落库、不留痕，因此"有行才说得出有没有"。
//
// This file is the HTTP layer of the read-only diagnostics endpoint: management surface,
// one anchor enforced before the port, contract limit bounds, and a nil port failing
// closed instead of faking "no records".

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// 契约 `GET /diagnostics` 的 `limit` 边界（minimum 1 / maximum 1000 / default 200）。
// web 层复刻这三项只为在触达端口之前 400 / 补默认；存储侧再判一次上界（失败关闭），
// 两处取值以契约为准。
// The contract's limit bounds; the store re-checks them as a second gate.
const (
	defaultDiagnosticLimit = 200
	maxDiagnosticLimit     = diagnostics.MaxQueryLimit
)

// 诊断视图固定文案（契约 §8：400 只说明"请求形状非法"，不回显内部规则细节）。
// Fixed diagnostics copy; a shape denial never explains internal rules.
const (
	messageDiagnosticAnchorRequired = "必须给出至少一个关联标识：run_id / request_id / operation_id / ingest_job_id / session_id"
	messageDiagnosticLimitRange     = "limit 必须是 1 到 1000 之间的整数"
	messageDiagnosticKindUnknown    = "kind 不是契约内的事件种类"
	messageDiagnosticTimeRange      = "from / to 必须是 RFC3339 时间且 from 不晚于 to"
	// `request_id` 的形状拒绝固定文案：**不回显**用户给的值（契约 §8）。
	// The shape denial for request_id never echoes the submitted value.
	messageDiagnosticRequestIDInvalid = "request_id 只允许 1 到 64 位的字母、数字与 . _ : -"
)

// handleListDiagnostics 处理 `GET /api/v1/diagnostics`：按关联标识返回一页事件
// （`occurred_at` 升序），并显式表达证据缺口。次序与截断由存储层固定，本层不重排。
// handleListDiagnostics reads one page in store order and reports the evidence gap
// instead of presenting an empty page as "all good".
func (s *Server) handleListDiagnostics(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.Diagnostics == nil {
		s.writeDiagnosticsPortMissing(w, "GET /diagnostics")
		return
	}
	query, ok := s.parseDiagnosticQuery(w, r)
	if !ok {
		return
	}
	events, err := s.opts.Diagnostics.List(r.Context(), query)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			s.logger().Warn("读取诊断事件已取消", "actor", actor.Subject)
			return
		case errors.Is(err, diagnostics.ErrQueryWithoutAnchor), errors.Is(err, diagnostics.ErrQueryLimit):
			// 形状已在触达端口之前判过；走到这里说明端口与 handler 口径分叉，
			// 按请求形状错误返回而不是 500——错的是这次请求的形状，不是服务端。
			s.writeError(w, http.StatusBadRequest, codeBadRequest, messageDiagnosticAnchorRequired)
			return
		}
		s.logger().Error("读取诊断事件失败", "actor", actor.Subject, "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	config := s.opts.Diagnostics.Config()
	result := DiagnosticsResult{
		Events:        diagnosticEventViews(events),
		RetentionDays: config.RetentionDays,
	}
	if len(events) == 0 {
		// 采样率 < 1 时"无行"无法区分"本来没有"与"丢光了"：只有全量采样才敢说缺失。
		// Below rate 1 an empty page cannot tell "none" from "all dropped", so only a
		// full-rate instance may claim missing.
		if config.SampleRate < diagnostics.DefaultSampleRate {
			result.SampledOut = true
		} else {
			result.Missing = true
		}
	}
	s.writeOK(w, result)
}

// parseDiagnosticQuery 解析并校验查询参数；返回 false 表示已写出 400。
// parseDiagnosticQuery validates the query string and writes the 400 itself.
func (s *Server) parseDiagnosticQuery(w http.ResponseWriter, r *http.Request) (diagnostics.Query, bool) {
	params := r.URL.Query()
	query := diagnostics.Query{
		RunID:       params.Get("run_id"),
		TaskID:      params.Get("task_id"),
		OperationID: params.Get("operation_id"),
		IngestJobID: params.Get("ingest_job_id"),
		SessionID:   params.Get("session_id"),
	}
	// `request_id` 是服务端生成的追踪标识（A-F2 / C-F02）：Run 准入在 Run 账本创建
	// 之前被拒时还没有 run_id，管理端只能按它定位。形状只限字符集与长度（1..64）——
	// 生成侧在随机源失败时退化成字面量 "unknown"，按 16 位十六进制死判会拒掉合法值；
	// 非法值**不回显**，也不触达端口。
	// The server-generated tracing id is validated by charset and length only: the
	// generator falls back to the literal "unknown", so a 16-hex rule would reject it.
	if raw := params.Get("request_id"); raw != "" {
		if !validDiagnosticRequestID(raw) {
			s.writeError(w, http.StatusBadRequest, codeBadRequest, messageDiagnosticRequestIDInvalid)
			return diagnostics.Query{}, false
		}
		query.RequestID = raw
	}
	// `task_id` 不在锚点集合内：调度任务的事件由 `task_id + request_id` 或
	// `task_id + run_id` 共同定位，单给 task_id 等于按任务号全表扫。
	// TaskID is a qualifier, never an anchor on its own.
	if query.RunID == "" && query.RequestID == "" && query.OperationID == "" &&
		query.IngestJobID == "" && query.SessionID == "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageDiagnosticAnchorRequired)
		return diagnostics.Query{}, false
	}
	if raw := params.Get("kind"); raw != "" {
		kind := diagnostics.Kind(raw)
		if !kind.Valid() {
			s.writeError(w, http.StatusBadRequest, codeBadRequest, messageDiagnosticKindUnknown)
			return diagnostics.Query{}, false
		}
		query.Kind = kind
	}
	from, ok := s.parseDiagnosticTime(w, params.Get("from"))
	if !ok {
		return diagnostics.Query{}, false
	}
	to, ok := s.parseDiagnosticTime(w, params.Get("to"))
	if !ok {
		return diagnostics.Query{}, false
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageDiagnosticTimeRange)
		return diagnostics.Query{}, false
	}
	query.From, query.To = from, to
	query.Limit = defaultDiagnosticLimit
	if raw := params.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxDiagnosticLimit {
			s.writeError(w, http.StatusBadRequest, codeBadRequest, messageDiagnosticLimitRange)
			return diagnostics.Query{}, false
		}
		query.Limit = parsed
	}
	return query, true
}

// validDiagnosticRequestID 判 `request_id` 的请求形状：1..64 位、字符集
// `[A-Za-z0-9._:-]`。生成侧是 16 位小写十六进制，随机源失败时退化为字面量
// "unknown"——两者都必须通过，因此**不**按固定长度判卷。
// validDiagnosticRequestID accepts the 16-hex form and the "unknown" fallback alike;
// length is bounded but never pinned to 16.
func validDiagnosticRequestID(raw string) bool {
	if raw == "" || len(raw) > 64 {
		return false
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == ':', r == '-':
		default:
			return false
		}
	}
	return true
}

// parseDiagnosticTime 解析一个可空时间参数：空串返回零值（该侧不限）。
// parseDiagnosticTime parses an optional RFC3339 bound; an empty string leaves it open.
func (s *Server) parseDiagnosticTime(w http.ResponseWriter, raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, true
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageDiagnosticTimeRange)
		return time.Time{}, false
	}
	return parsed, true
}

// writeDiagnosticsPortMissing 是装配缺失的失败关闭出口：ERROR 日志 + 固定 500。
// 与 `writeCompactionPortMissing` 同一口径，端口名进日志、不进响应体。
// writeDiagnosticsPortMissing fails closed with a fixed 500 and names the port in the log.
func (s *Server) writeDiagnosticsPortMissing(w http.ResponseWriter, endpoint string) {
	s.logger().Error("装配缺少诊断读取端口", "port", "Diagnostics", "endpoint", endpoint)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}
