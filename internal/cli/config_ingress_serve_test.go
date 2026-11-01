package cli_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
)

func TestServeIngressConfigRequiresExplicitPositiveBudgets(t *testing.T) {
	const ingress = `callers:
  ingress:
    run: {qps: 2, burst: 3}
    ingest: {qps: 1, burst: 2}
    read: {qps: 3, burst: 5}
`
	lookup := envLookup(map[string]string{"DEMO_KEY": "test-key", "DEMO_HOST": "example.test"})
	t.Run("missing section", func(t *testing.T) {
		cfg, err := cli.LoadConfig(writeConfig(t, baseConfig), lookup)
		require.NoError(t, err, "离线命令仍可读取旧配置")
		err = cli.ValidateServeIngress(cfg)
		require.ErrorContains(t, err, "callers.ingress.run.qps")
		require.ErrorContains(t, err, "callers.ingress.read.burst")
		require.ErrorContains(t, err, "缺少")
	})
	t.Run("missing one", func(t *testing.T) {
		cfg, err := cli.LoadConfig(writeConfig(t, baseConfig+strings.Replace(ingress, "qps: 3, burst: 5", "qps: 3", 1)), lookup)
		require.NoError(t, err)
		err = cli.ValidateServeIngress(cfg)
		require.ErrorContains(t, err, "callers.ingress.read.burst")
		require.NotContains(t, err.Error(), "callers.ingress.run.qps")
	})
	t.Run("zero", func(t *testing.T) {
		cfg, err := cli.LoadConfig(writeConfig(t, baseConfig+strings.Replace(ingress, "qps: 2", "qps: 0", 1)), lookup)
		require.NoError(t, err)
		err = cli.ValidateServeIngress(cfg)
		require.ErrorContains(t, err, "callers.ingress.run.qps")
		require.ErrorContains(t, err, "大于零")
	})
	t.Run("negative", func(t *testing.T) {
		_, err := cli.LoadConfig(writeConfig(t, baseConfig+strings.Replace(ingress, "qps: 2", "qps: -1", 1)), lookup)
		require.ErrorContains(t, err, "callers.ingress.run.qps")
	})
	t.Run("complete", func(t *testing.T) {
		cfg, err := cli.LoadConfig(writeConfig(t, baseConfig+ingress), lookup)
		require.NoError(t, err)
		require.NoError(t, cli.ValidateServeIngress(cfg))
	})
	t.Run("no config", func(t *testing.T) {
		err := cli.ValidateServeIngress(nil)
		require.ErrorContains(t, err, "配置文件")
		require.ErrorContains(t, err, "callers.ingress.run.qps")
	})
}
