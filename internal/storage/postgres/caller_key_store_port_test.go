//go:build integration

package postgres

// 本文件是 `callers.KeyStore` 的 PostgreSQL 实现（`caller_key_store.go`）的包级判卷。
// 本文件**没有 SQLite 旧用例可移植**——`internal/storage/postgres` 的 Key / 调用方
// 仓储是 US-16 新增实现，按实现契约新写。判据集中在真库才看得见的地方：
//
//  1. **明文绝不落库**——库里只有 SHA-256 指纹与展示前缀；未知指纹按「未找到」拒绝，
//     错误文案不回显查询输入（认证面不得成为探测面）。
//  2. **轮换单事务**——旧 Key 必须仍可用 → 插入新 Key → 吊销旧 Key → 按需再绑定任务；
//     插入冲突整体回滚，不保留双活窗口，也不留下「新 Key 已发、旧 Key 未吊销」。
//  3. **状态是投影**——`active` / `revoked` / `expired` 由 `revoked_at` / `expires_at`
//     （吊销优先）与句柄时钟算出；调用方停用按「未找到」处理（认证面不区分原因）。
//  4. **停用 / 吊销即再认证失败**——认证器复用同一个存储，断言读的是认证器的回答。
//  5. **用量按周期聚合**——限额行缺失返回零限额 + day/UTC 缺省（不是不限量）；账本
//     行照实读；类别用量按预留标识前缀拆账本，解析不出按数据不一致报错。
//  6. **没有句柄即失败关闭**——每个方法都拒绝服务，不得退化成"空结果"。
//
// These are the package-level cases for the PostgreSQL KeyStore. There is no SQLite
// lineage to port from: the store is new in US-16, so the assertions are written from
// the implementation contract. Every case runs against a real, isolated, migrated
// schema; the clock is pinned so time-derived projections are recomputable.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// keyStoreNow 是本文件夹具的固定写库时刻（时间列一律 Unix 毫秒 UTC，断言可复算）。
// keyStoreNow is the fixed write instant of this file's fixture.
var keyStoreNow = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// 两个外部调用方的固定标识（`cal_` 前缀只作可读性，表上没有格式约束）。
// Two fixed external caller ids; the cal_ shape is documentation only.
const (
	keyStoreCallerA = "cal_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	keyStoreCallerB = "cal_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"
)

// keyStoreTestDB 返回已迁移的隔离 schema 句柄，并把句柄时钟钉在 keyStoreNow——
// `Lookup` 的状态投影、`Create` / `CreateCaller` 的缺省创建时刻、`Rotate` 的事务
// 时刻与 `Usage` 的当前周期都取它，不注入就只能拿真实时钟赌时序。
// keyStoreTestDB pins the handle clock so every time-derived projection is deterministic.
func keyStoreTestDB(t *testing.T) *DB {
	t.Helper()
	db := portTestDB(t)
	db.now = func() time.Time { return keyStoreNow }
	return db
}

// keyStoreSeedCaller 直接种调用方行（kind 取 DDL 的 `service` / `builtin` 两值）。
// keyStoreSeedCaller inserts one caller row with the DDL kind literal.
func keyStoreSeedCaller(t *testing.T, db *DB, callerID, kind, name, status string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, $2, $3, '', $4, $5)`,
		callerID, kind, name, status, keyStoreNow.UnixMilli())
	require.NoError(t, err)
}

// keyStoreSecret 造一把与签发格式一致的明文（`bk_` + 40 位小写十六进制）。
// keyStoreSecret mints a plaintext shaped exactly like an issued key.
func keyStoreSecret(digit string) string {
	return "bk_" + strings.Repeat(digit, 40)
}

// keyStoreFingerprint 独立算出明文的 SHA-256 小写十六进制（不调用实现里的私有函数）。
// keyStoreFingerprint recomputes the fingerprint independently of the implementation.
func keyStoreFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// keyStoreRecord 按签发形态构造一行 Key：指纹与前缀由明文派生，明文本身不进记录。
// keyStoreRecord builds a key row whose hash and prefix derive from the plaintext.
func keyStoreRecord(keyID, callerID, secret string, scopes ...string) callers.KeyRecord {
	return callers.KeyRecord{
		KeyID:     keyID,
		CallerID:  callerID,
		KeyHash:   keyStoreFingerprint(secret),
		KeyPrefix: secret[:8],
		Scopes:    scopes,
		CreatedAt: keyStoreNow,
	}
}

// keyStoreMillis 把可选时刻转成可空毫秒列（nil 落 NULL）。
// keyStoreMillis renders an optional instant as a nullable millisecond column.
func keyStoreMillis(at *time.Time) any {
	if at == nil {
		return nil
	}
	return at.UTC().UnixMilli()
}

// keyStoreSeedKeyRow 直接写一行 Key（绕过 `Create`），用来制造吊销 / 过期 / 停用等
// 已有状态；`scopes` 一律传空切片而不是 nil（列是 `text[] NOT NULL DEFAULT '{}'`，
// 传 nil 会落 SQL NULL 被 NOT NULL 拒绝）。
// keyStoreSeedKeyRow plants a key row in a chosen lifecycle state; scopes are always an
// empty slice rather than nil because the column is NOT NULL text[].
func keyStoreSeedKeyRow(t *testing.T, db *DB, callerID, keyID string, scopes []string, expiresAt, revokedAt *time.Time) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO caller_keys (key_id, caller_id, key_hash, key_prefix, scopes, expires_at, revoked_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		keyID, callerID, "hash-"+keyID, "bk_"+keyID, append([]string{}, scopes...),
		keyStoreMillis(expiresAt), keyStoreMillis(revokedAt), keyStoreNow.UnixMilli())
	require.NoError(t, err)
}

// keyStoreSeedTask 种一条绑定到指定 Key 的定时任务；`retired` 为真时写退役时刻。
// keyStoreSeedTask plants one scheduled task bound to the given key.
func keyStoreSeedTask(t *testing.T, db *DB, taskID, callerID, keyID, agent string, retired bool) {
	t.Helper()
	var retiredAt any
	if retired {
		retiredAt = keyStoreNow.UnixMilli()
	}
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO scheduled_tasks
			(task_id, agent_name, cron, zone, message, deliver_to, enabled, definition_hash,
			 retired_at, created_at, updated_at, caller_id, authorizing_kind, authorizing_key_id)
		VALUES ($1, $2, '0 */5 * * * *', 'UTC', '巡检', '', true, 'hash', $3, $4, $4, $5, 'caller-key', $6)`,
		taskID, agent, retiredAt, keyStoreNow.UnixMilli(), callerID, keyID)
	require.NoError(t, err)
}

// keyStoreTaskKey 读任务当前绑定的 Key。
// keyStoreTaskKey reads the task's current authorizing key.
func keyStoreTaskKey(t *testing.T, db *DB, taskID string) string {
	t.Helper()
	var keyID string
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT authorizing_key_id FROM scheduled_tasks WHERE task_id = $1`, taskID).Scan(&keyID))
	return keyID
}

// keyStoreReservationID 拼一个 `qres_<账本>_<26 位令牌>` 形态的预留标识；账本前缀
// 逐字取自实现契约（`qres_tasks_` / `qres_tokens_` / `qres_ingest_bytes_`）。
// keyStoreReservationID builds a reservation id in the frozen `qres_<ledger>_<payload>` form.
func keyStoreReservationID(prefix, suffix string) string {
	return prefix + strings.Repeat("0", 26-len(suffix)) + suffix
}

// keyStoreSeedReservation 直接种一条预留行；`actual` 为 nil 时落 NULL。
// keyStoreSeedReservation plants one reservation row.
func keyStoreSeedReservation(t *testing.T, db *DB, reservationID, callerID, idem, class string,
	periodStart, amount int64, actual *int64, state string,
) {
	t.Helper()
	var actualValue any
	if actual != nil {
		actualValue = *actual
	}
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO quota_reservations
			(reservation_id, caller_id, idempotency_key, class, period_start, amount, actual, state, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		reservationID, callerID, idem, class, periodStart, amount, actualValue, state, keyStoreNow.UnixMilli())
	require.NoError(t, err)
}

// keyStoreSeedLedger 直接种一行周期账本。
// keyStoreSeedLedger plants one period ledger row.
func keyStoreSeedLedger(t *testing.T, db *DB, callerID string, periodStart int64, kind string, used, reserved, overage int64) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO caller_usage_periods
			(caller_id, period_start, kind, used, reserved, overage, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		callerID, periodStart, kind, used, reserved, overage, keyStoreNow.UnixMilli())
	require.NoError(t, err)
}

// keyStoreSeedLimitRow 直接种一行限额（其余维度为 0，额度只看 `tokens`）。
// keyStoreSeedLimitRow plants one limit row; only the token ledger gets a quota.
func keyStoreSeedLimitRow(t *testing.T, db *DB, callerID, period, timezone string, tokens int64) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO caller_limits
			(caller_id, qps, burst, max_concurrent, tasks_per_period, tokens_per_period,
			 ingest_bytes_per_period, period, timezone, updated_at)
		VALUES ($1, 0, 0, 0, 0, $2, 0, $3, $4, $5)`,
		callerID, tokens, period, timezone, keyStoreNow.UnixMilli())
	require.NoError(t, err)
}

// keyStoreCountLimits 数某调用方的限额行（判"limit 为 nil 不得落行"）。
// keyStoreCountLimits counts the limit rows of one caller.
func keyStoreCountLimits(t *testing.T, db *DB, callerID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM caller_limits WHERE caller_id = $1`, callerID).Scan(&count))
	return count
}

// TestKeyStore_LookupRoundTripByFingerprint：签发形态的行写入后按指纹读回全部字段——
// 标识、归属、指纹、前缀、scope（保序）、过期时刻、创建时刻与 active 状态；空 scope
// 读回空切片而不是 nil。
// 中文验收点：按指纹查表是认证路径唯一的查表形态，读回值必须逐字段对得上。
func TestKeyStore_LookupRoundTripByFingerprint(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")

	expiresAt := keyStoreNow.Add(48 * time.Hour)
	secret := keyStoreSecret("a")
	record := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A", keyStoreCallerA, secret,
		core.ScopeSessionsRead, core.AgentInvokeScopePrefix+"daily")
	record.ExpiresAt = &expiresAt
	require.NoError(t, store.Create(ctx, record))

	got, err := store.Lookup(ctx, keyStoreFingerprint(secret))
	require.NoError(t, err)
	require.Equal(t, record.KeyID, got.KeyID)
	require.Equal(t, record.CallerID, got.CallerID)
	require.Equal(t, record.KeyHash, got.KeyHash)
	require.Equal(t, record.KeyPrefix, got.KeyPrefix)
	require.Equal(t, record.Scopes, got.Scopes, "scope 保序，不重排")
	require.NotNil(t, got.ExpiresAt)
	require.True(t, got.ExpiresAt.Equal(expiresAt), "过期时刻按毫秒往返")
	require.Nil(t, got.RevokedAt)
	require.True(t, got.CreatedAt.Equal(keyStoreNow))
	require.Equal(t, callers.KeyStatusActive, got.Status)

	// 空 scope 读回为空切片而不是 nil：调用方按集合处理，不需要再做 nil 判定。
	// An empty scope set comes back as an empty slice, never nil.
	empty := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B", keyStoreCallerA, keyStoreSecret("b"))
	require.NoError(t, store.Create(ctx, empty))
	got, err = store.Lookup(ctx, empty.KeyHash)
	require.NoError(t, err)
	require.NotNil(t, got.Scopes)
	require.Empty(t, got.Scopes)
}

// TestKeyStore_LookupRejectsMalformedAndUnknownFingerprint：空指纹在触达 SQL 前拒绝；
// 未知指纹按 ErrNotFound 拒绝且错误文案不回显查询输入；认证面把同一失败投影成唯一的
// ErrUnauthorized，同样不回显凭据。
// 中文验收点：认证失败不得成为探测面——错误里既不能有明文，也不能有指纹。
func TestKeyStore_LookupRejectsMalformedAndUnknownFingerprint(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()

	_, err := store.Lookup(ctx, "   ")
	require.ErrorIs(t, err, errKeyInvalidArgument, "空指纹是参数错误，不查库")

	fingerprint := keyStoreFingerprint(keyStoreSecret("c"))
	require.Len(t, fingerprint, 64, "查表键是 64 位十六进制 SHA-256")
	_, err = store.Lookup(ctx, fingerprint)
	require.ErrorIs(t, err, callers.ErrNotFound, "未知指纹按未找到拒绝")
	require.NotContains(t, err.Error(), fingerprint, "错误不得回显查询指纹")

	// 认证面：同一失败返回唯一对外错误，且文案里没有明文。
	auth := callers.NewAuthenticator(store, nil)
	secret := keyStoreSecret("d")
	_, err = auth.Authenticate(ctx, secret)
	require.ErrorIs(t, err, callers.ErrUnauthorized)
	require.NotContains(t, err.Error(), secret, "认证失败不得回显凭据")
}

// TestKeyStore_LookupProjectsLifecycleStatus：状态由 `revoked_at` / `expires_at` 与
// 句柄时钟投影——吊销优先于过期；恰好到期的瞬时刻判 expired（`!After(now)`）；
// 调用方停用时整把 Key 按「未找到」处理。
// 中文验收点：存储层只投影状态，不替认证层放行，也不把不可用行伪装成命中。
func TestKeyStore_LookupProjectsLifecycleStatus(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
	keyStoreSeedCaller(t, db, keyStoreCallerB, "service", "beta", "disabled")

	future := keyStoreNow.Add(time.Hour)
	past := keyStoreNow.Add(-time.Hour)
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-active", nil, &future, nil)
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-expired", nil, &past, nil)
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-revoked", nil, &future, &past)
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-revoked-expired", nil, &past, &past)
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-expires-now", nil, &keyStoreNow, nil)
	keyStoreSeedKeyRow(t, db, keyStoreCallerB, "key-disabled-caller", nil, &future, nil)

	lookup := func(keyID string) callers.KeyRecord {
		t.Helper()
		record, err := store.Lookup(ctx, "hash-"+keyID)
		require.NoError(t, err)
		return record
	}
	require.Equal(t, callers.KeyStatusActive, lookup("key-active").Status)
	require.Equal(t, callers.KeyStatusExpired, lookup("key-expired").Status, "过期时刻已过即 expired")
	require.Equal(t, callers.KeyStatusRevoked, lookup("key-revoked").Status, "吊销优先于过期")
	require.Equal(t, callers.KeyStatusRevoked, lookup("key-revoked-expired").Status, "吊销优先于过期")
	require.Equal(t, callers.KeyStatusExpired, lookup("key-expires-now").Status,
		"恰好等于当前时刻也算过期（!expiresAt.After(now)）")

	_, err := store.Lookup(ctx, "hash-key-disabled-caller")
	require.ErrorIs(t, err, callers.ErrNotFound, "调用方停用按未找到处理，不返回可区分状态")
}

// TestKeyStore_LookupByIDOwnershipAndVisibility：按 `(caller_id, key_id)` 定位——归属
// 不匹配、行不存在、空标识与调用方停用一律 ErrNotFound；已吊销的行仍按主键可见，
// 状态由存储层投影（认证层自行决定放不放行）。
// 中文验收点：后台复核读的是「那一把」Key，归属漂移必须收敛到同一种未找到。
func TestKeyStore_LookupByIDOwnershipAndVisibility(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
	keyStoreSeedCaller(t, db, keyStoreCallerB, "service", "beta", "active")
	keyStoreSeedCaller(t, db, "cal_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D", "service", "delta", "disabled")

	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-a", []string{"sessions:read"}, nil, nil)
	keyStoreSeedKeyRow(t, db, "cal_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D", "key-off", nil, nil, nil)

	record, err := store.LookupByID(ctx, keyStoreCallerA, "key-a")
	require.NoError(t, err)
	require.Equal(t, "key-a", record.KeyID)
	require.Equal(t, keyStoreCallerA, record.CallerID)
	require.Equal(t, callers.KeyStatusActive, record.Status)

	_, err = store.LookupByID(ctx, keyStoreCallerB, "key-a")
	require.ErrorIs(t, err, callers.ErrNotFound, "归属不匹配按未找到")
	_, err = store.LookupByID(ctx, keyStoreCallerA, "key-missing")
	require.ErrorIs(t, err, callers.ErrNotFound)
	_, err = store.LookupByID(ctx, "", "key-a")
	require.ErrorIs(t, err, callers.ErrNotFound)
	_, err = store.LookupByID(ctx, keyStoreCallerA, " ")
	require.ErrorIs(t, err, callers.ErrNotFound)
	_, err = store.LookupByID(ctx, "cal_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D", "key-off")
	require.ErrorIs(t, err, callers.ErrNotFound, "调用方停用时按未找到")

	require.NoError(t, store.SetStatus(ctx, keyStoreCallerA, "key-a", callers.KeyStatusRevoked))
	record, err = store.LookupByID(ctx, keyStoreCallerA, "key-a")
	require.NoError(t, err)
	require.Equal(t, callers.KeyStatusRevoked, record.Status)
	require.NotNil(t, record.RevokedAt)
}

// TestKeyStore_CreateStoresOnlyFingerprintAndPrefix：整行序列化后不得包含明文；指纹
// 等于独立算出的 SHA-256；前缀是明文的一段且必须短于明文。
// 中文验收点：Key 明文绝不入库——库里只有不可逆的指纹与展示前缀。
func TestKeyStore_CreateStoresOnlyFingerprintAndPrefix(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")

	secret := keyStoreSecret("e")
	record := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C", keyStoreCallerA, secret, core.ScopeSessionsRead)
	require.NoError(t, store.Create(ctx, record))

	var rowText, hash, prefix string
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT to_jsonb(k)::text, key_hash, key_prefix FROM caller_keys AS k WHERE key_id = $1`,
		record.KeyID).Scan(&rowText, &hash, &prefix))
	require.NotContains(t, rowText, secret, "库里只存指纹与前缀，明文绝不落库")
	require.Equal(t, keyStoreFingerprint(secret), hash, "指纹必须是明文的 SHA-256（小写十六进制）")
	require.True(t, strings.HasPrefix(secret, prefix), "前缀是明文的一段，只用于展示")
	require.Less(t, len(prefix), len(secret), "前缀必须短于明文：等长等于把 Key 明文留在库里")
}

// TestKeyStore_CreateRejectsInvalidAndDuplicateRows：缺标识 / 归属 / 指纹 / 前缀在触达
// SQL 前拒绝；主键与指纹冲突都映射成 core.ErrConflict；外键缺失是普通写入错误，
// 不得被误报成冲突；缺省创建时刻用句柄时钟补齐。
// 中文验收点：冲突与装配错误的分类只由存储层做，web 层不必认识驱动错误。
func TestKeyStore_CreateRejectsInvalidAndDuplicateRows(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")

	valid := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D", keyStoreCallerA, keyStoreSecret("f"))

	missing := []struct {
		name   string
		mutate func(*callers.KeyRecord)
	}{
		{"key_id", func(r *callers.KeyRecord) { r.KeyID = "" }},
		{"caller_id", func(r *callers.KeyRecord) { r.CallerID = "" }},
		{"key_hash", func(r *callers.KeyRecord) { r.KeyHash = "" }},
		{"key_prefix", func(r *callers.KeyRecord) { r.KeyPrefix = "" }},
	}
	for _, tc := range missing {
		t.Run(tc.name, func(t *testing.T) {
			record := valid
			tc.mutate(&record)
			require.ErrorIs(t, store.Create(ctx, record), errKeyInvalidArgument)
		})
	}

	require.NoError(t, store.Create(ctx, valid))

	duplicateID := valid
	duplicateID.KeyHash = keyStoreFingerprint(keyStoreSecret("g"))
	require.ErrorIs(t, store.Create(ctx, duplicateID), core.ErrConflict, "主键冲突映射为 ErrConflict")

	duplicateHash := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3E", keyStoreCallerA, keyStoreSecret("f"))
	require.ErrorIs(t, store.Create(ctx, duplicateHash), core.ErrConflict, "指纹唯一冲突映射为 ErrConflict")

	// 外键缺失是普通写入错误（不是 23505），不得被误报成冲突。
	orphan := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3F", "cal_missing", keyStoreSecret("h"))
	err := store.Create(ctx, orphan)
	require.Error(t, err)
	require.NotErrorIs(t, err, core.ErrConflict)
	require.ErrorContains(t, err, "写入调用方 Key")

	// 缺省 CreatedAt 用句柄时钟补齐。
	noCreated := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3G", keyStoreCallerA, keyStoreSecret("i"))
	noCreated.CreatedAt = time.Time{}
	require.NoError(t, store.Create(ctx, noCreated))
	stored, err := store.LookupByID(ctx, keyStoreCallerA, noCreated.KeyID)
	require.NoError(t, err)
	require.True(t, stored.CreatedAt.Equal(keyStoreNow))
}

// TestKeyStore_SetStatusRevokesIdempotently：吊销落 `revoked_at` 并让同一把明文立刻
// 认证失败；重复吊销幂等（保持首次时刻）；行不存在与归属不匹配都是 ErrNotFound。
// 中文验收点：停用 / 吊销状态的变化必须让再认证失败，且重复动作不改写历史时刻。
func TestKeyStore_SetStatusRevokesIdempotently(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")

	// scope 非空：认证返回的调用方 Principal 要求至少一个已冻结的 scope，
	// 这里签一把带 `sessions:read` 的 Key 才能先证明"吊销前可用"。
	secret := keyStoreSecret("j")
	record := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3H", keyStoreCallerA, secret, core.ScopeSessionsRead)
	require.NoError(t, store.Create(ctx, record))

	auth := callers.NewAuthenticator(store, nil)
	_, err := auth.Authenticate(ctx, secret)
	require.NoError(t, err, "吊销前同一把 Key 可认证")

	require.NoError(t, store.SetStatus(ctx, keyStoreCallerA, record.KeyID, callers.KeyStatusRevoked))
	revoked, err := store.LookupByID(ctx, keyStoreCallerA, record.KeyID)
	require.NoError(t, err)
	require.Equal(t, callers.KeyStatusRevoked, revoked.Status)
	require.NotNil(t, revoked.RevokedAt)
	require.True(t, revoked.RevokedAt.Equal(keyStoreNow))

	// 重复吊销幂等：时钟前进了也不改写首次吊销时刻。
	db.now = func() time.Time { return keyStoreNow.Add(2 * time.Hour) }
	require.NoError(t, store.SetStatus(ctx, keyStoreCallerA, record.KeyID, callers.KeyStatusRevoked))
	again, err := store.LookupByID(ctx, keyStoreCallerA, record.KeyID)
	require.NoError(t, err)
	require.True(t, again.RevokedAt.Equal(keyStoreNow), "重复吊销保持首次时刻")

	_, err = auth.Authenticate(ctx, secret)
	require.ErrorIs(t, err, callers.ErrUnauthorized, "吊销后再认证失败")

	// 指纹查表仍返回行（状态是投影结果），由认证层拒绝。
	byFingerprint, err := store.Lookup(ctx, record.KeyHash)
	require.NoError(t, err)
	require.Equal(t, callers.KeyStatusRevoked, byFingerprint.Status)

	require.ErrorIs(t, store.SetStatus(ctx, keyStoreCallerA, "key-missing", callers.KeyStatusRevoked), callers.ErrNotFound)
	require.ErrorIs(t, store.SetStatus(ctx, keyStoreCallerB, record.KeyID, callers.KeyStatusRevoked), callers.ErrNotFound)
	require.ErrorIs(t, store.SetStatus(ctx, "", record.KeyID, callers.KeyStatusRevoked), callers.ErrNotFound)
	require.ErrorIs(t, store.SetStatus(ctx, keyStoreCallerA, " ", callers.KeyStatusRevoked), callers.ErrNotFound)
}

// TestKeyStore_SetStatusRejectsUnsupportedStates：本实现只表示吊销，active / expired /
// 空值一律参数错误，且被拒的调用不得改动行。
// 中文验收点：可表示的迁移只有一条，其他状态不得被静默当成"没变化"。
func TestKeyStore_SetStatusRejectsUnsupportedStates(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-a", nil, nil, nil)

	for _, status := range []callers.KeyStatus{callers.KeyStatusActive, callers.KeyStatusExpired, ""} {
		err := store.SetStatus(ctx, keyStoreCallerA, "key-a", status)
		require.ErrorIs(t, err, errKeyInvalidArgument, "只支持吊销，%q 必须拒绝", status)
	}

	record, err := store.LookupByID(ctx, keyStoreCallerA, "key-a")
	require.NoError(t, err)
	require.Nil(t, record.RevokedAt, "被拒的调用不得留下吊销时刻")
}

// TestKeyStore_RotateReplacesWithoutDualActiveWindow：轮换同一事务内新建新 Key 并吊销
// 旧 Key——旧明文立刻失效、新明文可用，同一调用方恰好一把未吊销 Key，不存在双活窗口。
// 中文验收点：轮换不得留下「新旧都能用」或「新已发、旧未吊销」的中间态。
func TestKeyStore_RotateReplacesWithoutDualActiveWindow(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")

	oldSecret := keyStoreSecret("k")
	oldRecord := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3J", keyStoreCallerA, oldSecret, core.ScopeSessionsRead)
	require.NoError(t, store.Create(ctx, oldRecord))

	newSecret := keyStoreSecret("m")
	next := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3K", keyStoreCallerA, newSecret,
		core.ScopeSessionsRead, core.ScopeSessionsWrite)
	next.CreatedAt = time.Time{} // 创建时刻由存储层按事务时刻写入，不取调用方给的值

	result, err := store.Rotate(ctx, keyStoreCallerA, oldRecord.KeyID, next, callers.RotateOptions{})
	require.NoError(t, err)
	require.Equal(t, next.KeyID, result.Key.KeyID)
	require.Equal(t, keyStoreCallerA, result.Key.CallerID)
	require.Equal(t, next.KeyHash, result.Key.KeyHash)
	require.Equal(t, next.KeyPrefix, result.Key.KeyPrefix)
	require.Equal(t, next.Scopes, result.Key.Scopes)
	require.Nil(t, result.Key.ExpiresAt)
	require.Equal(t, callers.KeyStatusActive, result.Key.Status)
	require.True(t, result.Key.CreatedAt.Equal(keyStoreNow))
	require.Empty(t, result.ReboundTaskIDs)
	require.Empty(t, result.LeftBound)

	oldStored, err := store.LookupByID(ctx, keyStoreCallerA, oldRecord.KeyID)
	require.NoError(t, err)
	require.Equal(t, callers.KeyStatusRevoked, oldStored.Status, "旧 Key 已在同一事务内吊销")
	require.NotNil(t, oldStored.RevokedAt)
	require.True(t, oldStored.RevokedAt.Equal(keyStoreNow))

	var live int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM caller_keys WHERE caller_id = $1 AND revoked_at IS NULL`,
		keyStoreCallerA).Scan(&live))
	require.Equal(t, 1, live, "同一调用方此刻恰好一把未吊销 Key")

	auth := callers.NewAuthenticator(store, nil)
	_, err = auth.Authenticate(ctx, oldSecret)
	require.ErrorIs(t, err, callers.ErrUnauthorized, "旧明文轮换后立刻失效")
	principal, err := auth.Authenticate(ctx, newSecret)
	require.NoError(t, err)
	require.Equal(t, next.KeyID, principal.KeyID, "新明文可用且身份绑定新 Key")
}

// TestKeyStore_RotateRollsBackWhenNextConflicts：新 Key 复用已存在的指纹或标识时整个
// 事务回滚——旧 Key 保持可用，冲突的新行不残留。
// 中文验收点：任何一步失败整体回滚，不留下「新 Key 已发、旧 Key 未吊销」的中间态。
func TestKeyStore_RotateRollsBackWhenNextConflicts(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")

	old := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3L", keyStoreCallerA, keyStoreSecret("n"))
	require.NoError(t, store.Create(ctx, old))
	existing := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3M", keyStoreCallerA, keyStoreSecret("p"))
	require.NoError(t, store.Create(ctx, existing))

	// 新 Key 复用已存在的指纹：唯一约束让整个事务回滚。
	conflictHash := keyStoreRecord("key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3N", keyStoreCallerA, keyStoreSecret("p"))
	_, err := store.Rotate(ctx, keyStoreCallerA, old.KeyID, conflictHash, callers.RotateOptions{})
	require.ErrorIs(t, err, core.ErrConflict)

	oldStored, err := store.LookupByID(ctx, keyStoreCallerA, old.KeyID)
	require.NoError(t, err)
	require.Equal(t, callers.KeyStatusActive, oldStored.Status, "插入失败必须整体回滚：旧 Key 保持可用")
	require.Nil(t, oldStored.RevokedAt)

	_, err = store.LookupByID(ctx, keyStoreCallerA, conflictHash.KeyID)
	require.ErrorIs(t, err, callers.ErrNotFound, "冲突的新行不得残留")

	// 新 Key 复用已存在的标识同样冲突，旧 Key 仍可用。
	conflictID := keyStoreRecord(existing.KeyID, keyStoreCallerA, keyStoreSecret("q"))
	_, err = store.Rotate(ctx, keyStoreCallerA, old.KeyID, conflictID, callers.RotateOptions{})
	require.ErrorIs(t, err, core.ErrConflict)
	oldStored, err = store.LookupByID(ctx, keyStoreCallerA, old.KeyID)
	require.NoError(t, err)
	require.Equal(t, callers.KeyStatusActive, oldStored.Status, "第二次失败同样不得吊销旧 Key")
}

// TestKeyStore_RotateRejectsInvalidArgumentsAndIneligibleOldKey：缺标识 / 归属 / 指纹 /
// 前缀在触达 SQL 前拒绝；旧 Key 不存在按 ErrNotFound；已吊销的旧 Key 不能再轮换
// （Conflict），失败不得留下新行。
// 中文验收点：轮换不能把已失效的 Key "救活"，也不能在拒绝路径上产生写入。
func TestKeyStore_RotateRejectsInvalidArgumentsAndIneligibleOldKey(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-live", nil, nil, nil)
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-revoked", nil, nil, &keyStoreNow)

	next := keyStoreRecord("key-next", keyStoreCallerA, keyStoreSecret("r"))

	_, err := store.Rotate(ctx, "", "key-live", next, callers.RotateOptions{})
	require.ErrorIs(t, err, errKeyInvalidArgument)
	_, err = store.Rotate(ctx, keyStoreCallerA, "", next, callers.RotateOptions{})
	require.ErrorIs(t, err, errKeyInvalidArgument)

	missingID := next
	missingID.KeyID = ""
	_, err = store.Rotate(ctx, keyStoreCallerA, "key-live", missingID, callers.RotateOptions{})
	require.ErrorIs(t, err, errKeyInvalidArgument)

	missingHash := next
	missingHash.KeyHash = ""
	_, err = store.Rotate(ctx, keyStoreCallerA, "key-live", missingHash, callers.RotateOptions{})
	require.ErrorIs(t, err, errKeyInvalidArgument)

	missingPrefix := next
	missingPrefix.KeyPrefix = ""
	_, err = store.Rotate(ctx, keyStoreCallerA, "key-live", missingPrefix, callers.RotateOptions{})
	require.ErrorIs(t, err, errKeyInvalidArgument)

	_, err = store.Rotate(ctx, keyStoreCallerA, "key-missing", next, callers.RotateOptions{})
	require.ErrorIs(t, err, callers.ErrNotFound)

	_, err = store.Rotate(ctx, keyStoreCallerA, "key-revoked", next, callers.RotateOptions{})
	require.ErrorIs(t, err, core.ErrConflict, "已吊销的旧 Key 不能再轮换")
	_, err = store.LookupByID(ctx, keyStoreCallerA, next.KeyID)
	require.ErrorIs(t, err, callers.ErrNotFound, "拒绝路径不得留下新行")

	// 有效旧 Key 未被上述失败调用动过。
	live, err := store.LookupByID(ctx, keyStoreCallerA, "key-live")
	require.NoError(t, err)
	require.Equal(t, callers.KeyStatusActive, live.Status)
}

// TestKeyStore_RotateRebindsScheduledTasks：只有显式声明 `RebindScheduledTasks` 时才在
// 同一事务内改绑任务，且逐条判定归属与 scope——判不通过的任务进 `LeftBound` 并带原因
// 码，已退役任务不参与；未声明时连候选都不读。
// 中文验收点：不得静默改绑，也不得为让任务可绑定而放宽新 Key 的 scope。
func TestKeyStore_RotateRebindsScheduledTasks(t *testing.T) {
	covered := []string{core.ScopeSessionsRead, core.AgentInvokeScopePrefix + "daily"}

	t.Run("rebind_not_requested", func(t *testing.T) {
		db := keyStoreTestDB(t)
		store := NewKeyStore(db)
		ctx := context.Background()
		keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
		old := keyStoreRecord("key_old", keyStoreCallerA, keyStoreSecret("s"))
		require.NoError(t, store.Create(ctx, old))
		keyStoreSeedTask(t, db, "task-a", keyStoreCallerA, old.KeyID, "daily", false)

		next := keyStoreRecord("key_next", keyStoreCallerA, keyStoreSecret("t"), covered...)
		result, err := store.Rotate(ctx, keyStoreCallerA, old.KeyID, next, callers.RotateOptions{})
		require.NoError(t, err)
		require.Empty(t, result.ReboundTaskIDs, "未声明再绑定时连候选都不读")
		require.Empty(t, result.LeftBound, "未声明的任务不是逐条判失败，而是根本不参与")
		require.Equal(t, old.KeyID, keyStoreTaskKey(t, db, "task-a"),
			"任务保持旧绑定，下次触发按 owner_unauthorized fail-closed")
	})

	t.Run("rebind_reports_every_ineligible_task", func(t *testing.T) {
		db := keyStoreTestDB(t)
		store := NewKeyStore(db)
		ctx := context.Background()
		keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
		keyStoreSeedCaller(t, db, keyStoreCallerB, "service", "beta", "active")
		old := keyStoreRecord("key_old", keyStoreCallerA, keyStoreSecret("u"))
		require.NoError(t, store.Create(ctx, old))

		keyStoreSeedTask(t, db, "task-a-covered", keyStoreCallerA, old.KeyID, "daily", false)
		keyStoreSeedTask(t, db, "task-b-scope", keyStoreCallerA, old.KeyID, "ops", false)
		keyStoreSeedTask(t, db, "task-c-owner", keyStoreCallerB, old.KeyID, "daily", false)
		keyStoreSeedTask(t, db, "task-d-retired", keyStoreCallerA, old.KeyID, "daily", true)

		next := keyStoreRecord("key_next", keyStoreCallerA, keyStoreSecret("v"), covered...)
		result, err := store.Rotate(ctx, keyStoreCallerA, old.KeyID, next,
			callers.RotateOptions{RebindScheduledTasks: true})
		require.NoError(t, err)
		require.Equal(t, []string{"task-a-covered"}, result.ReboundTaskIDs)
		require.Equal(t, []callers.BoundTask{
			{TaskID: "task-b-scope", Reason: callers.BoundTaskScopeMissing},
			{TaskID: "task-c-owner", Reason: callers.BoundTaskCallerMismatch},
		}, result.LeftBound, "判不通过的任务逐条报出，不得静默改绑")

		require.Equal(t, next.KeyID, keyStoreTaskKey(t, db, "task-a-covered"), "scope 覆盖的任务换到新 Key")
		require.Equal(t, old.KeyID, keyStoreTaskKey(t, db, "task-b-scope"), "scope 不覆盖的任务保持旧绑定")
		require.Equal(t, old.KeyID, keyStoreTaskKey(t, db, "task-c-owner"), "归属不一致的任务保持旧绑定")
		require.Equal(t, old.KeyID, keyStoreTaskKey(t, db, "task-d-retired"), "已退役任务不参与再绑定")
	})
}

// TestKeyStore_ListProjectsCallersAndLiveKeyCount：调用方按 `caller_id` 升序；`service`
// 在存储边界映射成 `external`；内置管理员恒在；停用行仍可见；`KeyCount` 只数未吊销
// 的 Key（过期但未吊销的仍算未吊销）。
// 中文验收点：管理面列表是投影事实，词表映射只发生在存储边界。
func TestKeyStore_ListProjectsCallersAndLiveKeyCount(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()

	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
	keyStoreSeedCaller(t, db, keyStoreCallerB, "service", "beta", "disabled")
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-live", nil, nil, nil)
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-revoked", nil, nil, &keyStoreNow)
	keyStoreSeedKeyRow(t, db, keyStoreCallerB, "key-other", nil, nil, nil)

	records, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, records, 3, "迁移已种 local-admin，再加两个外部调用方")

	require.Equal(t, keyStoreCallerA, records[0].CallerID, "按 caller_id 升序")
	require.Equal(t, callers.CallerKindExternal, records[0].Kind, "service → external 只在此处映射")
	require.Equal(t, "alpha", records[0].Name)
	require.Equal(t, callers.CallerStatusActive, records[0].Status)
	require.Equal(t, 1, records[0].KeyCount, "KeyCount 只数未吊销的 Key")

	require.Equal(t, keyStoreCallerB, records[1].CallerID)
	require.Equal(t, "beta", records[1].Name)
	require.Equal(t, callers.CallerStatusDisabled, records[1].Status, "停用行仍在列表里")
	require.Equal(t, 1, records[1].KeyCount)

	require.Equal(t, core.LocalAdminCallerID, records[2].CallerID)
	require.Equal(t, callers.CallerKindBuiltin, records[2].Kind)
	require.Equal(t, 0, records[2].KeyCount)
}

// TestKeyStore_LookupCallerVisibilityAndKindMapping：按标识返回调用方行——停用是可见
// 状态而不是「不存在」；内置管理员映射成 builtin；KeyCount 只数未吊销 Key；空标识
// 与不存在都是 ErrNotFound。
// 中文验收点：管理面 404 只表示"没有这个调用方"，停用另有其状态。
func TestKeyStore_LookupCallerVisibilityAndKindMapping(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()

	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-live", nil, nil, nil)
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-revoked", nil, nil, &keyStoreNow)

	caller, err := store.LookupCaller(ctx, keyStoreCallerA)
	require.NoError(t, err)
	require.Equal(t, keyStoreCallerA, caller.CallerID)
	require.Equal(t, callers.CallerKindExternal, caller.Kind)
	require.Equal(t, "alpha", caller.Name)
	require.Equal(t, callers.CallerStatusActive, caller.Status)
	require.Equal(t, 1, caller.KeyCount)
	require.True(t, caller.CreatedAt.Equal(keyStoreNow))

	admin, err := store.LookupCaller(ctx, core.LocalAdminCallerID)
	require.NoError(t, err)
	require.Equal(t, callers.CallerKindBuiltin, admin.Kind)

	_, err = db.pool.ExecContext(ctx,
		`UPDATE callers SET status = 'disabled' WHERE caller_id = $1`, keyStoreCallerA)
	require.NoError(t, err)
	disabled, err := store.LookupCaller(ctx, keyStoreCallerA)
	require.NoError(t, err, "停用是可见状态，不是未找到")
	require.Equal(t, callers.CallerStatusDisabled, disabled.Status)

	_, err = store.LookupCaller(ctx, "cal_missing")
	require.ErrorIs(t, err, callers.ErrNotFound)
	_, err = store.LookupCaller(ctx, "  ")
	require.ErrorIs(t, err, callers.ErrNotFound)
}

// TestKeyStore_CreateCallerWritesOptionalLimitAtomically：`limit` 为 nil 时**不写**
// `caller_limits`（缺配置即拒绝，不落一份看起来已配置的默认数字）；给了限额时同一
// 事务写两行，空 Period / Timezone 按 DDL 缺省补齐（day / UTC）。
// 中文验收点：零限额表示"没有配置"而不是"不限量"，缺行与零值必须保持可区分。
func TestKeyStore_CreateCallerWritesOptionalLimitAtomically(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()

	withoutLimit := callers.CallerRecord{
		CallerID: keyStoreCallerA, Kind: callers.CallerKindExternal, Name: "no-limit",
		Status: callers.CallerStatusActive,
	}
	require.NoError(t, store.CreateCaller(ctx, withoutLimit, nil))
	stored, err := store.LookupCaller(ctx, keyStoreCallerA)
	require.NoError(t, err)
	require.True(t, stored.CreatedAt.Equal(keyStoreNow), "缺省 CreatedAt 用句柄时钟")
	require.Equal(t, callers.CallerKindExternal, stored.Kind)
	require.Zero(t, keyStoreCountLimits(t, db, keyStoreCallerA), "limit 为 nil 不得写 caller_limits")

	withLimit := callers.CallerRecord{
		CallerID: keyStoreCallerB, Kind: callers.CallerKindExternal, Name: "with-limit",
		Project: "proj", Status: callers.CallerStatusActive,
	}
	limit := callers.Limit{
		QPS: 1.5, Burst: 3, MaxConcurrent: 2,
		TasksPerPeriod: 10, TokensPerPeriod: 20, IngestBytesPerPeriod: 30,
	}
	require.NoError(t, store.CreateCaller(ctx, withLimit, &limit))

	var (
		qps                        float64
		burst, maxConcurrent       int
		tasks, tokens, ingestBytes int64
		period, timezone           string
	)
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		SELECT qps::float8, burst, max_concurrent, tasks_per_period, tokens_per_period,
		       ingest_bytes_per_period, period, timezone
		FROM caller_limits WHERE caller_id = $1`, keyStoreCallerB).
		Scan(&qps, &burst, &maxConcurrent, &tasks, &tokens, &ingestBytes, &period, &timezone))
	require.Equal(t, 1.5, qps)
	require.Equal(t, 3, burst)
	require.Equal(t, 2, maxConcurrent)
	require.Equal(t, int64(10), tasks)
	require.Equal(t, int64(20), tokens)
	require.Equal(t, int64(30), ingestBytes)
	require.Equal(t, "day", period, "空 Period 取 day")
	require.Equal(t, callers.DefaultTimezone, timezone, "空 Timezone 取 UTC")

	stored, err = store.LookupCaller(ctx, keyStoreCallerB)
	require.NoError(t, err)
	require.Equal(t, "proj", stored.Project)
}

// TestKeyStore_CreateCallerRejectsConflictsAndInvalidInput：缺标识 / 名称、非法种类 /
// 状态、以及管理面新建 builtin 都在写行之前拒绝；负限额在 Normalize 处拒绝；主键与
// `(project, name)` 重名冲突映射成 core.ErrConflict，且冲突事务整体回滚。
// 中文验收点：管理面不得新建内置调用方，冲突分类由存储层给出。
func TestKeyStore_CreateCallerRejectsConflictsAndInvalidInput(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()

	base := callers.CallerRecord{
		CallerID: keyStoreCallerA, Kind: callers.CallerKindExternal, Name: "alpha",
		Status: callers.CallerStatusActive,
	}
	invalid := []struct {
		name   string
		mutate func(*callers.CallerRecord)
	}{
		{"caller_id", func(r *callers.CallerRecord) { r.CallerID = "" }},
		{"name", func(r *callers.CallerRecord) { r.Name = "  " }},
		{"kind", func(r *callers.CallerRecord) { r.Kind = "robot" }},
		{"status", func(r *callers.CallerRecord) { r.Status = "sleeping" }},
		{"builtin", func(r *callers.CallerRecord) { r.Kind = callers.CallerKindBuiltin }},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			record := base
			tc.mutate(&record)
			require.ErrorIs(t, store.CreateCaller(ctx, record, nil), errKeyInvalidArgument)
		})
	}

	// 负限额在写任何行之前拒绝。
	err := store.CreateCaller(ctx, base, &callers.Limit{TokensPerPeriod: -1})
	require.Error(t, err)
	require.ErrorContains(t, err, "限额不得为负")

	require.NoError(t, store.CreateCaller(ctx, base, nil))

	// 主键冲突。
	duplicateID := base
	duplicateID.Name = "another"
	require.ErrorIs(t, store.CreateCaller(ctx, duplicateID, nil), core.ErrConflict)

	// `(project, name)` 唯一索引冲突；冲突事务整体回滚，限额行不得残留。
	sameName := base
	sameName.CallerID = keyStoreCallerB
	require.ErrorIs(t, store.CreateCaller(ctx, sameName, &callers.Limit{TokensPerPeriod: 1}), core.ErrConflict)
	require.Zero(t, keyStoreCountLimits(t, db, keyStoreCallerB), "冲突事务整体回滚，限额行不得残留")
}

// TestKeyStore_UsageWithoutLimitRow：调用方不存在 / 空标识都是 ErrNotFound；没有限额
// 行时返回零限额 + day / UTC 缺省（不是不限量）；周期起点由 `now` 折算，零值 `now`
// 取句柄时钟，同一周期内的任意瞬时得到同一视图；跨周期 pending 计入待核对条数。
// 中文验收点：缺配置与没这个调用方是两件事，存储层不虚构数字。
func TestKeyStore_UsageWithoutLimitRow(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()

	_, err := store.Usage(ctx, "cal_missing", 0, keyStoreNow)
	require.ErrorIs(t, err, callers.ErrNotFound)
	_, err = store.Usage(ctx, "  ", 0, keyStoreNow)
	require.ErrorIs(t, err, callers.ErrNotFound)

	require.NoError(t, store.CreateCaller(ctx, callers.CallerRecord{
		CallerID: keyStoreCallerA, Kind: callers.CallerKindExternal, Name: "alpha",
		Status: callers.CallerStatusActive,
	}, nil))

	periodStart := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC).UnixMilli()
	aggregate, err := store.Usage(ctx, keyStoreCallerA, 0, keyStoreNow)
	require.NoError(t, err)
	require.Equal(t, keyStoreCallerA, aggregate.CallerID)
	require.Equal(t, periodStart, aggregate.PeriodStart)
	require.Equal(t, callers.PeriodDay, aggregate.Limit.Period, "缺行按 day 折算")
	require.Equal(t, callers.DefaultTimezone, aggregate.Limit.Timezone, "缺行按 UTC 折算")
	require.Zero(t, aggregate.Limit.QPS)
	require.Zero(t, aggregate.Limit.TasksPerPeriod)
	require.Empty(t, aggregate.Ledger)
	require.Empty(t, aggregate.Classes)
	require.Zero(t, aggregate.PendingReservations)

	// 零值 now 取句柄时钟；同一周期内的任意瞬时折算到同一周期起点。
	same, err := store.Usage(ctx, keyStoreCallerA, 0, time.Time{})
	require.NoError(t, err)
	require.Equal(t, periodStart, same.PeriodStart)
	snapped, err := store.Usage(ctx, keyStoreCallerA, keyStoreNow.Add(3*time.Hour).UnixMilli(), keyStoreNow)
	require.NoError(t, err)
	require.Equal(t, periodStart, snapped.PeriodStart)

	// 上一周期的 pending 预留：当前周期账本与类别用量仍为空，但待核对条数是跨周期的。
	previous := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).UnixMilli()
	keyStoreSeedReservation(t, db, keyStoreReservationID("qres_tokens_", "01"), keyStoreCallerA,
		"idem-previous", "chat", previous, 5, nil, "pending")
	aggregate, err = store.Usage(ctx, keyStoreCallerA, 0, keyStoreNow)
	require.NoError(t, err)
	require.Equal(t, 1, aggregate.PendingReservations, "pending 条数跨周期统计")
	require.Empty(t, aggregate.Ledger, "当前周期没有账本行")
	require.Empty(t, aggregate.Classes, "上一周期的预留不计入本周期视图")
}

// TestKeyStore_UsageAggregatesPeriod：月周期按调用方时区的本地 1 日 00:00 起算；账本
// 行按 kind 升序照实读；类别用量按预留标识前缀拆账本——committed / reconciled 累加
// actual、pending 累加 amount、released 两者都不计；pending 条数跨周期。
// 中文验收点：用量视图是真实聚合，不是把预留行按类别"就近猜账本"。
func TestKeyStore_UsageAggregatesPeriod(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")

	shanghai, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err, "内嵌 tzdata 必须能解析调用方时区")
	periodStart := time.Date(2026, 10, 1, 0, 0, 0, 0, shanghai).UTC().UnixMilli()
	nextPeriod := time.Date(2026, 11, 1, 0, 0, 0, 0, shanghai).UTC().UnixMilli()

	// 限额：月周期、上海时区；Token 额度非零，其余维度为 0（本用例只看聚合，不看准入）。
	_, err = db.pool.ExecContext(ctx, `
		INSERT INTO caller_limits
			(caller_id, qps, burst, max_concurrent, tasks_per_period, tokens_per_period,
			 ingest_bytes_per_period, period, timezone, updated_at)
		VALUES ($1, 5.5, 10, 2, 100, 1000, 5000, 'month', 'Asia/Shanghai', $2)`,
		keyStoreCallerA, keyStoreNow.UnixMilli())
	require.NoError(t, err)

	// 账本行（与下面的预留互相印证）。
	keyStoreSeedLedger(t, db, keyStoreCallerA, periodStart, "tasks", 2, 0, 0)
	keyStoreSeedLedger(t, db, keyStoreCallerA, periodStart, "tokens", 130, 7, 2)
	keyStoreSeedLedger(t, db, keyStoreCallerA, periodStart, "ingest_bytes", 0, 50, 0)

	// 本周期：committed / reconciled 计 actual，pending 计 amount，released 两者都不计。
	actual30, actual2, actual100 := int64(30), int64(2), int64(100)
	keyStoreSeedReservation(t, db, keyStoreReservationID("qres_tokens_", "01"), keyStoreCallerA,
		"idem-chat-committed", "chat", periodStart, 40, &actual30, "committed")
	keyStoreSeedReservation(t, db, keyStoreReservationID("qres_tokens_", "02"), keyStoreCallerA,
		"idem-chat-pending", "chat", periodStart, 7, nil, "pending")
	keyStoreSeedReservation(t, db, keyStoreReservationID("qres_tokens_", "03"), keyStoreCallerA,
		"idem-chat-released", "chat", periodStart, 5, nil, "released")
	keyStoreSeedReservation(t, db, keyStoreReservationID("qres_tasks_", "01"), keyStoreCallerA,
		"idem-run-committed", "run", periodStart, 2, &actual2, "committed")
	keyStoreSeedReservation(t, db, keyStoreReservationID("qres_tokens_", "04"), keyStoreCallerA,
		"idem-ingest-reconciled", "ingest", periodStart, 90, &actual100, "reconciled")
	keyStoreSeedReservation(t, db, keyStoreReservationID("qres_ingest_bytes_", "01"), keyStoreCallerA,
		"idem-ingest-pending", "ingest", periodStart, 50, nil, "pending")

	// 下一个周期的 pending：计入待核对条数，但不进本周期视图。
	keyStoreSeedReservation(t, db, keyStoreReservationID("qres_tasks_", "02"), keyStoreCallerA,
		"idem-run-next", "run", nextPeriod, 1, nil, "pending")

	aggregate, err := store.Usage(ctx, keyStoreCallerA, 0, keyStoreNow)
	require.NoError(t, err)
	require.Equal(t, periodStart, aggregate.PeriodStart, "月周期按调用方时区的本地 1 日 00:00 起算")
	require.Equal(t, callers.PeriodMonth, aggregate.Limit.Period)
	require.Equal(t, "Asia/Shanghai", aggregate.Limit.Timezone)
	require.Equal(t, 5.5, aggregate.Limit.QPS)
	require.Equal(t, 1000, int(aggregate.Limit.TokensPerPeriod))

	require.Equal(t, []callers.LedgerUsage{
		{Kind: "ingest_bytes", Used: 0, Reserved: 50, Overage: 0},
		{Kind: "tasks", Used: 2, Reserved: 0, Overage: 0},
		{Kind: "tokens", Used: 130, Reserved: 7, Overage: 2},
	}, aggregate.Ledger, "账本按 kind 升序照实读")

	require.Equal(t, []callers.ClassUsage{
		{Class: callers.ClassChat, Ledger: "tokens", Used: 30, Reserved: 7},
		{Class: callers.ClassIngest, Ledger: "ingest_bytes", Used: 0, Reserved: 50},
		{Class: callers.ClassIngest, Ledger: "tokens", Used: 100, Reserved: 0},
		{Class: callers.ClassRun, Ledger: "tasks", Used: 2, Reserved: 0},
	}, aggregate.Classes, "committed/reconciled 计 actual，pending 计 amount，released 不计")
	require.Equal(t, 3, aggregate.PendingReservations, "pending 条数跨周期统计")

	// 同一周期内的任意瞬时得到同一视图。
	snapped, err := store.Usage(ctx, keyStoreCallerA, keyStoreNow.Add(-7*24*time.Hour).UnixMilli(), keyStoreNow)
	require.NoError(t, err)
	require.Equal(t, periodStart, snapped.PeriodStart)

	// 传入下一周期的瞬时：只看到该周期的账本与类别行。
	november, err := store.Usage(ctx, keyStoreCallerA, nextPeriod, keyStoreNow)
	require.NoError(t, err)
	require.Equal(t, nextPeriod, november.PeriodStart)
	require.Empty(t, november.Ledger)
	require.Equal(t, []callers.ClassUsage{
		{Class: callers.ClassRun, Ledger: "tasks", Used: 0, Reserved: 1},
	}, november.Classes)
	require.Equal(t, 3, november.PendingReservations, "pending 条数不随查看周期变化")
}

// TestKeyStore_UsageRejectsUnparseableReservationID：预留标识前缀解析不出账本时按数据
// 不一致报错，**不得**静默把它算到某一本账上。
// 中文验收点：账本维度编码在标识里，无法解析就是数据缺陷，不是"归零"。
func TestKeyStore_UsageRejectsUnparseableReservationID(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	require.NoError(t, store.CreateCaller(ctx, callers.CallerRecord{
		CallerID: keyStoreCallerA, Kind: callers.CallerKindExternal, Name: "alpha",
		Status: callers.CallerStatusActive,
	}, nil))

	periodStart := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC).UnixMilli()
	keyStoreSeedReservation(t, db, "hand-written-reservation", keyStoreCallerA,
		"idem-bad", "chat", periodStart, 1, nil, "pending")

	_, err := store.Usage(ctx, keyStoreCallerA, 0, keyStoreNow)
	require.ErrorIs(t, err, errQuotaInvalidReservationID)
	require.ErrorContains(t, err, "无法解析账本")
}

// TestKeyStore_UsageRejectsInvalidLimitRow：库里的时区不是合法 IANA 名时按配置缺陷
// 返回错误，**不得**静默退回 UTC。
// 中文验收点：周期边界靠时区名解析，坏数据必须暴露而不是被默认值掩盖。
func TestKeyStore_UsageRejectsInvalidLimitRow(t *testing.T) {
	db := keyStoreTestDB(t)
	store := NewKeyStore(db)
	ctx := context.Background()
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
	keyStoreSeedLimitRow(t, db, keyStoreCallerA, "day", "Mars/Olympus_Mons", 10)

	_, err := store.Usage(ctx, keyStoreCallerA, 0, keyStoreNow)
	require.Error(t, err)
	require.ErrorContains(t, err, "Mars/Olympus_Mons", "错误必须点名坏掉的时区")
}

// TestKeyStore_UnconfiguredHandleFailsClosed：没有数据库句柄时每个方法都拒绝服务，
// 不得退化成"空结果"或"未找到"。
// 中文验收点：装配错误失败关闭（宪法 V / 原则 2 的同一口径）。
func TestKeyStore_UnconfiguredHandleFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := NewKeyStore(nil)
	record := keyStoreRecord("key_x", keyStoreCallerA, keyStoreSecret("w"))
	next := keyStoreRecord("key_y", keyStoreCallerA, keyStoreSecret("x"))

	_, err := store.Lookup(ctx, record.KeyHash)
	require.ErrorIs(t, err, errKeyStoreUnconfigured)
	_, err = store.LookupByID(ctx, keyStoreCallerA, record.KeyID)
	require.ErrorIs(t, err, errKeyStoreUnconfigured)
	require.ErrorIs(t, store.Create(ctx, record), errKeyStoreUnconfigured)
	require.ErrorIs(t, store.SetStatus(ctx, keyStoreCallerA, record.KeyID, callers.KeyStatusRevoked), errKeyStoreUnconfigured)
	_, err = store.Rotate(ctx, keyStoreCallerA, record.KeyID, next, callers.RotateOptions{})
	require.ErrorIs(t, err, errKeyStoreUnconfigured)
	_, err = store.List(ctx)
	require.ErrorIs(t, err, errKeyStoreUnconfigured)
	_, err = store.LookupCaller(ctx, keyStoreCallerA)
	require.ErrorIs(t, err, errKeyStoreUnconfigured)
	require.ErrorIs(t, store.CreateCaller(ctx, callers.CallerRecord{
		CallerID: keyStoreCallerA, Kind: callers.CallerKindExternal, Name: "alpha",
		Status: callers.CallerStatusActive,
	}, nil), errKeyStoreUnconfigured)
	_, err = store.Usage(ctx, keyStoreCallerA, 0, keyStoreNow)
	require.ErrorIs(t, err, errKeyStoreUnconfigured)
}
