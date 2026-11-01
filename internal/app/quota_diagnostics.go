package app

// 本文件是**配额判定**诊断事件（`quota_decision`）的公共落点（V-8）。五个签发方
// （`chatQuota` / `runAdmission` / `runQuota`（Run 级任务次数，N-01）/ 摘要调用闸门 /
// 入口速率门）各自知道锚点与判定，翻译成契约事件的规则只此一份——另写一套会让同一个
// 哨兵在两条路径上映射出不同原因码。
//
// 三条口径：
//   - **判定与原因码分开**：`decision` 回答"放行还是拒绝"，`reason_code` 回答"为什么
//     拒绝"；`allow` 永远不带原因码（带着会让"原因"被读成"发生过的坏事"）；
//   - **让出的约定**：`errors.Is` 只认 `callers` 的三条哨兵（配额耗尽 / 并发名额 /
//     速率）；其余错误按"无法归因"留空原因码——诊断不猜，排障时结合日志定位；
//   - **没有锚点不发**：入口类判定若拿不到 `run_id`（请求还没变成 Run），可退到
//     `request_id`（A-F2 / C-F02）：该锚点**只记拒绝**——放行事件由一个 Run 自己的
//     时间线承载，再记一条会让同一个准入在诊断面出现两次；拒绝则没有别的痕迹。
//
// This file holds the one mapping from quota denials to contract reason codes, shared by
// every quota producer; a second copy would let one sentinel mean two different things.

import (
	"context"
	"errors"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// quotaDecision 是一次配额判定的最小事实：锚点、类目与判定结果。
// quotaDecision is the minimal fact of one quota verdict.
type quotaDecision struct {
	// RunID 是关联主锚点；与 RequestID 都为空时**不**落库（没有锚点的事件不发）。
	RunID string
	// RequestID 是退路锚点：Run 准入在账本创建之前被拒时还没有 run_id，管理端
	// 只能按 request_id 定位（A-F2 / C-F02）。该锚点**只记拒绝**。
	// RequestID is the fallback anchor for denials that happen before a run exists;
	// it never carries an allow event.
	RequestID string
	// StepID / Seq 取轮次口径（与 `llm_call` / `tool_call` 同一个序号空间）。
	StepID string
	Seq    int64
	// Class 是配额类目：`run`（并发名额）/ `run_task`（Run 级任务次数）/ `chat` /
	// `compaction`（入口与摄取不在此文件）。`attrs.class` 在契约里是自由字段，取值
	// 由签发方各自固定——同一个哨兵说明"额度不够"，但不够的是哪一条额度要看类目。
	Class string
	// Err 是拒绝原因；nil 表示放行。
	Err error
}

// quotaDenyReason 把拒绝翻译成契约原因码。未知错误返回空串——契约允许 `reason_code`
// 缺省，胡乱归因比不归因更糟（会把排障引到错误方向）。
// quotaDenyReason maps the three caller sentinels; anything else stays unattributed.
func quotaDenyReason(err error) diagnostics.ReasonCode {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, callers.ErrQuotaExhausted):
		return diagnostics.ReasonQuotaExhausted
	case errors.Is(err, callers.ErrConcurrencyLimited):
		return diagnostics.ReasonConcurrencyLimited
	case errors.Is(err, callers.ErrRateLimited):
		return diagnostics.ReasonRateLimited
	default:
		return ""
	}
}

// recordQuotaDecision 写一条 `quota_decision` 事件。`recorder` 为 nil（未接诊断）或
// 锚点为空时静默跳过：诊断可丢弃，且**不得**为了凑关联伪造锚点（§2.9）。
// recordQuotaDecision records one quota_decision event, silently skipped without an anchor.
func recordQuotaDecision(ctx context.Context, recorder diagnosticRecorder, d quotaDecision) {
	if recorder == nil {
		return
	}
	// 锚点优先级 RunID > RequestID；按 request_id 锚定的事件只记拒绝——放行由 Run
	// 自己的时间线承载，重复记录会让同一次准入在诊断面出现两次。
	// Anchor priority is RunID then RequestID; request-anchored events are denials only.
	correlation := diagnostics.Correlation{RunID: d.RunID}
	if d.RunID == "" {
		if d.RequestID == "" || d.Err == nil {
			return
		}
		correlation = diagnostics.Correlation{RequestID: d.RequestID}
	}
	ev := diagnostics.DiagnosticEvent{
		Kind:        diagnostics.KindQuotaDecision,
		Correlation: correlation,
		StepID:      d.StepID,
		Seq:         d.Seq,
		Attrs:       map[string]any{"class": d.Class},
	}
	if d.Err != nil {
		ev.Decision = diagnostics.DecisionDeny
		ev.ReasonCode = quotaDenyReason(d.Err)
	} else {
		ev.Decision = diagnostics.DecisionAllow
	}
	recorder.Record(ctx, ev)
}
