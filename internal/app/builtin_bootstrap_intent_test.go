package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestInstanceBootstrap_OpenBoxIntentPrecedesRoster 验证首启中断后仍由已有安装标记续做，不重定义fresh。
// The real bootstrap must preserve open-box intent before a retry becomes non-fresh.
func TestInstanceBootstrap_OpenBoxIntentPrecedesRoster(t *testing.T) {
	f := newProvisioningFixture(t)
	f.bootstrap(t)
	wsRoot := filepath.Join(f.workDir, workspaceDirName)
	state, present, corrupt, err := readBuiltinOpenBoxState(wsRoot)
	require.NoError(t, err)
	require.True(t, present, "initial open-box intent must precede the roster")
	require.False(t, corrupt)
	require.False(t, state.AgentsInstalled)
	require.False(t, state.AgentsSkipped)
	require.Empty(t, state.InstalledAt, "intent is not an actual installation timestamp")
	retry, err := ensureNewInstanceConfig(f.workDir, "")
	require.NoError(t, err)
	require.False(t, retry.Fresh, "retry must not become globally fresh again")
	report, err := prepareBuiltinOpenBox(context.Background(), builtinTestDeps(wsRoot, "", retry.Fresh, nil))
	require.NoError(t, err)
	require.False(t, report.Backfilled)
	require.Len(t, report.AgentsInstalled, 5)
	require.True(t, readBuiltinTestMarker(t, wsRoot).AgentsInstalled)
}

// TestBuiltinOpenBoxIntent_PreservesExisting 验证原字节权威覆盖已安装、跳过、损坏和未知版本。
// Exclusive publication never overwrites any existing marker, including unreadable future state.
func TestBuiltinOpenBoxIntent_PreservesExisting(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version":1,"agents_installed":true,"agents_skipped":false}`,
		`{"schema_version":1,"agents_installed":false,"agents_skipped":true}`,
		`{"schema_version":999,"agents_installed":false}`,
		`not-json`,
	} {
		t.Run(raw, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, builtinOpenBoxControlDir), 0o750))
			path := builtinOpenBoxStatePath(root)
			require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
			id, err := newInstanceID()
			require.NoError(t, err)
			require.NoError(t, recordBuiltinOpenBoxIntent(root, id))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, []byte(raw), after)
			entries, err := os.ReadDir(filepath.Dir(path))
			require.NoError(t, err)
			require.Len(t, entries, 1, "only the pre-existing final marker may remain")
		})
	}
}

// TestBuiltinOpenBoxIntent_ConcurrentPublication 验证并发首启只发布完整JSON并清理各自临时文件。
// Concurrent candidates publish one complete marker and remove only their own staging files.
func TestBuiltinOpenBoxIntent_ConcurrentPublication(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		id, err := newInstanceID()
		require.NoError(t, err)
		wg.Go(func() { errs <- recordBuiltinOpenBoxIntent(root, id) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	state, present, corrupt, err := readBuiltinOpenBoxState(root)
	require.NoError(t, err)
	require.True(t, present)
	require.False(t, corrupt)
	require.False(t, state.AgentsInstalled)
	require.False(t, state.AgentsSkipped)
	entries, err := os.ReadDir(filepath.Join(root, builtinOpenBoxControlDir))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

// TestInstanceBootstrap_IntentFailureDoesNotPublishRoster 验证安装意图写失败时名册仍未落盘。
// Failure to record intent must leave the instance eligible for an actual first-start retry.
func TestInstanceBootstrap_IntentFailureDoesNotPublishRoster(t *testing.T) {
	f := newProvisioningFixture(t)
	root := filepath.Join(f.workDir, workspaceDirName)
	require.NoError(t, os.MkdirAll(root, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, builtinOpenBoxControlDir), []byte("blocked"), 0o600))
	_, err := ensureNewInstanceConfig(f.workDir, "")
	require.Error(t, err)
	_, present, readErr := readInstanceMarker(f.workDir)
	require.NoError(t, readErr)
	require.False(t, present)
	fresh, detectErr := detectNewInstance(f.workDir)
	require.NoError(t, detectErr)
	require.True(t, fresh)
}

// TestInstanceBootstrap_ExistingInstanceDoesNotRecreateIntent 验证存量缺标记仍不补写安装所有权。
// An established instance with a missing installer marker never acquires new install intent.
func TestInstanceBootstrap_ExistingInstanceDoesNotRecreateIntent(t *testing.T) {
	f := newProvisioningFixture(t)
	f.bootstrap(t)
	path := builtinOpenBoxStatePath(filepath.Join(f.workDir, workspaceDirName))
	require.NoError(t, os.Remove(path))
	report, err := ensureNewInstanceConfig(f.workDir, "")
	require.NoError(t, err)
	require.False(t, report.Fresh)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "missing legacy/deleted marker must not revive installation")
}
