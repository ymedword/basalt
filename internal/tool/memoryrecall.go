package tool

// 本文件是 `recall_memory` 语义召回的**消费方端口**（P-07 / A-08）：`internal/tool` 只依赖
// `core`，不能 import `internal/memory`（依赖方向纪律），因此由消费方在本包声明端口，
// 由 app 用 `memory.SemanticStore` 适配。端口只有一条方法：工具不做权限判定、不做模式
// 分流语义的二次实现——身份判定在载体侧按**每次调用**重做（C-11 / C-28）。
// The consumer port of semantic recall: tool cannot import memory, so the port is declared
// here and adapted by the assembly layer; permission is re-checked per call in the store.
//
// 三条纪律：
//  1. **身份显式传递**：`Principal` 是请求字段，不从模型入参、Agent 名或包级状态反推；
//     零值请求由载体拒绝（本包工具也在触碰载体前先判一次，见 `RecallMemoryTool.Execute`）。
//  2. **装配缺失失败关闭**：语义路径未装配（缺载体或 embedding 连接）时返回
//     `ErrRecallUnavailable`，由工具翻译成**可自我纠正**的 `invalid_input`——不静默降级成
//     关键词路径：那会让"检索变弱"看起来像"没有更多内容"。
//  3. **零命中是成功**：空切片 + nil 错误；占位文案由工具渲染。
// Three disciplines: the principal travels explicitly, a missing assembly fails closed with
// an actionable refusal instead of a silent keyword fallback, and no hit is a success.

import (
	"context"
	"errors"

	"github.com/ymedword/basalt/internal/core"
)

// ErrRecallUnavailable 表示语义召回路径**未装配**（缺载体或 embedding 连接）——不是
// "库里没有"，也不是最后一次检索的结论。装配层据此把 `memory` 侧的两种装配错误收敛成
// 一个可判哨兵，工具据此给模型"改用 mode=keyword"这类可执行的下一步。
// ErrRecallUnavailable marks an unassembled semantic path; the tool turns it into an
// actionable invalid_input rather than a silent fallback.
var ErrRecallUnavailable = errors.New("tool: semantic recall path is not assembled")

// ErrRecallIndexNotReady 表示语义召回遇到了**索引缺口**（V-5）：同层存在生效归档记忆
// 但尚无当前代际的向量，本次结果**不能**解释成"没有可召回记忆"。它与
// `ErrRecallUnavailable` 是两种结局：前者装配在位、只是索引没补齐（重试或改用关键词
// 都可继续），后者压根没装配。工具层把前者渲染成可自我纠正的失败，**不得**回
// `(无匹配条目)`。
// ErrRecallIndexNotReady marks a vector gap: the path is assembled, but the index is
// incomplete, so the empty result must not be rendered as "no match".
var ErrRecallIndexNotReady = errors.New("tool: memory vector index is not ready")

// MemoryRecallRequest 是一次归档区召回的全部入参。字段与 `memory.RecallQuery` 逐项对应，
// 但**在本包声明**（消费方接口）：两处形状一旦漂移，适配层的编译期断言先红。
// MemoryRecallRequest mirrors the domain query shape, declared by the consumer.
type MemoryRecallRequest struct {
	// Principal 是本次调用的调用方身份（服务端注入，零值非法）。
	Principal          core.Principal
	AuthorizationAgent string
	// Scope 是检索的记忆分层（Agent 名；零值 = 全局层）。
	Scope core.Scope
	// Query 是检索文本；keyword 模式按整串子串匹配，语义模式送去 embedding。
	Query string
	// Mode 取 "semantic" / "keyword"；空串按语义处理（P-07 的契约变更，显式登记）。
	Mode string
	// Limit 是返回条数上限（<= 0 表示不截断）。
	Limit int
}

// MemoryRecallHit 是一条召回结果：工具只渲染这三项——名次（返回顺序）、来源（写入路径
// 的闭集取值，空串不渲染）与正文。`Score` 不进模型上下文：异质分数不可比，名次才是
// 模型能用的稳定信号（A-08「带名次与来源」）。
// MemoryRecallHit is what the model sees: rank, source and content; raw scores stay out
// because heterogeneous scores are not comparable.
type MemoryRecallHit struct {
	// Rank 是名次（1 起）。
	Rank int
	// Source 是写入来源（`memory_entries.source` 的闭集取值；空串 = 不渲染）。
	Source string
	// Content 是条目正文（已规范化）。
	Content string
}

// MemoryRecaller 是 `recall_memory` 的召回端口（消费者声明）。实现必须：
//   - 在触碰载体**之前**判定 `req.Principal.Valid()`；
//   - 每次调用重判权限，不复用上一次的结论（C-11「冻结内容不冻结权限」）；
//   - 语义路径未装配时返回包裹 `ErrRecallUnavailable` 的错误。
//
// The recall port: implementations validate the principal before touching the store and
// re-check permission on every call.
type MemoryRecaller interface {
	RecallArchive(ctx context.Context, req MemoryRecallRequest) ([]MemoryRecallHit, error)
}
