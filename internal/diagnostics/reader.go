package diagnostics

// 本文件是诊断事件的**读取路径**（颗粒度文档 §2.9 / §3.1.6、tasks.md T097）。
//
// 三条纪律：
//
//  1. **至少一个锚点**：没有关联标识的读取 = 无条件全表扫，既无界也回答不了"这些
//     证据属于谁"。`Query.Validate` 在触达存储之前拒绝这种查询（契约 400 的同口径）。
//  2. **只读、不修补**：读出什么就是什么——本层不补 `seq`、不重排、不把空集解释成
//     "一切正常"。缺失语义（`missing`）由端点按"有没有行"显式表达。
//  3. **读面也脱敏**：库里存的是落库前脱敏过的 attrs，读面再过一次同一个脱敏器是
//     **纵深防御**（历史行、手工补数、迁移来的旧行都可能绕过写入路径）。
//
// This file is the read path: at least one anchor, no repair, and redaction applied
// again on the way out as defence in depth.

import (
	"context"
	"errors"
	"time"
)

// ErrQueryWithoutAnchor 表示查询没有任何关联锚点。
// ErrQueryWithoutAnchor means the query carries no correlation anchor.
var ErrQueryWithoutAnchor = errors.New("诊断查询必须给出至少一个关联锚点")

// ErrQueryLimit 表示 limit 越界（契约 minimum 1 / maximum 1000）。
// ErrQueryLimit means the limit is outside the contract's bounds.
var ErrQueryLimit = errors.New("诊断查询 limit 越界")

// MaxQueryLimit 是契约允许的单次读取上限（`limit` maximum 1000）。
// MaxQueryLimit is the contract's upper bound for one read.
const MaxQueryLimit = 1000

// Query 是一次诊断读取的筛选条件。零值字段 = 不按该维度过滤；`Limit` 为 0 时由存储层
// 补缺省 200（与契约 default 同值）。
// Query filters one read. Zero fields do not filter; a zero Limit falls back to 200.
type Query struct {
	// RequestID / RunID / OperationID / IngestJobID 是四个主锚点，至少给一个。
	// The four primary anchors; at least one is required.
	RequestID   string
	RunID       string
	TaskID      string
	OperationID string
	IngestJobID string
	// SessionID 是会话锚点：存储层经 `runs.session_id` 解析成 Run 集合。单独给出即可，
	// 因为它最终仍然落成一组 `run_id` 锚点，不是无条件扫描。
	// SessionID resolves to a set of run ids through runs.session_id, so it stands in
	// for a run anchor rather than degrading into a full scan.
	SessionID string
	// Kind 非空时只取该种类。
	// A non-empty Kind restricts the read to that kind.
	Kind Kind
	// From / To 是 `occurred_at` 的闭区间边界（零值 = 该侧不限）。
	// From and To bound occurred_at inclusively; the zero value leaves that side open.
	From time.Time
	To   time.Time
	// Limit 是返回条数上限（1–1000；0 = 用缺省 200）。
	// Limit caps the page size; zero means the default of 200.
	Limit int
}

// Validate 校验锚点与上限。`TaskID` 只作限定符：**单独给出不构成查询**——调度任务
// 的事件由 `task_id + request_id` 或 `task_id + run_id` 共同定位。
// Validate enforces an anchor and the contract bounds; TaskID alone is not a query.
func (q Query) Validate() error {
	if q.RequestID == "" && q.RunID == "" && q.OperationID == "" && q.IngestJobID == "" &&
		q.SessionID == "" {
		return ErrQueryWithoutAnchor
	}
	if q.Limit < 0 || q.Limit > MaxQueryLimit {
		return ErrQueryLimit
	}
	return nil
}

// EventSource 是诊断读取的**消费方端口**：实现放 `internal/storage/postgres`。
// EventSource is the read port; the implementation lives in storage.
type EventSource interface {
	// ListEvents 按 `occurred_at` 升序返回一页事件；无行返回空切片而非错误。
	// ListEvents returns one page in occurred_at order; no rows is empty, not an error.
	ListEvents(ctx context.Context, q Query) ([]DiagnosticEvent, error)
}

// Reader 是诊断事件的读入口（§3.1.6 的 `Reader` 行）。
// Reader is the single read entry point for diagnostic events.
type Reader struct {
	source   EventSource
	config   Config
	redactor Redactor
}

// NewReader 组装读取路径；`config` 决定端点回给管理台的保留期与采样口径，
// `redactor` 为空时回落白名单实现（读面脱敏不可关）。
// NewReader assembles the read path; a nil redactor still redacts on the way out.
func NewReader(source EventSource, config Config, redactor Redactor) *Reader {
	return &Reader{source: source, config: config, redactor: redactor}
}

// List 读取一页事件。**无端口即错误**，不返回空切片——空切片在端点上表示"没有记录"，
// 装配缺失与"查过了没有"必须能区分开。
// List reads one page. A missing port is an error rather than an empty page, because an
// empty page means "no records" at the endpoint and the two must stay distinguishable.
func (r *Reader) List(ctx context.Context, q Query) ([]DiagnosticEvent, error) {
	if r == nil || r.source == nil {
		return nil, errors.New("诊断读取端口未装配")
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}
	events, err := r.source.ListEvents(ctx, q)
	if err != nil {
		return nil, err
	}
	redacted := make([]DiagnosticEvent, len(events))
	for i, event := range events {
		redacted[i] = event.Redacted(r.redactor)
	}
	return redacted, nil
}

// Config 返回生效配置：端点据此给出 `retention_days` 与 `sampled_out` 的判定。
// Config exposes the effective settings the endpoint reports back.
func (r *Reader) Config() Config {
	if r == nil {
		return DefaultConfig()
	}
	return r.config
}
