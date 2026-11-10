package app

// 本文件是 US-25C FR-25C-15 / FR-25C-16（五组缺口里的 G1）的判据：四类运行期策略
// （允许域、拒绝域、网络例外、执行档位）的一致快照刷新。判据分三层：
//
//  1. **摘要来自实际应用的那份快照**（C5）：每轮每服务只读一次，应用与取摘要同源。
//     「发现读 A、应用读 B、却存下 A 的摘要」的写法必须不可复现——否则数据库回到 A
//     时比较相等、跳过应用，内存永久停在 B（`TestPolicyRefresh_DigestMatchesApplied`）；
//  2. **变化必须被发现**：删除（例外表无 `updated_at`，`DELETE` + `INSERT` 对任何
//     `max()` 不可见）与「同内容不同版本号」都不能被上一轮的摘要骗过去
//     （`TestPolicyRefresh_DigestDetectsDelete` / `TestPolicyRefresh_SameContentNewRevision`）；
//  3. **新鲜度与内容版本分开**（C6）：`lastVerifiedAt` 只描述「最近一轮四个服务全部
//     成功」。内容长期不变 + 刷新持续成功必须始终放行；读失败不推进时间戳，越过
//     `policy.max_stale` 后准入返回 `core.ErrPolicyStale`（503 `POLICY_STALE`），
//     被拒的 Run 不进入执行路径（`TestPolicyRefresh_LongIdleStaysFresh` /
//     `TestPolicyRefresh_ConcurrentNoRegression` / `TestPolicyRefresh_StaleRejectsRun`）。
//
// 本层**不**覆盖：真实 MCP 连接的撤销动作（`RevokeHTTP` 以计数桩判定「是否要求撤销」，
// 连接级行为在资源准备与 A+B 真实链路验收）、`Run` 的周期驱动（启动顺序由装配层与
// 集成层承担）。`.RefreshOnce` 的单飞是整轮互斥，不依赖各服务自身锁的串行。
// This file pins the consistent-snapshot policy refresh: digests come from the exact
// rows applied, deletions and revision-only changes are detected, and freshness is
// judged by the last fully successful round, never by content timestamps.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// T035 的符号契约：四个策略服务必须实现一致快照刷新端口，刷新器必须满足准入
// 新鲜度门读取的只读接口。任何一项缺失都在编译期暴露，而不是留到接线时才红。
var (
	_ core.PolicyDigestSource = (*NetworkAllowlist)(nil)
	_ core.PolicyDigestSource = (*NetworkExceptionService)(nil)
	_ core.PolicyDigestSource = (*ExecutionPolicyService)(nil)
	_ PolicyFreshness         = (*PolicyRefresher)(nil)
)

const (
	// policyRefreshInterval 是配置候选默认值 `policy.sync_interval`（30s 的十倍，
	// 让十个周期真的越过 max_stale，放大「用内容时间戳判过期」的误判窗口）。
	policyRefreshInterval = time.Minute
	// policyRefreshMaxStale 是配置候选默认值 `policy.max_stale`（§3.3）。
	policyRefreshMaxStale = 5 * time.Minute
)

// refreshClock 是可推进的测试时钟：四个服务与刷新器共用同一读数，避免"服务写
// 一个时间、刷新器记另一个时间"的伪影。
// A single advanceable clock feeds both the services and the refresher.
type refreshClock struct {
	mu  sync.Mutex
	now time.Time
}

// newRefreshClock 从固定时刻起步，用例不依赖真实时间。
func newRefreshClock() *refreshClock {
	return &refreshClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
}

// Now 返回当前读数。
func (c *refreshClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 推进时钟。
func (c *refreshClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(delta)
}

// refreshDomainStore 是 `core.NetworkDomainPolicyStore` 的最小替身：按作用域持一行，
// 并允许测试在某一次刷新前排队若干个「瞬时值」——每个瞬时值被一次 Load 消耗，用来
// 构造「同一次刷新里两次读拿到不同内容」的并发写窗口。未观测的 Seed / Update 经
// 内嵌 nil 接口保持未实现：误调用即 panic，而不是悄悄落一行。
// The queued overrides model a concurrent writer landing between two reads inside one
// refresh; unobserved write methods stay nil so an accidental call panics.
type refreshDomainStore struct {
	core.NetworkDomainPolicyStore

	mu       sync.Mutex
	rows     map[string]core.NetworkDomainPolicy
	queued   map[string][]core.NetworkDomainPolicy
	failures map[string]error
	delay    time.Duration
	inFlight int
	peak     int
}

// newRefreshDomainStore 按作用域预置权威行。
func newRefreshDomainStore(rows ...core.NetworkDomainPolicy) *refreshDomainStore {
	store := &refreshDomainStore{
		rows:     make(map[string]core.NetworkDomainPolicy, len(rows)),
		queued:   map[string][]core.NetworkDomainPolicy{},
		failures: map[string]error{},
	}
	for _, row := range rows {
		store.rows[row.Scope] = row
	}
	return store
}

// Load 返回该作用域的一行；排队值优先于稳定值。
func (s *refreshDomainStore) Load(_ context.Context, scope string) (core.NetworkDomainPolicy, bool, error) {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	delay := s.delay
	s.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
	if err := s.failures[scope]; err != nil {
		return core.NetworkDomainPolicy{}, false, err
	}
	if queued := s.queued[scope]; len(queued) > 0 {
		next := queued[0]
		s.queued[scope] = queued[1:]
		return next, true, nil
	}
	row, exists := s.rows[scope]
	return row, exists, nil
}

// set 覆盖该作用域的稳定行（模拟一次已落库的管理写）。
func (s *refreshDomainStore) set(row core.NetworkDomainPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[row.Scope] = row
}

// queue 排入一批按次消耗的瞬时值。
func (s *refreshDomainStore) queue(scope string, rows ...core.NetworkDomainPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queued[scope] = append(s.queued[scope], rows...)
}

// clearQueue 清空瞬时值（模拟并发写窗口结束、数据库回到稳定值）。
func (s *refreshDomainStore) clearQueue(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.queued, scope)
}

// fail 让该作用域的读失败（模拟数据库中断）。
func (s *refreshDomainStore) fail(scope string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[scope] = err
}

// clearFailure 解除读失败。
func (s *refreshDomainStore) clearFailure(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, scope)
}

// setDelay 放大读窗口，供并发用例观测是否有两轮同时在读。
func (s *refreshDomainStore) setDelay(delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = delay
}

// peakConcurrency 返回见过的最大并发读数量。
func (s *refreshDomainStore) peakConcurrency() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// refreshExceptionStore 是 `core.NetworkExceptionStore` 的最小替身。
type refreshExceptionStore struct {
	core.NetworkExceptionStore

	mu   sync.Mutex
	list []core.NetworkException
	err  error
}

// List 返回当前集合快照。
func (s *refreshExceptionStore) List(context.Context) ([]core.NetworkException, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return append([]core.NetworkException(nil), s.list...), nil
}

// set 覆盖集合（模拟一次增删已落库）。
func (s *refreshExceptionStore) set(list ...core.NetworkException) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append([]core.NetworkException(nil), list...)
}

// refreshExecutionStore 是 `core.ExecutionPolicyStore` 的最小替身。
type refreshExecutionStore struct {
	core.ExecutionPolicyStore

	mu     sync.Mutex
	policy core.ExecutionPolicy
}

// Load 返回档位行。
func (s *refreshExecutionStore) Load(context.Context) (core.ExecutionPolicy, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy, true, nil
}

// refreshOperationGuard 把「刷新是只读的」变成可判事实：刷新若试图写管理操作行，
// 会在这组内嵌 nil 接口上 panic，而不是静默留下一条审计记录。
// Any management-operation write during a refresh panics instead of landing.
type refreshOperationGuard struct {
	core.OperationStore
}

// policyRefreshFixture 是一次刷新链的完整装配：四个策略服务共用同一个策略实例
// （与生产同形），刷新器的撤销动作以计数器观测。
type policyRefreshFixture struct {
	clock            *refreshClock
	policy           *tool.WhitelistPolicy
	domains          *refreshDomainStore
	exceptions       *refreshExceptionStore
	execution        *refreshExecutionStore
	allowlist        *NetworkAllowlist
	denylist         *NetworkAllowlist
	exceptionService *NetworkExceptionService
	executionService *ExecutionPolicyService
	revokes          atomic.Int64
	refresher        *PolicyRefresher
}

// newPolicyRefreshFixture 按 §2.3.6 的装配形态给出一套策略刷新链。
func newPolicyRefreshFixture(t *testing.T) *policyRefreshFixture {
	t.Helper()

	clock := newRefreshClock()
	now := clock.Now()
	logger := slog.New(slog.DiscardHandler)
	policy := tool.NewWhitelistPolicy(tool.PolicyOptions{})

	domains := newRefreshDomainStore(
		core.NetworkDomainPolicy{
			Scope:     core.PolicyScopeHTTPDomains,
			Domains:   []string{"a.example.com"},
			Revision:  1,
			UpdatedAt: now,
		},
		core.NetworkDomainPolicy{
			Scope:     core.PolicyScopeHTTPDeniedDomains,
			Domains:   []string{"blocked.example.com"},
			Revision:  1,
			UpdatedAt: now,
		},
	)
	exceptions := &refreshExceptionStore{list: []core.NetworkException{{
		ExceptionID: "exc_intranet",
		Kind:        core.NetworkExceptionKindHost,
		Value:       "intranet.example.com",
		Note:        "内网只读接口",
		Revision:    1,
		CreatedAt:   now,
	}}}
	execution := &refreshExecutionStore{policy: core.ExecutionPolicy{
		Profile:   core.ProfileStandard,
		Revision:  1,
		UpdatedAt: now,
	}}
	operations := refreshOperationGuard{}

	fixture := &policyRefreshFixture{
		clock:      clock,
		policy:     policy,
		domains:    domains,
		exceptions: exceptions,
		execution:  execution,
	}
	fixture.allowlist = NewNetworkAllowlist(NetworkAllowlistDeps{
		Scope:      core.PolicyScopeHTTPDomains,
		Store:      domains,
		Operations: operations,
		Policy:     policy,
		Logger:     logger,
		Now:        clock.Now,
	})
	fixture.denylist = NewNetworkAllowlist(NetworkAllowlistDeps{
		Scope:      core.PolicyScopeHTTPDeniedDomains,
		Store:      domains,
		Operations: operations,
		Policy:     policy,
		Logger:     logger,
		Now:        clock.Now,
	})
	fixture.exceptionService = NewNetworkExceptionService(NetworkExceptionDeps{
		Store:      exceptions,
		Operations: operations,
		Policy:     policy,
		Logger:     logger,
		Now:        clock.Now,
	})
	fixture.executionService = NewExecutionPolicyService(ExecutionPolicyDeps{
		Store:      execution,
		Operations: operations,
		Policy:     policy,
		Logger:     logger,
		Now:        clock.Now,
	})

	// RevokeHTTP 的返回值是本次撤销的连接数；用例只判「有没有要求撤销」。
	// The revocation count is irrelevant here; the test judges whether it was requested.
	fixture.refresher = NewPolicyRefresher(PolicyRefresherDeps{
		Allowlist:  fixture.allowlist,
		Denylist:   fixture.denylist,
		Exceptions: fixture.exceptionService,
		Execution:  fixture.executionService,
		RevokeHTTP: func() int { return int(fixture.revokes.Add(1)) },
		Logger:     logger,
		Now:        clock.Now,
		Interval:   policyRefreshInterval,
		MaxStale:   policyRefreshMaxStale,
	})
	return fixture
}

// refreshOnce 执行一轮同步刷新并要求四个服务全部成功。
func (f *policyRefreshFixture) refreshOnce(t *testing.T) {
	t.Helper()
	_, err := f.refresher.RefreshOnce(context.Background())
	require.NoError(t, err, "四个策略服务都成功的一轮必须返回 nil")
}

// admission 按生产接线的形状给出带新鲜度门的准入层。
func (f *policyRefreshFixture) admission() *runAdmission {
	return &runAdmission{
		policyFreshness: f.refresher,
		policyMaxStale:  policyRefreshMaxStale,
		now:             f.clock.Now,
	}
}

// runAttempt 记录一次准入尝试：err 是准入结论，calls 是执行路径被进入的次数。
type runAttempt struct {
	err   error
	calls int
}

// tryRun 经准入层发起一次交互 Run（无 Flow、无限制器）。
func tryRun(t *testing.T, admission *runAdmission) runAttempt {
	t.Helper()
	attempt := runAttempt{}
	_, attempt.err = admission.run(
		context.Background(),
		core.RunRequest{RunID: "run-1", Agent: "daily"},
		func(context.Context, core.RunRequest) (core.RunResult, error) {
			attempt.calls++
			return core.RunResult{RunID: "run-1"}, nil
		},
	)
	return attempt
}

// TestPolicyRefresh_DigestMatchesApplied 守 C5：摘要必须来自本轮**实际应用**的那份
// 快照。用例构造「发现读 A、应用读 B」的并发写窗口——错误实现会应用 B 却存下 A 的
// 摘要；数据库回到 A 后，比较相等 → 跳过应用 → 被撤掉的内容永远留在内存里。
// The stored digest must come from the exact rows applied; a discovery-read digest
// would keep a withdrawn policy alive forever.
func TestPolicyRefresh_DigestMatchesApplied(t *testing.T) {
	fixture := newPolicyRefreshFixture(t)

	fixture.refreshOnce(t)
	require.Equal(t, []string{"a.example.com"}, fixture.allowlist.Current().Domains,
		"首轮必须把数据库行应用到内存")

	// 同一次刷新里的两次读拿到不同内容：第一次 A、第二次 B。
	// 正确实现每轮只读一次，本轮仍然应用 A；错误实现（先用发现读的摘要、后应用 B）
	// 会应用 B 却存下 A 的摘要。
	stable := fixture.allowlist.Current()
	transient := stable
	transient.Domains = []string{"b.example.com"}
	transient.Revision = stable.Revision + 1
	fixture.domains.queue(core.PolicyScopeHTTPDomains, stable, transient)

	fixture.refreshOnce(t)
	fixture.domains.clearQueue(core.PolicyScopeHTTPDomains)

	// 数据库回到 A（B 只是一次短暂并发写）。若摘要来自"发现读"的 A，本轮比较相等 →
	// 跳过应用 → 内存永久停在 B。
	fixture.refreshOnce(t)

	require.Equal(t, []string{"a.example.com"}, fixture.allowlist.Current().Domains,
		"摘要必须来自本轮实际应用的那份快照：否则数据库回到 A 后内存会永久停在 B")
	require.Equal(t, []string{"a.example.com"}, fixture.policy.AllowedDomains(),
		"判定面（策略实例）必须与视图快照同源")
}

// TestPolicyRefresh_DigestDetectsDelete 守「删除必须被发现」：访问例外没有
// `updated_at`，更新走 `DELETE` + `INSERT`——删除对任何 `max(updated_at)` 都不可见，
// 只有内容摘要能看见。残留的旧例外会继续放行已撤销的主机。
// A deleted exception must disappear from the live set, not linger from the last round.
func TestPolicyRefresh_DigestDetectsDelete(t *testing.T) {
	fixture := newPolicyRefreshFixture(t)
	second := core.NetworkException{
		ExceptionID: "exc_api",
		Kind:        core.NetworkExceptionKindHost,
		Value:       "api.example.com",
		Note:        "外部只读接口",
		Revision:    1,
		CreatedAt:   fixture.clock.Now(),
	}
	fixture.exceptions.set(fixture.exceptions.list[0], second)

	fixture.refreshOnce(t)
	require.Len(t, fixture.exceptionService.List(), 2)
	require.Len(t, fixture.policy.NetworkExceptions(), 2)
	baseline := fixture.revokes.Load()
	require.Positive(t, baseline, "首轮摘要从空到有，必须要求撤销一次")

	// 删除一条：行没了，时间戳什么都不会变。
	fixture.exceptions.set(fixture.exceptions.list[0])
	fixture.refreshOnce(t)

	require.Len(t, fixture.exceptionService.List(), 1,
		"删除必须被检出：残留的旧例会继续放行已撤销的主机")
	require.Equal(t, "intranet.example.com", fixture.exceptionService.List()[0].Value)
	require.Len(t, fixture.policy.NetworkExceptions(), 1,
		"策略实例（判定面）必须与视图同源")
	require.Greater(t, fixture.revokes.Load(), baseline,
		"内容变化必须触发 RevokeDisallowedHTTP（收紧已建立的 MCP http 连接）")
}

// TestPolicyRefresh_SameContentNewRevision 守「同内容不同版本号」也必须被发现：
// 若内容摘要按"内容相同"短路，另一次把列表原样重写的写入就会被历史摘要骗过去。
// 规范化口径是 scope → (revision, 排序去重 domains)。
// A revision bump with identical domains is still a change under the frozen digest rule.
func TestPolicyRefresh_SameContentNewRevision(t *testing.T) {
	fixture := newPolicyRefreshFixture(t)
	fixture.refreshOnce(t)
	baseline := fixture.revokes.Load()
	require.Positive(t, baseline)

	row := fixture.allowlist.Current()
	row.Revision++
	row.UpdatedAt = fixture.clock.Now()
	fixture.domains.set(row)

	fixture.refreshOnce(t)
	require.Equal(t, row.Revision, fixture.allowlist.Current().Revision,
		"新修订号必须被应用")
	require.Greater(t, fixture.revokes.Load(), baseline,
		"同内容不同版本号也必须被发现（C5 规范化口径含 revision）")
}

// TestPolicyRefresh_ConcurrentNoRegression 守两件事：刷新整轮单飞（并发 tick 不得让
// 两轮同时读同一批服务，否则组合摘要可能来自两轮），以及失败轮不推进
// `lastVerifiedAt`（否则一次读失败会被洗成「刚核对过」）。
// Whole-round single-flight, and a failed round never advances the verified stamp.
func TestPolicyRefresh_ConcurrentNoRegression(t *testing.T) {
	fixture := newPolicyRefreshFixture(t)
	fixture.domains.setDelay(2 * time.Millisecond)

	const rounds = 8
	var group sync.WaitGroup
	errs := make([]error, rounds)
	for i := 0; i < rounds; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, errs[index] = fixture.refresher.RefreshOnce(context.Background())
		}(i)
	}
	group.Wait()
	for index, err := range errs {
		require.NoError(t, err, "第 %d 个并发刷新失败", index)
	}

	require.Equal(t, 1, fixture.domains.peakConcurrency(),
		"刷新必须单飞：并发 tick 不得让两轮同时读同一批策略服务")

	// 并发只影响时序，不影响事实：结束状态必须与库里一致。
	require.Equal(t, []string{"a.example.com"}, fixture.allowlist.Current().Domains)
	require.Equal(t, []string{"blocked.example.com"}, fixture.denylist.Current().Domains)
	require.Len(t, fixture.exceptionService.List(), 1)
	require.Equal(t, core.ProfileStandard, fixture.executionService.Current().Profile)
	require.False(t, fixture.refresher.LastVerified().IsZero(),
		"全部成功的一轮必须推进 lastVerifiedAt")

	verified := fixture.refresher.LastVerified()
	fixture.clock.Advance(30 * time.Second)
	fixture.domains.fail(core.PolicyScopeHTTPDomains, errors.New("数据库中断"))
	_, err := fixture.refresher.RefreshOnce(context.Background())
	require.Error(t, err, "任一服务读失败必须如实返回错误")
	require.Equal(t, verified, fixture.refresher.LastVerified(),
		"失败轮不得推进 lastVerifiedAt：否则一次读失败会被洗成「刚核对过」")
	require.Equal(t, []string{"a.example.com"}, fixture.allowlist.Current().Domains,
		"失败的服务保持上一轮内容，不得被清空或半应用")
}

// TestPolicyRefresh_LongIdleStaysFresh 守 C6 的前半：内容长期不变 + 刷新持续成功
// 必须始终放行。十个刷新周期（600s）已越过 `policy.max_stale`（300s）——若过期判定
// 用内容时间戳 / 版本号，健康实例会在这里被误判 `POLICY_STALE`。
// Ten idle-but-healthy cycles exceed max_stale; freshness must follow the verified stamp.
func TestPolicyRefresh_LongIdleStaysFresh(t *testing.T) {
	fixture := newPolicyRefreshFixture(t)
	fixture.refreshOnce(t)
	start := fixture.clock.Now()
	revision := fixture.allowlist.Current().Revision

	for cycle := 0; cycle < 10; cycle++ {
		fixture.clock.Advance(policyRefreshInterval)
		fixture.refreshOnce(t)
		require.False(t, fixture.refresher.LastVerified().IsZero())
		require.Equal(t, revision, fixture.allowlist.Current().Revision,
			"内容自始至终未变（第 %d 轮）", cycle+1)
	}
	require.Greater(t, fixture.clock.Now().Sub(start), policyRefreshMaxStale,
		"十个刷新周期必须真的越过 max_stale，否则本用例判不出区别")

	attempt := tryRun(t, fixture.admission())
	require.NoError(t, attempt.err,
		"内容长期不变 + 刷新持续成功 = 新鲜：不得因 max_stale 误判过期")
	require.Equal(t, 1, attempt.calls)
}

// TestPolicyRefresh_StaleRejectsRun 守 C6 的后半：过期判定用「最近一次成功核对时间」。
// 从未成功过 → 拒绝；阈值内 → 放行；越过阈值 → `core.ErrPolicyStale`；读失败不推进
// 时间戳；恢复后重新放行。被拒的 Run 一律不进入执行路径。
// Staleness follows the last fully successful round; a rejected run never executes.
func TestPolicyRefresh_StaleRejectsRun(t *testing.T) {
	fixture := newPolicyRefreshFixture(t)

	// 启动期同步刷新失败：lastVerifiedAt 保持零值。
	fixture.domains.fail(core.PolicyScopeHTTPDomains, errors.New("数据库不可达"))
	_, err := fixture.refresher.RefreshOnce(context.Background())
	require.Error(t, err)
	require.True(t, fixture.refresher.LastVerified().IsZero(),
		"从未成功核对过策略时 lastVerifiedAt 必须是零值，不能记成本次尝试时间")

	attempt := tryRun(t, fixture.admission())
	require.ErrorIs(t, attempt.err, core.ErrPolicyStale,
		"从未成功核对过策略时必须失败关闭（fail-closed）")
	require.Zero(t, attempt.calls, "被拒的 Run 不得进入执行路径")

	// 恢复：成功一轮 → 立刻放行。
	fixture.domains.clearFailure(core.PolicyScopeHTTPDomains)
	fixture.refreshOnce(t)
	attempt = tryRun(t, fixture.admission())
	require.NoError(t, attempt.err, "刷新恢复后必须重新放行")
	require.Equal(t, 1, attempt.calls)

	// 阈值内：距上次成功 200s（< max_stale）仍放行。
	fixture.clock.Advance(200 * time.Second)
	attempt = tryRun(t, fixture.admission())
	require.NoError(t, attempt.err, "阈值内必须放行")
	require.Equal(t, 1, attempt.calls)

	// 越过阈值：距上次成功 400s（> max_stale）→ 拒绝。
	fixture.clock.Advance(200 * time.Second)
	attempt = tryRun(t, fixture.admission())
	require.ErrorIs(t, attempt.err, core.ErrPolicyStale,
		"超过 policy.max_stale 必须拒绝新 Run")
	require.Zero(t, attempt.calls, "被拒的 Run 不得进入执行路径")

	// 读失败推进过期：持续失败、越过阈值后即使时钟不再前进也保持拒绝。
	fixture.domains.fail(core.PolicyScopeHTTPDomains, errors.New("数据库中断"))
	for cycle := 0; cycle < 6; cycle++ {
		fixture.clock.Advance(policyRefreshInterval)
		_, _ = fixture.refresher.RefreshOnce(context.Background())
	}
	attempt = tryRun(t, fixture.admission())
	require.ErrorIs(t, attempt.err, core.ErrPolicyStale,
		"最近一轮读失败且已越过 max_stale：准入必须继续拒绝（不得把失败轮洗成成功）")

	// 恢复 → 重新放行。
	fixture.domains.clearFailure(core.PolicyScopeHTTPDomains)
	fixture.refreshOnce(t)
	attempt = tryRun(t, fixture.admission())
	require.NoError(t, attempt.err, "刷新恢复后必须重新放行")
	require.Equal(t, 1, attempt.calls)
}
