package cli_test

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/cli"
)

func TestObservability_ConfigDefaults(t *testing.T) {
	cfg, err := cli.LoadConfig(writeConfig(t, "{}\n"), envLookup(nil))
	require.NoError(t, err)
	o := cfg.Observability
	require.NotNil(t, o.Enabled)
	require.True(t, *o.Enabled)
	require.False(t, o.Export.Enabled)
	require.False(t, o.Retention.CleanupEnabled)
	require.True(t, *o.Alerts.Enabled)
	require.Equal(t, 256, o.Export.BatchSize)
	require.Equal(t, 5*time.Minute, o.Export.LeaseTTL)
	require.Equal(t, 30, o.Retention.DiagnosticEventsDays)
	require.False(t, o.Retention.HistoryEnabled)
	require.Equal(t, 30, o.Retention.HistoryDays)
	require.Equal(t, time.Minute, o.Alerts.EvaluationInterval)
}

func TestObservability_ConfigUnsignedOverflow(t *testing.T) {
	for _, value := range []string{"18446744073709551615", "9223372036854775808", "2049"} {
		_, err := cli.LoadConfig(writeConfig(t, "observability:\n  export:\n    batch_size: "+value+"\n"), envLookup(nil))
		require.Error(t, err)
		require.Contains(t, err.Error(), "observability.export.batch_size")
	}
	cfg, err := cli.LoadConfig(writeConfig(t, "observability:\n  export:\n    batch_size: 2048\n"), envLookup(nil))
	require.NoError(t, err)
	require.Equal(t, 2048, cfg.Observability.Export.BatchSize)
}

func TestObservability_ExampleConfigMatchesDefaults(t *testing.T) {
	// 只抽真实示例的OBS段；未配置可选headers时不依赖ENV，保持网络/清理关闭。
	// The actual example OBS section loads without optional headers ENV and keeps export/cleanup off.
	raw, err := os.ReadFile("../../basalt.yaml.example")
	require.NoError(t, err)
	var example map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &example))
	require.Contains(t, example, "observability")
	section, err := yaml.Marshal(map[string]any{"observability": example["observability"]})
	require.NoError(t, err)
	cfg, err := cli.LoadConfig(writeConfig(t, string(section)), envLookup(nil))
	require.NoError(t, err)
	require.Equal(t, cli.DefaultObservabilityConfig(), cfg.Observability)
	require.NoError(t, cli.ValidateObservabilityConfig(cfg.Observability))
}

func TestObservability_ConfigExplicitAndSecrets(t *testing.T) {
	raw := `observability:
  enabled: false
  export:
    enabled: true
    endpoint: ${COLLECTOR}
    headers: ${HEADERS}
    protocol: http/json
    batch_size: 1
    scan_overlap: 0s
    safety_lag: 0s
  alerts:
    enabled: false
`
	cfg, err := cli.LoadConfig(writeConfig(t, raw), envLookup(map[string]string{"COLLECTOR": "http://127.0.0.1:4318", "HEADERS": "Authorization=Bearer CANARY_ENV_SECRET"}))
	require.NoError(t, err)
	o := cfg.Observability
	require.False(t, *o.Enabled)
	require.True(t, o.Export.Enabled)
	require.Equal(t, "http/json", o.Export.Protocol)
	require.Equal(t, 1, o.Export.BatchSize)
	require.Zero(t, o.Export.ScanOverlap)
	require.Zero(t, o.Export.SafetyLag)
	require.False(t, *o.Alerts.Enabled)
	require.Equal(t, "Authorization=Bearer CANARY_ENV_SECRET", o.Export.Headers)
	view, err := json.Marshal(o)
	require.NoError(t, err)
	require.NotContains(t, string(view), "CANARY")
	require.NotContains(t, string(view), "4318")
}

func TestObservability_ConfigRejectsInvalid(t *testing.T) {
	for index, section := range []string{
		"export:\n    enabled: true",
		"export:\n    batch_size: 0",
		"export:\n    batch_size: 2049",
		"export:\n    endpoint: file:///CANARY_PRIVATE_PATH",
		"export:\n    timeout: nonsense",
		"export:\n    headers: Authorization=CANARY_PLAINTEXT",
		"export:\n    rules: CANARY_UNKNOWN",
		"retention:\n    diagnostic_events_days: 0",
		"retention:\n    batch_size: 99",
		"alerts:\n    evaluation_interval: 0s",
		"enabled: CANARY_WRONG_TYPE",
		"unknown: CANARY_KEY",
	} {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			_, err := cli.LoadConfig(writeConfig(t, "observability:\n  "+section+"\n"), envLookup(nil))
			require.Error(t, err)
			require.NotContains(t, err.Error(), "CANARY")
		})
	}
}

func TestObservability_ExportConfigRedacted(t *testing.T) {
	cfg, err := cli.LoadConfig(writeConfig(t, "observability:\n  export:\n    endpoint: ${COLLECTOR}\n    headers: ${HEADERS}\n"), envLookup(map[string]string{"COLLECTOR": "http://CANARY_ENDPOINT.test:4318", "HEADERS": "Authorization=Bearer CANARY_HEADER"}))
	require.NoError(t, err)
	view, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NotContains(t, string(view), "CANARY")
	_, err = cli.LoadConfig(writeConfig(t, "observability:\n  export:\n    headers: ${HEADERS}\n    batch_size: CANARY_WRONG_NUMBER\n"), envLookup(map[string]string{"HEADERS": "Authorization=Bearer CANARY_HEADER"}))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "CANARY")
}

func TestObservability_ConfigHeaderAndBounds(t *testing.T) {
	for index, headers := range []string{"Host=CANARY", "X-Test=value%0D%0ACANARY", "X-Test=a,x-test=b", "X-Test=%zz", "Content-Length=3"} {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			_, err := cli.LoadConfig(writeConfig(t, "observability:\n  export:\n    headers: ${HEADERS}\n"), envLookup(map[string]string{"HEADERS": headers}))
			require.Error(t, err)
			require.NotContains(t, err.Error(), "CANARY")
		})
	}
	cfg, err := cli.LoadConfig(writeConfig(t, "observability:\n  export:\n    headers: ${HEADERS}\n"), envLookup(map[string]string{"HEADERS": "X-Test=a%2Bb%3D%2Cc,Authorization=Bearer%20synthetic"}))
	require.NoError(t, err)
	headers, err := cfg.Observability.Export.ParsedHeaders()
	require.NoError(t, err)
	require.Equal(t, "a+b=,c", headers["X-Test"])
	require.Equal(t, "Bearer synthetic", headers["Authorization"])
	cfg, err = cli.LoadConfig(writeConfig(t, "observability:\n  export:\n    headers: ${HEADERS}\n"), envLookup(map[string]string{"HEADERS": ""}))
	require.NoError(t, err)
	require.Empty(t, cfg.Observability.Export.Headers)
	for index, section := range []string{"export:\n    lease_ttl: 1s\n    timeout: 2s", "export:\n    initial_backfill: 1m\n    safety_lag: 2m", "retention:\n    cleanup_enabled: true\n    assembly_records_days: 1\n    diagnostic_events_days: 7", "alerts:\n    enabled: false\n    unknown_rule: true"} {
		t.Run("bounds-"+strconv.Itoa(index), func(t *testing.T) {
			_, err := cli.LoadConfig(writeConfig(t, "observability:\n  "+section+"\n"), envLookup(nil))
			require.Error(t, err)
		})
	}
}

func TestObservability_ConfigProtectsHistoryWindow(t *testing.T) {
	_, err := cli.LoadConfig(writeConfig(t, "diagnostics:\n  retention_days: 90\nobservability:\n  retention:\n    cleanup_enabled: true\n    assembly_records_days: 30\n"), envLookup(nil))
	require.Error(t, err)
	require.Contains(t, err.Error(), "observability.retention.assembly_records_days")
	_, err = cli.LoadConfig(writeConfig(t, "diagnostics:\n  retention_days: 90\nobservability:\n  retention:\n    cleanup_enabled: false\n    assembly_records_days: 30\n"), envLookup(nil))
	require.NoError(t, err)
}
