package releasekit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUS17_Smoke_HelperDefaults(t *testing.T) {
	if got := smokeChecks(Target{Tier: TierFull}); len(got) != 4 {
		t.Fatalf("full 档应有 4 项检查，实际 %v", got)
	}
	if got := smokeChecks(Target{Tier: TierBinary}); len(got) != 2 {
		t.Fatalf("binary 档应有 2 项检查，实际 %v", got)
	}
	if got := smokeTokenName(ReleaseOptions{}); got != smokeTokenEnv {
		t.Fatalf("缺省 token 变量名 = %q", got)
	}
	if got := smokeTokenName(ReleaseOptions{AdminTokenEnv: "CUSTOM_TOKEN"}); got != "CUSTOM_TOKEN" {
		t.Fatalf("自定义 token 变量名 = %q", got)
	}
	if got := smokeAgent(ReleaseOptions{}); got != smokeAgentDefault {
		t.Fatalf("缺省 Agent = %q", got)
	}
	if got := smokeAgent(ReleaseOptions{Agent: "ops"}); got != "ops" {
		t.Fatalf("自定义 Agent = %q", got)
	}
	if got := versionFromPackage("basalt-1.2.3-linux-amd64.tar.gz"); got != "1.2.3" {
		t.Fatalf("版本解析 = %q，期望 1.2.3", got)
	}
	if got := versionFromPackage("basalt"); got != "" {
		t.Fatalf("单段名应返回空版本，实际 %q", got)
	}
}

// 中文验收点：管理台必须返回 HTML 壳、任务必须 completed，否则烟测失败。
// The console must return an HTML shell and the task must complete, or the smoke
// run fails.
func TestUS17_Smoke_ConsoleAndTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case request.URL.Path == "/console":
			_, _ = writer.Write([]byte("<!DOCTYPE html><html></html>"))
		case request.URL.Path == "/api/v1/sessions":
			_, _ = writer.Write([]byte(`{"data":{"session_id":"s1"}}`))
		case request.URL.Path == "/api/v1/sessions/s1/messages":
			_, _ = writer.Write([]byte(`{"data":{"status":"completed"}}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	if err := smokeConsole(ctx, server.URL, "secret"); err != nil {
		t.Fatalf("HTML 管理台应通过：%v", err)
	}
	if err := smokeTask(ctx, server.URL, "secret", "demo"); err != nil {
		t.Fatalf("completed 任务应通过：%v", err)
	}
	if err := smokeConsole(ctx, server.URL, "wrong"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误 token 应报 HTTP 401，实际 %v", err)
	}
}

func TestUS17_Smoke_RejectsBadResponses(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		run     func(base string) error
	}{
		{
			"非 HTML 管理台",
			func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte("{}")) },
			func(base string) error { return smokeConsole(context.Background(), base, "t") },
		},
		{
			"建会话失败",
			func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusInternalServerError) },
			func(base string) error { return smokeTask(context.Background(), base, "t", "demo") },
		},
		{
			"缺 session_id",
			func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(`{"data":{}}`)) },
			func(base string) error { return smokeTask(context.Background(), base, "t", "demo") },
		},
		{
			"任务未完成",
			func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api/v1/sessions" {
					_, _ = writer.Write([]byte(`{"data":{"session_id":"s1"}}`))
					return
				}
				_, _ = writer.Write([]byte(`{"data":{"status":"failed","reason":"boom"}}`))
			},
			func(base string) error { return smokeTask(context.Background(), base, "t", "demo") },
		},
	}
	for _, testCase := range cases {
		server := httptest.NewServer(testCase.handler)
		err := testCase.run(server.URL)
		server.Close()
		if err == nil {
			t.Fatalf("%s: 应返回错误", testCase.name)
		}
	}
}

func TestUS17_Smoke_DecodeAndRequest(t *testing.T) {
	var decoded struct {
		Value string `json:"value"`
	}
	if err := decodeSmokeData([]byte(`{"data":{"value":"x"}}`), &decoded); err != nil || decoded.Value != "x" {
		t.Fatalf("正常 data 应解出：%+v, %v", decoded, err)
	}
	if err := decodeSmokeData([]byte(`{"data":null}`), &decoded); err == nil {
		t.Fatal("null data 应报错")
	}
	if err := decodeSmokeData([]byte(`[`), &decoded); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if _, _, err := smokeRequest(context.Background(), http.MethodGet, "://bad", "t", nil); err == nil {
		t.Fatal("非法 URL 应报错")
	}
	body, err := json.Marshal(map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("构造请求体：%v", err)
	}
	if len(body) == 0 {
		t.Fatal("请求体不应为空")
	}
}
