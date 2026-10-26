package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `web_search` 的判卷面（14D T125–T128 / 契约 §5.7）：七个冻结测试名 + 支撑
// 不变量的补充用例。判据只经**工具接口与执行器**观察——不复制工具内部逻辑，也不为测试
// 导出生产 API（假 doer / 假凭据面都在包内）。
// The judging surface of web_search: the seven frozen tests plus supporting invariants.

// webSearchTestKey 是夹具密钥：**不得**出现在任何审计字段、结果正文或错误文案里。
const webSearchTestKey = "sk-test-deepseek-search-key"

// webSearchFixtureBody 是官方响应形态（字段名与 2026-10-26 官方文档逐字一致，含
// date 可空与 authority=-1 两个边界）。
// The official response shape, including a null date and authority -1.
const webSearchFixtureBody = `{"results":[` +
	`{"rerank_score":2.4,"title":"DeepSeek V4 model released","url":"https://example.com/deepseek-v4",` +
	`"snippet":"DeepSeek V4 is now available.","summary":"DeepSeek has released the V4 model.",` +
	`"content":"","site_name":"Example News","site_icon":null,` +
	`"date":"2026-06-01T00:00:00+00:00","authority":3},` +
	`{"rerank_score":1.1,"title":"Second result","url":"https://news.example.com/a",` +
	`"snippet":"second","summary":"second summary","date":null,"authority":-1}]}`

// staticSearchCredential 是固定密钥的假解析面：记账被调用次数（判"校验在取凭据之前"）。
// A fixed-key resolver that counts calls.
type staticSearchCredential struct {
	key    string
	err    error
	called int
}

// ResolveSearchCredential 实现 tool.WebSearchCredentialResolver。
func (c *staticSearchCredential) ResolveSearchCredential(context.Context, string) (string, error) {
	c.called++
	return c.key, c.err
}

// allowDeepSeek 是放行官方端点的假策略（其余与 spyPolicy 同口径：四段式拒 + 记账）。
// A policy stub that admits the search endpoint only.
func allowDeepSeek() *spyPolicy { return &spyPolicy{allowed: []string{"api.deepseek.com"}} }

// webSearchEnv 是工具执行环境的最小真实形态（Agent 名参与判定与凭据解析）。
// The minimal real ToolEnv for a search call.
func webSearchEnv() core.ToolEnv {
	return core.ToolEnv{AgentName: "industry-brief", MaxOutputBytes: 256 * 1024}
}

// recordSleep 返回一个不真等、只记时长的假退避（判重试次数与间隔）。
// A fake backoff that records delays instead of waiting.
func recordSleep(rec *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*rec = append(*rec, d)
		return nil
	}
}

// scriptedResponses 按序返回状态码（用完后恒用最后一个）；200 带官方响应夹具。
// Returns the scripted statuses in order, then repeats the last one.
func scriptedResponses(statuses ...int) func(*http.Request) (*http.Response, error) {
	i := 0
	return func(*http.Request) (*http.Response, error) {
		status := statuses[len(statuses)-1]
		if i < len(statuses) {
			status = statuses[i]
		}
		i++
		body := ""
		if status == http.StatusOK {
			body = webSearchFixtureBody
		}
		return statusResponse(status, body), nil
	}
}

// readRequestBody 读出请求体并解成映射（请求对象由工具构造，工具本身不读 Body）。
// Reads the outbound body back out for assertions.
func readRequestBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.NoError(t, req.Body.Close())
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	return body
}

// TestWebSearch_ParsesResults：官方响应 → 七个字段逐一映射；请求面固定
// （POST 常量端点、Bearer 头、JSON 正文、need_content=false）；缺省 count=10。
func TestWebSearch_ParsesResults(t *testing.T) {
	doer := &fakeDoer{respond: okResponder(http.StatusOK, webSearchFixtureBody)}
	credentials := &staticSearchCredential{key: webSearchTestKey}
	ws := newWebSearchTool(allowDeepSeek(), doer, credentials)

	res, err := ws.Execute(context.Background(), webSearchEnv(),
		jsonInput(t, map[string]any{"query": "DeepSeek V4", "count": 2}))
	require.NoError(t, err)

	var out struct {
		Query   string `json:"query"`
		Results []struct {
			Title       string  `json:"title"`
			URL         string  `json:"url"`
			Snippet     string  `json:"snippet"`
			Summary     string  `json:"summary"`
			Date        string  `json:"date"`
			RerankScore float64 `json:"rerank_score"`
			Authority   int     `json:"authority"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &out))
	require.Equal(t, "DeepSeek V4", out.Query, "输出回显查询词")
	require.Len(t, out.Results, 2)
	first := out.Results[0]
	require.Equal(t, "DeepSeek V4 model released", first.Title)
	require.Equal(t, "https://example.com/deepseek-v4", first.URL)
	require.Equal(t, "DeepSeek V4 is now available.", first.Snippet)
	require.Equal(t, "DeepSeek has released the V4 model.", first.Summary)
	require.Equal(t, "2026-06-01T00:00:00+00:00", first.Date)
	require.InDelta(t, 2.4, first.RerankScore, 1e-9)
	require.Equal(t, 3, first.Authority)
	require.Equal(t, "", out.Results[1].Date, "date 为 null 时映射成空串")
	require.Equal(t, -1, out.Results[1].Authority, "authority=-1（未知）原样保留")
	require.False(t, res.Retryable, "与两件出网工具同口径：不给可重试建议")

	require.Equal(t, 1, credentials.called, "一次工具调用解析一次凭据")
	require.Equal(t, 1, doer.count())
	req := doer.recorded()[0]
	require.Equal(t, http.MethodPost, req.Method)
	require.Equal(t, webSearchEndpoint, req.URL.String())
	require.Equal(t, "Bearer "+webSearchTestKey, req.Header.Get("Authorization"))
	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	body := readRequestBody(t, req)
	require.Equal(t, "DeepSeek V4", body["query"])
	require.Equal(t, float64(2), body["count"])
	require.Equal(t, false, body["need_content"], "need_content 恒 false（正文走 Fetch）")

	t.Run("缺省条数为十", func(t *testing.T) {
		doer := &fakeDoer{respond: okResponder(http.StatusOK, `{"results":[]}`)}
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
		res, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.NoError(t, err)
		require.Contains(t, res.Content, `"results":[]`)
		body := readRequestBody(t, doer.recorded()[0])
		require.Equal(t, float64(webSearchDefaultCount), body["count"], "缺省 10")
	})
}

// TestWebSearch_RunsWithoutBraveEnv：历史 D5 配方依赖 `BRAVE_API_KEY` 与 Brave 私有鉴权头；
// 改用内置工具后这两样都**不得**再被读取或发送（环境里清空也照样成功）。
func TestWebSearch_RunsWithoutBraveEnv(t *testing.T) {
	// t.Setenv 记下原值用于恢复，再 unset 让"变量不存在"成为本用例的真实环境。
	t.Setenv("BRAVE_API_KEY", "placeholder")
	t.Setenv("BRAVE_SEARCH_API_KEY", "placeholder")
	require.NoError(t, os.Unsetenv("BRAVE_API_KEY"))
	require.NoError(t, os.Unsetenv("BRAVE_SEARCH_API_KEY"))

	doer := &fakeDoer{respond: okResponder(http.StatusOK, webSearchFixtureBody)}
	ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})

	res, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"deepseek"}`))
	require.NoError(t, err, "没有 BRAVE_* 也必须成功")
	require.Contains(t, res.Content, `"results"`)

	req := doer.recorded()[0]
	require.Equal(t, "api.deepseek.com", req.URL.Hostname())
	require.Equal(t, "Bearer "+webSearchTestKey, req.Header.Get("Authorization"))
	require.Empty(t, req.Header.Get("X-Subscription-Token"), "不得使用 Brave 私有鉴权头")
}

// TestWebSearch_Retry429Bounded：只对 408 / 429 / 5xx 重发，且最多 2 次（三次尝试）；
// 退避 200ms 起逐次翻倍；重发耗尽后如实报外部错误（不无限重试、不进后台队列）。
func TestWebSearch_Retry429Bounded(t *testing.T) {
	t.Run("第二次成功", func(t *testing.T) {
		doer := &fakeDoer{respond: scriptedResponses(429, 200)}
		credentials := &staticSearchCredential{key: webSearchTestKey}
		var sleeps []time.Duration
		ws := newWebSearchTool(allowDeepSeek(), doer, credentials)
		ws.sleep = recordSleep(&sleeps)

		res, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.NoError(t, err)
		require.Contains(t, res.Content, `"results"`)
		require.Equal(t, 2, doer.count())
		require.Equal(t, []time.Duration{webSearchRetryBase}, sleeps)
		require.Equal(t, 1, credentials.called, "重发不重新解析凭据（同一次调用内）")
	})

	t.Run("第三次成功", func(t *testing.T) {
		doer := &fakeDoer{respond: scriptedResponses(429, 429, 200)}
		var sleeps []time.Duration
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
		ws.sleep = recordSleep(&sleeps)

		_, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.NoError(t, err)
		require.Equal(t, 3, doer.count())
		require.Equal(t, []time.Duration{webSearchRetryBase, 2 * webSearchRetryBase}, sleeps, "退避逐次翻倍")
	})

	for _, status := range []int{429, 503} {
		t.Run("始终 "+strconv.Itoa(status)+" 即停", func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(status, "")}
			var sleeps []time.Duration
			ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
			ws.sleep = recordSleep(&sleeps)

			_, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
			require.Error(t, err)
			require.True(t, core.IsExternalError(err), "外部系统错误")
			require.Equal(t, 1+webSearchMaxRetries, doer.count(), "最多 1 次初始 + 2 次重发")
			require.Len(t, sleeps, webSearchMaxRetries)
			require.Contains(t, err.Error(), strconv.Itoa(status))
			require.Contains(t, err.Error(), "api.deepseek.com")
		})
	}
}

// TestWebSearch_NoRetry401：鉴权与参数类 4xx 一次即停——重发只会重复失败（402 还会重复
// 计费口径）；错误文案自带状态码与 host，不含任何请求材料。
func TestWebSearch_NoRetry401(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 413} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(status, "")}
			var sleeps []time.Duration
			credentials := &staticSearchCredential{key: webSearchTestKey}
			ws := newWebSearchTool(allowDeepSeek(), doer, credentials)
			ws.sleep = recordSleep(&sleeps)

			_, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
			require.Error(t, err)
			require.True(t, core.IsExternalError(err))
			require.Equal(t, 1, doer.count(), "不重试")
			require.Empty(t, sleeps, "不等待")
			require.Contains(t, err.Error(), strconv.Itoa(status))
			require.Contains(t, err.Error(), "api.deepseek.com")
			require.NotContains(t, err.Error(), "Bearer")
			require.NotContains(t, err.Error(), webSearchTestKey)
		})
	}
}

// TestWebSearch_NoSecretInAudit：密钥只随 `Authorization` 头发出——审计入参 / 结果 / 错误
// 文案、回填正文与 WARN 日志里都**不得**出现（成功与失败两条路各判一次）。
func TestWebSearch_NoSecretInAudit(t *testing.T) {
	const secret = "sk-search-secret-do-not-leak-0123456789"

	run := func(t *testing.T, respond func(*http.Request) (*http.Response, error)) (
		core.Result, *fakeInvocationStore, *fakeDoer, *bytes.Buffer,
	) {
		t.Helper()
		doer := &fakeDoer{respond: respond}
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: secret})
		ws.sleep = func(context.Context, time.Duration) error { return nil }
		registry := NewRegistry(slog.New(slog.DiscardHandler))
		require.NoError(t, registry.Register(ws))
		audit := &fakeInvocationStore{}
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))

		res := core.NewToolExecutor(registry, audit, logger).Execute(context.Background(),
			core.ToolEnv{RunID: "run-search", AgentName: "industry-brief", MaxOutputBytes: 256 * 1024},
			[]string{"web_search"},
			core.ToolCall{ID: "call_1", Name: "web_search", Arguments: `{"query":"deepseek"}`})
		return res, audit, doer, &logs
	}

	t.Run("成功路径", func(t *testing.T) {
		res, audit, doer, logs := run(t, okResponder(http.StatusOK, webSearchFixtureBody))
		require.Len(t, audit.rows, 1)
		require.NotContains(t, string(audit.rows[0].inv.InputJSON), secret, "入参审计不得含密钥")
		require.NotContains(t, string(audit.rows[0].result.ResultJSON), secret, "结果审计不得含密钥")
		require.NotContains(t, audit.rows[0].result.ErrorMessage, secret)
		require.NotContains(t, res.Content, secret)
		require.NotContains(t, logs.String(), secret)
		// 正向对照：密钥确实随请求头发出——否则上面的断言全是空验证。
		require.Equal(t, "Bearer "+secret, doer.recorded()[0].Header.Get("Authorization"))
	})

	t.Run("失败路径", func(t *testing.T) {
		res, audit, _, logs := run(t, okResponder(http.StatusInternalServerError, ""))
		require.Len(t, audit.rows, 1)
		require.NotContains(t, audit.rows[0].result.ErrorMessage, secret, "失败审计不得含密钥")
		require.NotContains(t, res.Content, secret)
		require.NotContains(t, logs.String(), secret, "WARN 日志的 detail 不得含密钥")
		require.Contains(t, res.Content, "500", "回填给模型的是自产状态码文案")
	})
}

// TestWebSearch_CountsBudget：一次工具调用 = 一条审计行（预写 + 终态回填），HTTP 层
// 重发**不**额外计行（预算按 `max_tool_calls` 的工具调用数计）——成功与失败同判。
func TestWebSearch_CountsBudget(t *testing.T) {
	execute := func(t *testing.T, respond func(*http.Request) (*http.Response, error)) (
		core.Result, *fakeInvocationStore, *fakeDoer,
	) {
		t.Helper()
		doer := &fakeDoer{respond: respond}
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
		ws.sleep = func(context.Context, time.Duration) error { return nil }
		registry := NewRegistry(slog.New(slog.DiscardHandler))
		require.NoError(t, registry.Register(ws))
		audit := &fakeInvocationStore{}

		res := core.NewToolExecutor(registry, audit, nil).Execute(context.Background(),
			core.ToolEnv{RunID: "run-search", AgentName: "industry-brief", MaxOutputBytes: 256 * 1024},
			[]string{"web_search"},
			core.ToolCall{ID: "call_1", Name: "web_search", Arguments: `{"query":"deepseek"}`})
		return res, audit, doer
	}

	t.Run("重发后成功只记一行", func(t *testing.T) {
		res, audit, doer := execute(t, scriptedResponses(429, 429, 200))
		require.Equal(t, 3, doer.count(), "HTTP 层确实重发了两次")
		require.Len(t, audit.rows, 1, "三次 HTTP 尝试仍只记一次工具调用")
		require.Equal(t, core.LLMCallSuccess, audit.rows[0].status)
		require.True(t, audit.rows[0].terminal)
		require.Contains(t, res.Content, `"results"`)
	})

	t.Run("重发耗尽失败也只记一行", func(t *testing.T) {
		res, audit, doer := execute(t, okResponder(http.StatusServiceUnavailable, ""))
		require.Equal(t, 1+webSearchMaxRetries, doer.count())
		require.Len(t, audit.rows, 1)
		require.Equal(t, core.LLMCallFailed, audit.rows[0].status)
		require.Equal(t, core.ErrorKindExternalError, audit.rows[0].result.ErrorKind)
		require.Contains(t, res.Content, "503")
	})
}

// TestWebSearch_FailureLeavesNoSources：失败**不编造**来源——工具返回零结果 + 错误（不会
// 出现半截 results）；空结果是合法的空数组；不可用条目被丢弃而不是补字段。
func TestWebSearch_FailureLeavesNoSources(t *testing.T) {
	t.Run("HTTP 失败无结果体", func(t *testing.T) {
		doer := &fakeDoer{respond: okResponder(http.StatusUnauthorized, "")}
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
		res, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.Error(t, err)
		require.Empty(t, res.Content, "失败时工具不返回结果正文")
		require.NotContains(t, res.Content, `"results"`)

		// 经执行器：回填文本是错误文案，同样没有任何 sources 形状。
		registry := NewRegistry(slog.New(slog.DiscardHandler))
		require.NoError(t, registry.Register(ws))
		audit := &fakeInvocationStore{}
		out := core.NewToolExecutor(registry, audit, nil).Execute(context.Background(),
			core.ToolEnv{RunID: "run-search", AgentName: "industry-brief", MaxOutputBytes: 256 * 1024},
			[]string{"web_search"},
			core.ToolCall{ID: "call_1", Name: "web_search", Arguments: `{"query":"x"}`})
		require.NotContains(t, out.Content, `"results"`)
		require.Contains(t, out.Content, "401")
	})

	t.Run("空结果如实为空数组", func(t *testing.T) {
		doer := &fakeDoer{respond: okResponder(http.StatusOK, `{"results":[]}`)}
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
		res, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.NoError(t, err)
		require.Contains(t, res.Content, `"results":[]`, "空数组而不是 null")
		require.NotContains(t, res.Content, `"results":null`)
	})

	t.Run("不可用条目被丢弃", func(t *testing.T) {
		body := `{"results":[` +
			`{"title":"good","url":"https://ok.example.com/a","snippet":"","summary":"","date":null,"authority":0},` +
			`{"title":"no url","url":"","snippet":"","summary":"","date":null,"authority":0},` +
			`{"title":"","url":"https://blank-title.example.com/a","snippet":"","summary":"","date":null,"authority":0},` +
			`{"title":"ftp","url":"ftp://files.example.com/a","snippet":"","summary":"","date":null,"authority":0},` +
			`{"title":"userinfo","url":"https://user:pass@evil.example.com/a","snippet":"","summary":"","date":null,"authority":0}` +
			`]}`
		doer := &fakeDoer{respond: okResponder(http.StatusOK, body)}
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
		res, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.NoError(t, err)

		var out struct {
			Results []struct {
				Title string `json:"title"`
				URL   string `json:"url"`
			} `json:"results"`
		}
		require.NoError(t, json.Unmarshal([]byte(res.Content), &out))
		require.Len(t, out.Results, 1, "只有可用条目保留（不补造缺失字段）")
		require.Equal(t, "good", out.Results[0].Title)
		require.Equal(t, "https://ok.example.com/a", out.Results[0].URL)
	})
}

// TestWebSearch_InputValidation：取值校验在**取凭据与外呼之前**拒（零解析、零发送），
// 失败类别是 invalid_input。
func TestWebSearch_InputValidation(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"空查询", `{"query":""}`},
		{"全空白查询", `{"query":"   "}`},
		{"超长查询", `{"query":"` + strings.Repeat("地", webSearchMaxQueryRune+1) + `"}`},
		{"条数为零", `{"query":"x","count":0}`},
		{"条数超上限", `{"query":"x","count":51}`},
		{"条数负值", `{"query":"x","count":-1}`},
		{"条数非整数", `{"query":"x","count":2.5}`},
		{"查询非字符串", `{"query":42}`},
		{"非法 JSON", `{"query":}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{respond: okResponder(http.StatusOK, webSearchFixtureBody)}
			credentials := &staticSearchCredential{key: webSearchTestKey}
			ws := newWebSearchTool(allowDeepSeek(), doer, credentials)

			_, err := ws.Execute(context.Background(), webSearchEnv(), []byte(tc.input))
			require.Error(t, err)
			require.True(t, core.IsInvalidInput(err), "入参非法类别")
			require.Equal(t, 0, doer.count(), "零发送")
			require.Equal(t, 0, credentials.called, "零凭据解析")
		})
	}

	t.Run("128 字符边界放行", func(t *testing.T) {
		doer := &fakeDoer{respond: okResponder(http.StatusOK, `{"results":[]}`)}
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
		query := strings.Repeat("地", webSearchMaxQueryRune)
		input, marshalErr := json.Marshal(map[string]any{"query": query})
		require.NoError(t, marshalErr)

		_, err := ws.Execute(context.Background(), webSearchEnv(), input)
		require.NoError(t, err, "恰好 128 个 Unicode 字符必须放行")
		require.Equal(t, 1, doer.count())
	})
}

// TestWebSearch_PolicyDeniedNoSend：判定归 core.Policy——被拒即 sandbox_denied，零发送、
// 零凭据解析（拒绝发生在取密钥之前）。
func TestWebSearch_PolicyDeniedNoSend(t *testing.T) {
	doer := &fakeDoer{respond: okResponder(http.StatusOK, webSearchFixtureBody)}
	credentials := &staticSearchCredential{key: webSearchTestKey}
	policy := &spyPolicy{allowed: []string{"other.example.com"}} // 不含 api.deepseek.com
	ws := newWebSearchTool(policy, doer, credentials)

	_, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err), "策略拒绝归 sandbox_denied")
	require.Equal(t, 0, doer.count(), "零发送")
	require.Equal(t, 0, credentials.called, "零凭据解析")
	require.Equal(t, 1, policy.denials())
	require.Equal(t, []string{webSearchEndpoint}, policy.targets(), "判定目标是常量端点")
}

// TestWebSearch_MissingCredentialFailsClosed：没有解析面 / 解析失败 / 空密钥一律失败
// 关闭——零发送；错误只点名 Agent，不带任何材料。
func TestWebSearch_MissingCredentialFailsClosed(t *testing.T) {
	t.Run("未接解析面", func(t *testing.T) {
		doer := &fakeDoer{respond: okResponder(http.StatusOK, webSearchFixtureBody)}
		ws := newWebSearchTool(allowDeepSeek(), doer, nil)
		_, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "industry-brief")
		require.Equal(t, 0, doer.count())
	})

	t.Run("解析失败", func(t *testing.T) {
		doer := &fakeDoer{respond: okResponder(http.StatusOK, webSearchFixtureBody)}
		ws := newWebSearchTool(allowDeepSeek(), doer,
			&staticSearchCredential{err: errors.New("凭据已撤销")})
		_, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "industry-brief")
		require.Equal(t, 0, doer.count())
	})

	t.Run("解析为空", func(t *testing.T) {
		doer := &fakeDoer{respond: okResponder(http.StatusOK, webSearchFixtureBody)}
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: "  "})
		_, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "industry-brief")
		require.Equal(t, 0, doer.count())
	})
}

// TestWebSearch_TransportErrors：传输层失败归外部系统错误且**不重发**；受控拨号的策略
// 拒绝原样上抛（sandbox_denied），不包成 ExternalError。
func TestWebSearch_TransportErrors(t *testing.T) {
	t.Run("连接失败不重发", func(t *testing.T) {
		doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: connect: connection refused")
		}}
		var sleeps []time.Duration
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
		ws.sleep = recordSleep(&sleeps)

		_, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.Error(t, err)
		require.True(t, core.IsExternalError(err))
		require.Equal(t, 1, doer.count(), "传输失败不重发")
		require.Empty(t, sleeps)
	})

	t.Run("受控拨号拒绝原样透传", func(t *testing.T) {
		violation := &core.SandboxViolationError{
			Rule: "受保护地址段", Target: "api.deepseek.com", Guidance: "测试夹具",
		}
		doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
			return nil, violation
		}}
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
		_, err := ws.Execute(context.Background(), webSearchEnv(), []byte(`{"query":"x"}`))
		require.Error(t, err)
		require.True(t, core.IsSandboxViolation(err), "策略拒绝不得被包成外部错误")
	})
}

// TestWebSearch_BackoffHonoursContext：取消 / 截止在退避等待期间到达即按身份上抛
// （执行器据此归 timeout），不吞、不等满。
func TestWebSearch_BackoffHonoursContext(t *testing.T) {
	t.Run("等待期取消", func(t *testing.T) {
		start := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := sleepContext(ctx, time.Hour)
		require.ErrorIs(t, err, context.Canceled)
		require.Less(t, time.Since(start), time.Second, "取消后不得等满退避")
	})

	t.Run("重发前取消", func(t *testing.T) {
		doer := &fakeDoer{respond: okResponder(http.StatusTooManyRequests, "")}
		ws := newWebSearchTool(allowDeepSeek(), doer, &staticSearchCredential{key: webSearchTestKey})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		_, err := ws.Execute(ctx, webSearchEnv(), []byte(`{"query":"x"}`))
		require.ErrorIs(t, err, context.Canceled)
		require.Less(t, time.Since(start), time.Second)
		require.Equal(t, 1, doer.count(), "取消后不再重发")
	})
}
