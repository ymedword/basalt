package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是资源版本 / 发布指针 / Run 引用 / 生命周期索引的**唯一 SQL 入口**
// （颗粒度文档 §3.1.17）。版本表是**不可变历史**：同内容哈希只留一行，撤回 = 用旧内容
// 建新版本；Run 引用在终态释放但行保留作证据；生命周期索引可由状态文件重建。
//
// 身份纪律（US-12 R4）：版本号逐 **resource_uid** 从 1 起，`resource_id` 只是名称快照；
// 指针行的 `resource_uid` 回答"这个名称当前指向哪个实例"，同名重建换新 UID，旧 UID 的
// 版本与删除事实原样保留。存储层**不得**按同名把旧 UID 的历史接到新 UID 上。
// This file is the only SQL entry point for resource versions, publish pointers, run
// references and the lifecycle index. Version numbers count per resource uid; the name is
// only a snapshot, and a same-name rebuild never adopts the previous uid's history.

// versionColumns 是 resource_versions 的读取列序（scanVersion 按同一顺序扫描）。
const versionColumns = `id, resource_kind, resource_uid, resource_id, version_no, content_hash,
	definition_yaml, manifest_json, storage_path, created_at, operation_id`

// pointerColumns 是 resource_pointers 的读取列序（scanPointer 按同一顺序扫描）。
const pointerColumns = `resource_kind, resource_id, resource_uid, published_version_id,
	applied_version_id, scope, owner_agent_uid, revision, deleted_at, updated_at, operation_id`

// ResourceStore 是资源版本与指针仓储。
// ResourceStore is the repository of resource versions and pointers.
type ResourceStore struct{ db *DB }

// 编译期断言：满足 core 声明的生命周期索引写端口。
// Compile-time assertion that the store satisfies the lifecycle index port.
var _ core.LifecycleIndex = (*ResourceStore)(nil)

// NewResourceStore 绑定一个已打开的写模式库句柄。
// NewResourceStore binds the store to an opened writable database handle.
func NewResourceStore(db *DB) *ResourceStore { return &ResourceStore{db: db} }

// EnsureVersion 按 `(kind, resource_uid, content_hash)` 幂等建版本：同哈希命中即返回
// `inserted=false`；否则插入新版本行。`VersionNo <= 0` 时取该资源实例当前最大版本 + 1；
// 并发撞唯一键时**重查哈希后重试一次**（同哈希 → 幂等返回，异构版本号 → 换下一个号）。
// EnsureVersion is idempotent by content hash; a unique-key race retries exactly once
// after re-checking the hash.
func (s *ResourceStore) EnsureVersion(ctx context.Context, v core.ResourceVersion) (core.ResourceVersion, bool, error) {
	v, err := normalizeResourceVersion(v)
	if err != nil {
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
		if existing, ok, lookupErr := versionByHash(ctx, s.db.read, v.Kind, v.ResourceUID, v.ContentHash); lookupErr == nil && ok {
			return existing, false, nil
		}
	}
	return core.ResourceVersion{}, false, lastErr
}

// ensureVersionTx 在同一事务内按哈希查重并插入；`recomputeVersionNo` 强制重新取号
// （唯一键冲突后的重试路径）。
// ensureVersionTx deduplicates by hash and inserts inside the transaction.
func ensureVersionTx(ctx context.Context, tx *sql.Tx, v core.ResourceVersion, recomputeVersionNo bool) (core.ResourceVersion, bool, error) {
	existing, ok, err := versionByHash(ctx, tx, v.Kind, v.ResourceUID, v.ContentHash)
	if err != nil {
		return core.ResourceVersion{}, false, err
	}
	if ok {
		return existing, false, nil
	}

	versionNo := v.VersionNo
	if recomputeVersionNo || versionNo <= 0 {
		next, err := nextVersionNo(ctx, tx, v.Kind, v.ResourceUID)
		if err != nil {
			return core.ResourceVersion{}, false, err
		}
		versionNo = next
	}

	inserted, err := insertVersionRowTx(ctx, tx, v, versionNo)
	if err != nil {
		return core.ResourceVersion{}, false, err
	}
	return inserted, true, nil
}

// insertVersionTx 无条件追加一个新版本行（发布 / 撤回路径）：版本号取该资源实例当前最大
// 版本 + 1，**不**按内容哈希去重——撤回必须留下"用旧内容新建的版本"这一条历史。
// insertVersionTx always appends a new version row (publish / revert): the version
// number is per resource uid, and identical content still becomes a new history row.
func insertVersionTx(ctx context.Context, tx *sql.Tx, v core.ResourceVersion) (core.ResourceVersion, error) {
	versionNo, err := nextVersionNo(ctx, tx, v.Kind, v.ResourceUID)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	return insertVersionRowTx(ctx, tx, v, versionNo)
}

// insertVersionRowTx 是版本行插入的唯一 SQL 实现；调用方负责决定版本号。
// insertVersionRowTx is the single INSERT for version rows; callers pick the number.
func insertVersionRowTx(ctx context.Context, tx *sql.Tx, v core.ResourceVersion, versionNo int) (core.ResourceVersion, error) {
	createdAt := v.CreatedAt
	if createdAt == 0 {
		// created_at 是 NOT NULL 且含义为"这一时刻"；零值时间比当前时刻更误导。
		createdAt = time.Now().UnixMilli()
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO resource_versions
		   (resource_kind, resource_uid, resource_id, version_no, content_hash, definition_yaml,
		    manifest_json, storage_path, created_at, operation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(v.Kind), string(v.ResourceUID), v.ResourceID, versionNo, v.ContentHash,
		nullIfEmpty(v.DefinitionYAML), nullIfEmpty(v.ManifestJSON),
		nullIfEmpty(v.StoragePath), createdAt, v.OperationID)
	if err != nil {
		return core.ResourceVersion{}, fmt.Errorf("写入资源版本 %s/%s: %w", v.Kind, v.ResourceID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.ResourceVersion{}, fmt.Errorf("写入资源版本 %s/%s: 读取新版本 id: %w", v.Kind, v.ResourceID, err)
	}

	v.ID = id
	v.VersionNo = versionNo
	v.CreatedAt = createdAt
	return v, nil
}

// Publish 更新发布指针：`applied=true` 同时把实际生效指针指向新版本；`applied=false`
// 保留旧 applied（MCP 首版重启前）。发布前校验版本属于同一资源——跨资源误挂直接拒绝；
// 指针行的资源 UID **取自版本行自身**，不按名称推断身份。
// Publish upserts the publish pointer; applied=false keeps the previous applied version.
// The pointer's uid comes from the version row itself, never inferred from the name.
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

	uid, err := checkVersionBelongsTo(ctx, tx, kind, resourceID, versionID)
	if err != nil {
		return err
	}
	if err := publishTx(ctx, tx, kind, resourceID, uid, versionID, applied, operationID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("发布资源 %s/%s: 提交: %w", kind, resourceID, err)
	}
	return nil
}

// CASApplied 是受控重建（`mcp_server.apply`）的指针提交：只有在发布指针仍指向
// `expectedPublished`、实际生效指针仍等于 `expectedApplied` 且资源未被删除时，才把
// `applied_version_id` 推进到 `target`。三项期望值来自第 1 步冻结的读数——任何一个
// 在此期间变了（publish / 另一代配置 / revoke / 删除），本次切换都必须丢弃（契约第 6 步）。
//
// 返回 `(false, nil)` 表示"期望不成立"而**不是**错误：调用方据此走 superseded 分支，
// 与写失败（数据库层错误）区分开。
// CASApplied commits the applied pointer only while all three expectations still hold.
// A false result with a nil error means "expectation missed", not a storage failure.
func (s *ResourceStore) CASApplied(ctx context.Context, kind core.ResourceKind, resourceID string,
	expectedPublished, expectedApplied, target int64, operationID string, now time.Time,
) (bool, error) {
	if err := validateResourceKind(kind); err != nil {
		return false, err
	}
	if resourceID == "" {
		return false, errors.New("应用资源版本: resource_id 不能为空")
	}
	if now.IsZero() {
		now = time.Now()
	}
	result, err := s.db.write.ExecContext(ctx,
		`UPDATE resource_pointers
		    SET applied_version_id = ?, revision = revision + 1, updated_at = ?, operation_id = ?
		  WHERE resource_kind = ? AND resource_id = ?
		    AND published_version_id = ?
		    AND COALESCE(applied_version_id, 0) = ?
		    AND deleted_at IS NULL`,
		target, now.UnixMilli(), operationID, string(kind), resourceID,
		expectedPublished, expectedApplied)
	if err != nil {
		return false, fmt.Errorf("应用资源版本 %s/%s: %w", kind, resourceID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("应用资源版本 %s/%s: 读取影响行数: %w", kind, resourceID, err)
	}
	return affected == 1, nil
}

// PublishWithVersion 在**同一事务**内追加版本行、更新发布指针并回填操作终态
// （US-12 的发布 / 撤回路径）：版本、指针与操作状态不会出现中间态。版本**总是**
// 新插入一行（撤回 = 用旧内容建新版本）；指针写失败时版本行与终态一起回滚。
// PublishWithVersion appends a new version, moves the pointer and closes the
// operation in one transaction; any failure rolls all three back together.
func (s *ResourceStore) PublishWithVersion(ctx context.Context, v core.ResourceVersion, operationID string, now time.Time) (core.ResourceVersion, error) {
	return s.PublishWithVersionState(ctx, v, true, operationID, now)
}

// PublishWithVersionState 是 PublishWithVersion 的可控 applied 版本：MCP 首版发布后
// 要等重启才应用（约束 9），`applied=false` 保留旧的实际生效指针。
// PublishWithVersionState is PublishWithVersion with an explicit applied flag;
// MCP publishes keep the previous applied pointer until the next restart.
func (s *ResourceStore) PublishWithVersionState(ctx context.Context, v core.ResourceVersion, applied bool, operationID string, now time.Time) (core.ResourceVersion, error) {
	v, err := normalizeResourceVersion(v)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	if operationID == "" {
		return core.ResourceVersion{}, errors.New("发布资源: operation_id 不能为空")
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return core.ResourceVersion{}, fmt.Errorf("发布资源 %s/%s: 开启事务: %w", v.Kind, v.ResourceID, err)
	}
	defer func() { _ = tx.Rollback() }()

	version, err := insertVersionTx(ctx, tx, v)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	if err := publishTx(ctx, tx, v.Kind, v.ResourceID, v.ResourceUID, version.ID, applied, operationID, now); err != nil {
		return core.ResourceVersion{}, err
	}
	if err := finishOperationTx(ctx, tx, operationID, core.OperationSuccess, "", "", nil, now); err != nil {
		return core.ResourceVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.ResourceVersion{}, fmt.Errorf("发布资源 %s/%s: 提交: %w", v.Kind, v.ResourceID, err)
	}
	return version, nil
}

// publishTx 是指针写入的唯一 SQL 实现：插入时 applied 取决于本次发布，更新时
// `applied=false` 保留旧值；发布同时清掉删除标记（撤回 = 新版本）。
//
// 身份守卫：名称的活跃指针已指向**另一个**资源实例时拒绝——同名重建只允许发生在删除
// 之后（墓碑行），不得让两个活着的实例共用一个名称。`revision` 每次发布 +1（参与 ETag）。
// publishTx is the single pointer UPSERT. An active pointer already bound to another uid
// is refused: a same-name rebuild is only possible after the previous delete.
func publishTx(ctx context.Context, tx *sql.Tx, kind core.ResourceKind, resourceID string,
	uid core.ResourceUID, versionID int64, applied bool, operationID string, now time.Time,
) error {
	if now.IsZero() {
		now = time.Now()
	}
	if err := checkPointerIdentity(ctx, tx, kind, resourceID, uid); err != nil {
		return err
	}
	// 身份行与指针同一事务：版本 / 指针提交成功而身份行缺失，会让"按 UID 查名称"
	// 与删除闸门失去权威来源。首版发布顺手写身份行；后续发布核对身份未被挪用。
	// The identity row is written in the same transaction as the pointer: a committed
	// pointer without its identity would leave the uid history unanswerable.
	if err := ensureIdentityTx(ctx, tx, kind, resourceID, uid, operationID, now); err != nil {
		return err
	}

	_, err := tx.ExecContext(ctx,
		`INSERT INTO resource_pointers
		   (resource_kind, resource_id, resource_uid, published_version_id, applied_version_id,
		    scope, owner_agent_uid, revision, deleted_at, updated_at, operation_id)
		 VALUES (?, ?, ?, ?, CASE WHEN ? THEN ? ELSE NULL END, 'shared', NULL, 1, NULL, ?, ?)
		 ON CONFLICT(resource_kind, resource_id) DO UPDATE SET
		   resource_uid = excluded.resource_uid,
		   published_version_id = excluded.published_version_id,
		   applied_version_id = CASE WHEN ? THEN excluded.applied_version_id
		                             ELSE resource_pointers.applied_version_id END,
		   revision = resource_pointers.revision + 1,
		   deleted_at = NULL,
		   updated_at = excluded.updated_at,
		   operation_id = excluded.operation_id`,
		string(kind), resourceID, string(uid), versionID, applied, versionID, now.UnixMilli(),
		operationID, applied)
	if err != nil {
		return fmt.Errorf("更新发布指针 %s/%s: %w", kind, resourceID, err)
	}
	return nil
}

// checkPointerIdentity 读当前指针行并核对实例身份：无行 → 可插入；有行且是同 UID
// （含已删除的同一代 → 撤回）→ 可更新；有行且是**活跃行**上的另一个 UID → 拒绝。
// checkPointerIdentity allows a new row, the same uid, or a tombstoned predecessor; an
// active pointer bound to a different uid is refused.
func checkPointerIdentity(ctx context.Context, tx *sql.Tx, kind core.ResourceKind, resourceID string, uid core.ResourceUID) error {
	var (
		currentUID sql.NullString
		deletedAt  sql.NullInt64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT resource_uid, deleted_at FROM resource_pointers
		  WHERE resource_kind = ? AND resource_id = ?`, string(kind), resourceID).
		Scan(&currentUID, &deletedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("校验发布指针身份 %s/%s: %w", kind, resourceID, err)
	case !deletedAt.Valid && currentUID.Valid && currentUID.String != string(uid):
		return fmt.Errorf("%w: %s/%s 当前指向 %s，不能发布 %s 的版本",
			core.ErrResourceUIDMismatch, kind, resourceID, currentUID.String, uid)
	}
	return nil
}

// ensureIdentityTx 让资源身份行与发布事务同生：首版发布插入身份，后续发布核对
// 身份未被挪用（同 UID 只能属于同一 `(kind, 名称)`），已删除的 UID 一律拒绝复用。
// 身份表的 INSERT 仍只有 `insertIdentityTx` 一处实现。
// ensureIdentityTx writes the identity row on first publish and verifies on later ones;
// a tombstoned uid is never revived.
func ensureIdentityTx(ctx context.Context, tx *sql.Tx, kind core.ResourceKind, name string,
	uid core.ResourceUID, operationID string, at time.Time,
) error {
	var (
		gotKind string
		gotName string
		state   string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT resource_kind, name, state FROM resource_identities WHERE resource_uid = ?`,
		string(uid)).Scan(&gotKind, &gotName, &state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		rec, normErr := normalizeIdentity(core.ResourceIdentity{
			UID:         uid,
			Kind:        kind,
			Name:        name,
			State:       core.IdentityActive,
			CreatedAt:   at.UnixMilli(),
			OperationID: operationID,
		})
		if normErr != nil {
			return normErr
		}
		return insertIdentityTx(ctx, tx, rec)
	case err != nil:
		return fmt.Errorf("读取资源身份 %s: %w", uid, err)
	case gotKind != string(kind) || gotName != name:
		return fmt.Errorf("%w: 资源身份 %s 已属于 %s/%s，不能用于 %s/%s",
			core.ErrResourceUIDMismatch, uid, gotKind, gotName, kind, name)
	case state != string(core.IdentityActive):
		return fmt.Errorf("%w: 资源身份 %s 已是 %s 状态，UID 不得复用",
			core.ErrResourceUIDMismatch, uid, state)
	}
	return nil
}

// checkVersionBelongsTo 确认目标版本存在且属于 `(kind, resource_id)`——把 A 资源的
// 版本挂到 B 的指针上属于数据事故，必须在写指针前拒绝。返回版本的资源 UID（指针身份
// 的唯一来源：版本行自身）。
// checkVersionBelongsTo rejects cross-resource pointer updates and returns the version's
// resource uid, which is the only authority for the pointer's identity.
func checkVersionBelongsTo(ctx context.Context, tx *sql.Tx, kind core.ResourceKind, resourceID string, versionID int64) (core.ResourceUID, error) {
	var (
		gotKind core.ResourceKind
		gotUID  core.ResourceUID
		gotID   string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT resource_kind, resource_uid, resource_id FROM resource_versions WHERE id = ?`, versionID).
		Scan(&gotKind, &gotUID, &gotID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: 版本 %d", core.ErrResourceNotFound, versionID)
	}
	if err != nil {
		return "", fmt.Errorf("校验资源版本 %d: %w", versionID, err)
	}
	if gotKind != kind || gotID != resourceID {
		return "", fmt.Errorf("版本 %d 属于 %s/%s，不能发布为 %s/%s", versionID, gotKind, gotID, kind, resourceID)
	}
	if _, err := core.ParseResourceUID(string(gotUID)); err != nil {
		return "", fmt.Errorf("版本 %d 的资源 UID 非法: %w", versionID, err)
	}
	return gotUID, nil
}

// MarkDeleted 给资源指针打删除标记（颗粒粒文档 §3.1.6 图 G）：只置 `deleted_at`、
// `updated_at` 与 `operation_id`，**不删指针行、不删版本**——版本历史与审计证据完整保留。
// 身份按名称当前指向的实例解析（管理面按名称寻址，UID 校验由调用方在条件写里先行完成）；
// 需要严格的 UID 相等判据时用 MarkDeletedByUID。
// MarkDeleted stamps the tombstone on the instance the name currently points at;
// MarkDeletedByUID carries the strict uid equality guard.
func (s *ResourceStore) MarkDeleted(ctx context.Context, kind core.ResourceKind, id, opID string, at time.Time) error {
	if err := validateResourceKind(kind); err != nil {
		return err
	}
	if id == "" {
		return errors.New("删除资源: resource_id 不能为空")
	}
	if opID == "" {
		return errors.New("删除资源: operation_id 不能为空")
	}
	if at.IsZero() {
		at = time.Now()
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("删除资源 %s/%s: 开启事务: %w", kind, id, err)
	}
	defer func() { _ = tx.Rollback() }()

	var uid sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT resource_uid FROM resource_pointers WHERE resource_kind = ? AND resource_id = ?`,
		string(kind), id).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s/%s", core.ErrResourceNotFound, kind, id)
	}
	if err != nil {
		return fmt.Errorf("删除资源 %s/%s: 读取发布指针: %w", kind, id, err)
	}
	if !uid.Valid {
		// v3 迁移回填后指针必有 UID；无 UID 说明库被外部改写，拒绝猜身份。
		return fmt.Errorf("删除资源 %s/%s: 指针缺少资源 UID，拒绝删除", kind, id)
	}

	if err := markDeletedTx(ctx, tx, kind, id, core.ResourceUID(uid.String), opID, at); err != nil {
		return err
	}
	if err := finishOperationTx(ctx, tx, opID, core.OperationSuccess, "", "", nil, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("删除资源 %s/%s: 提交: %w", kind, id, err)
	}
	return nil
}

// MarkDeletedByUID 是带严格身份判据的删除：`resource_uid` 必须与名称当前指针一致，
// 不一致 → core.ErrResourceUIDMismatch 且零写入（条件写的存储侧判据）。
// MarkDeletedByUID requires the uid to match the current pointer exactly.
func (s *ResourceStore) MarkDeletedByUID(ctx context.Context, kind core.ResourceKind, id string,
	uid core.ResourceUID, opID string, at time.Time,
) error {
	if err := validateResourceKind(kind); err != nil {
		return err
	}
	if id == "" {
		return errors.New("删除资源: resource_id 不能为空")
	}
	if opID == "" {
		return errors.New("删除资源: operation_id 不能为空")
	}
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return fmt.Errorf("删除资源 %s/%s: %w", kind, id, err)
	}
	if at.IsZero() {
		at = time.Now()
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("删除资源 %s/%s: 开启事务: %w", kind, id, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := markDeletedTx(ctx, tx, kind, id, canonical, opID, at); err != nil {
		return err
	}
	if err := finishOperationTx(ctx, tx, opID, core.OperationSuccess, "", "", nil, at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("删除资源 %s/%s: 提交: %w", kind, id, err)
	}
	return nil
}

// markDeletedTx 是墓碑写入的唯一 SQL 实现；`uid` 参与 WHERE，名称与实例必须同时对上。
// 同一实例重复删除幂等（标记覆盖为最近一次的时间与操作号）。
// markDeletedTx is the single tombstone UPDATE; repeating it for the same instance is
// idempotent and only moves the timestamp and operation id forward.
func markDeletedTx(ctx context.Context, tx *sql.Tx, kind core.ResourceKind, id string,
	uid core.ResourceUID, opID string, at time.Time,
) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE resource_pointers
		    SET deleted_at = ?, updated_at = ?, operation_id = ?
		  WHERE resource_kind = ? AND resource_id = ? AND resource_uid = ?`,
		at.UnixMilli(), at.UnixMilli(), opID, string(kind), id, string(uid))
	if err != nil {
		return fmt.Errorf("删除资源 %s/%s: 标记删除: %w", kind, id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("删除资源 %s/%s: 读取影响行数: %w", kind, id, err)
	}
	if affected > 0 {
		// 身份墓碑与指针墓碑同一事务：`resource_identities` 是 UID 级删除事实的权威
		// 来源，漏写会让"这个实例当时被删过吗"只能靠指针行反推。
		missing, err := tombstoneIdentityTx(ctx, tx, uid, opID, at.UnixMilli())
		if err != nil {
			return fmt.Errorf("删除资源 %s/%s: 写身份墓碑: %w", kind, id, err)
		}
		if missing {
			return fmt.Errorf("%w: 资源身份 %s", core.ErrResourceNotFound, uid)
		}
		// 删除与凭据退役同事务（R4.8）：不存在"资源已删、凭据仍 active"的窗口。
		// 只有模型连接与通知渠道有托管凭据；其余类别没有凭据行，退役是空操作。
		switch kind {
		case core.ResourceModelConnection, core.ResourceNotifyChannel:
			if err := retireCredentialByOwnerTx(ctx, tx, core.BindingKind(kind), uid, opID, at); err != nil {
				return fmt.Errorf("删除资源 %s/%s: 退役凭据: %w", kind, id, err)
			}
		}
		return nil
	}

	// 0 行：分诊"没有这一行"与"名称已指向另一个实例"——两者都是零写入，但对外的
	// 裁决不同（404 vs 412 语义）。
	var current sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT resource_uid FROM resource_pointers WHERE resource_kind = ? AND resource_id = ?`,
		string(kind), id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s/%s", core.ErrResourceNotFound, kind, id)
	}
	if err != nil {
		return fmt.Errorf("删除资源 %s/%s: 读取发布指针: %w", kind, id, err)
	}
	return fmt.Errorf("%w: %s/%s 当前指向 %s，不能删除 %s",
		core.ErrResourceUIDMismatch, kind, id, current.String, uid)
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

// PointerByUID 按资源实例身份读它当前占据的名称与指针行（含删除标记）——
// 凭据与范围动作按 UID 寻址，不经过名称猜测。未命中 → core.ErrResourceNotFound。
// PointerByUID reads the pointer row by resource identity; credential and scope actions
// address resources by uid rather than guessing from the name.
func (s *ResourceStore) PointerByUID(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) (core.ResourcePointer, error) {
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return core.ResourcePointer{}, err
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+pointerColumns+` FROM resource_pointers
		  WHERE resource_kind = ? AND resource_uid = ?`, string(kind), string(canonical))
	ptr, err := scanPointer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ResourcePointer{}, fmt.Errorf("%w: %s/%s", core.ErrResourceNotFound, kind, canonical)
	}
	if err != nil {
		return core.ResourcePointer{}, fmt.Errorf("读取发布指针 %s/%s: %w", kind, canonical, err)
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

// ScopeConversion 是一次范围转换的输入（颗粒度文档 R4.4 / T121）。
// ExpectUID 是调用方显式给出的资源身份：**没有它就不转**——按名称猜测目标会把动作
// 落到同名重建的新实例上。DocumentRefs 是定义文件整词扫描的结果、RouteBlockers 是
// 消息路由端口给出的阻断项（本轮恒为空，端口留给消息接入阶段），两者都由调用方在
// 进入本方法前取得：文件系统与外部端口的取证不属于数据库事务。
// ScopeConversion carries one scope change; the caller must supply the resource uid
// explicitly, and both the document scan and route blockers resolve before the
// transaction starts.
type ScopeConversion struct {
	Kind          core.ResourceKind
	Name          string
	ExpectUID     core.ResourceUID
	Target        core.ResourceScope
	OwnerAgentUID core.AgentUID // Target=exclusive 时必填
	DocumentRefs  []string
	RouteBlockers []string
	OperationID   string
	At            time.Time
}

// ConvertScope 在**同一事务**里重读目标、收集占用证据并写范围：
// 检查与变更之间没有窗口，任一步失败整笔回滚（失败零写入）。
//
// 占用证据（只在转 exclusive 时收集）：
//   - 其他 Agent 的绑定投影（含归档 Agent，`agent_uid <> 目标属主`）；
//   - 调用方带来的定义文件整词扫描结果（投影不可信时的 fail-closed 证据）；
//   - 未释放的运行引用——模型连接看 `run_resource_refs`，通知渠道看 `run_notify_refs`；
//   - 调用方带来的路由阻断项。
//
// 有阻断 → 返回证据与 `core.ErrResourceScopeInUse`，**不写**；调用方据此 409 点名。
// 没有阻断 → 写 `scope` / `owner_agent_uid` 并把 `revision` +1（参与资源 ETag）。
// ConvertScope re-reads the target, gathers blockers and writes the scope inside one
// transaction; any blocker aborts the write and is returned to the caller.
func (s *ResourceStore) ConvertScope(ctx context.Context, req ScopeConversion) (core.ScopeUsage, error) {
	usage := core.ScopeUsage{
		DocumentRefs:  req.DocumentRefs,
		MessageRoutes: req.RouteBlockers,
	}
	if err := normalizeScopeConversion(req); err != nil {
		return usage, err
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return usage, fmt.Errorf("转换资源范围 %s/%s: 开启事务: %w", req.Kind, req.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	currentUID, err := currentScopeTargetTx(ctx, tx, req.Kind, req.Name)
	if err != nil {
		return usage, err
	}
	if currentUID != req.ExpectUID {
		return usage, fmt.Errorf("%w: %s/%s 当前指向 %s，请求指向 %s",
			core.ErrResourceUIDMismatch, req.Kind, req.Name, currentUID, req.ExpectUID)
	}

	if req.Target == core.ScopeExclusive {
		usage, err = scopeUsageTx(ctx, tx, req)
		if err != nil {
			return usage, err
		}
		if usage.InUse() {
			return usage, fmt.Errorf("%w: %s/%s 仍被引用（%s）", core.ErrResourceScopeInUse,
				req.Kind, req.Name, strings.Join(usage.Blockers(), "、"))
		}
	}

	if err := writeScopeTx(ctx, tx, req); err != nil {
		return usage, err
	}
	if err := tx.Commit(); err != nil {
		return usage, fmt.Errorf("转换资源范围 %s/%s: 提交: %w", req.Kind, req.Name, err)
	}
	return usage, nil
}

// normalizeScopeConversion 校验输入：只覆盖带使用范围的两类资源、名称与身份必填、
// 目标是 exclusive 时必须给出合法属主。范围字面量由 core.ParseScope 逐字判定。
// normalizeScopeConversion validates the request before any SQL runs.
func normalizeScopeConversion(req ScopeConversion) error {
	switch req.Kind {
	case core.ResourceModelConnection, core.ResourceNotifyChannel:
	default:
		return fmt.Errorf("%w: 范围转换只覆盖 model_connection / notify_channel，收到 %q",
			core.ErrResourceNotFound, req.Kind)
	}
	if req.Name == "" {
		return errors.New("转换资源范围: 名称不能为空")
	}
	if _, err := core.ParseResourceUID(string(req.ExpectUID)); err != nil {
		return fmt.Errorf("转换资源范围 %s/%s: 必须显式给出资源身份: %w", req.Kind, req.Name, err)
	}
	scope, err := core.ParseScope(string(req.Target))
	if err != nil {
		return fmt.Errorf("转换资源范围 %s/%s: %w", req.Kind, req.Name, err)
	}
	if scope == core.ScopeExclusive {
		if _, err := core.ParseAgentUID(string(req.OwnerAgentUID)); err != nil {
			return fmt.Errorf("转换资源范围 %s/%s: 转专属必须给出合法属主: %w",
				req.Kind, req.Name, err)
		}
	} else if req.OwnerAgentUID != "" {
		return fmt.Errorf("转换资源范围 %s/%s: 共享范围不得带属主", req.Kind, req.Name)
	}
	return nil
}

// currentScopeTargetTx 在事务内读目标当前指向的资源身份（按名称解析**活动**实例）。
// 模型连接读发布指针，通知渠道读渠道行（渠道没有版本表，指针表里也没有它的行）。
// currentScopeTargetTx resolves the resource uid the name currently points at.
func currentScopeTargetTx(ctx context.Context, tx *sql.Tx, kind core.ResourceKind, name string) (core.ResourceUID, error) {
	var (
		uid       sql.NullString
		deletedAt sql.NullInt64
	)
	query := `SELECT resource_uid, deleted_at FROM resource_pointers
	           WHERE resource_kind = ? AND resource_id = ?`
	if kind == core.ResourceNotifyChannel {
		query = `SELECT resource_uid, deleted_at FROM notify_channels WHERE name = ?`
	}
	args := []any{string(kind), name}
	if kind == core.ResourceNotifyChannel {
		args = []any{name}
	}
	err := tx.QueryRowContext(ctx, query, args...).Scan(&uid, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s/%s", core.ErrResourceNotFound, kind, name)
	}
	if err != nil {
		return "", fmt.Errorf("转换资源范围 %s/%s: 读取当前实例: %w", kind, name, err)
	}
	if deletedAt.Valid || !uid.Valid {
		return "", fmt.Errorf("%w: %s/%s", core.ErrResourceNotFound, kind, name)
	}
	return core.ResourceUID(uid.String), nil
}

// scopeUsageTx 收集共享转专属的占用证据（顺序确定，便于比对与点名）。
// scopeUsageTx gathers the blockers that forbid narrowing a shared resource.
func scopeUsageTx(ctx context.Context, tx *sql.Tx, req ScopeConversion) (core.ScopeUsage, error) {
	usage := core.ScopeUsage{
		DocumentRefs:  req.DocumentRefs,
		MessageRoutes: req.RouteBlockers,
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT agent_uid FROM agent_resource_bindings
		  WHERE resource_uid = ? AND agent_uid <> ? ORDER BY agent_uid ASC`,
		string(req.ExpectUID), string(req.OwnerAgentUID))
	if err != nil {
		return usage, fmt.Errorf("转换资源范围 %s/%s: 读取绑定占用: %w", req.Kind, req.Name, err)
	}
	bound, err := collectStrings(rows)
	if err != nil {
		return usage, fmt.Errorf("转换资源范围 %s/%s: 读取绑定占用: %w", req.Kind, req.Name, err)
	}
	for _, uid := range bound {
		usage.BoundAgents = append(usage.BoundAgents, core.AgentUID(uid))
	}

	runQuery := `SELECT DISTINCT run_id FROM run_resource_refs
	              WHERE resource_uid = ? AND released_at IS NULL ORDER BY run_id ASC`
	if req.Kind == core.ResourceNotifyChannel {
		runQuery = `SELECT DISTINCT run_id FROM run_notify_refs
		             WHERE channel_uid = ? AND released_at IS NULL ORDER BY run_id ASC`
	}
	runRows, err := tx.QueryContext(ctx, runQuery, string(req.ExpectUID))
	if err != nil {
		return usage, fmt.Errorf("转换资源范围 %s/%s: 读取运行占用: %w", req.Kind, req.Name, err)
	}
	runs, err := collectStrings(runRows)
	if err != nil {
		return usage, fmt.Errorf("转换资源范围 %s/%s: 读取运行占用: %w", req.Kind, req.Name, err)
	}
	usage.ActiveRuns = runs
	return usage, nil
}

// collectStrings 读一个单列结果集（调用方负责关闭）。
// collectStrings reads a single-column result set.
func collectStrings(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// writeScopeTx 写范围与属主并把 revision +1（地址 / 范围 / 凭据变化都算一次修订）。
// writeScopeTx writes the scope and bumps the revision.
func writeScopeTx(ctx context.Context, tx *sql.Tx, req ScopeConversion) error {
	at := req.At
	if at.IsZero() {
		at = time.Now()
	}
	query := `UPDATE resource_pointers
	             SET scope = ?, owner_agent_uid = ?, revision = revision + 1,
	                 updated_at = ?, operation_id = ?
	           WHERE resource_kind = ? AND resource_id = ? AND resource_uid = ?`
	args := []any{
		string(req.Target), nullIfEmpty(string(req.OwnerAgentUID)), at.UnixMilli(),
		req.OperationID, string(req.Kind), req.Name, string(req.ExpectUID),
	}
	if req.Kind == core.ResourceNotifyChannel {
		query = `UPDATE notify_channels
		            SET scope = ?, owner_agent_uid = ?, revision = revision + 1,
		                updated_at = ?, operation_id = ?
		          WHERE name = ? AND resource_uid = ? AND deleted_at IS NULL`
		args = []any{
			string(req.Target), nullIfEmpty(string(req.OwnerAgentUID)), at.UnixMilli(),
			req.OperationID, req.Name, string(req.ExpectUID),
		}
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("转换资源范围 %s/%s: %w", req.Kind, req.Name, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("转换资源范围 %s/%s: 读取影响行数: %w", req.Kind, req.Name, err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: %s/%s 的范围写入未生效（影响 %d 行）",
			core.ErrResourceNotFound, req.Kind, req.Name, affected)
	}
	return nil
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

// Versions 按**名称快照**取历史版本（含同名重建的前代），按版本号倒序。这是存储层
// 的原始能力；面对调用方的「按名只回当前指针 UID」规则（R4.5）由服务层用
// VersionsByUID 落实，两处不得互相替代。
// Versions lists versions by name snapshot, including previous generations of a
// same-name rebuild. The "current uid only" surface rule (R4.5) is enforced in the
// service layer through VersionsByUID, not by narrowing this primitive.
func (s *ResourceStore) Versions(ctx context.Context, kind core.ResourceKind, resourceID string) ([]core.ResourceVersion, error) {
	return s.queryVersions(ctx,
		`SELECT `+versionColumns+` FROM resource_versions
		  WHERE resource_kind = ? AND resource_id = ? ORDER BY version_no DESC, id DESC`,
		string(kind), resourceID)
}

// VersionsByUID 取一个资源实例的不可变版本，按版本号倒序（最新在前）——身份维度的
// 唯一正确读法：同名重建的另一代不会被混进来。
// VersionsByUID lists one resource instance's versions, newest first.
func (s *ResourceStore) VersionsByUID(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) ([]core.ResourceVersion, error) {
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return nil, err
	}
	return s.queryVersions(ctx,
		`SELECT `+versionColumns+` FROM resource_versions
		  WHERE resource_kind = ? AND resource_uid = ? ORDER BY version_no DESC, id DESC`,
		string(kind), string(canonical))
}

// queryVersions 是版本列表查询的唯一扫描实现（按名称与按 UID 共用同一列序与错误口径）。
// queryVersions is the shared scan path of the two version listings.
func (s *ResourceStore) queryVersions(ctx context.Context, query string, args ...any) ([]core.ResourceVersion, error) {
	rows, err := s.db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("列出资源版本: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var versions []core.ResourceVersion
	for rows.Next() {
		version, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描资源版本: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资源版本: %w", err)
	}
	return versions, nil
}

// ReferencedBy 返回仍持有该资源某版本的 Run（`released_at IS NULL`），按 run_id 升序
// 去重——只统计未释放引用，已结束 Run 的历史行不计入。按名称匹配包含迁移前
// 无法证明归属的旧行；身份维度的判据见 ReferencedByUID。
// ReferencedBy lists runs still holding an unreleased reference by name, ascending and
// distinct; identity-scoped callers use ReferencedByUID.
func (s *ResourceStore) ReferencedBy(ctx context.Context, kind core.ResourceKind, resourceID string) ([]string, error) {
	return s.queryReferencedBy(ctx,
		`SELECT DISTINCT run_id FROM run_resource_refs
		  WHERE resource_kind = ? AND resource_id = ? AND released_at IS NULL
		  ORDER BY run_id ASC`, string(kind), resourceID)
}

// ReferencedByUID 按资源实例身份给出未释放引用的 Run（同名重建的两代互不影响）。
// ReferencedByUID lists unreleased runs by resource identity.
func (s *ResourceStore) ReferencedByUID(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) ([]string, error) {
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return nil, err
	}
	return s.queryReferencedBy(ctx,
		`SELECT DISTINCT run_id FROM run_resource_refs
		  WHERE resource_kind = ? AND resource_uid = ? AND released_at IS NULL
		  ORDER BY run_id ASC`, string(kind), string(canonical))
}

// queryReferencedBy 是引用查询的唯一扫描实现。
// queryReferencedBy is the shared scan path of the reference queries.
func (s *ResourceStore) queryReferencedBy(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("列出资源引用: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return nil, fmt.Errorf("扫描资源引用: %w", err)
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资源引用: %w", err)
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
		   (agent_name, agent_uid, uid_source, state, generation, state_file_hash, valid,
		    error_message, updated_at, operation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(agent_name) DO UPDATE SET
		   agent_uid = COALESCE(excluded.agent_uid, agent_lifecycle.agent_uid),
		   uid_source = COALESCE(excluded.uid_source, agent_lifecycle.uid_source),
		   state = excluded.state,
		   generation = excluded.generation,
		   state_file_hash = excluded.state_file_hash,
		   valid = excluded.valid,
		   error_message = excluded.error_message,
		   updated_at = excluded.updated_at,
		   operation_id = excluded.operation_id`,
		rec.Agent, nullIfEmpty(string(rec.AgentUID)), uidSourceOf(rec.AgentUID),
		state, generation, fileHash, validInt, errMessage,
		updatedAt, nullIfEmpty(rec.OperationID))
	if err != nil {
		return fmt.Errorf("写生命周期索引 %s: %w", rec.Agent, err)
	}
	return nil
}

// uidSourceOf 把身份映射为镜像来源：有 UID = 状态文件（`state_file`），无 UID = NULL
// （"尚未对账"）。`legacy_unrecorded` 只由迁移回填写入，索引重建不擅自改判。
// uidSourceOf maps an identity to its mirror source; only the migration writes
// legacy_unrecorded, so a rebuilt index never re-judges it.
func uidSourceOf(uid core.AgentUID) any {
	if uid == "" {
		return nil
	}
	return "state_file"
}

// AgentIdentity 读 `agent_lifecycle` 的身份镜像：返回 UID、来源与行是否存在。
// 存在但两列都为空 = 迁移尚未对账（found 仍为 true）。
// AgentIdentity reads the identity mirror of one agent.
func (s *ResourceStore) AgentIdentity(ctx context.Context, agent string) (core.AgentUID, string, bool, error) {
	var (
		uid    sql.NullString
		source sql.NullString
	)
	err := s.db.read.QueryRowContext(ctx,
		`SELECT agent_uid, uid_source FROM agent_lifecycle WHERE agent_name = ?`, agent).
		Scan(&uid, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("读取生命周期身份镜像 %s: %w", agent, err)
	}
	return core.AgentUID(uid.String), source.String, true, nil
}

// AgentNameByUID 是身份镜像的反向查询：由 Agent UID 找它当前的名称，供 exclusive
// 资源的 `owner_agent` 展示。活动 Agent 取 `agent_lifecycle`，归档 Agent 回退到
// `agent_archives`（最近一次未恢复的归档）——归档 Agent 仍可能是专属资源的属主，
// 找不到名字不等于没有属主。两处都没有 → found=false，调用方**不得**把查不到写成
// 空属主；UID 合法性由调用方先行判卷。
// AgentNameByUID resolves the current name of an agent uid, falling back to the
// archive rows because an archived agent can still own exclusive resources.
func (s *ResourceStore) AgentNameByUID(ctx context.Context, uid core.AgentUID) (string, bool, error) {
	if uid == "" {
		return "", false, nil
	}
	var name string
	err := s.db.read.QueryRowContext(ctx,
		`SELECT agent_name FROM agent_lifecycle WHERE agent_uid = ?`, string(uid)).Scan(&name)
	if err == nil {
		return name, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("读取 Agent 身份 %s 的名称: %w", uid, err)
	}
	err = s.db.read.QueryRowContext(ctx,
		`SELECT agent_name FROM agent_archives WHERE agent_uid = ? AND restored_at IS NULL
		  ORDER BY archived_at DESC LIMIT 1`, string(uid)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取归档 Agent %s 的名称: %w", uid, err)
	}
	return name, true, nil
}

// BackfillAgentUID 是 v3 迁移 2c 的数据库回填（契约 3g → 2c：**必须**在 0003 加列之后）：
// 以权威状态文件为准写入 `agent_uid` / `uid_source`。
//
//   - 状态文件给了 UID → `uid_source='state_file'`；数据库已有**不同**的非空 UID →
//     `core.ErrLifecycleConflict`（迁移中止，**不覆盖任一侧**）；
//   - 状态文件缺失（uid 为空）→ `uid_source='legacy_unrecorded'`，**不按同名猜**；
//     数据库已有非空 UID 却丢了状态文件同样中止（两边对不上就是证据不足）。
//
// 已一致的行幂等 no-op（重跑不刷新 updated_at，也不改操作归属）。
// BackfillAgentUID mirrors the authoritative state-file identity into agent_lifecycle;
// any disagreement between the two sides aborts the migration instead of picking one.
func (s *ResourceStore) BackfillAgentUID(ctx context.Context, agent string, uid core.AgentUID, operationID string, at time.Time) error {
	if agent == "" {
		return errors.New("回填 Agent 身份: agent 不能为空")
	}
	if uid != "" {
		if _, err := core.ParseAgentUID(string(uid)); err != nil {
			return fmt.Errorf("回填 Agent 身份 %s: %w", agent, err)
		}
	}
	if at.IsZero() {
		at = time.Now()
	}

	existing, source, found, err := s.AgentIdentity(ctx, agent)
	if err != nil {
		return err
	}
	if found {
		if existing != "" && existing != uid {
			return fmt.Errorf("%w: Agent %s 的数据库身份 %q 与状态文件身份 %q 不一致，迁移中止",
				core.ErrLifecycleConflict, agent, existing, uid)
		}
		if source == uidSourceString(uid) && existing == uid {
			return nil // 两侧已一致：幂等 no-op
		}
	}

	if found {
		_, err = s.db.write.ExecContext(ctx,
			`UPDATE agent_lifecycle
			    SET agent_uid = ?, uid_source = ?, updated_at = ?, operation_id = ?
			  WHERE agent_name = ?`,
			nullIfEmpty(string(uid)), uidSourceString(uid), at.UnixMilli(),
			nullIfEmpty(operationID), agent)
	} else {
		_, err = s.db.write.ExecContext(ctx,
			`INSERT INTO agent_lifecycle
			   (agent_name, agent_uid, uid_source, state, generation, valid, updated_at, operation_id)
			 VALUES (?, ?, ?, NULL, 0, 0, ?, ?)`,
			agent, nullIfEmpty(string(uid)), uidSourceString(uid), at.UnixMilli(),
			nullIfEmpty(operationID))
	}
	if err != nil {
		return fmt.Errorf("回填 Agent 身份 %s: %w", agent, err)
	}
	return nil
}

// uidSourceString 给出回填值：有 UID = `state_file`，无 UID = `legacy_unrecorded`。
// uidSourceString returns the backfill literal for the migration's two outcomes.
func uidSourceString(uid core.AgentUID) string {
	if uid == "" {
		return "legacy_unrecorded"
	}
	return "state_file"
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
		uid            core.ResourceUID
		definitionYAML sql.NullString
		manifestJSON   sql.NullString
		storagePath    sql.NullString
	)
	err := row.Scan(&version.ID, &kind, &uid, &version.ResourceID, &version.VersionNo,
		&version.ContentHash, &definitionYAML, &manifestJSON, &storagePath,
		&version.CreatedAt, &version.OperationID)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	version.Kind = kind
	version.ResourceUID = uid
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
		uid       sql.NullString
		applied   sql.NullInt64
		scope     core.ResourceScope
		owner     sql.NullString
		deletedAt sql.NullInt64
	)
	err := row.Scan(&kind, &ptr.ResourceID, &uid, &ptr.PublishedVersionID, &applied,
		&scope, &owner, &ptr.Revision, &deletedAt, &ptr.UpdatedAt, &ptr.OperationID)
	if err != nil {
		return core.ResourcePointer{}, err
	}
	ptr.Kind = kind
	// v3 迁移回填后 resource_uid 必有值；列可空只因 SQLite 的 ALTER ADD COLUMN 无法
	// 带 NOT NULL（契约注）。读到 NULL 说明库被外部改写，按零值如实呈现，不猜身份。
	ptr.ResourceUID = core.ResourceUID(uid.String)
	ptr.Scope = scope
	ptr.OwnerAgentUID = core.AgentUID(owner.String)
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

// versionByHash 按 `(kind, resource_uid, content_hash)` 查已有版本（唯一性由应用层保证）。
// versionByHash looks up an existing version by resource identity and content hash.
func versionByHash(ctx context.Context, q queryer, kind core.ResourceKind, uid core.ResourceUID, contentHash string) (core.ResourceVersion, bool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+versionColumns+` FROM resource_versions
		  WHERE resource_kind = ? AND resource_uid = ? AND content_hash = ?
		  ORDER BY id ASC LIMIT 1`, string(kind), string(uid), contentHash)
	version, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ResourceVersion{}, false, nil
	}
	if err != nil {
		return core.ResourceVersion{}, false, fmt.Errorf("按内容哈希查询资源版本 %s/%s: %w", kind, uid, err)
	}
	return version, true, nil
}

// nextVersionNo 返回该资源实例的下一个版本号（从 1 起）——同名重建（新 UID）重新计数。
// nextVersionNo returns the next monotonic version number for one resource uid.
func nextVersionNo(ctx context.Context, q queryer, kind core.ResourceKind, uid core.ResourceUID) (int, error) {
	var next int
	err := q.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version_no), 0) + 1 FROM resource_versions
		  WHERE resource_kind = ? AND resource_uid = ?`, string(kind), string(uid)).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("计算资源版本号 %s/%s: %w", kind, uid, err)
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

// normalizeResourceVersion 校验版本行的必填字段与按类型的非空约束（与 schema CHECK 同口径），
// 并把资源 UID 规范化为大写载荷：身份是版本的键，缺失或非法一律在写库前拒绝——
// 存储层不按名称推断身份。
// normalizeResourceVersion validates the row and canonicalizes the uid; a missing or
// malformed uid is refused because identity is never inferred from the name.
func normalizeResourceVersion(v core.ResourceVersion) (core.ResourceVersion, error) {
	if err := validateResourceKind(v.Kind); err != nil {
		return core.ResourceVersion{}, err
	}
	uid, err := core.ParseResourceUID(string(v.ResourceUID))
	if err != nil {
		return core.ResourceVersion{}, fmt.Errorf("资源版本 %s/%s: %w", v.Kind, v.ResourceID, err)
	}
	v.ResourceUID = uid
	if v.ResourceID == "" {
		return core.ResourceVersion{}, errors.New("资源版本: resource_id 不能为空")
	}
	if v.ContentHash == "" {
		return core.ResourceVersion{}, errors.New("资源版本: content_hash 不能为空")
	}
	switch v.Kind {
	case core.ResourceModelConnection, core.ResourceMCPServer:
		if v.DefinitionYAML == "" {
			return core.ResourceVersion{}, fmt.Errorf("资源版本 %s/%s: 定义原文不能为空", v.Kind, v.ResourceID)
		}
	case core.ResourceSkill:
		if v.ManifestJSON == "" || v.StoragePath == "" {
			return core.ResourceVersion{}, fmt.Errorf("资源版本 %s/%s: 技能版本必须带清单与存储路径", v.Kind, v.ResourceID)
		}
	}
	return v, nil
}
