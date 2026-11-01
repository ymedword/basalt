package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

func TestAdminMemory_UTF8RequestReachesPortUnchanged(t *testing.T) {
	s, fake := newMemoryAdminServer(t, nil)
	fake.proposeFn = func(context.Context, core.Actor, core.MemoryProposal) (core.MemoryEntryResult, error) {
		return core.MemoryEntryResult{EntryID: 1, State: "candidate"}, nil
	}
	const text = "项目代号：蓝岩-2048；交接时间 09:30；表情 🪨"
	response := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/proposals",
		`{"content":"`+text+`"}`)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, text, fake.lastProposal.Content)
}
