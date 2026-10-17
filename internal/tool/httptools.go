package tool

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是两件内置出网工具（技术方案 §4.1.1 与 §4.3 授权矩阵 HTTP 行、契约 §5.3）：
// `http_get` / `http_post`。三条纪律贯穿全篇：① 判定只归 **core.Policy**——初始请求与
// 每一次重定向各判一次，工具不预判、不改写目标串（宪法 VI + 约束 15）；② 客户端**不**自行
// 跟随重定向，3xx 由本文件的逐跳循环决定下一跳并重新判定（FR9）；③ 失败文案只出**自产
// 类别与 host**——`url.Error` 的原文里带整条 URL（含 query），**不得**回显（FR16 / 口径 17）。
// 两件工具**只差初始请求规格**（`http_post` 才消费 body 与 content_type），故内核是一个
// 值类型 `httpTool`，导出形态只是它的薄壳。
// The two builtin HTTP tools of contract §5.3. Judgement belongs to core.Policy on every
// hop, the client never follows redirects on its own, and every failure summary is
// self-authored (category + host) instead of echoing url.Error's full-URL text.

// 两件工具的入参 Schema——**内联 JSON Schema 常量**（颗粒度文档 §3.1），`required` 数组是
// 执行侧必填校验的**唯一依据**（core 按它判 invalid_input 并仍写审计行）。两件**都不得**
// 出现 timeout 参数：超时由执行侧统一承担（FR8）。
// Inline JSON Schema literals; core reads the required array to judge missing input.
const (
	httpGetInputSchema  = `{"type":"object","properties":{"url":{"type":"string","description":"请求地址：完整 URL（http 或 https，省略端口或 :80 / :443）；域名须在 http.allowed_domains 白名单内"},"headers":{"type":"object","description":"附加请求头（键与值都是字符串）；只随首跳，跨源重定向不转发","additionalProperties":{"type":"string"}}},"required":["url"]}`
	httpPostInputSchema = `{"type":"object","properties":{"url":{"type":"string","description":"请求地址：完整 URL（http 或 https，省略端口或 :80 / :443）；域名须在 http.allowed_domains 白名单内"},"body":{"type":"string","description":"请求体（必填，空串合法）；按 UTF-8 字节计最多 256 KiB"},"content_type":{"type":"string","description":"请求体的正文类型；缺省 application/json"},"headers":{"type":"object","description":"附加请求头（键与值都是字符串）；只随首跳，跨源重定向不转发","additionalProperties":{"type":"string"}}},"required":["url","body"]}`
)

const (
	// maxRedirectHops 是单次出网允许跟随的重定向次数上限（口径 4）：初始请求**不计**，
	// 故生产 doer 最多收到 6 次请求；超限归 `sandbox_denied`（不是 external_error——FR12）。
	// The hop cap of clause 4: the initial request is not counted, so the doer sees at
	// most six requests; exceeding it is a sandbox denial.
	maxRedirectHops = 5
	// maxRequestBodyBytes 是出站 body 上限（口径 16 的新增裁决，按 UTF-8 字节计）：
	// 超限 → `invalid_input`、**零发送**；契约与方案均无此规定，是本 US 的冻结值。
	// The outbound body cap added by clause 16; over it is invalid input and nothing is
	// sent.
	maxRequestBodyBytes = 256 * 1024
	// defaultContentType 是 http_post 未给 content_type 时的正文类型（契约 §5.3）。
	// The default body content type of http_post.
	defaultContentType = "application/json"

	// 跳数超限的拒绝文案（四段式段 1 与段 4；段 2 是 host、段 3 由 core 拼固定句）。
	// 段 4 **不得**指向域名白名单——白名单管 host、不管跳数。
	ruleRedirectLimit     = "重定向次数超过上限"
	guidanceRedirectLimit = "单次出网最多跟随 5 跳重定向（代码常量），没有配置键可以加；请直接请求最终地址。"
)

// httpDoer 是逐跳循环的发送面：生产实现是 `*http.Client`（天然满足），包内测试注入假
// doer 记账（同包测试不为此导出生产 API——§4.1）。
// The send side of the hop loop: *http.Client in production, a fake doer in-package in
// tests.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// httpTool 是两件出网工具的共用内核（值类型——值本身就实现 core.Tool）。
// `carriesBody` 是两件的**唯一**差异：body 与 content_type 只有 http_post 面消费
// （http_get 的 schema 里没有这两个参数，模型多给也不进请求）。
// The shared kernel of both tools; carriesBody is the only difference between them.
type httpTool struct {
	policy      core.Policy
	doer        httpDoer
	name        string
	description string
	schema      string
	method      string
	carriesBody bool
}

// 编译期断言：内核与两个导出形态都满足工具接口（宪法 VI——调用方只依赖接口）。
// Compile-time assertions that the kernel and both exported shapes are core.Tool values.
var (
	_ core.Tool = httpTool{}
	_ core.Tool = (*HTTPGetTool)(nil)
	_ core.Tool = (*HTTPPostTool)(nil)
)

// HTTPGetTool 是 `http_get` 的导出形态（内嵌共用内核）。
// HTTPGetTool is the exported shape of http_get.
type HTTPGetTool struct{ httpTool }

// HTTPPostTool 是 `http_post` 的导出形态（内嵌共用内核）。
// HTTPPostTool is the exported shape of http_post.
type HTTPPostTool struct{ httpTool }

// NewHTTPGetTool 装配 `http_get`：policy 做判定，发送面用生产客户端 `newHTTPClient()`。
// NewHTTPGetTool wires http_get with the production client as its send side.
func NewHTTPGetTool(policy core.Policy) *HTTPGetTool {
	return &HTTPGetTool{newHTTPTool(policy, newHTTPClient())}
}

// NewHTTPPostTool 装配 `http_post`（同一份逐跳循环，只差初始请求规格）。
// NewHTTPPostTool wires http_post onto the same hop loop with a body-carrying spec.
func NewHTTPPostTool(policy core.Policy) *HTTPPostTool {
	return &HTTPPostTool{newHTTPPostTool(policy, newHTTPClient())}
}

// newHTTPTool 产出 `http_get` 内核（包内测试接缝：注入假 doer 判逐跳行为；生产装配走
// NewHTTPGetTool——不为此导出生产 API）。
// newHTTPTool builds the http_get kernel; the doer seam exists for in-package tests.
func newHTTPTool(policy core.Policy, doer httpDoer) httpTool {
	return httpTool{
		policy: policy,
		doer:   doer,
		name:   "http_get",
		method: http.MethodGet,
		schema: httpGetInputSchema,
		description: "发起 GET 请求取回内容：首行是响应状态行，其后是响应体；" +
			"域名须在 http.allowed_domains 白名单内，只允许 http / https 的默认端口",
	}
}

// newHTTPPostTool 产出 `http_post` 内核：与 GET 面同一条接缝、同一份循环——POST 的判卷
// 面（303 转 GET、Content-Type、body 上限）同样只能靠假 doer 观察。
// newHTTPPostTool builds the http_post kernel from the same seam.
func newHTTPPostTool(policy core.Policy, doer httpDoer) httpTool {
	return httpTool{
		policy:      policy,
		doer:        doer,
		name:        "http_post",
		method:      http.MethodPost,
		schema:      httpPostInputSchema,
		carriesBody: true,
		description: "发起 POST 请求取回内容（body 必填，缺省 JSON 正文类型）；" +
			"域名须在 http.allowed_domains 白名单内，只允许 http / https 的默认端口",
	}
}

// Name 是工具名——与 AGENT.md 的 `tools:` 声明逐字比对。
// Name is matched verbatim against the tools declaration of AGENT.md.
func (t httpTool) Name() string { return t.name }

// Description 是给模型看的能力说明。
// Description is the model-facing summary.
func (t httpTool) Description() string { return t.description }

// InputSchema 返回内联 Schema（`required` 是执行侧必填校验的唯一依据）。
// InputSchema returns the inline schema pinned by the contract.
func (t httpTool) InputSchema() json.RawMessage { return json.RawMessage(t.schema) }

// Execute 落流程 B：解码 → 组**初始请求规格** → 进入逐跳循环。必填键缺失由执行器入口
// 更早判（分层不重复——注 11）；本函数只在解码点判字段类型与取值。
// Execute decodes the input, builds the initial request spec, and enters the hop loop.
func (t httpTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	in, err := decodeHTTPToolInput(t.name, t.carriesBody, input)
	if err != nil {
		return core.Result{}, err
	}
	spec := httpRequestSpec{Method: t.method, URL: in.URL, Headers: in.Headers}
	if t.carriesBody {
		// decode 已保证 carriesBody 工具的 body 非 nil（null / 缺失在那里即拒）。
		spec.Body = *in.Body
		spec.ContentType = in.ContentType
	}
	return executeHTTP(ctx, t.name, t.policy, t.doer, env, spec)
}

// newHTTPClient 是生产发送面——两条配置缺一不可（D-01）：
// ① `CheckRedirect` 返回 `http.ErrUseLastResponse`：客户端**不**自行跟随重定向，3xx 原样
// 交给逐跳循环（每一跳都要重新过判定——FR9）；
// ② `Transport.Proxy` 恒 nil：`HTTP_PROXY` 一设就换出口，域名白名单形同虚设（FR17）。
// 其余沿用 DefaultTransport 的默认值：本 US **不**关传输层对幂等请求的连接复用重发——
// 该重发收敛在一次审计行之内，如实登记（注 10）。
// The production send side: no client-side redirect following, no environment proxy.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// httpToolInput 是两件工具共用的入参结构（两件只差消费哪几个字段）。
// The input shape shared by both tools.
type httpToolInput struct {
	URL         string            `json:"url"`
	Body        *string           `json:"body"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers"`
}

// decodeHTTPToolInput 解入参并做**取值**校验（分层：执行器入口判「合法 JSON 对象 + 必填
// 键」，这里判「字段类型与取值」——两处产出同一类别 `invalid_input`，口径 18）：字段类型
// 不符（如 headers 里混进非字符串）、body 为 `null` / 缺失、body 超上限都在**发出去之前**
// 拒——零发送。
// Decodes and value-checks the input; every failure is an invalid-input carrier and none
// of it is ever sent.
//
// `carriesBody` 决定 body 判据是否生效：`http_get` 的 schema 没有 body，`"body": null`
// 对它只是被忽略的多余字段（与其余多余字段同待遇）；`http_post` 的 body 是必填正文
// ——`null` 解进 `*string` 得 nil、`""` 得非 nil，二者由此分清（口径 19）。
// The pointer field tells null and empty-string apart; only body-carrying tools judge it.
func decodeHTTPToolInput(toolName string, carriesBody bool, input json.RawMessage) (httpToolInput, error) {
	var in httpToolInput
	if err := json.Unmarshal(input, &in); err != nil {
		return httpToolInput{}, &core.InvalidInputError{
			Tool:   toolName,
			Reason: fmt.Sprintf("解析 %s 入参：%v", toolName, err),
		}
	}
	if carriesBody {
		if in.Body == nil {
			// JSON 的 `null` 解进 `string` 字段不留任何痕迹（等同缺席）——上层必填键检查
			// 又只看键存在，放行后这里必须补：null 不是"空正文"的合法写法（口径 19）。
			return httpToolInput{}, &core.InvalidInputError{
				Tool:   toolName,
				Reason: "body 为 null 或缺失：body 必须是字符串，空串才是合法的无正文写法",
			}
		}
		if len(*in.Body) > maxRequestBodyBytes {
			// 只报数字与上限，**不得**回显 body 内容（可能带敏感数据——口径 16）。
			return httpToolInput{}, &core.InvalidInputError{
				Tool: toolName,
				Reason: fmt.Sprintf("请求体超过上限：%d 字节 > %d 字节（按 UTF-8 字节计）",
					len(*in.Body), maxRequestBodyBytes),
			}
		}
	}
	return in, nil
}

// httpRequestSpec 是**当前这一跳**的请求规格；唯一变更路径是 `next(status, location)`——
// 方法 / body / 跨跳请求头的语义因此集中在一处（口径 1 + 约束 30）。
// The spec of the current hop; next() is its only mutation path.
type httpRequestSpec struct {
	Method      string
	URL         string
	Body        string
	ContentType string
	Headers     map[string]string
}

// next 产出一跳的规格，**只在 301 / 302 / 303 / 307 / 308 时被调用**（口径 1）：
// 301 / 302 / 303 → 转 `GET`、丢弃 body 并清正文相关头；307 / 308 → 保持原方法与 body。
// 模型提供的请求头**只随首跳**：仅当目标与当前跳同源（scheme + host + **有效端口**全同）
// 才转发，跨源一律空集；转 GET 时清掉正文相关头（约束 30）。
// next() produces the next hop: 301/302/303 turn into a GET and drop the body, 307/308
// keep both, and model-supplied headers travel only across same-origin hops.
func (spec httpRequestSpec) next(status int, location string) (httpRequestSpec, error) {
	nextURL, err := url.Parse(location)
	if err != nil {
		return spec, fmt.Errorf("解析重定向目标：%w", err)
	}
	current, err := url.Parse(spec.URL)
	if err != nil {
		return spec, fmt.Errorf("解析当前地址：%w", err)
	}

	next := spec
	next.URL = location
	if !sameOrigin(current, nextURL) {
		// 跨源：一律不转发模型头（可能是给首跳的凭证——转发等于把凭证带出去）。
		next.Headers = nil
	}
	if redirectsToGet(status) {
		next.Method = http.MethodGet
		next.Body = ""
		next.ContentType = ""
		next.Headers = withoutBodyHeaders(next.Headers)
	}
	return next, nil
}

// isRedirectStatus 判状态码是否属于本仓跟随的**五种**重定向（口径 1）：其余 3xx（如 304）
// 不跳转、按普通响应返回。
// Only the five redirect statuses of clause 1 are followed; every other 3xx is a plain
// response.
func isRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

// redirectsToGet 判这次跳转是否转 `GET`（口径 1：301 / 302 / 303，历史行为一律转 GET）。
// Whether this redirect status turns the method into GET (clause 1).
func redirectsToGet(status int) bool {
	return status == http.StatusMovedPermanently ||
		status == http.StatusFound ||
		status == http.StatusSeeOther
}

// withoutBodyHeaders 去掉正文相关头（`Content-Type` / `Content-Length` /
// `Content-Encoding`——约束 30 的冻结清单；大小写**不敏感**，模型给的键名大小写任意，
// 进 `http.Header` 时才会被规范化）：转 GET 之后**不得**再带正文头——漏清
// `Content-Encoding` 会让已无正文的 GET 仍宣称"以下内容按 gzip 解释"。
// Strips the three body-related headers case-insensitively after a turn into GET.
func withoutBodyHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	kept := make(map[string]string, len(headers))
	for key, value := range headers {
		switch http.CanonicalHeaderKey(key) {
		case "Content-Type", "Content-Length", "Content-Encoding":
			continue
		}
		kept[key] = value
	}
	return kept
}

// sameOrigin 判两跳是否**同源**（约束 30：scheme + host + 有效端口全同才转发模型头）：
// 省略端口与显式默认端口等价（http 省略 ≡ `:80`、https 省略 ≡ `:443`）。
// Whether two hops share an origin, with implicit default ports treated as explicit.
func sameOrigin(current, next *url.URL) bool {
	return current.Scheme == next.Scheme &&
		current.Hostname() == next.Hostname() &&
		effectivePort(current) == effectivePort(next)
}

// effectivePort 取 URL 的**有效端口**：显式端口原样，省略时按 scheme 的默认档补齐；
// 认不出的 scheme 回空串（端口判定会拒它——这里只服务同源比较）。
// The effective port of a URL: explicit port as-is, implicit default by scheme.
func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if def, ok := defaultPortFor(u.Scheme); ok {
		return strconv.Itoa(def)
	}
	return ""
}

// executeHTTP 落流程 B 的逐跳循环：每一跳「**先判后发**」——判定只此一处（约束 15），
// 工具不预判、不改写目标串。三条路径都必须关闭响应体（连接不泄漏）；读取错误**不得**
// 丢弃（否决 `n, _ =` 形态——EOF 与取消会被读成同一种"读完了"）。
// executeHTTP runs the per-hop loop: every hop is judged before it is sent, every
// response body is closed, and a read error is never discarded.
func executeHTTP(
	ctx context.Context, toolName string, policy core.Policy, doer httpDoer,
	env core.ToolEnv, spec httpRequestSpec,
) (core.Result, error) {
	hop := 0 // 已跟随的重定向次数——初始请求不计（口径 4）
	for {
		// ① 本跳判定（含初始请求）：与每一次重定向走**同一条**路，没有跳过判定的快路径。
		if err := policy.Check(ctx, core.Action{
			Type:   core.ActionHTTPRequest,
			Target: spec.URL,
			Agent:  env.AgentName,
		}); err != nil {
			return core.Result{}, err
		}
		// 文案脱敏：只留 host（不含端口 / userinfo / path / query——口径 17）。
		host := httpHost(spec.URL)

		// ② 建请求：模型头逐项 Set（口径 11，不加黑名单）；POST 定正文类型——专职参数
		// content_type（缺省 application/json）优先于 headers 里的同名键（契约 §5.3）。
		var body io.Reader
		if spec.Body != "" {
			body = strings.NewReader(spec.Body)
		}
		req, err := http.NewRequestWithContext(ctx, spec.Method, spec.URL, body)
		if err != nil {
			// 目标串已过 ①（scheme / host / 端口都判过）却仍构造不出请求：模型给的地址形态
			// 不可用 → 入参非法；不回声原文（原文可能带 userinfo / query）。
			return core.Result{}, &core.InvalidInputError{
				Tool:   toolName,
				Reason: "请求地址无法构造：" + host,
			}
		}
		for key, value := range spec.Headers {
			req.Header.Set(key, value)
		}
		// 正文类型：**只要这一跳是 POST 就设**——空串 body 仍是一次携带正文语义的请求，
		// 漏设会让服务端按别的猜测解释正文（口径 19）；301/302/303 转出的 GET 不设，
		// 且 `next` 已把模型给的正文头清掉（约束 30）。
		if spec.Method == http.MethodPost {
			contentType := spec.ContentType
			if contentType == "" {
				contentType = defaultContentType
			}
			req.Header.Set("Content-Type", contentType)
		}

		// ③ 发送：生产 doer 的 CheckRedirect 恒返回 ErrUseLastResponse——3xx 由本循环决定
		// 下一跳，客户端不得自行跟随（FR9）。
		resp, err := doer.Do(req)
		if err != nil {
			// 取消 / 截止**原样上抛**（身份优先，执行器据此归 timeout——约束 20、口径 3）；
			// 其余归外部系统错误，文案只出自产类别 + host（不回显 err 原文）。
			if ctxErr := ctxPreferred(ctx, err); ctxErr != nil {
				return core.Result{}, ctxErr
			}
			return core.Result{}, &core.ExternalError{
				Target: host,
				Reason: httpTransportReason(err) + "：" + host,
				Cause:  err,
			}
		}

		// ④ 按状态码分三条路（**每条路都关响应体**——连接不泄漏）。
		switch {
		case isRedirectStatus(resp.StatusCode):
			discardBody(resp) // 本跳不读正文：只关闭（判卷面是状态码与判定）
			hop++
			if hop > maxRedirectHops {
				return core.Result{}, &core.SandboxViolationError{
					Rule:     ruleRedirectLimit,
					Target:   host,
					Guidance: guidanceRedirectLimit,
				}
			}
			location := resp.Header.Get("Location")
			if location == "" {
				// 外部系统给不出目标——不是我们的错误（口径 10）；不回显任何原文。
				return core.Result{}, &core.ExternalError{
					Target: host,
					Reason: "重定向目标缺失：" + host,
				}
			}
			ref, err := url.Parse(location)
			if err != nil {
				// Location 原文可能带 userinfo / 凭证：只进原因链、**不进文案**。
				return core.Result{}, &core.ExternalError{
					Target: host,
					Reason: "重定向目标无法解析：" + host,
					Cause:  err,
				}
			}
			// 相对 Location 按**当前 URL** 解析（RFC 3986）；下一跳从 ① 重新判起。
			resolved := req.URL.ResolveReference(ref)
			next, err := spec.next(resp.StatusCode, resolved.String())
			if err != nil {
				return core.Result{}, &core.ExternalError{
					Target: host,
					Reason: "重定向目标无法解析：" + host,
					Cause:  err,
				}
			}
			spec = next
			continue

		case resp.StatusCode >= 400:
			discardBody(resp) // 失败面是状态码；关闭只为不让连接泄漏
			return core.Result{}, httpFailure(resp.StatusCode, host)

		default:
			// 2xx 与**未被列为跳转的 3xx**（如 304）：普通返回；读取上限 = 生效预算，多读
			// 1 字节让执行器的截断判据必定命中（约束 12）；截断标记由执行器追加一次。
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(env.MaxOutputBytes)+1))
			closeErr := resp.Body.Close()
			if readErr != nil {
				if ctxErr := ctxPreferred(ctx, readErr); ctxErr != nil {
					return core.Result{}, ctxErr
				}
				return core.Result{}, &core.ExternalError{
					Target: host,
					Reason: "响应读取失败：" + host,
					Cause:  readErr,
				}
			}
			if closeErr != nil {
				// 读取路径的关闭错误**不得**吞（同包先例：filetools 把 readErr 与 closeErr
				// 一并上报）。类别沿用「响应读取失败」——同一条"拿响应体"的路，不新造文案
				// （口径 17 的类别是闭集）；内容虽已读全，连接状态可疑仍如实报失败。
				return core.Result{}, &core.ExternalError{
					Target: host,
					Reason: "响应读取失败：" + host,
					Cause:  closeErr,
				}
			}
			return core.Result{
				Content:   "HTTP " + resp.Status + "\n" + string(raw),
				Retryable: false, // 口径 2：当前不给出可重试建议（本 US 两件一律 false）
			}, nil
		}
	}
}

// httpFailure 组装 4xx / 5xx 的外部系统错误（口径 9 / 17）：文案**不含** query / 响应体 /
// Location 原文（query 可能带凭证、响应体可能极大且不可信）；`Target` 是状态码与 host 的
// 组合，原因链为空——这里没有需要保留的底层错误。
// httpFailure builds the external error of a 4xx/5xx response from the status code and
// the host only.
func httpFailure(status int, host string) error {
	code := strconv.Itoa(status)
	return &core.ExternalError{
		Target: code + " " + host,
		Reason: "HTTP " + code + "：" + host,
	}
}

// httpHost 取 URL 的 host（**不含**端口 / userinfo / path / query——口径 17 的脱敏口径）；
// 解析不出来时回无法点名的安全文案（只进错误承载，不参与判定）。
// The host-only form used in every error summary.
func httpHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return targetUnresolvable
	}
	return u.Hostname()
}

// httpTransportReason 给传输层失败一个**自产类别**（口径 17：类别按错误**类型**判定——
// 原文是数据、不进文案）：DNS 解析失败 / TLS 握手失败 / 连接失败。判类型用
// `errors.AsType`（Go 1.26 起的类型化形态，本机 `go doc errors.AsType` 已核）：写前先按
// **具体类型**判，落不到任何一类才是兜底的「连接失败」。
// A self-authored category for transport failures, decided by error type.
func httpTransportReason(err error) string {
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return "DNS 解析失败"
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return "TLS 握手失败"
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return "TLS 握手失败"
	}
	return "连接失败"
}

// ctxPreferred 判这条错误是不是**取消 / 截止**（约束 20：两个错误点都先判 ctx 再包装）：
// 命中即返回 **ctx 身份哨兵本体**、由调用方直接上抛（执行器按身份归 timeout），未命中返回 nil。
// **两条路都只回哨兵**——调用方 ctx 已结束回 `ctx.Err()`，ctx 仍有效但底层错误自带截止身份
// （如传输层内部超时）同样只回哨兵：`url.Error` 的文案里带整条 URL（含 query），而失败文案
// 会回填进模型可见的对话、WARN 日志与审计 error_message（FR20 / 口径 17）。
// ctxPreferred reports the cancellation identity of an error before it is ever wrapped;
// a dead context yields the bare sentinel so no transport text reaches the model.
func ctxPreferred(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// 调用方 ctx 仍有效、底层错误却自带取消 / 截止身份（如传输层内部超时）：**只回身份
	// 哨兵**，绝不上抛传输层的包装错误——`url.Error` 的文案里带整条 URL（含 query），
	// 而这条错误随后进入模型可见的对话、WARN 日志与审计 error_message（FR20 / 口径 17）。
	// 哨兵本体即身份，`errors.Is`（乃至 `==`）分类能力原样保留（约束 20）。
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

// discardBody 关闭本轮**不读取**的响应体（跳转与 4xx / 5xx 两条路）：关闭只为不让连接
// 泄漏——这两条路的结论由状态码与判定给出，关闭失败不改结论，故按同仓清理写法并入 `_ =`
// 丢弃（internal/storage 的 rows.Close 同一形态）；工具不记日志（FR24），没有第二条出口。
// 读取路径**不得**走本函数：那条路的关闭错误与读取错误一并上报（见 executeHTTP）。
// Closes a body the hop loop will not read; the verdict comes from the status code, so a
// close failure no longer changes it. The reading path must not use this helper.
func discardBody(resp *http.Response) {
	if resp.Body == nil {
		return
	}
	_ = resp.Body.Close()
}
