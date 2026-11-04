package flow

import (
	"errors"
	"time"
)

// Config 是部署有界值；只在 serve 显式启用，不包含密钥。
// Config defines bounded deployment controls and contains no credentials.
type Config struct {
	Enabled                      bool  `json:"enabled"`
	MaxActive                    int   `json:"max_active"`
	InteractiveReservedRuns      *int  `json:"interactive_reserved_runs"`
	InteractiveReservedPerCaller *int  `json:"interactive_reserved_per_caller"`
	MaxOpenTasks                 int   `json:"max_open_tasks"`
	MaxStoredTasks               int   `json:"max_stored_tasks"`
	PollIntervalMS               int   `json:"poll_interval_ms"`
	MaxDurationSeconds           int64 `json:"max_duration_seconds"`
	MaxNodes                     int   `json:"max_nodes"`
	MaxDepth                     int   `json:"max_depth"`
	MaxChildren                  int   `json:"max_children"`
	MaxAttemptsPerNode           int   `json:"max_attempts_per_node"`
	MaxRevisions                 int   `json:"max_revisions"`
	MaxTotalTokens               int64 `json:"max_total_tokens"`
	MaxToolCalls                 int64 `json:"max_tool_calls"`
	CancelWaitSeconds            int   `json:"cancel_wait_seconds"`
	ShutdownGraceSeconds         int   `json:"shutdown_grace_seconds"`
}

// DefaultConfig 返回有界缺省，不启动 Worker。
// DefaultConfig returns bounded defaults without starting a Worker.
func DefaultConfig() Config {
	one, another := 1, 1
	return Config{MaxActive: 4, InteractiveReservedRuns: &one, InteractiveReservedPerCaller: &another, MaxOpenTasks: 32, MaxStoredTasks: 10000, PollIntervalMS: 250, MaxDurationSeconds: 1800, MaxNodes: 64, MaxDepth: 3, MaxChildren: 16, MaxAttemptsPerNode: 3, MaxRevisions: 2, MaxTotalTokens: 200000, MaxToolCalls: 200, CancelWaitSeconds: 30, ShutdownGraceSeconds: 35}
}

// Validate 拒绝无界或吞掉交互预留的启用配置，不自动降级为不限量。
// Validate rejects unbounded controls and unusable enabled reserves.
func (c Config) Validate(instanceRuns int, httpGrace time.Duration) error {
	if c.MaxActive < 1 || c.MaxActive > 8 || c.MaxOpenTasks < 1 || c.MaxOpenTasks > 32 || c.MaxStoredTasks < c.MaxOpenTasks || c.MaxStoredTasks > 10000 || c.PollIntervalMS < 100 || c.PollIntervalMS > 5000 || c.MaxNodes < 1 || c.MaxNodes > 64 || c.CancelWaitSeconds < 1 || c.CancelWaitSeconds > 30 || c.ShutdownGraceSeconds < c.CancelWaitSeconds+5 || c.ShutdownGraceSeconds > 70 || time.Duration(c.ShutdownGraceSeconds)*time.Second > httpGrace {
		return errors.New("flow deployment bounds invalid")
	}
	if c.InteractiveReservedRuns == nil || c.InteractiveReservedPerCaller == nil || *c.InteractiveReservedRuns < 0 || *c.InteractiveReservedPerCaller < 0 {
		return errors.New("flow interactive reserves must be explicit bounded values")
	}
	if c.Enabled && instanceRuns <= *c.InteractiveReservedRuns {
		return errors.New("flow interactive reserve leaves no instance run slot")
	}
	return validateBudget(c.Budget())
}

// Budget 是部署预算上限，不是每个子 Run 的独立额度。
// Budget returns deployment ceilings, never fresh per-child balances.
func (c Config) Budget() BudgetCap {
	return BudgetCap{MaxTotalTokens: c.MaxTotalTokens, MaxToolCalls: c.MaxToolCalls, MaxDurationSeconds: c.MaxDurationSeconds, MaxDepth: c.MaxDepth, MaxChildren: c.MaxChildren, MaxAttemptsPerNode: c.MaxAttemptsPerNode, MaxRevisions: c.MaxRevisions}
}
