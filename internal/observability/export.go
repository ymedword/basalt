package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// SignalKind 每批只允许一个OTLP信号。
// SignalKind restricts every batch to one OTLP signal.
type SignalKind string

// 三种已审信号均进入持久账本。
// All three approved signals use the durable ledger.
const (
	SignalTrace  SignalKind = "trace"
	SignalLog    SignalKind = "log"
	SignalMetric SignalKind = "metric"
)

// ExportState 是遥测账本状态，不改变业务状态。
// ExportState belongs to the telemetry ledger, never business execution.
type ExportState string

// unconfirmed不自动重试；inflight租约过期后可能重复发送。
// Unconfirmed is not retried; an expired inflight lease may cause duplicates.
const (
	ExportPending         ExportState = "pending"
	ExportInflight        ExportState = "inflight"
	ExportSent            ExportState = "sent"
	ExportUnconfirmed     ExportState = "unconfirmed"
	ExportRetry           ExportState = "retry"
	ExportFailedPermanent ExportState = "failed_permanent"
	ExportDropped         ExportState = "dropped"
	ExportStale           ExportState = "stale"
)

// ExportItem 仅持久化定位、哈希、状态及派生指标快照，不复制执行正文。
// ExportItem persists identity, hash, state and derived metric snapshots, not execution bodies.
type ExportItem struct {
	ItemID                    string
	SourceKind                SourceKind
	SourceID                  string
	StateHash                 string
	Signal                    SignalKind
	SourceTerminalAt          time.Time
	State                     ExportState
	Attempts                  int
	LastBatchID               string
	CreatedAt                 time.Time
	NextAttemptAt, LeaseUntil *time.Time
	LastErrorKind             string
	Payload                   json.RawMessage `json:"-"`
	Fact                      any             `json:"-"`
}

// ExportBatch 的Items只用于关联；网络Payload只能由Mapper构造。
// ExportBatch uses items for correlation; only the mapper builds the network payload.
type ExportBatch struct {
	BatchID     string
	Signal      SignalKind
	RecordUnit  string
	Items       []ExportItem
	RecordCount int
	Payload     any
	MaxAttempts int
	MaxItemAge  time.Duration
}

// SendResult 仅含批次级计数，不猜测逐条接收结果。
// SendResult carries batch-level counts without inventing per-item acceptance.
type SendResult struct {
	Accepted       int
	Rejected       int
	PartialSuccess bool
	Warning        string
	Retryable      bool
	RetryAfter     time.Duration
	ErrorKind      string
	HTTPStatus     int
	Duration       time.Duration
}

// Sender 是唯一网络发送端口；内部重试被禁止。
// Sender is the sole network port and performs no internal retries.
type Sender interface {
	Send(context.Context, ExportBatch) (SendResult, error)
}

// ExportScanner 只扫描权威终态，不恢复或重放业务。
// ExportScanner scans terminal facts without recovering or replaying business work.
type ExportScanner interface {
	Scan(context.Context, time.Time) (ScanResult, error)
}

// ExportLedger 把发送尝试与确认分开，确认失败不保留本地响应计数。
// ExportLedger separates attempts from acknowledgments; lost acknowledgments retain no local counts.
type ExportLedger interface {
	Claim(context.Context, int, time.Time, time.Duration) ([]ExportItem, error)
	Enqueue(context.Context, []ExportItem) error
	MarkStale(context.Context, []ExportItem) error
	BeginBatch(context.Context, ExportBatch, time.Time) error
	ApplySendResult(context.Context, ExportBatch, SendResult, error, time.Time) error
	PendingCount(context.Context) (int64, error)
	OldestPendingAge(context.Context) (time.Duration, error)
	UnconfirmedCount(context.Context) (int64, error)
}

// ExportConfig 是固定有界策略；零值在构造时采用已审候选默认值。
// ExportConfig defines bounded policy; constructors fill approved defaults for zero values.
type ExportConfig struct {
	BatchSize        int
	LeaseTTL         time.Duration
	MaxAttempts      int
	MaxItemAge       time.Duration
	ScanOverlap      time.Duration
	SafetyLag        time.Duration
	ReconcileHorizon time.Duration
	InitialBackfill  time.Duration
	ScanPageSize     int
	MetricsInterval  time.Duration
}

// DefaultExportConfig 只提供有界候选默认值，不启用任何网络发送。
// DefaultExportConfig supplies bounded defaults without enabling network export.
func DefaultExportConfig() ExportConfig {
	return ExportConfig{
		BatchSize: 256, LeaseTTL: 5 * time.Minute, MaxAttempts: 10, MaxItemAge: 24 * time.Hour,
		ScanOverlap: 5 * time.Minute, SafetyLag: 2 * time.Minute, ReconcileHorizon: 24 * time.Hour, InitialBackfill: 24 * time.Hour, ScanPageSize: 500, MetricsInterval: time.Minute,
	}
}

// ExportResult 只汇总本轮账本落账结果，不能证明外部后端可查询。
// ExportResult summarizes local acknowledgments, not external backend queryability.
type ExportResult struct{ Batches, Claimed, Sent, Unconfirmed, Retried, FailedPermanent, Stale int }

// ExportStatus 结果未知的批次与逐条不确定的项分开显示。
// ExportStatus separates unknown batch attempts from unconfirmed individual items.
type ExportStatus struct {
	Enabled                 bool             `json:"enabled"`
	EndpointConfigured      bool             `json:"endpoint_configured"`
	HeadersConfigured       bool             `json:"headers_configured"`
	Protocol                string           `json:"protocol"`
	Pending                 int64            `json:"pending"`
	Inflight                int64            `json:"inflight"`
	Retry                   int64            `json:"retry"`
	RetryAttempts           *int64           `json:"retry_attempts"`
	NextRetryAt             *time.Time       `json:"next_retry_at"`
	Unconfirmed             int64            `json:"unconfirmed"`
	FailedPermanent         int64            `json:"failed_permanent"`
	Dropped                 int64            `json:"dropped"`
	Stale                   int64            `json:"stale"`
	UnknownAttempts         int64            `json:"unknown_attempts"`
	OldestPendingAge        time.Duration    `json:"-"`
	OldestPendingAgeSeconds float64          `json:"oldest_pending_age_seconds"`
	LastScanAt              time.Time        `json:"-"`
	LastScanTime            *time.Time       `json:"last_scan_at"`
	LastBatchID             string           `json:"last_batch_id"`
	LastPartialAt           *time.Time       `json:"last_partial_at"`
	LastRecordUnit          string           `json:"last_record_unit"`
	LastRecordCount         int              `json:"last_record_count"`
	LastOutcome             string           `json:"last_outcome"`
	LastHTTPStatus          *int             `json:"last_http_status"`
	LastAccepted            *int             `json:"last_accepted"`
	LastRejected            *int             `json:"last_rejected"`
	LastWarning             string           `json:"last_warning"`
	LastErrorKind           string           `json:"last_error_kind"`
	Sources                 []SourceCoverage `json:"sources"`
}

// ScanResult 只在完整分页与覆盖CAS成功后报告推进边界。
// ScanResult reports coverage advancement only after complete pagination and a successful CAS.
type ScanResult struct {
	Enqueued, Scanned, ReconcileScanned int
	CoveredFrom, CoveredThrough         map[SourceKind]time.Time
}

// ExportError 不携带原文，只携带定位与固定分类。
// ExportError contains identity and a fixed class, never raw source text.
type ExportError struct {
	SourceKind     SourceKind
	SourceID, Kind string
}

// ExportDisposition 集中解释协议结果，worker与PG账本共用同一规则。
// ExportDisposition provides one protocol interpretation for workers and the ledger.
type ExportDisposition struct {
	State                       ExportState
	Outcome, ErrorKind, Warning string
	Accepted, Rejected          *int
}

// ResolveSendResult 保留批次不确定性，非法计数不猜测为零。
// ResolveSendResult preserves batch uncertainty and never replaces invalid counts with zero.
func ResolveSendResult(batch ExportBatch, res SendResult, sendErr error) ExportDisposition {
	out := ExportDisposition{State: ExportFailedPermanent, Outcome: "permanent", ErrorKind: exportErrorKind(res.ErrorKind)}
	if sendErr != nil && out.ErrorKind == "" {
		out.ErrorKind = "transport_error"
		if errors.Is(sendErr, context.DeadlineExceeded) {
			out.ErrorKind = "timeout"
		}
		if errors.Is(sendErr, context.Canceled) {
			out.ErrorKind = "cancelled"
		}
	}
	if res.Warning != "" {
		out.Warning = "receiver_warning"
	}
	valid := res.Accepted >= 0 && res.Rejected >= 0 && res.Accepted <= batch.RecordCount && res.Rejected <= batch.RecordCount && res.Accepted+res.Rejected == batch.RecordCount
	if res.PartialSuccess || sendErr == nil {
		if valid && sendErr == nil && (res.PartialSuccess || res.Rejected == 0) {
			accepted, rejected := res.Accepted, res.Rejected
			out.Accepted, out.Rejected = &accepted, &rejected
		}
		if !valid || !res.PartialSuccess && res.Rejected != 0 {
			out.State, out.Outcome, out.ErrorKind = ExportUnconfirmed, "partial_unconfirmed", "protocol_invalid"
			return out
		}
		if res.PartialSuccess && (res.Rejected > 0 || sendErr != nil) {
			out.State, out.Outcome = ExportUnconfirmed, "partial_unconfirmed"
			if sendErr != nil {
				out.ErrorKind = "partial_with_transport_error"
			} else {
				out.ErrorKind = "partial_rejected"
			}
			return out
		}
		out.State, out.Outcome, out.ErrorKind = ExportSent, "sent", ""
		if res.Warning != "" {
			out.Outcome = "sent_with_warning"
		}
		return out
	}
	if res.Retryable {
		out.State, out.Outcome = ExportRetry, "retry"
		if errors.Is(sendErr, context.DeadlineExceeded) || res.ErrorKind == "timeout" {
			out.Outcome = "timeout"
		}
	}
	return out
}

func exportErrorKind(value string) string {
	if slices.Contains([]string{"", "timeout", "transport_error", "http_retryable", "http_permanent", "response_too_large", "protocol_invalid", "partial_rejected", "partial_with_transport_error", "mapping_error", "retry_exhausted", "source_unavailable", "lease_lost", "cancelled"}, value) {
		return value
	}
	return "transport_error"
}

// ReadExportErrorKind 收窄历史自由文本列；未知值不作为传输错误证据或原文外发。
// ReadExportErrorKind narrows legacy free text without treating it as transport evidence or returning it.
func ReadExportErrorKind(value string) string {
	if exportErrorKind(value) == value {
		return value
	}
	return "unclassified"
}

// ExportItemKey 为同一来源状态生成稳定键，不依赖数据库序号。
// ExportItemKey derives a stable key from source identity and state.
func ExportItemKey(source SourceKind, id, hash string) string {
	raw, _ := json.Marshal([3]string{string(source), id, hash})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CanonicalStateHash 使用确定性JSON编码；不把正文复制到账本。
// CanonicalStateHash uses deterministic JSON without copying source bodies into the ledger.
func CanonicalStateHash(fact any) (string, error) {
	raw, err := json.Marshal(fact)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Validate 拒绝未知来源、伪造初态和执行正文payload。
// Validate rejects unknown sources, invented initial states and execution payload copies.
func (item ExportItem) Validate() error {
	if !slices.Contains([]SourceKind{SourceRun, SourceLLM, SourceTool, SourceEmbedding, SourceTask, SourceOperation, SourceIngest, SourceDiagnostic, SourceMetricSnapshot}, item.SourceKind) || item.SourceID == "" || len(item.SourceID) > 256 || item.SourceTerminalAt.IsZero() || item.State != "" && item.State != ExportPending {
		return ErrInvalidQuery
	}
	if !slices.Contains([]SignalKind{SignalTrace, SignalLog, SignalMetric}, item.Signal) {
		return ErrInvalidQuery
	}
	hash, err := hex.DecodeString(item.StateHash)
	if err != nil || len(hash) != sha256.Size {
		return ErrInvalidQuery
	}
	if item.ItemID != "" && item.ItemID != ExportItemKey(item.SourceKind, item.SourceID, item.StateHash) {
		return ErrInvalidQuery
	}
	if item.SourceKind == SourceMetricSnapshot {
		if item.Signal != SignalMetric || len(item.Payload) == 0 || len(item.Payload) > 1<<20 || !json.Valid(item.Payload) {
			return ErrInvalidQuery
		}
	} else if len(item.Payload) != 0 || item.Signal == SignalMetric {
		return ErrInvalidQuery
	}
	return nil
}
