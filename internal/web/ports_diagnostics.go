package web

// 本文件是诊断读取端点的**消费方端口**（US-16 / T097；颗粒度文档 §3.1.7 的
// 「诊断端点」行；端点与字段以冻结契约 `docs/contracts/openapi.yaml` 的
// `GET /diagnostics` / `DiagnosticEventView` 为准）。三条口径：
//
//  1. **只读、管理面**：契约根 security 只有 `bearerAuth` / `localTrust`，没有
//     `callerKey`——caller key 一律拒绝，调用方自助读取不在 B 范围（归 OBS / G）。
//  2. **直接复用 `internal/diagnostics` 的领域类型**：`*diagnostics.Reader` 以具体
//     方法 `List` / `Config` 结构化满足，本包不另造一套事件投影类型。
//  3. **装配缺失按失败关闭**：端口为 nil 时端点固定 500（`writeDiagnosticsPortMissing`），
//     **不得**用空集冒充"没有记录"——空集在契约里带 `missing=true`，是有含义的读数，
//     装配故障不能借用它。
//
// This file declares the consumer port of the read-only diagnostics endpoint. The domain
// reader satisfies it structurally and a nil port fails the endpoint closed.

import (
	"context"

	"github.com/ymedword/basalt/internal/diagnostics"
)

// DiagnosticsReader 是诊断读取的消费方端口：按关联标识取一页事件，并给出本实例的
// 生效配置（端点据此填 `retention_days` 与判定 `sampled_out`）。
// DiagnosticsReader reads one page of events and reports the effective settings the
// endpoint mirrors back to the console.
type DiagnosticsReader interface {
	// List 返回一页事件（`occurred_at` 升序）；无命中返回空切片而非错误。
	// List returns one page in occurred_at order; no rows is an empty slice.
	List(ctx context.Context, q diagnostics.Query) ([]diagnostics.DiagnosticEvent, error)
	// Config 返回生效的保留期与采样率。
	// Config reports the effective retention and sampling settings.
	Config() diagnostics.Config
}

// DiagnosticWriter 是诊断事件的**写**端口（V-8）。目前唯一的调用方是入口速率中间件：
// 速率拒绝发生在请求变成 Run **之前**，`request_id` 是它唯一的主锚点，而通过
// `RequestID` 建立关联的能力只有中间件这一层有。
//
// 只记**拒绝**：逐请求记放行会把采样额度耗在常态流量上，而放行本身由请求日志与
// 后续 Run 的 `quota_decision` 共同证明；拒绝是异常，需要一条可查询的记录。
// The write face of the diagnostics store. Only the ingress middleware uses it, and it
// records denials alone: allows are the norm and are recoverable from request logs.
type DiagnosticWriter interface {
	// Record 写一条事件；实现**必须**吞掉写入失败（诊断可丢弃，不得阻断请求）。
	// Record must never block the request: diagnostics are droppable by contract.
	Record(ctx context.Context, ev diagnostics.DiagnosticEvent)
}
