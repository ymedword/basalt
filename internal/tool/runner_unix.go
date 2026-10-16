//go:build unix

package tool

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// 本文件是受控执行器的 Unix 平台半边：进程组随 exec **原子生效**（Setpgid），子进程从
// 第一条指令起就在自己的组里，故不需要 Windows 侧那套"先挂起、再关联"的顺序——没有
// 关联窗口要消除。代价是终止面只有信号：一次 `kill(-pgid)` 覆盖全组，与 Windows 的
// TerminateJobObject 同义（都终止已派生的后代，而不是单杀一个进程）。
// Unix half of the runner: the process group takes effect atomically at exec, so there
// is no association window; one signal to the group covers every descendant.

// unixProcess 是受进程组约束的子进程句柄：组长 pid 即子进程 pid（Setpgid 让它自组）。
// unixProcess holds the group leader pid; the process group is the termination surface.
type unixProcess struct {
	cmd *exec.Cmd
	pid int
}

// 编译期断言：平台实现满足主流程依赖的契约面。
var _ guardedProcess = (*unixProcess)(nil)

// spawnGuarded 起一个自组子进程（Setpgid）：标准输出与标准错误合流到同一个写端，
// argv 直执不经任何 shell 解释，可执行名的查找语义交给 exec。
// spawnGuarded starts a child in its own process group; no shell is involved.
func spawnGuarded(req spawnRequest) (guardedProcess, error) {
	cmd := exec.Command(req.executable, req.args...)
	cmd.Dir = req.dir
	cmd.Env = req.env
	cmd.Stdout = req.output
	cmd.Stderr = req.output
	// Stdin 留 nil：exec 给子进程接空设备，与 Windows 侧接 NUL 同义。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动子进程 %s：%w", req.executable, err)
	}
	return &unixProcess{cmd: cmd, pid: cmd.Process.Pid}, nil
}

// wait 回收子进程并取退出码；非零退出码与被信号结束都算**回收成功**（退出码交给调用方
// 判读——被信号时是 -1，该值不会流到最终结果里，取消由 ctx 语义承担）。
// wait reaps the child; a non-zero exit or a signal is still a successful reap.
func (p *unixProcess) wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, fmt.Errorf("回收子进程 %d：%w", p.pid, err)
}

// killTree 向**整个进程组**发终止信号（`kill(-pgid)`）：组随 exec 原子生效，一次调用
// 即覆盖已派生的后代——不是单杀一个进程（那会留下仍在跑用户代码的子孙）。
// killTree signals the whole process group rather than a single process.
func killTree(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("终止进程组 %d：%w", pid, err)
	}
	return nil
}

// killTree 是 guardedProcess 契约面的绑定，实参是进程组 pid。
func (p *unixProcess) killTree() error { return killTree(p.pid) }

// close 在 Unix 侧无实体资源可还：进程由 wait 回收，进程组随组内进程清空而消失。
// close has nothing to release on Unix: wait already reaped the process.
func (p *unixProcess) close() error { return nil }
