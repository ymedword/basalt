//go:build e2e

package e2e

// 本文件回填 US-3（技能 L1 渐进披露）/ US-4（ReAct 循环）/ US-5（凭证与敏感面）的人工项
// （模块卡 §2.6.3~§2.6.5）。八个子测试名与 `backfillCatalog` 的 `Parent/sub` 逐字对应；
// 场景一律驱动真实二进制 + 回环假模型，断言只读四处：进程输出、退出码、发往模型的请求体
// 与工作区 SQLite 库。
// This file backfills the manual items of US-3/US-4/US-5. The eight subtests match the
// catalog names verbatim. Every scenario drives the real binary against a loopback fake
// model and reads only process output, exit codes, outbound request bodies and the DB.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ymedword/basalt/internal/testutil"
)

// TestBackfill_AgentLoop 覆盖 Agent 循环的八个人工项。
// TestBackfill_AgentLoop covers the eight manual items of the agent loop.
func TestBackfill_AgentLoop(t *testing.T) {
	t.Run("multiturn_identity", testAgentLoopMultiturnIdentity)
	t.Run("read_file", testAgentLoopReadFile)
	t.Run("sandbox_denied_audit", testAgentLoopSandboxDeniedAudit)
	t.Run("reasoning_roundtrip", testAgentLoopReasoningRoundtrip)
	t.Run("injection_denied", testAgentLoopInjectionDenied)
	t.Run("reasoning_required", testAgentLoopReasoningRequired)
	t.Run("redaction", testAgentLoopRedaction)
	t.Run("skill_l1", testAgentLoopSkillL1)
}

// testAgentLoopMultiturnIdentity：两轮对话 + 身份生效（US-4 §6.1 第 2 项 Demo ①）。
// 判据：段一含 identity.prompt 与自我称谓；第二轮请求带上第一轮的完整历史；
// 两次运行共用同一会话标识。
func testAgentLoopMultiturnIdentity(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t, textResponse("第一轮回答"), textResponse("第二轮回答"))
	h.writeConfig(t, configWithProvider(fake.URL()))
	front := "identity:\n  agent_name: 演示小助手\n  prompt: 你是一个用于回填验证的助手。\n" +
		agentFrontmatter("demo", nil)
	h.writeAgent(t, "demo", front, "AGENT.md 正文标记-多轮身份。")

	res := h.runInput(t, fixtureEnv(), "你好\n再聊聊\n", "chat", "--agent", "demo")
	requireExit(t, res, 0, "多轮对话")
	requireContains(t, res.Stdout, "第一轮回答", "多轮对话")
	requireContains(t, res.Stdout, "第二轮回答", "多轮对话")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	for i, req := range reqs {
		system := systemPrompt(t, req)
		for _, want := range []string{
			"你是一个用于回填验证的助手。",      // identity.prompt 拼在段一开头
			"你的名字是「演示小助手」。",       // 自我称谓
			"AGENT.md 正文标记-多轮身份。", // AGENT.md 正文
		} {
			requireContains(t, system, want, "第 "+string(rune('1'+i))+" 次请求的系统提示词")
		}
	}
	// 第二轮请求必须带上第一轮的完整历史（user → assistant → 当前 user）。
	msgs := requestMessages(t, reqs[1])
	if len(msgs) != 4 {
		t.Fatalf("第二轮请求消息数 = %d，期望 4（system + 两轮问答）：%s", len(msgs), requestText(t, reqs[1]))
	}
	if got := messageString(msgs[1], "content"); got != "你好" {
		t.Fatalf("第二轮历史第一条 = %q，期望首轮用户输入", got)
	}
	if got := messageString(msgs[2], "content"); got != "第一轮回答" {
		t.Fatalf("第二轮历史第二条 = %q，期望首轮助手回复", got)
	}
	if got := messageString(msgs[3], "content"); got != "再聊聊" {
		t.Fatalf("第二轮当前消息 = %q，期望第二轮用户输入", got)
	}

	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 2 {
		t.Fatalf("runs 应有 2 行，实际 %d 行", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(DISTINCT session_id) FROM runs`); n != 1 {
		t.Fatalf("两次运行应共用同一会话标识，实际不同会话数 %d", n)
	}
}

// testAgentLoopReadFile：模型调 read_file 读工作区文件，内容回填进下一轮请求
// （US-4 §6.1 第 2 项 Demo ②）。审计侧要求一行 success。
func testAgentLoopReadFile(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-read", "read_file", `{"path":"notes.txt"}`),
		textResponse("笔记内容：回填专用内容-7f3a"),
	)
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"read_file"}), "读取工作区文件")
	h.writeAgentFile(t, "demo", "notes.txt", "回填专用内容-7f3a\n")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "读一下 notes.txt")
	requireExit(t, res, 0, "read_file 闭环")
	requireContains(t, res.Stdout, "⚙ 调用工具 read_file…", "read_file 闭环")
	requireContains(t, res.Stdout, "笔记内容：回填专用内容-7f3a", "read_file 闭环")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[1], "回填专用内容-7f3a", "文件内容必须回填给模型")

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "read_file", "success", "", 1)
}

// testAgentLoopSandboxDeniedAudit：越界路径 → 四段式拒绝可见 + 审计 failed +
// error_kind=sandbox_denied，运行继续（US-4 §6.1 第 2 项 Demo ③）。
func testAgentLoopSandboxDeniedAudit(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-deny", "read_file", `{"path":"../../etc/passwd"}`),
		textResponse("拒绝已收到，继续回答"),
	)
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"read_file"}), "越界读取演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "读一下 ../../etc/passwd")
	requireExit(t, res, 0, "越界读取后运行继续")
	requireContains(t, res.Stdout, "⚙ 调用工具 read_file…", "越界读取")
	requireContains(t, res.Stdout, "拒绝已收到，继续回答", "越界读取")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	for _, want := range []string{
		"读取目标不在可读根之内",                    // 段 1 规则
		"这是安全策略，请勿反复重试；",                 // 段 3 固定句
		"可读根固定为当前 Agent 自己的目录与 skills 库", // 段 4 放行指引
	} {
		requireToolResultContains(t, reqs[1], want, "四段式拒绝必须回填给模型")
	}

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "read_file", "failed", "sandbox_denied", 1)
	rows := queryRows(t, db, `SELECT error_message FROM tool_invocations`)
	if msg, _ := rows[0]["error_message"].(string); !strings.Contains(msg, "读取目标不在可读根之内") {
		t.Fatalf("审计 error_message = %q，期望逐字保留四段式拒绝原文", msg)
	}
}

// testAgentLoopReasoningRoundtrip：带 reasoning_content 的工具调用两跳保真
// （US-4 §6.1 第 2 项 Demo ④）。判据：第二轮请求里的助手消息仍带推理字段，
// tool_call_id 原样回传。
func testAgentLoopReasoningRoundtrip(t *testing.T) {
	const reasoning = "先读笔记再回答"
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		reasoningToolCallResponse("call-think", "read_file", `{"path":"notes.txt"}`, reasoning),
		textResponse("已经读完笔记"),
	)
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"read_file"}), "推理字段回传演示")
	h.writeAgentFile(t, "demo", "notes.txt", "笔记正文\n")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "读笔记")
	requireExit(t, res, 0, "推理字段两跳")
	requireContains(t, res.Stdout, "⚙ 调用工具 read_file…", "推理字段两跳")
	requireContains(t, res.Stdout, "已经读完笔记", "推理字段两跳")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	calls := assistantToolCalls(t, reqs[1])
	if len(calls) != 1 {
		t.Fatalf("第二轮请求应带 1 条工具调用助手消息，实际 %d 条：%s", len(calls), requestText(t, reqs[1]))
	}
	if got := messageString(calls[0], "reasoning_content"); got != reasoning {
		t.Fatalf("出站 reasoning_content = %q，期望 %q（清空会被供应商 400）", got, reasoning)
	}
	if !containsString(toolCallIDs(t, reqs[1]), "call-think") {
		t.Fatalf("出站 tool_call_id 未原样回传：%v", toolCallIDs(t, reqs[1]))
	}

	db := openDB(t, h.dbPath())
	requireLLMCallStatuses(t, db, []string{"success", "success"})
}

// testAgentLoopInjectionDenied：注入用例（跨 Agent 读取）被四段式拒绝、审计 failed，
// 且受害文件内容从未进入模型请求（US-4 §6.1 第 2 项 Demo ⑦）。
func testAgentLoopInjectionDenied(t *testing.T) {
	const secret = "受害者机密-不得跨 Agent 读取"
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-cross", "read_file", `{"path":"../victim/secret.txt"}`),
		textResponse("越权读取被拒绝"),
	)
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "intruder", agentFrontmatter("intruder", []string{"read_file"}), "试图读取别的 Agent")
	h.writeAgent(t, "victim", agentFrontmatter("victim", nil), "被读取方")
	h.writeAgentFile(t, "victim", "secret.txt", secret+"\n")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "intruder", "--message", "读取 ../victim/secret.txt")
	requireExit(t, res, 0, "注入用例后运行继续")
	requireContains(t, res.Stdout, "⚙ 调用工具 read_file…", "注入用例")
	requireContains(t, res.Stdout, "越权读取被拒绝", "注入用例")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	for _, want := range []string{
		"跨 Agent 读取未授权",            // 段 1 规则
		"这是安全策略，请勿反复重试；",           // 段 3 固定句
		"sandbox.cross_agent_read", // 段 4 放行指引点名配置键
	} {
		requireToolResultContains(t, reqs[1], want, "跨 Agent 拒绝必须回填给模型")
	}
	requireAbsent(t, requestText(t, reqs[1]), secret, "被拒读取的内容不得进入模型请求")

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "read_file", "failed", "sandbox_denied", 1)
}

// testAgentLoopReasoningRequired：假服务在助手工具调用消息缺少 reasoning_content
// 时返回 400（Compatibility §5.3 判据 3 的协议面）。判据：正常运行下该字段在
// 第二轮请求里非空，两次模型调用均落 success。
func testAgentLoopReasoningRequired(t *testing.T) {
	const reasoning = "必须回传的推理字段"
	h := newHarness(t)
	initWorkspace(t, h)

	var calls atomic.Int64
	var sawReasoning atomic.Bool
	server := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Errorf("假服务：期望 POST /chat/completions，实际 %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("假服务：读请求体失败: %v", err)
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("假服务：请求体不是 JSON 对象: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, reasoningToolCallResponse(
				"call-gate", "read_file", `{"path":"notes.txt"}`, reasoning).Body)
			return
		}
		if !assistantCallsCarryReasoning(req) {
			// 供应商对缺失字段的拒绝形态：非 2xx + error 信封（客户端归 external_error）。
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"assistant tool call without reasoning_content"}}`)
			return
		}
		sawReasoning.Store(true)
		_, _ = io.WriteString(w, textResponse("推理字段回传成功").Body)
	}))
	h.writeConfig(t, configWithProvider(server.URL))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"read_file"}), "推理字段必填演示")
	h.writeAgentFile(t, "demo", "notes.txt", "笔记正文\n")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "读笔记")
	requireExit(t, res, 0, "推理字段必填")
	requireContains(t, res.Stdout, "推理字段回传成功", "推理字段必填")
	if got := calls.Load(); got != 2 {
		t.Fatalf("假服务应收到 2 次请求，实际 %d 次", got)
	}
	if !sawReasoning.Load() {
		t.Fatal("第二轮请求未带 reasoning_content——供应商侧应返回 400 而这里放行了")
	}

	db := openDB(t, h.dbPath())
	requireLLMCallStatuses(t, db, []string{"success", "success"})
}

// testAgentLoopRedaction：凭证只在运行时使用、零落盘（US-4 §6.1 第 10 项 +
// US-5 §6.1 第 9 项）。判据：出站 Authorization 头带着哨兵值（证明确实解析使用），
// 而进程输出、工作区全部文件均不含该值，配置里的 ${ENV_VAR} 占位原样保留。
func testAgentLoopRedaction(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)

	var rec authRecorder
	server := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.Header.Get("Authorization"))
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, textResponse("凭证已在内存中使用").Body)
	}))
	h.writeConfig(t, configWithProvider(server.URL))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil), "凭证脱敏演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "你好")
	requireExit(t, res, 0, "凭证使用")
	requireContains(t, res.Stdout, "凭证已在内存中使用", "凭证使用")

	auths := rec.values()
	if len(auths) != 1 {
		t.Fatalf("假服务应收到 1 次请求，实际 %d 次", len(auths))
	}
	if want := "Bearer " + credentialSentinel; auths[0] != want {
		t.Fatalf("出站 Authorization = %q，期望运行期解析后的凭证（%q）", auths[0], want)
	}
	requireAbsent(t, res.Stdout+res.Stderr, credentialSentinel, "进程输出不得回显凭证")
	requireAbsentInWorkspace(t, h.Root, credentialSentinel, "工作区不得落明文凭证")
	config, err := os.ReadFile(filepath.Join(h.Root, "basalt.yaml"))
	if err != nil {
		t.Fatalf("读配置文件失败: %v", err)
	}
	requireContains(t, string(config), "${"+fixtureKeyEnv+"}", "配置必须保留占位符")
}

// testAgentLoopSkillL1：技能声明即绑定，段一含 L1 三要素卡片、不含 SKILL.md 正文
// （US-3 §6 Demo ③；渐进披露）。判据：卡片逐字为 name + description + 技能库路径。
func testAgentLoopSkillL1(t *testing.T) {
	const bodySentinel = "技能正文哨兵-5c1e：此正文不得进入段一。"
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t, textResponse("按技能格式回答"))
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeSkill(t, "digest-format",
		"name: digest-format\ndescription: 把内容整理成三段摘要\n", bodySentinel)
	front := agentFrontmatter("demo", nil) + "skills:\n  - digest-format\n"
	h.writeAgent(t, "demo", front, "技能绑定演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "整理一下")
	requireExit(t, res, 0, "技能 L1")
	requireContains(t, res.Stdout, "按技能格式回答", "技能 L1")

	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("假模型应收到 1 次请求，实际 %d 次", len(reqs))
	}
	system := systemPrompt(t, reqs[0])
	requireContains(t, system, "## 技能", "段一技能小节")
	requireContains(t, system,
		"- digest-format：把内容整理成三段摘要（.basalt/skills/digest-format/SKILL.md）",
		"L1 卡片三要素逐字")
	requireAbsent(t, system, bodySentinel, "SKILL.md 正文不得预载进段一")
}

// writeSkill 落一份技能定义：frontmatter 原样放进两条围栏之间，正文追加在后。
// writeSkill writes one SKILL.md with the frontmatter verbatim between the fences.
func (h *harness) writeSkill(t *testing.T, name, frontmatter, body string) {
	t.Helper()
	dir := filepath.Join(h.Root, ".basalt", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建技能目录 %s 失败: %v", dir, err)
	}
	content := "---\n" + strings.TrimRight(frontmatter, "\n") + "\n---\n\n" + strings.TrimRight(body, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("写 SKILL.md %s 失败: %v", name, err)
	}
}

// writeAgentFile 在某个 Agent 的自有目录里落一个附加文件（read_file 的读取目标）。
// writeAgentFile writes an extra file inside one agent's own directory.
func (h *harness) writeAgentFile(t *testing.T, agent, rel, content string) {
	t.Helper()
	path := filepath.Join(h.Root, ".basalt", "agents", agent, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建 %s 的父目录失败: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 %s 失败: %v", rel, err)
	}
}

// requestMessages 取请求体里的 messages 数组（形状由本仓 provider 客户端产出）。
// requestMessages returns the decoded messages array of one recorded request.
func requestMessages(t *testing.T, req map[string]any) []map[string]any {
	t.Helper()
	raw, ok := req["messages"].([]any)
	if !ok {
		t.Fatalf("请求体缺少 messages 数组：%s", requestText(t, req))
	}
	out := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		msg, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("messages[%d] 不是对象：%v", i, item)
		}
		out = append(out, msg)
	}
	return out
}

// messageString 取消息的字符串字段（缺失或非字符串 → 空串）。
// messageString reads a string field from one message.
func messageString(msg map[string]any, key string) string {
	value, _ := msg[key].(string)
	return value
}

// systemPrompt 返回请求里的系统提示词（第一条 system 消息）。
// systemPrompt returns the system message of one recorded request.
func systemPrompt(t *testing.T, req map[string]any) string {
	t.Helper()
	for _, msg := range requestMessages(t, req) {
		if messageString(msg, "role") == "system" {
			return messageString(msg, "content")
		}
	}
	t.Fatalf("请求体里没有 system 消息：%s", requestText(t, req))
	return ""
}

// toolResults 返回请求里全部工具结果消息的正文（按出站顺序）。
// toolResults returns the contents of every tool message, in order.
func toolResults(t *testing.T, req map[string]any) []string {
	t.Helper()
	var out []string
	for _, msg := range requestMessages(t, req) {
		if messageString(msg, "role") == "tool" {
			out = append(out, messageString(msg, "content"))
		}
	}
	return out
}

// requireToolResultContains 断言至少一条工具结果正文含指定片段。
// requireToolResultContains asserts some tool result carries the fragment.
func requireToolResultContains(t *testing.T, req map[string]any, want, label string) {
	t.Helper()
	for _, content := range toolResults(t, req) {
		if strings.Contains(content, want) {
			return
		}
	}
	t.Fatalf("%s：工具结果不含 %q\n%s", label, want, requestText(t, req))
}

// assistantToolCalls 返回请求里带 tool_calls 的助手消息。
// assistantToolCalls returns the assistant messages that carry tool calls.
func assistantToolCalls(t *testing.T, req map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, msg := range requestMessages(t, req) {
		if messageString(msg, "role") != "assistant" {
			continue
		}
		if _, ok := msg["tool_calls"]; ok {
			out = append(out, msg)
		}
	}
	return out
}

// toolCallIDs 返回请求里工具结果消息携带的 tool_call_id。
// toolCallIDs returns the tool_call_id values carried by tool messages.
func toolCallIDs(t *testing.T, req map[string]any) []string {
	t.Helper()
	var out []string
	for _, msg := range requestMessages(t, req) {
		if messageString(msg, "role") == "tool" {
			out = append(out, messageString(msg, "tool_call_id"))
		}
	}
	return out
}

// assistantCallsCarryReasoning 报告请求里是否有一条带非空 reasoning_content 的
// 工具调用助手消息（假供应商据此决定放行还是 400）。
// assistantCallsCarryReasoning reports whether a tool-call assistant message carries
// a non-empty reasoning_content field.
func assistantCallsCarryReasoning(req map[string]any) bool {
	raw, _ := req["messages"].([]any)
	for _, item := range raw {
		msg, _ := item.(map[string]any)
		if msg == nil {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		if _, has := msg["tool_calls"]; !has {
			continue
		}
		if reasoning, _ := msg["reasoning_content"].(string); strings.TrimSpace(reasoning) != "" {
			return true
		}
	}
	return false
}

// containsString 报告切片里是否含指定值。
// containsString reports membership.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// requestText 把一次记录的请求体序列化回 JSON 文本（查模型可见文本用）。
// requestText re-serializes a recorded request for text assertions.
func requestText(t *testing.T, req map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	return string(raw)
}

// requireAbsent 断言文本不含片段。
// requireAbsent asserts the text does not contain the fragment.
func requireAbsent(t *testing.T, text, needle, label string) {
	t.Helper()
	if strings.Contains(text, needle) {
		t.Fatalf("%s：文本不应包含 %q\n%s", label, needle, text)
	}
}

// requireAbsentInWorkspace 断言工作区里的任何文件都不含给定字节（凭证零落盘）。
// requireAbsentInWorkspace asserts no file under the workspace contains the bytes.
func requireAbsentInWorkspace(t *testing.T, root, needle, label string) {
	t.Helper()
	for _, path := range walkFiles(t, root) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", path, err)
		}
		if bytes.Contains(data, []byte(needle)) {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			t.Fatalf("%s：文件 %s 含明文凭证", label, rel)
		}
	}
}

// requireToolAudit 断言工具审计表的行数与最后一行的四列读数。
// requireToolAudit asserts the row count and the last row's four columns.
func requireToolAudit(t *testing.T, db *sql.DB, tool, status, errorKind string, wantRows int) {
	t.Helper()
	rows := queryRows(t, db,
		`SELECT tool_name, status, error_kind, error_message FROM tool_invocations ORDER BY id`)
	if len(rows) != wantRows {
		t.Fatalf("tool_invocations 应有 %d 行，实际 %d 行：%v", wantRows, len(rows), rows)
	}
	last := rows[len(rows)-1]
	if got, _ := last["tool_name"].(string); got != tool {
		t.Fatalf("tool_invocations.tool_name = %v，期望 %s", last["tool_name"], tool)
	}
	if got, _ := last["status"].(string); got != status {
		t.Fatalf("tool_invocations.status = %v，期望 %s", last["status"], status)
	}
	if got, _ := last["error_kind"].(string); got != errorKind {
		t.Fatalf("tool_invocations.error_kind = %v，期望 %q", last["error_kind"], errorKind)
	}
}

// requireLLMCallStatuses 断言 llm_calls 的逐行状态（按写入顺序）。
// requireLLMCallStatuses asserts the per-row llm_calls statuses in insertion order.
func requireLLMCallStatuses(t *testing.T, db *sql.DB, want []string) {
	t.Helper()
	rows := queryRows(t, db, `SELECT status FROM llm_calls ORDER BY id`)
	if len(rows) != len(want) {
		t.Fatalf("llm_calls 应有 %d 行，实际 %d 行：%v", len(want), len(rows), rows)
	}
	for i, status := range want {
		if got, _ := rows[i]["status"].(string); got != status {
			t.Fatalf("llm_calls[%d].status = %v，期望 %s", i, rows[i]["status"], status)
		}
	}
}

// authRecorder 记录回环假服务收到的 Authorization 头。
// authRecorder records the Authorization headers a loopback server received.
type authRecorder struct {
	mu   sync.Mutex
	auth []string
}

func (r *authRecorder) record(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auth = append(r.auth, value)
}

func (r *authRecorder) values() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auth...)
}
