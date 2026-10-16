package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	logger  *slog.Logger
}

// 编译期断言：路由面与单供应商面同形——调用方不必知道自己在跟谁说话。
// Compile-time assertion that the routing face has the same shape as a single provider.
var _ core.ProviderClient = (*ProviderService)(nil)

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

// Chat 是契约 §2.3 流程 B 的落地：三道拒绝都在预写之前（无审计行）；预写失败即拒绝发出；
// 调用返回后无论成败都回填终态（取消路径亦然）。返回的调用结果**不因审计写失败而改写**。
// Chat follows flow B: the three rejections happen before the pre-write (no audit row),
// a failed pre-write refuses the call, and the terminal state is written on both
// outcomes. Audit write failures never rewrite the call result.
func (s *ProviderService) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	// 1. 路由：只查表——不遍历（Go map 遍历顺序随机）、不推断、不取"唯一的那个"当默认。
	client, ok := s.clients[req.Provider]
	if !ok {
		err := fmt.Errorf("%w: %s", core.ErrProviderNotConfigured, req.Provider)
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

	// 3. 预写（留痕先于发出，契约 §7.2）：审计不可用时**拒绝发出**——审计为什么比调用更硬，
	// 见宪法 V：没有留痕的调用在事后无法被复核，等于没发生过。
	beginCtx, cancelBegin := auditContext(ctx)
	callID, err := s.store.BeginCall(beginCtx, core.LLMCall{
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
		return core.ChatResponse{}, err
	}

	// 4. 发出：一次调用、不重试、不 fallback（技术方案 §3.3）。
	start := time.Now()
	resp, chatErr := client.Chat(ctx, req)
	duration := time.Since(start)

	// 5. 终态回填：与预写共用同一把解耦上下文——被取消的调用同样要落终态，那是"为什么停了"
	// 最需要证据的时刻（契约 §4.2）。
	auditCtx, cancelAudit := auditContext(ctx)
	defer cancelAudit()

	if chatErr == nil {
		s.finish(auditCtx, callID, core.LLMCallResult{
			Status:   core.LLMCallSuccess,
			Duration: duration,
			// 三列显式平铺（不嵌 Usage）：审计表的列是平的，回填结构与列同构；
			// 供应商未返回 usage 时为零值——不判失败（约束 11 / FR-006）。
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		})
		return resp, nil
	}

	kind := classifyError(chatErr, ctx.Err())
	s.logger.Warn("模型调用失败", "provider", req.Provider, "model", req.Model,
		"run_id", req.RunID, "error_kind", string(kind), "error", chatErr)
	s.finish(auditCtx, callID, core.LLMCallResult{
		Status:       core.LLMCallFailed,
		ErrorMessage: chatErr.Error(), // 原样记录——脱敏在客户端边界做完（约束 8）
		ErrorKind:    kind,
		Duration:     duration,
	})
	return resp, chatErr
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
func (s *ProviderService) finish(ctx context.Context, id int64, result core.LLMCallResult) {
	if err := s.store.FinishCall(ctx, id, result); err != nil {
		s.logger.Error("审计终态回填失败——行保留 pending，待启动恢复标 unknown", "id", id, "error", err)
	}
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
