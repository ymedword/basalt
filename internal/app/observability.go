package app

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/historyretention"
	"github.com/ymedword/basalt/internal/observability"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// ObservabilityConfig 复用装载层同一配置，不另造配置/凭据来源。
// ObservabilityConfig reuses the loader's configuration and credential source.
type ObservabilityConfig = cli.ObservabilityConfig

// Deps 只显式传递观测依赖，不接受Run/工具/恢复执行服务。
// Deps explicitly carries observation dependencies, never run/tool/recovery services.
type Deps struct {
	Store       *postgres.ObservabilityStore
	Diagnostics *diagnostics.Recorder
	Clock       func() time.Time
	Metrics     *observability.MetricsCollector
	SampleRate  *float64
	Logger      *slog.Logger
}

// Observability 的单个后台循环只随serve生命周期启动和取消。
// Observability owns one background loop bound to the serve lifecycle.
type Observability struct {
	Enabled         bool
	Projector       *observability.Projector
	Metrics         *observability.MetricsCollector
	MetricsExport   *observability.MetricsExporter
	Alerts          *observability.AlertEngine
	Export          *observability.ExportWorker
	Cleaner         *observability.RetentionCleaner
	History         historyretention.Store
	Files           *historyretention.Files
	fileView        observability.FileRetentionView
	SessionGate     historyretention.SessionGate
	historyProgress map[string]string
	store           *postgres.ObservabilityStore
	cfg             ObservabilityConfig
	clock           func() time.Time
	logger          *slog.Logger
	sender          *otlpSender
	mu              sync.Mutex
	cancel          context.CancelFunc
	done            chan struct{}
	doneOnce        sync.Once
	started, closed bool
	alertViews      []observability.AlertStateView
}

// NewObservability 仅构造，不在配置解析或命令请求路径启动工作。
// NewObservability only assembles components and never starts background work.
func NewObservability(ctx context.Context, cfg ObservabilityConfig, deps Deps) (*Observability, error) {
	if ctx == nil {
		return nil, errors.New("observability context unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg = cfg.Effective()
	if err := cli.ValidateObservabilityConfig(cfg); err != nil {
		return nil, err
	}
	enabled, alertsEnabled := *cfg.Enabled, *cfg.Alerts.Enabled
	cfg.Enabled, cfg.Alerts.Enabled = &enabled, &alertsEnabled
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	o := &Observability{Enabled: *cfg.Enabled, cfg: cfg, store: deps.Store, clock: clock, logger: logger, done: make(chan struct{})}
	if !o.Enabled {
		return o, nil
	}
	if deps.Store == nil {
		return nil, errors.New("observability store unavailable")
	}
	deps.Store = deps.Store.WithClock(clock)
	o.store = deps.Store
	metrics := deps.Metrics
	if metrics == nil {
		metrics = observability.NewMetricsCollector(deps.Store, clock)
	}
	rate := float64(1)
	if deps.SampleRate != nil {
		rate = *deps.SampleRate
	}
	if rate < 0 || rate > 1 || math.IsNaN(rate) {
		return nil, errors.New("invalid observability diagnostic sample rate")
	}
	o.Metrics = metrics
	o.Projector = observability.NewProjector(observability.Ports{Runs: deps.Store, Exec: deps.Store, Tasks: deps.Store, Ops: deps.Store, Ingest: deps.Store, Assembly: deps.Store, Diagnostics: deps.Store, Cleanup: deps.Store}, observability.ProjectorConfig{SampleRate: rate, Now: clock})
	rules := observability.BuiltinAlertRules(metrics, deps.Store)
	o.Alerts = observability.NewAlertEngine(observationAlertStore{ObservabilityStore: deps.Store, metrics: metrics}, rules, clock)
	policies := observability.DefaultRetentionPolicies()
	days := map[string]int{"diagnostic_events": cfg.Retention.DiagnosticEventsDays, "context_assembly_records": cfg.Retention.AssemblyRecordsDays, "alert_history": cfg.Retention.AlertHistoryDays, "export_history": cfg.Retention.ExportHistoryDays}
	for i := range policies {
		policies[i].Enabled = cfg.Retention.CleanupEnabled
		policies[i].Retention = time.Duration(days[policies[i].Table]) * 24 * time.Hour
		policies[i].BatchSize = cfg.Retention.BatchSize
		policies[i].MaxBatches = cfg.Retention.MaxBatchesPerRun
	}
	o.Cleaner = observability.NewRetentionCleaner(observationRetentionStore{ObservabilityStore: deps.Store, metrics: metrics}, policies, cfg.Export.Enabled)
	if cfg.Export.Enabled {
		headers, err := cfg.Export.ParsedHeaders()
		if err != nil {
			return nil, err
		}
		sender, err := newOTLPSender(otlpSenderConfig{Endpoint: cfg.Export.Endpoint, Protocol: cfg.Export.Protocol, Headers: headers, Timeout: cfg.Export.Timeout})
		if err != nil {
			return nil, err
		}
		o.sender = sender
		ledger := observationExportLedger{ObservabilityStore: deps.Store, metrics: metrics}
		exportCfg := observability.ExportConfig{BatchSize: cfg.Export.BatchSize, LeaseTTL: cfg.Export.LeaseTTL, MaxAttempts: cfg.Export.MaxAttempts, MaxItemAge: cfg.Export.MaxItemAge, ScanOverlap: cfg.Export.ScanOverlap, SafetyLag: cfg.Export.SafetyLag, ReconcileHorizon: cfg.Export.ReconcileHorizon, InitialBackfill: cfg.Export.InitialBackfill, ScanPageSize: cfg.Export.ScanPageSize, MetricsInterval: cfg.Export.MetricsInterval}
		o.Export = observability.NewExportWorker(observability.NewScanner(deps.Store, exportCfg), ledger, observationSender{sender: sender, metrics: metrics}, exportCfg, clock)
		o.MetricsExport = observability.NewMetricsExporter(metrics, ledger, cfg.Export.MetricsInterval, clock)
	}
	return o, nil
}

// Start 只能调用一次，与调用方已有runCtx共享取消源。
// Start runs once using the caller's existing cancellation source.
func (o *Observability) Start(ctx context.Context) error {
	if o == nil || ctx == nil {
		return errors.New("observability lifecycle unavailable")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.started {
		return errors.New("observability lifecycle already started or closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o.started = true
	interval := o.interval()
	if !o.Enabled || interval == 0 {
		o.doneOnce.Do(func() { close(o.done) })
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	o.cancel = cancel
	go o.run(workerCtx, interval)
	return nil
}

// Close 等待后台退出，必须早于共享数据库关闭。
// Close waits for background work before the shared database closes.
func (o *Observability) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	o.closed = true
	cancel, started := o.cancel, o.started
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !started {
		o.doneOnce.Do(func() { close(o.done) })
	}
	<-o.done
	if o.sender != nil {
		o.sender.close()
	}
	return nil
}

func (o *Observability) interval() time.Duration {
	var interval time.Duration
	include := func(value time.Duration) {
		if interval == 0 || value < interval {
			interval = value
		}
	}
	if o.cfg.Export.Enabled {
		include(o.cfg.Export.MetricsInterval)
	}
	if o.cfg.Alerts.Enabled != nil && *o.cfg.Alerts.Enabled {
		include(o.cfg.Alerts.EvaluationInterval)
	}
	if o.cfg.Retention.CleanupEnabled || o.cfg.Retention.HistoryEnabled {
		include(o.cfg.Retention.Interval)
	}
	return interval
}

func (o *Observability) run(ctx context.Context, interval time.Duration) {
	defer o.doneOnce.Do(func() { close(o.done) })
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	last := map[string]time.Time{}
	for {
		o.round(ctx, last)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (o *Observability) round(ctx context.Context, last map[string]time.Time) {
	if ctx.Err() != nil {
		return
	}
	now := o.clock()
	due := func(name string, interval time.Duration) bool {
		previous := last[name]
		if previous.IsZero() || now.Before(previous) || now.Sub(previous) >= interval {
			last[name] = now
			return true
		}
		return false
	}
	perform := func(name string, work func(context.Context) error) {
		if ctx.Err() != nil {
			return
		}
		taskCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := work(taskCtx); err != nil && ctx.Err() == nil {
			o.logger.WarnContext(ctx, "observability background operation failed", "operation", name, "error_kind", name+"_failed")
		}
	}
	if *o.cfg.Alerts.Enabled && due("alerts", o.cfg.Alerts.EvaluationInterval) {
		perform("alerts", func(ctx context.Context) error {
			states, err := o.Alerts.Evaluate(ctx, now)
			o.mu.Lock()
			o.alertViews = append([]observability.AlertStateView(nil), states...)
			o.mu.Unlock()
			return err
		})
	}
	if o.cfg.Export.Enabled && due("export", o.cfg.Export.MetricsInterval) {
		perform("export", func(ctx context.Context) error {
			if err := o.MetricsExport.EnqueueOnce(ctx); err != nil {
				return err
			}
			_, err := o.Export.ExportOnce(ctx)
			return err
		})
	}
	if o.cfg.Retention.CleanupEnabled && due("cleanup", o.cfg.Retention.Interval) {
		perform("cleanup", func(ctx context.Context) error { _, err := o.Cleaner.CleanupOnce(ctx, now); return err })
	}
	if o.cfg.Retention.HistoryEnabled && due("history", o.cfg.Retention.Interval) {
		perform("history", func(ctx context.Context) error {
			if o.Files != nil {
				n, fileErr := o.Files.Cleanup(ctx)
				at := o.clock()
				o.mu.Lock()
				o.fileView = observability.FileRetentionView{Enabled: true, Days: o.cfg.Retention.HistoryDays, LogFileMiB: int(historyretention.LogFileBytes >> 20), LogTotalMiB: int(historyretention.LogTotalBytes >> 20), MinVerifiedBackups: 2, LastRunAt: &at, DeletedFiles: n, Status: "succeeded"}
				if fileErr != nil {
					o.fileView.Status = "failed"
				}
				o.mu.Unlock()
				if fileErr != nil {
					o.logger.WarnContext(ctx, "file retention failed", "error_kind", "cleanup_failed")
				}
			}
			if o.History == nil || o.SessionGate == nil {
				return errors.New("history maintenance unavailable")
			}
			if o.historyProgress == nil {
				o.historyProgress = map[string]string{}
			}
			for _, group := range historyretention.Groups {
				for range o.cfg.Retention.MaxBatchesPerRun {
					r, err := o.History.CleanupHistoryBatch(ctx, group, now.Add(-time.Duration(o.cfg.Retention.HistoryDays)*24*time.Hour), min(o.cfg.Retention.BatchSize, 1000), o.historyProgress[group], o.SessionGate)
					if err != nil {
						return err
					}
					if r.Scanned < min(o.cfg.Retention.BatchSize, 1000) {
						delete(o.historyProgress, group)
						break
					}
					o.historyProgress[group] = r.Next
				}
			}
			return nil
		})
	}
}
