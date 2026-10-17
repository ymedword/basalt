package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是工具调度面（US2 的 B 组）的验收测试：逐条判卷 §2.3 流程 D 的八步——
// 两道执行前拒绝（名字与归属）、fail-closed 预写、终态回填的残余窗口、七类 error_kind 的
// 分类口径、截断与结果包装。B1 / B3 两条是 §4.3 写出代码的**原样落地**（经唯一运行入口驱动，
// 断言逐条保真）；其余各支走本文件的单元夹具，直接驱动 core.ToolExecutor.Execute——
// 逐分支摆位不需要每次都搭一整条循环。
// This file holds the tool-dispatch acceptance tests (B group) covering flow D end to end:
// the two pre-execution refusals, the fail-closed pre-write, the terminal-write residual
// window, error_kind classification, truncation and result packing. B1/B3 land the §4.3
// regression code verbatim through the single run entry point; the rest use a unit fixture.

// execToolSchema 是假工具的入参 schema（带必填字段——B7 的"必填缺失"分支要用它）。
// execToolSchema is the fake tool's input schema; the required field feeds B7's branch.
const execToolSchema = `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`

// execSessionID / execRunID 是单元夹具的会话与运行标识（形态与 §6.2 固定口径 1 的派生结果同构）。
// execSessionID and execRunID are the unit fixture's identifiers, shaped like the real ones.
const (
	execSessionID = "cli-5b8e0d1c7a4f2963"
	execRunID     = "cli-1760616000000-k3m7q2"
)

// execToolSet 是本 Agent 声明的工具名集（流程 D ① 的运行时归属判据）。
// execToolSet is the agent-declared tool name set checked at dispatch time.
var execToolSet = []string{"read_file"}

// execTool 是可按用例脚本返回的假工具：默认回一条成功结果，用例可换上错误、自定义执行体
// 或长正文——流程 D 的每个分支都靠它摆位。
// execTool is a scripted fake tool: a success by default, overridable per case.
type execTool struct {
	name   string
	schema json.RawMessage
	result core.Result
	err    error
	exec   func(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error)

	mu       sync.Mutex
	executed int
}

// Name 是注册名（与调用名逐字一致）。
func (s *execTool) Name() string { return s.name }

// Description 是段 4 用的描述。
func (s *execTool) Description() string { return "流程 D 判卷用假工具 " + s.name }

// InputSchema 是段 4 用的参数 schema（入参必填校验也读它）。
func (s *execTool) InputSchema() json.RawMessage { return s.schema }

// Execute 记一次执行，随后走用例脚本或默认成功路径。
func (s *execTool) Execute(execCtx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	s.mu.Lock()
	s.executed++
	hook, res, err := s.exec, s.result, s.err
	s.mu.Unlock()
	if hook != nil {
		return hook(execCtx, env, input)
	}
	return res, err
}

// executedCount 是执行次数——"执行之前的拒绝不产生审计行 / 工具一次都不得执行"的判据面。
func (s *execTool) executedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executed
}

// execPool 是假注册池（实现 core.ToolResolver）：查表、不遍历（宪法 III）。
// execPool is the fake registry: a table lookup, never an inferred traversal.
type execPool struct{ tools map[string]core.Tool }

// newExecPool 按传入的工具建池。
func newExecPool(tools ...core.Tool) *execPool {
	pool := &execPool{tools: map[string]core.Tool{}}
	for _, tool := range tools {
		pool.tools[tool.Name()] = tool
	}
	return pool
}

// Resolve 查池；未命中返回 ok=false（不遍历、不推断）。
func (p *execPool) Resolve(name string) (core.Tool, bool) {
	tool, ok := p.tools[name]
	return tool, ok
}

// Specs 按传入名字的**顺序**取规格、未命中的跳过。
func (p *execPool) Specs(names []string) []core.ToolSpec {
	out := make([]core.ToolSpec, 0, len(names))
	for _, name := range names {
		tool, ok := p.tools[name]
		if !ok {
			continue
		}
		out = append(out, core.ToolSpec{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  tool.InputSchema(),
		})
	}
	return out
}

// execRow 是假审计仓储里的一行：预写形态 + 终态（含"终态没写进去"这个状态本身）。
// execRow is one audit row: the pre-write shape plus the terminal state, if any.
type execRow struct {
	inv      core.ToolInvocation
	status   core.LLMCallStatus // 预写恒为 pending；回填成功后转终态
	result   core.ToolInvocationResult
	terminal bool
}

// execAudit 是两阶段审计的假仓储（实现 core.ToolInvocationStore）：可注入预写失败
// （B3 的 fail-closed）与终态回填失败（B4 的残余窗口）。
// execAudit is the two-phase audit fake with injectable failures on either write.
type execAudit struct {
	mu         sync.Mutex
	rows       []execRow
	failStart  error // 预写失败注入
	failFinish error // 终态回填失败注入
}

// BeginInvocation 写一行 pending；注入失败时**不写行**（与契约"预写失败即无动作"同源）。
func (a *execAudit) BeginInvocation(_ context.Context, inv core.ToolInvocation) (int64, error) {
	if a.failStart != nil {
		return 0, a.failStart
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, execRow{inv: inv, status: core.LLMCallPending})
	return int64(len(a.rows)), nil
}

// FinishInvocation 把同一行更新为终态；注入失败时行**留在 pending**、结果不得被伪造。
func (a *execAudit) FinishInvocation(_ context.Context, id int64, res core.ToolInvocationResult) error {
	if a.failFinish != nil {
		return a.failFinish
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if id < 1 || int(id) > len(a.rows) {
		return fmt.Errorf("tool invocation row %d not found", id)
	}
	a.rows[id-1].status = res.Status
	a.rows[id-1].result = res
	a.rows[id-1].terminal = true
	return nil
}

// count 是审计行数（0 行 = 执行之前的拒绝）。
func (a *execAudit) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.rows)
}

// row 取第 i 行（0 起）。
func (a *execAudit) row(i int) execRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rows[i]
}

// execFixture 是流程 D 的面板夹具：一只带必填 schema 的假工具 + 假注册池 + 假审计 +
// 可读日志缓冲，直接驱动 core.ToolExecutor.Execute。
// execFixture wires one fake tool, pool, audit and log buffer around the executor.
type execFixture struct {
	audit    *execAudit
	tool     *execTool
	logs     *bytes.Buffer
	executor *core.ToolExecutor
	rctx     context.Context
}

// newExecFixture 组装夹具（用例之间零共享状态）。
func newExecFixture(t *testing.T) *execFixture {
	t.Helper()
	tool := &execTool{
		name:   "read_file",
		schema: json.RawMessage(execToolSchema),
		result: core.Result{Content: "【read_file 执行结果】"},
	}
	audit := &execAudit{}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &execFixture{
		audit:    audit,
		tool:     tool,
		logs:     logs,
		executor: core.NewToolExecutor(newExecPool(tool), audit, logger),
		rctx:     context.Background(),
	}
}

// env 是本轮的工具执行环境（值传递；个别用例自行收紧 Timeout / MaxOutputBytes）。
func (f *execFixture) env() core.ToolEnv {
	return core.ToolEnv{
		RunID:          execRunID,
		SessionID:      execSessionID,
		AgentName:      "demo",
		AgentDir:       `D:\workspace\agents\demo`,
		Timeout:        30 * time.Second,
		MaxOutputBytes: 256 * 1024,
	}
}

// execCall 组装一次模型给出的工具调用。
func execCall(id, name, args string) core.ToolCall {
	return core.ToolCall{ID: id, Name: name, Arguments: args}
}

// lastInvocation 取库里最后一行工具审计——端到端用例经假仓储读终态（同包测试面）。
func lastInvocation(t *testing.T, store *fakeStore) storedInvocation {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	require.NotEmpty(t, store.invocations, "库里没有工具审计行")
	return store.invocations[len(store.invocations)-1]
}

// TestToolExecutor_RejectsToolOutsideAgentToolSet 守"运行时 ToolSet 归属校验"：
// 验收点——模型返回"全局已注册、该 Agent 未声明"的工具名时，不得执行、不得产生审计行，
// 且回填四段式拒绝文案（加载期验证挡不住注册池与快照不同源的情形）。
func TestToolExecutor_RejectsToolOutsideAgentToolSet(t *testing.T) {
	h := newLoopHarness(t)
	writeFile := &execTool{name: "write_file", schema: json.RawMessage(execToolSchema)}
	h.registry.register(writeFile) // 全局注册，但**不在**本 Agent 的 ToolSet
	h.toolSet = []string{"read_file"}
	h.provider.onCall(0, toolCallResponse("c1", "write_file", `{"path":"x","content":"y"}`))
	h.provider.onCall(1, textResponse("改用其它方式"))
	res, err := h.service.Run(ctx, h.req("写个文件"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Zero(t, writeFile.executedCount(), "未声明的工具不得执行")
	require.Zero(t, h.store.countToolInvocations(t), "执行之前的拒绝不产生审计行")
	refusal := h.store.toolContent(t, h.sessionID, "c1")
	require.Contains(t, refusal, "tools", "拒绝文案点名 AGENT.md 的 tools 声明（四段式放行指引）")
	require.Contains(t, refusal, "工具未在本 Agent 的 tools 声明中", "四段式第 1 段：点名规则")
	require.Contains(t, refusal, "这是安全策略，请勿反复重试", "四段式第 3 段是固定句")
}

// TestToolExecutor_UnknownToolNameLeavesNoAuditRow 守"名字不在注册池"那一支：
// 验收点（§2.3 流程 D ①）——回填**契约既有字面量** `tool not registered: <name>`；
// 不得执行、不得产生审计行。四段式只属"归属拒绝"那一支（§3.2 的摘要措辞以流程 D 为准）。
func TestToolExecutor_UnknownToolNameLeavesNoAuditRow(t *testing.T) {
	f := newExecFixture(t)
	res := f.executor.Execute(f.rctx, f.env(), execToolSet, execCall("c1", "shell", `{"cmd":"ls"}`))
	require.Equal(t, "tool not registered: shell", res.Content, "回填契约既有字面量（与加载期门禁同一句）")
	require.Zero(t, f.audit.count(), "执行之前的拒绝不产生审计行")
	require.Zero(t, f.tool.executedCount())
}

// TestToolExecutor_PreWriteFailureRefusesExecution 守"审计 fail-closed"（宪法 V + 契约 §7.2 工具侧）：
// 验收点——审计 pending 预写失败时，工具**一次都不得执行**；但**Run 不因此失败**：
// 失败结果必须回填模型并说明审计不可用（模型看到失败 -> 下一轮收敛为最终回答）。
func TestToolExecutor_PreWriteFailureRefusesExecution(t *testing.T) {
	h := newLoopHarness(t)
	h.audit.failStart = errors.New("disk full") // 注入预写失败
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("工具不可用，我直接回答"))
	res, err := h.service.Run(ctx, h.req("读文件"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "工具审计不可用不判 Run 失败（契约 §7.2）")
	require.Zero(t, h.tools.executed, "预写失败时工具不得执行（fail-closed）")
	require.Zero(t, h.store.countToolInvocations(t))
	require.Contains(t, h.store.toolContent(t, h.sessionID, "c1"), "审计不可用",
		"失败信息必须回填进对话上下文（模型看不到结果就不会收敛）")
	require.Contains(t, h.logs.String(), "工具审计预写失败", "预写失败记 ERROR（可观测性双轨）")
}

// TestToolExecutor_TerminalWriteFailureKeepsPendingRow 守"终态回填失败的残余窗口"（契约 §7.2）：
// 验收点——回填失败时行**留在 pending**（不得伪造终态、不得覆盖真实结果）并记 ERROR；
// 而**结果照常回给模型**（修复历史不能以扣下答案为代价）。
func TestToolExecutor_TerminalWriteFailureKeepsPendingRow(t *testing.T) {
	f := newExecFixture(t)
	f.audit.failFinish = errors.New("injected: finish write failed")
	res := f.executor.Execute(f.rctx, f.env(), execToolSet, execCall("c1", "read_file", `{"path":"notes.txt"}`))
	require.Equal(t, "【read_file 执行结果】", res.Content, "回填失败不得扣下结果")
	require.Equal(t, 1, f.tool.executedCount(), "工具确实执行过——故必须留行")

	row := f.audit.row(0)
	require.False(t, row.terminal, "回填失败 → 行留在 pending，由下次启动的恢复扫描归一化")
	require.Equal(t, core.LLMCallPending, row.status, "不得伪造终态")
	require.Empty(t, row.result.Status, "不得覆盖真实结果")
	require.Contains(t, f.logs.String(), "工具审计终态回填失败", "回填失败记 ERROR")
}

// TestToolExecutor_SandboxDenialIsAuditedAsSandboxDenied 守"沙箱拒绝发生在执行**之内**"：
// 验收点（流程 D ⑥ + 契约 §7.3）——工具已启动、Policy 判拒 → **有行**、`failed` + `sandbox_denied`；
// 四段式文案原样回填（不得吞掉、不得替换为泛化文案——模型看不到"为什么被拒"就不会收敛）。
func TestToolExecutor_SandboxDenialIsAuditedAsSandboxDenied(t *testing.T) {
	f := newExecFixture(t)
	refusal := &core.SandboxViolationError{
		Rule:     "读白名单未放行",
		Target:   "../../etc/passwd",
		Guidance: "请在 basalt.yaml 的 sandbox 配置中放行该路径后重启实例",
	}
	f.tool.err = refusal
	res := f.executor.Execute(f.rctx, f.env(), execToolSet, execCall("c1", "read_file", `{"path":"../../etc/passwd"}`))
	require.Equal(t, refusal.Error(), res.Content, "拒绝文案原样进工具结果")
	row := f.audit.row(0)
	require.True(t, row.terminal, "执行之内的拒绝有行")
	require.Equal(t, core.LLMCallFailed, row.status)
	require.Equal(t, core.ErrorKindSandboxDenied, row.result.ErrorKind)
	require.Contains(t, f.logs.String(), "reason=sandbox_denied", "失败路径 WARN 与审计落库构成双轨")
}

// TestToolExecutor_ToolTimeoutKeepsRunAlive 守"单工具超时是工具级失败、不是运行级故障"：
// 验收点（流程 D ⑤⑥⑧ + 契约 §7.3）——超时**实际建立**在 timeout 上下文上、审计落 `failed` + `timeout`，
// 而 Run 照常收敛为 completed（失败信息回填后模型换条路走）。
func TestToolExecutor_ToolTimeoutKeepsRunAlive(t *testing.T) {
	h := newLoopHarness(t)
	h.budget.ToolTimeout = 20 * time.Millisecond
	blocking := &execTool{
		name:   "read_file",
		schema: json.RawMessage(execToolSchema),
		exec: func(execCtx context.Context, _ core.ToolEnv, _ json.RawMessage) (core.Result, error) {
			<-execCtx.Done() // 等超时上下文到期（不 sleep 赌时序）
			return core.Result{}, execCtx.Err()
		},
	}
	h.registry.register(blocking) // 覆盖 harness 缺省的 read_file
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("读超时了，我直接回答"))
	res, err := h.service.Run(ctx, h.req("读文件"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "单工具超时不判 Run 失败（流程 D ⑧）")
	require.Equal(t, 1, blocking.executedCount())
	row := lastInvocation(t, h.store)
	require.Equal(t, core.LLMCallFailed, row.status)
	require.Equal(t, core.ErrorKindTimeout, row.result.ErrorKind)
	require.Contains(t, h.store.toolContent(t, h.sessionID, "c1"), context.DeadlineExceeded.Error(),
		"失败信息必须回填（模型看到超时才知道换条路）")
	require.Contains(t, h.logs.String(), "reason=timeout")
}

// TestToolExecutor_InvalidInputStillWritesAuditRow 守"入参非法仍写审计行"：
// 验收点（流程 D ② + 契约 §7.3）——非法 JSON / 必填缺失 → `failed` + `invalid_input`，
// **仍写审计行**（工具确实被调用了，只是没被执行）；工具本体一次都不得进入。
func TestToolExecutor_InvalidInputStillWritesAuditRow(t *testing.T) {
	f := newExecFixture(t)
	env := f.env()

	broken := f.executor.Execute(f.rctx, env, execToolSet, execCall("c1", "read_file", `{"path":`))
	require.Contains(t, broken.Content, "入参不是合法 JSON")
	missing := f.executor.Execute(f.rctx, env, execToolSet, execCall("c2", "read_file", `{}`))
	require.Contains(t, missing.Content, "入参缺少必填字段 path")

	require.Equal(t, 2, f.audit.count(), "非法入参**仍写审计行**（两次调用两行）")
	for i, wantID := range []string{"c1", "c2"} {
		row := f.audit.row(i)
		require.True(t, row.terminal)
		require.Equal(t, core.LLMCallFailed, row.status)
		require.Equal(t, core.ErrorKindInvalidInput, row.result.ErrorKind)
		require.Equal(t, wantID, row.inv.ToolCallID)
	}
	require.Zero(t, f.tool.executedCount(), "入参校验在工具之外——工具本体不得被调用")
}

// TestToolExecutor_ClassifiesNotFoundAndRuntimeError 守"失败分类走错误链"：
// 验收点（流程 D ⑥ + 契约 §7.3）——文件 / 脚本不存在（`fs.ErrNotExist`，**含包装**）→ `not_found`；
// 其余工具内部错误（进程启动失败等）→ `runtime_error`；判定不得比对错误文本。
func TestToolExecutor_ClassifiesNotFoundAndRuntimeError(t *testing.T) {
	f := newExecFixture(t)
	env := f.env()

	f.tool.err = fmt.Errorf("open notes.txt: %w", fs.ErrNotExist)
	notFound := f.executor.Execute(f.rctx, env, execToolSet, execCall("c1", "read_file", `{"path":"notes.txt"}`))
	require.Contains(t, notFound.Content, "notes.txt", "失败原文回填")
	require.Equal(t, core.ErrorKindNotFound, f.audit.row(0).result.ErrorKind, "包装过的 not-exist 也要认")

	f.tool.err = errors.New("进程启动失败：exec format error")
	_ = f.executor.Execute(f.rctx, env, execToolSet, execCall("c2", "read_file", `{"path":"x.sh"}`))
	require.Equal(t, core.ErrorKindRuntimeError, f.audit.row(1).result.ErrorKind, "其余归 runtime_error")
}

// TestToolExecutor_KeepsToolCallIDAndReasoningInHistory 守"消息模型三条硬约束"（技术方案 §2.3）：
// 验收点——`tool_call_id` 原样贯通"模型给出 → 审计行 → 回填消息 → 下一轮出站"，
// `reasoning_content` 随助手消息落库并进下一轮请求（丢了供应商会 400）。
func TestToolExecutor_KeepsToolCallIDAndReasoningInHistory(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, core.ChatResponse{
		ReasoningContent: "先看看 notes.txt 里有什么",
		ToolCalls:        []core.ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"notes.txt"}`}},
	})
	h.provider.onCall(1, textResponse("总结如下"))
	res, err := h.service.Run(ctx, h.req("读一下 notes.txt"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	msgs := h.store.messages(t, h.sessionID)
	require.Len(t, msgs, 4, "user + assistant(工具调用) + tool + assistant(最终回答)")
	require.Equal(t, "先看看 notes.txt 里有什么", msgs[1].ReasoningContent, "思考内容随助手消息落库")
	require.Equal(t, "c1", msgs[1].ToolCalls[0].ID)
	require.Equal(t, "c1", msgs[2].ToolCallID, "回填消息的 tool_call_id 与模型给的一致")
	// 出站消息 = [段 1 system] + 历史，故按角色找助手消息（不按下标硬编码）。
	var roundTwoReasoning string
	for _, m := range h.provider.request(1).Messages {
		if m.Role == core.RoleAssistant {
			roundTwoReasoning = m.ReasoningContent
		}
	}
	require.Equal(t, "先看看 notes.txt 里有什么", roundTwoReasoning,
		"第二轮出站请求必须带思考内容（清空即复现供应商 400——Compatibility §5.3 判据 3）")

	row := lastInvocation(t, h.store)
	require.Equal(t, "c1", row.inv.ToolCallID, "审计详情里的调用 id 原样取自模型（不得改写 / 重编号）")
	require.Equal(t, "read_file", row.inv.ToolName)
	require.Equal(t, `{"path":"notes.txt"}`, string(row.inv.InputJSON), "入参原样保存（不重编码）")
}

// TestToolExecutor_TruncatesOutputAndPacksFailureText 守"截断与结果包装"：
// 验收点（流程 D ⑥⑦ + §6.2 固定口径 7）——截断标记追加在结果尾部、N = **生效上限**
// （不承诺原始长度）；`truncated` 与内容同在 `result_json` **值内**（列面没有该列）；
// 失败信息必须回填（content = 错误原文）。
func TestToolExecutor_TruncatesOutputAndPacksFailureText(t *testing.T) {
	f := newExecFixture(t)
	env := f.env()
	env.MaxOutputBytes = 16

	f.tool.result = core.Result{Content: strings.Repeat("地", 20)} // 60 字节 > 16 字节上限
	res := f.executor.Execute(f.rctx, env, execToolSet, execCall("c1", "read_file", `{"path":"a.txt"}`))
	require.True(t, strings.HasSuffix(res.Content, core.TruncationMarker(16)), "标记写生效上限")
	require.NotContains(t, res.Content, "60", "不承诺原始长度（标记里不得出现原始字节数）")

	row := f.audit.row(0)
	var shape struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal(row.result.ResultJSON, &shape))
	require.True(t, shape.Truncated, "`truncated` 写在 result_json 值内——列面没有该列")
	require.True(t, strings.HasSuffix(shape.Content, core.TruncationMarker(16)))

	failed := "读文件失败：权限不足"
	f.tool.result = core.Result{}
	f.tool.err = errors.New(failed)
	// 失败段换回缺省上限：截断是**内容**的规则，这一问只判"失败信息必须回填"。
	failRes := f.executor.Execute(f.rctx, f.env(), execToolSet, execCall("c2", "read_file", `{"path":"b.txt"}`))
	require.Equal(t, failed, failRes.Content, "失败信息必须回填（模型看不到失败就不会收敛）")
	require.Equal(t, core.ErrorKindRuntimeError, f.audit.row(1).result.ErrorKind)
}

// —— US-5 追加四条（流程 C 的两条新分支与七值完整性）——
// Four added by US-5: flow C's two new branches plus the seven-value completeness fixture.

// TestToolExecutor_ClassifiesExternalError 守"外部系统错误按类型判卷"：
// 验收点（流程 C 新增分支 + 契约 §7.3）——`*core.ExternalError` → `external_error`；
// 落库的 `error_message` 是**自产安全摘要**（口径 17）：`Cause` 里的原始错误含整条 URL / query，
// 只经 `Unwrap` 供分类与排障，**不进文案**、也不进 WARN 日志（可观测性双轨同一口径）。
func TestToolExecutor_ClassifiesExternalError(t *testing.T) {
	f := newExecFixture(t)
	raw := errors.New(`Get "http://api.example.com/weather?token=secret": connection refused`)
	external := &core.ExternalError{
		Target: "api.example.com",
		Reason: "连接失败：api.example.com",
		Cause:  raw,
	}
	f.tool.err = external

	res := f.executor.Execute(f.rctx, f.env(), execToolSet, execCall("c1", "read_file", `{"path":"x"}`))
	require.Equal(t, "连接失败：api.example.com", res.Content, "回填的是安全摘要（即外部错误文案）")

	row := f.audit.row(0)
	require.True(t, row.terminal, "外部系统错误发生在执行之内——有行")
	require.Equal(t, core.LLMCallFailed, row.status)
	require.Equal(t, core.ErrorKindExternalError, row.result.ErrorKind, "判据只认类型，不比文本")
	require.Equal(t, "连接失败：api.example.com", row.result.ErrorMessage,
		"入库的是自产摘要——原始错误（url.Error 原文含整条 URL）不得进错误文案")
	require.NotContains(t, string(row.result.ResultJSON), "token=secret", "query 不进结果体（口径 17）")
	require.NotContains(t, f.logs.String(), "token=secret", "WARN 的 detail 是安全摘要——原文不得进日志")
	require.ErrorIs(t, external, raw, "Unwrap 穿透：原因链可达（供分类与排障，不进文案）")
	require.Contains(t, f.logs.String(), "reason=external_error", "审计落库与 WARN 构成双轨")
}

// TestToolExecutor_ClassifiesInvalidInputCarrier 守"工具解码点与执行器入口同类别"：
// 验收点（流程 C 新增分支 + 注 11）——工具内部解出 `*core.InvalidInputError`（字段类型不符 /
// 取值为空）与执行器入口的必填校验产出**同一个** `invalid_input`；完整可读文案原样回填。
func TestToolExecutor_ClassifiesInvalidInputCarrier(t *testing.T) {
	f := newExecFixture(t)
	reason := "解析 read_file 入参：json: cannot unmarshal string into Go struct field .path of type string"
	f.tool.err = &core.InvalidInputError{Tool: "read_file", Reason: reason}

	res := f.executor.Execute(f.rctx, f.env(), execToolSet, execCall("c1", "read_file", `{"path":"x"}`))
	require.Equal(t, reason, res.Content, "完整可读文案原样回填给模型")
	require.Equal(t, 1, f.tool.executedCount(), "工具确实被调用过——故必须有审计行")

	row := f.audit.row(0)
	require.True(t, row.terminal)
	require.Equal(t, core.ErrorKindInvalidInput, row.result.ErrorKind, "载体改造后与入口校验同类别")
	require.Equal(t, reason, row.result.ErrorMessage)
	require.Contains(t, f.logs.String(), "reason=invalid_input")
}

// TestToolExecutor_ContextIdentityOutranksExternalError 守"取消 / 截止的身份优先"（约束 20 的第二道防线）：
// 验收点（流程 C 的**判序**）——`ExternalError` 包着 `context.DeadlineExceeded` 时仍归 `timeout`：
// ctx 判定排在外部系统**之前**；即便未来某处把 ctx 错误经 `%w` 包进 `Cause`（或再包一层），
// `errors.Is` 也能穿透判出——先判后包与 Unwrap 两条防线并存。
func TestToolExecutor_ContextIdentityOutranksExternalError(t *testing.T) {
	f := newExecFixture(t)
	env := f.env()

	f.tool.err = &core.ExternalError{
		Target: "api.example.com",
		Reason: "连接失败：api.example.com",
		Cause:  context.DeadlineExceeded,
	}
	_ = f.executor.Execute(f.rctx, env, execToolSet, execCall("c1", "read_file", `{"path":"a.txt"}`))
	require.Equal(t, core.ErrorKindTimeout, f.audit.row(0).result.ErrorKind,
		"出网超时仍归 timeout——外部分支不得截胡 ctx 身份")

	f.tool.err = fmt.Errorf("重试耗尽：%w", &core.ExternalError{
		Target: "api.example.com",
		Reason: "连接失败：api.example.com",
		Cause:  context.Canceled,
	})
	_ = f.executor.Execute(f.rctx, env, execToolSet, execCall("c2", "read_file", `{"path":"b.txt"}`))
	require.Equal(t, core.ErrorKindTimeout, f.audit.row(1).result.ErrorKind,
		"再包一层也要认：ctx 分支在外部系统之前（判序不可交换）")
}

// TestToolExecutor_ClassifiesSevenKindsVerbatim 守"七值字面量完整性"（契约 §7.3 冻结集）：
// 验收点——分类器产出的每一类**逐值对号**（防 `default` 把新增分支吞回 `runtime_error`）；
// 第七值 `unknown` 由**恢复扫描**写入（崩溃窗口的 pending 行），分类器不产出它——该边界同样钉住。
func TestToolExecutor_ClassifiesSevenKindsVerbatim(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want core.ErrorKind
	}{
		{"sandbox_denied", &core.SandboxViolationError{Rule: "读白名单未放行", Target: "x", Guidance: "g"}, core.ErrorKindSandboxDenied},
		{"timeout", context.DeadlineExceeded, core.ErrorKindTimeout},
		{"not_found", fmt.Errorf("open notes.txt: %w", fs.ErrNotExist), core.ErrorKindNotFound},
		{"invalid_input", &core.InvalidInputError{Tool: "read_file", Reason: "r"}, core.ErrorKindInvalidInput},
		{"external_error", &core.ExternalError{Target: "api.example.com", Reason: "HTTP 500：api.example.com"}, core.ErrorKindExternalError},
		{"runtime_error", errors.New("进程启动失败：exec format error"), core.ErrorKindRuntimeError},
	}
	classified := make([]core.ErrorKind, 0, len(cases))
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newExecFixture(t)
			f.tool.err = c.err
			_ = f.executor.Execute(f.rctx, f.env(), execToolSet, execCall("c1", "read_file", `{"path":"a.txt"}`))
			row := f.audit.row(0)
			require.Equal(t, core.LLMCallFailed, row.status)
			require.Equal(t, c.want, row.result.ErrorKind)
			require.Equal(t, c.name, string(row.result.ErrorKind), "字面量与契约 §7.3 逐字一致")
			classified = append(classified, row.result.ErrorKind)
		})
	}
	// 第七值：恢复扫描写的 `unknown`——它不出自分类器，故表里六行都不该是它；
	// 同时钉住该字面量本身（七值集完整，改字面量即改契约）。
	require.NotContains(t, classified, core.ErrorKindUnknown, "分类器不得产出 unknown（那由崩溃恢复写）")
	require.Equal(t, "unknown", string(core.ErrorKindUnknown))
}
