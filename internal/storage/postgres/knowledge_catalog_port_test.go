//go:build integration

package postgres

// 本文件是知识库**目录读面**（`knowledge_catalog.go`，US-16 §3.1.7 / T042）在
// PostgreSQL 谱系上的包级用例。该实现是 US-16 新增（SQLite 谱系没有同型仓储），
// 用例按实现契约新写，逐条钉住文件头的五条纪律：
//
//  1. **排序在 SQL 里**：集合 `name` 升序（并列按 `collection_id`）、文档
//     `source_path` 升序、版本 `created_at` 倒序（并列按 `version_id` 倒序）；
//  2. **ACL 顺序固定**：身份 → scope → 调用方项目 → 目标集合；集合不存在 / 已软删
//     按 `knowledge.ErrNotFound`，项目不匹配按 `core.ErrForbidden`——两种失败
//     **不得**合并成同一句话；
//  3. **`activated_at` 由 `updated_at` 推导**：DDL 没有激活时间列，只有 `active`
//     行带激活时刻，其余状态返回零值；
//  4. **同名软删集合按复活处理**：保留 `collection_id` 与 `created_at`，活行同名
//     仍按 `core.ErrConflict` 拒绝；
//  5. **计数不可省**：`DocumentCount` 是未删除文档数（软删文档不计），本实现
//     永远给出真实计数，负数（未统计）是留给未来适配器的语义。
//
// 逆改点（任一即红）：把 `ORDER BY` 去掉 → 排序断言红；把 NotFound 与 Forbidden
// 合并 → ACL 断言红；让复活退化成新插入 → `collection_id` / `created_at` 断言红；
// 把计数子查询的 `d.deleted_at IS NULL` 删掉 → 计数断言红；把 `active` 的
// `ActivatedAt` 改回零值 → 激活时刻断言红。
//
// The catalog store is new in US-16 and has no SQLite counterpart, so these cases are
// written against the implementation contract: SQL ordering, fixed ACL order, derived
// activation time, revival of soft-deleted namesakes and always-real document counts.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// catalogPrincipal 造一个外部调用方身份（不是内置管理员）：scope 精确匹配，
// 项目范围不放在身份里——它只能从 `callers` 行现读。
// catalogPrincipal builds an external caller identity; the project scope lives in the
// callers row, never in the principal.
func catalogPrincipal(callerID string, scopes ...string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: callerID,
		Actor:    core.ActorCaller,
		Scopes:   scopes,
	}
}

// catalogSeedCaller 种一行 `service` 调用方；项目与状态由用例指定。
// catalogSeedCaller inserts one service caller row with the given project and status.
func catalogSeedCaller(t *testing.T, db *DB, callerID, project, status string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, $2, $3, $4)`,
		callerID, project, status, opNow.UnixMilli())
	require.NoError(t, err)
}

// catalogSeedCollection 种一个集合；`deletedAt` 非 nil 表示已软删。
// catalogSeedCollection inserts one collection row, optionally soft-deleted.
func catalogSeedCollection(t *testing.T, db *DB, collectionID, name, project string,
	createdAt time.Time, deletedAt *time.Time,
) {
	t.Helper()
	var deleted any
	if deletedAt != nil {
		deleted = deletedAt.UnixMilli()
	}
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO kb_collections (collection_id, name, project, created_at, deleted_at)
		VALUES ($1, $2, $3, $4, $5)`,
		collectionID, name, project, createdAt.UnixMilli(), deleted)
	require.NoError(t, err)
}

// catalogDocumentRow 是一行文档的种子数据。`kb_documents.latest_version_id` 与
// `kb_document_versions.document_id` 互引，种子分两步：先插文档（指针为空），
// 插完版本再用 `catalogSetLatestVersion` 回填，规避循环外键。
// catalogDocumentRow seeds one document; the latest-version pointer is filled in after
// the version rows exist because the two tables reference each other.
type catalogDocumentRow struct {
	DocumentID   string
	CollectionID string
	SourcePath   string
	ContentHash  string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
}

// catalogSeedDocument 种一行文档（最新版本指针先留空）。
// catalogSeedDocument inserts one document with a NULL latest-version pointer.
func catalogSeedDocument(t *testing.T, db *DB, row catalogDocumentRow) {
	t.Helper()
	var deleted any
	if row.DeletedAt != nil {
		deleted = row.DeletedAt.UnixMilli()
	}
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO kb_documents
			(document_id, collection_id, source_path, content_hash, created_at, updated_at, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		row.DocumentID, row.CollectionID, row.SourcePath, row.ContentHash,
		row.CreatedAt.UnixMilli(), row.UpdatedAt.UnixMilli(), deleted)
	require.NoError(t, err)
}

// catalogSetLatestVersion 回填文档的最新版本指针（版本行必须已存在）。
// catalogSetLatestVersion points a document at an existing version row.
func catalogSetLatestVersion(t *testing.T, db *DB, documentID, versionID string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`UPDATE kb_documents SET latest_version_id = $2 WHERE document_id = $1`,
		documentID, versionID)
	require.NoError(t, err)
}

// catalogVersionRow 是一行版本的种子数据；`Generation` 为 nil 表示 NULL（未挂代际）。
// catalogVersionRow seeds one version row; a nil Generation means NULL.
type catalogVersionRow struct {
	VersionID      string
	DocumentID     string
	ContentHash    string
	Status         knowledge.VersionStatus
	ParserVersion  string
	ChunkerVersion string
	Generation     *int
	ErrorKind      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// catalogSeedVersion 种一行版本；`failed` 状态按 CHECK 必须带 error_kind。
// catalogSeedVersion inserts one version row; a failed status carries an error kind
// because the schema CHECK demands it.
func catalogSeedVersion(t *testing.T, db *DB, row catalogVersionRow) {
	t.Helper()
	var generation any
	if row.Generation != nil {
		generation = *row.Generation
	}
	var errorKind any
	if row.ErrorKind != "" {
		errorKind = row.ErrorKind
	}
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO kb_document_versions
			(version_id, document_id, content_hash, status, parser_version, chunker_version,
			 generation, error_kind, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		row.VersionID, row.DocumentID, row.ContentHash, string(row.Status),
		row.ParserVersion, row.ChunkerVersion, generation, errorKind,
		row.CreatedAt.UnixMilli(), row.UpdatedAt.UnixMilli())
	require.NoError(t, err)
}

// catalogCollectionNames 取出集合名称序列（顺序即仓储给出的顺序）。
// catalogCollectionNames extracts collection names in store order.
func catalogCollectionNames(records []KnowledgeCollectionRecord) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.Name)
	}
	return out
}

// catalogCollectionIDs 取出集合标识序列（判排序与复活时用）。
// catalogCollectionIDs extracts collection ids in store order.
func catalogCollectionIDs(records []KnowledgeCollectionRecord) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.CollectionID)
	}
	return out
}

// catalogDocumentPaths 取出文档来源路径序列。
// catalogDocumentPaths extracts source paths in store order.
func catalogDocumentPaths(records []KnowledgeDocumentRecord) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.SourcePath)
	}
	return out
}

// catalogVersionIDs 取出版本标识序列。
// catalogVersionIDs extracts version ids in store order.
func catalogVersionIDs(records []KnowledgeVersionRecord) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.VersionID)
	}
	return out
}

// catalogCollectionRows 直接读集合表的行数（判"被拒的写不留行"）。
// catalogCollectionRows counts collection rows straight from the table.
func catalogCollectionRows(t *testing.T, db *DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM kb_collections`).Scan(&count))
	return count
}

// catalogRequireCollectionIDShape 判集合主键形态：`col_` + 26 位 Crockford Base32，
// 与契约 `CollectionSummary.collection_id` 的声明一致。
// catalogRequireCollectionIDShape checks the `col_` + 26 Crockford chars contract form.
func catalogRequireCollectionIDShape(t *testing.T, collectionID string) {
	t.Helper()
	require.True(t, strings.HasPrefix(collectionID, knowledgeCollectionIDPrefix),
		"集合标识带 col_ 前缀，实际 %q", collectionID)
	payload := strings.TrimPrefix(collectionID, knowledgeCollectionIDPrefix)
	require.Len(t, payload, reservationPayloadLength, "载荷是 26 位 Crockford Base32")
	for _, r := range payload {
		require.Contains(t, crockfordAlphabet, string(r),
			"载荷字符 %q 不在 Crockford 字符集内", string(r))
	}
}

// TestKnowledgeCatalog_ListCollectionsFiltersAndCounts：列表的过滤、计数与时间列
// 还原。中文验收点：项目过滤按 `callers` 之外的管理面参数生效；软删集合默认不列；
// 计数只算未删除文档。
// 逆改点：把计数子查询的 `d.deleted_at IS NULL` 删掉 → alpha 的计数变成 2。
func TestKnowledgeCatalog_ListCollectionsFiltersAndCounts(t *testing.T) {
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	ctx := context.Background()

	deletedAt := opNow.Add(time.Hour)
	catalogSeedCollection(t, db, "col_alpha", "alpha", "proj-a", opNow, nil)
	catalogSeedCollection(t, db, "col_beta", "beta", "proj-a", opNow.Add(time.Minute), nil)
	catalogSeedCollection(t, db, "col_gamma", "gamma", "proj-b", opNow, nil)
	catalogSeedCollection(t, db, "col_delta", "delta", "proj-a", opNow, &deletedAt)

	// alpha 下两张文档：一张活着、一张软删；gamma 下也有一张活文档。
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_alpha_live", CollectionID: "col_alpha",
		SourcePath: "docs/a.md", ContentHash: "hash-a",
		CreatedAt: opNow, UpdatedAt: opNow,
	})
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_alpha_gone", CollectionID: "col_alpha",
		SourcePath: "docs/b.md", ContentHash: "hash-b",
		CreatedAt: opNow, UpdatedAt: opNow, DeletedAt: &deletedAt,
	})
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_gamma_live", CollectionID: "col_gamma",
		SourcePath: "docs/c.md", ContentHash: "hash-c",
		CreatedAt: opNow, UpdatedAt: opNow,
	})

	records, err := store.ListCollections(ctx, "", false)
	require.NoError(t, err)
	require.Len(t, records, 3, "默认不列软删集合")
	require.Equal(t, []string{"alpha", "beta", "gamma"}, catalogCollectionNames(records),
		"集合按 name 升序")

	require.Equal(t, "col_alpha", records[0].CollectionID)
	require.Equal(t, "proj-a", records[0].Project)
	require.Equal(t, 1, records[0].DocumentCount, "软删文档不计入计数")
	require.Equal(t, opNow.UnixMilli(), records[0].CreatedAt.UnixMilli(),
		"毫秒列按 UTC 还原")
	require.True(t, records[0].DeletedAt.IsZero(), "未删除集合的 DeletedAt 为零值")

	require.Equal(t, 0, records[1].DocumentCount, "空集合是 0，不是未统计 -1")
	require.Equal(t, 1, records[2].DocumentCount)
	require.Equal(t, "proj-b", records[2].Project)

	// 项目过滤带首尾空白也按投影处理；不匹配的项目返回空列表而不是错误。
	records, err = store.ListCollections(ctx, "  proj-a  ", false)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta"}, catalogCollectionNames(records),
		"project 过滤只留本项目")

	records, err = store.ListCollections(ctx, "proj-a", true)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta", "delta"}, catalogCollectionNames(records),
		"includeDeleted 为真才列出软删集合")
	require.Equal(t, deletedAt.UnixMilli(), records[2].DeletedAt.UnixMilli(),
		"软删时刻按毫秒列原样还原")

	records, err = store.ListCollections(ctx, "proj-missing", false)
	require.NoError(t, err)
	require.Empty(t, records, "不匹配的项目是空结果，不是错误")
}

// TestKnowledgeCatalog_ListCollectionsOrdersByNameThenID：并列排序的主键次序。
// 中文验收点：先按 name 升序，同名再按 collection_id 升序——同一份数据每次列出的
// 顺序可复现。
// 逆改点：把 `, c.collection_id ASC` 删掉 → 同名两行的顺序不可复现，断言可能红。
func TestKnowledgeCatalog_ListCollectionsOrdersByNameThenID(t *testing.T) {
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	ctx := context.Background()

	catalogSeedCollection(t, db, "col_z", "alpha", "", opNow, nil)
	catalogSeedCollection(t, db, "col_b", "shared", "proj-b", opNow, nil)
	catalogSeedCollection(t, db, "col_a", "shared", "proj-a", opNow, nil)

	records, err := store.ListCollections(ctx, "", false)
	require.NoError(t, err)
	require.Equal(t, []string{"col_z", "col_a", "col_b"}, catalogCollectionIDs(records),
		"先按 name 升序，同名再按 collection_id 升序")
}

// TestKnowledgeCatalog_CreateCollectionEnforcesIdentityAndScope：建集合的身份与
// scope 判定。中文验收点：零值身份 401、缺 scope 403、停用 / 不存在调用方 401、
// 空名 400，且全部拒绝路径零写入。
// 逆改点：把 scope 判定挪到写库之后 → 最后一行计数断言红。
func TestKnowledgeCatalog_CreateCollectionEnforcesIdentityAndScope(t *testing.T) {
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	ctx := context.Background()

	_, err := store.CreateCollection(ctx, core.Principal{}, "runbooks", "")
	require.ErrorIs(t, err, core.ErrUnauthorized, "零值身份不得建集合")

	catalogSeedCaller(t, db, "caller_reader", "proj-a", "active")
	_, err = store.CreateCollection(ctx,
		catalogPrincipal("caller_reader", core.ScopeKnowledgeRead), "runbooks", "")
	require.ErrorIs(t, err, core.ErrForbidden, "缺 knowledge:write 是越权，不是未认证")

	_, err = store.CreateCollection(ctx,
		catalogPrincipal("caller_ghost", core.ScopeKnowledgeWrite), "runbooks", "")
	require.ErrorIs(t, err, core.ErrUnauthorized, "不存在的调用方按未认证拒绝")

	catalogSeedCaller(t, db, "caller_off", "proj-a", "disabled")
	_, err = store.CreateCollection(ctx,
		catalogPrincipal("caller_off", core.ScopeKnowledgeWrite), "runbooks", "")
	require.ErrorIs(t, err, core.ErrUnauthorized, "停用的调用方按未认证拒绝")

	_, err = store.CreateCollection(ctx, core.LocalAdminPrincipal(), "   ", "")
	require.ErrorIs(t, err, knowledge.ErrInvalidRequest, "空白集合名在写库前拒绝")

	// 全部拒绝路径都零写入：被拒的请求不得留下半截集合。
	require.Equal(t, 0, catalogCollectionRows(t, db))
}

// TestKnowledgeCatalog_CreateCollectionBindsProject：项目归属规则。
// 中文验收点：未给 project 落到调用方项目；项目受限的调用方不得在别处建集合；
// 调用方项目为空（不限）时按请求原样落库。
// 逆改点：去掉 `callerProject != "" && project != callerProject` 判定 →
// 跨项目建集合的 ErrorIs 断言红。
func TestKnowledgeCatalog_CreateCollectionBindsProject(t *testing.T) {
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	ctx := context.Background()

	catalogSeedCaller(t, db, "caller_a", "proj-a", "active")
	caller := catalogPrincipal("caller_a", core.ScopeKnowledgeWrite)

	before := time.Now().UTC()
	created, err := store.CreateCollection(ctx, caller, "  runbooks  ", "")
	require.NoError(t, err)
	require.Equal(t, "runbooks", created.Name, "名称首尾空白投影掉")
	require.Equal(t, "proj-a", created.Project, "未给 project 时落到调用方自己的项目")
	require.Equal(t, 0, created.DocumentCount, "新集合计数为 0")
	require.True(t, created.DeletedAt.IsZero())
	require.WithinDuration(t, before, created.CreatedAt, time.Minute, "CreatedAt 取句柄时钟")
	catalogRequireCollectionIDShape(t, created.CollectionID)

	// 返回时刻与落库毫秒一致：视图拿到的就是这个值。
	var (
		storedProject   string
		storedCreatedAt int64
	)
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT project, created_at FROM kb_collections WHERE collection_id = $1`,
		created.CollectionID).Scan(&storedProject, &storedCreatedAt))
	require.Equal(t, "proj-a", storedProject)
	require.Equal(t, created.CreatedAt.UnixMilli(), storedCreatedAt)

	_, err = store.CreateCollection(ctx, caller, "other", "proj-b")
	require.ErrorIs(t, err, core.ErrForbidden, "项目受限的调用方不得在别的项目建集合")
	require.Equal(t, 1, catalogCollectionRows(t, db), "被拒的写不留行")

	// 项目字段同样去空白后才与调用方项目比较。
	scoped, err := store.CreateCollection(ctx, caller, "scoped", "  proj-a  ")
	require.NoError(t, err)
	require.Equal(t, "proj-a", scoped.Project)

	// 项目为空（不限）的调用方按请求原样落库；请求也空则落空项目。
	catalogSeedCaller(t, db, "caller_free", "", "active")
	free := catalogPrincipal("caller_free", core.ScopeKnowledgeWrite)
	elsewhere, err := store.CreateCollection(ctx, free, "elsewhere", "proj-c")
	require.NoError(t, err)
	require.Equal(t, "proj-c", elsewhere.Project, "不限项目的调用方按请求原样落库")

	global, err := store.CreateCollection(ctx, free, "global", "")
	require.NoError(t, err)
	require.Equal(t, "", global.Project)

	// 内置管理员的 callers 行 project 为空：同样不受项目限制。
	admin, err := store.CreateCollection(ctx, core.LocalAdminPrincipal(), "admin-made", "proj-d")
	require.NoError(t, err)
	require.Equal(t, "proj-d", admin.Project)
}

// TestKnowledgeCatalog_CreateCollectionConflictsAndRevivesNamesake：同名集合的
// 冲突与复活语义。中文验收点：活行同名 409；软删同名按复活处理，保留
// `collection_id` 与 `created_at`，并同时给出未删除文档数；复活按项目隔离。
// 逆改点：把 reviveCollection 改成直接返回冲突 → 复活断言红；让复活插新行 →
// collection_id / created_at / 行数断言红。
func TestKnowledgeCatalog_CreateCollectionConflictsAndRevivesNamesake(t *testing.T) {
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	ctx := context.Background()

	catalogSeedCaller(t, db, "caller_a", "proj-a", "active")
	caller := catalogPrincipal("caller_a", core.ScopeKnowledgeWrite)

	deletedAt := opNow.Add(time.Hour)
	catalogSeedCollection(t, db, "col_old", "shared", "proj-a", opNow, &deletedAt)
	catalogSeedCollection(t, db, "col_oldb", "shared", "proj-b", opNow, &deletedAt)

	// 复活前：proj-a 的软删集合默认不可见，只有 includeDeleted 才列出来。
	records, err := store.ListCollections(ctx, "proj-a", false)
	require.NoError(t, err)
	require.Empty(t, records, "软删集合不算活动集合")
	records, err = store.ListCollections(ctx, "proj-a", true)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, deletedAt.UnixMilli(), records[0].DeletedAt.UnixMilli())
	require.Equal(t, 0, records[0].DocumentCount)

	// 软删集合下仍留着文档：一张活、一张软删，复活后的计数只算活的。
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_live", CollectionID: "col_old",
		SourcePath: "docs/live.md", ContentHash: "hash-live",
		CreatedAt: opNow, UpdatedAt: opNow,
	})
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_gone", CollectionID: "col_old",
		SourcePath: "docs/gone.md", ContentHash: "hash-gone",
		CreatedAt: opNow, UpdatedAt: opNow, DeletedAt: &deletedAt,
	})

	revived, err := store.CreateCollection(ctx, caller, "shared", "")
	require.NoError(t, err)
	require.Equal(t, "col_old", revived.CollectionID,
		"同名软删集合按复活处理，保留 collection_id")
	require.Equal(t, opNow.UnixMilli(), revived.CreatedAt.UnixMilli(), "复活保留 created_at")
	require.Equal(t, "proj-a", revived.Project)
	require.Equal(t, 1, revived.DocumentCount, "复活时同一条语句给出未删除文档数")
	require.True(t, revived.DeletedAt.IsZero())

	var deletedAtColumn sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT deleted_at FROM kb_collections WHERE collection_id = $1`, "col_old").
		Scan(&deletedAtColumn))
	require.False(t, deletedAtColumn.Valid, "复活即撤销软删")
	require.Equal(t, 2, catalogCollectionRows(t, db), "复活不新增行")

	// 复活后是活动集合：默认列表可见，同名再建是活行冲突。
	records, err = store.ListCollections(ctx, "proj-a", false)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "col_old", records[0].CollectionID)
	require.Equal(t, 1, records[0].DocumentCount)

	_, err = store.CreateCollection(ctx, caller, " shared ", "")
	require.ErrorIs(t, err, core.ErrConflict, "同名活动集合冲突")

	// 复活按项目隔离：proj-b 的同名软删行由 proj-b 的创建复活，而不是抢 proj-a 的行。
	revivedB, err := store.CreateCollection(ctx, core.LocalAdminPrincipal(), "shared", "proj-b")
	require.NoError(t, err)
	require.Equal(t, "col_oldb", revivedB.CollectionID)

	// 没有任何同名软删行的项目走全新插入。
	fresh, err := store.CreateCollection(ctx, core.LocalAdminPrincipal(), "shared", "proj-c")
	require.NoError(t, err)
	require.NotEqual(t, "col_old", fresh.CollectionID)
	require.NotEqual(t, "col_oldb", fresh.CollectionID)
	require.Equal(t, 3, catalogCollectionRows(t, db), "只有真正的新项目新增行")
}

// TestKnowledgeCatalog_ListDocumentsOrdersAndResolvesLatestStatus：文档列表的排序、
// 软删过滤与最新版本状态。中文验收点：固定 `source_path` 升序；尚无版本时标识与
// 状态都为空串；最新版本状态取版本行的闭集值。
// 逆改点：把 LEFT JOIN 的 `COALESCE(v.status, ”)` 删掉 → 无版本文档扫描失败；
// 把 ORDER BY 改倒序 → 路径序列断言红。
func TestKnowledgeCatalog_ListDocumentsOrdersAndResolvesLatestStatus(t *testing.T) {
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	ctx := context.Background()
	admin := core.LocalAdminPrincipal()

	catalogSeedCollection(t, db, "col_docs", "docs", "proj-a", opNow, nil)
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_a", CollectionID: "col_docs", SourcePath: "docs/a.md",
		ContentHash: "hash-a", CreatedAt: opNow, UpdatedAt: opNow,
	})
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_b", CollectionID: "col_docs", SourcePath: "docs/b.md",
		ContentHash: "hash-b", CreatedAt: opNow.Add(time.Minute),
		UpdatedAt: opNow.Add(10 * time.Minute),
	})
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_c", CollectionID: "col_docs", SourcePath: "docs/c.md",
		ContentHash: "hash-c", CreatedAt: opNow.Add(2 * time.Minute),
		UpdatedAt: opNow.Add(20 * time.Minute),
	})
	docDeletedAt := opNow.Add(40 * time.Minute)
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_z", CollectionID: "col_docs", SourcePath: "docs/z.md",
		ContentHash: "hash-z", CreatedAt: opNow.Add(3 * time.Minute),
		UpdatedAt: opNow.Add(30 * time.Minute), DeletedAt: &docDeletedAt,
	})

	catalogSeedVersion(t, db, catalogVersionRow{
		VersionID: "ver_b", DocumentID: "doc_b", ContentHash: "hash-b",
		Status: knowledge.VersionActive, ParserVersion: knowledge.ParserVersion,
		ChunkerVersion: "chunk-v1", CreatedAt: opNow, UpdatedAt: opNow.Add(10 * time.Minute),
	})
	catalogSetLatestVersion(t, db, "doc_b", "ver_b")
	catalogSeedVersion(t, db, catalogVersionRow{
		VersionID: "ver_c", DocumentID: "doc_c", ContentHash: "hash-c",
		Status: knowledge.VersionFailed, ParserVersion: knowledge.ParserVersion,
		ChunkerVersion: "chunk-v1", ErrorKind: knowledge.ErrorKindParserFailed,
		CreatedAt: opNow.Add(2 * time.Minute), UpdatedAt: opNow.Add(20 * time.Minute),
	})
	catalogSetLatestVersion(t, db, "doc_c", "ver_c")

	records, err := store.ListDocuments(ctx, admin, "col_docs", false)
	require.NoError(t, err)
	require.Len(t, records, 3, "默认不列软删文档")
	require.Equal(t, []string{"docs/a.md", "docs/b.md", "docs/c.md"},
		catalogDocumentPaths(records), "文档按 source_path 升序")

	require.Equal(t, "doc_a", records[0].DocumentID)
	require.Equal(t, "col_docs", records[0].CollectionID)
	require.Equal(t, "hash-a", records[0].ContentHash)
	require.Equal(t, "", records[0].LatestVersionID, "尚无版本时最新版本标识为空")
	require.Equal(t, "", records[0].LatestStatus, "尚无版本时状态为空串")
	require.Equal(t, opNow.UnixMilli(), records[0].CreatedAt.UnixMilli())
	require.Equal(t, opNow.UnixMilli(), records[0].UpdatedAt.UnixMilli())
	require.True(t, records[0].DeletedAt.IsZero())

	require.Equal(t, "ver_b", records[1].LatestVersionID)
	require.Equal(t, "active", records[1].LatestStatus)
	require.Equal(t, "ver_c", records[2].LatestVersionID)
	require.Equal(t, "failed", records[2].LatestStatus)

	records, err = store.ListDocuments(ctx, admin, "col_docs", true)
	require.NoError(t, err)
	require.Len(t, records, 4, "includeDeleted 为真才列出软删文档")
	require.Equal(t, "docs/z.md", records[3].SourcePath)
	require.Equal(t, docDeletedAt.UnixMilli(), records[3].DeletedAt.UnixMilli())
}

// TestKnowledgeCatalog_ListDocumentsEnforcesCollectionACL：文档列表的可见性。
// 中文验收点：项目不匹配按 `core.ErrForbidden` 拒绝而不是回空列表；集合不存在 /
// 已软删按 `knowledge.ErrNotFound`；身份与 scope 先于任何库查询；调用方项目范围
// 取自 `callers` 行，不取 `Principal.Project`。
// 逆改点：把项目不匹配改回空结果 → ErrorIs 断言红；把 callerProject 读取挪到
// 集合可见性之后 → 停用调用方用例拿到 NotFound 而不是 Unauthorized。
func TestKnowledgeCatalog_ListDocumentsEnforcesCollectionACL(t *testing.T) {
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	ctx := context.Background()

	catalogSeedCaller(t, db, "caller_a", "proj-a", "active")
	catalogSeedCaller(t, db, "caller_off", "proj-a", "disabled")
	catalogSeedCaller(t, db, "caller_empty", "", "active")
	catalogSeedCollection(t, db, "col_a", "a", "proj-a", opNow, nil)
	catalogSeedCollection(t, db, "col_b", "b", "proj-b", opNow, nil)
	deletedAt := opNow.Add(time.Hour)
	catalogSeedCollection(t, db, "col_gone", "gone", "proj-a", opNow, &deletedAt)

	reader := catalogPrincipal("caller_a", core.ScopeKnowledgeRead)

	records, err := store.ListDocuments(ctx, reader, "col_a", false)
	require.NoError(t, err)
	require.Empty(t, records, "空集合是空结果，不是错误")

	_, err = store.ListDocuments(ctx, reader, "col_b", false)
	require.ErrorIs(t, err, core.ErrForbidden, "项目不匹配按越权拒绝，不是空结果")

	_, err = store.ListDocuments(ctx, reader, "col_missing", false)
	require.ErrorIs(t, err, knowledge.ErrNotFound, "集合不存在按 404 哨兵拒绝")

	_, err = store.ListDocuments(ctx, reader, "col_gone", false)
	require.ErrorIs(t, err, knowledge.ErrNotFound, "集合已软删按 404 哨兵拒绝")

	_, err = store.ListDocuments(ctx, reader, "col_gone", true)
	require.ErrorIs(t, err, knowledge.ErrNotFound,
		"includeDeleted 不得让软删集合的文档重新可见")

	// 内置管理员无项目限制：别的项目的集合也能列。
	records, err = store.ListDocuments(ctx, core.LocalAdminPrincipal(), "col_b", false)
	require.NoError(t, err)
	require.Empty(t, records)

	// 项目为空的调用方（不限）同样能读任意项目。
	records, err = store.ListDocuments(ctx,
		catalogPrincipal("caller_empty", core.ScopeKnowledgeRead), "col_b", false)
	require.NoError(t, err)
	require.Empty(t, records)

	// 身份与 scope 先于任何库查询：零值身份、缺 scope、停用调用方都先被拒。
	_, err = store.ListDocuments(ctx, core.Principal{}, "col_a", false)
	require.ErrorIs(t, err, core.ErrUnauthorized)
	_, err = store.ListDocuments(ctx,
		catalogPrincipal("caller_a", core.ScopeSessionsRead), "col_a", false)
	require.ErrorIs(t, err, core.ErrForbidden, "缺 knowledge:read 是越权")
	_, err = store.ListDocuments(ctx,
		catalogPrincipal("caller_off", core.ScopeKnowledgeRead), "col_a", false)
	require.ErrorIs(t, err, core.ErrUnauthorized, "停用的调用方按未认证拒绝")
	_, err = store.ListDocuments(ctx,
		catalogPrincipal("caller_ghost", core.ScopeKnowledgeRead), "col_a", false)
	require.ErrorIs(t, err, core.ErrUnauthorized, "不存在的调用方按未认证拒绝")

	// 项目范围取自 callers 行，不取 Principal.Project：身份里写别的项目不改变可见范围。
	spoofed := catalogPrincipal("caller_a", core.ScopeKnowledgeRead)
	spoofed.Project = "proj-b"
	records, err = store.ListDocuments(ctx, spoofed, "col_a", false)
	require.NoError(t, err, "Principal.Project 不参与授权判定")
	require.Empty(t, records)
	_, err = store.ListDocuments(ctx, spoofed, "col_b", false)
	require.ErrorIs(t, err, core.ErrForbidden, "范围仍按 callers.project 现读")
}

// TestKnowledgeCatalog_ListVersionsOrdersAndDerivesActivation：版本列表的排序、
// 激活时刻推导与字段映射。中文验收点：`created_at` 倒序、并列 `version_id` 倒序；
// 只有 active 行带激活时刻（取 `updated_at`）；未挂代际按 0 返回；failed 行带
// `error_kind`。
// 逆改点：把 `ActivatedAt` 的推导删掉 → 激活时刻断言红；把并列排序改成单向 →
// 版本序列断言红。
func TestKnowledgeCatalog_ListVersionsOrdersAndDerivesActivation(t *testing.T) {
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	ctx := context.Background()
	admin := core.LocalAdminPrincipal()

	knSeedGeneration(t, db, 1)
	catalogSeedCollection(t, db, "col_v", "versions", "proj-a", opNow, nil)
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_live", CollectionID: "col_v", SourcePath: "docs/live.md",
		ContentHash: "hash-doc", CreatedAt: opNow, UpdatedAt: opNow,
	})
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_empty", CollectionID: "col_v", SourcePath: "docs/empty.md",
		ContentHash: "hash-empty", CreatedAt: opNow, UpdatedAt: opNow,
	})

	generation := 1
	catalogSeedVersion(t, db, catalogVersionRow{
		VersionID: "ver_old", DocumentID: "doc_live", ContentHash: "hash-v1",
		Status: knowledge.VersionActive, ParserVersion: knowledge.ParserVersion,
		ChunkerVersion: "chunk-v1", Generation: &generation,
		CreatedAt: opNow, UpdatedAt: opNow.Add(30 * time.Minute),
	})
	catalogSeedVersion(t, db, catalogVersionRow{
		VersionID: "ver_aaa", DocumentID: "doc_live", ContentHash: "hash-v2",
		Status: knowledge.VersionSuperseded, ParserVersion: knowledge.ParserVersion,
		ChunkerVersion: "chunk-v1",
		CreatedAt:      opNow.Add(20 * time.Minute), UpdatedAt: opNow.Add(20 * time.Minute),
	})
	catalogSeedVersion(t, db, catalogVersionRow{
		VersionID: "ver_zzz", DocumentID: "doc_live", ContentHash: "hash-v3",
		Status: knowledge.VersionFailed, ParserVersion: knowledge.ParserVersion,
		ChunkerVersion: "chunk-v1", ErrorKind: knowledge.ErrorKindParserFailed,
		CreatedAt: opNow.Add(20 * time.Minute), UpdatedAt: opNow.Add(25 * time.Minute),
	})
	catalogSetLatestVersion(t, db, "doc_live", "ver_old")

	records, err := store.ListVersions(ctx, admin, "doc_live")
	require.NoError(t, err)
	require.Len(t, records, 3)
	require.Equal(t, []string{"ver_zzz", "ver_aaa", "ver_old"}, catalogVersionIDs(records),
		"版本按 created_at 倒序，并列按 version_id 倒序")

	require.Equal(t, knowledge.VersionFailed, records[0].Status)
	require.Equal(t, "failed", string(records[0].Status))
	require.Equal(t, knowledge.ErrorKindParserFailed, records[0].ErrorKind)
	require.Equal(t, 0, records[0].Generation, "未挂代际的版本按 0 返回")
	require.True(t, records[0].ActivatedAt.IsZero(), "非 active 状态没有激活时刻")
	require.Equal(t, "doc_live", records[0].DocumentID)
	require.Equal(t, "hash-v3", records[0].ContentHash)

	require.Equal(t, knowledge.VersionSuperseded, records[1].Status)
	require.True(t, records[1].ActivatedAt.IsZero())

	require.Equal(t, knowledge.VersionActive, records[2].Status)
	require.Equal(t, 1, records[2].Generation)
	require.Equal(t, opNow.Add(30*time.Minute).UnixMilli(), records[2].ActivatedAt.UnixMilli(),
		"active 行的 updated_at 就是激活时刻（DDL 没有激活时间列）")
	require.Equal(t, "", records[2].ErrorKind)
	require.Equal(t, knowledge.ParserVersion, records[2].ParserVersion)
	require.Equal(t, "chunk-v1", records[2].ChunkerVersion)
	require.Equal(t, opNow.UnixMilli(), records[2].CreatedAt.UnixMilli())

	records, err = store.ListVersions(ctx, admin, "doc_empty")
	require.NoError(t, err)
	require.Empty(t, records, "尚无版本是空结果，不是错误")
}

// TestKnowledgeCatalog_ListVersionsHidesDeletedAndChecksACL：版本列表的可见性。
// 中文验收点：文档软删、所在集合软删、跨项目都不可见——前两者按
// `knowledge.ErrNotFound`，跨项目按 `core.ErrForbidden`；身份 / scope / 调用方
// 状态先于任何库查询。
// 逆改点：把 `collectDeletedAt.Valid` 从可见性判定里删掉 → 集合软删用例红。
func TestKnowledgeCatalog_ListVersionsHidesDeletedAndChecksACL(t *testing.T) {
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	ctx := context.Background()

	catalogSeedCaller(t, db, "caller_a", "proj-a", "active")
	catalogSeedCaller(t, db, "caller_b", "proj-b", "active")
	catalogSeedCaller(t, db, "caller_off", "proj-a", "disabled")
	catalogSeedCollection(t, db, "col_a", "a", "proj-a", opNow, nil)
	catalogSeedCollection(t, db, "col_b", "b", "proj-b", opNow, nil)
	collectionGoneAt := opNow.Add(time.Hour)
	catalogSeedCollection(t, db, "col_gone", "gone", "proj-a", opNow, &collectionGoneAt)

	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_a", CollectionID: "col_a", SourcePath: "docs/a.md",
		ContentHash: "hash-doc-a", CreatedAt: opNow, UpdatedAt: opNow,
	})
	documentGoneAt := opNow.Add(2 * time.Hour)
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_gone", CollectionID: "col_a", SourcePath: "docs/gone.md",
		ContentHash: "hash-doc-gone", CreatedAt: opNow, UpdatedAt: opNow,
		DeletedAt: &documentGoneAt,
	})
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_in_col_gone", CollectionID: "col_gone", SourcePath: "docs/x.md",
		ContentHash: "hash-doc-x", CreatedAt: opNow, UpdatedAt: opNow,
	})
	catalogSeedDocument(t, db, catalogDocumentRow{
		DocumentID: "doc_b", CollectionID: "col_b", SourcePath: "docs/b.md",
		ContentHash: "hash-doc-b", CreatedAt: opNow, UpdatedAt: opNow,
	})
	for _, documentID := range []string{"doc_a", "doc_gone", "doc_in_col_gone", "doc_b"} {
		catalogSeedVersion(t, db, catalogVersionRow{
			VersionID:  "ver_" + strings.TrimPrefix(documentID, "doc_"),
			DocumentID: documentID, ContentHash: "hash-version-" + documentID,
			Status: knowledge.VersionActive, ParserVersion: knowledge.ParserVersion,
			ChunkerVersion: "chunk-v1", CreatedAt: opNow, UpdatedAt: opNow,
		})
	}

	reader := catalogPrincipal("caller_a", core.ScopeKnowledgeRead)

	records, err := store.ListVersions(ctx, reader, "doc_a")
	require.NoError(t, err)
	require.Len(t, records, 1, "本项目的文档版本可见")
	require.Equal(t, knowledge.VersionActive, records[0].Status)

	_, err = store.ListVersions(ctx, reader, "doc_b")
	require.ErrorIs(t, err, core.ErrForbidden, "跨项目文档按越权拒绝")

	_, err = store.ListVersions(ctx, reader, "doc_missing")
	require.ErrorIs(t, err, knowledge.ErrNotFound, "不存在的文档按 404 哨兵拒绝")

	_, err = store.ListVersions(ctx, reader, "doc_gone")
	require.ErrorIs(t, err, knowledge.ErrNotFound, "已软删文档按 404 哨兵拒绝")

	_, err = store.ListVersions(ctx, reader, "doc_in_col_gone")
	require.ErrorIs(t, err, knowledge.ErrNotFound, "集合已软删，文档版本一并不可见")

	// 内置管理员无项目限制。
	records, err = store.ListVersions(ctx, core.LocalAdminPrincipal(), "doc_b")
	require.NoError(t, err)
	require.Len(t, records, 1)

	// 身份 / scope / 调用方状态先于任何库查询。
	_, err = store.ListVersions(ctx, core.Principal{}, "doc_a")
	require.ErrorIs(t, err, core.ErrUnauthorized)
	_, err = store.ListVersions(ctx,
		catalogPrincipal("caller_a", core.ScopeKnowledgeWrite), "doc_a")
	require.ErrorIs(t, err, core.ErrForbidden, "缺 knowledge:read 是越权")
	_, err = store.ListVersions(ctx,
		catalogPrincipal("caller_off", core.ScopeKnowledgeRead), "doc_a")
	require.ErrorIs(t, err, core.ErrUnauthorized, "停用的调用方按未认证拒绝")
	_, err = store.ListVersions(ctx,
		catalogPrincipal("caller_ghost", core.ScopeKnowledgeRead), "doc_a")
	require.ErrorIs(t, err, core.ErrUnauthorized, "不存在的调用方按未认证拒绝")
}

// TestKnowledgeCatalog_FailsClosedWithoutHandle：四个方法的统一前置——句柄缺失
// 即失败关闭，**不得**用空列表冒充"没有数据"。
// 逆改点：把 ready() 判定删掉 → 这里会空指针或返回空列表，断言红。
// TestKnowledgeCatalog_CollectionIDsByName —— D-19 的"名字 → 身份"唯一入口：
// 只认未软删行；同名活行两条（`UNIQUE (project, name)` 允许跨项目同名）解析不出
// 唯一身份，按**未解析**处理——宁可让 Agent 不注册，也不猜一个集合；未解析名单
// 保持输入顺序并去重，供注册闸门逐条点名。
// 逆改点：把 `deleted_at IS NULL` 删掉 → 软删集合被解析，断言红；同名活行取第一条
// → `dup` 出现在 resolved 里，断言红；未解析按 map 遍历输出 → 顺序断言不稳定。
func TestKnowledgeCatalog_CollectionIDsByName(t *testing.T) {
	db := portTestDB(t)
	store := NewKnowledgeCatalogStore(db)
	ctx := context.Background()
	deletedAt := opNow.Add(time.Hour)
	catalogSeedCollection(t, db, "col_run", "runbooks", "", opNow, nil)
	catalogSeedCollection(t, db, "col_faq", "faq", "proj-a", opNow, nil)
	catalogSeedCollection(t, db, "col_gone", "gone", "", opNow, &deletedAt)
	catalogSeedCollection(t, db, "col_dup1", "dup", "proj-a", opNow, nil)
	catalogSeedCollection(t, db, "col_dup2", "dup", "proj-b", opNow, nil)

	resolved, unresolved, err := store.CollectionIDsByName(ctx,
		[]string{" runbooks ", "faq", "runbooks", "", "gone", "ghost", "dup"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"runbooks": "col_run", "faq": "col_faq"}, resolved,
		"去空白 / 去重后解析；软删与同名活行都不猜")
	require.Equal(t, []string{"gone", "ghost", "dup"}, unresolved,
		"未解析保持输入顺序（去重后的首个顺序）")

	resolved, unresolved, err = store.CollectionIDsByName(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, resolved)
	require.Empty(t, unresolved, "空名单天然不产生缺失")
}

func TestKnowledgeCatalog_FailsClosedWithoutHandle(t *testing.T) {
	ctx := context.Background()
	admin := core.LocalAdminPrincipal()

	unwired := NewKnowledgeCatalogStore(&DB{})
	_, err := unwired.ListCollections(ctx, "", false)
	require.ErrorIs(t, err, errKnowledgeStoreUnconfigured, "列集合没有句柄即失败关闭")

	_, err = unwired.CreateCollection(ctx, admin, "runbooks", "")
	require.ErrorIs(t, err, errKnowledgeStoreUnconfigured, "建集合没有句柄即失败关闭")

	_, err = unwired.ListDocuments(ctx, admin, "col_it", false)
	require.ErrorIs(t, err, errKnowledgeStoreUnconfigured, "列文档没有句柄即失败关闭")

	_, err = unwired.ListVersions(ctx, admin, "doc_it")
	require.ErrorIs(t, err, errKnowledgeStoreUnconfigured, "列版本没有句柄即失败关闭")

	_, _, err = unwired.CollectionIDsByName(ctx, []string{"runbooks"})
	require.ErrorIs(t, err, errKnowledgeStoreUnconfigured, "名字解析没有句柄即失败关闭")

	var missing *KnowledgeCatalogStore
	_, err = missing.ListCollections(ctx, "", false)
	require.ErrorIs(t, err, errKnowledgeStoreUnconfigured, "nil 仓储同样失败关闭")

	_, err = NewKnowledgeCatalogStore(nil).CreateCollection(ctx, admin, "runbooks", "")
	require.ErrorIs(t, err, errKnowledgeStoreUnconfigured, "nil 句柄同样失败关闭")
}
