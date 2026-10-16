package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// 本文件是半截工具组补齐的**分类唯一份**（docs/us/us4-react-loop.md §2.3 流程 E / 契约
// §4.4 表）：取消路径（循环内）与崩溃路径（启动恢复）共用同一张文本表——两处各写一份
// 必然漂移，而漂移的后果是"同一事件在两条路径下说法不同"，判卷与排查都会失准。
// This file is the single copy of half-group completion classification (flows E and G):
// the cancel path and the crash path share one text table on purpose.
//
// 五分支的**判据只看审计行有没有终态**（success / failed 是终态；pending 与 unknown
// 同判为"结果不可知"）——"执行过但终态未落"绝**不得**写成"未执行"：那会把一次可能已
// 产生外部副作用的调用说成没发生过（契约 §4.4 的诚实原则）。
// The five branches key on the presence of a *terminal* audit row only. A call that ran
// but never landed its terminal state must never be reported as "not executed".

// 补齐文本四段（逐字取自契约 §4.4；**不得**改写、不得拼接变量）。
// The four synthesized result texts, verbatim from contract §4.4.
const (
	// TextToolCancelled 是"取消时尚未执行"的合成结果。
	TextToolCancelled = "已取消（运行被取消，该调用未执行）"
	// TextToolBudget 是"预算耗尽未轮到"的合成结果。
	TextToolBudget = "未执行（工具调用次数超限）"
	// TextResultUnknown 是"崩溃时执行中（含执行完但终态未落）"的合成结果。
	TextResultUnknown = "结果未知（进程中断，无法确定是否已产生外部副作用）"
	// TextToolInterrupted 是"崩溃时尚未轮到"的合成结果。
	TextToolInterrupted = "未执行（进程中断）"
)

// 运行终态原因（挂 `runs.reason`；`CompleteToolGroup` 的 `reason` 参数取这四值之一）。
// Run termination reasons; the reason parameter of CompleteToolGroup is one of these.
const (
	// ReasonCancelled 是取消终止的原因。
	ReasonCancelled = "运行被取消"
	// ReasonToolBudget 是工具调用次数耗尽的终止原因。
	ReasonToolBudget = "工具调用次数超限"
	// ReasonIterationLimit 是迭代上限耗尽的终止原因。
	ReasonIterationLimit = "超过最大迭代次数"
	// ReasonRunFailed 是运行内部错误（消息写入失败等）的终止原因。
	ReasonRunFailed = "运行内部错误"
)

// finishTimeout 是收尾动作（补齐写消息、审计终态回填）的独立超时时长（契约 §4.2）。
// 调用时父 ctx 往往已取消，收尾写入**不得**依赖它；5 秒是"够写一条消息、又不拖住退出"的量。
// finishTimeout bounds finish-order writes; the parent context is usually already
// cancelled when they run, so they must not depend on it.
const finishTimeout = 5 * time.Second

// toolResultJSON 是 `tool_invocations.result_json` 列的形态：写入方在 `tool_exec.go`，
// 读回方在本文件（还原真实结果）——两方**共用**同一个结构，避免"写一种形状、读另一种"。
// 三个键各自回答一个问题：content 是什么、可否重试、是否被截断（截断标记另在 content 尾部）。
// toolResultJSON is the shape of the result_json column, shared by its writer (tool_exec.go)
// and its reader here.
type toolResultJSON struct {
	Content   string `json:"content"`
	Retryable bool   `json:"retryable,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// SynthesizeMissingResult 为一个没有结果消息的工具调用合成结果文本（五分支）。
// 顺序固定：**先查审计行、再按原因兜底**——反过来会把"执行过"的调用降级成"未执行"。
// SynthesizeMissingResult builds the result text for one tool call that has no result
// message. The order is fixed: the audit row is consulted first, the run reason only
// answers "no row at all".
//
// 返回 error 只表示"查审计失败"（仓储故障）——调用方记 ERROR 后仍写兜底文本
// （`TextResultUnknown`），**不得**因为查不动审计就把消息写漏：漏写一条 tool 结果
// 会让整段历史对供应商不可用（400），比"结果未知"的损失大得多。
// A non-nil error only means the audit lookup failed; callers log it and still write
// the fallback text.
func SynthesizeMissingResult(ctx context.Context, audit ToolInvocationReader, runID string, call ToolCall, reason string) (string, error) {
	if audit != nil {
		view, found, err := audit.Lookup(ctx, runID, call.ID)
		if err != nil {
			return "", fmt.Errorf("lookup tool invocation %s: %w", call.ID, err)
		}
		if found {
			switch view.Status {
			case LLMCallSuccess, LLMCallFailed:
				// 终态行 → 还原真实结果（复原失败也不编造：降级为"结果未知"并记账）。
				content, restoreErr := restoreResultContent(view.ResultJSON)
				if restoreErr != nil {
					return "", fmt.Errorf("restore result of tool invocation %s: %w", call.ID, restoreErr)
				}
				return content, nil
			case LLMCallPending, LLMCallUnknown:
				// 执行过但终态未落（或已被恢复扫描标 unknown）：如实说"结果未知"。
				// WARN 点名该行——终态回填曾失败是要有人看见的事（可观测性双轨）。
				// `session_id` 不在本层：冻结签名收不下它（调用方 `Session` 有，但分类在
				// `SynthesizeMissingResult` 内）——会话可从 `run_id` 反查，故不另传参。
				slog.Default().Warn("工具结果未知（终态审计行缺失）",
					"run_id", runID, "tool", call.Name, "reason", reason, "detail", string(view.Status))
				return TextResultUnknown, nil
			}
		}
	}
	switch reason {
	case ReasonCancelled:
		return TextToolCancelled, nil
	case ReasonToolBudget:
		return TextToolBudget, nil
	default:
		// 无行 = fail-closed 下"确定没执行"：取消与预算两条路径已在上面分辨，
		// 其余（恢复扫描的进程中断、消息追加失败等）统一归"未执行（进程中断）"。
		return TextToolInterrupted, nil
	}
}

// restoreResultContent 从审计行的 `result_json` 还原工具结果正文。
// restoreResultContent rebuilds the tool result content from the audit row's result_json.
func restoreResultContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		// 终态行却没有结果体（不该发生）：归"结果未知"分支由调用方兜底，不编造内容。
		return "", errNoResultBody
	}
	var shape toolResultJSON
	if err := json.Unmarshal(raw, &shape); err != nil {
		return "", fmt.Errorf("decode result_json: %w", err)
	}
	return shape.Content, nil
}

// errNoResultBody 表示终态审计行里没有可还原的结果体。
// errNoResultBody reports a terminal audit row whose result body is absent.
var errNoResultBody = errors.New("terminal tool invocation has no result body")

// TruncationMarker 生成输出截断标记（内容尾部追加，§6.2 固定口径 7）。
// N = **生效上限**——读取中限流意味着从未读完整个输出，故**不承诺**原始长度。
// TruncationMarker renders the truncation marker appended to truncated content; N is the
// effective cap, never the original length (streamed reads never see the whole output).
func TruncationMarker(maxBytes int) string {
	return fmt.Sprintf("\n[已截断：输出超过 %d 字节上限，此处不是完整输出]", maxBytes)
}
