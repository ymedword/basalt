package diagnostics

import (
	"bytes"
	"errors"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

func TestRateSampler_EntropyFailureDropsDiagnostic(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SampleRate = 0.5
	for _, reader := range []interface{ Read([]byte) (int, error) }{iotest.ErrReader(errors.New("synthetic entropy failure")), bytes.NewReader([]byte{0})} {
		sampler := NewRateSampler(cfg).WithRoll(func() float64 { return sampleFractionFrom(reader) })
		require.False(t, sampler.Sample(), "reported entropy errors and short reads only drop the optional diagnostic")
	}
	require.True(t, NewRateSampler(cfg).WithRoll(func() float64 { return sampleFractionFrom(bytes.NewReader(make([]byte, 8))) }).Sample())
	require.False(t, NewRateSampler(cfg).WithRoll(func() float64 { return sampleFractionFrom(bytes.NewReader(bytes.Repeat([]byte{255}, 8))) }).Sample())
}
