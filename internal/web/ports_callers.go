package web

// 本文件是调用方管理面的**端口与线上形状**（US-16 / T026，颗粒度文档 §3.1.5 /
// §3.1.7）：七个端点只依赖 `CallerAdminService`，由 app 层注入
// `*callers.CallerAdmin`（结构化满足，不写适配器）。三条口径与写面其余端口一致：
//
//  1. 线上请求体在这里定义，snake_case 与契约逐字一致，解码时拒绝未知字段；
//  2. 领域类型（`callers.CallerRecord` 等）由 `views_callers.go` 投影，web 层
//     **不**另造一套同义类型充当内部表示；
//  3. 成功响应**不得**出现 Key 明文以外的凭据，Key 明文只经 `IssuedKeyView` 出站。
//
// This file declares the caller-admin port and the wire request bodies. The domain
// types stay in internal/callers and are projected by views_callers.go; the only
// place a plaintext key may leave the server is IssuedKeyView.

import (
	"context"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// CallerAdminService 是调用方 / Key / 配额管理面的消费方端口：列调用方、建调用方、
// 签发 / 吊销 Key、查周期用量、核对未知结局的预留、清零 overage。方法与
// `*callers.CallerAdmin` 逐个同签名——端口只做依赖倒置，**不**在转发层补默认值。
// 认证与授权不在这里：admin 路由已解析出 `core.Actor` 再进来。
// CallerAdminService mirrors *callers.CallerAdmin method for method; authentication
// and authorisation already happened on the admin route.
type CallerAdminService interface {
	ListCallers(ctx context.Context) ([]callers.CallerRecord, error)
	CreateCaller(ctx context.Context, actor core.Actor, req callers.CreateCallerRequest) (callers.CallerRecord, error)
	IssueKey(ctx context.Context, actor core.Actor, req callers.IssueRequest) (callers.IssuedKey, error)
	RevokeKey(ctx context.Context, actor core.Actor, req callers.RevokeRequest) (callers.KeyRecord, error)
	Usage(ctx context.Context, callerID string, periodStart int64) (callers.CallerUsage, error)
	ReconcileReservation(ctx context.Context, actor core.Actor, req callers.ReconcileRequest) (callers.Settlement, error)
	ClearOverage(ctx context.Context, actor core.Actor, req callers.ClearOverageRequest) (callers.CallerUsage, error)
}

// CallerLimitWrite 是 `CallerLimitView` 的线上形状：限额字段全部可缺省，零值一律
// 按「拒绝」解释（**不**在这里补默认数字——缺配置就是拒绝，补一个看起来合理的
// 数字会让"没配置"与"配置为零"不可区分）。
// CallerLimitWrite carries the limit fields verbatim; zero never becomes a default.
type CallerLimitWrite struct {
	QPS                  float64 `json:"qps"`
	Burst                int     `json:"burst"`
	MaxConcurrent        int     `json:"max_concurrent"`
	TasksPerPeriod       int64   `json:"tasks_per_period"`
	TokensPerPeriod      int64   `json:"tokens_per_period"`
	IngestBytesPerPeriod int64   `json:"ingest_bytes_per_period"`
	Period               string  `json:"period"`
	Timezone             string  `json:"timezone"`
}

// limit 把线上限额转成领域值；nil 接收者返回 nil——`POST /callers` 未给 `limits`
// 时**不写**限额行，而不是落一份全零（全零等于拒绝一切，语义与"没配置"不同）。
// limit converts the wire shape; a nil receiver means the limit row is not written.
func (w *CallerLimitWrite) limit() *callers.Limit {
	if w == nil {
		return nil
	}
	return &callers.Limit{
		QPS:                  w.QPS,
		Burst:                w.Burst,
		MaxConcurrent:        w.MaxConcurrent,
		TasksPerPeriod:       w.TasksPerPeriod,
		TokensPerPeriod:      w.TokensPerPeriod,
		IngestBytesPerPeriod: w.IngestBytesPerPeriod,
		Period:               callers.Period(w.Period),
		Timezone:             w.Timezone,
	}
}

// CallerCreateWrite 是 `POST /callers` 的请求体（契约 CallerCreateRequest）：
// `name` 必填（空串由领域按 400 拒绝）；`caller_id` 缺省由服务端生成。
// CallerCreateWrite is the create request; the domain owns the required-name check.
type CallerCreateWrite struct {
	CallerID string            `json:"caller_id"`
	Name     string            `json:"name"`
	Project  string            `json:"project"`
	Limits   *CallerLimitWrite `json:"limits"`
}

// CallerKeyIssueWrite 是 `POST /callers/{id}/keys` 的请求体（契约
// CallerKeyIssueRequest）：`scopes` 必填（空数组、未知字面量与两个管理面 scope
// 都由领域按 400 `INVALID_SCOPE` 拒绝，且**不留** `pending` 操作行）；
// `expires_at` 是可选 RFC3339 时刻，缺省不过期。
// CallerKeyIssueWrite is the issuance request; scope validation stays in the domain.
type CallerKeyIssueWrite struct {
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expires_at"`
}

// CallerKeyRevokeWrite 是 `POST /callers/{id}/keys/{key_id}/revoke` 的可选请求体：
// 整个请求体可省略，`reason` 只进管理审计（**不得**含凭据）。
// CallerKeyRevokeWrite is the optional revoke body; reason only feeds the audit row.
type CallerKeyRevokeWrite struct {
	Reason string `json:"reason"`
}

// CallerReconcileWrite 是 `POST /callers/{id}/reservations/{reservation_id}/reconcile`
// 的请求体（契约 ReservationReconcileRequest）：`outcome` 与 `reason` 必填；
// `actual` 用指针区分"没给"与"给了 0"——`committed` 要求前者非空，`released`
// 要求它为空，两者都由领域判定。
// CallerReconcileWrite keeps actual as a pointer so "absent" and "zero" stay distinct.
type CallerReconcileWrite struct {
	Outcome string `json:"outcome"`
	Actual  *int64 `json:"actual"`
	Reason  string `json:"reason"`
}

// CallerClearOverageWrite 是 `POST /callers/{id}/usage/clear-overage` 的请求体
// （契约 ClearOverageRequest）：`reason` 必填；`period_start` 是该周期内的任一
// 时刻，空串表示当前周期（折算到周期起点由存储层负责）。
// CallerClearOverageWrite zeroes one period's overage; an empty instant means now.
type CallerClearOverageWrite struct {
	PeriodStart string `json:"period_start"`
	Reason      string `json:"reason"`
}
