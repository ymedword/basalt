package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRun_ExitCodes 验收点（文档用例名同名）：装配骨架是退出码的唯一出口——
// 成功 0、失败 1，原因由文案区分而不是靠不同退出码堆叠。
func TestRun_ExitCodes(t *testing.T) {
	t.Run("成功路径返回 0", func(t *testing.T) {
		require.Equal(t, 0, Run(context.Background(), []string{"--version"}))
	})

	t.Run("失败路径返回 1", func(t *testing.T) {
		require.Equal(t, 1, Run(context.Background(), []string{"no-such-command"}))
	})
}
