package memory_test

// 本文件是**注入面**的验收面（颗粒度文档 §3.1.4 / C-11 / FR-018）：注入文本来自
// 生效层核心区的 `Load` 读面，**不是** query 召回；归档区不进注入面；空层是空文本
// （提示词与基线逐字节一致的前提，提示词侧由
// `TestPromptBuilder_Build_EmptyMemoryIsByteIdenticalToSectionOne` 判卷）。
// T051 在本文件补**读错误三档**（层不存在 / I/O 失败 / 结构损坏不得合并成一个 `ok`）；
// US-16 补**两层不叠加**（Agent 层命中时全局层再注入一次 = 同一句出现两遍）。
// This file covers the injection surface: the frozen text comes from the effective
// layer's core region, never from a query recall, and an empty layer yields "";
// T051 adds the three read-error tiers that must never collapse into one "ok";
// US-16 adds the two-layer non-stacking judgment (stacking shows as a doubled sentence).

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// TestMemory_InjectionUsesLoadNotQuery 守 C-11 的注入语义：Run 开始的冻结读**生效层核心区
// 文本**——核心区完整、归档区不混入、全程不调用任何召回（语义或关键词）。
// 逆改点：Freeze 改成 Recall(query) → 归档条目混入或缺核心区 → 红；改成直接读整层原文
// → 归档条目混入 → 红。
// 验收点：C-11 / FR-009 / FR-015。
func TestMemory_InjectionUsesLoadNotQuery(t *testing.T) {
	svc, store, embedder := newSemanticForTest(t)
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "偏好：回答用中文"))
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionArchive, "项目代号：玄武"))

	snap, err := svc.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-inject",
		Scope:     core.GlobalScope,
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime,
	})
	require.NoError(t, err)
	require.Equal(t, "## 核心记忆\n\n- 偏好：回答用中文", snap.MemoryText,
		"注入文本 = 生效层核心区（重建形态），不含归档区")
	require.NotContains(t, snap.MemoryText, "玄武")
	require.Zero(t, embedder.calls, "注入面不得走语义召回")
	require.Zero(t, store.recallCalls, "注入面不得走归档召回")

	// 有 Agent 层时生效层是 Agent 层：全局层不叠加（两层不叠加的唯一实现）。
	require.NoError(t, svc.Append(ctx, core.Scope{Agent: "assistant"}, core.SectionCore, "Agent 偏好：先给结论"))
	snapAgent, err := svc.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-agent",
		Scope:     core.Scope{Agent: "assistant"},
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime,
	})
	require.NoError(t, err)
	require.Equal(t, "## 核心记忆\n\n- Agent 偏好：先给结论", snapAgent.MemoryText,
		"Agent 层命中时全局层一个字都不进")
}

// TestMemory_TwoLayersDoNotStack 守「两层不叠加」在**注入面**上的判据（US-16 §4 的
// `TestMemory_TwoLayersDoNotStack`）：两个层各写**同一句**核心条目时，生效层仍是 Agent
// 层这一份——同一句在注入文本里**只出现一次**（叠加的形态就是出现两遍），全局层独有的
// 条目一个字都不进；只有 Agent 层为空时才回退全局层，回退后同样只有一份。
//
// 逆改点：`LoadEffective` 在 Agent 层命中后仍加载并追加全局层核心区（叠加）→ 同一句出现
// 两遍、全局层独有条目混入，`Count` 断言与 `NotContains` 断言同时红。
// 验收点：C-11 / FR-018 / 口径 2。
func TestMemory_TwoLayersDoNotStack(t *testing.T) {
	svc, store, _ := newSemanticForTest(t)
	// 门面的 `LoadEffective` 是生效层回退的唯一实现；同一条存储上另起门面直判读面，
	// 语义存储的 `Freeze` 则判真正进提示词的那一份——两条路径判同一个决定。
	facade, err := memory.NewService(memory.Options{Backend: store, Logger: testLogger(t)})
	require.NoError(t, err)

	const shared = "回答一律先给结论"
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, shared))
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "只在全局层：称谓用「您」"))
	require.NoError(t, svc.Append(ctx, core.Scope{Agent: "assistant"}, core.SectionCore, shared))
	require.NoError(t, svc.Append(ctx, core.Scope{Agent: "assistant"}, core.SectionCore, "只在 Agent 层：先列风险"))

	// ① 读面：Agent 层命中 → 同一句只出现一次，且生效层确为 Agent 层（不是全局层）。
	effective, err := facade.LoadEffective(ctx, adminPrincipal(), "assistant")
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(effective, shared),
		"Agent 层有内容仍注入全局层 = 同一句出现两遍")
	require.Contains(t, effective, "只在 Agent 层：先列风险")
	require.NotContains(t, effective, "只在全局层：称谓用「您」", "Agent 层命中时全局层一个字都不进")

	// ② 注入面：Run 冻结文本与读面同源，判据同上。
	snap, err := svc.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-two-layers",
		Scope:     core.Scope{Agent: "assistant"},
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime,
	})
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(snap.MemoryText, shared), "注入面不得叠加全局层")
	require.NotContains(t, snap.MemoryText, "只在全局层：称谓用「您」")

	// ③ Agent 层为空才回退全局层：这时同一句来自全局层，仍然只有一份。
	effective, err = facade.LoadEffective(ctx, adminPrincipal(), "bob")
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(effective, shared), "回退后同样只有一份")
	require.Contains(t, effective, "只在全局层：称谓用「您」")
	require.NotContains(t, effective, "只在 Agent 层：先列风险")
}

// TestMemory_InjectionEmptyLayerIsEmptyText 守 FR-018 的记忆侧前提：没有任何记忆时冻结
// 文本是 `""`——提示词侧因此逐字节等于基线（不拼分隔符、不留空段）。
// 逆改点：空层返回标题行或占位文本 → 提示词与基线不再逐字节一致。
// 验收点：FR-018。
func TestMemory_InjectionEmptyLayerIsEmptyText(t *testing.T) {
	svc, _, _ := newSemanticForTest(t)

	snap, err := svc.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-empty",
		Scope:     core.GlobalScope,
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime,
	})
	require.NoError(t, err)
	require.Equal(t, "", snap.MemoryText, "空层 = 空文本（FR-018）")
	require.Equal(t, "run-empty", snap.RunID)
}

// readErrorStore 把 `Load` 固定成注入错误（其余端口由内嵌替身提供）——三档读错误里
// I/O 失败那档的注入面。
// readErrorStore pins Load to an injected error for the I/O-failure tier.
type readErrorStore struct {
	*semanticFakeStore
	err error
}

func (s *readErrorStore) Load(context.Context, core.Scope) (string, error) {
	return "", s.err
}

// 编译期断言：替身必须仍是本包端口，否则用例退化成"编译不过"而不是"判据成立"。
var _ memory.MemoryStore = (*readErrorStore)(nil)

// TestMemory_ReadErrorTiers 守注入面读路径的**三档**（§4 判别力表 / T051 / 约束 23 /
// 27）：① 层不存在 = 空文本（正常取值，不是失败）；② I/O 失败 = 错误上抛、不得返回
// 可注入文本；③ 非空但结构损坏 = 拒绝，错误点名缺失的分区标题，且不静默去注全局层。
// 三档**不得**合并成一个 `ok`——把损坏层当空层会让注入静默丢记忆。
// 逆改点：Freeze 吞掉 LoadEffective 的错误改成 `("", nil)` → ②③ 红；把"文件不存在"
// 当读取失败 → ① 红；把结构损坏当空层 → ③ 的错误断言与"归档记忆"文案断言红。
// 验收点：约束 23 / 27 / FR-018。
func TestMemory_ReadErrorTiers(t *testing.T) {
	root := t.TempDir()
	store := newSemanticFakeStore(root)
	svc := memory.NewSemanticStore(memory.SemanticDeps{Store: store})

	// ① 文件不存在 = 空层：冻结文本是空串，不是错误。
	snap, err := svc.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-missing",
		Scope:     core.GlobalScope,
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime,
	})
	require.NoError(t, err, "层不存在是正常取值，不是读取失败")
	require.Equal(t, "", snap.MemoryText)

	// ② I/O 失败 = 读取失败：上抛，不得返回任何可注入文本（含兼容读面）。
	failing := memory.NewSemanticStore(memory.SemanticDeps{
		Store: &readErrorStore{semanticFakeStore: newSemanticFakeStore(t.TempDir()), err: fs.ErrPermission},
	})
	snap, err = failing.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-io",
		Scope:     core.Scope{Agent: "alice"},
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime,
	})
	require.ErrorIs(t, err, fs.ErrPermission, "I/O 失败必须上抛（不得当空层回退继续跑）")
	require.Empty(t, snap.MemoryText, "失败时不得返回可注入文本")

	_, err = failing.Load(ctx, core.Scope{Agent: "alice"})
	require.ErrorIs(t, err, fs.ErrPermission, "兼容读面同样不得吞掉 I/O 失败")

	// ③ 非空但结构损坏（缺 `## 归档记忆`）：拒绝使用，且不得当空层去注全局层。
	manual := globalMemoryTitle + "\n\n## 核心记忆\n\n- 手写的偏好\n"
	file := filepath.Join(root, "memory", "agents", "alice", "MEMORY.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o750))
	require.NoError(t, os.WriteFile(file, []byte(manual), 0o600))

	snap, err = svc.Freeze(ctx, memory.FreezeRequest{
		RunID:     "run-corrupt",
		Scope:     core.Scope{Agent: "alice"},
		Principal: adminPrincipal(),
		Now:       fixedFreezeTime,
	})
	require.Error(t, err, "结构损坏拒绝使用（不得当空层回退）")
	require.Empty(t, snap.MemoryText)
	require.Contains(t, err.Error(), "归档记忆", "错误文案点名缺失的分区标题")
}
