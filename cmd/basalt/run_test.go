package main

import (
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// captureStdout 把标准输出换成管道，跑完 fn 后取回内容。
// 入口包必须可测（覆盖率判据不豁免入口包），而 run 把结果写到进程标准输出——
// 于是这里换掉 os.Stdout：app.Run 在执行时经 root.SetOut(os.Stdout) 取用，
// 换在调用前即可拦住输出。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = original })

	fn()

	require.NoError(t, writer.Close())
	out, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	return string(out)
}

// TestEntryRun_VersionFlag 验收点（文档用例名同名）：--version 退出码 0，且输出
// **包含**构建期注入值（不锁死 cobra 的输出格式——格式随版本变，值不变）。
func TestEntryRun_VersionFlag(t *testing.T) {
	original := version
	version = "v0.0.0-inject-probe"
	t.Cleanup(func() { version = original })

	var code int
	out := captureStdout(t, func() { code = run([]string{"--version"}) })

	require.Equal(t, 0, code, "--version 必须成功退出")
	require.Contains(t, out, "v0.0.0-inject-probe", "输出必须包含注入的版本值")
}

// TestEntryRun_UnknownCommand 验收点：未知命令必须非零退出（失败不静默）。
func TestEntryRun_UnknownCommand(t *testing.T) {
	var code int
	captureStdout(t, func() { code = run([]string{"no-such-command"}) })

	require.NotEqual(t, 0, code, "未知命令必须非零退出")
}
