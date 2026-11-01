package tool_test

// 本文件是 `recall_memory` 的**召回模式与身份面**（US-16 P-07 / A-08）：缺省语义召回、
// 显式 `mode=keyword` 保留旧行为、身份由 Run 注入且零值拒绝、装配缺失失败关闭。
// 与 `memorytools_test.go` 同属外部测试包（`package tool_test`），共用 `countingStore`
// 与 `toolEnvWithAgent` 两件夹具。
// The recall mode and identity surface of recall_memory, in the external test package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// fakeRecaller 是 `tool.MemoryRecaller` 的**记录型**替身：留痕每次请求的完整字段
// （身份 / 作用域 / 检索词 / 模式），并按注入值回结果或错误。
// A recording double of the recall port; it keeps the whole request so the identity and
// mode assertions have real evidence.
type fakeRecaller struct {
	calls []tool.MemoryRecallRequest
	hits  []tool.MemoryRecallHit
	err   error
}

// RecallArchive 记一笔请求并回注入结果。
func (r *fakeRecaller) RecallArchive(_ context.Context, req tool.MemoryRecallRequest) ([]tool.MemoryRecallHit, error) {
	r.calls = append(r.calls, req)
	return r.hits, r.err
}

// 共享模式（`ShareGlobalMemory=true`）的分层块判据在 memoryrecall_shared_test.go：
// D-4 裁决把输出从"跨层连续编号 + [层名] 前缀"改成分层块、层内各自从 1 编号，原用例的
// 连续编号断言因此作废（判据面变更已登记）。
// The shared-mode judgement surface moved to memoryrecall_shared_test.go after the D-4
// ruling; the old continuous-numbering assertions are retired on purpose.

// TestRecallMemoryTool_DefaultModeIsSemantic 判 P-07 的缺省档：**不传 `mode` = 语义**
// （契约变更，显式登记）。三面一起判：召回端口收到的是语义模式与**原样**身份（归属 /
// 执行者 / scope 全等，不经过任何推断）、检索词先规范化、兼容载体一次都没碰；命中渲染
// 成 `名次. [来源] 正文`（A-08「带名次与来源」），来源为空时不渲染方括号。
// 逆改点：缺省模式改回 keyword → 端口零调用断言红；把 Principal 换成零值或从 Agent 名
// 反推 → 身份全等断言红；渲染丢掉名次 / 来源 → 文案断言红。
func TestRecallMemoryTool_DefaultModeIsSemantic(t *testing.T) {
	store := &countingStore{}
	recaller := &fakeRecaller{hits: []tool.MemoryRecallHit{
		{Rank: 1, Source: "save_memory", Content: "第一条：成本评估"},
		{Rank: 2, Content: "第二条：旧导入条目"},
	}}
	env := toolEnvWithAgent("alice")

	res, err := tool.NewRecallMemoryTool(store, recaller).Execute(ctx, env,
		json.RawMessage(`{"query":"  成本  "}`))
	require.NoError(t, err)

	require.Len(t, recaller.calls, 1, "缺省走语义端口，恰好一次")
	got := recaller.calls[0]
	require.Equal(t, "semantic", got.Mode, "省略 mode 即语义（P-07）")
	require.Equal(t, "成本", got.Query, "检索词先规范化再交给载体")
	require.Equal(t, core.Scope{Agent: "alice"}, got.Scope, "作用域仍只取 env.AgentName（FR-021）")
	require.Equal(t, env.Principal, got.Principal,
		"身份原样传递：归属（CallerID）与执行者（Actor）都不得被工具改写或推断")
	require.Zero(t, store.recalls, "语义档不得顺带碰关键词载体（不静默降级）")

	require.Equal(t, "1. [save_memory] 第一条：成本评估\n\n2. 第二条：旧导入条目", res.Content,
		"命中渲染 = `名次. [来源] 正文`，条目之间空行分隔；来源为空不渲染方括号")
}

// TestRecallMemoryTool_SemanticZeroHitIsSuccess 判语义档的零命中同样是**成功**（口径 10）：
// 占位文案逐字，且端口真的被调用过——「没有」必须是检索的结论，不是没查。
// 逆改点：零命中改成返回错误 → `NoError` 红；零命中直接短路不调研端口 → 调用计数红。
func TestRecallMemoryTool_SemanticZeroHitIsSuccess(t *testing.T) {
	recaller := &fakeRecaller{}
	res, err := tool.NewRecallMemoryTool(&countingStore{}, recaller).Execute(ctx, toolEnvWithAgent("alice"),
		json.RawMessage(`{"query":"没有这条"}`))
	require.NoError(t, err, "未命中是成功取值，不是失败（口径 10）")
	require.Equal(t, "(无匹配条目)", res.Content, "占位文案逐字（口径 11）")
	require.Len(t, recaller.calls, 1, "零命中也要真的查过")
}

// TestRecallMemoryTool_KeywordModeBypassesRecaller 判显式 `mode=keyword` 走**旧路径**：
// 兼容载体被调用恰好一次、语义端口零调用、渲染逐字等于旧行为（无编号、空行分隔）。
// 逆改点：keyword 也转发给语义端口 → 端口调用数红；渲染加上编号 → 文案断言红。
func TestRecallMemoryTool_KeywordModeBypassesRecaller(t *testing.T) {
	store := &countingStore{hits: []string{"第一条：成本评估", "第二条：成本复评"}}
	recaller := &fakeRecaller{hits: []tool.MemoryRecallHit{{Rank: 1, Content: "不该出现"}}}

	res, err := tool.NewRecallMemoryTool(store, recaller).Execute(ctx, toolEnvWithAgent("alice"),
		json.RawMessage(`{"query":"成本","mode":"keyword"}`))
	require.NoError(t, err)
	require.Equal(t, 1, store.recalls, "关键词档走兼容载体")
	require.Empty(t, recaller.calls, "关键词档不得调用语义端口")
	require.Equal(t, "第一条：成本评估\n\n第二条：成本复评", res.Content,
		"旧行为逐字保留：无编号、空行分隔（口径 11）")
}

// TestRecallMemoryTool_RejectsUnknownMode 判取值**区分大小写、不给别名**（与
// `save_memory.scope` 同形）：空串 / 大小写变体 / 不存在的模式一律 `invalid_input`，
// 且**零调用**——拒得不干净（先查一次再报错）比不拒更糟。
// 逆改点：比较改成 `strings.EqualFold` 或接受空串为语义 → `IsInvalidInput` 断言红；
// 先分派再校验取值 → 零调用断言红。
func TestRecallMemoryTool_RejectsUnknownMode(t *testing.T) {
	for _, in := range []string{
		`{"query":"成本","mode":""}`,
		`{"query":"成本","mode":"Semantic"}`,
		`{"query":"成本","mode":"KEYWORD"}`,
		`{"query":"成本","mode":"vector"}`,
	} {
		store := &countingStore{}
		recaller := &fakeRecaller{}
		_, err := tool.NewRecallMemoryTool(store, recaller).Execute(ctx, toolEnvWithAgent("alice"),
			json.RawMessage(in))
		require.Error(t, err, "取值不合法必须拒：%s", in)
		require.True(t, core.IsInvalidInput(err), "归类 = invalid_input：%s", in)
		require.Zero(t, store.recalls, "拒绝即零检索（兼容载体）：%s", in)
		require.Empty(t, recaller.calls, "拒绝即零调用（语义端口）：%s", in)
	}
}

// TestRecallMemoryTool_ZeroPrincipalFailsClosed 判 P-07 的身份闸门：零值身份在**触碰任何
// 载体之前**被拒（两种模式各判一次），错误链可判 `core.ErrUnauthorized`——**不得**把零值
// 补成管理员，也不得先查一次再拒绝。
// 逆改点：删掉 `Principal.Valid()` 闸门 → 无错误断言红；把闸门放在载体调用之后 →
// 零调用断言红；返回 `invalid_input` 冒充"模型入参问题" → `ErrUnauthorized` 断言红。
func TestRecallMemoryTool_ZeroPrincipalFailsClosed(t *testing.T) {
	for _, in := range []string{
		`{"query":"成本"}`,
		`{"query":"成本","mode":"keyword"}`,
	} {
		store := &countingStore{}
		recaller := &fakeRecaller{}
		env := toolEnvWithAgent("alice")
		env.Principal = core.Principal{} // 服务端漏注入 / 零值：**不得**补默认值

		_, err := tool.NewRecallMemoryTool(store, recaller).Execute(ctx, env, json.RawMessage(in))
		require.Error(t, err, "零值身份必须拒：%s", in)
		require.ErrorIs(t, err, core.ErrUnauthorized, "归类 = 授权不可用，不是入参问题：%s", in)
		require.False(t, core.IsInvalidInput(err), "不得冒充 invalid_input：%s", in)
		require.Zero(t, store.recalls, "拒绝发生在触碰载体之前：%s", in)
		require.Empty(t, recaller.calls, "拒绝发生在触碰端口之前：%s", in)
	}
}

// TestRecallMemoryTool_PassesPrincipalPerCall 判**每次调用重新传身份**（C-11「冻结内容
// 不冻结权限」）：同一工具实例上第二次调用携带另一个身份（后台调度档）时，端口收到的
// 是第二次那一份——工具不缓存上一次的结论，也不改写 Actor。这一条是"权限撤销 / 跨调用方"
// 在工具层的可判面：载体按当次身份重判，工具只负责如实传递。
// 逆改点：工具缓存第一次请求或按 Agent 名重建身份 → 第二次请求的身份断言红。
func TestRecallMemoryTool_PassesPrincipalPerCall(t *testing.T) {
	recaller := &fakeRecaller{}
	recall := tool.NewRecallMemoryTool(&countingStore{}, recaller)

	calls := toolEnvWithAgent("alice") // 第一次：外部调用方
	scheduled, err := core.OwnedPrincipal(
		"caller-memory-test", core.AuthorizingKindCallerKey, "key-memory-test",
		[]string{"agents:invoke:alice"}, core.ActorScheduler)
	require.NoError(t, err)
	background := toolEnvWithAgent("alice") // 第二次：后台调度（同一任务，执行者不同）
	background.Principal = scheduled

	for _, env := range []core.ToolEnv{calls, background} {
		_, err := recall.Execute(ctx, env, json.RawMessage(`{"query":"成本"}`))
		require.NoError(t, err)
	}
	require.Len(t, recaller.calls, 2)
	require.Equal(t, calls.Principal, recaller.calls[0].Principal)
	require.Equal(t, scheduled, recaller.calls[1].Principal,
		"第二次请求必须携带当次身份（归属不变、执行者 = scheduler）")
	require.Equal(t, core.ActorScheduler, recaller.calls[1].Principal.Actor,
		"后台任务的执行者标记得以保留——归属与执行者是两个字段")
}

// TestRecallMemoryTool_SemanticUnavailableIsActionable 判语义路径**未装配**时的处置：
// 返回**可自我纠正**的 `invalid_input`，文案点名缺什么并给出下一步（改用 `mode=keyword`）
// ——不静默降级成关键词检索（降级会让"检索变弱"看起来像"没有更多内容"）。两档：
// 端口未绑定（nil）与端口回 `tool.ErrRecallUnavailable`；第三档是反向对照——**其他**
// 错误原样上抛，不得被改写成入参问题。
// 逆改点：未装配时回落 `RecallByKeyword` → 关键词载体计数红；错误被吞成成功 → NoError 红。
func TestRecallMemoryTool_SemanticUnavailableIsActionable(t *testing.T) {
	t.Run("端口未绑定", func(t *testing.T) {
		store := &countingStore{hits: []string{"不该被取到"}}
		_, err := tool.NewRecallMemoryTool(store, nil).Execute(ctx, toolEnvWithAgent("alice"),
			json.RawMessage(`{"query":"成本"}`))
		require.Error(t, err, "未装配的语义路径不得静默降级")
		require.True(t, core.IsInvalidInput(err), "给模型的是可自我纠正的入参提示（口径 18）")
		require.Contains(t, err.Error(), "mode=keyword", "文案必须给出可执行的下一步")
		require.Zero(t, store.recalls, "未装配即零检索：不得回落到关键词路径")
	})

	t.Run("端口报告未装配", func(t *testing.T) {
		recaller := &fakeRecaller{err: fmt.Errorf("%w: 缺 embedding", tool.ErrRecallUnavailable)}
		_, err := tool.NewRecallMemoryTool(&countingStore{}, recaller).Execute(ctx,
			toolEnvWithAgent("alice"), json.RawMessage(`{"query":"成本"}`))
		require.Error(t, err)
		require.True(t, core.IsInvalidInput(err))
		require.Contains(t, err.Error(), "mode=keyword")
	})

	t.Run("其他错误原样上抛", func(t *testing.T) {
		recaller := &fakeRecaller{err: core.ErrForbidden}
		_, err := tool.NewRecallMemoryTool(&countingStore{}, recaller).Execute(ctx,
			toolEnvWithAgent("alice"), json.RawMessage(`{"query":"成本"}`))
		require.ErrorIs(t, err, core.ErrForbidden, "越权不是入参问题：不得被改写成 invalid_input")
		require.False(t, core.IsInvalidInput(err))
	})
}

// TestRecallMemoryTool_UnavailableErrorIsSentinel 守哨兵的**可判性**：`errors.Is` 必须能
// 穿过包装层识别未装配（工具据此翻译文案，**不得**比对错误文本）。
// 逆改点：把 `%w` 改成 `%v` → 本断言红（这正是文本匹配与错误链判定的分界）。
func TestRecallMemoryTool_UnavailableErrorIsSentinel(t *testing.T) {
	wrapped := fmt.Errorf("%w: %s", tool.ErrRecallUnavailable, "x")
	require.True(t, errors.Is(wrapped, tool.ErrRecallUnavailable))
	require.False(t, errors.Is(core.ErrForbidden, tool.ErrRecallUnavailable))
}

// TestRecallMemoryTool_IndexNotReadyIsNotNoMatch 判 V-5 的核心区分在**工具层**仍成立：
// 载体报"索引未就绪"时，工具必须返回可自我纠正的 `invalid_input` 并点名缺口；**不得**
// 落成 `(无匹配条目)`（那会让一次索引故障看起来像一次正常空结果），也不得与"未装配"
// 共用文案（两者的修复动作不同：一个等回填，一个补装配）。
// 逆改点：删掉 `ErrRecallIndexNotReady` 分支 → 错误原样上抛、不再 `IsInvalidInput`；
// 分支落到零命中占位 → `(无匹配条目)` 断言红；文案与未装配共用 → `等回填` 断言红。
func TestRecallMemoryTool_IndexNotReadyIsNotNoMatch(t *testing.T) {
	recaller := &fakeRecaller{err: fmt.Errorf("%w: 代际 3 在 agent 层有 2 条生效归档记忆尚无向量",
		tool.ErrRecallIndexNotReady)}

	res, err := tool.NewRecallMemoryTool(&countingStore{}, recaller).Execute(ctx,
		toolEnvWithAgent("alice"), json.RawMessage(`{"query":"成本"}`))
	require.Error(t, err, "索引缺口不是成功取值")
	require.True(t, core.IsInvalidInput(err), "给模型的是可自我纠正的提示（口径 18）")
	require.False(t, errors.Is(err, tool.ErrRecallUnavailable), "缺口不等于未装配：修复动作不同")
	require.Contains(t, err.Error(), "索引未就绪", "文案必须点名缺口，不得只报内部故障")
	require.Contains(t, err.Error(), "mode=keyword", "文案必须给出可执行的下一步")
	require.NotEqual(t, "(无匹配条目)", res.Content, "缺口不得伪装成无匹配（V-5）")
	require.Len(t, recaller.calls, 1, "结论来自真的一次检索，不是短路")
}

// TestRecallMemoryTool_IndexNotReadyErrorIsSentinel 守缺口哨兵的**可判性**：`errors.Is`
// 必须穿过包装层识别缺口，且与未装配哨兵互不误认——工具靠错误链分派文案，不比对文本。
// 逆改点：把 `%w` 改成 `%v` → 第一条断言红；两个哨兵合成一个 → 互不误认断言红。
func TestRecallMemoryTool_IndexNotReadyErrorIsSentinel(t *testing.T) {
	wrapped := fmt.Errorf("%w: %s", tool.ErrRecallIndexNotReady, "x")
	require.True(t, errors.Is(wrapped, tool.ErrRecallIndexNotReady))
	require.False(t, errors.Is(wrapped, tool.ErrRecallUnavailable), "缺口不得被识别成未装配")
	require.False(t, errors.Is(fmt.Errorf("%w: y", tool.ErrRecallUnavailable),
		tool.ErrRecallIndexNotReady), "未装配不得被识别成缺口")
}
