package app

// 本文件判卷「收紧域名白名单后撤销已建立的 MCP http 连接」（US-12 R8.2.3 / R8.2.4）：
// 只碰当前确有 http 连接的槽位、关闭连接并清空工具名、**保留**已应用版本、返回被撤销
// 的 server 名；stdio 与其他 server 不受影响；放宽白名单**不**自动重连。
// Revocation acceptance: only live http links are closed, applied versions stay, stdio is
// untouched, and loosening never reconnects.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// allowedHTTP 构造一个策略可放行的 http 版本（白名单夹具放行 mcp.example.com）。
func allowedHTTP(name string, versionID int64) core.PublishedMCPServer {
	return core.PublishedMCPServer{VersionID: versionID, Config: core.MCPServerConfig{
		Name: name, Transport: "http", URL: "https://mcp.example.com/mcp",
	}}
}

// TestMCPSupervisor_RevokeDisallowedHTTPClosesLink —— 收紧后连接被关闭、工具名清空、
// 状态短语是固定撤销短语、已应用版本**保留**（版本事实不变）；stdio 槽位不受影响。
// 逆改点：撤销时顺手清 applied / 误伤 stdio → 对应断言红。
func TestMCPSupervisor_RevokeDisallowedHTTPClosesLink(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	ctx := context.Background()
	env.setToolNames("search")
	require.NoError(t, env.supervisor.Start(ctx, allowedHTTP("remote", 7)))
	require.NoError(t, env.supervisor.Start(ctx, allowedStdio("fs", 3)))
	require.Equal(t, 2, env.createdCount())

	revoked := env.supervisor.RevokeDisallowedHTTP(func(host string) bool {
		return host != "mcp.example.com"
	})
	require.Equal(t, []string{"remote"}, revoked)
	require.Equal(t, 1, env.closedCount(), "只关闭被撤销的那一个连接器")

	remote, ok := env.supervisor.Status("remote")
	require.True(t, ok)
	require.False(t, remote.Connected)
	require.Empty(t, remote.Tools, "撤销后不得再暴露旧工具名")
	require.Equal(t, mcpStatusAllowlistRevoked, remote.Error)
	require.Equal(t, int64(7), remote.AppliedVersionID, "已应用版本是版本事实，撤销不得清除")
	version, applied := env.appliedVersion("remote")
	require.True(t, applied)
	require.Equal(t, int64(7), version)
	require.True(t, env.connectors[0].isClosed(), "被撤销的连接器必须真正关闭")

	fs, _ := env.supervisor.Status("fs")
	require.True(t, fs.Connected, "stdio 不由域名白名单管辖")
	require.Equal(t, "", fs.Error)
	require.True(t, env.startedProcess(), "stdio 的进程仍在（未被误停）")
}

// TestMCPSupervisor_RevokeKeepsAllowedHosts —— 谓词放行的 host 不撤销：返回空列表、
// 零关闭、状态保持已连接。
// 逆改点：撤销按"是否存在连接"而不是按 host 判定 → 本用例红。
func TestMCPSupervisor_RevokeKeepsAllowedHosts(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	require.NoError(t, env.supervisor.Start(context.Background(), allowedHTTP("remote", 1)))

	revoked := env.supervisor.RevokeDisallowedHTTP(func(host string) bool { return true })
	require.Empty(t, revoked)
	require.Zero(t, env.closedCount())

	status, _ := env.supervisor.Status("remote")
	require.True(t, status.Connected)
	require.Empty(t, status.Error)
}

// TestMCPSupervisor_LooseningDoesNotReconnect —— 撤销之后再放宽：**不**自动重连，
// 也不悄悄恢复工具名；重新连接要等重启或重新发布（如实边界）。
// 逆改点：撤销路径改成"仅标记、连接还在"或放宽时自动重连 → 对应断言红。
func TestMCPSupervisor_LooseningDoesNotReconnect(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	require.NoError(t, env.supervisor.Start(context.Background(), allowedHTTP("remote", 2)))
	require.Equal(t, []string{"remote"}, env.supervisor.RevokeDisallowedHTTP(func(string) bool {
		return false
	}))
	created := env.createdCount()

	revoked := env.supervisor.RevokeDisallowedHTTP(func(string) bool { return true })
	require.Empty(t, revoked, "放宽不是撤销动作")
	require.Equal(t, created, env.createdCount(), "放宽不得触发新连接")
	status, _ := env.supervisor.Status("remote")
	require.False(t, status.Connected, "仍为未连接，等待重启或重新发布")
	require.Equal(t, mcpStatusAllowlistRevoked, status.Error)
}

// TestMCPSupervisor_RevokeIdempotentAndEmpty —— 重复撤销是 no-op（第二次返回空、不重复
// 关闭）；状态表为空时直接返回空列表，不 panic。
// 逆改点：已断开的槽位被再次 Close → 关闭计数翻倍，本用例红。
func TestMCPSupervisor_RevokeIdempotentAndEmpty(t *testing.T) {
	t.Parallel()
	env := newMCPEnv(t)
	require.Empty(t, env.supervisor.RevokeDisallowedHTTP(func(string) bool { return false }),
		"空状态表必须是 no-op")

	require.NoError(t, env.supervisor.Start(context.Background(), allowedHTTP("remote", 1)))
	require.Len(t, env.supervisor.RevokeDisallowedHTTP(func(string) bool { return false }), 1)
	closes := env.closedCount()
	require.Empty(t, env.supervisor.RevokeDisallowedHTTP(func(string) bool { return false }))
	require.Equal(t, closes, env.closedCount(), "已撤销的连接不得重复关闭")
}
