package memory_test

// 本文件是**核心区配额边界**在语义存储组装层的验收面（颗粒度文档 §3.2 / T051 /
// FR-021）：单条 4096、层内总量 32768 的判据是**严格大于**——恰在上限放行、多 1 字节
// 即拒；拒绝是类型化 `*core.MemoryQuotaError`，调用方按 `Kind` 分档判；归档区无配额。
// 组装层的判别力是"SemanticStore 没有另起一套配额（含把边界写成 `>=`）"。
// This file pins the core-quota boundaries through the SemanticStore assembly: the
// comparison is strict greater-than, refusals are typed, and archive writes are uncapped.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestMemory_CoreQuotaBoundaries 守配额边界「不含等号」（§4 判别力表 / T051）：
// ① 单条恰 4096 放行、4097 拒绝且 `Kind=entry`；② 层内总量恰 32768 放行、多 1 字节
// 拒绝且 `Kind=section`，被拒的写入不留痕；③ 归档区同一份超限内容照常写入。
// 逆改点：任一处把 `>` 写成 `>=` → 恰在上限的写入被拒、断言红；把配额套到归档区 →
// ③ 红；让拒绝路径先落半条 → 条目数断言红。
// 验收点：FR-021 / 约束 7 / 口径 6。
func TestMemory_CoreQuotaBoundaries(t *testing.T) {
	svc, _, _ := newSemanticForTest(t)

	// ① 单条边界：恰 4096 放行，4097 拒绝；拒绝档位是 entry（单条判据先于总量判据）。
	exact := strings.Repeat("a", core.MaxCoreEntryBytes)
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, exact),
		"恰等单条上限 == 放行（判据是严格大于）")

	var quota *core.MemoryQuotaError
	err := svc.Append(ctx, core.GlobalScope, core.SectionCore, strings.Repeat("b", core.MaxCoreEntryBytes+1))
	require.ErrorAs(t, err, &quota, "单条越限必须是类型化拒绝（errors.As 可判）")
	require.Equal(t, core.QuotaKindEntry, quota.Kind)
	require.Equal(t, core.MaxCoreEntryBytes, quota.Limit)

	// ② 总量边界：补齐到 8 条恰 4096（= 32768）全部放行，多 1 字节即拒，档位是 section。
	perSection := core.MaxCoreSectionBytes / core.MaxCoreEntryBytes
	for i := 1; i < perSection; i++ {
		require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore,
			strings.Repeat(string(rune('c'+i)), core.MaxCoreEntryBytes)),
			"第 %d 条恰在单条上限内", i+1)
	}
	err = svc.Append(ctx, core.GlobalScope, core.SectionCore, "x")
	require.ErrorAs(t, err, &quota, "到顶后 1 字节都进不去")
	require.Equal(t, core.QuotaKindSection, quota.Kind, "总量判据报的是层内求和上限")
	require.Equal(t, core.MaxCoreSectionBytes, quota.Limit)

	// 被拒的两次写入不留痕：核心区仍是 8 条（拒绝路径不得先落半条再报错）。
	text, err := svc.Load(ctx, core.GlobalScope)
	require.NoError(t, err)
	require.Len(t, sectionBodiesForTest(t, text, coreHeading), perSection, "被拒的写入不得留痕")

	// ③ 归档区无配额（约束 7）：两倍层上限的内容照常追加。
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionArchive,
		strings.Repeat("z", core.MaxCoreSectionBytes*2)), "归档区无配额，只追加不拒绝")
}
