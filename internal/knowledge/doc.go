// Package knowledge 是知识库的摄取、检索与引用解析：切分与版本行、向量与关键词双路检索
// 经 RRF 融合、引用解析与有效性校验、来源视图、以及上下文装配记录。
// Package knowledge covers ingestion, retrieval and citation resolution for the knowledge
// base: chunking and version rows, vector plus keyword retrieval fused by RRF, citation
// resolution and validity checks, the source view, and the context assembly records.
//
// **检索结果不是引用**：命中只带指针与片段，检索引擎结论**不得**直接进 CitationReport；
// 引用由引用解析按原文 span 复核后才成立。
// A hit is not a citation: retrieval returns pointers and snippets only, and a citation
// stands only after resolution re-checks the quoted span against the source.
//
// **单代际与生效点**：一次查询只用一个 active 索引代际，跨代际结果不得混排；新版本在索引
// 完成前不可见，删除与撤权在检索、引用、注入三处都要失效。
// One generation per query, never mixed; a new version stays invisible until indexed, and
// deletion or revocation must invalidate retrieval, citation and injection alike.
//
// **越权内容不进入 prompt**：身份与作用域在检索前判定，被拒片段既不出现在结果里，也不
// 出现在装配记录的可见字段里。
// Denied chunks never reach the prompt: authorization is decided before retrieval, and
// rejected chunks appear neither in results nor in the visible assembly record fields.
package knowledge
