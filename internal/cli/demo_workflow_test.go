package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/builtin"
	"github.com/ymedword/basalt/internal/workspace"
)

func TestFlow_DefaultWorkflowCLIInstallsWithoutRuntime(t *testing.T) {
	dir := initDemoWorkspace(t)
	deps := Deps{WorkDir: dir, AcquireWorkspaceLock: func(root string) (func() error, error) {
		lock, err := workspace.AcquireInstanceLock(root)
		if err != nil {
			return nil, err
		}
		return lock.Release, nil
	}}
	cmd := NewRootCommand(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"demo", "workflow", "install"})
	require.NoError(t, cmd.Execute())
	require.Contains(t, out.String(), "安装没有调用模型")
	require.FileExists(t, filepath.Join(dir, workspaceDirName, "samples", "bid-readiness", "workflow.json"))
	prepared, err := builtin.WorkflowPrepared(filepath.Join(dir, workspaceDirName))
	require.NoError(t, err)
	require.True(t, prepared, "CLI completion must prevent automatic resurrection before first startup")
	cmd = NewRootCommand(Deps{WorkDir: t.TempDir()})
	cmd.SetArgs([]string{"demo", "workflow", "install"})
	require.ErrorContains(t, cmd.Execute(), "basalt init")
	lock, err := workspace.AcquireInstanceLock(filepath.Join(dir, workspaceDirName))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })
	cmd = NewRootCommand(deps)
	cmd.SetArgs([]string{"demo", "workflow", "install"})
	require.Error(t, cmd.Execute(), "live instance must exclude offline install")
	for _, acquire := range []func(string) (func() error, error){nil, func(string) (func() error, error) { return nil, nil }} {
		fresh := initDemoWorkspace(t)
		cmd = NewRootCommand(Deps{WorkDir: fresh, AcquireWorkspaceLock: acquire})
		cmd.SetArgs([]string{"demo", "workflow", "install"})
		require.ErrorContains(t, cmd.Execute(), "工作区锁端口")
		require.NoDirExists(t, filepath.Join(fresh, workspaceDirName, "samples", "bid-readiness"))
	}
	released := false
	cmd = NewRootCommand(Deps{WorkDir: initDemoWorkspace(t), AcquireWorkspaceLock: func(string) (func() error, error) {
		return func() error { released = true; return errors.New("release failure") }, nil
	}})
	cmd.SetArgs([]string{"demo", "workflow", "install"})
	require.ErrorContains(t, cmd.Execute(), "release failure")
	require.True(t, released)
}
