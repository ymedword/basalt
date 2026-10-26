package storage

// 本文件判卷 `execution_policy`（schema v6 单行表）的仓储面：无行是合法答案、种子只写
// 一次且永不覆盖管理值、修订号 CAS 冲突零写入、操作者经审计行回读、底层故障不被伪装成
// 修订冲突。档位的**语义**由 core 的用例判，这里只判 SQL 面的形状与失败分类。
// These tests pin the single-row execution policy store: seed-once, revision CAS with
// zero writes on conflict, actor join, and honest error classification.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// newExecutionPolicyStore 给每个用例一个独立的真库（全量迁移已完成）。
func newExecutionPolicyStore(t *testing.T) (*ExecutionPolicyStore, *DB) {
	t.Helper()
	db := openTempDB(t)
	return NewExecutionPolicyStore(db), db
}

// closeReadPool / closeWritePool 关掉对应的池，造出真实的查询 / 写入失败；
// 其余存储用例文件共用这两个助手（同包测试一起编译）。
// closeReadPool / closeWritePool force real query and write failures; the other store
// test files reuse them.
func closeReadPool(t *testing.T, db *DB) {
	t.Helper()
	require.NoError(t, db.read.Close())
}

func closeWritePool(t *testing.T, db *DB) {
	t.Helper()
	require.NoError(t, db.write.Close())
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
		closeReadPool(t, db)
		_, err := store.Seed(context.Background(), core.ProfileStandard, core.SystemOperationInstanceInit, policyNow)
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
