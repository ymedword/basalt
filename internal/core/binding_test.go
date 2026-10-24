package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 验收点（T105）：绑定模型双向严格对应——引用缺绑定、绑定无引用、重复名、
// 重复 UID、未知类别都必须拒绝；数据库投影不参与判定。
// Acceptance points (T105): bindings must match references in both directions;
// missing, orphan, duplicate-name, duplicate-uid and unknown-kind input is refused.

const (
	testBindingUID1 ResourceUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	testBindingUID2 ResourceUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"
)

func TestBinding_RoundTripShape(t *testing.T) {
	t.Parallel()
	bindings := ResourceBindings{
		ModelConnections: []ResourceBinding{{Name: "primary", UID: testBindingUID1}},
		NotifyChannels:   []ResourceBinding{{Name: "ops", UID: testBindingUID2}},
	}
	require.NoError(t, bindings.Validate())
	require.NoError(t, bindings.ValidateCorrespondence([]string{"primary"}, []string{"ops"}))
	require.Equal(t, bindings.ModelConnections, bindings.Entries(BindingModelConnection))
	require.Equal(t, bindings.NotifyChannels, bindings.Entries(BindingNotifyChannel))
	require.Nil(t, bindings.Entries(BindingKind("skill")))
}

func TestBinding_RejectsDuplicateName(t *testing.T) {
	t.Parallel()
	duplicateName := ResourceBindings{
		ModelConnections: []ResourceBinding{
			{Name: "primary", UID: testBindingUID1},
			{Name: "primary", UID: testBindingUID2},
		},
	}
	require.ErrorIs(t, duplicateName.Validate(), ErrAgentResourceBindingInvalid)

	duplicateUID := ResourceBindings{
		NotifyChannels: []ResourceBinding{
			{Name: "ops", UID: testBindingUID1},
			{Name: "alerts", UID: testBindingUID1},
		},
	}
	require.ErrorIs(t, duplicateUID.Validate(), ErrAgentResourceBindingInvalid)

	missingBinding := ResourceBindings{
		ModelConnections: []ResourceBinding{{Name: "primary", UID: testBindingUID1}},
	}
	require.ErrorIs(t,
		missingBinding.ValidateCorrespondence([]string{"primary", "backup"}, nil),
		ErrAgentResourceBindingMissing, "引用缺绑定必须点名缺失")

	orphanBinding := ResourceBindings{
		NotifyChannels: []ResourceBinding{{Name: "ops", UID: testBindingUID2}},
	}
	require.ErrorIs(t,
		orphanBinding.ValidateCorrespondence(nil, nil),
		ErrAgentResourceBindingInvalid, "绑定无对应引用同样拒绝，不按名补绑")
}

func TestBinding_RejectsUnknownKind(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "skill", "mcp_server", "provider"} {
		_, err := ParseBindingKind(raw)
		require.ErrorIs(t, err, ErrAgentResourceBindingInvalid, raw)
	}
	kind, err := ParseBindingKind("model_connection")
	require.NoError(t, err)
	require.Equal(t, BindingModelConnection, kind)
}
