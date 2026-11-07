//go:build integration

package postgres

// 本文件是资源版本 / 指针 / 引用 / 生命周期索引仓储在 PostgreSQL 谱系上的判卷
// （颗粒度文档 §3.1.17，check `011-storage-resources`）：版本不可变、按哈希幂等、
// 指针更新与 applied 语义、引用只计未释放、索引可重建，以及版本 + 指针单事务的原子性。
//
// 由 SQLite 谱系 `internal/storage/resource_store_test.go` 逐条移植：测试函数名、
// 断言与中文验收点注释原样保留；只替换「库怎么来」与「原始 SQL 的方言」
// （`openTempDB` → `portTestDB`、`db.read` / `db.write` → `db.pool`、`?` → `$n`）。
// 本文件另补 SQLite 谱系未覆盖的读取面（按 UID 的指针 / 引用 / 身份镜像 / 范围转换），
// 以便包级覆盖率判据有完整的可执行面。
// Ported from the SQLite lineage with assertions verbatim; the dialect-only edits are the
// fixture handle, the read/write pool and the placeholder numbering.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// resourceUIDFor 按 `(kind, name)` 派生一个稳定的测试 UID：同名同种永远是同一实例，
// 不同名称 / 不同种类是不同实例——测试要判的正是"身份跟着实例走、不跟着名称走"。
// 真实路径的 UID 由 core.NewResourceUID 随机生成；测试里确定性更重要。
// resourceUIDFor derives a stable test uid from kind and name; the production path uses
// crypto/rand, while tests need reproducibility.
func resourceUIDFor(kind core.ResourceKind, name string) core.ResourceUID {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + name))
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	payload := make([]byte, 26)
	for i := range payload {
		payload[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return core.ResourceUID(core.ResourceUIDPrefix + string(payload))
}

// modelVersion 造一条模型连接版本；版本号 0 表示让仓储自动取号。
// modelVersion builds a model_connection version; zero means auto-numbering.
func modelVersion(resourceID, contentHash, operationID string, versionNo int) core.ResourceVersion {
	return core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    resourceUIDFor(core.ResourceModelConnection, resourceID),
		ResourceID:     resourceID,
		VersionNo:      versionNo,
		ContentHash:    contentHash,
		DefinitionYAML: "name: " + resourceID + "\nmodel: gpt-test\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    operationID,
	}
}

// resourceRawOperation 是管理操作行的原始读数（只取判终态用得着的两列）。
// 移植自 SQLite 谱系的 readRawOperation；本包内加 `resourceStore` 前缀，避免与其他
// 移植文件的同名助手冲突。
// resourceRawOperation is the raw operation reading; the resourceStore prefix keeps the
// helper from colliding with other ported test files.
type resourceRawOperation struct {
	Status     string
	FinishedAt sql.NullInt64
}

// resourceStoreReadRawOperation 直读管理操作行（不经过仓储的扫描口径）。
// resourceStoreReadRawOperation reads the operation row straight from the table.
func resourceStoreReadRawOperation(t *testing.T, db *DB, operationID string) resourceRawOperation {
	t.Helper()
	var row resourceRawOperation
	err := db.pool.QueryRowContext(context.Background(),
		`SELECT status, finished_at FROM management_operations WHERE operation_id = $1`, operationID).
		Scan(&row.Status, &row.FinishedAt)
	require.NoError(t, err)
	return row
}

// resourceStoreSeedRunRow 建一行账本。终态行的 `reason` / `finished_at` 由用例显式给
// （nil → NULL）——只有它们非空，"恢复有没有覆盖已结束的行"才判得出来。
// PostgreSQL 谱系的 `runs` 多出归属 / 授权 / 执行者三列且 caller_id 有外键：迁移已补
// 内置 `local-admin` 行，这里固定用它。
// resourceStoreSeedRunRow plants one run row; the PG lineage requires the attribution
// columns, and the built-in local-admin caller is seeded by migration.
func resourceStoreSeedRunRow(t *testing.T, db *DB, runID, status string, session sql.NullString, reason, finishedAt any) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`INSERT INTO runs
		   (run_id, session_id, agent, channel, user_id, trigger, status,
		    reason, iterations, tool_calls, config_hash, started_at, finished_at,
		    caller_id, authorizing_kind, key_id, actor)
		 VALUES ($1, $2, 'assistant', 'cli', 'u1', 'user', $3, $4, 0, 0, NULL, $5, $6,
		         'local-admin', 'admin-builtin', NULL, 'admin-cli')`,
		runID, session, status, reason, opNow.Add(-time.Hour).UnixMilli(), finishedAt)
	require.NoError(t, err)
}

// TestResourceStore_EnsureVersionInsertsFirstVersion：首个版本号从 1 起、字段逐列落库、
// `inserted=true`；按 id 读回的形态与写入一致。
// 中文用例名：首版入库与读回。
func TestResourceStore_EnsureVersionInsertsFirstVersion(t *testing.T) {
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	db := portTestDB(t)
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

// TestResourceStore_EnsureVersionValidatesInput：未知类型、缺资源 UID、空标识 / 哈希、
// 按类型缺失必填内容在写库前拒绝（schema CHECK 只是最后一道）。
// 中文用例名：非法版本输入零写入。
func TestResourceStore_EnsureVersionValidatesInput(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-r-4")
	store := NewResourceStore(db)
	uid := resourceUIDFor(core.ResourceModelConnection, "x")
	skillUID := resourceUIDFor(core.ResourceSkill, "x")

	cases := []core.ResourceVersion{
		{Kind: "unknown", ResourceUID: uid, ResourceID: "x", ContentHash: "h", DefinitionYAML: "y"},
		{Kind: core.ResourceModelConnection, ResourceID: "x", ContentHash: "h", DefinitionYAML: "y"},
		{
			Kind: core.ResourceModelConnection, ResourceUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
			ResourceID: "x", ContentHash: "h", DefinitionYAML: "y",
		},
		{Kind: core.ResourceModelConnection, ResourceUID: uid, ResourceID: "", ContentHash: "h", DefinitionYAML: "y"},
		{Kind: core.ResourceModelConnection, ResourceUID: uid, ResourceID: "x", ContentHash: ""},
		{Kind: core.ResourceModelConnection, ResourceUID: uid, ResourceID: "x", ContentHash: "h"},
		{Kind: core.ResourceSkill, ResourceUID: skillUID, ResourceID: "x", ContentHash: "h", StoragePath: "p"},
		{Kind: core.ResourceSkill, ResourceUID: skillUID, ResourceID: "x", ContentHash: "h", ManifestJSON: "[]"},
	}
	for _, v := range cases {
		_, _, err := store.EnsureVersion(context.Background(), v)
		require.Error(t, err, "input %+v", v)
	}

	var count int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM resource_versions`).Scan(&count))
	require.Zero(t, count)
}

// TestResourceStore_ConcurrentEnsure：并发写同一资源的不同内容、都显式带 VersionNo=1——
// 唯一键冲突后重查哈希并重试一次，全部成功且版本号互不重复（011-race 必跑）。
// 中文用例名：并发建版本唯一键重试一次（011-race）。
func TestResourceStore_ConcurrentEnsure(t *testing.T) {
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	db := portTestDB(t)
	seedOperation(t, db, "op-r-9")
	store := NewResourceStore(db)

	first, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v1", "op-r-9", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(context.Background(), core.ResourceModelConnection,
		"openai", first.ID, true, "op-r-9", opNow))

	_, err = db.pool.ExecContext(context.Background(),
		`UPDATE resource_pointers SET deleted_at = $1 WHERE resource_kind = $2 AND resource_id = $3`,
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
	db := portTestDB(t)
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
	db := portTestDB(t)
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
	_, err = db.pool.ExecContext(context.Background(),
		`UPDATE resource_pointers SET deleted_at = $1 WHERE resource_id = $2`,
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
	db := portTestDB(t)
	seedOperation(t, db, "op-r-12")
	store := NewResourceStore(db)

	version, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v1", "op-r-12", 0))
	require.NoError(t, err)
	resourceStoreSeedRunRow(t, db, "run-2", "running", sql.NullString{}, nil, nil)
	resourceStoreSeedRunRow(t, db, "run-1", "running", sql.NullString{}, nil, nil)

	insertRef := func(runID string, releasedAt any) {
		t.Helper()
		_, err := db.pool.ExecContext(context.Background(),
			`INSERT INTO run_resource_refs
			   (run_id, resource_kind, resource_id, version_id, content_hash, released_at)
			 VALUES ($1, $2, 'openai', $3, 'sha256:v1', $4)`,
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
	db := portTestDB(t)
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

// TestResourceStore_PublishWithVersionStateKeepsApplied：`applied=false` 的版本化发布
// 仍插入新版本并前进 published，但 applied 留在旧版本（MCP 发布等重启才生效）；
// 操作终态与指针同事务回填。`PublishWithVersion` 默认 applied=true 的路径由上一用例覆盖。
// 中文用例名：版本化发布保留旧生效指针。
func TestResourceStore_PublishWithVersionStateKeepsApplied(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-r-14")
	seedOperation(t, db, "op-r-15")
	store := NewResourceStore(db)

	first, err := store.PublishWithVersionState(context.Background(),
		modelVersion("openai", "sha256:v1", "op-r-14", 0), true, "op-r-14", opNow)
	require.NoError(t, err)

	second, err := store.PublishWithVersionState(context.Background(),
		modelVersion("openai", "sha256:v2", "op-r-15", 0), false, "op-r-15", opNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, 2, second.VersionNo)

	ptr, err := store.Pointer(context.Background(), core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Equal(t, second.ID, ptr.PublishedVersionID)
	require.NotNil(t, ptr.AppliedVersionID)
	require.Equal(t, first.ID, *ptr.AppliedVersionID)

	op, err := NewManagementStore(db).GetOperation(context.Background(), "op-r-15")
	require.NoError(t, err)
	require.Equal(t, core.OperationSuccess, op.Status)
}

// TestResourceStore_LifecycleIndexRoundTrip：索引写有效记录后可读回；`valid=false` 清空
// state 并记原因；无索引行不是错误。
// 中文用例名：生命周期索引往返与失效清空。
func TestResourceStore_LifecycleIndexRoundTrip(t *testing.T) {
	db := portTestDB(t)
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

	got, valid, errMsg, err := store.ReadLifecycleIndex(context.Background(), "writer")
	require.NoError(t, err)
	require.True(t, valid)
	require.Empty(t, errMsg)
	require.Equal(t, rec, got)

	require.NoError(t, store.PutLifecycleIndex(context.Background(), rec, false, "状态文件校验和不符"))
	got, valid, errMsg, err = store.ReadLifecycleIndex(context.Background(), "writer")
	require.NoError(t, err)
	require.False(t, valid)
	require.Equal(t, "状态文件校验和不符", errMsg)
	require.Empty(t, got.State)
	require.Zero(t, got.Generation)

	missing, valid, errMsg, err := store.ReadLifecycleIndex(context.Background(), "ghost")
	require.NoError(t, err)
	require.False(t, valid)
	require.Empty(t, errMsg)
	require.Equal(t, "ghost", missing.Agent)
}

// TestResourceStore_MarkDeleted：删除只给指针打标记——`deleted_at` / `operation_id` /
// `updated_at` 刷新为本次删除，published 与 applied 不动；指针行与版本行都保留，版本
// 列表排除已删除资源，版本历史（含原 operation_id）原样可读；删除操作的终态在同一
// 事务内回填 success。
// 中文用例名：删除只标记指针、保留版本、同事务回填终态。
func TestResourceStore_MarkDeleted(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-md-pub")
	seedOperation(t, db, "op-md-del")
	store := NewResourceStore(db)
	ctx := context.Background()

	version, _, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:v1", "op-md-pub", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", version.ID, true, "op-md-pub", opNow))

	at := opNow.Add(2 * time.Minute)
	require.NoError(t, store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "op-md-del", at))

	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.NotNil(t, ptr.DeletedAt)
	require.Equal(t, at.UnixMilli(), *ptr.DeletedAt)
	require.Equal(t, at.UnixMilli(), ptr.UpdatedAt)
	require.Equal(t, "op-md-del", ptr.OperationID)
	require.Equal(t, version.ID, ptr.PublishedVersionID)
	require.NotNil(t, ptr.AppliedVersionID)
	require.Equal(t, version.ID, *ptr.AppliedVersionID)

	// 只读面排除已删除；版本历史完整保留（连原 operation_id 都没被改写）。
	list, err := store.ListPointers(ctx, core.ResourceModelConnection)
	require.NoError(t, err)
	require.Empty(t, list)

	versions, err := store.Versions(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	require.Equal(t, version.ID, versions[0].ID)
	require.Equal(t, "op-md-pub", versions[0].OperationID)

	// 删除操作的终态与指针标记同一事务落库。
	op := resourceStoreReadRawOperation(t, db, "op-md-del")
	require.Equal(t, string(core.OperationSuccess), op.Status)
	require.True(t, op.FinishedAt.Valid)
	require.Equal(t, at.UnixMilli(), op.FinishedAt.Int64)
}

// TestResourceStore_MarkDeletedKeepsVersionsAppendOnly：删除后版本仍在、可继续追加；
// 同名重建是**新 UID**——旧 UID 不得复活（删除即终态），新 UID 的版本号从 1 起，
// 指针换成新 UID 并清掉删除标记，旧 UID 的版本仍按 UID 可查（SC-017 取代 SC-005 的
// 「版本号继续递增」）。
// 中文用例名：删除后版本只追加、同名重建换新 UID 并从 1 计数。
func TestResourceStore_MarkDeletedKeepsVersionsAppendOnly(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-mv-all")
	store := NewResourceStore(db)
	ctx := context.Background()

	v1, _, err := store.EnsureVersion(ctx, modelVersion("openai", "sha256:v1", "op-mv-all", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", v1.ID, true, "op-mv-all", opNow))
	require.NoError(t, store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "op-mv-all",
		opNow.Add(time.Minute)))

	// 删除后按 id 仍可读旧版本（历史是可查证据）。
	got, err := store.VersionByID(ctx, v1.ID)
	require.NoError(t, err)
	require.Equal(t, v1, got)

	// 旧 UID 已在墓碑下：再追加同样 UID 的版本并发布必须被拒（UID 永不复用）。
	stale, inserted, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:v2", "op-mv-all", 0))
	require.NoError(t, err)
	require.True(t, inserted)
	require.ErrorIs(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", stale.ID, true, "op-mv-all", opNow.Add(time.Minute+time.Second)),
		core.ErrResourceUIDMismatch)

	// 同名重建 = 新实例：新 UID、版本号从 1 起。
	rebuilt := modelVersion("openai", "sha256:rebuild", "op-mv-all", 0)
	rebuilt.ResourceUID = resourceUIDFor(core.ResourceModelConnection, "openai#rebuild")
	v2, _, err := store.EnsureVersion(ctx, rebuilt)
	require.NoError(t, err)
	require.Equal(t, 1, v2.VersionNo)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", v2.ID, true, "op-mv-all", opNow.Add(2*time.Minute)))

	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Nil(t, ptr.DeletedAt)
	require.Equal(t, v2.ID, ptr.PublishedVersionID)
	require.Equal(t, rebuilt.ResourceUID, ptr.ResourceUID)

	list, err := store.ListPointers(ctx, core.ResourceModelConnection)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// 名称快照包含两代；UID 维度各自成线。服务层的「按名只回当前 UID」用后者落实。
	current, err := store.VersionsByUID(ctx, core.ResourceModelConnection, rebuilt.ResourceUID)
	require.NoError(t, err)
	require.Len(t, current, 1)
	require.Equal(t, 1, current[0].VersionNo)
	legacy, err := store.VersionsByUID(ctx, core.ResourceModelConnection, v1.ResourceUID)
	require.NoError(t, err)
	require.Len(t, legacy, 2)

	byName, err := store.Versions(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Len(t, byName, 3)
}

// TestResourceStore_MarkDeletedIsIdempotent：重复删除不新增行、不加版本；标记覆盖为
// 最近一次的时间与操作号，两次操作各自回填 success。
// 中文用例名：重复删除幂等，标记覆盖、行数不变。
func TestResourceStore_MarkDeletedIsIdempotent(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-idem-pub")
	seedOperation(t, db, "op-idem-1")
	seedOperation(t, db, "op-idem-2")
	store := NewResourceStore(db)
	ctx := context.Background()

	version, _, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:v1", "op-idem-pub", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", version.ID, true, "op-idem-pub", opNow))

	first := opNow.Add(time.Minute)
	second := opNow.Add(2 * time.Minute)
	require.NoError(t, store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "op-idem-1", first))
	require.NoError(t, store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "op-idem-2", second))

	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.NotNil(t, ptr.DeletedAt)
	require.Equal(t, second.UnixMilli(), *ptr.DeletedAt)
	require.Equal(t, second.UnixMilli(), ptr.UpdatedAt)
	require.Equal(t, "op-idem-2", ptr.OperationID)

	var pointerRows, versionRows int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM resource_pointers WHERE resource_kind = $1 AND resource_id = $2`,
		string(core.ResourceModelConnection), "openai").Scan(&pointerRows))
	require.Equal(t, 1, pointerRows)
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM resource_versions WHERE resource_kind = $1 AND resource_id = $2`,
		string(core.ResourceModelConnection), "openai").Scan(&versionRows))
	require.Equal(t, 1, versionRows)

	for _, opID := range []string{"op-idem-1", "op-idem-2"} {
		op := resourceStoreReadRawOperation(t, db, opID)
		require.Equal(t, string(core.OperationSuccess), op.Status, "operation %s", opID)
		require.True(t, op.FinishedAt.Valid)
	}
}

// TestResourceStore_MarkDeletedMissingPointer：指针行不存在返回 core.ErrResourceNotFound，
// 且不写操作终态（操作保持 pending，交由调用方 / 恢复路径处置）。
// 中文用例名：未命中指针返回 not-found，操作不终态化。
func TestResourceStore_MarkDeletedMissingPointer(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-miss-ptr")
	store := NewResourceStore(db)

	err := store.MarkDeleted(context.Background(), core.ResourceModelConnection, "ghost", "op-miss-ptr", opNow)
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	op := resourceStoreReadRawOperation(t, db, "op-miss-ptr")
	require.Equal(t, string(core.OperationPending), op.Status)
	require.False(t, op.FinishedAt.Valid)
}

// TestResourceStore_MarkDeletedMissingOperation：operation_id 外键要求删除操作已预写；
// 悬空操作号在标记指针时被拒，指针标记与终态都不落库。
// 中文用例名：删除必须挂已预写的操作，悬空操作号零写入。
func TestResourceStore_MarkDeletedMissingOperation(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-mo-pub")
	store := NewResourceStore(db)
	ctx := context.Background()

	version, _, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:v1", "op-mo-pub", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", version.ID, true, "op-mo-pub", opNow))

	err = store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "op-mo-ghost", opNow.Add(time.Minute))
	require.ErrorContains(t, err, "标记删除")

	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Nil(t, ptr.DeletedAt)
	require.Equal(t, "op-mo-pub", ptr.OperationID)
	require.Equal(t, opNow.UnixMilli(), ptr.UpdatedAt)
}

// TestResourceStore_MarkDeletedRollsBackWhenTerminalWriteFails：终态回填失败时（这里用
// 触发器让 management_operations 的 UPDATE 中止），指针上的删除标记必须一起回滚——
// 不允许"指针已删、操作还 pending"的中间态。
// 中文用例名：终态写失败时删除标记整笔回滚。
func TestResourceStore_MarkDeletedRollsBackWhenTerminalWriteFails(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-atomic-pub")
	seedOperation(t, db, "op-atomic-del")
	store := NewResourceStore(db)
	ctx := context.Background()

	version, _, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:v1", "op-atomic-pub", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", version.ID, true, "op-atomic-pub", opNow))

	// SQLite 的 `CREATE TRIGGER ... RAISE(ABORT)` 在 PostgreSQL 里对应
	// PL/pgSQL 异常触发器；函数与触发器都建在隔离 schema 内，随 schema 一起回收。
	// The SQLite RAISE(ABORT) trigger becomes a PL/pgSQL exception trigger.
	_, err = db.pool.ExecContext(ctx,
		`CREATE FUNCTION resource_store_block_terminal_write() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'terminal write blocked'; END; $$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx,
		`CREATE TRIGGER resource_store_block_terminal_write BEFORE UPDATE ON management_operations
		 FOR EACH ROW EXECUTE FUNCTION resource_store_block_terminal_write()`)
	require.NoError(t, err)

	err = store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "op-atomic-del",
		opNow.Add(time.Minute))
	require.ErrorContains(t, err, "回填管理操作")

	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Nil(t, ptr.DeletedAt)
	require.Equal(t, "op-atomic-pub", ptr.OperationID)
}

// TestResourceStore_MarkDeletedValidatesInput：未知类型、空标识、空操作号在开启事务前
// 拒绝，已发布指针逐字段不变。
// 中文用例名：非法删除输入零写入。
func TestResourceStore_MarkDeletedValidatesInput(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-val-pub")
	store := NewResourceStore(db)
	ctx := context.Background()

	version, _, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:v1", "op-val-pub", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", version.ID, true, "op-val-pub", opNow))

	require.Error(t, store.MarkDeleted(ctx, core.ResourceKind("unknown"), "openai", "op-val-pub", opNow))
	require.Error(t, store.MarkDeleted(ctx, core.ResourceModelConnection, "", "op-val-pub", opNow))
	require.Error(t, store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "", opNow))

	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Nil(t, ptr.DeletedAt)
	require.Equal(t, "op-val-pub", ptr.OperationID)
	require.Equal(t, opNow.UnixMilli(), ptr.UpdatedAt)
}

// TestResourceStore_MarkDeletedLeavesOtherResources：删除只作用于目标资源——同类其他
// 指针不受影响（仍在未删除列表、标记为空），目标指针行保留作已删除证据。
// 中文用例名：删除不波及其他资源。
func TestResourceStore_MarkDeletedLeavesOtherResources(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-other-pub")
	seedOperation(t, db, "op-other-del")
	store := NewResourceStore(db)
	ctx := context.Background()

	openai, _, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:v1", "op-other-pub", 0))
	require.NoError(t, err)
	local, _, err := store.EnsureVersion(ctx,
		modelVersion("local", "sha256:v1", "op-other-pub", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", openai.ID, true, "op-other-pub", opNow))
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"local", local.ID, true, "op-other-pub", opNow))

	require.NoError(t, store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "op-other-del",
		opNow.Add(time.Minute)))

	list, err := store.ListPointers(ctx, core.ResourceModelConnection)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "local", list[0].ResourceID)

	localPtr, err := store.Pointer(ctx, core.ResourceModelConnection, "local")
	require.NoError(t, err)
	require.Nil(t, localPtr.DeletedAt)
	require.Equal(t, "op-other-pub", localPtr.OperationID)

	// 目标指针行没有删除：仍可按名读回带删除标记的证据行。
	openaiPtr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.NotNil(t, openaiPtr.DeletedAt)

	versions, err := store.Versions(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Len(t, versions, 1)
}

// TestResourceStore_EnsureVersionRequiresOperation：版本行的 operation_id 外键指向管理
// 操作——没有预写操作就建不了版本，发布路径无法绕过审计（宪法 V）。
// 中文用例名：版本行必须挂已预写的操作。
func TestResourceStore_EnsureVersionRequiresOperation(t *testing.T) {
	db := portTestDB(t)
	store := NewResourceStore(db)

	_, _, err := store.EnsureVersion(context.Background(),
		modelVersion("openai", "sha256:v1", "op-missing", 0))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "FOREIGN KEY") || strings.Contains(err.Error(), "constraint"),
		"期望外键拒绝，实际 %v", err)

	var count int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM resource_versions`).Scan(&count))
	require.Zero(t, count)
}

// TestResourceStore_ConcurrentVersionNo：并发发布同一资源的不同内容——版本号在写连接上
// 自动取号（MAX+1），不重复、不塌缩，指针最终指向本次发布的某个版本（012-race 必跑）。
// 同一事务内完成取号、指针更新与操作终态回填是本用例的判据面。
// TestResourceStore_ConcurrentVersionNo pins auto-numbering under concurrency: unique,
// gap-free version numbers and a pointer that resolves to one of the winners.
func TestResourceStore_ConcurrentVersionNo(t *testing.T) {
	db := portTestDB(t)
	store := NewResourceStore(db)

	const workers = 8
	operationIDs := make([]string, workers)
	for i := 0; i < workers; i++ {
		operationIDs[i] = fmt.Sprintf("op-race-%02d", i)
		seedOperation(t, db, operationIDs[i])
	}

	start := make(chan struct{})
	errs := make([]error, workers)
	versions := make([]core.ResourceVersion, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			versions[index], errs[index] = store.PublishWithVersionState(
				context.Background(),
				modelVersion("shared", fmt.Sprintf("sha256:race-%02d", index), operationIDs[index], 0),
				true, operationIDs[index], opNow)
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[int]string, workers)
	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i], "worker %d", i)
		require.NotZero(t, versions[i].VersionNo)
		require.Zero(t, seen[versions[i].VersionNo], "版本号 %d 重复", versions[i].VersionNo)
		seen[versions[i].VersionNo] = versions[i].ContentHash
	}
	require.Len(t, seen, workers)
	for number := 1; number <= workers; number++ {
		require.Contains(t, seen, number, "版本号必须从 1 起连续")
	}

	stored, err := store.Versions(context.Background(), core.ResourceModelConnection, "shared")
	require.NoError(t, err)
	require.Len(t, stored, workers)

	winners := make(map[int64]bool, workers)
	for _, version := range versions {
		winners[version.ID] = true
	}
	ptr, err := store.Pointer(context.Background(), core.ResourceModelConnection, "shared")
	require.NoError(t, err)
	require.True(t, winners[ptr.PublishedVersionID], "指针必须指向本次发布的某个版本")
	require.NotNil(t, ptr.AppliedVersionID)
	require.Equal(t, ptr.PublishedVersionID, *ptr.AppliedVersionID,
		"applied=true 的发布必须同时推进实际生效指针")
}

// 以下用例补 SQLite 谱系未覆盖的方法：按 UID 的指针 / 引用读取、严格 UID 删除、
// applied 指针的条件写（CAS）、使用范围转换、身份镜像与迁移回填。判据面与既有用例
// 同口径：失败零写入、身份不按名称猜测、占用证据逐项点名。
// The cases below cover the methods the SQLite lineage left untested: uid-scoped reads,
// the strict-uid tombstone, the applied-pointer CAS, scope conversion and the identity
// mirror with its migration backfill.

// TestResourceStore_PointerAndReferenceByUID：按资源身份读指针与未释放引用——同名重建
// 的两代互不串线；非法 UID 在查询前拒绝，未命中返回 not-found。
// 中文验收点：身份维度的读取不经过名称猜测。
func TestResourceStore_PointerAndReferenceByUID(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-uid-1")
	store := NewResourceStore(db)
	ctx := context.Background()

	version, _, err := store.EnsureVersion(ctx, modelVersion("openai", "sha256:v1", "op-uid-1", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", version.ID, true, "op-uid-1", opNow))

	ptr, err := store.PointerByUID(ctx, core.ResourceModelConnection, version.ResourceUID)
	require.NoError(t, err)
	require.Equal(t, "openai", ptr.ResourceID)
	require.Equal(t, version.ResourceUID, ptr.ResourceUID)
	require.Equal(t, version.ID, ptr.PublishedVersionID)

	_, err = store.PointerByUID(ctx, core.ResourceModelConnection,
		resourceUIDFor(core.ResourceModelConnection, "ghost"))
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	_, err = store.PointerByUID(ctx, core.ResourceModelConnection, core.ResourceUID("rs_bogus"))
	require.Error(t, err)

	resourceStoreSeedRunRow(t, db, "run-uid-1", "running", sql.NullString{}, nil, nil)
	resourceStoreSeedRunRow(t, db, "run-uid-2", "running", sql.NullString{}, nil, nil)
	insertRef := func(runID string, releasedAt any) {
		t.Helper()
		_, err := db.pool.ExecContext(ctx,
			`INSERT INTO run_resource_refs
			   (run_id, resource_kind, resource_id, resource_uid, version_id, content_hash, released_at)
			 VALUES ($1, $2, 'openai', $3, $4, 'sha256:v1', $5)`,
			runID, string(core.ResourceModelConnection), string(version.ResourceUID),
			version.ID, releasedAt)
		require.NoError(t, err)
	}
	insertRef("run-uid-2", nil)
	insertRef("run-uid-1", opNow.UnixMilli()) // 已释放：不计入

	runIDs, err := store.ReferencedByUID(ctx, core.ResourceModelConnection, version.ResourceUID)
	require.NoError(t, err)
	require.Equal(t, []string{"run-uid-2"}, runIDs)

	_, err = store.ReferencedByUID(ctx, core.ResourceModelConnection, core.ResourceUID("rs_bogus"))
	require.Error(t, err)
	_, err = store.VersionsByUID(ctx, core.ResourceModelConnection, core.ResourceUID("rs_bogus"))
	require.Error(t, err)
}

// TestResourceStore_MarkDeletedByUID：严格身份删除——UID 与名称当前指针不一致即零写入；
// 悬空操作号被外键拒绝且不回填终态；匹配时与普通删除同语义（墓碑 + 终态同事务）。
// 中文验收点：条件写的存储侧判据按 UID 裁决。
func TestResourceStore_MarkDeletedByUID(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-byuid-pub")
	seedOperation(t, db, "op-byuid-del")
	store := NewResourceStore(db)
	ctx := context.Background()

	version, _, err := store.EnsureVersion(ctx, modelVersion("openai", "sha256:v1", "op-byuid-pub", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", version.ID, true, "op-byuid-pub", opNow))

	// 身份不一致：ErrResourceUIDMismatch，指针零写入。
	err = store.MarkDeletedByUID(ctx, core.ResourceModelConnection, "openai",
		resourceUIDFor(core.ResourceModelConnection, "other"), "op-byuid-del", opNow.Add(time.Minute))
	require.ErrorIs(t, err, core.ErrResourceUIDMismatch)
	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Nil(t, ptr.DeletedAt)
	require.Equal(t, "op-byuid-pub", ptr.OperationID)

	// 输入校验：未知类型 / 空标识 / 空操作号 / 非法 UID 都在写库前拒绝。
	require.Error(t, store.MarkDeletedByUID(ctx, core.ResourceKind("unknown"), "openai",
		version.ResourceUID, "op-byuid-del", opNow))
	require.Error(t, store.MarkDeletedByUID(ctx, core.ResourceModelConnection, "",
		version.ResourceUID, "op-byuid-del", opNow))
	require.Error(t, store.MarkDeletedByUID(ctx, core.ResourceModelConnection, "openai",
		version.ResourceUID, "", opNow))
	require.Error(t, store.MarkDeletedByUID(ctx, core.ResourceModelConnection, "openai",
		core.ResourceUID("rs_bogus"), "op-byuid-del", opNow))

	// 未命中的指针返回 not-found（0 行的分诊）。
	err = store.MarkDeletedByUID(ctx, core.ResourceModelConnection, "ghost",
		resourceUIDFor(core.ResourceModelConnection, "ghost"), "op-byuid-del", opNow)
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	// 悬空操作号在标记指针时被外键拒绝，指针标记与终态都不落库。
	err = store.MarkDeletedByUID(ctx, core.ResourceModelConnection, "openai",
		version.ResourceUID, "op-byuid-missing", opNow.Add(time.Minute))
	require.ErrorContains(t, err, "标记删除")
	ptr, err = store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Nil(t, ptr.DeletedAt)

	// 匹配 UID：墓碑与终态同事务落库。
	at := opNow.Add(2 * time.Minute)
	require.NoError(t, store.MarkDeletedByUID(ctx, core.ResourceModelConnection,
		"openai", version.ResourceUID, "op-byuid-del", at))
	ptr, err = store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.NotNil(t, ptr.DeletedAt)
	require.Equal(t, at.UnixMilli(), *ptr.DeletedAt)
	op := resourceStoreReadRawOperation(t, db, "op-byuid-del")
	require.Equal(t, string(core.OperationSuccess), op.Status)
	require.Equal(t, at.UnixMilli(), op.FinishedAt.Int64)
}

// TestResourceStore_MarkDeletedDefenceBranches：两条防御分支——指针缺资源 UID 时拒绝
// 猜身份；身份镜像行缺失时整笔回滚（不留"指针已删、身份还在"的中间态）。
// 中文验收点：库被外部改写时 fail-closed，不按名称补身份。
func TestResourceStore_MarkDeletedDefenceBranches(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-def-pub")
	seedOperation(t, db, "op-def-del")
	store := NewResourceStore(db)
	ctx := context.Background()

	version, _, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:v1", "op-def-pub", 0))
	require.NoError(t, err)

	// ① 指针行缺 resource_uid：删除拒绝，不按名称猜身份。
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO resource_pointers
		   (resource_kind, resource_id, resource_uid, published_version_id, applied_version_id,
		    scope, owner_agent_uid, revision, deleted_at, updated_at, operation_id)
		 VALUES ($1, 'openai', NULL, $2, NULL, 'shared', NULL, 1, NULL, $3, 'op-def-pub')`,
		string(core.ResourceModelConnection), version.ID, opNow.UnixMilli())
	require.NoError(t, err)
	err = store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "op-def-del", opNow)
	require.ErrorContains(t, err, "缺少资源 UID")

	// ② 指针存在但身份镜像行缺失：墓碑整笔回滚。
	orphanUID := resourceUIDFor(core.ResourceModelConnection, "orphan")
	_, err = db.pool.ExecContext(ctx,
		`UPDATE resource_pointers
		    SET resource_uid = $1, deleted_at = NULL
		  WHERE resource_kind = $2 AND resource_id = 'openai'`,
		string(orphanUID), string(core.ResourceModelConnection))
	require.NoError(t, err)
	err = store.MarkDeletedByUID(ctx, core.ResourceModelConnection, "openai",
		orphanUID, "op-def-del", opNow.Add(time.Minute))
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Nil(t, ptr.DeletedAt, "身份墓碑失败时指针标记必须一起回滚")
}

// TestResourceStore_CASApplied：受控重建的 applied 指针条件写——三项期望全部成立才推进；
// 期望不成立返回 (false, nil)（不是错误）；已删除 / 缺行一律不写。
// 中文验收点：CAS 只看冻结读数，不猜当前状态。
func TestResourceStore_CASApplied(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-cas-1")
	seedOperation(t, db, "op-cas-2")
	seedOperation(t, db, "op-cas-del")
	store := NewResourceStore(db)
	ctx := context.Background()

	first, err := store.PublishWithVersion(ctx,
		modelVersion("openai", "sha256:v1", "op-cas-1", 0), "op-cas-1", opNow)
	require.NoError(t, err)
	second, _, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:v2", "op-cas-2", 0))
	require.NoError(t, err)

	// 期望全部成立：applied 推进到目标版本，revision +1，操作归属刷新。
	ok, err := store.CASApplied(ctx, core.ResourceModelConnection, "openai",
		first.ID, first.ID, second.ID, "op-cas-2", opNow.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Equal(t, first.ID, ptr.PublishedVersionID)
	require.NotNil(t, ptr.AppliedVersionID)
	require.Equal(t, second.ID, *ptr.AppliedVersionID)
	require.Equal(t, 2, ptr.Revision)
	require.Equal(t, "op-cas-2", ptr.OperationID)

	// 期望不成立（applied 已不是旧值）：false + nil 错误，不当作存储故障。
	ok, err = store.CASApplied(ctx, core.ResourceModelConnection, "openai",
		first.ID, first.ID, first.ID, "op-cas-2", opNow.Add(2*time.Minute))
	require.NoError(t, err)
	require.False(t, ok)

	// 零值时间走当前时刻；目标与原值相同时仍是一次成功提交（revision 继续 +1）。
	ok, err = store.CASApplied(ctx, core.ResourceModelConnection, "openai",
		first.ID, second.ID, second.ID, "op-cas-2", time.Time{})
	require.NoError(t, err)
	require.True(t, ok)

	// 输入校验：未知类型 / 空标识在写库前拒绝。
	_, err = store.CASApplied(ctx, core.ResourceKind("unknown"), "openai", 1, 1, 1, "op-cas-2", opNow)
	require.Error(t, err)
	_, err = store.CASApplied(ctx, core.ResourceModelConnection, "", 1, 1, 1, "op-cas-2", opNow)
	require.Error(t, err)

	// 已删除的资源：CAS 不生效（deleted_at IS NULL 是期望的一部分）。
	require.NoError(t, store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "op-cas-del",
		opNow.Add(3*time.Minute)))
	ok, err = store.CASApplied(ctx, core.ResourceModelConnection, "openai",
		first.ID, second.ID, first.ID, "op-cas-2", opNow.Add(4*time.Minute))
	require.NoError(t, err)
	require.False(t, ok)
}

// TestResourceStore_ConvertScopeToExclusive：共享转专属在同一事务内重读目标、收集占用
// 证据并写范围——绑定投影 / 未释放运行引用 / 定义扫描 / 路由阻断任一命中即 409 且零写入；
// 证据清零后转换成功并把 revision +1，转回共享清空属主。
// 中文验收点：有阻断不写、无阻断才写、身份不符不猜。
func TestResourceStore_ConvertScopeToExclusive(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-scope-pub")
	seedOperation(t, db, "op-scope-ex")
	seedOperation(t, db, "op-scope-sh")
	store := NewResourceStore(db)
	ctx := context.Background()
	owner := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B")

	version, _, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:v1", "op-scope-pub", 0))
	require.NoError(t, err)
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", version.ID, true, "op-scope-pub", opNow))

	// 占用证据：另一个 Agent 的绑定投影 + 未释放运行引用。
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO agent_resource_bindings
		   (agent_uid, agent_name, resource_kind, ref_name, resource_uid, content_hash, projected_at)
		 VALUES ($1, 'writer', 'model_connection', 'openai', $2, 'sha256:v1', $3)`,
		string(testAgentUID), string(version.ResourceUID), opNow.UnixMilli())
	require.NoError(t, err)
	resourceStoreSeedRunRow(t, db, "run-scope-1", "running", sql.NullString{}, nil, nil)
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO run_resource_refs
		   (run_id, resource_kind, resource_id, resource_uid, version_id, content_hash, released_at)
		 VALUES ('run-scope-1', $1, 'openai', $2, $3, 'sha256:v1', NULL)`,
		string(core.ResourceModelConnection), string(version.ResourceUID), version.ID)
	require.NoError(t, err)

	blocked, err := store.ConvertScope(ctx, ScopeConversion{
		Kind:          core.ResourceModelConnection,
		Name:          "openai",
		ExpectUID:     version.ResourceUID,
		Target:        core.ScopeExclusive,
		OwnerAgentUID: owner,
		DocumentRefs:  []string{"writer"},
		RouteBlockers: []string{"route:ops"},
		OperationID:   "op-scope-ex",
		At:            opNow,
	})
	require.ErrorIs(t, err, core.ErrResourceScopeInUse)
	require.Equal(t, []core.AgentUID{testAgentUID}, blocked.BoundAgents)
	require.Equal(t, []string{"run-scope-1"}, blocked.ActiveRuns)
	require.Equal(t, []string{"writer"}, blocked.DocumentRefs)
	require.Equal(t, []string{"route:ops"}, blocked.MessageRoutes)

	// 有阻断 = 零写入：指针仍是共享、属主为空、revision 未动。
	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Equal(t, core.ScopeShared, ptr.Scope)
	require.Empty(t, ptr.OwnerAgentUID)
	require.Equal(t, 1, ptr.Revision)

	// 身份不符：拒绝且零写入。
	_, err = store.ConvertScope(ctx, ScopeConversion{
		Kind:          core.ResourceModelConnection,
		Name:          "openai",
		ExpectUID:     resourceUIDFor(core.ResourceModelConnection, "other"),
		Target:        core.ScopeExclusive,
		OwnerAgentUID: owner,
		OperationID:   "op-scope-ex",
		At:            opNow,
	})
	require.ErrorIs(t, err, core.ErrResourceUIDMismatch)

	// 证据清零后转换成功。
	_, err = db.pool.ExecContext(ctx, `DELETE FROM agent_resource_bindings`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `DELETE FROM run_resource_refs`)
	require.NoError(t, err)
	usage, err := store.ConvertScope(ctx, ScopeConversion{
		Kind:          core.ResourceModelConnection,
		Name:          "openai",
		ExpectUID:     version.ResourceUID,
		Target:        core.ScopeExclusive,
		OwnerAgentUID: owner,
		OperationID:   "op-scope-ex",
		At:            opNow.Add(time.Minute),
	})
	require.NoError(t, err)
	require.False(t, usage.InUse())
	ptr, err = store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Equal(t, core.ScopeExclusive, ptr.Scope)
	require.Equal(t, owner, ptr.OwnerAgentUID)
	require.Equal(t, 2, ptr.Revision)
	require.Equal(t, "op-scope-ex", ptr.OperationID)

	// 转回共享：属主清空（共享范围不得带属主）。
	_, err = store.ConvertScope(ctx, ScopeConversion{
		Kind:        core.ResourceModelConnection,
		Name:        "openai",
		ExpectUID:   version.ResourceUID,
		Target:      core.ScopeShared,
		OperationID: "op-scope-sh",
		At:          opNow.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	ptr, err = store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Equal(t, core.ScopeShared, ptr.Scope)
	require.Empty(t, ptr.OwnerAgentUID)
	require.Equal(t, 3, ptr.Revision)
}

// TestResourceStore_ConvertScopeValidationAndNotifyChannel：范围转换的输入判卷逐条拒绝；
// 通知渠道没有版本表，目标身份从渠道行读取，占用证据走 `run_notify_refs`，写的是
// 渠道行的范围列；已删除渠道返回 not-found。
// 中文验收点：渠道独立成线、非法输入零写入。
func TestResourceStore_ConvertScopeValidationAndNotifyChannel(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-nc-1")
	store := NewResourceStore(db)
	ctx := context.Background()
	owner := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B")
	channelUID := resourceUIDFor(core.ResourceNotifyChannel, "ops")

	base := ScopeConversion{
		Kind:          core.ResourceNotifyChannel,
		Name:          "ops",
		ExpectUID:     channelUID,
		Target:        core.ScopeExclusive,
		OwnerAgentUID: owner,
		OperationID:   "op-nc-1",
		At:            opNow,
	}

	// 输入判卷：覆盖 normalizeScopeConversion 的每条拒绝分支。
	unknown := base
	unknown.Kind = core.ResourceSkill
	_, err := store.ConvertScope(ctx, unknown)
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	noName := base
	noName.Name = ""
	_, err = store.ConvertScope(ctx, noName)
	require.Error(t, err)

	badUID := base
	badUID.ExpectUID = core.ResourceUID("rs_bogus")
	_, err = store.ConvertScope(ctx, badUID)
	require.Error(t, err)

	badScope := base
	badScope.Target = core.ResourceScope("global")
	_, err = store.ConvertScope(ctx, badScope)
	require.Error(t, err)

	noOwner := base
	noOwner.OwnerAgentUID = ""
	_, err = store.ConvertScope(ctx, noOwner)
	require.Error(t, err)

	ownerOnShared := base
	ownerOnShared.Target = core.ScopeShared
	_, err = store.ConvertScope(ctx, ownerOnShared)
	require.Error(t, err)

	// 渠道尚未登记：not-found。
	_, err = store.ConvertScope(ctx, base)
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO notify_channels
		   (resource_uid, name, type, url, url_host, credential_mode, scope, owner_agent_uid,
		    credential_id, revision, description, created_at, updated_at, deleted_at, operation_id)
		 VALUES ($1, 'ops', 'webhook', 'https://example.test/hook', 'example.test', 'env',
		         'shared', NULL, NULL, 1, '', $2, $2, NULL, 'op-nc-1')`,
		string(channelUID), opNow.UnixMilli())
	require.NoError(t, err)

	// 渠道占用证据：绑定投影 + 未释放的 run_notify_refs。
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO agent_resource_bindings
		   (agent_uid, agent_name, resource_kind, ref_name, resource_uid, content_hash, projected_at)
		 VALUES ($1, 'writer', 'notify_channel', 'ops', $2, 'sha256:v1', $3)`,
		string(testAgentUID), string(channelUID), opNow.UnixMilli())
	require.NoError(t, err)
	resourceStoreSeedRunRow(t, db, "run-nc-1", "running", sql.NullString{}, nil, nil)
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO run_notify_refs (run_id, agent_uid, channel_uid, channel_name, released_at)
		 VALUES ('run-nc-1', $1, $2, 'ops', NULL)`,
		string(testAgentUID), string(channelUID))
	require.NoError(t, err)

	blocked, err := store.ConvertScope(ctx, base)
	require.ErrorIs(t, err, core.ErrResourceScopeInUse)
	require.Equal(t, []core.AgentUID{testAgentUID}, blocked.BoundAgents)
	require.Equal(t, []string{"run-nc-1"}, blocked.ActiveRuns)

	var scope string
	var ownerAgent sql.NullString
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT scope, owner_agent_uid FROM notify_channels WHERE resource_uid = $1`,
		string(channelUID)).Scan(&scope, &ownerAgent))
	require.Equal(t, string(core.ScopeShared), scope, "有阻断不得写范围")
	require.False(t, ownerAgent.Valid)

	// 证据清零后转换成功：渠道行范围与属主同事务写入。
	_, err = db.pool.ExecContext(ctx, `DELETE FROM agent_resource_bindings`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `DELETE FROM run_notify_refs`)
	require.NoError(t, err)
	at := opNow.Add(time.Minute)
	_, err = store.ConvertScope(ctx, base)
	require.NoError(t, err)
	var revision int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT scope, owner_agent_uid, revision FROM notify_channels WHERE resource_uid = $1`,
		string(channelUID)).Scan(&scope, &ownerAgent, &revision))
	require.Equal(t, string(core.ScopeExclusive), scope)
	require.Equal(t, string(owner), ownerAgent.String)
	require.Equal(t, 2, revision)

	// 已删除的渠道：当前目标不再是活动实例。
	_, err = db.pool.ExecContext(ctx,
		`UPDATE notify_channels SET deleted_at = $1 WHERE resource_uid = $2`,
		at.UnixMilli(), string(channelUID))
	require.NoError(t, err)
	_, err = store.ConvertScope(ctx, base)
	require.ErrorIs(t, err, core.ErrResourceNotFound)
}

// TestResourceStore_AgentIdentityMirror：身份镜像的读写与反向查名——没有索引行不是故障；
// 归档 Agent 的名字回退到归档索引（未恢复的那条），已恢复的行不再参与。
// 中文验收点：查不到名字不等于没有属主，也不写成空属主。
func TestResourceStore_AgentIdentityMirror(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-id-1")
	store := NewResourceStore(db)
	ctx := context.Background()
	archivedUID := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B")
	restoredUID := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C")

	// 没有索引行：found=false，不报错。
	uid, source, found, err := store.AgentIdentity(ctx, "ghost")
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, uid)
	require.Empty(t, source)

	// 索引行带身份：镜像来源是状态文件。
	require.NoError(t, store.PutLifecycleIndex(ctx, core.LifecycleRecord{
		SchemaVersion: 1,
		Agent:         "writer",
		AgentUID:      testAgentUID,
		State:         core.AgentActive,
		Generation:    1,
		UpdatedAt:     opNow.UnixMilli(),
		OperationID:   "op-id-1",
	}, true, ""))
	uid, source, found, err = store.AgentIdentity(ctx, "writer")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, testAgentUID, uid)
	require.Equal(t, "state_file", source)

	name, ok, err := store.AgentNameByUID(ctx, testAgentUID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "writer", name)

	// 空 UID 与未记录 UID：查不到，不报错。
	_, ok, err = store.AgentNameByUID(ctx, "")
	require.NoError(t, err)
	require.False(t, ok)
	_, ok, err = store.AgentNameByUID(ctx, restoredUID)
	require.NoError(t, err)
	require.False(t, ok)

	// 归档 Agent：未恢复的归档行给出名字；已恢复的行不让 UID 复活。
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO agent_archives
		   (archive_id, agent_name, agent_uid, uid_source, archived_at, content_hash,
		    manifest_json, operation_id, restored_at)
		 VALUES ('arch-1', 'old-writer', $1, 'archive', $2, 'sha256:x', '[]', 'op-id-1', NULL)`,
		string(archivedUID), opNow.UnixMilli())
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO agent_archives
		   (archive_id, agent_name, agent_uid, uid_source, archived_at, content_hash,
		    manifest_json, operation_id, restored_at)
		 VALUES ('arch-2', 'restored-writer', $1, 'archive', $2, 'sha256:y', '[]', 'op-id-1', $2)`,
		string(restoredUID), opNow.UnixMilli())
	require.NoError(t, err)

	name, ok, err = store.AgentNameByUID(ctx, archivedUID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "old-writer", name)
	_, ok, err = store.AgentNameByUID(ctx, restoredUID)
	require.NoError(t, err)
	require.False(t, ok)
}

// TestResourceStore_BackfillAgentUID：v3 迁移回填——两侧一致即幂等 no-op，任何一侧对不上
// 都中止且不覆盖；状态文件缺失按 legacy_unrecorded 记录，不按同名猜。
// 中文验收点：迁移只镜像权威身份，冲突即停。
func TestResourceStore_BackfillAgentUID(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-bf-1")
	store := NewResourceStore(db)
	ctx := context.Background()
	otherUID := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B")

	// 无行 + 有 UID：插入 state_file 行。
	require.NoError(t, store.BackfillAgentUID(ctx, "writer", testAgentUID, "op-bf-1", opNow))
	uid, source, found, err := store.AgentIdentity(ctx, "writer")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, testAgentUID, uid)
	require.Equal(t, "state_file", source)

	// 已一致：幂等 no-op（updated_at 不刷新）。
	before, valid, _, err := store.ReadLifecycleIndex(ctx, "writer")
	require.NoError(t, err)
	require.False(t, valid)
	require.NoError(t, store.BackfillAgentUID(ctx, "writer", testAgentUID, "op-bf-1", opNow.Add(time.Hour)))
	after, _, _, err := store.ReadLifecycleIndex(ctx, "writer")
	require.NoError(t, err)
	require.Equal(t, before.UpdatedAt, after.UpdatedAt)

	// 数据库身份与状态文件不一致：中止，不覆盖任何一侧。
	require.ErrorIs(t, store.BackfillAgentUID(ctx, "writer", otherUID, "op-bf-1", opNow),
		core.ErrLifecycleConflict)
	uid, _, _, err = store.AgentIdentity(ctx, "writer")
	require.NoError(t, err)
	require.Equal(t, testAgentUID, uid)

	// 数据库有身份、状态文件缺失：同样中止。
	require.ErrorIs(t, store.BackfillAgentUID(ctx, "writer", "", "op-bf-1", opNow),
		core.ErrLifecycleConflict)

	// 行存在但镜像为空（uid_source NULL）：由 UPDATE 路径补写。
	require.NoError(t, store.PutLifecycleIndex(ctx, core.LifecycleRecord{
		SchemaVersion: 1,
		Agent:         "pending",
		State:         core.AgentActive,
		Generation:    1,
		UpdatedAt:     opNow.UnixMilli(),
		OperationID:   "op-bf-1",
	}, true, ""))
	require.NoError(t, store.BackfillAgentUID(ctx, "pending", otherUID, "op-bf-1", opNow))
	uid, source, found, err = store.AgentIdentity(ctx, "pending")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, otherUID, uid)
	require.Equal(t, "state_file", source)

	// 无行 + 无 UID：legacy_unrecorded，不按同名猜身份。
	require.NoError(t, store.BackfillAgentUID(ctx, "legacy", "", "op-bf-1", opNow))
	uid, source, found, err = store.AgentIdentity(ctx, "legacy")
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, uid)
	require.Equal(t, "legacy_unrecorded", source)

	// 输入校验：空 Agent 与非法 UID 在写库前拒绝。
	require.Error(t, store.BackfillAgentUID(ctx, "", testAgentUID, "op-bf-1", opNow))
	require.Error(t, store.BackfillAgentUID(ctx, "writer", core.AgentUID("bogus"), "op-bf-1", opNow))
}

// TestResourceStore_WriteValidationAndZeroTime：写入口的必填校验与零值时间——非法输入
// 零写入；零值时刻落库时取当前毫秒（NOT NULL 列不用零值冒充）。
// 中文验收点：校验先于 SQL，零值时间不落成 1970。
func TestResourceStore_WriteValidationAndZeroTime(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-zero-1")
	seedOperation(t, db, "op-zero-2")
	store := NewResourceStore(db)
	ctx := context.Background()

	require.Error(t, store.Publish(ctx, core.ResourceKind("unknown"), "openai", 1, true, "op-zero-1", opNow))
	require.Error(t, store.Publish(ctx, core.ResourceModelConnection, "", 1, true, "op-zero-1", opNow))
	require.Error(t, store.PutLifecycleIndex(ctx, core.LifecycleRecord{Agent: ""}, true, ""))

	// 零值 CreatedAt：插入时取当前毫秒。
	zero := modelVersion("openai", "sha256:zero", "op-zero-1", 0)
	zero.CreatedAt = 0
	version, inserted, err := store.EnsureVersion(ctx, zero)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NotZero(t, version.CreatedAt)

	// 发布的操作号必填。
	_, err = store.PublishWithVersionState(ctx,
		modelVersion("openai", "sha256:zero2", "op-zero-1", 0), true, "", opNow)
	require.Error(t, err)

	// 零值删除时刻：取当前时刻，墓碑照常落库。
	require.NoError(t, store.Publish(ctx, core.ResourceModelConnection,
		"openai", version.ID, true, "op-zero-1", opNow))
	require.NoError(t, store.MarkDeleted(ctx, core.ResourceModelConnection, "openai", "op-zero-2", time.Time{}))
	ptr, err := store.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.NotNil(t, ptr.DeletedAt)
	require.NotZero(t, *ptr.DeletedAt)
}

// TestResourceStore_EnsureVersionAgentBundle：`agent_bundle` 的 `ag_` 身份经**真实仓储
// API** 落库——`resource_uid` 与 `resource_id` 同为规范化 AgentUID；幂等键是内容哈希
// （bundle hash），二次调用 `inserted=false` 且版本号不变（US-24 §2.3.9 / P-19）。
// 中文用例名：资产 bundle 版本经仓储 API 插入且按哈希幂等。
func TestResourceStore_EnsureVersionAgentBundle(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-ab-1")
	store := NewResourceStore(db)
	ctx := context.Background()

	bundle := core.ResourceVersion{
		Kind:         core.ResourceAgentBundle,
		ResourceUID:  core.ResourceUID(testAgentUID),
		ResourceID:   string(testAgentUID),
		ContentHash:  "sha256:bundle-1",
		ManifestJSON: `{"format_version":1}`,
		StoragePath:  ".basalt/assets/bundles/writer/" + strings.Repeat("0", 64),
		CreatedAt:    opNow.UnixMilli(),
		OperationID:  "op-ab-1",
	}
	first, inserted, err := store.EnsureVersion(ctx, bundle)
	require.NoError(t, err)
	require.True(t, inserted)
	require.Equal(t, 1, first.VersionNo)
	require.Equal(t, core.ResourceUID(testAgentUID), first.ResourceUID)
	require.Equal(t, string(testAgentUID), first.ResourceID)

	second, inserted, err := store.EnsureVersion(ctx, bundle)
	require.NoError(t, err)
	require.False(t, inserted, "同一 bundle hash 二次写入必须是幂等 no-op")
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.VersionNo, second.VersionNo)

	// 发布指针仍走 `resource_pointers`（P-19 / §2.3.9）：读取路径不另开一张表，
	// 期望的 BundleHash 由 指针 → 版本行 content_hash 得到。
	require.NoError(t, store.Publish(ctx, core.ResourceAgentBundle,
		string(testAgentUID), first.ID, true, "op-ab-1", opNow))
	ptr, err := store.Pointer(ctx, core.ResourceAgentBundle, string(testAgentUID))
	require.NoError(t, err)
	require.Equal(t, first.ID, ptr.PublishedVersionID)
	require.Equal(t, core.ResourceUID(testAgentUID), ptr.ResourceUID)
}

// TestResourceStore_RejectsCrossKindUID：身份解析按 kind 分派——`agent_bundle` 只认
// `ag_`、`skill` 只认 `rs_`；**两个方向都拒绝**（只放宽一类等于只校验不校准），
// `agent_bundle` 也不接受用 Agent 名顶替 UID（存储层不按名称推断身份）。
// 中文用例名：跨种类身份双向拒绝、拒绝路径零写入。
func TestResourceStore_RejectsCrossKindUID(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "op-xk-1")
	store := NewResourceStore(db)
	ctx := context.Background()

	skillUID := resourceUIDFor(core.ResourceSkill, "research")
	_, _, err := store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceAgentBundle,
		ResourceUID:  skillUID,
		ResourceID:   string(skillUID),
		ContentHash:  "sha256:cross-1",
		ManifestJSON: `{"format_version":1}`,
		StoragePath:  ".basalt/assets/bundles/writer/x",
		OperationID:  "op-xk-1",
	})
	require.Error(t, err, "agent_bundle + rs_ 必须拒绝")

	_, _, err = store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceUID:  core.ResourceUID(testAgentUID),
		ResourceID:   string(testAgentUID),
		ContentHash:  "sha256:cross-2",
		ManifestJSON: `[]`,
		StoragePath:  ".basalt/versions/skills/research/1",
		OperationID:  "op-xk-1",
	})
	require.Error(t, err, "skill + ag_ 必须拒绝")

	_, _, err = store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceAgentBundle,
		ResourceUID:  core.ResourceUID(testAgentUID),
		ResourceID:   "writer",
		ContentHash:  "sha256:cross-3",
		ManifestJSON: `{"format_version":1}`,
		StoragePath:  ".basalt/assets/bundles/writer/x",
		OperationID:  "op-xk-1",
	})
	require.Error(t, err, "agent_bundle 的 resource_id 不能用 Agent 名顶替 AgentUID")

	var count int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM resource_versions`).Scan(&count))
	require.Zero(t, count, "三种拒绝都必须零写入")
}
