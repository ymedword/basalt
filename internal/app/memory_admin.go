package app

// 本文件是**管理面记忆**的编排层（颗粒度文档 C-21 / 契约 `run-and-errors.md` §9.3）：
// 结构化读模型 + 四个写动作（显式提议 / 确认 / 纠正 / 软删）。它实现 `core.MemoryAdmin`
// 端口，把 web 的管理请求转成领域动作，并按宪法第 5 条为每个写动作编排
// 「预写 `pending` → 动目标 → 回填终态」。
//
// 三条硬边界（与 C-21 同一口径，编译器不管、只能靠这里守）：
//  1. **没有自动抽取**——本文件只在管理台显式提交时被调用；Run 结束路径**不得**引用它；
//  2. **只有 Confirm 能晋升**——Propose 永远落 candidate，纠正只作用于既有条目；
//  3. **候选不参与召回**——召回面在 `internal/memory`，这里只读条目、从不召回。
//
// 审计纪律：正文**不入** `detail_json`（只落规范化正文的 sha256），因为记忆正文可能含
// 用户隐私；页面读正文走读模型接口，审计只回答"改的是哪一条、改前改后是什么状态"。
// This file orchestrates the management side of memory: the structured read model plus
// the four explicit write actions, each wrapped in a pending → act → finish audit. The
// entry body never reaches detail_json — only its sha256 — because memory text is user
// content; the page reads bodies through the read model instead.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// memoryAdminToolPropose / …Correct / …Delete 是入参错误的点名标识：与审计里的
	// `action` 用同一批契约字面量，**不另造工具名**（同一个写面在审计与错误里必须同名）。
	// The invalid-input identifiers reuse the contract action literals verbatim.
	memoryAdminToolPropose = core.ActionMemoryPropose
	memoryAdminToolConfirm = core.ActionMemoryConfirm
	memoryAdminToolCorrect = core.ActionMemoryCorrect
	memoryAdminToolDelete  = core.ActionMemoryDelete

	// memoryAdminListLimit 是读模型的缺省条数上限（与操作列表同量级）：页面一次渲染
	// 一个分层的可见条目，超出部分靠状态过滤缩小，不做分页游标。
	// memoryAdminListLimit is the default read-model window.
	memoryAdminListLimit = 200

	// memoryAdminGlobalTarget 是全局层在 `target_id` 里的固定取值：条目主键只出现在
	// `detail_json`，`target_id` 统一表达"动的是哪一层的记忆"。
	// memoryAdminGlobalTarget names the global layer in target_id.
	memoryAdminGlobalTarget = "global"

	// memoryAdminAgentTargetPrefix 是 Agent 层的 `target_id` 前缀（`agent:<name>`）。
	// memoryAdminAgentTargetPrefix prefixes an agent-layer target_id.
	memoryAdminAgentTargetPrefix = "agent:"
)

// MemoryAdminDeps 是管理面记忆编排的依赖。
// MemoryAdminDeps carries the management-side memory dependencies.
type MemoryAdminDeps struct {
	// Store 是记忆的窄持久化端口——**必填**：缺它即 management surface 未装配，
	// 每个方法都 fail-closed 而不是静默返回空列表。
	// Store is required; without it every method fails closed.
	Store memory.MemoryStore
	// Operations 是管理操作仓储：四个写动作都要预写 pending 再回填终态。
	// Operations backs the two-phase audit of all four writes.
	Operations core.OperationStore
	// Index 是确认成功后的 best-effort 向量补索引槽位（C-28）；nil = 不补索引。
	// Index is the optional post-confirmation indexing slot (C-28).
	Index *memoryIndexSlot
	// Logger / Now 为 nil 时回落进程默认值。
	// Logger and Now fall back to process defaults when nil.
	Logger *slog.Logger
	Now    func() time.Time
}

// MemoryAdmin 是 `core.MemoryAdmin`（以及可选的 `core.MemoryConfirmResolver`）的生产
// 实现。它**不是**召回面，也不被 Run 结束路径引用——C-21 的"不自动抽取"由此处没有
// 调用点来保证。
// MemoryAdmin implements core.MemoryAdmin and the optional resolver. It has no run-end
// caller, which is how the "no automatic extraction" boundary is enforced.
type MemoryAdmin struct {
	store        *memory.SemanticStore
	listStore    memory.MemoryStore
	confirmStore memory.CandidateConfirmStore
	operations   core.OperationStore
	index        *memoryIndexSlot
	logger       *slog.Logger
	now          func() time.Time
}

// 编译期断言：端口形状由 core 声明，本包只适配。
// Compile-time assertion that this adapter satisfies the core port.
var _ core.MemoryAdmin = (*MemoryAdmin)(nil)

// NewMemoryAdmin 组装管理面记忆。Store 为 nil 时仍返回实例，但每个方法都返回装配
// 错误——装配失败必须可见，**不得**退化成"空列表 = 没有记忆"。
// NewMemoryAdmin assembles the surface; a nil store makes every call fail closed rather
// than degrade into "empty list means no memory".
func NewMemoryAdmin(deps MemoryAdminDeps) *MemoryAdmin {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	admin := &MemoryAdmin{
		listStore:  deps.Store,
		operations: deps.Operations,
		index:      deps.Index,
		logger:     logger,
		now:        now,
	}
	if deps.Store != nil {
		// 生命周期动作不需要 embedding：候选晋升、纠正、删除都不碰向量，因此这里
		// **不**传 Embedder。召回另由 `newSemanticRecaller` 装配（同一份存储端口）。
		// Lifecycle actions never need an embedder; recall is assembled elsewhere over
		// the same store port.
		admin.store = memory.NewSemanticStore(memory.SemanticDeps{Store: deps.Store})
		// 确认处置是**可选**扩展（卡 §3.1.4）：存储端口不必实现它，管理面在需要时
		// 类型断言；自动候选遇到未实现即 fail-closed，**不得**退化成 keep_both。
		// The resolution port is optional; auto candidates fail closed without it.
		if confirmStore, ok := deps.Store.(memory.CandidateConfirmStore); ok {
			admin.confirmStore = confirmStore
		}
	}
	return admin
}

// ListEntries 返回一个分层的条目列表（六态全集，主键倒序）。它是**读面**：不做
// 权限过滤之外的任何裁剪，也不参与召回——候选与已删条目必须照常返回，页面才有
// 可确认 / 可纠正 / 可删除的对象。
// ListEntries is the read model. It deliberately returns candidates and deleted rows.
func (s *MemoryAdmin) ListEntries(ctx context.Context, filter core.MemoryEntryFilter) ([]core.MemoryEntryRecord, error) {
	if s.listStore == nil || s.store == nil {
		return nil, errMemoryAdminNotAssembled
	}
	if err := validateMemoryScope(filter.Scope); err != nil {
		return nil, err
	}
	if filter.State != "" && !memory.KnownState(filter.State) {
		return nil, &core.InvalidInputError{
			Tool:   memoryAdminToolPropose,
			Reason: fmt.Sprintf("未知的记忆状态：%q（只接受六态字面量）", filter.State),
		}
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = memoryAdminListLimit
	}
	entries, err := s.store.ListEntries(ctx, core.LocalAdminPrincipal(), memory.ListQuery{
		Scope: filter.Scope,
		State: filter.State,
		Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	records := make([]core.MemoryEntryRecord, 0, len(entries))
	for _, entry := range entries {
		record := core.MemoryEntryRecord{
			EntryID:      entry.ID,
			Scope:        entry.Scope,
			Section:      entry.Section,
			State:        entry.State,
			Content:      entry.Content,
			Source:       entry.Source(),
			SupersedesID: entry.SupersedesID,
			CreatedAt:    entry.CreatedAt,
		}
		// 自动候选的元数据是**证据**：页面据此展示逐字引文与建议目标。读失败必须上抛，
		// **不得**把"证据读不出来"降级成"没有证据"的普通候选。
		// Auto candidates carry their evidence here; a read failure surfaces rather than
		// degrading into a candidate that merely looks evidence-free.
		if s.confirmStore != nil && entry.Source() == memory.SourceAutoExtract {
			meta, found, err := s.confirmStore.LoadCandidateMeta(ctx, entry.ID)
			if err != nil {
				return nil, err
			}
			if found {
				record.Candidate = &meta
			}
		}
		records = append(records, record)
	}
	return records, nil
}

// Propose 落一条候选。纯请求校验（分层 / 分区 / 正文非空）在**预写之前**完成：被拒绝
// 的请求不留操作行。同源重复提议由领域层合并到既有条目，回执带 `Merged`。
// 逆改点：把 BeginOperation 挪到领域调用之后 → TestMemoryAdmin_ProposePrewriteFailureRefusesWrite 红。
// Propose creates a candidate; request validation runs before the pending pre-write so
// a rejected request leaves no operation row behind.
func (s *MemoryAdmin) Propose(ctx context.Context, actor core.Actor, proposal core.MemoryProposal) (core.MemoryEntryResult, error) {
	if s.store == nil {
		return core.MemoryEntryResult{}, errMemoryAdminNotAssembled
	}
	if err := validateMemoryScope(proposal.Scope); err != nil {
		return core.MemoryEntryResult{}, err
	}
	section := proposal.Section
	if section == "" {
		// 空分区按归档区落：候选不进注入面，即便提议的是核心区知识也必须先确认。
		// An empty section falls back to the archive; candidates never reach the prompt.
		section = core.SectionArchive
	}
	if section != core.SectionCore && section != core.SectionArchive {
		return core.MemoryEntryResult{}, &core.InvalidInputError{
			Tool:   memoryAdminToolPropose,
			Reason: fmt.Sprintf("未知的记忆分区：%q（只接受 core / archive）", section),
		}
	}
	content := strings.TrimSpace(proposal.Content)
	if content == "" {
		return core.MemoryEntryResult{}, &core.InvalidInputError{
			Tool:   memoryAdminToolPropose,
			Reason: "提议正文不得为空",
		}
	}

	// 去重预读：领域层也会做同一次查询，但只有这里能**在预写之前**知道"这次提议会
	// 合并到既有条目"，从而把条目主键写进审计（恢复查证靠它区分"新增"与"合并"）并让
	// 回执如实告诉页面没有新增行。纯读、无副作用。
	// The dedup pre-read is what lets the audit record the merge target and the receipt
	// tell the page that no new row was created; the domain still owns the decision.
	existing, found, err := s.listStore.FindEntryByContent(ctx, proposal.Scope, section, content)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}
	merged := found &&
		(existing.State == memory.StateCandidate || existing.State == memory.StateActive)

	detail := memoryOperationDetail{
		Section:       string(section),
		Agent:         proposal.Scope.Agent,
		StateAfter:    memory.StateCandidate,
		ContentSHA256: memoryDigestHex(content),
	}
	if merged {
		detail.EntryID = existing.ID
	}
	operationID, err := s.begin(ctx, actor, core.ActionMemoryPropose,
		memoryTargetID(proposal.Scope), memoryRequestHash(string(proposal.Scope.Agent), string(section), memoryDigestHex(content)), detail)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}

	entryID, proposeErr := s.store.Propose(ctx, memory.ProposeRequest{
		Principal: core.LocalAdminPrincipal(),
		Scope:     proposal.Scope,
		Section:   section,
		Content:   content,
	})
	if proposeErr != nil {
		s.finish(ctx, operationID, core.OperationFailed, memoryAdminResultFor(proposeErr),
			"显式提议写入失败", detail)
		return core.MemoryEntryResult{}, postPendingError(proposeErr, operationID)
	}

	// 领域层只在**新增**时返回新主键；命中去重时返回的是既有条目的主键。两种情形
	// 都没有额外副作用，终态一律 success——"合并"不是失败。
	//
	// 回执的 `state` 是**该行动作后的真实状态**：合并进一条 active 记忆时如实给 active，
	// 而**不是**一律回 candidate——谎报候选会让页面为一条已生效的记忆显示"待确认"，
	// 点下去只会拿到 409。只有真正新增的行才恒为 candidate。
	// 主键相等是"确实合并"的判据：并发下预读命中的那条可能已被删除，此时领域层会
	// 新插入一行，回执按新行（candidate、未合并）如实给。
	// A dedup hit returns the existing id; the receipt reports that row's real state, so a
	// merge into an active entry never masquerades as a fresh candidate.
	merged = merged && entryID == existing.ID
	state := memory.StateCandidate
	if merged {
		state = existing.State
	}
	detail.EntryID = entryID
	detail.StateAfter = state
	result := core.MemoryEntryResult{EntryID: entryID, State: state, Merged: merged}
	s.finish(ctx, operationID, core.OperationSuccess, memoryAdminResultOK, "", detail)
	return result, nil
}

// Confirm 把一条候选晋升为 active——这是**唯一**的晋升路径。预写前先读一次目标，
// 既把 `state_before` 记进审计（恢复查证要用），也让"条目不存在"在留痕之前就 404。
// 非候选目标由领域层返回 `core.ErrConflict`（409），**不静默重放**。
// 逆改点：去掉 state_before 预读 → TestMemoryAdmin_ConfirmRecordsStateBefore 红。
// Confirm is the only promotion path; a pre-read records state_before for recovery and
// turns a missing entry into a 404 before any audit row exists.
func (s *MemoryAdmin) Confirm(ctx context.Context, actor core.Actor, entryID int64) (core.MemoryEntryResult, error) {
	if s.store == nil {
		return core.MemoryEntryResult{}, errMemoryAdminNotAssembled
	}
	target, err := s.loadTarget(ctx, entryID, memoryAdminToolConfirm)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}
	// 自动候选**不得**走旧 Confirm：这条路径没有候选到期、来源闭包与来源身份门禁。
	// web 在未装配 `core.MemoryConfirmResolver` 时仍会调到这里，自动候选必须在这里
	// fail-closed（web 503 NOT_CONFIGURED），不得静默晋升（§3.1.8）。
	auto, err := s.autoCandidateFor(ctx, target)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}
	if auto {
		return core.MemoryEntryResult{}, core.ErrMemoryConfirmUnavailable
	}

	detail := memoryOperationDetail{
		EntryID:     target.ID,
		Section:     string(target.Section),
		Agent:       target.Scope.Agent,
		StateBefore: target.State,
		StateAfter:  memory.StateActive,
	}
	operationID, err := s.begin(ctx, actor, core.ActionMemoryConfirm,
		memoryTargetID(target.Scope), memoryRequestHash(memoryAdminTargetLayers(target.Scope), fmt.Sprint(entryID)), detail)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}

	if confirmErr := s.store.Confirm(ctx, core.LocalAdminPrincipal(), entryID,
		memoryConfirmedBy(actor)); confirmErr != nil {
		s.finish(ctx, operationID, core.OperationFailed, memoryAdminResultFor(confirmErr),
			"确认候选记忆失败", detail)
		return core.MemoryEntryResult{}, postPendingError(confirmErr, operationID)
	}
	s.finish(ctx, operationID, core.OperationSuccess, memoryAdminResultOK, "", detail)
	return core.MemoryEntryResult{EntryID: entryID, State: memory.StateActive}, nil
}

// ConfirmWithResolution 是自动候选的确认入口（颗粒度文档 §3.1.8 / C-16 / C-17），也是
// `core.MemoryConfirmResolver` 的实现。顺序固定：校验处置字面量 → 纯读候选与元数据 →
// 复核目标 / 直接来源 / 候选到期 → 预写 `memory.confirm` `pending` → 单事务确认
// （存储侧再做一遍同样检查）→ 回填终态 → best-effort 补索引（C-28）。
//
// 两条边界**不得**越过：
//  1. **空 body 不改走旧 Confirm**：空 body 只把处置取 `keep_both`，自动候选照样走全门禁；
//     只有 `source<>'auto_extract'` 且无候选元数据的显式条目才保持旧路径（C-03）。
//  2. 预写之后的失败（来源到期、Key 撤销、调用方停用、并发竞争）必须把审计回填
//     `failed` 并返回 409，**不得**留下 `pending`，也**不得**退还条目原状之外的副作用。
//
// The resolution entry point: literal check → pure reads → pending audit → single
// transaction → terminal write → best-effort indexing. An empty body only picks
// keep_both; it never bypasses the gates for an auto candidate.
func (s *MemoryAdmin) ConfirmWithResolution(ctx context.Context, actor core.Actor,
	req core.MemoryConfirmRequest,
) (core.MemoryEntryResult, error) {
	if s.store == nil || s.listStore == nil {
		return core.MemoryEntryResult{}, errMemoryAdminNotAssembled
	}
	resolution, err := memoryConfirmResolution(req)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}
	target, err := s.loadTarget(ctx, req.EntryID, memoryAdminToolConfirm)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}
	meta, auto, err := s.candidateMetaFor(ctx, target)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}
	if !auto {
		// 显式提议候选 / 手工条目：既有确认路径与回执逐字不变（C-03）。
		return s.Confirm(ctx, actor, req.EntryID)
	}

	now := s.now()
	// 候选到期在预写**之前**判：被拒绝的请求不留操作行（C-17 的预写纪律）。
	if !meta.ValidTo.IsZero() && !meta.ValidTo.After(now) {
		return core.MemoryEntryResult{}, memoryConfirmReject("候选 %d 已过期（valid_to=%s）",
			req.EntryID, meta.ValidTo.UTC().Format(time.RFC3339))
	}
	if resolution == memory.ResolutionSupersede {
		// 旧建议不得覆盖新纠正：目标必须与元数据里的建议逐字相等，且仍 active。
		if meta.SuggestedSupersedesID <= 0 || meta.SuggestedSupersedesID != req.SupersedesEntryID {
			return core.MemoryEntryResult{}, memoryConfirmReject(
				"替代目标 %d 与候选建议 %d 不一致", req.SupersedesEntryID, meta.SuggestedSupersedesID)
		}
		superseded, found, err := s.listStore.LoadEntry(ctx, req.SupersedesEntryID)
		if err != nil {
			return core.MemoryEntryResult{}, err
		}
		if !found || superseded.State != memory.StateActive {
			return core.MemoryEntryResult{}, memoryConfirmReject(
				"替代目标 %d 不存在或已不在 active", req.SupersedesEntryID)
		}
	}
	// 直接来源先做快速复核；来源闭包（递归）、来源到期、Key 撤销 / 到期与调用方停用
	// 只有事务内可见，由 ConfirmCandidate 复核；那时的失败回填 failed 并返回 409
	// （§3.1.8 明文允许）。
	for _, sourceID := range memoryUniqueIDs(meta.SourceEntryIDs) {
		source, found, err := s.listStore.LoadEntry(ctx, sourceID)
		if err != nil {
			return core.MemoryEntryResult{}, err
		}
		if !found || source.State != memory.StateActive {
			return core.MemoryEntryResult{}, memoryConfirmReject(
				"来源条目 %d 不存在或已不在 active", sourceID)
		}
	}
	if s.confirmStore == nil {
		// candidateMetaFor 已保证自动候选必须有确认端口；这里只是防御式复查。
		return core.MemoryEntryResult{}, core.ErrMemoryConfirmUnavailable
	}

	detail := memoryOperationDetail{
		EntryID:      target.ID,
		Section:      string(target.Section),
		Agent:        target.Scope.Agent,
		StateBefore:  target.State,
		StateAfter:   memory.StateActive,
		Resolution:   resolution,
		SupersedesID: req.SupersedesEntryID,
	}
	operationID, err := s.begin(ctx, actor, core.ActionMemoryConfirm,
		memoryTargetID(target.Scope),
		memoryRequestHash(memoryAdminTargetLayers(target.Scope), fmt.Sprint(req.EntryID),
			resolution, fmt.Sprint(req.SupersedesEntryID)), detail)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}

	result, confirmErr := s.confirmStore.ConfirmCandidate(ctx, memory.ConfirmCandidateRequest{
		EntryID:           req.EntryID,
		Resolution:        resolution,
		SupersedesEntryID: req.SupersedesEntryID,
		ConfirmedBy:       memoryConfirmedBy(actor),
		Now:               now,
	})
	if confirmErr != nil {
		s.finish(ctx, operationID, core.OperationFailed, memoryAdminResultFor(confirmErr),
			"确认候选记忆失败", detail)
		return core.MemoryEntryResult{}, postPendingError(confirmErr, operationID)
	}
	detail.SupersedesID = result.SupersededID
	s.finish(ctx, operationID, core.OperationSuccess, memoryAdminResultOK, "", detail)

	// C-28：晋升已经提交，补索引只是**最好努力**——失败只记 WARN，确认保持 active，
	// 缺口由启动期回填补齐。**不得**把补索引失败写成确认失败。
	if s.index != nil {
		s.index.IndexAppended(ctx, target.Scope, target.Section, target.Content)
	}
	return core.MemoryEntryResult{
		EntryID:      result.EntryID,
		State:        memory.StateActive,
		SupersededID: result.SupersededID,
	}, nil
}

// Correct 纠正一条既有条目：目标 active 时新条 active、旧条转 corrected；目标已不在
// 有效态时新条落 conflicted 待人工裁决（领域层决定，本层不替它选）。
// 逆改点：把返回的 State 硬写成 active → TestMemoryAdmin_CorrectPropagatesConflictedState 红。
// Correct supersedes an active target or records a conflicted entry; the resulting state
// comes from the domain and is never rewritten here.
func (s *MemoryAdmin) Correct(ctx context.Context, actor core.Actor, correction core.MemoryCorrection) (core.MemoryEntryResult, error) {
	if s.store == nil {
		return core.MemoryEntryResult{}, errMemoryAdminNotAssembled
	}
	content := strings.TrimSpace(correction.Content)
	if content == "" {
		return core.MemoryEntryResult{}, &core.InvalidInputError{
			Tool:   memoryAdminToolCorrect,
			Reason: "纠正后的正文不得为空",
		}
	}
	target, err := s.loadTarget(ctx, correction.EntryID, memoryAdminToolCorrect)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}

	detail := memoryOperationDetail{
		EntryID:       target.ID,
		Section:       string(target.Section),
		Agent:         target.Scope.Agent,
		StateBefore:   target.State,
		ContentSHA256: memoryDigestHex(content),
	}
	operationID, err := s.begin(ctx, actor, core.ActionMemoryCorrect,
		memoryTargetID(target.Scope),
		memoryRequestHash(memoryAdminTargetLayers(target.Scope), fmt.Sprint(correction.EntryID), memoryDigestHex(content)), detail)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}

	result, correctErr := s.store.Correct(ctx, memory.CorrectRequest{
		Principal:   core.LocalAdminPrincipal(),
		TargetID:    correction.EntryID,
		Content:     content,
		ConfirmedBy: memoryConfirmedBy(actor),
	})
	if correctErr != nil {
		s.finish(ctx, operationID, core.OperationFailed, memoryAdminResultFor(correctErr),
			"纠正记忆失败", detail)
		return core.MemoryEntryResult{}, postPendingError(correctErr, operationID)
	}
	// 回执里的 EntryID 是**新条**，被纠正的旧条单列 `ReplacedID`：页面据此刷新两行。
	// The receipt carries the new row; the retired one goes in ReplacedID.
	detail.EntryID = result.EntryID
	detail.ReplacedID = result.ReplacedID
	detail.StateAfter = result.State
	s.finish(ctx, operationID, core.OperationSuccess, memoryAdminResultOK, "", detail)
	return core.MemoryEntryResult{
		EntryID:    result.EntryID,
		State:      result.State,
		ReplacedID: result.ReplacedID,
	}, nil
}

// Delete 软删一条条目：行与来源保留以便审计，立即不可召回；已删除的条目重复删除
// 视为成功（幂等）。物理删除**不在**本层能力内——审计要留得下来。
// 逆改点：把软删改成物理删除 → TestMemoryAdmin_DeleteKeepsRowForAudit 红。
// Delete is a soft delete; the row stays for audit and repeats are idempotent.
func (s *MemoryAdmin) Delete(ctx context.Context, actor core.Actor, entryID int64) (core.MemoryEntryResult, error) {
	if s.store == nil {
		return core.MemoryEntryResult{}, errMemoryAdminNotAssembled
	}
	target, err := s.loadTarget(ctx, entryID, memoryAdminToolDelete)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}

	detail := memoryOperationDetail{
		EntryID:     target.ID,
		Section:     string(target.Section),
		Agent:       target.Scope.Agent,
		StateBefore: target.State,
		StateAfter:  memory.StateDeleted,
	}
	operationID, err := s.begin(ctx, actor, core.ActionMemoryDelete,
		memoryTargetID(target.Scope), memoryRequestHash(memoryAdminTargetLayers(target.Scope), fmt.Sprint(entryID)), detail)
	if err != nil {
		return core.MemoryEntryResult{}, err
	}

	if deleteErr := s.store.Delete(ctx, memory.DeleteRequest{
		Principal: core.LocalAdminPrincipal(),
		ID:        entryID,
	}); deleteErr != nil {
		s.finish(ctx, operationID, core.OperationFailed, memoryAdminResultFor(deleteErr),
			"删除记忆失败", detail)
		return core.MemoryEntryResult{}, postPendingError(deleteErr, operationID)
	}
	s.finish(ctx, operationID, core.OperationSuccess, memoryAdminResultOK, "", detail)
	return core.MemoryEntryResult{EntryID: entryID, State: memory.StateDeleted}, nil
}

// errMemoryAdminNotAssembled 是管理面记忆未装配时的唯一拒绝：缺少存储句柄等于这个
// 写面根本不存在，**不得**用空列表或"没有找到"冒充结果。
// errMemoryAdminNotAssembled is the single refusal when the surface has no store.
var errMemoryAdminNotAssembled = errors.New("记忆管理面未装配：缺少存储端口")

// memoryAdminResultOK / memoryAdminResultFor 把领域结果映射到失败终态的 result_code
// （错误码表：`OK` / `NOT_FOUND` / `CONFLICT` / `BAD_REQUEST`；其余按 `INTERNAL`）。
// The failure result codes reuse the shared error-code table; everything else is INTERNAL.
const (
	memoryAdminResultOK           = "OK"
	memoryAdminResultNotFound     = "NOT_FOUND"
	memoryAdminResultConflict     = "CONFLICT"
	memoryAdminResultBadRequest   = "BAD_REQUEST"
	memoryAdminResultInternalCode = "INTERNAL"
)

// memoryConfirmResolution 校验确认处置的闭集：空串（无 body）等价 `keep_both`，**不是**
// 第三种处置；`keep_both` 不得夹带替代目标，`supersede` 必须带正整数目标。
// `keep_both` + 目标 = 两种处置被拼在一起，按状态冲突（409，契约 §7.1）拒绝；未知处置
// 字面量与缺少目标属入参错误（400，OpenAPI）。都发生在预写之前，不留操作行。
// memoryConfirmResolution validates the closed set; an empty value means keep_both.
func memoryConfirmResolution(req core.MemoryConfirmRequest) (string, error) {
	switch req.Resolution {
	case "", memory.ResolutionKeepBoth:
		if req.SupersedesEntryID != 0 {
			return "", memoryConfirmReject("保留双方不得携带替代目标（收到 %d）", req.SupersedesEntryID)
		}
		return memory.ResolutionKeepBoth, nil
	case memory.ResolutionSupersede:
		if req.SupersedesEntryID <= 0 {
			return "", &core.InvalidInputError{
				Tool:   memoryAdminToolConfirm,
				Reason: "替代处置缺少有效的替代目标",
			}
		}
		return memory.ResolutionSupersede, nil
	default:
		return "", &core.InvalidInputError{
			Tool:   memoryAdminToolConfirm,
			Reason: fmt.Sprintf("未知的确认处置：%q（只接受 keep_both / supersede）", req.Resolution),
		}
	}
}

// autoCandidateFor 判定目标是不是自动候选（`source='auto_extract'` 或存在候选元数据）。
// 旧 Confirm 用它挡下自动候选：读元数据失败时按错误上抛，**不**猜"大概不是候选"。
// autoCandidateFor reports whether the entry must be confirmed through the gated path;
// a metadata read failure propagates instead of guessing.
func (s *MemoryAdmin) autoCandidateFor(ctx context.Context, entry memory.MemoryEntry) (bool, error) {
	if entry.Source() == memory.SourceAutoExtract {
		return true, nil
	}
	if s.confirmStore == nil {
		return false, nil
	}
	_, found, err := s.confirmStore.LoadCandidateMeta(ctx, entry.ID)
	if err != nil {
		return false, err
	}
	return found, nil
}

// candidateMetaFor 判定目标是不是**自动候选**：`source='auto_extract'` 或存在候选
// 元数据，任一成立即走统一确认事务（C-16）。两种情况 fail-closed、**不得**退化成旧的
// Confirm 或 keep_both：自动候选缺元数据（来源不可查证）、存储未实现确认事务。
// candidateMetaFor decides whether the target is an auto candidate; both "auto without
// metadata" and "port not assembled" fail closed rather than degrading to the old path.
func (s *MemoryAdmin) candidateMetaFor(ctx context.Context, entry memory.MemoryEntry) (core.MemoryCandidateMeta, bool, error) {
	auto := entry.Source() == memory.SourceAutoExtract
	if s.confirmStore == nil {
		if auto {
			// 候选确认事务端口未装配：自动候选无法在不关掉门禁的前提下确认，按
			// 503「未装配」拒绝（web 映射 NOT_CONFIGURED），**不得**退化成旧 Confirm。
			return core.MemoryCandidateMeta{}, false, core.ErrMemoryConfirmUnavailable
		}
		return core.MemoryCandidateMeta{}, false, nil
	}
	meta, found, err := s.confirmStore.LoadCandidateMeta(ctx, entry.ID)
	if err != nil {
		return core.MemoryCandidateMeta{}, false, err
	}
	if found {
		return meta, true, nil
	}
	if auto {
		return core.MemoryCandidateMeta{}, false,
			memoryConfirmReject("自动候选 %d 缺少候选元数据，无法查证来源", entry.ID)
	}
	return core.MemoryCandidateMeta{}, false, nil
}

// memoryConfirmReject 把门禁拒绝统一包成 `core.ErrConflict`（web 409），消息只含主键与
// 状态名——正文、证据引文与凭据**不得**进入错误串（它们会进日志与响应）。
// memoryConfirmReject wraps a gate failure as core.ErrConflict with a safe message.
func memoryConfirmReject(format string, args ...any) error {
	return fmt.Errorf("确认候选记忆: %s: %w", fmt.Sprintf(format, args...), core.ErrConflict)
}

// memoryUniqueIDs 保序去重来源主键；0 与负数不是合法条目主键，一并剔除。
// memoryUniqueIDs dedupes source ids while preserving order.
func memoryUniqueIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// memoryAdminResultFor 按错误类别选 result_code——只认类型 / 哨兵，**不比文案**。
// memoryAdminResultFor classifies by sentinel and type, never by error text.
func memoryAdminResultFor(err error) string {
	switch {
	case errors.Is(err, core.ErrMemoryEntryNotFound):
		return memoryAdminResultNotFound
	case errors.Is(err, core.ErrConflict):
		return memoryAdminResultConflict
	case core.IsInvalidInput(err):
		return memoryAdminResultBadRequest
	default:
		return memoryAdminResultInternalCode
	}
}

// memoryOperationDetail 是四个记忆动作共用的 `detail_json` 形状（契约 §9.3）。
// **正文不入审计**：`content_sha256` 是规范化正文的摘要，删掉正文也能核对"改的是不是
// 这一条"。`entry_id` 在提议预写时还未知（领域层才分配），因此允许为 0。
// The shared detail_json shape; the body itself never enters the audit.
type memoryOperationDetail struct {
	EntryID       int64  `json:"entry_id"`
	ReplacedID    int64  `json:"replaced_id"`
	Section       string `json:"section"`
	Agent         string `json:"agent"`
	StateBefore   string `json:"state_before"`
	StateAfter    string `json:"state_after"`
	ContentSHA256 string `json:"content_sha256"`
	// Resolution / SupersedesID 是 US-22 追加的**可选**键（C-17）：旧 Confirm 路径
	// 不写这两个键，旧操作行与旧客户端照常可读。它们只记处置与主键，不记正文或证据。
	// The two keys are additive; the legacy Confirm path leaves them out entirely.
	Resolution   string `json:"resolution,omitempty"`
	SupersedesID int64  `json:"supersedes_id,omitempty"`
}

// loadTarget 预读目标条目：不存在 → 404 分类（`core.ErrMemoryEntryNotFound`），
// 读失败原样上抛。它在**预写之前**调用，因此"查不到"不会留下操作行。
// loadTarget pre-reads the target before the pre-write, so a missing entry leaves no row.
func (s *MemoryAdmin) loadTarget(ctx context.Context, entryID int64, tool string) (memory.MemoryEntry, error) {
	if entryID <= 0 {
		return memory.MemoryEntry{}, &core.InvalidInputError{
			Tool:   tool,
			Reason: "记忆条目标识必须是正整数",
		}
	}
	entry, found, err := s.listStore.LoadEntry(ctx, entryID)
	if err != nil {
		return memory.MemoryEntry{}, err
	}
	if !found {
		return memory.MemoryEntry{}, core.ErrMemoryEntryNotFound
	}
	return entry, nil
}

// begin 预写 pending 行并返回操作标识；预写失败 = **拒绝执行**（宪法第 5 条）：
// 目标一个字节都没动，错误上抛由 web 映射为 500 `INTERNAL`（审计不可用）。
// begin pre-writes the pending row; a failure refuses the change outright.
func (s *MemoryAdmin) begin(ctx context.Context, actor core.Actor, action, targetID, requestHash string,
	detail memoryOperationDetail,
) (string, error) {
	raw, err := json.Marshal(detail)
	if err != nil {
		return "", fmt.Errorf("序列化记忆操作摘要失败: %w", err)
	}
	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   core.TargetKindMemory,
		TargetID:     targetID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  requestHash,
		Status:       core.OperationPending,
		DetailJSON:   raw,
		CreatedAt:    now,
	}
	if beginErr := s.operations.BeginOperation(ctx, record); beginErr != nil {
		s.logger.Error("记忆管理预写操作记录失败，拒绝执行",
			"action", action, "target_id", targetID, "error", beginErr)
		return "", fmt.Errorf("记忆管理预写操作记录失败，拒绝执行: %w", beginErr)
	}
	return operationID, nil
}

// finish 回填终态；与 Agent 写面同一纪律——不随请求取消而放弃，回填失败只记 ERROR
// 并留给启动恢复按 operation_id 查证（**不得**让审计停在 pending 当作成功）。
// finish writes the terminal state detached from request cancellation.
func (s *MemoryAdmin) finish(ctx context.Context, operationID string, status core.OperationStatus,
	code, message string, detail memoryOperationDetail,
) {
	raw, err := json.Marshal(detail)
	if err != nil {
		s.logger.Error("序列化记忆操作终态摘要失败", "operation_id", operationID, "error", err)
		raw = nil
	}
	if finishErr := s.operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		status, code, message, raw, s.now()); finishErr != nil {
		s.logger.Error("记忆管理写回填操作终态失败，留待启动恢复查证",
			"operation_id", operationID, "status", string(status), "error", finishErr)
	}
}

// memoryTargetID 把分层投影成 `target_id`：全局层是固定字面量 `global`，Agent 层是
// `agent:<name>`。条目主键只出现在 `detail_json`——`target_id` 回答"动的是哪一层"。
// memoryTargetID projects the layer into target_id; row ids stay in detail_json.
func memoryTargetID(scope core.Scope) string {
	if scope.Agent == "" {
		return memoryAdminGlobalTarget
	}
	return memoryAdminAgentTargetPrefix + scope.Agent
}

// memoryAdminTargetLayers 是请求摘要里的分层片段（与 target_id 同形）。
// memoryAdminTargetLayers is the layer fragment of the request hash.
func memoryAdminTargetLayers(scope core.Scope) string {
	return memoryTargetID(scope)
}

// memoryConfirmedBy 取确认人列（`memory_entries.confirmed_by`）：管理面统一记
// actor_subject，**不**记 token 明文（actor_subject 本身已是脱敏形态）。
// memoryConfirmedBy records the actor subject, never the token plaintext.
func memoryConfirmedBy(actor core.Actor) string {
	if strings.TrimSpace(actor.Subject) == "" {
		// 领域层拒绝空确认人；这里补一个固定占位，让"没有主语"不会把一次成功写入
		// 变成 500——actor_subject 由装配层保证非空，占位只是防御。
		return core.TargetKindMemory
	}
	return actor.Subject
}

// memoryRequestHash 是脱敏请求摘要：分层 / 分区 / 条目标识 / 正文摘要在内，正文原文
// 在外。**不得**把正文直接拼进来——`request_hash` 会随审计长期保留。
// memoryRequestHash hashes the sanitized request shape; the body itself never enters it.
func memoryRequestHash(parts ...string) string {
	return memoryDigestHex(strings.Join(parts, "|"))
}

// memoryDigestHex 是审计里统一的摘要写法（64 位小写十六进制）。
// memoryDigestHex is the single digest spelling used across the audit.
func memoryDigestHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// validateMemoryScope 校验分层的 Agent 名——与工作区同名规则（点前缀 / 路径分隔符 /
// 保留后缀一律拒绝）。空串是**正常取值**（全局层），不在这里拒绝。
// validateMemoryScope validates the agent name; an empty name is the global layer.
func validateMemoryScope(scope core.Scope) error {
	if scope.Agent == "" {
		return nil
	}
	if err := workspace.ValidateAgentName(scope.Agent); err != nil {
		return &core.InvalidInputError{Tool: core.ActionMemoryPropose, Reason: err.Error()}
	}
	return nil
}
