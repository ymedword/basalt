//go:build integration

package callers_test

// 本文件是 C-07「API Key 不得可恢复」的**落库与撤销**判卷（`us16-pg` 具名用例
// `TestAPIKey_NoPlaintextAtRestAndRevoke`）：签发走生产签发器、撤销走生产撤销、
// 认证走生产认证器；断言读的是**库里的行**与认证器的回答，不是签发函数的返回值。
//
// 与 `caller_key_redact_test.go`（`internal/app`）的分工：那边判"整库 + 日志 + 错误
// 文案"的扫描面与运行期传导，需要完整装配；这边判签发器自身的不变量——指纹等于
// 独立算出的 SHA-256、整行不得出现明文、撤销后同一把 Key 立刻失效。两份都留：
// 前者能发现"别处漏写"，后者能在不装配运行面的前提下定位到签发这一步。
// The issuer's own invariants: fingerprint, row-level secrecy, immediate revocation.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// TestAPIKey_NoPlaintextAtRestAndRevoke 判"库里只有指纹与前缀，撤销立即生效"。
//
// 逆改点：把 `key_hash` 写成明文 → SHA-256 断言与整行 NotContains 同时红；把前缀
// 换成整把明文 → 长度断言红；撤销只写 `status` 而认证路径仍查得到 → 最后一步红。
// Reverting any one of the three turns exactly one verdict red.
func TestAPIKey_NoPlaintextAtRestAndRevoke(t *testing.T) {
	db := pgtest.NewTestDB(t)
	keys := postgres.NewKeyStore(db)
	issuer := callers.NewKeyIssuer(keys, postgres.NewManagementStore(db))
	auth := callers.NewAuthenticator(keys, nil)
	ctx := t.Context()

	// 签发前调用方必须存在：`caller_keys.caller_id` 有外键。
	// The caller row must exist first; caller_keys has a foreign key.
	_, err := db.ExecContext(ctx, `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, '', 'active', $2)`, "c1", time.Now().UnixMilli())
	require.NoError(t, err)

	actor := core.Actor{
		Subject: core.ActorSubjectLocalTrust,
		Method:  core.AuthMethodLocalTrust,
		Source:  "127.0.0.1:1",
	}
	issued, err := issuer.Issue(ctx, actor, callers.IssueRequest{
		CallerID: "c1",
		Scopes:   []string{"sessions:write"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, issued.Secret, "签发响应是明文唯一的合法出口")

	// ① 落库形态：整行序列化后不得包含明文；指纹等于**独立算出**的 SHA-256
	//    （不是拿实现自己的函数去核对自己）。
	// The fingerprint is recomputed here rather than read back from the implementation.
	var rowText, hash, prefix string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT to_jsonb(k)::text, key_hash, key_prefix FROM caller_keys k WHERE key_id = $1`,
		issued.KeyID).Scan(&rowText, &hash, &prefix))
	sum := sha256.Sum256([]byte(issued.Secret))
	require.Equal(t, hex.EncodeToString(sum[:]), hash, "指纹必须是明文的 SHA-256（小写十六进制）")
	require.NotContains(t, rowText, issued.Secret, "库里的整行都不得出现明文")
	require.True(t, strings.HasPrefix(issued.Secret, prefix), "前缀是明文的一段（展示用）")
	require.Less(t, len(prefix), len(issued.Secret),
		"前缀必须短于明文：等长等于把 Key 明文留在库里")

	// ② 管理审计详情同样不得携带明文（阳性对照证明这一行真的被读到）。
	// The audit detail is a second place a plaintext could leak into.
	var opText string
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT to_jsonb(m)::text FROM management_operations m
		WHERE action = 'caller.key.issue' ORDER BY created_at DESC LIMIT 1`).Scan(&opText))
	require.Contains(t, opText, issued.KeyID, "阳性对照：审计详情必须记下 Key 标识")
	require.NotContains(t, opText, issued.Secret, "审计详情不得携带明文")

	// ③ 撤销即失效：同一把明文立刻认证失败，且失败原因不细分。
	// Revocation takes effect on the very next authentication.
	principal, err := auth.Authenticate(ctx, issued.Secret)
	require.NoError(t, err)
	require.Equal(t, "c1", principal.CallerID)
	require.Equal(t, issued.KeyID, principal.KeyID)

	require.NoError(t, issuer.Revoke(ctx, actor, callers.RevokeRequest{
		CallerID: "c1",
		KeyID:    issued.KeyID,
	}))
	_, err = auth.Authenticate(ctx, issued.Secret)
	require.ErrorIs(t, err, callers.ErrUnauthorized, "撤销后同一 Key 立即失效")

	// 撤销是 `revoked_at` 时刻，不是 `status` 列（契约 `schema.pg.sql:911` 的列就是这五个）。
	// Revocation is the revoked_at instant, not a status column.
	var revokedAt *int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT revoked_at FROM caller_keys WHERE key_id = $1`, issued.KeyID).Scan(&revokedAt))
	require.NotNil(t, revokedAt, "撤销必须落行，不只是内存里的标记")
}
