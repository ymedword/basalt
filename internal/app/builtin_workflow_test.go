package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/builtin"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

func TestFlow_DefaultWorkflowCLILockIsWired(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Cleanup(func() { tool.SetNetGuard(nil) })
	root := filepath.Join(dir, ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)
	lock, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })
	require.Equal(t, 1, Run(context.Background(), []string{"demo", "workflow", "install"}))
	require.NoDirExists(t, filepath.Join(root, "samples", "bid-readiness"))
	require.NoError(t, lock.Release())
	require.Equal(t, 0, Run(context.Background(), []string{"demo", "workflow", "install"}))
	require.FileExists(t, filepath.Join(root, "samples", "bid-readiness", "workflow.json"))
	lock, err = workspace.AcquireInstanceLock(root)
	require.NoError(t, err, "CLI must release its instance lock")
}

func TestFlow_DefaultWorkflowStartupOnceAndLegacySkipped(t *testing.T) {
	for _, scenario := range []string{"fresh", "managed-upgrade", "legacy", "skipped", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			state := builtinOpenBoxState{SchemaVersion: 1, AgentsInstalled: true}
			switch scenario {
			case "managed-upgrade":
				require.NoError(t, writeBuiltinOpenBoxState(dir, state))
			case "skipped":
				state.AgentsInstalled, state.AgentsSkipped = false, true
				require.NoError(t, writeBuiltinOpenBoxState(dir, state))
			case "corrupt":
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "control"), 0o750))
				require.NoError(t, os.WriteFile(builtinOpenBoxStatePath(dir), []byte("bad"), 0o600))
			}
			deps := builtinTestDeps(dir, "", scenario == "fresh", nil)
			report, err := prepareBuiltinOpenBox(context.Background(), deps)
			require.NoError(t, err)
			if scenario != "fresh" && scenario != "managed-upgrade" {
				require.Empty(t, report.WorkflowAgents)
				require.NoDirExists(t, filepath.Join(dir, "agents", "bid-reviewer"))
				return
			}
			require.Len(t, report.WorkflowAgents, 4)
			require.True(t, readBuiltinTestMarker(t, dir).WorkflowPrepared)
			require.NoError(t, os.RemoveAll(filepath.Join(dir, "agents", "bid-reviewer")))
			deps.Fresh = false
			report, err = prepareBuiltinOpenBox(context.Background(), deps)
			require.NoError(t, err)
			require.Empty(t, report.WorkflowAgents)
			require.NoDirExists(t, filepath.Join(dir, "agents", "bid-reviewer"))
		})
	}
}

func TestFlow_DefaultWorkflowExplicitInstallDoesNotResurrect(t *testing.T) {
	for _, scenario := range []string{"fresh", "managed", "corrupt-receipt"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			if scenario != "fresh" {
				require.NoError(t, writeBuiltinOpenBoxState(dir, builtinOpenBoxState{SchemaVersion: 1, AgentsInstalled: true}))
			}
			// CLI 在实例锁内调用同一安装器；删除发生于首次基线发布之前，无生命周期记录。
			// The CLI uses this installer before any lifecycle identity has been published.
			_, err := builtin.InstallWorkflow(dir, nil)
			require.NoError(t, err)
			require.NoError(t, os.RemoveAll(filepath.Join(dir, "agents", "bid-reviewer")))
			if scenario == "corrupt-receipt" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "control", "workflow-defaults.json"), []byte("broken"), 0o600))
			}
			deps := builtinTestDeps(dir, "", scenario == "fresh", nil)
			report, err := prepareBuiltinOpenBox(context.Background(), deps)
			require.NoError(t, err)
			require.Empty(t, report.WorkflowAgents)
			require.NoDirExists(t, filepath.Join(dir, "agents", "bid-reviewer"))
			require.True(t, readBuiltinTestMarker(t, dir).WorkflowPrepared)
		})
	}
}
