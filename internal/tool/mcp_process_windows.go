//go:build windows

package tool

// 本文件是 MCP stdio 子进程的 Windows 平台半边：整树终止用 `taskkill /T /F`。与
// `runner_windows.go` 的 Job 对象不同，MCP 子进程由 SDK 的 `CommandTransport` 在内部
// 启动（拿不到"创建即挂 Job"的窗口），故按颗粒度文档 §3.1.7 的选择走 taskkill。
// Windows half of MCP stdio tree cleanup: taskkill /T, because the SDK owns process
// creation and there is no create-suspended window to attach a job object.

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// configureMCPChildProcess 在 Windows 上是空操作：没有进程组可预设，整树关系由
// taskkill /T 在系统快照上遍历。
// configureMCPChildProcess is a no-op on Windows; taskkill walks the tree instead.
func configureMCPChildProcess(*exec.Cmd) {}

// killMCPProcessTree 终止一个 stdio 子进程及其全部后代。进程已被 Wait 收尸时是空操作；
// 否则 `taskkill /T` 从该 PID 向下遍历——因此它**必须**在父进程仍存活时调用。
// killMCPProcessTree terminates the child and all descendants; it only works while the
// parent is still alive, so callers run it before closing the session.
func killMCPProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return nil
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	// #nosec G204 -- 豁免理由：taskkill 是常量可执行名，唯一变量是 int 形状的 PID（来自
	// os.Process，不经 shell、不经配置文本）；G204 只接受常量实参，PID 必然是变量。
	output, err := exec.Command("taskkill", "/T", "/F", "/PID", pid).CombinedOutput()
	if err != nil {
		// 竞态：判定之后、taskkill 之前进程自己退了（或树已空）——没有可清理的对象。
		if cmd.ProcessState != nil {
			return nil
		}
		return fmt.Errorf("终止 MCP 子进程树 %s：%w（%s）", pid, err, strings.TrimSpace(string(output)))
	}
	return nil
}
