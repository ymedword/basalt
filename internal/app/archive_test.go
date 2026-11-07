//go:build integration

package app

// 本文件是 US-24 T025 的归档准入屏障验收面（模块卡 §4.4.6 / FR-24-23 / FR-24-41 / C-27）。
//
// 与 agent_admin_test.go 的归档用例分工明确：那批用记录型替身判"等待 / 回滚"的**编排**
// （谁先谁后、回滚哪几步），本文件用真实 `RunStore` / `AgentService` / PostgreSQL 行锁判
// **跨 Worker 的持久事实**——屏障真的落在共享索引上、复查真的取同一行锁、拒绝真的没有
// 留下 Run 行。替身判不出这三件事：它们恰好都是"另一台机器看到的同一个库"才成立的性质。
//
//	B1 `TestArchive_BarrierRejectsNewRunsBeforeWait`——先等再改状态 → 等待窗口内新 Run 仍被准入
//	B2 `TestArchive_AdmissionLockOrder`——屏障与 StartRun 复查不是同一行锁 → 两种获锁顺序之一崩
//	B3 `TestArchive_RefusesWhenRemoteLeaseLive`——只查本地忙碌 → 远端仍在跑就宣布归档完成
//	B4 `TestArchive_CLIPathRejectsRemoteLiveRun`——CLI 装配把取消器并进 all-or-nothing → 无本地忙碌即搬目录
//	B5 `TestArchive_CLIPathCompletesWithoutLiveRuns`——CLI 装配的等待段不因注册面缺席而误伤成功路径
//
// 三条用例**都不并行**：B2 用 `pg_stat_activity` 观测真实锁等待，与同包并行用例抢同一
// 组连接会让读数失去意义；B1 / B3 与它们共用同一套夹具语义，保持串行便于逐条复核。
//
// 夹具的一条前提值得写明：`StartRun` 的发布 / 生命周期复查只在清单携带 bundle 期望时
// 启用（`FormatVersion != 0 && AssetBundleHash != ""`），因此本文件必须落下**完整的**
// 四份持久事实——`agent_lifecycle` 索引行、`agent_bundle` 发布指针、Provider 发布版本、
// 生命周期期望值。少任何一份，屏障用例就会因为"清单没带期望值"而空过。
//
// The three cases above only hold when the barrier lives on the shared row; recording
// doubles cannot see that, so this file drives the real stores and real row locks.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// archiveDBEnv 在 `archiveEnv` 之上把 Run 取消器换成**真实装配**（`runCancellerAdapter`
// + `RunStore` + `AgentService`），并补齐准入复查需要的持久事实。
// 与生产 `wire.go` 的差别只有一处：`AgentService` 只接了 `Runs` / `Logger`，够 `Cancel`
// 走"运行不在本进程 → 只写取消标志"那一支；本文件不跑 ReAct 循环。
//
// archiveDBEnv swaps the recording canceller for the production adapter over real stores
// and seeds every fact admission re-checks; the AgentService is wired only as far as
// Cancel needs (remote flag), the loop is out of scope here.
type archiveDBEnv struct {
	*archiveEnv
	name       string
	runs       *postgres.RunStore
	manifest   core.RunResourceManifest
	generation int64
	stateHash  string
}

// newArchiveDBEnv 建 serve 形态夹具：注册面齐全、取消器接 `AgentService`。
// busyWait 是归档等待窗口（用例按需给几百毫秒到两秒）。
// newArchiveDBEnv builds the serve-shaped fixture; busyWait is the archive wait window.
func newArchiveDBEnv(t *testing.T, name string, busyWait time.Duration) *archiveDBEnv {
	t.Helper()
	return newArchiveDBEnvWithAssembly(t, name, busyWait, true)
}

// newArchiveCLIEnv 建 CLI 形态夹具：注册面为 nil（CLI 持实例锁、不跑调度），Run 取消器
// 是**真实适配器但不接 `AgentService`**（进程里没有 ReAct 循环）——这正是
// `openCLIAdmin` 的装配形态。残量快照与跨 Worker 取消仍读同一个共享账本。
// newArchiveCLIEnv mirrors the CLI assembly: no registry/scheduler, a ledger-only
// canceller, while the residual facts still come from the shared database.
func newArchiveCLIEnv(t *testing.T, name string, busyWait time.Duration) *archiveDBEnv {
	t.Helper()
	return newArchiveDBEnvWithAssembly(t, name, busyWait, false)
}

// newArchiveDBEnvWithAssembly 是两种装配的共同夹具：live 决定注册面与取消器的形状。
// newArchiveDBEnvWithAssembly builds the shared fixture; live selects the assembly shape.
func newArchiveDBEnvWithAssembly(t *testing.T, name string, busyWait time.Duration, live bool) *archiveDBEnv {
	t.Helper()
	base := newAgentEnv(t, name)
	ctx := context.Background()

	rec, exists, err := base.lifecycle.Read(ctx, name)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentActive, rec.State, "夹具 Agent 必须处于 active")
	require.NotEmpty(t, rec.AgentUID, "夹具 Agent 必须带稳定身份（agent_bundle 的键是 AgentUID）")

	// 共享索引行：代次与状态文件哈希取自状态文件，准入期望值随后按它比对。
	// `agent_lifecycle.operation_id` 是指向 `management_operations` 的外键，而夹具状态文件
	// 记的是 `newAgentEnv` 用的种子标识 `op-seed`——先把这一行补上，否则索引写不进去，
	// 归档超时的 `RestoreLifecycleBarrier` 也会因同一外键写不回去（那会把"回滚断言"变成
	// 假红）。
	seedLifecycleOperation(t, base.agentAdminEnv, "op-seed")
	require.NoError(t, base.versions.PutLifecycleIndex(ctx, rec, true, ""))

	bundleHash := sha256Hex(t, []byte("bundle:"+name))
	seedPublishedBundle(t, base.agentAdminEnv, string(rec.AgentUID), bundleHash)
	model := base.seedModel(t, "alpha")

	resolved := core.ResolvedResources{
		Provider: core.ResourceVersionRef{
			Name:        model.ResourceID,
			ResourceUID: model.ResourceUID,
			VersionID:   model.ID,
			VersionNo:   model.VersionNo,
			ContentHash: model.ContentHash,
		},
		ProviderModel: "alpha-model",
		ProviderCredential: core.CredentialPin{
			Mode: core.CredentialModeEnv,
			// 目标绑定记的是规范化目标地址的哈希，不是凭据材料：env 模式同样必填。
			TargetBinding: strings.Repeat("a", 64),
		},
		Notify:      core.NotifyManifest{Mode: core.NotifyModeAllEligible, Channels: []core.NotifyChannelRef{}},
		AssetBundle: core.AssetBundlePin{Hash: bundleHash},
	}
	manifest, err := core.BuildRunResourceManifest(rec.AgentUID, core.AgentActive, resolved)
	require.NoError(t, err)

	runs := postgres.NewRunStoreWithLeaseTTL(base.db, 5*time.Minute)
	if live {
		service := core.NewAgentService(core.ServiceOptions{Runs: runs, Logger: base.deps.Logger})
		base.deps.RunCanceller = runCancellerAdapter{runs: runs, service: service}
	} else {
		// CLI：无注册面、无本地循环；取消走共享标志（FR-24-22 的远端分支）。
		base.deps.Registry = nil
		base.deps.Scheduler = nil
		base.deps.RunCanceller = runCancellerAdapter{runs: runs}
	}
	base.deps.BusyWait = busyWait
	base.build()

	return &archiveDBEnv{
		archiveEnv: base,
		name:       name,
		runs:       runs,
		manifest:   manifest,
		generation: rec.Generation,
		stateHash:  rec.Checksum,
	}
}

// seedLifecycleOperation 补一行管理操作记录，让 `agent_lifecycle.operation_id` 的外键成立。
// 夹具的状态文件由 `newAgentEnv` 用固定种子标识 `op-seed` 建立，却从未建过对应的操作行；
// 索引写入与屏障恢复都会因此撞上外键。这里按迁移 0001 的列与 CHECK 取值建行，`ON CONFLICT`
// 保证重复调用幂等。
// seedLifecycleOperation materializes the operation row the fixture's state file points at.
func seedLifecycleOperation(t *testing.T, env *agentAdminEnv, operationID string) {
	t.Helper()
	_, err := env.db.ExecContext(context.Background(), `
INSERT INTO management_operations
  (operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
   status, created_at)
VALUES ($1, 'agent.lifecycle', 'agent', 'system', 'migration', 'migration', 'test',
        'success', $2)
ON CONFLICT (operation_id) DO NOTHING`, operationID, baselineClock().UnixMilli())
	require.NoError(t, err, "补种管理操作行")
}

// seedPublishedBundle 用原始 SQL 落一条 `agent_bundle` 发布记录（版本行 + 指针行）。
//
// 为什么不走 `ResourceStore.EnsureVersion`：`validateResourceKind` 与
// `normalizeResourceVersion` 的 `agent_bundle` / `ag_` 分支是 T028 的交付物，此刻仓储
// API 还只认三类。迁移 0012 已把数据库侧的字面量闭集放宽到四类，因此这里直接建行——
// 与 `migration_0012_test.go` 同一手法。T028 / T029 接上真实管线后本函数退出。
//
// `resource_id` 与 `resource_uid` 都取 AgentUID：R4 定的键是"AgentUID → bundle hash"，
// 而 `recheckAgentBundle` 正是按 `resource_id = <agent_uid>` 查这条指针。
//
// seedPublishedBundle writes the agent_bundle version+pointer by raw SQL because the
// store API only knows three kinds until T028; migration 0012 already widened the
// database CHECK. The pointer key is the AgentUID, matching recheckAgentBundle.
func seedPublishedBundle(t *testing.T, env *agentAdminEnv, agentUID, bundleHash string) {
	t.Helper()
	ctx := context.Background()
	opID := env.seedOperation(t)

	var versionID int64
	require.NoError(t, env.db.QueryRowContext(ctx, `
INSERT INTO resource_versions
  (resource_kind, resource_uid, resource_id, version_no, content_hash,
   manifest_json, storage_path, created_at, operation_id)
VALUES ('agent_bundle', $1, $1, 1, $2, '{"files":[]}', $3, $4, $5)
RETURNING id`,
		agentUID, bundleHash, "bundles/"+agentUID+"/"+bundleHash,
		baselineClock().UnixMilli(), opID).Scan(&versionID),
		"写入 agent_bundle 版本行")

	_, err := env.db.ExecContext(ctx, `
INSERT INTO resource_pointers
  (resource_kind, resource_id, resource_uid, published_version_id, updated_at, operation_id)
VALUES ('agent_bundle', $1, $1, $2, $3, $4)`,
		agentUID, versionID, baselineClock().UnixMilli(), opID)
	require.NoError(t, err, "写入 agent_bundle 发布指针")
}

// startRun 以真实准入事务创建一条 Run：会话标识决定 SESSION_BUSY 归属，workerID 决定
// 租约属主（远端用例靠它区分"不在本进程的 Run"）。
// startRun creates one run through the real admission transaction.
func (e *archiveDBEnv) startRun(ctx context.Context, runID, sessionID, workerID string,
) (core.LeaseToken, error) {
	return e.runs.StartRun(ctx, core.RunRecord{
		RunID:     runID,
		SessionID: sessionID,
		Agent:     e.name,
		Channel:   "web",
		UserID:    "u1",
		Principal: core.LocalAdminCLIPrincipal(),
		StartedAt: time.Now().UTC(),
		Lifecycle: core.LifecycleExpectation{
			Generation:    e.generation,
			StateFileHash: e.stateHash,
		},
	}, e.manifest,
		core.WorkerIdentity{InstanceID: workerID, Name: "worker-" + workerID},
		core.CallerAdmission{}, runID)
}

// indexRow 读共享生命周期索引行的 `(state, generation)`——屏障是否落下、是否按
// `prior_state` 回滚，只认这一行（内存里的状态不可信，另一台机器看不到）。
// indexRow reads the shared lifecycle row; only the row evidences the barrier.
func (e *archiveDBEnv) indexRow(t *testing.T) (string, int64) {
	t.Helper()
	var (
		state      string
		generation int64
	)
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		`SELECT state, generation FROM agent_lifecycle WHERE agent_name = $1`, e.name).
		Scan(&state, &generation))
	return state, generation
}

// countRunRows 数指定 run_id 的账本行（被拒的准入必须零行）。
// countRunRows counts ledger rows for one run id; a refused admission leaves none.
func countRunRows(t *testing.T, env *archiveDBEnv, runID string) int {
	t.Helper()
	var n int
	require.NoError(t, env.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM runs WHERE run_id = $1`, runID).Scan(&n))
	return n
}

// cancelRequested 读 `runs.cancel_requested`（跨 Worker 取消的唯一持久信号）。
// cancelRequested reads the cross-worker cancel flag.
func cancelRequested(t *testing.T, env *archiveDBEnv, runID string) bool {
	t.Helper()
	var flagged bool
	require.NoError(t, env.db.QueryRowContext(context.Background(),
		`SELECT cancel_requested FROM runs WHERE run_id = $1`, runID).Scan(&flagged))
	return flagged
}

// waitLifecycleState 轮询共享索引直到出现目标状态：归档的屏障是**提交后**才可见的，
// 用固定 Sleep 猜时机会在慢机器上假红。
// waitLifecycleState polls the shared row until the target state is visible.
func waitLifecycleState(t *testing.T, env *archiveDBEnv, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, _ := env.indexRow(t)
		if state == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, _ := env.indexRow(t)
	t.Fatalf("%s 内共享索引未进入 %q（实际 %q）", timeout, want, state)
}

// waitArchiveBarrierBlocked 等归档的屏障 UPDATE 真的停在 `agent_lifecycle` 的行锁上。
//
// 判据取 `pg_stat_activity`：某个后端 `wait_event_type = 'Lock'` 且当前语句是带
// `'disabled'` 字面量的 `agent_lifecycle` 更新——全仓只有 `SetLifecycleBarrier` 是这种
// 语句（`PutLifecycleIndex` 用参数绑定，语句文本里没有这个字面量），因此不会把索引重建
// 误认成屏障。等到它之后再去读索引，读到的"仍是 active"才是"另一个事务还没提交"的证据，
// 而不是"归档还没来得及跑"。
//
// waitArchiveBarrierBlocked waits until the barrier UPDATE is genuinely row-lock-waiting,
// so the subsequent "still active" reading proves an uncommitted peer rather than a
// goroutine that has not started yet.
func waitArchiveBarrierBlocked(t *testing.T, env *archiveDBEnv, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var waiting int
		require.NoError(t, env.db.QueryRowContext(context.Background(), `
SELECT count(*)
  FROM pg_stat_activity
 WHERE datname = current_database()
   AND pid <> pg_backend_pid()
   AND wait_event_type = 'Lock'
   AND query LIKE '%agent_lifecycle%'
   AND query LIKE '%''disabled''%'`).Scan(&waiting))
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s 内未观察到归档屏障在 agent_lifecycle 行锁上等待", timeout)
}

// TestArchive_BarrierRejectsNewRunsBeforeWait 逐字落地模块卡 §4.4.6 / FR-24-41 的第一条：
// 归档**先**把共享索引置 `disabled` 再发取消与等待；等待窗口内的新 Run 一律被拒且零占用；
// 窗口到期按 `prior_state` 回滚，之后重新放行。
//
// 逆改点（红态判据）：把屏障挪到等待之后（"先等再改状态"）→ 第二步的 StartRun 成功返回、
// 并留下 `run-new-1` 的账本行；把 `recheckAgentLifecycle` 的 `state != active` 判定删掉 →
// 同上。两条都会让 `require.ErrorIs(..., ErrAgentLifecycleMismatch)` 逐字变红。
func TestArchive_BarrierRejectsNewRunsBeforeWait(t *testing.T) {
	ctx := context.Background()
	env := newArchiveDBEnv(t, "alpha", 2*time.Second)

	// 一条真实在途 Run：取消只写标志不改状态，因此归档必然停在等待窗口里。
	_, err := env.startRun(ctx, "run-live-1", "sess-live-1", "worker-remote")
	require.NoError(t, err, "夹具前提：完整清单必须可准入")

	archiveDone := make(chan error, 1)
	go func() {
		_, archiveErr := env.admin.Archive(ctx, consoleActor(), env.name)
		archiveDone <- archiveErr
	}()

	waitLifecycleState(t, env, string(core.AgentDisabled), 5*time.Second)

	// 屏障已提交、等待窗口仍在：新 Run 必须被拒，且不得留下任何账本行（零占用）。
	_, err = env.startRun(ctx, "run-new-1", "sess-new-1", "worker-other")
	require.ErrorIs(t, err, core.ErrAgentLifecycleMismatch,
		"等待窗口内新 Run 必须被准入屏障拒绝（先等再改状态的写法会放行）")
	require.Equal(t, 0, countRunRows(t, env, "run-new-1"),
		"被屏障拒绝的 Run 不得留下账本行")

	err = <-archiveDone
	var busy *core.AgentBusyError
	require.ErrorAs(t, err, &busy, "等待窗口到期必须 409 AGENT_BUSY")
	require.Contains(t, busy.RunIDs, "run-live-1", "409 必须点名未退出的 Run")
	require.Contains(t, busy.Unmet, core.ArchiveResidualRunningRun,
		"409 必须点名未达成的判据")

	// 超时按 prior_state 回滚：索引回 active、代次不动（屏障不是迁移）。
	state, generation := env.indexRow(t)
	require.Equal(t, string(core.AgentActive), state, "超时必须按 prior_state 回滚屏障")
	require.EqualValues(t, env.generation, generation, "屏障不是迁移，代次不得变化")

	_, err = env.startRun(ctx, "run-after-1", "sess-after-1", "worker-other")
	require.NoError(t, err, "屏障回滚后准入必须重新放行")
}

// TestArchive_AdmissionLockOrder 逐字落地模块卡 §4.4.6 / FR-24-41 的第二种判据：
// **分别**验证两种真实可发生的获锁顺序。
//
// 不写"StartRun 已持锁时归档抢先提交"——行锁互斥下它不可能发生；写了就是一条永远为真
// 的空断言（模块卡 §7 的错误方案自检表第 C 条）。
//
// The two orders below are the only ones a row lock permits; the impossible third order
// is deliberately not asserted.
func TestArchive_AdmissionLockOrder(t *testing.T) {
	t.Run("start_run_first", func(t *testing.T) {
		ctx := context.Background()
		env := newArchiveDBEnv(t, "alpha", 1500*time.Millisecond)

		// 暂停点装在 StartRun 事务内、生命周期 `FOR UPDATE` 之后：此刻行锁已在
		// StartRun 手里。只在集成构建可见（生产构建的同名方法是无操作）。
		reached, release := postgres.ArmStartRunLockHold(t, env.db)
		t.Cleanup(release)

		startDone := make(chan error, 1)
		go func() {
			_, startErr := env.startRun(ctx, "run-lock-1", "sess-lock-1", "worker-remote")
			startDone <- startErr
		}()
		select {
		case <-reached:
		case <-time.After(5 * time.Second):
			t.Fatal("5s 内 StartRun 未到达准入暂停点")
		}

		archiveDone := make(chan error, 1)
		go func() {
			_, archiveErr := env.admin.Archive(ctx, consoleActor(), env.name)
			archiveDone <- archiveErr
		}()

		// 归档的屏障 UPDATE 只能等在这一行上：它没提交之前索引仍是 active。
		waitArchiveBarrierBlocked(t, env, 5*time.Second)
		state, generation := env.indexRow(t)
		require.Equal(t, string(core.AgentActive), state,
			"StartRun 未提交时归档屏障不得提交：两者必须是同一行 FOR UPDATE 互斥")
		require.EqualValues(t, env.generation, generation)
		require.Equal(t, 0, countRunRows(t, env, "run-lock-1"),
			"StartRun 尚未提交，新的 Run 行不该可见")

		release()
		require.NoError(t, <-startDone, "暂停点放行后 StartRun 必须提交成功")
		require.Equal(t, 1, countRunRows(t, env, "run-lock-1"),
			"StartRun 提交后账本行必须可见")

		err := <-archiveDone
		var busy *core.AgentBusyError
		require.ErrorAs(t, err, &busy,
			"归档必须在 StartRun 提交后读到新行再决定（新 Run 已准入 → 转等待收尾）")
		require.Contains(t, busy.RunIDs, "run-lock-1",
			"归档必须把刚提交的 Run 计入在途集合")
		require.Contains(t, busy.Unmet, core.ArchiveResidualRunningRun)
	})

	t.Run("barrier_first", func(t *testing.T) {
		ctx := context.Background()
		env := newArchiveDBEnv(t, "alpha", 1500*time.Millisecond)

		// 先有一条在途 Run，归档才会停在等待窗口里而不是直接收尾。
		_, err := env.startRun(ctx, "run-live-2", "sess-live-2", "worker-remote")
		require.NoError(t, err)

		archiveDone := make(chan error, 1)
		go func() {
			_, archiveErr := env.admin.Archive(ctx, consoleActor(), env.name)
			archiveDone <- archiveErr
		}()

		// 屏障先提交：此后 StartRun 的 `FOR UPDATE` 复查读到的是 disabled。
		waitLifecycleState(t, env, string(core.AgentDisabled), 5*time.Second)

		_, err = env.startRun(ctx, "run-lock-2", "sess-lock-2", "worker-other")
		require.ErrorIs(t, err, core.ErrAgentLifecycleMismatch,
			"屏障先行提交后，StartRun 的 FOR UPDATE 复查必须读到 disabled 并拒绝")
		require.Equal(t, 0, countRunRows(t, env, "run-lock-2"),
			"被复查拒绝的 Run 不得留下账本行")

		err = <-archiveDone
		var busy *core.AgentBusyError
		require.ErrorAs(t, err, &busy)
		require.Contains(t, busy.RunIDs, "run-live-2")

		state, generation := env.indexRow(t)
		require.Equal(t, string(core.AgentActive), state, "超时后屏障必须按 prior_state 回滚")
		require.EqualValues(t, env.generation, generation)
	})
}

// TestArchive_RefusesWhenRemoteLeaseLive 逐字落地模块卡 §4.4.6 / FR-24-23（C-27 的
// 第二个失败模式）：归档必须在**所有** Worker 上拒绝在途 Run，只查本地忙碌就移动目录
// 会把另一台机器正在跑的工作目录搬走。
//
// 夹具里的 Run 由另一个 Worker 身份持有租约、本进程内存里没有它——这正是生产里"远端
// 在跑"的形态。逆改点：把 `Residuals` 换成"本地在跑集合"（记录型替身）→ 本用例的
// 409 变成成功归档、目录被搬走、`Archived` 出现一条记录。
func TestArchive_RefusesWhenRemoteLeaseLive(t *testing.T) {
	ctx := context.Background()
	env := newArchiveDBEnv(t, "alpha", 400*time.Millisecond)

	_, err := env.startRun(ctx, "run-remote-1", "sess-remote-1", "worker-remote")
	require.NoError(t, err)
	before := env.snapshotEverything(t)

	_, err = env.admin.Archive(ctx, consoleActor(), env.name)
	var busy *core.AgentBusyError
	require.ErrorAs(t, err, &busy, "远端仍在跑时归档必须 409 AGENT_BUSY")
	require.Contains(t, busy.RunIDs, "run-remote-1", "409 必须点名远端在途 Run")
	require.Contains(t, busy.Unmet, core.ArchiveResidualRunningRun)

	require.True(t, cancelRequested(t, env, "run-remote-1"),
		"归档必须先发跨 Worker 取消（写 runs.cancel_requested）再等待")
	require.True(t, env.equalEverything(t, before),
		"远端仍在跑时不得移动目录、不得改状态文件、不得恢复注册以外的任何东西")

	records, listErr := env.admin.Archived(ctx)
	require.NoError(t, listErr)
	require.Empty(t, records, "不得宣称归档完成")

	row := findConsoleOperation(t, env.ops, core.ActionAgentArchive)
	require.Equal(t, core.OperationFailed, row.Status)
	require.Equal(t, agentResultAgentBusy, row.ResultCode)

	state, generation := env.indexRow(t)
	require.Equal(t, string(core.AgentActive), state, "失败后屏障必须按 prior_state 回滚")
	require.EqualValues(t, env.generation, generation)
}

// TestArchive_CLIPathRejectsRemoteLiveRun 落地 FR-24-23 的 CLI 分支：`openCLIAdmin` 不装
// 注册面，但 **Run 取消器必须接共享账本**——归档的在途 Run 判定是库上的持久事实，不是
// 进程内的忙碌集合。CLI 归档时远端 Worker 仍在跑 → 409 AGENT_BUSY、取消标志落库、
// 状态文件与目录零变化。
//
// 逆改点：把 `archiveLiveDeps` 写回"注册表 / 调度 / 取消器三者 all-or-nothing"
// （live=false 连等待段一起跳过）→ 本用例变成归档成功、目录被搬走、`Archived` 出现一条
// 记录；`runCancellerAdapter.CancelRun` 不处理 nil service → 取消段 nil 指针 panic。
func TestArchive_CLIPathRejectsRemoteLiveRun(t *testing.T) {
	ctx := context.Background()
	env := newArchiveCLIEnv(t, "alpha", 300*time.Millisecond)

	_, err := env.startRun(ctx, "run-cli-1", "sess-cli-1", "worker-remote")
	require.NoError(t, err, "夹具前提：完整清单必须可准入")
	before := env.snapshotEverything(t)

	_, err = env.admin.Archive(ctx, consoleActor(), env.name)
	var busy *core.AgentBusyError
	require.ErrorAs(t, err, &busy, "CLI 路径同样必须在远端在跑时 409 AGENT_BUSY")
	require.Contains(t, busy.RunIDs, "run-cli-1", "409 必须点名远端在途 Run")
	require.Contains(t, busy.Unmet, core.ArchiveResidualRunningRun)

	require.True(t, cancelRequested(t, env, "run-cli-1"),
		"CLI 也必须发跨 Worker 取消（写 runs.cancel_requested）再等待")
	require.True(t, env.equalEverything(t, before),
		"失败不得移动目录、不得改状态文件、不得动注册面")

	records, listErr := env.admin.Archived(ctx)
	require.NoError(t, listErr)
	require.Empty(t, records, "不得宣称归档完成")

	row := findConsoleOperation(t, env.ops, core.ActionAgentArchive)
	require.Equal(t, core.OperationFailed, row.Status)
	require.Equal(t, agentResultAgentBusy, row.ResultCode)

	state, generation := env.indexRow(t)
	require.Equal(t, string(core.AgentActive), state, "失败后屏障必须按 prior_state 回滚")
	require.EqualValues(t, env.generation, generation)
}

// TestArchive_CLIPathCompletesWithoutLiveRuns 是 CLI 路径的成功侧：没有在途 Run 时等待段
// 立即返回，归档照常完成——证明把取消器改成必填并没有把 CLI 分支的等待段误伤成阻塞。
// The CLI success side: an empty residual snapshot lets the archive finish normally.
func TestArchive_CLIPathCompletesWithoutLiveRuns(t *testing.T) {
	ctx := context.Background()
	env := newArchiveCLIEnv(t, "alpha", 2*time.Second)

	record, err := env.admin.Archive(ctx, consoleActor(), env.name)
	require.NoError(t, err)
	require.NotEmpty(t, record.ArchiveID)
	require.Equal(t, env.name, record.AgentName)

	records, listErr := env.admin.Archived(ctx)
	require.NoError(t, listErr)
	require.Len(t, records, 1, "归档成功必须留下且仅留下一条归档行")

	rec, exists, readErr := env.lifecycle.Read(ctx, env.name)
	require.NoError(t, readErr)
	require.True(t, exists)
	require.Equal(t, core.AgentArchived, rec.State, "成功归档后状态文件必须是 archived")
}

// TestArchive_RequiresRunCanceller 钉死"缺取消器 = 装配错误"：残量快照与有界等待是归档
// 判据本身，任何装配都**不得**把取消器缺席解释成"跳过等待"。判定发生在任何状态读写之前，
// 因此失败必须零副作用（状态文件仍是 active、没有归档行、没有操作行）。
// A missing canceller is an assembly error checked before any state read or write.
func TestArchive_RequiresRunCanceller(t *testing.T) {
	ctx := context.Background()
	env := newAgentEnv(t, "reporter")
	env.deps.RunCanceller = nil
	env.build()

	_, err := env.admin.Archive(ctx, consoleActor(), "reporter")
	require.Error(t, err)
	require.Contains(t, err.Error(), "Run 取消器")

	rec, exists, readErr := env.lifecycle.Read(ctx, "reporter")
	require.NoError(t, readErr)
	require.True(t, exists)
	require.Equal(t, core.AgentActive, rec.State, "装配错误必须在任何状态写入之前返回")

	records, listErr := env.admin.Archived(ctx)
	require.NoError(t, listErr)
	require.Empty(t, records)
}
