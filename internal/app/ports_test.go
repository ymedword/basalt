package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/testutil"
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
