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

// TestRun_HeavyCommandSplit 追加一条（US-5 / T031）：`tool` 属**重路径**——工具清单的答案
// 来自装配期构建的内存注册池，注册池之外根本不存在（颗粒度文档注 2 的推导链：答案涉及
// 注册表或数据库 ⇒ 走完整装配），因此它与 `chat` 同路；`session` 仍走轻路径的**只读库句柄**
// 支路（不取锁、不迁移、不恢复——FR-024）。
// `tool` joins the heavy path because its answer lives in the assembly-time registry;
// `session` keeps the read-only handle branch.
//
// 判据落在分流函数上（两条清单就是分流的全部依据，`Run` 的分支只是照它走）：清单改了而
// 行为没跟上、或行为改了而清单没跟上，这里都会红。
func TestRun_HeavyCommandSplit(t *testing.T) {
	require.True(t, needsRuntime([]string{"tool", "list"}), "tool 是重命令（与 chat 同路）")
	require.True(t, needsRuntime([]string{"chat", "--message", "hi"}), "chat 仍是重命令")
	require.False(t, needsReadStore([]string{"tool", "list"}),
		"重路径**不再**走只读句柄支路——两条支路互斥，答案只能来自其中一条")

	require.False(t, needsRuntime([]string{"session", "list"}), "session 仍是轻命令（不取锁）")
	require.True(t, needsReadStore([]string{"session", "list"}), "session 需要一条只读库句柄")

	require.False(t, needsRuntime([]string{"--version"}), "全是选项的调用没有命令名")
}
