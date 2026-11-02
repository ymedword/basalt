package app

// 本文件是 `core.KnowledgeRunSource` 的装配层实现（US-16 V-6，2026-10-30 用户裁决）：
// 把 Run 侧的三个时刻接到真实的知识库组件上——
//
//  1. **装配**（`RetrieveForRun`）：Agent 声明范围解析 → 查询向量（可选）→ 混合检索 →
//     逐命中**精确读取**校验后落成注入面。检索失败里的"无权限 / 无声明集合"是合法
//     空注入，只有存储违约才返回 error（Run 据此拒绝启动）；
//  2. **轮内复核**（`RevalidateChunks`）：每轮组装前按**当前** ACL / 删除 / 版本重查，
//     只收窄不补入（C-26）。撤权 / 删除 / 换版是结论而不是错误；
//  3. **引用复核**（`ResolveAnswer`）：答案产出后重新精确读取片段，把答案里的标记与
//     **实际进入上下文**的来源逐条比对，四类计数进返回、未解析计数进诊断事件。
//
// 三条纪律：
//   - **先判身份与 scope，再算向量**：向量化是一次外部调用，准入必须发生在它之前
//     （与 `knowledgeSearchAdapter` 同一理由）；领域层稍后还会再判一次，两处不是重复，
//     是"外部调用之前"与"任何查询之前"两个不同时刻的同一条规则；
//   - **引用只能用精确读取的文本**：`knowledge.ResolveCitations` 的 `CitationSource`
//     只由 `SourceView` 填充，检索命中**不得**把片段正文冒充成原文依据；
//   - **诊断可丢弃、审计不可**：诊断写入没有返回值，事件全量脱敏（只带计数与
//     run_id），正文 / 路径 / 集合标识一律不进事件与日志。
//
// This file adapts the run-side knowledge port: the assembly-time retrieval with an
// exact read per hit, the per-round narrowing revalidation, and the answer-time
// citation review against exactly-read text. Identity and scope are checked before the
// embedding call; retrieval hits never become citation sources.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// knowledgeRunSource 编译期锁定端口形状：`core` 的接口一旦漂移，这里先红。
// The compile-time assertion pins the port shape.
var _ core.KnowledgeRunSource = (*knowledgeRunSource)(nil)

// knowledgeRunSource 是 Run 侧知识链路的装配层实现。
//
// `generation` 与向量检索共用**启动期固定**的那一个代际（C-09）：本 US 没有运行期
// 切换代际的端点，逐 Run 重读代际只会让同一次查询跨代合并。`embedder` 是查询侧
// 带审计的装饰器——它与管理面检索共用同一条查询向量化审计路径。
// knowledgeRunSource shares the startup-pinned generation and the audited query
// embedder with the management-side search adapter.
type knowledgeRunSource struct {
	phases     core.PhaseRecorder
	retriever  *knowledge.Retriever
	viewer     *knowledge.SourceViewer
	embedder   knowledge.Embedder
	generation int
	diag       knowledge.DiagnosticRecorder
	logger     *slog.Logger
}

// newKnowledgeRunSource 装配 Run 侧知识端口。`diag` / `logger` 可空（诊断可丢弃、
// 日志回落进程缺省），检索器与来源视图**不可空**——缺了就退回"未接线"，
// 由装配层决定是否传这个端口，而不是运行期猜。
// newKnowledgeRunSource assembles the run-side port; the retriever and viewer are
// required because a half-wired port would silently degrade every run.
func newKnowledgeRunSource(
	retriever *knowledge.Retriever,
	viewer *knowledge.SourceViewer,
	embedder knowledge.Embedder,
	generation int,
	diag knowledge.DiagnosticRecorder,
	logger *slog.Logger,
) *knowledgeRunSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &knowledgeRunSource{
		retriever:  retriever,
		viewer:     viewer,
		embedder:   embedder,
		generation: generation,
		diag:       diag,
		logger:     logger,
	}
}

func (s *knowledgeRunSource) withPhases(p core.PhaseRecorder) *knowledgeRunSource {
	s.phases = p
	return s
}

// RetrieveForRun 装配本次运行的知识注入面。
//
// 返回约定（与 `core.KnowledgeRunSource` 的注释同一口径）：
//   - 没有可注入片段 → `(nil, nil)`：无权限、无声明集合、命中为空、片段在精确读取时
//     已失效，全部走这一条——它们都是**合法状态**，不是故障；
//   - 存储违约 / 端口不可用 → `error`：Run 拒绝启动（零模型调用、零工具执行）。
//
// `StepID` 留空：运行级检索发生在第 1 轮装配之前，没有轮次可填；装配记录与诊断事件
// 因此只挂 `run_id` 主锚点（留空不是"忘了填"，是"没有这一步"）。
// RetrieveForRun returns (nil, nil) for every legal "nothing to inject" state and an
// error only for storage or port failures. StepID stays empty: a run-level retrieval
// happens before round one and has no step to name.
func (s *knowledgeRunSource) RetrieveForRun(ctx context.Context,
	req core.KnowledgeRunRequest,
) (injection *core.KnowledgeInjection, resultErr error) {
	if !req.Principal.Valid() {
		return nil, fmt.Errorf("%w: 知识检索身份不可用", core.ErrUnauthorized)
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		// 空消息没有查询词：不检索、不记诊断（没有可判定的查询）。
		return nil, nil
	}
	if !req.Principal.HasScope(core.ScopeKnowledgeRead) {
		s.recordPermissionDenied(ctx, req)
		s.logger.WarnContext(ctx, "知识库：调用方缺少读 scope，本 Run 不注入知识",
			"run_id", req.RunID, "agent", req.Agent, "scope", core.ScopeKnowledgeRead)
		return nil, nil
	}

	embedding, generation := s.embedQuery(ctx, req, query)
	// 向量化期间可能已取消：**不得**在已结束的 ctx 上继续发起检索（那会把取消误记成
	// 一次检索降级 / 失败）。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	result, err := s.retriever.Retrieve(ctx, knowledge.RetrieveRequest{
		Principal:  req.Principal,
		RunID:      req.RunID,
		AgentName:  strings.TrimSpace(req.Agent),
		Query:      query,
		Embedding:  embedding,
		Generation: generation,
		Budget:     req.Budget,
	})
	if err != nil {
		return s.benignRetrieveError(ctx, req, err)
	}

	if len(result.Hits) > 0 {
		finishRead := core.BeginPhase(ctx, s.phases, core.PhaseSourceRead, core.PhaseKnowledge, req.RunID, "", 0)
		defer func() { finishRead(resultErr) }()
	}
	chunks := make([]core.KnowledgeChunk, 0, len(result.Hits))
	revoked, changed := 0, 0
	for _, hit := range result.Hits {
		ref := core.EvidenceRef{
			Kind:        core.EvidenceKindKnowledge,
			ID:          hit.DocumentID,
			VersionID:   hit.VersionID,
			ContentHash: hit.ContentHash,
		}
		if !ref.Valid() {
			// 端口违约（缺文档或版本标识）：引用无法复核，**不得**注入。
			changed++
			continue
		}
		view, err := s.viewer.SourceView(ctx, req.Principal, knowledge.SourceViewRequest{Ref: ref})
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil && errors.Is(err, core.ErrForbidden):
			revoked++
			continue
		case err != nil:
			return nil, fmt.Errorf("知识片段精确读取（%s）: %w", hit.DocumentID, err)
		case !view.Validity.Valid:
			revoked++
			continue
		case view.Validity.ContentHash != "" && hit.ContentHash != "" &&
			view.Validity.ContentHash != hit.ContentHash:
			// 索引副本与当前版本内容不一致（索引 / 版本漂移）：旧片段**不得**当原文注入。
			changed++
			continue
		}
		// 标记按**最终顺序**连续编号：被剔除的片段不占号，避免提示词里出现空洞。
		chunks = append(chunks, core.KnowledgeChunk{
			Marker:      fmt.Sprintf("c%d", len(chunks)+1),
			SourcePath:  hit.SourcePath,
			HeadingPath: hit.HeadingPath,
			Text:        hit.Text,
			Ref:         ref,
		})
	}
	if revoked > 0 || changed > 0 {
		// 只给计数与原因分类：集合 / 路径 / 片段标识**不得**进日志（C-08 同理）。
		s.logger.WarnContext(ctx, "知识库：检索命中在精确读取时被剔除",
			"run_id", req.RunID, "revoked", revoked, "changed", changed, "kept", len(chunks))
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	return &core.KnowledgeInjection{Chunks: chunks}, nil
}

// embedQuery 计算查询向量；任何失败都只降级（返回空向量、代际 0），由检索器如实
// 记 `vector_unavailable`——**不得**把降级伪装成零命中，也不得让检索整体失败。
// 查询成功时才带代际：没有向量却声称代际会让向量路按错误代际查。
// embedQuery degrades on any failure and only carries the generation when a vector
// actually exists.
func (s *knowledgeRunSource) embedQuery(ctx context.Context, req core.KnowledgeRunRequest,
	query string,
) ([]float32, int) {
	if s.embedder == nil || s.generation < 1 {
		return nil, 0
	}
	finishEmbedding := core.BeginPhase(ctx, s.phases, core.PhaseQueryEmbedding, core.PhaseKnowledge, req.RunID, "", 0)
	batch, err := s.embedder.Embed(ctx, []string{query})
	finishEmbedding(err)
	switch {
	case err != nil:
		if ctx.Err() != nil {
			// 取消 / 截止不是"向量化失败"：交给调用方按取消处理，不写降级日志。
			return nil, 0
		}
		s.logger.WarnContext(ctx, "知识库：运行级查询向量化失败，本次降级为关键词检索",
			"run_id", req.RunID, "error", err)
		return nil, 0
	case len(batch.Vectors) != 1:
		s.logger.WarnContext(ctx, "知识库：运行级查询向量化没有可用向量，本次降级为关键词检索",
			"run_id", req.RunID, "vectors", len(batch.Vectors))
		return nil, 0
	default:
		return batch.Vectors[0], s.generation
	}
}

// benignRetrieveError 把检索失败分流成"合法空注入"与"存储违约"两类。
// 权限拒绝、请求形状被拒都不是基础设施故障——知识是运行增强面，不是运行前提；
// 只有其余错误（存储违约、驱动错误、两路皆不可用）才让 Run 拒绝启动。
// benignRetrieveError separates legal empty injections from real storage failures.
func (s *knowledgeRunSource) benignRetrieveError(ctx context.Context,
	req core.KnowledgeRunRequest, err error,
) (*core.KnowledgeInjection, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	switch {
	case errors.Is(err, core.ErrUnauthorized), errors.Is(err, core.ErrForbidden):
		// Agent 声明的集合全部被 ACL / 项目 / 删除剔除：领域回 ErrForbidden。
		s.recordPermissionDenied(ctx, req)
		return nil, nil
	case errors.Is(err, knowledge.ErrInvalidRequest):
		// 形状被拒（Agent 未注册 / 集合声明与投影不一致）：如实 WARN，不判 Run 失败。
		s.logger.WarnContext(ctx, "知识库：运行级检索请求被拒，本 Run 不注入知识",
			"run_id", req.RunID, "agent", req.Agent, "error", err)
		return nil, nil
	default:
		return nil, fmt.Errorf("运行级知识检索: %w", err)
	}
}

// RevalidateChunks 按当前状态复核注入面：只收窄，不重新检索、不补入新片段。
// 标记保持原样（允许空洞——剔除后的编号不重排），因为提示词里的编号引用以它为准。
// RevalidateChunks only narrows the injection; markers keep their original numbers.
func (s *knowledgeRunSource) RevalidateChunks(ctx context.Context,
	req core.KnowledgeRevalidateRequest,
) (*core.KnowledgeInjection, error) {
	if len(req.Chunks) == 0 {
		return nil, nil
	}
	if !req.Principal.Valid() {
		return nil, fmt.Errorf("%w: 知识注入面复核身份不可用", core.ErrUnauthorized)
	}
	kept := make([]core.KnowledgeChunk, 0, len(req.Chunks))
	revoked, changed := 0, 0
	for _, chunk := range req.Chunks {
		view, err := s.viewer.SourceView(ctx, req.Principal, knowledge.SourceViewRequest{Ref: chunk.Ref})
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil && errors.Is(err, core.ErrForbidden):
			revoked++
			continue
		case err != nil:
			return nil, fmt.Errorf("知识注入面复核（%s）: %w", chunk.Ref.ID, err)
		case !view.Validity.Valid:
			revoked++
			continue
		case view.Validity.ContentHash != "" && chunk.Ref.ContentHash != "" &&
			view.Validity.ContentHash != chunk.Ref.ContentHash:
			changed++
			continue
		}
		kept = append(kept, chunk)
	}
	if revoked > 0 || changed > 0 {
		if s.diag != nil && req.RunID != "" {
			s.diag.Record(ctx, knowledge.DiagnosticEvent{
				RunID: req.RunID, Reason: knowledge.DiagnosticReasonChunkInvalidated, Decision: knowledge.DiagnosticDecisionTrim,
				Attrs: map[string]any{"count": revoked + changed},
			})
		}
		s.logger.WarnContext(ctx, "知识库：注入面片段已失效，自本轮起不再进入提示词",
			"run_id", req.RunID, "revoked", revoked, "changed", changed, "kept", len(kept))
	}
	if len(kept) == 0 {
		return nil, nil
	}
	return &core.KnowledgeInjection{Chunks: kept}, nil
}

// ResolveAnswer 把答案里的引用标记与本次上下文做过精确读取的片段逐条比对。
//
// 复核时**重新读取**每个片段：答案产出时点仍然可读才算"可定位"（C-26 的同一口径）；
// 期间被撤权 / 删除 / 换版的片段保留在 `sources` 里但内容哈希为空，由
// `knowledge.ResolveCitations` 如实归入 `Unsupported`（not_located），**不得**改用
// 检索命中的文本冒充原文。
// ResolveAnswer re-reads each chunk at answer time; a chunk that is no longer readable
// stays in the source list with an empty hash so the resolver reports it as not located.
func (s *knowledgeRunSource) ResolveAnswer(ctx context.Context,
	req core.KnowledgeAnswerRequest,
) (core.KnowledgeAnswerReport, error) {
	if !req.Principal.Valid() {
		return core.KnowledgeAnswerReport{}, fmt.Errorf("%w: 引用复核身份不可用", core.ErrUnauthorized)
	}
	if strings.TrimSpace(req.RunID) == "" {
		return core.KnowledgeAnswerReport{}, fmt.Errorf("引用复核缺少 Run 标识——事件与记录必须挂在 Run 上")
	}
	sources := make([]knowledge.CitationSource, 0, len(req.Chunks))
	for _, chunk := range req.Chunks {
		source := knowledge.CitationSource{
			ChunkID:     chunk.Marker,
			VersionID:   chunk.Ref.VersionID,
			EvidenceRef: chunk.Ref,
		}
		view, err := s.viewer.SourceView(ctx, req.Principal, knowledge.SourceViewRequest{Ref: chunk.Ref})
		switch {
		case err != nil && ctx.Err() != nil:
			return core.KnowledgeAnswerReport{}, ctx.Err()
		case err != nil && errors.Is(err, core.ErrForbidden):
			// 复核时已不可读：按"不可定位"参与判定（哈希留空）。
		case err != nil:
			return core.KnowledgeAnswerReport{}, fmt.Errorf("引用复核精确读取（%s）: %w", chunk.Ref.ID, err)
		case view.Validity.Valid:
			// 只有仍为当前生效版本时才采用精确读取的正文；被取代版本即便回了旧正文
			// 也不能充当引用依据（`locateSources` 只认版本与内容哈希，故此处先挡）。
			source.ContentHash = view.Validity.ContentHash
			source.Text = view.Text
			source.Truncated = view.Truncated
		}
		sources = append(sources, source)
	}

	report := knowledge.ResolveCitations(req.Answer, sources)
	answer := core.KnowledgeAnswerReport{
		Supported:   len(report.Supported),
		Unsupported: len(report.Unsupported),
		Unverified:  len(report.Unverified),
		Unresolved:  len(report.Unresolved),
	}
	if answer.Unresolved > 0 {
		// 只记计数：未解析的标识本身不进诊断（与"被拒资料只计数"同一纪律）。
		s.record(ctx, req.RunID, req.Principal.Actor, knowledge.DiagnosticEvent{
			RunID:  req.RunID,
			Actor:  req.Principal.Actor,
			Reason: knowledge.DiagnosticReasonCitationUnresolved,
			Attrs:  map[string]any{"count": answer.Unresolved},
		})
	}
	return answer, nil
}

// recordPermissionDenied 写一条权限拒绝诊断。没有 `run_id` 的调用不写（不伪造锚点）。
// recordPermissionDenied records the run-scoped denial; an empty run id records nothing.
func (s *knowledgeRunSource) recordPermissionDenied(ctx context.Context, req core.KnowledgeRunRequest) {
	s.record(ctx, req.RunID, req.Principal.Actor, knowledge.DiagnosticEvent{
		RunID:    req.RunID,
		Actor:    req.Principal.Actor,
		Reason:   knowledge.DiagnosticReasonPermissionDenied,
		Decision: knowledge.DiagnosticDecisionDeny,
	})
}

// record 是诊断写入的唯一出口：端口为空或没有主锚点时静默跳过（诊断可丢弃）。
// record is the only diagnostic entry point; a nil port or a missing anchor is a no-op.
func (s *knowledgeRunSource) record(ctx context.Context, runID string,
	actor core.ActorKind, event knowledge.DiagnosticEvent,
) {
	if s.diag == nil || strings.TrimSpace(runID) == "" {
		return
	}
	event.RunID = runID
	event.Actor = actor
	s.diag.Record(ctx, event)
}
