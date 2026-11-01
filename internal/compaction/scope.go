package compaction

import (
	"crypto/sha256"
	"encoding/binary"
)

// 本文件定义压缩条目的**作用域**与 scope 级 advisory 锁键（设计 §3.1.11）。
// Scope 的两种形态互斥：有 `SessionID` 即会话范围（可跨该会话此前的 Run），
// 否则为 Run 范围（无状态 Run，消息只在内存、条目只作审计）。
// Scope plus the scope-level advisory-lock key. A session scope may span earlier runs of
// the same session; a run scope is a stateless run whose entries are audit-only.

// advisoryPrefix 是压缩追加锁的前缀串（设计 §3.1.11 冻结字面量）——与迁移的
// `basalt/schema-migrate` 分属不同键空间，避免压缩追加被迁移锁串行化。
// The fixed prefix keeps the compaction-append key space disjoint from the migration one.
const advisoryPrefix = "basalt/compaction-append"

// Scope 是一条压缩条目的作用域：`SessionID != ""` ⇒ 会话范围；否则 Run 范围（无状态）。
// 两者**不得**同时为空——空 scope 既定位不到覆盖范围，也取不到锁键。
// Scope is either a session scope or a (stateless) run scope, never both empty.
type Scope struct {
	SessionID string
	RunID     string
}

// Valid 判定 scope 可用：会话范围要求 `SessionID` 非空，Run 范围要求 `RunID` 非空。
// Valid reports whether the scope names a session or a run.
func (s Scope) Valid() bool { return s.SessionID != "" || s.RunID != "" }

// Canonical 返回 scope 的规范串（`session:<id>` 或 `run:<id>`），锁键与日志共用同一形态。
// Canonical is the scope's canonical string, shared by the lock key and the logs.
func (s Scope) Canonical() string {
	if s.SessionID != "" {
		return "session:" + s.SessionID
	}
	return "run:" + s.RunID
}

// scopeAdvisoryKey 把 scope 映射成 `pg_advisory_xact_lock` 的键：前缀串 + 规范串的
// sha256 前 8 字节按大端读成 `int64`。键**不落库**、不参与覆盖范围判定，只用于串行。
// 按两个 32 位半边拼接而不是直接 `int64(uint64)`：位型逐字一致，且不触发 gosec G115
// 的有符号溢出告警（与 `postgres.advisoryKey` 同一手法）。
// scopeAdvisoryKey derives the pg_advisory_xact_lock key from the scope; it is never
// persisted and never takes part in coverage decisions.
func scopeAdvisoryKey(scope Scope) int64 {
	sum := sha256.Sum256([]byte(advisoryPrefix + scope.Canonical()))
	hi := int64(binary.BigEndian.Uint32(sum[:4]))
	lo := int64(binary.BigEndian.Uint32(sum[4:8]))
	return hi<<32 | lo
}

// ScopeAdvisoryKey 是 `scopeAdvisoryKey` 的导出包装：存储适配层（`storage/postgres`）
// 实现 `CompactionStore.Append` 时必须取**同一把**锁，而锁键公式只能有一份。
// 适配层可 import 本包（设计 §3.1.1 依赖方向），领域包不反向 import 存储包。
// ScopeAdvisoryKey exposes the very same key to the storage adapter so the lock formula
// exists exactly once.
func ScopeAdvisoryKey(scope Scope) int64 { return scopeAdvisoryKey(scope) }
