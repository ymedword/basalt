package app

// 本文件是图 6 [3]「调用方并发」的**限额来源**：把 `caller_limits.max_concurrent`
// 投影成 `callers.NewMemoryConcurrency` 需要的同步查询函数。名额表本身在
// `internal/callers`（进程内计数、退还幂等），本文件只回答"这个调用方最多几个"。
// This file supplies the per-caller slot limit for admission stage [3]; the slot
// table itself lives in internal/callers and this layer only answers "how many".
//
// 三档语义（本 US 内的裁决，记录见 specs/016-knowledge-memory-history/run-state.md）：
//   - 行存在且 `max_concurrent > 0` → 该调用方的每调用方上限；
//   - 行存在但为 0，或根本没有这一行 → 回退到实例级 `server.max_concurrent_runs`
//     （配置 0 → `core.DefaultMaxConcurrentRuns`）。理由有二：① 卡 §2.3 要求内置
//     管理员的 `unlimited=true` **仍**受并发上限约束，而 `seedBuiltinCaller` 只建
//     `callers` 行、不建 `caller_limits` 行——没有回退就等于"内置管理员永远起不了
//     Run"；② 会话压缩、历史读取等既有测试里的调用方同样没有限额行，把"未配置"
//     一律读成拒绝会让既有链路整段停摆，而 0 在兄弟列（`qps` / `burst`）里明确的
//     语义就是"不限"。
//   - **读存储失败 → 失败关闭**（返回 ok=false，名额表拒绝）：这是 C-02 的口径，
//     "看不见上限"不得退化成"不限并发"。
//
// 缓存：限额是低频写、高频读的配置。查询结果按 `concurrencyLimitTTL` 缓存，管理面
// 改完最迟一个 TTL 后生效（`SetLimit` 只写库，跨进程无失效广播——这一点在验收报告
// 的"未覆盖面"里登记）。读失败**不**缓存，下一次 Acquire 会立刻重试。

import (
	"context"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// concurrencyLimitTTL 是限额投影的缓存有效期。取值只影响"管理面改限额后多久
	// 生效"，不影响额度正确性：名额表每次 Acquire 都读这份投影。
	concurrencyLimitTTL = 10 * time.Second

	// concurrencyLimitReadTimeout 是单次投影读取的截止时间。`Acquire` 是同步准入
	// 路径，查询卡住不得拖住整个 Run 的准入。
	concurrencyLimitReadTimeout = 2 * time.Second
)

// concurrencyLimitReader 是 `postgres.QuotaStore.MaxConcurrent` 的方法值形态。
// 用函数类型而不是把存储类型存进结构体：`postgres` 的配额存储不导出具体类型，
// 装配层只需要这一个读点，不需要为它新增导出类型。
// The reader is the method value of the quota store, so no exported store type is
// needed just to read one column.
type concurrencyLimitReader func(ctx context.Context, callerID string) (int, bool, error)

// concurrencyLimitCache 把存储读点包成 `callers.NewMemoryConcurrency` 要求的
// `func(callerID string) (int, bool)`：带 TTL 缓存、读失败失败关闭、缺省回退到
// 实例级上限。
// concurrencyLimitCache adapts the store read into the slot table's lookup.
type concurrencyLimitCache struct {
	read     concurrencyLimitReader
	fallback int
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]concurrencyLimitEntry
}

// concurrencyLimitEntry 是一条缓存：有效期与该调用方的上限。
type concurrencyLimitEntry struct {
	limit   int
	ok      bool
	expires time.Time
}

// newConcurrencyLimitCache 构造缓存。`fallback <= 0` 时取 core 的实例级缺省值——
// 与 `server.max_concurrent_runs` 的 0 语义（交给 core 落缺省）保持同一口径。
// `now` 为空时用 `time.Now`，与其余装配点的时钟注入一致。
func newConcurrencyLimitCache(
	read concurrencyLimitReader, fallback int, now func() time.Time,
) *concurrencyLimitCache {
	if fallback <= 0 {
		fallback = core.DefaultMaxConcurrentRuns
	}
	if now == nil {
		now = time.Now
	}
	return &concurrencyLimitCache{
		read:     read,
		fallback: fallback,
		now:      now,
		cache:    make(map[string]concurrencyLimitEntry),
	}
}

// limit 是 `callers.NewMemoryConcurrency` 的查询函数：返回值 **必须** 是
// "已配置且可用"的限额，`ok=false` 会让名额表直接拒绝（fail-closed）。
// 存储读失败与负值都在这里定档：前者拒绝，后者原样上交由名额表报"配置缺陷"。
// limit is the lookup handed to the slot table; ok=false denies the run outright.
func (c *concurrencyLimitCache) limit(callerID string) (int, bool) {
	if c == nil || c.read == nil || callerID == "" {
		return 0, false
	}
	now := c.now()

	c.mu.Lock()
	entry, hit := c.cache[callerID]
	c.mu.Unlock()
	if hit && now.Before(entry.expires) {
		return entry.limit, entry.ok
	}

	ctx, cancel := context.WithTimeout(context.Background(), concurrencyLimitReadTimeout)
	defer cancel()
	value, configured, err := c.read(ctx, callerID)
	if err != nil {
		// 看不见限额 → 不放行；不写缓存，下一次重新读。
		// The store cannot answer, so admission fails closed and the next call retries.
		return 0, false
	}

	var result concurrencyLimitEntry
	switch {
	case configured && value < 0:
		// 负值是配置缺陷：原样上交，名额表按 `errInvalidConcurrencyLimit` 报错，
		// **不得**在这里悄悄改成缺省值。
		result = concurrencyLimitEntry{limit: value, ok: true}
	case configured && value > 0:
		result = concurrencyLimitEntry{limit: value, ok: true}
	default:
		// 无行或 0 → 实例级上限兜底（见文件头三档语义）。
		result = concurrencyLimitEntry{limit: c.fallback, ok: true}
	}
	result.expires = now.Add(concurrencyLimitTTL)

	c.mu.Lock()
	c.cache[callerID] = result
	c.mu.Unlock()
	return result.limit, result.ok
}
