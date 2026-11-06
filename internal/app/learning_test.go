//go:build integration

package app

// 本文件是 US-22 T013 的验收面（颗粒度文档 §3.1.8 / §4.3 / §4.4）：
//
//	S1 `TestUS22_Status_ShowsBudget`——状态页给 used / limit / window 与版本号，孤儿草案只报不删
//	S2 `TestUS22_Status_ShowsExhausted`——尝试数达到上限即 exhausted（判定按 >=，不是 >）
//	S3 `TestUS22_Status_StoreError`——存储不可用返回错误，**不得**返回空计数冒充「没有抽取」
//	D1 `TestUS22_LoadDraft_RejectsDotIDs`——草案正文读取只认单层目录名，`.` / `..` 不得跳出草案根
//	C1 `TestUS22_Candidate_NotRecalledBeforeConfirm`——候选确认前不参与召回（真实 PG 判据）
//	C2 `TestUS22_Candidate_NotInjected`——候选不进入生效层核心区文本（LoadEffective）
//	K1 `TestUS22_Compat_SaveMemoryStaysActive`——`save_memory` 的显式保存仍是 active / save_memory
//	K2 `TestUS22_Compat_AdminProposeUnchanged`——管理 Propose 仍写 candidate / explicit_propose
//
// 判据口径：S1–S3 用假存储判读模型**自身的**映射与错误传播（不触库）；C1–C2 / K1–K2
// 走真实 PostgreSQL 与生产存储，因为 `state='active'` 过滤、`source` 推导与
// `LoadEffective` 的生效层判据都写在 SQL / 生产实现里，用内存替身会把断言变成
// 「替身替我做了判断」的同义反复。
// The status cases assert the read model's own mapping over a fake store; the four
// behavioral cases run against the real stores because the filter under test lives in SQL.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
	"github.com/ymedword/basalt/internal/tool"
)

// fakeLearningAdminStore 是只读查询端口的替身：只实现状态页真正会调到的三个方法
// （LearningCounts / LastExtractionAt / LoadDraft），其余方法返回零值——若被测代码
// 意外调用它们，用例仍会以可读的失败暴露，而不是 panic。
// fakeLearningAdminStore implements the three methods the status page calls; the rest
// return zero values so an unexpected call fails visibly instead of panicking.
type fakeLearningAdminStore struct {
	counts      memory.LearningCounts
	countsErr   error
	countsSince time.Time

	lastAt    time.Time
	lastFound bool
	lastErr   error

	drafts       map[string]core.LearningDraftView
	loadDraftErr error
}

func (f *fakeLearningAdminStore) PendingExtractions(context.Context) ([]core.LearningExtraction, error) {
	return nil, nil
}

func (f *fakeLearningAdminStore) ExtractionCandidateCount(context.Context, string) (int, error) {
	return 0, nil
}

func (f *fakeLearningAdminStore) ExtractionDraftCount(context.Context, string) (int, error) {
	return 0, nil
}

func (f *fakeLearningAdminStore) RecoverExtraction(context.Context, string,
	core.LearningExtractionState, time.Time,
) error {
	return nil
}

func (f *fakeLearningAdminStore) ListExtractions(context.Context,
	core.LearningExtractionFilter,
) ([]core.LearningExtractionView, error) {
	return nil, nil
}

func (f *fakeLearningAdminStore) LoadExtraction(context.Context, string) (core.LearningExtractionView, bool, error) {
	return core.LearningExtractionView{}, false, nil
}

func (f *fakeLearningAdminStore) ListDrafts(context.Context,
	core.LearningDraftFilter,
) ([]core.LearningDraftView, error) {
	return nil, nil
}

func (f *fakeLearningAdminStore) LoadDraft(_ context.Context, draftID string) (core.LearningDraftView, bool, error) {
	if f.loadDraftErr != nil {
		return core.LearningDraftView{}, false, f.loadDraftErr
	}
	view, found := f.drafts[draftID]
	return view, found, nil
}

func (f *fakeLearningAdminStore) LearningCounts(_ context.Context, since time.Time) (memory.LearningCounts, error) {
	f.countsSince = since
	if f.countsErr != nil {
		return memory.LearningCounts{}, f.countsErr
	}
	return f.counts, nil
}

func (f *fakeLearningAdminStore) LastExtractionAt(context.Context) (time.Time, bool, error) {
	if f.lastErr != nil {
		return time.Time{}, false, f.lastErr
	}
	return f.lastAt, f.lastFound, nil
}

// TestUS22_Status_ShowsBudget 判状态页的字段映射：预算窗口按 now-24h 起算、版本号来自
// 代码常量而不是第二份字面量、孤儿草案只统计 `dft_` 目录且只报告（不删）。
// 逆改点：窗口起点写成 now-1h → `countsSince` 断言红；版本号在 app 里复制字面量并写错
// → 版本断言红；孤儿计数把非 `dft_` 目录算进去 → 计数断言红。
func TestUS22_Status_ShowsBudget(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dft_orphan"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dft_known"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "not-a-draft"), 0o755))

	last := baselineClock().Add(-2 * time.Hour)
	store := &fakeLearningAdminStore{
		counts:    memory.LearningCounts{Attempts: 7, Pending: 2, Unknown: 1},
		lastAt:    last,
		lastFound: true,
		drafts: map[string]core.LearningDraftView{
			"dft_known": {DraftID: "dft_known"},
		},
	}
	logger, _ := newCapturedLogger()
	admin := NewLearningAdmin(LearningAdminDeps{
		Store:         store,
		DraftRoot:     root,
		Enabled:       true,
		Agents:        []string{"ops", "writer"},
		UnknownAgents: []string{"writer"},
		Logger:        logger,
		Now:           baselineClock,
	})

	status, err := admin.Status(context.Background())
	require.NoError(t, err)

	require.True(t, status.Enabled, "启用态必须原样展示")
	require.Equal(t, []string{"ops", "writer"}, status.Agents, "名单按配置顺序展示")
	require.Equal(t, []string{"writer"}, status.UnknownAgents, "未注册名单项单独列出（Q7）")
	require.Equal(t, 7, status.AttemptsUsed, "滚动窗口内的尝试数来自存储")
	require.Equal(t, 200, status.AttemptLimit, "尝试上限是 §3.1.12 的代码常量")
	require.False(t, status.Exhausted, "7 < 200 不得报耗尽")
	require.Equal(t, 24*time.Hour, status.BudgetWindow)
	require.Equal(t, 2, status.PendingExtractions)
	require.Equal(t, 1, status.UnknownExtractions)
	require.Equal(t, 1, status.OrphanDrafts, "只有 dft_ 前缀且无 DB 行的目录算孤儿")
	require.Equal(t, last, status.LastExtractionAt)
	require.Equal(t, memory.LearningRuleVersion, status.RuleVersion)
	require.Equal(t, memory.LearningPromptVersion, status.PromptVersion)

	// 滚动窗口的起点必须是 now-24h：预算按「最近的尝试」求和，不是「当天零点」。
	require.Equal(t, baselineClock().Add(-24*time.Hour), store.countsSince,
		"预算窗口起点 = now - BudgetWindow")
	// 孤儿目录只报告、不修补：目录仍在原处。
	_, statErr := os.Stat(filepath.Join(root, "dft_orphan"))
	require.NoError(t, statErr, "统计孤儿草案不得删除或移走目录")
}

// TestUS22_Status_ShowsExhausted 判耗尽的边界：达到上限即 exhausted（`>=`，不是 `>`）。
// 逆改点：判定写成 `>` → attempts=limit 时 exhausted 为 false，用例红。
func TestUS22_Status_ShowsExhausted(t *testing.T) {
	store := &fakeLearningAdminStore{
		counts: memory.LearningCounts{Attempts: 200, Pending: 1},
	}
	logger, _ := newCapturedLogger()
	admin := NewLearningAdmin(LearningAdminDeps{
		Store:  store,
		Limits: memory.DefaultLearningLimits(),
		Logger: logger,
		Now:    baselineClock,
	})

	status, err := admin.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, 200, status.AttemptsUsed)
	require.Equal(t, 200, status.AttemptLimit)
	require.True(t, status.Exhausted, "达到上限即耗尽")
}

// TestUS22_Status_StoreError 判错误不被吞：计数读取失败必须上抛（web 映射 500）。
// 逆改点：返回空计数 + nil error → 断言红；错误串里丢掉原因 → ErrorIs 红。
func TestUS22_Status_StoreError(t *testing.T) {
	boom := errors.New("storage down")
	store := &fakeLearningAdminStore{countsErr: boom}
	logger, _ := newCapturedLogger()
	admin := NewLearningAdmin(LearningAdminDeps{
		Store:  store,
		Logger: logger,
		Now:    baselineClock,
	})

	status, err := admin.Status(context.Background())
	require.Error(t, err, "存储不可用时不得返回空状态")
	require.ErrorIs(t, err, boom, "原始原因必须可追溯")
	require.Zero(t, status.AttemptsUsed)
}

// TestUS22_LoadDraft_RejectsDotIDs 判草案正文读取的单层目录名守卫：`.` / `..` 经
// filepath.Base 后原样返回，不显式拒绝的话 Join 会跳出草案根，读到根目录或草案根下的
// 同名 DRAFT.md。哨兵文件放在这两处：守卫失效时它们会被读进 Content。
// 逆改点：删掉 `== "." || == ".."` 分支 → 两次读取都拿到哨兵正文，断言红。
// The single-element guard must reject "." and ".."; sentinel DRAFT.md files sit where a
// broken guard would land so a regression surfaces as their body appearing in Content.
func TestUS22_LoadDraft_RejectsDotIDs(t *testing.T) {
	root := t.TempDir()
	drafts := filepath.Join(root, "drafts")
	require.NoError(t, os.MkdirAll(filepath.Join(drafts, "dft_ok"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(drafts, "dft_ok", "DRAFT.md"), []byte("ok"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "DRAFT.md"), []byte("sentinel-parent"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(drafts, "DRAFT.md"), []byte("sentinel-drafts"), 0o600))

	store := &fakeLearningAdminStore{drafts: map[string]core.LearningDraftView{
		"dft_ok": {DraftID: "dft_ok"},
		"..":     {DraftID: ".."},
		".":      {DraftID: "."},
	}}
	logger, _ := newCapturedLogger()
	admin := NewLearningAdmin(LearningAdminDeps{
		Store:     store,
		DraftRoot: drafts,
		Logger:    logger,
		Now:       baselineClock,
	})

	for _, id := range []string{"..", "."} {
		view, err := admin.LoadDraft(context.Background(), id)
		require.NoError(t, err, "拒绝读盘不是错误：元数据照常返回、正文留空")
		require.Empty(t, view.Content, "%q 不得读出草案根之外的正文", id)
	}
	ok, err := admin.LoadDraft(context.Background(), "dft_ok")
	require.NoError(t, err)
	require.Equal(t, "ok", ok.Content, "正常单层目录名仍必须读到正文（守卫不是一刀切）")
}

// seedUS22Candidate 落一条自动抽取候选（candidate / auto_extract / 归档区）。
// seedUS22Candidate inserts one auto-extracted candidate exactly as the commit path does.
func seedUS22Candidate(t *testing.T, store *postgres.MemoryStore, agent, content string) int64 {
	t.Helper()
	id, err := store.InsertEntry(context.Background(), memory.WithSource(memory.MemoryEntry{
		Scope:   core.Scope{Agent: agent},
		Section: core.SectionArchive,
		Content: content,
		State:   memory.StateCandidate,
	}, memory.SourceAutoExtract))
	require.NoError(t, err)
	require.NotZero(t, id)
	return id
}

// TestUS22_Candidate_NotRecalledBeforeConfirm 判候选在确认前不参与召回；同正文的生效
// 条目必须照常召回——后者证明空结果不是「查询写错了」（非空对照）。
// 逆改点：召回 SQL 去掉 `state='active'`（或层内 activeOnly 守点）→ 候选出现在结果里，
// 用例红。
func TestUS22_Candidate_NotRecalledBeforeConfirm(t *testing.T) {
	db := pgtest.NewTestDB(t)
	store := postgres.NewMemoryStore(db, baselineClock)
	recaller := memory.NewSemanticStore(memory.SemanticDeps{Store: store})
	query := memory.RecallQuery{
		Principal: core.LocalAdminPrincipal(),
		Scope:     core.Scope{Agent: "ops"},
		Query:     "季度报表",
		Mode:      memory.RecallModeKeyword,
		Limit:     10,
	}

	seedUS22Candidate(t, store, "ops", "季度报表在每月 5 号前提交")

	hits, err := recaller.Recall(context.Background(), query)
	require.NoError(t, err)
	require.Empty(t, hits, "候选不得参与召回")

	// 非空对照：同分层同分区的一条 active 条目按同一查询必须召回。keyword 模式走
	// 旧门面只回正文（不携带主键），因此按正文断言；主键级排除由存储层用例覆盖
	// （TestUS22_Recall_*）。
	_, err = store.InsertEntry(context.Background(), memory.MemoryEntry{
		Scope:   core.Scope{Agent: "ops"},
		Section: core.SectionArchive,
		Content: "季度报表模板放在共享盘 /reports",
		State:   memory.StateActive,
	})
	require.NoError(t, err)
	hits, err = recaller.Recall(context.Background(), query)
	require.NoError(t, err)
	require.Len(t, hits, 1, "生效条目必须召回（证明过滤不是把一切都挡掉）")
	require.Equal(t, "季度报表模板放在共享盘 /reports", hits[0].Content)
	require.Equal(t, memory.StateActive, hits[0].State, "召回结果按生效条目投影")
}

// TestUS22_Candidate_NotInjected 判候选不进入生效层核心区文本：即使候选行被手工放进
// 核心区（防御性投影），`LoadEffective` 也只能取 active 行。
// 逆改点：`LoadEffective` 的读取不再只取 active 核区 → 候选正文出现在注入文本里，用例红。
func TestUS22_Candidate_NotInjected(t *testing.T) {
	db := pgtest.NewTestDB(t)
	store := postgres.NewMemoryStore(db, baselineClock)
	logger, _ := newCapturedLogger()
	svc, err := memory.NewService(memory.Options{Backend: store, Logger: logger})
	require.NoError(t, err)

	_, err = store.InsertEntry(context.Background(), memory.MemoryEntry{
		Scope:   core.Scope{Agent: "ops"},
		Section: core.SectionCore,
		Content: "对外工作语言为中文",
		State:   memory.StateActive,
	})
	require.NoError(t, err)
	// 候选故意落在核心区：若读取面放宽了 active 判据，它就会进系统提示。
	_, err = store.InsertEntry(context.Background(), memory.WithSource(memory.MemoryEntry{
		Scope:   core.Scope{Agent: "ops"},
		Section: core.SectionCore,
		Content: "季度报表在每月 5 号前提交",
		State:   memory.StateCandidate,
	}, memory.SourceAutoExtract))
	require.NoError(t, err)

	text, err := svc.LoadEffective(context.Background(), core.LocalAdminPrincipal(), "ops")
	require.NoError(t, err)
	require.Contains(t, text, "对外工作语言为中文", "生效核心区条目必须注入")
	require.NotContains(t, text, "季度报表", "候选不得进入注入面")
}

// TestUS22_Compat_SaveMemoryStaysActive 判 `save_memory` 的显式保存语义不变：工具调用的
// `Append` 仍写 active / save_memory（C-03）。
// 逆改点：把 Append 改成写 candidate（自动抽取顺手统一）→ 状态断言红。
func TestUS22_Compat_SaveMemoryStaysActive(t *testing.T) {
	db := pgtest.NewTestDB(t)
	store := postgres.NewMemoryStore(db, baselineClock)
	logger, _ := newCapturedLogger()
	svc, err := memory.NewService(memory.Options{Backend: store, Logger: logger})
	require.NoError(t, err)

	scope := core.Scope{Agent: "ops"}
	require.NoError(t, svc.Append(context.Background(), scope, core.SectionArchive,
		"发布窗口固定在每周二"))

	entry, found, err := store.FindEntryByContent(context.Background(), scope,
		core.SectionArchive, "发布窗口固定在每周二")
	require.NoError(t, err)
	require.True(t, found, "显式保存必须落库")
	require.Equal(t, memory.StateActive, entry.State, "显式保存即生效")
	require.Equal(t, memory.SourceSaveMemory, entry.Source(), "来源仍是 save_memory")
}

// TestUS22_Compat_AdminProposeUnchanged 判管理提议的语义不变：仍写 candidate /
// explicit_propose，回执如实给 candidate（C-03）。
// 逆改点：把 Propose 改成直接写 active → 状态断言红；来源改写成 auto_extract → 来源断言红。
func TestUS22_Compat_AdminProposeUnchanged(t *testing.T) {
	db := pgtest.NewTestDB(t)
	store := postgres.NewMemoryStore(db, baselineClock)
	logger, _ := newCapturedLogger()
	admin := NewMemoryAdmin(MemoryAdminDeps{
		Store:      store,
		Operations: postgres.NewManagementStore(db),
		Logger:     logger,
		Now:        baselineClock,
	})

	result, err := admin.Propose(context.Background(), consoleActor(), core.MemoryProposal{
		Scope:   core.Scope{Agent: "ops"},
		Content: "每月首个工作日出上月结算",
	})
	require.NoError(t, err)
	require.Equal(t, memory.StateCandidate, result.State, "提议回执如实给候选")

	entry, found, err := store.LoadEntry(context.Background(), result.EntryID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memory.StateCandidate, entry.State, "提议落库仍是候选")
	require.Equal(t, memory.SourceExplicitPropose, entry.Source(), "来源仍是 explicit_propose")
}

// us22CountingMemoryStore 数 `RecallByKeyword` 的进入次数：判据必须证明用例走的是
// **真实工具入口**，不是绕过工具直接调存储辅助函数。
// us22CountingMemoryStore counts keyword-recall entries so the case proves the real
// tool entry ran, not a storage helper called around it.
type us22CountingMemoryStore struct {
	core.LongTermMemoryStore
	calls int
}

func (s *us22CountingMemoryStore) RecallByKeyword(ctx context.Context, scope core.Scope, query string) ([]string, error) {
	s.calls++
	return s.LongTermMemoryStore.RecallByKeyword(ctx, scope, query)
}

// us22RecallKeywordViaTool 走 `recall_memory(mode=keyword)` 的真实 Execute 路径。
// us22RecallKeywordViaTool executes the real recall_memory tool in keyword mode.
func us22RecallKeywordViaTool(t *testing.T, counted *us22CountingMemoryStore, agent, query string) string {
	t.Helper()
	recall := tool.NewRecallMemoryTool(counted, nil)
	result, err := recall.Execute(context.Background(), core.ToolEnv{
		Principal: core.LocalAdminPrincipal(),
		AgentName: agent,
	}, json.RawMessage(`{"query":"`+query+`","mode":"keyword"}`))
	require.NoError(t, err)
	return result.Content
}

// us22SeedExtraction 插一条成功抽取行——来源关系行的 `extraction_id` 外键目标。
func us22SeedExtraction(t *testing.T, raw *sql.DB, extractionID string) {
	t.Helper()
	at := baselineClock().UnixMilli()
	_, err := raw.ExecContext(context.Background(), `
		INSERT INTO memory_extractions
			(extraction_id, agent_name, session_id, run_id, last_attempt_run_id,
			 origin_caller_id, origin_key_id, status, window_from_message_id, window_to_message_id,
			 window_messages, prompt_version, rule_version, provider, model, created_at, last_attempt_at)
		VALUES ($1, 'recall', 'sess_tool', $1 || '_run', $1 || '_run', 'caller-tool', '',
		        'success', 1, 2, 2, 'mv1', 'rv1', 'stub', 'stub-model', $2, $2)`,
		extractionID, at)
	require.NoError(t, err)
}

// us22Derive 写一行来源关系：entryID 的直接来源是 sourceID。
func us22Derive(t *testing.T, raw *sql.DB, extractionID string, entryID, sourceID int64) {
	t.Helper()
	_, err := raw.ExecContext(context.Background(), `
		INSERT INTO memory_candidate_sources (entry_id, source_entry_id, extraction_id, created_at)
		VALUES ($1, $2, $3, $4)`,
		entryID, sourceID, extractionID, baselineClock().UnixMilli())
	require.NoError(t, err)
}

// us22SeedRecallEntry 落一条归档区条目；derived=true 按 `auto_extract` 落库（与提交
// 事务同形），否则是普通显式写入（无来源行）。
func us22SeedRecallEntry(t *testing.T, store *postgres.MemoryStore, agent, content string, derived bool) int64 {
	t.Helper()
	entry := memory.MemoryEntry{
		Scope:   core.Scope{Agent: agent},
		Section: core.SectionArchive,
		Content: content,
		State:   memory.StateActive,
	}
	if derived {
		entry = memory.WithSource(entry, memory.SourceAutoExtract)
	}
	id, err := store.InsertEntry(context.Background(), entry)
	require.NoError(t, err)
	return id
}

// TestUS22_Recall_ToolKeywordStopsDerived 守 C-31 的**真实入口**：`recall_memory`
// (mode=keyword) 的 Execute 一次调用即进入 `RecallByKeyword`，来源被删除后派生条目
// 从工具输出里消失；显式条目（无来源行）照常召回。
// 逆改点：从 recallByKeywordSQL 删掉 `derivedLiveFilterSQL` → 删除来源后派生正文仍在
// 工具输出里 → 红；若用例绕过工具直接调存储，进入次数断言先红。
func TestUS22_Recall_ToolKeywordStopsDerived(t *testing.T) {
	fixture := pgtest.NewFixture(t)
	store := postgres.NewMemoryStore(fixture.DB, baselineClock)
	raw := openRawSQL(t, fixture.DSN)

	const extractionID = "lrn_tool_recall"
	us22SeedExtraction(t, raw, extractionID)
	sourceID := us22SeedRecallEntry(t, store, "recall", "来源甲-记忆", false)
	derivedID := us22SeedRecallEntry(t, store, "recall", "派生乙-记忆", true)
	us22SeedRecallEntry(t, store, "recall", "对照-记忆", false)
	us22Derive(t, raw, extractionID, derivedID, sourceID)

	counted := &us22CountingMemoryStore{LongTermMemoryStore: store}
	content := us22RecallKeywordViaTool(t, counted, "recall", "记忆")
	require.Contains(t, content, "来源甲-记忆")
	require.Contains(t, content, "派生乙-记忆", "来源有效时派生条目照常召回（夹具先说清楚）")
	require.Contains(t, content, "对照-记忆")
	require.Equal(t, 1, counted.calls, "真实工具入口必须进入一次 RecallByKeyword")

	_, err := raw.ExecContext(context.Background(),
		`UPDATE memory_entries SET state = $2 WHERE id = $1`, sourceID, memory.StateDeleted)
	require.NoError(t, err)

	content = us22RecallKeywordViaTool(t, counted, "recall", "记忆")
	require.NotContains(t, content, "派生乙-记忆", "来源被删除后派生条目不得再从工具入口召回")
	require.Equal(t, "对照-记忆", content, "只剩显式条目——来源自身也因删除退出")
	require.Equal(t, 2, counted.calls)
}

// TestUS22_Recall_ToolKeywordTransitiveChain 守 C-31 的递归语义经过真实工具入口：
// A → B → C 时删除 A，B 与 C 一起退出；只查直接来源会漏掉 C。
// 逆改点：把递归闭包换成「只查直接来源」→ C 仍出现在工具输出里 → 红。
func TestUS22_Recall_ToolKeywordTransitiveChain(t *testing.T) {
	fixture := pgtest.NewFixture(t)
	store := postgres.NewMemoryStore(fixture.DB, baselineClock)
	raw := openRawSQL(t, fixture.DSN)

	const extractionID = "lrn_tool_chain"
	us22SeedExtraction(t, raw, extractionID)
	rootID := us22SeedRecallEntry(t, store, "recall", "链首-记忆", false)
	midID := us22SeedRecallEntry(t, store, "recall", "链中-记忆", true)
	leafID := us22SeedRecallEntry(t, store, "recall", "链尾-记忆", true)
	us22SeedRecallEntry(t, store, "recall", "显式-记忆", false)
	us22Derive(t, raw, extractionID, midID, rootID)
	us22Derive(t, raw, extractionID, leafID, midID)

	counted := &us22CountingMemoryStore{LongTermMemoryStore: store}
	content := us22RecallKeywordViaTool(t, counted, "recall", "记忆")
	require.Equal(t, 4, strings.Count(content, "记忆"),
		"闭包全部有效时三级派生与显式条目都在（4 条，每条正文含一个「记忆」）")

	_, err := raw.ExecContext(context.Background(),
		`UPDATE memory_entries SET state = $2 WHERE id = $1`, rootID, memory.StateDeleted)
	require.NoError(t, err)

	content = us22RecallKeywordViaTool(t, counted, "recall", "记忆")
	require.Equal(t, "显式-记忆", content, "链首失效 → 一级与二级派生一起退出")
	require.Equal(t, 2, counted.calls, "两次判定都经真实工具入口")
}

// TestUS22_Recall_ToolKeywordDepthFailsClosed 守 C-31 的防御性上限经过真实工具入口：
// 恰为 8 跳的链可召回；深度 8 仍有更深来源的条目按失效处理（fail-closed）。
// 逆改点：删掉 `c.depth >= 8 AND EXISTS (...)` 分支 → 链首出现在工具输出里 → 红。
func TestUS22_Recall_ToolKeywordDepthFailsClosed(t *testing.T) {
	fixture := pgtest.NewFixture(t)
	store := postgres.NewMemoryStore(fixture.DB, baselineClock)
	raw := openRawSQL(t, fixture.DSN)

	const extractionID = "lrn_tool_depth"
	us22SeedExtraction(t, raw, extractionID)
	const chain = 10 // E0 的链是 9 跳（超限）；E1 的链恰为 8 跳（在上限内）。
	ids := make([]int64, chain)
	for i := 0; i < chain; i++ {
		ids[i] = us22SeedRecallEntry(t, store, "recall", fmt.Sprintf("链%02d-记忆", i), true)
	}
	for i := 0; i < chain-1; i++ {
		us22Derive(t, raw, extractionID, ids[i], ids[i+1])
	}
	us22SeedRecallEntry(t, store, "recall", "显式-记忆", false)

	counted := &us22CountingMemoryStore{LongTermMemoryStore: store}
	content := us22RecallKeywordViaTool(t, counted, "recall", "记忆")
	require.NotContains(t, content, "链00-记忆", "深度 8 仍有更深来源 → fail-closed 排除")
	require.Contains(t, content, "链01-记忆", "恰为 8 跳的链仍可召回（上限是 8，不是 7）")
	require.Contains(t, content, "显式-记忆")
	require.Equal(t, 1, counted.calls, "一次工具调用即完成全部判据")
}

// ─────────────────────── T022：方法草案转交（handoff，C-19） ───────────────────────
//
//	H1 `TestUS22_Handoff_TakesLock`——取锁先于读草案与 Propose、成功与失败都释放；
//	   锁不可得 / 端口缺失时零读取零提议
//	H2 `TestUS22_Handoff_ForcesSourceFields`——kind / ref 由程序强制，其余声明字段保留
//	H3 `TestUS22_Handoff_RequiresUserConfirmed`——非 user_confirmed 在取锁前拒绝
//	H4 `TestUS22_Handoff_CallsProposeOnly`——只调用 Propose，七个写动作其余计数为零
//
// 判据口径：四个用例都走生产 `LearningHandoff`，依赖替身只记录调用与顺序；不触库、
// 不建冻结目录，转交的磁盘副作用全部归 US-21 的 Propose，本层不复制。
// The four cases run the production adapter over recording doubles; no database and no
// candidate directory is built here, because those side effects belong to US-21 Propose.

// us22HandoffDraftID 是转交夹具的草案标识（前缀与生产常量一致）。
const us22HandoffDraftID = "dft_handoff01"

// us22HandoffSourceJSON 返回一份字段完整的严格来源声明；confirmation 由调用方给出，
// 用于证明「确认态」是唯一被额外收紧的字段。
func us22HandoffSourceJSON(confirmation string) []byte {
	return []byte(`{"kind":"manual","ref":"admin-note","task_outcome":"completed",` +
		`"confirmation":"` + confirmation + `","authorization":"not_required",` +
		`"redaction":"not_needed","applicable":true,"notes":"人工确认后转交",` +
		`"counterexamples":["跳过跨团队汇总"],"evidence":["msg_1"],"limitations":["单次样本"]}`)
}

// us22HandoffLock 记录取锁 / 释放与调用顺序；err 模拟锁被占用。
type us22HandoffLock struct {
	root     string
	acquires int
	releases int
	err      error
	events   *[]string
}

func (l *us22HandoffLock) acquire(root string) (func() error, error) {
	l.root = root
	l.acquires++
	if l.err != nil {
		return nil, l.err
	}
	*l.events = append(*l.events, "lock")
	return func() error {
		l.releases++
		*l.events = append(*l.events, "release")
		return nil
	}, nil
}

// us22HandoffDrafts 是草案读取替身：记录读取次数与顺序，可按需制造读失败。
type us22HandoffDrafts struct {
	drafts  map[string]core.LearningDraftView
	err     error
	loads   int
	loadIDs []string
	events  *[]string
}

func (d *us22HandoffDrafts) LoadDraft(_ context.Context, draftID string) (core.LearningDraftView, bool, error) {
	d.loads++
	d.loadIDs = append(d.loadIDs, draftID)
	*d.events = append(*d.events, "load")
	if d.err != nil {
		return core.LearningDraftView{}, false, d.err
	}
	view, found := d.drafts[draftID]
	return view, found, nil
}

// us22HandoffProposer 同时实现七个准入写动作：只有 Propose 记录请求，其余只计数——
// 「只调 Propose」因此是**可判读**的断言，而不是接口形状的自我证明。
type us22HandoffProposer struct {
	requests  []core.AdmissionProposeRequest
	actors    []core.Actor
	candidate core.AdmissionCandidate
	err       error
	events    *[]string

	evaluate, approve, reject, cancel, publish, withdraw int
}

func (p *us22HandoffProposer) Propose(_ context.Context, actor core.Actor,
	req core.AdmissionProposeRequest,
) (core.AdmissionCandidate, error) {
	p.requests = append(p.requests, req)
	p.actors = append(p.actors, actor)
	*p.events = append(*p.events, "propose")
	if p.err != nil {
		return core.AdmissionCandidate{}, p.err
	}
	return p.candidate, nil
}

func (p *us22HandoffProposer) Evaluate(context.Context, core.Actor, string,
	core.AdmissionPolicy,
) (core.AdmissionEvaluation, error) {
	p.evaluate++
	return core.AdmissionEvaluation{}, nil
}

func (p *us22HandoffProposer) Approve(context.Context, core.Actor, string, []byte,
	core.AdmissionPolicy,
) (core.AdmissionDecision, error) {
	p.approve++
	return core.AdmissionDecision{}, nil
}

func (p *us22HandoffProposer) Reject(context.Context, core.Actor, string,
	[]byte,
) (core.AdmissionDecision, error) {
	p.reject++
	return core.AdmissionDecision{}, nil
}

func (p *us22HandoffProposer) Cancel(context.Context, core.Actor, string, string) (core.AdmissionCandidate, error) {
	p.cancel++
	return core.AdmissionCandidate{}, nil
}

func (p *us22HandoffProposer) Publish(context.Context, core.Actor, string,
	core.AdmissionPolicy,
) (core.AdmissionPublication, error) {
	p.publish++
	return core.AdmissionPublication{}, nil
}

func (p *us22HandoffProposer) Withdraw(context.Context, core.Actor, string, string) (core.AdmissionPublication, error) {
	p.withdraw++
	return core.AdmissionPublication{}, nil
}

// us22HandoffRig 组装转交夹具：默认全部依赖成功、事件共享同一序列。
type us22HandoffRig struct {
	handoff  *LearningHandoff
	drafts   *us22HandoffDrafts
	proposer *us22HandoffProposer
	lock     *us22HandoffLock
	events   *[]string
}

func newUS22HandoffRig(t *testing.T) *us22HandoffRig {
	t.Helper()
	events := make([]string, 0, 8)
	lock := &us22HandoffLock{events: &events}
	drafts := &us22HandoffDrafts{
		drafts: map[string]core.LearningDraftView{
			us22HandoffDraftID: {
				DraftID: us22HandoffDraftID,
				Agent:   "demo",
				Name:    "weekly-report",
				RelDir:  ".basalt/learning/drafts/" + us22HandoffDraftID,
			},
		},
		events: &events,
	}
	proposer := &us22HandoffProposer{
		candidate: core.AdmissionCandidate{CandidateID: "adm_handoff01", SkillName: "weekly-report"},
		events:    &events,
	}
	handoff := NewLearningHandoff(LearningHandoffDeps{
		Drafts:      drafts,
		Admission:   proposer,
		Root:        "/workspace/.basalt",
		AcquireLock: lock.acquire,
	})
	return &us22HandoffRig{handoff: handoff, drafts: drafts, proposer: proposer, lock: lock, events: &events}
}

// us22HandoffRequest 是合法请求：可信 CLI 主体、真实草案、用户确认来源。
// 请求类型在 core（命令面与装配层共用同一契约），夹具因此也用 core 类型。
func us22HandoffRequest(t *testing.T) core.LearningHandoffRequest {
	t.Helper()
	return core.LearningHandoffRequest{
		Actor:      core.Actor{Subject: "cli:alice", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		DraftID:    us22HandoffDraftID,
		SkillName:  "weekly-report",
		Files:      []core.AuthorFile{{Path: "SKILL.md", Content: "---\nname: weekly-report\n---\n"}},
		SourceJSON: us22HandoffSourceJSON("user_confirmed"),
	}
}

// H1：锁在读取草案与 Propose 之前取得、之后释放；锁不可得或端口缺失时零读取零提议。
// 逆改点：把 AcquireLock 挪到 Propose 之后 → 顺序断言红；失败路径不释放锁 → releases 红。
func TestUS22_Handoff_TakesLock(t *testing.T) {
	rig := newUS22HandoffRig(t)
	_, err := rig.handoff.Handoff(context.Background(), us22HandoffRequest(t))
	require.NoError(t, err)
	require.Equal(t, 1, rig.lock.acquires, "一次转交只取一次实例锁")
	require.Equal(t, "/workspace/.basalt", rig.lock.root, "锁必须取在工作区根上")
	require.Equal(t, 1, rig.lock.releases, "成功路径释放一次")
	require.Equal(t, []string{"lock", "load", "propose", "release"}, *rig.events,
		"固定顺序：取锁 → 读草案 → Propose → 释放")

	t.Run("锁被占用", func(t *testing.T) {
		busy := newUS22HandoffRig(t)
		busyErr := errors.New("workspace busy: serve 正在运行")
		busy.lock.err = busyErr
		_, err := busy.handoff.Handoff(context.Background(), us22HandoffRequest(t))
		require.ErrorIs(t, err, busyErr, "锁被占用原样上抛")
		require.Zero(t, busy.drafts.loads, "锁不可得时不得读草案")
		require.Zero(t, len(busy.proposer.requests), "锁不可得时不得提议")
	})

	t.Run("Propose 失败仍释放", func(t *testing.T) {
		failing := newUS22HandoffRig(t)
		proposeErr := errors.New("磁盘冻结失败")
		failing.proposer.err = proposeErr
		_, err := failing.handoff.Handoff(context.Background(), us22HandoffRequest(t))
		require.ErrorIs(t, err, proposeErr)
		require.Equal(t, 1, failing.lock.releases, "失败路径也要释放实例锁")
	})

	t.Run("端口缺失失败关闭", func(t *testing.T) {
		missingDrafts := newUS22HandoffRig(t)
		handoff := NewLearningHandoff(LearningHandoffDeps{
			Admission: missingDrafts.proposer,
			Root:      "/workspace/.basalt",
			// Drafts 与 AcquireLock 均缺失。
		})
		_, err := handoff.Handoff(context.Background(), us22HandoffRequest(t))
		require.Error(t, err, "缺依赖必须拒绝，不得无锁执行")
		require.Zero(t, len(missingDrafts.proposer.requests))

		missingLock := newUS22HandoffRig(t)
		noLock := NewLearningHandoff(LearningHandoffDeps{
			Drafts:      missingLock.drafts,
			Admission:   missingLock.proposer,
			Root:        "/workspace/.basalt",
			AcquireLock: nil,
		})
		_, err = noLock.Handoff(context.Background(), us22HandoffRequest(t))
		require.Error(t, err, "实例锁端口缺失同样失败关闭")
		require.Zero(t, len(missingLock.proposer.requests))
	})
}

// H2：来源文件里的 kind / ref 被程序覆盖，其余声明字段逐字保留。
// 逆改点：直接透传管理员文件 → Kind / Ref 断言红，且原始 ref 会出现在请求里。
func TestUS22_Handoff_ForcesSourceFields(t *testing.T) {
	rig := newUS22HandoffRig(t)
	result, err := rig.handoff.Handoff(context.Background(), us22HandoffRequest(t))
	require.NoError(t, err)
	require.Len(t, rig.proposer.requests, 1)

	request := rig.proposer.requests[0]
	require.Equal(t, "weekly-report", request.SkillName)
	require.Len(t, request.Files, 1)
	require.Equal(t, "cli:alice", rig.proposer.actors[0].Subject)

	require.NotContains(t, string(request.SourceJSON), "admin-note",
		"管理员文件里的 ref 不得出现在转交请求中")
	forced, err := core.ParseAdmissionSource(request.SourceJSON)
	require.NoError(t, err)
	require.Equal(t, "model_proposal", forced.Kind, "kind 由程序强制")
	require.Equal(t, "learning/draft/"+us22HandoffDraftID, forced.Ref, "ref 由程序强制")
	require.Equal(t, "user_confirmed", forced.Confirmation, "确认态原样保留")
	require.Equal(t, "人工确认后转交", forced.Notes, "其余声明字段保留")
	require.Equal(t, []string{"跳过跨团队汇总"}, forced.Counterexamples)
	require.Equal(t, []string{"msg_1"}, forced.Evidence)
	require.Equal(t, []string{"单次样本"}, forced.Limitations)

	require.Equal(t, "weekly-report", result.Draft.Name, "回执携带草案元数据")
	require.Equal(t, "adm_handoff01", result.Candidate.CandidateID, "回执原样返回 propose 结果")
}

// H3：确认态不是 user_confirmed 时拒绝，且拒绝发生在取锁之前（纯校验零副作用）。
// 逆改点：接受 model_inferred → 本用例红；把校验挪到取锁之后 → acquires 断言红。
func TestUS22_Handoff_RequiresUserConfirmed(t *testing.T) {
	for _, confirmation := range []string{"model_inferred", "direct", "relayed"} {
		t.Run(confirmation, func(t *testing.T) {
			rig := newUS22HandoffRig(t)
			request := us22HandoffRequest(t)
			request.SourceJSON = us22HandoffSourceJSON(confirmation)
			_, err := rig.handoff.Handoff(context.Background(), request)
			require.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
			require.Contains(t, err.Error(), "user_confirmed")
			require.Zero(t, rig.lock.acquires, "纯校验失败不得取实例锁")
			require.Zero(t, rig.drafts.loads, "纯校验失败不得读草案")
			require.Zero(t, len(rig.proposer.requests), "纯校验失败不得提议")
		})
	}

	t.Run("未知确认态被严格解码拒绝", func(t *testing.T) {
		rig := newUS22HandoffRig(t)
		request := us22HandoffRequest(t)
		request.SourceJSON = us22HandoffSourceJSON("assumed")
		_, err := rig.handoff.Handoff(context.Background(), request)
		require.ErrorIs(t, err, core.ErrAdmissionInvalidInput, "闭集外的确认态由严格解码拒绝")
		require.Zero(t, len(rig.proposer.requests))
	})

	t.Run("草案不存在", func(t *testing.T) {
		rig := newUS22HandoffRig(t)
		request := us22HandoffRequest(t)
		request.DraftID = "dft_missing"
		_, err := rig.handoff.Handoff(context.Background(), request)
		require.ErrorIs(t, err, core.ErrLearningDraftNotFound)
		require.Zero(t, len(rig.proposer.requests), "没有草案就不该有提议")
		require.Equal(t, 1, rig.lock.releases, "查无草案也要释放锁")
	})
}

// H4：转交只调用 Propose；七个写动作其余六个一次都不调用。
// 逆改点：实现路径直接调 Publish / Approve → 对应计数器非零，本用例红。
func TestUS22_Handoff_CallsProposeOnly(t *testing.T) {
	rig := newUS22HandoffRig(t)
	result, err := rig.handoff.Handoff(context.Background(), us22HandoffRequest(t))
	require.NoError(t, err)

	require.Len(t, rig.proposer.requests, 1, "Propose 恰好一次")
	require.Equal(t, rig.proposer.candidate, result.Candidate)
	require.Zero(t, rig.proposer.evaluate, "转交不评估")
	require.Zero(t, rig.proposer.approve, "转交不批准")
	require.Zero(t, rig.proposer.reject, "转交不拒绝")
	require.Zero(t, rig.proposer.cancel, "转交不取消")
	require.Zero(t, rig.proposer.publish, "转交不发布")
	require.Zero(t, rig.proposer.withdraw, "转交不撤回")

	t.Run("不可信主体不碰锁", func(t *testing.T) {
		untrusted := newUS22HandoffRig(t)
		request := us22HandoffRequest(t)
		request.Actor = core.Actor{Subject: "cli:unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI}
		_, err := untrusted.handoff.Handoff(context.Background(), request)
		require.ErrorIs(t, err, core.ErrAdmissionUntrustedActor)
		require.Zero(t, untrusted.lock.acquires, "身份校验先于取锁")
		require.Zero(t, len(untrusted.proposer.requests))
	})
}

// 以下四例是 T034 补遗（批复口径 C-29 / C-32 / FR-22-02）：全部走真实 PostgreSQL 与
// 真实抽取服务——「只读消息」「证据脱离消息仍可复核」「装配缺陷 fail-closed」「终态引用
// 补齐」判的都是生产写路径与 SQL 的结果，用内存替身只会把断言变成同义反复。
// The four cases run the real stores: the properties under test belong to the production
// write path and its SQL, not to a stand-in.

// us22LearningService 用生产仓储装配抽取服务（启用、名单只含 ops）；草案根落在本轮临时
// 目录，越界写不会碰仓库。
func us22LearningService(t *testing.T, db *postgres.DB, logger *slog.Logger) *memory.LearningService {
	t.Helper()
	return memory.NewLearningService(memory.LearningDeps{
		Store:     postgres.NewLearningRepo(db),
		Messages:  postgres.NewSessionStore(db),
		Enabled:   true,
		Agents:    []string{us22RecoveryAgent},
		DraftRoot: filepath.Join(t.TempDir(), "drafts"),
		Logger:    logger,
		Now:       baselineClock,
	})
}

// us22LearningRunInput 构造一次已完成 web Run 的抽取输入。
func us22LearningRunInput(runID string, extractor core.MemoryExtractor) core.LearningRunInput {
	return core.LearningRunInput{
		RunID:       runID,
		Agent:       us22RecoveryAgent,
		SessionID:   us22RecoverySession,
		Channel:     "web",
		Provider:    "stub",
		Model:       "stub-model",
		ConfigHash:  "cfg-us22",
		CompletedAt: baselineClock(),
		Extractor:   extractor,
	}
}

// seedLearningSession 落一条 active 会话；caller_id 与种下的 Run 同源，窗口 JOIN 才成立。
func (e *us22RecoveryEnv) seedLearningSession(t *testing.T) {
	t.Helper()
	at := baselineClock().UnixMilli()
	_, err := e.raw.ExecContext(t.Context(), `
		INSERT INTO sessions (session_id, profile_name, channel, user_id, status,
		                      message_count, created_at, last_active_at, caller_id)
		VALUES ($1, $2, 'web', 'u1', 'active', 0, $3, $3, $4)`,
		us22RecoverySession, us22RecoveryAgent, at, us22RecoveryCaller)
	require.NoError(t, err)
}

// seedLearningMessage 落一条消息并返回主键；seq 显式给定（同会话内唯一）。
func (e *us22RecoveryEnv) seedLearningMessage(t *testing.T, runID string, seq int, role, content string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, e.raw.QueryRowContext(t.Context(), `
		INSERT INTO messages (session_id, run_id, seq, role, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		us22RecoverySession, runID, seq, role, content, baselineClock().UnixMilli()).Scan(&id))
	return id
}

// messageDigest 汇总全部消息行的身份、角色、正文、时刻与所属 Run；抽取前后逐字节相同
// 即证明「没有第二条追加路径、没有改写、没有删除」。
func (e *us22RecoveryEnv) messageDigest(t *testing.T) string {
	t.Helper()
	var digest string
	require.NoError(t, e.raw.QueryRowContext(t.Context(), `
		SELECT COALESCE(string_agg(
		         id::text || '|' || role || '|' || COALESCE(content, '') || '|' ||
		         created_at::text || '|' || COALESCE(run_id, ''), E'\n' ORDER BY id), '')
		  FROM messages`).Scan(&digest))
	return digest
}

// us22ScriptedExtractor 是抽取面协议桩：返回固定 JSON，只数调用次数。
type us22ScriptedExtractor struct {
	reply string
	calls int
}

func (x *us22ScriptedExtractor) ChatMemoryExtract(context.Context, core.ChatRequest) (core.ChatResponse, error) {
	x.calls++
	return core.ChatResponse{Content: x.reply}, nil
}

// us22FactReply 生成一条带 user 证据的合法 fact 项；quote 必须是窗口消息的逐字子串。
func us22FactReply(messageID int64, quote string) string {
	return fmt.Sprintf(`{"items":[{"kind":"fact","content":"用户偏好：%s",`+
		`"evidence":[{"message_id":%d,"role":"user","quote":%q}]}]}`, quote, messageID, quote)
}

// FR-22-02：启用且 Run 已完成，但 Runner 没装配冻结的抽取客户端 → 记 ERROR 后放弃；
// 不建抽取行、不发出模型调用（fail-closed，而不是静默跳过或退回默认客户端）。
// 逆改点：把 nil 抽取器当成静默跳过而不记日志 / 继续走到预写 → 日志或计数断言红。
func TestUS22_Hook_ExtractorNotAssembledFailsClosed(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	key := us22RecoveryKey
	env.seedRun(t, "run_no_extractor", us22RecoveryCaller, &key)
	env.seedLearningSession(t)
	env.seedLearningMessage(t, "run_no_extractor", 1, "user", "我习惯用中文写周报")

	logger, captured := newCapturedLogger()
	service := us22LearningService(t, env.db, logger)

	service.AfterRun(t.Context(), us22LearningRunInput("run_no_extractor", nil),
		core.RunResult{RunID: "run_no_extractor", Status: core.StatusCompleted})

	require.Contains(t, captured.String(), "level=ERROR", "装配缺陷必须记 ERROR")
	require.Contains(t, captured.String(), "未装配", "日志必须说明放弃原因")
	require.Equal(t, 0, env.countRows(t, `SELECT count(*) FROM memory_extractions`),
		"抽取器未装配时不得预写抽取行")
	require.Equal(t, 0, env.countRows(t, `SELECT count(*) FROM llm_calls`),
		"抽取器未装配时不得发出模型调用")
}

// C-29：抽取只读消息——合法输出照常提交，但消息行逐字节不变（无第二条追加路径、
// 无改写、无删除）。
// 逆改点：抽取顺手写一条 assistant / system 摘要消息，或回写 created_at → 摘要断言红。
func TestUS22_Messages_ReadOnly(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	key := us22RecoveryKey
	env.seedRun(t, "run_read_only", us22RecoveryCaller, &key)
	env.seedLearningSession(t)
	userID := env.seedLearningMessage(t, "run_read_only", 1, "user", "我习惯用中文写周报")
	env.seedLearningMessage(t, "run_read_only", 2, "assistant", "好的，我会记住。")
	before := env.messageDigest(t)
	require.NotEmpty(t, before)

	extractor := &us22ScriptedExtractor{reply: us22FactReply(userID, "我习惯用中文写周报")}
	service := us22LearningService(t, env.db, us22DiscardLogger())
	service.AfterRun(t.Context(), us22LearningRunInput("run_read_only", extractor),
		core.RunResult{RunID: "run_read_only", Status: core.StatusCompleted})

	require.Equal(t, 1, extractor.calls)
	var status string
	require.NoError(t, env.raw.QueryRowContext(t.Context(),
		`SELECT status FROM memory_extractions WHERE run_id = $1`, "run_read_only").Scan(&status))
	require.Equal(t, "success", status, "合法输出应提交成功")
	require.Equal(t, 1, env.countRows(t, `SELECT count(*) FROM memory_candidate_meta`),
		"合法证据应写出候选元数据")
	require.Equal(t, before, env.messageDigest(t),
		"抽取只读消息：不得追加、改写或删除任何消息行")
	require.Equal(t, 2, env.countRows(t, `SELECT count(*) FROM messages`))
}

// C-29：证据（消息主键、所属 Run、角色、逐字引文、消息时刻）随候选元数据落库；
// 消息被历史保留策略清理后证据仍可复核。
// 逆改点：证据只存一个可丢失的外键引用 / 时刻不落库 → 删除后读回断言红。
func TestUS22_Evidence_SurvivesMessageDeletion(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	key := us22RecoveryKey
	env.seedRun(t, "run_evidence", us22RecoveryCaller, &key)
	env.seedLearningSession(t)
	userID := env.seedLearningMessage(t, "run_evidence", 1, "user", "项目代号是玄武")
	env.seedLearningMessage(t, "run_evidence", 2, "assistant", "收到。")

	extractor := &us22ScriptedExtractor{reply: us22FactReply(userID, "项目代号是玄武")}
	service := us22LearningService(t, env.db, us22DiscardLogger())
	service.AfterRun(t.Context(), us22LearningRunInput("run_evidence", extractor),
		core.RunResult{RunID: "run_evidence", Status: core.StatusCompleted})
	require.Equal(t, 1, extractor.calls)

	var entryID int64
	require.NoError(t, env.raw.QueryRowContext(t.Context(),
		`SELECT entry_id FROM memory_candidate_meta`).Scan(&entryID))

	// 模拟历史保留策略清理消息：证据必须不依赖消息行仍然存在。
	_, err := env.raw.ExecContext(t.Context(), `DELETE FROM messages`)
	require.NoError(t, err)
	require.Equal(t, 0, env.countRows(t, `SELECT count(*) FROM messages`))

	meta, found, err := env.memory.LoadCandidateMeta(t.Context(), entryID)
	require.NoError(t, err)
	require.True(t, found, "候选元数据不得随消息删除消失")
	require.Len(t, meta.Evidence, 1)
	evidence := meta.Evidence[0]
	require.Equal(t, userID, evidence.MessageID, "证据必须保留原始消息主键")
	require.Equal(t, "run_evidence", evidence.RunID, "证据必须保留所属 Run")
	require.Equal(t, "user", evidence.Role)
	require.Equal(t, "项目代号是玄武", evidence.Quote, "证据必须是逐字引文")
	require.Equal(t, baselineClock(), evidence.MessageAt, "证据必须保留消息时刻")
}

// C-32 / B-17：已终态但引用未释放的 Run 由启动恢复补齐——`released_at` 落时间戳，
// Run 终态不被回滚。
// 逆改点：`releaseTerminalRefs` 只补资源引用而不补通知 / 凭据 → 通知引用保持 NULL，断言红。
func TestUS22_Recovery_TerminalRefsReleased(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	key := us22RecoveryKey
	env.seedRun(t, "run_refs", us22RecoveryCaller, &key)
	_, err := env.raw.ExecContext(t.Context(), `
		INSERT INTO run_notify_refs (run_id, agent_uid, channel_uid, channel_name)
		VALUES ('run_refs', 'agent-1', 'chan-1', 'ops')`)
	require.NoError(t, err)

	_, err = env.db.Recover(t.Context(), us22DiscardLogger(), baselineClock())
	require.NoError(t, err)

	var released sql.NullInt64
	require.NoError(t, env.raw.QueryRowContext(t.Context(),
		`SELECT released_at FROM run_notify_refs WHERE run_id = 'run_refs'`).Scan(&released))
	require.True(t, released.Valid, "启动恢复必须补齐已终态 Run 的残留引用")
	require.Equal(t, baselineClock().UnixMilli(), released.Int64)

	var status string
	require.NoError(t, env.raw.QueryRowContext(t.Context(),
		`SELECT status FROM runs WHERE run_id = 'run_refs'`).Scan(&status))
	require.Equal(t, "completed", status, "补齐释放不得回滚 Run 终态")
}
