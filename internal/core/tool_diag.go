package core

// 本文件声明工具执行诊断的**消费方窄端口**（V-8 补生产端；颗粒度文档 §2.9 的
// `tool_call` / `permission_decision` 两种 kind）。两条纪律与装配层的模型调用装饰器同源：
//
//   - `core` 只依赖标准库（宪法「架构约束」），**不得**为发诊断 import
//     `internal/diagnostics`；这一层只说"发生了什么"，种类字面量与脱敏由 `app` 的
//     适配器翻译成冻结契约事件；
//   - `decision` 只在**真的有一次判定**时给出：工具集之外的拒绝是 `permission_decision`
//     + deny；一次执行（无论成败）是 `tool_call`，`decision` 留空——工具报错不是策略判定。
//
// 端口可空：执行器没接诊断时逐字保持接线前的行为（不分配、不调用）。
// This file declares the consumer-side tool diagnostics port. core never imports the
// diagnostics package: it names what happened, and the app adapter translates kinds,
// decisions and redaction into the frozen event contract.

import "context"

// ToolDiagnosticKind 是工具诊断的两种形态：一次真实执行，或一次**未执行**的权限拒绝。
// ToolDiagnosticKind distinguishes an execution from a refusal that never executed.
type ToolDiagnosticKind string

const (
	// ToolDiagnosticCall 对应契约 kind `tool_call`：工具被启动（或解析失败、入参被拒），
	// 携带审计行标识与失败分类。解析 / 入参两类拒绝没有审计行，`AuditID` 为 0。
	ToolDiagnosticCall ToolDiagnosticKind = "call"
	// ToolDiagnosticDenied 对应契约 kind `permission_decision`：工具不在本 Agent 声明的
	// 工具集内，调用**未启动**、没有审计行（审计行只记真实执行）。
	ToolDiagnosticDenied ToolDiagnosticKind = "denied"
)

// ToolDiagnostic 是一次工具调用的诊断事实。字段只放**定位一次调用**所需的信息：
// 工具名与失败分类是标识，参数、入参正文与工具结果**不得**进入本结构（脱敏边界在
// 适配器与写入器，但源头不给才是第一道闸）。
// ToolDiagnostic carries only what locates one call: the tool name and the failure class.
// Arguments, inputs and results never enter this struct.
type ToolDiagnostic struct {
	Kind ToolDiagnosticKind
	// RunID 是关联主锚点；空串表示这次调用不属于任何 Run（非 Run 直调），此时执行器
	// **不**发出事件——诊断不得为了凑关联伪造锚点。
	RunID string
	// StepID / Seq 由循环侧给出：StepID 取轮次（与装配记录、配额幂等键同一口径），
	// Seq 同一轮内同值（契约只要求"同 correlation 内局部有序"）。
	StepID string
	Seq    int64
	Tool   string
	// ToolCallID 来自模型实际返回的调用标识；没有标识时保持空。
	// ToolCallID preserves the real model-provided call identity.
	ToolCallID string
	// ErrorKind 是失败分类（八类字面量之一）；成功时留空。
	ErrorKind ErrorKind
	// AuditID 是 `tool_invocations` 的行标识（预写成功才有）；0 表示没有审计行。
	AuditID int64
}

// ToolStep 是循环侧给出的步骤定位：StepID 取轮次（与装配记录、配额幂等键
// `run:<id>:chat:<round>:a1` 同一口径），Seq 与 StepID 同值（同一轮内同一次判定）。
// 零值表示"调用方没提供定位"（非循环直调），此时事件的 step_id / seq 留空 / 0。
// ToolStep is the loop-side locator; the zero value means the caller had none.
type ToolStep struct {
	StepID string
	Seq    int64
}

// ToolDiagnosticRecorder 是诊断写入的消费方端口（`app` 的适配器实现）。
// ToolDiagnosticRecorder is the write port implemented by the app adapter.
type ToolDiagnosticRecorder interface {
	RecordToolDiagnostic(ctx context.Context, ev ToolDiagnostic)
}
