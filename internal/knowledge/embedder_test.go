package knowledge

// 本文件覆盖 T036：`HTTPEmbedder` 的协议行为，以及摄取侧两阶段审计与真实客户端
// 的接线。
//
// 两类用例共用一个进程内假提供方（`newEmbeddingServer`）：
//   - 接线类（`TestEmbeddingAudit_*` / `TestEmbedding_RejectsDimensionMismatch`）
//     用真实客户端驱动完整摄取样例，验证"审计 `pending` 先于调用""维度不符在
//     校验阶段失败而不是被客户端抹平"；
//   - 客户端类（`TestHTTPEmbedder_*`）直接驱动客户端：顺序归位、错误脱敏、响应
//     解析、未知用量、空输入与截止时间。
//
// This file covers the embedding client and its ingest wiring: the httptest
// provider proves pre-write ordering and verify-stage dimension checks.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// embeddingServerOptions 控制假提供方的回执形态；零值 = 正常回执（按请求维度
// 回非零向量、带用量、带序号）。
// embeddingServerOptions shapes one fake provider reply; the zero value is normal.
type embeddingServerOptions struct {
	width        int    // 非零时无视请求维度，按此宽度回向量（维度不符用例）
	omitUsage    bool   // 不带 usage 对象（用量未知用例）
	promptTokens int64  // usage.prompt_tokens
	model        string // 非空则回带 model
	omitIndex    bool   // 不回 index（按数组序用例）
	reverse      bool   // 倒序返回数组，但每条声明的 index 保持真实位置
	status       int    // 非零则回该状态码
	errorMessage string // 非零状态时的 error.message
	rawBody      string // 非空时原样回写正文（畸形响应用例）
	onRequest    func(req embeddingRequest, header http.Header)
}

// newEmbeddingServer 起一个进程内 OpenAI 兼容 `/embeddings` 端点：第 i 条向量的
// 首分量为 i+1，便于断言"按 index 归位"；`reverse` 只倒数组序、不动声明的 index。
// newEmbeddingServer starts an in-process embeddings endpoint; vector i starts
// with i+1 so ordering assertions are exact.
func newEmbeddingServer(t *testing.T, opts embeddingServerOptions) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/embeddings" {
			http.NotFound(w, r)
			return
		}
		var req embeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if opts.onRequest != nil {
			opts.onRequest(req, r.Header)
		}
		if opts.rawBody != "" {
			writeServerText(t, w, opts.rawBody)
			return
		}
		if opts.status != 0 {
			w.WriteHeader(opts.status)
			writeServerJSON(t, w, map[string]any{
				"error": map[string]any{"message": opts.errorMessage},
			})
			return
		}
		width := opts.width
		if width == 0 {
			width = req.Dimensions
		}
		data := make([]embeddingEntry, len(req.Input))
		for idx := range req.Input {
			vector := make([]float32, width)
			vector[0] = float32(idx + 1)
			entry := embeddingEntry{Embedding: vector}
			if !opts.omitIndex {
				index := idx
				entry.Index = &index
			}
			data[idx] = entry
		}
		if opts.reverse {
			for left, right := 0, len(data)-1; left < right; left, right = left+1, right-1 {
				data[left], data[right] = data[right], data[left]
			}
		}
		body := map[string]any{"data": data}
		if opts.model != "" {
			body["model"] = opts.model
		}
		if !opts.omitUsage {
			body["usage"] = map[string]any{"prompt_tokens": opts.promptTokens}
		}
		writeServerJSON(t, w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeServerText 原样回写正文（畸形响应）。
// writeServerText writes arbitrary bytes back verbatim.
func writeServerText(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := fmt.Fprint(w, body); err != nil {
		t.Errorf("假提供方写响应失败: %v", err)
	}
}

// writeServerJSON 回写 JSON 响应。
// writeServerJSON encodes one JSON reply.
func writeServerJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("假提供方编码响应失败: %v", err)
	}
}

// mustEmbedder 用假提供方地址构造客户端（Key 固定为夹具值）。
// mustEmbedder builds a client against the fake provider.
func mustEmbedder(t *testing.T, baseURL string) *HTTPEmbedder {
	t.Helper()
	embedder, err := NewHTTPEmbedder(EmbedderOptions{
		Name:       "fixture",
		BaseURL:    baseURL,
		APIKey:     "key_fixture",
		Model:      "text-embedding-v4",
		Dimensions: VectorDimensions,
	})
	require.NoError(t, err)
	return embedder
}

// syncAudit 是线程安全的伪审计：httptest 处理器在另一个 goroutine 里读它，
// 直接复用 `fakeAudit` 会被 `-race` 抓（无锁计数器）。
// syncAudit is a race-safe audit fake: the httptest handler reads it concurrently.
type syncAudit struct {
	mu       sync.Mutex
	beginErr error
	begins   int
	finishes int
	observed []string
	inputs   [][]string
}

func (a *syncAudit) BeginEmbedding(_ context.Context, _, _ string, _ int) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.begins++
	if a.beginErr != nil {
		return "", a.beginErr
	}
	return fmt.Sprintf("call_%d", a.begins), nil
}

func (a *syncAudit) FinishEmbedding(_ context.Context, _ string, _ int64, _ error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.finishes++
	return nil
}

// note 在请求到达时记下 (预写数/终态数) 与输入；只有处理器 goroutine 调它。
// note records the audit state at request arrival; only the handler calls it.
func (a *syncAudit) note(req embeddingRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observed = append(a.observed, fmt.Sprintf("%d/%d", a.begins, a.finishes))
	a.inputs = append(a.inputs, append([]string(nil), req.Input...))
}

func (a *syncAudit) results() ([]string, [][]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	observed := append([]string(nil), a.observed...)
	inputs := make([][]string, len(a.inputs))
	for idx, input := range a.inputs {
		inputs[idx] = append([]string(nil), input...)
	}
	return observed, inputs
}

func (a *syncAudit) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.begins, a.finishes
}

// buildIngestor 用夹具端口 + 给定嵌入器 / 审计装配摄取器：harness 的 `build`
// 把嵌入器固定成 `*fakeEmbedder`，真实客户端用例在这里手工装配。
// buildIngestor assembles an ingestor with the given real client and audit.
func buildIngestor(t *testing.T, h *ingestHarness, embedder Embedder, audit EmbeddingAudit) *Ingestor {
	t.Helper()
	return NewIngestor(IngestorDeps{
		Store:    h.store,
		Reader:   h.reader,
		Chunker:  h.chunker,
		Embedder: embedder,
		Vectors:  h.vectors,
		Audit:    audit,
		Quota:    h.quota,
		Config:   h.cfg,
		Logger:   testLogger(t),
		Now:      func() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() },
		NewID:    func(prefix string) (string, error) { return prefix + "fixed", nil },
	})
}

// TestEmbeddingAudit_PendingBeforeCall 守 C-29 的落库顺序：每批**先**写 `pending`
// 审计、**再**发调用——第一批到达时是 (1 预写 / 0 终态)，第二批是 (2 / 1)。
// 逆改点：把 `BeginEmbedding` 挪到 `Embed` 之后，观察到的是 (0/0) 与 (0/1)。
// TestEmbeddingAudit_PendingBeforeCall pins the pending write before the call.
func TestEmbeddingAudit_PendingBeforeCall(t *testing.T) {
	h := newHarness(t)
	audit := &syncAudit{}
	srv := newEmbeddingServer(t, embeddingServerOptions{
		promptTokens: 7,
		onRequest: func(req embeddingRequest, header http.Header) {
			assert.Equal(t, "Bearer key_fixture", header.Get("Authorization"))
			assert.Equal(t, "application/json", header.Get("Content-Type"))
			assert.Equal(t, "text-embedding-v4", req.Model)
			assert.Equal(t, VectorDimensions, req.Dimensions)
			assert.Equal(t, "float", req.EncodingFormat)
			audit.note(req)
		},
	})

	result, err := buildIngestor(t, h, mustEmbedder(t, srv.URL), audit).Ingest(t.Context(), h.request())
	require.NoError(t, err)
	require.Equal(t, VersionActive, result.Status)
	require.Empty(t, result.ErrorKind)

	observed, inputs := audit.results()
	assert.Equal(t, []string{"1/0", "2/1"}, observed)
	assert.Equal(t, [][]string{
		{"第一段正文", "A > B\n第二段正文"},
		{"第三段正文"},
	}, inputs)
	begins, finishes := audit.counts()
	assert.Equal(t, 2, begins)
	assert.Equal(t, 2, finishes)
	assert.Equal(t, []string{"ver_1"}, h.store.activated)

	t.Run("预写失败不发请求", func(t *testing.T) {
		h := newHarness(t)
		var requests atomic.Int32
		srv := newEmbeddingServer(t, embeddingServerOptions{
			promptTokens: 7,
			onRequest:    func(embeddingRequest, http.Header) { requests.Add(1) },
		})
		audit := &syncAudit{beginErr: errors.New("audit store down")}

		result, err := buildIngestor(t, h, mustEmbedder(t, srv.URL), audit).Ingest(t.Context(), h.request())
		require.NoError(t, err)

		assert.Equal(t, VersionFailed, result.Status)
		assert.Equal(t, ErrorKindEmbedFailed, result.ErrorKind)
		assert.Zero(t, requests.Load(), "预写失败后不得发出调用")
		assert.Empty(t, h.vectors.writes)
		assert.Empty(t, h.store.activated)
		assert.Equal(t, []QuotaReleaseReason{QuotaReleaseAuditFailed}, h.quota.release)
	})
}

// TestEmbedding_RejectsDimensionMismatch 守"维度不符在**校验**阶段失败"：提供方回
// 3 维向量，配置要求 512——版本失败记 `verify_failed`、零向量写入、零激活，且
// 第一批失败后不再发第二批。
// 逆改点：客户端把向量截断 / 补齐到请求维度，或把维度校验搬进客户端后放行，
// 版本会变成 active 且 `vectors.writes` 非空。
// TestEmbedding_RejectsDimensionMismatch keeps width checks in the verify stage.
func TestEmbedding_RejectsDimensionMismatch(t *testing.T) {
	h := newHarness(t)
	var requests atomic.Int32
	srv := newEmbeddingServer(t, embeddingServerOptions{
		width:        3,
		promptTokens: 7,
		onRequest:    func(embeddingRequest, http.Header) { requests.Add(1) },
	})

	result, err := buildIngestor(t, h, mustEmbedder(t, srv.URL), h.audit).Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionFailed, result.Status)
	assert.Equal(t, ErrorKindVerifyFailed, result.ErrorKind)
	assert.Equal(t, int32(1), requests.Load(), "第一批校验失败后不得继续")
	assert.Empty(t, h.vectors.writes)
	assert.Empty(t, h.store.activated)
	assert.Equal(t, []string{ErrorKindVerifyFailed}, h.store.failReasons)
	assert.Equal(t, "embed/failed/verify_failed", h.store.jobOutcomes[0])
}

// TestHTTPEmbedder_OrdersVectorsByIndex 守按 `index` 归位：提供方倒序返回时，
// 客户端仍按声明的序号还原输入顺序；全部无序号时按数组序；部分缺号 / 重复号
// 一律报错而不是猜一个顺序。
// TestHTTPEmbedder_OrdersVectorsByIndex pins index-based reordering.
func TestHTTPEmbedder_OrdersVectorsByIndex(t *testing.T) {
	t.Run("倒序回执按序号归位", func(t *testing.T) {
		srv := newEmbeddingServer(t, embeddingServerOptions{promptTokens: 11, reverse: true})
		batch, err := mustEmbedder(t, srv.URL).Embed(t.Context(), []string{"甲", "乙", "丙"})
		require.NoError(t, err)

		require.Len(t, batch.Vectors, 3)
		for idx, vector := range batch.Vectors {
			require.Len(t, vector, VectorDimensions)
			assert.Equal(t, float32(idx+1), vector[0], "第 %d 条向量", idx+1)
		}
		assert.Equal(t, int64(11), batch.PromptTokens)
		assert.Equal(t, "text-embedding-v4", batch.Model)
	})

	t.Run("不带序号按数组序", func(t *testing.T) {
		srv := newEmbeddingServer(t, embeddingServerOptions{omitIndex: true})
		batch, err := mustEmbedder(t, srv.URL).Embed(t.Context(), []string{"甲", "乙", "丙"})
		require.NoError(t, err)

		require.Len(t, batch.Vectors, 3)
		for idx, vector := range batch.Vectors {
			assert.Equal(t, float32(idx+1), vector[0], "第 %d 条向量", idx+1)
		}
	})

	broken := map[string]string{
		"部分缺号": `{"data":[{"index":0,"embedding":[1]},{"embedding":[2]}]}`,
		"序号重复": `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`,
		"序号越界": `{"data":[{"index":5,"embedding":[1]}]}`,
	}
	for name, body := range broken {
		t.Run(name, func(t *testing.T) {
			srv := newEmbeddingServer(t, embeddingServerOptions{rawBody: body})
			_, err := mustEmbedder(t, srv.URL).Embed(t.Context(), []string{"甲", "乙", "丙"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "无法按序号对齐")
		})
	}
}

// TestHTTPEmbedder_RedactsKeyAndCapsErrorText 守出口脱敏：提供方把 Key 回显在
// 错误文案里，出口必须是 `[REDACTED]`；超长文案截到 4 KB 且不劈开多字节字符。
// 逆改点：去掉 `sanitizeEmbeddingText` 后 `err.Error()` 含夹具 Key。
// TestHTTPEmbedder_RedactsKeyAndCapsErrorText keeps echoed credentials out.
func TestHTTPEmbedder_RedactsKeyAndCapsErrorText(t *testing.T) {
	const apiKey = "sk-fixture-secret-value"
	newClient := func(t *testing.T, srv *httptest.Server) *HTTPEmbedder {
		t.Helper()
		embedder, err := NewHTTPEmbedder(EmbedderOptions{
			Name:       "fixture",
			BaseURL:    srv.URL,
			APIKey:     apiKey,
			Model:      "text-embedding-v4",
			Dimensions: VectorDimensions,
		})
		require.NoError(t, err)
		return embedder
	}

	t.Run("抹除 Key 回显", func(t *testing.T) {
		srv := newEmbeddingServer(t, embeddingServerOptions{
			status:       http.StatusUnauthorized,
			errorMessage: "invalid api key " + apiKey,
		})
		_, err := newClient(t, srv).Embed(t.Context(), []string{"甲"})
		require.Error(t, err)

		var embedErr *EmbeddingError
		require.ErrorAs(t, err, &embedErr)
		assert.Equal(t, http.StatusUnauthorized, embedErr.Status)
		assert.Equal(t, "fixture", embedErr.Provider)
		assert.NotContains(t, err.Error(), apiKey)
		assert.Contains(t, embedErr.Message, "[REDACTED]")
	})

	t.Run("超长文案截断", func(t *testing.T) {
		srv := newEmbeddingServer(t, embeddingServerOptions{
			status:       http.StatusBadGateway,
			errorMessage: strings.Repeat("长文案", 2000),
		})
		_, err := newClient(t, srv).Embed(t.Context(), []string{"甲"})
		require.Error(t, err)

		var embedErr *EmbeddingError
		require.ErrorAs(t, err, &embedErr)
		assert.LessOrEqual(t, len(embedErr.Message), maxEmbeddingErrorBytes)
		assert.True(t, utf8.ValidString(embedErr.Message), "截断不得劈开多字节字符")
	})
}

// TestHTTPEmbedder_RejectsMalformedResponse 守响应解析的失败面：非 JSON、字段
// 类型不符都报"响应不可解析"；能解析但序号不可归位报"无法按序号对齐"（后者在
// `TestHTTPEmbedder_OrdersVectorsByIndex` 里按子用例覆盖）。
// TestHTTPEmbedder_RejectsMalformedResponse rejects undecodable replies.
func TestHTTPEmbedder_RejectsMalformedResponse(t *testing.T) {
	cases := map[string]string{
		"非 JSON 正文":       `not-json`,
		"data 不是数组":       `{"data":"oops"}`,
		"embedding 混入字符串": `{"data":[{"index":0,"embedding":"x"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newEmbeddingServer(t, embeddingServerOptions{rawBody: body})
			_, err := mustEmbedder(t, srv.URL).Embed(t.Context(), []string{"甲"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "响应不可解析")
		})
	}
}

// TestHTTPEmbedder_MissingUsageLeavesTokensUnknown 守"结果未知不是零"：usage 对象
// 缺失或计数为 0 时，回执的 `PromptTokens` 是 0，预留由摄取侧保持 pending。
// TestHTTPEmbedder_MissingUsageLeavesTokensUnknown never invents usage.
func TestHTTPEmbedder_MissingUsageLeavesTokensUnknown(t *testing.T) {
	t.Run("缺 usage 对象", func(t *testing.T) {
		srv := newEmbeddingServer(t, embeddingServerOptions{omitUsage: true})
		batch, err := mustEmbedder(t, srv.URL).Embed(t.Context(), []string{"甲"})
		require.NoError(t, err)
		assert.Zero(t, batch.PromptTokens)
	})

	t.Run("usage 计数为零", func(t *testing.T) {
		srv := newEmbeddingServer(t, embeddingServerOptions{promptTokens: 0})
		batch, err := mustEmbedder(t, srv.URL).Embed(t.Context(), []string{"甲"})
		require.NoError(t, err)
		assert.Zero(t, batch.PromptTokens)
	})

	t.Run("回带提供方模型名", func(t *testing.T) {
		srv := newEmbeddingServer(t, embeddingServerOptions{model: "text-embedding-v4-2026"})
		batch, err := mustEmbedder(t, srv.URL).Embed(t.Context(), []string{"甲"})
		require.NoError(t, err)
		assert.Equal(t, "text-embedding-v4-2026", batch.Model)
	})
}

// TestHTTPEmbedder_RejectsEmptyInput 守空输入与已取消的上下文都不发出请求。
// TestHTTPEmbedder_RejectsEmptyInput never calls the provider for empty input.
func TestHTTPEmbedder_RejectsEmptyInput(t *testing.T) {
	var requests atomic.Int32
	srv := newEmbeddingServer(t, embeddingServerOptions{
		onRequest: func(embeddingRequest, http.Header) { requests.Add(1) },
	})
	embedder := mustEmbedder(t, srv.URL)

	_, err := embedder.Embed(t.Context(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "输入为空")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = embedder.Embed(ctx, []string{"甲"})
	require.ErrorIs(t, err, context.Canceled)

	assert.Zero(t, requests.Load())
}

// TestHTTPEmbedder_RejectsInvalidOptions 守构造期防御：非法 base_url / 缺模型 /
// 非正维度一律拒绝，错误文案**不得**回显 URL 里的凭证段。
// TestHTTPEmbedder_RejectsInvalidOptions fails closed at construction time.
func TestHTTPEmbedder_RejectsInvalidOptions(t *testing.T) {
	valid := EmbedderOptions{
		Name:       "fixture",
		BaseURL:    "https://api.example.com/v1",
		APIKey:     "sk-fixture",
		Model:      "text-embedding-v4",
		Dimensions: VectorDimensions,
	}
	baseURLs := map[string]string{
		"空地址":        "",
		"缺 scheme":   "api.example.com/v1",
		"非 http(s)":  "ftp://api.example.com/v1",
		"带 userinfo": "https://user:secret-value@api.example.com/v1",
		"带 query":    "https://api.example.com/v1?key=secret-value",
		"带 fragment": "https://api.example.com/v1#secret-value",
		"字面井号收尾":     "https://api.example.com/v1#",
	}
	for name, baseURL := range baseURLs {
		t.Run(name, func(t *testing.T) {
			opts := valid
			opts.BaseURL = baseURL
			_, err := NewHTTPEmbedder(opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "base_url")
			assert.NotContains(t, err.Error(), "secret-value")
		})
	}

	t.Run("缺模型", func(t *testing.T) {
		opts := valid
		opts.Model = "  "
		_, err := NewHTTPEmbedder(opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "模型")
	})

	t.Run("维度非正", func(t *testing.T) {
		opts := valid
		opts.Dimensions = 0
		_, err := NewHTTPEmbedder(opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "维度")
	})

	t.Run("接受带路径的地址", func(t *testing.T) {
		srv := newEmbeddingServer(t, embeddingServerOptions{})
		opts := valid
		opts.BaseURL = srv.URL + "/"
		embedder, err := NewHTTPEmbedder(opts)
		require.NoError(t, err)

		batch, err := embedder.Embed(t.Context(), []string{"甲"})
		require.NoError(t, err)
		assert.Len(t, batch.Vectors, 1)
	})
}

// TestHTTPEmbedder_TimeoutStopsCall 守截止时间经 ctx 生效：提供方不回响应时，调用
// 在配置超时内返回取消错误，而不是无限等待。
// TestHTTPEmbedder_TimeoutStopsCall bounds one call through the context.
func TestHTTPEmbedder_TimeoutStopsCall(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	embedder, err := NewHTTPEmbedder(EmbedderOptions{
		Name:       "fixture",
		BaseURL:    srv.URL,
		APIKey:     "key_fixture",
		Model:      "text-embedding-v4",
		Dimensions: VectorDimensions,
		Timeout:    100 * time.Millisecond,
	})
	require.NoError(t, err)

	_, err = embedder.Embed(t.Context(), []string{"甲"})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	var embedErr *EmbeddingError
	require.ErrorAs(t, err, &embedErr)
	assert.Contains(t, embedErr.Message, "截止时间")
	select {
	case <-started:
	default:
		t.Fatal("请求没有到达假提供方")
	}
}
