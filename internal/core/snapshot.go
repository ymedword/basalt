package core

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

// 本文件是运行级配置快照的载体声明（技术方案 §3.6）：由 workspace 构造、storage 落库，
// 循环只消费。快照是"读即所记"的落点——注入内容与清单哈希出自同一次读取。
// This file declares the run-level config snapshot (Technical Solution §11.5): built by
// workspace, persisted by storage, consumed read-only by the loop.
//
// BuildConfigSnapshot 是**发布版本快照**的唯一构建入口（US-11 T026）：workspace 读到
// 工作副本后仍走它组装，准入侧按发布版本重读技能目录时也走它——两条来源共用同一段
// 排序 / 序列化 / 哈希代码，哈希算法只有一份（清单按 Path 升序后 JSON 序列化取 sha256）。
// BuildConfigSnapshot is the single assembly entry for a published-version snapshot: the
// workspace loader and the admission-side rebuild both go through it, so the manifest hash
// algorithm exists exactly once.

// ConfigSnapshot 是一次运行冻结的配置快照：运行开始**前**落库并冻结，磁盘上的
// AGENT.md 此刻无论怎么改，都不会让这次运行中途换配置（生命周期 = 每次运行一份，
// 不缓存、不复用）。
// ConfigSnapshot is the config frozen for one run: persisted and frozen before the run
// starts, one instance per run, never cached or reused.
type ConfigSnapshot struct {
	AgentName  string
	Hash       string // manifest_json **规整序列化**的 sha256；当前时间**不得**混入（同内容不同时刻同哈希）
	Content    string // AGENT.md 全文（含 frontmatter；人工可读的便捷副本——不能只存哈希）
	Manifest   []ManifestEntry
	SectionOne string   // 段一正文（系统提示词）；循环**不重读**盘——读即所记
	Profile    *Profile // 与 SectionOne 出自**同一次读取**；**不得**与旧的 entry.Profile 混用
}

// ManifestEntry 是冻结清单里的一条：文件相对路径 + 内容哈希。
// **JSON 键名是小写 `path` / `sha256`**（契约 DDL 的列注释把这列的形状写成 `[{path, sha256}]`）：
// `hash` 由 `manifest_json` 的**同一份序列化字节**派生（"二者永不脱钩"），而序列化请求由
// 两个包各自发出——标签写在这里，两侧 `json.Marshal` 才必然同形。
// ManifestEntry is one frozen manifest row. The JSON tags are part of the contract: the
// hash is derived from the same bytes that go into the manifest_json column, and both
// packages marshal independently — the tags are what keeps the two byte-identical.
type ManifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// BuildConfigSnapshot 组装一次运行的配置快照：清单按 Path 升序后规整序列化为
// `manifest_json` 的字节，`Hash` 是这串字节的 sha256（小写十六进制）。调用方保证
// 内容与清单出自**同一次读取**；本函数不改写入参（克隆后再排序）。
// BuildConfigSnapshot assembles one run's config snapshot: the manifest is sorted by path,
// serialized canonically, and hashed with sha256; the input slice is never mutated.
func BuildConfigSnapshot(agentName string, content []byte, sectionOne string, profile *Profile, manifest []ManifestEntry) (ConfigSnapshot, error) {
	sorted := slices.Clone(manifest)
	if sorted == nil {
		// nil 与空清单必须序列化到同一串字节（`[]` 而不是 `null`）：同一份"没有文件"
		// 的事实不得因切片的构造方式得到两个哈希。
		sorted = []ManifestEntry{}
	}
	slices.SortFunc(sorted, func(a, b ManifestEntry) int { return cmp.Compare(a.Path, b.Path) })
	raw, err := json.Marshal(sorted)
	if err != nil {
		return ConfigSnapshot{}, fmt.Errorf("序列化冻结清单（agent=%s）: %w", agentName, err)
	}
	sum := sha256.Sum256(raw)
	return ConfigSnapshot{
		AgentName:  agentName,
		Hash:       hex.EncodeToString(sum[:]),
		Content:    string(content),
		Manifest:   sorted,
		SectionOne: sectionOne,
		Profile:    profile,
	}, nil
}
