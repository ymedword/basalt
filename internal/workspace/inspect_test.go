package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// TestInspect_ReadyAfterInit：init 之后清点为空清单——"能用了"的判据是 Missing
// 为空（齐备时不列条目，见 Report 注释）。
func TestInspect_ReadyAfterInit(t *testing.T) {
	ws := newWorkspaceRoot(t)
	require.NoError(t, initErr(ws))

	report, err := workspace.Inspect(ws)
	require.NoError(t, err)

	require.Equal(t, ws, report.Root)
	require.Empty(t, report.Missing, "刚 init 完的工作区不得报缺失")
}

// TestInspect_Uninitialized：未初始化报 core.ErrWorkspaceNotInitialized（"没跑过
// init"的判读），且**只读**——清点不得顺手把目录建出来。
func TestInspect_Uninitialized(t *testing.T) {
	ws := newWorkspaceRoot(t)

	report, err := workspace.Inspect(ws)
	require.ErrorIs(t, err, core.ErrWorkspaceNotInitialized)
	require.Equal(t, ws, report.Root, "报错路径同样回显被清点的根路径")
	require.Empty(t, report.Missing)

	_, statErr := os.Stat(ws)
	require.ErrorIs(t, statErr, os.ErrNotExist, "清点不得创建目录")
}

// TestInspect_ReportsMissingEntries：缺项逐个点名，顺序 = 目录清单后文件清单
// （清单顺序即报告顺序——同一份残缺工作区两次清点必须给出同一份清单）。
func TestInspect_ReportsMissingEntries(t *testing.T) {
	ws := newWorkspaceRoot(t)
	require.NoError(t, initErr(ws))

	require.NoError(t, os.RemoveAll(filepath.Join(ws, "logs")))
	require.NoError(t, os.Remove(filepath.Join(ws, "USER.md")))

	report, err := workspace.Inspect(ws)
	require.NoError(t, err)
	require.Equal(t, []string{"logs", "USER.md"}, report.Missing)
}

// TestInspect_DetectsTypeMismatch：类型不符同样算缺失——"该是目录却是文件"的
// 工作区不可用，只查存在性会把这种残缺判成齐备。
func TestInspect_DetectsTypeMismatch(t *testing.T) {
	ws := newWorkspaceRoot(t)
	require.NoError(t, initErr(ws))

	// 目录位换成文件、文件位换成目录（各一处，两个方向都覆盖）。
	require.NoError(t, os.RemoveAll(filepath.Join(ws, "sessions")))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "sessions"), []byte("not a directory"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(ws, "SOUL.md")))
	require.NoError(t, os.Mkdir(filepath.Join(ws, "SOUL.md"), 0o750))

	report, err := workspace.Inspect(ws)
	require.NoError(t, err)
	require.Equal(t, []string{"sessions", "SOUL.md"}, report.Missing)
}

// TestInspect_RootNotADirectory：根路径不是目录（普通文件占位）时不冒充"未初始化"
// ——两者对运维是不同动作（前者要查路径是不是写错了）。
func TestInspect_RootNotADirectory(t *testing.T) {
	ws := filepath.Join(t.TempDir(), ".basalt")
	require.NoError(t, os.WriteFile(ws, []byte("occupied"), 0o644))

	report, err := workspace.Inspect(ws)
	require.Error(t, err)
	require.NotErrorIs(t, err, core.ErrWorkspaceNotInitialized)
	require.Equal(t, ws, report.Root)
}
