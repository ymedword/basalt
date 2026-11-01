package compaction

import (
	"context"
	"fmt"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是**降级回退时的来源闸门**（T087 / C-26）：压缩失败后仍要把原始历史交给模型，
// 但那不等于"绕过失效规则"——回退路径与压缩投影走**同一处**校验、同一套占位符，
// 否则失效来源会顺着降级路径漏给模型。
// The injection-time source gate used on the degraded path: falling back to raw history
// must not bypass the validity rules, so it reuses the very same check and placeholder.

// SourceGate 是 `core.SourceFilter` 的实现：逐条重查来源，失效消息**整条**换占位符；
// 工具组里只要有一条失效就**整组丢弃**——拆组会让 `assistant(tool_calls)` 与结果失配，
// 供应商直接拒收，等于是把"少一条消息"变成"整轮请求失败"。
// SourceGate implements core.SourceFilter: a stale message is replaced whole, and a tool
// group containing one stale member is dropped whole rather than split.
type SourceGate struct {
	Validator ValidityChecker
}

// Filter 实现 `core.SourceFilter`。**没有**"全部通过就原样返回同一个切片"的捷径：
// 返回值总是新切片，调用方改动它不会回头改到存储层读出的对象。
// Filter returns a fresh slice on every call; no aliasing back to the caller's input.
func (g SourceGate) Filter(ctx context.Context, p core.Principal, msgs []core.Message) ([]core.Message, error) {
	if !p.Valid() {
		// 身份零值：fail-closed，不产出 prompt（也不"顺手全放行"）。
		return nil, core.ErrUnauthorized
	}
	if g.Validator == nil {
		return nil, fmt.Errorf("%w: SourceGate 未装配 ValidityChecker", ErrScopeInvalid)
	}
	out := make([]core.Message, 0, len(msgs))
	for i := 0; i < len(msgs); {
		groupEnd, stale := g.groupEnd(ctx, p, msgs, i)
		if groupEnd-i > 1 && stale {
			// 工具组不完整：整组丢弃，**不**拆组、**不**保留半条。
			i = groupEnd
			continue
		}
		for j := i; j < groupEnd; j++ {
			m := msgs[j]
			if stale {
				m.Content = invalidMessagePlaceholder
				m.ReasoningContent = ""
			}
			out = append(out, m)
		}
		i = groupEnd
	}
	return out, nil
}

// groupEnd 返回从 `i` 起的工具组结束下标（不含），并报告组内是否**任一**消息失效。
// 非工具消息自成一组（`groupEnd == i+1`）。`ToolCallID` 匹配失败即结束当前组——
// 不猜归属，与 `AssignToolGroups` 同一口径。
// groupEnd finds the tool group starting at i and reports whether any member went stale.
func (g SourceGate) groupEnd(ctx context.Context, p core.Principal, msgs []core.Message, i int) (int, bool) {
	ok, _, err := checkRefs(ctx, g.Validator, p, msgs[i].EvidenceRefs)
	if err != nil {
		// 校验器故障按失效处理（fail-closed）：宁可少一段历史，也不放行未校验的来源。
		ok = false
	}
	stale := !ok
	end := i + 1
	if msgs[i].Role != core.RoleAssistant || len(msgs[i].ToolCalls) == 0 {
		return end, stale
	}
	want := make(map[string]struct{}, len(msgs[i].ToolCalls))
	for _, tc := range msgs[i].ToolCalls {
		want[tc.ID] = struct{}{}
	}
	for end < len(msgs) && msgs[end].Role == core.RoleTool {
		if _, ok := want[msgs[end].ToolCallID]; !ok {
			break
		}
		memberOK, _, memberErr := checkRefs(ctx, g.Validator, p, msgs[end].EvidenceRefs)
		if memberErr != nil {
			memberOK = false
		}
		if !memberOK {
			stale = true
		}
		delete(want, msgs[end].ToolCallID)
		end++
	}
	return end, stale
}
