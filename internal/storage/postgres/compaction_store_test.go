//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `CompactionStore` 的**真库**判据（颗粒度文档 §3.2 `us16-pg` 行 / T088）。
//
// 为什么必须真库：这里的两条收敛机制都只存在于服务端，内存替身看不见——
//   - scope 级 `pg_advisory_xact_lock` 是跨连接、跨进程的串行化点，假 store 里的
//     `sync.Mutex` 证不了"另一个句柄也等这把锁"；
//   - 两条部分唯一索引（`... WHERE status = 'committed'`）决定"失效后同范围重建"
//     能不能落，假 store 里没有索引，红不起来。
//
// 每个用例头部写明**逆改点**：实现改回去必然变红（判别力口径见 module-doc-gen 规则 8）。
//
// These are the database-backed cases for CompactionStore. Both convergence mechanisms —
// the scope-level advisory lock and the two partial unique indexes — exist only on the
// server, so an in-memory stand-in cannot tell a correct implementation from a wrong one.

// pgBase 是夹具时间基线：条目按 `(created_at, compaction_id)` 排序，时间差让顺序可判。
// pgBase is the fixture time base; spacing makes the ordering deterministic.
var pgBase = time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

// pgStore 是 `CompactionStore` 加真库句柄的夹具：用例既要经端口读写，也要直接对照行。
// pgStore pairs the store with the handle so cases can assert on raw rows too.
type pgStore struct {
	*CompactionStore
	db    *DB
	scope compaction.Scope // 夹具的会话范围：`entry` 用它与端口调用对齐
}

// newPGStore 建隔离 schema、种会话 `s1` 与 Run `r1`（`scope_session_id` /
// `trigger_run_id` 两个外键的目标），返回已迁移的库上的 store 与 ctx。
// newPGStore migrates an isolated schema and seeds the session and run rows the foreign
// keys require.
func newPGStore(t *testing.T) (*pgStore, context.Context) {
	t.Helper()
	ctx := context.Background()
	db := newTxTestDB(t) // 已种调用方 c1，并建好隔离 schema 与扩展 search_path

	_, err := db.pool.ExecContext(ctx, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at, caller_id)
		VALUES ('s1', 'demo', 'web', 'u1', 'active', 0, $1, $1, 'c1')`, pgBase.UnixMilli())
	require.NoError(t, err)

	// 最省事的授权组合：admin-builtin + local-admin + 无 key + admin-cli，三条 CHECK 全过。
	// The cheapest CHECK-satisfying authorisation triple for a seeded run.
	_, err = db.pool.ExecContext(ctx, `INSERT INTO runs
		(run_id, agent, channel, user_id, trigger, status, started_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ('r1', 'demo', 'web', 'u1', 'user', 'running', $1,
		        'local-admin', 'admin-builtin', NULL, 'admin-cli')`, pgBase.UnixMilli())
	require.NoError(t, err)

	store := NewCompactionStore(db, slog.New(slog.DiscardHandler))
	return &pgStore{CompactionStore: store, db: db, scope: compaction.Scope{SessionID: "s1"}}, ctx
}

// entry 造一条形状完整的条目（默认 committed、绑定夹具的会话范围）：标识 / 代次 /
// 父节点 / 覆盖起点 / 切点 / 保留起点。其余必填列给固定值，用例只关心链形状。
// entry builds a well-formed entry bound to the fixture scope; the fields under test are
// the chain shape.
func (h *pgStore) entry(id string, epoch int, parent, coveredFrom, cut, firstKept string) compaction.CompactionEntry {
	return compaction.CompactionEntry{
		CompactionID:         id,
		Scope:                h.scope,
		ChainEpoch:           epoch,
		ParentCompactionID:   parent,
		TriggerRunID:         "r1",
		AttemptID:            "att-" + id,
		CoveredFromMessageID: coveredFrom,
		CutMessageID:         cut,
		FirstKeptMessageID:   firstKept,
		StrategyVersion:      "strategy-v1",
		PromptVersion:        "prompt-v1",
		Model:                "test-model",
		Status:               compaction.StatusCommitted,
		Summary:              "摘要-" + id,
	}
}

// seedChain 直接写已提交条目（**绕过** `Append`）：要造出"祖先已失效而后代仍 committed"
// 这类正常路径写不出来的状态，就得绕开端口的校验。
// seedChain writes committed rows directly, bypassing Append: some states under test
// (a committed descendant of a stale ancestor) cannot arise through the port.
func (h *pgStore) seedChain(t *testing.T, scope compaction.Scope, entries ...compaction.CompactionEntry) {
	t.Helper()
	for i, e := range entries {
		e.Scope = scope
		e.CreatedAt = pgBase.Add(time.Duration(i+1) * time.Second)
		require.NoError(t, insertCompactionEntry(context.Background(), h.db.pool, e),
			"种子条目 %s 写入失败", e.CompactionID)
	}
}

// rows 读该 scope 的全部条目（所有状态）。
// rows loads every row of the scope, whatever its status.
func (h *pgStore) rows(t *testing.T, scope compaction.Scope) []compaction.CompactionEntry {
	t.Helper()
	out, err := loadCompactionRows(context.Background(), h.db.pool, scope)
	require.NoError(t, err)
	return out
}

// findEntry 按标识在结果集里取一条（不存在即让用例失败）。
// findEntry picks one entry by id and fails when it is absent.
func findEntry(t *testing.T, entries []compaction.CompactionEntry, id string) compaction.CompactionEntry {
	t.Helper()
	for _, e := range entries {
		if e.CompactionID == id {
			return e
		}
	}
	t.Fatalf("条目 %s 不在结果集里（实际 %d 条）", id, len(entries))
	return compaction.CompactionEntry{}
}

// beginScopeHolder 开一个外部事务并**持有** scope 锁，模拟"上一次追加仍在服务端执行"。
// 它的存在让"锁内重读"可被观测：调用方的 Append 必须停在这把锁上，等它提交。
// The holder stands in for an append still running on another connection; the caller's
// Append must block on the very same advisory lock until it commits.
func (h *pgStore) beginScopeHolder(t *testing.T, scope compaction.Scope) *sql.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := h.db.pool.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, compaction.ScopeAdvisoryKey(scope))
	require.NoError(t, err)
	return tx
}

// TestCompactionStore_AppendRejectsAmbiguousScope —— 会话与 Run 标识同时给出必须**拒绝**
// 且零写入：链作用域恰有一个身份，两者同填说明调用方把触发 Run 误当成了链身份。
//
// 逆改点：删掉 `normalizeCompactionEntry` 的互斥判据（或只依赖 DDL 的
// `num_nonnulls = 1` 而不先报语义错误）→ 本用例拿到 nil 错误。
// The append path must fail closed on an ambiguous scope; the row count stays zero.
func TestCompactionStore_AppendRejectsAmbiguousScope(t *testing.T) {
	h, ctx := newPGStore(t)
	entry := h.entry("c-ambiguous", 1, "", "m1", "m2", "m3")
	entry.Scope = compaction.Scope{SessionID: "s1", RunID: "r1"}

	_, err := h.Append(ctx, entry)
	require.ErrorContains(t, err, "两者互斥")

	var rows int
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM compaction_entries`).Scan(&rows))
	require.Zero(t, rows, "被拒绝的追加不得留下任何行")
}

// —— 引擎 + 真存储的端到端夹具：把 `compaction.Compactor` 真正接上 `CompactionStore` ——
// 会话阅读 / 摘要 / 校验三个端口用最小替身，只有**存储**是真的：本用例要判的正是
// "引擎写进真库的那一行长什么样"，替身存储会把这个缝抹平。

// engineStubMessages 按 seq 递增返回固定会话消息（两轮问答）。
// engineStubMessages serves a fixed transcript in seq order.
type engineStubMessages struct{ msgs []compaction.StoredMessage }

func (s *engineStubMessages) ListBySession(_ context.Context, _ string, afterSeq int64, _ int) ([]compaction.StoredMessage, error) {
	var out []compaction.StoredMessage
	for _, m := range s.msgs {
		if m.Seq > afterSeq {
			out = append(out, m)
		}
	}
	return out, nil
}

// engineStubSummarizer 返回固定摘要正文（远低于上限，不会触发重生成分支）。
// engineStubSummarizer returns a fixed body well under the limit.
type engineStubSummarizer struct{ text string }

func (s *engineStubSummarizer) Summarize(context.Context, compaction.SummaryInput) (compaction.SummaryOutput, error) {
	return compaction.SummaryOutput{Text: s.text, Model: "test-model"}, nil
}

// engineStubValidity 对任何来源都放行（本用例不判失效语义）。
// engineStubValidity admits every source; validity semantics are judged elsewhere.
type engineStubValidity struct{}

func (engineStubValidity) Check(context.Context, core.Principal, core.EvidenceRef) (compaction.Validity, error) {
	return compaction.Validity{Valid: true}, nil
}

// engineStubHistory 只满足无状态范围的必填端口；本用例走会话范围，不会读到它。
// engineStubHistory only satisfies the mandatory port; this case never reads it.
type engineStubHistory struct{}

func (engineStubHistory) Messages() []core.Message { return nil }

// TestCompactionStore_EngineSessionAppendWritesSessionOnlyScope —— 真引擎 + 真存储：
// 经 `compaction.NewCompactor` 走完 预算 → 切点 → 摘要 → 追加 全链路，落库条目的链作用域
// 必须**只有** `scope_session_id`，`run_id` 只出现在 `trigger_run_id`。
//
// 为什么必须在真库跑：互斥判据由服务端 `num_nonnulls(scope_session_id, scope_run_id) = 1`
// 兜底；内存替身的 `normalizeEntry` 会把空 scope 补成 `{SessionID:"s1"}`，把"引擎把触发
// Run 混进链身份"这个缺陷整条掩盖（2026-10-31 真实实例故障即此：摘要成功生成后追加被
// 拒，整次运行失败；单测因替身补全而全绿）。
//
// 逆改点：把 `Compact` 的作用域构造改回 `Scope{SessionID: req.SessionID, RunID: req.RunID}`
// → Append 被真库 CHECK 拒绝，本用例拿到错误而不是 committed 条目。
// The real engine must write a session-only chain scope through the real store.
func TestCompactionStore_EngineSessionAppendWritesSessionOnlyScope(t *testing.T) {
	h, ctx := newPGStore(t)

	// `trigger_run_id` 有外键：触发 Run `r2` 必须先存在（r1 是夹具的既有 Run）。
	// The trigger run must exist because trigger_run_id is a foreign key.
	_, err := h.db.pool.ExecContext(ctx, `INSERT INTO runs
		(run_id, agent, channel, user_id, trigger, status, started_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ('r2', 'demo', 'web', 'u1', 'user', 'running', $1,
		        'local-admin', 'admin-builtin', NULL, 'admin-cli')`, pgBase.UnixMilli()+1)
	require.NoError(t, err)

	messages := []compaction.StoredMessage{
		{MessageID: "m1", Seq: 1, Role: core.RoleUser, Content: "目标"},
		{MessageID: "m2", Seq: 2, Role: core.RoleAssistant, Content: "收到"},
		{MessageID: "m3", Seq: 3, Role: core.RoleUser, Content: "继续"},
		{MessageID: "m4", Seq: 4, Role: core.RoleAssistant, Content: "好"},
	}
	engine := compaction.NewCompactor(compaction.CompactorDeps{
		Messages:   &engineStubMessages{msgs: messages},
		Store:      h.CompactionStore,
		Summarizer: &engineStubSummarizer{text: "目标与确认：继续执行。"},
		Validator:  engineStubValidity{},
		Clock:      func() time.Time { return pgBase.Add(time.Minute) },
		Policy: compaction.Policy{
			TriggerRatio:      0.2,
			SafetyMarginRatio: 0.1,
			KeepRecentGroups:  1,
			SummaryMaxTokens:  1024,
			SummaryTimeout:    5 * time.Second,
			StrategyVersion:   "strategy-v1",
			PromptVersion:     "prompt-v1",
			SummaryModel:      "test-model",
		},
	})

	res, err := engine.Compact(ctx, core.CompactRequest{
		Principal: core.Principal{
			Kind:     core.PrincipalCaller,
			CallerID: "c1",
			KeyID:    "k1",
			Scopes:   []string{core.ScopeSessionsRead, core.ScopeSessionsWrite},
			Actor:    core.ActorCaller,
		},
		RunID:            "r2",
		SessionID:        "s1",
		AttemptID:        "att-engine-1",
		Budget:           core.ContextBudget{ContextTokens: 300, OutputReserve: 50, EstimatorVersion: core.EstimateVersion},
		Fixed:            core.FixedParts{Total: 200},
		History:          engineStubHistory{},
		EstimatorVersion: core.EstimateVersion,
	})
	require.NoError(t, err)
	require.False(t, res.Degraded, "触发与摘要都应成功，不得降级（reason=%s）", res.ReasonCode)
	require.NotNil(t, res.Entry)

	entries := h.rows(t, compaction.Scope{SessionID: "s1"})
	require.Len(t, entries, 1)
	committed := entries[0]
	require.Equal(t, compaction.StatusCommitted, committed.Status)
	require.Equal(t, "s1", committed.Scope.SessionID)
	require.Empty(t, committed.Scope.RunID, "链作用域只含 session_id；run_id 只作触发记录")
	require.Equal(t, "r2", committed.TriggerRunID)

	var runScope sql.NullString
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT scope_run_id FROM compaction_entries WHERE compaction_id = $1`,
		committed.CompactionID).Scan(&runScope))
	require.False(t, runScope.Valid, "落库的 scope_run_id 必须为 NULL")
}

// TestCompactionStore_AppendSerializesPerScope 同一 scope 的两个不同切点根只能落一个。
//
// 逆改点：去掉 scope 锁（或把锁内重读改成锁外读）→ 两个追加都看不到对方，都插入，
// `committed` 行出现两条重叠根；用例按"恰好一条已提交 + 只留一行"判红。
// 唯一索引挡不住这种分叉——它只排斥**完全相等**的覆盖范围，而两条根的切点不同。
//
// Two concurrent roots with different cuts: exactly one may commit. The partial unique
// index cannot catch this, because the coverage differs; only the scope lock can.
func TestCompactionStore_AppendSerializesPerScope(t *testing.T) {
	h, ctx := newPGStore(t)
	scope := compaction.Scope{SessionID: "s1"}

	candidates := []compaction.CompactionEntry{
		h.entry("c-root-a", 1, "", "m1", "m2", "m3"),
		h.entry("c-root-b", 1, "", "m1", "m4", "m5"), // 不同切点：唯一索引挡不住
	}
	var wg sync.WaitGroup
	results := make([]compaction.AppendResult, len(candidates))
	errs := make([]error, len(candidates))
	for i, cand := range candidates {
		wg.Add(1)
		go func(i int, cand compaction.CompactionEntry) {
			defer wg.Done()
			results[i], errs[i] = h.Append(ctx, cand)
		}(i, cand)
	}
	wg.Wait()

	committed := 0
	for i, res := range results {
		require.NoError(t, errs[i])
		switch res.Outcome {
		case compaction.AppendCommitted:
			committed++
		case compaction.AppendSuperseded:
			require.NotNil(t, res.Head, "被超越的追加必须带回新头，否则调用方无法重算")
		case compaction.AppendRebuildRoot:
			// 两个都"无头"时也是合法结局（另一方尚未落），仍只许一条成功。
		default:
			t.Fatalf("并发追加出现预期之外的结局 %q", res.Outcome)
		}
	}
	require.Equal(t, 1, committed, "同一 scope 的两个不同切点根只能有一个成功")
	require.Len(t, h.rows(t, scope), 1, "未写入的一方不得留下行")
}

// TestCompactionStore_RestartWithChangedCutWaitsThenRebases 在途旧追加 + 重启后切点变化。
//
// 场景：重启后的调用方按**旧头**算好了切点，但上一次的追加还持着 scope 锁在服务端跑。
// 它必须**等**锁，锁内重读后按当前头判定被超越，而不是拿唯一索引当收敛直接插入。
//
// 逆改点：把锁内重读改成锁外（或先读后插、不取锁）→ 调用方看不到在途那条，
// 直接插入第二条重叠根，用例按"结局为 AppendSuperseded 且只留两行"判红。
//
// The restarted caller computed its cut from the old head while an earlier append is
// still running. It must wait for the lock and rebase, never insert a second root.
func TestCompactionStore_RestartWithChangedCutWaitsThenRebases(t *testing.T) {
	h, ctx := newPGStore(t)
	scope := compaction.Scope{SessionID: "s1"}
	h.seedChain(t, scope, h.entry("c-a", 1, "", "m1", "m2", "m3"))

	// 在途的旧追加：持锁写入新头 c-b（切点 m4、保留起点 m5），尚未提交。
	holder := h.beginScopeHolder(t, scope)
	inFlight := h.entry("c-b", 1, "c-a", "m3", "m4", "m5")
	inFlight.Scope = scope
	inFlight.CreatedAt = pgBase.Add(10 * time.Second)
	require.NoError(t, insertCompactionEntry(context.Background(), holder, inFlight))

	// 重启后的调用方（新句柄、新 store）：按读到的旧头 c-a 算切点，与在途那条不同。
	restarted := NewCompactionStore(reopenDB(t, h.db), slog.New(slog.DiscardHandler))
	cand := h.entry("c-b2", 1, "c-a", "m3", "m5", "m6")

	done := make(chan compaction.AppendResult, 1)
	fail := make(chan error, 1)
	go func() {
		// 独立超时：锁一直被持有时用例按超时失败，而不是挂住整个测试进程。
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		res, err := restarted.Append(waitCtx, cand)
		fail <- err
		done <- res
	}()

	time.Sleep(200 * time.Millisecond)
	select {
	case res := <-done:
		t.Fatalf("追加没有等在途事务的 scope 锁（已提前返回 %q）——锁没取或取错了键", res.Outcome)
	default:
	}

	require.NoError(t, holder.Commit())

	var res compaction.AppendResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("在途事务提交后追加仍未返回——锁没有被释放或被等错")
	}
	require.NoError(t, <-fail)
	require.Equal(t, compaction.AppendSuperseded, res.Outcome,
		"锁内重读到的头已前进，本次切点作废")
	require.NotNil(t, res.Head)
	require.Equal(t, "c-b", res.Head.CompactionID, "带回的必须是当前有效头")
	require.Len(t, h.rows(t, scope), 2, "未写入的一方不得留下行")
}

// TestCompactionStore_SameCoverageRebuildAfterStale 失效后同覆盖范围重建不被旧 stale 行挡住。
//
// 逆改点：唯一索引条件不带 `status = 'committed'` → 新根撞上旧 `stale` 行的唯一键，
// `Append` 报错，而用例要求落库成功、`FindCommitted` 命中新行。
//
// The rebuild root reuses the stale row's exact coverage; only the committed-scoped
// partial index lets it land.
func TestCompactionStore_SameCoverageRebuildAfterStale(t *testing.T) {
	h, ctx := newPGStore(t)
	scope := compaction.Scope{SessionID: "s1"}
	h.seedChain(t, scope, h.entry("c-a", 1, "", "m1", "m2", "m3"))
	require.NoError(t, h.MarkStale(ctx, "c-a", "source_revoked"))

	rebuilt := h.entry("c-a2", 2, "", "m1", "m2", "m3") // 覆盖范围与已失效的 c-a 完全相同
	res, err := h.Append(ctx, rebuilt)
	require.NoError(t, err, "旧 stale 行不得挡住同范围的重建根")
	require.Equal(t, compaction.AppendCommitted, res.Outcome)
	require.Equal(t, 2, res.Entry.ChainEpoch)

	hit, ok, err := h.FindCommitted(ctx, scope, "m1", "m2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "c-a2", hit.CompactionID, "命中的必须是新一代 committed 行，不是 stale 版本")

	rows := h.rows(t, scope)
	require.Len(t, rows, 2, "旧行保留（审计），新行追加")
	require.Equal(t, compaction.StatusStale, findEntry(t, rows, "c-a").Status)
	require.Equal(t, compaction.StatusCommitted, findEntry(t, rows, "c-a2").Status)
}

// TestCompactionStore_RecoveryReusesCommittedWithoutSummaryCall 重启后先复用已提交条目。
//
// 逆改点：恢复路径不先按覆盖范围查已提交条目（或查了不认，仍走插入）→ 同覆盖范围再写
// 一行，用例按"返回的仍是原条目、库里仍只有一行、摘要正文未被新值覆盖"判红。
//
// After a restart the same coverage must resolve to the entry that is already committed;
// a second row would mean a second summary call and a second charge.
func TestCompactionStore_RecoveryReusesCommittedWithoutSummaryCall(t *testing.T) {
	h, ctx := newPGStore(t)
	scope := compaction.Scope{SessionID: "s1"}
	seeded := h.entry("c-a", 1, "", "m1", "m2", "m3")
	seeded.Summary = "崩溃前已落库的摘要"
	seeded.IncludedRefs = []core.EvidenceRef{{Kind: "knowledge", ID: "kb-a", VersionID: "v1"}}
	h.seedChain(t, scope, seeded)

	// 模拟进程重启：新句柄、新 store，内存里什么都没有。
	restarted := NewCompactionStore(reopenDB(t, h.db), slog.New(slog.DiscardHandler))

	hit, ok, err := restarted.FindCommitted(ctx, scope, "m1", "m2")
	require.NoError(t, err)
	require.True(t, ok, "崩溃前已提交的覆盖范围必须能命中")
	require.Equal(t, "c-a", hit.CompactionID)
	require.Equal(t, "崩溃前已落库的摘要", hit.Summary)

	// 恢复路径按命中结果直接复用；即便它仍提交一次（新 attempt_id），也不得产生第二行。
	retry := h.entry("c-a-retry", 1, "", "m1", "m2", "m3")
	retry.AttemptID = "att-c-a-retry"
	retry.Summary = "重算出来的第二份摘要"
	res, err := restarted.Append(ctx, retry)
	require.NoError(t, err)
	require.Equal(t, compaction.AppendCommitted, res.Outcome)
	require.Equal(t, "c-a", res.Entry.CompactionID, "同覆盖范围必须复用已提交条目")
	require.Equal(t, "崩溃前已落库的摘要", res.Entry.Summary, "回读的是库里那份，不是本次重算的")
	require.Len(t, h.rows(t, scope), 1, "复用不得留下第二行")
}

// TestCompactionStore_MarkStaleCascadesDescendants 失效沿链传播到全部后代。
//
// 逆改点：`MarkStale` 只改目标行 → 两级链 A→B 里 B 仍 `committed`，选头继续指向 B，
// 用例按"B 也是 stale、原因为 `ancestor_stale:c-a`、stale_at 已写"判红。
//
// The cascade must reach every descendant in the same transaction, recording the ancestor
// that failed first; a target-only update leaves the head pointing at a dead branch.
func TestCompactionStore_MarkStaleCascadesDescendants(t *testing.T) {
	h, ctx := newPGStore(t)
	scope := compaction.Scope{SessionID: "s1"}
	h.seedChain(t, scope,
		h.entry("c-a", 1, "", "m1", "m2", "m3"),
		h.entry("c-b", 1, "c-a", "m3", "m4", "m5"))

	require.NoError(t, h.MarkStale(ctx, "c-a", "source_revoked"))

	rows := h.rows(t, scope)
	a := findEntry(t, rows, "c-a")
	b := findEntry(t, rows, "c-b")
	require.Equal(t, compaction.StatusStale, a.Status)
	require.Equal(t, "source_revoked", a.ReasonCode, "目标记调用方给的原因")
	require.False(t, a.StaleAt.IsZero())
	require.Equal(t, compaction.StatusStale, b.Status, "后代必须同事务失效")
	require.Equal(t, "ancestor_stale:c-a", b.ReasonCode, "后代的失效原因指回目标")
	require.False(t, b.StaleAt.IsZero())

	chain, err := h.LatestCommittedChain(ctx, scope)
	require.NoError(t, err)
	require.Empty(t, chain, "整链失效后当前代次已无 committed 链")
}

// TestCompactionStore_RebuildRootAfterStaleChain 无有效头时按换代重建，且只认目标代次。
//
// 逆改点：
//   - 无头时仍按旧头校验（返回 `AppendSuperseded`）→ 重建根永远写不进去；
//   - 接受 `chain_epoch` 未推进的重建根 → 引擎每次重提都撞同一代次的唯一键。
//
// With no head, only a root carrying the next epoch may land; everything else is refused
// without writing, so the engine can rebuild exactly once per generation.
func TestCompactionStore_RebuildRootAfterStaleChain(t *testing.T) {
	h, ctx := newPGStore(t)
	scope := compaction.Scope{SessionID: "s1"}
	h.seedChain(t, scope,
		h.entry("c-a", 1, "", "m1", "m2", "m3"),
		h.entry("c-b", 1, "c-a", "m3", "m4", "m5"))
	require.NoError(t, h.MarkStale(ctx, "c-a", "source_revoked"))

	// ① 引擎按旧代次重提 → 拒绝，不写行；原因码指回"祖先失效"。
	stale := h.entry("c-rebuild-stale", 1, "", "m1", "m2", "m3")
	res, err := h.Append(ctx, stale)
	require.NoError(t, err)
	require.Equal(t, compaction.AppendRebuildRoot, res.Outcome)
	require.Nil(t, res.Head, "换代是无头结论，不得带回被超越的头")
	require.Equal(t, compaction.ReasonEpochRaced, res.ReasonCode, "旧代次重提按代次竞争归类")

	// ② 引擎仍以为有头（带着父节点）→ 同样是换代，原因码按链形状给出。
	orphan := h.entry("c-rebuild-orphan", 2, "c-b", "m5", "m6", "m7")
	res, err = h.Append(ctx, orphan)
	require.NoError(t, err)
	require.Equal(t, compaction.AppendRebuildRoot, res.Outcome)
	require.Equal(t, compaction.ReasonStaleAncestor, res.ReasonCode)

	require.Len(t, h.rows(t, scope), 2, "两次拒绝都不得留下行")

	// ③ 目标代次的重建根 → 落库。
	rebuilt := h.entry("c-root", 2, "", "m1", "m2", "m3")
	res, err = h.Append(ctx, rebuilt)
	require.NoError(t, err)
	require.Equal(t, compaction.AppendCommitted, res.Outcome)
	require.Equal(t, 2, res.Entry.ChainEpoch)
	require.Empty(t, res.Entry.ParentCompactionID)
}

// TestCompactionStore_CommittedDescendantOfStaleIsNotHead 遗留的 committed 后代不是有效头。
//
// 造法：直接 UPDATE 把祖先置 stale（**绕过**级联），留下"祖先 stale、后代 committed"
// 这种正常路径写不出来的状态。
//
// 逆改点：选头只看行状态、不回溯祖先链 → 遗留的 committed 后代被当有效头，
// 引擎以它为父继续追加；用例按"以 c-b 为父的追加被判 RebuildRoot"判红。
//
// A committed descendant whose ancestor is stale must not become the head: the chain is
// broken, so the next append rebuilds instead of silently continuing on a dead branch.
func TestCompactionStore_CommittedDescendantOfStaleIsNotHead(t *testing.T) {
	h, ctx := newPGStore(t)
	scope := compaction.Scope{SessionID: "s1"}
	h.seedChain(t, scope,
		h.entry("c-a", 1, "", "m1", "m2", "m3"),
		h.entry("c-b", 1, "c-a", "m3", "m4", "m5"))

	_, err := h.db.pool.ExecContext(ctx, `UPDATE compaction_entries
		SET status = 'stale', reason_code = 'source_revoked', stale_at = $1
		WHERE compaction_id = 'c-a'`, pgBase.UnixMilli())
	require.NoError(t, err)

	chain, err := h.LatestCommittedChain(ctx, scope)
	require.NoError(t, err)
	require.Len(t, chain, 1)
	require.Equal(t, "c-b", chain[0].CompactionID, "读取侧不隐去断链行：断点必须可见")
	require.ErrorIs(t, compaction.ValidateChain(chain), compaction.ErrChainBroken,
		"遗孤后代过不了链资格，引擎据此走换代重建")

	_, ok, err := h.FindCommitted(ctx, scope, "m3", "m4")
	require.NoError(t, err)
	require.False(t, ok, "祖先失效的同范围条目按未命中处理——不得用于崩溃恢复复用")

	child := h.entry("c-c", 1, "c-b", "m5", "m6", "m7")
	res, err := h.Append(ctx, child)
	require.NoError(t, err)
	require.Equal(t, compaction.AppendRebuildRoot, res.Outcome,
		"遗孤后代不是有效头：以它为父的追加必须被判换代")

	rebuilt := h.entry("c-root", 2, "", "m1", "m2", "m3")
	res, err = h.Append(ctx, rebuilt)
	require.NoError(t, err)
	require.Equal(t, compaction.AppendCommitted, res.Outcome)
}

// TestCompactionStore_MiddleStaleKeepsEpochOpen 中段失效回退到合格祖先，不抬代次。
//
// 逆改点：`MarkStale` 写成整链作废（或 `LatestCommittedChain` 只认最新行）→ A 一起失效、
// `Append` 返回 `AppendRebuildRoot`、代次被抬到 2；或覆盖范围唯一索引缺
// `status = 'committed'` 条件 → 同范围的旧 `stale` 行挡住 c-c 的插入。
//
// Losing a middle entry must fall back to a still-qualified ancestor and continue in the
// same epoch; only losing the root forces a new generation.
func TestCompactionStore_MiddleStaleKeepsEpochOpen(t *testing.T) {
	h, ctx := newPGStore(t)
	scope := compaction.Scope{SessionID: "s1"}
	h.seedChain(t, scope,
		h.entry("c-a", 1, "", "m1", "m2", "m3"),
		h.entry("c-b", 1, "c-a", "m3", "m4", "m5"))

	require.NoError(t, h.MarkStale(ctx, "c-b", "source_revoked"))

	// 断言 1：A 保留 committed（B 的失效不波及祖先），有效链退回 A、仍属原代次。
	rows := h.rows(t, scope)
	require.Equal(t, compaction.StatusCommitted, findEntry(t, rows, "c-a").Status,
		"中段失效不得波及祖先")
	require.Equal(t, compaction.StatusStale, findEntry(t, rows, "c-b").Status)

	a, ok, err := h.FindCommitted(ctx, scope, "m1", "m2")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, a.ChainEpoch)

	chain, err := h.LatestCommittedChain(ctx, scope)
	require.NoError(t, err)
	require.Len(t, chain, 1)
	require.Equal(t, "c-a", chain[0].CompactionID)

	// 断言 2：以 A 为父、在原代次继续追加；覆盖范围与已失效的 B **完全相同**，
	// 因此同时验证"部分唯一索引只约束 committed、旧 stale 行不挡"。
	res, err := h.Append(ctx, h.entry("c-c", 1, "c-a", "m3", "m4", "m5"))
	require.NoError(t, err)
	require.Equal(t, compaction.AppendCommitted, res.Outcome,
		"有合格祖先时不得走 AppendRebuildRoot")
	require.Equal(t, 1, res.Entry.ChainEpoch, "仍留在原代次，不抬代次")
	require.Equal(t, "c-a", res.Entry.ParentCompactionID)
	require.Equal(t, "c-c", res.Entry.CompactionID, "同覆盖范围的旧 stale 行不得挡住新 committed 行")
	require.Equal(t, "m3", res.Entry.CoveredFromMessageID, "覆盖起点仍接在 A 之后")
}

// TestMessageEvidence_WrittenInSameTransaction 来源引用与消息同一事务落库。
//
// 逆改点：引用另起事务 / 先提交消息再补写引用 → 坏引用（空 kind）那条消息**留在库里**
// 而引用缺失，用例按"消息行数为 2、计数仍为 2"判红。
//
// Evidence refs commit with the message. A ref that fails validation must take the message
// row down with it — otherwise a message survives with a truncated reference set.
func TestMessageEvidence_WrittenInSameTransaction(t *testing.T) {
	h, ctx := newPGStore(t)
	store := NewSessionStore(h.db)

	refA := core.EvidenceRef{Kind: "knowledge", ID: "kb-a", VersionID: "v1", ContentHash: "sha256:a"}
	refB := core.EvidenceRef{Kind: "tool", ID: "tool-b", VersionID: "v2"}

	toolID, err := store.AppendMessage(ctx, "s1", "r1", core.Message{
		Role: "tool", Content: "工具结果", ToolCallID: "call-1",
		EvidenceRefs: []core.EvidenceRef{refA, refB, refA}, // 重复引用按集合语义去重
	})
	require.NoError(t, err)
	require.NotEmpty(t, toolID)

	assistantID, err := store.AppendMessage(ctx, "s1", "r1", core.Message{
		Role: "assistant", Content: "基于 A 与 B 的回答",
		EvidenceRefs: []core.EvidenceRef{refA},
	})
	require.NoError(t, err)
	require.NotEmpty(t, assistantID)

	// 坏引用：空 kind 必须 fail-closed，且整条消息一起回滚。
	_, err = store.AppendMessage(ctx, "s1", "r1", core.Message{
		Role: "tool", Content: "不该落库的消息", ToolCallID: "call-2",
		EvidenceRefs: []core.EvidenceRef{{Kind: "", ID: "kb-x", VersionID: "v1"}},
	})
	require.Error(t, err, "空 kind 的来源引用必须被拒绝")

	var messages, refs, counted int
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM messages WHERE session_id = 's1'`).Scan(&messages))
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM message_evidence_refs`).Scan(&refs))
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT message_count FROM sessions WHERE session_id = 's1'`).Scan(&counted))

	require.Equal(t, 2, messages, "坏引用那条消息不得留下行——引用与消息同一事务")
	require.Equal(t, 3, refs, "两条消息的引用合计三条（同一消息内重复引用去重）")
	require.Equal(t, 2, counted, "失败的那次不得计数")

	// tool 与 assistant 两类都带引用：压缩的来源集合取的是它们，缺一类就偏窄。
	var toolRefs, assistantRefs int
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM message_evidence_refs WHERE message_id = $1::bigint`, toolID).Scan(&toolRefs))
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM message_evidence_refs WHERE message_id = $1::bigint`, assistantID).Scan(&assistantRefs))
	require.Equal(t, 2, toolRefs)
	require.Equal(t, 1, assistantRefs)

	var hash string
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT content_hash FROM message_evidence_refs WHERE message_id = $1::bigint AND ref_id = 'kb-a'`,
		toolID).Scan(&hash))
	require.Equal(t, "sha256:a", hash, "内容哈希随引用原样落库")
}

// TestCompactionStore_ListCurrentChainReadsWholeGeneration 是管理台压缩面板的**读法**
// 判据（T089）：只回当前 `chain_epoch`、三种状态都在、按 `(created_at, id)` 升序、
// `limit` 截最前 N 条、另一种 scope 不同范围串味。
//
// 为什么必须真库：代次（epoch）是"该 scope 全部行（含 failed / stale）的最大值"，
// 假 store 里是内存切片，测不出"代次由落库状态决定"；`stale_at` 的非空约束也只有
// 真表才判。
//
// 逆改点：把"当前代次"改成"只回 committed"→ 旧代次行混入 / failed 行消失，标识
// 序列断言红；排序改按父到子 → 中间那条 stale 行位置变化，断言红；去掉 limit 截断
// → 长度断言红；去掉 scope 过滤 → Run 范围读到会话行，`nil` 断言红。
//
// The management-panel read path: current generation only, every status, ascending by
// created_at, first-N truncation, and per-scope isolation.
func TestCompactionStore_ListCurrentChainReadsWholeGeneration(t *testing.T) {
	t.Parallel()
	h, ctx := newPGStore(t)

	// 上一代：读面**不得**回它——换代之后旧条目只作历史，混进来会让面板把两代拼成一条。
	h.seedChain(t, h.scope, h.entry("cp-old", 1, "", "1", "10", "11"))

	failed := h.entry("cp-failed", 2, "", "1", "20", "21")
	failed.Status = compaction.StatusFailed
	failed.ReasonCode = "compaction_timeout"
	// 中间那条先按 committed 写入，再直写翻成 stale：`insertCompactionEntry` 不写
	// stale_at（正常失效走 MarkStale，会级联后代），本用例要的正是"同代次三态并存"。
	middle := h.entry("cp-middle", 2, "cp-failed", "21", "30", "31")
	newest := h.entry("cp-new", 2, "cp-middle", "31", "40", "41")
	h.seedChain(t, h.scope, failed, middle, newest)

	staleAt := pgBase.Add(90 * time.Second).UnixMilli()
	_, err := h.db.pool.ExecContext(ctx,
		`UPDATE compaction_entries SET status = 'stale', stale_at = $2, reason_code = 'revoked'
		 WHERE compaction_id = $1`, "cp-middle", staleAt)
	require.NoError(t, err)

	got, err := h.ListCurrentChain(ctx, h.scope, 50)
	require.NoError(t, err)
	require.Len(t, got, 3, "当前代次的三条都在（旧代次不出现）")

	require.Equal(t, []string{"cp-failed", "cp-middle", "cp-new"}, []string{
		got[0].CompactionID, got[1].CompactionID, got[2].CompactionID,
	}, "按 (created_at, id) 升序，本层不按父指针重排")

	require.Equal(t, compaction.StatusFailed, got[0].Status)
	require.Equal(t, "compaction_timeout", got[0].ReasonCode, "失败必须带降级原因")
	require.Equal(t, compaction.StatusStale, got[1].Status)
	require.Equal(t, "revoked", got[1].ReasonCode)
	require.True(t, got[1].StaleAt.Equal(time.UnixMilli(staleAt).UTC()), "失效时间原样读回")
	require.Equal(t, compaction.StatusCommitted, got[2].Status)
	require.True(t, got[2].StaleAt.IsZero(), "未失效的行 stale_at 是零值（出站为空串）")

	require.Equal(t, "摘要-cp-new", got[2].Summary, "摘要随条目原样返回")

	// `limit` 截**最前** N 条：与 SQL LIMIT 在同一次序下的语义一致，且本层不重排。
	page, err := h.ListCurrentChain(ctx, h.scope, 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, []string{"cp-failed", "cp-middle"}, []string{
		page[0].CompactionID, page[1].CompactionID,
	})

	// scope 隔离：无状态 Run 范围读不到会话范围的行。
	other, err := h.ListCurrentChain(ctx, compaction.Scope{RunID: "r1"}, 50)
	require.NoError(t, err)
	require.Nil(t, other, "另一种 scope 不得串味")

	// 入参违约：空 scope / 非正 limit 都是调用方错误，不得静默当成"没有记录"。
	_, err = h.ListCurrentChain(ctx, compaction.Scope{}, 50)
	require.Error(t, err, "空 scope 必须报错")
	_, err = h.ListCurrentChain(ctx, h.scope, 0)
	require.Error(t, err, "limit < 1 必须报错")

	// 无任何行的 scope 是合法空集：`nil, nil`——由 handler 投影成 `[]`。
	empty, err := h.ListCurrentChain(ctx, compaction.Scope{RunID: "no-such-run"}, 50)
	require.NoError(t, err)
	require.Nil(t, empty)
}
