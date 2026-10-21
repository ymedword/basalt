//go:build e2e && real

package e2e

// 本文件是模块卡 §4.5 的**已授权乙类执行面**：真实模型（OpenAI 兼容端点）与真实飞书
// 外发。它只在收口时、用户明确授权后由 `us15-real` stage 编译执行，**不进入** CI 与
// `current.json`；默认 `make check` / `go test ./...` 都不会选中 `real` 标签。
//
// 三条硬约束（违反即 FAIL，不得降级）：
//   - 环境变量缺失 → `t.Fatalf`（FAIL，**不得** SKIP）；不复用 `ANTHROPIC_*` 变量；
//   - 模型调用 ≤30 次、飞书发送 ≤5 次，用包级原子计数封顶，超限即停；
//   - 证据只允许 `scheme://host`：完整 webhook（含 path / query / userinfo）不得进入
//     进程输出、断言文本、审计或工作区；调用失败文案先经 `h.redact` 脱敏。
//
// This file is the authorized B-class execution surface of §4.5: real model calls and
// real Feishu sends. It is compiled only for the `us15-real` stage after explicit user
// authorization. Missing environment fails (never skips); model calls and sends are
// capped atomically; evidence carries only scheme://host.

import (
	"database/sql"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// realProviderName 是临时 basalt.yaml 里的 provider 名（映射表按名查询）。
	// realProviderName is the provider name in the temporary basalt.yaml.
	realProviderName = "real"
	// realModelCallLimit / realFeishuSendLimit 是授权 scope 的次数上限（模块卡 §4.5）。
	// 执法方式是事后累计判红：模型调用按子测试结束后的 llm_calls 行数累计，飞书
	// 发送在返回后累计；超限当轮即失败，但已发生的调用 / 外发无法撤回。事前逐次
	// 封顶需要生产代码提供调用回调（§4.5 范围外）；实测使用量远低于上限
	// （21/30 次调用、1/5 次发送），边界记入 run-state §8.5。
	// The per-run caps from the authorization scope are enforced post hoc: model
	// calls are counted from llm_calls after each subtest and Feishu sends after
	// returning; an overrun fails the run but cannot undo the call. Pre-emptive
	// per-call blocking would need production callbacks (out of §4.5 scope).
	realModelCallLimit  = 30
	realFeishuSendLimit = 5
	// 四个环境变量名与 §4.5 契约逐字一致；值只在运行时经环境变量注入。
	// The four environment variable names match §4.5 verbatim.
	realLLMBaseURLEnv = "BASALT_REAL_LLM_BASE_URL"
	realLLMModelEnv   = "BASALT_REAL_LLM_MODEL"
	realLLMAPIKeyEnv  = "BASALT_REAL_LLM_API_KEY"
	realFeishuURLEnv  = "BASALT_REAL_FEISHU_URL"
	// realRunTimeout 是单条短命命令的上限：真实服务往返远慢于回环假模型。
	// realRunTimeout bounds one-shot commands; real round trips are far slower.
	realRunTimeout = 240 * time.Second
	// realWeatherCity 是回环天气服务的城市字段哨兵（只验模型是否读到了响应）。
	// realWeatherCity is the weather service's city sentinel.
	realWeatherCity = "真实城市-6d3c"
)

// 包级原子计数跨子测试累计（同一 `go test` 进程内），超限即判红。
// Package-level atomics accumulate across subtests; exceeding a cap fails the test.
var (
	realModelCalls  atomic.Int64
	realModelTokens atomic.Int64
	realFeishuSends atomic.Int64
)

// realLLM 是真实 provider 的三项运行时读数；APIKey 只进子进程环境，不落盘、不打印。
// realLLM carries the three runtime readings; the API key only reaches the child env.
type realLLM struct {
	BaseURL string
	Model   string
	APIKey  string
}

// TestBackfill_Real 覆盖已授权乙类的真实模型半：US-2 provider、US-4 多轮 / 工具 / 重启、
// US-5 回环天气、US-6 Memory、US-7 MCP、US-9 web invoke、US-10 调度触发与归属。
// 十一个子测试名与 `backfillCatalog` 的 `TestBackfill_Real/` 全名逐字对应。
// TestBackfill_Real covers the authorized real-model half; the eleven subtests match
// the catalog names verbatim.
func TestBackfill_Real(t *testing.T) {
	t.Run("provider_call", testRealProviderCall)
	t.Run("multiturn", testRealMultiturn)
	t.Run("chat_tool", testRealChatTool)
	t.Run("weather_tool", testRealWeatherTool)
	t.Run("memory_save", testRealMemorySave)
	t.Run("mcp_tool", testRealMCPTool)
	t.Run("scheduler_fire", testRealSchedulerFire)
	t.Run("scheduler_manage_self", testRealSchedulerManageSelf)
	t.Run("scheduler_manage_other_denied", testRealSchedulerManageOtherDenied)
	t.Run("restart_session", testRealRestartSession)
	t.Run("web_invoke_tool", testRealWebInvokeTool)
	t.Logf("real_model_calls=%d real_model_tokens=%d", realModelCalls.Load(), realModelTokens.Load())
}

// TestBackfill_RealFeishu 覆盖已授权乙类的真实飞书外发：发送与脱敏。
// TestBackfill_RealFeishu covers the authorized real Feishu send and its redaction.
func TestBackfill_RealFeishu(t *testing.T) {
	t.Run("send", testRealFeishuSend)
	t.Run("redaction", testRealFeishuRedaction)
	t.Logf("real_feishu_sends=%d", realFeishuSends.Load())
}

// requireRealLLMEnv 读取三个真实模型环境变量；缺失即 FAIL（不 SKIP），错误只点名变量。
// requireRealLLMEnv reads the three real-model variables; missing ones fail (never
// skip) and the message names only the variables, never their values.
func requireRealLLMEnv(t *testing.T) realLLM {
	t.Helper()
	required := []string{realLLMBaseURLEnv, realLLMModelEnv, realLLMAPIKeyEnv}
	var missing []string
	for _, key := range required {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("真实模型回填缺少环境变量 %v——按模块卡 §4.5，缺少凭证即 FAIL，不得 SKIP", missing)
	}
	base := strings.TrimSpace(os.Getenv(realLLMBaseURLEnv))
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		t.Fatalf("%s 不是绝对 URL（需要 scheme://host）", realLLMBaseURLEnv)
	}
	return realLLM{
		BaseURL: base,
		Model:   strings.TrimSpace(os.Getenv(realLLMModelEnv)),
		APIKey:  os.Getenv(realLLMAPIKeyEnv),
	}
}

// requireRealFeishuURL 读取真实飞书 webhook；缺失即 FAIL。返回值与原文只为拼配置与
// 登记脱敏，失败文案只出 `scheme://host`。
// requireRealFeishuURL reads the real webhook; diagnostics carry scheme://host only.
func requireRealFeishuURL(t *testing.T) (raw string, parsed *url.URL) {
	t.Helper()
	raw = strings.TrimSpace(os.Getenv(realFeishuURLEnv))
	if raw == "" {
		t.Fatalf("真实飞书外发缺少环境变量 %s——按模块卡 §4.5，缺少凭证即 FAIL，不得 SKIP", realFeishuURLEnv)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Fatalf("%s 不是绝对 URL（需要 scheme://host）", realFeishuURLEnv)
	}
	if u.Path == "" && u.RawQuery == "" && u.User == nil {
		t.Fatalf("%s 缺少可判定的令牌段（path / query / userinfo 均为空）", realFeishuURLEnv)
	}
	return raw, u
}

// newRealHarness 建工作区夹具并登记需要脱敏的运行时值（base URL 与 key 只存内存）。
// newRealHarness builds the fixture and registers the runtime values for redaction.
func newRealHarness(t *testing.T, llm realLLM) *harness {
	t.Helper()
	h := newHarness(t)
	h.registerSecret(realLLMBaseURLEnv, llm.BaseURL)
	h.registerSecret(realLLMAPIKeyEnv, llm.APIKey)
	h.setRunTimeout(realRunTimeout)
	return h
}

// realEnv 返回调用真实 provider 所需的环境：真实 key 与管理面 token（serve 场景
// 需要后者；token 值是夹具哨兵，不是真实凭证）。
// realEnv returns the child environment: the real API key plus the management token
// the serve scenarios need.
func realEnv(llm realLLM) []string {
	return []string{
		realLLMAPIKeyEnv + "=" + llm.APIKey,
		e2eAdminTokenEnv + "=" + e2eAdminToken,
	}
}

// realConfig 组装临时 basalt.yaml：provider `real` 指向运行时给出的端点，`api_key` 只写
// `${VAR}` 占位；extra 追加 http 段等场景配置。
// realConfig assembles the temporary basalt.yaml; extra appends scenario sections.
func realConfig(llm realLLM, extra string) string {
	return "providers:\n" +
		"  - name: " + realProviderName + "\n" +
		"    base_url: " + llm.BaseURL + "\n" +
		"    api_key: ${" + realLLMAPIKeyEnv + "}\n" +
		"    default_model: \"" + llm.Model + "\"\n" + extra
}

// realHTTPBlock 组装 http 段：addr 与 allowed_domains 都可省略（空段返回空串）；
// 有段时必带管理认证（T025 起 serve 没有它拒绝启动）。
// realHTTPBlock renders the http section; when non-empty it always carries the
// management-auth block serve requires since T025.
func realHTTPBlock(addr string, domains []string) string {
	if addr == "" && len(domains) == 0 {
		return ""
	}
	out := "http:\n" + adminAuthSection()
	if addr != "" {
		out += "  addr: " + addr + "\n"
	}
	if len(domains) > 0 {
		out += "  allowed_domains:\n"
		for _, domain := range domains {
			out += "    - " + domain + "\n"
		}
	}
	return out
}

// realAgentFrontmatter 组装指向 `real` provider 的 AGENT.md frontmatter。
// realAgentFrontmatter assembles frontmatter pointing at the real provider.
func realAgentFrontmatter(name, model string, tools, servers []string) string {
	out := "name: " + name + "\n" +
		"description: e2e 真实模型回填夹具\n" +
		"provider:\n  name: " + realProviderName + "\n  model: \"" + model + "\"\n"
	if len(tools) == 0 {
		out += "tools: []\n"
	} else {
		out += "tools:\n"
		for _, tool := range tools {
			out += "  - " + tool + "\n"
		}
	}
	if len(servers) == 0 {
		return out + "mcp_servers: []\n"
	}
	out += "mcp_servers:\n"
	for _, server := range servers {
		out += "  - " + server + "\n"
	}
	return out
}

// requireRealExit 断言退出码；失败文案经 redact 脱敏后输出。
// requireRealExit asserts the exit code with redacted diagnostics.
func requireRealExit(t *testing.T, h *harness, res runResult, want int, label string) {
	t.Helper()
	if res.ExitCode != want {
		t.Fatalf("%s：退出码 = %d，期望 %d\n%s", label, res.ExitCode, want, h.redact(res.Stdout+"\n"+res.Stderr))
	}
}

// requireRealContains 断言文本含片段；失败文案经 redact 脱敏。
// requireRealContains asserts the fragment with redacted diagnostics.
func requireRealContains(t *testing.T, h *harness, text, want, label string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%s：未找到 %q\n%s", label, want, h.redact(text))
	}
}

// accountRealModelCalls 累计一次子测试的模型调用：全部行必须 success 且 provider /
// model 与运行时读数一致；累计超过 30 次即停。
// accountRealModelCalls accumulates one subtest's calls; every row must be a success
// for the configured provider/model, and the cumulative count is capped.
func accountRealModelCalls(t *testing.T, llm realLLM, db *sql.DB, label string) {
	t.Helper()
	rows := queryRows(t, db, `SELECT provider, model, prompt_tokens, completion_tokens, status FROM llm_calls`)
	var tokens int64
	for _, row := range rows {
		if got := stringValue(t, row, "provider"); got != realProviderName {
			t.Fatalf("%s：llm_calls.provider = %q，期望 %q", label, got, realProviderName)
		}
		if got := stringValue(t, row, "model"); got != llm.Model {
			t.Fatalf("%s：llm_calls.model = %q，期望运行时模型 %q", label, got, llm.Model)
		}
		if got := stringValue(t, row, "status"); got != "success" {
			t.Fatalf("%s：llm_calls 存在非 success 行（%q）——真实服务失败必须如实判红", label, got)
		}
		tokens += intValue(t, row, "prompt_tokens") + intValue(t, row, "completion_tokens")
	}
	total := realModelCalls.Add(int64(len(rows)))
	realModelTokens.Add(tokens)
	if total > realModelCallLimit {
		t.Fatalf("%s：真实模型累计调用 %d 次，超过授权上限 %d 次", label, total, realModelCallLimit)
	}
}

// testRealProviderCall：真实 provider 调用（US-2 §6.1；§4.5 的 16c）。判据：进程退出 0、
// 回答含合成校验码、llm_calls 一行 success 且 provider / model 为运行时读数。
// testRealProviderCall exercises one real provider round trip.
func testRealProviderCall(t *testing.T) {
	const marker = "REALCK-7c1e"
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	h.writeConfig(t, realConfig(llm, ""))
	h.writeAgent(t, "demo", realAgentFrontmatter("demo", llm.Model, nil, nil), "真实模型回填")

	res := h.run(t, realEnv(llm), "chat", "--agent", "demo", "--message",
		"请原样回复这个校验码，不要添加任何其他内容："+marker)
	requireRealExit(t, h, res, 0, "真实 provider 调用")
	requireRealContains(t, h, res.Stdout, marker, "真实回答必须包含校验码")

	db := openDB(t, h.dbPath())
	accountRealModelCalls(t, llm, db, "provider_call")
}

// testRealMultiturn：真实模型两轮对话（US-4 Demo① 的模型半）。判据：第二轮回答复述
// 第一轮给出的数字；两次运行共用同一派生会话。
// testRealMultiturn proves session continuity across two real-model turns.
func testRealMultiturn(t *testing.T) {
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	h.writeConfig(t, realConfig(llm, ""))
	h.writeAgent(t, "demo", realAgentFrontmatter("demo", llm.Model, nil, nil), "真实多轮演示")

	res := h.runInput(t, realEnv(llm),
		"请记住数字 4312，只回复两个字：已记\n我刚才让你记住的数字是多少？只回复阿拉伯数字\n",
		"chat", "--agent", "demo")
	requireRealExit(t, h, res, 0, "真实多轮对话")
	requireRealContains(t, h, res.Stdout, "4312", "第二轮必须复述第一轮的数字")

	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 2 {
		t.Fatalf("runs 应有 2 行，实际 %d 行", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(DISTINCT session_id) FROM runs`); n != 1 {
		t.Fatalf("两轮应共用同一会话，实际不同会话数 %d", n)
	}
	accountRealModelCalls(t, llm, db, "multiturn")
}

// testRealChatTool：真实模型驱动 read_file（US-4 Demo② 的模型半）。判据：工具审计成功，
// 回答含文件里的合成标记。
// testRealChatTool drives read_file through a real model.
func testRealChatTool(t *testing.T) {
	const marker = "真实工具-notes-4a9f"
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	h.writeConfig(t, realConfig(llm, ""))
	h.writeAgent(t, "demo", realAgentFrontmatter("demo", llm.Model, []string{"read_file"}, nil),
		"真实工具演示")
	h.writeAgentFile(t, "demo", "notes.txt", "回填笔记："+marker+"\n")

	res := h.run(t, realEnv(llm), "chat", "--agent", "demo", "--message",
		"请调用 read_file 工具读取 notes.txt，并把文件里以「真实工具-」开头的标记原样告诉我。")
	requireRealExit(t, h, res, 0, "真实工具调用")
	requireRealContains(t, h, res.Stdout, marker, "回答必须带上工具读到的标记")

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "read_file", "success", "", 1)
	accountRealModelCalls(t, llm, db, "chat_tool")
}

// testRealWeatherTool：真实模型调 http_get 访问回环天气服务（US-5 Demo 一的模型半；
// 真实天气 API 未授权，服务仍是回环夹具）。判据：命中计数 > 0、审计成功、回答含 city。
// testRealWeatherTool sends the real model to a loopback weather service.
func testRealWeatherTool(t *testing.T) {
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	service := newRealWeatherService(t)
	h.writeConfig(t, realConfig(llm, realHTTPBlock("", []string{"localhost"})))
	h.writeAgent(t, "demo", realAgentFrontmatter("demo", llm.Model, []string{"http_get"}, nil),
		"真实天气演示")

	res := h.run(t, realEnv(llm), "chat", "--agent", "demo", "--message",
		"请调用 http_get 工具访问 http://localhost/weather ，然后告诉我返回 JSON 里 city 字段的值。")
	requireRealExit(t, h, res, 0, "真实天气调用")
	requireRealContains(t, h, res.Stdout, realWeatherCity, "回答必须带上天气服务的 city 字段")
	if hits := service.hits.Load(); hits == 0 {
		t.Fatal("回环天气服务未收到任何请求：模型没有真正调用 http_get")
	}

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "http_get", "success", "", 1)
	accountRealModelCalls(t, llm, db, "weather_tool")
}

// testRealMemorySave：真实模型调 save_memory 写 CORE，重启后新进程读注入（US-6 演示 2
// 的模型半）。判据：MEMORY.md 落盘标记、审计成功、新进程复述该标记。
// testRealMemorySave persists a core memory through a real model call and recalls it.
func testRealMemorySave(t *testing.T) {
	const marker = "真实模型记忆-7c1e"
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	h.writeConfig(t, realConfig(llm, ""))
	h.writeAgent(t, "demo", realAgentFrontmatter("demo", llm.Model, []string{"save_memory"}, nil),
		"真实记忆演示")

	first := h.run(t, realEnv(llm), "chat", "--agent", "demo", "--message",
		"请调用 save_memory 工具：content 设为「"+marker+"」，scope 设为 CORE；然后回复「已保存」。")
	requireRealExit(t, h, first, 0, "真实记忆写入")

	text := readText(t, memoryPath(h, "demo"))
	requireRealContains(t, h, text, marker, "核心记忆必须落盘")

	second := h.run(t, realEnv(llm), "chat", "--agent", "demo", "--message",
		"请把我核心记忆里那条内容原样复述一遍。")
	requireRealExit(t, h, second, 0, "真实记忆读取")
	requireRealContains(t, h, second.Stdout, "7c1e", "新进程必须读到注入的核心记忆")

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "save_memory", "success", "", 1)
	accountRealModelCalls(t, llm, db, "memory_save")
}

// testRealMCPTool：真实模型调 MCP helper 的回显工具（US-7 演示 3 / S1 的模型半）。
// 判据：连接摘要 `mcp_connected=1`、审计成功、回答含回显标记。
// testRealMCPTool calls the MCP helper's echo tool through a real model.
func testRealMCPTool(t *testing.T) {
	const marker = "真实-MCP-回显-913a"
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	h.writeMCPServers(t, mcpServersYAML(mcpHelperServer))
	h.writeConfig(t, realConfig(llm, ""))
	h.writeAgent(t, "demo",
		realAgentFrontmatter("demo", llm.Model, []string{mcpEchoTool}, []string{mcpHelperServer}),
		"真实 MCP 演示")

	env := append(realEnv(llm), mcpHelperEnvRef+"=1")
	res := h.run(t, env, "chat", "--agent", "demo", "--message",
		"请调用 echo 工具，text 参数设为「"+marker+"」，然后把工具返回的内容告诉我。")
	requireRealExit(t, h, res, 0, "真实 MCP 调用")
	requireRealContains(t, h, res.Stderr, "mcp_connected=1", "启动摘要按实际连接数")
	requireRealContains(t, h, res.Stdout, "913a", "回答必须带上 MCP 回显标记")

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT tool_name, status FROM tool_invocations ORDER BY id`)
	if len(rows) != 1 {
		t.Fatalf("tool_invocations 应有 1 行，实际 %d 行：%v", len(rows), rows)
	}
	if got := stringValue(t, rows[0], "tool_name"); got != mcpEchoTool {
		t.Fatalf("审计 tool_name = %q，期望 %q", got, mcpEchoTool)
	}
	if got := stringValue(t, rows[0], "status"); got != "success" {
		t.Fatalf("审计 status = %q，期望 success", got)
	}
	accountRealModelCalls(t, llm, db, "mcp_tool")
}

// testRealSchedulerFire：真实模型由 cron 到点触发一次完整运行（US-10 §2.5.2 1c）。
// 判据：进程日志出现「调度触发」、任务 completed、Run 账本 trigger=cron、助手消息含校验码。
// testRealSchedulerFire fires a scheduled run backed by a real model.
func testRealSchedulerFire(t *testing.T) {
	const marker = "REALCRON-5b2e"
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	cron := cronAt(time.Now().UTC().Add(scheduleLead))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, realConfig(llm, realHTTPBlock(addr, nil)))
	h.writeAgent(t, "demo",
		realAgentFrontmatter("demo", llm.Model, nil, nil)+
			scheduleBlock(cron, "", "到点任务：请只回复 "+marker), "真实调度演示")

	p := startGrouped(t, h, realEnv(llm), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)
	p.waitOutput(t, "调度触发", 120*time.Second)
	waitScheduleStatus(t, addr, "demo-1", "completed", 60*time.Second)
	p.stop(t)

	db := openDB(t, h.dbPath())
	runs := queryRows(t, db, `SELECT trigger, status FROM runs`)
	if len(runs) != 1 {
		t.Fatalf("runs 应有 1 行，实际 %d 行：%v", len(runs), runs)
	}
	if got := stringValue(t, runs[0], "trigger"); got != "cron" {
		t.Fatalf("runs.trigger = %q，期望 cron", got)
	}
	if got := stringValue(t, runs[0], "status"); got != "completed" {
		t.Fatalf("runs.status = %q，期望 completed", got)
	}
	assistants := queryRows(t, db, `SELECT content FROM messages WHERE role = 'assistant'`)
	if len(assistants) != 1 {
		t.Fatalf("助手消息应有 1 条，实际 %d 条：%v", len(assistants), assistants)
	}
	requireRealContains(t, h, stringValue(t, assistants[0], "content"), marker, "助手消息必须复述校验码")
	accountRealModelCalls(t, llm, db, "scheduler_fire")
}

// testRealSchedulerManageSelf：真实模型经 manage_schedule 给自己加任务（US-10 §6.2 第 4 项）。
// 判据：AGENT.md 写回带引号的任务块、HTTP 管理面任务列表随之出现该任务、库内
// scheduled_tasks 落行、审计成功。必须走 serve：按 US-10 注 13 / 约束 21，chat / tool
// 下 ReconcileAgent 是 no-op（只落盘、不落库），人工项的「端点列表随之变化」只有
// serve / gateway 才可观察——chat 模式无法满足该判据。
// testRealSchedulerManageSelf lets a real model add its own schedule over serve.
func testRealSchedulerManageSelf(t *testing.T) {
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	cron := farFutureCron()
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, realConfig(llm, realHTTPBlock(addr, nil)))
	h.writeAgent(t, "demo", realAgentFrontmatter("demo", llm.Model, []string{"manage_schedule"}, nil),
		"真实自管演示")

	p := startGrouped(t, h, realEnv(llm), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)

	resp := webInvoke(t, addr, "demo",
		"请调用 manage_schedule 工具：action 设为 set，id 用 self-added，cron 用「"+cron+
			"」，message 用「自管任务-真实」；只调用一次，然后回复「已设置」。")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "真实自管任务")
	var run webRun
	decodeWebData(t, resp, &run, "真实自管任务")
	if run.Status != "completed" {
		t.Fatalf("RunView.status = %q，期望 completed（信封 %s）", run.Status, h.redact(resp.Raw))
	}
	requireScheduleIDs(t, addr, "self-added", "真实自管后管理面任务列表")
	p.stop(t)

	requireRealContains(t, h, string(agentFile(t, h, "demo")), `- id: "self-added"`,
		"AGENT.md 必须写回任务 id")

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT task_id, agent_name FROM scheduled_tasks`)
	if len(rows) != 1 {
		t.Fatalf("scheduled_tasks 应有 1 行，实际 %d 行：%v", len(rows), rows)
	}
	if got := stringValue(t, rows[0], "task_id"); got != "self-added" {
		t.Fatalf("scheduled_tasks.task_id = %q，期望 self-added", got)
	}
	if got := stringValue(t, rows[0], "agent_name"); got != "demo" {
		t.Fatalf("scheduled_tasks.agent_name = %q，期望 demo", got)
	}
	requireToolAudit(t, db, "manage_schedule", "success", "", 1)
	accountRealModelCalls(t, llm, db, "scheduler_manage_self")
}

// testRealSchedulerManageOtherDenied：真实模型尝试接管别的 Agent 的任务（US-10 §2.5.2 5c）。
// 判据：拒绝文案点名归属回给模型、审计 failed / invalid_input、两侧 AGENT.md 字节不变。
// testRealSchedulerManageOtherDenied proves the ownership gate under a real model.
func testRealSchedulerManageOtherDenied(t *testing.T) {
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	cron := farFutureCron()
	h.writeConfig(t, realConfig(llm, ""))
	h.writeAgent(t, "demo-a",
		realAgentFrontmatter("demo-a", llm.Model, nil, nil)+
			"schedules:\n  - id: shared\n    cron: \""+cron+"\"\n    message: 共享任务\n",
		"归属演示")
	h.writeAgent(t, "demo-b", realAgentFrontmatter("demo-b", llm.Model, []string{"manage_schedule"}, nil),
		"归属演示")
	beforeA := agentFile(t, h, "demo-a")
	beforeB := agentFile(t, h, "demo-b")

	res := h.run(t, realEnv(llm), "chat", "--agent", "demo-b", "--message",
		"请调用 manage_schedule 工具：action 设为 set，id 用 shared，cron 用「"+cron+
			"」，message 用「接管尝试」；然后告诉我结果。")
	requireRealExit(t, h, res, 0, "跨 Agent 改写")

	if got, want := string(agentFile(t, h, "demo-a")), string(beforeA); got != want {
		t.Fatalf("demo-a 的 AGENT.md 被改动：\n--- 之前 ---\n%s\n--- 之后 ---\n%s", want, got)
	}
	if got, want := string(agentFile(t, h, "demo-b")), string(beforeB); got != want {
		t.Fatalf("demo-b 的 AGENT.md 被改动：\n--- 之前 ---\n%s\n--- 之后 ---\n%s", want, got)
	}

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "manage_schedule", "failed", "invalid_input", 1)
	audit := queryRows(t, db, `SELECT error_message FROM tool_invocations WHERE tool_name = 'manage_schedule'`)
	requireContains(t, stringValue(t, audit[0], "error_message"), "属于 Agent demo-a", "审计点名归属")
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM scheduled_tasks WHERE agent_name = 'demo-b'`); n != 0 {
		t.Fatalf("demo-b 不应拥有任何任务，实际 %d 条", n)
	}
	accountRealModelCalls(t, llm, db, "scheduler_manage_other_denied")
}

// testRealRestartSession：真实模型跨进程重启的会话历史（US-4 Demo⑤ 的模型半）。
// 判据：第二个进程复述第一个进程给出的数字；两次运行同一会话。
// testRealRestartSession proves session history survives a process restart.
func testRealRestartSession(t *testing.T) {
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	h.writeConfig(t, realConfig(llm, ""))
	h.writeAgent(t, "demo", realAgentFrontmatter("demo", llm.Model, nil, nil), "真实重启演示")

	first := h.run(t, realEnv(llm), "chat", "--agent", "demo", "--message",
		"请记住数字 5821，只回复两个字：已记")
	requireRealExit(t, h, first, 0, "重启前对话")

	second := h.run(t, realEnv(llm), "chat", "--agent", "demo", "--message",
		"刚才让你记住的数字是多少？只回复阿拉伯数字")
	requireRealExit(t, h, second, 0, "重启后对话")
	requireRealContains(t, h, second.Stdout, "5821", "重启后必须复述此前的数字")

	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 2 {
		t.Fatalf("runs 应有 2 行，实际 %d 行", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(DISTINCT session_id) FROM runs`); n != 1 {
		t.Fatalf("重启前后应共用同一会话，实际不同会话数 %d", n)
	}
	accountRealModelCalls(t, llm, db, "restart_session")
}

// testRealWebInvokeTool：真实模型经 web invoke 调 http_get 访问回环天气服务（US-9 第 5 项）。
// 判据：invoke completed、最终回答含 city、命中计数 > 0、审计成功。
// testRealWebInvokeTool drives a real model through the stateless invoke endpoint.
func testRealWebInvokeTool(t *testing.T) {
	llm := requireRealLLMEnv(t)
	h := newRealHarness(t, llm)
	initWorkspace(t, h)
	service := newRealWeatherService(t)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, realConfig(llm, realHTTPBlock(addr, []string{"localhost"})))
	h.writeAgent(t, "demo", realAgentFrontmatter("demo", llm.Model, []string{"http_get"}, nil),
		"真实 web 调用演示")

	p := startGrouped(t, h, realEnv(llm), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)
	resp := webInvoke(t, addr, "demo",
		"请调用 http_get 工具访问 http://localhost/weather ，然后告诉我返回 JSON 里 city 字段的值。")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "真实 web invoke")
	var run webRun
	decodeWebData(t, resp, &run, "真实 web invoke")
	if run.Status != "completed" {
		t.Fatalf("RunView.status = %q，期望 completed（信封 %s）", run.Status, h.redact(resp.Raw))
	}
	requireRealContains(t, h, run.FinalText, realWeatherCity, "最终回答必须带上 city 字段")
	p.stop(t)

	if hits := service.hits.Load(); hits == 0 {
		t.Fatal("回环天气服务未收到任何请求：模型没有真正调用 http_get")
	}
	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "http_get", "success", "", 1)
	accountRealModelCalls(t, llm, db, "web_invoke_tool")
}

// testRealFeishuSend：真实飞书外发（US-8 §6.1 第 5 项）。固定合成文本、渠道 URL 只写
// `${VAR}`；判据：退出 0、成功文案、审计 success、完整 URL / 令牌零回显、发送累计 ≤5。
// testRealFeishuSend performs one real Feishu send with synthetic text.
func testRealFeishuSend(t *testing.T) {
	const marker = "真实飞书回填-8f21（合成数据）"
	raw, parsed := requireRealFeishuURL(t)
	token := strings.TrimPrefix(raw, parsed.Scheme+"://"+parsed.Host)
	display := parsed.Scheme + "://" + parsed.Host

	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-feishu", "notify", mustJSON(map[string]string{
			"channel": notifyChannelName, "content": marker,
		})),
		textResponse("真实外发完成"),
	)
	h.writeConfig(t, configWithProvider(fake.URL())+realHTTPBlock("", []string{parsed.Hostname()}))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"notify"}), "真实飞书演示")
	insertNotifyChannel(t, h, notifyChannelName, "feishu", "${"+realFeishuURLEnv+"}", "真实飞书渠道")

	res := h.run(t, append(fixtureEnv(), realFeishuURLEnv+"="+raw), "chat", "--agent", "demo",
		"--message", "发送巡检摘要")
	requireRealExit(t, h, res, 0, "真实飞书外发")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	result := lastToolContent(reqs[1])
	requireContains(t, result, "已推送到通知渠道："+notifyChannelName, "成功文案回给模型")
	requireAbsent(t, result, token, "模型上下文不得回显 webhook 令牌")
	requireAbsent(t, res.Stdout+res.Stderr, token, "进程输出不得回显 webhook 令牌")
	requireAbsentInWorkspace(t, h.Root, token, "webhook 令牌不得落盘到工作区")

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "notify", "success", "", 1)
	audit := queryRows(t, db, `SELECT error_message FROM tool_invocations WHERE tool_name = 'notify'`)
	if message, ok := audit[0]["error_message"].(string); ok {
		requireAbsent(t, message, token, "审计不得回显 webhook 令牌")
	}
	if sends := realFeishuSends.Add(1); sends > realFeishuSendLimit {
		t.Fatalf("真实飞书累计发送 %d 次，超过授权上限 %d 次（本场景只应发送 1 次）", sends, realFeishuSendLimit)
	}
	t.Logf("feishu_target=%s", display)
}

// testRealFeishuRedaction：真实 webhook 在域名白名单外被拒（US-8 §6.1 第 7 项）。
// 判据：拒绝先于外呼（零发送）、模型与审计只出 `scheme://host`、令牌零回显。
// testRealFeishuRedaction proves the policy refuses before any send and leaks nothing.
func testRealFeishuRedaction(t *testing.T) {
	raw, parsed := requireRealFeishuURL(t)
	token := strings.TrimPrefix(raw, parsed.Scheme+"://"+parsed.Host)
	display := parsed.Scheme + "://" + parsed.Host
	// 拒绝场景只断言本子测试未新增发送：realFeishuSends 是包级累计计数，
	// send 子测试先运行会把累计值推到 1，累计值不能判断本场景是否外呼。
	// The refusal scenario asserts a zero delta, not a zero cumulative count.
	before := realFeishuSends.Load()

	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-feishu-deny", "notify", mustJSON(map[string]string{
			"channel": notifyChannelName, "content": "真实脱敏读数（合成数据）",
		})),
		textResponse("拒绝完成"),
	)
	h.writeConfig(t, configWithProvider(fake.URL())+realHTTPBlock("", []string{"other.example"}))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"notify"}), "真实飞书脱敏演示")
	insertNotifyChannel(t, h, notifyChannelName, "feishu", "${"+realFeishuURLEnv+"}", "真实飞书渠道")

	res := h.run(t, append(fixtureEnv(), realFeishuURLEnv+"="+raw), "chat", "--agent", "demo",
		"--message", "发送巡检摘要")
	requireRealExit(t, h, res, 0, "真实飞书脱敏")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	result := lastToolContent(reqs[1])
	requireContains(t, result, "域名不在白名单："+display, "拒绝文案只出 scheme://host")
	requireAbsent(t, result, token, "模型上下文不得回显 webhook 令牌")
	requireAbsent(t, res.Stdout+res.Stderr, token, "进程输出不得回显 webhook 令牌")
	requireAbsentInWorkspace(t, h.Root, token, "webhook 令牌不得落盘到工作区")

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT status, error_kind, error_message FROM tool_invocations ORDER BY id`)
	if len(rows) != 1 {
		t.Fatalf("tool_invocations 应有 1 行，实际 %d 行：%v", len(rows), rows)
	}
	if got := stringValue(t, rows[0], "status"); got != "failed" {
		t.Fatalf("审计 status = %q，期望 failed", got)
	}
	if got := stringValue(t, rows[0], "error_kind"); got != "sandbox_denied" {
		t.Fatalf("审计 error_kind = %q，期望 sandbox_denied", got)
	}
	message := stringValue(t, rows[0], "error_message")
	requireContains(t, message, "域名不在白名单："+display, "审计只出 scheme://host")
	requireAbsent(t, message, token, "审计不得回显 webhook 令牌")
	if after := realFeishuSends.Load(); after != before {
		t.Fatalf("拒绝场景不得产生任何真实发送，本场景新增 %d 次（累计 %d）", after-before, after)
	}
}

// realWeatherService 是回环天气夹具：**固定监听 127.0.0.1:80**——出网策略只放 scheme 的
// 默认端口（口径 15），其他端口会被拒绝，测不到模型真实工具调用。命中次数供判"确实调了"。
// realWeatherService listens on 127.0.0.1:80 because the HTTP policy only allows the
// scheme's default port; the hit counter proves the model really called the tool.
type realWeatherService struct {
	server *http.Server
	hits   atomic.Int64
}

// newRealWeatherService 起一个只服务 /weather 的回环服务；端口 80 被占即 FAIL（不 SKIP：
// 真实项缺环境不得冒充通过，见模块卡 §4.5）。
// newRealWeatherService serves /weather on port 80; an unavailable port fails.
func newRealWeatherService(t *testing.T) *realWeatherService {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("回环天气服务无法监听 127.0.0.1:80（出网策略只放默认端口）：%v", err)
	}
	service := &realWeatherService{}
	mux := http.NewServeMux()
	mux.HandleFunc("/weather", func(w http.ResponseWriter, _ *http.Request) {
		service.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"city":"`+realWeatherCity+`","temperature":"21C"}`)
	})
	service.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		_ = service.server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = service.server.Close()
	})
	return service
}
