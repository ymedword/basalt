//go:build integration

package postgres

// 本文件是 `agent_archives` 归档记录仓储与 `resource_identities` 身份仓储的判卷
// （颗粒度文档 §3.1.6 / R4.5 / T113，check `012-storage-admin`）：归档行与操作终态
// 同事务、重复归档标识不覆盖、恢复只落一次、读面与清单顺序确定；身份一次写入、
// 删除只翻状态留墓碑、版本号按实例（UID）计。
//
// 由 SQLite 谱系 `internal/storage/agent_admin_store_test.go` 与
// `internal/storage/identity_store_test.go` 逐条移植：测试函数名、断言、中文验收点
// 注释与逆改点注释原样保留；只把夹具换成隔离 schema（`portTestDB`）、读写句柄合一为
// `db.pool`（本组用例未直接写原始 SQL，故无占位符 / 布尔方言改动），时间列按 BIGINT
// Unix 毫秒读写。
// Ported verbatim from the SQLite lineage; only the fixture and the handle differ.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

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
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	empty := NewAgentAdminStore(portTestDB(t))
	list, err = empty.ListArchives(context.Background())
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Empty(t, list)
}

// newIdentity 造一条形状完整的身份行（默认 active、无删除时间）。
// newIdentity builds a complete identity row.
func newIdentity(uid core.ResourceUID, kind core.ResourceKind, name, operationID string) core.ResourceIdentity {
	return core.ResourceIdentity{
		UID:         uid,
		Kind:        kind,
		Name:        name,
		State:       core.IdentityActive,
		CreatedAt:   opNow.UnixMilli(),
		OperationID: operationID,
	}
}

// TestIdentityStore_Tombstone：删除是**状态翻转**而不是删行——墓碑保留名称快照与删除
// 时间；重复删除幂等（保留第一次时间）；不存在 → `core.ErrResourceNotFound`；
// 新身份不允许带删除态或复用已有 UID。
// 中文验收点：墓碑不删行、重复删除幂等、UID 不复用。
func TestIdentityStore_Tombstone(t *testing.T) {
	db := portTestDB(t)
	store := NewIdentityStore(db)
	ctx := context.Background()
	uid := testResourceUID('A')

	require.NoError(t, store.Create(ctx, newIdentity(uid, core.ResourceModelConnection, "openai", "op-id-1")))

	rec, err := store.IdentityByUID(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, uid, rec.UID)
	require.Equal(t, core.ResourceModelConnection, rec.Kind)
	require.Equal(t, "openai", rec.Name)
	require.Equal(t, core.IdentityActive, rec.State)
	require.Nil(t, rec.DeletedAt)

	first := opNow
	require.NoError(t, store.Tombstone(ctx, uid, "op-id-2", first))

	rec, err = store.IdentityByUID(ctx, uid)
	require.NoError(t, err, "墓碑仍可读——身份行不物理删除")
	require.Equal(t, core.IdentityDeleted, rec.State)
	require.NotNil(t, rec.DeletedAt)
	require.Equal(t, first.UnixMilli(), *rec.DeletedAt)
	require.Equal(t, "openai", rec.Name, "名称快照保留，历史仍能回答它是谁")

	// 重复删除：幂等，第一次的删除时间不被覆盖。
	require.NoError(t, store.Tombstone(ctx, uid, "op-id-3", first.Add(time.Hour)))
	rec, err = store.IdentityByUID(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, first.UnixMilli(), *rec.DeletedAt)

	// 不存在的 UID → 404 语义。
	err = store.Tombstone(ctx, testResourceUID('B'), "op-id-4", first)
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	// UID 永不复用：再建同一 UID 被拒；删除态也不能作为"新身份"写入。
	require.Error(t, store.Create(ctx, newIdentity(uid, core.ResourceModelConnection, "openai", "op-id-5")))
	deletedAt := first.UnixMilli()
	revived := newIdentity(testResourceUID('C'), core.ResourceModelConnection, "other", "op-id-6")
	revived.State = core.IdentityDeleted
	revived.DeletedAt = &deletedAt
	require.Error(t, store.Create(ctx, revived))
}

// TestIdentityStore_VersionScopeByUID：版本号的作用域是**实例**而不是名称——
// 两个同名但 UID 不同的资源各自从 1 计数，取号互不影响。
// 中文验收点：同名不同实例的版本号互不干扰。
func TestIdentityStore_VersionScopeByUID(t *testing.T) {
	db := portTestDB(t)
	store := NewIdentityStore(db)
	versions := NewResourceStore(db)
	ctx := context.Background()

	seedOperation(t, db, "op-scope-1")
	seedOperation(t, db, "op-scope-2")

	uidA := testResourceUID('A')
	uidB := testResourceUID('B')
	require.NoError(t, store.Create(ctx, newIdentity(uidA, core.ResourceModelConnection, "openai", "op-scope-1")))
	require.NoError(t, store.Create(ctx, newIdentity(uidB, core.ResourceModelConnection, "openai", "op-scope-2")))

	next, err := store.AllocateVersionNo(ctx, core.ResourceModelConnection, uidA)
	require.NoError(t, err)
	require.Equal(t, 1, next, "还没版本时取 1")

	versionOf := func(uid core.ResourceUID, hash, operationID string) core.ResourceVersion {
		return core.ResourceVersion{
			Kind:           core.ResourceModelConnection,
			ResourceUID:    uid,
			ResourceID:     "openai",
			ContentHash:    hash,
			DefinitionYAML: "name: openai\n",
			CreatedAt:      opNow.UnixMilli(),
			OperationID:    operationID,
		}
	}
	first, _, err := versions.EnsureVersion(ctx, versionOf(uidA, "sha256:a", "op-scope-1"))
	require.NoError(t, err)
	require.Equal(t, 1, first.VersionNo)

	next, err = store.AllocateVersionNo(ctx, core.ResourceModelConnection, uidA)
	require.NoError(t, err)
	require.Equal(t, 2, next)

	next, err = store.AllocateVersionNo(ctx, core.ResourceModelConnection, uidB)
	require.NoError(t, err)
	require.Equal(t, 1, next, "同名不同 UID 是另一实例，从 1 开始")

	second, _, err := versions.EnsureVersion(ctx, versionOf(uidA, "sha256:b", "op-scope-1"))
	require.NoError(t, err)
	require.Equal(t, 2, second.VersionNo)

	// 种类不匹配的取号在写库前拒绝（版本表只认三值）。
	_, err = store.AllocateVersionNo(ctx, core.ResourceNotifyChannel, uidA)
	require.Error(t, err)

	// 身份种类按四值解析：身份表里的通知渠道合法，版本表里仍是非法种类。
	require.NoError(t, store.Create(ctx, newIdentity(testResourceUID('C'), core.ResourceNotifyChannel, "值班", "op-scope-2")))
}

// TestIdentityStore_SameNameRebuildStartsAtOne：同名删除后重建是**新身份**——
// 旧墓碑仍在（两个代际都查得到），新实例的版本号从 1 重新计。
// 中文验收点：同名重建版本号从 1 起，旧代际保留。
func TestIdentityStore_SameNameRebuildStartsAtOne(t *testing.T) {
	db := portTestDB(t)
	store := NewIdentityStore(db)
	versions := NewResourceStore(db)
	ctx := context.Background()

	seedOperation(t, db, "op-rebuild-1")
	seedOperation(t, db, "op-rebuild-2")

	oldUID := testResourceUID('A')
	newUID := testResourceUID('B')
	require.NoError(t, store.Create(ctx, newIdentity(oldUID, core.ResourceModelConnection, "openai", "op-rebuild-1")))
	_, _, err := versions.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    oldUID,
		ResourceID:     "openai",
		ContentHash:    "sha256:a",
		DefinitionYAML: "name: openai\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    "op-rebuild-1",
	})
	require.NoError(t, err)
	require.NoError(t, store.Tombstone(ctx, oldUID, "op-rebuild-1", opNow))

	require.NoError(t, store.Create(ctx, newIdentity(newUID, core.ResourceModelConnection, "openai", "op-rebuild-2")))

	next, err := store.AllocateVersionNo(ctx, core.ResourceModelConnection, newUID)
	require.NoError(t, err)
	require.Equal(t, 1, next, "重建是新实例，版本号重新从 1 起")

	oldNext, err := store.AllocateVersionNo(ctx, core.ResourceModelConnection, oldUID)
	require.NoError(t, err)
	require.Equal(t, 2, oldNext, "旧墓碑的版本账不因重建清零")

	generations, err := store.IdentitiesByName(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Len(t, generations, 2, "同名两个代际都保留")
	require.Equal(t, oldUID, generations[0].UID)
	require.Equal(t, core.IdentityDeleted, generations[0].State)
	require.Equal(t, newUID, generations[1].UID)
	require.Equal(t, core.IdentityActive, generations[1].State)

	// 未知种类与空名称在查询前拒绝。
	_, err = store.IdentitiesByName(ctx, core.ResourceKind("unknown"), "openai")
	require.Error(t, err)
	_, err = store.IdentitiesByName(ctx, core.ResourceModelConnection, "")
	require.Error(t, err)
}
