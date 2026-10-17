// Package memory 承载长期记忆的**门面与 markdown 载体**：作用域解析与生效层回退、
// 按完整 `core.Scope` 分档的进程内写锁、两档载体共用的分区解析、注入期超配额 WARN。
// 另一档载体（sqlite）落在 `internal/storage`，与 markdown 档跑**同一组**语义断言；
// 换档 = 改 `memory.backend` 一行 + 重启，本包对此零感知。
// Package memory holds the long-term memory facade and the markdown carrier: scope
// resolution, effective-layer fallback, the per-scope write lock, the section parser
// shared by both carriers, and the injection-time over-quota warning.
//
// **依赖方向**：本包的生产导入面只有 `core`、`workspace` 与标准库——sqlite 载体经
// `core.LongTermMemoryStore` 端口注入，本包**不** import `internal/storage`；包级架构
// 测试 `import_test.go` 守这一条（口径 26）。mem0 档延后，取值域外即拒绝启动
// （Compatibility §4 #2）。
// The production import surface is core + workspace + the standard library only; the
// sqlite carrier arrives through the core port, never as a package dependency.
package memory
