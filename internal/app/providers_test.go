package app

// 本文件是 `internal/app/providers.go` 的验收面：US-3 起的显式映射（宪法 III）在这里判卷，
// US-12 T017 追加八条「目录只来自已发布版本」（约束 16 / 图 A ⑥）：
//
//	T1 `TestProviderCatalog_PublishedNamesOnly`——键集逐字 = 已发布条目名集，构造零请求
//	T2 `TestProviderCatalog_OverrideNoRealClient`——override 时同一替身、不建真实客户端、零请求
//	T3 `TestProviderCatalog_OverridePerPublishedName`——每个已发布名各一条，无通配默认键
//	T4 `TestProviderCatalog_ZeroPublishedBuildsEmpty`——零已发布版本建空表（非 nil）
//	T5 `TestProviderCatalog_BuildFailureKeepsOld`——重建失败保留旧表（原子持有者）
//	T6 `TestProviderCatalog_MissingEnvFailsClosed`——凭证占位缺失 / 为空失败关闭且不回显值
//	T7 `TestProviderCatalog_DefinitionNameMismatchRejected`——条目名与资源标识不一致拒绝
//	T8 `TestProviderCatalog_MultipleEntriesRejected`——单资源版本必须恰含一个条目
//
// 全部离线：假 OpenAI 服务只用于判"构造期没有请求"，不产生任何真实网络调用。

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	// 判卷侧要走裸连接读库（与 internal/cli/status_test.go 同一先例）：驱动经副作用注册。
	_ "modernc.org/sqlite"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/testutil"
)

// providersConfig 造一份装配入参：条目逐条显式，名称用作映射表的键。
// providersConfig builds one assembly input: entries are explicit and their names key the map.
func providersConfig(entries ...cli.ProviderConfig) *cli.Config {
	return &cli.Config{Providers: entries}
}

// TestNewProviderClients_ExplicitMapMatchesConfig —— 宪法 III：映射键集**逐字** = 配置条目名集；
// 无"默认供应商"、无遍历推断留下的额外键（多一个键就是一个隐式兜底）。
func TestNewProviderClients_ExplicitMapMatchesConfig(t *testing.T) {
	cfg := providersConfig(
		cli.ProviderConfig{Name: "alpha", BaseURL: "https://alpha.example.com/v1", DefaultModel: "m-alpha"},
		cli.ProviderConfig{Name: "beta", BaseURL: "https://beta.example.com", DefaultModel: "m-beta"},
		cli.ProviderConfig{Name: "gamma", BaseURL: "http://127.0.0.1:8080/v1"},
	)

	clients, models, err := newProviderClients(cfg)
	require.NoError(t, err)

	require.Len(t, clients, len(cfg.Providers), "键数 = 条目数（不多不少）")
	for _, entry := range cfg.Providers {
		require.Contains(t, clients, entry.Name, "每个条目名都要有客户端")
		require.Contains(t, models, entry.Name, "每个条目名都要有缺省模型")
	}
	require.Equal(t, "m-alpha", models["alpha"])
	require.Equal(t, "m-beta", models["beta"])
	require.Equal(t, "", models["gamma"], "未配置缺省模型 = 空（不推断、不继承别家）")
	require.NotContains(t, clients, "", "不得出现空键（'默认供应商'的出入口）")
}

// TestNewProviderClients_InvalidBaseURLRejected —— 构造期防御校验（§2.4 裁决注 9）：
// 非法 `base_url` → 装配失败并点名 `providers[i]`；文案不回显原 URL 与其中的值。
func TestNewProviderClients_InvalidBaseURLRejected(t *testing.T) {
	const raw = "https://svc-acct:secret-7f@api.example.com/v1"
	cfg := providersConfig(
		cli.ProviderConfig{Name: "ok", BaseURL: "https://ok.example.com/v1", DefaultModel: "m"},
		cli.ProviderConfig{Name: "bad", BaseURL: raw, DefaultModel: "m"},
	)

	clients, models, err := newProviderClients(cfg)

	require.Error(t, err, "含 userinfo 的地址必须拒绝（不静默裁剪）")
	require.Contains(t, err.Error(), "providers[1]", "必须点名配置条目下标")
	require.Contains(t, err.Error(), "base_url", "必须点名键")
	require.Contains(t, err.Error(), "userinfo", "必须点出不支持的成分")
	require.NotContains(t, err.Error(), raw, "不得回显原 URL")
	require.NotContains(t, err.Error(), "secret-7f", "不得回显其中的值（userinfo 可能就是凭证）")
	require.Nil(t, clients, "任一构造失败即整体失败——不返回半张映射表")
	require.Nil(t, models)
}

// TestNewProviderService_ConfigToAuditRowChain —— 端到端（开发操作 §4.2.2 验收 Demo 的仓内可判面）：
// 配置 → 显式映射 → 调用 → `llm_calls` 落库。走真客户端 + 假服务 + 真库，中途不换零件。
func TestNewProviderService_ConfigToAuditRowChain(t *testing.T) {
	ctx := context.Background()
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		Body: `{"choices":[{"message":{"role":"assistant","content":"pong"}}],` +
			`"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`,
	})

	path := filepath.Join(t.TempDir(), "basalt.db")
	db, err := storage.OpenWritable(ctx, path, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	cfg := providersConfig(cli.ProviderConfig{
		Name: "demo", BaseURL: srv.URL(), DefaultModel: "demo-model",
	})
	svc, err := newProviderService(cfg, storage.NewLLMCallRepo(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)

	resp, err := svc.Chat(ctx, core.ChatRequest{
		Provider: "demo", RunID: "run-chain", SessionID: "sess-chain",
		Round: 1, Messages: []core.Message{{Role: "user", Content: "ping"}},
	})
	require.NoError(t, err)
	require.Equal(t, "pong", resp.Content)

	read, err := sql.Open("sqlite", filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, read.Close()) })

	var (
		status, model, runID string
		prompt, completion   int
	)
	require.NoError(t, read.QueryRowContext(ctx,
		`SELECT status, model, run_id, prompt_tokens, completion_tokens FROM llm_calls`).
		Scan(&status, &model, &runID, &prompt, &completion))

	require.Equal(t, string(core.LLMCallSuccess), status)
	require.Equal(t, "demo-model", model, "缺省模型来自配置条目")
	require.Equal(t, "run-chain", runID, "关联标识随请求落到审计行")
	require.Equal(t, 4, prompt)
	require.Equal(t, 6, completion)

	// 配置里的地址是假服务的回环地址——出站协议由假服务那一侧判卷（§3.2 的 client 用例）。
	require.Len(t, srv.Requests(), 1, "一次调用 = 一次 POST")
	require.Equal(t, "demo-model", srv.Requests()[0]["model"])
}

// providerCatalogFixture 建一个带资源版本表的临时库，并预写一条 pending 管理操作行
// （`resource_versions.operation_id` 是非空外键，种子版本复用它）。
// providerCatalogFixture seeds a temp database with one pending operation reused as the
// non-null foreign key of every seeded version.
type providerCatalogFixture struct {
	store *storage.ResourceStore
	opID  string
}

func newProviderCatalogFixture(t *testing.T) *providerCatalogFixture {
	t.Helper()

	ctx := context.Background()
	db, err := storage.OpenWritable(ctx, filepath.Join(t.TempDir(), "basalt.db"),
		storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	ops := storage.NewManagementStore(db)
	opID := core.NewOperationID(time.Now())
	require.NoError(t, ops.BeginOperation(ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       "system.baseline",
		TargetKind:   "system",
		TargetID:     "system",
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       "test",
		Status:       core.OperationPending,
		CreatedAt:    time.Now(),
	}))
	return &providerCatalogFixture{store: storage.NewResourceStore(db), opID: opID}
}

// publish 幂等写入一个 model_connection 版本并把发布指针指过去（内容哈希由定义原文派生）。
// publish seeds one version row and points the publish pointer at it.
func (f *providerCatalogFixture) publish(t *testing.T, name, definition string) {
	t.Helper()

	ctx := context.Background()
	version, _, err := f.store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    fixtureResourceUID(t, core.ResourceModelConnection, name),
		ResourceID:     name,
		ContentHash:    sha256Hex(t, []byte(definition)),
		DefinitionYAML: definition,
		CreatedAt:      time.Now().UnixMilli(),
		OperationID:    f.opID,
	})
	require.NoError(t, err)
	require.NoError(t, f.store.Publish(ctx, core.ResourceModelConnection, name,
		version.ID, true, f.opID, time.Now()))
}

// publishedProviderBlock 是**基线**（US-11）写入 `definition_yaml` 的形态：序列指示符已被
// 剥离的条目映射原文。目录构建必须兼容这一形态（存量库里的 v1 就是它）。
// publishedProviderBlock renders the baseline's dash-stripped entry mapping, the shape
// already stored by US-11 and therefore mandatory for the catalog to accept.
func publishedProviderBlock(name, baseURL, apiKey, model string) string {
	return "name: " + name + "\n" +
		"base_url: " + baseURL + "\n" +
		"api_key: " + apiKey + "\n" +
		"default_model: " + model + "\n"
}

// publishedProviderItem 是管理写路径（本 US 发布）存储的**块式列表片段**形态：单条目以
// `- ` 开头；多条拼接即为多条目列表。
// publishedProviderItem renders the block-list excerpt written by the management path.
func publishedProviderItem(name, baseURL, apiKey, model string) string {
	return "- name: " + name + "\n" +
		"  base_url: " + baseURL + "\n" +
		"  api_key: " + apiKey + "\n" +
		"  default_model: " + model + "\n"
}

// countingProviderClient 是零网络替身：只数调用次数，用来判"构造期未调用 / 未请求"。
// countingProviderClient counts calls without any network activity.
type countingProviderClient struct {
	calls int
}

func (c *countingProviderClient) Chat(context.Context, core.ChatRequest) (core.ChatResponse, error) {
	c.calls++
	return core.ChatResponse{}, nil
}

// TestProviderCatalog_PublishedNamesOnly —— 宪法 III / 约束 16：键集**逐字**等于已发布
// 条目名集，缺省模型取条目自身；构造目录**不发出任何请求**（客户端构造不是连接）。
func TestProviderCatalog_PublishedNamesOnly(t *testing.T) {
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{})
	t.Setenv("PROVIDER_CATALOG_ALPHA_KEY", "alpha-test-sentinel")
	t.Setenv("PROVIDER_CATALOG_BETA_KEY", "beta-test-sentinel")

	f := newProviderCatalogFixture(t)
	f.publish(t, "alpha", publishedProviderBlock("alpha", srv.URL(),
		"${PROVIDER_CATALOG_ALPHA_KEY}", "alpha-model"))
	f.publish(t, "beta", publishedProviderBlock("beta", "https://beta.example.com/v1",
		"${PROVIDER_CATALOG_BETA_KEY}", "beta-model"))

	catalog, err := newProviderCatalogFromPublished(context.Background(), f.store, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	require.NoError(t, err)
	require.Len(t, catalog.clients, 2)
	require.Contains(t, catalog.clients, "alpha")
	require.Contains(t, catalog.clients, "beta")
	require.NotContains(t, catalog.clients, "", "不得出现空键（'默认供应商'的出入口）")
	require.Equal(t, "alpha-model", catalog.models["alpha"])
	require.Equal(t, "beta-model", catalog.models["beta"])
	require.NotSame(t, catalog.clients["alpha"], catalog.clients["beta"],
		"每个已发布名各得一个客户端实例，不共享、不推断")
	require.Empty(t, srv.Requests(), "构造目录不得发出任何请求")
}

// TestProviderCatalog_OverrideNoRealClient —— 评测注入三保证之一：override 非 nil 时按已发布名
// 建表但仍**不构造真实客户端、不请求**，连凭证占位都不解析（变量未设置也不报错）。
func TestProviderCatalog_OverrideNoRealClient(t *testing.T) {
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{})
	f := newProviderCatalogFixture(t)
	f.publish(t, "alpha", publishedProviderBlock("alpha", srv.URL(),
		"${PROVIDER_CATALOG_OVERRIDE_KEY_ABSENT}", "alpha-model"))

	override := &countingProviderClient{}
	catalog, err := newProviderCatalogFromPublished(context.Background(), f.store, override, nil, nil)
	require.NoError(t, err, "override 路径不解析环境变量")
	require.Same(t, override, catalog.clients["alpha"])
	require.Zero(t, override.calls)
	require.Empty(t, srv.Requests(), "替身注入不得触发真实请求")
}

// TestProviderCatalog_OverridePerPublishedName —— 每个已发布名各一条替身；没有通配默认键，
// 未发布的名称绝不进表（否则路由查表就失去"名字 → 客户端"的检验）。
func TestProviderCatalog_OverridePerPublishedName(t *testing.T) {
	f := newProviderCatalogFixture(t)
	f.publish(t, "alpha", publishedProviderBlock("alpha", "https://alpha.example.com/v1",
		"${PROVIDER_CATALOG_ALPHA_KEY}", "alpha-model"))
	f.publish(t, "beta", publishedProviderBlock("beta", "https://beta.example.com/v1",
		"${PROVIDER_CATALOG_BETA_KEY}", "beta-model"))

	override := &countingProviderClient{}
	catalog, err := newProviderCatalogFromPublished(context.Background(), f.store, override, nil, nil)
	require.NoError(t, err)
	require.Len(t, catalog.clients, 2)
	require.Same(t, override, catalog.clients["alpha"])
	require.Same(t, override, catalog.clients["beta"])
	require.NotContains(t, catalog.clients, "")
	require.NotContains(t, catalog.clients, "gamma", "未发布的 Provider 名不得出现在表中")
	require.Len(t, catalog.models, 2)
}

// TestProviderCatalog_ZeroPublishedBuildsEmpty —— 零已发布版本 → 空表（非 nil）：调用方不必
// 区分 "nil 表" 与 "空表"，路由查表对未发布名一律未命中。
func TestProviderCatalog_ZeroPublishedBuildsEmpty(t *testing.T) {
	f := newProviderCatalogFixture(t)

	catalog, err := newProviderCatalogFromPublished(context.Background(), f.store, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, catalog.clients)
	require.NotNil(t, catalog.models)
	require.Empty(t, catalog.clients)
	require.Empty(t, catalog.models)
}

// TestProviderCatalog_BuildFailureKeepsOld —— 约束 16：重建失败**保留旧表**。持有者只在构建
// 成功后原子替换；损坏的定义（明文 api_key）让候选构建失败，旧客户端继续服务。
func TestProviderCatalog_BuildFailureKeepsOld(t *testing.T) {
	f := newProviderCatalogFixture(t)
	oldClient := &countingProviderClient{}
	old := &providerCatalog{
		clients: map[string]core.ProviderClient{"alpha": oldClient},
		models:  map[string]string{"alpha": "old-model"},
	}
	holder, err := newProviderCatalogStore(old)
	require.NoError(t, err)

	// 明文 api_key 在候选校验处即被拒（约束 7），构建不可能产出新表。
	f.publish(t, "alpha", publishedProviderBlock("alpha", "https://alpha.example.com/v1",
		"plaintext-not-a-placeholder", "new-model"))

	err = holder.rebuild(context.Background(), f.store, nil, nil, nil)
	require.Error(t, err)
	require.Same(t, old, holder.Load(), "构建失败必须保留旧表")
}

// TestProviderCatalog_MissingEnvFailsClosed —— 凭证占位缺失 / 为空一律失败关闭（不返回半张
// 表），错误点名资源与变量名，**不回显** base_url 或值。
func TestProviderCatalog_MissingEnvFailsClosed(t *testing.T) {
	t.Setenv("PROVIDER_CATALOG_MISSING_KEY", "")

	f := newProviderCatalogFixture(t)
	f.publish(t, "alpha", publishedProviderBlock("alpha", "https://alpha.example.com/v1",
		"${PROVIDER_CATALOG_MISSING_KEY}", "alpha-model"))

	catalog, err := newProviderCatalogFromPublished(context.Background(), f.store, nil, nil, nil)
	require.Error(t, err)
	require.Nil(t, catalog, "失败不得返回半张映射表")
	require.Contains(t, err.Error(), "alpha")
	require.Contains(t, err.Error(), "PROVIDER_CATALOG_MISSING_KEY")
	require.NotContains(t, err.Error(), "https://", "不得回显 base_url")
}

// TestProviderCatalog_DefinitionNameMismatchRejected —— 版本归属与条目名必须与资源标识一致：
// 存储与定义分叉时启动失败关闭，不把错名的客户端挂到资源名下。
func TestProviderCatalog_DefinitionNameMismatchRejected(t *testing.T) {
	t.Setenv("PROVIDER_CATALOG_MISMATCH_KEY", "mismatch-test-sentinel")

	f := newProviderCatalogFixture(t)
	f.publish(t, "alpha", publishedProviderItem("beta", "https://beta.example.com/v1",
		"${PROVIDER_CATALOG_MISMATCH_KEY}", "beta-model"))

	_, err := newProviderCatalogFromPublished(context.Background(), f.store, nil, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "不一致")
}

// TestProviderCatalog_MultipleEntriesRejected —— 一个 `(kind, resource_id)` 的版本只能承载
// 恰一个条目；多条目说明发布路径越权改写了别的块，按失败关闭处理。
func TestProviderCatalog_MultipleEntriesRejected(t *testing.T) {
	t.Setenv("PROVIDER_CATALOG_MULTI_KEY", "multi-test-sentinel")

	f := newProviderCatalogFixture(t)
	definition := publishedProviderItem("alpha", "https://alpha.example.com/v1",
		"${PROVIDER_CATALOG_MULTI_KEY}", "alpha-model") +
		publishedProviderItem("beta", "https://beta.example.com/v1",
			"${PROVIDER_CATALOG_MULTI_KEY}", "beta-model")
	f.publish(t, "alpha", definition)

	_, err := newProviderCatalogFromPublished(context.Background(), f.store, nil, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "恰含一个条目")
}

// catalogCredentialSpy 是 `managedCredentialSource` 的计数替身：记录收到的发布版本引用，
// 可注入固定失败。目录构建对每个托管条目只应调用它**一次**，且 override 注入时一次都不调。
// catalogCredentialSpy counts resolution calls and records the pinned version references.
type catalogCredentialSpy struct {
	inner managedCredentialSource
	err   error
	refs  []core.ResourceVersionRef
}

func (s *catalogCredentialSpy) ResolveModelCredential(ctx context.Context,
	ref core.ResourceVersionRef,
) (string, error) {
	s.refs = append(s.refs, ref)
	if s.err != nil {
		return "", s.err
	}
	return s.inner.ResolveModelCredential(ctx, ref)
}

// TestProviders_ManagedCredentialSource —— T133 / 约束 3 + 16：`credential_mode: managed`
// 的已发布条目经 `managedCredentialSource` 按**发布版本**解析（准入固定的同一份引用），
// 键仍来自版本条目自身；评测注入时**不**构造真实客户端、**不**解密；没有解析器时失败
// 关闭并点名资源，不回显材料。逆改点：退回 env 解析 → 定义里没有 `api_key`，构建失败；
// override 路径仍调用解析面 → `injected.refs` 非空；缺解析器时静默给空 Key → 第三段断言红。
// TestProviders_ManagedCredentialSource pins the managed entry path and its two bypasses.
func TestProviders_ManagedCredentialSource(t *testing.T) {
	ctx := context.Background()
	f := newResolverFixture(t)
	const (
		name = "alpha"
		base = "https://api.example.com/v1"
	)
	version := f.seedManagedModelConnection(t, name, base)
	target, err := CredentialTargetBinding(core.BindingModelConnection, base, "")
	require.NoError(t, err)
	credID := f.seedSealedCredential(t, core.ResourceModelConnection, name, target,
		"sk-managed-catalog-plaintext")
	require.NotEmpty(t, credID)

	source := &catalogCredentialSpy{inner: f.resolver}
	catalog, err := newProviderCatalogFromPublished(ctx, f.versions, nil, nil, source)
	require.NoError(t, err, "托管条目必须经解析面取 Key，不需要任何环境变量")
	require.Len(t, catalog.clients, 1)
	require.Contains(t, catalog.clients, name)
	require.Equal(t, "alpha-model", catalog.models[name], "缺省模型仍取条目自身")
	require.Len(t, source.refs, 1, "每个托管条目恰解析一次")
	ref := source.refs[0]
	require.Equal(t, name, ref.Name)
	require.Equal(t, fixtureResourceUID(t, core.ResourceModelConnection, name), ref.ResourceUID)
	require.Equal(t, version.ID, ref.VersionID, "按发布版本解析，不按名称重新取指针")
	require.Equal(t, version.VersionNo, ref.VersionNo)
	require.Equal(t, version.ContentHash, ref.ContentHash)

	// 评测注入：override 路径不构造真实客户端，也**不**解密托管凭据。
	injected := &catalogCredentialSpy{err: errors.New("override 路径不得解析凭据")}
	override := &countingProviderClient{}
	catalog, err = newProviderCatalogFromPublished(ctx, f.versions, override, nil, injected)
	require.NoError(t, err)
	require.Same(t, override, catalog.clients[name])
	require.Empty(t, injected.refs, "override 注入不得触碰托管凭据")

	// 没有解析器：失败关闭，点名资源且不回显材料。
	_, err = newProviderCatalogFromPublished(ctx, f.versions, nil, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), name)
	require.Contains(t, err.Error(), "托管凭据")
	require.NotContains(t, err.Error(), "sk-managed-catalog-plaintext")
}

// TestProviders_RevokedCredentialKeepsOldTable —— T133 / 约束 16 + FR-078：撤销后重建目录
// 必须失败并**保留旧表**（旧客户端继续服务，不换半张表）；错误可追溯到资源与撤销类别，
// 不回显材料。逆改点：失败后仍替换目录 → `holder.Load()` 断言红；吞掉撤销错误 → 红。
// TestProviders_RevokedCredentialKeepsOldTable pins that a revoked credential leaves the
// previous catalog serving.
func TestProviders_RevokedCredentialKeepsOldTable(t *testing.T) {
	ctx := context.Background()
	f := newResolverFixture(t)
	const (
		name = "alpha"
		base = "https://api.example.com/v1"
	)
	f.seedManagedModelConnection(t, name, base)
	target, err := CredentialTargetBinding(core.BindingModelConnection, base, "")
	require.NoError(t, err)
	credID := f.seedSealedCredential(t, core.ResourceModelConnection, name, target,
		"sk-managed-revoked-plaintext")

	old, err := newProviderCatalogFromPublished(ctx, f.versions, nil, nil, f.resolver)
	require.NoError(t, err)
	holder, err := newProviderCatalogStore(old)
	require.NoError(t, err)
	oldClient := old.clients[name]

	require.NoError(t, f.credentials.Revoke(ctx, credID, f.opID, baselineClock()))

	err = holder.rebuild(ctx, f.versions, nil, nil, f.resolver)
	require.Error(t, err, "撤销后的托管凭据必须让重建失败")
	require.ErrorIs(t, err, core.ErrCredentialRevoked)
	require.Same(t, old, holder.Load(), "构建失败保留旧表")
	require.Same(t, oldClient, holder.Load().clients[name], "旧客户端继续服务")
	require.Contains(t, err.Error(), name)
	require.NotContains(t, err.Error(), "sk-managed-revoked-plaintext")
}
