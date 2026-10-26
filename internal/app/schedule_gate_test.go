package app

// 本文件是 14D / E5「启用才运行」在装配层的判卷：lifecycleScheduleGate 把生命周期
// 状态文件接成调度门，读不到 / 校验不过 / 文件不存在一律失败关闭（false），只有
// active 放行。门不缓存状态，停用后下一次触发即被拦住。
// The gate adapter reads the lifecycle file on every call and fails closed.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// TestLifecycleScheduleGate_OnlyActiveAdmitsTicks 覆盖门的四种读数：active 放行、
// disabled 拒绝、状态文件损坏拒绝、Agent 从未初始化拒绝。
// 逆改点：把 `exists` 缺失读成 active，或吞掉 err 当放行 → 本用例红。
// Only an active, readable state admits a scheduled tick.
func TestLifecycleScheduleGate_OnlyActiveAdmitsTicks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	now := func() time.Time { return time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC) }
	lifecycle := workspace.NewLifecycleStore(root, now)
	gate := lifecycleScheduleGate(lifecycle)
	require.NotNil(t, gate)

	require.False(t, gate(ctx, "never-bootstrapped"), "没有状态文件不得放行")

	// 新建即停用（14D / E1）：Bootstrap 落 disabled，门必须立即拒绝。
	rec, err := lifecycle.Bootstrap(ctx, "writer", "op-gate-1")
	require.NoError(t, err)
	require.Equal(t, core.AgentDisabled, rec.State, "Bootstrap 后是 disabled")
	require.False(t, gate(ctx, "writer"), "刚创建的 Agent 不得被排期")

	rec, err = lifecycle.Transition(ctx, "writer", core.AgentDisabled, rec.Generation,
		core.AgentActive, "op-gate-2")
	require.NoError(t, err)
	require.True(t, gate(ctx, "writer"), "active 必须放行")

	rec, err = lifecycle.Transition(ctx, "writer", core.AgentActive, rec.Generation,
		core.AgentDisabled, "op-gate-3")
	require.NoError(t, err)
	require.False(t, gate(ctx, "writer"), "disabled 必须拒绝")

	// 文件损坏：读失败同样拒绝（不得把损坏状态读成 active）。
	corrupt := filepath.Join(root, "control", "agents", "broken.json")
	require.NoError(t, os.WriteFile(corrupt, []byte("{not json"), 0o600))
	require.False(t, gate(ctx, "broken"), "校验失败必须失败关闭")

	// 非法 Agent 名直接拒绝，不落到文件系统。
	require.False(t, gate(ctx, "../escape"), "非法目录名必须拒绝")
}

// TestLifecycleScheduleGate_NilStoreDisablesGate 钉住装配约定：没有生命周期存储时
// 返回 nil 门（不设门），而不是一个永远拒绝的门——CLI 写路径没有调度器，重路径
// 一定会传非 nil 存储。
// A nil store yields a nil gate rather than an always-false one.
func TestLifecycleScheduleGate_NilStoreDisablesGate(t *testing.T) {
	require.Nil(t, lifecycleScheduleGate(nil))
}
