package diagnostics

// 本文件是诊断脱敏规则（颗粒度文档 §2.9「脱敏与权限边界」，任务 T123）：`attrs` 只保留
// 白名单键、只收标量值，地址只留 `scheme://host`。**存储 / 查询 / 导出三处必须同源**——
// 落库前脱敏一次，读出与导出各再走同一个入口，任何一条路径都不得自己拼一套规则。
// This file is the diagnostics redaction rule: attrs keep whitelisted scalar keys only
// and URLs keep their scheme and host. Storage, query and export share this one entry
// point so no path can drift into its own rule set.

import (
	"net/url"
	"strings"
	"time"
)

// URLAttrKey 是唯一允许承载地址的 attrs 键；值只保留 `scheme://host`。
// URLAttrKey is the only attribute that may carry an address; only scheme and host
// survive redaction.
const URLAttrKey = "url"

// commonAttrKeys 是所有 kind 共用的白名单键，每个键都有设计卡出处；新增键会经
// `GET /diagnostics` 外发，属契约可见变更，**不得**由实现随手加。
// commonAttrKeys is the whitelist shared by every kind. Each key has a design source;
// adding one is a contract-visible change that needs adjudication.
var commonAttrKeys = map[string]struct{}{
	"owner_caller_id":  {}, // 设计卡 :909——归属调用方
	"authorizing_kind": {}, // 设计卡 :909——授权来源
	"count":            {}, // 设计卡 :1219——被拒元素只允许计数
	URLAttrKey:         {}, // 设计卡 :1219——地址只存 scheme://host
}

// kindAttrKeys 是各 kind 的增量白名单（V-8：生产端补齐后按 kind 收窄）。三条取舍：
//   - 只加**能定位一次判定**的标识键（工具名 / 配额类目 / 失败分类），不加任何正文、
//     参数、结果或地址；描述性字段留在审计行（`audit_ref` 指向它），诊断不复制审计；
//   - 键按 kind 收窄而不是并入共用集：`tool` 出现在 `quota_decision` 上大概率是生产者
//     串了事件种类，丢弃比留着一个语义不明的键更能暴露接线错误；
//   - 键名必须能通过 `sensitiveKey` 的第二道闸（`error_kind` 不含凭据片段，`tool` 与
//     `class` 同理）。
//
// kindAttrKeys holds the per-kind additions. Only identifier-shaped keys that locate a
// single decision are admitted, and each is scoped to the kind that produces it.
var kindAttrKeys = map[Kind]map[string]struct{}{
	KindToolCall: {
		"tool":         {}, // 工具名（注册名，不是参数或结果）
		"tool_call_id": {}, // US-16A：模型实际返回的调用标识。
		"error_kind":   {}, // 失败分类（core 的七类错误，成功时缺省）
	},
	KindPermissionDecision: {
		"tool":         {}, // 被拒的工具名
		"tool_call_id": {}, // 有模型调用标识时保留，不补造。
	},
	KindQuotaDecision: {
		"class": {}, // 配额类目：run / chat / compaction / ingest / read
	},
}

// sensitiveAttrFragments 是键名的第二道闸：即使日后误把带凭据含义的键加进白名单，
// 也一律丢弃。白名单本身已不含这些键，这里只做纵深防御。
// sensitiveAttrFragments is the second gate on key names: a credential-shaped key is
// dropped even if it ever lands in the whitelist.
var sensitiveAttrFragments = []string{
	"key", "token", "secret", "password", "credential", "authorization", "cookie", "bearer",
}

// Redactor 把 attrs 收敛成白名单内的标量键值。
// Redactor narrows attrs into whitelisted scalar key-value pairs.
type Redactor interface {
	// Redact 返回脱敏后的新映射；入参**不被修改**。
	// Redact returns a fresh map and never mutates the input.
	Redact(kind Kind, attrs map[string]any) map[string]any
}

// WhitelistRedactor 是 v0 脱敏实现：白名单 + 标量值 + 地址收敛。
// WhitelistRedactor implements the v0 rule: whitelist, scalars and address reduction.
type WhitelistRedactor struct{}

// NewWhitelistRedactor 返回 v0 脱敏实现。
// NewWhitelistRedactor returns the v0 redactor.
func NewWhitelistRedactor() *WhitelistRedactor { return &WhitelistRedactor{} }

// Redact 丢弃未知键、非标量值与带凭据含义的键，并把 `url` 收敛成 `scheme://host`。
// 丢弃而不是报错：诊断可丢弃，一个不该出现的键不得阻断业务。
// Redact drops unknown keys, non-scalar values and credential-shaped keys, and reduces
// url to scheme://host. Dropping rather than failing keeps diagnostics droppable.
func (*WhitelistRedactor) Redact(kind Kind, attrs map[string]any) map[string]any {
	out := make(map[string]any, len(attrs))
	for key, value := range attrs {
		if !attrAllowed(kind, key) || sensitiveKey(key) {
			continue
		}
		if key == URLAttrKey {
			if reduced, ok := reduceURL(value); ok {
				out[key] = reduced
			}
			continue
		}
		if scalar, ok := scalarAttr(value); ok {
			out[key] = scalar
		}
	}
	if phase, ok := ReadPhase(kind, attrs); ok {
		out["phase"], out["phase_parent"], out["phase_status"] = phase.Name, phase.Parent, phase.Status
		out["duration_ms"], out["phase_end"] = phase.DurationMS, phase.End.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// Redacted 返回脱敏后的副本。`nil` 脱敏器回落到默认白名单实现，让"忘记注入"变成
// fail-safe（仍然脱敏）而不是 fail-open（原样落库）。
// Redacted returns a redacted copy. A nil redactor falls back to the whitelist
// implementation so a missing injection stays fail-safe instead of fail-open.
func (e DiagnosticEvent) Redacted(r Redactor) DiagnosticEvent {
	if r == nil {
		r = NewWhitelistRedactor()
	}
	e.Attrs = r.Redact(e.Kind, e.Attrs)
	return e
}

// attrAllowed 报告键是否属于该 kind 的白名单（共用集 ∪ 增量集）。
// attrAllowed reports whether the key is whitelisted for the kind.
func attrAllowed(kind Kind, key string) bool {
	if _, ok := commonAttrKeys[key]; ok {
		return true
	}
	_, ok := kindAttrKeys[kind][key]
	return ok
}

// sensitiveKey 报告键名是否带凭据含义（不区分大小写）。
// sensitiveKey reports whether the key name looks like a credential.
func sensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range sensitiveAttrFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// scalarAttr 只放行标量与字符串；map / 切片 / nil 一律丢弃——嵌套结构是夹带正文与
// 凭据的常见通道，`attrs` 不需要它们。
// scalarAttr admits scalars and strings only; maps, slices and nil are dropped because
// nested structures are the usual way content and credentials sneak in.
func scalarAttr(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return v, true
	case int, int8, int16, int32, int64:
		return v, true
	case uint, uint8, uint16, uint32, uint64:
		return v, true
	case float32, float64:
		return v, true
	default:
		return nil, false
	}
}

// reduceURL 把地址收敛成 `scheme://host`：路径、查询串、片段与用户信息全部丢弃；
// 解析失败、缺 scheme 或缺 host（相对地址、mailto 一类 opaque 形态）直接丢弃。
// reduceURL keeps scheme and host only; anything else, or an address without both
// parts, is dropped.
func reduceURL(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	parsed, err := url.Parse(text)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	return parsed.Scheme + "://" + parsed.Host, true
}
