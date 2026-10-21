package web

// 本文件覆盖通知渠道只读双端点（模块卡 §3.1.29 / §4.1 第 25–26 行，8 例）：
// 列表只回 `url_host`（原始 URL 与查询串无任何响应载体）、解析不出的地址回空、
// 类型表按装配层顺序、空集合 `[]`、端口缺失 / 读取失败的固定 500 与认证闸门。
// Notify channel read acceptance: url_host-only projection with no raw-URL carrier,
// empty hosts for unparsable addresses, assembly order for types, and fixed failures.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeNotifyChannels 是可编程的渠道只读面：channels / err 由用例预置。
// fakeNotifyChannels is a programmable NotifyChannelReader.
type fakeNotifyChannels struct {
	channels []core.NotifyChannel
	err      error
}

func (f fakeNotifyChannels) ListNotifyChannels(context.Context) ([]core.NotifyChannel, error) {
	return f.channels, f.err
}

// 编译期断言：假件实现被测端口——端口漂移在编译期就红。
var _ core.NotifyChannelReader = fakeNotifyChannels{}

// errNotifyListFailed 是读取失败的注入错误（文案只用于「不得出站」断言）。
var errNotifyListFailed = errors.New("db unavailable")

// newNotifyReadServer 装配带假渠道端口与固定类型表的 handler 级服务。
func newNotifyReadServer(t *testing.T, mutate func(*Options)) *Server {
	t.Helper()
	return newTestServer(t, func(o *Options) {
		o.Notify = fakeNotifyChannels{}
		o.NotifyTypes = []string{"feishu", "webhook"}
		if mutate != nil {
			mutate(o)
		}
	})
}

// TestNotifyRead_ListProjectsURLHostOnly —— 地址只以 scheme + host 出站；路径、
// 查询串与凭证没有任何响应载体（`url_host` 是唯一地址字段）。
// 逆改点：把原始 URL 塞进某个字段 / 新增 `url` 字段 → 正文扫描断言红。
// TestNotifyRead_ListProjectsURLHostOnly pins the url_host-only projection.
func TestNotifyRead_ListProjectsURLHostOnly(t *testing.T) {
	const rawURL = "https://hooks.example.com/services/T1/B2?token=sup3r-secret"
	s := newNotifyReadServer(t, func(o *Options) {
		o.Notify = fakeNotifyChannels{channels: []core.NotifyChannel{{
			Name: "ops-hook", Type: "webhook", Description: "运维告警", URL: rawURL,
		}}}
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/notify-channels", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeResourcesEnvelope[[]NotifyChannelView](t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Len(t, env.Data, 1)
	require.Equal(t, "ops-hook", env.Data[0].Name)
	require.Equal(t, "webhook", env.Data[0].Type)
	require.Equal(t, "运维告警", env.Data[0].Description)
	require.Equal(t, "https://hooks.example.com", env.Data[0].URLHost)

	body := rec.Body.String()
	for _, leaked := range []string{"sup3r-secret", "/services/", "B2"} {
		require.NotContains(t, body, leaked, "原始地址的任何片段都不得出站：%s", leaked)
	}
	require.NotContains(t, body, `"url"`, "不存在承载原始 URL 的字段")
}

// TestNotifyRead_UnparseableURLsYieldEmptyHost —— `${VAR}` 引用、相对路径、空串与
// 无 host 的绝对地址都回空串（解析失败不得退回原文案）。
// 逆改点：解析失败时 return raw → 空串断言红。
// TestNotifyRead_UnparseableURLsYieldEmptyHost pins the empty host fallback.
func TestNotifyRead_UnparseableURLsYieldEmptyHost(t *testing.T) {
	s := newNotifyReadServer(t, func(o *Options) {
		o.Notify = fakeNotifyChannels{channels: []core.NotifyChannel{
			{Name: "env-ref", Type: "webhook", URL: "${BASALT_WEBHOOK_URL}"},
			{Name: "relative", Type: "webhook", URL: "hooks/relative"},
			{Name: "empty", Type: "feishu", URL: ""},
			{Name: "no-host", Type: "feishu", URL: "mailto:ops@example.com"},
		}}
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/notify-channels", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeResourcesEnvelope[[]NotifyChannelView](t, rec)
	require.Len(t, env.Data, 4)
	for _, item := range env.Data {
		require.Empty(t, item.URLHost, "%s 的地址解析不出 scheme+host，必须是空串", item.Name)
	}
	require.NotContains(t, rec.Body.String(), "BASALT_WEBHOOK_URL", "环境变量引用名也不得回显")
}

// TestNotifyRead_TypesInAssemblyOrder —— 类型表逐字取装配层的顺序（单一真值），
// 本层不排序、不增删。
// TestNotifyRead_TypesInAssemblyOrder pins the assembly-provided order.
func TestNotifyRead_TypesInAssemblyOrder(t *testing.T) {
	s := newNotifyReadServer(t, func(o *Options) {
		// 故意给非字典序：handler 若自行排序，本断言会红。
		o.NotifyTypes = []string{"webhook", "feishu"}
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/notify-channels/types", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeResourcesEnvelope[[]string](t, rec)
	require.Equal(t, []string{"webhook", "feishu"}, env.Data)
}

// TestNotifyRead_EmptyListsRenderArrays —— 两个端点在空数据上都输出 `[]` 而不是 `null`
// （约束 34）。
// TestNotifyRead_EmptyListsRenderArrays pins the empty-array encoding on both paths.
func TestNotifyRead_EmptyListsRenderArrays(t *testing.T) {
	s := newNotifyReadServer(t, func(o *Options) {
		o.Notify = fakeNotifyChannels{channels: nil}
		o.NotifyTypes = nil
	})

	for _, path := range []string{"/api/v1/notify-channels", "/api/v1/notify-channels/types"} {
		rec := serveAdmin(s, adminRequest(http.MethodGet, path, ""))
		require.Equal(t, http.StatusOK, rec.Code, path)
		require.Contains(t, rec.Body.String(), `"data":[]`, "%s 空列表必须是 []", path)
	}
}

// TestNotifyRead_MissingPortInternal —— 渠道端口未装配是装配错误：固定 500，
// 不是空列表（空列表会让运维以为"配了但没有渠道"）。
// TestNotifyRead_MissingPortInternal pins the fixed 500 for a missing port.
func TestNotifyRead_MissingPortInternal(t *testing.T) {
	s := newTestServer(t, nil) // 缺省 Notify 为 nil
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/notify-channels", ""))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	env := decodeResourcesEnvelope[map[string]any](t, rec)
	require.Equal(t, codeInternal, env.Code)
	require.Equal(t, messageInternal, env.Message)
}

// TestNotifyRead_ListErrorInternal —— 读取失败回固定 500 文案，底层错误只进日志。
// TestNotifyRead_ListErrorInternal pins the fixed 500 without leaking the cause.
func TestNotifyRead_ListErrorInternal(t *testing.T) {
	s := newNotifyReadServer(t, func(o *Options) {
		o.Notify = fakeNotifyChannels{err: errNotifyListFailed}
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/notify-channels", ""))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	env := decodeResourcesEnvelope[map[string]any](t, rec)
	require.Equal(t, codeInternal, env.Code)
	require.Equal(t, messageInternal, env.Message)
	require.Nil(t, env.Data)
	require.NotContains(t, rec.Body.String(), errNotifyListFailed.Error())
}

// TestNotifyRead_RequiresAuth —— 两个端点都在认证闸门之后：缺凭据与错凭据 401，
// 正确凭据 200。
// TestNotifyRead_RequiresAuth pins the auth gate on both endpoints.
func TestNotifyRead_RequiresAuth(t *testing.T) {
	const token = "notify-token"
	paths := []string{"/api/v1/notify-channels", "/api/v1/notify-channels/types"}
	for _, path := range paths {
		// 每个端点用新闸门：累计失败达 10 次会转 429（FR4）。
		newServer := func() *Server {
			return newNotifyReadServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(token) })
		}
		rec := serveAdmin(newServer(), adminRequest(http.MethodGet, path, ""))
		require.Equal(t, http.StatusUnauthorized, rec.Code, "%s 缺凭据必须 401", path)

		rec = serveAdmin(newServer(), withBearer(adminRequest(http.MethodGet, path, ""), "wrong-token"))
		require.Equal(t, http.StatusUnauthorized, rec.Code, "%s 错凭据必须 401", path)

		rec = serveAdmin(newServer(), withBearer(adminRequest(http.MethodGet, path, ""), token))
		require.Equal(t, http.StatusOK, rec.Code, "%s 正确凭据必须放行", path)
	}
}

// TestNotifyRead_ViewShapeMatchesContract —— 真实 HTTP 载荷的字段集与契约
// NotifyChannelView 完全一致并过 schema（地址只能是 url_host）。
// TestNotifyRead_ViewShapeMatchesContract pins the exact contract property set.
func TestNotifyRead_ViewShapeMatchesContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	s := newNotifyReadServer(t, func(o *Options) {
		o.Notify = fakeNotifyChannels{channels: []core.NotifyChannel{{
			Name: "ops-hook", Type: "webhook", Description: "告警", URL: "https://hooks.example.com",
		}}}
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/notify-channels", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	var env resourcesEnvelope[[]map[string]any]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Len(t, env.Data, 1)
	requireResourcePayloadMatches(t, doc, "NotifyChannelView", env.Data[0])
}
