package app

// 本文件是图 6 [1]「入口速率」的**覆盖来源**（C-F01）：把 `caller_limits.qps` /
// `burst` 两列投影成 `callers.IngressOverrides` 需要的同步读口。令牌桶本身在
// `internal/callers`，本文件只回答"这个调用方要不要比类别预算更紧"。
// This file supplies the per-caller ingress override for admission stage [2]; the
// bucket itself lives in internal/callers and this layer only answers "tighter?".
//
// 语义（契约 `docs/contracts/schema.pg.sql` 的 `0 = 不限`，2026-10-31 裁决第 5 条）：
//   - 没有 `caller_limits` 行，或 `qps` / `burst` 两列都为 0 → 没有覆盖，类别预算
//     单独生效（**不是**"不限流"：入口统一限流是外闸）。
//   - 任一列 > 0 → 该列作为覆盖交给令牌桶，由 `tightenIngressBudget` 取
//     `min(类别, 覆盖)`——覆盖只能收紧，不能放宽。
//   - **读存储失败 → 错误上抛**（不降级为"没有覆盖"）：入口判定失败关闭，中间件
//     按未知错误记 WARN 并回 500。这里与并发限额缓存（读失败 → 名额拒绝）方向一致，
//     但表现不同：速率没有"拒绝即安全"之外的中性答案。
//
// 缓存：与 `concurrency_limits.go` 同一口径——结果按 `ingressLimitTTL` 缓存，管理面
// 改完最迟一个 TTL 后生效（跨进程无失效广播，登记在验收报告的未覆盖面）；读失败
// **不**缓存，下一个请求立刻重试。桶键按 CallerID 而不是 KeyID：轮换 Key 不重置
// 配额（C-F01 要求的键轮换语义）。

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/callers"
)

const (
	// ingressLimitTTL 是覆盖投影的缓存有效期。只影响"管理面改完多久生效"，不影响
	// 类别预算的正确性：类别预算来自进程内配置，不经这条路径。
	ingressLimitTTL = 10 * time.Second

	// ingressLimitReadTimeout 是单次投影读取的截止时间。入口判定是同步路径，
	// 查询卡住不得拖住整个请求；超时按读失败处理（500，不伪装成 429）。
	ingressLimitReadTimeout = 2 * time.Second
)

// errIngressOverridesUnconfigured 是装配错误：读口没有接上时拒绝服务，**不得**
// 悄悄当成"没有覆盖"（那正是 C-F01 要消灭的"配了不生效"）。
// errIngressOverridesUnconfigured fails closed instead of silently disabling
// per-caller enforcement.
var errIngressOverridesUnconfigured = errors.New("app: 调用方入口预算读口未装配")

// ingressLimitReader 是 `postgres.QuotaStore.IngressBudget` 的方法值形态。用函数
// 类型而不是把存储类型存进结构体：装配层只需要这一个读点。
// The reader is the method value of the quota store, so no exported store type is
// needed just to read the override.
type ingressLimitReader func(ctx context.Context, callerID string) (callers.IngressBudget, bool, error)

// ingressLimitCache 把存储读点包成 `callers.IngressOverrides`：带 TTL 缓存、读失败
// 原样上抛、只缓存成功结果。
// ingressLimitCache adapts the store read into the limiter's override port with a
// TTL cache; failures are never cached.
type ingressLimitCache struct {
	read ingressLimitReader
	now  func() time.Time

	mu    sync.Mutex
	cache map[string]ingressLimitEntry
}

// ingressLimitEntry 是一条缓存：有效期与该调用方的覆盖值。
type ingressLimitEntry struct {
	budget  callers.IngressBudget
	found   bool
	expires time.Time
}

// newIngressLimitCache 构造缓存；`now` 为空时用 `time.Now`，与其余装配点的时钟
// 注入一致（测试用假时钟验证 TTL 与失败不缓存）。
// newIngressLimitCache builds the cache; a nil clock means time.Now.
func newIngressLimitCache(read ingressLimitReader, now func() time.Time) *ingressLimitCache {
	if now == nil {
		now = time.Now
	}
	return &ingressLimitCache{
		read:  read,
		now:   now,
		cache: make(map[string]ingressLimitEntry),
	}
}

// Override 是 `callers.IngressOverrides` 的实现：读口未装配或调用方为空即错误 /
// 无覆盖；缓存命中的结果按快照返回，读失败**不**写缓存。
// Override is the limiter-side port; a missing reader is an assembly error and read
// failures are propagated without being cached.
func (c *ingressLimitCache) Override(
	ctx context.Context, callerID string,
) (callers.IngressBudget, bool, error) {
	if c == nil || c.read == nil {
		return callers.IngressBudget{}, false, errIngressOverridesUnconfigured
	}
	if callerID == "" {
		return callers.IngressBudget{}, false, nil
	}
	now := c.now()

	c.mu.Lock()
	entry, hit := c.cache[callerID]
	c.mu.Unlock()
	if hit && now.Before(entry.expires) {
		return entry.budget, entry.found, nil
	}

	readCtx, cancel := context.WithTimeout(ctx, ingressLimitReadTimeout)
	defer cancel()
	budget, found, err := c.read(readCtx, callerID)
	if err != nil {
		// 看不见覆盖 → 不放行；不写缓存，下一个请求重新读。
		// The store cannot answer, so admission fails closed and the next call retries.
		return callers.IngressBudget{}, false, err
	}

	entry = ingressLimitEntry{budget: budget, found: found, expires: now.Add(ingressLimitTTL)}
	c.mu.Lock()
	c.cache[callerID] = entry
	c.mu.Unlock()
	return entry.budget, entry.found, nil
}
