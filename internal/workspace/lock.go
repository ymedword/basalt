package workspace

// 本文件是工作区**实例锁**（.basalt/basalt.lock，契约 run-and-errors.md §4.3.1）的持有
// 句柄与诊断读写：OS 级独占文件锁，生命周期由内核决定；instance_id / pid / 开始时间只是
// **诊断**——**不得**据其抢锁（不得用 exists 判据、不得用心跳过期判据）。
// 布局：锁区 = offset 0 起 1 字节；诊断记录写在锁定区**之外**的固定偏移（4096 起、定长）——
// Windows 排他锁阻止其他句柄读锁定区，诊断放区外才读得到"是谁占着"，而"是谁占着"正是
// 拿不到锁的那一刻唯一要的信息。平台落位见 lock_windows.go / lock_unix.go。
// This file implements the instance lock: an OS-level exclusive file lock plus an
// out-of-lock diagnostic record read by contended processes.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// lockFileName 是锁文件名（契约 §4.3.1 字面量：.basalt/basalt.lock）。
	lockFileName = "basalt.lock"

	// diagnosticOffset / diagnosticLength 是诊断区的位置与定长（§2.3 流程 D：offset 4096
	// 起、定长、字段序固定 instance_id → pid → 开始时间）。定长是**读得到**的前提：区外
	// 留下的旧记录（更长的 pid / id）必须被空格补齐覆盖，不得被读成当前记录。
	diagnosticOffset = 4096
	diagnosticLength = 512
)

// InstanceLock 是实例锁的持有句柄；取不到锁时**不存在**句柄（返回 nil + *LockBusyError）。
// InstanceLock holds the lock until Release; a failed acquire yields no handle at all.
type InstanceLock struct {
	file *os.File  // 锁文件句柄（锁随句柄存活——不得提前关闭）
	once sync.Once // Release 幂等的标志位
}

// AcquireInstanceLock 非阻塞取锁：成功返回句柄（调用方 defer Release）；被活实例持有 →
// *LockBusyError。取锁前**不截断**锁文件（O_CREATE|O_RDWR，无 O_TRUNC——竞争方可能正在
// 读诊断）；诊断写失败 → **先释放句柄**再返回错误（不留"持锁但无诊断"的句柄）。
// AcquireInstanceLock takes the lock without blocking and never truncates the file.
func AcquireInstanceLock(root string) (*InstanceLock, error) {
	lockPath := filepath.Join(root, lockFileName)

	file, err := openLockFile(root, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, err
	}

	if err := lockHandle(file); err != nil {
		if !isLockContended(err) {
			// 非竞争类失败（磁盘故障 / 句柄无效）必须原样上抛，不得伪装成"被占用"。
			return nil, errors.Join(
				fmt.Errorf("锁定实例锁文件 %s: %w", lockPath, err),
				closeFile(lockPath, file))
		}
		// 取不到：另开**免锁读句柄**读锁定区之外的诊断（Windows 排他锁只挡锁定区）。
		busy := readLockDiagnostics(root)
		return nil, errors.Join(busy, closeFile(lockPath, file))
	}

	lock := &InstanceLock{file: file}
	if err := lock.writeDiagnostic(); err != nil {
		return nil, errors.Join(err, lock.Release())
	}
	return lock, nil
}

// Release 幂等（重复调用 no-op 返回 nil）：只解锁 + 关闭句柄，**不删除锁文件**（契约
// §4.3.1："解锁后删文件"会引入竞态——后到者可能锁在已被 unlink 的 inode 上）。
// Release is idempotent and never removes the lock file.
func (l *InstanceLock) Release() error {
	var err error
	l.once.Do(func() {
		// 两个动作都执行（参数先求值）：解锁失败也必须关闭句柄——关闭本身就会释放内核锁。
		err = errors.Join(unlockHandle(l.file), closeFile(l.file.Name(), l.file))
	})
	return err
}

// writeDiagnostic 把诊断记录（instance_id / pid / 开始时间，字段序固定）写入锁定区之外的
// 固定偏移，定长 + 空格补齐。取锁时刻取 time.Now()：本函数没有 now 形参（签名冻结），
// 诊断时间必须是**真实**取锁时刻，显式传入反而会失真。
// writeDiagnostic writes the fixed-layout diagnostic record outside the lock region.
func (l *InstanceLock) writeDiagnostic() error {
	started := time.Now().UTC()
	record := fmt.Sprintf("instance_id=%d-%d\npid=%d\nstarted_at=%s\n",
		os.Getpid(), started.UnixNano(), os.Getpid(), started.Format(time.RFC3339))
	if len(record) > diagnosticLength {
		return fmt.Errorf("实例锁诊断记录过长（%d > %d 字节）", len(record), diagnosticLength)
	}

	buf := make([]byte, diagnosticLength)
	for i := range buf {
		buf[i] = ' '
	}
	copy(buf, record)

	if _, err := l.file.WriteAt(buf, diagnosticOffset); err != nil {
		return fmt.Errorf("写入实例锁诊断 %s: %w", l.file.Name(), err)
	}
	return nil
}

// readLockDiagnostics 用**免锁**只读句柄读诊断区并转成 *LockBusyError；读不到 / 解析不了
// 时在 Detail 里如实说明（不编造持有者——诊断只是诊断）。
// readLockDiagnostics reads the out-of-lock diagnostic area without taking the lock.
func readLockDiagnostics(root string) *LockBusyError {
	lockPath := filepath.Join(root, lockFileName)

	file, err := openLockFile(root, os.O_RDONLY)
	if err != nil {
		return &LockBusyError{Detail: "诊断记录读不出来（打开 " + lockPath + " 失败：" + err.Error() + "）"}
	}
	buf := make([]byte, diagnosticLength)
	n, readErr := file.ReadAt(buf, diagnosticOffset)
	closeErr := file.Close()

	busy := parseDiagnosticRecord(strings.TrimSpace(string(buf[:n])))
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		// EOF = 文件短于诊断区（诊断或未写入或被截断）——那属于"记录不完整"，不是读失败。
		busy = &LockBusyError{Detail: "诊断记录读取失败：" + readErr.Error()}
	}
	if closeErr != nil {
		// 关闭失败不改结论，但要如实并入（不吞错误）。
		busy.Detail = strings.TrimSpace(busy.Detail + "；关闭诊断句柄失败：" + closeErr.Error())
	}
	return busy
}

// parseDiagnosticRecord 解析固定字段序的诊断记录；缺字段时零值 + Detail 如实点名缺哪个，
// 不得编造持有者信息。instance_id 只作诊断标识，进不了人读文案（契约 §4.3.1 未要求展示）。
// parseDiagnosticRecord turns the record into a busy error, naming what is missing.
func parseDiagnosticRecord(record string) *LockBusyError {
	if record == "" {
		return &LockBusyError{Detail: "诊断区为空（持锁者可能是旧版本进程，或诊断尚未写入）"}
	}

	busy := &LockBusyError{}
	for _, line := range strings.Split(record, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "pid":
			pid, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				busy.Detail = "诊断记录里的 pid 不可解析"
				continue
			}
			busy.PID = pid
		case "started_at":
			at, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
			if err != nil {
				busy.Detail = "诊断记录里的 started_at 不可解析"
				continue
			}
			busy.StartedAt = at
		}
	}

	if busy.PID == 0 && busy.Detail == "" {
		busy.Detail = "诊断记录缺少 pid 字段"
	}
	if busy.StartedAt.IsZero() && busy.Detail == "" {
		busy.Detail = "诊断记录缺少 started_at 字段"
	}
	return busy
}

// openLockFile 经**根约束**打开锁文件（os.Root）：锁文件只能落在工作区里，这是结构保证
// 而不是路径拼接的约定（与 init / agent_files / context_loader 同一口径——裸 os.Open /
// os.OpenFile 拼路径既拦不住越界软连接，也是 gosec G304 的形态）。根句柄开完即关：返回的
// 文件句柄不依赖它存活，锁的语义只看文件本身。
// openLockFile opens the lock file through an os.Root and closes the root at once;
// the returned file handle outlives the root handle.
func openLockFile(root string, flag int) (*os.File, error) {
	lockPath := filepath.Join(root, lockFileName) // 只用于文案（真正打开走根约束）

	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	file, openErr := base.OpenFile(lockFileName, flag, filePerm)
	if closeErr := base.Close(); closeErr != nil {
		if openErr != nil {
			return nil, errors.Join(fmt.Errorf("打开实例锁文件 %s: %w", lockPath, openErr), closeErr)
		}
		// 根句柄关不掉：不留悬空句柄——关掉刚打开的文件并把失败如实上报（不吞）。
		return nil, errors.Join(fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr), closeFile(lockPath, file))
	}
	if openErr != nil {
		return nil, fmt.Errorf("打开实例锁文件 %s: %w", lockPath, openErr)
	}
	return file, nil
}

// closeFile 关闭句柄并把失败并入错误链（不吞：关闭失败可能正是"诊断没落盘"的信号）。
// closeFile closes the handle, folding any failure into the error chain.
func closeFile(path string, file *os.File) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭 %s: %w", path, err)
	}
	return nil
}

// LockBusyError 是取锁失败的结果（被活实例持有）：PID / StartedAt 来自锁定区之外的诊断
// 记录；诊断缺失时为零值 + Detail 如实说明。写模式命令据此**拒绝启动**。
// LockBusyError carries the holder diagnostics read from the out-of-lock area.
type LockBusyError struct {
	PID       int
	StartedAt time.Time
	Detail    string
}

// Error 渲染人读文案：点名 pid 与开始时间，并指引改用 REST / 管理台（§2.3 流程 D）。
// Error renders the holder diagnostics and the way around them.
func (e *LockBusyError) Error() string {
	var b strings.Builder
	b.WriteString("工作区实例锁已被占用")

	hasHolder := e.PID != 0 || !e.StartedAt.IsZero()
	if hasHolder {
		b.WriteString("：")
		if e.PID != 0 {
			b.WriteString("pid=" + strconv.Itoa(e.PID))
		}
		if !e.StartedAt.IsZero() {
			if e.PID != 0 {
				b.WriteString("，")
			}
			b.WriteString("开始于 " + e.StartedAt.Format(time.RFC3339))
		}
	}

	b.WriteString("——另一个活实例正在运行，写模式命令拒绝启动")
	if hasHolder {
		b.WriteString("（服务在跑时请改用 REST / 管理台）")
	}
	if e.Detail != "" {
		b.WriteString("；" + e.Detail)
	}
	return b.String()
}
