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

// SendResult 是系统代投用的详细结果面：2xx 即 sent（自定义接收端没有业务回执可判），
// 非 2xx 是确定失败，传输歧义按 classifyDeliveryTransport 三分。
// SendResult reports the detailed outcome for system delivery: 2xx is sent, non-2xx is a
// definite failure, and ambiguous transport errors are classified three ways.
func (a *WebhookNotifyAdapter) SendResult(ctx context.Context, rawURL, content string) core.DeliveryOutcome {
	display := notifyDisplayURL(rawURL)
	resp, stage, err := notifyDispatch(ctx, a.doer, rawURL, webhookNotifyPayload{Content: content})
	switch stage {
	case notifySendEncode, notifySendRequest:
		return failedOutcome(0, "构造通知请求失败："+display)
	case notifySendTransport:
		return classifyDeliveryTransport(ctx, err, display)
	}
	status := resp.StatusCode
	discardBody(resp)
	if status < 200 || status >= 300 {
		return failedOutcome(status, fmt.Sprintf("HTTP %d：%s", status, display))
	}
	return sentOutcome(status, fmt.Sprintf("已发送（HTTP %d）：%s", status, display))
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

// SendResult 是系统代投用的详细结果面：2xx 且回执 code=0 才是 sent，明确回执错误是
// failed，回执不可解析是 unknown——飞书失败同样回 HTTP 200，状态码不是投递真值。
// SendResult needs both 2xx and code == 0 for a success; Feishu reports failures with
// HTTP 200, and an unparsable receipt stays unknown.
func (a *FeishuNotifyAdapter) SendResult(ctx context.Context, rawURL, content string) core.DeliveryOutcome {
	display := notifyDisplayURL(rawURL)
	resp, stage, err := notifyDispatch(ctx, a.doer, rawURL, feishuNotifyPayload{
		MsgType: "text",
		Content: feishuNotifyText{Text: content},
	})
	switch stage {
	case notifySendEncode, notifySendRequest:
		return failedOutcome(0, "构造通知请求失败："+display)
	case notifySendTransport:
		return classifyDeliveryTransport(ctx, err, display)
	}
	status := resp.StatusCode
	if status < 200 || status >= 300 {
		discardBody(resp)
		return failedOutcome(status, fmt.Sprintf("HTTP %d：%s", status, display))
	}
	code, verdict, _ := readFeishuReceipt(resp)
	switch verdict {
	case receiptAccepted:
		return core.DeliveryOutcome{
			Status:      core.DeliverySent,
			HTTPStatus:  status,
			ReceiptCode: &code,
			Detail:      "飞书已受理（code=0）：" + display,
		}
	case receiptRejected:
		return core.DeliveryOutcome{
			Status:      core.DeliveryFailed,
			HTTPStatus:  status,
			ReceiptCode: &code,
			Detail:      fmt.Sprintf("飞书投递失败：code=%d（%s）", code, display),
		}
	}
	return core.DeliveryOutcome{
		Status:     core.DeliveryUnknown,
		HTTPStatus: status,
		Detail:     "飞书回执无法解析，结果未知：" + display,
	}
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

// notifySendStage 标记发送在哪一步停下：编码 / 构造属**请求未发出**的确定失败，
// 传输失败可能已送达，调用方据此区分确定失败与结果未知。
// notifySendStage marks where a send stopped: encoding and request construction fail
// before anything leaves the process, while transport errors may already have delivered.
type notifySendStage int

const (
	// notifySendOK 表示请求已发出并拿到响应（含非 2xx）。
	notifySendOK notifySendStage = iota
	// notifySendEncode 表示载荷编码失败。
	notifySendEncode
	// notifySendRequest 表示请求构造失败。
	notifySendRequest
	// notifySendTransport 表示传输层失败，是否送达未知。
	notifySendTransport
)

// notifyDispatch 编码、建请求并发送；非 2xx 的响应照常返回，由调用方判状态并关闭。
// 这个分层让 `Send`（错误即失败）与 `SendResult`（三分结果）共享同一条发送路径。
// notifyDispatch encodes and sends, returning non-2xx responses to the caller so the
// binary and three-valued faces share one send path.
func notifyDispatch(ctx context.Context, doer httpDoer, rawURL string, payload any) (*http.Response, notifySendStage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, notifySendEncode, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, notifySendRequest, err
	}
	req.Header.Set("Content-Type", notifyContentType)

	resp, err := doer.Do(req)
	if err != nil {
		return nil, notifySendTransport, err
	}
	return resp, notifySendOK, nil
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

	resp, stage, err := notifyDispatch(ctx, doer, rawURL, payload)
	switch stage {
	case notifySendEncode:
		return nil, fmt.Errorf("编码通知载荷失败：%s", display)
	case notifySendRequest:
		return nil, fmt.Errorf("构造通知请求失败：%s", display)
	case notifySendTransport:
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

// receiptVerdict 是飞书回执的三值判定。
// receiptVerdict is the three-valued verdict of a Feishu receipt.
type receiptVerdict int

const (
	// receiptAccepted 表示 2xx 且 code == 0。
	receiptAccepted receiptVerdict = iota
	// receiptRejected 表示回执明确给出非 0 的 code。
	receiptRejected
	// receiptUnparsable 表示回执读不全 / 不是 JSON / 缺 code——结果未知。
	receiptUnparsable
)

// readFeishuReceipt 读取并判定回执：最多读 64 KiB **多读 1 字节**——超限是显式检测出
// 的事实，不是截断后的猜测；解析用 json.Unmarshal（尾随垃圾同样报错）。空体 / 非 JSON
// / code 缺失、null、非整数一律落 receiptUnparsable。所有分支关闭 resp.Body。
// readFeishuReceipt reads at most 64 KiB plus one byte; anything it cannot parse, over
// limit included, is unparsable. The body is closed on every branch.
func readFeishuReceipt(resp *http.Response) (int, receiptVerdict, error) {
	if resp.Body == nil {
		return 0, receiptUnparsable, nil
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxNotifyReceiptBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return 0, receiptUnparsable, readErr
	}
	if closeErr != nil {
		// 内容已读全，但连接状态可疑仍如实报未知（同包 httptools 的口径）。
		// The content is complete, but a suspicious connection still reports unknown.
		return 0, receiptUnparsable, closeErr
	}
	if len(raw) > maxNotifyReceiptBytes {
		return 0, receiptUnparsable, nil
	}

	var receipt feishuNotifyReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return 0, receiptUnparsable, err
	}
	if receipt.Code == nil {
		return 0, receiptUnparsable, nil
	}
	if *receipt.Code != 0 {
		return *receipt.Code, receiptRejected, nil
	}
	return 0, receiptAccepted, nil
}

// feishuReceiptVerdict 把回执判定转成工具路径的错误：不可解析按失败（`Send` 只有二值
// 结果），读取原因先判 ctx 身份。
// feishuReceiptVerdict turns the verdict into the binary tool-path error.
func feishuReceiptVerdict(ctx context.Context, resp *http.Response, display string) error {
	code, verdict, cause := readFeishuReceipt(resp)
	switch verdict {
	case receiptAccepted:
		return nil
	case receiptRejected:
		return &core.ExternalError{
			Target: display,
			Reason: fmt.Sprintf("飞书投递失败：code=%d（%s）", code, display),
		}
	}
	if ctxErr := ctxPreferred(ctx, cause); ctxErr != nil {
		return ctxErr
	}
	return &core.ExternalError{
		Target: display,
		Reason: "飞书回执无法解析：" + display,
		Cause:  cause,
	}
}
