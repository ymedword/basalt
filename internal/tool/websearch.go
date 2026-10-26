package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是内置搜索工具 `web_search`（14D T125 / 契约 §5.7）：DeepSeek 官方 Search API 的
// 纯 `net/http` 客户端，零新增 Go 坐标。三条纪律：
// ① **凭据只经既有凭据解析服务**——搜索键取该 Agent 绑定的 DeepSeek 模型连接（T126 的
//    两段绑定槽位注入解析面）；参数面**不接受**密钥，也不新增第二条读取路径；
// ② **判定归 core.Policy**——目标串是编译期常量，但每次外呼（含重发）仍各判一次，
//    与 httptools 的逐跳纪律同口径；
// ③ **失败不编造**——非 2xx / 解析失败 / 传输失败一律返回错误，`results` 不会凭空出现；
//    仅 408 / 429 / 5xx 有限重试（最多 2 次重发），其余 4xx 直接失败（官方口径）。
// This file is the builtin web_search tool: a zero-dependency DeepSeek Search client. The
// key comes only from the existing credential service, every outbound call is judged by
// core.Policy, and failures never fabricate results.

// webSearchEndpoint 是 DeepSeek 官方 Search 接口（官方文档 2026-10-26 实测核对）。
// The search endpoint, verified against the official documentation on 2026-10-26.
const webSearchEndpoint = "https://api.deepseek.com/v1/tools/search"

// webSearchHost 由端点派生（只用于失败文案的脱敏点名——口径 17：只出 host）。
// The host-only form used in every error summary.
var webSearchHost = httpHost(webSearchEndpoint)

// 参数与重试的冻结值（契约 §5.7）：query 1–128 个 Unicode 字符且不得全空白；count
// 缺省 10、取值 1–50；重发最多 2 次（三次尝试），退避 200ms 起、逐次翻倍。
// Frozen limits and the bounded retry policy of contract §5.7.
const (
	webSearchDefaultCount = 10
	webSearchMinCount     = 1
	webSearchMaxCount     = 50
	webSearchMaxQueryRune = 128
	webSearchMaxRetries   = 2
	webSearchRetryBase    = 200 * time.Millisecond
	// webSearchMaxResponseBytes 是响应体读取上限（1 MiB）：50 条结果连同 600–800 字摘要
	// 远小于此；超限即失败，不截断后再当成功解析（截断的 JSON 必然解析失败，早拒更诚实）。
	// The response read cap; an over-limit body fails instead of being parsed half-way.
	webSearchMaxResponseBytes = 1 << 20
)

// webSearchInputSchema 是入参 Schema 的固定字面量（契约 §5.7）：`required` 恰好只有
// `query`——执行器入口按它判 `invalid_input`；取值区间由工具解码点判（分层不重复）。
// The fixed input schema; only query is required.
const webSearchInputSchema = `{"type":"object","properties":{"query":{"type":"string","description":"搜索词：1–128 个字符（Unicode），不得全为空白；支持 site: / -site: / lang: / after: / before: 等查询运算符"},"count":{"type":"integer","minimum":1,"maximum":50,"description":"返回条数：1–50，缺省 10"}},"required":["query"]}`

// WebSearchCredentialResolver 按 Agent 名解析其**绑定 DeepSeek 模型连接**的当前 Key
// （T126）：每次调用都重新解析、**不缓存**——凭据撤销 / 轮换下一次调用立刻生效。返回的
// Key 只随 `Authorization` 头发出，**不得**进入结果正文、错误文案或审计字段。
// The credential face: resolve the agent's bound model connection key on every call, never
// cache it, and never let it reach any output.
type WebSearchCredentialResolver interface {
	ResolveSearchCredential(ctx context.Context, agentName string) (string, error)
}

// WebSearchTool 是 `web_search` 工具：解码 → 判定 → 取凭据 → 有界重试的外呼循环。
// WebSearchTool performs one search with a bounded retry loop.
type WebSearchTool struct {
	policy      core.Policy
	doer        httpDoer
	credentials WebSearchCredentialResolver
	sleep       func(context.Context, time.Duration) error
}

// 编译期断言：本工具就是循环消费的统一 Tool 接口。
// Compile-time assertion that the tool satisfies the single Tool interface.
var _ core.Tool = (*WebSearchTool)(nil)

// NewWebSearchTool 装配生产形态：判定用传入的 Policy，发送面用生产客户端
// `newHTTPClient()`（不自行跟随重定向 + 受控拨号），凭据经注入的解析面。
// NewWebSearchTool wires the production client and the injected credential face.
func NewWebSearchTool(policy core.Policy, credentials WebSearchCredentialResolver) *WebSearchTool {
	return newWebSearchTool(policy, newHTTPClient(), credentials)
}

// newWebSearchTool 是包内测试接缝（注入假 doer 与假 sleep 观察重试行为；不为此导出
// 生产 API——同 httptools 的先例）。
// newWebSearchTool is the in-package seam for a fake doer and a fake sleep.
func newWebSearchTool(policy core.Policy, doer httpDoer, credentials WebSearchCredentialResolver) *WebSearchTool {
	return &WebSearchTool{
		policy:      policy,
		doer:        doer,
		credentials: credentials,
		sleep:       sleepContext,
	}
}

// Name 是工具名——与 AGENT.md 的 `tools:` 声明逐字比对。
// Name is matched verbatim against the tools declaration of AGENT.md.
func (t *WebSearchTool) Name() string { return "web_search" }

// Description 是给模型看的能力说明：本工具只给摘要与链接，正文仍须另行抓取（内置
// http_get 或 MCP Fetch——工具面里哪个能抓就用哪个，本工具不预设）。
// Description is the model-facing summary; page bodies still come from http_get or a
// Fetch server, whichever the tool face offers.
func (t *WebSearchTool) Description() string {
	return "搜索网页并返回结构化结果（标题 / 链接 / 摘要 / 发布时间 / 相关性 / 权威度）；" +
		"只给搜索摘要，正文须用 http_get 或 Fetch 另行抓取"
}

// InputSchema 返回内联 Schema（`required` 是执行侧必填校验的唯一依据）。
// InputSchema returns the inline schema pinned by the contract.
func (t *WebSearchTool) InputSchema() json.RawMessage { return json.RawMessage(webSearchInputSchema) }

// webSearchInput 是入参结构：`count` 用指针区分"未给"（取缺省 10）与显式 0（非法）。
// The input shape; a pointer count tells absent from an explicit zero.
type webSearchInput struct {
	Query string `json:"query"`
	Count *int   `json:"count"`
}

// webSearchRequest 是发给官方接口的请求体（`need_content` 恒 false：正文走 Fetch，
// 搜索结果只作线索——§2.5.4）。
// The request body; need_content is always false because bodies come from Fetch.
type webSearchRequest struct {
	Query       string `json:"query"`
	Count       int    `json:"count"`
	NeedContent bool   `json:"need_content"`
}

// webSearchResponse 是官方响应里本工具消费的部分（其余字段忽略——官方说明未知字段被忽略，
// 反向同样只挑本工具用得到的）。
// The subset of the official response this tool consumes.
type webSearchResponse struct {
	Results []webSearchItem `json:"results"`
}

// webSearchItem 是官方结果项：`date` 可空（*string），`authority` 的 -1 表示未知。
// One official result item; date is nullable and authority -1 means unknown.
type webSearchItem struct {
	RerankScore float64 `json:"rerank_score"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"`
	Date        *string `json:"date"`
	Authority   int     `json:"authority"`
}

// webSearchResultItem 是回给模型的结果项（七个字段**恒在**，date 未知时为空串）。
// The model-facing result item; all seven fields are always present.
type webSearchResultItem struct {
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"`
	Date        string  `json:"date"`
	RerankScore float64 `json:"rerank_score"`
	Authority   int     `json:"authority"`
}

// webSearchOutput 是工具成功的输出正文：`results` **恒为数组**（空结果写 `[]`，不写 null）。
// The success body; results is always an array.
type webSearchOutput struct {
	Query   string                `json:"query"`
	Results []webSearchResultItem `json:"results"`
}

// Execute 落 14D 搜索流程：解码 → 首次判定 → 取凭据 → 外呼（重发前再判一次）。
// 任一前置失败都在外呼之前返回；重发次数有硬上限，不做后台补投。
// Execute decodes, judges, resolves the key, and runs the bounded outbound loop.
func (t *WebSearchTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	in, err := decodeWebSearchInput(input)
	if err != nil {
		return core.Result{}, err
	}
	// ① 判定（首次外呼之前）：目标串是常量，但判定仍归 core.Policy，不由工具预判。
	// The first policy verdict happens before any credential work or outbound call.
	if err := t.checkPolicy(ctx, env); err != nil {
		return core.Result{}, err
	}
	// ② 凭据：每次调用重新解析（撤销 / 轮换立刻生效），失败关闭、不回显任何材料。
	// The key is resolved per call and never cached; a failure fails closed.
	key, err := t.resolveCredential(ctx, env)
	if err != nil {
		return core.Result{}, err
	}
	body, err := json.Marshal(webSearchRequest{
		Query:       in.Query,
		Count:       *in.Count, // decode 已把缺省补成 10（指针非 nil 由它保证）
		NeedContent: false,
	})
	if err != nil {
		return core.Result{}, fmt.Errorf("编码 web_search 请求体: %w", err)
	}

	var lastErr error
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			// ③ 每次外呼各判一次（与 httptools 逐跳纪律同口径）：重发也是一次外呼。
			// Every outbound attempt is judged, retries included.
			if err := t.checkPolicy(ctx, env); err != nil {
				return core.Result{}, err
			}
		}
		results, retryable, err := t.searchOnce(ctx, key, body)
		if err == nil {
			out, marshalErr := json.Marshal(webSearchOutput{Query: in.Query, Results: results})
			if marshalErr != nil {
				return core.Result{}, fmt.Errorf("编码 web_search 结果: %w", marshalErr)
			}
			return core.Result{Content: string(out), Retryable: false}, nil
		}
		lastErr = err
		if !retryable || attempt > webSearchMaxRetries {
			break
		}
		// 退避等待也归 ctx：Run 取消 / 工具截止在等待期间到达即按身份上抛（归 timeout）。
		// The backoff wait honours the context so a cancellation is never swallowed.
		if err := t.sleep(ctx, webSearchRetryBase<<(attempt-1)); err != nil {
			return core.Result{}, err
		}
	}
	return core.Result{}, lastErr
}

// checkPolicy 是本工具**唯一**的判定点：动作类型 HTTP 请求、目标为常量端点、Agent 取自
// 执行环境（与 httptools 同一 Action 形态，白名单 / 档位语义完全复用）。
// checkPolicy is the only judgement point, using the same action shape as httptools.
func (t *WebSearchTool) checkPolicy(ctx context.Context, env core.ToolEnv) error {
	return t.policy.Check(ctx, core.Action{
		Type:   core.ActionHTTPRequest,
		Target: webSearchEndpoint,
		Agent:  env.AgentName,
	})
}

// resolveCredential 经既有凭据解析面取 Key：没有解析器 / 解析失败 / 空值一律失败关闭。
// 错误只点名 Agent，**不含**任何材料片段。
// resolveCredential fails closed on a missing resolver, a resolver error, or an empty key.
func (t *WebSearchTool) resolveCredential(ctx context.Context, env core.ToolEnv) (string, error) {
	if t.credentials == nil {
		return "", fmt.Errorf("web_search 未配置搜索凭据解析面：无法解析 Agent %s 绑定的模型连接", env.AgentName)
	}
	key, err := t.credentials.ResolveSearchCredential(ctx, env.AgentName)
	if err != nil {
		// %w 保留底层身份：取消 / 截止仍归 timeout，其余归 runtime_error。
		return "", fmt.Errorf("取不到 Agent %s 的搜索凭据: %w", env.AgentName, err)
	}
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("解析到空的搜索凭据：Agent %s", env.AgentName)
	}
	return key, nil
}

// searchOnce 执行一次外呼并解析响应；第二个返回值表示该失败是否值得重发（仅
// 408 / 429 / 5xx）。失败文案只出自产类别 + host，`Authorization` 与响应体都不回显。
// searchOnce performs one outbound call; only 408/429/5xx are reported as retryable.
func (t *WebSearchTool) searchOnce(ctx context.Context, key string, body []byte) ([]webSearchResultItem, bool, error) {
	host := webSearchHost
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webSearchEndpoint, bytes.NewReader(body))
	if err != nil {
		// 常量端点构造失败只可能是编码层缺陷：本仓内部错误（runtime_error）。
		return nil, false, fmt.Errorf("构造搜索请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := t.doer.Do(req)
	if err != nil {
		if ctxErr := ctxPreferred(ctx, err); ctxErr != nil {
			return nil, false, ctxErr
		}
		// 受控拨号在**建连之前**拒绝：策略判定，原样上抛（执行器记 sandbox_denied）。
		// A guard refusal before dialing is a policy verdict, passed through unwrapped.
		var violation *core.SandboxViolationError
		if errors.As(err, &violation) {
			return nil, false, violation
		}
		return nil, false, &core.ExternalError{
			Target: host,
			Reason: httpTransportReason(err) + "：" + host,
			Cause:  err,
		}
	}

	// 失败面先判：非 2xx 的结论由状态码给出，响应体**不读**（只关闭——同 httptools）。
	// The status verdict comes first; a failure body is closed unread.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		discardBody(resp)
		return nil, webSearchRetryableStatus(resp.StatusCode), webSearchStatusError(resp.StatusCode)
	}

	// 读取上限 = 常量上限；多读 1 字节让超限判定必定命中（同 httptools 的截断判据形态）。
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, webSearchMaxResponseBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		if ctxErr := ctxPreferred(ctx, readErr); ctxErr != nil {
			return nil, false, ctxErr
		}
		return nil, false, &core.ExternalError{Target: host, Reason: "响应读取失败：" + host, Cause: readErr}
	}
	if closeErr != nil {
		// 读取路径的关闭错误不得吞（同包先例）：内容虽已读全，连接状态可疑仍如实报失败。
		return nil, false, &core.ExternalError{Target: host, Reason: "响应读取失败：" + host, Cause: closeErr}
	}
	if len(raw) > webSearchMaxResponseBytes {
		// 超限即失败：截断后的 JSON 必然解析失败，早拒比"解析一半再报语法错"更诚实。
		return nil, false, &core.ExternalError{
			Target: host,
			Reason: fmt.Sprintf("响应超过上限 %d 字节：%s", webSearchMaxResponseBytes, host),
		}
	}

	var decoded webSearchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false, &core.ExternalError{Target: host, Reason: "响应解析失败：" + host, Cause: err}
	}
	// 空结果合法（官方：无匹配返回空数组）——如实回 `[]`，不编造、不失败。
	// An empty result set is a success; the tool never invents entries.
	kept := make([]webSearchResultItem, 0, len(decoded.Results))
	for _, item := range decoded.Results {
		if !webSearchUsableResult(item) {
			continue // 无标题 / 无可用 URL 的条目丢弃，不补造字段
		}
		date := ""
		if item.Date != nil {
			date = strings.TrimSpace(*item.Date)
		}
		kept = append(kept, webSearchResultItem{
			Title:       strings.TrimSpace(item.Title),
			URL:         strings.TrimSpace(item.URL),
			Snippet:     item.Snippet,
			Summary:     item.Summary,
			Date:        date,
			RerankScore: item.RerankScore,
			Authority:   item.Authority,
		})
	}
	return kept, false, nil
}

// webSearchRetryableStatus 判状态码是否属于官方建议"等待后重试"的三档：408 / 429 / 5xx。
// 402（余额不足）与其余 4xx 明确不重试——重发只会重复付费或重复失败。
// Only 408/429/5xx are retryable per the official guidance; 402 and other 4xx are not.
func webSearchRetryableStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= 500
}

// webSearchStatusError 把非 2xx 状态码翻成自产文案的 ExternalError：只出状态码与 host
// （响应体可能含任意内容，**不得**回显）；401 / 402 / 403 / 404 附带固定指引，帮模型判断
// "换条路"还是"停下"。
// webSearchStatusError builds a self-authored external error from the status code only.
func webSearchStatusError(status int) error {
	code := strconv.Itoa(status)
	reason := "HTTP " + code + "：" + webSearchHost
	switch status {
	case http.StatusUnauthorized:
		reason += "（搜索凭据无效或已撤销，请管理员检查该 Agent 绑定的模型连接）"
	case http.StatusPaymentRequired:
		reason += "（账户余额不足，需充值后才能继续搜索）"
	case http.StatusForbidden:
		reason += "（请求被官方安全策略拒绝）"
	case http.StatusNotFound:
		reason += "（搜索接口不存在，请检查服务端版本）"
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		reason += "（已按上限重试仍失败，请稍后再试）"
	default:
		if status >= 500 {
			reason += "（官方服务端错误，已按上限重试仍失败）"
		}
	}
	return &core.ExternalError{Target: code + " " + webSearchHost, Reason: reason}
}

// webSearchUsableResult 判条目是否可用：标题非空，URL 是绝对 http(s) 地址、有 host、
// 不带 userinfo（同 industry_brief.py 的来源校验口径——不可用的条目在这里就丢弃）。
// A usable result has a title and an absolute http(s) URL without userinfo.
func webSearchUsableResult(item webSearchItem) bool {
	if strings.TrimSpace(item.Title) == "" {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(item.URL))
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Hostname() == "" || u.User != nil {
		return false
	}
	return true
}

// decodeWebSearchInput 解入参并做**取值**校验（executor 入口判必填键，这里判取值——两处
// 同产 `invalid_input`）：query 去空白后非空且不超过 128 个 Unicode 字符；count 缺省 10、
// 取值 1–50。全部在**外呼之前**拒——零发送、零凭据解析。
// decodeWebSearchInput value-checks the input before any credential or network work.
func decodeWebSearchInput(input json.RawMessage) (webSearchInput, error) {
	var in webSearchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return webSearchInput{}, &core.InvalidInputError{
			Tool:   "web_search",
			Reason: fmt.Sprintf("解析 web_search 入参：%v", err),
		}
	}
	if strings.TrimSpace(in.Query) == "" {
		return webSearchInput{}, &core.InvalidInputError{
			Tool:   "web_search",
			Reason: "query 不能为空或全为空白",
		}
	}
	if n := utf8.RuneCountInString(in.Query); n > webSearchMaxQueryRune {
		return webSearchInput{}, &core.InvalidInputError{
			Tool:   "web_search",
			Reason: fmt.Sprintf("query 超过长度上限：%d 个字符 > %d（按 Unicode 字符计）", n, webSearchMaxQueryRune),
		}
	}
	if in.Count == nil {
		in.Count = new(int)
		*in.Count = webSearchDefaultCount
	}
	if *in.Count < webSearchMinCount || *in.Count > webSearchMaxCount {
		return webSearchInput{}, &core.InvalidInputError{
			Tool:   "web_search",
			Reason: fmt.Sprintf("count 必须在 %d–%d 之间（实际 %d）", webSearchMinCount, webSearchMaxCount, *in.Count),
		}
	}
	return in, nil
}

// sleepContext 是可取消的退避等待（生产实现；测试注入立即返回的替身跳过真实等待）。
// sleepContext waits unless the context ends first; tests inject an immediate sleep.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
