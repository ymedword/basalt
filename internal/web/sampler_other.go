//go:build !windows && !linux

package web

// 其它平台没有实现进程采样（模块卡 §3.1.4）：两个读数一律返回 false，采样器据此
// 把对应字段置 null 并列入 unavailable——**不得**用 0 冒充读数。
// Unsupported platforms report false; the sampler renders null, never zero.

// readProcessCPU 在未实现平台恒返回 false。
// readProcessCPU always reports unsupported outside Windows and Linux.
func readProcessCPU() (cpuTime, bool) { return cpuTime{}, false }

// readProcessRSS 在未实现平台恒返回 false。
// readProcessRSS always reports unsupported outside Windows and Linux.
func readProcessRSS() (int64, bool) { return 0, false }
