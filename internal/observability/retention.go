package observability

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// CleanupRun 只证明某窗口发生过清理，不能证明具体记录被删。
// CleanupRun proves window-level cleanup, not an individual deletion.
type CleanupRun struct {
	Next               *CleanupCursor `json:"-"`
	CleanupID          string         `json:"cleanup_id"`
	TableKind          string         `json:"table_kind"`
	WindowStart        time.Time      `json:"window_start"`
	WindowEnd          time.Time      `json:"window_end"`
	ScanCoveredFrom    *time.Time     `json:"scan_covered_from"`
	ScanCoveredThrough *time.Time     `json:"scan_covered_through"`
	Deleted            int64          `json:"deleted"`
	Skipped            int64          `json:"skipped"`
	SkipReason         string         `json:"skip_reason"`
	Status             string         `json:"status"`
	ErrorKind          string         `json:"error_kind"`
	StartedAt          time.Time      `json:"started_at"`
	FinishedAt         *time.Time     `json:"finished_at"`
}

// RetentionPolicy 仅允许四个可丢弃表组，默认全部关闭。
// RetentionPolicy covers four droppable groups and defaults to disabled.
type RetentionPolicy struct {
	Table                 string
	ExportSource, Enabled bool
	Retention             time.Duration
	BatchSize, MaxBatches int
}

// CleanupCursor 是进程内分页位置，不是删除证明或逐条墓碑。
// CleanupCursor is a process-local position, not deletion proof or a tombstone.
type CleanupCursor struct {
	At time.Time
	ID string
}

// CleanupRequest 显式携带覆盖与确认条件，不能通过context偷渡业务状态。
// CleanupRequest explicitly carries coverage and confirmation requirements.
type CleanupRequest struct {
	ExportEnabled               bool
	Cursor                      *CleanupCursor
	Table                       string
	Cutoff                      time.Time
	Limit                       int
	Exporting                   bool
	CoveredFrom, CoveredThrough time.Time
	RequireConfirmedExport      bool
}

// CleanupResult 只统计成功提交的删除和跳过。
// CleanupResult counts committed deletions and skips only.
type CleanupResult struct{ Deleted, Skipped int64 }

// RetentionStore 的删除与成功账本写入必须同一事务。
// RetentionStore atomically deletes rows and records successful cleanup evidence.
type RetentionStore interface {
	ScanCoverage(context.Context, string) (time.Time, time.Time, bool, error)
	CleanupBatch(context.Context, CleanupRequest) (CleanupRun, error)
	RecordSkipped(context.Context, string, time.Time, string) error
}

// RetentionCleaner 不启动任务或接收业务写口。
// RetentionCleaner starts no tasks and receives no business mutation port.
type RetentionCleaner struct {
	mu        sync.Mutex
	progress  map[string]*CleanupCursor
	store     RetentionStore
	policies  []RetentionPolicy
	exporting bool
}

// DefaultRetentionPolicies 提供候选保留期，全部保持关闭。
// DefaultRetentionPolicies supplies candidate durations with every policy disabled.
func DefaultRetentionPolicies() []RetentionPolicy {
	return []RetentionPolicy{
		{Table: "diagnostic_events", ExportSource: true, Retention: 30 * 24 * time.Hour, BatchSize: 1000, MaxBatches: 20},
		{Table: "context_assembly_records", Retention: 30 * 24 * time.Hour, BatchSize: 1000, MaxBatches: 20},
		{Table: "alert_history", Retention: 30 * 24 * time.Hour, BatchSize: 1000, MaxBatches: 20},
		{Table: "export_history", Retention: 30 * 24 * time.Hour, BatchSize: 1000, MaxBatches: 20},
	}
}

// NewRetentionCleaner 显式复制策略，不会因导出开关自动启用清理。
// NewRetentionCleaner copies policy and never enables cleanup implicitly.
func NewRetentionCleaner(store RetentionStore, policies []RetentionPolicy, exporting bool) *RetentionCleaner {
	if policies == nil {
		policies = DefaultRetentionPolicies()
	}
	return &RetentionCleaner{store: store, policies: append([]RetentionPolicy(nil), policies...), exporting: exporting, progress: map[string]*CleanupCursor{}}
}

// CleanupOnce 在启用策略内按覆盖与分页边界清理。
// CleanupOnce cleans enabled policies within coverage and pagination bounds.
func (c *RetentionCleaner) CleanupOnce(ctx context.Context, now time.Time) (CleanupResult, error) {
	var total CleanupResult
	if c == nil || now.IsZero() {
		return total, ErrInvalidQuery
	}
	if !c.mu.TryLock() {
		return total, errors.New("retention cleaner busy")
	}
	defer c.mu.Unlock()
	for _, policy := range c.policies {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if !slices.Contains([]string{"diagnostic_events", "context_assembly_records", "alert_history", "export_history"}, policy.Table) || policy.ExportSource != (policy.Table == "diagnostic_events") {
			return total, ErrInvalidQuery
		}
		if !policy.Enabled {
			continue
		}
		if c.store == nil || policy.Retention <= 0 || policy.BatchSize < 1 || policy.BatchSize > 10000 || policy.MaxBatches < 1 || policy.MaxBatches > 100 {
			return total, ErrInvalidQuery
		}
		cutoff := now.Add(-policy.Retention)
		var from, through time.Time
		guarded := c.exporting && policy.ExportSource
		if guarded {
			var covered bool
			var err error
			from, through, covered, err = c.store.ScanCoverage(ctx, policy.Table)
			if err != nil {
				return total, fmt.Errorf("read cleanup scan coverage: %w", err)
			}
			reason := ""
			if !covered || from.IsZero() || through.IsZero() || from.After(through) {
				reason = "scan_not_covered"
			} else if !cutoff.After(from) {
				reason = "out_of_coverage"
			}
			if reason != "" {
				if err := c.store.RecordSkipped(ctx, policy.Table, now, reason); err != nil {
					return total, fmt.Errorf("record skipped cleanup: %w", err)
				}
				continue
			}
		}
		cursor := c.progress[policy.Table]
		if cursor != nil && cursor.At.After(cutoff) {
			cursor = nil
			delete(c.progress, policy.Table)
		}
		for range policy.MaxBatches {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			run, err := c.store.CleanupBatch(ctx, CleanupRequest{Table: policy.Table, Cutoff: cutoff, Limit: policy.BatchSize, Exporting: guarded, CoveredFrom: from, CoveredThrough: through, RequireConfirmedExport: guarded, Cursor: cursor, ExportEnabled: c.exporting})
			if err != nil {
				return total, fmt.Errorf("cleanup batch: %w", err)
			}
			processed := run.Deleted + run.Skipped
			if run.Deleted < 0 || run.Skipped < 0 || processed > int64(policy.BatchSize) {
				return total, errors.New("invalid cleanup batch result")
			}
			total.Deleted, total.Skipped = total.Deleted+run.Deleted, total.Skipped+run.Skipped
			if processed < int64(policy.BatchSize) {
				delete(c.progress, policy.Table)
				break
			}
			if run.Next == nil || cursor != nil && (run.Next.At.Before(cursor.At) || run.Next.At.Equal(cursor.At) && run.Next.ID <= cursor.ID) {
				return total, errors.New("invalid cleanup page cursor")
			}
			cursor = run.Next
			next := *cursor
			c.progress[policy.Table] = &next
		}
	}
	return total, nil
}
