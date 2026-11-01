// Package memory 承载长期记忆的**门面与旧载体导入源**：作用域解析与生效层回退、
// 按完整 `core.Scope` 分档的进程内写锁、分区解析、注入期超配额 WARN，以及 `db switch`
// 用的一次性只读导入（`MarkdownLegacySource` + `ImportLegacy`）。
// Package memory holds the long-term memory facade and the legacy import source: scope
// resolution, effective-layer fallback, the per-scope write lock, the section parser,
// the injection-time over-quota warning, and the one-shot read-only import used by
// `db switch`.
//
// 运行期唯一载体是 PostgreSQL（D-18，实现落在 `internal/storage/postgres`，经
// `core.LongTermMemoryStore` 端口注入）；markdown 文件只剩**一次性只读导入来源**这一个
// 用途，运行期既不回读也不双写（D-22）。
// The runtime carrier is PostgreSQL only (D-18). The markdown files survive solely as a
// one-shot import source (D-22): never read at runtime, never written back.
//
// **依赖方向**：本包的生产导入面只有 `core`、`workspace` 与标准库——PostgreSQL 载体经
// 端口注入，本包**不** import `internal/storage`；包级架构测试 `import_test.go` 守这一条
// （口径 26）。
// The production import surface is core + workspace + the standard library only; the
// PostgreSQL carrier arrives through the core port, never as a package dependency.
package memory
