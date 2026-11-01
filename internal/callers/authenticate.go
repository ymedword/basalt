package callers

// 本文件实现调用方认证（US-16 §3.1.5 / 图 6 [0] / C-28）：管理 token 只经
// SHA-256 摘要做常量时间比对，映射到 `core.LocalAdminPrincipal()`；调用方 Key
// **只按指纹**查表（明文不查库、不落日志、不进错误），任何凭据失败一律
// `ErrUnauthorized`，不区分不存在 / 过期 / 撤销，避免成为探测面。
// This file implements caller authentication: the admin token is matched through a
// SHA-256 digest in constant time, caller keys are looked up by fingerprint only, and
// every credential failure is the indistinguishable ErrUnauthorized.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// ErrUnauthorized 是认证失败的唯一对外错误：不存在 / 已吊销 / 已过期 / 作用域
// 非法 / 调用方已停用一律映射到它。调用方**不得**据此区分原因——响应面与错误
// 文案完全相同，具体原因只允许留在存储层自己的日志里。
// ErrUnauthorized is the single external error of authentication: absent, revoked,
// expired, invalid scopes and disabled callers are indistinguishable by design.
var ErrUnauthorized = errors.New("callers: unauthorized")

// KeyStatus 是 Key 的生效状态，取自契约 `CallerKeySummary.status` 的闭集；
// 空值与未知值同样按不可用处理（默认拒绝）。
// KeyStatus is the key state from the contract's closed set; the zero value and
// unknown states are unusable as well.
type KeyStatus string

const (
	// KeyStatusActive 表示 Key 可用于认证。
	// KeyStatusActive means the key may authenticate requests.
	KeyStatusActive KeyStatus = "active"
	// KeyStatusRevoked 表示 Key 已被吊销（`caller_keys.revoked_at` 非空）。
	// KeyStatusRevoked means the key has been revoked.
	KeyStatusRevoked KeyStatus = "revoked"
	// KeyStatusExpired 表示 Key 已过期（`expires_at` 已过）。
	// KeyStatusExpired means the key has expired.
	KeyStatusExpired KeyStatus = "expired"
)

// KeyRecord 是 `caller_keys` 行在认证路径上的投影：只含查表与构造 Principal
// 所需的字段，**不含**明文（服务端本来也不存明文）。`ExpiresAt` 为 nil 表示
// 不过期；`Status` 由存储层按 `revoked_at` / `expires_at` / `callers.status`
// 投影，认证路径只接受 `KeyStatusActive`。
// KeyRecord is the authentication projection of a caller_keys row; it never carries
// plaintext. A nil ExpiresAt means no expiry, and only the active status authenticates.
type KeyRecord struct {
	// KeyID 是 Key 的稳定标识（`key_` 形式，进审计与 Principal）。
	KeyID string
	// CallerID 是归属调用方（配额、审计与会话归属都记它）。
	CallerID string
	// KeyHash 是明文 Key 的 SHA-256 十六进制指纹（小写）；查询键就是它。
	KeyHash string
	// KeyPrefix 是展示用前缀，不参与认证判定。
	KeyPrefix string
	// Scopes 是发放时冻结的 scope 集合。
	Scopes []string
	// ExpiresAt 是过期时刻；nil 表示不过期。
	ExpiresAt *time.Time
	// Status 是 Key 的生效状态。
	Status KeyStatus
	// CreatedAt 是签发时刻（契约 `CallerKeySummary.created_at` 必填）。
	// CreatedAt is the issue time, required by CallerKeySummary.
	CreatedAt time.Time
	// RevokedAt 是吊销时刻；nil 表示未吊销。契约要求吊销响应回填它，
	// 因此这里必须由存储层投影出真实值，**不**由调用方现编。
	// RevokedAt is the revocation time; the contract echoes it on revoke.
	RevokedAt *time.Time
}

// KeyStore 是调用方 Key 的存储端口（实现放 `internal/storage/postgres`）：
// 认证路径只消费 `Lookup`，生命周期（签发 / 轮换 / 吊销）消费其余方法。
// `Lookup` **只按 SHA-256 指纹**定位，**不得**接受明文；返回记录时由认证路径
// 自行判定状态与过期，存储层**不得**把已吊销 / 已过期的行静默当作命中。
// 调用方已停用（`callers.status = disabled`）时按「未找到」处理并返回错误，
// 认证路径对所有错误一视同仁地拒绝。
//
// `LookupByID` 按 `(caller_id, key_id)` 定位——**不匹配即「未找到」**，供调度 /
// 恢复核对原授权 Key（C-32）；`Create` / `SetStatus` / `Rotate` 只由 `KeyIssuer`
// 调用，`Rotate` **必须**在单个存储事务内完成「新建 + 吊销旧 Key + 按需再绑定任务」，
// 不保留双活窗口。
// KeyStore is the storage port of caller keys. Lookup is fingerprint-only; the
// lifecycle methods are called by KeyIssuer alone, and Rotate must create the new key,
// revoke the old one and optionally rebind tasks inside one transaction.
type KeyStore interface {
	// Lookup 按指纹返回 Key 记录；不存在 / 调用方停用时返回错误。
	Lookup(ctx context.Context, fingerprint string) (KeyRecord, error)

	// LookupByID 按 `(caller_id, key_id)` 返回 Key 记录；行不存在、归属不匹配或
	// 调用方停用时返回 `ErrNotFound`——调用方**不得**据错误区分这几种情形。
	LookupByID(ctx context.Context, callerID, keyID string) (KeyRecord, error)

	// Create 写入一条新 Key 行；`KeyHash` / `KeyPrefix` 原样落库，**不**接受明文
	// （`KeyRecord` 里本来也没有明文字段）。主键冲突返回错误。
	Create(ctx context.Context, record KeyRecord) error

	// SetStatus 把 `(caller_id, key_id)` 的 Key 置为给定状态；行不存在返回
	// `ErrNotFound`。重复置为同一状态幂等（吊销是唯一可表示的迁移）。
	SetStatus(ctx context.Context, callerID, keyID string, status KeyStatus) error

	// Rotate 在单个事务内新建 `next`、吊销 `oldKeyID`，并按 `opts` 把
	// `scheduled_tasks.authorizing_key_id` 由旧 Key 改为新 Key（仅限 scope 覆盖的
	// 行）；未能再绑定的任务逐条写进 `RotateResult.LeftBound`，**不得**静默改绑。
	Rotate(ctx context.Context, callerID, oldKeyID string, next KeyRecord, opts RotateOptions) (RotateResult, error)

	// List 返回调用方列表（`caller_id` 升序），内置管理员恒在；`KeyCount` 只数
	// 未吊销的 Key。它服务于管理面 `GET /callers`，**不**参与认证。
	// List returns callers in ascending caller_id order with live key counts.
	List(ctx context.Context) ([]CallerRecord, error)

	// LookupCaller 按标识返回调用方行；不存在返回 `ErrNotFound`（管理面 404）。
	// LookupCaller returns one caller row or ErrNotFound.
	LookupCaller(ctx context.Context, callerID string) (CallerRecord, error)

	// CreateCaller 在单个事务内写入调用方行与可选限额行（`limit` 为 nil 表示
	// **不写**限额——缺配置即拒绝，**不得**落一份看起来已配置的默认数字）。
	// 主键或 `(project, name)` 重名冲突返回 `core.ErrConflict`。
	// CreateCaller writes the caller and optional limit row atomically; a nil limit
	// stays missing rather than being filled with defaults.
	CreateCaller(ctx context.Context, record CallerRecord, limit *Limit) error

	// Usage 聚合某周期的用量视图：`periodStart` 为 0 时取当前周期，非 0 时把该
	// 瞬时折算到所属周期起点。`now` 由调用方显式传入——存储层不提供注入口。
	// Usage aggregates one period of usage; a zero periodStart resolves to the current
	// period and a non-zero one is snapped to its period start.
	Usage(ctx context.Context, callerID string, periodStart int64, now time.Time) (UsageAggregate, error)
}

// AdminAuth 提供**已解析**管理 token 的 SHA-256 指纹，供认证路径做常量时间
// 比对。`configured=false` 表示当前实例不以 token 形式提供管理认证
// （`local_trust`），此时字符串凭据**不得**被提升为管理员。
// AdminAuth supplies the fingerprint of the resolved admin token for constant-time
// comparison. When configured is false the token form is not in use, so no presented
// string may be promoted to administrator.
type AdminAuth interface {
	// AdminTokenFingerprint 返回管理 token 的 SHA-256 指纹；configured 为假时
	// fingerprint 无意义。
	AdminTokenFingerprint() (fingerprint [sha256.Size]byte, configured bool)
}

// Authenticator 把请求携带的字符串凭据解析成 `core.Principal`。它不持有明文
// token，也不提供任何「凭据存在即管理能力」的入口——管理能力只由启动期
// `core.AdminPosture` 表达（§2.14）。
// Authenticator resolves a presented credential into a core.Principal. It holds no
// plaintext and exposes no "credential exists means admin" accessor.
type Authenticator struct {
	keys  KeyStore
	admin AdminAuth
	// now 只用于过期判定；测试注入固定时钟，生产用 time.Now。
	now func() time.Time
}

// NewAuthenticator 构造认证器。`keys` 或 `admin` 为 nil 时对应凭据形态不可用
// （拒绝而非放行）；两者都不提供时每次认证都返回 ErrUnauthorized。
// NewAuthenticator builds an authenticator; a nil key store or admin source disables
// that credential form rather than granting access.
func NewAuthenticator(keys KeyStore, admin AdminAuth) *Authenticator {
	return &Authenticator{keys: keys, admin: admin, now: time.Now}
}

// Authenticate 按图 6 [0] 解析凭据：admin token 命中即返回内置管理员（Actor
// 固定 caller）；否则把字符串当调用方 Key，只按 SHA-256 指纹查表，校验状态、
// 过期与 scope 后返回调用方身份。所有失败一律 `ErrUnauthorized`。
// Authenticate resolves the credential: an admin token match yields the built-in
// administrator, otherwise the string is a caller key looked up by fingerprint and
// validated for status, expiry and scopes. Every failure is ErrUnauthorized.
func (a *Authenticator) Authenticate(ctx context.Context, presented string) (core.Principal, error) {
	if presented == "" {
		return core.Principal{}, ErrUnauthorized
	}
	digest := sha256.Sum256([]byte(presented))
	if a.adminMatches(digest) {
		return core.LocalAdminPrincipal(), nil
	}
	if a.keys == nil {
		return core.Principal{}, ErrUnauthorized
	}
	// 查表键是十六进制指纹：明文既不进 SQL 参数，也不进错误与日志。
	// The lookup key is the hex digest; the plaintext reaches neither SQL nor logs.
	fingerprint := hex.EncodeToString(digest[:])
	record, err := a.keys.Lookup(ctx, fingerprint)
	if err != nil {
		return core.Principal{}, ErrUnauthorized
	}
	if !recordUsable(record, fingerprint, a.now()) {
		return core.Principal{}, ErrUnauthorized
	}
	principal := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: record.CallerID,
		KeyID:    record.KeyID,
		Scopes:   append([]string(nil), record.Scopes...),
		Actor:    core.ActorCaller,
	}
	if !principal.Valid() {
		return core.Principal{}, ErrUnauthorized
	}
	return principal, nil
}

// adminMatches 只在管理 token 形态启用时做常量时间比对；未启用或未配置一律
// 返回假，**不得**把「没有管理 token」解释成「任意凭据都是管理员」。
// adminMatches compares in constant time only when the token form is configured.
func (a *Authenticator) adminMatches(digest [sha256.Size]byte) bool {
	if a.admin == nil {
		return false
	}
	fingerprint, configured := a.admin.AdminTokenFingerprint()
	if !configured {
		return false
	}
	return subtle.ConstantTimeCompare(digest[:], fingerprint[:]) == 1
}

// recordUsable 判定 Key 记录能否用于认证：状态必须 active、过期时刻必须仍在
// 未来、记录必须与查询指纹一致（存储层返回错行时不得放行）。字段缺失同样
// 拒绝——认证路径不做补默认值。
// recordUsable requires the active status, a future expiry and a matching hash;
// missing fields are rejected rather than defaulted.
func recordUsable(record KeyRecord, fingerprint string, now time.Time) bool {
	if record.Status != KeyStatusActive {
		return false
	}
	if record.KeyID == "" || record.CallerID == "" || record.KeyHash != fingerprint {
		return false
	}
	if record.ExpiresAt != nil && !record.ExpiresAt.After(now) {
		return false
	}
	return true
}
