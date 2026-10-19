package core

import (
	"context"
	"encoding/json"
	"time"
)

// 本文件是运行面的**消费方接口声明**：core 需要什么就声明什么，实现分别落在
// internal/storage（四个仓储）与 internal/tool（Resolver / Policy / CommandRunner）。
// 接口定义在消费方是依赖倒置的落点——引擎不 import 任何实现包（宪法 VIII）。
// This file declares the consumer-side interfaces of the run surface. Implementations
// live in internal/storage and internal/tool; the engine imports no implementation
// package (constitution VIII).

// MessageStore 是会话历史的写透接口：内存里的 Session 经它落库。
// **追加接口显式承载 `runID`**——Message 本身没有 Run 字段（契约 DDL 事实），
// 而崩溃恢复要按 `messages.run_id` 定位半截组；缺了它，恢复无从判定。
// MessageStore is the write-through interface for session history. AppendMessage takes
// runID explicitly: Message carries no run field, yet recovery locates half-groups by
// messages.run_id — without it recovery cannot decide anything.
type MessageStore interface {
	// AppendMessage 追加一条消息，`runID` 落 `run_id` 列（system 注入类可为空串 → 列写 NULL）。
	// AppendMessage appends one message; an empty runID writes NULL into the run_id column.
	AppendMessage(ctx context.Context, sessionID, runID string, m Message) error

	// Messages 按 `seq` 升序取回整个会话历史。
	// Messages returns the full session history in ascending seq order.
	Messages(ctx context.Context, sessionID string) ([]Message, error)

	// UpsertSession 写入或刷新会话行（不存在即建、存在则刷新最后活动时间）。
	// UpsertSession inserts or refreshes the session row.
	UpsertSession(ctx context.Context, rec SessionRecord) error

	// ListSessions 列出会话（`session list` 只读命令的数据源；排序口径由实现固定）。
	// ListSessions lists sessions for the read-only `session list` command.
	ListSessions(ctx context.Context) ([]SessionRecord, error)
}

// RunStore 是运行账本的写接口。**不接受 finalText 参数**——最终回答只存在
// RunResult 与会话的 assistant 消息里（§3.4：`runs` 表无 `final_text` 列）。
// RunStore is the write interface of the run ledger. FinishRun takes no finalText:
// the final answer lives in RunResult and the assistant message only.
type RunStore interface {
	// StartRun 建 `status=running` 的账本行（`trigger` 由 Channel 按契约 §5 映射）。
	// StartRun inserts the running ledger row; the trigger derives from Channel (contract §5).
	StartRun(ctx context.Context, rec RunRecord) error

	// FinishRun 把终态写回同一行（status / reason / iterations / tool_calls / finished_at）。
	// 写失败**不得**覆盖已得结果：调用方如实返回 RunResult，行留给下次启动的恢复扫描收尾。
	// FinishRun writes the terminal state onto the same row. A write failure must not
	// overwrite an obtained result: the caller returns it as-is and recovery closes the row.
	FinishRun(ctx context.Context, runID string, status RunStatus, reason string, iterations, toolCalls int, now time.Time) error
}

// ToolInvocationStore 是工具审计的两阶段写接口（契约 §7.2，fail-closed）：
// 先写 `pending` 再执行；**预写失败即拒绝执行**；返回后回填终态同一行。
// ToolInvocationStore is the two-phase write interface of tool audit (contract §7.2):
// a failed pre-write means the execution is refused; the terminal state updates the same row.
type ToolInvocationStore interface {
	// BeginInvocation 写 `pending` 行并返回行 id；失败 = 调用方**拒绝执行**该次调用。
	// BeginInvocation inserts the pending row and returns its id; a failure here means
	// the caller must refuse to execute.
	BeginInvocation(ctx context.Context, inv ToolInvocation) (int64, error)

	// FinishInvocation 把该行更新为终态；回填失败**不得**覆盖真实结果（记 ERROR、行留 pending）。
	// FinishInvocation writes the terminal state; a failed write keeps the row pending and
	// must never fake a terminal value.
	FinishInvocation(ctx context.Context, id int64, res ToolInvocationResult) error
}

// ToolInvocationReader 是**补齐路径专用的只读**审计接口：Session 只读不写审计，
// 仓储的同一实现满足它（写路径经 ToolInvocationStore，读写合成一个实现、两个消费面）。
// ToolInvocationReader is the read-only audit interface used by the synthesis path:
// Session reads audit rows but never writes them.
type ToolInvocationReader interface {
	// Lookup 按 (runID, toolCallID) 取审计行；未命中返回 ok=false（无行 ≠ 错误）。
	// Lookup fetches the row by (runID, toolCallID); a miss returns ok=false, not an error.
	Lookup(ctx context.Context, runID, toolCallID string) (ToolInvocationView, bool, error)
}

// SnapshotStore 是配置快照的落库接口（insert-if-absent；`UNIQUE(agent_name, hash)` 冲突即 no-op）。
// SnapshotStore persists the config snapshot; a unique-key conflict is a no-op.
type SnapshotStore interface {
	// SaveSnapshot 保存快照；失败 = 该次运行**拒绝启动**（零模型调用）。
	// SaveSnapshot persists the snapshot; a failure refuses the run (zero model calls).
	SaveSnapshot(ctx context.Context, snap *ConfigSnapshot) error
}

// ToolResolver 按名解析工具与取规格——名字集由装配期从注册表导出（宪法 III 的显式面）。
// ToolResolver resolves tools by name and returns their specs; the name set is derived
// explicitly at assembly time (constitution III).
type ToolResolver interface {
	// Resolve 查注册池；未命中返回 ok=false（不遍历、不推断）。
	// Resolve looks the pool up by name; a miss returns ok=false.
	Resolve(name string) (Tool, bool)

	// Specs 按传入名字的**顺序**逐名取规格；未命中的名字跳过（运行时归属校验已先行把关）。
	// Specs returns specs in the order the names were given; unknown names are skipped.
	Specs(names []string) []ToolSpec
}

// Policy 是授权判定接口（技术方案 §4.3 逐字）：回答"这个动作允许吗"，不负责"怎么做"。
// 调用点在**工具内部**（文件工具只需 Policy，命令执行才需 CommandRunner）——调用方
// 只依赖本接口，**不得**依赖 WhitelistPolicy 等具体实现。
// Policy answers "is this action allowed?"; the call site lives inside each tool and
// callers depend on the interface only, never on a concrete implementation.
type Policy interface {
	// Check 判定失败返回拒绝错误（`*SandboxViolationError`，四段式文案，模型可见）。
	// Check returns a SandboxViolationError on refusal; the message is model-visible.
	Check(ctx context.Context, action Action) error
}

// CommandRunner 是受控子进程执行接口（技术方案 §4.3 逐字）：回答"怎么受控地执行"——
// cwd、环境变量白名单、输出上限、取消与进程树回收都在实现侧。
// CommandRunner executes a command under control: cwd, env allowlist, output cap,
// cancellation and process-tree cleanup all live in the implementation.
type CommandRunner interface {
	// Run 执行一次受控命令；超时 / 取消经 ctx 表达，实现必须回收整棵进程树。
	// Run executes one controlled command; the implementation must reap the whole tree.
	Run(ctx context.Context, cmd Command) (CommandResult, error)
}

// ToolInvocation 是工具审计行的**预写形态**：只含执行前可知的字段。
// **不带状态字段**——预写态恒为 `pending`，由仓储 SQL 固定写（调用方给不出错状态）。
// 入参 `InputJSON` 原样保存（模型给的 arguments 是 JSON 字符串，不重编码）。
// ToolInvocation is the pre-write shape of a tool audit row: fields known before
// execution only. It carries no status field on purpose — the repo writes pending in SQL.
type ToolInvocation struct {
	RunID      string
	SessionID  string
	ToolCallID string
	ToolName   string
	InputJSON  json.RawMessage
	CreatedAt  time.Time // 列存 Unix 毫秒 UTC（契约迁移纪律 4）
}

// ToolInvocationResult 是工具审计行的**终态回填形态**：成败只由 Status 表达，不设布尔列。
// `ResultJSON` 是结果表示（`{"content":…,"retryable":…,"truncated":true}`）——截断标记
// 写在值内，**不**新造列；`ErrorMessage` 截断 4 KB 的责任在 storage。
// ToolInvocationResult is the terminal shape of a tool audit row: success or failure is
// expressed by Status alone; the truncation flag lives inside ResultJSON, never in a column.
type ToolInvocationResult struct {
	Status       LLMCallStatus
	ResultJSON   json.RawMessage
	ErrorMessage string
	ErrorKind    ErrorKind // 失败必填（契约 §7.3 七值）；成功留空 → 列写 NULL
	Duration     time.Duration
}

// ToolInvocationView 是补齐路径读到的审计行视图：终态行按 `ResultJSON` **重建真实结果**，
// `pending` / `unknown` 行取「结果未知」文案（契约 §4.4 五分支）——故只读这三样，
// 不把整行搬进 core（审计列面归 storage）。
// ToolInvocationView is what the synthesis path reads: a terminal row rebuilds the real
// result from ResultJSON, a pending/unknown row yields the unknown-result text.
type ToolInvocationView struct {
	Status       LLMCallStatus
	ResultJSON   json.RawMessage
	ErrorMessage string
	ErrorKind    ErrorKind
}

// SessionRecord 是会话行的载体（`sessions` 表的读写面）：
// 读侧供 `session list` 渲染（标识 / 消息数 / 最后活动），写侧建行与刷新。
// SessionRecord carries a session row: the read side feeds `session list`, the write
// side inserts or refreshes the row.
type SessionRecord struct {
	ID           string // 由命令面按「渠道 + 用户 + Agent 画像」派生（契约 §5）
	ProfileName  string
	Channel      string
	UserID       string
	MessageCount int
	CreatedAt    time.Time
	LastActiveAt time.Time
	// Status 是会话状态（active | archived）；ListSessions 不填，保持零值。
	Status string
	// ArchivedAt 是可空归档时间；仅 GetSession / ArchiveSession 填充。
	ArchivedAt *time.Time
}

// RunRecord 是运行账本的**起始行载体**：只含开跑前可知的字段。
// **不含状态与终态列**——起始态恒为 `running`、终态经 FinishRun 写入（同预写纪律：
// 状态不由调用方给，也就没有传错的可能）。`SessionID` 空串 = 无状态调用 → 列写 NULL。
// RunRecord is the starting-row shape of the run ledger: fields known before the run.
// Status is deliberately absent — the row starts as running and only FinishRun writes
// a terminal state. An empty SessionID means a stateless run and writes NULL.
type RunRecord struct {
	RunID      string
	SessionID  string
	Agent      string
	Channel    string
	UserID     string
	Trigger    Trigger // 落库触发方式：scheduler 显式填 cron / manual；空 = 由 storage 按 Channel 映射（cli→user / web→api / scheduler→cron / admin→manual）
	ConfigHash string  // 本次运行使用的配置快照版本（关联 config_snapshots.hash）
	StartedAt  time.Time
}
