package memory

// 本文件是**长期记忆的六态生命周期**（颗粒度文档 §3.1.4 / 设计图 5 / C-21）：显式提议
// 产生候选，确认才晋升为有效；纠正、删除、冲突都作用于既有条目。Run 结束**不得**自动
// 抽取、候选**不得**自动晋升、候选与冲突条**不得**参与召回——这三条是 C-21 的硬边界，
// 由本文件的入口检查与 `Recall` 的 active 过滤共同守住。
// This file implements the six-state lifecycle: explicit proposal creates a candidate,
// only an explicit confirmation promotes it, and recall never returns anything but active
// entries. Nothing here is called automatically at run end (C-21).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// 六态取值是**契约字面量**（`docs/contracts/schema.pg.sql` 的 memory_entries.state
// CHECK）——不得改写、不得新增第七态。设计散文里的"过期（expired）"在契约里**没有**
// 这个取值：到期由 `valid_to` 表达（存储层按时间过滤），行本身仍是 active。
//
// 导出是因为**管理面**要按状态过滤与判读（`internal/app` 的恢复查证、web 的入参校验
// 都引用这一份）；本包仍是六态字面量在 Go 侧的唯一来源，其它包不得各写一套。
// Exported because the management surface filters and reads by state; this package stays
// the single Go-side source of the six literals.
// The six state values are contract literals; there is no "expired" value — expiry is
// expressed by valid_to, not by a seventh state.
const (
	StateCandidate  = "candidate"
	StateActive     = "active"
	StateSuperseded = "superseded"
	StateCorrected  = "corrected"
	StateDeleted    = "deleted"
	StateConflicted = "conflicted"
)

// 生命周期入口的两种拒绝：内容为空是调用方错误，找不到条目按"不存在"处理。
// 后者直接用 `core.ErrMemoryEntryNotFound`——管理面端口声明的是同一个哨兵，本包不再
// 另立一份（两套同义哨兵会让 `errors.Is` 在上层悄悄失配，404 变成 500）。
// Empty content is a caller error; a missing entry returns the core sentinel so the
// management port and this package never carry two sentinels for one condition.
var (
	errEmptyMemoryContent = errors.New("memory: content must not be empty")
	errEmptyConfirmedBy   = errors.New("memory: confirmed_by must not be empty")
)

// ProposeRequest 是**显式提议**的入参：管理台 / 未来 D 的学习准入调用它，Run 结束
// **不得**调用（C-21）。Section 为空按归档区落——候选不进注入面，即使提议的是核心区
// 知识也必须先确认再迁移。
// ProposeRequest is the explicit proposal input; an empty Section means the archive.
type ProposeRequest struct {
	// Principal 是提议人身份；零值 / 无效值一律拒绝（C-28）。
	Principal core.Principal
	// Scope 是落库的记忆分层。
	Scope core.Scope
	// Section 是落库的分区；空串 = 归档区（候选不进注入面）。
	Section core.Section
	// Content 是提议正文（两端空白由本包规范化后落库）。
	Content string
}

// CorrectRequest 是用户纠正的入参：新条继承目标条的 Scope / Section，旧条转 corrected，
// 新条直接 active（设计图 5 的"用户纠正"分支）。
// CorrectRequest corrects an existing entry; the new entry inherits the target's scope.
type CorrectRequest struct {
	// Principal 是纠正人身份。
	Principal core.Principal
	// TargetID 是被纠正的条目。
	TargetID int64
	// Content 是纠正后的新正文。
	Content string
	// ConfirmedBy 记录确认人（审计列 confirmed_by，不得为空）。
	ConfirmedBy string
}

// CorrectResult 是纠正结果：`State` 是新条的最终状态——目标条仍是 active 时为 active；
// 目标已不在有效态时为 conflicted（待人工裁决，不进召回）。
// CorrectResult reports the new entry id and its state (active or conflicted).
type CorrectResult struct {
	// EntryID 是新插入条目的主键。
	EntryID int64
	// ReplacedID 是被纠正的旧条目。
	ReplacedID int64
	// State 是新条状态。
	State string
}

// DeleteRequest 是删除入参：软删（state=deleted），行与审计保留，立即不可召回。
// DeleteRequest soft-deletes an entry: the row stays, recall stops immediately.
type DeleteRequest struct {
	// Principal 是删除人身份。
	Principal core.Principal
	// ID 是被删除的条目。
	ID int64
}

// Propose 落地"C-21 显式提议 → candidate"。同源重复转述**合并到同条**（设计图 5）：
// 已存在同分层 / 同分区 / 同正文的 candidate 或 active 条目时直接返回它的 ID，不新增
// 独立证据；已删除 / 已纠正 / 已替代的条目不再吸收新提议，重新提议会生成新候选。
// 逆改点：Propose 直接写 active 或跳过去重 → TestMemory_DuplicateProposalMergesWithoutNewRow 红。
// Propose creates a candidate; repeated retellings merge into the existing row.
func (s *SemanticStore) Propose(ctx context.Context, req ProposeRequest) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	if !req.Principal.Valid() {
		return 0, core.ErrUnauthorized
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		return 0, errEmptyMemoryContent
	}
	section := req.Section
	if section == "" {
		section = core.SectionArchive
	}
	if section != core.SectionCore && section != core.SectionArchive {
		return 0, fmt.Errorf("memory: unknown section %q", section)
	}

	existing, found, err := s.deps.Store.FindEntryByContent(ctx, req.Scope, section, content)
	if err != nil {
		return 0, err
	}
	if found && (existing.State == StateCandidate || existing.State == StateActive) {
		return existing.ID, nil
	}
	return s.deps.Store.InsertEntry(ctx, MemoryEntry{
		Scope:   req.Scope,
		Section: section,
		Content: content,
		State:   StateCandidate,
	})
}

// Confirm 是候选晋升的**唯一**入口：只有显式确认才写 active 并记录 confirmed_by；
// 非候选（已有效 / 已删除等）一律按状态冲突拒绝，不静默重放。
// 逆改点：去掉候选态检查 → 二次确认改状态 → TestMemory_CandidateNotRecalledUntilConfirmed 红。
// Confirm is the only promotion path; it refuses entries that are not candidates.
func (s *SemanticStore) Confirm(ctx context.Context, principal core.Principal, id int64, confirmedBy string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if !principal.Valid() {
		return core.ErrUnauthorized
	}
	if strings.TrimSpace(confirmedBy) == "" {
		return errEmptyConfirmedBy
	}
	entry, found, err := s.deps.Store.LoadEntry(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return core.ErrMemoryEntryNotFound
	}
	if entry.State != StateCandidate {
		return core.ErrConflict
	}
	return s.deps.Store.UpdateEntryState(ctx, id, StateActive, confirmedBy)
}

// Correct 落地"用户纠正"分支：目标仍 active → 原子地插入新条（active，supersedes_id
// 指向目标）并把旧条转 corrected；目标已不在有效态 → 新条落 conflicted 待裁决，旧条
// 不动（两个来源互相矛盾时不得由一次写入替人决定谁对）。
// 逆改点：纠正后仍返回旧正文 / 旧条不改状态 → TestMemory_CorrectionSupersedesAndPreventsRecall 红。
// Correct either supersedes an active target or records a conflicted entry for adjudication.
func (s *SemanticStore) Correct(ctx context.Context, req CorrectRequest) (CorrectResult, error) {
	if err := s.ready(); err != nil {
		return CorrectResult{}, err
	}
	if !req.Principal.Valid() {
		return CorrectResult{}, core.ErrUnauthorized
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		return CorrectResult{}, errEmptyMemoryContent
	}
	if strings.TrimSpace(req.ConfirmedBy) == "" {
		return CorrectResult{}, errEmptyConfirmedBy
	}
	target, found, err := s.deps.Store.LoadEntry(ctx, req.TargetID)
	if err != nil {
		return CorrectResult{}, err
	}
	if !found {
		return CorrectResult{}, core.ErrMemoryEntryNotFound
	}

	next := MemoryEntry{
		Scope:        target.Scope,
		Section:      target.Section,
		Content:      content,
		SupersedesID: target.ID,
		// `ConfirmedBy` 是必填入参，**必须**随新条落库（`confirmed_by` 审计列）：此处
		// 丢掉它会让纠正出的 active 头永远没有确认人，而"谁担保了这条正文"正是纠正
		// 在审计之外要留下的数据级证据。两个分支（active / conflicted）都记。
		// The required confirmer must reach the row: without it a corrected head would
		// never record who vouched for the new body.
		confirmedBy: req.ConfirmedBy,
	}
	if target.State != StateActive {
		next.State = StateConflicted
		newID, err := s.deps.Store.InsertEntry(ctx, next)
		if err != nil {
			return CorrectResult{}, err
		}
		return CorrectResult{EntryID: newID, ReplacedID: target.ID, State: StateConflicted}, nil
	}

	next.State = StateActive
	newID, err := s.deps.Store.ReplaceEntry(ctx, target.ID, StateCorrected, next)
	if err != nil {
		return CorrectResult{}, err
	}
	return CorrectResult{EntryID: newID, ReplacedID: target.ID, State: StateActive}, nil
}

// Delete 是软删：只把状态改成 deleted，行与来源保留以便审计；已删除的条目重复删除
// 视为成功（幂等），找不到的 ID 报"不存在"而不是静默成功。
// 逆改点：物理删除或删除后仍可召回 → TestMemory_DeleteIsSoftAndStopsRecall 红。
// Delete is a soft delete and is idempotent for an already-deleted entry.
func (s *SemanticStore) Delete(ctx context.Context, req DeleteRequest) error {
	if err := s.ready(); err != nil {
		return err
	}
	if !req.Principal.Valid() {
		return core.ErrUnauthorized
	}
	entry, found, err := s.deps.Store.LoadEntry(ctx, req.ID)
	if err != nil {
		return err
	}
	if !found {
		return core.ErrMemoryEntryNotFound
	}
	if entry.State == StateDeleted {
		return nil
	}
	return s.deps.Store.UpdateEntryState(ctx, req.ID, StateDeleted, "")
}

// activeOnly 是召回面的层内守点：候选、冲突、已纠正、已替代、已删除的条目一律不返回。
// 存储层的 SQL 过滤（T052）是主判据，这里再判一次——两层都错才可能漏出，而单靠存储
// 实现时一次 WHERE 漏写就会把候选直接喂给模型。
// The layer-level guard on recall; the store filters too, so neither side alone can leak
// a non-active entry into the prompt.
func activeOnly(entries []MemoryEntry) []MemoryEntry {
	out := make([]MemoryEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.State == StateActive {
			out = append(out, entry)
		}
	}
	return out
}
