//go:build e2e

package e2e

// 本文件是回填夹具的素材层：假模型剧本、回环接收器、MCP stdio helper 与配置 / 画像
// 构造器。素材一律复用 internal/testutil（模块卡 §3.1.1：**不重写**假模型、HTTP 回环、
// MCP 服务与工作区夹具），本文件只补"剧本长什么样"。
// This file holds the fixtures' materials: fake-model scripts, a loopback receiver, the
// MCP stdio helper, and configuration/agent builders. The transports themselves are
// reused from internal/testutil; only the script shapes live here.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ymedword/basalt/internal/testutil"
)

// 假模型剧本的固定缺省值：provider 名与模型名贯穿配置与 AGENT.md，只在这里定义一次。
// The fixture's fixed provider and model names are defined here once.
const (
	fixtureProvider = "fake"
	fixtureModel    = "fake-model"
	// fixtureKeyEnv 是 api_key 占位引用的变量名；值只在运行时经 env 注入。
	// fixtureKeyEnv names the variable the api_key placeholder refers to.
	fixtureKeyEnv = "BASALT_E2E_KEY"
	// mcpHelperEnvRef 是 mcp_servers.yaml 里 env 占位引用的变量名：父进程把它设为 1，
	// 装配层解析后子进程拿到 BASALT_MCP_HELPER=1（helper 入口据此服务 stdio）。
	// mcpHelperEnvRef is the placeholder variable the MCP helper entry reads after
	// assembly resolves it into BASALT_MCP_HELPER.
	mcpHelperEnvRef = "BASALT_E2E_MCP_HELPER"
)

// mustJSON 序列化固定剧本；剧本只含字符串与容器，失败即夹具写错，直接 panic。
// mustJSON serializes a fixed script; a failure means the fixture itself is broken.
func mustJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic("e2e fixture: 序列化固定剧本失败: " + err.Error())
	}
	return string(raw)
}

// textResponse 是一条纯文本助手回复。
// textResponse is a plain assistant text reply.
func textResponse(content string) testutil.FakeOpenAIResponse {
	return testutil.FakeOpenAIResponse{Body: mustJSON(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": content},
		}},
	})}
}

// usageResponse 是带用量统计的文本回复（判 `llm_calls` 的 token 列用）。
// usageResponse is a text reply carrying usage (the llm_calls token columns).
func usageResponse(content string, promptTokens, completionTokens int) testutil.FakeOpenAIResponse {
	return testutil.FakeOpenAIResponse{Body: mustJSON(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": content},
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	})}
}

// toolCallResponse 是一条工具调用回复：content 为 null、tool_calls 带一函数。
// toolCallResponse is a tool-call reply: null content plus one function call.
func toolCallResponse(id, name, arguments string) testutil.FakeOpenAIResponse {
	return testutil.FakeOpenAIResponse{Body: mustJSON(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": arguments,
					},
				}},
			},
		}},
	})}
}

// reasoningToolCallResponse 是同时携带 reasoning_content 与工具调用的回复：
// 推理字段两跳保真（US-4 §6.1 Demo ④）与「清空字段必须 400」（Demo ③ 负例）共用。
// reasoningToolCallResponse carries reasoning_content next to a tool call: one fixture
// for the round-trip demo and for the "missing field must 400" negative case.
func reasoningToolCallResponse(id, name, arguments, reasoning string) testutil.FakeOpenAIResponse {
	return testutil.FakeOpenAIResponse{Body: mustJSON(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":              "assistant",
				"content":           nil,
				"reasoning_content": reasoning,
				"tool_calls": []any{map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": arguments,
					},
				}},
			},
		}},
	})}
}

// reasoningResponse 是同时携带 reasoning_content 的回复（推理字段两跳保真用）。
// reasoningResponse carries reasoning_content next to the visible answer.
func reasoningResponse(content, reasoning string) testutil.FakeOpenAIResponse {
	return testutil.FakeOpenAIResponse{Body: mustJSON(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":              "assistant",
				"content":           content,
				"reasoning_content": reasoning,
			},
		}},
	})}
}

// errorResponse 是供应商错误：非 2xx + 只含 `error.message` 的信封。
// errorResponse is a provider error: a non-2xx status with an error envelope.
func errorResponse(status int, message string) testutil.FakeOpenAIResponse {
	return testutil.FakeOpenAIResponse{StatusCode: status, Body: mustJSON(map[string]any{
		"error": map[string]any{"message": message},
	})}
}

// newFakeModel 起回环假模型（脚本用尽即判本用例红——防用例少写一步而静默通过）。
// newFakeModel starts the loopback fake model; an exhausted script fails this test.
func newFakeModel(t *testing.T, steps ...testutil.FakeOpenAIResponse) *testutil.FakeOpenAIServer {
	t.Helper()
	return testutil.NewFakeOpenAIServer(t, steps...)
}

// receiverRequest 是接收器记下的一次请求。
// receiverRequest is one request recorded by the receiver.
type receiverRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

// receiver 是本地回环接收器：记录每次请求，按设定状态与响应体答复。
// receiver is a loopback receiver: it records every request and replays the
// configured status and body.
type receiver struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	recorded []receiverRequest
	status   int
	body     string
}

// newReceiver 起回环接收器（默认 200 + `{"ok":true}`）。
// newReceiver starts the receiver with a 200 default.
func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{t: t, status: http.StatusOK, body: `{"ok":true}`}
	r.server = testutil.NewHTTPServer(t, http.HandlerFunc(r.handle))
	return r
}

// URL 返回接收器地址（供 notify 渠道登记）。
// URL returns the receiver address.
func (r *receiver) URL() string { return r.server.URL }

// setResponse 改响应的状态与正文（用例中途切换成功 / 失败路径）。
// setResponse switches the replayed status and body.
func (r *receiver) setResponse(status int, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.body = status, body
}

// requests 返回已收到的请求副本。
// requests returns a copy of the recorded requests.
func (r *receiver) requests() []receiverRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]receiverRequest(nil), r.recorded...)
}

func (r *receiver) handle(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		r.t.Errorf("回环接收器：读请求体失败: %v", err)
	}
	r.mu.Lock()
	r.recorded = append(r.recorded, receiverRequest{
		Method: req.Method,
		Path:   req.URL.Path,
		Header: req.Header.Clone(),
		Body:   string(body),
	})
	status, reply := r.status, r.body
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, reply); err != nil {
		r.t.Errorf("回环接收器：写响应失败: %v", err)
	}
}

// writeMCPServers 落一份 servers 列表形态的 mcp_servers.yaml。
// writeMCPServers writes an mcp_servers.yaml in the servers-list shape.
func (h *harness) writeMCPServers(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(h.Root, ".basalt", "mcp_servers.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建 mcp_servers.yaml 目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写 mcp_servers.yaml 失败: %v", err)
	}
}

// mcpHelperCommand 返回 mcp_servers.yaml 里 `command` 的取值：本测试二进制自举为
// stdio server（`-test.run` 只选中 helper 入口，TestMain 见 BASALT_MCP_HELPER 短路）。
// 配置面按空白切分命令，故要求测试二进制路径不含空格（Go 的临时构建目录满足）。
// mcpHelperCommand returns the helper command line: this very test binary re-executed
// as a stdio server. The configuration splits on whitespace, so the path must not
// contain spaces, which the Go build directory satisfies.
func mcpHelperCommand() string {
	return os.Args[0] + " -test.run=^TestMCPHelperProcess$"
}

// mcpServersYAML 组装一份单 server 的配置：command 自举 helper，env 经占位注入。
// mcpServersYAML assembles a single-server configuration whose env arrives through
// the placeholder the assembly layer resolves.
func mcpServersYAML(name string) string {
	return fmt.Sprintf("servers:\n  - name: %s\n    command: %s\n    env:\n      BASALT_MCP_HELPER: ${%s}\n",
		name, mcpHelperCommand(), mcpHelperEnvRef)
}

// TestMCPHelperProcess 是 MCP helper 的子进程入口：仅在 BASALT_MCP_HELPER 置位时服务
// stdio；默认运行下 testutil.RunMCPHelper 自行 SKIP（它是入口，不是用例）。
// TestMCPHelperProcess is the MCP helper subprocess entry; it serves stdio only when
// BASALT_MCP_HELPER is set and skips otherwise.
func TestMCPHelperProcess(t *testing.T) {
	testutil.RunMCPHelper(t)
}

// configWithProvider 返回最小可装载配置：一家指向回环假模型的 provider。
// 凭证只以 ${VAR} 占位出现，真实值由用例经 env 注入。
// configWithProvider returns the minimal loadable configuration pointing at the fake
// loopback model; the credential is a placeholder resolved from the environment.
func configWithProvider(baseURL string) string {
	return "providers:\n" +
		"  - name: " + fixtureProvider + "\n" +
		"    base_url: " + baseURL + "\n" +
		"    api_key: ${" + fixtureKeyEnv + "}\n" +
		"    default_model: " + fixtureModel + "\n"
}

// fixtureEnv 返回每次调用都要带上的环境：凭证哨兵（值不落盘、不进记录）。
// fixtureEnv returns the credential sentinel every invocation carries.
func fixtureEnv() []string {
	return []string{fixtureKeyEnv + "=e2e-sentinel-not-a-secret"}
}

// agentFrontmatter 组装一份可装载的 AGENT.md frontmatter（provider 指向夹具假模型）。
// agentFrontmatter assembles a loadable AGENT.md frontmatter pointing at the fixture.
func agentFrontmatter(name string, tools []string) string {
	out := "name: " + name + "\n" +
		"description: e2e 回填夹具\n" +
		"provider:\n  name: " + fixtureProvider + "\n  model: " + fixtureModel + "\n"
	if len(tools) == 0 {
		return out + "tools: []\n"
	}
	out += "tools:\n"
	for _, tool := range tools {
		out += "  - " + tool + "\n"
	}
	return out
}

// scalarInt 读一行标量计数（判"零调用 / 一行审计"这类读数）。
// scalarInt reads one scalar count.
func scalarInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("查询计数失败 %q: %v", query, err)
	}
	return n
}
