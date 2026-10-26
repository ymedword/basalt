package app

// 本文件判卷网络域名白名单的管理服务（US-12 R8 裁决 2 / R8.2.1–R8.2.4）：首次种子与
// 「配置此后不再覆盖」、条件写（ETag → 预写 → 修订号 CAS → 热替换）、失败路径零写入 /
// 带操作标识、以及收紧后撤销 MCP http 连接。真实 SQLite + 真实管理操作仓储作底座，
// 只有需要注入失败的用例才包一层假 store。
// Acceptance tests for the managed allowlist service over a real SQLite database.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
)

// allowlistEnv 是白名单服务的共享夹具：真库 + 真管理操作仓储 + 真策略实例。
type allowlistEnv struct {
	service *NetworkAllowlist
	store   *storage.NetworkPolicyStore
	ops     *storage.ManagementStore
	db      *storage.DB
	policy  *tool.WhitelistPolicy
	logs    *bytes.Buffer
}

// allowlistClock 是夹具的固定时钟（写入时间可逐字断言）。
func allowlistClock() time.Time { return time.Date(2026, 10, 24, 8, 30, 0, 0, time.UTC) }

// newAllowlistEnv 装配夹具；`mutate` 可替换 store / operations / supervisor。
func newAllowlistEnv(t *testing.T, mutate func(*NetworkAllowlistDeps)) *allowlistEnv {
	t.Helper()
	db, err := storage.OpenWritable(context.Background(),
		filepath.Join(t.TempDir(), "basalt.db"), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	store := storage.NewNetworkPolicyStore(db)
	ops := storage.NewManagementStore(db)
	policy := tool.NewWhitelistPolicy(tool.PolicyOptions{Root: t.TempDir()})
	logger, logs := newCapturedLogger()

	deps := NetworkAllowlistDeps{
		Store:      store,
		Operations: ops,
		Policy:     policy,
		Logger:     logger,
		Now:        allowlistClock,
	}
	if mutate != nil {
		mutate(&deps)
	}
	if deps.Store == nil {
		deps.Store = store
	}
	if deps.Operations == nil {
		deps.Operations = ops
	}
	return &allowlistEnv{
		service: NewNetworkAllowlist(deps),
		store:   store,
		ops:     ops,
		db:      db,
		policy:  policy,
		logs:    logs,
	}
}

// actor 是夹具固定的管理凭据主体。
func allowlistActor() core.Actor {
	return core.Actor{Subject: "token:abc123", Method: "bearer", Source: "127.0.0.1:5000"}
}

// operationRow 读回一行管理操作；不存在时返回 exists=false。
func (e *allowlistEnv) operationRow(t *testing.T, operationID string) (core.OperationRecord, bool) {
	t.Helper()
	rec, err := e.ops.GetOperation(context.Background(), operationID)
	if err != nil {
		return core.OperationRecord{}, false
	}
	return rec, true
}

// TestNetworkAllowlist_SeedsOnceFromConfig —— 空表时把配置种子规整后写入（revision 1、
// source=config_seed），并立即应用到策略实例；非法种子项被丢弃且记 WARN（旧实现下
// 它们同样从不命中）。
// 逆改点：种子未应用 / 非法项让进程起不来 → 本用例红。
func TestNetworkAllowlist_SeedsOnceFromConfig(t *testing.T) {
	env := newAllowlistEnv(t, nil)
	err := env.service.EnsureSeeded(context.Background(),
		[]string{" API.Example.com ", "api.example.com", "https://bad.example.org/x", "*.Hooks.Example.net"})
	require.NoError(t, err)

	live := env.service.Current()
	require.Equal(t, int64(1), live.Revision)
	require.Equal(t, core.NetworkAllowlistSourceConfigSeed, live.Source())
	require.Equal(t, []string{"*.hooks.example.net", "api.example.com"}, live.Domains)
	require.Equal(t, live.Domains, env.policy.AllowedDomains(), "种子必须立即生效")
	require.Contains(t, env.logs.String(), "dropping invalid network domain allowlist seed entry")
}

// TestNetworkAllowlist_ConfigNeverOverridesDatabase —— 表内已有行（管理台写过）时，
// 重启只装载数据库值，配置键被忽略；即使配置是空列表也不清空。
// 逆改点：EnsureSeeded 在有行时仍写种子 → 管理值被配置覆盖，本用例红。
func TestNetworkAllowlist_ConfigNeverOverridesDatabase(t *testing.T) {
	env := newAllowlistEnv(t, nil)
	require.NoError(t, env.service.EnsureSeeded(context.Background(), []string{"api.example.com"}))
	updated, _, err := env.service.Update(context.Background(),
		[]string{"managed.example.com"}, core.NetworkDomainETag(env.service.Current()), allowlistActor())
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Revision)

	restarted := NewNetworkAllowlist(NetworkAllowlistDeps{
		Store: env.store, Operations: env.ops, Policy: env.policy, Now: allowlistClock,
	})
	require.NoError(t, restarted.EnsureSeeded(context.Background(), []string{"config.example.com"}))
	require.Equal(t, []string{"managed.example.com"}, restarted.Current().Domains)
	require.Equal(t, int64(2), restarted.Current().Revision)
	require.Equal(t, []string{"managed.example.com"}, env.policy.AllowedDomains(),
		"重启装载的值同样要应用到运行期策略")
}

// TestNetworkAllowlist_SeedOverLimitRefusesStart —— 种子条目超过上限属于配置错误：
// 拒绝启动并带上下文，不写半份种子。
// 逆改点：超限被静默截断 → 部分策略生效，本用例红。
func TestNetworkAllowlist_SeedOverLimitRefusesStart(t *testing.T) {
	env := newAllowlistEnv(t, nil)
	domains := make([]string, 0, core.MaxDomainEntries+1)
	for i := 0; i <= core.MaxDomainEntries; i++ {
		domains = append(domains, fmt.Sprintf("host-%03d.example.com", i))
	}
	err := env.service.EnsureSeeded(context.Background(), domains)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid network domain allowlist seed")

	_, exists, loadErr := env.store.Load(context.Background(), core.PolicyScopeHTTPDomains)
	require.NoError(t, loadErr)
	require.False(t, exists, "拒绝启动时不得留下半份种子")
}

// TestNetworkAllowlist_UpdateHotAppliesAndAudits —— 成功路径：规整 → 条件写 →
// 立即热替换；管理操作先 pending 后 success，记录动作 / 目标 / 期望 ETag / 详情。
// 逆改点：先换策略后落库 / 不写操作行 / 终态缺失 → 对应断言红。
func TestNetworkAllowlist_UpdateHotAppliesAndAudits(t *testing.T) {
	env := newAllowlistEnv(t, nil)
	require.NoError(t, env.service.EnsureSeeded(context.Background(), []string{"api.example.com"}))
	etag := core.NetworkDomainETag(env.service.Current())

	policy, operationID, err := env.service.Update(context.Background(),
		[]string{" Hooks.Example.net ", "hooks.example.net", "*.api.example.com"}, etag, allowlistActor())
	require.NoError(t, err)
	require.NotEmpty(t, operationID)
	require.Equal(t, int64(2), policy.Revision)
	require.Equal(t, []string{"*.api.example.com", "hooks.example.net"}, policy.Domains)
	require.Equal(t, "token:abc123", policy.UpdatedBy)
	require.Equal(t, operationID, policy.OperationID)
	require.Equal(t, policy.Domains, env.policy.AllowedDomains(), "保存后必须立即生效")

	rec, exists := env.operationRow(t, operationID)
	require.True(t, exists)
	require.Equal(t, core.ActionNetworkAllowlistUpdate, rec.Action)
	require.Equal(t, core.TargetKindNetworkAllowlist, rec.TargetKind)
	require.Equal(t, core.PolicyScopeHTTPDomains, rec.TargetID)
	require.Equal(t, "token:abc123", rec.ActorSubject)
	require.Equal(t, etag, rec.ExpectedETag)
	require.Equal(t, core.OperationSuccess, rec.Status)
	require.Equal(t, "OK", rec.ResultCode)
	require.Contains(t, string(rec.DetailJSON), `"before_revision":1`)
	require.Contains(t, string(rec.DetailJSON), `"after_revision":2`)
	require.NotContains(t, string(rec.DetailJSON), "hooks.example.net",
		"detail 只放修订号与摘要，不放域名清单")
}

// TestNetworkAllowlist_StaleETagWritesNothing —— 陈旧 / 非法 ETag：412 且**零写入、
// 零操作行**；当前策略保持不变。
// 逆改点：ETag 判定挪到预写之后 → 出现 pending 行，本用例红。
func TestNetworkAllowlist_StaleETagWritesNothing(t *testing.T) {
	env := newAllowlistEnv(t, nil)
	require.NoError(t, env.service.EnsureSeeded(context.Background(), []string{"api.example.com"}))
	before := env.service.Current()

	for _, ifMatch := range []string{"", "*", `W/"sha256:` + "0000000000000000000000000000000000000000000000000000000000000000" + `"`} {
		_, _, err := env.service.Update(context.Background(), []string{"evil.example.org"}, ifMatch,
			allowlistActor())
		require.ErrorIs(t, err, core.ErrPreconditionFailed)
	}
	require.Equal(t, before, env.service.Current())
	require.Equal(t, []string{"api.example.com"}, env.policy.AllowedDomains())

	pending, err := env.ops.PendingOperations(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "条件失败不得留下操作行")
}

// TestNetworkAllowlist_InvalidDomainWritesNothing —— 非法条目不进入条件写：返回
// `InvalidInputError` 并点名第一处，零写入、零操作行。
// 逆改点：校验挪到预写之后 → pending 行出现，本用例红。
func TestNetworkAllowlist_InvalidDomainWritesNothing(t *testing.T) {
	env := newAllowlistEnv(t, nil)
	require.NoError(t, env.service.EnsureSeeded(context.Background(), []string{"api.example.com"}))
	etag := core.NetworkDomainETag(env.service.Current())

	_, _, err := env.service.Update(context.Background(),
		[]string{"ok.example.com", "http://bad.example.com/x"}, etag, allowlistActor())
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, err.Error(), "第 2 项")
	require.Equal(t, int64(1), env.service.Current().Revision)

	pending, err := env.ops.PendingOperations(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending)
}

// TestNetworkAllowlist_PreWriteFailureRefuses —— 预写失败即拒绝执行（宪法 V）：策略
// 一个字节都没改，且错误如实上抛（不是"成功但没生效"）。
// 逆改点：预写失败后仍热替换 → policy 变化，本用例红。
func TestNetworkAllowlist_PreWriteFailureRefuses(t *testing.T) {
	env := newAllowlistEnv(t, func(d *NetworkAllowlistDeps) {
		d.Operations = failingBeginOperations{OperationStore: d.Operations, err: errors.New("审计库不可写")}
	})
	// 种子走 store，不经管理操作，因此可以在装配失败面之前完成。
	require.NoError(t, env.service.EnsureSeeded(context.Background(), []string{"api.example.com"}))
	etag := core.NetworkDomainETag(env.service.Current())

	_, operationID, err := env.service.Update(context.Background(), []string{"hooks.example.net"}, etag,
		allowlistActor())
	require.Error(t, err)
	require.Contains(t, err.Error(), "pre-write operation record failed")
	require.Empty(t, operationID)
	require.Equal(t, []string{"api.example.com"}, env.policy.AllowedDomains())
	require.Equal(t, int64(1), env.service.Current().Revision)
}

// failingPolicyStore 把 Update 换成注入错误（其余委托真仓储），用于判"pending 之后
// 失败"的终态与操作标识。
type failingPolicyStore struct {
	core.NetworkDomainPolicyStore
	err error
}

func (s failingPolicyStore) Update(context.Context, string, []string, int64, string, string, time.Time,
) (core.NetworkDomainPolicy, error) {
	return core.NetworkDomainPolicy{}, s.err
}

// TestNetworkAllowlist_PostPendingFailureCarriesOperationID —— 写入失败：返回操作标识，
// 行置 failed 并带结果码；策略保持旧值。
// 逆改点：失败不带 operationID / 行仍是 pending → 页面无法追查，本用例红。
func TestNetworkAllowlist_PostPendingFailureCarriesOperationID(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"revision conflict", fmt.Errorf("%w: lost", core.ErrPolicyRevisionConflict), "CONFLICT"},
		{"internal", errors.New("磁盘已满"), "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newAllowlistEnv(t, nil)
			require.NoError(t, env.service.EnsureSeeded(context.Background(), []string{"api.example.com"}))
			etag := core.NetworkDomainETag(env.service.Current())
			env.service.deps.Store = failingPolicyStore{
				NetworkDomainPolicyStore: env.store, err: tc.err,
			}

			_, operationID, err := env.service.Update(context.Background(),
				[]string{"hooks.example.net"}, etag, allowlistActor())
			require.ErrorIs(t, err, tc.err)
			require.NotEmpty(t, operationID, "预写之后失败必须带回操作标识")
			require.Equal(t, []string{"api.example.com"}, env.policy.AllowedDomains())

			rec, exists := env.operationRow(t, operationID)
			require.True(t, exists)
			require.Equal(t, core.OperationFailed, rec.Status)
			require.Equal(t, tc.code, rec.ResultCode)
			require.NotEmpty(t, rec.ErrorMessage)
		})
	}
}

// TestNetworkAllowlist_RevisionConflictReloadsAuthoritativeRow —— 修订号 CAS 落败后
// 失败路径重读并应用数据库权威行：页面随后的 GET 与逐跳判定看到的是真值，而不是
// 落败进程的旧快照（409 响应本身不承诺 `current_etag`）。
// 逆改点：落败后不重读 → 内存仍停在旧修订，本用例红。
func TestNetworkAllowlist_RevisionConflictReloadsAuthoritativeRow(t *testing.T) {
	env := newAllowlistEnv(t, nil)
	require.NoError(t, env.service.EnsureSeeded(context.Background(), []string{"api.example.com"}))
	stale := core.NetworkDomainETag(env.service.Current())

	// 模拟另一进程先写：数据库推进到 revision 2，本进程快照仍停在 1。
	other, err := env.store.Update(context.Background(), core.PolicyScopeHTTPDomains, []string{"other.example.com"},
		1, "op-other", "other-process", allowlistClock())
	require.NoError(t, err)
	require.Equal(t, int64(2), other.Revision)
	require.Equal(t, int64(1), env.service.Current().Revision, "前提：本进程快照落后")

	env.service.deps.Store = failingPolicyStore{
		NetworkDomainPolicyStore: env.store,
		err:                      fmt.Errorf("%w: lost", core.ErrPolicyRevisionConflict),
	}
	_, operationID, err := env.service.Update(context.Background(),
		[]string{"hooks.example.net"}, stale, allowlistActor())
	require.ErrorIs(t, err, core.ErrPolicyRevisionConflict)
	require.NotEmpty(t, operationID)

	require.Equal(t, int64(2), env.service.Current().Revision, "CAS 落败后必须重读权威行")
	require.Equal(t, []string{"other.example.com"}, env.service.Current().Domains)
	require.Equal(t, []string{"other.example.com"}, env.policy.AllowedDomains(),
		"重读结果必须同时应用到判定策略实例")
	require.Contains(t, env.logs.String(), "reloaded after revision conflict")
}

// TestNetworkAllowlist_TighteningRevokesMCPHTTP —— 收紧后撤销已建立的 MCP http 连接；
// 放宽不重连。撤销复用策略实例的 `AllowsHost`（单一通配语义）。
// 逆改点：撤销另写一份匹配 / 放宽时重连 → 对应断言红。
func TestNetworkAllowlist_TighteningRevokesMCPHTTP(t *testing.T) {
	mcp := newMCPEnv(t)
	require.NoError(t, mcp.supervisor.Start(context.Background(), allowedHTTP("remote", 5)))

	env := newAllowlistEnv(t, func(d *NetworkAllowlistDeps) { d.Supervisor = mcp.supervisor })
	require.NoError(t, env.service.EnsureSeeded(context.Background(), []string{"mcp.example.com"}))

	_, _, err := env.service.Update(context.Background(), []string{"other.example.com"},
		core.NetworkDomainETag(env.service.Current()), allowlistActor())
	require.NoError(t, err)
	require.Equal(t, 1, mcp.closedCount(), "收紧后连接必须关闭")
	status, _ := mcp.supervisor.Status("remote")
	require.False(t, status.Connected)
	require.Equal(t, mcpStatusAllowlistRevoked, status.Error)
	require.Equal(t, int64(5), status.AppliedVersionID, "已应用版本保留")
	require.Contains(t, env.logs.String(), "revoked disallowed MCP http links")

	created := mcp.createdCount()
	_, _, err = env.service.Update(context.Background(), []string{"mcp.example.com"},
		core.NetworkDomainETag(env.service.Current()), allowlistActor())
	require.NoError(t, err)
	require.Equal(t, created, mcp.createdCount(), "放宽白名单不得自动重连")
	status, _ = mcp.supervisor.Status("remote")
	require.False(t, status.Connected)
}

// TestNetworkAllowlist_CurrentZeroValueBeforeSeed —— 尚未装载时 `Current` 返回零值
// （revision 0 / 无域名 / source=config_seed），web 端口据此如实投影"无可写目标"
// （视图把 revision 0 的 ETag 留空，见 web 层用例）。
// 逆改点：零值被当成 revision 1 → 页面能对一个不存在的修订做条件写，本用例红。
func TestNetworkAllowlist_CurrentZeroValueBeforeSeed(t *testing.T) {
	env := newAllowlistEnv(t, nil)
	live := env.service.Current()
	require.Equal(t, int64(0), live.Revision)
	require.Empty(t, live.Domains)
	require.Equal(t, core.NetworkAllowlistSourceConfigSeed, live.Source())
}
