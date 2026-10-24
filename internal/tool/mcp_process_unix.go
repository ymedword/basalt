//go:build !windows

package tool

// 本文件是 MCP stdio 子进程的 Unix 平台半边：子进程在独立进程组内启动，停止时对
// **进程组**发 SIGKILL——只杀直接子进程会把它的后代留给系统（约束 19 / 22）。
// Unix half of MCP stdio tree cleanup: the child starts in its own process group and
// stopping kills the whole group, not just the direct child.

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// configureMCPChildProcess 在启动前把子进程放进独立进程组（pgid = 自身 PID），使
// `kill(-pid)` 覆盖它的全部后代。**必须**在 Start 之前设置。
// configureMCPChildProcess puts the child in its own process group before Start.
func configureMCPChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killMCPProcessTree 对整个进程组发 SIGKILL；组已不存在（ESRCH）按"没有可清理的对象"
// 放行——进程组在组长退出后仍对其存活成员有效，因此这一调用不依赖父进程存活。
// killMCPProcessTree SIGKILLs the whole group and tolerates an already-empty group.
func killMCPProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("终止 MCP 子进程组 %d：%w", pid, err)
	}
	return nil
}
