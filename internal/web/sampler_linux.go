//go:build linux

package web

// Linux 采样实现（模块卡 §3.1.4）：`/proc/self/stat` 第 14 / 15 字段给累计
// 用户态 / 内核态度量（USER_HZ=100），`/proc/self/statm` 第 2 字段给常驻页数。
// 任一解析失败按"平台不支持"返回 false，由采样器置 null 并列入 unavailable。
// Linux sampling from /proc; parse failures degrade to "unsupported".

import (
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// linuxUserHZ 是内核 USER_HZ：stat 的 utime / stime 以 1/100 秒计。
// linuxUserHZ is the kernel clock-tick frequency used by /proc/self/stat.
const linuxUserHZ = 100

// linuxTickDuration 是一个 tick 的时长（10ms）。解析结果一律先做上界检查再换算：
// 超出 time.Duration 范围时按"平台不支持"降级，**不得**让强制转换静默回绕。
// linuxTickDuration is one 10ms tick; out-of-range values degrade to unsupported
// instead of letting a conversion wrap silently.
const linuxTickDuration = time.Second / linuxUserHZ

// readProcessCPU 读当前进程的累计用户态 / 内核态 CPU 时间。
// readProcessCPU returns the cumulative user and kernel CPU time.
func readProcessCPU() (cpuTime, bool) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return cpuTime{}, false
	}
	stat := string(data)
	// comm 字段可含空格与括号，字段切分只能从**最后一个** ')' 之后开始：
	// 之后的第 1 项是整体第 3 字段 state，utime / stime 是整体第 14 / 15 字段。
	// Split after the last ')': the remainder starts at field 3.
	idx := strings.LastIndexByte(stat, ')')
	if idx < 0 {
		return cpuTime{}, false
	}
	fields := strings.Fields(stat[idx+1:])
	if len(fields) < 13 {
		return cpuTime{}, false
	}
	user, err := strconv.ParseInt(fields[11], 10, 64)
	if err != nil || user < 0 {
		return cpuTime{}, false
	}
	system, err := strconv.ParseInt(fields[12], 10, 64)
	if err != nil || system < 0 {
		return cpuTime{}, false
	}
	maxTicks := math.MaxInt64 / int64(linuxTickDuration)
	if user > maxTicks || system > maxTicks {
		return cpuTime{}, false
	}
	return cpuTime{
		User:   time.Duration(user * int64(linuxTickDuration)),
		System: time.Duration(system * int64(linuxTickDuration)),
	}, true
}

// readProcessRSS 读当前进程的常驻内存（statm 第 2 字段 × 页大小）。
// readProcessRSS returns resident pages times the page size.
func readProcessRSS() (int64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || pages < 0 {
		return 0, false
	}
	pageSize := os.Getpagesize()
	if pageSize <= 0 || pages > math.MaxInt64/int64(pageSize) {
		return 0, false
	}
	return pages * int64(pageSize), true
}
