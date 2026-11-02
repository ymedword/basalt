package observability

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/diagnostics"
)

type workerLedger struct {
	items            map[string]ExportItem
	batches          map[string]ExportDisposition
	beginErr, ackErr error
	now              time.Time
}

func (l *workerLedger) Enqueue(_ context.Context, items []ExportItem) error {
	for _, item := range items {
		if _, ok := l.items[item.ItemID]; !ok {
			item.State = ExportPending
			item.CreatedAt = l.now
			l.items[item.ItemID] = item
		}
	}
	return nil
}

func (l *workerLedger) Claim(_ context.Context, limit int, now time.Time, ttl time.Duration) ([]ExportItem, error) {
	keys := make([]string, 0, len(l.items))
	for k := range l.items {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []ExportItem
	for _, key := range keys {
		item := l.items[key]
		ready := item.State == ExportPending || item.State == ExportRetry && (item.NextAttemptAt == nil || !now.Before(*item.NextAttemptAt)) || item.State == ExportInflight && item.LeaseUntil != nil && !now.Before(*item.LeaseUntil)
		if !ready {
			continue
		}
		until := now.Add(ttl)
		item.Attempts++
		item.State = ExportInflight
		item.LeaseUntil = &until
		l.items[key] = item
		out = append(out, item)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (l *workerLedger) MarkStale(_ context.Context, items []ExportItem) error {
	for _, item := range items {
		v := l.items[item.ItemID]
		v.State = ExportStale
		l.items[item.ItemID] = v
	}
	return nil
}

func (l *workerLedger) FailExportItems(_ context.Context, items []ExportItem, kind string, _ time.Time) error {
	for _, item := range items {
		v := l.items[item.ItemID]
		v.State = ExportFailedPermanent
		v.LastErrorKind = kind
		l.items[item.ItemID] = v
	}
	return nil
}

func (l *workerLedger) BeginBatch(_ context.Context, b ExportBatch, _ time.Time) error {
	if l.beginErr != nil {
		return l.beginErr
	}
	l.batches[b.BatchID] = ExportDisposition{Outcome: "unknown"}
	for _, item := range b.Items {
		v := l.items[item.ItemID]
		v.LastBatchID = b.BatchID
		l.items[item.ItemID] = v
	}
	return nil
}

func (l *workerLedger) ApplySendResult(_ context.Context, b ExportBatch, result SendResult, sendErr error, now time.Time) error {
	if l.ackErr != nil {
		return l.ackErr
	}
	d := ResolveSendResult(b, result, sendErr)
	l.batches[b.BatchID] = d
	for _, item := range b.Items {
		v := l.items[item.ItemID]
		v.State = d.State
		if v.State == ExportRetry && (item.Attempts >= b.MaxAttempts || now.Sub(v.CreatedAt) >= b.MaxItemAge) {
			v.State = ExportFailedPermanent
			v.LastErrorKind = "retry_exhausted"
		}
		if d.State == ExportRetry {
			due := now.Add(time.Second)
			v.NextAttemptAt = &due
		}
		l.items[item.ItemID] = v
	}
	return nil
}
func (l *workerLedger) PendingCount(context.Context) (int64, error)             { return 0, nil }
func (l *workerLedger) OldestPendingAge(context.Context) (time.Duration, error) { return 0, nil }
func (l *workerLedger) UnconfirmedCount(context.Context) (int64, error)         { return 0, nil }

type workerScanner struct {
	ledger             *workerLedger
	facts              map[string]ExportItem
	scanErr, reloadErr error
}

func (s *workerScanner) Scan(ctx context.Context, _ time.Time) (ScanResult, error) {
	if s.scanErr != nil {
		return ScanResult{}, s.scanErr
	}
	var items []ExportItem
	for _, item := range s.facts {
		items = append(items, item)
	}
	return ScanResult{}, s.ledger.Enqueue(ctx, items)
}

func (s *workerScanner) Reload(_ context.Context, item ExportItem) (ExportItem, bool, error) {
	v, ok := s.facts[item.ItemID]
	return v, ok, s.reloadErr
}

type workerSender struct {
	batches []ExportBatch
	result  *SendResult
	err     error
	before  func(ExportBatch)
}

func (s *workerSender) Send(_ context.Context, b ExportBatch) (SendResult, error) {
	s.batches = append(s.batches, b)
	if s.before != nil {
		s.before(b)
	}
	if s.result != nil {
		return *s.result, s.err
	}
	return SendResult{Accepted: b.RecordCount}, s.err
}

func workerFixture(t *testing.T, count int) (*ExportWorker, *workerScanner, *workerLedger, *workerSender, *time.Time) {
	t.Helper()
	now := time.UnixMilli(1700000000000).UTC()
	l := &workerLedger{items: map[string]ExportItem{}, batches: map[string]ExportDisposition{}, now: now}
	s := &workerScanner{ledger: l, facts: map[string]ExportItem{}}
	for i := range count {
		id := string(rune('a' + i))
		finish := now
		f := RunFacts{RunID: id, Status: "completed", StartedAt: now, FinishedAt: &finish}
		item := mapperItem(t, SourceRun, id, SignalTrace, f)
		hash, err := ExportStateHash(item)
		require.NoError(t, err)
		item.StateHash = hash
		item.ItemID = ExportItemKey(item.SourceKind, item.SourceID, hash)
		s.facts[item.ItemID] = item
	}
	sender := &workerSender{}
	w := NewExportWorker(s, l, sender, DefaultExportConfig(), func() time.Time { return now })
	return w, s, l, sender, &now
}

func TestObservability_ExportPartialSuccessBatch(t *testing.T) {
	w, _, ledger, sender, _ := workerFixture(t, 3)
	sender.result = &SendResult{Accepted: 2, Rejected: 1, PartialSuccess: true}
	result, err := w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 3, result.Unconfirmed)
	require.Zero(t, result.Sent)
	for _, item := range ledger.items {
		require.Equal(t, ExportUnconfirmed, item.State)
	}
	require.Len(t, sender.batches, 1)
}

func TestObservability_ExportPartialNoRetry(t *testing.T) {
	w, _, _, sender, now := workerFixture(t, 3)
	sender.result = &SendResult{Accepted: 2, Rejected: 1, PartialSuccess: true}
	_, err := w.ExportOnce(t.Context())
	require.NoError(t, err)
	*now = now.Add(time.Hour)
	_, err = w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Len(t, sender.batches, 1)
}

func TestObservability_ExportPartialWarningNoReject(t *testing.T) {
	w, _, ledger, sender, _ := workerFixture(t, 3)
	sender.result = &SendResult{Accepted: 3, PartialSuccess: true, Warning: "password=CANARY_RECEIVER"}
	result, err := w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 3, result.Sent)
	require.Zero(t, result.Unconfirmed)
	for _, batch := range ledger.batches {
		require.Equal(t, "sent_with_warning", batch.Outcome)
		require.Equal(t, "receiver_warning", batch.Warning)
	}
}

func TestObservability_ExportPartialInvalidCount(t *testing.T) {
	w, _, ledger, sender, _ := workerFixture(t, 3)
	sender.result = &SendResult{Rejected: 4, PartialSuccess: true}
	result, err := w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 3, result.Unconfirmed)
	for _, batch := range ledger.batches {
		require.Equal(t, "protocol_invalid", batch.ErrorKind)
		require.Nil(t, batch.Accepted)
		require.Nil(t, batch.Rejected)
	}
}

func TestObservability_ExportCrashBeforeAckDuplicates(t *testing.T) { testWorkerCrashWindow(t, false) }

func TestObservability_ExportPartialSuccessCrashBeforeAck(t *testing.T) {
	testWorkerCrashWindow(t, true)
}

func testWorkerCrashWindow(t *testing.T, partial bool) {
	t.Helper()
	w, _, ledger, sender, now := workerFixture(t, 3)
	ledger.ackErr = errors.New("synthetic lost acknowledgment")
	if partial {
		sender.result = &SendResult{Accepted: 2, Rejected: 1, PartialSuccess: true}
	}
	result, err := w.ExportOnce(t.Context())
	require.Error(t, err)
	require.Zero(t, result.Sent)
	require.Zero(t, result.Unconfirmed)
	require.Len(t, sender.batches, 1)
	require.Len(t, ledger.batches, 1)
	for _, b := range ledger.batches {
		require.Equal(t, "unknown", b.Outcome)
		require.Nil(t, b.Accepted)
		require.Nil(t, b.Rejected)
	}
	for _, item := range ledger.items {
		require.Equal(t, ExportInflight, item.State)
	}
	ledger.ackErr = nil
	_, err = w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Len(t, sender.batches, 1, "unexpired lease prevents resend")
	*now = now.Add(6 * time.Minute)
	_, err = w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Len(t, sender.batches, 2, "lost ack can duplicate a received request")
	require.Len(t, ledger.batches, 2)
	unknown := 0
	for _, b := range ledger.batches {
		if b.Outcome == "unknown" {
			unknown++
			require.Nil(t, b.Accepted)
			require.Nil(t, b.Rejected)
		}
	}
	require.Equal(t, 1, unknown)
}

func TestObservability_ExportStaleStateHash(t *testing.T) {
	w, scanner, ledger, sender, _ := workerFixture(t, 1)
	for _, item := range scanner.facts {
		require.NoError(t, ledger.Enqueue(t.Context(), []ExportItem{item}))
		item.StateHash = "different-state"
		scanner.facts[item.ItemID] = item
	}
	result, err := w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, result.Stale)
	require.Empty(t, sender.batches)
	for _, item := range ledger.items {
		require.Equal(t, ExportStale, item.State)
	}
}

func TestObservability_ExportAttemptBeforeNetwork(t *testing.T) {
	w, _, ledger, sender, _ := workerFixture(t, 1)
	sender.before = func(batch ExportBatch) {
		require.Contains(t, ledger.batches, batch.BatchID)
		require.Equal(t, "unknown", ledger.batches[batch.BatchID].Outcome)
		require.Nil(t, ledger.batches[batch.BatchID].Accepted)
	}
	_, err := w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Len(t, sender.batches, 1)
	w, _, ledger, sender, _ = workerFixture(t, 1)
	ledger.beginErr = errors.New("synthetic attempt failure")
	_, err = w.ExportOnce(t.Context())
	require.Error(t, err)
	require.Empty(t, sender.batches)
	require.Empty(t, ledger.batches)
}

func TestObservability_ExportDoesNotTouchBusiness(t *testing.T) {
	w, scanner, _, sender, _ := workerFixture(t, 1)
	before := map[string]ExportItem{}
	for key, item := range scanner.facts {
		before[key] = item
	}
	_, err := w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Len(t, sender.batches, 1)
	require.Equal(t, before, scanner.facts, "authoritative facts are read-only")
}

func addWorkerFact(t *testing.T, scanner *workerScanner, item ExportItem) {
	t.Helper()
	hash, err := ExportStateHash(item)
	require.NoError(t, err)
	item.StateHash = hash
	item.ItemID = ExportItemKey(item.SourceKind, item.SourceID, hash)
	scanner.facts[item.ItemID] = item
}

func TestObservability_ExportMixedSignalsSeparateBatches(t *testing.T) {
	w, scanner, _, sender, now := workerFixture(t, 1)
	ev := diagnostics.DiagnosticEvent{EventID: "event", Kind: diagnostics.KindPermissionDecision, OccurredAt: now.UnixMilli(), Correlation: diagnostics.Correlation{RunID: "a"}}
	addWorkerFact(t, scanner, mapperItem(t, SourceDiagnostic, "event", SignalLog, ev))
	snapshot := MetricsSnapshot{CollectedAt: *now, Samples: []MetricSample{{Name: "obs_tool_denials_total", Type: MetricGauge, Scope: ScopeDatabase, Window: time.Hour, Available: true, Value: 1}}}
	addWorkerFact(t, scanner, mapperItem(t, SourceMetricSnapshot, "snapshot", SignalMetric, snapshot))
	result, err := w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 3, result.Sent)
	require.Equal(t, 3, result.Batches)
	require.Len(t, sender.batches, 3)
	for i, signal := range []SignalKind{SignalTrace, SignalLog, SignalMetric} {
		batch := sender.batches[i]
		require.Equal(t, signal, batch.Signal)
		require.Len(t, batch.Items, 1)
		p := batch.Payload.(TelemetryPayload)
		switch signal {
		case SignalTrace:
			require.Len(t, p.Spans, 1)
			require.Empty(t, p.Logs)
			require.Empty(t, p.Metrics)
		case SignalLog:
			require.Len(t, p.Logs, 1)
			require.Empty(t, p.Spans)
			require.Empty(t, p.Metrics)
		case SignalMetric:
			require.Len(t, p.Metrics, 1)
			require.Empty(t, p.Spans)
			require.Empty(t, p.Logs)
		}
	}
}

func TestObservability_ExportLedgerIdempotent(t *testing.T) {
	w, _, ledger, sender, now := workerFixture(t, 2)
	for range 3 {
		result, err := w.ExportOnce(t.Context())
		require.NoError(t, err)
		require.Zero(t, result.Unconfirmed)
		*now = now.Add(time.Minute)
	}
	require.Len(t, ledger.items, 2)
	require.Len(t, ledger.batches, 1)
	require.Len(t, sender.batches, 1)
}

func TestObservability_ExportRetryBudget(t *testing.T) {
	for _, mode := range []string{"attempts_before_send", "age_before_send", "last_allowed_attempt"} {
		t.Run(mode, func(t *testing.T) {
			w, scanner, ledger, sender, now := workerFixture(t, 1)
			for _, item := range scanner.facts {
				require.NoError(t, ledger.Enqueue(t.Context(), []ExportItem{item}))
			}
			w.cfg.MaxAttempts = 1
			for key, item := range ledger.items {
				switch mode {
				case "attempts_before_send":
					item.Attempts = 1
				case "age_before_send":
					item.CreatedAt = now.Add(-25 * time.Hour)
				}
				ledger.items[key] = item
			}
			sender.result = &SendResult{Retryable: true, ErrorKind: "timeout"}
			sender.err = context.DeadlineExceeded
			result, err := w.ExportOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, result.FailedPermanent)
			require.Zero(t, result.Retried)
			if mode == "last_allowed_attempt" {
				require.Len(t, sender.batches, 1)
			} else {
				require.Empty(t, sender.batches)
			}
			for _, item := range ledger.items {
				require.Equal(t, ExportFailedPermanent, item.State)
				require.Equal(t, "retry_exhausted", item.LastErrorKind)
			}
		})
	}
}

func TestObservability_ExportReadFailureNotPermanent(t *testing.T) {
	w, scanner, ledger, sender, _ := workerFixture(t, 1)
	scanner.reloadErr = errors.New("synthetic database read failure")
	_, err := w.ExportOnce(t.Context())
	require.Error(t, err)
	require.Empty(t, sender.batches)
	for _, item := range ledger.items {
		require.Equal(t, ExportInflight, item.State)
		require.Empty(t, item.LastErrorKind)
	}
}

func TestObservability_ExportMappingFailureDoesNotBlockGoodItem(t *testing.T) {
	w, scanner, ledger, sender, now := workerFixture(t, 1)
	bad := mapperItem(t, SourceRun, "broken", SignalTrace, RunFacts{RunID: "broken", Status: "completed", StartedAt: *now})
	addWorkerFact(t, scanner, bad)
	result, err := w.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, result.Sent)
	require.Equal(t, 1, result.FailedPermanent)
	require.Len(t, sender.batches, 1)
	require.Len(t, sender.batches[0].Items, 1)
	failed := 0
	for _, item := range ledger.items {
		if item.State == ExportFailedPermanent {
			failed++
			require.Equal(t, "mapping_error", item.LastErrorKind)
		}
	}
	require.Equal(t, 1, failed)
}
