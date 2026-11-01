package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/tool"
)

const serveTestIngress = "callers:\n  ingress:\n    run: {qps: 2, burst: 3}\n    ingest: {qps: 1, burst: 2}\n    read: {qps: 3, burst: 5}\n"

func TestServe_MissingIngressKeepsFirstStartRetryable(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	dataRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", dataRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)
	t.Setenv("BASALT_DATABASE_DSN", "postgres://basalt:test@127.0.0.1:1/basalt_test")
	t.Cleanup(func() { tool.SetNetGuard(nil) })

	require.Equal(t, 1, Run(context.Background(), []string{"serve"}))
	path := filepath.Join(workDir, configFileName)
	cfg, err := cli.LoadConfig(path, os.LookupEnv)
	require.NoError(t, err)
	require.ErrorContains(t, cli.ValidateServeIngress(cfg), "callers.ingress.run.qps")
	pending, instanceID, reason := adminTokenProvisioningPending(workDir)
	require.True(t, pending)
	require.Contains(t, reason, "pending")
	require.NotEmpty(t, instanceID)
	require.NoFileExists(t, filepath.Join(workDir, workspaceDirName, databaseFileName))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(serveTestIngress)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	cfg, err = cli.LoadConfig(path, os.LookupEnv)
	require.NoError(t, err)
	require.NoError(t, cli.ValidateServeIngress(cfg))
	// PostgreSQL 此处故意不可达：不能因补限流参数就生成另一实例身份。
	require.Equal(t, 1, Run(context.Background(), []string{"serve"}))
	pending, afterID, reason := adminTokenProvisioningPending(workDir)
	require.True(t, pending)
	require.Contains(t, reason, "pending")
	require.Equal(t, instanceID, afterID)
}
