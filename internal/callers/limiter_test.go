package callers

// 本文件覆盖 T021 / T022 的准入验收点（颗粒度文档 §3.1.5 / 图 6 [1] [3] / C-35 /
// FR-033 / FR-034 / FR-036）：三类入口各自独立的令牌桶、每个已认证请求只扣一次、
// 被拒请求不占任何名额；调用方并发名额的获取 / 释放四路径（成功 / 超限 / 取消 /
// 超时）、panic 收口与重复退还幂等；零值身份与未映射类别一律失败关闭。
// The tests pin T021 and T022: independent per-class token buckets, one token per
// authenticated request, zero footprint on rejection, plus slot acquire/release on
// success, limit, cancel and timeout paths, panic cleanup and idempotent release.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// testCallerPrincipal 构造合法调用方身份；scope 只需一个非管理面取值。
// testCallerPrincipal builds a valid caller identity for ingress tests.
func testCallerPrincipal(callerID string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: callerID,
		KeyID:    "key-" + callerID,
		Scopes:   []string{core.ScopeSessionsRead},
		Actor:    core.ActorCaller,
	}
}

// countingRateBucket 记录 Allow 调用次数并放行，用来验证拒绝发生在扣桶之前。
// countingRateBucket counts calls and always allows, proving rejections happen
// before the bucket is touched.
type countingRateBucket struct {
	calls atomic.Int64
}

func (c *countingRateBucket) Allow(string, IngressClass) bool {
	c.calls.Add(1)
	return true
}

// AllowWith 与 Allow 同口径：记录调用次数并放行（覆盖判定本身由真实桶判）。
func (c *countingRateBucket) AllowWith(string, IngressClass, *IngressBudget) bool {
	c.calls.Add(1)
	return true
}

// recordingConcurrency 记录名额获取 / 归还次数：用来验证拒绝发生在占名额之前、
// 以及重复退还只真正释放一次。
// recordingConcurrency counts acquires and releases so tests can prove rejections
// take no slot and repeated releases free only once.
type recordingConcurrency struct {
	acquires int
	releases int
}

func (c *recordingConcurrency) Acquire(ctx context.Context, _ string) error {
	c.acquires++
	return ctx.Err()
}

func (c *recordingConcurrency) Release(string) { c.releases++ }

// TestRateBucket_BurstThenRefill 验证满桶起步、按经过时间回填：burst 用尽后立即
// 拒绝，1 秒后仅回填一个令牌。逆改点：去掉回填 → 第三秒仍拒绝；首次不按满桶
// → 第二个请求就红。
func TestRateBucket_BurstThenRefill(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 1, Burst: 2}}, func() time.Time { return now })
	require.NoError(t, err)

	require.True(t, bucket.Allow("c1", IngressRun), "第一个请求应命中初始突发额度")
	require.True(t, bucket.Allow("c1", IngressRun), "burst=2 允许第二个请求")
	require.False(t, bucket.Allow("c1", IngressRun), "burst 用尽后必须拒绝")

	now = now.Add(time.Second)
	require.True(t, bucket.Allow("c1", IngressRun), "1 秒后按 qps=1 回填一个令牌")
	require.False(t, bucket.Allow("c1", IngressRun), "只回填一个令牌")
}

// TestRateBucket_PerCallerAndClassIsolation 验证桶粒度是「调用方 + 类别」：
// 一个调用方的 Run 额度耗尽，不得影响别的调用方，也不得影响自己的读端点。
func TestRateBucket_PerCallerAndClassIsolation(t *testing.T) {
	bucket, err := NewRateBucket(IngressLimits{
		Run:  IngressBudget{QPS: 1, Burst: 1},
		Read: IngressBudget{QPS: 1, Burst: 1},
	}, nil)
	require.NoError(t, err)

	require.True(t, bucket.Allow("c1", IngressRun))
	require.False(t, bucket.Allow("c1", IngressRun))
	require.True(t, bucket.Allow("c2", IngressRun), "其他调用方不受影响")
	require.True(t, bucket.Allow("c1", IngressRead), "读端点使用独立桶")
}

// TestRateBucket_ZeroBudgetDeniesAll 验证零预算合法但拒绝全部请求：阈值未冻结时
// 宁可全拒，也不得退化成不限流。
func TestRateBucket_ZeroBudgetDeniesAll(t *testing.T) {
	bucket, err := NewRateBucket(IngressLimits{
		Run:  IngressBudget{QPS: 1, Burst: 0},
		Read: IngressBudget{QPS: 0, Burst: 1},
	}, nil)
	require.NoError(t, err)

	require.False(t, bucket.Allow("c1", IngressRun), "burst=0 必须拒绝")
	require.False(t, bucket.Allow("c1", IngressRead), "qps=0 必须拒绝")
	require.False(t, bucket.Allow("", IngressRun), "空调用方必须拒绝")
	require.False(t, bucket.Allow("c1", IngressClass("upload")), "未知类别必须拒绝")
}

// TestRateBucket_ConcurrentAllowStaysWithinBurst 用冻结时钟并发打桶：允许次数
// 必须恰好等于 burst，既不能超发，也不能因锁竞争漏发。`-race` 同时覆盖数据竞争。
func TestRateBucket_ConcurrentAllowStaysWithinBurst(t *testing.T) {
	const (
		burst      = 8
		goroutines = 64
	)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 1, Burst: burst}}, func() time.Time { return now })
	require.NoError(t, err)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if bucket.Allow("c1", IngressRun) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(burst), allowed.Load(), "并发下允许次数必须恰好等于 burst")
}

// TestNewRateBucket_RejectsNegativeBudgets 验证负数参数是装配错误，不得被静默
// 当成零值或无穷大。
func TestNewRateBucket_RejectsNegativeBudgets(t *testing.T) {
	_, err := NewRateBucket(IngressLimits{Ingest: IngressBudget{QPS: -1, Burst: 1}}, nil)
	require.Error(t, err)
	_, err = NewRateBucket(IngressLimits{Read: IngressBudget{QPS: 1, Burst: -1}}, nil)
	require.Error(t, err)
}

// TestAdmitIngress_ChargesOnceAndRejectsOverLimit 验证每个请求只扣一次，超限返回
// ErrRateLimited。逆改点：Allow 恒真 → 第二次仍 nil，断言红。
func TestAdmitIngress_ChargesOnceAndRejectsOverLimit(t *testing.T) {
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 1, Burst: 1}}, nil)
	require.NoError(t, err)
	limiter := NewLimiter(LimiterDeps{Buckets: bucket})
	ctx := context.Background()

	require.NoError(t, limiter.AdmitIngress(ctx, testCallerPrincipal("c1"), IngressRun))
	require.ErrorIs(t, limiter.AdmitIngress(ctx, testCallerPrincipal("c1"), IngressRun), ErrRateLimited)
}

// TestAdmitIngress_RejectsInvalidPrincipalBeforeBucket 验证零值身份在触达令牌桶
// 之前就被拒绝：否则未认证流量会以空 CallerID 建桶，还可能被当成放行。
func TestAdmitIngress_RejectsInvalidPrincipalBeforeBucket(t *testing.T) {
	counting := &countingRateBucket{}
	limiter := NewLimiter(LimiterDeps{Buckets: counting})

	err := limiter.AdmitIngress(context.Background(), core.Principal{}, IngressRun)
	require.ErrorIs(t, err, core.ErrUnauthorized)
	require.Zero(t, counting.calls.Load(), "无效身份不得触碰令牌桶")
}

// TestAdmitIngress_UnknownClassAndMissingPortFailClosed 验证未知类别与缺失端口都
// 返回错误而不是放行：路由映射遗漏必须在启动期暴露，运行期也不得静默通融。
func TestAdmitIngress_UnknownClassAndMissingPortFailClosed(t *testing.T) {
	limiter := NewLimiter(LimiterDeps{Buckets: &countingRateBucket{}})
	err := limiter.AdmitIngress(context.Background(), testCallerPrincipal("c1"), IngressClass("upload"))
	require.ErrorIs(t, err, errUnknownIngressClass)

	unconfigured := NewLimiter(LimiterDeps{})
	err = unconfigured.AdmitIngress(context.Background(), testCallerPrincipal("c1"), IngressRun)
	require.ErrorIs(t, err, errLimiterUnconfigured)
}

// TestAdmitIngress_ContextCanceled 验证取消的请求不扣令牌：已断开的上传不应
// 消耗调用方的突发额度。
func TestAdmitIngress_ContextCanceled(t *testing.T) {
	counting := &countingRateBucket{}
	limiter := NewLimiter(LimiterDeps{Buckets: counting})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := limiter.AdmitIngress(ctx, testCallerPrincipal("c1"), IngressRun)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, counting.calls.Load(), "取消的请求不得扣桶")
}

// TestMemoryConcurrency_AcquireRespectsLimitAndRelease 验证名额按调用方计数：
// 限额用尽即拒，释放后可再次准入。逆改点：比较写成 > → 第二个请求也放行。
func TestMemoryConcurrency_AcquireRespectsLimitAndRelease(t *testing.T) {
	port, err := NewMemoryConcurrency(func(callerID string) (int, bool) {
		return 1, callerID == "c1"
	})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, port.Acquire(ctx, "c1"))
	require.ErrorIs(t, port.Acquire(ctx, "c1"), ErrConcurrencyLimited)
	port.Release("c1")
	require.NoError(t, port.Acquire(ctx, "c1"), "释放后名额必须可再次获取")
}

// TestMemoryConcurrency_IsolatesCallers 验证额度按调用方隔离：一个调用方打满
// 不得挡住别人，也不能借用别人的名额。
func TestMemoryConcurrency_IsolatesCallers(t *testing.T) {
	port, err := NewMemoryConcurrency(func(string) (int, bool) { return 1, true })
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, port.Acquire(ctx, "c1"))
	require.ErrorIs(t, port.Acquire(ctx, "c1"), ErrConcurrencyLimited)
	require.NoError(t, port.Acquire(ctx, "c2"), "其他调用方不受影响")
}

// TestMemoryConcurrency_ZeroMissingAndNegativeLimits 验证限额缺失与零值是拒绝
// 而非不限并发；负数是配置缺陷，按错误暴露。
func TestMemoryConcurrency_ZeroMissingAndNegativeLimits(t *testing.T) {
	limits := map[string]int{"zero": 0, "negative": -1}
	port, err := NewMemoryConcurrency(func(callerID string) (int, bool) {
		limit, ok := limits[callerID]
		return limit, ok
	})
	require.NoError(t, err)
	ctx := context.Background()

	require.ErrorIs(t, port.Acquire(ctx, "missing"), ErrConcurrencyLimited, "无限额行必须拒绝")
	require.ErrorIs(t, port.Acquire(ctx, "zero"), ErrConcurrencyLimited, "零限额必须拒绝")
	require.ErrorIs(t, port.Acquire(ctx, "negative"), errInvalidConcurrencyLimit)
	require.ErrorIs(t, port.Acquire(ctx, ""), core.ErrUnauthorized, "空调用方必须拒绝")
}

// TestMemoryConcurrency_CanceledAndTimedOutAcquireTakeNoSlot 验证取消与超时的
// 获取不占名额：之后同调用方仍能用满额度。
func TestMemoryConcurrency_CanceledAndTimedOutAcquireTakeNoSlot(t *testing.T) {
	port, err := NewMemoryConcurrency(func(string) (int, bool) { return 1, true })
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, port.Acquire(canceled, "c1"), context.Canceled)

	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	require.ErrorIs(t, port.Acquire(expired, "c1"), context.DeadlineExceeded)

	require.NoError(t, port.Acquire(context.Background(), "c1"), "被取消 / 超时的请求不得占用名额")
	require.ErrorIs(t, port.Acquire(context.Background(), "c1"), ErrConcurrencyLimited)
}

// TestMemoryConcurrency_ReleaseAtZeroDoesNotGoNegative 验证未占用时释放、以及
// 重复释放都不会造出额外名额。
func TestMemoryConcurrency_ReleaseAtZeroDoesNotGoNegative(t *testing.T) {
	port, err := NewMemoryConcurrency(func(string) (int, bool) { return 1, true })
	require.NoError(t, err)
	ctx := context.Background()

	port.Release("c1") // 从未占用
	require.NoError(t, port.Acquire(ctx, "c1"))
	require.ErrorIs(t, port.Acquire(ctx, "c1"), ErrConcurrencyLimited, "空放行不得变成负数额度")
	port.Release("c1")
	port.Release("c1")
	require.NoError(t, port.Acquire(ctx, "c1"))
	require.ErrorIs(t, port.Acquire(ctx, "c1"), ErrConcurrencyLimited, "重复释放不得多出名额")
}

// TestNewMemoryConcurrency_RejectsNilLimitSource 验证缺少限额来源是装配错误。
func TestNewMemoryConcurrency_RejectsNilLimitSource(t *testing.T) {
	_, err := NewMemoryConcurrency(nil)
	require.ErrorIs(t, err, errConcurrencyUnconfigured)
}

// TestLimiterAdmitRun_TakesOneSlotPerRun 验证每个 Run 只占一个名额，且退还可
// 再次准入；票据带出调用方与进程内唯一键。
func TestLimiterAdmitRun_TakesOneSlotPerRun(t *testing.T) {
	port, err := NewMemoryConcurrency(func(string) (int, bool) { return 1, true })
	require.NoError(t, err)
	limiter := NewLimiter(LimiterDeps{Concurrency: port})
	ctx := context.Background()

	ticket, err := limiter.AdmitRun(ctx, testCallerPrincipal("c1"))
	require.NoError(t, err)
	require.Equal(t, "c1", ticket.CallerID)
	require.NotEmpty(t, ticket.RunKey)

	_, err = limiter.AdmitRun(ctx, testCallerPrincipal("c1"))
	require.ErrorIs(t, err, ErrConcurrencyLimited)

	require.NoError(t, limiter.ReleaseRun(ctx, ticket))
	_, err = limiter.AdmitRun(ctx, testCallerPrincipal("c1"))
	require.NoError(t, err, "退还后名额必须可再次获取")
}

// TestLimiterAdmitRun_RejectsInvalidPrincipalAndMissingPort 验证身份闸在占名额
// 之前，以及缺失端口失败关闭。
func TestLimiterAdmitRun_RejectsInvalidPrincipalAndMissingPort(t *testing.T) {
	recording := &recordingConcurrency{}
	limiter := NewLimiter(LimiterDeps{Concurrency: recording})

	_, err := limiter.AdmitRun(context.Background(), core.Principal{})
	require.ErrorIs(t, err, core.ErrUnauthorized)
	require.Zero(t, recording.acquires, "无效身份不得占用名额")

	unconfigured := NewLimiter(LimiterDeps{})
	_, err = unconfigured.AdmitRun(context.Background(), testCallerPrincipal("c1"))
	require.ErrorIs(t, err, errConcurrencyUnconfigured)
}

// TestLimiterReleaseRun_IsIdempotent 验证同一票据重复退还只真正释放一次：
// 计数用假端口，否则端口的自愈会掩盖重复释放。
func TestLimiterReleaseRun_IsIdempotent(t *testing.T) {
	recording := &recordingConcurrency{}
	limiter := NewLimiter(LimiterDeps{Concurrency: recording})
	ctx := context.Background()

	ticket, err := limiter.AdmitRun(ctx, testCallerPrincipal("c1"))
	require.NoError(t, err)
	require.NoError(t, limiter.ReleaseRun(ctx, ticket))
	require.NoError(t, limiter.ReleaseRun(ctx, ticket))

	require.Equal(t, 1, recording.acquires)
	require.Equal(t, 1, recording.releases, "重复退还只能释放一次")
}

// TestLimiterReleaseRun_ReleasesUnderCanceledContextAndPanic 验证两条最易漏的
// 退出路径：ctx 已取消时仍归还，以及 panic 展开时由 defer 归还。
func TestLimiterReleaseRun_ReleasesUnderCanceledContextAndPanic(t *testing.T) {
	port, err := NewMemoryConcurrency(func(string) (int, bool) { return 1, true })
	require.NoError(t, err)
	limiter := NewLimiter(LimiterDeps{Concurrency: port})
	ctx := context.Background()

	ticket, err := limiter.AdmitRun(ctx, testCallerPrincipal("c1"))
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.NoError(t, limiter.ReleaseRun(canceled, ticket), "取消路径仍须归还名额")

	second, err := limiter.AdmitRun(ctx, testCallerPrincipal("c1"))
	require.NoError(t, err)
	func() {
		defer func() { require.NotNil(t, recover(), "panic 应按预期传播") }()
		defer func() { require.NoError(t, limiter.ReleaseRun(ctx, second)) }()
		panic("provider exploded")
	}()

	_, err = limiter.AdmitRun(ctx, testCallerPrincipal("c1"))
	require.NoError(t, err, "panic 收口后名额必须可用")
}

// TestLimiterReleaseRun_RejectsZeroTicketAndCallerMismatch 验证零值票据与串票
// 都不得释放名额：否则一个调用方能把别人的名额还掉。
func TestLimiterReleaseRun_RejectsZeroTicketAndCallerMismatch(t *testing.T) {
	recording := &recordingConcurrency{}
	limiter := NewLimiter(LimiterDeps{Concurrency: recording})
	ctx := context.Background()

	require.ErrorIs(t, limiter.ReleaseRun(ctx, RunTicket{}), errInvalidRunTicket)

	ticket, err := limiter.AdmitRun(ctx, testCallerPrincipal("c1"))
	require.NoError(t, err)
	forged := RunTicket{CallerID: "c2", RunKey: ticket.RunKey}
	require.ErrorIs(t, limiter.ReleaseRun(ctx, forged), errRunTicketMismatch)
	require.Zero(t, recording.releases, "串票不得释放任何名额")

	require.NoError(t, limiter.ReleaseRun(ctx, ticket))
	require.Equal(t, 1, recording.releases)
}

// TestLimiterAdmitIngress_DenialDoesNotTakeRunSlot 验证图 6 的顺序：入口速率
// 被拒时尚未进入 [3]，不得占用并发名额。
func TestLimiterAdmitIngress_DenialDoesNotTakeRunSlot(t *testing.T) {
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 1, Burst: 1}}, nil)
	require.NoError(t, err)
	recording := &recordingConcurrency{}
	limiter := NewLimiter(LimiterDeps{Buckets: bucket, Concurrency: recording})
	ctx := context.Background()

	require.NoError(t, limiter.AdmitIngress(ctx, testCallerPrincipal("c1"), IngressRun))
	require.ErrorIs(t, limiter.AdmitIngress(ctx, testCallerPrincipal("c1"), IngressRun), ErrRateLimited)
	require.Zero(t, recording.acquires, "入口速率被拒时尚未进入并发阶段")
}
