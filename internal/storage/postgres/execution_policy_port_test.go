//go:build integration

package postgres

// 本文件是执行档位 / 网络域名策略 / 网络例外三个仓储的判卷（颗粒度文档 P15，
// 存储移植工作包）。
// 覆盖：单行表的种一次且不覆盖管理值、修订号 CAS 零写入、操作者经审计行回读、
// 空列表往返、例外稳定排序与重复登记不覆盖理由、删除未命中报 ErrResourceNotFound、
// 以及底层故障不被伪装成业务结论。
//
// 由 SQLite 谱系逐条移植：`internal/storage/execution_policy_store_test.go`、
// `internal/storage/network_policy_store_test.go`、`internal/storage/network_exception_store_test.go`。
// 测试函数名、断言、中文验收点注释与逆改点注释原样保留；只替换夹具
// （`openTempDB` → `portTestDB`）、读写句柄（`db.read` / `db.write` → `db.pool`）与
// 方言（`?` → `$n`；时间列按 BIGINT Unix 毫秒读写）。PG 谱系读写合一为单一连接池，
// 与 SQLite 的读写双池不同：需要"只让读路径失败"的用例改注入读侧故障，见
// TestExecutionPolicyStore_ErrorsAreNotMislabelled 的 seed_after_insert 子用例。
// Ported verbatim from the three SQLite lineage files: function names and assertions
// stay as-is; only the fixture, the handle and the dialect differ.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// policyNow 是移植用例的固定写库时刻（与 SQLite 侧同名变量同一时刻，断言可复算）。
// policyNow is the fixed instant ported cases write with, identical to the SQLite lineage.
var policyNow = time.Date(2026, 10, 24, 8, 30, 0, 0, time.UTC)

// newExecutionPolicyStore 给每个用例一个独立的真库（全量迁移已完成）。
func newExecutionPolicyStore(t *testing.T) (*ExecutionPolicyStore, *DB) {
	t.Helper()
	db := portTestDB(t)
	return NewExecutionPolicyStore(db), db
}

// newPolicyStore 给每个用例一个独立的真库（全量迁移已完成）。
func newPolicyStore(t *testing.T) (*NetworkPolicyStore, *DB) {
	t.Helper()
	db := portTestDB(t)
	return NewNetworkPolicyStore(db), db
}

// newNetworkExceptionStore 给每个用例一个独立的真库。
func newNetworkExceptionStore(t *testing.T) (*NetworkExceptionStore, *DB) {
	t.Helper()
	db := portTestDB(t)
	return NewNetworkExceptionStore(db), db
}

// closeReadPool / closeWritePool 造出真实的查询 / 写入失败。PG 谱系读写合一（只有一个
// `db.pool`），关掉它查询与写入都失败——SQLite 侧"只关一个池"的分工在 PG 上不存在；
// 需要只让读路径失败的用例另行注入，见本文件开头说明。
// closeReadPool / closeWritePool force failures; PG has one pool, so both close the same
// handle. Cases needing a read-only fault inject it differently (see the file header).
func closeReadPool(t *testing.T, db *DB) {
	t.Helper()
	require.NoError(t, db.pool.Close())
}

func closeWritePool(t *testing.T, db *DB) {
	t.Helper()
	require.NoError(t, db.pool.Close())
}

// seedPolicyOperation 写一行 pending 管理操作，供 Update / List 的 operation_id
// 外键式回读用。
func seedPolicyOperation(t *testing.T, db *DB, operationID, actor string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`INSERT INTO management_operations
		   (operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
		    status, created_at)
		 VALUES ($1, $2, $3, $4, $5, 'bearer', '127.0.0.1:5000', 'pending', $6)`,
		operationID, core.ActionNetworkAllowlistUpdate, core.TargetKindNetworkAllowlist,
		core.PolicyScopeHTTPDomains, actor, policyNow.UnixMilli())
	require.NoError(t, err)
}

// TestExecutionPolicyStore_LoadMissingIsNotAnError —— 空表返回 exists=false 与零值
// 策略（不是 `sql.ErrNoRows`）：调用方据此决定是否写入种子。
// 逆改点：把无行当错误返回 → 新建实例永远无法种子，本用例红。
func TestExecutionPolicyStore_LoadMissingIsNotAnError(t *testing.T) {
	store, _ := newExecutionPolicyStore(t)
	policy, exists, err := store.Load(context.Background())
	require.NoError(t, err)
	require.False(t, exists)
	require.Equal(t, core.ExecutionPolicy{}, policy)
}

// TestExecutionPolicyStore_SeedWritesOnceAndNeverOverwrites —— 首次种子落 `revision=1`、
// 操作标识原样保留、无审计行时操作者投影为空串；再次种子返回既有行，**不**覆盖管理台
// 写入的档位。非法档位 / 空操作标识在落库前就被拒绝。
// 逆改点：Seed 用 `INSERT OR REPLACE` → 第二次种子覆盖管理值，本用例红。
func TestExecutionPolicyStore_SeedWritesOnceAndNeverOverwrites(t *testing.T) {
	store, db := newExecutionPolicyStore(t)
	ctx := context.Background()

	_, err := store.Seed(ctx, core.ExecutionProfile("loose"), core.SystemOperationInstanceInit, policyNow)
	require.Error(t, err, "非法档位不得落库")
	require.Contains(t, err.Error(), "invalid profile")

	_, err = store.Seed(ctx, core.ProfileStandard, "", policyNow)
	require.Error(t, err, "空操作标识不得落库")

	seeded, err := store.Seed(ctx, core.ProfileStandard, core.SystemOperationInstanceInit, policyNow)
	require.NoError(t, err)
	require.Equal(t, core.ProfileStandard, seeded.Profile)
	require.Equal(t, int64(1), seeded.Revision)
	require.Equal(t, core.SystemOperationInstanceInit, seeded.OperationID)
	require.Empty(t, seeded.UpdatedBy, "系统流程写入没有审计行，操作者为空串")
	require.True(t, seeded.UpdatedAt.Equal(policyNow))

	// 管理台写入之后再种子：既有行原样返回。
	seedPolicyOperation(t, db, "op-managed", "token:abc123")
	updated, err := store.Update(ctx, core.ProfileStrict, 1, "op-managed", "token:abc123", policyNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Revision)

	again, err := store.Seed(ctx, core.ProfileStandard, core.SystemOperationInstanceInit, policyNow.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, core.ProfileStrict, again.Profile, "已有行时配置种子不得覆盖管理值")
	require.Equal(t, int64(2), again.Revision)
	require.Equal(t, "op-managed", again.OperationID)
}

// TestExecutionPolicyStore_UpdateRevisionCAS —— 修订号 CAS：落后 / 超前 / `< 1` 的期望值
// 一律 `ErrPolicyRevisionConflict` 且**零写入**；成功后修订号 +1、操作者取本次入参。
// 逆改点：UPDATE 不带 `revision = ?` → 落后写入成功，本用例红。
func TestExecutionPolicyStore_UpdateRevisionCAS(t *testing.T) {
	store, _ := newExecutionPolicyStore(t)
	ctx := context.Background()
	_, err := store.Seed(ctx, core.ProfileStandard, core.SystemOperationInstanceInit, policyNow)
	require.NoError(t, err)

	_, err = store.Update(ctx, core.ProfileStrict, 0, "op-zero", "token:abc123", policyNow)
	require.ErrorIs(t, err, core.ErrPolicyRevisionConflict, "期望修订号 < 1 直接拒绝")

	updated, err := store.Update(ctx, core.ProfileStrict, 1, "op-first", "token:abc123", policyNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, core.ProfileStrict, updated.Profile)
	require.Equal(t, int64(2), updated.Revision)
	require.Equal(t, "op-first", updated.OperationID)
	require.Equal(t, "token:abc123", updated.UpdatedBy, "操作者是本次调用的入参")
	require.True(t, updated.UpdatedAt.Equal(policyNow.Add(time.Minute)))

	for _, expected := range []int64{1, 3} {
		_, err = store.Update(ctx, core.ProfileStandard, expected, "op-stale", "token:abc123",
			policyNow.Add(2*time.Minute))
		require.ErrorIs(t, err, core.ErrPolicyRevisionConflict, "期望修订号 %d 必须冲突", expected)
	}

	policy, exists, err := store.Load(ctx)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.ProfileStrict, policy.Profile, "冲突写入必须零写入")
	require.Equal(t, int64(2), policy.Revision)
	require.Equal(t, "op-first", policy.OperationID)
	require.True(t, policy.UpdatedAt.Equal(policyNow.Add(time.Minute)))

	_, err = store.Update(ctx, core.ExecutionProfile("loose"), 2, "op-bad", "token:abc123", policyNow)
	require.Error(t, err, "非法档位不得落库")
	require.False(t, errors.Is(err, core.ErrPolicyRevisionConflict), "形状错误不是修订冲突")
}

// TestExecutionPolicyStore_UpdateWithoutRowConflicts —— 表内无行时更新一律冲突（不是
// 隐式插入）：唯一权威行的建立只有 Seed 一条路径。
// 逆改点：Update 改成 upsert → 未经种子的实例可被管理台"凭空"建行，本用例红。
func TestExecutionPolicyStore_UpdateWithoutRowConflicts(t *testing.T) {
	store, _ := newExecutionPolicyStore(t)
	_, err := store.Update(context.Background(), core.ProfileStrict, 1, "op-one", "token:abc123", policyNow)
	require.ErrorIs(t, err, core.ErrPolicyRevisionConflict)

	_, exists, err := store.Load(context.Background())
	require.NoError(t, err)
	require.False(t, exists, "冲突路径不得留下任何行")
}

// TestExecutionPolicyStore_ActorComesFromAuditRow —— 页面上的操作者来自审计行（按
// `operation_id` 关联），表内不重复存操作者；审计行缺失时投影空串而不是扫描错误。
// 逆改点：改成在档位表里存操作者 → 审计与档位可能分叉，本用例红。
func TestExecutionPolicyStore_ActorComesFromAuditRow(t *testing.T) {
	store, db := newExecutionPolicyStore(t)
	ctx := context.Background()
	_, err := store.Seed(ctx, core.ProfileStandard, core.SystemOperationInstanceInit, policyNow)
	require.NoError(t, err)

	// 故意不写审计行：Update 回读不报错（操作者以入参投影）。
	updated, err := store.Update(ctx, core.ProfileStrict, 1, "op-orphan", "token:feed", policyNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, "token:feed", updated.UpdatedBy)

	// 有审计行时，重启后的 Load 也能把操作者投影出来。
	seedPolicyOperation(t, db, "op-orphan", "token:feed")
	policy, _, err := store.Load(ctx)
	require.NoError(t, err)
	require.Equal(t, "op-orphan", policy.OperationID)
	require.Equal(t, "token:feed", policy.UpdatedBy)
}

// TestExecutionPolicyStore_ErrorsAreNotMislabelled —— 底层池故障带上下文前缀返回，
// 且不得被错认成修订号冲突（否则管理台会把存储故障显示成"请刷新重试"）。
// 逆改点：错误被吞成 nil / 一律包成 ErrPolicyRevisionConflict → 本用例红。
func TestExecutionPolicyStore_ErrorsAreNotMislabelled(t *testing.T) {
	t.Run("load", func(t *testing.T) {
		store, db := newExecutionPolicyStore(t)
		closeReadPool(t, db)
		_, _, err := store.Load(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "load execution policy")
	})

	t.Run("seed_after_insert", func(t *testing.T) {
		store, db := newExecutionPolicyStore(t)
		// PG 谱系读写共用一个连接池，无法像 SQLite 那样"写池可用、读池已关闭"；
		// 改为让 Load 的关联表不可解析：execution_policy 的 INSERT 仍成功（该表对
		// management_operations 没有外键），随后的 Load 必然失败——故障仍只落在读路径。
		// PG has one pool, so the read-only fault is injected by renaming the joined
		// table: the insert still succeeds while the following load cannot run.
		_, err := db.pool.ExecContext(context.Background(),
			`ALTER TABLE management_operations RENAME TO management_operations_unreadable`)
		require.NoError(t, err)
		_, err = store.Seed(context.Background(), core.ProfileStandard, core.SystemOperationInstanceInit, policyNow)
		require.Error(t, err, "写入成功却读不到：如实报错而不是假装成功")
		require.Contains(t, err.Error(), "load execution policy")
	})

	t.Run("seed_write", func(t *testing.T) {
		store, db := newExecutionPolicyStore(t)
		closeWritePool(t, db)
		_, err := store.Seed(context.Background(), core.ProfileStandard, core.SystemOperationInstanceInit, policyNow)
		require.Error(t, err)
		require.Contains(t, err.Error(), "seed execution policy")
	})

	t.Run("update_begin", func(t *testing.T) {
		store, db := newExecutionPolicyStore(t)
		_, err := store.Seed(context.Background(), core.ProfileStandard, core.SystemOperationInstanceInit, policyNow)
		require.NoError(t, err)
		closeWritePool(t, db)
		_, err = store.Update(context.Background(), core.ProfileStrict, 1, "op-x", "token:abc123", policyNow)
		require.Error(t, err)
		require.False(t, errors.Is(err, core.ErrPolicyRevisionConflict), "底层错误不得伪装成冲突")
		require.Contains(t, err.Error(), "begin execution policy write")
	})
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
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
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
	closeWritePool(t, db)

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
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM network_domain_policy`).Scan(&count))
	require.Equal(t, 2, count, "只允许两个作用域各一行，空作用域不得落库")
}

// exceptionFixture 造一条完整例外行（时间取固定时钟，判据不依赖实现自取时钟）。
func exceptionFixture(id, kind, value, note string) core.NetworkException {
	return core.NetworkException{
		ExceptionID: id,
		Kind:        kind,
		Value:       value,
		Note:        note,
		CreatedAt:   policyNow,
		OperationID: "op-exc",
	}
}

// TestNetworkExceptionStore_ListOrderAndLookup —— List 按 `kind` 升序、同 kind 内按
// `value` 升序（与写入顺序无关）；Lookup 命中返回与写入一致的行、未命中 ok=false 且
// 不是错误。
// 逆改点：ORDER BY 改成插入顺序 / Lookup 未命中报错 → 本用例红。
func TestNetworkExceptionStore_ListOrderAndLookup(t *testing.T) {
	store, _ := newNetworkExceptionStore(t)
	ctx := context.Background()

	for _, exc := range []core.NetworkException{
		exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z30", "ip_port", "10.0.0.2:443", "内网直连"),
		exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z31", "host", "b.example.com", "业务回调"),
		exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z32", "host", "a.example.com", "行情接口"),
	} {
		require.NoError(t, store.Create(ctx, exc))
	}

	listed, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 3)
	require.Equal(t, []string{"a.example.com", "b.example.com", "10.0.0.2:443"},
		[]string{listed[0].Value, listed[1].Value, listed[2].Value})
	require.Equal(t, "host", listed[0].Kind)
	require.Equal(t, int64(1), listed[0].Revision, "首写修订号固定为 1")
	require.Equal(t, "op-exc", listed[0].OperationID)
	require.True(t, listed[0].CreatedAt.Equal(policyNow), "时间以 UTC 毫秒读回")
	require.Empty(t, listed[0].CreatedBy, "没有审计行时操作者投影为空串")

	found, ok, err := store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z31")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "b.example.com", found.Value)
	require.Equal(t, "业务回调", found.Note)

	_, ok, err = store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z39")
	require.NoError(t, err)
	require.False(t, ok, "未命中是合法答案，不是错误")
}

// TestNetworkExceptionStore_DuplicatePairNeverOverwrites —— `(kind, value)` 重复登记
// 映射为 `ErrPolicyRevisionConflict`，既有行的理由、标识与时间一字不动。
// 逆改点：改成 upsert / INSERT OR REPLACE → 原有审计理由被悄悄替换，本用例红。
func TestNetworkExceptionStore_DuplicatePairNeverOverwrites(t *testing.T) {
	store, _ := newNetworkExceptionStore(t)
	ctx := context.Background()
	original := exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z33", "host", "hooks.example.net", "原始理由")
	require.NoError(t, store.Create(ctx, original))

	dup := exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z34", "host", "hooks.example.net", "篡改理由")
	err := store.Create(ctx, dup)
	require.ErrorIs(t, err, core.ErrPolicyRevisionConflict)
	require.Contains(t, err.Error(), "hooks.example.net")

	listed, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1, "重复登记不得留下第二行")
	require.Equal(t, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z33", listed[0].ExceptionID)
	require.Equal(t, "原始理由", listed[0].Note)
}

// TestNetworkExceptionStore_CreateRejectsInvalidShape —— 空理由 / 非法 kind 被 schema
// 检出来，且**不得**被错认成重复登记（管理台会据此显示"已存在"）。
// 逆改点：所有写错误一律包成 ErrPolicyRevisionConflict → 本用例红。
func TestNetworkExceptionStore_CreateRejectsInvalidShape(t *testing.T) {
	store, _ := newNetworkExceptionStore(t)
	ctx := context.Background()

	err := store.Create(ctx, exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z35", "host", "a.example.com", ""))
	require.Error(t, err, "放行必须带理由")
	require.False(t, errors.Is(err, core.ErrPolicyRevisionConflict))
	require.Contains(t, err.Error(), "create network exception")

	err = store.Create(ctx, exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z36", "domain", "a.example.com", "非法类别"))
	require.Error(t, err)
	require.False(t, errors.Is(err, core.ErrPolicyRevisionConflict))
}

// TestNetworkExceptionStore_DeleteReportsMissing —— 删除既有行生效；再删同一标识返回
// `core.ErrResourceNotFound`（不是静默成功）。
// 逆改点：按 0 行影响当成功返回 → 页面把"已被别人删掉"显示成删除成功，本用例红。
func TestNetworkExceptionStore_DeleteReportsMissing(t *testing.T) {
	store, _ := newNetworkExceptionStore(t)
	ctx := context.Background()
	require.NoError(t, store.Create(ctx, exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z37",
		"ip_port", "192.0.2.10:8443", "备用出口")))

	require.NoError(t, store.Delete(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z37"))
	_, ok, err := store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z37")
	require.NoError(t, err)
	require.False(t, ok)

	require.ErrorIs(t, store.Delete(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z37"), core.ErrResourceNotFound)
}

// TestNetworkExceptionStore_ActorFromAuditRow —— 写入者来自审计行（按 `operation_id`
// 关联），例外表本身不存操作者。
// 逆改点：改成在例外表里存操作者 → 审计与例外可能分叉，本用例红。
func TestNetworkExceptionStore_ActorFromAuditRow(t *testing.T) {
	store, db := newNetworkExceptionStore(t)
	ctx := context.Background()
	seedPolicyOperation(t, db, "op-exc", "token:abc123")
	require.NoError(t, store.Create(ctx, exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z38",
		"host", "api.example.com", "对账接口")))

	listed, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "token:abc123", listed[0].CreatedBy)

	found, ok, err := store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z38")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "token:abc123", found.CreatedBy)
}

// TestNetworkExceptionStore_ErrorPathsKeepClassification —— 池故障带上下文前缀返回，
// 且不得被误判为"未找到"或"重复登记"。
// 逆改点：错误被吞成 nil / ok=false → 页面把存储故障显示成"例外不存在"，本用例红。
func TestNetworkExceptionStore_ErrorPathsKeepClassification(t *testing.T) {
	ctx := context.Background()

	t.Run("list", func(t *testing.T) {
		store, db := newNetworkExceptionStore(t)
		closeReadPool(t, db)
		_, err := store.List(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "list network exceptions")
	})

	t.Run("lookup", func(t *testing.T) {
		store, db := newNetworkExceptionStore(t)
		closeReadPool(t, db)
		_, ok, err := store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z30")
		require.Error(t, err, "存储故障不得伪装成未命中")
		require.False(t, ok)
		require.Contains(t, err.Error(), "lookup network exception")
	})

	t.Run("create_and_delete", func(t *testing.T) {
		store, db := newNetworkExceptionStore(t)
		closeWritePool(t, db)
		err := store.Create(ctx, exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z30",
			"host", "a.example.com", "理由"))
		require.Error(t, err)
		require.False(t, errors.Is(err, core.ErrPolicyRevisionConflict))
		require.Contains(t, err.Error(), "create network exception")

		err = store.Delete(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z30")
		require.Error(t, err)
		require.False(t, errors.Is(err, core.ErrResourceNotFound))
		require.Contains(t, err.Error(), "delete network exception")
	})
}

// TestNetworkExceptionStore_CreatedAtIsStoredAsMillis —— 创建时间按 Unix 毫秒落库，
// 读回仍是同一时刻（UTC）：页面按它排序 / 展示，不得随时区漂移。
func TestNetworkExceptionStore_CreatedAtIsStoredAsMillis(t *testing.T) {
	store, db := newNetworkExceptionStore(t)
	ctx := context.Background()
	moment := time.Date(2026, 3, 1, 23, 59, 30, 250_000_000, time.FixedZone("UTC+8", 8*3600))
	require.NoError(t, store.Create(ctx, core.NetworkException{
		ExceptionID: "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
		Kind:        "host",
		Value:       "tz.example.com",
		Note:        "时区往返",
		CreatedAt:   moment,
		OperationID: "op-tz",
	}))

	var stored int64
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT created_at FROM network_exceptions WHERE exception_id = $1`,
		"exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A").Scan(&stored))
	require.Equal(t, moment.UnixMilli(), stored)

	found, ok, err := store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, found.CreatedAt.Equal(moment))
	require.Equal(t, time.UTC, found.CreatedAt.Location())
}
