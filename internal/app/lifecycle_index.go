package app

// 本文件把「生命周期判定索引优先」（FR-25C-14 / 模块卡 §2.3.2 路径表第 3 行）接成可用的
// 读面：准入端口与调度门共用同一个读取实现，因此「索引优先、无行回退状态文件、坏行 /
// 读失败失败关闭」只有一处判据——两处各写一套正是本 US 要关掉的半截一致性。
// This file wires index-first lifecycle reads. Admission and the schedule gate share one
// reader, so the precedence rule has exactly one implementation.

import (
	"context"
	"errors"
	"fmt"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// errLifecycleStoreAbsent 表示端口既没有索引行可读、也没有状态文件存储可回退：装配缺失
// 不得演变成 panic，也不得猜成 active。
// errLifecycleStoreAbsent is the assembly guard: a missing store is an error, never active.
var errLifecycleStoreAbsent = errors.New("生命周期状态文件存储未装配")

// lifecycleIndexReader 是消费方声明的最小读面（`*postgres.ResourceStore` 天然满足）：
// 只含索引读取。共享索引的**写**归管理面单点回写，本装配不写索引——多一条写路径就多
// 一份代次单调争执。
// lifecycleIndexReader is the minimal read face this assembly consumes; the index is
// written by the management path only, never from here.
type lifecycleIndexReader interface {
	ReadLifecycleIndex(ctx context.Context, agent string) (core.LifecycleRecord, bool, string, error)
}

// readLifecyclePreferred 是索引优先判定的**唯一**实现（准入端口与调度门共用）：
//
//  1. 未装配索引（老部署 / 索引尚未接线）→ 与改造前逐字一致，直接读状态文件；
//  2. 索引行可用 → 以索引为准：本地状态文件是各机旧副本，不得反过来盖住共享判定；
//  3. 索引行存在但不可用（valid=false 且带原因）→ 失败关闭，**不回退**文件——回退
//     等于把共享判定降级成各机各判，"A 停用、B 仍按旧文件跑"正是这么来的；
//  4. 索引读失败（数据库不可用）→ 同样失败关闭，不静默拿旧副本顶上；
//  5. 索引里**没有**该 Agent 的行（valid=false 且原因为空）→ 不是故障，回退状态文件。
//
// 返回 `(record, exists, error)` 与 `core.LifecyclePort.Read` 同口径：exists=false 且
// error 说明原因，调用方映射为不可用。
// readLifecyclePreferred is the single implementation of the precedence rule; a row that
// exists but is unusable fails closed instead of falling back to the stale local copy.
func readLifecyclePreferred(ctx context.Context, file *workspace.LifecycleStore, index lifecycleIndexReader, agent string) (core.LifecycleRecord, bool, error) {
	if index != nil {
		rec, valid, reason, err := index.ReadLifecycleIndex(ctx, agent)
		if err != nil {
			return core.LifecycleRecord{}, false, fmt.Errorf("读取生命周期索引失败（%s）：%w", agent, err)
		}
		if valid {
			return rec, true, nil
		}
		if reason != "" {
			return core.LifecycleRecord{}, false, fmt.Errorf("生命周期索引行不可用（%s）：%s", agent, reason)
		}
	}
	if file == nil {
		return core.LifecycleRecord{}, false, errLifecycleStoreAbsent
	}
	return file.Read(ctx, agent)
}

// lifecycleIndexPort 是准入路径的 `core.LifecyclePort` 实现：读走索引优先，写仍**只**
// 委托本地状态文件。索引由管理面在状态文件成功落盘后单点回写；端口不写索引，因此不会
// 出现绕过代次单调守卫的第二条写路径。
// lifecycleIndexPort reads index-first and still delegates every write to the state file.
type lifecycleIndexPort struct {
	file  *workspace.LifecycleStore
	index lifecycleIndexReader
}

// 编译期断言：端口与状态文件存储同形，装配处可直接替换。
// Compile-time assertion: the port is a drop-in LifecyclePort.
var _ core.LifecyclePort = (*lifecycleIndexPort)(nil)

// newLifecycleIndexPort 组装端口；index 为 nil 时行为与状态文件存储逐字相同。
// newLifecycleIndexPort accepts a nil index, which preserves the legacy file-only behavior.
func newLifecycleIndexPort(file *workspace.LifecycleStore, index lifecycleIndexReader) *lifecycleIndexPort {
	return &lifecycleIndexPort{file: file, index: index}
}

// Read 按索引优先口径读取，见 readLifecyclePreferred。
// Read follows the index-first precedence documented on readLifecyclePreferred.
func (p *lifecycleIndexPort) Read(ctx context.Context, agent string) (core.LifecycleRecord, bool, error) {
	return readLifecyclePreferred(ctx, p.file, p.index, agent)
}

// Transition 委托状态文件存储；索引不参与写。调用方仍须已持有该 Agent 写锁。
// Transition delegates to the state file; the index takes no part in writes.
func (p *lifecycleIndexPort) Transition(ctx context.Context, agent string, expectedState core.AgentState, expectedGeneration int64, target core.AgentState, operationID string) (core.LifecycleRecord, error) {
	if p.file == nil {
		return core.LifecycleRecord{}, errLifecycleStoreAbsent
	}
	return p.file.Transition(ctx, agent, expectedState, expectedGeneration, target, operationID)
}

// Bootstrap 委托状态文件存储（只在文件不存在时写 disabled / generation=1）。
// Bootstrap delegates to the state file store.
func (p *lifecycleIndexPort) Bootstrap(ctx context.Context, agent, operationID string) (core.LifecycleRecord, error) {
	if p.file == nil {
		return core.LifecycleRecord{}, errLifecycleStoreAbsent
	}
	return p.file.Bootstrap(ctx, agent, operationID)
}

// BootstrapIdentity 委托状态文件存储：归档恢复必须沿用原 agent_uid，不得在此另生成。
// BootstrapIdentity delegates so archive restore keeps the original UID.
func (p *lifecycleIndexPort) BootstrapIdentity(ctx context.Context, agent string, agentUID core.AgentUID, operationID string) (core.LifecycleRecord, error) {
	if p.file == nil {
		return core.LifecycleRecord{}, errLifecycleStoreAbsent
	}
	return p.file.BootstrapIdentity(ctx, agent, agentUID, operationID)
}

// Discard 委托状态文件存储：只撤销同一 operationID 创建且 generation=1 的文件。
// Discard delegates to the state file store.
func (p *lifecycleIndexPort) Discard(ctx context.Context, agent, operationID string) error {
	if p.file == nil {
		return errLifecycleStoreAbsent
	}
	return p.file.Discard(ctx, agent, operationID)
}

// lifecycleScheduleGateFor 构造调度门（14D / E5「启用才运行」）：每次触发都重读，不缓存
// ——启用 / 停用在下一次到点即生效。索引优先、无行回退状态文件；任何读失败、索引行
// 不可用或状态不是 active 一律 false（失败关闭）——门的作用是少跑而不是多跑。
// The gate re-reads every tick and fails closed on any error or unusable index row.
func lifecycleScheduleGateFor(file *workspace.LifecycleStore, index lifecycleIndexReader) core.ScheduleGate {
	if file == nil && index == nil {
		return nil
	}
	return func(ctx context.Context, agent string) bool {
		rec, exists, err := readLifecyclePreferred(ctx, file, index, agent)
		return err == nil && exists && rec.State == core.AgentActive
	}
}
