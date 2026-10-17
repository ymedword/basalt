package provider

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

	openai "github.com/sashabaranov/go-openai"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件实现单个 OpenAI 兼容供应商的**一次调用**：协议翻译（出站）→ 响应解析（入站）→
// 错误归类与脱敏（出口）。
// This file implements one call against a single OpenAI-compatible provider: protocol
// translation on the way out, response parsing on the way back, and error
// classification plus redaction at the boundary.
//
// 分工（约束 2 / 任务书裁决注 7）：`go-openai` 只出**协议类型与已知字段的编解码**
// （请求信封基座、工具调用）；HTTP 往返由本文件自建（`net/http` + `encoding/json`）——
// 一次 POST、不重试、不跟随重定向。库的消息编解码只认已知键，未知字段进出都会被丢，
// 故消息对象的平铺与收集由本文件自己做（协议往返保真，技术方案 §2.3 约束 3）。
// go-openai supplies protocol types and known-field codecs only; the HTTP round trip is
// built here because the library's message codec drops unknown keys in both directions.

// maxMessageBytes 是出口错误文案的长度上界（契约 §7.4 口径：4 KB，UTF-8 安全）。
// maxMessageBytes caps outbound error text at 4 KB without splitting a rune.
const maxMessageBytes = 4 * 1024

// maxResponseBodyBytes 是响应体的**读取上限**（成功体与错误体走同一条读路径，同判）。
// 4 KB 是**读完之后**对文案的截断，管不住读取过程本身的内存占用——上限必须落在读之前。
// 超限即安全失败：文案只说事实与上限，正文一个字不外带（契约 §7.4 的口径不变）。
// maxResponseBodyBytes bounds how much of a response body is read at all: the 4 KB
// text truncation happens after reading and cannot bound memory during the read.
const maxResponseBodyBytes = 4 << 20

// redactedKey 是抹除凭证回显后的占位符——留个痕迹比留空白更容易看出"这里被处理过"。
// redactedKey marks where an echoed credential was removed.
const redactedKey = "[REDACTED]"

// Options 是单个供应商客户端的构造入参——逐条来自配置（app 装配处），不来自遍历推断。
// Options carries one provider's construction inputs, taken entry by entry from config.
type Options struct {
	// Name 是映射表的键，也用于错误文案点名供应商。
	// Name keys the routing map and names the provider in error text.
	Name string

	// BaseURL 是 OpenAI 兼容端点：只接受 scheme + host + path（空 / 非法 / 含
	// userinfo / query / fragment → 构造失败）；文案不回显 URL 与其中的值。
	// BaseURL accepts scheme + host + path only; the constructor re-checks the rule
	// the loader already enforces, without echoing the URL or its values.
	BaseURL string

	// APIKey 是**已解析**的真实值：只进客户端，不进日志 / 错误 / 审计。
	// APIKey is the resolved real value: it never reaches logs, errors or audit rows.
	APIKey string
}

// OpenAIClient 是 OpenAI 兼容协议的实现：自建薄传输（一次 POST、不重试、不跟随重定向），
// 未知字段两端保真；只做协议调用与响应解析，不接管循环（宪法 I / II）。
// OpenAIClient does protocol translation and response parsing only: one POST, no retry,
// no redirect following, and unknown fields preserved in both directions.
type OpenAIClient struct {
	name     string
	endpoint string // 已含 /chat/completions；base_url 的 scheme + host + path 原样保留
	apiKey   string
	http     *http.Client // 不设超时：截止时间由调用方 ctx 承载（宪法 VII）
}

// 编译期断言：客户端满足契约层声明的调用面。
// Compile-time assertion that the client satisfies the contract-layer interface.
var _ core.ProviderClient = (*OpenAIClient)(nil)

// NewOpenAIClient 构造客户端；地址不合规时失败并点名供应商与不支持的成分。
// NewOpenAIClient builds the client; an unsupported base_url fails the construction.
func NewOpenAIClient(opts Options) (*OpenAIClient, error) {
	endpoint, err := parseBaseURL(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", opts.Name, err)
	}
	return &OpenAIClient{
		name:     opts.Name,
		endpoint: endpoint,
		apiKey:   opts.APIKey,
		http: &http.Client{
			// 重定向**不得**跟随（约束 2）：跟随会把凭证转发到另一个主机，也把一次调用
			// 变成两次请求；3xx 原样返回给上层记 external_error。
			// Redirects are never followed: the credential must not travel to another
			// host, and one call must remain one request.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			// Timeout 留零值——超时语义由调用方 ctx 承载（宪法 VII）。
		},
	}, nil
}

// Chat 发出**一次**调用：构造请求体 → 一次 POST → 解析响应；一切失败经 CallError 出口。
// Chat performs exactly one call: build the body, POST once, parse the response. Every
// failure leaves through a CallError.
func (c *OpenAIClient) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	body, err := c.buildRequestBody(req.Model, req.Messages, req.Tools)
	if err != nil {
		// 请求体组装失败是**本仓**的问题（不是供应商的），故归 unknown 而不是 external_error。
		// A body assembly failure is ours, not the provider's.
		return core.ChatResponse{}, c.callError(0, core.ErrorKindUnknown, "构造请求体失败", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return core.ChatResponse{}, c.callError(0, core.ErrorKindExternalError, "构造 HTTP 请求失败", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// 取消 / 截止归并记 timeout（错误链经 Unwrap 保留，errors.Is 仍可判）；
		// 其余传输层失败（DNS / 连接 / TLS）归 external_error。
		// Cancellation and deadlines fold into timeout with the chain preserved; other
		// transport failures are external errors.
		if isContextError(err) {
			return core.ChatResponse{}, &CallError{
				Provider: c.name, Kind: core.ErrorKindTimeout,
				Message: sanitizeMessage("调用被取消或超过截止时间（未取得响应）", c.apiKey), err: err,
			}
		}
		return core.ChatResponse{}, &CallError{
			Provider: c.name, Kind: core.ErrorKindExternalError,
			Message: sanitizeMessage(transportReason(err), c.apiKey), err: err,
		}
	}

	// 上限落在读之前：`io.ReadAll` 无条件读到 EOF，会让失控响应按供应商给的体量吃内存。
	// The cap sits before the read: an unbounded read would let a runaway response size
	// the client's memory.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		// 读取阶段同样要分两类：**响应头已返回**后因取消 / 截止中断 → timeout（与传输阶段
		// 同一归并口径，错误链区分保留）；其余读取失败（对端半途断开等）仍是 external_error。
		// Cancellation during the read phase folds into timeout exactly as it does in the
		// transport phase; every other read failure stays an external error.
		if isContextError(readErr) || ctx.Err() != nil {
			cause := readErr
			if !isContextError(readErr) && ctx.Err() != nil {
				// 罕见形态：取消与对端断开同时发生而读错误链上没带 ctx 错误——补进链，
				// 「Canceled / DeadlineExceeded 错误链区分保留」（FR-008）不因阶段而失效。
				cause = errors.Join(readErr, ctx.Err())
			}
			return core.ChatResponse{}, &CallError{
				Provider: c.name, Status: resp.StatusCode, Kind: core.ErrorKindTimeout,
				Message: sanitizeMessage("调用被取消或超过截止时间（未取完响应）", c.apiKey), err: cause,
			}
		}
		return core.ChatResponse{}, c.callError(resp.StatusCode, core.ErrorKindExternalError, "读取响应体失败", readErr)
	}
	if closeErr != nil {
		// 不静默丢弃：已经读完的响应体不受影响，但连接状态可疑，如实报失败。
		// Not swallowed: the body was read, but the connection state is suspect.
		return core.ChatResponse{}, c.callError(resp.StatusCode, core.ErrorKindExternalError, "关闭响应体失败", closeErr)
	}
	if len(raw) > maxResponseBodyBytes {
		// 超限：只说事实与上限，正文一个字不外带（也**不**继续排空——排空等于把上限绕回来）。
		// Over the cap: report the fact and the cap, never the body, and abandon the read.
		return core.ChatResponse{}, c.callError(resp.StatusCode, core.ErrorKindExternalError,
			fmt.Sprintf("响应体超过读取上限（%d 字节）", maxResponseBodyBytes), nil)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return core.ChatResponse{}, c.statusError(resp.StatusCode, raw)
	}

	parsed, err := parseChatResponse(raw)
	if err != nil {
		return core.ChatResponse{}, c.callError(resp.StatusCode, core.ErrorKindExternalError, "响应不可解析", err)
	}
	return parsed, nil
}

// callError 是出口错误的**单点**：一切文案先过脱敏（抹 key 回显 + 4 KB UTF-8 安全截断），
// 原始链经 Unwrap 保留。集中在这里是为了让"忘了脱敏"这件事没有分支可漏。
// callError is the single exit for outbound errors: every message is redacted and
// truncated here, with the original chain preserved through Unwrap.
func (c *OpenAIClient) callError(status int, kind core.ErrorKind, message string, cause error) *CallError {
	return &CallError{
		Provider: c.name,
		Status:   status,
		Kind:     kind,
		Message:  sanitizeMessage(message, c.apiKey),
		err:      cause,
	}
}

// statusError 把非 2xx 响应转成错误：文案只取供应商自己的 `error.message`——响应正文
// 全文不外带（可能是巨幅 HTML 或含敏感上下文）。
// statusError keeps only the provider's own error message; the full body never leaves.
func (c *OpenAIClient) statusError(status int, body []byte) *CallError {
	message := "供应商未提供错误说明"
	var envelope openai.ErrorResponse
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	return c.callError(status, core.ErrorKindExternalError, message, nil)
}

// buildRequestBody 组装出站信封：以库的请求类型为**已知字段基座**（model / stream 等），
// 再把 `messages` **显式覆盖**为我们自己编好的数组——不依赖 Go 结构体的字段仲裁，
// 出站体里 `"messages"` 只此一份（判据见 TestOpenAIClient_RequestEnvelopeKeepsSingleMessagesKey）。
// `tools` 同理：**空池不带该键**（旧行为逐字不变——工具池为空时"没有工具"与"没传工具"
// 对供应商是同一件事，改出站形态会让所有既有请求体变样）。
// buildRequestBody uses the library request as the known-field base and then explicitly
// overrides the messages key, so the envelope carries exactly one such key. The tools
// key appears only when the pool is non-empty, keeping the empty-pool body byte-identical
// to what this client has always sent.
func (c *OpenAIClient) buildRequestBody(model string, msgs []core.Message, tools []core.ToolSpec) ([]byte, error) {
	base, err := json.Marshal(openai.ChatCompletionRequest{Model: model})
	if err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(base, &envelope); err != nil {
		return nil, err
	}

	messages, err := encodeMessages(msgs)
	if err != nil {
		return nil, err
	}
	rawMessages, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	envelope["messages"] = rawMessages

	if len(tools) > 0 {
		rawTools, err := json.Marshal(encodeTools(tools))
		if err != nil {
			return nil, err
		}
		envelope["tools"] = rawTools
	}

	return json.Marshal(envelope)
}

// encodeTools 把工具定义编成 Function Calling 形态（协议形状 `[{"type":"function",
// "function":{...}}]`；库的 `Tool.MarshalJSON` 走 map 序列化，出站体的键序是
// `function` 在前、`type` 在后——同形不同序，判据看**解码后的形状**与 schema 的逐字字节）；
// `Parameters` 是工具自己的 JSON Schema，**原样透传**——解析后重序列化会改键序与数字形状
// （与 ToolCall.Arguments 同一条纪律）。
// encodeTools renders the Function Calling shape; the library serializes the object
// through a map, so key order is function-then-type, and the schema passes through
// untouched byte for byte.
func encodeTools(specs []core.ToolSpec) []openai.Tool {
	encoded := make([]openai.Tool, 0, len(specs))
	for _, spec := range specs {
		encoded = append(encoded, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  spec.Parameters,
			},
		})
	}
	return encoded
}

// encodeMessages 把消息模型编成协议对象：**已知字段 + 透传位平铺**（未知键以原名原值回到
// 出站体——两跳保真的第一跳）。返回类型带 error 是本 US 定的签名（协议校验属 US-4 的工具
// 字段扩展；当前没有失败分支，避免为凑签名编造校验）。
// encodeMessages flattens known fields together with the pass-through extras so that
// unknown keys travel back out under their original names.
func encodeMessages(msgs []core.Message) ([]map[string]any, error) {
	encoded := make([]map[string]any, 0, len(msgs))
	for _, msg := range msgs {
		obj := make(map[string]any, len(msg.ProviderExtra)+4)
		// 先铺透传位、后写已知字段：已知字段是权威形态，不得被供应商回显的键顶掉。
		// Extras first, known fields second: the known shape wins.
		for key, value := range msg.ProviderExtra {
			obj[key] = value
		}
		obj["role"] = msg.Role
		obj["content"] = msg.Content
		if len(msg.ToolCalls) > 0 {
			obj["tool_calls"] = encodeToolCalls(msg.ToolCalls)
		}
		if msg.ToolCallID != "" {
			obj["tool_call_id"] = msg.ToolCallID
		}
		if msg.ReasoningContent != "" {
			obj["reasoning_content"] = msg.ReasoningContent
		}
		encoded = append(encoded, obj)
	}
	return encoded, nil
}

// encodeToolCalls 用库的工具调用类型编码（协议类型归库、组装归我们）。
// encodeToolCalls encodes tool calls through the library's protocol type.
func encodeToolCalls(calls []core.ToolCall) []openai.ToolCall {
	encoded := make([]openai.ToolCall, 0, len(calls))
	for _, call := range calls {
		encoded = append(encoded, openai.ToolCall{
			ID:   call.ID,
			Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      call.Name,
				Arguments: call.Arguments,
			},
		})
	}
	return encoded
}

// parseChatResponse 解析响应：已知键进显式列，**未知键收进 ProviderExtra**（两跳保真的
// 第二跳必须落在这里，因为库的消息解码会把未知键丢掉）。
// parseChatResponse routes known keys into explicit fields and collects everything else
// into ProviderExtra, which is where pass-through fidelity is actually decided.
func parseChatResponse(body []byte) (core.ChatResponse, error) {
	var envelope struct {
		Choices []struct {
			Message map[string]json.RawMessage `json:"message"`
		} `json:"choices"`
		// usage 走 SDK 的已知字段编解码（裁决注 7）：`core.Usage` 是**契约层的纯声明**，
		// 不带 JSON 标签（契约层不夹带编解码），直接解它会得到三列全零。
		// Usage is decoded through the SDK type: the contract-layer type carries no
		// JSON tags by design, so decoding into it would silently yield zeros.
		Usage openai.Usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return core.ChatResponse{}, fmt.Errorf("响应不是合法 JSON: %w", err)
	}
	if len(envelope.Choices) == 0 {
		// 200 但没有 choices：不是"空回答"，是异常响应——静默返回空串会被误当成正常结果。
		// A 200 with no choices is an anomalous response, not an empty answer.
		return core.ChatResponse{}, errors.New("响应中没有 choices")
	}
	if envelope.Choices[0].Message == nil {
		// message 缺失或在 JSON 里是 `null`（`{"choices":[{}]}` 这类）：解出来的零值响应会被
		// 服务当成功记进审计——坏响应必须在客户端边界判死。`{}`（空对象）不拒：协议允许
		// content 为 null，工具调用响应正是这个形态（FR-006）。
		// A missing or null message is an anomalous response; an empty object stays legal
		// because tool-call responses look exactly like that.
		return core.ChatResponse{}, errors.New("响应中首条 choice 缺少 message 对象")
	}

	out := core.ChatResponse{Usage: core.Usage{
		PromptTokens:     envelope.Usage.PromptTokens,
		CompletionTokens: envelope.Usage.CompletionTokens,
		TotalTokens:      envelope.Usage.TotalTokens,
	}}
	extras := map[string]any{}
	for key, raw := range envelope.Choices[0].Message {
		switch key {
		case "role":
			// 响应的 role 不进 core.ChatResponse：会话历史里的角色由调用方组装（US-4）。
			// The response role is not part of the response model.
		case "content":
			if err := decodeNullableString(raw, &out.Content); err != nil {
				return core.ChatResponse{}, err
			}
		case "reasoning_content":
			if err := decodeNullableString(raw, &out.ReasoningContent); err != nil {
				return core.ChatResponse{}, err
			}
		case "tool_calls":
			calls, err := decodeToolCalls(raw)
			if err != nil {
				return core.ChatResponse{}, err
			}
			out.ToolCalls = calls
		default:
			// UseNumber：默认解码把数字变成 float64，2^53 以上的整数会**悄悄改值**，
			// 两跳之后供应商收到的是另一个数；json.Number 原样保留字面量（含嵌套结构）。
			// UseNumber keeps large integers exact across the two-hop round trip.
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			var value any
			if err := dec.Decode(&value); err != nil {
				return core.ChatResponse{}, err
			}
			extras[key] = value
		}
	}
	if len(extras) > 0 {
		out.ProviderExtra = extras
	}
	return out, nil
}

// decodeNullableString 解引号字符串；协议允许 `null`（带工具调用的响应 content 常为 null）——
// 归零值而不是判失败。
// decodeNullableString maps a JSON null to the zero value instead of failing the call.
func decodeNullableString(raw json.RawMessage, dst *string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// decodeToolCalls 解析工具调用：只透传（执行权归 US-4 的 ToolExecutor），故字段映射到
// core.ToolCall 即可，不做任何校验式拒绝。
// decodeToolCalls only passes tool calls through; execution belongs to US-4.
func decodeToolCalls(raw json.RawMessage) ([]core.ToolCall, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var decoded []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	calls := make([]core.ToolCall, 0, len(decoded))
	for _, call := range decoded {
		calls = append(calls, core.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		})
	}
	return calls, nil
}

// sanitizeMessage 抹除 key 回显并截断到 4 KB——**唯一**持有凭证的层负责把凭证挡在出口内。
// sanitizeMessage removes any echo of the credential and truncates to 4 KB: the only
// layer holding the key is the only layer that can redact it.
func sanitizeMessage(text, apiKey string) string {
	if apiKey != "" {
		text = strings.ReplaceAll(text, apiKey, redactedKey)
	}
	if len(text) <= maxMessageBytes {
		return text
	}
	// 按 rune 边界回退，最多 3 步——切半个汉字会让库内出现非法 UTF-8，下游全线踩雷。
	// Back off to a rune boundary so truncation never splits a character.
	cut := maxMessageBytes
	for cut > 0 && text[cut]&0xC0 == 0x80 {
		cut--
	}
	return text[:cut]
}

// transportReason 给传输层失败一个**不含地址**的分类名：`url.Error` 自带完整 URL、
// `net.OpError` 自带 host:port，两者都不得外带（约束 8 只允许供应商名 / HTTP 状态 /
// 服务器给的错误说明）；分类名保留诊断价值，细节经 Unwrap 仍在错误链上。
// transportReason names the failure class without any address: raw transport errors
// carry the URL and the host:port, and neither may leave the boundary.
func transportReason(err error) string {
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

// isContextError 判定错误链上是否携带上下文取消 / 截止（归并口径：两类都记 timeout，
// 但链保留，调用方仍可用 errors.Is 区分）。
// isContextError reports whether the chain carries cancellation or a deadline.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// parseBaseURL 是构造期的**防御校验**：只放行 scheme（http / https）+ host + path 三部分，
// 原样保留（尾斜杠之外不做任何拼接改写）；userinfo / query / fragment / 缺 scheme / 非
// http(s) 一律拒绝。装载期已拦同一口径（§2.4 裁决注 9），此处再判一次——客户端可被直接
// 构造，不依赖上一层。
// parseBaseURL re-applies the same rule as the loader: scheme + host + path only,
// everything else rejected rather than silently trimmed.
func parseBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("base_url 为空——只支持 scheme + host + path")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		// 解析失败路径**不得**直传原错误：`url.Parse` 的错误文案自带整条 URL。
		// The parse error embeds the URL, so it is never forwarded.
		return "", unsupportedBaseURL("scheme")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		// 缺 scheme 不代补 https——不做隐式推断（与"不静默改配置"同一原则）。
		return "", unsupportedBaseURL("scheme")
	}
	if parsed.Opaque != "" || parsed.Host == "" {
		return "", unsupportedBaseURL("host")
	}
	if parsed.User != nil {
		return "", unsupportedBaseURL("userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", unsupportedBaseURL("query")
	}
	if parsed.Fragment != "" || strings.Contains(raw, "#") {
		// 尾随的空 `#`（`…/v1#`）解析后 Fragment 是**空串**，只看它会漏——而拼接端点后
		// `/chat/completions` 整段落进 fragment，请求实际打到 base path，等于静默改写端点。
		// `url.URL` 没有 ForceFragment 字段（query 侧才有 ForceQuery），故按字面 `#` 判；
		// 路径里的 `%23` 是合法编码，不含字面 `#`，不受影响。
		// A trailing empty `#` leaves Fragment empty while still swallowing the appended
		// endpoint into the fragment, so the literal delimiter is checked too.
		return "", unsupportedBaseURL("fragment")
	}
	// 只去掉一个尾斜杠（避免拼出 `//chat/completions`），其余部分原样保留。
	return strings.TrimSuffix(raw, "/") + "/chat/completions", nil
}

// unsupportedBaseURL 产出统一文案：点名键与不支持的成分，**不回显** URL 或其中的值。
// unsupportedBaseURL names the key and the offending component, never the values.
func unsupportedBaseURL(component string) error {
	return fmt.Errorf("base_url 只支持 scheme + host + path（不支持的成分: %s）", component)
}

// CallError 是客户端边界产出的**安全调用错误**：文案已脱敏（抹 key 回显 + 截断 4 KB、
// 不含请求 URL 与响应正文全文）、类别已判好、原始链经 Unwrap 保留——取消 / 截止用
// errors.Is(err, context.Canceled / DeadlineExceeded) 仍可判。
// CallError is the safe error produced at the client boundary: redacted text, decided
// kind, and the original chain preserved for errors.Is.
type CallError struct {
	Provider string         // 供应商名（点名用）
	Status   int            // HTTP 状态码；0 = 传输层失败（DNS / 连接 / TLS）
	Kind     core.ErrorKind // 客户端的判定面：external_error / timeout
	Message  string         // 已脱敏、已截断的文案
	err      error          // 原始错误链（ctx 错误经此保留）
}

// Error 渲染出口文案：状态码只在真的取到响应时出现。
// Error renders the boundary text; the status appears only when a response arrived.
func (e *CallError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("provider %q: HTTP %d: %s", e.Provider, e.Status, e.Message)
	}
	return fmt.Sprintf("provider %q: %s", e.Provider, e.Message)
}

// Unwrap 保留原始错误链（取消 / 截止的判定靠它）。
// Unwrap keeps the original chain, which is what errors.Is relies on.
func (e *CallError) Unwrap() error { return e.err }
