package web

// 本文件是通知渠道写面的 handler 级验收（模块卡 §3.2.1 web 行：9 例）。断言分三层：
// ① 状态码与固定错误码（同名创建是契约既有 400，**不是** 409）；② 脱敏——成功视图
// 只含 `url_host`，原始 `${VAR}` 引用与历史行里的真实地址（含查询串）没有任何响应
// 载体，5xx 不回显底层原因；③ handler 自身零副作用——纯请求校验失败（请求体非法、
// name 不一致、force 非字面量）**不**触达端口，因此不可能写操作行或动磁盘。
// 类型校验的唯一真值在装配层有序切片（约束 39），本层没有第二份类型表：这里只判
// "type 原样进、响应取端口返回值"。
// This file is the handler-level acceptance surface for the notify write endpoints:
// status/code mapping, redaction (only url_host leaves), and the absence of handler
// side effects on pure request errors. The ordered type table stays in assembly.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// errFakeNotifyAdminUnused 标记用例没有为该端口方法准备行为。
// errFakeNotifyAdminUnused marks a port method the case never intended to call.
var errFakeNotifyAdminUnused = errors.New("fake notify admin: unexpected call")

// fakeNotifyAdminService 是可注入行为的 `NotifyAdminService` 替身。
// fakeNotifyAdminService is a behavior-injected notify write port.
type fakeNotifyAdminService struct {
	createFn func(context.Context, core.Actor, NotifyChannelWrite) (core.NotifyChannel, error)
	updateFn func(context.Context, core.Actor, string, NotifyChannelWrite) (core.NotifyChannel, error)
	deleteFn func(context.Context, core.Actor, string, string, bool) error
	testFn   func(context.Context, core.Actor, string) (core.NotifyChannelTestResult, error)
}

// Create 委托创建行为。
func (f *fakeNotifyAdminService) Create(ctx context.Context, actor core.Actor, write NotifyChannelWrite) (core.NotifyChannel, error) {
	if f.createFn == nil {
		return core.NotifyChannel{}, errFakeNotifyAdminUnused
	}
	return f.createFn(ctx, actor, write)
}

// Update 委托更新行为。
func (f *fakeNotifyAdminService) Update(ctx context.Context, actor core.Actor, name string, write NotifyChannelWrite) (core.NotifyChannel, error) {
	if f.updateFn == nil {
		return core.NotifyChannel{}, errFakeNotifyAdminUnused
	}
	return f.updateFn(ctx, actor, name, write)
}

// Delete 委托删除行为。
func (f *fakeNotifyAdminService) Delete(ctx context.Context, actor core.Actor, name, resourceUID string, force bool) error {
	if f.deleteFn == nil {
		return errFakeNotifyAdminUnused
	}
	return f.deleteFn(ctx, actor, name, resourceUID, force)
}

// TestChannel 委托连通性测试行为。
func (f *fakeNotifyAdminService) TestChannel(ctx context.Context, actor core.Actor, name string) (core.NotifyChannelTestResult, error) {
	if f.testFn == nil {
		return core.NotifyChannelTestResult{}, errFakeNotifyAdminUnused
	}
	return f.testFn(ctx, actor, name)
}

// 编译期断言：假件实现被测端口——端口漂移在编译期就红。
var _ NotifyAdminService = (*fakeNotifyAdminService)(nil)

// notifyAdminHarness 是通知写面用例的装配：真实 handler 链 + 注入写端口。
// notifyAdminHarness wires the real handler chain over the injected write port.
type notifyAdminHarness struct {
	*Server
	fake *fakeNotifyAdminService
}

// notifyAdminCall 发送一个已认证的通知写面请求并返回记录器。
// notifyAdminCall serves one authenticated notify write request.
func notifyAdminCall(h *notifyAdminHarness, method, path, body string) *httptest.ResponseRecorder {
	return serveAdmin(h.Server, resourceCall(method, path, body))
}

// newNotifyAdminHarness 构造固定时钟、注入写端口的服务器。
// newNotifyAdminHarness assembles the fixture with the injected write port.
func newNotifyAdminHarness(t *testing.T, mutate func(*Options)) *notifyAdminHarness {
	t.Helper()
	fake := &fakeNotifyAdminService{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = resourceAuth{}
		o.NotifyAdminService = fake
		if mutate != nil {
			mutate(o)
		}
	})
	return &notifyAdminHarness{Server: srv, fake: fake}
}

// notifyView 解码成功信封里的 `NotifyChannelView`。
// notifyView decodes the channel view from a success envelope.
func notifyView(t *testing.T, rec *httptest.ResponseRecorder) NotifyChannelView {
	t.Helper()
	var view NotifyChannelView
	require.NoError(t, json.Unmarshal(decodeResourceEnvelope(t, rec).Data, &view),
		"body=%s", rec.Body.String())
	return view
}

// TestAdminNotify_CreateDuplicate400 —— 创建成功回 `NotifyChannelView`（只含
// `url_host`，占位不出站）；同名创建是同一条写路径里的契约既有 400 `BAD_REQUEST`
// ——**不是** 409，也不带 data；`type` 原样进端口、响应取端口返回值（本层无第二份
// 类型表）。空 URL（暂不可发送）同样成立，`url_host` 为空串。
// 逆改点：同名映射成 409 / 视图带原始 url / 本层擅自补类型表 → 对应断言红。
// TestAdminNotify_CreateDuplicate400: the view carries only url_host, and a duplicate
// name is the contract's 400 rather than a 409.
func TestAdminNotify_CreateDuplicate400(t *testing.T) {
	t.Parallel()
	h := newNotifyAdminHarness(t, nil)
	h.fake.createFn = func(_ context.Context, _ core.Actor, write NotifyChannelWrite) (core.NotifyChannel, error) {
		if write.Name == "dup" {
			return core.NotifyChannel{}, &core.InvalidInputError{
				Tool:   core.ActionNotifyChannelCreate,
				Reason: "通知渠道 dup 已存在，未执行创建",
			}
		}
		var url string
		if write.URL != nil {
			url = *write.URL
		}
		return core.NotifyChannel{
			Name: write.Name, Type: write.Type, URL: url, Description: write.Description,
		}, nil
	}

	rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"ops-alert","type":"webhook","url":"${OPS_HOOK}"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	view := notifyView(t, rec)
	require.Equal(t, NotifyChannelView{
		Name: "ops-alert", Type: "webhook",
		URLConfigured: true, ReferencedBy: []string{},
		// 空模式 / 空范围按列默认值投影成 env / shared（契约枚举不接受空串）。
		AddressStatus: "env", CredentialMode: "env", Scope: "shared",
	}, view, "占位引用已配置（url_configured=true）但解析不出 host，视图仍没有地址载体")
	require.NotContains(t, rec.Body.String(), "OPS_HOOK")
	require.NotContains(t, rec.Body.String(), "${")

	rec = notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"no-url","type":"feishu"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "feishu", notifyView(t, rec).Type,
		"type 由端口返回、原样投影：本层不得维护第二份类型集合")
	require.Equal(t, "", notifyView(t, rec).URLHost, "空 URL = 暂不可发送，url_host 为空串")

	rec = notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"dup","type":"webhook","url":"${OPS_HOOK}"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "同名创建是契约既有的 400")
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.NotEqual(t, codeConflict, env.Code, "契约 POST /notify-channels 不列 409")
	require.Contains(t, env.Message, "已存在")
	require.Nil(t, env.Data, "同名 400 不携带 data")
}

// TestAdminNotify_CreateBodyRejected —— 非法 JSON 与未知字段都在纯请求校验段拒绝：
// 400 + 固定文案，端口一次都不被调用（因此不可能写操作行或动磁盘）。
// 逆改点：解码放到委托之后 / 把未知字段吞掉 → 对应断言红。
// TestAdminNotify_CreateBodyRejected: malformed or unknown fields stop before the port.
func TestAdminNotify_CreateBodyRejected(t *testing.T) {
	t.Parallel()
	h := newNotifyAdminHarness(t, nil)
	h.fake.createFn = func(context.Context, core.Actor, NotifyChannelWrite) (core.NotifyChannel, error) {
		t.Error("请求体非法时不得触达端口")
		return core.NotifyChannel{}, nil
	}

	for _, body := range []string{
		`not-json`,
		`{"name":"ops-alert","type":"webhook","bogus":true}`,
		`{"name":"ops-alert","type":"webhook"} {"name":"second"}`,
	} {
		rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels", body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", body)
		env := decodeResourceEnvelope(t, rec)
		require.Equal(t, codeBadRequest, env.Code)
		require.Equal(t, messageNotifyChannelBody, env.Message, "body=%s", body)
	}
}

// TestAdminNotify_UpdateRequestShape —— 路径名是唯一身份：请求体 name 留空按路径
// 补齐；非空且不一致 → 400 固定文案且零端口调用。`url` 留空原样交给端口（"保留
// 原值"由适配器解释，本层不读存储、不回填地址）。
// 逆改点：本层回填已存地址 / name 不一致时仍委托 → 对应断言红。
// TestAdminNotify_UpdateRequestShape: the path supplies the identity, and a blank url
// is forwarded verbatim for the adapter to interpret as "keep".
func TestAdminNotify_UpdateRequestShape(t *testing.T) {
	t.Parallel()
	h := newNotifyAdminHarness(t, nil)
	var captured NotifyChannelWrite
	var capturedName string
	h.fake.updateFn = func(_ context.Context, _ core.Actor, name string, write NotifyChannelWrite) (core.NotifyChannel, error) {
		capturedName, captured = name, write
		echo := core.NotifyChannel{Name: name, Type: write.Type}
		if write.URL != nil {
			echo.URL = *write.URL
		}
		return echo, nil
	}

	emptyURL := ""
	rec := notifyAdminCall(h, http.MethodPut, "/api/v1/notify-channels/ops-alert",
		`{"type":"webhook","url":""}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ops-alert", capturedName)
	require.Equal(t, NotifyChannelWrite{Name: "ops-alert", Type: "webhook", URL: &emptyURL}, captured,
		"出现但为空的 url 必须原样进端口：空串是 400，不是保留，判定在适配器写锁内")
	require.Equal(t, "", notifyView(t, rec).URLHost)

	// 省略 url（字段不出现）与出现空串必须可区分：前者指针 nil = 保留原值。
	rec = notifyAdminCall(h, http.MethodPut, "/api/v1/notify-channels/ops-alert",
		`{"type":"webhook","description":"只改说明"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Nil(t, captured.URL, "省略 url 必须解码成 nil（保留原值），不得退化成空串")

	rec = notifyAdminCall(h, http.MethodPut, "/api/v1/notify-channels/ops-alert",
		`{"name":"other","type":"webhook","url":""}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, messageNotifyChannelNameMismatch, decodeResourceEnvelope(t, rec).Message,
		"name 不一致是纯请求校验，端口调用次数不增加")

	rec = notifyAdminCall(h, http.MethodPut, "/api/v1/notify-channels/ops-alert", `{`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, messageNotifyChannelBody, decodeResourceEnvelope(t, rec).Message)
}

// TestAdminNotify_UpdateErrorMapping —— 适配器失败按类型映射：不存在 → 404
// `NOT_FOUND`（点名渠道）；输入错误 → 400 `BAD_REQUEST`（Reason 原样）；其余 →
// 固定 500 `INTERNAL`，底层原因只进日志、**不**回显。
// 逆改点：把未知错误映射成 400 / 把底层错误文本写进信封 → 对应断言红。
// TestAdminNotify_UpdateErrorMapping: typed failures map to 404/400/500 by type, and
// an unknown failure never leaks its text.
func TestAdminNotify_UpdateErrorMapping(t *testing.T) {
	t.Parallel()
	h := newNotifyAdminHarness(t, nil)

	h.fake.updateFn = func(_ context.Context, _ core.Actor, name string, _ NotifyChannelWrite) (core.NotifyChannel, error) {
		return core.NotifyChannel{}, fmt.Errorf("%w: 通知渠道 %s", core.ErrResourceNotFound, name)
	}
	rec := notifyAdminCall(h, http.MethodPut, "/api/v1/notify-channels/ghost",
		`{"type":"webhook","url":""}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, codeNotFound, decodeResourceEnvelope(t, rec).Code)
	require.Contains(t, decodeResourceEnvelope(t, rec).Message, "ghost")

	h.fake.updateFn = func(_ context.Context, _ core.Actor, _ string, _ NotifyChannelWrite) (core.NotifyChannel, error) {
		return core.NotifyChannel{}, &core.InvalidInputError{
			Tool:   core.ActionNotifyChannelUpdate,
			Reason: "通知渠道类型不在已装配集合：只接受 feishu、webhook",
		}
	}
	rec = notifyAdminCall(h, http.MethodPut, "/api/v1/notify-channels/ops-alert",
		`{"type":"slack","url":""}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, codeBadRequest, decodeResourceEnvelope(t, rec).Code)
	require.Contains(t, decodeResourceEnvelope(t, rec).Message, "feishu、webhook")

	h.fake.updateFn = func(_ context.Context, _ core.Actor, _ string, _ NotifyChannelWrite) (core.NotifyChannel, error) {
		return core.NotifyChannel{}, errors.New("磁盘写入失败：C:\\secret\\path")
	}
	rec = notifyAdminCall(h, http.MethodPut, "/api/v1/notify-channels/ops-alert",
		`{"type":"webhook","url":""}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, codeInternal, decodeResourceEnvelope(t, rec).Code)
	require.Equal(t, messageInternal, decodeResourceEnvelope(t, rec).Message)
	require.NotContains(t, rec.Body.String(), "secret", "底层原因不得出站")
}

// TestAdminNotify_DeleteForceLiteral —— `force` 只认字面 `true`：缺省 = false；
// `true` = 强删；其余取值（`1` / `TRUE` / `false`）→ 400 固定文案且零端口调用。
// 逆改点：按 presence 判强删 / 接受大小写变体 → 对应断言红。
// TestAdminNotify_DeleteForceLiteral: only the literal "true" enables a forced delete;
// every other value is the contract's 400 before the port runs.
func TestAdminNotify_DeleteForceLiteral(t *testing.T) {
	t.Parallel()
	h := newNotifyAdminHarness(t, nil)
	var calls []bool
	var uids []string
	h.fake.deleteFn = func(_ context.Context, _ core.Actor, name, resourceUID string, force bool) error {
		require.Equal(t, "ops-alert", name)
		calls = append(calls, force)
		uids = append(uids, resourceUID)
		return nil
	}

	rec := notifyAdminCall(h, http.MethodDelete, "/api/v1/notify-channels/ops-alert", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Nil(t, decodeResourceEnvelope(t, rec).Data, "删除成功是 200 + 空 data")

	rec = notifyAdminCall(h, http.MethodDelete, "/api/v1/notify-channels/ops-alert?force=true", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []bool{false, true}, calls, "缺省 = 非强删；字面 true = 强删")

	// 资源身份是契约的必需查询参数：本层原样下传，不在 web 侧比较（比较归适配器）。
	rec = notifyAdminCall(h, http.MethodDelete,
		"/api/v1/notify-channels/ops-alert?resource_uid=rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"", "", "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"}, uids,
		"resource_uid 必须原样下传")

	for _, raw := range []string{"?force=1", "?force=TRUE", "?force=false"} {
		rec = notifyAdminCall(h, http.MethodDelete, "/api/v1/notify-channels/ops-alert"+raw, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, "query=%s", raw)
		env := decodeResourceEnvelope(t, rec)
		require.Equal(t, codeBadRequest, env.Code)
		require.Equal(t, messageNotifyForceValue, env.Message)
	}
	require.Len(t, calls, 3, "非法 force 不得触达端口")
}

// TestAdminNotify_DeleteReferenced400 —— 整词引用命中（含归档 Agent 的
// `archive:<id>` 形式）由适配器回 400 并携带 `data.agents`；不存在 → 404。
// 本层只做白名单投影，不改写点名内容。
// 逆改点：吞掉 data / 把引用命中映射成 409 / 把 404 投影成 400 → 对应断言红。
// TestAdminNotify_DeleteReferenced400: the adapter's 400 keeps data.agents, and a
// missing channel stays a 404.
func TestAdminNotify_DeleteReferenced400(t *testing.T) {
	t.Parallel()
	h := newNotifyAdminHarness(t, nil)

	h.fake.deleteFn = func(_ context.Context, _ core.Actor, _, _ string, _ bool) error {
		return &core.AdminError{
			Kind: &core.InvalidInputError{
				Tool:   core.ActionNotifyChannelDelete,
				Reason: "通知渠道 ops-alert 被 2 个 Agent 的 AGENT.md 引用，未执行删除",
			},
			Data: map[string]any{
				"agents":       []string{"archive:a-1", "ops-agent"},
				"operation_id": "op-notify-referenced",
			},
		}
	}
	rec := notifyAdminCall(h, http.MethodDelete, "/api/v1/notify-channels/ops-alert", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Contains(t, env.Message, "引用")
	var data struct {
		Agents      []string `json:"agents"`
		OperationID string   `json:"operation_id"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, []string{"archive:a-1", "ops-agent"}, data.Agents,
		"点名内容原样投影：归档者写 archive:<archive_id>")
	require.Equal(t, "op-notify-referenced", data.OperationID,
		"pending 之后的引用闸门失败必须可追查（约束 1a）")

	h.fake.deleteFn = func(_ context.Context, _ core.Actor, name, _ string, _ bool) error {
		return fmt.Errorf("%w: 通知渠道 %s", core.ErrResourceNotFound, name)
	}
	rec = notifyAdminCall(h, http.MethodDelete, "/api/v1/notify-channels/ghost", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, codeNotFound, decodeResourceEnvelope(t, rec).Code)
	require.Contains(t, decodeResourceEnvelope(t, rec).Message, "ghost")
}

// TestAdminNotify_ListHostOnly —— 写后读：写端点成功回包与 `GET /notify-channels`
// 的列表项都只含 `url_host`。`${VAR}` 引用投影成空串；历史行里的绝对地址只留
// scheme + host，查询串（可能带 token）没有任何响应载体。
// 逆改点：视图换成带 URL 的原始结构 / url_host 退回原文案 → 对应断言红。
// TestAdminNotify_ListHostOnly: both the write response and the US-11 list carry only
// url_host; a placeholder projects to an empty string and a query string never leaks.
func TestAdminNotify_ListHostOnly(t *testing.T) {
	t.Parallel()
	placeholder := core.NotifyChannel{Name: "ops-alert", Type: "webhook", URL: "${OPS_HOOK}"}
	legacy := core.NotifyChannel{
		Name: "legacy", Type: "webhook",
		URL:         "https://hooks.example.com/services/x?token=super-secret",
		Description: "旧行",
	}
	h := newNotifyAdminHarness(t, func(o *Options) {
		o.Notify = fakeNotifyChannels{channels: []core.NotifyChannel{placeholder, legacy}}
	})
	h.fake.createFn = func(_ context.Context, _ core.Actor, write NotifyChannelWrite) (core.NotifyChannel, error) {
		echo := core.NotifyChannel{Name: write.Name, Type: write.Type}
		if write.URL != nil {
			echo.URL = *write.URL
		}
		return echo, nil
	}

	rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"ops-alert","type":"webhook","url":"${OPS_HOOK}"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "", notifyView(t, rec).URLHost)
	require.NotContains(t, rec.Body.String(), "OPS_HOOK")

	rec = serveAdmin(h.Server, withBearer(adminRequest(http.MethodGet, "/api/v1/notify-channels", ""), testToken))
	require.Equal(t, http.StatusOK, rec.Code)
	var views []NotifyChannelView
	require.NoError(t, json.Unmarshal(decodeResourceEnvelope(t, rec).Data, &views))
	require.Equal(t, []NotifyChannelView{
		{
			Name: "ops-alert", Type: "webhook", URLConfigured: true, ReferencedBy: []string{},
			AddressStatus: "env", CredentialMode: "env", Scope: "shared",
		},
		{
			Name: "legacy", Type: "webhook", Description: "旧行", URLHost: "https://hooks.example.com",
			URLConfigured: true, ReferencedBy: []string{},
			AddressStatus: "env", CredentialMode: "env", Scope: "shared",
		},
	}, views)
	require.NotContains(t, rec.Body.String(), "super-secret")
	require.NotContains(t, rec.Body.String(), "/services/x")
}

// TestAdminNotify_PortMissing —— 端口未装配时三条写端点都按固定 500 INTERNAL
// 失败关闭：不伪装成功、不回空视图、不暴露内部原因。
// 逆改点：缺端口时填充默认实现 / 回 200 → 对应断言红。
// TestAdminNotify_PortMissing: an unassembled port fails closed on all three writes.
func TestAdminNotify_PortMissing(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, func(o *Options) { o.AdminAuth = resourceAuth{} })

	for _, call := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/notify-channels", `{"name":"ops-alert","type":"webhook"}`},
		{http.MethodPut, "/api/v1/notify-channels/ops-alert", `{"type":"webhook"}`},
		{http.MethodDelete, "/api/v1/notify-channels/ops-alert", ""},
	} {
		rec := serveAdmin(srv, resourceCall(call.method, call.path, call.body))
		require.Equal(t, http.StatusInternalServerError, rec.Code, "%s %s", call.method, call.path)
		env := decodeResourceEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
	}
}

// TestAdminNotify_CreateViewOnlyHost —— 创建回包对"历史行式"绝对地址同样只投影
// scheme + host：查询串与路径都不出站（凭证红线不依赖适配器是否总能存占位）。
// 逆改点：`notifyChannelView` 改成直接回 ch.URL → 对应断言红。
// TestAdminNotify_CreateViewOnlyHost: the write response projects a literal address to
// scheme+host as well, so the red line does not depend on the adapter.
func TestAdminNotify_CreateViewOnlyHost(t *testing.T) {
	t.Parallel()
	h := newNotifyAdminHarness(t, nil)
	h.fake.createFn = func(_ context.Context, _ core.Actor, write NotifyChannelWrite) (core.NotifyChannel, error) {
		return core.NotifyChannel{
			Name: write.Name, Type: write.Type,
			URL: "https://open.feishu.cn/open-apis/bot/v2/hook/abc?token=super-secret",
		}, nil
	}

	rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"feishu-alert","type":"feishu","url":"${FEISHU_HOOK}"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "https://open.feishu.cn", notifyView(t, rec).URLHost)
	require.NotContains(t, rec.Body.String(), "super-secret")
	require.NotContains(t, rec.Body.String(), "/open-apis")
	require.NotContains(t, rec.Body.String(), "FEISHU_HOOK")
}

// TestAdminNotify_WriteViewCarriesDocumentReferences —— 写成功回包与列表同源：引用
// 端口给出的文档引用者原样带到 `referenced_by`，创建与更新共用同一条投影。
// The write response reuses the list projection for document references.
func TestAdminNotify_WriteViewCarriesDocumentReferences(t *testing.T) {
	t.Parallel()
	h := newNotifyAdminHarness(t, func(o *Options) {
		o.NotifyReferences = fakeNotifyReferences{refs: map[string][]string{
			"ops-alert": {"writer", "archive:arc-1"},
		}}
	})
	h.fake.createFn = func(_ context.Context, _ core.Actor, write NotifyChannelWrite) (core.NotifyChannel, error) {
		echo := core.NotifyChannel{Name: write.Name, Type: write.Type}
		if write.URL != nil {
			echo.URL = *write.URL
		}
		return echo, nil
	}
	h.fake.updateFn = func(_ context.Context, _ core.Actor, name string, write NotifyChannelWrite) (core.NotifyChannel, error) {
		return core.NotifyChannel{Name: name, Type: write.Type}, nil
	}

	rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"ops-alert","type":"webhook","url":"${OPS_HOOK}"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"writer", "archive:arc-1"}, notifyView(t, rec).ReferencedBy)

	rec = notifyAdminCall(h, http.MethodPut, "/api/v1/notify-channels/ops-alert",
		`{"type":"webhook"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"writer", "archive:arc-1"}, notifyView(t, rec).ReferencedBy)
}

// TestAdminNotify_WriteReferenceScanFailureKeepsWrite —— 写已落盘后引用扫描失败：
// 回 200 并降级 `referenced_by=[]`，不能回 500 让客户端以为写失败而重试（重试会撞
// 同名 400 或覆盖更新）；写端口只被调用一次。
// A post-write scan failure degrades the view instead of failing the write.
func TestAdminNotify_WriteReferenceScanFailureKeepsWrite(t *testing.T) {
	t.Parallel()
	errScan := errors.New("scan failed")
	h := newNotifyAdminHarness(t, func(o *Options) {
		o.NotifyReferences = fakeNotifyReferences{err: errScan}
	})
	creates := 0
	h.fake.createFn = func(_ context.Context, _ core.Actor, write NotifyChannelWrite) (core.NotifyChannel, error) {
		creates++
		return core.NotifyChannel{Name: write.Name, Type: write.Type}, nil
	}

	rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"ops-alert","type":"webhook"}`)
	require.Equal(t, http.StatusOK, rec.Code, "写已落盘，扫描失败不得回 500")
	require.Equal(t, []string{}, notifyView(t, rec).ReferencedBy, "降级为空引用而不是省略字段")
	require.Equal(t, 1, creates, "客户端不因扫描失败重试")
	require.NotContains(t, rec.Body.String(), errScan.Error(), "底层原因只进日志")
}

// TestAdminNotify_CredentialRefReadOnly —— `credential_ref` 是服务端生成的只读字段：
// 创建与更新的请求体里出现即 400 `CREDENTIAL_REF_READ_ONLY`，且**不**触达写端口
// （因此不可能写操作行，也不会先落一半再拒）。
// TestAdminNotify_CredentialRefReadOnly refuses a client-supplied credential_ref on both
// write endpoints before the port runs.
func TestAdminNotify_CredentialRefReadOnly(t *testing.T) {
	t.Parallel()
	h := newNotifyAdminHarness(t, nil)
	h.fake.createFn = func(context.Context, core.Actor, NotifyChannelWrite) (core.NotifyChannel, error) {
		t.Error("credential_ref 必须在调端口之前被拒绝")
		return core.NotifyChannel{}, nil
	}
	h.fake.updateFn = func(context.Context, core.Actor, string, NotifyChannelWrite) (core.NotifyChannel, error) {
		t.Error("credential_ref 必须在调端口之前被拒绝")
		return core.NotifyChannel{}, nil
	}

	rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"ops-alert","type":"webhook","credential_mode":"managed","credential_ref":"cred_deadbeef"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, codeCredentialRefReadOnly, decodeResourceEnvelope(t, rec).Code)

	rec = notifyAdminCall(h, http.MethodPut, "/api/v1/notify-channels/ops-alert",
		`{"type":"webhook","credential_mode":"managed","credential_ref":"cred_deadbeef"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, codeCredentialRefReadOnly, decodeResourceEnvelope(t, rec).Code)
	require.NotContains(t, rec.Body.String(), "cred_deadbeef", "不得回显客户端自造的凭据 ID")
}

// TestAdminNotify_CreateWriteOnlyShape —— 创建同样把 `url` 解成三态：省略 = nil、
// 空串 = 非 nil 空值、有值 = 原样；`credential_mode` / `scope` / `owner_agent` 原样
// 进端口，托管明文地址不出现在响应里。
// TestAdminNotify_CreateWriteOnlyShape pins the create-side tri-state url and the
// verbatim forwarding of mode and scope.
func TestAdminNotify_CreateWriteOnlyShape(t *testing.T) {
	t.Parallel()
	h := newNotifyAdminHarness(t, nil)
	var captured NotifyChannelWrite
	h.fake.createFn = func(_ context.Context, _ core.Actor, write NotifyChannelWrite) (core.NotifyChannel, error) {
		captured = write
		return core.NotifyChannel{Name: write.Name, Type: write.Type, URLHost: "hooks.example.com"}, nil
	}

	rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"ops-alert","type":"webhook"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Nil(t, captured.URL, "省略 url 必须解码成 nil")

	rec = notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"ops-alert","type":"webhook","url":""}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, captured.URL, "出现但为空的 url 必须可区分")
	require.Empty(t, *captured.URL)

	rec = notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
		`{"name":"ops-alert","type":"webhook","credential_mode":"managed",`+
			`"url":"https://hooks.example.com/t/v1/token-abc","scope":"exclusive","owner_agent":"ops-agent"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "managed", captured.CredentialMode)
	require.Equal(t, "exclusive", captured.Scope)
	require.Equal(t, "ops-agent", captured.OwnerAgent)
	require.NotContains(t, rec.Body.String(), "token-abc", "托管地址是 write-only，不得回显")
}

// TestAdminNotify_CredentialErrorMapping —— 保存期凭据失败按 sentinel 映射：模式不符 /
// 撤销 → 400，范围被占用 → 409，主密钥不可用 → 503；文案由适配器给出，不是一律 400。
// TestAdminNotify_CredentialErrorMapping pins the save-time status split per sentinel.
func TestAdminNotify_CredentialErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"CREDENTIAL_MODE_REQUIRED", core.ErrCredentialModeRequired, http.StatusBadRequest, codeCredentialModeRequired},
		{"CREDENTIAL_REVOKED", core.ErrCredentialRevoked, http.StatusBadRequest, codeCredentialRevoked},
		{"RESOURCE_SCOPE_IN_USE", core.ErrResourceScopeInUse, http.StatusConflict, codeResourceScopeInUse},
		{"CREDENTIAL_UNAVAILABLE", core.ErrCredentialUnavailable, http.StatusServiceUnavailable, codeCredentialUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newNotifyAdminHarness(t, nil)
			h.fake.createFn = func(context.Context, core.Actor, NotifyChannelWrite) (core.NotifyChannel, error) {
				return core.NotifyChannel{}, &core.AdminError{Kind: tc.err, Message: "适配器文案：" + tc.name}
			}

			rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels",
				`{"name":"ops-alert","type":"webhook","url":"${BASALT_TEST_OPS_HOOK}"}`)
			require.Equal(t, tc.status, rec.Code)
			env := decodeResourceEnvelope(t, rec)
			require.Equal(t, tc.code, env.Code)
			require.Contains(t, env.Message, tc.name)
		})
	}
}

// TestAdminNotify_TestChannel —— 连通性测试端点：成功 / 确定失败 / 结果未知都回 200
// 与 `NotifyChannelTestResult`（未知是测试结论，不是接口错误）；未配置地址 → 503
// `NOT_CONFIGURED` 且带 operation_id；渠道缺失 → 404；缺端口 → 500 通用文案。
// 逆改点：把 unknown 映射成 500/504 丢掉结论 / 失败回包写成 ok=true →
// 对应断言红。
// TestAdminNotify_TestChannel pins the three-valued 200 body and the error split.
func TestAdminNotify_TestChannel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		result core.NotifyChannelTestResult
	}{{
		name:   "成功",
		result: core.NotifyChannelTestResult{OK: true, Status: core.ConnectionTestSuccess, LatencyMS: 42, Detail: "测试消息已发送"},
	}, {
		name:   "确定失败",
		result: core.NotifyChannelTestResult{OK: false, Status: core.ConnectionTestFailed, LatencyMS: 31, Detail: "飞书回执 code=19001"},
	}, {
		name:   "结果未知",
		result: core.NotifyChannelTestResult{OK: false, Status: core.ConnectionTestUnknown, LatencyMS: 15000, Detail: "等待回执超时"},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newNotifyAdminHarness(t, nil)
			h.fake.testFn = func(_ context.Context, _ core.Actor, name string) (core.NotifyChannelTestResult, error) {
				require.Equal(t, "ops-alert", name)
				return tc.result, nil
			}
			rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels/ops-alert/test", "")
			require.Equal(t, http.StatusOK, rec.Code)
			var view NotifyChannelTestResultView
			require.NoError(t, json.Unmarshal(decodeResourceEnvelope(t, rec).Data, &view))
			require.Equal(t, tc.result.OK, view.OK)
			require.Equal(t, tc.result.Status, view.Status)
			require.Equal(t, tc.result.LatencyMS, view.LatencyMS)
			require.Equal(t, tc.result.Detail, view.Detail)
		})
	}

	t.Run("未配置地址", func(t *testing.T) {
		t.Parallel()
		h := newNotifyAdminHarness(t, nil)
		h.fake.testFn = func(context.Context, core.Actor, string) (core.NotifyChannelTestResult, error) {
			return core.NotifyChannelTestResult{Status: core.ConnectionTestFailed},
				&core.AdminError{
					Kind:    core.ErrProviderNotConfigured,
					Message: "通知渠道 ops-alert 未配置地址：请先在渠道详情补录地址后重试",
					Data:    map[string]any{"operation_id": "op-1760000000000-abcdef"},
				}
		}
		rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels/ops-alert/test", "")
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		env := decodeResourceEnvelope(t, rec)
		require.Equal(t, codeNotConfigured, env.Code)
		require.Contains(t, env.Message, "补录地址")
		require.Contains(t, string(env.Data), "op-1760000000000-abcdef", "失败要可查证")
	})

	t.Run("渠道缺失", func(t *testing.T) {
		t.Parallel()
		h := newNotifyAdminHarness(t, nil)
		h.fake.testFn = func(_ context.Context, _ core.Actor, name string) (core.NotifyChannelTestResult, error) {
			return core.NotifyChannelTestResult{}, fmt.Errorf("%w: 通知渠道 %s", core.ErrResourceNotFound, name)
		}
		rec := notifyAdminCall(h, http.MethodPost, "/api/v1/notify-channels/ghost/test", "")
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, codeNotFound, decodeResourceEnvelope(t, rec).Code)
	})

	t.Run("缺端口", func(t *testing.T) {
		t.Parallel()
		srv := newTestServer(t, func(o *Options) { o.AdminAuth = resourceAuth{} })
		rec := serveAdmin(srv, resourceCall(http.MethodPost, "/api/v1/notify-channels/ops-alert/test", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		env := decodeResourceEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
	})
}
