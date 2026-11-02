package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionMaintenanceExcludesAdmission(t *testing.T) {
	s := NewAgentService(ServiceOptions{MaxConcurrent: 1})
	release, ok := s.TrySessionMaintenance("session")
	require.True(t, ok)
	_, err := s.admit(t.Context(), RunRequest{SessionID: "session"})
	require.ErrorAs(t, err, new(*SessionBusyError))
	_, ok = s.TrySessionMaintenance("session")
	require.False(t, ok)
	// Maintenance has not used the instance's one run slot.
	other, err := s.admit(t.Context(), RunRequest{SessionID: "other"})
	require.NoError(t, err)
	other()
	release()
	release()
	run, err := s.admit(t.Context(), RunRequest{SessionID: "session"})
	require.NoError(t, err)
	_, ok = s.TrySessionMaintenance("session")
	require.False(t, ok)
	run()
	_, ok = s.TrySessionMaintenance("")
	require.False(t, ok)
}
