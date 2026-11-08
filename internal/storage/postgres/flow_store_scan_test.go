package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/flow"
)

// TestFlowCheckpointScanErrorDistinguishesCancellation 守住"取消不是检查点损坏"：
// Worker 停机恰好落在检查点归一化途中时，扫描必须把取消原样上抛，**不得**折成
// ErrCheckpointUnsupported——后者会让 `flow.Worker.Run` 把一次正常停机返回成致命
// 错误（CI `-race` 019-flow-race 的偶发失败即这条路径）。真实数据损坏仍在
// `TestFlow_CrossCallerMalformedCheckpointHidden` / `TestFlow_TaskContractHashMismatchBlocksWorker`
// 按 ErrCheckpointUnsupported 判红。
// A cancelled canonicalization must stay a cancellation, not corruption.
func TestFlowCheckpointScanErrorDistinguishesCancellation(t *testing.T) {
	t.Run("cancelled-context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := flowCheckpointScanError(ctx, context.Canceled)
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, flow.ErrCheckpointUnsupported)
	})
	t.Run("expired-deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
		defer cancel()
		err := flowCheckpointScanError(ctx, context.DeadlineExceeded)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NotErrorIs(t, err, flow.ErrCheckpointUnsupported)
	})
	t.Run("cancellation-sentinel-with-live-context", func(t *testing.T) {
		err := flowCheckpointScanError(context.Background(), context.Canceled)
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, flow.ErrCheckpointUnsupported)
	})
	t.Run("corrupt-checkpoint", func(t *testing.T) {
		err := flowCheckpointScanError(context.Background(), errors.New("flow json syntax invalid"))
		require.ErrorIs(t, err, flow.ErrCheckpointUnsupported)
	})
}
