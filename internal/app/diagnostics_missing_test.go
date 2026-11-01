//go:build integration

package app

// 本文件判"没有记录时必须显式说 missing，不得用空集冒充"（US-16 / T097；模块卡
// G1-24 的 `TestDiagnostics_MissingFlagWhenNoRecords`，`us16-pg` 具名用例）。
//
// 形态：走**生产装配**跑一次真实 Run（模型是脚本替身，不联网），诊断事件因此由真实
// 写入路径落进夹具自己的隔离 schema；然后分两层判：
//
//  1. **读面**：装配期同一个 `Reader` 读出两页——刚跑过的 Run 非空且逐行挂这个
//     `run_id`；从未出现过的 `run_id` 是空页 + `nil` 错误（"查过了没有"不是故障）。
//     这一层同时证明"写进入口 → 存储 → 读面"是同一条链路，而不是两个各写各的实例。
//  2. **端点**：经真实 HTTP 判标记——有行 → `missing=false`；无行 → `missing=true`，
//     且**不是** `sampled_out`（全量采样下把"没写"说成"被采样丢掉"，会把排障方向从
//     写入路径拐到采样率上；两者互斥，R23 裁决 4）。
//
// 逆改点：把 handler 里置 `Missing` 的分支删掉 → 无记录组红；让 `Reader` 在无行时返回
// 错误 → 端点回 500，无记录组红；把诊断装饰器从运行路径上摘掉 → 第一组（非空）红。
//
// The verdicts: the run path really writes diagnostics into the same store the reader
// reads, and an empty page is reported as `missing` rather than presented as "all good".

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/web"
)

// diagnosticsPage 是 `GET /api/v1/diagnostics` 的响应形状：统一信封 + `data` 下的页。
// 少解一层 `data` 会把 `missing` 读成零值，判据静默失效。
// diagnosticsPage mirrors the envelope; decoding one level short would zero the flags.
type diagnosticsPage struct {
	Code string `json:"code"`
	Data struct {
		Events []struct {
			EventID     string `json:"event_id"`
			Kind        string `json:"kind"`
			Correlation struct {
				RunID string `json:"run_id"`
			} `json:"correlation"`
		} `json:"events"`
		Missing       bool `json:"missing"`
		SampledOut    bool `json:"sampled_out"`
		RetentionDays int  `json:"retention_days"`
	} `json:"data"`
}

// bootDiagnosticsEndpoint 起一个真实监听的 HTTP 服务，只把诊断端口换成给定实现；
// 其余端口是同一包内的惰性替身（本用例不经过它们）。返回基址与管理员 token。
// bootDiagnosticsEndpoint serves the real router with the given diagnostics port.
func bootDiagnosticsEndpoint(t *testing.T, port web.DiagnosticsReader) (string, string) {
	t.Helper()
	const token = "diagnostics-missing-token-7c31"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	bound := ln.Addr().String()
	guard, err := web.NewAuthGuard(web.AdminAuthConfig{
		Mode:           adminAuthModeToken,
		Token:          token,
		AllowedHosts:   []string{bound},
		AllowedOrigins: []string{"http://" + bound},
	}, discardDiagnosticsLogger(), time.Now)
	require.NoError(t, err)
	srv, err := web.NewServer(web.Options{
		Addr:        bound,
		Listener:    ln,
		AdminAuth:   guard,
		Ingress:     allowAllIngress{},
		Diagnostics: port,
		Runs:        stubRunsPort{},
		Sessions:    stubSessionsPort{},
		Agents:      stubAgentProfilesPort{},
		Tools:       stubToolCatalogPort{},
		Memory:      stubMemoryPort{},
		Console:     assets.Console(),
		Logger:      discardDiagnosticsLogger(),
		Now:         time.Now,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("HTTP 服务未在时限内收尾")
		}
	})
	return "http://" + bound, token
}

// readDiagnosticsPage 发一次带管理员凭据的读取并解码统一信封。
// readDiagnosticsPage performs one authenticated read and decodes the envelope.
func readDiagnosticsPage(t *testing.T, base, token string, params url.Values) diagnosticsPage {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/diagnostics?"+params.Encode(), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var page diagnosticsPage
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&page))
	require.Equal(t, "OK", page.Code)
	return page
}

// TestDiagnostics_MissingFlagWhenNoRecords 判读面与端点两层：先有真实事件，再判空页。
// TestDiagnostics_MissingFlagWhenNoRecords judges both the read path and the endpoint.
func TestDiagnostics_MissingFlagWhenNoRecords(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "runner")
	fx.override = &scriptedProviderClient{replies: []core.ChatResponse{{Content: "done"}}}
	rt := fx.assemble(t)

	ctx := context.Background()
	res, err := rt.service.Run(ctx, core.RunRequest{
		Principal: core.LocalAdminPrincipal(),
		Agent:     "runner",
		SessionID: "diagnostics-missing",
		Message:   "你好",
		Channel:   "cli",
	})
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)
	require.NotEmpty(t, res.RunID, "Run 必须回带标识，后续两层都靠它定位")

	// ① 读面：同一存储、同一 Reader。刚跑过的 Run 有事件，没跑过的 Run 是空页。
	// The same reader sees the events the run path just wrote, and an empty page otherwise.
	recorded, err := rt.diagnostics.List(ctx, diagnostics.Query{RunID: res.RunID})
	require.NoError(t, err)
	require.NotEmpty(t, recorded, "运行路径必须真的发出诊断事件——空页说明生产端没接上")
	kinds := make([]string, len(recorded))
	for i, ev := range recorded {
		require.Equal(t, res.RunID, ev.Correlation.RunID, "事件必须挂在这一个 Run 上")
		kinds[i] = string(ev.Kind)
	}
	require.Contains(t, kinds, string(diagnostics.KindLLMCall), "模型调用必须有 llm_call 事件")

	empty, err := rt.diagnostics.List(ctx, diagnostics.Query{RunID: "run-never-ran"})
	require.NoError(t, err, "没有记录不是错误")
	require.Empty(t, empty)

	// ② 端点：有行 → missing=false；无行 → missing=true 且不是 sampled_out。
	// 有记录的一页不得被报成缺失，无记录的一页不得被报成"一切正常"。
	base, token := bootDiagnosticsEndpoint(t, rt.diagnostics)

	page := readDiagnosticsPage(t, base, token, url.Values{"run_id": {res.RunID}})
	require.False(t, page.Data.Missing, "有记录时不得报 missing")
	require.False(t, page.Data.SampledOut)
	require.NotEmpty(t, page.Data.Events)
	for _, ev := range page.Data.Events {
		require.Equal(t, res.RunID, ev.Correlation.RunID)
	}
	require.Equal(t, diagnostics.DefaultRetentionDays, page.Data.RetentionDays,
		"端点必须报告生效的保留期，否则管理台无法判断证据边界")

	page = readDiagnosticsPage(t, base, token, url.Values{"run_id": {"run-never-ran"}})
	require.Empty(t, page.Data.Events)
	require.True(t, page.Data.Missing, "全量采样下没有记录必须报 missing=true，不得用空集冒充")
	require.False(t, page.Data.SampledOut, "missing 与 sampled_out 互斥")
	require.Equal(t, diagnostics.DefaultRetentionDays, page.Data.RetentionDays)
}
