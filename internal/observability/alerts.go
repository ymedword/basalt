package observability

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// AlertState 的resolved只用于转换历史，不作为当前状态。
// AlertState uses resolved only in history, never as a current state.
type AlertState string

// 五个冻结字面量分别用于状态与转换。
// Five fixed literals serve states and transitions.
const (
	StateInactive AlertState = "inactive"
	StatePending  AlertState = "pending"
	StateFiring   AlertState = "firing"
	StateResolved AlertState = "resolved"
	StateUnknown  AlertState = "unknown"
)

// AlertSample 的正值表示规则条件成立，0表示成功评估后条件不成立。
// AlertSample uses a positive value for a matched condition and zero for a proven non-match.
type AlertSample struct {
	ScopeKey string
	Value    float64
}

// AlertStateView 保留只读展示，恢复元数据不进入管理API。
// AlertStateView keeps recovery metadata out of the management API.
type AlertStateView struct {
	RuleID        string     `json:"rule_id"`
	ScopeKey      string     `json:"scope_key"`
	State         AlertState `json:"state"`
	Since         time.Time  `json:"since"`
	LastEvalAt    time.Time  `json:"last_eval_at"`
	Value         float64    `json:"value"`
	Reason        string     `json:"reason"`
	PreviousState AlertState `json:"-"`
	PreviousSince time.Time  `json:"-"`
	RecoverySince time.Time  `json:"-"`
}

// AlertRule 是代码内置规则，不接受表达式或动态DSL。
// AlertRule is a compiled rule rather than a user expression or dynamic DSL.
type AlertRule struct {
	ID      string
	Enabled bool
	Window  time.Duration
	For     time.Duration
	Collect func(context.Context, time.Time) ([]AlertSample, error)
}

// AlertStore 保存派生告警状态，不拥有业务写权限。
// AlertStore persists derived alert state without business mutation rights.
type AlertStore interface {
	LoadStates(context.Context) (map[string]AlertStateView, error)
	SaveState(context.Context, AlertStateView) error
	SaveTransition(context.Context, AlertStateView, AlertStateView, time.Time) error
}

type alertAtomicStore interface {
	ApplyAlertEvaluation(context.Context, AlertStateView, AlertStateView, time.Time) error
}

// AlertEngine 串行评估本地规则，数据库仍是重启后的状态来源。
// AlertEngine serializes local evaluations while the database owns restart state.
type AlertEngine struct {
	store AlertStore
	rules []AlertRule
	clock func() time.Time
	mu    sync.Mutex
}

// NewAlertEngine 显式注入规则和时钟，不启动线程。
// NewAlertEngine injects rules and clock without spawning work.
func NewAlertEngine(store AlertStore, rules []AlertRule, clock func() time.Time) *AlertEngine {
	if clock == nil {
		clock = time.Now
	}
	return &AlertEngine{store: store, rules: rules, clock: clock}
}

// Evaluate 将读取或持久化失败显示为unknown，绝不伪造恢复。
// Evaluate reports failed reads or persistence as unknown rather than inventing recovery.
func (e *AlertEngine) Evaluate(ctx context.Context, now time.Time) ([]AlertStateView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.IsZero() {
		now = e.clock()
	}
	now = now.UTC()
	var previous map[string]AlertStateView
	var loadErr error
	if e.store == nil {
		loadErr = errors.New("observability alert store unavailable")
	} else {
		previous, loadErr = e.store.LoadStates(ctx)
	}
	out := make([]AlertStateView, 0, len(e.rules))
	var resultErr error
	for _, rule := range e.rules {
		if !rule.Enabled {
			out = append(out, AlertStateView{RuleID: rule.ID, ScopeKey: defaultAlertScope(rule.ID), State: StateUnknown, Since: now, LastEvalAt: now, Reason: "disabled"})
			continue
		}
		if loadErr != nil {
			out = append(out, unknownAlert(AlertStateView{RuleID: rule.ID, ScopeKey: defaultAlertScope(rule.ID)}, now, "state_store_unavailable"))
			continue
		}
		var samples []AlertSample
		var collectErr error
		if rule.Collect == nil || rule.For < 0 || !slices.Contains(metricRules, rule.ID) {
			collectErr = errors.New("invalid compiled alert rule")
		} else {
			samples, collectErr = rule.Collect(ctx, now)
		}
		byScope := make(map[string]AlertSample)
		for _, sample := range samples {
			if sample.ScopeKey == "" {
				sample.ScopeKey = defaultAlertScope(rule.ID)
			}
			if math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) || sample.Value < 0 || len(sample.ScopeKey) > 256 {
				collectErr = errors.New("invalid alert sample")
				break
			}
			byScope[sample.ScopeKey] = sample
		}
		if len(byScope) > 1000 {
			collectErr = errors.New("alert source exceeded bounded scopes")
		}
		// 成功的完整读数中消失的旧scope以0复核；来源失败时这些scope一律unknown。
		// Missing scopes from a complete successful read evaluate as zero; failed reads stay unknown.
		for key, state := range previous {
			if strings.HasPrefix(key, rule.ID+"/") {
				if _, ok := byScope[state.ScopeKey]; !ok {
					byScope[state.ScopeKey] = AlertSample{ScopeKey: state.ScopeKey}
				}
			}
		}
		if len(byScope) == 0 {
			scope := defaultAlertScope(rule.ID)
			byScope[scope] = AlertSample{ScopeKey: scope}
		}
		for scope, sample := range byScope {
			from := previous[rule.ID+"/"+scope]
			from.RuleID, from.ScopeKey = rule.ID, scope
			if from.State == "" {
				from.State = StateInactive
				from.Since = now
			}
			var next AlertStateView
			if collectErr != nil {
				reason := "source_unavailable"
				if errors.Is(collectErr, errProcessRestartGap) {
					reason = "process_restart_gap"
				}
				next = unknownAlert(from, now, reason)
			} else {
				next = nextAlert(rule, from, sample.Value, now)
			}
			var saveErr error
			if atomic, ok := e.store.(alertAtomicStore); ok {
				saveErr = atomic.ApplyAlertEvaluation(ctx, from, next, now)
			} else {
				if alertChanged(from, next) {
					transition := next
					if alertResolved(from, next) {
						transition.State = StateResolved
					}
					saveErr = e.store.SaveTransition(ctx, from, transition, now)
				}
				if saveErr == nil {
					saveErr = e.store.SaveState(ctx, next)
				}
			}
			if saveErr != nil {
				resultErr = errors.Join(resultErr, saveErr)
				next = unknownAlert(from, now, "state_store_unavailable")
			}
			out = append(out, next)
		}
	}
	slices.SortFunc(out, func(a, b AlertStateView) int {
		if a.RuleID != b.RuleID {
			return strings.Compare(a.RuleID, b.RuleID)
		}
		return strings.Compare(a.ScopeKey, b.ScopeKey)
	})
	return out, errors.Join(loadErr, resultErr)
}

func defaultAlertScope(rule string) string {
	switch rule {
	case "audit_write_failed", "diagnostic_write_failed":
		return "process"
	case "export_permanent_failure", "export_backlog_stuck":
		return "export"
	case "cleanup_failed":
		return "cleanup"
	default:
		return "global"
	}
}

func unknownAlert(from AlertStateView, now time.Time, reason string) AlertStateView {
	next := from
	if from.State != StateUnknown {
		next.PreviousState, next.PreviousSince = from.State, from.Since
		next.Since = now
	}
	next.State, next.LastEvalAt, next.Value, next.Reason = StateUnknown, now, 0, reason
	// 数据缺口中不能证明恢复条件持续成立。
	// An evidence gap cannot prove a sustained recovery condition.
	next.RecoverySince = time.Time{}
	return next
}

func nextAlert(rule AlertRule, from AlertStateView, value float64, now time.Time) AlertStateView {
	next := from
	next.LastEvalAt, next.Value, next.Reason = now, value, ""
	if from.State == StateUnknown {
		if from.PreviousState == StateFiring {
			next.State, next.Since = StateFiring, from.PreviousSince
		} else {
			next.State, next.Since = StateInactive, now
		}
		next.RecoverySince = time.Time{}
	}
	next.PreviousState, next.PreviousSince = "", time.Time{}
	if value > 0 {
		if next.State == StateFiring {
			next.RecoverySince = time.Time{}
			return next
		}
		if next.State != StatePending {
			next.State, next.Since = StatePending, now
		}
		if rule.For == 0 || now.Sub(next.Since) >= rule.For {
			next.State, next.Since = StateFiring, now
		}
		return next
	}
	if next.State == StateFiring {
		if next.RecoverySince.IsZero() {
			next.RecoverySince = now
		}
		if rule.For > 0 && now.Sub(next.RecoverySince) < rule.For {
			next.Reason = "recovery_pending"
			return next
		}
	}
	if next.State != StateInactive {
		next.Since = now
	}
	next.State, next.RecoverySince = StateInactive, time.Time{}
	return next
}

func alertChanged(from, to AlertStateView) bool {
	return from.State != to.State || from.Reason != to.Reason || !from.RecoverySince.Equal(to.RecoverySince)
}

func alertResolved(from, to AlertStateView) bool {
	return to.State == StateInactive && (from.State == StateFiring || from.State == StateUnknown && from.PreviousState == StateFiring)
}
