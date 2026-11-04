package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOfflineModelPolicy_RefusesBeforeProcessStart 保证旧授权不能恢复付费测试入口。
// TestOfflineModelPolicy_RefusesBeforeProcessStart prevents dispatch before command lookup.
func TestOfflineModelPolicy_RefusesBeforeProcessStart(t *testing.T) {
	for _, args := range [][]string{
		{"go", "test", "-tags=e2e real", "./internal/e2e"},
		{"go", "test", "-tags", "e2e,real", "./internal/e2e"},
		{"go", "test", "--tags=e2e,real", "./internal/e2e"},
		{"go", "-C", "internal/e2e", "test", "--tags", "e2e,real"},
		{"go", "--C=internal/e2e", "test", "-tags=real"},
		{"go", "-C", ".", "run", "--overlay=alternate.json", "probe.go"},
		{"not-an-executable", "--provider=deepseek"},
		{"not-an-executable", "--provider", "deepseek"},
	} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "never-created.log")
			code, err := execute(context.Background(), ".", ".", command{Args: args, TimeoutSeconds: 1}, log)
			require.Equal(t, 2, code)
			require.ErrorContains(t, err, "automatic validation forbids")
			_, statErr := os.Stat(log)
			require.True(t, os.IsNotExist(statErr), "refusal must precede process/log creation")
		})
	}
}

// TestOfflineModelPolicy_AllowedInfrastructure 保留真实PG/容器/故障进程及只读源码检查。
// TestOfflineModelPolicy_AllowedInfrastructure keeps infrastructure tests and read-only vet.
func TestOfflineModelPolicy_AllowedInfrastructure(t *testing.T) {
	for _, args := range [][]string{{"go", "vet", "-tags=e2e,slow,real"}, {"go", "test", "-tags=integration"}, {"docker", "compose", "up"}, {"go", "test", "-tags=e2e"}} {
		require.NoError(t, offlineModelPolicy([]check{{command: command{Args: args}}}, ""))
	}
	for _, flags := range []string{"-tags=e2e,real", "-tags='e2e real'", "--tags='e2e real'", "-overlay=other.json", "--overlay=other.json"} {
		require.Error(t, offlineModelPolicy(nil, flags))
	}
}

// TestOfflineModelPolicy_EnvironmentPreservesFixtures 验证剥离真实凭据而不影响本地夹具。
// TestOfflineModelPolicy_EnvironmentPreservesFixtures preserves local fixture variables.
func TestOfflineModelPolicy_EnvironmentPreservesFixtures(t *testing.T) {
	got := offlineModelEnvironment([]string{"DASHSCOPE_API_KEY=synthetic", "BASALT_REAL_LLM_API_KEY=synthetic", "BASALT_US19_GATEWAY_TOKEN=synthetic", "BASALT_TEST_PG_DSN=local", "BASALT_E2E_KEY=fixture", "PATH=tools"})
	items := []string{"BASALT_TEST_PG_DSN=local", "BASALT_E2E_KEY=fixture", "PATH=tools", "BASALT_AUTOMATED_VALIDATION=1"}
	for _, value := range items {
		require.Contains(t, got, value)
	}
	for _, value := range []string{"DASHSCOPE_API_KEY=synthetic", "BASALT_REAL_LLM_API_KEY=synthetic", "BASALT_US19_GATEWAY_TOKEN=synthetic"} {
		require.NotContains(t, got, value)
	}
}

// TestOfflineModelPolicy_ChildEnvironment 实际子进程只能看到本地夹具变量。
// TestOfflineModelPolicy_ChildEnvironment checks the real child's inherited environment.
func TestOfflineModelPolicy_ChildEnvironment(t *testing.T) {
	t.Setenv("DASHSCOPE_API_KEY", "synthetic-model-key")
	t.Setenv("BASALT_REAL_LLM_API_KEY", "synthetic-model-key")
	t.Setenv("BASALT_TEST_PG_DSN", "offline-fixture-dsn")
	t.Setenv("BASALT_E2E_KEY", "offline-fixture-key")
	t.Setenv("BASALT_OFFLINE_ENV_CHILD", "1")
	dir := t.TempDir()
	code, err := execute(context.Background(), dir, dir, command{Args: []string{os.Args[0], "-test.run=^TestOfflineModelEnvironmentChild$"}, TimeoutSeconds: 10}, filepath.Join(dir, "child.log"))
	require.NoError(t, err)
	require.Zero(t, code)
}

// TestOfflineModelEnvironmentChild 是离线环境检查子进程，不执行网络请求。
// TestOfflineModelEnvironmentChild is an offline child-process probe with no network calls.
func TestOfflineModelEnvironmentChild(t *testing.T) {
	if os.Getenv("BASALT_OFFLINE_ENV_CHILD") != "1" {
		return
	}
	require.Empty(t, os.Getenv("DASHSCOPE_API_KEY"))
	require.Empty(t, os.Getenv("BASALT_REAL_LLM_API_KEY"))
	require.Equal(t, "offline-fixture-dsn", os.Getenv("BASALT_TEST_PG_DSN"))
	require.Equal(t, "offline-fixture-key", os.Getenv("BASALT_E2E_KEY"))
	require.Equal(t, "1", os.Getenv("BASALT_AUTOMATED_VALIDATION"))
}
