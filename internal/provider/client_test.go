package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/testutil"
)

// maxResponseBodyBytesForTest 与 provider 包的响应体读取上限**同值**（4 MiB）：包外测试看不见
// 未导出的常量，故按字面量钉住；上限一旦被调大而本值未同步，超限用例会立刻判红——这正是想要的
// 耦合（常量改动必须连同判据一起改，不得静默放宽）。
// maxResponseBodyBytesForTest mirrors the provider package's read cap; the oversize case is
// red the moment the cap is raised without updating it.
const maxResponseBodyBytesForTest = 4 << 20

// TestOpenAIClient_TranslatesRequestToOpenAIProtocol：协议翻译的出站判据——假服务断言
// 收到的请求：`model` / `messages` 顺序与内容 / `stream` **不得为 true**。
// TestOpenAIClient_TranslatesRequestToOpenAIProtocol pins the outbound request: model,
// message order and content, and a stream flag that must not be true.
func TestOpenAIClient_TranslatesRequestToOpenAIProtocol(t *testing.T) {
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		Body: `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
	})
	client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: srv.URL(), APIKey: "k"})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), core.ChatRequest{Model: "m", Messages: []core.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
	}})
	require.NoError(t, err)

	bodies := srv.Requests()
	require.Len(t, bodies, 1)
	sent := bodies[0]

	require.Equal(t, "m", sent["model"])
	msgs, ok := sent["messages"].([]any)
	require.True(t, ok, "messages 必须是数组，实际 %T", sent["messages"])
	require.Len(t, msgs, 2, "条数与顺序 = 传入消息")
	first := msgs[0].(map[string]any)
	second := msgs[1].(map[string]any)
	require.Equal(t, "system", first["role"])
	require.Equal(t, "be brief", first["content"])
	require.Equal(t, "user", second["role"])
	require.Equal(t, "hi", second["content"])
	require.NotEqual(t, true, sent["stream"], "出站不得请求流式（本 US 无流式；契约 §3.3）")
}

// captureRawRequests 起一个只记录**原始请求体**的回环服务并返回读取器；第 n 次请求回放
// 第 n 份响应体（脚本用尽则复用最后一份——两跳用例的末尾几跳没有响应面断言）。
// 为什么不用假服务的解码视图：信封用例的判据是"字面键出现几次"、大整数用例的判据是
// "字面数字原样回发"，解码视图会把重复键折叠、把数字读成 float64——用它判卷恰好抹掉
// 这两条要防的失效。故另用一份原样捕获。
// captureRawRequests starts a loopback service that records raw request bodies only.
// The decoded view collapses duplicate keys and float64-izes numbers, masking exactly
// the failures these cases guard.
func captureRawRequests(t *testing.T, responseBodies ...string) (baseURL string, read func() []string) {
	t.Helper()
	require.NotEmpty(t, responseBodies, "至少给一份响应体")
	var (
		mu     sync.Mutex
		bodies []string
	)
	srv := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("读请求体失败: %v", readErr) // 处理器在另一个 goroutine 里跑——只记错，不 FailNow
		}
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, string(raw))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if _, writeErr := io.WriteString(w, responseBodies[min(n, len(responseBodies)-1)]); writeErr != nil {
			t.Errorf("写响应失败: %v", writeErr)
		}
	}))
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(bodies))
		copy(out, bodies)
		return out
	}
}

// TestOpenAIClient_RequestEnvelopeKeepsSingleMessagesKey：信封构造——出站体里 `"messages":`
// **只出现一次**，条数与顺序 = 传入消息（防"已知字段基座把我们的 messages 顶掉"）。
// 判据取**原始请求体**：解码成映射会把重复键折叠掉，看不出顶替。
// TestOpenAIClient_RequestEnvelopeKeepsSingleMessagesKey counts the literal messages key
// in the raw body: decoding collapses duplicates, which is exactly what it guards.
func TestOpenAIClient_RequestEnvelopeKeepsSingleMessagesKey(t *testing.T) {
	base, rawBodies := captureRawRequests(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: base, APIKey: "k"})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), core.ChatRequest{Model: "m", Messages: []core.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"},
	}})
	require.NoError(t, err)

	raw := rawBodies()
	require.Len(t, raw, 1)
	require.Equal(t, 1, strings.Count(raw[0], `"messages":`), `出站体里 "messages" 只允许出现一次：%s`, raw[0])

	// 归属校验：那个唯一的 messages 是我们构造的（条数与顺序都对），不是基座留下的空值。
	var sent map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw[0]), &sent))
	msgs, ok := sent["messages"].([]any)
	require.True(t, ok, "messages 必须是数组，实际 %T", sent["messages"])
	require.Len(t, msgs, 3)
	require.Equal(t, "one", msgs[0].(map[string]any)["content"])
	require.Equal(t, "three", msgs[2].(map[string]any)["content"])
}

// TestOpenAIClient_HTTPErrorMapsToExternalError：错误模型响应——4xx / 5xx / 3xx（不跟随）/
// 畸形 JSON → `external_error`（不 panic）；3xx 断言**未**跟到 Location（假服务对脚本外
// 请求即判失败，跟随会被当场抓住）。
// TestOpenAIClient_HTTPErrorMapsToExternalError: every non-2xx path and malformed JSON
// surface as external_error, and a 3xx is never followed.
func TestOpenAIClient_HTTPErrorMapsToExternalError(t *testing.T) {
	cases := []struct {
		name     string
		response testutil.FakeOpenAIResponse
		want     int
	}{
		{name: "4xx", response: testutil.FakeOpenAIResponse{
			StatusCode: http.StatusUnauthorized,
			Body:       `{"error":{"message":"bad key","type":"invalid_request_error"}}`,
		}, want: http.StatusUnauthorized},
		{name: "5xx", response: testutil.FakeOpenAIResponse{
			StatusCode: http.StatusBadGateway,
			Body:       `{"error":{"message":"upstream boom","type":"server_error"}}`,
		}, want: http.StatusBadGateway},
		{name: "3xx 不跟随", response: testutil.FakeOpenAIResponse{
			StatusCode: http.StatusFound,
			Body:       "",
		}, want: http.StatusFound},
		{name: "畸形 JSON", response: testutil.FakeOpenAIResponse{Body: `{"choices":[`}, want: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testutil.NewFakeOpenAIServer(t, tc.response)
			client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: srv.URL(), APIKey: "k"})
			require.NoError(t, err)

			_, err = client.Chat(context.Background(), core.ChatRequest{
				Model:    "m",
				Messages: []core.Message{{Role: "user", Content: "hi"}},
			})

			var callErr *provider.CallError
			require.ErrorAs(t, err, &callErr, "错误必须是 CallError 形态")
			require.Equal(t, core.ErrorKindExternalError, callErr.Kind)
			require.Equal(t, tc.want, callErr.Status)
			require.NotEmpty(t, callErr.Error())
			require.Len(t, srv.Requests(), 1, "不得重试、不得跟随重定向")
		})
	}
}

// TestOpenAIClient_ContextCancelStopsCall：慢响应 + ctx 取消 → 立即返回，
// `errors.Is(err, context.Canceled)` 成立（链未断）。
// TestOpenAIClient_ContextCancelStopsCall keeps the cancellation chain intact.
func TestOpenAIClient_ContextCancelStopsCall(t *testing.T) {
	release := make(chan struct{})
	srv := testutil.NewHTTPServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // 一直挂着，直到客户端取消
		<-release
	}))
	t.Cleanup(func() { close(release) })

	client, err := provider.NewOpenAIClient(provider.Options{Name: "slow", BaseURL: srv.URL, APIKey: "k"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { cancel() }()

	_, err = client.Chat(ctx, core.ChatRequest{Model: "m", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	require.ErrorIs(t, err, context.Canceled, "取消错误链必须保留")

	var callErr *provider.CallError
	require.ErrorAs(t, err, &callErr)
	require.Equal(t, core.ErrorKindTimeout, callErr.Kind, "取消归并记 timeout")
	require.Zero(t, callErr.Status)
}

// TestOpenAIClient_NoCredentialInErrorMessages：凭证纪律——构造失败 / 传输失败 /
// **服务器回显 key** 的错误文案均不含 key 值，且不含请求 URL 的 query。
// TestOpenAIClient_NoCredentialInErrorMessages: no path leaks the credential or the
// request URL into the error text.
func TestOpenAIClient_NoCredentialInErrorMessages(t *testing.T) {
	const key = "sk-synthetic-echo-probe"

	t.Run("构造失败", func(t *testing.T) {
		raw := "https://user-x9:" + key + "@api.example.com/v1"
		_, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: raw, APIKey: key})
		require.Error(t, err)
		require.NotContains(t, err.Error(), key, "构造失败文案不得回显 key（它在 userinfo 里）")
		require.NotContains(t, err.Error(), raw, "不得回显原 URL")
	})

	t.Run("传输失败", func(t *testing.T) {
		client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: "http://127.0.0.1:1", APIKey: key})
		require.NoError(t, err)

		_, err = client.Chat(context.Background(), core.ChatRequest{
			Model:    "m",
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		})
		require.Error(t, err)
		require.NotContains(t, err.Error(), key)
		require.NotContains(t, err.Error(), "127.0.0.1:1", "错误文案不得携带请求 URL")

		var callErr *provider.CallError
		require.ErrorAs(t, err, &callErr)
		require.Equal(t, core.ErrorKindExternalError, callErr.Kind, "DNS / 连接失败归 external_error")
	})

	t.Run("服务器回显 key", func(t *testing.T) {
		srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
			StatusCode: http.StatusUnauthorized,
			Body:       `{"error":{"message":"invalid key: ` + key + `","type":"invalid_request_error"}}`,
		})
		client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: srv.URL(), APIKey: key})
		require.NoError(t, err)

		_, err = client.Chat(context.Background(), core.ChatRequest{
			Model:    "m",
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		})
		require.Error(t, err)
		require.NotContains(t, err.Error(), key, "供应商回显的 key 必须在出口前被抹除")
		require.NotContains(t, err.Error(), srv.URL(), "错误文案不得携带请求 URL")
	})
}

// TestOpenAIClient_RejectsBaseURLWithUnsupportedParts：§2.4 裁决注 9 的**构造期防御校验**：
// userinfo / query / fragment / 缺 scheme / 非 http(s) 方案 → 构造失败；文案点名键路径与成分，
// **不含原 URL 与其中的值**（`url.Parse` 的错误不得直传）。
// 空分隔符同拒（P2 回归）：尾随的 `#` / `?` 解析后各成分字段是**空串**，只看成分字段会漏，
// 而 `…/v1#` 会让拼接的端点整段落进 fragment——请求静默打到 base path。
// TestOpenAIClient_RejectsBaseURLWithUnsupportedParts: the constructor re-applies the
// same base_url rule as the loader, without echoing the URL or its values; a bare
// trailing `#` or `?` is rejected as well, since both leave the component fields empty.
func TestOpenAIClient_RejectsBaseURLWithUnsupportedParts(t *testing.T) {
	cases := []struct {
		name      string
		baseURL   string
		component string
		sensitive string
	}{
		{name: "userinfo", baseURL: "https://user-x9:secret-7f@api.example.com/v1", component: "userinfo", sensitive: "secret-7f"},
		{name: "query", baseURL: "https://api.example.com/v1?route=9f3a", component: "query", sensitive: "route=9f3a"},
		{name: "fragment", baseURL: "https://api.example.com/v1#alt-2b", component: "fragment", sensitive: "alt-2b"},
		{name: "空 fragment（尾随 #）", baseURL: "https://api.example.com/marker-a1f#", component: "fragment", sensitive: "marker-a1f"},
		{name: "空 query（尾随 ?）", baseURL: "https://api.example.com/marker-b2e?", component: "query", sensitive: "marker-b2e"},
		{name: "缺 scheme", baseURL: "api.example.com/v1", component: "scheme", sensitive: "api.example.com"},
		{name: "非 http(s) 方案", baseURL: "ftp://api.example.com/v1", component: "scheme", sensitive: "ftp://"},
		{name: "空值", baseURL: "", component: "base_url", sensitive: ""},
		{name: "opaque 形态", baseURL: "http:api.example.com/v1", component: "host", sensitive: "api.example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: tc.baseURL, APIKey: "k"})

			require.Error(t, err, "不支持的地址成分必须拒绝（不静默裁剪）")
			require.Contains(t, err.Error(), "base_url", "必须点名键")
			require.Contains(t, err.Error(), tc.component, "必须点出不支持的成分")
			if tc.baseURL != "" {
				require.NotContains(t, err.Error(), tc.baseURL, "不得回显原 URL")
			}
			if tc.sensitive != "" {
				require.NotContains(t, err.Error(), tc.sensitive, "不得回显其中的值")
			}
		})
	}

	t.Run("合法形态原样保留（含 base path）", func(t *testing.T) {
		var (
			mu    sync.Mutex
			paths []string
		)
		srv := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			if _, err := io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`); err != nil {
				t.Errorf("写响应失败: %v", err) // 处理器在另一个 goroutine 里跑——只记错，不 FailNow
			}
		}))
		client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: srv.URL + "/v1", APIKey: "k"})
		require.NoError(t, err, "scheme + host + path 三部分是支持面")

		_, err = client.Chat(context.Background(), core.ChatRequest{
			Model:    "m",
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		})
		require.NoError(t, err)

		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, []string{"/v1/chat/completions"}, paths,
			"base path 原样保留，端点只接在其后（不覆盖、不丢弃）")
	})
}

// TestOpenAIClient_RoundTripsUnknownFieldsTwoHops —— 技术方案 §4.4 约束 3 的验收（两跳，P1-1）。
// 缺一不可：① 假服务在响应消息里放一个库不认识的字段；② 把解析结果交回客户端发第二次调用，
// 假服务收到的第二个请求体**必须仍含该字段**。只对 core.Message 做一次 JSON 往返检不出
// "库的编解码把未知键丢了"——那正是本条要防的失效。
func TestOpenAIClient_RoundTripsUnknownFieldsTwoHops(t *testing.T) {
	ctx := context.Background()
	const unknown = "vendor_thought_signature"
	srv := testutil.NewFakeOpenAIServer(t,
		testutil.FakeOpenAIResponse{Body: `{"choices":[{"message":{"role":"assistant","content":"ok",` +
			`"reasoning_content":"think","` + unknown + `":"sig-1"}}],"usage":{"total_tokens":3}}`},
		testutil.FakeOpenAIResponse{Body: `{"choices":[{"message":{"role":"assistant","content":"ok2"}}]}`})

	client, err := provider.NewOpenAIClient(provider.Options{Name: "vendor", BaseURL: srv.URL(), APIKey: "k"})
	require.NoError(t, err)

	// 第一跳：未知字段进 ProviderExtra；reasoning_content 是已知字段，走显式列。
	resp, err := client.Chat(ctx, core.ChatRequest{
		Model:    "m",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)
	require.Equal(t, "think", resp.ReasoningContent)
	require.Equal(t, "sig-1", resp.ProviderExtra[unknown])

	// 第二跳：把该消息交回——请求体里必须仍带这个键（原名、原值）。
	_, err = client.Chat(ctx, core.ChatRequest{Model: "m", Messages: []core.Message{
		{
			Role: "assistant", Content: "ok", ReasoningContent: resp.ReasoningContent,
			ProviderExtra: resp.ProviderExtra,
		},
	}})
	require.NoError(t, err)

	bodies := srv.Requests()
	require.Len(t, bodies, 2)
	sent := bodies[1]["messages"].([]any)[0].(map[string]any)
	require.Equal(t, "sig-1", sent[unknown], "未知字段必须在第二次请求里原样回发")
	require.Equal(t, "think", sent["reasoning_content"], "已知扩展字段（思考内容）同样不得丢")
}

// TestOpenAIClient_RoundTripsBigIntegersTwoHops —— 未知字段里的大整数两跳保真（P1 回归）：
// 默认解码把数字解成 `any` 时变 float64，2^53 以上的整数在第二跳**悄悄改值**
// （9007199254740993 回发成了 …992），嵌套对象与数组同样受影响。
// 判据取**原始出站 JSON**：假服务的解码视图也会把数字读成 float64，用它判卷会掩盖同一个缺陷。
// TestOpenAIClient_RoundTripsBigIntegersTwoHops asserts the raw outbound JSON: decoding on
// the judging side would float64-ize the number again and mask the very defect.
func TestOpenAIClient_RoundTripsBigIntegersTwoHops(t *testing.T) {
	const exact = "9007199254740993" // 2^53 + 1：float64 的相邻可表示值是 …992
	base, rawBodies := captureRawRequests(t,
		`{"choices":[{"message":{"role":"assistant","content":"ok","vendor_counter":{`+
			`"exact":9007199254740993,"nested":{"exact":9007199254740993},"list":[9007199254740993]}}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"ok2"}}]}`)
	client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: base, APIKey: "k"})
	require.NoError(t, err)

	resp, err := client.Chat(context.Background(), core.ChatRequest{
		Model:    "m",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)
	require.Contains(t, resp.ProviderExtra, "vendor_counter", "未知字段必须进透传位")

	_, err = client.Chat(context.Background(), core.ChatRequest{Model: "m", Messages: []core.Message{
		{Role: "assistant", Content: "ok", ProviderExtra: resp.ProviderExtra},
	}})
	require.NoError(t, err)

	raw := rawBodies()
	require.Len(t, raw, 2)
	require.Equal(t, 3, strings.Count(raw[1], exact),
		"三处大整数都必须原样回发（顶层 / 嵌套对象 / 数组）：%s", raw[1])
	require.NotContains(t, raw[1], "9007199254740992",
		"不得退化成 float64 可表示的值（那正是 9007199254740992）")
}

// TestOpenAIClient_AcceptsEncodedHashInPath —— 空 fragment 拒绝的**正例守卫**：被拒的是字面
// `#` 分隔符；路径里编码过的 `%23` 是合法成分，不得连坐。判据取服务端看到的**转义路径**——
// 它同时证明 `%23` 是被当作编码字节发出去的，而不是在本地又被解成了 fragment。
// TestOpenAIClient_AcceptsEncodedHashInPath guards the negative case: an encoded `%23` in
// the path stays legal and goes out on the wire as an escape.
func TestOpenAIClient_AcceptsEncodedHashInPath(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`); err != nil {
			t.Errorf("写响应失败: %v", err)
		}
	}))
	client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: srv.URL + "/v1%23suffix", APIKey: "k"})
	require.NoError(t, err, "%23 是合法编码 path——空 fragment 的修复不得连坐")

	_, err = client.Chat(context.Background(), core.ChatRequest{
		Model:    "m",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/v1%23suffix/chat/completions"}, paths,
		"编码后的 %23 原样上线（转义形态），且端点路径完整")
}

// TestOpenAIClient_RejectsResponseWithoutChoiceMessage —— 200 的响应必须有 message 对象
// （P2 回归）：`{"choices":[{}]}` 与 `{"choices":[{"message":null}]}` 曾被当成功——返回零值
// 响应、error=nil，服务随后会把它写进审计记 success。message 存在但不是对象同样判死。
// 合法形态不连坐：content 为 `null` 且带 tool_calls 的响应仍判成功（工具调用只透传）。
// TestOpenAIClient_RejectsResponseWithoutChoiceMessage: a 200 whose first choice has no
// message object is rejected, while a null-content tool-call response stays legal.
func TestOpenAIClient_RejectsResponseWithoutChoiceMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "choice 为空对象", body: `{"choices":[{}]}`},
		{name: "message 为 null", body: `{"choices":[{"message":null}]}`},
		{name: "message 不是对象", body: `{"choices":[{"message":"text"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{Body: tc.body})
			client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: srv.URL(), APIKey: "k"})
			require.NoError(t, err)

			_, err = client.Chat(context.Background(), core.ChatRequest{
				Model:    "m",
				Messages: []core.Message{{Role: "user", Content: "hi"}},
			})

			var callErr *provider.CallError
			require.ErrorAs(t, err, &callErr, "坏响应必须判死，不得返回零值响应 + error=nil")
			require.Equal(t, core.ErrorKindExternalError, callErr.Kind, "响应不可解析归 external_error")
			require.Equal(t, http.StatusOK, callErr.Status)
		})
	}

	t.Run("content 为 null 且带 tool_calls 仍判成功", func(t *testing.T) {
		srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
			Body: `{"choices":[{"message":{"role":"assistant","content":null,` +
				`"tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}`,
		})
		client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: srv.URL(), APIKey: "k"})
		require.NoError(t, err)

		resp, err := client.Chat(context.Background(), core.ChatRequest{
			Model:    "m",
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		})
		require.NoError(t, err, "工具调用响应的 content 常为 null——不得被 message 校验误伤")
		require.Empty(t, resp.Content)
		require.Len(t, resp.ToolCalls, 1)
		require.Equal(t, "read_file", resp.ToolCalls[0].Name)
	})
}

// TestOpenAIClient_RejectsResponseBodyOverLimit —— 响应体**读取上限**（成功体与错误体走同一条
// 读路径，同判）：超限即安全失败，正文一个字不外带。用例发 8 MiB（上限 4 MiB 的两倍）——
// 上限若被调大或取消，本用例会立刻判红，这正是"上限存在且随代码走"的守卫。
// TestOpenAIClient_RejectsResponseBodyOverLimit sends twice the cap on both the success and
// the error path and expects a redacted, safe failure.
func TestOpenAIClient_RejectsResponseBodyOverLimit(t *testing.T) {
	const oversize = 2 * maxResponseBodyBytesForTest
	cases := []struct {
		name   string
		status int
	}{
		{name: "成功体超限", status: http.StatusOK},
		{name: "错误体超限", status: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				// 客户端读到上限即断开——服务端这里的写失败是**预期事件**（本用例判的是
				// 客户端侧的失败分类与脱敏，不是服务端写成功），故不作断言。
				_, _ = w.Write(bytes.Repeat([]byte("x"), oversize))
			}))
			client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: srv.URL, APIKey: "k"})
			require.NoError(t, err)

			_, err = client.Chat(context.Background(), core.ChatRequest{
				Model:    "m",
				Messages: []core.Message{{Role: "user", Content: "hi"}},
			})

			var callErr *provider.CallError
			require.ErrorAs(t, err, &callErr, "超限必须安全失败，不得把整份正文吃进内存")
			require.Equal(t, core.ErrorKindExternalError, callErr.Kind)
			require.Equal(t, tc.status, callErr.Status, "状态码取自已返回的响应头")
			require.Contains(t, callErr.Message, "读取上限", "文案必须点名上限")
			require.NotContains(t, callErr.Message, "xxxx", "正文一个字不外带")
		})
	}
}

// readPhaseTrigger 是"读取中"回归用例的触发延迟：响应头在毫秒级到达，触发点在这个量级的
// 百倍之后——取消 / 截止因此必然落在**读取正文**阶段（已越过 Do），而不是还在等响应头。
// 这是余量，不是同步手段：真正的判据见用例里的 Status == 200（取自已返回的响应头）。
// readPhaseTrigger is a margin, not a synchronization primitive.
const readPhaseTrigger = 500 * time.Millisecond

// stalledResponseServer 起一个"响应头已返回、正文写了一半就挂住"的回环服务：客户端必然越过
// Do 并阻塞在读取上，直到 ctx 取消 / 截止把它打断（服务端请求上下文随之结束）。
// 被 client_test.go 与 service_test.go 共用（同一包）。
// The server returns headers and half a body, then stalls until the client goes away.
func stalledResponseServer(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	srv := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"choices":[{"message":{"content":"par`); err != nil {
			t.Errorf("假服务写响应失败: %v", err)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done() // 客户端取消 / 截止后，服务端的请求上下文随之结束
		<-release            // 兜底：即使上面没等到，清理阶段也能放开处理器
	}))
	t.Cleanup(func() { close(release) })
	return srv.URL
}

// TestOpenAIClient_CancelOrDeadlineDuringResponseReadStaysTimeout —— P1 回归：失败发生在
// **响应头已返回之后**（客户端已越过 Do、阻塞在读取正文）时，归类口径与传输阶段一致：
// timeout，且错误链保留 Canceled / DeadlineExceeded（归并口径不丢区分）。
// "确在读取阶段"的依据有两条，都在断言里：Status == 200（取自**已返回的响应头**；传输阶段的
// 失败拿不到状态码）与文案「未取完响应」（传输阶段是「未取得响应」）。
func TestOpenAIClient_CancelOrDeadlineDuringResponseReadStaysTimeout(t *testing.T) {
	cases := []struct {
		name    string
		makeCtx func(t *testing.T) context.Context
		wantCtx error
	}{
		{
			name: "读取中取消",
			makeCtx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				timer := time.AfterFunc(readPhaseTrigger, cancel)
				t.Cleanup(func() { timer.Stop(); cancel() })
				return ctx
			},
			wantCtx: context.Canceled,
		},
		{
			name: "读取中截止",
			makeCtx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), readPhaseTrigger)
				t.Cleanup(cancel)
				return ctx
			},
			wantCtx: context.DeadlineExceeded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := provider.NewOpenAIClient(provider.Options{
				Name: "slow", BaseURL: stalledResponseServer(t), APIKey: "k",
			})
			require.NoError(t, err)

			_, err = client.Chat(tc.makeCtx(t), core.ChatRequest{
				Model:    "m",
				Messages: []core.Message{{Role: "user", Content: "hi"}},
			})

			require.ErrorIs(t, err, tc.wantCtx, "错误链必须保留上下文错误（Canceled 与 DeadlineExceeded 区分保留）")
			var callErr *provider.CallError
			require.ErrorAs(t, err, &callErr)
			require.Equal(t, core.ErrorKindTimeout, callErr.Kind,
				"读取阶段的取消 / 截止归并记 timeout——曾误记 external_error（P1 回归）")
			require.Equal(t, http.StatusOK, callErr.Status, "响应头已返回——这条把'读取阶段'钉死")
			require.Contains(t, callErr.Message, "未取完响应", "文案点名阶段，与传输失败（未取得响应）区分")
		})
	}
}

// readFileSchema / listDirSchema 是 U1 用的两份工具 schema：键序**故意不是字典序**
// （`type` 打头、`additionalProperties` 收尾）——逐字透传的实现会原样回发，解析后重序列化
// 的实现会把它排成字典序，本用例因此分得出"RawMessage 透传"与"解析再编"。
// readFileSchema / listDirSchema carry deliberately non-alphabetical key order, so the
// verbatim-passthrough case is distinguishable from a parse-and-re-encode implementation.
const (
	readFileSchema = `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`
	listDirSchema  = `{"type":"object","properties":{"path":{"type":"string"}}}`
)

// TestOpenAIClient_EncodesToolsInOutboundRequest —— 技术方案 §4.2 段 4 + 颗粒度文档 §2.4
// 裁决注 3 的出站判据：带工具池时请求体**恰一个** `tools` 键，形如
// `[{"function":{name,description,parameters},"type":"function"}]`（Function Calling 形态，
// 经 go-openai 类型编解码），按声明顺序排列、schema **逐字**透传。
// TestOpenAIClient_EncodesToolsInOutboundRequest pins the single tools key, its Function
// Calling shape, the declaration order and the byte-for-byte schema passthrough.
func TestOpenAIClient_EncodesToolsInOutboundRequest(t *testing.T) {
	base, rawBodies := captureRawRequests(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: base, APIKey: "k"})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), core.ChatRequest{
		Model:    "m",
		Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
		Tools: []core.ToolSpec{
			{Name: "read_file", Description: "读取工作区内的文件", Parameters: json.RawMessage(readFileSchema)},
			{Name: "list_dir", Description: "列出目录（不递归）", Parameters: json.RawMessage(listDirSchema)},
		},
	})
	require.NoError(t, err)

	raw := rawBodies()
	require.Len(t, raw, 1)
	require.Equal(t, 1, strings.Count(raw[0], `"tools":`),
		`出站体里 "tools" 只允许出现一次：%s`, raw[0])
	require.Contains(t, raw[0], readFileSchema, "参数 schema 逐字透传（键序即判据：解析再编会排成字典序）")
	require.Contains(t, raw[0], listDirSchema)
	require.Less(t, strings.Index(raw[0], `"read_file"`), strings.Index(raw[0], `"list_dir"`),
		"工具按声明顺序排列（不是 map 遍历顺序）")

	var sent map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw[0]), &sent))
	tools, ok := sent["tools"].([]any)
	require.True(t, ok, "tools 必须是数组，实际 %T", sent["tools"])
	require.Len(t, tools, 2)

	first := tools[0].(map[string]any)
	require.Equal(t, "function", first["type"])
	fn, ok := first["function"].(map[string]any)
	require.True(t, ok, "function 必须是对象，实际 %T", first["function"])
	require.Equal(t, "read_file", fn["name"])
	require.Equal(t, "读取工作区内的文件", fn["description"])
	require.Equal(t, []string{"description", "name", "parameters"}, slices.Sorted(maps.Keys(fn)),
		"function 对象只含三要素（不得混入库默认字段，如 strict）")

	var wantSchema any
	require.NoError(t, json.Unmarshal([]byte(readFileSchema), &wantSchema))
	require.Equal(t, wantSchema, fn["parameters"], "parameters = 该工具的 InputSchema")
}

// TestOpenAIClient_OmitsToolsKeyWhenPoolIsEmpty —— **空池不带键 = 旧行为逐字不变**（颗粒度
// 文档 §3.5 改造点 2 的软门禁）：nil 与空切片两种空池形态都不产出 `tools` 键，且两份出站体
// **逐字相同**、整份请求体等于 US-2 以来的字面量（形态一旦被改动，本用例立刻判红）。
// TestOpenAIClient_OmitsToolsKeyWhenPoolIsEmpty keeps the empty-pool body byte-identical to
// what this client has always sent, for both the nil and the empty-slice pool.
func TestOpenAIClient_OmitsToolsKeyWhenPoolIsEmpty(t *testing.T) {
	base, rawBodies := captureRawRequests(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: base, APIKey: "k"})
	require.NoError(t, err)

	msgs := []core.Message{{Role: core.RoleUser, Content: "hi"}}
	_, err = client.Chat(context.Background(), core.ChatRequest{Model: "m", Messages: msgs})
	require.NoError(t, err)
	_, err = client.Chat(context.Background(), core.ChatRequest{Model: "m", Messages: msgs, Tools: []core.ToolSpec{}})
	require.NoError(t, err)

	raw := rawBodies()
	require.Len(t, raw, 2)
	require.NotContains(t, raw[0], `"tools"`, "空池（nil）不得带 tools 键")
	require.NotContains(t, raw[1], `"tools"`, "空池（空切片）不得带 tools 键——判据是长度，不是 nil")
	require.Equal(t, raw[0], raw[1], "两种空池形态的出站体逐字相同")

	// 字面量取自**本机实测**（库基座 ChatCompletionRequest 的 `stream` 无 omitempty，
	// 故恒以 false 出现）：空池请求体 = US-2 以来的字面量，形态变化必须连同本判据复核。
	const legacyBody = `{"messages":[{"content":"hi","role":"user"}],"model":"m","stream":false}`
	require.Equal(t, legacyBody, raw[0],
		"空池请求体 = US-2 以来的字面量；形态变化必须连同本判据一起被审视（库升级同理）")
}

// TestOpenAIClient_ParsesToolCallsAndKeepsReasoningRoundTrip —— 技术方案 §4.4 三条硬约束的
// 客户端侧（颗粒度文档 §4.2 映射表：U3 对号 `tool_call_id` / `reasoning_content`）：入站
// `tool_calls` 映射到 `core.ToolCall`（id / name 显式列、arguments **原样保留为字符串**），
// 两跳之后出站体里 `tool_calls` / `reasoning_content` / `tool_call_id` 逐字仍在——
// 清空思考内容会让供应商返回 400（Compatibility §5.3 判据 3）。
// TestOpenAIClient_ParsesToolCallsAndKeepsReasoningRoundTrip: tool calls parse into the
// explicit fields and all three round-trip fields survive the second hop verbatim.
func TestOpenAIClient_ParsesToolCallsAndKeepsReasoningRoundTrip(t *testing.T) {
	base, rawBodies := captureRawRequests(t,
		`{"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"think",`+
			`"tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file",`+
			`"arguments":"{\"path\":\"notes.txt\"}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: base, APIKey: "k"})
	require.NoError(t, err)

	resp, err := client.Chat(context.Background(), core.ChatRequest{
		Model:    "m",
		Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)
	require.Len(t, resp.ToolCalls, 1)
	require.Equal(t, "call-1", resp.ToolCalls[0].ID)
	require.Equal(t, "read_file", resp.ToolCalls[0].Name)
	require.Equal(t, `{"path":"notes.txt"}`, resp.ToolCalls[0].Arguments,
		"arguments 是字符串，客户端不解析（执行权归 ToolExecutor）")
	require.Equal(t, "think", resp.ReasoningContent)

	// 第二跳：助手消息（含工具调用与思考内容）与工具结果消息一起回发。
	_, err = client.Chat(context.Background(), core.ChatRequest{Model: "m", Messages: []core.Message{
		{
			Role: core.RoleAssistant, Content: resp.Content,
			ToolCalls: resp.ToolCalls, ReasoningContent: resp.ReasoningContent,
		},
		{Role: core.RoleTool, Content: "文件内容：a", ToolCallID: resp.ToolCalls[0].ID},
	}})
	require.NoError(t, err)

	raw := rawBodies()
	require.Len(t, raw, 2)
	require.Equal(t, 1, strings.Count(raw[1], `"tool_calls":`), `只允许一个 tool_calls 键：%s`, raw[1])
	require.Contains(t, raw[1],
		`"tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"notes.txt\"}"}}]`,
		"工具调用三要素（id / name / arguments）逐字回发")
	require.Contains(t, raw[1], `"reasoning_content":"think"`,
		"思考内容必须回传（清空即供应商 400——Compatibility §5.3 判据 3）")
	require.Contains(t, raw[1], `"tool_call_id":"call-1"`,
		"工具结果消息必须带回被回填的那次调用 id")
}

// TestOpenAIClient_RoundTripsVendorExtrasOnToolCallMessage —— 技术方案 §4.4 约束 3 的
// 携带工具调用形态（颗粒度文档 §4.2 映射表：U4 对号 ProviderExtra 往返保真）：供应商扩展
// 字段与 `tool_calls` 同处一条消息时两跳仍逐字；数字按**原始字面量**回发——默认解码把
// 数字变 float64，`1e3` 会写成 `1000`、长小数会被截到 17 位有效数字，UseNumber 才留得住。
// TestOpenAIClient_RoundTripsVendorExtrasOnToolCallMessage asserts the raw outbound JSON:
// the decoded view would float64-ize the numbers and mask the very defect under test.
func TestOpenAIClient_RoundTripsVendorExtrasOnToolCallMessage(t *testing.T) {
	const (
		exponentLiteral = `"retry_after":1e3`
		preciseLiteral  = `"temperature":0.1234567890123456789`
	)
	base, rawBodies := captureRawRequests(t,
		`{"choices":[{"message":{"role":"assistant","content":null,`+
			`"tool_calls":[{"id":"call-7","type":"function","function":{"name":"read_file","arguments":"{}"}}],`+
			`"vendor_sampling":{"temperature":0.1234567890123456789,"retry_after":1e3},`+
			`"vendor_tag":"a b"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	client, err := provider.NewOpenAIClient(provider.Options{Name: "p", BaseURL: base, APIKey: "k"})
	require.NoError(t, err)

	resp, err := client.Chat(context.Background(), core.ChatRequest{
		Model:    "m",
		Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
	})
	require.NoError(t, err)
	require.Contains(t, resp.ProviderExtra, "vendor_sampling", "未知字段必须进透传位")
	require.Equal(t, "a b", resp.ProviderExtra["vendor_tag"])
	require.Len(t, resp.ToolCalls, 1)

	_, err = client.Chat(context.Background(), core.ChatRequest{Model: "m", Messages: []core.Message{{
		Role: core.RoleAssistant, Content: resp.Content,
		ToolCalls: resp.ToolCalls, ProviderExtra: resp.ProviderExtra,
	}}})
	require.NoError(t, err)

	raw := rawBodies()
	require.Len(t, raw, 2)
	require.Contains(t, raw[1], exponentLiteral, "指数写法按原字面量回发（float64 会写成 1000）：%s", raw[1])
	require.NotContains(t, raw[1], "1000", "不得退化成 float64 的可表示值")
	require.Contains(t, raw[1], preciseLiteral, "长小数逐位保真（float64 只留 17 位有效数字）")
	require.Contains(t, raw[1], `"vendor_tag":"a b"`)
	require.Contains(t, raw[1], `"name":"read_file"`,
		"扩展字段与工具调用同处一条消息时互不顶替（额外键不得盖掉已知字段）")
}

// 编译期断言：客户端形态与本 US 的契约面一致（用例经 NewOpenAIClient 构造，此处再钉一次）。
var (
	_ core.ProviderClient = (*provider.OpenAIClient)(nil)
	_ error               = (*provider.CallError)(nil)
)
