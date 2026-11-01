package app

// 本文件是诊断读取端点的**接线用例**（US-16 / T097 / T101 的认证面；`us16-wiring`
// 的具名用例 `TestDiagnostics_ReadRequiresAdminAuth`）。不依赖数据库：端口是进程内替身，
// 判据落在"认证闸门真的挡在端口之前"。
//
// 三条**契约级**要求（openapi 根 security 只有 `bearerAuth` / `localTrust`，没有
// `callerKey`）：
//
//  1. 无凭据 → 401，且**端口零调用**（认证必须先于任何读取）；
//  2. 有效 caller key → 403（"拿错了面"是 403，不是 401，也不计失败配额）；
//  3. 管理员 token → 200，端口恰好调用一次且收到 `run_id`。
//
// 逆改点：把路由行从 `adminHandler` 改成业务面（加 scope / ingress）→ 第 2 条变成
// 200 或 500；把认证放到 handler 之后（先读端口再判身份）→ "端口零调用"三条断言红；
// 端口 nil 时回 200 + 空集 → 第 4 组断言红（装配缺失必须失败关闭）。
//
// This file pins the diagnostics endpoint's authentication surface: the port is never
// reached before the guard, a valid caller key gets 403, and a missing assembly fails
// closed instead of returning an empty page.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/assets"
	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/web"
)

// stubDiagnosticsReader 是诊断读取端口的进程内替身：记下每次查询与调用次数。
// stubDiagnosticsReader records every query so the "never reached" assertions hold.
type stubDiagnosticsReader struct {
	mu      sync.Mutex
	queries []diagnostics.Query
	events  []diagnostics.DiagnosticEvent
	config  diagnostics.Config
}

// List 记录查询并返回预置事件。
func (s *stubDiagnosticsReader) List(_ context.Context, q diagnostics.Query) ([]diagnostics.DiagnosticEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, q)
	return s.events, nil
}

// Config 返回预置的生效配置。
func (s *stubDiagnosticsReader) Config() diagnostics.Config { return s.config }

// callCount / lastQuery 是加锁读面（-race 下不得直接读未加锁字段）。
func (s *stubDiagnosticsReader) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queries)
}

func (s *stubDiagnosticsReader) lastQuery() diagnostics.Query {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queries) == 0 {
		return diagnostics.Query{}
	}
	return s.queries[len(s.queries)-1]
}

// stubPrincipals 是调用方 Key 解析端口的替身：只认一个有效 Key。
// stubPrincipals recognises exactly one caller key.
type stubPrincipals struct {
	key       string
	principal core.Principal
}

// Authenticate 逐字比对凭据；其余一律失败（失败关闭，不回落管理员）。
func (s stubPrincipals) Authenticate(_ context.Context, presented string) (core.Principal, error) {
	if presented == s.key {
		return s.principal, nil
	}
	return core.Principal{}, errors.New("未知凭据")
}

// 以下五个替身只用来满足 `web.NewServer` 的必需端口集合：本用例判的是诊断端点的
// 认证次序，其他端口一旦被调用就说明请求走错了行——因此除 `Memory` 外全部返回零值，
// 断言不依赖它们的返回值。
// Five inert ports satisfy the server's required set; this case judges the diagnostics
// row's authentication order and nothing else.

// stubRunsPort 是运行端口的空实现。
type stubRunsPort struct{}

func (stubRunsPort) Run(context.Context, core.RunRequest) (core.RunResult, error) {
	return core.RunResult{}, nil
}

func (stubRunsPort) Archive(context.Context, string) error { return nil }

func (stubRunsPort) ListRuns(context.Context, core.RunFilter) ([]core.RunDetail, error) {
	return nil, nil
}

func (stubRunsPort) GetRun(context.Context, string) (core.RunDetail, error) {
	return core.RunDetail{}, nil
}

func (stubRunsPort) Cancel(context.Context, string) (core.RunDetail, error) {
	return core.RunDetail{}, nil
}

// stubSessionsPort 是会话端口的空实现。
type stubSessionsPort struct{}

func (stubSessionsPort) GetOrCreateSession(context.Context, core.SessionRecord) (core.SessionRecord, bool, error) {
	return core.SessionRecord{}, false, nil
}

func (stubSessionsPort) GetSession(context.Context, string) (core.SessionRecord, error) {
	return core.SessionRecord{}, nil
}

func (stubSessionsPort) MessagesTail(context.Context, string, int) ([]core.Message, error) {
	return nil, nil
}

func (stubSessionsPort) ListSessions(context.Context, core.SessionFilter) ([]core.SessionRecord, error) {
	return nil, nil
}

// stubAgentProfilesPort / stubToolCatalogPort / stubMemoryPort 是其余三个必需端口。
type stubAgentProfilesPort struct{}

func (stubAgentProfilesPort) Profiles() []core.AgentEntry { return nil }

type stubToolCatalogPort struct{}

func (stubToolCatalogPort) Tools() []web.ToolDescriptor { return nil }

type stubMemoryPort struct{}

func (stubMemoryPort) Sections(context.Context, core.Scope) (string, string, error) {
	return "", "", nil
}

// allowAllIngress 是入口速率端口的替身：本用例判认证次序，不判限流。
// allowAllIngress admits every request; this case judges ordering, not rate limits.
type allowAllIngress struct{}

// AdmitIngress 恒放行。
func (allowAllIngress) AdmitIngress(context.Context, core.Principal, callers.IngressClass) error {
	return nil
}

// TestDiagnostics_ReadRequiresAdminAuth 判管理面的三条认证结论与一条装配缺失结论。
// TestDiagnostics_ReadRequiresAdminAuth pins the authentication contract of the endpoint.
func TestDiagnostics_ReadRequiresAdminAuth(t *testing.T) {
	const (
		adminToken = "diagnostics-admin-token-4d21"
		callerKey  = "caller-key-diagnostics-9a13"
	)
	port := &stubDiagnosticsReader{config: diagnostics.DefaultConfig()}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	bound := ln.Addr().String()
	guard, err := web.NewAuthGuardWithPrincipals(web.AdminAuthConfig{
		Mode:           adminAuthModeToken,
		Token:          adminToken,
		AllowedHosts:   []string{bound},
		AllowedOrigins: []string{"http://" + bound},
	}, discardDiagnosticsLogger(), time.Now, stubPrincipals{key: callerKey, principal: core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller-diag",
		KeyID:    "key_diag",
		Scopes:   []string{core.ScopeSessionsRead},
		Actor:    core.ActorCaller,
	}})
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

	base := "http://" + bound + "/api/v1/diagnostics?run_id=run-diag-1"
	get := func(credential string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base, nil)
		require.NoError(t, err)
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
		return resp
	}

	// ① 无凭据：401，且读取端口**零调用**——认证必须挡在读库之前。
	require.Equal(t, http.StatusUnauthorized, get("").StatusCode, "缺凭据必须是 401")
	require.Zero(t, port.callCount(), "认证失败前不得读取诊断事件")

	// ② 无效凭据：401，同样不触达端口。
	require.Equal(t, http.StatusUnauthorized, get("not-a-valid-credential").StatusCode)
	require.Zero(t, port.callCount(), "无效凭据不得读取诊断事件")

	// ③ 有效 caller key：403（契约 §8：两面凭据不互换），且仍未读取。
	require.Equal(t, http.StatusForbidden, get(callerKey).StatusCode,
		"有效 caller key 打管理面必须是 403，不是 401")
	require.Zero(t, port.callCount(), "caller key 不得读取诊断事件")

	// ④ 管理员 token：200，端口恰好一次，且拿到 run_id 锚点。
	resp := get(adminToken)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 1, port.callCount(), "管理员请求必须恰好读取一次")
	require.Equal(t, "run-diag-1", port.lastQuery().RunID, "锚点必须原样传给端口")
	require.Equal(t, defaultDiagnosticLimitOfContract(), port.lastQuery().Limit,
		"缺省 limit 取契约默认值 200")

	// 响应走统一信封（`{code, data, ...}`），载荷在 `data` 下——按契约形状解包，
	// 不多不少（少解一层会把"missing=true"读成零值，判据静默失效）。
	// The payload sits under the envelope's data key; decoding one level short would
	// silently read the missing flag as its zero value.
	var envelope struct {
		Code string `json:"code"`
		Data struct {
			Events        []json.RawMessage `json:"events"`
			Missing       bool              `json:"missing"`
			SampledOut    bool              `json:"sampled_out"`
			RetentionDays int               `json:"retention_days"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&envelope))
	require.Equal(t, "OK", envelope.Code)
	require.Empty(t, envelope.Data.Events)
	require.True(t, envelope.Data.Missing, "全量采样下无记录必须报 missing=true，不得用空集冒充")
	require.False(t, envelope.Data.SampledOut, "missing 与 sampled_out 互斥")
	require.Equal(t, diagnostics.DefaultRetentionDays, envelope.Data.RetentionDays)

	// ⑤ 装配缺失：端口 nil → 固定 500（失败关闭），不得回 200 + 空集。
	missingLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	missingBound := missingLn.Addr().String()
	missingGuard, err := web.NewAuthGuard(web.AdminAuthConfig{
		Mode:           adminAuthModeToken,
		Token:          adminToken,
		AllowedHosts:   []string{missingBound},
		AllowedOrigins: []string{"http://" + missingBound},
	}, discardDiagnosticsLogger(), time.Now)
	require.NoError(t, err)
	missingSrv, err := web.NewServer(web.Options{
		Addr:      missingBound,
		Listener:  missingLn,
		AdminAuth: missingGuard,
		Ingress:   allowAllIngress{},
		// 刻意**不**给 Diagnostics：本组判的就是"端口缺失必须失败关闭"。
		// Diagnostics is deliberately absent: this case judges the fail-closed exit.
		Runs:     stubRunsPort{},
		Sessions: stubSessionsPort{},
		Agents:   stubAgentProfilesPort{},
		Tools:    stubToolCatalogPort{},
		Memory:   stubMemoryPort{},
		Console:  assets.Console(),
		Logger:   discardDiagnosticsLogger(),
		Now:      time.Now,
	})
	require.NoError(t, err)
	missingCtx, missingCancel := context.WithCancel(context.Background())
	missingDone := make(chan error, 1)
	go func() { missingDone <- missingSrv.Run(missingCtx) }()
	t.Cleanup(func() {
		missingCancel()
		select {
		case err := <-missingDone:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("HTTP 服务未在时限内收尾")
		}
	})
	req, err := http.NewRequest(http.MethodGet,
		"http://"+missingBound+"/api/v1/diagnostics?run_id=run-diag-1", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"诊断端口未装配时必须失败关闭，不得以空集冒充可用")
}

// discardDiagnosticsLogger 丢弃日志：本用例的 401 / 403 / 500 都是**预期**拒绝，
// 留痕另有专门断言，不往测试输出里灌。
// discardDiagnosticsLogger silences the expected denials; their evidence is asserted
// elsewhere.
func discardDiagnosticsLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// defaultDiagnosticLimitOfContract 是契约的 `limit` 默认值（minimum 1 / maximum 1000 /
// default 200）。用例侧独立写一份期望值，不从被测实现读常量——读实现会让"默认值写错"
// 这类回归永远绿。
// defaultDiagnosticLimitOfContract mirrors the contract default; the expectation is not
// read from the implementation under test.
func defaultDiagnosticLimitOfContract() int { return 200 }
