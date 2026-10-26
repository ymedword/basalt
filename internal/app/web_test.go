package app

// 本文件是 US-9 装配层的验收面（颗粒度文档 §3.2 的 `internal/app/web_test.go` 六条），
// US-10 追加两条（§3.2.2）：T7 `TestDeclaredToolNames_IncludesManageSchedule`——十名
// 声明清单逐字对照；T8 `TestWebOptions_SchedulesWired`——调度器与归档入口的装配注入。
//
//	T1 `TestNewWebServer_OptionsProjection`——addr / provider 摘要 / backend 归一化
//	   与真实 MCP 连接数 / 预算映射（4 例）
//	T2 `TestWebProviders_NoCredentialFields`——模型列表空 / 单元素；结构与 JSON 无凭证（2 例）
//	T3 `TestEnsureWorkspaceForServe`——建骨架 / 二次幂等（2 例）
//	T4 `TestNeedsServer`——只有 serve 需要 HTTP 面
//	T5 `TestNewRuntime_MaxConcurrentRuns`——配置 1 → 第二个并发运行被拒（R13）
//	T6 `TestNewToolCatalogPort_ClassificationAndOrder`——来源分类与顺序（R20）
//	T7 `TestDeclaredToolNames_IncludesManageSchedule`——第 10 件内置工具的十名清单
//	T8 `TestWebOptions_SchedulesWired`——Schedules 恒非 nil、Runs 指向 AgentService
//
// US-11 T023 追加认证装配五条（§3.1.2 的 app 侧）：
//
//	T9  `TestNewWebServer_RefusesInvalidAdminAuth`——缺 mode / 非法 mode / 缺凭据 /
//	    未解析占位，一律在交出端口前拒绝启动（4 例）
//	T10 `TestAdminAuthConfig_DerivesLoopbackDefaults`——回环监听派生 Host 三元组与
//	    http origins（约束 4 / 5）
//	T11 `TestAdminAuthConfig_KeepsExplicitValues`——显式白名单原样保留且与配置副本隔离
//	T12 `TestAdminAuthConfig_NonLoopbackNeedsExplicitHosts`——非回环 / 通配监听不派生，
//	    显式列出后才放行（D-10）
//	T13 `TestListenAddr_DefaultsToLoopback`——空地址落回环缺省，绝不落到全接口监听
//
// US-12 T016 追加写面端口投影三条（§3.1.8 的 app 侧）：
//
//	T14 `TestWritePorts_FailClosedWithoutAdapters`——半截运行期 → 四个写面端口全 nil
//	T15 `TestWritePorts_ProjectedIdentically`——槽里的适配器逐字段同一实例投影
//	T16 `TestWritePorts_IndependentOfContentEditor`——写面端口与 US-11 内容编辑器互不顶替
//
// 全部离线：不访问真实网络与真实模型；T5 的 provider 卡在首次 Chat 上制造准入窗口。
// Every case is offline; T5 parks the first run inside Chat to hold the admission window.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/web"
)

// TestNewWebServer_OptionsProjection 逐项断言装配层投影到 web.Options 的生效值。
// 包外看不到 `web.Server.opts`，投影错了只能靠行为用例间接发现——这里直接对
// `webOptions` 判卷，把装配错误挡在启动之前。
func TestNewWebServer_OptionsProjection(t *testing.T) {
	t.Run("addr 投影且 HTTP 面可装配", func(t *testing.T) {
		f := newWireFixture(t)
		f.cfg.HTTP.Addr = "127.0.0.1:18099"
		f.cfg.HTTP.MaxRequestBytes = 4096
		rt := f.assemble(t)

		a := f.app()
		opts := a.webOptions(rt)
		require.Equal(t, "127.0.0.1:18099", opts.Addr)
		require.EqualValues(t, 4096, opts.MaxRequestBytes)

		// 装配会真实绑定监听（T023）：这里改用端口 0 拿临时端口，避免与别的进程抢固定端口；
		// cfg 与 rt.cfg 是同一指针，webOptions 的投影断言已在上方完成。
		// Assembly really binds now; port 0 keeps the case from fighting over a fixed port.
		f.cfg.HTTP.Addr = "127.0.0.1:0"
		f.cfg.HTTP.AdminAuth = cli.AdminAuthConfig{Mode: "token", Token: "test-token"}
		srv, err := a.newWebServer(rt)
		require.NoError(t, err, "装配态齐全时 HTTP 面必须能建")
		require.NotNil(t, srv)
	})

	t.Run("provider 摘要按配置顺序且不含凭证", func(t *testing.T) {
		rt := &heavyRuntime{cfg: &cli.Config{Providers: []cli.ProviderConfig{
			{Name: "probe", BaseURL: "http://127.0.0.1:1", APIKey: "placeholder-unused-3c71", DefaultModel: "probe-model"},
			{Name: "bare"},
		}}}
		opts := (&App{}).webOptions(rt)
		require.Equal(t, []web.ProviderInfo{
			{Name: "probe", Models: []string{"probe-model"}, Configured: true},
			{Name: "bare", Models: []string{}, Configured: false},
		}, opts.Providers)
	})

	t.Run("backend 归一化与真实 MCP 连接数", func(t *testing.T) {
		rt := &heavyRuntime{
			cfg: &cli.Config{}, // memory.backend 为空 → 归一化为 markdown
			mcpConns: []tool.MCPConnection{
				{Server: "fs", Tools: []core.Tool{fakeTool{name: "mcp__fs__read"}}},
				{Server: "gh", Tools: []core.Tool{fakeTool{name: "mcp__gh__search"}}},
			},
		}
		opts := (&App{}).webOptions(rt)
		require.Equal(t, "markdown", opts.MemoryBackend)
		require.Equal(t, 2, opts.MCPConnected, "MCP 连接数取真实连接结果，不是配置条目数")

		rt.cfg.Memory.Backend = "sqlite"
		require.Equal(t, "sqlite", (&App{}).webOptions(rt).MemoryBackend)
	})

	t.Run("预算映射与越界值丢弃", func(t *testing.T) {
		rt := &heavyRuntime{cfg: &cli.Config{Budget: cli.BudgetConfig{
			MaxIterations:  7,
			MaxToolCalls:   9,
			MaxOutputBytes: 4096,
			ContextTokens:  12345,
		}}}
		require.Equal(t, core.Budget{
			MaxIterations:  7,
			MaxToolCalls:   9,
			MaxOutputBytes: 4096,
			ContextTokens:  12345,
		}, (&App{}).webOptions(rt).RunBudget)

		rt.cfg.Budget.MaxOutputBytes = -1
		require.Zero(t, (&App{}).webOptions(rt).RunBudget.MaxOutputBytes,
			"非正数不搬运——留在零值由运行入口补缺省")
	})

	t.Run("实例上限逐键投影，零值交给 NewServer 归一", func(t *testing.T) {
		// 14D / E8：就绪视图要拿**生效**上限对照，投影只是搬运——缺省值由
		// `web.NewServer` 补 core 的同一对常量，装配层不再抄一份 10 / 32。
		rt := &heavyRuntime{cfg: &cli.Config{
			Budget: cli.BudgetConfig{MaxIterations: 12},
			Server: cli.ServerConfig{MaxConcurrentRuns: 4},
		}}
		require.Equal(t, web.InstanceLimits{MaxIterations: 12, MaxConcurrentRuns: 4},
			(&App{}).webOptions(rt).InstanceLimits)

		rt.cfg.Budget.MaxIterations = 0
		rt.cfg.Server.MaxConcurrentRuns = 0
		require.Equal(t, web.InstanceLimits{}, (&App{}).webOptions(rt).InstanceLimits,
			"未配置就原样投影零值（归一在 web.NewServer，与运行面同源）")
	})
}

// TestNewWebServer_PreparesAdminTokenBeforeBinding 判首启生成密码与绑定监听之间的次序：
// 端口被占时首次启动照样失败，但密码文件必须已经落盘——否则重试时库与名册都在、不再
// 算新实例，"三处都没有"只能靠人工写文件恢复（WSL2 实装首启踩到：旧实例占着 8080）。
func TestNewWebServer_PreparesAdminTokenBeforeBinding(t *testing.T) {
	f := newWireFixture(t)
	// 三个平台的数据根环境变量一起指向临时目录：私有目录绝不落到真实用户目录。
	dataRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", dataRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)
	t.Setenv("HOME", dataRoot)

	id, err := newInstanceID()
	require.NoError(t, err)
	markerPath := instanceMarkerPath(f.workDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(markerPath), 0o750))
	raw, err := marshalInstanceMarker(instanceMarker{Version: instanceMarkerVersion, InstanceID: id})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(markerPath, raw, 0o644))

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = occupied.Close() }()

	f.cfg.HTTP.Addr = occupied.Addr().String()
	f.cfg.HTTP.AdminAuth = cli.AdminAuthConfig{Mode: adminAuthModeToken}
	a := f.app()
	a.freshInstance = true
	rt := f.assemble(t)

	_, err = a.newWebServer(rt)
	require.Error(t, err, "端口被占时应拒绝启动")
	require.Contains(t, err.Error(), "监听")

	tokenPath, err := defaultAdminTokenFile(f.workDir)
	require.NoError(t, err)
	_, statErr := os.Stat(tokenPath)
	require.NoError(t, statErr, "密码文件必须在绑定监听之前生成：%s", tokenPath)
}

// TestNewWebServer_RefusesInvalidAdminAuth 判启动期拒绝（FR1 / 约束 1）：配置不成形时
// 服务**不得**被构造出来——拒绝发生在监听创建之后、交出服务之前，错误须指向具体配置键。
// Startup refusal: an invalid management-auth section never yields a server, and the
// error names the offending key.
func TestNewWebServer_RefusesInvalidAdminAuth(t *testing.T) {
	cases := []struct {
		name     string
		auth     cli.AdminAuthConfig
		wantPart string
	}{
		{name: "模式缺失", auth: cli.AdminAuthConfig{}, wantPart: `当前为 ""`},
		{name: "模式非法", auth: cli.AdminAuthConfig{Mode: "none"}, wantPart: `当前为 "none"`},
		{
			name:     "token 模式缺少凭据",
			auth:     cli.AdminAuthConfig{Mode: "token"},
			wantPart: "http.admin_auth.token 缺失",
		},
		{
			name:     "凭据仍是未解析占位",
			auth:     cli.AdminAuthConfig{Mode: "token", Token: "${BASALT_ADMIN_TOKEN}"},
			wantPart: "未解析的 ${...} 占位",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &heavyRuntime{cfg: &cli.Config{HTTP: cli.HTTPConfig{
				Addr:      "127.0.0.1:0",
				AdminAuth: tc.auth,
			}}}
			srv, err := (&App{}).newWebServer(rt)
			require.Nil(t, srv, "拒绝启动时不得交出服务")
			require.Error(t, err)
			require.Contains(t, err.Error(), "管理认证配置校验失败")
			require.Contains(t, err.Error(), tc.wantPart, "错误须指向具体配置键，便于运维定位")
		})
	}
}

// TestAdminAuthConfig_DerivesLoopbackDefaults 判缺省派生（约束 4 / 5）：回环监听派生
// 三个本机 Host（IPv4 / localhost / IPv6）与其 http:// origins，且派生结果能过启动校验。
// A loopback listener derives the local-host trio and their http origins.
func TestAdminAuthConfig_DerivesLoopbackDefaults(t *testing.T) {
	cfg := &cli.Config{HTTP: cli.HTTPConfig{AdminAuth: cli.AdminAuthConfig{
		Mode:  "token",
		Token: "test-token",
	}}}

	auth := adminAuthConfig(cfg, "127.0.0.1:18080")
	require.Equal(t, []string{"127.0.0.1:18080", "localhost:18080", "[::1]:18080"}, auth.AllowedHosts)
	require.Equal(t, []string{
		"http://127.0.0.1:18080",
		"http://localhost:18080",
		"http://[::1]:18080",
	}, auth.AllowedOrigins, "缺省 origins 取有效 Host 的 http:// 形态")
	require.Equal(t, "token", auth.Mode)
	require.Equal(t, "test-token", auth.Token)
	require.NoError(t, web.ValidateAdminAuth(auth, []string{"127.0.0.1:18080"}),
		"派生结果必须能过启动校验，否则安全缺省形同虚设")

	// IPv6 回环监听与 IPv4 回环同一口径（端口取自实际监听）。
	v6 := adminAuthConfig(cfg, "[::1]:18081")
	require.Equal(t, []string{"127.0.0.1:18081", "localhost:18081", "[::1]:18081"}, v6.AllowedHosts)
}

// TestAdminAuthConfig_KeepsExplicitValues 判显式配置不被缺省覆盖（约束 4）：只要
// allowed_hosts 非空就原样使用；origins 缺省时按**有效** Host 派生；投影与配置切片
// 互不共享底层数组（装配输入一经交出，后续改配置不得回写）。
// Explicit lists win over derivation, and the projection never aliases the config slices.
func TestAdminAuthConfig_KeepsExplicitValues(t *testing.T) {
	cfg := &cli.Config{HTTP: cli.HTTPConfig{AdminAuth: cli.AdminAuthConfig{
		Mode:           "local_trust",
		AllowedHosts:   []string{"127.0.0.1:9000", "localhost:9000"},
		TrustedProxies: []string{"10.0.0.0/8"},
	}}}

	auth := adminAuthConfig(cfg, "127.0.0.1:9000")
	require.Equal(t, []string{"127.0.0.1:9000", "localhost:9000"}, auth.AllowedHosts,
		"显式 Host 列表非空即原样使用，不补三元组")
	require.Equal(t, []string{"http://127.0.0.1:9000", "http://localhost:9000"}, auth.AllowedOrigins,
		"origins 缺省时按有效 Host 派生（约束 5）")
	require.Equal(t, []string{"10.0.0.0/8"}, auth.TrustedProxies)

	// 显式 origins 优先，且切片是副本。
	cfg.HTTP.AdminAuth.AllowedOrigins = []string{"https://console.internal:9443"}
	auth = adminAuthConfig(cfg, "127.0.0.1:9000")
	require.Equal(t, []string{"https://console.internal:9443"}, auth.AllowedOrigins)
	cfg.HTTP.AdminAuth.AllowedHosts[0] = "mutated:1"
	cfg.HTTP.AdminAuth.AllowedOrigins[0] = "mutated:1"
	require.Equal(t, []string{"127.0.0.1:9000", "localhost:9000"}, auth.AllowedHosts,
		"投影必须是副本：改配置不得回写已交出的装配输入")
	require.Equal(t, []string{"https://console.internal:9443"}, auth.AllowedOrigins)
}

// TestAdminAuthConfig_NonLoopbackNeedsExplicitHosts 判 D-10：非回环与通配监听**不派生**
// 白名单，未显式列出即拒绝启动；显式列出后才放行（通配监听按同端口判定）。
// Non-loopback and wildcard binds never derive; they start only when listed explicitly.
func TestAdminAuthConfig_NonLoopbackNeedsExplicitHosts(t *testing.T) {
	cfg := &cli.Config{HTTP: cli.HTTPConfig{AdminAuth: cli.AdminAuthConfig{
		Mode:  "token",
		Token: "test-token",
	}}}

	for _, listener := range []string{"192.168.1.10:8443", "0.0.0.0:8080", "[::]:8080"} {
		auth := adminAuthConfig(cfg, listener)
		require.Empty(t, auth.AllowedHosts, "非回环 / 通配监听不得派生 Host：%s", listener)
		require.Empty(t, auth.AllowedOrigins)
		require.Error(t, web.ValidateAdminAuth(auth, []string{listener}),
			"未显式列出白名单必须拒绝启动：%s", listener)
	}

	// 通配监听：显式列出同端口的主机名即放行；origins 随显式 Host 派生。
	cfg.HTTP.AdminAuth.AllowedHosts = []string{"127.0.0.1:8080", "basalt.internal:8080"}
	auth := adminAuthConfig(cfg, "0.0.0.0:8080")
	require.NoError(t, web.ValidateAdminAuth(auth, []string{"0.0.0.0:8080"}))
	require.Equal(t, []string{"http://127.0.0.1:8080", "http://basalt.internal:8080"}, auth.AllowedOrigins)

	// 具体非回环地址：监听地址本身在清单里才放行。
	cfg.HTTP.AdminAuth.AllowedHosts = []string{"192.168.1.10:8443"}
	auth = adminAuthConfig(cfg, "192.168.1.10:8443")
	require.NoError(t, web.ValidateAdminAuth(auth, []string{"192.168.1.10:8443"}))
}

// TestListenAddr_DefaultsToLoopback 判安全缺省：未配置或空白地址一律落回环，
// 绝不落成"全接口监听"；显式地址（含运维有意暴露的地址）原样使用。
// An empty address stays loopback; explicit values pass through untouched.
func TestListenAddr_DefaultsToLoopback(t *testing.T) {
	require.Equal(t, "127.0.0.1:8080", listenAddr(nil), "配置缺失时不得落到全接口监听")
	require.Equal(t, "127.0.0.1:8080", listenAddr(&cli.Config{}))
	require.Equal(t, "127.0.0.1:8080", listenAddr(&cli.Config{HTTP: cli.HTTPConfig{Addr: "   "}}))
	require.Equal(t, "0.0.0.0:9000", listenAddr(&cli.Config{HTTP: cli.HTTPConfig{Addr: "0.0.0.0:9000"}}),
		"显式地址原样使用——是否安全由启动校验判定")
}

// TestWebProviders_NoCredentialFields 断言 /info 的 provider 摘要形状：模型列表
// 空则不造占位，结构里没有凭证与 base_url（约束 25）。
func TestWebProviders_NoCredentialFields(t *testing.T) {
	t.Run("模型列表为空不造占位", func(t *testing.T) {
		got := webProviders(&cli.Config{Providers: []cli.ProviderConfig{{
			Name:    "p1",
			BaseURL: "http://127.0.0.1:1",
			APIKey:  "placeholder-unused-3c71",
		}}})
		require.Len(t, got, 1)
		require.Equal(t, []string{}, got[0].Models, "空模型列表保持为空切片，不填占位")
		require.True(t, got[0].Configured, "base_url 与 api_key 都非空才算已配置")
	})

	t.Run("单元素模型且结构无凭证字段", func(t *testing.T) {
		got := webProviders(&cli.Config{Providers: []cli.ProviderConfig{{
			Name:         "probe",
			BaseURL:      "http://127.0.0.1:1",
			APIKey:       "placeholder-unused-3c71",
			DefaultModel: "probe-model",
		}}})
		require.Equal(t, []web.ProviderInfo{{
			Name:       "probe",
			Models:     []string{"probe-model"},
			Configured: true,
		}}, got)

		typ := reflect.TypeOf(web.ProviderInfo{})
		fields := make([]string, 0, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			fields = append(fields, typ.Field(i).Name)
		}
		require.ElementsMatch(t, []string{"Name", "Models", "Configured"}, fields,
			"ProviderInfo 只允许三字段——多一个就可能是凭证泄漏面")

		raw, err := json.Marshal(got)
		require.NoError(t, err)
		for _, banned := range []string{"api_key", "base_url", "credential", "placeholder-unused-3c71", "127.0.0.1:1"} {
			require.NotContains(t, string(raw), banned, "响应摘要不得出现凭证或地址")
		}
	})
}

// TestEnsureWorkspaceForServe 对应颗粒度文档 R12（原文保留）：
// 「逆改点：ensureWorkspaceForServe 改成非幂等（二次调用报错）→ 第二段红」。
func TestEnsureWorkspaceForServe(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".basalt")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, ensureWorkspaceForServe(root, logger))
	require.DirExists(t, root)
	require.NoError(t, ensureWorkspaceForServe(root, logger), "二次调用必须幂等")
}

// TestNeedsServer 对应约束 29：只有 serve 需要 HTTP 面；gateway / chat 不启 HTTP。
func TestNeedsServer(t *testing.T) {
	require.True(t, needsServer([]string{"serve"}))
	require.False(t, needsServer([]string{"gateway"}), "gateway 只常驻等信号，不启 HTTP")
	require.False(t, needsServer([]string{"chat", "--message", "hi"}), "chat 不启 HTTP")
	require.False(t, needsServer(nil))
}

// TestNewRuntime_MaxConcurrentRuns 对应颗粒度文档 R13（原文保留）：
// 「逆改点：漏接 ServiceOptions.MaxConcurrent → 上限回落到 core 缺省 32，第二个运行
// 被放行并进入 Chat 阻塞 → 1 秒超时分支 t.Fatal；把值写成 MaxConcurrentRuns+1 →
// Limit 断言红（429 文案里的数字与实际配置脱钩）」。
func TestNewRuntime_MaxConcurrentRuns(t *testing.T) {
	f := newWireFixture(t)
	f.cfg.Server.MaxConcurrentRuns = 1
	// 声明一件内置工具而非零工具：夹具的零工具分支会把 `tools: []` 与收尾围栏拼成
	// `tools: []---`（wire_test.go 既有缺陷，不在本 US 允许触碰的文件清单内）。本用例
	// 判的是并发准入，与工具无关；首个响应不含工具调用，声明不影响运行路径。
	f.writeAgent(t, "ops-agent", "read_file")
	blocker := &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
	// 探针的已发布版本先种子：AGENT.md 的 `resource_bindings` 要指向真实指针，否则两次
	// Run 都会停在绑定准入，判不到并发上限。
	// Seed the probe's published version so admission reaches the concurrency gate.
	f.seedPublishedProviders(t)
	a := f.app()
	a.providerOverride = blocker
	rt, err := a.newRuntime(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		blocker.Release()
		require.NoError(t, rt.Close())
	})
	// 直接装配（不走夹具的自动启用）：基线写下的 disabled 必须先经真实迁移入口启用，
	// 否则本用例判到的会是生命周期准入而不是并发上限（同一处 Run 入口，见 D-01 A）。
	f.enableAgents(t, rt)

	ctx := context.Background()
	first := make(chan error, 1)
	go func() {
		_, err := rt.service.Run(ctx, core.RunRequest{
			Agent: "ops-agent", SessionID: "s1", Message: "hi", Channel: "web", UserID: "u",
		})
		first <- err
	}()
	<-blocker.started // 第一个运行已准入并进入模型调用

	second := make(chan error, 1)
	go func() {
		_, err := rt.service.Run(ctx, core.RunRequest{
			Agent: "ops-agent", SessionID: "s2", Message: "hi", Channel: "web", UserID: "u",
		})
		second <- err
	}()
	select {
	case err := <-second:
		var full *core.InstanceFullError
		require.ErrorAs(t, err, &full)
		require.Equal(t, 1, full.Limit, "429 点名的上限必须等于配置值")
	case <-time.After(time.Second):
		t.Fatal("并发上限未接线：第二个运行未被拒绝（回落到 core 缺省 32）")
	}

	blocker.Release()
	require.NoError(t, <-first, "第一个运行正常收尾")
}

// blockingProvider 让首次 Chat 卡住，制造「已有运行在跑」的窗口。
type blockingProvider struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
}

// Chat 首次调用时关闭 started，随后阻塞到 Release 或上下文取消。
func (p *blockingProvider) Chat(ctx context.Context, _ core.ChatRequest) (core.ChatResponse, error) {
	p.startedOnce.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return core.ChatResponse{Content: "ok"}, nil
	case <-ctx.Done():
		return core.ChatResponse{}, ctx.Err()
	}
}

// Release 幂等放行（用例主体与 Cleanup 都会调用）。
func (p *blockingProvider) Release() { p.releaseOnce.Do(func() { close(p.release) }) }

// TestNewToolCatalogPort_ClassificationAndOrder 对应颗粒度文档 R20（原文保留）：
// 「逆改点：来源改按名字前缀猜 → code_only_tool 被标 builtin/mcp，断言红；
// builtin 集换掉 → 九名逐项断言或 code_only_tool 断言红；顺序改成 map 遍历 →
// 与 Names() 的相等断言红」。边界（US-10 §3.1.17 ① / P-03）：九名夹具只证明
// **已注册**名归 builtin——漏登记的第十名仍绿，多列未注册名不可见；第十名的
// 判别力由 T7 的十名清单承担。
func TestNewToolCatalogPort_ClassificationAndOrder(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := tool.NewRegistry(logger)
	// US-10 前十名子集（分类与顺序断言不变）：九名只验**已注册**名的来源分类。
	builtinNames := []string{
		"http_get", "http_post", "list_dir", "notify", "read_file",
		"recall_memory", "save_memory", "shell", "write_file",
	}
	for _, name := range builtinNames {
		require.NoError(t, reg.Register(fakeTool{name: name}))
	}
	require.NoError(t, reg.Register(fakeTool{name: "code_only_tool"})) // 仅代码注册、不在声明名集
	mcpTool := fakeTool{name: "mcp__fs__read"}
	require.NoError(t, reg.Register(mcpTool))

	port := newToolCatalogPort(reg, newMCPRuntime(nil, []tool.MCPConnection{
		{Server: "fs", Tools: []core.Tool{mcpTool}},
	}))
	got := port.Tools()

	names := make([]string, len(got))
	sources := make(map[string]string, len(got))
	for i, d := range got {
		names[i] = d.Spec.Name
		sources[d.Spec.Name] = d.Source
	}
	require.Equal(t, reg.Names(), names, "顺序必须等于 Registry.Names()")
	for _, name := range builtinNames {
		require.Equal(t, "builtin", sources[name], "声明名集内的工具必须归 builtin")
	}
	require.Equal(t, "mcp", sources["mcp__fs__read"])
	require.Equal(t, "code", sources["code_only_tool"])
}

// fakeTool 只提供来源分类所需的 Name；其余接口方法返回零值。
type fakeTool struct{ name string }

func (f fakeTool) Name() string               { return f.name }
func (fakeTool) Description() string          { return "" }
func (fakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (fakeTool) Execute(context.Context, core.ToolEnv, json.RawMessage) (core.Result, error) {
	return core.Result{}, nil
}

// TestDeclaredToolNames_IncludesManageSchedule 对应颗粒度文档 §3.2.2（约束 32）：
// 十一名声明清单**逐字对照**（判据面变更 §3.5 #4；14D T125 精确集 10 → 11）。实现若漏
// 登记 `manage_schedule` 或 `web_search`，轻路径会判它"池外"；若多列未注册名，重路径
// 等值门禁会拒绝启动——两种漂移都在这里红。
// The eleven-name literal list is the only judge of the newest tools' drift: the nine-name
// classification fixture above cannot see a missing or extra declaration (P-03).
func TestDeclaredToolNames_IncludesManageSchedule(t *testing.T) {
	require.Equal(t, []string{
		"http_get", "http_post", "list_dir", "manage_schedule", "notify", "read_file",
		"recall_memory", "save_memory", "shell", "web_search", "write_file",
	}, declaredToolNames, "声明名集必须与注册池十一件逐字一致（排序序）")
}

// TestWebOptions_SchedulesWired 对应颗粒度文档 §3.2.2（§3.1.19 / 约束 21）：
// 重路径**始终**构造调度器，投影进 web.Options 的必须是同一个实例（chat / tool
// 不 Start 只是不启动，端口不得为空）；Runs 仍指向唯一运行入口——其 `Archive`
// 就是会话归档的唯一写面，非法改成另一个实例会在归档语义上分叉。
// The scheduler is always constructed on the heavy path, and Runs must stay the one
// service whose Archive is the sole archiving write face.
func TestWebOptions_SchedulesWired(t *testing.T) {
	f := newWireFixture(t)
	rt := f.assemble(t)

	opts := f.app().webOptions(rt)
	require.NotNil(t, rt.scheduler, "重路径必须构造调度器（约束 21）")
	require.NotNil(t, opts.Schedules, "调度器必须注入 web.Options.Schedules（重路径恒构造）")
	require.Same(t, rt.scheduler, opts.Schedules, "web 端口必须指向装配出的同一个调度器实例")
	require.NotNil(t, opts.Runs, "运行入口是归档的唯一写面")
	adapter, ok := opts.Runs.(*runService)
	require.True(t, ok, "Runs 必须是装配层适配器：*core.AgentService 方法集缺 ListRuns / GetRun（§3.1.31）")
	require.Same(t, rt.service, adapter.Service, "Run / Archive / Cancel 必须指向唯一运行入口")
	require.Same(t, rt.runs, adapter.Runs, "ListRuns / GetRun 必须读 AgentService 写入的同一份账本")
}

// 中文验收点（T057 / 模块卡 §3.1.15 的 webOptions 行）：重路径装配的管理面七端口
// 全部非 nil——Registry / Lifecycle / Operations / Resources / System / Sampler /
// AgentAdmin；Registry 指向装配期唯一的注册表快照，System 读数走真实库（库版本 +
// 三个运行计数 + 三类发布指针计数），Sampler 按注入时钟构造。
// 逆改点：漏装任一端口（handler 固定 500）/ Registry 另建清单 / System 伪造零值 → 断言红。
func TestRunService_WebOptionsManagementPortsWired(t *testing.T) {
	f := newWireFixture(t)
	f.writeAgent(t, "alpha", "read_file")
	rt := f.assemble(t)

	opts := f.app().webOptions(rt)
	require.NotNil(t, opts.Registry, "Registry 必须装配（管理台与准入看到同一份快照）")
	require.NotNil(t, opts.Lifecycle, "Lifecycle 必须装配（生命周期读面）")
	require.NotNil(t, opts.AgentAdmin, "AgentAdmin 必须装配（内容读写 + 启停）")
	require.NotNil(t, opts.Operations, "Operations 必须装配（操作记录读面）")
	require.NotNil(t, opts.Resources, "Resources 必须装配（六条只读资源路由）")
	require.NotNil(t, opts.System, "System 必须装配（概览数据库侧读数）")
	require.NotNil(t, opts.Sampler, "Sampler 按注入时钟构造")

	entries := opts.Registry.Entries()
	require.Len(t, entries, 1)
	require.Equal(t, "alpha", entries[0].Name)
	require.True(t, entries[0].Registered, "夹具 Agent 已启用且注册")

	ctx := context.Background()
	readings, err := opts.System.ReadOverview(ctx)
	require.NoError(t, err, "概览读数走真实库，不得因缺端口失败")
	require.Equal(t, storage.SupportedSchemaVersion, readings.SchemaVersion)
	require.Zero(t, readings.RunsRunning, "空账本的运行中计数是真实读数 0")
	require.Zero(t, readings.RunsLast24h)
	require.Zero(t, readings.RunsFailedLast24h)

	// 资源计数与只读资源路由同一处数据源：逐类与 ListPointers 的条数相等。
	for _, tc := range []struct {
		kind core.ResourceKind
		got  int64
	}{
		{core.ResourceModelConnection, readings.ModelConnections},
		{core.ResourceMCPServer, readings.MCPServers},
		{core.ResourceSkill, readings.Skills},
	} {
		pointers, err := rt.versions.ListPointers(ctx, tc.kind)
		require.NoError(t, err)
		require.EqualValues(t, len(pointers), tc.got, "%s 计数必须等于未删除发布指针数", tc.kind)
	}
	require.NotZero(t, readings.ModelConnections, "基线已为探针 provider 发布 v1 指针")

	require.NotZero(t, opts.Sampler.Sample().SampledAt)
}

// 中文验收点（T057 / Demo C 行 55）：内容条件写经**生产 webOptions 装配**走通——
// 读取返回强 ETag；带 If-Match 的整份替换经与热加载同一道装载期校验后落盘并替换
// 注册表条目；候选不合法（缺 provider.model）按 invalid_input 拒绝且磁盘逐字节不变。
// 逆改点：Validate 未接装载器（编辑器报"缺少装载期校验器"）/ 校验放行坏候选 /
// 拒绝仍写盘 → 断言红。
func TestAgentConsole_WiredThroughWebOptions(t *testing.T) {
	f := newWireFixture(t)
	f.writeAgent(t, "alpha", "read_file")
	rt := f.assemble(t)

	opts := f.app().webOptions(rt)
	require.NotNil(t, opts.AgentAdmin, "内容读写端点必须由生产装配提供")

	ctx := context.Background()
	current, err := opts.AgentAdmin.ReadContent(ctx, "alpha")
	require.NoError(t, err)
	require.NotEmpty(t, current.ETag, "读取必须返回强 ETag")

	updated := strings.Replace(current.Content, "你是「alpha」。", "你是「alpha（已更新）」。", 1)
	require.NotEqual(t, current.Content, updated, "夹具正文必须命中替换点")

	files, err := opts.AgentAdmin.UpdateContent(ctx, "alpha",
		core.AgentContentUpdate{Content: updated}, current.ETag, consoleActor())
	require.NoError(t, err, "候选通过装载期校验后必须保存成功")
	require.Equal(t, "alpha", files.Agent)
	require.NotEmpty(t, files.Files, "成功返回该 Agent 的文件集合视图")

	after, err := opts.AgentAdmin.ReadContent(ctx, "alpha")
	require.NoError(t, err)
	require.Equal(t, updated, after.Content)
	require.NotEqual(t, current.ETag, after.ETag, "内容变化后 ETag 必须变化")
	entry, ok := opts.Registry.Lookup("alpha")
	require.True(t, ok)
	require.True(t, entry.Registered, "写入后注册表保持注册（候选校验与热加载同一道规则）")

	// 反例：候选缺 provider.model → 装载期以**类型化拒绝**上抛（HTTP 层映射为 400
	// 并按分类出中文），磁盘逐字节不变。
	broken := strings.Replace(updated, "  model: probe-model\n", "", 1)
	require.NotEqual(t, updated, broken, "夹具必须命中 model 行")
	_, err = opts.AgentAdmin.UpdateContent(ctx, "alpha",
		core.AgentContentUpdate{Content: broken}, after.ETag, consoleActor())
	var rejected *core.AgentDefinitionRejectedError
	require.ErrorAs(t, err, &rejected, "坏候选必须按装载期分类拒绝")
	require.Equal(t, core.AgentRejectBlankModel, rejected.Kind, "缺 model 归类为 blank model")
	onDisk, readErr := os.ReadFile(filepath.Join(f.root, agentsDirName, "alpha", "AGENT.md"))
	require.NoError(t, readErr)
	require.Equal(t, updated, string(onDisk), "拒绝时磁盘零写入")
}

// fakeAgentAdminService 只实现一个写面接口：用例只判"投影是同一实例"，所有方法
// 返回零值，不参与业务判定。`AgentAdminService.Create` 与 `NotifyAdminService.Create`
// 签名不同，四个桩因此**不能**合并成一个类型——这正是按消费方拆分端口的结果。
// The stub implements exactly one port; Create has different signatures in two ports,
// so a single combined fake is impossible by construction.
type fakeAgentAdminService struct{}

var _ web.AgentAdminService = (*fakeAgentAdminService)(nil)

func (*fakeAgentAdminService) Create(context.Context, core.Actor, core.AgentSpec) (web.AgentFileSet, error) {
	return web.AgentFileSet{}, nil
}

func (*fakeAgentAdminService) GenerateFiles(context.Context, string, string, []core.AuthorAnswer) (web.AgentDraftResult, error) {
	return web.AgentDraftResult{}, nil
}

func (*fakeAgentAdminService) ScheduleDraft(context.Context, string, string) (web.ScheduleDraftResult, error) {
	return web.ScheduleDraftResult{}, nil
}

func (*fakeAgentAdminService) Files(context.Context, string) (web.AgentFileSet, error) {
	return web.AgentFileSet{}, nil
}

func (*fakeAgentAdminService) SubmitFiles(context.Context, core.Actor, string, string, []core.AuthorFile) (web.AgentFileSet, error) {
	return web.AgentFileSet{}, nil
}

func (*fakeAgentAdminService) Copy(context.Context, core.Actor, string, string, []core.ResourceMapping) (web.AgentFileSet, error) {
	return web.AgentFileSet{}, nil
}

func (*fakeAgentAdminService) Archive(context.Context, core.Actor, string) (core.AgentArchiveRecord, error) {
	return core.AgentArchiveRecord{}, nil
}

func (*fakeAgentAdminService) Restore(context.Context, core.Actor, string, string) (web.AgentFileSet, error) {
	return web.AgentFileSet{}, nil
}

func (*fakeAgentAdminService) Archived(context.Context) ([]core.AgentArchiveRecord, error) {
	return nil, nil
}

// fakeResourceAdminService 是资源写面端口的零值桩。
// fakeResourceAdminService is the zero-value resource write stub.
type fakeResourceAdminService struct{}

var _ web.ResourceAdminService = (*fakeResourceAdminService)(nil)

func (*fakeResourceAdminService) UpsertModelConnection(context.Context, core.Actor, web.ModelConnectionWrite, string) (web.ModelConnectionView, error) {
	return web.ModelConnectionView{}, nil
}

func (*fakeResourceAdminService) UpsertMCPServer(context.Context, core.Actor, web.MCPServerWrite, string) (web.McpServerView, error) {
	return web.McpServerView{}, nil
}

func (*fakeResourceAdminService) CreateSkill(context.Context, core.Actor, web.SkillCreateWrite) (web.SkillDetailView, error) {
	return web.SkillDetailView{}, nil
}

func (*fakeResourceAdminService) ImportSkill(context.Context, core.Actor, web.SkillImportWrite) (web.SkillDetailView, error) {
	return web.SkillDetailView{}, nil
}

func (*fakeResourceAdminService) UpdateSkill(context.Context, core.Actor, string, web.SkillUpdateWrite, string) (web.SkillDetailView, error) {
	return web.SkillDetailView{}, nil
}

func (*fakeResourceAdminService) ValidateSkill(context.Context, string, web.SkillUpdateWrite) (core.SkillValidateResult, error) {
	return core.SkillValidateResult{}, nil
}

func (*fakeResourceAdminService) Delete(context.Context, core.Actor, core.ResourceKind, string, string, string) error {
	return nil
}

func (*fakeResourceAdminService) Publish(context.Context, core.Actor, core.ResourcePublishRequest) (core.ResourcePublishResult, error) {
	return core.ResourcePublishResult{}, nil
}

func (*fakeResourceAdminService) Versions(context.Context, core.ResourceKind, string) ([]core.ResourceVersionDetail, error) {
	return nil, nil
}

func (*fakeResourceAdminService) CurrentETag(context.Context, core.ResourceKind, string) (string, bool) {
	return "", false
}

func (*fakeResourceAdminService) VersionDetail(context.Context, int64) (core.ResourceVersionDetail, error) {
	return core.ResourceVersionDetail{}, nil
}

func (*fakeResourceAdminService) References(context.Context, core.ResourceKind, string) (core.ResourceReferences, error) {
	return core.ResourceReferences{}, nil
}

func (*fakeResourceAdminService) TestModelConnection(context.Context, core.Actor, string) (core.ResourceTestResult, error) {
	return core.ResourceTestResult{}, nil
}

func (*fakeResourceAdminService) TestMCPServer(context.Context, core.Actor, string) (core.ResourceTestResult, error) {
	return core.ResourceTestResult{}, nil
}

func (*fakeResourceAdminService) DiagnoseMCPServer(context.Context, core.Actor, string) (core.MCPServerDiagnosis, error) {
	return core.MCPServerDiagnosis{}, nil
}

func (*fakeResourceAdminService) ApplyMCPServer(context.Context, core.Actor, string, string, int64) (core.MCPServerApplyResult, error) {
	return core.MCPServerApplyResult{}, nil
}

// fakeNotifyAdminService 是通知渠道写面端口的零值桩。
// fakeNotifyAdminService is the zero-value notify write stub.
type fakeNotifyAdminService struct{}

var _ web.NotifyAdminService = (*fakeNotifyAdminService)(nil)

func (*fakeNotifyAdminService) Create(context.Context, core.Actor, web.NotifyChannelWrite) (core.NotifyChannel, error) {
	return core.NotifyChannel{}, nil
}

func (*fakeNotifyAdminService) Update(context.Context, core.Actor, string, web.NotifyChannelWrite) (core.NotifyChannel, error) {
	return core.NotifyChannel{}, nil
}

func (*fakeNotifyAdminService) Delete(context.Context, core.Actor, string, string, bool) error {
	return nil
}

func (*fakeNotifyAdminService) TestChannel(context.Context, core.Actor, string) (core.NotifyChannelTestResult, error) {
	return core.NotifyChannelTestResult{OK: true, Status: core.ConnectionTestSuccess}, nil
}

// fakeWorkspaceService 是只读工作区端口的零值桩。
// fakeWorkspaceService is the zero-value workspace stub.
type fakeWorkspaceService struct{}

var _ web.WorkspaceService = (*fakeWorkspaceService)(nil)

func (*fakeWorkspaceService) Tree(context.Context, string) (web.WorkspaceTreeView, error) {
	return nil, nil
}

func (*fakeWorkspaceService) File(context.Context, string) (web.WorkspaceFileView, error) {
	return web.WorkspaceFileView{}, nil
}

// TestWritePorts_FailClosedWithoutAdapters：半截运行期（只有配置）投影出的四个写面
// 端口必须全为 nil——handler 对缺端口按固定 500 拒绝，装配层**不得**用空实现冒充
// "可用"。与 wireManagementPorts 的失败关闭口径相同。
// 逆改点：给任一端口填零值实现 / 在 webOptions 里临时构造服务 → 对应断言红。
// 中文用例名：写面端口缺适配器时失败关闭。
func TestWritePorts_FailClosedWithoutAdapters(t *testing.T) {
	opts := (&App{}).webOptions(&heavyRuntime{cfg: &cli.Config{}})
	require.Nil(t, opts.AgentAdminService)
	require.Nil(t, opts.ResourceAdminService)
	require.Nil(t, opts.NotifyAdminService)
	require.Nil(t, opts.WorkspaceService)
}

// TestWritePorts_ProjectedIdentically：槽里放什么，`web.Options` 就得到什么——四个
// 端口逐字段是**同一实例**（不是包装、不是副本）。构造任务把适配器填进
// `heavyRuntime.writeServices` 后，路由处理函数看到的就是那个实例。
// 逆改点：投影时重新包一层 / 字段串位（如把通知渠道装进 WorkspaceService）→ 红。
// 中文用例名：写面端口逐字段同一实例投影。
func TestWritePorts_ProjectedIdentically(t *testing.T) {
	agentAdmin := &fakeAgentAdminService{}
	resourceAdmin := &fakeResourceAdminService{}
	notifyAdmin := &fakeNotifyAdminService{}
	workspace := &fakeWorkspaceService{}

	rt := &heavyRuntime{cfg: &cli.Config{}}
	rt.writeServices = writeServices{
		agentAdmin:    agentAdmin,
		resourceAdmin: resourceAdmin,
		notifyAdmin:   notifyAdmin,
		workspace:     workspace,
	}

	opts := (&App{}).webOptions(rt)
	require.Same(t, agentAdmin, opts.AgentAdminService)
	require.Same(t, resourceAdmin, opts.ResourceAdminService)
	require.Same(t, notifyAdmin, opts.NotifyAdminService)
	require.Same(t, workspace, opts.WorkspaceService)
}

// TestWritePorts_IndependentOfContentEditor：US-11 的 `Options.AgentAdmin`（内容读写
// 与启停）与 US-12 的写面端口互不顶替——装配出的内容编辑器**不**让写面端口变成
// 非 nil，填了写面槽也**不**让内容编辑器出现。两个方向都判，避免以后把两者合并成
// 一个"大接口"。
// 逆改点：用 AgentAdmin 顶替 AgentAdminService / 把写面槽回填给 AgentAdmin → 红。
// 中文用例名：写面端口与内容编辑器互不顶替。
func TestWritePorts_IndependentOfContentEditor(t *testing.T) {
	// 方向一：完整装配路径会给内容编辑器，写面端口仍只来自 writeServices 槽。
	f := newWireFixture(t)
	f.writeAgent(t, "alpha", "read_file")
	rt := f.assemble(t)

	opts := f.app().webOptions(rt)
	require.NotNil(t, opts.AgentAdmin, "US-11 内容编辑器必须仍在装配面上")
	require.Equal(t, rt.writeServices.agentAdmin == nil, opts.AgentAdminService == nil,
		"AgentAdminService 只来自写面槽，不得由内容编辑器顶替")
	require.Equal(t, rt.writeServices.resourceAdmin == nil, opts.ResourceAdminService == nil)
	require.Equal(t, rt.writeServices.notifyAdmin == nil, opts.NotifyAdminService == nil)
	require.Equal(t, rt.writeServices.workspace == nil, opts.WorkspaceService == nil)

	// 方向二：只填写面槽（半截运行期）→ 内容编辑器保持 nil。
	partial := &heavyRuntime{cfg: &cli.Config{}}
	partial.writeServices.agentAdmin = &fakeAgentAdminService{}
	opts = (&App{}).webOptions(partial)
	require.NotNil(t, opts.AgentAdminService)
	require.Nil(t, opts.AgentAdmin, "写面端口不得让内容编辑器变成非 nil")
}
