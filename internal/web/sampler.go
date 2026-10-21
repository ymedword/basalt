package web

// 本文件实现进程级观测采样（模块卡 §3.1.4 / 约束 33、34）：CPU 利用率需要两次
// 读数求差，内存与 goroutine 可单次读取。采样最多每 5 秒重算一次，缓存期内返回
// 同一份读数（`sampled_at` 是缓存时间）；平台不支持或首次无差值基线时字段为
// null 并列入 `unavailable`，**不得**伪造 0（`SystemSample` 不含磁盘 / uptime）。
// Process sampling: CPU needs two readings, memory and goroutines need one. Sampling
// recomputes at most every 5s; unavailable readings stay null and are listed, never
// fabricated. Disk and uptime are deliberately absent from SystemSample.

import (
	"runtime"
	"sync"
	"time"
)

// samplerCacheWindow 是采样缓存窗口：5 秒内的重复请求返回同一份读数。
// samplerCacheWindow is the sampling cache window.
const samplerCacheWindow = 5 * time.Second

// SystemSample 是最近一次进程采样（模块卡 §3.1.4）：空值字段必须以 null 出现并
// 同时列入 `Unavailable`，不得用 0 代替"未采到"。
// SystemSample is one process sampling; missing readings are null and listed.
type SystemSample struct {
	SampledAt      time.Time
	CPUPercent     *float64
	MemoryRSSBytes *int64
	Goroutines     *int64
	Unavailable    []string
}

// cpuTime 是进程 CPU 的累计时间（用户态 + 内核态）。
// cpuTime is the cumulative process CPU time split by mode.
type cpuTime struct{ User, System time.Duration }

// Sampler 是采样的运行时状态：互斥保护缓存与上一次 CPU 读数；`lastAt` 零值表示
// 还没有任何采样。`prevCPU` / `prevAt` 只在真正重算时推进——缓存命中不改变基线，
// 因此 5 秒窗口内的百分比始终是"上一次重算时刻"以来的平均值。
// Sampler holds the cache and the previous CPU baseline; cache hits never advance the
// baseline, so the percentage always covers the last recomputation interval.
type Sampler struct {
	mu      sync.Mutex
	now     func() time.Time
	last    SystemSample
	lastAt  time.Time
	prevCPU cpuTime
	prevAt  time.Time
}

// NewSampler 构造采样器；nil 时钟退回 time.Now（装配期注入唯一时钟源）。
// NewSampler builds a sampler around the injected clock.
func NewSampler(now func() time.Time) *Sampler {
	if now == nil {
		now = time.Now
	}
	return &Sampler{now: now}
}

// processCPU / processRSS 是平台读数的调用接缝：生产路径直指平台实现，包内测试
// 用它们注入"平台不支持"（否则在 Windows / Linux 上无法覆盖 null 分支）。
// The process readers are seams for tests to inject unsupported platforms.
var (
	processCPU = readProcessCPU
	processRSS = readProcessRSS
)

// Sample 返回最近一次采样，必要时重算：距上次重算不足 5 秒直接返回缓存副本；
// 否则读取 CPU / RSS / goroutine，CPU 百分比按两次读数的差值与墙钟差值计算。
// 首次采样没有 CPU 基线 → `cpu_percent` 为 null 并列入 unavailable；基线会在本次
// 采样后建立，因此第二次重算即可给出读数。
// Sample returns the cached reading or recomputes it; the first CPU reading has no
// baseline, so it stays null and seeds the baseline for the next recomputation.
func (s *Sampler) Sample() SystemSample {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if !s.lastAt.IsZero() && now.Sub(s.lastAt) < samplerCacheWindow {
		return s.last.clone()
	}

	sample := SystemSample{SampledAt: now, Unavailable: make([]string, 0, 2)}
	if cpu, ok := processCPU(); ok {
		if !s.prevAt.IsZero() {
			wall := now.Sub(s.prevAt)
			delta := (cpu.User - s.prevCPU.User) + (cpu.System - s.prevCPU.System)
			if wall > 0 && delta >= 0 {
				percent := delta.Seconds() / wall.Seconds() * 100
				sample.CPUPercent = &percent
			} else {
				// 时钟回拨或计数回退：不猜读数，按不可用处理。
				sample.Unavailable = append(sample.Unavailable, "cpu_percent")
			}
		} else {
			sample.Unavailable = append(sample.Unavailable, "cpu_percent")
		}
		s.prevCPU = cpu
		s.prevAt = now
	} else {
		sample.Unavailable = append(sample.Unavailable, "cpu_percent")
	}

	if rss, ok := processRSS(); ok {
		sample.MemoryRSSBytes = &rss
	} else {
		sample.Unavailable = append(sample.Unavailable, "memory_rss_bytes")
	}

	goroutines := int64(runtime.NumGoroutine())
	sample.Goroutines = &goroutines

	s.last = sample
	s.lastAt = now
	return sample.clone()
}

// clone 深拷贝指针字段与切片：缓存值不得被调用方改写，返回的 nil 切片也必须
// 保持非 nil（`unavailable` 在 JSON 里永远是数组，不能是 null）。
// clone deep-copies the sample so callers can never mutate the cache.
func (s SystemSample) clone() SystemSample {
	out := SystemSample{
		SampledAt:   s.SampledAt,
		Unavailable: append(make([]string, 0, len(s.Unavailable)), s.Unavailable...),
	}
	if s.CPUPercent != nil {
		v := *s.CPUPercent
		out.CPUPercent = &v
	}
	if s.MemoryRSSBytes != nil {
		v := *s.MemoryRSSBytes
		out.MemoryRSSBytes = &v
	}
	if s.Goroutines != nil {
		v := *s.Goroutines
		out.Goroutines = &v
	}
	return out
}
