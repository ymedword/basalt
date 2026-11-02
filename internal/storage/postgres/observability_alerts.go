package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

var _ observability.AlertStore = (*ObservabilityStore)(nil)

type obsAlertMeta struct {
	Reason        string                   `json:"reason"`
	Previous      observability.AlertState `json:"previous_state,omitempty"`
	PreviousSince *int64                   `json:"previous_since,omitempty"`
	RecoverySince *int64                   `json:"recovery_since,omitempty"`
}

// LoadStates 从当前行及受保护的最近转换恢复unknown前状态和恢复计时。
// LoadStates restores pre-unknown state and recovery timing from protected transition evidence.
func (s *ObservabilityStore) LoadStates(ctx context.Context) (map[string]observability.AlertStateView, error) {
	states, err := obsRows(ctx, s.db, `SELECT s.rule_id,s.scope_key,s.state,s.since,s.last_eval_at,s.last_value,
		COALESCE(t.evidence_ref,''),COALESCE(p.to_state,''),p.occurred_at
		FROM observability_alert_states s
		LEFT JOIN LATERAL(SELECT evidence_ref FROM observability_alert_transitions WHERE rule_id=s.rule_id AND scope_key=s.scope_key ORDER BY id DESC LIMIT 1)t ON true
		LEFT JOIN LATERAL(SELECT to_state,occurred_at FROM observability_alert_transitions WHERE rule_id=s.rule_id AND scope_key=s.scope_key AND to_state<>'unknown' ORDER BY id DESC LIMIT 1)p ON true
		ORDER BY s.rule_id,s.scope_key LIMIT 10001`, nil, func(rows *sql.Rows) (observability.AlertStateView, error) {
		var out observability.AlertStateView
		var since, eval int64
		var ref, prior string
		var priorAt sql.NullInt64
		if err := rows.Scan(&out.RuleID, &out.ScopeKey, &out.State, &since, &eval, &out.Value, &ref, &prior, &priorAt); err != nil {
			return out, err
		}
		out.Since, out.LastEvalAt = time.UnixMilli(since).UTC(), time.UnixMilli(eval).UTC()
		if strings.HasPrefix(ref, "obs-meta:") {
			var meta obsAlertMeta
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ref, "obs-meta:")), &meta); err != nil {
				return out, errors.New("invalid alert recovery metadata")
			}
			if !slices.Contains([]string{"", "source_unavailable", "process_restart_gap", "state_store_unavailable", "recovery_pending"}, meta.Reason) {
				return out, errors.New("unknown alert recovery reason")
			}
			out.Reason, out.PreviousState = meta.Reason, meta.Previous
			if out.State == observability.StateUnknown && !slices.Contains([]observability.AlertState{observability.StateInactive, observability.StatePending, observability.StateFiring}, meta.Previous) {
				return out, errors.New("alert previous state unavailable")
			}
			if meta.PreviousSince != nil {
				out.PreviousSince = time.UnixMilli(*meta.PreviousSince).UTC()
			}
			if meta.RecoverySince != nil {
				out.RecoverySince = time.UnixMilli(*meta.RecoverySince).UTC()
			}
		} else if out.State == observability.StateUnknown {
			// 历史非结构化引用只从真实转换恢复，缺证据不能假定原来正常。
			// Legacy references recover only from recorded transitions, never an assumed inactive state.
			if prior == "" || !priorAt.Valid {
				return out, errors.New("alert recovery evidence unavailable")
			}
			out.PreviousState = observability.AlertState(prior)
			if out.PreviousState == observability.StateResolved {
				out.PreviousState = observability.StateInactive
			}
			out.PreviousSince = time.UnixMilli(priorAt.Int64).UTC()
			out.Reason = "source_unavailable"
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	if len(states) > 10000 {
		return nil, errors.New("alert state prefix truncated")
	}
	out := make(map[string]observability.AlertStateView, len(states))
	for _, state := range states {
		out[state.RuleID+"/"+state.ScopeKey] = state
	}
	return out, nil
}

const obsAlertUpsert = `INSERT INTO observability_alert_states(rule_id,scope_key,state,since,last_eval_at,last_value,updated_at)
	VALUES ($1,$2,$3,$4,$5,$6,$5) ON CONFLICT(rule_id,scope_key) DO UPDATE SET state=excluded.state,since=excluded.since,last_eval_at=excluded.last_eval_at,last_value=excluded.last_value,updated_at=excluded.updated_at`

// SaveState 是端口兼容的单状态写入；运行引擎优先使用原子评估方法。
// SaveState preserves the port while the runtime engine uses atomic evaluations.
func (s *ObservabilityStore) SaveState(ctx context.Context, state observability.AlertStateView) error {
	_, err := s.db.pool.ExecContext(ctx, obsAlertUpsert, state.RuleID, state.ScopeKey, state.State, state.Since.UnixMilli(), state.LastEvalAt.UnixMilli(), state.Value)
	return err
}

// UpsertAlertState 保持模块卡列出的存储入口。
// UpsertAlertState is the storage entry named by the module card.
func (s *ObservabilityStore) UpsertAlertState(ctx context.Context, state observability.AlertStateView) error {
	return s.SaveState(ctx, state)
}

func obsAlertRef(state observability.AlertStateView) (string, error) {
	meta := obsAlertMeta{Reason: state.Reason, Previous: state.PreviousState, PreviousSince: obsAlertTime(state.PreviousSince), RecoverySince: obsAlertTime(state.RecoverySince)}
	raw, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return "obs-meta:" + string(raw), nil
}

func obsAlertTime(at time.Time) *int64 {
	if at.IsZero() {
		return nil
	}
	millis := at.UnixMilli()
	return &millis
}

// SaveTransition 的evidence_ref只保存固定原因和恢复时刻，不保存错误原文。
// SaveTransition stores fixed recovery metadata, never raw errors, in evidence_ref.
func (s *ObservabilityStore) SaveTransition(ctx context.Context, from, to observability.AlertStateView, at time.Time) error {
	ref, err := obsAlertRef(to)
	if err != nil {
		return err
	}
	_, err = s.db.pool.ExecContext(ctx, `INSERT INTO observability_alert_transitions(rule_id,scope_key,from_state,to_state,value,occurred_at,evidence_ref) VALUES ($1,$2,$3,$4,$5,$6,$7)`, to.RuleID, to.ScopeKey, from.State, to.State, to.Value, at.UnixMilli(), ref)
	return err
}

// InsertAlertTransition 保持已审存储入口，不引入第二张历史表。
// InsertAlertTransition preserves the approved entry without a second history table.
func (s *ObservabilityStore) InsertAlertTransition(ctx context.Context, from, to observability.AlertStateView, at time.Time) error {
	return s.SaveTransition(ctx, from, to, at)
}

// ApplyAlertEvaluation 把状态与转换放在同一事务，拒绝基于过期评估覆盖新状态。
// ApplyAlertEvaluation atomically saves state and transition and rejects stale evaluations.
func (s *ObservabilityStore) ApplyAlertEvaluation(ctx context.Context, from, to observability.AlertStateView, at time.Time) error {
	if to.LastEvalAt.Before(from.LastEvalAt) {
		return core.ErrConflict
	}
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var state string
		var last int64
		var value float64
		err := tx.QueryRowContext(ctx, `SELECT state,last_eval_at,last_value FROM observability_alert_states WHERE rule_id=$1 AND scope_key=$2 FOR UPDATE`, to.RuleID, to.ScopeKey).Scan(&state, &last, &value)
		if errors.Is(err, sql.ErrNoRows) {
			result, insertErr := tx.ExecContext(ctx, `INSERT INTO observability_alert_states(rule_id,scope_key,state,since,last_eval_at,last_value,updated_at) VALUES ($1,$2,$3,$4,$5,$6,$5) ON CONFLICT DO NOTHING`, to.RuleID, to.ScopeKey, to.State, to.Since.UnixMilli(), to.LastEvalAt.UnixMilli(), to.Value)
			if insertErr != nil {
				return insertErr
			}
			count, countErr := result.RowsAffected()
			if countErr != nil {
				return countErr
			}
			if count != 1 {
				return core.ErrConflict
			}
		} else {
			if err != nil {
				return err
			}
			if state != string(from.State) || last != obsMillis(from.LastEvalAt) || value != from.Value {
				return core.ErrConflict
			}
			if _, err := tx.ExecContext(ctx, obsAlertUpsert, to.RuleID, to.ScopeKey, to.State, to.Since.UnixMilli(), to.LastEvalAt.UnixMilli(), to.Value); err != nil {
				return err
			}
		}
		if from.State != to.State || from.Reason != to.Reason || !from.RecoverySince.Equal(to.RecoverySince) {
			target := to.State
			if target == observability.StateInactive && (from.State == observability.StateFiring || from.State == observability.StateUnknown && from.PreviousState == observability.StateFiring) {
				target = observability.StateResolved
			}
			ref, err := obsAlertRef(to)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO observability_alert_transitions(rule_id,scope_key,from_state,to_state,value,occurred_at,evidence_ref) VALUES ($1,$2,$3,$4,$5,$6,$7)`, to.RuleID, to.ScopeKey, from.State, target, to.Value, at.UnixMilli(), ref)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// AlertEvents 仅读取固定时间窗的分类，不读取身份、attrs或错误原文。
// AlertEvents reads only bounded classifications, without identities, attrs, or raw errors.
func (s *ObservabilityStore) AlertEvents(ctx context.Context, from, to time.Time) ([]diagnostics.DiagnosticEvent, error) {
	if from.IsZero() || to.IsZero() || from.After(to) || to.Sub(from) > 24*time.Hour {
		return nil, observability.ErrInvalidQuery
	}
	return obsRows(ctx, s.db, `SELECT kind,reason_code,occurred_at FROM diagnostic_events WHERE occurred_at BETWEEN $1 AND $2
		AND kind IN ('permission_decision','retrieval') AND reason_code='permission_denied' ORDER BY occurred_at,event_id LIMIT 10001`, []any{from.UnixMilli(), to.UnixMilli()}, func(rows *sql.Rows) (diagnostics.DiagnosticEvent, error) {
		var event diagnostics.DiagnosticEvent
		err := rows.Scan(&event.Kind, &event.ReasonCode, &event.OccurredAt)
		return event, err
	})
}

// AlertSamples 使用固定查询与阈值；scope只用于管理下钻，不成为指标标签。
// AlertSamples uses compiled queries and thresholds; scopes never become metric labels.
func (s *ObservabilityStore) AlertSamples(ctx context.Context, rule string, window time.Duration, now time.Time) ([]observability.AlertSample, error) {
	if now.IsZero() || window <= 0 || window > 24*time.Hour {
		return nil, observability.ErrInvalidQuery
	}
	if rule == "run_failure_burst" {
		samples, err := obsRows(ctx, s.db, `SELECT agent,count(*) FILTER(WHERE status='failed') FROM runs WHERE started_at BETWEEN $1 AND $2 GROUP BY agent
			HAVING count(*)>=5 AND count(*) FILTER(WHERE status='failed')>=5 AND (count(*) FILTER(WHERE status='failed'))::double precision/count(*)>=0.5 ORDER BY agent LIMIT 1001`, []any{now.Add(-window).UnixMilli(), now.UnixMilli()}, func(rows *sql.Rows) (observability.AlertSample, error) {
			var sample observability.AlertSample
			err := rows.Scan(&sample.ScopeKey, &sample.Value)
			return sample, err
		})
		if len(samples) > 1000 {
			return nil, errors.New("alert agent groups truncated")
		}
		return samples, err
	}
	var value float64
	scope := "global"
	switch rule {
	case "export_permanent_failure":
		scope = "export"
		if err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FROM observability_export_items WHERE state='failed_permanent' AND updated_at BETWEEN $1 AND $2`, now.Add(-window).UnixMilli(), now.UnixMilli()).Scan(&value); err != nil {
			return nil, err
		}
	case "export_backlog_stuck":
		scope = "export"
		if err := s.db.pool.QueryRowContext(ctx, `SELECT GREATEST(COALESCE($1-min(created_at),0),0) FROM observability_export_items WHERE state IN ('pending','inflight','retry')`, now.UnixMilli()).Scan(&value); err != nil {
			return nil, err
		}
		if value <= float64((30 * time.Minute).Milliseconds()) {
			value = 0
		}
	case "cleanup_failed":
		scope = "cleanup"
		if err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT DISTINCT ON(table_kind) status FROM observability_cleanup_runs WHERE status IN ('failed','succeeded') ORDER BY table_kind,started_at DESC,cleanup_id DESC)latest WHERE status='failed'`).Scan(&value); err != nil {
			return nil, err
		}
		var historyFailures float64
		if err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT DISTINCT ON(group_kind) status FROM history_retention_runs ORDER BY group_kind,occurred_at DESC,id DESC)latest WHERE status='failed'`).Scan(&historyFailures); err != nil {
			return nil, err
		}
		value += historyFailures
	case "llm_timeout_burst":
		if err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FROM llm_calls WHERE status='failed' AND created_at BETWEEN $1 AND $2`, now.Add(-window).UnixMilli(), now.UnixMilli()).Scan(&value); err != nil {
			return nil, err
		}
		if value < 5 {
			value = 0
		}
	case "tool_denial_burst":
		if err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FROM diagnostic_events WHERE kind='permission_decision' AND reason_code='permission_denied' AND occurred_at BETWEEN $1 AND $2`, now.Add(-window).UnixMilli(), now.UnixMilli()).Scan(&value); err != nil {
			return nil, err
		}
		if value < 20 {
			value = 0
		}
	default:
		return nil, fmt.Errorf("%w: compiled alert rule", observability.ErrInvalidQuery)
	}
	return []observability.AlertSample{{ScopeKey: scope, Value: value}}, nil
}
