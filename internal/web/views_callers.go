package web

// 本文件是调用方管理面响应的**唯一投影面**（US-16 / T026）：端口已经给出
// `callers` 领域记录，此处只做形状转换——字段名与契约逐字一致，时间统一
// RFC3339 UTC（零值渲染空串），数组字段输出 `[]` 而不是 `null`。
// 一条硬规则：Key 明文只经 `IssuedKeyView.Key` 出站一次；其余视图都从
// `callers.KeyRecord` 投影，结构上没有明文字段。
// This file is the single projection surface for caller-admin responses; the only
// view that can carry a plaintext key is IssuedKeyView, and only once.

import (
	"time"

	"github.com/ymedword/basalt/internal/callers"
)

// CallerSummaryView 是 `CallerSummary`（契约）：`project` 与 `key_count` 虽非必填，
// 但管理台要据此分组与判断"还有没有可用 Key"，因此恒输出（空串 / 0 照实报）。
// CallerSummaryView always emits project and key_count so the console need not guess.
type CallerSummaryView struct {
	CallerID  string `json:"caller_id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Project   string `json:"project"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	KeyCount  int    `json:"key_count"`
}

// callerSummaryView 投影一条调用方行：枚举字面量原样出站（领域类型已按闭集校验）。
// callerSummaryView projects one caller row; enum literals pass through verbatim.
func callerSummaryView(record callers.CallerRecord) CallerSummaryView {
	return CallerSummaryView{
		CallerID:  record.CallerID,
		Kind:      string(record.Kind),
		Name:      record.Name,
		Project:   record.Project,
		Status:    string(record.Status),
		CreatedAt: formatTime(record.CreatedAt),
		KeyCount:  record.KeyCount,
	}
}

// callerSummaryViews 投影列表；空集合输出 `[]`（约束 34）。
// callerSummaryViews never emits null for an empty list.
func callerSummaryViews(records []callers.CallerRecord) []CallerSummaryView {
	views := make([]CallerSummaryView, len(records))
	for i, record := range records {
		views[i] = callerSummaryView(record)
	}
	return views
}

// CallerKeySummaryView 是 `CallerKeySummary`（契约）：`expires_at` / `revoked_at`
// 是普通字符串字段，不过期 / 未吊销时为空串（**不是**缺省——页面据空串判断）。
// 结构里没有明文，只有展示前缀。
// CallerKeySummaryView carries no plaintext; the empty string marks absent instants.
type CallerKeySummaryView struct {
	KeyID     string   `json:"key_id"`
	CallerID  string   `json:"caller_id"`
	KeyPrefix string   `json:"key_prefix"`
	Scopes    []string `json:"scopes"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
	ExpiresAt string   `json:"expires_at"`
	RevokedAt string   `json:"revoked_at"`
}

// callerKeySummaryView 投影一条 Key 行；可空时刻按契约渲染空串。
// callerKeySummaryView renders absent instants as empty strings per the contract.
func callerKeySummaryView(record callers.KeyRecord) CallerKeySummaryView {
	view := CallerKeySummaryView{
		KeyID:     record.KeyID,
		CallerID:  record.CallerID,
		KeyPrefix: record.KeyPrefix,
		Scopes:    stringSlice(record.Scopes),
		Status:    string(record.Status),
		CreatedAt: formatTime(record.CreatedAt),
	}
	if record.ExpiresAt != nil {
		view.ExpiresAt = formatTime(*record.ExpiresAt)
	}
	if record.RevokedAt != nil {
		view.RevokedAt = formatTime(*record.RevokedAt)
	}
	return view
}

// IssuedKeyView 是 `IssuedKey`（契约 CallerKeySummary + `key`）：明文**只**在这里
// 出现一次，服务端只存指纹；它不落日志、不进审计、不进诊断（领域层同样保证）。
// IssuedKeyView is the one place a plaintext key leaves the server.
type IssuedKeyView struct {
	CallerKeySummaryView
	Key string `json:"key"`
}

// issuedKeyView 投影签发结果：Key 行字段 + 一次性明文。
// issuedKeyView projects the issuance result with its one-time plaintext.
func issuedKeyView(issued callers.IssuedKey) IssuedKeyView {
	return IssuedKeyView{
		CallerKeySummaryView: callerKeySummaryView(issued.KeyRecord),
		Key:                  issued.Secret,
	}
}

// CallerLimitView 是 `CallerLimitView`（契约）：零值照实输出——零表示"该维度拒绝"，
// **不是**"不限量"，页面与调用方都按这个口径读。
// CallerLimitView reports zeros verbatim; zero means denied, never unlimited.
type CallerLimitView struct {
	QPS                  float64 `json:"qps"`
	Burst                int     `json:"burst"`
	MaxConcurrent        int     `json:"max_concurrent"`
	TasksPerPeriod       int64   `json:"tasks_per_period"`
	TokensPerPeriod      int64   `json:"tokens_per_period"`
	IngestBytesPerPeriod int64   `json:"ingest_bytes_per_period"`
	Period               string  `json:"period"`
	Timezone             string  `json:"timezone"`
}

// callerLimitView 投影限额；领域已 Normalize（period / timezone 有缺省值）。
// callerLimitView projects the normalized limit.
func callerLimitView(limit callers.Limit) CallerLimitView {
	return CallerLimitView{
		QPS:                  limit.QPS,
		Burst:                limit.Burst,
		MaxConcurrent:        limit.MaxConcurrent,
		TasksPerPeriod:       limit.TasksPerPeriod,
		TokensPerPeriod:      limit.TokensPerPeriod,
		IngestBytesPerPeriod: limit.IngestBytesPerPeriod,
		Period:               string(limit.Period),
		Timezone:             limit.Timezone,
	}
}

// UsageLineView 是 `UsageLine`（契约）：三个计数都必填，零值照实输出。
// UsageLineView always emits its three counters, zeros included.
type UsageLineView struct {
	Kind     string `json:"kind"`
	Used     int64  `json:"used"`
	Reserved int64  `json:"reserved"`
	Overage  int64  `json:"overage"`
}

// CallerUsageView 是 `CallerUsageView`（契约）：周期边界按 timezone 本地 00:00
// 折算后以 UTC 瞬时出站（同一时刻的另一种写法），`timezone` 字段标明折算口径。
// CallerUsageView emits the period bounds as UTC instants plus the local timezone.
type CallerUsageView struct {
	CallerID            string          `json:"caller_id"`
	PeriodStart         string          `json:"period_start"`
	PeriodEnd           string          `json:"period_end"`
	Timezone            string          `json:"timezone"`
	Limits              CallerLimitView `json:"limits"`
	Usage               []UsageLineView `json:"usage"`
	PendingReservations int             `json:"pending_reservations"`
}

// callerUsageView 投影周期用量：行顺序由领域固定（六行），此处不重排。
// callerUsageView keeps the domain's fixed row order.
func callerUsageView(usage callers.CallerUsage) CallerUsageView {
	lines := make([]UsageLineView, len(usage.Lines))
	for i, line := range usage.Lines {
		lines[i] = UsageLineView{Kind: line.Kind, Used: line.Used, Reserved: line.Reserved, Overage: line.Overage}
	}
	return CallerUsageView{
		CallerID:            usage.CallerID,
		PeriodStart:         formatTime(usage.PeriodStart),
		PeriodEnd:           formatTime(usage.PeriodEnd),
		Timezone:            usage.Timezone,
		Limits:              callerLimitView(usage.Limit),
		Usage:               lines,
		PendingReservations: usage.PendingReservations,
	}
}

// ReservationView 是 `ReservationView`（契约）。`Settlement` 只覆盖必填六项与
// `actual` / `overage`；`idempotency_key` / `run_id` / `ingest_job_id` / `reason` /
// `created_at` 是可选字段，结算结果不携带它们，因此省略而不是编造空值。
// ReservationView fills only what Settlement carries; unknown optional fields are
// omitted rather than fabricated.
type ReservationView struct {
	ReservationID string `json:"reservation_id"`
	CallerID      string `json:"caller_id"`
	Class         string `json:"class"`
	PeriodStart   string `json:"period_start"`
	Amount        int64  `json:"amount"`
	Actual        *int64 `json:"actual,omitempty"`
	Overage       int64  `json:"overage"`
	Status        string `json:"status"`
}

// reservationView 投影核对结果：状态是结算后的终态（`reconciled`），`actual`
// 是落库值（`released` 时为零并被照实报出）。
// reservationView projects the settled reservation; actual is the stored value.
func reservationView(settlement callers.Settlement) ReservationView {
	actual := settlement.Actual
	return ReservationView{
		ReservationID: settlement.ReservationID,
		CallerID:      settlement.CallerID,
		Class:         string(settlement.Class),
		PeriodStart:   formatTime(time.UnixMilli(settlement.PeriodStart)),
		Amount:        settlement.Amount,
		Actual:        &actual,
		Overage:       settlement.Overage,
		Status:        string(settlement.State),
	}
}
