package app

// 本文件是 `internal/app/definition_reconcile.go` 的验收面（颗粒度文档 §3.2.1：
// app 的 definition_reconcile_test.go，12 例；约束 34 / 注 7）：
//
//	R1  `TestReconcile_PublishesValidEdit`——文件新且候选合法 → 新版本 + 发布 + applied 同步；
//	    操作行 actor / action / source / 终态逐项核对；第二次对账幂等（不新增版本 / 操作行）
//	R2  `TestReconcile_InvalidEditKeepsPublished`——模块卡 §4.3.4 原样落地（非法编辑保留旧版本）
//	R3  `TestReconcile_NoChangeSkipsEverything`——相同哈希跳过：零版本、零操作行、零 WARN
//	R4  `TestReconcile_IgnoresRemoval`——条目被删 → 忽略 + WARN、指针不动、零操作行
//	R5  `TestReconcile_MissingEnvKeepsPublished`——候选占位缺失 → 保留旧版本、零写入（约束 7）
//	R6  `TestReconcile_FileUnparseableKeepsPublished`——文件结构不可解析 → 保留全部已发布版本
//	R7  `TestReconcile_SkillWorkCopyPublishes`——技能工作副本变更 → 新版本 + 不可变快照 + applied 同步
//	R8  `TestReconcile_SkillSameContentIdempotent`——技能相同清单哈希 → 无动作、零操作行
//	R9  `TestReconcile_SkillMissingWorkCopyIgnored`——工作副本被删 → 忽略 + WARN、指针不动
//	R10 `TestReconcile_PublishedMissingRefusesStartup`——文件 / 工作副本有、库里无发布版本 → 拒绝启动
//	R11 `TestReconcile_PublishFailureFailsStartup`——提交事务失败 → 操作 failed、指针保旧、拒绝启动
//	R12 `TestReconcile_ConcurrentStartup`——并发对账无数据竞争、版本号不重复、指针内容与文件一致
//
// T049 接入 MCP 分支（约束 34 的 MCP 行 + 约束 21）后的追加用例：
//
//	R13 `TestReconcile_MCPExternalEditPublishesPendingRestart`——MCP 外部编辑 → 新版本发布但
//	    applied 不推进（待重启）；第二次对账幂等
//	R14 `TestReconcile_MCPNoChangeSkipsEverything`——MCP 相同哈希 → 无动作、零操作行
//	R15 `TestReconcile_MCPInvalidEntryKeepsPublished`——MCP 候选非法 → 保留旧版本、零写入
//	R16 `TestReconcile_MCPMissingEnvKeepsPublished`——MCP 占位缺失 → 保留旧版本、零写入
//	R17 `TestReconcile_MCPRemovalIgnored`——MCP 条目被删 → 忽略 + WARN、指针不动
//	R18 `TestReconcile_MCPPublishedMissingRefusesStartup`——MCP 文件条目无发布版本 → 拒绝启动
//
// 判据只看真实磁盘与真实数据库行：定义文件、技能工作副本 / 快照目录、
// `management_operations`、`resource_versions` 都由真实实现写；唯一的注入点是
// `PublishVersion` 的事务失败。
// Every judgement below reads real files and real database rows; the only injection
// point is the transactional publish failure.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// reconcileConfigHeader 是夹具定义文件的初始内容：没有任何条目，对账因此只看已发布
// 指针，不会被夹具自身的条目干扰。
// reconcileConfigHeader starts the fixture file without any entry.
const reconcileConfigHeader = "# 启动对账夹具\n"

// reconcileEnv 是启动对账夹具：临时工作区（技能库根 = 工作区根）+ 真实定义文件 + 真实
// SQLite + 共享闸门 + 可注入的提交失败点。技能工作副本在 `skills/<name>/`，不可变版本在
// `versions/skills/<name>/<no>/`（`SkillLibrary` 的根口径是 `.basalt/`）。
// reconcileEnv wires a real workspace, a real database and an injectable commit
// failure point.
type reconcileEnv struct {
	t          *testing.T
	root       string
	configPath string
	mcpPath    string
	store      *storage.ResourceStore
	ops        *storage.ManagementStore
	skills     *workspace.SkillLibrary
	gate       *core.ResourceGate
	logger     *slog.Logger
	logs       *bytes.Buffer

	mu       sync.Mutex
	failNext error
}

// newReconcileEnv 建夹具：临时目录即工作区根，定义文件由用例按需覆盖。MCP 定义文件
// 同样先落一个只有注释头的版本——对账要求路径必填，缺文件会让「文件缺失」分支掩盖
// 本可判定的行为。
// newReconcileEnv builds the fixture with both definition files present.
func newReconcileEnv(t *testing.T) *reconcileEnv {
	t.Helper()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "skills"), 0o755))
	configPath := filepath.Join(root, "basalt.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(reconcileConfigHeader), 0o600))
	mcpPath := filepath.Join(root, mcpServersFileName)
	require.NoError(t, os.WriteFile(mcpPath, []byte("# MCP 定义夹具\n"), 0o600))

	dbPath := filepath.Join(root, "basalt.db")
	db, err := storage.OpenWritable(context.Background(), dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	logger, logs := newCapturedLogger()
	return &reconcileEnv{
		t:          t,
		root:       root,
		configPath: configPath,
		mcpPath:    mcpPath,
		store:      storage.NewResourceStore(db),
		ops:        storage.NewManagementStore(db),
		skills:     workspace.NewSkillLibrary(root),
		gate:       &core.ResourceGate{},
		logger:     logger,
		logs:       logs,
	}
}

// deps 组装对账入参：时钟固定、用户名固定（审计行断言不依赖跑测试的账户）。
// deps assembles the reconcile input with a fixed clock and user name.
func (e *reconcileEnv) deps() ReconcileDeps {
	return ReconcileDeps{
		ConfigPath:     e.configPath,
		MCPServersPath: e.mcpPath,
		Store:          e.store,
		Operations:     e.ops,
		Skills:         e.skills,
		Gate:           e.gate,
		Logger:         e.logger,
		Now:            func() time.Time { return baselineClock() },
		UserName:       "reconcile-test",
		PublishVersion: e.publishVersion,
	}
}

// publishVersion 是提交段的失败注入点：`failNext` 非空时这一次提交返回错误，
// 其余情况走真实的同事务实现（`applied` 原样透传——MCP 的对账发布必须保持待重启）。
// publishVersion injects at most one commit failure and forwards applied verbatim.
func (e *reconcileEnv) publishVersion(ctx context.Context, v core.ResourceVersion, applied bool,
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
func (e *reconcileEnv) failNextVersionInsert(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failNext = err
}

// writeConfig 覆盖定义文件（模拟手工编辑）——签名与模块卡原样用例一致。
// writeConfig overwrites the definition file, mirroring the card's verbatim case.
func (e *reconcileEnv) writeConfig(t *testing.T, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(e.configPath, []byte(content), 0o600))
}

// writeProviders 按生产线格式写 `providers:` 块。
// writeProviders writes a providers block in production form.
func (e *reconcileEnv) writeProviders(t *testing.T, entries ...string) {
	t.Helper()
	e.writeConfig(t, providerConfigText(entries...))
}

// pointer 取资源发布指针。
// pointer returns the publish pointer.
func (e *reconcileEnv) pointer(kind core.ResourceKind, id string) core.ResourcePointer {
	e.t.Helper()
	ptr, err := e.store.Pointer(context.Background(), kind, id)
	require.NoError(e.t, err)
	return ptr
}

// versionsOf 取资源的全部版本（新到旧）。
// versionsOf returns every version, newest first.
func (e *reconcileEnv) versionsOf(kind core.ResourceKind, id string) []core.ResourceVersion {
	e.t.Helper()
	versions, err := e.store.Versions(context.Background(), kind, id)
	require.NoError(e.t, err)
	return versions
}

// listOperations 按过滤条件取操作行。
// listOperations returns the operation rows matching the filter.
func (e *reconcileEnv) listOperations(filter core.OperationFilter) []core.OperationRecord {
	e.t.Helper()
	rows, err := e.ops.ListOperations(context.Background(), filter)
	require.NoError(e.t, err)
	return rows
}

// operationCount 数全部操作行。
// operationCount counts every operation row.
func (e *reconcileEnv) operationCount() int {
	e.t.Helper()
	return len(e.listOperations(core.OperationFilter{Limit: 200}))
}

// providerEntryText 渲染一条 provider 的块式列表条目（两个空格缩进，与定义文件一致）。
// providerEntryText renders one block-style provider entry.
func providerEntryText(name, baseURL, model string) string {
	return "  - name: " + name + "\n" +
		"    base_url: " + baseURL + "\n" +
		"    api_key: ${" + resourceTestKeyEnv + "}\n" +
		"    default_model: " + model + "\n"
}

// providerConfigText 把若干条目拼成完整定义文件。
// providerConfigText joins entries into a whole definition file.
func providerConfigText(entries ...string) string {
	return "providers:\n" + strings.Join(entries, "")
}

// providerDefinitionFor 按基线 / 对账同一份切块实现，从条目原文得到已发布定义的字节。
// providerDefinitionFor derives the published definition bytes through the same
// extraction used by the baseline.
func providerDefinitionFor(t *testing.T, entry string) []byte {
	t.Helper()
	blocks, err := extractDefinitionBlocks([]byte(providerConfigText(entry)), providersBlockKey)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	return blocks[0].content
}

// mcpEntryText 渲染一条 mcp server 的块式列表条目（两个空格缩进，与定义文件一致）。
// mcpEntryText renders one block-style mcp server entry.
func mcpEntryText(name, command string) string {
	return "  - name: " + name + "\n" +
		"    transport: stdio\n" +
		"    command: " + command + "\n"
}

// mcpConfigText 把若干条目拼成完整的 MCP 定义文件。
// mcpConfigText joins entries into a whole mcp_servers.yaml.
func mcpConfigText(entries ...string) string {
	return "servers:\n" + strings.Join(entries, "")
}

// mcpDefinitionFor 按基线同一份切块实现，从条目原文得到已发布定义的字节。
// mcpDefinitionFor derives the published definition bytes through the same extraction
// used by the baseline.
func mcpDefinitionFor(t *testing.T, entry string) []byte {
	t.Helper()
	blocks, err := extractDefinitionBlocks([]byte(mcpConfigText(entry)), mcpServersDefinitionsKey)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	return blocks[0].content
}

// writeMCP 覆盖 MCP 定义文件（模拟手工编辑）。
// writeMCP overwrites the MCP definition file, mirroring a manual edit.
func (e *reconcileEnv) writeMCP(t *testing.T, entries ...string) {
	t.Helper()
	require.NoError(t, os.WriteFile(e.mcpPath, []byte(mcpConfigText(entries...)), 0o600))
}

// resourceUIDFor 为夹具的种子发布取身份：名的当前指针已存在就复用（更新语义），否则
// 新分配一个（v3 起版本行必带 UID，空值会被存储层按非法身份拒绝）。
// resourceUIDFor reuses the current pointer's uid or mints one for a fresh seed.
func (e *reconcileEnv) resourceUIDFor(kind core.ResourceKind, name string) core.ResourceUID {
	if ptr, err := e.store.Pointer(context.Background(), kind, name); err == nil {
		return ptr.ResourceUID
	}
	uid, err := core.NewResourceUID()
	require.NoError(e.t, err)
	return uid
}

// publishMCP 建**一条已应用**的 MCP server（直接写版本与指针）：applied=true 代表
// 上一进程已成功连接并接管，于是后续对账发布可以用“applied 是否被推进”来判待重启语义。
// publishMCP seeds one applied MCP server so a later reconcile publish can prove that
// applied stays behind published.
func (e *reconcileEnv) publishMCP(t *testing.T, name, command string) core.ResourceVersion {
	t.Helper()
	content := mcpDefinitionFor(t, mcpEntryText(name, command))
	contentHash := baselineResourceHash(string(core.ResourceMCPServer), name, content)
	operationID := e.seedOperation(core.ResourceMCPServer, name, contentHash)
	version, err := e.store.PublishWithVersionState(context.Background(), core.ResourceVersion{
		Kind:           core.ResourceMCPServer,
		ResourceUID:    e.resourceUIDFor(core.ResourceMCPServer, name),
		ResourceID:     name,
		ContentHash:    contentHash,
		DefinitionYAML: string(content),
		OperationID:    operationID,
		CreatedAt:      baselineClock().UnixMilli(),
	}, true, operationID, baselineClock())
	require.NoError(t, err)
	return version
}

// seedOperation 为夹具的初始发布预写一条真实操作行（`resource_versions.operation_id`
// 是 NOT NULL，且提交段要回填这条行）。
// seedOperation pre-writes the operation row of one fixture publication.
func (e *reconcileEnv) seedOperation(kind core.ResourceKind, id, requestHash string) string {
	e.t.Helper()
	now := baselineClock()
	operationID := core.NewOperationID(now)
	action := core.ActionModelConnectionCreate
	targetKind := core.TargetKindModelConnection
	switch kind {
	case core.ResourceSkill:
		action = core.ActionSkillCreate
		targetKind = core.TargetKindSkill
	case core.ResourceMCPServer:
		action = core.ActionMCPServerCreate
		targetKind = core.TargetKindMCPServer
	}
	err := e.ops.BeginOperation(context.Background(), core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   targetKind,
		TargetID:     id,
		ActorSubject: "test",
		AuthMethod:   core.AuthMethodLocalTrust,
		Source:       "127.0.0.1:41001",
		RequestHash:  requestHash,
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	})
	require.NoError(e.t, err)
	return operationID
}

// publishProvider 建**一条已发布的模型连接**（直接写版本与指针，不经过管理台写路径）：
// 内容就是 `providerEntryText` 在定义文件里的条目原文，与基线写入形态逐字节一致。
// publishProvider seeds one published model connection in the baseline byte shape.
func (e *reconcileEnv) publishProvider(t *testing.T, name, baseURL, model string) core.ResourceVersion {
	t.Helper()
	content := providerDefinitionFor(t, providerEntryText(name, baseURL, model))
	contentHash := baselineResourceHash(string(core.ResourceModelConnection), name, content)
	operationID := e.seedOperation(core.ResourceModelConnection, name, contentHash)
	version, err := e.store.PublishWithVersion(context.Background(), core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    e.resourceUIDFor(core.ResourceModelConnection, name),
		ResourceID:     name,
		ContentHash:    contentHash,
		DefinitionYAML: string(content),
		OperationID:    operationID,
		CreatedAt:      baselineClock().UnixMilli(),
	}, operationID, baselineClock())
	require.NoError(t, err)
	return version
}

// writeSkill 在工作副本里落一份技能（覆盖已有 SKILL.md）。
// writeSkill writes one skill work copy.
func (e *reconcileEnv) writeSkill(t *testing.T, name, description string) {
	t.Helper()
	dir := filepath.Join(e.root, "skills", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	doc := "---\nname: " + name + "\ndescription: " + description + "\n---\n# " + name + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(doc), 0o600))
}

// publishSkill 建**一条已发布的技能**：先按工作副本冻结 v1 快照，再写版本与指针。
// publishSkill seeds one published skill with its v1 snapshot.
func (e *reconcileEnv) publishSkill(t *testing.T, name string) core.ResourceVersion {
	t.Helper()
	work, err := e.skills.HashWorkCopy(name)
	require.NoError(t, err)
	materialized, err := e.skills.Materialize(context.Background(), name, e.resourceUIDFor(core.ResourceSkill, name), 1, work.ContentHash)
	require.NoError(t, err)
	manifest, err := marshalSkillFileList(materialized.Files)
	require.NoError(t, err)
	operationID := e.seedOperation(core.ResourceSkill, name, materialized.ContentHash)
	version, err := e.store.PublishWithVersion(context.Background(), core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceUID:  e.resourceUIDFor(core.ResourceSkill, name),
		ResourceID:   name,
		ContentHash:  materialized.ContentHash,
		ManifestJSON: string(manifest),
		StoragePath:  materialized.Dir,
		OperationID:  operationID,
		CreatedAt:    baselineClock().UnixMilli(),
	}, operationID, baselineClock())
	require.NoError(t, err)
	return version
}

// R1：文件新且候选合法 → 新版本 + 发布 + applied 同步；第二次对账幂等。
// TestReconcile_PublishesValidEdit covers the publish decision and its idempotency.
func TestReconcile_PublishesValidEdit(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishProvider(t, "main", "https://a.example", "m1")
	changed := providerEntryText("main", "https://b.example", "m2")
	env.writeProviders(t, changed)

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Published: 1}, result)

	pointer := env.pointer(core.ResourceModelConnection, "main")
	require.NotEqual(t, v1.ID, pointer.PublishedVersionID)
	require.NotNil(t, pointer.AppliedVersionID)
	require.Equal(t, pointer.PublishedVersionID, *pointer.AppliedVersionID,
		"Provider 的 applied 必须与 published 同步")

	published, err := env.store.VersionByID(context.Background(), pointer.PublishedVersionID)
	require.NoError(t, err)
	require.Equal(t, 2, published.VersionNo)
	require.Equal(t, string(providerDefinitionFor(t, changed)), published.DefinitionYAML,
		"发布内容必须是文件里的条目原文，不得重新序列化")
	require.Equal(t, baselineResourceHash(string(core.ResourceModelConnection), "main",
		providerDefinitionFor(t, changed)), published.ContentHash)

	operations := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionUpdate})
	require.Len(t, operations, 1)
	operation := operations[0]
	require.Equal(t, core.TargetKindModelConnection, operation.TargetKind)
	require.Equal(t, "main", operation.TargetID)
	require.Equal(t, "cli:reconcile-test", operation.ActorSubject)
	require.Equal(t, core.AuthMethodCLI, operation.AuthMethod)
	require.Equal(t, "cli", operation.Source)
	require.Equal(t, core.OperationSuccess, operation.Status)
	require.JSONEq(t, `{"source":"startup_reconcile"}`, string(operation.DetailJSON))
	require.Equal(t, published.OperationID, operation.OperationID)

	// 重复启动（注 7）：同一份文件再对账一次 → 无新版本、无新操作行。
	versionsBefore := len(env.versionsOf(core.ResourceModelConnection, "main"))
	operationsBefore := env.operationCount()
	second, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Unchanged: 1}, second)
	require.Len(t, env.versionsOf(core.ResourceModelConnection, "main"), versionsBefore)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Equal(t, pointer.PublishedVersionID,
		env.pointer(core.ResourceModelConnection, "main").PublishedVersionID)
}

// R2：模块卡 §4.3.4 原样落地——非法编辑保留已发布版本。
// TestReconcile_InvalidEditKeepsPublished is the card's verbatim regression.
func TestReconcile_InvalidEditKeepsPublished(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishProvider(t, "main", "https://a.example", "m1")
	env.writeConfig(t, "providers:\n  - name: main\n    base_url: \"\"\n") // 非法

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	if err != nil {
		t.Fatalf("ReconcileDefinitions: %v", err)
	}
	if result.Retained != 1 || result.Published != 0 {
		t.Fatalf("result = %+v", result)
	}
	ptr, _ := env.store.Pointer(context.Background(), core.ResourceModelConnection, "main")
	if ptr.PublishedVersionID != v1.ID {
		t.Fatalf("published changed to %d", ptr.PublishedVersionID)
	}
}

// R3：相同哈希 → 无动作、零操作行、零 WARN。
// TestReconcile_NoChangeSkipsEverything covers the unchanged decision.
func TestReconcile_NoChangeSkipsEverything(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	env.publishProvider(t, "main", "https://a.example", "m1")
	env.writeProviders(t, providerEntryText("main", "https://a.example", "m1"))
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Unchanged: 1}, result)
	require.Equal(t, operationsBefore, env.operationCount())
	require.NotContains(t, env.logs.String(), "level=WARN")
}

// R4：条目被删 → 忽略 + WARN 点名、指针不动、零操作行（删除只能经 DELETE API）。
// TestReconcile_IgnoresRemoval covers the removal decision.
func TestReconcile_IgnoresRemoval(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishProvider(t, "main", "https://a.example", "m1")
	env.writeConfig(t, "# 运维手工删除了条目（不被接受为删除）\n")
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Ignored: 1}, result)
	require.Equal(t, v1.ID, env.pointer(core.ResourceModelConnection, "main").PublishedVersionID)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Contains(t, env.logs.String(), "保留已发布版本（删除只能经 DELETE API）")
	require.Contains(t, env.logs.String(), "resource=main")
}

// R5：候选占位缺失 → 保留旧版本、零写入（约束 7：只解析本资源条目的占位）。
// TestReconcile_MissingEnvKeepsPublished covers the missing-placeholder branch.
func TestReconcile_MissingEnvKeepsPublished(t *testing.T) {
	const unsetKey = "BASALT_RECONCILE_UNSET_KEY"
	t.Setenv(unsetKey, "")
	env := newReconcileEnv(t)
	v1 := env.publishProvider(t, "main", "https://a.example", "m1")
	env.writeProviders(t, "  - name: main\n    base_url: https://b.example\n"+
		"    api_key: ${"+unsetKey+"}\n    default_model: m2\n")
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Retained: 1}, result)
	require.Equal(t, v1.ID, env.pointer(core.ResourceModelConnection, "main").PublishedVersionID)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Contains(t, env.logs.String(), unsetKey)
}

// fixedReconcileBindings 是托管凭据核对面的替身：记录每次固定的目标绑定，可注入失败。
// fixedReconcileBindings records each pinned target binding and can fail on demand.
type fixedReconcileBindings struct {
	err  error
	pins []reconcilePin
}

// reconcilePin 是一次核对收到的身份与目标绑定。
type reconcilePin struct {
	uid    core.ResourceUID
	target string
}

// PinModelCredential 记录入参并返回固定 pin（或注入的错误）。
// PinModelCredential records the call and returns a fixed pin or the injected error.
func (f *fixedReconcileBindings) PinModelCredential(_ context.Context,
	uid core.ResourceUID, targetBinding string,
) (core.CredentialPin, error) {
	f.pins = append(f.pins, reconcilePin{uid: uid, target: targetBinding})
	if f.err != nil {
		return core.CredentialPin{}, f.err
	}
	return core.CredentialPin{Mode: core.CredentialModeManaged, TargetBinding: targetBinding}, nil
}

// managedProviderEntryText 渲染一条托管模式条目：**没有** `api_key` 行。
// managedProviderEntryText renders a managed entry without any api_key line.
func managedProviderEntryText(name, baseURL, model string) string {
	return "  - name: " + name + "\n" +
		"    base_url: " + baseURL + "\n" +
		"    credential_mode: managed\n" +
		"    default_model: " + model + "\n"
}

// publishManagedProvider 建**一条已发布的托管模型连接**（直接写版本与指针）。
// publishManagedProvider seeds one published managed model connection.
func (e *reconcileEnv) publishManagedProvider(t *testing.T, name, baseURL, model string) core.ResourceVersion {
	t.Helper()
	content := providerDefinitionFor(t, managedProviderEntryText(name, baseURL, model))
	contentHash := baselineResourceHash(string(core.ResourceModelConnection), name, content)
	operationID := e.seedOperation(core.ResourceModelConnection, name, contentHash)
	version, err := e.store.PublishWithVersion(context.Background(), core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    e.resourceUIDFor(core.ResourceModelConnection, name),
		ResourceID:     name,
		ContentHash:    contentHash,
		DefinitionYAML: string(content),
		OperationID:    operationID,
		CreatedAt:      baselineClock().UnixMilli(),
	}, operationID, baselineClock())
	require.NoError(t, err)
	return version
}

// R5b：托管条目没有 `${VAR}` 占位——手改候选必须按「已存凭据能否配合候选定义」核对，
// 通过则发布新版本（2026-10-24 复核发现：此前被误判为缺占位而永远保留旧版本）。
// 逆改点：对账继续走 env-only 解析 → 托管候选被误判、本用例 Retained 而非 Published。
// TestReconcile_ManagedEditPublishes pins the managed candidate path.
func TestReconcile_ManagedEditPublishes(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishManagedProvider(t, "primary", "https://api.example.com/v1", "m1")
	guard := &fixedReconcileBindings{}
	deps := env.deps()
	deps.Credentials = guard
	env.writeProviders(t, managedProviderEntryText("primary", "https://api.example.com/v1", "m2"))

	result, err := ReconcileDefinitions(context.Background(), deps)
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Published: 1}, result)
	pointer := env.pointer(core.ResourceModelConnection, "primary")
	require.NotEqual(t, v1.ID, pointer.PublishedVersionID)

	require.Len(t, guard.pins, 1, "候选核对恰一次")
	require.Equal(t, v1.ResourceUID, guard.pins[0].uid)
	wantTarget, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
	require.NoError(t, err)
	require.Equal(t, wantTarget, guard.pins[0].target, "目标绑定按候选 base_url 重算")
}

// R5c：托管凭据已撤销 / 无解析面 → 保留旧版本、零写入、零操作行。
// TestReconcile_ManagedRevokedKeepsPublished pins the fail-closed branch.
func TestReconcile_ManagedRevokedKeepsPublished(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishManagedProvider(t, "primary", "https://api.example.com/v1", "m1")
	guard := &fixedReconcileBindings{err: core.ErrCredentialRevoked}
	deps := env.deps()
	deps.Credentials = guard
	env.writeProviders(t, managedProviderEntryText("primary", "https://api.example.com/v1", "m2"))
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), deps)
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Retained: 1}, result)
	require.Equal(t, v1.ID, env.pointer(core.ResourceModelConnection, "primary").PublishedVersionID)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Contains(t, env.logs.String(), "primary")
}

// R5d：没有凭据解析面时托管候选失败关闭（保留旧版本），错误文案不得把托管条目说成
// 缺 `${VAR}` 占位。
// TestReconcile_ManagedWithoutGuardKeepsPublished pins the missing-guard branch.
func TestReconcile_ManagedWithoutGuardKeepsPublished(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishManagedProvider(t, "primary", "https://api.example.com/v1", "m1")
	env.writeProviders(t, managedProviderEntryText("primary", "https://api.example.com/v1", "m2"))

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Retained: 1}, result)
	require.Equal(t, v1.ID, env.pointer(core.ResourceModelConnection, "primary").PublishedVersionID)
	require.Contains(t, env.logs.String(), "没有凭据解析面")
	require.NotContains(t, env.logs.String(), "不是精确 ${VAR} 占位")
}

// R6：文件结构不可解析 → 保留全部已发布版本、零操作行。
// TestReconcile_FileUnparseableKeepsPublished keeps every published version when the
// file itself cannot be parsed.
func TestReconcile_FileUnparseableKeepsPublished(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishProvider(t, "main", "https://a.example", "m1")
	env.writeConfig(t, "providers:\n  - name: main\n    base_url: [\n")
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Retained: 1}, result)
	require.Equal(t, v1.ID, env.pointer(core.ResourceModelConnection, "main").PublishedVersionID)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Contains(t, env.logs.String(), "结构不可解析")
}

// R7：技能工作副本变更 → 新版本 + 不可变快照 + applied 同步 + 操作行。
// TestReconcile_SkillWorkCopyPublishes covers the skill publish decision.
func TestReconcile_SkillWorkCopyPublishes(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	env.writeSkill(t, "brief", "v1")
	v1 := env.publishSkill(t, "brief")
	env.writeSkill(t, "brief", "v2")

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Published: 1}, result)

	pointer := env.pointer(core.ResourceSkill, "brief")
	require.NotEqual(t, v1.ID, pointer.PublishedVersionID)
	require.NotNil(t, pointer.AppliedVersionID)
	require.Equal(t, pointer.PublishedVersionID, *pointer.AppliedVersionID)

	published, err := env.store.VersionByID(context.Background(), pointer.PublishedVersionID)
	require.NoError(t, err)
	require.Equal(t, 2, published.VersionNo)
	uid := env.resourceUIDFor(core.ResourceSkill, "brief")
	require.Equal(t, ".basalt/versions/skills/brief/"+string(uid)+"/2", published.StoragePath,
		"对账发布的新版本按身份分段（同名重建换代不撞路径）")
	require.FileExists(t, filepath.Join(env.root, "versions", "skills", "brief", string(uid), "2", "SKILL.md"))
	require.NotEmpty(t, published.ManifestJSON)

	operations := env.listOperations(core.OperationFilter{Action: core.ActionSkillUpdate})
	require.Len(t, operations, 1)
	require.Equal(t, core.TargetKindSkill, operations[0].TargetKind)
	require.Equal(t, "brief", operations[0].TargetID)
	require.Equal(t, "cli:reconcile-test", operations[0].ActorSubject)
	require.Equal(t, core.OperationSuccess, operations[0].Status)
}

// R8：技能清单哈希相同 → 无动作、零操作行。
// TestReconcile_SkillSameContentIdempotent covers the unchanged skill decision.
func TestReconcile_SkillSameContentIdempotent(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	env.writeSkill(t, "brief", "v1")
	env.publishSkill(t, "brief")
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Unchanged: 1}, result)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Len(t, env.versionsOf(core.ResourceSkill, "brief"), 1)
}

// R9：工作副本被删 → 忽略 + WARN、指针不动。
// TestReconcile_SkillMissingWorkCopyIgnored covers the skill removal decision.
func TestReconcile_SkillMissingWorkCopyIgnored(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	env.writeSkill(t, "brief", "v1")
	v1 := env.publishSkill(t, "brief")
	require.NoError(t, os.RemoveAll(filepath.Join(env.root, "skills", "brief")))
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Ignored: 1}, result)
	require.Equal(t, v1.ID, env.pointer(core.ResourceSkill, "brief").PublishedVersionID)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Contains(t, env.logs.String(), "技能工作副本已不存在")
}

// R10：文件 / 工作副本有、库里没有发布版本 → 拒绝启动（约束 34 第 5 行）。
// TestReconcile_PublishedMissingRefusesStartup refuses startup for both kinds.
func TestReconcile_PublishedMissingRefusesStartup(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		t.Parallel()
		env := newReconcileEnv(t)
		env.writeProviders(t, providerEntryText("orphan", "https://o.example", "m9"))

		_, err := ReconcileDefinitions(context.Background(), env.deps())
		require.Error(t, err)
		require.Contains(t, err.Error(), "没有已发布版本")
		require.Equal(t, 0, env.operationCount())
	})

	t.Run("skill", func(t *testing.T) {
		t.Parallel()
		env := newReconcileEnv(t)
		env.writeSkill(t, "orphan", "v1")

		_, err := ReconcileDefinitions(context.Background(), env.deps())
		require.Error(t, err)
		require.Contains(t, err.Error(), "没有已发布版本")
		require.Equal(t, 0, env.operationCount())
	})
}

// R11：提交事务失败 → 操作 failed、指针保旧、对账返回错误（拒绝启动）。
// TestReconcile_PublishFailureFailsStartup covers the database-failure branch.
func TestReconcile_PublishFailureFailsStartup(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishProvider(t, "main", "https://a.example", "m1")
	env.writeProviders(t, providerEntryText("main", "https://b.example", "m2"))

	env.failNextVersionInsert(errors.New("disk full"))
	_, err := ReconcileDefinitions(context.Background(), env.deps())
	require.Error(t, err)
	require.Contains(t, err.Error(), "disk full")
	require.Equal(t, v1.ID, env.pointer(core.ResourceModelConnection, "main").PublishedVersionID)
	require.Len(t, env.versionsOf(core.ResourceModelConnection, "main"), 1)

	operations := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionUpdate})
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationFailed, operations[0].Status)
	require.Equal(t, "cli:reconcile-test", operations[0].ActorSubject)
}

// R12：并发对账无数据竞争（`-race` 判卷）、版本号不重复、发布指针的内容与文件一致。
// 生产上实例锁保证启动单线程；本用例只证明实现本身不共享可变状态。
// TestReconcile_ConcurrentStartup proves the reconciler is race-free and never
// corrupts version numbers; a single-instance lock still guards production startup.
func TestReconcile_ConcurrentStartup(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	env.publishProvider(t, "main", "https://a.example", "m1")
	changed := providerEntryText("main", "https://b.example", "m2")
	env.writeProviders(t, changed)
	expected := string(providerDefinitionFor(t, changed))

	var wg sync.WaitGroup
	results := make([]ReconcileResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = ReconcileDefinitions(context.Background(), env.deps())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "对账第 %d 个并发调用失败", i)
	}
	for _, result := range results {
		require.Zero(t, result.Retained, "并发对账不允许把合法编辑判成保留")
		require.Zero(t, result.Ignored)
	}

	pointer := env.pointer(core.ResourceModelConnection, "main")
	published, err := env.store.VersionByID(context.Background(), pointer.PublishedVersionID)
	require.NoError(t, err)
	require.Equal(t, expected, published.DefinitionYAML)

	seen := make(map[int]struct{})
	for _, version := range env.versionsOf(core.ResourceModelConnection, "main") {
		_, duplicate := seen[version.VersionNo]
		require.False(t, duplicate, "版本号 %d 重复", version.VersionNo)
		seen[version.VersionNo] = struct{}{}
	}
}

// R13：MCP 外部编辑 → 新版本 + 发布，但 applied **不**推进（约束 21：待重启——新内容
// 要等 supervisor 连接成功才生效）；第二次对账幂等。
// TestReconcile_MCPExternalEditPublishesPendingRestart proves the MCP publish decision
// leaves applied behind published.
func TestReconcile_MCPExternalEditPublishesPendingRestart(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishMCP(t, "search", "/opt/mcp/search-v1")
	changed := mcpEntryText("search", "/opt/mcp/search-v2")
	env.writeMCP(t, changed)

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Published: 1}, result)

	pointer := env.pointer(core.ResourceMCPServer, "search")
	require.NotEqual(t, v1.ID, pointer.PublishedVersionID)
	require.NotNil(t, pointer.AppliedVersionID)
	require.Equal(t, v1.ID, *pointer.AppliedVersionID,
		"MCP 对账发布不得推进 applied——新内容要等 supervisor 连接成功才生效")

	published, err := env.store.VersionByID(context.Background(), pointer.PublishedVersionID)
	require.NoError(t, err)
	require.Equal(t, 2, published.VersionNo)
	require.Equal(t, string(mcpDefinitionFor(t, changed)), published.DefinitionYAML,
		"发布内容必须是文件里的条目原文，不得重新序列化")

	operations := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerUpdate})
	require.Len(t, operations, 1)
	operation := operations[0]
	require.Equal(t, core.TargetKindMCPServer, operation.TargetKind)
	require.Equal(t, "search", operation.TargetID)
	require.Equal(t, "cli:reconcile-test", operation.ActorSubject)
	require.Equal(t, core.AuthMethodCLI, operation.AuthMethod)
	require.Equal(t, "cli", operation.Source)
	require.Equal(t, core.OperationSuccess, operation.Status)
	require.JSONEq(t, `{"source":"startup_reconcile"}`, string(operation.DetailJSON))
	require.Equal(t, published.OperationID, operation.OperationID)

	versionsBefore := len(env.versionsOf(core.ResourceMCPServer, "search"))
	operationsBefore := env.operationCount()
	second, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Unchanged: 1}, second)
	require.Len(t, env.versionsOf(core.ResourceMCPServer, "search"), versionsBefore)
	require.Equal(t, operationsBefore, env.operationCount())
}

// R14：MCP 相同哈希 → 无动作、零操作行、零 WARN。
// TestReconcile_MCPNoChangeSkipsEverything covers the unchanged MCP decision.
func TestReconcile_MCPNoChangeSkipsEverything(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishMCP(t, "search", "/opt/mcp/search")
	env.writeMCP(t, mcpEntryText("search", "/opt/mcp/search"))
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Unchanged: 1}, result)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Equal(t, v1.ID, env.pointer(core.ResourceMCPServer, "search").PublishedVersionID)
	require.NotContains(t, env.logs.String(), "level=WARN")
}

// R15：MCP 候选非法（stdio 缺 command）→ 保留旧版本、指针不动、零操作行。
// TestReconcile_MCPInvalidEntryKeepsPublished keeps the published MCP version when the
// edited entry fails candidate validation.
func TestReconcile_MCPInvalidEntryKeepsPublished(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishMCP(t, "search", "/opt/mcp/search")
	env.writeMCP(t, "  - name: search\n    transport: stdio\n") // 缺 command → 非法
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Retained: 1}, result)
	require.Equal(t, v1.ID, env.pointer(core.ResourceMCPServer, "search").PublishedVersionID)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Contains(t, env.logs.String(), "保留已发布版本")
	require.Contains(t, env.logs.String(), "resource=search")
}

// R16：MCP 占位缺失 → 保留旧版本、指针不动、零操作行（占位解析在发布之前）。
// TestReconcile_MCPMissingEnvKeepsPublished covers the missing-placeholder branch.
func TestReconcile_MCPMissingEnvKeepsPublished(t *testing.T) {
	const unsetKey = "BASALT_RECONCILE_UNSET_MCP_KEY"
	t.Setenv(unsetKey, "")
	env := newReconcileEnv(t)
	v1 := env.publishMCP(t, "search", "/opt/mcp/search")
	env.writeMCP(t, "  - name: search\n    transport: stdio\n    command: /opt/mcp/search-v2\n"+
		"    env:\n      SEARCH_TOKEN: ${"+unsetKey+"}\n")
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Retained: 1}, result)
	require.Equal(t, v1.ID, env.pointer(core.ResourceMCPServer, "search").PublishedVersionID)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Contains(t, env.logs.String(), unsetKey)
}

// R17：MCP 条目被删 → 忽略 + WARN 点名、指针不动、零操作行（删除只能经 DELETE API）。
// TestReconcile_MCPRemovalIgnored covers the removal decision for MCP servers.
func TestReconcile_MCPRemovalIgnored(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	v1 := env.publishMCP(t, "search", "/opt/mcp/search")
	require.NoError(t, os.WriteFile(env.mcpPath, []byte("# 运维手工删除了条目（不被接受为删除）\n"), 0o600))
	operationsBefore := env.operationCount()

	result, err := ReconcileDefinitions(context.Background(), env.deps())
	require.NoError(t, err)
	require.Equal(t, ReconcileResult{Ignored: 1}, result)
	require.Equal(t, v1.ID, env.pointer(core.ResourceMCPServer, "search").PublishedVersionID)
	require.Equal(t, operationsBefore, env.operationCount())
	require.Contains(t, env.logs.String(), "保留已发布版本（删除只能经 DELETE API）")
	require.Contains(t, env.logs.String(), "resource=search")
}

// R18：MCP 文件条目有、库里没有发布版本 → 拒绝启动（约束 34 第 5 行）。
// TestReconcile_MCPPublishedMissingRefusesStartup refuses startup for MCP entries
// without a live pointer.
func TestReconcile_MCPPublishedMissingRefusesStartup(t *testing.T) {
	t.Parallel()
	env := newReconcileEnv(t)
	env.writeMCP(t, mcpEntryText("orphan", "/opt/mcp/orphan"))

	_, err := ReconcileDefinitions(context.Background(), env.deps())
	require.Error(t, err)
	require.Contains(t, err.Error(), "没有已发布版本")
	require.Equal(t, 0, env.operationCount())
}
