//go:build integration

package web

// 本文件是 C-35 `TestIngress_RateDeniedDoesNotTakeRunSlot` 的落地（`us16-pg` 具名
// 用例）：图 6 [1] 的入口速率拒绝必须发生在 [3]（调用方并发名额）与运行入口之前——
// 被限流的请求**不得**触达 `RunService`，因此也不会有任何名额被取走。
//
// 与 `internal/callers/limiter_test.go` 的
// `TestLimiterAdmitIngress_DenialDoesNotTakeRunSlot` 的分工：那边判限制器内部两段
// 顺序（`AdmitIngress` 被拒时未进 `AdmitRun`）；这边从真实路由表出发判**生产链路**
// ——中间件拒绝之后处理函数根本不被调用，运行入口的调用计数就是副作用判据。
// The limiter test pins the two stages inside the limiter; this one pins the whole
// production chain: a rate refusal must never reach the run entry at all.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// TestIngress_RateDeniedDoesNotTakeRunSlot 判 Run 类业务行受入口速率约束，且拒绝
// 先于运行入口：固定时钟、burst=1 时第一次 invoke 触达运行入口，第二次在路由处理
// 之前就被 429 挡下，运行入口计数不增。
//
// 逆改点：把 invoke 行的 ingress 换成 `IngressIngest` 或不映射 → 第二次仍触达运行
// 入口（计数 2；不映射还会被启动自检 `checkRouteIngress` 直接拒绝装配）；把速率
// 判定挪到处理函数之后 → 同样计数 2；去掉 `Retry-After` → 头断言红。
// Reverting the route class or moving the check behind the handler touches the run
// entry twice, which the count assertion catches.
func TestIngress_RateDeniedDoesNotTakeRunSlot(t *testing.T) {
	t.Parallel()
	// 三类桶给出**不同**容量：Run 桶只有 1 个令牌，读 / 摄取桶给足。这样"invoke 行
	// 映射到 Run 桶"才是可判的——把行改映射到另外两类，第二次请求就会放行。
	// Distinct budgets make the route's class observable: only the Run bucket runs dry.
	buckets, err := callers.NewRateBucket(callers.IngressLimits{
		Run:    callers.IngressBudget{QPS: 1, Burst: 1},
		Ingest: callers.IngressBudget{QPS: 100, Burst: 100},
		Read:   callers.IngressBudget{QPS: 100, Burst: 100},
	}, func() time.Time { return authFixedNow })
	require.NoError(t, err)

	runs := &recordingRunService{}
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"run-secret": callerPrincipal("c1", core.AgentInvokeScopePrefix+"ops-agent"),
	}}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = businessGuard(t, keys)
		o.Ingress = callers.NewLimiter(callers.LimiterDeps{Buckets: buckets})
		o.Runs = runs
	})

	invoke := func() *httptest.ResponseRecorder {
		req := withBearer(adminRequest(http.MethodPost,
			"/api/v1/agents/ops-agent/invoke", `{"message":"hi"}`), "run-secret")
		return serveAdmin(srv, req)
	}

	first := invoke()
	require.Equal(t, http.StatusOK, first.Code, "第一次 invoke 必须到达运行入口：%s", first.Body.String())
	require.Len(t, runs.requests(), 1, "阳性对照：第一次 invoke 确实调用了运行入口")

	// 拒绝面的三条判据用 assert：它们观察同一条性质的不同侧面，逆改后要一次看到
	// 状态码、错误码、等待头与运行入口副作用**同时**破了，而不是被第一条挡住。
	// Non-fatal so one mutation reports every face the bypass opens.
	second := invoke()
	assert.Equal(t, http.StatusTooManyRequests, second.Code, "Run 类业务行必须受入口速率约束")
	assert.Equal(t, codeCallerRateLimited, decodeAuthEnvelope(t, second).Code)
	assert.NotEmpty(t, second.Header().Get("Retry-After"), "速率拒绝必须带 Retry-After")
	assert.Len(t, runs.requests(), 1, "被限流的请求不得触达运行入口，因而不占并发名额")
}
