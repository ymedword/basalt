package diagnostics

// 本文件是 T101 的落地形态（模块卡 §2.9 裁决 2）：导出半部在 B 内**不交付**，收窄为
// `internal/diagnostics/leak_scan_test.go` 的零凭据泄漏扫描，**不**创建
// `export_resume_test.go`。判的是同一条脱敏规则在每个外发面上都生效：
//
//  1. **事件层**：脏 attrs 经 `Redacted` 后按外发形态序列化，凭据、正文、嵌套结构与
//     明文地址一个都不许出现；同时白名单标量必须**仍在**——"全删了"也叫不泄漏，
//     那样判不出白名单是否还在工作。
//  2. **attrs 层**：`Redact` 的输出键集必须落在白名单内，两种地址形态（https 与带
//     userinfo 的 DSN）各自收敛成 `scheme://host`。
//  3. **审计引用层**：`table:id` 原样往返且不带凭据；非枚举表名被拒，审计引用不得
//     变成第二个自由文本通道。
//  4. **读面投影层**：`Reader.List` 对**存储里已经脏掉**的事件再脱敏一次（reader.go
//     第 3 条纪律：历史行 / 手工补数 / 迁移旧行都可能绕过写入路径）。
//
// 逆改点：把 `Reader.List` 里的 `event.Redacted(...)` 去掉 → ④ 红；拆掉 `sensitiveKey`
// 第二道闸 → ① 的 `api_key` / `authorization` 哨兵红、② 的键集断言红；把 `reduceURL`
// 换成原样返回 → ① 的 path / query 哨兵红、② 红；把白名单收窄成空集 → ① / ④ 的
// "白名单标量必须仍在"红。四类逆改各自只打红一组断言。
//
// The scan covers the event, attrs, audit reference and the read-side projection: no
// sentinel injected at any seam may survive, while whitelisted scalars must still come
// through so the verdict cannot be satisfied by dropping everything.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 以下是**合成哨兵**：串内自带 `LEAKCANARY` 标记，命中即一定是泄漏而不是误报；
// 它们不是任何真实服务的凭据。
// Synthetic canaries: a hit can only mean a leak, never a false positive.
const (
	leakCanaryKey      = "sk-live-LEAKCANARY-1f4c9a"
	leakCanaryBearer   = "Bearer eyJhbGciOiJIUzI1NiJ9.LEAKCANARY.9d2e"
	leakCanaryPasswd   = "pw-LEAKCANARY-7b31"
	leakCanaryBody     = "LEAKCANARY-用户正文：把这份材料转给外部"
	leakCanaryNested   = "LEAKCANARY-nested-secret"
	leakCanaryURLPath  = "/v1/chat/completions"
	leakCanaryURLQuery = "api_key=" + leakCanaryKey
	leakCanaryDSNQuery = "sslmode=disable"
)

// leakCanaries 是扫描清单：凭据、正文、嵌套结构里的秘密、地址的路径与查询串。
// leakCanaries is the scan list: credentials, content, nested secrets, path and query.
func leakCanaries() []string {
	return []string{
		leakCanaryKey,
		leakCanaryBearer,
		leakCanaryPasswd,
		leakCanaryBody,
		leakCanaryNested,
		leakCanaryURLPath,
		leakCanaryURLQuery,
		leakCanaryDSNQuery,
	}
}

// poisonAttrs 是"什么都往里塞"的脏 attrs：三个凭据形态的键、正文、嵌套结构、
// 两个地址形态，外加三个必须存活的白名单标量（存活断言防"全删了"假绿）。
// poisonAttrs carries every leak shape plus three whitelisted scalars that must survive.
func poisonAttrs() map[string]any {
	return map[string]any{
		"api_key":          leakCanaryKey,
		"authorization":    leakCanaryBearer,
		"password":         leakCanaryPasswd,
		"prompt":           leakCanaryBody,
		"request_body":     map[string]any{"text": leakCanaryNested},
		"url":              "https://api.example.com" + leakCanaryURLPath + "?" + leakCanaryURLQuery,
		"count":            3,
		"owner_caller_id":  "caller-leak-scan",
		"authorizing_kind": "caller-key",
	}
}

// leakPayload 按**外发形态**序列化：端点就是把事件编码成 JSON 后发给管理台，
// 内存结构干净不等于载荷干净。
// leakPayload serialises the outbound shape; a clean struct is not a clean payload.
func leakPayload(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return string(raw)
}

// requireNoLeak 逐条扫描哨兵；命中时把哨兵与所在面一起报出来，避免"有一处泄漏"
// 这种不可定位的失败。
// requireNoLeak scans every canary and names both the canary and the seam on a hit.
func requireNoLeak(t *testing.T, payload, where string) {
	t.Helper()
	for _, canary := range leakCanaries() {
		require.NotContains(t, payload, canary, "%s：%q 不得出现在外发载荷里", where, canary)
	}
}

// leakDirtyEvent 造一条**已经脏掉**的事件（脏 attrs + 合法审计引用），模拟绕过写入
// 路径的历史行。时间与标识固定，读数可复现。
// leakDirtyEvent builds a dirty event that looks like a row that bypassed the write path.
func leakDirtyEvent(t *testing.T) DiagnosticEvent {
	t.Helper()
	at := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	id, err := NewEventID(at)
	require.NoError(t, err)
	return DiagnosticEvent{
		EventID:       id,
		SchemaVersion: SchemaVersion,
		Kind:          KindLLMCall,
		OccurredAt:    at.UnixMilli(),
		Correlation:   Correlation{RunID: "run-leak-scan"},
		Actor:         ActorCaller,
		StepID:        "step-leak-scan",
		Seq:           1,
		Attrs:         poisonAttrs(),
		AuditRef:      &AuditRef{Table: "llm_calls", ID: "42"},
	}
}

// TestDiagnostics_LeakScanEventPayload 判事件层：脱敏后的外发载荷零哨兵，白名单
// 标量仍在，且入参没有被就地改写。
// TestDiagnostics_LeakScanEventPayload judges the event payload and the surviving scalars.
func TestDiagnostics_LeakScanEventPayload(t *testing.T) {
	dirty := leakDirtyEvent(t)
	clean := dirty.Redacted(nil)

	requireNoLeak(t, leakPayload(t, clean), "事件载荷")

	// 白名单标量必须仍在，否则"什么都丢"也能让上面的扫描变绿。
	require.Equal(t, 3, clean.Attrs["count"])
	require.Equal(t, "caller-leak-scan", clean.Attrs["owner_caller_id"])
	require.Equal(t, "caller-key", clean.Attrs["authorizing_kind"])
	require.Equal(t, "https://api.example.com", clean.Attrs[URLAttrKey],
		"地址收敛后只留 scheme://host")

	// 脱敏返回新映射：入参保持原样，写入路径的原事件不被悄悄改写。
	require.Equal(t, leakCanaryKey, dirty.Attrs["api_key"], "脱敏不得就地修改入参")
}

// TestDiagnostics_LeakScanAttrsWhitelist 判 attrs 层：每个 kind 的输出键集都落在白
// 名单内，两种地址形态各自收敛。
// TestDiagnostics_LeakScanAttrsWhitelist judges the key set for every kind and both URL shapes.
func TestDiagnostics_LeakScanAttrsWhitelist(t *testing.T) {
	redactor := NewWhitelistRedactor()

	// 逐 kind 过一遍：日后新增 per-kind 白名单时，同样受敏感词第二道闸约束。
	for _, kind := range allKinds {
		clean := redactor.Redact(kind, poisonAttrs())
		for key := range clean {
			require.Contains(t, commonAttrKeys, key,
				"kind %s 放行了白名单外的键 %q——新增键属契约可见变更", kind, key)
		}
		requireNoLeak(t, leakPayload(t, clean), "kind "+string(kind))
	}

	// https 形态：路径、查询串、片段全部丢弃。
	require.Equal(t,
		map[string]any{URLAttrKey: "https://api.example.com"},
		redactor.Redact(KindLLMCall, map[string]any{
			URLAttrKey: "https://api.example.com" + leakCanaryURLPath + "?" + leakCanaryURLQuery + "#frag",
		}))

	// DSN 形态：userinfo（含口令）与查询串丢弃，主机与端口保留——排障需要知道
	// "连的是哪台"，不需要知道"用什么口令连"。
	require.Equal(t,
		map[string]any{URLAttrKey: "postgres://db.internal.example:5432"},
		redactor.Redact(KindLLMCall, map[string]any{
			URLAttrKey: "postgres://basalt:" + leakCanaryPasswd + "@db.internal.example:5432/basalt?" + leakCanaryDSNQuery,
		}))

	// 非 https / 非分层形态（相对地址、mailto）直接丢弃，不猜它能不能外发。
	require.Empty(t, redactor.Redact(KindLLMCall, map[string]any{URLAttrKey: "mailto:ops@example.com"}))
	require.Empty(t, redactor.Redact(KindLLMCall, map[string]any{URLAttrKey: "/relative/only"}))

	// 非标量值一律丢弃：嵌套结构是夹带正文与凭据的常见通道。
	require.Empty(t, redactor.Redact(KindLLMCall, map[string]any{
		"count": map[string]any{"nested": leakCanaryNested},
	}))
}

// TestDiagnostics_LeakScanAuditRef 判审计引用层：`table:id` 往返不变、不带凭据，
// 且表名受封闭枚举约束，非枚举表名不得成为外发自由文本。
// TestDiagnostics_LeakScanAuditRef judges the audit reference and its closed table set.
func TestDiagnostics_LeakScanAuditRef(t *testing.T) {
	clean := leakDirtyEvent(t).Redacted(NewWhitelistRedactor())
	require.NotNil(t, clean.AuditRef, "脱敏不得丢掉审计引用——排障要能跳到权威记录")
	require.Equal(t, "llm_calls:42", clean.AuditRef.String())

	requireNoLeak(t, leakPayload(t, clean.AuditRef), "审计引用")

	reparsed, err := ParseAuditRef(clean.AuditRef.String())
	require.NoError(t, err)
	require.Equal(t, *clean.AuditRef, reparsed, "`table:id` 往返必须逐字不变")

	// 表名不在枚举内 → 拒绝；空主键 → 拒绝。两道都是"不许当自由文本用"。
	_, err = ParseAuditRef("sk-live-LEAKCANARY-1f4c9a:42")
	require.ErrorIs(t, err, ErrInvalidEvent, "非枚举表名必须拒绝")
	_, err = ParseAuditRef("llm_calls:")
	require.ErrorIs(t, err, ErrInvalidEvent, "空审计主键必须拒绝")
}

// leakEventSource 回放预置事件：本层判"读面自己脱敏"，与存储实现无关。
// leakEventSource replays preloaded events; the read-side redaction is the verdict.
type leakEventSource struct{ events []DiagnosticEvent }

// ListEvents 恒回放预置切片。
func (s leakEventSource) ListEvents(context.Context, Query) ([]DiagnosticEvent, error) {
	return s.events, nil
}

// TestDiagnostics_LeakScanReadPath 判读面投影层：存储里已经脏掉的事件，读出时仍被
// 脱敏（纵深防御），且白名单标量不被顺手丢掉。
// TestDiagnostics_LeakScanReadPath judges the read-side defence in depth.
func TestDiagnostics_LeakScanReadPath(t *testing.T) {
	reader := NewReader(leakEventSource{events: []DiagnosticEvent{leakDirtyEvent(t)}}, DefaultConfig(), nil)

	events, err := reader.List(context.Background(), Query{RunID: "run-leak-scan"})
	require.NoError(t, err)
	require.Len(t, events, 1)

	requireNoLeak(t, leakPayload(t, events), "读面投影")
	require.Equal(t, 3, events[0].Attrs["count"], "读面脱敏不得把白名单标量一起丢掉")
	require.Equal(t, "https://api.example.com", events[0].Attrs[URLAttrKey])
}
