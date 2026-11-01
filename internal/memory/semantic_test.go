package memory_test

// 本文件是**语义记忆存储的召回面**（颗粒度文档 §3.1.4 / P-07 / T049）：语义为缺省模式、
// `keyword` 保留旧行为、身份先行拒绝、装配缺失 fail-closed。freeze / injection /
// lifecycle 三个文件共用这里的测试替身——替身的兼容三方法走**真实 markdown 载体**，
// 只给 `RecallEntries` 加计数与可注入错误，避免测试复制分区解析与回退逻辑。
// This file is the semantic store's recall surface; the fakes are shared with the
// freeze / injection / lifecycle files and reuse the real markdown carrier.

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// fixedFreezeTime 是冻结快照用例的固定时钟（不读全局时间，断言可复现）。
var fixedFreezeTime = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

// semanticFakeStore 是 `memory.MemoryStore` 的测试替身：兼容三方法直接委托真实
// markdown 载体（分区解析、生效层回退、写锁全走生产实现），语义召回可计数、可注入错误。
// The fake delegates the compat surface to the real markdown carrier and only stubs the
// semantic recall call.
type semanticFakeStore struct {
	*memory.MarkdownStore

	appends       int
	recallCalls   int
	recallErr     error
	results       []memory.MemoryEntry
	lastPrincipal core.Principal
	lastVector    []float32
	lastModel     string

	// 生命周期面用一张内存表：兼容三方法仍走真实 markdown 载体，六态只在这里模拟；
	// lifecycleCalls 统计每一个生命周期端口的调用，用来判"无效身份不触碰载体"。
	entries        map[int64]memory.MemoryEntry
	confirmedBy    map[int64]string
	nextID         int64
	lifecycleCalls int
}

func newSemanticFakeStore(root string) *semanticFakeStore {
	return &semanticFakeStore{
		MarkdownStore: memory.NewMarkdownStore(root),
		entries:       make(map[int64]memory.MemoryEntry),
		confirmedBy:   make(map[int64]string),
	}
}

// LoadEntry 按主键读内存表。
func (f *semanticFakeStore) LoadEntry(_ context.Context, id int64) (memory.MemoryEntry, bool, error) {
	f.lifecycleCalls++
	entry, ok := f.entries[id]
	return entry, ok, nil
}

// FindEntryByContent 按（分层, 分区, 正文）查同源条目。
func (f *semanticFakeStore) FindEntryByContent(
	_ context.Context, scope core.Scope, section core.Section, content string,
) (memory.MemoryEntry, bool, error) {
	f.lifecycleCalls++
	for _, entry := range f.entries {
		if entry.Scope == scope && entry.Section == section && entry.Content == content {
			return entry, true, nil
		}
	}
	return memory.MemoryEntry{}, false, nil
}

// ListEntries 按分层与状态过滤内存表（主键倒序），窗口与生产实现同口径。
// ListEntries mirrors the production ordering and window over the in-memory table.
func (f *semanticFakeStore) ListEntries(_ context.Context, query memory.ListQuery) ([]memory.MemoryEntry, error) {
	f.lifecycleCalls++
	out := make([]memory.MemoryEntry, 0, len(f.entries))
	for _, entry := range f.entries {
		if entry.Scope != query.Scope {
			continue
		}
		if query.State != "" && entry.State != query.State {
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

// InsertEntry 分配主键并落内存表；state 由调用方给定，替身不改写。确认人按条目自带
// 的值记进 confirmedBy（与生产实现同口径），空值不记。
func (f *semanticFakeStore) InsertEntry(_ context.Context, entry memory.MemoryEntry) (int64, error) {
	f.lifecycleCalls++
	f.nextID++
	entry.ID = f.nextID
	f.entries[entry.ID] = entry
	if who := entry.ConfirmedBy(); who != "" {
		f.confirmedBy[entry.ID] = who
	}
	return entry.ID, nil
}

// UpdateEntryState 只迁移状态与 confirmed_by，不改正文。
func (f *semanticFakeStore) UpdateEntryState(_ context.Context, id int64, state string, confirmedBy string) error {
	f.lifecycleCalls++
	entry, ok := f.entries[id]
	if !ok {
		return core.ErrConflict
	}
	entry.State = state
	f.entries[id] = entry
	if confirmedBy != "" {
		f.confirmedBy[id] = confirmedBy
	}
	return nil
}

// ReplaceEntry 模拟存储侧的事务守卫：目标必须仍是 active，否则拒绝且不插入。
func (f *semanticFakeStore) ReplaceEntry(
	_ context.Context, oldID int64, retireState string, next memory.MemoryEntry,
) (int64, error) {
	f.lifecycleCalls++
	old, ok := f.entries[oldID]
	if !ok || old.State != "active" {
		return 0, core.ErrConflict
	}
	old.State = retireState
	f.entries[oldID] = old
	f.nextID++
	next.ID = f.nextID
	f.entries[next.ID] = next
	if who := next.ConfirmedBy(); who != "" {
		f.confirmedBy[next.ID] = who
	}
	return next.ID, nil
}

// Append 计数后委托真实载体（"Run 结束不得自动写"的判据面）。
func (f *semanticFakeStore) Append(ctx context.Context, scope core.Scope, section core.Section, content string) error {
	f.appends++
	return f.MarkdownStore.Append(ctx, scope, section, content)
}

// RecallEntries 记录调用事实（每次召回都带上当次 Principal）并可注入拒绝错误。
func (f *semanticFakeStore) RecallEntries(
	_ context.Context, query memory.RecallQuery, vector []float32, model string,
) ([]memory.MemoryEntry, error) {
	f.recallCalls++
	f.lastPrincipal = query.Principal
	f.lastVector = vector
	f.lastModel = model
	if f.recallErr != nil {
		return nil, f.recallErr
	}
	if f.results != nil {
		return f.results, nil
	}
	// 未预设结果时返回生命周期表的快照（按主键升序，读数可复现）；状态过滤由层内
	// activeOnly 负责，替身不替它判——那正是用例要守的守点。
	entries := make([]memory.MemoryEntry, 0, len(f.entries))
	for _, entry := range f.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, nil
}

// fakeEmbedder 记录 embedding 调用次数与输入文本，可注入错误或固定回执。
type fakeEmbedder struct {
	calls int
	texts []string
	batch memory.EmbedBatch
	err   error
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) (memory.EmbedBatch, error) {
	e.calls++
	e.texts = append([]string(nil), texts...)
	if e.err != nil {
		return memory.EmbedBatch{}, e.err
	}
	if e.batch.Vectors == nil {
		return memory.EmbedBatch{Vectors: [][]float32{{0.25, 0.5}}, Dimensions: 2, Model: "test-embed"}, nil
	}
	return e.batch, nil
}

// newSemanticForTest 组装一份"真实 markdown 载体 + 计数替身"的语义记忆存储。
func newSemanticForTest(t *testing.T) (*memory.SemanticStore, *semanticFakeStore, *fakeEmbedder) {
	t.Helper()
	store := newSemanticFakeStore(t.TempDir())
	embedder := &fakeEmbedder{}
	return memory.NewSemanticStore(memory.SemanticDeps{Store: store, Embedder: embedder}), store, embedder
}

// testCallerPrincipal 是合法的调用方身份（非管理员、带一个具名 Agent scope）。
func testCallerPrincipal() core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller-1",
		KeyID:    "key-1",
		Scopes:   []string{core.AgentInvokeScopePrefix + "assistant"},
		Actor:    core.ActorCaller,
	}
}

// TestSemanticStore_RecallDefaultsToSemanticAndKeepsKeywordFallback 守 P-07 的契约变更：
// 不传 `mode` = 语义召回（调用一次 Embedder 并把向量与模型交给载体）；`mode=keyword`
// = 旧整串子串行为（不调用 Embedder、结果不带分数）。
// 验收点：P-07 / FR-016。
func TestSemanticStore_RecallDefaultsToSemanticAndKeepsKeywordFallback(t *testing.T) {
	svc, store, embedder := newSemanticForTest(t)
	store.results = []memory.MemoryEntry{{
		ID:      7,
		Scope:   core.GlobalScope,
		Section: core.SectionArchive,
		Content: "服务超时前先看连接池",
		State:   "active",
		Score:   0.91,
	}}
	principal := testCallerPrincipal()

	got, err := svc.Recall(ctx, memory.RecallQuery{
		Principal: principal,
		Scope:     core.GlobalScope,
		Query:     "服务为什么没响应",
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(7), got[0].ID, "语义召回保留载体给的名次与条目")
	require.InDelta(t, 0.91, got[0].Score, 1e-9)
	require.Equal(t, 1, embedder.calls, "缺省模式必须走 embedding")
	require.Equal(t, []string{"服务为什么没响应"}, embedder.texts)
	require.Equal(t, []float32{0.25, 0.5}, store.lastVector)
	require.Equal(t, "test-embed", store.lastModel)
	require.Equal(t, principal, store.lastPrincipal, "身份必须显式交给载体判定")

	// keyword 模式：旧行为逐字保留，**不**触碰 Embedder。
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionArchive, "重启前先核对连接池上限"))
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionArchive, "另一条不相干细节"))
	callsBefore := embedder.calls

	got, err = svc.Recall(ctx, memory.RecallQuery{
		Principal: principal,
		Scope:     core.GlobalScope,
		Query:     "连接池",
		Mode:      memory.RecallModeKeyword,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "重启前先核对连接池上限", got[0].Content)
	require.Equal(t, core.SectionArchive, got[0].Section)
	require.Zero(t, got[0].Score, "keyword 模式不产生向量分数")
	require.Equal(t, callsBefore, embedder.calls, "keyword 模式不得调用 Embedder")
}

// TestSemanticStore_RecallRejectsUnusablePrincipalBeforeAnyWork 守 C-28：零值 / 无效身份
// 在触碰载体与 Embedder **之前**拒绝——不得默认成管理员，也不得先花一次 embedding。
// 验收点：C-28 / FR-015。
func TestSemanticStore_RecallRejectsUnusablePrincipalBeforeAnyWork(t *testing.T) {
	svc, store, embedder := newSemanticForTest(t)

	_, err := svc.Recall(ctx, memory.RecallQuery{Scope: core.GlobalScope, Query: "任意"})
	require.ErrorIs(t, err, core.ErrUnauthorized)
	require.Zero(t, store.recallCalls, "零值身份不得触碰载体")
	require.Zero(t, embedder.calls, "零值身份不得消耗 embedding 调用")

	// 无效身份（调用方携带管理面 scope）同样拒绝。
	invalid := testCallerPrincipal()
	invalid.Scopes = []string{core.ScopeDiagnosticsRead}
	_, err = svc.Recall(ctx, memory.RecallQuery{Principal: invalid, Query: "任意"})
	require.ErrorIs(t, err, core.ErrUnauthorized)
	require.Zero(t, store.recallCalls)
	require.Zero(t, embedder.calls)
}

// TestSemanticStore_RecallRejectsUnknownModeWithoutSideEffects 守模式闭集：未知取值
// 显式报错，不落回任一旧路径。
// 验收点：P-07。
func TestSemanticStore_RecallRejectsUnknownModeWithoutSideEffects(t *testing.T) {
	svc, store, embedder := newSemanticForTest(t)

	_, err := svc.Recall(ctx, memory.RecallQuery{Principal: testCallerPrincipal(), Query: "任意", Mode: "fuzzy"})
	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown recall mode "fuzzy"`)
	require.Zero(t, store.recallCalls)
	require.Zero(t, embedder.calls)
}

// TestSemanticStore_RecallAppliesLimit 守返回条数上限（<= 0 不截断，超出按载体名次截断）。
// 验收点：§3.1.4 `Recall`。
func TestSemanticStore_RecallAppliesLimit(t *testing.T) {
	svc, store, _ := newSemanticForTest(t)
	store.results = []memory.MemoryEntry{
		{ID: 1, Content: "第一条", Score: 0.9, State: "active"},
		{ID: 2, Content: "第二条", Score: 0.8, State: "active"},
		{ID: 3, Content: "第三条", Score: 0.7, State: "active"},
	}
	principal := testCallerPrincipal()

	got, err := svc.Recall(ctx, memory.RecallQuery{Principal: principal, Query: "条目", Limit: 2})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(1), got[0].ID)
	require.Equal(t, int64(2), got[1].ID)

	got, err = svc.Recall(ctx, memory.RecallQuery{Principal: principal, Query: "条目"})
	require.NoError(t, err)
	require.Len(t, got, 3, "Limit <= 0 表示不截断")
}

// TestSemanticStore_FailsClosedWhenNotAssembled 守装配失败不静默：缺 Store 时各入口
// 全部拒绝；有 Store 但缺 Embedder 时语义召回拒绝、keyword 路径仍可用（旧行为不依赖
// embedding）。**不得** panic，也**不得**静默返回空记忆。
// 验收点：§3.1.4 装配面。
func TestSemanticStore_FailsClosedWhenNotAssembled(t *testing.T) {
	empty := memory.NewSemanticStore(memory.SemanticDeps{})
	_, err := empty.Recall(ctx, memory.RecallQuery{Principal: testCallerPrincipal(), Query: "任意"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "semantic store is not assembled")
	_, err = empty.Freeze(ctx, memory.FreezeRequest{RunID: "run-x", Principal: adminPrincipal(), Now: fixedFreezeTime})
	require.Error(t, err)
	require.Contains(t, err.Error(), "semantic store is not assembled")
	err = empty.Append(ctx, core.GlobalScope, core.SectionCore, "任意")
	require.Error(t, err)
	require.Contains(t, err.Error(), "semantic store is not assembled")
	_, err = empty.Load(ctx, core.GlobalScope)
	require.Error(t, err)
	require.Contains(t, err.Error(), "semantic store is not assembled")
	_, err = empty.RecallByKeyword(ctx, core.GlobalScope, "任意")
	require.Error(t, err)
	require.Contains(t, err.Error(), "semantic store is not assembled")

	// 缺 Embedder：语义召回 fail-closed，keyword 路径不受影响。
	store := newSemanticFakeStore(t.TempDir())
	noEmbedder := memory.NewSemanticStore(memory.SemanticDeps{Store: store})
	require.NoError(t, noEmbedder.Append(ctx, core.GlobalScope, core.SectionArchive, "连接池细节"))
	_, err = noEmbedder.Recall(ctx, memory.RecallQuery{Principal: testCallerPrincipal(), Query: "连接池"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires an embedder")
	require.Zero(t, store.recallCalls)
	got, err := noEmbedder.Recall(ctx, memory.RecallQuery{
		Principal: testCallerPrincipal(), Query: "连接池", Mode: memory.RecallModeKeyword,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
}
