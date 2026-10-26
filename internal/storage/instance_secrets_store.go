package storage

// 本文件是实例加密初始化记录（`instance_secrets`）的读写面：一张只有一行的身份表，
// 记下**这把主密钥是谁生成的、内容指纹是多少、由哪次操作写的**。判定实例是否就绪时
// 必须把本表与密钥文件、现存托管密文三项联立——只看文件是否存在会把"格式正确但内容
// 错误"的密钥判成就绪，那是代价最大的一种假成功。
// This file is the read/write surface of the instance encryption record: which key was
// generated, its fingerprint and the operation that wrote it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrInstanceSecretExists 表示该 scope 的初始化记录已存在（主键冲突）。
// 生成路径必须把它当作"别人已经赢了"，转入 adopt 而不是覆盖。
// ErrInstanceSecretExists marks the primary-key conflict; the generator must fall back to
// adoption instead of overwriting.
var ErrInstanceSecretExists = errors.New("instance_secrets 记录已存在")

// InstanceSecretRecord 是 `instance_secrets` 表的一行。`Fingerprint` 与主密钥文件的
// 指纹同源（`MasterKeyFingerprint`：sha256 前 16 位十六进制），用于跨三项比对。
// InstanceSecretRecord is one row of the instance identity table.
type InstanceSecretRecord struct {
	Scope         string
	KeyID         string
	Fingerprint   string
	InitializedAt time.Time
	OperationID   string
}

// SealedCredential 是"现存托管密文"的一条只读引用：解密所需的 AAD 段与密文材料，
// **不含**任何明文。adopt 分支必须逐条试解它，解不开就进恢复态。
// SealedCredential is one sealed credential reference used by the adopt verification.
type SealedCredential struct {
	CredentialID  string
	OwnerKind     string
	OwnerUID      string
	VersionNo     int
	KeyID         string
	TargetBinding string
	Nonce         []byte
	Ciphertext    []byte
}

// InstanceSecretsStore 是 `instance_secrets` 与"现存密文枚举"的只读 / 追加写入面。
// InstanceSecretsStore wraps the instance identity table and the sealed-credential scan.
type InstanceSecretsStore struct{ db *DB }

// NewInstanceSecretsStore 绑定到既有库句柄。
// NewInstanceSecretsStore binds the store to an existing handle.
func NewInstanceSecretsStore(db *DB) *InstanceSecretsStore { return &InstanceSecretsStore{db: db} }

// Load 读取某 scope 的初始化记录；未命中返回 `exists=false` 与空记录（不是错误——
// "未初始化"是判定表里合法的一格）。
// Load returns the record; a miss is a legal "uninitialized" answer, not an error.
func (s *InstanceSecretsStore) Load(ctx context.Context, scope string) (InstanceSecretRecord, bool, error) {
	if err := validateInstanceSecretScope(scope); err != nil {
		return InstanceSecretRecord{}, false, err
	}
	var (
		record        InstanceSecretRecord
		initializedAt int64
	)
	err := s.db.read.QueryRowContext(ctx,
		`SELECT scope, key_id, fingerprint, initialized_at, operation_id
		   FROM instance_secrets WHERE scope = ?`, scope,
	).Scan(&record.Scope, &record.KeyID, &record.Fingerprint, &initializedAt, &record.OperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return InstanceSecretRecord{}, false, nil
	}
	if err != nil {
		return InstanceSecretRecord{}, false, fmt.Errorf("读取 instance_secrets 记录 %q: %w", scope, err)
	}
	record.InitializedAt = time.UnixMilli(initializedAt).UTC()
	return record, true, nil
}

// Insert 追加初始化记录。主键冲突映射为 `ErrInstanceSecretExists`，**不**改写既有行：
// 记录是"谁先写谁算"的胜负凭据，覆盖等于允许第二把密钥接管第一把。
// Insert appends the record and never overwrites an existing one.
func (s *InstanceSecretsStore) Insert(ctx context.Context, record InstanceSecretRecord) error {
	if err := validateInstanceSecretScope(record.Scope); err != nil {
		return err
	}
	if record.KeyID == "" || record.Fingerprint == "" || record.OperationID == "" {
		return errors.New("写入 instance_secrets 记录: key_id / fingerprint / operation_id 均为必填")
	}
	if record.InitializedAt.IsZero() {
		return errors.New("写入 instance_secrets 记录: initialized_at 必填")
	}
	_, err := s.db.write.ExecContext(ctx,
		`INSERT INTO instance_secrets (scope, key_id, fingerprint, initialized_at, operation_id)
		 VALUES (?, ?, ?, ?, ?)`,
		record.Scope, record.KeyID, record.Fingerprint, record.InitializedAt.UnixMilli(), record.OperationID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: scope %q", ErrInstanceSecretExists, record.Scope)
		}
		return fmt.Errorf("写入 instance_secrets 记录 %q: %w", record.Scope, err)
	}
	return nil
}

// CountManagedCiphertext 统计现存托管密文的版本数（含已 superseded / retired 的保留密文）。
// 判定表只用它区分"有没有密文"，因此计数为 0 与非 0 的语义必须稳定：撤销过的行密文
// 为 NULL，自然不计入——那正是不需要 adopt 验证的状态。
// CountManagedCiphertext counts every version row that still carries ciphertext.
func (s *InstanceSecretsStore) CountManagedCiphertext(ctx context.Context) (int, error) {
	var count int
	err := s.db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM credential_versions WHERE ciphertext IS NOT NULL`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("统计现存托管密文: %w", err)
	}
	return count, nil
}

// SealedCredentials 按 (凭据 ID, 版本号) 升序列出全部现存密文的解密材料。adopt 分支
// 用它逐条试解；顺序固定让失败点位可复现。
// SealedCredentials lists every sealed version in a stable order for adopt verification.
func (s *InstanceSecretsStore) SealedCredentials(ctx context.Context) ([]SealedCredential, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT c.credential_id, c.owner_kind, c.owner_uid, v.version_no, v.key_id,
		        v.target_binding, v.nonce, v.ciphertext
		   FROM credential_versions v
		   JOIN credentials c ON c.credential_id = v.credential_id
		  WHERE v.ciphertext IS NOT NULL
		  ORDER BY c.credential_id ASC, v.version_no ASC`)
	if err != nil {
		return nil, fmt.Errorf("列出现存托管密文: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SealedCredential
	for rows.Next() {
		var sealed SealedCredential
		if err := rows.Scan(&sealed.CredentialID, &sealed.OwnerKind, &sealed.OwnerUID, &sealed.VersionNo,
			&sealed.KeyID, &sealed.TargetBinding, &sealed.Nonce, &sealed.Ciphertext); err != nil {
			return nil, fmt.Errorf("扫描现存托管密文: %w", err)
		}
		out = append(out, sealed)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历现存托管密文: %w", err)
	}
	return out, nil
}

// validateInstanceSecretScope 只放行 `master_key`：本表记的是实例加密身份，迁移版本
// 另有 `schema_version`，混用会让"回滚库版本"误伤密钥判定。
// validateInstanceSecretScope accepts exactly the master_key scope.
func validateInstanceSecretScope(scope string) error {
	if scope != "master_key" {
		return fmt.Errorf("instance_secrets scope %q 非法：只允许 master_key", scope)
	}
	return nil
}
