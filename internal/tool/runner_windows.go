//go:build windows

package tool

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 本文件是受控执行器的 Windows 平台半边：用 Job 对象把子进程整棵子树圈住，取消 / 超时
// 时一次终止整棵树。与 Unix 的差别在**关联关系**上：Windows 没有"新进程自动进某个组"
// 的机制，子进程必须被显式关联进 Job，于是就有了"启动与关联之间"这个窗口——顺序块
// ①~④（先挂起创建、再关联、最后恢复线程）就是为消除它而定的，**不得**调换。
// Windows half of the runner: a Job object wraps the child's whole subtree so that
// cancellation can terminate the tree at once. The create-suspended / assign / resume
// ordering exists to close the "already running but not yet in the job" window.

// treeKilledExitCode 是我们主动终止整棵树时记在进程上的退出码：它不会出现在最终结果里
// （取消路径对外由 ctx 语义承担），取值只为让事后用第三方工具看进程时能分辨。
// Exit code recorded on a tree we terminated ourselves; the result reports ctx instead.
const treeKilledExitCode = 1

// windowsProcess 是受 Job 对象约束的子进程句柄集：Job 用于整树终止，进程 / 线程句柄
// 用于等待与释放（三者都要还，谁申请谁释放）。
// windowsProcess holds the handles of a job-constrained child process.
type windowsProcess struct {
	job     windows.Handle
	process windows.Handle
	thread  windows.Handle
	pid     int
}

// 编译期断言：平台实现满足主流程依赖的契约面。
var _ guardedProcess = (*windowsProcess)(nil)

// spawnGuarded 按顺序块 ①~④ 起一个受 Job 约束的子进程：设 Job 限额（关句柄即杀全树）→
// CREATE_SUSPENDED 先挂起创建（挂起态不跑用户代码）→ 关联进 Job → ResumeThread。
// ③ / ④ 失败都走 cleanupSpawnFailure 的固定清理序列，错误如实上抛。
// spawnGuarded follows steps ①-④; the suspend-then-assign order is the whole point.
func spawnGuarded(req spawnRequest) (guardedProcess, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 Job 对象：%w", err)
	}
	// ① 关句柄即杀全树：即使我们漏掉某条终止路径，内核也会在 Job 句柄归零时清干净后代。
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	// 下面这处指针转换是本仓唯一一处未审计转换（另一处 `unsafe.Sizeof` 是编译期常量、不
	// 产生地址），且无法消除：x/sys 对这一内核 API 只有 //sys 声明——形参本来就是 uintptr，
	// 没有类型化封装。转换落在调用实参位置，指针在调用期间由运行时保持存活，没有悬垂窗口；
	// 消除它只剩两条更差的路：放弃 KILL_ON_JOB_CLOSE（丢掉步骤 ① 的内核兜底），或把同一
	// 转换藏进 reflect（掩盖审计点）。豁免经用户 2026-10-16 裁决，理由与复核读数见验收报告。
	// The only unaudited pointer conversion in the repo; the kernel API takes uintptr and
	// x/sys ships no typed wrapper, so it cannot be avoided without dropping step ①.
	// #nosec G103 -- 豁免理由：① 目标 API 形参即 uintptr、x/sys 无类型化封装；② 转换在调用实参位置，指针在调用期间由运行时保持存活；③ 消除它只能放弃 KILL_ON_JOB_CLOSE 兜底或把转换藏进 reflect，二者都更差（用户 2026-10-16 裁决）。
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("设置 Job 限额（关句柄即杀全树）：%w", err),
			closeHandle("Job 对象", job),
		)
	}
	// argv 直执：命令行按 Windows 的引号规则逐项转义（可执行名同样参与转义），
	// 中间不经任何 shell 解释；可执行名的查找语义交给 CreateProcess。
	argv := append([]string{req.executable}, req.args...)
	appName, err := windows.UTF16PtrFromString(req.executable)
	if err != nil {
		return nil, jobStartFailure(fmt.Errorf("编码可执行名 %q：%w", req.executable, err), job)
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		return nil, jobStartFailure(fmt.Errorf("编码命令行：%w", err), job)
	}
	var workDir *uint16
	if req.dir != "" {
		if workDir, err = windows.UTF16PtrFromString(req.dir); err != nil {
			return nil, jobStartFailure(fmt.Errorf("编码工作目录 %q：%w", req.dir, err), job)
		}
	}
	envBlock, err := buildEnvBlock(req.env)
	if err != nil {
		return nil, jobStartFailure(err, job)
	}
	// buildEnvBlock 至少产出收尾 NUL，故取首元素地址是安全的；这个指针指向 Go 切片支撑的
	// 内存，CreateProcess 调用期间由运行时保持存活（指针作为实参传入即覆盖整块分配）。
	envPtr := &envBlock[0]
	stdin, err := openInheritableNull()
	if err != nil {
		return nil, jobStartFailure(err, job)
	}

	// ② 先挂起创建：子进程在关联完成之前不执行任何用户代码。标准输出与标准错误合流到
	// 同一个写端（顺序即进程写出的顺序），标准输入接空设备；CREATE_NO_WINDOW 让它不额外
	// 申请控制台（我们的标准句柄已经接满，多余的控制台窗口只会弹到用户屏幕上）。
	var info windows.ProcessInformation
	startup := windows.StartupInfo{
		Cb:        uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Flags:     windows.STARTF_USESTDHANDLES,
		StdInput:  windows.Handle(stdin.Fd()),
		StdOutput: windows.Handle(req.output.Fd()),
		StdErr:    windows.Handle(req.output.Fd()),
	}
	createErr := windows.CreateProcess(appName, commandLine, nil, nil, true,
		windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW,
		envPtr, workDir, &startup, &info)
	// 空设备副本的使命到此为止（子进程已拿到自己的一份），成败都要关；它的关闭失败
	// 不动摇"子进程起没起来"这个结论，故与创建结果一起 Join。
	stdinErr := closePipe("空设备", stdin)
	if createErr != nil {
		return nil, errors.Join(
			fmt.Errorf("创建子进程 %s：%w", req.executable, createErr),
			stdinErr, closeHandle("Job 对象", job),
		)
	}
	if stdinErr != nil {
		return nil, errors.Join(stdinErr, cleanupWindowsSpawn(job, info.Process, info.Thread))
	}
	// ③ 关联进 Job：失败时子进程还停在挂起态、没跑过一行用户代码，整树终止只是兜底。
	if err := windows.AssignProcessToJobObject(job, info.Process); err != nil {
		return nil, errors.Join(
			fmt.Errorf("把子进程 %s 关联到 Job 对象：%w", req.executable, err),
			cleanupWindowsSpawn(job, info.Process, info.Thread),
		)
	}
	// ④ 恢复主线程：到这里子进程才真正开跑，且已经在 Job 之内。
	if _, err := windows.ResumeThread(info.Thread); err != nil {
		return nil, errors.Join(
			fmt.Errorf("恢复子进程 %s 的主线程：%w", req.executable, err),
			cleanupWindowsSpawn(job, info.Process, info.Thread),
		)
	}
	return &windowsProcess{job: job, process: info.Process, thread: info.Thread, pid: int(info.ProcessId)}, nil
}

// wait 等子进程退出并取退出码；只有等待 / 取值本身失败才算错误（非零退出码是正常产出）。
// wait blocks until the child exits; a non-zero exit code is not an error.
func (p *windowsProcess) wait() (int, error) {
	if _, err := windows.WaitForSingleObject(p.process, windows.INFINITE); err != nil {
		return 0, fmt.Errorf("等待子进程 %d 退出：%w", p.pid, err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.process, &code); err != nil {
		return 0, fmt.Errorf("取子进程 %d 的退出码：%w", p.pid, err)
	}
	return int(code), nil
}

// killTree 终止 Job 对象内的**整棵树**（顺序块 ⑤）：不是 TerminateProcess 单杀——
// 单杀只结束子进程一个，它已经派生的后代照旧活着，而它们同样是用户代码。
// killTree terminates the whole job; killing one process would leave its descendants.
func killTree(job uintptr) error {
	if err := windows.TerminateJobObject(windows.Handle(job), treeKilledExitCode); err != nil {
		return fmt.Errorf("终止 Job 对象内的进程树：%w", err)
	}
	return nil
}

// killTree 是 guardedProcess 契约面的绑定，实参是 Job 句柄而非进程组。
func (p *windowsProcess) killTree() error { return killTree(uintptr(p.job)) }

// close 按 ⑥ 的顺序释放句柄：进程 → 线程 → Job。Job 句柄一关就触发 KILL_ON_JOB_CLOSE，
// 把仍挂在 Job 里的后代一并清掉——这是"兜底不漏孙进程"的另一半。
// close releases handles in the step-⑥ order; closing the job trips KILL_ON_JOB_CLOSE.
func (p *windowsProcess) close() error {
	return errors.Join(
		closeHandle("进程", p.process),
		closeHandle("线程", p.thread),
		closeHandle("Job 对象", p.job),
	)
}

// cleanupSpawnFailure 是 ③ / ④ 失败时的固定清理序列：先整树终止兜底（挂起态的进程也可能
// 已经占住内核资源），再依次关进程、线程、Job 句柄，最后把各步错误 Join 上抛——某一步
// 失败**不得**让后面的步骤被跳过，清理也不掩盖原始失败原因。**实参求值顺序即本函数的
// 契约**（Go 保证按实参从左到右求值），调用方**必须**按这个顺序传参。
// cleanupSpawnFailure runs the fixed cleanup sequence of steps ③/④; the argument order
// *is* the contract (Go evaluates call arguments left to right).
func cleanupSpawnFailure(terminateTree, closeProcess, closeThread, closeJob func() error) error {
	return errors.Join(terminateTree(), closeProcess(), closeThread(), closeJob())
}

// cleanupWindowsSpawn 把 cleanupSpawnFailure 绑到真实句柄上（三个失败路径共用一处绑定）。
// cleanupWindowsSpawn binds the sequence to the real handles.
func cleanupWindowsSpawn(job, process, thread windows.Handle) error {
	return cleanupSpawnFailure(
		func() error { return killTree(uintptr(job)) },
		func() error { return closeHandle("进程", process) },
		func() error { return closeHandle("线程", thread) },
		func() error { return closeHandle("Job 对象", job) },
	)
}

// jobStartFailure 收口"还没起进程就失败"的那一族：此时 Job 句柄是唯一已申请的资源。
// jobStartFailure releases the job handle when spawning fails before CreateProcess.
func jobStartFailure(err error, job windows.Handle) error {
	return errors.Join(err, closeHandle("Job 对象", job))
}

// buildEnvBlock 编码 CreateProcess 需要的环境块：逐条 UTF-16、各自 NUL 收尾，整体再补一个
// NUL（CREATE_UNICODE_ENVIRONMENT 形态）。**顺序保持传入顺序**、不排序不合并：同名变量在
// Windows 上以先出现的为准，排序会把"工具声明覆盖白名单透传"的优先级弄反。
// buildEnvBlock encodes the environment block; order is preserved because Windows
// honours the first occurrence of a duplicated name.
func buildEnvBlock(env []string) ([]uint16, error) {
	block := make([]uint16, 0, 64)
	for _, entry := range env {
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, fmt.Errorf("编码环境变量 %q：%w", entry, err)
		}
		block = append(block, encoded...)
	}
	return append(block, 0), nil
}

// openInheritableNull 打开空设备（NUL）并显式标上继承标志：子进程的标准输入接它，读到的是
// 干净的 EOF。os.Open 拿到的句柄带 O_CLOEXEC（不可继承），**必须**显式置位——不置位的话
// 子进程拿到的标准输入是无效句柄，读到 ERROR_INVALID_HANDLE 而不是 EOF。
// os.Open yields a non-inheritable handle, so the inherit flag must be set explicitly.
func openInheritableNull() (*os.File, error) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		return nil, fmt.Errorf("打开空设备：%w", err)
	}
	if err := windows.SetHandleInformation(windows.Handle(file.Fd()),
		windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return nil, errors.Join(fmt.Errorf("把空设备句柄标为可继承：%w", err), closePipe("空设备", file))
	}
	return file, nil
}

// closeHandle 关闭一个内核句柄；无效句柄（0 / InvalidHandle）按"本来就没有"放行——
// 调用点不为了省这一次判断而分支。
// closeHandle releases a kernel handle, tolerating the invalid placeholder value.
func closeHandle(name string, handle windows.Handle) error {
	if handle == 0 || handle == windows.InvalidHandle {
		return nil
	}
	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("关闭%s句柄：%w", name, err)
	}
	return nil
}
