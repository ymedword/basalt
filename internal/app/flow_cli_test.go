package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/flow"
)

func flowCLIFiles(t *testing.T) (token, input, secret string) {
	t.Helper()
	dir := t.TempDir()
	tightenMasterKeyACLForTest(t, dir, true)
	token, input = filepath.Join(dir, "token"), filepath.Join(dir, "input.json")
	secret = "synthetic-flow-cli-secret"
	require.NoError(t, os.WriteFile(token, []byte(secret+"\n"), 0o600))
	tightenMasterKeyACLForTest(t, token, false)
	require.NoError(t, os.WriteFile(input, []byte(`{"answer":"input"}`), 0o600))
	return token, input, secret
}

func TestFlow_CLIRoutesThroughService(t *testing.T) {
	token, input, secret := flowCLIFiles(t)
	var calls atomic.Int64
	var identity string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "Bearer "+secret, r.Header.Get("Authorization"))
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/flow-tasks", r.URL.Path)
		identity = r.Header.Get("Idempotency-Key")
		require.NotEmpty(t, identity)
		var request struct {
			DefinitionUID string          `json:"definition_uid"`
			Input         json.RawMessage `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, "fd_fixture", request.DefinitionUID)
		require.JSONEq(t, `{"answer":"input"}`, string(request.Input))
		w.WriteHeader(202)
		_, err := io.WriteString(w, `{"code":"OK","data":{"task_id":"ft_fixture","status":"queued"}}`)
		require.NoError(t, err)
	}))
	defer server.Close()
	var output, diagnostic bytes.Buffer
	cmd := cli.NewRootCommand(cli.Deps{WorkDir: t.TempDir(), Flow: flowCLIPort{}, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostic)
	cmd.SetArgs([]string{"flow", "start", "fd_fixture", "--server", server.URL, "--token-file", token, "--input-file", input})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	require.EqualValues(t, 1, calls.Load())
	require.Contains(t, diagnostic.String(), identity, "identity must be available before an uncertain response")
	require.NotContains(t, output.String()+diagnostic.String(), secret)
	require.Contains(t, output.String(), "ft_fixture")
	require.False(t, needsRuntime([]string{"flow", "start"}))
	require.False(t, needsReadStore([]string{"flow", "status"}))
}

func TestFlow_CLIUncertainWriteRetainsIdentityAndRejectsUnsafeRequests(t *testing.T) {
	token, input, secret := flowCLIFiles(t)
	var calls, forwarded atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	request := cli.FlowRequest{Operation: "start", Server: server.URL, TokenFile: token, InputFile: input, ID: "fd_fixture", IdempotencyKey: "fixed-original-key"}
	_, err := (flowCLIPort{}).Request(t.Context(), request)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret)
	require.Zero(t, forwarded.Load(), "redirect must never carry a credential to another server")
	require.EqualValues(t, 1, calls.Load())
	for _, body := range []string{`{"answer":1,"answer":2}`, `{} {}`, `{"bad":NaN}`, strings.Repeat("[", 17) + "0" + strings.Repeat("]", 17)} {
		require.NoError(t, os.WriteFile(input, []byte(body), 0o600))
		_, err = (flowCLIPort{}).Request(t.Context(), request)
		require.Error(t, err)
		require.NotContains(t, err.Error(), secret)
	}
	require.EqualValues(t, 1, calls.Load(), "invalid JSON is rejected before any HTTP dispatch")
	request.Server = "http://example.invalid"
	_, err = (flowCLIPort{}).Request(t.Context(), request)
	require.ErrorContains(t, err, "HTTPS")
	request.Server = server.URL + "?token=" + secret
	_, err = (flowCLIPort{}).Request(t.Context(), request)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret)
}

func TestFlow_CLIStatusDoesNotTreatStoppedAsBusinessSuccess(t *testing.T) {
	token, _, _ := flowCLIFiles(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		_, err := io.WriteString(w, `{"code":"OK","data":{"task_id":"ft_fixture","status":"stopped"}}`)
		require.NoError(t, err)
	}))
	defer server.Close()
	var output bytes.Buffer
	cmd := cli.NewRootCommand(cli.Deps{Flow: flowCLIPort{}})
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"flow", "status", "ft_fixture", "--server", server.URL, "--token-file", token})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	require.Contains(t, output.String(), "stopped", "failed business result remains visible")
}

func TestFlow_CLIOfflineStructureHasNoRuntimeAuthority(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "local-probe")
	require.NoError(t, os.Mkdir(dir, 0o700))
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	cap := flow.BudgetCap{MaxTotalTokens: 100, MaxToolCalls: 10, MaxDurationSeconds: 10, MaxAttemptsPerNode: 1}
	definition := flow.Definition{FormatVersion: 1, Name: "local-probe", Entry: "draft", TaskContract: "TASK.json", Limits: cap, Nodes: []flow.Node{{ID: "draft", Kind: "agent", Agent: &flow.AgentRef{UID: "agt_synthetic", Name: "unresolved-agent", Version: 1, ConfigHash: strings.Repeat("a", 64), ManifestHash: strings.Repeat("b", 64)}, OutputSchema: schema, Failure: flow.FailurePolicy{MaxAttempts: 1}}}, Contract: flow.TaskContract{ContractVersion: 1, Goal: "offline probe", Constraints: []string{"keep constraint"}, InputSchema: schema, OutputSchema: schema, Budget: cap, ArtifactSpec: []flow.ArtifactSpec{{ID: "report", Format: "json", Required: true}}, Acceptance: []flow.AcceptanceRule{{ID: "shape", Version: 1, Kind: "deterministic", TargetKind: "artifact", TargetID: "report", Required: true, Schema: schema}}, StopConditions: []string{"budget"}, Escalation: "stop"}}
	def, contract, err := flow.DefinitionContent(definition)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "FLOW.yaml"), def, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "TASK.json"), contract, 0o600))
	raw, err := (flowCLIPort{}).ValidateStructure(context.Background(), dir)
	require.NoError(t, err)
	require.Contains(t, string(raw), "structure_only")
	require.Contains(t, string(raw), "发布依赖")
	// The export path is read-only and emits exactly the bundle consumed by the
	// management UI, rather than mixing the structural receipt into author fields.
	var output bytes.Buffer
	cmd := cli.NewRootCommand(cli.Deps{Flow: flowCLIPort{}})
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"flow", "validate", dir, "--bundle"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	var exported flow.Definition
	require.NoError(t, json.Unmarshal(output.Bytes(), &exported))
	require.Equal(t, definition.Name, exported.Name)
	require.Equal(t, definition.Contract.Goal, exported.Contract.Goal)
	require.NotContains(t, output.String(), "structure_only")
	// No runtime or dependency registry is installed. Full publication cannot
	// silently reuse this structural verdict as permission or dependency proof.
	err = (&flow.ContractValidator{}).ValidateDefinition(t.Context(), definition)
	require.Error(t, err)
	for _, bad := range []string{"format_version: 1\nformat_version: 2\n", string(def) + " {}", `{"format_version":1,"contract":{"goal":"injected"}}`} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "FLOW.yaml"), []byte(bad), 0o600))
		_, err = (flowCLIPort{}).ValidateStructure(t.Context(), dir)
		require.Error(t, err)
	}
}
