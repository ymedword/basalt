//go:build integration

package app

// 本文件是 C-07 的判卷（US-16：`TestLogsAndAudit_RedactKey`，`us16-pg` 具名用例）：
// 调用方 Key 的**明文只在签发响应出现一次**——库里只有指纹与前缀，日志、审计详情与
// 错误文案里都找不到它。
//
// 判法沿用泄露哨兵的既有纪律（`credential_leak_test.go`）：扫描对象是**整库全部表
// 全部列**与真实日志缓冲，不是"某个函数没返回 Key"；每条扫描都配阳性对照，证明扫描器
// 真的读到了这把 Key 的行——否则"零命中"只说明扫了个空库。
// The plaintext exists in exactly one place: the issuance response. The sweep covers every
// column of every table plus the live log buffer, each with a positive control.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// TestLogsAndAudit_RedactKey 判"签发 → 认证 → 运行"整条链上明文零出现。
//
// 逆改点：把明文写进 `caller_keys.key_hash` / 操作详情 → 库扫描红；签发或认证的失败
// 文案回显入参 → 错误文案断言红；把明文打进运行日志 → 日志扫描红。
// Reverting any one of the three surfaces turns exactly one sweep red.
func TestLogsAndAudit_RedactKey(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "runner")
	fx.override = &scriptedProviderClient{replies: []core.ChatResponse{{Content: "done"}}}
	rt := fx.assemble(t)

	ctx := context.Background()
	now := time.Now().UnixMilli()
	raw := openRawWrite(t, fx.dsn)
	_, err := raw.ExecContext(ctx, `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, '', 'active', $2)`, "caller-redact", now)
	require.NoError(t, err, "签发前调用方必须存在（caller_keys 有外键）")
	fx.seedCallerLimit(t, "caller-redact")

	// 走真实签发器：明文由它生成，同时落两阶段管理操作审计。
	keys := postgres.NewKeyStore(rt.db)
	issuer := callers.NewKeyIssuer(keys, postgres.NewManagementStore(rt.db))
	actor := core.Actor{
		Subject: core.ActorSubjectLocalTrust,
		Method:  core.AuthMethodLocalTrust,
		Source:  "127.0.0.1:1",
	}
	issued, err := issuer.Issue(ctx, actor, callers.IssueRequest{
		CallerID: "caller-redact",
		Scopes:   []string{"sessions:read", core.AgentInvokeScopePrefix + "runner"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, issued.Secret, "签发响应是明文 Key 唯一合法的出口")
	require.True(t, strings.HasPrefix(issued.Secret, issued.KeyPrefix),
		"前缀是明文的一段（展示用），但短到不可逆推")
	require.Less(t, len(issued.KeyPrefix), len(issued.Secret),
		"前缀只是明文的一段：两者长度相同就等于把 Key 明文留在库里")

	// ① 认证面：正确 Key 得到调用方身份，错误 Key 一律 `ErrUnauthorized` 且不回显入参。
	auth := callers.NewAuthenticator(keys, nil)
	principal, err := auth.Authenticate(ctx, issued.Secret)
	require.NoError(t, err)
	require.Equal(t, "caller-redact", principal.CallerID)
	require.Equal(t, issued.KeyID, principal.KeyID)

	_, wrongErr := auth.Authenticate(ctx, issued.Secret+"x")
	require.ErrorIs(t, wrongErr, callers.ErrUnauthorized, "多一位也算另一把 Key")
	require.NotContains(t, wrongErr.Error(), issued.Secret, "错误文案不得回显入参")

	// ② 运行面：用这把 Key 的身份真跑一次 Run，让审计行（runs / llm_calls）与运行日志
	//    都由生产路径写出来——扫描的对象因此不是空库与空缓冲。
	// The run writes the very audit rows and log lines the sweep reads.
	res, err := rt.service.Run(ctx, core.RunRequest{
		Principal: principal,
		Agent:     "runner",
		SessionID: "redact-key-run",
		Message:   "你好",
		Channel:   "cli",
	})
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)

	// ③ 阳性对照：库里确有这把 Key 的行（前缀与指纹可读），审计详情也记了它。
	var keyPrefix, detailJSON string
	require.NoError(t, raw.QueryRowContext(ctx,
		`SELECT key_prefix FROM caller_keys WHERE key_id = $1`, issued.KeyID).Scan(&keyPrefix))
	require.Equal(t, issued.KeyPrefix, keyPrefix, "阳性对照：扫描器必须真的读到这一行")
	require.NoError(t, raw.QueryRowContext(ctx, `
		SELECT detail_json FROM management_operations
		WHERE action = 'caller.key.issue' ORDER BY created_at DESC LIMIT 1`).Scan(&detailJSON))
	require.Contains(t, detailJSON, issued.KeyID, "阳性对照：审计详情必须记下 Key 标识")
	require.Contains(t, detailJSON, issued.KeyPrefix, "阳性对照：审计详情必须记下前缀")

	// ④ 扫描面：日志缓冲、整库、错误文案。
	require.NotEmpty(t, fx.logs.String(), "阳性对照：装配与运行必须真的写日志")
	assertNoLeak(t, "日志（装配 + 运行）", fx.logs.Bytes(), []string{issued.Secret})
	scanned := scanDatabaseForLeaks(t, rt.db, []string{issued.Secret})
	require.Greater(t, scanned, 100, "阳性对照：库扫描必须真的读到值")
	require.NotContains(t, detailJSON, issued.Secret, "审计详情不得携带明文")
}
