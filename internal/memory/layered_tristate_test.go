package memory_test

// 本文件是 review ② D-1 的实现侧回归：`include_global_memory` 的三态语义，以及
// 「配置只决定读哪几层，不决定能不能读」。
// This file is the review ② D-1 regression: the three-state semantics of
// include_global_memory, and the rule that configuration selects layers but never grants
// permission to read them.
//
// 三态（C-52）：
//   - 未声明 → 兼容回退：Agent 层任一分区有内容只注该层核心区，为空才回退全局层；
//   - `false` → 只注 Agent 层，**永不**回退；
//   - `true`  → 两层合并注入（`## 全局记忆` 在前、`## Agent 记忆` 在后，按正文去重）。
//
// 授权（技术方案 §3.5.2「配置只决定读哪几层，不决定能不能读」）：身份有效、
// `agents:invoke:<agent>` scope、调用方 Key 的**当前**状态（撤销即拒）——三条路径
// 一致，且判定必须发生在读**任何一层**之前。未声明不是鉴权旁路：它只是"层怎么选"
// 的缺省语义，不是"跳过复核"的开关。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// layerCountBackend 是按层计数的记忆载体替身：记录每次 Load 读的是哪一层，并可按层
// 注入读取错误。「读了几层、先读哪层」因此成为可断言的事实——三态语义里最容易悄悄
// 漂移的就是"该不该多读一层"。
// layerCountBackend records which layer each Load touched and can fail a layer on
// demand, making "how many layers were read" an assertable fact.
type layerCountBackend struct {
	texts map[string]string
	errs  map[string]error
	reads []string
}

// Append 是写面，本文件的用例不写。
func (b *layerCountBackend) Append(context.Context, core.Scope, core.Section, string) error {
	return nil
}

// Load 记录作用域（Agent 名为空 = 全局层）并返回预置文本或预置错误。
func (b *layerCountBackend) Load(_ context.Context, scope core.Scope) (string, error) {
	b.reads = append(b.reads, scope.Agent)
	if err, ok := b.errs[scope.Agent]; ok {
		return "", err
	}
	return b.texts[scope.Agent], nil
}

// RecallByKeyword 是归档区检索，本文件的用例不碰。
func (b *layerCountBackend) RecallByKeyword(context.Context, core.Scope, string) ([]string, error) {
	return nil, nil
}

// gatedLayerBackend 在计数carrier 之上再实现 `MemoryAccessChecker`：把"授权在第几层
// 读取之前发生"变成可断言事实。
// gatedLayerBackend adds the live-key check so that "authorization precedes the read"
// becomes assertable.
type gatedLayerBackend struct {
	layerCountBackend
	authorizeErr error
	authorized   []string
}

// AuthorizeMemory 记录被授权检查的 Agent 并按预置错误返回。
func (b *gatedLayerBackend) AuthorizeMemory(_ context.Context, _ core.Principal, agent string) error {
	b.authorized = append(b.authorized, agent)
	return b.authorizeErr
}

// callerFor 构造一个只覆盖指定 Agent 的合法调用方身份。
func callerFor(agent string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller-1",
		KeyID:    "key-1",
		Actor:    core.ActorCaller,
		Scopes:   []string{core.AgentInvokeScopePrefix + agent},
	}
}

// memoryDoc 按载体的文档化形态拼一层记忆：核心区在前、归档区在后（空条目即空分区）。
func memoryDoc(coreEntries ...string) string {
	var b strings.Builder
	b.WriteString(globalMemoryTitle + "\n\n## 核心记忆\n\n")
	for _, entry := range coreEntries {
		b.WriteString("- " + entry + "\n\n")
	}
	b.WriteString("## 归档记忆\n")
	return b.String()
}

// newLayerService 用替身载体建门面。
func newLayerService(t *testing.T, backend core.LongTermMemoryStore) *memory.Service {
	t.Helper()
	svc, err := memory.NewService(memory.Options{Backend: backend, Logger: testLogger(t)})
	require.NoError(t, err)
	return svc
}

// TestMemory_LoadEffectiveAuthorizationPrecedesRead 守「未声明路径同样要授权」（C-52）：
// 兼容回退读的是**全局层**——正是最需要复核身份与 Key 状态的那一层。
// 逆改点：把 `LoadEffective` 顶部的 `authorizeRead` 删掉或挪到 `backend.Load` 之后 →
// `reads` 非空、错误变成"读到内容"→ 本用例红。
// 验收点：FR-015 / C-52；review ② D-1。
func TestMemory_LoadEffectiveAuthorizationPrecedesRead(t *testing.T) {
	newBackend := func() *gatedLayerBackend {
		return &gatedLayerBackend{layerCountBackend: layerCountBackend{
			// Agent 层为空、全局层有内容：直接落在回退分支上。
			texts: map[string]string{"": memoryDoc("共享事实")},
		}}
	}

	t.Run("零值身份", func(t *testing.T) {
		backend := newBackend()
		svc := newLayerService(t, backend)
		_, err := svc.LoadEffective(ctx, core.Principal{}, "alice")
		require.ErrorIs(t, err, core.ErrUnauthorized)
		require.Empty(t, backend.reads, "身份无效时不得先读载体")
		require.Empty(t, backend.authorized)
	})

	t.Run("缺少 agent scope", func(t *testing.T) {
		backend := newBackend()
		svc := newLayerService(t, backend)
		_, err := svc.LoadEffective(ctx, callerFor("bob"), "alice")
		require.ErrorIs(t, err, core.ErrForbidden)
		require.Empty(t, backend.reads, "scope 不足时不得先读载体")
		require.Empty(t, backend.authorized, "scope 判定先于 Key 复核")
	})

	t.Run("Key 已撤销", func(t *testing.T) {
		backend := newBackend()
		backend.authorizeErr = core.ErrForbidden
		svc := newLayerService(t, backend)
		_, err := svc.LoadEffective(ctx, callerFor("alice"), "alice")
		require.ErrorIs(t, err, core.ErrForbidden)
		require.Equal(t, []string{"alice"}, backend.authorized, "调用方每次读取都要重查 Key 状态")
		require.Empty(t, backend.reads, "撤销判定必须发生在读载体之前")
	})

	t.Run("无 Agent 上下文只允许管理员", func(t *testing.T) {
		// 空 Agent 名 = 全局层（core.Scope 的零值语义），这一层没有 agents:invoke:<name>
		// 可判，因此只允许内置管理员读取；调用方即使持有 Key 也不得越界。
		backend := newBackend()
		svc := newLayerService(t, backend)
		caller := callerFor("alice")
		_, err := svc.LoadEffective(ctx, caller, "")
		require.ErrorIs(t, err, core.ErrForbidden)
		require.Empty(t, backend.reads)
	})

	t.Run("管理员读到全局回退层", func(t *testing.T) {
		backend := newBackend()
		svc := newLayerService(t, backend)
		text, err := svc.LoadEffective(ctx, core.LocalAdminPrincipal(), "alice")
		require.NoError(t, err)
		require.Contains(t, text, "共享事实")
		require.Equal(t, []string{"alice", ""}, backend.reads, "先 Agent 层、为空才回退全局层")
		require.Empty(t, backend.authorized, "内置管理员不查 Key")
	})
}

// TestMemory_LoadLayersAuthorizationPrecedesLayerChoice 守「配置只决定读哪几层，不决定
// 能不能读」：`false` 与 `true` 两条显式路径都必须在任一层读取之前完成授权。
// 逆改点：把 `LoadLayers` 的授权块移到 `backend.Load` 之后，或只在 `includeGlobal`
// 为真时授权 → `false` 分支的 `reads` 非空 → 本用例红。
// 验收点：FR-015 / C-52；review ② D-1。
func TestMemory_LoadLayersAuthorizationPrecedesLayerChoice(t *testing.T) {
	for _, includeGlobal := range []bool{false, true} {
		t.Run(map[bool]string{false: "仅 Agent 层", true: "两层合并"}[includeGlobal], func(t *testing.T) {
			newBackend := func() *gatedLayerBackend {
				return &gatedLayerBackend{layerCountBackend: layerCountBackend{
					texts: map[string]string{
						"alice": memoryDoc("Agent 偏好"),
						"":      memoryDoc("共享事实"),
					},
				}}
			}

			backend := newBackend()
			svc := newLayerService(t, backend)
			_, err := svc.LoadLayers(ctx, "alice", core.Principal{}, includeGlobal)
			require.ErrorIs(t, err, core.ErrUnauthorized)
			require.Empty(t, backend.reads, "身份无效时不得读任何一层")

			backend = newBackend()
			backend.authorizeErr = core.ErrForbidden
			svc = newLayerService(t, backend)
			_, err = svc.LoadLayers(ctx, "alice", callerFor("alice"), includeGlobal)
			require.ErrorIs(t, err, core.ErrForbidden,
				"撤销的 Key 不得因为 include_global_memory=%v 就放行", includeGlobal)
			require.Equal(t, []string{"alice"}, backend.authorized)
			require.Empty(t, backend.reads, "撤销的 Key 不得触发任何一层读取")
		})
	}

	t.Run("无 Agent 上下文的显式分层读取一律拒绝", func(t *testing.T) {
		backend := &gatedLayerBackend{layerCountBackend: layerCountBackend{
			texts: map[string]string{"": memoryDoc("共享事实")},
		}}
		svc := newLayerService(t, backend)
		_, err := svc.LoadLayers(ctx, "", core.LocalAdminPrincipal(), true)
		require.ErrorIs(t, err, core.ErrForbidden)
		require.Empty(t, backend.reads)
	})
}

// TestMemory_IncludeGlobalMemoryFalseNeverFallsBackToGlobal 守 `false` 的分支语义：
// 只读 Agent 层，Agent 层为空也**不**回退——把 false 读成"沿用旧规则"会让用户显式关掉
// 的全局层内容在空 Agent 层上悄悄回来。
// 逆改点：把 `LoadLayers` 的 `if !includeGlobal` 分支改成 `return s.LoadEffective(...)`
// → 回退发生、`reads` 变成两层 → 本用例红。
// 验收点：C-52；review ② D-1。
func TestMemory_IncludeGlobalMemoryFalseNeverFallsBackToGlobal(t *testing.T) {
	backend := &gatedLayerBackend{layerCountBackend: layerCountBackend{
		texts: map[string]string{"": memoryDoc("共享事实")},
	}}
	svc := newLayerService(t, backend)
	text, err := svc.LoadLayers(ctx, "alice", core.LocalAdminPrincipal(), false)
	require.NoError(t, err)
	require.Empty(t, text, "Agent 层为空时 false 给空段，不回退全局层")
	require.Equal(t, []string{"alice"}, backend.reads, "false 只读 Agent 层")
}

// TestMemory_IncludeGlobalMemoryTrueSectionOrderAndHeadingOmission 守合并注入的形状
// （C-52）：`## 全局记忆` 在前、`## Agent 记忆` 在后；某一层为空时**不**渲染空标题。
// 逆改点：交换两段的拼接顺序，或用 `renderSection`（无内容也出行标题）替换
// `renderSectionOrEmpty` → 本用例红。
// 验收点：C-52；review ② D-1。
func TestMemory_IncludeGlobalMemoryTrueSectionOrderAndHeadingOmission(t *testing.T) {
	t.Run("两层都有内容", func(t *testing.T) {
		svc := newLayerService(t, &gatedLayerBackend{layerCountBackend: layerCountBackend{
			texts: map[string]string{"": memoryDoc("共享事实"), "alice": memoryDoc("Agent 偏好")},
		}})
		text, err := svc.LoadLayers(ctx, "alice", core.LocalAdminPrincipal(), true)
		require.NoError(t, err)
		globalAt := strings.Index(text, "## 全局记忆")
		agentAt := strings.Index(text, "## Agent 记忆")
		require.NotEqual(t, -1, globalAt)
		require.NotEqual(t, -1, agentAt)
		require.Less(t, globalAt, agentAt, "全局层整段在前、Agent 层整段在后")
	})

	t.Run("只有全局层", func(t *testing.T) {
		svc := newLayerService(t, &gatedLayerBackend{layerCountBackend: layerCountBackend{
			texts: map[string]string{"": memoryDoc("共享事实")},
		}})
		text, err := svc.LoadLayers(ctx, "alice", core.LocalAdminPrincipal(), true)
		require.NoError(t, err)
		require.Contains(t, text, "## 全局记忆")
		require.NotContains(t, text, "## Agent 记忆", "空层不得渲染空标题")
	})

	t.Run("只有 Agent 层", func(t *testing.T) {
		svc := newLayerService(t, &gatedLayerBackend{layerCountBackend: layerCountBackend{
			texts: map[string]string{"alice": memoryDoc("Agent 偏好")},
		}})
		text, err := svc.LoadLayers(ctx, "alice", core.LocalAdminPrincipal(), true)
		require.NoError(t, err)
		require.Contains(t, text, "## Agent 记忆")
		require.NotContains(t, text, "## 全局记忆", "空层不得渲染空标题")
	})
}

// TestMemory_IncludeGlobalMemoryTrueDedupeKeepsAgentCopy 守去重方向：同一句正文两层都
// 有时，保留 **Agent 层**那一份（Agent 层的表述比全局层更具体），全局段把重句剔掉。
// 逆改点：把 `seen` 的填充顺序反过来（先灌 global 再滤 local）→ 重句落进全局段、
// 位置断言红。
// 验收点：C-52；review ② D-1。
func TestMemory_IncludeGlobalMemoryTrueDedupeKeepsAgentCopy(t *testing.T) {
	svc := newLayerService(t, &gatedLayerBackend{layerCountBackend: layerCountBackend{
		texts: map[string]string{
			"":      memoryDoc("相同事实", "只属于全局"),
			"alice": memoryDoc("相同事实", "只属于 Agent"),
		},
	}})
	text, err := svc.LoadLayers(ctx, "alice", core.LocalAdminPrincipal(), true)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(text, "相同事实"), "重句只出现一次")
	agentAt := strings.Index(text, "## Agent 记忆")
	require.Greater(t, strings.Index(text, "相同事实"), agentAt, "保留的是 Agent 层那一份")
	require.Less(t, strings.Index(text, "只属于全局"), agentAt, "全局层独有句留在全局段")
}

// TestMemory_IncludeGlobalMemoryTrueGlobalReadFailureIsFatal 守「任一层的读取失败即整次
// 失败」（C-52）：不返回部分命中、不把全局层读失败悄悄当成"全局层为空"。
// 逆改点：把全局层 `backend.Load` 的错误改成 `continue` / 吞掉 → `err` 变 nil 且
// 文本只剩 Agent 段 → 本用例红。
// 验收点：C-52；review ② D-1。
func TestMemory_IncludeGlobalMemoryTrueGlobalReadFailureIsFatal(t *testing.T) {
	boom := errors.New("injected: global layer unreadable")
	svc := newLayerService(t, &gatedLayerBackend{layerCountBackend: layerCountBackend{
		texts: map[string]string{"alice": memoryDoc("Agent 偏好")},
		errs:  map[string]error{"": boom},
	}})
	text, err := svc.LoadLayers(ctx, "alice", core.LocalAdminPrincipal(), true)
	require.ErrorIs(t, err, boom, "载体错误原样上抛")
	require.Empty(t, text, "失败时不得返回部分合并结果")
}

// TestMemory_LayerReadCountsByChoice 把三态压成一张"读了几层"的表：未声明在 Agent 层
// 命中时**不得**多读全局层（旧行为逐字节不变），未命中才回退一次。
// 逆改点：未声明路径改成"总是读两层再挑" → 第一条用例的 `reads` 变成两层 → 红。
// 验收点：C-52 / 口径 2；review ② D-1。
func TestMemory_LayerReadCountsByChoice(t *testing.T) {
	enabled, disabled := true, false
	backendFor := func(agentText string) *gatedLayerBackend {
		return &gatedLayerBackend{layerCountBackend: layerCountBackend{
			texts: map[string]string{"alice": agentText, "": memoryDoc("共享事实")},
		}}
	}

	cases := []struct {
		name      string
		agentText string
		settings  *bool
		wantReads []string
	}{
		{name: "未声明/Agent 层有内容", agentText: memoryDoc("Agent 偏好"), settings: nil, wantReads: []string{"alice"}},
		{name: "未声明/Agent 层为空", agentText: "", settings: nil, wantReads: []string{"alice", ""}},
		{name: "false/Agent 层为空也不回退", agentText: "", settings: &disabled, wantReads: []string{"alice"}},
		{name: "true/两层都读", agentText: memoryDoc("Agent 偏好"), settings: &enabled, wantReads: []string{"alice", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := backendFor(tc.agentText)
			svc := newLayerService(t, backend)
			if tc.settings == nil {
				_, err := svc.LoadEffective(ctx, core.LocalAdminPrincipal(), "alice")
				require.NoError(t, err)
			} else {
				_, err := svc.LoadLayers(ctx, "alice", core.LocalAdminPrincipal(), *tc.settings)
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantReads, backend.reads)
		})
	}
}
