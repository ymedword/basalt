package storage

// 本文件判卷 `instance_secrets` 与"现存托管密文枚举"的仓储面：scope 只放行 master_key、
// 主键冲突是"别人先赢"而不是覆盖、计数只算仍带密文的版本、adopt 用的枚举按
// (凭据 ID, 版本号) 稳定升序。判定逻辑（三项联立）在 app 层判，这里只判 SQL 面。
// These tests pin the instance identity record and the sealed-credential scan: the only
// legal scope, never-overwrite on conflict, and a stable enumeration order.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// instanceTestUID 是测试用的资源 UID（26 位 Crockford 载荷、无易混字符）。
const instanceTestUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"

// newInstanceSecretsStore 给每个用例一个独立的真库。
func newInstanceSecretsStore(t *testing.T) (*InstanceSecretsStore, *DB) {
	t.Helper()
	db := openTempDB(t)
	return NewInstanceSecretsStore(db), db
}

// recordFixture 造一条完整的初始化记录。
func recordFixture() InstanceSecretRecord {
	return InstanceSecretRecord{
		Scope:         "master_key",
		KeyID:         "mk-1",
		Fingerprint:   "0123456789abcdef",
		InitializedAt: policyNow,
		OperationID:   "op-init",
	}
}

// TestInstanceSecretsStore_LoadMissIsAnAnswer —— 未初始化返回 exists=false（不是错误）；
// scope 只接受 master_key，其它值在读写两侧都拒绝。
// 逆改点：去掉 scope 校验 → 迁移版本号被当加密身份写进同表，本用例红。
func TestInstanceSecretsStore_LoadMissIsAnAnswer(t *testing.T) {
	store, _ := newInstanceSecretsStore(t)
	ctx := context.Background()

	record, exists, err := store.Load(ctx, "master_key")
	require.NoError(t, err)
	require.False(t, exists)
	require.Equal(t, InstanceSecretRecord{}, record)

	_, _, err = store.Load(ctx, "schema_version")
	require.Error(t, err)
	require.Contains(t, err.Error(), "只允许 master_key")

	err = store.Insert(ctx, InstanceSecretRecord{
		Scope:         "schema_version",
		KeyID:         "mk-1",
		Fingerprint:   "0123456789abcdef",
		InitializedAt: policyNow,
		OperationID:   "op-init",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "只允许 master_key")
}

// TestInstanceSecretsStore_InsertShapeAndRoundTrip —— 关键字段缺一即拒（不落半截行）；
// 合法写入后按 UTC 毫秒读回，指纹与操作标识一字不差。
func TestInstanceSecretsStore_InsertShapeAndRoundTrip(t *testing.T) {
	store, _ := newInstanceSecretsStore(t)
	ctx := context.Background()

	cases := map[string]func(r *InstanceSecretRecord){
		"key_id":         func(r *InstanceSecretRecord) { r.KeyID = "" },
		"fingerprint":    func(r *InstanceSecretRecord) { r.Fingerprint = "" },
		"operation_id":   func(r *InstanceSecretRecord) { r.OperationID = "" },
		"initialized_at": func(r *InstanceSecretRecord) { r.InitializedAt = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run("reject_"+name, func(t *testing.T) {
			record := recordFixture()
			mutate(&record)
			require.Error(t, store.Insert(ctx, record), "%s 必填", name)
		})
	}

	record := recordFixture()
	require.NoError(t, store.Insert(ctx, record))

	loaded, exists, err := store.Load(ctx, "master_key")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "master_key", loaded.Scope)
	require.Equal(t, "mk-1", loaded.KeyID)
	require.Equal(t, "0123456789abcdef", loaded.Fingerprint)
	require.Equal(t, "op-init", loaded.OperationID)
	require.True(t, loaded.InitializedAt.Equal(policyNow))
	require.Equal(t, time.UTC, loaded.InitializedAt.Location())
}

// TestInstanceSecretsStore_ConflictNeverOverwrites —— 主键冲突映射为
// `ErrInstanceSecretExists`，既有行原样保留：记录是"谁先写谁算"的胜负凭据，覆盖等于
// 允许第二把主密钥接管第一把。
// 逆改点：改成 `INSERT OR REPLACE` / upsert → 第二把密钥悄悄接管，本用例红。
func TestInstanceSecretsStore_ConflictNeverOverwrites(t *testing.T) {
	store, _ := newInstanceSecretsStore(t)
	ctx := context.Background()
	require.NoError(t, store.Insert(ctx, recordFixture()))

	intruder := recordFixture()
	intruder.KeyID = "mk-2"
	intruder.Fingerprint = "fedcba9876543210"
	intruder.OperationID = "op-intruder"
	err := store.Insert(ctx, intruder)
	require.ErrorIs(t, err, ErrInstanceSecretExists)
	require.Contains(t, err.Error(), "master_key")

	loaded, exists, err := store.Load(ctx, "master_key")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "mk-1", loaded.KeyID, "既有行不得被覆盖")
	require.Equal(t, "0123456789abcdef", loaded.Fingerprint)
	require.Equal(t, "op-init", loaded.OperationID)
}

// seedSealedCredentials 直插两个凭据身份与三个版本（revoked 行密文为 NULL），
// 判 adopt 用的计数与逐条枚举。仅用于造出仓储 API 造不出的既成状态。
func seedSealedCredentials(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	_, err := db.write.ExecContext(ctx,
		`INSERT INTO management_operations
		   (operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
		    status, created_at)
		 VALUES ('op-cred', 'credential.create', 'credential', 'cred_a', 'token:abc123',
		         'bearer', '127.0.0.1:5000', 'success', ?)`, policyNow.UnixMilli())
	require.NoError(t, err)

	const insertCred = `INSERT INTO credentials
		(credential_id, owner_kind, owner_uid, owner_name, state, current_version,
		 revocation_epoch, key_id, created_at, updated_at, operation_id)
		VALUES (?, ?, ?, ?, 'active', ?, 0, 'mk-1', ?, ?, 'op-cred')`
	_, err = db.write.ExecContext(ctx, insertCred, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		"model_connection", instanceTestUID, "openai", 2, policyNow.UnixMilli(), policyNow.UnixMilli())
	require.NoError(t, err)
	_, err = db.write.ExecContext(ctx, insertCred, "cred_bbbbbbbbbbbbbbbbbbbbbbbbbb",
		"notify_channel", "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B", "alerts", 1,
		policyNow.UnixMilli(), policyNow.UnixMilli())
	require.NoError(t, err)

	const insertVersion = `INSERT INTO credential_versions
		(credential_id, version_no, state, key_id, target_binding, last4, nonce, ciphertext,
		 created_at, updated_at, operation_id)
		VALUES (?, ?, ?, 'mk-1', 'binding-hash', '1234', ?, ?, ?, ?, 'op-cred')`
	nonce := make([]byte, 12)
	_, err = db.write.ExecContext(ctx, insertVersion, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", 2,
		"active", nonce, []byte{0x02}, policyNow.UnixMilli(), policyNow.UnixMilli())
	require.NoError(t, err)
	_, err = db.write.ExecContext(ctx, insertVersion, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", 1,
		"superseded", nonce, []byte{0x01}, policyNow.UnixMilli(), policyNow.UnixMilli())
	require.NoError(t, err)
	_, err = db.write.ExecContext(ctx, insertVersion, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", 3,
		"revoked", nonce, nil, policyNow.UnixMilli(), policyNow.UnixMilli())
	require.NoError(t, err)
	_, err = db.write.ExecContext(ctx, insertVersion, "cred_bbbbbbbbbbbbbbbbbbbbbbbbbb", 1,
		"active", nonce, []byte{0x03}, policyNow.UnixMilli(), policyNow.UnixMilli())
	require.NoError(t, err)
}

// TestInstanceSecretsStore_CountAndEnumerateSealed —— 计数只算仍带密文的版本（撤销行
// 为 NULL 自然不计入）；枚举按 (凭据 ID, 版本号) 升序且只列带密文的行，解密材料
// （AAD 段 + 密文）逐字段读回。
// 逆改点：计数带上 revoked 行 / 枚举丢排序 → adopt 试解点位不可复现，本用例红。
func TestInstanceSecretsStore_CountAndEnumerateSealed(t *testing.T) {
	store, db := newInstanceSecretsStore(t)
	ctx := context.Background()

	count, err := store.CountManagedCiphertext(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, count)

	empty, err := store.SealedCredentials(ctx)
	require.NoError(t, err)
	require.Empty(t, empty)

	seedSealedCredentials(t, db)

	count, err = store.CountManagedCiphertext(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, count, "只算仍带密文的版本，revoked 的 NULL 不计入")

	sealed, err := store.SealedCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, sealed, 3)
	require.Equal(t, []string{
		"cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		"cred_bbbbbbbbbbbbbbbbbbbbbbbbbb",
	},
		[]string{sealed[0].CredentialID, sealed[1].CredentialID, sealed[2].CredentialID})
	require.Equal(t, []int{1, 2, 1}, []int{sealed[0].VersionNo, sealed[1].VersionNo, sealed[2].VersionNo})

	first := sealed[0]
	require.Equal(t, "model_connection", first.OwnerKind)
	require.Equal(t, instanceTestUID, first.OwnerUID)
	require.Equal(t, "mk-1", first.KeyID)
	require.Equal(t, "binding-hash", first.TargetBinding)
	require.Len(t, first.Nonce, 12)
	require.Equal(t, []byte{0x01}, first.Ciphertext)
}

// TestInstanceSecretsStore_ErrorPathsReportContext —— 池故障带上下文前缀返回
// （判定流程据此区分"未初始化"与"读不出来"）。
// 逆改点：错误被吞成 exists=false → 读故障被误判成新建实例，本用例红。
func TestInstanceSecretsStore_ErrorPathsReportContext(t *testing.T) {
	ctx := context.Background()

	t.Run("load", func(t *testing.T) {
		store, db := newInstanceSecretsStore(t)
		closeReadPool(t, db)
		_, _, err := store.Load(ctx, "master_key")
		require.Error(t, err, "读故障不得伪装成未初始化")
		require.Contains(t, err.Error(), "读取 instance_secrets 记录")
	})

	t.Run("count_and_enumerate", func(t *testing.T) {
		store, db := newInstanceSecretsStore(t)
		closeReadPool(t, db)
		_, err := store.CountManagedCiphertext(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "统计现存托管密文")
		_, err = store.SealedCredentials(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "列出现存托管密文")
	})

	t.Run("insert", func(t *testing.T) {
		store, db := newInstanceSecretsStore(t)
		closeWritePool(t, db)
		err := store.Insert(ctx, recordFixture())
		require.Error(t, err)
		require.False(t, errors.Is(err, ErrInstanceSecretExists), "池故障不是主键冲突")
		require.Contains(t, err.Error(), "写入 instance_secrets 记录")
	})
}
