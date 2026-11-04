package callers

// 本文件是调用方准入的前两段（图 6 [1] [3] / C-35 / FR-033 / FR-034 / FR-036）：
// 三类入口的令牌桶与 `Limiter.AdmitIngress`，以及调用方并发名额与
// `Limiter.AdmitRun` / `ReleaseRun`。入口速率**只在 HTTP 边界**扣——认证之后、
// 路由处理之前，每个已认证业务请求一次；Run 内部的 Chat / Embedding / 重排不再扣，
// 调度与恢复没有 HTTP 入口也**不得**扣。把速率判定挪进 `AdmitRun` 会让后台任务按
// Run 数吃 HTTP 桶，同时让摄取上传漏检——这正是 R5 拆开三段的原因。
// This file covers admission stages [2] and [3]: the three ingress token buckets
// with AdmitIngress, plus the per-caller concurrency slots with AdmitRun /
// ReleaseRun. Ingress rate is charged only at the HTTP boundary, once per
// authenticated business request; internal calls and background triggers never
// recharge it.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// ErrRateLimited 表示入口速率超限；对外映射 429 CALLER_RATE_LIMITED + Retry-After。
// ErrRateLimited maps to 429 CALLER_RATE_LIMITED with a Retry-After header.
var ErrRateLimited = errors.New("callers: rate limited")

// errLimiterUnconfigured 是装配错误：缺少令牌桶端口时拒绝请求，**不得**静默放行。
// errLimiterUnconfigured is an assembly error; a missing bucket fails closed.
var errLimiterUnconfigured = errors.New("callers: ingress rate bucket is not configured")

// errUnknownIngressClass 表示调用方传入了闭集之外的入口类别。启动期路由映射会先
// 拒绝未知类别，运行期再出现即程序错误，按失败关闭处理。
// errUnknownIngressClass is a programming error after startup route mapping; it still
// fails closed rather than guessing a class.
var errUnknownIngressClass = errors.New("callers: unknown ingress class")

// ErrConcurrencyLimited 表示调用方在跑的 Run 数已达上限；对外映射 429
// CALLER_CONCURRENCY_LIMITED。它与速率、配额是三个独立判据（C-35），不得合并。
// ErrConcurrencyLimited maps to 429 CALLER_CONCURRENCY_LIMITED and stays
// independent of both rate and quota.
var ErrConcurrencyLimited = errors.New("callers: caller concurrency limit reached")

// errConcurrencyUnconfigured 是装配错误：缺少并发端口时拒绝请求，**不得**放行。
// errConcurrencyUnconfigured fails closed when the concurrency port is missing.
var errConcurrencyUnconfigured = errors.New("callers: concurrency port is not configured")

// errInvalidConcurrencyLimit 表示限额为负——配置或存储行已损坏，按错误处理而不是
// 当成零或无穷大。
// errInvalidConcurrencyLimit marks a negative limit as a configuration/store defect.
var errInvalidConcurrencyLimit = errors.New("callers: caller concurrency limit is negative")

// errInvalidRunTicket 表示票据缺少归属或 RunKey；零值票据从未被签发，不得退还。
// errInvalidRunTicket rejects a ticket that was never issued.
var errInvalidRunTicket = errors.New("callers: run ticket is missing its caller or key")

// errRunTicketMismatch 表示票据的归属与签发时不一致；拒绝退还以暴露串票错误，
// 避免把别人的名额还给当前调用方。
// errRunTicketMismatch refuses to return another caller's slot.
var errRunTicketMismatch = errors.New("callers: run ticket belongs to another caller")

// IngressClass 是三类入口之一：Run / Chat / 会话写、上传与摄取触发、读端点。
// 管理面请求不进入任何调用方桶；未知类别**不得**放行。
// IngressClass is one of the three ingress classes; admin traffic never enters a
// caller bucket, and an unknown class never passes.
type IngressClass string

const (
	// IngressRun 覆盖 Run / Chat / 会话写。
	// IngressRun covers runs, chat and session writes.
	IngressRun IngressClass = "run"
	// IngressIngest 覆盖上传与摄取触发。
	// IngressIngest covers uploads and ingest triggers.
	IngressIngest IngressClass = "ingest"
	// IngressRead 覆盖历史、来源、列表等读端点。
	// IngressRead covers history, source and list reads.
	IngressRead IngressClass = "read"
)

// Valid 判定入口类别是否在闭集内；空值与未知值一律拒绝。
// Valid accepts exactly the three literals; the zero value and unknown values fail.
func (c IngressClass) Valid() bool {
	switch c {
	case IngressRun, IngressIngest, IngressRead:
		return true
	default:
		return false
	}
}

// IngressBudget 是单个入口类的令牌桶参数：QPS 是每调用方稳态速率，Burst 是突发
// 额度。阈值随 D-30 容量实测冻结，代码里**不得**预填默认值。QPS 用 float64 而不是
// int：调用方覆盖来自 `caller_limits.qps numeric(10,3)`（C-F01），0.5 这类小数速率
// 必须原样生效——截断成 0 会变成"全拒"、截断成 1 会放宽一倍，两种都不对。
// IngressBudget is one ingress class's token bucket; thresholds are frozen by D-30
// and never defaulted. QPS is fractional so a per-caller override such as 0.5 stays
// exact instead of truncating to 0 (deny-all) or 1 (twice as loose).
type IngressBudget struct {
	QPS   float64
	Burst int
}

// IngressLimits 是三类入口各自的预算，由装配层从 `callers.ingress.*` 配置读入。
// IngressLimits carries the three classes' budgets read from callers.ingress.*.
type IngressLimits struct {
	Run    IngressBudget
	Ingest IngressBudget
	Read   IngressBudget
}

// budgetFor 返回类别对应的预算；未知类别返回 false，调用方必须按拒绝处理。
// budgetFor returns the class budget; an unknown class reports false.
func (l IngressLimits) budgetFor(class IngressClass) (IngressBudget, bool) {
	switch class {
	case IngressRun:
		return l.Run, true
	case IngressIngest:
		return l.Ingest, true
	case IngressRead:
		return l.Read, true
	default:
		return IngressBudget{}, false
	}
}

// validate 拒绝负数参数：配置层已做非负校验，这里再挡一次装配错误。
// Zero budgets are legal and deny everything; negative values are assembly errors.
func (l IngressLimits) validate() error {
	classes := []struct {
		name   string
		budget IngressBudget
	}{
		{"run", l.Run},
		{"ingest", l.Ingest},
		{"read", l.Read},
	}
	for _, class := range classes {
		if class.budget.QPS < 0 || class.budget.Burst < 0 {
			return fmt.Errorf("callers: ingress.%s 的 qps / burst 不得为负", class.name)
		}
	}
	return nil
}

// RateBucket 是单实例内存令牌桶端口，只服务 `AdmitIngress`（图 6 [1]）。
// 重启后计数清零——丢失的只是突发额度，不丢审计或配额；装配层需记录 WARN。
// RateBucket is the single-instance in-memory token bucket used only by
// AdmitIngress; restarting resets the counters and loses nothing but burst credit.
type RateBucket interface {
	// Allow 按入口类别的统一预算判定（无调用方覆盖时使用）。
	Allow(callerID string, class IngressClass) bool

	// AllowWith 在类别预算之上叠加调用方覆盖（C-F01）：`override` 为 nil 或零列
	// 表示不覆盖；覆盖只能收紧、**不得**放宽类别上限（入口统一限流是外闸）。
	// AllowWith applies the caller override on top of the class budget; the override
	// can only narrow the class ceiling, never widen it.
	AllowWith(callerID string, class IngressClass, override *IngressBudget) bool
}

// IngressOverrides 是调用方入口速率的覆盖读口（C-F01）：`caller_limits.qps` /
// `burst` 两列的投影。实现**必须**同步、有界并在读失败时返回错误——"看不见覆盖"
// 不得当成"没有覆盖"，更不得当成"不限"（裁决第 5 条）。覆盖按 CallerID 读，不按
// KeyID：同一调用方轮换 Key 后配额**不得**重置。
// IngressOverrides projects caller_limits.qps/burst for admission stage [2]; a read
// failure must surface as an error, never as "no override" or "unlimited".
type IngressOverrides interface {
	// Override 返回该调用方的覆盖预算；`found=false` 表示没有生效的覆盖（没有
	// `caller_limits` 行，或两列都为 0）。负数按数据缺陷返回错误。
	// Override reports the caller's override; found=false means none applies and a
	// negative value is a data defect.
	Override(ctx context.Context, callerID string) (IngressBudget, bool, error)
}

// rateBucketKey 以「调用方 + 入口类别」为桶粒度：三类额度互不挤占，调用方之间
// 也不共享——一个调用方打满摄取桶不得影响自己的读端点，更不得影响别人。
// Buckets are keyed by caller and class so one caller's ingest burst cannot starve
// its reads or another caller.
type rateBucketKey struct {
	callerID string
	class    IngressClass
}

// rateBucketState 是单个桶的令牌数与上次回填时刻。tokens 用 float64 保存，
// 回填按经过的秒数乘以 QPS 精确累加，避免整数截断把低频调用饿死。
// rateBucketState keeps the remaining tokens and the last refill instant; refills
// use seconds as float64 so low rates are not starved by integer truncation.
type rateBucketState struct {
	tokens float64
	last   time.Time
}

// memoryRateBucket 是单实例内存实现：map + 互斥锁，进程内共享。
// 桶不主动回收——调用方数量由管理面控制，进程生命周期内的键空间有界。
// memoryRateBucket is the in-memory implementation; keys live for the process
// lifetime and are bounded by the number of callers.
type memoryRateBucket struct {
	mu      sync.Mutex
	limits  IngressLimits
	now     func() time.Time
	buckets map[rateBucketKey]*rateBucketState
}

// NewRateBucket 构造单实例内存令牌桶。`now` 为 nil 时使用 time.Now；测试注入
// 假时钟验证回填，不靠 sleep 赌时序。负数预算按装配错误拒绝，零预算合法且拒绝
// 全部请求（fail-closed）。
// NewRateBucket builds the in-memory bucket; a nil clock means time.Now, negative
// budgets are assembly errors, and zero budgets deny everything.
func NewRateBucket(limits IngressLimits, now func() time.Time) (RateBucket, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &memoryRateBucket{
		limits:  limits,
		now:     now,
		buckets: make(map[rateBucketKey]*rateBucketState),
	}, nil
}

// Allow 扣一次令牌，等价于不带调用方覆盖的 AllowWith。
// Allow charges one token and is AllowWith without a caller override.
func (b *memoryRateBucket) Allow(callerID string, class IngressClass) bool {
	return b.AllowWith(callerID, class, nil)
}

// tightenIngressBudget 把调用方覆盖收紧到类别预算之下（C-F01）：入口统一限流是外闸，
// `caller_limits.qps` / `burst` 只能进一步收紧、**不得**放宽。两列的 0 都表示"该列
// 不覆盖"（契约 `schema.pg.sql` 的 `0 = 不限` 指不额外限制，不是绕过类别预算）；
// 表中已有 `CHECK (>= 0)`，负值到不了这里，存储层读到即按数据缺陷报错。
// tightenIngressBudget only ever narrows the class budget; a zero column means
// "no override for that column", never "unlimited".
//
// 逆改点：把覆盖写成直接替换（`out = *override`）→ `TestRateBucket_OverrideCannotWiden`
// 红（类别 1 QPS 被 100 覆盖）；把 0 当成"该列取 0"→ 只配 qps 的调用方突发额度
// 归零，`TestRateBucket_OverrideZeroColumnKeepsClassValue` 红。
// Mutation: replacing instead of narrowing lets a caller widen the class ceiling.
func tightenIngressBudget(class IngressBudget, override *IngressBudget) IngressBudget {
	if override == nil {
		return class
	}
	out := class
	if override.QPS > 0 && override.QPS < out.QPS {
		out.QPS = override.QPS
	}
	if override.Burst > 0 && override.Burst < out.Burst {
		out.Burst = override.Burst
	}
	return out
}

// AllowWith 扣一次令牌：首次请求以满桶开始，之后按经过时间回填到**生效**预算的
// burst 上限（生效预算 = 类别预算按调用方覆盖收紧）。空调用方、未知类别与零预算
// 一律拒绝——参数缺失不得退化成"不限流"；类别预算未配置（0）时覆盖也救不回来，
// 入口统一限流是外闸。
// AllowWith consumes one token against the effective budget; empty callers,
// unknown classes and zero budgets deny, and an override never rescues a class
// budget that was left unconfigured.
func (b *memoryRateBucket) AllowWith(callerID string, class IngressClass, override *IngressBudget) bool {
	if callerID == "" {
		return false
	}
	classBudget, ok := b.limits.budgetFor(class)
	if !ok {
		return false
	}
	budget := tightenIngressBudget(classBudget, override)
	if budget.QPS <= 0 || budget.Burst <= 0 {
		return false
	}
	now := b.now()

	b.mu.Lock()
	defer b.mu.Unlock()

	key := rateBucketKey{callerID: callerID, class: class}
	state, exists := b.buckets[key]
	if !exists {
		state = &rateBucketState{tokens: float64(budget.Burst), last: now}
		b.buckets[key] = state
	} else if elapsed := now.Sub(state.last).Seconds(); elapsed > 0 {
		state.tokens = math.Min(float64(budget.Burst), state.tokens+elapsed*budget.QPS)
		state.last = now
	}
	if state.tokens < 1 {
		return false
	}
	state.tokens--
	return true
}

// Concurrency 是调用方并发名额端口（图 6 [3]）：Acquire 每个 Run 一次，
// Release 在所有退出路径调用。计数与取限都按 CallerID——同一调用方的多个 Key
// 共享额度，轮换 / 吊销单个 Key **不得**重置或清零并发占用。
// Concurrency counts running slots per CallerID; keys of one caller share the limit.
type Concurrency interface {
	// Acquire 尝试占用一个名额：上下文已取消 / 超时、限额缺失或为零、额度已满
	// 都返回错误且**不占用**名额。限额为负按配置缺陷返回错误。
	Acquire(ctx context.Context, callerID string) error

	// Release 归还一个名额；对没有占用的调用方为空操作（不出现负计数）。
	Release(callerID string)
}

// FlowConcurrency extends the same slot table, atomically checking total and
// Flow occupancy. It must not be implemented with a separate semaphore.
type FlowConcurrency interface {
	AcquireFlow(ctx context.Context, callerID string, interactiveReserved int) error
	ReleaseFlow(callerID string)
}

// RunTicket 是 Run 并发票据：只承载名额，不含任何配额预留，**不得**当作
// `Ticket` 结算。RunKey 是进程内唯一标识，重复退还同一票据为空操作。
// RunTicket carries only the slot; RunKey makes repeated releases a no-op.
type RunTicket struct {
	// CallerID 是占用名额的调用方（额度按调用方计，不按 Key 计）。
	CallerID string
	// RunKey 是本次 Run 的进程内唯一标识，只用于退还幂等。
	RunKey string
}

// memoryConcurrency 是单实例内存名额表：按 CallerID 计数，限额由装配注入的
// 查询函数提供（生产实现读 `caller_limits.max_concurrent` 的缓存投影）。进程
// 重启后计数清零——崩溃遗留的 Run 已随进程消失，不占新进程的名额。
// memoryConcurrency is the in-process slot table; limits come from an injected
// lookup and restarting clears counts because the crashed runs are gone too.
type memoryConcurrency struct {
	mu        sync.Mutex
	limit     func(callerID string) (int, bool)
	inUse     map[string]int
	flowInUse map[string]int
}

// NewMemoryConcurrency 构造内存名额表。`limit` 为 nil 是装配错误；限额缺失或为
// 零时**拒绝全部**（fail-closed），不得退化成不限并发。
// NewMemoryConcurrency builds the slot table; a nil lookup is an assembly error,
// while a missing or zero limit denies every run.
func NewMemoryConcurrency(limit func(callerID string) (int, bool)) (Concurrency, error) {
	if limit == nil {
		return nil, errConcurrencyUnconfigured
	}
	return &memoryConcurrency{limit: limit, inUse: make(map[string]int), flowInUse: make(map[string]int)}, nil
}

// Acquire 先判上下文与身份，再取限、最后在锁内判定并计数：取消 / 超时的请求
// 不占名额，未知调用方与零限额一律拒绝。
// Acquire checks context and identity before taking a slot, and denies unknown
// callers or zero limits rather than defaulting to unlimited.
func (c *memoryConcurrency) Acquire(ctx context.Context, callerID string) error {
	return c.acquire(ctx, callerID, 0, false)
}

func (c *memoryConcurrency) AcquireFlow(ctx context.Context, callerID string, reserved int) error {
	if reserved < 0 {
		return errInvalidConcurrencyLimit
	}
	return c.acquire(ctx, callerID, reserved, true)
}

func (c *memoryConcurrency) acquire(ctx context.Context, callerID string, reserved int, flow bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if callerID == "" {
		return core.ErrUnauthorized
	}
	limit, ok := c.limit(callerID)
	if !ok {
		return ErrConcurrencyLimited
	}
	if limit < 0 {
		return errInvalidConcurrencyLimit
	}
	if limit == 0 {
		return ErrConcurrencyLimited
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.inUse[callerID] >= limit || flow && (reserved >= limit || c.flowInUse[callerID] >= limit-reserved) {
		return ErrConcurrencyLimited
	}
	c.inUse[callerID]++
	if flow {
		c.flowInUse[callerID]++
	}
	return nil
}

// Release 归还名额；计数归零时删除键，避免表随历史调用方无限增长。
// Release returns the slot and drops the key at zero to keep the map bounded.
func (c *memoryConcurrency) Release(callerID string) {
	c.release(callerID, false)
}

func (c *memoryConcurrency) ReleaseFlow(callerID string) { c.release(callerID, true) }

func (c *memoryConcurrency) release(callerID string, flow bool) {
	if callerID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if flow {
		if c.flowInUse[callerID] == 0 {
			return
		}
		c.flowInUse[callerID]--
		if c.flowInUse[callerID] == 0 {
			delete(c.flowInUse, callerID)
		}
	} else if c.inUse[callerID] <= c.flowInUse[callerID] {
		return
	}
	if c.inUse[callerID] <= 1 {
		delete(c.inUse, callerID)
		return
	}
	c.inUse[callerID]--
}

// LimiterDeps 是 Limiter 的装配依赖：入口令牌桶、并发名额端口与配额端口。三段
// 各自独立（C-35），**不得**把速率判定挪进并发或配额。
// LimiterDeps carries the three independent admission ports; they are never folded
// into each other.
type LimiterDeps struct {
	Buckets     RateBucket
	Concurrency Concurrency
	// Quotas 是 [5] 的配额端口（`postgres.NewQuotaStore`）；缺失时 `AdmitCall`
	// 失败关闭。
	// Quotas is stage [5]; a missing port fails AdmitCall closed.
	Quotas QuotaStore
	// LocalAdminUnlimited 对应 `callers.local_admin.unlimited`：为真时内置管理员
	// 的 `AdmitCall` 不写预留行；**不**绕过并发名额、超时或审计。
	// LocalAdminUnlimited lets the built-in admin skip reservation rows only.
	LocalAdminUnlimited bool
	// IngressOverrides 是 [2] 的调用方覆盖读口（C-F01）。装配层**必须**接上它，
	// 否则 `caller_limits.qps/burst` 配了也不生效；为 nil 时只剩类别预算（测试与
	// 不接库的场景），入口统一限流仍在。
	// IngressOverrides is the [2] override reader; nil leaves only the class budget.
	IngressOverrides IngressOverrides
}

// Limiter 是准入限制器。`AdmitIngress` 只扣令牌桶，`AdmitRun` 只占名额，
// `AdmitCall` 只预留配额；调用顺序由 HTTP 中间件与 `RunService` 保证。
// Limiter owns the three admission stages; none of them touches another's budget.
type Limiter struct {
	buckets     RateBucket
	concurrency Concurrency
	quotas      QuotaStore
	// ingressOverrides 是 [2] 的调用方覆盖读口（C-F01），可为 nil。
	ingressOverrides IngressOverrides
	// localAdminUnlimited 对应 `callers.local_admin.unlimited`。
	localAdminUnlimited bool

	// mu 保护 active 与 nextRun：active 记录已签发且尚未退还的票据，
	// 既提供退还幂等，又让键空间随在跑 Run 数有界。
	mu      sync.Mutex
	active  map[string]runTicketOwner
	nextRun uint64
}

type runTicketOwner struct {
	callerID string
	flow     bool
}

// NewLimiter 构造限制器；缺少端口时仍返回实例，由各 Admit 方法失败关闭，
// 这样装配错误不会在构造处被吞成 nil 解引用。
// NewLimiter always returns an instance; a missing port fails closed on use.
func NewLimiter(deps LimiterDeps) *Limiter {
	return &Limiter{
		buckets:             deps.Buckets,
		concurrency:         deps.Concurrency,
		quotas:              deps.Quotas,
		ingressOverrides:    deps.IngressOverrides,
		localAdminUnlimited: deps.LocalAdminUnlimited,
		active:              make(map[string]runTicketOwner),
	}
}

// AdmitIngress 承担图 6 的 [1]：每个已认证业务请求调用一次，与是否创建 Run 无关。
// 只扣令牌桶，不占名额、不预留配额、不可返还。零值 Principal 一律拒绝，**不得**
// 默认成管理员；未知类别与缺失端口按失败关闭处理。调用方覆盖（`caller_limits`
// 的 `qps` / `burst`，C-F01）在扣桶前读取并**只收紧**类别预算；读失败按错误上交
// ——**不得**映射成 429（那是真实限流），中间件对未知错误记 WARN 并回 500。
// AdmitIngress is admission stage [2]. The caller override is read before charging
// and can only narrow the class budget; a read failure surfaces as a non-429 error.
//
// 逆改点：把本方法挪进 AdmitRun → 后台调度按 Run 扣 HTTP 速率、HTTP 摄取无人守；
// 在 Run 内部每次模型调用上再调一次 → 多步 Run 自己把速率吃光；把覆盖读失败吞成
// "没有覆盖" → 库不可达时覆盖静默失效（`TestLimiter_AdmitIngressOverrideReadFailure`
// 红）。
// Mutation: moving this into AdmitRun lets background triggers drain the HTTP bucket
// while ingest goes unthrottled; swallowing an override read failure silently
// disables the enforcement.
func (l *Limiter) AdmitIngress(ctx context.Context, p core.Principal, class IngressClass) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.Valid() {
		return core.ErrUnauthorized
	}
	if !class.Valid() {
		return errUnknownIngressClass
	}
	if l == nil || l.buckets == nil {
		return errLimiterUnconfigured
	}
	override, err := l.ingressOverride(ctx, p.CallerID)
	if err != nil {
		return err
	}
	if !l.buckets.AllowWith(p.CallerID, class, override) {
		return ErrRateLimited
	}
	return nil
}

// ingressOverride 读取调用方的入口覆盖。未装配读口时返回 `nil, nil`——类别预算
// 照旧生效，不是"不限"；读失败原样包装上抛（**不**降级为"无覆盖"），由调用链的
// 中间件记 WARN 并回 500。
// ingressOverride returns nil when the port is absent; read failures are wrapped
// and propagated rather than degraded to "no override".
func (l *Limiter) ingressOverride(ctx context.Context, callerID string) (*IngressBudget, error) {
	if l == nil || l.ingressOverrides == nil {
		return nil, nil
	}
	budget, found, err := l.ingressOverrides.Override(ctx, callerID)
	if err != nil {
		return nil, fmt.Errorf("callers: 读取调用方入口预算: %w", err)
	}
	if !found {
		return nil, nil
	}
	return &budget, nil
}

// AdmitRun 承担图 6 的 [3]：每个 Run 调用一次，由 `RunService` 在 [4]（会话
// 串行 / 实例容量）之前调用。只占并发名额，不扣入口速率、不预留配额；被 [2]
// 拒绝的请求根本到不了这里，因此不会占名额。
// AdmitRun is stage [3]: once per run, before the existing session-serial check.
// It only takes a slot and never touches ingress rate or quotas.
//
// 逆改点：在 AdmitCall 里再调 Acquire → 同一 Run 的第二次 Chat 撞自己的名额；
// 把速率判定挪进来 → 后台调度按 Run 吃 HTTP 桶；先占名额再做身份判定 → 零值
// Principal 也能占名额。
// Mutation: acquiring again per call makes a run collide with its own slot; moving
// rate here drains the HTTP bucket from background triggers; checking identity
// after acquiring lets zero principals take slots.
func (l *Limiter) AdmitRun(ctx context.Context, p core.Principal) (RunTicket, error) {
	return l.admitRun(ctx, p, false, 0)
}

// AdmitFlowRun shares ordinary per-caller counters and reserves interactive room.
// Scope remains explicit; adding this method grants nothing to existing keys.
func (l *Limiter) AdmitFlowRun(ctx context.Context, p core.Principal, reserved int) (RunTicket, error) {
	if !p.Valid() || !p.HasScope(core.ScopeFlowsWrite) {
		return RunTicket{}, core.ErrUnauthorized
	}
	return l.admitRun(ctx, p, true, reserved)
}

func (l *Limiter) admitRun(ctx context.Context, p core.Principal, flow bool, reserved int) (RunTicket, error) {
	if err := ctx.Err(); err != nil {
		return RunTicket{}, err
	}
	if !p.Valid() {
		return RunTicket{}, core.ErrUnauthorized
	}
	if l == nil || l.concurrency == nil {
		return RunTicket{}, errConcurrencyUnconfigured
	}
	var acquireErr error
	if flow {
		port, ok := l.concurrency.(FlowConcurrency)
		if !ok {
			return RunTicket{}, errConcurrencyUnconfigured
		}
		acquireErr = port.AcquireFlow(ctx, p.CallerID, reserved)
	} else {
		acquireErr = l.concurrency.Acquire(ctx, p.CallerID)
	}
	if acquireErr != nil {
		return RunTicket{}, acquireErr
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextRun++
	key := fmt.Sprintf("run_%d", l.nextRun)
	l.active[key] = runTicketOwner{callerID: p.CallerID, flow: flow}
	return RunTicket{CallerID: p.CallerID, RunKey: key}, nil
}

// ReleaseRun 归还 [3] 的名额。**必须**由生产路径（`RunService` / 调度器）以
// `defer` 调用，覆盖正常完成、失败、取消、[4] 拒绝、配额拒绝与 panic；同一票据
// 重复调用为空操作。与配额结算无关：`pending` 预留不占名额，也不因退还而释放。
// ReleaseRun returns the slot from a deferred call on every exit path, including
// panics. Repeating a ticket is a no-op and quotas are never touched here.
//
// 本方法**不**检查 ctx.Err()：Run 在取消后收尾时 ctx 已取消，若先判取消再归还，
// 取消路径会永久泄漏名额。ctx 只承载追踪标识。
// It deliberately ignores ctx.Err(): a canceled run still owes its slot back, and
// consulting cancellation first would leak the slot on every cancel.
func (l *Limiter) ReleaseRun(ctx context.Context, t RunTicket) error {
	if l == nil || l.concurrency == nil {
		return errConcurrencyUnconfigured
	}
	if t.RunKey == "" || t.CallerID == "" {
		return errInvalidRunTicket
	}

	l.mu.Lock()
	owner, issued := l.active[t.RunKey]
	if !issued {
		l.mu.Unlock()
		return nil // 重复退还：票据已归还过，空操作
	}
	if owner.callerID != t.CallerID {
		l.mu.Unlock()
		return errRunTicketMismatch
	}
	delete(l.active, t.RunKey)
	l.mu.Unlock()

	if owner.flow {
		port, ok := l.concurrency.(FlowConcurrency)
		if !ok {
			return errConcurrencyUnconfigured
		}
		port.ReleaseFlow(t.CallerID)
	} else {
		l.concurrency.Release(t.CallerID)
	}
	return nil
}
