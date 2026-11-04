//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

type (
	flowCursorPage struct {
		Source string `json:"source"`
		Next   int    `json:"next"`
	}
	flowCursorProbe struct {
		target string
		client *http.Client
	}
)

func (*flowCursorProbe) Name() string { return "flow_probe" }
func (*flowCursorProbe) Description() string {
	const description = "registered loopback pagination protocol probe"
	return description
}

func (*flowCursorProbe) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"cursor":{"type":"integer"},"nonce":{"type":"integer"}},"required":["cursor","nonce"],"additionalProperties":false}`)
}

func (p *flowCursorProbe) Execute(ctx context.Context, _ core.ToolEnv, raw json.RawMessage) (core.Result, error) {
	var args struct{ Cursor int }
	if err := json.Unmarshal(raw, &args); err != nil {
		return core.Result{}, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, p.target+"?cursor="+strconv.Itoa(args.Cursor), nil)
	if err != nil {
		return core.Result{}, err
	}
	response, err := p.client.Do(r)
	if err != nil {
		return core.Result{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	return core.Result{Content: string(body)}, err
}

// FR016：原执行器/原审计/PG验证代码注册的单调协议；不是通用MCP分页支持声明。
// Prove the trusted cursor seam with real I/O and durable history, not arbitrary basis strings.
func TestFlow_TrustedCursorProgressRequiresOriginalAuditAndMonotonicPage(t *testing.T) {
	for _, mode := range []string{"advance", "repeat", "backward", "forged-source"} {
		t.Run(mode, func(t *testing.T) {
			p := newPGFlowActionProbe(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p.received.Add(1)
				cursor, err := strconv.Atoi(r.URL.Query().Get("cursor"))
				if err != nil {
					t.Error(err)
					return
				}
				page := flowCursorPage{Source: "registered-source-v1", Next: cursor + 1}
				if mode == "backward" {
					page.Next = cursor - 1
				}
				if mode == "forged-source" {
					page.Source = "model-claimed-source"
				}
				if err := json.NewEncoder(w).Encode(page); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			pinned := &flowCursorProbe{target: server.URL, client: server.Client()}
			compiler := tool.NewSchemaCompiler()
			schema, err := compiler.Compile(t.Context(), pinned.InputSchema())
			require.NoError(t, err)
			p.executor = core.NewToolExecutor(pgFlowProbeResolver{pinned}, NewToolInvocationRepo(p.store.db), nil)
			p.env.Tools = map[string]core.Tool{pinned.Name(): pinned}
			p.execution.OriginalSchemaHash = flowDataHash(pinned.InputSchema())
			p.execution.Validator = compiler
			p.execution.Request.SchemaHash = schema
			proofCalls := 0
			proof := func(ctx context.Context, call core.ToolCall, result core.Result, _ core.ActionOutcome) (string, error) {
				proofCalls++
				var input struct{ Cursor int }
				var page flowCursorPage
				if json.Unmarshal([]byte(call.Arguments), &input) != nil || json.Unmarshal([]byte(result.Content), &page) != nil || page.Source != "registered-source-v1" {
					return "", nil
				}
				task, err := p.store.ReadForWorker(ctx, p.task.ID)
				if err != nil {
					return "", err
				}
				last := 0
				for _, basis := range task.Revision.Progress[p.task.ID+":draft"].Basis {
					if strings.HasPrefix(basis, "cursor:registered-source-v1:") {
						n, err := strconv.Atoi(strings.TrimPrefix(basis, "cursor:registered-source-v1:"))
						if err != nil {
							return "", err
						}
						last = max(last, n)
					}
				}
				if input.Cursor != last || page.Next <= last || page.Next > 8 {
					return "", nil
				}
				return "cursor:registered-source-v1:" + strconv.Itoa(page.Next), nil
			}
			for i := range 9 {
				guard := &FlowProgress{Store: NewFlowStore(p.store.db), Principal: p.env.Principal, TaskID: p.task.ID, NodeAttemptID: "node-action", Proof: proof}
				p.execution.Flow.Progress = guard // Discard all guard memory between every page.
				cursor := i
				if mode == "repeat" && i > 1 {
					cursor = 1
				}
				if mode == "backward" || mode == "forged-source" {
					cursor = 0
				}
				input := json.RawMessage(fmt.Sprintf(`{"cursor":%d,"nonce":%d}`, cursor, i))
				p.call.Arguments = string(input)
				p.execution.Request.Input = input
				p.execution.Request.InputHash = flowDataHash(input)
				p.execution.Request.LogicalSlot = fmt.Sprint("cursor-", i)
				_, out, err := p.run(t.Context())
				if mode != "advance" && ((mode == "repeat" && i == 8) || (mode != "repeat" && i == 6)) {
					var limit *core.ProgressLimitError
					require.ErrorAs(t, err, &limit)
					require.Equal(t, "no_progress", limit.Reason)
					require.False(t, out.Dispatched)
					break
				}
				require.NoError(t, err)
				require.True(t, out.AuditConfirmed)
				if mode == "advance" && i == 6 {
					break
				}
			}
			task, err := p.store.ReadForWorker(t.Context(), p.task.ID)
			require.NoError(t, err)
			window := task.Revision.Progress[p.task.ID+":draft"]
			if mode == "advance" {
				require.Len(t, window.Basis, 7)
				require.Zero(t, window.NoProgress)
				require.EqualValues(t, 7, p.received.Load())
			} else {
				require.Equal(t, 6, window.NoProgress)
			}
			require.EqualValues(t, p.received.Load(), proofCalls, "only terminal original-audit-confirmed dispatches reach trusted cursor validation")
			before := proofCalls
			err = (&FlowProgress{Store: p.store, Principal: p.env.Principal, TaskID: p.task.ID, NodeAttemptID: "node-action", Proof: proof}).AfterCall(t.Context(), p.call, core.Result{}, core.ActionOutcome{AuditID: 999999, Dispatched: true, AuditConfirmed: true, Outcome: "success", OutputHash: "forged"})
			require.Error(t, err)
			require.Equal(t, before, proofCalls, "forged audit identity cannot invoke the proof adapter")
		})
	}
}
