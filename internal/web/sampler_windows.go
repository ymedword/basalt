//go:build windows

package web

// Windows 采样实现（模块卡 §3.1.4）：`GetProcessTimes` 读累计 CPU，
// `psapi!GetProcessMemoryInfo` 读进程常驻内存。两者都不经 CGO，发布构建保持
// CGO_ENABLED=0；任一调用失败按"平台不支持"返回 false，由采样器置 null 并
// 列入 unavailable。
// Windows sampling without CGO; any failing call degrades to "unsupported".

import (
	"math"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processMemoryCounters 是 Win32 `PROCESS_MEMORY_COUNTERS` 的 Go 投影（psapi
// 的入参结构）；字段顺序与宽度必须与 psapi.h 一致。
// processMemoryCounters mirrors the Win32 PROCESS_MEMORY_COUNTERS layout.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// procGetProcessMemoryInfo 用 NewLazySystemDLL 解析：只从 System32 加载，避免
// DLL 预加载劫持（模块卡要求 LazySystemDLL）。
// The psapi proc is resolved through NewLazySystemDLL to defeat DLL preloading.
var procGetProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

// readProcessCPU 读当前进程的累计用户态 / 内核态 CPU 时间。
// readProcessCPU returns the cumulative user and kernel CPU time.
func readProcessCPU() (cpuTime, bool) {
	handle := windows.CurrentProcess()
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return cpuTime{}, false
	}
	return cpuTime{
		User:   time.Duration(user.Nanoseconds()),
		System: time.Duration(kernel.Nanoseconds()),
	}, true
}

// readProcessRSS 读当前进程的常驻内存（WorkingSetSize，单位字节）。
// readProcessRSS returns the working-set size in bytes.
func readProcessRSS() (int64, bool) {
	handle := windows.CurrentProcess()
	counters := processMemoryCounters{CB: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	// #nosec G103 -- 豁免理由：① psapi!GetProcessMemoryInfo 的形参就是结构体指针，
	// x/sys/windows 没有类型化封装（模块卡 §3.1.4 明确要求 LazySystemDLL +
	// PROCESS_MEMORY_COUNTERS 直调）；② 转换在调用实参位置，counters 是栈变量、
	// 调用期间由运行时保持存活；③ 消除它只能放弃无 CGO 的内存采样或把转换藏进
	// reflect，二者都更差；只在 Windows 构建、不涉及凭证与文件路径。
	// 成功与否只看返回值 r1；last-error 不参与判定（失败即按不支持处理）。
	r1, _, _ := procGetProcessMemoryInfo.Call(
		uintptr(handle), uintptr(unsafe.Pointer(&counters)), unsafe.Sizeof(counters))
	if r1 == 0 {
		return 0, false
	}
	// SIZE_T 是无符号指针宽度；先按 1<<63 判界再转 int64，超出可表示范围按
	// "平台不支持"处理——概览**不得**给出回绕后的假读数。
	// Bound the unsigned SIZE_T before narrowing it to int64.
	workingSet := uint64(counters.WorkingSetSize)
	if workingSet > math.MaxInt64 {
		return 0, false
	}
	return int64(workingSet), true
}
