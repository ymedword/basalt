package storage

// 本文件是 `resource_identities` 的**唯一 SQL 入口**（颗粒度文档 R4.5 / T113）：
// 身份一次写入、删除只翻状态、按 UID 查名称快照、按 (kind, uid) 分配版本号。
//
// 三条纪律：
//   - **身份不可变**：UID 写入后不改；名称只是创建时的标签，改名不回写身份行。
//   - **删除不物理清除**：墓碑（`state='deleted'` + `deleted_at`）保留，历史检索与
//     审计仍能回答"这个 UID 当时叫什么"；UID 永不复用。
//   - **版本号跟着实例走**：同名重建是新 UID，版本号从 1 重新计——本文件的
//     AllocateVersionNo 就是 `resource_versions` 取号的公开入口，不另立第二套。
//
// This file is the only SQL entry point for resource identities: written once, never
// renamed, tombstoned rather than deleted, and the single allocator of per-uid version
// numbers (a same-name rebuild starts again at one).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// identityColumns 是 resource_identities 的读取列序（scanIdentity 按同一顺序扫描）。
// identityColumns is the read order of the identity table.
const identityColumns = `resource_uid, resource_kind, name, state, created_at, deleted_at, operation_id`

// IdentityStore 是资源身份仓储。
// IdentityStore is the repository of resource identities.
type IdentityStore struct{ db *DB }

// NewIdentityStore 绑定一个已打开的写模式库句柄。
// NewIdentityStore binds the store to an opened writable database handle.
func NewIdentityStore(db *DB) *IdentityStore { return &IdentityStore{db: db} }

// Create 写入一条**新**身份（创建资源的动作）：已有同 UID 行说明身份被复用，
// 直接拒绝——UID 永不复用是身份纪律的底线。
// Create writes a brand-new identity; a re-used uid is refused because identities are
// never recycled.
func (s *IdentityStore) Create(ctx context.Context, rec core.ResourceIdentity) error {
	rec, err := normalizeIdentity(rec)
	if err != nil {
		return err
	}
	if rec.State == "" {
		rec.State = core.IdentityActive
	}
	if rec.State != core.IdentityActive || rec.DeletedAt != nil {
		return errors.New("创建资源身份: 新身份必须是 active 且无删除时间")
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("创建资源身份 %s: 开启事务: %w", rec.UID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := insertIdentityTx(ctx, tx, rec); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("创建资源身份 %s: 提交: %w", rec.UID, err)
	}
	return nil
}

// Tombstone 把身份置为 deleted 并写删除时间；重复删除幂等（保留第一次的时间）。
// 身份行**不删除**——历史版本、审计与引用都要靠它回答"这个 UID 是谁"。
// Tombstone flips the identity to deleted and is idempotent; the row is never removed.
func (s *IdentityStore) Tombstone(ctx context.Context, uid core.ResourceUID, operationID string, at time.Time) error {
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return err
	}
	if operationID == "" {
		return errors.New("删除资源身份: operation_id 不能为空")
	}
	if at.IsZero() {
		at = time.Now()
	}

	missing, err := tombstoneIdentityTx(ctx, s.db.write, canonical, operationID, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("删除资源身份 %s: %w", canonical, err)
	}
	if missing {
		return fmt.Errorf("%w: 资源身份 %s", core.ErrResourceNotFound, canonical)
	}
	return nil
}

// tombstoneIdentityTx 是身份墓碑的唯一 SQL（独立调用与同事务删除共用）：返回值
// `missing` 表示库中没有该 UID——调用方决定是 404 还是只是漏写。
// `deleted_at` 用 COALESCE 保留第一次删除时间，重复删除幂等。
// tombstoneIdentityTx is the single tombstone UPDATE; missing reports an absent uid and
// COALESCE keeps the first deletion time so retries stay idempotent.
func tombstoneIdentityTx(ctx context.Context, ex sqlExecutor, uid core.ResourceUID, operationID string, at int64) (bool, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE resource_identities
		    SET state = 'deleted', deleted_at = COALESCE(deleted_at, ?), operation_id = ?
		  WHERE resource_uid = ?`,
		at, operationID, string(uid))
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取影响行数: %w", err)
	}
	return affected == 0, nil
}

// IdentityByUID 按身份读一行（含墓碑）；未命中 → `core.ErrResourceNotFound`。
// IdentityByUID reads one identity row, tombstones included.
func (s *IdentityStore) IdentityByUID(ctx context.Context, uid core.ResourceUID) (core.ResourceIdentity, error) {
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return core.ResourceIdentity{}, err
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+identityColumns+` FROM resource_identities WHERE resource_uid = ?`, string(canonical))
	rec, err := scanIdentity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ResourceIdentity{}, fmt.Errorf("%w: 资源身份 %s", core.ErrResourceNotFound, canonical)
	}
	if err != nil {
		return core.ResourceIdentity{}, fmt.Errorf("读取资源身份 %s: %w", canonical, err)
	}
	return rec, nil
}

// IdentitiesByName 按 `(kind, 名称)` 读全部代际，按创建时间升序——同名重建的多代
// 实例在这里各自成行，调用方不得只取其中一行当成"这个名称的身份"。
// IdentitiesByName returns every generation of a name, oldest first.
func (s *IdentityStore) IdentitiesByName(ctx context.Context, kind core.ResourceKind, name string) ([]core.ResourceIdentity, error) {
	if _, err := core.ParseIdentityKind(string(kind)); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("读取资源身份: 名称不得为空")
	}
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+identityColumns+` FROM resource_identities
		  WHERE resource_kind = ? AND name = ? ORDER BY created_at ASC, resource_uid ASC`,
		string(kind), name)
	if err != nil {
		return nil, fmt.Errorf("列出资源身份 %s/%s: %w", kind, name, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.ResourceIdentity
	for rows.Next() {
		rec, err := scanIdentity(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描资源身份 %s/%s: %w", kind, name, err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资源身份 %s/%s: %w", kind, name, err)
	}
	return out, nil
}

// AllocateVersionNo 返回该资源实例的下一个版本号（从 1 起）。取号 SQL 与
// `resource_store.go` 共用同一实现：两处取号是漂移的开始。
// AllocateVersionNo returns the next version number for one resource instance; the SQL
// is shared with the version store so the two can never drift.
func (s *IdentityStore) AllocateVersionNo(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) (int, error) {
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return 0, err
	}
	if _, err := core.ParseResourceKind(string(kind)); err != nil {
		return 0, err
	}
	return nextVersionNo(ctx, s.db.read, kind, canonical)
}

// insertIdentityTx 是身份行插入的唯一 SQL 实现（资源创建路径与通知渠道创建共用；
// 调用方负责事务边界与"是否允许已存在"的判定）。
// insertIdentityTx is the single identity INSERT shared by every creation path.
func insertIdentityTx(ctx context.Context, ex sqlExecutor, rec core.ResourceIdentity) error {
	deletedAt := any(nil)
	if rec.DeletedAt != nil {
		deletedAt = *rec.DeletedAt
	}
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO resource_identities
		   (resource_uid, resource_kind, name, state, created_at, deleted_at, operation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(rec.UID), string(rec.Kind), rec.Name, string(rec.State),
		rec.CreatedAt, deletedAt, rec.OperationID); err != nil {
		return fmt.Errorf("写入资源身份 %s: %w", rec.UID, err)
	}
	return nil
}

// scanIdentity 按 identityColumns 的列序扫描一行；`deleted_at` 为 NULL 映射为 nil。
// scanIdentity scans one identity row, mapping a NULL deleted_at to nil.
func scanIdentity(row interface{ Scan(...any) error }) (core.ResourceIdentity, error) {
	var (
		rec       core.ResourceIdentity
		uid       string
		kind      string
		state     string
		deletedAt sql.NullInt64
	)
	err := row.Scan(&uid, &kind, &rec.Name, &state, &rec.CreatedAt, &deletedAt, &rec.OperationID)
	if err != nil {
		return core.ResourceIdentity{}, err
	}
	rec.UID = core.ResourceUID(uid)
	rec.Kind = core.ResourceKind(kind)
	rec.State = core.IdentityState(state)
	if deletedAt.Valid {
		value := deletedAt.Int64
		rec.DeletedAt = &value
	}
	return rec, nil
}

// normalizeIdentity 校验并规范化身份行：UID 合法、种类合法（身份表四值）、名称与
// 操作号非空、创建时间缺省为当下。状态与删除时间的一致性由调用方与 schema CHECK
// 共同保证（本函数只判"字段在不在、格式对不对"）。
// normalizeIdentity validates the identity row shape.
func normalizeIdentity(rec core.ResourceIdentity) (core.ResourceIdentity, error) {
	uid, err := core.ParseResourceUID(string(rec.UID))
	if err != nil {
		return core.ResourceIdentity{}, err
	}
	rec.UID = uid
	kind, err := core.ParseIdentityKind(string(rec.Kind))
	if err != nil {
		return core.ResourceIdentity{}, err
	}
	rec.Kind = kind
	if rec.Name == "" {
		return core.ResourceIdentity{}, errors.New("资源身份: 名称不得为空")
	}
	if rec.OperationID == "" {
		return core.ResourceIdentity{}, errors.New("资源身份: operation_id 不能为空")
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().UnixMilli()
	}
	return rec, nil
}
