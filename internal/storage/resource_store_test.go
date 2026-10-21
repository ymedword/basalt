package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是资源版本 / 指针 / 引用 / 生命周期索引仓储的判卷（颗粒度文档 §3.1.17，
// check `011-storage-resources`）：版本不可变、按哈希幂等、指针更新与 applied 语义、
// 引用只计未释放、索引可重建，以及版本 + 指针单事务的原子性。
// These tests pin the resource repository: immutable versions, hash idempotence, pointer
// semantics, unreleased references, the rebuildable lifecycle index, and atomicity.

// seedOperation 建一行管理操作（版本行的 operation_id 外键要求它先存在；
// 真实路径由基线 / US-12 的 BeginOperation 预写）。
// seedOperation inserts the management operation row the resource foreign keys require.
func seedOperation(t *testing.T, db *DB, operationID string) {
	t.Helper()
	require.NoError(t, NewManagementStore(db).BeginOperation(context.Background(), core.OperationRecord{
		OperationID:  operationID,
		Action:       "system.baseline_v2",
		TargetKind:   "system",
		TargetID:     "system",
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       "migration",
		CreatedAt:    opNow,
	}))
}

// modelVersion 造一条模型连接版本；版本号 0 表示让仓储自动取号。
// modelVersion builds a model_connection version; zero means auto-numbering.
func modelVersion(resourceID, contentHash, operationID string, versionNo int) core.ResourceVersion {
	return core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceID:     resourceID,
		VersionNo:      versionNo,
		ContentHash:    contentHash,
		DefinitionYAML: "name: " + resourceID + "\nmodel: gpt-test\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    operationID,
	}
}

// TestResourceStore_EnsureVersionInsertsFirstVersion：首个版本号从 1 起、字段逐列落库、
// `inserted=true`；按 id 读回的形态与写入一致。
// 中文用例名：首版入库与读回。
func TestResourceStore_EnsureVersionInsertsFirstVersion(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-1")
	store := NewResourceStore(db)

	version, inserted, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:aaa", "op-r-1", 0))
	require.NoError(t, err)
	require.True(t, inserted)
	require.Equal(t, 1, version.VersionNo)
	require.NotZero(t, version.ID)
	require.Equal(t, core.ResourceModelConnection, version.Kind)

	got, err := store.VersionByID(context.Background(), version.ID)
	require.NoError(t, err)
	require.Equal(t, version, got)
	require.Equal(t, opNow.UnixMilli(), got.CreatedAt)
}

// TestResourceStore_EnsureVersionIsIdempotentByHash：同 `(kind, id, content_hash)` 再次
// Ensure 返回既有版本、`inserted=false`，版本表不增长——版本是不可变历史。
// 中文用例名：同哈希幂等，不产生第二行。
func TestResourceStore_EnsureVersionIsIdempotentByHash(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-2")
	store := NewResourceStore(db)

	first, inserted, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:same", "op-r-2", 0))
	require.NoError(t, err)
	require.True(t, inserted)

	second, inserted, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:same", "op-r-2", 9))
	require.NoError(t, err)
	require.False(t, inserted)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, 1, second.VersionNo)

	versions, err := store.Versions(context.Background(), core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Len(t, versions, 1)
}

// TestResourceStore_EnsureVersionAssignsMonotonicNumbers：`VersionNo=0` 时自动取
// `MAX(version_no)+1`，逐资源单调递增。
// 中文用例名：自动取号逐资源单调递增。
func TestResourceStore_EnsureVersionAssignsMonotonicNumbers(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-3")
	store := NewResourceStore(db)

	first, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:one", "op-r-3", 0))
	require.NoError(t, err)
	second, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:two", "op-r-3", 0))
	require.NoError(t, err)
	require.Equal(t, 1, first.VersionNo)
	require.Equal(t, 2, second.VersionNo)

	// 另一个资源独立从 1 起。
	other, _, err := store.EnsureVersion(context.Background(),
		modelVersion("local", "sha256:one", "op-r-3", 0))
	require.NoError(t, err)
	require.Equal(t, 1, other.VersionNo)
}

// TestResourceStore_EnsureVersionValidatesInput：未知类型、空标识 / 哈希、按类型缺失
// 必填内容在写库前拒绝（schema CHECK 只是最后一道）。
// 中文用例名：非法版本输入零写入。
func TestResourceStore_EnsureVersionValidatesInput(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-4")
	store := NewResourceStore(db)

	cases := []core.ResourceVersion{
		{Kind: "unknown", ResourceID: "x", ContentHash: "h", DefinitionYAML: "y"},
		{Kind: core.ResourceModelConnection, ResourceID: "", ContentHash: "h", DefinitionYAML: "y"},
		{Kind: core.ResourceModelConnection, ResourceID: "x", ContentHash: ""},
		{Kind: core.ResourceModelConnection, ResourceID: "x", ContentHash: "h"},
		{Kind: core.ResourceSkill, ResourceID: "x", ContentHash: "h", StoragePath: "p"},
		{Kind: core.ResourceSkill, ResourceID: "x", ContentHash: "h", ManifestJSON: "[]"},
	}
	for _, v := range cases {
		_, _, err := store.EnsureVersion(context.Background(), v)
		require.Error(t, err, "input %+v", v)
	}

	var count int
	require.NoError(t, db.read.QueryRowContext(context.Background(),
		`SELECT count(*) FROM resource_versions`).Scan(&count))
	require.Zero(t, count)
}

// TestResourceStore_ConcurrentEnsure：并发写同一资源的不同内容、都显式带 VersionNo=1——
// 唯一键冲突后重查哈希并重试一次，全部成功且版本号互不重复（011-race 必跑）。
// 中文用例名：并发建版本唯一键重试一次（011-race）。
func TestResourceStore_ConcurrentEnsure(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-5")
	store := NewResourceStore(db)

	const workers = 6
	errs := make([]error, workers)
	versions := make([]core.ResourceVersion, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			versions[i], _, errs[i] = store.EnsureVersion(context.Background(),
				modelVersion("openai", fmt.Sprintf("sha256:c%d", i), "op-r-5", 1))
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i], "worker %d", i)
		require.False(t, seen[versions[i].VersionNo], "版本号 %d 重复", versions[i].VersionNo)
		seen[versions[i].VersionNo] = true
	}
	require.Len(t, seen, workers)
}

// TestResourceStore_PublishUpsertsPointer：首发布 `applied=false` 时 applied 为 NULL；
// 再发布 `applied=true` 时 published 与 applied 同时指向新版本。
// 中文用例名：指针 upsert 与 applied 首次写入。
func TestResourceStore_PublishUpsertsPointer(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-6")
	store := NewResourceStore(db)

	first, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v1", "op-r-6", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(context.Background(), core.ResourceModelConnection,
		"openai", first.ID, false, "op-r-6", opNow))

	ptr, err := store.Pointer(context.Background(), core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Equal(t, first.ID, ptr.PublishedVersionID)
	require.Nil(t, ptr.AppliedVersionID)
	require.Nil(t, ptr.DeletedAt)
	require.Equal(t, opNow.UnixMilli(), ptr.UpdatedAt)
	require.Equal(t, "op-r-6", ptr.OperationID)

	require.NoError(t, store.Publish(context.Background(), core.ResourceModelConnection,
		"openai", first.ID, true, "op-r-6", opNow.Add(time.Minute)))
	ptr, err = store.Pointer(context.Background(), core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Equal(t, first.ID, ptr.PublishedVersionID)
	require.NotNil(t, ptr.AppliedVersionID)
	require.Equal(t, first.ID, *ptr.AppliedVersionID)
}

// TestResourceStore_PublishKeepsAppliedWhenNotApplied：`applied=false` 的发布保留旧
// applied（MCP 首版重启前的语义），published 前进而 applied 不动。
// 中文用例名：applied=false 保留旧生效版本。
func TestResourceStore_PublishKeepsAppliedWhenNotApplied(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-7")
	store := NewResourceStore(db)

	first, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v1", "op-r-7", 0))
	require.NoError(t, err)
	second, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v2", "op-r-7", 0))
	require.NoError(t, err)

	require.NoError(t, store.Publish(context.Background(), core.ResourceModelConnection,
		"openai", first.ID, true, "op-r-7", opNow))
	require.NoError(t, store.Publish(context.Background(), core.ResourceModelConnection,
		"openai", second.ID, false, "op-r-7", opNow.Add(time.Minute)))

	ptr, err := store.Pointer(context.Background(), core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Equal(t, second.ID, ptr.PublishedVersionID)
	require.NotNil(t, ptr.AppliedVersionID)
	require.Equal(t, first.ID, *ptr.AppliedVersionID)
}

// TestResourceStore_PublishRejectsCrossResourceVersion：版本必须属于同一资源；把 A 的
// 版本挂到 B 的指针上直接拒绝，且不产生指针行。
// 中文用例名：拒绝跨资源挂指针。
func TestResourceStore_PublishRejectsCrossResourceVersion(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-8")
	store := NewResourceStore(db)

	version, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v1", "op-r-8", 0))
	require.NoError(t, err)

	err = store.Publish(context.Background(), core.ResourceModelConnection,
		"local", version.ID, true, "op-r-8", opNow)
	require.ErrorContains(t, err, "不能发布为")

	err = store.Publish(context.Background(), core.ResourceMCPServer,
		"openai", version.ID, true, "op-r-8", opNow)
	require.ErrorContains(t, err, "不能发布为")

	_, err = store.Pointer(context.Background(), core.ResourceModelConnection, "local")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
}

// TestResourceStore_PublishClearsDeletedMark：撤回 = 用新版本重新发布；发布时清掉
// `deleted_at`，资源重新出现在未删除列表里。
// 中文用例名：重新发布清除删除标记。
func TestResourceStore_PublishClearsDeletedMark(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-9")
	store := NewResourceStore(db)

	first, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v1", "op-r-9", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(context.Background(), core.ResourceModelConnection,
		"openai", first.ID, true, "op-r-9", opNow))

	_, err = db.write.ExecContext(context.Background(),
		`UPDATE resource_pointers SET deleted_at = ? WHERE resource_kind = ? AND resource_id = ?`,
		opNow.UnixMilli(), string(core.ResourceModelConnection), "openai")
	require.NoError(t, err)
	list, err := store.ListPointers(context.Background(), core.ResourceModelConnection)
	require.NoError(t, err)
	require.Empty(t, list)

	require.NoError(t, store.Publish(context.Background(), core.ResourceModelConnection,
		"openai", first.ID, true, "op-r-9", opNow.Add(time.Minute)))
	ptr, err := store.Pointer(context.Background(), core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Nil(t, ptr.DeletedAt)
}

// TestResourceStore_PointerAndVersionNotFound：未命中的指针 / 版本返回
// core.ErrResourceNotFound——只读面据此映射 404，不静默返回零值。
// 中文用例名：指针与版本未命中返回 not-found。
func TestResourceStore_PointerAndVersionNotFound(t *testing.T) {
	db := openTempDB(t)
	store := NewResourceStore(db)

	_, err := store.Pointer(context.Background(), core.ResourceModelConnection, "ghost")
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	_, err = store.VersionByID(context.Background(), 42)
	require.ErrorIs(t, err, core.ErrResourceNotFound)
}

// TestResourceStore_VersionsNewestFirstAndListPointersSkipsDeleted：版本历史最新在前；
// 指针列表只含未删除资源、按 resource_id 升序。
// 中文用例名：版本倒序、指针列表排除已删除。
func TestResourceStore_VersionsNewestFirstAndListPointersSkipsDeleted(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-11")
	store := NewResourceStore(db)

	v1, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v1", "op-r-11", 0))
	require.NoError(t, err)
	v2, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v2", "op-r-11", 0))
	require.NoError(t, err)
	v3, _, err := store.EnsureVersion(context.Background(),
		modelVersion("local", "sha256:v1", "op-r-11", 0))
	require.NoError(t, err)

	versions, err := store.Versions(context.Background(), core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Len(t, versions, 2)
	require.Equal(t, v2.ID, versions[0].ID)
	require.Equal(t, v1.ID, versions[1].ID)

	require.NoError(t, store.Publish(context.Background(), core.ResourceModelConnection,
		"openai", v2.ID, true, "op-r-11", opNow))
	require.NoError(t, store.Publish(context.Background(), core.ResourceModelConnection,
		"local", v3.ID, true, "op-r-11", opNow))
	_, err = db.write.ExecContext(context.Background(),
		`UPDATE resource_pointers SET deleted_at = ? WHERE resource_id = ?`,
		opNow.UnixMilli(), "local")
	require.NoError(t, err)

	pointers, err := store.ListPointers(context.Background(), core.ResourceModelConnection)
	require.NoError(t, err)
	require.Len(t, pointers, 1)
	require.Equal(t, "openai", pointers[0].ResourceID)
}

// TestResourceStore_ReferencedByCountsUnreleasedOnly：引用只统计 `released_at IS NULL`
// 的 Run，按 run_id 升序；释放后的历史行保留但不计入。
// 中文用例名：引用只计未释放 Run、升序。
func TestResourceStore_ReferencedByCountsUnreleasedOnly(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-12")
	store := NewResourceStore(db)

	version, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v1", "op-r-12", 0))
	require.NoError(t, err)
	seedRunRow(t, db.write, "run-2", "running", sql.NullString{}, nil, nil)
	seedRunRow(t, db.write, "run-1", "running", sql.NullString{}, nil, nil)

	insertRef := func(runID string, releasedAt any) {
		t.Helper()
		_, err := db.write.ExecContext(context.Background(),
			`INSERT INTO run_resource_refs
			   (run_id, resource_kind, resource_id, version_id, content_hash, released_at)
			 VALUES (?, ?, 'openai', ?, 'sha256:v1', ?)`,
			runID, string(core.ResourceModelConnection), version.ID, releasedAt)
		require.NoError(t, err)
	}
	insertRef("run-1", nil)
	insertRef("run-2", opNow.UnixMilli()) // 已释放：不计入

	runIDs, err := store.ReferencedBy(context.Background(), core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Equal(t, []string{"run-1"}, runIDs)
}

// TestResourceStore_PublishWithVersionIsAtomic：版本插入与指针更新同事务；指针写失败
// （operation_id 悬空，外键拒绝）时版本行一并回滚，不留"版本在、指针没动"的中间态。
// 中文用例名：版本 + 指针单事务，失败整笔回滚。
func TestResourceStore_PublishWithVersionIsAtomic(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-13")
	store := NewResourceStore(db)

	v := modelVersion("openai", "sha256:v1", "op-r-13", 0)
	published, err := store.PublishWithVersion(context.Background(), v, "op-r-13", opNow)
	require.NoError(t, err)
	require.Equal(t, 1, published.VersionNo)
	ptr, err := store.Pointer(context.Background(), core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Equal(t, published.ID, ptr.PublishedVersionID)
	require.NotNil(t, ptr.AppliedVersionID)
	require.Equal(t, published.ID, *ptr.AppliedVersionID)

	// 指针写失败：operation_id 不存在触发外键；版本插入必须一起回滚。
	bad := modelVersion("anthropic", "sha256:v1", "op-r-13", 0)
	_, err = store.PublishWithVersion(context.Background(), bad, "op-missing", opNow)
	require.ErrorContains(t, err, "更新发布指针")

	versions, err := store.Versions(context.Background(), core.ResourceModelConnection, "anthropic")
	require.NoError(t, err)
	require.Empty(t, versions)
}

// TestResourceStore_LifecycleIndexRoundTrip：索引写有效记录后可读回；`valid=false` 清空
// state 并记原因；无索引行不是错误。
// 中文用例名：生命周期索引往返与失效清空。
func TestResourceStore_LifecycleIndexRoundTrip(t *testing.T) {
	db := openTempDB(t)
	seedOperation(t, db, "op-r-14")
	store := NewResourceStore(db)

	rec := core.LifecycleRecord{
		SchemaVersion: 1,
		Agent:         "writer",
		State:         core.AgentActive,
		Generation:    3,
		UpdatedAt:     opNow.UnixMilli(),
		OperationID:   "op-r-14",
		Checksum:      "sha256:abc",
	}
	require.NoError(t, store.PutLifecycleIndex(context.Background(), rec, true, ""))

	got, valid, errMsg, err := store.LifecycleIndex(context.Background(), "writer")
	require.NoError(t, err)
	require.True(t, valid)
	require.Empty(t, errMsg)
	require.Equal(t, rec, got)

	require.NoError(t, store.PutLifecycleIndex(context.Background(), rec, false, "状态文件校验和不符"))
	got, valid, errMsg, err = store.LifecycleIndex(context.Background(), "writer")
	require.NoError(t, err)
	require.False(t, valid)
	require.Equal(t, "状态文件校验和不符", errMsg)
	require.Empty(t, got.State)
	require.Zero(t, got.Generation)

	missing, valid, errMsg, err := store.LifecycleIndex(context.Background(), "ghost")
	require.NoError(t, err)
	require.False(t, valid)
	require.Empty(t, errMsg)
	require.Equal(t, "ghost", missing.Agent)
}

// TestResourceStore_EnsureVersionRequiresOperation：版本行的 operation_id 外键指向管理
// 操作——没有预写操作就建不了版本，发布路径无法绕过审计（宪法 V）。
// 中文用例名：版本行必须挂已预写的操作。
func TestResourceStore_EnsureVersionRequiresOperation(t *testing.T) {
	db := openTempDB(t)
	store := NewResourceStore(db)

	_, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v1", "op-missing", 0))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "FOREIGN KEY") || strings.Contains(err.Error(), "constraint"),
		"期望外键拒绝，实际 %v", err)

	var count int
	require.NoError(t, db.read.QueryRowContext(context.Background(),
		`SELECT count(*) FROM resource_versions`).Scan(&count))
	require.Zero(t, count)
}
