package cli

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlow_ConfigClosedAndBounded(t *testing.T) {
	cfg, err := parseFlowConfig(nil, false, 0)
	require.NoError(t, err)
	require.False(t, cfg.Enabled)
	require.Equal(t, 4, cfg.MaxActive)
	require.Equal(t, 1, *cfg.InteractiveReservedRuns)
	cfg, err = parseFlowConfig(map[string]any{"enabled": true, "max_active": 2, "interactive_reserved_runs": 0}, true, 1)
	require.NoError(t, err)
	require.True(t, cfg.Enabled)
	require.Equal(t, 2, cfg.MaxActive)
	require.Zero(t, *cfg.InteractiveReservedRuns)
	for _, section := range []any{nil, []any{}, map[string]any{"enabled": "true"}, map[string]any{"unknown-SENTINEL": "private-SENTINEL"}, map[string]any{"max_active": 0}, map[string]any{"max_active": 9}, map[string]any{"max_open_tasks": 33}, map[string]any{"max_stored_tasks": 10001}, map[string]any{"interactive_reserved_runs": nil}, map[string]any{"poll_interval_ms": 0}, map[string]any{"shutdown_grace_seconds": 29}} {
		_, err := parseFlowConfig(section, true, 32)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "SENTINEL")
	}
	for _, field := range []string{"enabled", "max_active", "max_total_tokens"} {
		_, err := parseFlowConfig(map[string]any{field: nil}, true, 32)
		require.Error(t, err)
	}
	_, err = parseFlowConfig(map[string]any{"enabled": true}, true, 1)
	require.Error(t, err, "interactive reservation must leave a Flow slot")
}
