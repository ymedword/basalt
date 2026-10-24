package app

// 本文件是 `internal/app/agent_admin.go` 的验收面（颗粒度文档 §3.2.1：内部 app 的
// agent_admin_test.go，25 例；归档 / 复制 / 恢复与归档回滚矩阵在 T033 追加）。
// 本文件当前覆盖创建、读面、多文件提交与作者草稿编排：
//
//	C1 `TestAgentAdmin_CreateDefaultDisabled`——默认停用 / generation=1 / 状态文件记本次操作
//	C2 `TestAgentAdmin_CreateExistingName`——同名目录已存在 → 400、零操作行、零写入
//	C3 `TestAgentAdmin_CreateResidualStateRefused`——孤立状态文件 → 400、不覆盖
//	C4 `TestAgentAdmin_CreateRejectsInvalidName`——名称规则逐条拒绝
//	C5 `TestAgentAdmin_CreateNoResidueOnPreWriteFailure`——预写失败零写入（约束 1）
//	C6 `TestAgentAdmin_CreateNoResidueOnFailure`——Bootstrap 后失败：Discard 收回状态文件（约束 27）
//	C7 `TestAgentAdmin_CreateIndexFailureBestEffort`——索引失败不回滚、detail 记 deferred（约束 37）
//	C8 `TestAgentAdmin_EveryWriteHasOperation`——每个写操作恰有一行终态台账（DoD #10）
//	T1 `TestAgentTemplate_DeterministicProvider`——模板取已发布列表首项（约束 28）
//	F1 `TestAgentAdmin_FilesReturnsCollectionETag`——集合 ETag 与文件清单
//	F2 `TestAgentAdmin_FilesMissingAgent`——未知 Agent → AgentNotFoundError
//	S1 `TestAgentAdmin_SubmitCollectionETag`——条件提交成功、未列出文件保留、审计投影
//	S2 `TestAgentAdmin_SubmitStaleETagNoOperation`——缺失 / 弱 / `*` / 不匹配 → 412 零操作行
//	S3 `TestAgentAdmin_SubmitMissingDefinitionRejected`——候选集合缺 AGENT.md → 400 零操作行
//	S4 `TestAgentAdmin_SubmitInvalidPathNoOperation`——路径越界 → 400 零操作行
//	S5 `TestAgentAdmin_SubmitValidationFailureNoOperation`——加载期校验失败 → 400 零操作行
//	S6 `TestAgentAdmin_SubmitWriteFailureFinishesFailed`——预写后失败 → failed 并如实上报
//	S7 `TestAgentAdmin_SubmitPreWriteFailureZeroWrites`——预写失败零写入
//	A1 `TestAgentAdmin_GenerateFilesNoOperationRow`——草稿只回内存、零操作行、零落盘
//	A2 `TestAgentAdmin_GenerateFilesNotConfigured`——作者未配置 → 503 分类
//	A3 `TestAgentAdmin_GenerateFilesCallErrorPassthrough`——上游调用失败不被改判为 400
//
// 判据只看"磁盘与数据库里发生了什么"：状态文件、目录树、`management_operations` 行都是
// 真实实现写的，不使用调用记录替身。Everything below asserts real files and rows.
//
// 命名说明：模块卡约束 27 的复核清单点名 `TestAgentAdmin_CreateDefaultDisabled`（单数），
// stage 草稿曾写作复数；按 T003 / T019 先例以人审模块卡的精确名为准，控制侧同时把
// stage 的 required_tests 锚点对齐（只改名字，断言与验收语义不动）。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// agentAdminChatter 是作者调用面的记录型替身：零网络，返回预设正文。
// agentAdminChatter records requests and returns a canned body without any network.
type agentAdminChatter struct {
	response string
	err      error
	requests []core.ChatRequest
}

func (c *agentAdminChatter) Chat(_ context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	c.requests = append(c.requests, req)
	if c.err != nil {
		return core.ChatResponse{}, c.err
	}
	return core.ChatResponse{Content: c.response}, nil
}

// agentAdminEnv 是 Agent 写面夹具：临时工作区 + 真库 + 共享闸门 + 固定时钟。
// 已发布 Provider 列表刻意按 `[beta, alpha]` 给出（非字母序），模板取首项的判据
// 因此不会被"恰好按名字排好"掩盖。
// The fixture uses real stores; the published provider list is deliberately ordered
// beta-before-alpha so the template test cannot pass by accidental sorting.
type agentAdminEnv struct {
	admin     *AgentAdmin
	deps      AgentAdminDeps
	db        *storage.DB
	root      string
	ops       *storage.ManagementStore
	versions  *storage.ResourceStore
	lifecycle *workspace.LifecycleStore
	gate      *core.AgentGate
	logs      *bytes.Buffer
	chatter   *agentAdminChatter
	// seededOpID 是按需建立的种子操作标识（`resource_versions.operation_id` 是非空外键，
	// 正常路径由基线或发布写入）。夹具惰性建立，避免给不碰资源表的用例凭空多出一行。
	// seededOpID is created lazily so resource-writing tests get an FK without adding a
	// stray operation row to every other test.
	seededOpID string
}

// agentAdminPublishedProviders 是默认已发布条目：beta 在前、alpha 在后。
// agentAdminPublishedProviders is the default published list, beta first.
func agentAdminPublishedProviders() []workspace.ProviderEntry {
	return []workspace.ProviderEntry{
		{Name: "beta", BaseURL: "https://beta.example.com/v1", DefaultModel: "beta-default"},
		{Name: "alpha", BaseURL: "https://alpha.example.com/v1", DefaultModel: "alpha-default"},
	}
}

// newAgentAdminEnv 建夹具；mutate 在构造前改写入参（注入失败点用）。
// newAgentAdminEnv builds the fixture; mutate adjusts the deps before construction.
func newAgentAdminEnv(t *testing.T, mutate ...func(*AgentAdminDeps)) *agentAdminEnv {
	t.Helper()
	env := newAgentAdminEnvRaw(t, baselineClock)
	for _, m := range mutate {
		m(&env.deps)
	}
	return env.build()
}

// newAgentAdminEnvRaw 只建存储与缺省依赖，**不**构造 AgentAdmin：归档 / 复制 / 恢复
// 夹具需要在构造前注入注册表、调度、Run 取消器与归档存储（它们都依赖同一个库句柄
// 与可推进时钟）。调用方改完 `deps` 后必须调 `build`。
// newAgentAdminEnvRaw builds the stores only; callers inject the activity-surface
// dependencies (which need the same database handle) and then call build.
func newAgentAdminEnvRaw(t *testing.T, now func() time.Time) *agentAdminEnv {
	t.Helper()

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)

	db, err := storage.OpenWritable(context.Background(),
		filepath.Join(root, databaseFileName), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	ops := storage.NewManagementStore(db)
	versions := storage.NewResourceStore(db)
	gate := core.NewAgentGate()
	lifecycle := workspace.NewLifecycleStore(root, now)
	logger, logs := newCapturedLogger()
	chatter := &agentAdminChatter{}

	deps := AgentAdminDeps{
		Root:       root,
		Gate:       gate,
		Lifecycle:  lifecycle,
		Index:      versions,
		Operations: ops,
		Logger:     logger,
		Now:        now,
		// 资源 / 通知渠道是复制期专属判定的只读来源；投影是写操作成功后的重建面。
		// 三者在生产装配里都常驻，夹具同样给真实现，不用替身掩盖"投影没重建"这类缺陷。
		// The exclusivity sources and the projection are wired for real, mirroring the
		// production assembly so a missing rebuild cannot hide behind a fake.
		Resources:   versions,
		NotifyAdmin: storage.NewNotifyAdminStore(db),
		Projection: NewBindingProjection(storage.NewBindingStore(db), versions,
			storage.NewAgentAdminStore(db), root, logger, now),
		PublishedProviders: func(context.Context) ([]workspace.ProviderEntry, error) {
			return agentAdminPublishedProviders(), nil
		},
		PublishedSkills:   func(context.Context) ([]string, error) { return []string{"research"}, nil },
		AppliedMCPServers: func(context.Context) ([]string, error) { return []string{"reference"}, nil },
		AuthorChat:        chatter,
		AuthorTemplate:    "---\nname: <name>\n---\n",
		MaxRequestBytes:   1 << 20,
		ValidateDefinition: func(context.Context, string, []byte) error {
			return nil
		},
	}

	return &agentAdminEnv{
		deps:      deps,
		db:        db,
		root:      root,
		ops:       ops,
		versions:  versions,
		lifecycle: lifecycle,
		gate:      gate,
		logs:      logs,
		chatter:   chatter,
	}
}

// build 用当前 `deps` 构造写面；第二次调用用于在注入包装依赖后重建。
// build constructs the admin from the current deps.
func (env *agentAdminEnv) build() *agentAdminEnv {
	env.admin = NewAgentAdmin(env.deps)
	return env
}

// writeAdminAgent 在活动区落一个可装载的 Agent 目录（提交类用例的基础版本）。
// writeAdminAgent materializes a loadable agent directory as the submit baseline.
func writeAdminAgent(t *testing.T, root, name string) {
	t.Helper()
	writeWorkspaceFile(t, root, "agents/"+name+"/AGENT.md",
		authorAgentMD(name, "alpha", "alpha-model", nil, nil))
}

// readAdminFile 读工作区内的一个文件；缺失直接失败。
// readAdminFile reads one workspace file and fails the test when it is absent.
func readAdminFile(t *testing.T, root, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	require.NoError(t, err)
	return string(raw)
}

// statePath 返回 Agent 状态文件的绝对路径（契约 `control/agents/<name>.json`）。
// statePath returns the absolute lifecycle state-file path.
func statePath(root, name string) string {
	return filepath.Join(root, "control", "agents", name+".json")
}

// seedOperation 惰性建立并返回夹具的种子操作标识（版本行的非空外键）。
// seedOperation lazily creates the fixture's seeding operation id.
func (env *agentAdminEnv) seedOperation(t *testing.T) string {
	t.Helper()
	if env.seededOpID != "" {
		return env.seededOpID
	}
	opID := core.NewOperationID(baselineClock())
	require.NoError(t, env.ops.BeginOperation(context.Background(), core.OperationRecord{
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
	env.seededOpID = opID
	return opID
}

// seedModel 写入并发布一条共享模型连接，返回落库后的版本行（身份 = 当前指针身份）。
// seedModel publishes one shared model connection and returns its stored version.
func (env *agentAdminEnv) seedModel(t *testing.T, name string) core.ResourceVersion {
	t.Helper()
	ctx := context.Background()
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	version, _, err := env.versions.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    uid,
		ResourceID:     name,
		ContentHash:    sha256Hex(t, []byte("model-"+name)),
		DefinitionYAML: "name: " + name + "\nbase_url: https://api.example.com\n",
		CreatedAt:      baselineClock().UnixMilli(),
		OperationID:    env.seedOperation(t),
	})
	require.NoError(t, err)
	require.NoError(t, env.versions.Publish(ctx, core.ResourceModelConnection, name, version.ID,
		true, env.seedOperation(t), baselineClock()))
	return version
}

// makeExclusive 把一条已发布的资源收缩为该 Agent 专属（复制期"源专属"判定的前提）。
// makeExclusive narrows one published resource to the given owner agent.
func (env *agentAdminEnv) makeExclusive(t *testing.T, name string, uid core.ResourceUID,
	owner core.AgentUID,
) {
	t.Helper()
	_, err := env.versions.ConvertScope(context.Background(), storage.ScopeConversion{
		Kind:          core.ResourceModelConnection,
		Name:          name,
		ExpectUID:     uid,
		Target:        core.ScopeExclusive,
		OwnerAgentUID: owner,
		OperationID:   env.seedOperation(t),
		At:            baselineClock(),
	})
	require.NoError(t, err)
}

// countOperations 统计指定动作的管理操作行数（"零操作行"判据）。
// countOperations counts the management operation rows carrying one action.
func countOperations(t *testing.T, store core.OperationStore, action string) int {
	t.Helper()
	count := 0
	for _, row := range listConsoleOperations(t, store) {
		if row.Action == action {
			count++
		}
	}
	return count
}

// requireAgentDirAbsent 断言 Agent 正式目录与 staging / backup 残留都不存在。
// requireAgentDirAbsent asserts that neither the agent directory nor residue exists.
func requireAgentDirAbsent(t *testing.T, root, name string) {
	t.Helper()
	for _, rel := range []string{"agents/" + name, "agents/" + name + ".staging", "agents/" + name + ".backup"} {
		_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		require.ErrorIs(t, err, os.ErrNotExist, "%s 必须不存在", rel)
	}
}

// TestAgentAdmin_CreateDefaultDisabled：创建产物默认停用、generation=1、状态文件的
// operation_id 指向创建操作；响应视图返回落盘后的受管文件集合（约束 27）。
func TestAgentAdmin_CreateDefaultDisabled(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)

	view, err := env.admin.Create(context.Background(), consoleActor(), core.AgentSpec{
		Name:        "alpha",
		Description: "值班助手",
	})
	require.NoError(t, err)

	rec, exists, readErr := env.lifecycle.Read(context.Background(), "alpha")
	require.NoError(t, readErr)
	require.True(t, exists)
	require.Equal(t, core.AgentDisabled, rec.State, "模板创建默认停用")
	require.EqualValues(t, 1, rec.Generation)

	row := findConsoleOperation(t, env.ops, core.ActionAgentCreate)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, agentResultOKForTest(), row.ResultCode)
	require.Equal(t, row.OperationID, rec.OperationID, "状态文件必须记录本次操作标识")
	require.Equal(t, "alpha", row.TargetID)
	require.Equal(t, targetKindAgent, row.TargetKind)

	require.Equal(t, "alpha", view.Agent)
	require.NotNil(t, view.Lifecycle.State)
	require.Equal(t, string(core.AgentDisabled), *view.Lifecycle.State)
	require.True(t, view.Lifecycle.Valid)
	require.NotEmpty(t, view.ETag, "创建成功后响应必须带集合 ETag")
	require.Len(t, view.Files, 1)
	require.Equal(t, "AGENT.md", view.Files[0].Path)

	def := readAdminFile(t, env.root, "agents/alpha/AGENT.md")
	require.Contains(t, def, "name: alpha")
	require.Contains(t, def, "值班助手")
	require.Contains(t, def, "name: beta", "模板 provider 取已发布列表首项")
}

// agentResultOKForTest 是成功终态的 result_code（契约：OK 或错误码表里的 code）。
// agentResultOKForTest mirrors the success result code without importing the const
// from the console file into the assertion text.
func agentResultOKForTest() string { return lifecycleResultOK }

// TestAgentAdmin_CreateExistingName：同名目录已存在 → 400、零操作行、目录原样。
func TestAgentAdmin_CreateExistingName(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	writeAdminAgent(t, env.root, "alpha")
	before := readAdminFile(t, env.root, "agents/alpha/AGENT.md")

	_, err := env.admin.Create(context.Background(), consoleActor(), core.AgentSpec{Name: "alpha"})
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err), "目标已存在是入参问题：%v", err)
	require.Empty(t, listConsoleOperations(t, env.ops), "未开始变更不得留下操作行")
	require.Equal(t, before, readAdminFile(t, env.root, "agents/alpha/AGENT.md"))
	_, statErr := os.Stat(statePath(env.root, "alpha"))
	require.ErrorIs(t, statErr, os.ErrNotExist, "拒绝创建时不得写状态文件")
}

// TestAgentAdmin_CreateResidualStateRefused：目录不在但状态文件残留 → 400 拒绝覆盖。
func TestAgentAdmin_CreateResidualStateRefused(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	_, err := env.lifecycle.Bootstrap(context.Background(), "alpha", "op-seed")
	require.NoError(t, err)
	before, readErr := os.ReadFile(statePath(env.root, "alpha"))
	require.NoError(t, readErr)

	_, err = env.admin.Create(context.Background(), consoleActor(), core.AgentSpec{Name: "alpha"})
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err), "孤立状态文件必须拒绝创建：%v", err)
	require.Empty(t, listConsoleOperations(t, env.ops))
	after, readErr := os.ReadFile(statePath(env.root, "alpha"))
	require.NoError(t, readErr)
	require.Equal(t, before, after, "拒绝创建时状态文件逐字节不变")
	requireAgentDirAbsent(t, env.root, "alpha")
}

// TestAgentAdmin_CreateRejectsInvalidName：名称规则（约束 27 / 约束 41）逐条拒绝。
func TestAgentAdmin_CreateRejectsInvalidName(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)

	for _, name := range []string{"", ".", "..", ".hidden", "alpha.staging", "alpha.backup", "a/b", "a\\b"} {
		_, err := env.admin.Create(context.Background(), consoleActor(), core.AgentSpec{Name: name})
		require.Error(t, err, "name=%q 必须被拒绝", name)
		require.True(t, core.IsInvalidInput(err), "name=%q 是入参问题：%v", name, err)
	}
	require.Empty(t, listConsoleOperations(t, env.ops))
}

// TestAgentAdmin_CreateNoResidueOnPreWriteFailure：预写失败 = 拒绝执行（约束 1），
// 目录、状态文件、staging 一个字节都没动。
func TestAgentAdmin_CreateNoResidueOnPreWriteFailure(t *testing.T) {
	t.Parallel()
	realOps := newAgentAdminEnv(t)
	env := newAgentAdminEnv(t, func(deps *AgentAdminDeps) {
		deps.Operations = failingBeginOperations{OperationStore: realOps.ops, err: errors.New("数据库不可用")}
	})

	_, err := env.admin.Create(context.Background(), consoleActor(), core.AgentSpec{Name: "alpha"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "拒绝执行")
	requireAgentDirAbsent(t, env.root, "alpha")
	_, statErr := os.Stat(statePath(env.root, "alpha"))
	require.ErrorIs(t, statErr, os.ErrNotExist, "预写失败不得写状态文件")
	require.Empty(t, listConsoleOperations(t, realOps.ops), "预写失败连 pending 都不许留下")
	require.Contains(t, env.logs.String(), "预写操作记录失败")
}

// TestAgentAdmin_CreateNoResidueOnFailure：Bootstrap 之后落地失败 → 状态文件经
// `Discard` 收回、目标目录与 staging 都不存在、操作终态不谎报 success（约束 27）。
func TestAgentAdmin_CreateNoResidueOnFailure(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t, func(deps *AgentAdminDeps) {
		deps.CreateStaged = func(string, core.AgentSpec, []string, core.ResourceBindings) ([]core.AgentFileInfo, error) {
			return nil, errors.New("rename 失败")
		}
	})

	_, err := env.admin.Create(context.Background(), consoleActor(), core.AgentSpec{Name: "alpha"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "rename 失败")
	requireAgentDirAbsent(t, env.root, "alpha")
	_, statErr := os.Stat(statePath(env.root, "alpha"))
	require.ErrorIs(t, statErr, os.ErrNotExist, "Discard 必须收回本次创建的状态文件")

	row := findConsoleOperation(t, env.ops, core.ActionAgentCreate)
	require.Equal(t, core.OperationFailed, row.Status, "失败不得谎报 success")
	require.NotEqual(t, lifecycleResultOK, row.ResultCode)
}

// TestAgentAdmin_CreateIndexFailureBestEffort：索引可重建，写失败不回滚状态文件与目录；
// 操作仍是 success，detail_json 记 `index_write=deferred`，日志有 ERROR（约束 27 / 37）。
func TestAgentAdmin_CreateIndexFailureBestEffort(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t, func(deps *AgentAdminDeps) {
		deps.Index = failingLifecycleIndex{err: errors.New("索引不可用")}
	})

	view, err := env.admin.Create(context.Background(), consoleActor(), core.AgentSpec{Name: "alpha"})
	require.NoError(t, err)
	require.Equal(t, "alpha", view.Agent)
	require.FileExists(t, statePath(env.root, "alpha"))
	require.FileExists(t, filepath.Join(env.root, "agents", "alpha", "AGENT.md"))

	row := findConsoleOperation(t, env.ops, core.ActionAgentCreate)
	require.Equal(t, core.OperationSuccess, row.Status)
	var detail agentCreateOperationDetail
	require.NoError(t, json.Unmarshal(row.DetailJSON, &detail))
	require.Equal(t, agentIndexWriteDeferred, detail.IndexWrite)
	require.Contains(t, env.logs.String(), "索引同步失败")
}

// TestAgentAdmin_EveryWriteHasOperation：创建与提交各恰有一行台账，且都处于终态、
// result_code 为 OK；读面不写操作行（DoD #10）。
func TestAgentAdmin_EveryWriteHasOperation(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)

	view, err := env.admin.Create(context.Background(), consoleActor(), core.AgentSpec{Name: "alpha"})
	require.NoError(t, err)
	require.Empty(t, listConsoleOperations(t, env.ops)[0].ErrorMessage)

	_, err = env.admin.Files(context.Background(), "alpha")
	require.NoError(t, err)

	_, err = env.admin.SubmitFiles(context.Background(), consoleActor(), "alpha", view.ETag,
		[]core.AuthorFile{{Path: "notes.md", Content: "补充说明\n"}})
	require.NoError(t, err)

	rows := listConsoleOperations(t, env.ops)
	require.Len(t, rows, 2, "两次写操作必须恰有两行；读面不得写行")
	seen := map[string]core.OperationRecord{}
	for _, row := range rows {
		require.NotEqual(t, core.OperationPending, row.Status, "不得留下 pending 行")
		require.Equal(t, lifecycleResultOK, row.ResultCode, "action=%s", row.Action)
		seen[row.Action] = row
	}
	require.Contains(t, seen, core.ActionAgentCreate)
	require.Contains(t, seen, core.ActionAgentFilesUpdate)
	require.Equal(t, "alpha", seen[core.ActionAgentCreate].TargetID)
	require.Equal(t, "alpha", seen[core.ActionAgentFilesUpdate].TargetID)
	require.NotEmpty(t, seen[core.ActionAgentFilesUpdate].RequestHash)
	require.NotEmpty(t, seen[core.ActionAgentFilesUpdate].ExpectedETag)
}

// TestAgentTemplate_DeterministicProvider：模板 provider 恒取已发布列表配置顺序首项，
// 不因进程 / 遍历顺序改变；没有已发布 Provider 时写可见占位符（约束 28）。
func TestAgentTemplate_DeterministicProvider(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)

	for i, name := range []string{"alpha1", "alpha2", "alpha3", "alpha4", "alpha5"} {
		_, err := env.admin.Create(context.Background(), consoleActor(), core.AgentSpec{Name: name})
		require.NoError(t, err, "第 %d 次创建", i+1)
		def := readAdminFile(t, env.root, "agents/"+name+"/AGENT.md")
		require.Contains(t, def, "name: beta", "第 %d 次创建必须取列表首项 beta", i+1)
	}

	empty := newAgentAdminEnv(t, func(deps *AgentAdminDeps) {
		deps.PublishedProviders = func(context.Context) ([]workspace.ProviderEntry, error) { return nil, nil }
	})
	_, err := empty.admin.Create(context.Background(), consoleActor(), core.AgentSpec{Name: "alpha"})
	require.NoError(t, err)
	require.Contains(t, readAdminFile(t, empty.root, "agents/alpha/AGENT.md"),
		"请在此填写 provider 名", "无已发布 Provider 时必须写可见占位符")
}

// TestAgentAdmin_FilesReturnsCollectionETag：读面返回集合 ETag 与文件清单；生命周期
// 缺失时投影为无效（valid=false + 原因），不得读成 active（约束 45）。
func TestAgentAdmin_FilesReturnsCollectionETag(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	writeAdminAgent(t, env.root, "alpha")

	view, err := env.admin.Files(context.Background(), "alpha")
	require.NoError(t, err)
	require.Equal(t, "alpha", view.Agent)
	require.False(t, view.Lifecycle.Valid)
	require.Nil(t, view.Lifecycle.State)
	require.NotEmpty(t, view.Lifecycle.LifecycleError)
	require.Len(t, view.Files, 1)
	require.Equal(t, "AGENT.md", view.Files[0].Path)

	want := workspace.FileSetETag([]core.AuthorFile{{
		Path:    "AGENT.md",
		Content: readAdminFile(t, env.root, "agents/alpha/AGENT.md"),
	}})
	require.Equal(t, want, view.ETag)
	require.Empty(t, listConsoleOperations(t, env.ops), "读面不得写操作行")
}

// TestAgentAdmin_FilesMissingAgent：未知 Agent → *core.AgentNotFoundError（404 分类）。
func TestAgentAdmin_FilesMissingAgent(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)

	_, err := env.admin.Files(context.Background(), "ghost")
	require.Error(t, err)
	var notFound *core.AgentNotFoundError
	require.ErrorAs(t, err, &notFound)
	require.Equal(t, "ghost", notFound.Name)
}

// TestAgentAdmin_SubmitCollectionETag：条件提交成功后返回落盘真值——新集合 ETag、
// 完整文件清单、未列出的现有文件原样保留；操作 detail 记基础 / 候选集合 ETag（约束 30）。
func TestAgentAdmin_SubmitCollectionETag(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	writeAdminAgent(t, env.root, "alpha")
	writeWorkspaceFile(t, env.root, "agents/alpha/keep.txt", "保持原样\n")

	base, err := env.admin.Files(context.Background(), "alpha")
	require.NoError(t, err)

	updated := authorAgentMD("alpha", "alpha", "alpha-model-2", nil, nil)
	view, err := env.admin.SubmitFiles(context.Background(), consoleActor(), "alpha", base.ETag,
		[]core.AuthorFile{
			{Path: "AGENT.md", Content: updated},
			{Path: "notes.md", Content: "新增说明\n"},
		})
	require.NoError(t, err)
	require.NotEqual(t, base.ETag, view.ETag, "提交后集合 ETag 必须变化")
	require.Equal(t, updated, readAdminFile(t, env.root, "agents/alpha/AGENT.md"))
	require.Equal(t, "新增说明\n", readAdminFile(t, env.root, "agents/alpha/notes.md"))
	require.Equal(t, "保持原样\n", readAdminFile(t, env.root, "agents/alpha/keep.txt"),
		"未列出的现有文件必须保留")

	paths := map[string]int64{}
	for _, file := range view.Files {
		paths[file.Path] = file.Size
	}
	require.Len(t, paths, 3)
	require.Contains(t, paths, "AGENT.md")
	require.Contains(t, paths, "notes.md")
	require.Contains(t, paths, "keep.txt")

	row := findConsoleOperation(t, env.ops, core.ActionAgentFilesUpdate)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, base.ETag, row.ExpectedETag)
	var detail agentFilesOperationDetail
	require.NoError(t, json.Unmarshal(row.DetailJSON, &detail))
	require.Equal(t, base.ETag, detail.BaseETag)
	require.Equal(t, view.ETag, detail.CandidateETag)
}

// TestAgentAdmin_SubmitStaleETagNoOperation：缺失 / 弱 / `*` / 形状非法与不匹配一律 412，
// 磁盘零写入、操作行零行（约束 30 / 约束 3）。
func TestAgentAdmin_SubmitStaleETagNoOperation(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	writeAdminAgent(t, env.root, "alpha")
	base, err := env.admin.Files(context.Background(), "alpha")
	require.NoError(t, err)
	before := readAdminFile(t, env.root, "agents/alpha/AGENT.md")

	cases := map[string]string{
		"缺失":    "",
		"弱ETag": `W/` + base.ETag,
		"通配":    "*",
		"不匹配":   `"sha256:` + strings.Repeat("0", 64) + `"`,
	}
	for label, ifMatch := range cases {
		_, err := env.admin.SubmitFiles(context.Background(), consoleActor(), "alpha", ifMatch,
			[]core.AuthorFile{{Path: "AGENT.md", Content: "覆盖内容\n"}})
		require.Error(t, err, "%s：必须拒绝", label)
		require.ErrorIs(t, err, core.ErrPreconditionFailed, "%s：必须 412 分类", label)
	}
	require.Empty(t, listConsoleOperations(t, env.ops), "412 不得写操作行")
	require.Equal(t, before, readAdminFile(t, env.root, "agents/alpha/AGENT.md"), "412 不得写盘")
}

// TestAgentAdmin_SubmitMissingDefinitionRejected：候选集合必须仍有 AGENT.md；基础目录
// 只有别的文件时提交被 400 拒绝，零操作行（约束 30）。
func TestAgentAdmin_SubmitMissingDefinitionRejected(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	writeWorkspaceFile(t, env.root, "agents/alpha/notes.md", "只有说明\n")

	base, err := env.admin.Files(context.Background(), "alpha")
	require.NoError(t, err)

	_, err = env.admin.SubmitFiles(context.Background(), consoleActor(), "alpha", base.ETag,
		[]core.AuthorFile{{Path: "notes.md", Content: "改了说明\n"}})
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err), "缺少定义文件是入参问题：%v", err)
	require.Empty(t, listConsoleOperations(t, env.ops))
	require.Equal(t, "只有说明\n", readAdminFile(t, env.root, "agents/alpha/notes.md"))
}

// TestAgentAdmin_SubmitInvalidPathNoOperation：越界路径在预写之前被拒——400、零操作行、
// 工作区外零文件（约束 41）。
func TestAgentAdmin_SubmitInvalidPathNoOperation(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	writeAdminAgent(t, env.root, "alpha")
	base, err := env.admin.Files(context.Background(), "alpha")
	require.NoError(t, err)

	_, err = env.admin.SubmitFiles(context.Background(), consoleActor(), "alpha", base.ETag,
		[]core.AuthorFile{{Path: "../escape.md", Content: "越界\n"}})
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err), "越界路径是入参问题：%v", err)
	require.Empty(t, listConsoleOperations(t, env.ops))
	_, statErr := os.Stat(filepath.Join(env.root, "agents", "escape.md"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

// TestAgentAdmin_SubmitValidationFailureNoOperation：加载期校验失败 = 候选非法，
// 预写之前拒绝（400），磁盘与台账零变化（约束 30）。
func TestAgentAdmin_SubmitValidationFailureNoOperation(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t, func(deps *AgentAdminDeps) {
		deps.ValidateDefinition = func(context.Context, string, []byte) error {
			return errors.New("schedules 的 id 与其它 Agent 冲突")
		}
	})
	writeAdminAgent(t, env.root, "alpha")
	base, err := env.admin.Files(context.Background(), "alpha")
	require.NoError(t, err)
	before := readAdminFile(t, env.root, "agents/alpha/AGENT.md")

	_, err = env.admin.SubmitFiles(context.Background(), consoleActor(), "alpha", base.ETag,
		[]core.AuthorFile{{Path: "AGENT.md", Content: "新定义\n"}})
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err), "加载期校验失败是入参问题：%v", err)
	require.Empty(t, listConsoleOperations(t, env.ops))
	require.Equal(t, before, readAdminFile(t, env.root, "agents/alpha/AGENT.md"))
}

// TestAgentAdmin_SubmitWriteFailureFinishesFailed：预写成功后落盘失败 → 操作回填
// failed（不谎报 success），正式目录与文件原样（workspace 层自保）。
func TestAgentAdmin_SubmitWriteFailureFinishesFailed(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	writeAdminAgent(t, env.root, "alpha")
	base, err := env.admin.Files(context.Background(), "alpha")
	require.NoError(t, err)
	before := readAdminFile(t, env.root, "agents/alpha/AGENT.md")

	// staging 残留让 workspace 层在预写之后拒绝覆盖；残留是测试预置的普通文件。
	residue := filepath.Join(env.root, "agents", "alpha.staging")
	require.NoError(t, os.WriteFile(residue, []byte("residue"), 0o600))

	_, err = env.admin.SubmitFiles(context.Background(), consoleActor(), "alpha", base.ETag,
		[]core.AuthorFile{{Path: "AGENT.md", Content: "新定义\n"}})
	require.Error(t, err)
	require.False(t, core.IsInvalidInput(err), "落盘失败不是入参问题")

	row := findConsoleOperation(t, env.ops, core.ActionAgentFilesUpdate)
	require.Equal(t, core.OperationFailed, row.Status)
	require.Equal(t, agentResultInternalForTest(), row.ResultCode)
	require.Equal(t, before, readAdminFile(t, env.root, "agents/alpha/AGENT.md"))
	info, statErr := os.Stat(residue)
	require.NoError(t, statErr)
	require.True(t, info.Mode().IsRegular(), "预置残留不得被本次提交改写")
}

// agentResultInternalForTest 是内部失败终态的 result_code（错误码表 `INTERNAL`）。
func agentResultInternalForTest() string { return lifecycleResultInternal }

// TestAgentAdmin_SubmitPreWriteFailureZeroWrites：预写失败 = 拒绝执行，磁盘与台账
// 零变化（约束 1）。
func TestAgentAdmin_SubmitPreWriteFailureZeroWrites(t *testing.T) {
	t.Parallel()
	realOps := newAgentAdminEnv(t)
	env := newAgentAdminEnv(t, func(deps *AgentAdminDeps) {
		deps.Operations = failingBeginOperations{OperationStore: realOps.ops, err: errors.New("数据库不可用")}
	})
	writeAdminAgent(t, env.root, "alpha")
	base, err := env.admin.Files(context.Background(), "alpha")
	require.NoError(t, err)
	before := readAdminFile(t, env.root, "agents/alpha/AGENT.md")

	_, err = env.admin.SubmitFiles(context.Background(), consoleActor(), "alpha", base.ETag,
		[]core.AuthorFile{{Path: "AGENT.md", Content: "新定义\n"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "拒绝执行")
	require.Equal(t, before, readAdminFile(t, env.root, "agents/alpha/AGENT.md"))
	require.Empty(t, listConsoleOperations(t, realOps.ops))
}

// TestAgentAdmin_GenerateFilesNoOperationRow：草稿只在内存里，零操作行、零落盘；
// 唯一模型调用经注入的审计端口（约束 29）。
func TestAgentAdmin_GenerateFilesNoOperationRow(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	env.chatter.response = authorDraftJSON(t, core.AuthorFile{
		Path:    "AGENT.md",
		Content: authorAgentMD("alpha", "alpha", "alpha-model", nil, nil),
	})

	draft, err := env.admin.GenerateFiles(context.Background(),
		core.AuthorConfig{Provider: "alpha", Model: "alpha-model"}, "alpha", "值班助手", nil)
	require.NoError(t, err)
	require.Equal(t, "alpha", draft.Agent)
	require.Len(t, draft.Files, 1)
	require.Equal(t, "AGENT.md", draft.Files[0].Path)
	require.Equal(t, 1, len(env.chatter.requests))

	require.Empty(t, listConsoleOperations(t, env.ops), "草稿不得写管理操作行")
	requireAgentDirAbsent(t, env.root, "alpha")
}

// TestAgentAdmin_GenerateFilesNotConfigured：作者未配置 → ErrProviderNotConfigured
// （503 分类），不读已发布快照、不发模型调用（约束 29）。
func TestAgentAdmin_GenerateFilesNotConfigured(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)

	_, err := env.admin.GenerateFiles(context.Background(), core.AuthorConfig{}, "alpha", "值班助手", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrProviderNotConfigured)
	require.Empty(t, env.chatter.requests, "未配置时不得发出模型调用")
	require.Empty(t, listConsoleOperations(t, env.ops))
}

// TestAgentAdmin_GenerateFilesCallErrorPassthrough：上游调用失败原样上抛，不被改判为
// 入参错误（失败分类表：外部错误 ≠ 400）。
func TestAgentAdmin_GenerateFilesCallErrorPassthrough(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	callErr := &provider.CallError{
		Provider: "alpha",
		Status:   502,
		Kind:     core.ErrorKindExternalError,
		Message:  "上游不可用",
	}
	env.chatter.err = callErr

	_, err := env.admin.GenerateFiles(context.Background(),
		core.AuthorConfig{Provider: "alpha", Model: "alpha-model"}, "alpha", "值班助手", nil)
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err) == false, "外部错误不得改判为 400")
	var got *provider.CallError
	require.ErrorAs(t, err, &got)
	require.Empty(t, listConsoleOperations(t, env.ops))
}

// TestAgentAdmin_GenerateFilesQuestions：作者返回澄清问题时原样透出 questions，
// files 为空；仍零操作行、零落盘（R6.3）。
// TestAgentAdmin_GenerateFilesQuestions passes the questions through with no writes.
func TestAgentAdmin_GenerateFilesQuestions(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	env.chatter.response = `{"questions":[{"id":"frequency","question":"每天几点运行？"}]}`

	draft, err := env.admin.GenerateFiles(context.Background(),
		core.AuthorConfig{Provider: "alpha", Model: "alpha-model"}, "alpha", "值班助手", nil)
	require.NoError(t, err)
	require.Equal(t, "alpha", draft.Agent)
	require.Empty(t, draft.Files)
	require.Equal(t, []core.AuthorQuestion{{ID: "frequency", Question: "每天几点运行？"}}, draft.Questions)
	require.Len(t, env.chatter.requests, 1)

	require.Empty(t, listConsoleOperations(t, env.ops), "问答轮次不得写管理操作行")
	requireAgentDirAbsent(t, env.root, "alpha")
}

// TestAgentAdmin_GenerateFilesAnswers：答案逐条写进提示词（已澄清段），模型据此
// 直接产出文件；答案仍受 3 条与长度上限约束（上游 400 分类）。
// TestAgentAdmin_GenerateFilesAnswers feeds the answers into the prompt and keeps limits.
func TestAgentAdmin_GenerateFilesAnswers(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	env.chatter.response = authorDraftJSON(t, core.AuthorFile{
		Path:    "AGENT.md",
		Content: authorAgentMD("alpha", "alpha", "alpha-model", nil, nil),
	})
	answers := []core.AuthorAnswer{{ID: "frequency", Question: "每天几点运行？", Answer: "08:30"}}

	draft, err := env.admin.GenerateFiles(context.Background(),
		core.AuthorConfig{Provider: "alpha", Model: "alpha-model"}, "alpha", "值班助手", answers)
	require.NoError(t, err)
	require.Len(t, draft.Files, 1)
	require.Empty(t, draft.Questions)
	require.Len(t, env.chatter.requests, 1)
	prompt := env.chatter.requests[0].Messages[0].Content
	require.Contains(t, prompt, "已澄清的问题")
	require.Contains(t, prompt, "[frequency] 每天几点运行？：08:30")

	env.chatter.requests = nil
	_, err = env.admin.GenerateFiles(context.Background(),
		core.AuthorConfig{Provider: "alpha", Model: "alpha-model"}, "alpha", "值班助手",
		[]core.AuthorAnswer{{ID: "q", Question: "问？", Answer: " "}})
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err), "空答案按 400 分类")
	require.Empty(t, env.chatter.requests, "非法答案不得发出模型调用")
	require.Empty(t, listConsoleOperations(t, env.ops))
}

// TestAgentAdmin_ScheduleDraft：定时候选透出规整后的候选与固定时钟算出的三次触发；
// 伪 Run 前缀独立；零操作行、零落盘（R6.4）。
// TestAgentAdmin_ScheduleDraft exposes the candidate and server-computed triggers with
// no operations and no disk writes.
func TestAgentAdmin_ScheduleDraft(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	env.chatter.response = `{"id":"daily-report","cron":"0 30 8 * * *","zone":"Asia/Shanghai","message":"生成每日运维日报"}`

	result, err := env.admin.ScheduleDraft(context.Background(),
		core.AuthorConfig{Provider: "alpha", Model: "alpha-model"}, "alpha", "每天 8:30 生成日报")
	require.NoError(t, err)
	require.Equal(t, core.Schedule{
		ID: "daily-report", Cron: "0 30 8 * * *", Zone: "Asia/Shanghai", Message: "生成每日运维日报",
	}, result.Candidate)
	require.Equal(t, []time.Time{
		time.Date(2026, 10, 22, 0, 30, 0, 0, time.UTC),
		time.Date(2026, 10, 23, 0, 30, 0, 0, time.UTC),
		time.Date(2026, 10, 24, 0, 30, 0, 0, time.UTC),
	}, result.NextRuns, "触发时间必须由注入时钟在服务端计算")

	require.Len(t, env.chatter.requests, 1)
	require.Equal(t, "agent-schedule-alpha", env.chatter.requests[0].RunID)
	require.Empty(t, listConsoleOperations(t, env.ops), "候选不得写管理操作行")
	requireAgentDirAbsent(t, env.root, "alpha")
}

// TestAgentAdmin_ScheduleDraftRejected：非法 Agent 名与非法指令/候选都按 400 分类，
// 零操作行；作者未配置保留 503 分类。
// TestAgentAdmin_ScheduleDraftRejected maps bad input to 400 and keeps 503 for config.
func TestAgentAdmin_ScheduleDraftRejected(t *testing.T) {
	t.Parallel()
	env := newAgentAdminEnv(t)
	config := core.AuthorConfig{Provider: "alpha", Model: "alpha-model"}

	_, err := env.admin.ScheduleDraft(context.Background(), config, "../escape", "每天 8:30")
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err), "非法 Agent 名按 400 分类")

	_, err = env.admin.ScheduleDraft(context.Background(), config, "alpha", "  ")
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err))

	env.chatter.response = `{"id":"daily","cron":"30 8 * * *","zone":"UTC","message":"m"}`
	_, err = env.admin.ScheduleDraft(context.Background(), config, "alpha", "每天 8:30")
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err), "非法候选按 400 分类")

	_, err = env.admin.ScheduleDraft(context.Background(), core.AuthorConfig{}, "alpha", "每天 8:30")
	require.ErrorIs(t, err, core.ErrProviderNotConfigured)

	require.Empty(t, listConsoleOperations(t, env.ops))
}

// —— 复制 / 归档 / 恢复（约束 31—33）——
// Copy, archive and restore fixtures (constraints 31–33).

// fakeClock 是可推进的测试时钟：Now 读当前读数，Advance 只前推——归档标识的
// UTC 时间戳因此可复算（约束 33）。
// fakeClock is an advanceable test clock so archive ids stay reproducible.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now 返回当前读数。
// Now returns the current reading.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 把时钟前推 d。
// Advance moves the clock forward by d.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// archiveScheduler 是 ScheduleRegistry 的记录型替身：注册可见性与定时任务都留在内存里，
// 归档注销与回滚恢复的判据是它最后的取值（约束 32 ①②）。
// archiveScheduler records registration and schedules in memory.
type archiveScheduler struct {
	mu         sync.Mutex
	registered map[string]bool
	schedules  map[string][]core.Schedule
}

// newArchiveScheduler 建一个已注册、带一条定时任务的调度替身。
// newArchiveScheduler seeds one registered agent with its schedules.
func newArchiveScheduler(agent string, schedules []core.Schedule) *archiveScheduler {
	return &archiveScheduler{
		registered: map[string]bool{agent: true},
		schedules:  map[string][]core.Schedule{agent: append([]core.Schedule(nil), schedules...)},
	}
}

// SetRegistered 记录注册可见性。
// SetRegistered records the registration flag.
func (s *archiveScheduler) SetRegistered(agent string, registered bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered[agent] = registered
	return nil
}

// ReconcileAgent 记录该 Agent 当前的定时任务（nil 即退休 / 清空）。
// ReconcileAgent records the agent's current schedules.
func (s *archiveScheduler) ReconcileAgent(_ context.Context, agent string, schedules []core.Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedules[agent] = append([]core.Schedule(nil), schedules...)
	return nil
}

// isRegistered 返回注册可见性（读加锁）。
// isRegistered reads the registration flag under the lock.
func (s *archiveScheduler) isRegistered(agent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registered[agent]
}

// snapshotRegistered 返回注册表的副本（整体比较用）。
// snapshotRegistered copies the registration map.
func (s *archiveScheduler) snapshotRegistered() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.registered))
	for name, registered := range s.registered {
		out[name] = registered
	}
	return out
}

// snapshotSchedules 返回定时任务表的副本（整体比较用）。
// snapshotSchedules copies the schedule map.
func (s *archiveScheduler) snapshotSchedules() map[string][]core.Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]core.Schedule, len(s.schedules))
	for name, schedules := range s.schedules {
		out[name] = append([]core.Schedule(nil), schedules...)
	}
	return out
}

// archiveCanceller 是 RunCanceller 的替身：running 里的 Run 永不退出，WaitRuns 在窗口
// 结束时把它们原样交回——归档超时分支的判据（约束 32 ③）。
// archiveCanceller keeps the listed runs alive forever; WaitRuns hands them back.
type archiveCanceller struct {
	mu        sync.Mutex
	running   map[string][]string
	cancelled []string
}

// RunningRunIDs 返回该 Agent 仍在运行的 Run 快照。
// RunningRunIDs snapshots the runs still alive for the agent.
func (c *archiveCanceller) RunningRunIDs(_ context.Context, agent string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.running[agent]...), nil
}

// CancelRun 记录取消信号（返回不代表 Run 已退出）。
// CancelRun records the signal; a nil error does not mean the run exited.
func (c *archiveCanceller) CancelRun(_ context.Context, runID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled = append(c.cancelled, runID)
	return nil
}

// WaitRuns 在窗口结束时交回仍未退出的标识（替身里的 Run 永不退出）。
// WaitRuns returns the ids still alive when the window closes.
func (c *archiveCanceller) WaitRuns(_ context.Context, runIDs []string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	alive := make([]string, 0, len(runIDs))
	for _, id := range runIDs {
		for _, runs := range c.running {
			if slices.Contains(runs, id) {
				alive = append(alive, id)
				break
			}
		}
	}
	return alive, nil
}

// archiveExitCanceller 在 archiveCanceller 之上给出可控的退出信号：RunningRunIDs 总是
// 返回一个在途 Run，WaitRuns 阻塞到测试关闭 exit 或等待窗口到期——归档的"取消后真正
// 退出"分支因此可在 -race 下端到端判定（约束 32 ③ 的成功侧）。
// archiveExitCanceller blocks WaitRuns until the test closes exit (or the window
// expires), so the archive path can be judged end to end under -race.
type archiveExitCanceller struct {
	mu        sync.Mutex
	cancelled []string
	runID     string
	signalled chan struct{}
	once      sync.Once
	exit      chan struct{}
}

// newArchiveExitCanceller 建一个持有一条在途 Run 的取消器替身。
// newArchiveExitCanceller builds a canceller fake holding one live run.
func newArchiveExitCanceller(runID string) *archiveExitCanceller {
	return &archiveExitCanceller{
		runID:     runID,
		signalled: make(chan struct{}),
		exit:      make(chan struct{}),
	}
}

// RunningRunIDs 总是返回同一条在途 Run。
// RunningRunIDs always reports the same live run.
func (c *archiveExitCanceller) RunningRunIDs(context.Context, string) ([]string, error) {
	return []string{c.runID}, nil
}

// CancelRun 记录取消并关闭 signalled（只关一次）。
// CancelRun records the signal and closes signalled exactly once.
func (c *archiveExitCanceller) CancelRun(_ context.Context, runID string) error {
	c.mu.Lock()
	c.cancelled = append(c.cancelled, runID)
	c.mu.Unlock()
	c.once.Do(func() { close(c.signalled) })
	return nil
}

// WaitRuns 阻塞到测试关闭 exit；窗口到期则把剩余集合原样交回（超时分支）。
// WaitRuns blocks until exit closes; an expired window returns the remaining ids.
func (c *archiveExitCanceller) WaitRuns(ctx context.Context, runIDs []string) ([]string, error) {
	select {
	case <-c.exit:
		return nil, nil
	case <-ctx.Done():
		return append([]string(nil), runIDs...), nil
	}
}

// cancelledRunIDs 返回已发出取消的 Run 标识副本。
// cancelledRunIDs copies the run ids that received a cancel signal.
func (c *archiveExitCanceller) cancelledRunIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.cancelled)
}

// archiveEnv 在 agentAdminEnv 之上补齐活动面依赖（注册表 / 调度 / Run 取消器三项齐全，
// 即 HTTP 路径的归档语义）。clock 可推进，供归档标识与超时用例使用。
// archiveEnv adds the activity-surface dependencies archive, copy and restore need.
type archiveEnv struct {
	*agentAdminEnv
	clock     *fakeClock
	registry  *agentRegistry
	scheduler *archiveScheduler
	canceller *archiveCanceller
}

// newAgentEnv 建一个处于 active（generation=2）的 Agent：定义里带 identity 与一条
// schedules，真实注册表与记录型调度持有同一条目——复制 / 恢复的「改写身份 + 清空
// 定时任务」因此有可断言的基线。
// newAgentEnv builds one active agent with identity and one schedule, registered in
// both the real registry and the recording scheduler.
func newAgentEnv(t *testing.T, name string) *archiveEnv {
	t.Helper()

	clock := &fakeClock{now: baselineClock()}
	base := newAgentAdminEnvRaw(t, clock.Now)

	schedules := []core.Schedule{{
		ID:      name + "-daily",
		Cron:    "0 0 9 * * *",
		Zone:    "UTC",
		Message: "每日巡检",
	}}
	registry := newAgentRegistry(base.root, []core.AgentEntry{{
		Name:       name,
		Registered: true,
		Profile:    &core.Profile{Name: name, Schedules: schedules},
	}}, base.deps.Logger)
	scheduler := newArchiveScheduler(name, schedules)
	canceller := &archiveCanceller{running: map[string][]string{}}

	base.deps.Registry = registry
	base.deps.Scheduler = scheduler
	base.deps.RunCanceller = canceller
	base.deps.Archives = storage.NewAgentAdminStore(base.db)
	base.deps.Operations = defaultingActorStore{OperationStore: base.ops}
	base.deps.BusyWait = time.Millisecond
	base.build()

	writeWorkspaceFile(t, base.root, "agents/"+name+"/AGENT.md", archiveAgentDefinition(name))
	rec, err := base.lifecycle.Bootstrap(context.Background(), name, "op-seed")
	require.NoError(t, err)
	rec, err = base.lifecycle.Transition(context.Background(), name,
		core.AgentDisabled, rec.Generation, core.AgentActive, "op-seed")
	require.NoError(t, err)
	require.Equal(t, core.AgentActive, rec.State, "夹具 Agent 必须处于 active")

	return &archiveEnv{
		agentAdminEnv: base,
		clock:         clock,
		registry:      registry,
		scheduler:     scheduler,
		canceller:     canceller,
	}
}

// archiveAgentDefinition 是归档 / 复制 / 恢复用例的定义正文：identity 与块式
// schedules 都在，用来断言目标上的身份改写与定时任务清空；正文逐字保留。
// archiveAgentDefinition carries identity and a block-style schedules list.
func archiveAgentDefinition(name string) string {
	return "---\nname: " + name + "\n" +
		"description: 归档与复制用例\n" +
		"identity:\n  agent_name: " + name + "-persona\n  prompt: 保持人格设定。\n" +
		"provider:\n  name: alpha\n  model: alpha-model\n" +
		"schedules:\n" +
		"  - id: " + name + "-daily\n    cron: \"0 0 9 * * *\"\n    zone: UTC\n    message: \"每日巡检\"\n" +
		"---\n\n正文必须逐字保留。\n"
}

// startNeverEndingRun 登记一个永不退出的 Run（归档等待超时的判据）。
// startNeverEndingRun registers a run that never exits.
func (e *archiveEnv) startNeverEndingRun(t *testing.T, agent, runID string) {
	t.Helper()
	e.canceller.mu.Lock()
	defer e.canceller.mu.Unlock()
	e.canceller.running[agent] = append(e.canceller.running[agent], runID)
}

// everythingSnapshot 是归档回滚的整体快照：状态文件字节、注册表、调度与 agents /
// archive 目录树——回滚后四项必须逐项相等（约束 32）。
// everythingSnapshot captures state bytes, registry, scheduling and both trees.
type everythingSnapshot struct {
	stateFiles map[string]string
	registry   []core.AgentEntry
	registered map[string]bool
	schedules  map[string][]core.Schedule
	agentTree  map[string]string
	archiveDir map[string]string
}

// snapshotEverything 取一份整体快照。
// snapshotEverything captures one whole-world snapshot.
func (e *archiveEnv) snapshotEverything(t *testing.T) everythingSnapshot {
	t.Helper()
	return everythingSnapshot{
		stateFiles: readAdminTree(t, filepath.Join(e.root, "control", "agents")),
		registry:   e.registry.Entries(),
		registered: e.scheduler.snapshotRegistered(),
		schedules:  e.scheduler.snapshotSchedules(),
		agentTree:  readAdminTree(t, filepath.Join(e.root, "agents")),
		archiveDir: readAdminTree(t, filepath.Join(e.root, "archive")),
	}
}

// equalEverything 逐项比较快照并返回是否相等；不等时打印首个差异项便于定位。
// equalEverything compares every snapshot member and logs the differing one.
func (e *archiveEnv) equalEverything(t *testing.T, before everythingSnapshot) bool {
	t.Helper()
	after := e.snapshotEverything(t)
	switch {
	case !reflect.DeepEqual(before.stateFiles, after.stateFiles):
		t.Logf("状态文件快照不一致：before=%v after=%v", before.stateFiles, after.stateFiles)
		return false
	case !reflect.DeepEqual(before.registry, after.registry):
		t.Logf("注册表快照不一致：before=%v after=%v", before.registry, after.registry)
		return false
	case !reflect.DeepEqual(before.registered, after.registered):
		t.Logf("调度注册快照不一致：before=%v after=%v", before.registered, after.registered)
		return false
	case !reflect.DeepEqual(before.schedules, after.schedules):
		t.Logf("定时任务快照不一致：before=%v after=%v", before.schedules, after.schedules)
		return false
	case !reflect.DeepEqual(before.agentTree, after.agentTree):
		t.Logf("agents 目录树快照不一致：before=%v after=%v", before.agentTree, after.agentTree)
		return false
	case !reflect.DeepEqual(before.archiveDir, after.archiveDir):
		t.Logf("archive 目录树快照不一致：before=%v after=%v", before.archiveDir, after.archiveDir)
		return false
	}
	return true
}

// readAdminTree 把一棵目录树读成「相对路径 → 文件字节」；目录记作 `rel/` 空值，
// 顶层目录不存在时返回空表（归档区可能尚未创建）。
// readAdminTree maps a tree onto relative path → file bytes, directories as `rel/`.
func readAdminTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			tree[rel+"/"] = ""
			return nil
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		tree[rel] = string(raw)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}
	}
	require.NoError(t, err)
	return tree
}

// defaultingActorStore 在操作行写入前补全空 `auth_method`：模块卡 §4.3.3 的回归用例
// 直接传 `core.Actor{Subject: "test"}`（不带 Method），而 schema.sql 的 CHECK 只接受
// 四个固定字面量。夹具在写入边界补成 local_trust——真实调用方（web / CLI）一律带
// 完整 Actor，生产行为不变。
// defaultingActorStore fills an empty auth_method at the write boundary so the card's
// verbatim regression (bare core.Actor) reaches the busy path; production callers
// always carry a complete actor.
type defaultingActorStore struct {
	core.OperationStore
}

// BeginOperation 补全空 Method 后委托真存储。
// BeginOperation defaults an empty method and delegates to the real store.
func (s defaultingActorStore) BeginOperation(ctx context.Context, rec core.OperationRecord) error {
	if rec.AuthMethod == "" {
		rec.AuthMethod = core.AuthMethodLocalTrust
	}
	return s.OperationStore.BeginOperation(ctx, rec)
}

// failingInsertArchiveStore 只让归档行写入失败，其余动作委托真存储（约束 32 ⑧ 档）。
// failingInsertArchiveStore fails only the archive-row insert.
type failingInsertArchiveStore struct {
	AgentArchiveStore
	err error
}

// InsertArchive 总是返回注入的错误。
// InsertArchive always returns the injected error.
func (s failingInsertArchiveStore) InsertArchive(context.Context, core.AgentArchiveRecord, string) error {
	return s.err
}

// archiveStamp 返回夹具时钟下的归档时间戳（UTC，`20060102-150405`，约束 33）。
// archiveStamp renders the fixture clock in the archive-id layout.
func archiveStamp(clock *fakeClock) string {
	return clock.Now().UTC().Format("20060102-150405")
}

// TestAgentCopy_DisabledBootstrap：复制源必须在活动区；目标一律以 disabled /
// generation=1 / 本次操作引导，定义改写 name 与 identity、清空 schedules，源逐字节
// 不变；复制失败时 Discard 收回状态文件、目标目录零残余（约束 31）。
func TestAgentCopy_DisabledBootstrap(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t, "reporter")
	sourceBefore := readAdminFile(t, env.root, "agents/reporter/AGENT.md")

	view, err := env.admin.Copy(context.Background(), consoleActor(), "reporter", "reporter-copy", nil)
	require.NoError(t, err)
	require.Equal(t, "reporter-copy", view.Agent)
	require.NotNil(t, view.Lifecycle.State)
	require.Equal(t, string(core.AgentDisabled), *view.Lifecycle.State, "复制目标默认停用")
	require.True(t, view.Lifecycle.Valid)

	rec, exists, readErr := env.lifecycle.Read(context.Background(), "reporter-copy")
	require.NoError(t, readErr)
	require.True(t, exists)
	require.Equal(t, core.AgentDisabled, rec.State)
	require.EqualValues(t, 1, rec.Generation, "复制目标 generation=1")

	row := findConsoleOperation(t, env.ops, core.ActionAgentCopy)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, "reporter-copy", row.TargetID)
	require.Equal(t, row.OperationID, rec.OperationID, "状态文件必须记录本次复制操作")

	target := readAdminFile(t, env.root, "agents/reporter-copy/AGENT.md")
	require.Contains(t, target, "name: reporter-copy")
	require.Contains(t, target, "agent_name: reporter-copy")
	require.Contains(t, target, "schedules: []", "复制不得继承定时任务")
	require.NotContains(t, target, "reporter-daily")
	require.Contains(t, target, "正文必须逐字保留。")
	require.Equal(t, sourceBefore, readAdminFile(t, env.root, "agents/reporter/AGENT.md"),
		"复制不得改动源 Agent")

	for _, rel := range []string{"agents/reporter-copy/sessions", "agents/reporter-copy/memory", "agents/reporter-copy/audit"} {
		_, statErr := os.Lstat(filepath.Join(env.root, filepath.FromSlash(rel)))
		require.ErrorIs(t, statErr, os.ErrNotExist, "复制不得携带运行数据：%s", rel)
	}
	_, registered := env.registry.Lookup("reporter-copy")
	require.False(t, registered, "复制的目标不自动进入注册表")

	t.Run("failure_zero_residue", func(t *testing.T) {
		t.Parallel()
		failEnv := newAgentEnv(t, "reporter")
		// staging 残留让 workspace 层在预写之后拒绝覆盖：复制失败必须 Discard 状态文件。
		residue := filepath.Join(failEnv.root, "agents", "reporter-copy.staging")
		require.NoError(t, os.WriteFile(residue, []byte("residue"), 0o600))
		failBefore := readAdminFile(t, failEnv.root, "agents/reporter/AGENT.md")

		_, err := failEnv.admin.Copy(context.Background(), consoleActor(), "reporter", "reporter-copy", nil)
		require.Error(t, err)
		_, statErr := os.Stat(filepath.Join(failEnv.root, "agents", "reporter-copy"))
		require.ErrorIs(t, statErr, os.ErrNotExist, "失败不得留下目标目录")
		_, statErr = os.Stat(statePath(failEnv.root, "reporter-copy"))
		require.ErrorIs(t, statErr, os.ErrNotExist, "Discard 必须收回本次复制的状态文件")
		require.Equal(t, failBefore, readAdminFile(t, failEnv.root, "agents/reporter/AGENT.md"))

		row := findConsoleOperation(t, failEnv.ops, core.ActionAgentCopy)
		require.Equal(t, core.OperationFailed, row.Status, "失败不得谎报 success")
	})
}

// copyMappingEnv 是「源 Agent 绑定了源专属模型连接」的复制夹具：
//   - `alpha` 已收缩为 reporter 专属（专属闸门的触发点）；
//   - `beta` 是工作区共享资源（唯一合法的映射目标）；
//   - `gamma` 也已收缩为 reporter 专属（「映射目标必须是共享」的拒绝判据）。
//
// copyMappingEnv carries one source-exclusive binding plus a shared and a second
// exclusive resource so both the accept and the reject sides are covered.
type copyMappingEnv struct {
	*archiveEnv
	sourceUID       core.AgentUID
	exclusive       core.ResourceVersion
	shared          core.ResourceVersion
	exclusiveTarget core.ResourceVersion
}

// newCopyMappingEnv 建夹具并把源定义改写成带绑定块的版本：`provider.name` 与绑定名
// 一一对应——只改引用不改绑定（或反过来）都会被 workspace 的落盘前复核拒绝。
// newCopyMappingEnv rewrites the source definition so the provider reference and the
// binding block line up, which is what the rewrite pre-check demands.
func newCopyMappingEnv(t *testing.T) *copyMappingEnv {
	t.Helper()
	env := newAgentEnv(t, "reporter")
	rec, exists, err := env.lifecycle.Read(context.Background(), "reporter")
	require.NoError(t, err)
	require.True(t, exists)
	require.NotEmpty(t, rec.AgentUID, "夹具 Agent 必须有稳定身份")

	exclusive := env.seedModel(t, "alpha")
	shared := env.seedModel(t, "beta")
	exclusiveTarget := env.seedModel(t, "gamma")
	env.makeExclusive(t, "alpha", exclusive.ResourceUID, rec.AgentUID)
	env.makeExclusive(t, "gamma", exclusiveTarget.ResourceUID, rec.AgentUID)

	// `provider` 必须是块式映射：改写走 ApplyChanges 的行级外科手术，流式写法会被
	// 判为 `edit unsupported`（fail-closed，候选取代树随 staging 丢弃）。
	writeWorkspaceFile(t, env.root, "agents/reporter/AGENT.md",
		"---\nname: reporter\ndescription: 复制映射用例\n"+
			"provider:\n  name: alpha\n  model: alpha-model\n"+
			bindingsBlock([]core.ResourceBinding{{Name: "alpha", UID: exclusive.ResourceUID}}, nil)+
			"---\n\n正文必须逐字保留。\n")

	return &copyMappingEnv{
		archiveEnv:      env,
		sourceUID:       rec.AgentUID,
		exclusive:       exclusive,
		shared:          shared,
		exclusiveTarget: exclusiveTarget,
	}
}

// mustBindings 读一个身份的投影行；读取失败直接判失败。
// mustBindings reads one identity's projection rows.
func mustBindings(t *testing.T, store *storage.BindingStore, uid core.AgentUID) []core.AgentResourceBinding {
	t.Helper()
	rows, err := store.Bindings(context.Background(), uid)
	require.NoError(t, err)
	return rows
}

// TestAgentCopy_ExclusiveBindingRequiresMapping —— 复制不带走源专属资源（FR-070 /
// 约束 31）：未给出映射 → 409 `AGENT_COPY_BLOCKED` 且 `data.problems` 点名资源类别与
// 名称，零副本零操作行（闸门在预写**之前**）；给出映射 → 副本按共享目标改写
// `provider.name` 与绑定块、源逐字节不变；映射本身非法 → 400 零操作行。
// TestAgentCopy_ExclusiveBindingRequiresMapping covers both sides of the copy gate.
func TestAgentCopy_ExclusiveBindingRequiresMapping(t *testing.T) {
	t.Parallel()

	t.Run("unmapped_exclusive_binding_blocks_before_prewrite", func(t *testing.T) {
		t.Parallel()
		env := newCopyMappingEnv(t)
		sourceBefore := readAdminFile(t, env.root, "agents/reporter/AGENT.md")

		_, err := env.admin.Copy(context.Background(), consoleActor(), "reporter", "reporter-copy", nil)
		require.ErrorIs(t, err, core.ErrAgentCopyBlocked)

		problems, ok := core.AdminErrorData(err)["problems"].([]string)
		require.True(t, ok, "409 必须带 data.problems 明细：[%v]", core.AdminErrorData(err))
		require.Contains(t, problems, "model_connection alpha", "问题清单必须点名类别与名称")

		requireAgentDirAbsent(t, env.root, "reporter-copy")
		require.Zero(t, countOperations(t, env.ops, core.ActionAgentCopy),
			"专属闸门在预写之前：不得留下 pending 行")
		require.Equal(t, sourceBefore, readAdminFile(t, env.root, "agents/reporter/AGENT.md"))
	})

	t.Run("explicit_mapping_rewrites_copy", func(t *testing.T) {
		t.Parallel()
		env := newCopyMappingEnv(t)
		sourceBefore := readAdminFile(t, env.root, "agents/reporter/AGENT.md")

		view, err := env.admin.Copy(context.Background(), consoleActor(), "reporter", "reporter-copy",
			[]core.ResourceMapping{{
				Kind:      core.BindingModelConnection,
				SourceUID: env.exclusive.ResourceUID,
				TargetUID: env.shared.ResourceUID,
			}})
		require.NoError(t, err, "逐条给出映射之后复制必须成功")
		require.Equal(t, "reporter-copy", view.Agent)

		target := readAdminFile(t, env.root, "agents/reporter-copy/AGENT.md")
		bindings, parseErr := workspace.ParseResourceBindings([]byte(target))
		require.NoError(t, parseErr)
		require.Equal(t, []core.ResourceBinding{{Name: "beta", UID: env.shared.ResourceUID}},
			bindings.ModelConnections, "绑定块必须整块改写成映射目标")
		require.Contains(t, target, `"beta"`, "provider.name 必须随映射一并改写")
		require.NotContains(t, target, string(env.exclusive.ResourceUID),
			"副本不得再引用源专属资源")
		require.Contains(t, target, "正文必须逐字保留。")
		require.Equal(t, sourceBefore, readAdminFile(t, env.root, "agents/reporter/AGENT.md"),
			"复制不得改动源 Agent")

		row := findConsoleOperation(t, env.ops, core.ActionAgentCopy)
		require.Equal(t, core.OperationSuccess, row.Status)
		require.Equal(t, "reporter-copy", row.TargetID)
		var detail agentCopyOperationDetail
		require.NoError(t, json.Unmarshal(row.DetailJSON, &detail))
		require.Equal(t, []string{"model_connection:" + string(env.exclusive.ResourceUID) +
			"→" + string(env.shared.ResourceUID)}, detail.Mapped)

		// 复制成功即按副本的定义重建投影（T128）：行挂在**副本的新身份**上，
		// 不是源身份，更不是源专属资源的旧绑定。
		targetRec, exists, readErr := env.lifecycle.Read(context.Background(), "reporter-copy")
		require.NoError(t, readErr)
		require.True(t, exists)
		require.NotEqual(t, env.sourceUID, targetRec.AgentUID, "副本必须拿到新身份")
		rows := mustBindings(t, storage.NewBindingStore(env.db), targetRec.AgentUID)
		require.Len(t, rows, 1)
		require.Equal(t, core.BindingModelConnection, rows[0].Kind)
		require.Equal(t, "reporter-copy", rows[0].AgentName)
		require.Equal(t, "beta", rows[0].RefName)
		require.Equal(t, env.shared.ResourceUID, rows[0].ResourceUID)
		require.Empty(t, mustBindings(t, storage.NewBindingStore(env.db), env.sourceUID),
			"副本的投影不得写到源身份上")
	})

	t.Run("invalid_mapping_rejected_before_prewrite", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			mapping func(*copyMappingEnv) []core.ResourceMapping
		}{
			{"unknown_kind", func(env *copyMappingEnv) []core.ResourceMapping {
				return []core.ResourceMapping{{
					Kind: "tool", SourceUID: env.exclusive.ResourceUID, TargetUID: env.shared.ResourceUID,
				}}
			}},
			{"source_uid_not_bound", func(env *copyMappingEnv) []core.ResourceMapping {
				return []core.ResourceMapping{{
					Kind: core.BindingModelConnection, SourceUID: env.shared.ResourceUID,
					TargetUID: env.exclusive.ResourceUID,
				}}
			}},
			{"duplicate_source", func(env *copyMappingEnv) []core.ResourceMapping {
				return []core.ResourceMapping{
					{Kind: core.BindingModelConnection, SourceUID: env.exclusive.ResourceUID, TargetUID: env.shared.ResourceUID},
					{Kind: core.BindingModelConnection, SourceUID: env.exclusive.ResourceUID, TargetUID: env.shared.ResourceUID},
				}
			}},
			{"target_reuses_source_uid", func(env *copyMappingEnv) []core.ResourceMapping {
				return []core.ResourceMapping{{
					Kind: core.BindingModelConnection, SourceUID: env.exclusive.ResourceUID,
					TargetUID: env.exclusive.ResourceUID,
				}}
			}},
			{"target_not_shared", func(env *copyMappingEnv) []core.ResourceMapping {
				return []core.ResourceMapping{{
					Kind: core.BindingModelConnection, SourceUID: env.exclusive.ResourceUID,
					TargetUID: env.exclusiveTarget.ResourceUID,
				}}
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				env := newCopyMappingEnv(t)
				_, err := env.admin.Copy(context.Background(), consoleActor(), "reporter",
					"reporter-copy", tc.mapping(env))
				var invalid *core.InvalidInputError
				require.ErrorAs(t, err, &invalid, "非法映射必须是 400 分类：%v", err)
				requireAgentDirAbsent(t, env.root, "reporter-copy")
				require.Zero(t, countOperations(t, env.ops, core.ActionAgentCopy),
					"请求校验在预写之前：零操作行")
			})
		}
	})
}

// TestAgentAdmin_WriteActionsRebuildBindingProjection —— 创建 / 归档 / 异名恢复成功之后
// 投影按**落盘的定义**重建（T128 / R4.6）。投影是可重建视图，但写动作结束的那一刻它
// 必须与文件一致：删除闸门与准入都按投影反查引用，留着旧行会误挡删除、按旧身份放行。
//
// 三个动作的可观测点各不相同：归档前投影为空、归档后行按归档副本回来；异名恢复把行
// 挂到新身份并把归档身份的旧行清掉；创建模板不带绑定块，因此以「失败记录被摘除」
// 佐证重建确实走了成功分支（空行集和"没重建"在数据上无法区分）。
// TestAgentAdmin_WriteActionsRebuildBindingProjection pins the projection rebuild to the
// on-disk definition for create, archive and renamed restore.
func TestAgentAdmin_WriteActionsRebuildBindingProjection(t *testing.T) {
	t.Parallel()
	env := newCopyMappingEnv(t)
	ctx := context.Background()
	store := storage.NewBindingStore(env.db)

	// 归档：清空投影后归档，行必须按归档副本重建回来（身份不变）。
	require.NoError(t, store.Clear(ctx, env.sourceUID))
	require.Empty(t, mustBindings(t, store, env.sourceUID))

	archive, err := env.admin.Archive(ctx, consoleActor(), "reporter")
	require.NoError(t, err)
	archivedRows := mustBindings(t, store, env.sourceUID)
	require.Len(t, archivedRows, 1, "归档成功之后投影必须按归档副本重建")
	require.Equal(t, "reporter", archivedRows[0].AgentName)
	require.Equal(t, "alpha", archivedRows[0].RefName)
	require.Equal(t, env.exclusive.ResourceUID, archivedRows[0].ResourceUID)

	// 异名恢复：副本换新身份，行跟到新身份，归档身份的旧行被清掉。
	_, err = env.admin.Restore(ctx, consoleActor(), archive.ArchiveID, "reporter-restored")
	require.NoError(t, err)
	restoredRec, exists, readErr := env.lifecycle.Read(ctx, "reporter-restored")
	require.NoError(t, readErr)
	require.True(t, exists)
	require.NotEqual(t, env.sourceUID, restoredRec.AgentUID, "异名恢复必须引导新身份")

	restoredRows := mustBindings(t, store, restoredRec.AgentUID)
	require.Len(t, restoredRows, 1)
	require.Equal(t, "reporter-restored", restoredRows[0].AgentName)
	require.Equal(t, "alpha", restoredRows[0].RefName)
	require.Empty(t, mustBindings(t, store, env.sourceUID),
		"归档身份已无定义文件可重建：旧行必须清掉，否则删除闸门永远看到引用")

	// 创建：先把一个失败分类挂到目标名上，创建成功必须摘除（模板没有绑定块，
	// 没有行可比，失败集合是这条路径唯一的可观测证据）。
	require.NoError(t, env.deps.Projection.RebuildAgentDefinition(ctx, "fresh", "",
		[]byte("---\nresource_bindings:\n  model_connection: {\n---\n")))
	reason, failed := env.deps.Projection.ProjectionError("fresh")
	require.True(t, failed, "预置的不可解析定义必须留下失败记录")
	require.Equal(t, projectionReasonInvalidBindings, reason)

	_, err = env.admin.Create(ctx, consoleActor(), core.AgentSpec{Name: "fresh"})
	require.NoError(t, err)
	reason, failed = env.deps.Projection.ProjectionError("fresh")
	require.False(t, failed, "创建成功之后失败记录必须被摘除（仍记 %q）", reason)
	freshRec, freshExists, freshErr := env.lifecycle.Read(ctx, "fresh")
	require.NoError(t, freshErr)
	require.True(t, freshExists)
	require.Empty(t, mustBindings(t, store, freshRec.AgentUID), "模板不带绑定块：投影为空即正确")
}

// TestAgentArchive_TimeoutRollsBack：模块卡 §4.3.3 原样落地（约束 32 ③）——在途 Run
// 等待上限到期 → 409 AGENT_BUSY（data.run_ids）且状态文件、注册表、定时与目录树
// 完整回滚。
// TestAgentArchive_TimeoutRollsBack is the card's verbatim regression.
func TestAgentArchive_TimeoutRollsBack(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t, "reporter")
	env.startNeverEndingRun(t, "reporter", "run-1")
	before := env.snapshotEverything(t) // 状态文件字节、注册表、定时、目录树

	env.clock.Advance(31 * time.Second) // 或注入 1ms 超时
	_, err := env.admin.Archive(context.Background(), core.Actor{Subject: "test"}, "reporter")
	var busy *core.AgentBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v, want AgentBusyError", err)
	}
	if !slices.Contains(busy.RunIDs, "run-1") {
		t.Fatalf("run ids = %v", busy.RunIDs)
	}
	// 约束 1a：pending 之后的失败必须带可查的 operation_id。
	operationID, _ := core.AdminErrorData(err)["operation_id"].(string)
	if operationID == "" {
		t.Fatalf("err = %v, want data.operation_id", err)
	}
	if row := findConsoleOperation(t, env.ops, core.ActionAgentArchive); row.OperationID != operationID {
		t.Fatalf("operation_id = %q, want %q", operationID, row.OperationID)
	}
	if !env.equalEverything(t, before) {
		t.Fatal("archive timeout did not roll back state/registry/schedules/files")
	}
}

// TestAgentArchive_RollbackMatrix：图 F 回滚矩阵的两档——归档目标已存在（目录零变化、
// 状态 generation 再 +1 回退、注册恢复、操作 failed CONFLICT）与归档行写入失败
// （反向 rename + 状态 / 注册回滚、归档目录零残留、操作 failed）（约束 32）。
func TestAgentArchive_RollbackMatrix(t *testing.T) {
	t.Parallel()

	t.Run("target_exists", func(t *testing.T) {
		t.Parallel()
		env := newAgentEnv(t, "reporter")
		sourceBefore := readAdminFile(t, env.root, "agents/reporter/AGENT.md")

		// 归档标识 = `<name>-YYYYMMDD-HHMMSS`（UTC）；夹具时钟固定，标识可复算。
		target := filepath.Join(env.root, "archive", "reporter-"+archiveStamp(env.clock))
		require.NoError(t, os.MkdirAll(target, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(target, "marker.txt"), []byte("已存在"), 0o600))

		_, err := env.admin.Archive(context.Background(), consoleActor(), "reporter")
		require.Error(t, err)
		require.ErrorIs(t, err, workspace.ErrArchiveTargetExists)
		// 边界映射 409 CONFLICT，且 pending 之后失败带 operation_id（约束 1a / 约束 33）。
		require.ErrorIs(t, err, core.ErrConflict)
		require.NotEmpty(t, core.AdminErrorData(err)["operation_id"])

		require.Equal(t, sourceBefore, readAdminFile(t, env.root, "agents/reporter/AGENT.md"),
			"归档目标冲突不得动源目录")
		require.FileExists(t, filepath.Join(target, "marker.txt"), "已存在的归档目录原样保留")

		rec, exists, readErr := env.lifecycle.Read(context.Background(), "reporter")
		require.NoError(t, readErr)
		require.True(t, exists)
		require.Equal(t, core.AgentActive, rec.State, "失败必须回退到归档前状态")
		require.EqualValues(t, 4, rec.Generation, "归档 +1、回退再 +1")

		_, registered := env.registry.Lookup("reporter")
		require.True(t, registered, "失败必须把注册条目放回")
		require.True(t, env.scheduler.isRegistered("reporter"))
		require.Equal(t, []core.Schedule{{
			ID: "reporter-daily", Cron: "0 0 9 * * *", Zone: "UTC", Message: "每日巡检",
		}}, env.scheduler.snapshotSchedules()["reporter"])

		row := findConsoleOperation(t, env.ops, core.ActionAgentArchive)
		require.Equal(t, core.OperationFailed, row.Status)
		require.Equal(t, agentResultConflict, row.ResultCode)
	})

	t.Run("insert_failure", func(t *testing.T) {
		t.Parallel()
		env := newAgentEnv(t, "reporter")
		env.deps.Archives = failingInsertArchiveStore{
			AgentArchiveStore: env.deps.Archives,
			err:               errors.New("归档表不可写"),
		}
		env.build()
		sourceBefore := readAdminFile(t, env.root, "agents/reporter/AGENT.md")

		_, err := env.admin.Archive(context.Background(), consoleActor(), "reporter")
		require.Error(t, err)
		require.Contains(t, err.Error(), "归档表不可写")
		require.NotEmpty(t, core.AdminErrorData(err)["operation_id"], "归档行失败同样要可追查")

		require.Equal(t, sourceBefore, readAdminFile(t, env.root, "agents/reporter/AGENT.md"),
			"归档行失败必须反向 rename 回源目录")
		_, statErr := os.Stat(filepath.Join(env.root, "archive", "reporter-"+archiveStamp(env.clock)))
		require.ErrorIs(t, statErr, os.ErrNotExist, "回滚后归档目录不得残留")

		rec, exists, readErr := env.lifecycle.Read(context.Background(), "reporter")
		require.NoError(t, readErr)
		require.True(t, exists)
		require.Equal(t, core.AgentActive, rec.State)
		require.EqualValues(t, 4, rec.Generation)
		_, registered := env.registry.Lookup("reporter")
		require.True(t, registered)
		require.True(t, env.scheduler.isRegistered("reporter"))

		rows, listErr := env.admin.Archived(context.Background())
		require.NoError(t, listErr)
		require.Empty(t, rows, "归档行失败不得留下半条记录")

		row := findConsoleOperation(t, env.ops, core.ActionAgentArchive)
		require.Equal(t, core.OperationFailed, row.Status)
		require.Equal(t, lifecycleResultInternal, row.ResultCode)
	})
}

// TestAgentRestore_OriginalNameGeneration：恢复的两条路径与失败回滚（约束 31 / 图 F）——
// 原名保留并递增 generation（条件迁移，不 Bootstrap），异名 Bootstrap 为 disabled /
// generation=1；两条路径都清空 schedules、默认停用、不自动回到注册表；失败时归档行
// 保持未恢复、归档目录不动，目标零残留且可重试。
func TestAgentRestore_OriginalNameGeneration(t *testing.T) {
	t.Parallel()

	t.Run("original_name", func(t *testing.T) {
		t.Parallel()
		env := newAgentEnv(t, "reporter")
		archived, err := env.admin.Archive(context.Background(), consoleActor(), "reporter")
		require.NoError(t, err)
		require.Equal(t, "reporter", archived.AgentName)

		archivedRec, exists, readErr := env.lifecycle.Read(context.Background(), "reporter")
		require.NoError(t, readErr)
		require.True(t, exists)
		require.Equal(t, core.AgentArchived, archivedRec.State)
		require.EqualValues(t, 3, archivedRec.Generation, "active(2) → archived(3)")

		view, err := env.admin.Restore(context.Background(), consoleActor(), archived.ArchiveID, "")
		require.NoError(t, err)
		require.Equal(t, "reporter", view.Agent)

		rec, exists, readErr := env.lifecycle.Read(context.Background(), "reporter")
		require.NoError(t, readErr)
		require.True(t, exists)
		require.Equal(t, core.AgentDisabled, rec.State, "恢复默认停用")
		require.EqualValues(t, 4, rec.Generation, "原名恢复保留 generation 并递增，不重置为 1")

		row := findConsoleOperation(t, env.ops, core.ActionAgentRestore)
		require.Equal(t, core.OperationSuccess, row.Status)
		require.Equal(t, row.OperationID, rec.OperationID)

		rows, listErr := env.admin.Archived(context.Background())
		require.NoError(t, listErr)
		require.Len(t, rows, 1)
		require.NotNil(t, rows[0].RestoredAt)
		require.Equal(t, "reporter", rows[0].RestoredAgentName)

		require.DirExists(t, filepath.Join(env.root, "archive", archived.ArchiveID),
			"恢复是复制，归档目录不移动")
		def := readAdminFile(t, env.root, "agents/reporter/AGENT.md")
		require.Contains(t, def, "schedules: []", "恢复清空定时任务")
		require.NotContains(t, def, "reporter-daily")

		_, registered := env.registry.Lookup("reporter")
		require.False(t, registered, "恢复不自动回到注册表")
		require.False(t, env.scheduler.isRegistered("reporter"), "恢复不自动启用定时")
	})

	t.Run("renamed_bootstrap", func(t *testing.T) {
		t.Parallel()
		env := newAgentEnv(t, "reporter")
		archived, err := env.admin.Archive(context.Background(), consoleActor(), "reporter")
		require.NoError(t, err)

		view, err := env.admin.Restore(context.Background(), consoleActor(), archived.ArchiveID, "reporter-restored")
		require.NoError(t, err)
		require.Equal(t, "reporter-restored", view.Agent)

		rec, exists, readErr := env.lifecycle.Read(context.Background(), "reporter-restored")
		require.NoError(t, readErr)
		require.True(t, exists)
		require.Equal(t, core.AgentDisabled, rec.State)
		require.EqualValues(t, 1, rec.Generation, "异名恢复 Bootstrap 为 generation=1")

		original, exists, readErr := env.lifecycle.Read(context.Background(), "reporter")
		require.NoError(t, readErr)
		require.True(t, exists)
		require.Equal(t, core.AgentArchived, original.State, "原名状态文件保持 archived")

		def := readAdminFile(t, env.root, "agents/reporter-restored/AGENT.md")
		require.Contains(t, def, "name: reporter-restored")
		require.Contains(t, def, "agent_name: reporter-restored")
		require.Contains(t, def, "schedules: []")

		rows, listErr := env.admin.Archived(context.Background())
		require.NoError(t, listErr)
		require.Len(t, rows, 1)
		require.NotNil(t, rows[0].RestoredAt)
		require.Equal(t, "reporter-restored", rows[0].RestoredAgentName)
		require.DirExists(t, filepath.Join(env.root, "archive", archived.ArchiveID))
	})

	t.Run("failure_keeps_archive_retryable", func(t *testing.T) {
		t.Parallel()

		for _, renamed := range []bool{false, true} {
			label := "original"
			newName := ""
			target := "reporter"
			if renamed {
				label = "renamed"
				newName = "reporter-restored"
				target = newName
			}
			t.Run(label, func(t *testing.T) {
				t.Parallel()
				env := newAgentEnv(t, "reporter")
				archived, err := env.admin.Archive(context.Background(), consoleActor(), "reporter")
				require.NoError(t, err)

				// staging 残留让目录复制在状态迁移之后失败：回滚必须把状态与目标收回去。
				residue := filepath.Join(env.root, "agents", target+".staging")
				require.NoError(t, os.WriteFile(residue, []byte("residue"), 0o600))

				_, err = env.admin.Restore(context.Background(), consoleActor(), archived.ArchiveID, newName)
				require.Error(t, err)

				_, statErr := os.Stat(filepath.Join(env.root, "agents", target))
				require.ErrorIs(t, statErr, os.ErrNotExist, "失败不得留下目标目录")
				if renamed {
					_, statErr = os.Stat(statePath(env.root, target))
					require.ErrorIs(t, statErr, os.ErrNotExist, "异名失败必须 Discard 目标状态文件")
				} else {
					rec, exists, readErr := env.lifecycle.Read(context.Background(), target)
					require.NoError(t, readErr)
					require.True(t, exists)
					require.Equal(t, core.AgentArchived, rec.State, "原名失败必须回退为 archived")
					require.EqualValues(t, 5, rec.Generation, "archived(3) → disabled(4) → archived(5)")
				}

				rows, listErr := env.admin.Archived(context.Background())
				require.NoError(t, listErr)
				require.Len(t, rows, 1)
				require.Nil(t, rows[0].RestoredAt, "失败后归档行保持未恢复")
				require.DirExists(t, filepath.Join(env.root, "archive", archived.ArchiveID),
					"归档目录从不移动，失败后可重试")

				row := findConsoleOperation(t, env.ops, core.ActionAgentRestore)
				require.Equal(t, core.OperationFailed, row.Status)

				// 清掉测试预置的残留后重试：归档行仍未被标记，恢复必须成功。
				require.NoError(t, os.Remove(residue))
				_, err = env.admin.Restore(context.Background(), consoleActor(), archived.ArchiveID, newName)
				require.NoError(t, err, "失败不消耗归档：清理后重试必须成功")

				retryRec, exists, readErr := env.lifecycle.Read(context.Background(), target)
				require.NoError(t, readErr)
				require.True(t, exists)
				require.Equal(t, core.AgentDisabled, retryRec.State)
				if renamed {
					require.EqualValues(t, 1, retryRec.Generation)
				} else {
					require.EqualValues(t, 6, retryRec.Generation, "archived(5) → disabled(6)")
				}

				rows, listErr = env.admin.Archived(context.Background())
				require.NoError(t, listErr)
				require.Len(t, rows, 1)
				require.NotNil(t, rows[0].RestoredAt, "重试成功后归档行标记恢复")
			})
		}
	})
}

// —— US-12 恢复动作矩阵（T067A / 约束 44 / 契约 §9.4）——
// 恢复只查证、不重做：本组用例逐动作验证 applied / not_applied / indeterminate 三态，
// 以及约束 44 点名的**原名恢复崩溃窗口**（状态已迁移、目录未落位）的补偿。每个子用例
// 独立夹具、只放一条 pending 行；判定全部来自真实状态文件、目录树与归档行。
// Recovery verifies; it never replays. Each case builds one pending row over real state.

// recoveryDepsForAgentAdmin 把 Agent 写面夹具的共享实例装成恢复依赖：与 wire.go 的
// 生产装配同源（同一库句柄、同一写锁表、同一工作区根），不另造第二套仓储。
// recoveryDepsForAgentAdmin mirrors the production wiring over the fixture's shared
// stores; nothing is re-instantiated from scratch.
func recoveryDepsForAgentAdmin(env *agentAdminEnv) RecoveryDeps {
	return RecoveryDeps{
		Operations: env.ops,
		Resources:  env.versions,
		Lifecycle:  env.lifecycle,
		Editor:     workspace.NewAgentEditor(workspace.AgentEditorDeps{Root: env.root}),
		Logger:     env.deps.Logger,
		Now:        env.deps.Now,
		Root:       env.root,
		ConfigPath: filepath.Join(env.root, configFileName),
		Skills:     workspace.NewSkillLibrary(env.root),
		Gate:       env.gate,
		Archives:   storage.NewAgentAdminStore(env.db),
		Notify:     storage.NewNotifyAdminStore(env.db),
	}
}

// agentRecoveryDetail 序列化一条 Agent 动作的预写详情（与生产写入口同一键名）。
// agentRecoveryDetail marshals one agent action detail with the production keys.
func agentRecoveryDetail(t *testing.T, detail any) []byte {
	t.Helper()
	raw, err := json.Marshal(detail)
	require.NoError(t, err)
	return raw
}

// TestRecoverAdmin_ActionMatrix：Agent 动作组的三态矩阵 + 原名恢复崩溃窗口补偿。
// TestRecoverAdmin_ActionMatrix walks the create/files/archive/restore verdicts and the
// original-name restore compensation mandated by constraint 44.
func TestRecoverAdmin_ActionMatrix(t *testing.T) {
	ctx := context.Background()

	t.Run("create_applied", func(t *testing.T) {
		env := newAgentAdminEnv(t)
		opID := core.NewOperationID(baselineClock())
		_, err := env.lifecycle.Bootstrap(ctx, "newbie", opID)
		require.NoError(t, err)
		writeWorkspaceFile(t, env.root, "agents/newbie/AGENT.md", "---\nname: newbie\n---\n")

		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentCreate,
			TargetKind: targetKindAgent, TargetID: "newbie",
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationSuccess, op.Status)
		require.Equal(t, core.VerificationApplied, op.Verification)
	})

	t.Run("create_not_applied", func(t *testing.T) {
		env := newAgentAdminEnv(t)
		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentCreate,
			TargetKind: targetKindAgent, TargetID: "newbie",
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)
	})

	t.Run("create_half_state_indeterminate", func(t *testing.T) {
		env := newAgentAdminEnv(t)
		opID := core.NewOperationID(baselineClock())
		_, err := env.lifecycle.Bootstrap(ctx, "newbie", opID)
		require.NoError(t, err)

		// 状态文件先落、目录未落位：半成品状态不猜成功也不猜失败。
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentCreate,
			TargetKind: targetKindAgent, TargetID: "newbie",
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationUnknown, op.Status)
		require.Equal(t, core.VerificationIndeterminate, op.Verification)
	})

	t.Run("files_update_applied", func(t *testing.T) {
		env := newAgentAdminEnv(t)
		writeWorkspaceFile(t, env.root, "agents/writer/AGENT.md", "---\nname: writer\n---\n\n新\n")
		_, etag, err := workspace.ManagedAgentFiles(env.root, "writer")
		require.NoError(t, err)

		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentFilesUpdate,
			TargetKind: targetKindAgent, TargetID: "writer",
			DetailJSON: agentRecoveryDetail(t, agentFilesOperationDetail{
				BaseETag:      core.FormatStrongETag(strings.Repeat("0", 64)),
				CandidateETag: etag,
			}),
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationSuccess, op.Status)
		require.Equal(t, core.VerificationApplied, op.Verification)
	})

	t.Run("files_update_not_applied", func(t *testing.T) {
		env := newAgentAdminEnv(t)
		writeWorkspaceFile(t, env.root, "agents/writer/AGENT.md", "---\nname: writer\n---\n\n旧\n")
		_, etag, err := workspace.ManagedAgentFiles(env.root, "writer")
		require.NoError(t, err)

		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentFilesUpdate,
			TargetKind: targetKindAgent, TargetID: "writer",
			DetailJSON: agentRecoveryDetail(t, agentFilesOperationDetail{
				BaseETag:      etag,
				CandidateETag: core.FormatStrongETag(strings.Repeat("1", 64)),
			}),
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)
	})

	t.Run("archive_not_applied", func(t *testing.T) {
		env := newAgentEnv(t, "writer")
		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentArchive,
			TargetKind: targetKindAgent, TargetID: "writer",
			DetailJSON: agentRecoveryDetail(t, agentArchiveOperationDetail{
				BeforeState: string(core.AgentActive),
			}),
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env.agentAdminEnv)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)
		require.DirExists(t, filepath.Join(env.root, agentsDirName, "writer"), "目录不得被恢复挪动")
	})

	t.Run("archive_applied_defensive", func(t *testing.T) {
		env := newAgentEnv(t, "writer")
		opID := core.NewOperationID(baselineClock())
		rec, exists, readErr := env.lifecycle.Read(ctx, "writer")
		require.NoError(t, readErr)
		require.True(t, exists)
		_, err := env.lifecycle.Transition(ctx, "writer", core.AgentActive, rec.Generation,
			core.AgentArchived, opID)
		require.NoError(t, err)
		archived, err := workspace.ArchiveAgentDir(env.root, "writer", baselineClock())
		require.NoError(t, err)

		// 防御分支：生产的归档行与终态同事务，pending 行本不该命中本分支；这里用裸连接
		// 直接落行（与既有判卷先例同一手法），验证"行 + 清单哈希"这条最强判据。
		raw, openErr := sql.Open("sqlite", filepath.ToSlash(filepath.Join(env.root, databaseFileName)))
		require.NoError(t, openErr)
		t.Cleanup(func() { require.NoError(t, raw.Close()) })
		_, insertErr := raw.ExecContext(ctx,
			`INSERT INTO agent_archives
			   (archive_id, agent_name, archived_at, content_hash, manifest_json, operation_id)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			archived.ArchiveID, "writer", baselineClock().UnixMilli(),
			archived.ContentHash, archived.ManifestJSON, opID)
		require.NoError(t, insertErr)

		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentArchive,
			TargetKind: targetKindAgent, TargetID: "writer",
			DetailJSON: agentRecoveryDetail(t, agentArchiveOperationDetail{
				BeforeState: string(core.AgentActive),
			}),
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env.agentAdminEnv)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationSuccess, op.Status)
		require.Equal(t, core.VerificationApplied, op.Verification)
	})

	t.Run("archive_moved_without_row_indeterminate", func(t *testing.T) {
		env := newAgentEnv(t, "writer")
		opID := core.NewOperationID(baselineClock())
		rec, exists, readErr := env.lifecycle.Read(ctx, "writer")
		require.NoError(t, readErr)
		require.True(t, exists)
		_, err := env.lifecycle.Transition(ctx, "writer", core.AgentActive, rec.Generation,
			core.AgentArchived, opID)
		require.NoError(t, err)
		_, err = workspace.ArchiveAgentDir(env.root, "writer", baselineClock())
		require.NoError(t, err)

		// 目录已移出、归档行缺失：效果在而记账缺，不自动补做（约束 32 ⑦）。
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentArchive,
			TargetKind: targetKindAgent, TargetID: "writer",
			DetailJSON: agentRecoveryDetail(t, agentArchiveOperationDetail{
				BeforeState: string(core.AgentActive),
			}),
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env.agentAdminEnv)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationUnknown, op.Status)
		require.Equal(t, core.VerificationIndeterminate, op.Verification)
	})

	t.Run("restore_original_crash_window_compensated", func(t *testing.T) {
		env := newAgentEnv(t, "reporter")
		archived, err := env.admin.Archive(ctx, consoleActor(), "reporter")
		require.NoError(t, err)

		opID := core.NewOperationID(baselineClock())
		rec, exists, readErr := env.lifecycle.Read(ctx, "reporter")
		require.NoError(t, readErr)
		require.True(t, exists)
		require.Equal(t, core.AgentArchived, rec.State)

		// 崩溃窗口：状态已从 archived 迁到 disabled，目录 rename 尚未发生。
		_, err = env.lifecycle.Transition(ctx, "reporter", core.AgentArchived, rec.Generation,
			core.AgentDisabled, opID)
		require.NoError(t, err)

		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentRestore,
			TargetKind: targetKindAgent, TargetID: "reporter",
			DetailJSON: agentRecoveryDetail(t, agentRestoreOperationDetail{
				ArchiveID: archived.ArchiveID, Target: "reporter", Renamed: false,
			}),
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env.agentAdminEnv)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)

		compensated, exists, readErr := env.lifecycle.Read(ctx, "reporter")
		require.NoError(t, readErr)
		require.True(t, exists)
		require.Equal(t, core.AgentArchived, compensated.State, "崩溃窗口必须补偿回 archived")
		require.EqualValues(t, rec.Generation+2, compensated.Generation,
			"archived(g) → disabled(g+1) → archived(g+2)，只补偿不回退")
		require.Equal(t, opID, compensated.OperationID)
		require.Contains(t, env.logs.String(), "补偿回 archived")

		rows, listErr := env.admin.Archived(ctx)
		require.NoError(t, listErr)
		require.Len(t, rows, 1)
		require.Nil(t, rows[0].RestoredAt, "补偿后归档行保持未恢复、可重试")
		require.DirExists(t, filepath.Join(env.root, agentArchiveDirName, archived.ArchiveID))
	})

	t.Run("restore_original_not_started_not_applied", func(t *testing.T) {
		env := newAgentEnv(t, "reporter")
		archived, err := env.admin.Archive(ctx, consoleActor(), "reporter")
		require.NoError(t, err)

		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentRestore,
			TargetKind: targetKindAgent, TargetID: "reporter",
			DetailJSON: agentRecoveryDetail(t, agentRestoreOperationDetail{
				ArchiveID: archived.ArchiveID, Target: "reporter", Renamed: false,
			}),
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env.agentAdminEnv)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)

		rec, exists, readErr := env.lifecycle.Read(ctx, "reporter")
		require.NoError(t, readErr)
		require.True(t, exists)
		require.Equal(t, core.AgentArchived, rec.State, "未开始的恢复不得改动状态")
	})

	t.Run("restore_renamed_before_bootstrap_not_applied", func(t *testing.T) {
		env := newAgentEnv(t, "reporter")
		archived, err := env.admin.Archive(ctx, consoleActor(), "reporter")
		require.NoError(t, err)

		opID := core.NewOperationID(baselineClock())
		beginPendingOperation(t, env.ops, core.OperationRecord{
			OperationID: opID, Action: core.ActionAgentRestore,
			TargetKind: targetKindAgent, TargetID: "reporter-restored",
			DetailJSON: agentRecoveryDetail(t, agentRestoreOperationDetail{
				ArchiveID: archived.ArchiveID, Target: "reporter-restored", Renamed: true,
			}),
		})
		require.NoError(t, recoverPendingOperations(ctx, recoveryDepsForAgentAdmin(env.agentAdminEnv)))

		op := getOperation(t, env.ops, opID)
		require.Equal(t, core.OperationFailed, op.Status)
		require.Equal(t, core.VerificationNotApplied, op.Verification)

		_, statErr := os.Stat(filepath.Join(env.root, agentsDirName, "reporter-restored"))
		require.ErrorIs(t, statErr, fs.ErrNotExist, "恢复不得替未开始的异名恢复建目录")
	})
}

// TestAgentAdmin_ConcurrentCreate：同名并发创建由 Agent 写锁串行化——目标存在性在锁内
// 复核，恰好一个成功（disabled / generation=1 / 状态文件记本次操作），其余在预写前或
// 锁内失败；台账不留 pending，目录无 staging / backup 残骸（012-race 必跑）。
// TestAgentAdmin_ConcurrentCreate pins the gate re-check: exactly one creator wins and
// the operation ledger holds no pending rows.
func TestAgentAdmin_ConcurrentCreate(t *testing.T) {
	env := newAgentAdminEnv(t)

	const creators = 6
	start := make(chan struct{})
	errs := make([]error, creators)
	var wg sync.WaitGroup
	for i := 0; i < creators; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, errs[index] = env.admin.Create(context.Background(), consoleActor(),
				core.AgentSpec{Name: "racer"})
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i := 0; i < creators; i++ {
		if errs[i] == nil {
			winners++
			continue
		}
		var invalid *core.InvalidInputError
		require.ErrorAs(t, errs[i], &invalid, "creator %d 必须按 400 失败", i)
	}
	require.Equal(t, 1, winners, "同名并发创建只能有一个成功")

	rec, exists, err := env.lifecycle.Read(context.Background(), "racer")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentDisabled, rec.State, "创建默认停用")
	require.EqualValues(t, 1, rec.Generation)

	successRows := make([]core.OperationRecord, 0, 1)
	for _, row := range listConsoleOperations(t, env.ops) {
		require.NotEqual(t, core.OperationPending, row.Status, "并发创建后不得留下 pending 行")
		if row.Action == core.ActionAgentCreate && row.Status == core.OperationSuccess {
			successRows = append(successRows, row)
		}
	}
	require.Len(t, successRows, 1)
	require.Equal(t, successRows[0].OperationID, rec.OperationID,
		"状态文件必须记录胜出者的创建操作")

	_, statErr := os.Stat(filepath.Join(env.root, agentsDirName, "racer"))
	require.NoError(t, statErr, "胜出者目录必须存在")
	entries, readErr := os.ReadDir(filepath.Join(env.root, agentsDirName))
	require.NoError(t, readErr)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".staging", "不得留下 staging 残骸")
		require.NotContains(t, entry.Name(), ".backup", "不得留下 backup 残骸")
	}
}

// TestAgentArchive_ConcurrentRun：归档与在途 Run 的时序——先注销定时与注册、发出取消，
// 在等待窗口内 Run 退出后继续完成；等待期间状态不得提前迁移，完成后状态 archived、
// 目录移入归档区、归档行与操作终态各一件（012-race 必跑）。
// TestAgentArchive_ConcurrentRun pins the wait: no state migration while a run is
// alive, and the archive completes once the run exits.
func TestAgentArchive_ConcurrentRun(t *testing.T) {
	env := newAgentEnv(t, "reporter")
	canceller := newArchiveExitCanceller("run-1")
	env.deps.RunCanceller = canceller
	env.deps.BusyWait = 10 * time.Second
	env.build()

	type archiveOutcome struct {
		record core.AgentArchiveRecord
		err    error
	}
	done := make(chan archiveOutcome, 1)
	go func() {
		record, err := env.admin.Archive(context.Background(), consoleActor(), "reporter")
		done <- archiveOutcome{record: record, err: err}
	}()

	select {
	case <-canceller.signalled:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "归档未在等待窗口内发出取消信号")
	}

	waiting, exists, err := env.lifecycle.Read(context.Background(), "reporter")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentActive, waiting.State, "在途 Run 未退出前不得迁移状态")
	require.False(t, env.scheduler.isRegistered("reporter"), "等待前必须已注销调度注册")

	close(canceller.exit)

	var outcome archiveOutcome
	select {
	case outcome = <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "归档未在 Run 退出后继续")
	}
	require.NoError(t, outcome.err)
	require.NotEmpty(t, outcome.record.ArchiveID)

	archived, exists, err := env.lifecycle.Read(context.Background(), "reporter")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentArchived, archived.State)
	require.EqualValues(t, 3, archived.Generation, "active(2) → archived(3)")

	require.Equal(t, []string{"run-1"}, canceller.cancelledRunIDs(), "在途 Run 只取消一次")
	require.Empty(t, env.scheduler.snapshotSchedules()["reporter"], "归档必须退休定时任务")
	_, registered := env.registry.Lookup("reporter")
	require.False(t, registered)
	require.DirExists(t, filepath.Join(env.root, agentArchiveDirName, outcome.record.ArchiveID))
	requireAgentDirAbsent(t, env.root, "reporter")

	row := findConsoleOperation(t, env.ops, core.ActionAgentArchive)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, row.OperationID, archived.OperationID)
	for _, operation := range listConsoleOperations(t, env.ops) {
		require.NotEqual(t, core.OperationPending, operation.Status)
	}
}

// TestAgentRestore_ConcurrentCopy：复制与异名恢复争用同一目标名——写入在 Agent 写锁内
// 复核目标存在性，恰好一个成功、另一个 409；胜出者的目录 / 状态 / 操作行一致，归档行
// 只在恢复胜出时标记已恢复，无 staging / backup 残骸（012-race 必跑）。
// TestAgentRestore_ConcurrentCopy pins the shared target gate: one winner, one conflict,
// and the archive row is marked restored only when restore wins.
func TestAgentRestore_ConcurrentCopy(t *testing.T) {
	env := newAgentEnv(t, "reporter")
	_, err := env.admin.Copy(context.Background(), consoleActor(), "reporter", "source", nil)
	require.NoError(t, err)
	archived, err := env.admin.Archive(context.Background(), consoleActor(), "reporter")
	require.NoError(t, err)

	start := make(chan struct{})
	var copyErr, restoreErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, copyErr = env.admin.Copy(context.Background(), consoleActor(), "source", "target", nil)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, restoreErr = env.admin.Restore(context.Background(), consoleActor(),
			archived.ArchiveID, "target")
	}()
	close(start)
	wg.Wait()

	restoreWon := restoreErr == nil
	if copyErr == nil {
		require.False(t, restoreWon, "同一目标名不能同时复制与恢复")
		restoreWon = false
	} else {
		require.True(t, restoreWon, "复制失败时恢复必须成功：copy=%v restore=%v", copyErr, restoreErr)
	}
	if restoreWon {
		require.ErrorIs(t, copyErr, core.ErrConflict)
	} else {
		require.ErrorIs(t, restoreErr, core.ErrConflict)
	}

	rec, exists, err := env.lifecycle.Read(context.Background(), "target")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentDisabled, rec.State, "复制与异名恢复都以 disabled 起步")
	require.EqualValues(t, 1, rec.Generation)

	successRows := make([]core.OperationRecord, 0, 1)
	for _, row := range listConsoleOperations(t, env.ops) {
		require.NotEqual(t, core.OperationPending, row.Status, "并发写后不得留下 pending 行")
		if row.TargetID == "target" && row.Status == core.OperationSuccess {
			successRows = append(successRows, row)
		}
	}
	require.Len(t, successRows, 1, "目标名的成功操作只能有一行")
	require.Equal(t, successRows[0].OperationID, rec.OperationID)
	if restoreWon {
		require.Equal(t, core.ActionAgentRestore, successRows[0].Action)
	} else {
		require.Equal(t, core.ActionAgentCopy, successRows[0].Action)
	}

	definition := readAdminFile(t, env.root, "agents/target/AGENT.md")
	require.Contains(t, definition, "name: target")
	require.Contains(t, definition, "schedules: []", "目标不得继承定时任务")
	require.NotContains(t, definition, "reporter-daily")

	rows, err := env.admin.Archived(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	if restoreWon {
		require.NotNil(t, rows[0].RestoredAt)
		require.Equal(t, "target", rows[0].RestoredAgentName)
	} else {
		require.Nil(t, rows[0].RestoredAt, "恢复失败时归档行必须保持未恢复")
	}

	entries, err := os.ReadDir(filepath.Join(env.root, agentsDirName))
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".staging")
		require.NotContains(t, entry.Name(), ".backup")
	}
}
