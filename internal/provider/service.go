package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件实现**路由 + 两阶段审计写**：查表路由（宪法 III）→ 预写 pending（fail-closed）→
// 一次调用 → 终态回填。
// This file implements routing plus the two-phase audit write: look up the explicit map,
// pre-write a pending row, make exactly one call, then fill in the terminal state.

// auditWriteTimeout 是审计写入的独立短超时（契约 §4.2「缺省 5 秒」；不新增配置键）。
// auditWriteTimeout is the independent short timeout for audit writes.
const auditWriteTimeout = 5 * time.Second

// ProviderService 持有 name → 客户端的显式映射（宪法 III）——装配时一次构建，之后只读。
// ProviderService holds the explicit name-to-client map, built once at assembly and
// read-only afterwards.
type ProviderService struct {
	clients map[string]core.ProviderClient
	models  map[string]string
	store   core.LLMCallStore
	// memoryStore 是 purpose='memory_extract' 的专用预写面（US-22）；nil = 该视图不支持
	// 抽取，`ChatMemoryExtract` 在预写处 fail-closed，**不得**退回 `purpose='chat'`。
	// memoryStore is the dedicated pre-write surface for memory_extract; nil means the
	// view cannot extract and must fail closed rather than fall back to purpose='chat'.
	memoryStore core.MemoryExtractCallStore
	logger      *slog.Logger
	// built 是本视图所绑 Provider 实例的构建证据（US-24 P-21 / FR-24-25）：版本引用 +
	// 凭据 pin。零值 = 未声明（轻装配 / 评测注入），准入据此跳过本地实例比对。
	// built is the local instance's build proof; the zero value means undeclared.
	built core.BuiltVersion
}

// 编译期断言：路由面与单供应商面同形——调用方不必知道自己在跟谁说话。
// Compile-time assertion that the routing face has the same shape as a single provider.
var _ core.ProviderClient = (*ProviderService)(nil)

// 编译期断言：抽取调用面由本服务唯一实现（颗粒度文档 §3.1.6）。
// Compile-time assertion that this service is the extraction call surface.
var _ core.MemoryExtractor = (*ProviderService)(nil)

// NewProviderService 装配服务；映射表与模型表由装配处逐条构造后传入（不遍历推断）。
// NewProviderService wires the service to the maps built entry by entry at assembly time.
func NewProviderService(clients map[string]core.ProviderClient, models map[string]string,
	store core.LLMCallStore, logger *slog.Logger,
) *ProviderService {
	if logger == nil {
		logger = slog.Default()
	}
	return &ProviderService{clients: clients, models: models, store: store, logger: logger}
}

// WithBuiltVersion 给本视图挂上构建证据并返回自身（与 `WithMemoryExtractStore` 同形：
// 装配期一次性配置，运行期只读）。生产目录在构造条目时写入，`Resolve` 用它做
// "本地实例版本证明"；不带证据的视图（零值）由准入跳过比对。
// WithBuiltVersion attaches the instance's build proof at assembly time.
func (s *ProviderService) WithBuiltVersion(built core.BuiltVersion) *ProviderService {
	if s == nil {
		return nil
	}
	s.built = built
	return s
}

// BuiltVersion 报告本视图的构建证据（`core.BuiltVersioned`）。
// BuiltVersion reports this view's build proof.
func (s *ProviderService) BuiltVersion() core.BuiltVersion {
	if s == nil {
		return core.BuiltVersion{}
	}
	return s.built
}

// clientFor 是路由的唯一实现：只查显式映射表——不遍历、不推断、不取"唯一的那个"当默认
// （宪法 III）。聊天与抽取共用，两条路径的语义完全一致。
// clientFor is the single routing implementation; chat and extraction share it.
func (s *ProviderService) clientFor(providerName string) (core.ProviderClient, error) {
	client, ok := s.clients[providerName]
	if !ok {
		return nil, fmt.Errorf("%w: %s", core.ErrProviderNotConfigured, providerName)
	}
	return client, nil
}

// Chat 是契约 §2.3 流程 B 的落地：三道拒绝都在预写之前（无审计行）；预写失败即拒绝发出；
// 调用返回后无论成败都回填终态（取消路径亦然）。返回的调用结果**不因审计写失败而改写**。
// Chat follows flow B: the three rejections happen before the pre-write (no audit row),
// a failed pre-write refuses the call, and the terminal state is written on both
// outcomes. Audit write failures never rewrite the call result.
func (s *ProviderService) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	// 1. 路由：只查表——不遍历（Go map 遍历顺序随机）、不推断、不取"唯一的那个"当默认。
	client, err := s.clientFor(req.Provider)
	if err != nil {
		s.logger.Warn("provider 不在显式映射表内——拒绝发出", "provider", req.Provider,
			"run_id", req.RunID, "error", err)
		return core.ChatResponse{}, err
	}

	// 2. 关联与模型解析：两道拒绝同样在预写之前——「无行 = 确定没执行」（契约 §4.4）。
	if req.RunID == "" || req.Round < 1 {
		err := fmt.Errorf("审计关联标识缺失（provider=%s）：run_id 不得为空、round 必须 >= 1（实际 round=%d）",
			req.Provider, req.Round)
		s.logger.Warn("审计关联标识缺失——拒绝发出", "provider", req.Provider,
			"run_id", req.RunID, "error", err)
		return core.ChatResponse{}, err
	}
	req.Model = s.resolveModel(req)
	if req.Model == "" {
		err := fmt.Errorf("无法解析生效模型（provider=%s）：请求模型与供应商缺省模型都为空", req.Provider)
		s.logger.Warn("模型无法解析——拒绝发出", "provider", req.Provider,
			"run_id", req.RunID, "error", err)
		return core.ChatResponse{}, err
	}
	var flowPermit core.FlowModelPermit
	if req.Flow != nil {
		if req.Flow.Models == nil {
			return core.ChatResponse{}, errors.Join(core.ErrFlowModelNotIssued, core.ErrFlowDispatch)
		}
		var err error
		req, _, err = core.SnapshotFlowModelRequest(ctx, req)
		if err != nil {
			return core.ChatResponse{}, errors.Join(core.ErrFlowModelNotIssued, err)
		}
		prepared, _, err := core.SnapshotFlowModelRequest(ctx, req)
		if err != nil {
			return core.ChatResponse{}, errors.Join(core.ErrFlowModelNotIssued, err)
		}
		flowPermit, err = req.Flow.Models.PrepareModel(ctx, prepared, req.FlowEstimatedInput)
		if err != nil || flowPermit == nil {
			return core.ChatResponse{}, errors.Join(core.ErrFlowModelNotIssued, core.ErrFlowDispatch, err)
		}
	}

	// 3. 预写（留痕先于发出，契约 §7.2）：审计不可用时**拒绝发出**——审计为什么比调用更硬，
	// 见宪法 V：没有留痕的调用在事后无法被复核，等于没发生过。
	beginCtx, cancelBegin := auditContext(ctx)
	callID, err := beginChatCall(beginCtx, s.store, req.Lease, core.LLMCall{
		RunID:      req.RunID,
		SessionID:  req.SessionID,
		Round:      req.Round,
		Provider:   req.Provider,
		Model:      req.Model,
		ConfigHash: req.ConfigHash, // 由运行入口运行期填充（US-4 起）；空则列写 NULL
		CreatedAt:  time.Now(),
	})
	cancelBegin()
	if err != nil {
		s.logger.Error("审计预写失败——拒绝发出该次调用（fail-closed）", "provider", req.Provider,
			"model", req.Model, "run_id", req.RunID, "error", err)
		// 双 `%w`：`core.ErrAuditWrite` 是"确定未发出"的判定锚点（配额结算据此归还预留，
		// C-29），底层仓储错误同时保留——排障要看得到真正的原因。
		// Two %w verbs keep both the sentinel and the underlying store error reachable.
		if flowPermit != nil {
			finish, cancel := auditContext(ctx)
			defer cancel()
			err = errors.Join(err, flowPermit.RecordOutcome(finish, core.FlowModelOutcome{}))
		}
		return core.ChatResponse{}, fmt.Errorf("%w: %w", core.ErrAuditWrite, err)
	}
	if flowPermit != nil {
		grant, _, cloneErr := core.SnapshotFlowModelRequest(ctx, req)
		grantErr := cloneErr
		if grantErr == nil {
			grantErr = flowPermit.BeforeDispatch(ctx, callID, grant)
		}
		if grantErr != nil {
			finish, cancel := auditContext(ctx)
			defer cancel()
			confirmed := s.finish(finish, callID, core.LLMCallResult{Status: core.LLMCallFailed, ErrorKind: core.ErrorKindRuntimeError, ErrorMessage: "flow dispatch refused before model call"})
			return core.ChatResponse{}, errors.Join(core.ErrFlowModelNotIssued, grantErr, flowPermit.RecordOutcome(finish, core.FlowModelOutcome{AuditID: callID, AuditConfirmed: confirmed, Status: core.LLMCallFailed}))
		}
	}

	// 4. 发出：一次调用、不重试、不 fallback（技术方案 §2.4.3）。
	if req.Phases != nil {
		req.Timing = &core.ModelTiming{}
	}
	start := time.Now()
	resp, chatErr := client.Chat(ctx, req)
	duration := time.Since(start)
	// Publish only after the call duration is frozen; transport callbacks do no I/O.
	defer s.recordTransportTiming(ctx, req, callID)

	// 5. 终态回填：与预写共用同一把解耦上下文——被取消的调用同样要落终态，那是"为什么停了"
	// 最需要证据的时刻（契约 §4.2）。
	auditCtx, cancelAudit := auditContext(ctx)
	defer cancelAudit()

	if chatErr == nil {
		confirmed := s.finish(auditCtx, callID, core.LLMCallResult{
			Status:   core.LLMCallSuccess,
			Duration: duration,
			// 三列显式平铺（不嵌 Usage）：审计表的列是平的，回填结构与列同构；
			// 供应商未返回 usage 时为零值——不判失败（约束 11 / FR-006）。
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		})
		if flowPermit != nil {
			if err := flowPermit.RecordOutcome(auditCtx, core.FlowModelOutcome{AuditID: callID, Dispatched: true, AuditConfirmed: confirmed, Status: core.LLMCallSuccess, Usage: resp.Usage}); err != nil {
				return resp, err
			}
			if !confirmed {
				return resp, core.ErrFlowDispatch
			}
		}
		return resp, nil
	}

	kind := classifyError(chatErr, ctx.Err())
	s.logger.Warn("模型调用失败", "provider", req.Provider, "model", req.Model,
		"run_id", req.RunID, "error_kind", string(kind), "error", chatErr)
	confirmed := s.finish(auditCtx, callID, core.LLMCallResult{
		Status:       core.LLMCallFailed,
		ErrorMessage: chatErr.Error(), // 原样记录——脱敏在客户端边界做完（约束 8）
		ErrorKind:    kind,
		Duration:     duration,
	})
	if flowPermit != nil {
		chatErr = errors.Join(chatErr, flowPermit.RecordOutcome(auditCtx, core.FlowModelOutcome{AuditID: callID, Dispatched: true, AuditConfirmed: confirmed, Status: core.LLMCallFailed, Usage: resp.Usage}))
	}
	return resp, chatErr
}

// beginChatCall 选对话预写入口（US-24 T008 / FR-24-36）：零值凭证走普通入口（未接
// 执行权的装配），非零凭证**必须**走 fenced 入口；实现缺失即 fail-closed——装配缺陷
// 不得降级成无保护预写，否则旧持有者仍能在接管后写审计行。
// beginChatCall picks the chat pre-write entry point by the credential and fails closed
// when a fenced entry is required but absent.
func beginChatCall(ctx context.Context, store core.LLMCallStore, token core.LeaseToken,
	call core.LLMCall,
) (int64, error) {
	if token.IsZero() {
		return store.BeginCall(ctx, call)
	}
	fenced, ok := store.(core.FencedChatCallStore)
	if !ok {
		return 0, fmt.Errorf("%w（llm_calls 存储 %T）", core.ErrLeaseFenceUnavailable, store)
	}
	return fenced.FencedBeginCall(ctx, token, call)
}

// beginExtractCall 选抽取预写入口：口径与 beginChatCall 相同，但**不**在两套接口之间
// 互相顶替——抽取凭证缺失时也绝不退回 `purpose='chat'` 的预写（那会写错用途列）。
// beginExtractCall mirrors beginChatCall for the extraction surface; it never falls back
// to the chat entry, which would write the wrong purpose.
func beginExtractCall(ctx context.Context, store core.MemoryExtractCallStore, token core.LeaseToken,
	call core.LLMCall,
) (int64, error) {
	if token.IsZero() {
		return store.BeginMemoryExtractCall(ctx, call)
	}
	fenced, ok := store.(core.FencedMemoryExtractCallStore)
	if !ok {
		return 0, fmt.Errorf("%w（memory_extract 存储 %T）", core.ErrLeaseFenceUnavailable, store)
	}
	return fenced.FencedBeginMemoryExtractCall(ctx, token, call)
}

// WithMemoryExtractStore 返回共享客户端、模型表、日志与聊天审计仓储的新实例，并装上抽取
// 预写面；装配期调用，运行期只读。未设置该仓储时 `ChatMemoryExtract` 拒绝发出（fail-closed）。
// WithMemoryExtractStore returns a new view that shares the routing map, model map, logger
// and chat audit store, plus the extraction pre-write surface.
func (s *ProviderService) WithMemoryExtractStore(store core.MemoryExtractCallStore) *ProviderService {
	clone := *s
	clone.memoryStore = store
	return &clone
}

// ChatMemoryExtract 是唯一允许的抽取调用入口（颗粒度文档 §3.1.6）：查显式映射表 →
// 固定 `round=0` → `BeginMemoryExtractCall(pending)` → 一次调用（不重试、不 fallback、
// 无工具、无 Flow）→ `FinishCall`。任一预写失败返回 `*core.ErrAuditWrite`（包装底层错误），
// 调用不发出。
//
// 截止口径（FR-22-10）：预写用 `context.WithTimeout(ctx, 5s)` 继承工作段截止，**不**
// `WithoutCancel` 外扩；终态回填在**开始回填时**计算 `min(now+5s, HookDeadline)`，再以
// `context.WithDeadline(context.WithoutCancel(ctx), …)` 写入——预先算死会让慢模型正常返回
// 时回填上下文已过期，成功审计落不了库。
// ChatMemoryExtract is the only extraction entry: explicit map, round=0, pre-write,
// exactly one call, terminal write. The finalize deadline is computed when finalize
// starts, never at call start.
func (s *ProviderService) ChatMemoryExtract(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	// 1. 路由与关联校验：全部在预写之前——拒绝即无审计行，且确定没有发出调用。
	client, err := s.clientFor(req.Provider)
	if err != nil {
		s.logger.Warn("provider 不在显式映射表内——抽取拒绝发出", "provider", req.Provider,
			"run_id", req.RunID, "error", err)
		return core.ChatResponse{}, err
	}
	if req.RunID == "" || req.Round != 0 {
		err = fmt.Errorf("抽取调用关联标识非法（provider=%s）：run_id 不得为空、round 必须为 0（实际 round=%d）",
			req.Provider, req.Round)
		s.logger.Warn("抽取调用关联标识非法——拒绝发出", "provider", req.Provider,
			"run_id", req.RunID, "error", err)
		return core.ChatResponse{}, err
	}
	// 抽取调用不是对话轮：Flow 的模型许可与工具定义都不属于它（§3.1.6）。
	if req.Flow != nil {
		err = fmt.Errorf("抽取调用不得携带 Flow（provider=%s）", req.Provider)
		s.logger.Warn("抽取调用携带 Flow——拒绝发出", "provider", req.Provider,
			"run_id", req.RunID, "error", err)
		return core.ChatResponse{}, err
	}
	if len(req.Tools) > 0 {
		err = fmt.Errorf("抽取调用不得携带工具定义（provider=%s，tools=%d）", req.Provider, len(req.Tools))
		s.logger.Warn("抽取调用携带工具定义——拒绝发出", "provider", req.Provider,
			"run_id", req.RunID, "error", err)
		return core.ChatResponse{}, err
	}
	req.Model = s.resolveModel(req)
	if req.Model == "" {
		err = fmt.Errorf("无法解析生效模型（provider=%s）：请求模型与供应商缺省模型都为空", req.Provider)
		s.logger.Warn("模型无法解析——抽取拒绝发出", "provider", req.Provider,
			"run_id", req.RunID, "error", err)
		return core.ChatResponse{}, err
	}
	if s.memoryStore == nil {
		// 装配缺陷：绝不能用聊天预写顶替（那会把用途写成 chat，破坏审计口径）。
		s.logger.Error("抽取预写仓储未装配——拒绝发出该次调用（fail-closed）",
			"provider", req.Provider, "run_id", req.RunID)
		return core.ChatResponse{}, fmt.Errorf("%w: memory_extract store is nil",
			core.ErrLearningExtractorUnavailable)
	}

	// 2. 预写：受工作段截止约束（不 WithoutCancel 外扩），失败即拒绝发出（宪法 V）。
	beginCtx, cancelBegin := context.WithTimeout(ctx, auditWriteTimeout)
	callID, err := beginExtractCall(beginCtx, s.memoryStore, req.Lease, core.LLMCall{
		RunID:      req.RunID,
		SessionID:  req.SessionID,
		Round:      req.Round,
		Provider:   req.Provider,
		Model:      req.Model,
		ConfigHash: req.ConfigHash,
		CreatedAt:  time.Now(),
	})
	cancelBegin()
	if err != nil {
		s.logger.Error("抽取审计预写失败——拒绝发出该次调用（fail-closed）", "provider", req.Provider,
			"model", req.Model, "run_id", req.RunID, "error", err)
		return core.ChatResponse{}, fmt.Errorf("%w: %w", core.ErrAuditWrite, err)
	}

	// 3. 发出：一次调用、不重试、不 fallback。
	start := time.Now()
	resp, callErr := client.Chat(ctx, req)
	duration := time.Since(start)

	// 4. 终态回填：截止在**开始回填时**才计算（慢调用正常返回仍要能写 success）。
	finalizeCtx, cancelFinalize := memoryExtractFinalizeContext(ctx, req.HookDeadline)
	defer cancelFinalize()

	if callErr == nil {
		s.finish(finalizeCtx, callID, core.LLMCallResult{
			Status:           core.LLMCallSuccess,
			Duration:         duration,
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		})
		return resp, nil
	}

	kind := classifyError(callErr, ctx.Err())
	s.logger.Warn("抽取模型调用失败", "provider", req.Provider, "model", req.Model,
		"run_id", req.RunID, "error_kind", string(kind), "error", callErr)
	s.finish(finalizeCtx, callID, core.LLMCallResult{
		Status:       core.LLMCallFailed,
		ErrorMessage: callErr.Error(), // 原样记录——脱敏在客户端边界做完（约束 8）
		ErrorKind:    kind,
		Duration:     duration,
	})
	return resp, callErr
}

// memoryExtractFinalizeContext 在开始回填时计算截止：`min(now+5s, HookDeadline)`；零值
// HookDeadline 表示不额外限制。取消继承被摘除——被取消 / 超时的调用同样要落终态。
// memoryExtractFinalizeContext computes min(now+5s, HookDeadline) when finalize starts.
func memoryExtractFinalizeContext(ctx context.Context, hookDeadline time.Time) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(auditWriteTimeout)
	if !hookDeadline.IsZero() && hookDeadline.Before(deadline) {
		deadline = hookDeadline
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}

func (s *ProviderService) recordTransportTiming(ctx context.Context, req core.ChatRequest, callID int64) {
	if req.Timing == nil {
		return
	}
	for _, captured := range []*core.PhaseTiming{req.Timing.Wait, req.Timing.Read} {
		if captured == nil {
			continue
		}
		measurement := *captured
		measurement.RunID, measurement.StepID, measurement.CallID = req.RunID, strconv.Itoa(req.Round), callID
		core.RecordPhaseTiming(ctx, req.Phases, measurement)
	}
}

// Pinned 返回某个 Provider 的**准入固定视图**（人工 review ② 缺陷 2）：路由表只含这一条，
// 模型表、审计仓储与日志器共用。`RunRuntime` 发放的就是它——一个 Run 的模型调用因此只查
// 准入时固定下来的那一行（宪法 III），发布换表不影响在途运行；调用仍走本服务的两阶段
// 审计（宪法 V），**不得**退回裸协议客户端。名字不在映射表 → false，由准入失败关闭。
// Pinned returns a one-entry routing view that shares the audit store, so a run can
// neither reach another provider nor bypass the two-phase audit.
func (s *ProviderService) Pinned(name string) (*ProviderService, bool) {
	client, ok := s.clients[name]
	if !ok {
		return nil, false
	}
	models := make(map[string]string, 1)
	if model, ok := s.models[name]; ok {
		models[name] = model
	}
	return &ProviderService{
		clients:     map[string]core.ProviderClient{name: client},
		models:      models,
		store:       s.store,
		memoryStore: s.memoryStore,
		logger:      s.logger,
		// 构建证据随固定视图一起交出（US-24 P-21）：丢了它，准入就无法证明运行时
		// 实际拿到的实例是目标版本。
		built: s.built,
	}, true
}

// SummaryPinned 返回某个 Provider 的**裸协议客户端**，只服务会话压缩的摘要调用
// （C-25）。理由与 `catalogProviderRouter.SummaryPinned` 逐字相同：摘要不是对话轮，
// 它自己的 `purpose='compaction'` 审计与配额由压缩接线在发出之前完成。
// 本方法**不**发放给运行时：`RunRuntime.Provider` 仍然只发 `Pinned` 的审计视图。
// SummaryPinned exposes the raw client for the summary call only; the runtime still
// only ever receives the audited pinned view.
func (s *ProviderService) SummaryPinned(name string) (core.ProviderClient, bool) {
	client, ok := s.clients[name]
	if !ok {
		return nil, false
	}
	return client, true
}

// resolveModel 解析生效模型：请求模型 > 供应商缺省模型；都空则返回空串（调用方据此拒绝）。
// resolveModel prefers the requested model and falls back to the provider default.
func (s *ProviderService) resolveModel(req core.ChatRequest) string {
	if req.Model != "" {
		return req.Model
	}
	return s.models[req.Provider]
}

// finish 是终态回填的**唯一出口**：失败只记 ERROR——绝不 panic、绝不覆盖调用结果。
// 行会留在 pending，由下次启动的恢复扫描标 unknown（契约 §7.2 残余窗口；扫描本体归 US-4）。
// finish is the only exit for the terminal write: failures are logged, never panicked
// and never allowed to rewrite the call result.
func (s *ProviderService) finish(ctx context.Context, id int64, result core.LLMCallResult) bool {
	if err := s.store.FinishCall(ctx, id, result); err != nil {
		s.logger.Error("审计终态回填失败——行保留 pending，待启动恢复标 unknown", "id", id, "error", err)
		return false
	}
	return true
}

// auditContext 是审计写入的上下文（契约 §4.2：WithoutCancel + 独立短超时）——预写与终态
// 回填**共用**，单点定义避免两处各写一份而漂移。
// auditContext is shared by both audit writes so the decoupling rule has a single home.
func auditContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
}

// classifyError 把错误归到七类字面量之一（契约 §7.3）：客户端已判好的类别照用；
// 错误链上带 ctx 错误的记 timeout（归并口径：Canceled 与 DeadlineExceeded 同值，链保留）；
// 其余（第三方实现的裸错误）兜底记 unknown——unknown **不是**恢复路径专属。
// classifyError maps an error onto one of the seven kinds: the client's verdict is kept,
// context errors fold into timeout, and anything else falls back to unknown.
func classifyError(err error, ctxErr error) core.ErrorKind {
	var callErr *CallError
	if errors.As(err, &callErr) {
		switch callErr.Kind {
		case core.ErrorKindExternalError, core.ErrorKindTimeout:
			return callErr.Kind
		default:
			// 类别不是本 US 的取值面（external_error / timeout）：不把来源不明的字面量
			// 抄进审计，兜底记 unknown。
			// Only the kinds this US produces are trusted; anything else is unknown.
			return core.ErrorKindUnknown
		}
	}
	if isContextError(err) || isContextError(ctxErr) {
		return core.ErrorKindTimeout
	}
	return core.ErrorKindUnknown
}
