package web

// 本文件是系统概览与进程采样的 handler 层验收（模块卡 §4.3 handlers_system_test.go
// 12 例；§4.4 的选择器家族为 TestSystem_*）。断言守点：概览计数只来自注册表快照
// 与生命周期读、数据库不可用时 200 + status=unavailable 且计数省略（不补 0）、
// 首次采样无 CPU 基线为 null + unavailable、5 秒缓存、平台不支持字段一律 null。
// Handler-level acceptance for /system/overview and /system/sample.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeSystemReader 记录概览读数调用次数，按用例预置读数或错误。
// fakeSystemReader returns preset overview readings and counts calls.
type fakeSystemReader struct {
	mu       sync.Mutex
	readings SystemReadings
	err      error
	calls    int
}

func (f *fakeSystemReader) ReadOverview(context.Context) (SystemReadings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.readings, f.err
}

// fakeAgentRegistry 是只读注册表快照的最小实现。
// fakeAgentRegistry is the minimal immutable snapshot port.
type fakeAgentRegistry struct{ entries []core.AgentEntry }

func (f fakeAgentRegistry) Entries() []core.AgentEntry { return f.entries }

func (f fakeAgentRegistry) Lookup(name string) (core.AgentEntry, bool) {
	for _, entry := range f.entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return core.AgentEntry{}, false
}

// fakeLifecycle 只实现 Read：概览不迁移状态，其余方法出现即说明用错了端口。
// fakeLifecycle implements Read only; the overview never mutates lifecycle state.
type fakeLifecycle struct {
	mu    sync.Mutex
	recs  map[string]core.LifecycleRecord
	errs  map[string]error
	reads int
}

func (f *fakeLifecycle) Read(_ context.Context, agent string) (core.LifecycleRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := f.errs[agent]; err != nil {
		return core.LifecycleRecord{}, false, err
	}
	rec, ok := f.recs[agent]
	return rec, ok, nil
}

func (f *fakeLifecycle) Transition(context.Context, string, core.AgentState, int64, core.AgentState, string) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, errors.New("fakeLifecycle: Transition 不在概览路径上")
}

func (f *fakeLifecycle) Bootstrap(context.Context, string, string) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, errors.New("fakeLifecycle: Bootstrap 不在概览路径上")
}

func (f *fakeLifecycle) Discard(context.Context, string, string) error {
	return errors.New("fakeLifecycle: Discard 不在概览路径上")
}

// decodeSystemOverview 解出概览视图并校验信封码。
// decodeSystemOverview decodes the overview view and asserts the envelope code.
func decodeSystemOverview(t *testing.T, rec *httptest.ResponseRecorder) SystemOverviewView {
	t.Helper()
	var env struct {
		Code string             `json:"code"`
		Data SystemOverviewView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, codeOK, env.Code)
	return env.Data
}

// decodeSystemDataKeys 解出 data 对象的原始键集，供"键缺失 / null"断言。
// decodeSystemDataKeys exposes the raw data object for key-presence asserts.
func decodeSystemDataKeys(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var env struct {
		Code string                     `json:"code"`
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, codeOK, env.Code)
	return env.Data
}

// sampleClock 是可推进的测试时钟。
// sampleClock is an advanceable test clock.
type sampleClock struct{ now time.Time }

func (c *sampleClock) Now() time.Time { return c.now }

// stubProcessReaders 注入平台读数并在用例结束后还原（包级接缝，串行用例专用）。
// stubProcessReaders swaps the platform readers for one test and restores them.
func stubProcessReaders(t *testing.T, cpu func() (cpuTime, bool), rss func() (int64, bool)) {
	t.Helper()
	prevCPU, prevRSS := processCPU, processRSS
	processCPU, processRSS = cpu, rss
	t.Cleanup(func() { processCPU, processRSS = prevCPU, prevRSS })
}

// TestSystem_OverviewCounts 覆盖概览的正常读数：版本 / 运行时长 / Agent 四态计数 /
// 运行与资源计数 / 数据库状态 / 认证模式（模块卡 §4.3 第 1 行）。
// 逆改点：Agent 计数漏掉 invalid、runs 计数取错来源、uptime 取错起点 → 对应断言红。
func TestSystem_OverviewCounts(t *testing.T) {
	clock := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	reader := &fakeSystemReader{readings: SystemReadings{
		RunsRunning:       3,
		RunsLast24h:       12,
		RunsFailedLast24h: 2,
		ModelConnections:  4,
		MCPServers:        5,
		Skills:            6,
		SchemaVersion:     2,
	}}
	lifecycle := &fakeLifecycle{recs: map[string]core.LifecycleRecord{
		"alpha": {State: core.AgentActive},
		"beta":  {State: core.AgentDisabled},
		"gamma": {State: core.AgentArchived},
	}}
	s := newTestServer(t, func(opts *Options) {
		opts.StartedAt = clock.Add(-90 * time.Minute)
		opts.AdminAuth = testAdminAuth("s3cret")
		opts.System = reader
		opts.Registry = fakeAgentRegistry{entries: []core.AgentEntry{
			{Name: "alpha", Registered: true},
			{Name: "beta", Registered: true},
			{Name: "gamma", Registered: true},
			{Name: "delta", Registered: true},
			{Name: "broken", Registered: false},
		}}
		opts.Lifecycle = lifecycle
	})

	rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/system/overview", ""), "s3cret"))
	require.Equal(t, http.StatusOK, rec.Code)
	view := decodeSystemOverview(t, rec)

	require.Equal(t, "test", view.Version)
	require.Equal(t, int64(90*60), view.UptimeSeconds, "运行时长从 StartedAt 起算")
	require.Equal(t, "2026-10-18T10:30:00Z", view.StartedAt)
	require.Equal(t, SystemAgentsView{Total: 5, Active: 1, Disabled: 1, Archived: 1, Invalid: 2}, view.Agents)
	require.Equal(t, &SystemRunsView{Running: 3, Last24h: 12, FailedLast24h: 2}, view.Runs)
	require.Equal(t, &SystemResourcesView{ModelConnections: 4, MCPServers: 5, Skills: 6}, view.Resources)
	require.Equal(t, databaseStatusOK, view.Database.Status)
	require.NotNil(t, view.Database.SchemaVersion)
	require.Equal(t, 2, *view.Database.SchemaVersion)
	require.Equal(t, adminAuthModeToken, view.Auth.Mode, "认证模式取自闸门配置")
	require.True(t, view.Auth.LoopbackOnly, "缺省监听 127.0.0.1 是回环")
	require.Equal(t, 1, reader.calls, "概览只发一次数据库读数")
	require.Equal(t, 4, lifecycle.reads, "每个已注册条目恰读一次生命周期（未注册项不读）")
}

// TestSystem_OverviewDatabaseUnavailable 覆盖数据库读失败：仍返回 200，只把
// database.status 置 unavailable，runs / resources 与 schema_version 整组省略
// （读不到不得补 0）；Agent 计数不依赖库，照常给出。
// 逆改点：失败时返回 500、把计数写成 0 或给出 schema_version → 对应断言红。
func TestSystem_OverviewDatabaseUnavailable(t *testing.T) {
	reader := &fakeSystemReader{err: errors.New("database is locked")}
	lifecycle := &fakeLifecycle{recs: map[string]core.LifecycleRecord{
		"alpha": {State: core.AgentActive},
	}}
	s := newTestServer(t, func(opts *Options) {
		opts.System = reader
		opts.Registry = fakeAgentRegistry{entries: []core.AgentEntry{{Name: "alpha", Registered: true}}}
		opts.Lifecycle = lifecycle
	})

	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/system/overview", ""))
	require.Equal(t, http.StatusOK, rec.Code, "库不可用不是 500：契约只为概览声明 200/401/403")

	keys := decodeSystemDataKeys(t, rec)
	require.NotContains(t, keys, "runs", "读不到运行计数时必须整组省略，而不是补 0")
	require.NotContains(t, keys, "resources", "读不到资源计数时必须整组省略")

	var database map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(keys["database"], &database))
	var status string
	require.NoError(t, json.Unmarshal(database["status"], &status))
	require.Equal(t, databaseStatusUnavailable, status)
	require.NotContains(t, database, "schema_version", "库不可用时不报版本号")

	var agents SystemAgentsView
	require.NoError(t, json.Unmarshal(keys["agents"], &agents))
	require.Equal(t, SystemAgentsView{Total: 1, Active: 1}, agents, "Agent 计数不依赖数据库")
}

// TestSystem_OverviewLifecycleAnomaliesCountInvalid 覆盖状态文件异常：读失败、
// 缺失与未知状态都计入 invalid，且读失败必须留 ERROR 日志（可观测性双轨）。
// 逆改点：把缺失状态当 active / 吞掉读错误不记日志 → 对应断言红。
func TestSystem_OverviewLifecycleAnomaliesCountInvalid(t *testing.T) {
	var logs bytes.Buffer
	lifecycle := &fakeLifecycle{
		recs: map[string]core.LifecycleRecord{
			"paused": {State: core.AgentState("paused")},
		},
		errs: map[string]error{"io-error": errors.New("permission denied")},
	}
	s := newTestServer(t, func(opts *Options) {
		opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		opts.System = &fakeSystemReader{}
		opts.Registry = fakeAgentRegistry{entries: []core.AgentEntry{
			{Name: "io-error", Registered: true},
			{Name: "missing", Registered: true},
			{Name: "paused", Registered: true},
			{Name: "rejected", Registered: false},
		}}
		opts.Lifecycle = lifecycle
	})

	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/system/overview", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	view := decodeSystemOverview(t, rec)
	require.Equal(t, SystemAgentsView{Total: 4, Invalid: 4}, view.Agents,
		"异常状态不得算成可用")
	require.Contains(t, logs.String(), "系统概览读取生命周期状态失败",
		"状态文件读失败必须有 ERROR 日志")
	require.Contains(t, logs.String(), "io-error")
}

// TestSystem_OverviewMissingPorts 覆盖装配缺端口：System / Registry / Lifecycle
// 任一为 nil 都返回 500 固定文案（缺端口是装配错误，不是运行时降级）。
// 逆改点：nil 端口下继续拼视图（会 panic 或伪造计数）→ 对应子例红。
func TestSystem_OverviewMissingPorts(t *testing.T) {
	reader := &fakeSystemReader{}
	registry := fakeAgentRegistry{entries: []core.AgentEntry{{Name: "alpha", Registered: true}}}
	lifecycle := &fakeLifecycle{recs: map[string]core.LifecycleRecord{"alpha": {State: core.AgentActive}}}

	cases := []struct {
		name   string
		mutate func(*Options)
	}{
		{"缺 System", func(opts *Options) { opts.Registry = registry; opts.Lifecycle = lifecycle }},
		{"缺 Registry", func(opts *Options) { opts.System = reader; opts.Lifecycle = lifecycle }},
		{"缺 Lifecycle", func(opts *Options) { opts.System = reader; opts.Registry = registry }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, func(opts *Options) { tc.mutate(opts) })
			rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/system/overview", ""))
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			var env Envelope
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			require.Equal(t, codeInternal, env.Code)
			require.Equal(t, messageInternal, env.Message)
			require.Equal(t, 0, reader.calls, "缺端口时不得触达数据库读数")
		})
	}
}

// TestSystem_OverviewAuthModes 覆盖认证视图：local_trust 模式如实上报；非回环
// 监听 loopback_only=false；没有闸门（handler 级装配）时 mode 省略。
// 逆改点：loopback_only 恒 true、mode 写死 token → 对应子例红。
func TestSystem_OverviewAuthModes(t *testing.T) {
	lifecycle := &fakeLifecycle{recs: map[string]core.LifecycleRecord{"alpha": {State: core.AgentActive}}}
	base := func(opts *Options) {
		opts.System = &fakeSystemReader{readings: SystemReadings{SchemaVersion: 2}}
		opts.Registry = fakeAgentRegistry{entries: []core.AgentEntry{{Name: "alpha", Registered: true}}}
		opts.Lifecycle = lifecycle
	}

	t.Run("local_trust 且非回环监听", func(t *testing.T) {
		guard, err := NewAuthGuard(AdminAuthConfig{
			Mode:           adminAuthModeLocalTrust,
			AllowedHosts:   []string{"127.0.0.1:8080"},
			AllowedOrigins: []string{"http://127.0.0.1:8080"},
		}, discardLogger(), nil)
		require.NoError(t, err)
		s := newTestServer(t, func(opts *Options) {
			base(opts)
			opts.Addr = "0.0.0.0:8080"
			opts.AdminAuth = guard
		})
		req := adminRequest(http.MethodGet, "/api/v1/system/overview", "")
		req.RemoteAddr = "127.0.0.1:52000"
		req.Header.Set(localTrustHeader, "1")
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusOK, rec.Code)
		view := decodeSystemOverview(t, rec)
		require.Equal(t, adminAuthModeLocalTrust, view.Auth.Mode)
		require.False(t, view.Auth.LoopbackOnly, "0.0.0.0 不是回环监听")
	})

	t.Run("无闸门时省略 mode", func(t *testing.T) {
		s := newTestServer(t, base)
		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/system/overview", ""))
		require.Equal(t, http.StatusOK, rec.Code)
		var raw struct {
			Auth map[string]json.RawMessage `json:"auth"`
		}
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.NoError(t, json.Unmarshal(env.Data, &raw))
		require.NotContains(t, raw.Auth, "mode", "无法确定闸门配置时不得猜模式")
		require.Contains(t, raw.Auth, "loopback_only")
	})
}

// TestSystem_SampleFirstHasNoCPUBaseline 覆盖首次采样：CPU 无差值基线 → null 并
// 列入 unavailable；RSS / goroutine 可读时如实给出（模块卡约束 33）。
// 逆改点：首次采样伪造 0、unavailable 为空 → 对应断言红。
func TestSystem_SampleFirstHasNoCPUBaseline(t *testing.T) {
	clock := &sampleClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	stubProcessReaders(t,
		func() (cpuTime, bool) {
			return cpuTime{User: 100 * time.Millisecond, System: 50 * time.Millisecond}, true
		},
		func() (int64, bool) { return 4096, true })
	s := newTestServer(t, func(opts *Options) { opts.Sampler = NewSampler(clock.Now) })

	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/system/sample", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	keys := decodeSystemDataKeys(t, rec)
	require.JSONEq(t, "null", string(keys["cpu_percent"]), "首次采样没有 CPU 基线必须是 null")

	var env struct {
		Data SystemSampleView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	view := env.Data
	require.Equal(t, "2026-10-18T12:00:00Z", view.SampledAt)
	require.Equal(t, []string{"cpu_percent"}, view.Unavailable)
	require.NotNil(t, view.MemoryRSSBytes)
	require.Equal(t, int64(4096), *view.MemoryRSSBytes)
	require.NotNil(t, view.Goroutines)
	require.Positive(t, *view.Goroutines, "goroutine 读数恒可用")
}

// TestSystem_SampleSecondReading 覆盖第二次重算：CPU 百分比 = 期间 CPU 时间增量 /
// 墙钟增量；缓存过期后立即出现读数（Demo A 的"5 秒后出现读数"）。
// 逆改点：除零、用宿主 CPU、把区间算错 → 对应断言红。
func TestSystem_SampleSecondReading(t *testing.T) {
	clock := &sampleClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	reads := 0
	stubProcessReaders(t,
		func() (cpuTime, bool) {
			mu.Lock()
			defer mu.Unlock()
			reads++
			if reads == 1 {
				return cpuTime{User: 100 * time.Millisecond}, true
			}
			return cpuTime{User: 300 * time.Millisecond}, true
		},
		func() (int64, bool) { return 4096, true })
	sampler := NewSampler(clock.Now)

	first := sampler.Sample()
	require.Nil(t, first.CPUPercent)

	clock.now = clock.now.Add(5 * time.Second)
	second := sampler.Sample()
	require.NotNil(t, second.CPUPercent, "5 秒后必须出现读数")
	require.InDelta(t, 4.0, *second.CPUPercent, 1e-9, "200ms CPU / 5s 墙钟 = 4%")
	require.Equal(t, clock.now.UTC(), second.SampledAt.UTC())
	require.Empty(t, second.Unavailable)
	require.Equal(t, 2, reads, "只重算两次：首次基线 + 一次重算")
}

// TestSystem_SampleCacheFiveSeconds 覆盖 5 秒缓存：窗口内返回同一份读数（含
// sampled_at），CPU 基线不随缓存命中推进；返回值是副本，调用方改不动缓存。
// 逆改点：每次请求都重算、缓存共享底层切片 → 对应断言红。
func TestSystem_SampleCacheFiveSeconds(t *testing.T) {
	clock := &sampleClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	reads := 0
	stubProcessReaders(t,
		func() (cpuTime, bool) {
			mu.Lock()
			defer mu.Unlock()
			reads++
			return cpuTime{User: time.Duration(reads) * 100 * time.Millisecond}, true
		},
		func() (int64, bool) { return 4096, true })
	sampler := NewSampler(clock.Now)

	first := sampler.Sample()
	clock.now = clock.now.Add(time.Second)
	cached := sampler.Sample()
	require.Equal(t, first.SampledAt, cached.SampledAt, "窗口内 sampled_at 是缓存时间")
	require.Nil(t, cached.CPUPercent, "缓存命中不得推进 CPU 基线")
	require.Equal(t, 1, reads)

	clock.now = clock.now.Add(3999 * time.Millisecond) // 累计 4.999 秒
	require.Equal(t, first.SampledAt, sampler.Sample().SampledAt)
	require.Equal(t, 1, reads)

	clock.now = clock.now.Add(time.Millisecond) // 累计正好 5 秒
	refreshed := sampler.Sample()
	require.NotEqual(t, first.SampledAt, refreshed.SampledAt)
	require.Equal(t, 2, reads)
	require.NotNil(t, refreshed.CPUPercent)
	require.InDelta(t, 2.0, *refreshed.CPUPercent, 1e-9, "100ms / 5s = 2%")

	refreshed.Unavailable = append(refreshed.Unavailable, "tampered")
	again := sampler.Sample()
	require.Empty(t, again.Unavailable, "调用方改写返回值不得污染缓存")
}

// TestSystem_SampleUnsupportedPlatformFields 覆盖平台不支持：两个读数都不可用时
// 字段为 null 并同时列入 unavailable，goroutine 照常给出；JSON 里空值是 null
// 而不是 0，unavailable 是数组而不是 null。
// 逆改点：不支持时回 0、unavailable 序列化成 null → 对应断言红。
func TestSystem_SampleUnsupportedPlatformFields(t *testing.T) {
	clock := &sampleClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	stubProcessReaders(t,
		func() (cpuTime, bool) { return cpuTime{}, false },
		func() (int64, bool) { return 0, false })
	s := newTestServer(t, func(opts *Options) { opts.Sampler = NewSampler(clock.Now) })

	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/system/sample", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	keys := decodeSystemDataKeys(t, rec)
	require.JSONEq(t, "null", string(keys["cpu_percent"]))
	require.JSONEq(t, "null", string(keys["memory_rss_bytes"]))
	var unavailable []string
	require.NoError(t, json.Unmarshal(keys["unavailable"], &unavailable))
	require.Equal(t, []string{"cpu_percent", "memory_rss_bytes"}, unavailable)
	require.NotNil(t, keys["unavailable"], "unavailable 永远是数组")
}

// TestSystem_SampleCounterRollbackIsUnavailable 覆盖计数回退：CPU 累计值变小
// （时钟 / 计数器异常）时不猜读数，按不可用处理，并在下一次采样重建基线。
// 逆改点：负数百分比直接输出 → 对应断言红。
func TestSystem_SampleCounterRollbackIsUnavailable(t *testing.T) {
	clock := &sampleClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	reads := 0
	stubProcessReaders(t,
		func() (cpuTime, bool) {
			mu.Lock()
			defer mu.Unlock()
			reads++
			switch reads {
			case 1:
				return cpuTime{User: 300 * time.Millisecond}, true
			case 2:
				return cpuTime{User: 100 * time.Millisecond}, true
			default:
				return cpuTime{User: 400 * time.Millisecond}, true
			}
		},
		func() (int64, bool) { return 4096, true })
	sampler := NewSampler(clock.Now)

	_ = sampler.Sample()
	clock.now = clock.now.Add(5 * time.Second)
	rolled := sampler.Sample()
	require.Nil(t, rolled.CPUPercent, "计数回退不得输出负百分比")
	require.Contains(t, rolled.Unavailable, "cpu_percent")

	clock.now = clock.now.Add(5 * time.Second)
	next := sampler.Sample()
	require.NotNil(t, next.CPUPercent, "回退后的读数成为新基线")
	require.InDelta(t, 6.0, *next.CPUPercent, 1e-9, "300ms / 5s = 6%")
}

// TestSystem_RoutesRegistered 覆盖路由登记：两个端点都在 routeTable 内且标记
// 受保护，GET 走真实 handler 链返回 200；未登记的方法落到兜底 404 信封。
// 逆改点：漏登记路由 / 标成公开 / 允许任意方法 → 对应断言红。
func TestSystem_RoutesRegistered(t *testing.T) {
	s := newTestServer(t, func(opts *Options) {
		opts.System = &fakeSystemReader{readings: SystemReadings{SchemaVersion: 2}}
		opts.Registry = fakeAgentRegistry{}
		opts.Lifecycle = &fakeLifecycle{}
	})
	visibility := map[string]routeVisibility{}
	for _, rt := range s.routeTable() {
		visibility[routeKey(rt)] = rt.visibility
	}
	require.Equal(t, routeProtected, visibility["GET /api/v1/system/overview"])
	require.Equal(t, routeProtected, visibility["GET /api/v1/system/sample"])

	for _, path := range []string{"/api/v1/system/overview", "/api/v1/system/sample"} {
		rec := serveAdmin(s, adminRequest(http.MethodGet, path, ""))
		require.Equal(t, http.StatusOK, rec.Code, path)
	}
	rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/system/overview", ""))
	require.Equal(t, http.StatusNotFound, rec.Code, "未登记的方法必须落到 404 信封")
}

// TestSystem_SampleConcurrent 覆盖并发采样：多 goroutine 同时读同一采样器不得
// 产生数据竞争（-race 判卷），且每次都拿到完整读数。
// 逆改点：去掉 Sampler 的互斥保护 → -race 兜不住并发写缓存。
func TestSystem_SampleConcurrent(t *testing.T) {
	s := newTestServer(t, nil)
	var wg sync.WaitGroup
	results := make([]SystemSample, 16)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = s.opts.Sampler.Sample()
		}(i)
	}
	wg.Wait()
	for i, sample := range results {
		require.False(t, sample.SampledAt.IsZero(), "第 %d 个并发读数缺少 sampled_at", i)
		require.NotNil(t, sample.Goroutines, "第 %d 个并发读数缺少 goroutines", i)
		require.NotNil(t, sample.Unavailable, "unavailable 不得为 nil")
	}
}
