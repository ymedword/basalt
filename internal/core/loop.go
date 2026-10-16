package core

import (
	"context"
	"log/slog"
)

// 本文件是自实现的 ReAct 循环本体（颗粒度文档 §2.3 流程 B；技术方案 §4.1 是算法主源——
// 两处差异逐条冻结在颗粒度文档 §2.4 注 12，本文件按该注落地）。
// This file is the hand-rolled ReAct loop (flow B). The design is ours: no agent framework
// takes over the iteration, and no protocol SDK auto-executes tools (constitution I/II).
//
// 循环只做四件事：组装 → 调用 → **先落库** → 有工具则串行执行。三条边界纪律：
//   - **留痕顺序**：assistant 消息（含 `tool_calls`）先入库、再执行工具——恢复扫描的半截组
//     判据依赖这个顺序，颠倒过来"执行中崩溃"就无法与"还没执行"区分；
//   - **追加唯一路径**：模型响应与工具结果都由本循环经 `Session` 写入，`ToolExecutor` 只返回
//     `Result`（宪法 II）；`complete` 只组装 `RunResult`、不再写库；
//   - **取消优先于预算**：每次执行工具之前两道检查按此顺序，轮首取消不计当前轮。
// The loop assembles, calls, persists the assistant message, then runs tools strictly in
// series. Persisting before executing is what makes a crash mid-tool distinguishable from
// "never ran"; the loop is the only appender; cancellation outranks the tool budget.

// ReActLoop 驱动一次运行的迭代。构造入参全是**显式依赖**（提示词组装的解析器、工具调度器、
// 日志器），没有包级状态、没有异步编排。
// ReActLoop drives the iterations of one run. Its dependencies are explicit and it holds
// no package-level state.
type ReActLoop struct {
	builder  *PromptBuilder
	executor *ToolExecutor
	logger   *slog.Logger
}

// NewReActLoop 组装循环；`logger` 为空时用进程缺省日志器。
// NewReActLoop assembles the loop; a nil logger falls back to the default one.
func NewReActLoop(builder *PromptBuilder, executor *ToolExecutor, logger *slog.Logger) *ReActLoop {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReActLoop{builder: builder, executor: executor, logger: logger}
}

// run 执行一轮运行的完整迭代，返回终态结果。
// **error 恒 nil**（契约 §1.2）：运行期失败一律体现在 `RunResult.Status` 里——只有调用方
// 问题（请求非法 / Agent 不存在 / 同会话忙 / 实例满）才走函数级 error，且那些在入口判完。
// run returns the terminal outcome. The error is always nil: run-time failures show up in
// the status, and caller-side problems never reach the loop.
//
// `env.Budget.MaxIterations <= 0` 时循环体一次都不进，直接落 `limit_reached`——预算为零是
// 合法输入（调用方明确不要模型调用），不是异常路径。
// A non-positive iteration cap never enters the loop body and lands on limit_reached.
func (l *ReActLoop) run(ctx context.Context, runID string, env *RunEnv) (RunResult, error) {
	toolCalls := 0
	for round := 1; round <= env.Budget.MaxIterations; round++ { // round 从 1 起（契约 §7.1 的 round 语义）
		// ① 轮首查取消：本轮的模型调用未发出，故**不计**当前轮（iterations = round-1）。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return env.cancelled(ctxErr, round-1, toolCalls), nil
		}

		// ② 轮内组装：**每轮重建**（上一轮追加的工具结果必须进本轮历史）。
		// 不可裁部分超限 → failed + 点名 `budget.context_tokens`，**零 LLM 调用**。
		prompt, err := l.builder.Build(env)
		if err != nil {
			return env.fail(err, round-1, toolCalls), nil
		}

		// ③ 唯一的模型调用入口：路由查表 + 两阶段审计都在 ProviderService 内（约束 4），
		// 循环不碰审计仓储、不自建第二套。
		resp, err := env.client().Chat(ctx, env.llmEnv(runID, round, prompt))
		if err != nil {
			// **先看 ctx.Err()**：取消不是失败——截止时间到与被取消都归 cancelled，
			// 而非 cancelled 的错误（含预写失败被拒发）才是 failed。
			if ctxErr := ctx.Err(); ctxErr != nil {
				return env.cancelled(ctxErr, round-1, toolCalls), nil
			}
			return env.fail(err, round-1, toolCalls), nil
		}

		// ④ **先落 assistant**（带工具调用 id / 思考内容 / 供应商扩展字段，三条硬约束原样带上），
		// 再考虑执行工具；写失败 → failed（不得先执行工具再补记）。
		if err := env.Session.AppendMessage(ctx, runID, env.assistantMessage(resp)); err != nil {
			return env.fail(err, round, toolCalls), nil
		}

		// ⑤ 无工具调用 = 完成：首次纯文本完成时 round=1 → `Iterations = 1`。
		if len(resp.ToolCalls) == 0 {
			return env.complete(resp.Content, round, toolCalls), nil
		}

		// ⑥ **串行**逐个执行——每次执行之前两道检查（取消 → 工具预算），顺序固定。
		for _, call := range resp.ToolCalls {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// 先补齐（独立收尾上下文——父 ctx 已取消也得写得进去）再落终态：
				// 组内未执行的调用取"已取消"合成文本，而不是预算文本。
				l.completeGroup(ctx, runID, env, resp.ToolCalls, ReasonCancelled)
				return env.cancelled(ctxErr, round, toolCalls), nil
			}
			if env.Budget.ToolCallsExhausted(toolCalls) {
				l.completeGroup(ctx, runID, env, resp.ToolCalls, ReasonToolBudget)
				return env.limitReached(ReasonToolBudget, round, toolCalls), nil
			}
			result := l.executor.Execute(ctx, env.ToolEnv(ctx, runID), env.ToolSet, call) // 流程 D（返回结果，**不写消息**）
			if err := env.Session.AppendToolResult(ctx, runID, call, result); err != nil {
				// 工具结果写不进去 = 历史会缺一条 tool 消息（供应商会拒收整段）→ Run failed；
				// 补齐是幂等兜底，没补上的留给下次启动的恢复扫描。
				l.completeGroup(ctx, runID, env, resp.ToolCalls, ReasonRunFailed)
				return env.fail(err, round, toolCalls), nil
			}
			toolCalls++
		}

		// 组返回后复查取消：**取消优先于预算**——本组已执行完，剩余调用按"已取消"补齐。
		if ctxErr := ctx.Err(); ctxErr != nil {
			l.completeGroup(ctx, runID, env, resp.ToolCalls, ReasonCancelled)
			return env.cancelled(ctxErr, round, toolCalls), nil
		}
	}

	// 循环外：迭代耗尽发生在**所有工具结果都已追加之后**（每组跑完才进下一轮），故按定义
	// 无半截组可补——这里不调补齐（传 nil 调用恒为 no-op，留着只会误导读者以为存在该分支）。
	return env.limitReached(ReasonIterationLimit, env.Budget.MaxIterations, toolCalls), nil
}

// completeGroup 补齐全组缺失的工具结果，失败只记 ERROR：Run **照常收场**，半截组留给下次
// 启动的恢复扫描（约束 24 ③）。补齐动作本身有独立收尾上下文，不受父 ctx 取消影响。
// completeGroup repairs a half group and only logs on failure — the run still finishes, and
// recovery picks up whatever could not be written.
func (l *ReActLoop) completeGroup(ctx context.Context, runID string, env *RunEnv, calls []ToolCall, reason string) {
	if err := env.Session.CompleteToolGroup(ctx, runID, calls, reason); err != nil {
		l.logger.Error("半截工具组补齐失败", "run_id", runID, "reason", reason, "error", err)
	}
}
