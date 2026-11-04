package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlow_InstanceReserveUsesOriginalTotal(t *testing.T) {
	s := NewAgentService(ServiceOptions{MaxConcurrent: 2})
	flowReq := RunRequest{Principal: LocalAdminCLIPrincipal(), SessionID: "flow-one", Flow: &FlowRunContext{TaskID: "task-one"}}
	releaseFlow, err := s.admit(t.Context(), flowReq)
	require.NoError(t, err)
	flowReq.SessionID = "flow-two"
	_, err = s.admit(t.Context(), flowReq)
	var full *InstanceFullError
	require.ErrorAs(t, err, &full)
	releaseOrdinary, err := s.admit(t.Context(), RunRequest{SessionID: "ordinary"})
	require.NoError(t, err, "interactive reserve must remain in the same total table")
	require.Equal(t, 2, s.running)
	require.Equal(t, 1, s.flowRunning)
	releaseFlow()
	releaseFlow()
	require.Equal(t, 1, s.running)
	require.Zero(t, s.flowRunning)
	releaseAgain, err := s.admit(t.Context(), flowReq)
	require.NoError(t, err)
	require.Equal(t, 2, s.running)
	require.Equal(t, 1, s.flowRunning)
	releaseAgain()
	releaseOrdinary()
	require.Zero(t, s.running)
	require.Zero(t, s.flowRunning)
}

func TestFlow_InstanceSingleSlotNeedsExplicitZeroReserve(t *testing.T) {
	s := NewAgentService(ServiceOptions{MaxConcurrent: 1})
	request := RunRequest{Flow: &FlowRunContext{TaskID: "task"}}
	_, err := s.admit(t.Context(), request)
	require.Error(t, err)
	zero := 0
	s = NewAgentService(ServiceOptions{MaxConcurrent: 1, FlowInteractiveReservedRuns: &zero})
	release, err := s.admit(t.Context(), request)
	require.NoError(t, err)
	defer release()
	_, err = s.admit(t.Context(), RunRequest{})
	require.Error(t, err, "explicit zero does not bypass total capacity")
}

func TestFlow_UnwiredRunNeverFallsBackToOrdinaryExecution(t *testing.T) {
	s := NewAgentService(ServiceOptions{})
	_, err := s.Run(t.Context(), RunRequest{Principal: LocalAdminCLIPrincipal(), Flow: &FlowRunContext{TaskID: "untrusted"}})
	require.ErrorIs(t, err, ErrFlowDispatch)
	require.Zero(t, s.running)
}
