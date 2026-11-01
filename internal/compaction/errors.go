package compaction

import "errors"

// 本文件是压缩引擎的**分类错误**（设计 §3.1.11）：降级路径按错误种类挑 `reason_code`，
// 而不是按错误文本猜。七个错误各有唯一的后续动作，**不得**合并成一个 "compaction failed"。
// Classified sentinel errors: the degradation path maps each to one reason code, and each
// error implies exactly one follow-up action.
var (
	// ErrNoValidCut 表示找不到合法的完整工具组切点（剩余内容全在保留组内，或
	// `keep_recent_groups` 未接线）。降级，**不得**退化成"压缩全部"。
	// ErrNoValidCut means there is no whole-group cut point to compact at.
	ErrNoValidCut = errors.New("compaction: 找不到合法的完整工具组切点")

	// ErrSummaryTimeout 表示摘要调用超时。降级；**不得**使用半成品摘要。
	// ErrSummaryTimeout means the summary call timed out.
	ErrSummaryTimeout = errors.New("compaction: 摘要调用超时")

	// ErrSummaryFailed 表示摘要调用失败（供应商错误 / 解析失败 / 取消之外的失败）。
	// 降级；**不得**把失败摘要当空串继续。
	// ErrSummaryFailed means the summary call failed.
	ErrSummaryFailed = errors.New("compaction: 摘要调用失败")

	// ErrSummaryOverLimit 表示摘要输出超过 `summary_max_tokens`。**不得**静默截断
	// （截断会丢掉"硬约束 / 未决项"两节），按失败降级。
	// ErrSummaryOverLimit means the summary exceeded its own cap; it is never truncated.
	ErrSummaryOverLimit = errors.New("compaction: 摘要输出超过 summary_max_tokens")

	// ErrScopeInvalid 表示压缩请求未接线或范围非法：`History` / `Budget` / `Fixed` 缺失、
	// 策略数值缺失、两种范围同时为空或同时给出。**无默认值**——不猜就是拒绝。
	// ErrScopeInvalid means the request is unwired or the scope is malformed.
	ErrScopeInvalid = errors.New("compaction: 压缩请求未接线或范围非法")

	// ErrCompactionPending 表示条目结局**不可判定**（`AppendPending` / `AppendInconsistent`）：
	// 冻结前滚、告警，**不重试**。**不得**降级成"需要压缩"再试一次。
	// ErrCompactionPending means the append outcome is undecidable; freeze and never retry.
	ErrCompactionPending = errors.New("compaction: 条目结局不可判定（冻结前滚，不重试）")

	// ErrCompactionStale 表示本次尝试依赖的链已失效（被超越且重算后仍不合格）。
	// 降级；**不得**把旧摘要重提到新范围上。
	// ErrCompactionStale means the chain this attempt relied on went stale.
	ErrCompactionStale = errors.New("compaction: 压缩链已失效")
)

// 降级原因码（设计 §3.1.11 / §2.10.3 失败矩阵冻结的五个，外加重建失败的唯一一个）。
// reason_code 进 `context_assembly_records.compression` 与诊断；**不得**自造第六个之外的取值。
// The frozen degradation reason codes.
const (
	// ReasonCompactionTimeout 对应 `ErrSummaryTimeout`。
	ReasonCompactionTimeout = "compaction_timeout"
	// ReasonCompactionFailed 对应 `ErrSummaryFailed` / `ErrNoValidCut`（切点不可用视作失败）。
	ReasonCompactionFailed = "compaction_failed"
	// ReasonCompactionOverLimit 对应 `ErrSummaryOverLimit`，以及"单个工具组本身超限"。
	ReasonCompactionOverLimit = "compaction_over_limit"
	// ReasonAppendPending 对应 `ErrCompactionPending`。
	ReasonAppendPending = "append_pending"
	// ReasonStale 对应 `ErrCompactionStale`。
	ReasonStale = "stale"
	// ReasonNoInjectableChain 表示重建后仍不可注入：本 Run 余下轮次**不再调用摘要**。
	// ReasonNoInjectableChain stops further regeneration for the rest of the run.
	ReasonNoInjectableChain = "no_injectable_chain"
)
