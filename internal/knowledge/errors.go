package knowledge

// 本文件声明摄取侧的错误分类与**冻结字面量**（US-16 §2.3 图 3 / T034）。
//
// `error_kind` 的六个取值逐字取自冻结契约 `docs/contracts/openapi.yaml` 的
// `IngestResult.error_kind` / `DocumentVersionSummary.error_kind`：
// `path_rejected` / `decode_failed` / `parser_failed` / `chunker_failed` /
// `embed_failed` / `verify_failed`。摄取**不得**发明第七个取值——HTTP 面把
// `error_kind` 直接映射成管理台文案，多一个值就是界面上的空洞；契约要改先改契约。
//
// 方向约定：**准入期**的拒绝（路径、配额）返回错误、零写入；**作业开始后**的失败
// 一律以 `IngestResult{Status: failed}` 正常返回（契约 `POST .../documents` 的 200
// 语义），只有基础设施错误才同时上抛。
//
// This file pins the six frozen `error_kind` literals and the admission sentinels.
// Admission rejections return errors with zero writes; failures after a job starts
// come back as a failed IngestResult, because the contract returns 200 for both.

import (
	"errors"
)

var (
	// ErrIngestUnavailable 表示装配缺端口或配置不完整：请求在读取正文与任何写入
	// 之前被拒绝，**不得**用默认值把缺失配置补成可用。
	// ErrIngestUnavailable reports a missing port or incomplete config; the request
	// is rejected before reading any content or writing anything.
	ErrIngestUnavailable = errors.New("knowledge: ingest is not configured")

	// ErrPathRejected 表示来源路径不合规（空路径、扩展名不在允许集合、超过大小上限）。
	// 工作区根与真实路径投影由 `DocumentReader` 负责，本错误只覆盖领域规则。
	// ErrPathRejected covers the domain-level path rules; real-path projection and
	// workspace-root checks belong to the DocumentReader implementation.
	ErrPathRejected = errors.New("knowledge: source path rejected")

	// ErrInvalidRequest 表示请求形状非法（删除时两个目标同时给出或都未给出）。
	// ErrInvalidRequest reports a malformed request shape.
	ErrInvalidRequest = errors.New("knowledge: invalid request")

	// ErrNotFound 表示文档或集合不存在 / 已软删。
	// ErrNotFound reports a missing or soft-deleted document or collection.
	ErrNotFound = errors.New("knowledge: not found")

	// ErrRetrieverUnavailable 表示检索器装配不可用：端口缺一、参数为负，或配置要求
	// 尚未交付的重排。检索在解析范围与任何查询之前被拒绝，**不得**降级成"空结果"
	// 让调用方以为检索过。
	// ErrRetrieverUnavailable reports an unusable retriever assembly: a missing port, a
	// negative parameter, or a configured-but-undelivered rerank. Retrieval is rejected
	// before scope resolution and never degrades into a misleading empty result.
	ErrRetrieverUnavailable = errors.New("knowledge: retriever is not configured")

	// ErrQuotaExhausted 表示配额预留被拒（额度不足、周期超限或限额缺失）。
	// 适配器在翻译实现侧哨兵时**同时**保留原错误（双 `%w`），HTTP 面的 429 映射
	// 因而两边的 `errors.Is` 都成立。
	// ErrQuotaExhausted marks a rejected reservation; the adapter wraps the original
	// error alongside it so both spellings match through errors.Is.
	ErrQuotaExhausted = errors.New("knowledge: quota exhausted")

	// ErrRateLimited 表示入口速率超限。
	// ErrRateLimited marks an ingress rate limit.
	ErrRateLimited = errors.New("knowledge: rate limited")

	// ErrConcurrencyLimited 表示并发名额用尽。
	// ErrConcurrencyLimited marks an exhausted concurrency slot.
	ErrConcurrencyLimited = errors.New("knowledge: concurrency limited")
)

// 摄取作业的 `error_kind` 取值（契约冻结，逐字）。
// The ingest error kinds are frozen contract literals.
const (
	// ErrorKindPathRejected 表示来源路径在准入期被拒（零写入）。
	// ErrorKindPathRejected marks an admission-time path rejection.
	ErrorKindPathRejected = "path_rejected"
	// ErrorKindDecodeFailed 表示原始字节无法按 UTF-8 解码。
	// ErrorKindDecodeFailed marks a UTF-8 decoding failure.
	ErrorKindDecodeFailed = "decode_failed"
	// ErrorKindParserFailed 表示解析失败。
	// ErrorKindParserFailed marks a parse failure.
	ErrorKindParserFailed = "parser_failed"
	// ErrorKindChunkerFailed 表示切分失败或切分结果违反约定（空结果 / 序号不连续）。
	// ErrorKindChunkerFailed marks a chunking failure or a broken chunk contract.
	ErrorKindChunkerFailed = "chunker_failed"
	// ErrorKindEmbedFailed 表示 embedding 阶段失败：配额拒绝、审计预写失败或提供方报错。
	// ErrorKindEmbedFailed marks an embedding-stage failure: quota denial, a failed
	// audit pre-write, or a provider error.
	ErrorKindEmbedFailed = "embed_failed"
	// ErrorKindVerifyFailed 表示校验失败或索引写入 / 版本激活失败——版本没有通过
	// 「片段计数、维度一致、非空、与内容哈希绑定」这一关。
	// ErrorKindVerifyFailed marks a failed verification or index/activation step.
	ErrorKindVerifyFailed = "verify_failed"
)

// ErrorKinds 返回 `error_kind` 的冻结全集（测试与界面文案映射共用一份枚举）。
// ErrorKinds returns the frozen set so tests and console mappings share one list.
func ErrorKinds() []string {
	return []string{
		ErrorKindPathRejected,
		ErrorKindDecodeFailed,
		ErrorKindParserFailed,
		ErrorKindChunkerFailed,
		ErrorKindEmbedFailed,
		ErrorKindVerifyFailed,
	}
}
