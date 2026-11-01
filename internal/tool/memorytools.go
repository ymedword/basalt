package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是两件记忆工具（save_memory / recall_memory，契约 §5.4、技术方案 §3.5.1 契约 5）。
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
	saveMemoryInputSchema   = `{"type":"object","properties":{"content":{"type":"string","description":"要记住的内容（纯文本；写入前会去掉首尾空白）"},"scope":{"type":"string","enum":["CORE","ARCHIVAL"],"description":"落哪个区：CORE = 核心区（每轮完整注入系统提示词，受配额限制），ARCHIVAL = 归档区（缺省；不自动注入，由 recall_memory 召回，缺省语义检索）"}},"required":["content"]}`
	recallMemoryInputSchema = `{"type":"object","properties":{"query":{"type":"string","description":"检索文本：语义模式（缺省）送 embedding 在归档区做向量召回，返回名次与来源；keyword 模式按整串子串匹配（区分大小写）"},"mode":{"type":"string","enum":["semantic","keyword"],"description":"召回模式：省略 = semantic（语义召回）；keyword = 旧的整串子串行为（不分词、不扩展、不向量化）"}},"required":["query"]}`
)

// recallModeSemantic / recallModeKeyword 是 `recall_memory` 的两种召回模式字面量（P-07）。
// 取值与 `memory.RecallMode*` 逐字同源，但**本包不得 import memory**（依赖方向），
// 因此在这里声明一份：两面由测试同时钉住（本包判枚举、memory 包判行为），漂移即红。
// The two recall mode literals, mirrored locally because tool cannot import memory.
const (
	recallModeSemantic = "semantic"
	recallModeKeyword  = "keyword"
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
	return "记住一条长期记忆，跨对话保留（缺省落归档区，用 recall_memory 召回——缺省语义检索；scope=CORE 落核心区，每轮完整注入系统提示词并受配额限制）"
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

// RecallMemoryTool 是 recall_memory：从归档区召回条目。核心区**不参与检索**
// （它每轮已完整注入），故本工具没有分区参数。
//
// 两条召回路（P-07 / A-08）：**缺省是语义召回**（`mode` 省略 = 语义，命中带名次与来源），
// 显式 `mode=keyword` 走旧的整串子串路径（渲染逐字节保留旧行为）。两条路都在触碰载体
// **之前**判一次 `env.Principal.Valid()`——身份由 Run 准入注入，零值不得补成管理员；
// 语义路的权限判定由载体按**每次调用**重做（冻结内容不冻结权限，C-11）。
// RecallMemoryTool recalls from the archive: semantic by default, keyword on request. Both
// paths check the injected principal before touching the store.
type RecallMemoryTool struct {
	// store 是 keyword 模式的兼容载体（旧行为逐字保留；markdown 之类的旧档也能用）。
	store core.LongTermMemoryStore
	// recaller 是语义模式的召回端口；nil = 语义路径未装配（装配层两段绑定，见 wire）。
	recaller MemoryRecaller
}

var _ core.Tool = (*RecallMemoryTool)(nil)

// NewRecallMemoryTool 装配 recall_memory：关键词路只接记忆载体、语义路只接召回端口
// ——都不接 Policy（注 3）。`recaller` 允许为 nil（语义路径未装配），此时语义请求按
// 失败关闭给可执行的 `invalid_input`，**不**静默降级成关键词。
// NewRecallMemoryTool wires both recall paths; a nil recaller fails closed on semantic
// requests instead of silently degrading.
func NewRecallMemoryTool(store core.LongTermMemoryStore, recaller MemoryRecaller) *RecallMemoryTool {
	return &RecallMemoryTool{store: store, recaller: recaller}
}

// Name 是工具名——与 AGENT.md 的 `tools:` 声明、注册池逐字比对。
// Name is matched verbatim against the tools declaration of AGENT.md.
func (t *RecallMemoryTool) Name() string { return "recall_memory" }

// Description 是给模型看的能力说明。
// Description is the model-facing summary.
func (t *RecallMemoryTool) Description() string {
	return "从归档区召回记忆条目（缺省语义召回，命中带名次与来源；mode=keyword 按整串子串匹配、区分大小写；核心记忆每轮已完整注入，不需要检索）"
}

// InputSchema 返回内联 Schema（测试按参数面判卷）。
// InputSchema returns the inline schema.
func (t *RecallMemoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(recallMemoryInputSchema)
}

// Execute 取回记忆：解码 → 规范化 → 判空 → 解析模式 → 判身份 → 分派。**零命中是成功**：
// 返回 `(无匹配条目)` 而不是 `not_found` / `runtime_error`（口径 10）——把"没有"报成
// "失败"会让模型反复重试。
// A no-hit search succeeds with an explicit copy; the principal is checked before either
// path touches a carrier, and an unknown mode is refused before any dispatch.
func (t *RecallMemoryTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	args, err := decodeRecallMemoryInput(t.Name(), input)
	if err != nil {
		return core.Result{}, err
	}
	args.Query = strings.TrimSpace(args.Query) // 先规范化再判空（口径 10）
	if err := args.check(t.Name()); err != nil {
		return core.Result{}, err
	}
	mode, err := args.mode(t.Name())
	if err != nil {
		return core.Result{}, err
	}
	// 身份闸门（P-07 / C-28）：两种模式同一道——keyword 模式的载体没有权限面，若这里
	// 不判，"零值身份"就会在兼容路径上悄悄可用。零值**不得**补成管理员。
	// One identity gate for both modes: the legacy carrier has no permission surface of
	// its own, so the zero value must be refused here rather than defaulted.
	if !env.Principal.Valid() {
		return core.Result{}, fmt.Errorf("%w: %s 缺少可用的调用方身份", core.ErrUnauthorized, t.Name())
	}
	if env.AgentName != "" && !env.Principal.HasAgentScope(env.AgentName) {
		return core.Result{}, core.ErrForbidden
	}
	if env.ShareGlobalMemory && env.AgentName != "" {
		return t.recallShared(ctx, env, args.Query, mode)
	}
	if mode == recallModeKeyword {
		return t.recallKeyword(ctx, env, args.Query)
	}
	return t.recallSemantic(ctx, env, args.Query)
}

// 共享召回的两个层名（渲染标题与失败文案共用一份字面量）。
// The two layer names shared by the block headings and the failure copy.
const (
	sharedLayerAgent  = "Agent 记忆"
	sharedLayerGlobal = "全局记忆"
	blockHeading      = "## "
)

// recallShared 是 `include_global_memory = true` 时的召回：**分层块**输出。
//
// 为什么按层分块、层内各自从 1 编号（D-4 裁决，判据面变更）：两层由载体各自检索、
// 语义模式下各自一次 embedding，从未放在一起排序。跨层连续编号会读成一个不存在的
// 统一名次——文档再多解释也补不回来，所以序号只在层内有效，层与层之间靠标题分开。
// 层序固定 Agent 层 → 全局层（与注入面 `## 全局记忆` 在前、`## Agent 记忆` 在后的
// 拼接顺序**不同**：注入面是前缀叠加、召回面是逐层检索，两者不互相套用）。
//
// 任一层失败即整次失败：不返回另一层的部分命中——半份召回会让模型以为"那一层没有"。
// 跨层重复正文只保留**先读到**的 Agent 层那一份（Agent 层的表述更具体）。
// Shared recall renders one block per layer with per-layer numbering: the layers are
// retrieved and embedded independently and were never jointly ranked. Any layer failure
// fails the whole call rather than returning a partial hit list.
func (t *RecallMemoryTool) recallShared(ctx context.Context, env core.ToolEnv, query, mode string) (core.Result, error) {
	if t.recaller == nil {
		return core.Result{}, t.sharedUnavailable()
	}
	layers := []struct {
		scope core.Scope
		name  string
	}{
		{scope: core.Scope{Agent: env.AgentName}, name: sharedLayerAgent},
		{scope: core.GlobalScope, name: sharedLayerGlobal},
	}
	seen := map[string]struct{}{}
	blocks := make([]string, 0, len(layers))
	for _, layer := range layers {
		hits, err := t.recaller.RecallArchive(ctx, MemoryRecallRequest{Principal: env.Principal, AuthorizationAgent: env.AgentName, Scope: layer.scope, Query: query, Mode: mode})
		if err != nil {
			if errors.Is(err, ErrRecallUnavailable) {
				return core.Result{}, t.sharedUnavailable()
			}
			// 缺口点名**是哪一层**：不分层的文案会让模型以为整个召回都不可用，而实际
			// 上换关键词仍能检索——错误信息必须指向可执行的下一步。
			if errors.Is(err, ErrRecallIndexNotReady) {
				return core.Result{}, t.sharedIndexNotReady(layer.name)
			}
			return core.Result{}, err
		}
		lines := make([]string, 0, len(hits))
		for _, hit := range hits {
			if _, ok := seen[hit.Content]; ok {
				continue
			}
			seen[hit.Content] = struct{}{}
			if hit.Source == "" {
				lines = append(lines, fmt.Sprintf("%d. %s", len(lines)+1, hit.Content))
				continue
			}
			lines = append(lines, fmt.Sprintf("%d. [%s] %s", len(lines)+1, hit.Source, hit.Content))
		}
		if len(lines) == 0 {
			continue
		}
		blocks = append(blocks, blockHeading+layer.name+"\n"+strings.Join(lines, "\n\n"))
	}
	if len(blocks) == 0 {
		return core.Result{Content: recallNoMatchContent}, nil
	}
	return core.Result{Content: strings.Join(blocks, "\n\n")}, nil
}

// recallKeyword 是旧路径逐字保留：整串子串、区分大小写、按写入顺序以空行分隔（口径 11）。
// The legacy keyword path, preserved verbatim.
func (t *RecallMemoryTool) recallKeyword(ctx context.Context, env core.ToolEnv, query string) (core.Result, error) {
	hits, err := t.store.RecallByKeyword(ctx, memoryScopeOf(env), query)
	if err != nil {
		return core.Result{}, err
	}
	if len(hits) == 0 {
		return core.Result{Content: recallNoMatchContent}, nil
	}
	return core.Result{Content: strings.Join(hits, "\n\n")}, nil
}

// recallSemantic 走召回端口：命中渲染成 `名次. [来源] 正文`，条目之间空行分隔（A-08
// 「带名次与来源」）。来源为空时不渲染方括号——旧载体合成的条目没有来源列，渲染一个
// `[]` 会让模型以为这是缺失的数据。
// Semantic hits render as `rank. [source] content`, one block per entry.
func (t *RecallMemoryTool) recallSemantic(ctx context.Context, env core.ToolEnv, query string) (core.Result, error) {
	if t.recaller == nil {
		return core.Result{}, t.semanticUnavailable()
	}
	hits, err := t.recaller.RecallArchive(ctx, MemoryRecallRequest{
		Principal: env.Principal,
		Scope:     memoryScopeOf(env),
		Query:     query,
		Mode:      recallModeSemantic,
	})
	if err != nil {
		if errors.Is(err, ErrRecallUnavailable) {
			return core.Result{}, t.semanticUnavailable()
		}
		// 索引缺口不是"没有匹配"（V-5）：显式提示并给可执行的下一步。**不得**落到
		// 下方的 `recallNoMatchContent`——那会让一次索引故障看起来像一次正常空结果。
		// A vector gap must not be rendered as the no-match placeholder.
		if errors.Is(err, ErrRecallIndexNotReady) {
			return core.Result{}, t.indexNotReady()
		}
		return core.Result{}, err
	}
	if len(hits) == 0 {
		return core.Result{Content: recallNoMatchContent}, nil
	}
	lines := make([]string, 0, len(hits))
	for i, hit := range hits {
		rank := hit.Rank
		if rank <= 0 {
			rank = i + 1
		}
		if hit.Source == "" {
			lines = append(lines, fmt.Sprintf("%d. %s", rank, hit.Content))
			continue
		}
		lines = append(lines, fmt.Sprintf("%d. [%s] %s", rank, hit.Source, hit.Content))
	}
	return core.Result{Content: strings.Join(lines, "\n\n")}, nil
}

// semanticUnavailable 是**可自我纠正**的失败文案（口径 18）：点名缺什么、给出下一步
// （改用 keyword），而不是一句"内部故障"。装配故障不静默降级成关键词——降级会让
// "检索变弱"看起来像"没有更多内容"。
// The actionable refusal for an unassembled semantic path; never a silent fallback.
func (t *RecallMemoryTool) semanticUnavailable() error {
	return &core.InvalidInputError{
		Tool: t.Name(),
		Reason: "语义召回未装配（缺语义记忆载体或 embedding 连接）——改用 mode=keyword 按关键词检索，" +
			"或请管理员完成 embedding 装配后重试",
	}
}

// sharedUnavailable 是共享模式装配缺失的失败文案：**不**把 `mode=keyword` 当下一步
// ——共享模式下两层都经同一召回端口读，关键词模式同样走不通，写一句做不到的建议只会
// 让模型反复重试。
// The shared-mode refusal names the assembly gap and deliberately omits the keyword hint,
// which cannot work on this path.
func (t *RecallMemoryTool) sharedUnavailable() error {
	return &core.InvalidInputError{
		Tool: t.Name(),
		Reason: "共享记忆召回未装配（缺语义记忆载体或 embedding 连接）：本次调用需要依次检索 Agent 层与全局层，" +
			"两层都经该端口读取。共享模式下的关键词检索同样走这条端口，换 mode 不能绕开；" +
			"请管理员完成装配后重试",
	}
}

// sharedIndexNotReady 是共享模式索引缺口的失败文案（V-5）：**点名缺口的层**，并保留
// 「改用 mode=keyword」这条真能走的下一步——关键词路径不需要向量，与装配缺失不同。
// The shared-mode vector gap names the layer and keeps the keyword hint, which does work.
func (t *RecallMemoryTool) sharedIndexNotReady(layer string) error {
	return &core.InvalidInputError{
		Tool: t.Name(),
		Reason: fmt.Sprintf("记忆向量索引未就绪：%s层有生效归档记忆尚未建立当前代际的向量，"+
			"本次共享召回**不能**下「无匹配」结论。可改用 mode=keyword 检索（该模式不走向量），"+
			"或等待索引回填完成后重试", layer),
	}
}

// indexNotReady 是**可自我纠正**的失败文案（V-5）：索引缺口不是"没有匹配"，也不是
// 装配缺失——它意味着"这次召回不能下无匹配结论"。给出两条可执行的下一步（改用关键词、
// 等回填完成后重试），而不是一句内部故障。
// The actionable refusal for a vector gap; never the no-match placeholder.
func (t *RecallMemoryTool) indexNotReady() error {
	return &core.InvalidInputError{
		Tool: t.Name(),
		Reason: "记忆向量索引未就绪：本层有生效归档记忆尚未建立当前代际的向量，" +
			"本次语义召回**不能**下「无匹配」结论。可改用 mode=keyword 检索，或等待索引回填完成后重试",
	}
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

// recallMemoryInput 是 recall_memory 的入参形态：检索词 + 可选召回模式（核心区不参与检索）。
// Mode 用**指针**接：省略与显式 `null` 同档（缺省语义），而 `""` 是非法取值必须报错——
// 与 `save_memory.scope` 同一形态（口径 9 的两态）。
// recallMemoryInput carries the query and the optional mode; a pointer keeps an omitted
// key and an explicit null on the same default while an empty string stays illegal.
type recallMemoryInput struct {
	Query string  `json:"query"`
	Mode  *string `json:"mode"`
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

// mode 解析召回模式：省略 / `null` → 语义；`semantic` / `keyword` 逐字接受；其余
// （含 `""`、`Semantic`、`KEYWORD`）→ 非法。**严格区分大小写**、不给别名——取值集合
// 是契约字面量（A-08），静默收下一个"差不多"的写法会让模型学到一个不存在的取值。
// mode resolves the recall mode verbatim and case-sensitively; omission means semantic.
func (a recallMemoryInput) mode(toolName string) (string, error) {
	if a.Mode == nil {
		return recallModeSemantic, nil
	}
	switch *a.Mode {
	case recallModeSemantic, recallModeKeyword:
		return *a.Mode, nil
	default:
		return "", &core.InvalidInputError{
			Tool:   toolName,
			Reason: fmt.Sprintf("%s 的 mode 取值非法：%q（只能是 semantic 或 keyword，区分大小写；省略即 semantic）", toolName, *a.Mode),
		}
	}
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
