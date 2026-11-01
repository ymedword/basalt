package history

// 本文件声明历史回查的**消费方端口**与错误分类（US-16 §3.1.10 / T131）。
//
// 三条硬约束：
//  1. **端口定义在消费方**（技术方案 §7.1.2 第二条）：`MessageQueryStore` 由本包声明、
//     由 `internal/storage/postgres` 的 `history_store.go` 实现；本包**不得** import
//     适配层，适配层可以 import 本包；
//  2. **归属过滤是强制项不是提示项**：`Scope` 由 `permission.go` 从 `core.Principal`
//     结算而来，端口实现**必须**按它过滤；查询里的 `CallerID` 只能**收窄**，不能
//     放大——两者分开传，实现方拿不到"只按请求参数过滤"的写法；
//  3. **错误分类是冻结的对外语义**：`ErrForbidden` → 403、`ErrNotFound` → 404、
//     `ErrDeleted` → 410。跨调用方一律 `ErrNotFound`（**不得**用 403 泄露他人会话
//     的存在性，C-28）。
//
// This file declares the consumer-side port and the frozen error classes: 403 for a
// rejected identity, 404 for anything the caller may not see (cross-caller reads
// report not-found, never forbidden), 410 for a message removed by retention.

import (
	"context"
	"errors"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

var (
	// ErrForbidden 表示身份合法但无权读取该范围（缺 `sessions:read`，或以调用方身份
	// 指定了他人 `caller_id`）。它是 `core.ErrForbidden` 的**同值别名**——两种拼写
	// `errors.Is` 都成立，web 层的 403 映射只有一处判据（沿 `knowledge.ErrForbidden`
	// 先例）。
	// ErrForbidden is a same-value alias of core.ErrForbidden so both spellings match.
	ErrForbidden = core.ErrForbidden

	// ErrNotFound 表示消息、会话不存在，或存在但**不属于**当前调用方。跨调用方读取
	// 一律走这一条，**不得**改报 `ErrForbidden`（那会泄露他人会话的存在性）。
	// ErrNotFound covers "missing" and "not yours"; cross-caller reads never report
	// forbidden, which would leak existence.
	ErrNotFound = errors.New("history: not found")

	// ErrDeleted 表示消息已被保留策略删除（410 GONE）。与 `ErrNotFound` 分开：前者
	// 说明"曾经存在且被清理"，后者说明"从未有过或无权知道"。
	// ErrDeleted marks a message removed by the retention policy; it is distinct from
	// not-found because it tells the caller the row used to exist.
	ErrDeleted = errors.New("history: message deleted by retention policy")

	// ErrInvalidQuery 表示查询形状非法（关键词为空、时间窗倒置、limit 越界）。
	// 它在任何存储调用之前拒绝，**不得**降级成"空结果"。
	// ErrInvalidQuery rejects a malformed query before any store call; it never
	// degrades into an empty result.
	ErrInvalidQuery = errors.New("history: invalid query")

	// ErrHistoryUnavailable 表示装配缺存储端口：检索在解析范围与任何查询之前被拒绝，
	// **不得**用空结果冒充"查过了没有"。
	// ErrHistoryUnavailable reports a missing port; it never impersonates an empty
	// result.
	ErrHistoryUnavailable = errors.New("history: history store is not configured")
)

// Scope 是**已结算**的归属范围：由 `core.Principal` 得来，端口实现按它过滤，
// **不再**读请求里的身份字段。
//
//   - `Admin` 为真时 `CallerID` 为空表示不限调用方（含迁移遗留的空归属会话）；
//     `CallerID` 非空表示管理员只看某一个调用方。
//   - `Admin` 为假时 `CallerID` **必填**——空值会让过滤退化成"全部"，是被
//     C-28 明确禁止的越权形态。
//
// Scope is the resolved ownership filter: admin reads may span callers, a caller
// scope always names exactly one owner and must never be empty.
type Scope struct {
	// CallerID 是 enforced 的归属过滤；非管理员身份下必填。
	// CallerID is the enforced owner filter; required for a caller scope.
	CallerID string
	// Admin 表示这是内置管理员的跨调用方读取。
	// Admin marks a cross-caller administrator read.
	Admin bool
}

// SessionRef 是 `ResolveSession` 读回的会话标识：只含归属判定与展示所需的最小字段。
// SessionRef is the minimal session identity returned by ResolveSession.
type SessionRef struct {
	// SessionID 是会话标识。
	SessionID string
	// CallerID 是会话归属；空串表示迁移遗留的无归属会话（只允许管理员读取）。
	// CallerID is the owning caller; empty means legacy migration rows, admin only.
	CallerID string
	// Agent 是会话所属 Agent 画像（`sessions.profile_name`）。
	Agent string
	// CreatedAt 是会话创建时间（UTC）。
	CreatedAt time.Time
}

// MessageRef 是精确读取的请求：按**消息主键**定位，不经过检索。
//
// `MessageID` 是 `messages.id` 的十进制串；无状态 Run 的临时标识
// `msg/<run>/<pos>` 不是主键，读取返回 `ErrNotFound`。给出 `SessionID` 时按会话
// 范围复核，归属不符按未找到处理。
// MessageRef locates a message by primary key; the stateless `msg/<run>/<pos>` id is
// not a primary key and resolves to ErrNotFound.
type MessageRef struct {
	// MessageID 是 `messages.id` 的十进制串。
	MessageID string
	// SessionID 可选：给出时按会话范围复核归属。
	SessionID string
}

// MessageView 是精确读取的原文视图，字段逐条对应冻结契约的 `MessageDetail`：
// 原文 + 元数据 + 截断标注 + 消息携带的来源引用。它**不**携带检索分数，也**不**
// 携带引用有效性判定——`EvidenceRefs` 只是原样透传，`valid` / `reason` 由调用方
// 拿 `knowledge.Validity` 逐个按当次身份复核后合成。
// MessageView mirrors the frozen MessageDetail schema: content, metadata and the
// truncation flag, nothing else.
type MessageView struct {
	// MessageID 是 `messages.id` 的十进制串。
	MessageID string
	// SessionID 是消息所属会话。
	SessionID string
	// RunID 是产生该消息的 Run；system 注入类消息可为空。
	RunID string
	// Seq 是会话内序号（1 起，追加序）。
	Seq int64
	// Role 取 user / assistant / tool / system。
	Role string
	// Content 是原文；工具结果可能已按上限截断，截断时 Truncated 为真。
	Content string
	// Truncated 表示原文被截断（重查保留策略后判定），**不得**静默截断。
	Truncated bool
	// ToolCallID 是工具结果消息对应的工具调用标识（可空）。
	ToolCallID string
	// EvidenceRefs 是消息携带的来源引用（`message_evidence_refs`），**原样**返回：
	// 精确读取**不得**把它们解析成"最新版本"，也**不得**改写已发送消息（C-26）。
	// 每条的 `valid` / `reason` 标注由能读知识库的调用方（web 端点）按**当次**身份
	// 重查后合成——本包**不**读取 B 的知识库（§3.1.10），因此这里只有原始引用。
	// EvidenceRefs passes the persisted references through untouched; validity is
	// annotated by the caller that may read the knowledge base, never here.
	EvidenceRefs []core.EvidenceRef
	// CreatedAt 是消息创建时间（UTC）。
	CreatedAt time.Time
}

// MessageQueryStore 是历史回查的存储端口：检索、精确读取、会话归属解析三件事。
//
// 实现方（`storage/postgres`）**必须**：
//   - 在 SQL 内按 `scope` 过滤归属——`sessions:read` 只是前提，归属过滤在查询里
//     执行，**不得**先取回再在内存里筛；
//   - 按 `q.Limit` 截断并保持"时间倒序、同刻按 message_id 倒序"的稳定顺序；
//   - **不**在检索结果里返回整段正文：`Snippet` 是片段，原文只经 `ReadMessages`；
//   - 按 `messages.id` 定位 `ReadMessages`；无对应行 → `ErrNotFound`，保留策略
//     删除 → `ErrDeleted`。
//
// The port requires ownership filtering inside SQL, stable ordering, snippets
// rather than full content on search, and ErrNotFound/ErrDeleted on exact reads.
type MessageQueryStore interface {
	// SearchMessages 按范围与条件检索消息指针，返回至多 `q.Limit` 条。
	// SearchMessages returns at most q.Limit message pointers.
	SearchMessages(ctx context.Context, scope Scope, q HistoryQuery) ([]HistoryHit, error)
	// ReadMessages 按主键精确读取原文；范围归属不符按 `ErrNotFound`。
	// ReadMessages returns the full text for the given refs.
	ReadMessages(ctx context.Context, scope Scope, refs []MessageRef) ([]MessageView, error)
	// ResolveSession 解析会话归属；不存在或不在 `scope` 内 → `ErrNotFound`。
	// ResolveSession resolves session ownership; out-of-scope is ErrNotFound.
	ResolveSession(ctx context.Context, scope Scope, sessionID string) (SessionRef, error)
}
