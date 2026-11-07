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
//
// US-24 P-06 起本文件供数的名额表**降为预检**：它给出的判定只是"早失败"的快路径，
// 不跨进程；权威判定在 `StartRun` 的同一事务内（调用方 advisory 锁后
// `checkCallerConcurrency` 按 `runs` 表 running 行复查，FR-24-08）。因此本文件不承诺
// 集群级并发上限，也不得据此声称跨 Worker 名额已被强制——两个 Worker 各自的预检
// 可以同时通过，最后由数据库那次复查裁决（拒绝方拿到
// `core.ErrCallerConcurrencyLimited`，经 callers 别名映射 429）。
// Since US-24 P-06 this lookup only feeds the precheck: it is a per-process fast
// path. The authoritative verdict is the in-transaction recheck in StartRun, which
// counts running rows in the shared database.

import (
	"context"
	"errors"
	"fmt"
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

// —— `worker.max_inflight` 池化名额（US-24 FR-24-40 / 模块卡 §2.3.11 [1]）——
//
// 三条执行路径共用**同一个**信号量：① HTTP / CLI 的交互 Run、② 调度器领到的执行组、
// ③ Flow 节点 Run。三者最终都经过 `runAdmission.run`，池化名额因此只有这一个取槽点。
// One pooled semaphore is shared by the three execution paths; they all funnel through
// runAdmission.run, so this is the only place a slot is taken.

// errShutdownDraining 表示本进程已进入停机排空（图 11 [2] 之后）：仍在排队等槽的
// 后台执行不再等待。它不是对外的错误码——排空期间本来就不该再有合法的新领取，
// 这条错误只让等待者立刻退出，而不是把停机的 45 秒耗在队列里。
// errShutdownDraining releases background executions still queued for a slot once the
// process starts draining; no legitimate new claim can start after that point.
var errShutdownDraining = errors.New("shutdown draining: max_inflight pool closed")

// inflightLimiter 是池化在途名额。`nil` 表示未接线（轻装配与既有单测）：不加判据、
// 行为与接线前逐字一致。
// inflightLimiter is the pooled in-flight budget; nil means "not wired".
type inflightLimiter struct {
	slots chan struct{}
	limit int
	// drained 在停机进入排空时关闭：等待中的后台 goroutine 据此立刻返回，而不是
	// 等到执行 ctx 被取消（那要等到宽限耗尽）。
	// drained is closed when the drain phase starts, so queued background work bails out
	// immediately instead of waiting for the grace to expire.
	drained   chan struct{}
	drainOnce sync.Once
}

// newInflightLimiter 构造名额池；`limit <= 0` 返回 nil（= 不限、不接线），与
// `worker.max_inflight` 的配置校验（1..512）保持同一口径。
// newInflightLimiter returns nil for a non-positive limit, matching the config contract.
func newInflightLimiter(limit int) *inflightLimiter {
	if limit <= 0 {
		return nil
	}
	return &inflightLimiter{
		slots:   make(chan struct{}, limit),
		limit:   limit,
		drained: make(chan struct{}),
	}
}

// acquireInteractive 给交互 Run 取槽：**非阻塞**，满额立即
// `core.InstanceFullError`（HTTP 429 `TOO_MANY_RUNS`，沿用"拒绝、不排队"的既有契约）。
// 取不到槽的 Run 一个字节都没占用：不写 Run 行、不建租约、不记配额。
// acquireInteractive never queues: a full pool rejects immediately with the same
// capacity error the instance cap uses.
func (l *inflightLimiter) acquireInteractive() (func(), error) {
	if l == nil {
		return nil, nil
	}
	select {
	case l.slots <- struct{}{}:
		return l.release, nil
	default:
		return nil, &core.InstanceFullError{Limit: l.limit}
	}
}

// acquireBackground 给后台执行（调度执行组 / Flow 节点 Run）取槽：阻塞等待，
// 可被执行 ctx 取消，也可被停机排空打断。阻塞期间不推进任何状态——调用方拿到
// 名额之前不会开始执行，`next_eligible_at` 之类的推进都在执行内部。
// acquireBackground blocks until a slot frees up, the execution context ends, or the
// drain starts; no state is advanced while queued.
func (l *inflightLimiter) acquireBackground(ctx context.Context) (func(), error) {
	if l == nil {
		return nil, nil
	}
	// 已取消的执行不再取槽：名额取到也只会被立刻归还，先拒绝更诚实。
	// A cancelled execution never takes a slot it will not use.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	// 快路径：有空槽就不进第二段 select，避免与已关闭的 drained 随机二选一。
	// Fast path first, so an idle pool never loses the race against a closed drain.
	select {
	case l.slots <- struct{}{}:
		return l.release, nil
	default:
	}
	select {
	case l.slots <- struct{}{}:
		return l.release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.drained:
		return nil, errShutdownDraining
	}
}

// release 归还名额。与取槽一一对应，由调用方的 defer 保证恰好执行一次。
// release returns the slot; the caller's defer guarantees exactly one call.
func (l *inflightLimiter) release() { <-l.slots }

// drain 关闭排队入口（幂等）：停机 [2] 之后仍在等槽的后台 goroutine 立刻返回。
// 已持槽的执行不受影响——它们照常走完收尾。
// drain closes the queue entry once; holders keep their slots until their closeout ends.
func (l *inflightLimiter) drain() {
	if l == nil {
		return
	}
	l.drainOnce.Do(func() { close(l.drained) })
}

// callerAdmissionResolver 是 `core.CallerAdmissionResolver` 的装配层实现（US-24
// T013 / 模块卡 §3.1.2）：把调用方身份投影成一次准入的判定输入。它读的是
// `concurrencyLimitCache` 的**同一份**投影——预检名额表与事务内权威复查因此不会
// 各拿一份限额视图（C-35 的单实例口径）。
//
// `Enabled=false` 只给内置不限量管理员：与 `callers.Limiter` 的
// `LocalAdminUnlimited` 同一个开关，表示"不占调用方名额桶"，**不**表示跳过实例
// 上限、会话串行或审计。其余调用方一律 `Enabled=true`。
//
// 读失败 **fail-closed**：返回包装后的 `core.ErrCallerConcurrencyLimited`（与名额
// 表"看不见上限即拒绝"同一个哨兵，入口映射 429）。不得退化成 `Enabled=false`——
// 那会把"仓库读不出来"变成"不限并发"。
// The resolver projects a principal onto the admission input using the very same limit
// cache the precheck slot table reads. An unreadable limit fails closed with the shared
// sentinel; only the built-in unlimited admin is exempt from the caller slot bucket.
type callerAdmissionResolver struct {
	limits *concurrencyLimitCache
	// localAdminUnlimited 与 `callers.Limiter` 的同名开关同源（`callers.local_admin.unlimited`，
	// 未写 = true）。为真时内置管理员 `Enabled=false`。
	localAdminUnlimited bool
	// reserved 是交互预留（`flows.interactive_reserved_per_caller`）：Flow 叶 Run 的上限
	// = MaxConcurrent - reserved。nil 时按 1（与 `runAdmission` 的缺省同值）。
	reserved *int
}

var _ core.CallerAdmissionResolver = (*callerAdmissionResolver)(nil)

// For 解析一次准入的判定输入。身份不合法 → `core.ErrUnauthorized`（fail-closed，
// 不回落成"不限量"）。
// For resolves the admission input; an invalid principal is refused, never widened.
func (r *callerAdmissionResolver) For(p core.Principal) (core.CallerAdmission, error) {
	if !p.Valid() {
		return core.CallerAdmission{}, core.ErrUnauthorized
	}
	if r == nil || r.limits == nil {
		// 未接线（轻装配与既有单测）：不占桶、不加判据，行为与接线前逐字一致。
		// Not wired: behaves exactly as before the resolver existed.
		return core.CallerAdmission{}, nil
	}
	if r.localAdminUnlimited && p.Kind == core.PrincipalLocalAdmin {
		return core.CallerAdmission{Enabled: false}, nil
	}
	limit, ok := r.limits.limit(p.CallerID)
	if !ok {
		// 看不见上限 → 拒绝（C-02）。负值原样上交：那是配置缺陷，由事务复查按
		// `MaxConcurrent <= 0` 拒绝，不在这里悄悄改成缺省值。
		// An unreadable limit denies; a negative value is passed through as a config defect.
		return core.CallerAdmission{}, fmt.Errorf("读取调用方并发上限 %q: %w", p.CallerID, core.ErrCallerConcurrencyLimited)
	}
	reserved := 1
	if r.reserved != nil {
		reserved = *r.reserved
	}
	return core.CallerAdmission{Enabled: true, MaxConcurrent: limit, ReservedForInteractive: reserved}, nil
}
