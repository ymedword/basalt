package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是两件记忆工具（save_memory / recall_memory，契约 §5.4、技术方案 §5.1 契约 5）。
// 三条纪律贯穿全篇：① 作用域**只在工具层解析**且只取 `env.AgentName`——模型不得传
// `agent_name`、工具不得从路径或包级状态反推（FR-021；空名 = 无 Agent 上下文 = 全局层，
// 是正常取值不是异常）；② 工具**不持有 Policy / CommandRunner**（颗粒度文档 §2.4 注 3）：
// 目标路径由工作区约定拼出、写入面由配额封闭，本文件里没有任何"可被模型指向的位置"；
// ③ **不落审计、不写消息、不记日志**（口径 17）——记忆读写是内容面不是治理面，审计归
// ToolExecutor 的既有判序，本文件不新增任何写路径。
// The two memory tools. Scope is resolved here from the explicit ToolEnv only, the tools
// hold no Policy/CommandRunner, and they neither audit nor log: ToolExecutor owns the
// audit trail of every call.
//
// 入参纪律（约束 20 / 29，两件工具同形）：**先 `TrimSpace` 规范化、再判空**——纯空白与
// 空串同档拒；规范化后的文本才是交给载体的那个表示（写入 / 单条检查 / 总量求和 / 存储 /
// 重建 / 召回六面同表示）。失败一律归 `*core.InvalidInputError`，由执行器分类
// `invalid_input`（口径 18）。
// Both tools normalize before checking for emptiness, so a whitespace-only argument is
// the same refusal as an empty one and the store only ever sees the normalized text.

// saveMemoryInputSchema / recallMemoryInputSchema 是两件工具的**内联 JSON Schema**
// （颗粒度文档 §3.1：测试按参数名 / 必填集 / 取值集合三面判卷）。`required` 数组是执行侧
// 必填校验的唯一依据；`scope` **不在** required 里——省略即归档区（口径 9）。
// Inline schemas; the required array is the only source the executor reads, and scope is
// deliberately optional because omitting it means the archive.
const (
	saveMemoryInputSchema   = `{"type":"object","properties":{"content":{"type":"string","description":"要记住的内容（纯文本；写入前会去掉首尾空白）"},"scope":{"type":"string","enum":["CORE","ARCHIVAL"],"description":"落哪个区：CORE = 核心区（每轮完整注入系统提示词，受配额限制），ARCHIVAL = 归档区（缺省；按关键词召回）"}},"required":["content"]}`
	recallMemoryInputSchema = `{"type":"object","properties":{"query":{"type":"string","description":"检索词：单个关键词，对归档区条目按整串子串匹配（区分大小写）"}},"required":["query"]}`
)

// recallNoMatchContent 是召回未命中的结果文案——**不是错误**（口径 10）：模型要能区分
// "查过了没有"与"这一步失败了"。沿 `(空目录)` 的字面先例。
// The no-hit copy of recall; an empty result is a success, never an error.
const recallNoMatchContent = "(无匹配条目)"

// SaveMemoryTool 是 save_memory：把一条长期记忆写进生效层的指定分区。
// 缺省分区是**归档区**（核心区有配额、每轮完整注入，写它应当是显式决定）；确认文案含
// 作用域与分区、**不回显内容**（口径 11）——回显会让敏感内容二次进模型上下文。
// SaveMemoryTool writes one entry into the requested region; the archive is the default
// and the confirmation names scope and region without echoing the content.
type SaveMemoryTool struct {
	store core.LongTermMemoryStore
}

var _ core.Tool = (*SaveMemoryTool)(nil)

// NewSaveMemoryTool 装配 save_memory：只接受记忆载体接口——不接 Policy（注 3）。
// NewSaveMemoryTool wires save_memory against the memory store alone.
func NewSaveMemoryTool(store core.LongTermMemoryStore) *SaveMemoryTool {
	return &SaveMemoryTool{store: store}
}

// Name 是工具名——与 AGENT.md 的 `tools:` 声明、注册池逐字比对。
// Name is matched verbatim against the tools declaration of AGENT.md.
func (t *SaveMemoryTool) Name() string { return "save_memory" }

// Description 是给模型看的能力说明。
// Description is the model-facing summary.
func (t *SaveMemoryTool) Description() string {
	return "记住一条长期记忆，跨对话保留（缺省落归档区，用 recall_memory 按关键词取回；scope=CORE 落核心区，每轮完整注入系统提示词并受配额限制）"
}

// InputSchema 返回内联 Schema（测试按参数面判卷）。
// InputSchema returns the inline schema.
func (t *SaveMemoryTool) InputSchema() json.RawMessage { return json.RawMessage(saveMemoryInputSchema) }

// Execute 写一条记忆：解码 → 规范化 → 判空 → 解析分区 → 落载体。
// 配额拒绝**映射成可读的 `invalid_input`**（口径 18 / D4）：模型看到的是"改写归档区 /
// 压缩内容"这类可自我纠正的指令，而不是一句内部故障。其余载体错误原样上抛（I/O →
// `runtime_error`，执行器判序负责分类）。
// Quota refusals are mapped to a readable invalid_input; every other store error is
// passed through untouched so the executor's ordering can classify it.
func (t *SaveMemoryTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	args, err := decodeSaveMemoryInput(t.Name(), input)
	if err != nil {
		return core.Result{}, err
	}
	args.Content = strings.TrimSpace(args.Content) // 先规范化再判空（口径 10）
	if err := args.check(t.Name()); err != nil {
		return core.Result{}, err
	}
	section, err := args.section(t.Name())
	if err != nil {
		return core.Result{}, err
	}
	scope := memoryScopeOf(env)
	if err := t.store.Append(ctx, scope, section, args.Content); err != nil {
		var quota *core.MemoryQuotaError
		if errors.As(err, &quota) {
			return core.Result{}, &core.InvalidInputError{Tool: t.Name(), Reason: quota.Error()}
		}
		return core.Result{}, err
	}
	return core.Result{
		Content: "已记住（作用域：" + memoryLayerName(scope) + "；分区：" + memorySectionName(section) + "）",
	}, nil
}

// RecallMemoryTool 是 recall_memory：按关键词从归档区取回条目。核心区**不参与检索**
// （它每轮已完整注入），故本工具没有分区参数。
// RecallMemoryTool searches the archive; the core region is injected whole every round
// and therefore never searched.
type RecallMemoryTool struct {
	store core.LongTermMemoryStore
}

var _ core.Tool = (*RecallMemoryTool)(nil)

// NewRecallMemoryTool 装配 recall_memory：只接受记忆载体接口——不接 Policy（注 3）。
// NewRecallMemoryTool wires recall_memory against the memory store alone.
func NewRecallMemoryTool(store core.LongTermMemoryStore) *RecallMemoryTool {
	return &RecallMemoryTool{store: store}
}

// Name 是工具名——与 AGENT.md 的 `tools:` 声明、注册池逐字比对。
// Name is matched verbatim against the tools declaration of AGENT.md.
func (t *RecallMemoryTool) Name() string { return "recall_memory" }

// Description 是给模型看的能力说明。
// Description is the model-facing summary.
func (t *RecallMemoryTool) Description() string {
	return "从归档区按关键词取回记忆条目（整串子串匹配、区分大小写；核心记忆每轮已完整注入，不需要检索）"
}

// InputSchema 返回内联 Schema（测试按参数面判卷）。
// InputSchema returns the inline schema.
func (t *RecallMemoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(recallMemoryInputSchema)
}

// Execute 取回记忆：解码 → 规范化 → 判空 → 检索。**零命中是成功**：返回 `(无匹配条目)`
// 而不是 `not_found` / `runtime_error`（口径 10）——把"没有"报成"失败"会让模型反复重试。
// A no-hit search succeeds with an explicit copy, so the model never retries a correct
// "nothing matched" as if it were a failure.
func (t *RecallMemoryTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	args, err := decodeRecallMemoryInput(t.Name(), input)
	if err != nil {
		return core.Result{}, err
	}
	args.Query = strings.TrimSpace(args.Query) // 先规范化再判空（口径 10）
	if err := args.check(t.Name()); err != nil {
		return core.Result{}, err
	}
	hits, err := t.store.RecallByKeyword(ctx, memoryScopeOf(env), args.Query)
	if err != nil {
		return core.Result{}, err
	}
	if len(hits) == 0 {
		return core.Result{Content: recallNoMatchContent}, nil
	}
	return core.Result{Content: strings.Join(hits, "\n\n")}, nil // 条目原文按写入顺序、空行分隔（口径 11）
}

// saveMemoryInput 是 save_memory 的入参形态。Scope 用**指针**接：省略与显式 `null` 都解成
// nil，两者同档落归档区（口径 9 的两态——用 string 接会把 `null` 与 `""` 混成一个取值，
// 而 `""` 是非法取值，必须报错）。
// Scope is a pointer so that an omitted key and an explicit null collapse to the same
// default, while an empty string stays an illegal value.
type saveMemoryInput struct {
	Content string  `json:"content"`
	Scope   *string `json:"scope"`
}

// decodeSaveMemoryInput 解出 save_memory 入参；字段类型不符是模型的入参问题（不是本仓
// 缺陷），失败载体是 `*core.InvalidInputError`。
// decodeSaveMemoryInput reports a decode failure as an invalid input.
func decodeSaveMemoryInput(toolName string, input json.RawMessage) (saveMemoryInput, error) {
	var args saveMemoryInput
	if err := json.Unmarshal(input, &args); err != nil {
		return saveMemoryInput{}, &core.InvalidInputError{
			Tool:   toolName,
			Reason: fmt.Sprintf("解析 %s 入参：%s", toolName, err.Error()),
		}
	}
	return args, nil
}

// check 判**规范化后**的 content 非空：`{"content":"   "}` 在 Schema 上合法（键存在），
// 但它什么都记不住——放它过会写进一条空条目，还占配额。
// check refuses an empty content after normalization: the key was present, but there is
// nothing to remember and an empty entry would still consume quota.
func (a saveMemoryInput) check(toolName string) error {
	if a.Content == "" {
		return &core.InvalidInputError{
			Tool:   toolName,
			Reason: fmt.Sprintf("%s 的 content 不能为空（纯空白与空串同档）", toolName),
		}
	}
	return nil
}

// section 解析分区取值：省略 / `null` / `"ARCHIVAL"` → 归档区；`"CORE"` → 核心区；
// 其余（含 `"core"` / `"Core"` / `"ARCHIVE"`）→ 非法。**严格区分大小写**（口径 9）——
// 不做大小写归一，也不给别名：取值集合是契约字面量，静默收下一个"差不多"的写法会让
// 模型学到一个不存在的取值。
// section resolves the region verbatim, case-sensitively; near-miss spellings are
// refused rather than silently normalized.
func (a saveMemoryInput) section(toolName string) (core.Section, error) {
	if a.Scope == nil || *a.Scope == "ARCHIVAL" {
		return core.SectionArchive, nil
	}
	if *a.Scope == "CORE" {
		return core.SectionCore, nil
	}
	return "", &core.InvalidInputError{
		Tool:   toolName,
		Reason: fmt.Sprintf("%s 的 scope 取值非法：%q（只能是 CORE 或 ARCHIVAL，区分大小写）", toolName, *a.Scope),
	}
}

// recallMemoryInput 是 recall_memory 的入参形态：只有一个检索词（核心区不参与检索）。
// recallMemoryInput carries the single keyword; the core region is never searched.
type recallMemoryInput struct {
	Query string `json:"query"`
}

// decodeRecallMemoryInput 解出 recall_memory 入参（失败同上归 invalid_input）。
// decodeRecallMemoryInput reports a decode failure as an invalid input.
func decodeRecallMemoryInput(toolName string, input json.RawMessage) (recallMemoryInput, error) {
	var args recallMemoryInput
	if err := json.Unmarshal(input, &args); err != nil {
		return recallMemoryInput{}, &core.InvalidInputError{
			Tool:   toolName,
			Reason: fmt.Sprintf("解析 %s 入参：%s", toolName, err.Error()),
		}
	}
	return args, nil
}

// check 判**规范化后**的 query 非空：空串在实现侧会匹配上每一条（`strings.Contains` 的
// 空针恒真），放它过等于让"没给检索词"变成"取回整个归档区"——**零检索**才是正确处置。
// An empty query would match every entry, so it is refused before the store is reached.
func (a recallMemoryInput) check(toolName string) error {
	if a.Query == "" {
		return &core.InvalidInputError{
			Tool:   toolName,
			Reason: fmt.Sprintf("%s 的 query 不能为空（纯空白与空串同档）", toolName),
		}
	}
	return nil
}

// memoryScopeOf 从**显式传入**的执行环境解析作用域（FR-021）：`env.AgentName` 是唯一
// 来源——模型不得传 `agent_name`、工具**不得**从 `AgentDir` 反推（路径形态是实现细节，
// 名字才是契约取值）。空名 = 无 Agent 上下文 = 全局层，是正常取值（约束 3）。
// memoryScopeOf derives the scope from the explicit environment alone: the agent name is
// the only source, and an empty name is the normal global-layer case.
func memoryScopeOf(env core.ToolEnv) core.Scope {
	if env.AgentName == "" {
		return core.GlobalScope
	}
	return core.Scope{Agent: env.AgentName}
}

// memoryLayerName 与 memorySectionName 渲染确认文案里的两个名字（口径 11 的固定字面量：
// `全局层` / `Agent 层 <name>`、`核心记忆` / `归档记忆`）——文案是给模型看的，取值只有
// 两档，故直接分支而不建映射表。
// memoryLayerName and memorySectionName render the two names of the confirmation copy.
func memoryLayerName(scope core.Scope) string {
	if scope.Agent == "" {
		return "全局层"
	}
	return "Agent 层 " + scope.Agent
}

// memorySectionName 渲染分区名（核心记忆 / 归档记忆）。
func memorySectionName(section core.Section) string {
	if section == core.SectionCore {
		return "核心记忆"
	}
	return "归档记忆"
}
