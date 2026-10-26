package app

// 本文件是 `internal/app/resource_admin.go` 的验收面（颗粒度文档 §3.2.1：app 的
// resource_admin_test.go，32 例；删除 / 引用由 T040 落地、版本详情由 T042 落地，
// MCP 写面由 T044–T049 落地、技能写面由 T051 追加到同一文件）。当前覆盖模型连接的
// 写 / 发布 / 撤回 / 模型测试、引用与删除闸门、版本详情、MCP 与技能写面：
//
//	C1  `TestResourceAdmin_PublishFailureKeepsOldPointer`——发布事务失败保旧指针（约束 10，原样落地）
//	C2  `TestResourceAdmin_PublishBuildFailureKeepsOld`——运行时对象构建失败 → 400、文件与指针零变化、零操作行
//	C3  `TestResourceAdmin_DBFailureRestoresFile`——提交段失败 → 身份检查后恢复原字节、操作 failed 可查（约束 36）
//	C4  `TestResourceAdmin_NoFileWriteOnPreWriteFailure`——预写失败零写入（约束 1）
//	C5  `TestResourceAdmin_EveryWriteHasOperation`——每个写操作恰一行终态台账；412 不产生行（约束 1a）
//	W1  `TestModelConnection_UpsertPublishes`——新建即发布：版本 / 指针 / 目录 / 文件四处一致
//	W2  `TestModelConnection_UpdateInheritsAPIKey`——更新空 api_key 保留已发布占位
//	W3  `TestModelConnection_StaleETagZeroWrite`——If-Match 不匹配 → 412 + `current_etag`、零写入零操作行
//	W4  `TestModelConnection_MissingEnvZeroWrite`——占位缺失 → 400 点名变量、零写入、零操作行（约束 7）
//	W5  `TestModelConnection_DuplicateCreateWritesFailedOperation`——重名 → 409 + 一条 failed 行
//	W6  `TestResourceWrite_FileDivergedConflict`——文件被手工改动 → 409、文件不动、零操作行（约束 34）
//	W7  `TestResourceWrite_DivergenceCheckedInsideFileLock`——分歧闸门在文件锁内重读（约束 36）
//	P1  `TestPublish_RewritesDefinitionFile`——撤回把定义文件改写为来源版本字节
//	P2  `TestPublish_RevertCreatesNewVersion`——撤回只追加新版本，历史版本不改写（约束 9）
//	P3  `TestPublish_Idempotent`——不带版本发布且已发布 → 幂等成功、不新增版本
//	T1  `TestModelConnection_TestWritesOperation`——先写操作行、只发一次最小调用、结果脱敏（约束 50）
//	T2  `TestModelConnection_TestFailureIsSuccessOperation`——连接失败仍是 success + ok=false
//	T3  `TestModelConnection_TestTimeout504`——超时 → 504 分类、操作 failed + `TIMEOUT`
//	T4  `TestModelConnection_TestNotConfigured503`——凭证缺失 → 503、操作 failed + `NOT_CONFIGURED`
//	R1  `TestReferences_AgentAndRunUnion`——引用视图 = 注册表与归档扫描 ∪ 未释放运行引用（约束 14）
//	R2  `TestReferences_ActiveRunRefs`——活动运行引用来自未释放引用记录
//	R3  `TestReferences_DistinguishesReleasedRuns`——已释放的运行引用不再计入
//	R4  `TestReferences_DeletedResourceStillAnswerable`——已删除资源的引用面仍可查
//	D1  `TestResourceDelete_ReferencedByAgent`——被 Agent 引用 → 400 点名、不提供 force、零写入（约束 11）
//	D2  `TestResourceDelete_UnparseableAgentBlocks`——Agent 定义解析失败按可能引用处理
//	D3  `TestResourceDelete_ArchivedAgentBlocks`——归档 Agent 同样计入引用扫描
//	D4  `TestResourceDelete_ActiveRunRef`——未释放运行引用阻断删除
//	D5  `TestResourceDelete_NoNewVersion`——删除不追加版本、不改写历史
//	D6  `TestResourceRecreate_VersionContinues`——同名重建是新实例：新 UID、版本号从 1 重新计（函数名沿用 stage 固定名）
//	D7  `TestResourceDelete_RebuildFailureRestoresFile`——删除后运行时目录重建失败 → 恢复文件与指针
//	D8  `TestResourceDelete_SkipsTargetEnvResolution`——删除不解析无关段环境变量
//	D9  `TestResourceDelete_StaleETag412`——If-Match 不匹配 → 412 且零写入
//	D10 `TestResourceDelete_UnknownName404`——名字不存在 → 404、零写入
//	D11 `TestResourceDelete_UnsupportedKind`——类型不支持 → 400、零写入
//	D12 `TestResourceDelete_MCPStopFailureRestoresFile`——MCP 断开失败 → 恢复文件、指针不动、操作 failed
//	M1  `TestMCPCreate_PublishesPendingRestart`——新建即发布、applied 留空、待重启为真（约束 9）
//	M2  `TestMCP_StaleETagZeroWrite`——MCP If-Match 不匹配 → 412 零写入零操作行
//	M3  `TestMCPPublish_KeepsApplied`——撤回发布只推进 published，applied 保持旧版本（约束 9）
//	M4  `TestMCPServer_EditLegacyMultiWordRejected`——旧多词 command 可读不可写，严格候选指向 args（约束 47）
//	M5  `TestMCPTest_EndpointPolicyDeniedAudited`——测试先审计后连接、策略拒绝零连接器（约束 20 / 22）
//	M6  `TestMCPTest_EndpointTimeout504`——测试超时 → 504 分类、操作 failed + `TIMEOUT`
//	M7  `TestMCPTest_EndpointNoRunNoLLMCall`——测试不建 Run、不写 `llm_calls`、不动 applied
//	M8  `TestMCPTest_NoProcessOnPreWriteFailure`——预写失败零外部动作（约束 1 / 21）
//	M10 `TestMCPError_NoSecrets`——连接错误只收敛为固定短语，结果与审计不回显 URL / 凭证（约束 23）
//	V1  `TestResourceVersionDetail_Projection`——版本详情投影发布 / 应用指针与撤回来源（H2）
//	V2  `TestResourceVersionDetail_DeletedResourceReadable`——删除后历史版本详情仍可读
//	V3  `TestResourceVersionDetail_NotFound`——非正 / 不存在 → `core.ErrResourceNotFound`、零操作行
//	V4  `TestResourceVersionDetail_RedactionFailClosed`——未脱敏定义一律拒绝，错误不含敏感值
//	V5  `TestResourceVersionDetail_SkillManifest`——技能版本回文件清单、定义恒为空
//	V6  `TestResourceAdmin_Versions_Projection`——版本列表新 → 旧、指针状态与撤回来源逐行派生（T043）
//	V7  `TestResourceAdmin_Versions_DeletedResourceReadable`——删除后历史版本列表仍可读（T043）
//	V8  `TestResourceAdmin_Versions_NotFound`——指针行不存在 → `core.ErrResourceNotFound`、零操作行（T043）
//	V9  `TestResourceAdmin_Versions_InvalidInput`——未知类型 / 非法标识 → `InvalidInputError`（T043）
//	K1  `TestResourceAdmin_SkillCreatePublishes`——创建即发布 v1：工作副本 / 快照 / 指针 / 操作行四处一致（图 D）
//	K2  `TestResourceAdmin_SkillImportPublishes`——导入整棵目录：清单升序、快照逐字节、版本行只存清单
//	K3  `TestResourceAdmin_SkillUpdateFullReplacement`——更新是全量替换：未列出的文件消失、历史快照不改写（约束 25）
//	K4  `TestSkill_StaleETagZeroWrite`——If-Match 缺失 / 不匹配 → 412 + `current_etag`、零写入零操作行
//	K5  `TestSkillWrite_WorkCopyDivergedConflict`——工作副本被外部修改 / 缺失 → 409、零写入零操作行（图 D）
//	K6  `TestResourceAdmin_SkillDuplicateCreateZeroOperations`——同名创建 → 409、零操作行（图 D「不写操作行」）
//	K7  `TestResourceAdmin_SkillCommitFailureRollsBack`——提交段失败 → 恢复工作副本、删本次快照、操作 failed
//	K8  `TestResourceDelete_SkillArchivesWorkCopy`——删除把工作副本移入归档区、版本与历史不动（图 G）
//	K9  `TestResourceRecreate_SkillVersionContinues`——同名重建是新实例：新 UID、版本号从 1 重新计、删除标记清空（函数名沿用 stage 固定名）
//	K10 `TestResourceDelete_SkillReferencedByAgent`——被 Agent 引用 → 400 点名、工作副本保留、操作 failed
//	K11 `TestPublish_SkillRevertCreatesNewVersion`——撤回用旧版本字节追加新版本，历史快照不改写（约束 9 / 25）
//	K12 `TestPublish_SkillIdempotent`——不带版本发布且已发布 → 幂等成功、不新增版本与快照
//	K13 `TestResourceAdmin_SkillValidateNoWrites`——校验端点与写面共用判据：不落盘、不写操作行（约束 26）
//
// 判据只看真实磁盘与真实数据库行：定义文件、`management_operations`、`resource_versions`
// 都是真实实现写的；只有外部模型调用用回环假服务。
// Every judgement below is made against real files and real database rows; only the
// outbound model call goes to a loopback fake.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/testutil"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// 哨兵环境变量：条目里的 api_key 恒为精确 `${VAR}` 占位，发布路径构造运行时客户端时
// 才解析它。正常路径要求它存在，因此放在包级 TestMain——`t.Setenv` 与 `t.Parallel`
// 互斥，而原样落地的回归用例带 `t.Parallel()`。
// The sentinel variable is set package-wide because the verbatim regression case calls
// t.Parallel, which rules out t.Setenv.
const (
	resourceTestKeyEnv      = "BASALT_RESOURCE_TEST_KEY"
	resourceTestKeySentinel = "resource-test-sentinel"
	// mcpTestTokenEnv 是 MCP 测试端点用例的 env 占位变量：连接前解析成终值（约束 22），
	// 与资源测试 key 同口径放在包级 TestMain。
	// mcpTestTokenEnv is the placeholder resolved before an MCP test connection.
	mcpTestTokenEnv      = "MCP_TEST_TOKEN"
	mcpTestTokenSentinel = "mcp-test-sentinel-not-a-secret"
)

// resourceActor 是本机信任主体的测试形态：卡片用例写作 `core.Actor{Subject: "test"}`，
// 但 `auth_method` 列有 CHECK 约束（bearer / local_trust / cli / migration），夹具必须
// 补一个合法方法；主体名保持卡片的 "test"（`actor_subject` 无约束）。
// resourceActor fills the auth method the schema requires; the subject stays "test".
func resourceActor() core.Actor {
	return core.Actor{
		Subject: "test",
		Method:  core.AuthMethodLocalTrust,
		Source:  "127.0.0.1:41001",
	}
}

// TestMain 在整包范围内准备哨兵环境变量：设置失败直接退 1（不得带着未就绪的凭证
// 让用例以"400 缺变量"的方式假绿）。
// TestMain prepares the sentinel variable; a failed setup exits 1 rather than letting
// cases pass for the wrong reason.
func TestMain(m *testing.M) {
	// 诊断哨兵：诊断用例把测试二进制自己当命令、并把 `diagSentinelArgPrefix<path>`
	// 作为唯一参数——若实现真的启动了子进程，子进程走到这里写下哨兵文件就退出，
	// 用例据此断言"文件从未出现"。正常跑测试时没有参数会命中这个前缀。
	// The diagnosis sentinel: a spawned child writes the marker file and exits, so the
	// test can prove no process was ever started.
	for _, arg := range os.Args[1:] {
		if path, found := strings.CutPrefix(arg, diagSentinelArgPrefix); found {
			if err := os.WriteFile(path, []byte("started"), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "写诊断哨兵失败: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	for name, value := range map[string]string{
		resourceTestKeyEnv: resourceTestKeySentinel,
		mcpTestTokenEnv:    mcpTestTokenSentinel,
	} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintf(os.Stderr, "设置 %s 失败: %v\n", name, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// initialResourceConfig 是夹具的定义文件：块外带顶部 / 底部注释，块内有一个**同文件
// 其它资源**（backup）——块编辑必须逐字保留这两部分，且 backup 的占位与主干资源不同，
// "更新时空 api_key 继承"不会被兄弟条目掩盖。
// The fixture file carries surrounding comments and a sibling entry, so block edits
// must preserve both and inheritance cannot be confused with the sibling's placeholder.
const initialResourceConfig = `# 顶部注释
providers:
  - name: backup
    base_url: https://backup.example
    api_key: ${BACKUP_KEY}
    default_model: backup-model
# 底部注释
`

// resourceRuntime 是"当前生效目录"的只读测试视图：按名取已发布客户端。
// resourceRuntime is the read-only test view over the live catalog.
type resourceRuntime struct{ catalog *providerCatalogStore }

// Provider 返回该名字当前生效的客户端；未发布 → nil。
// Provider returns the live client for the name, or nil when it is not published.
func (r resourceRuntime) Provider(name string) core.ProviderClient {
	catalog := r.catalog.Load()
	if catalog == nil {
		return nil
	}
	return catalog.clients[name]
}

// resourceEnv 是资源写面夹具：临时目录里的真实定义文件 + 真实 SQLite + 共享闸门 +
// 共享目录持有者 + 可注入的提交失败点。失败注入走 `PublishVersion` 注入位——它只在
// 事务内失败，不伪造第二套实现。
// The fixture wires real files, a real database, a shared gate and catalog, and an
// injectable failure point that fails inside the transaction.
type resourceEnv struct {
	t          *testing.T
	root       string
	configPath string
	dbPath     string
	db         *storage.DB
	store      *storage.ResourceStore
	ops        *storage.ManagementStore
	gate       *core.ResourceGate
	catalog    *providerCatalogStore
	runtime    resourceRuntime
	admin      *ResourceAdmin
	logger     *slog.Logger
	logs       *bytes.Buffer

	// 14D 起 MCP 保存必须显式声明每个占位符的来源；夹具因此也装配真实的凭据服务，
	// 用例可直读来源声明与托管状态（T101 / B1–B8）。
	// Since 14D every MCP placeholder needs an explicit source, so the fixture carries
	// the real credential service.
	mcpCredentials *MCPCredentialService
	masterKeyFile  string

	mu       sync.Mutex
	failNext error
}

// newResourceEnv 建夹具；mutate 在构造前改写入参（注入超时 / 失败点用）。
// newResourceEnv builds the fixture; mutate adjusts the deps before construction.
func newResourceEnv(t *testing.T, mutate ...func(*ResourceAdminDeps)) *resourceEnv {
	t.Helper()

	root := t.TempDir()
	configPath := filepath.Join(root, "basalt.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(initialResourceConfig), 0o600))

	dbPath := filepath.Join(root, "basalt.db")
	db, err := storage.OpenWritable(context.Background(), dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	catalog := &providerCatalogStore{}
	catalog.Store(&providerCatalog{
		clients: map[string]core.ProviderClient{},
		models:  map[string]string{},
	})
	logger, logs := newCapturedLogger()

	// 主密钥放在工作区**之外**：`root` 同时是 MCP 定义文件的根（`ParseMCPServers(root)`），
	// 密钥落在 `root/secrets` 会被 `LoadMasterKey` 按"不得放进工作区"拒绝——真实部署
	// 也是这条纪律。第二份 `t.TempDir()` 与 `root` 同级，既不互相包含又不跨到别的用例。
	// The master key lives outside the workspace root, mirroring production: root doubles
	// as the definition root here, so the key sits in a sibling temp directory.
	masterKeyFile := writeMasterKeyFile(t, t.TempDir(), "k_mcp", testMasterKey().Key)
	mcpCredentials, err := NewMCPCredentialService(MCPCredentialServiceDeps{
		Credentials:   storage.NewCredentialStore(db),
		Sources:       storage.NewMCPCredentialSourceStore(db),
		MasterKeyFile: masterKeyFile,
		WorkspaceRoot: root,
		Logger:        logger,
	})
	require.NoError(t, err)

	env := &resourceEnv{
		t:              t,
		root:           root,
		configPath:     configPath,
		dbPath:         dbPath,
		db:             db,
		store:          storage.NewResourceStore(db),
		ops:            storage.NewManagementStore(db),
		gate:           &core.ResourceGate{},
		catalog:        catalog,
		runtime:        resourceRuntime{catalog: catalog},
		logger:         logger,
		logs:           logs,
		mcpCredentials: mcpCredentials,
		masterKeyFile:  masterKeyFile,
	}

	deps := ResourceAdminDeps{
		ConfigPath: configPath,
		Root:       root,
		// 缺省快照为零 Agent：归档目录此时也不存在（`<root>/archive` 由归档路径创建），
		// 引用闸门走"无引用"分支。需要引用的用例在 mutate 里换掉它。
		Agents:         func() ([]core.AgentEntry, error) { return nil, nil },
		Store:          env.store,
		Operations:     env.ops,
		Gate:           env.gate,
		Catalog:        catalog,
		Logger:         logger,
		Now:            baselineClock,
		PublishVersion: env.publishVersion,
		// 诊断构造器与启动共用 `tool.NewMCPCommand`（C-04）：夹具按 wire 的装配口径
		// 绑定 root 与 envPassthrough，诊断用例因此走真实构造路径而不是近似解析。
		DiagnoseMCP: func(spec core.MCPServerConfig) core.MCPCommandDiagnosis {
			return diagnoseMCPCommand(spec, env.root, nil)
		},
		// 依赖轴锚在工作区下的 `runtime/`：夹具与 wire 用同一个探测实现，只是根目录取
		// 测试自己的临时目录（用例要造组件，不能碰真实程序目录）。
		MCPDependencies: mcpDependencyProbe{runtimeRoot: filepath.Join(root, "runtime")}.Diagnose,
		MCPCredentials:  mcpCredentials,
	}
	for _, m := range mutate {
		m(&deps)
	}
	env.admin = NewResourceAdmin(deps)
	return env
}

// publishVersion 是提交段的失败注入点：`failNext` 非空时这一次提交返回错误，
// 其余情况走真实的同事务实现。
// publishVersion injects at most one commit failure and otherwise delegates to the
// real transactional implementation.
func (e *resourceEnv) publishVersion(ctx context.Context, v core.ResourceVersion, applied bool,
	operationID string, now time.Time,
) (core.ResourceVersion, error) {
	e.mu.Lock()
	fail := e.failNext
	e.failNext = nil
	e.mu.Unlock()
	if fail != nil {
		return core.ResourceVersion{}, fail
	}
	return e.store.PublishWithVersionState(ctx, v, applied, operationID, now)
}

// failNextVersionInsert 让下一次提交在事务内失败（只生效一次）。
// failNextVersionInsert makes the next commit fail once.
func (e *resourceEnv) failNextVersionInsert(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failNext = err
}

// publishModelConnection 通过真实写路径建一条模型连接并立即发布，返回已发布版本。
// publishModelConnection creates and publishes one model connection through the real
// write path and returns the published version.
func (e *resourceEnv) publishModelConnection(t *testing.T, name, baseURL, model string) core.ResourceVersion {
	t.Helper()
	version, err := e.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         name,
			BaseURL:      baseURL,
			APIKey:       "${" + resourceTestKeyEnv + "}",
			APIKeySet:    true,
			DefaultModel: model,
			Create:       true,
		})
	require.NoError(t, err)
	return version
}

// readConfig 读定义文件当前字节。
// readConfig returns the definition file's current bytes.
func (e *resourceEnv) readConfig() string {
	e.t.Helper()
	raw, err := os.ReadFile(e.configPath)
	require.NoError(e.t, err)
	return string(raw)
}

// writeConfig 覆盖定义文件（模拟手工编辑）。
// writeConfig overwrites the definition file to simulate a manual edit.
func (e *resourceEnv) writeConfig(content string) {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(e.configPath, []byte(content), 0o600))
}

// listOperations 按过滤条件取操作行。
// listOperations returns the operation rows matching the filter.
func (e *resourceEnv) listOperations(filter core.OperationFilter) []core.OperationRecord {
	e.t.Helper()
	rows, err := e.ops.ListOperations(context.Background(), filter)
	require.NoError(e.t, err)
	return rows
}

// operationCount 数全部操作行。
// operationCount counts every operation row.
func (e *resourceEnv) operationCount() int {
	e.t.Helper()
	return len(e.listOperations(core.OperationFilter{Limit: 200}))
}

// pointer 取资源发布指针。
// pointer returns the publish pointer.
func (e *resourceEnv) pointer(name string) core.ResourcePointer {
	e.t.Helper()
	ptr, err := e.store.Pointer(context.Background(), core.ResourceModelConnection, name)
	require.NoError(e.t, err)
	return ptr
}

// versionsOf 取资源的全部版本（新到旧）。
// versionsOf returns every version of the resource, newest first.
func (e *resourceEnv) versionsOf(name string) []core.ResourceVersion {
	e.t.Helper()
	versions, err := e.store.Versions(context.Background(), core.ResourceModelConnection, name)
	require.NoError(e.t, err)
	return versions
}

// TestResourceAdmin_PublishFailureKeepsOldPointer：模块卡 §4.3.2（R3.3 修订）落地——
// 已发布内容不同的 v1 / v2，当前发布 / 应用指针与运行时都在 v2；显式撤回 v1 时注入
// 新版本插入失败，必须证明故障真被触发、返回对应错误、文件恢复为操作前字节、指针与
// 运行时仍为 v2、没有残留新版本，操作行 failed 可查（约束 1a / 9 / 10）。
// TestResourceAdmin_PublishFailureKeepsOldPointer covers the R3.3 revert-failure scenario.
func TestResourceAdmin_PublishFailureKeepsOldPointer(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t) // 临时工作区 + 已发布 v1
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	v2, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://b.example",
			DefaultModel: "m2",
			IfMatch:      env.currentETag(t, "main"),
		})
	require.NoError(t, err)
	require.NotEqual(t, v1.ContentHash, v2.ContentHash, "撤回前后的版本内容必须不同")

	initial := env.pointer("main")
	require.Equal(t, v2.ID, initial.PublishedVersionID)
	require.NotNil(t, initial.AppliedVersionID)
	require.Equal(t, v2.ID, *initial.AppliedVersionID)
	runtimeClient := env.runtime.Provider("main")
	require.NotNil(t, runtimeClient)
	require.Equal(t, "m2", env.catalog.Load().models["main"])
	before := env.readConfig()
	versionsBefore := len(env.versionsOf("main"))

	env.failNextVersionInsert(errors.New("disk full"))
	_, err = env.admin.Publish(context.Background(), resourceActor(),
		core.ResourcePublishRequest{
			Kind:         core.ResourceModelConnection,
			ID:           "main",
			VersionID:    &v1.ID,
			ExpectedETag: env.currentETag(t, "main"),
		})
	require.Error(t, err)

	// 故障注入必须真的触发：注入文案只出现在提交段失败日志里（对外错误不含它）。
	require.Contains(t, env.logs.String(), "disk full")

	require.Equal(t, before, env.readConfig(), "失败后定义文件必须恢复为操作前字节（v2 内容）")

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.ErrorIs(t, err, errResourceInternal)
	operationID, ok := adminErr.Data["operation_id"].(string)
	require.True(t, ok, "pending 之后的失败必须带回 operation_id")
	require.NotEmpty(t, operationID)

	ptr := env.pointer("main")
	require.Equal(t, v2.ID, ptr.PublishedVersionID, "失败保旧：发布指针仍为 v2")
	require.NotNil(t, ptr.AppliedVersionID)
	require.Equal(t, v2.ID, *ptr.AppliedVersionID, "失败保旧：应用指针仍为 v2")

	require.Equal(t, "m2", env.catalog.Load().models["main"], "运行时不得退回 v1 的默认模型")
	require.Same(t, runtimeClient, env.runtime.Provider("main"), "运行时映射必须是操作前的同一 v2 客户端")

	versions := env.versionsOf("main")
	require.Len(t, versions, versionsBefore, "事务回滚后不得残留新版本")
	require.Equal(t, v2.ID, versions[0].ID)
	require.Equal(t, v2.ContentHash, versions[0].ContentHash)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionPublish})
	require.Len(t, rows, 1)
	require.Equal(t, operationID, rows[0].OperationID)
	require.Equal(t, core.OperationFailed, rows[0].Status)
	require.Equal(t, lifecycleResultInternal, rows[0].ResultCode)
	require.Equal(t, "发布事务失败", rows[0].ErrorMessage)
	require.Equal(t, env.currentETag(t, "main"), rows[0].ExpectedETag)
	require.Equal(t, v1.ID, rows[0].RequestedVersionID)
}

// TestResourceAdmin_PublishBuildFailureKeepsOld：运行时对象构建失败（占位变量为空）
// 发生在预写之前——400 点名变量、文件逐字节不变、指针不变、零操作行（约束 7 / 10）。
// TestResourceAdmin_PublishBuildFailureKeepsOld covers the pre-pending build failure.
func TestResourceAdmin_PublishBuildFailureKeepsOld(t *testing.T) {
	// 本用例改环境变量，不得与 t.Parallel 混用；Go 会等全部串行用例结束后才恢复平行用例。
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	before := env.readConfig()
	rowsBefore := env.operationCount()

	t.Setenv(resourceTestKeyEnv, "")
	_, err := env.admin.Publish(context.Background(), resourceActor(),
		core.ResourcePublishRequest{Kind: core.ResourceModelConnection, ID: "main", VersionID: &v1.ID})
	require.Error(t, err)
	require.Contains(t, err.Error(), resourceTestKeyEnv, "错误必须点名变量名，且不回显值")

	require.Equal(t, before, env.readConfig())
	require.Equal(t, rowsBefore, env.operationCount(), "构建失败在预写之前，不得留下操作行")
	require.Equal(t, v1.ID, env.pointer("main").PublishedVersionID)
	require.NotNil(t, env.runtime.Provider("main"))
}

// TestResourceAdmin_DBFailureRestoresFile：文件已写、提交段失败 → 身份检查通过后
// 恢复原字节，操作 failed（不是 unknown）且响应带回可查的 operation_id。
// TestResourceAdmin_DBFailureRestoresFile asserts the identity-checked restore and a
// queryable failed operation.
func TestResourceAdmin_DBFailureRestoresFile(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	before := env.readConfig()

	env.failNextVersionInsert(errors.New("disk full"))
	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://b.example",
			DefaultModel: "m2",
			IfMatch:      env.currentETag(t, "main"),
		})
	require.Error(t, err)

	require.Equal(t, before, env.readConfig(), "提交段失败必须把定义文件恢复为原字节")
	require.Equal(t, v1.ID, env.pointer("main").PublishedVersionID)

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	operationID, ok := adminErr.Data["operation_id"].(string)
	require.True(t, ok, "pending 之后的失败必须带回 operation_id")
	require.NotEmpty(t, operationID)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionUpdate})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status, "身份检查通过并完成恢复 → failed，不是 unknown")
	require.Equal(t, lifecycleResultInternal, rows[0].ResultCode)

	stored, err := env.ops.GetOperation(context.Background(), operationID)
	require.NoError(t, err)
	require.Equal(t, core.OperationFailed, stored.Status)
}

// TestResourceAdmin_NoFileWriteOnPreWriteFailure：预写失败 → 拒绝执行、文件与台账零变化。
// TestResourceAdmin_NoFileWriteOnPreWriteFailure asserts zero writes when the pending
// row cannot be written.
func TestResourceAdmin_NoFileWriteOnPreWriteFailure(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t, func(d *ResourceAdminDeps) {
		d.Operations = failingBeginOperations{OperationStore: d.Operations, err: errors.New("数据库不可用")}
	})
	before := env.readConfig()

	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://a.example",
			APIKey:       "${" + resourceTestKeyEnv + "}",
			APIKeySet:    true,
			DefaultModel: "m1",
			Create:       true,
		})
	require.Error(t, err)
	require.Equal(t, before, env.readConfig())
	require.Equal(t, 0, env.operationCount())
}

// TestResourceAdmin_EveryWriteHasOperation：创建 / 更新 / 发布 / 测试各留一行终态台账，
// 标识互不相同；被 412 拒绝的请求不产生行（约束 1a）。
// TestResourceAdmin_EveryWriteHasOperation asserts one row per write and none for 412.
func TestResourceAdmin_EveryWriteHasOperation(t *testing.T) {
	t.Parallel()
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		Body: `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`,
	})
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", srv.URL(), "m1")

	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      srv.URL(),
			DefaultModel: "m2",
			IfMatch:      env.currentETag(t, "main"),
		})
	require.NoError(t, err)

	_, err = env.admin.Publish(context.Background(), resourceActor(),
		core.ResourcePublishRequest{Kind: core.ResourceModelConnection, ID: "main", VersionID: &v1.ID})
	require.NoError(t, err)

	_, err = env.admin.TestModelConnection(context.Background(), resourceActor(), "main")
	require.NoError(t, err)

	rows := env.listOperations(core.OperationFilter{
		TargetKind: core.TargetKindModelConnection,
		TargetID:   "main",
	})
	require.Len(t, rows, 4)

	actions := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		actions = append(actions, row.Action)
		require.Equal(t, "test", row.ActorSubject)
		require.NotEmpty(t, row.RequestHash)
		require.Equal(t, core.OperationSuccess, row.Status)
		require.NotEmpty(t, row.OperationID)
		seen[row.OperationID] = struct{}{}
	}
	require.Len(t, seen, len(rows), "每个写操作必须有自己的操作标识")
	require.ElementsMatch(t, []string{
		core.ActionModelConnectionCreate,
		core.ActionModelConnectionUpdate,
		core.ActionModelConnectionPublish,
		core.ActionModelConnectionTest,
	}, actions)

	// 412 在预写之前完成判定：不产生第五行。
	_, err = env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      srv.URL(),
			DefaultModel: "m3",
			IfMatch:      `"sha256:0000000000000000000000000000000000000000000000000000000000000000"`,
		})
	require.ErrorIs(t, err, core.ErrPreconditionFailed)
	require.Equal(t, 4, env.operationCount())
}

// TestModelConnection_UpsertPublishes：新建并立即发布——版本 1、指针 / 生效指针同指、
// 目录换新客户端、定义文件只追加目标块且块外注释保留。
// TestModelConnection_UpsertPublishes asserts the four sides agree after a create.
func TestModelConnection_UpsertPublishes(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")

	require.Equal(t, 1, v1.VersionNo)
	require.NotZero(t, v1.ID)
	require.Equal(t, core.ResourceModelConnection, v1.Kind)
	require.Equal(t, "main", v1.ResourceID)
	require.NotEmpty(t, v1.ContentHash)
	require.Contains(t, v1.DefinitionYAML, "name: main")
	require.NotEmpty(t, v1.OperationID)

	ptr := env.pointer("main")
	require.Equal(t, v1.ID, ptr.PublishedVersionID)
	require.NotNil(t, ptr.AppliedVersionID)
	require.Equal(t, v1.ID, *ptr.AppliedVersionID)
	require.Nil(t, ptr.DeletedAt)

	require.NotNil(t, env.runtime.Provider("main"))
	require.Equal(t, "m1", env.catalog.Load().models["main"])

	config := env.readConfig()
	require.Contains(t, config, "# 顶部注释", "块外字节必须逐字保留")
	require.Contains(t, config, "# 底部注释", "块外字节必须逐字保留")
	require.Contains(t, config, "name: backup", "同文件其它资源不得被改动")
	require.Contains(t, config, "name: main")
}

// TestModelConnection_UpdateInheritsAPIKey：更新时空 api_key 保留已发布版本的占位，
// 版本号递增，目录换成新值。
// TestModelConnection_UpdateInheritsAPIKey asserts the published placeholder survives.
func TestModelConnection_UpdateInheritsAPIKey(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")

	v2, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://b.example",
			DefaultModel: "m2",
			IfMatch:      env.currentETag(t, "main"),
		})
	require.NoError(t, err)
	require.Equal(t, 2, v2.VersionNo)

	config := env.readConfig()
	require.Equal(t, 1, strings.Count(config, "${"+resourceTestKeyEnv+"}"), "占位必须原样继承")
	require.Contains(t, config, "https://b.example")
	require.NotContains(t, config, "https://a.example")
	require.Equal(t, "m2", env.catalog.Load().models["main"])
	require.Equal(t, v2.ID, env.pointer("main").PublishedVersionID)
}

// TestModelConnection_StaleETagZeroWrite：If-Match 与当前发布版本不等 → 412、
// `data.current_etag` 给出当前值、零写入、零操作行（约束 3 / 1a）。
// TestModelConnection_StaleETagZeroWrite asserts the 412 shape and that nothing moved.
func TestModelConnection_StaleETagZeroWrite(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")
	before := env.readConfig()
	rowsBefore := env.operationCount()

	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://b.example",
			DefaultModel: "m2",
			IfMatch:      `"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"`,
		})
	require.ErrorIs(t, err, core.ErrPreconditionFailed)

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, env.currentETag(t, "main"), adminErr.Data["current_etag"])
	require.NotContains(t, fmt.Sprint(adminErr.Data), "base_url", "响应不得回显定义正文")

	require.Equal(t, before, env.readConfig())
	require.Equal(t, rowsBefore, env.operationCount())
}

// TestModelConnection_MissingEnvZeroWrite：占位引用的变量未设置 → 400 点名变量、
// 文件逐字节不变、零操作行（约束 7）。
// TestModelConnection_MissingEnvZeroWrite asserts the pre-pending environment failure.
func TestModelConnection_MissingEnvZeroWrite(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	before := env.readConfig()
	rowsBefore := env.operationCount()

	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://a.example",
			APIKey:       "${BASALT_RESOURCE_UNSET_KEY}",
			APIKeySet:    true,
			DefaultModel: "m1",
			Create:       true,
		})
	require.Error(t, err)
	require.Contains(t, err.Error(), "BASALT_RESOURCE_UNSET_KEY", "必须点名变量名")

	require.Equal(t, before, env.readConfig())
	require.Equal(t, rowsBefore, env.operationCount())
}

// TestModelConnection_DuplicateCreateWritesFailedOperation：同名已存在 → 409、
// 一条 failed 操作行、零文件改动（失败判据表：重名 = 操作 failed）。
// TestModelConnection_DuplicateCreateWritesFailedOperation asserts the failed row.
func TestModelConnection_DuplicateCreateWritesFailedOperation(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")
	before := env.readConfig()
	rowsBefore := env.operationCount()

	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://b.example",
			APIKey:       "${" + resourceTestKeyEnv + "}",
			APIKeySet:    true,
			DefaultModel: "m2",
			Create:       true,
		})
	require.ErrorIs(t, err, core.ErrConflict)

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Contains(t, adminErr.Data, "operation_id")
	require.Equal(t, before, env.readConfig())
	require.Equal(t, rowsBefore+1, env.operationCount())

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionCreate})
	require.Len(t, rows, 2)
	var failed []core.OperationRecord
	for _, row := range rows {
		if row.Status != core.OperationSuccess {
			failed = append(failed, row)
		}
	}
	require.Len(t, failed, 1)
	require.Equal(t, core.OperationFailed, failed[0].Status)
	require.Equal(t, resourceResultConflict, failed[0].ResultCode)
}

// TestResourceWrite_FileDivergedConflict：文件里目标块的字节已不是已发布版本的字节
// （手工改动）→ 409、文件保持手工版本、零操作行（约束 34）。
// TestResourceWrite_FileDivergedConflict asserts the divergence gate refuses the write.
func TestResourceWrite_FileDivergedConflict(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")

	diverged := strings.Replace(env.readConfig(), "https://a.example", "https://hand-edited.example", 1)
	require.Contains(t, diverged, "https://hand-edited.example")
	env.writeConfig(diverged)
	rowsBefore := env.operationCount()

	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://b.example",
			DefaultModel: "m2",
			IfMatch:      env.currentETag(t, "main"),
		})
	require.ErrorIs(t, err, core.ErrConflict)

	require.Equal(t, diverged, env.readConfig(), "拒绝写入时不得覆盖手工改动")
	require.Equal(t, rowsBefore, env.operationCount())
}

// TestResourceWrite_DivergenceCheckedInsideFileLock：闸门判据必须读取**锁内重读**的
// 文件——测试先持有定义文件锁，在更新等待期间改动文件，更新拿到锁后必须看到改动后的
// 字节并拒绝（约束 36：闸门在文件锁内）。
// TestResourceWrite_DivergenceCheckedInsideFileLock proves the gate reads the file
// re-read inside the file lock, not a snapshot taken earlier.
func TestResourceWrite_DivergenceCheckedInsideFileLock(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")

	unlock := env.admin.fileLocks.lock(env.configPath)
	done := make(chan error, 1)
	go func() {
		_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
			ModelConnectionRequest{
				Name:         "main",
				BaseURL:      "https://b.example",
				DefaultModel: "m2",
				IfMatch:      env.currentETag(t, "main"),
			})
		done <- err
	}()

	// 更新此刻要么已阻塞在文件锁上、要么还在读指针；两种情形下它都只能在放锁之后
	// 读文件，因此先改文件再放锁即可确定它看到的是改动后的字节。
	diverged := strings.Replace(env.readConfig(), "https://a.example", "https://hand-edited.example", 1)
	writeErr := os.WriteFile(env.configPath, []byte(diverged), 0o600)
	unlock()
	require.NoError(t, writeErr)

	require.ErrorIs(t, <-done, core.ErrConflict)
	require.Equal(t, diverged, env.readConfig())
}

// TestPublish_RewritesDefinitionFile：显式撤回把定义文件改写为来源版本的字节。
// TestPublish_RewritesDefinitionFile asserts the file follows the published version.
func TestPublish_RewritesDefinitionFile(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")

	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://b.example",
			DefaultModel: "m2",
			IfMatch:      env.currentETag(t, "main"),
		})
	require.NoError(t, err)
	require.Contains(t, env.readConfig(), "https://b.example")

	_, err = env.admin.Publish(context.Background(), resourceActor(),
		core.ResourcePublishRequest{Kind: core.ResourceModelConnection, ID: "main", VersionID: &v1.ID})
	require.NoError(t, err)

	config := env.readConfig()
	require.Contains(t, config, "https://a.example")
	require.NotContains(t, config, "https://b.example")
	require.Contains(t, config, "# 顶部注释")
	require.Equal(t, "m1", env.catalog.Load().models["main"])
}

// TestPublish_RevertCreatesNewVersion：撤回只追加新版本（version_no = N+1、
// content_hash 等于来源版本），历史版本逐字不变（约束 9 / FR-13）。
// TestPublish_RevertCreatesNewVersion asserts append-only history.
func TestPublish_RevertCreatesNewVersion(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")

	v2, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://b.example",
			DefaultModel: "m2",
			IfMatch:      env.currentETag(t, "main"),
		})
	require.NoError(t, err)

	result, err := env.admin.Publish(context.Background(), resourceActor(),
		core.ResourcePublishRequest{Kind: core.ResourceModelConnection, ID: "main", VersionID: &v1.ID})
	require.NoError(t, err)
	require.False(t, result.PendingRestart, "模型连接发布后立即生效")

	versions := env.versionsOf("main")
	require.Len(t, versions, 3)
	v3 := versions[0]
	require.Equal(t, 3, v3.VersionNo)
	require.NotEqual(t, v1.ID, v3.ID)
	require.Equal(t, v3.ID, result.Version.ID)
	require.Equal(t, v1.ContentHash, v3.ContentHash)
	require.Equal(t, v1.DefinitionYAML, v3.DefinitionYAML)
	require.NotEqual(t, v2.ContentHash, v3.ContentHash)

	stored, err := env.store.VersionByID(context.Background(), v1.ID)
	require.NoError(t, err)
	require.Equal(t, v1.DefinitionYAML, stored.DefinitionYAML, "历史版本不得被改写")
	require.Equal(t, 1, stored.VersionNo)

	require.Equal(t, v3.ID, env.pointer("main").PublishedVersionID)
}

// TestPublish_Idempotent：不带版本号发布且最新版本已是发布指针 → 幂等成功、
// 不新增版本、文件不动，但仍留一条成功操作行（约束 9）。
// TestPublish_Idempotent asserts the idempotent branch.
func TestPublish_Idempotent(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	before := env.readConfig()
	rowsBefore := env.operationCount()

	result, err := env.admin.Publish(context.Background(), resourceActor(),
		core.ResourcePublishRequest{Kind: core.ResourceModelConnection, ID: "main"})
	require.NoError(t, err)
	require.Equal(t, v1.ID, result.Version.ID)

	require.Len(t, env.versionsOf("main"), 1, "幂等发布不得新增版本")
	require.Equal(t, before, env.readConfig())
	require.Equal(t, rowsBefore+1, env.operationCount())

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionPublish})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	require.Equal(t, lifecycleResultOK, rows[0].ResultCode)
}

// TestPublish_RetryAfterFileRewriteCompletes：约束 13 的"重新发布"——上次撤回发布
// 已把定义文件改写为来源版本字节、但版本 / 指针事务未提交时，重试按幂等补齐完成：
// 不重复改写文件、追加一个新版本并推进指针，文件与指针重新一致。
// 逆改点：重试被分歧闸门 409 / 重复改写文件 / 不追加版本 → 对应断言红。
func TestPublish_RetryAfterFileRewriteCompletes(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://b.example",
			DefaultModel: "m2",
			IfMatch:      env.currentETag(t, "main"),
		})
	require.NoError(t, err)

	// 模拟崩溃窗口：撤回发布已写好文件，事务没提交。
	rewritten, lost, replaceErr := workspace.ReplaceEntry([]byte(env.readConfig()),
		providersBlockKey, "main", definitionEntry(v1.DefinitionYAML))
	require.NoError(t, replaceErr)
	require.False(t, lost)
	env.writeConfig(string(rewritten))
	before := env.readConfig()
	versionsBefore := len(env.versionsOf("main"))
	rowsBefore := env.operationCount()

	result, err := env.admin.Publish(context.Background(), resourceActor(),
		core.ResourcePublishRequest{Kind: core.ResourceModelConnection, ID: "main", VersionID: &v1.ID})
	require.NoError(t, err)
	require.Equal(t, v1.ContentHash, result.Version.ContentHash)
	require.Equal(t, versionsBefore+1, len(env.versionsOf("main")), "重试仍追加新版本")
	require.Equal(t, result.Version.ID, env.pointer("main").PublishedVersionID)
	require.Equal(t, before, env.readConfig(), "重试不得重复改写文件")
	require.Equal(t, "m1", env.catalog.Load().models["main"])
	require.Equal(t, rowsBefore+1, env.operationCount())
}

// TestResourceDelete_RetryAfterBlockRemovedCompletes：约束 13 的幂等补齐——上次删除
// 已把目标块移出定义文件、指针事务未提交时，重试不再 409：不再改写文件，仍走引用
// 闸门并打删除标记，版本历史不动。
// 逆改点：缺失块按分歧 409 / 重写文件重建块 / 不标记删除 → 对应断言红。
func TestResourceDelete_RetryAfterBlockRemovedCompletes(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")

	removed, _, removeErr := workspace.RemoveEntry([]byte(env.readConfig()), providersBlockKey, "main")
	require.NoError(t, removeErr)
	env.writeConfig(string(removed))
	before := env.readConfig()
	versionsBefore := len(env.versionsOf("main"))
	rowsBefore := env.operationCount()

	require.NoError(t, env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main")))

	require.Equal(t, before, env.readConfig(), "幂等补齐不得改写文件")
	require.NotNil(t, env.pointer("main").DeletedAt)
	require.Equal(t, versionsBefore, len(env.versionsOf("main")), "删除不追加版本")
	require.Nil(t, env.runtime.Provider("main"), "运行时目录不得保留已删除资源")
	require.Equal(t, rowsBefore+1, env.operationCount())
	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionDelete})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
}

// TestResourceDelete_SkillRetryAfterArchiveCompletes：技能删除的同一崩溃窗口——工作
// 副本已移入归档、指针事务未提交时，重试按幂等补齐完成：不再生成第二份归档，只打
// 删除标记；目录仍在但缺 SKILL.md 的外部改动仍走 409（见
// TestSkillWrite_WorkCopyDivergedConflict）。
// 逆改点：工作副本缺席按 409 / 重复归档 / 不标记删除 → 对应断言红。
func TestResourceDelete_SkillRetryAfterArchiveCompletes(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	detail := env.createSkill(t, "logger", "日志查询")
	archived := filepath.Join(env.root, "archive", "skills", "logger-20261022-090000")
	require.NoError(t, os.MkdirAll(filepath.Dir(archived), 0o755))
	require.NoError(t, os.Rename(filepath.Join(env.root, "skills", "logger"), archived))
	rowsBefore := env.operationCount()

	require.NoError(t, env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceSkill, "logger", detail.ETag,
		env.uidOf(core.ResourceSkill, "logger")))

	require.NotNil(t, env.skillPointer(t, "logger").DeletedAt)
	archives, err := os.ReadDir(filepath.Join(env.root, "archive", "skills"))
	require.NoError(t, err)
	require.Len(t, archives, 1, "幂等补齐不得生成第二份归档")
	require.Len(t, env.skillVersions(t, "logger"), 1, "删除不追加版本")
	require.Equal(t, rowsBefore+1, env.operationCount())
	rows := env.listOperations(core.OperationFilter{Action: core.ActionSkillDelete})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
}

// TestModelConnection_TestWritesOperation：测试端点先写操作行、只发一次最小调用、
// 不建运行、不写 `llm_calls`、不动版本 / 指针 / 目录；结果脱敏落在 detail_json。
// TestModelConnection_TestWritesOperation asserts one small call and no side effects.
func TestModelConnection_TestWritesOperation(t *testing.T) {
	t.Parallel()
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		Body: `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`,
	})
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", srv.URL(), "m1")

	result, err := env.admin.TestModelConnection(context.Background(), resourceActor(), "main")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.GreaterOrEqual(t, result.LatencyMS, int64(0))
	require.Equal(t, "m1", result.Model)

	requests := srv.Requests()
	require.Len(t, requests, 1, "测试只发一次最小调用")
	require.Equal(t, "m1", requests[0]["model"])
	messages, ok := requests[0]["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 1)
	require.Equal(t, modelTestPing, messages[0].(map[string]any)["content"])

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionTest})
	require.Len(t, rows, 1)
	row := rows[0]
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, lifecycleResultOK, row.ResultCode)
	require.Equal(t, core.TargetKindModelConnection, row.TargetKind)
	require.Equal(t, "main", row.TargetID)
	require.NotEmpty(t, row.RequestHash)

	var detail modelTestOperationDetail
	require.NoError(t, json.Unmarshal(row.DetailJSON, &detail))
	require.True(t, detail.OK)
	require.Equal(t, "m1", detail.Model)
	require.NotContains(t, string(row.DetailJSON), resourceTestKeySentinel, "detail 必须脱敏")

	require.Len(t, env.versionsOf("main"), 1, "测试不得新增版本")
	require.Equal(t, v1.ID, env.pointer("main").PublishedVersionID)
	require.NotNil(t, env.runtime.Provider("main"))
	require.Zero(t, authorRowCount(t, env.dbPath, "llm_calls"), "测试不是 Run，不得写模型调用审计")
}

// TestModelConnection_TestFailureIsSuccessOperation：外部调用失败（连接 / 非 2xx）
// 仍是 success 操作 + `ok=false`，错误码来自客户端分类；不写 `llm_calls`。
// TestModelConnection_TestFailureIsSuccessOperation asserts a completed test records
// success at the operation level while the result says ok=false.
func TestModelConnection_TestFailureIsSuccessOperation(t *testing.T) {
	t.Parallel()
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		StatusCode: http.StatusInternalServerError,
		Body:       `{"error":{"message":"boom"}}`,
	})
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", srv.URL(), "m1")

	result, err := env.admin.TestModelConnection(context.Background(), resourceActor(), "main")
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, string(core.ErrorKindExternalError), result.ErrorCode)
	require.NotEmpty(t, result.ErrorMessage)
	require.NotContains(t, result.ErrorMessage, resourceTestKeySentinel, "错误文案不得回显凭证")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionTest})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	require.Equal(t, lifecycleResultOK, rows[0].ResultCode)

	var detail modelTestOperationDetail
	require.NoError(t, json.Unmarshal(rows[0].DetailJSON, &detail))
	require.False(t, detail.OK)
	require.Equal(t, string(core.ErrorKindExternalError), detail.ErrorCode)
	require.Zero(t, authorRowCount(t, env.dbPath, "llm_calls"))
}

// TestModelConnection_TestTimeout504：固定超时到期 → 504 分类、操作 failed +
// `TIMEOUT`（调用没有完成，不能记成功）。
// TestModelConnection_TestTimeout504 asserts the timeout classification.
func TestModelConnection_TestTimeout504(t *testing.T) {
	t.Parallel()
	// 处理函数挂起到客户端超时：优先等请求上下文结束，另有 release 兜底——Windows 上
	// `httptest.Server.Close` 需要处理函数先返回，不能只靠连接断开传导。
	// The handler waits for the request context and has a release fallback so Close
	// never blocks on a connection Go has not yet noticed as gone.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	env := newResourceEnv(t, func(d *ResourceAdminDeps) {
		d.ModelTestTimeout = 50 * time.Millisecond
	})
	env.publishModelConnection(t, "main", srv.URL, "m1")

	_, err := env.admin.TestModelConnection(context.Background(), resourceActor(), "main")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionTest})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status)
	require.Equal(t, resourceResultTimeout, rows[0].ResultCode)
}

// TestModelConnection_TestNotConfigured503：占位变量为空 → 操作 failed +
// `NOT_CONFIGURED`、503 分类（预写先于外部调用）。
// TestModelConnection_TestNotConfigured503 asserts the not-configured classification.
func TestModelConnection_TestNotConfigured503(t *testing.T) {
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		Body: `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`,
	})
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", srv.URL(), "m1")

	t.Setenv(resourceTestKeyEnv, "")
	_, err := env.admin.TestModelConnection(context.Background(), resourceActor(), "main")
	require.ErrorIs(t, err, core.ErrProviderNotConfigured)
	require.Contains(t, err.Error(), resourceTestKeyEnv)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionTest})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status)
	require.Equal(t, resourceResultNotConfigured, rows[0].ResultCode)
	require.Empty(t, srv.Requests(), "凭证缺失时不得发出任何请求")
}

// currentETag 取模型连接当前的条件 ETag（删除 / 修改的条件写期望值）。FR-081 起
// 条件 ETag 是内容 + 资源 UID + revision + 范围 + 凭据状态的规范哈希，**不是**
// 内容哈希本身——测试必须从与写路径同源的读取点取，不能自己拼。
// currentETag reads the conditional ETag from the same source the write paths use.
func (e *resourceEnv) currentETag(t *testing.T, name string) string {
	e.t.Helper()
	return e.currentETagOf(t, core.ResourceModelConnection, name)
}

// currentETagOf 取任意资源类型的条件 ETag（MCP / 技能的条件写同样走 FR-081 组成）。
// currentETagOf reads the conditional ETag for any resource kind.
func (e *resourceEnv) currentETagOf(t *testing.T, kind core.ResourceKind, name string) string {
	e.t.Helper()
	etag, ok := e.admin.CurrentETag(context.Background(), kind, name)
	require.True(e.t, ok, "资源必须存在且当前条件 ETag 可读")
	return etag
}

// seedRunReference 直接写一条 `runs` + `run_resource_refs`：引用视图只认未释放行，
// 运行对象本身由运行路径负责，这里只为引用闸门造真实行（不伪造仓储实现）。
// seedRunReference inserts a real run row plus one resource reference.
func (e *resourceEnv) seedRunReference(t *testing.T, runID string, version core.ResourceVersion, released bool) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.ToSlash(e.dbPath))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	ctx := context.Background()
	_, err = db.ExecContext(ctx,
		`INSERT INTO runs (run_id, agent, channel, user_id, trigger, status, iterations, tool_calls, started_at)
		 VALUES (?, 'writer', 'cli', 'tester', 'user', 'running', 0, 0, ?)`,
		runID, baselineClock().UnixMilli())
	require.NoError(t, err)

	var releasedAt any
	if released {
		releasedAt = baselineClock().UnixMilli()
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO run_resource_refs (run_id, resource_kind, resource_id, version_id, content_hash, released_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		runID, string(version.Kind), version.ResourceID, version.ID, version.ContentHash, releasedAt)
	require.NoError(t, err)
}

// writeArchivedAgent 在 `<root>/archive/<archive_id>/AGENT.md` 写入归档定义。
// writeArchivedAgent writes one archived agent definition under the workspace root.
func (e *resourceEnv) writeArchivedAgent(t *testing.T, archiveID, content string) {
	t.Helper()
	dir := filepath.Join(e.root, agentArchiveDirName, archiveID)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, authorDraftDefinitionPath), []byte(content), 0o600))
}

// agentEntryWithProvider 造一个已注册、引用指定 Provider 的装载条目。
// agentEntryWithProvider builds one registered loader entry referencing a provider.
func agentEntryWithProvider(name, provider string) core.AgentEntry {
	return core.AgentEntry{
		Name:       name,
		Registered: true,
		Profile: &core.Profile{
			Name:     name,
			Provider: core.ProviderRef{Name: provider, Model: "m1"},
		},
	}
}

// TestReferences_AgentAndRunUnion：引用视图是**联合**——活动 Agent、归档 Agent 与
// 未释放运行引用同时进入结果，名字去重且升序（同一输入两次调用同一顺序）。
// TestReferences_AgentAndRunUnion asserts the union of agent and run references.
func TestReferences_AgentAndRunUnion(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t, func(d *ResourceAdminDeps) {
		d.Agents = func() ([]core.AgentEntry, error) {
			return []core.AgentEntry{
				agentEntryWithProvider("writer", "main"),
				agentEntryWithProvider("reader", "backup"),
			}, nil
		}
	})
	version := env.publishModelConnection(t, "main", "https://a.example", "m1")
	env.seedRunReference(t, "run-union-1", version, false)
	env.writeArchivedAgent(t, "old-20260101-000000", `---
name: old
provider:
  name: main
  model: m1
---
`)

	refs, err := env.admin.References(context.Background(), core.ResourceModelConnection, "main")
	require.NoError(t, err)
	require.Equal(t, core.ResourceModelConnection, refs.Kind)
	require.Equal(t, "main", refs.ID)
	require.Equal(t, version.ID, refs.PublishedVersionID)
	require.Equal(t, []string{"archive:old-20260101-000000", "writer"}, refs.Agents)
	require.Equal(t, []string{"run-union-1"}, refs.ActiveRuns)
}

// TestReferences_ActiveRunRefs：运行引用只来自未释放的 `run_resource_refs` 行，
// 且点名 run_id（阶段检查锚点）。
// TestReferences_ActiveRunRefs is the stage anchor for unreleased run references.
func TestReferences_ActiveRunRefs(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	version := env.publishModelConnection(t, "main", "https://a.example", "m1")
	env.seedRunReference(t, "run-active-1", version, false)

	refs, err := env.admin.References(context.Background(), core.ResourceModelConnection, "main")
	require.NoError(t, err)
	require.Equal(t, []string{"run-active-1"}, refs.ActiveRuns)
	require.NotNil(t, refs.Agents)
	require.Empty(t, refs.Agents)
}

// TestReferences_DistinguishesReleasedRuns：已释放（released_at 非空）的历史行
// **不**计入引用——不得用"曾经跑过"当作当前占用。
// TestReferences_DistinguishesReleasedRuns ignores released references.
func TestReferences_DistinguishesReleasedRuns(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	version := env.publishModelConnection(t, "main", "https://a.example", "m1")
	env.seedRunReference(t, "run-released", version, true)
	env.seedRunReference(t, "run-open", version, false)

	refs, err := env.admin.References(context.Background(), core.ResourceModelConnection, "main")
	require.NoError(t, err)
	require.Equal(t, []string{"run-open"}, refs.ActiveRuns)
}

// TestReferences_DeletedResourceStillAnswerable：删除后引用视图仍可查询——历史版本
// 与引用证据保留，404 只属于"指针行不存在"。
// TestReferences_DeletedResourceStillAnswerable keeps deleted resources queryable.
func TestReferences_DeletedResourceStillAnswerable(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	version := env.publishModelConnection(t, "main", "https://a.example", "m1")
	require.NoError(t, env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main")))

	refs, err := env.admin.References(context.Background(), core.ResourceModelConnection, "main")
	require.NoError(t, err)
	require.Equal(t, version.ID, refs.PublishedVersionID)
	require.Equal(t, []string{}, refs.Agents)
	require.Equal(t, []string{}, refs.ActiveRuns)
}

// fakeBindingReferrer 是绑定投影反查的替身：记录被查询的 UID（断言"按身份不按名"），
// 可注入行集或错误。
// fakeBindingReferrer records the queried uid and returns the injected rows or error.
type fakeBindingReferrer struct {
	queried core.ResourceUID
	rows    []core.AgentResourceBinding
	err     error
}

// ReferencingAgents 返回注入的投影行。
// ReferencingAgents returns the injected projection rows.
func (f *fakeBindingReferrer) ReferencingAgents(_ context.Context, uid core.ResourceUID) ([]core.AgentResourceBinding, error) {
	f.queried = uid
	return f.rows, f.err
}

// fakeArchiveLister 是归档索引替身。
// fakeArchiveLister returns the injected archive rows.
type fakeArchiveLister struct {
	records []core.AgentArchiveRecord
	err     error
}

// ListArchives 返回注入的归档行。
// ListArchives returns the injected archive rows.
func (f fakeArchiveLister) ListArchives(context.Context) ([]core.AgentArchiveRecord, error) {
	return f.records, f.err
}

// TestReferences_BoundAgentsFromProjection：引用视图的 `bound_agents` 只来自绑定投影，
// 按指针 UID 反查（不按名称），按 `agent_uid` 去重，归档标志取自未恢复的归档行；
// `resource_uid` 与指针一致——同名重建后旧绑定不会混进新身份。
// TestReferences_BoundAgentsFromProjection pins the UID-keyed projection union.
func TestReferences_BoundAgentsFromProjection(t *testing.T) {
	t.Parallel()
	referrer := &fakeBindingReferrer{}
	restoredAt := int64(1)
	archives := fakeArchiveLister{records: []core.AgentArchiveRecord{
		{ArchiveID: "b-1", AgentName: "archived-b", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"},
		{ArchiveID: "c-1", AgentName: "restored-c", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C", RestoredAt: &restoredAt},
	}}
	env := newResourceEnv(t, func(d *ResourceAdminDeps) {
		d.Bindings = referrer
		d.Archives = archives
	})
	env.publishModelConnection(t, "main", "https://a.example", "m1")
	uid := core.ResourceUID(env.uidOf(core.ResourceModelConnection, "main"))
	referrer.rows = []core.AgentResourceBinding{
		{
			AgentName: "agent-a", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
			Kind: core.BindingModelConnection, RefName: "main", ResourceUID: uid,
		},
		// 同一 Agent 的两条行（不同 kind）在视图里只算一个引用者。
		{
			AgentName: "agent-a", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
			Kind: core.BindingNotifyChannel, RefName: "main", ResourceUID: uid,
		},
		{
			AgentName: "archived-b", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B",
			Kind: core.BindingModelConnection, RefName: "main", ResourceUID: uid,
		},
	}

	refs, err := env.admin.References(context.Background(), core.ResourceModelConnection, "main")
	require.NoError(t, err)
	require.Equal(t, uid, referrer.queried, "必须按指针 UID 反查绑定投影，不得按名称")
	require.Equal(t, uid, refs.ResourceUID)
	require.Equal(t, []core.ResourceBoundAgent{
		{Agent: "agent-a", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"},
		{Agent: "archived-b", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B", Archived: true},
	}, refs.BoundAgents)
}

// TestReferences_BindingProjectionFailureFailsClosed：投影读取失败不得降级成"没有
// 绑定"——引用视图是排障证据，缺证据要报错（fail-closed）。
// TestReferences_BindingProjectionFailureFailsClosed refuses to answer without evidence.
func TestReferences_BindingProjectionFailureFailsClosed(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t, func(d *ResourceAdminDeps) {
		d.Bindings = &fakeBindingReferrer{err: errors.New("投影不可读")}
	})
	env.publishModelConnection(t, "main", "https://a.example", "m1")

	_, err := env.admin.References(context.Background(), core.ResourceModelConnection, "main")
	require.Error(t, err)
	require.Contains(t, err.Error(), "绑定投影")
}

// TestResourceDelete_ReferencedByAgent：活动 Agent 引用 → 400 + `data.agents` 点名、
// 操作 failed、文件与指针零变化、不新增版本（约束 11 / 图 G）。
// TestResourceDelete_ReferencedByAgent is the card's agent-reference gate.
func TestResourceDelete_ReferencedByAgent(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t, func(d *ResourceAdminDeps) {
		d.Agents = func() ([]core.AgentEntry, error) {
			return []core.AgentEntry{agentEntryWithProvider("writer", "main")}, nil
		}
	})
	version := env.publishModelConnection(t, "main", "https://a.example", "m1")
	before := env.readConfig()

	err := env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main"))
	require.Error(t, err)

	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, []string{"writer"}, adminErr.Data["agents"])
	require.Equal(t, []string{}, adminErr.Data["active_runs"])
	require.Contains(t, adminErr.Message, "writer")

	require.Equal(t, before, env.readConfig(), "被引用时文件必须逐字节不变")
	require.Nil(t, env.pointer("main").DeletedAt)
	require.Len(t, env.versionsOf("main"), 1)
	require.Equal(t, version.ID, env.pointer("main").PublishedVersionID)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionDelete})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status)
	require.Equal(t, agentResultBadRequest, rows[0].ResultCode)
}

// TestResourceDelete_UnparseableAgentBlocks：Agent 定义解析失败按"可能引用"计入，
// 点名为 `<agent>（unparseable）`——宁可多拦，不放行不确定的删除。
// TestResourceDelete_UnparseableAgentBlocks fails closed on unparseable definitions.
func TestResourceDelete_UnparseableAgentBlocks(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t, func(d *ResourceAdminDeps) {
		d.Agents = func() ([]core.AgentEntry, error) {
			return []core.AgentEntry{{Name: "broken", RejectReason: "frontmatter 不可解析"}}, nil
		}
	})
	env.publishModelConnection(t, "main", "https://a.example", "m1")

	err := env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main"))
	require.Error(t, err)

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, []string{"broken" + unparseableAgentMarker}, adminErr.Data["agents"])
}

// TestResourceDelete_ArchivedAgentBlocks：归档 Agent 同样参与引用闸门；归档定义
// 不可解析时点名 `archive:<id>`，不引用的归档与缺 AGENT.md 的归档目录不计入。
// TestResourceDelete_ArchivedAgentBlocks scans archives and fails closed on bad files.
func TestResourceDelete_ArchivedAgentBlocks(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")
	env.writeArchivedAgent(t, "old-20260101-000000", `---
name: old
provider:
  name: main
  model: m1
---
`)
	env.writeArchivedAgent(t, "broken-20260101-000001", "不是 frontmatter\n")
	env.writeArchivedAgent(t, "other-20260101-000002", `---
name: other
provider:
  name: backup
  model: m1
---
`)
	// 缺 AGENT.md 的归档目录不可能引用任何资源：跳过而不是按损坏计入。
	require.NoError(t, os.MkdirAll(filepath.Join(env.root, agentArchiveDirName, "empty-20260101-000003"), 0o700))

	err := env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main"))
	require.Error(t, err)

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t,
		[]string{"archive:broken-20260101-000001", "archive:old-20260101-000000"},
		adminErr.Data["agents"])
}

// TestResourceDelete_ActiveRunRef：未释放运行引用 → 400 + `data.active_runs` 点名、
// 操作 failed、零写入。
// TestResourceDelete_ActiveRunRef is the card's active-run reference gate.
func TestResourceDelete_ActiveRunRef(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	version := env.publishModelConnection(t, "main", "https://a.example", "m1")
	env.seedRunReference(t, "run-blocking", version, false)
	before := env.readConfig()

	err := env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main"))
	require.Error(t, err)

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, []string{}, adminErr.Data["agents"])
	require.Equal(t, []string{"run-blocking"}, adminErr.Data["active_runs"])

	require.Equal(t, before, env.readConfig())
	require.Nil(t, env.pointer("main").DeletedAt)
	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionDelete})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status)
}

// TestResourceDelete_NoNewVersion：删除只置 `deleted_at`，不新增版本、不改写历史；
// 定义文件里目标块被移除、其余条目与块外字节保留；操作行成功可查（约束 12）。
// TestResourceDelete_NoNewVersion asserts deletion writes no version and keeps history.
func TestResourceDelete_NoNewVersion(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	version := env.publishModelConnection(t, "main", "https://a.example", "m1")

	require.NoError(t, env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main")))

	pointer := env.pointer("main")
	require.NotNil(t, pointer.DeletedAt)
	require.Equal(t, version.ID, pointer.PublishedVersionID)
	require.Len(t, env.versionsOf("main"), 1)
	config := env.readConfig()
	require.NotContains(t, config, "name: main")
	require.Contains(t, config, "name: backup", "兄弟条目必须保留")
	require.Contains(t, config, "# 顶部注释", "块外字节必须逐字保留")
	require.Contains(t, config, "# 底部注释")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionDelete})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	require.Equal(t, "main", rows[0].TargetID)
}

// TestResourceRecreate_VersionContinues：删除后同名重建 = **新资源实例**（FR-066 /
// SC-017，取代旧卡的"继续递增"）：新建生成新 UID、版本号从 1 重新计；前一代的版本与
// 墓碑仍按身份保留，按名快照能看到两代（用例名沿用 stage 的固定名）。
// A same-name rebuild is a new instance: fresh UID, version numbers restart at 1, and the
// previous generation's versions stay readable under its own identity.
func TestResourceRecreate_VersionContinues(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	first := env.publishModelConnection(t, "main", "https://a.example", "m1")
	require.Equal(t, 1, first.VersionNo)
	require.NoError(t, env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main")))

	second, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "main",
			BaseURL:      "https://b.example",
			APIKey:       "${" + resourceTestKeyEnv + "}",
			APIKeySet:    true,
			DefaultModel: "m2",
			Create:       true,
		})
	require.NoError(t, err)
	require.Equal(t, 1, second.VersionNo, "同名重建是新实例：版本号从 1 重新计")
	require.NotEqual(t, first.ResourceUID, second.ResourceUID, "同名重建必须生成新资源 UID")

	pointer := env.pointer("main")
	require.Nil(t, pointer.DeletedAt)
	require.Equal(t, second.ID, pointer.PublishedVersionID)
	require.Equal(t, second.ResourceUID, pointer.ResourceUID)

	// 按名快照含两代（新在前），但两代版本号都是 1——身份维度的读法各回各的版本。
	versions := env.versionsOf("main")
	require.Len(t, versions, 2, "历史版本只追加不删除")
	require.Equal(t, 1, versions[0].VersionNo)
	require.Equal(t, second.ID, versions[0].ID)
	require.Equal(t, 1, versions[1].VersionNo)
	require.Equal(t, first.ID, versions[1].ID)

	oldVersions, err := env.store.VersionsByUID(context.Background(),
		core.ResourceModelConnection, first.ResourceUID)
	require.NoError(t, err)
	require.Len(t, oldVersions, 1, "前一代身份的历史版本仍可查")
	require.Equal(t, first.ID, oldVersions[0].ID)
}

// TestResourceDelete_RebuildFailureRestoresFile：移除后的候选映射表构建失败 →
// 身份检查恢复文件、指针不动、操作 failed（500 分类，约束 36）。
// TestResourceDelete_RebuildFailureRestoresFile restores the file and keeps the pointer.
func TestResourceDelete_RebuildFailureRestoresFile(t *testing.T) {
	const siblingEnv = "BASALT_RESOURCE_SIBLING_KEY"
	t.Setenv(siblingEnv, "sibling-sentinel")
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")
	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:         "sibling",
			BaseURL:      "https://sibling.example",
			APIKey:       "${" + siblingEnv + "}",
			APIKeySet:    true,
			DefaultModel: "m2",
			Create:       true,
		})
	require.NoError(t, err)
	before := env.readConfig()

	// 撤掉兄弟条目的凭证：删除 main 时必须重建"其余已发布资源"的映射表，构建失败。
	t.Setenv(siblingEnv, "")
	err = env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main"))
	require.Error(t, err)

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.NotEmpty(t, adminErr.Data["operation_id"])
	require.Equal(t, before, env.readConfig(), "重建失败必须恢复原字节")
	require.Nil(t, env.pointer("main").DeletedAt, "指针不得改变")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionDelete})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status)
	require.Equal(t, lifecycleResultInternal, rows[0].ResultCode)
}

// TestResourceDelete_SkipsTargetEnvResolution：目标自己的凭证已撤掉也不能挡住自己的
// 删除——候选映射表在解析该条目前就跳过目标（约束 15 / 图 G）。
// TestResourceDelete_SkipsTargetEnvResolution skips the target before env resolution.
func TestResourceDelete_SkipsTargetEnvResolution(t *testing.T) {
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")
	t.Setenv(resourceTestKeyEnv, "")

	require.NoError(t, env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main")))
	require.NotNil(t, env.pointer("main").DeletedAt)
	require.NotContains(t, env.readConfig(), "name: main")
}

// TestResourceDelete_StaleETag412：`If-Match` 缺失或不匹配 → 412 且零写入零操作行
// （删除路径的缺失归入 412，与发布路径的"空串 = 未声明"不同）。
// TestResourceDelete_StaleETag412 asserts the conditional-write gate without an op row.
func TestResourceDelete_StaleETag412(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")
	before := env.readConfig()
	rowsBefore := env.operationCount()
	current := env.currentETag(t, "main")

	for _, ifMatch := range []string{"", `"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"`} {
		err := env.admin.Delete(context.Background(), resourceActor(),
			core.ResourceModelConnection, "main", ifMatch,
			env.uidOf(core.ResourceModelConnection, "main"))
		require.ErrorIs(t, err, core.ErrPreconditionFailed)

		var adminErr *core.AdminError
		require.ErrorAs(t, err, &adminErr)
		require.Equal(t, current, adminErr.Data["current_etag"])
	}

	require.Equal(t, before, env.readConfig())
	require.Equal(t, rowsBefore, env.operationCount(), "412 不得写操作行")
}

// TestResourceDelete_UIDMismatch409 —— 契约要求删除显式携带 `resource_uid`：缺失、
// 形状非法、或与当前指针不一致都回 409 `ErrResourceUIDMismatch`，`data.resource_uid`
// 给出当前身份供客户端重新读取；零写入、零操作行——不按名称猜目标（R1 / R2 裁决：
// 同名重建后旧页面提交的删除不能删到新实例）。
// 逆改点：缺失按当前指针补齐 / 非法形状回 400 / 不一致按名称执行 → 各自断言红。
func TestResourceDelete_UIDMismatch409(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", "https://a.example", "m1")
	before := env.readConfig()
	rowsBefore := env.operationCount()
	uid := env.uidOf(core.ResourceModelConnection, "main")
	other, err := core.NewResourceUID()
	require.NoError(t, err)

	for _, resourceUID := range []string{"", "rs_not-a-uid", string(other)} {
		err := env.admin.Delete(context.Background(), resourceActor(),
			core.ResourceModelConnection, "main", env.currentETag(t, "main"), resourceUID)
		require.ErrorIs(t, err, core.ErrResourceUIDMismatch, "resource_uid=%q", resourceUID)

		var adminErr *core.AdminError
		require.ErrorAs(t, err, &adminErr)
		require.Equal(t, uid, adminErr.Data["resource_uid"], "必须回当前身份")
	}

	require.Equal(t, before, env.readConfig(), "身份不一致不得改文件")
	require.Equal(t, rowsBefore, env.operationCount(), "身份不一致不得写操作行")
	require.Nil(t, env.pointer("main").DeletedAt)
}

// TestResourceDelete_UnknownName404：资源不存在 / 已删除 → 404，不写操作行。
// TestResourceDelete_UnknownName404 maps a missing resource to 404.
func TestResourceDelete_UnknownName404(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	rowsBefore := env.operationCount()

	err := env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "ghost", `"sha256:ffff"`, "")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	require.Equal(t, rowsBefore, env.operationCount())
}

// TestResourceDelete_UnsupportedKind：三类资源（模型连接 / MCP / 技能）都有各自的
// 删除编排；表外类型（如 `agent`——Agent 归档走生命周期端口，不经资源删除）显式
// 拒绝 400，不静默并入某条资源路径、不落操作行。
// TestResourceDelete_UnsupportedKind rejects kinds outside the three-value literal set.
func TestResourceDelete_UnsupportedKind(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	rowsBefore := env.operationCount()

	err := env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceKind("agent"), "writer", `"sha256:ffff"`, "")
	require.Error(t, err)

	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Equal(t, rowsBefore, env.operationCount())
}

// corruptVersionDefinition 直接改写版本行的定义原文：脱敏闸门的负向用例需要一行
// "存储里已经有明文"的数据，正常写路径拒绝这种输入，只能绕过编排层造行（不伪造
// 仓储实现，仍然是真实的 SQLite 行）。
// corruptVersionDefinition rewrites the stored definition of one version row so the
// redaction gate has plaintext to reject; the normal write path refuses such input.
func (e *resourceEnv) corruptVersionDefinition(t *testing.T, versionID int64, definition string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.ToSlash(e.dbPath))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = db.ExecContext(context.Background(),
		`UPDATE resource_versions SET definition_yaml = ? WHERE id = ?`, definition, versionID)
	require.NoError(t, err)
}

// seedSkillVersion 直接插入一条技能版本行（技能写面在 T049 才落地）：版本详情只读
// `manifest_json`，用真实行验证清单投影与"definition_yaml 恒空"。
// seedSkillVersion inserts one real skill version row so the detail path can project
// its manifest before the skill write surface lands.
func (e *resourceEnv) seedSkillVersion(t *testing.T, name, manifestJSON string) core.ResourceVersion {
	t.Helper()
	ctx := context.Background()
	actor := resourceActor()
	opID := core.NewOperationID(baselineClock())
	require.NoError(t, e.ops.BeginOperation(ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       core.ActionSkillCreate,
		TargetKind:   "skill",
		TargetID:     name,
		ActorSubject: actor.Subject,
		AuthMethod:   string(actor.Method),
		Source:       actor.Source,
		CreatedAt:    baselineClock(),
	}))

	db, err := sql.Open("sqlite", filepath.ToSlash(e.dbPath))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	res, err := db.ExecContext(ctx,
		`INSERT INTO resource_versions
		   (resource_kind, resource_uid, resource_id, version_no, content_hash, definition_yaml,
		    manifest_json, storage_path, created_at, operation_id)
		 VALUES ('skill', ?, ?, 1, ?, NULL, ?, ?, ?, ?)`,
		string(fixtureResourceUID(t, core.ResourceSkill, name)), name,
		strings.Repeat("f", 64), manifestJSON, "skills/.versions/"+name+"/1",
		baselineClock().UnixMilli(), opID)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return core.ResourceVersion{
		ID: id, Kind: core.ResourceSkill, ResourceUID: fixtureResourceUID(t, core.ResourceSkill, name),
		ResourceID: name, VersionNo: 1,
		ContentHash: strings.Repeat("f", 64), ManifestJSON: manifestJSON,
		StoragePath: "skills/.versions/" + name + "/1",
		CreatedAt:   baselineClock().UnixMilli(), OperationID: opID,
	}
}

// TestResourceVersionDetail_Projection：版本详情把版本行 + 定义原文 + 指针状态 +
// 撤回来源投影成 `core.ResourceVersionDetail`——发布前旧版本两个状态位为 false，
// 撤回产生的新版本 published / applied 为 true，来源版本从创建操作派生（约束 14）。
// TestResourceVersionDetail_Projection pins the published/applied/reverted projection.
func TestResourceVersionDetail_Projection(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")

	first, err := env.admin.VersionDetail(context.Background(), v1.ID)
	require.NoError(t, err)
	require.Equal(t, v1.ID, first.Version.ID)
	require.Equal(t, v1.DefinitionYAML, first.DefinitionYAML, "definition_yaml 是存储的块原文")
	require.True(t, first.Published, "创建即发布，v1 当前就是指针目标")
	require.True(t, first.Applied, "模型连接发布即生效")
	require.Nil(t, first.RevertedFromVersionID)
	require.Empty(t, first.Manifest, "模型连接没有快照清单")

	result, err := env.admin.Publish(context.Background(), resourceActor(), core.ResourcePublishRequest{
		Kind: core.ResourceModelConnection, ID: "main", VersionID: &v1.ID,
	})
	require.NoError(t, err)
	require.Equal(t, 2, result.Version.VersionNo, "撤回创建新版本")

	second, err := env.admin.VersionDetail(context.Background(), result.Version.ID)
	require.NoError(t, err)
	require.Equal(t, v1.DefinitionYAML, second.DefinitionYAML, "撤回版本内容来自来源版本")
	require.True(t, second.Published)
	require.True(t, second.Applied, "模型连接发布即生效")
	require.NotNil(t, second.RevertedFromVersionID)
	require.Equal(t, v1.ID, *second.RevertedFromVersionID)

	// 撤回后旧版本不再是发布 / 生效指针的目标，但仍然可读。
	superseded, err := env.admin.VersionDetail(context.Background(), v1.ID)
	require.NoError(t, err)
	require.False(t, superseded.Published)
	require.False(t, superseded.Applied)
	require.Equal(t, 2, env.operationCount(), "读取详情不得写操作行（创建 + 撤回各一行）")
}

// TestResourceVersionDetail_DeletedResourceReadable：删除只打 `deleted_at`，历史版本
// 仍可读——详情继续返回定义原文与指针状态，`VersionByID` 不过滤删除标记。
// TestResourceVersionDetail_DeletedResourceReadable keeps deleted resources readable.
func TestResourceVersionDetail_DeletedResourceReadable(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	require.NoError(t, env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main")))

	detail, err := env.admin.VersionDetail(context.Background(), v1.ID)
	require.NoError(t, err)
	require.Equal(t, v1.DefinitionYAML, detail.DefinitionYAML)
	require.True(t, detail.Published, "指针行保留，发布版本仍是 v1")
	require.True(t, detail.Applied)
}

// TestResourceVersionDetail_NotFound：≤0 与不存在的版本号一律 `core.ErrResourceNotFound`
// （web 映射 404，不做 400 区分；不写操作行）。
// TestResourceVersionDetail_NotFound maps non-positive and missing ids to not-found.
func TestResourceVersionDetail_NotFound(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	rowsBefore := env.operationCount()
	for _, id := range []int64{0, -3, 999999} {
		_, err := env.admin.VersionDetail(context.Background(), id)
		require.ErrorIs(t, err, core.ErrResourceNotFound, "id=%d", id)
	}
	require.Equal(t, rowsBefore, env.operationCount())
}

// TestResourceVersionDetail_RedactionFailClosed：脱敏闸门逐形态 fail-closed——凭证键
// 明文、`env` 明文值与 URL userinfo 都拒绝返回；错误与 WARN / ERROR 日志都不含命中
// 的值，失败时 `definition_yaml` 为空（不返回部分内容）。占位合法的两种落库形态
// （块式列表 / 顶层条目映射）都放行。
// TestResourceVersionDetail_RedactionFailClosed walks every credential shape and
// proves the offending value never reaches the error, the log or a partial detail.
func TestResourceVersionDetail_RedactionFailClosed(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	ctx := context.Background()

	leaky := []struct {
		name       string
		definition string
	}{
		{
			name: "api_key plaintext in block form",
			definition: "- name: main\n  base_url: https://a.example\n" +
				"  api_key: sk-live-plaintext\n  default_model: m1\n",
		},
		{
			name: "token plaintext in mapping form",
			definition: "name: main\nbase_url: https://a.example\n" +
				"token: sk-live-plaintext\ndefault_model: m1\n",
		},
		{
			name:       "credential value is not a scalar",
			definition: "name: main\napi_key:\n  nested: sk-live-plaintext\n",
		},
		{
			name:       "env value plaintext",
			definition: "name: main\ntransport: stdio\ncommand: fetch\nenv:\n  SERVICE_TOKEN: sk-live-plaintext\n",
		},
		{
			name:       "url userinfo",
			definition: "name: main\nbase_url: https://user:sk-live-plaintext@a.example\napi_key: ${BASALT_RESOURCE_TEST_KEY}\n",
		},
		{
			name:       "unparsable definition",
			definition: "name: [main\napi_key: sk-live-plaintext\n",
		},
	}
	for _, tc := range leaky {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env.corruptVersionDefinition(t, v1.ID, tc.definition)
			detail, err := env.admin.VersionDetail(ctx, v1.ID)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "sk-live-plaintext", "错误不得回显命中的值")
			require.NotContains(t, err.Error(), "user:", "错误不得回显 URL userinfo")
			require.Empty(t, detail.DefinitionYAML, "失败不得返回部分内容")
			require.NotContains(t, env.logs.String(), "sk-live-plaintext", "日志不得记正文")
			require.Contains(t, env.logs.String(), "资源版本定义未脱敏")
		})
	}

	valid := []string{
		// 块式列表片段（管理写路径形态）。
		"- name: main\n  base_url: https://a.example\n" +
			"  api_key: ${BASALT_RESOURCE_TEST_KEY}\n  default_model: m1\n",
		// 顶层条目映射（基线写入形态）。
		"name: main\nbase_url: https://a.example\n" +
			"api_key: ${BASALT_RESOURCE_TEST_KEY}\ndefault_model: m1\n",
	}
	for _, definition := range valid {
		env.corruptVersionDefinition(t, v1.ID, definition)
		detail, err := env.admin.VersionDetail(ctx, v1.ID)
		require.NoError(t, err, "definition=%q", definition)
		require.Equal(t, definition, detail.DefinitionYAML)
	}
}

// TestResourceVersionDetail_SkillManifest：技能版本返回解析后的快照清单，
// `definition_yaml` 恒为空串（定义在不可变目录而不在版本行）。
// TestResourceVersionDetail_SkillManifest projects the skill snapshot manifest.
func TestResourceVersionDetail_SkillManifest(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	manifest := `[{"path":"SKILL.md","sha256":"` + strings.Repeat("a", 64) + `","size":12}]`
	version := env.seedSkillVersion(t, "writing", manifest)

	detail, err := env.admin.VersionDetail(context.Background(), version.ID)
	require.NoError(t, err)
	require.Equal(t, core.ResourceSkill, detail.Version.Kind)
	require.Empty(t, detail.DefinitionYAML, "技能 definition_yaml 恒为空串")
	require.Equal(t, []core.ResourceManifestFile{
		{Path: "SKILL.md", SHA256: strings.Repeat("a", 64), Size: 12},
	}, detail.Manifest)
}

// TestResourceAdmin_Versions_Projection：版本列表按"新 → 旧"返回，并把指针状态与撤回
// 来源逐行派生——撤回产生的新版本 `published` / `applied` 为 true、来源版本为 v1，旧版本
// 两个状态位为 false；读取不写操作行（约束 14）。
// TestResourceAdmin_Versions_Projection pins the list order, pointer flags and revert
// provenance; listing never writes an operation row.
func TestResourceAdmin_Versions_Projection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	result, err := env.admin.Publish(ctx, resourceActor(), core.ResourcePublishRequest{
		Kind: core.ResourceModelConnection, ID: "main", VersionID: &v1.ID,
	})
	require.NoError(t, err)

	rowsBefore := env.operationCount()
	versions, err := env.admin.Versions(ctx, core.ResourceModelConnection, "main")
	require.NoError(t, err)
	require.Len(t, versions, 2)
	require.Equal(t, result.Version.ID, versions[0].ID, "最新版本在前")
	require.Equal(t, v1.ID, versions[1].ID)
	require.Equal(t, rowsBefore, env.operationCount(), "读取版本列表不得写操作行")

	// 指针状态与撤回来源由端口用的同一派生入口给出（`versionDetails`）。
	details, err := env.admin.versionDetails(ctx, core.ResourceModelConnection, "main")
	require.NoError(t, err)
	require.Len(t, details, 2)
	require.True(t, details[0].Published)
	require.True(t, details[0].Applied)
	require.NotNil(t, details[0].RevertedFromVersionID)
	require.Equal(t, v1.ID, *details[0].RevertedFromVersionID)
	require.False(t, details[1].Published, "旧版本不再是指针目标")
	require.False(t, details[1].Applied)
	require.Nil(t, details[1].RevertedFromVersionID)
}

// TestResourceAdmin_Versions_DeletedResourceReadable：删除只打 `deleted_at`，指针行保留，
// 历史版本列表仍可读——与版本详情同一口径（约束 14）。
// TestResourceAdmin_Versions_DeletedResourceReadable keeps deleted history listable.
func TestResourceAdmin_Versions_DeletedResourceReadable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	require.NoError(t, env.admin.Delete(ctx, resourceActor(),
		core.ResourceModelConnection, "main", env.currentETag(t, "main"),
		env.uidOf(core.ResourceModelConnection, "main")))

	versions, err := env.admin.Versions(ctx, core.ResourceModelConnection, "main")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	require.Equal(t, v1.ID, versions[0].ID)

	details, err := env.admin.versionDetails(ctx, core.ResourceModelConnection, "main")
	require.NoError(t, err)
	require.True(t, details[0].Published, "指针行保留，发布版本仍是 v1")
	require.True(t, details[0].Applied)
}

// TestResourceAdmin_Versions_NotFound：指针行不存在（含从未发布）→ `core.ErrResourceNotFound`，
// 与版本列表的 404 同一口径；读取不写操作行。
// TestResourceAdmin_Versions_NotFound maps an absent pointer to not-found.
func TestResourceAdmin_Versions_NotFound(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	rowsBefore := env.operationCount()

	_, err := env.admin.Versions(context.Background(), core.ResourceModelConnection, "ghost")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	require.Equal(t, rowsBefore, env.operationCount())
}

// TestResourceAdmin_Versions_InvalidInput：未知资源类型与含路径分隔符的标识都在读取前
// 被拒绝（纯校验，不写操作行、不触达存储语义）。
// TestResourceAdmin_Versions_InvalidInput rejects unknown kinds and path-like ids.
func TestResourceAdmin_Versions_InvalidInput(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	rowsBefore := env.operationCount()

	_, err := env.admin.Versions(context.Background(), core.ResourceKind("provider"), "main")
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)

	_, err = env.admin.Versions(context.Background(), core.ResourceModelConnection, "a/b")
	require.ErrorAs(t, err, &invalid)
	require.Equal(t, rowsBefore, env.operationCount())
}

// ---------------------------------------------------------------------------
// US-12 / US6（T044–T048）：MCP 写面——创建 / 更新 / 撤回 / 删除 / 测试端点
// ---------------------------------------------------------------------------

// readMCPConfig 读 `mcp_servers.yaml` 当前字节。
// readMCPConfig returns the MCP definition file's current bytes.
func (e *resourceEnv) readMCPConfig() string {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.root, mcpServersFileName))
	require.NoError(e.t, err)
	return string(raw)
}

// writeMCPConfig 覆盖 `mcp_servers.yaml`（模拟既有文件 / 手工编辑）。
// writeMCPConfig overwrites the MCP definition file to simulate an existing one.
func (e *resourceEnv) writeMCPConfig(content string) {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(filepath.Join(e.root, mcpServersFileName), []byte(content), 0o600))
}

// pointerOf 取任意资源类型的发布指针。
// pointerOf returns the publish pointer for any resource kind.
func (e *resourceEnv) pointerOf(kind core.ResourceKind, name string) core.ResourcePointer {
	e.t.Helper()
	ptr, err := e.store.Pointer(context.Background(), kind, name)
	require.NoError(e.t, err)
	return ptr
}

// uidOf 取名称当前指针的资源身份——删除按契约必须显式携带 `resource_uid`，测试
// 一律从当前指针取真值，不手写字面量（手写的合法 UID 只会测到形状而不是身份）。
// uidOf returns the current pointer's uid, so delete tests always carry the real
// identity instead of a hand-written literal.
func (e *resourceEnv) uidOf(kind core.ResourceKind, name string) string {
	e.t.Helper()
	ptr := e.pointerOf(kind, name)
	require.NotEmpty(e.t, ptr.ResourceUID, "指针必须带资源身份")
	return string(ptr.ResourceUID)
}

// versionsOfKind 取任意资源类型的全部版本（新到旧）。
// versionsOfKind returns every version of a resource, newest first.
func (e *resourceEnv) versionsOfKind(kind core.ResourceKind, name string) []core.ResourceVersion {
	e.t.Helper()
	versions, err := e.store.Versions(context.Background(), kind, name)
	require.NoError(e.t, err)
	return versions
}

// setApplied 直接改写指针的实际生效版本：supervisor 的 applied 回写在 T049 接线，
// 本任务用一行真实 SQLite 数据模拟"上一次成功连接已经应用该版本"。
// setApplied rewrites the applied pointer to simulate a previous successful connect.
func (e *resourceEnv) setApplied(t *testing.T, kind core.ResourceKind, name string, versionID int64) {
	t.Helper()
	db := openRead(t, e.dbPath)
	_, err := db.ExecContext(context.Background(),
		`UPDATE resource_pointers SET applied_version_id = ? WHERE resource_kind = ? AND resource_id = ?`,
		versionID, string(kind), name)
	require.NoError(t, err)
}

// createMCPStdio 经真实写路径建一条 stdio MCP server（新建即发布，applied 留空）。
// Env 值保持精确 `${VAR}`：候选校验不得解析环境变量。
// createMCPStdio creates one stdio server through the real write path; the env
// value stays an exact placeholder because candidate validation never resolves it.
func (e *resourceEnv) createMCPStdio(t *testing.T, name, command string) core.ResourceVersion {
	t.Helper()
	version, err := e.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:        name,
		Transport:   "stdio",
		Command:     command,
		Args:        []string{"--root", "./workspace"},
		Env:         map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"},
		Credentials: envOnlyCredentials(map[string]string{"MCP_TEST_TOKEN": "${MCP_TEST_TOKEN}"}),
		Create:      true,
	})
	require.NoError(t, err)
	return version
}

// envOnlyCredentials 是 14D「来源显式」的**机械适配**：把给定占位符全部声明成
// `mode=env`（14D 之前进程环境是唯一来源）。判据面不变——这些用例断言的是闸门、诊断与
// 测试行为，不是来源选择；裁决依据是 14D 设计 §2.3.3「未声明的槽位合并后仍无声明 →
// 400」与冻结契约 `McpServerUpsert.credentials`。请求顺序不参与合并（按槽位名索引）。
// envOnlyCredentials declares every placeholder as env, the only pre-14D source; the
// assertions themselves are untouched. Order is irrelevant because Plan indexes by key.
func envOnlyCredentials(env map[string]string) []MCPCredentialRequest {
	decls := make([]MCPCredentialRequest, 0, len(env))
	for key := range env {
		decls = append(decls, MCPCredentialRequest{Key: key, Mode: "env"})
	}
	return decls
}

// withSupervisor 在资源写面夹具上装配 supervisor 实例。
// withSupervisor wires the supervisor into the resource fixture.
func withSupervisor(supervisor *MCPSupervisor) func(*ResourceAdminDeps) {
	return func(d *ResourceAdminDeps) { d.Supervisor = supervisor }
}

// TestMCPCreate_PublishesPendingRestart：新建 MCP server 即发布，但**不**生效——
// 指针的 applied 留空、待重启为真；条目落进 `servers:` 块，另一份定义文件零变化
// （约束 9 / 19）。
// TestMCPCreate_PublishesPendingRestart asserts publish-without-apply on create.
func TestMCPCreate_PublishesPendingRestart(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.createMCPStdio(t, "fs", "allowed-mcp")

	require.Equal(t, 1, v1.VersionNo)
	require.Equal(t, core.ResourceMCPServer, v1.Kind)
	require.Equal(t, "fs", v1.ResourceID)
	require.Contains(t, v1.DefinitionYAML, "transport: stdio")
	require.Contains(t, v1.DefinitionYAML, "command: allowed-mcp")

	ptr := env.pointerOf(core.ResourceMCPServer, "fs")
	require.Equal(t, v1.ID, ptr.PublishedVersionID)
	require.Nil(t, ptr.AppliedVersionID, "发布不等于生效：applied 等下一次成功连接")
	require.Nil(t, ptr.DeletedAt)
	require.True(t, mcpRestartPending(ptr), "待重启 = 发布版本 ≠ 应用版本")

	config := env.readMCPConfig()
	require.Contains(t, config, "servers:")
	require.Contains(t, config, "name: fs")
	require.Contains(t, config, "command: allowed-mcp")
	require.Contains(t, config, "MCP_TEST_TOKEN: ${MCP_TEST_TOKEN}", "env 值原样保留占位")
	require.Equal(t, initialResourceConfig, env.readConfig(), "MCP 写入不得触碰 basalt.yaml")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerCreate})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	require.Equal(t, core.TargetKindMCPServer, rows[0].TargetKind)
	require.Equal(t, "fs", rows[0].TargetID)
}

// TestMCP_StaleETagZeroWrite：MCP 更新的 If-Match 与当前发布版本不等 → 412、
// `data.current_etag` 给出当前值、零写入、零操作行（约束 3 / 1a）。
// TestMCP_StaleETagZeroWrite asserts the 412 shape for the MCP update path.
func TestMCP_StaleETagZeroWrite(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.createMCPStdio(t, "fs", "allowed-mcp")
	before := env.readMCPConfig()
	rowsBefore := env.operationCount()

	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      "fs",
		Transport: "stdio",
		Command:   "allowed-mcp",
		Args:      []string{"--other"},
		IfMatch:   `"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"`,
	})
	require.ErrorIs(t, err, core.ErrPreconditionFailed)

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Equal(t, env.currentETagOf(t, core.ResourceMCPServer, "fs"), adminErr.Data["current_etag"])
	require.NotContains(t, fmt.Sprint(adminErr.Data), "command", "响应不得回显定义正文")

	require.Equal(t, before, env.readMCPConfig())
	require.Equal(t, rowsBefore, env.operationCount())
}

// TestMCPPublish_KeepsApplied：撤回式发布（用旧版本字节建新版本）只推进 published；
// applied 保持旧值，待重启为真；幂等重发同一结论（约束 9 / 10）。
// TestMCPPublish_KeepsApplied asserts that publishing never advances applied.
func TestMCPPublish_KeepsApplied(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	v1 := env.createMCPStdio(t, "fs", "allowed-mcp")
	env.setApplied(t, core.ResourceMCPServer, "fs", v1.ID)

	result, err := env.admin.Publish(context.Background(), resourceActor(), core.ResourcePublishRequest{
		Kind:         core.ResourceMCPServer,
		ID:           "fs",
		VersionID:    &v1.ID,
		ExpectedETag: env.currentETagOf(t, core.ResourceMCPServer, "fs"),
	})
	require.NoError(t, err)
	require.True(t, result.PendingRestart)
	require.Equal(t, 2, result.Version.VersionNo)
	require.Equal(t, v1.ContentHash, result.Version.ContentHash, "撤回只复制来源字节，哈希不重算")

	ptr := env.pointerOf(core.ResourceMCPServer, "fs")
	require.Equal(t, result.Version.ID, ptr.PublishedVersionID)
	require.NotNil(t, ptr.AppliedVersionID)
	require.Equal(t, v1.ID, *ptr.AppliedVersionID, "发布不得推进 applied")
	require.True(t, mcpRestartPending(ptr))

	versions := env.versionsOfKind(core.ResourceMCPServer, "fs")
	require.Len(t, versions, 2, "版本只追加")
	require.Equal(t, v1.DefinitionYAML, versions[1].DefinitionYAML, "历史版本不被改写")

	// 已发布最新版本 → 幂等成功；待重启结论照旧（不因重发变绿）。条件 ETag 取写路径
	// 同源的当前值（FR-081 组成含 revision，内容哈希单独拼不出来）。
	// The idempotent republish supplies the current conditional ETag read from the same
	// source the write path compares against.
	idempotent, err := env.admin.Publish(context.Background(), resourceActor(), core.ResourcePublishRequest{
		Kind:         core.ResourceMCPServer,
		ID:           "fs",
		ExpectedETag: env.currentETagOf(t, core.ResourceMCPServer, "fs"),
	})
	require.NoError(t, err)
	require.True(t, idempotent.PendingRestart)
	require.Equal(t, result.Version.ID, idempotent.Version.ID)
	require.Len(t, env.versionsOfKind(core.ResourceMCPServer, "fs"), 2, "幂等发布不新增版本")
}

// TestMCPServer_EditLegacyMultiWordRejected：旧格式的多词 `command` 在**读取**时
// 按空白拆分并 WARN（兼容），旁邻新增不受影响；但严格候选**拒绝**编辑它并指向
// `args` 字段——400、零写入、零操作行（约束 8 / 47）。
// TestMCPServer_EditLegacyMultiWordRejected asserts read-compatible but
// write-rejected legacy multi-word commands.
func TestMCPServer_EditLegacyMultiWordRejected(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	env.writeMCPConfig("servers:\n  - name: legacy\n    command: allowed-mcp --flag\n")

	env.createMCPStdio(t, "fs", "allowed-mcp")
	config := env.readMCPConfig()
	require.Contains(t, config, "command: allowed-mcp --flag", "未触碰的旧条目必须逐字保留")
	require.Contains(t, config, "name: fs")

	before := env.readMCPConfig()
	rowsBefore := env.operationCount()
	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      "legacy",
		Transport: "stdio",
		Command:   "allowed-mcp --flag",
		IfMatch:   core.FormatStrongETag("deadbeef"),
	})
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, err.Error(), "args", "必须指向 args 字段")

	require.Equal(t, before, env.readMCPConfig())
	require.Equal(t, rowsBefore, env.operationCount())
}

// TestResourceDelete_MCPStopFailureRestoresFile：断开 MCP 连接失败 → 定义文件按身份
// 检查恢复原字节、指针不标记删除、操作 failed 可查（约束 12 / 36）。
// TestResourceDelete_MCPStopFailureRestoresFile asserts the stop-failure compensation.
func TestResourceDelete_MCPStopFailureRestoresFile(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	v1 := env.createMCPStdio(t, "fs", "allowed-mcp")

	require.NoError(t, mcpEnv.supervisor.Start(context.Background(), core.PublishedMCPServer{
		VersionID: v1.ID,
		Config: core.MCPServerConfig{
			Name:      "fs",
			Transport: "stdio",
			Command:   "allowed-mcp",
			Args:      []string{"--root", "./workspace"},
		},
	}))
	require.True(t, mcpEnv.startedProcess())

	mcpEnv.setCloseErr(errors.New("close failed"))
	before := env.readMCPConfig()

	err := env.admin.Delete(context.Background(), resourceActor(), core.ResourceMCPServer, "fs",
		env.currentETagOf(t, core.ResourceMCPServer, "fs"), env.uidOf(core.ResourceMCPServer, "fs"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "删除 MCP server 失败")

	require.Equal(t, before, env.readMCPConfig(), "断开失败必须按身份检查恢复定义文件")

	ptr := env.pointerOf(core.ResourceMCPServer, "fs")
	require.Nil(t, ptr.DeletedAt, "指针不得标记删除")
	require.Equal(t, v1.ID, ptr.PublishedVersionID)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerDelete})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status)
}

// TestMCPTest_EndpointPolicyDeniedAudited：测试端点按传输分支先过策略并审计；
// 拒绝即不创建连接器、不起进程；操作记 success（外部判定确实完成），结论 ok=false
// 由错误码表达（约束 20 / 22）。
// TestMCPTest_EndpointPolicyDeniedAudited asserts authorization precedes the connector.
func TestMCPTest_EndpointPolicyDeniedAudited(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	env.createMCPStdio(t, "fs", "outside-allowlist")

	result, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "fs")
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, mcpResultPolicyDenied, result.ErrorCode)
	require.Equal(t, mcpStatusPolicyDenied, result.ErrorMessage)

	require.Zero(t, mcpEnv.createdCount(), "策略拒绝发生在连接器创建之前")
	require.False(t, mcpEnv.startedProcess())

	audits := mcpEnv.auditEvents()
	require.Len(t, audits, 1)
	require.Equal(t, "mcp.test", audits[0].Event)
	require.Equal(t, "deny", audits[0].Decision)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	require.Equal(t, core.TargetKindMCPServer, rows[0].TargetKind)
	require.Len(t, env.versionsOfKind(core.ResourceMCPServer, "fs"), 1, "测试不得新增版本")
}

// TestMCPTest_EndpointTimeout504：固定预算到期 → web 层映射 504，操作 failed +
// `TIMEOUT`；一次性连接仍被收尾（约束 21 / 22）。
// TestMCPTest_EndpointTimeout504 asserts the timeout classification.
func TestMCPTest_EndpointTimeout504(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	mcpEnv.setBlockConnect(true)
	mcpEnv.supervisor.deps.TestTimeout = 50 * time.Millisecond
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	env.createMCPStdio(t, "fs", "allowed-mcp")

	_, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "fs")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Contains(t, adminErr.Data, "operation_id")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status)
	require.Equal(t, resourceResultTimeout, rows[0].ResultCode)
	require.Equal(t, 1, mcpEnv.closedCount(), "超时也必须关闭一次性连接")
}

// TestMCPTest_EndpointNoRunNoLLMCall：连接成功返回工具发现；不建 Run、不写
// `llm_calls`、不动 applied 与发布指针（约束 21 / 50）。
// TestMCPTest_EndpointNoRunNoLLMCall asserts the test endpoint never creates a run.
func TestMCPTest_EndpointNoRunNoLLMCall(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	mcpEnv.setToolNames("fs_read")
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	v1 := env.createMCPStdio(t, "fs", "allowed-mcp")

	result, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "fs")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, []string{"fs_read"}, result.Tools)
	require.GreaterOrEqual(t, result.LatencyMS, int64(0))

	require.Equal(t, 1, mcpEnv.createdCount())
	require.Equal(t, 1, mcpEnv.closedCount(), "一次性连接必须收尾")
	require.True(t, mcpEnv.startedProcess())

	require.Zero(t, authorRowCount(t, env.dbPath, "runs"), "测试不是 Run")
	require.Zero(t, authorRowCount(t, env.dbPath, "llm_calls"), "测试不写模型调用审计")

	ptr := env.pointerOf(core.ResourceMCPServer, "fs")
	require.Equal(t, v1.ID, ptr.PublishedVersionID)
	require.Nil(t, ptr.AppliedVersionID, "测试不得推进 applied")
	_, applied := mcpEnv.appliedVersion("fs")
	require.False(t, applied, "supervisor 的一次性测试不写 applied")
}

// TestMCPTest_ResolvesEnvBeforeConnect：测试端点按**已发布版本**连接前必须把 env 的
// `${VAR}` 占位解析成终值，否则子进程收到的是字面占位（约束 7 / 22）。
// TestMCPTest_ResolvesEnvBeforeConnect asserts placeholders are resolved before the
// one-off connection sees the config.
func TestMCPTest_ResolvesEnvBeforeConnect(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	env.createMCPStdio(t, "fs", "allowed-mcp")

	_, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "fs")
	require.NoError(t, err)

	specs := mcpEnv.lastConnectConfigs()
	require.Len(t, specs, 1)
	require.Equal(t, mcpTestTokenSentinel, specs[0].Env[mcpTestTokenEnv],
		"连接前必须把 ${VAR} 解析成终值")
}

// TestMCPTest_MissingEnvNotConfigured：env 占位解析失败 → 不拨号、零连接器；操作
// failed + `NOT_CONFIGURED`，错误映射 503 并返回 operation_id（与模型连接测试同口径）。
// TestMCPTest_MissingEnvNotConfigured fails before dialing and keeps the operation id.
func TestMCPTest_MissingEnvNotConfigured(t *testing.T) {
	// t.Setenv 与 t.Parallel 互斥：本用例需要显式控制一个未配置变量。
	// US-14 起保存期有 env 闸门（FR5，见 mcp_env_test.go）：变量缺失的条目已无法保存，
	// 因此本用例在保存**之后**把变量置空——模拟"保存时满足、测试时进程环境已不满足"
	// （重启漏配变量）的真实形态。测试端点的判据（不拨号、failed/NOT_CONFIGURED、
	// 503 + operation_id、点名变量、不回显占位）逐条不变。
	const missingEnv = "BASALT_US12_MISSING_TOKEN"
	t.Setenv(missingEnv, "present-when-saved")

	mcpEnv := newMCPEnv(t)
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	values := map[string]string{missingEnv: "${" + missingEnv + "}"}
	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:        "fs",
		Transport:   "stdio",
		Command:     "allowed-mcp",
		Env:         values,
		Credentials: envOnlyCredentials(values),
		Create:      true,
	})
	require.NoError(t, err)

	t.Setenv(missingEnv, "")
	_, err = env.admin.TestMCPServer(context.Background(), resourceActor(), "fs")
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrProviderNotConfigured)
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.Contains(t, adminErr.Data, "operation_id")
	require.Contains(t, err.Error(), missingEnv, "错误必须点名变量")

	require.Zero(t, mcpEnv.createdCount(), "占位解析失败不得拨号")
	// 变量名不是凭证：错误文案里也不得出现任何值（本用例的值是空串）。
	require.NotContains(t, err.Error(), "${", "不得回显占位原文")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status)
	require.Equal(t, resourceResultNotConfigured, rows[0].ResultCode)
}

// TestMCPError_NoSecrets：连接失败时底层错误可能带完整 URL / 凭证，结果字段与审计
// 事件只允许固定短语（约束 23）。逆改点：把底层 `err.Error()` 拼进 `ErrorMessage` 或
// 审计 reason → 本用例红。
// TestMCPError_NoSecrets keeps results and audit events on fixed phrases even when the
// underlying connect error carries a full URL with credentials.
func TestMCPError_NoSecrets(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	mcpEnv.setConnectErr(errors.New(`connect "https://svc:s3cret@mcp.example/mcp?token=t0ken" failed`))
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	env.createMCPStdio(t, "fs", "allowed-mcp")

	result, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "fs")
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, mcpStatusConnectFailed, result.ErrorMessage)
	for _, leaked := range []string{"s3cret", "t0ken", "mcp.example"} {
		require.NotContains(t, result.ErrorMessage, leaked, "结果文案不得回显底层错误")
	}
	for _, audit := range mcpEnv.auditEvents() {
		require.NotContains(t, audit.Reason, "s3cret")
		require.NotContains(t, audit.Reason, "mcp.example")
	}

	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status, "外部判定已完成，结论 ok=false")
}

// TestMCPTest_NoProcessOnPreWriteFailure：预写失败 → 拒绝外部调用；连接器零创建、
// 进程零启动、零写入（约束 1 / 21）。
// TestMCPTest_NoProcessOnPreWriteFailure asserts the pre-write gate precedes the
// outbound connection.
func TestMCPTest_NoProcessOnPreWriteFailure(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	v1 := env.createMCPStdio(t, "fs", "allowed-mcp")
	rowsBefore := env.operationCount()

	env.admin.operations = failingBeginStore{OperationStore: env.ops, err: errBeginRefused}

	_, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "fs")
	require.Error(t, err)
	require.Contains(t, err.Error(), "预写管理操作失败")

	require.Zero(t, mcpEnv.createdCount(), "预写失败不得创建连接器")
	require.False(t, mcpEnv.startedProcess())
	require.Equal(t, rowsBefore, env.operationCount())
	require.Equal(t, v1.ID, env.pointerOf(core.ResourceMCPServer, "fs").PublishedVersionID)
}

// mcpTestDetailProbe 是 D-07 固定字段的**测试侧**投影：刻意不复用实现里的 detail 类型，
// 实现漏写某个字段就表现为零值读数（断言红），而不是编译错误。
// The probe mirrors contract D-07 independently so a missing field shows as a reading.
type mcpTestDetailProbe struct {
	OK           bool     `json:"ok"`
	LatencyMS    int64    `json:"latency_ms"`
	Tools        []string `json:"tools"`
	ErrorCode    string   `json:"error_code"`
	ErrorMessage string   `json:"error_message"`
	Stage        string   `json:"stage"`
	FailureClass string   `json:"failure_class"`
}

// mcpTestDetailOf 解出一条测试操作的 detail（D-07 对象）。
// mcpTestDetailOf decodes one test operation's detail.
func mcpTestDetailOf(t *testing.T, row core.OperationRecord) mcpTestDetailProbe {
	t.Helper()
	var probe mcpTestDetailProbe
	require.NoError(t, json.Unmarshal(row.DetailJSON, &probe), "detail_json 必须是对象：%s", row.DetailJSON)
	return probe
}

// mcpTestDetailKeys 返回 detail 的顶层键集（判"字段始终存在"用的是键，不是长度——`nil`
// 与 `[]` 的 len 都是 0，只看长度会把缺字段误判成合法空列表）。
// mcpTestDetailKeys returns the top-level keys so "field always present" is judged on
// presence rather than length (nil and [] share len 0).
func mcpTestDetailKeys(t *testing.T, row core.OperationRecord) map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(row.DetailJSON, &raw), "detail_json 必须是对象：%s", row.DetailJSON)
	return raw
}

// mcpTestPagePass 是 §2.4.2 的页面通过判据（测试侧独立实现）：`detail.ok` 为真、
// 被测版本等于当前发布版本、且被测版本非零；操作 `status` **不参与**判定——success
// 只表示管理动作执行完毕并留下结论。
// mcpTestPagePass mirrors the §2.4.2 page predicate; operation status never participates.
func mcpTestPagePass(detail mcpTestDetailProbe, requestedVersionID, publishedVersionID int64) bool {
	return detail.OK && requestedVersionID == publishedVersionID && requestedVersionID != 0
}

// TestMCPTest_RecordsVersionAndTools：模块卡 §4.5.6 逐字落地——成功测试返回
// operation_id / tested_version_id，操作行记**被测试的版本**，detail 写 tools、
// stage=complete、失败分类为空。逆改点：只写 ok/latency、或操作行不记
// requested_version_id → 红。
// TestMCPTest_RecordsVersionAndTools pins the tested version and the tool list.
func TestMCPTest_RecordsVersionAndTools(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	mcpEnv.setToolNames("echo")
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	env.createMCPStdio(t, "demo", "allowed-mcp")
	ptr := env.pointerOf(core.ResourceMCPServer, "demo")

	result, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "demo")
	require.NoError(t, err, "测试应成功")
	require.NotEmpty(t, result.OperationID, "响应必须带 operation_id")
	require.Equal(t, ptr.PublishedVersionID, result.TestedVersionID, "响应必须带被测版本")
	require.True(t, result.OK)
	require.Equal(t, "complete", result.Stage)
	require.Empty(t, result.FailureClass)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
	require.Len(t, rows, 1)
	require.Equal(t, result.OperationID, rows[0].OperationID)
	require.Equal(t, ptr.PublishedVersionID, rows[0].RequestedVersionID, "操作行必须记被测版本")
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	require.Equal(t, lifecycleResultOK, rows[0].ResultCode)

	detail := mcpTestDetailOf(t, rows[0])
	require.True(t, detail.OK)
	require.Equal(t, []string{"echo"}, detail.Tools, "detail 必须写 tools")
	require.Equal(t, "complete", detail.Stage, "成功 stage 必须是 complete")
	require.Empty(t, detail.FailureClass)
	require.True(t, mcpTestPagePass(detail, rows[0].RequestedVersionID, ptr.PublishedVersionID))
}

// TestMCPTest_FailureClassification：模块卡 §4.5.7——supervisor 自己判定的两条分支
// （策略拒绝 = 200 + ok=false，超时 = 504 + failed/TIMEOUT），另加结构化失败的
// stage × class 透传与"没有 ConnectDetailed 时回退 dependency_unconfirmed"。
// 连接池自己的 10 个 class 由 `internal/tool` 的表驱动用例覆盖，这里只判装配层的映射。
// TestMCPTest_FailureClassification pins the classification rows the assembly owns.
func TestMCPTest_FailureClassification(t *testing.T) {
	t.Parallel()

	t.Run("policy_denied", func(t *testing.T) {
		t.Parallel()
		mcpEnv := newMCPEnv(t) // 白名单只有 allowed-mcp
		env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
		env.createMCPStdio(t, "demo", "other-mcp")

		result, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "demo")
		require.NoError(t, err, "策略拒绝是 200，不是错误")
		require.False(t, result.OK)
		require.Equal(t, "preflight", result.Stage)
		require.Equal(t, "policy_denied", result.FailureClass)

		rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
		require.Len(t, rows, 1)
		require.Equal(t, core.OperationSuccess, rows[0].Status, "策略拒绝必须记 success")
		require.Equal(t, lifecycleResultOK, rows[0].ResultCode)

		detail := mcpTestDetailOf(t, rows[0])
		require.False(t, detail.OK)
		require.Equal(t, "preflight", detail.Stage)
		require.Equal(t, "policy_denied", detail.FailureClass)
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		mcpEnv := newMCPEnv(t)
		mcpEnv.setBlockConnect(true)
		mcpEnv.supervisor.deps.TestTimeout = 50 * time.Millisecond
		env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
		env.createMCPStdio(t, "demo", "allowed-mcp")

		result, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "demo")
		require.ErrorIs(t, err, context.DeadlineExceeded, "超时必须映射 504")
		require.Equal(t, core.ResourceTestResult{}, result, "504 不返回测试结果对象")

		rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
		require.Len(t, rows, 1)
		require.Equal(t, core.OperationFailed, rows[0].Status, "超时必须记 failed")
		require.Equal(t, resourceResultTimeout, rows[0].ResultCode)

		detail := mcpTestDetailOf(t, rows[0])
		require.False(t, detail.OK)
		require.Equal(t, "spawn", detail.Stage, "无结构化失败时超时 stage 退到 spawn")
		require.Equal(t, "timeout", detail.FailureClass)
		require.Equal(t, ptrOrZero(env, "demo"), rows[0].RequestedVersionID, "超时行同样记被测版本")
	})

	t.Run("structured_failure_passthrough", func(t *testing.T) {
		t.Parallel()
		mcpEnv := newMCPEnv(t)
		mcpEnv.setConnectFailure("spawn", "command_not_found")
		env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
		env.createMCPStdio(t, "demo", "allowed-mcp")

		result, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "demo")
		require.NoError(t, err, "连接失败是 200 + ok=false，不是 5xx")
		require.False(t, result.OK)
		require.Equal(t, "spawn", result.Stage, "stage 必须来自结构化失败")
		require.Equal(t, "command_not_found", result.FailureClass)
		require.Equal(t, mcpResultConnectFailed, result.ErrorCode)

		rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
		require.Len(t, rows, 1)
		require.Equal(t, core.OperationSuccess, rows[0].Status)
		require.Equal(t, lifecycleResultOK, rows[0].ResultCode)
		detail := mcpTestDetailOf(t, rows[0])
		require.Equal(t, "spawn", detail.Stage)
		require.Equal(t, "command_not_found", detail.FailureClass)
	})

	t.Run("timeout_takes_stage_from_failure", func(t *testing.T) {
		t.Parallel()
		mcpEnv := newMCPEnv(t)
		mcpEnv.setConnectFailure("initialize", "handshake_failed")
		mcpEnv.setConnectFailureDeadline(true)
		env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
		env.createMCPStdio(t, "demo", "allowed-mcp")

		_, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "demo")
		require.ErrorIs(t, err, context.DeadlineExceeded)

		rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
		require.Len(t, rows, 1)
		require.Equal(t, core.OperationFailed, rows[0].Status)
		detail := mcpTestDetailOf(t, rows[0])
		require.Equal(t, "initialize", detail.Stage, "超时 stage 取第一条结构化失败")
		require.Equal(t, "timeout", detail.FailureClass, "分类仍是 timeout")
	})

	t.Run("legacy_connector_fallback", func(t *testing.T) {
		t.Parallel()
		mcpEnv := newMCPEnv(t)
		mcpEnv.setLegacyConnector(true)
		mcpEnv.setConnectErr(errors.New("boom"))
		env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
		env.createMCPStdio(t, "demo", "allowed-mcp")

		result, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "demo")
		require.NoError(t, err)
		require.False(t, result.OK)
		require.Equal(t, "spawn", result.Stage)
		require.Equal(t, "dependency_unconfirmed", result.FailureClass,
			"没有结构化失败就不猜分类（D-05 / C-06）")
	})
}

// TestMCPTest_EmptyToolList：模块卡 §4.5.13——`tools/list` 成功但为空记 ok=true、
// `tools` 是**存在**的空数组、无失败分类；不得改写成 `tools_list_failed`。
// 逆改点：把空列表判成失败、或省略 tools 字段 → 红。
// TestMCPTest_EmptyToolList forbids turning an empty tool list into a failure.
func TestMCPTest_EmptyToolList(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	mcpEnv.setToolNames() // 连接成功，但 tools/list 为空
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	env.createMCPStdio(t, "demo", "allowed-mcp")

	result, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "demo")
	require.NoError(t, err, "协议成功不应报错")
	require.True(t, result.OK)
	require.Empty(t, result.Tools)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
	require.Len(t, rows, 1)
	detail := mcpTestDetailOf(t, rows[0])
	require.True(t, detail.OK)
	require.Empty(t, detail.Tools, "空列表是合法读数，不是失败分类")
	require.Empty(t, detail.FailureClass)
	require.Equal(t, "complete", detail.Stage)

	keys := mcpTestDetailKeys(t, rows[0])
	require.Contains(t, keys, "tools", "tools 字段必须存在")
	require.JSONEq(t, "[]", string(keys["tools"]), "空列表必须是 []，不是 null")
}

// TestMCPTest_LegacyOperationNotPass：§2.4.2 的"历史无版本"行——预写形态的操作行
// （`requested_version_id` 为空、`ok=true`）**不得**被判为通过；新记录必须能通过。
// 逆改点：页面判据只看 `ok`、或预写不写被测版本 → 红（新旧两条都过 / 都不过）。
// TestMCPTest_LegacyOperationNotPass keeps version-less history from passing the page.
func TestMCPTest_LegacyOperationNotPass(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	mcpEnv.setToolNames("echo")
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	published := env.createMCPStdio(t, "demo", "allowed-mcp")

	// ① 历史行：US-14 之前的预写形态（没有被测版本），结论 ok=true。
	actor := resourceActor()
	legacyID := core.NewOperationID(baselineClock())
	require.NoError(t, env.ops.BeginOperation(context.Background(), core.OperationRecord{
		OperationID:  legacyID,
		Action:       core.ActionMCPServerTest,
		TargetKind:   core.TargetKindMCPServer,
		TargetID:     "demo",
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  mcpServerTestHash("demo"),
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    baselineClock(),
	}))
	require.NoError(t, env.ops.FinishOperation(context.Background(), legacyID, core.OperationSuccess,
		lifecycleResultOK, "", []byte(`{"ok":true,"latency_ms":1,"tools":[]}`), baselineClock()))

	legacy, err := env.ops.GetOperation(context.Background(), legacyID)
	require.NoError(t, err)
	require.Zero(t, legacy.RequestedVersionID, "历史行没有被测版本")
	require.False(t, mcpTestPagePass(mcpTestDetailOf(t, legacy), legacy.RequestedVersionID,
		published.ID), "没有被测版本的历史记录不得判通过")

	// ② 新记录：同一个判据必须能通过——否则"不通过"不是判别力，只是常量。
	_, err = env.admin.TestMCPServer(context.Background(), resourceActor(), "demo")
	require.NoError(t, err)
	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
	require.Len(t, rows, 2)
	var fresh core.OperationRecord
	for _, row := range rows {
		if row.OperationID != legacyID {
			fresh = row
		}
	}
	require.NotZero(t, fresh.OperationID, "必须能找到本次测试的行")
	require.Equal(t, published.ID, fresh.RequestedVersionID, "新记录必须带被测版本")
	require.True(t, mcpTestPagePass(mcpTestDetailOf(t, fresh), fresh.RequestedVersionID, published.ID),
		"新记录必须能通过页面判据")
}

// TestMCPTest_UnconfirmedEntryStillAttempts：模块卡 §4.5.13——诊断 `unconfirmed`
// （含分隔符的相对入口）**不得**预判测试结论：测试仍必须真实尝试并连接。
// 判据取"连接器创建 + 进程启动"：一次性测试路径不经过 `OnConnected`，卡片示例里的
// `connectedCount()` 在该路径上恒为 0，等价证据是"真的发起了连接"。
// 逆改点：诊断分支直接返回 ok=false / 跳过连接 → createdCount=0 → 红。
// TestMCPTest_UnconfirmedEntryStillAttempts proves diagnosis never pre-judges a test.
func TestMCPTest_UnconfirmedEntryStillAttempts(t *testing.T) {
	t.Parallel()
	// 含分隔符的相对入口：不启动进程无法证明能否定位，诊断只能给 unconfirmed。
	policy := tool.NewWhitelistPolicy(tool.PolicyOptions{
		Root:            t.TempDir(),
		AllowedCommands: []string{"./mcp-helper"},
	})
	mcpEnv := newMCPEnvWithPolicy(t, policy)
	mcpEnv.setToolNames("echo")
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	env.createMCPStdio(t, "demo", "./mcp-helper")

	diag, err := env.admin.DiagnoseMCPServer(context.Background(), resourceActor(), "demo")
	require.NoError(t, err)
	require.NotNil(t, diag.Command)
	require.Equal(t, "unconfirmed", diag.Command.State)

	_, err = env.admin.TestMCPServer(context.Background(), resourceActor(), "demo")
	require.NoError(t, err, "测试应真实尝试并成功")
	require.Equal(t, 1, mcpEnv.createdCount(), "诊断 unconfirmed 时测试仍必须创建连接器")
	require.True(t, mcpEnv.startedProcess(), "诊断 unconfirmed 时测试仍必须真实发起连接")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerTest})
	require.Len(t, rows, 1)
	detail := mcpTestDetailOf(t, rows[0])
	require.True(t, detail.OK, "诊断 unconfirmed 不得预判 ok=false")
	require.Equal(t, "complete", detail.Stage)
	require.Empty(t, detail.FailureClass, "实际成功的测试不得带失败分类")
}

// TestMCPTest_NoOperationOnPrewriteFailure：模块卡 §4.5.12——预写失败 → 拒绝执行、
// 不留操作行、不创建连接器、不启动子进程。卡片里的 `beginFailStore` 按"实现后以实际
// 签名为准"的约定复用既有的 `failingBeginStore`（注入位相同：预写返回错误）。
// 逆改点：先 supervisor.Test 再 BeginOperation → 连接器计数 / 进程标记变真 → 红。
// TestMCPTest_NoOperationOnPrewriteFailure keeps the pre-write gate ahead of any side
// effect.
func TestMCPTest_NoOperationOnPrewriteFailure(t *testing.T) {
	t.Parallel()
	mcpEnv := newMCPEnv(t)
	env := newResourceEnv(t, withSupervisor(mcpEnv.supervisor))
	v1 := env.createMCPStdio(t, "demo", "allowed-mcp")
	before := env.operationCount()

	env.admin.operations = failingBeginStore{OperationStore: env.ops, err: errBeginRefused}

	_, err := env.admin.TestMCPServer(context.Background(), resourceActor(), "demo")
	require.Error(t, err, "预写失败必须拒绝执行")
	require.Contains(t, err.Error(), "预写管理操作失败")

	require.Equal(t, before, env.operationCount(), "预写失败不得留下操作行")
	require.Zero(t, mcpEnv.createdCount(), "预写失败不得创建连接器")
	require.False(t, mcpEnv.startedProcess(), "预写失败不得启动子进程")
	require.Zero(t, mcpEnv.closedCount(), "没有连接就没有连接可收尾")
	require.Equal(t, v1.ID, env.pointerOf(core.ResourceMCPServer, "demo").PublishedVersionID)
}

// ptrOrZero 取当前发布版本号；指针缺失返回 0（只用于断言里的期望值）。
// ptrOrZero reads the published version id, or 0 when there is no pointer.
func ptrOrZero(env *resourceEnv, name string) int64 {
	ptr, err := env.store.Pointer(context.Background(), core.ResourceMCPServer, name)
	if err != nil {
		return 0
	}
	return ptr.PublishedVersionID
}

// ---------------------------------------------------------------------------
// 技能写面（颗粒度文档 §3.2.1 / 图 D，T051）
//
// 技能的判据与模型连接的刻意差异（文件头「差异说明」）：同名创建与工作副本分歧是
// **预写之前**的 409，零操作行；预写之后的失败必须回填终态并带回 operation_id。
// ---------------------------------------------------------------------------

// unlockTreeOnCleanup 在清理阶段解除只读位：已发布技能版本目录按要求是 0o444 / 0o555，
// 而 t.TempDir 的 RemoveAll 两平台都会因此失败——Windows 删不掉只读属性为只读的文件，
// Linux 的 0o555 目录不允许 unlink 其子项（CI 实测：TempDir RemoveAll permission denied）。
// unlockTreeOnCleanup clears read-only bits so temp-dir cleanup can remove them on both
// platforms (Windows read-only attributes; Linux 0o555 directories blocking unlink).
func unlockTreeOnCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(p, 0o755)
			} else {
				_ = os.Chmod(p, 0o644)
			}
			return nil
		})
	})
}

// skillCardFixture 造一份最小可导入的 SKILL.md：frontmatter 只含 name / description。
// skillCardFixture renders a minimal well-formed SKILL.md for fixtures.
func skillCardFixture(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body + "\n"
}

// agentEntryWithSkill 造一个已注册、声明指定技能的装载条目。
// agentEntryWithSkill builds one registered loader entry declaring a skill.
func agentEntryWithSkill(name, skill string) core.AgentEntry {
	return core.AgentEntry{
		Name:       name,
		Registered: true,
		Profile:    &core.Profile{Name: name, Skills: []string{skill}},
	}
}

// createSkill 经真实创建路径建一只技能并立即发布（当前版本 1）。
// createSkill creates and publishes version 1 through the real write path.
func (e *resourceEnv) createSkill(t *testing.T, name, description string) SkillDetail {
	t.Helper()
	detail, err := e.admin.CreateSkill(context.Background(), resourceActor(),
		SkillCreateRequest{Name: name, Description: description})
	require.NoError(t, err)
	return detail
}

// skillPointer 取技能的发布指针（含删除标记）。
// skillPointer returns the skill publish pointer.
func (e *resourceEnv) skillPointer(t *testing.T, name string) core.ResourcePointer {
	t.Helper()
	ptr, err := e.store.Pointer(context.Background(), core.ResourceSkill, name)
	require.NoError(t, err)
	return ptr
}

// skillVersions 取技能的全部版本（新到旧）。
// skillVersions returns every skill version, newest first.
func (e *resourceEnv) skillVersions(t *testing.T, name string) []core.ResourceVersion {
	t.Helper()
	versions, err := e.store.Versions(context.Background(), core.ResourceSkill, name)
	require.NoError(t, err)
	return versions
}

// workCopyFile 读回技能工作副本里的一个文件。
// workCopyFile reads one file of the skill work copy.
func (e *resourceEnv) workCopyFile(t *testing.T, name, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.root, "skills", name, filepath.FromSlash(rel)))
	require.NoError(t, err)
	return string(raw)
}

// versionRelDir 按登记行解析版本的**落盘目录**（相对工作区根的形态）——读取一律以版本行
// 持久化的 `storage_path` 为准，不按当前身份重拼：新发布是
// `versions/skills/<name>/<uid>/<no>`，旧扁平目录仍是 `versions/skills/<name>/<no>`
// （2026-10-23 裁定：旧目录不搬迁、不覆盖）。同版本号跨代出现时取最新一行。
// versionRelDir resolves a version row's recorded storage_path into a workspace-relative
// directory (identity-segmented for new publications, legacy flat for older rows).
func (e *resourceEnv) versionRelDir(t *testing.T, name string, versionNo int) string {
	t.Helper()
	for _, version := range e.skillVersions(t, name) {
		if version.VersionNo != versionNo {
			continue
		}
		rel, err := workspace.PublishedVersionRelDir(version.StoragePath)
		require.NoError(t, err, "登记的 storage_path 必须属于两种契约形态之一")
		return rel
	}
	require.FailNow(t, "版本行不存在", "技能 %s 没有登记的 v%d", name, versionNo)
	return ""
}

// versionFile 读回不可变版本目录里的一个文件（目录由登记行解析，兼容新旧两种形态）。
// versionFile reads one file of an immutable version directory resolved from the record.
func (e *resourceEnv) versionFile(t *testing.T, name string, versionNo int, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(e.versionRelDir(t, name, versionNo)),
		filepath.FromSlash(rel)))
	require.NoError(t, err)
	return string(raw)
}

// TestResourceAdmin_SkillCreatePublishes：创建即发布 v1（图 D）——工作副本、不可变
// 快照、发布 / 应用指针与成功操作行四处一致；快照文件只读（约束 25）。
// TestResourceAdmin_SkillCreatePublishes asserts the four-way agreement after create.
func TestResourceAdmin_SkillCreatePublishes(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	detail, err := env.admin.CreateSkill(context.Background(), resourceActor(),
		SkillCreateRequest{Name: "logger", Description: "日志查询"})
	require.NoError(t, err)
	require.Equal(t, "logger", detail.Name)
	require.Equal(t, "日志查询", detail.Description)
	require.Equal(t, ".basalt/skills/logger", detail.Path)
	require.Equal(t, 1, detail.VersionNo)
	require.NotZero(t, detail.VersionID)
	// FR-081：返回的 ETag 必须与写路径比较的"当前值"同源（内容 + 身份 + 修订 + 范围 +
	// 凭据段），不能拿内容哈希自己拼——那样页面第二次保存必然 412。
	// FR-081: the returned ETag must come from the same read point the write paths use.
	require.Equal(t, env.currentETagOf(t, core.ResourceSkill, "logger"), detail.ETag)
	require.Empty(t, detail.ReferencedBy)
	require.Len(t, detail.Files, 1)
	require.Equal(t, "SKILL.md", detail.Files[0].Path)

	card := env.workCopyFile(t, "logger", "SKILL.md")
	require.Contains(t, card, "name: logger")
	require.Contains(t, card, "description: 日志查询")
	require.Equal(t, card, string(detail.Files[0].Content))
	require.Equal(t, sha256Hex(t, []byte(card)), detail.Files[0].SHA256)
	require.Equal(t, int64(len(card)), detail.Files[0].Size)

	require.Equal(t, card, env.versionFile(t, "logger", 1, "SKILL.md"))
	info, err := os.Stat(filepath.Join(env.root, filepath.FromSlash(env.versionRelDir(t, "logger", 1)), "SKILL.md"))
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm()&0o200, "已发布版本文件必须只读")

	ptr := env.skillPointer(t, "logger")
	require.Equal(t, detail.VersionID, ptr.PublishedVersionID)
	require.NotNil(t, ptr.AppliedVersionID, "技能发布即应用（applied = published）")
	require.Equal(t, detail.VersionID, *ptr.AppliedVersionID)
	require.Nil(t, ptr.DeletedAt)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionSkillCreate})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	require.Equal(t, core.TargetKindSkill, rows[0].TargetKind)
	require.Equal(t, "logger", rows[0].TargetID)
}

// TestResourceAdmin_SkillImportPublishes：导入整棵技能目录——清单按 path 升序、
// 快照逐字节等于候选、版本行只存清单（技能的定义原文恒为空）。
// TestResourceAdmin_SkillImportPublishes asserts the sorted manifest and byte-exact snapshot.
func TestResourceAdmin_SkillImportPublishes(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	guide := "# 查询语法\n"
	detail, err := env.admin.ImportSkill(context.Background(), resourceActor(), SkillImportRequest{
		Name: "logger",
		Files: []core.AuthorFile{
			{Path: "docs/guide.md", Content: guide},
			{Path: "SKILL.md", Content: skillCardFixture("logger", "日志查询", "用法见 docs/guide.md")},
		},
	})
	require.NoError(t, err)

	paths := make([]string, len(detail.Files))
	for i, file := range detail.Files {
		paths[i] = file.Path
	}
	require.Equal(t, []string{"SKILL.md", "docs/guide.md"}, paths, "清单必须按 path 升序")
	require.Equal(t, guide, env.workCopyFile(t, "logger", "docs/guide.md"))
	require.Equal(t, guide, env.versionFile(t, "logger", 1, "docs/guide.md"))

	versions := env.skillVersions(t, "logger")
	require.Len(t, versions, 1)
	require.Empty(t, versions[0].DefinitionYAML, "技能版本不存定义原文")
	require.Equal(t, fmt.Sprintf(".basalt/versions/skills/logger/%s/1", env.uidOf(core.ResourceSkill, "logger")),
		versions[0].StoragePath, "新发布的版本目录必须是 <name>/<resource_uid>/<version_no>")

	var manifest []core.FileHash
	require.NoError(t, json.Unmarshal([]byte(versions[0].ManifestJSON), &manifest))
	require.Len(t, manifest, 2)
	require.Equal(t, "SKILL.md", manifest[0].Path)
	require.Equal(t, "docs/guide.md", manifest[1].Path)
	require.Equal(t, sha256Hex(t, []byte(guide)), manifest[1].SHA256)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionSkillImport})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
}

// TestResourceAdmin_SkillUpdateFullReplacement：更新是全量替换（约束 25）——未列出的
// 文件随旧树消失，v1 快照逐字节不变，新版本号递增且指针跟进。
// TestResourceAdmin_SkillUpdateFullReplacement asserts wholesale replacement and an
// untouched v1 snapshot.
func TestResourceAdmin_SkillUpdateFullReplacement(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	first, err := env.admin.ImportSkill(context.Background(), resourceActor(), SkillImportRequest{
		Name: "logger",
		Files: []core.AuthorFile{
			{Path: "SKILL.md", Content: skillCardFixture("logger", "日志查询", "v1")},
			{Path: "docs/guide.md", Content: "# 旧指南\n"},
		},
	})
	require.NoError(t, err)

	second, err := env.admin.UpdateSkill(context.Background(), resourceActor(), SkillUpdateRequest{
		Name:    "logger",
		Files:   []core.AuthorFile{{Path: "SKILL.md", Content: skillCardFixture("logger", "日志查询 v2", "v2")}},
		IfMatch: first.ETag,
	})
	require.NoError(t, err)
	require.Equal(t, 2, second.VersionNo)
	require.NotEqual(t, first.ContentHash, second.ContentHash)
	require.Len(t, second.Files, 1, "全量替换后只剩列出的文件")

	_, statErr := os.Stat(filepath.Join(env.root, "skills", "logger", "docs"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "未列出的旧文件必须随旧树消失")
	require.Equal(t, "# 旧指南\n", env.versionFile(t, "logger", 1, "docs/guide.md"),
		"历史快照不得随更新改写")

	versions := env.skillVersions(t, "logger")
	require.Len(t, versions, 2)
	require.Equal(t, 2, versions[0].VersionNo)
	require.Equal(t, 1, versions[1].VersionNo)
	require.Equal(t, first.ContentHash, versions[1].ContentHash)
	require.Equal(t, second.VersionID, env.skillPointer(t, "logger").PublishedVersionID)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionSkillUpdate})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
}

// TestSkill_StaleETagZeroWrite：技能的 If-Match 缺失或不匹配都是 412 + `current_etag`，
// 零写入、零操作行（失败判据表：PUT 的缺失归 412）。
// TestSkill_StaleETagZeroWrite covers both the missing and the stale If-Match.
func TestSkill_StaleETagZeroWrite(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	detail := env.createSkill(t, "logger", "日志查询")
	before := env.workCopyFile(t, "logger", "SKILL.md")
	rowsBefore := env.operationCount()

	next := []core.AuthorFile{{Path: "SKILL.md", Content: skillCardFixture("logger", "日志查询", "改过")}}
	for _, ifMatch := range []string{"", `"sha256:ffff"`} {
		_, err := env.admin.UpdateSkill(context.Background(), resourceActor(), SkillUpdateRequest{
			Name: "logger", Files: next, IfMatch: ifMatch,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, core.ErrPreconditionFailed)
		var adminErr *core.AdminError
		require.ErrorAs(t, err, &adminErr)
		require.Equal(t, detail.ETag, adminErr.Data["current_etag"])
	}

	require.Equal(t, before, env.workCopyFile(t, "logger", "SKILL.md"))
	require.Equal(t, rowsBefore, env.operationCount(), "412 不得写操作行")
	require.Equal(t, detail.VersionID, env.skillPointer(t, "logger").PublishedVersionID)
}

// TestSkillWrite_WorkCopyDivergedConflict：工作副本被外部修改 / 缺失 → 409 且零操作行
// （图 D 的「不写操作行」与失败表技能行）；拒绝写入不得改动磁盘。
// TestSkillWrite_WorkCopyDivergedConflict covers both the tampered and the missing copy.
func TestSkillWrite_WorkCopyDivergedConflict(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	detail := env.createSkill(t, "logger", "日志查询")
	workPath := filepath.Join(env.root, "skills", "logger", "SKILL.md")
	tampered := env.workCopyFile(t, "logger", "SKILL.md") + "\n外部改动\n"
	require.NoError(t, os.WriteFile(workPath, []byte(tampered), 0o644))
	rowsBefore := env.operationCount()

	next := []core.AuthorFile{{Path: "SKILL.md", Content: skillCardFixture("logger", "日志查询", "改过")}}
	_, err := env.admin.UpdateSkill(context.Background(), resourceActor(), SkillUpdateRequest{
		Name: "logger", Files: next, IfMatch: detail.ETag,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrConflict)
	require.Contains(t, err.Error(), "外部变化")
	require.Equal(t, rowsBefore, env.operationCount(), "分歧闸门在预写之前，不得写操作行")
	require.Equal(t, tampered, env.workCopyFile(t, "logger", "SKILL.md"), "拒绝写入不得改动工作副本")

	require.NoError(t, os.RemoveAll(filepath.Join(env.root, "skills", "logger")))
	_, err = env.admin.UpdateSkill(context.Background(), resourceActor(), SkillUpdateRequest{
		Name: "logger", Files: next, IfMatch: detail.ETag,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrConflict)
	require.Contains(t, err.Error(), "不存在或缺少")
	require.Equal(t, rowsBefore, env.operationCount(), "工作副本缺失同样是预写之前的 409")
	require.Equal(t, detail.VersionID, env.skillPointer(t, "logger").PublishedVersionID)
}

// TestResourceAdmin_SkillDuplicateCreateZeroOperations：同名创建是**预写之前**的 409，
// 零操作行（与模型连接的「一条 failed 行」刻意不同，见图 D；T051 落地时按图 D 裁决）。
// TestResourceAdmin_SkillDuplicateCreateZeroOperations pins the row-free duplicate create.
func TestResourceAdmin_SkillDuplicateCreateZeroOperations(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	first := env.createSkill(t, "logger", "日志查询")
	card := env.workCopyFile(t, "logger", "SKILL.md")
	rowsBefore := env.operationCount()

	_, err := env.admin.CreateSkill(context.Background(), resourceActor(),
		SkillCreateRequest{Name: "logger", Description: "另一个"})
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrConflict)
	require.Contains(t, err.Error(), "已存在")

	require.Equal(t, rowsBefore, env.operationCount(), "同名创建是预写之前的 409，零操作行")
	require.Equal(t, card, env.workCopyFile(t, "logger", "SKILL.md"))
	require.Len(t, env.skillVersions(t, "logger"), 1)
	require.Equal(t, first.VersionID, env.skillPointer(t, "logger").PublishedVersionID)
}

// TestResourceAdmin_SkillCommitFailureRollsBack：提交段失败 → 恢复旧工作副本、删除本次
// 快照、操作 failed（不是 unknown）、指针不动（约束 25 / 36）。
// TestResourceAdmin_SkillCommitFailureRollsBack asserts the restore and snapshot removal.
func TestResourceAdmin_SkillCommitFailureRollsBack(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	first := env.createSkill(t, "logger", "日志查询")
	card := env.workCopyFile(t, "logger", "SKILL.md")

	env.failNextVersionInsert(errors.New("disk full"))
	_, err := env.admin.UpdateSkill(context.Background(), resourceActor(), SkillUpdateRequest{
		Name:    "logger",
		Files:   []core.AuthorFile{{Path: "SKILL.md", Content: skillCardFixture("logger", "日志查询", "v2")}},
		IfMatch: first.ETag,
	})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "待查证", "补偿成功不得标 unknown")

	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	operationID, ok := adminErr.Data["operation_id"].(string)
	require.True(t, ok, "pending 之后的失败必须带回 operation_id")
	require.NotEmpty(t, operationID)

	require.Equal(t, card, env.workCopyFile(t, "logger", "SKILL.md"), "提交失败必须恢复旧工作副本")
	_, statErr := os.Stat(filepath.Join(env.root, "versions", "skills", "logger",
		env.uidOf(core.ResourceSkill, "logger"), "2"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "本次快照必须删除")
	require.Equal(t, first.VersionID, env.skillPointer(t, "logger").PublishedVersionID)
	require.Len(t, env.skillVersions(t, "logger"), 1)

	stored, err := env.ops.GetOperation(context.Background(), operationID)
	require.NoError(t, err)
	require.Equal(t, core.OperationFailed, stored.Status)
	require.Equal(t, lifecycleResultInternal, stored.ResultCode)
}

// TestResourceDelete_SkillArchivesWorkCopy：删除把工作副本移入
// `archive/skills/<name>-<UTC 时间戳>`（图 G），指针只打删除标记，版本与历史不动。
// TestResourceDelete_SkillArchivesWorkCopy asserts the archive move and the frozen history.
func TestResourceDelete_SkillArchivesWorkCopy(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	detail := env.createSkill(t, "logger", "日志查询")
	card := env.workCopyFile(t, "logger", "SKILL.md")

	require.NoError(t, env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceSkill, "logger", detail.ETag,
		env.uidOf(core.ResourceSkill, "logger")))

	_, statErr := os.Stat(filepath.Join(env.root, "skills", "logger"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "工作副本必须离开活动区")
	archive := filepath.Join(env.root, "archive", "skills", "logger-20261021-090000", "SKILL.md")
	raw, err := os.ReadFile(archive)
	require.NoError(t, err, "归档标识 = 名字 + UTC 时间戳（固定时钟）")
	require.Equal(t, card, string(raw), "归档内容逐字节保留")

	ptr := env.skillPointer(t, "logger")
	require.NotNil(t, ptr.DeletedAt)
	require.Equal(t, detail.VersionID, ptr.PublishedVersionID, "删除不动发布版本")
	require.Len(t, env.skillVersions(t, "logger"), 1, "删除不追加版本")

	versions, err := env.admin.Versions(context.Background(), core.ResourceSkill, "logger")
	require.NoError(t, err)
	require.Len(t, versions, 1, "历史版本仍可查")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionSkillDelete})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
}

// TestResourceRecreate_SkillVersionContinues：删除后同名重建 = 新实例（FR-066 /
// SC-017）：新 UID、版本号从 1 重新计、删除标记清空、前一代版本按身份仍可查，新工作
// 副本与归档并存（用例名沿用 stage 的固定名）。
// A same-name skill rebuild starts a new instance: fresh UID, version numbers restart at
// one, while the previous generation stays readable under its own identity.
func TestResourceRecreate_SkillVersionContinues(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	first := env.createSkill(t, "logger", "第一版")
	firstUID := env.uidOf(core.ResourceSkill, "logger")
	require.NoError(t, env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceSkill, "logger", first.ETag, firstUID))

	second, err := env.admin.CreateSkill(context.Background(), resourceActor(),
		SkillCreateRequest{Name: "logger", Description: "重建"})
	require.NoError(t, err)
	require.Equal(t, 1, second.VersionNo, "同名重建是新实例：版本号从 1 重新计")
	require.NotEqual(t, first.VersionID, second.VersionID)
	secondUID := env.uidOf(core.ResourceSkill, "logger")
	require.NotEqual(t, firstUID, secondUID, "同名重建必须生成新资源 UID")

	ptr := env.skillPointer(t, "logger")
	require.Equal(t, second.VersionID, ptr.PublishedVersionID)
	require.Nil(t, ptr.DeletedAt, "重建必须清掉删除标记")

	versions := env.skillVersions(t, "logger")
	require.Len(t, versions, 2, "历史版本只追加不删除")
	require.Equal(t, 1, versions[0].VersionNo)
	require.Equal(t, second.VersionID, versions[0].ID)
	require.Equal(t, 1, versions[1].VersionNo)
	require.Equal(t, first.VersionID, versions[1].ID)
	require.Equal(t, first.ContentHash, versions[1].ContentHash)

	oldVersions, err := env.store.VersionsByUID(context.Background(), core.ResourceSkill,
		core.ResourceUID(firstUID))
	require.NoError(t, err)
	require.Len(t, oldVersions, 1, "前一代身份的历史版本仍可查")

	_, statErr := os.Stat(filepath.Join(env.root, "archive", "skills", "logger-20261021-090000"))
	require.NoError(t, statErr, "重建不得消费或覆盖归档")
}

// TestResourceDelete_SkillReferencedByAgent：被 Agent 声明的技能删除 → 400 点名引用者、
// 工作副本保留、操作 failed 且 detail 同时给出名单（约束 11 / 14；预写之后才查引用）。
// TestResourceDelete_SkillReferencedByAgent asserts the reference gate names the agent.
func TestResourceDelete_SkillReferencedByAgent(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t, func(d *ResourceAdminDeps) {
		d.Agents = func() ([]core.AgentEntry, error) {
			return []core.AgentEntry{agentEntryWithSkill("writer", "logger")}, nil
		}
	})
	unlockTreeOnCleanup(t, env.root)

	detail := env.createSkill(t, "logger", "日志查询")
	card := env.workCopyFile(t, "logger", "SKILL.md")
	rowsBefore := env.operationCount()

	err := env.admin.Delete(context.Background(), resourceActor(),
		core.ResourceSkill, "logger", detail.ETag, env.uidOf(core.ResourceSkill, "logger"))
	require.Error(t, err)
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, err.Error(), "writer")
	require.Contains(t, err.Error(), "正在被引用")

	require.Equal(t, card, env.workCopyFile(t, "logger", "SKILL.md"), "被引用时不得动工作副本")
	ptr := env.skillPointer(t, "logger")
	require.Nil(t, ptr.DeletedAt)
	require.Equal(t, detail.VersionID, ptr.PublishedVersionID)
	require.Equal(t, rowsBefore+1, env.operationCount(), "引用闸门在预写之后，必须回填终态")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionSkillDelete})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status)
	require.Equal(t, agentResultBadRequest, rows[0].ResultCode)
	var detailData map[string]any
	require.NoError(t, json.Unmarshal(rows[0].DetailJSON, &detailData))
	require.Equal(t, []any{"writer"}, detailData["agents"])
}

// TestPublish_SkillRevertCreatesNewVersion：带旧版本发布 → 用旧版本**存储字节**追加新
// 版本并改写工作副本；历史快照永不改写（约束 9 / 25）。
// TestPublish_SkillRevertCreatesNewVersion asserts the append-only revert.
func TestPublish_SkillRevertCreatesNewVersion(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	first := env.createSkill(t, "logger", "第一版")
	v1Card := env.workCopyFile(t, "logger", "SKILL.md")
	second, err := env.admin.UpdateSkill(context.Background(), resourceActor(), SkillUpdateRequest{
		Name:    "logger",
		Files:   []core.AuthorFile{{Path: "SKILL.md", Content: skillCardFixture("logger", "第二版", "v2")}},
		IfMatch: first.ETag,
	})
	require.NoError(t, err)
	v2Card := env.workCopyFile(t, "logger", "SKILL.md")

	result, err := env.admin.Publish(context.Background(), resourceActor(), core.ResourcePublishRequest{
		Kind:         core.ResourceSkill,
		ID:           "logger",
		VersionID:    &first.VersionID,
		ExpectedETag: second.ETag,
	})
	require.NoError(t, err)
	require.False(t, result.PendingRestart)
	require.Equal(t, 3, result.Version.VersionNo)
	require.Equal(t, first.ContentHash, result.Version.ContentHash, "新版本内容 = 来源版本")
	require.Equal(t, fmt.Sprintf(".basalt/versions/skills/logger/%s/3", env.uidOf(core.ResourceSkill, "logger")),
		result.Version.StoragePath, "撤回新版本同属当前身份分段")

	require.Equal(t, v1Card, env.workCopyFile(t, "logger", "SKILL.md"), "撤回必须把工作副本改回旧版本字节")
	require.Equal(t, v1Card, env.versionFile(t, "logger", 1, "SKILL.md"), "v1 快照不得改写")
	require.Equal(t, v2Card, env.versionFile(t, "logger", 2, "SKILL.md"), "v2 快照不得改写")

	versions := env.skillVersions(t, "logger")
	require.Len(t, versions, 3)
	require.Equal(t, 3, versions[0].VersionNo)
	require.Equal(t, result.Version.ID, env.skillPointer(t, "logger").PublishedVersionID)

	rows := env.listOperations(core.OperationFilter{Action: core.ActionSkillPublish})
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	require.Equal(t, first.VersionID, rows[0].RequestedVersionID, "来源版本登记在操作行上")
}

// TestPublish_SkillIdempotent：不带版本发布且最新已发布 → 幂等成功：不新建版本、不写
// 快照、不改工作副本，仍留一条 success 操作行（约束 9）。
// TestPublish_SkillIdempotent pins the idempotent publish.
func TestPublish_SkillIdempotent(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)

	detail := env.createSkill(t, "logger", "日志查询")
	card := env.workCopyFile(t, "logger", "SKILL.md")

	result, err := env.admin.Publish(context.Background(), resourceActor(), core.ResourcePublishRequest{
		Kind: core.ResourceSkill,
		ID:   "logger",
	})
	require.NoError(t, err)
	require.False(t, result.PendingRestart)
	require.Equal(t, detail.VersionID, result.Version.ID)
	require.Equal(t, card, env.workCopyFile(t, "logger", "SKILL.md"))
	require.Len(t, env.skillVersions(t, "logger"), 1, "幂等发布不追加版本")
	_, statErr := os.Stat(filepath.Join(env.root, "versions", "skills", "logger",
		env.uidOf(core.ResourceSkill, "logger"), "2"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "幂等发布不写新快照")

	rows := env.listOperations(core.OperationFilter{Action: core.ActionSkillPublish})
	require.Len(t, rows, 1, "幂等成功仍留一条操作行")
	require.Equal(t, core.OperationSuccess, rows[0].Status)
}

// TestResourceAdmin_SkillValidateNoWrites：校验端点与创建 / 导入 / 更新共用同一判据
// （约束 26）——合法与非法候选都以 200 形态返回结论，磁盘与操作台账零变化。
// TestResourceAdmin_SkillValidateNoWrites asserts the shared gate touches nothing.
func TestResourceAdmin_SkillValidateNoWrites(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	rowsBefore := env.operationCount()

	valid, err := env.admin.ValidateSkill(context.Background(), "logger", SkillUpdateRequest{
		Files: []core.AuthorFile{{Path: "SKILL.md", Content: skillCardFixture("logger", "日志查询", "正文")}},
	})
	require.NoError(t, err)
	require.True(t, valid.Valid)
	require.Empty(t, valid.Problems)

	missing, err := env.admin.ValidateSkill(context.Background(), "logger", SkillUpdateRequest{
		Files: []core.AuthorFile{{Path: "notes.md", Content: "没有卡片"}},
	})
	require.NoError(t, err)
	require.False(t, missing.Valid)
	require.NotEmpty(t, missing.Problems)
	require.Contains(t, missing.Problems[0], "缺少根级 SKILL.md")

	hidden, err := env.admin.ValidateSkill(context.Background(), "logger", SkillUpdateRequest{
		Files: []core.AuthorFile{
			{Path: ".cache/SKILL.md", Content: skillCardFixture("logger", "日志查询", "隐藏目录")},
		},
	})
	require.NoError(t, err)
	require.False(t, hidden.Valid)
	require.Contains(t, strings.Join(hidden.Problems, " "), "以点开头")

	_, statErr := os.Stat(filepath.Join(env.root, "skills"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "校验端点不得落盘")
	require.Equal(t, rowsBefore, env.operationCount(), "校验端点不写操作行")
}

// —— US-12 资源动作组恢复矩阵（T067A / 约束 44 / 契约 §9.4）——
// 恢复只查证、不重做：发布 / 删除按"版本行 + 指针 + 定义文件"三处事实判三态，
// 测试动作的 pending detail（`{}`）既不构成完整结果也证明不了未发出，恒为 unknown，
// 且**不得重发**。每个子用例独立夹具、只放一条 pending 行。
// Recovery proves resource outcomes from versions, the pointer and the definition file;
// a pending test row can prove neither verdict and is never resent.

// recoveryDepsForResource 把资源写面夹具装成恢复依赖（与 wire.go 的生产装配同源）。
// recoveryDepsForResource mirrors the production recovery wiring over the fixture.
func recoveryDepsForResource(env *resourceEnv) RecoveryDeps {
	return RecoveryDeps{
		Operations: env.ops,
		Resources:  env.store,
		Lifecycle:  workspace.NewLifecycleStore(env.root, baselineClock),
		Logger:     env.logger,
		Now:        baselineClock,
		Root:       env.root,
		ConfigPath: env.configPath,
		Skills:     workspace.NewSkillLibrary(env.root),
		Archives:   storage.NewAgentAdminStore(env.db),
		Notify:     storage.NewNotifyAdminStore(env.db),
	}
}

// TestRecoverAdmin_ResourceActionMatrix：模型连接 / 技能的写与删除三态矩阵。
// TestRecoverAdmin_ResourceActionMatrix covers the resource write and delete verdicts.
func TestRecoverAdmin_ResourceActionMatrix(t *testing.T) {
	ctx := context.Background()

	t.Run("create_not_applied", func(t *testing.T) {
		env := newResourceEnv(t)
		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionModelConnectionCreate,
			TargetKind: core.TargetKindModelConnection, TargetID: "ghost",
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForResource(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)
	})

	t.Run("publish_not_applied", func(t *testing.T) {
		env := newResourceEnv(t)
		v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")

		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionModelConnectionPublish,
			TargetKind: core.TargetKindModelConnection, TargetID: "main",
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForResource(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)
		require.Equal(t, v1.ID, env.pointer("main").PublishedVersionID, "指针必须留在旧版本")
	})

	t.Run("publish_file_diverged_indeterminate", func(t *testing.T) {
		env := newResourceEnv(t)
		env.publishModelConnection(t, "main", "https://a.example", "m1")
		// 崩溃窗口：文件已被手工改动、指针仍是旧版本——不得据此断言发布未生效。
		env.writeConfig(strings.Replace(env.readConfig(),
			"https://a.example", "https://b.example", 1))

		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionModelConnectionPublish,
			TargetKind: core.TargetKindModelConnection, TargetID: "main",
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForResource(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationUnknown, op.Status)
		require.Equal(t, core.VerificationIndeterminate, op.Verification)
	})

	t.Run("delete_not_applied", func(t *testing.T) {
		env := newResourceEnv(t)
		env.publishModelConnection(t, "main", "https://a.example", "m1")

		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionModelConnectionDelete,
			TargetKind: core.TargetKindModelConnection, TargetID: "main",
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForResource(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)
		require.Nil(t, env.pointer("main").DeletedAt, "恢复不得替未生效的删除补标记")
	})

	t.Run("delete_file_removed_indeterminate", func(t *testing.T) {
		env := newResourceEnv(t)
		env.publishModelConnection(t, "main", "https://a.example", "m1")
		// 崩溃窗口：文件块已被移除、删除标记未写——不自动补做（约束 13）。
		env.writeConfig(initialResourceConfig)

		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionModelConnectionDelete,
			TargetKind: core.TargetKindModelConnection, TargetID: "main",
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForResource(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationUnknown, op.Status)
		require.Equal(t, core.VerificationIndeterminate, op.Verification)
		require.Nil(t, env.pointer("main").DeletedAt, "恢复不得替半成品删除补标记")
	})

	t.Run("skill_create_absent_not_applied", func(t *testing.T) {
		env := newResourceEnv(t)
		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionSkillCreate,
			TargetKind: core.TargetKindSkill, TargetID: "weekly",
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForResource(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)
	})
}

// TestRecoverTest_UnknownNotResent：pending 的测试操作只带预写形态 `{}`——既没有
// 完整结果、也证明不了"调用没发出"，只能 unknown；恢复**不得重发**调用（重发会产生
// 新的 operation_id，且在可能计费的调用上再打一次），detail 原样保留供查证。
// A pending test row can prove neither outcome, so recovery keeps it unknown and never
// resends the call; the detail stays byte-for-byte.
func TestRecoverTest_UnknownNotResent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		Body: `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`,
	})
	env := newResourceEnv(t)
	env.publishModelConnection(t, "main", srv.URL(), "m1")

	opID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.ops, core.OperationRecord{
		OperationID: opID,
		Action:      core.ActionModelConnectionTest,
		TargetKind:  core.TargetKindModelConnection,
		TargetID:    "main",
		RequestHash: modelConnectionTestHash("main"),
		DetailJSON:  []byte("{}"),
	})
	rowsBefore := env.operationCount()
	require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForResource(env)))

	op := getOperation(t, env.ops, opID)
	require.Equal(t, core.OperationUnknown, op.Status)
	require.Equal(t, core.VerificationIndeterminate, op.Verification)
	require.Equal(t, "{}", string(op.DetailJSON), "unknown 必须原样保留预写 detail")
	require.False(t, op.RecoveredAt.IsZero(), "恢复必须记下查证时间")
	require.Empty(t, srv.Requests(), "恢复不得重发测试调用")
	require.Equal(t, rowsBefore, env.operationCount(), "恢复只回填，不追加操作行")
	require.Contains(t, env.logs.String(), "不重做")
}

// TestResourceAdmin_ConcurrentPublish：同一资源上的并发撤回发布由资源锁串行化——每个
// 请求都追加一个新版本（版本号连续、互不重复），历史版本逐字节不变，定义文件与原子
// 映射表保持一致，全部请求各留一条 success 操作行（012-race 必跑）。
// TestResourceAdmin_ConcurrentPublish pins per-resource serialization: every revert
// appends one version, history never changes, and the file/runtime stay consistent.
func TestResourceAdmin_ConcurrentPublish(t *testing.T) {
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	before := env.readConfig()
	rowsBefore := env.operationCount()

	const publishers = 8
	start := make(chan struct{})
	errs := make([]error, publishers)
	var wg sync.WaitGroup
	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, errs[index] = env.admin.Publish(context.Background(), resourceActor(),
				core.ResourcePublishRequest{
					Kind:      core.ResourceModelConnection,
					ID:        "main",
					VersionID: &v1.ID,
				})
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < publishers; i++ {
		require.NoError(t, errs[i], "publisher %d", i)
	}

	versions := env.versionsOf("main")
	require.Len(t, versions, publishers+1)
	seen := make(map[int]bool, len(versions))
	for _, version := range versions {
		require.False(t, seen[version.VersionNo], "版本号 %d 重复", version.VersionNo)
		seen[version.VersionNo] = true
		require.Equal(t, v1.ContentHash, version.ContentHash, "撤回版本必须保留来源内容哈希")
	}
	for number := 1; number <= publishers+1; number++ {
		require.True(t, seen[number], "版本号必须从 1 起连续")
	}

	require.Equal(t, versions[0].ID, env.pointer("main").PublishedVersionID,
		"发布指针必须指向最新版本")
	require.Equal(t, before, env.readConfig(), "并发撤回不得改变定义文件字节")
	require.NotNil(t, env.runtime.Provider("main"), "并发发布后运行时映射仍可用")

	stored, err := env.store.VersionByID(context.Background(), v1.ID)
	require.NoError(t, err)
	require.Equal(t, v1.DefinitionYAML, stored.DefinitionYAML, "历史版本不得被改写")
	require.Equal(t, 1, stored.VersionNo)

	publishRows := env.listOperations(core.OperationFilter{
		Action: core.ActionModelConnectionPublish, Limit: 100,
	})
	require.Len(t, publishRows, publishers)
	for _, row := range publishRows {
		require.Equal(t, core.OperationSuccess, row.Status)
	}
	require.Equal(t, rowsBefore+publishers, env.operationCount())
}

// TestResourceAdmin_DeleteDuringPublish：删除与撤回发布共用同一把资源锁。FR-081 起
// revision 参与条件 ETag，撤回发布一旦抢先，删除方手里的旧 If-Match 就失效——按契约
// 412 后重读当前值重试（有界，最多被每个成功发布各打断一次），最终删除成功；落败的
// 发布在 activePointer 处得到 404 且不写操作行。最终文件无该块、指针带删除标记、
// 操作台账无 pending（012-race 必跑）。
// TestResourceAdmin_DeleteDuringPublish pins the shared resource lock: the deleter
// re-reads the current ETag after a 412 (revision is part of the ETag), late
// publishers see 404 before any operation row is written.
func TestResourceAdmin_DeleteDuringPublish(t *testing.T) {
	env := newResourceEnv(t)
	v1 := env.publishModelConnection(t, "main", "https://a.example", "m1")
	rowsBefore := env.operationCount()

	const publishers = 8
	start := make(chan struct{})
	publishErrs := make([]error, publishers)
	var deleteErr error
	var wg sync.WaitGroup
	for i := 0; i < publishers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, publishErrs[index] = env.admin.Publish(context.Background(), resourceActor(),
				core.ResourcePublishRequest{
					Kind:      core.ResourceModelConnection,
					ID:        "main",
					VersionID: &v1.ID,
				})
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		// 每次 412 都重读当前 ETag 再来：成功发布至多 publishers 次，重试上界即
		// publishers+1；412 发生在预写操作行之前，重试不会多出台账行。
		// Each 412 re-reads the current ETag; every successful publish can invalidate
		// the precondition at most once, so the retry bound is publishers+1.
		for attempt := 0; attempt <= publishers; attempt++ {
			etag, ok := env.admin.CurrentETag(context.Background(),
				core.ResourceModelConnection, "main")
			if !ok {
				deleteErr = fmt.Errorf("删除方是唯一删除者，当前 ETag 却在第 %d 次尝试时不可读", attempt+1)
				return
			}
			err := env.admin.Delete(context.Background(), resourceActor(),
				core.ResourceModelConnection, "main", etag,
				env.uidOf(core.ResourceModelConnection, "main"))
			if err == nil {
				deleteErr = nil
				return
			}
			if !errors.Is(err, core.ErrPreconditionFailed) {
				deleteErr = err
				return
			}
			deleteErr = err
		}
	}()
	close(start)
	wg.Wait()

	require.NoError(t, deleteErr, "删除必须成功：每次 412 后重读当前 ETag 重试，直到拿到锁")

	successes := 0
	for i := 0; i < publishers; i++ {
		if publishErrs[i] == nil {
			successes++
			continue
		}
		// 删除先行：发布在 activePointer 处返回 404，此刻尚未预写操作行。
		require.ErrorIs(t, publishErrs[i], core.ErrResourceNotFound, "publisher %d", i)
	}

	versions := env.versionsOf("main")
	require.Len(t, versions, successes+1)
	seen := make(map[int]bool, len(versions))
	for _, version := range versions {
		require.False(t, seen[version.VersionNo], "版本号 %d 重复", version.VersionNo)
		seen[version.VersionNo] = true
	}

	ptr := env.pointer("main")
	require.NotNil(t, ptr.DeletedAt, "删除必须落下删除标记")
	require.Equal(t, versions[0].ID, ptr.PublishedVersionID,
		"删除不创建版本，指针仍指向最后一个已发布版本")
	require.NotContains(t, env.readConfig(), "name: main", "删除必须移除定义块")

	publishRows := env.listOperations(core.OperationFilter{
		Action: core.ActionModelConnectionPublish, Limit: 100,
	})
	successRows := 0
	for _, row := range publishRows {
		require.NotEqual(t, core.OperationPending, row.Status)
		if row.Status == core.OperationSuccess {
			successRows++
		}
	}
	require.Equal(t, successes, successRows, "成功发布各留一条终态行")
	deleteRows := env.listOperations(core.OperationFilter{
		Action: core.ActionModelConnectionDelete, Limit: 10,
	})
	require.Len(t, deleteRows, 1)
	require.Equal(t, core.OperationSuccess, deleteRows[0].Status)
	require.Equal(t, rowsBefore+successes+1, env.operationCount(),
		"失败发布在预写之前返回，不得留下操作行")
}

// ============================================================
// 托管凭据的模型连接保存面（R4.8 / T132 app 侧）
// ============================================================
//
// 模型连接与通知渠道的保存顺序不同：定义的 `credential_mode` 与 `base_url` 是凭据目标
// 绑定的来源，因此**先发布定义、后写凭据**（通知渠道相反：凭据写面要按名字读渠道行，
// 所以先写行）。运行时客户端只能由「已发布版本 + 已落库的 active 凭据」构建：新 Key
// 在发布时尚不存在，客户端必须等凭据写入之后再换入目录。下列用例钉住这条顺序以及
// 失败时的零写入面。
// Model connections publish the definition before the credential (the target binding
// reads the new base_url), so the runtime client can only be built after the credential
// lands. These cases pin that ordering and the zero-write failure paths.

// fixedManagedCredentials 是 `managedCredentialSource` 的固定返回值替身：记录每次解析
// 收到的发布版本引用，可注入失败。保存路径换入客户端时只能解析**本次发布**的版本。
// fixedManagedCredentials returns one canned key and records the pinned version refs.
type fixedManagedCredentials struct {
	key  string
	err  error
	refs []core.ResourceVersionRef
}

// ResolveModelCredential 记录引用并返回固定 Key。
// ResolveModelCredential records the reference and returns the canned key.
func (f *fixedManagedCredentials) ResolveModelCredential(_ context.Context,
	ref core.ResourceVersionRef,
) (string, error) {
	f.refs = append(f.refs, ref)
	if f.err != nil {
		return "", f.err
	}
	return f.key, nil
}

// newManagedModelEnv 在资源夹具上接托管凭据的读写两面替身。
// newManagedModelEnv wires the resource fixture with a fake resolver and writer.
func newManagedModelEnv(t *testing.T, source *fixedManagedCredentials) (*resourceEnv, *credentialRecorder) {
	t.Helper()
	recorder := &credentialRecorder{}
	env := newResourceEnv(t, func(deps *ResourceAdminDeps) {
		deps.CredentialSource = source
		deps.CredentialWriter = recorder
	})
	return env, recorder
}

// managePrimary 通过真实写路径创建一条托管模型连接。
// managePrimary creates one managed model connection through the real write path.
func (e *resourceEnv) managePrimary(t *testing.T, key, baseURL string) core.ResourceVersion {
	t.Helper()
	version, err := e.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:           "primary",
			BaseURL:        baseURL,
			APIKey:         key,
			APIKeySet:      true,
			DefaultModel:   "gpt-primary",
			CredentialMode: "managed",
			Create:         true,
		})
	require.NoError(t, err)
	return version
}

// TestModelConnection_ManagedSaveBuildsRuntimeClient：创建托管连接 → 定义里**没有**
// `api_key`、凭据段恰写一次（预检一次）、目录里的客户端来自凭据写入后的版本解析。
// 逆改点：保存路径继续在发布前按定义构建客户端 → 空 `api_key` 直接 400，创建失败。
// TestModelConnection_ManagedSaveBuildsRuntimeClient pins the post-credential client build.
func TestModelConnection_ManagedSaveBuildsRuntimeClient(t *testing.T) {
	t.Parallel()
	source := &fixedManagedCredentials{key: "sk-managed-save-plaintext"}
	env, recorder := newManagedModelEnv(t, source)

	const key = "sk-managed-save-plaintext"
	const base = "https://api.example.com/v1"
	version := env.managePrimary(t, key, base)

	require.Equal(t, 1, recorder.preflight)
	require.Len(t, recorder.writes, 1)
	require.Equal(t, core.BindingModelConnection, recorder.writes[0].Kind)
	require.Equal(t, core.CredentialModeManaged, recorder.writes[0].Mode)
	require.Equal(t, []byte(key), recorder.writes[0].Value)

	// 明文只在凭据材料里：定义文件与版本定义都不得出现 Key。
	require.Contains(t, env.readConfig(), "credential_mode: managed")
	require.NotContains(t, env.readConfig(), key)
	require.NotContains(t, version.DefinitionYAML, "api_key")
	require.NotContains(t, version.DefinitionYAML, key)

	// 客户端由**本次发布的版本**解析构建（引用必须带版本号与内容哈希）。
	require.Len(t, source.refs, 1)
	require.Equal(t, "primary", source.refs[0].Name)
	require.Equal(t, version.ID, source.refs[0].VersionID)
	require.Equal(t, version.VersionNo, source.refs[0].VersionNo)
	require.NotNil(t, env.runtime.Provider("primary"), "托管连接保存后必须立即进入运行时目录")

	// 操作行：夹具里的凭据写面是替身，不自己预写——真实 `CredentialOps.Write` 会再留
	// 一条 `model_connection.credential.write`。这里只钉定义段的一条终态行。
	require.Equal(t, 1, env.operationCount())
}

// TestPublish_ManagedCredentialPublishByVersion：带版本号的发布（撤回 / 重发）必须按
// **目标版本**声明的托管模式取 Key，而不是把没有 `api_key` 的条目当缺占位拒成 400
// （2026-10-24 复核发布段发现的回归点）。内容未变时文件不改写，仍追加新版本、推进指针。
// 逆改点：发布路径继续走 env-only 解析 → 托管连接发布失败。
// TestPublish_ManagedCredentialPublishByVersion pins the managed publish-by-version path.
func TestPublish_ManagedCredentialPublishByVersion(t *testing.T) {
	t.Parallel()
	source := &fixedManagedCredentials{key: "sk-managed-publish-plaintext"}
	env, _ := newManagedModelEnv(t, source)
	v1 := env.managePrimary(t, "sk-managed-publish-plaintext", "https://api.example.com/v1")
	before := env.readConfig()
	rowsBefore := env.operationCount()

	result, err := env.admin.Publish(context.Background(), resourceActor(),
		core.ResourcePublishRequest{Kind: core.ResourceModelConnection, ID: "primary", VersionID: &v1.ID})
	require.NoError(t, err)
	require.False(t, result.PendingRestart)
	require.Equal(t, 2, result.Version.VersionNo)
	require.Equal(t, v1.ContentHash, result.Version.ContentHash)
	require.Equal(t, before, env.readConfig(), "内容与发布版本一致时不得改写定义文件")

	require.Len(t, source.refs, 2, "保存一次 + 发布一次")
	ref := source.refs[len(source.refs)-1]
	require.Equal(t, "primary", ref.Name)
	require.Equal(t, v1.ResourceUID, ref.ResourceUID)
	require.Equal(t, v1.ID, ref.VersionID)
	require.Equal(t, v1.VersionNo, ref.VersionNo)
	require.NotNil(t, env.runtime.Provider("primary"), "发布后运行时目录必须含该连接")
	require.Equal(t, rowsBefore+1, env.operationCount())
}

// TestPublish_ManagedCredentialRevokedFailsClosed：托管凭据已撤销时发布失败关闭——
// 错误保留哨兵（web 层映射 400 CREDENTIAL_REVOKED），文件 / 版本 / 指针 / 操作行全零变化。
// TestPublish_ManagedCredentialRevokedFailsClosed pins the fail-closed revoked path.
func TestPublish_ManagedCredentialRevokedFailsClosed(t *testing.T) {
	t.Parallel()
	source := &fixedManagedCredentials{key: "sk-managed-revoke-plaintext"}
	env, _ := newManagedModelEnv(t, source)
	v1 := env.managePrimary(t, "sk-managed-revoke-plaintext", "https://api.example.com/v1")
	source.err = core.ErrCredentialRevoked
	before := env.readConfig()
	rowsBefore := env.operationCount()

	_, err := env.admin.Publish(context.Background(), resourceActor(),
		core.ResourcePublishRequest{Kind: core.ResourceModelConnection, ID: "primary", VersionID: &v1.ID})
	require.ErrorIs(t, err, core.ErrCredentialRevoked)
	require.Equal(t, before, env.readConfig(), "失败发布不得改写定义文件")
	require.Equal(t, rowsBefore, env.operationCount(), "客户端构建在预写之前，失败不留操作行")
	require.Len(t, env.versionsOf("primary"), 1, "失败发布不得追加版本")
	require.Equal(t, v1.ID, env.pointer("primary").PublishedVersionID)
}

// TestModelConnection_ManagedSavePreflightFailsZeroWrites：主密钥不可用 → 503、定义
// 文件逐字节不变、零操作行、零凭据写入、目录不新增条目（预检在写盘之前）。
// TestModelConnection_ManagedSavePreflightFailsZeroWrites pins the zero-write 503 path.
func TestModelConnection_ManagedSavePreflightFailsZeroWrites(t *testing.T) {
	t.Parallel()
	source := &fixedManagedCredentials{key: "sk-managed-save-plaintext"}
	recorder := &credentialRecorder{preflightErr: credentialUnavailableError("夹具：主密钥不可读")}
	env := newResourceEnv(t, func(deps *ResourceAdminDeps) {
		deps.CredentialSource = source
		deps.CredentialWriter = recorder
	})
	before := env.readConfig()

	_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
		ModelConnectionRequest{
			Name:           "primary",
			BaseURL:        "https://api.example.com/v1",
			APIKey:         "sk-managed-save-plaintext",
			APIKeySet:      true,
			DefaultModel:   "gpt-primary",
			CredentialMode: "managed",
			Create:         true,
		})
	require.ErrorIs(t, err, core.ErrCredentialUnavailable)
	require.Equal(t, before, env.readConfig(), "预检失败不得写定义文件")
	require.Zero(t, env.operationCount(), "预检失败不得留下操作行")
	require.Empty(t, recorder.writes)
	require.Empty(t, source.refs)
	require.Nil(t, env.runtime.Provider("primary"))
}

// TestModelConnection_TestManagedCredentials：托管连接的连接测试必须经凭据解析器按
// **已发布版本**取 Key，而不是把托管条目当成缺 `${VAR}` 占位拒绝（2026-10-24 在 8080
// 真实配置上实测到的 503 NOT_CONFIGURED 回归点）。解析引用必须带版本号与内容哈希，
// 测试与运行时目录同源。
// TestModelConnection_TestManagedCredentials pins the managed test-path key resolution.
func TestModelConnection_TestManagedCredentials(t *testing.T) {
	t.Parallel()
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		Body: `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`,
	})
	source := &fixedManagedCredentials{key: "sk-managed-test-plaintext"}
	env, _ := newManagedModelEnv(t, source)
	version := env.managePrimary(t, "sk-managed-test-plaintext", srv.URL())
	require.Len(t, source.refs, 1, "保存路径解析恰一次")

	result, err := env.admin.TestModelConnection(context.Background(), resourceActor(), "primary")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, "gpt-primary", result.Model)

	require.Len(t, source.refs, 2, "测试路径必须再解析一次已发布版本")
	ref := source.refs[len(source.refs)-1]
	require.Equal(t, "primary", ref.Name)
	require.Equal(t, version.ResourceUID, ref.ResourceUID)
	require.Equal(t, version.ID, ref.VersionID)
	require.Equal(t, version.VersionNo, ref.VersionNo)
	require.NotEmpty(t, ref.ContentHash)
}

// TestModelConnection_ManagedRetainKeyAndSwitchToEnv：省略凭据字段只改元数据 → 不写
// 凭据段、目录客户端保持；显式 `credential_mode: env` + 精确占位 → 凭据段以 `env`
// 模式退役托管身份（无值），定义改回 `api_key` 占位，客户端按 env 规则重建。
// TestModelConnection_ManagedRetainKeyAndSwitchToEnv covers both non-write transitions.
func TestModelConnection_ManagedRetainKeyAndSwitchToEnv(t *testing.T) {
	t.Parallel()
	source := &fixedManagedCredentials{key: "sk-managed-save-plaintext"}
	env, recorder := newManagedModelEnv(t, source)

	const base = "https://api.example.com/v1"
	env.managePrimary(t, "sk-managed-save-plaintext", base)
	retained := env.runtime.Provider("primary")
	require.NotNil(t, retained)

	t.Run("省略凭据字段保留 Key", func(t *testing.T) {
		updated, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
			ModelConnectionRequest{
				Name:         "primary",
				BaseURL:      base,
				DefaultModel: "gpt-primary-2",
				IfMatch:      env.currentETag(t, "primary"),
			})
		require.NoError(t, err)
		require.Len(t, recorder.writes, 1, "保留旧 Key 不得写凭据段")
		require.Len(t, source.refs, 1, "保留旧 Key 不得重新解析")
		require.Same(t, retained, env.runtime.Provider("primary"), "地址未变的保留路径沿用同一客户端")
		require.NotContains(t, updated.DefinitionYAML, "api_key", "托管模式的定义不得出现 api_key")
	})

	t.Run("显式切回 env 退役身份", func(t *testing.T) {
		etag := env.currentETag(t, "primary")
		_, err := env.admin.UpsertModelConnection(context.Background(), resourceActor(),
			ModelConnectionRequest{
				Name:           "primary",
				BaseURL:        base,
				DefaultModel:   "gpt-primary-2",
				APIKey:         "${" + resourceTestKeyEnv + "}",
				APIKeySet:      true,
				CredentialMode: "env",
				IfMatch:        etag,
			})
		require.NoError(t, err)
		require.Len(t, recorder.writes, 2)
		require.Equal(t, core.CredentialModeEnv, recorder.writes[1].Mode)
		require.Nil(t, recorder.writes[1].Value, "退役只改状态，不带值")
		require.Contains(t, env.readConfig(), "api_key: ${"+resourceTestKeyEnv+"}")
		require.NotContains(t, env.readConfig(), "credential_mode: managed")
		require.NotNil(t, env.runtime.Provider("primary"), "切回 env 后按占位重建客户端")
	})
}
