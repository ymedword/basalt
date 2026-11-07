//go:build integration

package app

// 本文件是 US-24 §2.3.9「生命周期写入」在 app 装配面的判卷：索引写入口的代次单调
// 规则——本地代次低于索引不得回退写入，同代次只接受逐项完全一致的内容（零写入拒绝）。
// 判据走真实仓储（`postgres.ResourceStore`）与真实数据库，不用替身：守卫写在 SQL
// 事务里，替身只能证明调用顺序，证明不了"索引一个字节都没动"。
// These tests judge the monotonic-generation rule of the lifecycle index write guard
// against a real store and a real database; a fake could only prove call order.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// seedIndexOperation 为 `agent_lifecycle.operation_id` 的外键准备一行操作台账。
// seedIndexOperation seeds the operation row the index foreign key requires.
func seedIndexOperation(t *testing.T, db *postgres.DB, operationID string) {
	t.Helper()
	require.NoError(t, postgres.NewManagementStore(db).BeginOperation(context.Background(), core.OperationRecord{
		OperationID:  operationID,
		Action:       "system.baseline_v2",
		TargetKind:   "system",
		TargetID:     "system",
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       "test",
		Status:       core.OperationPending,
		CreatedAt:    time.Now(),
	}))
}

// lifecycleIndexFixture 是一次索引写入的完整六列元组样本（§2.3.9 的比较元组）。
// lifecycleIndexFixture is one complete six-column sample of the comparison tuple.
func lifecycleIndexFixture(operationID string) core.LifecycleRecord {
	return core.LifecycleRecord{
		SchemaVersion: 1,
		Agent:         "writer",
		AgentUID:      core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"),
		State:         core.AgentActive,
		Generation:    3,
		UpdatedAt:     time.UnixMilli(1_700_000_000_000).UnixMilli(),
		OperationID:   operationID,
		Checksum:      "sha256:" + strings.Repeat("a", 64),
	}
}

// TestLifecycleIndex_SameGenerationOnlyIdenticalWrite：同代次只接受逐项一致的内容——
// 一致即幂等成功（不刷新 updated_at），任一列不同即 409 语义的 mismatch 且**零写入**。
// 中文验收点：同代次异内容拒绝、逐项一致幂等。
func TestLifecycleIndex_SameGenerationOnlyIdenticalWrite(t *testing.T) {
	db := pgtest.NewTestDB(t)
	seedIndexOperation(t, db, "op-li-same")
	store := postgres.NewResourceStore(db)
	ctx := context.Background()

	base := lifecycleIndexFixture("op-li-same")
	require.NoError(t, store.PutLifecycleIndex(ctx, base, true, ""))

	// 同一内容重放（updated_at 不同）：幂等成功，索引的 updated_at 保持首次值。
	replay := base
	replay.UpdatedAt += 86_400_000
	require.NoError(t, store.PutLifecycleIndex(ctx, replay, true, ""))
	got, valid, errMsg, err := store.ReadLifecycleIndex(ctx, "writer")
	require.NoError(t, err)
	require.True(t, valid)
	require.Empty(t, errMsg)
	require.Equal(t, core.AgentActive, got.State)
	require.Equal(t, int64(3), got.Generation)
	require.Equal(t, base.Checksum, got.Checksum)
	require.Equal(t, base.OperationID, got.OperationID)
	require.Equal(t, base.UpdatedAt, got.UpdatedAt, "幂等重放不得刷新 updated_at")

	// 同代次、任一列不同：拒绝且零写入。逐列覆盖比较元组（generation 相同是前提）。
	changes := map[string]func(*core.LifecycleRecord){
		"state":        func(r *core.LifecycleRecord) { r.State = core.AgentDisabled },
		"state_file":   func(r *core.LifecycleRecord) { r.Checksum = "sha256:" + strings.Repeat("b", 64) },
		"agent_uid":    func(r *core.LifecycleRecord) { r.AgentUID = "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B" },
		"operation_id": func(r *core.LifecycleRecord) { r.OperationID = "op-li-other" },
	}
	seedIndexOperation(t, db, "op-li-other")
	for column, mutate := range changes {
		changed := base
		mutate(&changed)
		require.ErrorIs(t, store.PutLifecycleIndex(ctx, changed, true, ""),
			core.ErrAgentLifecycleMismatch, "同代次异内容必须拒绝：%s", column)

		after, afterValid, _, err := store.ReadLifecycleIndex(ctx, "writer")
		require.NoError(t, err)
		require.True(t, afterValid)
		require.Equal(t, core.AgentActive, after.State, "拒绝路径零写入：%s", column)
		require.Equal(t, int64(3), after.Generation, "拒绝路径零写入：%s", column)
		require.Equal(t, base.Checksum, after.Checksum, "拒绝路径零写入：%s", column)
		require.Equal(t, base.OperationID, after.OperationID, "拒绝路径零写入：%s", column)
	}
}

// TestLifecycleIndex_StaleWorkerCannotRollback：旧 Worker 的代次低于索引时写入被拒，
// 索引保持新代次；本地代次更高可写（管理操作的单调修复路径），写入只增不回退。
// 中文验收点：旧代次写不回退、代次只增。
func TestLifecycleIndex_StaleWorkerCannotRollback(t *testing.T) {
	db := pgtest.NewTestDB(t)
	seedIndexOperation(t, db, "op-li-new")
	seedIndexOperation(t, db, "op-li-old")
	store := postgres.NewResourceStore(db)
	ctx := context.Background()

	advanced := lifecycleIndexFixture("op-li-new")
	advanced.Generation = 5
	require.NoError(t, store.PutLifecycleIndex(ctx, advanced, true, ""))

	// 旧 Worker 手里的代次 4：内容再"新"也不得把索引写回去。
	stale := advanced
	stale.Generation = 4
	stale.State = core.AgentDisabled
	stale.OperationID = "op-li-old"
	require.ErrorIs(t, store.PutLifecycleIndex(ctx, stale, true, ""), core.ErrAgentLifecycleStale)

	after, valid, _, err := store.ReadLifecycleIndex(ctx, "writer")
	require.NoError(t, err)
	require.True(t, valid)
	require.Equal(t, int64(5), after.Generation, "旧代次写入必须零写入")
	require.Equal(t, core.AgentActive, after.State, "旧代次写入必须零写入")
	require.Equal(t, "op-li-new", after.OperationID)

	// 管理操作的单调修复：本地代次更高时允许推进索引（是否准入由调用方判定，P-17）。
	repaired := advanced
	repaired.Generation = 6
	repaired.OperationID = "op-li-old"
	require.NoError(t, store.PutLifecycleIndex(ctx, repaired, true, ""))

	after, valid, _, err = store.ReadLifecycleIndex(ctx, "writer")
	require.NoError(t, err)
	require.True(t, valid)
	require.Equal(t, int64(6), after.Generation)
	require.Equal(t, "op-li-old", after.OperationID)

	// 代次 5 的写入此时同样是旧代次：单调规则对"修复后"的迟到写者一样生效。
	require.ErrorIs(t, store.PutLifecycleIndex(ctx, advanced, true, ""), core.ErrAgentLifecycleStale)
}

// TestLifecycleBarrier_RestoreOnlyUndoesOwnDisable 判归档屏障的**补偿边界**
// （review ② 复现项 2 / FR-24-41）：超时回滚只能用状态文件把**本次屏障留下的那一行**
// 写回，CAS 必须同时判操作身份（`operation_id = 本次屏障`）与屏障期代次
// （`generation = 屏障期观测值`）。
//
//   - 只判 `state='disabled'`：后续一次停用同样把行写成 disabled，补偿会把它回写成
//     `active / generation=3`，覆盖别人的 `disabled / generation=4`（review 实测读数：
//     `restored=true` 且旧元组覆盖新停用）。
//   - 只判身份：同一归档操作的后续推进保留 `operation_id` 但递增代次，补偿仍会覆盖。
//
// 逆改点：把 `RestoreLifecycleBarrier` 的 WHERE 收窄回 `state='disabled'` → 第二、
// 三个子场景 `restored=true` 且新元组被旧元组覆盖，断言红。
func TestLifecycleBarrier_RestoreOnlyUndoesOwnDisable(t *testing.T) {
	// ① 正常补偿：身份与代次都匹配时写回状态文件元组（正向路径不能被 CAS 误杀，
	// 否则归档超时就再也回滚不了）。
	t.Run("own_barrier_restores", func(t *testing.T) {
		db := pgtest.NewTestDB(t)
		seedIndexOperation(t, db, "op-lb-initial")
		seedIndexOperation(t, db, "op-lb-archive")
		store := postgres.NewResourceStore(db)
		ctx := context.Background()

		before := lifecycleIndexFixture("op-lb-initial")
		require.NoError(t, store.PutLifecycleIndex(ctx, before, true, ""))
		applied, err := store.SetLifecycleBarrier(ctx, "writer", "op-lb-archive", time.Now())
		require.NoError(t, err)
		require.True(t, applied, "屏障必须命中 active 行")

		restored, err := store.RestoreLifecycleBarrier(ctx, before, "op-lb-archive", before.Generation)
		require.NoError(t, err)
		require.True(t, restored, "本次屏障仍在原处：补偿必须生效")

		got, valid, _, err := store.ReadLifecycleIndex(ctx, "writer")
		require.NoError(t, err)
		require.True(t, valid)
		require.Equal(t, core.AgentActive, got.State)
		require.Equal(t, int64(3), got.Generation)
		require.Equal(t, "op-lb-initial", got.OperationID)
		require.Equal(t, before.Checksum, got.Checksum)
	})

	// ② 后续停用（不同操作身份、代次 +1）：补偿必须放弃，不得把新停用覆盖回旧代次。
	t.Run("later_disable_is_preserved", func(t *testing.T) {
		db := pgtest.NewTestDB(t)
		seedIndexOperation(t, db, "op-lb-initial")
		seedIndexOperation(t, db, "op-lb-archive")
		seedIndexOperation(t, db, "op-lb-disable")
		store := postgres.NewResourceStore(db)
		ctx := context.Background()

		before := lifecycleIndexFixture("op-lb-initial")
		require.NoError(t, store.PutLifecycleIndex(ctx, before, true, ""))
		applied, err := store.SetLifecycleBarrier(ctx, "writer", "op-lb-archive", time.Now())
		require.NoError(t, err)
		require.True(t, applied)

		later := before
		later.State = core.AgentDisabled
		later.Generation = 4
		later.OperationID = "op-lb-disable"
		later.Checksum = "sha256:" + strings.Repeat("c", 64)
		require.NoError(t, store.PutLifecycleIndex(ctx, later, true, ""),
			"后续管理操作把同一行推进到 disabled / generation=4")

		restored, err := store.RestoreLifecycleBarrier(ctx, before, "op-lb-archive", before.Generation)
		require.NoError(t, err)
		require.False(t, restored, "行已不是本次屏障留下的状态：补偿必须放弃")

		got, valid, _, err := store.ReadLifecycleIndex(ctx, "writer")
		require.NoError(t, err)
		require.True(t, valid)
		require.Equal(t, core.AgentDisabled, got.State, "后续停用不得被覆盖")
		require.Equal(t, int64(4), got.Generation, "代次不得被回写")
		require.Equal(t, "op-lb-disable", got.OperationID)
		require.Equal(t, later.Checksum, got.Checksum)
	})

	// ③ 同一归档操作推进了代次（身份相同）：代次 CAS 拦下补偿，行保持推进后的值。
	t.Run("same_operation_advanced_generation_is_preserved", func(t *testing.T) {
		db := pgtest.NewTestDB(t)
		seedIndexOperation(t, db, "op-lb-initial")
		seedIndexOperation(t, db, "op-lb-archive")
		store := postgres.NewResourceStore(db)
		ctx := context.Background()

		before := lifecycleIndexFixture("op-lb-initial")
		require.NoError(t, store.PutLifecycleIndex(ctx, before, true, ""))
		applied, err := store.SetLifecycleBarrier(ctx, "writer", "op-lb-archive", time.Now())
		require.NoError(t, err)
		require.True(t, applied)

		advanced := before
		advanced.State = core.AgentDisabled
		advanced.Generation = 4
		advanced.OperationID = "op-lb-archive"
		advanced.Checksum = "sha256:" + strings.Repeat("d", 64)
		require.NoError(t, store.PutLifecycleIndex(ctx, advanced, true, ""))

		restored, err := store.RestoreLifecycleBarrier(ctx, before, "op-lb-archive", before.Generation)
		require.NoError(t, err)
		require.False(t, restored, "屏障期代次已被推进：补偿必须放弃")

		got, valid, _, err := store.ReadLifecycleIndex(ctx, "writer")
		require.NoError(t, err)
		require.True(t, valid)
		require.Equal(t, int64(4), got.Generation)
		require.Equal(t, advanced.Checksum, got.Checksum)
	})
}
