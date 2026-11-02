package observability

import (
	"encoding/json"
	"time"

	"github.com/ymedword/basalt/internal/historyretention"
)

// ReadWindow 是管理读取的有界时间窗，限额不影响后台评估或清理。
// ReadWindow bounds management reads without changing background evaluation or cleanup.
type ReadWindow struct {
	From, To time.Time
	Limit    int
}

// MaxReadWindow bounds administrator history reads; it does not change worker,
// metric, retention or export windows. The default remains one day.
const MaxReadWindow = 30 * 24 * time.Hour

// Validate allows at most 30 days and 1000 rows; callers supply explicit defaults.
func (q ReadWindow) Validate() error {
	if q.From.IsZero() || q.To.IsZero() || q.From.After(q.To) || q.To.Sub(q.From) > MaxReadWindow || q.Limit < 1 || q.Limit > 1000 {
		return ErrInvalidQuery
	}
	return nil
}

// AlertTransition 只给转换事实，不返回历史非结构化evidence_ref。
// AlertTransition returns transition facts without legacy unstructured evidence references.
type AlertTransition struct {
	ID         int64      `json:"id"`
	RuleID     string     `json:"rule_id"`
	ScopeKey   string     `json:"scope_key"`
	From       AlertState `json:"from_state"`
	To         AlertState `json:"to_state"`
	Value      float64    `json:"value"`
	OccurredAt time.Time  `json:"occurred_at"`
}

// AlertRuleStatus 公开固定规则元数据，不公开表达式或执行函数。
// AlertRuleStatus exposes fixed rule metadata, never expressions or callbacks.
type AlertRuleStatus struct {
	RuleID        string           `json:"rule_id"`
	Enabled       bool             `json:"enabled"`
	WindowSeconds int64            `json:"window_seconds"`
	ForSeconds    int64            `json:"for_seconds"`
	States        []AlertStateView `json:"states"`
}

// AlertStatus 中尚未评估的规则为unknown，不等价于正常。
// AlertStatus represents unevaluated rules as unknown, never healthy.
type AlertStatus struct {
	Enabled     bool              `json:"enabled"`
	Rules       []AlertRuleStatus `json:"rules"`
	Transitions []AlertTransition `json:"transitions"`
	Truncated   bool              `json:"truncated"`
}

// InitialAlertState 给出固定作用域；零时刻表示尚未记录，不伪造评估。
// InitialAlertState supplies the compiled scope without inventing an evaluation time.
func InitialAlertState(rule string, enabled bool) AlertStateView {
	reason := "not_evaluated"
	if !enabled {
		reason = "disabled"
	}
	return AlertStateView{RuleID: rule, ScopeKey: defaultAlertScope(rule), State: StateUnknown, Reason: reason}
}

// MarshalJSON 把未记录的评估/起始时间保留为null。
// MarshalJSON preserves unrecorded evaluation and start times as null.
func (s AlertStateView) MarshalJSON() ([]byte, error) {
	type plain AlertStateView
	return json.Marshal(struct {
		plain
		Since      *time.Time `json:"since"`
		LastEvalAt *time.Time `json:"last_eval_at"`
	}{plain(s), nullableTime(s.Since), nullableTime(s.LastEvalAt)})
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// SourceCoverage 的两个边界都存在才证明完整扫描覆盖，不承诺无限历史。
// SourceCoverage proves completed scanning only when both bounds exist.
type SourceCoverage struct {
	Source          SourceKind `json:"source"`
	CoverageKind    string     `json:"coverage_kind"`
	CoveredFrom     *time.Time `json:"covered_from"`
	CoveredThrough  *time.Time `json:"covered_through"`
	LastAttemptAt   *time.Time `json:"last_attempt_at"`
	LastSuccessAt   *time.Time `json:"last_success_at"`
	LastReconcileAt *time.Time `json:"last_reconcile_at"`
}

// RetentionPolicyView 只展示已生效的策略及单位，不提供写入口。
// RetentionPolicyView exposes effective policy values and units without mutations.
type RetentionPolicyView struct {
	Table                  string `json:"table"`
	Enabled                bool   `json:"enabled"`
	RetentionDays          int    `json:"retention_days"`
	BatchSize              int    `json:"batch_size"`
	MaxBatches             int    `json:"max_batches"`
	RequireConfirmedExport bool   `json:"require_confirmed_export"`
}

// CleanupStatus 只提供窗口账本；跳过且计数0可能表示尚未枚举候选。
// CleanupStatus provides window ledgers; skipped zero counts may mean no enumeration.
type CleanupStatus struct {
	Enabled        bool                      `json:"enabled"`
	HistoryEnabled bool                      `json:"history_enabled"`
	HistoryDays    int                       `json:"history_days"`
	History        []historyretention.Report `json:"history"`
	Files          FileRetentionView         `json:"files"`
	Policies       []RetentionPolicyView     `json:"policies"`
	Runs           []CleanupRun              `json:"runs"`
	Truncated      bool                      `json:"truncated"`
}

type FileRetentionView struct {
	Enabled            bool       `json:"enabled"`
	Days               int        `json:"days"`
	LogFileMiB         int        `json:"log_file_mib"`
	LogTotalMiB        int        `json:"log_total_mib"`
	MinVerifiedBackups int        `json:"min_verified_backups"`
	LastRunAt          *time.Time `json:"last_run_at"`
	DeletedFiles       int64      `json:"deleted_files"`
	Status             string     `json:"status"`
}
