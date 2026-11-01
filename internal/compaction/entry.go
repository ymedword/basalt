package compaction

import (
	"context"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是压缩条目与存储端口的**领域契约**（设计 §3.1.11）：字段与
// `compaction_entries` 的列一一对应，追加结局是**六值**而不是布尔。
// Entry shapes mirror the compaction_entries columns; the append outcome has six values
// rather than a boolean, because "not written" has four different meanings.

// 条目状态字面量，与 `compaction_entries.status` 的 CHECK 逐字一致（契约冻结）。
// Entry statuses mirror the frozen CHECK of compaction_entries.status.
const (
	// StatusCommitted 表示条目已落且参与链；只有它才可能成为有效头。
	StatusCommitted = "committed"
	// StatusFailed 表示本次压缩失败（摘要失败 / 超限）——留痕用，不进链。
	StatusFailed = "failed"
	// StatusStale 表示来源失效或祖先失效导致不可注入；**不删除**、不改写审计集。
	StatusStale = "stale"
)

// AppendOutcome 是 `Append` 的结局分类（六值，设计 §3.1.11 冻结）：
// **不得**合并成布尔——"没写进去"有四种不同的后续动作。
// AppendOutcome classifies what Append did; the six values drive four different
// follow-up actions, so they must never collapse into a boolean.
type AppendOutcome string

const (
	// AppendCommitted 表示条目已落且唯一，可直接用于投影。
	AppendCommitted AppendOutcome = "committed"
	// AppendRetryable 表示原事务**确认未落**（`TxVerifyAbsent` 或锁等待超时）：
	// 换 `attempt_id` 重提**同一条**摘要，不重发摘要调用、不重复预留。
	AppendRetryable AppendOutcome = "retryable"
	// AppendSuperseded 表示头已前进或切点已变，本次**未写入**；`Head` 必非 nil。
	// **不得**重提旧摘要：必须按新头重算范围并重新生成。
	AppendSuperseded AppendOutcome = "superseded"
	// AppendRebuildRoot 表示**无有效头**（无 committed / 祖先失效 / 断链 / 多叶），
	// 本次**未写入**，`Head` 为 nil；引擎以 `chain_epoch+1` 重建新根，每 Run 至多一次。
	AppendRebuildRoot AppendOutcome = "rebuild_root"
	// AppendPending 表示结局读不到（`TxVerifyPending`）：限时等待后冻结前滚并告警，
	// **不重试**。
	AppendPending AppendOutcome = "pending"
	// AppendInconsistent 表示结局与业务行矛盾：冻结前滚并告警，**不重试**。
	AppendInconsistent AppendOutcome = "inconsistent"
)

// Valid 拒绝零值与词表外取值。
// Valid rejects the zero value and anything outside the frozen vocabulary.
func (o AppendOutcome) Valid() bool {
	switch o {
	case AppendCommitted, AppendRetryable, AppendSuperseded, AppendRebuildRoot,
		AppendPending, AppendInconsistent:
		return true
	}
	return false
}

// 无头结局的原因码（设计 §3.1.11：`ReasonCode` ∈ 这四个之一）。
// Reason codes for an AppendRebuildRoot outcome.
const (
	ReasonNoCommittedHead = "no_committed_head"
	ReasonStaleAncestor   = "stale_ancestor"
	ReasonChainBroken     = "chain_broken"
	ReasonEpochRaced      = "epoch_raced"
	// ReasonAppendLockTimeout 是锁等待超时（`AppendRetryable` 的唯一原因码）。
	ReasonAppendLockTimeout = "append_lock_timeout"
)

// CompactionEntry 是一条压缩条目：字段与 `compaction_entries` 列**一一对应**，不另立存储。
//
// `SourceRefs`（审计集）与 `IncludedRefs`（当前依赖集）是**两个不同集合**，**不得**互相
// 冒充：前者含此后失效 / 本次被剔除的引用、不参与注入判定；后者才是注入时逐个校验的对象。
// CompactionEntry mirrors the compaction_entries columns. SourceRefs is the audit set and
// IncludedRefs the current dependency set; only the latter gates injection.
type CompactionEntry struct {
	CompactionID         string
	Scope                Scope
	ChainEpoch           int
	ParentCompactionID   string
	TriggerRunID         string
	AttemptID            string
	CoveredFromMessageID string
	CutMessageID         string
	FirstKeptMessageID   string
	StrategyVersion      string
	PromptVersion        string
	Model                string
	Usage                core.Usage
	TokensBefore         int
	TokensAfter          int
	Status               string
	ReasonCode           string
	SourceRefs           []core.EvidenceRef
	IncludedRefs         []core.EvidenceRef
	Summary              string
	CreatedAt            time.Time
	StaleAt              time.Time
}

// Valid 判定条目形状可写：标识、scope、覆盖范围、保留起点与两个集合的位置必须齐备。
// 摘要正文可以短，但覆盖范围与保留起点缺失的条目**不得**写进链——投影无从重建。
// Valid checks the fields a chain entry cannot be without.
func (e CompactionEntry) Valid() bool {
	return e.CompactionID != "" && e.Scope.Valid() && e.CoveredFromMessageID != "" &&
		e.CutMessageID != "" && e.FirstKeptMessageID != "" && e.ChainEpoch >= 1
}

// AppendResult 是一次追加的完整结局：条目、分类、原因码与（被超越时的）新头。
// `Head` 为 nil **只**在 `AppendRebuildRoot` / 未对应的结局里出现——有头结局必须带上它，
// 否则引擎无法按新头重算。
// AppendResult carries the outcome; Head is non-nil exactly when the outcome needs it.
type AppendResult struct {
	Entry      CompactionEntry
	Outcome    AppendOutcome
	ReasonCode string
	Head       *CompactionEntry
}

// CompactionStore 是 `internal/compaction` 声明的**存储端口**（设计 §3.1.11）：
// 只追加、读只取当前 `chain_epoch` 的有效链、`Append` 自持事务并在锁内校验有效头。
// 签名里**不出现** `*sql.Tx` / 存储包的结局类型——事务与三态判定归实现方。
// CompactionStore is the append-only port; transaction handling and outcome mapping stay
// with the implementation, so no sql.Tx ever appears in this signature.
type CompactionStore interface {
	// Append 追加一条条目并在锁内校验有效头（见实现与设计的头校验规则）。
	Append(ctx context.Context, entry CompactionEntry) (AppendResult, error)
	// FindCommitted 按覆盖范围找**链资格通过**的条目；祖先失效 / 断链按未命中处理。
	FindCommitted(ctx context.Context, scope Scope, coveredFromMessageID, cutMessageID string) (CompactionEntry, bool, error)
	// LatestCommittedChain 返回**当前 `chain_epoch`** 的 committed 链（epoch 随条目返回）。
	LatestCommittedChain(ctx context.Context, scope Scope) ([]CompactionEntry, error)
	// MarkStale 在同一把 scope 锁内把目标及其全部后代置 `stale`。
	MarkStale(ctx context.Context, compactionID, reason string) error
}

// MessageReader 是**仅会话范围**的只读消息端口：按 `(session_id, seq)` 顺序读取，
// 不分页游标、不按相关性排序、不改写。**没有 `ListByRun`**——无状态 Run 的消息从不落库，
// 按 Run 去 PG 查只能得到空集，无状态范围一律走 `CompactRequest.History`。
// MessageReader reads one session in seq order. There is deliberately no ListByRun: a
// stateless run's messages never reach the database, so a run-scoped query can only
// return an empty set.
type MessageReader interface {
	ListBySession(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]StoredMessage, error)
}
