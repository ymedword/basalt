//go:build integration

package app

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/web"
)

// 作者目录必须复用真实快照，读取不会产生 Run 或调用审计。
// Authoring reads must expose current real pins without creating execution records.
func TestFlow_AuthoringCatalogUsesCurrentPinsWithoutDispatch(t *testing.T) {
	f := newFlowHTTPFixture(t)
	expected := liveFlowDefinition(t, f.rt).Nodes[0].Agent
	status, _, raw, response := f.request(http.MethodGet, "/api/v1/admin/flow-agents?limit=1", f.admin, "", "", nil)
	require.Equal(t, http.StatusOK, status, "authoring catalog must be available: %s", response)
	var page struct {
		Items []struct {
			Name        string         `json:"name"`
			Eligible    bool           `json:"eligible"`
			Reason      string         `json:"reason"`
			Agent       *flow.AgentRef `json:"agent"`
			MemoryScope string         `json:"memory_scope"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(raw, &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, "reader", page.Items[0].Name)
	require.True(t, page.Items[0].Eligible)
	require.Equal(t, "ready", page.Items[0].Reason)
	require.Equal(t, expected, page.Items[0].Agent)
	require.Equal(t, "legacy_default", page.Items[0].MemoryScope)
	require.NotContains(t, response, "api_key")
	require.NotContains(t, response, "prompt")
	require.NotContains(t, response, f.rt.root)
	for _, query := range []string{`SELECT count(*) FROM runs`, `SELECT count(*) FROM llm_calls`, `SELECT count(*) FROM tool_invocations`} {
		var count int
		require.NoError(t, f.rt.db.QueryRowContext(t.Context(), query).Scan(&count))
		require.Zero(t, count, query)
	}
	require.Empty(t, f.model.requests())
	for _, suffix := range []string{"?limit=0", "?limit=101", "?cursor=" + strings.Repeat("a", 129)} {
		status, _, _, _ = f.request(http.MethodGet, "/api/v1/admin/flow-agents"+suffix, f.admin, "", "", nil)
		require.Equal(t, http.StatusBadRequest, status)
	}
}

// 完整作者内容只能由 admin 按固定版本读取；数字正文必须通过字符串无损传输。
// Exact author versions are admin-only and carry numeric JSON as a lossless string.
func TestFlow_AuthoringVersionReadRequiresAdmin(t *testing.T) {
	f := newFlowHTTPFixture(t)
	definition := liveFlowDefinition(t, f.rt)
	definition.Contract.InputSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"},"exact":{"type":"integer","minimum":9007199254740993}},"required":["answer"],"additionalProperties":false}`)
	status, _, raw, response := f.request(http.MethodPost, "/api/v1/admin/flows/publish", f.admin, "", "", map[string]any{"operation_id": "authoring-publish", "expected_version": 0, "definition": definition})
	require.Equal(t, http.StatusOK, status, response)
	var published web.FlowDefinitionView
	require.NoError(t, json.Unmarshal(raw, &published))
	path := "/api/v1/admin/flows/" + published.UID + "/versions/" + strconv.FormatInt(published.Version, 10)
	status, _, raw, response = f.request(http.MethodGet, path, f.admin, "", "", nil)
	require.Equal(t, http.StatusOK, status, "exact author version must be readable: %s", response)
	var view struct {
		Bundle string `json:"bundle_json"`
	}
	require.NoError(t, json.Unmarshal(raw, &view))
	require.Contains(t, view.Bundle, "9007199254740993")
	var roundTrip flow.Definition
	require.NoError(t, json.Unmarshal([]byte(view.Bundle), &roundTrip))
	wantHash, err := flow.DefinitionHash(definition)
	require.NoError(t, err)
	actualHash, err := flow.DefinitionHash(roundTrip)
	require.NoError(t, err)
	require.Equal(t, wantHash, actualHash)
	key := f.issue("cal_authoring-reader", true, core.ScopeFlowsRead)
	status, _, _, response = f.request(http.MethodGet, path, key, "", "", nil)
	require.Equal(t, http.StatusForbidden, status)
	require.NotContains(t, response, "bundle_json")
	status, _, _, _ = f.request(http.MethodGet, path, "", "", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _, _, _ = f.request(http.MethodGet, "/api/v1/admin/flows/"+published.UID+"/versions/99", f.admin, "", "", nil)
	require.Equal(t, http.StatusNotFound, status)
	require.Empty(t, f.model.requests())
}

// 目录不是授权缓存：停用／重新启用产生新 generation，旧引用不能发布。
// The catalog is not an authorization cache; disabling and enabling invalidates old pins.
func TestFlow_AuthoringCatalogRejectsStaleAndDisabled(t *testing.T) {
	f := newFlowHTTPFixture(t)
	old := liveFlowDefinition(t, f.rt)
	a := f.rt.flows.runner.authority.(*flowAuthority)
	current, exists, err := a.lifecycle.Read(t.Context(), "reader")
	require.NoError(t, err)
	require.True(t, exists)
	unlock := a.gate.Lock("reader")
	disabled, err := a.lifecycle.Transition(t.Context(), "reader", current.State, current.Generation, core.AgentDisabled, "op-authoring-disable")
	unlock()
	require.NoError(t, err)
	status, _, raw, response := f.request(http.MethodGet, "/api/v1/admin/flow-agents", f.admin, "", "", nil)
	require.Equal(t, http.StatusOK, status, response)
	var page struct {
		Items []struct {
			Eligible bool           `json:"eligible"`
			Reason   string         `json:"reason"`
			Agent    *flow.AgentRef `json:"agent"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(raw, &page))
	require.Len(t, page.Items, 1)
	require.False(t, page.Items[0].Eligible)
	require.Nil(t, page.Items[0].Agent)
	require.Equal(t, "agent_disabled", page.Items[0].Reason)
	unlock = a.gate.Lock("reader")
	_, err = a.lifecycle.Transition(t.Context(), "reader", disabled.State, disabled.Generation, core.AgentActive, "op-authoring-enable")
	unlock()
	require.NoError(t, err)
	status, _, _, _ = f.request(http.MethodPost, "/api/v1/admin/flows/validate", f.admin, "", "", map[string]any{"definition": old})
	require.Equal(t, http.StatusBadRequest, status, "stale catalog pins must not validate after re-enable")
	require.Empty(t, f.model.requests())
}
