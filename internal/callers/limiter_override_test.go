package callers

// 本文件覆盖 C-F01（2026-10-31 裁决第 5 条）：`caller_limits.qps` / `burst` 作为
// 调用方入口速率覆盖**必须真的生效**。判据五条——覆盖只收紧不放宽、两列独立
// （0 = 该列不覆盖）、覆盖按调用方与入口类别各自隔离、小数 QPS 原样生效、
// 读覆盖失败不得伪装成 429 且不得扣桶。
// These tests pin C-F01: the per-caller override must actually tighten the class
// budget, never widen it, stay isolated per caller and class, honour fractional
// QPS, and never turn a store failure into a rate-limit verdict.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stubIngressOverrides 是覆盖读口的替身：按调用方给预算，或统一返回错误。
// stubIngressOverrides returns per-caller budgets or a shared error.
type stubIngressOverrides struct {
	budgets map[string]IngressBudget
	err     error
	calls   atomic.Int64
}

func (s *stubIngressOverrides) Override(_ context.Context, callerID string) (IngressBudget, bool, error) {
	s.calls.Add(1)
	if s.err != nil {
		return IngressBudget{}, false, s.err
	}
	budget, ok := s.budgets[callerID]
	return budget, ok, nil
}

// fixedClock 返回不前进的时钟：并发用例判"恰好 N 个放行"时不靠 sleep 赌回填。
// fixedClock never advances so burst counting is exact.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// TestRateBucket_OverrideTightensClassBudget 验证覆盖把类别预算收紧：类别 burst=10
// 时，覆盖 burst=2 的调用方第三个请求就被拒，没有覆盖的调用方不受影响。
// 逆改点：`AllowWith` 忽略 override → 第三次仍放行，第一条拒绝断言红。
func TestRateBucket_OverrideTightensClassBudget(t *testing.T) {
	now := fixedClock(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 10, Burst: 10}}, now)
	require.NoError(t, err)

	tight := IngressBudget{QPS: 2, Burst: 2}
	require.True(t, bucket.AllowWith("c1", IngressRun, &tight), "覆盖 burst=2 的第一个请求")
	require.True(t, bucket.AllowWith("c1", IngressRun, &tight), "覆盖 burst=2 的第二个请求")
	require.False(t, bucket.AllowWith("c1", IngressRun, &tight), "覆盖额度用尽必须拒绝（类别 burst=10）")

	require.True(t, bucket.Allow("c2", IngressRun), "没有覆盖的调用方仍按类别预算")
}

// TestRateBucket_OverridePerClassIsolation 验证覆盖按「调用方 + 类别」各自生效：
// 同一调用方的 Run 桶打满不影响它的 Read 桶，覆盖值也不跨类别共享。
// 逆改点：桶键丢掉 class → Read 被 Run 的消耗拖累，最后一条断言红。
func TestRateBucket_OverridePerClassIsolation(t *testing.T) {
	now := fixedClock(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))
	bucket, err := NewRateBucket(IngressLimits{
		Run:  IngressBudget{QPS: 10, Burst: 10},
		Read: IngressBudget{QPS: 10, Burst: 10},
	}, now)
	require.NoError(t, err)

	tight := IngressBudget{QPS: 2, Burst: 2}
	require.True(t, bucket.AllowWith("c1", IngressRun, &tight))
	require.True(t, bucket.AllowWith("c1", IngressRun, &tight))
	require.False(t, bucket.AllowWith("c1", IngressRun, &tight))

	require.True(t, bucket.AllowWith("c1", IngressRead, &tight), "读端点使用独立桶")
	require.True(t, bucket.AllowWith("c1", IngressRead, &tight))
	require.False(t, bucket.AllowWith("c1", IngressRead, &tight), "读端点按同一覆盖值独立计额")
}

// TestRateBucket_OverrideCannotWidenClassBudget 验证覆盖**不得**放宽类别上限：
// 类别 1 QPS / 1 burst 时，覆盖 100/100 仍然只放行一个。
// 逆改点：实现写成 `out = *override` → 第二次放行，断言红。
func TestRateBucket_OverrideCannotWidenClassBudget(t *testing.T) {
	now := fixedClock(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 1, Burst: 1}}, now)
	require.NoError(t, err)

	wide := IngressBudget{QPS: 100, Burst: 100}
	require.True(t, bucket.AllowWith("c1", IngressRun, &wide))
	require.False(t, bucket.AllowWith("c1", IngressRun, &wide), "覆盖不得放宽类别上限")
}

// TestRateBucket_OverrideZeroColumnKeepsClassValue 验证两列独立：覆盖只给 burst=3、
// qps 留 0 时，速率仍取类别值 1——1 秒后必须回填出一个令牌（若把 0 当成 qps=0，
// 回填为零，最后一条断言红）。
func TestRateBucket_OverrideZeroColumnKeepsClassValue(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 1, Burst: 10}}, func() time.Time { return now })
	require.NoError(t, err)

	override := IngressBudget{QPS: 0, Burst: 3}
	require.True(t, bucket.AllowWith("c1", IngressRun, &override))
	require.True(t, bucket.AllowWith("c1", IngressRun, &override))
	require.True(t, bucket.AllowWith("c1", IngressRun, &override))
	require.False(t, bucket.AllowWith("c1", IngressRun, &override), "burst 覆盖为 3，第四个请求必须拒绝")

	now = now.Add(time.Second)
	require.True(t, bucket.AllowWith("c1", IngressRun, &override), "qps 列留 0 时沿用类别速率 1")
}

// TestRateBucket_OverrideNeverRescuesUnconfiguredClass 验证类别预算未配置（0/0）时
// 覆盖救不回来：入口统一限流是外闸，调用方覆盖只能更紧。
// 逆改点：把覆盖当成"类别缺省的补位" → 本条放行，断言红。
func TestRateBucket_OverrideNeverRescuesUnconfiguredClass(t *testing.T) {
	bucket, err := NewRateBucket(IngressLimits{}, fixedClock(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)))
	require.NoError(t, err)

	override := IngressBudget{QPS: 5, Burst: 5}
	require.False(t, bucket.AllowWith("c1", IngressRun, &override), "类别预算为 0 时覆盖不得放行")
}

// TestRateBucket_OverrideFractionalQPS 验证小数速率原样生效：覆盖 QPS=0.5 时，
// 1 秒只回填半个令牌（拒绝）、2 秒整回填一个（放行）。
// 逆改点：覆盖速率截断成 int → 0.5 变 0（2 秒后仍拒绝）或变 1（1 秒后就放行），
// 两条断言必有一条红。
func TestRateBucket_OverrideFractionalQPS(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 10, Burst: 1}}, func() time.Time { return now })
	require.NoError(t, err)

	override := IngressBudget{QPS: 0.5, Burst: 1}
	require.True(t, bucket.AllowWith("c1", IngressRun, &override), "初始突发额度")

	now = now.Add(time.Second)
	require.False(t, bucket.AllowWith("c1", IngressRun, &override), "0.5 QPS 一秒只回填半个令牌")

	now = now.Add(time.Second)
	require.True(t, bucket.AllowWith("c1", IngressRun, &override), "两秒回填一个令牌")
}

// TestLimiter_AdmitIngressUsesCallerOverride 验证 [2] 把覆盖接进判定：同一类别下，
// 有覆盖的调用方按覆盖额度被拒，无覆盖的调用方按类别预算放行。
func TestLimiter_AdmitIngressUsesCallerOverride(t *testing.T) {
	now := fixedClock(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 100, Burst: 100}}, now)
	require.NoError(t, err)
	overrides := &stubIngressOverrides{budgets: map[string]IngressBudget{
		"c1": {QPS: 1, Burst: 1},
	}}
	limiter := NewLimiter(LimiterDeps{Buckets: bucket, IngressOverrides: overrides})

	ctx := context.Background()
	require.NoError(t, limiter.AdmitIngress(ctx, testCallerPrincipal("c1"), IngressRun))
	require.ErrorIs(t, limiter.AdmitIngress(ctx, testCallerPrincipal("c1"), IngressRun), ErrRateLimited,
		"覆盖额度用尽必须 429")
	require.NoError(t, limiter.AdmitIngress(ctx, testCallerPrincipal("c2"), IngressRun),
		"没有覆盖的调用方按类别预算（burst=100）")
	require.Equal(t, int64(3), overrides.calls.Load(), "每个请求都要查一次覆盖（缓存由装配层负责）")
}

// TestLimiter_AdmitIngressWithoutOverridePortUsesClassBudget 验证读口未装配时退回
// 类别预算，而不是 panic 或放行一切——类别闸门始终是外闸。
func TestLimiter_AdmitIngressWithoutOverridePortUsesClassBudget(t *testing.T) {
	now := fixedClock(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 1, Burst: 1}}, now)
	require.NoError(t, err)
	limiter := NewLimiter(LimiterDeps{Buckets: bucket})

	ctx := context.Background()
	require.NoError(t, limiter.AdmitIngress(ctx, testCallerPrincipal("c1"), IngressRun))
	require.ErrorIs(t, limiter.AdmitIngress(ctx, testCallerPrincipal("c1"), IngressRun), ErrRateLimited)
}

// TestLimiter_AdmitIngressOverrideReadFailureIsNotRateLimited 验证读覆盖失败：
// ① 错误如实上抛（保留原因），② **不得**是 `ErrRateLimited`（否则中间件会回 429
// 并记一条限流诊断，把存储故障说成限流），③ 失败不扣桶——故障排除后第一个请求
// 仍命中完整突发额度。
// 逆改点：把读失败吞成"没有覆盖" → AdmitIngress 放行、第一条断言红；映射成
// ErrRateLimited → 第二条断言红；先扣桶再读覆盖 → 第三条断言红。
func TestLimiter_AdmitIngressOverrideReadFailureIsNotRateLimited(t *testing.T) {
	now := fixedClock(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 1, Burst: 1}}, now)
	require.NoError(t, err)

	broken := &stubIngressOverrides{err: errors.New("db down")}
	limiter := NewLimiter(LimiterDeps{Buckets: bucket, IngressOverrides: broken})

	admitErr := limiter.AdmitIngress(context.Background(), testCallerPrincipal("c1"), IngressRun)
	require.Error(t, admitErr, "读覆盖失败必须拒绝请求（失败关闭）")
	require.NotErrorIs(t, admitErr, ErrRateLimited, "存储故障不得伪装成限流")
	require.Contains(t, admitErr.Error(), "db down", "原始原因必须保留在错误链里")

	healthy := NewLimiter(LimiterDeps{Buckets: bucket, IngressOverrides: &stubIngressOverrides{}})
	require.NoError(t, healthy.AdmitIngress(context.Background(), testCallerPrincipal("c1"), IngressRun),
		"失败路径不得扣桶")
}

// TestLimiter_AdmitIngressOverrideIsolatesCallersUnderConcurrency 验证并发下覆盖的
// 隔离性：50 个并发请求里，有覆盖的调用方恰好放行 burst=5 个，同一时刻无覆盖的
// 调用方 50 个全放行（类别 burst=100）。`-race` 同时覆盖桶的互斥与读口并发调用。
// 逆改点：桶键漏掉 callerID → 两个调用方共享一个桶，放行数不再是 5 / 50。
func TestLimiter_AdmitIngressOverrideIsolatesCallersUnderConcurrency(t *testing.T) {
	now := fixedClock(time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))
	bucket, err := NewRateBucket(IngressLimits{Run: IngressBudget{QPS: 1, Burst: 100}}, now)
	require.NoError(t, err)
	overrides := &stubIngressOverrides{budgets: map[string]IngressBudget{
		"tight": {QPS: 1, Burst: 5},
	}}
	limiter := NewLimiter(LimiterDeps{Buckets: bucket, IngressOverrides: overrides})

	var (
		wg            sync.WaitGroup
		tightOK       atomic.Int64
		tightDenied   atomic.Int64
		looseOK       atomic.Int64
		looseDenied   atomic.Int64
		unexpectedErr atomic.Int64
	)
	run := func(callerID string, allowed, denied *atomic.Int64) {
		defer wg.Done()
		switch err := limiter.AdmitIngress(context.Background(), testCallerPrincipal(callerID), IngressRun); {
		case err == nil:
			allowed.Add(1)
		case errors.Is(err, ErrRateLimited):
			denied.Add(1)
		default:
			unexpectedErr.Add(1)
		}
	}
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go run("tight", &tightOK, &tightDenied)
		go run("loose", &looseOK, &looseDenied)
	}
	wg.Wait()

	require.Zero(t, unexpectedErr.Load(), "并发路径不得出现非限流错误")
	require.Equal(t, int64(5), tightOK.Load(), "覆盖 burst=5 恰好放行五个")
	require.Equal(t, int64(45), tightDenied.Load())
	require.Equal(t, int64(50), looseOK.Load(), "无覆盖的调用方不受影响")
	require.Zero(t, looseDenied.Load())
}
