package storage

// 本文件是 `notify_channels` 的**管理写面**（颗粒度文档 §3.1.6 / 约束 38）：
// Create / Update / Delete / Get。发送路径的只读仓储在 notify_store.go（US-8），
// 两者读写同一张表——写后下一发即可见，零缓存、零失效策略。
//
// 身份纪律（US-12 R4）：v3 起主键是 `resource_uid`，名称只是**当前标签**。
//   - 所有写入先按名称解析**活动行**（`deleted_at IS NULL`，由 `ux_notify_channels_active_name`
//     保证唯一），再用解析出的 UID 定位——直接 `WHERE name = ?` 会命中同名墓碑行，
//     把删除过的渠道"复活"或改写历史；
//   - 删除是**墓碑**（`deleted_at` + 身份行 `state='deleted'`），行保留供历史查询与审计；
//   - 调用方带 `ResourceUID` 时它是**条件**：与活动行不一致即拒绝（条件写语义），
//     存储层不得默默按名称改到另一代实例上。
//
// 本文件只做 SQL 与行映射：`${ENV_VAR}` 占位符语法、域名白名单、类型表、引用扫描
// 与操作行（`notify_channel.*`）都归 app.NotifyAdmin 编排（T056）；存储层不知道
// 占位符语义，也不替调用方决定"空 URL = 暂不可发送"——那一列按参数原样落库。
// This file is the admin write face of notify_channels. The primary key is the resource
// uid; the name only resolves the current active row, and a deletion leaves a tombstone
// instead of removing history.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// notifyChannelColumns 是渠道行的读取列序（管理写面与发送只读面共用同一份，
// 不写第二处列定义；scanNotifyChannel 按同一顺序扫描）。
// notifyChannelColumns is the single column order shared by the admin and read faces.
const notifyChannelColumns = `resource_uid, name, type, url, url_host, credential_mode,
	scope, owner_agent_uid, credential_id, revision, description, created_at, updated_at,
	deleted_at, operation_id`

// NotifyAdminStore 是通知渠道的管理写仓储。
// NotifyAdminStore is the admin write repository of notification channels.
type NotifyAdminStore struct{ db *DB }

// NewNotifyAdminStore 绑定一个已打开的写模式库句柄。
// NewNotifyAdminStore binds the store to an opened writable database handle.
func NewNotifyAdminStore(db *DB) *NotifyAdminStore { return &NotifyAdminStore{db: db} }

// Create 插入一条渠道：同名**活动**已存在 → `core.ErrConflict`（app 层按约束 38 映射
// 400 BAD_REQUEST，不新增 CONFLICT 错误码），既有行**原样保留**、不被覆盖。
// `url` 允许空串（渠道暂不可发送）；`created_at` / `updated_at` 取写入时刻。
// 身份行（`resource_identities`）与渠道行**同事务**写入：UID 一旦对外出现，
// 两个面必须同时可见，否则历史检索与身份校验会各缺一半。
// Create inserts the channel and its identity row in one transaction; a duplicate
// active name is a conflict and never overwrites.
func (s *NotifyAdminStore) Create(ctx context.Context, ch core.NotifyChannel) error {
	ch, err := normalizeNotifyChannel(ch, "创建")
	if err != nil {
		return err
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("创建通知渠道 %s: 开启事务: %w", ch.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	exists, err := activeNotifyChannelExists(ctx, tx, ch.Name)
	if err != nil {
		return fmt.Errorf("创建通知渠道 %s: %w", ch.Name, err)
	}
	if exists {
		return fmt.Errorf("%w: 通知渠道 %s 已存在", core.ErrConflict, ch.Name)
	}

	now := time.Now().UnixMilli()
	// 身份行只经 insertIdentityTx 写入：身份表的 INSERT 全仓一处，不给第二份同义 SQL。
	// Identity rows go through the single shared INSERT in identity_store.go.
	if err := insertIdentityTx(ctx, tx, core.ResourceIdentity{
		UID:         ch.ResourceUID,
		Kind:        core.ResourceNotifyChannel,
		Name:        ch.Name,
		State:       core.IdentityActive,
		CreatedAt:   now,
		OperationID: ch.OperationID,
	}); err != nil {
		return fmt.Errorf("创建通知渠道 %s: 写入资源身份: %w", ch.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO notify_channels
		   (resource_uid, name, type, url, url_host, credential_mode, scope, owner_agent_uid,
		    credential_id, revision, description, created_at, updated_at, deleted_at, operation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, NULL, ?)`,
		string(ch.ResourceUID), ch.Name, ch.Type, ch.URL, ch.URLHost, string(ch.CredentialMode),
		string(ch.Scope), nullIfEmpty(string(ch.OwnerAgentUID)), nullIfEmpty(ch.CredentialID),
		ch.Description, now, now, ch.OperationID); err != nil {
		// 唯一键冲突的驱动文案随版本变化，不做字符串匹配：重查活动行来判定。
		// The driver's constraint wording is not stable; re-check the active row instead.
		if dup, lookupErr := activeNotifyChannelExists(ctx, tx, ch.Name); lookupErr == nil && dup {
			return fmt.Errorf("%w: 通知渠道 %s 已存在", core.ErrConflict, ch.Name)
		}
		return fmt.Errorf("创建通知渠道 %s: %w", ch.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("创建通知渠道 %s: 提交: %w", ch.Name, err)
	}
	return nil
}

// Update 更新一条渠道：按名称解析**活动行**后按 UID 改值（主键已不是名称）；
// `keepURL=true` 时**不动**地址相关列（契约 NotifyChannelUpsert：更新留空 = 保留原值，
// 由 app 层翻译成这个开关）。地址 / 凭据变更使 `revision` +1（参与资源 ETag）；
// 只改类型与说明不递增。活动行不存在 → `core.ErrResourceNotFound`。
// 调用方带 `ResourceUID` 时它与活动行不一致 → `core.ErrResourceUIDMismatch`（零写入）。
// Update resolves the active row by name and writes by uid; a caller-supplied uid acts
// as a precondition, and an address change bumps the revision.
func (s *NotifyAdminStore) Update(ctx context.Context, ch core.NotifyChannel, keepURL bool) error {
	ch, err := normalizeNotifyChannel(ch, "更新")
	if err != nil {
		return err
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("更新通知渠道 %s: 开启事务: %w", ch.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	active, err := activeNotifyChannelUID(ctx, tx, ch.Name)
	if err != nil {
		return fmt.Errorf("更新通知渠道 %s: %w", ch.Name, err)
	}
	if ch.ResourceUID != "" && ch.ResourceUID != active {
		return fmt.Errorf("%w: 通知渠道 %s 当前指向 %s，不能更新 %s",
			core.ErrResourceUIDMismatch, ch.Name, active, ch.ResourceUID)
	}

	query := `UPDATE notify_channels
	             SET type = ?, description = ?, updated_at = ?, operation_id = ?
	           WHERE resource_uid = ? AND deleted_at IS NULL`
	args := []any{ch.Type, ch.Description, time.Now().UnixMilli(), ch.OperationID, string(active)}
	if !keepURL {
		query = `UPDATE notify_channels
		            SET type = ?, description = ?, url = ?, url_host = ?, credential_mode = ?,
		                credential_id = ?, revision = revision + 1,
		                updated_at = ?, operation_id = ?
		          WHERE resource_uid = ? AND deleted_at IS NULL`
		args = []any{
			ch.Type, ch.Description, ch.URL, ch.URLHost, string(ch.CredentialMode),
			nullIfEmpty(ch.CredentialID), time.Now().UnixMilli(), ch.OperationID, string(active),
		}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("更新通知渠道 %s: %w", ch.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("更新通知渠道 %s: 提交: %w", ch.Name, err)
	}
	return nil
}

// UpdateCredentialColumns 只改凭据列并推进 revision：类型、说明、范围一律不动；
// 地址列只在切到 managed 时清空（managed 行上的完整地址在密文里，行上不得留明文），
// 切回 env 时保留调用方刚写下的 `${VAR}` 占位。
// 凭据动作（写新版本 / 退役 / 撤销）发生在调用方自己的地址写入**之后**，地址列是那次
// 写入的结果，不能被凭据动作读到的旧快照覆盖；同时 `credential_id` 只能由这里写入——
// 新身份是在凭据动作内部铸造的，调用方写入时还拿不到它。
// UpdateCredentialColumns touches only the credential columns and the address column it
// must clear for managed rows; the rest of the row belongs to the caller's own write.
func (s *NotifyAdminStore) UpdateCredentialColumns(ctx context.Context, uid core.ResourceUID,
	mode core.CredentialMode, credentialID, operationID string,
) error {
	if operationID == "" {
		return errors.New("更新通知渠道凭据列: operation_id 不能为空")
	}
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return fmt.Errorf("更新通知渠道凭据列: %w", err)
	}
	if _, err := core.ParseCredentialMode(string(mode)); err != nil {
		return fmt.Errorf("更新通知渠道凭据列: %w", err)
	}
	if mode == core.CredentialModeEnv && credentialID != "" {
		return errors.New("更新通知渠道凭据列: env 模式不得带凭据身份")
	}
	result, err := s.db.write.ExecContext(ctx,
		`UPDATE notify_channels
		    SET credential_mode = ?, credential_id = ?,
		        url = CASE WHEN ? = 'managed' THEN '' ELSE url END,
		        revision = revision + 1, updated_at = ?, operation_id = ?
		  WHERE resource_uid = ? AND deleted_at IS NULL`,
		string(mode), nullIfEmpty(credentialID), string(mode),
		time.Now().UnixMilli(), operationID, string(canonical))
	if err != nil {
		return fmt.Errorf("更新通知渠道 %s 的凭据列: %w", canonical, err)
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows == 0 {
		// 行在读到写之间被删除：凭据动作不得复活墓碑，按 404 结束。
		return fmt.Errorf("更新通知渠道 %s 的凭据列: %w", canonical, core.ErrResourceNotFound)
	}
	return nil
}

// Delete 给渠道打墓碑（引用闸门与 `?force=true` 由 app 层在调用前判定）：
// `notify_channels.deleted_at` 与身份行 `state='deleted'` 同事务写入，行本身保留——
// 已发出的通知与审计里出现的渠道名必须还能查到它当时是谁。
// 活动行不存在 → `core.ErrResourceNotFound`（删除重试的 404 语义）；重复删除幂等。
// Delete tombstones the channel and its identity row; repeating it is idempotent.
func (s *NotifyAdminStore) Delete(ctx context.Context, name, operationID string) error {
	if name == "" {
		return errors.New("删除通知渠道: 名称不得为空")
	}
	if operationID == "" {
		return errors.New("删除通知渠道: operation_id 不能为空")
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("删除通知渠道 %s: 开启事务: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	uid, err := activeNotifyChannelUID(ctx, tx, name)
	if errors.Is(err, core.ErrResourceNotFound) {
		return fmt.Errorf("%w: 通知渠道 %s", core.ErrResourceNotFound, name)
	}
	if err != nil {
		return fmt.Errorf("删除通知渠道 %s: %w", name, err)
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`UPDATE notify_channels
		    SET deleted_at = ?, updated_at = ?, operation_id = ?
		  WHERE resource_uid = ? AND deleted_at IS NULL`,
		now, now, operationID, string(uid)); err != nil {
		return fmt.Errorf("删除通知渠道 %s: 写墓碑: %w", name, err)
	}
	if missing, err := tombstoneIdentityTx(ctx, tx, uid, operationID, now); err != nil {
		return fmt.Errorf("删除通知渠道 %s: 更新资源身份: %w", name, err)
	} else if missing {
		// 渠道行存在而身份行缺失 = schema 被绕过；拒绝提交，不留下"删了渠道却查不到身份"的库。
		return fmt.Errorf("删除通知渠道 %s: %w", name, core.ErrResourceNotFound)
	}
	// 删除与凭据退役同事务（R4.8）：通知渠道可能持有托管凭据，下一次发送据此失败关闭。
	if err := retireCredentialByOwnerTx(ctx, tx, core.BindingNotifyChannel, uid, operationID,
		time.UnixMilli(now)); err != nil {
		return fmt.Errorf("删除通知渠道 %s: 退役凭据: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("删除通知渠道 %s: 提交: %w", name, err)
	}
	return nil
}

// Get 按名称读一条**活动**渠道（写后重新读取的投影面）；未命中 → `core.ErrResourceNotFound`。
// 可空的说明 / 属主 / 凭据列读成空串（契约视图不出现 null）。
// Get reads one active channel by name; a miss maps to ErrResourceNotFound.
func (s *NotifyAdminStore) Get(ctx context.Context, name string) (core.NotifyChannel, error) {
	if name == "" {
		return core.NotifyChannel{}, errors.New("读取通知渠道: 名称不得为空")
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+notifyChannelColumns+` FROM notify_channels
		  WHERE name = ? AND deleted_at IS NULL`, name)
	ch, err := scanNotifyChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.NotifyChannel{}, fmt.Errorf("%w: 通知渠道 %s", core.ErrResourceNotFound, name)
	}
	if err != nil {
		return core.NotifyChannel{}, fmt.Errorf("读取通知渠道 %s: %w", name, err)
	}
	return ch, nil
}

// GetByUID 按资源身份读渠道（含墓碑行）——凭据与范围动作按 UID 寻址，
// 不经过名称猜测；未命中 → `core.ErrResourceNotFound`。
// GetByUID reads the channel by identity, tombstones included.
func (s *NotifyAdminStore) GetByUID(ctx context.Context, uid core.ResourceUID) (core.NotifyChannel, error) {
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return core.NotifyChannel{}, err
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+notifyChannelColumns+` FROM notify_channels WHERE resource_uid = ?`, string(canonical))
	ch, err := scanNotifyChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.NotifyChannel{}, fmt.Errorf("%w: 通知渠道 %s", core.ErrResourceNotFound, canonical)
	}
	if err != nil {
		return core.NotifyChannel{}, fmt.Errorf("读取通知渠道 %s: %w", canonical, err)
	}
	return ch, nil
}

// scanNotifyChannel 按 notifyChannelColumns 的列序扫描一行；NULL 映射为空串 / nil，
// 不用零值当哨兵（`owner_agent_uid IS NULL` 与"空属主"是同一事实，但说明列不是）。
// scanNotifyChannel scans one channel row, mapping NULLs to empty values.
func scanNotifyChannel(row interface{ Scan(...any) error }) (core.NotifyChannel, error) {
	var (
		ch          core.NotifyChannel
		uid         string
		urlHost     string
		mode        string
		scope       string
		owner       sql.NullString
		credential  sql.NullString
		description sql.NullString
		deletedAt   sql.NullInt64
	)
	err := row.Scan(&uid, &ch.Name, &ch.Type, &ch.URL, &urlHost, &mode, &scope, &owner,
		&credential, &ch.Revision, &description, &ch.CreatedAt, &ch.UpdatedAt, &deletedAt,
		&ch.OperationID)
	if err != nil {
		return core.NotifyChannel{}, err
	}
	ch.ResourceUID = core.ResourceUID(uid)
	ch.URLHost = urlHost
	ch.CredentialMode = core.CredentialMode(mode)
	ch.Scope = core.ResourceScope(scope)
	ch.OwnerAgentUID = core.AgentUID(owner.String)
	ch.CredentialID = credential.String
	ch.Description = description.String
	if deletedAt.Valid {
		value := deletedAt.Int64
		ch.DeletedAt = &value
	}
	return ch, nil
}

// normalizeNotifyChannel 校验并规范化写入形状：名称 / 类型非空、UID 合法（创建必带，
// 更新可空表示"按活动行"）、操作号非空（管理写先留痕）、范围与属主自洽、凭据模式与
// 凭据身份自洽。语法级判据（占位符、域名、类型白名单）仍在 app / web 层。
// normalizeNotifyChannel validates the write shape shared by Create and Update.
func normalizeNotifyChannel(ch core.NotifyChannel, action string) (core.NotifyChannel, error) {
	if ch.Name == "" {
		return core.NotifyChannel{}, fmt.Errorf("%s通知渠道: 名称不得为空", action)
	}
	if ch.Type == "" {
		return core.NotifyChannel{}, fmt.Errorf("%s通知渠道 %s: 类型不得为空", action, ch.Name)
	}
	if ch.OperationID == "" {
		return core.NotifyChannel{}, fmt.Errorf("%s通知渠道 %s: operation_id 不能为空", action, ch.Name)
	}
	if ch.ResourceUID != "" {
		canonical, err := core.ParseResourceUID(string(ch.ResourceUID))
		if err != nil {
			return core.NotifyChannel{}, fmt.Errorf("%s通知渠道 %s: %w", action, ch.Name, err)
		}
		ch.ResourceUID = canonical
	} else if action == "创建" {
		return core.NotifyChannel{}, fmt.Errorf("%s通知渠道 %s: 资源 UID 不能为空", action, ch.Name)
	}
	if ch.CredentialMode == "" {
		ch.CredentialMode = core.CredentialModeEnv
	}
	if _, err := core.ParseCredentialMode(string(ch.CredentialMode)); err != nil {
		return core.NotifyChannel{}, fmt.Errorf("%s通知渠道 %s: %w", action, ch.Name, err)
	}
	if ch.Scope == "" {
		ch.Scope = core.ScopeShared
	}
	switch ch.Scope {
	case core.ScopeShared:
		if ch.OwnerAgentUID != "" {
			return core.NotifyChannel{}, fmt.Errorf("%s通知渠道 %s: 共享范围不得带专属属主", action, ch.Name)
		}
	case core.ScopeExclusive:
		if _, err := core.ParseAgentUID(string(ch.OwnerAgentUID)); err != nil {
			return core.NotifyChannel{}, fmt.Errorf("%s通知渠道 %s: 专属范围必须带合法属主: %w",
				action, ch.Name, err)
		}
	default:
		return core.NotifyChannel{}, fmt.Errorf("%s通知渠道 %s: 未知使用范围 %q", action, ch.Name, ch.Scope)
	}
	switch ch.CredentialMode {
	case core.CredentialModeEnv:
		if ch.CredentialID != "" {
			return core.NotifyChannel{}, fmt.Errorf("%s通知渠道 %s: env 模式不得带凭据身份", action, ch.Name)
		}
	case core.CredentialModeManaged:
		if ch.URL != "" {
			return core.NotifyChannel{}, fmt.Errorf("%s通知渠道 %s: managed 模式的 url 必须为空（地址在密文里）",
				action, ch.Name)
		}
	}
	return ch, nil
}

// activeNotifyChannelUID 解析名称当前指向的活动渠道 UID；没有活动行 → ErrResourceNotFound。
// 墓碑行不参与解析——这正是"按名称直接 UPDATE 会命中同名墓碑行"的防线。
// activeNotifyChannelUID resolves the uid of the active row; tombstones never match.
func activeNotifyChannelUID(ctx context.Context, q queryer, name string) (core.ResourceUID, error) {
	var uid string
	err := q.QueryRowContext(ctx,
		`SELECT resource_uid FROM notify_channels WHERE name = ? AND deleted_at IS NULL`, name).
		Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrResourceNotFound
	}
	if err != nil {
		return "", err
	}
	return core.ResourceUID(uid), nil
}

// activeNotifyChannelExists 是唯一键冲突与创建前查重共用的存在性判据（只看活动行）。
// activeNotifyChannelExists reports whether an active channel with the name exists.
func activeNotifyChannelExists(ctx context.Context, q queryer, name string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM notify_channels WHERE name = ? AND deleted_at IS NULL`, name).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}
