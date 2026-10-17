package tool_test

// 本文件是两件记忆工具（save_memory / recall_memory）的验收面（颗粒度文档 §3.2，共 9 条）：
// 本 US 分四批落地——T024 落 4 条（两份 Schema 契约 + 缺省分区 + 取值大小写），T025 落
// R7（载体隔离），T040（US4）落其余 3 条（无命中占位 / 配额可读拒绝 / 确认文案），T045
// （US4）落末条 R8（空输入即拒，零写入零检索）。**外部测试包**（与 policy_test.go /
// filetools_test.go 同形）：断言的是工具经导出面暴露的契约（名字 / Schema / 返回文案 /
// 错误类型），不是内部实现。
// The acceptance surface of the two memory tools, in the external test package.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/tool"
)

// ctx 是记忆工具用例的取消 / 截止载体：这些用例都不取消，取值只求"传一个非 nil 的
// context"（沿 internal/memory 测试包的同名约定）。
var ctx = context.Background()

// memoryAppend 记一笔追加调用的可判读数（作用域 / 分区 / 正文）：R8 只看计数，缺省分区
// 与取值大小写两例还要看"落到了哪一层的哪个分区"——只数次数判不出落错分区。
type memoryAppend struct {
	scope   core.Scope
	section core.Section
	content string
}

// countingStore 是 `core.LongTermMemoryStore` 的**记录型**替身：Append 记数并留痕、
// RecallByKeyword 记数、Load 恒空（颗粒度文档 §4.2 的 `countingStore`）。
// 它是 R8「零写入 / 零检索」、缺省分区两处与 T040 三条的判据来源：`hits` 造命中 / 零命中
// 两态，`appendErr` 造配额拒绝档——**缺省值保持"恒成功 + 零命中"**，既有用例零影响。
// A recording double of core.LongTermMemoryStore.
type countingStore struct {
	appends int
	recalls int
	calls   []memoryAppend

	// hits 是 RecallByKeyword 的返回值（缺省 nil = 零命中档）。
	hits []string

	// appendErr 非 nil 时 Append 按注入错误返回（缺省 nil = 恒成功；配额路径取用）。
	appendErr error
}

// Append 记一笔写入并留痕（缺省恒成功；`appendErr` 非 nil 时按注入错误返回）。
func (s *countingStore) Append(_ context.Context, scope core.Scope, section core.Section, content string) error {
	s.appends++
	s.calls = append(s.calls, memoryAppend{scope: scope, section: section, content: content})
	return s.appendErr
}

// Load 恒返回空层（本替身不承载读取语义）。
func (s *countingStore) Load(context.Context, core.Scope) (string, error) { return "", nil }

// RecallByKeyword 记数并回注入的命中集（仍不入参——「检索词传得对不对」由零检索档判）。
func (s *countingStore) RecallByKeyword(context.Context, core.Scope, string) ([]string, error) {
	s.recalls++
	return s.hits, nil
}

// toolEnvWithAgent 造一次记忆工具调用的**显式**执行环境：Agent 名是作用域的**唯一**取值
// 来源（FR-021——模型不得传 agent_name、工具不得从路径或包级状态反推）。空名 = 无 Agent
// 上下文 → 全局层（正常取值，不是异常）。沿 US-5 fileToolFixture.toolEnv() / httpToolEnv()
// 的"显式环境"形状。
// The explicit per-call environment; the agent name is the only source of scope.
func toolEnvWithAgent(name string) core.ToolEnv {
	// 记忆工具不消费输出上限（截断是执行侧的事），这里按契约缺省给一个具体值即可。
	return core.ToolEnv{
		RunID:          "run-memory-1",
		SessionID:      "sess-memory-1",
		AgentName:      name,
		MaxOutputBytes: 256 * 1024,
	}
}

// toolSchemaSpec 是内联 JSON Schema 的**最小投影**：本文件的用例判的是参数面（参数名 /
// 必填集 / 取值集合），不是整份字面量——Schema 文本未在文档冻结，逐字钉死会把测试与实现
// 绑成同一份文本，形态漂移时两边一起改（钉不住任何东西）。
// The minimal projection of the tool schemas: names, required set and value sets.
type toolSchemaSpec struct {
	Type       string `json:"type"`
	Properties map[string]struct {
		Type string   `json:"type"`
		Enum []string `json:"enum"`
	} `json:"properties"`
	Required []string `json:"required"`
}

// parseToolSchema 解析工具的入参 Schema（Schema 必须是合法 JSON 对象——执行侧按它判
// 必填缺失）。
func parseToolSchema(t *testing.T, tl core.Tool) toolSchemaSpec {
	t.Helper()
	var spec toolSchemaSpec
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &spec), "Schema 必须是合法 JSON")
	require.Equal(t, "object", spec.Type, "入参契约是对象（checkToolInput 只接受 JSON 对象）")
	return spec
}

// TestSaveMemoryTool_InputSchemaContract 判 save_memory 的参数面：参数名逐字（只有
// content 与 scope——不得多出 agent_name 之类旁路）、content 必填、scope 取值集合逐字
// （口径 9：CORE / ARCHIVAL）。
// 逆改点：把 scope 的枚举改小写或加一个别名（如 `"ARCHIVE"`）→ 枚举断言红；把 scope 也
// 列入必填 → required 断言红。
func TestSaveMemoryTool_InputSchemaContract(t *testing.T) {
	save := tool.NewSaveMemoryTool(&countingStore{})
	require.Equal(t, "save_memory", save.Name(), "工具名与 AGENT.md 的 tools: 声明、注册池逐字比对")
	require.NotEmpty(t, save.Description(), "描述是给模型的能力说明，不得为空")

	spec := parseToolSchema(t, save)
	require.Len(t, spec.Properties, 2, "参数面只有 content 与 scope")
	require.Contains(t, spec.Properties, "content")
	require.Contains(t, spec.Properties, "scope")
	require.Equal(t, "string", spec.Properties["content"].Type)
	require.Equal(t, "string", spec.Properties["scope"].Type)
	require.Equal(t, []string{"content"}, spec.Required, "content 必填、scope 可选（契约 §5.4）")
	require.ElementsMatch(t, []string{"CORE", "ARCHIVAL"}, spec.Properties["scope"].Enum,
		"取值集合逐字（口径 9；顺序不判——JSON Schema 的 enum 无序）")
}

// TestRecallMemoryTool_InputSchemaContract 判 recall_memory 的参数面：只有一个必填的
// query（单个关键词，契约 §5.4）——不得多出 scope / agent_name 之类的旁路。
// 逆改点：给 query 加 `"required":[]` 或再添一个参数 → 对应断言红。
func TestRecallMemoryTool_InputSchemaContract(t *testing.T) {
	recall := tool.NewRecallMemoryTool(&countingStore{})
	require.Equal(t, "recall_memory", recall.Name(), "工具名与 AGENT.md 的 tools: 声明、注册池逐字比对")
	require.NotEmpty(t, recall.Description(), "描述是给模型的能力说明，不得为空")

	spec := parseToolSchema(t, recall)
	require.Len(t, spec.Properties, 1, "参数面只有 query（核心区每轮已完整注入，检索不入参）")
	require.Contains(t, spec.Properties, "query")
	require.Equal(t, "string", spec.Properties["query"].Type)
	require.Equal(t, []string{"query"}, spec.Required, "query 必填（契约 §5.4）")
}

// TestSaveMemoryTool_DefaultsScopeToArchival 判缺省档的**两态**：省略 scope 与显式
// `null` 都落归档区（口径 9）——不得把缺省改成核心区，也不得让 null 报错。
// 逆改点：缺省值改 `CORE`（或 `null` 走成非法）→ 分区断言 / 无错误断言红。
func TestSaveMemoryTool_DefaultsScopeToArchival(t *testing.T) {
	for _, in := range []string{
		`{"content":"缺省档的偏好"}`,
		`{"content":"缺省档的偏好","scope":null}`,
	} {
		store := &countingStore{}
		_, err := tool.NewSaveMemoryTool(store).Execute(ctx, toolEnvWithAgent("alice"), json.RawMessage(in))
		require.NoError(t, err, "省略与 null 都是合法入参：%s", in)
		require.Equal(t, 1, store.appends, "恰好一笔写入：%s", in)
		require.Equal(t, core.SectionArchive, store.calls[0].section, "缺省分区 = 归档区：%s", in)
		require.Equal(t, core.Scope{Agent: "alice"}, store.calls[0].scope,
			"作用域由 env.AgentName 解析（FR-021——模型不得传 agent_name）：%s", in)
	}
}

// TestSaveMemoryTool_RejectsUnknownScopeCaseSensitive 判取值**区分大小写**（口径 9）：
// `core` / `Core` / `ARCHIVE` 三类都非法 → `invalid_input` 且**零写入**——拒得不干净
// （先写一条再报错）比不拒更糟：模型会以为记住了。
// 逆改点：把取值比较改成 `strings.EqualFold` 或加 `ARCHIVE` 别名 → 无错误断言红；
// 先 Append 再校验取值 → 零写入断言红。
func TestSaveMemoryTool_RejectsUnknownScopeCaseSensitive(t *testing.T) {
	for _, in := range []string{
		`{"content":"大小写不合法","scope":"core"}`,
		`{"content":"大小写不合法","scope":"Core"}`,
		`{"content":"大小写不合法","scope":"ARCHIVE"}`,
	} {
		store := &countingStore{}
		_, err := tool.NewSaveMemoryTool(store).Execute(ctx, toolEnvWithAgent("alice"), json.RawMessage(in))
		require.Error(t, err, "取值不合法必须拒：%s", in)
		require.True(t, core.IsInvalidInput(err), "归类 = invalid_input（口径 18 判序先于 runtime_error）：%s", in)
		require.Zero(t, store.appends, "拒绝即零写入：%s", in)
	}
}

// R7 —— 载体隔离负向：文件工具拒写 memory/、记忆工具可写（tool 层，真策略 Fixture）。
// 拒绝面的两条字面量**独立抄一份**（沿 filetools_test.go 的 fileReadLimit / wantWriteFileSchema 先例）：
// policy.go 只导出 NewWhitelistPolicy，测试侧引用实现常量会让两边一起漂、钉不住任何东西。
const (
	memoryNotWritableRule     = "记忆目录不在可写范围"
	memoryNotWritableGuidance = "记忆的写入归记忆工具，文件工具不改写 memory/（代码常量），没有配置键可以加。"
)

// TestMemoryDirIsolation 覆盖契约 §5.1 的载体隔离：文件工具够不到 memory/，记忆工具够得到。
// 逆改点：① 把 memory 分支从 checkFileWrite 里删掉 → 工具侧落回通用规则（ruleWriteOutside），
// 规则名断言红——**这是本用例唯一的判别力来源**：只判 `IsSandboxViolation` 时，
// 删掉 memory 分支它照样绿（探针实测：agent 上下文 + 相对 `memory/…` 的入参解析到
// `agents/<name>/memory/…`，通用规则会顶上来，文案却完全不提记忆）；
// ② 给记忆工具也接上文件工具的那条路径 → 正向对照红。
func TestMemoryDirIsolation(t *testing.T) {
	f := newFileToolFixture(t, nil) // 最小工作区骨架（含 memory/MEMORY.md 占位文件）+ 真策略
	before, err := os.ReadFile(f.path(filepath.Join("memory", "MEMORY.md")))
	require.NoError(t, err)

	// ① 相对 Agent 自有目录**爬出去**打到 memory/：解析后落在 `<root>/memory/…`，
	//    因此判的必须正是记忆目录那条规则（相对入参 `memory/…` 会落在自有目录里
	//    `agents/<name>/memory/…` → 走通用规则，判不出记忆分支）。
	for _, tc := range []struct{ target, wantTarget string }{
		{filepath.Join("..", "..", "memory", "MEMORY.md"), filepath.Join("memory", "MEMORY.md")},
		{
			filepath.Join("..", "..", "memory", "agents", policyAgent, "MEMORY.md"),
			filepath.Join("memory", "agents", policyAgent, "MEMORY.md"),
		},
	} {
		_, err := runTool(t, f.write, f.toolEnv(),
			`{"path":`+strconv.Quote(tc.target)+`,"content":"越界写入"}`)
		var violation *core.SandboxViolationError
		require.ErrorAs(t, err, &violation, "文件工具必须拒写 memory/：%s", tc.target)
		require.Equal(t, memoryNotWritableRule, violation.Rule, "拒绝必须出自记忆目录规则：%s", tc.target)
		require.Equal(t, memoryNotWritableGuidance, violation.Guidance, "指引逐字固定：%s", tc.target)
		require.Equal(t, tc.wantTarget, violation.Target,
			"Target 给工作区相对形态（拒绝文案不回显绝对路径）")
	}

	// ② 拒绝之后原文件**逐字节不变**（拒绝不是"清空重写"，也不是"改完再报错"）。
	after, err := os.ReadFile(f.path(filepath.Join("memory", "MEMORY.md")))
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "被拒的写入不得改动原文件一个字节")

	// ③ 正向对照：同一 root、同一 Agent 上下文下，记忆工具写入成功——隔离是"通道分离"，
	//    不是"记忆不可写"（否则 ① 用"所有 memory/ 写入都拒"的错实现也能过）。
	store := memory.NewMarkdownStore(f.root)
	_, err = tool.NewSaveMemoryTool(store).Execute(ctx, toolEnvWithAgent(policyAgent),
		json.RawMessage(`{"content":"用户偏好简洁回答","scope":"CORE"}`))
	require.NoError(t, err)
	written, err := os.ReadFile(f.path(filepath.Join("memory", "agents", policyAgent, "MEMORY.md")))
	require.NoError(t, err)
	require.Contains(t, string(written), "用户偏好简洁回答", "记忆工具必须真的写进去")
}

// TestRecallMemoryTool_NoHitReturnsSuccessPlaceholder 守「未命中不是错误」（约束 10 /
// 口径 10）：零命中 → **成功** + 占位文案 `(无匹配条目)`——不得映射成 `not_found` /
// `runtime_error`（把"没有"报成"失败"会让模型反复重试一次正确的空结果）。命中档是同一条
// 路径的对照面：条目原文按写入顺序以**空行**分隔（口径 11）。
// 逆改点：零命中改成返回错误 → ① 的 `NoError` 红（口径 10 的判别力全在这一档）；
// 命中渲染改成单换行 / 逗号分隔 → ② 的分隔断言红。
func TestRecallMemoryTool_NoHitReturnsSuccessPlaceholder(t *testing.T) {
	t.Run("零命中", func(t *testing.T) {
		store := &countingStore{} // hits 缺省 nil = 零命中
		res, err := tool.NewRecallMemoryTool(store).Execute(ctx, toolEnvWithAgent("alice"),
			json.RawMessage(`{"query":"没有这条"}`))
		require.NoError(t, err, "未命中是成功取值，不是失败（口径 10）")
		require.Equal(t, "(无匹配条目)", res.Content, "占位文案逐字（口径 11）")
		require.Equal(t, 1, store.recalls, "零命中也要真的查过——否则「没有」不是检索的结论")
	})

	t.Run("命中", func(t *testing.T) {
		store := &countingStore{hits: []string{"第一条：成本评估", "第二条：成本复评"}}
		res, err := tool.NewRecallMemoryTool(store).Execute(ctx, toolEnvWithAgent("alice"),
			json.RawMessage(`{"query":"成本"}`))
		require.NoError(t, err)
		require.Equal(t, "第一条：成本评估\n\n第二条：成本复评", res.Content,
			"条目原文按写入顺序以**空行**分隔（口径 11）")
	})
}

// TestSaveMemoryTool_MapsQuotaToReadableInvalidInput 守「超配额映射成可读的 invalid_input」
// （约束 16 / 口径 18）：载体回 `*core.MemoryQuotaError` 时工具**不原样上抛**，而是换成
// `invalid_input` 并附**可自我纠正**的文案——点名被触发的上限数字 + 给出下一步（压缩内容 /
// 改写归档区）。两档各一条：单条上限与层内总量上限的文案不同，只判一档会漏掉另一档的数字
// （模型据此决定压缩内容还是改写归档区）。
// 逆改点：把配额错误原样 return（不做映射）→ `IsInvalidInput` 断言红；文案丢掉数字 →
// 点名上限的断言红。
func TestSaveMemoryTool_MapsQuotaToReadableInvalidInput(t *testing.T) {
	cases := []struct {
		name  string
		quota *core.MemoryQuotaError
		want  []string // 文案里必须逐字出现的内容：上限数字 + 可执行的下一步
	}{
		{
			name:  "单条越限",
			quota: &core.MemoryQuotaError{Kind: core.QuotaKindEntry, Limit: core.MaxCoreEntryBytes},
			want:  []string{"4096", "压缩内容", "改写归档区"},
		},
		{
			name:  "层内总量越限",
			quota: &core.MemoryQuotaError{Kind: core.QuotaKindSection, Limit: core.MaxCoreSectionBytes},
			want:  []string{"32768", "改写归档区"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &countingStore{appendErr: c.quota}
			_, err := tool.NewSaveMemoryTool(store).Execute(ctx, toolEnvWithAgent("alice"),
				json.RawMessage(`{"content":"一条核心记忆","scope":"CORE"}`))
			require.Error(t, err, "超配额必须拒")
			require.True(t, core.IsInvalidInput(err),
				"归类 = invalid_input（约束 16 / 口径 18）——模型看到的是可自我纠正的指令，不是一句内部故障")

			var invalid *core.InvalidInputError
			require.ErrorAs(t, err, &invalid)
			require.Equal(t, "save_memory", invalid.Tool, "错误点名是哪件工具的入参问题")
			for _, want := range c.want {
				require.Contains(t, err.Error(), want, "文案必须点名触发的上限并给出下一步")
			}
			require.Equal(t, 1, store.appends, "载体真的被调用过（拒绝来自载体裁决，不是工具层先拦）")
		})
	}
}

// TestMemoryTools_ConfirmationNamesScopeAndSection 守 save 的确认文案（口径 11 的固定字面量）：
// `已记住（作用域：<层名>；分区：<分区名>）`——层名取 `全局层` / `Agent 层 <name>`，分区名取
// `核心记忆` / `归档记忆`；**不回显内容**（回显会把刚写下的内容再塞回模型上下文，敏感内容
// 二次曝光）。两档：Agent 层核心区（显式 `scope=CORE`）与全局层归档区（缺省分区 + 无 Agent
// 上下文）——后者同时钉住「无 Agent 上下文是正常取值」在文案面上的表现。
// 逆改点：把 content 拼进确认文案 → 不回显断言红；层名恒渲染 `全局层`（丢 Agent 名）→
// Agent 档的逐字相等断言红。
func TestMemoryTools_ConfirmationNamesScopeAndSection(t *testing.T) {
	cases := []struct {
		name    string
		agent   string
		content string
		input   string
		want    string
	}{
		{
			name:    "Agent 层核心区",
			agent:   "alice",
			content: "用户偏好简洁回答",
			input:   `{"content":"用户偏好简洁回答","scope":"CORE"}`,
			want:    "已记住（作用域：Agent 层 alice；分区：核心记忆）",
		},
		{
			name:    "全局层归档区（缺省分区 + 无 Agent 上下文）",
			agent:   "",
			content: "跨 Agent 的共享偏好",
			input:   `{"content":"跨 Agent 的共享偏好"}`,
			want:    "已记住（作用域：全局层；分区：归档记忆）",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &countingStore{}
			res, err := tool.NewSaveMemoryTool(store).Execute(ctx, toolEnvWithAgent(c.agent),
				json.RawMessage(c.input))
			require.NoError(t, err)
			require.Equal(t, c.want, res.Content, "确认文案逐字（口径 11；分号与括号都是字面量的一部分）")
			require.NotContains(t, res.Content, c.content, "不回显内容（口径 11）")
		})
	}
}

// R8 —— 空 query / 空 content 在工具层即拒（零写入、零检索，`package tool_test`）。
// 判据面 = **规范化（TrimSpace）后为空**：空串与纯空白同档（约束 20 / 29，P1-b 裁决）。
// 逆改点：① 删掉 check 里的空值分支 → sqlite 档 instr(content,”) 命中全部 →
// 「空词召回倒出整个归档区」；② 判据只写 `args.Content == ""` 而不先 TrimSpace →
// 纯空白两态（P1-b 新增）红。
func TestMemoryTools_RejectEmptyInputs(t *testing.T) {
	store := &countingStore{} // 记录 Append / RecallByKeyword 调用次数的替身（外部测试包内自建）
	save := tool.NewSaveMemoryTool(store)
	recall := tool.NewRecallMemoryTool(store)

	// 空串与纯空白四态：规范化后都为空 → 全部必须拒（只判 `== ""` 时后两条红）。
	for _, in := range []string{`{"content":""}`, `{"content":"   \n\t "}`} {
		_, err := save.Execute(ctx, toolEnvWithAgent("alice"), json.RawMessage(in))
		require.Error(t, err, "规范化后为空的 content 必须拒：%s", in)
		require.True(t, core.IsInvalidInput(err))
	}
	for _, in := range []string{`{"query":""}`, `{"query":" "}`} {
		_, err := recall.Execute(ctx, toolEnvWithAgent("alice"), json.RawMessage(in))
		require.Error(t, err, "规范化后为空的 query 必须拒：%s", in)
		require.True(t, core.IsInvalidInput(err))
	}

	require.Zero(t, store.appends, "规范化后为空 → 零写入")
	require.Zero(t, store.recalls, "规范化后为空 → 零检索")
}
