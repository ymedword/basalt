package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// maxNotifyReceiptBytes 是飞书回执的读取上限（64 KiB）。读取时多读 1 字节判超限——
// 截断后解析可能恰好得到 `code:0`，把「结果未知」伪装成成功。
// maxNotifyReceiptBytes caps the Feishu receipt read at 64 KiB; one extra byte makes an
// over-limit receipt a decidable fact instead of a guess.
const maxNotifyReceiptBytes = 64 * 1024

// notifyContentType 是两个适配器唯一的请求正文类型（§6.2 第 16 条，固定字面量）。
// notifyContentType is the one fixed content type both adapters send.
const notifyContentType = "application/json; charset=utf-8"

// NotifyAdapter 是一次渠道投递的发送面；具体类型由装配层按 channel.Type 查表选定。
// 工具内核只查表，不出现类型判断分支。
// NotifyAdapter is the send face of one channel delivery; the assembly layer maps type
// to adapter, and the tool kernel never branches on the type string.
type NotifyAdapter interface {
	Send(ctx context.Context, rawURL, content string) error
}

// WebhookNotifyAdapter 发 `{"content": "…"}`；成功 = HTTP 2xx（仅表示自定义接收端
// 接受请求，不承诺业务级投递）。只面向接受 Basalt JSON 协议的自定义接收端 / 中转
// 服务，原生企微 / 钉钉机器人不在直连承诺内。
// WebhookNotifyAdapter posts the fixed JSON payload; 2xx means the receiver accepted the
// request, nothing more. Native WeCom / DingTalk bots are out of scope.
type WebhookNotifyAdapter struct{ doer httpDoer }

// 编译期断言：两个适配器就是工具内核查表用的发送面。
// Compile-time assertions that both adapters satisfy the send face.
var _ NotifyAdapter = (*WebhookNotifyAdapter)(nil)

// NewWebhookNotifyAdapter 用既有生产客户端（不跟随重定向、不读环境代理）构造。
// NewWebhookNotifyAdapter builds the production adapter on the shared client.
func NewWebhookNotifyAdapter() *WebhookNotifyAdapter {
	return newWebhookNotifyAdapter(newHTTPClient())
}

// newWebhookNotifyAdapter 是测试缝：注入伪 doer，不触网。
// newWebhookNotifyAdapter is the test seam that injects a fake doer.
func newWebhookNotifyAdapter(doer httpDoer) *WebhookNotifyAdapter {
	return &WebhookNotifyAdapter{doer: doer}
}

// Send 编码固定载荷并发送；成功响应体不读，所有分支都关闭响应体。
// Send posts the fixed payload; the success body is not read and every branch closes it.
func (a *WebhookNotifyAdapter) Send(ctx context.Context, rawURL, content string) error {
	resp, err := notifyPost(ctx, a.doer, rawURL, webhookNotifyPayload{Content: content})
	if err != nil {
		return err
	}
	discardBody(resp)
	return nil
}

// FeishuNotifyAdapter 发 `{"msg_type":"text","content":{"text":"…"}}`；成功 = HTTP
// 2xx 且回执 `code == 0`——飞书投递失败同样回 HTTP 200，状态码不是投递真值。
// FeishuNotifyAdapter posts the fixed text payload; success requires 2xx and code == 0,
// because Feishu reports delivery failures with HTTP 200 as well.
type FeishuNotifyAdapter struct{ doer httpDoer }

// 编译期断言：飞书适配器同样是工具内核查表的发送面。
// Compile-time assertion for the Feishu adapter.
var _ NotifyAdapter = (*FeishuNotifyAdapter)(nil)

// NewFeishuNotifyAdapter 用既有生产客户端（不跟随重定向、不读环境代理）构造。
// NewFeishuNotifyAdapter builds the production adapter on the shared client.
func NewFeishuNotifyAdapter() *FeishuNotifyAdapter {
	return newFeishuNotifyAdapter(newHTTPClient())
}

// newFeishuNotifyAdapter 是测试缝：注入伪 doer，不触网。
// newFeishuNotifyAdapter is the test seam that injects a fake doer.
func newFeishuNotifyAdapter(doer httpDoer) *FeishuNotifyAdapter {
	return &FeishuNotifyAdapter{doer: doer}
}

// Send 编码固定载荷、发送并判定回执；回执读取与判定的全部分支都关闭响应体。
// Send posts the fixed payload and judges the receipt; every branch closes the body.
func (a *FeishuNotifyAdapter) Send(ctx context.Context, rawURL, content string) error {
	resp, err := notifyPost(ctx, a.doer, rawURL, feishuNotifyPayload{
		MsgType: "text",
		Content: feishuNotifyText{Text: content},
	})
	if err != nil {
		return err
	}
	return feishuReceiptVerdict(ctx, resp, notifyDisplayURL(rawURL))
}

// webhookNotifyPayload 是 webhook 适配器的固定载荷：`{"content": "…"}`。
// webhookNotifyPayload is the fixed webhook payload.
type webhookNotifyPayload struct {
	Content string `json:"content"`
}

// feishuNotifyPayload 是飞书文本消息的固定载荷：`{"msg_type":"text","content":{"text":"…"}}`。
// feishuNotifyPayload is the fixed Feishu text payload.
type feishuNotifyPayload struct {
	MsgType string           `json:"msg_type"`
	Content feishuNotifyText `json:"content"`
}

// feishuNotifyText 是飞书消息体的 text 段。
// feishuNotifyText is the text segment of the Feishu payload.
type feishuNotifyText struct {
	Text string `json:"text"`
}

// feishuNotifyReceipt 是飞书回执的判定面：Code 用指针区分「缺失」与 0——两者语义相反：
// 0 是成功，缺失是「结果未知」。
// feishuNotifyReceipt keeps a pointer so missing and zero stay distinguishable: zero is
// success, missing is an unknown result.
type feishuNotifyReceipt struct {
	Code *int `json:"code"`
}

// notifyPost 编码、建请求并发送；非 2xx（含 3xx）在此统一判 external_error 并关闭
// 响应体，2xx 交回调用方（调用方在所有分支关闭 resp.Body）。编码 / 建请求失败属本侧
// 问题（runtime_error），文案只含 `scheme://host`；传输失败先判 ctx 身份，原始错误
// 只进 Cause 永不渲染——`url.Error` 的原文含整条 URL。
// notifyPost encodes and sends; every non-2xx status fails here with the body closed,
// and 2xx goes back to the caller. Transport failures check the context identity first,
// and the raw error stays in the cause chain only.
func notifyPost(ctx context.Context, doer httpDoer, rawURL string, payload any) (*http.Response, error) {
	display := notifyDisplayURL(rawURL)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("编码通知载荷失败：%s", display)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造通知请求失败：%s", display)
	}
	req.Header.Set("Content-Type", notifyContentType)

	resp, err := doer.Do(req)
	if err != nil {
		if ctxErr := ctxPreferred(ctx, err); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &core.ExternalError{
			Target: display,
			Reason: httpTransportReason(err) + "：" + display,
			Cause:  err,
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		discardBody(resp)
		return nil, &core.ExternalError{
			Target: display,
			Reason: fmt.Sprintf("HTTP %d：%s", resp.StatusCode, display),
		}
	}
	return resp, nil
}

// feishuReceiptVerdict 读取并判定飞书回执：最多读 64 KiB **多读 1 字节**——超限是显式
// 检测出的事实，不是截断后的猜测；解析用 json.Unmarshal（尾随垃圾同样报错）。空体 /
// 非 JSON / code 缺失、null、非整数一律按「飞书回执无法解析」判失败——「结果未知」不
// 冒充成功。读取错误先判 ctx 身份；所有分支关闭 resp.Body。
// feishuReceiptVerdict reads at most 64 KiB plus one byte and treats anything it cannot
// parse, including an over-limit receipt, as failure. The body is closed on every branch,
// and read errors check the context identity first.
func feishuReceiptVerdict(ctx context.Context, resp *http.Response, display string) error {
	unparsable := func(cause error) error {
		return &core.ExternalError{
			Target: display,
			Reason: "飞书回执无法解析：" + display,
			Cause:  cause,
		}
	}

	if resp.Body == nil {
		return unparsable(nil)
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxNotifyReceiptBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		if ctxErr := ctxPreferred(ctx, readErr); ctxErr != nil {
			return ctxErr
		}
		return unparsable(readErr)
	}
	if closeErr != nil {
		// 内容已读全，但连接状态可疑仍如实报失败（同包 httptools 的口径）。
		// The content is complete, but a suspicious connection still fails honestly.
		return unparsable(closeErr)
	}
	if len(raw) > maxNotifyReceiptBytes {
		return unparsable(nil)
	}

	var receipt feishuNotifyReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return unparsable(err)
	}
	if receipt.Code == nil {
		return unparsable(nil)
	}
	if *receipt.Code != 0 {
		return &core.ExternalError{
			Target: display,
			Reason: fmt.Sprintf("飞书投递失败：code=%d（%s）", *receipt.Code, display),
		}
	}
	return nil
}
