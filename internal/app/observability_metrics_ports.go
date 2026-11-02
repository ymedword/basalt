package app

import (
	"context"
	"time"

	"github.com/ymedword/basalt/internal/observability"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

type observationExportLedger struct {
	*postgres.ObservabilityStore
	metrics *observability.MetricsCollector
}

func (s observationExportLedger) ApplySendResult(ctx context.Context, batch observability.ExportBatch, result observability.SendResult, sendErr error, now time.Time) error {
	if err := s.ObservabilityStore.ApplySendResult(ctx, batch, result, sendErr, now); err != nil {
		return err
	}
	d := observability.ResolveSendResult(batch, result, sendErr)
	for _, item := range batch.Items {
		label := map[observability.ExportState]string{observability.ExportSent: "sent", observability.ExportUnconfirmed: "unconfirmed", observability.ExportRetry: "retryable", observability.ExportFailedPermanent: "permanent"}[d.State]
		if d.State == observability.ExportRetry && (item.Attempts >= batch.MaxAttempts || now.Sub(item.CreatedAt) >= batch.MaxItemAge) {
			label = "permanent"
		}
		s.metrics.IncCounter("obs_export_items_total", map[string]string{"source_kind": string(item.SourceKind), "result": label})
	}
	return nil
}

func (s observationExportLedger) MarkStale(ctx context.Context, items []observability.ExportItem) error {
	if err := s.ObservabilityStore.MarkStale(ctx, items); err != nil {
		return err
	}
	for _, item := range items {
		s.metrics.IncCounter("obs_export_items_total", map[string]string{"source_kind": string(item.SourceKind), "result": "stale"})
	}
	return nil
}

func (s observationExportLedger) FailExportItems(ctx context.Context, items []observability.ExportItem, kind string, now time.Time) error {
	if err := s.ObservabilityStore.FailExportItems(ctx, items, kind, now); err != nil {
		return err
	}
	for _, item := range items {
		s.metrics.IncCounter("obs_export_items_total", map[string]string{"source_kind": string(item.SourceKind), "result": "permanent"})
	}
	return nil
}

type observationSender struct {
	sender  observability.Sender
	metrics *observability.MetricsCollector
}

func (s observationSender) Send(ctx context.Context, batch observability.ExportBatch) (observability.SendResult, error) {
	start := time.Now()
	result, err := s.sender.Send(ctx, batch)
	label := "error"
	d := observability.ResolveSendResult(batch, result, err)
	if d.State == observability.ExportSent {
		label = "ok"
		if d.Warning != "" {
			label = "warning"
		}
	} else if d.State == observability.ExportUnconfirmed {
		label = "partial"
	}
	s.metrics.ObserveExportDuration(time.Since(start), label)
	return result, err
}

type observationAlertStore struct {
	*postgres.ObservabilityStore
	metrics *observability.MetricsCollector
}

func (s observationAlertStore) ApplyAlertEvaluation(ctx context.Context, from, to observability.AlertStateView, at time.Time) error {
	if err := s.ObservabilityStore.ApplyAlertEvaluation(ctx, from, to, at); err != nil {
		return err
	}
	if from.State != to.State || from.Reason != to.Reason || !from.RecoverySince.Equal(to.RecoverySince) {
		target := to.State
		if target == observability.StateInactive && (from.State == observability.StateFiring || from.State == observability.StateUnknown && from.PreviousState == observability.StateFiring) {
			target = observability.StateResolved
		}
		s.metrics.IncCounter("obs_alert_transitions_total", map[string]string{"rule_id": to.RuleID, "to_state": string(target)})
	}
	return nil
}

type observationRetentionStore struct {
	*postgres.ObservabilityStore
	metrics *observability.MetricsCollector
}

func (s observationRetentionStore) CleanupBatch(ctx context.Context, req observability.CleanupRequest) (observability.CleanupRun, error) {
	run, err := s.ObservabilityStore.CleanupBatch(ctx, req)
	if err != nil {
		return run, err
	}
	for range run.Deleted {
		s.metrics.IncCounter("obs_cleanup_deleted_rows_total", map[string]string{"table_kind": req.Table})
	}
	for range run.Skipped {
		s.metrics.IncCounter("obs_cleanup_skipped_rows_total", map[string]string{"reason": run.SkipReason})
	}
	return run, nil
}
