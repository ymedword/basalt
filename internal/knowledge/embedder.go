package knowledge

// 本文件实现 `Embedder` 的 HTTP 协议客户端（US-16 §3.1.3 / T036）：按**固定连接**
// 发一次 embedding 调用，不重试、不跟随重定向、不截断 / 不补齐向量。
//
// 三条边界：
//  1. **连接在构造期固定**：供应商名、base_url、Key、模型与维度都来自装配期解析好的
//     一条 `embedding.model_connection`（P-06 的 Provider 目录发布 / 冻结语义在装配层
//     落地）；本包不读目录、不 import `provider` / `app`——消费方声明端口，实现由
//     装配层注入（ports.go 头注释）。
//  2. **请求只按配置成形**：`{model, input, dimensions, encoding_format:"float"}` POST
//     到 `base_url + "/embeddings"`——与 D-05 实验脚本（`dev-env/scripts/
//     Test-BailianEmbedding.ps1`）真实验证过的请求形状逐字一致。
//  3. **响应原样交回**：按提供方声明的 `index` 排列向量；**维度与条数不在这里判**——
//     它们归摄取侧 `checkVectors`（`verify_failed`）。客户端若截断 / 补齐向量，
//     "维度不符"会静默通过并让错误版本生效（T036 `TestEmbedding_RejectsDimensionMismatch`
//     的逆改点）。
//
// This file implements the HTTP embedding client: one POST per call against the
// fixed connection resolved at assembly, with the response handed back untouched so
// count and width stay a verification-stage (not client-stage) judgement.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// maxEmbeddingResponseBytes 是响应体的**读取上限**（落在读之前，原因同 provider 客户端）：
// 一批 32 条 512 维向量约 160 KB，4 MiB 留足余量；超限即安全失败，正文一个字不外带。
// maxEmbeddingResponseBytes bounds the read itself; a batch of 32×512 floats is
// about 160 KB, so 4 MiB is generous headroom rather than a working limit.
const maxEmbeddingResponseBytes = 4 << 20

// maxEmbeddingErrorBytes 是出口错误文案的字节上界（4 KB，UTF-8 安全），与 provider
// 客户端同一口径——错误文案可能包含提供方回显的输入片段或凭证。
// maxEmbeddingErrorBytes caps outbound error text at 4 KB without splitting a rune.
const maxEmbeddingErrorBytes = 4 * 1024

// redactedEmbeddingKey 是抹除凭证回显后的占位符。
// redactedEmbeddingKey marks where an echoed credential was removed.
const redactedEmbeddingKey = "[REDACTED]"

// EmbedderOptions 是 embedding 客户端的构造入参，逐条来自装配期的固定连接——
// 不来自映射表遍历推断（宪法 III 的同一口径）。
// EmbedderOptions carries one frozen connection's construction inputs.
type EmbedderOptions struct {
	// Name 是供应商名，只用于错误文案点名。
	// Name names the provider in error text and nothing else.
	Name string
	// BaseURL 只接受 scheme + host + path（与 provider 客户端同一规则）；构造时补
	// `/embeddings`。
	// BaseURL accepts scheme + host + path only; `/embeddings` is appended.
	BaseURL string
	// APIKey 是**已解析**的真实值：只进客户端，不进日志 / 错误 / 审计。
	// APIKey is the resolved value; it never reaches logs, errors or audit rows.
	APIKey string
	// Model 是请求里的模型 id，必填（配置层已保证连接与模型同时给出）。
	// Model is the requested model id and is required.
	Model string
	// Dimensions 是请求里的向量维度，必须为正；**不**用于校验响应。
	// Dimensions shapes the request only; the response is never resized to match it.
	Dimensions int
	// Timeout 是单次调用的截止时间（配置 `embedding.timeout_seconds`）；为零表示
	// 由调用方 ctx 承载。截止时间经 ctx 生效，`http.Client` 不设 Timeout（宪法 VII）。
	// Timeout bounds one call through the context; zero leaves it to the caller.
	Timeout time.Duration
}

// HTTPEmbedder 是 OpenAI 兼容 `/embeddings` 端点的薄协议客户端。
// HTTPEmbedder is the thin protocol client for an OpenAI-compatible embeddings endpoint.
type HTTPEmbedder struct {
	name       string
	endpoint   string
	apiKey     string
	model      string
	dimensions int
	timeout    time.Duration
	http       *http.Client // 不设超时：截止时间由 ctx 承载（宪法 VII）
}

// 编译期断言：客户端满足摄取侧声明的端口。
// Compile-time assertion that the client satisfies the consumer-side port.
var _ Embedder = (*HTTPEmbedder)(nil)

// NewHTTPEmbedder 构造客户端；地址 / 模型 / 维度不合法时失败并点名供应商。
// 构造期**不**做网络探测——连通性由首次真实调用回答，构造成功不代表凭据有效。
// NewHTTPEmbedder builds the client; invalid inputs fail the construction, and no
// network probe is attempted—construction says nothing about credential validity.
func NewHTTPEmbedder(opts EmbedderOptions) (*HTTPEmbedder, error) {
	endpoint, err := parseEmbeddingBaseURL(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("knowledge: embedding provider %q: %w", opts.Name, err)
	}
	if strings.TrimSpace(opts.Model) == "" {
		return nil, fmt.Errorf("knowledge: embedding provider %q: 模型 id 为空", opts.Name)
	}
	if opts.Dimensions < 1 {
		return nil, fmt.Errorf("knowledge: embedding provider %q: 维度必须为正（当前 %d）", opts.Name, opts.Dimensions)
	}
	if opts.Timeout < 0 {
		return nil, fmt.Errorf("knowledge: embedding provider %q: 超时不得为负（当前 %s）", opts.Name, opts.Timeout)
	}
	return &HTTPEmbedder{
		name:       opts.Name,
		endpoint:   endpoint,
		apiKey:     opts.APIKey,
		model:      opts.Model,
		dimensions: opts.Dimensions,
		timeout:    opts.Timeout,
		http: &http.Client{
			// 重定向**不得**跟随：跟随即把凭证转发给另一个主机，也把一次调用变成两次请求。
			// Redirects are never followed, so the credential cannot travel to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// embeddingRequest 是与提供方约定的出站信封（D-05 实验脚本的请求形状）。
// embeddingRequest is the outbound envelope validated by the D-05 probe.
type embeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     int      `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

// embeddingResponse 只取本仓需要的字段；未知键不报错（提供方常加字段）。
// embeddingResponse keeps only the fields this repository consumes.
type embeddingResponse struct {
	Data  []embeddingEntry `json:"data"`
	Model string           `json:"model"`
	Usage *embeddingUsage  `json:"usage"`
}

// embeddingEntry 是一条回执向量；`index` 用指针区分"缺省"与"第 0 条"。
// embeddingEntry is one returned vector; a pointer keeps "absent" apart from zero.
type embeddingEntry struct {
	Index     *int      `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// embeddingUsage 是回执用量；整个对象缺失时 `PromptTokens` 保持 0（未知）。
// embeddingUsage carries the reported usage; a missing object leaves tokens unknown.
type embeddingUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
}

// Embed 发出**一次**调用并返回提供方回执：不重排（按 `index` 归位是协议要求，不是
// 重排）、不截断、不补默认值。空输入不发出请求。
// Embed makes exactly one call and returns the provider's reply as-is.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) (EmbedBatch, error) {
	if len(texts) == 0 {
		return EmbedBatch{}, fmt.Errorf("knowledge: embedding provider %q: 输入为空，未发出调用", e.name)
	}
	if err := ctx.Err(); err != nil {
		return EmbedBatch{}, err
	}

	body, err := json.Marshal(embeddingRequest{
		Model:          e.model,
		Input:          texts,
		Dimensions:     e.dimensions,
		EncodingFormat: "float",
	})
	if err != nil {
		// 请求体组装失败是**本仓**的问题，不是提供方的。
		// A body assembly failure is ours, not the provider's.
		return EmbedBatch{}, e.callError(0, "构造请求体失败", err)
	}

	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return EmbedBatch{}, e.callError(0, "构造 HTTP 请求失败", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.http.Do(httpReq)
	if err != nil {
		// 传输层错误自带 URL 与 host:port，**不得**原样外带；分类名进文案，原始链
		// 经 Unwrap 保留（取消 / 截止仍可 errors.Is 判定）。
		// Transport errors carry the URL and host:port, so only a class name leaves
		// the boundary; the original chain stays reachable through Unwrap.
		if isEmbeddingContextError(err) {
			return EmbedBatch{}, &EmbeddingError{
				Provider: e.name,
				Message:  sanitizeEmbeddingText("调用被取消或超过截止时间（未取得响应）", e.apiKey),
				err:      err,
			}
		}
		return EmbedBatch{}, &EmbeddingError{
			Provider: e.name,
			Message:  sanitizeEmbeddingText(embeddingTransportReason(err), e.apiKey),
			err:      err,
		}
	}

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxEmbeddingResponseBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		// 响应头已返回后因取消 / 截止中断 → 同一归并口径；其余读取失败仍是传输失败。
		// Cancellation during the read folds into the same class as in transport.
		if isEmbeddingContextError(readErr) || ctx.Err() != nil {
			cause := readErr
			if !isEmbeddingContextError(readErr) && ctx.Err() != nil {
				cause = errors.Join(readErr, ctx.Err())
			}
			return EmbedBatch{}, &EmbeddingError{
				Provider: e.name,
				Status:   resp.StatusCode,
				Message:  sanitizeEmbeddingText("调用被取消或超过截止时间（未取完响应）", e.apiKey),
				err:      cause,
			}
		}
		return EmbedBatch{}, e.callError(resp.StatusCode, "读取响应体失败", readErr)
	}
	if closeErr != nil {
		// 已读完的响应体不受影响，但连接状态可疑，如实报失败而不是静默丢弃。
		// The body was read, but the connection state is suspect, so this still fails.
		return EmbedBatch{}, e.callError(resp.StatusCode, "关闭响应体失败", closeErr)
	}
	if len(raw) > maxEmbeddingResponseBytes {
		// 超限：只说事实与上限，正文一个字不外带，也不继续排空（排空等于绕回上限）。
		// Over the cap: report the fact and the cap, never the body.
		return EmbedBatch{}, e.callError(resp.StatusCode,
			fmt.Sprintf("响应体超过读取上限（%d 字节）", maxEmbeddingResponseBytes), nil)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return EmbedBatch{}, e.statusError(resp.StatusCode, raw)
	}

	var parsed embeddingResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return EmbedBatch{}, e.callError(resp.StatusCode, "响应不可解析", err)
	}
	vectors, err := orderEmbeddingVectors(parsed.Data)
	if err != nil {
		// 序号无法归位是协议违约（不是维度 / 条数问题），交回错误而不是猜顺序。
		// Broken indexes are a protocol violation; guessing an order would be worse.
		return EmbedBatch{}, e.callError(resp.StatusCode, "响应向量无法按序号对齐", err)
	}

	batch := EmbedBatch{Vectors: vectors, Model: e.model}
	if parsed.Model != "" {
		batch.Model = parsed.Model
	}
	if parsed.Usage != nil {
		// 用量为 0（或对象缺失）保持"未知"：预留停在 pending，**不得**按零结算。
		// Zero or missing usage stays unknown so the reservation is never settled as zero.
		batch.PromptTokens = parsed.Usage.PromptTokens
	}
	return batch, nil
}

// orderEmbeddingVectors 按提供方声明的 `index` 把向量放回输入顺序。全部条目都缺
// `index` 时按数组顺序；只要有任意一条带 `index`，就要求全部带、且构成
// [0, n) 的排列——缺号 / 越界 / 重复一律报错，**不得**挑一个顺序凑合。
// orderEmbeddingVectors restores input order by declared index; a partial or broken
// index set is an error rather than a guess.
func orderEmbeddingVectors(entries []embeddingEntry) ([][]float32, error) {
	vectors := make([][]float32, len(entries))
	anyIndex := false
	for _, entry := range entries {
		if entry.Index != nil {
			anyIndex = true
			break
		}
	}
	if !anyIndex {
		for idx, entry := range entries {
			vectors[idx] = entry.Embedding
		}
		return vectors, nil
	}
	seen := make([]bool, len(entries))
	for _, entry := range entries {
		if entry.Index == nil {
			return nil, errors.New("部分向量带序号、部分缺失")
		}
		idx := *entry.Index
		if idx < 0 || idx >= len(entries) {
			return nil, fmt.Errorf("序号 %d 超出 [0, %d)", idx, len(entries))
		}
		if seen[idx] {
			return nil, fmt.Errorf("序号 %d 重复", idx)
		}
		seen[idx] = true
		vectors[idx] = entry.Embedding
	}
	return vectors, nil
}

// callError 是出口错误的**单点**：一切文案先过脱敏（抹 key 回显 + 4 KB UTF-8 安全
// 截断），原始链经 Unwrap 保留。集中在这里是为了让"忘了脱敏"没有分支可漏。
// callError is the single exit for outbound errors: every message is redacted here.
func (e *HTTPEmbedder) callError(status int, message string, cause error) *EmbeddingError {
	return &EmbeddingError{
		Provider: e.name,
		Status:   status,
		Message:  sanitizeEmbeddingText(message, e.apiKey),
		err:      cause,
	}
}

// statusError 解析提供方的错误信封（`error.message`）；解析不出时给固定文案，
// **不**外带响应正文。
// statusError extracts the provider's error message and never forwards the raw body.
func (e *HTTPEmbedder) statusError(status int, body []byte) *EmbeddingError {
	message := "提供方未给出错误说明"
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	return e.callError(status, message, nil)
}

// sanitizeEmbeddingText 抹除 key 回显并按字节上界截断——唯一持有凭证的层负责把
// 凭证挡在出口内（与 provider 客户端同一口径）。
// sanitizeEmbeddingText redacts the key and truncates at a rune boundary.
func sanitizeEmbeddingText(text, apiKey string) string {
	if apiKey != "" {
		text = strings.ReplaceAll(text, apiKey, redactedEmbeddingKey)
	}
	if len(text) <= maxEmbeddingErrorBytes {
		return text
	}
	cut := maxEmbeddingErrorBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// embeddingTransportReason 给传输层失败一个**不含地址**的分类名：`url.Error` 自带
// 完整 URL、`net.OpError` 自带 host:port，两者都不得外带。
// embeddingTransportReason names the failure class without any address.
func embeddingTransportReason(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS 解析失败（未取得响应）"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "连接被拒绝（未取得响应）"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "网络超时（未取得响应）"
	}
	return "网络传输失败（未取得响应）"
}

// isEmbeddingContextError 判定错误链上是否携带上下文取消 / 截止。
// isEmbeddingContextError reports cancellation or a deadline on the chain.
func isEmbeddingContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// parseEmbeddingBaseURL 是构造期的防御校验：只放行 scheme（http / https）+ host +
// path，其余成分一律拒绝而不是静默裁剪；成功后补 `/embeddings`。解析失败**不得**
// 直传原错误——`url.Parse` 的文案自带整条 URL。
// parseEmbeddingBaseURL admits scheme + host + path only, then appends the endpoint.
func parseEmbeddingBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("base_url 为空——只支持 scheme + host + path")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", unsupportedEmbeddingBaseURL("scheme")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		// 缺 scheme 不代补 https——不做隐式推断（与"不静默改配置"同一原则）。
		// A missing scheme is never filled in: implicit inference is not allowed.
		return "", unsupportedEmbeddingBaseURL("scheme")
	}
	if parsed.Opaque != "" || parsed.Host == "" {
		return "", unsupportedEmbeddingBaseURL("host")
	}
	if parsed.User != nil {
		return "", unsupportedEmbeddingBaseURL("userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", unsupportedEmbeddingBaseURL("query")
	}
	if parsed.Fragment != "" || strings.Contains(raw, "#") {
		// 尾随空 `#` 解析后 Fragment 是空串，但会把拼上的端点整段吞进 fragment；`url.URL`
		// 没有 ForceFragment，故按字面 `#` 判（`%23` 是合法编码，不含字面 `#`）。
		// A trailing empty `#` swallows the appended endpoint into the fragment.
		return "", unsupportedEmbeddingBaseURL("fragment")
	}
	// 只去掉一个尾斜杠（避免拼出 `//embeddings`），其余部分原样保留。
	return strings.TrimSuffix(raw, "/") + "/embeddings", nil
}

// unsupportedEmbeddingBaseURL 产出统一文案：点名不支持的成分，**不回显** URL 或其中的值。
// unsupportedEmbeddingBaseURL names the offending component, never the values.
func unsupportedEmbeddingBaseURL(component string) error {
	return fmt.Errorf("base_url 只支持 scheme + host + path（不支持的成分: %s）", component)
}

// EmbeddingError 是 embedding 客户端边界的**安全错误**：文案已脱敏（抹 key 回显 +
// 4 KB 截断、不含请求 URL 与响应正文全文），原始链经 Unwrap 保留——取消 / 截止用
// errors.Is(err, context.Canceled / DeadlineExceeded) 仍可判。
// EmbeddingError is the safe error produced at the client boundary.
type EmbeddingError struct {
	// Provider 是供应商名（点名用）。
	Provider string
	// Status 是 HTTP 状态码；0 = 传输层失败（DNS / 连接 / TLS）。
	Status int
	// Message 是已脱敏、已截断的文案。
	Message string
	err     error // 原始错误链（ctx 错误经此保留）
}

// Error 渲染出口文案：状态码只在真的取到响应时出现。
// Error renders the boundary text; the status appears only when a response arrived.
func (e *EmbeddingError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("knowledge: embedding provider %q: HTTP %d: %s", e.Provider, e.Status, e.Message)
	}
	return fmt.Sprintf("knowledge: embedding provider %q: %s", e.Provider, e.Message)
}

// Unwrap 保留原始错误链（取消 / 截止的判定靠它）。
// Unwrap keeps the original chain, which is what errors.Is relies on.
func (e *EmbeddingError) Unwrap() error { return e.err }
