package compaction_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件覆盖压缩引擎（T083～T087）的运行时行为。R19～R23 的冻结用例按设计卡
// §4.4 逐条保真落地；其余用例的断言对应设计卡 §4.5「逆改点表」给出的失败面——
// 每条都写得出"实现怎么改它会红"。
// Engine behaviour tests. The frozen R19–R23 cases follow the design card; every other
// case pins one failure surface from its mutation table.
//
// ctx 是冻结用例直接引用的包级 context（设计卡 R19 起逐字使用 `ctx`）。
var ctx = context.Background()

// —— 触发与预算（T083 / C-31）——

// TestCompaction_BudgetSnapshotIncludesFixedParts —— 触发判定**必须**把不可裁段落算进
// projected：同一段历史，只有固定开销变了才越过比例线。
// 逆改点：projected 只累加消息 token（漏 Fixed.Total）→ 第二个用例本轮不压缩、零调用。
func TestCompaction_BudgetSnapshotIncludesFixedParts(t *testing.T) {
	compact := func(t *testing.T, fixed int) (*compactionHarness, core.CompactResult) {
		h := newCompactionHarness(t)
		h.setLimits(2000, 1, 100)
		h.budget(2000, 50) // limit 2000、safety 200、usable 1750 → 比例线 350
		h.fixed(fixed)
		h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))
		res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
		require.NoError(t, err)
		return h, res
	}

	h, res := compact(t, 200)
	require.Nil(t, res.Entry, "固定开销 200：projected≈231 未过 350，本轮不压缩")
	require.False(t, res.Degraded)
	require.Zero(t, h.provider.calls(t), "不触发就不得调用摘要")

	h, res = compact(t, 600)
	require.NotNil(t, res.Entry, "同一段历史、固定开销 600：projected≈631 过线，必须压缩")
	require.False(t, res.Degraded)
	require.Equal(t, 1, h.provider.calls(t))
}

// TestCompaction_SummaryCapDistinctFromOutputReserve —— `summary_max_tokens` 是**本次请求**
// 的上限，不参与 projected；只有 `output_reserve` 是预算链的减项（C-31 的两个数不得混用）。
// 逆改点：把 summary_max_tokens 当回复预留减进 usable → 第一、二个用例的触发结论翻面。
func TestCompaction_SummaryCapDistinctFromOutputReserve(t *testing.T) {
	compact := func(t *testing.T, summaryMax, outputReserve int) core.CompactResult {
		h := newCompactionHarness(t)
		h.setLimits(2000, 1, summaryMax)
		h.budget(2000, outputReserve)
		h.fixed(200)
		h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))
		res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
		require.NoError(t, err)
		return res
	}

	// usable = 2000−200−200 = 1600，比例线 320；projected ≈ 260 → 不触发。
	require.Nil(t, compact(t, 100, 200).Entry, "摘要上限 100：不触发")
	require.Nil(t, compact(t, 5000, 200).Entry,
		"摘要上限 5000 与 100 的触发结论必须一致——它不参与 projected")
	// 把 output_reserve 抬到 1600：usable = 200、比例线 40 < 260 → 必须触发。
	require.NotNil(t, compact(t, 100, 1600).Entry,
		"output_reserve 变化必须移动触发点（它是预算链的减项）")
}

// —— 覆盖范围与切点（C-22 / T084）——

// TestCompaction_SessionScopeCoversPriorRuns 是冻结用例（设计卡 R19）。
// 逆改点：覆盖范围按 Run 而非会话 → 覆盖起点落到 r2，上一 Run 的目标原话未被覆盖。
func TestCompaction_SessionScopeCoversPriorRuns(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100) // 预算 / 保留组 / 摘要上限由用例显式给定
	h.seedRun(t, "s1", "r1", u("第一轮目标：整理 Q3 报表"), a("收到"))
	h.seedRun(t, "s1", "r2", u("继续"), a("好"), u("再继续"), a("好"))

	res, err := h.compactor.Compact(ctx, h.request("s1", "r2"))
	require.NoError(t, err)
	require.False(t, res.Degraded)
	require.NotNil(t, res.Entry)
	require.Equal(t, "r1", h.runOfMessage(t, res.Entry.CoveredFromMessageID),
		"覆盖起点必须落在上一 Run 的消息上——取数按会话，不按 Run")
	require.Equal(t, "r2", res.Entry.TriggerRunID, "run_id 只记录触发者，不是覆盖边界")
	require.True(t, h.summaryInputContains(t, "第一轮目标：整理 Q3 报表"), "上一 Run 的目标原话必须进入摘要输入")
}

// TestCompaction_CutAtToolGroupBoundary 是冻结用例（设计卡 R19）。
// 逆改点：切点不按完整工具组 → 覆盖段出现"只有 tool_calls 没有结果"的半组。
func TestCompaction_CutAtToolGroupBoundary(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(350, 1, 100)
	h.seedGroups(t, "s1", "r1",
		group("g1", "查 A 文档", "A 的内容"),
		group("g2", "查 B 文档", "B 的内容"),
		group("g3", "查 C 文档", "C 的内容"),
	)

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.False(t, res.Degraded)
	require.NotNil(t, res.Entry)

	covered := h.messagesBetween(t, res.Entry.CoveredFromMessageID, res.Entry.CutMessageID)
	require.NoError(t, assertWholeGroups(covered),
		"切点只落在完整工具组边界：assistant(tool_calls) 与全部 tool 结果同进同出")
	require.Equal(t, "g3", h.groupOfMessage(t, res.Entry.FirstKeptMessageID), "最近一组必须保留")
}

// TestCompaction_NotStoredAsMessage 是冻结用例（设计卡 R19）。
// 逆改点：摘要写回 messages → messageCount +1，历史检索能命中摘要文本。
func TestCompaction_NotStoredAsMessage(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("目标：整理 Q3 报表"), a("收到"), u("继续"), a("好"))
	before := h.messageCount(t)

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.Equal(t, before, h.messageCount(t),
		"摘要不得写回消息仓储（原则 II：messages 只由 AgentService / ReActLoop / 恢复路径追加）")
	require.Equal(t, 1, h.store.count(t), "摘要只落在 compaction_entries")
	require.Equal(t, 0, h.store.committedMessagesWith(t, res.Entry.Text),
		"摘要文本不得出现在任何消息行里（否则会被检索命中并当作原文）")
}

// TestCompaction_OrderedBySessionSeq —— 覆盖顺序只由读取端口保证的 `(session_id, seq)` 决定；
// 引擎**不得**按标识排序（标识是随机 / 补写形态，与顺序无关）。
// 逆改点：引擎按 MessageID 排序 → 覆盖起点变成 "m2"（标识最小），而不是 seq 最早的那条。
func TestCompaction_OrderedBySessionSeq(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("一"), a("二"), u("三"), a("四"))
	h.renameMessage(t, "m1", "zz-最早")
	h.renameMessage(t, "m4", "aa-最晚")

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.Equal(t, "zz-最早", res.Entry.CoveredFromMessageID,
		"覆盖起点必须是 seq 最小的一条，而不是标识最小的一条")
	require.Equal(t, "m3", res.Entry.CutMessageID)
	require.Equal(t, "aa-最晚", res.Entry.FirstKeptMessageID)
}

// —— 无状态范围（C-22 后半段）——

// TestCompaction_StatelessUsesRunHistoryPort 是冻结用例（设计卡 R20）。
// 逆改点：无状态范围改回按 Run 查库 → reader.calls > 0 且覆盖起点为空。
func TestCompaction_StatelessUsesRunHistoryPort(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.history(u("目标：整理 Q3 报表"), a("收到"), u("继续"), a("好")) // 无状态：消息只在内存
	h.reader.failIfCalled(t, "无状态 Run 的消息从不落 messages，按 Run 反查只能得到空集")

	res, err := h.compactor.Compact(ctx, h.request("", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.Equal(t, compaction.StatelessMessageID("r1", 1), res.Entry.CoveredFromMessageID,
		"无状态范围的覆盖起点用临时标识 msg/<run_id>/<pos>（只在本 Run 内自洽）")
	require.Equal(t, "r1", res.Entry.TriggerRunID, "run_id 只是触发者")
	require.Zero(t, h.reader.calls(t), "无状态范围不得读 PG——这条路径一次库读都不该发生")
}

// TestCompaction_StatelessRunScopeIsRunOnly —— 无状态条目的 scope 是 `run:<id>`，
// 既看不见同会话的链，也不会被同会话的链看见。
// 逆改点：无状态范围也带 SessionID → 条目落进会话 scope，下一次同会话压缩把它当增量。
func TestCompaction_StatelessRunScopeIsRunOnly(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.store.seedChain(t, compaction.CompactionEntry{
		CompactionID: "c-session", ChainEpoch: 1,
		CoveredFromMessageID: "m1", CutMessageID: "m2", FirstKeptMessageID: "m3",
	})
	h.seedRun(t, "s1", "r0", u("会话目标"), a("收到"), u("继续"), a("好"))
	h.history(u("无状态目标"), a("收到"), u("继续"), a("好"))
	h.reader.failIfCalled(t, "无状态 Run 只经 History 读消息")

	res, err := h.compactor.Compact(ctx, h.request("", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.Equal(t, compaction.Scope{RunID: "r1"}, h.store.appended[0].Scope,
		"无状态范围的 scope 是 run:<id>，不带 session_id")
	require.Empty(t, h.store.appended[0].ParentCompactionID, "不得接在同会话的链上")
	require.Equal(t, compaction.StatelessMessageID("r1", 1), res.Entry.CoveredFromMessageID)
}

// TestCompaction_SessionEntryScopeIsSessionOnly —— 会话范围条目的链作用域只含
// `session_id`；`run_id` 只进 `trigger_run_id`（触发记录与审计关联），不是链身份。
// 逆改点：链作用域同时带 SessionID 与 RunID → PG 写入被 `num_nonnulls = 1` 拒绝，
// 摘要一旦成功生成即整次运行失败（2026-10-31 真实实例实测复现）。
func TestCompaction_SessionEntryScopeIsSessionOnly(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))

	res, err := h.compactor.Compact(ctx, h.request("s1", "r2"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.Len(t, h.store.appended, 1)
	require.Equal(t, compaction.Scope{SessionID: "s1"}, h.store.appended[0].Scope,
		"会话条目的链作用域只含 session_id；run_id 只作 TriggerRunID 记录")
	require.Equal(t, "r2", h.store.appended[0].TriggerRunID,
		"run_id 仍是触发记录的一部分，只是不参与链身份")
}

// TestCompaction_StatelessEntriesAreAuditOnly —— 上一个无状态 Run 的条目只作审计，
// 下一个 Run 不得复用（临时标识按 run_id 生成，跨 Run 无从对齐）。
// 逆改点：无状态条目被当作可复用链 → 第二个 Run 的父节点非空、覆盖起点等于上一 Run 的标识。
func TestCompaction_StatelessEntriesAreAuditOnly(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.history(u("第一轮目标"), a("收到"), u("继续"), a("好"))

	first, err := h.compactor.Compact(ctx, h.request("", "r1"))
	require.NoError(t, err)
	require.NotNil(t, first.Entry)

	h.restart() // Run 结束后进程内状态也不保留
	h.history(u("第二轮目标"), a("收到"), u("继续"), a("好"))
	second, err := h.compactor.Compact(ctx, h.request("", "r2"))
	require.NoError(t, err)
	require.NotNil(t, second.Entry)
	require.NotEqual(t, first.Entry.CompactionID, second.Entry.CompactionID)
	require.Empty(t, h.store.appended[1].ParentCompactionID, "无状态条目不参与跨 Run 链")
	require.Equal(t, compaction.StatelessMessageID("r2", 1), second.Entry.CoveredFromMessageID,
		"覆盖起点用本 Run 自己的临时标识")
	require.Equal(t, 2, h.provider.calls(t), "两个 Run 各自生成一份摘要")
}

// —— 滚动链（C-22 滚动增量）——

// TestCompaction_CrossRunReusesChain —— 同一会话的第二个 Run 从上一份的保留起点接着压，
// 不从头重来。
// 逆改点：每轮都从 m1 重新摘要 → 覆盖起点回到 m1、父节点为空、摘要范围重叠。
func TestCompaction_CrossRunReusesChain(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("第一轮目标"), a("收到"), u("继续"), a("好"))

	first, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, first.Entry)

	h.seedRun(t, "s1", "r2", u("第二轮新增"), a("收到"), u("继续"), a("好"))
	second, err := h.compactor.Compact(ctx, h.request("s1", "r2"))
	require.NoError(t, err)
	require.NotNil(t, second.Entry)
	require.Equal(t, first.Entry.FirstKeptMessageID, second.Entry.CoveredFromMessageID,
		"新条目必须精确接在上一份的保留起点")
	require.Equal(t, first.Entry.CompactionID, h.store.appended[1].ParentCompactionID)
	require.Equal(t, 1, second.Entry.ChainEpoch, "滚动增量留在同一代，不换代")
	require.Equal(t, 2, h.provider.calls(t), "只对新增的覆盖段生成一次摘要")
}

// TestCompaction_CoverageMonotonic —— 连续多轮压缩的覆盖区间首尾相接、起点单调前移：
// 既不重叠也不跳段。
// 逆改点：覆盖起点取窗口首条（而不是头的 first_kept）→ 第一段被重复摘要，起点不单调。
func TestCompaction_CoverageMonotonic(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	for i := 1; i <= 3; i++ {
		h.seedRun(t, "s1", fmt.Sprintf("r%d", i), u("目标"), a("收到"), u("继续"), a("好"))
		res, err := h.compactor.Compact(ctx, h.request("s1", fmt.Sprintf("r%d", i)))
		require.NoError(t, err)
		require.NotNil(t, res.Entry, "第 %d 轮必须触发压缩", i)
	}

	require.Len(t, h.store.appended, 3)
	for i := 1; i < len(h.store.appended); i++ {
		prev, cur := h.store.appended[i-1], h.store.appended[i]
		require.Equal(t, prev.FirstKeptMessageID, cur.CoveredFromMessageID,
			"新条目必须精确接在上一份的保留起点：既不重叠也不跳段")
		require.Greater(t, h.indexOf(t, cur.CoveredFromMessageID), h.indexOf(t, prev.CoveredFromMessageID),
			"覆盖起点单调前移")
	}
}

// TestCompaction_ConsecutiveRetainsSourceRefs —— 两跳之后审计集与依赖集都保留整条链上的来源
// （父链集合 ∪ 本段消息）。
// 逆改点：只用本段消息算集合（漏 parentAudit / parentIncluded）→ 第二跳的两个集合都少了 kb-a。
func TestCompaction_ConsecutiveRetainsSourceRefs(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", withSourceRefs([]compaction.StoredMessage{
		u("第一轮目标"), a("收到"), u("继续"), a("好"),
	}, ref("kb-a", "v1"))...)

	first, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, first.Entry)

	h.seedRun(t, "s1", "r2", withSourceRefs([]compaction.StoredMessage{
		u("第二轮"), a("收到"), u("继续"), a("好"),
	}, ref("kb-b", "v1"))...)
	second, err := h.compactor.Compact(ctx, h.request("s1", "r2"))
	require.NoError(t, err)
	require.NotNil(t, second.Entry)

	want := []core.EvidenceRef{ref("kb-a", "v1"), ref("kb-b", "v1")}
	require.ElementsMatch(t, want, second.Entry.SourceRefs, "审计集必须累计父链与本段")
	require.ElementsMatch(t, want, second.Entry.IncludedRefs, "依赖集同样累计（两处都有效）")
	require.Equal(t, first.Entry.CompactionID, h.store.appended[1].ParentCompactionID)
}

// —— 引用集合由代码算出（C-26 / C-36）——

// TestCompaction_SourceRefsComputedByCode 是冻结用例（设计卡 R20）。
// 逆改点：引用集合改成取自模型输出 → 只剩输出里写的那一条（这里一条都不写，集合为空）。
func TestCompaction_SourceRefsComputedByCode(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedGroups(t, "s1", "r1",
		withSourceRefs(group("g1", "查 A 文档", "A 的内容"), ref("kb-a", "v1")),
		withSourceRefs(group("g2", "查 B 文档", "B 的内容"), ref("kb-b", "v1")),
		group("g3", "查 C 文档", "C 的内容"), // 无来源的消息不贡献引用
	)
	h.store.seedParentEntry(compaction.CompactionEntry{
		CompactionID: "c-parent",
		SourceRefs:   []core.EvidenceRef{ref("kb-old", "v1")}, // 父链上更早的来源
	})
	h.provider.respond("摘要正文") // 模型输出**不含** source_refs 字段

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.ElementsMatch(t, []core.EvidenceRef{
		ref("kb-a", "v1"), ref("kb-b", "v1"), ref("kb-old", "v1"),
	}, res.Entry.SourceRefs,
		"引用集合 = 覆盖消息的来源元数据 ∪ 父链，由代码算出；模型漏写引用不得使集合变窄")
}

// TestCompaction_RevokedSourceNotReinjected —— 撤权来源不得经"上一份摘要"洗进新正文：
// 父摘要不可注入时 `Previous` 必须为空，审计集留痕、依赖集剔除。
// 逆改点：无头推导也用旧摘要正文当增量 → 标记串出现在摘要输入里、依赖集非空。
func TestCompaction_RevokedSourceNotReinjected(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	const marker = "PARENT-SUMMARY-MARKER"
	h.seedGroups(t, "s1", "r1",
		withSourceRefs(group("g1", "查 A 文档", "A 的内容"), ref("kb-b", "v1")),
		group("g2", "查 B 文档", "B 的内容"),
		group("g3", "查 C 文档", "C 的内容"))
	h.store.seedChain(t, compaction.CompactionEntry{
		CompactionID: "c-a", ChainEpoch: 1,
		CoveredFromMessageID: "m1", CutMessageID: "m2", FirstKeptMessageID: "m3",
		Summary:      marker,
		SourceRefs:   []core.EvidenceRef{ref("kb-b", "v1")},
		IncludedRefs: []core.EvidenceRef{ref("kb-b", "v1")},
	})
	h.validity.revoke(ref("kb-b", "v1"))

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.NotEqual(t, "c-a", res.Entry.CompactionID, "失效的旧摘要不得再被当作增量")
	require.Equal(t, 1, h.provider.calls(t))
	require.True(t, h.summaryInputExcludes(t, marker),
		"父摘要正文不得进入新摘要输入（否则已撤权内容被洗白）")
	require.Empty(t, res.Entry.IncludedRefs, "撤权来源不得成为新条目的当前依赖")
	require.ElementsMatch(t, []core.EvidenceRef{ref("kb-b", "v1")}, res.Entry.SourceRefs,
		"审计集仍须记下这条摘要涉过已撤权来源")
}

// TestCompaction_StaleChainNotInjected —— 根失效 ⇒ 整条链不可注入，一个摘要段都不产出；
// 级联标 stale 覆盖全部后代。
// 逆改点：只校验链尾（或跳过坏祖先取后代）→ 仍返回 c-b 的摘要、MarkStale 只标一条。
func TestCompaction_StaleChainNotInjected(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(100000, 1, 100) // 预算充足：只验注入资格
	h.seedGroups(t, "s1", "r1", group("g1", "查 A 文档", "A 的内容"), group("g2", "查 B 文档", "B 的内容"))
	h.store.seedChain(t,
		compaction.CompactionEntry{
			CompactionID: "c-a", ChainEpoch: 1,
			CoveredFromMessageID: "m1", CutMessageID: "m2", FirstKeptMessageID: "m3",
			SourceRefs:   []core.EvidenceRef{ref("kb-a", "v1")},
			IncludedRefs: []core.EvidenceRef{ref("kb-a", "v1")},
		},
		compaction.CompactionEntry{
			CompactionID: "c-b", ChainEpoch: 1, ParentCompactionID: "c-a",
			CoveredFromMessageID: "m3", CutMessageID: "m4", FirstKeptMessageID: "m5",
			SourceRefs:   []core.EvidenceRef{ref("kb-a", "v1"), ref("kb-b", "v1")},
			IncludedRefs: []core.EvidenceRef{ref("kb-a", "v1"), ref("kb-b", "v1")},
		})
	h.validity.revoke(ref("kb-a", "v1")) // 根失效 ⇒ 整链不可注入

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.Nil(t, res.Entry, "整链失效时不得注入任何摘要段")
	require.False(t, res.Degraded, "不注入不是压缩失败——本轮只是没有可用摘要")
	require.Equal(t, 2, h.store.staleCount(t), "级联：A 与 B 都应为 stale")
	require.Zero(t, h.provider.calls(t))
}

// —— 摘要输入与校验（C-23 / C-25）——

// TestCompaction_ConstraintsPreservedVerbatim —— 硬约束段代码保真：用户原话进摘要输入不被改写，
// 上一份摘要的两节由代码原样搬进新摘要（模型无权删除）。
// 逆改点：不调 CarryCodeBlocks → 新摘要缺「硬约束与否决条件」「阻塞 / 未决」两节。
func TestCompaction_ConstraintsPreservedVerbatim(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	const parent = "## 硬约束与否决条件\n- 原话：不要删除 X（m1）\n## 阻塞 / 未决\n- 备份恢复未验证"
	h.store.seedChain(t, compaction.CompactionEntry{
		CompactionID: "c-a", ChainEpoch: 1,
		CoveredFromMessageID: "m1", CutMessageID: "m2", FirstKeptMessageID: "m3",
		Summary: parent,
	})
	h.seedRun(t, "s1", "r1",
		u("硬约束：不要删除 X"), a("明白"), u("继续 A"), a("好 A"), u("继续 B"), a("好 B"))
	h.provider.respond("## 目标\n新摘要正文") // 模型不再写这两节

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.True(t, h.summaryInputContains(t, "不要删除 X（m1）"),
		"上一份摘要必须逐字进入本次摘要输入——约束不能被模型的转述磨掉")
	require.Contains(t, res.Entry.Text, "## 硬约束与否决条件")
	require.Contains(t, res.Entry.Text, "- 原话：不要删除 X（m1）", "硬约束段由代码原样追加")
	require.Contains(t, res.Entry.Text, "## 阻塞 / 未决")
	require.Contains(t, res.Entry.Text, "备份恢复未验证", "未决项不得被模型删掉")
	require.Contains(t, res.Entry.Text, "新摘要正文")
}

// TestCompaction_DoesNotMarkPendingDone —— "已完成"条目无证据 ID ⇒ 整条摘要不合规，
// 不落库、不返回，按可分类降级。
// 逆改点：跳过 ValidateSummaryText → 未验证的"已完成"被写进链。
func TestCompaction_DoesNotMarkPendingDone(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))
	h.provider.respond("## 已完成\n- 备份恢复未验证（已完成）\n## 下一步\n1. 继续")

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err, "摘要不合规是**可分类降级**，不是不可判定错误")
	require.True(t, res.Degraded)
	require.Equal(t, compaction.ReasonCompactionFailed, res.ReasonCode)
	require.Nil(t, res.Entry, "未验证的完成结论不得成为条目")
	require.Zero(t, h.store.count(t), "不得落库")
}

// TestCompaction_StateBlockWinsOverSummary —— 结构化状态是权威块：引擎不自造状态（`StateBlock`
// 由 C 的接线层填），投影里状态段排在摘要段之前，冲突时以状态为准。
// 逆改点：段序把 summary 排在 state 之前（或空状态也产出空段）→ 冲突时模型先读摘要的旧结论。
func TestCompaction_StateBlockWinsOverSummary(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	for _, in := range h.provider.recorded() {
		require.Empty(t, in.StateBlock, "引擎不得自造结构化状态")
	}

	chain := []compaction.CompactionEntry{{
		CompactionID: "c-1", ChainEpoch: 1,
		Status: compaction.StatusCommitted, Summary: "摘要声称：备份未做",
	}}
	segments := compaction.Projection{}.Assemble("系统提示", "记忆", "状态：备份恢复未验证", chain, h.msgs[:1])
	require.Equal(t, []string{
		compaction.SegSystem, compaction.SegMemory, compaction.SegState,
		compaction.SegSummary, compaction.SegRecent,
	}, segmentIDs(segments),
		"结构化状态段必须存在且排在摘要之前：冲突时以状态为准")
	require.Equal(t, "状态：备份恢复未验证", segments[2].Content)
	require.Equal(t, "摘要声称：备份未做", segments[3].Content)

	blank := compaction.Projection{}.Assemble("系统提示", "", "   ", chain, nil)
	require.Equal(t, []string{compaction.SegSystem, compaction.SegSummary}, segmentIDs(blank),
		"空白状态不产出空段（记录只写真发生的事）")
}

// TestCompaction_EntryRecordsCoverageAndUsage —— 条目字段与 `compaction_entries` 一一对应：
// 覆盖区间、切点、策略与提示版本、模型与用量、时间全部留痕。
// 逆改点：漏写 usage / 版本字段 → 报告与对账无从回溯这次调用的成本与策略。
func TestCompaction_EntryRecordsCoverageAndUsage(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)

	require.Len(t, h.store.appended, 1)
	e := h.store.appended[0]
	require.NotEmpty(t, e.CompactionID)
	require.True(t, e.Valid())
	require.Equal(t, "session:s1", e.Scope.Canonical(),
		"会话范围的条目身份是 session:<id>（条目里即便带 RunID 也不参与身份）")
	require.Equal(t, "s1", e.Scope.SessionID)
	require.Equal(t, 1, e.ChainEpoch)
	require.Equal(t, "r1", e.TriggerRunID)
	require.Equal(t, "a1", e.AttemptID)
	require.Equal(t, "m1", e.CoveredFromMessageID)
	require.Equal(t, "m3", e.CutMessageID)
	require.Equal(t, "m4", e.FirstKeptMessageID)
	require.Equal(t, "cut-v1", e.StrategyVersion)
	require.Equal(t, "summary-v1", e.PromptVersion)
	require.Equal(t, "summary-model", e.Model)
	require.Equal(t, core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, e.Usage)
	require.Positive(t, e.TokensBefore)
	require.Positive(t, e.TokensAfter)
	require.Equal(t, compaction.StatusCommitted, e.Status)
	require.NotEmpty(t, e.Summary)
	require.Equal(t, time.Unix(1760000000, 0).UTC(), e.CreatedAt)
}

// —— 只追加（C-22）——

// TestCompaction_AppendOnly —— 旧条目不得原地改写：重建生成**新**条目并保留旧条目。
// 逆改点：重建时覆盖旧行（同一 CompactionID 改正文）→ 审计无法还原当时注入了什么。
func TestCompaction_AppendOnly(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedGroups(t, "s1", "r1",
		withSourceRefs(group("g1", "查 A 文档", "A 的内容"), ref("kb-a", "v1")),
		group("g2", "查 B 文档", "B 的内容"),
		group("g3", "查 C 文档", "C 的内容"))
	h.store.seedChain(t, compaction.CompactionEntry{
		CompactionID: "c-old", ChainEpoch: 1,
		CoveredFromMessageID: "m1", CutMessageID: "m2", FirstKeptMessageID: "m3",
		Summary:      "旧摘要正文",
		SourceRefs:   []core.EvidenceRef{ref("kb-a", "v1")},
		IncludedRefs: []core.EvidenceRef{ref("kb-a", "v1")},
	})
	h.validity.revoke(ref("kb-a", "v1")) // 旧链不可注入 ⇒ 走换代重建

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.NotEqual(t, "c-old", res.Entry.CompactionID, "重建必须**追加**新条目")

	old, ok := h.store.entryByID(t, "c-old")
	require.True(t, ok, "旧条目必须仍在存储里")
	require.Equal(t, "旧摘要正文", old.Summary, "旧条目正文不得被覆盖")
	require.Equal(t, []core.EvidenceRef{ref("kb-a", "v1")}, old.SourceRefs, "旧条目引用集不得被改写")
	require.Equal(t, 1, h.store.count(t), "只会追加一条新条目")
	require.Equal(t, 2, h.store.appended[0].ChainEpoch, "重建以 chain_epoch+1 开新代")
	require.Empty(t, h.store.appended[0].ParentCompactionID, "新根没有父节点")
}

// —— 切点与超限（T084 / C-24）——

// TestCompaction_SingleGroupOverLimitRejected —— 只剩一个完整工具组且它本身超限：明确拒绝，
// 不拆组、不静默截断、不调用摘要。
// 逆改点：找不到切点时改成"压缩全部"或截断单组 → 出现半组历史或丢失工具结果。
func TestCompaction_SingleGroupOverLimitRejected(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	// 只剩 assistant(tool_calls) + 它的结果：**一个**完整工具组（组由 `AssignToolGroups` 现分）。
	h.seedRun(t, "s1", "r1", asstCall("call-1", "source_view"), toolResult("call-1", strings.Repeat("长", 400)))

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.True(t, res.Degraded)
	require.Equal(t, compaction.ReasonCompactionOverLimit, res.ReasonCode,
		"单个工具组本身超限：走明确拒绝，不得拆组或静默截断")
	require.Nil(t, res.Entry)
	require.Zero(t, h.provider.calls(t), "没有可压缩的完整组，不得调用摘要")
	require.Zero(t, h.store.count(t))
}

// —— 取消、失败与降级（T087 / C-25）——

// TestCompaction_FailureFallsBackToTrim —— 摘要失败与超时都降级为分类原因码，不落库、不返回半成品。
// 逆改点：把失败当空串继续 → 条目正文为空、覆盖段被"摘要"掉却没有任何内容。
func TestCompaction_FailureFallsBackToTrim(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"供应商失败", errors.New("upstream 500"), compaction.ReasonCompactionFailed},
		{"摘要超时", fmt.Errorf("wrapped: %w", compaction.ErrSummaryTimeout), compaction.ReasonCompactionTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCompactionHarness(t)
			h.setLimits(300, 1, 100)
			h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))
			h.provider.failWith(tc.err)

			res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
			require.NoError(t, err)
			require.True(t, res.Degraded)
			require.Equal(t, tc.want, res.ReasonCode)
			require.Nil(t, res.Entry, "失败不得注入半成品摘要")
			require.Zero(t, h.store.count(t), "失败不得落库")
		})
	}
}

// TestCompaction_SummaryOverLimitNotTruncated —— 摘要输出超 `summary_max_tokens`：重新生成一次，
// 仍超限即失败；**不得**截断后使用（截断先丢硬约束 / 未决项）。
// 逆改点：超限就截断 → 第二次调用不发生、条目被写入。
func TestCompaction_SummaryOverLimitNotTruncated(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 20) // 摘要上限 20 token
	h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))
	h.provider.respond(strings.Repeat("长", 200))

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.True(t, res.Degraded)
	require.Equal(t, compaction.ReasonCompactionOverLimit, res.ReasonCode)
	require.Nil(t, res.Entry, "超限摘要不得被截断后使用")
	require.Zero(t, h.store.count(t))
	require.Equal(t, 2, h.provider.calls(t), "超限只重生成一次，两次仍超限即失败")
}

// TestCompaction_CancelKeepsHistory —— 取消按 ctx 错误上抛（不降级成压缩失败），
// 历史原样保留，不写条目。
// 逆改点：把取消吞成 compaction_failed → 调用方以为只是"压缩失败"而继续跑。
func TestCompaction_CancelKeepsHistory(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))
	before := h.messageCount(t)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.provider.onEachCall(cancel)
	h.provider.failWith(context.Canceled)

	res, err := h.compactor.Compact(runCtx, h.request("s1", "r1"))
	require.ErrorIs(t, err, context.Canceled, "取消必须传播给调用方")
	require.Nil(t, res.Entry)
	require.Equal(t, before, h.messageCount(t), "历史保持原样：压缩不得吃掉消息")
	require.Zero(t, h.store.count(t), "取消后不得写条目")
	require.Equal(t, 1, h.provider.calls(t))
}

// —— 六态追加与恢复（C-25）——

// TestCompaction_RestartUsesCommittedOnly —— 重启后只认 `status=committed` 的条目：
// failed / stale 一律不作投影。
// 逆改点：把"最新一行"当有效头（不筛状态）→ failed 条目的半成品摘要被注入。
func TestCompaction_RestartUsesCommittedOnly(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))
	h.store.seedChain(t,
		compaction.CompactionEntry{
			CompactionID: "c-failed", ChainEpoch: 1, Status: compaction.StatusFailed,
			CoveredFromMessageID: "m1", CutMessageID: "m1", FirstKeptMessageID: "m2", Summary: "半成品摘要",
		},
		compaction.CompactionEntry{
			CompactionID: "c-stale", ChainEpoch: 1, Status: compaction.StatusStale,
			CoveredFromMessageID: "m1", CutMessageID: "m2", FirstKeptMessageID: "m3", Summary: "失效摘要",
		})
	h.budget(1000000, 50)

	h.restart()
	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.Nil(t, res.Entry, "failed / stale 条目都不得作为投影（重启后只认 committed）")
	require.False(t, res.Degraded)
	require.Zero(t, h.provider.calls(t))
}

// TestCompaction_UnknownCommitVerifiesBeforeRetry 是冻结用例（设计卡 R19）。
// 逆改点：未知提交直接重发 → 出现两条 committed 条目与第二次摘要调用。
func TestCompaction_UnknownCommitVerifiesBeforeRetry(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))
	h.store.appendOutcome(compaction.AppendPending) // 条目提交结局未知（VerifyTx=Pending）

	_, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.ErrorIs(t, err, compaction.ErrCompactionPending, "结局不可判定时冻结前滚，不猜、不重提")
	require.Equal(t, 1, h.provider.calls(t), "冻结前不得再发一次摘要调用")
	require.Zero(t, h.store.count(t), "未确认提交前不作有效投影")

	h.store.appendOutcome(compaction.AppendCommitted)
	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.Equal(t, 1, h.store.count(t), "AppendCommitted 复用条目，不得追加第二条")
	require.Equal(t, 1, h.provider.calls(t), "已提交时不得重发摘要调用")
}

// TestCompaction_CrashRecoveryReusesCommittedCoverage —— 崩溃重启后已提交条目仍是有效头：
// 直接复用它作投影，不重新生成摘要、不追加第二行。
// 逆改点：崩溃后不看既有 committed 条目 → 第二轮既拿不到投影（Entry 为 nil），
// 又可能再次生成摘要（provider.calls 变 2）。
func TestCompaction_CrashRecoveryReusesCommittedCoverage(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedRun(t, "s1", "r1", u("目标"), a("收到"), u("继续"), a("好"))

	first, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, first.Entry)
	require.Equal(t, 1, h.provider.calls(t))

	h.restart()          // 崩溃：内存里的正文与结论都丢了，只剩已提交条目
	h.budget(100000, 50) // 重启后预算充足：本轮**不**触发新压缩，只验既有条目被复用
	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.False(t, res.Degraded, "既有头仍可注入——'没有增量可压'不是压缩失败")
	require.NotNil(t, res.Entry, "重启后必须用已提交条目作投影（忽略它就只能得到空投影）")
	require.Equal(t, first.Entry.CompactionID, res.Entry.CompactionID, "已提交条目直接复用")
	require.Equal(t, core.SummaryValid, res.Entry.Validity)
	require.Equal(t, 1, h.provider.calls(t), "恢复路径零摘要调用")
	require.Equal(t, 1, h.store.count(t), "不得追加第二条")
}

// TestCompaction_RebasesWhenHeadAdvanced 是冻结用例（设计卡 R21）。
// 逆改点：忽略 AppendSuperseded 直接重提旧摘要 → provider.calls 停在 1，覆盖起点仍是旧头。
func TestCompaction_RebasesWhenHeadAdvanced(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedGroups(t, "s1", "r1",
		group("g1", "查 A", "A"), group("g2", "查 B", "B"), group("g3", "查 C", "C"))
	// 追加时发现头已被另一次追加推进：m5 起才是当前保留尾部
	h.store.appendSupersededOnce(compaction.CompactionEntry{
		CompactionID: "c-new-head", FirstKeptMessageID: "m5", CutMessageID: "m4",
	})
	h.provider.respond("第一版摘要", "第二版摘要")

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.Equal(t, 2, h.provider.calls(t), "被超越后必须按新头重新生成，不得重提旧摘要")
	require.Equal(t, "m5", res.Entry.CoveredFromMessageID, "覆盖起点必须等于当前头的 first_kept")
	require.Equal(t, "c-new-head", h.store.appended[0].ParentCompactionID,
		"父节点必须指向锁内读到的头（冻结断言写作 res.Entry.ParentCompactionID；"+
			"core.CompactionSummary 的字段表无该字段，断言逻辑不变，改读落库条目）")
	require.NotEqual(t, "第一版摘要", res.Entry.Text, "旧摘要覆盖的是已失效范围，不得写进新条目")
	require.Equal(t, 1, h.store.count(t), "被超越的那次不写行；只有重算后的条目（attempt 身份不复用）")
}

// —— 链资格与失效（C-36）——

// TestCompaction_AncestorStaleRebuildsNewRoot 是冻结用例（设计卡 R22）。
// 逆改点：把"最新 committed 行"当有效头（不查链资格）→ 拿不到新根，ChainEpoch 停在 1。
func TestCompaction_AncestorStaleRebuildsNewRoot(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedGroups(t, "s1", "r1",
		withSourceRefs(group("g1", "查 A 文档", "A 的内容"), ref("kb-a", "v1")),
		group("g2", "查 B 文档", "B 的内容"), group("g3", "查 C 文档", "C 的内容"))
	// 已提交两级链：A（覆盖 m1、m2）→ B（覆盖 m3、m4）；A 的来源现已撤权
	h.store.seedChain(t,
		compaction.CompactionEntry{
			CompactionID: "c-a", ChainEpoch: 1,
			CoveredFromMessageID: "m1", CutMessageID: "m2", FirstKeptMessageID: "m3",
			SourceRefs:   []core.EvidenceRef{ref("kb-a", "v1")},
			IncludedRefs: []core.EvidenceRef{ref("kb-a", "v1")},
		},
		compaction.CompactionEntry{
			CompactionID: "c-b", ChainEpoch: 1, ParentCompactionID: "c-a",
			CoveredFromMessageID: "m3", CutMessageID: "m4", FirstKeptMessageID: "m5",
			SourceRefs:   []core.EvidenceRef{ref("kb-a", "v1")},
			IncludedRefs: []core.EvidenceRef{ref("kb-a", "v1")},
		})
	h.validity.revoke(ref("kb-a", "v1")) // A 的 included 失效 ⇒ 整链不可注入（后代不得绕过坏祖先）
	h.provider.respond("重建后的摘要")

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.Equal(t, 2, res.Entry.ChainEpoch, "无有效链必须换代重建，而不是沿失效链继续")
	require.Empty(t, h.store.appended[0].ParentCompactionID, "重建根没有父节点")
	require.Equal(t, "m1", res.Entry.CoveredFromMessageID, "从覆盖范围起点重新摘要")
	require.Equal(t, 1, h.provider.calls(t), "重建只生成一次摘要，不得重提旧摘要")
	require.ElementsMatch(t, []core.EvidenceRef{ref("kb-a", "v1")}, res.Entry.SourceRefs,
		"审计集保留来源（含已撤权的），用于追溯这条摘要曾涉及什么")
	require.Empty(t, res.Entry.IncludedRefs,
		"撤权内容已剔除，当前依赖集必须为空——否则新摘要永远注入不进去")
	require.Equal(t, 2, h.store.staleCount(t), "MarkStale 级联：A 与 B 都应为 stale")
	require.Equal(t, core.SummaryValid, res.Entry.Validity, "重建出的条目本身必须可注入")
}

// TestCompaction_MiddleEntryStaleFallsBackToHead 是冻结用例（设计卡 R22）。
// 逆改点：失效一律从叶往根整链作废 → 回不到 c-a、凭空多一次重建与一次摘要调用。
func TestCompaction_MiddleEntryStaleFallsBackToHead(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(100000, 1, 100) // 不触发新压缩，只验"链中段失效 → 回到上一份可注入摘要"
	h.seedGroups(t, "s1", "r1", group("g1", "查 A 文档", "A 的内容"), group("g2", "查 B 文档", "B 的内容"))
	// A（只依赖 kb-a）→ B（新增依赖 kb-b）；B 的来源已撤权，A 仍可注入
	h.store.seedChain(t,
		compaction.CompactionEntry{
			CompactionID: "c-a", ChainEpoch: 1,
			CoveredFromMessageID: "m1", CutMessageID: "m2", FirstKeptMessageID: "m3",
			SourceRefs:   []core.EvidenceRef{ref("kb-a", "v1")},
			IncludedRefs: []core.EvidenceRef{ref("kb-a", "v1")},
		},
		compaction.CompactionEntry{
			CompactionID: "c-b", ChainEpoch: 1, ParentCompactionID: "c-a",
			CoveredFromMessageID: "m3", CutMessageID: "m4", FirstKeptMessageID: "m5",
			SourceRefs:   []core.EvidenceRef{ref("kb-a", "v1"), ref("kb-b", "v1")},
			IncludedRefs: []core.EvidenceRef{ref("kb-a", "v1"), ref("kb-b", "v1")},
		})
	h.validity.revoke(ref("kb-b", "v1"))

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.Equal(t, "c-a", res.Entry.CompactionID, "中段失效 → 回到上一份可注入摘要，不必整链重建")
	require.Equal(t, core.SummaryValid, res.Entry.Validity)
	require.Equal(t, 1, h.store.staleCalls(t), "只标最早失效的那一条（级联其后代），不动祖先")
	require.Equal(t, 1, h.store.staleCount(t), "B 被标 stale，A 保持 committed（不是把整链一起废掉）")
	require.Equal(t, 0, h.provider.calls(t), "回到 A 即可，零摘要调用")
}

// TestCompaction_NoRepeatedRegenerationWhenChainNotInjectable 是冻结用例（设计卡 R22）。
// 逆改点：重建后每轮再试 → provider.calls 超过 1（反复计费且永远注入不进去）。
func TestCompaction_NoRepeatedRegenerationWhenChainNotInjectable(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	h.seedGroups(t, "s1", "r1",
		group("g1", "查 A 文档", "A 的内容"), group("g2", "查 B 文档", "B 的内容"),
		group("g3", "查 C 文档", "C 的内容"))
	h.store.seedChain(t, compaction.CompactionEntry{
		CompactionID: "c-a", ChainEpoch: 1,
		CoveredFromMessageID: "m1", CutMessageID: "m2", FirstKeptMessageID: "m3",
		SourceRefs:   []core.EvidenceRef{ref("kb-a", "v1")},
		IncludedRefs: []core.EvidenceRef{ref("kb-a", "v1")},
	})
	h.validity.revoke(ref("kb-a", "v1")) // 旧根唯一依赖已失效 ⇒ 必须走重建路径
	h.store.rebuildRejected()            // 重建写入时再次被判无有效头（并发失效 / 断链）
	h.provider.respond("这次也会被丢弃")

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.Nil(t, res.Entry, "不可注入 / 未写入的摘要不得作为条目返回")
	require.True(t, res.Degraded)
	require.Equal(t, compaction.ReasonNoInjectableChain, res.ReasonCode)
	require.Equal(t, 1, h.provider.calls(t), "重建只试一次——不得每轮重试")

	_, err = h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.Equal(t, 1, h.provider.calls(t), "同一 Run 余下轮次不再调用摘要，直接降级")
}

// TestCompaction_AuditRefsDoNotGateInjection 是冻结用例（设计卡 R22）。
// 逆改点：拿审计集 source_refs 当注入判据 → 只因一条已被剔除的旧引用就判死整条链。
func TestCompaction_AuditRefsDoNotGateInjection(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(100000, 1, 100) // 预算充足 → 本轮不触发压缩，只验注入资格
	h.seedGroups(t, "s1", "r1", group("g1", "查 A 文档", "A 的内容"), group("g2", "查 B 文档", "B 的内容"))
	// 生成时 kb-old 已被剔除（只进审计集）；kb-a 才是正文真正依赖的来源
	h.store.seedChain(t, compaction.CompactionEntry{
		CompactionID: "c-a", ChainEpoch: 1,
		CoveredFromMessageID: "m1", CutMessageID: "m2", FirstKeptMessageID: "m3",
		SourceRefs:   []core.EvidenceRef{ref("kb-a", "v1"), ref("kb-old", "v1")},
		IncludedRefs: []core.EvidenceRef{ref("kb-a", "v1")},
	})
	h.validity.revoke(ref("kb-old", "v1")) // 只影响审计集，不影响当前依赖

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry, "审计集里的旧引用不得把整链判死")
	require.Equal(t, "c-a", res.Entry.CompactionID, "链合格 → 沿用既有条目，不重建")
	require.Equal(t, core.SummaryValid, res.Entry.Validity)
	require.Equal(t, 0, h.store.staleCalls(t), "审计集的失效不触发 MarkStale")
	require.Equal(t, 0, h.provider.calls(t), "链合格且无需压缩时零摘要调用")
	require.ElementsMatch(t, []core.EvidenceRef{ref("kb-a", "v1"), ref("kb-old", "v1")},
		res.Entry.SourceRefs, "装配记录与视图仍保留审计集，供追溯")
}

// —— 摘要调用前的过滤（C-26）——

// TestCompaction_InvalidSourcesFilteredBeforeSummaryCall —— 已撤权来源**不进入摘要请求**：
// 对应消息在输入里换占位符（原文不外发），引用只进审计集、不进依赖集。
// 逆改点：先发原文再靠注入前校验兜底 → 已撤权内容已经外发给摘要模型（收不回来）。
func TestCompaction_InvalidSourcesFilteredBeforeSummaryCall(t *testing.T) {
	h := newCompactionHarness(t)
	h.setLimits(300, 1, 100)
	const secret = "SECRET-A-内容"
	h.seedGroups(t, "s1", "r1",
		withSourceRefs(group("g1", "查 A 文档", secret), ref("kb-x", "v1")),
		group("g2", "查 B 文档", "B 的内容"),
		group("g3", "查 C 文档", "C 的内容"))
	h.validity.revoke(ref("kb-x", "v1"))

	res, err := h.compactor.Compact(ctx, h.request("s1", "r1"))
	require.NoError(t, err)
	require.NotNil(t, res.Entry)
	require.Equal(t, 1, h.provider.calls(t))
	require.True(t, h.summaryInputExcludes(t, secret), "已撤权原文不得进入摘要请求")
	require.True(t, h.summaryInputExcludes(t, "查 A 文档"), "撤权消息的提问原话同样不得外发")
	require.True(t, h.summaryInputContains(t, "[内容不可用：来源已失效]"),
		"失效消息整条换占位符、保留位置与标识")
	require.ElementsMatch(t, []core.EvidenceRef{ref("kb-x", "v1")}, res.Entry.SourceRefs,
		"引用进审计集，供追溯")
	require.Empty(t, res.Entry.IncludedRefs, "但不得进当前依赖集（否则永远注入不进去）")
}

// —— 引擎小工具 ——

func segmentIDs(segments []compaction.ContextSegment) []string {
	out := make([]string, 0, len(segments))
	for _, s := range segments {
		out = append(out, s.ID)
	}
	return out
}
