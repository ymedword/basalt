package web

// 本文件是管理认证失败的来源限速（模块卡 §3.1.2、约束 7）：按来源 IP 在固定窗口内
// 累计认证失败，第 10 次请求起在**凭据校验之前**直接 429——429 先于凭据判定，避免
// 攻击者继续制造 token 试探。计数在进程内、重启清零；成功认证**不**重置窗口，
// 计数只在窗口到期时清零（约束 7）。
// This file is the per-source authentication failure limiter: a fixed window counts
// failures per source IP and the tenth request is refused with 429 before credentials
// are checked. Counts are in-process, reset on restart, and only expire with the window.

import (
	"net"
	"strings"
	"sync"
	"time"
)

// 限速参数（模块卡约束 7 / 契约 run-and-errors §8 行 459：60 秒内失败 10 次）。
// Limiter parameters: ten failures in a sixty-second window.
const (
	authFailureLimit  = 10
	authFailureWindow = 60 * time.Second
)

// authFailureEntry 是一个来源在当前窗口内的失败计数。
// One source's failure count inside the current window.
type authFailureEntry struct {
	count int
	start time.Time
}

// authLimiter 是进程内的来源失败计数表：`*authLimiter` 的全部方法可并发调用，
// 计数与窗口判定在同一把锁内完成（并发失败不会丢计数）。
// The limiter is a process-local failure table; counting and expiry are evaluated
// under one lock so concurrent failures never lose a count.
type authLimiter struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	now      func() time.Time
	failures map[string]*authFailureEntry
	// lastSweep 控制过期清理的节奏：每次写入时最多每窗口清扫一遍，防止只被
	// 访问一次的来源把表撑大（约束 7 的"窗口过期清理"）。
	// lastSweep bounds sweep frequency to once per window.
	lastSweep time.Time
}

// newAuthLimiter 构造限速表。max 是触发限速的请求序号（第 max 次请求起 429），
// 因而窗口内最多记录 max-1 次失败；window 必须为正。
// newAuthLimiter builds the table. max is the ordinal of the first request to be
// refused, so at most max-1 failures are recorded per window.
func newAuthLimiter(max int, window time.Duration, now func() time.Time) *authLimiter {
	if max < 2 {
		max = authFailureLimit
	}
	if window <= 0 {
		window = authFailureWindow
	}
	if now == nil {
		now = time.Now
	}
	return &authLimiter{
		max:      max,
		window:   window,
		now:      now,
		failures: make(map[string]*authFailureEntry),
	}
}

// blocked 判定某来源是否已被限速；被限速时返回窗口剩余时长供 Retry-After 使用。
// 过期条目在这里原地清除——窗口到期即恢复，不做滑动窗口。
// blocked reports whether a source is currently limited and how long the window has
// left; an expired entry is dropped in place, so the window is fixed, not sliding.
func (l *authLimiter) blocked(source string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	entry, ok := l.failures[source]
	if !ok {
		return 0, false
	}
	if !now.Before(entry.start.Add(l.window)) {
		delete(l.failures, source)
		return 0, false
	}
	if entry.count < l.max-1 {
		return 0, false
	}
	return entry.start.Add(l.window).Sub(now), true
}

// Fail 记录一次认证失败，并返回该来源此后是否处于限速状态。达到上限后不再累加
// （并发下最多记录 max-1 次），返回值表达"下一次请求将被 429"——**本次失败仍按
// 原状态码返回 401 / 403**（约束 7：第 1–9 次保持原状态码）。
// Fail records one failure and reports whether the source is now limited. The count
// stops at the threshold, and the return value describes the *next* request; the
// failing request itself keeps its original 401/403 status.
func (l *authLimiter) Fail(source string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	entry, ok := l.failures[source]
	if !ok || !now.Before(entry.start.Add(l.window)) {
		entry = &authFailureEntry{start: now}
		l.failures[source] = entry
	}
	if entry.count < l.max-1 {
		entry.count++
	}
	if entry.count < l.max-1 {
		return 0, false
	}
	return entry.start.Add(l.window).Sub(now), true
}

// Reset 清除某来源的计数：只用于显式重置（测试与后续管理入口），
// **不得**在认证成功时调用——成功认证不重置窗口（约束 7）。
// Reset drops one source's counter for explicit resets only; it must not be called
// on a successful authentication.
func (l *authLimiter) Reset(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, source)
}

// sweepLocked 每个窗口最多清扫一次过期条目；调用方必须已持锁。
// sweepLocked drops expired entries at most once per window; the caller holds the lock.
func (l *authLimiter) sweepLocked(now time.Time) {
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < l.window {
		return
	}
	l.lastSweep = now
	for key, entry := range l.failures {
		if !now.Before(entry.start.Add(l.window)) {
			delete(l.failures, key)
		}
	}
}

// authLimitKey 把来源归因结果归一为计数键：只按 IP 计数，丢端口（同一客户端的
// 每次连接端口都不同，按端口计数会让限速形同虚设）；IPv6 去方括号。
// authLimitKey reduces the attributed source to its IP: counting by port would make
// the limiter useless, since each client connection uses a fresh port.
func authLimitKey(source string) string {
	if host, _, err := net.SplitHostPort(source); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(strings.TrimSpace(source), "[]")
}

// retryAfterSeconds 把剩余时长换算成 Retry-After 的整数秒（向上取整，至少 1 秒）。
// retryAfterSeconds rounds the remainder up to whole seconds, with a floor of one.
func retryAfterSeconds(remaining time.Duration) int64 {
	seconds := int64((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}
