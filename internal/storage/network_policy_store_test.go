package storage

// 本文件判卷 `network_domain_policy`（schema v5 单行表，US-12 R8 裁决 2）：种子只在
// 无行时写入、管理写以修订号 CAS、操作者经审计行回读、空格 / 换行往返与空列表。
// These tests pin the authoritative allowlist row: seed-once, revision CAS, actor
// join, and the empty-list round trip.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

var policyNow = time.Date(2026, 10, 24, 8, 30, 0, 0, time.UTC)

// newPolicyStore 给每个用例一个独立的真库（全量迁移已完成）。
func newPolicyStore(t *testing.T) (*NetworkPolicyStore, *DB) {
	t.Helper()
	db := openTempDB(t)
	return NewNetworkPolicyStore(db), db
}

// seedPolicyOperation 写一行 pending 管理操作，供 Update 的 operation_id 外键式回读用。
func seedPolicyOperation(t *testing.T, db *DB, operationID, actor string) {
	t.Helper()
	_, err := db.write.ExecContext(context.Background(),
		`INSERT INTO management_operations
		   (operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
		    status, created_at)
		 VALUES (?, ?, ?, ?, ?, 'bearer', '127.0.0.1:5000', 'pending', ?)`,
		operationID, core.ActionNetworkAllowlistUpdate, core.TargetKindNetworkAllowlist,
		core.PolicyScopeHTTPDomains, actor, policyNow.UnixMilli())
	require.NoError(t, err)
}

// TestNetworkPolicyStore_LoadMissingIsNotAnError —— 空表返回 exists=false 且零值策略
// （不是 `sql.ErrNoRows`）：调用方据此决定是否种子。
// 逆改点：把无行当错误返回 → 首次启动永远无法种子，本用例红。
func TestNetworkPolicyStore_LoadMissingIsNotAnError(t *testing.T) {
	store, _ := newPolicyStore(t)
	policy, exists, err := store.Load(context.Background(), core.PolicyScopeHTTPDomains)
	require.NoError(t, err)
	require.False(t, exists)
	require.Equal(t, core.NetworkDomainPolicy{}, policy)
}

// TestNetworkPolicyStore_SeedOnceAndConfigNeverWins —— 首次种子落库 revision=1、
// operation_id 为空（= 种子来源）、写入者为空串；再次 Seed 原样返回既有行，
// **不**覆盖管理台写入的值。
// 逆改点：Seed 用 `INSERT OR REPLACE` → 第二次种子覆盖管理值，本用例红。
func TestNetworkPolicyStore_SeedOnceAndConfigNeverWins(t *testing.T) {
	store, db := newPolicyStore(t)
	seeded, err := store.Seed(context.Background(), core.PolicyScopeHTTPDomains, []string{"api.example.com", "hooks.example.net"}, policyNow)
	require.NoError(t, err)
	require.Equal(t, core.PolicyScopeHTTPDomains, seeded.Scope)
	require.Equal(t, []string{"api.example.com", "hooks.example.net"}, seeded.Domains)
	require.Equal(t, int64(1), seeded.Revision)
	require.True(t, seeded.Seeded(), "种子的 operation_id 为空")
	require.Empty(t, seeded.UpdatedBy)
	require.True(t, seeded.UpdatedAt.Equal(policyNow))

	seedPolicyOperation(t, db, "op-managed", "token:abc123")
	updated, err := store.Update(context.Background(), core.PolicyScopeHTTPDomains, []string{"managed.example.com"}, 1,
		"op-managed", "token:abc123", policyNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Revision)

	again, err := store.Seed(context.Background(), core.PolicyScopeHTTPDomains, []string{"config.example.com"}, policyNow.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, []string{"managed.example.com"}, again.Domains, "已有行时配置种子不得覆盖")
	require.Equal(t, int64(2), again.Revision)
}

// TestNetworkPolicyStore_UpdateRevisionCAS —— 修订号 CAS：期望值落后 → 冲突且**零写入**
// （域名、修订号、时间、操作标识都不变）；期望值超前同样冲突；`< 1` 直接拒绝。
// 逆改点：UPDATE 不带 `revision = ?` → 落后写入成功，本用例红。
func TestNetworkPolicyStore_UpdateRevisionCAS(t *testing.T) {
	store, db := newPolicyStore(t)
	_, err := store.Seed(context.Background(), core.PolicyScopeHTTPDomains, []string{"api.example.com"}, policyNow)
	require.NoError(t, err)
	seedPolicyOperation(t, db, "op-first", "token:abc123")

	updated, err := store.Update(context.Background(), core.PolicyScopeHTTPDomains, []string{"hooks.example.net"}, 1,
		"op-first", "token:abc123", policyNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Revision)

	for _, expected := range []int64{1, 3} {
		_, err = store.Update(context.Background(), core.PolicyScopeHTTPDomains, []string{"evil.example.org"}, expected,
			"op-stale", "token:abc123", policyNow.Add(2*time.Minute))
		require.ErrorIs(t, err, core.ErrPolicyRevisionConflict, "期望修订号 %d 必须冲突", expected)
	}
	_, err = store.Update(context.Background(), core.PolicyScopeHTTPDomains, []string{"evil.example.org"}, 0,
		"op-zero", "token:abc123", policyNow)
	require.ErrorIs(t, err, core.ErrPolicyRevisionConflict)

	policy, exists, err := store.Load(context.Background(), core.PolicyScopeHTTPDomains)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, []string{"hooks.example.net"}, policy.Domains, "冲突写入必须零写入")
	require.Equal(t, int64(2), policy.Revision)
	require.Equal(t, "op-first", policy.OperationID)
	require.Equal(t, "token:abc123", policy.UpdatedBy)
	require.True(t, policy.UpdatedAt.Equal(policyNow.Add(time.Minute)))
}

// TestNetworkPolicyStore_UpdateWithoutRowConflicts —— 表内无行时更新一律冲突（不是
// 隐式插入）：唯一权威行的建立只有 `Seed` 一条路径。
// 逆改点：Update 改成 upsert → 未经种子的实例可被管理台"凭空"建行，本用例红。
func TestNetworkPolicyStore_UpdateWithoutRowConflicts(t *testing.T) {
	store, _ := newPolicyStore(t)
	_, err := store.Update(context.Background(), core.PolicyScopeHTTPDomains, []string{"a.example.com"}, 1,
		"op-one", "token:abc123", policyNow)
	require.ErrorIs(t, err, core.ErrPolicyRevisionConflict)

	_, exists, err := store.Load(context.Background(), core.PolicyScopeHTTPDomains)
	require.NoError(t, err)
	require.False(t, exists, "冲突路径不得留下任何行")
}

// TestNetworkPolicyStore_EmptyListRoundTrip —— 清空白名单是合法写入：落库为空串，
// 读回是**非 nil** 的空切片（页面按 `[]` 输出，不是 null）。
// 逆改点：空列表写成 "[]" 或读回 nil → JSON 出现 null，本用例红。
func TestNetworkPolicyStore_EmptyListRoundTrip(t *testing.T) {
	store, db := newPolicyStore(t)
	_, err := store.Seed(context.Background(), core.PolicyScopeHTTPDomains, []string{"api.example.com"}, policyNow)
	require.NoError(t, err)
	seedPolicyOperation(t, db, "op-clear", "local_trust")

	cleared, err := store.Update(context.Background(), core.PolicyScopeHTTPDomains, []string{}, 1, "op-clear", "local_trust",
		policyNow.Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, cleared.Domains)
	require.Empty(t, cleared.Domains)

	policy, _, err := store.Load(context.Background(), core.PolicyScopeHTTPDomains)
	require.NoError(t, err)
	require.NotNil(t, policy.Domains)
	require.Empty(t, policy.Domains)
}

// TestNetworkPolicyStore_ActorComesFromAuditRow —— 页面上的写入者来自审计行（按
// `operation_id` 关联），表内不重复存操作者；审计行缺失时投影空串而不是 NULL 扫描错误。
// 逆改点：改成在策略表里存操作者 → 审计与策略可能分叉，本用例红。
func TestNetworkPolicyStore_ActorComesFromAuditRow(t *testing.T) {
	store, db := newPolicyStore(t)
	_, err := store.Seed(context.Background(), core.PolicyScopeHTTPDomains, []string{"api.example.com"}, policyNow)
	require.NoError(t, err)

	// 故意不写审计行：LEFT JOIN 取空串，Update 回读不报错（操作者以入参投影）。
	updated, err := store.Update(context.Background(), core.PolicyScopeHTTPDomains, []string{"b.example.com"}, 1,
		"op-orphan", "token:feed", policyNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, "token:feed", updated.UpdatedBy, "本次调用的操作者以入参为准")

	// 有审计行时，重启后的 Load 也能把操作者投影出来。
	seedPolicyOperation(t, db, "op-orphan", "token:feed")
	policy, _, err := store.Load(context.Background(), core.PolicyScopeHTTPDomains)
	require.NoError(t, err)
	require.Equal(t, "op-orphan", policy.OperationID)
	require.Equal(t, "token:feed", policy.UpdatedBy)
}

// TestNetworkPolicyStore_SingleRowOnly —— 表只有一行：再次 Seed 与多次 Update 之后
// `policy_scope` 仍只有 `http` 一条，历史值不堆积。
// 逆改点：Update 改成插入新行 → 行数变多，本用例红。
func TestNetworkPolicyStore_SingleRowOnly(t *testing.T) {
	store, db := newPolicyStore(t)
	_, err := store.Seed(context.Background(), core.PolicyScopeHTTPDomains, []string{"a.example.com"}, policyNow)
	require.NoError(t, err)
	for i, opID := range []string{"op-1", "op-2", "op-3"} {
		seedPolicyOperation(t, db, opID, "token:abc123")
		_, err := store.Update(context.Background(), core.PolicyScopeHTTPDomains, []string{"a.example.com"}, int64(i+1),
			opID, "token:abc123", policyNow.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
	}
	_, err = store.Seed(context.Background(), core.PolicyScopeHTTPDomains, []string{"z.example.com"}, policyNow)
	require.NoError(t, err)

	var count int
	require.NoError(t, db.read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM network_domain_policy`).Scan(&count))
	require.Equal(t, 1, count)
}

// TestNetworkPolicyStore_NonConflictErrorsAreNotMislabelled —— 底层错误带上下文前缀
// 返回，且不得被错认成修订号冲突（否则管理台会把存储故障显示成"请刷新重试"）。
// 逆改点：错误被吞成 nil / 一律包成 ErrPolicyRevisionConflict → 本用例红。
func TestNetworkPolicyStore_NonConflictErrorsAreNotMislabelled(t *testing.T) {
	store, db := newPolicyStore(t)
	_, err := store.Seed(context.Background(), core.PolicyScopeHTTPDomains, []string{"a.example.com"}, policyNow)
	require.NoError(t, err)
	require.NoError(t, db.write.Close())

	_, err = store.Update(context.Background(), core.PolicyScopeHTTPDomains, []string{"b.example.com"}, 1,
		"op-closed", "token:abc123", policyNow)
	require.Error(t, err)
	require.False(t, errors.Is(err, core.ErrPolicyRevisionConflict), "底层错误不得伪装成冲突")
	require.Contains(t, err.Error(), "network domain policy")
}

// TestNetworkPolicyStore_ScopesIndependent —— 白名单与禁止域名表共用一张表的两行：
// 各自的种子、修订号与 CAS 互不影响；空作用域被拒绝而不是写出幽灵行。
// 逆改点：两个作用域读写同一行 → 保存禁止域名会覆盖白名单，本用例红。
func TestNetworkPolicyStore_ScopesIndependent(t *testing.T) {
	store, db := newPolicyStore(t)
	ctx := context.Background()

	allowed, err := store.Seed(ctx, core.PolicyScopeHTTPDomains, []string{"api.example.com"}, policyNow)
	require.NoError(t, err)
	require.Equal(t, core.PolicyScopeHTTPDomains, allowed.Scope)
	require.Equal(t, int64(1), allowed.Revision)

	denied, err := store.Seed(ctx, core.PolicyScopeHTTPDeniedDomains, []string{"ads.example.net"}, policyNow)
	require.NoError(t, err)
	require.Equal(t, core.PolicyScopeHTTPDeniedDomains, denied.Scope)
	require.Equal(t, []string{"ads.example.net"}, denied.Domains)
	require.Equal(t, int64(1), denied.Revision, "两个作用域各自从 revision=1 起")

	// 只改禁止域名表：白名单行必须原样不动。
	seedPolicyOperation(t, db, "op-denied", "token:abc123")
	updated, err := store.Update(ctx, core.PolicyScopeHTTPDeniedDomains,
		[]string{"ads.example.net", "tracker.example.org"}, 1, "op-denied", "token:abc123", policyNow)
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Revision)

	reloadedAllowed, exists, err := store.Load(ctx, core.PolicyScopeHTTPDomains)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, []string{"api.example.com"}, reloadedAllowed.Domains, "改禁止域名不得动白名单")
	require.Equal(t, int64(1), reloadedAllowed.Revision)

	// 作用域隔离也意味着各自的 CAS 独立：拿白名单的修订号改禁止域名表必然不匹配。
	_, err = store.Update(ctx, core.PolicyScopeHTTPDeniedDomains, []string{"x.example.com"}, 1,
		"op-denied", "token:abc123", policyNow)
	require.ErrorIs(t, err, core.ErrPolicyRevisionConflict)

	// 空作用域：拒绝，且不留下任何行。
	_, _, err = store.Load(ctx, "")
	require.Error(t, err)
	_, err = store.Seed(ctx, "", []string{"ghost.example.com"}, policyNow)
	require.Error(t, err)

	var count int
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM network_domain_policy`).Scan(&count))
	require.Equal(t, 2, count, "只允许两个作用域各一行，空作用域不得落库")
}
