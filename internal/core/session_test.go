package core_test

// 本文件是流程 E（半截工具组补齐）的专属判卷面——E1~E8 全落此处（`CompleteToolGroup`
// 是 `Session` 的方法，不挂 tool_exec_test.go）。替身家族是**本文件自带**的
// `sessionStore` / `lookupAudit`：它们要记录的读数（写入时的 `runID`、上下文的剩余
// 时限、审计查询序）比 loop_test.go 的通用替身更细，判据因此不依赖别处夹具的演进。
// Flow E regression tests (E1~E8) with a local fake family: these stubs record the
// readings this flow judges on — the run id carried by each write, the remaining
// deadline on the context, and the order of audit lookups.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// appendedMessage 是一次写透的量：会话 / 运行标识 + 消息本身。
type appendedMessage struct {
	sessionID string
	runID     string
	msg       core.Message
}

// sessionStore 是流程 E 判卷用的会话仓储替身（`core.MessageStore`）：
// 记录写入序、写入时的上下文读数，并可按注入项造失败 / 尊重取消。
// sessionStore records every write together with the context readings the finish
// path is judged on.
type sessionStore struct {
	mu sync.Mutex

	appended          []appendedMessage
	history           []core.Message
	failNext          error           // 一次性注入写入失败（E6）
	respectCtx        bool            // 为真时按 ctx 取消状态拒写（E7：证明写入不依赖父 ctx）
	remainingDeadline []time.Duration // 每次写入时 ctx 的剩余时限；无 deadline 记 -1
}

// AppendMessage 记录一次写入；注入失败时**不写**（与真仓储同语义）。
func (s *sessionStore) AppendMessage(ctx context.Context, sessionID, runID string, m core.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	remaining := time.Duration(-1)
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	s.remainingDeadline = append(s.remainingDeadline, remaining)

	if s.respectCtx {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return err
	}
	s.appended = append(s.appended, appendedMessage{sessionID: sessionID, runID: runID, msg: m})
	s.history = append(s.history, m)
	return nil
}

// Messages 取整段历史（升序副本）。
func (s *sessionStore) Messages(_ context.Context, _ string) ([]core.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.Message, len(s.history))
	copy(out, s.history)
	return out, nil
}

// UpsertSession 是陪衬面（流程 E 不判它）。
func (s *sessionStore) UpsertSession(_ context.Context, _ core.SessionRecord) error { return nil }

// ListSessions 是陪衬面（流程 E 不判它）。
func (s *sessionStore) ListSessions(_ context.Context, _ core.SessionFilter) ([]core.SessionRecord, error) {
	return nil, nil
}

// count 返回已落库的消息条数。
func (s *sessionStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.appended)
}

// appendedMessages 返回写入序的副本。
func (s *sessionStore) appendedMessages() []appendedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]appendedMessage, len(s.appended))
	copy(out, s.appended)
	return out
}

// lastContent 返回最后一条落库消息的正文。
func (s *sessionStore) lastContent(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.appended, "尚无任何写入")
	return s.appended[len(s.appended)-1].msg.Content
}

// remainingTimes 返回每次写入时 ctx 的剩余时限（无 deadline 记 -1）。
func (s *sessionStore) remainingTimes() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, len(s.remainingDeadline))
	copy(out, s.remainingDeadline)
	return out
}

// lookupAudit 是补齐分类用的审计只读替身：按 (runID, toolCallID) 命中预置行，
// 并把**查询序**记下来（E5 的顺序断言判据）。
// lookupAudit records the lookup order so E5 can assert that classification never
// runs ahead of the missing-result check.
type lookupAudit struct {
	mu sync.Mutex

	rows   map[string]core.ToolInvocationView // 键：runID + "|" + toolCallID
	err    error
	looked []string
}

// Lookup 查一行并记序；`err` 非空时一律失败（E 族不判它，留给调用方用例）。
func (a *lookupAudit) Lookup(_ context.Context, runID, toolCallID string) (core.ToolInvocationView, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := runID + "|" + toolCallID
	a.looked = append(a.looked, key)
	if a.err != nil {
		return core.ToolInvocationView{}, false, a.err
	}
	view, ok := a.rows[key]
	return view, ok, nil
}

// lookedUp 返回查询序的副本。
func (a *lookupAudit) lookedUp() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.looked))
	copy(out, a.looked)
	return out
}

// 中文验收点（E1）：`AppendMessage` 把 `runID` 原样透传进仓储——消息本身没有该字段，
// 恢复却依赖 `messages.run_id` 落列；内存历史取的是副本，调用方改不动会话状态。
// Acceptance point (E1): the run id reaches the store verbatim, and Messages() hands
// out a copy.
func TestSession_AppendMessageCarriesRunID(t *testing.T) {
	store := &sessionStore{}
	sess := core.NewSession("cli-3f9a1c07b2d84e55", store, nil)

	require.NoError(t, sess.AppendMessage(context.Background(), "cli-1776000000123-abc123",
		core.Message{Role: core.RoleUser, Content: "你好"}))

	appended := store.appendedMessages()
	require.Len(t, appended, 1)
	require.Equal(t, "cli-3f9a1c07b2d84e55", appended[0].sessionID)
	require.Equal(t, "cli-1776000000123-abc123", appended[0].runID, "运行标识显式入列")
	require.Equal(t, "你好", appended[0].msg.Content)

	snapshot := sess.Messages()
	require.Len(t, snapshot, 1)
	snapshot[0].Content = "改内存"
	require.Equal(t, "你好", sess.Messages()[0].Content, "Messages 返回副本，改不动会话状态")
}

// 中文验收点（E2）：判据**只看消息**——某个调用有没有一条 `role=tool` 且 id 相同的
// 消息；工具名相同不得互相顶替、空 id 不答任何调用、非 tool 角色不参与判定，
// 返回序保持声明序。
// Acceptance point (E2): the criterion is purely the messages of the window handed in.
func TestSession_MissingToolResultsOnlyTrustsMessages(t *testing.T) {
	calls := []core.ToolCall{
		{ID: "c1", Name: "read_file"},
		{ID: "c2", Name: "read_file"},
		{ID: "c3", Name: "write_file"},
	}
	window := []core.Message{
		{Role: core.RoleAssistant, Content: "", ToolCalls: calls},
		{Role: core.RoleTool, Content: "第二件的结果", ToolCallID: "c2"},
		{Role: core.RoleTool, Content: "无 id 的结果", ToolCallID: ""},
		{Role: core.RoleAssistant, Content: "同名工具不算结果", ToolCallID: "c1"}, // 角色不对
	}

	missing := core.MissingToolResults(calls, window)
	require.Equal(t, []core.ToolCall{calls[0], calls[2]}, missing,
		"逐 id 判、同名不顶替、错误角色不算数、返回序 = 声明序")

	// 人人有结果 → 无缺失（判据正向面）。
	full := append(window,
		core.Message{Role: core.RoleTool, Content: "第一件的结果", ToolCallID: "c1"},
		core.Message{Role: core.RoleTool, Content: "第三件的结果", ToolCallID: "c3"})
	require.Empty(t, core.MissingToolResults(calls, full))

	// 按位置判组的口径：判据只认**传入的这一段窗口**——别的位置（别的 Run）里 id 相同的
	// 结果消息不参与判定，窗口内没有结果就是整组缺失。
	bare := []core.Message{{Role: core.RoleAssistant, Content: "", ToolCalls: calls}}
	require.Equal(t, calls, core.MissingToolResults(calls, bare),
		"窗口内无结果消息 → 整组缺失（判组按窗口位置，不按全会话 id 大扫）")
}

// 中文验收点（E3）：补齐分类的五分支逐条——**先查审计行**：终态行还原真实结果；
// `pending`（执行过但终态回填失败）与 `unknown` 一律"结果未知"（**不得**写成"未执行"）；
// 无行才按原因取取消 / 预算 / 中断三条文本。
// Acceptance point (E3): the five branches in order, with the honesty rule that an
// executed-but-unlanded call is never reported as "not executed".
func TestSession_CompleteToolGroupSynthesizesFiveBranches(t *testing.T) {
	var logBuf strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx := context.Background()
	runID := "cli-1776000000123-abc123"
	call := core.ToolCall{ID: "c1", Name: "read_file"}
	restorable := json.RawMessage(`{"content":"笔记正文","retryable":false}`)

	cases := []struct {
		name   string
		reason string
		row    *core.ToolInvocationView // nil = 无审计行
		want   string
	}{
		{
			"分支①终态行 success → 还原真实结果", core.ReasonCancelled,
			&core.ToolInvocationView{Status: core.LLMCallSuccess, ResultJSON: restorable}, "笔记正文",
		},
		{
			"分支①终态行 failed → 同样还原行内结果", core.ReasonToolBudget,
			&core.ToolInvocationView{Status: core.LLMCallFailed, ResultJSON: json.RawMessage(`{"content":"权限不足","retryable":true}`)}, "权限不足",
		},
		{
			"分支②pending（执行过但回填失败）→ 结果未知", core.ReasonToolBudget,
			&core.ToolInvocationView{Status: core.LLMCallPending}, core.TextResultUnknown,
		},
		{
			"分支②unknown（恢复扫描归一化）→ 结果未知", core.ReasonRunFailed,
			&core.ToolInvocationView{Status: core.LLMCallUnknown}, core.TextResultUnknown,
		},
		{"分支③无行 + 取消 → 已取消", core.ReasonCancelled, nil, core.TextToolCancelled},
		{"分支④无行 + 预算 → 未执行（次数超限）", core.ReasonToolBudget, nil, core.TextToolBudget},
		{"分支⑤无行 + 崩溃 → 未执行（进程中断）", core.ReasonRunFailed, nil, core.TextToolInterrupted},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			audit := &lookupAudit{rows: map[string]core.ToolInvocationView{}}
			if tc.row != nil {
				audit.rows[runID+"|"+call.ID] = *tc.row
			}
			store := &sessionStore{}
			sess := core.NewSession("cli-3f9a1c07b2d84e55", store, audit)

			require.NoError(t, sess.CompleteToolGroup(ctx, runID, []core.ToolCall{call}, tc.reason))
			require.Equal(t, tc.want, store.lastContent(t))
			require.Equal(t, 1, store.count(), "只补了缺失的那一条")

			if tc.row != nil && (tc.row.Status == core.LLMCallPending || tc.row.Status == core.LLMCallUnknown) {
				require.NotEqual(t, core.TextToolInterrupted, store.lastContent(t),
					"执行过的调用绝不得写成『未执行』")
				require.Equal(t, "结果未知（进程中断，无法确定是否已产生外部副作用）",
					store.lastContent(t), "逐字取契约 §4.4 的文本")
			}
		})
	}

	require.Contains(t, logBuf.String(), "工具结果未知（终态审计行缺失）",
		"结果未知分支同时经 WARN 留痕（可观测性双轨）")
}

// 中文验收点（E4）：幂等——组已完整即 no-op、重复调用不重复插入；
// 冗余调用连带把"审计一次都不查"的守卫一并钉住。
// Acceptance point (E4): completion is idempotent because its criterion lives on the
// messages, not on a write log.
func TestSession_CompleteToolGroupIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := &sessionStore{}
	audit := &lookupAudit{rows: map[string]core.ToolInvocationView{}}
	sess := core.NewSession("cli-3f9a1c07b2d84e55", store, audit)

	calls := []core.ToolCall{
		{ID: "c1", Name: "read_file"},
		{ID: "c2", Name: "read_file"},
	}
	require.NoError(t, sess.AppendToolResult(ctx, "r1", calls[0], core.Result{Content: "第一件的真实结果"}))

	require.NoError(t, sess.CompleteToolGroup(ctx, "r1", calls, core.ReasonCancelled))
	require.Equal(t, 2, store.count(), "c1 真实结果 + c2 合成结果 = 2 条")

	require.NoError(t, sess.CompleteToolGroup(ctx, "r1", calls, core.ReasonCancelled))
	require.Equal(t, 2, store.count(), "重复调用不重复插入")

	require.Equal(t, []string{"r1|c2"}, audit.lookedUp(),
		"只对缺失的调用查一次审计——组已完整的那一轮零查询（先判缺失的守卫）")
	require.Len(t, sess.Messages(), 2, "内存历史同样不涨")
}

// 中文验收点（E5）：**先判缺失、再分类**——审计查询**只**发生在缺失的调用上、
// 且按声明序；已有真实结果的调用既不再查审计，其真实结果也不会被合成文本顶掉。
// Acceptance point (E5): classification runs only for calls that lack a result
// message, in declaration order.
func TestSession_CompleteToolGroupClassifiesOnlyMissingCalls(t *testing.T) {
	ctx := context.Background()
	runID := "cli-1776000000123-abc123"
	calls := []core.ToolCall{
		{ID: "c1", Name: "read_file"},
		{ID: "c2", Name: "read_file"},
		{ID: "c3", Name: "list_dir"},
	}
	audit := &lookupAudit{rows: map[string]core.ToolInvocationView{
		runID + "|c2": {Status: core.LLMCallSuccess, ResultJSON: json.RawMessage(`{"content":"第二件的真实结果"}`)},
	}}
	store := &sessionStore{}
	sess := core.NewSession("cli-3f9a1c07b2d84e55", store, audit)

	// c1 已有一条真实结果消息（循环当场回填的那种）。
	require.NoError(t, sess.AppendToolResult(ctx, runID, calls[0], core.Result{Content: "第一件的真实结果"}))

	require.NoError(t, sess.CompleteToolGroup(ctx, runID, calls, core.ReasonCancelled))

	require.Equal(t, []string{runID + "|c2", runID + "|c3"}, audit.lookedUp(),
		"审计查询只认缺失调用、按声明序——分类若先于缺失判定，c1 会多出一条查询")
	require.Equal(t, 3, store.count())

	msgs := sess.Messages()
	require.Equal(t, "第一件的真实结果", msgs[0].Content, "真实结果不被合成文本顶掉")
	require.Equal(t, "第二件的真实结果", msgs[1].Content)
	require.Equal(t, core.TextToolCancelled, msgs[2].Content)
}

// 中文验收点（E6）：补齐失败**如实返回 error**（写失败一条不吞）——调用方据此记 ERROR
// 并让 Run 照常收场；失败的消息不进内存（不得只存在于内存）；存储恢复后重试即补齐。
// Acceptance point (E6): a write failure surfaces as an error, and the failed message
// never lands in memory alone.
func TestSession_CompleteToolGroupReturnsWriteFailure(t *testing.T) {
	ctx := context.Background()
	store := &sessionStore{failNext: errors.New("注入：会话仓储不可用")}
	sess := core.NewSession("cli-3f9a1c07b2d84e55", store, nil) // audit 为空：一律走"无行"分支（不猜）

	calls := []core.ToolCall{{ID: "c1", Name: "read_file"}}
	err := sess.CompleteToolGroup(ctx, "r1", calls, core.ReasonRunFailed)
	require.Error(t, err, "补齐失败必须返回 error（调用方记 ERROR，Run 照常收尾）")
	require.Contains(t, err.Error(), "会话仓储不可用")
	require.Zero(t, store.count())
	require.Empty(t, sess.Messages(), "写失败的消息不得只存在于内存")

	require.NoError(t, sess.CompleteToolGroup(ctx, "r1", calls, core.ReasonRunFailed),
		"存储恢复后重试即补齐（幂等判据在消息上）")
	require.Equal(t, 1, store.count())
	require.Equal(t, core.TextToolInterrupted, store.lastContent(t))
}

// 中文验收点（E7）：**收尾 context 独立**——父 ctx 已取消（取消路径正是调用补齐的时刻）
// 仍必须写得进去，因为写入用的是剥掉取消信号、带有限时限的派生上下文；
// 时限有上界（不超过 5 秒），既不随父取消、也不无限期拖住退出。
// Acceptance point (E7): finish-order writes keep working after the parent context is
// cancelled, bounded by a finite timeout.
func TestSession_CompleteToolGroupUsesDetachedFinishContext(t *testing.T) {
	store := &sessionStore{respectCtx: true} // 尊重取消的仓储：父 ctx 直接用来写必失败
	sess := core.NewSession("cli-3f9a1c07b2d84e55", store, nil)

	require.NoError(t, sess.AppendMessage(context.Background(), "r1",
		core.Message{Role: core.RoleUser, Content: "读文件"}))

	parent, cancel := context.WithCancel(context.Background())
	cancel() // 父上下文已取消

	require.NoError(t, sess.CompleteToolGroup(parent, "r1",
		[]core.ToolCall{{ID: "c1", Name: "read_file"}}, core.ReasonCancelled),
		"父 ctx 已取消仍必须写得进去（收尾 context 独立）")
	require.Equal(t, 2, store.count())
	require.Equal(t, core.TextToolCancelled, store.lastContent(t))

	for i, remaining := range store.remainingTimes() {
		require.Greater(t, remaining, time.Duration(0), "第 %d 次写入应带剩余时限", i)
		require.LessOrEqual(t, remaining, 5*time.Second,
			"第 %d 次写入的时限不超过 finishTimeout（5 秒）", i)
	}
}

// 中文验收点（E8）：无状态会话（空会话标识）只在内存——不落会话历史、Load 是 no-op；
// 会话标识有而仓储为空同样按无状态处置（宁可不落，不猜落哪）。
// Acceptance point (E8): a stateless session keeps everything in memory.
func TestSession_StatelessSessionStaysInMemory(t *testing.T) {
	ctx := context.Background()

	store := &sessionStore{}
	sess := core.NewSession("", store, nil) // 空会话标识 = 无状态
	require.NoError(t, sess.AppendMessage(ctx, "r1", core.Message{Role: core.RoleUser, Content: "你好"}))
	require.NoError(t, sess.CompleteToolGroup(ctx, "r1",
		[]core.ToolCall{{ID: "c1", Name: "read_file"}}, core.ReasonRunFailed))

	require.Zero(t, store.count(), "无状态会话不落会话历史（Run 与审计仍照常落库——不归本层）")
	require.Len(t, sess.Messages(), 2, "历史只在内存")
	require.NoError(t, sess.Load(ctx), "无状态会话的 Load 是 no-op")
	require.Len(t, sess.Messages(), 2)

	second := core.NewSession("cli-3f9a1c07b2d84e55", nil, nil) // 无仓储同样按无状态处置
	require.NoError(t, second.AppendMessage(ctx, "r1", core.Message{Role: core.RoleUser, Content: "你好"}))
	require.Len(t, second.Messages(), 1)
}
