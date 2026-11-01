package diagnostics

// 本文件是脱敏规则的判卷用例（任务 T123 / 颗粒度文档 §2.9「脱敏与权限边界」）。
// These tests hold the redaction rule described by §2.9 of the design card.
//
// 逆改点：把白名单换成黑名单 → `TestDiagnostics_DeniedItemsLeakNoTitles` 与
// `TestRedact_DropsUnknownNonScalarAndSensitiveKeys` 红；`url` 原样保留 → 地址收敛
// 用例红；脱敏器 nil 时直接原样返回 → fail-safe 用例红。

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDiagnostics_DeniedItemsLeakNoTitles 断言被权限拒绝的资料一个字符都不进事件：
// 标题、文档 ID、片段、地址路径与查询串全部消失，只留计数与归属。
// 逆改点：白名单失效（改成"只删已知危险键"）→ attrs 出现文档名，最后一组断言红。
func TestDiagnostics_DeniedItemsLeakNoTitles(t *testing.T) {
	const (
		title     = "Q3 财报-绝密"
		document  = "doc_01JABCDEFGHJKMNPQRSTVWXYZ0"
		snippet   = "……正文片段……"
		secretURL = "https://kb.internal/docs/42?token=s3cr3t"
	)
	event := DiagnosticEvent{
		Kind:       KindPermissionDecision,
		Decision:   DecisionDeny,
		ReasonCode: ReasonPermissionDenied,
		Attrs: map[string]any{
			"document_title":  title,
			"document_id":     document,
			"chunk_text":      snippet,
			"url":             secretURL,
			"count":           3,
			"owner_caller_id": "cal_01JABCDEFGHJKMNPQRSTVWXYZ1",
		},
	}

	redacted := event.Redacted(nil)

	require.Equal(t, map[string]any{
		"count":           3,
		"owner_caller_id": "cal_01JABCDEFGHJKMNPQRSTVWXYZ1",
		"url":             "https://kb.internal",
	}, redacted.Attrs)

	encoded, err := json.Marshal(redacted.Attrs)
	require.NoError(t, err)
	for _, leaked := range []string{title, document, snippet, "token=s3cr3t", "/docs/42"} {
		require.NotContains(t, string(encoded), leaked, "脱敏后的 attrs 不得含 %q", leaked)
	}

	require.Len(t, event.Attrs, 6, "脱敏不得修改入参")
	require.Contains(t, event.Attrs, "document_title")
}

// TestRedact_URLKeepsSchemeHostOnly 断言地址只保留 scheme://host：路径、查询串、片段与
// 用户信息全部丢弃；缺 scheme 或 host 的形态直接丢弃。
// 逆改点：保留完整 URL 或只去掉 query → 前四条红；放行相对地址 / mailto → 后四条红。
func TestRedact_URLKeepsSchemeHostOnly(t *testing.T) {
	redactor := NewWhitelistRedactor()

	reduced := map[string]struct{ in, out string }{
		"path and query":    {"https://kb.internal/docs/42?q=1", "https://kb.internal"},
		"fragment":          {"http://kb.internal/page#top", "http://kb.internal"},
		"userinfo and port": {"https://user:pass@kb.internal:8443/x", "https://kb.internal:8443"},
		"root only":         {"https://kb.internal/", "https://kb.internal"},
	}
	for name, tc := range reduced {
		t.Run(name, func(t *testing.T) {
			out := redactor.Redact(KindRetrieval, map[string]any{URLAttrKey: tc.in})
			require.Equal(t, map[string]any{URLAttrKey: tc.out}, out)
		})
	}

	dropped := map[string]any{
		"relative":    "/docs/42",
		"opaque":      "mailto:ops@example.com",
		"empty":       "",
		"host-less":   "https://",
		"not a text":  42,
		"with space":  "https://kb .internal/x",
		"unparseable": "https://kb.internal/%zz",
	}
	for name, value := range dropped {
		t.Run("dropped/"+name, func(t *testing.T) {
			out := redactor.Redact(KindRetrieval, map[string]any{URLAttrKey: value})
			require.Empty(t, out, "非法地址必须整个丢弃，而不是留半截")
		})
	}
}

// TestRedact_DropsUnknownNonScalarAndSensitiveKeys 断言三类丢弃：白名单外的键、非标量值、
// 带凭据含义的键名；白名单内的标量按原值保留。
// 逆改点：放行嵌套结构 → 嵌套 / 切片子用例红；删除 sensitiveKey 闸 → 敏感键名子用例红。
func TestRedact_DropsUnknownNonScalarAndSensitiveKeys(t *testing.T) {
	redactor := NewWhitelistRedactor()
	attrs := map[string]any{
		"count":         5,
		"ok":            true,
		"score":         1.5,
		"unknown":       "值",
		"nested":        map[string]any{"title": "秘密"},
		"list":          []string{"a"},
		"nothing":       nil,
		"api_key":       "sk-live-0000",
		"authorization": "Bearer abc",
		"owner_token":   "tok",
	}
	out := redactor.Redact(KindRetrieval, attrs)
	require.Equal(t, map[string]any{"count": 5}, out)
}

// TestRedact_PerKindAttrWhitelist 守按 kind 收窄的增量白名单（V-8）：`tool` 只出现在
// 工具类事件上、`class` 只出现在配额判定上、`error_kind` 只出现在 `tool_call` 上；
// 共用集对每个 kind 仍然生效。
//
// 逆改点：把增量键并进共用集 → 每种事件都能读到不相干的键，对应子用例红；
// 删掉任一增量集 → 该类事件的属性被清空，对应用例红。
func TestRedact_PerKindAttrWhitelist(t *testing.T) {
	redactor := NewWhitelistRedactor()
	attrs := map[string]any{
		"tool":       "fs.read",
		"error_kind": "not_found",
		"class":      "chat",
		"count":      2,
	}

	require.Equal(t, map[string]any{
		"tool": "fs.read", "error_kind": "not_found", "count": 2,
	}, redactor.Redact(KindToolCall, attrs))

	require.Equal(t, map[string]any{
		"tool": "fs.read", "count": 2,
	}, redactor.Redact(KindPermissionDecision, attrs))

	require.Equal(t, map[string]any{
		"class": "chat", "count": 2,
	}, redactor.Redact(KindQuotaDecision, attrs))

	// 检索事件不带这三类增量键：串了事件种类的生产者在这里被暴露。
	require.Equal(t, map[string]any{"count": 2}, redactor.Redact(KindRetrieval, attrs))
}

// TestRedact_SensitiveKeyFragments 断言键名第二道闸本身：哪怕某个带凭据含义的键日后进了
// 白名单，也必须被丢弃。
// 逆改点：删除 sensitiveAttrFragments 或大小写不敏感比较 → 对应子用例红。
func TestRedact_SensitiveKeyFragments(t *testing.T) {
	for _, key := range []string{"api_key", "API-KEY", "access_token", "client_secret", "password", "credential", "Authorization", "cookie", "bearer"} {
		require.True(t, sensitiveKey(key), "%q 必须被判为敏感键", key)
	}
	for _, key := range []string{"owner_caller_id", "authorizing_kind", "count", "url"} {
		require.False(t, sensitiveKey(key), "%q 是白名单键，不得误伤", key)
	}
}

// TestRedact_IsIdempotentAndNeverMutates 断言脱敏两次与一次等价、入参不被修改，且结果
// 总是非 nil 空映射（落库是 NOT NULL DEFAULT '{}'）。
// 逆改点：直接改写入参 map → 入参断言红；返回 nil → 空映射断言红。
func TestRedact_IsIdempotentAndNeverMutates(t *testing.T) {
	redactor := NewWhitelistRedactor()
	attrs := map[string]any{"count": 1, "unknown": "x", URLAttrKey: "https://kb.internal/a?b=c"}

	once := redactor.Redact(KindAssembly, attrs)
	twice := redactor.Redact(KindAssembly, once)
	require.Equal(t, once, twice)
	require.Len(t, attrs, 3, "入参不得被修改")

	empty := redactor.Redact(KindAssembly, nil)
	require.NotNil(t, empty)
	require.Empty(t, empty)
}

// TestRedact_NilRedactorStillRedacts 断言"忘记注入脱敏器"是 fail-safe：默认实现仍然丢掉
// 白名单外的键，而不是原样放过。
// 逆改点：nil 分支改成直接返回原 attrs → 红。
func TestRedact_NilRedactorStillRedacts(t *testing.T) {
	event := DiagnosticEvent{
		Kind:  KindToolCall,
		Attrs: map[string]any{"document_title": "绝密", "count": 2},
	}
	out := event.Redacted(nil)
	require.Equal(t, map[string]any{"count": 2}, out.Attrs)
}

// TestAuditRef_RoundTrip 断言审计引用的存储形态 `table:id` 可无损往返，且表名封闭。
// 逆改点：允许任意表名 → 非法表子用例红；按最后一个冒号切分失败 → 往返断言红。
func TestAuditRef_RoundTrip(t *testing.T) {
	ref := AuditRef{Table: "llm_calls", ID: "42"}
	require.NoError(t, ref.Validate())
	require.Equal(t, "llm_calls:42", ref.String())

	parsed, err := ParseAuditRef(ref.String())
	require.NoError(t, err)
	require.Equal(t, ref, parsed)

	for name, text := range map[string]string{
		"unknown table": "runs:42",
		"no separator":  "llm_calls",
		"empty id":      "llm_calls:",
		"empty table":   ":42",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseAuditRef(text)
			require.ErrorIs(t, err, ErrInvalidEvent)
		})
	}
}
