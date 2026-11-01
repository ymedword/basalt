package memory_test

// 本文件是 **Run 冻结**的验收面（颗粒度文档 §3.1.4 / 设计图 5 / C-11 / FR-015）：注入
// 文本在 Run 开始读一次并冻结、运行内写入只从新 Run 生效、归档区仍按调用时检索；
// 冻结**不冻结权限**——每次召回都按当前身份重新判定。
// This file covers the run-start freeze: one read at start, changes visible only to new
// runs, call-time archive recall, and permissions re-checked after the freeze.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// TestMemory_RunFrozenSnapshot 守 C-11：Run 开始冻结注入文本；运行中新增核心 / 归档条目
// 都不改动本 Run 的快照（修改从新 Run 生效）；归档区仍按调用时检索，不受快照影响。
// 逆改点：Freeze 改成每轮重读或返回指针 → "冻结快照不随写入变化"红；Recall 改成读快照
// → "运行内新增归档条目可见"红。
// 验收点：C-11 / FR-015。
func TestMemory_RunFrozenSnapshot(t *testing.T) {
	svc, _, _ := newSemanticForTest(t)
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "初始偏好：文风简洁"))
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionArchive, "运行前归档：代号玄武"))

	snap, err := svc.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-1",
		Scope:     core.GlobalScope,
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime,
	})
	require.NoError(t, err)
	require.Equal(t, "run-1", snap.RunID)
	require.True(t, snap.FrozenAt.Equal(fixedFreezeTime))
	require.Equal(t, "## 核心记忆\n\n- 初始偏好：文风简洁", snap.MemoryText)
	require.NotContains(t, snap.MemoryText, "代号玄武", "归档区不进注入面")

	// 运行中写入：核心区与归档区各一条。
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "运行中新增：偏好表格"))
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionArchive, "运行中归档：代号青蚨"))

	require.Equal(t, "## 核心记忆\n\n- 初始偏好：文风简洁", snap.MemoryText,
		"冻结快照不随运行中写入变化")
	require.NotContains(t, snap.MemoryText, "运行中新增")

	// 新 Run 重新读：运行中的写入生效。
	snap2, err := svc.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-2",
		Scope:     core.GlobalScope,
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime.Add(time.Minute),
	})
	require.NoError(t, err)
	require.Contains(t, snap2.MemoryText, "运行中新增：偏好表格", "修改从新 Run 生效")
	require.NotContains(t, snap2.MemoryText, "代号青蚨", "归档区仍不进注入面")

	// 运行内召回按调用时检索：新归档条目立即可见，且不进注入快照。
	got, err := svc.Recall(ctx, memory.RecallQuery{
		Principal: testCallerPrincipal(),
		Scope:     core.GlobalScope,
		Query:     "青蚨",
		Mode:      memory.RecallModeKeyword,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "运行中归档：代号青蚨", got[0].Content)
	require.NotContains(t, snap.MemoryText, got[0].Content)
}

// TestMemory_PermissionStillCheckedAfterFreeze 守 FR-015 的"冻结内容不冻结权限"：冻结成功
// 不构成后续动作的授权；召回每次按**当次** Principal 判定，撤销后立即拒绝，且冻结文本
// 本身不因权限变化而改变。
// 逆改点：Recall 复用冻结时的授权结论 / 不再传 Principal → 撤销后仍成功 → 红。
// 验收点：FR-015 / C-11。
func TestMemory_PermissionStillCheckedAfterFreeze(t *testing.T) {
	svc, store, _ := newSemanticForTest(t)
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "既有偏好：先给结论"))
	store.results = []memory.MemoryEntry{{
		ID:      9,
		Scope:   core.GlobalScope,
		Section: core.SectionArchive,
		Content: "既有偏好：先给结论",
		State:   "active",
		Score:   0.8,
	}}

	snap, err := svc.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-1",
		Scope:     core.GlobalScope,
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime,
	})
	require.NoError(t, err)
	require.NotEmpty(t, snap.MemoryText)
	require.Zero(t, store.recallCalls, "冻结只读注入面，不触发召回")

	// 运行中授权被撤销：下一次召回按当前状态拒绝。
	store.recallErr = core.ErrForbidden
	_, err = svc.Recall(ctx, memory.RecallQuery{
		Principal: testCallerPrincipal(),
		Scope:     core.GlobalScope,
		Query:     "结论",
	})
	require.ErrorIs(t, err, core.ErrForbidden, "冻结不冻结权限：召回按当前授权判定")
	require.Equal(t, 1, store.recallCalls, "被拒绝的召回同样经过了权限判定")

	// 冻结文本本身不因权限变化而改变（内容冻结 ≠ 权限冻结）。
	require.Contains(t, snap.MemoryText, "既有偏好：先给结论")
	require.Equal(t, "## 核心记忆\n\n- 既有偏好：先给结论", snap.MemoryText)

	// 身份仍是显式必填：冻结之后再传零值 Principal 依然拒绝，且不触碰载体。
	store.recallErr = nil
	_, err = svc.Recall(ctx, memory.RecallQuery{Scope: core.GlobalScope, Query: "结论"})
	require.ErrorIs(t, err, core.ErrUnauthorized)
	require.Equal(t, 1, store.recallCalls, "零值身份不得到达载体")

	// 恢复授权后正常召回，且每次调用都重新判定（不是一次性的冻结结论）。
	got, err := svc.Recall(ctx, memory.RecallQuery{
		Principal: testCallerPrincipal(),
		Scope:     core.GlobalScope,
		Query:     "结论",
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(9), got[0].ID)
	require.Equal(t, 2, store.recallCalls)
}
