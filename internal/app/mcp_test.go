package app

// 本文件是 US-7 装配面凭证解析的关键回归（颗粒度文档 §3.2 的 app/mcp_test.go 一例）。
// 包内测试：直接调用非导出 `resolveMCPEnv`——测试缝纪律要求不为测试新增导出 API
// （app 层经既有包内字段与函数判卷，与 `mcpConnect` 同一先例）。
// An in-package test on purpose: resolveMCPEnv stays unexported, so the assembly-layer
// seam is exercised without adding a test-only exported API.

import (
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
