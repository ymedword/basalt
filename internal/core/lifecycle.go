package core

import (
	"context"
	"errors"
	"sync"
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
	SchemaVersion int    // 契约固定首版 1
	Agent         string // Agent 名（= 目录名）
	State         AgentState
	Generation    int64  // 每次迁移 +1；启停的乐观锁字段
	UpdatedAt     int64  // Unix 毫秒（UTC）
	OperationID   string // 写入本次状态的真实 op-... 标识（含基线）
	Checksum      string // "sha256:<64 位小写十六进制>"
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
	// Discard 只撤销"由同一 operationID 创建、generation=1"的状态文件；其它情况返回
	// ErrLifecycleConflict。用于创建 / 复制 / 恢复失败后的回滚与 CLI 目录删除。
	// Discard removes only a generation-1 file created by the same operation.
	Discard(ctx context.Context, agent, operationID string) error
}

// LifecycleIndex 是 `agent_lifecycle` 索引的写端口：索引可由状态文件重建，
// 写失败不回滚状态文件，但必须让调用方可见（WARN + 下次启动基线修复）。
// LifecycleIndex is the rebuildable index write port; a write failure never rolls
// back the authoritative file, but must stay visible to the caller.
type LifecycleIndex interface {
	PutLifecycleIndex(ctx context.Context, rec LifecycleRecord, valid bool, errMsg string) error
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
