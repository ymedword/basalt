package flow

import (
	"errors"

	"github.com/ymedword/basalt/internal/core"
)

// BudgetAllocation 是根／祖先账本中的不可免计分配，不因子任务关闭而删除。
// BudgetAllocation is a retained ancestor allocation, independent of child closure.
type BudgetAllocation struct {
	ID, TaskID, RunID, ActionID string
	Reserved                    core.UsageEstimate
	Actual                      core.UsageActual
	State                       string
}

// BudgetState 是事务内计算用的账本事实，不是任务摘要中的第二份余额。
// BudgetState is transaction-local ledger input, never another summary balance.
type BudgetState struct {
	Limit       BudgetCap
	Allocations []BudgetAllocation
}

// TightenBudget 仅接受完整且不抬高任一维度的调用方上限。
// TightenBudget accepts a complete caller cap only when every dimension narrows.
func TightenBudget(author BudgetCap, requested *BudgetCap) (BudgetCap, error) {
	if err := validateBudget(author); err != nil {
		return BudgetCap{}, err
	}
	if requested == nil {
		return author, nil
	}
	if err := validateBudget(*requested); err != nil {
		return BudgetCap{}, err
	}
	if requested.MaxTotalTokens > author.MaxTotalTokens || requested.MaxToolCalls > author.MaxToolCalls || requested.MaxDurationSeconds > author.MaxDurationSeconds || requested.MaxDepth > author.MaxDepth || requested.MaxChildren > author.MaxChildren || requested.MaxAttemptsPerNode > author.MaxAttemptsPerNode || requested.MaxRevisions > author.MaxRevisions {
		return BudgetCap{}, errors.New("flow caller budget cannot exceed author ceiling")
	}
	return *requested, nil
}

// ReserveAllocation 校验同票幂等与剩余祖先额度；调用者须在同一根锁事务内落盘。
// ReserveAllocation checks ticket idempotency and remaining capacity under one root lock.
func ReserveAllocation(state BudgetState, proposed BudgetAllocation) (BudgetAllocation, error) {
	if proposed.ID == "" || proposed.TaskID == "" || proposed.RunID == "" || proposed.Reserved.Tokens < 0 || proposed.Reserved.ToolCalls < 0 || proposed.Reserved.Tokens == 0 && proposed.Reserved.ToolCalls == 0 {
		return BudgetAllocation{}, errors.New("flow budget allocation invalid")
	}
	for _, allocation := range state.Allocations {
		if allocation.ID == proposed.ID {
			if allocation.TaskID != proposed.TaskID || allocation.RunID != proposed.RunID || allocation.ActionID != proposed.ActionID || allocation.Reserved != proposed.Reserved {
				return BudgetAllocation{}, ErrStateConflict
			}
			return allocation, nil
		}
	}
	remaining, overage, err := budgetBalance(state)
	if err != nil {
		return BudgetAllocation{}, err
	}
	if overage || proposed.Reserved.Tokens > remaining.Tokens || proposed.Reserved.ToolCalls > remaining.ToolCalls {
		return BudgetAllocation{}, ErrBudget
	}
	proposed.State = "pending"
	return proposed, nil
}

// RemainingBudget 对未知／pending 使用完整预留，对已知超量仍保留实际占用。
// RemainingBudget retains pending reservations and accounts for known overage.
func RemainingBudget(state BudgetState) (core.UsageEstimate, error) {
	remaining, _, err := budgetBalance(state)
	return remaining, err
}

func budgetBalance(state BudgetState) (core.UsageEstimate, bool, error) {
	if err := validateBudget(state.Limit); err != nil {
		return core.UsageEstimate{}, false, err
	}
	overage := false
	remaining := core.UsageEstimate{Tokens: state.Limit.MaxTotalTokens, ToolCalls: state.Limit.MaxToolCalls}
	for _, allocation := range state.Allocations {
		used := allocation.Reserved
		switch allocation.State {
		case "pending", "unknown":
		case "settled":
			if !allocation.Actual.Known {
				return core.UsageEstimate{}, false, errors.New("flow settled allocation missing actual usage")
			}
			used = core.UsageEstimate{Tokens: allocation.Actual.Tokens, ToolCalls: allocation.Actual.ToolCalls}
		default:
			return core.UsageEstimate{}, false, errors.New("flow budget state invalid")
		}
		if used.Tokens < 0 || used.ToolCalls < 0 {
			return core.UsageEstimate{}, false, errors.New("flow usage cannot be negative")
		}
		overage = overage || used.Tokens > remaining.Tokens || used.ToolCalls > remaining.ToolCalls
		// 饱和扣减可表达超额阻断且不会因恶意大整数发生累加溢出。
		// Saturating subtraction blocks overage without sum overflow.
		remaining.Tokens = subtractUsage(remaining.Tokens, used.Tokens)
		remaining.ToolCalls = subtractUsage(remaining.ToolCalls, used.ToolCalls)
	}
	return remaining, overage, nil
}

func subtractUsage(left, used int64) int64 {
	if used > left {
		return 0
	}
	return left - used
}

// SettleAllocation 只记录可证明的实际值；unknown 不由重试或关闭自动释放。
// SettleAllocation records proven actuals and never frees unknown usage through closure.
func SettleAllocation(allocation BudgetAllocation, actual core.UsageActual) (BudgetAllocation, error) {
	if actual.Tokens < 0 || actual.ToolCalls < 0 {
		return allocation, errors.New("flow actual usage invalid")
	}
	if allocation.State == "settled" {
		if allocation.Actual != actual {
			return allocation, ErrStateConflict
		}
		return allocation, nil
	}
	if allocation.State != "pending" && allocation.State != "unknown" {
		return allocation, ErrStateConflict
	}
	if !actual.Known {
		allocation.State = "unknown"
		return allocation, nil
	}
	allocation.Actual, allocation.State = actual, "settled"
	return allocation, nil
}
