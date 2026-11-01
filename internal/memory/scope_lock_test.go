package memory_test

// 本文件是**同作用域写入串行**在语义存储组装层的验收面（颗粒度文档 §3.2 / T051 /
// 约束 13）：`SemanticStore.Append` 必须沿用门面的 per-scope 锁——同 Scope 的第二笔在
// 首笔放行前不得进入载体，不同 Agent 之间互不阻塞。锁的**唯一实现**在门面；本文件的
// 判别力是"组装层没有绕开门面另起一套（或换成一把大锁）"。
// This file pins per-scope serialization through the SemanticStore assembly: the lock
// lives in the facade only, and the gate-double makes entry order observable.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// semanticGateStore 是语义存储的**闸门式**替身：`Append` 一进入就把本次 Scope 发到
// `entered`，然后阻塞到 `release` 关闭——载体进入即意味着门面已持该 Scope 的锁，
// 锁缺失 / 粒度错时进入次序立刻与期望不符。其余端口由内嵌替身提供。
// The gate double turns "who entered the carrier, and when" into an observable event.
type semanticGateStore struct {
	*semanticFakeStore
	entered chan core.Scope // 有缓冲：主 goroutine 先断言"没进来"也不会卡死子 goroutine
	release chan struct{}   // 关闭 = 放行全部被闸住的 Append
}

func (s *semanticGateStore) Append(_ context.Context, scope core.Scope, _ core.Section, _ string) error {
	s.entered <- scope
	<-s.release
	return nil
}

// 编译期断言：闸门替身必须仍是本包端口，否则用例退化成"编译不过"而不是"判据成立"。
var _ memory.MemoryStore = (*semanticGateStore)(nil)

// TestMemory_SameScopeSerialized 守「同作用域串行、不同 Agent 不互相阻塞」（§4 判别力
// 表 / 约束 13）：① alice 首笔被闸住时，alice 第二笔**不得**进入载体；② 同一时刻 bob
// **必须**能进入载体——把 per-scope 锁换成一把大锁 / 按层只建两把时，② 等到超时红。
// 逆改点：组装层绕开门面直接写载体 → ① 立刻红（第二笔直接进了载体）；换一把大锁 →
// ② 超时红。并发纪律：require 一律回主 goroutine，所有等待都带超时。
// 验收点：约束 13 / 口径 19。
func TestMemory_SameScopeSerialized(t *testing.T) {
	alice := core.Scope{Agent: "alice"}
	bob := core.Scope{Agent: "bob"}

	gate := &semanticGateStore{
		semanticFakeStore: newSemanticFakeStore(t.TempDir()),
		entered:           make(chan core.Scope, 8),
		release:           make(chan struct{}),
	}
	// 用例中途失败时也放行全部被闸住的 Append——失败的用例不该把 goroutine 永久挂住。
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(releaseAll)

	svc := memory.NewSemanticStore(memory.SemanticDeps{Store: gate})

	// 首笔 alice：进入载体后即被闸住（门面持锁不放）。
	aliceFirst := make(chan error, 1)
	go func() { aliceFirst <- svc.Append(ctx, alice, core.SectionCore, "alice 第一笔") }()
	require.Equal(t, alice, waitEntered(t, gate.entered, "alice 第一笔必须进入载体"))

	// ① 同 Scope 第二笔：首笔仍在闸内 → 第二笔不得进入载体。
	aliceSecond := make(chan error, 1)
	go func() { aliceSecond <- svc.Append(ctx, alice, core.SectionCore, "alice 第二笔") }()
	select {
	case scope := <-gate.entered:
		t.Fatalf("同 Scope 的第二笔在首笔放行前进入了载体（scope = %+v）——组装层绕开了 per-scope 锁", scope)
	case <-time.After(200 * time.Millisecond):
		// 预期：窗口内没有任何进入信号（正确实现下这不是竞态，锁让它永远不成立）。
	}

	// ② alice 仍被闸住 → bob 必须能进入（不同 Agent 不共用一把锁）。
	bobAppend := make(chan error, 1)
	go func() { bobAppend <- svc.Append(ctx, bob, core.SectionCore, "bob 的条目") }()
	require.Equal(t, bob, waitEntered(t, gate.entered, "alice 阻塞期间 bob 必须能进入载体（不同 Agent 不互相阻塞）"))

	// 放行：三笔都必须以 nil 结束（同 Scope 的第二笔在锁放行后补进载体）。
	releaseAll()
	require.NoError(t, <-aliceFirst, "alice 第一笔")
	require.NoError(t, <-bobAppend, "bob 的追加")
	require.NoError(t, <-aliceSecond, "alice 第二笔（锁放行后补进载体）")
}
