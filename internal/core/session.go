package core

import (
	"context"
	"errors"
)

// 本文件是会话面（`RunEnv.Session` 的载体）：内存历史 + 写透仓储，外加半截工具组补齐
// （docs/us/us4-react-loop.md §2.3 流程 E / 契约 §4.4 Option A）。
// This file carries the session face: an in-memory history with a write-through store,
// plus half-group completion (flow E / contract §4.4 option A).
//
// **消息追加唯一路径**（宪法 II）：正常运行中模型响应与工具结果都由本类型的
// `AppendMessage` / `AppendToolResult` 落库，用户输入由 `AgentService` 调同一方法写入，
// 崩溃 / 取消后的半截组由 `CompleteToolGroup` 补齐——**不得**出现第二个出口；
// 工具调度器只返回结果、不写消息。
// Message appends have exactly one path: the loop and the service write through this type,
// and the completion path repairs through it as well. The tool executor never writes messages.

// Session 是会话历史的读写面：内存历史 + 写透仓储；空会话标识 = **无状态**（仅内存，
// 不落会话历史，但该次运行的 Run 与工具审计照常落库）；半截工具组的补齐也归它
// （流程 E——补进**消息**、不进审计）。
// Session is the read/write face of session history: an in-memory history with a
// write-through store. An empty session id means stateless. Half-group completion lands
// here too, and it only ever writes messages, never audit rows.
type Session struct {
	sessionID string               // 空串 = 无状态（不落库）
	store     MessageStore         // 写透仓储；无状态会话为 nil
	audit     ToolInvocationReader // 补齐分类只读审计行（Session 只读不写审计）
	messages  []Message            // 内存历史（升序；Load 一次、之后只追加）
}

// NewSession 组装一个会话面。`store` 为空（或 `sessionID` 为空）即无状态会话；
// `audit` 供补齐分类使用，可为 nil（此时一律走"无行"分支——宁可说"未执行"也不猜）。
// NewSession assembles the session face. An empty store or session id yields a stateless
// session; audit may be nil, in which case completion always takes the "no row" branch.
func NewSession(sessionID string, store MessageStore, audit ToolInvocationReader) *Session {
	return &Session{sessionID: sessionID, store: store, audit: audit}
}

// Load 载入整段会话历史（升序）；无状态会话为 no-op。
// Load reads the whole session history; a stateless session does nothing.
func (s *Session) Load(ctx context.Context) error {
	if !s.persistent() {
		return nil
	}
	msgs, err := s.store.Messages(ctx, s.sessionID)
	if err != nil {
		return err
	}
	s.messages = msgs
	return nil
}

// AppendMessage 追加一条消息：**先写透仓储、再进内存**——反过来会在写失败时留下
// "只存在于内存"的消息，而调用方拿到错误后会按失败处置（半截组交恢复扫描），
// 内存里不该多出一条库中没有的历史。
// AppendMessage writes through to the store first and only then to memory, so a failed
// write never leaves a message that exists in memory alone.
//
// 写入上下文经 `finishContext` 派生（剥取消信号 + 独立短超时）：取消是**预期动作**，
// 不该让"取消"伪装成"存储故障"——那会把 `cancelled` 误判成 `failed`（契约 §2 状态机）。
// Writes use the detached finish context: a cancellation must never masquerade as a
// store failure, which would turn a cancelled run into a failed one.
func (s *Session) AppendMessage(ctx context.Context, runID string, m Message) error {
	if s.persistent() {
		writeCtx, cancel := finishContext(ctx)
		defer cancel()
		if err := s.store.AppendMessage(writeCtx, s.sessionID, runID, m); err != nil {
			return err
		}
	}
	s.messages = append(s.messages, m)
	return nil
}

// AppendToolResult 追加一条工具结果消息（由循环在工具执行后调用——**循环是唯一追加者**）。
// AppendToolResult appends one tool result message; the loop is its only caller.
func (s *Session) AppendToolResult(ctx context.Context, runID string, call ToolCall, r Result) error {
	return s.AppendMessage(ctx, runID, Message{
		Role:       RoleTool,
		Content:    r.Content,
		ToolCallID: call.ID,
	})
}

// AppendSynthesizedToolMessage 追加一条**合成**的工具结果（补齐路径专用）。
// 与 `AppendToolResult` 分开是为了让"真实结果"与"合成结果"在调用点就能区分——
// 合成的只进消息、不进审计（审计永远只记真实发生的事）。
// AppendSynthesizedToolMessage appends a synthesized result. Keeping it distinct from
// AppendToolResult makes the synthesis path explicit: it writes messages only.
func (s *Session) AppendSynthesizedToolMessage(ctx context.Context, runID, toolCallID, content string) error {
	return s.AppendMessage(ctx, runID, Message{
		Role:       RoleTool,
		Content:    content,
		ToolCallID: toolCallID,
	})
}

// CompleteToolGroup 把一组工具调用补齐到"人人有结果"（流程 E；契约 §4.4）。
// 判据落在**消息**上（先判缺失、再分类，顺序不得颠倒），故本方法是**幂等**的：
// 组已完整即 no-op，重复调用不重复插入。
// CompleteToolGroup makes a tool-call group complete. Its criterion lives on the
// messages, so it is idempotent: a complete group is a no-op.
//
// 收尾写入用**独立**于父 ctx 的有限时长上下文——调用它时 ctx 往往已取消。
// 补齐失败**必须**如实返回（`errors.Join`），由调用方记 ERROR 并照常收场；
// 但**不得**因为补齐失败就不写：漏一条 tool 结果会让整段历史对供应商不可用。
// The finish context is detached from the parent; failures are reported through the
// joined error, and callers log them without failing the run.
func (s *Session) CompleteToolGroup(ctx context.Context, runID string, calls []ToolCall, reason string) error {
	finCtx, cancel := finishContext(ctx)
	defer cancel()

	var errs []error
	for _, call := range MissingToolResults(calls, s.Messages()) { // ① 先判缺失（判据在消息上）
		content, err := SynthesizeMissingResult(finCtx, s.audit, runID, call, reason) // ② 再分类（五分支）
		if err != nil {
			// 查审计失败不吞：记 error 后仍写兜底文本——宁可说"结果未知"，不可漏消息。
			errs = append(errs, err)
			content = TextResultUnknown
		}
		if err := s.AppendSynthesizedToolMessage(finCtx, runID, call.ID, content); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Messages 返回内存历史的**副本**（升序）。
// Messages returns a copy of the in-memory history; callers cannot mutate session state.
func (s *Session) Messages() []Message {
	out := make([]Message, len(s.messages))
	copy(out, s.messages)
	return out
}

// persistent 判定是否需要写透仓储。
// persistent reports whether this session writes through to the store.
func (s *Session) persistent() bool { return s.sessionID != "" && s.store != nil }

// MissingToolResults 返回 `calls` 里**还没有结果消息**的那些调用（顺序保持）。
// 判据只看消息：某个 `tool_call_id` 是否已有一条 `role=tool` 的消息——
// 取消路径（循环内）与崩溃路径（启动恢复）**共用**这一份实现，两处判定必须同解。
// MissingToolResults reports which calls lack a result message. The criterion is purely
// the messages, so the cancel path and the recovery path always agree.
func MissingToolResults(calls []ToolCall, msgs []Message) []ToolCall {
	answered := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		if m.Role == RoleTool && m.ToolCallID != "" {
			answered[m.ToolCallID] = struct{}{}
		}
	}
	var missing []ToolCall
	for _, call := range calls {
		if _, ok := answered[call.ID]; ok {
			continue
		}
		missing = append(missing, call)
	}
	return missing
}

// finishContext 派生收尾上下文：剥掉取消信号 + 独立有限时长（契约 §4.2）。
// 三类写入（消息、工具审计终态、运行终态）共用它——被取消的运行正是最需要证据的时刻，
// 证据**不得**随请求一起被掐掉。
// finishContext derives the detached, bounded context shared by finish-order writes.
func finishContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
}
