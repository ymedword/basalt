package core

import (
	"context"
	"errors"
	"sync"
	"time"
)

// 本文件是 Agent 生命周期的领域类型与**进程内同步原语**（颗粒度文档 §3.1.10）：
// 三值状态、生命周期记录、状态文件端口、索引写端口与按 Agent 分桶的读写锁。
// 状态文件的字节格式、校验和与原子写属 internal/workspace（那里有文件系统），
// core 只冻结"谁能读写、以什么契约读写"。
// This file declares the agent lifecycle domain types and the per-agent read/write
// gate. Byte format, checksum and atomic writes belong to internal/workspace; core
// freezes only the contract of who may read or write the authoritative state.

// AgentState 是生命周期的三值字面量（契约 agent_lifecycle.state CHECK 逐字）。
// AgentState is the three-value literal set of the lifecycle state.
type AgentState string

const (
	// AgentActive 是唯一放行新运行的状态。
	// AgentActive is the only state that admits new runs.
	AgentActive AgentState = "active"
	// AgentDisabled 拒绝新运行、不取消在途运行。
	// AgentDisabled refuses new runs without cancelling in-flight ones.
	AgentDisabled AgentState = "disabled"
	// AgentArchived 是归档态：任何入口（HTTP / CLI / 调度）都拒绝新运行。
	// AgentArchived refuses new runs from every entry point.
	AgentArchived AgentState = "archived"
)

// LifecycleRecord 是一个 Agent 状态文件的领域形态；`updated_at` 是 Unix 毫秒（UTC），
// 与状态文件字节和 `agent_lifecycle` 索引列同口径。
// LifecycleRecord is the domain shape of one agent state file; UpdatedAt is Unix
// milliseconds in UTC, matching both the file bytes and the index column.
type LifecycleRecord struct {
	SchemaVersion int    // v1 为 US-11 形态；v2（US-12 R4）追加 AgentUID
	Agent         string // Agent 名（= 目录名）
	// AgentUID 是稳定身份（`ag_` 前缀）：v2 起必填，v1 记录为空、由迁移补写。
	// 资源资格判定与审计关联只认它，不认名称。
	AgentUID    AgentUID // stable agent identity; empty on v1 records until migration
	State       AgentState
	Generation  int64  // 每次迁移 +1；启停的乐观锁字段
	UpdatedAt   int64  // Unix 毫秒（UTC）
	OperationID string // 写入本次状态的真实 op-... 标识（含基线）
	Checksum    string // "sha256:<64 位小写十六进制>"
}

// LifecycleExpectation 是准入时从 AgentGate 读到的生命周期期望值（P-16）：`StartRun`
// 在事务内与 `agent_lifecycle` 索引比对（代次 + 状态文件哈希），不符即拒绝。
// `StateFileHash` 与索引列同口径（`sha256:<64 位小写十六进制>`）。零值表示调用方
// **未提供**——`StartRun` 据此**拒绝**而不是跳过：缺少期望值就无法做最终复查，
// "跳过复查"等于让发布竞争静默通过。
// LifecycleExpectation is the admission-time lifecycle expectation compared inside
// StartRun's transaction. Zero means "not provided" and is refused, never skipped.
type LifecycleExpectation struct {
	Generation    int64
	StateFileHash string
}

// ErrLifecycleConflict 表示生命周期迁移 / 撤销的前置条件不成立：期望状态或代数不符，
// 或撤销目标不是"同一操作创建、generation=1"的状态文件。调用方以 errors.Is 判定，
// **不得**比对错误文本。
// ErrLifecycleConflict means the lifecycle precondition failed: the expected state or
// generation does not match, or a discard targets a file it must not remove.
var ErrLifecycleConflict = errors.New("lifecycle conflict")

// LifecyclePort 是状态文件的消费方端口（实现在 internal/workspace）。
// `Read` 返回 `(record, exists, error)`：缺失 / 损坏时 exists=false 且 error 说明原因，
// 由准入映射为不可用——**不得**把缺失读成 active（失败关闭）。
// Read returns exists=false with a reason error when the file is missing or corrupt;
// admission maps that to unavailable, never to active.
type LifecyclePort interface {
	Read(ctx context.Context, agent string) (LifecycleRecord, bool, error)
	// Transition 必须在调用方已持有该 Agent 写锁的前提下使用；前置条件不符返回
	// ErrLifecycleConflict。
	// Transition requires the caller to hold the agent's write lock; a failed
	// precondition returns ErrLifecycleConflict.
	Transition(ctx context.Context, agent string, expectedState AgentState, expectedGeneration int64, target AgentState, operationID string) (LifecycleRecord, error)
	// Bootstrap 只在状态文件不存在时写 disabled / generation=1；已存在则原样返回。
	// Bootstrap writes disabled/generation=1 only when the file is absent.
	Bootstrap(ctx context.Context, agent, operationID string) (LifecycleRecord, error)
	// BootstrapIdentity 与 Bootstrap 相同，但使用调用方给定的稳定身份：归档恢复（原名 /
	// 异名两条路径）必须沿用原 agent_uid，不得在此另生成新身份。已存在的 v2 文件原样
	// 返回；已存在且身份不同 → ErrLifecycleConflict（**不得**静默改身份）。
	// BootstrapIdentity writes a disabled/generation=1 file carrying the caller's UID;
	// archive restore must reuse the original identity, never mint a new one.
	BootstrapIdentity(ctx context.Context, agent string, agentUID AgentUID, operationID string) (LifecycleRecord, error)
	// Discard 只撤销"由同一 operationID 创建、generation=1"的状态文件；其它情况返回
	// ErrLifecycleConflict。用于创建 / 复制 / 恢复失败后的回滚与 CLI 目录删除。
	// Discard removes only a generation-1 file created by the same operation.
	Discard(ctx context.Context, agent, operationID string) error
}

// IdentityUpgradePort 供迁移编排把 v1 状态文件升级为 v2（补发 agent_uid）。
// 文件缺失返回 (zero, false, nil)：由调用方按 `legacy_unrecorded` 记数据库镜像，
// **不得**按同名猜身份。
// IdentityUpgradePort upgrades v1 state files during the v3 migration; a missing file
// is reported as not-upgraded so callers can record legacy_unrecorded.
type IdentityUpgradePort interface {
	UpgradeIdentity(ctx context.Context, agent, operationID string) (LifecycleRecord, bool, error)
}

// LifecycleIndex 是 `agent_lifecycle` 索引的读写端口：索引可由状态文件重建，
// 写失败不回滚状态文件，但必须让调用方可见（WARN + 下次启动基线修复）。
// 写入遵守**代次单调**：本地代次低于索引即拒绝（`ErrAgentLifecycleStale`）；同代次
// 只接受逐项完全一致的内容（`ErrAgentLifecycleMismatch`），本地代次更高由调用方
// 走管理操作单调修复，索引自己**不得**自动回退或分裂。
// LifecycleIndex is the rebuildable index read/write port; writes are monotonic in
// generation and refuse same-generation disagreement, never rolling back the index.
type LifecycleIndex interface {
	// ReadLifecycleIndex 读共享索引行：返回记录、`valid` 标志与无效原因。索引无该
	// Agent 时返回带 Agent 名的零记录、valid=false、空原因与 nil 错误（"没有索引行"
	// 不是故障，调用方据此回退到状态文件读取）。调用方从返回的代次与
	// `state_file_hash` 派生准入期期望（`LifecycleExpectation`，本文件）。
	//
	// ReadLifecycleIndex reads one index row; a missing row is not an error, so callers
	// can fall back to the authoritative state file.
	ReadLifecycleIndex(ctx context.Context, agent string) (LifecycleRecord, bool, string, error)
	PutLifecycleIndex(ctx context.Context, rec LifecycleRecord, valid bool, errMsg string) error
	// SetLifecycleBarrier 是归档的**准入屏障**（FR-24-41）：在同一条 `agent_lifecycle`
	// 行上把 `active` 条件更新为 `disabled`，并记下发起归档的操作标识。它是准入否决，
	// **不是**状态迁移——`generation`、`state_file_hash` 与状态文件一律不动：状态文件
	// 仍是唯一权威，屏障只封住共享索引上的新 Run。取行锁的 UPDATE 与 `StartRun` 事务内
	// 对同一行的 `SELECT … FOR UPDATE` 复查因此互斥（两种真实获锁顺序都成立）。
	// 行不存在或已非 active → applied=false（这两种情况本就不再准入）；只有数据库
	// 不可用才返回 error。
	//
	// SetLifecycleBarrier flips active→disabled on the shared row as an admission veto.
	// It is not a state transition: generation, state_file_hash and the authoritative
	// file are untouched, so the row lock it takes is exactly the lock StartRun's
	// FOR UPDATE recheck takes.
	SetLifecycleBarrier(ctx context.Context, agent, operationID string, at time.Time) (applied bool, err error)
	// RestoreLifecycleBarrier 把状态文件记录里的 `(state, generation, state_file_hash,
	// updated_at, operation_id)` 原样写回索引，且**只撤销本次屏障**：条件是当前行仍
	// 为 `disabled`、`operation_id` 等于本次归档屏障写下的标识、`generation` 仍等于
	// 屏障期的代次。三者缺一即 applied=false——屏障之外的推进（别的进程已完成归档、
	// 用户重新启用、同一归档操作自己推进了代次）都**不得**被这次补偿覆盖。
	// 只判 `state='disabled'` 不够：后续停用同样把行置为 disabled，会把更晚的管理
	// 操作回写成旧代次。
	// 它不走 `PutLifecycleIndex` 的代次单调规则：写回的是**同一代次**的权威内容。
	//
	// RestoreLifecycleBarrier writes the state-file tuple back only when the row is
	// still the disabled row this archive operation installed: same barrier operation id
	// and the generation observed at barrier time. It bypasses the monotonic-generation
	// rule because it restores the same generation from the authoritative file.
	RestoreLifecycleBarrier(ctx context.Context, rec LifecycleRecord,
		barrierOperationID string, expectedGeneration int64) (applied bool, err error)
}

// 归档完成判据的**固定字面量**（图 8 [4] ①②③④）。它们是 `AgentBusyError.Unmet`
// 与 `data.unmet_predicates` 的出站取值，**不得**改写或本地另拼。
// The fixed predicate literals for "the agent finished cleanly"; they are the only
// values that may appear in AgentBusyError.Unmet.
const (
	// ArchiveResidualRunningRun ①：该 Agent 仍有 `status='running'` 的 run 行。
	ArchiveResidualRunningRun = "no_running_run"
	// ArchiveResidualPendingAudit ②：以该 Agent 的 Run 为范围仍有 pending 审计行。
	ArchiveResidualPendingAudit = "no_pending_audit"
	// ArchiveResidualRefsReleased ③：三类运行引用对该 Agent 的 Run 全部已释放。
	ArchiveResidualRefsReleased = "refs_released"
	// ArchiveResidualPendingDelivery ④：该 Agent 没有 pending 的投递。
	ArchiveResidualPendingDelivery = "no_pending_delivery"
)

// ArchiveResiduals 是一次"归档完成判据"快照：仍在运行的 Run 与**未达成**的判据
// （取值只来自上面四个固定字面量）。空 Unmet 表示可以安全继续归档；租约到期与否
// 不参与判定——租约只说明没人能再提交，不证明执行已退出（图 8 的 R4 修正）。
//
// ArchiveResiduals snapshots the archive-completion predicates: the runs still running
// and the predicates that do not hold yet. An empty Unmet means the archive may proceed.
type ArchiveResiduals struct {
	RunIDs []string
	Unmet  []string
}

// AgentGate 按 Agent 名分桶：同一 Agent 的准入读锁、启停写锁、内容写锁互斥，
// 不同 Agent 互不影响——全局一把大锁会让一只 Agent 的编辑阻塞其余全部运行。
// 分桶在最后一个持有者释放时回收，锁表不会随历史 Agent 数量无界增长。
// AgentGate buckets one RWMutex per agent name so that agents never block each
// other; an idle bucket is reclaimed when its last holder releases it.
type AgentGate struct {
	mu    sync.Mutex
	locks map[string]*agentGateBucket
}

// agentGateBucket 是一个 Agent 的锁与持有计数；计数保护回收时机。
// agentGateBucket pairs the per-agent lock with a holder count for reclamation.
type agentGateBucket struct {
	mu   sync.RWMutex
	refs int
}

// NewAgentGate 创建一个空的 Agent 级读写锁表。
// NewAgentGate returns an empty per-agent gate.
func NewAgentGate() *AgentGate { return &AgentGate{locks: map[string]*agentGateBucket{}} }

// RLock 取得该 Agent 的读锁，返回幂等的释放函数（defer 与错误路径都可能调用）。
// 读锁用于运行准入、内容读取等"不改变生命周期"的路径。
// RLock acquires the agent's read lock and returns an idempotent release function.
func (g *AgentGate) RLock(agent string) func() {
	b := g.acquire(agent)
	b.mu.RLock()
	return sync.OnceFunc(func() {
		b.mu.RUnlock()
		g.release(agent, b)
	})
}

// Lock 取得该 Agent 的写锁，返回幂等的释放函数。写锁用于启停迁移与内容写入。
// Lock acquires the agent's write lock and returns an idempotent release function.
func (g *AgentGate) Lock(agent string) func() {
	b := g.acquire(agent)
	b.mu.Lock()
	return sync.OnceFunc(func() {
		b.mu.Unlock()
		g.release(agent, b)
	})
}

// acquire 取回（必要时创建）该 Agent 的桶并把持有计数 +1。
// **先加计数再等锁**：等待者也算持有者，桶在其等待期间不得被回收。
// acquire returns (creating if needed) the agent's bucket and increments its holder
// count before waiting, so a waiting holder keeps the bucket alive.
func (g *AgentGate) acquire(agent string) *agentGateBucket {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.locks == nil {
		g.locks = map[string]*agentGateBucket{}
	}
	b := g.locks[agent]
	if b == nil {
		b = &agentGateBucket{}
		g.locks[agent] = b
	}
	b.refs++
	return b
}

// release 把持有计数 -1；归零时从锁表移除。必须在解锁**之后**调用。
// release decrements the holder count and reclaims an idle bucket; it must run
// after the bucket lock has been released.
func (g *AgentGate) release(agent string, b *agentGateBucket) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b.refs--
	if b.refs == 0 {
		delete(g.locks, agent)
	}
}
