//go:build integration

package app

// 本文件守 C-F01 的**真实库**链路（2026-10-31 裁决第 5 条）：`caller_limits.qps` /
// `burst` 写进 PostgreSQL 之后，必须经 `postgres.QuotaStore.IngressBudget` →
// `ingressLimitCache` → `callers.Limiter.AdmitIngress` 真的生效。判据五条——行里的
// 覆盖收紧类别预算、两列的 0 表示"该列不覆盖"而不是"不限流"、没有行的调用方按类别
// 预算、单列覆盖只影响那一列、覆盖按调用方隔离。
// The real-database half of C-F01: a caller_limits row must actually tighten
// admission stage [2] through the production assembly shape.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// TestCallerRateLimit_AppliesPerCallerQPSFromLimitRow：真实库 + 生产装配形状。
//
// 逆改点：`IngressBudget` 把 0/0 读成"覆盖 0"→ c-zero 的请求全被拒；装配层不接
// `IngressOverrides` → c1 的两个请求都放行；把两列合并成一个开关 → c-burst 的
// qps 列被误用（burst 之外的判定红）。
// Mutation: reading 0/0 as an override denies c-zero; dropping the port lets c1
// through; folding the two columns breaks the single-column case.
func TestCallerRateLimit_AppliesPerCallerQPSFromLimitRow(t *testing.T) {
	h := newRunHarness(t)
	ctx := context.Background()
	store := postgres.NewQuotaStore(h.db)

	// c1：两列都覆盖（qps=0.5 验证小数原样入库与读出）。
	require.NoError(t, store.SetLimit(ctx, "c1", callers.Limit{
		QPS: 0.5, Burst: 1, MaxConcurrent: 4,
		TasksPerPeriod: 10, TokensPerPeriod: 1000, IngestBytesPerPeriod: 1 << 20,
	}))
	// c-zero：两列都留 0——契约 `0 = 不限`（不额外收紧）。
	require.NoError(t, store.SetLimit(ctx, "c-zero", callers.Limit{
		MaxConcurrent: 4, TasksPerPeriod: 10, TokensPerPeriod: 1000, IngestBytesPerPeriod: 1 << 20,
	}))
	// c-burst：只覆盖 burst，qps 列留 0。
	require.NoError(t, store.SetLimit(ctx, "c-burst", callers.Limit{
		QPS: 0, Burst: 2, MaxConcurrent: 4,
		TasksPerPeriod: 10, TokensPerPeriod: 1000, IngestBytesPerPeriod: 1 << 20,
	}))

	// 仓储只回答事实，不替装配层裁决。
	budget, found, err := store.IngressBudget(ctx, "c1")
	require.NoError(t, err)
	require.True(t, found)
	require.InDelta(t, 0.5, budget.QPS, 1e-9)
	require.Equal(t, 1, budget.Burst)

	_, found, err = store.IngressBudget(ctx, "c-zero")
	require.NoError(t, err)
	require.False(t, found, "两列为 0 必须报没有覆盖")

	budget, found, err = store.IngressBudget(ctx, "c-burst")
	require.NoError(t, err)
	require.True(t, found)
	require.Zero(t, budget.QPS, "qps 列留 0 时原样报 0，由收紧逻辑按「不覆盖」处理")
	require.Equal(t, 2, budget.Burst)

	_, found, err = store.IngressBudget(ctx, "nobody")
	require.NoError(t, err)
	require.False(t, found, "没有 limit 行")

	// 生产装配形状：`newWebServer` 走的同一段代码。
	cfg := &cli.Config{}
	cfg.Callers.Ingress.Run = cli.IngressBudgetConfig{QPS: 100, Burst: 100}
	limiter, err := newIngressLimiter(cfg, h.db, nil)
	require.NoError(t, err)

	require.NoError(t, limiter.AdmitIngress(ctx, overridePrincipal("c1"), callers.IngressRun))
	require.ErrorIs(t, limiter.AdmitIngress(ctx, overridePrincipal("c1"), callers.IngressRun),
		callers.ErrRateLimited, "行里的 burst=1 必须压过类别 burst=100")

	require.NoError(t, limiter.AdmitIngress(ctx, overridePrincipal("c-zero"), callers.IngressRun),
		"0/0 行不额外收紧，按类别预算")
	require.NoError(t, limiter.AdmitIngress(ctx, overridePrincipal("nobody"), callers.IngressRun),
		"没有 limit 行的调用方按类别预算")

	require.NoError(t, limiter.AdmitIngress(ctx, overridePrincipal("c-burst"), callers.IngressRun))
	require.NoError(t, limiter.AdmitIngress(ctx, overridePrincipal("c-burst"), callers.IngressRun))
	require.ErrorIs(t, limiter.AdmitIngress(ctx, overridePrincipal("c-burst"), callers.IngressRun),
		callers.ErrRateLimited, "只覆盖 burst=2：第三个请求被拒")

	// 覆盖按调用方隔离：c1 打满不影响 c-zero。
	require.NoError(t, limiter.AdmitIngress(ctx, overridePrincipal("c-zero"), callers.IngressRun))
}
