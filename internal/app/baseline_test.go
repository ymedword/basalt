package app

// 本文件是启动基线的验收面（颗粒度文档 §3.2.1：internal/app/baseline_test.go，16 例）。
// 用例覆盖：首次基线计数、重复启动幂等、技能快照、状态文件 disabled、损坏拒绝启动、
// 部分完成补齐、pending 复用与拒绝、撤回不复活、内容变化不自动升版、MCP applied 补齐、
// 快照缺失修复、预写失败拒绝执行、中途失败回填 failed、非技能目录跳过。
// 另含 `011-race` 具名的 `TestRunBaseline_ConcurrentStartup`（§4.4：并发启动只放行一方，
// 基线只执行一次）。
// 基线在开放服务前单线程执行，全部用例离线：只碰临时工作区与临时库。
// This file covers the startup baseline; every case is offline and uses a temp workspace.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	// 判卷侧走裸连接改库（与 wire_test.go 同一先例）。
	_ "modernc.org/sqlite"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// baselineClock 是基线用例的固定时钟：操作标识、版本时间与状态文件都落在同一读数上，
// 读数可复算。The fixed clock keeps every recorded timestamp reproducible.
func baselineClock() time.Time {
	return time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
}

// newCapturedLogger 返回写进缓冲区的 logger：WARN 是"待发布 / 已撤回 / 跳过"的唯一
// 可判痕迹，必须能在用例里逐字断言。
// newCapturedLogger captures log output so WARN-only outcomes stay assertable.
func newCapturedLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// sha256Hex 在用例侧独立计算摘要——期望值不调用被测实现。
// sha256Hex computes the digest independently of the implementation under test.
func sha256Hex(t *testing.T, raw []byte) string {
	t.Helper()
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// writeWorkspaceFile 在临时工作区里落一份文件（自动创建父目录）。
func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

// baselineEnv 是一套基线夹具：临时工作区 + 临时库 + 固定时钟 + captured logger。
// 技能 weekly 是唯一实体（SKILL.md + 一份 L3），skills/draft 是无 SKILL.md 的非实体目录。
type baselineEnv struct {
	deps   BaselineDeps
	root   string
	dbPath string
	logs   *bytes.Buffer
}

// newBaselineEnv 建夹具：两个 Provider、一个 MCP、一个技能、一个 Agent。
func newBaselineEnv(t *testing.T) *baselineEnv {
	t.Helper()

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)

	writeWorkspaceFile(t, root, "skills/weekly/SKILL.md",
		"---\nname: weekly\ndescription: 周报汇总\n---\n\n正文\n")
	writeWorkspaceFile(t, root, "skills/weekly/references/guide.md", "# 指南\n")
	writeWorkspaceFile(t, root, "skills/draft/notes.md", "草稿\n")

	dbPath := filepath.Join(root, databaseFileName)
	db, err := storage.OpenWritable(context.Background(), dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	logger, logs := newCapturedLogger()
	env := &baselineEnv{
		root:   root,
		dbPath: dbPath,
		logs:   logs,
		deps: BaselineDeps{
			Operations: storage.NewManagementStore(db),
			Resources:  storage.NewResourceStore(db),
			Skills:     workspace.NewSkillLibrary(root),
			Lifecycle:  workspace.NewLifecycleStore(root, baselineClock),
			Providers: []ProviderDefinition{
				{Name: "alpha", Content: []byte("name: alpha\nbase_url: http://127.0.0.1:1\napi_key: ${BASALT_ALPHA_KEY}\ndefault_model: alpha-model\n")},
				{Name: "beta", Content: []byte("name: beta\nbase_url: http://127.0.0.1:1\napi_key: ${BASALT_BETA_KEY}\ndefault_model: beta-model\n")},
			},
			MCPServers: []MCPServerDefinition{
				{Name: "web", Content: []byte("name: web\ntransport: stdio\ncommand: node\n")},
			},
			Agents: []core.AgentEntry{{Name: "writer", Registered: true}},
			Logger: logger,
			Now:    baselineClock,
		},
	}
	return env
}

// newBaselineFixture 是颗粒度文档 §4.3.6 点名形态：返回同一套夹具的依赖面。
// newBaselineFixture is the exact shape the module card's verbatim case expects.
func newBaselineFixture(t *testing.T) BaselineDeps {
	t.Helper()
	return newBaselineEnv(t).deps
}

// baselineSnapshotValues 是公开面的计数快照：版本数、指针数、Agent 状态与操作数。
// baselineSnapshotValues is the public-surface snapshot used by the idempotence case.
type baselineSnapshotValues struct {
	VersionCount    int
	PointerCount    int
	AgentState      core.AgentState
	AgentGeneration int64
	OperationCount  int
}

// baselineSnapshot 只经公开仓储统计（ListPointers / Versions / Lifecycle.Read /
// ListOperations），不引入生产专用测试方法（颗粒度文档 §4.3.6 注）。
// baselineSnapshot reads only public store methods, as the module card requires.
func baselineSnapshot(t *testing.T, deps BaselineDeps) baselineSnapshotValues {
	t.Helper()
	ctx := context.Background()

	var snap baselineSnapshotValues
	ids := make([]struct {
		kind core.ResourceKind
		id   string
	}, 0, len(deps.Providers)+len(deps.MCPServers))
	for _, def := range deps.Providers {
		ids = append(ids, struct {
			kind core.ResourceKind
			id   string
		}{core.ResourceModelConnection, def.Name})
	}
	for _, def := range deps.MCPServers {
		ids = append(ids, struct {
			kind core.ResourceKind
			id   string
		}{core.ResourceMCPServer, def.Name})
	}
	names, err := deps.Skills.Names()
	require.NoError(t, err)
	for _, name := range names {
		ids = append(ids, struct {
			kind core.ResourceKind
			id   string
		}{core.ResourceSkill, name})
	}
	for _, item := range ids {
		versions, err := deps.Resources.Versions(ctx, item.kind, item.id)
		require.NoError(t, err)
		snap.VersionCount += len(versions)
	}

	for _, kind := range []core.ResourceKind{
		core.ResourceModelConnection, core.ResourceMCPServer, core.ResourceSkill,
	} {
		pointers, err := deps.Resources.ListPointers(ctx, kind)
		require.NoError(t, err)
		snap.PointerCount += len(pointers)
	}

	if len(deps.Agents) > 0 {
		record, exists, err := deps.Lifecycle.Read(ctx, deps.Agents[0].Name)
		require.NoError(t, err)
		require.True(t, exists, "基线必须为既有 Agent 建立状态文件")
		snap.AgentState = record.State
		snap.AgentGeneration = record.Generation
	}

	operations, err := deps.Operations.ListOperations(ctx, core.OperationFilter{Limit: 200})
	require.NoError(t, err)
	snap.OperationCount = len(operations)
	return snap
}

// baselineOperations 只取 `system.baseline_v2` 的行，按仓储固定排序（新在前）。
func baselineOperations(t *testing.T, deps BaselineDeps) []core.OperationRecord {
	t.Helper()
	operations, err := deps.Operations.ListOperations(context.Background(),
		core.OperationFilter{Action: actionBaseline, Limit: 200})
	require.NoError(t, err)
	return operations
}

// TestRunBaseline_Idempotent 逐字保留颗粒度文档 §4.3.6 的用例体：重复启动不增版本、
// 不写第二条基线操作，既有 Agent 停在 disabled。
func TestRunBaseline_Idempotent(t *testing.T) {
	t.Parallel()
	deps := newBaselineFixture(t) // 临时工作区 + 临时库 + 两个 Provider / 一个技能 / 一个 Agent

	if err := runBaseline(context.Background(), deps); err != nil {
		t.Fatalf("first baseline: %v", err)
	}
	first := baselineSnapshot(t, deps)

	if err := runBaseline(context.Background(), deps); err != nil {
		t.Fatalf("second baseline: %v", err)
	}
	second := baselineSnapshot(t, deps)

	if first.VersionCount != second.VersionCount {
		t.Fatalf("version count grew: %d -> %d", first.VersionCount, second.VersionCount)
	}
	if first.AgentState != core.AgentDisabled {
		t.Fatalf("agent state = %q, want disabled", first.AgentState)
	}
	if first.OperationCount != second.OperationCount {
		t.Fatalf("baseline wrote a second operation: %d -> %d", first.OperationCount, second.OperationCount)
	}
}

// TestRunBaseline_CreatesVersionsAndCounts：首次基线为两个 Provider、一个 MCP、一个技能
// 建 version 1 与已生效指针，`system.baseline_v2` 行以 migration 身份写下本次**处理条数**。
func TestRunBaseline_CreatesVersionsAndCounts(t *testing.T) {
	env := newBaselineEnv(t)
	ctx := context.Background()
	require.NoError(t, runBaseline(ctx, env.deps))

	for _, def := range env.deps.Providers {
		assertResourceVersion(t, env, core.ResourceModelConnection, def.Name, def.Content)
	}
	for _, def := range env.deps.MCPServers {
		assertResourceVersion(t, env, core.ResourceMCPServer, def.Name, def.Content)
	}

	operations := baselineOperations(t, env.deps)
	require.Len(t, operations, 1, "首次基线只写一条操作")
	op := operations[0]
	require.Equal(t, core.OperationSuccess, op.Status)
	require.Equal(t, baselineResultOK, op.ResultCode)
	require.Equal(t, core.ActorSubjectMigration, op.ActorSubject)
	require.Equal(t, core.AuthMethodMigration, op.AuthMethod)
	require.Equal(t, baselineSource, op.Source)
	require.Equal(t, targetKindSystem, op.TargetKind)
	require.Equal(t, targetKindSystem, op.TargetID)
	require.False(t, op.FinishedAt.IsZero(), "成功终态必须回填 finished_at")

	var counts map[string]int
	require.NoError(t, json.Unmarshal(op.DetailJSON, &counts))
	require.Equal(t, map[string]int{"providers": 2, "mcp": 1, "skills": 1, "agents": 1}, counts,
		"detail_json 记本次处理条数：两 Provider / 一 MCP / 一技能 / 一 Agent")
}

// assertResourceVersion 断言一条资源的 v1 事实：原文逐字节保真、哈希只覆盖内容、
// 指针已发布且已生效。
func assertResourceVersion(t *testing.T, env *baselineEnv, kind core.ResourceKind, id string, definition []byte) {
	t.Helper()
	ctx := context.Background()

	versions, err := env.deps.Resources.Versions(ctx, kind, id)
	require.NoError(t, err)
	require.Len(t, versions, 1, "资源 %s/%s 只该有 v1", kind, id)
	version := versions[0]
	require.Equal(t, baselineVersionNo, version.VersionNo)
	require.Equal(t, string(definition), version.DefinitionYAML, "definition_yaml 必须逐字节保真")
	require.Equal(t, sha256Hex(t, definition), version.ContentHash, "content_hash 只对定义原文取摘要")
	require.NotEmpty(t, version.OperationID, "版本行必须记下建立它的基线操作")

	pointer, err := env.deps.Resources.Pointer(ctx, kind, id)
	require.NoError(t, err)
	require.Nil(t, pointer.DeletedAt)
	require.Equal(t, version.ID, pointer.PublishedVersionID)
	require.NotNil(t, pointer.AppliedVersionID)
	require.Equal(t, version.ID, *pointer.AppliedVersionID, "基线建立的 v1 同时是已生效版本")
}

// TestRunBaseline_MaterializesSkillSnapshot：技能 v1 物化到契约路径，清单与磁盘一致，
// `storage_path` 是 `.basalt/versions/skills/<name>/<resource_uid>/1` 字面量
// （2026-10-23 裁定：新发布按身份分段；旧扁平目录只读兼容、不搬迁）。
func TestRunBaseline_MaterializesSkillSnapshot(t *testing.T) {
	env := newBaselineEnv(t)
	require.NoError(t, runBaseline(context.Background(), env.deps))

	versions, err := env.deps.Resources.Versions(context.Background(), core.ResourceSkill, "weekly")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	version := versions[0]
	require.Equal(t, ".basalt/versions/skills/weekly/"+string(version.ResourceUID)+"/1", version.StoragePath)

	var files []core.FileHash
	require.NoError(t, json.Unmarshal([]byte(version.ManifestJSON), &files))
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	require.Equal(t, []string{"SKILL.md", "references/guide.md"}, paths, "清单按 path 升序")

	snapshot := filepath.Join(env.root, "versions", "skills", "weekly", string(version.ResourceUID), "1")
	require.FileExists(t, filepath.Join(snapshot, "SKILL.md"))
	require.FileExists(t, filepath.Join(snapshot, "references", "guide.md"))
}

// TestRunBaseline_BootstrapsAgentDisabled：既有 Agent 得到 disabled / generation=1 的
// 状态文件（operation_id 指向本次基线），索引与状态文件一致。
func TestRunBaseline_BootstrapsAgentDisabled(t *testing.T) {
	env := newBaselineEnv(t)
	ctx := context.Background()
	require.NoError(t, runBaseline(ctx, env.deps))

	operations := baselineOperations(t, env.deps)
	require.Len(t, operations, 1)

	record, exists, err := env.deps.Lifecycle.Read(ctx, "writer")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentDisabled, record.State)
	require.Equal(t, int64(1), record.Generation)
	require.Equal(t, operations[0].OperationID, record.OperationID)

	require.FileExists(t, filepath.Join(env.root, "control", "agents", "writer.json"))

	index, ok, errMessage, err := env.deps.Resources.LifecycleIndex(ctx, "writer")
	require.NoError(t, err)
	require.True(t, ok, "基线必须同步可重建的生命周期索引")
	require.Empty(t, errMessage)
	require.Equal(t, record.State, index.State)
	require.Equal(t, record.Generation, index.Generation)
	require.Equal(t, record.Checksum, index.Checksum)
}

// TestRunBaseline_ConcurrentStartup 是颗粒度文档 §4.4 `011-race` 的具名用例：两个启动方
// 同时装配同一工作区时，实例锁必须恰好放行一方，基线只在胜者里执行一次——失败方在取锁处
// 止步（零写入），既有 Agent 得到唯一的 disabled/1 状态文件，不留第二条基线操作。
// 基线本身按单线程设计（启动期先取锁再装配）；本用例的断言面是"并发启动不得写出两份基线"，
// 并发发布的存储层一致性由 `TestResourceStore_ConcurrentEnsure` 覆盖。
// Two concurrent startups must serialize on the instance lock; the baseline runs once,
// and the loser performs no writes.
func TestRunBaseline_ConcurrentStartup(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reporter")

	type startup struct {
		rt  *heavyRuntime
		err error
	}
	starts := make([]startup, 2)
	begin := make(chan struct{})
	var wg sync.WaitGroup
	for i := range starts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-begin
			rt, err := fx.app().newRuntime(context.Background())
			starts[i] = startup{rt: rt, err: err}
		}()
	}
	close(begin)
	wg.Wait()

	var winners []*heavyRuntime
	for _, start := range starts {
		if start.err == nil {
			winners = append(winners, start.rt)
			rt := start.rt
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			continue
		}
		var busy *workspace.LockBusyError
		require.ErrorAs(t, start.err, &busy, "竞争失败必须是实例锁占用，而不是其它装配错误")
	}
	require.Len(t, winners, 1, "同一工作区并发启动只允许一方取得实例锁")

	db := openRead(t, fx.dbPath())
	require.Equal(t, 1, countRows(t, db,
		`SELECT COUNT(*) FROM management_operations WHERE action = 'system.baseline_v2'`),
		"基线只执行一次：失败方在取锁处止步，零写入")
	require.Equal(t, 1, countRows(t, db,
		`SELECT COUNT(*) FROM management_operations WHERE action = 'system.baseline_v2' AND status = 'success'`),
		"胜者的基线操作必须回填 success")

	record, exists, err := workspace.NewLifecycleStore(fx.root, time.Now).Read(context.Background(), "reporter")
	require.NoError(t, err)
	require.True(t, exists, "胜者的基线必须为既有 Agent 建立状态文件")
	require.Equal(t, core.AgentDisabled, record.State)
	require.Equal(t, int64(1), record.Generation)
}

// TestRunBaseline_NoWorkWritesNoOperation：空工作区无活可做 → 不写操作行、不改状态文件
// （注 13："没有工作"本身不得留下待恢复的 pending）。
func TestRunBaseline_NoWorkWritesNoOperation(t *testing.T) {
	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)

	dbPath := filepath.Join(root, databaseFileName)
	db, err := storage.OpenWritable(context.Background(), dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	logger, logs := newCapturedLogger()
	deps := BaselineDeps{
		Operations: storage.NewManagementStore(db),
		Resources:  storage.NewResourceStore(db),
		Skills:     workspace.NewSkillLibrary(root),
		Lifecycle:  workspace.NewLifecycleStore(root, baselineClock),
		Logger:     logger,
		Now:        baselineClock,
	}
	require.NoError(t, runBaseline(context.Background(), deps))

	require.Zero(t, countRows(t, openRead(t, dbPath), `SELECT COUNT(*) FROM management_operations`),
		"无活可做时不写操作行")
	require.Contains(t, logs.String(), "无需补建")
}

// TestRunBaseline_ReusesPendingBaselineOperation：库里已有一条未完成的基线操作 → 复用其
// operation_id 补齐，不另起一条。
func TestRunBaseline_ReusesPendingBaselineOperation(t *testing.T) {
	env := newBaselineEnv(t)
	ctx := context.Background()

	pendingID := core.NewOperationID(baselineClock())
	require.NoError(t, env.deps.Operations.BeginOperation(ctx, core.OperationRecord{
		OperationID:  pendingID,
		Action:       actionBaseline,
		TargetKind:   targetKindSystem,
		TargetID:     targetKindSystem,
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       baselineSource,
		Status:       core.OperationPending,
		CreatedAt:    baselineClock(),
	}))

	require.NoError(t, runBaseline(ctx, env.deps))

	operations := baselineOperations(t, env.deps)
	require.Len(t, operations, 1, "复用未完成操作，不新增第二条")
	require.Equal(t, pendingID, operations[0].OperationID)
	require.Equal(t, core.OperationSuccess, operations[0].Status)
}

// TestRunBaseline_RefusesDuplicatePendingBaselines：两条未完成的基线操作 → 无法判断复用
// 哪一条，拒绝启动且不写新操作。
func TestRunBaseline_RefusesDuplicatePendingBaselines(t *testing.T) {
	env := newBaselineEnv(t)
	ctx := context.Background()

	for range 2 {
		require.NoError(t, env.deps.Operations.BeginOperation(ctx, core.OperationRecord{
			OperationID:  core.NewOperationID(baselineClock()),
			Action:       actionBaseline,
			TargetKind:   targetKindSystem,
			TargetID:     targetKindSystem,
			ActorSubject: core.ActorSubjectMigration,
			AuthMethod:   core.AuthMethodMigration,
			Source:       baselineSource,
			Status:       core.OperationPending,
			CreatedAt:    baselineClock(),
		}))
	}

	err := runBaseline(ctx, env.deps)
	require.ErrorContains(t, err, "无法确定复用哪一条")
	require.Len(t, baselineOperations(t, env.deps), 2, "拒绝启动时不追加操作行")
	require.Zero(t, countRows(t, openRead(t, env.dbPath), `SELECT COUNT(*) FROM resource_versions`))
}

// TestRunBaseline_RefusesCorruptAgentState：状态文件损坏 → 整个基线失败且零写入；
// 扫描先于预写，损坏文件不会被"顺手重建"（约束 13）。
func TestRunBaseline_RefusesCorruptAgentState(t *testing.T) {
	env := newBaselineEnv(t)
	writeWorkspaceFile(t, env.root, "control/agents/writer.json", "{not-json")

	err := runBaseline(context.Background(), env.deps)
	require.ErrorContains(t, err, "拒绝覆盖")

	read := openRead(t, env.dbPath)
	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM management_operations`), "扫描失败时一行都不写")
	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM resource_versions`))
}

// TestRunBaseline_PartialFailureThenRerunCompletes：技能快照目录被异内容占用 → 本次基线
// 在 Provider / MCP 已补齐后失败、操作回填 failed；清掉占位目录后重跑补齐技能并成功。
func TestRunBaseline_PartialFailureThenRerunCompletes(t *testing.T) {
	env := newBaselineEnv(t)
	ctx := context.Background()

	// 确定性身份：占位目录必须正好是本次基线将要物化的目标（新发布按身份分段）。
	uid := fixtureResourceUID(t, core.ResourceSkill, "weekly")
	env.deps.NewUID = func(kind core.ResourceKind, name string) (core.ResourceUID, error) {
		return fixtureResourceUIDFor(kind, name), nil
	}
	occupiedRel := "versions/skills/weekly/" + string(uid) + "/1"
	occupied := filepath.Join(env.root, filepath.FromSlash(occupiedRel))
	writeWorkspaceFile(t, env.root, occupiedRel+"/SKILL.md",
		"---\nname: weekly\ndescription: 占位内容\n---\n\n别的正文\n")

	err := runBaseline(ctx, env.deps)
	require.ErrorContains(t, err, "拒绝覆盖", "已存在的异内容发布目录必须令本次基线失败")

	operations := baselineOperations(t, env.deps)
	require.Len(t, operations, 1)
	failedID := operations[0].OperationID
	require.Equal(t, core.OperationFailed, operations[0].Status)
	require.Equal(t, baselineResultInternal, operations[0].ResultCode)
	require.NotEmpty(t, operations[0].ErrorMessage, "失败原文必须落库，便于查证")

	require.Equal(t, 1, countResourceVersions(t, env.deps, core.ResourceModelConnection, "alpha"),
		"失败之前已完成的 Provider 版本保留（基线可重入的前提）")
	require.Equal(t, 1, countResourceVersions(t, env.deps, core.ResourceMCPServer, "web"))
	require.Zero(t, countResourceVersions(t, env.deps, core.ResourceSkill, "weekly"))

	require.NoError(t, os.RemoveAll(occupied))
	require.NoError(t, runBaseline(ctx, env.deps))

	operations = baselineOperations(t, env.deps)
	require.Len(t, operations, 2, "失败行是终态，重跑另起一条")
	// 两次操作落在同一固定时钟上，排序按随机后缀决胜——按标识区分，不靠行序。
	// Both operations share the fixed clock; identify them by id, not by row order.
	var successID string
	for _, op := range operations {
		if op.OperationID != failedID {
			successID = op.OperationID
		}
	}
	require.NotEmpty(t, successID)
	success, err := env.deps.Operations.GetOperation(ctx, successID)
	require.NoError(t, err)
	require.Equal(t, core.OperationSuccess, success.Status)
	require.Equal(t, 1, countResourceVersions(t, env.deps, core.ResourceSkill, "weekly"))
	require.Equal(t, 1, countResourceVersions(t, env.deps, core.ResourceModelConnection, "alpha"),
		"已完成项不重复建版本")
}

// countResourceVersions 数一个资源已有的版本行数。
func countResourceVersions(t *testing.T, deps BaselineDeps, kind core.ResourceKind, id string) int {
	t.Helper()
	versions, err := deps.Resources.Versions(context.Background(), kind, id)
	require.NoError(t, err)
	return len(versions)
}

// TestRunBaseline_DoesNotResurrectDeletedPointer：指针已撤回 → 只 WARN，不复活、不建版本。
func TestRunBaseline_DoesNotResurrectDeletedPointer(t *testing.T) {
	env := newBaselineEnv(t)
	ctx := context.Background()
	require.NoError(t, runBaseline(ctx, env.deps))

	read := openRead(t, env.dbPath)
	_, err := read.ExecContext(ctx,
		`UPDATE resource_pointers SET deleted_at = ? WHERE resource_kind = ? AND resource_id = ?`,
		baselineClock().UnixMilli(), string(core.ResourceModelConnection), "alpha")
	require.NoError(t, err)

	require.NoError(t, runBaseline(ctx, env.deps))

	require.Equal(t, 1, countResourceVersions(t, env.deps, core.ResourceModelConnection, "alpha"),
		"撤回后不得重建发布版本")
	pointer, err := env.deps.Resources.Pointer(ctx, core.ResourceModelConnection, "alpha")
	require.NoError(t, err)
	require.NotNil(t, pointer.DeletedAt, "撤回标记必须保留")
	require.Contains(t, env.logs.String(), "已撤回")
	require.Len(t, baselineOperations(t, env.deps), 1, "没有活可做时不再写操作")
}

// TestRegistryEntries_SkillWithoutPublishedPointerIsNotRegistered 覆盖 F-03 A 的启动路径
// （约束 36 / FR-042）：基线建好 v1 后撤回技能指针 → 注册装配把绑定该技能的 Agent 降级
// 为未注册并点名 WARN；没有技能绑定的 Agent 不受影响。逆改点：wire 不调用
// registryEntriesWithSkillPublication → 第一条 Registered 断言红。
// After the skill pointer is withdrawn, the startup filter demotes the bound agent and
// leaves agents without skill bindings untouched.
func TestRegistryEntries_SkillWithoutPublishedPointerIsNotRegistered(t *testing.T) {
	env := newBaselineEnv(t)
	ctx := context.Background()
	require.NoError(t, runBaseline(ctx, env.deps))

	read := openRead(t, env.dbPath)
	_, err := read.ExecContext(ctx,
		`UPDATE resource_pointers SET deleted_at = ? WHERE resource_kind = ? AND resource_id = ?`,
		baselineClock().UnixMilli(), string(core.ResourceSkill), "weekly")
	require.NoError(t, err)

	entries := []core.AgentEntry{
		{Name: "writer", Registered: true, Profile: &core.Profile{Name: "writer", Skills: []string{"weekly"}}},
		{Name: "plain", Registered: true},
	}
	adjusted, err := registryEntriesWithSkillPublication(ctx, env.deps.Resources, entries, env.deps.Logger)
	require.NoError(t, err)
	require.Len(t, adjusted, 2)
	require.False(t, adjusted[0].Registered, "技能指针已撤回 → 不注册")
	require.Equal(t, "skill version missing: weekly", adjusted[0].RejectReason)
	require.True(t, adjusted[1].Registered, "无技能绑定的 Agent 不受影响")
	require.Contains(t, env.logs.String(), "skill version missing: weekly")
}

// TestRegistryEntries_PointerLookupFailureRejectsStartup 覆盖 F-03 A 的失败关闭：存储
// 查询失败 → 启动装配上抛错误、不返回任何条目——**不得**把"读不到"默认成"都正常"。
// 逆改点：查询失败返回原 entries → 本用例 Error 断言红。
func TestRegistryEntries_PointerLookupFailureRejectsStartup(t *testing.T) {
	env := newBaselineEnv(t)
	adjusted, err := registryEntriesWithSkillPublication(context.Background(),
		fakeSkillPointers{err: errors.New("db unavailable")},
		[]core.AgentEntry{{Name: "writer", Registered: true}}, env.deps.Logger)
	require.Error(t, err)
	require.Nil(t, adjusted)
}

// TestRunBaseline_DoesNotAutoPublishChangedDefinition：工作区定义变了 → 只 WARN"待发布"，
// 不自动升版（注 9：基线的职责是补缺，不是发布）。
func TestRunBaseline_DoesNotAutoPublishChangedDefinition(t *testing.T) {
	env := newBaselineEnv(t)
	ctx := context.Background()
	require.NoError(t, runBaseline(ctx, env.deps))

	before, err := env.deps.Resources.Pointer(ctx, core.ResourceModelConnection, "alpha")
	require.NoError(t, err)

	env.deps.Providers[0].Content = []byte("name: alpha\nbase_url: http://127.0.0.1:2\napi_key: ${BASALT_ALPHA_KEY}\ndefault_model: alpha-v2\n")
	require.NoError(t, runBaseline(ctx, env.deps))

	require.Equal(t, 1, countResourceVersions(t, env.deps, core.ResourceModelConnection, "alpha"),
		"内容变化不自动升版")
	after, err := env.deps.Resources.Pointer(ctx, core.ResourceModelConnection, "alpha")
	require.NoError(t, err)
	require.Equal(t, before.PublishedVersionID, after.PublishedVersionID)
	require.Contains(t, env.logs.String(), "待发布")
}

// TestRunBaseline_CatchesUpAppliedPointerForMCP：MCP 已发布但 applied 为空（首版待重启 /
// 发布中断）→ 重跑把同一版本补成已生效，不建新版本。
func TestRunBaseline_CatchesUpAppliedPointerForMCP(t *testing.T) {
	env := newBaselineEnv(t)
	ctx := context.Background()
	require.NoError(t, runBaseline(ctx, env.deps))
	firstID := baselineOperations(t, env.deps)[0].OperationID

	read := openRead(t, env.dbPath)
	_, err := read.ExecContext(ctx,
		`UPDATE resource_pointers SET applied_version_id = NULL WHERE resource_kind = ? AND resource_id = ?`,
		string(core.ResourceMCPServer), "web")
	require.NoError(t, err)

	require.NoError(t, runBaseline(ctx, env.deps))

	pointer, err := env.deps.Resources.Pointer(ctx, core.ResourceMCPServer, "web")
	require.NoError(t, err)
	require.NotNil(t, pointer.AppliedVersionID, "重跑必须补齐 applied")
	require.Equal(t, pointer.PublishedVersionID, *pointer.AppliedVersionID)
	require.Equal(t, 1, countResourceVersions(t, env.deps, core.ResourceMCPServer, "web"),
		"补 applied 不建新版本")

	operations := baselineOperations(t, env.deps)
	require.Len(t, operations, 2)
	// 两次操作同一固定时钟，按标识区分，不靠行序。
	var catchUp *core.OperationRecord
	for i := range operations {
		if operations[i].OperationID != firstID {
			catchUp = &operations[i]
		}
	}
	require.NotNil(t, catchUp)
	var counts map[string]int
	require.NoError(t, json.Unmarshal(catchUp.DetailJSON, &counts))
	require.Equal(t, 1, counts["mcp"], "计数是本次处理的条数")
}

// TestRunBaseline_RepairsMissingSkillSnapshot：已发布版本的不可变快照缺失 → 按该版本重建，
// 版本号、哈希与指针一律不动。
func TestRunBaseline_RepairsMissingSkillSnapshot(t *testing.T) {
	env := newBaselineEnv(t)
	ctx := context.Background()
	require.NoError(t, runBaseline(ctx, env.deps))

	before, err := env.deps.Resources.Pointer(ctx, core.ResourceSkill, "weekly")
	require.NoError(t, err)

	versions, err := env.deps.Resources.Versions(ctx, core.ResourceSkill, "weekly")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	// 按登记行解析目录：修复必须在**原目录**重建，不迁移、不改写 storage_path。
	versionRel, err := workspace.PublishedVersionRelDir(versions[0].StoragePath)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Join(env.root, filepath.FromSlash(versionRel))))
	require.NoError(t, runBaseline(ctx, env.deps))

	require.FileExists(t, filepath.Join(env.root, filepath.FromSlash(versionRel), "SKILL.md"))
	require.Equal(t, 1, countResourceVersions(t, env.deps, core.ResourceSkill, "weekly"))
	after, err := env.deps.Resources.Pointer(ctx, core.ResourceSkill, "weekly")
	require.NoError(t, err)
	require.Equal(t, before.PublishedVersionID, after.PublishedVersionID)
}

// TestRunBaseline_RefusesBeginOperationFailure：预写失败 → 基线一项也不做（宪法 V 的
// fail-closed），零版本、零指针。
func TestRunBaseline_RefusesBeginOperationFailure(t *testing.T) {
	env := newBaselineEnv(t)
	env.deps.Operations = failingBeginStore{
		OperationStore: env.deps.Operations,
		err:            errBeginRefused,
	}

	err := runBaseline(context.Background(), env.deps)
	require.ErrorContains(t, err, "拒绝执行")

	read := openRead(t, env.dbPath)
	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM resource_versions`))
	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM resource_pointers`))
}

// errBeginRefused 是注入的预写失败：仓储不可用即可让基线拒绝执行。
var errBeginRefused = errors.New("management_operations 不可写")

// failingBeginStore 只改 `BeginOperation` 的行为，其余方法委托给真实仓储。
type failingBeginStore struct {
	core.OperationStore
	err error
}

func (s failingBeginStore) BeginOperation(context.Context, core.OperationRecord) error {
	return s.err
}

// TestRunBaseline_ApplyFailureFinishesOperationFailed：预写成功但补齐中途失败 → 操作以
// failed / INTERNAL 收口，失败原文落库；残缺资源不得留下版本或指针。
func TestRunBaseline_ApplyFailureFinishesOperationFailed(t *testing.T) {
	env := newBaselineEnv(t)
	env.deps.Providers = append(env.deps.Providers, ProviderDefinition{Name: "", Content: []byte("name: \n")})

	err := runBaseline(context.Background(), env.deps)
	require.ErrorContains(t, err, "建首版")

	operations := baselineOperations(t, env.deps)
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationFailed, operations[0].Status)
	require.Equal(t, baselineResultInternal, operations[0].ResultCode)
	require.NotEmpty(t, operations[0].ErrorMessage)
	require.Zero(t, countResourceVersions(t, env.deps, core.ResourceModelConnection, ""),
		"建版失败不得留下半截版本行")
}

// TestRunBaseline_SkipsNonEntitySkillDirectory：缺 SKILL.md 的目录不是技能实体（约束 36）→
// WARN 跳过，不建出版本，也不影响整次基线成功。
func TestRunBaseline_SkipsNonEntitySkillDirectory(t *testing.T) {
	env := newBaselineEnv(t)
	require.NoError(t, runBaseline(context.Background(), env.deps))

	require.Zero(t, countResourceVersions(t, env.deps, core.ResourceSkill, "draft"))
	require.Contains(t, env.logs.String(), "缺少 SKILL.md")
	require.Equal(t, core.OperationSuccess, baselineOperations(t, env.deps)[0].Status)
}
