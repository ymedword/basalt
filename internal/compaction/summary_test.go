package compaction_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件覆盖摘要调用（T085）、输出校验与降级闸门（T087 / C-25 / C-26）。
// Summary call, output validation, and the injection-time source gate.

// —— 替身：Provider 客户端 / 调用闸门 / 票据 ——

// fakeClient 记录每次 Chat 的请求，按脚本返回响应或错误。
// fakeClient records every chat request and answers with the scripted response.
type fakeClient struct {
	name string
	log  *[]string
	reqs []core.ChatRequest
	resp core.ChatResponse
	err  error
}

func (c *fakeClient) Chat(_ context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	c.reqs = append(c.reqs, req)
	if c.log != nil {
		*c.log = append(*c.log, "chat:"+c.name)
	}
	return c.resp, c.err
}

// fakeGate 是 `SummaryCallGate` 的替身：记录调用前事实，可编程拒绝。
// fakeGate records the pre-call facts and can refuse the call outright.
type fakeGate struct {
	log       *[]string
	calls     []compaction.SummaryCall
	tickets   []*fakeTicket
	err       error
	ticketErr error
}

func (g *fakeGate) Begin(_ context.Context, call compaction.SummaryCall) (compaction.SummaryTicket, error) {
	if g.log != nil {
		*g.log = append(*g.log, "begin")
	}
	g.calls = append(g.calls, call)
	if g.err != nil {
		return nil, g.err
	}
	t := &fakeTicket{log: g.log, completeErr: g.ticketErr}
	g.tickets = append(g.tickets, t)
	return t, nil
}

// fakeTicket 记录终态回填。
// fakeTicket records the terminal write-back.
type fakeTicket struct {
	log         *[]string
	results     []compaction.SummaryCallResult
	completeErr error
}

func (t *fakeTicket) Complete(_ context.Context, res compaction.SummaryCallResult) error {
	if t.log != nil {
		*t.log = append(*t.log, "complete")
	}
	t.results = append(t.results, res)
	return t.completeErr
}

// summarizerInput 构造一次摘要调用输入（会话范围、固定 attempt）。
// summarizerInput builds one summary-call input.
func summarizerInput(model string) compaction.SummaryInput {
	return compaction.SummaryInput{
		Scope:           compaction.Scope{SessionID: "s1"},
		RunID:           "r1",
		SessionID:       "s1",
		AttemptID:       "a1",
		Model:           model,
		PromptVersion:   "summary-v1",
		StrategyVersion: "cut-v1",
		Covered: []compaction.SummaryMessage{
			{MessageID: "m1", Role: core.RoleUser, Content: "目标"},
		},
		MaxOutputTokens: 100,
	}
}

// —— C-25：审计预写 + 配额预留 + 冻结映射 ——

// TestCompaction_AuditBeforeSummaryCall —— 摘要调用**必须**先预写审计（`pending`）再发出：
// 顺序是 Begin → Chat → Complete，且闸门拒绝时一个字节都不发出（原则 V fail-closed）。
// 逆改点：先 Chat 后 Begin → 顺序断言红；Begin 失败仍发 → calls 变 1（摘要已在无审计时外发）。
func TestCompaction_AuditBeforeSummaryCall(t *testing.T) {
	log := []string{}
	gate := &fakeGate{log: &log}
	client := &fakeClient{name: "frozen", log: &log}
	s := compaction.NewModelSummarizer(map[string]core.ProviderClient{"summary-model": client},
		"summary-model", gate)

	_, err := s.Summarize(ctx, summarizerInput("summary-model"))
	require.NoError(t, err)
	require.Equal(t, []string{"begin", "chat:frozen", "complete"}, log,
		"顺序必须是 预写 → 调用 → 回填；先发后写等于无审计调用")

	require.Len(t, gate.calls, 1)
	call := gate.calls[0]
	require.Equal(t, compaction.Scope{SessionID: "s1"}, call.Scope)
	require.Equal(t, "r1", call.RunID)
	require.Equal(t, "s1", call.SessionID)
	require.Equal(t, "a1", call.AttemptID)
	require.Equal(t, "summary-model", call.Model)
	require.Equal(t, "summary-v1", call.PromptVersion)
	require.NotEmpty(t, call.Provider)
	require.Positive(t, call.RequestBytes, "预写要带请求体积，否则预留算不出成本")

	// 闸门拒绝：预写 / 预留失败 ⇒ 摘要**不得**发出（fail-closed）。
	log2 := []string{}
	rejecting := &fakeGate{log: &log2, err: errors.New("审计写入失败")}
	blocked := &fakeClient{name: "blocked", log: &log2}
	s2 := compaction.NewModelSummarizer(map[string]core.ProviderClient{"summary-model": blocked},
		"summary-model", rejecting)
	_, err = s2.Summarize(ctx, summarizerInput("summary-model"))
	require.ErrorIs(t, err, compaction.ErrSummaryFailed)
	require.Equal(t, []string{"begin"}, log2, "闸门拒绝后不得调用、不得回填")
	require.Empty(t, blocked.reqs)
}

// TestCompaction_SummaryCallQuotaReserved —— 每次真实调用都要有**稳定的预留身份**并在结束后
// 结算：`attempt_id` 由调用方给定（配额键 `run:<run_id>:compact:<attempt_id>` 的组成部分），
// 摘要器**不得**自造；调用后**必须**回填终态（success/failed + 用量/错误类别），
// 回填失败不改调用结论。
// 逆改点：每次调用自造 attempt → 预留在不同键上重复计费；不回填 → 预留永远 pending。
func TestCompaction_SummaryCallQuotaReserved(t *testing.T) {
	gate := &fakeGate{}
	client := &fakeClient{name: "frozen", resp: core.ChatResponse{
		Content: "摘要正文",
		Usage:   core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}}
	s := compaction.NewModelSummarizer(map[string]core.ProviderClient{"summary-model": client},
		"summary-model", gate)

	_, err := s.Summarize(ctx, summarizerInput("summary-model"))
	require.NoError(t, err)
	_, err = s.Summarize(ctx, summarizerInput("summary-model"))
	require.NoError(t, err)

	require.Len(t, gate.calls, 2)
	require.Equal(t, gate.calls[0].AttemptID, gate.calls[1].AttemptID,
		"同一 attempt 的重复调用必须落在同一个配额键上（不得每次自造身份）")
	require.Len(t, gate.tickets, 2, "每次真实调用各有一张预留票据")
	for _, tk := range gate.tickets {
		require.Len(t, tk.results, 1, "调用后必须结算，不得把预留留在 pending")
		require.Equal(t, core.LLMCallSuccess, tk.results[0].Status)
		require.Equal(t, core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
			tk.results[0].Usage, "结算按实际用量回填")
	}

	// 失败调用：终态是 failed + 错误类别，且错误按超时 / 其它分类上报。
	failing := &fakeClient{name: "frozen", err: context.DeadlineExceeded}
	gate2 := &fakeGate{}
	s2 := compaction.NewModelSummarizer(map[string]core.ProviderClient{"summary-model": failing},
		"summary-model", gate2)
	_, err = s2.Summarize(ctx, summarizerInput("summary-model"))
	require.ErrorIs(t, err, compaction.ErrSummaryTimeout)
	require.Len(t, gate2.tickets, 1)
	require.Equal(t, core.LLMCallFailed, gate2.tickets[0].results[0].Status)
	require.Equal(t, core.ErrorKindTimeout, gate2.tickets[0].results[0].ErrorKind)
	require.Error(t, gate2.tickets[0].results[0].Err)

	// 回填失败不改结论：调用已经发生，结论不因写不进去而翻转。
	gate3 := &fakeGate{ticketErr: errors.New("终态回填失败")}
	s3 := compaction.NewModelSummarizer(map[string]core.ProviderClient{"summary-model": &fakeClient{
		name: "frozen",
		resp: core.ChatResponse{Content: "摘要正文"},
	}}, "summary-model", gate3)
	out, err := s3.Summarize(ctx, summarizerInput("summary-model"))
	require.NoError(t, err, "回填失败不是调用失败——结论不得翻转")
	require.Equal(t, "摘要正文", out.Text)
}

// TestCompaction_SummaryCallUsesFrozenProvider —— 摘要调用只查**准入时冻结**的映射：
// 构造后改动的 map 不影响已发布映射；查表不中即失败，**不得**换模型 / 换供应商重试。
// 逆改点：每次按名字重新解析（或持有调用方 map 引用）→ 改动后的客户端被调用；
// 失败后 fallback → 备用客户端收到第二次调用。
func TestCompaction_SummaryCallUsesFrozenProvider(t *testing.T) {
	frozen := &fakeClient{name: "frozen", resp: core.ChatResponse{Content: "冻结的响应"}}
	late := &fakeClient{name: "late", resp: core.ChatResponse{Content: "后加的响应"}}
	clients := map[string]core.ProviderClient{"summary-model": frozen}
	s := compaction.NewModelSummarizer(clients, "summary-model", &fakeGate{})

	clients["summary-model"] = late // 构造后再改调用方的 map
	clients["other-model"] = late

	out, err := s.Summarize(ctx, summarizerInput("summary-model"))
	require.NoError(t, err)
	require.Equal(t, "冻结的响应", out.Text, "必须用构造时冻结的客户端")
	require.Len(t, frozen.reqs, 1)
	require.Empty(t, late.reqs)

	// 模型名为空 → 用冻结的默认模型（仍在冻结表内）。
	out, err = s.Summarize(ctx, summarizerInput(""))
	require.NoError(t, err)
	require.Equal(t, "summary-model", out.Model)
	require.Len(t, frozen.reqs, 2)

	// 查表不中：报错，不遍历推断、不回落。
	gate := &fakeGate{}
	s2 := compaction.NewModelSummarizer(map[string]core.ProviderClient{
		"summary-model": frozen, "other-model": late,
	}, "summary-model", gate)
	_, err = s2.Summarize(ctx, summarizerInput("missing-model"))
	require.ErrorIs(t, err, compaction.ErrSummaryFailed)
	require.Empty(t, gate.calls, "映射查表不中时不得预写 / 发出任何调用")
	before := len(frozen.reqs)
	require.Equal(t, before, len(frozen.reqs), "不得回落到默认模型重试")

	// 调用失败也不得自动换供应商。
	boom := &fakeClient{name: "boom", err: errors.New("upstream 500")}
	spare := &fakeClient{name: "spare", resp: core.ChatResponse{Content: "备用的"}}
	s3 := compaction.NewModelSummarizer(map[string]core.ProviderClient{
		"summary-model": boom, "spare-model": spare,
	}, "summary-model", &fakeGate{})
	_, err = s3.Summarize(ctx, summarizerInput("summary-model"))
	require.ErrorIs(t, err, compaction.ErrSummaryFailed)
	require.Len(t, boom.reqs, 1)
	require.Empty(t, spare.reqs, "失败后自动换供应商绕过冻结映射（原则 III）")
}

// —— C-23：输出校验 ——

// TestCompaction_NoClaimWithoutEvidence —— 「已完成」的每条**必须**带证据 ID：
// 没有锚点的完成结论整条摘要判不合规；其它段不受此约束。
// 逆改点：跳过校验 → 无证据的"已完成"被写进链（未验证被当成完成）。
func TestCompaction_NoClaimWithoutEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		text    string
		wantErr bool
	}{
		{"消息锚点", "## 已完成\n- 备份恢复验证通过（msg/m12）", false},
		{"工具调用锚点", "## 已完成\n- 查询成功（tool:call-1）", false},
		{"证据引用锚点", "## 已完成\n- 结论有据（kb-a@v1）", false},
		{"message_id 字面量", "## 已完成\n- 有据（message_id=m12）", false},
		{"无锚点", "## 已完成\n- 备份恢复未验证", true},
		{"混合：一条有据一条没有", "## 已完成\n- 有据（msg/m1）\n- 无据", true},
		{"非完成段不判", "## 进行中\n- 备份恢复未验证\n## 阻塞 / 未决\n- 未知", false},
		{"没有完成段", "## 目标\n整理报表", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compaction.ValidateSummaryText(tc.text)
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, compaction.ErrSummaryClaimWithoutEvidence)
		})
	}
}

// TestCompaction_RenderCoveredFiltering —— 摘要输入的序列化口径（纯函数）：
// 失效消息整条换占位符（原文不外发）、工具结果截断带显式标记且保留首尾、
// 用户原话不截断（硬约束可能在任意一轮用户消息里）。
// 逆改点：失效消息只清空正文不留位置标识；工具结果截成空串（工具组"看起来没结果"）。
func TestCompaction_RenderCoveredFiltering(t *testing.T) {
	const secret = "SECRET-原文"
	long := strings.Repeat("a", 100)
	rendered := compaction.RenderCovered([]compaction.SummaryMessage{
		{MessageID: "m1", Role: core.RoleUser, Content: secret, Redacted: true},
		{MessageID: "m2", Role: core.RoleTool, Content: long, ToolCallID: "call-1"},
		{MessageID: "m3", Role: core.RoleUser, Content: long},
	}, 30)

	require.NotContains(t, rendered, secret, "失效消息的原文不得进入摘要输入")
	require.Contains(t, rendered, "m1", "位置与 message_id 必须保留（证据锚点）")
	require.Contains(t, rendered, "[内容不可用：来源已失效]")
	require.Contains(t, rendered, "tool_call_id=call-1", "截断不得让调用与结果失配")
	require.Contains(t, rendered, "[truncated 70 chars]", "截断必须显式标记，不得静默")
	require.Contains(t, rendered, long[:20], "保留首段")
	require.Contains(t, rendered, long[90:], "保留尾段")
	require.Equal(t, 1, strings.Count(rendered, long),
		"只有用户那条整条保留；工具结果已被截断（首尾各留片段，中间带标记）")
}

// TestCompaction_CarryCodeBlocksVerbatim —— 两节代码保真段原样搬运；段不存在时不产出空段，
// 也不得把整份摘要当某一节复制过去。
// 逆改点：ExtractSection 返回整份摘要 → 新摘要出现重复全文。
func TestCompaction_CarryCodeBlocksVerbatim(t *testing.T) {
	previous := "## 目标\n整理报表\n## 硬约束与否决条件\n- 原话：不要删除 X（m1）\n" +
		"## 阻塞 / 未决\n- 备份恢复未验证\n## 下一步\n1. 继续"
	carried := compaction.CarryCodeBlocks(previous)
	require.Contains(t, carried, "## 硬约束与否决条件\n- 原话：不要删除 X（m1）")
	require.Contains(t, carried, "## 阻塞 / 未决\n- 备份恢复未验证")
	require.NotContains(t, carried, "整理报表", "只搬两节，不得把其它段一起复制")
	require.NotContains(t, carried, "1. 继续")

	require.Equal(t, "- 原话：不要删除 X（m1）",
		compaction.ExtractSection(previous, "## 硬约束与否决条件"))
	require.Empty(t, compaction.ExtractSection(previous, "## 不存在的段"))
	require.Empty(t, compaction.CarryCodeBlocks(""), "没有上一份摘要时不产出空段")
}

// —— C-26：降级路径共用同一处过滤 ——

// TestCompaction_FallbackAppliesSameFilter —— 压缩失败后回退原始历史时，**同一处**来源闸门
// 照常生效：工具组任一条失效整组丢弃、单条失效换占位符、身份零值拒绝、校验器未装配拒绝，
// 且返回值总是新切片。
// 逆改点：降级路径不过滤 → 已撤权原文随回退历史照发给模型。
func TestCompaction_FallbackAppliesSameFilter(t *testing.T) {
	p := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller-1",
		KeyID:    "key-1",
		Scopes:   []string{core.ScopeSessionsRead},
		Actor:    core.ActorCaller,
	}
	revoked := ref("kb-x", "v1")
	validator := &fakeValidity{revoked: map[core.EvidenceRef]string{revoked: compaction.ValidityRevoked}}
	gate := compaction.SourceGate{Validator: validator}

	t.Run("工具组有一条失效则整组丢弃", func(t *testing.T) {
		msgs := []core.Message{
			{Role: core.RoleUser, Content: "目标", ID: "m1"},
			{Role: core.RoleAssistant, ID: "m2", ToolCalls: []core.ToolCall{{ID: "call-1", Name: "source_view"}}},
			{Role: core.RoleTool, ID: "m3", ToolCallID: "call-1", Content: "结果", EvidenceRefs: []core.EvidenceRef{revoked}},
			{Role: core.RoleUser, Content: "继续", ID: "m4"},
		}
		out, err := gate.Filter(ctx, p, msgs)
		require.NoError(t, err)
		require.Equal(t, []string{"m1", "m4"}, idsOf(out),
			"工具组里只要有一条失效就整组丢弃——拆组会让调用与结果失配")
		require.NotSame(t, &msgs[0], &out[0], "返回新切片，调用方改动不会回改存储层对象")
	})

	t.Run("单条失效换占位符且原文不外发", func(t *testing.T) {
		msgs := []core.Message{
			{
				Role: core.RoleUser, Content: "SECRET-原文", ID: "m1", EvidenceRefs: []core.EvidenceRef{revoked},
				ReasoningContent: "内部推理",
			},
			{Role: core.RoleUser, Content: "继续", ID: "m2"},
		}
		out, err := gate.Filter(ctx, p, msgs)
		require.NoError(t, err)
		require.Len(t, out, 2, "单条失效只替换正文，不丢位置")
		require.Equal(t, "[内容不可用：来源已失效]", out[0].Content)
		require.Empty(t, out[0].ReasoningContent, "推理内容一并清掉——它同样是原文")
		require.Equal(t, "m1", out[0].ID, "证据锚点保留")
		require.Equal(t, "SECRET-原文", msgs[0].Content, "入参不得被就地改写")
	})

	t.Run("身份零值拒绝", func(t *testing.T) {
		_, err := gate.Filter(ctx, core.Principal{}, []core.Message{{Role: core.RoleUser, Content: "x"}})
		require.ErrorIs(t, err, core.ErrUnauthorized, "身份零值 fail-closed，不试产 prompt")
	})

	t.Run("校验器未装配拒绝", func(t *testing.T) {
		_, err := (compaction.SourceGate{}).Filter(ctx, p, []core.Message{{Role: core.RoleUser, Content: "x"}})
		require.ErrorIs(t, err, compaction.ErrScopeInvalid,
			"没有校验器就放行 = 失效规则被跳过；必须拒绝")
	})
}

// idsOf 取消息标识序列（过滤结果的判据）。
// idsOf extracts the message ids of a filtered slice.
func idsOf(msgs []core.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out
}
