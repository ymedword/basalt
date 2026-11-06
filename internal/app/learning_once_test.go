//go:build integration

package app

// 本文件是 US-22 T018 的**有库**用例：判唯一的同步钩子在生产 Run 链路上每 Run 恰好
// 触发一次（C-05 / FR-22-02）。钩子拿到的是本次 Run 准入时冻结的客户端（可断言成
// MemoryExtractor），渠道是 web，且两个 Run 各得一次、run_id 对得上。
//
// 需要 PostgreSQL（`pgtest.NewTestDB`）：库不可用时本文件只在编译层被检查，运行记
// 「未验证」——不冒充通过。
// The database-backed case for "exactly one hook per run" on the production run path.

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// us22CountingHook 记录每次钩子调用的输入与结果；并发安全留给 run 链路的顺序性。
// us22CountingHook records every hook invocation.
type us22CountingHook struct {
	mu      sync.Mutex
	inputs  []core.LearningRunInput
	results []core.RunResult
}

func (h *us22CountingHook) AfterRun(_ context.Context, in core.LearningRunInput, res core.RunResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inputs = append(h.inputs, in)
	h.results = append(h.results, res)
}

func (h *us22CountingHook) snapshot() ([]core.LearningRunInput, []core.RunResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]core.LearningRunInput(nil), h.inputs...), append([]core.RunResult(nil), h.results...)
}

// TestUS22_Hook_OncePerRun 守 C-05 / FR-22-02：两个已完成的 web Run 各触发一次抽取
// 钩子（不是零次、不是每次两次）；inputs 的 run_id 与返回结果一一对应；钩子拿到的是
// 准入冻结的客户端（非 nil Extractor），不是装配期全局客户端。
// 逆改点：在 web handler 再调一次钩子 → 计数翻倍而红；钩子改挂终态写回之前 → 顺序
// 用例（core）先红，此处也会看到未落库的 run_id。
// Exactly one hook invocation per completed web run on the production path.
func TestUS22_Hook_OncePerRun(t *testing.T) {
	hook := &us22CountingHook{}
	h := newRunHarnessWith(t, runHarnessOptions{Learning: hook})

	var runIDs []string
	for i := 0; i < 2; i++ {
		result, err := h.postChat(context.Background(), runHarnessCaller)
		require.NoError(t, err)
		require.Equal(t, core.StatusCompleted, result.Status)
		require.NotEmpty(t, result.RunID)
		runIDs = append(runIDs, result.RunID)
	}

	inputs, results := hook.snapshot()
	require.Len(t, inputs, 2, "两个 Run 各触发一次抽取")
	require.Len(t, results, 2)
	require.NotEqual(t, runIDs[0], runIDs[1], "夹具必须真的跑出两个不同的 Run")
	for i := range inputs {
		require.Equal(t, runIDs[i], inputs[i].RunID, "钩子输入绑定本次 Run")
		require.Equal(t, core.StatusCompleted, results[i].Status)
		require.Equal(t, "web", inputs[i].Channel)
		require.Equal(t, runHarnessAgentName, inputs[i].Agent)
		require.NotNil(t, inputs[i].Extractor,
			"钩子必须拿到准入时冻结的抽取视图，而不是 nil / 全局客户端")
	}
}
