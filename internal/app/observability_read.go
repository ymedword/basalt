package app

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/ymedword/basalt/internal/historyretention"
	"github.com/ymedword/basalt/internal/observability"
)

// ObservationEnabled 只报告生效的启动快照，不进行惰性初始化。
// ObservationEnabled reports the effective startup snapshot without lazy initialization.
func (o *Observability) ObservationEnabled() bool { return o != nil && o.Enabled }

func (o *Observability) readReady() error {
	if !o.ObservationEnabled() || o.store == nil {
		return errors.New("observability unavailable")
	}
	return nil
}

// Overview 只读事实，不触发后台评估或业务执行。
// Overview reads facts without evaluating alerts or executing business work.
func (o *Observability) Overview(ctx context.Context, q observability.OverviewQuery) (observability.Overview, error) {
	if err := o.readReady(); err != nil {
		return observability.Overview{}, err
	}
	return o.Projector.Overview(ctx, q)
}

// Timeline 复用同一只读投影，来源失败由明确缺口表达。
// Timeline reuses the read-only projection with explicit source-failure gaps.
func (o *Observability) Timeline(ctx context.Context, q observability.TimelineQuery) (observability.Timeline, error) {
	if err := o.readReady(); err != nil {
		return observability.Timeline{}, err
	}
	return o.Projector.Timeline(ctx, q)
}

// Context 不读取或返回消息、工具、知识正文。
// Context reads and returns no message, tool, or knowledge bodies.
func (o *Observability) Context(ctx context.Context, id string) (observability.ContextDiagnosis, error) {
	if err := o.readReady(); err != nil {
		return observability.ContextDiagnosis{}, err
	}
	return o.Projector.Context(ctx, id)
}

// MetricsSnapshot 采样当前只读计数，不写持久快照或导出账本。
// MetricsSnapshot reads current counters without persisting snapshots or export items.
func (o *Observability) MetricsSnapshot(ctx context.Context, window time.Duration) (observability.MetricsSnapshot, error) {
	if err := o.readReady(); err != nil {
		return observability.MetricsSnapshot{}, err
	}
	return o.Metrics.Snapshot(ctx, window)
}

// AlertStatus 读取已评估状态；GET不调用Evaluate或写转换。
// AlertStatus reads evaluated states; GET never evaluates rules or writes transitions.
func (o *Observability) AlertStatus(ctx context.Context, q observability.ReadWindow) (observability.AlertStatus, error) {
	out := observability.AlertStatus{Rules: []observability.AlertRuleStatus{}, Transitions: []observability.AlertTransition{}}
	if err := q.Validate(); err != nil {
		return out, err
	}
	if err := o.readReady(); err != nil {
		return out, err
	}
	out.Enabled = *o.cfg.Alerts.Enabled
	states, err := o.store.LoadStates(ctx)
	if err != nil {
		return out, err
	}
	o.mu.Lock()
	cached := slices.Clone(o.alertViews)
	o.mu.Unlock()
	for _, s := range cached {
		key := s.RuleID + "/" + s.ScopeKey
		old, ok := states[key]
		if !ok || !s.LastEvalAt.Before(old.LastEvalAt) {
			states[key] = s
		}
	}
	for _, r := range observability.BuiltinAlertRules(o.Metrics, o.store) {
		v := observability.AlertRuleStatus{RuleID: r.ID, Enabled: out.Enabled && r.Enabled, WindowSeconds: int64(r.Window / time.Second), ForSeconds: int64(r.For / time.Second), States: []observability.AlertStateView{}}
		initial := observability.InitialAlertState(r.ID, v.Enabled)
		if v.Enabled {
			for _, s := range states {
				if s.RuleID == r.ID && s.ScopeKey == initial.ScopeKey {
					v.States = append(v.States, s)
				}
			}
		}
		if len(v.States) == 0 {
			v.States = append(v.States, initial)
		}
		out.Rules = append(out.Rules, v)
	}
	out.Transitions, err = o.store.AlertTransitions(ctx, q)
	if err != nil {
		return out, err
	}
	if len(out.Transitions) > q.Limit {
		out.Transitions = out.Transitions[:q.Limit]
		out.Truncated = true
	}
	return out, nil
}

// ExportStatus 不返回端点或header的值，只报告是否配置及持久结果。
// ExportStatus returns configuration presence and durable results, never endpoint or header values.
func (o *Observability) ExportStatus(ctx context.Context) (observability.ExportStatus, error) {
	if err := o.readReady(); err != nil {
		return observability.ExportStatus{}, err
	}
	out, err := o.store.ExportStatus(ctx)
	if err != nil {
		return out, err
	}
	out.Enabled = o.cfg.Export.Enabled
	out.EndpointConfigured = o.cfg.Export.Endpoint != ""
	out.Protocol = o.cfg.Export.Protocol
	headers, err := o.cfg.Export.ParsedHeaders()
	if err != nil {
		return out, err
	}
	out.HeadersConfigured = len(headers) > 0
	return out, nil
}

// CleanupStatus 返回有界窗口账本与生效策略，不运行清理或声称逐条删除原因。
// CleanupStatus returns bounded ledgers and policies without running cleanup or claiming per-row deletion causes.
func (o *Observability) CleanupStatus(ctx context.Context, q observability.ReadWindow) (observability.CleanupStatus, error) {
	out := observability.CleanupStatus{History: []historyretention.Report{}, Policies: []observability.RetentionPolicyView{}, Runs: []observability.CleanupRun{}}
	if err := q.Validate(); err != nil {
		return out, err
	}
	if err := o.readReady(); err != nil {
		return out, err
	}
	out.Enabled = o.cfg.Retention.CleanupEnabled
	out.HistoryEnabled = o.cfg.Retention.HistoryEnabled
	out.HistoryDays = o.cfg.Retention.HistoryDays
	o.mu.Lock()
	fv := o.fileView
	o.mu.Unlock()
	if fv.Status == "" {
		fv = observability.FileRetentionView{Enabled: o.cfg.Retention.HistoryEnabled, Days: o.cfg.Retention.HistoryDays, LogFileMiB: 32, LogTotalMiB: 256, MinVerifiedBackups: 2, Status: "not_run"}
	}
	out.Files = fv
	if o.History != nil {
		history, err := o.History.HistoryReports(ctx, q.From, q.To, q.Limit)
		if err != nil {
			return out, err
		}
		out.History = history
	}
	days := map[string]int{"diagnostic_events": o.cfg.Retention.DiagnosticEventsDays, "context_assembly_records": o.cfg.Retention.AssemblyRecordsDays, "alert_history": o.cfg.Retention.AlertHistoryDays, "export_history": o.cfg.Retention.ExportHistoryDays}
	for _, p := range observability.DefaultRetentionPolicies() {
		out.Policies = append(out.Policies, observability.RetentionPolicyView{Table: p.Table, Enabled: out.Enabled, RetentionDays: days[p.Table], BatchSize: o.cfg.Retention.BatchSize, MaxBatches: o.cfg.Retention.MaxBatchesPerRun, RequireConfirmedExport: o.cfg.Export.Enabled && p.ExportSource})
	}
	rows, err := o.store.CleanupHistory(ctx, q)
	if err != nil {
		return out, err
	}
	out.Truncated = len(rows) > q.Limit
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	out.Runs = rows
	return out, nil
}
