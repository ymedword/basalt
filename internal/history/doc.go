// Package history 补齐历史使用面：把「检索未知位置」与「精确读取已知引用」分成两条路径，
// 供 web 端点与后续模型工具使用。
// Package history completes the history surface by separating search over unknown
// locations from exact reads of known references, serving the web endpoints and later
// model tools.
//
// **检索只是指针**：Search 返回 message_id / run_id / session_id / 片段与分数，
// **不得**当成引用参与 CitationReport——要引用就先精确读取并复核。
// Search returns pointers only; hits never become citations by themselves.
//
// **精确读取每次都复核当前 ACL、删除状态与版本**：读取通过不等于内容仍有效，越权跨调用方
// 一律按未找到处理，不泄露存在性。
// Exact reads re-check the current ACL, deletion state and version on every call, and
// cross-caller reads report not-found rather than leaking existence.
//
// **压缩引擎不经过本包**：它走 compaction.MessageReader 的会话顺序读取（按 seq、无相关性
// 排序、无游标分页），两者的输入与语义不同，**不得**合并成一条路径。
// The compaction engine does not use this package: it reads through
// compaction.MessageReader in session order, which is a different contract.
package history
