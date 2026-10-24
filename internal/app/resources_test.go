package app

// 本文件覆盖准入资源解析与只读投影（模块卡 §3.1.32、§4.4 的 011-web-observability
// 的 TestRunResources_* 家族）：发布 / 已应用指针的固定口径、失败关闭的分类、闸门
// 读锁、运行时的固定与释放、只读视图的列表 / 详情与凭证脱敏。
// 数据侧一律走公开仓储；只有"正常写入路径拒绝制造的库内状态"（损坏的清单行、
// 未释放的资源引用）用裸连接制造，并逐处注明理由。
// The admission-resolver and read-projection acceptance tests. Seeding goes through
// public stores; a bare connection is used only for states the write path refuses to
// create, each annotated at the call site.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/web"
	"github.com/ymedword/basalt/internal/workspace"
)

// runResourcesEnv 是资源适配器的夹具：临时工作区 + 临时库 + 显式 Provider 目录 +
// 只读 MCP 运行期视图 + 共享资源闸门。
// runResourcesEnv is the fixture for both resource adapters.
type runResourcesEnv struct {
	root        string
	dbPath      string
	db          *storage.DB
	versions    *storage.ResourceStore
	ops         *storage.ManagementStore
	credentials *storage.CredentialStore
	providers   *providerCatalog
	llm         *provider.ProviderService
	mcp         *mcpRuntime
	skills      *workspace.SkillLibrary
	lifecycle   *workspace.LifecycleStore
	gate        *core.ResourceGate
	resources   core.RunResources
	views       web.ResourceReader
	// admission 是准入侧的只读依赖（身份镜像 / 通知目录 / 绑定投影），与生产装配同形。
	// admission mirrors the production admission dependencies.
	admission AdmissionDeps
	// opID 是夹具预写的一条 pending 管理操作：`resource_versions.operation_id` 是
	// 非空外键，正常路径由基线或发布写入，夹具复用它承载全部种子版本。
	// opID is the fixture's pending operation, reused as the FK for seeded versions.
	opID string
}

// newRunResourcesEnv 建夹具：Provider alpha / beta 已装配，MCP web 已配置且已连接。
// newRunResourcesEnv assembles the fixture; no resource version is seeded yet.
func newRunResourcesEnv(t *testing.T) *runResourcesEnv {
	t.Helper()

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)

	dbPath := filepath.Join(root, databaseFileName)
	db, err := storage.OpenWritable(context.Background(), dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	client := providerClientFunc(func(context.Context, core.ChatRequest) (core.ChatResponse, error) {
		return core.ChatResponse{}, nil
	})
	providers := &providerCatalog{
		clients: map[string]core.ProviderClient{"alpha": client, "beta": client},
		models:  map[string]string{"alpha": "alpha-model", "beta": "beta-model"},
	}
	// 准入固定的客户端是**带两阶段审计的路由视图**，不是目录里的裸协议客户端
	// （人工 review ② 缺陷 2）：测试夹具也必须走生产那条构造。
	llm := provider.NewProviderService(providers.clients, providers.models,
		storage.NewLLMCallRepo(db), nil)
	mcp := newMCPRuntime(
		[]core.MCPServerConfig{{Name: "web"}},
		[]tool.MCPConnection{{Server: "web", Tools: []core.Tool{fakeTool{name: "web_search"}}}},
	)

	versions := storage.NewResourceStore(db)
	skills := workspace.NewSkillLibrary(root)
	lifecycle := workspace.NewLifecycleStore(root, baselineClock)
	gate := &core.ResourceGate{}

	ops := storage.NewManagementStore(db)
	opID := core.NewOperationID(baselineClock())
	require.NoError(t, ops.BeginOperation(context.Background(), core.OperationRecord{
		OperationID:  opID,
		Action:       "system.baseline",
		TargetKind:   "system",
		TargetID:     "system",
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       "test",
		Status:       core.OperationPending,
		CreatedAt:    baselineClock(),
	}))

	// 准入要先把名字固定成 agent_uid（v3）；夹具按生产路径写入身份镜像行，
	// 不给"没有身份也能跑"的假绿。
	// Admission freezes the agent uid first, so the fixture records the identity row.
	agentUID, err := core.NewAgentUID()
	require.NoError(t, err)
	require.NoError(t, versions.PutLifecycleIndex(context.Background(), core.LifecycleRecord{
		SchemaVersion: 2,
		Agent:         "writer",
		AgentUID:      agentUID,
		State:         core.AgentDisabled,
		Generation:    1,
		UpdatedAt:     baselineClock().UnixMilli(),
		OperationID:   opID,
	}, true, ""))

	admission := AdmissionDeps{
		Identities:  versions,
		NotifyAdmin: storage.NewNotifyAdminStore(db),
		Channels:    storage.NewNotifyChannelRepo(db),
		Bindings:    storage.NewBindingStore(db),
	}

	return &runResourcesEnv{
		opID:        opID,
		root:        root,
		dbPath:      dbPath,
		db:          db,
		versions:    versions,
		ops:         ops,
		credentials: storage.NewCredentialStore(db),
		providers:   providers,
		llm:         llm,
		mcp:         mcp,
		skills:      skills,
		lifecycle:   lifecycle,
		gate:        gate,
		resources:   NewRunResources(versions, providers, llm, mcp, skills, gate, admission),
		views:       NewResourceViews(versions, lifecycle, mcp, storage.NewCredentialStore(db), ResourceViewDeps{}),
		admission:   admission,
	}
}

// fixtureResourceUID 由 `(kind, id)` 派生一个**稳定**的资源身份：同一资源的多个版本
// 必须共享一个 UID（指针与版本按 UID 对账），重复播种也要得到同一个值。载荷按
// Crockford Base32 取模映射，与 `core.NewResourceUID` 的字符集一致。
// fixtureResourceUID derives a stable uid for a (kind, id) pair so repeated seeding of
// the same resource keeps one identity.
func fixtureResourceUID(t *testing.T, kind core.ResourceKind, id string) core.ResourceUID {
	t.Helper()
	uid := fixtureResourceUIDFor(kind, id)
	_, err := core.ParseResourceUID(string(uid))
	require.NoError(t, err)
	return uid
}

// fixtureResourceUIDFor 是 fixtureResourceUID 的无 *testing.T 版本：绑定声明要在构造
// Agent 入口时同步给出同名 UID，那里拿不到 t。
// fixtureResourceUIDFor is the t-free form so entry builders can declare bindings.
func fixtureResourceUIDFor(kind core.ResourceKind, id string) core.ResourceUID {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // 与 core 的字符集一致
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + id))
	payload := make([]byte, 26)
	for i := range payload {
		payload[i] = crockford[int(sum[i%len(sum)])%len(crockford)]
	}
	return core.ResourceUID(core.ResourceUIDPrefix) + core.ResourceUID(payload)
}

// seedVersion 幂等写入一个资源版本行；哈希由内容原文派生，同内容天然幂等。
// seedVersion writes one version row whose hash derives from its content.
func (e *runResourcesEnv) seedVersion(t *testing.T, kind core.ResourceKind, id, definition, manifest, storagePath string) core.ResourceVersion {
	t.Helper()
	version := core.ResourceVersion{
		Kind:           kind,
		ResourceID:     id,
		ResourceUID:    fixtureResourceUID(t, kind, id),
		ContentHash:    sha256Hex(t, []byte(definition+"\x00"+manifest+"\x00"+storagePath)),
		DefinitionYAML: definition,
		ManifestJSON:   manifest,
		StoragePath:    storagePath,
		CreatedAt:      baselineClock().UnixMilli(),
		OperationID:    e.opID,
	}
	created, _, err := e.versions.EnsureVersion(context.Background(), version)
	require.NoError(t, err)
	return created
}

// publish 更新发布指针；applied=true 时同时把实际生效指针指向该版本。
// publish moves the pointer; applied=true also moves the applied pointer.
func (e *runResourcesEnv) publish(t *testing.T, kind core.ResourceKind, id string, versionID int64, applied bool) {
	t.Helper()
	require.NoError(t, e.versions.Publish(context.Background(), kind, id, versionID, applied, e.opID, baselineClock()))
}

// providerDefinition / mcpDefinition 是与 basalt.yaml providers[] / servers[] 同形的定义原文。
// 模型连接的 api_key 必须写成 `${ENV_VAR}`；MCP 的 env 值同理。
// providerDefinition / mcpDefinition mirror the config-file blocks verbatim.
func providerDefinition(name, baseURL, apiKeyEnv, model string) string {
	return "name: " + name + "\n" +
		"base_url: " + baseURL + "\n" +
		"api_key: " + apiKeyEnv + "\n" +
		"default_model: " + model + "\n"
}

// mcpDefinition 拼一份 stdio MCP 定义；env 只有引用名。
// mcpDefinition builds a stdio MCP definition whose env holds reference names only.
func mcpDefinition(name, command string, envKeys ...string) string {
	def := "name: " + name + "\ntransport: stdio\ncommand: " + command + "\n"
	if len(envKeys) == 0 {
		return def
	}
	def += "env:\n"
	for _, key := range envKeys {
		def += "  " + key + ": ${" + key + "}\n"
	}
	return def
}

// skillManifest 拼技能文件清单（顺序即写入顺序）。
// skillManifest renders a skill file list in the given order.
func skillManifest(paths ...string) string {
	files := make([]map[string]any, 0, len(paths))
	for i, path := range paths {
		files = append(files, map[string]any{"path": path, "sha256": "hash-" + string(rune('a'+i)), "size": i + 1})
	}
	raw, err := json.Marshal(files)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// agentEntry 构造一个声明了 Provider / MCP / 技能的 Agent 入口。
// agentEntry builds an agent entry with the given bindings.
func agentEntry(provider string, mcps, skills []string) core.AgentEntry {
	profile := &core.Profile{
		Name:       "writer",
		Provider:   core.ProviderRef{Name: provider, Model: "alpha-model"},
		MCPServers: mcps,
		Skills:     skills,
	}
	if provider != "" {
		// 引用与绑定严格一一对应（R4.4）：夹具按同一份 (kind, name) 派生 UID，
		// 与 seedVersion 写入的资源身份天然一致。
		profile.ResourceBindings = core.ResourceBindings{
			ModelConnections: []core.ResourceBinding{{
				Name: provider,
				UID:  fixtureResourceUIDFor(core.ResourceModelConnection, provider),
			}},
		}
	}
	return core.AgentEntry{Name: "writer", Registered: true, Profile: profile}
}

// requireUnavailable 断言准入失败是 `*core.ResourceUnavailableError` 且 Reason 逐字命中。
// requireUnavailable pins the failure classification and its fixed reason.
func requireUnavailable(t *testing.T, err error, reason string) *core.ResourceUnavailableError {
	t.Helper()
	var unavailable *core.ResourceUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, reason, unavailable.Reason)
	require.Equal(t, "writer", unavailable.Name)
	require.Error(t, unavailable.Cause, "底层原因必须保留，只经 Unwrap 暴露")
	return unavailable
}

// openRawWrite 开一条不带 pragma 的裸连接（与 wire_test.go 的 openRead 同一手法）。
// 只用于制造正常写入路径拒绝制造的库内状态。
// openRawWrite opens a bare connection without the production pragmas; it exists only
// to create row states the write path refuses to produce.
func openRawWrite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// TestRunResources_ResolvePinsPublishedVersions 覆盖成功路径的版本固定：Provider 与
// 技能取发布指针的最新版本、MCP 取已应用版本（可以比发布版本旧），运行时同时固定。
// 逆改点：MCP 改用发布指针 / 技能版本取错行 / 运行时漏装 → 三条断言各自红。
func TestRunResources_ResolvePinsPublishedVersions(t *testing.T) {
	env := newRunResourcesEnv(t)
	ctx := context.Background()

	providerV1 := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", providerV1.ID, true)
	providerV2 := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:2", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", providerV2.ID, true)

	mcpV1 := env.seedVersion(t, core.ResourceMCPServer, "web",
		mcpDefinition("web", "npx", "MCP_TOKEN"), "", "")
	env.publish(t, core.ResourceMCPServer, "web", mcpV1.ID, true)
	mcpV2 := env.seedVersion(t, core.ResourceMCPServer, "web",
		mcpDefinition("web", "node", "MCP_TOKEN"), "", "")
	env.publish(t, core.ResourceMCPServer, "web", mcpV2.ID, false) // 已发布、待重启

	skill := env.seedVersion(t, core.ResourceSkill, "pdf", "", skillManifest("SKILL.md", "references/guide.md"),
		".basalt/versions/skills/pdf/1")
	env.publish(t, core.ResourceSkill, "pdf", skill.ID, true)

	resolved, err := env.resources.Resolve(ctx, agentEntry("alpha", []string{"web"}, []string{"pdf"}))
	require.NoError(t, err)
	// Resolve 成功即带回读锁租约（人工 review ② US-12 交接裁决）：本用例不跑 StartRun，用毕归还。
	t.Cleanup(func() { resolved.Runtime.Release() })

	require.Equal(t, "alpha", resolved.Provider.Name)
	require.Equal(t, providerV2.ID, resolved.Provider.VersionID, "Provider 必须取已发布的最新版本")
	require.Equal(t, 2, resolved.Provider.VersionNo)
	require.Equal(t, providerV2.ContentHash, resolved.Provider.ContentHash)

	require.Len(t, resolved.MCPServers, 1)
	require.Equal(t, mcpV1.ID, resolved.MCPServers[0].VersionID, "MCP 必须取已应用版本，不得取未生效的发布版本")
	require.Equal(t, 1, resolved.MCPServers[0].VersionNo)

	require.Len(t, resolved.Skills, 1)
	require.Equal(t, skill.ID, resolved.Skills[0].VersionID)
	require.Equal(t, 1, resolved.Skills[0].VersionNo)
	require.Len(t, resolved.Skills[0].Files, 2)

	client, ok := resolved.Runtime.Provider("alpha")
	require.True(t, ok, "运行时必须固定本次的 Provider 客户端")
	require.NotNil(t, client)
	handle, ok := resolved.Runtime.MCP("web")
	require.True(t, ok, "运行时必须固定本次的 MCP 句柄")
	require.Equal(t, "web", handle.ServerName())
	_, ok = resolved.Runtime.Provider("beta")
	require.False(t, ok, "未绑定的 Provider 不得出现在运行时视图里")
	_, ok = resolved.Runtime.MCP("queue")
	require.False(t, ok)
}

// TestRunResources_ResolveMissingProvider 覆盖 Provider 缺失的两种分类：未声明与指针缺失。
// 逆改点：缺失时退回工作副本 / 把指针缺失归到运行时分类 → 断言红。
func TestRunResources_ResolveMissingProvider(t *testing.T) {
	env := newRunResourcesEnv(t)

	t.Run("未声明 Provider", func(t *testing.T) {
		_, err := env.resources.Resolve(context.Background(), agentEntry("", nil, nil))
		unavailable := requireUnavailable(t, err, resourceReasonProvider)
		require.Contains(t, unavailable.Error(), "resource unavailable: writer")
	})

	t.Run("指针缺失", func(t *testing.T) {
		_, err := env.resources.Resolve(context.Background(), agentEntry("ghost", nil, nil))
		unavailable := requireUnavailable(t, err, resourceReasonProvider)
		require.ErrorIs(t, unavailable.Cause, core.ErrResourceNotFound)
	})
}

// TestRunResources_ResolveProviderNotInCatalog 覆盖"指针在、显式映射表里没有"：
// 资源账与发布映射必须来自同一次装配，缺条目说明装配被绕过——失败关闭。
// 逆改点：map 里查不到就新建客户端 / 回落到唯一那个 Provider → 断言红。
func TestRunResources_ResolveProviderNotInCatalog(t *testing.T) {
	env := newRunResourcesEnv(t)
	version := env.seedVersion(t, core.ResourceModelConnection, "gamma",
		providerDefinition("gamma", "http://127.0.0.1:3", "${BASALT_GAMMA_KEY}", "gamma-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "gamma", version.ID, true)

	_, err := env.resources.Resolve(context.Background(), agentEntry("gamma", nil, nil))
	unavailable := requireUnavailable(t, err, resourceReasonProviderRuntime)
	require.Contains(t, unavailable.Cause.Error(), "不在已发布映射表中")
}

// TestRunResources_ResolveMCPUsesAppliedVersion 覆盖 MCP 的已应用口径与两条失败分支：
// 已应用版本旧于发布版本照用；从未应用 → 失败；未连接 → 失败。
// 逆改点：applied 为空时回落到 published / 未连接仍发放句柄 → 断言红。
func TestRunResources_ResolveMCPUsesAppliedVersion(t *testing.T) {
	t.Run("已应用旧版本", func(t *testing.T) {
		env := newRunResourcesEnv(t)
		v1 := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "npx"), "", "")
		env.publish(t, core.ResourceMCPServer, "web", v1.ID, true)
		v2 := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "node"), "", "")
		env.publish(t, core.ResourceMCPServer, "web", v2.ID, false)

		provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
			providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
		env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)

		resolved, err := env.resources.Resolve(context.Background(), agentEntry("alpha", []string{"web"}, nil))
		require.NoError(t, err)
		t.Cleanup(func() { resolved.Runtime.Release() }) // 归还准入读锁租约
		require.Equal(t, v1.ID, resolved.MCPServers[0].VersionID)
	})

	t.Run("从未应用", func(t *testing.T) {
		env := newRunResourcesEnv(t)
		provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
			providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
		env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)
		mcp := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "npx"), "", "")
		env.publish(t, core.ResourceMCPServer, "web", mcp.ID, false)

		_, err := env.resources.Resolve(context.Background(), agentEntry("alpha", []string{"web"}, nil))
		unavailable := requireUnavailable(t, err, resourceReasonMCP)
		require.Contains(t, unavailable.Cause.Error(), "尚无已应用版本")
	})

	t.Run("未在本进程连接", func(t *testing.T) {
		env := newRunResourcesEnv(t)
		provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
			providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
		env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)
		mcp := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "npx"), "", "")
		env.publish(t, core.ResourceMCPServer, "web", mcp.ID, true)

		// 同一份库与目录，但运行期没有任何连接——句柄无从固定。
		unconnected := NewRunResources(env.versions, env.providers, env.llm,
			newMCPRuntime([]core.MCPServerConfig{{Name: "web"}}, nil), env.skills, env.gate,
			env.admission)
		_, err := unconnected.Resolve(context.Background(), agentEntry("alpha", []string{"web"}, nil))
		unavailable := requireUnavailable(t, err, resourceReasonMCP)
		require.Contains(t, unavailable.Cause.Error(), "未在本进程连接")
	})
}

// TestRunResources_ResolveSkillRealPath 覆盖技能目录取 `storage_path` 原文：工作区相对
// 路径（不是绝对路径），文件清单顺序照搬发布行——排序归 BuildRunResourceManifest。
// 逆改点：用 root 拼绝对路径 / 在解析层重排文件 → 断言红。
func TestRunResources_ResolveSkillRealPath(t *testing.T) {
	env := newRunResourcesEnv(t)
	provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)

	const storagePath = ".basalt/versions/skills/pdf/2"
	skill := env.seedVersion(t, core.ResourceSkill, "pdf", "",
		skillManifest("references/guide.md", "SKILL.md"), storagePath)
	env.publish(t, core.ResourceSkill, "pdf", skill.ID, true)

	resolved, err := env.resources.Resolve(context.Background(), agentEntry("alpha", nil, []string{"pdf"}))
	require.NoError(t, err)
	pdfRuntime := resolved.Runtime
	t.Cleanup(func() { pdfRuntime.Release() }) // 归还准入读锁租约（下面 resolved 会被再次赋值）
	dir := resolved.SkillDirs["pdf"]
	require.Equal(t, storagePath, dir, "技能目录必须是 storage_path 原文")
	require.False(t, filepath.IsAbs(dir), "storage_path 是工作区相对路径")
	require.NotContains(t, dir, env.root, "不得把临时工作区根拼进清单")
	require.Equal(t, []string{"references/guide.md", "SKILL.md"},
		[]string{resolved.Skills[0].Files[0].Path, resolved.Skills[0].Files[1].Path},
		"解析层不得重排文件清单")

	empty := env.seedVersion(t, core.ResourceSkill, "bare", "", "[]", ".basalt/versions/skills/bare/1")
	env.publish(t, core.ResourceSkill, "bare", empty.ID, true)
	resolved, err = env.resources.Resolve(context.Background(), agentEntry("alpha", nil, []string{"bare"}))
	require.NoError(t, err)
	bareRuntime := resolved.Runtime
	t.Cleanup(func() { bareRuntime.Release() })
	require.NotNil(t, resolved.Skills[0].Files, "空清单必须是 []，不是 nil")
	require.Empty(t, resolved.Skills[0].Files)
}

// TestRunResources_ResolveMissingSkillManifest 覆盖四条损坏发布行的失败关闭：坏 JSON、
// null 清单、空路径与空存储路径——一律报错，不当作空技能继续。
// 逆改点：把损坏清单当成空列表 / 空存储路径照发 → 断言红。
func TestRunResources_ResolveMissingSkillManifest(t *testing.T) {
	env := newRunResourcesEnv(t)
	provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)

	seed := func(t *testing.T, name, manifest, storagePath string) {
		t.Helper()
		version := env.seedVersion(t, core.ResourceSkill, name, "", manifest, storagePath)
		env.publish(t, core.ResourceSkill, name, version.ID, true)
	}

	cases := []struct {
		name       string
		manifest   string
		storage    string
		wantPhrase string
	}{
		{"清单不是 JSON 数组", "not-json", ".basalt/versions/skills/broken/1", "无法解析"},
		{"清单为 null", "null", ".basalt/versions/skills/broken/1", "不是数组"},
		{"清单含空路径", `[{"path":"","sha256":"a","size":1}]`, ".basalt/versions/skills/broken/1", "空路径"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seed(t, "broken", tc.manifest, tc.storage)
			_, err := env.resources.Resolve(context.Background(), agentEntry("alpha", nil, []string{"broken"}))
			unavailable := requireUnavailable(t, err, resourceReasonSkill)
			require.Contains(t, unavailable.Cause.Error(), tc.wantPhrase)
		})
	}

	t.Run("存储路径被清空", func(t *testing.T) {
		seed(t, "bare", "[]", ".basalt/versions/skills/bare/1")
		// 正常写入路径不允许空 storage_path（EnsureVersion 校验），只能改库制造。
		raw := openRawWrite(t, env.dbPath)
		_, execErr := raw.ExecContext(context.Background(),
			`UPDATE resource_versions SET storage_path = '' WHERE resource_kind = 'skill' AND resource_id = 'bare'`)
		require.NoError(t, execErr)

		_, err := env.resources.Resolve(context.Background(), agentEntry("alpha", nil, []string{"bare"}))
		unavailable := requireUnavailable(t, err, resourceReasonSkill)
		require.Contains(t, unavailable.Cause.Error(), "缺少存储路径")
	})
}

// TestRunResources_RuntimeFixedAndReleased 覆盖运行时的固定与幂等释放：释放后不再
// 发放句柄，重复释放无副作用，nil 接收者安全。
// 逆改点：释放后仍返回客户端 / 双重释放 panic → 断言红。
func TestRunResources_RuntimeFixedAndReleased(t *testing.T) {
	env := newRunResourcesEnv(t)
	provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)

	resolved, err := env.resources.Resolve(context.Background(), agentEntry("alpha", nil, nil))
	require.NoError(t, err)

	runtime := resolved.Runtime
	_, ok := runtime.Provider("alpha")
	require.True(t, ok)

	runtime.Release()
	runtime.Release() // 幂等：重复归还不得 panic、不得改变已释放状态

	_, ok = runtime.Provider("alpha")
	require.False(t, ok, "释放后不得再发放 Provider")
	_, ok = runtime.MCP("web")
	require.False(t, ok, "释放后不得再发放 MCP 句柄")

	var nilRuntime *runRuntime
	nilRuntime.Release()
	_, ok = nilRuntime.Provider("alpha")
	require.False(t, ok)
	_, ok = nilRuntime.MCP("web")
	require.False(t, ok)
}

// TestRunResources_ResolveHoldsReadLock 覆盖准入持闸门读锁：发布写锁持有期间
// Resolve 不得完成，释放后必须完成——版本与运行时来自同一快照（约束 25）。
// 逆改点：Resolve 不取闸门锁 → 写锁期间的"未完成"断言红。
func TestRunResources_ResolveHoldsReadLock(t *testing.T) {
	env := newRunResourcesEnv(t)
	provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)

	type resolveOutcome struct {
		resolved core.ResolvedResources
		err      error
	}
	env.gate.Lock()
	done := make(chan resolveOutcome, 1)
	go func() {
		resolved, err := env.resources.Resolve(context.Background(), agentEntry("alpha", nil, nil))
		done <- resolveOutcome{resolved: resolved, err: err}
	}()

	select {
	case outcome := <-done:
		if outcome.resolved.Runtime != nil {
			outcome.resolved.Runtime.Release()
		}
		t.Fatalf("写锁持有期间 Resolve 不得完成（err=%v）", outcome.err)
	case <-time.After(100 * time.Millisecond):
	}

	env.gate.Unlock()
	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.resolved.Runtime, "写锁释放后 Resolve 必须在时限内完成")
		outcome.resolved.Runtime.Release() // 归还读锁租约与运行时
	case <-time.After(5 * time.Second):
		t.Fatal("写锁释放后 Resolve 必须在时限内完成")
	}
}

// TestRunResources_ReadLockLeasesUntilReleaseAdmission（人工 review ② US-12 交接裁决）：
// Resolve 成功后读锁**仍由运行时持有**——发布方取写锁必须等到 ReleaseAdmission；
// 归还之后写锁立即可得，且随后的 Release 不得重复归还读锁（重复 RUnlock 会 panic）。
// 逆改点：Resolve 内 defer 归还读锁（旧实现）→ "Resolve 返回后写锁仍拿不到"的断言红。
// The resolved runtime keeps the read lock until ReleaseAdmission; Release then must not
// return it a second time.
func TestRunResources_ReadLockLeasesUntilReleaseAdmission(t *testing.T) {
	env := newRunResourcesEnv(t)
	provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)

	resolved, err := env.resources.Resolve(context.Background(), agentEntry("alpha", nil, nil))
	require.NoError(t, err)
	require.NotNil(t, resolved.Runtime)

	locked := make(chan struct{})
	go func() {
		env.gate.Lock()
		close(locked)
		env.gate.Unlock()
	}()

	select {
	case <-locked:
		t.Fatal("Resolve 返回后发布写锁必须仍被准入读锁挡住——读锁租约不得止于 Resolve")
	case <-time.After(100 * time.Millisecond):
	}

	resolved.Runtime.ReleaseAdmission()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("ReleaseAdmission 后发布写锁必须立即可得")
	}

	resolved.Runtime.Release() // 读锁已归还：不得重复 RUnlock（synctest 之外会直接 panic）
	_, ok := resolved.Runtime.Provider("alpha")
	require.False(t, ok, "Release 之后不再发放固定视图")
}

// TestRunResources_ResourceViewsLists 覆盖三张只读列表：技能按仓储序、MCP 取
// "发布指针 ∪ 运行期已配置"的并集升序、模型连接按仓储序；缺失资源的详情返回
// `core.ErrResourceNotFound`。
// 逆改点：MCP 只列运行期名单 / 只列指针 / 详情缺失回 500 类错误 → 断言红。
func TestRunResources_ResourceViewsLists(t *testing.T) {
	env := newRunResourcesEnv(t)
	ctx := context.Background()

	for _, name := range []string{"alpha", "beta"} {
		version := env.seedVersion(t, core.ResourceModelConnection, name,
			providerDefinition(name, "http://127.0.0.1:1", "${"+strings.ToUpper(name)+"_KEY}", name+"-model"), "", "")
		env.publish(t, core.ResourceModelConnection, name, version.ID, true)
	}
	for _, name := range []string{"draft", "pdf"} {
		version := env.seedVersion(t, core.ResourceSkill, name, "", skillManifest("SKILL.md", "references/guide.md"),
			".basalt/versions/skills/"+name+"/1")
		env.publish(t, core.ResourceSkill, name, version.ID, true)
	}
	webServer := env.seedVersion(t, core.ResourceMCPServer, "web",
		mcpDefinition("web", "npx --token hidden", "MCP_TOKEN"), "", "")
	env.publish(t, core.ResourceMCPServer, "web", webServer.ID, true)

	// queue 只存在于运行期配置里（US-12 新登记、尚未重启）：列表必须并集。
	views := NewResourceViews(env.versions, env.lifecycle, newMCPRuntime(
		[]core.MCPServerConfig{{Name: "web"}, {Name: "queue"}},
		[]tool.MCPConnection{{Server: "web", Tools: []core.Tool{fakeTool{name: "web_search"}}}},
	), env.credentials, ResourceViewDeps{})
	// 约束 12：只读面不得写管理审计——读前读后操作行数相同。
	before, err := env.ops.ListOperations(ctx, core.OperationFilter{Limit: 100})
	require.NoError(t, err)

	skills, err := views.Skills(ctx)
	require.NoError(t, err)
	require.Len(t, skills, 2)
	require.Equal(t, []string{"draft", "pdf"}, []string{skills[0].Name, skills[1].Name}, "顺序取 ListPointers 的 resource_id 升序")
	require.Equal(t, ".basalt/skills/pdf", skills[1].Path)
	require.Empty(t, skills[1].Description, "description 在本 US 没有可达来源")
	require.Equal(t, 2, skills[1].FileCount)
	// R4 / FR-081 裁决（2026-10-23）：条件 ETag 不再是 content_hash 本身——还要覆盖
	// 资源 UID、修订号、范围与凭据段。这里判强 ETag 形状与"不等于纯内容哈希"，逐字段
	// 性质由 TestResources_EtagIncludesScopeAndCredential 判。
	require.Regexp(t, `^"sha256:[0-9a-f]{64}"$`, skills[1].ETag)
	require.NotEqual(t, `"sha256:`+skills[1].ContentHash+`"`, skills[1].ETag)
	require.NotNil(t, skills[1].ReferencedBy)

	mcpServers, err := views.MCPServers(ctx)
	require.NoError(t, err)
	require.Len(t, mcpServers, 2)
	require.Equal(t, []string{"queue", "web"}, []string{mcpServers[0].Name, mcpServers[1].Name}, "并集必须按名升序")
	require.Equal(t, "stdio", mcpServers[1].Transport)
	require.Equal(t, "npx", mcpServers[1].Command, "只回可执行名")
	require.Equal(t, []string{"MCP_TOKEN"}, mcpServers[1].EnvKeys)
	require.True(t, mcpServers[1].Connected)
	require.Equal(t, []string{"web_search"}, mcpServers[1].Tools)
	require.False(t, mcpServers[1].PendingRestart)
	require.NotNil(t, mcpServers[1].Tools)

	require.Equal(t, "queue", mcpServers[0].Name)
	require.Zero(t, mcpServers[0].PublishedVersionID, "无指针时版本字段为零，不伪造")
	require.False(t, mcpServers[0].Connected)
	require.False(t, mcpServers[0].PendingRestart)
	require.NotNil(t, mcpServers[0].Tools)

	connections, err := views.ModelConnections(ctx)
	require.NoError(t, err)
	require.Len(t, connections, 2)
	require.Equal(t, []string{"alpha", "beta"}, []string{connections[0].Name, connections[1].Name})
	require.Equal(t, "ALPHA_KEY", connections[0].APIKeyEnv)
	require.Equal(t, "alpha-model", connections[0].DefaultModel)
	require.NotNil(t, connections[0].ReferencedBy)

	skill, err := views.Skill(ctx, "pdf")
	require.NoError(t, err)
	require.Equal(t, "pdf", skill.Name)

	mcpDetail, err := views.McpServer(ctx, "web")
	require.NoError(t, err)
	require.Equal(t, "web", mcpDetail.View.Name)
	// 详情 ETag 与列表 / 写路径同源（FR-081 裁决取代"纯 content_hash"口径）：形状是
	// 强 ETag，且已把身份与修订折进哈希——不得再回显裸内容哈希。
	require.Regexp(t, `^"sha256:[0-9a-f]{64}"$`, mcpDetail.ETag)
	require.NotEqual(t, `"sha256:`+webServer.ContentHash+`"`, mcpDetail.ETag)
	bareDetail, err := views.McpServer(ctx, "queue")
	require.NoError(t, err)
	require.Zero(t, bareDetail.View.PublishedVersionID)
	require.Empty(t, bareDetail.ETag, "无发布指针的配置项不得伪造 ETag")

	_, err = views.Skill(ctx, "ghost")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	_, err = views.McpServer(ctx, "ghost")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	_, err = views.ModelConnection(ctx, "ghost")
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	after, err := env.ops.ListOperations(ctx, core.OperationFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, after, len(before), "只读投影不得写管理操作（约束 12）")
	require.Len(t, after, 1, "夹具里只有预写的那条基线操作")
}

// TestRunResources_ResourceViewsBlockDefinition 覆盖 `definition_yaml` 的块式列表
// 形态（管理写路径落库的块原文，约束 4）：只读投影必须与基线写入的条目映射形态
// 同形可用——两种形态都接受，且读取不改写存储字节。
// 逆改点：只按映射解析块原文 → 管理写路径写出的资源在列表 / 详情上直接 500
// （e2e `TestAdminConsoleE2E_ModelConnectionPublishRevert` 实测的首错）。
func TestRunResources_ResourceViewsBlockDefinition(t *testing.T) {
	env := newRunResourcesEnv(t)
	ctx := context.Background()

	provider := "  - name: gamma\n" +
		"    base_url: https://g.example\n" +
		"    api_key: ${GAMMA_KEY}\n" +
		"    default_model: gamma-model\n"
	providerVersion := env.seedVersion(t, core.ResourceModelConnection, "gamma", provider, "", "")
	env.publish(t, core.ResourceModelConnection, "gamma", providerVersion.ID, true)

	mcp := "  - name: files\n" +
		"    transport: stdio\n" +
		"    command: npx\n" +
		"    env:\n" +
		"      FS_TOKEN: ${FS_TOKEN}\n"
	mcpVersion := env.seedVersion(t, core.ResourceMCPServer, "files", mcp, "", "")
	env.publish(t, core.ResourceMCPServer, "files", mcpVersion.ID, true)

	views := NewResourceViews(env.versions, env.lifecycle, newMCPRuntime(nil, nil), env.credentials, ResourceViewDeps{})

	connection, err := views.ModelConnection(ctx, "gamma")
	require.NoError(t, err)
	require.Equal(t, "https://g.example", connection.BaseURL)
	require.Equal(t, "GAMMA_KEY", connection.APIKeyEnv)
	require.Equal(t, "gamma-model", connection.DefaultModel)
	require.Equal(t, providerVersion.VersionNo, connection.PublishedVersionNo)

	detail, err := views.McpServer(ctx, "files")
	require.NoError(t, err)
	require.Equal(t, "stdio", detail.View.Transport)
	require.Equal(t, "npx", detail.View.Command)
	require.Equal(t, []string{"FS_TOKEN"}, detail.View.EnvKeys)
	require.Equal(t, mcpVersion.ID, detail.View.PublishedVersionID)
}

// TestRunResources_ResourceViewsRedactCredentials 覆盖只读视图的凭证红线（约束 35）：
// 模型连接只回 `${ENV_VAR}` 引用名，非占位形态的原文回空串；MCP 只回可执行名与
// 变量键名，参数与变量值都不出站。
// 逆改点：把 api_key 原文投影进视图 / 回整条 command / 回 env 值 → 断言红。
func TestRunResources_ResourceViewsRedactCredentials(t *testing.T) {
	env := newRunResourcesEnv(t)
	ctx := context.Background()

	placeholder := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", placeholder.ID, true)
	literal := env.seedVersion(t, core.ResourceModelConnection, "legacy",
		"name: legacy\nbase_url: http://127.0.0.1:2\napi_key: sk-live-secret\n", "", "")
	env.publish(t, core.ResourceModelConnection, "legacy", literal.ID, true)

	mcp := env.seedVersion(t, core.ResourceMCPServer, "web",
		"name: web\ntransport: stdio\ncommand: npx --token arg-secret\nenv:\n  MCP_TOKEN: ${MCP_TOKEN}\n  OTHER: plaintext-secret\n", "", "")
	env.publish(t, core.ResourceMCPServer, "web", mcp.ID, true)

	connections, err := env.views.ModelConnections(ctx)
	require.NoError(t, err)
	require.Len(t, connections, 2)
	require.Equal(t, "BASALT_ALPHA_KEY", connections[0].APIKeyEnv)
	require.Equal(t, "", connections[1].APIKeyEnv, "非占位形态不得投影出任何值")

	servers, err := env.views.MCPServers(ctx)
	require.NoError(t, err)
	require.Len(t, servers, 1)
	require.Equal(t, "npx", servers[0].Command, "参数可能含凭证，不得出站")
	require.Equal(t, []string{"MCP_TOKEN", "OTHER"}, servers[0].EnvKeys)

	rawConnections, err := json.Marshal(connections)
	require.NoError(t, err)
	rawServers, err := json.Marshal(servers)
	require.NoError(t, err)
	for _, body := range []string{string(rawConnections), string(rawServers)} {
		require.NotContains(t, body, "sk-live-secret")
		require.NotContains(t, body, "arg-secret")
		require.NotContains(t, body, "plaintext-secret")
		require.NotContains(t, body, "--token")
	}
}

// TestRunResources_ResourceViewsPendingRestartAndReferencedBy 覆盖需重启标记与未释放
// 引用：发布 ≠ 已应用 ⇒ pending_restart；`released_at` 非空的引用不计入 referenced_by。
// 资源引用行由准入门槛写入，正常路径没有"终态外"的插入口——用裸连接制造未释放引用。
// 逆改点：pending_restart 只看有指针 / 把已释放引用也算进 referenced_by → 断言红。
func TestRunResources_ResourceViewsPendingRestartAndReferencedBy(t *testing.T) {
	env := newRunResourcesEnv(t)
	ctx := context.Background()

	provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)
	mcpV1 := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "npx"), "", "")
	env.publish(t, core.ResourceMCPServer, "web", mcpV1.ID, true)
	mcpV2 := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "node"), "", "")
	env.publish(t, core.ResourceMCPServer, "web", mcpV2.ID, false)
	skill := env.seedVersion(t, core.ResourceSkill, "pdf", "", skillManifest("SKILL.md"),
		".basalt/versions/skills/pdf/1")
	env.publish(t, core.ResourceSkill, "pdf", skill.ID, true)

	servers, err := env.views.MCPServers(ctx)
	require.NoError(t, err)
	require.True(t, servers[0].PendingRestart, "发布版本未生效必须标需重启")
	require.Equal(t, mcpV2.ID, servers[0].PublishedVersionID)
	require.Equal(t, mcpV2.VersionNo, servers[0].PublishedVersionNo)
	require.Equal(t, mcpV1.ID, servers[0].AppliedVersionID)
	require.Equal(t, mcpV1.VersionNo, servers[0].AppliedVersionNo)

	// 裸连接：foreign_keys 默认关闭，可写入尚未建 Run 行的引用（本用例只验读投影）。
	raw := openRawWrite(t, env.dbPath)
	insert := `INSERT INTO run_resource_refs
	    (run_id, resource_kind, resource_id, version_id, content_hash, released_at)
	    VALUES (?, ?, ?, ?, ?, ?)`
	_, err = raw.ExecContext(ctx, insert, "run-live", string(core.ResourceModelConnection), "alpha",
		provider.ID, provider.ContentHash, nil)
	require.NoError(t, err)
	releasedAt := baselineClock().UnixMilli()
	_, err = raw.ExecContext(ctx, insert, "run-done", string(core.ResourceSkill), "pdf",
		skill.ID, skill.ContentHash, releasedAt)
	require.NoError(t, err)

	connections, err := env.views.ModelConnections(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"run-live"}, connections[0].ReferencedBy, "未释放引用必须计入")

	skills, err := env.views.Skills(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{}, skills[0].ReferencedBy, "已释放引用不得计入")
	require.NotNil(t, skills[0].ReferencedBy)
}

// writeAgentMD 落一份声明了给定技能的 AGENT.md（快照装配的唯一正文来源）。
// writeAgentMD writes the AGENT.md that the snapshot assembler reads.
func (e *runResourcesEnv) writeAgentMD(t *testing.T, skills ...string) {
	t.Helper()
	var body strings.Builder
	body.WriteString("---\nname: writer\ndescription: 快照夹具\nskills:\n")
	for _, skill := range skills {
		body.WriteString("  - " + skill + "\n")
	}
	body.WriteString("---\n\n你是「writer」。\n")
	full := filepath.Join(e.root, "agents", "writer", "AGENT.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(body.String()), 0o644))
}

// writeWorkSkill 写技能工作副本。快照用例必须先有工作副本，"不得回落"的断言才立得住。
// writeWorkSkill writes the work copy so that "no fallback" is falsifiable.
func (e *runResourcesEnv) writeWorkSkill(t *testing.T, name, raw string) {
	t.Helper()
	full := filepath.Join(e.root, "skills", name, "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(raw), 0o644))
}

// publishSkillVersion 把当前工作副本冻结成发布目录并在库里发布该版本，返回 storage_path。
// publishSkillVersion freezes the current work copy and publishes it.
func (e *runResourcesEnv) publishSkillVersion(t *testing.T, name string, versionNo int) string {
	t.Helper()
	work, err := e.skills.HashWorkCopy(name)
	require.NoError(t, err)
	published, err := e.skills.Materialize(context.Background(), name, fixtureResourceUID(t, core.ResourceSkill, name), versionNo, work.ContentHash)
	require.NoError(t, err)
	version := e.seedVersion(t, core.ResourceSkill, name, "", skillManifest("SKILL.md"), published.Dir)
	e.publish(t, core.ResourceSkill, name, version.ID, true)
	return published.Dir
}

// manifestHashes 把快照清单摊平成 path → sha256。
// manifestHashes flattens the snapshot manifest into a lookup map.
func manifestHashes(snapshot core.ConfigSnapshot) map[string]string {
	m := make(map[string]string, len(snapshot.Manifest))
	for _, entry := range snapshot.Manifest {
		m[entry.Path] = entry.SHA256
	}
	return m
}

// TestRunResources_SnapshotBuilderUsesPublishedCards 覆盖约束 31 的生产接线：准入结果里的
// `SkillDirs` 原样交给装配器——卡片行是发布真路径、清单记逻辑路径且哈希取发布字节；
// 发布后改写工作副本不影响快照。
// 逆改点：Builder 改读工作副本 / 把发布真路径写进清单 → 断言红。
func TestRunResources_SnapshotBuilderUsesPublishedCards(t *testing.T) {
	env := newRunResourcesEnv(t)
	env.writeAgentMD(t, "pdf")
	const publishedMD = "---\nname: pdf\ndescription: PDF 技能\n---\n\n发布正文\n"
	env.writeWorkSkill(t, "pdf", publishedMD)
	dir := env.publishSkillVersion(t, "pdf", 1)
	require.Equal(t, ".basalt/versions/skills/pdf/"+string(fixtureResourceUID(t, core.ResourceSkill, "pdf"))+"/1", dir,
		"新发布的版本目录按身份分段")

	// 发布之后改写工作副本：快照不得读它。
	env.writeWorkSkill(t, "pdf", "---\nname: pdf\ndescription: 改过的说明\n---\n\n改过的正文\n")

	provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)

	entry := agentEntry("alpha", nil, []string{"pdf"})
	resolved, err := env.resources.Resolve(context.Background(), entry)
	require.NoError(t, err)
	t.Cleanup(func() { resolved.Runtime.Release() }) // 归还准入读锁租约

	builder := newRunSnapshotBuilder(workspace.NewContextLoader(nil), env.root, baselineClock)
	snapshot, err := builder.Build(context.Background(), entry, resolved)
	require.NoError(t, err)

	require.Contains(t, snapshot.SectionOne,
		"- pdf：PDF 技能（"+dir+"/SKILL.md）", "卡片行展示发布真路径")
	require.NotContains(t, snapshot.SectionOne, "改过的说明")
	require.NotContains(t, snapshot.SectionOne, ".basalt/skills/pdf/SKILL.md")

	manifest := manifestHashes(snapshot)
	require.Equal(t, sha256Hex(t, []byte(publishedMD)), manifest[".basalt/skills/pdf/SKILL.md"],
		"清单记逻辑路径，哈希取发布版本的字节")
	require.NotContains(t, manifest, dir+"/SKILL.md", "发布真路径不进清单")
}

// TestRunResources_SnapshotBuilderFailsClosed 覆盖装配器的失败关闭矩阵：准入结果缺目录
// 映射、映射形态不符、发布目录缺 SKILL.md——一律报错；工作副本存在也不得回落。
// 逆改点：缺映射时退回工作副本 / 吞掉读取错误 → 断言红。
func TestRunResources_SnapshotBuilderFailsClosed(t *testing.T) {
	env := newRunResourcesEnv(t)
	env.writeAgentMD(t, "pdf")
	// 工作副本存在：任何回落都会让下面的用例"看起来成功"。
	env.writeWorkSkill(t, "pdf", "---\nname: pdf\ndescription: 工作副本\n---\n\n正文\n")
	// 版本 3 的发布目录存在但没有 SKILL.md。
	require.NoError(t, os.MkdirAll(filepath.Join(env.root, "versions", "skills", "pdf", "3"), 0o750))

	builder := newRunSnapshotBuilder(workspace.NewContextLoader(nil), env.root, baselineClock)
	entry := agentEntry("alpha", nil, []string{"pdf"})

	cases := []struct {
		label  string
		dirs   map[string]string
		phrase string
	}{
		{"缺目录映射", map[string]string{}, "缺少发布目录"},
		{"目录形态不符", map[string]string{"pdf": ".basalt/skills/pdf"}, "不符合契约形态"},
		{"发布目录缺 SKILL.md", map[string]string{"pdf": ".basalt/versions/skills/pdf/3"}, "读取技能定义"},
	}
	for _, tc := range cases {
		_, err := builder.Build(context.Background(), entry, core.ResolvedResources{SkillDirs: tc.dirs})
		require.ErrorContains(t, err, tc.phrase, "场景「%s」必须失败关闭", tc.label)
	}
}

// TestRunResources_SnapshotBuilderPinsAdmissionVersion 覆盖"运行中的运行不受新发布影响"：
// 旧准入结果（SkillDirs 指向版本 1）在版本 2 发布之后仍构建出版本 1 的快照——装配器只消费
// 准入时固定的目录，不在构建时重新查库。
// 逆改点：Builder 自己查发布指针 / 重新解析版本 → 断言红。
func TestRunResources_SnapshotBuilderPinsAdmissionVersion(t *testing.T) {
	env := newRunResourcesEnv(t)
	env.writeAgentMD(t, "pdf")
	const v1MD = "---\nname: pdf\ndescription: 第一版\n---\n\n第一版正文\n"
	const v2MD = "---\nname: pdf\ndescription: 第二版\n---\n\n第二版正文\n"

	provider := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", provider.ID, true)

	env.writeWorkSkill(t, "pdf", v1MD)
	env.publishSkillVersion(t, "pdf", 1)
	entry := agentEntry("alpha", nil, []string{"pdf"})
	admitted, err := env.resources.Resolve(context.Background(), entry)
	require.NoError(t, err)
	t.Cleanup(func() { admitted.Runtime.Release() })
	pdfDir := func(versionNo int) string {
		return ".basalt/versions/skills/pdf/" + string(fixtureResourceUID(t, core.ResourceSkill, "pdf")) + "/" +
			strconv.Itoa(versionNo)
	}
	require.Equal(t, pdfDir(1), admitted.SkillDirs["pdf"])
	// 模拟 StartRun 成功：读锁租约到此归还，运行继续用准入时固定的视图——之后的发布不再被它挡住。
	admitted.Runtime.ReleaseAdmission()

	// 第二版发布：指针移向版本 2，新准入会固定新目录。
	env.writeWorkSkill(t, "pdf", v2MD)
	env.publishSkillVersion(t, "pdf", 2)
	refreshed, err := env.resources.Resolve(context.Background(), entry)
	require.NoError(t, err)
	t.Cleanup(func() { refreshed.Runtime.Release() })
	require.Equal(t, pdfDir(2), refreshed.SkillDirs["pdf"])

	builder := newRunSnapshotBuilder(workspace.NewContextLoader(nil), env.root, baselineClock)
	snapshot, err := builder.Build(context.Background(), entry, admitted)
	require.NoError(t, err)
	require.Contains(t, snapshot.SectionOne, pdfDir(1)+"/SKILL.md")
	require.NotContains(t, snapshot.SectionOne, pdfDir(2)+"/")
	require.NotContains(t, snapshot.SectionOne, "第二版")
	require.Equal(t, sha256Hex(t, []byte(v1MD)), manifestHashes(snapshot)[".basalt/skills/pdf/SKILL.md"])
}

// 中文验收点（T057 / 契约 SystemOverview / FR19）：概览读数适配器一次汇总库版本、
// 三个运行计数与三类发布指针计数；资源计数与只读资源路由同源（未删除发布指针），
// 撤回的指针不计。任一读失败整体报错——**不得**把读不到填成 0。
// 逆改点：统计全部指针（含 deleted_at 非空）/ 运行计数错窗口 / 漏库版本 → 断言红。
func TestRunResources_SystemOverviewCountsPublishedPointers(t *testing.T) {
	env := newRunResourcesEnv(t)
	ctx := context.Background()

	for _, name := range []string{"alpha", "beta"} {
		version := env.seedVersion(t, core.ResourceModelConnection, name,
			providerDefinition(name, "http://127.0.0.1:1", "${"+strings.ToUpper(name)+"_KEY}", name+"-model"), "", "")
		env.publish(t, core.ResourceModelConnection, name, version.ID, true)
	}
	for _, name := range []string{"draft", "pdf"} {
		version := env.seedVersion(t, core.ResourceSkill, name, "", skillManifest("SKILL.md"),
			".basalt/versions/skills/"+name+"/1")
		env.publish(t, core.ResourceSkill, name, version.ID, true)
	}
	webVersion := env.seedVersion(t, core.ResourceMCPServer, "web",
		mcpDefinition("web", "npx", "MCP_TOKEN"), "", "")
	env.publish(t, core.ResourceMCPServer, "web", webVersion.ID, true)

	reader := newSystemReader(env.db, storage.NewRunStore(env.db), env.versions, baselineClock)
	readings, err := reader.ReadOverview(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, readings.ModelConnections)
	require.EqualValues(t, 1, readings.MCPServers)
	require.EqualValues(t, 2, readings.Skills)
	require.Zero(t, readings.RunsRunning, "空账本的运行计数是真实读数 0")
	require.Zero(t, readings.RunsLast24h)
	require.Zero(t, readings.RunsFailedLast24h)
	require.Equal(t, storage.SupportedSchemaVersion, readings.SchemaVersion)

	// 撤回（deleted_at 非空）的指针不计入——正常写路径在 US-11 不提供撤回，故用裸连接
	// 制造这一库内状态（与文件头说明同一例外）。
	raw := openRawWrite(t, env.dbPath)
	_, err = raw.ExecContext(ctx,
		`UPDATE resource_pointers SET deleted_at = ? WHERE resource_kind = ? AND resource_id = ?`,
		baselineClock().UnixMilli(), string(core.ResourceSkill), "draft")
	require.NoError(t, err)

	readings, err = reader.ReadOverview(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, readings.Skills, "撤回的指针不得计入概览")
}

// TestRunResources_ProviderIsAuditedPinnedView（人工 review ② 缺陷 2）：准入交给运行时的
// Provider 必须是**带两阶段审计的固定视图**——不是目录里的裸协议客户端。固定视图只认
// 自己那一条名字：别的名字在预写之前拒绝（零审计行）；自己的名字照常预写 + 回填。
// 逆改点：runtime.providers 放回裸客户端 → 第一个断言红（裸客户端对任何名字都照答，
// 且一行审计都不写）。
// The runtime hands out the audited pinned view, never the raw protocol client.
func TestRunResources_ProviderIsAuditedPinnedView(t *testing.T) {
	env := newRunResourcesEnv(t)
	ctx := context.Background()
	version := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", version.ID, true)

	resolved, err := env.resources.Resolve(ctx, agentEntry("alpha", nil, nil))
	require.NoError(t, err)
	t.Cleanup(func() { resolved.Runtime.Release() }) // 归还准入读锁租约
	require.NotNil(t, resolved.Runtime)
	client, ok := resolved.Runtime.Provider("alpha")
	require.True(t, ok)
	require.NotNil(t, client)

	read := openRead(t, env.dbPath)
	// 别的 provider 名：固定表里没有 → 预写之前拒绝（无审计行 = 确定没发出）。
	_, err = client.Chat(ctx, core.ChatRequest{Provider: "beta", RunID: "run-pin-1", Round: 1})
	require.ErrorIs(t, err, core.ErrProviderNotConfigured, "固定视图只认准入固定的 provider")
	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM llm_calls`), "拒绝发生在预写之前")

	// 自己的名字：调用发出并落两阶段审计（固定视图没有绕过 ProviderService）。
	_, err = client.Chat(ctx, core.ChatRequest{
		Provider:  "alpha",
		RunID:     "run-pin-2",
		SessionID: "sess-pin",
		Round:     1,
		Model:     "alpha-model",
		Messages:  []core.Message{{Role: core.RoleUser, Content: "在吗"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, countRows(t, read, `SELECT COUNT(*) FROM llm_calls`), "固定视图仍写两阶段审计")
	require.Equal(t, 1, countRows(t, read,
		`SELECT COUNT(*) FROM llm_calls WHERE provider = 'alpha' AND status = 'success'`),
		"审计行的 provider 与生效状态")
}

// ---- US-12 条件 ETag 与墓碑（FR-066 / FR-067 / FR-081，模块卡 R4 裁决 2026-10-23）----

// fixtureCredentialID 由属主派生一个稳定的 `cred_` ID；取模手法与 fixtureResourceUID
// 相同，载荷落在 Crockford 字符集内（I / L / O / U 不在集合里）。
// fixtureCredentialID derives a stable credential id for one owner.
func fixtureCredentialID(t *testing.T, owner string) string {
	t.Helper()
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // 与 core 的字符集一致
	sum := sha256.Sum256([]byte("cred\x00" + owner))
	payload := make([]byte, 26)
	for i := range payload {
		payload[i] = crockford[int(sum[i%len(sum)])%len(crockford)]
	}
	id := "cred_" + string(payload)
	require.NoError(t, core.ValidateCredentialID(id))
	return id
}

// seedCredential 为 `(kind, name)` 建第 1 个托管凭据版本；属主身份取资源 UID（不是名称）。
// seedCredential creates version 1 of a managed credential owned by the resource uid.
func (e *runResourcesEnv) seedCredential(t *testing.T, kind core.ResourceKind, name, baseURL string) string {
	t.Helper()
	return seedManagedCredential(t, e.credentials, e.opID, kind, name,
		fixtureResourceUID(t, kind, name), baseURL)
}

// seedManagedCredential 建第 1 个托管凭据版本；属主 UID 由调用方给出——夹具里既可能
// 来自 `fixtureResourceUID`，也可能来自仓储实际分配的指针身份（跨写面用例）。
// seedManagedCredential creates version 1 with a caller-supplied owner uid.
func seedManagedCredential(t *testing.T, store *storage.CredentialStore, opID string,
	kind core.ResourceKind, name string, uid core.ResourceUID, baseURL string,
) string {
	t.Helper()
	binding, ok := credentialBindingKind(kind)
	require.True(t, ok, "只有模型连接与通知渠道有托管凭据身份")
	credID := fixtureCredentialID(t, string(kind)+"/"+name)
	require.NoError(t, store.Create(context.Background(), core.CredentialRecord{
		ID:          credID,
		OwnerKind:   binding,
		OwnerUID:    uid,
		OwnerName:   name,
		KeyID:       "master/v1",
		OperationID: opID,
		CreatedAt:   baselineClock().UnixMilli(),
	}, core.CredentialVersionRecord{
		KeyID:         "master/v1",
		TargetBinding: core.TargetBinding{Kind: binding, Owner: uid, BaseURL: baseURL}.Hash(),
		Last4:         "0001",
		Nonce:         make([]byte, 12),
		Ciphertext:    []byte("sealed:" + name + ":v1"),
		OperationID:   opID,
	}))
	return credID
}

// TestResources_EtagIncludesScopeAndCredential —— FR-081（模块卡 R4 v3 裁决）：条件
// ETag 覆盖内容哈希、资源 UID、修订号、范围、属主与凭据段（凭据 ID / 当前版本 /
// 版本状态 / 撤销纪元 / 目标绑定），且**不得**等于 `content_hash` 本身；只换 Key
// （定义字节不变）必须换 ETag，换 Key 不得波及其他资源的 ETag；只读视图与写路径的
// 当前值必须同源（否则条件写永远 412）。
// 逆改点：ETag 退回纯内容哈希 → "只换 Key" 三次断言全红；漏掉 scope / revision →
// 对应单字段扰动断言红；读面另写一份组成 → 同源断言红；换 Key 波及其他资源 →
// beta 的恒定断言红。
func TestResources_EtagIncludesScopeAndCredential(t *testing.T) {
	t.Run("字段敏感度", func(t *testing.T) {
		uid := fixtureResourceUID(t, core.ResourceModelConnection, "alpha")
		otherUID := fixtureResourceUID(t, core.ResourceModelConnection, "beta")
		owner, err := core.NewAgentUID()
		require.NoError(t, err)
		otherOwner, err := core.NewAgentUID()
		require.NoError(t, err)

		basePointer := core.ResourcePointer{
			Kind: core.ResourceModelConnection, ResourceID: "alpha",
			ResourceUID: uid, PublishedVersionID: 7, Revision: 3, Scope: core.ScopeShared,
		}
		baseVersion := core.ResourceVersion{ContentHash: strings.Repeat("a", 64)}
		baseDigest := &credentialDigest{
			ID: "cred_" + strings.Repeat("0", 26), Version: 1,
			State: core.CredentialVersionActive, Epoch: 0,
			TargetBinding: strings.Repeat("b", 64),
		}
		exclusive := basePointer
		exclusive.Scope = core.ScopeExclusive
		exclusive.OwnerAgentUID = owner
		exclusiveOther := exclusive
		exclusiveOther.OwnerAgentUID = otherOwner

		baseline := resourceETag(basePointer, baseVersion, baseDigest)
		require.Regexp(t, `^"sha256:[0-9a-f]{64}"$`, baseline)
		require.Equal(t, baseline, resourceETag(basePointer, baseVersion, baseDigest), "同输入必须稳定")

		mutations := []struct {
			field  string
			render func() string
		}{
			{"content_hash", func() string {
				version := baseVersion
				version.ContentHash = strings.Repeat("c", 64)
				return resourceETag(basePointer, version, baseDigest)
			}},
			{"resource_uid", func() string {
				pointer := basePointer
				pointer.ResourceUID = otherUID
				return resourceETag(pointer, baseVersion, baseDigest)
			}},
			{"revision", func() string {
				pointer := basePointer
				pointer.Revision++
				return resourceETag(pointer, baseVersion, baseDigest)
			}},
			{"scope", func() string { return resourceETag(exclusive, baseVersion, baseDigest) }},
			{"owner_agent_uid", func() string { return resourceETag(exclusiveOther, baseVersion, baseDigest) }},
			{"credential_id", func() string {
				digest := *baseDigest
				digest.ID = "cred_" + strings.Repeat("1", 26)
				return resourceETag(basePointer, baseVersion, &digest)
			}},
			{"credential_version", func() string {
				digest := *baseDigest
				digest.Version = 2
				return resourceETag(basePointer, baseVersion, &digest)
			}},
			{"credential_version_state", func() string {
				digest := *baseDigest
				digest.State = core.CredentialVersionSuperseded
				return resourceETag(basePointer, baseVersion, &digest)
			}},
			{"revocation_epoch", func() string {
				digest := *baseDigest
				digest.Epoch = 1
				return resourceETag(basePointer, baseVersion, &digest)
			}},
			{"target_binding", func() string {
				digest := *baseDigest
				digest.TargetBinding = strings.Repeat("d", 64)
				return resourceETag(basePointer, baseVersion, &digest)
			}},
			{"凭据段整体缺失（技能 / MCP / env 模式）", func() string {
				return resourceETag(basePointer, baseVersion, nil)
			}},
		}
		for _, mutation := range mutations {
			require.NotEqual(t, baseline, mutation.render(), "字段 %s 单独变化必须换 ETag", mutation.field)
		}
	})

	t.Run("只换 Key 必须换 ETag 且不影响其他资源", func(t *testing.T) {
		ctx := context.Background()
		env := newRunResourcesEnv(t)
		alpha := env.seedVersion(t, core.ResourceModelConnection, "alpha",
			providerDefinition("alpha", "https://a.example", "${ALPHA_KEY}", "alpha-model"), "", "")
		env.publish(t, core.ResourceModelConnection, "alpha", alpha.ID, true)
		beta := env.seedVersion(t, core.ResourceModelConnection, "beta",
			providerDefinition("beta", "https://b.example", "${BETA_KEY}", "beta-model"), "", "")
		env.publish(t, core.ResourceModelConnection, "beta", beta.ID, true)

		admin := NewResourceAdmin(ResourceAdminDeps{
			Store:       env.versions,
			Operations:  env.ops,
			Gate:        env.gate,
			Credentials: env.credentials,
			Now:         baselineClock,
		})
		readETag := func(name string) string {
			t.Helper()
			view, err := env.views.ModelConnection(ctx, name)
			require.NoError(t, err)
			current, ok := admin.CurrentETag(ctx, core.ResourceModelConnection, name)
			require.True(t, ok, "写路径必须能取到当前条件值")
			require.Equal(t, current, view.ETag, "只读视图与写路径必须同源")
			return view.ETag
		}
		publishedHash := func() string {
			t.Helper()
			version, err := env.versions.VersionByID(ctx, alpha.ID)
			require.NoError(t, err)
			return version.ContentHash
		}

		initial := readETag("alpha")
		betaInitial := readETag("beta")
		require.Regexp(t, `^"sha256:[0-9a-f]{64}"$`, initial)
		require.NotEqual(t, `"sha256:`+alpha.ContentHash+`"`, initial, "条件 ETag 不得等于纯内容哈希")

		// 1) 范围转换：定义字节不变、revision +1、scope 与属主写入 → ETag 必须变。
		// 属主必须能在生命周期索引里反查到名字——生产路径的范围转换先按名字解析出
		// UID（`ScopeService.resolveOwner`），读面按同一前提反查，缺行即 fail-closed。
		owner, err := core.NewAgentUID()
		require.NoError(t, err)
		require.NoError(t, env.versions.PutLifecycleIndex(ctx, core.LifecycleRecord{
			SchemaVersion: 2, Agent: "alpha-owner", AgentUID: owner,
			State: core.AgentDisabled, Generation: 1,
			UpdatedAt: baselineClock().UnixMilli(), OperationID: env.opID,
		}, true, ""))
		_, err = env.versions.ConvertScope(ctx, storage.ScopeConversion{
			Kind:          core.ResourceModelConnection,
			Name:          "alpha",
			ExpectUID:     fixtureResourceUID(t, core.ResourceModelConnection, "alpha"),
			Target:        core.ScopeExclusive,
			OwnerAgentUID: owner,
			OperationID:   env.opID,
			At:            baselineClock(),
		})
		require.NoError(t, err)
		afterScope := readETag("alpha")
		require.NotEqual(t, initial, afterScope, "范围与属主写入必须换 ETag")
		require.Equal(t, alpha.ContentHash, publishedHash(), "范围转换不改内容字节")

		// 2) 建托管凭据（env → managed）：内容字节完全不变，只有凭据段出现 → ETag 必须变。
		credID := env.seedCredential(t, core.ResourceModelConnection, "alpha", "https://a.example")
		afterCreate := readETag("alpha")
		require.NotEqual(t, afterScope, afterCreate, "建立凭据身份必须换 ETag")
		require.Equal(t, alpha.ContentHash, publishedHash())

		// 3) 换 Key = 追加版本：定义字节仍然不变 → ETag 必须再变（纯内容哈希发现不了）。
		require.NoError(t, env.credentials.AppendVersion(ctx, credID, "master/v1", env.opID,
			core.CredentialVersionRecord{
				KeyID: "master/v1",
				TargetBinding: core.TargetBinding{
					Kind:    core.BindingModelConnection,
					Owner:   fixtureResourceUID(t, core.ResourceModelConnection, "alpha"),
					BaseURL: "https://a.example",
				}.Hash(),
				Last4:       "0002",
				Nonce:       make([]byte, 12),
				Ciphertext:  []byte("sealed:alpha:v2"),
				OperationID: env.opID,
			}, baselineClock()))
		afterRotate := readETag("alpha")
		require.NotEqual(t, afterCreate, afterRotate, "只换 Key（内容不变）必须换 ETag")
		require.Equal(t, alpha.ContentHash, publishedHash())

		// 4) 撤销：版本状态 revoked + 撤销纪元 +1 → ETag 必须再变。
		require.NoError(t, env.credentials.Revoke(ctx, credID, env.opID, baselineClock()))
		afterRevoke := readETag("alpha")
		require.NotEqual(t, afterRotate, afterRevoke, "撤销必须换 ETag")

		require.Equal(t, betaInitial, readETag("beta"), "全程不得波及其他资源的 ETag")
	})
}

// TestResources_EtagKeyOnlyChange —— FR-081 的单点判据：只换 Key（追加第 2 个凭据版本）
// 时已发布内容哈希逐字节不变，资源 ETag 必须变化，且只读视图与写路径始终同源。
// 逆改点：ETag 退回纯内容哈希 → "换 Key 必须换 ETag" 断言红；读面另算一份 → 同源断言红。
func TestResources_EtagKeyOnlyChange(t *testing.T) {
	ctx := context.Background()
	env := newRunResourcesEnv(t)
	published := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "https://a.example", "${ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", published.ID, true)
	credID := env.seedCredential(t, core.ResourceModelConnection, "alpha", "https://a.example")

	admin := NewResourceAdmin(ResourceAdminDeps{
		Store:       env.versions,
		Operations:  env.ops,
		Gate:        env.gate,
		Credentials: env.credentials,
		Now:         baselineClock,
	})
	readETag := func() string {
		t.Helper()
		view, err := env.views.ModelConnection(ctx, "alpha")
		require.NoError(t, err)
		current, ok := admin.CurrentETag(ctx, core.ResourceModelConnection, "alpha")
		require.True(t, ok, "写路径必须能取到当前条件值")
		require.Equal(t, current, view.ETag, "只读视图与写路径必须同源")
		return view.ETag
	}

	before := readETag()
	require.Regexp(t, `^"sha256:[0-9a-f]{64}"$`, before)
	require.NotEqual(t, `"sha256:`+published.ContentHash+`"`, before, "条件 ETag 不得等于纯内容哈希")

	// 只换 Key = 同一凭据追加第 2 个版本；定义文件与已发布版本字节完全不动。
	require.NoError(t, env.credentials.AppendVersion(ctx, credID, "master/v1", env.opID,
		core.CredentialVersionRecord{
			KeyID: "master/v1",
			TargetBinding: core.TargetBinding{
				Kind:    core.BindingModelConnection,
				Owner:   fixtureResourceUID(t, core.ResourceModelConnection, "alpha"),
				BaseURL: "https://a.example",
			}.Hash(),
			Last4:       "0002",
			Nonce:       make([]byte, 12),
			Ciphertext:  []byte("sealed:alpha:v2"),
			OperationID: env.opID,
		}, baselineClock()))
	stored, err := env.versions.VersionByID(ctx, published.ID)
	require.NoError(t, err)
	require.Equal(t, published.ContentHash, stored.ContentHash, "换 Key 不得改已发布内容")

	after := readETag()
	require.NotEqual(t, before, after, "只换 Key（内容字节不变）必须换 ETag")
}

// TestResources_KeyChangeDoesNotBreakOtherAgentSave —— 反向边界：凭据段进资源 ETag
// 只影响**该资源**的条件写，不得把改动扩散到 Agent 正文保存。换 Key 之后，另一个
// Agent 用换 Key 之前读到的集合 ETag 提交自己的定义文件必须成功（不 412）。
// 逆改点：把凭据状态混进 Agent 集合 ETag → 提交断言红；资源 ETag 不随 Key 变化 →
// 前半段断言红（两半互为对照，任一方向的"顺手统一"都会被抓到）。
func TestResources_KeyChangeDoesNotBreakOtherAgentSave(t *testing.T) {
	ctx := context.Background()
	env := newAgentAdminEnv(t)
	writeAdminAgent(t, env.root, "writer")

	// 同库同工作区：已发布模型连接 + 托管凭据（属主身份取仓储实际分配的指针 UID）。
	published := env.seedModel(t, "alpha")
	opID := env.seedOperation(t)
	credentials := storage.NewCredentialStore(env.db)
	credID := seedManagedCredential(t, credentials, opID, core.ResourceModelConnection,
		"alpha", published.ResourceUID, "https://api.example.com")
	admin := NewResourceAdmin(ResourceAdminDeps{
		Store:       env.versions,
		Operations:  env.ops,
		Credentials: credentials,
		Now:         baselineClock,
	})

	files, err := env.admin.Files(ctx, "writer")
	require.NoError(t, err)
	resourceBefore, ok := admin.CurrentETag(ctx, core.ResourceModelConnection, "alpha")
	require.True(t, ok)

	// 换 Key：只追加凭据版本，模型连接的定义与发布指针都不动。
	require.NoError(t, credentials.AppendVersion(ctx, credID, "master/v1", opID,
		core.CredentialVersionRecord{
			KeyID: "master/v1",
			TargetBinding: core.TargetBinding{
				Kind:    core.BindingModelConnection,
				Owner:   published.ResourceUID,
				BaseURL: "https://api.example.com",
			}.Hash(),
			Last4:       "0002",
			Nonce:       make([]byte, 12),
			Ciphertext:  []byte("sealed:alpha:v2"),
			OperationID: opID,
		}, baselineClock()))

	resourceAfter, ok := admin.CurrentETag(ctx, core.ResourceModelConnection, "alpha")
	require.True(t, ok)
	require.NotEqual(t, resourceBefore, resourceAfter, "换 Key 必须换该资源的 ETag")

	// Agent 集合 ETag 不含资源状态：换 Key 前后必须完全相同，旧 ETag 提交不得 412。
	refreshed, err := env.admin.Files(ctx, "writer")
	require.NoError(t, err)
	require.Equal(t, files.ETag, refreshed.ETag, "资源凭据变化不得影响 Agent 集合 ETag")

	updated := authorAgentMD("writer", "alpha", "alpha-model", nil, nil) + "\n额外一行\n"
	view, err := env.admin.SubmitFiles(ctx, consoleActor(), "writer", files.ETag,
		[]core.AuthorFile{{Path: "AGENT.md", Content: updated}})
	require.NoError(t, err, "换 Key 之后，别处 Agent 用换 Key 前的集合 ETag 提交必须成功")
	require.NotEqual(t, files.ETag, view.ETag)
}

// TestResources_TombstoneOnDelete —— FR-066 / FR-067 / SC-017（后者取代 SC-005
// 后半句）：删除只打墓碑——按名不可见，但指针行、版本行与删除事实全部保留；同名重建
// 是**新 UID**、版本号从 1 重新计，旧 UID 的版本不被接管，只有显式按 UID 才查得到；
// 指针行的 revision 跨代继续递增（ETag 因此不会与删除前相同）。
// 本节直接调用仓储，因此只断言仓储写下的指针墓碑；身份表的 `deleted` 行由管理写
// 路径在同一动作里写（见 T128），不在这里伪造。
// 逆改点：删除物理删行 / 按名仍可见 / 重建沿用旧 UID（版本号从 2 起）/ 旧 UID 版本
// 混入新身份 → 各自断言红。
func TestResources_TombstoneOnDelete(t *testing.T) {
	ctx := context.Background()
	env := newRunResourcesEnv(t)

	version := env.seedVersion(t, core.ResourceSkill, "pdf", "",
		skillManifest("SKILL.md"), ".basalt/versions/skills/pdf/1")
	env.publish(t, core.ResourceSkill, "pdf", version.ID, true)
	before, err := env.views.Skill(ctx, "pdf")
	require.NoError(t, err)
	oldUID := fixtureResourceUID(t, core.ResourceSkill, "pdf")

	require.NoError(t, env.versions.MarkDeleted(ctx, core.ResourceSkill, "pdf", env.opID, baselineClock()))

	// 墓碑：按名不可见（单查与列表同口径），指针行与版本行都还在。
	_, err = env.views.Skill(ctx, "pdf")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	skills, err := env.views.Skills(ctx)
	require.NoError(t, err)
	require.Empty(t, skills, "已删除资源不得出现在列表里")
	pointer, err := env.versions.Pointer(ctx, core.ResourceSkill, "pdf")
	require.NoError(t, err)
	require.NotNil(t, pointer.DeletedAt, "删除只打墓碑，不物理删指针行")
	require.Equal(t, oldUID, pointer.ResourceUID)
	revisionAfterDelete := pointer.Revision
	historical, err := env.versions.VersionsByUID(ctx, core.ResourceSkill, oldUID)
	require.NoError(t, err)
	require.Len(t, historical, 1, "版本历史必须保留")
	require.Equal(t, version.ID, historical[0].ID)

	// 同名重建 = 新 UID：版本号从 1 重新计，墓碑清除，revision 跨代继续递增。
	newUID := fixtureResourceUID(t, core.ResourceSkill, "pdf#rebuild")
	rebuilt, err := env.versions.PublishWithVersionState(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceID:   "pdf",
		ResourceUID:  newUID,
		ContentHash:  sha256Hex(t, []byte("重建后的技能正文")),
		ManifestJSON: skillManifest("SKILL.md", "references/guide.md"),
		StoragePath:  ".basalt/versions/skills/pdf/2",
		CreatedAt:    baselineClock().UnixMilli(),
		OperationID:  env.opID,
	}, true, env.opID, baselineClock())
	require.NoError(t, err)
	require.Equal(t, 1, rebuilt.VersionNo, "新实例的版本号从 1 重新计（SC-017）")

	after, err := env.views.Skill(ctx, "pdf")
	require.NoError(t, err, "重建后按名可见")
	require.NotEqual(t, before.ETag, after.ETag, "身份换代必须换 ETag")

	pointer, err = env.versions.Pointer(ctx, core.ResourceSkill, "pdf")
	require.NoError(t, err)
	require.Nil(t, pointer.DeletedAt, "同名重建清除墓碑")
	require.Equal(t, newUID, pointer.ResourceUID)
	require.Greater(t, pointer.Revision, revisionAfterDelete, "指针行的修订号跨代继续递增")

	// 按 UID 的历史彼此隔离：旧 UID 仍只有一条，新 UID 也只有一条。
	historical, err = env.versions.VersionsByUID(ctx, core.ResourceSkill, oldUID)
	require.NoError(t, err)
	require.Len(t, historical, 1, "旧 UID 的版本不得被新身份接管")
	current, err := env.versions.VersionsByUID(ctx, core.ResourceSkill, newUID)
	require.NoError(t, err)
	require.Len(t, current, 1)
	require.Equal(t, rebuilt.ID, current[0].ID)
}
