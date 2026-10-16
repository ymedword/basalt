package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/testutil"
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
	require.Len(t, tables, 11, "11 张表随 schema.sql 建齐")
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
		WorkDir: workDir,
		Logger:  slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
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
