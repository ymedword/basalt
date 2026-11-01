package postgres

// 本文件是知识库**目录读面**（集合 / 文档 / 版本三个只读列表）与集合创建的 PG
// 实现（US-16 §3.1.7；冻结契约 `GET /collections`、`POST /collections`、
// `GET /collections/{id}/documents`、`GET /documents/{id}/versions`）。
//
// 五条纪律：
//
//  1. **排序在 SQL 里**：契约把顺序写死（集合 `name` 升序、文档 `source_path` 升序、
//     版本 `created_at` 倒序），端口**不提供**排序参数；并列时按主键再排一次，
//     让同一份数据每次列出的顺序可复现。
//  2. **ACL 顺序固定**：身份 → scope → 调用方项目 → 目标集合。集合不存在或已软删
//     按 `knowledge.ErrNotFound`（契约 404），项目不匹配按 `core.ErrForbidden`
//     （契约 403）；两种失败**不得**合并——合并会让"存在但不属于你"与"从未存在"
//     混成同一句话，审计与排障都失去判据。
//  3. **`activated_at` 由 `updated_at` 推导**：DDL 没有激活时间列，`status='active'`
//     的最后一次写入就是 `ActivateVersion`；其余状态返回零值（视图渲染空串）。
//     该推导只在"激活是 active 行最后写者"这一不变量成立时有效，已登记为局限。
//  4. **同名软删集合按"复活"处理**：契约说"软删行不算活动"，但 DDL 的
//     `UNIQUE (project, name)` 把软删行也算在内——插入撞 23505 时先尝试复活软删行
//     （保留 `collection_id` 与 `created_at`，既有文档引用不失效），只有活行冲突
//     才回 `core.ErrConflict`（409）。
//  5. **计数不可省**：`DocumentCount` 按未删除文档数统计；本实现永远给出真实计数，
//     -1（未统计）是留给未来带预算的适配器的语义，不是本层会返回的值。
//
// This file is the PostgreSQL catalog reader and collection creator. Ordering lives in
// the SQL, ACL checks run identity → scope → caller project → target collection, and a
// 23505 on collection insert first tries to revive a soft-deleted namesake before
// reporting a real conflict.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// knowledgeCollectionIDPrefix 是集合主键前缀；载荷沿用 `newKnowledgeID` 的 26 位
// Crockford Base32，与契约 `CollectionSummary.collection_id` 的形态声明一致。
// knowledgeCollectionIDPrefix mirrors the contract's `col_` + 26 Crockford chars form.
const knowledgeCollectionIDPrefix = "col_"

// KnowledgeCollectionRecord 是集合行在存储层的投影（契约 `CollectionSummary` 的
// 数据来源）。`DocumentCount` 是未删除文档数；`DeletedAt` 零值表示未删除。
// KnowledgeCollectionRecord mirrors one kb_collections row plus its live document count.
type KnowledgeCollectionRecord struct {
	CollectionID        string
	Name                string
	Project             string
	Description         string
	DescriptionRevision int64
	DeclaredAgents      []string
	DocumentCount       int
	CreatedAt           time.Time
	DeletedAt           time.Time
}

// KnowledgeDocumentRecord 是文档行在存储层的投影（契约 `DocumentSummary` 的来源）。
// `LatestStatus` 空串表示尚无版本；`DeletedAt` 零值表示未删除。
// KnowledgeDocumentRecord mirrors one kb_documents row with its latest version status.
type KnowledgeDocumentRecord struct {
	DocumentID      string
	CollectionID    string
	SourcePath      string
	ContentHash     string
	LatestVersionID string
	LatestStatus    string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       time.Time
}

// KnowledgeVersionRecord 是版本行在存储层的投影（契约 `DocumentVersionSummary` 的
// 来源）。`ActivatedAt` 零值表示未激活（非 `active` 状态）。
// KnowledgeVersionRecord mirrors one kb_document_versions row; a zero ActivatedAt means
// the version is not active.
type KnowledgeVersionRecord struct {
	VersionID      string
	DocumentID     string
	Revision       int64
	ContentHash    string
	Status         knowledge.VersionStatus
	ParserVersion  string
	ChunkerVersion string
	Generation     int
	ErrorKind      string
	CreatedAt      time.Time
	ActivatedAt    time.Time
}

type KnowledgeIngestAttemptRecord struct {
	JobID, VersionID, Stage, Status, ErrorKind string
	Attempt                                    int
	StartedAt, FinishedAt                      time.Time
}

// KnowledgeCatalogStore 实现目录读面与集合创建；SQL 与 ACL 判定都在这里，上层适配
// 只做字段搬运。
// KnowledgeCatalogStore owns the catalog SQL and the ACL checks; callers only map shapes.
type KnowledgeCatalogStore struct {
	db *DB
}

// NewKnowledgeCatalogStore 构造目录存储；句柄缺失时各方法按失败关闭返回错误。
// NewKnowledgeCatalogStore builds the catalog store; a missing handle fails closed.
func NewKnowledgeCatalogStore(db *DB) *KnowledgeCatalogStore {
	return &KnowledgeCatalogStore{db: db}
}

// ready 是所有方法的统一前置：句柄缺失即失败关闭，**不得**用空列表冒充"没有数据"。
// ready is the shared fail-closed precondition; an empty list never stands in for it.
func (s *KnowledgeCatalogStore) ready() error {
	if s == nil || s.db == nil || s.db.pool == nil {
		return errKnowledgeStoreUnconfigured
	}
	return nil
}

// ListCollections 列出集合（管理面，契约无 Principal 参数——admin token 已由路由层
// 判定）。`project` 为空串表示不限项目；软删集合只在 `includeDeleted` 时为真才列出。
// ListCollections lists collections; an empty project means "all projects".
func (s *KnowledgeCatalogStore) ListCollections(ctx context.Context, project string,
	includeDeleted bool,
) ([]KnowledgeCollectionRecord, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	project = strings.TrimSpace(project)

	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT c.collection_id, c.name, c.project, c.description, c.description_revision,
		       (SELECT COUNT(*) FROM kb_documents d
		         WHERE d.collection_id = c.collection_id AND d.deleted_at IS NULL),
		       c.created_at, c.deleted_at
		  FROM kb_collections c
		 WHERE ($1 = '' OR c.project = $1)
		   AND ($2 OR c.deleted_at IS NULL)
		 ORDER BY c.name ASC, c.collection_id ASC`, project, includeDeleted)
	if err != nil {
		return nil, fmt.Errorf("列集合: %w", err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]KnowledgeCollectionRecord, 0, 8)
	for rows.Next() {
		var (
			record    KnowledgeCollectionRecord
			createdAt int64
			deletedAt sql.NullInt64
		)
		if err := rows.Scan(&record.CollectionID, &record.Name, &record.Project, &record.Description, &record.DescriptionRevision,
			&record.DocumentCount, &createdAt, &deletedAt); err != nil {
			return nil, fmt.Errorf("读取集合行: %w", err)
		}
		record.CreatedAt = time.UnixMilli(createdAt).UTC()
		record.DeletedAt = unixMilliOrZero(deletedAt)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取集合行: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("关闭集合读取: %w", err)
	}
	refs, err := NewBindingStore(s.db).KnowledgeReferences(ctx)
	if err != nil {
		return nil, err
	}
	for i := range records {
		records[i].DeclaredAgents = append([]string{}, refs[records[i].CollectionID]...)
	}
	return records, nil
}

// CollectionIDsByName 把声明里的**集合名**解析成稳定集合 ID（D-19 投影与检索授权的
// 唯一名字 → 身份入口）：只认未软删的行；同名活行出现两条（占位符里不含项目，跨项目
// 同名时 `UNIQUE (project, name)` 拦不住）按**未解析**处理——名字解析不出唯一身份时
// 宁可让 Agent 不注册，也不猜一个集合。
//
// 返回 (解析成功表, 未解析名单, error)：未解析名单保持输入顺序、去重，供注册闸门生成
// `knowledge collection missing: ...` 与 WARN 日志。空输入不查库（`= ANY('{}')` 也
// 合法，但省一次往返，且空名单天然不该产生"缺失"）。
// CollectionIDsByName resolves declared names to stable ids; a name with two live rows is
// treated as unresolved rather than guessed.
func (s *KnowledgeCatalogStore) CollectionIDsByName(ctx context.Context, names []string) (map[string]string, []string, error) {
	if err := s.ready(); err != nil {
		return nil, nil, err
	}
	unique := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		unique = append(unique, name)
	}
	resolved := make(map[string]string, len(unique))
	if len(unique) == 0 {
		return resolved, nil, nil
	}

	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT name, collection_id
		  FROM kb_collections
		 WHERE name = ANY($1) AND deleted_at IS NULL
		 ORDER BY name ASC, collection_id ASC`, unique)
	if err != nil {
		return nil, nil, fmt.Errorf("按名解析集合: %w", err)
	}
	defer func() { _ = rows.Close() }()

	ambiguous := make(map[string]bool, 0)
	for rows.Next() {
		var name, collectionID string
		if err := rows.Scan(&name, &collectionID); err != nil {
			return nil, nil, fmt.Errorf("读取集合名解析行: %w", err)
		}
		if _, ok := resolved[name]; ok {
			ambiguous[name] = true
			continue
		}
		resolved[name] = collectionID
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("读取集合名解析行: %w", err)
	}
	for name := range ambiguous {
		delete(resolved, name)
	}

	var unresolved []string
	for _, name := range unique {
		if _, ok := resolved[name]; !ok {
			unresolved = append(unresolved, name)
		}
	}
	return resolved, unresolved, nil
}

// CreateCollection 建一个集合（`knowledge:write`）。
//
// 项目归属规则：请求未给 `project` 时落到调用方自己的项目；调用方项目受限而请求给出
// 别的项目 → `core.ErrForbidden`——否则项目受限的调用方能建出自己看不见、别人看得见
// 的集合，等于绕过 ACL 写全局数据。调用方项目为空（不限）时按请求原样落库。
// 同名活动集合 → `core.ErrConflict`；同名软删行 → 复活该行（见文件头第 4 条）。
// CreateCollection creates a collection; an omitted project falls back to the caller's,
// and a project-scoped caller may not create one elsewhere.
func (s *KnowledgeCatalogStore) CreateCollection(ctx context.Context, p core.Principal,
	name, project string,
) (KnowledgeCollectionRecord, error) {
	if err := s.ready(); err != nil {
		return KnowledgeCollectionRecord{}, err
	}
	if !p.Valid() {
		return KnowledgeCollectionRecord{}, fmt.Errorf("%w: 建集合身份不可用", core.ErrUnauthorized)
	}
	if !p.HasScope(core.ScopeKnowledgeWrite) {
		return KnowledgeCollectionRecord{}, fmt.Errorf("%w: 缺少 %s scope",
			core.ErrForbidden, core.ScopeKnowledgeWrite)
	}
	callerProject, err := lookupCallerProject(ctx, s.db, p.CallerID)
	if err != nil {
		return KnowledgeCollectionRecord{}, err
	}

	name = strings.TrimSpace(name)
	project = strings.TrimSpace(project)
	if name == "" {
		return KnowledgeCollectionRecord{}, fmt.Errorf("%w: 集合名为空", knowledge.ErrInvalidRequest)
	}
	if project == "" {
		project = callerProject
	}
	if callerProject != "" && project != callerProject {
		return KnowledgeCollectionRecord{}, fmt.Errorf(
			"%w: 调用方只能在项目 %q 下建集合", core.ErrForbidden, callerProject)
	}

	collectionID, err := newKnowledgeID(knowledgeCollectionIDPrefix)
	if err != nil {
		return KnowledgeCollectionRecord{}, err
	}
	now := s.db.clock().UTC()
	_, err = s.db.pool.ExecContext(ctx,
		`INSERT INTO kb_collections (collection_id, name, project, created_at)
		 VALUES ($1, $2, $3, $4)`,
		collectionID, name, project, now.UnixMilli())
	if err == nil {
		return KnowledgeCollectionRecord{
			CollectionID:  collectionID,
			Name:          name,
			Project:       project,
			DocumentCount: 0,
			CreatedAt:     now,
		}, nil
	}
	// 只有唯一约束冲突才有"复活"这条退路；其余错误按基础设施错误上抛。
	// Only a unique violation may fall back to revival; anything else propagates.
	if !isPGUniqueViolation(err) {
		return KnowledgeCollectionRecord{}, fmt.Errorf("建集合（%s/%s）: %w", project, name, err)
	}
	return s.reviveCollection(ctx, project, name)
}

// reviveCollection 把同名的软删集合复活：DDL 的 `UNIQUE (project, name)` 包含软删行，
// 而契约说软删行不算活动集合——两句话只能在插入撞 23505 时用"撤销软删"同时满足。
// 用一条数据修改 CTE 完成"复活 + 计数"，避免两步之间留下"已复活但计数失败"的半截状态。
// reviveCollection revives the soft-deleted namesake and counts its live documents in a
// single statement; no revival row means a real live conflict.
func (s *KnowledgeCatalogStore) reviveCollection(ctx context.Context, project,
	name string,
) (KnowledgeCollectionRecord, error) {
	var (
		record    KnowledgeCollectionRecord
		createdAt int64
	)
	err := s.db.pool.QueryRowContext(ctx, `
		WITH revived AS (
			UPDATE kb_collections
			   SET deleted_at = NULL
			 WHERE project = $1 AND name = $2 AND deleted_at IS NOT NULL
			RETURNING collection_id, created_at, description, description_revision
		)
		SELECT r.collection_id, r.created_at, r.description, r.description_revision,
		       (SELECT COUNT(*) FROM kb_documents d
		         WHERE d.collection_id = r.collection_id AND d.deleted_at IS NULL)
		  FROM revived r`, project, name).
		Scan(&record.CollectionID, &createdAt, &record.Description, &record.DescriptionRevision, &record.DocumentCount)
	if errors.Is(err, sql.ErrNoRows) {
		return KnowledgeCollectionRecord{}, fmt.Errorf(
			"%w: 项目 %q 下已存在同名活动集合 %q", core.ErrConflict, project, name)
	}
	if err != nil {
		return KnowledgeCollectionRecord{}, fmt.Errorf("复活同名软删集合（%s/%s）: %w", project, name, err)
	}
	record.Name = name
	record.Project = project
	record.CreatedAt = time.UnixMilli(createdAt).UTC()
	return record, nil
}

// UpdateDescription changes only the metadata of a live collection. The revision
// is compared in SQL so a stale page cannot overwrite another administrator's edit.
func (s *KnowledgeCatalogStore) UpdateDescription(ctx context.Context, id, description string, expected int64) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	var revision int64
	err := s.db.pool.QueryRowContext(ctx, `UPDATE kb_collections
		SET description = $2, description_revision = description_revision + 1
		WHERE collection_id = $1 AND deleted_at IS NULL AND description_revision = $3
		RETURNING description_revision`, id, description, expected).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if e := s.db.pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kb_collections WHERE collection_id = $1 AND deleted_at IS NULL)`, id).Scan(&exists); e != nil {
			return 0, fmt.Errorf("检查集合描述: %w", e)
		}
		if !exists {
			return 0, knowledge.ErrNotFound
		}
		return 0, core.ErrPreconditionFailed
	}
	if err != nil {
		return 0, fmt.Errorf("更新集合描述: %w", err)
	}
	return revision, nil
}

// ListDocuments 列集合内文档（`knowledge:read`）：固定 `source_path` 升序
// （集合内唯一，不需要并列再排）。集合不存在 / 已软删 → `knowledge.ErrNotFound`；
// 调用方项目与集合归属不一致 → `core.ErrForbidden`。
// ListDocuments lists a collection's documents; missing or soft-deleted collections are
// NotFound, a foreign project is Forbidden.
func (s *KnowledgeCatalogStore) ListDocuments(ctx context.Context, p core.Principal,
	collectionID string, includeDeleted bool,
) ([]KnowledgeDocumentRecord, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	callerProject, err := s.catalogScope(ctx, p)
	if err != nil {
		return nil, err
	}
	collectionID = strings.TrimSpace(collectionID)
	project, err := s.visibleCollectionProject(ctx, collectionID)
	if err != nil {
		return nil, err
	}
	if callerProject != "" && project != callerProject {
		return nil, fmt.Errorf("%w: 集合 %s 不属于调用方项目", core.ErrForbidden, collectionID)
	}

	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT d.document_id, d.collection_id, d.source_path, d.content_hash,
		       COALESCE(d.latest_version_id, ''), COALESCE(v.status, ''),
		       d.created_at, d.updated_at, d.deleted_at
		  FROM kb_documents d
		  LEFT JOIN kb_document_versions v ON v.version_id = d.latest_version_id
		 WHERE d.collection_id = $1
		   AND ($2 OR d.deleted_at IS NULL)
		 ORDER BY d.source_path ASC`, collectionID, includeDeleted)
	if err != nil {
		return nil, fmt.Errorf("列集合 %s 的文档: %w", collectionID, err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]KnowledgeDocumentRecord, 0, 16)
	for rows.Next() {
		var (
			record    KnowledgeDocumentRecord
			createdAt int64
			updatedAt int64
			deletedAt sql.NullInt64
		)
		if err := rows.Scan(&record.DocumentID, &record.CollectionID, &record.SourcePath,
			&record.ContentHash, &record.LatestVersionID, &record.LatestStatus,
			&createdAt, &updatedAt, &deletedAt); err != nil {
			return nil, fmt.Errorf("读取文档行: %w", err)
		}
		record.CreatedAt = time.UnixMilli(createdAt).UTC()
		record.UpdatedAt = time.UnixMilli(updatedAt).UTC()
		record.DeletedAt = unixMilliOrZero(deletedAt)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取文档行: %w", err)
	}
	return records, nil
}

// ListVersions 列文档的版本（`knowledge:read`）：固定 `created_at` 倒序，并列按
// `version_id` 再排一次。文档或它所在的集合已软删 → `knowledge.ErrNotFound`；
// 项目不匹配 → `core.ErrForbidden`。
// ListVersions lists newest-first; a soft-deleted document or collection is NotFound.
func (s *KnowledgeCatalogStore) ListVersions(ctx context.Context, p core.Principal,
	documentID string,
) ([]KnowledgeVersionRecord, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	callerProject, err := s.catalogScope(ctx, p)
	if err != nil {
		return nil, err
	}
	documentID = strings.TrimSpace(documentID)

	// 文档可见性一次判完：文档与集合的软删状态、集合归属都在这一行里。
	// One query decides document visibility: both soft-delete flags plus the project.
	var (
		project           string
		documentDeletedAt sql.NullInt64
		collectDeletedAt  sql.NullInt64
	)
	err = s.db.pool.QueryRowContext(ctx, `
		SELECT c.project, d.deleted_at, c.deleted_at
		  FROM kb_documents d
		  JOIN kb_collections c ON c.collection_id = d.collection_id
		 WHERE d.document_id = $1`, documentID).
		Scan(&project, &documentDeletedAt, &collectDeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 文档 %s", knowledge.ErrNotFound, documentID)
	}
	if err != nil {
		return nil, fmt.Errorf("读取文档归属: %w", err)
	}
	if documentDeletedAt.Valid || collectDeletedAt.Valid {
		return nil, fmt.Errorf("%w: 文档 %s 已删除", knowledge.ErrNotFound, documentID)
	}
	if callerProject != "" && project != callerProject {
		return nil, fmt.Errorf("%w: 文档 %s 不属于调用方项目", core.ErrForbidden, documentID)
	}

	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT version_id, document_id, revision, content_hash, status,
		       parser_version, chunker_version, COALESCE(generation, 0),
		       COALESCE(error_kind, ''), created_at, updated_at
		  FROM kb_document_versions
		 WHERE document_id = $1
		 ORDER BY revision DESC`, documentID)
	if err != nil {
		return nil, fmt.Errorf("列文档 %s 的版本: %w", documentID, err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]KnowledgeVersionRecord, 0, 8)
	for rows.Next() {
		var (
			record    KnowledgeVersionRecord
			status    string
			createdAt int64
			updatedAt int64
		)
		if err := rows.Scan(&record.VersionID, &record.DocumentID, &record.Revision, &record.ContentHash,
			&status, &record.ParserVersion, &record.ChunkerVersion, &record.Generation,
			&record.ErrorKind, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("读取版本行: %w", err)
		}
		record.Status = knowledge.VersionStatus(status)
		record.CreatedAt = time.UnixMilli(createdAt).UTC()
		// `activated_at` 的推导：DDL 没有激活时间列，`active` 行的最后一次写入就是
		// 激活（ActivateVersion 是唯一置 active 的写者）。
		// ActivatedAt is derived: for an active row, updated_at is the activation instant.
		if record.Status == knowledge.VersionActive {
			record.ActivatedAt = time.UnixMilli(updatedAt).UTC()
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取版本行: %w", err)
	}
	return records, nil
}

// ListIngestAttempts returns at most the newest 100 jobs and explicitly reports
// truncation. The joins recheck deletion in the same query that reads the jobs.
func (s *KnowledgeCatalogStore) ListIngestAttempts(ctx context.Context, documentID string) ([]KnowledgeIngestAttemptRecord, bool, error) {
	if err := s.ready(); err != nil {
		return nil, false, err
	}
	var visible bool
	if err := s.db.pool.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM kb_documents d JOIN kb_collections c ON c.collection_id = d.collection_id
		WHERE d.document_id = $1 AND d.deleted_at IS NULL AND c.deleted_at IS NULL)`, documentID).Scan(&visible); err != nil {
		return nil, false, fmt.Errorf("检查文档状态: %w", err)
	}
	if !visible {
		return nil, false, knowledge.ErrNotFound
	}
	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT j.ingest_job_id, j.version_id, j.attempt, j.stage, j.status,
		       COALESCE(j.error_kind, ''), j.started_at, j.finished_at
		FROM kb_ingest_jobs j JOIN kb_documents d ON d.document_id = j.document_id
		JOIN kb_collections c ON c.collection_id = d.collection_id
		WHERE j.document_id = $1 AND d.deleted_at IS NULL AND c.deleted_at IS NULL
		ORDER BY j.started_at DESC, j.ingest_job_id DESC LIMIT 101`, documentID)
	if err != nil {
		return nil, false, fmt.Errorf("列处理尝试: %w", err)
	}
	defer rows.Close()
	results := make([]KnowledgeIngestAttemptRecord, 0, 100)
	for rows.Next() {
		var row KnowledgeIngestAttemptRecord
		var started int64
		var finished sql.NullInt64
		if err := rows.Scan(&row.JobID, &row.VersionID, &row.Attempt, &row.Stage, &row.Status, &row.ErrorKind, &started, &finished); err != nil {
			return nil, false, fmt.Errorf("读取处理尝试: %w", err)
		}
		row.StartedAt = time.UnixMilli(started).UTC()
		row.FinishedAt = unixMilliOrZero(finished)
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(results) > 100
	if truncated {
		results = results[:100]
	}
	return results, truncated, nil
}

// catalogScope 是业务面三个方法的统一前置：身份 → scope → 调用方项目。顺序与检索
// 授权（`authorize.go`）一致：身份与 scope 的判定发生在任何库查询之前。
// catalogScope checks identity, scope and caller project before any query runs.
func (s *KnowledgeCatalogStore) catalogScope(ctx context.Context, p core.Principal) (string, error) {
	if !p.Valid() {
		return "", fmt.Errorf("%w: 目录读取身份不可用", core.ErrUnauthorized)
	}
	if !p.HasScope(core.ScopeKnowledgeRead) {
		return "", fmt.Errorf("%w: 缺少 %s scope", core.ErrForbidden, core.ScopeKnowledgeRead)
	}
	return lookupCallerProject(ctx, s.db, p.CallerID)
}

// visibleCollectionProject 返回未软删集合的归属项目；不存在与已软删同归
// `knowledge.ErrNotFound`——两者在业务面上同样是"这个集合现在不可用"。
// visibleCollectionProject folds "missing" and "soft-deleted" into ErrNotFound.
func (s *KnowledgeCatalogStore) visibleCollectionProject(ctx context.Context,
	collectionID string,
) (string, error) {
	var (
		project   string
		deletedAt sql.NullInt64
	)
	err := s.db.pool.QueryRowContext(ctx,
		`SELECT project, deleted_at FROM kb_collections WHERE collection_id = $1`,
		collectionID).Scan(&project, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: 集合 %s", knowledge.ErrNotFound, collectionID)
	}
	if err != nil {
		return "", fmt.Errorf("读取集合归属: %w", err)
	}
	if deletedAt.Valid {
		return "", fmt.Errorf("%w: 集合 %s 已删除", knowledge.ErrNotFound, collectionID)
	}
	return project, nil
}
