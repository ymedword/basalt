package app

// 本文件把 `core.ToolDiagnosticRecorder` 接到诊断写入器上（V-8；颗粒度文档 §2.9 的
// `tool_call` / `permission_decision` 两种 kind）。与 `knowledge_adapters.go` 同一套
// 纪律：`core` 只说"发生了什么"，字面量、判定与审计引用的翻译只在这里发生。
//
// 两条边界：
//   - **只翻译、不补事实**：`audit_ref` 只在执行器真的拿到审计行时出现（`AuditID` 非 0）；
//     解析失败与工具集拒绝没有行，就**不**写引用——伪造引用比没有引用更坏；
//   - **不替代审计**：工具集拒绝仍只有 WARN、不发审计行（执行从未启动），诊断只是让
//     这类尝试在排障面上可见；一次真实执行的权威记录永远是 `tool_invocations` 那一行。
//
// This adapter translates core's tool diagnostics into frozen contract events. It never
// invents an audit reference and never stands in for the audit row itself.

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// toolDiagAdapter 实现 `core.ToolDiagnosticRecorder`（消费方端口）。
// toolDiagAdapter implements the core-declared port.
type toolDiagAdapter struct {
	recorder diagnosticRecorder
	logger   *slog.Logger
}

var _ core.ToolDiagnosticRecorder = (*toolDiagAdapter)(nil)

// newToolDiagAdapter 构造适配器；`recorder` 为 nil 时原样返回 nil——执行器据此短路，
// 未接诊断的实例与接线前逐字一致。日志器只用于未知形态告警。
// newToolDiagAdapter returns nil when there is no recorder, so the executor short-circuits.
func newToolDiagAdapter(recorder diagnosticRecorder, logger *slog.Logger) core.ToolDiagnosticRecorder {
	if recorder == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &toolDiagAdapter{recorder: recorder, logger: logger}
}

// RecordToolDiagnostic 把一次工具诊断翻译成契约事件。未知形态只 WARN 丢弃（与知识面
// 适配器同一口径：诊断可丢弃，一个不该出现的形态不得阻断运行）。
// RecordToolDiagnostic translates one tool diagnostic; an unknown shape is dropped with a
// warning because diagnostics never block a run.
func (a *toolDiagAdapter) RecordToolDiagnostic(ctx context.Context, ev core.ToolDiagnostic) {
	if a == nil || a.recorder == nil {
		return
	}
	if ev.RunID == "" {
		// 没有主锚点就不发（§2.9）：诊断不伪造关联。
		return
	}
	out := diagnostics.DiagnosticEvent{
		Correlation: diagnostics.Correlation{RunID: ev.RunID},
		StepID:      ev.StepID,
		Seq:         ev.Seq,
		Attrs:       map[string]any{"tool": ev.Tool},
	}
	if ev.ToolCallID != "" {
		out.Attrs["tool_call_id"] = ev.ToolCallID
	}
	switch ev.Kind {
	case core.ToolDiagnosticCall:
		out.Kind = diagnostics.KindToolCall
		if ev.ErrorKind != "" {
			out.Attrs["error_kind"] = string(ev.ErrorKind)
		}
		if ev.AuditID != 0 {
			out.AuditRef = &diagnostics.AuditRef{
				Table: "tool_invocations",
				ID:    strconv.FormatInt(ev.AuditID, 10),
			}
		}
	case core.ToolDiagnosticDenied:
		out.Kind = diagnostics.KindPermissionDecision
		out.Decision = diagnostics.DecisionDeny
		out.ReasonCode = diagnostics.ReasonPermissionDenied
	default:
		a.logger.WarnContext(ctx, "工具诊断：未知形态，事件丢弃", "kind", string(ev.Kind))
		return
	}
	a.recorder.Record(ctx, out)
}
