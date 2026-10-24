package app

// 本文件是 US-7 装配面凭证解析的关键回归（颗粒度文档 §3.2 的 app/mcp_test.go 一例）。
// 包内测试：直接调用非导出 `resolveMCPEnv`——测试缝纪律要求不为测试新增导出 API
// （app 层经既有包内字段与函数判卷，与 `mcpConnect` 同一先例）。
// An in-package test on purpose: resolveMCPEnv stays unexported, so the assembly-layer
// seam is exercised without adding a test-only exported API.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// R1b —— 装配期解析：未设置 / 空值同档拒绝；错误点名 server 与变量，不进入值。
// 逆改点：把 !ok 改成 value := os.Getenv(name) 继续装配
//
//	→ 两组 require.Error 都红（空凭证被送给 server）。
func TestResolveMCPEnv_MissingOrEmptyFailsClosed(t *testing.T) {
	specs := []core.MCPServerConfig{{
		Name: "gh", Command: "gh-mcp",
		Env: map[string]string{"GITHUB_TOKEN": "${GH_TOKEN}"},
	}}
	_, err := resolveMCPEnv(specs, func(string) (string, bool) { return "", false })
	require.Error(t, err)
	require.Contains(t, err.Error(), "gh")
	require.Contains(t, err.Error(), "GH_TOKEN")

	_, err = resolveMCPEnv(specs, func(string) (string, bool) { return "", true })
	require.Error(t, err, "空值与未设置同档（空凭证不可用）")

	resolved, err := resolveMCPEnv(specs, func(string) (string, bool) { return "tok-1", true })
	require.NoError(t, err)
	require.Equal(t, "tok-1", resolved[0].Env["GITHUB_TOKEN"])
}

// TestMCPRuntime_ConnectedNamesFollowSupervisor：句柄的连接名单实时取自 supervisor——
// 启停一发生，名单立刻变化（不是启动时的静态快照）。
// Connected names are read live from the supervisor, so start/stop is visible at once.
func TestMCPRuntime_ConnectedNamesFollowSupervisor(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	specs := []core.MCPServerConfig{
		{Name: "alpha", Transport: "stdio", Command: "allowed-mcp"},
		{Name: "beta", Transport: "stdio", Command: "allowed-mcp"},
	}
	runtime := newMCPRuntimeFromSupervisor(env.supervisor, specs)
	ctx := context.Background()

	require.Empty(t, runtime.ConnectedNames())
	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("beta", 1)))
	require.Equal(t, []string{"beta"}, runtime.ConnectedNames(), "名单按文件序、取实时状态")

	require.NoError(t, env.supervisor.Stop(ctx, "beta"))
	require.Empty(t, runtime.ConnectedNames())
}

// TestMCPRuntime_ToolsFromSupervisorSnapshot：工具名取连接顺序的 supervisor 快照；
// 未连接或未知 server 返回空列表（非 nil，视图序列化为 `[]`）。
// Tools come from the supervisor snapshot in connection order; disconnected servers
// yield an empty, non-nil list.
func TestMCPRuntime_ToolsFromSupervisorSnapshot(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	runtime := newMCPRuntimeFromSupervisor(env.supervisor, []core.MCPServerConfig{
		{Name: "alpha", Transport: "stdio", Command: "allowed-mcp"},
	})
	env.setToolNames("search_docs", "fetch_page")

	require.NotNil(t, runtime.Tools("alpha"))
	require.Empty(t, runtime.Tools("alpha"))
	require.NotNil(t, runtime.Tools("unknown"))

	require.NoError(t, env.supervisor.Start(context.Background(), allowedStdio("alpha", 1)))
	require.Equal(t, []string{"search_docs", "fetch_page"}, runtime.Tools("alpha"))
}

// TestMCPRuntime_AppliedVersionAfterConnect：applied 只在连接成功后可从句柄读出；
// 连接失败保留旧值（新版本不写 applied）。
// Applied is visible only after a successful connect and survives a failed upgrade.
func TestMCPRuntime_AppliedVersionAfterConnect(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	runtime := newMCPRuntimeFromSupervisor(env.supervisor, []core.MCPServerConfig{
		{Name: "alpha", Transport: "stdio", Command: "allowed-mcp"},
	})
	ctx := context.Background()

	_, applied := runtime.AppliedVersion("alpha")
	require.False(t, applied, "未连接不得报告 applied")

	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("alpha", 3)))
	versionID, applied := runtime.AppliedVersion("alpha")
	require.True(t, applied)
	require.Equal(t, int64(3), versionID)

	env.setConnectErr(errors.New("boom"))
	require.Error(t, env.supervisor.Start(ctx, allowedStdio("alpha", 4)))
	versionID, applied = runtime.AppliedVersion("alpha")
	require.True(t, applied)
	require.Equal(t, int64(3), versionID, "连接失败保留旧 applied")
}

// TestMCPRuntime_PendingRestart：待重启 = 发布版本非零且 ≠ 已应用版本；无发布版本
// 不算待重启（视图无版本可展示）。
// PendingRestart is published != applied, and false when nothing is published.
func TestMCPRuntime_PendingRestart(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	runtime := newMCPRuntimeFromSupervisor(env.supervisor, []core.MCPServerConfig{
		{Name: "alpha", Transport: "stdio", Command: "allowed-mcp"},
	})
	ctx := context.Background()

	require.False(t, runtime.PendingRestart("alpha", 0), "无发布版本不算待重启")
	require.True(t, runtime.PendingRestart("alpha", 1), "尚未应用 → 待重启")

	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("alpha", 1)))
	require.False(t, runtime.PendingRestart("alpha", 1), "发布 = 应用 → 不待重启")
	require.True(t, runtime.PendingRestart("alpha", 2), "新发布尚未应用 → 待重启")
}
