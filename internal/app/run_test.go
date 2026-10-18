package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
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

	require.True(t, needsRuntime([]string{"serve"}), "serve 是重命令（US-9：全量装配 + HTTP 面）")
	require.True(t, needsRuntime([]string{"gateway"}), "gateway 是重命令（US-9：全量装配 + 常驻）")

	require.False(t, needsRuntime([]string{"--version"}), "全是选项的调用没有命令名")
}

// TestRunWithRuntime_ReleasesAfterExec 对应颗粒度文档 R17（原文保留）：
// 「逆改点：把 defer rt.Close() 留在 runWithRuntime 之外或提前执行 → order 变成
// ["runtime-close", "exec"]，断言红；runWithRuntime 忘记 defer → exec 出错时资源不
// 释放，第二分支红；Close 调用两次 → closeCalls 断言红」。
func TestRunWithRuntime_ReleasesAfterExec(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("正常返回后释放", func(t *testing.T) {
		rt := &closeRecorderRuntime{}
		err := runWithRuntime(rt, logger, func() error {
			rt.record("exec")
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, []string{"exec", "runtime-close"}, rt.order(),
			"共享资源必须在 exec（Serve）返回之后才释放")
		require.Equal(t, 1, rt.closeCallCount(), "Close 必须恰好调用一次")
	})

	t.Run("exec 出错也释放", func(t *testing.T) {
		rt := &closeRecorderRuntime{}
		err := runWithRuntime(rt, logger, func() error {
			rt.record("exec")
			return errors.New("serve failed")
		})
		require.Error(t, err)
		require.Equal(t, []string{"exec", "runtime-close"}, rt.order())
		require.Equal(t, 1, rt.closeCallCount())
	})
}

// closeRecorderRuntime 是 runtimeCloser 的记录型实现。heavyRuntime.Close 内部
// 「MCP → DB → 锁」的顺序由既有实现保证（wire.go），本用例只验证调用时机与单次性。
type closeRecorderRuntime struct {
	mu         sync.Mutex
	events     []string
	closeCalls int
}

func (r *closeRecorderRuntime) record(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *closeRecorderRuntime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "runtime-close")
	r.closeCalls++
	return nil
}

// order / closeCallCount 返回加锁副本（-race 下不得直接读未加锁字段）。
func (r *closeRecorderRuntime) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *closeRecorderRuntime) closeCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeCalls
}
