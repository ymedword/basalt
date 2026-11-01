package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// quotaSettleTimeout 是配额结算的独立短超时：结算发生在调用返回之后，**不得**因为
// 调用方上下文已取消就丢掉一次真实消耗的记账（与审计终态回填同一纪律）。
// quotaSettleTimeout bounds settlement, which must survive the caller's cancellation.
const quotaSettleTimeout = 5 * time.Second

// 本文件是自实现的 ReAct 循环本体（颗粒度文档 §2.3 流程 B；技术方案 §2.2.1 是算法主源——
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
	// assembleCompactor 是压缩接缝：nil = 未装配压缩（行为与 P-12 之前逐字一致）。
	// 每次 `run` 只调用一次，入参是本次 Run 的冻结环境——压缩器按 Run 构造，摘要调用
	// 因此拿到的是该 Run 准入时固定的那张映射表，而不是事后按名字重新解析。
	assembleCompactor func(*RunEnv) ContextCompactor
	// compactionOutputReserve 只来自 `compaction.output_reserve_tokens`：它是"给对话回复
	// 留的空间"，**不得**用协议余量 `ReserveTokens` 顶替（C-31）。
	compactionOutputReserve int
	// quota 是图 6 [5] 的配额端口（C-29 / C-35）；nil = 未接线（轻装配与既有单测）：
	// 每次调用照常发出，不预留、不结算——行为与接线前逐字一致。
	// A nil quota port means stage [5] is not wired; every call proceeds unreserved.
	quota CallQuota
	// assembly 是装配记录的写入端口（V-7）；nil = 未接线（轻装配与既有单测）：
	// `Build` 照常算出记录，只是不落库——行为与接线前逐字一致。
	// A nil assembly port keeps the pre-wiring behaviour: the record is computed but
	// never persisted.
	assembly AssemblyRecorder
}

// NewReActLoop 组装循环；`logger` 为空时用进程缺省日志器。
// NewReActLoop assembles the loop; a nil logger falls back to the default one.
func NewReActLoop(builder *PromptBuilder, executor *ToolExecutor, logger *slog.Logger) *ReActLoop {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReActLoop{builder: builder, executor: executor, logger: logger}
}

// WithCompaction 接上会话压缩并返回自身（链式，`NewReActLoop` 的既有调用点不受影响）。
// `outputReserve` 取 `compaction.output_reserve_tokens`；`assemble` 按本次 Run 的冻结
// 环境构造压缩器——返回 nil 表示本次 Run 不压缩（例如知识库 / 压缩未启用）。
// WithCompaction wires compaction; the assembler receives this run's frozen environment
// so the summary call can only use the provider table pinned at admission.
func (l *ReActLoop) WithCompaction(outputReserve int, assemble func(*RunEnv) ContextCompactor) *ReActLoop {
	l.compactionOutputReserve = outputReserve
	l.assembleCompactor = assemble
	return l
}

// WithCallQuota 接上图 6 [5] 的配额端口并返回自身（链式，既有调用点不受影响）。
// `q` 为 nil 时保持"不预留"——与未调用本方法等价。
// WithCallQuota wires admission stage [5]; a nil port keeps calls unreserved.
func (l *ReActLoop) WithCallQuota(q CallQuota) *ReActLoop {
	l.quota = q
	return l
}

// WithAssemblyRecorder 接上装配记录的写入端口并返回自身（链式，既有调用点不受影响）。
// `r` 为 nil 时保持"只算不落库"——与未调用本方法等价。
// WithAssemblyRecorder wires the assembly recorder; a nil port keeps the record
// computed-but-unpersisted behaviour.
func (l *ReActLoop) WithAssemblyRecorder(r AssemblyRecorder) *ReActLoop {
	l.assembly = r
	return l
}

// admitCall 申请本次模型调用的配额（图 6 [5]）。幂等键按 C-29 生成：`step_id` 取本轮
// 轮次（一轮恰好一次真实调用），`attempt_id` 取 `a1`——同一 Run 内本循环**不重发**
// 同一轮，若将来引入重发必须递增 attempt 而不是复用本键。
// admitCall reserves stage [5]; the key follows C-29 and assumes one attempt per round.
func (l *ReActLoop) admitCall(
	ctx context.Context, runID string, round int, env *RunEnv, prompt Prompt,
) (CallQuotaTicket, error) {
	if l == nil || l.quota == nil {
		return nil, nil
	}
	return l.quota.AdmitCall(ctx, CallQuotaRequest{
		Principal:      env.Req.Principal,
		RunID:          runID,
		Round:          round,
		EstimatedInput: estimateTokens(prompt),
		IdempotencyKey: fmt.Sprintf("run:%s:chat:%d:a1", runID, round),
	})
}

// settleCall 按"确知程度"分流本次预留（C-29）：
//
//	已知用量        → CommitCall（`pending → committed`，余额自动释放）
//	确定未发出      → ReleaseCall（审计预写失败 / 路由查表不中）
//	发出后结局未知  → 保持 `pending`，由管理面 `Reconcile` 收口（**不**按零提交）
//
// 结算用独立上下文：调用已经发生过，用量与额度都得落地，取消只影响这次 Run 的结果。
// settleCall commits known usage, releases provably-unsent calls and leaves the rest
// pending; settlement survives cancellation.
func (l *ReActLoop) settleCall(
	ctx context.Context, runID string, round int, ticket CallQuotaTicket, usage Usage, callErr error,
) {
	if l == nil || l.quota == nil || ticket == nil {
		return
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), quotaSettleTimeout)
	defer cancel()

	switch {
	case callErr == nil && usageReported(usage):
		if err := l.quota.CommitCall(settleCtx, ticket, usage); err != nil {
			l.logger.Error("配额结算失败——预留留在原状态待管理面核对",
				"run_id", runID, "round", round, "op", "commit", "error", err)
		}
	case callErr == nil:
		// 调用成功但供应商没给用量：全零既可能是"确实零消耗"也可能是"没返回"，
		// 在循环这一层无法区分——保持 pending，不臆断。
		l.logger.Warn("模型调用未返回用量——配额预留保持 pending 待管理面核对",
			"run_id", runID, "round", round, "op", "commit")
	case callNotIssued(callErr):
		if err := l.quota.ReleaseCall(settleCtx, ticket, callErr); err != nil {
			l.logger.Error("配额归还失败——预留可能滞留 pending",
				"run_id", runID, "round", round, "op", "release", "error", err)
		}
	default:
		// 已发出但结局未知（超时 / 取消 / 供应商拒绝）：预留继续占额度。
		l.logger.Warn("模型调用结局未知——配额预留保持 pending 待管理面核对",
			"run_id", runID, "round", round, "op", "hold")
	}
}

// callNotIssued 判定错误是否**确定未发出**。这两个判定点都在 `ProviderService.Chat`
// 的发出动作之前：审计预写失败（fail-closed）与路由查表不中。其余错误一律按"可能
// 已经发出"处理——宁可让预留等管理面核对，也不把真实消耗释放掉。
// callNotIssued reports errors raised strictly before the request went out.
func callNotIssued(err error) bool {
	return errors.Is(err, ErrAuditWrite) || errors.Is(err, ErrProviderNotConfigured)
}

// recordAssembly 把本轮装配记录交给写入器（V-7）。
//
// 三个"只有调用方知道"的字段在这里补齐：Run 标识、轮次（= `step_id`，与配额幂等键
// `run:<id>:chat:<round>:a1` 同一口径）、冻结快照里的 Agent 与模型。**每轮恰好一次**：
// 调用点在 `Build` 之后、`Compression` 并入之后的唯一位置——提前交出去会丢掉压缩结论，
// 放到模型调用之后则会多出一条"装配了但没调用"的错位证据。
//
// `LLMCallID` 保持 0（写入器落 NULL）：装配发生在模型调用之前，标识此时还不存在；
// **不**事后回填——那需要第二条更新路径，而记录本就可丢弃，不值得为它开一条写入面。
// `PromptVersion` 留空：chat 的提示词由代码拼装、没有模板版本；本 US 只产生
// `purpose='chat'` 的装配记录，摘要模板版本记在压缩条目的 `prompt_version` 列。
// recordAssembly stamps the correlation fields only the caller knows and hands the
// record over; it is called exactly once per round, after the compression verdict is
// merged and before the model call. LLMCallID stays 0 by design.
func (l *ReActLoop) recordAssembly(ctx context.Context, runID string, round int, env *RunEnv, record AssemblyRecord) {
	if l == nil || l.assembly == nil {
		return
	}
	record.RunID = runID
	record.StepID = strconv.Itoa(round)
	record.Purpose = PurposeChat
	record.Agent = env.agentName()
	record.ModelVersion = env.ProviderModel()
	l.assembly.Record(ctx, record)
}

// revalidateKnowledge 在每轮组装前复核注入面的当前可见性（V-6）。
//
// 三条边界：**只收窄**（不重新检索、不补入新片段）；**失败即丢弃**（复核是存储调用，
// 失败时片段可能已撤权——fail-closed 的做法是让本轮起看不到知识，而不是照旧注入）；
// **不判运行失败**（知识是增强面，存储抖动记 WARN 即可；与装配记录同纪律）。
// 取消导致的复核失败不记 WARN——取消是正常收场路径，否则每个被取消的运行都会多一条噪音。
// revalidateKnowledge re-checks the injected chunks before each assembly. It only narrows
// the set, drops everything on failure, and never fails the run; a cancelled context is
// not logged because cancellation is a normal exit.
func (l *ReActLoop) revalidateKnowledge(ctx context.Context, runID string, env *RunEnv) {
	if env == nil || env.knowledgeSource == nil || env.knowledge.Empty() {
		return
	}
	reviewed, err := env.knowledgeSource.RevalidateChunks(ctx, KnowledgeRevalidateRequest{
		Principal: env.Req.Principal,
		RunID:     runID,
		Chunks:    env.knowledge.Chunks,
	})
	if err != nil {
		env.knowledge = nil
		if ctx.Err() == nil {
			l.logger.Warn("知识注入面复核失败——本轮起丢弃全部片段",
				"run_id", runID, "op", "knowledge_revalidate", "error", err)
		}
		return
	}
	env.knowledge = reviewed
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

		// ①½ 知识注入面的轮内复核（V-6）：注入面本身运行级冻结，**权限不冻结**——撤权 /
		// 删除 / 换版必须从下一轮提示词里立即消失（C-26：每次精确使用重查当前 ACL）。
		// 复核只**收窄**注入面：不重新检索、不补入新片段。
		// Chunks are frozen per run, but their visibility is re-checked before every
		// assembly: revocation or deletion must vanish from the very next prompt.
		l.revalidateKnowledge(ctx, runID, env)

		// ② 轮内组装：**每轮重建**（上一轮追加的工具结果必须进本轮历史）。
		// 不可裁部分超限 → failed + 点名 `budget.context_tokens`，**零 LLM 调用**。
		// 压缩在组装**之前**判定（它决定本轮注入哪份摘要、保留哪段历史）；
		// 装配记录由本循环在压缩结论并入后交给写入器（V-7）；未接线时只算不落库，
		// 行为与接线前逐字一致（P-12）。
		outcome := l.compact(ctx, runID, round, env)
		if outcome.abort != nil {
			// 压缩的接线 / 存储失败是基础设施失败，不静默降级成"看起来正常的一轮"；
			// 取消不算失败（轮首与轮内口径一致）。
			if ctxErr := ctx.Err(); ctxErr != nil {
				return env.cancelled(ctxErr, round-1, toolCalls), nil
			}
			return env.fail(outcome.abort, round-1, toolCalls), nil
		}
		prompt, record, err := l.builder.Build(ctx, env, outcome.inputs)
		if err != nil {
			return env.fail(err, round-1, toolCalls), nil
		}
		record.Compression = outcome.record
		// ②¼ 装配证据落库（V-7）：压缩结论并入之后、模型调用之前交出本轮记录。
		// 写入**无错误返回**：记录可丢弃，Run 不因证据写失败而失败。
		l.recordAssembly(ctx, runID, round, env, record)

		// ②½ 配额预留（图 6 [5]）：在唯一的模型调用入口**之前**、且在 ProviderService 的
		// 审计预写 [6] 之前申请本次调用的额度。拒绝属"准入期拒绝"（契约 §2：error 只表达
		// 调用方问题）——Run 尚未产生任何消耗，因此**不**记成 failed 的 RunResult，
		// 而是原样上抛给运行入口（由它收口账本并映射 429）。
		// [5] runs before [6]; a refusal is an admission-class rejection, not a result.
		ticket, quotaErr := l.admitCall(ctx, runID, round, env, prompt)
		if quotaErr != nil {
			// 取消优先：上下文已取消时不是配额拒绝，按取消收场（不计当前轮）。
			if ctxErr := ctx.Err(); ctxErr != nil {
				return env.cancelled(ctxErr, round-1, toolCalls), nil
			}
			return RunResult{}, quotaErr
		}

		// ③ 唯一的模型调用入口：路由查表 + 两阶段审计都在 ProviderService 内（约束 4），
		// 循环不碰审计仓储、不自建第二套。
		resp, err := env.client().Chat(ctx, env.llmEnv(runID, round, prompt))
		// 结算先于终态判定：无论下面走 cancelled / failed / 完成，预留都必须当场分流。
		l.settleCall(ctx, runID, round, ticket, resp.Usage, err)
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
		// 来源依赖随转述传播：assistant 消息带上**本次模型输入实际包含**的来源集合
		// （`Prompt.EvidenceRefs` 由组装层算出，审计集不参与），与消息同事务落库——
		// 撤权后这条转述与工具原文一起被替换（C-26）。
		assistant := env.assistantMessage(resp)
		assistant.EvidenceRefs = prompt.EvidenceRefs
		if err := env.Session.AppendMessage(ctx, runID, assistant); err != nil {
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
			// 步骤定位由循环给出（V-8）：`step_id` 取轮次、`seq` 同轮同值——执行器不知道
			// 自己在第几轮，而诊断的"同一 correlation 内局部有序"只能由知道轮次的一侧补齐。
			// The loop supplies the step locator because only it knows the round.
			result := l.executor.ExecuteStep(ctx, env.ToolEnv(ctx, runID), env.ToolSet, call,
				ToolStep{StepID: strconv.Itoa(round), Seq: int64(round)}) // 流程 D（返回结果，**不写消息**）
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

// compactionOutcome 是一轮压缩的结论：注入用的装配输入、写进装配记录的压缩段，
// 以及"必须终止本 Run"的接线级错误（`abort`）。
// A round's compaction outcome: the assembly inputs, the record fragment, and a
// wiring-level abort.
type compactionOutcome struct {
	inputs ContextInputs
	record CompressionRecord
	abort  error
}

// compact 在一轮组装前判定压缩，并把结果翻译成本轮的 `ContextInputs`。
//
// 三条纪律：
//   - 压缩器**按 Run 构造**（`assembleCompactor`）：摘要调用只查该 Run 准入时冻结的
//     Provider 映射，不按名字重新解析（原则 III）；
//   - `Budget` / `Fixed` / `History` 全部**显式**填写，缺一项引擎就拒绝——绝不让引擎
//     猜固定开销（C-31）；
//   - 降级是**结果**不是错误：`Degraded` 照常组装（走整组裁剪），原因码进装配记录。
//
// compact decides this round's compaction; a degraded result still assembles normally,
// and only a wiring-level failure aborts the run.
func (l *ReActLoop) compact(ctx context.Context, runID string, round int, env *RunEnv) compactionOutcome {
	none := compactionOutcome{record: CompressionRecord{Decision: DecisionNone}}
	if l.assembleCompactor == nil {
		return none
	}
	compactor := l.assembleCompactor(env)
	if compactor == nil {
		return none
	}

	request := CompactRequest{
		Principal: env.Req.Principal,
		RunID:     runID,
		SessionID: env.Req.SessionID,
		// 每次真实尝试一个标识：同一轮重复进入压缩也要能区分（配额幂等键取它）。
		AttemptID: runID + "/round-" + strconv.Itoa(round),
		Budget: ContextBudget{
			ContextTokens:    effectiveContextTokens(env.Budget.ContextTokens),
			ProtocolReserve:  ReserveTokens,
			OutputReserve:    l.compactionOutputReserve,
			EstimatorVersion: EstimateVersion,
		},
		Fixed:            l.builder.FixedTokens(env),
		History:          env.Session,
		EstimatorVersion: EstimateVersion,
	}
	result, err := compactor.Compact(ctx, request)
	if err != nil {
		return compactionOutcome{abort: err}
	}
	if result.Degraded {
		return compactionOutcome{
			record: CompressionRecord{
				Enabled:    true,
				Decision:   DecisionDegraded,
				ReasonCode: result.ReasonCode,
			},
		}
	}
	if result.Entry == nil {
		return none
	}
	return compactionOutcome{
		inputs: ContextInputs{Summary: result.Entry},
		record: CompressionRecord{
			Enabled:              true,
			Decision:             DecisionCompressed,
			CompactionID:         result.Entry.CompactionID,
			CoveredFromMessageID: result.Entry.CoveredFromMessageID,
			CutMessageID:         result.Entry.CutMessageID,
			FirstKeptMessageID:   result.Entry.FirstKeptMessageID,
			StrategyVersion:      result.Entry.StrategyVersion,
			PromptVersion:        result.Entry.PromptVersion,
		},
	}
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
