package app

// 本文件是 US-25C FR-25C-14（模块卡 §2.3.2 路径表第 3 行「Agent 启用 / 停用 → `agent_lifecycle`
// 索引 → 索引优先 → 调度门 + 准入」）的定向判据：
//
//   - 有索引行时，**准入与调度门都按索引判**——本地状态文件是各机旧副本，反过来盖住共享
//     判定正是本 US 要关掉的缺口（A 停用、B 仍按旧文件跑）；
//   - **无索引行**才回退本地状态文件（旧部署 / 索引尚未建立时行为不变）；
//   - 索引行存在但不可用（valid=false + 原因）或读失败一律**失败关闭**，不回退文件
//     ——回退等于把共享判定降级成各机各判，恰好是"停用后仍执行"的成因。
//
// 落点（T034 按此实现，本文件先钉判据）：`internal/app/lifecycle_index.go` 给出
// `lifecycleIndexReader`（消费方声明的最小读面）、`readLifecyclePreferred`（唯一实现）、
// `lifecycleIndexPort` + `newLifecycleIndexPort`（实现 `core.LifecyclePort`，写仍走文件）
// 与 `lifecycleScheduleGateFor`；既有 `lifecycleScheduleGate(file)` 退化为
// `lifecycleScheduleGateFor(file, nil)`——既有行为与既有用例逐字不变。
// This file pins index-first lifecycle: with an index row, admission and the schedule gate
// both follow the index; only a missing row falls back to the local state file, and an
// invalid or unreadable index row fails closed rather than falling back.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// errLifecycleAdmitProbe 是夹具在快照阶段的固定失败：走到它就说明生命周期门禁放行了。
// 用它当"过门"的证据，比让用例真的进模型循环更稳定，也不需要模型客户端。
// The sentinel proves the gate let the run through without entering the model loop.
var errLifecycleAdmitProbe = errors.New("生命周期门禁已放行：夹具在快照阶段停下")

// lifecycleIndexRow 是一条夹具索引行（记录 + 可用性 + 无效原因）。
type lifecycleIndexRow struct {
	record core.LifecycleRecord
	valid  bool
	reason string
}

// lifecycleIndexFake 是 `lifecycleIndexReader` 的内存替身：按 Agent 名持行，并提供
// 一次性读故障注入（`fail`），用来证明"读失败不回退文件"。写入面不实现——本文件只判
// 读面，写走状态文件（索引由管理面单点回写），这里留空实现避免出现第二套写语义。
// The fake holds rows plus an injectable read failure; it deliberately exposes no writes.
type lifecycleIndexFake struct {
	mu   sync.Mutex
	rows map[string]lifecycleIndexRow
	fail error
}

// newLifecycleIndexFake 建一个空索引替身。
func newLifecycleIndexFake() *lifecycleIndexFake {
	return &lifecycleIndexFake{rows: make(map[string]lifecycleIndexRow)}
}

// set 覆盖一行（valid / reason 与 `ReadLifecycleIndex` 的返回口径一致）。
func (f *lifecycleIndexFake) set(agent string, record core.LifecycleRecord, valid bool, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record.Agent = agent
	f.rows[agent] = lifecycleIndexRow{record: record, valid: valid, reason: reason}
}

// drop 删掉一行，模拟"索引还没有这个 Agent"。
func (f *lifecycleIndexFake) drop(agent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, agent)
}

// setFailure 注入读故障（nil 表示恢复到可读）。
func (f *lifecycleIndexFake) setFailure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

// ReadLifecycleIndex 按仓储口径返回：无行 = 零记录 + valid=false + 空原因 + nil 错误。
// 无行不是故障，调用方据此回退状态文件。
func (f *lifecycleIndexFake) ReadLifecycleIndex(_ context.Context, agent string) (core.LifecycleRecord, bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return core.LifecycleRecord{}, false, "", f.fail
	}
	row, ok := f.rows[agent]
	if !ok {
		return core.LifecycleRecord{Agent: agent}, false, "", nil
	}
	return row.record, row.valid, row.reason, nil
}

// lifecycleAdmitSource 是准入用的 Agent 装载面替身：`Lookup` 命中，`Snapshot` 固定失败
// ——用例只需要"过没过生命周期门"，不需要真的构建快照或调用模型。
// The source satisfies Lookup and fails deterministically at the snapshot stage.
type lifecycleAdmitSource struct{ name string }

// Lookup 命中夹具里唯一的那只 Agent。
func (s lifecycleAdmitSource) Lookup(name string) (core.AgentEntry, bool) {
	if name != s.name {
		return core.AgentEntry{}, false
	}
	return core.AgentEntry{Name: name, Registered: true}, true
}

// Dir 返回空目录（本用例不触碰文件系统沙箱）。
func (lifecycleAdmitSource) Dir(string) string { return "" }

// Snapshot 固定返回探针错误：到达这里说明生命周期门禁已放行。
func (lifecycleAdmitSource) Snapshot(context.Context, core.AgentEntry, time.Time) (*core.ConfigSnapshot, error) {
	return nil, errLifecycleAdmitProbe
}

// newLifecycleAdmitService 装配一个只跑到快照为止的运行入口：`Loop` 必须非 nil（否则
// 准入在生命周期门禁**之前**就报装配错误），但它不会被进入。
// The service stops at the snapshot stage; the loop is non-nil but never entered.
func newLifecycleAdmitService(port core.LifecyclePort, agent string) *core.AgentService {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return core.NewAgentService(core.ServiceOptions{
		Agents:        lifecycleAdmitSource{name: agent},
		Loop:          core.NewReActLoop(nil, nil, logger),
		Lifecycle:     port,
		MaxConcurrent: 1,
		Logger:        logger,
	})
}

// newLifecycleFile 建一个真实状态文件存储（固定时钟，便于比对代次与哈希）。
func newLifecycleFile(root string) *workspace.LifecycleStore {
	return workspace.NewLifecycleStore(root, func() time.Time {
		return time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	})
}

// setLifecycleState 用真实状态文件存储把某 Agent 落到目标状态：先 Bootstrap（新建即
// disabled / gen 1），目标不同再走一次启停迁移——与生产启停同一入口，不直接写文件。
// 文件已存在时重复调用只会继续迁移，因此同一 Agent 的多次切换是安全的。
func setLifecycleState(t *testing.T, lifecycle *workspace.LifecycleStore, agent string, state core.AgentState) core.LifecycleRecord {
	t.Helper()
	ctx := context.Background()
	rec, err := lifecycle.Bootstrap(ctx, agent, "op-lifecycle-seed")
	require.NoError(t, err)
	if rec.State != state {
		rec, err = lifecycle.Transition(ctx, agent, rec.State, rec.Generation, state, "op-lifecycle-switch")
		require.NoError(t, err)
	}
	require.Equal(t, state, rec.State)
	return rec
}

// TestAdmission_UsesLifecycleIndex 守"准入按索引判"：索引说停用时，本地状态文件仍是
// active 也必须拒绝；索引说启用时，本地文件停用（旧副本）不得反过来拦住 —— 两个方向
// 都要验，只验一个方向的话"读索引"可以被写成"索引为停用时拒绝、否则回退文件"。
// Admission follows the index in both directions, not just for denials.
func TestAdmission_UsesLifecycleIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := "alpha"
	lifecycle := newLifecycleFile(t.TempDir())
	fileRecord := setLifecycleState(t, lifecycle, agent, core.AgentActive)
	uid, err := core.NewAgentUID()
	require.NoError(t, err)

	index := newLifecycleIndexFake()
	// 索引行：另一个 Worker 已把它停用（代次更高），本地文件仍停在 active。
	index.set(agent, core.LifecycleRecord{
		SchemaVersion: 2,
		AgentUID:      uid,
		State:         core.AgentDisabled,
		Generation:    fileRecord.Generation + 3,
		Checksum:      fileRecord.Checksum,
	}, true, "")

	port := newLifecycleIndexPort(lifecycle, index)
	svc := newLifecycleAdmitService(port, agent)

	_, runErr := svc.Run(ctx, core.RunRequest{
		Agent:     agent,
		Principal: core.LocalAdminPrincipal(),
		Channel:   "web",
		Message:   "索引优先判据",
	})
	var unavailable *core.AgentUnavailableError
	require.ErrorAs(t, runErr, &unavailable, "索引说停用必须拒绝新运行，零副作用")
	require.Equal(t, core.AgentDisabled, unavailable.State,
		"拒绝理由必须取索引的状态；取本地文件就会把停用读成 active")
	require.Equal(t, core.AgentUnavailableDisabled, unavailable.Kind)

	// 反向：索引说启用、本地文件已停用（各机旧副本），准入必须放行。
	fileRecord = setLifecycleState(t, lifecycle, agent, core.AgentDisabled)
	index.set(agent, core.LifecycleRecord{
		SchemaVersion: 2,
		AgentUID:      uid,
		State:         core.AgentActive,
		Generation:    fileRecord.Generation + 1,
		Checksum:      fileRecord.Checksum,
	}, true, "")

	result, runErr := svc.Run(ctx, core.RunRequest{
		Agent:     agent,
		Principal: core.LocalAdminPrincipal(),
		Channel:   "web",
		Message:   "索引优先判据（反向）",
	})
	require.NoError(t, runErr, "放行路径不得返回调用方错误")
	require.Equal(t, core.StatusFailed, result.Status)
	require.Contains(t, result.Reason, errLifecycleAdmitProbe.Error(),
		"到达快照阶段的探针错误即证明生命周期门禁已放行；卡在这里说明又被本地文件拦住了")

	// 索引行存在但不可用：失败关闭，不得回退文件（文件是 active）。
	fileRecord = setLifecycleState(t, lifecycle, agent, core.AgentActive)
	index.set(agent, core.LifecycleRecord{
		Agent: agent, State: core.AgentActive,
		Generation: fileRecord.Generation, Checksum: fileRecord.Checksum,
	}, false,
		"状态文件哈希与索引不符")
	_, runErr = svc.Run(ctx, core.RunRequest{
		Agent:     agent,
		Principal: core.LocalAdminPrincipal(),
		Channel:   "web",
		Message:   "索引不可用判据",
	})
	require.ErrorAs(t, runErr, &unavailable, "索引行不可用必须失败关闭")
	require.Equal(t, core.AgentUnavailableCorrupt, unavailable.Kind,
		"索引读不出可信状态按损坏处理，不得当成缺失去回退文件")

	// 索引读失败（数据库不可用）：同样失败关闭。
	index.set(agent, core.LifecycleRecord{Agent: agent, State: core.AgentActive}, true, "")
	index.setFailure(errors.New("database is down"))
	_, runErr = svc.Run(ctx, core.RunRequest{
		Agent:     agent,
		Principal: core.LocalAdminPrincipal(),
		Channel:   "web",
		Message:   "索引读失败判据",
	})
	require.ErrorAs(t, runErr, &unavailable, "索引读失败必须失败关闭，不得静默回退文件")
}

// TestScheduleGate_UsesIndex 守"调度门按索引判"：门与准入必须同口径，否则会出现
// "准入拒绝、调度照跑"或反过来的半截一致性。门每次触发重读，不缓存。
// The schedule gate follows the index every tick, in both directions.
func TestScheduleGate_UsesIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := "alpha"
	lifecycle := newLifecycleFile(t.TempDir())
	setLifecycleState(t, lifecycle, agent, core.AgentActive)
	uid, err := core.NewAgentUID()
	require.NoError(t, err)

	index := newLifecycleIndexFake()
	gate := lifecycleScheduleGateFor(lifecycle, index)
	require.NotNil(t, gate)

	index.set(agent, core.LifecycleRecord{AgentUID: uid, State: core.AgentDisabled, Generation: 9}, true, "")
	require.False(t, gate(ctx, agent),
		"索引说停用就是停用——本地文件还是 active 也不得放行")

	index.set(agent, core.LifecycleRecord{AgentUID: uid, State: core.AgentActive, Generation: 10}, true, "")
	require.True(t, gate(ctx, agent),
		"索引说启用就放行——本地文件停用是各机旧副本，不得反过来盖住共享判定")

	index.set(agent, core.LifecycleRecord{AgentUID: uid, State: core.AgentActive, Generation: 11}, false,
		"索引行校验失败")
	require.False(t, gate(ctx, agent), "索引行不可用必须失败关闭，不得回退文件")

	index.set(agent, core.LifecycleRecord{AgentUID: uid, State: core.AgentActive, Generation: 12}, true, "")
	index.setFailure(errors.New("database is down"))
	require.False(t, gate(ctx, agent), "索引读失败必须失败关闭，不得按本地文件放行")
}

// TestLifecycleIndex_NoRowFallsBackToFile 守"无索引行才回退本地状态文件"：索引里没有
// 该 Agent 不是故障（老部署 / 索引未建），判定必须与改造前逐字一致；缺失 / 损坏仍是
// 失败关闭。回退只在**没有行**时发生——有行但不可用不走这条路（上一条用例守）。
// A missing index row falls back to the state file exactly as before.
func TestLifecycleIndex_NoRowFallsBackToFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	lifecycle := newLifecycleFile(t.TempDir())
	active := setLifecycleState(t, lifecycle, "active-agent", core.AgentActive)
	setLifecycleState(t, lifecycle, "disabled-agent", core.AgentDisabled)

	index := newLifecycleIndexFake()
	index.drop("active-agent")
	index.drop("disabled-agent")
	gate := lifecycleScheduleGateFor(lifecycle, index)

	require.True(t, gate(ctx, "active-agent"), "无索引行时按状态文件：active 放行")
	require.False(t, gate(ctx, "disabled-agent"), "无索引行时按状态文件：disabled 拒绝")
	require.False(t, gate(ctx, "never-bootstrapped"), "没有状态文件仍是失败关闭")

	// 准入面同口径：端口在没有索引行时返回的就是状态文件那条记录（含代次与哈希），
	// 不得补造字段——`StartRun` 的生命周期期望取的就是这两项。
	port := newLifecycleIndexPort(lifecycle, index)
	rec, exists, err := port.Read(ctx, "active-agent")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, active.Generation, rec.Generation, "回退时取文件代次，不另算")
	require.Equal(t, active.Checksum, rec.Checksum, "回退时取文件哈希，不另算")
	require.Equal(t, core.AgentActive, rec.State)

	// 索引未接线时（老装配）端口行为与状态文件存储逐字相同。
	plain := newLifecycleIndexPort(lifecycle, nil)
	rec, exists, err = plain.Read(ctx, "disabled-agent")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentDisabled, rec.State)
}
