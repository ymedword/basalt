package web

// 本文件是诊断读取端点的**响应投影**（US-16 / T097）：形状逐字段取自冻结契约
// `docs/contracts/openapi.yaml` 的 `DiagnosticEventView` / `DiagnosticsResult`。
//
// 四条**投影级**约束：
//
//  1. **`correlation` 五个键恒出站**：契约把 request_id / run_id / task_id /
//     operation_id / ingest_job_id 全部列为必填；空串表示"这条事件没有这个锚点"，
//     省掉键会让面板分不清"没有"与"没给"。
//  2. **`occurred_at` 保留毫秒**：列存 Unix 毫秒，投影按同一精度渲染（`...T..:..:..Z`
//     的固定毫秒后缀），四舍五入到秒会让同一秒内的两条事件看起来同时发生。
//  3. **`missing` 与 `sampled_out` 互斥**：无行且采样率 < 1 → `sampled_out=true`；
//     无行且全量采样 → `missing=true`。两者**不得**同时出现，也不得用空 `events`
//     冒充"一切正常"。
//  4. **`attrs` 恒为对象**：`nil` 投影成 `{}`，因为 `jsonb` 列本身不允许 NULL。
//
// This file projects the diagnostics result. Every correlation key ships, occurred_at
// keeps its millisecond precision, and missing / sampled_out stay mutually exclusive.

import (
	"time"

	"github.com/ymedword/basalt/internal/diagnostics"
)

// diagnosticOccurredAtLayout 是 `occurred_at` 的渲染格式：RFC3339 + 固定三位毫秒
// （`2006-01-02T15:04:05.000Z07:00`）。毫秒是列的真实精度，截到秒会丢信息。
// The render layout keeps the column's millisecond precision.
const diagnosticOccurredAtLayout = "2006-01-02T15:04:05.000Z07:00"

// DiagnosticCorrelationView 是 `correlation` 子对象：五个键恒出站。
// DiagnosticCorrelationView mirrors the contract's correlation object.
type DiagnosticCorrelationView struct {
	RequestID   string `json:"request_id"`
	RunID       string `json:"run_id"`
	TaskID      string `json:"task_id"`
	OperationID string `json:"operation_id"`
	IngestJobID string `json:"ingest_job_id"`
}

// DiagnosticAuditRefView 是 `audit_ref` 子对象：只引用审计行，不替代审计。
// DiagnosticAuditRefView points at one audit row; it never stands in for audit.
type DiagnosticAuditRefView struct {
	Table string `json:"table"`
	ID    string `json:"id"`
}

// DiagnosticEventView 是 `DiagnosticEventView`：一条诊断事件的只读形态。
// 字段顺序与契约一致，便于逐行对照。
// DiagnosticEventView mirrors the contract's event view; field order follows the schema.
type DiagnosticEventView struct {
	EventID       string                    `json:"event_id"`
	SchemaVersion int                       `json:"schema_version"`
	Kind          string                    `json:"kind"`
	OccurredAt    string                    `json:"occurred_at"`
	Correlation   DiagnosticCorrelationView `json:"correlation"`
	Actor         string                    `json:"actor"`
	StepID        string                    `json:"step_id"`
	AttemptID     string                    `json:"attempt_id"`
	ParentStepID  string                    `json:"parent_step_id"`
	Seq           int64                     `json:"seq"`
	ReasonCode    string                    `json:"reason_code"`
	Decision      string                    `json:"decision"`
	Attrs         map[string]any            `json:"attrs"`
	AuditRef      *DiagnosticAuditRefView   `json:"audit_ref,omitempty"`
}

// DiagnosticsResult 是 `DiagnosticsResult`：一页事件 + 证据缺口标记。
// DiagnosticsResult is one page of events plus the evidence-gap marker.
type DiagnosticsResult struct {
	Events  []DiagnosticEventView `json:"events"`
	Missing bool                  `json:"missing"`
	// SampledOut 只在**采样率 < 1 且本页为空**时为 true：此时无法区分"本来没有"与
	// "被采样丢光"（被丢的事件不落库、不留痕，服务端无法断言记录存在）。全量采样下
	// 空页用 `missing` 表达缺口；`omitempty` 让 false 不出站，两者的互斥由构造保证
	// （见 handler）。
	// SampledOut is true only for an empty page under rate < 1; a full-rate empty page
	// reports missing instead, and the handler guarantees the two are exclusive.
	SampledOut bool `json:"sampled_out,omitempty"`
	// RetentionDays 是本实例的**配置值**，不是保留承诺（契约逐字）。
	// RetentionDays is the configured value, never a retention promise.
	RetentionDays int `json:"retention_days"`
}

// diagnosticEventViews 投影一页事件：次序由存储层固定，本层不重排、不截断。
// diagnosticEventViews projects the page verbatim; the store already fixed the order.
func diagnosticEventViews(events []diagnostics.DiagnosticEvent) []DiagnosticEventView {
	views := make([]DiagnosticEventView, 0, len(events))
	for _, event := range events {
		attrs := event.Attrs
		if attrs == nil {
			attrs = map[string]any{}
		}
		view := DiagnosticEventView{
			EventID:       event.EventID,
			SchemaVersion: event.SchemaVersion,
			Kind:          string(event.Kind),
			OccurredAt:    time.UnixMilli(event.OccurredAt).UTC().Format(diagnosticOccurredAtLayout),
			Correlation: DiagnosticCorrelationView{
				RequestID:   event.Correlation.RequestID,
				RunID:       event.Correlation.RunID,
				TaskID:      event.Correlation.TaskID,
				OperationID: event.Correlation.OperationID,
				IngestJobID: event.Correlation.IngestJobID,
			},
			Actor:        string(event.Actor),
			StepID:       event.StepID,
			AttemptID:    event.AttemptID,
			ParentStepID: event.ParentStepID,
			Seq:          event.Seq,
			ReasonCode:   string(event.ReasonCode),
			Decision:     string(event.Decision),
			Attrs:        attrs,
		}
		if event.AuditRef != nil {
			view.AuditRef = &DiagnosticAuditRefView{Table: event.AuditRef.Table, ID: event.AuditRef.ID}
		}
		views = append(views, view)
	}
	return views
}
