//go:build integration

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/assets"
	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/observability"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/web"
)

type scenarioRunSpy struct {
	web.RunService
	seen chan core.RunRequest
}

type scenarioNamedTool struct {
	*armedTool
	name string
}

func (t *scenarioNamedTool) Name() string { return t.name }

func (s *scenarioRunSpy) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	s.seen <- req
	return s.RunService.Run(ctx, req)
}

// scenarioHTTP 只替换外部协议；认证、限流、Run、工具、审计及OBS读取均为真实实例。
// scenarioHTTP substitutes only outside protocols; auth, admission, runs, tools, audits, and observation reads are real.
func scenarioHTTP(t *testing.T, h *runHarness) (string, string, <-chan core.RunRequest) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	secret := "bk_" + strings.Repeat("a", 40)
	sum := sha256.Sum256([]byte(secret))
	keys := postgres.NewKeyStore(h.db)
	scopes := append(h.principal(runHarnessCaller).Scopes, core.AgentInvokeScopePrefix+h.agent)
	require.NoError(t, keys.Create(t.Context(), callers.KeyRecord{KeyID: "key_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A", CallerID: runHarnessCaller, KeyHash: hex.EncodeToString(sum[:]), KeyPrefix: secret[:8], Scopes: scopes, CreatedAt: time.Now()}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	guard, err := web.NewAuthGuardWithPrincipals(web.AdminAuthConfig{Mode: "token", Token: "synthetic-admin", AllowedHosts: []string{addr}}, logger, time.Now, callers.NewAuthenticator(keys, adminTokenFingerprint{token: "synthetic-admin"}))
	require.NoError(t, err)
	mem, err := memory.NewService(memory.Options{Backend: memory.NewMarkdownStore(t.TempDir()), Logger: logger})
	require.NoError(t, err)
	spy := &scenarioRunSpy{RunService: h.service, seen: make(chan core.RunRequest, 1)}
	srv, err := web.NewServer(web.Options{Listener: ln, Addr: addr, RunBudget: harnessBudget(), Runs: spy, Sessions: postgres.NewSessionStore(h.db), Agents: agentProfilesPort{registry: h.agents}, Tools: newToolCatalogPort(h.registry, nil), Memory: mem, Console: assets.Console(), AdminAuth: guard, Ingress: h.limiter, Observability: h.observation, DiagnosticWriter: h.diag, Diagnostics: diagnostics.NewReader(postgres.NewDiagnosticStore(h.db), diagnostics.DefaultConfig(), nil), Logger: logger})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("scenario HTTP shutdown timed out")
		}
	})
	return "http://" + addr, secret, spy.seen
}

func scenarioTimeline(t *testing.T, h *runHarness, id string) observability.Timeline {
	t.Helper()
	got, err := h.observation.Timeline(t.Context(), observability.TimelineQuery{Anchor: observability.Anchor{Kind: observability.AnchorRun, Value: id}, Limit: 1000})
	require.NoError(t, err)
	return got
}

func scenarioMetric(t *testing.T, h *runHarness, name string) float64 {
	t.Helper()
	snapshot, err := h.observation.MetricsSnapshot(t.Context(), 15*time.Minute)
	require.NoError(t, err)
	var value float64
	found := false
	for _, sample := range snapshot.Samples {
		if sample.Name == name {
			require.True(t, sample.Available, name+" must be a real available reading")
			value += sample.Value
			found = true
		}
	}
	require.True(t, found, "required catalog reading missing: "+name)
	return value
}

func TestScenario_NormalChain(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true, WithAssembly: true, Observability: true})
	e.ingest(t)
	e.provider.respondWithToolCalls(1)
	e.provider.mu.Lock()
	e.provider.text = knRunE2EAnswerCited
	e.provider.mu.Unlock()
	base, secret, seen := scenarioHTTP(t, e.runHarness)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/api/v1/agents/"+e.agent+"/invoke", strings.NewReader(`{"message":"退款政策"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "attacker-controlled")
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	res, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, 200, res.StatusCode, string(body))
	var envelope struct {
		Data web.RunView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Equal(t, "completed", envelope.Data.Status)
	entered := <-seen
	require.NotEmpty(t, entered.RequestID)
	require.NotEqual(t, "attacker-controlled", entered.RequestID)
	facts, err := postgres.NewObservabilityStore(e.db).RunFacts(t.Context(), envelope.Data.RunID)
	require.NoError(t, err)
	require.Equal(t, entered.RequestID, facts.RequestID)
	require.Equal(t, entered.RequestID, envelope.Data.RequestID)
	got := scenarioTimeline(t, e.runHarness, facts.RunID)
	require.Empty(t, got.Gaps, "all normally produced sources must be readable")
	counts := map[observability.SourceKind]int{}
	purposes := map[string]int{}
	for i, entry := range got.Entries {
		counts[entry.Source]++
		if entry.Source == observability.SourceAssembly {
			purposes[entry.Attrs["purpose"].(string)]++
		}
		if i > 0 {
			require.False(t, entry.OccurredAt.Before(got.Entries[i-1].OccurredAt), "local chronology cannot run backwards")
		}
	}
	require.Equal(t, 1, counts[observability.SourceRun])
	require.Equal(t, 2, counts[observability.SourceLLM])
	require.Equal(t, 1, counts[observability.SourceTool])
	require.Equal(t, 1, purposes["retrieval"])
	require.Equal(t, 2, purposes["chat"])
	store := postgres.NewObservabilityStore(e.db)
	llm, err := store.LLMCallsByRun(t.Context(), facts.RunID)
	require.NoError(t, err)
	require.Len(t, llm, 2)
	tools, err := store.ToolInvocationsByRun(t.Context(), facts.RunID)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, "call-1", tools[0].ToolCallID)
	diagnosis, err := e.observation.Context(t.Context(), facts.RunID)
	require.NoError(t, err)
	require.Len(t, diagnosis.Rounds, 2)
	require.NotEmpty(t, diagnosis.Retrieval.Hits)
	require.Equal(t, float64(1), scenarioMetric(t, e.runHarness, "obs_runs_total"))
}

func TestScenario_ModelTimeout(t *testing.T) {
	h := newRunHarnessWith(t, runHarnessOptions{Observability: true, Assembly: func(t *testing.T, db *postgres.DB) core.AssemblyRecorder {
		return postgres.NewAssemblyStore(db, slog.New(slog.DiscardHandler))
	}})
	h.provider.failAfterTimeout()
	req := h.requestFor(runHarnessCaller)
	req.RequestID = "scenario-timeout"
	res, err := h.invoker.Run(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, core.StatusFailed, res.Status)
	require.Equal(t, 1, h.provider.calls(), "protocol call really happened")
	calls, err := postgres.NewObservabilityStore(h.db).LLMCallsByRun(t.Context(), res.RunID)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, "failed", calls[0].Status)
	require.Equal(t, "timeout", calls[0].ErrorKind)
	got := scenarioTimeline(t, h, res.RunID)
	llmEvent, audit := false, false
	for _, entry := range got.Entries {
		if entry.Source == observability.SourceDiagnostic && entry.Kind == "llm_call" && entry.ReasonCode == "llm_call_failed" {
			llmEvent = true
		}
		if entry.Source == observability.SourceLLM && entry.ReasonCode == "timeout" && entry.Status == "failed" {
			audit = true
		}
	}
	require.True(t, llmEvent, "sent timeout must have its real producer diagnostic")
	require.True(t, audit)
	require.Equal(t, float64(1), scenarioMetric(t, h, "obs_llm_timeout_total"))
}

func TestScenario_ToolDenied(t *testing.T) {
	h := newRunHarnessWith(t, runHarnessOptions{Observability: true, RequestedTool: "never-authorized"})
	require.NoError(t, h.registry.Register(&scenarioNamedTool{armedTool: h.tools, name: "never-authorized"}))
	h.provider.respondWithToolCalls(1)
	res, err := h.invoker.Run(t.Context(), h.requestFor(runHarnessCaller))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	tools, err := postgres.NewObservabilityStore(h.db).ToolInvocationsByRun(t.Context(), res.RunID)
	require.NoError(t, err)
	require.Empty(t, tools, "denial happens before the tool audit begins")
	h.tools.mu.Lock()
	invocations := h.tools.invocations
	h.tools.mu.Unlock()
	require.Zero(t, invocations, "forbidden request must have no tool execution effect")
	events, err := postgres.NewObservabilityStore(h.db).ListEvents(t.Context(), diagnostics.Query{RunID: res.RunID, Limit: 1000})
	require.NoError(t, err)
	denials := 0
	for _, event := range events {
		if event.Kind == diagnostics.KindPermissionDecision && event.ReasonCode == diagnostics.ReasonPermissionDenied {
			denials++
			require.Equal(t, diagnostics.DecisionDeny, event.Decision)
			require.Equal(t, "call-1", event.Attrs["tool_call_id"])
		}
	}
	require.Equal(t, 1, denials)
	got := scenarioTimeline(t, h, res.RunID)
	found := false
	for _, entry := range got.Entries {
		if entry.Kind == "permission_decision" && entry.ReasonCode == "permission_denied" {
			found = true
		}
	}
	require.True(t, found)
	require.Equal(t, float64(1), scenarioMetric(t, h, "obs_tool_denials_total"))
}

func TestScenario_EmptyRetrieval(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true, WithAssembly: true, Observability: true})
	// 真正空库、不调用摄取；索引代际已固定，检索仍走实际授权与SQL路径。
	// The actual empty collection still runs authorization and SQL retrieval.
	e.provider.respondWithText("本次没有检索到资料")
	res, err := e.invoker.Run(t.Context(), e.requestFor(runHarnessCaller))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	diagnosis, err := e.observation.Context(t.Context(), res.RunID)
	require.NoError(t, err)
	require.NotNil(t, diagnosis.Retrieval)
	require.Empty(t, diagnosis.Retrieval.Hits)
	require.Len(t, diagnosis.Rounds, 1)
	require.Equal(t, float64(1), scenarioMetric(t, e.runHarness, "obs_retrieval_empty_total"))
	events, err := postgres.NewObservabilityStore(e.db).ListEvents(t.Context(), diagnostics.Query{RunID: res.RunID, Limit: 1000})
	require.NoError(t, err)
	for _, event := range events {
		require.NotEqual(t, diagnostics.ReasonNoAnswer, event.ReasonCode, "empty retrieval is not evidence that the model produced no answer")
	}
	for _, round := range diagnosis.Rounds {
		require.NotContains(t, round.ReasonCodes, "no_answer")
	}
}

func TestScenario_PermissionAndBudget(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true, WithAssembly: true, Observability: true, RetrievalConfig: knowledge.RetrievalConfig{TopK: 2, CandidateK: 20}, ExtraBindings: []core.KnowledgeBinding{{Name: "obs-project", Access: core.KnowledgeAccessRead}, {Name: "obs-deleted", Access: core.KnowledgeAccessRead}}})
	_, err := e.db.ExecContext(t.Context(), `UPDATE callers SET project='obs-project' WHERE caller_id=$1`, runHarnessCaller)
	require.NoError(t, err)
	_, err = e.db.ExecContext(t.Context(), `UPDATE kb_collections SET project='obs-project' WHERE collection_id=$1`, knRunE2ECollectionID)
	require.NoError(t, err)
	base := e.ingest(t)
	for _, kind := range []string{"project", "deleted", "inactive"} {
		obsCloneFilteredMaterial(t, e, base.VersionID, kind)
	}
	for i, name := range []string{"z-budget", "z-rank"} {
		scenarioAllowedMaterial(t, e, base.VersionID, name, strings.Repeat("padding ", 10000+i*5000))
	}
	e.provider.respondWithText(knRunE2EAnswerCited)
	run := func() core.RunResult {
		req := e.requestFor(runHarnessCaller)
		req.Budget.ContextTokens = 20000
		res, err := e.invoker.Run(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, core.StatusCompleted, res.Status)
		return res
	}
	res := run()
	diagnosis, err := e.observation.Context(t.Context(), res.RunID)
	require.NoError(t, err)
	require.NotNil(t, diagnosis.Retrieval)
	r := diagnosis.Retrieval
	require.Equal(t, 5, r.FilteredCount, "2 denied collections + max(3 keyword rows,3 generation-wide vector rows)")
	require.Equal(t, "not_distinguished", r.FilteredReason)
	require.Len(t, r.Candidates, 3)
	decisions := map[string]string{}
	for _, c := range r.Candidates {
		decisions[c.ChunkID] = c.Decision
		require.Equal(t, "allow", c.ACLDecision)
		require.Nil(t, c.RerankScore)
	}
	require.Equal(t, "budget_drop", decisions["z-budget"])
	require.Equal(t, "rank_cut", decisions["z-rank"])
	kept := 0
	for _, d := range decisions {
		if d == "kept" {
			kept++
		}
	}
	require.Equal(t, 1, kept)
	raw, err := json.Marshal(diagnosis)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "CANARY_FILTERED")
	require.Contains(t, string(raw), `"filtered_count":"direct"`)
	require.Contains(t, string(raw), `"filtered_reason":"insufficient"`)
	events, err := postgres.NewObservabilityStore(e.db).ListEvents(t.Context(), diagnostics.Query{RunID: res.RunID, Limit: 1000})
	require.NoError(t, err)
	filtered := 0
	for _, event := range events {
		if event.Kind == diagnostics.KindRetrieval && event.ReasonCode == diagnostics.ReasonPermissionDenied {
			filtered++
			require.Equal(t, float64(5), event.Attrs["count"])
		}
	}
	require.Equal(t, 1, filtered)
	// 达到真实工具拒绝阈值所需的事件数，仍只有检索过滤；错误按reason计数会触发。
	// Reach the actual tool-refusal threshold using retrieval events only; reason-only counting would fire.
	for range 19 {
		run()
	}
	store := postgres.NewObservabilityStore(e.db)
	for _, rule := range observability.BuiltinAlertRules(e.observation.Metrics, store) {
		if rule.ID == "tool_denial_burst" {
			samples, err := rule.Collect(t.Context(), time.Now())
			require.NoError(t, err)
			require.Len(t, samples, 1)
			require.Zero(t, samples[0].Value)
		}
	}
	require.Zero(t, scenarioMetric(t, e.runHarness, "obs_tool_denials_total"))
}

func scenarioAllowedMaterial(t *testing.T, e *knRunE2E, baseVersion, id, padding string) {
	t.Helper()
	text := knRunE2EDocBody + padding
	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:])
	doc, version := "doc-"+id, "ver-"+id
	_, err := e.db.ExecContext(t.Context(), `INSERT INTO kb_documents(document_id,collection_id,source_path,content_hash,created_at,updated_at) VALUES($1,$2,$3,$4,1,1)`, doc, knRunE2ECollectionID, "synthetic/"+id+".md", hash)
	require.NoError(t, err)
	_, err = e.db.ExecContext(t.Context(), `INSERT INTO kb_document_versions(version_id,document_id,content_hash,status,parser_version,chunker_version,generation,source_text,created_at,updated_at) VALUES($1,$2,$3,'active','text-v1','chunk-v1',1,$4,1,1)`, version, doc, hash, text)
	require.NoError(t, err)
	_, err = e.db.ExecContext(t.Context(), `INSERT INTO kb_chunks(chunk_id,version_id,seq,char_start,char_end,text,content_hash,created_at) VALUES($1,$2,1,0,$3,$4,$5,1)`, id, version, len([]rune(text)), text, hash)
	require.NoError(t, err)
	result, err := e.db.ExecContext(t.Context(), `INSERT INTO kb_chunk_embeddings(chunk_id,generation,embedding,model,dimensions,created_at) SELECT $1,generation,embedding,model,dimensions,1 FROM kb_chunk_embeddings WHERE chunk_id IN(SELECT chunk_id FROM kb_chunks WHERE version_id=$2)`, id, baseVersion)
	require.NoError(t, err)
	n, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "each visible fixture has exactly one current-generation vector")
}
