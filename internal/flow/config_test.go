package flow

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlow_ConfigBoundsCannotOverflowOrRemoveReserves(t *testing.T) {
	config := DefaultConfig()
	require.False(t, config.Enabled)
	require.NoError(t, config.Validate(32, 70*time.Second))
	config.Enabled = true
	require.Error(t, config.Validate(1, 70*time.Second), "single-slot deployment requires explicit zero reserve")
	zero := 0
	config.InteractiveReservedRuns = &zero
	require.NoError(t, config.Validate(1, 70*time.Second))
	config.ShutdownGraceSeconds = math.MaxInt
	require.Error(t, config.Validate(32, 70*time.Second), "huge values must fail before duration multiplication wraps")
	config = DefaultConfig()
	config.ShutdownGraceSeconds = 34
	require.Error(t, config.Validate(32, 70*time.Second))
	config.ShutdownGraceSeconds = 35
	require.Error(t, config.Validate(32, 34*time.Second), "worker drain cannot outlive the real outer shutdown grace")
}
