package web

// 本文件是 C-35 第一例 `TestIngress_HTTPIngestRateLimited` 的落地（`us16-pg` 具名
// 用例）：摄取上传走的是 `IngressIngest` 入口桶，**不是** Run 桶——上传一张文档
// 也必须受入口速率约束，否则"不走 Run 的写入口"就成了不被计量的旁路。
//
// 与 `middleware_ingress_rate_test.go` 的分工：那边判中间件的分支（401 / 403 文案、
// 是否扣桶、端口故障失败关闭）与路由映射表；这边用**真实令牌桶**判端到端的一条
// 具体业务链路，并把"是否触达摄取端口"作为副作用判据。
// The route mapping is pinned there; this file pins the real bucket against the real
// ingest route and proves the port is not reached once the bucket is empty.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// recordingKnowledgeIngest 记录摄取端口的调用次数；本文件靠"被限流后计数不增"
// 证明判定发生在处理函数之前。
// recordingKnowledgeIngest counts ingest calls; the port is the side-effect probe.
type recordingKnowledgeIngest struct {
	ingests int
}

// Ingest 记录一次摄取并回一个形状合法的结果。
func (f *recordingKnowledgeIngest) Ingest(
	context.Context, knowledge.IngestRequest,
) (knowledge.IngestResult, error) {
	f.ingests++
	return knowledge.IngestResult{
		DocumentID:  "doc-1",
		VersionID:   "ver-1",
		IngestJobID: "job-1",
		Status:      knowledge.VersionActive,
	}, nil
}

// Delete 不参与本文件判据。
func (f *recordingKnowledgeIngest) Delete(context.Context, knowledge.DeleteRequest) error {
	return nil
}

// Reindex 不参与本文件判据。
func (f *recordingKnowledgeIngest) Reindex(
	context.Context, knowledge.ReindexRequest,
) (knowledge.IngestResult, error) {
	return knowledge.IngestResult{}, nil
}

// count 返回摄取端口的调用次数。
// count reports how many ingest calls reached the port.
func (f *recordingKnowledgeIngest) count() int { return f.ingests }

// TestIngress_HTTPIngestRateLimited 判摄取上传受入口速率约束：固定时钟、burst=1
// 时第一次上传到达端口，第二次在上传解析之前就被 429 挡下，且固定
// `CALLER_RATE_LIMITED` + `Retry-After`。
//
// 逆改点：把摄取行的 ingress 换成 `IngressRun` 或干脆不映射 → `Ingest` 预算不再
// 被消耗，第二次仍到达端口（计数 2）；把速率判定挪到处理函数之后 → 同样计数 2；
// 去掉 Retry-After → 头断言红。
// Reverting the route class or moving the check behind the handler lets the second
// upload reach the port, which the count assertion catches.
func TestIngress_HTTPIngestRateLimited(t *testing.T) {
	t.Parallel()
	buckets, err := callers.NewRateBucket(callers.IngressLimits{
		Run:    callers.IngressBudget{QPS: 1, Burst: 1},
		Ingest: callers.IngressBudget{QPS: 1, Burst: 1},
		Read:   callers.IngressBudget{QPS: 1, Burst: 1},
	}, func() time.Time { return authFixedNow })
	require.NoError(t, err)

	ingest := &recordingKnowledgeIngest{}
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"ing-secret": callerPrincipal("c1", core.ScopeKnowledgeWrite),
	}}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = businessGuard(t, keys)
		o.Ingress = callers.NewLimiter(callers.LimiterDeps{Buckets: buckets})
		o.KnowledgeIngest = ingest
	})

	upload := func() *httptest.ResponseRecorder {
		req := withBearer(adminRequest(http.MethodPost, "/api/v1/collections/col-1/documents",
			`{"path":"docs/runbook.md"}`), "ing-secret")
		return serveAdmin(srv, req)
	}

	first := upload()
	require.Equal(t, http.StatusOK, first.Code, "第一次上传必须到达摄取端口：%s", first.Body.String())
	require.Equal(t, 1, ingest.count(), "阳性对照：第一次上传确实执行了摄取")

	// 拒绝面的四条判据用 assert：它们是同一条性质的多个观察点，逆改后要一次看到
	// 状态码、错误码、等待头与端口副作用**同时**破了，而不是被第一条挡住。
	// Non-fatal so one mutation reports every face the bypass opens.
	second := upload()
	assert.Equal(t, http.StatusTooManyRequests, second.Code, "摄取上传必须受入口速率约束")
	assert.Equal(t, codeCallerRateLimited, decodeAuthEnvelope(t, second).Code)
	assert.NotEmpty(t, second.Header().Get("Retry-After"), "速率拒绝必须带 Retry-After")
	assert.Equal(t, 1, ingest.count(), "被限流的上传不得触达摄取端口")
}
