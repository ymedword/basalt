package flow

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlow_CancelIntentNeverRequeues(t *testing.T) {
	task := Task{Status: Blocked, CloseIntent: CloseCancel, CancelRequested: true}
	status, intent, cancelled, err := CloseState(task, CloseDeadline, true, false)
	require.NoError(t, err)
	require.Equal(t, Cancelled, status, "deadline must not overwrite a joined user cancellation")
	require.Equal(t, CloseCancel, intent)
	require.True(t, cancelled)
	status, intent, _, err = CloseState(task, CloseAbandon, true, true)
	require.NoError(t, err)
	require.Equal(t, Cancelled, status, "late administrator must preserve joined cancellation")
	require.Equal(t, CloseCancel, intent)
	require.False(t, CanTransition(Cancelling, Stopped), "cancelling must never transition to stopped")
	for _, terminal := range []TaskStatus{Succeeded, Failed, Stopped, Cancelled, Abandoned} {
		require.True(t, terminal.Terminal())
		require.False(t, CanTransition(terminal, Queued), "terminal task must never requeue")
	}
}

func TestFlow_CloseWaitsForJoinAndPreservesIntent(t *testing.T) {
	task := Task{Status: Running, CloseIntent: CloseNone}
	status, intent, bit, err := CloseState(task, CloseAbandon, false, false)
	require.NoError(t, err)
	require.Equal(t, Blocked, status)
	require.Equal(t, CloseAbandon, intent)
	require.True(t, bit)
	task.Status, task.CloseIntent, task.CancelRequested = status, intent, bit
	status, intent, bit, err = CloseState(task, CloseDeadline, true, false)
	require.NoError(t, err)
	require.Equal(t, Abandoned, status)
	require.Equal(t, CloseAbandon, intent)
	require.True(t, bit)
}

func TestFlow_CloseIntentCombinationDomain(t *testing.T) {
	for _, intent := range []CloseIntent{CloseNone, CloseCancel, CloseAbandon, CloseDeadline} {
		for _, bit := range []bool{false, true} {
			expected := bit == (intent == CloseCancel || intent == CloseAbandon)
			require.Equal(t, expected, ValidCloseIntent(Blocked, intent, bit))
			require.Equal(t, expected && bit, ValidCloseIntent(Cancelling, intent, bit))
		}
	}
	require.False(t, ValidCloseIntent(Blocked, "unknown", false))
	require.NotEqual(t, (TaskVersion{Revision: 1, StateVersion: 1, ExecutionEpoch: 2}).ETag("a"), (TaskVersion{Revision: 1, StateVersion: 2, ExecutionEpoch: 2}).ETag("a"))
}

func TestFlow_BlockedCloseWaitsWithoutIllegalTransition(t *testing.T) {
	for _, previous := range []CloseIntent{CloseNone, CloseCancel} {
		task := Task{Status: Blocked, CloseIntent: previous, CancelRequested: previous == CloseCancel}
		status, intent, bit, err := CloseState(task, CloseCancel, false, false)
		require.NoError(t, err)
		require.Equal(t, Blocked, status, "blocked cancellation must wait in a legal state until join")
		require.Equal(t, CloseCancel, intent)
		require.True(t, bit)
	}
}
