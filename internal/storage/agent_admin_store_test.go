package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `agent_archives` 归档记录仓储的判卷（颗粒度文档 §3.1.6，check
// `012-storage-admin`）：归档行与操作终态同事务、重复归档标识不覆盖、
// 恢复只落一次、读面与清单顺序确定。
// These tests pin the archive repository: one transaction for the row and the
// operation terminal state, no overwrite on a duplicate id, a single restore,
// and deterministic read/list shapes.

// archiveAt 是判卷统一的归档时刻（与 fixedNow 同一读数，UTC 毫秒）。
var archiveAt = time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)

// sampleArchive 造一行形状完整的归档记录；调用方按用例改字段。
// sampleArchive builds a complete archive row the caller may adjust.
func sampleArchive(archiveID, agentName, opID string) core.AgentArchiveRecord {
	return core.AgentArchiveRecord{
		ArchiveID:    archiveID,
		AgentName:    agentName,
		ArchivedAt:   archiveAt.UnixMilli(),
		ContentHash:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ManifestJSON: `[{"path":"AGENT.md","sha256":"aaa","size":7}]`,
		OperationID:  opID,
	}
}

// TestAgentAdminStore_InsertArchiveWritesRow：字段逐列落库、读回一致；
// 形状不合格的记录在任何 SQL 之前被拒（零行落库）。
// TestAgentAdminStore_InsertArchiveWritesRow pins the written columns and the shape gate.
func TestAgentAdminStore_InsertArchiveWritesRow(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-arch-1")
	store := NewAgentAdminStore(db)

	rec := sampleArchive("digest-agent-20261016-093000", "digest-agent", "op-arch-1")
	require.NoError(t, store.InsertArchive(context.Background(), rec, "op-arch-1"))

	got, err := store.ArchiveByID(context.Background(), rec.ArchiveID)
	require.NoError(t, err)
	require.Equal(t, rec.ArchiveID, got.ArchiveID)
	require.Equal(t, rec.AgentName, got.AgentName)
	require.Equal(t, rec.ArchivedAt, got.ArchivedAt)
	require.Equal(t, rec.ContentHash, got.ContentHash)
	require.Equal(t, rec.ManifestJSON, got.ManifestJSON)
	require.Equal(t, "op-arch-1", got.OperationID)
	require.Nil(t, got.RestoredAt)
	require.Empty(t, got.RestoredAgentName)

	// 形状判据：六类不合格输入全部在写事务之前拒绝，库中不发生任何写入。
	bad := []struct {
		name string
		rec  core.AgentArchiveRecord
	}{
		{"missing_archive_id", sampleArchive("", "digest-agent", "op-arch-1")},
		{"missing_agent_name", sampleArchive("x-20261016-093000", "", "op-arch-1")},
		{"non_positive_time", func() core.AgentArchiveRecord {
			r := sampleArchive("x-20261016-093000", "x", "op-arch-1")
			r.ArchivedAt = 0
			return r
		}()},
		{"missing_hash", func() core.AgentArchiveRecord {
			r := sampleArchive("x-20261016-093000", "x", "op-arch-1")
			r.ContentHash = ""
			return r
		}()},
		{"missing_manifest", func() core.AgentArchiveRecord {
			r := sampleArchive("x-20261016-093000", "x", "op-arch-1")
			r.ManifestJSON = ""
			return r
		}()},
		{"restored_fact_on_insert", func() core.AgentArchiveRecord {
			r := sampleArchive("x-20261016-093000", "x", "op-arch-1")
			restored := archiveAt.UnixMilli()
			r.RestoredAt = &restored
			return r
		}()},
	}
	for _, tc := range bad {
		require.Error(t, store.InsertArchive(context.Background(), tc.rec, "op-arch-1"), tc.name)
	}
	// 空操作标识同样被拒（与记录形状无关的独立前置条件）。
	require.Error(t, store.InsertArchive(context.Background(),
		sampleArchive("y-20261016-093000", "y", ""), ""))

	list, err := store.ListArchives(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, rec.ArchiveID, list[0].ArchiveID)
}

// TestAgentAdminStore_InsertArchiveFinishesOperation：归档行与操作终态同事务——
// 插入成功后操作行已是 success，finished_at 取归档时刻（同一行事实的两种投影）。
// TestAgentAdminStore_InsertArchiveFinishesOperation pins the shared transaction.
func TestAgentAdminStore_InsertArchiveFinishesOperation(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-arch-2")
	store := NewAgentAdminStore(db)

	require.NoError(t, store.InsertArchive(context.Background(),
		sampleArchive("digest-agent-20261016-093000", "digest-agent", "op-arch-2"), "op-arch-2"))

	op, err := NewManagementStore(db).GetOperation(context.Background(), "op-arch-2")
	require.NoError(t, err)
	require.Equal(t, core.OperationSuccess, op.Status)
	require.True(t, op.FinishedAt.Equal(archiveAt), "finished_at=%v, want %v", op.FinishedAt, archiveAt)
}

// TestAgentAdminStore_InsertArchiveRollsBackOnMissingOperation：操作行不存在时，
// `agent_archives.operation_id` 的外键在 INSERT 处即拒绝（早于 `finishOperationTx`），
// 归档行零落库——与"终态写失败整笔回滚"是同一个 fail-closed 结论。
// TestAgentAdminStore_InsertArchiveRollsBackOnMissingOperation pins the fail-closed write.
func TestAgentAdminStore_InsertArchiveRollsBackOnMissingOperation(t *testing.T) {
	db := openTempDB(t)
	store := NewAgentAdminStore(db)

	err := store.InsertArchive(context.Background(),
		sampleArchive("ghost-agent-20261016-093000", "ghost-agent", "op-missing"), "op-missing")
	require.Error(t, err)

	_, readErr := store.ArchiveByID(context.Background(), "ghost-agent-20261016-093000")
	require.ErrorIs(t, readErr, core.ErrResourceNotFound)

	list, listErr := store.ListArchives(context.Background())
	require.NoError(t, listErr)
	require.Empty(t, list)
}

// TestAgentAdminStore_InsertArchiveRefusesDuplicateID：归档标识是主键——重复插入
// 报错且**不覆盖**原行；第二次归档的操作行保持 pending（尚未写终态）。
// TestAgentAdminStore_InsertArchiveRefusesDuplicateID pins the duplicate rejection.
func TestAgentAdminStore_InsertArchiveRefusesDuplicateID(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-arch-4a")
	seedOperation(t, db, "op-arch-4b")
	store := NewAgentAdminStore(db)

	first := sampleArchive("digest-agent-20261016-093000", "digest-agent", "op-arch-4a")
	require.NoError(t, store.InsertArchive(context.Background(), first, "op-arch-4a"))

	second := sampleArchive("digest-agent-20261016-093000", "digest-agent-renamed", "op-arch-4b")
	second.ContentHash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	require.Error(t, store.InsertArchive(context.Background(), second, "op-arch-4b"))

	got, err := store.ArchiveByID(context.Background(), first.ArchiveID)
	require.NoError(t, err)
	require.Equal(t, first.AgentName, got.AgentName, "重复插入不得覆盖原行的 Agent 名")
	require.Equal(t, first.ContentHash, got.ContentHash, "重复插入不得覆盖原行的内容哈希")

	op, err := NewManagementStore(db).GetOperation(context.Background(), "op-arch-4b")
	require.NoError(t, err)
	require.Equal(t, core.OperationPending, op.Status, "归档行回滚后操作行不得被置终态")
}

// TestAgentAdminStore_MarkRestoredSetsFieldsAndFinishesOperation：恢复写恢复时刻与
// 目标名，并在同一事务把恢复操作置 success。
// TestAgentAdminStore_MarkRestoredSetsFieldsAndFinishesOperation pins the restore write.
func TestAgentAdminStore_MarkRestoredSetsFieldsAndFinishesOperation(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-arch-5")
	seedOperation(t, db, "op-restore-5")
	store := NewAgentAdminStore(db)
	require.NoError(t, store.InsertArchive(context.Background(),
		sampleArchive("digest-agent-20261016-093000", "digest-agent", "op-arch-5"), "op-arch-5"))

	restoredAt := archiveAt.Add(2 * time.Hour)
	require.NoError(t, store.MarkRestored(context.Background(),
		"digest-agent-20261016-093000", "digest-agent-restored", "op-restore-5", restoredAt))

	got, err := store.ArchiveByID(context.Background(), "digest-agent-20261016-093000")
	require.NoError(t, err)
	require.NotNil(t, got.RestoredAt)
	require.Equal(t, restoredAt.UnixMilli(), *got.RestoredAt)
	require.Equal(t, "digest-agent-restored", got.RestoredAgentName)

	op, err := NewManagementStore(db).GetOperation(context.Background(), "op-restore-5")
	require.NoError(t, err)
	require.Equal(t, core.OperationSuccess, op.Status)
	require.True(t, op.FinishedAt.Equal(restoredAt))

	// 同一归档不得恢复两次：第二次（另一条操作行）报冲突，且不写终态。
	seedOperation(t, db, "op-restore-5b")
	err = store.MarkRestored(context.Background(),
		"digest-agent-20261016-093000", "digest-agent-again", "op-restore-5b", restoredAt.Add(time.Minute))
	require.ErrorIs(t, err, core.ErrConflict)
	op, err = NewManagementStore(db).GetOperation(context.Background(), "op-restore-5b")
	require.NoError(t, err)
	require.Equal(t, core.OperationPending, op.Status)
}

// TestAgentAdminStore_MarkRestoredMissingArchive：归档不存在 → `core.ErrResourceNotFound`，
// 操作行保持 pending（由恢复路径查证后处置，不盲目补做）。
// TestAgentAdminStore_MarkRestoredMissingArchive pins the not-found branch.
func TestAgentAdminStore_MarkRestoredMissingArchive(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-restore-6")
	store := NewAgentAdminStore(db)

	err := store.MarkRestored(context.Background(),
		"ghost-agent-20261016-093000", "ghost-agent", "op-restore-6", archiveAt)
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	op, err := NewManagementStore(db).GetOperation(context.Background(), "op-restore-6")
	require.NoError(t, err)
	require.Equal(t, core.OperationPending, op.Status)
}

// TestAgentAdminStore_ArchiveByIDRoundTripsRestoredFields：两种状态的读回形态——
// 未恢复时 RestoredAt 为 nil，恢复后为指针（不被零值吞掉）；未命中是 not-found。
// TestAgentAdminStore_ArchiveByIDRoundTripsRestoredFields pins the pointer round-trip.
func TestAgentAdminStore_ArchiveByIDRoundTripsRestoredFields(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-arch-7")
	seedOperation(t, db, "op-restore-7")
	store := NewAgentAdminStore(db)
	require.NoError(t, store.InsertArchive(context.Background(),
		sampleArchive("digest-agent-20261016-093000", "digest-agent", "op-arch-7"), "op-arch-7"))

	pending, err := store.ArchiveByID(context.Background(), "digest-agent-20261016-093000")
	require.NoError(t, err)
	require.Nil(t, pending.RestoredAt)

	require.NoError(t, store.MarkRestored(context.Background(),
		"digest-agent-20261016-093000", "digest-agent", "op-restore-7", archiveAt))

	restored, err := store.ArchiveByID(context.Background(), "digest-agent-20261016-093000")
	require.NoError(t, err)
	require.NotNil(t, restored.RestoredAt)
	require.Equal(t, archiveAt.UnixMilli(), *restored.RestoredAt)
	require.Equal(t, "digest-agent", restored.RestoredAgentName)

	_, err = store.ArchiveByID(context.Background(), "no-such-archive")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
}

// TestAgentAdminStore_ListArchivesNewestFirst：清单按归档时刻倒序；同一毫秒按
// 归档标识倒序（顺序确定可比，不依赖插入顺序或行号）。
// TestAgentAdminStore_ListArchivesNewestFirst pins the deterministic list order.
func TestAgentAdminStore_ListArchivesNewestFirst(t *testing.T) {
	db := openTempDB(t)
	store := NewAgentAdminStore(db)

	rows := []struct {
		archiveID  string
		archivedAt time.Time
	}{
		{"old-agent-20261015-080000", archiveAt.Add(-24 * time.Hour)},
		{"tie-b-20261016-093000", archiveAt},
		{"new-agent-20261017-100000", archiveAt.Add(24 * time.Hour)},
		{"tie-a-20261016-093000", archiveAt},
	}
	for _, row := range rows {
		seedOperation(t, db, "op-list-"+row.archiveID)
		rec := sampleArchive(row.archiveID, "agent", "op-list-"+row.archiveID)
		rec.ArchivedAt = row.archivedAt.UnixMilli()
		require.NoError(t, store.InsertArchive(context.Background(), rec, rec.OperationID))
	}

	list, err := store.ListArchives(context.Background())
	require.NoError(t, err)
	require.Len(t, list, len(rows))
	require.Equal(t, []string{
		"new-agent-20261017-100000", // 最新
		"tie-b-20261016-093000",     // 同一毫秒，标识倒序在前
		"tie-a-20261016-093000",
		"old-agent-20261015-080000",
	}, []string{list[0].ArchiveID, list[1].ArchiveID, list[2].ArchiveID, list[3].ArchiveID})

	// 恢复不改变归档时刻与清单位置（恢复事实只落两个恢复列）。
	seedOperation(t, db, "op-list-restore")
	require.NoError(t, store.MarkRestored(context.Background(),
		"tie-a-20261016-093000", "agent", "op-list-restore", archiveAt))
	after, err := store.ListArchives(context.Background())
	require.NoError(t, err)
	require.Equal(t, list[0].ArchiveID, after[0].ArchiveID)
	require.Equal(t, "tie-a-20261016-093000", after[2].ArchiveID)
	require.NotNil(t, after[2].RestoredAt)

	// 清理判据：错误路径不吞错——空库清单是空切片而非 nil。
	empty := NewAgentAdminStore(openTempDB(t))
	list, err = empty.ListArchives(context.Background())
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Empty(t, list)
}
