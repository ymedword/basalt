//go:build windows

package tool

// 本文件是 MCP stdio 子进程的 Windows 平台半边：整树终止用 `taskkill /T /F`。与
// `runner_windows.go` 的 Job 对象不同，MCP 子进程由 SDK 的 `CommandTransport` 在内部
// 启动（拿不到"创建即挂 Job"的窗口），故按颗粒度文档 §3.1.7 的选择走 taskkill。
// Windows half of MCP stdio tree cleanup: taskkill /T, because the SDK owns process
// creation and there is no create-suspended window to attach a job object.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// configureMCPChildProcess 在 Windows 上是空操作：没有进程组可预设，整树关系由
// taskkill /T 在系统快照上遍历。
// configureMCPChildProcess is a no-op on Windows; taskkill walks the tree instead.
func configureMCPChildProcess(*exec.Cmd) {}

// processReaped 用信号 0 探测进程是否已被回收。**Windows 与 Unix 的形状不同**：Windows 的
// `Process.signal` 根本不处理信号 0——`handleTransientAcquire` 拿到 `statusReleased` 时返回
// `syscall.EINVAL`，其余一律落到 `syscall.EWINDOWS`（`statusDone` 在 Windows 上不可达，只在
// Unix / pidfd 侧出现，故下面那个 `os.ErrProcessDone` 分支纯属防御）。handle 型进程的
// `wait()` 结束时置的正是 `statusReleased`（不是 `statusDone`，为兼容二次 Wait），因此 EINVAL
// 的准确含义是「`Process.Wait` 已过回收点，或 `Process.Release` 被调用」；本仓与 go-sdk v1.8.0
// 的 mcp 包都只调 `Cmd.Wait`、从不调 `Release`，故在本路径上等价于"已收尸"。
// **不得**照抄 Unix 写法：只认 `os.ErrProcessDone` 会让本判断恒为 false，清理路径于是对已收尸的
// PID 白跑一次 `taskkill`，再把它当成真实失败上报。
// 两点边界：(1) 探测点在 `Process.Wait` 内部就翻真，比 `exec.Cmd.ProcessState` 被赋值更早，
// 方向是安全的（提前跳过注定失败的 taskkill），但与旧判定不是逐字等价；(2) 已退出但尚未 Wait
// 的进程仍读作"未收尸"——本函数回答的是"是否已被回收"，**不是**"进程是否还活着"。
// 实测（windows/amd64, go1.27.3）：未收尸 → "not supported by windows"；Wait 之后 → EINVAL，
// 可重复。判据由 TestMCPProcess_ProcessDone_AfterWait 钉住，改动前先跑它。
// processReaped probes with signal 0. On Windows Process.signal never handles signal 0:
// statusReleased maps to EINVAL and everything else falls through to EWINDOWS, and a
// handle-backed process ends wait() in statusReleased rather than statusDone. It flips a
// little before exec.Cmd.ProcessState is assigned and answers "already reaped", not
// "still running".
func processReaped(p *os.Process) bool {
	err := p.Signal(syscall.Signal(0))
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, os.ErrProcessDone)
}

// killMCPProcessTree 终止一个 stdio 子进程及其全部后代。进程已被 Wait 收尸时是空操作；
// 否则 `taskkill /T` 从该 PID 向下遍历——因此它**必须**在父进程仍存活时调用。
// killMCPProcessTree terminates the child and all descendants; it only works while the
// parent is still alive, so callers run it before closing the session.
func killMCPProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || processDone(cmd) {
		return nil
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	// #nosec G204 -- 豁免理由：taskkill 是常量可执行名，唯一变量是 int 形状的 PID（来自
	// os.Process，不经 shell、不经配置文本）；G204 只接受常量实参，PID 必然是变量。
	output, err := exec.Command("taskkill", "/T", "/F", "/PID", pid).CombinedOutput()
	if err != nil {
		// 竞态：判定之后、taskkill 之前进程自己退了（或树已空）——没有可清理的对象。
		if processDone(cmd) {
			return nil
		}
		return fmt.Errorf("终止 MCP 子进程树 %s：%w（%s）", pid, err, strings.TrimSpace(string(output)))
	}
	return nil
}
