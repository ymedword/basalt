package workspace_test

// 本文件是 US-3 实例锁面的关键回归用例（颗粒度文档 §3.2 的 lock_test.go 八例，
// §4.3 两条"直写代码"逐条落地 + 另六条同判据用例；A10 = 跨进程互斥与生命周期）。
// 契约 run-and-errors.md §4.3.1：锁的生命周期由内核决定，诊断只作诊断——
// **不得**用 exists / 心跳过期作抢锁判据。
// Regression tests for the workspace instance lock. The primary evidence is
// cross-process (helper-process), never same-process alone.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/workspace"
)

// 中文验收点（**主判据**）：**跨进程**互斥——父进程持锁时子进程取锁必须失败并点名持有者
// （pid 即父进程），父进程释放后子进程再取必须成功。同进程双句柄只作补充：锁的判据必须
// 落在进程边界上，否则"锁只在本进程内生效"也能全绿（§2.3 流程 D）。
// Acceptance point (primary): the instance lock excludes across process boundaries.
const (
	lockHelperEnv    = "BASALT_LOCK_HELPER" // "1" = 本进程是 helper
	lockHelperRoot   = "BASALT_LOCK_ROOT"   // 工作区路径
	lockHelperAction = "BASALT_LOCK_ACTION" // acquire（取到即释放）/ hold（取到后阻塞，等被强杀）
)

func TestInstanceLock_HelperProcessMutualExclusion(t *testing.T) {
	if os.Getenv(lockHelperEnv) == "1" {
		lockHelperMain() // 子进程分支：做一遍并如实打印 "acquired" / "busy pid=…"，退出码恒 0
		return
	}
	root := t.TempDir()

	first, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Release() })

	// ① 父进程持锁 → 子进程取锁必须失败、且能读到持有者诊断（诊断在锁定区之外）
	out := runLockHelper(t, root, "acquire")
	require.Contains(t, out, "busy pid="+strconv.Itoa(os.Getpid()), "子进程应点名持锁者：%q", out)

	// ② 释放后子进程再取必须成功（重取路径）
	require.NoError(t, first.Release())
	require.Equal(t, "acquired", strings.TrimSpace(runLockHelper(t, root, "acquire")))
}

// runLockHelper 以自身测试二进制为 helper 跑一次，返回子进程 stdout；非零退出即用例失败。
// exec + 环境变量分派是标准形态；同步靠子进程**输出行**，不用睡眠（US-2 的 Started 信号同纪律）。
func runLockHelper(t *testing.T, root, action string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestInstanceLock_HelperProcessMutualExclusion")
	cmd.Env = append(os.Environ(),
		lockHelperEnv+"=1", lockHelperRoot+"="+root, lockHelperAction+"="+action)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "helper 退出异常：%s", out)
	return string(out)
}

// lockHelperMain 是 helper 子进程的主体：按 action 取一次锁并**如实打印**结果，随后
// os.Exit(0)。用 os.Exit 而不是 return：①断言把子进程 stdout 与 "acquired" 逐字比较，
// return 会让测试框架再打印 "PASS / ok …" 把输出弄脏；②"退出码恒 0"是判据的一部分——
// busy 是**预期结果**不是 helper 的失败（判定在父进程侧）。
// lockHelperMain acquires once, prints the truthful outcome, then exits zero.
func lockHelperMain() {
	root := os.Getenv(lockHelperRoot)
	action := os.Getenv(lockHelperAction)

	lock, err := workspace.AcquireInstanceLock(root)
	if err != nil {
		var busy *workspace.LockBusyError
		if errors.As(err, &busy) {
			fmt.Printf("busy pid=%d started=%s\n", busy.PID, busy.StartedAt.Format(time.RFC3339))
		} else {
			fmt.Printf("lock error %v\n", err)
		}
		os.Exit(0)
	}

	fmt.Println("acquired") // 持锁信号：父进程读到这一行才知道"锁已保住"（不用睡眠赌）
	if action == "hold" {
		for { // 阻塞等被强杀——锁的生命周期由内核决定（契约 §4.3.1）
			time.Sleep(time.Second)
		}
	}
	if err := lock.Release(); err != nil {
		fmt.Printf("release error %v\n", err)
	}
	os.Exit(0)
}

// 中文验收点（**主判据**）：强杀持锁进程 → 父进程必须能**立即**重取（OS 级锁随进程消亡释放），
// 且**不得**依赖心跳 / exists 判据（契约 §4.3.1；US-1 §6 三场景读数）。
// Acceptance point: killing the holder releases the OS-level lock immediately.
func TestInstanceLock_HelperProcessReacquiresAfterHolderKilled(t *testing.T) {
	if os.Getenv(lockHelperEnv) == "1" {
		lockHelperMain()
		return
	}
	root := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=TestInstanceLock_HelperProcessMutualExclusion")
	cmd.Env = append(os.Environ(),
		lockHelperEnv+"=1", lockHelperRoot+"="+root, lockHelperAction+"=hold")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	// 等子进程**打印** "acquired"——它是"锁已保住"的信号（不是睡一会儿再赌）
	lines := bufio.NewScanner(stdout)
	require.True(t, lines.Scan() && strings.TrimSpace(lines.Text()) == "acquired",
		"helper 未报告持锁：%q", lines.Text())

	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait() // 强杀后 Wait 必返回非 nil（平台差异），此处不判

	lock, err := workspace.AcquireInstanceLock(root) // 强杀后必须立刻可取——锁文件残留不构成障碍
	require.NoError(t, err, "强杀后未释放锁：%v", err)
	require.NoError(t, lock.Release())
}

// 中文验收点（补充）：同进程双句柄也必须互斥——第二条取锁失败并给出 *LockBusyError，
// 释放后重取成功。锁的判据以跨进程为主（上两例），本例只作补充。
// Acceptance point (supplementary): two handles in one process also exclude.
func TestInstanceLock_SecondAcquireFailsWhileHeld(t *testing.T) {
	root := t.TempDir()

	first, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err)

	second, err := workspace.AcquireInstanceLock(root)
	require.Error(t, err)
	require.Nil(t, second)

	var busy *workspace.LockBusyError
	require.ErrorAs(t, err, &busy)
	require.Equal(t, os.Getpid(), busy.PID)

	require.NoError(t, first.Release())

	third, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err, "释放后必须可重取")
	require.NoError(t, third.Release())
}

// 中文验收点：Release **幂等**——重复调用是 no-op 且返回 nil；"幂等"不等于"锁还占着"：
// 释放之后第三者必须取得到（漏解锁的实现会在这里判红）。
// Acceptance point: Release is idempotent and really releases.
func TestInstanceLock_ReleaseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	lock, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err)

	require.NoError(t, lock.Release())
	require.NoError(t, lock.Release(), "重复 Release 必须是 no-op 且返回 nil")
	require.NoError(t, lock.Release())

	again, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err, "幂等 Release 之后锁必须真的空出来")
	require.NoError(t, again.Release())
}

// 中文验收点：Release **不删除锁文件**（契约 §4.3.1："解锁后删文件"会引入竞态——
// 后到者可能锁在已被 unlink 的 inode 上）。
// Acceptance point: Release keeps the lock file on disk.
func TestInstanceLock_KeepsLockFileAfterRelease(t *testing.T) {
	root := t.TempDir()
	lock, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err)
	require.NoError(t, lock.Release())

	info, statErr := os.Stat(filepath.Join(root, "basalt.lock"))
	require.NoError(t, statErr, "Release 不得删除锁文件")
	require.False(t, info.IsDir())
}

// 中文验收点：拒锁文案点名持有者——pid 与开始时间来自**锁定区之外**的诊断记录（只要诊断
// 与锁区重叠，拿不到锁的进程恰好读不到"是谁占着"），并指引改用 REST / 管理台。
// Acceptance point: the busy error names the holder from the out-of-lock diagnostic area.
func TestInstanceLock_BusyErrorNamesHolder(t *testing.T) {
	root := t.TempDir()
	holder, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, holder.Release()) })

	_, err = workspace.AcquireInstanceLock(root)
	var busy *workspace.LockBusyError
	require.ErrorAs(t, err, &busy)

	require.Equal(t, os.Getpid(), busy.PID)
	require.False(t, busy.StartedAt.IsZero(), "开始时间应当可解析（诊断写在锁定区之外）")

	msg := err.Error()
	require.Contains(t, msg, "pid="+strconv.Itoa(os.Getpid()))
	require.Contains(t, msg, busy.StartedAt.Format(time.RFC3339))
	require.Contains(t, msg, "REST", "文案须指引改用 REST / 管理台（§2.3 流程 D）")
}

// readLockDiagnosticArea 读锁文件**锁定区之外**的诊断区（offset 4096 起 512 字节）。
// 不能 os.ReadFile 整个文件：Windows 的区段锁会挡掉任何覆盖 [0,1) 的读——**连同进程的
// 新句柄也挡**（本机实测：The process cannot access the file because another process has
// locked a portion of the file）。这正是"诊断必须写在锁定区之外"的另一面：能整文件读的
// 时刻，锁区恰好没被占着。
// readLockDiagnosticArea reads the out-of-lock diagnostic area; a whole-file read
// would be blocked by the Windows byte-range lock even within the same process.
func readLockDiagnosticArea(t *testing.T, lockPath string) []byte {
	t.Helper()
	file, err := os.Open(lockPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	buf := make([]byte, 512)
	n, err := file.ReadAt(buf, 4096)
	if err != nil {
		require.ErrorIs(t, err, io.EOF, "诊断区读取失败")
	}
	require.NotEmpty(t, strings.TrimSpace(string(buf[:n])), "诊断区应当有内容")
	return buf[:n]
}

// 中文验收点：竞争失败的取锁**不得改动锁文件字节**——打开时不带 O_TRUNC 的可判据形态
// （若先截断再锁，竞争方会把正在被读的诊断抹掉，本断言即判红）。
// Acceptance point: a failed acquire leaves the lock file byte-identical.
func TestInstanceLock_ContendedAcquireLeavesLockFileIntact(t *testing.T) {
	root := t.TempDir()
	holder, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, holder.Release()) })

	lockPath := filepath.Join(root, "basalt.lock")
	before := readLockDiagnosticArea(t, lockPath)

	_, err = workspace.AcquireInstanceLock(root)
	require.Error(t, err)

	after := readLockDiagnosticArea(t, lockPath)
	require.Equal(t, before, after, "竞争失败的取锁改动了锁文件（取锁前不得截断）")
}

// 中文验收点：**只读命令不取锁、不得清理或删除锁文件**（契约 §4.3.1；场景 3 的"服务在跑
// 时 list 照常可用"）——装载（轻路径）与段一装配跑完之后：锁文件字节不变、持锁方的锁仍在。
// Acceptance point: read-only paths touch nothing, and the holder keeps its lock.
func TestInstanceLock_ReadOnlyCommandsDoNotTouchLockFile(t *testing.T) {
	root := writeCompatSampleWorkspace(t)

	holder, err := workspace.AcquireInstanceLock(root) // "一个活实例正在运行"
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, holder.Release()) })

	lockPath := filepath.Join(root, "basalt.lock")
	before := readLockDiagnosticArea(t, lockPath)

	// 只读命令面走的路径：profile list / show 的装载 + 段一装配（无写、无锁）。
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
		Logger:    bufferedLogger(t),
	}).LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	section, err := workspace.NewContextLoader(bufferedLogger(t)).SectionOne(root, entries[0], sectionOneNow())
	require.NoError(t, err)
	require.NotEmpty(t, section)

	after := readLockDiagnosticArea(t, lockPath)
	require.Equal(t, before, after, "只读路径改动了锁文件")

	_, err = workspace.AcquireInstanceLock(root)
	require.Error(t, err, "只读路径不得释放他人的锁")
}
