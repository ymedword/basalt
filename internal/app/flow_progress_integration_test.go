//go:build integration

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// 真实 ToolExecutor、权限、文件与 PG；只有外部 Chat 协议使用脚本。
// Real executor, policy, files and PG; only the external Chat protocol is scripted.
func TestFlow_ProductionProgressAcceptsNewSourcesAndRejectsChangedBytes(t *testing.T) {
	for _, distinct := range []bool{true, false} {
		t.Run(fmt.Sprint("distinct=", distinct), func(t *testing.T) {
			fx := newWireFixture(t)
			fx.writeAgent(t, "reader", "read_file")
			fx.cfg.Flows = flow.DefaultConfig()
			fx.cfg.Flows.Enabled = true
			usage := core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}
			model := &scriptedProviderClient{}
			for i := range 7 {
				name := fmt.Sprintf("source-%d.txt", i)
				if !distinct {
					name = "source-0.txt"
				}
				require.NoError(t, os.WriteFile(filepath.Join(fx.root, "agents", "reader", name), []byte(fmt.Sprint("source ", i)), 0o600))
				args, err := json.Marshal(map[string]string{"path": name})
				require.NoError(t, err)
				model.replies = append(model.replies, core.ChatResponse{ToolCalls: []core.ToolCall{{ID: fmt.Sprint("read-", i), Name: "read_file", Arguments: string(args)}}, Usage: usage})
			}
			model.replies = append(model.replies, core.ChatResponse{Content: `{"answer":"read sources"}`, Usage: usage})
			protocol := &flowProofModel{inner: model}
			if !distinct {
				protocol.after = func() {
					require.NoError(t, os.WriteFile(filepath.Join(fx.root, "agents", "reader", "source-0.txt"), []byte(fmt.Sprint("changed at call ", len(model.requests()))), 0o600))
				}
			}
			fx.override = protocol
			rt := fx.assemble(t)
			rt.adminPosture.Set(core.AdminPostureToken)
			require.NoError(t, rt.flows.prepare(t.Context()))
			definition := liveFlowDefinition(t, rt)
			published, err := rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-progress", Definition: definition})
			require.NoError(t, err)
			task, err := rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: published.DefinitionUID, IdempotencyKey: "progress", Input: json.RawMessage(`{"answer":"input"}`)})
			require.NoError(t, err)
			finished, err := advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version})
			require.NoError(t, err)
			window := finished.Revision.Progress[task.ID+":draft"]
			var calls int
			require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM tool_invocations i JOIN runs r ON r.run_id=i.run_id WHERE r.flow_task_id=$1`, task.ID).Scan(&calls))
			if distinct {
				require.Equal(t, flow.Succeeded, finished.Status, "reason=%s checkpoint=%+v", finished.Reason, finished.Revision)
				require.Equal(t, 7, calls)
				require.Len(t, model.requests(), 8)
				require.Len(t, window.Basis, 7)
				require.Zero(t, window.NoProgress)
			} else {
				require.Equal(t, flow.Stopped, finished.Status)
				require.Equal(t, 2, calls, "third repeated call must be refused before dispatch")
				require.Len(t, model.requests(), 3)
				require.Len(t, window.Basis, 1)
				require.Equal(t, 1, window.NoProgress)
			}
		})
	}
}

func TestFlow_ProductionAliasReplacementChainCannotMintProgress(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader", "read_file")
	fx.cfg.Flows = flow.DefaultConfig()
	fx.cfg.Flows.Enabled = true
	path := func(name string) string { return filepath.Join(fx.root, "agents", "reader", name+".txt") }
	require.NoError(t, os.WriteFile(path("a"), []byte("same bytes"), 0o600))
	require.NoError(t, os.Link(path("a"), path("b")))
	model := &scriptedProviderClient{}
	for i, name := range []string{"a", "b", "b", "c", "c", "d", "d", "e"} {
		args, err := json.Marshal(map[string]string{"path": name + ".txt"})
		require.NoError(t, err)
		model.replies = append(model.replies, core.ChatResponse{ToolCalls: []core.ToolCall{{ID: fmt.Sprint("alias-", i), Name: "read_file", Arguments: string(args)}}, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}})
	}
	protocol := &flowProofModel{inner: model, after: func() {
		switch n := len(model.requests()); n {
		case 3, 5, 7:
			name := map[int]string{3: "b", 5: "c", 7: "d"}[n]
			require.NoError(t, os.WriteFile(path("replacement"), []byte("same bytes"), 0o600))
			require.NoError(t, os.Rename(path("replacement"), path(name)))
		case 4, 6, 8:
			names := map[int][2]string{4: {"b", "c"}, 6: {"c", "d"}, 8: {"d", "e"}}[n]
			require.NoError(t, os.Link(path(names[0]), path(names[1])))
		}
	}}
	fx.override = protocol
	rt := fx.assemble(t)
	rt.adminPosture.Set(core.AdminPostureToken)
	require.NoError(t, rt.flows.prepare(t.Context()))
	published, err := rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-alias-chain", Definition: liveFlowDefinition(t, rt)})
	require.NoError(t, err)
	task, err := rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: published.DefinitionUID, IdempotencyKey: "alias-chain", Input: json.RawMessage(`{"answer":"input"}`)})
	require.NoError(t, err)
	finished, err := advanceUnderFlowRoot(t, rt.db, rt.flows.service, task.RootTaskID, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version})
	require.NoError(t, err)
	require.Equal(t, flow.Stopped, finished.Status)
	window := finished.Revision.Progress[task.ID+":draft"]
	require.Equal(t, 6, window.NoProgress)
	require.Len(t, window.Calls, 7)
	require.Len(t, model.requests(), 8)
	var calls int
	require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM tool_invocations i JOIN runs r ON r.run_id=i.run_id WHERE r.flow_task_id=$1`, task.ID).Scan(&calls))
	require.Equal(t, 7, calls, "the eighth alias/replacement call is refused before dispatch")
}
