package app

// 本文件覆盖范围转换编排（颗粒度文档 R4.4 / T121）：共享转专属前必须点名
// 绑定者 / 未释放运行 / 消息路由，任一命中即 409 且零写入；无阻断时原子生效；
// 按名不按 UID 的调用被拒；并发转换不丢修订。
// These tests cover scope conversion: narrowing requires proof that nobody uses the
// resource, and a blocked conversion writes nothing.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// stubRoutePort 是消息路由端口的替身：本轮产品里恒为空，测试用它验证阻断路径。
// stubRoutePort stands in for the message-route port, which is empty in this revision.
type stubRoutePort struct {
	routes []string
	err    error
}

func (s *stubRoutePort) RoutesBlocking(context.Context, core.ResourceKind, core.ResourceUID) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.routes, nil
}

// scopeEnv 是范围转换的夹具：在绑定夹具之上补第二个 Agent、归档 Agent 与运行账本。
// scopeEnv extends the binding fixture with a second agent, an archived agent and runs.
type scopeEnv struct {
	*bindingsEnv
	service *ScopeService
	routes  *stubRoutePort
	runs    *storage.RunStore
}

// newScopeEnv 建夹具：reporter（启用）由绑定夹具写入，这里补 ops-bot（停用，可作属主）
// 与 retired（归档，不可作属主）。
// newScopeEnv assembles the fixture; a disabled agent is a valid owner, an archived one is not.
func newScopeEnv(t *testing.T, routes *stubRoutePort) *scopeEnv {
	t.Helper()
	base := newBindingsEnv(t)
	ctx := context.Background()

	seedAgent := func(name string, state core.AgentState) {
		uid, err := core.NewAgentUID()
		require.NoError(t, err)
		require.NoError(t, base.resources.PutLifecycleIndex(ctx, core.LifecycleRecord{
			SchemaVersion: 2, Agent: name, AgentUID: uid, State: state,
			Generation: 1, UpdatedAt: baselineClock().UnixMilli(), OperationID: base.opID,
		}, true, ""))
	}
	seedAgent("ops-bot", core.AgentDisabled)
	seedAgent("retired", core.AgentArchived)

	if routes == nil {
		routes = &stubRoutePort{}
	}
	return &scopeEnv{
		bindingsEnv: base,
		routes:      routes,
		runs:        storage.NewRunStore(base.db),
		service: &ScopeService{
			Resources:     base.resources,
			Agents:        base.resources,
			WorkspaceRoot: base.root,
			Routes:        routes,
			Now:           baselineClock,
		},
	}
}

// pointerScope 读模型连接当前的指针字段（用于判"零写入"与"已生效"）。
// pointerScope reads the current pointer of one model connection.
func (e *scopeEnv) pointerScope(t *testing.T, name string) core.ResourcePointer {
	t.Helper()
	ptr, err := e.resources.Pointer(context.Background(), core.ResourceModelConnection, name)
	require.NoError(t, err)
	return ptr
}

// TestScopeConversion_BlockedByBinding —— 别的 Agent 还绑着这条资源时，共享转专属
// 必须被拒绝并点名，指针零变化；解绑后才允许转换。
func TestScopeConversion_BlockedByBinding(t *testing.T) {
	ctx := context.Background()
	env := newScopeEnv(t, nil)
	version := env.publishModelVersion(t, "alpha")

	otherAgent, err := core.NewAgentUID()
	require.NoError(t, err)
	require.NoError(t, env.bindings.Rebuild(ctx, otherAgent, "ops-bot", "hash",
		core.ResourceBindings{
			ModelConnections: []core.ResourceBinding{{Name: "alpha", UID: version.ResourceUID}},
		}, baselineClock()))

	before := env.pointerScope(t, "alpha")
	_, err = env.service.Convert(ctx, ScopeRequest{
		Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
		Scope: core.ScopeExclusive, OwnerAgent: "reporter", OperationID: env.opID,
	})
	require.ErrorIs(t, err, core.ErrResourceScopeInUse)
	require.Contains(t, err.Error(), string(otherAgent), "409 必须点名绑定者")

	after := env.pointerScope(t, "alpha")
	require.Equal(t, before.Scope, after.Scope, "被拒绝的转换必须零写入")
	require.Equal(t, before.Revision, after.Revision)
	require.Equal(t, core.ScopeShared, after.Scope)

	// 解除绑定后同一个请求通过：属主是 reporter（启用）。
	require.NoError(t, env.bindings.Clear(ctx, otherAgent))
	result, err := env.service.Convert(ctx, ScopeRequest{
		Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
		Scope: core.ScopeExclusive, OwnerAgent: "reporter", OperationID: env.opID,
	})
	require.NoError(t, err)
	require.Equal(t, core.ScopeExclusive, result.Scope)
	require.Equal(t, bindingAgentUID, result.OwnerAgentUID)

	applied := env.pointerScope(t, "alpha")
	require.Equal(t, core.ScopeExclusive, applied.Scope)
	require.Equal(t, bindingAgentUID, applied.OwnerAgentUID)
	require.Equal(t, before.Revision+1, applied.Revision, "一次转换 = 一次修订")
}

// TestScopeConversion_BlockedByRun —— 未释放的运行引用同样阻断收缩；运行结束后放行。
func TestScopeConversion_BlockedByRun(t *testing.T) {
	ctx := context.Background()
	env := newScopeEnv(t, nil)
	version := env.publishModelVersion(t, "alpha")

	manifest := core.RunResourceManifest{
		FormatVersion:  1,
		AgentLifecycle: string(core.AgentActive),
		Provider: core.ProviderVersionRef{
			ResourceVersionRef: core.ResourceVersionRef{
				Name: "alpha", ResourceUID: version.ResourceUID,
				VersionID: version.ID, VersionNo: version.VersionNo,
				ContentHash: version.ContentHash,
			},
			Model: "alpha-model",
		},
	}
	manifestHash, err := core.HashManifest(manifest)
	require.NoError(t, err)
	manifest.Hash = manifestHash
	runID := "run-scope-1"
	require.NoError(t, env.runs.StartRun(ctx, core.RunRecord{
		RunID: runID, Agent: "reporter", Channel: "web", UserID: "u-1",
		StartedAt: baselineClock(),
	}, manifest))

	_, err = env.service.Convert(ctx, ScopeRequest{
		Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
		Scope: core.ScopeExclusive, OwnerAgent: "reporter", OperationID: env.opID,
	})
	require.ErrorIs(t, err, core.ErrResourceScopeInUse)
	require.Contains(t, err.Error(), runID, "409 必须点名未释放的运行")
	require.Equal(t, core.ScopeShared, env.pointerScope(t, "alpha").Scope)

	// 运行结束 → 引用释放 → 转换通过。
	require.NoError(t, env.runs.FinishRun(ctx, runID, core.StatusCompleted, "", 1, 0,
		baselineClock().Add(0)))
	_, err = env.service.Convert(ctx, ScopeRequest{
		Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
		Scope: core.ScopeExclusive, OwnerAgent: "reporter", OperationID: env.opID,
	})
	require.NoError(t, err)
}

// TestScopeConversion_BlockedByRoute —— 消息路由端口报出的接入点同样阻断；端口报错
// 按失败关闭拒绝（"查不到"不等于"没有引用"）。
func TestScopeConversion_BlockedByRoute(t *testing.T) {
	ctx := context.Background()
	routes := &stubRoutePort{routes: []string{"route:inbox"}}
	env := newScopeEnv(t, routes)
	version := env.publishModelVersion(t, "alpha")

	_, err := env.service.Convert(ctx, ScopeRequest{
		Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
		Scope: core.ScopeExclusive, OwnerAgent: "reporter", OperationID: env.opID,
	})
	require.ErrorIs(t, err, core.ErrResourceScopeInUse)
	require.Contains(t, err.Error(), "route:inbox")
	require.Equal(t, core.ScopeShared, env.pointerScope(t, "alpha").Scope)

	// 端口故障 → 转换被拒（fail-closed），不是"当作没有引用"。
	routes.routes = nil
	routes.err = errors.New("路由端口不可用")
	_, err = env.service.Convert(ctx, ScopeRequest{
		Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
		Scope: core.ScopeExclusive, OwnerAgent: "reporter", OperationID: env.opID,
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, core.ErrResourceScopeInUse)
	require.Equal(t, core.ScopeShared, env.pointerScope(t, "alpha").Scope)
}

// TestScopeConversion_AppliesAtomically —— 两个方向都生效、身份与属主校验逐条拒绝、
// 通知渠道走同一套语义。所有拒绝路径都零写入。
func TestScopeConversion_AppliesAtomically(t *testing.T) {
	ctx := context.Background()
	env := newScopeEnv(t, nil)
	version := env.publishModelVersion(t, "alpha")

	t.Run("共享转专属再转回共享", func(t *testing.T) {
		result, err := env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
			Scope: core.ScopeExclusive, OwnerAgent: "ops-bot", OperationID: env.opID,
		})
		require.NoError(t, err)
		require.Equal(t, core.ScopeExclusive, result.Scope)
		require.Equal(t, "ops-bot", result.OwnerAgent, "停用的 Agent 可以作属主")
		require.NotZero(t, result.Revision)

		back, err := env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
			Scope: core.ScopeShared, OperationID: env.opID,
		})
		require.NoError(t, err)
		require.Equal(t, core.ScopeShared, back.Scope)
		ptr := env.pointerScope(t, "alpha")
		require.Equal(t, core.ScopeShared, ptr.Scope)
		require.Empty(t, ptr.OwnerAgentUID, "转回共享必须清空属主")
	})

	t.Run("按名不按 UID 被拒", func(t *testing.T) {
		before := env.pointerScope(t, "alpha")
		_, err := env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha",
			Scope: core.ScopeShared, OperationID: env.opID,
		})
		require.Error(t, err, "没有 resource_uid 的请求不得执行")
		require.Contains(t, err.Error(), "resource_uid")
		require.Equal(t, before, env.pointerScope(t, "alpha"))
	})

	t.Run("UID 与当前指针不一致", func(t *testing.T) {
		_, err := env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: bindingOtherUID,
			Scope: core.ScopeShared, OperationID: env.opID,
		})
		require.ErrorIs(t, err, core.ErrResourceUIDMismatch)
	})

	t.Run("归档 Agent 不能作属主", func(t *testing.T) {
		_, err := env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
			Scope: core.ScopeExclusive, OwnerAgent: "retired", OperationID: env.opID,
		})
		require.ErrorIs(t, err, core.ErrConflict)
		require.Equal(t, core.ScopeShared, env.pointerScope(t, "alpha").Scope)
	})

	t.Run("不存在的 Agent 不能作属主", func(t *testing.T) {
		_, err := env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
			Scope: core.ScopeExclusive, OwnerAgent: "nobody", OperationID: env.opID,
		})
		require.ErrorIs(t, err, core.ErrResourceNotFound)
	})

	t.Run("专属必须带属主 / 共享不得带属主", func(t *testing.T) {
		_, err := env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
			Scope: core.ScopeExclusive, OperationID: env.opID,
		})
		require.Error(t, err)
		_, err = env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
			Scope: core.ScopeShared, OwnerAgent: "reporter", OperationID: env.opID,
		})
		require.Error(t, err)
	})

	t.Run("通知渠道走同一套语义", func(t *testing.T) {
		notifyUID := env.seedNotifyChannel(t, "ops")
		result, err := env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceNotifyChannel, Name: "ops", ResourceUID: notifyUID,
			Scope: core.ScopeExclusive, OwnerAgent: "reporter", OperationID: env.opID,
		})
		require.NoError(t, err)
		require.Equal(t, core.ScopeExclusive, result.Scope)

		channel, err := env.notify.Get(ctx, "ops")
		require.NoError(t, err)
		require.Equal(t, core.ScopeExclusive, channel.Scope)
		require.Equal(t, bindingAgentUID, channel.OwnerAgentUID)

		// 别的 Agent 绑着这条渠道 → 转回共享没问题（放开），但再转专属给 reporter 会被挡。
		require.NoError(t, env.bindings.Rebuild(ctx, "ag_00000000000000000000000009",
			"intruder", "hash", core.ResourceBindings{
				NotifyChannels: []core.ResourceBinding{{Name: "ops", UID: notifyUID}},
			}, baselineClock()))
		_, err = env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceNotifyChannel, Name: "ops", ResourceUID: notifyUID,
			Scope: core.ScopeShared, OperationID: env.opID,
		})
		require.NoError(t, err, "专属转共享永远放行")
		_, err = env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceNotifyChannel, Name: "ops", ResourceUID: notifyUID,
			Scope: core.ScopeExclusive, OwnerAgent: "reporter", OperationID: env.opID,
		})
		require.ErrorIs(t, err, core.ErrResourceScopeInUse)
	})

	t.Run("不支持的资源类别", func(t *testing.T) {
		_, err := env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceSkill, Name: "weekly", ResourceUID: version.ResourceUID,
			Scope: core.ScopeShared, OperationID: env.opID,
		})
		require.Error(t, err)
	})
}

// TestScopeConversion_Concurrent —— 并发转换同一资源：每次成功都恰好 +1 修订，
// 最终范围是最后一次写入，不出现半写或丢修订。
func TestScopeConversion_Concurrent(t *testing.T) {
	ctx := context.Background()
	env := newScopeEnv(t, nil)
	version := env.publishModelVersion(t, "alpha")
	start := env.pointerScope(t, "alpha")

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := ScopeRequest{
				Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
				Scope: core.ScopeShared, OperationID: env.opID,
			}
			if i%2 == 0 {
				req.Scope = core.ScopeExclusive
				req.OwnerAgent = "reporter"
			}
			if _, err := env.service.Convert(ctx, req); err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	final := env.pointerScope(t, "alpha")
	require.Equal(t, start.Revision+workers, final.Revision, "每次成功转换恰好一次修订")
	require.Contains(t, []core.ResourceScope{core.ScopeShared, core.ScopeExclusive}, final.Scope)
	if final.Scope == core.ScopeShared {
		require.Empty(t, final.OwnerAgentUID)
	} else {
		require.Equal(t, bindingAgentUID, final.OwnerAgentUID)
	}
}

// TestScopeConversion_BlockedByDefinitionScan —— R4.4 ②：绑定投影之外，`AGENT.md`
// 正文整词提到资源名同样阻断收缩。投影在 `projection_error` / 归档身份缺失 / 写后
// 重建失败时可能是空或陈旧的，正文扫描不依赖投影，因此**无条件**执行：
//   - 另一个活动 Agent 只在正文里提到（没有绑定行）也点名阻断；
//   - 解析不了 frontmatter 的文档照样参与（fail-closed）；
//   - 归档文档以 `archive:<id>` 点名；`alphax` 这种词内片段不算引用；
//   - 属主自己的活动定义不是阻断项；
//   - 扫描失败 / 缺工作区根 → 拒绝转换（不是"当作没有引用"）。
//
// TestScopeConversion_BlockedByDefinitionScan pins the unconditional whole-word scan:
// the projection is a derived view, so the file bodies always get the last word.
func TestScopeConversion_BlockedByDefinitionScan(t *testing.T) {
	ctx := context.Background()

	convert := func(env *scopeEnv, uid core.ResourceUID, owner string) error {
		_, err := env.service.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: uid,
			Scope: core.ScopeExclusive, OwnerAgent: owner, OperationID: env.opID,
		})
		return err
	}

	t.Run("活动文档整词命中", func(t *testing.T) {
		env := newScopeEnv(t, nil)
		version := env.publishModelVersion(t, "alpha")
		// ops-bot 没有绑定行：唯一证据是正文里的整词引用。
		writeWorkspaceFile(t, env.root, "agents/ops-bot/AGENT.md",
			"---\nname: ops-bot\n---\n\n每天把 alpha 的日报发给我。\n")

		before := env.pointerScope(t, "alpha")
		err := convert(env, version.ResourceUID, "reporter")
		require.ErrorIs(t, err, core.ErrResourceScopeInUse)
		require.Contains(t, err.Error(), "ops-bot", "409 必须点名正文引用者")
		require.Contains(t, err.Error(), "定义文件的引用")

		after := env.pointerScope(t, "alpha")
		require.Equal(t, before.Scope, after.Scope, "被拒绝的转换必须零写入")
		require.Equal(t, before.Revision, after.Revision)

		// 删掉引用后同一请求通过：阻断项是文档而不是名称本身。
		require.NoError(t, os.Remove(filepath.Join(env.root, "agents", "ops-bot", "AGENT.md")))
		require.NoError(t, convert(env, version.ResourceUID, "reporter"))
		require.Equal(t, core.ScopeExclusive, env.pointerScope(t, "alpha").Scope)
	})

	t.Run("解析失败的文档照样参与", func(t *testing.T) {
		env := newScopeEnv(t, nil)
		version := env.publishModelVersion(t, "alpha")
		// frontmatter 故意写坏：扫描不依赖解析，漏扫会让仍被引用的资源被静默收缩。
		writeWorkspaceFile(t, env.root, "agents/broken/AGENT.md",
			"---\nname: [unclosed\n---\n\n引用 alpha 的正文。\n")

		err := convert(env, version.ResourceUID, "reporter")
		require.ErrorIs(t, err, core.ErrResourceScopeInUse)
		require.Contains(t, err.Error(), "broken")
	})

	t.Run("归档文档与整词边界", func(t *testing.T) {
		env := newScopeEnv(t, nil)
		version := env.publishModelVersion(t, "alpha")
		writeWorkspaceFile(t, env.root, "archive/reporter-20250101-000000/AGENT.md",
			"---\nname: reporter\n---\n\n只引用 alphax 与 alphas 这两个名字。\n")
		require.NoError(t, convert(env, version.ResourceUID, "reporter"),
			"词内片段不是整词引用，不得阻断")

		writeWorkspaceFile(t, env.root, "archive/reporter-20250101-000000/AGENT.md",
			"---\nname: reporter\n---\n\n引用 alpha。\n")
		err := convert(env, version.ResourceUID, "reporter")
		require.ErrorIs(t, err, core.ErrResourceScopeInUse)
		require.Contains(t, err.Error(), "archive:reporter-20250101-000000",
			"归档目录沿用扫描给出的标识，不编造 Agent 名")
	})

	t.Run("属主自己的活动定义不阻断", func(t *testing.T) {
		env := newScopeEnv(t, nil)
		version := env.publishModelVersion(t, "alpha")
		writeWorkspaceFile(t, env.root, "agents/reporter/AGENT.md",
			"---\nname: reporter\n---\n\n我自己用 alpha。\n")

		require.NoError(t, convert(env, version.ResourceUID, "reporter"),
			"转换完成后属主仍是合法引用者")

		// 同一个名字的归档文档照算：归档身份不可信时宁可多挡一次。
		writeWorkspaceFile(t, env.root, "archive/reporter-20250102-000000/AGENT.md",
			"---\nname: reporter\n---\n\n我自己用 alpha。\n")
		err := convert(env, version.ResourceUID, "ops-bot")
		require.ErrorIs(t, err, core.ErrResourceScopeInUse)
		require.Contains(t, err.Error(), "archive:reporter-20250102-000000")
	})

	t.Run("扫描失败与缺根都失败关闭", func(t *testing.T) {
		env := newScopeEnv(t, nil)
		version := env.publishModelVersion(t, "alpha")

		broken := *env.service
		broken.WorkspaceRoot = filepath.Join(env.root, "no-such-root")
		_, err := broken.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
			Scope: core.ScopeExclusive, OwnerAgent: "reporter", OperationID: env.opID,
		})
		require.Error(t, err)
		require.NotErrorIs(t, err, core.ErrResourceScopeInUse, "扫描失败不是「没有引用」")
		require.Equal(t, core.ScopeShared, env.pointerScope(t, "alpha").Scope)

		noRoot := *env.service
		noRoot.WorkspaceRoot = ""
		_, err = noRoot.Convert(ctx, ScopeRequest{
			Kind: core.ResourceModelConnection, Name: "alpha", ResourceUID: version.ResourceUID,
			Scope: core.ScopeExclusive, OwnerAgent: "reporter", OperationID: env.opID,
		})
		require.Error(t, err, "证明不了没人引用时不得收缩")
		require.Equal(t, core.ScopeShared, env.pointerScope(t, "alpha").Scope)
	})
}
