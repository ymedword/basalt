package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是资源版本 / 发布指针 / Run 引用 / 生命周期索引的**唯一 SQL 入口**
// （颗粒度文档 §3.1.17）。版本表是**不可变历史**：同内容哈希只留一行，撤回 = 用旧内容
// 建新版本；Run 引用在终态释放但行保留作证据；生命周期索引可由状态文件重建。
// This file is the only SQL entry point for resource versions, publish pointers, run
// references and the lifecycle index. Versions are immutable history; references are
// released but never deleted; the lifecycle index is rebuildable.

// versionColumns 是 resource_versions 的读取列序（scanVersion 按同一顺序扫描）。
const versionColumns = `id, resource_kind, resource_id, version_no, content_hash,
	definition_yaml, manifest_json, storage_path, created_at, operation_id`

// pointerColumns 是 resource_pointers 的读取列序（scanPointer 按同一顺序扫描）。
const pointerColumns = `resource_kind, resource_id, published_version_id,
	applied_version_id, deleted_at, updated_at, operation_id`

// ResourceStore 是资源版本与指针仓储。
// ResourceStore is the repository of resource versions and pointers.
type ResourceStore struct{ db *DB }

// 编译期断言：满足 core 声明的生命周期索引写端口。
// Compile-time assertion that the store satisfies the lifecycle index port.
var _ core.LifecycleIndex = (*ResourceStore)(nil)

// NewResourceStore 绑定一个已打开的写模式库句柄。
// NewResourceStore binds the store to an opened writable database handle.
func NewResourceStore(db *DB) *ResourceStore { return &ResourceStore{db: db} }

// EnsureVersion 按 `(kind, resource_id, content_hash)` 幂等建版本：同哈希命中即返回
// `inserted=false`；否则插入新版本行。`VersionNo <= 0` 时取该资源当前最大版本 + 1；
// 并发撞唯一键时**重查哈希后重试一次**（同哈希 → 幂等返回，异构版本号 → 换下一个号）。
// EnsureVersion is idempotent by content hash; a unique-key race retries exactly once
// after re-checking the hash.
func (s *ResourceStore) EnsureVersion(ctx context.Context, v core.ResourceVersion) (core.ResourceVersion, bool, error) {
	if err := validateResourceVersion(v); err != nil {
		return core.ResourceVersion{}, false, err
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		tx, err := s.db.write.BeginTx(ctx, nil)
		if err != nil {
			return core.ResourceVersion{}, false, fmt.Errorf("写入资源版本 %s/%s: 开启事务: %w", v.Kind, v.ResourceID, err)
		}
		version, inserted, err := ensureVersionTx(ctx, tx, v, attempt > 0)
		if err == nil {
			if commitErr := tx.Commit(); commitErr != nil {
				return core.ResourceVersion{}, false, fmt.Errorf("写入资源版本 %s/%s: 提交: %w", v.Kind, v.ResourceID, commitErr)
			}
			return version, inserted, nil
		}
		_ = tx.Rollback()
		lastErr = err
		// 冲突后先重查同哈希：另一个写者可能刚插入了同一内容。
		// Re-check the hash before retrying: a concurrent writer may have inserted it.
		if existing, ok, lookupErr := versionByHash(ctx, s.db.read, v.Kind, v.ResourceID, v.ContentHash); lookupErr == nil && ok {
			return existing, false, nil
		}
	}
	return core.ResourceVersion{}, false, lastErr
}

// ensureVersionTx 在同一事务内按哈希查重并插入；`recomputeVersionNo` 强制重新取号
// （唯一键冲突后的重试路径）。
// ensureVersionTx deduplicates by hash and inserts inside the transaction.
func ensureVersionTx(ctx context.Context, tx *sql.Tx, v core.ResourceVersion, recomputeVersionNo bool) (core.ResourceVersion, bool, error) {
	existing, ok, err := versionByHash(ctx, tx, v.Kind, v.ResourceID, v.ContentHash)
	if err != nil {
		return core.ResourceVersion{}, false, err
	}
	if ok {
		return existing, false, nil
	}

	versionNo := v.VersionNo
	if recomputeVersionNo || versionNo <= 0 {
		next, err := nextVersionNo(ctx, tx, v.Kind, v.ResourceID)
		if err != nil {
			return core.ResourceVersion{}, false, err
		}
		versionNo = next
	}

	createdAt := v.CreatedAt
	if createdAt == 0 {
		// created_at 是 NOT NULL 且含义为"这一时刻"；零值时间比当前时刻更误导。
		createdAt = time.Now().UnixMilli()
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO resource_versions
		   (resource_kind, resource_id, version_no, content_hash, definition_yaml,
		    manifest_json, storage_path, created_at, operation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(v.Kind), v.ResourceID, versionNo, v.ContentHash,
		nullIfEmpty(v.DefinitionYAML), nullIfEmpty(v.ManifestJSON),
		nullIfEmpty(v.StoragePath), createdAt, v.OperationID)
	if err != nil {
		return core.ResourceVersion{}, false, fmt.Errorf("写入资源版本 %s/%s: %w", v.Kind, v.ResourceID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.ResourceVersion{}, false, fmt.Errorf("写入资源版本 %s/%s: 读取新版本 id: %w", v.Kind, v.ResourceID, err)
	}

	v.ID = id
	v.VersionNo = versionNo
	v.CreatedAt = createdAt
	return v, true, nil
}

// Publish 更新发布指针：`applied=true` 同时把实际生效指针指向新版本；`applied=false`
// 保留旧 applied（MCP 首版重启前）。发布前校验版本属于同一资源——跨资源误挂直接拒绝。
// Publish upserts the publish pointer; applied=false keeps the previous applied version.
func (s *ResourceStore) Publish(ctx context.Context, kind core.ResourceKind, resourceID string, versionID int64, applied bool, operationID string, now time.Time) error {
	if err := validateResourceKind(kind); err != nil {
		return err
	}
	if resourceID == "" {
		return errors.New("发布资源: resource_id 不能为空")
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("发布资源 %s/%s: 开启事务: %w", kind, resourceID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := checkVersionBelongsTo(ctx, tx, kind, resourceID, versionID); err != nil {
		return err
	}
	if err := publishTx(ctx, tx, kind, resourceID, versionID, applied, operationID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("发布资源 %s/%s: 提交: %w", kind, resourceID, err)
	}
	return nil
}

// PublishWithVersion 在**同一事务**内插入版本行并更新发布指针（US-12 的发布路径）：
// 版本与指针不会出现"版本在、指针没动"的中间态。已存在同哈希版本时复用该版本。
// PublishWithVersion inserts the version and updates the pointer in one transaction.
func (s *ResourceStore) PublishWithVersion(ctx context.Context, v core.ResourceVersion, operationID string, now time.Time) (core.ResourceVersion, error) {
	if err := validateResourceVersion(v); err != nil {
		return core.ResourceVersion{}, err
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return core.ResourceVersion{}, fmt.Errorf("发布资源 %s/%s: 开启事务: %w", v.Kind, v.ResourceID, err)
	}
	defer func() { _ = tx.Rollback() }()

	version, _, err := ensureVersionTx(ctx, tx, v, false)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	if err := publishTx(ctx, tx, v.Kind, v.ResourceID, version.ID, true, operationID, now); err != nil {
		return core.ResourceVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.ResourceVersion{}, fmt.Errorf("发布资源 %s/%s: 提交: %w", v.Kind, v.ResourceID, err)
	}
	return version, nil
}

// publishTx 是指针写入的唯一 SQL 实现：插入时 applied 取决于本次发布，更新时
// `applied=false` 保留旧值；发布同时清掉删除标记（撤回 = 新版本）。
// publishTx is the single pointer UPSERT implementation.
func publishTx(ctx context.Context, tx *sql.Tx, kind core.ResourceKind, resourceID string, versionID int64, applied bool, operationID string, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO resource_pointers
		   (resource_kind, resource_id, published_version_id, applied_version_id,
		    deleted_at, updated_at, operation_id)
		 VALUES (?, ?, ?, CASE WHEN ? THEN ? ELSE NULL END, NULL, ?, ?)
		 ON CONFLICT(resource_kind, resource_id) DO UPDATE SET
		   published_version_id = excluded.published_version_id,
		   applied_version_id = CASE WHEN ? THEN excluded.applied_version_id
		                             ELSE resource_pointers.applied_version_id END,
		   deleted_at = NULL,
		   updated_at = excluded.updated_at,
		   operation_id = excluded.operation_id`,
		string(kind), resourceID, versionID, applied, versionID, now.UnixMilli(), operationID, applied)
	if err != nil {
		return fmt.Errorf("更新发布指针 %s/%s: %w", kind, resourceID, err)
	}
	return nil
}

// checkVersionBelongsTo 确认目标版本存在且属于 `(kind, resource_id)`——把 A 资源的
// 版本挂到 B 的指针上属于数据事故，必须在写指针前拒绝。
// checkVersionBelongsTo rejects cross-resource pointer updates.
func checkVersionBelongsTo(ctx context.Context, tx *sql.Tx, kind core.ResourceKind, resourceID string, versionID int64) error {
	var gotKind core.ResourceKind
	var gotID string
	err := tx.QueryRowContext(ctx,
		`SELECT resource_kind, resource_id FROM resource_versions WHERE id = ?`, versionID).
		Scan(&gotKind, &gotID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 版本 %d", core.ErrResourceNotFound, versionID)
	}
	if err != nil {
		return fmt.Errorf("校验资源版本 %d: %w", versionID, err)
	}
	if gotKind != kind || gotID != resourceID {
		return fmt.Errorf("版本 %d 属于 %s/%s，不能发布为 %s/%s", versionID, gotKind, gotID, kind, resourceID)
	}
	return nil
}

// Pointer 读一个资源的指针（包含已删除标记；是否展示由视图决定）。
// 未命中返回 core.ErrResourceNotFound。
// Pointer returns the pointer row including its deleted mark; a miss is ErrResourceNotFound.
func (s *ResourceStore) Pointer(ctx context.Context, kind core.ResourceKind, resourceID string) (core.ResourcePointer, error) {
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+pointerColumns+` FROM resource_pointers
		  WHERE resource_kind = ? AND resource_id = ?`, string(kind), resourceID)
	ptr, err := scanPointer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ResourcePointer{}, fmt.Errorf("%w: %s/%s", core.ErrResourceNotFound, kind, resourceID)
	}
	if err != nil {
		return core.ResourcePointer{}, fmt.Errorf("读取发布指针 %s/%s: %w", kind, resourceID, err)
	}
	return ptr, nil
}

// ListPointers 列出某类资源未删除的指针，按 resource_id 升序（只读面顺序固定）。
// ListPointers lists undeleted pointers of one kind ordered by resource id.
func (s *ResourceStore) ListPointers(ctx context.Context, kind core.ResourceKind) ([]core.ResourcePointer, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+pointerColumns+` FROM resource_pointers
		  WHERE resource_kind = ? AND deleted_at IS NULL ORDER BY resource_id ASC`, string(kind))
	if err != nil {
		return nil, fmt.Errorf("列出发布指针 %s: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	var pointers []core.ResourcePointer
	for rows.Next() {
		ptr, err := scanPointer(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描发布指针 %s: %w", kind, err)
		}
		pointers = append(pointers, ptr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历发布指针 %s: %w", kind, err)
	}
	return pointers, nil
}

// VersionByID 按全局版本 id 取一行；未命中返回 core.ErrResourceNotFound。
// VersionByID fetches one version by its global id.
func (s *ResourceStore) VersionByID(ctx context.Context, id int64) (core.ResourceVersion, error) {
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+versionColumns+` FROM resource_versions WHERE id = ?`, id)
	version, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ResourceVersion{}, fmt.Errorf("%w: 版本 %d", core.ErrResourceNotFound, id)
	}
	if err != nil {
		return core.ResourceVersion{}, fmt.Errorf("读取资源版本 %d: %w", id, err)
	}
	return version, nil
}

// Versions 取一个资源的全部不可变版本，按版本号倒序（最新在前）。
// Versions lists every immutable version of one resource, newest first.
func (s *ResourceStore) Versions(ctx context.Context, kind core.ResourceKind, resourceID string) ([]core.ResourceVersion, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+versionColumns+` FROM resource_versions
		  WHERE resource_kind = ? AND resource_id = ? ORDER BY version_no DESC, id DESC`,
		string(kind), resourceID)
	if err != nil {
		return nil, fmt.Errorf("列出资源版本 %s/%s: %w", kind, resourceID, err)
	}
	defer func() { _ = rows.Close() }()

	var versions []core.ResourceVersion
	for rows.Next() {
		version, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描资源版本 %s/%s: %w", kind, resourceID, err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资源版本 %s/%s: %w", kind, resourceID, err)
	}
	return versions, nil
}

// ReferencedBy 返回仍持有该资源某版本的 Run（`released_at IS NULL`），按 run_id 升序
// 去重——只统计未释放引用，已结束 Run 的历史行不计入。
// ReferencedBy lists runs still holding an unreleased reference, ascending and distinct.
func (s *ResourceStore) ReferencedBy(ctx context.Context, kind core.ResourceKind, resourceID string) ([]string, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT DISTINCT run_id FROM run_resource_refs
		  WHERE resource_kind = ? AND resource_id = ? AND released_at IS NULL
		  ORDER BY run_id ASC`, string(kind), resourceID)
	if err != nil {
		return nil, fmt.Errorf("列出资源引用 %s/%s: %w", kind, resourceID, err)
	}
	defer func() { _ = rows.Close() }()

	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return nil, fmt.Errorf("扫描资源引用 %s/%s: %w", kind, resourceID, err)
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资源引用 %s/%s: %w", kind, resourceID, err)
	}
	return runIDs, nil
}

// PutLifecycleIndex 写 `agent_lifecycle` 索引（upsert）。`valid=false` 时 state 写 NULL、
// generation 归零、error_message 记原因——索引可由状态文件重建，写失败不回滚权威文件。
// PutLifecycleIndex upserts the rebuildable lifecycle index; invalid rows carry NULL state.
func (s *ResourceStore) PutLifecycleIndex(ctx context.Context, rec core.LifecycleRecord, valid bool, errMsg string) error {
	if rec.Agent == "" {
		return errors.New("写生命周期索引: agent 不能为空")
	}
	updatedAt := rec.UpdatedAt
	if updatedAt == 0 {
		updatedAt = time.Now().UnixMilli()
	}

	var (
		state      any
		generation int64
		fileHash   any
		validInt   int
		errMessage any
	)
	if valid {
		state = string(rec.State)
		generation = rec.Generation
		fileHash = nullIfEmpty(rec.Checksum)
		validInt = 1
		errMessage = nil
	} else {
		state = nil
		generation = 0
		fileHash = nil
		validInt = 0
		errMessage = nullIfEmpty(errMsg)
	}

	_, err := s.db.write.ExecContext(ctx,
		`INSERT INTO agent_lifecycle
		   (agent_name, state, generation, state_file_hash, valid, error_message,
		    updated_at, operation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(agent_name) DO UPDATE SET
		   state = excluded.state,
		   generation = excluded.generation,
		   state_file_hash = excluded.state_file_hash,
		   valid = excluded.valid,
		   error_message = excluded.error_message,
		   updated_at = excluded.updated_at,
		   operation_id = excluded.operation_id`,
		rec.Agent, state, generation, fileHash, validInt, errMessage,
		updatedAt, nullIfEmpty(rec.OperationID))
	if err != nil {
		return fmt.Errorf("写生命周期索引 %s: %w", rec.Agent, err)
	}
	return nil
}

// LifecycleIndex 读 `agent_lifecycle` 索引：返回记录、`valid` 标志与无效原因；
// 索引无该 Agent 时返回零记录、valid=false、空原因、nil 错误（"没有索引行"不是故障，
// 视图据此回退到状态文件读取）。
// LifecycleIndex reads the index row; a missing row is not an error.
func (s *ResourceStore) LifecycleIndex(ctx context.Context, agent string) (core.LifecycleRecord, bool, string, error) {
	var (
		state      sql.NullString
		generation int64
		fileHash   sql.NullString
		validInt   int
		errMessage sql.NullString
		updatedAt  int64
		operation  sql.NullString
	)
	err := s.db.read.QueryRowContext(ctx,
		`SELECT state, generation, state_file_hash, valid, error_message, updated_at, operation_id
		   FROM agent_lifecycle WHERE agent_name = ?`, agent).
		Scan(&state, &generation, &fileHash, &validInt, &errMessage, &updatedAt, &operation)
	if errors.Is(err, sql.ErrNoRows) {
		// 没有索引行不是故障：带上 Agent 名返回，调用方据 valid=false 回退到状态文件。
		return core.LifecycleRecord{Agent: agent}, false, "", nil
	}
	if err != nil {
		return core.LifecycleRecord{}, false, "", fmt.Errorf("读取生命周期索引 %s: %w", agent, err)
	}

	rec := core.LifecycleRecord{
		SchemaVersion: 1,
		Agent:         agent,
		Generation:    generation,
		UpdatedAt:     updatedAt,
		OperationID:   operation.String,
		Checksum:      fileHash.String,
	}
	if state.Valid {
		rec.State = core.AgentState(state.String)
	}
	return rec, validInt == 1, errMessage.String, nil
}

// scanVersion 按 versionColumns 的列序扫描一行；三个可空文本列经 sql.NullString 往返。
// scanVersion scans one version row in versionColumns order.
func scanVersion(row interface{ Scan(...any) error }) (core.ResourceVersion, error) {
	var (
		version        core.ResourceVersion
		kind           core.ResourceKind
		definitionYAML sql.NullString
		manifestJSON   sql.NullString
		storagePath    sql.NullString
	)
	err := row.Scan(&version.ID, &kind, &version.ResourceID, &version.VersionNo,
		&version.ContentHash, &definitionYAML, &manifestJSON, &storagePath,
		&version.CreatedAt, &version.OperationID)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	version.Kind = kind
	version.DefinitionYAML = definitionYAML.String
	version.ManifestJSON = manifestJSON.String
	version.StoragePath = storagePath.String
	return version, nil
}

// scanPointer 按 pointerColumns 的列序扫描一行；`applied_version_id` / `deleted_at`
// 为 NULL 时映射为 nil 指针，不用 0 当哨兵。
// scanPointer scans one pointer row, mapping NULL applied/deleted to nil.
func scanPointer(row interface{ Scan(...any) error }) (core.ResourcePointer, error) {
	var (
		ptr       core.ResourcePointer
		kind      core.ResourceKind
		applied   sql.NullInt64
		deletedAt sql.NullInt64
	)
	err := row.Scan(&kind, &ptr.ResourceID, &ptr.PublishedVersionID, &applied,
		&deletedAt, &ptr.UpdatedAt, &ptr.OperationID)
	if err != nil {
		return core.ResourcePointer{}, err
	}
	ptr.Kind = kind
	if applied.Valid {
		value := applied.Int64
		ptr.AppliedVersionID = &value
	}
	if deletedAt.Valid {
		value := deletedAt.Int64
		ptr.DeletedAt = &value
	}
	return ptr, nil
}

// versionByHash 按 `(kind, resource_id, content_hash)` 查已有版本（唯一性由应用层保证）。
// versionByHash looks up an existing version by content hash.
func versionByHash(ctx context.Context, q queryer, kind core.ResourceKind, resourceID, contentHash string) (core.ResourceVersion, bool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+versionColumns+` FROM resource_versions
		  WHERE resource_kind = ? AND resource_id = ? AND content_hash = ?
		  ORDER BY id ASC LIMIT 1`, string(kind), resourceID, contentHash)
	version, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ResourceVersion{}, false, nil
	}
	if err != nil {
		return core.ResourceVersion{}, false, fmt.Errorf("按内容哈希查询资源版本 %s/%s: %w", kind, resourceID, err)
	}
	return version, true, nil
}

// nextVersionNo 返回该资源的下一个版本号（从 1 起）。
// nextVersionNo returns the next monotonic version number for one resource.
func nextVersionNo(ctx context.Context, q queryer, kind core.ResourceKind, resourceID string) (int, error) {
	var next int
	err := q.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version_no), 0) + 1 FROM resource_versions
		  WHERE resource_kind = ? AND resource_id = ?`, string(kind), resourceID).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("计算资源版本号 %s/%s: %w", kind, resourceID, err)
	}
	return next, nil
}

// validateResourceKind 锁定三值字面量；未知类型在写库前拒绝（CHECK 只是最后一道）。
// validateResourceKind freezes the three literal resource kinds.
func validateResourceKind(kind core.ResourceKind) error {
	switch kind {
	case core.ResourceModelConnection, core.ResourceMCPServer, core.ResourceSkill:
		return nil
	default:
		return fmt.Errorf("未知资源类型 %q", kind)
	}
}

// validateResourceVersion 校验版本行的必填字段与按类型的非空约束（与 schema CHECK 同口径）。
// validateResourceVersion enforces the per-kind required fields before hitting the CHECK.
func validateResourceVersion(v core.ResourceVersion) error {
	if err := validateResourceKind(v.Kind); err != nil {
		return err
	}
	if v.ResourceID == "" {
		return errors.New("资源版本: resource_id 不能为空")
	}
	if v.ContentHash == "" {
		return errors.New("资源版本: content_hash 不能为空")
	}
	switch v.Kind {
	case core.ResourceModelConnection, core.ResourceMCPServer:
		if v.DefinitionYAML == "" {
			return fmt.Errorf("资源版本 %s/%s: 定义原文不能为空", v.Kind, v.ResourceID)
		}
	case core.ResourceSkill:
		if v.ManifestJSON == "" || v.StoragePath == "" {
			return fmt.Errorf("资源版本 %s/%s: 技能版本必须带清单与存储路径", v.Kind, v.ResourceID)
		}
	}
	return nil
}
