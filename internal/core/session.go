package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
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
	// lease 是本次 Run 的执行权凭证（US-24 T008）：非零时消息追加走 fenced 入口，
	// 零值时退回普通入口（未接执行权的装配）。由 `BindLease` 在准入后绑定一次。
	// lease selects the fenced append when non-zero; BindLease sets it once at admission.
	lease    LeaseToken
	messages []Message // 内存历史（升序；Load 一次、之后只追加）
}

// NewSession 组装一个会话面。`store` 为空（或 `sessionID` 为空）即无状态会话；
// `audit` 供补齐分类使用，可为 nil（此时一律走"无行"分支——宁可说"未执行"也不猜）。
// NewSession assembles the session face. An empty store or session id yields a stateless
// session; audit may be nil, in which case completion always takes the "no row" branch.
func NewSession(sessionID string, store MessageStore, audit ToolInvocationReader) *Session {
	return &Session{sessionID: sessionID, store: store, audit: audit}
}

// BindLease 绑定本次 Run 的执行权凭证（US-24 T008）。准入领取租约后调用一次；
// 运行内**不换代次**——续约成功时代次不变，失权时运行必须结束而不是换凭证继续。
// 零值 token 等价于"未接执行权"，追加退回普通入口。
// BindLease binds the run's ownership credential once at admission. The epoch never
// changes mid-run: a renewal keeps it, and losing it ends the run.
func (s *Session) BindLease(token LeaseToken) {
	s.lease = token
}

// appendThrough 是两条追加入口的分流点：零值凭证走普通写，非零凭证**必须**走 fenced 写，
// 实现缺失即 fail-closed（`ErrLeaseFenceUnavailable`）。这里绝不"降级回普通写"——
// 那正是旧持有者在接管后覆盖新结果的入口。
// appendThrough picks the fenced entry point for a non-zero credential and fails closed
// when the store lacks one. It never degrades to the plain write.
func (s *Session) appendThrough(ctx context.Context, runID string, m Message) (string, error) {
	if s.lease.IsZero() {
		return s.store.AppendMessage(ctx, s.sessionID, runID, m)
	}
	fenced, ok := s.store.(FencedMessageStore)
	if !ok {
		return "", fmt.Errorf("%w（会话存储 %T）", ErrLeaseFenceUnavailable, s.store)
	}
	return fenced.FencedAppendMessage(ctx, s.lease, s.sessionID, runID, m)
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
	// 正文先过「可存储性清洗」（见 SanitizeMessage）：外部工具结果里的 NUL 与
	// 非法 UTF-8 不得让追加失败——失败会把整轮运行记成 failed，补齐路径也会在同一处
	// 再失败一次（2026-10-31 走查实况：`SQLSTATE 22021` + `半截工具组补齐失败`）。
	// The wash runs before both branches so memory-only sessions stay clean too.
	SanitizeMessage(&m, func(field string, replaced int) {
		s.warnReplacedText(runID, m.Role, m.ToolCallID, field, replaced)
	})
	if s.persistent() {
		writeCtx, cancel := finishContext(ctx)
		defer cancel()
		id, err := s.appendThrough(writeCtx, runID, m)
		if err != nil {
			return err
		}
		// 仓储没给出标识 = 库里那行的身份不可知：装配记录会缺 message_id，
		// 压缩覆盖范围也无从谈起。宁可让这次追加失败，也不留一条没有身份的历史。
		// A store that yields no id leaves the message without an identity; refusing the
		// append is safer than keeping a history row nobody can point at.
		if id == "" {
			return errors.New("追加会话消息: 仓储未返回消息标识——历史缺身份，装配记录将无法如实记账")
		}
		m.ID = id
	} else {
		// 无状态会话不落消息表：标识按本 Run 的追加位置生成（pos 从 1 起）。
		// Stateless sessions never persist messages, so the id derives from the append position.
		m.ID = StatelessMessageID(runID, len(s.messages)+1)
	}
	s.messages = append(s.messages, m)
	return nil
}

// warnReplacedText 记一条「正文被清洗」的 WARN：只带定位字段与替换规模，**不得**
// 带正文（正文可能含用户数据 / 外部页面内容）。会话标识在无状态会话里为空串，
// 那正是"不落库"的如实读数。
// warnReplacedText logs the wash with locating fields and a byte count only.
func (s *Session) warnReplacedText(runID, role, toolCallID, field string, replaced int) {
	slog.Default().Warn("消息正文含无法承载的字节，已替换为 U+FFFD",
		"session_id", s.sessionID,
		"run_id", runID,
		"role", role,
		"tool_call_id", toolCallID,
		"field", field,
		"replaced_bytes", replaced)
}

// SanitizeMessage 就地清洗一条消息的正文字段（`Content` / `ReasoningContent`），对每个
// **被改写**的字段调用 warn（可为 nil）——warn 只收字段名与替换字节数，日志的定位字段由
// 调用方补。清洗判据见 `sanitizeMessageText`。
//
// 导出是为了让**存储层的唯一插入点**（`messages` 表的 shared INSERT）复用同一份判据：
// 崩溃恢复的半截组补齐在事务内直写消息、不经过本类型的追加方法，从审计行还原的外部
// 结果同样可能带 NUL（2026-10-31 走查实况：启动恢复以 `SQLSTATE 22021` 失败，重路径
// 装配失败、服务起不来）。清洗做在插入点 = 任何调用方都落不了坏字节。
// Exported so the storage layer's single INSERT reuses this one criterion; recovery writes
// inside its transaction without going through this session type.
func SanitizeMessage(m *Message, warn func(field string, replaced int)) {
	if text, replaced := sanitizeMessageText(m.Content); replaced > 0 {
		m.Content = text
		if warn != nil {
			warn("content", replaced)
		}
	}
	if text, replaced := sanitizeMessageText(m.ReasoningContent); replaced > 0 {
		m.ReasoningContent = text
		if warn != nil {
			warn("reasoning_content", replaced)
		}
	}
}

// sanitizeMessageText 把消息正文里**存储与协议都无法承载**的字节替换成 U+FFFD，
// 返回替换后的文本与被替换的字节数（0 = 原文未变）。两类字节：
//
//   - **NUL（0x00）**：PostgreSQL 的 `text` 列在任何编码下都不能存 0x00；
//   - **非法 UTF-8 字节序列**：UTF8 编码的库要求参数是合法编码（`SQLSTATE 22021`）。
//
// 为什么替换而不是拒绝：这类字节来自**外部工具结果**（抓回的网页正文、MCP 回传），
// 不是客户端请求形状——为它让整轮运行失败，等于让一个网页把 Agent 打死；模型协议
// 同样只接受 UTF-8 文本。"宽容解码、把不可读处标成替换字符"与文件读取的口径一致
// （docs/contracts/agent-md.md：非法字节以替换字符呈现，不因编码拒绝读取）。
// 审计的 `tool_invocations.result_json` 保留原文（那是"工具实际返回了什么"的证据），
// 本条只治理会话消息——内存历史与落库正文因此始终是同一份文本（单一追加路径）。
// The wash is lossy on purpose and applies to every append; the audit copy stays
// verbatim because it records what the tool actually returned.
func sanitizeMessageText(text string) (string, int) {
	// 快路径：绝大多数正文本来就干净（一次扫描）。
	if utf8.ValidString(text) && !strings.Contains(text, "\x00") {
		return text, 0
	}
	var b strings.Builder
	b.Grow(len(text))
	replaced := 0
	for i := 0; i < len(text); {
		if text[i] == 0x00 {
			b.WriteRune(utf8.RuneError)
			replaced++
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			// 非法字节：每个坏字节换一个替换字符（与 strings.ToValidUTF8 同口径）。
			b.WriteRune(utf8.RuneError)
			replaced++
			i++
			continue
		}
		b.WriteString(text[i : i+size])
		i += size
	}
	return b.String(), replaced
}

// AppendToolResult 追加一条工具结果消息（由循环在工具执行后调用——**循环是唯一追加者**）。
// AppendToolResult appends one tool result message; the loop is its only caller.
func (s *Session) AppendToolResult(ctx context.Context, runID string, call ToolCall, r Result) error {
	// 来源引用随 tool 消息同事务落 `message_evidence_refs`：它是"这条结果依赖哪些
	// 知识库来源"的唯一载体，与 assistant 消息的引用集合共同构成压缩时的失效判定面
	// （C-26：来源依赖必须随转述传播）。
	// Evidence refs travel with the tool message into message_evidence_refs; together
	// with the assistant's refs they form the invalidation surface for compaction.
	return s.AppendMessage(ctx, runID, Message{
		Role:         RoleTool,
		Content:      r.Content,
		ToolCallID:   call.ID,
		EvidenceRefs: r.EvidenceRefs,
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
