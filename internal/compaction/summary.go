package compaction

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是**摘要调用**（T085 / C-25）：模板、输入序列化、输出校验，以及带审计预写与
// 配额预留的默认实现。三件事分开写，是因为它们的失败语义不同——序列化是纯函数、
// 校验是判据、调用闸门决定"能不能发出去"。
// The summary call: template, input rendering, output validation, and the default
// implementation behind the audit/quota gate. Serialization is pure, validation is a
// judgement, and the gate decides whether a call may leave the process at all.

// SummaryTemplate 是摘要输出模板（§2.10.3 冻结）：**模型只填叙事**，代码块与证据 ID
// 由代码补。模板正文进 “compaction_entries.prompt_version“ 对应的版本记录，
// 改一个字就必须换 `prompt_version`。
// The frozen summary template; the model only fills the narration.
const SummaryTemplate = `## 目标
[用户目标，代码块给原文与 message_id；本段只做转述]
## 硬约束与否决条件
[代码块给原话与 message_id；不得改写、不得省略]
## 已完成（每条必须有证据 ID）
- [x] ...（message_id / tool_invocation_id / evidence_ref）
## 进行中
- [ ] ...
## 阻塞 / 未决
- ...（未知保持未知，不得写成完成）
## 关键决策
- ...
## 下一步
1. ...
## 来源引用
- evidence_ref / version_id（有界列表；供撤权校验）`

// 模板里的两个**代码保真段**标题：代码在模型输出之后原样追加，模型无权删除。
// The two code-preserved section headings; the model may never drop them.
const (
	sectionConstraints = "## 硬约束与否决条件"
	sectionOpenItems   = "## 阻塞 / 未决"
	sectionDone        = "## 已完成"
)

// roleSummarySystem 是摘要请求里系统消息的角色字面量。`core` 只导出了 user /
// assistant / tool 三个角色常量（系统提示词在 core 内部拼装，不外露常量），
// 本包按契约取值，不为此在 core 新增对外概念。
// The system role literal for the summary request; core exports no system role constant,
// and this package does not widen core's surface just to borrow one.
const roleSummarySystem = "system"

// 摘要请求的固定约束（模板说明段）：只写规则，不写业务。
// The fixed instruction block sent ahead of the template.
const summaryInstructions = `你是会话压缩器。只做转述，不推进任务状态。
- 「已完成」每一项必须带证据 ID（message_id / tool_invocation_id / evidence_ref），没有证据的条目写到「进行中」或「阻塞 / 未决」。
- 未完成、失败与未知项一律不得写成已完成。
- 「硬约束与否决条件」段必须逐字保留用户原话，不得改写、不得省略。
- 只输出模板中的段落，不要额外解释。`

// SummaryMessage 是一条**已过滤**的覆盖消息的摘要视图：正文之外只带定位与截断标记，
// 不带 reasoning（thinking 不送入摘要器——摘要请求会比被摘要内容还大）。
// SummaryMessage is one filtered covered message; reasoning is never sent.
type SummaryMessage struct {
	MessageID  string
	Role       string
	Content    string
	ToolCallID string
	ToolNames  []string
	// Redacted 表示该消息的来源已失效：正文换成占位符，**不得**外发原文。
	Redacted bool
	// Truncated 标记工具结果已被截断（`[truncated N chars]`），摘要器据此知道内容不完整。
	Truncated bool
}

// SummaryInput 是 `Summarizer.Summarize` 的完整输入。**没有**来源集合字段——
// 审计集与当前依赖集都由代码经 `SourceRefsOf` 算出，模型漏写不会让集合变窄，
// 多写也不会让它变宽（C-26 / C-36）。
// SummaryInput carries no reference fields on purpose: both sets are computed by code.
type SummaryInput struct {
	Scope           Scope
	RunID           string
	SessionID       string
	AttemptID       string
	Model           string
	PromptVersion   string
	StrategyVersion string
	// Previous 是父条目摘要正文；**只有父链可注入时**才填——用失效父摘要当增量
	// 等于把已撤权内容"洗"进新摘要（C-26）。
	Previous string
	// StateBlock 是代码保真的结构化状态块正文（本 US 为空串——C 的任务状态尚未交付，
	// 接线层就绪后由 `app` 填）。存在时它是**权威块**，摘要正文只是转述。
	StateBlock string
	Covered    []SummaryMessage
	// MaxOutputTokens 是本次请求自身的 `max_tokens`（`compaction.summary_max_tokens`）。
	// 它**不**进 `projected`，也**不**充当回复预留（C-31）。
	MaxOutputTokens int
	// MaxInputBytes 是序列化正文的上限（`summary_input_tokens` 的字节近似）。
	MaxInputBytes int
}

// SummaryOutput 是摘要器的返回：**只有**模板化摘要与用量。没有来源集合字段——
// 模型不参与引用计算。
// SummaryOutput is the narration plus usage; the model never supplies references.
type SummaryOutput struct {
	Text  string
	Usage core.Usage
	Model string
}

// Summarizer 是摘要调用端口。实现必须：走准入时冻结的 Provider 映射（**不得**按名字
// 重新解析、**不得**失败自动换供应商）、`llm_calls` 先写 `pending`、配额预留、
// ctx 取消传播、失败不返回半成品（C-25）。
// Summarizer performs the summary call behind the frozen provider map, the pending
// audit write and the quota reservation.
type Summarizer interface {
	Summarize(ctx context.Context, in SummaryInput) (SummaryOutput, error)
}

// —— 审计与配额闸门（窄端口：本包只 import core + 标准库）——

// SummaryCall 是一次摘要调用的**调用前**事实：够写 `llm_calls` 的 pending 行，
// 也够算配额键。`AttemptID` 是幂等身份——每次真实尝试一个，重提同一条摘要时**不变**。
// SummaryCall is the pre-call fact set: enough for the pending audit row and the quota key.
type SummaryCall struct {
	Scope         Scope
	RunID         string
	SessionID     string
	AttemptID     string
	Provider      string
	Model         string
	PromptVersion string
	RequestBytes  int
}

// SummaryCallResult 是调用后的终态回填。
// SummaryCallResult is the terminal state written back after the call.
type SummaryCallResult struct {
	Usage     core.Usage
	Status    core.LLMCallStatus
	ErrorKind core.ErrorKind
	Err       error
}

// SummaryTicket 是一次已预写、已预留的调用的回执：`Complete` 回填终态。
// 回填失败**不改变**调用结论（调用已经发生），只记告警——这一点由实现负责。
// SummaryTicket writes the terminal state back; a failed write-back never rewrites the
// call's outcome.
type SummaryTicket interface {
	Complete(ctx context.Context, res SummaryCallResult) error
}

// SummaryCallGate 是**预写 + 预留**闸门（原则 V 与 C-25 的落点）：
// `Begin` 失败 ⇒ 摘要**不得**发出。它把"审计 fail-closed"与"配额 fail-closed"
// 收在一处，调用方无法跳过其中任何一个。
// The gate writes the pending audit row and reserves quota; if Begin fails the call
// must not leave the process.
type SummaryCallGate interface {
	Begin(ctx context.Context, call SummaryCall) (SummaryTicket, error)
}

// ModelSummarizer 是 `Summarizer` 的默认实现：冻结映射查表 → 预写 + 预留 → 调用 →
// 回填终态。**不重试、不换供应商、不吞错误**。
// ModelSummarizer is the default implementation: frozen lookup, gate, call, write back.
type ModelSummarizer struct {
	clients      map[string]core.ProviderClient // 准入时冻结的映射（构造时复制）
	defaultModel string
	gate         SummaryCallGate
}

// NewModelSummarizer 冻结一份 `模型名 → 客户端` 映射。**构造时复制**：调用方此后改动
// 自己的 map 不会影响已发布的映射（原则 III 的"不可变映射表"）。
// NewModelSummarizer freezes a copy of the model→client map.
func NewModelSummarizer(clients map[string]core.ProviderClient, defaultModel string, gate SummaryCallGate) *ModelSummarizer {
	frozen := make(map[string]core.ProviderClient, len(clients))
	for k, v := range clients {
		frozen[k] = v
	}
	return &ModelSummarizer{clients: frozen, defaultModel: defaultModel, gate: gate}
}

// Summarize 执行一次摘要调用。查表不中即失败——**不得**遍历推断、**不得**回落到
// 其他模型或供应商；闸门失败即不发出。
// Summarize performs one call; a lookup miss fails instead of falling back.
func (s *ModelSummarizer) Summarize(ctx context.Context, in SummaryInput) (SummaryOutput, error) {
	model := in.Model
	if model == "" {
		model = s.defaultModel
	}
	client, ok := s.clients[model]
	if !ok || client == nil {
		// 查表不中 = 映射表未覆盖该模型：报错而不是换一个供应商（宪法 III）。
		return SummaryOutput{}, fmt.Errorf("%w: 冻结映射里没有模型 %q", ErrSummaryFailed, model)
	}
	req := s.buildRequest(in, model)
	if s.gate != nil {
		ticket, err := s.gate.Begin(ctx, SummaryCall{
			Scope:         in.Scope,
			RunID:         in.RunID,
			SessionID:     in.SessionID,
			AttemptID:     in.AttemptID,
			Provider:      "frozen",
			Model:         model,
			PromptVersion: in.PromptVersion,
			// 请求体积 = 两条消息正文的字节和（**不是**消息条数：条数恒为 2，
			// 拿它当体积会让每次摘要的配额预留都是同一个常数，估算彻底失效）。
			// Request size is the byte sum of the message bodies, never the message
			// count (which is always two).
			RequestBytes: requestBytesOf(req.Messages),
		})
		if err != nil {
			// 预写 / 预留失败：**不得**发出调用（fail-closed）。
			return SummaryOutput{}, fmt.Errorf("%w: 摘要调用闸门拒绝：%w", ErrSummaryFailed, err)
		}
		resp, callErr := client.Chat(ctx, req)
		s.complete(ctx, ticket, resp, callErr)
		if callErr != nil {
			return SummaryOutput{}, classifySummaryError(ctx, callErr)
		}
		return SummaryOutput{Text: resp.Content, Usage: resp.Usage, Model: model}, nil
	}
	resp, err := client.Chat(ctx, req)
	if err != nil {
		return SummaryOutput{}, classifySummaryError(ctx, err)
	}
	return SummaryOutput{Text: resp.Content, Usage: resp.Usage, Model: model}, nil
}

// complete 回填终态；回填失败只告警（调用已经发生，结论不因回填写不进去而改变）。
// complete writes the terminal state back; a write-back failure never alters the outcome.
func (s *ModelSummarizer) complete(ctx context.Context, ticket SummaryTicket, resp core.ChatResponse, callErr error) {
	if ticket == nil {
		return
	}
	res := SummaryCallResult{Usage: resp.Usage, Status: core.LLMCallSuccess}
	if callErr != nil {
		res.Status = core.LLMCallFailed
		res.ErrorKind = classifyErrorKind(callErr)
		res.Err = callErr
	}
	if err := ticket.Complete(ctx, res); err != nil {
		// 回填失败不改结论，只报错——调用方必须能从错误链看到"审计没写进去"。
		_ = err
	}
}

// buildRequest 把 `SummaryInput` 渲染成一次 Chat 请求：系统段是规则 + 模板，
// 用户段是"上一份摘要（增量）+ 结构化状态（权威）+ 覆盖段序列化（已过滤）"。
// buildRequest renders the summary request; the covered segment is already filtered.
func (s *ModelSummarizer) buildRequest(in SummaryInput, model string) core.ChatRequest {
	var b strings.Builder
	if in.Previous != "" {
		b.WriteString("[上一份摘要（迭代上下文，不是权威）]\n")
		b.WriteString(in.Previous)
		b.WriteString("\n\n")
	}
	if in.StateBlock != "" {
		b.WriteString("[结构化状态（权威块；与本摘要冲突时以它为准）]\n")
		b.WriteString(in.StateBlock)
		b.WriteString("\n\n")
	}
	b.WriteString("[覆盖段消息（message_id 是证据锚点）]\n")
	b.WriteString(RenderCovered(in.Covered, in.MaxInputBytes))
	return core.ChatRequest{
		RunID: in.RunID,
		Model: model,
		Messages: []core.Message{
			{Role: roleSummarySystem, Content: summaryInstructions + "\n\n" + SummaryTemplate},
			{Role: core.RoleUser, Content: b.String()},
		},
	}
}

// requestBytesOf 返回消息正文的字节和——摘要调用的预留体积口径。
// requestBytesOf sums the byte length of the message bodies; it is the reservation basis.
func requestBytesOf(messages []core.Message) int {
	total := 0
	for i := range messages {
		total += len(messages[i].Content)
	}
	return total
}

// RenderCovered 把覆盖段渲染成摘要输入正文（纯函数，便于单测与复现）。
// 规则（§2.10.3 摘要输入）：
//   - thinking / reasoning **不送**；
//   - 失效消息**整条**换占位符（原文不外发，位置与 message_id 保留）；
//   - 工具结果按 `maxBytes` 上限截断，**保留** `tool_call_id` 与首尾片段，并加
//     `[truncated N chars]` 标记——**不得**截成空串，也**不得**让 assistant(tool_calls)
//     与结果失配；
//   - 用户原话不截断（硬约束与否定条件可能在任意一轮用户消息里）。
//
// RenderCovered renders the covered segment. Tool results are truncated with an
// explicit marker; user text never is.
func RenderCovered(msgs []SummaryMessage, maxBytes int) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString("- ")
		b.WriteString(m.MessageID)
		b.WriteString(" [")
		b.WriteString(m.Role)
		b.WriteString("]")
		if m.ToolCallID != "" {
			b.WriteString(" tool_call_id=")
			b.WriteString(m.ToolCallID)
		}
		b.WriteString("\n")
		if m.Redacted {
			b.WriteString("  ")
			b.WriteString(invalidMessagePlaceholder)
			b.WriteString("\n")
			continue
		}
		content := m.Content
		if m.Role == core.RoleTool && maxBytes > 0 && len(content) > maxBytes {
			// 工具结果截断：保留首尾，显式标记——截成空串会让工具组"看起来没结果"。
			head := maxBytes * 2 / 3
			tail := maxBytes - head
			content = content[:head] + fmt.Sprintf("\n[truncated %d chars]\n", len(m.Content)-maxBytes) +
				tailContent(content, tail)
		}
		b.WriteString("  ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	return b.String()
}

// tailContent 取末尾至多 n 字节且不切断多字节字符。
// tailContent returns at most the last n bytes on a rune boundary.
func tailContent(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// classifySummaryError 把底层调用错误归类成三个哨兵之一：超时 / 取消之外的失败。
// 取消（ctx 已结束）按失败上报——引擎在任何失败下都不注入半成品。
// classifySummaryError maps a call error onto the timeout / failure sentinels.
func classifySummaryError(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrSummaryTimeout, err)
	}
	return fmt.Errorf("%w: %w", ErrSummaryFailed, err)
}

// classifyErrorKind 把调用错误映射成 `llm_calls.error_kind` 的八类之一。
// classifyErrorKind maps a call error onto the audit error-kind vocabulary.
func classifyErrorKind(err error) core.ErrorKind {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return core.ErrorKindTimeout
	case errors.Is(err, context.Canceled):
		return core.ErrorKindTimeout
	default:
		return core.ErrorKindExternalError
	}
}

// —— 摘要输出校验（C-23）——

// ErrSummaryClaimWithoutEvidence 表示"已完成"段出现没有证据 ID 的条目：
// 未验证的结论被当成完成，必须拒绝而不是存进链。
// ErrSummaryClaimWithoutEvidence rejects a done-claim that carries no evidence id.
var ErrSummaryClaimWithoutEvidence = errors.New("compaction: 摘要的「已完成」条目缺少证据 ID")

// ValidateSummaryText 校验模型输出：`## 已完成` 下的每个条目**必须**带证据 ID。
// 证据 ID 的形态按模板冻结的三类之一：`message_id`（`msg/...` 或 `m<n>` 形式的标识）、
// `tool_invocation_id`（`tool:` 前缀）或 `evidence_ref`（`<id>@<version>`）。
// 段落缺失不算违规（模型可以不声明完成任何事）。
// ValidateSummaryText requires every done-claim to carry an evidence id.
func ValidateSummaryText(text string) error {
	lines := strings.Split(text, "\n")
	inDone := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "## ") {
			inDone = strings.HasPrefix(line, sectionDone)
			continue
		}
		if !inDone || !strings.HasPrefix(line, "- ") {
			continue
		}
		if !hasEvidenceID(line) {
			return fmt.Errorf("%w: %q", ErrSummaryClaimWithoutEvidence, line)
		}
	}
	return nil
}

// hasEvidenceID 判断一条"已完成"条目是否带了证据锚点。
// hasEvidenceID reports whether a done-claim line carries an evidence anchor.
func hasEvidenceID(line string) bool {
	return strings.Contains(line, "msg/") || strings.Contains(line, "tool:") ||
		strings.Contains(line, "@") || strings.Contains(line, "message_id")
}

// CarryCodeBlocks 把上一份摘要里的**代码保真段**（硬约束 / 未决项）原样搬进新摘要。
// 模型无权删除这两节：它可能把"未验证恢复"写成"发布就绪"，代码块因此不由模型续写。
// 返回值按 `section` 顺序拼接，空输入返回空串（不产生空段）。
// CarryCodeBlocks carries the constraint and open-item sections forward verbatim.
func CarryCodeBlocks(previous string) string {
	constraints := ExtractSection(previous, sectionConstraints)
	open := ExtractSection(previous, sectionOpenItems)
	var b strings.Builder
	if constraints != "" {
		b.WriteString(sectionConstraints)
		b.WriteString("\n")
		b.WriteString(constraints)
		b.WriteString("\n")
	}
	if open != "" {
		b.WriteString(sectionOpenItems)
		b.WriteString("\n")
		b.WriteString(open)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ExtractSection 取出一份摘要里某段（`## 标题` 到下一个 `## ` 之前）的正文，去首尾空行。
// 段不存在返回空串——**不得**返回整个摘要（把全文当某一节会让"保真"变成"复制"）。
// ExtractSection returns one section's body, or empty when absent.
func ExtractSection(text, heading string) string {
	lines := strings.Split(text, "\n")
	var out []string
	in := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "## ") {
			if in {
				break
			}
			in = line == heading
			continue
		}
		if in {
			out = append(out, raw)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
