package memory_test

// 本文件是**生命周期存续边界**的验收面（颗粒度文档 §3.1.4 / 设计图 5 / C-21 / FR-013）：
// Run 结束**不自动抽取**候选、不自动写任何条目；长期记忆只由显式动作产生。
// `Propose` / `Correct` / `Delete` 与六态迁移归 T050，本文件先把"读路径只读"钉住。
// This file pins the persistence boundary: freeze and recall are read-only, so a run
// that performs no explicit action leaves the layer byte-for-byte unchanged.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// TestMemory_NoAutoProposeAtRunEnd 守 FR-013：一次 Run（开始冻结 → 运行中召回 → 结束）
// 全程零写入；只有显式动作（`save_memory` / 显式提议）才写，且直接写 active。
// 逆改点：Freeze / Recall 里加自动抽取或候选落库 → 层原文与写入计数变化 → 红。
// 验收点：FR-013 / C-21。
func TestMemory_NoAutoProposeAtRunEnd(t *testing.T) {
	svc, store, _ := newSemanticForTest(t)
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "既有偏好：先给结论"))

	baseline, err := svc.Load(ctx, core.GlobalScope)
	require.NoError(t, err)
	require.NotEmpty(t, baseline)
	appendsBefore := store.appends

	// 模拟一次完整的 Run：冻结注入文本 → 运行内召回 → 结束（没有任何显式动作）。
	snap, err := svc.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-end",
		Scope:     core.GlobalScope,
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime,
	})
	require.NoError(t, err)
	require.NotEmpty(t, snap.MemoryText)
	_, err = svc.Recall(ctx, memory.RecallQuery{
		Principal: testCallerPrincipal(),
		Scope:     core.GlobalScope,
		Query:     "结论",
	})
	require.NoError(t, err)

	after, err := svc.Load(ctx, core.GlobalScope)
	require.NoError(t, err)
	require.Equal(t, baseline, after, "Run 结束不得自动抽取 / 写候选（层原文逐字节不变）")
	require.Equal(t, appendsBefore, store.appends, "读路径不得产生任何写入")

	// 显式动作才写（`save_memory` 直接写 active，不经过候选态）；这里判的是"只有显式
	// 调用才增加写入计数"，六态迁移由 T050 判卷。
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "显式保存：先给结论"))
	require.Equal(t, appendsBefore+1, store.appends)
	written, err := svc.Load(ctx, core.GlobalScope)
	require.NoError(t, err)
	require.Contains(t, written, "显式保存：先给结论")
}

// ---------------------------------------------------------------------------
// T050：六态生命周期（candidate / active / superseded / corrected / deleted /
// conflicted）。候选不自动晋升、不参与召回；纠正原子替换；删除软删；冲突待裁决。
// Six-state lifecycle cases: candidate promotion, correction, soft delete, conflict.
// ---------------------------------------------------------------------------

// TestMemory_CandidateNotRecalledUntilConfirmed 守 C-21 / 设计图 5：显式提议只产生
// candidate，确认前**不参与召回**；确认是唯一晋升入口并记录 confirmed_by；对已非候选
// 的条目再次确认按状态冲突拒绝。
// 逆改点：Propose 直接写 active，或 Recall 不过滤状态 → 确认前的召回断言红。
// 验收点：C-21 / FR-013。
func TestMemory_CandidateNotRecalledUntilConfirmed(t *testing.T) {
	svc, store, _ := newSemanticForTest(t)
	principal := testCallerPrincipal()

	id, err := svc.Propose(ctx, memory.ProposeRequest{
		Principal: principal,
		Scope:     core.GlobalScope,
		Content:   "候选事实：发布窗口在周一",
	})
	require.NoError(t, err)
	require.NotZero(t, id)
	require.Equal(t, "candidate", store.entries[id].State, "显式提议只产生候选，不得直接生效")
	require.Equal(t, core.SectionArchive, store.entries[id].Section, "候选默认落归档区，不进注入面")

	got, err := svc.Recall(ctx, memory.RecallQuery{
		Principal: principal,
		Scope:     core.GlobalScope,
		Query:     "发布窗口",
	})
	require.NoError(t, err)
	require.Empty(t, got, "候选不得参与召回（C-21）")

	require.NoError(t, svc.Confirm(ctx, principal, id, "admin-1"))
	require.Equal(t, "active", store.entries[id].State, "显式确认才晋升为 active")
	require.Equal(t, "admin-1", store.confirmedBy[id], "确认必须记录 confirmed_by")

	got, err = svc.Recall(ctx, memory.RecallQuery{
		Principal: principal,
		Scope:     core.GlobalScope,
		Query:     "发布窗口",
	})
	require.NoError(t, err)
	require.Len(t, got, 1, "确认后即可召回")
	require.Equal(t, "候选事实：发布窗口在周一", got[0].Content)

	// 已不是候选：再次确认按状态冲突拒绝，不静默重放。
	err = svc.Confirm(ctx, principal, id, "admin-1")
	require.ErrorIs(t, err, core.ErrConflict)
}

// TestMemory_CorrectionSupersedesAndPreventsRecall 守设计图 5 的"用户纠正"分支：新条
// 直接 active 且 supersedes_id 指向旧条，旧条转 corrected（不物理删除），召回只返回
// 新正文；旧事实不得再被搜出。
// 逆改点：Correct 只插新条不改旧条 → 旧条仍 active → 召回返回两条 → 红。
// 验收点：§3.1.4 生命周期 / 设计图 5。
func TestMemory_CorrectionSupersedesAndPreventsRecall(t *testing.T) {
	svc, store, _ := newSemanticForTest(t)
	principal := testCallerPrincipal()

	oldID, err := svc.Propose(ctx, memory.ProposeRequest{
		Principal: principal,
		Scope:     core.GlobalScope,
		Content:   "旧事实：发布在周一",
	})
	require.NoError(t, err)
	require.NoError(t, svc.Confirm(ctx, principal, oldID, "admin-1"))

	res, err := svc.Correct(ctx, memory.CorrectRequest{
		Principal:   principal,
		TargetID:    oldID,
		Content:     "新事实：发布改到周二",
		ConfirmedBy: "admin-1",
	})
	require.NoError(t, err)
	require.Equal(t, oldID, res.ReplacedID)
	require.NotEqual(t, oldID, res.EntryID, "纠正必须产生新条目，不改写旧正文")
	require.Equal(t, "active", res.State)
	require.Equal(t, "corrected", store.entries[oldID].State, "旧条转 corrected 并保留")
	require.Equal(t, oldID, store.entries[res.EntryID].SupersedesID, "新条必须指回被替代的旧条")
	require.Equal(t, "admin-1", store.confirmedBy[res.EntryID],
		"纠正的新条必须记录确认人（必填入参不得被静默丢弃）")

	got, err := svc.Recall(ctx, memory.RecallQuery{
		Principal: principal,
		Scope:     core.GlobalScope,
		Query:     "发布",
	})
	require.NoError(t, err)
	require.Len(t, got, 1, "纠正后只剩新条可召回")
	require.Equal(t, "新事实：发布改到周二", got[0].Content)
}

// TestMemory_DeleteIsSoftAndStopsRecall 守删除语义：软删保留行与正文（审计可查）、
// 立即不可召回；重复删除幂等；不存在的 ID 明确失败而不是静默成功。
// 逆改点：物理删除（行消失）→ 旧正文断言红；删除后仍可召回 → 召回断言红。
// 验收点：§3.1.4 生命周期 / 设计图 5。
func TestMemory_DeleteIsSoftAndStopsRecall(t *testing.T) {
	svc, store, _ := newSemanticForTest(t)
	principal := testCallerPrincipal()

	id, err := svc.Propose(ctx, memory.ProposeRequest{
		Principal: principal,
		Scope:     core.GlobalScope,
		Content:   "待删除事实：临时开关已开",
	})
	require.NoError(t, err)
	require.NoError(t, svc.Confirm(ctx, principal, id, "admin-1"))

	require.NoError(t, svc.Delete(ctx, memory.DeleteRequest{Principal: principal, ID: id}))
	require.Equal(t, "deleted", store.entries[id].State, "删除是状态迁移，不是物理删除")
	require.Equal(t, "待删除事实：临时开关已开", store.entries[id].Content, "正文保留，审计可查")

	got, err := svc.Recall(ctx, memory.RecallQuery{
		Principal: principal,
		Scope:     core.GlobalScope,
		Query:     "临时开关",
	})
	require.NoError(t, err)
	require.Empty(t, got, "已删除条目立即不可召回")

	require.NoError(t, svc.Delete(ctx, memory.DeleteRequest{Principal: principal, ID: id}), "重复删除幂等")

	err = svc.Delete(ctx, memory.DeleteRequest{Principal: principal, ID: 999})
	require.Error(t, err)
	require.Contains(t, err.Error(), "entry not found")
}

// TestMemory_DuplicateProposalMergesWithoutNewRow 守设计图 5 的"同源重复转述 → 合并到
// 同条（不新增独立证据）"：同分层 / 同分区 / 同正文（规范化后）重复提议返回同一条目，
// 不新增行；提议**不得**走兼容追加路径（那是 save_memory 的写入面）。
// 逆改点：去掉去重 → 两次提议产生两条 → 行数与 ID 断言红。
// 验收点：设计图 5。
func TestMemory_DuplicateProposalMergesWithoutNewRow(t *testing.T) {
	svc, store, _ := newSemanticForTest(t)
	principal := testCallerPrincipal()

	first, err := svc.Propose(ctx, memory.ProposeRequest{
		Principal: principal,
		Scope:     core.GlobalScope,
		Content:   "同源事实：季度结算在月末",
	})
	require.NoError(t, err)
	second, err := svc.Propose(ctx, memory.ProposeRequest{
		Principal: principal,
		Scope:     core.GlobalScope,
		Content:   "  同源事实：季度结算在月末  ",
	})
	require.NoError(t, err)

	require.Equal(t, first, second, "同源重复转述合并到同一条")
	require.Len(t, store.entries, 1, "不得新增独立证据行")
	require.Zero(t, store.appends, "提议不得走兼容三方法的追加路径")
}

// TestMemory_ConflictMarkedForAdjudication 守冲突态：对**已不在有效态**的条目做纠正时
// 不替人裁决——新条落 conflicted（不进召回），旧条状态不动，等待人工裁决。
// 逆改点：无条件把新条写成 active → 冲突条目可被召回 → 红。
// 验收点：六态契约（conflicted）/ 设计图 5。
func TestMemory_ConflictMarkedForAdjudication(t *testing.T) {
	svc, store, _ := newSemanticForTest(t)
	principal := testCallerPrincipal()

	candidateID, err := svc.Propose(ctx, memory.ProposeRequest{
		Principal: principal,
		Scope:     core.GlobalScope,
		Content:   "候选事实：发布在周三",
	})
	require.NoError(t, err)

	res, err := svc.Correct(ctx, memory.CorrectRequest{
		Principal:   principal,
		TargetID:    candidateID,
		Content:     "与之矛盾的事实：发布在周四",
		ConfirmedBy: "admin-1",
	})
	require.NoError(t, err)
	require.Equal(t, "conflicted", res.State, "目标不是 active 时新条只能待裁决")
	require.Equal(t, "conflicted", store.entries[res.EntryID].State)
	require.Equal(t, "admin-1", store.confirmedBy[res.EntryID],
		"conflicted 分支同样记录提出纠正的人")
	require.Equal(t, "candidate", store.entries[candidateID].State, "旧条状态不得被顺手改写")

	got, err := svc.Recall(ctx, memory.RecallQuery{
		Principal: principal,
		Scope:     core.GlobalScope,
		Query:     "发布",
	})
	require.NoError(t, err)
	require.Empty(t, got, "候选与冲突条目都不得参与召回")
}

// TestMemory_LifecycleRequiresPrincipal 守 C-28：四个生命周期入口都以显式身份为第一道
// 检查，零值身份在触碰载体之前拒绝——不得默认成管理员，也不得先读后拒。
// 逆改点：去掉 Valid() 检查 → 零值身份可提议 / 删除 → 断言红。
// 验收点：C-28。
func TestMemory_LifecycleRequiresPrincipal(t *testing.T) {
	svc, store, _ := newSemanticForTest(t)

	_, err := svc.Propose(ctx, memory.ProposeRequest{Scope: core.GlobalScope, Content: "任意"})
	require.ErrorIs(t, err, core.ErrUnauthorized)

	err = svc.Confirm(ctx, core.Principal{}, 1, "admin-1")
	require.ErrorIs(t, err, core.ErrUnauthorized)

	_, err = svc.Correct(ctx, memory.CorrectRequest{TargetID: 1, Content: "任意", ConfirmedBy: "admin-1"})
	require.ErrorIs(t, err, core.ErrUnauthorized)

	err = svc.Delete(ctx, memory.DeleteRequest{ID: 1})
	require.ErrorIs(t, err, core.ErrUnauthorized)

	require.Zero(t, store.lifecycleCalls, "无效身份不得触碰载体")
}
