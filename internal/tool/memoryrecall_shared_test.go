package tool_test

// 本文件是 review ② D-4 的**工具侧**判据：共享记忆开启时，两层命中按层分块渲染，
// 层内各自从 1 编号——输出里不出现横向比较两层相关性的序号。
//
// 为什么必须改输出而不只是改文档：模型读到 `2. [全局记忆] …` 会把它当成"第 2 名"，
// 而两层是各自检索、各自一次 embedding 的两份结果，从未放在一起排过序。序号本身在
// 暗示一个不存在的统一排名，文档再解释也补不回来。D-4 已裁决改成分层块（判据面变更：
// 原断言是 `1. [Agent 记忆] 私有 / 2. [全局记忆] 共享` 的连续编号）。
//
// This file pins the D-4 output shape: shared recall renders one block per layer with
// per-layer numbering, because the layers are retrieved independently and never jointly
// ranked. The judgement surface changed here on purpose and is recorded as such.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// 分层块的两个标题：与 `recallShared` 的输出逐字一致，也是模型识别"这两段来自不同层"
// 的唯一线索。
// The two block headings the model uses to tell the layers apart.
const (
	sharedAgentHeading  = "## Agent 记忆"
	sharedGlobalHeading = "## 全局记忆"
)

// layeredRecaller 按层回固定命中：Agent 层 {私有, 重复}、全局层 {共享, 重复}。
// 「重复」两侧都有，用来判跨层去重方向（保留先读到的 Agent 层那一份）。
// layeredRecaller answers per layer with a deliberate cross-layer duplicate.
type layeredRecaller struct{ calls []tool.MemoryRecallRequest }

// RecallArchive 记一笔请求，按作用域回预置命中。
func (r *layeredRecaller) RecallArchive(_ context.Context, req tool.MemoryRecallRequest) ([]tool.MemoryRecallHit, error) {
	r.calls = append(r.calls, req)
	if req.Scope.Agent == "" {
		return []tool.MemoryRecallHit{{Content: "共享"}, {Content: "重复"}}, nil
	}
	return []tool.MemoryRecallHit{{Content: "私有"}, {Content: "重复"}}, nil
}

// scriptedSharedRecaller 按召回次序回预置结果（第 n 次调用回第 n 项），用于"第一层成功、
// 第二层失败"这类跨调用行为。
// scriptedSharedRecaller answers the n-th call with the n-th scripted result.
type scriptedSharedRecaller struct {
	calls []tool.MemoryRecallRequest
	hits  []tool.MemoryRecallHit
	errs  []error
}

// RecallArchive 记一笔请求并按次序回结果或错误。
func (r *scriptedSharedRecaller) RecallArchive(_ context.Context, req tool.MemoryRecallRequest) ([]tool.MemoryRecallHit, error) {
	index := len(r.calls)
	r.calls = append(r.calls, req)
	if index < len(r.errs) && r.errs[index] != nil {
		return nil, r.errs[index]
	}
	return r.hits, nil
}

// TestRecallMemoryTool_ExplicitSharedLayers 判共享模式的分层块输出（D-4 裁决后的形态）：
// Agent 层整段在前、全局层整段在后；**层内重新从 1 编号**；跨层重复正文只留 Agent 层
// 那一份。两种模式（语义 / 关键词）输出同形——模式只决定怎么检索，不决定怎么呈现。
// 逆改点：改回连续编号（`len(lines)+1` 跨层累加）→ 精确文案断言红；交换两层顺序 →
// 同上；去掉跨层去重 → `Count(重复)` 变 2 红。
// 验收点：D-4；判据面变更（原断言为连续编号 + `[层名]` 前缀）。
func TestRecallMemoryTool_ExplicitSharedLayers(t *testing.T) {
	const want = sharedAgentHeading + "\n1. 私有\n\n2. 重复\n\n" +
		sharedGlobalHeading + "\n1. 共享"

	for _, mode := range []string{"semantic", "keyword"} {
		t.Run(mode, func(t *testing.T) {
			recaller := &layeredRecaller{}
			env := toolEnvWithAgent("alice")
			env.ShareGlobalMemory = true

			result, err := tool.NewRecallMemoryTool(&countingStore{}, recaller).Execute(ctx, env,
				json.RawMessage(fmt.Sprintf(`{"query":"事实","mode":%q}`, mode)))
			require.NoError(t, err)

			require.Len(t, recaller.calls, 2)
			require.Equal(t, core.Scope{Agent: "alice"}, recaller.calls[0].Scope, "先读 Agent 层")
			require.Equal(t, core.GlobalScope, recaller.calls[1].Scope, "再读全局层")
			require.Equal(t, "alice", recaller.calls[1].AuthorizationAgent,
				"全局层也按本次调用的 Agent 判权限，不跳过")
			require.Equal(t, mode, recaller.calls[0].Mode)

			require.Equal(t, want, result.Content,
				"分层块：层内各自从 1 编号，跨层不共享序号")
			require.Equal(t, 1, strings.Count(result.Content, "重复"), "跨层重复只留先读到的 Agent 层那一份")
		})
	}
}

// TestRecallMemoryTool_SharedOutputIsLayerBlocksNotUnifiedRank 判"序号不是排名"：
// 载体给的名次（这里 Agent 层 9、全局层 1）不进入共享模式的输出——共享模式的名次是
// **层内输出序号**，因为两层的检索结果从来没有一起排过序。若哪天真的实现了统一排序，
// 这个用例会红，届时必须连同契约一起改。
// 逆改点：改成直通 `hit.Rank` → 输出出现 `9.` 与 `1.` 的交错排布 → 精确文案断言红。
// 验收点：D-4。
func TestRecallMemoryTool_SharedOutputIsLayerBlocksNotUnifiedRank(t *testing.T) {
	recaller := &scriptedSharedRecaller{hits: []tool.MemoryRecallHit{{Rank: 9, Content: "同一条"}}}
	env := toolEnvWithAgent("alice")
	env.ShareGlobalMemory = true

	result, err := tool.NewRecallMemoryTool(&countingStore{}, recaller).Execute(ctx, env,
		json.RawMessage(`{"query":"事实","mode":"semantic"}`))
	require.NoError(t, err)

	// 两层回同一条正文：第二层被跨层去重吃掉，因此只剩 Agent 层那一块——正合"两层的
	// 名次不混排"的判据（若混排，这里会看到两段相同正文各带一个序号）。
	require.Equal(t, sharedAgentHeading+"\n1. 同一条", result.Content,
		"名次取层内序号，不直通载体给的 9")
	require.NotContains(t, result.Content, "9.")
}

// TestRecallMemoryTool_SharedUnavailableDoesNotSuggestKeyword 判装配缺失的文案在**共享
// 模式**下不给做不到的建议：两层都经语义召回端口读，`mode=keyword` 同样走不通，再写
// "改用 mode=keyword"就是把人往墙上引（D-4 要核实的一条边界）。
// 逆改点：复用语义模式的 `semanticUnavailable` 文案 → `NotContains(mode=keyword)` 红。
// 验收点：D-4。
func TestRecallMemoryTool_SharedUnavailableDoesNotSuggestKeyword(t *testing.T) {
	for _, mode := range []string{"semantic", "keyword"} {
		t.Run(mode, func(t *testing.T) {
			env := toolEnvWithAgent("alice")
			env.ShareGlobalMemory = true

			// recaller 为 nil：语义召回未装配（如 markdown 载体或缺 embedding 连接）。
			_, err := tool.NewRecallMemoryTool(&countingStore{}, nil).Execute(ctx, env,
				json.RawMessage(fmt.Sprintf(`{"query":"事实","mode":%q}`, mode)))
			require.Error(t, err)

			var invalid *core.InvalidInputError
			require.ErrorAs(t, err, &invalid, "装配缺失必须是可自我纠正的入参错误")
			require.Contains(t, invalid.Reason, "共享记忆",
				"文案要点名是共享模式要求的装配，而不是笼统的语义召回")
			require.NotContains(t, invalid.Reason, "mode=keyword",
				"共享模式不需要关键词：这条建议在这里做不到")
		})
	}
}

// TestRecallMemoryTool_SharedIndexGapNamesLayer 判索引缺口在共享模式下的可操作性：
// 文案点名**是哪一层**缺口，并保留"改用 mode=keyword 或等回填"两条真能走的路
// ——关键词路径不需要向量，这里给的建议是可执行的（与上一条相反）。
// 逆改点：沿用不分层的 `indexNotReady` 文案 → 层名断言红；改用装配缺失文案 →
// `mode=keyword` 断言红。
// 验收点：D-4。
func TestRecallMemoryTool_SharedIndexGapNamesLayer(t *testing.T) {
	gap := fmt.Errorf("%w: 代际 1 在全局层有 3 条生效归档记忆尚无向量", tool.ErrRecallIndexNotReady)
	recaller := &scriptedSharedRecaller{
		hits: []tool.MemoryRecallHit{{Content: "私有"}},
		errs: []error{nil, gap},
	}
	env := toolEnvWithAgent("alice")
	env.ShareGlobalMemory = true

	_, err := tool.NewRecallMemoryTool(&countingStore{}, recaller).Execute(ctx, env,
		json.RawMessage(`{"query":"事实","mode":"semantic"}`))
	require.Error(t, err)

	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, invalid.Reason, "全局记忆", "缺口必须点名是哪一层")
	require.Contains(t, invalid.Reason, "mode=keyword", "关键词路径不走向量，这条建议可执行")
	require.Len(t, recaller.calls, 2, "失败发生在第二层，两层的检索都已发生")
}

// TestRecallMemoryTool_SharedSecondLayerFailureDiscardsFirst 判"任一层失败即整次失败"：
// 第一层已经拿到的命中**不得**作为部分结果返回——半份召回会让模型以为全局层没有内容。
// 逆改点：把第二层的错误吞成 `continue` → NoError 且结果只剩 Agent 块 → 本用例红。
// 验收点：D-4。
func TestRecallMemoryTool_SharedSecondLayerFailureDiscardsFirst(t *testing.T) {
	boom := errors.New("injected: 全局层检索失败")
	recaller := &scriptedSharedRecaller{
		hits: []tool.MemoryRecallHit{{Content: "私有"}},
		errs: []error{nil, boom},
	}
	env := toolEnvWithAgent("alice")
	env.ShareGlobalMemory = true

	result, err := tool.NewRecallMemoryTool(&countingStore{}, recaller).Execute(ctx, env,
		json.RawMessage(`{"query":"事实","mode":"semantic"}`))
	require.ErrorIs(t, err, boom, "载体错误原样上抛")
	require.Empty(t, result.Content, "失败时不得返回第一层的部分结果")
}

// TestRecallMemoryTool_SharedRevokedKeyStopsAtFirstLayer 判权限撤销的失败关闭：第一层
// 就拒（`core.ErrForbidden`）时，第二层根本不再发起——撤销之后不该再多花一次检索，
// 更不该继续读全局层。
// 逆改点：把逐层的错误吞掉继续读 → `Len(calls)` 变 2 红。
// 验收点：D-4；C-11。
func TestRecallMemoryTool_SharedRevokedKeyStopsAtFirstLayer(t *testing.T) {
	recaller := &scriptedSharedRecaller{
		errs: []error{core.ErrForbidden, nil},
	}
	env := toolEnvWithAgent("alice")
	env.ShareGlobalMemory = true

	_, err := tool.NewRecallMemoryTool(&countingStore{}, recaller).Execute(ctx, env,
		json.RawMessage(`{"query":"事实","mode":"semantic"}`))
	require.ErrorIs(t, err, core.ErrForbidden)
	require.Len(t, recaller.calls, 1, "撤销的 Key 不得触发第二次检索")
	require.Equal(t, core.Scope{Agent: "alice"}, recaller.calls[0].Scope, "拒在第一层")
}
