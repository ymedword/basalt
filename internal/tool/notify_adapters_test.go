package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/testutil"
)

// TestWebhookNotifyAdapter_PostsContentJSON —— A1（约束 13 / R6）：webhook 载荷固定
// `{"content": "…"}`、POST、Content-Type 固定；成功 = HTTP 2xx。
// A1 (R6): the webhook payload, method and content type are fixed; 2xx is success.
func TestWebhookNotifyAdapter_PostsContentJSON(t *testing.T) {
	doer := &fakeDoer{respond: okResponder(200, "")}
	require.NoError(t, newWebhookNotifyAdapter(doer).Send(context.Background(),
		"https://hooks.example.com/services/T1", "每日摘要"))
	require.Equal(t, 1, doer.count())
	req := doer.recorded()[0]
	require.Equal(t, http.MethodPost, req.Method)
	require.Equal(t, "application/json; charset=utf-8", req.Header.Get("Content-Type"))
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.JSONEq(t, `{"content":"每日摘要"}`, string(body))
}

// TestWebhookNotifyAdapter_Non2xxIsExternalError —— A2（约束 13 / R6）：3xx / 4xx / 5xx
// 一律 external_error，文案只含状态码与 `scheme://host`，path 不进可观测面。
// A2 (R6): every non-2xx status is an external error naming only status and host.
func TestWebhookNotifyAdapter_Non2xxIsExternalError(t *testing.T) {
	for _, status := range []int{302, 400, 500} {
		err := newWebhookNotifyAdapter(&fakeDoer{respond: okResponder(status, "")}).
			Send(context.Background(), "https://hooks.example.com/T", "x")
		require.True(t, core.IsExternalError(err), "status=%d", status)
		require.Contains(t, err.Error(), strconv.Itoa(status))
		require.Contains(t, err.Error(), "https://hooks.example.com", "错误只出 scheme://host")
		require.NotContains(t, err.Error(), "/T", "path 不进可观测面")
	}
}

// TestWebhookNotifyAdapter_DoesNotFollowRedirects —— A3（约束 17 / R8）：生产客户端
// 不自行跟随重定向；第二跳一次都不发出，3xx 作为失败交回。
// A3 (R8): the production client never follows a redirect; the hop is never sent.
func TestWebhookNotifyAdapter_DoesNotFollowRedirects(t *testing.T) {
	var mu sync.Mutex
	var hops []string
	srv := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hops = append(hops, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/hook" {
			http.Redirect(w, r, "/landing", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := NewWebhookNotifyAdapter().Send(ctx, srv.URL+"/hook", "每日摘要")
	require.Error(t, err)
	require.True(t, core.IsExternalError(err), "3xx 不是成功")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/hook"}, hops, "客户端不得自行跟随——第二跳一次都没有发出")
}

// TestNotifyAdapters_ClientIgnoresEnvProxy —— A4（约束 17）：判据是两个适配器实际持有的
// 客户端配置本身——`Transport.Proxy == nil` 且 `CheckRedirect` 返回既有哨兵；
// 回环能发出去不具判别力（回环本就不走代理）。
// A4: both adapters hold a client whose transport ignores env proxies and which never
// follows redirects; the assertion targets the client config, not loopback behavior.
func TestNotifyAdapters_ClientIgnoresEnvProxy(t *testing.T) {
	adapters := map[string]httpDoer{
		"webhook": NewWebhookNotifyAdapter().doer,
		"feishu":  NewFeishuNotifyAdapter().doer,
	}
	for name, doer := range adapters {
		client, ok := doer.(*http.Client)
		require.True(t, ok, name)
		transport, ok := client.Transport.(*http.Transport)
		require.True(t, ok, name)
		require.Nil(t, transport.Proxy, "%s: 不读 HTTP(S)_PROXY", name)
		require.ErrorIs(t, client.CheckRedirect(nil, nil), http.ErrUseLastResponse, name)
	}
}

// TestWebhookNotifyAdapter_TransportErrorRedactsURL —— A5（约束 12 / 18 / R1）：
// 传输失败只出「类别：scheme://host」，原始 `url.Error`（含完整 URL）只进 Cause。
// A5 (R1): a transport failure renders only category plus host; the raw error stays in
// the cause chain and is never rendered.
func TestWebhookNotifyAdapter_TransportErrorRedactsURL(t *testing.T) {
	const token = "transport-token-must-not-leak"
	doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: "https://hooks.example.com/" + token, Err: errors.New("connection refused")}
	}}
	err := newWebhookNotifyAdapter(doer).Send(context.Background(),
		"https://hooks.example.com/"+token, "每日摘要")
	require.Error(t, err)
	require.True(t, core.IsExternalError(err))
	require.NotContains(t, err.Error(), token, "原始 url.Error 不进文案")
	require.Contains(t, err.Error(), "连接失败：https://hooks.example.com")
}

// TestWebhookNotifyAdapter_ContextIdentityWins —— A6（约束 19 / R9）：`Do` 错误命中
// ctx 身份时返回裸哨兵，不得包成 ExternalError（ErrorIs 对包装错误仍为真，不具区分力）。
// A6 (R9): a context identity wins over the external error and is returned bare.
func TestWebhookNotifyAdapter_ContextIdentityWins(t *testing.T) {
	doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial: %w", context.Canceled)
	}}
	err := newWebhookNotifyAdapter(doer).Send(context.Background(), "https://hooks.example.com/T", "x")
	require.Equal(t, context.Canceled, err, "裸哨兵：不得包装（ErrorIs 对包装错误仍为真，不具区分力）")
	require.False(t, core.IsExternalError(err))
}

// TestFeishuNotifyAdapter_PostsTextPayload —— A7（约束 14 / R7）：飞书文本载荷固定
// `{"msg_type":"text","content":{"text":"…"}}`，成功判据是回执而非仅状态码。
// A7 (R7): the fixed Feishu text payload is posted; success is decided by the receipt.
func TestFeishuNotifyAdapter_PostsTextPayload(t *testing.T) {
	doer := &fakeDoer{respond: okResponder(200, `{"code":0}`)}
	require.NoError(t, newFeishuNotifyAdapter(doer).Send(context.Background(),
		"https://open.feishu.cn/open-apis/bot/v2/hook/T1", "每日摘要"))
	require.Equal(t, 1, doer.count())
	req := doer.recorded()[0]
	require.Equal(t, http.MethodPost, req.Method)
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.JSONEq(t, `{"msg_type":"text","content":{"text":"每日摘要"}}`, string(body))
}

// TestFeishuNotifyAdapter_RequiresCodeZero —— A8（约束 14 / 15 / R7）：HTTP 200 但
// `code != 0` 判失败；文案含 `code=<N>` 与 `scheme://host`，不含回执正文。
// A8 (R7): a 200 response with a non-zero code fails; the receipt text is not echoed.
func TestFeishuNotifyAdapter_RequiresCodeZero(t *testing.T) {
	okDoer := &fakeDoer{respond: okResponder(200, `{"code":0,"msg":"success"}`)}
	require.NoError(t, newFeishuNotifyAdapter(okDoer).Send(context.Background(),
		"https://open.feishu.cn/open-apis/bot/v2/hook/T", "每日摘要"))

	badDoer := &fakeDoer{respond: okResponder(200, `{"code":19024,"msg":"Key Words Not Found"}`)}
	err := newFeishuNotifyAdapter(badDoer).Send(context.Background(),
		"https://open.feishu.cn/open-apis/bot/v2/hook/T", "每日摘要")
	require.True(t, core.IsExternalError(err))
	require.Contains(t, err.Error(), "19024")
	require.NotContains(t, err.Error(), "Key Words", "回执正文不进错误文案")
	require.Contains(t, err.Error(), "https://open.feishu.cn", "错误只出 scheme://host")
}

// TestFeishuNotifyAdapter_UnparsableReceiptFails —— A9（约束 15 / R7）：空体 / 非 JSON /
// 缺 code / `code:null` / 小数 / 字符串 / 尾随垃圾一律失败，固定「飞书回执无法解析」文案。
// A9 (R7): every unparsable receipt fails with the frozen phrase; only integer zero passes.
func TestFeishuNotifyAdapter_UnparsableReceiptFails(t *testing.T) {
	cases := []struct{ name, body string }{
		{"空体", ``},
		{"非 JSON", `ok`},
		{"缺 code", `{"msg":"ok"}`},
		{"code 为 null", `{"code":null}`},
		{"code 是小数", `{"code":1.5}`},
		{"code 是字符串", `{"code":"0"}`},
		{"尾随垃圾", `{"code":0}garbage`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(200, tc.body)}
			err := newFeishuNotifyAdapter(doer).Send(context.Background(),
				"https://open.feishu.cn/open-apis/bot/v2/hook/T", "每日摘要")
			require.True(t, core.IsExternalError(err))
			require.Contains(t, err.Error(), "飞书回执无法解析")
			require.Contains(t, err.Error(), "https://open.feishu.cn", "错误只出 scheme://host")
		})
	}
}

// TestFeishuNotifyAdapter_OversizedReceiptFails —— A10（约束 16 / R7）：正文前段是合法
// `{"code":0}`、整体超 64 KiB——读满截断解析会误判成功，只有显式检测超限才判失败。
// A10 (R7): an over-limit receipt whose prefix is a valid success JSON must fail; a
// read-to-cap-and-truncate implementation would wrongly report success.
func TestFeishuNotifyAdapter_OversizedReceiptFails(t *testing.T) {
	require.Equal(t, 64*1024, maxNotifyReceiptBytes,
		"回执上限钉死 64 KiB：常量被误改时用例不得随同变绿（A-05 强化，2026-10-18）")
	body := `{"code":0}` + strings.Repeat(" ", maxNotifyReceiptBytes)
	err := newFeishuNotifyAdapter(&fakeDoer{respond: okResponder(200, body)}).
		Send(context.Background(), "https://open.feishu.cn/open-apis/bot/v2/hook/T", "每日摘要")
	require.True(t, core.IsExternalError(err))
	require.Contains(t, err.Error(), "飞书回执无法解析")
}

// TestFeishuNotifyAdapter_Non2xxFailsBeforeReceipt —— A11（约束 14 / R7）：非 2xx 在回执
// 判定之前失败——即使响应体是合法 `{"code":0}` 也不得被当成功。
// A11 (R7): a non-2xx status fails before the receipt is even consulted.
func TestFeishuNotifyAdapter_Non2xxFailsBeforeReceipt(t *testing.T) {
	for _, status := range []int{302, 400, 500} {
		doer := &fakeDoer{respond: okResponder(status, `{"code":0}`)}
		err := newFeishuNotifyAdapter(doer).Send(context.Background(),
			"https://open.feishu.cn/open-apis/bot/v2/hook/T", "每日摘要")
		require.True(t, core.IsExternalError(err), "status=%d", status)
		require.Contains(t, err.Error(), strconv.Itoa(status))
		require.NotContains(t, err.Error(), "飞书回执无法解析", "非 2xx 不进入回执判定")
	}
}

// notifyReadCloser 是恒返回固定读取错误的响应体（A12 用）。
// notifyReadCloser is a response body whose read always fails with a fixed error.
type notifyReadCloser struct{ err error }

// Read 恒返回固定错误，不产生任何字节。
// Read always fails; it never yields bytes.
func (r notifyReadCloser) Read([]byte) (int, error) { return 0, r.err }

// Close 是空操作。
// Close is a no-op.
func (r notifyReadCloser) Close() error { return nil }

// TestFeishuNotifyAdapter_ReceiptReadFailureRespectsContext —— A12（约束 15 / 19 / R9）：
// 回执读取错误命中 ctx 身份时返回裸哨兵，不得包成 ExternalError。
// A12 (R9): a receipt read error carrying a context identity is returned bare.
func TestFeishuNotifyAdapter_ReceiptReadFailureRespectsContext(t *testing.T) {
	doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       notifyReadCloser{err: fmt.Errorf("receipt read: %w", context.Canceled)},
		}, nil
	}}
	err := newFeishuNotifyAdapter(doer).Send(context.Background(),
		"https://open.feishu.cn/open-apis/bot/v2/hook/T", "每日摘要")
	require.Equal(t, context.Canceled, err, "裸哨兵：读取错误命中 ctx 身份时不得包成 ExternalError")
	require.False(t, core.IsExternalError(err))
}

// TestNotifyAdapters_FixedContentType —— A13（§6.2 第 16 条）：两个适配器的请求头都固定
// `application/json; charset=utf-8`。
// A13: both adapters send the fixed JSON content type.
func TestNotifyAdapters_FixedContentType(t *testing.T) {
	const want = "application/json; charset=utf-8"

	webhook := &fakeDoer{respond: okResponder(200, "")}
	require.NoError(t, newWebhookNotifyAdapter(webhook).Send(context.Background(),
		"https://hooks.example.com/T", "x"))
	feishu := &fakeDoer{respond: okResponder(200, `{"code":0}`)}
	require.NoError(t, newFeishuNotifyAdapter(feishu).Send(context.Background(),
		"https://open.feishu.cn/open-apis/bot/v2/hook/T", "x"))

	require.Equal(t, want, webhook.recorded()[0].Header.Get("Content-Type"))
	require.Equal(t, want, feishu.recorded()[0].Header.Get("Content-Type"))
}
