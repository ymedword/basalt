//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件是**归属保护写入面**的有库验收（US-24 T008 / FR-24-36 / 模块卡 §4 §4.4）。
// 三个写入面（会话消息追加 / 工具审计 pending 预写与终态回填 / 模型调用 pending 预写）
// 在接管之后一律 `ErrLeaseLost`、零新行；接管前旧持有者持锁时接管必须**等待**，
// 不许跳过租约行直接提交。
//
// 用例与模块卡的对应：
//   - `TestFencedWrites_RejectAfterTakeover`：接管完成后旧 token 在三面写入全部被拒、
//     零新行，且**新** token 在同一库上仍然可写（证明 fence 不是"一律拒绝"）；
//     学习抽取的 Begin / Fail / Commit 三写面自 T009 起并入同一用例（FR-24-36 矩阵）。
//   - `TestFencedWrites_LateWriteAfterTakeover`：基线形态——接管已提交、新属主已写入
//     自己的结果，旧 token 的迟到写入不得覆盖它（消息仍是新属主那条，审计行仍 pending）。
//   - `TestFencedWrites_RetryBindsCurrentRun`：学习抽取的**合法重试**——同一来源的第二次
//     尝试由新 Run 触发并持有其租约，Begin / Commit 都放行，`run_id` 不动。
//   - `TestFencedWrites_WindowGoneControlledClose`：window_gone 的受控收尾三条件
//     （当前租约 + 同来源 + 预期旧尝试），任一不成立即零写入。
//   - `TestRun_FinishRejectsStaleOwnerAfterTakeover` / `TestRun_FinishRejectsExpiredLease`
//     （T010 / 模块卡 §4.4.1）：Run 终态提交的 token fencing——接管后旧代次被拒、
//     过期未接管同样被拒，两者都 `applied=false` 且运行行一字未改。
//
// FR-24-36 的"旧持有者持锁时接管等待"获锁顺序由 `lease_store_test.go` 的
// `TestFencedWrites_OldHolderBlocksTakeover` 覆盖（T005 落地，租约层）；模块卡 R5
// 已说明"持锁期间接管提交完成"的时序在行锁下不可能成立，原
// `TestFencedWrites_InterleavedTakeover` 因此改写为该用例（R7 §4 / 表 5 第 5 条）。
//
// 逆改点：把任一面改回无锁 `EXISTS` 快照 → 接管后的旧写会成功，零新行断言红。
// Reverse edit: an unlocked EXISTS snapshot lets stale writes through.

const (
	fencedRunID     = "run-fenced"
	fencedSessionID = "s-fenced"
	finishFenceRun  = "run-finish-fence"
)

// seedFinishFenceRun 建一条 `running` 的 Run 行（会话行同时种上），留给终态 fencing 用例。
// 这里**不用** `StartRun`：本任务（T010）落地的是仓储层 fencing，`StartRun` 的租约创建
// 在 T012 才换签名；夹具因此按模块卡 §4.4.1 的"写前按实际签名核实"直接写行。
func seedFinishFenceRun(t *testing.T, db *DB) *RunStore {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()
	_, err := db.pool.ExecContext(ctx, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at, caller_id)
		VALUES ('s-finish-fence', 'demo', 'web', 'u1', 'active', 0, $1, $1, 'c1')`, now)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `INSERT INTO runs
		(run_id, session_id, agent, channel, user_id, trigger, status, started_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ($1, 's-finish-fence', 'demo', 'web', 'u1', 'user', 'running', $2,
		        'local-admin', 'admin-builtin', NULL, 'admin-cli')`, finishFenceRun, now)
	require.NoError(t, err)
	return NewRunStore(db)
}

// runStatusOf 读运行行当前状态（判"失权提交一行都没改"）。
func runStatusOf(t *testing.T, db *DB, runID string) string {
	t.Helper()
	var status string
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT status FROM runs WHERE run_id = $1`, runID).Scan(&status))
	return status
}

// fencedFixture 是一个带**活租约**的 Run 夹具：会话行、Run 行、租约（worker-a、epoch 1）
// 都已就绪，四个仓储用同一份隔离 schema。
type fencedFixture struct {
	db       *DB
	leases   *LeaseStore
	session  *SessionStore
	tools    *ToolInvocationRepo
	calls    *LLMCallRepo
	learning *LearningRepo
	token    core.LeaseToken
}

// newFencedFixture 建库、种会话与 Run，并让 worker-a 领到 run 租约。
func newFencedFixture(t *testing.T) *fencedFixture {
	t.Helper()
	ctx := context.Background()
	db := newTxTestDB(t)
	now := time.Now().UTC().UnixMilli()
	_, err := db.pool.ExecContext(ctx, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at, caller_id)
		VALUES ($1, 'demo', 'web', 'u1', 'active', 0, $2, $2, 'c1')`, fencedSessionID, now)
	require.NoError(t, err)
	// 最省事的授权组合：admin-builtin + local-admin + 无 key，三条 CHECK 全过。
	_, err = db.pool.ExecContext(ctx, `INSERT INTO runs
		(run_id, session_id, agent, channel, user_id, trigger, status, started_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ($1, $2, 'demo', 'web', 'u1', 'user', 'running', $3,
		        'local-admin', 'admin-builtin', NULL, 'admin-cli')`, fencedRunID, fencedSessionID, now)
	require.NoError(t, err)

	leases := NewLeaseStore(db)
	lease := mustAcquire(t, leases, core.LeaseRun, fencedRunID, "worker-a")
	return &fencedFixture{
		db:       db,
		leases:   leases,
		session:  NewSessionStore(db),
		tools:    NewToolInvocationRepo(db),
		calls:    NewLLMCallRepo(db),
		learning: NewLearningRepo(db),
		token:    lease.LeaseToken,
	}
}

// takeover 让 worker-b 接管（调用前先把租约改到过去）。
func (f *fencedFixture) takeover(t *testing.T) core.LeaseToken {
	t.Helper()
	expireLease(t, f.leases, core.LeaseRun, fencedRunID)
	lease := mustAcquire(t, f.leases, core.LeaseRun, fencedRunID, "worker-b")
	require.True(t, lease.TookOver, "过期领取必须是接管")
	require.Equal(t, int64(2), lease.OwnershipEpoch, "接管递增代次")
	return lease.LeaseToken
}

// writeCounts 是六个写入面的行数快照（判"零新行"）：消息 / 工具审计 / 模型调用，以及
// 学习抽取的账本行 / 候选条目 / 草案行。
func (f *fencedFixture) writeCounts(t *testing.T) map[string]int {
	t.Helper()
	out := make(map[string]int, 6)
	for _, table := range []string{
		"messages", "tool_invocations", "llm_calls",
		"memory_extractions", "memory_entries", "memory_method_drafts",
	} {
		var n int
		require.NoError(t, f.db.pool.QueryRowContext(context.Background(),
			"SELECT count(*) FROM "+table).Scan(&n))
		out[table] = n
	}
	return out
}

// 终态回填面的夹具访问器自 T009 起由新属主 `FencedBeginInvocation` 的返回值替代：
// 原 `pendingToolID` 在接管后取不到"新属主写的那一行"，留着只会误导下一个改用例的人。
// The terminal-write fixture accessor is gone: the new owner's FencedBeginInvocation
// returns the row handle, and a pending-row lookup after takeover would find the wrong one.

// TestFencedWrites_RejectAfterTakeover 判 FR-24-36 的主判据：接管之后，旧 token 在
// 消息追加 / 工具审计预写与回填 / 模型调用预写三面**全部** `ErrLeaseLost`，且三张表
// 零新行；随后新 token 在同一库上三面写入全部成功——fence 拒绝的是失权者，不是写入本身。
func TestFencedWrites_RejectAfterTakeover(t *testing.T) {
	f := newFencedFixture(t)
	ctx := context.Background()
	stale := f.token
	// 学习抽取三写面：接管**之前**先用当时有效的 token 预写一条 pending 账本行，
	// 接管后才好判"失权者的 Fail / Commit 一行都改不动"。
	_, err := f.learning.BeginExtraction(ctx,
		lsRow("ext-fenced", "demo", fencedSessionID, fencedRunID, 10, 20, 4), stale)
	require.NoError(t, err)
	fresh := f.takeover(t)

	// 新属主先写一条 pending 工具审计行：终态回填面需要一条真实存在的行。
	freshToolID, err := f.tools.FencedBeginInvocation(ctx, fresh, core.ToolInvocation{
		RunID: fencedRunID, SessionID: fencedSessionID, ToolCallID: "call-b",
		ToolName: "read_file", InputJSON: []byte(`{"path":"a"}`), CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	before := f.writeCounts(t)

	_, err = f.session.FencedAppendMessage(ctx, stale, fencedSessionID, fencedRunID,
		core.Message{Role: core.RoleUser, Content: "旧持有者的迟到消息"})
	require.ErrorIs(t, err, core.ErrLeaseLost, "消息追加面必须拒绝失权写入")

	_, err = f.tools.FencedBeginInvocation(ctx, stale, core.ToolInvocation{
		RunID: fencedRunID, SessionID: fencedSessionID, ToolCallID: "call-a",
		ToolName: "read_file", InputJSON: []byte(`{"path":"a"}`), CreatedAt: time.Now().UTC(),
	})
	require.ErrorIs(t, err, core.ErrLeaseLost, "工具审计 pending 预写面必须拒绝失权写入")

	err = f.tools.FencedFinishInvocation(ctx, stale, freshToolID, core.ToolInvocationResult{
		Status: core.LLMCallSuccess, ResultJSON: []byte(`{"content":"旧写"}`),
	})
	require.ErrorIs(t, err, core.ErrLeaseLost, "工具审计终态回填面必须拒绝失权写入")

	_, err = f.calls.FencedBeginCall(ctx, stale, core.LLMCall{
		RunID: fencedRunID, SessionID: fencedSessionID, Round: 1,
		Provider: "stub", Model: "stub-model", CreatedAt: time.Now().UTC(),
	})
	require.ErrorIs(t, err, core.ErrLeaseLost, "模型调用 pending 预写面必须拒绝失权写入")

	// 学习抽取三写面（FR-24-36 矩阵 / T009）：Begin 在业务判定之前就被租约壳拒绝——
	// 同来源已有 pending 行本来会返回 ErrLearningUnresolved，这里必须先是 ErrLeaseLost，
	// 证明 fence 先于一切业务读。
	_, err = f.learning.BeginExtraction(ctx,
		lsRow("ext-stale", "demo", fencedSessionID, fencedRunID, 10, 20, 4), stale)
	require.ErrorIs(t, err, core.ErrLeaseLost, "学习抽取 Begin 面必须拒绝失权写入")

	err = f.learning.FailExtraction(ctx, "ext-fenced", fencedRunID, stale,
		"", 0, core.LearningFailureModel, "stale", time.Now().UTC())
	require.ErrorIs(t, err, core.ErrLeaseLost, "学习抽取 Fail 面必须拒绝失权写入")

	_, err = f.learning.CommitExtraction(ctx, core.CommitLearningExtraction{
		ExtractionID:  "ext-fenced",
		Candidates:    []core.LearningCandidate{lsCandidate("失权候选")},
		ItemsTotal:    1,
		ItemsAccepted: 1,
		CompletedAt:   time.Now().UTC(),
	}, stale)
	require.ErrorIs(t, err, core.ErrLeaseLost, "学习抽取 Commit 面必须拒绝失权写入")

	after := f.writeCounts(t)
	require.Equal(t, before, after,
		"失权写入不得产生任何新行（messages / tool_invocations / llm_calls / memory_*）")

	// 反向判据：同一个库、同一组仓储，换成**当前** token 就必须写得进去。
	msgID, err := f.session.FencedAppendMessage(ctx, fresh, fencedSessionID, fencedRunID,
		core.Message{Role: core.RoleUser, Content: "新属主的消息"})
	require.NoError(t, err)
	require.NotEmpty(t, msgID, "当前 token 的追加必须拿到消息标识")

	_, err = f.tools.FencedBeginInvocation(ctx, fresh, core.ToolInvocation{
		RunID: fencedRunID, SessionID: fencedSessionID, ToolCallID: "call-b2",
		ToolName: "read_file", InputJSON: []byte(`{"path":"b"}`), CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, err = f.calls.FencedBeginCall(ctx, fresh, core.LLMCall{
		RunID: fencedRunID, SessionID: fencedSessionID, Round: 1,
		Provider: "stub", Model: "stub-model", CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// 学习抽取的正向判据：同一个 pending 账本行换成当前 token 必须提交成功——
	// fence 拒绝的是失权者，不是学习写入本身。
	result, err := f.learning.CommitExtraction(ctx, core.CommitLearningExtraction{
		ExtractionID:  "ext-fenced",
		Candidates:    []core.LearningCandidate{lsCandidate("新属主的候选")},
		ItemsTotal:    1,
		ItemsAccepted: 1,
		CompletedAt:   time.Now().UTC(),
	}, fresh)
	require.NoError(t, err, "当前 token 的学习提交必须成功")
	require.Len(t, result.CandidateEntryIDs, 1)
}

// TestFencedWrites_LateWriteAfterTakeover 是模块卡 §4 的**基线**形态：接管先提交、
// 新属主已写入自己的结果，旧 token 的迟到写入不得覆盖它——消息历史仍是新属主那条，
// 被接管的 pending 审计行仍是 pending（终态由回收/新属主决定，不由旧写改）。
func TestFencedWrites_LateWriteAfterTakeover(t *testing.T) {
	f := newFencedFixture(t)
	ctx := context.Background()
	stale := f.token
	fresh := f.takeover(t)

	freshToolID, err := f.tools.FencedBeginInvocation(ctx, fresh, core.ToolInvocation{
		RunID: fencedRunID, SessionID: fencedSessionID, ToolCallID: "call-b",
		ToolName: "read_file", InputJSON: []byte(`{"path":"a"}`), CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	_, err = f.session.FencedAppendMessage(ctx, fresh, fencedSessionID, fencedRunID,
		core.Message{Role: core.RoleAssistant, Content: "新属主的结果"})
	require.NoError(t, err)

	_, err = f.session.FencedAppendMessage(ctx, stale, fencedSessionID, fencedRunID,
		core.Message{Role: core.RoleAssistant, Content: "旧属主的覆盖尝试"})
	require.ErrorIs(t, err, core.ErrLeaseLost)

	err = f.tools.FencedFinishInvocation(ctx, stale, freshToolID, core.ToolInvocationResult{
		Status: core.LLMCallSuccess, ResultJSON: []byte(`{"content":"旧属主改写"}`),
	})
	require.ErrorIs(t, err, core.ErrLeaseLost)

	var content string
	var count int
	require.NoError(t, f.db.pool.QueryRowContext(ctx,
		`SELECT count(*), max(content) FROM messages WHERE session_id = $1`,
		fencedSessionID).Scan(&count, &content))
	require.Equal(t, 1, count, "旧属主的迟到写入不得产生第二条消息")
	require.Equal(t, "新属主的结果", content, "新属主的结果不得被覆盖")

	var status string
	require.NoError(t, f.db.pool.QueryRowContext(ctx,
		`SELECT status FROM tool_invocations WHERE id = $1`, freshToolID).Scan(&status))
	require.Equal(t, string(core.LLMCallPending), status, "失权回填不得改动审计行终态")
}

// TestFencedWrites_RetryBindsCurrentRun 守 FR-24-36 的**合法重试**判据：同一来源的第二次
// 尝试由新的触发 Run 发起、且该 Run 持有有效租约时，Begin / Commit 都必须放行——fence
// 认的是本轮触发 Run（刚写进 `last_attempt_run_id` 的那一个），不是 `run_id` 里那个首次
// Run。三个 Run 身份的区分见模块卡 §2.3.5：`run_id` 首 Run 不可变且 UNIQUE，
// `last_attempt_run_id` 才是本轮尝试身份。
//
// 逆改点：把 fence / 绑定对象改回 row.RunID（首次 Run）→ Begin 在租约层就 ErrLeaseLost，
// 合法重试被误拒（断言红）。
// A legitimate retry fenced on the current trigger run must pass; fencing the first run
// would reject it.
func TestFencedWrites_RetryBindsCurrentRun(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	leases := NewLeaseStore(db)
	lsSeedRun(t, db, "lrb-r1", "ls-s1", "k1", lsBase)
	lsSeedRun(t, db, "lrb-r2", "ls-s1", "k1", lsBase.Add(time.Minute))
	// 本轮触发 Run 是在跑的 Run：租约由 worker-b 持有且未过期。
	trigger := mustAcquire(t, leases, core.LeaseRun, "lrb-r2", "worker-b").LeaseToken

	// 第一次尝试（旧装配谱系、无执行权凭证）以模型错误结束，行停在可重试的 failed。
	_, err := store.BeginExtraction(ctx, lsRow("ext-retry", "demo", "ls-s1", "lrb-r1", 10, 20, 4),
		core.LeaseToken{})
	require.NoError(t, err)
	require.NoError(t, store.FailExtraction(ctx, "ext-retry", "lrb-r1", core.LeaseToken{},
		"", 0, core.LearningFailureModel, "boom", lsBase))

	// 生产形态（internal/memory/learning.go:432-433）：重试传入的是**未决行本身**，
	// `RunID` 仍是首次 Run，只有 `LastAttemptRunID` 被改成本轮触发 Run。
	retry := lsRow("ext-retry", "demo", "ls-s1", "lrb-r1", 10, 20, 4)
	retry.LastAttemptRunID = "lrb-r2"
	retry.LastAttemptAt, retry.CreatedAt = lsBase.Add(time.Minute), lsBase.Add(time.Minute)
	got, err := store.BeginExtraction(ctx, retry, trigger)
	require.NoError(t, err, "同来源第二次尝试持有当前 Run 的有效租约时必须放行，不得误判失权")
	require.Equal(t, "ext-retry", got.ExtractionID, "重试复用同一账本行")
	require.Equal(t, 2, got.Attempts)
	require.Equal(t, "lrb-r2", got.LastAttemptRunID, "尝试身份绑定本轮触发 Run")
	require.Equal(t, "lrb-r1", got.RunID, "run_id 仍是首次 Run，UNIQUE 列不随重试改写")
	require.Equal(t, int64(20), got.WindowToMessageID, "重试仍复用冻结窗口")

	// 同一个 token 的提交同样必须成功：fence 对象与本轮尝试身份一致。
	result, err := store.CommitExtraction(ctx, core.CommitLearningExtraction{
		ExtractionID:  "ext-retry",
		Candidates:    []core.LearningCandidate{lsCandidate("重试成功的候选")},
		ItemsTotal:    1,
		ItemsAccepted: 1,
		CompletedAt:   lsBase.Add(2 * time.Minute),
	}, trigger)
	require.NoError(t, err, "fence 认的是本轮触发 Run，提交不得按首次 Run 拒绝")
	require.Len(t, result.CandidateEntryIDs, 1)

	var status, runID, lastAttempt string
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT status, run_id, last_attempt_run_id FROM memory_extractions
		  WHERE extraction_id = 'ext-retry'`).Scan(&status, &runID, &lastAttempt))
	require.Equal(t, string(core.LearningExtractionSuccess), status)
	require.Equal(t, "lrb-r1", runID)
	require.Equal(t, "lrb-r2", lastAttempt)
}

// TestFencedWrites_WindowGoneControlledClose 守 FR-24-36 的 window_gone **受控收尾**：
// 重试路径在 Begin 之前重读冻结窗口（C-33），发现消息已被清理时行还停在上一次尝试的
// failed；收尾必须同时满足三条——① 当前租约（外壳校验）、② 同来源（Agent / 会话 /
// 来源调用方 / 来源 Key 四项逐项相等）、③ 预期旧尝试（attempts 与
// last_attempt_run_id 的 CAS）。任一条不成立 → 不写、返回 ErrConflict。
//
// 收尾是这次 Run 的**失败判定**而非一次尝试：`attempts` 与 `last_attempt_run_id`
// 原样保留，不冒名顶替上一次尝试，也不计费。
//
// 逆改点：去掉 ③ 的 CAS 条件 → attempts 不符也写成 skipped（断言红）；同来源改成只比
// 会话 → 另一会话的触发 Run 也能替这行收尾（断言红）；收尾顺带写
// last_attempt_run_id = 触发 Run → 「原样」断言红。
// The window_gone closeout needs all three conditions: current lease, same origin, and
// the expected old attempt; none of them rewrites attempts or last_attempt_run_id.
func TestFencedWrites_WindowGoneControlledClose(t *testing.T) {
	ctx := context.Background()

	// seedCloseout 准备一条停在第 2 次尝试 failed 的账本行，并给第 3 个 Run（本轮触发）
	// 领取活租约。triggerSession 允许另设，用来构造"不同来源"的触发 Run。
	seedCloseout := func(t *testing.T, triggerSession string) (*DB, *LearningRepo, core.LeaseToken) {
		t.Helper()
		db := newLearningStoreDB(t)
		store := NewLearningRepo(db)
		leases := NewLeaseStore(db)
		lsSeedRun(t, db, "lwg-r1", "ls-s1", "k1", lsBase)
		lsSeedRun(t, db, "lwg-r2", "ls-s1", "k1", lsBase.Add(time.Minute))
		lsSeedRun(t, db, "lwg-r3", triggerSession, "k1", lsBase.Add(2*time.Minute))

		_, err := store.BeginExtraction(ctx, lsRow("ext-close", "demo", "ls-s1", "lwg-r1", 10, 20, 4),
			core.LeaseToken{})
		require.NoError(t, err)
		require.NoError(t, store.FailExtraction(ctx, "ext-close", "lwg-r1", core.LeaseToken{},
			"", 0, core.LearningFailureModel, "boom", lsBase))

		// 与生产一致：重试传未决行本身，RunID 仍是首次 Run，LastAttemptRunID 换成本轮。
		retry := lsRow("ext-close", "demo", "ls-s1", "lwg-r1", 10, 20, 4)
		retry.LastAttemptRunID = "lwg-r2"
		retry.LastAttemptAt, retry.CreatedAt = lsBase.Add(time.Minute), lsBase.Add(time.Minute)
		_, err = store.BeginExtraction(ctx, retry, core.LeaseToken{})
		require.NoError(t, err)
		require.NoError(t, store.FailExtraction(ctx, "ext-close", "lwg-r2", core.LeaseToken{},
			"", 0, core.LearningFailureModel, "boom", lsBase.Add(time.Minute)))

		token := mustAcquire(t, leases, core.LeaseRun, "lwg-r3", "worker-b").LeaseToken
		return db, store, token
	}

	// assertUntouched 判"条件不成立时一行都不写"：仍是 failed、attempts 与旧尝试身份
	// 原样、没有终态时刻、游标没有推进。
	assertUntouched := func(t *testing.T, db *DB, store *LearningRepo) {
		t.Helper()
		var status, last string
		var attempts int
		var finishedAt sql.NullInt64
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT status, attempts, last_attempt_run_id, finished_at
			   FROM memory_extractions WHERE extraction_id = 'ext-close'`).
			Scan(&status, &attempts, &last, &finishedAt))
		require.Equal(t, string(core.LearningExtractionFailed), status, "条件不成立时不得改写行")
		require.Equal(t, 2, attempts, "条件不成立时不得改动尝试数")
		require.Equal(t, "lwg-r2", last, "条件不成立时不得改动尝试身份")
		require.False(t, finishedAt.Valid, "条件不成立时不得写终态时刻")

		_, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "lwg-r3")
		require.NoError(t, err)
		require.False(t, found, "条件不成立时不得推进游标")
	}

	t.Run("controlled-close-once", func(t *testing.T) {
		db, store, token := seedCloseout(t, "ls-s1")
		require.NoError(t, store.FailExtraction(ctx, "ext-close", "lwg-r3", token,
			"lwg-r2", 2, core.LearningFailureWindowGone,
			"frozen window incomplete", lsBase.Add(2*time.Minute)),
			"三条同时成立时必须收尾为 skipped")

		var status, class, last string
		var attempts int
		var finishedAt sql.NullInt64
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT status, failure_class, attempts, last_attempt_run_id, finished_at
			   FROM memory_extractions WHERE extraction_id = 'ext-close'`).
			Scan(&status, &class, &attempts, &last, &finishedAt))
		require.Equal(t, string(core.LearningExtractionSkipped), status)
		require.Equal(t, string(core.LearningFailureWindowGone), class)
		require.Equal(t, 2, attempts, "受控收尾不计入本次尝试：attempts 原样")
		require.Equal(t, "lwg-r2", last, "last_attempt_run_id 仍停在上一次真实尝试")
		require.True(t, finishedAt.Valid)

		cursor, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "lwg-r3")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, int64(20), cursor.CommittedMessageID, "skipped 在同一事务推进游标")
	})

	t.Run("foreign-origin-no-write", func(t *testing.T) {
		db, store, token := seedCloseout(t, "ls-other")
		err := store.FailExtraction(ctx, "ext-close", "lwg-r3", token,
			"lwg-r2", 2, core.LearningFailureWindowGone,
			"frozen window incomplete", lsBase.Add(2*time.Minute))
		require.ErrorIs(t, err, core.ErrConflict, "触发 Run 与抽取行不同来源时必须拒收尾")
		assertUntouched(t, db, store)
	})

	t.Run("attempts-mismatch-no-write", func(t *testing.T) {
		db, store, token := seedCloseout(t, "ls-s1")
		err := store.FailExtraction(ctx, "ext-close", "lwg-r3", token,
			"lwg-r2", 1, core.LearningFailureWindowGone,
			"frozen window incomplete", lsBase.Add(2*time.Minute))
		require.ErrorIs(t, err, core.ErrConflict, "预期尝试数不符时必须拒收尾")
		assertUntouched(t, db, store)
	})

	t.Run("prev-run-mismatch-no-write", func(t *testing.T) {
		db, store, token := seedCloseout(t, "ls-s1")
		err := store.FailExtraction(ctx, "ext-close", "lwg-r3", token,
			"lwg-r1", 2, core.LearningFailureWindowGone,
			"frozen window incomplete", lsBase.Add(2*time.Minute))
		require.ErrorIs(t, err, core.ErrConflict, "预期旧尝试 Run 不符时必须拒收尾")
		assertUntouched(t, db, store)
	})

	t.Run("missing-trigger-no-write", func(t *testing.T) {
		db, store, _ := seedCloseout(t, "ls-s1")
		err := store.FailExtraction(ctx, "ext-close", "", core.LeaseToken{},
			"lwg-r2", 2, core.LearningFailureWindowGone,
			"frozen window incomplete", lsBase.Add(2*time.Minute))
		require.ErrorIs(t, err, core.ErrConflict, "没有触发 Run 就无从判同来源，必须拒收尾")
		assertUntouched(t, db, store)
	})
}

// TestRun_FinishRejectsStaleOwnerAfterTakeover 守模块卡 §4.4.1 / FR-24-06 / C-01 / C-04：
// 接管之后旧持有者的终态提交必须被拒（`ErrLeaseLost` + `applied=false`），新持有者的
// 提交必须成功。判据面按仓储层落地，配合两条核心层用例组成完整证据：
//   - `applied=false` **同时**意味着抽取钩子不会被触发——钩子的触发条件是
//     `applied == true && StatusCompleted`（`internal/core/service.go:707-715`），
//     这一半由 `core.TestUS22_Hook_OnlyAfterTerminalApplied/idempotent-not-applied` 判。
//   - 本用例额外断言运行行**仍是 running**：旧持有者连终态列都没写进去，所以即便钩子
//     条件写错，也没有可抽取的"完成"。
//
// §4.4.1 的 `StartRun` / `seedRunRecord` 写法依赖 T012 的租约内建事务，本任务按模块卡
// "写前按实际签名核实"的既有约定改为直接种行 + `LeaseStore.Acquire`；T012 落地后可在
// 同一断言集上换成 `StartRun` 入口。
//
// 逆改点：删掉 `fencedTx` 的租约行锁与 owner/epoch 校验、退回 `EXISTS` 快照写法 →
// 旧 token 的 `FinishRun` 成功，`ErrLeaseLost` 与 `applied=false` 两条断言同时红。
// A stale owner is fenced out after takeover while the new owner's write goes through.
func TestRun_FinishRejectsStaleOwnerAfterTakeover(t *testing.T) {
	ctx := context.Background()
	db := newTxTestDB(t)
	runs := seedFinishFenceRun(t, db)
	leases := NewLeaseStore(db)

	oldLease := mustAcquire(t, leases, core.LeaseRun, finishFenceRun, "worker-old")
	require.Equal(t, int64(1), oldLease.OwnershipEpoch)

	expireLease(t, leases, core.LeaseRun, finishFenceRun)
	taken := mustAcquire(t, leases, core.LeaseRun, finishFenceRun, "worker-new")
	require.True(t, taken.TookOver, "过期领取必须是接管")
	require.Equal(t, int64(2), taken.OwnershipEpoch, "接管必须递增归属代次")

	applied, err := runs.FinishRun(ctx, finishFenceRun, oldLease.LeaseToken,
		core.StatusFailed, "old owner resumed", 0, 0, time.Now().UTC())
	require.ErrorIs(t, err, core.ErrLeaseLost, "接管后旧持有者的终态提交必须被拒")
	require.False(t, applied, "旧持有者不得完成状态转换")
	require.Equal(t, "running", runStatusOf(t, db, finishFenceRun),
		"失权提交不得改写终态列——applied=false 之外，行本身也必须原样")

	applied, err = runs.FinishRun(ctx, finishFenceRun, taken.LeaseToken,
		core.StatusFailed, "new owner recovered", 0, 0, time.Now().UTC())
	require.NoError(t, err, "当前持有者的终态提交必须放行")
	require.True(t, applied)

	var status, reason string
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT status, reason FROM runs WHERE run_id = $1`, finishFenceRun).
		Scan(&status, &reason))
	require.Equal(t, string(core.StatusFailed), status)
	require.Equal(t, "new owner recovered", reason, "终态写回的是新持有者的结果")
}

// TestRun_FinishRejectsExpiredLease 守 C-03 / FR-24-04：租约**过期但尚未被接管**时，
// 原负责人同样不得提交——过期判据来自数据库时钟，不依赖"有没有别人抢走"。
// An expired lease stops its original owner even before anybody takes it over.
func TestRun_FinishRejectsExpiredLease(t *testing.T) {
	ctx := context.Background()
	db := newTxTestDB(t)
	runs := seedFinishFenceRun(t, db)
	leases := NewLeaseStore(db)

	lease := mustAcquire(t, leases, core.LeaseRun, finishFenceRun, "worker-a")
	expireLease(t, leases, core.LeaseRun, finishFenceRun)

	applied, err := runs.FinishRun(ctx, finishFenceRun, lease.LeaseToken,
		core.StatusCompleted, "expired owner", 1, 0, time.Now().UTC())
	require.ErrorIs(t, err, core.ErrLeaseLost, "过期后即使未被接管，原负责人也不得提交")
	require.False(t, applied)
	require.Equal(t, "running", runStatusOf(t, db, finishFenceRun), "过期提交不得改写终态列")

	// 过期 ≠ 已释放：仍须走合法接管，接管后的新代次才能收尾。
	taken := mustAcquire(t, leases, core.LeaseRun, finishFenceRun, "worker-b")
	require.True(t, taken.TookOver)
	applied, err = runs.FinishRun(ctx, finishFenceRun, taken.LeaseToken,
		core.StatusFailed, "taken over", 0, 0, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, applied)
}

// TestCancel_RemoteWorkerObservesFlag（T024 / 模块卡 §4.4.6 / FR-24-22 / C-16）：
// 非属主调用 `RequestCancel` **只写** `runs.cancel_requested` 并如实返回 running——
// 不假装取消成功、不替属主写终态；属主在下一次续约响应里看到标志并自行收敛；已终态
// 之后再取消是幂等成功（状态不被改口）。
//
// 夹具按模块卡 §4.4.6 的动作序列落地：`seedRunRecord` 未随 T008/T010 落地（见 run-state
// §1.3 T024 行），同一动作由本文件的 `seedFinishFenceRun` + `LeaseStore.Acquire` 承担。
// 逆改点：`RequestCancel` 改成只更新内存 / 不写库 → `CancelRequested` 断言红；改成直接
// 写终态 → "非属主不替属主写终态" 与终态幂等断言红。
// Reverse edit: dropping the flag write or claiming success turns the renewal and status
// assertions red.
func TestCancel_RemoteWorkerObservesFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := newTxTestDB(t)
	runs := seedFinishFenceRun(t, db)
	leases := NewLeaseStore(db)

	// 属主先领到租约：本用例只关心"请求从非属主到达属主"的通道。
	owner := mustAcquire(t, leases, core.LeaseRun, finishFenceRun, "worker-owner")

	detail, err := runs.RequestCancel(ctx, finishFenceRun)
	require.NoError(t, err)
	require.Equal(t, core.StatusRunning, detail.Status, "非属主只写标志，不假装取消成功")
	require.Equal(t, "running", runStatusOf(t, db, finishFenceRun),
		"非属主不得替属主写终态")

	lease, err := leases.Renew(ctx, owner.LeaseToken, leaseTestTTL)
	require.NoError(t, err)
	require.True(t, lease.CancelRequested, "属主必须在续约响应中看到取消标志")

	// 属主经 fencing 自行收敛：取消后的终态仍由属主提交，不由非属主代写。
	applied, err := runs.FinishRun(ctx, finishFenceRun, owner.LeaseToken,
		core.StatusCancelled, "cancelled by owner", 0, 0, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, applied, "属主持有效租约，取消终态提交必须放行")

	detail, err = runs.RequestCancel(ctx, finishFenceRun)
	require.NoError(t, err)
	require.Equal(t, core.StatusCancelled, detail.Status,
		"已终态再取消：幂等成功，不改变状态")
	require.Equal(t, "cancelled", runStatusOf(t, db, finishFenceRun))
}

// ============================================================
// StartRun 准入单事务（T012 / FR-24-07 / FR-24-33 / FR-24-34 / FR-24-43）
// ============================================================

// us24CallerPrincipal 构造合法调用方身份（caller-key 授权，Key 非空）。
// us24CallerPrincipal builds a valid caller-key principal for admission cases.
func us24CallerPrincipal(callerID string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: callerID,
		KeyID:    "key-" + callerID,
		Scopes:   []string{core.ScopeSessionsRead},
		Actor:    core.ActorCaller,
	}
}

// admissionRunRecord 组装一条最小 Run 记录（零清单；会话为空 = 无状态调用）。
// admissionRunRecord builds a minimal run record; an empty session means stateless.
func admissionRunRecord(runID, sessionID string, p core.Principal) core.RunRecord {
	return core.RunRecord{
		RunID:     runID,
		SessionID: sessionID,
		Agent:     "demo",
		Channel:   "web",
		UserID:    "u1",
		Principal: p,
		StartedAt: time.Now().UTC(),
	}
}

// admissionCount 数一条整数查询。
// admissionCount runs one integer count query.
func admissionCount(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.pool.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

// admissionWorker 构造一个非零 Worker 身份（StartRun 据此建租约）。
// admissionWorker builds a non-zero worker identity so StartRun creates a lease.
func admissionWorker(instanceID string) core.WorkerIdentity {
	return core.WorkerIdentity{InstanceID: instanceID, Name: "worker-" + instanceID}
}

// TestRun_SessionBusyAcrossWorkers 逐字落地模块卡 §4.4.2 / FR-24-07 / FR-24-43：
// 同一会话跨 Worker 同时最多一条未过期 run 租约。首次准入成功；第二个 Worker 的同会话
// 准入 409（零占用）；终态提交**只写终态列**、不释放租约，会话仍忙；收尾释放租约后
// 新准入才放行。
// 逆改点：把会话复查从"未释放租约 + 状态/未过期"退化成只查 `runs.status='running'`
// → 第三步（终态提交后仍忙）立刻放行而红。
func TestRun_SessionBusyAcrossWorkers(t *testing.T) {
	ctx := context.Background()
	db := newTxTestDB(t)
	store := NewRunStoreWithLeaseTTL(db, 5*time.Minute)
	principal := core.LocalAdminCLIPrincipal()
	const sessionID = "s-us24-busy"

	first := admissionRunRecord("run-busy-1", sessionID, principal)
	token, err := store.StartRun(ctx, first, core.RunResourceManifest{},
		admissionWorker("worker-a"), core.CallerAdmission{}, first.RunID)
	require.NoError(t, err, "首次准入必须成功")
	require.Equal(t, core.LeaseToken{
		Kind: core.LeaseRun, ResourceID: "run-busy-1",
		OwnerWorkerID: "worker-a", OwnershipEpoch: 1,
	}, token)

	second := admissionRunRecord("run-busy-2", sessionID, principal)
	_, err = store.StartRun(ctx, second, core.RunResourceManifest{},
		admissionWorker("worker-b"), core.CallerAdmission{}, second.RunID)
	var busy *core.SessionBusyError
	require.ErrorAs(t, err, &busy, "跨 Worker 同会话必须 409 SESSION_BUSY")
	require.ErrorIs(t, err, core.ErrConflict, "会话忙在分类上是冲突")
	require.Equal(t, sessionID, busy.SessionID)
	require.Equal(t, 1, admissionCount(t, db,
		`SELECT count(*) FROM runs WHERE session_id = $1`, sessionID),
		"被拒的 Run 不得留下账本行")
	require.Equal(t, 0, admissionCount(t, db,
		`SELECT count(*) FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(core.LeaseRun), "run-busy-2"), "被拒的 Run 不得留下租约")
	require.Equal(t, 0, admissionCount(t, db,
		`SELECT count(*) FROM run_resource_manifests WHERE run_id = $1`, "run-busy-2"),
		"被拒的 Run 不得留下清单")

	// 终态提交只写终态列：租约仍属于旧 Run，引用 / 投递留给收尾（FR-24-06）。
	applied, err := store.FinishRun(ctx, "run-busy-1", token, core.StatusCancelled,
		"cancelled", 0, 0, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, 1, admissionCount(t, db,
		`SELECT count(*) FROM execution_leases
		  WHERE kind = $1 AND resource_id = $2 AND released_at IS NULL`,
		string(core.LeaseRun), "run-busy-1"), "终态提交不得释放租约")

	third := admissionRunRecord("run-busy-3", sessionID, principal)
	_, err = store.StartRun(ctx, third, core.RunResourceManifest{},
		admissionWorker("worker-b"), core.CallerAdmission{}, third.RunID)
	require.ErrorAs(t, err, &busy, "收尾完成前会话仍为忙（终态不等于收尾）")

	require.NoError(t, store.CompleteRunCloseout(ctx, "run-busy-1", token, time.Now().UTC()))
	fourth := admissionRunRecord("run-busy-4", sessionID, principal)
	nextToken, err := store.StartRun(ctx, fourth, core.RunResourceManifest{},
		admissionWorker("worker-b"), core.CallerAdmission{}, fourth.RunID)
	require.NoError(t, err, "收尾释放租约后新 Run 必须被放行")
	require.Equal(t, int64(1), nextToken.OwnershipEpoch, "新 Run 是新租约，代次从 1 起")
}

// TestAdmission_BlocksUntilRecoveryCommits 守 FR-24-43 / 模块卡 §4.4.3：会话准入与
// 接管恢复共用同一把 advisory 锁。恢复事务持锁未提交时，同会话 StartRun **不得**在
// 恢复提交前返回成功；提交后按旧 Run 状态 409 或放行。
//
// 判"阻塞"的手法：给被阻塞的准入一个截止时间——若它没有取同一把会话锁，会在毫秒级
// 成功返回（`require.ErrorIs(..., context.DeadlineExceeded)` 红），不依赖机器快慢。
// 逆改点：把 StartRun 的会话 advisory 锁删掉 → 第一个子用例拿到成功返回而红。
func TestAdmission_BlocksUntilRecoveryCommits(t *testing.T) {
	seedCrashedRun := func(t *testing.T, db *DB) *RunStore {
		t.Helper()
		ctx := context.Background()
		store := NewRunStoreWithLeaseTTL(db, time.Minute)
		rec := admissionRunRecord("run-crash-1", "s-us24-recover", core.LocalAdminCLIPrincipal())
		_, err := store.StartRun(ctx, rec, core.RunResourceManifest{},
			admissionWorker("worker-a"), core.CallerAdmission{}, rec.RunID)
		require.NoError(t, err)
		// 制造"Worker A 崩溃留下的过期租约"：A1 恢复要处理的正是这种行。
		expireLease(t, NewLeaseStore(db), core.LeaseRun, "run-crash-1")
		return store
	}

	t.Run("blocks-until-recovery-commits", func(t *testing.T) {
		ctx := context.Background()
		db := newTxTestDB(t)
		store := seedCrashedRun(t, db)

		// "恢复事务"在另一条连接上持会话锁，并在锁内把旧 Run 收成 failed、释放租约。
		recovery, err := db.pool.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = recovery.Rollback() })
		_, err = recovery.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`,
			sessionAdvisoryKey("s-us24-recover"))
		require.NoError(t, err)
		_, err = recovery.ExecContext(ctx, `
			UPDATE runs SET status = 'failed', reason = 'worker lost', finished_at = $1
			 WHERE run_id = 'run-crash-1'`, time.Now().UTC().UnixMilli())
		require.NoError(t, err)
		_, err = recovery.ExecContext(ctx, `
			UPDATE execution_leases
			   SET released_at = (extract(epoch from clock_timestamp()) * 1000)::bigint
			 WHERE kind = 'run' AND resource_id = 'run-crash-1'`)
		require.NoError(t, err)

		// 恢复未提交：同会话准入必须在锁上等待，截止时间把它变成可判定的失败。
		blockedCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		next := admissionRunRecord("run-crash-2", "s-us24-recover", core.LocalAdminCLIPrincipal())
		_, err = store.StartRun(blockedCtx, next, core.RunResourceManifest{},
			admissionWorker("worker-b"), core.CallerAdmission{}, next.RunID)
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"恢复事务未提交时，同会话准入不得返回成功")
		require.Equal(t, 0, admissionCount(t, db,
			`SELECT count(*) FROM runs WHERE run_id = $1`, "run-crash-2"),
			"阻塞中的准入不得留下半截账本行")

		require.NoError(t, recovery.Commit())
		token, err := store.StartRun(ctx, next, core.RunResourceManifest{},
			admissionWorker("worker-b"), core.CallerAdmission{}, next.RunID)
		require.NoError(t, err, "恢复提交、旧 Run 已终态且租约已释放后，新准入必须放行")
		require.False(t, token.IsZero())
	})

	t.Run("commit-leaves-old-run-active", func(t *testing.T) {
		ctx := context.Background()
		db := newTxTestDB(t)
		store := seedCrashedRun(t, db)

		// 恢复事务只持锁不改变旧 Run 状态就提交：提交后准入按旧状态判 409。
		recovery, err := db.pool.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = recovery.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`,
			sessionAdvisoryKey("s-us24-recover"))
		require.NoError(t, err)
		require.NoError(t, recovery.Commit())

		next := admissionRunRecord("run-crash-3", "s-us24-recover", core.LocalAdminCLIPrincipal())
		_, err = store.StartRun(ctx, next, core.RunResourceManifest{},
			admissionWorker("worker-b"), core.CallerAdmission{}, next.RunID)
		var busy *core.SessionBusyError
		require.ErrorAs(t, err, &busy, "恢复未收尾旧 Run 时，同会话准入必须 409")
		require.Equal(t, 0, admissionCount(t, db,
			`SELECT count(*) FROM runs WHERE run_id = $1`, "run-crash-3"))
	})
}

// TestStartRun_LeaseCreatedInSameTx 守 FR-24-03 / 模块卡 §3.1.2：Run 行、清单、引用与
// run 租约**同一事务**创建，任一步失败整笔回滚（零占用）。
// 逆改点：把租约插入拆到事务外 → 冲突用例里 Run 行留下而红；租约缺 `claimed` 事件 →
// 事件断言红。
func TestStartRun_LeaseCreatedInSameTx(t *testing.T) {
	ctx := context.Background()
	db := newTxTestDB(t)
	store := NewRunStoreWithLeaseTTL(db, time.Minute)
	worker := admissionWorker("worker-sametx")

	rec := admissionRunRecord("run-sametx", "", core.LocalAdminCLIPrincipal())
	token, err := store.StartRun(ctx, rec, core.RunResourceManifest{}, worker,
		core.CallerAdmission{}, rec.RunID)
	require.NoError(t, err)
	require.Equal(t, core.LeaseToken{
		Kind: core.LeaseRun, ResourceID: "run-sametx",
		OwnerWorkerID: "worker-sametx", OwnershipEpoch: 1,
	}, token)

	var owner string
	var epoch, acquired, renewed, expires int64
	var released sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		SELECT owner_worker_id, ownership_epoch, acquired_at, renewed_at, expires_at, released_at
		  FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(core.LeaseRun), "run-sametx").
		Scan(&owner, &epoch, &acquired, &renewed, &expires, &released))
	require.Equal(t, "worker-sametx", owner)
	require.Equal(t, int64(1), epoch, "首次领取代次为 1")
	require.Equal(t, acquired, renewed, "领取时 renewed_at 与 acquired_at 同源")
	require.Equal(t, time.Minute.Milliseconds(), expires-acquired, "expires_at = acquired_at + ttl")
	require.False(t, released.Valid)

	var event, eventOwner string
	var eventEpoch int64
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		SELECT event, owner_worker_id, ownership_epoch FROM lease_events
		 WHERE kind = $1 AND resource_id = $2`, string(core.LeaseRun), "run-sametx").
		Scan(&event, &eventOwner, &eventEpoch))
	require.Equal(t, "claimed", event, "首次领取必须记 claimed 事件")
	require.Equal(t, "worker-sametx", eventOwner)
	require.Equal(t, int64(1), eventEpoch)

	// 反向判据：租约插入失败 → 已插入的 Run 行整笔回滚。预占同一 (kind, resource_id)
	// 的租约行，再让 StartRun 对同一 run_id 建行：runs 插入成功、租约撞主键。
	nowMS := time.Now().UTC().UnixMilli()
	_, err = db.pool.ExecContext(ctx, `
		INSERT INTO execution_leases
		  (kind, resource_id, owner_worker_id, ownership_epoch, acquired_at, renewed_at, expires_at)
		VALUES ($1, $2, 'ghost', 1, $3, $3, $3)`,
		string(core.LeaseRun), "run-sametx-conflict", nowMS)
	require.NoError(t, err)
	conflict := admissionRunRecord("run-sametx-conflict", "", core.LocalAdminCLIPrincipal())
	_, err = store.StartRun(ctx, conflict, core.RunResourceManifest{}, worker,
		core.CallerAdmission{}, conflict.RunID)
	require.Error(t, err, "租约插入失败必须整笔回滚")
	require.Equal(t, 0, admissionCount(t, db,
		`SELECT count(*) FROM runs WHERE run_id = $1`, "run-sametx-conflict"),
		"租约失败后 Run 行不得留下（同一事务）")
	require.Equal(t, 0, admissionCount(t, db,
		`SELECT count(*) FROM run_resource_manifests WHERE run_id = $1`, "run-sametx-conflict"))
}

// TestStartRun_TaskQuotaAccountedInSameTx 守 Q21 / FR-24-33：任务次数记账与准入、Run
// 写入、租约创建同一事务；被拒整笔回滚（不扣额度）；重复准入不得二次扣费。
// 逆改点：把记账移出事务（先扣后插）→ 租约冲突用例里 usage 仍 +1 而红；
// 回放判定放宽成"任意状态都接受" → 同一 run_id 二次准入后保留行数变 2 而红。
func TestStartRun_TaskQuotaAccountedInSameTx(t *testing.T) {
	ctx := context.Background()
	db := newTxTestDB(t)
	// 固定句柄时钟：周期起点与 usage 行断言必须可复算（opNow → UTC 当日 00:00）。
	db.now = func() time.Time { return opNow }
	store := NewRunStoreWithLeaseTTL(db, time.Minute)
	principal := us24CallerPrincipal("c1")
	admission := core.CallerAdmission{Enabled: true, MaxConcurrent: 10}
	quotaSetLimit(t, db, "c1", callers.Limit{TasksPerPeriod: 5, MaxConcurrent: 10})
	worker := admissionWorker("worker-quota")

	rec := admissionRunRecord("run-quota-1", "", principal)
	_, err := store.StartRun(ctx, rec, core.RunResourceManifest{}, worker, admission, rec.RunID)
	require.NoError(t, err)

	var reservationID, class, state string
	var amount, actual int64
	var runID sql.NullString
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		SELECT reservation_id, class, state, amount, actual, run_id
		  FROM quota_reservations WHERE caller_id = $1 AND idempotency_key = $2`,
		"c1", "run:run-quota-1:admit").
		Scan(&reservationID, &class, &state, &amount, &actual, &runID))
	require.Equal(t, "run", class)
	require.Equal(t, "committed", state, "记账与准入同事务：提交时已是终态")
	require.Equal(t, int64(1), amount)
	require.Equal(t, int64(1), actual)
	require.True(t, runID.Valid)
	require.Equal(t, "run-quota-1", runID.String)

	used, held, overage := quotaLedgerRow(t, db, "c1", usageKindTasks, quotaUTCDayStartMS)
	require.Equal(t, int64(1), used, "准入提交即计入已用")
	require.Zero(t, held, "同一事务内已结算，不留下预留")
	require.Zero(t, overage)

	var ledgerReservation sql.NullString
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT reservation_id FROM runs WHERE run_id = $1`, "run-quota-1").Scan(&ledgerReservation))
	require.True(t, ledgerReservation.Valid)
	require.Equal(t, reservationID, ledgerReservation.String, "账本行必须挂上预留标识")

	// 同一 run_id 的重复准入：回放识别出已提交的预留 → 不再扣费；Run 行主键冲突使
	// 整笔回滚。usage 仍是 1，预留仍是 1 行。
	_, err = store.StartRun(ctx, rec, core.RunResourceManifest{}, worker, admission, rec.RunID)
	require.Error(t, err, "同一 run_id 不得二次准入")
	require.Equal(t, 1, quotaReservationCount(t, db, "c1"), "重复准入不得产生第二条预留")
	used, held, _ = quotaLedgerRow(t, db, "c1", usageKindTasks, quotaUTCDayStartMS)
	require.Equal(t, int64(1), used, "重复准入不得二次扣费")
	require.Zero(t, held)

	// 额度为零：fail-closed 拒绝新准入，且整笔零占用。
	quotaSetLimit(t, db, "c2", callers.Limit{TasksPerPeriod: 0, MaxConcurrent: 10})
	denied := admissionRunRecord("run-quota-2", "", us24CallerPrincipal("c2"))
	_, err = store.StartRun(ctx, denied, core.RunResourceManifest{}, worker, admission, denied.RunID)
	var exhausted *callers.QuotaExhaustedError
	require.ErrorAs(t, err, &exhausted, "任务额度耗尽必须失败关闭")
	require.Equal(t, 0, admissionCount(t, db,
		`SELECT count(*) FROM runs WHERE run_id = $1`, "run-quota-2"), "被拒的 Run 零占用")
	require.Equal(t, 0, admissionCount(t, db,
		`SELECT count(*) FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(core.LeaseRun), "run-quota-2"))
	require.Equal(t, 0, quotaReservationCount(t, db, "c2"), "被拒的准入不得留下预留行")
}

// TestRun_CallerConcurrencyAcrossWorkers 守 FR-24-08 / FR-24-09 / FR-24-34：
// 调用方并发上限跨 Worker 生效（数据库计数，不是进程内名额表）；一个 Run 收尾为终态后
// 名额立刻释放；被拒的准入零占用。
// 逆改点：把并发计数改回进程内表 → 第二个 Worker 看不到第一个 Worker 的 running 行而红。
func TestRun_CallerConcurrencyAcrossWorkers(t *testing.T) {
	ctx := context.Background()
	db := newTxTestDB(t)
	db.now = func() time.Time { return opNow }
	quotaSetLimit(t, db, "c1", callers.Limit{TasksPerPeriod: 100, MaxConcurrent: 1})
	admission := core.CallerAdmission{Enabled: true, MaxConcurrent: 1}
	principal := us24CallerPrincipal("c1")

	// 两个 RunStore 实例模拟两个 Worker 进程：它们只共享数据库。
	workerAStore := NewRunStoreWithLeaseTTL(db, time.Minute)
	workerBStore := NewRunStoreWithLeaseTTL(db, time.Minute)

	first := admissionRunRecord("run-conc-1", "s-us24-conc-a", principal)
	token, err := workerAStore.StartRun(ctx, first, core.RunResourceManifest{},
		admissionWorker("worker-a"), admission, first.RunID)
	require.NoError(t, err)

	second := admissionRunRecord("run-conc-2", "s-us24-conc-b", principal)
	_, err = workerBStore.StartRun(ctx, second, core.RunResourceManifest{},
		admissionWorker("worker-b"), admission, second.RunID)
	require.ErrorIs(t, err, core.ErrCallerConcurrencyLimited,
		"第二个 Worker 必须看到第一个 Worker 的 running 行")
	require.Equal(t, 0, admissionCount(t, db,
		`SELECT count(*) FROM runs WHERE run_id = $1`, "run-conc-2"), "被拒的 Run 零占用")
	require.Equal(t, 0, admissionCount(t, db,
		`SELECT count(*) FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(core.LeaseRun), "run-conc-2"))
	require.Equal(t, 1, quotaReservationCount(t, db, "c1"), "被拒的准入不得扣任务次数")

	// 终态提交后并发计数（按 status='running'）立刻下降：另一 Worker 的名额恢复。
	applied, err := workerAStore.FinishRun(ctx, "run-conc-1", token,
		core.StatusCompleted, "done", 1, 0, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, applied)

	_, err = workerBStore.StartRun(ctx, second, core.RunResourceManifest{},
		admissionWorker("worker-b"), admission, second.RunID)
	require.NoError(t, err, "前一个 Run 终态后名额必须恢复")
}

// TestQuota_PeriodBoundaryAcrossWorkers 守 FR-24-09：周期配额按数据库记账跨 Worker
// 生效，周期起点由 `caller_limits.period/timezone` 与数据库时钟共同决定——同一个
// 周期内两个 Worker 记到同一行，跨过周期边界记到新行，互不串账。
// 逆改点：把周期起点改成进程启动时刻或按 Worker 各存一份 → 跨边界断言红。
func TestQuota_PeriodBoundaryAcrossWorkers(t *testing.T) {
	ctx := context.Background()
	db := newTxTestDB(t)
	dayOne := opNow
	db.now = func() time.Time { return dayOne }
	quotaSetLimit(t, db, "c1", callers.Limit{TasksPerPeriod: 100, MaxConcurrent: 10})
	admission := core.CallerAdmission{Enabled: true, MaxConcurrent: 10}
	principal := us24CallerPrincipal("c1")

	workerAStore := NewRunStoreWithLeaseTTL(db, time.Minute)
	workerBStore := NewRunStoreWithLeaseTTL(db, time.Minute)

	run := func(store *RunStore, runID, workerID string) {
		t.Helper()
		rec := admissionRunRecord(runID, "", principal)
		_, err := store.StartRun(ctx, rec, core.RunResourceManifest{},
			admissionWorker(workerID), admission, rec.RunID)
		require.NoError(t, err, "准入 %s 必须成功", runID)
	}
	run(workerAStore, "run-period-1", "worker-a")
	run(workerBStore, "run-period-2", "worker-b")

	dayOneStart := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC).UnixMilli()
	dayTwoStart := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC).UnixMilli()
	used, held, _ := quotaLedgerRow(t, db, "c1", usageKindTasks, dayOneStart)
	require.Equal(t, int64(2), used, "两个 Worker 记到同一周期行")
	require.Zero(t, held)

	// 跨过 UTC 日边界：第二个 Worker 的新准入记到新周期行，旧行不变。
	dayTwo := dayOne.Add(24 * time.Hour)
	db.now = func() time.Time { return dayTwo }
	run(workerBStore, "run-period-3", "worker-b")

	used, _, _ = quotaLedgerRow(t, db, "c1", usageKindTasks, dayOneStart)
	require.Equal(t, int64(2), used, "旧周期行不得被跨边界写入改变")
	used, held, overage := quotaLedgerRow(t, db, "c1", usageKindTasks, dayTwoStart)
	require.Equal(t, int64(1), used, "跨边界后的准入记到新周期行")
	require.Zero(t, held)
	require.Zero(t, overage)

	var periodStart int64
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		SELECT period_start FROM quota_reservations
		 WHERE caller_id = 'c1' AND idempotency_key = 'run:run-period-3:admit'`).Scan(&periodStart))
	require.Equal(t, dayTwoStart, periodStart, "预留行与 usage 行同一周期起点")
}
