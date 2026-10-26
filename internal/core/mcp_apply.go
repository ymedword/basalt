package core

// 本文件声明 MCP 诊断依赖轴与受控发布（apply）的冻结枚举（14D / 设计 §3.3）。
// 三组取值都写进 `docs/contracts/`：多一个、少一个或改字面量都属契约变更，
// 只能由契约先行，不能由实现顺手扩。
// This file declares the frozen enums of the MCP dependency axis and the controlled
// apply; every literal is contract-fixed and may only change contract-first.

import "errors"

// MCPDependencyState 是诊断「依赖」轴的取值：内置命令与已登记包按运行时目录判定。
// 空串表示该条目不适用（自定义命令没有可核对的目录项），**不是**枚举值——调用方
// 在视图里输出 `null`，不要把它当「就绪」。
// MCPDependencyState is the dependency axis of diagnostics; the empty string means
// "not applicable" and must be projected as null, never as ready.
type MCPDependencyState string

const (
	// MCPDependencyReady 表示依赖就位（可带版本）。
	MCPDependencyReady MCPDependencyState = "ready"
	// MCPDependencyMissing 表示依赖缺失。
	MCPDependencyMissing MCPDependencyState = "missing"
	// MCPDependencyVersionMismatch 表示依赖在位但版本不符。
	MCPDependencyVersionMismatch MCPDependencyState = "version_mismatch"
)

// Valid 报告该取值是否属于依赖轴闭集（空串是"不适用"，不算枚举值）。
// Valid reports membership in the closed set; the empty string is not a member.
func (s MCPDependencyState) Valid() bool {
	switch s {
	case MCPDependencyReady, MCPDependencyMissing, MCPDependencyVersionMismatch:
		return true
	default:
		return false
	}
}

// MCPApplyOutcome 是一次受控发布的结果（操作行 detail 的 `outcome`）。
// MCPApplyOutcome is the outcome field of one apply operation row.
type MCPApplyOutcome string

const (
	// MCPApplyApplied 表示新连接已切换并注册。
	MCPApplyApplied MCPApplyOutcome = "applied"
	// MCPApplySuperseded 表示期间被 publish / revoke / 再次 apply 取代，本次丢弃。
	MCPApplySuperseded MCPApplyOutcome = "superseded"
	// MCPApplyFailed 表示建连或预检失败，applied 不变。
	MCPApplyFailed MCPApplyOutcome = "failed"
)

// Valid 报告该取值是否属于 apply 结果闭集。
// Valid reports membership in the closed apply-outcome set.
func (o MCPApplyOutcome) Valid() bool {
	switch o {
	case MCPApplyApplied, MCPApplySuperseded, MCPApplyFailed:
		return true
	default:
		return false
	}
}

// MCPApplySupersededBy 说明本次 apply 被什么取代（操作行 detail 的 `superseded_by`）。
// 空串是合法取值——表示没有被取代；三个非空值分别对应并发发布、撤销与再次 apply。
// The empty value is legal and means "not superseded".
type MCPApplySupersededBy string

const (
	// MCPApplySupersededByNone 表示未被取代。
	MCPApplySupersededByNone MCPApplySupersededBy = ""
	// MCPApplySupersededByPublish 表示期间有更新的发布推进了指针。
	MCPApplySupersededByPublish MCPApplySupersededBy = "publish"
	// MCPApplySupersededByRevoke 表示期间冻结的某个凭据被撤销。
	MCPApplySupersededByRevoke MCPApplySupersededBy = "revoke"
	// MCPApplySupersededByApply 表示期间有另一次 apply 先完成切换。
	MCPApplySupersededByApply MCPApplySupersededBy = "apply"
)

// Valid 报告该取值是否属于取代者闭集（含空串）。
// Valid reports membership including the empty "not superseded" value.
func (b MCPApplySupersededBy) Valid() bool {
	switch b {
	case MCPApplySupersededByNone, MCPApplySupersededByPublish,
		MCPApplySupersededByRevoke, MCPApplySupersededByApply:
		return true
	default:
		return false
	}
}

// MCPApplyFailureDrainTimeout 是 apply 失败分类里唯一的新增值：旧连接排空窗口到点
// 仍有在途调用，本次切换按超时记失败。其余失败分类复用连接测试的既有枚举。
// MCPApplyFailureDrainTimeout is the one new failure class of apply; every other class
// reuses the connection-test enum.
const MCPApplyFailureDrainTimeout = "drain_timeout"

// MCPServerApplyResult 是受控发布对外的结果对象（契约 `McpServerApplyResult`）：成功
// （200）与 `superseded` / `failed` 共用同一形状，`server` 视图由 Web 层另挂。
// MCPServerApplyResult mirrors the contract; the server view is attached by the web layer.
type MCPServerApplyResult struct {
	Outcome             MCPApplyOutcome      `json:"outcome"`
	OperationID         string               `json:"operation_id"`
	TargetVersionID     int64                `json:"target_version_id"`
	PreviousVersionID   int64                `json:"previous_version_id"`
	AppliedVersionID    int64                `json:"applied_version_id"`
	DrainDeadline       string               `json:"drain_deadline"`
	DrainTimeoutSeconds int                  `json:"drain_timeout_seconds"`
	FailureClass        string               `json:"failure_class"`
	SupersededBy        MCPApplySupersededBy `json:"superseded_by"`
}

// AuditDetail 返回操作行 `detail_json` 的**固定 7 键集**：`target_version_id` /
// `previous_version_id` / `applied_version_id` / `outcome` / `failure_class` /
// `superseded_by` / `drain_deadline`。audit 契约逐字列出这 7 个键，比 openapi 的
// "结果对象去掉 `server`" 更具体，以 audit 契约为准——`operation_id`（本行主键）与
// `drain_timeout_seconds` **不**落在 detail 里。
// AuditDetail is the fixed seven-key set of the operation row; the audit contract is more
// specific than "result minus server" and wins.
func (r MCPServerApplyResult) AuditDetail() map[string]any {
	return map[string]any{
		"outcome":             string(r.Outcome),
		"target_version_id":   r.TargetVersionID,
		"previous_version_id": r.PreviousVersionID,
		"applied_version_id":  r.AppliedVersionID,
		"failure_class":       r.FailureClass,
		"superseded_by":       string(r.SupersededBy),
		"drain_deadline":      r.DrainDeadline,
	}
}

// ErrMCPApplySuperseded / ErrMCPApplyFailed 是受控发布两条失败出口的哨兵：Web 层据
// 它们分别映射 409 `MCP_APPLY_SUPERSEDED` 与 500 `MCP_APPLY_FAILED`。载体统一用
// `AdminError`（Kind = 哨兵，Data 带 `operation_id`）——不另立一套错误类型，避免
// "结果对象说被取代、错误对象说失败"的自相矛盾出口。策略拒绝（403）复用
// `ErrPolicyDenied`，同样不另立哨兵。
// The two sentinels behind 409 and 500; both ride on AdminError, and a policy denial
// reuses ErrPolicyDenied.
var (
	// ErrMCPApplySuperseded 表示本次切换被并发写取代。
	ErrMCPApplySuperseded = errors.New("MCP apply superseded")
	// ErrMCPApplyFailed 表示切换失败，`applied` 不变。
	ErrMCPApplyFailed = errors.New("MCP apply failed")
)
