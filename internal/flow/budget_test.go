package flow

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

func TestFlow_ChildBudgetNeverResets(t *testing.T) {
	state := BudgetState{Limit: validDefinition().Contract.Budget}
	first, err := ReserveAllocation(state, BudgetAllocation{ID: "one", TaskID: "child1", RunID: "run1", Reserved: core.UsageEstimate{Tokens: 700, ToolCalls: 6}})
	require.NoError(t, err)
	state.Allocations = append(state.Allocations, first)
	_, err = ReserveAllocation(state, BudgetAllocation{ID: "two", TaskID: "child2", RunID: "run2", Reserved: core.UsageEstimate{Tokens: 301, ToolCalls: 1}})
	require.ErrorIs(t, err, ErrBudget, "a child must not receive a fresh root budget")
	same, err := ReserveAllocation(state, first)
	require.NoError(t, err)
	require.Equal(t, first, same)
	first.Reserved.Tokens++
	_, err = ReserveAllocation(state, first)
	require.ErrorIs(t, err, ErrStateConflict, "same ticket must not change its reservation")
}

func TestFlow_UnknownUsageKeepsReservation(t *testing.T) {
	state := BudgetState{Limit: validDefinition().Contract.Budget}
	a, err := ReserveAllocation(state, BudgetAllocation{ID: "one", TaskID: "root", RunID: "run1", Reserved: core.UsageEstimate{Tokens: 1000, ToolCalls: 10}})
	require.NoError(t, err)
	a, err = SettleAllocation(a, core.UsageActual{Known: false})
	require.NoError(t, err)
	require.Equal(t, "unknown", a.State)
	state.Allocations = []BudgetAllocation{a}
	remaining, err := RemainingBudget(state)
	require.NoError(t, err)
	require.Zero(t, remaining.Tokens, "unknown tokens must retain the full reservation")
	require.Zero(t, remaining.ToolCalls)
	_, err = ReserveAllocation(state, BudgetAllocation{ID: "retry", TaskID: "child", RunID: "run2", Reserved: core.UsageEstimate{ToolCalls: 1}})
	require.ErrorIs(t, err, ErrBudget)
}

func TestFlow_BudgetTighteningAndKnownOverage(t *testing.T) {
	cap := validDefinition().Contract.Budget
	wider := cap
	wider.MaxTotalTokens++
	_, err := TightenBudget(cap, &wider)
	require.Error(t, err)
	tighter := cap
	tighter.MaxTotalTokens = 50
	require.Equal(t, int64(50), tighter.MaxTotalTokens)
	got, err := TightenBudget(cap, &tighter)
	require.NoError(t, err)
	require.Equal(t, tighter, got)
	a, err := SettleAllocation(BudgetAllocation{State: "pending", Reserved: core.UsageEstimate{Tokens: 40}}, core.UsageActual{Known: true, Tokens: 70})
	require.NoError(t, err)
	remaining, err := RemainingBudget(BudgetState{Limit: tighter, Allocations: []BudgetAllocation{a}})
	require.NoError(t, err)
	require.Zero(t, remaining.Tokens, "actual overage must prevent new allocations")
	_, err = ReserveAllocation(BudgetState{Limit: tighter, Allocations: []BudgetAllocation{a}}, BudgetAllocation{ID: "new-tool", TaskID: "task", RunID: "run", Reserved: core.UsageEstimate{ToolCalls: 1}})
	require.ErrorIs(t, err, ErrBudget, "known overage must block allocations in every dimension")
}
