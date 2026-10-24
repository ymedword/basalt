package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// 本文件是工具调度的**唯一归属**（颗粒度文档 §2.3 流程 D）：解析与归属校验 → 入参校验 →
// 审计预写（fail-closed）→ 执行 → 终态回填 → 结果包装。
// This file owns tool dispatch (flow D): name resolution, runtime ownership check, input
// validation, fail-closed audit pre-write, execution, terminal write, result packing.
//
// 两条纪律贯穿全篇：**执行器只返回结果、不写消息**（宪法 II 的追加唯一路径）；
// **执行之前的拒绝不写审计行**——`tool_invocations` 记的是"一次工具执行"（有入参、有结果），
// 名字不在注册池 / 不在本 Agent 工具集这两类拒绝没有任何工具被启动，尝试本身已由
// assistant 消息（含 tool_calls）与回填文本完整留痕。与沙箱拒绝区分：后者发生在**执行之内**
// （工具已启动、Policy 判拒），故**有行、failed + sandbox_denied**。
// Two rules run through everything: the executor returns results and never writes messages;
// and refusals that happen *before* execution leave no audit row, because the audit table
// records an execution — while a sandbox refusal happens inside one and therefore does.
//
// 一条运行最多给一次工具调用留**一行**审计：预写拿到的行 id 一路传到终态回填，中途**不得**
// 再写第二行（那样恢复扫描会把同一次调用看成两次半截调用）。
// One call gets exactly one audit row: the id from the pre-write travels to the terminal
// write; a second insert would make recovery read one call as two unfinished ones.

// ToolExecutor 执行一次工具调用。它不持有 Policy / CommandRunner——授权与受控执行
// 属工具内部（§2.4 注 1：入参只有解析它的那一方才有，再解析一遍正是越权漏洞的来源）。
// ToolExecutor performs one tool call. Policy and CommandRunner live inside the tools.
type ToolExecutor struct {
	resolver ToolResolver
	audit    ToolInvocationStore
	logger   *slog.Logger
}

// NewToolExecutor 组装执行器；`logger` 为空时用进程缺省日志器。
// NewToolExecutor assembles the executor; a nil logger falls back to the default one.
func NewToolExecutor(resolver ToolResolver, audit ToolInvocationStore, logger *slog.Logger) *ToolExecutor {
	if logger == nil {
		logger = slog.Default()
	}
	return &ToolExecutor{resolver: resolver, audit: audit, logger: logger}
}

// Execute 执行一次工具调用并返回可回填给模型的结果。
// **不返回 error**：工具失败是模型要看到的信息（换条路走），不是运行级故障（流程 D ⑧）。
// Execute runs one tool call and returns the result handed back to the model. It never
// returns an error: a tool failure is information for the model, not a run-level fault.
//
// `toolSet` 是本 Agent 声明的工具名集——**运行时归属校验**的依据。加载期门禁挡不住
// "注册池被替换 / 快照与加载不同源"，故这里必须再判一次（流程 D ①）。
// The tool set is re-checked at runtime because load-time gating cannot catch a registry
// that no longer matches the frozen snapshot.
func (e *ToolExecutor) Execute(ctx context.Context, env ToolEnv, toolSet []string, call ToolCall) Result {
	// ① 工具名解析 + 运行时归属校验（两道，均在执行之前——不产生审计行、不终止 Run）。
	tool, ok := e.resolver.Resolve(call.Name)
	if !ok {
		// 契约既有字面量（与加载期门禁同一句）：工具名不在全局注册池。
		return Result{Content: fmt.Sprintf("tool not registered: %s", call.Name)}
	}
	if !slices.Contains(toolSet, call.Name) {
		// 全局已注册但**本 Agent 未声明**：四段式拒绝，放行指引点名 `AGENT.md` 的 `tools:`（需重启）。
		refusal := &SandboxViolationError{
			Rule:     "工具未在本 Agent 的 tools 声明中",
			Target:   call.Name,
			Guidance: "请在 AGENT.md 的 tools: 声明中放行该工具后重启实例",
		}
		// 归属拒绝是安全事件：**只记 WARN、不落审计**——审计行留给真实执行（见文件头），
		// 而"模型试图越出本 Agent 工具集"这件事必须有运维看得见的痕迹。
		e.logger.Warn("工具不在本 Agent 的工具集内，调用被拒绝",
			"run_id", env.RunID, "session_id", env.SessionID, "tool", call.Name,
			"reason", "not_in_tool_set", "detail", refusal.Error())
		return Result{Content: refusal.Error()}
	}

	// ② 入参校验：json.Valid + 能解出**对象**。非法 → **仍写审计**（工具确实被调用了）。
	if reason := checkToolInput(tool, call.Arguments); reason != "" {
		return e.refuseInvalidInput(ctx, env, call, reason)
	}

	// ③ 审计**先写 pending**；写失败 → 拒绝执行（fail-closed）+ 回填"审计不可用" → Run 继续。
	if e.audit == nil {
		// 审计仓储没接上 = 装配错误：宁可拒执行也不留无痕调用（与 fail-closed 同源）。
		return Result{Content: auditUnavailableText}
	}
	id, ok := e.beginInvocation(ctx, env, call)
	if !ok {
		return Result{Content: auditUnavailableText}
	}
	// 预写成功后把行 id 注入执行环境：工具（目前是 notify）据此写与本次调用一对一的
	// 派生记录。没有行 id 时不写——审计失败已经在上面 fail-closed。
	// Inject the audit row id so the tool can write its one-per-invocation derived record.
	env.ToolInvocationID = id

	// ④⑤ 执行：`env` 由循环侧构造（本流程只消费）；单工具超时在这里**实际建立**
	// （env.Timeout 已是"工具默认 / Profile 覆盖 / 剩余 Run 预算"三者最小，工具要更短
	// 就自行 context.WithTimeout 再收窄一层）。取值 <= 0 视为不设限（防御，正常恒 > 0）。
	execCtx := ctx
	if env.Timeout > 0 {
		var cancelExec context.CancelFunc
		execCtx, cancelExec = context.WithTimeout(ctx, env.Timeout)
		defer cancelExec()
	}
	start := time.Now()
	res, err := tool.Execute(execCtx, env, json.RawMessage(call.Arguments))
	duration := time.Since(start)

	// ⑥⑦ 终态回填（更新**同一行**）+ 结果包装。
	out := ToolInvocationResult{Status: LLMCallSuccess, Duration: duration}
	content := res.Content
	if err != nil {
		out.Status = LLMCallFailed
		out.ErrorKind = classifyToolError(err)
		out.ErrorMessage = err.Error()
		// 可观测性双轨（约束 26）：审计落 `failed` **且**记 WARN——固定键
		// `run_id` / `session_id` / `tool` / `reason`（失败分类，机器可判）/ `detail`（错误原文）。
		// 记原文不记结果体：错误文案里没有文件内容，工具结果正文才是不该进日志的部分。
		e.logger.Warn("工具执行失败",
			"run_id", env.RunID, "session_id", env.SessionID, "tool", call.Name,
			"reason", string(out.ErrorKind), "detail", err.Error())
		// 失败信息**必须回填**：沙箱拒绝文案原样透传（模型看不到"为什么被拒"就不会收敛）。
		content = err.Error()
	}
	content, truncated := truncateToolOutput(content, env.MaxOutputBytes)
	out.ResultJSON = marshalToolResult(toolResultJSON{
		Content:   content,
		Retryable: res.Retryable,
		Truncated: truncated,
	})
	e.finishInvocation(ctx, env, id, out)

	// ⑧ 返回结果；**Run 不因工具失败而失败**。
	return Result{Content: content, Retryable: res.Retryable}
}

// refuseInvalidInput 为非法入参补一条 `failed` + `invalid_input` 审计行并回填失败文本（流程 D ②）。
// 这里的预写失败**不**降级为"审计不可用"文案：拒绝执行的理由本来就是入参，与审计可用性无关；
// 记 ERROR 即如实留痕（fail-closed 防的是"无痕执行"，而这条路径上根本不会发生执行）。
// refuseInvalidInput records a failed/invalid_input row and hands the reason back. A failed
// pre-write here is only logged: fail-closed protects against unrecorded *execution*, and
// nothing was ever going to execute.
func (e *ToolExecutor) refuseInvalidInput(ctx context.Context, env ToolEnv, call ToolCall, reason string) Result {
	out := ToolInvocationResult{
		Status:       LLMCallFailed,
		ErrorMessage: reason,
		ErrorKind:    ErrorKindInvalidInput,
	}
	if e.audit == nil {
		return Result{Content: reason}
	}
	id, ok := e.beginInvocation(ctx, env, call)
	if !ok {
		return Result{Content: reason}
	}
	e.finishInvocation(ctx, env, id, out)
	return Result{Content: reason}
}

// beginInvocation 预写 `pending` 审计行（收尾上下文：取消不该把预写变成"写不进去"）；
// 失败只记 ERROR（固定键 run_id / store / op）并返回 false，由调用方决定回填文本。
// beginInvocation pre-writes the pending row on a detached context; a failure is logged
// and reported to the caller, never turned into a run-level fault.
func (e *ToolExecutor) beginInvocation(ctx context.Context, env ToolEnv, call ToolCall) (int64, bool) {
	beginCtx, cancel := finishContext(ctx)
	defer cancel()
	id, err := e.audit.BeginInvocation(beginCtx, pendingInvocation(env, call))
	if err != nil {
		e.logger.Error("工具审计预写失败", "run_id", env.RunID, "store", "tool_invocations", "op", "start")
		return 0, false
	}
	return id, true
}

// finishInvocation 回填终态。**回填失败 → 记 ERROR、行留 `pending`、不覆盖真实结果**（契约 §7.2）：
// 停在进行中形态是"这次调用是否产生过外部副作用未知"的诚实记录，由下次启动的恢复扫描归一化；
// 而**结果照常返回给模型**——修复历史不能以扣下答案为代价。
// finishInvocation writes the terminal state. A failed write keeps the row pending and is
// logged only; the result still travels back to the model.
func (e *ToolExecutor) finishInvocation(ctx context.Context, env ToolEnv, id int64, out ToolInvocationResult) {
	finCtx, cancel := finishContext(ctx)
	defer cancel()
	if err := e.audit.FinishInvocation(finCtx, id, out); err != nil {
		e.logger.Error("工具审计终态回填失败", "run_id", env.RunID, "store", "tool_invocations", "op", "finish")
	}
}

// pendingInvocation 组装审计预写形态（`input_json` 原样保存：模型给的 arguments 是 JSON
// 字符串，不重编码；预写态恒为 `pending`，由仓储 SQL 固定写）。
// pendingInvocation builds the pre-write shape; InputJSON is stored verbatim and the status
// is always written as pending by the repository itself.
func pendingInvocation(env ToolEnv, call ToolCall) ToolInvocation {
	return ToolInvocation{
		RunID:      env.RunID,
		SessionID:  env.SessionID,
		ToolCallID: call.ID,
		ToolName:   call.Name,
		InputJSON:  json.RawMessage(call.Arguments),
		CreatedAt:  time.Now(), // 执行面的"此刻"：写入时刻即当前时刻（US-2 审计同款先例）
	}
}

// auditUnavailableText 是审计预写失败时回填给模型的文本（流程 D ③）。
// auditUnavailableText is handed back when the audit store refuses the pre-write.
const auditUnavailableText = "审计不可用：工具调用审计写入失败，本次调用未执行（fail-closed 策略）"

// classifyToolError 把工具返回的错误映射为 `error_kind` 八类之一（契约 §7.3 取值口径）。
// 判定一律走错误链（errors.Is / errors.As），**不得**比对错误文本。
// **判序不可交换**（颗粒度文档 §2.3 流程 C）：沙箱 → 通知禁用 → 入参非法 → 取消 / 截止 →
// 外部系统 → 文件不存在 → 兜底。两条分界的理由：① 入参非法排在取消 / 截止**之前**——入参
// 在工具动身之前就已判死，归类不该被"恰好同时到点的截止"改写；② 外部系统排在取消 / 截止
// **之后**——取消与截止的**身份**优先于"错误是从哪个系统回来的"，出网超时仍归 timeout。
// `notify_disabled` 紧跟沙箱：它是**授权面**的裁决（工具有没有资格发），与沙箱同属"动身之前
// 就被拒"的一类，排在会携带 ctx 原因的其余档之前，避免被改写。
// 第七值 `unknown` 不在此产生：它由崩溃恢复扫描写给半截的 pending 行（§4 口径）。
// classifyToolError maps a tool error onto the eight error_kind values by inspecting the
// error chain only. The order is fixed: sandbox, notify-disabled, invalid input,
// cancellation, external error, missing file, fallback — a disabled notification surface
// is an authorization verdict decided before the call departs.
func classifyToolError(err error) ErrorKind {
	switch {
	case err == nil:
		return "" // 成功留空 → 列写 NULL
	case IsSandboxViolation(err):
		return ErrorKindSandboxDenied
	case IsNotifyDisabled(err):
		// 通知被配置 / 凭据状态禁用（R4.7）：自带固定文案的独立类型，不参与其余五档的
		// 身份判定——它既不是入参问题，也不该被"恰好同时到点的截止"改写成 timeout。
		// A disabled notification surface carries its own fixed text and is decided before
		// the identity of any wrapped cause.
		return ErrorKindNotifyDisabled
	case IsInvalidInput(err):
		// 入参非法（工具解码点：字段类型与取值——入口的必填校验另有 refuseInvalidInput）。
		return ErrorKindInvalidInput
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// ctx 错误一律归 timeout（**与 ProviderService.classifyError 同口径**）——
		// 含 Run 级取消：Run 记 `cancelled`，审计按既有口径记 `failed` + `timeout`。
		// 这一条排在 external_error 之前：外部错误包着 ctx 错误时（`%w` 包装或结果就是
		// ctx 本身），取消 / 截止的身份说话。
		return ErrorKindTimeout
	case IsExternalError(err):
		// 外部系统错误（DNS / 连接 / TLS / 读取失败 / 4xx / 5xx）——文案是自产的类别摘要。
		return ErrorKindExternalError
	case errors.Is(err, fs.ErrNotExist):
		// 文件 / 脚本目标不存在。
		return ErrorKindNotFound
	default:
		// 进程启动失败 / 编码错误 / 其余本仓内部错误。
		return ErrorKindRuntimeError
	}
}

// checkToolInput 校验调用入参，返回空串表示通过、非空即回填给模型的失败文本。
// 判据两道（流程 D ②）：合法 JSON + 能解出**对象**；再按工具**自己声明的** schema 核对必填字段——
// 必填缺失若不在这一层判，就只能落到工具内部被归为 runtime_error，与契约 §7.3
// "入参非法 JSON 对象 / 必填缺失 → invalid_input" 的口径不符。
// The text returned here is what the model reads, so it must name the offending field.
func checkToolInput(tool Tool, arguments string) string {
	if !json.Valid([]byte(arguments)) {
		return fmt.Sprintf("入参不是合法 JSON：%s", tool.Name())
	}
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(arguments))
	dec.UseNumber() // 大整数保真：参数值不因解码而失真
	if err := dec.Decode(&obj); err != nil || obj == nil {
		// `null` / `[]` / `"x"` 都落这里：入参必须是一个 JSON 对象。
		return fmt.Sprintf("入参必须是 JSON 对象：%s", tool.Name())
	}
	if missing, absent := missingRequiredField(tool.InputSchema(), obj); absent {
		return fmt.Sprintf("入参缺少必填字段 %s：%s", missing, tool.Name())
	}
	return ""
}

// missingRequiredField 从工具**自己的** InputSchema 里读 `required` 并逐个核对键是否存在。
// schema 为空 / 不是对象 / 没有 `required` 键 → 视为不声明必填（跳过），不猜、不报错。
// missingRequiredField reads the tool's own schema; an absent or unreadable `required` key
// simply means the tool declares none.
func missingRequiredField(schema json.RawMessage, args map[string]any) (string, bool) {
	if len(schema) == 0 {
		return "", false
	}
	var shape struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &shape); err != nil {
		return "", false
	}
	for _, name := range shape.Required {
		if _, ok := args[name]; !ok {
			return name, true
		}
	}
	return "", false
}

// truncateToolOutput 按生效上限截断输出并追加标记（§6.2 固定口径 7）。
// 截断点回退到最后一个完整 UTF-8 字符——从中间劈开会写出半个字符，落库后无法修复。
// truncateToolOutput cuts the output at the effective cap and appends the marker, backing
// off to a rune boundary. `maxBytes <= 0` means "no cap".
func truncateToolOutput(content string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(content) <= maxBytes {
		return content, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return content[:cut] + TruncationMarker(maxBytes), true
}

// marshalToolResult 序列化 `result_json`（形态与 `synthesis.go` 的读回方共用同一结构）。
// 序列化失败时以最小合法 JSON 兜底，**绝不写空串**——空值会被恢复路径读成"没有结果体"，
// 而这里明明有结果。
// marshalToolResult serializes the audit result body, falling back to a minimal document
// rather than writing an empty value that recovery would read as "no result at all".
func marshalToolResult(shape toolResultJSON) json.RawMessage {
	raw, err := json.Marshal(shape)
	if err != nil {
		return json.RawMessage(`{"content":""}`)
	}
	return raw
}
