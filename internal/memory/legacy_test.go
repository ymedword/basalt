package memory_test

// 本文件是**旧记忆只读导入**的验收面（D-18 / D-22 / T130）：四类对账互斥且不丢条目、
// 重复导入幂等、冲突不覆盖现状、来源只由受信任路径盖戳。
// 数据库侧的同名回归（来源写入 / 回读 / 重复导入）在 `internal/storage/postgres` 的
// 集成用例里，走真库判 CHECK 闭集与列还原。
// This file pins the import reconciliation on the unit level; the database-backed
// round-trip lives in the postgres integration tests.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// legacySourceFunc 让用例用函数直接实现 `LegacySource`。
// legacySourceFunc adapts a function to the LegacySource interface.
type legacySourceFunc func(ctx context.Context) ([]memory.LegacyEntry, error)

// Read 调用底层函数。
func (f legacySourceFunc) Read(ctx context.Context) ([]memory.LegacyEntry, error) { return f(ctx) }

// legacyRecordingStore 在共享替身之上记录 `InsertEntry` 收到的条目（用来判来源戳），
// 并可注入写库错误。
// legacyRecordingStore records inserted entries and can inject a write failure.
type legacyRecordingStore struct {
	*semanticFakeStore

	inserted  []memory.MemoryEntry
	insertErr error
}

// newLegacyRecordingStore 组装替身（兼容三方法仍走真实 markdown 载体）。
func newLegacyRecordingStore(t *testing.T) *legacyRecordingStore {
	t.Helper()
	return &legacyRecordingStore{semanticFakeStore: newSemanticFakeStore(t.TempDir())}
}

// InsertEntry 记录来源戳后委托共享替身；可注入失败。
func (s *legacyRecordingStore) InsertEntry(ctx context.Context, entry memory.MemoryEntry) (int64, error) {
	if s.insertErr != nil {
		return 0, s.insertErr
	}
	s.inserted = append(s.inserted, entry)
	return s.semanticFakeStore.InsertEntry(ctx, entry)
}

// TestImportLegacy_ReconcilesEveryBucket 守 D-18 的逐条对账：源里的每一条都落在
// 导入 / 跳过 / 冲突 / 不可读四类之一，条数加总等于源条数；导入行写 active +
// legacy_import，冲突行**不改写**权威库里的现状。
// 逆改点：① 把不可读 / 空正文条目静默丢弃 → Total 少于源条数；② 冲突行照写或改状态
// → 预置 deleted 行被覆盖；③ 导入行不带来源戳 → inserted 来源断言红。
// 验收点：D-18 / D-22 / FR-013。
func TestImportLegacy_ReconcilesEveryBucket(t *testing.T) {
	ctx := context.Background()
	store := newLegacyRecordingStore(t)
	svc := memory.NewSemanticStore(memory.SemanticDeps{Store: store})

	// 预置两条权威库条目：一条生效（重复导入应跳过）、一条已删除（应记冲突且不被覆盖）。
	existingID, err := store.semanticFakeStore.InsertEntry(ctx, memory.MemoryEntry{
		Scope: core.GlobalScope, Section: core.SectionArchive, Content: "已存在生效", State: "active",
	})
	require.NoError(t, err)
	deletedID, err := store.semanticFakeStore.InsertEntry(ctx, memory.MemoryEntry{
		Scope: core.GlobalScope, Section: core.SectionArchive, Content: "已删除的知识", State: "deleted",
	})
	require.NoError(t, err)

	source := legacySourceFunc(func(context.Context) ([]memory.LegacyEntry, error) {
		return []memory.LegacyEntry{
			{Scope: core.GlobalScope, Section: core.SectionArchive, Content: "新的知识"},
			{Scope: core.GlobalScope, Section: core.SectionArchive, Content: "已存在生效"},
			{Scope: core.GlobalScope, Section: core.SectionArchive, Content: "已删除的知识"},
			{Scope: core.GlobalScope, Section: core.SectionArchive, Content: "坏行", Unreadable: "编码损坏"},
			{Scope: core.GlobalScope, Section: core.SectionArchive, Content: "   "},
			{Scope: core.GlobalScope, Section: "", Content: "无分区"},
			{Scope: core.GlobalScope, Section: core.SectionArchive, Content: "新的知识"},
		}, nil
	})

	report, err := memory.ImportLegacy(ctx, source, svc)
	require.NoError(t, err)

	require.Equal(t, 7, report.Total(), "四类对账必须覆盖源的每一条")
	require.Len(t, report.Imported, 1)
	require.Equal(t, "新的知识", report.Imported[0].Content)
	require.Positive(t, report.Imported[0].EntryID)

	require.Len(t, report.Skipped, 2, "同批内重复的正文第二次按跳过记账")
	require.Equal(t, existingID, report.Skipped[0].EntryID)
	require.Equal(t, report.Imported[0].EntryID, report.Skipped[1].EntryID)

	require.Len(t, report.Conflicts, 1)
	require.Equal(t, deletedID, report.Conflicts[0].EntryID)
	require.Equal(t, "deleted", report.Conflicts[0].State)

	require.Len(t, report.Unreadable, 3)
	require.Equal(t, "deleted", store.entries[deletedID].State, "冲突条不得被改写")

	require.Len(t, store.inserted, 1, "只有真正新增的那条走写入口")
	require.Equal(t, memory.SourceLegacyImport, store.inserted[0].Source(), "导入行必须盖 legacy_import")
	require.Equal(t, "active", store.inserted[0].State, "旧载体条目本来就是生效的")

	loaded, found, err := store.LoadEntry(ctx, report.Imported[0].EntryID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memory.SourceLegacyImport, loaded.Source(), "回读必须还原来源")
}

// TestImportLegacy_SecondRunOnlySkips 守"重复导入幂等"：同一个源连跑两次，第二次
// 零新增、全记跳过，权威库行数不变。
// 逆改点：幂等键换成"每次都插"→ 第二次 imported 非零 → 红。
// 验收点：D-18。
func TestImportLegacy_SecondRunOnlySkips(t *testing.T) {
	ctx := context.Background()
	store := newLegacyRecordingStore(t)
	svc := memory.NewSemanticStore(memory.SemanticDeps{Store: store})

	source := legacySourceFunc(func(context.Context) ([]memory.LegacyEntry, error) {
		return []memory.LegacyEntry{
			{Scope: core.GlobalScope, Section: core.SectionCore, Content: "偏好：先给结论"},
			{Scope: core.Scope{Agent: "demo"}, Section: core.SectionArchive, Content: "demo 的历史约定"},
		}, nil
	})

	first, err := memory.ImportLegacy(ctx, source, svc)
	require.NoError(t, err)
	require.Len(t, first.Imported, 2)
	require.Empty(t, first.Skipped)

	second, err := memory.ImportLegacy(ctx, source, svc)
	require.NoError(t, err)
	require.Empty(t, second.Imported, "重复导入不得再写一行")
	require.Len(t, second.Skipped, 2)
	require.Equal(t, first.Imported[0].EntryID, second.Skipped[0].EntryID)
	require.Equal(t, 2, len(store.entries), "权威库行数保持不变")
}

// TestImportLegacy_StoreFailureIsReported 守"导入不完整不得当成功"：写库失败必须
// 带着已完成的报告返回错误，不得吞掉错误返回一份看起来正常的报告。
// 逆改点：忽略 InsertEntry 的 error 继续循环 → 返回 err == nil → 红。
func TestImportLegacy_StoreFailureIsReported(t *testing.T) {
	ctx := context.Background()
	store := newLegacyRecordingStore(t)
	svc := memory.NewSemanticStore(memory.SemanticDeps{Store: store})
	store.insertErr = errors.New("库写失败")

	source := legacySourceFunc(func(context.Context) ([]memory.LegacyEntry, error) {
		return []memory.LegacyEntry{{Scope: core.GlobalScope, Section: core.SectionArchive, Content: "新知识"}}, nil
	})

	report, err := memory.ImportLegacy(ctx, source, svc)
	require.Error(t, err)
	require.ErrorContains(t, err, "导入旧记忆条目失败")
	require.Empty(t, report.Imported, "失败条目不得计入已导入")
}

// TestImportLegacy_RequiresSourceAndStore 守装配缺失 fail-closed：没有源或没有载体
// 时不得返回"零条导入"的假通过。
// 逆改点：把 nil 当空源直接返回空报告 → 断言红。
func TestImportLegacy_RequiresSourceAndStore(t *testing.T) {
	ctx := context.Background()
	svc := memory.NewSemanticStore(memory.SemanticDeps{Store: newLegacyRecordingStore(t)})
	source := legacySourceFunc(func(context.Context) ([]memory.LegacyEntry, error) { return nil, nil })

	_, err := memory.ImportLegacy(ctx, nil, svc)
	require.Error(t, err, "没有源就没有可导入的东西")

	_, err = memory.ImportLegacy(ctx, source, nil)
	require.Error(t, err, "没有载体不得静默通过")

	unassembled := memory.NewSemanticStore(memory.SemanticDeps{})
	_, err = memory.ImportLegacy(ctx, source, unassembled)
	require.Error(t, err, "装配失败的载体必须拒绝")
}

// TestMemory_SourceIsNotCallerAssignable 守"来源不可由普通写入伪造"：`MemoryEntry` 的
// source 字段不导出（普通入口没有来源入参，编译期就写不进来），普通写入落库时来源为
// 空串、由存储层按状态推导；只有 `ImportLegacy` 这条受信任路径能把条目盖成
// `legacy_import`。
// 逆改点：把 source 导出或给 `Propose` 加来源入参 → 本测试的"普通写入不带来源"断言红
// （导出字段本身属改已冻结接口）。
// 验收点：用户 2026-10-28 指令第 3 条（不允许调用方伪造导入来源）。
func TestMemory_SourceIsNotCallerAssignable(t *testing.T) {
	ctx := context.Background()
	store := newLegacyRecordingStore(t)
	svc := memory.NewSemanticStore(memory.SemanticDeps{Store: store})

	_, err := svc.Propose(ctx, memory.ProposeRequest{
		Principal: testCallerPrincipal(),
		Scope:     core.GlobalScope,
		Content:   "普通提议",
	})
	require.NoError(t, err)
	require.Len(t, store.inserted, 1)
	require.Empty(t, store.inserted[0].Source(), "普通写入没有来源入参，来源由存储层按状态推导")
	require.Equal(t, "candidate", store.inserted[0].State)

	store.inserted = nil
	source := legacySourceFunc(func(context.Context) ([]memory.LegacyEntry, error) {
		return []memory.LegacyEntry{{Scope: core.GlobalScope, Section: core.SectionArchive, Content: "旧记忆"}}, nil
	})
	_, err = memory.ImportLegacy(ctx, source, svc)
	require.NoError(t, err)
	require.Len(t, store.inserted, 1)
	require.Equal(t, memory.SourceLegacyImport, store.inserted[0].Source(), "只有导入路径能盖来源戳")
}
