package core

// 本文件是运行级配置快照的载体声明（技术方案 §11.5）：由 workspace 构造、storage 落库，
// 循环只消费。快照是"读即所记"的落点——注入内容与清单哈希出自同一次读取。
// This file declares the run-level config snapshot (Technical Solution §11.5): built by
// workspace, persisted by storage, consumed read-only by the loop.

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
