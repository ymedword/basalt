package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是两件出网工具（`http_get` / `http_post`）的假 doer 主面（颗粒度文档 §3.2 的
// httptools_test.go）：R1~R4 / R7 / R8 六条关键回归在本文件内独立成任务、同址落地。
// 三条判卷纪律决定用例形态（§4.1）：① 逐跳用例两侧记账——假 doer 记「发了几次」，
// 假策略记「判了哪些目标、拒了几次」；② 取消用例等「请求已发出」信号再取消、限时返回；
// ④ 真链路用例必须经生产 `newHTTPClient` + 本地回环 `httptest`，不得再造客户端。
// 夹具在本文件内定义、仅供本包测试共用；**不**扩 `internal/testutil`。
//
// The fake-doer home of the two HTTP tools: every hop is judged with two-sided
// accounting — sends on the doer, judgements on the policy. Fixtures live in this
// file and are shared with the R1-R8 regressions below.
//
// —— 夹具 ——

// httpTestBudget 是多数用例给 core.ToolEnv.MaxOutputBytes 的具体值（出网工具的读取上限
// 逐次由执行侧给出，工具不得自行假设）。
const httpTestBudget = 256 * 1024

// requestBodyLimit 是出站 body 上限的**独立副本**（口径 16 / FR14 的新增裁决 256 KiB）：
// 上限是模型可见契约的一部分，实现改动应当让这里的断言失败——引用实现常量会让两边一起改。
const requestBodyLimit = 256 * 1024

// httpToolEnv 造一次出网调用的显式执行环境（工具不回读任何全局状态）。
func httpToolEnv() core.ToolEnv {
	return core.ToolEnv{
		RunID:          "run-http-1",
		SessionID:      "sess-http-1",
		AgentName:      "weather-agent",
		MaxOutputBytes: httpTestBudget,
	}
}

// jsonInput 把入参对象编成 JSON（与执行侧同形，不经任何封装）。
func jsonInput(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(fields)
	require.NoError(t, err)
	return raw
}

// fakeDoer 是进程内假 doer：带锁记账请求次数与请求列表，响应由 respond 脚本给出（零 socket）。
// The in-process fake doer: sends are counted under a lock, responses come from a script.
type fakeDoer struct {
	mu       sync.Mutex
	respond  func(*http.Request) (*http.Response, error)
	requests []*http.Request
}

// Do 记账后交给 respond 脚本（脚本未提供即报错——不许静默发空响应）。
func (d *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.requests = append(d.requests, req)
	d.mu.Unlock()
	if d.respond == nil {
		return nil, errors.New("fakeDoer 未提供 respond 脚本")
	}
	return d.respond(req)
}

// count 返回已发出的请求次数（与 Do 并发安全——-race 下也是判卷依据）。
func (d *fakeDoer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

// recorded 返回请求列表的拷贝（顺序即发送顺序）。
func (d *fakeDoer) recorded() []*http.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests)
}

// redirectResponse 造一个带 Location 的 3xx 响应（location 为空串时不带头——缺 Location 用例）。
func redirectResponse(status int, location string) *http.Response {
	header := http.Header{}
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

// statusResponse 造一个普通响应（Body 来自字符串，零 socket）。
func statusResponse(status int, body string) *http.Response {
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// okResponder 是「总是返回同一个普通响应」的 respond 脚本。
func okResponder(status int, body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return statusResponse(status, body), nil
	}
}

// spyPolicy 是记账用假策略：放行名单内的目标（出网按 hostname、命令按整串——与真策略
// 同一口径的两条），其余四段式拒。记 raw target（判定顺序即逐跳顺序）与拒绝次数。
// The accounting stub policy: raw targets and refusal counts, both under a lock.
type spyPolicy struct {
	mu      sync.Mutex
	allowed []string
	judged  []string
	denied  int
}

// Check 判一次并记账（判据只认目标，不看动作细目）。
func (p *spyPolicy) Check(_ context.Context, action core.Action) error {
	subject := action.Target
	if action.Type == core.ActionHTTPRequest {
		if parsed, err := url.Parse(action.Target); err == nil {
			subject = parsed.Hostname()
		}
	}
	p.mu.Lock()
	p.judged = append(p.judged, action.Target)
	refused := !slices.Contains(p.allowed, subject)
	if refused {
		p.denied++
	}
	p.mu.Unlock()
	if refused {
		return &core.SandboxViolationError{
			Rule:     "测试夹具只放行名单内目标",
			Target:   action.Target,
			Guidance: "测试夹具不提供配置键。",
		}
	}
	return nil
}

// targets 返回判定过的目标串（顺序即逐跳判定顺序）。
func (p *spyPolicy) targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.judged)
}

// denials 返回被拒的次数。
func (p *spyPolicy) denials() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.denied
}

// httpPolicyFor 造一个只放行给定域名的**真策略**（端口与 IP 字面量用例的夹具）。
func httpPolicyFor(t *testing.T, domains ...string) *WhitelistPolicy {
	t.Helper()
	return NewWhitelistPolicy(PolicyOptions{Root: t.TempDir(), AllowedDomains: domains})
}

// requiredFieldsOf 从工具的 InputSchema 读 required 数组（执行侧必填校验的唯一依据）。
func requiredFieldsOf(t *testing.T, tl core.Tool) []string {
	t.Helper()
	var shape struct {
		Required []string `json:"required"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &shape), "Schema 必须是合法 JSON")
	return shape.Required
}

// cancelAwareReader 是阻塞中的响应体：ctx 结束即返回 ctx.Err()——**不得**返回 io.EOF
// （EOF 会被 ReadAll 读成「读完了」，取消被静默吞掉）。
type cancelAwareReader struct{ ctx context.Context }

// Read 阻塞到 ctx 结束（判据：返回 ctx 身份而非 EOF）。
func (r cancelAwareReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// errorReader 是「一读就错」的响应体。
type errorReader struct{ err error }

// Read 立刻返回预置错误。
func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// —— 用例（22 条）——

// TestHttpTools_SchemaRequiredFields：契约 §5.3 的参数与必填——http_get 必填 url；
// http_post 必填 url + body（body 必填与基线一致，但**空串合法**：必填 ≠ 非空）。
func TestHttpTools_SchemaRequiredFields(t *testing.T) {
	get := NewHTTPGetTool(&spyPolicy{})
	post := NewHTTPPostTool(&spyPolicy{})

	require.Equal(t, "http_get", get.Name())
	require.Equal(t, "http_post", post.Name())
	require.NotEmpty(t, get.Description())
	require.NotEmpty(t, post.Description())
	require.Equal(t, []string{"url"}, requiredFieldsOf(t, get))
	require.Equal(t, []string{"url", "body"}, requiredFieldsOf(t, post))
}

// TestHttpGet_StatusLineAndBody：口径 7/8——内容 = 首行 `HTTP <status>` + 换行 + 响应体
// 原文；空体只有首行；Retryable 恒 false（口径 2：当前不给出可重试建议）。
func TestHttpGet_StatusLineAndBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"普通响应体", "weather: sunny", "HTTP 200 OK\nweather: sunny"},
		{"空体只有首行", "", "HTTP 200 OK\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(200, tc.body)}
			res, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
				context.Background(), httpToolEnv(),
				jsonInput(t, map[string]any{"url": "http://api.example.com/weather"}))
			require.NoError(t, err)
			require.Equal(t, tc.want, res.Content)
			require.False(t, res.Retryable, "口径 2：一律 false（当前不给出可重试建议）")
			require.Equal(t, 1, doer.count())
		})
	}
}

// TestHTTP_ClientErrorStatusIsExternalError：口径 9/17——4xx / 5xx 归外部系统错误，
// 文案**全串** `HTTP <status>：<host>`：不含 query、不含响应体、不含 URL 原文。
func TestHTTP_ClientErrorStatusIsExternalError(t *testing.T) {
	cases := []int{400, 404, 500, 503}
	for _, status := range cases {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(status, "很长的响应体，可能带敏感数据")}
			_, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
				context.Background(), httpToolEnv(),
				jsonInput(t, map[string]any{"url": "http://api.example.com/weather?token=should-not-leak"}))
			require.Error(t, err)
			require.True(t, core.IsExternalError(err), "4xx/5xx 归外部系统错误（契约 §5.3）")
			require.Equal(t, "HTTP "+strconv.Itoa(status)+"：api.example.com", err.Error(),
				"失败文案全串：HTTP <status>：<host>")
			require.NotContains(t, err.Error(), "token", "文案不含 query")
			require.NotContains(t, err.Error(), "敏感数据", "文案不含响应体")
			require.Equal(t, 1, doer.count())
		})
	}
}

// TestHTTP_NonRedirectStatusIsOrdinaryResponse：口径 1/8——未被列为跳转的 3xx（如 304）
// 按普通响应返回：即使带了 Location 也不跟随。
func TestHTTP_NonRedirectStatusIsOrdinaryResponse(t *testing.T) {
	doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
		resp := statusResponse(http.StatusNotModified, "")
		resp.Header.Set("Location", "http://api.example.com/moved")
		return resp, nil
	}}
	res, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
		context.Background(), httpToolEnv(),
		jsonInput(t, map[string]any{"url": "http://api.example.com/weather"}))
	require.NoError(t, err)
	require.Equal(t, "HTTP 304 Not Modified\n", res.Content)
	require.Equal(t, 1, doer.count(), "304 不跳转——不得再发一次请求")
}

// TestHTTPRequestSpec_NextRedirectSemantics：口径 1 + 约束 30——301/302/303 转 GET 且
// 丢弃 body（同时清正文相关头，大小写不敏感）；307/308 保持原方法与 body；同源时模型头
// 随跳转发。语义集中 `next` 一处，故在此逐格冻结。
func TestHTTPRequestSpec_NextRedirectSemantics(t *testing.T) {
	base := httpRequestSpec{
		Method:      http.MethodPost,
		URL:         "http://api.example.com/weather",
		Body:        `{"city":"北京"}`,
		ContentType: "application/json",
		Headers: map[string]string{
			"X-Trace":        "t-1",
			"Content-Type":   "text/plain",
			"content-length": "18",
		},
	}
	cases := []struct {
		status   int
		wantMeth string
		wantBody string
	}{
		{http.StatusMovedPermanently, http.MethodGet, ""},
		{http.StatusFound, http.MethodGet, ""},
		{http.StatusSeeOther, http.MethodGet, ""},
		{http.StatusTemporaryRedirect, http.MethodPost, `{"city":"北京"}`},
		{http.StatusPermanentRedirect, http.MethodPost, `{"city":"北京"}`},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			next, err := base.next(tc.status, "http://api.example.com/next")
			require.NoError(t, err)
			require.Equal(t, tc.wantMeth, next.Method)
			require.Equal(t, tc.wantBody, next.Body)
			require.Equal(t, "http://api.example.com/next", next.URL)
			require.Equal(t, "t-1", next.Headers["X-Trace"], "同源跳带模型头（约束 30）")
			if tc.wantMeth == http.MethodGet {
				require.Empty(t, next.ContentType, "转 GET 丢弃正文类型")
				require.NotContains(t, next.Headers, "Content-Type", "转 GET 清正文相关头（大小写不敏感）")
				require.NotContains(t, next.Headers, "content-length")
				require.NotContains(t, next.Headers, "Content-Length")
				return
			}
			require.Equal(t, "application/json", next.ContentType, "307/308 保持正文类型")
			require.Equal(t, "text/plain", next.Headers["Content-Type"], "307/308 保持模型给的正文头")
		})
	}
}

// TestHttpGet_RelativeLocationResolved：口径 10——相对 Location 按**当前跳 URL** 解析
// （ResolveReference）；每一跳都由工具再判再审（判定只此一处）。
func TestHttpGet_RelativeLocationResolved(t *testing.T) {
	doer := &fakeDoer{respond: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/first" {
			return redirectResponse(http.StatusFound, "/second"), nil
		}
		return statusResponse(http.StatusOK, "ok"), nil
	}}
	policy := &spyPolicy{allowed: []string{"api.example.com"}}
	res, err := newHTTPTool(policy, doer).Execute(context.Background(), httpToolEnv(),
		jsonInput(t, map[string]any{"url": "http://api.example.com/first"}))
	require.NoError(t, err)
	require.Equal(t, "HTTP 200 OK\nok", res.Content)
	require.Equal(t, []string{"http://api.example.com/first", "http://api.example.com/second"},
		policy.targets(), "同一跳判定 + 发送，顺序即逐跳顺序")
	require.Equal(t, 2, doer.count())
}

// TestHTTPPost_Redirect303BecomesGet：口径 1 的端到端形态——303 之后发出的**确实是**
// GET、且不带正文（不是只改了 spec 没改请求）。
func TestHTTPPost_Redirect303BecomesGet(t *testing.T) {
	doer := &fakeDoer{respond: func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			return redirectResponse(http.StatusSeeOther, "http://api.example.com/result"), nil
		}
		return statusResponse(http.StatusOK, "ok"), nil
	}}
	post := newHTTPPostTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer)
	res, err := post.Execute(context.Background(), httpToolEnv(),
		jsonInput(t, map[string]any{"url": "http://api.example.com/submit", "body": `{"a":1}`}))
	require.NoError(t, err)
	require.Equal(t, "HTTP 200 OK\nok", res.Content)
	require.Equal(t, 2, doer.count())
	second := doer.recorded()[1]
	require.Equal(t, http.MethodGet, second.Method)
	require.Empty(t, second.Header.Get("Content-Type"), "转 GET 后不得再带正文头")
	require.Zero(t, second.ContentLength, "转 GET 后不得再带正文")
}

// TestHTTP_RedirectWithoutLocationIsExternalError：口径 10——3xx 缺 Location 是外部系统
// 给不出目标 → 外部系统错误；文案不回显任何原文。
func TestHTTP_RedirectWithoutLocationIsExternalError(t *testing.T) {
	doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
		return redirectResponse(http.StatusFound, ""), nil
	}}
	_, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
		context.Background(), httpToolEnv(),
		jsonInput(t, map[string]any{"url": "http://api.example.com/weather"}))
	require.Error(t, err)
	require.True(t, core.IsExternalError(err))
	require.Equal(t, "重定向目标缺失：api.example.com", err.Error())
	require.Equal(t, 1, doer.count(), "缺目标不产生第二跳")
}

// TestHTTP_UnparseableLocationIsExternalError：口径 10/17——坏 Location 同归外部系统
// 错误，**不回显 Location 原文**（原文可能带 query / 凭证）。
func TestHTTP_UnparseableLocationIsExternalError(t *testing.T) {
	const raw = "http://%zz/secret?token=leak"
	doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
		return redirectResponse(http.StatusFound, raw), nil
	}}
	_, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
		context.Background(), httpToolEnv(),
		jsonInput(t, map[string]any{"url": "http://api.example.com/weather"}))
	require.Error(t, err)
	require.True(t, core.IsExternalError(err))
	require.Equal(t, "重定向目标无法解析：api.example.com", err.Error())
	require.NotContains(t, err.Error(), "%zz", "不回显 Location 原文")
	require.NotContains(t, err.Error(), "leak")
	require.Equal(t, 1, doer.count())
}

// TestHTTP_SameOriginRedirectForwardsHeaders：约束 30——同源（scheme + host + 端口**全同**）
// 才转发模型头；显式 `:80` 与省略端口等价（有效端口比较）。
func TestHTTP_SameOriginRedirectForwardsHeaders(t *testing.T) {
	doer := &fakeDoer{respond: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/first" {
			return redirectResponse(http.StatusFound, "http://api.example.com:80/second"), nil
		}
		return statusResponse(http.StatusOK, "ok"), nil
	}}
	_, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
		context.Background(), httpToolEnv(),
		jsonInput(t, map[string]any{
			"url":     "http://api.example.com/first",
			"headers": map[string]string{"X-Trace": "t-1"},
		}))
	require.NoError(t, err)
	require.Equal(t, 2, doer.count())
	second := doer.recorded()[1]
	require.Equal(t, "t-1", second.Header.Get("X-Trace"), "同源跳转发模型头")
}

// TestHTTP_CrossOriginRedirectDropsHeaders：约束 30——跨源（host 变了）一律**不**转发
// 模型头：请求头只随首跳。
func TestHTTP_CrossOriginRedirectDropsHeaders(t *testing.T) {
	doer := &fakeDoer{respond: func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "api.example.com" {
			return redirectResponse(http.StatusFound, "http://other.example.com/second"), nil
		}
		return statusResponse(http.StatusOK, "ok"), nil
	}}
	policy := &spyPolicy{allowed: []string{"api.example.com", "other.example.com"}}
	_, err := newHTTPTool(policy, doer).Execute(context.Background(), httpToolEnv(),
		jsonInput(t, map[string]any{
			"url":     "http://api.example.com/first",
			"headers": map[string]string{"X-Trace": "t-1", "Authorization": "Bearer ${WEATHER_TOKEN}"},
		}))
	require.NoError(t, err)
	require.Equal(t, 2, doer.count(), "跨源跳照样发出（目标域名在白名单内），只是不带头")
	second := doer.recorded()[1]
	require.Empty(t, second.Header.Get("X-Trace"), "跨源不转发模型头")
	require.Empty(t, second.Header.Get("Authorization"))
}

// TestHttpPost_ContentTypeDefaultAndOverride：契约 §5.3——`content_type` 缺省
// `application/json`、显式给出即覆盖；body 原样发出。
func TestHttpPost_ContentTypeDefaultAndOverride(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		ctype string
		want  string
	}{
		{"缺省 application/json", `{"a":1}`, "", "application/json"},
		{"显式覆盖", "x=1", "application/x-www-form-urlencoded", "application/x-www-form-urlencoded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sentBodies []string
			doer := &fakeDoer{respond: func(req *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				sentBodies = append(sentBodies, string(raw))
				return statusResponse(http.StatusOK, "ok"), nil
			}}
			fields := map[string]any{"url": "http://api.example.com/submit", "body": tc.body}
			if tc.ctype != "" {
				fields["content_type"] = tc.ctype
			}
			post := newHTTPPostTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer)
			res, err := post.Execute(context.Background(), httpToolEnv(), jsonInput(t, fields))
			require.NoError(t, err)
			require.Equal(t, "HTTP 200 OK\nok", res.Content)
			require.Equal(t, []string{tc.body}, sentBodies, "body 原样发出")
			require.Equal(t, tc.want, doer.recorded()[0].Header.Get("Content-Type"))
		})
	}
}

// TestHttpGet_HeadersSetVerbatim：口径 11——模型给的请求头逐项 `Set` 原样发出
// （不加黑名单、不重命名）；未给头则一个不加。
func TestHttpGet_HeadersSetVerbatim(t *testing.T) {
	doer := &fakeDoer{respond: okResponder(http.StatusOK, "ok")}
	_, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
		context.Background(), httpToolEnv(),
		jsonInput(t, map[string]any{
			"url": "http://api.example.com/weather",
			"headers": map[string]string{
				"X-Weather": "rain",
				"Accept":    "text/plain",
			},
		}))
	require.NoError(t, err)
	first := doer.recorded()[0]
	require.Equal(t, "rain", first.Header.Get("X-Weather"))
	require.Equal(t, "text/plain", first.Header.Get("Accept"))
	require.Empty(t, first.Header.Get("Content-Type"), "GET 无 body 不带正文头")
}

// TestHTTP_TransportFailureIsExternalError：口径 3/17——DNS 解析失败 / 连接失败归外部
// 系统错误；文案只出自产类别 + host（**不回显** `url.Error` 原文——其原文含整条 URL），
// 原因链经 `Unwrap` 保留（分类与排障用）。
func TestHTTP_TransportFailureIsExternalError(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  string
	}{
		{
			"DNS 解析失败",
			&url.Error{
				Op: "Get", URL: "http://api.example.com/x?token=leak",
				Err: &net.DNSError{Err: "no such host", Name: "api.example.com"},
			},
			"DNS 解析失败：api.example.com",
		},
		{
			"连接失败",
			&url.Error{
				Op: "Get", URL: "http://api.example.com/x?token=leak",
				Err: errors.New("dial tcp: connection refused"),
			},
			"连接失败：api.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
				return nil, tc.cause
			}}
			_, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
				context.Background(), httpToolEnv(),
				jsonInput(t, map[string]any{"url": "http://api.example.com/weather?token=leak"}))
			require.Error(t, err)
			require.True(t, core.IsExternalError(err))
			require.Equal(t, tc.want, err.Error())
			require.NotContains(t, err.Error(), "http://", "文案不得回显 URL 原文")
			require.NotContains(t, err.Error(), "token")
			require.ErrorIs(t, err, tc.cause, "原因链经 Unwrap 保留")
		})
	}
}

// TestHTTP_ReadErrorKeepsContextIdentity：约束 20——`Do` 与读取两个错误点**先判 ctx 再
// 包装**：取消 / 截止保留原身份（执行器据此归 `timeout`）；普通读取错误归外部系统错误、
// **不得**丢弃。
func TestHTTP_ReadErrorKeepsContextIdentity(t *testing.T) {
	t.Run("取消身份保留", func(t *testing.T) {
		canceled := fmt.Errorf("读响应体：%w", context.Canceled)
		doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
			resp := statusResponse(http.StatusOK, "")
			resp.Body = io.NopCloser(errorReader{err: canceled})
			return resp, nil
		}}
		_, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
			context.Background(), httpToolEnv(),
			jsonInput(t, map[string]any{"url": "http://api.example.com/weather"}))
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, core.IsExternalError(err), "取消不得被包装成外部系统错误（分类归 timeout）")
	})

	t.Run("普通读取错误归外部系统", func(t *testing.T) {
		doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
			resp := statusResponse(http.StatusOK, "")
			resp.Body = io.NopCloser(errorReader{err: errors.New("unexpected EOF")})
			return resp, nil
		}}
		_, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
			context.Background(), httpToolEnv(),
			jsonInput(t, map[string]any{"url": "http://api.example.com/weather"}))
		require.Error(t, err)
		require.True(t, core.IsExternalError(err), "读取失败不得被丢弃（否决 `n, _ =` 形态）")
		require.Equal(t, "响应读取失败：api.example.com", err.Error())
	})
}

// TestHttpGet_ReadsAtMostBudgetPlusOneByte：约束 12 / 口径 7——读取上限 = 生效预算，
// 恰多读 1 字节（让执行器的截断判据必定命中）；截断标记归执行器，工具**不带**标记。
func TestHttpGet_ReadsAtMostBudgetPlusOneByte(t *testing.T) {
	const budget = 16
	doer := &fakeDoer{respond: okResponder(http.StatusOK, strings.Repeat("a", 100))}
	env := httpToolEnv()
	env.MaxOutputBytes = budget
	res, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
		context.Background(), env,
		jsonInput(t, map[string]any{"url": "http://api.example.com/big"}))
	require.NoError(t, err)
	require.Equal(t, "HTTP 200 OK\n"+strings.Repeat("a", budget+1), res.Content)
	require.NotContains(t, res.Content, core.TruncationMarker(budget), "工具不得自行加标记（约束 11/12）")
}

// TestHttpGet_IPLiteralRefusedBeforeSend：契约 §5.3——白名单按域名匹配，IP 字面量目标
// 直接拒（**即使**把 IP 写进白名单同拒）；真策略 × 假 doer：拒绝先于发送（零请求）。
func TestHttpGet_IPLiteralRefusedBeforeSend(t *testing.T) {
	doer := &fakeDoer{respond: okResponder(http.StatusOK, "ok")}
	_, err := newHTTPTool(httpPolicyFor(t, "127.0.0.1"), doer).Execute(
		context.Background(), httpToolEnv(),
		jsonInput(t, map[string]any{"url": "http://127.0.0.1/weather"}))
	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err))
	require.Equal(t, 0, doer.count(), "拒绝先于发送")
}

// TestHttpGet_NonDefaultPortRefusedBeforeSend：口径 15——直接请求的非默认端口拒、
// 归 `sandbox_denied`、**零发送**。
func TestHttpGet_NonDefaultPortRefusedBeforeSend(t *testing.T) {
	// 注：`http://host:/x`（写了冒号、端口为空）按**省略端口**处理——`url.URL.Port()` 为空，
	// 与省略形态语义等价（都走 80）；口径 15 的冻结格只有四格，不为它另造第五格。
	refused := []string{
		"http://api.example.com:8080/x",
		"https://api.example.com:8443/x",
	}
	for _, raw := range refused {
		t.Run(raw, func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(http.StatusOK, "ok")}
			_, err := newHTTPTool(httpPolicyFor(t, "api.example.com"), doer).Execute(
				context.Background(), httpToolEnv(), jsonInput(t, map[string]any{"url": raw}))
			require.Error(t, err)
			require.True(t, core.IsSandboxViolation(err), "端口档判 sandbox_denied（不是 external_error）")
			require.Equal(t, 0, doer.count(), "拒绝先于发送")
		})
	}
}

// TestHttpGet_PortFrozenCombinations：口径 15 的逐格冻结——省略端口与**显式**默认端口
// 等价放行（同 scheme）；交叉档（http:443 / https:80）拒、零发送。
func TestHttpGet_PortFrozenCombinations(t *testing.T) {
	allowed := []string{
		"http://api.example.com/a",
		"http://api.example.com:80/a",
		"https://api.example.com/a",
		"https://api.example.com:443/a",
	}
	for _, raw := range allowed {
		t.Run("放行_"+raw, func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(http.StatusOK, "ok")}
			res, err := newHTTPTool(httpPolicyFor(t, "api.example.com"), doer).Execute(
				context.Background(), httpToolEnv(), jsonInput(t, map[string]any{"url": raw}))
			require.NoError(t, err, "省略与显式默认端口等价放行")
			require.Equal(t, "HTTP 200 OK\nok", res.Content)
			require.Equal(t, 1, doer.count(), "放行档必须真的发出去")
		})
	}

	refused := []string{
		"http://api.example.com:443/a",
		"https://api.example.com:80/a",
	}
	for _, raw := range refused {
		t.Run("拒_"+raw, func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(http.StatusOK, "ok")}
			_, err := newHTTPTool(httpPolicyFor(t, "api.example.com"), doer).Execute(
				context.Background(), httpToolEnv(), jsonInput(t, map[string]any{"url": raw}))
			require.Error(t, err)
			require.True(t, core.IsSandboxViolation(err), "交叉档拒（scheme 与端口不匹配）")
			require.Equal(t, 0, doer.count(), "拒绝先于发送")
		})
	}
}

// TestHttpPost_OverLimitBodyIsInvalidInputBeforeSend：约束 29 / 口径 16——出站 body
// 上限 256 KiB（按 UTF-8 **字节**计）：超限 → 入参非法、**零发送**（连首行 JSON 都不发）；
// 恰好等于上限合法；空串合法（必填 ≠ 非空）。
func TestHttpPost_OverLimitBodyIsInvalidInputBeforeSend(t *testing.T) {
	atLimit := strings.Repeat("地", requestBodyLimit/3) + strings.Repeat("a", requestBodyLimit-requestBodyLimit/3*3)
	require.Len(t, atLimit, requestBodyLimit, "夹具自检：恰好骑在上限上")
	cases := []struct {
		name        string
		body        string
		wantInvalid bool
	}{
		{"空串合法", "", false},
		{"恰好等于上限", atLimit, false},
		{"超过一格", atLimit + "a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(http.StatusOK, "ok")}
			raw := jsonInput(t, map[string]any{"url": "http://api.example.com/submit", "body": tc.body})
			_, err := newHTTPPostTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
				context.Background(), httpToolEnv(), raw)
			if tc.wantInvalid {
				require.Error(t, err)
				require.True(t, core.IsInvalidInput(err), "入参非法（口径 16）")
				require.NotContains(t, err.Error(), strings.Repeat("地", 4), "文案不得回显 body 内容")
				require.Equal(t, 0, doer.count(), "超限零发送")
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, doer.count())
		})
	}
}

// TestNewHTTPClient_RedirectAndProxyConfiguration：FR9 / FR17——客户端**不**自行跟随
// 重定向（`CheckRedirect` 返回 `ErrUseLastResponse`，3xx 由工具的逐跳循环驱动）；
// `Transport.Proxy` 恒 nil（`HTTP_PROXY` 一设就换出口，域名档形同虚设）。
func TestNewHTTPClient_RedirectAndProxyConfiguration(t *testing.T) {
	client := newHTTPClient()
	require.ErrorIs(t, client.CheckRedirect(nil, nil), http.ErrUseLastResponse,
		"3xx 必须原样交给工具（由工具决定下一跳并重新判定）")
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok, "Transport 必须是 *http.Transport（要能判 Proxy 字段）")
	require.Nil(t, transport.Proxy, "不读环境代理变量")
}

// TestHTTPWithRealClient_CancelPropagatesPromptly：真链路第三例（§3.2「httptest 真链路
// 三条」之三）——生产 `newHTTPClient` + 本地回环服务：等「请求已到达」信号再取消，
// `Execute` 限时返回取消错误（判卷纪律 ②：等信号、限时等）。
func TestHTTPWithRealClient_CancelPropagatesPromptly(t *testing.T) {
	arrived := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		<-r.Context().Done() // 客户端取消前不写响应——Do 因此阻塞在等响应头
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		_, err := NewHTTPGetTool(&spyPolicy{allowed: []string{"127.0.0.1"}}).Execute(
			ctx, httpToolEnv(), jsonInput(t, map[string]any{"url": server.URL + "/slow"}))
		done <- err
	}()

	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("请求未在限时内到达测试服务")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled, "取消传播到工具侧（执行器据此归 timeout）")
	case <-time.After(2 * time.Second):
		t.Fatal("取消后未在限时内返回：请求没有跟随 ctx")
	}
}

// —— 关键回归 R1~R8（颗粒度文档 §4.3 代码原样落地；R5 / R6 在 shelltools_test.go）——

// TestHttpGet_PerHopCheckPrecedesEverySend：契约 §5.3「逐跳校验——每一跳发送前过白名单与
// scheme，客户端不得自行跟随」。判据取最严形态：第 2 跳的目标被策略拒 → 假 doer 记录的
// 请求次数必须**恰好是 1**（拒的那一跳不得发出），且错误是四段式沙箱拒绝
// （执行器按类型分类 ⇒ IsSandboxViolation 即 error_kind=sandbox_denied 的判据）。
func TestHttpGet_PerHopCheckPrecedesEverySend(t *testing.T) {
	doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
		return redirectResponse(301, "http://evil.example.com/next"), nil // 第一跳：3xx 指到白名单外
	}}
	policy := &spyPolicy{allowed: []string{"api.example.com"}} // 只放行 api.example.com
	httpGet := newHTTPTool(policy, doer)

	_, err := httpGet.Execute(context.Background(),
		core.ToolEnv{MaxOutputBytes: 256 * 1024}, []byte(`{"url":"http://api.example.com/weather"}`))

	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err), "越界跳必须判 sandbox_denied（不是 external_error）")
	require.Contains(t, err.Error(), "evil.example.com", "四段式第 2 段点名越界目标")
	require.Equal(t, 1, doer.count(), "被拒的那一跳不得发出——请求次数必须恰好 1")
	require.Equal(t, []string{"http://api.example.com/weather", "http://evil.example.com/next"}, policy.targets(),
		"每一跳发送前各判一次（顺序即发送顺序）")
}

// TestHttpGet_RedirectHopLimitIsFive：契约 §5.3「跳数超过 5」归 sandbox_denied（不是
// external_error）；技术方案 §4.3 授权矩阵同款。口径（§6.2 固定口径 4）：hop 计"已跟随的
// 重定向次数"，初始请求不计——5 跳跟完后第 6 个 3xx 即拒，故 doer 恰收到 6 次请求。
func TestHttpGet_RedirectHopLimitIsFive(t *testing.T) {
	n := 0
	doer := &fakeDoer{respond: func(req *http.Request) (*http.Response, error) {
		n++
		return redirectResponse(302, req.URL.String()+"/hop"+strconv.Itoa(n)), nil // 永远再跳一次
	}}
	policy := &spyPolicy{allowed: []string{"api.example.com"}} // 每跳都在白名单内

	_, err := newHTTPTool(policy, doer).Execute(context.Background(),
		core.ToolEnv{MaxOutputBytes: 256 * 1024}, []byte(`{"url":"http://api.example.com/weather"}`))

	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err), "超限判 sandbox_denied")
	require.Equal(t, 6, doer.count(), "初始请求 + 5 跳；第 6 个 3xx 被拒（未发出第 7 次请求）")
	require.Equal(t, 0, policy.denials(), "6 次判定都放行了——拒的原因只可能是跳数")
}

// fakeInvocationStore 是两阶段审计的假仓储（实现 core.ToolInvocationStore）：只做 I/O 边界，
// **不**复制执行器的任何逻辑（判卷纪律 ④——再造执行器等于把被测对象换成夹具）。
// The two-phase audit fake: an I/O boundary only, never a second executor.
type fakeInvocationStore struct {
	mu   sync.Mutex
	rows []fakeInvocationRow
}

// fakeInvocationRow 是一条审计行：预写形态 + 终态（含"终态没写进去"这个状态本身）。
type fakeInvocationRow struct {
	inv      core.ToolInvocation
	status   core.LLMCallStatus // 预写恒为 pending；回填成功后转终态
	result   core.ToolInvocationResult
	terminal bool
}

// BeginInvocation 写一行 pending；行号即返回的行 id。
func (s *fakeInvocationStore) BeginInvocation(_ context.Context, inv core.ToolInvocation) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, fakeInvocationRow{inv: inv, status: core.LLMCallPending})
	return int64(len(s.rows)), nil
}

// FinishInvocation 把同一行更新为终态（行 id 越界即错——不许静默吞掉）。
func (s *fakeInvocationStore) FinishInvocation(_ context.Context, id int64, res core.ToolInvocationResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 1 || int(id) > len(s.rows) {
		return fmt.Errorf("tool invocation row %d not found", id)
	}
	s.rows[id-1].status = res.Status
	s.rows[id-1].result = res
	s.rows[id-1].terminal = true
	return nil
}

// TestHTTPChain_ExecutorCapsFinalContent：§6.2 固定口径 7 的完整判卷——首行 `HTTP <status>`
// 的开销**计入**预算；读取上限 = min(env.MaxOutputBytes, 256 KB) 且工具多读 1 字节，执行器
// 的 `len(content) > maxBytes` 判据因此**必定命中**；标记恰一次、N = 生效上限、中文按 rune
// 边界回退、空体只有首行。链路 = 真工具 → **真实 core.ToolExecutor** → 假审计仓储
// （仓储只是 I/O 边界——判卷纪律 ④：不得再造一个执行器）。
func TestHTTPChain_ExecutorCapsFinalContent(t *testing.T) {
	const capBytes = 64
	statusLine := "HTTP 200 OK\n" // 开销常量：等于上限的用例把它算进去，差 1 字节必现形
	cases := []struct {
		name          string
		body          string
		wantTruncated bool
	}{
		{"小于上限", strings.Repeat("a", 16), false},
		{"等于上限", strings.Repeat("a", capBytes-len(statusLine)), false}, // 状态行计入 ⇒ 恰为上限
		{"超过上限一字节", strings.Repeat("a", capBytes-len(statusLine)+1), true},
		{"中文截断回退", strings.Repeat("地", 100), true}, // 3 字节/字：不得劈出半个 rune
		{"空体只有首行", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			httpGet := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}},
				&fakeDoer{respond: okResponder(200, tc.body)})
			registry := NewRegistry(slog.New(slog.DiscardHandler))
			require.NoError(t, registry.Register(httpGet))
			audit := &fakeInvocationStore{}

			res := core.NewToolExecutor(registry, audit, nil).Execute(context.Background(),
				core.ToolEnv{RunID: "cli-test-run", MaxOutputBytes: capBytes},
				[]string{"http_get"},
				core.ToolCall{
					ID: "call_1", Name: "http_get",
					Arguments: `{"url":"http://api.example.com/x"}`,
				})

			require.Len(t, audit.rows, 1, "一条审计行（预写 + 终态回填各一次）")
			var out struct {
				Content   string `json:"content"`
				Truncated bool   `json:"truncated"`
				Retryable bool   `json:"retryable"`
			}
			require.NoError(t, json.Unmarshal(audit.rows[0].result.ResultJSON, &out))

			require.Equal(t, out.Content, res.Content,
				"返回给循环的内容与落库值同一份（tool_exec.go:129-137 同一 content 变量）")
			require.Equal(t, tc.wantTruncated, out.Truncated)
			require.True(t, strings.HasPrefix(out.Content, statusLine), "首行是响应状态行")
			marker := core.TruncationMarker(capBytes) // N = 生效上限
			if tc.wantTruncated {
				require.Equal(t, 1, strings.Count(out.Content, marker), "标记恰一次")
				require.True(t, utf8.ValidString(out.Content), "截断不得劈出半个 rune")
				require.LessOrEqual(t, len(strings.TrimSuffix(out.Content, marker)), capBytes,
					"截断后的前缀不超过生效上限——中文用例会在此回退到 rune 边界")
			} else {
				require.NotContains(t, out.Content, marker)
				require.LessOrEqual(t, len(out.Content), capBytes)
			}
		})
	}
}

// TestHTTPGet_CancelDuringBodyReadReturnsPromptly：取消传播在工具侧的形状——请求已发出后
// 取消，Execute 必须在限时内返回**取消错误**（评审 P2-6 修正：阻塞体在 ctx.Done 时返回
// io.EOF 会被 ReadAll 读成"读完了"、取消被静默吞掉——夹具必须走与真实网络一致的形态：
// 返回 ctx.Err()），且**不得**留后台 goroutine 继续读响应体（宪法 VII 同步执行）。
// 断言以限时等待为主（goroutine 总数只作辅助观察）；等"已发出"信号也设超时。
func TestHTTPGet_CancelDuringBodyReadReturnsPromptly(t *testing.T) {
	started := make(chan struct{})
	doer := &fakeDoer{respond: func(req *http.Request) (*http.Response, error) {
		close(started)
		return &http.Response{
			Status: "200 OK", StatusCode: 200,
			Body: io.NopCloser(cancelAwareReader{ctx: req.Context()}), // 读到 ctx.Done 时**返回 ctx.Err()**
		}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // 清理注册：用例失败提前退出也不悬挂

	done := make(chan error, 1)
	go func() {
		_, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
			ctx, core.ToolEnv{MaxOutputBytes: 64}, []byte(`{"url":"http://api.example.com/x"}`))
		done <- err
	}()

	select {
	case <-started: // 等"请求已发出"信号**再**取消——先取消会把用例变成"没发出去"，判不出泄漏
	case <-time.After(2 * time.Second):
		t.Fatal("请求未在限时内发出")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled, "错误链保留 ctx 身份（执行器据此归 timeout）")
	case <-time.After(2 * time.Second):
		t.Fatal("取消后未在限时内返回：响应体读取没有跟随 ctx")
	}
}

// TestHTTPWithRealClient_DrivesHopsAndIgnoresEnvProxy：假 doer 绕过真实 Client / Transport，
// 两条行为只能在真链路上判——① 重定向由**逐跳循环**驱动而不是 Client 自行跟随（CheckRedirect
// 返回 ErrUseLastResponse，302 的相对 Location 经 ResolveReference 后由工具再判再发）；
// ② Transport.Proxy=nil：环境里有 HTTP_PROXY 也不读（技术方案 §4.3 授权矩阵 HTTP 行）。
// 夹具策略直接放行——本用例的判卷面是**客户端行为**；白名单与端口判定归 policy 用例。
func TestHTTPWithRealClient_DrivesHopsAndIgnoresEnvProxy(t *testing.T) {
	var mu sync.Mutex
	var hops []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hops = append(hops, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/second" {
			_, _ = io.WriteString(w, "weather: sunny")
			return
		}
		http.Redirect(w, r, "/second", http.StatusFound) // 302 + **相对** Location
	}))
	t.Cleanup(server.Close)

	// 不可达代理：Transport 若读环境代理变量，请求必失败——成功即证明 Proxy=nil 生效
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	httpGet := NewHTTPGetTool(&spyPolicy{allowed: []string{"127.0.0.1"}}) // 生产装配：真实 newHTTPClient
	res, err := httpGet.Execute(context.Background(),
		core.ToolEnv{MaxOutputBytes: 256 * 1024}, []byte(`{"url":"`+server.URL+`/first"}`))

	require.NoError(t, err)
	require.Contains(t, res.Content, "HTTP 200 OK")
	require.Contains(t, res.Content, "weather: sunny")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/first", "/second"}, hops, "两跳都由工具驱动（Client 不自行跟随）")
}

// TestHttpGet_RedirectToNonDefaultPortIsRefusedBeforeSend：口径 15（2026-10-17 用户裁决）——
// 端口判定与域名判定同源同判：初始请求与**每一次重定向**发送前都过 policy，白名单域名跳到
// 非默认端口同样拒、且拒在**发送之前**。分工写清：域名白名单管 host、**不管端口**——本条
// 即"host 在白名单内、端口不在默认档"的组合。夹具用**真 WhitelistPolicy**（端口规则落在
// 策略层，假策略测不到它）× 假 doer。
func TestHttpGet_RedirectToNonDefaultPortIsRefusedBeforeSend(t *testing.T) {
	doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
		// 第一跳：白名单域名内的 3xx，Location 指到**同一 host 的非默认端口**
		return redirectResponse(302, "http://api.example.com:8080/next"), nil
	}}
	policy := NewWhitelistPolicy(PolicyOptions{
		Root:           t.TempDir(),
		AllowedDomains: []string{"api.example.com"}, // host 命中白名单，端口不在默认档
	})
	httpGet := newHTTPTool(policy, doer)

	_, err := httpGet.Execute(context.Background(),
		core.ToolEnv{MaxOutputBytes: 256 * 1024}, []byte(`{"url":"http://api.example.com/weather"}`))

	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err), "非默认端口判 sandbox_denied（不是 external_error）")
	require.Contains(t, err.Error(), "api.example.com:8080", "四段式第 2 段点名目标 host 与端口（脱敏后不含 userinfo / query）")
	require.Contains(t, err.Error(), "当前版本不支持非默认端口", "第 4 段指引固定写这个原因")
	require.NotContains(t, err.Error(), "http.allowed_domains",
		"指引不得提示\"加入域名白名单即可解决\"——域名白名单管 host、不管端口")
	require.Equal(t, 1, doer.count(), "目标请求零发送——被拒的那一跳必须没发出（初始请求才是 1 次）")
}

// —— 第四轮：用户实测回归（2026-10-17，4 条） ——
//
// 用户以真实 http.Client 探针复现的四条边界（1 个 P1 + 3 个 P2）：① 调用方 ctx 仍有效、
// 底层错误自带截止身份时，包装错误直通让 URL 查询参数经错误文案外泄；② `body: null`
// 被两层放行、POST 照发；③ 空串 body 的 POST 丢 Content-Type；④ 转 GET 漏清
// `Content-Encoding`。四条各配一条回归，判据与探针同锚点。
// The four user-probed edges: identity passthrough leaking a query, null bodies being
// sent, empty-body POSTs losing their content type, and gzip claims surviving a turn
// into GET.

// TestHTTP_InternalDeadlineNeverLeaksURLText：P1（2026-10-17 用户实测）——调用方 ctx 仍
// 有效、底层错误自带截止身份时（如传输层内部超时），上抛的必须是**身份哨兵本体**：
// `errors.Is` 分类照旧（执行器据此归 timeout），而 `url.Error` 原文里的整条 URL（含
// query）**不得**进入错误文案——它随后就是模型可见内容、WARN 日志与审计 error_message。
func TestHTTP_InternalDeadlineNeverLeaksURLText(t *testing.T) {
	const marker = "token=leak-marker"
	// 传输层内部超时：调用方 ctx 未结束，截止身份在错误的包装层里
	leaky := &url.Error{
		Op:  "Get",
		URL: "http://api.example.com/weather?" + marker,
		Err: context.DeadlineExceeded,
	}

	cases := []struct {
		name string
		doer *fakeDoer
	}{
		{
			"发送错误点",
			&fakeDoer{respond: func(*http.Request) (*http.Response, error) { return nil, leaky }},
		},
		{
			"响应读取错误点",
			&fakeDoer{respond: func(*http.Request) (*http.Response, error) {
				resp := statusResponse(http.StatusOK, "")
				resp.Body = io.NopCloser(errorReader{err: leaky})
				return resp, nil
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, tc.doer).Execute(
				context.Background(), httpToolEnv(),
				jsonInput(t, map[string]any{"url": "http://api.example.com/weather?" + marker}))
			require.ErrorIs(t, err, context.DeadlineExceeded, "身份保留：执行器据此归 timeout")
			require.NotContains(t, err.Error(), marker, "查询参数不得出现在错误文案里")
			require.NotContains(t, err.Error(), "http://", "url.Error 原文不得直通")
			require.False(t, core.IsExternalError(err), "截止身份优先，不得被包装成外部系统错误")
		})
	}
}

// TestHttpPost_NullBodyIsInvalidInputBeforeSend：P2（2026-10-17 用户实测）——JSON
// `null` 解进 string 字段不留痕迹、执行器的必填键检查又只看键存在，两层放行会让
// `body: null` 的 POST 照常发出。空串是"无正文"的合法写法、`null` 不是：判
// `invalid_input`、**零发送**。
func TestHttpPost_NullBodyIsInvalidInputBeforeSend(t *testing.T) {
	doer := &fakeDoer{respond: okResponder(http.StatusOK, "ok")}
	post := newHTTPPostTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer)

	_, err := post.Execute(context.Background(), httpToolEnv(),
		[]byte(`{"url":"http://api.example.com/submit","body":null}`))

	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err), "null body 归入参非法（不是运行时错误）")
	require.Equal(t, 0, doer.count(), "入参非法发生在发送之前——零发送")

	// 对照：同一形态的空串 body 合法且照发（必填 ≠ 非空）
	controlDoer := &fakeDoer{respond: okResponder(http.StatusOK, "ok")}
	control := newHTTPPostTool(&spyPolicy{allowed: []string{"api.example.com"}}, controlDoer)
	res, err := control.Execute(context.Background(), httpToolEnv(),
		[]byte(`{"url":"http://api.example.com/submit","body":""}`))
	require.NoError(t, err)
	require.Equal(t, 1, controlDoer.count(), "空串 body 是合法入参、照常发出")
	require.Equal(t, http.MethodPost, controlDoer.recorded()[0].Method)
	require.Equal(t, "HTTP 200 OK\nok", res.Content)
}

// TestHttpPost_EmptyBodyStillSetsContentType：P2（2026-10-17 用户实测）——空串 body 的
// POST 仍是一次**携带正文语义**的请求：缺省与显式 `content_type` 都必须进入请求头
// （漏设会让服务端按自己的猜测解释正文）；转 GET 的跳不设（见下方转 GET 用例）。
func TestHttpPost_EmptyBodyStillSetsContentType(t *testing.T) {
	cases := []struct {
		name  string
		ctype string
		want  string
	}{
		{"缺省 application/json", "", "application/json"},
		{"显式覆盖", "text/plain", "text/plain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(http.StatusOK, "ok")}
			fields := map[string]any{"url": "http://api.example.com/submit", "body": ""}
			if tc.ctype != "" {
				fields["content_type"] = tc.ctype
			}
			_, err := newHTTPPostTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
				context.Background(), httpToolEnv(), jsonInput(t, fields))
			require.NoError(t, err)
			first := doer.recorded()[0]
			require.Equal(t, tc.want, first.Header.Get("Content-Type"), "空串 body 也带正文类型")
			require.Zero(t, first.ContentLength, "无正文：长度为 0")
		})
	}
}

// TestHTTP_PostToGetRedirectClearsEncodingHeader：P2（2026-10-17 用户实测）——约束 30 的
// 正文头清单是三个（`Content-Type` / `Content-Length` / `Content-Encoding`）：301 / 302 /
// 303 转 GET 后漏清 `Content-Encoding`，会让已无正文的 GET 仍宣称"以下内容按 gzip
// 解释"。判据覆盖三种状态码与**大小写变体**（模型给的键名大小写任意）。
func TestHTTP_PostToGetRedirectClearsEncodingHeader(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			doer := &fakeDoer{respond: func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost {
					return redirectResponse(status, "http://api.example.com/result"), nil
				}
				return statusResponse(http.StatusOK, "ok"), nil
			}}
			post := newHTTPPostTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer)
			_, err := post.Execute(context.Background(), httpToolEnv(), jsonInput(t, map[string]any{
				"url":  "http://api.example.com/submit",
				"body": `{"a":1}`,
				"headers": map[string]string{
					"content-type":     "text/csv", // 大小写变体：清单比对必须大小写不敏感
					"content-length":   "7",
					"content-encoding": "gzip",
					"X-Keep":           "yes",
				},
			}))
			require.NoError(t, err)
			require.Equal(t, 2, doer.count())

			first := doer.recorded()[0]
			require.Equal(t, "gzip", first.Header.Get("Content-Encoding"), "首跳照带模型头（对照）")

			second := doer.recorded()[1]
			require.Equal(t, http.MethodGet, second.Method)
			require.Empty(t, second.Header.Get("Content-Encoding"), "转 GET 后不得再宣称正文编码")
			require.Empty(t, second.Header.Get("Content-Type"), "转 GET 后不得再带正文类型")
			require.Empty(t, second.Header.Get("Content-Length"))
			require.Zero(t, second.ContentLength)
			require.Equal(t, "yes", second.Header.Get("X-Keep"), "非正文头照常转发（同源）")
		})
	}
}
