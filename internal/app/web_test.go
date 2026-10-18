package app

// 本文件是 US-9 装配层的验收面（颗粒度文档 §3.2 的 `internal/app/web_test.go` 六条）：
//
//	T1 `TestNewWebServer_OptionsProjection`——addr / provider 摘要 / backend 归一化
//	   与真实 MCP 连接数 / 预算映射（4 例）
//	T2 `TestWebProviders_NoCredentialFields`——模型列表空 / 单元素；结构与 JSON 无凭证（2 例）
//	T3 `TestEnsureWorkspaceForServe`——建骨架 / 二次幂等（2 例）
//	T4 `TestNeedsServer`——只有 serve 需要 HTTP 面
//	T5 `TestNewRuntime_MaxConcurrentRuns`——配置 1 → 第二个并发运行被拒（R13）
//	T6 `TestNewToolCatalogPort_ClassificationAndOrder`——来源分类与顺序（R20）
//
// 全部离线：不访问真实网络与真实模型；T5 的 provider 卡在首次 Chat 上制造准入窗口。
// Every case is offline; T5 parks the first run inside Chat to hold the admission window.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
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
	a := f.app()
	a.providerOverride = blocker
	rt, err := a.newRuntime(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		blocker.Release()
		require.NoError(t, rt.Close())
	})

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
// builtin 集换成另写一份名单（与 declaredToolNames 漂移——漏名或多出未声明名）→
// 九名逐项断言或 code_only_tool 断言红；顺序改成 map 遍历 → 与 Names() 的相等断言红」。
func TestNewToolCatalogPort_ClassificationAndOrder(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := tool.NewRegistry(logger)
	// 九件声明名集的**字面量副本**：实现若与 app.declaredToolNames 漂移，逐项断言红。
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

	port := newToolCatalogPort(reg, []tool.MCPConnection{
		{Server: "fs", Tools: []core.Tool{mcpTool}},
	})
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
