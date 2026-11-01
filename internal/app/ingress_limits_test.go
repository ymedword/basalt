package app

// 本文件覆盖 C-F01 的装配侧：`caller_limits.qps/burst` 的 TTL 缓存投影与
// `newIngressLimiter` 的装配形状。判据——覆盖必须真的进到 [2] 判定、TTL 内不重复
// 读库、读失败**不**缓存且不降级成"没有覆盖"、读口未装配即失败关闭。
// These tests pin the assembly side of C-F01: the override projection reaches
// admission stage [2], the TTL cache behaves, and failures never degrade to
// "no override".

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
)

// overridePrincipal 构造合法调用方身份（入口判定只读 Kind / CallerID）。
// overridePrincipal builds the minimal valid caller identity for stage [2].
func overridePrincipal(callerID string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: callerID,
		KeyID:    callerID + "-key",
		Scopes:   []string{core.ScopeSessionsRead},
		Actor:    core.ActorCaller,
	}
}

// TestIngressLimitCache_CachesUntilTTL 验证 TTL 内命中缓存、过期后重读：
// 管理面改完覆盖最迟一个 TTL 生效，不必每个请求打一次库。
// 逆改点：去掉缓存 → 第二次调用的读次数为 2，计数断言红。
func TestIngressLimitCache_CachesUntilTTL(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var reads atomic.Int64
	read := func(_ context.Context, callerID string) (callers.IngressBudget, bool, error) {
		reads.Add(1)
		if callerID != "c1" {
			return callers.IngressBudget{}, false, nil
		}
		return callers.IngressBudget{QPS: 2, Burst: 3}, true, nil
	}
	cache := newIngressLimitCache(read, func() time.Time { return now })

	budget, found, err := cache.Override(context.Background(), "c1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, callers.IngressBudget{QPS: 2, Burst: 3}, budget)

	_, _, err = cache.Override(context.Background(), "c1")
	require.NoError(t, err)
	require.Equal(t, int64(1), reads.Load(), "TTL 内必须命中缓存")

	now = now.Add(ingressLimitTTL)
	_, _, err = cache.Override(context.Background(), "c1")
	require.NoError(t, err)
	require.Equal(t, int64(2), reads.Load(), "TTL 过后必须重读")
}

// TestIngressLimitCache_ReadFailureIsNotCached 验证读失败原样上抛且不写缓存：
// 下一次调用立刻重试，故障恢复后覆盖立即回到判定里。
// 逆改点：把失败缓存成"没有覆盖" → 第二次仍不读库，读次数停在 1，断言红。
func TestIngressLimitCache_ReadFailureIsNotCached(t *testing.T) {
	boom := errors.New("db down")
	var reads atomic.Int64
	read := func(_ context.Context, _ string) (callers.IngressBudget, bool, error) {
		if reads.Add(1) == 1 {
			return callers.IngressBudget{}, false, boom
		}
		return callers.IngressBudget{QPS: 1, Burst: 1}, true, nil
	}
	cache := newIngressLimitCache(read, nil)

	_, _, err := cache.Override(context.Background(), "c1")
	require.ErrorIs(t, err, boom, "读失败必须如实上抛")

	budget, found, err := cache.Override(context.Background(), "c1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, callers.IngressBudget{QPS: 1, Burst: 1}, budget)
	require.Equal(t, int64(2), reads.Load(), "失败不得缓存")
}

// TestIngressLimitCache_MissingRowIsNoOverride 验证"没有行 / 0 列"是正常结果
// 而不是错误：返回 found=false，由令牌桶沿用类别预算。
func TestIngressLimitCache_MissingRowIsNoOverride(t *testing.T) {
	cache := newIngressLimitCache(
		func(context.Context, string) (callers.IngressBudget, bool, error) {
			return callers.IngressBudget{}, false, nil
		}, nil)

	budget, found, err := cache.Override(context.Background(), "c1")
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, callers.IngressBudget{}, budget)
}

// TestIngressLimitCache_UnconfiguredReaderFailsClosed 验证读口未装配是装配错误：
// 返回错误而不是"没有覆盖"——静默当成没覆盖正是 C-F01 要消灭的"配了不生效"。
func TestIngressLimitCache_UnconfiguredReaderFailsClosed(t *testing.T) {
	cache := newIngressLimitCache(nil, nil)
	_, _, err := cache.Override(context.Background(), "c1")
	require.ErrorIs(t, err, errIngressOverridesUnconfigured)

	var nilCache *ingressLimitCache
	_, _, err = nilCache.Override(context.Background(), "c1")
	require.ErrorIs(t, err, errIngressOverridesUnconfigured)
}

// TestIngressLimitCache_ConcurrentOverridesAreSafe 让缓存并发读写（`-race` 判互斥）；
// 断言只判"每个调用方都拿到自己的值、没有错误"，不断言读次数——并发首读可能多重
// 放行几次，属于缓存语义允许的范围。
func TestIngressLimitCache_ConcurrentOverridesAreSafe(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	read := func(_ context.Context, callerID string) (callers.IngressBudget, bool, error) {
		return callers.IngressBudget{QPS: float64(len(callerID)), Burst: 7}, true, nil
	}
	cache := newIngressLimitCache(read, func() time.Time { return now })

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 32; j++ {
				budget, found, err := cache.Override(context.Background(), "cc")
				assert.NoError(t, err)
				assert.True(t, found)
				assert.Equal(t, 7, budget.Burst)
			}
		}()
	}
	wg.Wait()
}

// TestCallerRateLimit_WiresOverrideIntoIngressAdmission 验证装配形状：类别预算宽松
// （100/100）而那一行是 1/1 时，第二个请求必须被拒——覆盖没有在装配层丢掉。
// 走的是 `newIngressLimiterWith`（`newIngressLimiter` 的内层），与 `newWebServer`
// 共享同一段依赖接线；真实库那一半由 `TestCallerRateLimit_AppliesPerCallerQPSFromLimitRow`
// 守（integration）。
// 逆改点：`LimiterDeps` 少传 `IngressOverrides` → 第二次放行，拒绝断言红。
// Mutation: dropping the port from LimiterDeps lets the second request through.
func TestCallerRateLimit_WiresOverrideIntoIngressAdmission(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC) }
	cfg := &cli.Config{}
	cfg.Callers.Ingress.Run = cli.IngressBudgetConfig{QPS: 100, Burst: 100}

	read := func(_ context.Context, callerID string) (callers.IngressBudget, bool, error) {
		if callerID == "tight" {
			return callers.IngressBudget{QPS: 1, Burst: 1}, true, nil
		}
		return callers.IngressBudget{}, false, nil
	}
	limiter, err := newIngressLimiterWith(cfg, read, now)
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, limiter.AdmitIngress(ctx, overridePrincipal("tight"), callers.IngressRun))
	require.ErrorIs(t, limiter.AdmitIngress(ctx, overridePrincipal("tight"), callers.IngressRun),
		callers.ErrRateLimited, "库里的 qps/burst 必须真的生效")
	require.NoError(t, limiter.AdmitIngress(ctx, overridePrincipal("loose"), callers.IngressRun),
		"没有覆盖的调用方按类别预算")
}
