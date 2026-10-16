package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件是提示词面（US1 / 流程 C）的**白盒**验收测试：两级裁剪与 token 估算器都是非导出件
// （§3.1 签名表：`trimByTurns` / `estimateTokens` / `EstimateVersion`），只有同包测试能
// 逐条断言它们的读数；黑盒面（四段出站顺序）另经 `Build` 与 `llmEnv` 一并覆盖。
// This file is the white-box acceptance test of the prompt surface: the estimator and the
// trimming helpers are unexported, so only an in-package test can assert their readings.
//
// 裁剪夹具的**参考读数**一律写在断言旁（人工实测）：估算器只承诺"版本固定 + 覆盖完整出站
// 字段 + 同输入同读数"，**不承诺**是保守上界（§2.4 注 9）——故这里是标定、不是证明。
// Fixture readings are the measured values written next to each assertion; the estimator
// promises a fixed version and full field coverage, never a conservative upper bound.

// testSectionOne 是假快照的段一正文。
// testSectionOne is the section-one text of the fake snapshot.
const testSectionOne = "段一：你是 demo。"

// stubResolver 是段 4 的假解析器：按传入名字顺序返回预置规格，查不到即跳过（不遍历推断）。
// Resolve 只为满足接口——提示词组装从不解析工具本体（那是调度器的事）。
// stubResolver serves segment-4 specs in the order asked; Resolve exists only to satisfy
// the interface, since prompt building never resolves tool bodies.
type stubResolver struct {
	specs map[string]ToolSpec
}

// Resolve 恒未命中：提示词面不解析工具实体。
// Resolve always misses; the prompt surface never resolves tool bodies.
func (r stubResolver) Resolve(string) (Tool, bool) { return nil, false }

// Specs 按名字顺序取规格。
// Specs returns the specs in the requested order.
func (r stubResolver) Specs(names []string) []ToolSpec {
	out := make([]ToolSpec, 0, len(names))
	for _, name := range names {
		if spec, ok := r.specs[name]; ok {
			out = append(out, spec)
		}
	}
	return out
}

// promptEnv 组装一个白盒运行环境：快照（段一正文 + 轮数上限）+ 内存会话 + 预算 + 工具池。
// 会话经 `NewSession("", nil, nil)` 建为**无状态**（仅内存）——提示词面不需要仓储。
// promptEnv assembles a white-box run environment: snapshot, in-memory session, budget and
// tool pool. The session is stateless, so no store is involved.
func promptEnv(sectionOne string, maxTurns int, history []Message, tools []string) *RunEnv {
	sess := NewSession("", nil, nil)
	sess.messages = append(sess.messages, history...)
	return &RunEnv{
		Req: RunRequest{Message: "当前消息"},
		Snapshot: &ConfigSnapshot{
			AgentName:  "demo",
			SectionOne: sectionOne,
			Profile:    &Profile{Settings: Settings{MaxHistoryTurns: maxTurns}},
		},
		Session: sess,
		Budget:  Budget{ContextTokens: 32768},
		ToolSet: tools,
	}
}

// userMsg 造一条用户消息。
// userMsg builds one user message.
func userMsg(text string) Message { return Message{Role: RoleUser, Content: text} }

// toolCallMsg 造一条"助手请求调用工具"的消息。
// toolCallMsg builds one assistant message carrying a tool call.
func toolCallMsg(id, name, args string) Message {
	return Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: args}}}
}

// toolResultMsg 造一条工具结果消息。
// toolResultMsg builds one tool-result message.
func toolResultMsg(id, text string) Message {
	return Message{Role: RoleTool, Content: text, ToolCallID: id}
}

// toolGroupHistory 造 `rounds` 轮"提问 + 工具调用 + 工具结果"，末尾再附一条**当前**提问。
// 每轮 3 条消息——这正是"按轮数裁剪 ≠ 按条数裁剪"的判据来源。
// toolGroupHistory builds n rounds of question + tool call + tool result, followed by the
// current question; three messages per round is what makes round-based trimming testable.
func toolGroupHistory(rounds int) []Message {
	var out []Message
	for i := 1; i <= rounds; i++ {
		out = append(out,
			userMsg(fmt.Sprintf("u%d", i)),
			toolCallMsg(fmt.Sprintf("c%d", i), "read_file", fmt.Sprintf(`{"path":"f%d.txt"}`, i)),
			toolResultMsg(fmt.Sprintf("c%d", i), fmt.Sprintf("t%d", i)),
		)
	}
	return append(out, userMsg(fmt.Sprintf("u%d", rounds+1)))
}

// orphanCalls 取历史里"没有结果消息"的工具调用（判定组是否被拆散）。
// orphanCalls reports tool calls whose result message is missing, proving a split group.
func orphanCalls(msgs []Message) []ToolCall {
	var calls []ToolCall
	for _, m := range msgs {
		calls = append(calls, m.ToolCalls...)
	}
	return MissingToolResults(calls, msgs)
}

// TestPromptBuilder_FourSegmentsInOrder 守"四段顺序与段 2 恒空"：
// 验收点（C1）——段 1 = 快照冻结的段一正文（不重读）；段 2 记忆注入恒空占位；
// 段 3 历史（当前消息在末位）；段 4 工具定义。出站顺序：system → 历史，tools 另经字段带出。
func TestPromptBuilder_FourSegmentsInOrder(t *testing.T) {
	resolver := stubResolver{specs: map[string]ToolSpec{
		"read_file": {Name: "read_file", Description: "读取文件", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}
	env := promptEnv(testSectionOne, 20, []Message{userMsg("读一下 a.txt")}, []string{"read_file"})

	p, err := NewPromptBuilder(resolver).Build(env)
	require.NoError(t, err)
	require.Equal(t, testSectionOne, p.System, "段 1 取快照正文；段 2 恒空——两段合成一个字段")
	require.Len(t, p.History, 1, "段 3")
	require.Equal(t, "读一下 a.txt", p.History[0].Content, "当前消息在历史末位")
	require.Len(t, p.Tools, 1, "段 4")
	require.Equal(t, "read_file", p.Tools[0].Name)

	req := env.llmEnv("cli-1-abc", 1, p)
	require.Equal(t, roleSystem, req.Messages[0].Role, "段 1 在最前")
	require.Equal(t, testSectionOne, req.Messages[0].Content)
	require.Equal(t, RoleUser, req.Messages[1].Role, "段 3 紧随其后")
	require.Equal(t, p.Tools, req.Tools, "段 4 与出站 tools 共用同一份规格")
}

// TestPromptBuilder_EmptyToolPoolOmitsTools 守"空池不带 tools 键"：
// 验收点（C2）——工具池为空时 `Tools` 为 **nil**，请求体因此不带该键（既有行为逐字不变）。
func TestPromptBuilder_EmptyToolPoolOmitsTools(t *testing.T) {
	resolver := stubResolver{specs: map[string]ToolSpec{
		"read_file": {Name: "read_file", Description: "读取文件", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}
	env := promptEnv(testSectionOne, 20, []Message{userMsg("你好")}, nil)

	p, err := NewPromptBuilder(resolver).Build(env)
	require.NoError(t, err)
	require.Nil(t, p.Tools, "空池 = nil（不是空切片：编码侧据 nil 省略键）")
	require.Nil(t, env.llmEnv("cli-1-abc", 1, p).Tools, "出站同样不带")
}

// TestPromptBuilder_TrimsByTurnsNotByMessages 守"第一级按轮数裁剪"：
// 验收点（C3）——保留最近 `max_history_turns` **轮**（轮 = 以 user 消息为界），
// **不得**按消息条数；25 轮 × 3 条的夹具下保留 60 条而不是 20 条。
func TestPromptBuilder_TrimsByTurnsNotByMessages(t *testing.T) {
	const (
		rounds = 25
		turns  = 20
	)
	history := toolGroupHistory(rounds - 1) // 24 轮工具交互 + 末条当前提问（第 25 轮）= 73 条
	require.Len(t, history, (rounds-1)*3+1, "夹具：每轮 3 条（提问 + 工具调用 + 工具结果）+ 当前提问")

	env := promptEnv(testSectionOne, turns, history, nil)
	p, err := NewPromptBuilder(stubResolver{}).Build(env)
	require.NoError(t, err)
	// 25 条提问、保留最近 20 轮 → 从 u6 起：19 轮 × 3 条 + 当前提问 = 58 条。
	require.Len(t, p.History, (turns-1)*3+1, "按轮保留 = 58 条；按条数保留只会剩 20 条")
	require.Equal(t, "u6", p.History[0].Content, "最老保留项是第 25−20+1=6 轮的提问")
	require.Equal(t, "u25", p.History[len(p.History)-1].Content, "当前提问仍在末位")
	require.Empty(t, orphanCalls(p.History), "轮数裁剪按轮切，不会切出孤儿工具调用")
}

// TestPromptBuilder_KeepsToolGroupsIntact 守"第二级不得拆散工具交互组"：
// 验收点（C4）——token 超限时从最老处整组丢弃（助手工具调用 + 其工具结果同进同出）；
// 拆半分出去，供应商会以 400 拒收整段历史。
func TestPromptBuilder_KeepsToolGroupsIntact(t *testing.T) {
	msgs := toolGroupHistory(2) // u1 a1 t1 u2 a2 t2 u3
	env := promptEnv(testSectionOne, 20, msgs, nil)
	want := Prompt{System: testSectionOne, History: msgs[3:]} // 丢 u1 + 整组 1，保留 u2 组与 u3
	limit := estimateTokens(want)
	env.Budget.ContextTokens = limit + ReserveTokens // Build 的生效上限恰为 limit

	p, err := NewPromptBuilder(stubResolver{}).Build(env)
	require.NoError(t, err)
	require.Equal(t, limit, estimateTokens(p), "裁剪恰好停在目标读数（多丢一条即低于它）")
	require.Len(t, p.History, 4)
	require.Equal(t, "u2", p.History[0].Content, "丢最老处：先丢 u1，再整组丢 c1")
	require.Empty(t, orphanCalls(p.History), "c1 的助手消息与其结果同进同出")
}

// TestPromptBuilder_TrimsByTokensGroupByGroup 守"超预算按组继续裁"：
// 验收点（C5）——第一级裁完仍超限时按完整组从最老处继续丢，直到进预算为止
// （可跨轮：某轮可能只剩提问消息）。
func TestPromptBuilder_TrimsByTokensGroupByGroup(t *testing.T) {
	msgs := toolGroupHistory(3) // 3 组 + 当前提问，共 10 条
	env := promptEnv(testSectionOne, 20, msgs, nil)
	want := Prompt{System: testSectionOne, History: msgs[6:]} // 丢 u1 组 u2 组，保留 u3 组 + 当前
	limit := estimateTokens(want)
	env.Budget.ContextTokens = limit + ReserveTokens

	p, err := NewPromptBuilder(stubResolver{}).Build(env)
	require.NoError(t, err)
	require.Len(t, p.History, 4, "u3 + 该组工具交互 + 当前提问")
	require.Equal(t, "u3", p.History[0].Content, "跨轮继续丢：u1 + 组1 + u2 + 组2 之后才进预算")
	require.LessOrEqual(t, estimateTokens(p), limit, "裁剪后的读数必须进预算")
	require.Empty(t, orphanCalls(p.History), "连续丢弃也不得留下半组")
}

// TestPromptBuilder_RejectsWhenUntrimmableExceedsBudget 守"不可裁部分超限 = 拒绝启动"：
// 验收点（C6）——段 1 + 段 2 + 段 4 + 当前消息本身超限时 `Build` 返回 error，
// 文本点名 `budget.context_tokens`（零模型调用由循环侧断言）。
func TestPromptBuilder_RejectsWhenUntrimmableExceedsBudget(t *testing.T) {
	resolver := stubResolver{specs: map[string]ToolSpec{
		"read_file": {Name: "read_file", Description: "读取文件", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}
	env := promptEnv(testSectionOne, 20, toolGroupHistory(3), []string{"read_file"})
	env.Budget.ContextTokens = 1 // 生效上限 0，连不可裁部分都放不下

	_, err := NewPromptBuilder(resolver).Build(env)
	require.Error(t, err, "不可裁部分超限只有一个出路：拒绝启动")
	require.Contains(t, err.Error(), "budget.context_tokens", "错误文本必须点名配置键")
	require.Contains(t, err.Error(), "不可裁部分", "并说清超限的是哪一部分")
}

// TestPromptBuilder_KeepsCurrentMessage 守"当前消息不可裁"：
// 验收点（C7）——丢弃永不越过最后一条 user 消息；预算仅够"系统提示词 + 当前消息"时，
// 历史被裁到只剩它一条。
func TestPromptBuilder_KeepsCurrentMessage(t *testing.T) {
	msgs := toolGroupHistory(3)
	env := promptEnv(testSectionOne, 20, msgs, nil)
	want := Prompt{System: testSectionOne, History: msgs[len(msgs)-1:]}
	env.Budget.ContextTokens = estimateTokens(want) + ReserveTokens

	p, err := NewPromptBuilder(stubResolver{}).Build(env)
	require.NoError(t, err)
	require.Len(t, p.History, 1, "裁到底也留着当前消息")
	require.Equal(t, "u4", p.History[0].Content)
	require.Equal(t, RoleUser, p.History[0].Role)
}

// TestPromptBuilder_NeverTrimsSystemPrompt 守"系统提示词不裁"：
// 验收点（C8）——段 1 无论预算多紧都逐字保留（它来自快照，不参与两级裁剪）。
func TestPromptBuilder_NeverTrimsSystemPrompt(t *testing.T) {
	long := strings.Repeat("段一正文。", 50)
	msgs := toolGroupHistory(3)
	env := promptEnv(long, 20, msgs, nil)
	want := Prompt{System: long, History: msgs[len(msgs)-1:]}
	env.Budget.ContextTokens = estimateTokens(want) + ReserveTokens

	p, err := NewPromptBuilder(stubResolver{}).Build(env)
	require.NoError(t, err)
	require.Equal(t, long, p.System, "系统提示词逐字保留")
	require.Len(t, p.History, 1, "被裁掉的是历史，不是系统提示词")
	require.Equal(t, long, env.llmEnv("cli-1-abc", 1, p).Messages[0].Content)
}

// TestPromptBuilder_NeverTrimsToolSpecs 守"工具定义不裁"：
// 验收点（C9）——段 4 的每个工具（名字 / 描述 / schema）在裁剪后仍完整，
// 且 schema 原样透传（不解析、不重序列化）。
func TestPromptBuilder_NeverTrimsToolSpecs(t *testing.T) {
	specs := map[string]ToolSpec{
		"read_file":  {Name: "read_file", Description: "读取文件", Parameters: json.RawMessage(`{"type":"object"}`)},
		"write_file": {Name: "write_file", Description: "写入文件", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	resolver := stubResolver{specs: specs}
	names := []string{"read_file", "write_file"}
	msgs := toolGroupHistory(3)
	env := promptEnv(testSectionOne, 20, msgs, names)
	want := Prompt{System: testSectionOne, History: msgs[len(msgs)-1:], Tools: resolver.Specs(names)}
	env.Budget.ContextTokens = estimateTokens(want) + ReserveTokens

	p, err := NewPromptBuilder(resolver).Build(env)
	require.NoError(t, err)
	require.Len(t, p.Tools, 2, "工具定义不参与任何一级裁剪")
	require.Equal(t, "read_file", p.Tools[0].Name, "保序：与 ToolSet 声明顺序一致")
	require.Equal(t, "write_file", p.Tools[1].Name)
	require.Equal(t, json.RawMessage(`{"type":"object"}`), p.Tools[0].Parameters, "schema 原样透传")
}

// TestPromptBuilder_EstimatorFixtures 标定 token 估算器的五类夹具：
// 验收点（C10）——中文 / 英文 / 代码 / 工具 schema / 工具调用参数与供应商扩展字段。
// 参考读数写在每个用例旁（人工实测值，按"汉字 1 token/字、其余 4 字节/token 向上取整"
// 加固定开销手算）；**不宣称**这些读数是上界（§2.4 注 9）。
func TestPromptBuilder_EstimatorFixtures(t *testing.T) {
	cases := []struct {
		name   string
		prompt Prompt
		want   int
	}{
		{
			// 中文夹具：4 个汉字 = 4 token（按字节折算会读成 3 —— 汉字必须按字计）。
			name:   "cjk",
			prompt: Prompt{System: "中文测试"},
			want:   4,
		},
		{
			// 英文夹具：11 字节 → ceil(11/4) = 3。
			name:   "latin",
			prompt: Prompt{System: "hello world"},
			want:   3,
		},
		{
			// 代码夹具：14 字节 → ceil(14/4) = 4（代码的符号密度高，读数明显高于同长度自然语言）。
			name:   "code",
			prompt: Prompt{System: "func main() {}"},
			want:   4,
		},
		{
			// 工具 schema 夹具：开销 8 + 名字 read_file(9B→3) + 描述 读取文件(4) + schema(17B→5) = 20。
			name: "tool_schema",
			prompt: Prompt{Tools: []ToolSpec{{
				Name:        "read_file",
				Description: "读取文件",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			}}},
			want: 20,
		},
		{
			// 工具调用参数与扩展字段夹具（一条助手消息）：
			// 消息开销 4 + role assistant(9B→3) + 调用 id c1(2B→1) + 名字 read_file(9B→3)
			// + arguments(16B→4) + 扩展字段 JSON(17B→5) = 20。
			name: "tool_args_and_extras",
			prompt: Prompt{History: []Message{{
				Role:          RoleAssistant,
				ToolCalls:     []ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"a.txt"}`}},
				ProviderExtra: map[string]any{"reasoning": "x"},
			}}},
			want: 20,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, estimateTokens(tc.prompt), "夹具读数（人工实测）")
		})
	}
}

// TestPromptBuilder_EstimatorIsDeterministic 守"同输入同读数"：
// 验收点（C11）——估算器是纯函数（扩展字段经 `json.Marshal` 定序），重复调用读数不变；
// 且只增不减（追加字段不会让读数变小——否则裁剪会震荡）。
func TestPromptBuilder_EstimatorIsDeterministic(t *testing.T) {
	p := Prompt{
		System:  testSectionOne,
		History: toolGroupHistory(2),
		Tools: []ToolSpec{{
			Name:        "read_file",
			Description: "读取文件",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	}
	first := estimateTokens(p)
	for i := 0; i < 5; i++ {
		require.Equal(t, first, estimateTokens(p), "第 %d 次重算读数漂了", i+1)
	}

	grown := p
	grown.History = append(append([]Message{}, p.History...), userMsg("再问一句"))
	require.GreaterOrEqual(t, estimateTokens(grown), first, "追加消息不得让读数变小")
}

// TestPromptBuilder_EstimatorVersionReadable 守"估算器版本与余量可读出"：
// 验收点（C12）——版本字与余量是固定口径（§6.2 固定口径 9），且生效上限 = 预算 − 余量。
func TestPromptBuilder_EstimatorVersionReadable(t *testing.T) {
	require.Equal(t, "v1", EstimateVersion, "版本字固定 v1（写进日志与评测结果文件，不落库）")
	require.Equal(t, 1024, ReserveTokens, "余量固定 1024")
	require.Equal(t, 32768-ReserveTokens, contextLimit(32768), "生效上限 = 预算 − 余量")
	require.Equal(t, defaultContextTokens-ReserveTokens, contextLimit(0), "零值兜底用缺省预算")
	require.Equal(t, 0, contextLimit(512), "预算小于余量时下限为 0（由准入拒绝兜底）")
}
