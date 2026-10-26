package storage

// 本文件是托管凭据三张表的唯一 SQL 入口（颗粒度文档 R4.8 / T115）：
// `credentials`（身份 + 当前版本 + 撤销纪元）、`credential_versions`（不可变版本）、
// `resource_version_credentials`（发布版本的审计快照）。
//
// 四条纪律：
//   - **版本只追加**：版本行写入后除状态列与本次撤销清空的密文外不再改写；
//     新 Key 是新版本，不是原地改值。
//   - **撤销清空密文**：`revoked` 版本的 `ciphertext` 置 NULL，库中不再留可解密材料，
//     同时身份 `revocation_epoch` +1——固定旧纪元的 Run 因此能发现自己已失效。
//   - **版本撤销是终态**：被撤销的版本不能迁回可用；重新录入只追加**新编号**的版本
//     并把身份迁回 `active`，旧版本与 `revocation_epoch` 都不回退（2026-10-23 裁决）。
//   - **状态迁移走领域函数**：`core.*State.CanTransitionTo` 收口合法性，本层只执行。
//
// This file is the only SQL entry point for managed credentials: append-only versions,
// revoke blanks the ciphertext and bumps the epoch; re-entry reactivates the identity
// only, while revoked versions stay permanently unusable.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// credentialColumns 是 credentials 的读取列序（scanCredential 按同一顺序扫描）。
// credentialColumns is the read order of the credential identity table.
// #nosec G101 -- SQL 列序常量，值不是凭据（变量名命中）。
const credentialColumns = `credential_id, owner_kind, owner_uid, binding_key, owner_name, state,
	current_version, revocation_epoch, key_id, created_at, updated_at, operation_id`

// credentialVersionColumns 是 credential_versions 的读取列序。
// credentialVersionColumns is the read order of the version table.
// #nosec G101 -- SQL 列序常量，值不是凭据（变量名命中）。
const credentialVersionColumns = `credential_id, version_no, state, key_id, target_binding,
	last4, nonce, ciphertext, created_at, updated_at, operation_id`

// CredentialStore 是托管凭据仓储。
// CredentialStore is the managed-credential repository.
type CredentialStore struct{ db *DB }

// NewCredentialStore 绑定一个已打开的写模式库句柄。
// NewCredentialStore binds the store to an opened writable database handle.
func NewCredentialStore(db *DB) *CredentialStore { return &CredentialStore{db: db} }

// Create 写入凭据身份与第 1 个版本（同一事务）：身份一旦可见，它的活动版本就必须
// 同时可见——否则解析会读到"有身份没 Key"的中间态。同属主已有凭据 → 冲突
// （`UNIQUE (owner_kind, owner_uid)`；一个资源实例至多一条凭据身份）。
// Create writes the identity and version 1 in one transaction; one owner has at most
// one credential identity.
func (s *CredentialStore) Create(ctx context.Context, identity core.CredentialRecord, version core.CredentialVersionRecord) error {
	identity, version, err := normalizeCredentialPair(identity, version, 1)
	if err != nil {
		return err
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("创建凭据 %s: 开启事务: %w", identity.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := insertCredentialTx(ctx, tx, identity); err != nil {
		if isUniqueViolation(err) && identity.BindingKey != "" {
			return fmt.Errorf("%w: 资源 %s 的占位符 %s 已有凭据",
				core.ErrConflict, identity.OwnerUID, identity.BindingKey)
		}
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: 资源 %s 已有凭据", core.ErrConflict, identity.OwnerUID)
		}
		return err
	}
	if err := insertCredentialVersionTx(ctx, tx, version); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("创建凭据 %s: 提交: %w", identity.ID, err)
	}
	return nil
}

// AppendVersion 追加一个新版本并把旧活动版本置 `superseded`（同一事务）：
// 新版本号 = 当前值 + 1，身份的 `current_version`、`key_id` 与 `state` 随之前进。
//
// 重新录入（2026-10-23 判据裁决）：`revoked` / `retired` 身份可经本方法迁回
// `active`——这是一条**新版本**路径，旧版本行只改状态不动材料：被撤销的版本保持
// `revoked` 且密文仍为 NULL，永不复活；`revocation_epoch` 只增不减，重新录入
// **不**重置、**不**回退。写入新版本与身份迁回在同一事务内，失败即整体回滚、
// 身份保持撤销 / 退役状态。
// AppendVersion supersedes the active version and appends the next one, reactivating a
// revoked or retired identity in the same transaction. Revoked versions keep their state
// and stay ciphertext-free, and the revocation epoch is never reset.
func (s *CredentialStore) AppendVersion(ctx context.Context, credentialID, keyID, operationID string, version core.CredentialVersionRecord, at time.Time) error {
	if err := validateCredentialID(credentialID); err != nil {
		return err
	}
	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("追加凭据版本 %s: 开启事务: %w", credentialID, err)
	}
	defer func() { _ = tx.Rollback() }()

	identity, err := credentialByID(ctx, tx, credentialID)
	if err != nil {
		return err
	}
	if identity.State != core.CredentialIdentityActive &&
		!identity.State.CanTransitionTo(core.CredentialIdentityActive) {
		return fmt.Errorf("%w: 凭据 %s 状态为 %s，不能追加新版本",
			core.ErrCredentialUnavailable, credentialID, identity.State)
	}

	now := at
	if now.IsZero() {
		now = time.Now()
	}
	version.CredentialID = credentialID
	version.VersionNo = identity.CurrentVersion + 1
	version.State = core.CredentialVersionActive
	if version.KeyID == "" {
		version.KeyID = keyID
	}
	version.CreatedAt, version.UpdatedAt, version.OperationID = now.UnixMilli(), now.UnixMilli(), operationID
	version, err = normalizeCredentialVersion(version)
	if err != nil {
		return err
	}

	// 旧活动版本 → superseded：新解析不再选它，在途使用仍可解密。
	if _, err := tx.ExecContext(ctx,
		`UPDATE credential_versions SET state = 'superseded', updated_at = ?, operation_id = ?
		  WHERE credential_id = ? AND state = 'active'`,
		version.CreatedAt, operationID, credentialID); err != nil {
		return fmt.Errorf("追加凭据版本 %s: 替换旧版本: %w", credentialID, err)
	}
	if err := insertCredentialVersionTx(ctx, tx, version); err != nil {
		return err
	}
	// 身份迁回 active 与版本切换同事务完成；revocation_epoch 不在更新列表里——
	// 重新录入既不重置也不回退纪元。
	if _, err := tx.ExecContext(ctx,
		`UPDATE credentials
		    SET state = 'active', current_version = ?, key_id = ?, updated_at = ?,
		        operation_id = ?
		  WHERE credential_id = ?`,
		version.VersionNo, version.KeyID, version.CreatedAt, operationID, credentialID); err != nil {
		return fmt.Errorf("追加凭据版本 %s: 更新身份: %w", credentialID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("追加凭据版本 %s: 提交: %w", credentialID, err)
	}
	return nil
}

// Revoke 撤销整条凭据：身份 → `revoked`，**全部**版本 → `revoked` 且密文置 NULL，
// `revocation_epoch` +1。在途 Run 下一次检查即失败（不静默回退到 env 或新版本）。
// 已经是 revoked → 幂等返回（不重复加纪元）。
// Revoke revokes the identity and every version, blanks the ciphertexts and bumps the
// epoch; an already-revoked credential is a no-op.
func (s *CredentialStore) Revoke(ctx context.Context, credentialID, operationID string, at time.Time) error {
	if err := validateCredentialID(credentialID); err != nil {
		return err
	}
	if operationID == "" {
		return errors.New("撤销凭据: operation_id 不能为空")
	}
	if at.IsZero() {
		at = time.Now()
	}
	now := at.UnixMilli()

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("撤销凭据 %s: 开启事务: %w", credentialID, err)
	}
	defer func() { _ = tx.Rollback() }()

	identity, err := credentialByID(ctx, tx, credentialID)
	if err != nil {
		return err
	}
	if identity.State == core.CredentialIdentityRevoked {
		return nil
	}
	if !identity.State.CanTransitionTo(core.CredentialIdentityRevoked) {
		return fmt.Errorf("%w: 凭据 %s 不能从 %s 撤销", core.ErrCredentialUnavailable, credentialID, identity.State)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE credentials
		    SET state = 'revoked', revocation_epoch = revocation_epoch + 1,
		        updated_at = ?, operation_id = ?
		  WHERE credential_id = ?`,
		now, operationID, credentialID); err != nil {
		return fmt.Errorf("撤销凭据 %s: 更新身份: %w", credentialID, err)
	}
	// 版本行保留（历史要能回答"哪一版被撤销过"），只把可解密材料清掉。
	if _, err := tx.ExecContext(ctx,
		`UPDATE credential_versions
		    SET state = 'revoked', ciphertext = NULL, updated_at = ?, operation_id = ?
		  WHERE credential_id = ? AND state <> 'revoked'`,
		now, operationID, credentialID); err != nil {
		return fmt.Errorf("撤销凭据 %s: 更新版本: %w", credentialID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("撤销凭据 %s: 提交: %w", credentialID, err)
	}
	return nil
}

// Retire 让整条凭据退役（资源删除 / 模式切走）：身份与活动版本 → `retired`，
// 密文**保留**——在途 Run 可能还持有这个版本。已 revoked 的凭据不得被"退役"改写。
// Retire retires the identity and active version while keeping the ciphertext so
// in-flight runs can still finish; a revoked credential is never rewritten.
func (s *CredentialStore) Retire(ctx context.Context, credentialID, operationID string, at time.Time) error {
	if err := validateCredentialID(credentialID); err != nil {
		return err
	}
	if operationID == "" {
		return errors.New("退役凭据: operation_id 不能为空")
	}
	if at.IsZero() {
		at = time.Now()
	}
	now := at.UnixMilli()

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("退役凭据 %s: 开启事务: %w", credentialID, err)
	}
	defer func() { _ = tx.Rollback() }()

	identity, err := credentialByID(ctx, tx, credentialID)
	if err != nil {
		return err
	}
	if identity.State == core.CredentialIdentityRetired {
		return nil
	}
	if !identity.State.CanTransitionTo(core.CredentialIdentityRetired) {
		return fmt.Errorf("%w: 凭据 %s 状态为 %s，不能退役", core.ErrCredentialUnavailable, credentialID, identity.State)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE credentials SET state = 'retired', updated_at = ?, operation_id = ?
		  WHERE credential_id = ?`, now, operationID, credentialID); err != nil {
		return fmt.Errorf("退役凭据 %s: 更新身份: %w", credentialID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE credential_versions
		    SET state = 'retired', updated_at = ?, operation_id = ?
		  WHERE credential_id = ? AND state = 'active'`,
		now, operationID, credentialID); err != nil {
		return fmt.Errorf("退役凭据 %s: 更新版本: %w", credentialID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("退役凭据 %s: 提交: %w", credentialID, err)
	}
	return nil
}

// retireCredentialByOwnerTx 在**调用方的事务里**把某资源实例的凭据整条退役（资源删除，
// 模块卡 R4.8）：身份与活动版本 → `retired`，密文保留给在途 Run 用完。与墓碑同事务，
// 因此不存在"资源已删、凭据仍 active"的窗口。没有凭据（env 模式）是空操作；已
// `revoked` 的身份不被改写成 `retired`（撤销是终态，撤销纪元不回退）。
// retireCredentialByOwnerTx retires a deleted resource's credential inside the caller's
// transaction; env-mode resources have no row, and a revoked identity is never rewritten.
func retireCredentialByOwnerTx(ctx context.Context, tx *sql.Tx, ownerKind core.BindingKind,
	ownerUID core.ResourceUID, operationID string, at time.Time,
) error {
	now := at.UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`UPDATE credentials SET state = 'retired', updated_at = ?, operation_id = ?
		  WHERE owner_kind = ? AND owner_uid = ? AND state = 'active'`,
		now, operationID, string(ownerKind), string(ownerUID)); err != nil {
		return fmt.Errorf("退役资源 %s 的凭据身份: %w", ownerUID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE credential_versions
		    SET state = 'retired', updated_at = ?, operation_id = ?
		  WHERE state = 'active'
		    AND credential_id IN (SELECT credential_id FROM credentials
		                           WHERE owner_kind = ? AND owner_uid = ?)`,
		now, operationID, string(ownerKind), string(ownerUID)); err != nil {
		return fmt.Errorf("退役资源 %s 的凭据版本: %w", ownerUID, err)
	}
	return nil
}

// Credential 按 ID 读身份行；未命中 → `core.ErrResourceNotFound`。
// Credential reads the identity row by id.
func (s *CredentialStore) Credential(ctx context.Context, credentialID string) (core.CredentialRecord, error) {
	if err := validateCredentialID(credentialID); err != nil {
		return core.CredentialRecord{}, err
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+credentialColumns+` FROM credentials WHERE credential_id = ?`, credentialID)
	rec, err := scanCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.CredentialRecord{}, fmt.Errorf("%w: 凭据 %s", core.ErrResourceNotFound, credentialID)
	}
	if err != nil {
		return core.CredentialRecord{}, fmt.Errorf("读取凭据 %s: %w", credentialID, err)
	}
	return rec, nil
}

// CredentialByOwner 按资源实例读它**唯一**的凭据身份；未命中 → `core.ErrResourceNotFound`。
// CredentialByOwner reads the single credential identity of one resource instance.
func (s *CredentialStore) CredentialByOwner(ctx context.Context, kind core.BindingKind, ownerUID core.ResourceUID) (core.CredentialRecord, error) {
	if _, err := core.ParseBindingKind(string(kind)); err != nil {
		return core.CredentialRecord{}, err
	}
	uid, err := core.ParseResourceUID(string(ownerUID))
	if err != nil {
		return core.CredentialRecord{}, err
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+credentialColumns+` FROM credentials
		  WHERE owner_kind = ? AND owner_uid = ? AND binding_key = ''`,
		string(kind), string(uid))
	rec, err := scanCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.CredentialRecord{}, fmt.Errorf("%w: 资源 %s 没有凭据", core.ErrResourceNotFound, uid)
	}
	if err != nil {
		return core.CredentialRecord{}, fmt.Errorf("读取资源 %s 的凭据: %w", uid, err)
	}
	return rec, nil
}

// CredentialByOwnerKey 按 `(属主类别, 资源 UID, 绑定键)` 读一条凭据身份：MCP server 的
// 每个占位符各一条，其余类别等价于 `CredentialByOwner`（绑定键恒为空串）。未命中 →
// `core.ErrResourceNotFound`——调用方据此判定"该占位符还没有托管值"，**不得**借此
// 回落进程环境。
// CredentialByOwnerKey reads the identity of one (owner, binding key) pair; a miss means
// "no managed value yet" and must never be read as "fall back to env".
func (s *CredentialStore) CredentialByOwnerKey(ctx context.Context, kind core.BindingKind,
	ownerUID core.ResourceUID, bindingKey string,
) (core.CredentialRecord, error) {
	normalizedKind, err := normalizeCredentialOwnerKind(kind, bindingKey)
	if err != nil {
		return core.CredentialRecord{}, err
	}
	uid, err := core.ParseResourceUID(string(ownerUID))
	if err != nil {
		return core.CredentialRecord{}, err
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+credentialColumns+` FROM credentials
		  WHERE owner_kind = ? AND owner_uid = ? AND binding_key = ?`,
		string(normalizedKind), string(uid), strings.TrimSpace(bindingKey))
	rec, err := scanCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.CredentialRecord{}, fmt.Errorf("%w: 资源 %s 的绑定键 %s 没有凭据",
			core.ErrResourceNotFound, uid, strings.TrimSpace(bindingKey))
	}
	if err != nil {
		return core.CredentialRecord{}, fmt.Errorf("读取资源 %s 的凭据: %w", uid, err)
	}
	return rec, nil
}

// Version 按 `(credential_id, version_no)` 读一个版本；未命中 → `core.ErrResourceNotFound`。
// Version reads one version row.
func (s *CredentialStore) Version(ctx context.Context, credentialID string, versionNo int) (core.CredentialVersionRecord, error) {
	if err := validateCredentialID(credentialID); err != nil {
		return core.CredentialVersionRecord{}, err
	}
	if versionNo < 1 {
		return core.CredentialVersionRecord{}, errors.New("读取凭据版本: version_no 必须 >= 1")
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+credentialVersionColumns+` FROM credential_versions
		  WHERE credential_id = ? AND version_no = ?`, credentialID, versionNo)
	rec, err := scanCredentialVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.CredentialVersionRecord{}, fmt.Errorf("%w: 凭据 %s 版本 %d",
			core.ErrResourceNotFound, credentialID, versionNo)
	}
	if err != nil {
		return core.CredentialVersionRecord{}, fmt.Errorf("读取凭据 %s 版本 %d: %w", credentialID, versionNo, err)
	}
	return rec, nil
}

// Versions 按版本号升序读全部版本（审计 / 展示面用的是状态与尾号，不含密文）。
// Versions reads every version in ascending order.
func (s *CredentialStore) Versions(ctx context.Context, credentialID string) ([]core.CredentialVersionRecord, error) {
	if err := validateCredentialID(credentialID); err != nil {
		return nil, err
	}
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+credentialVersionColumns+` FROM credential_versions
		  WHERE credential_id = ? ORDER BY version_no ASC`, credentialID)
	if err != nil {
		return nil, fmt.Errorf("列出凭据 %s 版本: %w", credentialID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.CredentialVersionRecord
	for rows.Next() {
		rec, scanErr := scanCredentialVersion(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("扫描凭据 %s 版本: %w", credentialID, scanErr)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历凭据 %s 版本: %w", credentialID, err)
	}
	return out, nil
}

// ListActive 按凭据 ID 升序列出全部 `active` 身份：`secrets status` 的统计与
// `secrets revoke-managed` 的枚举都从这里取。只读，**不**带版本行与密文材料。
// 非 active 身份（已撤销 / 已退役）在枚举里自然缺席——它们本就不需要再撤销。
// ListActive lists every active identity in ascending order; revoked and retired ones
// are absent because revoking them again would be a no-op.
func (s *CredentialStore) ListActive(ctx context.Context) ([]core.CredentialRecord, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+credentialColumns+` FROM credentials WHERE state = ? ORDER BY credential_id ASC`,
		string(core.CredentialIdentityActive))
	if err != nil {
		return nil, fmt.Errorf("列出活动凭据身份: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.CredentialRecord
	for rows.Next() {
		rec, scanErr := scanCredential(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("扫描活动凭据身份: %w", scanErr)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历活动凭据身份: %w", err)
	}
	return out, nil
}

// RecordVersionCredential 记录"某发布版本发布时活动凭据是第几版"（**仅供审计**）。
// 同一 `(version_id, credential_id)` 已记录时保持首次结论：发布版本不可变，快照也不重写。
// 运行时授权**不读**这张表——Run 按准入固定的版本，通知按发送时当前 active 版本。
// RecordVersionCredential snapshots the active credential version for audit only; the
// first resolution wins and the table is never an authorization source.
func (s *CredentialStore) RecordVersionCredential(ctx context.Context, versionID int64, credentialID string, resolvedVersion int, at time.Time) error {
	if err := validateCredentialID(credentialID); err != nil {
		return err
	}
	if resolvedVersion < 1 {
		return errors.New("记录版本凭据快照: resolved_version 必须 >= 1")
	}
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := s.db.write.ExecContext(ctx,
		`INSERT INTO resource_version_credentials
		   (version_id, credential_id, resolved_version, resolved_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(version_id, credential_id) DO NOTHING`,
		versionID, credentialID, resolvedVersion, at.UnixMilli()); err != nil {
		return fmt.Errorf("记录发布版本 %d 的凭据快照: %w", versionID, err)
	}
	return nil
}

// VersionCredential 读发布版本的凭据快照；未记录 → `ok=false`。
// VersionCredential reads the audit snapshot of one published version.
func (s *CredentialStore) VersionCredential(ctx context.Context, versionID int64, credentialID string) (int, bool, error) {
	var resolved int
	err := s.db.read.QueryRowContext(ctx,
		`SELECT resolved_version FROM resource_version_credentials
		  WHERE version_id = ? AND credential_id = ?`, versionID, credentialID).Scan(&resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("读取发布版本 %d 的凭据快照: %w", versionID, err)
	}
	return resolved, true, nil
}

// credentialByID 在事务内（或读池上）按 ID 读身份行。
// credentialByID reads one identity row through the given executor.
func credentialByID(ctx context.Context, q queryer, credentialID string) (core.CredentialRecord, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+credentialColumns+` FROM credentials WHERE credential_id = ?`, credentialID)
	rec, err := scanCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.CredentialRecord{}, fmt.Errorf("%w: 凭据 %s", core.ErrResourceNotFound, credentialID)
	}
	if err != nil {
		return core.CredentialRecord{}, fmt.Errorf("读取凭据 %s: %w", credentialID, err)
	}
	return rec, nil
}

// insertCredentialTx 是身份行插入的唯一 SQL。
// insertCredentialTx is the single identity INSERT.
func insertCredentialTx(ctx context.Context, ex sqlExecutor, rec core.CredentialRecord) error {
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO credentials
		   (credential_id, owner_kind, owner_uid, binding_key, owner_name, state,
		    current_version, revocation_epoch, key_id, created_at, updated_at, operation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, string(rec.OwnerKind), string(rec.OwnerUID), rec.BindingKey, rec.OwnerName,
		string(rec.State), rec.CurrentVersion, rec.RevocationEpoch, rec.KeyID, rec.CreatedAt,
		rec.UpdatedAt, rec.OperationID); err != nil {
		return fmt.Errorf("写入凭据 %s: %w", rec.ID, err)
	}
	return nil
}

// insertCredentialVersionTx 是版本行插入的唯一 SQL。
// insertCredentialVersionTx is the single version INSERT.
func insertCredentialVersionTx(ctx context.Context, ex sqlExecutor, rec core.CredentialVersionRecord) error {
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO credential_versions
		   (credential_id, version_no, state, key_id, target_binding, last4, nonce,
		    ciphertext, created_at, updated_at, operation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.CredentialID, rec.VersionNo, string(rec.State), rec.KeyID, rec.TargetBinding,
		rec.Last4, rec.Nonce, rec.Ciphertext, rec.CreatedAt, rec.UpdatedAt, rec.OperationID); err != nil {
		return fmt.Errorf("写入凭据 %s 版本 %d: %w", rec.CredentialID, rec.VersionNo, err)
	}
	return nil
}

// scanCredential 按 credentialColumns 的列序扫描一行。
// scanCredential scans one identity row.
func scanCredential(row interface{ Scan(...any) error }) (core.CredentialRecord, error) {
	var (
		rec       core.CredentialRecord
		ownerKind string
		ownerUID  string
		state     string
	)
	err := row.Scan(&rec.ID, &ownerKind, &ownerUID, &rec.BindingKey, &rec.OwnerName, &state,
		&rec.CurrentVersion, &rec.RevocationEpoch, &rec.KeyID, &rec.CreatedAt,
		&rec.UpdatedAt, &rec.OperationID)
	if err != nil {
		return core.CredentialRecord{}, err
	}
	rec.OwnerKind = core.BindingKind(ownerKind)
	rec.OwnerUID = core.ResourceUID(ownerUID)
	rec.State = core.CredentialIdentityState(state)
	return rec, nil
}

// scanCredentialVersion 按 credentialVersionColumns 的列序扫描一行；撤销后密文为 NULL。
// scanCredentialVersion scans one version row; a revoke leaves the ciphertext NULL.
func scanCredentialVersion(row interface{ Scan(...any) error }) (core.CredentialVersionRecord, error) {
	var (
		rec         core.CredentialVersionRecord
		state       string
		cipherBytes []byte
	)
	err := row.Scan(&rec.CredentialID, &rec.VersionNo, &state, &rec.KeyID, &rec.TargetBinding,
		&rec.Last4, &rec.Nonce, &cipherBytes, &rec.CreatedAt, &rec.UpdatedAt, &rec.OperationID)
	if err != nil {
		return core.CredentialVersionRecord{}, err
	}
	rec.State = core.CredentialVersionState(state)
	rec.Ciphertext = cipherBytes
	return rec, nil
}

// normalizeCredentialPair 校验创建输入：ID / 属主 / 名称 / 密钥标识 / 操作号齐备，
// 版本号恰为 1 且状态 active，密文与 nonce 按 schema CHECK 的形状存在。
// normalizeCredentialPair validates the create input.
func normalizeCredentialPair(identity core.CredentialRecord, version core.CredentialVersionRecord, versionNo int) (core.CredentialRecord, core.CredentialVersionRecord, error) {
	if err := validateCredentialID(identity.ID); err != nil {
		return core.CredentialRecord{}, core.CredentialVersionRecord{}, err
	}
	ownerKind, err := normalizeCredentialOwnerKind(identity.OwnerKind, identity.BindingKey)
	if err != nil {
		return core.CredentialRecord{}, core.CredentialVersionRecord{}, err
	}
	identity.OwnerKind = ownerKind
	ownerUID, err := core.ParseResourceUID(string(identity.OwnerUID))
	if err != nil {
		return core.CredentialRecord{}, core.CredentialVersionRecord{}, err
	}
	identity.OwnerUID = ownerUID
	if identity.OwnerName == "" {
		return core.CredentialRecord{}, core.CredentialVersionRecord{}, errors.New("凭据: 属主名称不得为空")
	}
	if identity.KeyID == "" {
		return core.CredentialRecord{}, core.CredentialVersionRecord{}, errors.New("凭据: key_id 不得为空")
	}
	if identity.OperationID == "" {
		return core.CredentialRecord{}, core.CredentialVersionRecord{}, errors.New("凭据: operation_id 不能为空")
	}
	if identity.State == "" {
		identity.State = core.CredentialIdentityActive
	}
	if identity.State != core.CredentialIdentityActive {
		return core.CredentialRecord{}, core.CredentialVersionRecord{}, errors.New("凭据: 新凭据必须是 active")
	}
	identity.CurrentVersion = versionNo
	if identity.CreatedAt == 0 {
		identity.CreatedAt = time.Now().UnixMilli()
	}
	if identity.UpdatedAt == 0 {
		identity.UpdatedAt = identity.CreatedAt
	}

	version.CredentialID = identity.ID
	version.VersionNo = versionNo
	if version.State == "" {
		version.State = core.CredentialVersionActive
	}
	if version.State != core.CredentialVersionActive {
		return core.CredentialRecord{}, core.CredentialVersionRecord{}, errors.New("凭据: 新版本必须是 active")
	}
	if version.KeyID == "" {
		version.KeyID = identity.KeyID
	}
	if version.CreatedAt == 0 {
		version.CreatedAt = identity.CreatedAt
	}
	if version.UpdatedAt == 0 {
		version.UpdatedAt = version.CreatedAt
	}
	if version.OperationID == "" {
		version.OperationID = identity.OperationID
	}
	version, err = normalizeCredentialVersion(version)
	if err != nil {
		return core.CredentialRecord{}, core.CredentialVersionRecord{}, err
	}
	return identity, version, nil
}

// normalizeCredentialOwnerKind 校验属主类别与绑定键的组合（与 schema 的 CHECK 同口径）：
// `mcp_server` 必须有合法槽位名（env 键名或头名）作绑定键（一个 server 的多个槽位各自独立），其余
// 属主类别必须为空串（"一个资源实例至多一条凭据"的语义因此原样保留）。两者错配一律
// 在写库前拒绝，错误不回显绑定键原值——它可能是误贴进来的凭证。
// normalizeCredentialOwnerKind enforces the owner-kind/binding-key pairing before the
// schema CHECK sees it; the error never echoes the raw key, which may be a credential.
func normalizeCredentialOwnerKind(kind core.BindingKind, bindingKey string) (core.BindingKind, error) {
	if !core.IsCredentialOwnerKind(kind) {
		return "", fmt.Errorf("%w: 未知的凭据属主类别 %q", core.ErrCredentialOwnerMismatch, kind)
	}
	key := strings.TrimSpace(bindingKey)
	if kind == core.BindingMCPServer {
		if _, ok := core.MCPBindingKey(key); !ok {
			return "", fmt.Errorf("%w: mcp_server 凭据必须带合法槽位名（env 键名或头名）作 binding_key",
				core.ErrCredentialOwnerMismatch)
		}
		return kind, nil
	}
	if key != "" {
		return "", fmt.Errorf("%w: %s 凭据不得带 binding_key（一个实例至多一条凭据）",
			core.ErrCredentialOwnerMismatch, kind)
	}
	return kind, nil
}

// normalizeCredentialVersion 校验版本行的形状：活动 / 在途版本必须有 12 字节 nonce 与
// 非空密文；撤销版本不得带密文（schema CHECK 同口径，写库前先拒）。
// normalizeCredentialVersion validates the version row shape before the CHECK sees it.
func normalizeCredentialVersion(version core.CredentialVersionRecord) (core.CredentialVersionRecord, error) {
	if version.VersionNo < 1 {
		return core.CredentialVersionRecord{}, errors.New("凭据版本: version_no 必须 >= 1")
	}
	if version.KeyID == "" {
		return core.CredentialVersionRecord{}, errors.New("凭据版本: key_id 不得为空")
	}
	if version.TargetBinding == "" {
		return core.CredentialVersionRecord{}, errors.New("凭据版本: target_binding 不得为空")
	}
	if version.OperationID == "" {
		return core.CredentialVersionRecord{}, errors.New("凭据版本: operation_id 不能为空")
	}
	if version.State == core.CredentialVersionRevoked {
		if len(version.Ciphertext) != 0 {
			return core.CredentialVersionRecord{}, errors.New("凭据版本: revoked 版本不得保留密文")
		}
		return version, nil
	}
	if len(version.Nonce) != 12 {
		return core.CredentialVersionRecord{}, fmt.Errorf("凭据版本: nonce 必须为 12 字节，实际 %d", len(version.Nonce))
	}
	if len(version.Ciphertext) == 0 {
		return core.CredentialVersionRecord{}, errors.New("凭据版本: 非 revoked 版本必须带密文")
	}
	return version, nil
}

// validateCredentialID 校验 `cred_` 前缀与 Crockford 载荷（领域函数收口格式）。
// validateCredentialID checks the cred_ prefix and Crockford payload.
func validateCredentialID(credentialID string) error {
	return core.ValidateCredentialID(credentialID)
}

// isUniqueViolation 判定驱动返回的唯一键冲突：modernc 的文案含 `UNIQUE constraint failed`。
// 文案只用于**分类**，不用于构造业务结论（调用方按语义另行判定归属）。
// isUniqueViolation classifies a unique-constraint failure; wording only picks the branch.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
