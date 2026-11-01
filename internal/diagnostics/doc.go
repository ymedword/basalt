// Package diagnostics 是结构化诊断层：诊断事件类型与字段契约、关联标识规则、脱敏与记录。
// Package diagnostics is the structured diagnostics layer: the event type and its field
// contract, correlation rules, redaction and recording.
//
// **诊断不是审计**：审计是 fail-closed 的权威记录（先写 pending 再发调用），诊断是**可
// 采样、可丢弃**的旁路——写失败不得阻塞业务，两者**不得**互相顶替。
// Diagnostics are not audit: audit is the fail-closed authoritative record, while
// diagnostics are sampled and droppable, so a write failure never blocks business and
// neither may stand in for the other.
//
// **关联锚点至少一个**：RequestID / RunID / OperationID / IngestJobID 之一必须存在，
// TaskID 只能作限定符——**不得**伪造 Run 来凑关联。
// At least one correlation anchor is required; TaskID only qualifies, and runs are never
// fabricated to satisfy correlation.
//
// **存储、查询与导出都脱敏**：事件中不得出现凭据、密钥、全文 URL 或未脱敏的用户数据。
// Redaction applies at storage, query and export alike.
package diagnostics
