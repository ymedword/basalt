package postgres

// 本文件是 `callers.KeyStore` 的 PostgreSQL 实现（US-16 §3.1.5 / C-07 / C-32）：
// Key 认证查表、生命周期写入、调用方行管理与周期用量聚合都落在 `callers` /
// `caller_keys` / `caller_limits` / `caller_usage_periods` / `quota_reservations`
// 五张表上。三条口径：
//
//  1. **查表只按指纹**——`Lookup` 的参数是 SHA-256 十六进制摘要，明文既进不了
//     SQL 参数，也进不了错误；调用方停用（`callers.status='disabled'`）按「未找到」
//     处理。
//  2. **轮换单事务**——新建、吊销、按需再绑定在同一事务内完成，不保留双活窗口；
//     未能再绑定的任务逐条报出（`scope_missing` / `caller_mismatch`）。
//  3. **词表映射只发生在边界**——`callers.kind` 的 `service` 投影成契约的
//     `external`，`text[]` 经 `array_to_string` 读成逗号串再在 Go 侧切分
//     （pgx stdlib 对未知 OID 的扫描目标是 string，不是 []string）。
//
// This file implements callers.KeyStore on PostgreSQL: fingerprint-only lookups,
// single-transaction rotation, and vocabulary mapping confined to this boundary.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// uniqueViolationSQLState 是 PostgreSQL 唯一约束冲突的 SQLSTATE。
// uniqueViolationSQLState is the PostgreSQL unique-violation SQLSTATE.
const uniqueViolationSQLState = "23505"

// errKeyStoreUnconfigured 是装配错误：没有数据库句柄时拒绝服务。
// errKeyStoreUnconfigured fails closed when the handle is missing.
var errKeyStoreUnconfigured = errors.New("postgres: caller key store has no database handle")

// errKeyInvalidArgument 表示参数不合法（空标识、非法状态或账本前缀无法解析）。
// errKeyInvalidArgument rejects malformed identities and states.
var errKeyInvalidArgument = errors.New("postgres: invalid caller key argument")

// keyStore 是 `callers.KeyStore` 的 PG 实现；对外只保证满足接口。
// keyStore implements callers.KeyStore; only the interface is guaranteed.
type keyStore struct {
	db *DB
}

// 编译期断言：实现满足消费方接口。
// The implementation satisfies the consumer-side interface.
var _ callers.KeyStore = (*keyStore)(nil)

// NewKeyStore 构造调用方 / Key 存储；返回类型不导出，调用方按接口使用。
// NewKeyStore builds the store; the concrete type stays unexported.
func NewKeyStore(db *DB) *keyStore {
	return &keyStore{db: db}
}

// keyRowColumns 是 Key 行投影的固定列序（两个查询共用同一扫描顺序）。
// keyRowColumns fixes the scan order shared by the key queries.
// 列名一律带 `k.` 前缀：两个查询都与 `callers` 联表，而 `caller_id` 在两侧同名，
// 不加限定会被 PostgreSQL 判成 ambiguous（SQLSTATE 42702）——只在真库上暴露。
// Every column is qualified because caller_id exists on both joined tables.
const keyRowColumns = `
	k.key_id, k.caller_id, k.key_hash, k.key_prefix,
	array_to_string(k.scopes, ',') AS scopes, k.expires_at, k.revoked_at, k.created_at`

// Lookup 按指纹返回 Key 记录：调用方已停用时按「未找到」拒绝；状态由
// `revoked_at` / `expires_at` 与当前时刻投影，存储层**不**替认证层放行。
// Lookup resolves a key by fingerprint and projects its status; a disabled caller is
// reported as not found.
func (s *keyStore) Lookup(ctx context.Context, fingerprint string) (callers.KeyRecord, error) {
	if s == nil || s.db == nil {
		return callers.KeyRecord{}, errKeyStoreUnconfigured
	}
	if strings.TrimSpace(fingerprint) == "" {
		return callers.KeyRecord{}, errKeyInvalidArgument
	}
	row := s.db.pool.QueryRowContext(ctx, `
		SELECT `+keyRowColumns+`
		FROM caller_keys AS k
		JOIN callers AS c ON c.caller_id = k.caller_id
		WHERE k.key_hash = $1 AND c.status = 'active'`, fingerprint)
	record, err := s.scanKeyRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return callers.KeyRecord{}, fmt.Errorf("%w: 按指纹未找到可用 Key", callers.ErrNotFound)
	}
	if err != nil {
		return callers.KeyRecord{}, fmt.Errorf("查询调用方 Key: %w", err)
	}
	return record, nil
}

// LookupByID 按 `(caller_id, key_id)` 返回 Key 记录；行不存在、归属不匹配或调用方
// 停用一律 `ErrNotFound`（调用方不得据错误区分）。
// LookupByID resolves a key by owner and id; every miss is ErrNotFound.
func (s *keyStore) LookupByID(ctx context.Context, callerID, keyID string) (callers.KeyRecord, error) {
	if s == nil || s.db == nil {
		return callers.KeyRecord{}, errKeyStoreUnconfigured
	}
	if strings.TrimSpace(callerID) == "" || strings.TrimSpace(keyID) == "" {
		return callers.KeyRecord{}, fmt.Errorf("%w: 调用方或 Key 标识为空", callers.ErrNotFound)
	}
	row := s.db.pool.QueryRowContext(ctx, `
		SELECT `+keyRowColumns+`
		FROM caller_keys AS k
		JOIN callers AS c ON c.caller_id = k.caller_id
		WHERE k.key_id = $1 AND k.caller_id = $2 AND c.status = 'active'`, keyID, callerID)
	record, err := s.scanKeyRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return callers.KeyRecord{}, fmt.Errorf("%w: 调用方 %s 没有 Key %s", callers.ErrNotFound, callerID, keyID)
	}
	if err != nil {
		return callers.KeyRecord{}, fmt.Errorf("查询调用方 Key: %w", err)
	}
	return record, nil
}

// scanKeyRow 把一行 Key 投影成领域记录；状态按吊销优先、过期其次的顺序判定。
// scanKeyRow projects the row and derives the status.
func (s *keyStore) scanKeyRow(row *sql.Row) (callers.KeyRecord, error) {
	var (
		record    callers.KeyRecord
		scopes    string
		expiresAt sql.NullInt64
		revokedAt sql.NullInt64
		createdAt int64
	)
	if err := row.Scan(&record.KeyID, &record.CallerID, &record.KeyHash, &record.KeyPrefix,
		&scopes, &expiresAt, &revokedAt, &createdAt); err != nil {
		return callers.KeyRecord{}, err
	}
	record.Scopes = splitScopes(scopes)
	record.ExpiresAt = millisPtr(expiresAt)
	record.RevokedAt = millisPtr(revokedAt)
	record.CreatedAt = time.UnixMilli(createdAt).UTC()
	record.Status = keyStatus(record.RevokedAt, record.ExpiresAt, s.db.clock())
	return record, nil
}

// splitScopes 把 `array_to_string` 的逗号串还原成切片；空串给空切片（非 nil）。
// splitScopes restores the scope slice from the comma-joined text.
func splitScopes(joined string) []string {
	if joined == "" {
		return []string{}
	}
	return strings.Split(joined, ",")
}

// millisPtr 把可空毫秒列转成 `*time.Time`（零值列返回 nil）。
// millisPtr converts a nullable millisecond column into a time pointer.
func millisPtr(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	at := time.UnixMilli(value.Int64).UTC()
	return &at
}

// keyStatus 按吊销优先、过期其次的顺序投影状态。
// keyStatus prefers revoked over expired, both over active.
func keyStatus(revokedAt, expiresAt *time.Time, now time.Time) callers.KeyStatus {
	switch {
	case revokedAt != nil:
		return callers.KeyStatusRevoked
	case expiresAt != nil && !expiresAt.After(now):
		return callers.KeyStatusExpired
	default:
		return callers.KeyStatusActive
	}
}

// Create 写入一条新 Key 行：主键或指纹冲突（`23505`）按 `core.ErrConflict` 返回，
// 便于 web 层映射 409 而无需认识驱动错误。
// Create inserts a key row; a unique violation becomes core.ErrConflict.
func (s *keyStore) Create(ctx context.Context, record callers.KeyRecord) error {
	if s == nil || s.db == nil {
		return errKeyStoreUnconfigured
	}
	if record.KeyID == "" || record.CallerID == "" || record.KeyHash == "" || record.KeyPrefix == "" {
		return fmt.Errorf("%w: Key 行缺少标识、归属、指纹或前缀", errKeyInvalidArgument)
	}
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = s.db.clock()
	}
	_, err := s.db.pool.ExecContext(ctx, `
		INSERT INTO caller_keys (key_id, caller_id, key_hash, key_prefix, scopes, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		record.KeyID, record.CallerID, record.KeyHash, record.KeyPrefix,
		append([]string{}, record.Scopes...), expiresAtColumn(record.ExpiresAt), createdAt.UnixMilli(),
	)
	if err != nil {
		if isPGUniqueViolation(err) {
			return fmt.Errorf("%w: Key 标识或指纹已存在", core.ErrConflict)
		}
		return fmt.Errorf("写入调用方 Key: %w", err)
	}
	return nil
}

// SetStatus 把 Key 置为给定状态：本实现只表示吊销，`revoked_at` 取首次吊销时刻
// （重复吊销幂等，不改写历史时刻）；行不存在返回 `ErrNotFound`。
// SetStatus only represents revocation and keeps the first revoked_at.
func (s *keyStore) SetStatus(ctx context.Context, callerID, keyID string, status callers.KeyStatus) error {
	if s == nil || s.db == nil {
		return errKeyStoreUnconfigured
	}
	if callerID == "" || keyID == "" {
		return fmt.Errorf("%w: 调用方或 Key 标识为空", callers.ErrNotFound)
	}
	if status != callers.KeyStatusRevoked {
		return fmt.Errorf("%w: 只支持吊销，收到状态 %q", errKeyInvalidArgument, string(status))
	}
	res, err := s.db.pool.ExecContext(ctx, `
		UPDATE caller_keys
		SET revoked_at = COALESCE(revoked_at, $3)
		WHERE key_id = $1 AND caller_id = $2`,
		keyID, callerID, s.db.clock().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("吊销调用方 Key: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取吊销结果: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: 调用方 %s 没有 Key %s", callers.ErrNotFound, callerID, keyID)
	}
	return nil
}

// Rotate 在单个事务内完成「旧 Key 必须仍可用 → 插入新 Key → 吊销旧 Key →
// 按需再绑定任务」：任何一步失败整体回滚，不保留双活窗口，也不留下"新 Key 已发、
// 旧 Key 未吊销"的中间态。
// Rotate performs the whole replacement in one transaction with no dual-active window.
func (s *keyStore) Rotate(ctx context.Context, callerID, oldKeyID string, next callers.KeyRecord, opts callers.RotateOptions) (callers.RotateResult, error) {
	if s == nil || s.db == nil {
		return callers.RotateResult{}, errKeyStoreUnconfigured
	}
	if callerID == "" || oldKeyID == "" || next.KeyID == "" || next.KeyHash == "" || next.KeyPrefix == "" {
		return callers.RotateResult{}, fmt.Errorf("%w: 轮换缺少标识、归属、指纹或前缀", errKeyInvalidArgument)
	}

	var out callers.RotateResult
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.db.clock().UTC()
		var revokedAt sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT revoked_at FROM caller_keys
			WHERE key_id = $1 AND caller_id = $2
			FOR UPDATE`, oldKeyID, callerID).Scan(&revokedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 调用方 %s 没有 Key %s", callers.ErrNotFound, callerID, oldKeyID)
		}
		if err != nil {
			return fmt.Errorf("锁读旧 Key: %w", err)
		}
		if revokedAt.Valid {
			return fmt.Errorf("%w: 旧 Key %s 已吊销，不能再轮换", core.ErrConflict, oldKeyID)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO caller_keys (key_id, caller_id, key_hash, key_prefix, scopes, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			next.KeyID, callerID, next.KeyHash, next.KeyPrefix,
			append([]string{}, next.Scopes...), expiresAtColumn(next.ExpiresAt), now.UnixMilli(),
		); err != nil {
			if isPGUniqueViolation(err) {
				return fmt.Errorf("%w: Key 标识或指纹已存在", core.ErrConflict)
			}
			return fmt.Errorf("写入新 Key: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE caller_keys SET revoked_at = $2 WHERE key_id = $1`,
			oldKeyID, now.UnixMilli(),
		); err != nil {
			return fmt.Errorf("吊销旧 Key: %w", err)
		}

		rebound, leftBound, err := rebindScheduledTasks(ctx, tx, callerID, oldKeyID, next.KeyID, next.Scopes, opts, now)
		if err != nil {
			return err
		}
		out = callers.RotateResult{
			Key: callers.KeyRecord{
				KeyID:     next.KeyID,
				CallerID:  callerID,
				KeyHash:   next.KeyHash,
				KeyPrefix: next.KeyPrefix,
				Scopes:    append([]string(nil), next.Scopes...),
				ExpiresAt: next.ExpiresAt,
				Status:    callers.KeyStatusActive,
				CreatedAt: now,
			},
			ReboundTaskIDs: rebound,
			LeftBound:      leftBound,
		}
		return nil
	})
	if err != nil {
		return callers.RotateResult{}, err
	}
	return out, nil
}

// rebindScheduledTasks 按 `opts.RebindScheduledTasks` 把旧 Key 授权的任务改绑到
// 新 Key：未声明再绑定时**不动**任何任务（默认保持旧绑定，下次触发 fail-closed）；
// 声明了则逐条判定归属与 scope，判不通过的进 `LeftBound`，**不得**静默改绑。
// rebindScheduledTasks only touches tasks when explicitly requested and reports every
// task it could not rebind.
func rebindScheduledTasks(ctx context.Context, tx *sql.Tx, callerID, oldKeyID, newKeyID string,
	scopes []string, opts callers.RotateOptions, now time.Time,
) ([]string, []callers.BoundTask, error) {
	if !opts.RebindScheduledTasks {
		return nil, nil, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT task_id, caller_id, agent_name
		FROM scheduled_tasks
		WHERE authorizing_key_id = $1 AND retired_at IS NULL
		ORDER BY task_id
		FOR UPDATE`, oldKeyID)
	if err != nil {
		return nil, nil, fmt.Errorf("查询待再绑定任务: %w", err)
	}
	// 先把候选行**读完并关游标**，再逐条 UPDATE：pgx 的 stdlib 驱动在结果集未取完时
	// 不接受同一连接上的第二条语句（`driver: bad connection`），边遍历边写会在真库上
	// 必失败。`FOR UPDATE` 的锁在事务内一直有效，收集完再写不改变并发语义。
	// Drain and close the cursor before updating: the pgx stdlib driver rejects a second
	// statement on the same connection while a result set is still open.
	type boundTask struct {
		taskID, taskOwner, agentName string
	}
	var candidates []boundTask
	for rows.Next() {
		var candidate boundTask
		if err := rows.Scan(&candidate.taskID, &candidate.taskOwner, &candidate.agentName); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("读取待再绑定任务: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("遍历待再绑定任务: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("关闭待再绑定任务结果集: %w", err)
	}

	covered := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		covered[scope] = struct{}{}
	}
	var rebound []string
	var leftBound []callers.BoundTask
	for _, candidate := range candidates {
		if candidate.taskOwner != callerID {
			leftBound = append(leftBound, callers.BoundTask{
				TaskID: candidate.taskID, Reason: callers.BoundTaskCallerMismatch,
			})
			continue
		}
		if _, ok := covered[core.AgentInvokeScopePrefix+candidate.agentName]; !ok {
			leftBound = append(leftBound, callers.BoundTask{
				TaskID: candidate.taskID, Reason: callers.BoundTaskScopeMissing,
			})
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE scheduled_tasks SET authorizing_key_id = $2, updated_at = $3
			WHERE task_id = $1`, candidate.taskID, newKeyID, now.UnixMilli()); err != nil {
			return nil, nil, fmt.Errorf("再绑定任务 %s: %w", candidate.taskID, err)
		}
		rebound = append(rebound, candidate.taskID)
	}
	return rebound, leftBound, nil
}

// List 返回调用方列表（`caller_id` 升序）：`KeyCount` 只数未吊销的 Key；种类在
// 这里由 `service` 映射成契约的 `external`（词表差异只在此处出现）。
// List returns callers in ascending id order with their live key counts.
func (s *keyStore) List(ctx context.Context) ([]callers.CallerRecord, error) {
	if s == nil || s.db == nil {
		return nil, errKeyStoreUnconfigured
	}
	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT c.caller_id, c.kind, c.name, c.project, c.status, c.created_at,
		       (SELECT count(*) FROM caller_keys AS k
		         WHERE k.caller_id = c.caller_id AND k.revoked_at IS NULL) AS key_count
		FROM callers AS c
		ORDER BY c.caller_id`)
	if err != nil {
		return nil, fmt.Errorf("查询调用方列表: %w", err)
	}
	defer rows.Close()

	var out []callers.CallerRecord
	for rows.Next() {
		record, err := scanCaller(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历调用方列表: %w", err)
	}
	return out, nil
}

// LookupCaller 按标识返回调用方行（含停用行——停用是可见状态，不是"不存在"）。
// LookupCaller returns one caller row; a disabled row is still visible.
func (s *keyStore) LookupCaller(ctx context.Context, callerID string) (callers.CallerRecord, error) {
	if s == nil || s.db == nil {
		return callers.CallerRecord{}, errKeyStoreUnconfigured
	}
	if strings.TrimSpace(callerID) == "" {
		return callers.CallerRecord{}, fmt.Errorf("%w: 调用方标识为空", callers.ErrNotFound)
	}
	row := s.db.pool.QueryRowContext(ctx, `
		SELECT c.caller_id, c.kind, c.name, c.project, c.status, c.created_at,
		       (SELECT count(*) FROM caller_keys AS k
		         WHERE k.caller_id = c.caller_id AND k.revoked_at IS NULL) AS key_count
		FROM callers AS c
		WHERE c.caller_id = $1`, callerID)
	record, err := scanCaller(row)
	if errors.Is(err, sql.ErrNoRows) {
		return callers.CallerRecord{}, fmt.Errorf("%w: 调用方 %s", callers.ErrNotFound, callerID)
	}
	if err != nil {
		return callers.CallerRecord{}, fmt.Errorf("查询调用方: %w", err)
	}
	return record, nil
}

// callerScanner 是 `*sql.Row` 与 `*sql.Rows` 的共同扫描面。
// callerScanner is the shared scan surface of sql.Row and sql.Rows.
type callerScanner interface{ Scan(dest ...any) error }

// scanCaller 把一行调用方投影成领域记录，并完成 `service → external` 映射。
// scanCaller projects a caller row and maps the kind vocabulary.
func scanCaller(scanner callerScanner) (callers.CallerRecord, error) {
	var (
		record    callers.CallerRecord
		kind      string
		status    string
		createdAt int64
	)
	if err := scanner.Scan(&record.CallerID, &kind, &record.Name, &record.Project,
		&status, &createdAt, &record.KeyCount); err != nil {
		return callers.CallerRecord{}, err
	}
	switch kind {
	case "builtin":
		record.Kind = callers.CallerKindBuiltin
	case "service":
		record.Kind = callers.CallerKindExternal
	default:
		return callers.CallerRecord{}, fmt.Errorf("%w: 未知调用方种类 %q", errKeyInvalidArgument, kind)
	}
	switch callers.CallerStatus(status) {
	case callers.CallerStatusActive, callers.CallerStatusDisabled:
		record.Status = callers.CallerStatus(status)
	default:
		return callers.CallerRecord{}, fmt.Errorf("%w: 未知调用方状态 %q", errKeyInvalidArgument, status)
	}
	record.CreatedAt = time.UnixMilli(createdAt).UTC()
	return record, nil
}

// CreateCaller 在单个事务内写入调用方行与可选限额行：限额为 nil 时**不写**
// `caller_limits`（缺配置即拒绝，不落一份看起来已配置的默认数字）。
// CreateCaller writes the caller and its optional limit row in one transaction.
func (s *keyStore) CreateCaller(ctx context.Context, record callers.CallerRecord, limit *callers.Limit) error {
	if s == nil || s.db == nil {
		return errKeyStoreUnconfigured
	}
	if record.CallerID == "" || strings.TrimSpace(record.Name) == "" {
		return fmt.Errorf("%w: 调用方缺少标识或名称", errKeyInvalidArgument)
	}
	if !record.Kind.Valid() || !record.Status.Valid() {
		return fmt.Errorf("%w: 调用方种类或状态不在闭集内", errKeyInvalidArgument)
	}
	kind, err := callerKindColumn(record.Kind)
	if err != nil {
		return err
	}
	var normalized *callers.Limit
	if limit != nil {
		value, err := limit.Normalize()
		if err != nil {
			return err
		}
		normalized = &value
	}
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = s.db.clock()
	}

	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO callers (caller_id, kind, name, project, status, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			record.CallerID, kind, record.Name, record.Project, string(record.Status), createdAt.UnixMilli(),
		); err != nil {
			if isPGUniqueViolation(err) {
				return fmt.Errorf("%w: 调用方 %s 或名称 %q 已存在", core.ErrConflict, record.CallerID, record.Name)
			}
			return fmt.Errorf("写入调用方: %w", err)
		}
		if normalized == nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO caller_limits
				(caller_id, qps, burst, max_concurrent, tasks_per_period, tokens_per_period,
				 ingest_bytes_per_period, period, timezone, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			record.CallerID, normalized.QPS, normalized.Burst, normalized.MaxConcurrent,
			normalized.TasksPerPeriod, normalized.TokensPerPeriod, normalized.IngestBytesPerPeriod,
			string(normalized.Period), normalized.Timezone, createdAt.UnixMilli(),
		); err != nil {
			return fmt.Errorf("写入调用方限额: %w", err)
		}
		return nil
	})
}

// callerKindColumn 把契约词汇映射回 DDL 的两值：`external → service`；
// 内置管理员只用于种子行，管理面**不得**新建 builtin 调用方。
// callerKindColumn maps external back to service and refuses new builtin rows.
func callerKindColumn(kind callers.CallerKind) (string, error) {
	switch kind {
	case callers.CallerKindExternal:
		return "service", nil
	case callers.CallerKindBuiltin:
		return "", fmt.Errorf("%w: 管理面不得新建内置调用方", errKeyInvalidArgument)
	default:
		return "", fmt.Errorf("%w: 未知调用方种类 %q", errKeyInvalidArgument, string(kind))
	}
}

// Usage 聚合一个周期的用量视图：先确认调用方存在（不存在 → `ErrNotFound`），
// 再读限额（缺失按 day / UTC 折算，零限额照实返回）、账本行、按类别拆开的
// 已结算 / 预留量与跨周期 pending 条数。`periodStart` 为 0 表示当前周期，
// 非 0 时折算到所属周期的起点（同一周期内的任意瞬时得到同一视图）。
// Usage aggregates one period; the caller's existence is the only 404 condition.
func (s *keyStore) Usage(ctx context.Context, callerID string, periodStart int64, now time.Time) (callers.UsageAggregate, error) {
	if s == nil || s.db == nil {
		return callers.UsageAggregate{}, errKeyStoreUnconfigured
	}
	if strings.TrimSpace(callerID) == "" {
		return callers.UsageAggregate{}, fmt.Errorf("%w: 调用方标识为空", callers.ErrNotFound)
	}
	if now.IsZero() {
		now = s.db.clock()
	}

	var out callers.UsageAggregate
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := ensureCallerExists(ctx, tx, callerID); err != nil {
			return err
		}
		limit, err := callerLimit(ctx, tx, callerID)
		if err != nil {
			return err
		}
		at := now
		if periodStart != 0 {
			at = time.UnixMilli(periodStart).UTC()
		}
		start, err := callers.PeriodStart(limit.Period, limit.Timezone, at)
		if err != nil {
			return err
		}
		startMillis := start.UnixMilli()

		ledger, err := readLedgerUsage(ctx, tx, callerID, startMillis)
		if err != nil {
			return err
		}
		classes, err := readClassUsage(ctx, tx, callerID, startMillis)
		if err != nil {
			return err
		}
		pending, err := countPendingReservations(ctx, tx, callerID)
		if err != nil {
			return err
		}
		out = callers.UsageAggregate{
			CallerID:            callerID,
			PeriodStart:         startMillis,
			Limit:               limit,
			Ledger:              ledger,
			Classes:             classes,
			PendingReservations: pending,
		}
		return nil
	})
	if err != nil {
		return callers.UsageAggregate{}, err
	}
	return out, nil
}

// ensureCallerExists 判定调用方行是否存在。
// ensureCallerExists reports whether the caller row exists.
func ensureCallerExists(ctx context.Context, tx *sql.Tx, callerID string) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM callers WHERE caller_id = $1`, callerID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 调用方 %s", callers.ErrNotFound, callerID)
	}
	if err != nil {
		return fmt.Errorf("查询调用方: %w", err)
	}
	return nil
}

// callerLimit 读取限额行；行缺失返回零限额 + DDL 缺省的周期 / 时区。
// callerLimit reads the limit row; a missing row yields zero limits with the
// documented period defaults.
func callerLimit(ctx context.Context, tx *sql.Tx, callerID string) (callers.Limit, error) {
	var (
		limit    callers.Limit
		qps      float64
		period   string
		timezone string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT qps::float8, burst, max_concurrent, tasks_per_period, tokens_per_period,
		       ingest_bytes_per_period, period, timezone
		FROM caller_limits WHERE caller_id = $1`, callerID,
	).Scan(&qps, &limit.Burst, &limit.MaxConcurrent, &limit.TasksPerPeriod,
		&limit.TokensPerPeriod, &limit.IngestBytesPerPeriod, &period, &timezone)
	if errors.Is(err, sql.ErrNoRows) {
		return callers.Limit{Period: "day", Timezone: callers.DefaultTimezone}, nil
	}
	if err != nil {
		return callers.Limit{}, fmt.Errorf("读取调用方限额: %w", err)
	}
	limit.QPS = qps
	limit.Period = callers.Period(period)
	limit.Timezone = timezone
	return limit.Normalize()
}

// readLedgerUsage 读该周期存在的账本行（可能为空）。
// readLedgerUsage reads the period's ledger rows.
func readLedgerUsage(ctx context.Context, tx *sql.Tx, callerID string, periodStart int64) ([]callers.LedgerUsage, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT kind, used, reserved, overage
		FROM caller_usage_periods
		WHERE caller_id = $1 AND period_start = $2
		ORDER BY kind`, callerID, periodStart)
	if err != nil {
		return nil, fmt.Errorf("查询周期账本: %w", err)
	}
	defer rows.Close()

	var out []callers.LedgerUsage
	for rows.Next() {
		var line callers.LedgerUsage
		if err := rows.Scan(&line.Kind, &line.Used, &line.Reserved, &line.Overage); err != nil {
			return nil, fmt.Errorf("读取周期账本: %w", err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历周期账本: %w", err)
	}
	return out, nil
}

// readClassUsage 按 (类别, 账本) 聚合该周期的预留：结算态（committed /
// reconciled）累加 actual，pending 累加 amount；released 两者都不计。
// 账本由预留标识前缀解析——`qres_ingest_bytes_` 本身含下划线，因此用前缀长度
// 比较而不是 `split_part`。
// readClassUsage aggregates reservations per class and ledger; the ledger is decoded
// from the id prefix by length comparison because "ingest_bytes" contains an
// underscore itself.
func readClassUsage(ctx context.Context, tx *sql.Tx, callerID string, periodStart int64) ([]callers.ClassUsage, error) {
	tasksPrefix := reservationIDPrefix + usageKindTasks + "_"
	tokensPrefix := reservationIDPrefix + usageKindTokens + "_"
	bytesPrefix := reservationIDPrefix + usageKindIngestBytes + "_"
	rows, err := tx.QueryContext(ctx, `
		SELECT class,
		       CASE WHEN left(reservation_id, length($3)) = $3 THEN 'tasks'
		            WHEN left(reservation_id, length($4)) = $4 THEN 'tokens'
		            WHEN left(reservation_id, length($5)) = $5 THEN 'ingest_bytes'
		            ELSE '' END AS ledger,
		       COALESCE(SUM(CASE WHEN state IN ('committed', 'reconciled') THEN actual ELSE 0 END), 0) AS used,
		       COALESCE(SUM(CASE WHEN state = 'pending' THEN amount ELSE 0 END), 0) AS reserved
		FROM quota_reservations
		WHERE caller_id = $1 AND period_start = $2
		GROUP BY 1, 2
		ORDER BY 1, 2`,
		callerID, periodStart, tasksPrefix, tokensPrefix, bytesPrefix)
	if err != nil {
		return nil, fmt.Errorf("查询类别用量: %w", err)
	}
	defer rows.Close()

	var out []callers.ClassUsage
	for rows.Next() {
		var (
			line   callers.ClassUsage
			class  string
			ledger string
		)
		if err := rows.Scan(&class, &ledger, &line.Used, &line.Reserved); err != nil {
			return nil, fmt.Errorf("读取类别用量: %w", err)
		}
		if ledger == "" {
			// 前缀解析不出账本说明预留标识不是本服务生成的——按数据不一致报错，
			// **不**静默把它算到某一本账上。
			return nil, fmt.Errorf("%w: 类别 %q 的预留标识无法解析账本", errQuotaInvalidReservationID, class)
		}
		line.Class = callers.RequestClass(class)
		line.Ledger = ledger
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历类别用量: %w", err)
	}
	return out, nil
}

// countPendingReservations 数该调用方**跨周期**的 pending 预留（与 `ListPending`
// 同口径）：周期视图里的 `reserved` 只算本周期，而需要人工核对的条数是全局的。
// countPendingReservations counts unresolved reservations across all periods.
func countPendingReservations(ctx context.Context, tx *sql.Tx, callerID string) (int, error) {
	var pending int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM quota_reservations
		WHERE caller_id = $1 AND state = 'pending'`, callerID,
	).Scan(&pending); err != nil {
		return 0, fmt.Errorf("统计未结算预留: %w", err)
	}
	return pending, nil
}

// expiresAtColumn 把可选过期时刻转成可空毫秒列（nil 落 NULL）。
// expiresAtColumn renders the optional expiry as a nullable millisecond column.
func expiresAtColumn(at *time.Time) any {
	if at == nil {
		return nil
	}
	return at.UTC().UnixMilli()
}

// isPGUniqueViolation 判定错误是否是 PostgreSQL 唯一约束冲突（`23505`）。
// isPGUniqueViolation reports a PostgreSQL unique violation.
func isPGUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolationSQLState
}
