package core_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 Agent 生命周期领域类型的验收测试（颗粒度文档 §3.2：core/lifecycle_test.go，
// 14 例）：状态字面量、按 Agent 分桶的读写锁互斥与回收、端口形状与冲突哨兵。
// 状态文件的字节格式与校验和属 workspace/lifecycle_test.go，不在本文件重复。
// This file covers the lifecycle domain types: state literals, the per-agent gate's
// mutual exclusion and bucket reclamation, port shapes and the conflict sentinel.

// waitAcquired 在截止时间内等待"已取得锁"信号；到点返回 false（有界等待，不无限阻塞）。
// waitAcquired waits for the acquired signal within the deadline; expiry returns false.
func waitAcquired(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// TestAgentGate_ReadLocksShare：同一 Agent 的读锁可共享——第二个读锁不被第一个阻塞。
func TestAgentGate_ReadLocksShare(t *testing.T) {
	gate := core.NewAgentGate()

	release1 := gate.RLock("reporter")
	defer release1()

	acquired := make(chan struct{})
	go func() {
		release2 := gate.RLock("reporter")
		close(acquired)
		release2()
	}()

	require.True(t, waitAcquired(acquired, 3*time.Second), "读锁必须可共享")
}

// TestAgentGate_ReadLockBlocksWriter：读锁持有期间，同一 Agent 的写锁必须等待。
func TestAgentGate_ReadLockBlocksWriter(t *testing.T) {
	gate := core.NewAgentGate()

	releaseRead := gate.RLock("reporter")
	acquired := make(chan struct{})
	go func() {
		releaseWrite := gate.Lock("reporter")
		close(acquired)
		releaseWrite()
	}()

	require.False(t, waitAcquired(acquired, 200*time.Millisecond), "读锁未释放时写锁不得取得")
	releaseRead()
	require.True(t, waitAcquired(acquired, 3*time.Second), "读锁释放后写锁必须取得")
}

// TestAgentGate_WriterBlocksReaders：写锁持有期间，同一 Agent 的读锁必须等待。
func TestAgentGate_WriterBlocksReaders(t *testing.T) {
	gate := core.NewAgentGate()

	releaseWrite := gate.Lock("reporter")
	acquired := make(chan struct{})
	go func() {
		releaseRead := gate.RLock("reporter")
		close(acquired)
		releaseRead()
	}()

	require.False(t, waitAcquired(acquired, 200*time.Millisecond), "写锁未释放时读锁不得取得")
	releaseWrite()
	require.True(t, waitAcquired(acquired, 3*time.Second), "写锁释放后读锁必须取得")
}

// TestAgentGate_DifferentAgentsIndependent：不同 Agent 的写锁互不阻塞（分桶而非全局锁）。
func TestAgentGate_DifferentAgentsIndependent(t *testing.T) {
	gate := core.NewAgentGate()

	releaseA := gate.Lock("reporter")
	defer releaseA()

	acquired := make(chan struct{})
	go func() {
		releaseB := gate.Lock("digest")
		close(acquired)
		releaseB()
	}()

	require.True(t, waitAcquired(acquired, 3*time.Second), "不同 Agent 的写锁必须互不影响")
}

// TestAgentGate_ReleaseIsIdempotent：释放函数可重复调用（defer 与错误路径都可能调），
// 第二次调用不得 panic、不得破坏后续取锁。
func TestAgentGate_ReleaseIsIdempotent(t *testing.T) {
	gate := core.NewAgentGate()

	release := gate.RLock("reporter")
	release()
	require.NotPanics(t, release, "释放函数必须幂等")

	acquired := make(chan struct{})
	go func() {
		releaseWrite := gate.Lock("reporter")
		close(acquired)
		releaseWrite()
	}()
	require.True(t, waitAcquired(acquired, 3*time.Second), "重复释放后锁必须仍可用")
}

// TestAgentGate_GateReusableAfterRelease：桶在最后一个持有者释放后回收，再次取锁仍可用。
func TestAgentGate_GateReusableAfterRelease(t *testing.T) {
	gate := core.NewAgentGate()

	for i := 0; i < 3; i++ {
		release := gate.Lock("reporter")
		release()
	}

	acquired := make(chan struct{})
	go func() {
		releaseRead := gate.RLock("reporter")
		close(acquired)
		releaseRead()
	}()
	require.True(t, waitAcquired(acquired, 3*time.Second), "回收后的桶必须可重新创建")
}

// TestAgentGate_ConcurrentReadWrite：多读者 + 多写者并发下不 panic、不死锁，
// 写者在最后一个读者释放后取得锁（`011-race` 的具名用例之一）。
func TestAgentGate_ConcurrentReadWrite(t *testing.T) {
	gate := core.NewAgentGate()

	var (
		wg      sync.WaitGroup
		counter int
	)

	// 读写都只经闸门同步：读者持 RLock 读、写者持 Lock 写——这正是运行准入与
	// 启停迁移的访问模式，`-race` 下不得报竞态。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				release := gate.RLock("reporter")
				_ = counter // 读者在闸门内读；写者持写锁改——跨 goroutine 的同步由 RWMutex 建立
				release()
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				release := gate.Lock("reporter")
				counter++
				release()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	require.True(t, waitAcquired(done, 10*time.Second), "并发读写必须在有界时间内完成")
	require.Equal(t, 100, counter)
}

// TestAgentGate_WaitingWriterGetsTurn：排队的写者在读者释放后取得锁（不发生饿死）。
func TestAgentGate_WaitingWriterGetsTurn(t *testing.T) {
	gate := core.NewAgentGate()

	releaseRead := gate.RLock("reporter")
	acquired := make(chan struct{})
	go func() {
		releaseWrite := gate.Lock("reporter")
		close(acquired)
		releaseWrite()
	}()

	require.False(t, waitAcquired(acquired, 100*time.Millisecond))
	releaseRead()
	require.True(t, waitAcquired(acquired, 3*time.Second), "排队写者必须在读者释放后取得锁")
}

// TestAgentGate_ManyAgentsNoInterference：多 Agent 并发取写锁互不干扰。
func TestAgentGate_ManyAgentsNoInterference(t *testing.T) {
	gate := core.NewAgentGate()

	const agents = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < agents; i++ {
		name := fmt.Sprintf("agent-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release := gate.Lock(name)
			release()
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	require.True(t, waitAcquired(done, 5*time.Second), "不同 Agent 的锁必须并行取得")
}

// TestLifecycleRecord_StateLiterals：三个状态字面量逐字取自契约 CHECK 子句。
func TestLifecycleRecord_StateLiterals(t *testing.T) {
	require.Equal(t, "active", string(core.AgentActive))
	require.Equal(t, "disabled", string(core.AgentDisabled))
	require.Equal(t, "archived", string(core.AgentArchived))
}

// TestLifecycleRecord_StatesDistinct：三态互不相同（准入判定依赖"只有 active 放行"）。
func TestLifecycleRecord_StatesDistinct(t *testing.T) {
	states := map[core.AgentState]struct{}{
		core.AgentActive:   {},
		core.AgentDisabled: {},
		core.AgentArchived: {},
	}
	require.Len(t, states, 3)
	for state := range states {
		require.NotEmpty(t, state)
	}
}

// stubLifecyclePort 以编译期断言冻结 LifecyclePort 的方法集；方法体只记录调用。
// stubLifecyclePort freezes the LifecyclePort method set at compile time.
type stubLifecyclePort struct {
	calls []string
}

func (s *stubLifecyclePort) Read(context.Context, string) (core.LifecycleRecord, bool, error) {
	s.calls = append(s.calls, "read")
	return core.LifecycleRecord{}, false, nil
}

func (s *stubLifecyclePort) Transition(_ context.Context, agent string, expectedState core.AgentState, expectedGeneration int64, target core.AgentState, operationID string) (core.LifecycleRecord, error) {
	s.calls = append(s.calls, "transition")
	return core.LifecycleRecord{
		Agent:       agent,
		State:       target,
		Generation:  expectedGeneration + 1,
		OperationID: operationID,
	}, nil
}

func (s *stubLifecyclePort) Bootstrap(_ context.Context, agent, operationID string) (core.LifecycleRecord, error) {
	s.calls = append(s.calls, "bootstrap")
	return core.LifecycleRecord{Agent: agent, State: core.AgentDisabled, Generation: 1, OperationID: operationID}, nil
}

func (s *stubLifecyclePort) BootstrapIdentity(_ context.Context, agent string, agentUID core.AgentUID, operationID string) (core.LifecycleRecord, error) {
	s.calls = append(s.calls, "bootstrap_identity")
	return core.LifecycleRecord{
		Agent: agent, AgentUID: agentUID, State: core.AgentDisabled, Generation: 1, OperationID: operationID,
	}, nil
}

func (s *stubLifecyclePort) Discard(_ context.Context, _, _ string) error {
	s.calls = append(s.calls, "discard")
	return nil
}

var _ core.LifecyclePort = (*stubLifecyclePort)(nil)

// TestLifecycleRecord_PortInterfaceShape：端口方法集与迁移前置条件参数逐字冻结；
// Transition 把期望状态与代数原样交给实现（乐观锁的两半都不得被调用方吞掉）。
func TestLifecycleRecord_PortInterfaceShape(t *testing.T) {
	var port core.LifecyclePort = &stubLifecyclePort{}

	rec, err := port.Transition(context.Background(), "reporter", core.AgentDisabled, 7, core.AgentActive, "op-1-abc123")
	require.NoError(t, err)
	require.Equal(t, core.AgentActive, rec.State)
	require.Equal(t, int64(8), rec.Generation)
	require.Equal(t, "op-1-abc123", rec.OperationID)
}

// stubLifecycleIndex 以编译期断言冻结 LifecycleIndex 的方法集。
// stubLifecycleIndex freezes the LifecycleIndex method set at compile time.
type stubLifecycleIndex struct {
	lastValid  bool
	lastErrMsg string
}

func (s *stubLifecycleIndex) PutLifecycleIndex(_ context.Context, _ core.LifecycleRecord, valid bool, errMsg string) error {
	s.lastValid = valid
	s.lastErrMsg = errMsg
	return nil
}

var _ core.LifecycleIndex = (*stubLifecycleIndex)(nil)

// TestLifecycleRecord_IndexInterfaceShape：索引写端口接受"无效 + 原因"形态——
// 状态文件缺失 / 损坏时索引据此写 NULL，而不是伪造一条 active。
func TestLifecycleRecord_IndexInterfaceShape(t *testing.T) {
	var index core.LifecycleIndex = &stubLifecycleIndex{}
	require.NoError(t, index.PutLifecycleIndex(context.Background(), core.LifecycleRecord{}, false, "checksum mismatch"))
	stub := index.(*stubLifecycleIndex)
	require.False(t, stub.lastValid)
	require.Equal(t, "checksum mismatch", stub.lastErrMsg)
}

// TestLifecycleRecord_ConflictSentinelIdentity：冲突哨兵经 %w 包装后仍可 errors.Is 判定
// （调用方**不得**比对错误文本）。
func TestLifecycleRecord_ConflictSentinelIdentity(t *testing.T) {
	wrapped := fmt.Errorf("transition reporter: %w", core.ErrLifecycleConflict)
	require.ErrorIs(t, wrapped, core.ErrLifecycleConflict)
	require.False(t, errors.Is(wrapped, context.Canceled))
}

// —— 准入重读（约束 15 / 17）——
// Admission re-read (constraints 15 and 17).

// TestAgentService_Lifecycle_AdmissionStateTable 是约束 15 的「四态表驱动」复核：同一张表
// 钉住放行条件与四类拒绝的固定分类——active 放行；disabled / archived / 缺失 / 损坏一律
// *AgentUnavailableError，且拒绝发生在任何落痕（快照 / 账本 / 模型调用）之前；缺失与损坏
// 同时留 ERROR 日志。每次准入都必须现场读一次状态文件（不得凭目录存在推断）。
// 逆改点：把状态缓存为 active，或把缺失 / 损坏读成 active → 对应行立即变红。
func TestAgentService_Lifecycle_AdmissionStateTable(t *testing.T) {
	const (
		reasonDisabled = "lifecycle state disabled; enable the agent first"
		reasonArchived = "lifecycle state archived"
		reasonMissing  = "lifecycle state file missing; restore it before running"
		reasonCorrupt  = "lifecycle state file corrupt; restore it from a trusted copy"
	)

	cases := []struct {
		name      string
		lifecycle *fakeLifecycle
		admitted  bool
		state     core.AgentState
		reason    string
		logMark   string
	}{
		{name: "active 放行", lifecycle: newFakeLifecycle(core.AgentActive), admitted: true},
		{name: "disabled 拒绝", lifecycle: newFakeLifecycle(core.AgentDisabled), state: core.AgentDisabled, reason: reasonDisabled},
		{name: "archived 拒绝", lifecycle: newFakeLifecycle(core.AgentArchived), state: core.AgentArchived, reason: reasonArchived},
		{name: "缺失拒绝", lifecycle: &fakeLifecycle{exists: false, readErr: fs.ErrNotExist}, reason: reasonMissing, logMark: "生命周期状态文件缺失"},
		{name: "损坏拒绝", lifecycle: &fakeLifecycle{exists: false, readErr: errors.New("lifecycle checksum mismatch")}, reason: reasonCorrupt, logMark: "生命周期状态文件损坏"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLoopHarness(t)
			h.provider.onCall(0, textResponse("好"))
			svc, store := h.assembleWithLifecycle(tc.lifecycle, core.NewAgentGate())

			res, err := svc.Run(ctx, h.req("在吗"))
			require.Equal(t, 1, tc.lifecycle.readCount(), "准入必须现场读一次状态文件")

			if tc.admitted {
				require.NoError(t, err)
				require.Equal(t, core.StatusCompleted, res.Status)
				require.Equal(t, 1, store.runsOf())
				require.Equal(t, 1, h.provider.callCount())
				require.NotContains(t, h.logs.String(), "level=ERROR", "放行路径不得留 ERROR")
				return
			}

			var unavailable *core.AgentUnavailableError
			require.ErrorAs(t, err, &unavailable)
			require.Equal(t, tc.state, unavailable.State)
			require.Equal(t, tc.reason, unavailable.Reason, "拒绝分类文案必须逐字固定")
			require.Equal(t, core.RunResult{}, res, "拒绝不产生运行结果")
			require.Zero(t, store.snapshotsOf(), "拒绝必须发生在快照落库之前")
			require.Zero(t, store.runsOf(), "拒绝必须发生在账本预写之前")
			require.Zero(t, h.provider.callCount(), "拒绝必须发生在模型调用之前")
			if tc.logMark != "" {
				// 约定：停用 / 归档是正常管理态，只拒绝不记 ERROR；缺失 / 损坏是故障，
				// 审计与日志双轨（约束 15）。
				require.Contains(t, h.logs.String(), "level=ERROR", "缺失 / 损坏必须同时记 ERROR")
				require.Contains(t, h.logs.String(), tc.logMark)
			} else {
				require.NotContains(t, h.logs.String(), "level=ERROR", "常规停用 / 归档不得记 ERROR")
			}
		})
	}
}

// TestAgentService_Lifecycle_AdmissionThreeEntryPoints 是约束 15 的第二条复核：HTTP、CLI
// 与调度器三种请求形状（渠道字 / 会话派生 / 触发源不同）共用同一个 AgentService.Run 的
// 准入判定——停用时三入口的拒绝分类逐字一致；重新启用后三入口同时恢复；每次运行都现场
// 重读状态文件（读次数逐次递增、翻转立即生效），证明进程内没有第二份生命周期缓存。
// 会话门禁与触发源映射不在这里的判据面内（分别由 service / scheduler 测试覆盖）。
// 逆改点：任一入口旁路 Run（自建生命周期缓存或自带状态判断）→ 停用态却完成运行，
// 或读次数不再随尝试递增，断言红。
func TestAgentService_Lifecycle_AdmissionThreeEntryPoints(t *testing.T) {
	h := newLoopHarness(t)
	lifecycle := newFakeLifecycle(core.AgentDisabled)
	svc, store := h.assembleWithLifecycle(lifecycle, core.NewAgentGate())

	points := []struct {
		name string
		req  core.RunRequest
	}{
		{
			name: "HTTP",
			req: core.RunRequest{
				Agent: loopAgentName, SessionID: "web-session", Message: "在吗",
				Channel: "web", UserID: "tester", Budget: h.budget,
			},
		},
		{
			name: "CLI",
			req:  h.req("在吗"), // Channel = "cli"（harness 的缺省渠道）
		},
		{
			name: "调度器",
			req: core.RunRequest{
				Agent: loopAgentName, SessionID: core.DeriveSessionID("scheduler", "scheduler", loopAgentName),
				Message: "在吗", Channel: "scheduler", UserID: "scheduler",
				Trigger: core.TriggerCron, Budget: h.budget,
			},
		},
	}

	// 停用态：三入口都经同一门禁拒绝，分类逐字一致，且读次数逐次 +1（无缓存）。
	for i, point := range points {
		res, err := svc.Run(ctx, point.req)
		var unavailable *core.AgentUnavailableError
		require.ErrorAs(t, err, &unavailable, "%s 入口必须经同一准入判定", point.name)
		require.Equal(t, core.AgentDisabled, unavailable.State)
		require.Equal(t, "lifecycle state disabled; enable the agent first", unavailable.Reason,
			"%s 入口的拒绝分类必须与其他入口逐字一致", point.name)
		require.Equal(t, core.RunResult{}, res)
		require.Equal(t, i+1, lifecycle.readCount(), "%s 入口每次准入都要现场重读", point.name)
		require.Zero(t, store.runsOf(), "%s 入口的拒绝发生在任何落痕之前", point.name)
	}
	require.Zero(t, h.provider.callCount())

	// 重新启用：状态翻转对三入口立即生效（再次证明没有第二份缓存）。
	lifecycle.setState(core.AgentActive)
	results := make([]core.RunResult, len(points))
	for i, point := range points {
		h.provider.onCall(i, textResponse("好"))
		res, err := svc.Run(ctx, point.req)
		require.NoError(t, err, "%s 入口启用后必须放行", point.name)
		require.Equal(t, core.StatusCompleted, res.Status)
		require.Equal(t, len(points)+i+1, lifecycle.readCount(), "%s 入口放行时同样现场重读", point.name)
		results[i] = res
	}
	require.Equal(t, len(points), store.runsOf(), "启用后三入口各落一行账本")
	require.Equal(t, len(points), h.provider.callCount())
	for i, point := range points {
		require.Equal(t, point.req.Channel, store.runRow(t, results[i].RunID).Channel,
			"%s 入口的渠道字必须原样落账", point.name)
	}
}
