//go:build integration

package app

// 本文件判 `request_id` 锚点的**真实库往返**（A-F2 / C-F02）：Run 准入在 Run 账本
// 创建之前被拒时还没有 `run_id`，管理端只能按服务端生成的 `request_id` 定位事件。
// 三层判据：
//
//  1. **生产端**：[3] 准入拒绝经生产 `runAdmission` 落一条 `quota_decision`；
//  2. **读面**：同一个 `diagnostics.Reader` 按 `request_id` 查回同一条，另一串 id 空页；
//  3. **端点**：真实 HTTP `GET /api/v1/diagnostics?request_id=…` 查回同一条。
//
// 逆改点：`run_admission.go` 不传 `RequestID` → 第 1、2 层红；web 层不解析
// `request_id` → 端点层 400；读面按 run_id 过滤（忽略 request_id）→ 第 2 层红。
//
// The verdict: a request-anchored admission denial written by the production path is
// readable back through the reader and the live endpoint on a real PostgreSQL.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// requestAnchorPage 是端点响应的最小形状：只解本用例要判的关联字段。
// requestAnchorPage decodes only the correlation fields this case judges.
type requestAnchorPage struct {
	Code string `json:"code"`
	Data struct {
		Events []struct {
			Kind        string `json:"kind"`
			Decision    string `json:"decision"`
			ReasonCode  string `json:"reason_code"`
			Correlation struct {
				RequestID string `json:"request_id"`
				RunID     string `json:"run_id"`
			} `json:"correlation"`
		} `json:"events"`
		Missing bool `json:"missing"`
	} `json:"data"`
}

// TestDiagnostics_ReadByRequestIDAfterAdmissionDenial 守无 Run 准入拒绝的 request_id 关联。
// TestDiagnostics_ReadByRequestIDAfterAdmissionDenial pins the request-anchored denial.
func TestDiagnostics_ReadByRequestIDAfterAdmissionDenial(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "runner")
	rt := fx.assemble(t)

	const requestID = "0f0e0d0c0b0a0908"

	// ① 生产端：身份非法的请求在 [3] 被拒，Run 从未创建（没有 run_id 可锚定）。
	_, err := rt.admission.run(context.Background(), core.RunRequest{
		RequestID: requestID,
		Agent:     "runner",
	}, func(context.Context, core.RunRequest) (core.RunResult, error) {
		t.Fatal("准入被拒的请求不得进入执行体")
		return core.RunResult{}, nil
	})
	require.ErrorIs(t, err, core.ErrUnauthorized)

	// ② 读面：真实 PostgreSQL 按 request_id 查回同一条。
	events, err := rt.diagnostics.List(context.Background(), diagnostics.Query{RequestID: requestID})
	require.NoError(t, err)
	require.Len(t, events, 1, "request_id 必须能查回无 Run 的准入拒绝")
	require.Equal(t, diagnostics.KindQuotaDecision, events[0].Kind)
	require.Equal(t, diagnostics.DecisionDeny, events[0].Decision)
	require.Equal(t, requestID, events[0].Correlation.RequestID)
	require.Empty(t, events[0].Correlation.RunID, "事件不得伪造 run_id")

	none, err := rt.diagnostics.List(context.Background(), diagnostics.Query{RequestID: "ffffffffffffffff"})
	require.NoError(t, err, "没有记录不是错误")
	require.Empty(t, none, "无匹配是空页")

	// ③ 端点：真实 HTTP 按 request_id 查回同一条。
	base, token := bootDiagnosticsEndpoint(t, rt.diagnostics)
	req, err := http.NewRequest(http.MethodGet,
		base+"/api/v1/diagnostics?"+url.Values{"request_id": {requestID}}.Encode(), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var page requestAnchorPage
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&page))
	require.Equal(t, "OK", page.Code)
	require.Len(t, page.Data.Events, 1)
	require.Equal(t, requestID, page.Data.Events[0].Correlation.RequestID)
	require.False(t, page.Data.Missing)
}
