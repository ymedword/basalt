package app

// 本文件是 US-25C §2.3.8（G5）与 §3.1.4 的定向判据：资源准备必须先于 Entry 构建与
// 注册，Provider 逐条构建且失败隔离，MCP 只对**已 applied** 的连接做只读准备，已连接
// 且（版本, 凭据代次）都相符时**零副作用**复用，两者不符才受控重建；受控重建的换入前
// 复核失败一律 `superseded`、不替换工具，既有管理 apply 保持"CAS 成功后立即换入"。
//
// 判据取值只看可观测面：连接器的 Connect / Close / 新建次数、supervisor 的 epoch 与
// 状态、`ReplaceTools` / `MarkApplied` / 审计回调的调用次数与次序。所有替身都在本文件
// 内自建——`internal/app` 现有的 MCP 夹具全部带 `//go:build integration`，本文件属于
// 源码级 stage，不得借用。
// This file pins resource preparation before entry construction: isolated provider
// builds, applied-only MCP preparation, zero-side-effect reuse, controlled rebuilds,
// and the swap-time recheck. Every double is local because the existing MCP fixtures
// are integration-tagged while this file runs under the source-level stage.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// —— 通用夹具 ——

// prepareDiscardLogger 给出静默日志器：本文件判的是副作用与次序，不判日志文本。
func prepareDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// prepareAgentUID 生成一个合法的 Agent 身份（夹具需要真实前缀）。
func prepareAgentUID(t *testing.T) core.AgentUID {
	t.Helper()
	uid, err := core.NewAgentUID()
	require.NoError(t, err, "夹具需要合法的 Agent 身份")
	return uid
}

// —— Provider 准备 ——

// prepareProviderSeed 是一条待发布的模型连接（名字 + `api_key` 原文）。
type prepareProviderSeed struct {
	name   string
	apiKey string
}

// prepareProviderStore 是 `providerPrepareStore` 的最小替身：只建模本文件观测到的
// 两件事——已发布指针列表与按版本号读版本行。真实仓储由集成层用例承担。
// prepareProviderStore models only the two reads this file observes.
type prepareProviderStore struct {
	pointers []core.ResourcePointer
	versions map[int64]core.ResourceVersion
}

// ListPointers 按资源标识升序返回（与真实仓储的 `ORDER BY resource_id` 同序）。
func (s *prepareProviderStore) ListPointers(_ context.Context, kind core.ResourceKind) ([]core.ResourcePointer, error) {
	out := make([]core.ResourcePointer, 0, len(s.pointers))
	for _, pointer := range s.pointers {
		if pointer.Kind == kind {
			out = append(out, pointer)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceID < out[j].ResourceID })
	return out, nil
}

// VersionByID 返回预置版本行；缺失按"查无此行"失败。
func (s *prepareProviderStore) VersionByID(_ context.Context, id int64) (core.ResourceVersion, error) {
	version, ok := s.versions[id]
	if !ok {
		return core.ResourceVersion{}, fmt.Errorf("版本 %d 不存在", id)
	}
	return version, nil
}

// newPrepareProviderStore 砌出 N 条已发布模型连接：每条一份发布指针与一行版本，
// 定义原文取管理写路径的块原文形态（`name/base_url/api_key/default_model`）。
func newPrepareProviderStore(t *testing.T, seeds ...prepareProviderSeed) *prepareProviderStore {
	t.Helper()
	store := &prepareProviderStore{versions: make(map[int64]core.ResourceVersion)}
	for index, seed := range seeds {
		uid, err := core.NewResourceUID()
		require.NoError(t, err)
		id := int64(index + 1)
		definition := fmt.Sprintf(
			"name: %s\nbase_url: https://%s.example.com/v1\napi_key: %s\ndefault_model: %s-model\n",
			seed.name, seed.name, seed.apiKey, seed.name)
		store.pointers = append(store.pointers, core.ResourcePointer{
			Kind:               core.ResourceModelConnection,
			ResourceID:         seed.name,
			ResourceUID:        uid,
			PublishedVersionID: id,
			Revision:           1,
		})
		store.versions[id] = core.ResourceVersion{
			ID:             id,
			Kind:           core.ResourceModelConnection,
			ResourceUID:    uid,
			ResourceID:     seed.name,
			VersionNo:      1,
			ContentHash:    fmt.Sprintf("hash-%s-%d", seed.name, id),
			DefinitionYAML: definition,
		}
	}
	return store
}

// newPrepareCatalogStore 用一份空目录初始化运行期目录持有者。
func newPrepareCatalogStore(t *testing.T) *providerCatalogStore {
	t.Helper()
	store, err := newProviderCatalogStore(&providerCatalog{
		clients: make(map[string]core.ProviderClient),
		models:  make(map[string]string),
		built:   make(map[string]core.BuiltVersion),
	})
	require.NoError(t, err)
	return store
}

// TestProviderPrepare_PartialFailureIsolated 守 §2.3.8 的 Provider 面：逐条构建、单条
// 失败只点名不影响其他条目，失败项不加入生效目录；同版本同凭据的第二轮**零重建**。
func TestProviderPrepare_PartialFailureIsolated(t *testing.T) {
	// 本用例改环境变量，按 testing 的约束不得并行。
	store := newPrepareProviderStore(t,
		prepareProviderSeed{name: "alpha", apiKey: "${BASALT_PREPARE_ALPHA_KEY}"},
		prepareProviderSeed{name: "beta", apiKey: "${BASALT_PREPARE_MISSING_KEY}"},
		prepareProviderSeed{name: "gamma", apiKey: "${BASALT_PREPARE_GAMMA_KEY}"},
	)
	t.Setenv("BASALT_PREPARE_ALPHA_KEY", "alpha-test-key")
	t.Setenv("BASALT_PREPARE_GAMMA_KEY", "gamma-test-key")
	// 空值与未设置同档拒绝（`resolveProviderAPIKey` 的既有口径）。
	t.Setenv("BASALT_PREPARE_MISSING_KEY", "")

	catalog := newPrepareCatalogStore(t)
	deps := providerPrepareDeps{
		catalog: catalog,
		store:   store,
		logger:  prepareDiscardLogger(),
	}

	report, err := prepareProviderCatalog(context.Background(), deps)
	require.NoError(t, err, "单条失败不得变成整体失败")
	require.Equal(t, []string{"alpha", "gamma"}, report.Providers,
		"成功名单只含构建成功的条目，且按资源标识升序")
	require.Len(t, report.Failures, 1, "失败项逐条点名，不聚合")
	require.Equal(t, "beta", report.Failures[0].Name)
	require.Contains(t, report.Failures[0].Reason, "BASALT_PREPARE_MISSING_KEY",
		"失败原因必须点名缺的是哪个环境变量")
	require.NotContains(t, report.Failures[0].Reason, "alpha-test-key",
		"错误不得回显任何凭据值")

	first := catalog.Load()
	require.NotNil(t, first)
	require.Contains(t, first.clients, "alpha")
	require.Contains(t, first.clients, "gamma")
	require.NotContains(t, first.clients, "beta", "失败项不加入生效目录")
	require.False(t, first.built["alpha"].IsZero(),
		"构建成功必须带版本与凭据证据——零值证据会让准入比对退化成永远相等")
	require.NotContains(t, first.built, "beta", "失败项不得留下构建证据")

	// 第二轮：版本与凭据都没变，必须复用同一个客户端实例（零重建）。
	second, err := prepareProviderCatalog(context.Background(), deps)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "gamma"}, second.Providers)
	reused := catalog.Load()
	require.Same(t, first.clients["alpha"], reused.clients["alpha"],
		"版本与凭据都未变时必须复用，不得每轮重建客户端")
	require.Same(t, first.clients["gamma"], reused.clients["gamma"])
}

// —— MCP 准备：自由函数面 ——

// preparePublishedServer 砌一条已发布 MCP server（stdio 传输，夹具连接器不执行命令）。
func preparePublishedServer(name string, versionID int64, generation string) core.PublishedMCPServer {
	return core.PublishedMCPServer{
		VersionID:            versionID,
		CredentialGeneration: generation,
		Config: core.MCPServerConfig{
			Name:      name,
			Transport: mcpTransportStdio,
			Command:   "/usr/bin/prepare-fixture-mcp",
		},
	}
}

// TestMCPPrepare_OnlyApplied 守"只对已 applied 的连接做准备"：未 applied 的条目如实
// 记 `not_applied`，既不冒充成功也不隐式 apply；三态名单分开给。
func TestMCPPrepare_OnlyApplied(t *testing.T) {
	t.Parallel()

	applied := preparePublishedServer("alpha-mcp", 5, "gen-5")
	targets := []mcpPrepareTarget{
		{Name: "alpha-mcp", Applied: true, Published: applied},
		// 只发布了、还没有 applied 指针：本函数不得替它做主。
		{Name: "beta-mcp", Applied: false},
	}

	var prepared []core.PublishedMCPServer
	report, err := prepareMCPInstances(context.Background(), mcpPrepareDeps{
		Targets: func(context.Context) ([]mcpPrepareTarget, error) { return targets, nil },
		Prepare: func(_ context.Context, published core.PublishedMCPServer) MCPApplyResult {
			prepared = append(prepared, published)
			return MCPApplyResult{Outcome: core.MCPApplyApplied, AppliedVersionID: published.VersionID}
		},
		Status: func(name string) (MCPServerStatus, bool) {
			if name != "alpha-mcp" {
				return MCPServerStatus{}, false
			}
			return MCPServerStatus{
				Name: name, Connected: true, AppliedVersionID: 5,
				Tools: []string{"alpha-tool"},
			}, true
		},
	})
	require.NoError(t, err)

	require.Equal(t, []core.PublishedMCPServer{applied}, prepared,
		"只对已 applied 的连接调用准备；未 applied 不得被隐式 apply")
	require.Equal(t, []string{"alpha-mcp", "beta-mcp"}, report.Configured,
		"configured 是全部已发布 server（含未 applied）")
	require.Equal(t, []string{"alpha-mcp"}, report.Connected, "connected 只含真正连上的")
	require.Equal(t, []string{"alpha-tool"}, report.Tools)

	require.Len(t, report.Results, 2)
	require.Equal(t, "alpha-mcp", report.Results[0].Name)
	require.Equal(t, string(core.MCPApplyApplied), report.Results[0].Outcome)
	require.Equal(t, int64(5), report.Results[0].AppliedVersionID)
	require.Equal(t, "beta-mcp", report.Results[1].Name)
	require.Equal(t, mcpPrepareOutcomeNotApplied, report.Results[1].Outcome,
		"未 applied 必须如实记 not_applied，不得冒充 applied 或 failed")
	require.Zero(t, report.Results[1].AppliedVersionID)
	require.False(t, report.Results[1].Reused)
}

// —— MCP 准备：supervisor 面 ——

// prepareTool 是一件夹具工具（连接适配器的最小实现）。
type prepareTool struct{ name string }

// Name 返回工具名。
func (t prepareTool) Name() string { return t.name }

// Description 返回工具描述。
func (prepareTool) Description() string { return "资源准备夹具工具" }

// InputSchema 返回固定的空对象模式。
func (prepareTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

// Execute 返回工具名（本文件不判执行语义，只用它证明连接仍可用）。
func (t prepareTool) Execute(context.Context, core.ToolEnv, json.RawMessage) (core.Result, error) {
	return core.Result{Content: t.name}, nil
}

// prepareConnectorPool 按每次连接发一个新连接器，并集中记"新建 / Connect / Close"
// 三个计数：零副作用复用直接判这三个数不变。
// prepareConnectorPool counts connector creations, connects and closes so reuse is
// provable as a zero-side-effect round.
type prepareConnectorPool struct {
	mu       sync.Mutex
	toolSets [][]core.Tool
	created  int
	connects int
	closes   int
}

// newPrepareConnectorPool 预置每次连接的（按连接序依次取用的）工具集。
func newPrepareConnectorPool(toolSets ...[]core.Tool) *prepareConnectorPool {
	return &prepareConnectorPool{toolSets: toolSets}
}

// NewConnector 发一个新连接器；工具集按连接序取，越界复用最后一个。
func (p *prepareConnectorPool) NewConnector() mcpConnector {
	p.mu.Lock()
	defer p.mu.Unlock()
	set := p.toolSets[0]
	if p.created < len(p.toolSets) {
		set = p.toolSets[p.created]
	}
	p.created++
	return &prepareConnector{pool: p, tools: set}
}

// counts 返回（新建, Connect, Close）三个计数。
func (p *prepareConnectorPool) counts() (created, connects, closes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.created, p.connects, p.closes
}

// prepareConnector 是一次连接的替身：Connect 记数后交回固定工具集。
type prepareConnector struct {
	pool  *prepareConnectorPool
	tools []core.Tool
}

// Connect 记一次连接尝试并返回单 server 的连接结果。
func (c *prepareConnector) Connect(ctx context.Context, specs []core.MCPServerConfig) ([]tool.MCPConnection, error) {
	c.pool.mu.Lock()
	c.pool.connects++
	c.pool.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(specs) != 1 {
		return nil, fmt.Errorf("夹具连接器只接受单 server（实际 %d）", len(specs))
	}
	return []tool.MCPConnection{{
		Server: specs[0].Name,
		Tools:  slices.Clone(c.tools),
	}}, nil
}

// Close 记一次关闭。
func (c *prepareConnector) Close() error {
	c.pool.mu.Lock()
	c.pool.closes++
	c.pool.mu.Unlock()
	return nil
}

// prepareAllowPolicy 放行一切（本文件不判策略，只判准备路径的副作用）。
type prepareAllowPolicy struct{}

// Check 恒放行。
func (prepareAllowPolicy) Check(context.Context, core.Action) error { return nil }

// prepareVerifyOK 是"复核通过、未被取代"的换入前复核。
func prepareVerifyOK(context.Context, core.PublishedMCPServer) (core.MCPApplySupersededBy, error) {
	return core.MCPApplySupersededByNone, nil
}

// prepareVerifyBy 返回一个恒报"被 `by` 取代"的复核。
func prepareVerifyBy(by core.MCPApplySupersededBy) func(context.Context, core.PublishedMCPServer) (core.MCPApplySupersededBy, error) {
	return func(context.Context, core.PublishedMCPServer) (core.MCPApplySupersededBy, error) {
		return by, nil
	}
}

// prepareMCPEnv 是一次 supervisor 现场：真实 `MCPSupervisor` + 计数替身。
// `verify` 为 nil 表示装配**没有**接换入前复核入口（`PrepareAtApplied` 据此失败关闭）。
type prepareMCPEnv struct {
	sup  *MCPSupervisor
	pool *prepareConnectorPool

	mu     sync.Mutex
	events []string
	audits []MCPAuditEvent
	marked []int64
	swaps  [][]core.Tool
}

// VerifyFunc 是换入前复核的形状（与 `MCPApplyInput.Verify` 同形）。
type VerifyFunc = func(ctx context.Context, published core.PublishedMCPServer) (core.MCPApplySupersededBy, error)

// newPrepareMCPEnv 装配一个真实 supervisor；`verify == nil` 时不接复核入口。
func newPrepareMCPEnv(t *testing.T, pool *prepareConnectorPool, verify VerifyFunc) *prepareMCPEnv {
	t.Helper()
	env := &prepareMCPEnv{pool: pool}
	deps := MCPSupervisorDeps{
		Policy: prepareAllowPolicy{},
		Gate:   &core.ResourceGate{},
		Logger: prepareDiscardLogger(),
		Now:    func() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() },
		Audit: func(event MCPAuditEvent) {
			env.mu.Lock()
			env.audits = append(env.audits, event)
			env.mu.Unlock()
		},
		MarkApplied: func(_ context.Context, _ string, versionID int64) error {
			env.mu.Lock()
			env.marked = append(env.marked, versionID)
			env.mu.Unlock()
			return nil
		},
		ReplaceTools: func(server string, tools []core.Tool) {
			env.mu.Lock()
			env.swaps = append(env.swaps, tools)
			env.events = append(env.events, "swap:"+server)
			env.mu.Unlock()
		},
		NewConnector: pool.NewConnector,
	}
	if verify != nil {
		deps.PrepareVerify = func(ctx context.Context, published core.PublishedMCPServer) (core.MCPApplySupersededBy, error) {
			env.record("verify")
			return verify(ctx, published)
		}
	}
	env.sup = NewMCPSupervisor(deps)
	return env
}

// record 记一条事件（判次序用）。
func (e *prepareMCPEnv) record(event string) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

// recorded 返回事件副本。
func (e *prepareMCPEnv) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

// auditCount 返回授权审计条数。
func (e *prepareMCPEnv) auditCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.audits)
}

// appliedCount 返回 applied 写库次数。
func (e *prepareMCPEnv) appliedCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.marked)
}

// swapCount 返回工具集替换次数。
func (e *prepareMCPEnv) swapCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.swaps)
}

// lastSwapNames 返回最近一次替换的工具名。
func (e *prepareMCPEnv) lastSwapNames() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.swaps) == 0 {
		return nil
	}
	names := make([]string, 0, len(e.swaps[len(e.swaps)-1]))
	for _, item := range e.swaps[len(e.swaps)-1] {
		names = append(names, item.Name())
	}
	return names
}

// entryValue 只读取一个 server 的状态槽（不存在返回 nil）。
func (e *prepareMCPEnv) entryValue(name string) *mcpServerEntry {
	entry, ok := e.sup.lookup(name)
	if !ok {
		return nil
	}
	return entry
}

// epochOf 读该 server 当前的 epoch。
func (e *prepareMCPEnv) epochOf(name string) uint64 {
	entry := e.entryValue(name)
	if entry == nil {
		return 0
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.epoch
}

// TestMCPPrepare_SameVersionReusesConnection 守 P1：已连接、版本与凭据代次都相符且
// link 仍 live 时**零副作用**——不新建连接器、不 Connect、不 Close、不递增 epoch、
// 不写授权审计、不写 applied、不替换工具集。
func TestMCPPrepare_SameVersionReusesConnection(t *testing.T) {
	t.Parallel()

	pool := newPrepareConnectorPool([]core.Tool{prepareTool{name: "alpha-tool"}})
	env := newPrepareMCPEnv(t, pool, prepareVerifyOK)
	ctx := context.Background()

	published := preparePublishedServer("alpha-mcp", 4, "gen-4")
	require.NoError(t, env.sup.Start(ctx, published))

	created, connects, closes := pool.counts()
	require.Equal(t, 1, connects, "首轮必须真的连接一次")
	epoch := env.epochOf("alpha-mcp")
	audits := env.auditCount()
	applied := env.appliedCount()
	swaps := env.swapCount()
	entry := env.entryValue("alpha-mcp")
	require.NotNil(t, entry)
	link := entry.link
	require.NotNil(t, link)
	require.True(t, link.isLive())

	result := env.sup.PrepareAtApplied(ctx, published)
	require.Equal(t, core.MCPApplyApplied, result.Outcome)
	require.True(t, result.Reused,
		"版本与凭据代次都相符且连接仍 live 时必须走复用分支")
	require.Equal(t, int64(4), result.AppliedVersionID)

	createdAfter, connectsAfter, closesAfter := pool.counts()
	require.Equal(t, created, createdAfter, "复用不得新建连接器")
	require.Equal(t, connects, connectsAfter, "复用不得重新 Connect")
	require.Equal(t, closes, closesAfter, "复用不得 Close 任何连接")
	require.Equal(t, epoch, env.epochOf("alpha-mcp"),
		"复用不得递增 epoch——在途工具代际一旦作废就是周期性断连重建")
	require.Equal(t, audits, env.auditCount(), "复用不得写授权审计")
	require.Equal(t, applied, env.appliedCount(), "复用不得写 applied 指针")
	require.Equal(t, swaps, env.swapCount(), "复用不得替换工具集")
	require.True(t, link.isLive(), "复用后旧代必须仍然 live")
	require.True(t, entry.status.Connected)
}

// TestMCPPrepare_CredentialRotationRebuilds 守 P6：版本号不变而凭据代次变了必须
// **受控重建一次**——旧代封门、连接关闭、工具集换新；同一代次再来一轮回到复用。
func TestMCPPrepare_CredentialRotationRebuilds(t *testing.T) {
	t.Parallel()

	pool := newPrepareConnectorPool(
		[]core.Tool{prepareTool{name: "alpha-tool"}},
		[]core.Tool{prepareTool{name: "alpha-tool-v2"}},
	)
	env := newPrepareMCPEnv(t, pool, prepareVerifyOK)
	ctx := context.Background()

	first := preparePublishedServer("alpha-mcp", 4, "gen-4")
	require.NoError(t, env.sup.Start(ctx, first))
	oldLink := env.entryValue("alpha-mcp").link
	require.NotNil(t, oldLink)
	_, connects, _ := pool.counts()

	rotated := preparePublishedServer("alpha-mcp", 4, "gen-5")
	result := env.sup.PrepareAtApplied(ctx, rotated)
	require.Equal(t, core.MCPApplyApplied, result.Outcome)
	require.False(t, result.Reused, "凭据代次变了必须受控重建，不得继续用旧连接")
	require.Equal(t, int64(4), result.AppliedVersionID, "版本号本身没变")

	_, connectsAfter, closesAfter := pool.counts()
	require.Equal(t, connects+1, connectsAfter, "换代必须且只能重建一次")
	require.Equal(t, 1, closesAfter, "旧代连接必须关闭")
	require.False(t, oldLink.isLive(),
		"旧代门必须封死——在途调用不得写进已换下的连接")
	require.Equal(t, 1, env.swapCount(), "换入必须整组替换工具集")
	require.Equal(t, []string{"alpha-tool-v2"}, env.lastSwapNames(),
		"注册池里的工具必须来自新代连接")

	// 同一代次再来一轮：回到零副作用复用。
	again := env.sup.PrepareAtApplied(ctx, rotated)
	require.True(t, again.Reused)
	_, final, _ := pool.counts()
	require.Equal(t, connectsAfter, final, "代次未变不得再重建")
}

// TestMCPPrepare_SupersededBeforeSwap 守换入前复核：复核报"已被取代"时不换入、
// 留旧工具、关闭候选；装配没有复核入口时按失败关闭（不得把"跳过复核"当成功）。
func TestMCPPrepare_SupersededBeforeSwap(t *testing.T) {
	t.Parallel()

	pool := newPrepareConnectorPool([]core.Tool{prepareTool{name: "alpha-tool"}})
	env := newPrepareMCPEnv(t, pool, prepareVerifyBy(core.MCPApplySupersededByPublish))
	ctx := context.Background()

	first := preparePublishedServer("alpha-mcp", 3, "gen-3")
	require.NoError(t, env.sup.Start(ctx, first))
	swapsBefore := env.swapCount()
	_, connectsBefore, _ := pool.counts()

	target := preparePublishedServer("alpha-mcp", 9, "gen-9")
	result := env.sup.PrepareAtApplied(ctx, target)
	require.Equal(t, core.MCPApplySuperseded, result.Outcome,
		"换入前复核失败必须记 superseded，不得换入")
	require.Equal(t, core.MCPApplySupersededByPublish, result.SupersededBy)
	require.Equal(t, swapsBefore, env.swapCount(), "被取代时不得替换工具集")
	require.Equal(t, int64(3), env.entryValue("alpha-mcp").status.AppliedVersionID,
		"被取代时本地已应用版本保持旧值")

	_, connectsAfter, closesAfter := pool.counts()
	require.Equal(t, connectsBefore+1, connectsAfter, "候选连接已经建立")
	require.Equal(t, 1, closesAfter, "被取代的候选必须关闭，不得泄漏")

	// 装配没接复核入口：没有复核就没有换入依据，一律失败关闭。
	barePool := newPrepareConnectorPool([]core.Tool{prepareTool{name: "alpha-tool"}})
	bare := newPrepareMCPEnv(t, barePool, nil)
	_, bareConnects, _ := barePool.counts()
	failed := bare.sup.PrepareAtApplied(ctx, first)
	require.Equal(t, core.MCPApplyFailed, failed.Outcome,
		"无复核入口时不得当成复核通过")
	require.Equal(t, mcpTestClassDependencyUnconfirmed, failed.FailureClass)
	_, bareAfter, _ := barePool.counts()
	require.Equal(t, bareConnects, bareAfter, "失败关闭发生在建连之前")
}

// TestMCPApply_AdminPathSwapsAfterCAS 守既有管理 apply 的路径分支：`Commit`（事务校验
// 与 CAS）成功后**立即**换入工具集，中间不得插入任何提交后可失败的复核；`PrepareVerify`
// 在该路径上根本不会被调用。
func TestMCPApply_AdminPathSwapsAfterCAS(t *testing.T) {
	t.Parallel()

	pool := newPrepareConnectorPool([]core.Tool{prepareTool{name: "alpha-tool"}})
	env := newPrepareMCPEnv(t, pool, prepareVerifyOK)
	ctx := context.Background()

	result := env.sup.Replace(ctx, MCPApplyInput{
		Target:            preparePublishedServer("alpha-mcp", 2, "gen-2"),
		PreviousVersionID: 1,
		Commit: func(context.Context) (core.MCPApplySupersededBy, error) {
			env.record("commit")
			return core.MCPApplySupersededByNone, nil
		},
	})
	require.Equal(t, core.MCPApplyApplied, result.Outcome)
	require.Equal(t, int64(2), result.AppliedVersionID)
	require.Equal(t, []string{"commit", "swap:alpha-mcp"}, env.recorded(),
		"管理 apply 必须在 CAS 成功后立即换入，中间不得插入复核或任何查询")
}

// TestMCPPrepare_NoDoubleLock 守三段锁序：准备与准入侧重建并发推进时全部完成——
// 持 `entry.mu` 调 `Replace` 会自锁（Go 互斥不可重入），锁序倒置会互等，两种写法
// 都会让本用例超时。
func TestMCPPrepare_NoDoubleLock(t *testing.T) {
	t.Parallel()

	pool := newPrepareConnectorPool([]core.Tool{prepareTool{name: "alpha-tool"}})
	env := newPrepareMCPEnv(t, pool, prepareVerifyOK)
	ctx := context.Background()

	first := preparePublishedServer("alpha-mcp", 1, "gen-1")
	require.NoError(t, env.sup.Start(ctx, first))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 8; i++ {
			env.sup.PrepareAtApplied(ctx, preparePublishedServer("alpha-mcp", int64(10+i), "gen-1"))
			// 准入路径的 `RebuildAtApplied` 与本路径**不得**合并，这里让两者并行。
			_ = env.sup.RebuildAtApplied(ctx, first)
		}
	}()
	for i := 0; i < 8; i++ {
		env.sup.PrepareAtApplied(ctx, preparePublishedServer("alpha-mcp", int64(20+i), "gen-1"))
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("准备与重建并发超时——疑似持 entry.mu 调 Replace（自锁）或锁序倒置")
	}
}

// —— AssetSync 面 ——

// prepareOKLoader 永远返回一只可注册的条目（本文件不判 frontmatter 解析）。
type prepareOKLoader struct{}

// EntryFromBundle 按已安装清单的 agent 名返回注册条目。
func (prepareOKLoader) EntryFromBundle(_ string, installed assets.Installed,
	_ PreparedResources, _ map[string]string,
) (core.AgentEntry, error) {
	return core.AgentEntry{
		Name:       installed.Manifest.Agent,
		Registered: true,
		Profile:    &core.Profile{Name: installed.Manifest.Agent},
	}, nil
}

// prepareEntryRecorder 把 Entry 构建转给内层实现，并留下本轮入参快照与错误。
type prepareEntryRecorder struct {
	inner  BundleEntryBuilder
	onCall func()

	mu       sync.Mutex
	snapshot PreparedResources
	calls    int
	err      error
}

// EntryFromBundle 记录快照与错误后转给内层。
func (r *prepareEntryRecorder) EntryFromBundle(root string, installed assets.Installed,
	prepared PreparedResources, published map[string]string,
) (core.AgentEntry, error) {
	r.mu.Lock()
	r.calls++
	r.snapshot = prepared
	r.mu.Unlock()
	if r.onCall != nil {
		r.onCall()
	}
	entry, err := r.inner.EntryFromBundle(root, installed, prepared, published)
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
	return entry, err
}

// snapshotOf 返回最近一次的入参快照。
func (r *prepareEntryRecorder) snapshotOf() PreparedResources {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot
}

// callCount 返回构建次数。
func (r *prepareEntryRecorder) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// lastError 返回最近一次构建的错误。
func (r *prepareEntryRecorder) lastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// addProviderBundle 砌一份声明了资源绑定的真实 bundle（定义原文由用例给出）并把载荷
// 放进分发读面：注册判定必须由真实装载器执行，"点名缺项"才不是夹具在演。
func addProviderBundle(t *testing.T, f *assetSyncFixture, agent string, uid core.AgentUID,
	updatedAt int64, body string,
) string {
	t.Helper()
	f.sequence++
	rel := "agents/" + agent + "/AGENT.md"
	writeWorkspaceFile(t, f.workspaceRoot, rel, body)
	dest := filepath.Join(f.buildRoot, fmt.Sprintf("build-%03d-%s", f.sequence, agent))
	manifest, err := assets.Build(context.Background(), assets.BuildSpec{
		Agent:       agent,
		AgentUID:    uid,
		ConfigHash:  "cfg-" + agent,
		ConfigRoot:  f.workspaceRoot,
		ConfigFiles: []string{rel},
		Dest:        dest,
	})
	require.NoError(t, err)
	payload, sum, err := assets.EncodeBundle(context.Background(), dest, syncTestMaxBytes)
	require.NoError(t, err)
	f.dist.addRow(agent, uid, manifest.BundleHash, updatedAt)
	f.dist.payloads[manifest.BundleHash] = core.BundlePayloadRecord{
		BundleHash:    manifest.BundleHash,
		Agent:         agent,
		AgentUID:      uid,
		FormatVersion: manifest.FormatVersion,
		Payload:       payload,
		PayloadSHA256: sum,
		SizeBytes:     int64(len(payload)),
	}
	f.records.pointers[uid] = core.ResourcePointer{
		Kind:               core.ResourceAgentBundle,
		ResourceID:         string(uid),
		ResourceUID:        core.ResourceUID(uid),
		PublishedVersionID: int64(f.sequence),
		UpdatedAt:          updatedAt,
	}
	return manifest.BundleHash
}

// TestAssetSync_PrepareBeforeEntryBuild 守 §2.3.4 的顺序：Provider 准备 → MCP 准备 →
// Entry 构建，三者不可交换（先建 Entry 会把"本轮还没准备好的资源"误报成 entry 非法）。
func TestAssetSync_PrepareBeforeEntryBuild(t *testing.T) {
	f := newAssetSyncFixture(t)
	f.addBundle("alpha", prepareAgentUID(t), 1000)

	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	loader := &prepareEntryRecorder{inner: prepareOKLoader{}, onCall: func() { record("entry") }}

	sync, err := NewAssetSync(f.deps(func(deps *AssetSyncDeps) {
		deps.Loader = loader
		deps.Providers = func(context.Context) (CatalogReport, error) {
			record("providers")
			return CatalogReport{Providers: []string{"alpha-provider"}}, nil
		}
		deps.MCP = func(context.Context) (MCPReport, error) {
			record("mcp")
			return MCPReport{Configured: []string{"alpha-mcp"}, Connected: []string{"alpha-mcp"}}, nil
		}
	}))
	require.NoError(t, err)
	report := f.syncOnce(sync)

	mu.Lock()
	got := slices.Clone(events)
	mu.Unlock()
	require.Equal(t, []string{"providers", "mcp", "entry"}, got,
		"资源准备必须先于 Entry 构建，且 Provider 先于 MCP")
	require.Equal(t, 1, loader.callCount())
	require.Len(t, report.Items, 1)
	require.Equal(t, syncStateReady, report.Items[0].State)
}

// TestAssetSync_EntryUsesPreparedSnapshot 守 C14：Entry 判定只读**本轮**资源快照，
// 不读实时目录；快照是拷贝，报告侧后续改动不得回流进判定输入。
func TestAssetSync_EntryUsesPreparedSnapshot(t *testing.T) {
	f := newAssetSyncFixture(t)
	f.addBundle("alpha", prepareAgentUID(t), 1000)

	loader := &prepareEntryRecorder{inner: prepareOKLoader{}}
	sync, err := NewAssetSync(f.deps(func(deps *AssetSyncDeps) {
		deps.Loader = loader
		deps.Providers = func(context.Context) (CatalogReport, error) {
			return CatalogReport{Providers: []string{"alpha-provider", "beta-provider"}}, nil
		}
		deps.MCP = func(context.Context) (MCPReport, error) {
			return MCPReport{
				Configured: []string{"alpha-mcp", "beta-mcp"},
				Connected:  []string{"alpha-mcp"},
				Tools:      []string{"alpha-tool"},
			}, nil
		}
	}))
	require.NoError(t, err)
	report := f.syncOnce(sync)

	// Tools 是注册池快照：内置声明名集 + 本轮已连接 MCP 工具（§3.1.4 C14）。
	// Tools is the registry snapshot: declared builtins plus this round's MCP tools.
	wantTools := append(slices.Clone(declaredToolNames), "alpha-tool")
	slices.Sort(wantTools)
	require.Equal(t, PreparedResources{
		Providers:     []string{"alpha-provider", "beta-provider"},
		MCPConfigured: []string{"alpha-mcp", "beta-mcp"},
		MCPConnected:  []string{"alpha-mcp"},
		Tools:         wantTools,
	}, loader.snapshotOf(), "Entry 判定必须拿到本轮准备的完整快照")

	// 快照是克隆：报告侧事后改动不得影响本轮已交给判定的输入。
	report.Providers.Providers[0] = "tampered"
	report.MCP.Connected[0] = "tampered"
	require.Equal(t, []string{"alpha-provider", "beta-provider"}, loader.snapshotOf().Providers)
	require.Equal(t, []string{"alpha-mcp"}, loader.snapshotOf().MCPConnected)
}

// TestAssetSync_PreparedToolsIncludeBuiltins 守 §3.1.4 C14 的 Tools 快照口径：
// `Tools` 取**进程内注册池**的名字快照（内置十二件 + 本轮已连接 MCP 工具），不是
// 只有 MCP 工具——bundle 路径的规则 4 拿它判 `tools:` 声明，缺内置名就把声明
// `read_file` 的 Agent 整只判成未注册（2026-11-10 定时任务实测复现：
// "tool not registered: read_file"）。
// Tools must be the registry name snapshot (builtins plus this round's MCP tools);
// an MCP-only snapshot rejects agents that declare builtin tools.
func TestAssetSync_PreparedToolsIncludeBuiltins(t *testing.T) {
	f := newAssetSyncFixture(t)
	addProviderBundle(t, f, "alpha", prepareAgentUID(t), 1000, `---
name: alpha
description: 内置工具声明用例
provider:
  name: deepseek
  model: deepseek-chat
tools:
  - read_file
  - alpha-tool
---

内置工具声明判据正文。
`)

	loader := &prepareEntryRecorder{inner: workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Logger: prepareDiscardLogger(),
	})}
	sync, err := NewAssetSync(f.deps(func(deps *AssetSyncDeps) {
		deps.Loader = loader
		deps.Providers = func(context.Context) (CatalogReport, error) {
			return CatalogReport{Providers: []string{"deepseek"}}, nil
		}
		deps.MCP = func(context.Context) (MCPReport, error) {
			return MCPReport{
				Configured: []string{"alpha-mcp"},
				Connected:  []string{"alpha-mcp"},
				Tools:      []string{"alpha-tool"},
			}, nil
		}
	}))
	require.NoError(t, err)
	report := f.syncOnce(sync)

	require.Len(t, report.Items, 1)
	require.NoError(t, loader.lastError(), "声明了内置工具的 Agent 必须能构建注册条目")
	require.Equal(t, syncStateReady, report.Items[0].State)

	wantTools := append(slices.Clone(declaredToolNames), "alpha-tool")
	slices.Sort(wantTools)
	require.Equal(t, wantTools, loader.snapshotOf().Tools,
		"Tools 快照必须同时含内置十二件与本轮已连接的 MCP 工具")
}

// TestRegistration_BlocksOnMissingResource 守"注册前未准备资源则报告点名缺项"：
// 本轮没有准备出被声明的 Provider 时，该 Agent 不注册、不进调度集合，阻塞原因按
// entry 非法记录，且错误本身点名缺的是哪个资源。
func TestRegistration_BlocksOnMissingResource(t *testing.T) {
	f := newAssetSyncFixture(t)
	addProviderBundle(t, f, "alpha", prepareAgentUID(t), 1000, `---
name: alpha
description: 资源准备用例
provider:
  name: deepseek
  model: deepseek-chat
---

资源准备判据正文。
`)

	loader := &prepareEntryRecorder{inner: workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Logger: prepareDiscardLogger(),
	})}
	sync, err := NewAssetSync(f.deps(func(deps *AssetSyncDeps) {
		deps.Loader = loader
		// 本轮确实做了准备，但没准备出 deepseek——判定必须按缺失处理。
		deps.Providers = func(context.Context) (CatalogReport, error) {
			return CatalogReport{}, nil
		}
	}))
	require.NoError(t, err)
	report := f.syncOnce(sync)

	require.Empty(t, f.registry.entries, "资源未准备时不得注册该 Agent")
	require.Empty(t, f.scheduler.entries, "注册表与调度集合必须同轮，注册没发生就没有调度项")
	require.Len(t, report.Items, 1)
	require.Equal(t, syncStateBlocked, report.Items[0].State)
	require.Equal(t, syncBlockedEntryInvalid, report.Items[0].BlockedReason)

	var rejected *core.AgentDefinitionRejectedError
	buildErr := loader.lastError()
	require.ErrorAs(t, buildErr, &rejected, "阻塞必须来自类型化拒绝，而不是笼统失败")
	require.Equal(t, core.AgentRejectProviderNotRegistered, rejected.Kind)
	require.Equal(t, "deepseek", rejected.Detail, "报告必须点名缺的是哪个资源")
}
