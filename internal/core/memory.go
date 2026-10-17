package core

import (
	"context"
	"fmt"
)

// 本文件是记忆面的**声明面**（纯类型 + 常量 + 两个接口，零文件与 SQL——颗粒度文档
// §3.1 / 技术方案 §5.1 契约 1）：载体实现落在 `internal/memory`（markdown 档与门面）
// 与 `internal/storage`（sqlite 档），流经 `core` 的只有接口形状。
//
// 两个维度**不得混淆**：`Scope` 是**分层**（这是谁的记忆），`Section` 是**分区**
// （记忆的哪一块）。契约 §5.4 里工具的参数名 `scope`（取值 `CORE` / `ARCHIVAL`）指的是
// **分区**，而 `memory_entries` 表的 `scope` 列指的是**分层**——同名不同物，照契约注
// 执行（颗粒度文档 §6.2 口径 8）。
// The declaration surface of memory: pure types, constants and the two interfaces.
// Scope is the layer, Section is the region; the tool parameter named `scope` means the
// region while the table column named `scope` means the layer.

// Scope 是记忆的**分层**标识：Agent 名为空即全局层。
// Scope identifies a memory layer; an empty agent name is the global layer.
//
// 零值 `Scope{}` 与 `GlobalScope` **同值**——两者是同一个作用域，区别只在语义可读性：
// 按上下文选表达更清楚的那个。「无 Agent 上下文 = 全局层」是**正常取值**，不是异常
// （如 CLI 顶层会话）；没有任何一处应当据此报错或拒绝（约束 3）。
// The zero value and GlobalScope are the same scope. Having no agent context is a normal
// value rather than an error, and nothing may refuse a run because of it.
type Scope struct {
	// Agent 是 Agent 名；空串 = 无 Agent 上下文（全局层）。
	// Agent names the agent; an empty string means no agent context (global layer).
	Agent string
}

// GlobalScope 是全局层作用域——所有 Agent 共享的那一层，也是无 Agent 上下文时的生效层。
// GlobalScope is the global layer shared by every agent.
var GlobalScope = Scope{}

// Section 是记忆的**分区**：核心区（每轮完整注入系统提示词）与归档区（只经关键词检索召回）。
// Section is the memory region: the core region is injected whole on every round, the
// archive is reachable only through keyword recall.
type Section string

const (
	// SectionCore 是核心区：每轮完整注入、**永不裁剪**，因此有写配额（单条 / 层内总量）。
	// SectionCore is the core region: injected whole and never trimmed, so it carries quotas.
	SectionCore Section = "core"

	// SectionArchive 是归档区：不进系统提示词、无配额，只由 RecallByKeyword 取回。
	// SectionArchive is the archive: never injected, unbounded, reachable only by recall.
	SectionArchive Section = "archive"
)

// 核心区配额（**代码常量，不是配置键**——颗粒度文档 §6.2 口径 15）：按 UTF-8 字节计。
// Core-region quotas in UTF-8 bytes; code constants rather than configuration keys.
const (
	// MaxCoreEntryBytes 是单条核心记忆的上限；只有 CORE 写受此限，归档区无单条上限。
	// MaxCoreEntryBytes caps one core entry; archiving writes are not capped per entry.
	MaxCoreEntryBytes = 4096

	// MaxCoreSectionBytes 是**每层**核心区的总量上限；归档区无总量上限。
	// MaxCoreSectionBytes caps the whole core region of one layer.
	MaxCoreSectionBytes = 32768
)

// MemoryQuotaError.Kind 的两个取值——判类型时不比文本，比的是这个枚举。
// The two Kind values of MemoryQuotaError; callers match by type, not by text.
const (
	// QuotaKindEntry 表示单条越限（MaxCoreEntryBytes）。
	QuotaKindEntry = "entry"

	// QuotaKindSection 表示层内核心区总量越限（MaxCoreSectionBytes）。
	QuotaKindSection = "section"
)

// LongTermMemoryStore 是长期记忆的**统一读写接口**（三方法逐字，FR-001）：门面与两档载体
// 都实现它。门面与载体之间**不得**另立第二套读写接口——读写的唯一形状就是这三个方法。
// LongTermMemoryStore is the one read/write interface shared by the facade and both
// backends; no second interface may be introduced between them.
type LongTermMemoryStore interface {
	// Append 向 scope 层的 section 分区追加一条记忆；超配额返回 *MemoryQuotaError。
	// Append adds one entry to the section of the scope's layer.
	Append(ctx context.Context, scope Scope, section Section, content string) error

	// Load 返回该层载体的完整文本（markdown 档是文件原文，sqlite 档是渲染文本）——
	// 两档只承诺同一解析器可切出同一分区结构，**不承诺**逐字节相同。
	// Load returns the layer's full text; the two backends agree on parseable shape,
	// not on bytes.
	Load(ctx context.Context, scope Scope) (string, error)

	// RecallByKeyword 在归档区按整串子串取回条目（区分大小写、按写入顺序）；
	// 无命中是**成功**：空切片 + nil error。
	// RecallByKeyword matches whole substrings over the archive, case-sensitively, in
	// write order; no hit is a success, not an error.
	RecallByKeyword(ctx context.Context, scope Scope, query string) ([]string, error)
}

// MemoryReader 是提示词侧对记忆的**唯一消费面**：运行开始时读一次生效层的核心区文本，
// 运行内冻结。接口只暴露"读生效层"这一件事——提示词侧拿不到写入面，也拿不到归档区。
// MemoryReader is the prompt side's only consumption surface: one read per run of the
// effective layer's core region.
type MemoryReader interface {
	// LoadEffective 返回生效层的核心区文本：有 Agent 上下文且该层有内容 → Agent 层，
	// 否则全局层（两层**不叠加**）。该层只有归档条目时返回空文本——层非空即生效层，
	// **不得**因此回退去注全局层的核心区（颗粒度文档 §6.2 口径 1 / 2）。
	// LoadEffective returns the effective layer's core text; layers never stack, and a
	// layer holding only archived entries yields an empty string rather than a fallback.
	LoadEffective(ctx context.Context, agent string) (string, error)
}

// MemoryQuotaError 是超配额的**类型化**拒绝：调用方按类型判（errors.As），**不得**比对
// 文案——文案是给模型看的可读指引，不是判据。
// MemoryQuotaError is the typed over-quota refusal; callers match by type, never by text.
type MemoryQuotaError struct {
	// Kind 取 QuotaKindEntry（单条越限）或 QuotaKindSection（层内核心区总量越限）。
	// Kind is QuotaKindEntry or QuotaKindSection.
	Kind string

	// Limit 是被触发的那个上限（字节）——文案据此点名，实现侧不得改写数字。
	// Limit is the bound that was hit, in bytes.
	Limit int
}

// Error 渲染可读拒绝：点名触发的上限，并给出可执行的下一步（改写归档区 / 压缩内容——
// 颗粒度文档 §6.2 口径 8 的两条固定文案）。未知 Kind 按总量档渲染（fail-closed：
// 宁可指出更宽的那条上限，也不给一句读不出所以然的拒绝）。
// Error names the bound that was hit and a next step the caller can act on.
func (e *MemoryQuotaError) Error() string {
	if e.Kind == QuotaKindEntry {
		return fmt.Sprintf("核心记忆单条超出上限（%d 字节），请压缩内容或改写归档区", e.Limit)
	}
	return fmt.Sprintf("核心记忆超出总量上限（%d 字节），请改写归档区", e.Limit)
}
