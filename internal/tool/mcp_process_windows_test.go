//go:build windows

package tool

// 本文件把 Windows 上"已被 Wait 收尸"的可观测形状钉死：`processReaped` 依赖
// `os.Process.signal` 对信号 0 的错误映射，那是标准库内部细节，不是公开契约。
// 本用例在形状变化时立刻变红，避免清理路径悄悄退化成"恒判未收尸"。
// This test pins the observable shape of "already reaped" on Windows. processReaped
// relies on os.Process.signal's error mapping for signal 0, which is a standard-library
// detail rather than a public contract.

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMCPProcess_ProcessDone_AfterWait 区分"未回收"与"已回收"两种状态的可观测差异，
// 两侧形状都断言：未回收必须是 EWINDOWS，回收后必须是 EINVAL。
// 逆改点：把 processReaped 改回只认 `os.ErrProcessDone`（Unix 写法）→ 第三条断言变红，
// 因为 Windows 的 `Process.signal` 在 `statusReleased` 上返回 `syscall.EINVAL`；
// 改回读 `cmd.ProcessState` → `-race` 下与 SDK 的 `Cmd.Wait` 相撞（见 mcp.go 的注释）。
// TestMCPProcess_ProcessDone_AfterWait tells the not-yet-reaped and reaped states apart.
func TestMCPProcess_ProcessDone_AfterWait(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	require.NoError(t, cmd.Start(), "启动探针子进程失败")
	require.False(t, processDone(cmd), "未 Wait 的进程不得被判为已回收")
	require.True(t, errors.Is(cmd.Process.Signal(syscall.Signal(0)), syscall.EWINDOWS),
		"未回收进程的 Signal(0) 应返回 EWINDOWS；拿到别的值说明 Windows 的错误映射变了")

	require.NoError(t, cmd.Wait())
	require.True(t, processDone(cmd),
		"Wait 之后必须判为已回收：Windows 的 Process.signal 对已释放句柄返回 syscall.EINVAL，"+
			"该映射变了就得重新核实 processReaped")
}
