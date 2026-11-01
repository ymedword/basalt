package knowledge

// 本文件声明摄取侧的配额**消费方值类型与端口**（US-16 §2.3 图 3 / C-06）：
// `knowledge` **不得** import `internal/callers`（技术方案 §7.1.2），配额类型因此
// 在包内声明；实现由 `app` 层的适配器桥接（`internal/app/knowledge_adapters.go`），
// 翻译规则由那边的契约面用例逐字锁死。`core.Usage` 已被模型用量占用，不能承载
// 这里的记账语义，所以本包自带一份窄值类型。
//
// 三条口径与调用方记账一致：一次预留至多一个成本维度；未知用量**不得**结算成零
// （预留保持 `pending` 交管理面核对）；归还原因只用两个「调用证明未发出」的字面量。
//
// This file declares the ingest-side quota value types and port. TechnicalDesign
// §7.1.2 forbids knowledge from importing internal/callers, so the types live here and
// the app layer adapts them; the mapping is pinned by an app-side contract test.
// A reservation carries at most one cost dimension, unknown usage is never settled as
// zero, and releases only use the two "the call provably never left" reasons.

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
)

// QuotaCost 是一次预留的成本声明：Tasks / Tokens / Bytes 至多一个非零。
// QuotaCost declares one reservation cost; at most one dimension is non-zero.
type QuotaCost struct {
	// Tasks 是任务次数。
	Tasks int64
	// Tokens 是预估 Token 数。
	Tokens int64
	// Bytes 是原始字节数（仅摄取使用）。
	Bytes int64
}

// QuotaUsage 是结算时的实际用量：只填该预留所属的维度，全零表示确认零消耗。
// QuotaUsage settles the actual amount; all-zero means a confirmed zero, never unknown.
type QuotaUsage struct {
	// Tasks 是实际任务次数。
	Tasks int64
	// Tokens 是实际 Token 数。
	Tokens int64
	// Bytes 是实际字节数。
	Bytes int64
}

// QuotaTicket 是预留票据。对 `knowledge` 是**不透明**的：摄取只把它原样交回端口，
// 日志只读 `ReservationID`；其余字段供适配器往返还原实现侧票据（含归属校验位）。
// QuotaTicket is opaque to knowledge: ingest hands it back verbatim and only logs the
// reservation id; the remaining fields let the adapter restore the implementation's
// ticket, which is located by (caller_id, reservation_id).
type QuotaTicket struct {
	// ReservationID 是服务端生成的预留标识。
	ReservationID string
	// CallerID 是归属调用方，结算与归还**必须**带上它做归属校验。
	CallerID string
	// PeriodStart 是预留所属周期起点（Unix 毫秒 UTC）。
	PeriodStart int64
	// NoReservation 表示未写预留行（内置管理员不限量）。
	NoReservation bool
}

// QuotaReleaseReason 是归还原因：只接受「调用证明没有发出」的两个字面量。
// QuotaReleaseReason only accepts the two "the call never left" literals.
type QuotaReleaseReason string

const (
	// QuotaReleaseNotSent 表示调用确定未发出，预留整笔归还。
	// QuotaReleaseNotSent means the call provably never left the process.
	QuotaReleaseNotSent QuotaReleaseReason = "not_sent"
	// QuotaReleaseAuditFailed 表示审计预写失败——fail-closed 下调用不得发出。
	// QuotaReleaseAuditFailed means the pending audit row failed, so no call was issued.
	QuotaReleaseAuditFailed QuotaReleaseReason = "audit_failed"
)

// Quota 是配额预留的消费方端口：类别固定为摄取（实现侧在适配器里绑定），
// 幂等键由摄取按「作业 + 尝试 + 批次」生成。摄取**不得**绕过本端口直接记账——
// 字节与 Token 都要先预留，未知用量保持 `pending`。
// Quota is the ingest-only reservation port; the adapter binds the request class and
// ingest must reserve bytes and tokens here, never settling unknown usage as zero.
type Quota interface {
	AdmitCall(ctx context.Context, p core.Principal, cost QuotaCost, idem string) (QuotaTicket, error)
	Commit(ctx context.Context, t QuotaTicket, usage QuotaUsage) error
	Release(ctx context.Context, t QuotaTicket, reason QuotaReleaseReason) error
}
