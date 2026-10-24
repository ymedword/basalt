//go:build e2e

package e2e

// 本文件承载实例锁的进程级回填（US-15 模块卡 §2.6.1 的 US-1 §6 探针②、§2.6.3 的
// US-3 §6 第 4/5 项、§2.6.9 的 US-9 §6.2 第 8 项）：每一项都以**两个真实进程**竞争
// 同一工作区的 `.basalt/basalt.lock` 重放，断言只读标准流、退出码与工作区文件。
// 平台分支（Windows LockFileEx / 类 Unix flock）各自留读数：可编译不等于行为已验证。
// This file backfills the instance lock at process level: two real processes contend
// for `.basalt/basalt.lock`, and each platform branch leaves its own reading.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 冻结字面量只在这里定义一次（与 internal/workspace 的 LockBusyError 及装配层日志逐字一致）。
// The frozen literals are defined once here, verbatim from the workspace package and the
// assembly-layer log lines.
const (
	// lockBusyMessage / lockBusyRefuse / lockBusyGuidance 是 LockBusyError.Error() 的三段固定文案。
	// The three fixed clauses of LockBusyError.Error().
	lockBusyMessage  = "工作区实例锁已被占用"
	lockBusyRefuse   = "另一个活实例正在运行，写模式命令拒绝启动"
	lockBusyGuidance = "服务在跑时请改用 REST / 管理台"

	// lockAcquiredLine 是重路径取锁后的 INFO（`step=lock`），也是「锁已就绪」的确定性观测点。
	// The INFO line emitted once the heavy path holds the lock: the deterministic
	// "lock is held" observation point.
	lockAcquiredLine = "重路径：实例锁已取得"

	// lockAssemblyFailed 是重路径装配失败的 ERROR 文案。
	// The ERROR line emitted when heavy assembly fails.
	lockAssemblyFailed = "重路径装配失败"

	// lockPlaceholderModelURL 是仅在装载期被校验的占位模型地址（本文件不触发模型调用）。
	// A placeholder model URL validated at load time only; no model call happens here.
	lockPlaceholderModelURL = "http://127.0.0.1:1/v1"
)

// TestBackfill_CrossProcessLock 是实例锁回填的总入口；子测试名与模块卡逐行对应。
// TestBackfill_CrossProcessLock is the process-level lock backfill; subtest names map
// one-to-one onto the module card.
func TestBackfill_CrossProcessLock(t *testing.T) {
	t.Run("instance_lock", testCrossProcessInstanceLock)
	t.Run("write_mode", testCrossProcessWriteMode)
	t.Run("platform_lock", testCrossProcessPlatformLock)
	t.Run("heavy_commands", testCrossProcessHeavyCommands)
}

// testCrossProcessInstanceLock 复跑 US-1 §6 探针②：S2 活实例互斥、S1 崩溃后重入。
// testCrossProcessInstanceLock replays US-1 probe ②: live-instance exclusion (S2) and
// re-acquisition after a crash (S1).
func testCrossProcessInstanceLock(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(lockPlaceholderModelURL, addr))

	holder := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, holder, addr, 30*time.Second)
	requireContains(t, holder.combined(), lockAcquiredLine, "S2 持有者取锁")
	holderPID := holder.cmd.Process.Pid

	// S2：第二个真实进程（同一重命令）必须因实例锁失败——不是因端口占用失败。
	// S2: the second real process must fail on the lock, not on a port collision.
	second := h.run(t, fixtureEnv(), "serve")
	requireExit(t, second, 1, "S2 第二实例")
	requireContains(t, second.Stderr, lockAssemblyFailed, "S2 第二实例：装配失败")
	requireLockBusyText(t, second.Stderr, holderPID, "S2 第二实例")
	if _, err := webHealthStatus(addr); err != nil {
		t.Fatalf("S2 拒绝期间原实例健康检查失败: %v", err)
	}

	// S1：持有者被强杀（内核随句柄释放锁），重启的实例必须重新取到锁并恢复服务。
	// S1: after a hard kill the kernel drops the lock; a restarting instance re-acquires it.
	holder.kill()
	if code := holder.waitExit(t, 15*time.Second); code == 0 {
		t.Fatalf("S1 强杀进程退出码 = 0，期望非零\n%s", holder.output())
	}
	waitListenerClosed(t, addr, 15*time.Second)

	resumed := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, resumed, addr, 30*time.Second)
	requireContains(t, resumed.combined(), lockAcquiredLine, "S1 崩溃后重新取锁")
	resumed.stop(t)
	requirePathExists(t, filepath.Join(h.Root, ".basalt", "basalt.lock"), "S1 锁文件未被删除")

	t.Logf("实例锁读数：S1 强杀 pid=%d 后新实例重新取锁；S2 第二实例被拒并点名 pid=%d",
		holderPID, holderPID)
}

// testCrossProcessWriteMode 复跑 US-3 §6 第 4 项：写模式命令跨进程拒绝，只读命令照常可用。
// testCrossProcessWriteMode replays US-3 §6 item 4: a second write-mode command is
// refused across processes while read-only commands keep working.
func testCrossProcessWriteMode(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(lockPlaceholderModelURL, addr))
	writeWebAgent(t, h, "ops-agent", nil)

	holder := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, holder, addr, 30*time.Second)
	holderPID := holder.cmd.Process.Pid

	// ① 写模式创建被拒：拒绝启动、文案点名 pid 与开始时间、指引 REST / 管理台、零写入。
	// ① Write-mode create is refused with the holder diagnostics and performs zero writes.
	create := h.run(t, fixtureEnv(), "profile", "create", "lock-probe")
	requireExit(t, create, 1, "持锁期间 profile create")
	requireContains(t, create.Stderr, "profile create 未通过：工作区写入被拒", "持锁期间 profile create：失败日志")
	requireLockBusyText(t, create.Stderr, holderPID, "持锁期间 profile create")
	requirePathAbsent(t, filepath.Join(h.Root, ".basalt", "agents", "lock-probe"), "被拒创建：零写入")

	// ② 写模式归档同样被拒，源目录原样保留（无归档副本）。
	// ② Write-mode delete is refused too; the source directory survives untouched.
	remove := h.run(t, fixtureEnv(), "profile", "delete", "ops-agent")
	requireExit(t, remove, 1, "持锁期间 profile delete")
	requireContains(t, remove.Stderr, "profile delete 未通过：工作区写入被拒", "持锁期间 profile delete：失败日志")
	requireLockBusyText(t, remove.Stderr, holderPID, "持锁期间 profile delete")
	requirePathExists(t, filepath.Join(h.Root, ".basalt", "agents", "ops-agent", "AGENT.md"), "被拒删除：源目录保留")
	requireNoArchiveEntry(t, h, "ops-agent-")

	// ③ 只读命令不取锁：profile list 照常可用。
	// ③ Read-only commands take no lock: profile list keeps working.
	list := h.run(t, fixtureEnv(), "profile", "list")
	requireExit(t, list, 0, "持锁期间 profile list")
	requireContains(t, list.Stdout, "汇总：", "profile list：汇总行")
	requireContains(t, list.Stdout, "ops-agent", "profile list：目录清单")

	holder.stop(t)

	// ④ 释放后两条写模式命令都成功；锁文件保留但不再被持有。
	// ④ After release both write commands succeed; the lock file stays on disk.
	created := h.run(t, fixtureEnv(), "profile", "create", "lock-probe")
	requireExit(t, created, 0, "释放后 profile create")
	requireContains(t, created.Stdout, "创建：agents/lock-probe/AGENT.md", "释放后 profile create：创建路径")
	requirePathExists(t, filepath.Join(h.Root, ".basalt", "agents", "lock-probe", "AGENT.md"), "释放后创建落盘")

	archived := h.run(t, fixtureEnv(), "profile", "delete", "lock-probe")
	requireExit(t, archived, 0, "释放后 profile delete")
	requireContains(t, archived.Stdout, "归档：archive/lock-probe-", "释放后 profile delete：归档路径")
	requirePathAbsent(t, filepath.Join(h.Root, ".basalt", "agents", "lock-probe"), "归档后源目录移出")
	requirePathExists(t, filepath.Join(h.Root, ".basalt", "basalt.lock"), "锁文件保留")

	t.Logf("写模式读数：持锁期间 create / delete 均被拒（pid=%d），list 照常；释放后 create / delete 成功", holderPID)
}

// testCrossProcessPlatformLock 复跑 US-3 §6 第 5 项：暂停中的持有者仍占锁；释放后可重新取锁。
// testCrossProcessPlatformLock replays US-3 §6 item 5: a suspended holder still owns the
// lock, and a released lock is re-acquirable.
func testCrossProcessPlatformLock(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(lockPlaceholderModelURL, addr))

	holder := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, holder, addr, 30*time.Second)
	holderPID := holder.cmd.Process.Pid

	// 暂停窗口：持有者线程全停，内核锁仍在——第二进程必须被拒（探针② S3）。
	// Suspended window: the holder is frozen but the kernel lock stands; a second
	// process must still be refused (probe ② S3).
	suspendHolder(t, holderPID)
	create := h.run(t, fixtureEnv(), "profile", "create", "suspended-probe")
	requireExit(t, create, 1, "暂停窗口内 profile create")
	requireLockBusyText(t, create.Stderr, holderPID, "暂停窗口内 profile create")
	requirePathAbsent(t, filepath.Join(h.Root, ".basalt", "agents", "suspended-probe"), "暂停窗口内：零写入")

	resumeHolder(t, holderPID)
	waitHealthHTTP(t, holder, addr, 30*time.Second)
	holder.stop(t)

	// 优雅释放后同一把锁可被下一条重命令取回（gateway 走同一装配顺序）。
	// After a graceful release the very same lock is re-acquired by the next heavy command.
	gateway := startGrouped(t, h, fixtureEnv(), "gateway")
	gateway.waitOutput(t, "调度器已启动", 30*time.Second)
	gateway.stop(t)

	t.Logf("平台分支读数：GOOS=%s GOARCH=%s 机制=%s；暂停窗口内第二进程被拒（pid=%d），释放后 gateway 重新取锁并优雅退出",
		runtime.GOOS, runtime.GOARCH, lockMechanism(), holderPID)
}

// testCrossProcessHeavyCommands 复跑 US-9 §6.2 第 8 项：chat 与 serve 不能并行。
// testCrossProcessHeavyCommands replays US-9 §6.2 item 8: chat and serve cannot run
// side by side.
func testCrossProcessHeavyCommands(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(lockPlaceholderModelURL, addr))
	writeWebAgent(t, h, "ops-agent", nil)

	server := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, server, addr, 30*time.Second)
	serverPID := server.cmd.Process.Pid

	// ① serve 持锁时 chat 单条模式在装配期即被拒（模型一次都不会被调用）。
	// ① With serve holding the lock, one-shot chat is refused during assembly.
	oneShot := h.run(t, fixtureEnv(), "chat", "--agent", "ops-agent", "--message", "锁回填夹具消息")
	requireExit(t, oneShot, 1, "serve 持锁时 chat --message")
	requireContains(t, oneShot.Stderr, lockAssemblyFailed, "serve 持锁时 chat --message：装配失败")
	requireLockBusyText(t, oneShot.Stderr, serverPID, "serve 持锁时 chat --message")
	server.stop(t)

	// ② 交互式 chat 持锁（标准输入保持打开，进程停在读循环里）。
	// ② Interactive chat holds the lock while its stdin stays open.
	interactive := startPiped(t, h, fixtureEnv(), "chat", "--agent", "ops-agent")
	interactive.waitOutput(t, lockAcquiredLine, 30*time.Second)
	chatPID := interactive.cmd.Process.Pid

	serve := h.run(t, fixtureEnv(), "serve")
	requireExit(t, serve, 1, "chat 持锁时 serve")
	requireContains(t, serve.Stderr, lockAssemblyFailed, "chat 持锁时 serve：装配失败")
	requireLockBusyText(t, serve.Stderr, chatPID, "chat 持锁时 serve")

	// ③ 关闭标准输入即 EOF：chat 正常收场（退出码 0），锁随之释放。
	// ③ Closing stdin yields EOF: chat exits zero and the lock goes with it.
	interactive.closeStdin(t)
	if code := interactive.waitExit(t, 30*time.Second); code != 0 {
		t.Fatalf("关闭标准输入后 chat 退出码 = %d，期望 0\n%s", code, interactive.output())
	}

	after := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, after, addr, 30*time.Second)
	after.stop(t)
}

// ---------------------------------------------------------------------------
// 带可写标准输入的常驻进程（chat 交互循环）
// ---------------------------------------------------------------------------

// pipedProcess 是带可写标准输入的常驻进程：保持管道打开，chat 就停在读循环里。
// pipedProcess keeps a writable stdin open so chat stays parked in its read loop.
type pipedProcess struct {
	*groupedProcess
	stdin     io.WriteCloser
	stdinOnce sync.Once
}

// startPiped 拉起一条带管道的常驻进程；清理时先关管道再兜底杀进程。
// startPiped launches one piped long-lived process; cleanup closes stdin then kills.
func startPiped(t *testing.T, h *harness, env []string, args ...string) *pipedProcess {
	t.Helper()
	cmd := h.command(t, context.Background(), env, args...)
	cmd.SysProcAttr = groupedSysProcAttr()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("创建 %q 的标准输入管道失败: %v", strings.Join(args, " "), err)
	}
	p := &groupedProcess{
		cmd:      cmd,
		stdout:   &lockedBuffer{},
		stderr:   &lockedBuffer{},
		done:     make(chan struct{}),
		exitCode: -1,
	}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 %q 失败: %v", strings.Join(args, " "), err)
	}
	go func() {
		_ = cmd.Wait()
		p.mu.Lock()
		if cmd.ProcessState != nil {
			p.exitCode = cmd.ProcessState.ExitCode()
		}
		p.mu.Unlock()
		close(p.done)
	}()
	out := &pipedProcess{groupedProcess: p, stdin: stdin}
	t.Cleanup(func() {
		out.stdinOnce.Do(func() { _ = out.stdin.Close() })
		p.kill()
	})
	return out
}

// closeStdin 关闭标准输入（即向 chat 发 EOF）；重复调用是幂等的。
// closeStdin sends EOF to chat; it is idempotent.
func (p *pipedProcess) closeStdin(t *testing.T) {
	t.Helper()
	p.stdinOnce.Do(func() {
		if err := p.stdin.Close(); err != nil {
			t.Errorf("关闭 chat 标准输入失败: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// 平台分支：暂停 / 恢复持有者
// ---------------------------------------------------------------------------

// lockMechanism 返回当前平台的锁机制描述（供平台分支读数登记）。
// lockMechanism names the platform lock primitive for the per-platform reading.
func lockMechanism() string {
	if runtime.GOOS == "windows" {
		return "LockFileEx(LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY) 区域 [0,1)"
	}
	return "flock(LOCK_EX|LOCK_NB)"
}

// suspendHolder 暂停持有者进程（Windows：NtSuspendProcess；类 Unix：SIGSTOP + 状态轮询）。
// suspendHolder freezes the holder (NtSuspendProcess on Windows, SIGSTOP elsewhere).
func suspendHolder(t *testing.T, pid int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		windowsProcessControl(t, "NtSuspendProcess", pid)
		return
	}
	runKill(t, "-STOP", pid)
	waitProcessStopped(t, pid, 10*time.Second)
}

// resumeHolder 恢复持有者进程（Windows：NtResumeProcess；类 Unix：SIGCONT）。
// resumeHolder unfreezes the holder.
func resumeHolder(t *testing.T, pid int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		windowsProcessControl(t, "NtResumeProcess", pid)
		return
	}
	runKill(t, "-CONT", pid)
}

// windowsProcessControl 经 PowerShell P/Invoke 调用 ntdll 的暂停 / 恢复入口。
// 探针实测（US-1 §6 探针② S3）：OpenProcess(PROCESS_SUSPEND_RESUME) + NtSuspendProcess
// 在本机可用；Add-Type 的 C# 片段只用双引号，脚本里不出现单引号以外的转义。
// windowsProcessControl calls ntdll's suspend/resume entry through PowerShell P/Invoke;
// the US-1 probe already validated the same route on this machine.
func windowsProcessControl(t *testing.T, function string, pid int) {
	t.Helper()
	script := `Add-Type -Namespace W -Name K -MemberDefinition '[DllImport("kernel32.dll", SetLastError=true)] public static extern IntPtr OpenProcess(uint dwDesiredAccess, bool bInheritHandle, int dwProcessId); [DllImport("kernel32.dll", SetLastError=true)] public static extern bool CloseHandle(IntPtr hObject); [DllImport("ntdll.dll")] public static extern int NtSuspendProcess(IntPtr processHandle); [DllImport("ntdll.dll")] public static extern int NtResumeProcess(IntPtr processHandle);'; $h = [W.K]::OpenProcess(0x0800, $false, ` +
		strconv.Itoa(pid) + `); if ($h -eq [IntPtr]::Zero) { exit 3 }; $status = [W.K]::` +
		function + `($h); [void][W.K]::CloseHandle($h); if ($status -ne 0) { exit 4 }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("经 PowerShell 调用 %s 失败（pid %d）: %v\n%s", function, pid, err, out)
	}
}

// runKill 调用系统 kill 发送信号（类 Unix 分支专用）。
// runKill sends one signal through the system kill (Unix branch only).
func runKill(t *testing.T, signal string, pid int) {
	t.Helper()
	out, err := exec.Command("kill", signal, strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		t.Fatalf("kill %s %d 失败: %v\n%s", signal, pid, err, out)
	}
}

// waitProcessStopped 轮询 ps 状态直到进程进入停止态（T 开头）。
// waitProcessStopped polls ps until the process reports a stopped state.
func waitProcessStopped(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err == nil {
			last = strings.TrimSpace(string(out))
			if strings.HasPrefix(last, "T") {
				return
			}
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("进程 %d 未在 %s 内进入停止态（最后读数 %q）", pid, timeout, last)
}

// ---------------------------------------------------------------------------
// 判卷小件
// ---------------------------------------------------------------------------

// requireLockBusyText 断言 LockBusyError 的冻结文案与持有者诊断（pid / 开始时间）。
// requireLockBusyText asserts the frozen busy text plus the holder diagnostics.
func requireLockBusyText(t *testing.T, text string, holderPID int, label string) {
	t.Helper()
	requireContains(t, text, lockBusyMessage, label+"：占用文案")
	requireContains(t, text, fmt.Sprintf("pid=%d", holderPID), label+"：持有者 pid")
	requireContains(t, text, "开始于 ", label+"：开始时间")
	requireContains(t, text, lockBusyRefuse, label+"：拒绝启动")
	requireContains(t, text, lockBusyGuidance, label+"：REST / 管理台指引")
}

// requirePathExists 断言路径存在（不跟随符号链接的语义对本判据无影响）。
// requirePathExists asserts the path exists.
func requirePathExists(t *testing.T, path, label string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s：路径应存在 %s: %v", label, path, err)
	}
}

// requirePathAbsent 断言路径不存在（只接受"确实不存在"，其余 stat 错误照实判红）。
// requirePathAbsent asserts the path does not exist; other stat errors fail loudly.
func requirePathAbsent(t *testing.T, path, label string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("%s：路径不应存在：%s", label, path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s：检查路径 %s 失败: %v", label, path, err)
	}
}

// requireNoArchiveEntry 断言归档目录里没有指定前缀的条目（被拒删除不得留下归档副本）。
// requireNoArchiveEntry asserts no archive entry with the prefix exists.
func requireNoArchiveEntry(t *testing.T, h *harness, prefix string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.Root, ".basalt", "archive"))
	if err != nil {
		t.Fatalf("读取归档目录失败: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			t.Fatalf("被拒删除不应产生归档副本：%s", entry.Name())
		}
	}
}
