package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/testutil"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// 本文件的被测面是装配层的**适配器**（ports.go）：它们是"实现包 ↔ 命令行端口"之间
// 的唯一接缝。`TestRun_ExitCodes` 走 `--version` 与未知命令，到不了这条路径——
// 接缝本身（错误转发、typed-nil 判空、报告类型收敛）必须直接判卷。
// The adapters are the only seam between the implementation packages and the
// command surface; TestRun_ExitCodes never reaches them, so they are judged here.

// TestStorageStore_BootstrapThenOpenReadOnly 端口往返：引导创建 → 二跑报 Kept →
// 只读打开读到版本与表清点（status 节 3 消费的就是这三个方法）。
func TestStorageStore_BootstrapThenOpenReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testutil.TempWorkspace(t), "basalt.db")

	store := storageStore{}

	created, err := store.Bootstrap(ctx, path, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	require.True(t, created, "首次引导必须报告实际创建")

	created, err = store.Bootstrap(ctx, path, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	require.False(t, created, "二跑必须报 Kept（created=false）")

	view, err := store.OpenReadOnly(ctx, path, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, view.Close()) })

	version, err := view.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, storage.SupportedSchemaVersion, version)

	tables, err := view.TableNames(ctx)
	require.NoError(t, err)
	require.Len(t, tables, 29, "29 张表随 schema.sql（v5）建齐——v1 11 张 + US-11 新增 7 张 + US-12 新增 10 张 + v5 的 network_domain_policy")
}

// TestStorageStore_OpenReadOnlyErrorReturnsNilView typed-nil 回归：错误路径**必须**
// 返回真正的 nil 接口——`*storage.DB` 装箱进接口会把 nil 变成"非空接口 + 空指针"，
// 上层的 `err != nil` 判据随之失效。
//
// 判据刻意用 `view == nil` 而不是 require.Nil：后者走 reflect 判空，对 typed-nil
// **同样通过**，判不出这个缺陷。
// The assertion is `view == nil` on purpose: require.Nil passes for a typed nil.
func TestStorageStore_OpenReadOnlyErrorReturnsNilView(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testutil.TempWorkspace(t), "missing.db")

	view, err := storageStore{}.OpenReadOnly(ctx, path, storage.SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrDatabaseMissing)
	require.True(t, view == nil, "错误路径必须返回 nil 接口（typed-nil 陷阱）；got %#v", view)
}

// TestWorkspacePort_InitAndInspect 端口往返：创建 → 二跑 Kept → 清点无缺失。
// 报告类型在这里被收成裸切片（端口形态），两条路径都要判。
func TestWorkspacePort_InitAndInspect(t *testing.T) {
	root := filepath.Join(testutil.TempWorkspace(t), ".basalt")
	port := workspacePort{}

	created, kept, err := port.Init(root)
	require.NoError(t, err)
	require.NotEmpty(t, created, "首跑必须报告新建条目")
	require.Empty(t, kept, "首跑没有「已存在」条目")

	created, kept, err = port.Init(root)
	require.NoError(t, err)
	require.Empty(t, created, "二跑不得报告新建")
	require.NotEmpty(t, kept, "二跑必须报告保留条目")

	missing, err := port.Inspect(root)
	require.NoError(t, err)
	require.Empty(t, missing, "齐备的工作区无缺失项")
}

// TestWorkspacePort_InspectUninitialized 错误转发：未初始化 → core 哨兵错误
// 原样上抛（端口不得吞错，也不得自造错误）。
func TestWorkspacePort_InspectUninitialized(t *testing.T) {
	root := filepath.Join(testutil.TempWorkspace(t), ".basalt")

	missing, err := workspacePort{}.Inspect(root)
	require.ErrorIs(t, err, core.ErrWorkspaceNotInitialized)
	require.Nil(t, missing, "错误路径不得顺手给出「缺全部」式的清单")
}

// TestWorkspacePort_InitErrorForwarded 错误转发：父路径被普通文件占住 → 建目录
// 失败，端口原样上抛（不得吞、不得返回半份清单）。
func TestWorkspacePort_InitErrorForwarded(t *testing.T) {
	base := testutil.TempWorkspace(t)
	blocker := filepath.Join(base, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0o600))

	created, kept, err := workspacePort{}.Init(filepath.Join(blocker, ".basalt"))
	require.Error(t, err, "父路径是文件时必须失败")
	require.Nil(t, created)
	require.Nil(t, kept)
}

// writeAgentDir 在给定工作区根下造一个 Agent 目录（内容由调用方给定）。
// writeAgentDir lays down one agent directory under the workspace root.
func writeAgentDir(t *testing.T, root, name, content string) {
	t.Helper()

	dir := filepath.Join(root, "agents", name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(content), 0o644))
}

// TestAgentsPort_ListForwardsLoaderEntries list 的转调：装载器的条目**原样**交回端口
// （注册与拒绝**都有一条**——单只失败不静默消失），装载期的点名 WARN 走**注入的**日志器
// （配置档位）而不是进程默认档——这正是适配器持有 Logger 的理由。
// Acceptance point: List forwards every entry (registered or rejected) and the
// loader's per-agent WARN lines land on the injected logger.
func TestAgentsPort_ListForwardsLoaderEntries(t *testing.T) {
	workDir := testutil.TempWorkspace(t)
	root := filepath.Join(workDir, ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)

	writeAgentDir(t, root, "compat-check", `---
name: compat-check
provider:
  name: deepseek
  model: deepseek-chat
---

段一素材。
`)
	writeAgentDir(t, root, "mismatch-agent", `---
name: other-name
provider:
  name: deepseek
  model: deepseek-chat
---

目录名与 name 不一致——必须整只不注册。
`)

	var logs bytes.Buffer
	port := agentsPort{WorkDir: workDir, Logger: slog.New(slog.NewTextHandler(&logs, nil))}

	entries, err := port.List(root, []string{"deepseek"})
	require.NoError(t, err)
	require.Len(t, entries, 2, "每个 Agent 目录一条：注册与拒绝都交回端口")

	byName := make(map[string]core.AgentEntry, len(entries))
	for _, entry := range entries {
		byName[entry.Name] = entry
	}

	registered := byName["compat-check"]
	require.True(t, registered.Registered, "应注册：%s", registered.RejectReason)
	require.Equal(t, "deepseek", registered.Profile.Provider.Name)

	rejected := byName["mismatch-agent"]
	require.False(t, rejected.Registered)
	require.NotEmpty(t, rejected.RejectReason, "未注册必须带原因（不得静默消失）")

	// 注入的日志器收到点名 WARN：固定三键 agent / reason / detail 都在。
	require.Contains(t, logs.String(), "agent=mismatch-agent")
	require.Contains(t, logs.String(), "reason=")
	require.Contains(t, logs.String(), "detail=")
}

// TestAgentsPort_CreateAndDeleteReachWorkspace 写模式两方法的转调：create 落盘后经**同一
// 端口** list 即为已注册（模板可装载是 create 的判据）；delete 用**注入的固定时刻**归档，
// 源目录移出、归档副本在；两条路径的错误（已存在 / 缺目录）原样上抛；写模式调用各留一条
// DEBUG 排障线（工作目录 + 工作区根）。
// Acceptance point: both write-mode methods reach the workspace, and the injected
// instant is what lands in the archive name.
func TestAgentsPort_CreateAndDeleteReachWorkspace(t *testing.T) {
	workDir := testutil.TempWorkspace(t)
	root := filepath.Join(workDir, ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)

	// 排障线是 DEBUG 档（prod 默认关闭），这里的 handler 必须显式开 Debug 才看得见。
	var logs bytes.Buffer
	port := agentsPort{
		WorkDir:                workDir,
		SupportedSchemaVersion: storage.SupportedSchemaVersion,
		Logger:                 slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	created, err := port.Create(root, "demo-agent", []string{"deepseek"})
	require.NoError(t, err)
	require.Equal(t, []string{"agents/demo-agent/AGENT.md"}, created)

	entries, err := port.List(root, []string{"deepseek"})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "刚写的模板应当可装载：%s", entries[0].RejectReason)

	_, err = port.Create(root, "demo-agent", []string{"deepseek"})
	require.Error(t, err, "已存在的目录必须拒绝")
	require.Contains(t, err.Error(), "demo-agent", "错误点名被拒目录")

	now := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	archived, err := port.Delete(root, "demo-agent", now)
	require.NoError(t, err)
	require.Equal(t, "archive/demo-agent-20261016-093000", archived, "注入的时刻原样进入归档名")
	require.NoDirExists(t, filepath.Join(root, "agents", "demo-agent"))
	require.FileExists(t, filepath.Join(root, "archive", "demo-agent-20261016-093000", "AGENT.md"))

	_, err = port.Delete(root, "demo-agent", now)
	require.Error(t, err, "缺目录必须失败")
	require.Contains(t, err.Error(), "demo-agent")

	require.Contains(t, logs.String(), "level=DEBUG", "写模式调用留 DEBUG 排障线")
	require.Contains(t, logs.String(), "workdir=")
}

// —— US-12 追加四条：CLI 写路径的完整生命周期（约束 27 / 32 / 49）——
// Four added by US-12: the CLI write path owns lock, database, operation row and
// archive row. (卡 §3.2.2 把「写真实操作行 / 归档行」归在 cli 行；`internal/cli`
// 不得 import `internal/app`（用户裁决）且 ports 类型未导出，因此两个真实行锚点
// 落在端口层测试文件，命令层判「调没调端口」。裁决记录见 run-state §1.33。)

// newCLIPortForTest 造本文件四个 CLI 写路径用例共用的端口与工作区骨架。
// newCLIPortForTest builds the shared port and workspace skeleton for the CLI-path tests.
func newCLIPortForTest(t *testing.T) (agentsPort, string, *bytes.Buffer) {
	t.Helper()

	workDir := testutil.TempWorkspace(t)
	root := filepath.Join(workDir, ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)

	var logs bytes.Buffer
	port := agentsPort{
		WorkDir:                workDir,
		SupportedSchemaVersion: storage.SupportedSchemaVersion,
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
	}
	return port, root, &logs
}

// cliProfileAdminForTest 在端口释放实例锁之后按同一装配口径重建管理面：`Archived` 与
// `Restore` 因此走的是 HTTP 端点背后的同一实现，不是测试自造的第二条路径。
// cliProfileAdminForTest rebuilds the same management surface the HTTP handlers use.
func cliProfileAdminForTest(t *testing.T, root string, logger *slog.Logger) *AgentAdmin {
	t.Helper()

	db, err := storage.OpenWritable(context.Background(),
		filepath.Join(root, databaseFileName), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	return NewAgentAdmin(AgentAdminDeps{
		Root:       root,
		Lifecycle:  workspace.NewLifecycleStore(root, time.Now),
		Index:      storage.NewResourceStore(db),
		Operations: storage.NewManagementStore(db),
		Archives:   storage.NewAgentAdminStore(db),
		Logger:     logger,
		Now:        time.Now,
	})
}

// TestCLIProfileCreate_WritesManagementOperation（约束 27 / 49；卡 §3.2.2 的
// `TestCLIProfileCreate_WritesManagementOperation`）：`profile create` 经端口写**真实**
// 管理操作行——pending → success、动作 / 目标 / 主体 / 认证方式逐字；状态文件由
// `Bootstrap` 落成 disabled / generation=1，索引行与操作行同源；模板 provider 取命令层
// 传入列表的首项（签名里的 providers 不是死参数）。
// Acceptance point: a CLI create leaves a real success operation row, a bootstrap state
// file and an index row, and the template honours the passed provider order.
func TestCLIProfileCreate_WritesManagementOperation(t *testing.T) {
	ctx := context.Background()
	port, root, _ := newCLIPortForTest(t)

	created, err := port.Create(root, "cli-agent", []string{"deepseek", "spare"})
	require.NoError(t, err)
	require.Equal(t, []string{"agents/cli-agent/AGENT.md"}, created)

	// 状态文件：默认停用、generation=1、operation_id 指向本次操作（约束 27）。
	rec, exists, err := workspace.NewLifecycleStore(root, time.Now).Read(ctx, "cli-agent")
	require.NoError(t, err)
	require.True(t, exists, "创建必须留下生命周期状态文件")
	require.Equal(t, core.AgentDisabled, rec.State, "创建默认停用")
	require.Equal(t, int64(1), rec.Generation)
	require.Regexp(t, `^op-\d{13}-[a-z0-9]{6}$`, rec.OperationID)

	// 模板取命令层列表首项（约束 28）：不是目录遍历，也不是库内快照。
	content, err := os.ReadFile(filepath.Join(root, "agents", "cli-agent", "AGENT.md"))
	require.NoError(t, err)
	require.Contains(t, string(content), "name: deepseek", "模板 provider 取配置列表首项")

	// 操作行：pending → success，动作 / 目标 / 主体 / 认证方式逐字（契约 §9.3）。
	db := openRead(t, filepath.Join(root, databaseFileName))
	var action, targetKind, targetID, subject, method, source, status, resultCode string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT action, target_kind, target_id, actor_subject, auth_method, source, status, result_code
		   FROM management_operations WHERE operation_id = ?`, rec.OperationID).
		Scan(&action, &targetKind, &targetID, &subject, &method, &source, &status, &resultCode))
	require.Equal(t, core.ActionAgentCreate, action)
	require.Equal(t, targetKindAgent, targetKind)
	require.Equal(t, "cli-agent", targetID)
	require.True(t, strings.HasPrefix(subject, core.ActorSubjectCLIPrefix),
		"actor_subject 必须是 cli:<本机用户>：%s", subject)
	require.NotEqual(t, core.ActorSubjectCLIPrefix, subject, "用户名不得为空")
	require.Equal(t, core.AuthMethodCLI, method)
	require.Equal(t, cliOperationSource, source)
	require.Equal(t, string(core.OperationSuccess), status)
	require.Equal(t, lifecycleResultOK, resultCode)

	// 生命周期索引落库（可重建视图；失败不回滚写，但正常路径必须写成功）。
	var indexOP string
	var valid int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT operation_id, valid FROM agent_lifecycle WHERE agent_name = ?`, "cli-agent").
		Scan(&indexOP, &valid))
	require.Equal(t, rec.OperationID, indexOP)
	require.Equal(t, 1, valid)

	require.Equal(t, 1, countRows(t, db,
		`SELECT COUNT(*) FROM management_operations WHERE target_id = ?`, "cli-agent"),
		"一次创建只写一行操作记录")
}

// TestCLIProfileDelete_WritesArchiveRow（约束 32 / 49；卡 §3.2.2 的
// `TestCLIProfileDelete_WritesArchiveRow`）：`profile delete` 走归档编排——目录搬入
// 归档区、归档行与操作终态同事务写入、`/archived-agents` 可见、原名恢复可回活动区并
// 标记 restored_at；注入时刻同时进入归档标识与操作行。
// Acceptance point: a CLI delete writes a restorable archive row and moves the
// directory; the injected instant drives the archive id.
func TestCLIProfileDelete_WritesArchiveRow(t *testing.T) {
	ctx := context.Background()
	port, root, _ := newCLIPortForTest(t)

	_, err := port.Create(root, "cli-agent", []string{"deepseek"})
	require.NoError(t, err)

	now := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	archived, err := port.Delete(root, "cli-agent", now)
	require.NoError(t, err)
	require.Equal(t, "archive/cli-agent-20261016-093000", archived)
	require.NoDirExists(t, filepath.Join(root, "agents", "cli-agent"))
	require.FileExists(t, filepath.Join(root, "archive", "cli-agent-20261016-093000", "AGENT.md"))

	dbPath := filepath.Join(root, databaseFileName)
	db := openRead(t, dbPath)

	var operationID, agentName, contentHash, manifest string
	var archivedAt int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT operation_id, agent_name, archived_at, content_hash, manifest_json
		   FROM agent_archives WHERE archive_id = ?`, "cli-agent-20261016-093000").
		Scan(&operationID, &agentName, &archivedAt, &contentHash, &manifest))
	require.Equal(t, "cli-agent", agentName)
	require.Equal(t, now.UnixMilli(), archivedAt, "归档时间以注入时刻为准")
	require.NotEmpty(t, contentHash, "归档清单哈希可复算")
	require.NotEmpty(t, manifest)

	var status, subject, method, source string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT status, actor_subject, auth_method, source
		   FROM management_operations WHERE operation_id = ?`, operationID).
		Scan(&status, &subject, &method, &source))
	require.Equal(t, string(core.OperationSuccess), status)
	require.True(t, strings.HasPrefix(subject, core.ActorSubjectCLIPrefix), "actor_subject：%s", subject)
	require.Equal(t, core.AuthMethodCLI, method)
	require.Equal(t, cliOperationSource, source)

	// `/archived-agents` 可见：与 HTTP 端点调用同一个方法（AgentAdmin.Archived）。
	admin := cliProfileAdminForTest(t, root, port.logger())
	rows, err := admin.Archived(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "cli-agent-20261016-093000", rows[0].ArchiveID)

	// 可恢复：原名恢复把目录搬回活动区并把归档行标为已恢复。
	view, err := admin.Restore(ctx, port.actor(), rows[0].ArchiveID, "")
	require.NoError(t, err)
	require.Equal(t, "cli-agent", view.Agent)
	require.FileExists(t, filepath.Join(root, "agents", "cli-agent", "AGENT.md"))
	// 恢复是"复制回活动区"：归档目录保留（物理清除与保留策略明确不在本模块范围）。
	require.DirExists(t, filepath.Join(root, "archive", "cli-agent-20261016-093000"),
		"归档副本保留，仅归档行标记 restored_at")
	require.Equal(t, 1, countRows(t, db,
		`SELECT COUNT(*) FROM agent_archives
		  WHERE archive_id = ? AND restored_at IS NOT NULL AND restored_agent_name = ?`,
		"cli-agent-20261016-093000", "cli-agent"))

	rec, exists, err := workspace.NewLifecycleStore(root, time.Now).Read(ctx, "cli-agent")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentDisabled, rec.State, "恢复默认停用")
	require.Equal(t, int64(3), rec.Generation, "创建 1 → 归档 2 → 恢复 3")
}

// TestArchivedList_IncludesCLIArchive（卡 §4.4 第 24 行 / §3.2.2）：`GET /archived-agents`
// 的数据源是 `web.AgentAdminService.Archived`；本用例经 T037 装配的 `agentAdminPort` 读——
// CLI `profile delete` 写下的归档行必须出现在这份列表里。CLI 与 HTTP 共写 `agent_archives`
// 一张表，管理台因此不需要第二条读路径（约束 27「CLI 归档在管理台可见」）。
// Acceptance point: the web archive port lists a row written by the CLI delete path.
func TestArchivedList_IncludesCLIArchive(t *testing.T) {
	ctx := context.Background()
	port, root, _ := newCLIPortForTest(t)

	_, err := port.Create(root, "cli-archive-agent", []string{"deepseek"})
	require.NoError(t, err)

	now := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	archived, err := port.Delete(root, "cli-archive-agent", now)
	require.NoError(t, err)
	require.Equal(t, "archive/cli-archive-agent-20261016-093000", archived)

	// 与 HTTP 端点背后是同一个实现：`agentAdminPort` 是 `web.AgentAdminService` 的唯一适配器。
	// The same implementation behind the HTTP endpoint: the sole web port adapter.
	admin := cliProfileAdminForTest(t, root, port.logger())
	webPort := agentAdminPort{admin: admin}
	rows, err := webPort.Archived(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "CLI 归档行必须出现在 /archived-agents 的数据源里")
	require.Equal(t, "cli-archive-agent-20261016-093000", rows[0].ArchiveID)
	require.Equal(t, "cli-archive-agent", rows[0].AgentName)
	require.Nil(t, rows[0].RestoredAt, "未恢复的归档行 restored_at 为空")
}

// TestAgentsPort_WritePathReleasesLockOnFailure（卡 §3.2.2 CLI 写路径行）：失败路径同样
// 关闭库、释放锁——同名再建的纯请求校验失败先于预写（零操作行），随后测试自己取锁成功、
// 库文件可改名（Windows 上句柄未关会被占用），再接一次归档仍成功。
// Acceptance point: a failed write closes the database and releases the lock, and a
// pre-write validation failure leaves no second operation row.
func TestAgentsPort_WritePathReleasesLockOnFailure(t *testing.T) {
	port, root, _ := newCLIPortForTest(t)

	_, err := port.Create(root, "locked-agent", nil)
	require.NoError(t, err)

	// 同名再建：锁与库都已取得，失败发生在目标空闲校验（FR1：纯请求校验不写操作行）。
	_, err = port.Create(root, "locked-agent", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "locked-agent")

	// 实例锁必须已释放：测试自己再取一次必须成功。
	lock, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err, "写路径失败后实例锁必须已释放")
	require.NoError(t, lock.Release())

	// 库句柄必须已关闭：Windows 上改名被打开的文件会失败；POSIX 下该操作恒成功，
	// 因而不作为 POSIX 平台的判据。
	dbPath := filepath.Join(root, databaseFileName)
	moved := dbPath + ".closed-probe"
	require.NoError(t, os.Rename(dbPath, moved), "写路径失败后库句柄必须已关闭")
	require.NoError(t, os.Rename(moved, dbPath))

	// 前一次失败没有留下第二套互斥或半截状态：归档照常走完。
	archived, err := port.Delete(root, "locked-agent", time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, "archive/locked-agent-20261016-093000", archived)

	db := openRead(t, dbPath)
	require.Equal(t, 1, countRows(t, db,
		`SELECT COUNT(*) FROM management_operations WHERE action = ? AND target_id = ?`,
		core.ActionAgentCreate, "locked-agent"),
		"同名再建的校验失败先于预写，只能有首次创建那一行")
}

// TestAgentsPort_LightPathSkipsWritePath（卡 §3.2.2 的「profile list / show 不进入写
// 路径」）：活实例持锁（= serve 正在运行）时 list 照常工作，且不取锁、不打开 / 创建库、
// 不写生命周期状态——零 SupportedSchemaVersion 也不影响读面。
// Acceptance point: the read path takes no lock and never opens the database or writes
// lifecycle state, even while another instance holds the lock.
func TestAgentsPort_LightPathSkipsWritePath(t *testing.T) {
	workDir := testutil.TempWorkspace(t)
	root := filepath.Join(workDir, ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)
	writeAgentDir(t, root, "reader", `---
name: reader
provider:
  name: deepseek
  model: deepseek-chat
---

只读浏览器。
`)

	lock, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Release()) }()

	port := agentsPort{WorkDir: workDir} // 刻意不设 schema 版本：读面不得依赖写路径配置

	entries, err := port.List(root, []string{"deepseek"})
	require.NoError(t, err, "读面不得取实例锁")
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "读面零写入：%s", entries[0].RejectReason)

	require.NoFileExists(t, filepath.Join(root, databaseFileName), "轻路径不得创建数据库")
	require.NoDirExists(t, filepath.Join(root, "control", "agents"), "轻路径不得写生命周期状态")
}

// TestLogLevel_MapsConfigValues 取值域已由 cli.LoadConfig 判卷，本用例只判**翻译**
// 本身：四档取值 + 未知值与空值回落 info（配置装载失败时日志出口仍要能工作）。
func TestLogLevel_MapsConfigValues(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  slog.Level
	}{
		{"debug", "debug", slog.LevelDebug},
		{"warn", "warn", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"info", "info", slog.LevelInfo},
		{"空值回落 info", "", slog.LevelInfo},
		{"未知值回落 info", "verbose", slog.LevelInfo},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, logLevel(tc.input))
		})
	}
}

// —— US-5 追加两条（工具清单端口与装配池六件）——
// Two added by US-5: the tool-listing adapter and the six-tool assembly pool.

// portsFakeTool 是注册池判据用的假工具：池只经 `core.Tool` 接口看它——名字、描述与 schema
// 各自可辨认，转调是否**逐字段**保真因此判得出（比对名字相等会漏掉描述 / schema 的漂移）。
// A fake tool whose three fields are individually recognisable, so the adapter's
// pass-through is judged field by field rather than by name alone.
type portsFakeTool struct{ name string }

// Name 是注册名。
func (t portsFakeTool) Name() string { return t.name }

// Description 是给模型的能力说明（含名字，便于逐条对号）。
func (t portsFakeTool) Description() string { return "假工具：" + t.name }

// InputSchema 是内联 Schema（转调保真的判据面之一）。
func (t portsFakeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`)
}

// Execute 是假执行体（本文件只判清单转调，不判执行）。
func (t portsFakeTool) Execute(context.Context, core.ToolEnv, json.RawMessage) (core.Result, error) {
	return core.Result{}, nil
}

// TestToolListerPort_ForwardsSpecsInRegistryOrder 端口转调：`Tools()` 就是
// `registry.Specs(registry.Names())`——顺序（排序稳定）与三个字段都同源，命令面因此
// **不得**重排也无法另取一份（渲染顺序的唯一来源）。
//
// 注册顺序刻意打乱：转调若按插入顺序或 map 遍历顺序导出，这里立刻分道扬镳。
func TestToolListerPort_ForwardsSpecsInRegistryOrder(t *testing.T) {
	registry := tool.NewRegistry(nil)
	for _, name := range []string{"zulu", "alpha", "mid"} {
		require.NoError(t, registry.Register(portsFakeTool{name: name}))
	}

	port := toolListerPort{registry: registry}
	specs := port.Tools()

	require.NotEmpty(t, specs, "池非空时清单不得为空")
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	require.Equal(t, []string{"alpha", "mid", "zulu"}, names, "顺序 = Names() 的排序稳定顺序")
	require.Equal(t, registry.Specs(registry.Names()), specs, "转调必须逐字段保真（描述与 schema 一并）")
}

// TestToolListerPort_HeavyAssemblyRegistersTenTools：重路径装配池**十件**——三件文件工具
// 加 `shell` / `http_get` / `http_post`（US-5）、`save_memory` / `recall_memory`（US-6/7 记忆对）、
// US-8 的 `notify` 与 US-10 的 `manage_schedule`，且新件确实可解析（`Resolve` 命中），
// 不再是"注册了名字、背后没有实体"的形态。（判据面变更 §3.5 #4：原名 `...NineTools`，
// 精确集 9 → 10，断言只收紧、不删除。）
//
// 名集是**恰好十件**而不是"至少包含"：装配池少一件（调度工具忘接）或多一件（误接线）
// 都是装配故障，集合比较让两种都当场落红。
// The pool is compared as an exact set: one tool short or one extra is an assembly
// fault either way, and only the set comparison catches both.
func TestToolListerPort_HeavyAssemblyRegistersTenTools(t *testing.T) {
	fx := newWireFixture(t)
	rt := fx.assemble(t)

	specs := rt.tools().Tools()
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	require.Equal(t, []string{
		"http_get", "http_post", "list_dir", "manage_schedule", "notify", "read_file",
		"recall_memory", "save_memory", "shell", "write_file",
	}, names, "装配池补到十件（三件文件工具 + shell / http_get / http_post + 两件记忆工具 + notify + manage_schedule）")

	for _, name := range []string{"shell", "http_get", "http_post", "save_memory", "recall_memory", "notify"} {
		resolved, ok := rt.registry.Resolve(name)
		require.True(t, ok, "工具必须在注册池内可解析：%s", name)
		require.Equal(t, name, resolved.Name())
		require.NotEmpty(t, resolved.Description(), "描述是给模型的能力说明")
		require.NotEmpty(t, resolved.InputSchema(), "schema 是执行侧判必填的唯一依据")
	}
}
