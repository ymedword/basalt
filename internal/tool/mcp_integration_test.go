//go:build integration

package tool_test

// 本文件是 US-7 的集成层（颗粒度文档 §3.2 / §4.3）：本仓**第一个起真实子进程**的协议测试。
// 实现 = 测试二进制自举——父进程用 `os.Args[0] -test.run=^TestMCPHelperProcess$` 拉起自己，
// 子进程在 `TestMCPHelperProcess` 里调 `testutil.RunMCPHelper`，以 stdio 传输服务标准测试
// 工具集。command 只按空白切分（注 10），因此**测试二进制路径含空白时显式跳过并写明理由**。
// This file is the integration layer: the first suite that starts a real subprocess, by
// re-executing the test binary itself as the MCP stdio server.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/testutil"
	"github.com/ymedword/basalt/internal/tool"
)

// helperServerName 是集成层 server 的固定名（断言与配置共用一处）。
// helperServerName is the fixed server name used by the integration layer.
const helperServerName = "helper"

// TestMCPHelperProcess 是子进程入口：设置 `BASALT_MCP_HELPER` 时以 stdio 服务标准工具集，
// 未设置时**跳过**（默认层跑到它时是跳过，不是失败）。
// TestMCPHelperProcess is the subprocess entry point; it skips when the helper env is
// absent.
func TestMCPHelperProcess(t *testing.T) {
	testutil.RunMCPHelper(t)
}

// helperCommand 组出「测试二进制 + 只跑 helper 入口」的命令串；路径含空白无法用空白切分
// 表达（注 10 的已知边界），此时显式跳过。
// helperCommand builds the self-exec command; a path containing whitespace cannot be
// expressed through whitespace-only splitting, so the test skips with the reason.
func helperCommand(t *testing.T) string {
	t.Helper()
	if strings.ContainsAny(os.Args[0], " \t") {
		t.Skip("测试二进制路径含空白——command 按空白切分，无法表达")
	}
	return os.Args[0] + " -test.run=^TestMCPHelperProcess$"
}

// newHelperPool 造一只指向 helper 子进程的连接池并返回已连上的工具列表；含透传白名单
// （子进程需要 PATH 等才能启动）。调用方负责登记 Cleanup——server 中途死亡的用例对
// Close 的期望不同。
// newHelperPool builds a pool pointed at the helper subprocess and returns its tools;
// the caller owns cleanup.
func newHelperPool(t *testing.T, logs *bytes.Buffer, extraEnv map[string]string) (*tool.MCPPool, []core.Tool) {
	t.Helper()
	env := map[string]string{"BASALT_MCP_HELPER": "1"}
	for name, value := range extraEnv {
		env[name] = value
	}
	pool := tool.NewMCPPool(tool.MCPPoolOptions{
		Logger:         slog.New(slog.NewTextHandler(logs, nil)),
		Root:           t.TempDir(),
		EnvPassthrough: []string{"PATH", "SYSTEMROOT", "TEMP", "TMP", "HOME"},
		ConnectTimeout: 5 * time.Second,
		Version:        "us7-test",
	})
	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{
		Name:    helperServerName,
		Command: helperCommand(t),
		Env:     env, // 已解析形态：值直接传给子进程
	}})
	require.NoError(t, err)
	require.Len(t, conns, 1, "helper 子进程必须连上：%s", logs.String())
	return pool, conns[0].Tools
}

// findTool 从连接结果里按名取工具；缺失即失败（本文件的所有用例都以工具存在为前提）。
// findTool returns the named tool from a connection, failing when it is absent.
func findTool(t *testing.T, tools []core.Tool, name string) core.Tool {
	t.Helper()
	for _, candidate := range tools {
		if candidate.Name() == name {
			return candidate
		}
	}
	t.Fatalf("工具 %s 未出现在连接结果里", name)
	return nil
}

// callDirect 直接调用适配器：期限由 ctx 承载——适配器**不消费** `ToolEnv.Timeout`，
// 只写 ToolEnv 并不构成五秒期限（执行器才是超时归属；这里是绕过执行器的直调）。
// callDirect gives the direct adapter call a context deadline because the adapter does
// not consume ToolEnv.Timeout.
func callDirect(t *testing.T, target core.Tool, input string) (core.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return target.Execute(ctx, core.ToolEnv{Timeout: 5 * time.Second}, json.RawMessage(input))
}

// TestMCPIntegration_ConnectListAndCallOverStdio：连接 → 初始化 → 列工具 → 调 echo 拿回
// 文本——真实子进程上的最小闭环。
// A minimal end-to-end loop over a real stdio subprocess.
func TestMCPIntegration_ConnectListAndCallOverStdio(t *testing.T) {
	var logs bytes.Buffer
	pool, tools := newHelperPool(t, &logs, nil)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	echo := findTool(t, tools, "echo")
	res, err := callDirect(t, echo, `{"text":"hello-stdio"}`)
	require.NoError(t, err)
	require.Equal(t, "hello-stdio", res.Content)
}

// TestMCPIntegration_PaginationOverStdio：`BASALT_MCP_PAGE_SIZE=1` 时三件工具仍全部列出
// （游标链路在真实传输上同样成立）。
// Pagination works over the real transport too: all three tools are listed page by page.
func TestMCPIntegration_PaginationOverStdio(t *testing.T) {
	var logs bytes.Buffer
	pool := tool.NewMCPPool(tool.MCPPoolOptions{
		Logger:         slog.New(slog.NewTextHandler(&logs, nil)),
		Root:           t.TempDir(),
		EnvPassthrough: []string{"PATH", "SYSTEMROOT", "TEMP", "TMP", "HOME"},
		ConnectTimeout: 5 * time.Second,
		Version:        "us7-test",
	})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{
		Name:    helperServerName,
		Command: helperCommand(t),
		Env:     map[string]string{"BASALT_MCP_HELPER": "1", "BASALT_MCP_PAGE_SIZE": "1"},
	}})
	require.NoError(t, err)
	require.Len(t, conns, 1, "helper 子进程必须连上：%s", logs.String())
	require.Len(t, conns[0].Tools, 3, "每页 1 件时三页必须全部列出")
	names := make([]string, 0, len(conns[0].Tools))
	for _, adapter := range conns[0].Tools {
		names = append(names, adapter.Name())
	}
	require.ElementsMatch(t, []string{"echo", "boom", "blob"}, names)
}

// TestMCPIntegration_BadCommandIsSkipped（R2 真进程版）：可执行文件不存在 → Connect 无错、
// 零连接、WARN 点名 server。
// R2 over real processes: a missing executable is skipped without failing Connect.
func TestMCPIntegration_BadCommandIsSkipped(t *testing.T) {
	var logs bytes.Buffer
	pool := tool.NewMCPPool(tool.MCPPoolOptions{
		Logger:         slog.New(slog.NewTextHandler(&logs, nil)),
		Root:           t.TempDir(),
		EnvPassthrough: []string{"PATH", "SYSTEMROOT", "TEMP", "TMP", "HOME"},
		ConnectTimeout: 5 * time.Second,
		Version:        "us7-test",
	})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{
		Name: "ghost", Command: "basalt-mcp-no-such-binary",
	}})
	require.NoError(t, err, "单 server 失败不得中断启动")
	require.Empty(t, conns)
	require.Contains(t, logs.String(), "MCP server 未连接——已跳过")
	require.Contains(t, logs.String(), "server=ghost")
}

// TestMCPIntegration_ServerDiesMidCall：boom 让 server 进程退出 → 调用返回外部错误，
// **不是**超时（取消 / 截止身份优先的判序在真实断连上同样成立）。
// A dying server surfaces as an external error, never as a timeout.
func TestMCPIntegration_ServerDiesMidCall(t *testing.T) {
	var logs bytes.Buffer
	pool, tools := newHelperPool(t, &logs, nil)
	// server 已死：Close 的错误不属本用例判据，不掩盖真实结果即可。
	t.Cleanup(func() { _ = pool.Close() })

	boom := findTool(t, tools, "boom")
	_, err := callDirect(t, boom, `{}`)
	require.Error(t, err)
	require.True(t, core.IsExternalError(err), "进程死亡是外部错误")
	require.False(t, errors.Is(err, context.DeadlineExceeded), "不得误判为超时")
	require.False(t, errors.Is(err, context.Canceled), "不得误判为取消")
}

// TestMCPIntegration_CallSucceedsAfterConnectBudgetReleased（**R11**）：进程生命周期与
// 握手 / 列工具预算分离。
// 逆改点：dialCommand 改用 exec.CommandContext(握手 ctx)（或把 cctx 存进会话）
//
//	→ Connect 末尾 cancel() 即杀子进程，等待后的调用报连接关闭，红。
func TestMCPIntegration_CallSucceedsAfterConnectBudgetReleased(t *testing.T) {
	var logs bytes.Buffer
	pool := tool.NewMCPPool(tool.MCPPoolOptions{
		Logger:         slog.New(slog.NewTextHandler(&logs, nil)),
		Root:           t.TempDir(),
		EnvPassthrough: []string{"PATH", "SYSTEMROOT", "TEMP", "TMP", "HOME"},
		ConnectTimeout: 5 * time.Second,
		Version:        "us7-test",
	})
	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{
		Name: "helper", Command: helperCommand(t),
		Env: map[string]string{"BASALT_MCP_HELPER": "1"}, // 已解析形态：值直接传给子进程
	}})
	require.NoError(t, err)
	require.Len(t, conns, 1, "helper 子进程必须连上：%s", logs.String())
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	// 预算 ctx 已在 Connect 末尾 cancel()；进程若被预算绑死，这里已经死透。
	time.Sleep(300 * time.Millisecond)
	var echo core.Tool
	for _, tl := range conns[0].Tools {
		if tl.Name() == "echo" {
			echo = tl
		}
	}
	require.NotNil(t, echo)
	res, err := callDirect(t, echo, `{"text":"after-budget"}`)
	require.NoError(t, err, "Connect 返回后的调用不得因预算释放而失败")
	require.Contains(t, res.Content, "after-budget")
}
