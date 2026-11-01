package core

// 本文件是**管理面记忆**的声明面：结构化读模型、四个写动作的入参 / 回执、以及
// `MemoryAdmin` 端口。端口声明在消费方（web / app 装配）是包纪律（技术方案 §7.1.2）：
// `core` 只依赖标准库，`internal/memory` 与 `internal/app` 各自实现 / 适配。
//
// 三条边界（与 C-21 同一口径）：
//  1. **候选只由显式提议产生**——本文件不提供任何"运行结束后自动抽取"的入口；
//  2. **候选不自动晋升**——`Confirm` 是唯一让条目从 candidate 变 active 的路径；
//  3. **候选不参与召回**——召回面在 `internal/memory` 的存储过滤与层内守点各自把关，
//     本端口只读不召回。
// The management-side memory declaration surface: the structured read model, the four
// write inputs and results, and the MemoryAdmin port. Candidates come only from an
// explicit proposal, only Confirm promotes them, and nothing here recalls anything.

import (
	"context"
	"time"
)

// 管理面记忆动作字面量（契约 `run-and-errors.md` §9.3，`<resource>.<verb>` 点号分隔）。
// 四个动作**都有**可查证的终态：条目行本身就是查证对象（§9.4）。
// The four management action literals; each has a verifiable terminal state in the row.
const (
	// ActionMemoryPropose 是显式提议一条候选记忆。
	ActionMemoryPropose = "memory.propose"
	// ActionMemoryConfirm 是确认一条候选记忆（唯一晋升路径）。
	ActionMemoryConfirm = "memory.confirm"
	// ActionMemoryCorrect 是纠正一条既有记忆（旧条转 corrected，新条 active）。
	ActionMemoryCorrect = "memory.correct"
	// ActionMemoryDelete 是软删一条既有记忆（行保留，立即不可召回）。
	ActionMemoryDelete = "memory.delete"
)

// TargetKindMemory 是四个记忆动作共用的目标类型字面量（契约 §9.3）。
// TargetKindMemory is the shared target kind of the four memory actions.
const TargetKindMemory = "memory"

// ErrMemoryEntryNotFound 表示按标识查不到记忆条目（含已被物理清理的情形）：web 映射
// 404，**不**与"状态不允许本次迁移"混为一谈——后者沿用既有的 `core.ErrConflict`
// （409）。两个分类都按 `errors.Is` 判，**不得**比对文案。
// ErrMemoryEntryNotFound maps to 404. A state conflict keeps using core.ErrConflict
// (409); neither class is ever matched by comparing error text.
var ErrMemoryEntryNotFound = errMemory("memory: entry not found")

// errMemory 是本地错误类型：只为了让 `errors.Is` 在同一哨兵上成立，不带额外载荷。
// errMemory is a local error type so the sentinels compare by identity.
type errMemory string

func (e errMemory) Error() string { return string(e) }

// MemoryEntryRecord 是管理面读取的**一条**记忆条目（结构化读模型）。它与渲染文本
// （`MemoryView.core` / `archive`）的关键差别是**带主键与状态**：没有主键，页面上
// 就没有可执行纠正 / 删除的对象；没有状态，候选与失效条目就无从显示。
//
// 读模型**只读**，不承担召回语义：`State` 覆盖六态全集（含 candidate / deleted），
// 而召回面永远只有 active。
// MemoryEntryRecord is one row of the structured read model. It carries the primary key
// and the state — without them the page has nothing to correct or delete, and candidates
// would be invisible. The read model is not a recall surface.
type MemoryEntryRecord struct {
	// EntryID 是条目主键（纠正 / 删除 / 确认的目标）。
	EntryID int64
	// Scope 是分层（零值 = 全局层）。
	Scope Scope
	// Section 是分区（核心区 / 归档区）。
	Section Section
	// State 是六态生命周期取值（契约 `schema.pg.sql` 的 CHECK 闭集，无 expired）。
	State string
	// Content 是条目正文（已规范化，无载体添加物）。
	Content string
	// Source 是写入来源（save_memory / explicit_propose / legacy_import / migration）。
	Source string
	// SupersedesID 指向被本条替代的旧条目（0 = 无）。
	SupersedesID int64
	// CreatedAt 是写入时刻（UTC）。
	CreatedAt time.Time
}

// MemoryEntryFilter 是条目列表的过滤条件；空字段表示不过滤。
// 排序与窗口由存储层固定（`id DESC`），调用方只给过滤值——排序不是参数。
// MemoryEntryFilter carries the list filters; ordering stays fixed in storage.
type MemoryEntryFilter struct {
	// Scope 是必给的过滤分层（全局层是零值，不是"不过滤"）。
	Scope Scope
	// State 是可选的状态过滤（契约六态字面量之一）；空串 = 六态全列。
	State string
	// Limit 是返回条数上限（<= 0 取缺省）。
	Limit int
}

// MemoryProposal 是**显式提议**的入参：只由用户在管理台点提交产生，Run 结束**不得**
// 调用（C-21）。`Section` 空串按归档区落——候选不进注入面。
// MemoryProposal is the explicit proposal input; an empty Section means the archive.
type MemoryProposal struct {
	// Scope 是落库的记忆分层。
	Scope Scope
	// Section 是落库的分区；空串 = 归档区。
	Section Section
	// Content 是提议正文（两端空白由领域层规范化）。
	Content string
}

// MemoryCorrection 是纠正的入参：新条继承目标条的 Scope / Section。
// MemoryCorrection corrects an existing entry; the new row inherits its scope.
type MemoryCorrection struct {
	// EntryID 是被纠正的条目。
	EntryID int64
	// Content 是纠正后的正文。
	Content string
}

// MemoryEntryResult 是四个写动作的统一回执：页面据此刷新那一行，不必整页重读。
// MemoryEntryResult is the shared write receipt; the page can refresh the one row.
type MemoryEntryResult struct {
	// EntryID 是本次动作产出的条目主键（propose / correct 可能是新行）。
	EntryID int64
	// State 是该条目在动作完成后的状态。
	State string
	// ReplacedID 是纠正动作被替代的旧条目（0 = 不适用）。
	ReplacedID int64
	// Merged 表示提议命中了同源去重、**没有**新增行（C-21 的"重复转述合并"）。
	Merged bool
}

// MemoryAdmin 是管理面记忆端口的**消费方接口**（实现在 `internal/app`）。
// 权限模型：四个动作都只由管理面（admin token）触发——调用方 Key **不得**读写他人
// 记忆层，Agent 自己的记忆只经 `save_memory` / `recall_memory` 在运行中访问。
// MemoryAdmin is the consumer-side port implemented by internal/app. All four actions
// are management-surface only; a caller key never reaches another layer's memory.
type MemoryAdmin interface {
	// ListEntries 返回该分层的条目列表（六态全集，按主键倒序）。
	ListEntries(ctx context.Context, filter MemoryEntryFilter) ([]MemoryEntryRecord, error)

	// Propose 落一条候选（同源重复提议合并到既有条目，`Merged` 为真）。
	Propose(ctx context.Context, actor Actor, proposal MemoryProposal) (MemoryEntryResult, error)

	// Confirm 把一条候选晋升为 active；非候选目标返回 `ErrConflict`（409），查不到条目
	// 返回 `ErrMemoryEntryNotFound`（404）。
	Confirm(ctx context.Context, actor Actor, entryID int64) (MemoryEntryResult, error)

	// Correct 纠正一条条目：目标 active 时新条 active 并退役旧条，否则新条 conflicted。
	Correct(ctx context.Context, actor Actor, correction MemoryCorrection) (MemoryEntryResult, error)

	// Delete 软删一条条目（幂等：已删除的条目重复删除仍返回成功）。
	Delete(ctx context.Context, actor Actor, entryID int64) (MemoryEntryResult, error)
}
