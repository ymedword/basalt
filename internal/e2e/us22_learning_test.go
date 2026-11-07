//go:build e2e

package e2e

// 本文件是 US-22「受控自我进化」的**端到端**判据（T031）：真实二进制 + 隔离 PostgreSQL
// schema + 回环假模型。六条用例覆盖模块卡 C-15 / C-31 的主链：
//
//	[1] ExtractConfirmRecall   开启抽取 → 候选生成（候选不召回）→ 人工确认 → 新 Run 召回
//	[2] RejectWrongCandidate   生成 → 删除候选 → 后续 Run 不召回（候选永不自动生效）
//	[3] DeleteStopsRecall      生效 → 召回 → 删除 → 停止召回
//	[4] DeleteSourceStopsDerived 来源失效 → 派生条目按闭包停止召回，显式条目不受影响
//	[5] RecoveryAfterCrash     抽取在途崩溃 → 只查证不重放 → 查证后不再阻断新窗口
//	[6] DisabledNoSideEffects  出厂默认关闭 → 零抽取行、零抽取调用、零候选
//
// 为什么会话必须用**调用方 Key** 而不是管理面 token：`ConfirmCandidate` 对抽取来源做
// `callers` 行存在性复核（来源调用方已停用 → 409），管理面 token 的运行没有调用方行，
// 确认会被无条件拒绝。管理面 token 只用于确认 / 删除 / 只读查询这些管理动作。
//
// Why sessions use a caller key: ConfirmCandidate re-checks the origin caller row, which an
// admin-token run does not have. The admin token is used only for management actions.
//
// 假模型不是 `testutil.FakeOpenAIServer`：那条脚本按请求顺序消费，而本模块的对话请求与
// 抽取请求**交错**发生（抽取在 Run 终态之后同步触发）。这里用回环 HTTP 端点分流：带
// 「记忆抽取器」系统提示词的请求交给抽取处理函数，其余按 FIFO 消费对话剧本。
// The fake model splits chat and extraction requests instead of consuming one sequential
// script, because extraction interleaves with the chat completions of the next run.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/testutil"
)

// 本套件共用的 Agent、调用方与正文。候选正文与显式保存正文**必须不同**：抽取校验按
// 「同 Agent + 归档区同文」去重，同文会让候选被静默丢弃。
// The candidate body must differ from any explicitly saved body or extraction dedupes it.
const (
	us22Agent      = "memory-learner"
	us22CallerName = "us22-learning-caller"

	us22SavedContent     = "成本评估每周五进行"
	us22CandidateContent = "季度评审固定在每月第一个周四"
	us22SourceContent    = "巡检每周三执行"
	us22DerivedContent   = "巡检简报在每周三下午生成"
	us22UnrelatedContent = "周报每周一汇总"

	us22CrashTurn  = "请记住：崩溃恢复之后仍然要按窗口推进。"
	us22CrashTurn2 = "崩溃恢复之后的新消息。"
)

// us22LearningSection 渲染 `learning` 段：显式启用 + 显式 Agent 名单（空名单是启动
// 错误，不是「全部 Agent」）。
// us22LearningSection renders the explicit enable switch and agent allow-list.
func us22LearningSection(agents ...string) string {
	out := "learning:\n  auto_extract: true\n  agents:\n"
	for _, name := range agents {
		out += "    - " + name + "\n"
	}
	return out
}

// us22Bearer 用调用方 Key 覆盖夹具默认的管理面 token。
// us22Bearer overrides the fixture's default admin bearer token.
func us22Bearer(key string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + key}
}

// us22IssueCallerKey 经管理面建调用方并签发最小 scope 的 Key：
// `sessions:read/write` 建会话与发消息，`agents:invoke:<agent>` 过记忆读取闸门
// （召回工具在任何取值下都先判这个 scope）。
// us22IssueCallerKey issues a business key scoped to the session API and one agent.
func us22IssueCallerKey(t *testing.T, addr, agent string) string {
	t.Helper()
	createBody := mustJSON(map[string]any{
		"name": us22CallerName,
		"limits": map[string]any{
			"qps":                     100,
			"burst":                   100,
			"max_concurrent":          8,
			"tasks_per_period":        1000,
			"tokens_per_period":       1000000,
			"ingest_bytes_per_period": 10000000,
			"period":                  "day",
			"timezone":                "UTC",
		},
	})
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/callers", createBody, nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "建调用方")
	var caller struct {
		CallerID string `json:"caller_id"`
	}
	decodeWebData(t, resp, &caller, "建调用方")
	if caller.CallerID == "" {
		t.Fatalf("建调用方：caller_id 为空（%s）", resp.Raw)
	}

	scopes := []string{
		"sessions:read", "sessions:write", "agents:invoke:" + agent,
	}
	issueBody := mustJSON(map[string]any{
		"scopes":     scopes,
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
	resp = webRequest(t, addr, http.MethodPost, "/api/v1/callers/"+caller.CallerID+"/keys", issueBody, nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "签发调用方 Key")
	var issued struct {
		Key string `json:"key"`
	}
	decodeWebData(t, resp, &issued, "签发调用方 Key")
	if issued.Key == "" {
		t.Fatalf("签发调用方 Key：明文为空（%s）", resp.Raw)
	}
	return issued.Key
}

// us22CreateSession 用调用方 Key 建会话并返回标识。
// us22CreateSession creates one session with the caller key.
func us22CreateSession(t *testing.T, addr, key, agent string) string {
	t.Helper()
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/sessions",
		mustJSON(map[string]any{"agent": agent}), us22Bearer(key))
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "建会话")
	var session struct {
		SessionID string `json:"session_id"`
	}
	decodeWebData(t, resp, &session, "建会话")
	if session.SessionID == "" {
		t.Fatalf("建会话：session_id 为空（%s）", resp.Raw)
	}
	return session.SessionID
}

// us22PostMessage 用调用方 Key 发一条消息并等运行终态；非 completed 判红。
// 学习钩子在 Run 终态落库之后**同步**执行，返回时抽取行已是终态。
// us22PostMessage posts one message and requires a completed run; the learning hook is
// synchronous, so the extraction row is terminal when this returns.
func us22PostMessage(t *testing.T, addr, key, sessionID, message string) webRun {
	t.Helper()
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages",
		mustJSON(map[string]any{"message": message}), us22Bearer(key))
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "发消息")
	var run webRun
	decodeWebData(t, resp, &run, "发消息")
	if run.Status != "completed" {
		t.Fatalf("运行的 status = %q，期望 completed（reason=%q）", run.Status, run.Reason)
	}
	return run
}

// us22Confirm 走管理面确认端点（空 body = keep_both），返回回执。
// us22Confirm confirms a candidate with an empty body (keep_both).
func us22Confirm(t *testing.T, addr string, entryID int64) struct {
	EntryID      int64  `json:"entry_id"`
	State        string `json:"state"`
	SupersededID int64  `json:"superseded_id"`
} {
	t.Helper()
	resp := webRequest(t, addr, http.MethodPost,
		fmt.Sprintf("/api/v1/memory/entries/%d/confirm", entryID), "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "确认候选")
	var receipt struct {
		EntryID      int64  `json:"entry_id"`
		State        string `json:"state"`
		SupersededID int64  `json:"superseded_id"`
	}
	decodeWebData(t, resp, &receipt, "确认候选")
	return receipt
}

// us22Delete 走管理面软删端点，返回回执状态。
// us22Delete soft-deletes one entry through the management surface.
func us22Delete(t *testing.T, addr string, entryID int64) struct {
	EntryID int64  `json:"entry_id"`
	State   string `json:"state"`
} {
	t.Helper()
	resp := webRequest(t, addr, http.MethodDelete,
		fmt.Sprintf("/api/v1/memory/entries/%d", entryID), "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "删除记忆")
	var receipt struct {
		EntryID int64  `json:"entry_id"`
		State   string `json:"state"`
	}
	decodeWebData(t, resp, &receipt, "删除记忆")
	return receipt
}

// us22Prompt 是抽取提示词用户消息的判卷投影（字段名即 §3.1.13 的载荷契约）。
// us22Prompt mirrors the extraction prompt payload field for field.
type us22Prompt struct {
	PromptVersion string              `json:"prompt_version"`
	RuleVersion   string              `json:"rule_version"`
	Prior         []us22PromptMessage `json:"prior"`
	Window        []us22PromptMessage `json:"window"`
	Entries       []us22PromptEntry   `json:"entries"`
}

// us22PromptMessage 是窗口 / 前文里的一条消息。
type us22PromptMessage struct {
	MessageID int64  `json:"message_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
}

// us22PromptEntry 是既有条目快照的一行（来源子集与建议目标只能落在快照内）。
type us22PromptEntry struct {
	EntryID int64  `json:"entry_id"`
	Section string `json:"section"`
	State   string `json:"state"`
	Content string `json:"content"`
}

// us22UserMessageID 在冻结窗口里找承载指定引文的用户消息；找不到返回 false——
// 调用方据此判红，不猜消息主键。
// us22UserMessageID finds the user message carrying the quote inside the frozen window.
func us22UserMessageID(prompt us22Prompt, quote string) (int64, bool) {
	for _, message := range prompt.Window {
		if message.Role == "user" && strings.Contains(message.Content, quote) {
			return message.MessageID, true
		}
	}
	return 0, false
}

// us22Evidence 组装一条逐字证据。
func us22Evidence(messageID int64, role, quote string) map[string]any {
	return map[string]any{"message_id": messageID, "role": role, "quote": quote}
}

// us22Item 组装一条模型自报条目（confidence 只作展示，不作门槛）。
func us22Item(kind, content string, evidence ...map[string]any) map[string]any {
	item := map[string]any{"kind": kind, "content": content, "confidence": 0.9}
	if len(evidence) > 0 {
		item["evidence"] = evidence
	}
	return item
}

// us22ExtractReply 渲染抽取模型的顶层输出 `{"items":[...]}`。
func us22ExtractReply(items ...map[string]any) string {
	return mustJSON(map[string]any{"items": items})
}

// us22Model 是回环假模型：对话请求按 FIFO 消费剧本，抽取请求交给抽取处理函数。
// 抽取处理函数可阻塞（崩溃恢复用例），阻塞时必须同时监听请求上下文，否则用例结束时
// `httptest.Server.Close` 会挂起。
// us22Model splits chat requests (FIFO script) from extraction requests (callback). A
// blocking handler also watches the request context so the test server can shut down.
type us22Model struct {
	t      *testing.T
	server *httptest.Server

	mu           sync.Mutex
	chat         []testutil.FakeOpenAIResponse
	chatRequests []map[string]any
	extractCalls int
	extractFn    func(prompt us22Prompt, call int) string

	blockExtract bool
	started      chan struct{}
	release      chan struct{}
	startedOnce  sync.Once
	releaseOnce  sync.Once
}

// newUS22Model 起回环假模型并装载对话剧本。
func newUS22Model(t *testing.T, steps ...testutil.FakeOpenAIResponse) *us22Model {
	t.Helper()
	m := &us22Model{
		t:       t,
		chat:    steps,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	m.server = testutil.NewHTTPServer(t, http.HandlerFunc(m.handle))
	return m
}

// URL 返回假模型基址（客户端自己补 `/chat/completions`）。
func (m *us22Model) URL() string { return m.server.URL }

// lastChatRequest 返回最近一次对话请求（判 Agent 拿到的工具结果）。
func (m *us22Model) lastChatRequest(t *testing.T) map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.chatRequests) == 0 {
		t.Fatalf("假模型一次对话请求都没收到")
	}
	return m.chatRequests[len(m.chatRequests)-1]
}

// extractionCount 返回收到的抽取请求数（默认关闭用例判零调用）。
func (m *us22Model) extractionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.extractCalls
}

// releaseExtract 解除抽取阻塞（幂等）。
func (m *us22Model) releaseExtract() {
	m.releaseOnce.Do(func() { close(m.release) })
}

// handle 按系统提示词分流：带「记忆抽取器」的请求是抽取调用，其余是对话回合。
func (m *us22Model) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
		http.NotFound(w, r)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		m.t.Errorf("假模型：读请求体失败: %v", err)
		http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
		return
	}
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		m.t.Errorf("假模型：请求体不是 JSON: %v", err)
		http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
		return
	}

	if strings.Contains(us22MessageContent(request, 0), "记忆抽取器") {
		m.handleExtraction(w, r, request)
		return
	}
	m.handleChat(w, request)
}

// handleChat 按 FIFO 消费对话剧本；剧本用尽即判红（防用例少写一步而静默通过）。
func (m *us22Model) handleChat(w http.ResponseWriter, request map[string]any) {
	m.mu.Lock()
	m.chatRequests = append(m.chatRequests, request)
	if len(m.chat) == 0 {
		m.mu.Unlock()
		m.t.Errorf("假模型：对话剧本已用尽，仍收到请求")
		http.Error(w, `{"error":{"message":"script exhausted"}}`, http.StatusInternalServerError)
		return
	}
	reply := m.chat[0]
	m.chat = m.chat[1:]
	m.mu.Unlock()
	us22WriteCompletion(w, reply)
}

// handleExtraction 解析抽取载荷并回一个 `{"items":[...]}`；失败仍回空 items，
// 由用例的 t.Errorf 判红而不是伪装成一次「成功但没有候选」。
func (m *us22Model) handleExtraction(w http.ResponseWriter, r *http.Request, request map[string]any) {
	m.mu.Lock()
	m.extractCalls++
	call := m.extractCalls
	fn := m.extractFn
	block := m.blockExtract
	m.mu.Unlock()

	if block {
		m.startedOnce.Do(func() { close(m.started) })
		select {
		case <-m.release:
		case <-r.Context().Done():
			return
		}
	}

	var prompt us22Prompt
	payload := us22MessageContent(request, 1)
	if err := json.Unmarshal([]byte(payload), &prompt); err != nil {
		m.t.Errorf("假模型：抽取载荷不是预期 JSON: %v", err)
		us22WriteCompletion(w, textResponse(us22ExtractReply()))
		return
	}
	reply := us22ExtractReply()
	if fn != nil {
		reply = fn(prompt, call)
	}
	// 抽取仍是**一次对话补全**：`{"items":[...]}` 是助手消息的 content，必须包在
	// OpenAI 兼容的补全信封里；裸 body 会被协议层判成「响应不可解析」。
	// Extraction is still a chat completion: the JSON rides as assistant content inside
	// the OpenAI-compatible envelope, not as the raw HTTP body.
	us22WriteCompletion(w, textResponse(reply))
}

// us22WriteCompletion 写一条 OpenAI 兼容的对话补全响应。
func us22WriteCompletion(w http.ResponseWriter, reply testutil.FakeOpenAIResponse) {
	status := reply.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, reply.Body); err != nil {
		return
	}
}

// us22MessageContent 取请求里第 index 条消息的文本 content（非字符串得空串）。
func us22MessageContent(request map[string]any, index int) string {
	messages, _ := request["messages"].([]any)
	if index < 0 || index >= len(messages) {
		return ""
	}
	message, _ := messages[index].(map[string]any)
	content, _ := message["content"].(string)
	return content
}

// us22TailToolResults 只取**最后一条用户消息之后**的工具结果：同一会话的历史请求里
// 仍带着前几轮的工具消息，整表扫描会把上一轮的命中误判成本轮召回。
// us22TailToolResults returns only the tool results produced after the last user message;
// scanning the whole history would count an earlier round's hit as this round's recall.
func us22TailToolResults(t *testing.T, req map[string]any) []string {
	t.Helper()
	messages := requestMessages(t, req)
	lastUser := -1
	for index, message := range messages {
		if messageString(message, "role") == "user" {
			lastUser = index
		}
	}
	var out []string
	for index, message := range messages {
		if index <= lastUser || messageString(message, "role") != "tool" {
			continue
		}
		out = append(out, messageString(message, "content"))
	}
	return out
}

// us22ExtractionRow 按触发 Run 读抽取账本行。
// us22ExtractionRow reads the extraction row keyed by the triggering run.
func us22ExtractionRow(t *testing.T, db *sql.DB, runID string) (extractionID, status string, windowTo int64) {
	t.Helper()
	err := db.QueryRow(`
		SELECT extraction_id, status, window_to_message_id
		  FROM memory_extractions WHERE run_id = $1`, runID).
		Scan(&extractionID, &status, &windowTo)
	if err != nil {
		t.Fatalf("读触发 Run 的抽取行失败（run_id=%s）: %v", runID, err)
	}
	return extractionID, status, windowTo
}

// us22LatestExtraction 读最近一条抽取行（崩溃用例在响应返回前就要判 pending）。
func us22LatestExtraction(t *testing.T, db *sql.DB) (extractionID, runID, status string, windowTo int64) {
	t.Helper()
	err := db.QueryRow(`
		SELECT extraction_id, run_id, status, window_to_message_id
		  FROM memory_extractions ORDER BY created_at DESC, run_id LIMIT 1`).
		Scan(&extractionID, &runID, &status, &windowTo)
	if err != nil {
		t.Fatalf("读最近一条抽取行失败: %v", err)
	}
	return extractionID, runID, status, windowTo
}

// us22ExtractionFailure 渲染抽取行的失败分类、尝试次数与错误摘要——失败用例要能直接
// 指回分类，而不是只看到 status=failed。
// us22ExtractionFailure renders the failure class, attempts and error for diagnosis.
func us22ExtractionFailure(t *testing.T, db *sql.DB, extractionID string) string {
	t.Helper()
	var class, message sql.NullString
	var attempts int
	if err := db.QueryRow(`
		SELECT failure_class, error_message, attempts
		  FROM memory_extractions WHERE extraction_id = $1`, extractionID).
		Scan(&class, &message, &attempts); err != nil {
		return fmt.Sprintf("（读失败详情失败: %v）", err)
	}
	return fmt.Sprintf("failure_class=%q attempts=%d error=%q", class.String, attempts, message.String)
}

// us22Int64 读一行 int64 标量（条目标识 / 游标这类读数）。
func us22Int64(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("查询 int64 失败 %q: %v", query, err)
	}
	return value
}

// us22String 读一行文本标量（状态 / 来源这类读数）。
func us22String(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var value string
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("查询文本失败 %q: %v", query, err)
	}
	return value
}

// us22EntryIDByContent 按 Agent 与正文读条目主键；不存在即判红。
func us22EntryIDByContent(t *testing.T, db *sql.DB, agent, content string) int64 {
	t.Helper()
	return us22Int64(t, db,
		`SELECT id FROM memory_entries WHERE agent_name = $1 AND content = $2`, agent, content)
}

// us22RequireNoActiveAutoCandidates 判 Agent 层没有生效中的自动抽取条目。
func us22RequireNoActiveAutoCandidates(t *testing.T, db *sql.DB, label string) {
	t.Helper()
	if n := scalarInt(t, db, `
		SELECT COUNT(*) FROM memory_entries
		 WHERE agent_name = $1 AND source = 'auto_extract' AND state = 'active'`, us22Agent); n != 0 {
		t.Fatalf("%s：生效中的自动抽取条目数 = %d，期望 0", label, n)
	}
}

// us22ExtractionDetail 是只读详情端点的判卷子集：账本行 + 候选（含逐字证据）。
type us22ExtractionDetail struct {
	Extraction struct {
		Status        string `json:"status"`
		ItemsAccepted int    `json:"items_accepted"`
	} `json:"extraction"`
	Candidates []struct {
		EntryID        int64  `json:"entry_id"`
		State          string `json:"state"`
		Kind           string `json:"kind"`
		Certainty      string `json:"certainty"`
		TimeConfidence string `json:"time_confidence"`
		Evidence       []struct {
			MessageID int64  `json:"message_id"`
			RunID     string `json:"run_id"`
			Role      string `json:"role"`
			Quote     string `json:"quote"`
		} `json:"evidence"`
	} `json:"candidates"`
}

// us22LoadExtraction 读一次抽取详情（管理面只读端点）。
func us22LoadExtraction(t *testing.T, addr, extractionID string) us22ExtractionDetail {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/learning/extractions/"+extractionID, "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读学习抽取详情")
	var detail us22ExtractionDetail
	decodeWebData(t, resp, &detail, "读学习抽取详情")
	return detail
}

// TestUS22_E2E_ExtractConfirmRecall 是主链：生成 → 确认前不召回 → 确认 → 关键词与语义
// 召回均命中。候选在确认前只以 `candidate` 落库，**不得**参与召回或注入。
// TestUS22_E2E_ExtractConfirmRecall walks generation, the pre-confirm miss, confirmation,
// and both recall paths hitting afterwards.
func TestUS22_E2E_ExtractConfirmRecall(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)

	// 七步剧本 = 三次运行：保存 + 终答 / 确认前召回 + 终答 / 关键词 + 语义 + 终答。
	model := newUS22Model(t,
		toolCallResponse("call-us22-save", "save_memory",
			mustJSON(map[string]string{"content": us22SavedContent})),
		textResponse("已记录成本评估的节奏"),
		toolCallResponse("call-us22-miss", "recall_memory",
			mustJSON(map[string]string{"query": us22CandidateContent, "mode": "keyword"})),
		textResponse("尚未找到"),
		toolCallResponse("call-us22-keyword", "recall_memory",
			mustJSON(map[string]string{"query": us22CandidateContent, "mode": "keyword"})),
		toolCallResponse("call-us22-semantic", "recall_memory",
			mustJSON(map[string]string{"query": us22CandidateContent, "mode": "semantic"})),
		textResponse("已确认并召回"),
	)
	model.extractFn = func(prompt us22Prompt, call int) string {
		if call != 1 {
			return us22ExtractReply()
		}
		messageID, ok := us22UserMessageID(prompt, us22CandidateContent)
		if !ok {
			model.t.Errorf("抽取窗口里没有承载候选的用户消息：%+v", prompt.Window)
			return us22ExtractReply()
		}
		return us22ExtractReply(us22Item("fact", us22CandidateContent,
			us22Evidence(messageID, "user", us22CandidateContent)))
	}
	embed := newUS16EmbeddingServer(t)
	providers := "providers:\n" +
		v3ProviderBlock(fixtureProvider, model.URL(), fixtureModel) +
		v3ProviderBlock(us16EmbedProvider, embed.URL(), us16EmbedModel)
	h.writeConfig(t, v3ServeConfig(addr, providers,
		us16EmbeddingSection()+us16CallersSection()+us22LearningSection(us22Agent)))
	writeWebAgent(t, h, us22Agent, []string{"save_memory", "recall_memory"})

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)
	db := h.openDB(t)

	key := us22IssueCallerKey(t, addr, us22Agent)
	sessionID := us22CreateSession(t, addr, key, us22Agent)

	// [1] 生成：显式保存照旧即生效；抽取候选只落 candidate。
	turn := "请记住：" + us22SavedContent + "。顺便记住：" + us22CandidateContent + "。"
	run1 := us22PostMessage(t, addr, key, sessionID, turn)

	extractionID, status, _ := us22ExtractionRow(t, db, run1.RunID)
	if status != "success" {
		t.Fatalf("抽取行 status = %q，期望 success（%s）", status,
			us22ExtractionFailure(t, db, extractionID))
	}
	if got := scalarInt(t, db,
		`SELECT items_accepted FROM memory_extractions WHERE extraction_id = $1`, extractionID); got != 1 {
		t.Fatalf("items_accepted = %d，期望 1（整条候选被接受）", got)
	}
	if n := scalarInt(t, db, `
		SELECT COUNT(*) FROM memory_entries
		 WHERE agent_name = $1 AND content = $2
		   AND state = 'candidate' AND source = 'auto_extract' AND section = 'archive'`,
		us22Agent, us22CandidateContent); n != 1 {
		t.Fatalf("候选条目数 = %d，期望 1（candidate / auto_extract / archive）", n)
	}
	entryID := us22EntryIDByContent(t, db, us22Agent, us22CandidateContent)

	var kind, certainty, timeConfidence string
	err := db.QueryRow(`
		SELECT kind, certainty, time_confidence FROM memory_candidate_meta WHERE entry_id = $1`,
		entryID).Scan(&kind, &certainty, &timeConfidence)
	if err != nil {
		t.Fatalf("读候选元数据失败: %v", err)
	}
	if kind != "fact" || certainty != "asserted" || timeConfidence != "unknown" {
		t.Fatalf("候选元数据 = (%s, %s, %s)，期望 (fact, asserted, unknown)",
			kind, certainty, timeConfidence)
	}

	// 只读详情：证据必须逐字、指向本 Run 的用户消息。
	detail := us22LoadExtraction(t, addr, extractionID)
	if detail.Extraction.Status != "success" || detail.Extraction.ItemsAccepted != 1 {
		t.Fatalf("详情账本行 = %+v，期望 success/1", detail.Extraction)
	}
	if len(detail.Candidates) != 1 {
		t.Fatalf("详情候选数 = %d，期望 1", len(detail.Candidates))
	}
	candidate := detail.Candidates[0]
	if candidate.EntryID != entryID || candidate.State != "candidate" {
		t.Fatalf("详情候选 = (%d, %s)，期望 (%d, candidate)",
			candidate.EntryID, candidate.State, entryID)
	}
	if len(candidate.Evidence) != 1 {
		t.Fatalf("候选证据数 = %d，期望 1", len(candidate.Evidence))
	}
	evidence := candidate.Evidence[0]
	if evidence.Role != "user" || evidence.Quote != us22CandidateContent ||
		evidence.RunID != run1.RunID || evidence.MessageID <= 0 {
		t.Fatalf("候选证据 = %+v，期望本 Run 的 user 逐字引文", evidence)
	}

	// [2] 确认前召回：候选不生效，必须落「无匹配条目」而不是空态。
	_ = us22PostMessage(t, addr, key, sessionID, "季度评审什么时候进行？")
	miss := toolResultContaining(t, model.lastChatRequest(t), "(无匹配条目)", "确认前不得召回")
	if strings.Contains(miss, us22CandidateContent) {
		t.Fatalf("确认前的召回结果含候选正文（候选不得参与召回）：%q", miss)
	}

	// [3] 确认（空 body = 保留双方）：候选 → active，且没有替代任何旧条目。
	receipt := us22Confirm(t, addr, entryID)
	if receipt.EntryID != entryID || receipt.State != "active" || receipt.SupersededID != 0 {
		t.Fatalf("确认回执 = %+v，期望 (%d, active, 0)", receipt, entryID)
	}
	if got := us22String(t, db, `SELECT state FROM memory_entries WHERE id = $1`, entryID); got != "active" {
		t.Fatalf("确认后条目 state = %q，期望 active", got)
	}

	// [4] 新 Run 召回：关键词命中正文，语义命中带名次与来源。
	_ = us22PostMessage(t, addr, key, sessionID, "季度评审什么时候进行？")
	results := toolResults(t, model.lastChatRequest(t))
	keywordHit, semanticHit := false, false
	for _, result := range results {
		if result == us22CandidateContent {
			keywordHit = true
		}
		if strings.Contains(result, "1. [auto_extract] "+us22CandidateContent) {
			semanticHit = true
		}
	}
	if !keywordHit {
		t.Fatalf("关键词召回没有命中确认后的候选：%v", results)
	}
	if !semanticHit {
		t.Fatalf("语义召回没有命中确认后的候选（期望 `1. [auto_extract] ...`）：%v", results)
	}
}

// TestUS22_E2E_RejectWrongCandidate 判「管理员删除候选即拒绝」：候选永不自动生效，
// 软删后行仍在、状态为 deleted，后续 Run 不召回。
// TestUS22_E2E_RejectWrongCandidate deletes the candidate and requires later runs to miss.
func TestUS22_E2E_RejectWrongCandidate(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)

	model := newUS22Model(t,
		textResponse("我先记下"),
		toolCallResponse("call-us22-miss", "recall_memory",
			mustJSON(map[string]string{"query": us22CandidateContent, "mode": "keyword"})),
		textResponse("没有找到"),
	)
	model.extractFn = func(prompt us22Prompt, call int) string {
		if call != 1 {
			return us22ExtractReply()
		}
		messageID, ok := us22UserMessageID(prompt, us22CandidateContent)
		if !ok {
			model.t.Errorf("抽取窗口里没有承载候选的用户消息：%+v", prompt.Window)
			return us22ExtractReply()
		}
		return us22ExtractReply(us22Item("fact", us22CandidateContent,
			us22Evidence(messageID, "user", us22CandidateContent)))
	}
	providers := "providers:\n" + v3ProviderBlock(fixtureProvider, model.URL(), fixtureModel)
	h.writeConfig(t, v3ServeConfig(addr, providers, us22LearningSection(us22Agent)))
	writeWebAgent(t, h, us22Agent, []string{"recall_memory"})

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)
	db := h.openDB(t)

	key := us22IssueCallerKey(t, addr, us22Agent)
	sessionID := us22CreateSession(t, addr, key, us22Agent)

	run1 := us22PostMessage(t, addr, key, sessionID, "请记住："+us22CandidateContent+"。")
	if extractionID, status, _ := us22ExtractionRow(t, db, run1.RunID); status != "success" {
		t.Fatalf("抽取行 status = %q，期望 success（%s）", status,
			us22ExtractionFailure(t, db, extractionID))
	}
	entryID := us22EntryIDByContent(t, db, us22Agent, us22CandidateContent)

	receipt := us22Delete(t, addr, entryID)
	if receipt.EntryID != entryID || receipt.State != "deleted" {
		t.Fatalf("删除回执 = %+v，期望 (%d, deleted)", receipt, entryID)
	}
	if got := us22String(t, db, `SELECT state FROM memory_entries WHERE id = $1`, entryID); got != "deleted" {
		t.Fatalf("删除后条目 state = %q，期望 deleted（软删，行保留）", got)
	}
	us22RequireNoActiveAutoCandidates(t, db, "删除候选后")

	_ = us22PostMessage(t, addr, key, sessionID, "季度评审什么时候进行？")
	toolResultContaining(t, model.lastChatRequest(t), "(无匹配条目)", "删除候选后不得召回")
}

// TestUS22_E2E_DeleteStopsRecall 判生效后的删除：确认 → 召回命中 → 软删 → 停止召回。
// TestUS22_E2E_DeleteStopsRecall confirms, recalls, deletes, and requires a miss.
func TestUS22_E2E_DeleteStopsRecall(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)

	model := newUS22Model(t,
		textResponse("我先记下"),
		toolCallResponse("call-us22-hit", "recall_memory",
			mustJSON(map[string]string{"query": us22CandidateContent, "mode": "keyword"})),
		textResponse("命中"),
		toolCallResponse("call-us22-miss", "recall_memory",
			mustJSON(map[string]string{"query": us22CandidateContent, "mode": "keyword"})),
		textResponse("已删除"),
	)
	model.extractFn = func(prompt us22Prompt, call int) string {
		if call != 1 {
			return us22ExtractReply()
		}
		messageID, ok := us22UserMessageID(prompt, us22CandidateContent)
		if !ok {
			model.t.Errorf("抽取窗口里没有承载候选的用户消息：%+v", prompt.Window)
			return us22ExtractReply()
		}
		return us22ExtractReply(us22Item("fact", us22CandidateContent,
			us22Evidence(messageID, "user", us22CandidateContent)))
	}
	providers := "providers:\n" + v3ProviderBlock(fixtureProvider, model.URL(), fixtureModel)
	h.writeConfig(t, v3ServeConfig(addr, providers, us22LearningSection(us22Agent)))
	writeWebAgent(t, h, us22Agent, []string{"recall_memory"})

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)
	db := h.openDB(t)

	key := us22IssueCallerKey(t, addr, us22Agent)
	sessionID := us22CreateSession(t, addr, key, us22Agent)

	run1 := us22PostMessage(t, addr, key, sessionID, "请记住："+us22CandidateContent+"。")
	if extractionID, status, _ := us22ExtractionRow(t, db, run1.RunID); status != "success" {
		t.Fatalf("抽取行 status = %q，期望 success（%s）", status,
			us22ExtractionFailure(t, db, extractionID))
	}
	entryID := us22EntryIDByContent(t, db, us22Agent, us22CandidateContent)
	if receipt := us22Confirm(t, addr, entryID); receipt.State != "active" {
		t.Fatalf("确认回执 state = %q，期望 active", receipt.State)
	}

	_ = us22PostMessage(t, addr, key, sessionID, "季度评审什么时候进行？")
	toolResultContaining(t, model.lastChatRequest(t), us22CandidateContent, "确认后召回命中")

	us22Delete(t, addr, entryID)
	_ = us22PostMessage(t, addr, key, sessionID, "季度评审什么时候进行？")
	toolResultContaining(t, model.lastChatRequest(t), "(无匹配条目)", "删除后停止召回")
}

// TestUS22_E2E_DeleteSourceStopsDerived 判派生复核：候选引用既有条目作为来源，来源被
// 删除后派生条目按依赖闭包停止召回（行本身仍是 active），显式条目不受影响。
// TestUS22_E2E_DeleteSourceStopsDerived removes the source and requires the derived entry
// to drop out while explicit entries stay recallable.
func TestUS22_E2E_DeleteSourceStopsDerived(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)

	model := newUS22Model(t,
		// 运行 1：只产候选。
		textResponse("我先记下"),
		// 运行 2：确认后三路召回（派生 / 来源 / 无关显式条目）。
		toolCallResponse("call-us22-derived", "recall_memory",
			mustJSON(map[string]string{"query": "巡检简报", "mode": "keyword"})),
		toolCallResponse("call-us22-source", "recall_memory",
			mustJSON(map[string]string{"query": "巡检每周三", "mode": "keyword"})),
		toolCallResponse("call-us22-unrelated", "recall_memory",
			mustJSON(map[string]string{"query": "周报", "mode": "keyword"})),
		textResponse("三路读完"),
		// 运行 3：来源删除后，派生与来源都必须落空。
		toolCallResponse("call-us22-derived-gone", "recall_memory",
			mustJSON(map[string]string{"query": "巡检简报", "mode": "keyword"})),
		toolCallResponse("call-us22-source-gone", "recall_memory",
			mustJSON(map[string]string{"query": "巡检每周三", "mode": "keyword"})),
		textResponse("都已失效"),
	)
	model.extractFn = func(prompt us22Prompt, call int) string {
		if call != 1 {
			return us22ExtractReply()
		}
		var sourceID int64
		for _, entry := range prompt.Entries {
			if entry.Content == us22SourceContent {
				sourceID = entry.EntryID
			}
		}
		if sourceID == 0 {
			model.t.Errorf("抽取提示词的既有条目快照里没有来源条目：%+v", prompt.Entries)
			return us22ExtractReply()
		}
		messageID, ok := us22UserMessageID(prompt, us22DerivedContent)
		if !ok {
			model.t.Errorf("抽取窗口里没有承载派生候选的用户消息：%+v", prompt.Window)
			return us22ExtractReply()
		}
		item := us22Item("fact", us22DerivedContent,
			us22Evidence(messageID, "user", us22DerivedContent))
		item["source_entry_ids"] = []int64{sourceID}
		return us22ExtractReply(item)
	}
	providers := "providers:\n" + v3ProviderBlock(fixtureProvider, model.URL(), fixtureModel)
	h.writeConfig(t, v3ServeConfig(addr, providers, us22LearningSection(us22Agent)))
	writeWebAgent(t, h, us22Agent, []string{"recall_memory"})
	// 存量记忆直接种库（代表「库里已有」），启动期回填只在有 embedding 时运行。
	seedMemoryRows(t, h,
		seedMemoryRow{Scope: "agent", Agent: us22Agent, Section: "archive", Content: us22SourceContent},
		seedMemoryRow{Scope: "agent", Agent: us22Agent, Section: "archive", Content: us22UnrelatedContent},
	)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)
	db := h.openDB(t)

	key := us22IssueCallerKey(t, addr, us22Agent)
	sessionID := us22CreateSession(t, addr, key, us22Agent)

	run1 := us22PostMessage(t, addr, key, sessionID, "请记住："+us22DerivedContent+"。")
	if extractionID, status, _ := us22ExtractionRow(t, db, run1.RunID); status != "success" {
		t.Fatalf("抽取行 status = %q，期望 success（%s）", status,
			us22ExtractionFailure(t, db, extractionID))
	}
	derivedID := us22EntryIDByContent(t, db, us22Agent, us22DerivedContent)
	sourceID := us22EntryIDByContent(t, db, us22Agent, us22SourceContent)
	if n := scalarInt(t, db, `
		SELECT COUNT(*) FROM memory_candidate_sources
		 WHERE entry_id = $1 AND source_entry_id = $2`, derivedID, sourceID); n != 1 {
		t.Fatalf("候选来源关系行数 = %d，期望 1", n)
	}
	if receipt := us22Confirm(t, addr, derivedID); receipt.State != "active" {
		t.Fatalf("确认派生候选 state = %q，期望 active", receipt.State)
	}

	// 来源有效时：派生、来源、无关显式条目都命中。
	_ = us22PostMessage(t, addr, key, sessionID, "巡检相关的都找一下")
	results := us22TailToolResults(t, model.lastChatRequest(t))
	derivedHit, sourceHit, unrelatedHit := false, false, false
	for _, result := range results {
		if strings.Contains(result, us22DerivedContent) {
			derivedHit = true
		}
		if strings.Contains(result, us22SourceContent) {
			sourceHit = true
		}
		if strings.Contains(result, us22UnrelatedContent) {
			unrelatedHit = true
		}
	}
	if !derivedHit || !sourceHit || !unrelatedHit {
		t.Fatalf("来源有效时的召回 = (派生 %v, 来源 %v, 无关 %v)，期望全部命中：%v",
			derivedHit, sourceHit, unrelatedHit, results)
	}

	// 删除来源：派生条目按闭包排除，来源本身也不再召回。
	us22Delete(t, addr, sourceID)
	_ = us22PostMessage(t, addr, key, sessionID, "巡检相关的都找一下")
	after := us22TailToolResults(t, model.lastChatRequest(t))
	if len(after) != 2 {
		t.Fatalf("来源删除后的本轮工具结果条数 = %d，期望 2：%q", len(after), after)
	}
	for _, result := range after {
		if strings.Contains(result, us22DerivedContent) || strings.Contains(result, us22SourceContent) {
			t.Fatalf("来源删除后仍召回失效闭包内的条目：%q", result)
		}
		if result != "(无匹配条目)" {
			t.Fatalf("来源删除后的召回结果 = %q，期望无匹配占位", result)
		}
	}
	if got := us22String(t, db, `SELECT state FROM memory_entries WHERE id = $1`, derivedID); got != "active" {
		t.Fatalf("派生条目 state = %q，期望 active（过滤在召回面，不做级联软删）", got)
	}
}

// TestUS22_E2E_RecoveryAfterCrash 判崩溃恢复：抽取在途被杀 → 重启只查证不重放 →
// 同来源新窗口不再被阻断。
// TestUS22_E2E_RecoveryAfterCrash kills an in-flight extraction and requires verification
// without replay, then a fresh extraction on the next window.
func TestUS22_E2E_RecoveryAfterCrash(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)

	model := newUS22Model(t, textResponse("第一次回复"))
	model.blockExtract = true
	providers := "providers:\n" + v3ProviderBlock(fixtureProvider, model.URL(), fixtureModel)
	// US-24 起崩溃恢复由过期租约的回收轮次驱动（FR-24-02 / 卡片 §7 Q2），夹具把执行权
	// 时效压到可等待的量级；判据仍是"只查证不重放"的终态，不放宽任何断言。
	h.writeConfig(t, v3ServeConfig(addr, providers, us22LearningSection(us22Agent))+fastWorkerSection())
	writeWebAgent(t, h, us22Agent, nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	// 本用例**故意**杀进程模拟崩溃：收尾不得按「优雅停机退出码 0」判卷。
	// This case kills the process on purpose, so cleanup must not assert a graceful exit.
	defer p.kill()
	waitHealthHTTP(t, p, addr, 30*time.Second)
	db := h.openDB(t)

	key := us22IssueCallerKey(t, addr, us22Agent)
	sessionID := us22CreateSession(t, addr, key, us22Agent)

	// 消息请求在抽取处阻塞：放到子 goroutine，用假模型的 started 信号判「已开始」。
	done := make(chan error, 1)
	go func() {
		_, err := doWebRequest(context.Background(), http.MethodPost, addr,
			"/api/v1/sessions/"+sessionID+"/messages",
			mustJSON(map[string]any{"message": us22CrashTurn}), us22Bearer(key))
		done <- err
	}()
	select {
	case <-model.started:
	case <-time.After(20 * time.Second):
		t.Fatalf("抽取调用在 20s 内没有开始\n%s", p.output())
	}

	// 在途状态：抽取行与抽取调用审计都是 pending，且没有候选落库。
	extractionID, runID, status, windowTo := us22LatestExtraction(t, db)
	if status != "pending" {
		t.Fatalf("在途抽取行 status = %q，期望 pending", status)
	}
	if got := us22String(t, db,
		`SELECT status FROM llm_calls WHERE run_id = $1 AND purpose = 'memory_extract'`, runID); got != "pending" {
		t.Fatalf("在途抽取调用审计 status = %q，期望 pending", got)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM memory_candidate_meta`); n != 0 {
		t.Fatalf("在途抽取写出的候选数 = %d，期望 0", n)
	}

	p.kill()
	_ = p.waitExit(t, 30*time.Second)
	model.releaseExtract()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("被杀进程的消息请求没有返回")
	}

	addr2 := freeLoopbackAddr(t)
	model2 := newUS22Model(t, textResponse("第二次回复"))
	providers2 := "providers:\n" + v3ProviderBlock(fixtureProvider, model2.URL(), fixtureModel)
	h.writeConfig(t, v3ServeConfig(addr2, providers2, us22LearningSection(us22Agent))+fastWorkerSection())
	p2 := startGrouped(t, h, fixtureEnv(), "serve")
	defer p2.stop(t)
	waitHealthHTTP(t, p2, addr2, 30*time.Second)

	// 恢复只查证：pending → unknown + verified_at，游标推进到冻结窗口末尾；
	// 抽取调用审计不重放（同一 Run 仍只有一条，且被归一化为 unknown）。
	// 查证入口有两处：启动扫描按归属保护跳过**仍有活租约**的行，孤儿行由回收器
	// （B3）在 TTL 到期后领取——因此这里等到状态翻转，而不是健康检查后立刻读。
	// Two entry points exist now: startup skips rows with a live lease and the reclaimer
	// takes over after expiry, so the verdict is a bounded wait rather than an instant read.
	require.Eventually(t, func() bool {
		var status string
		return db.QueryRow(`SELECT status FROM memory_extractions WHERE extraction_id = $1`, extractionID).Scan(&status) == nil && status == "unknown"
	}, 30*time.Second, 50*time.Millisecond, "恢复后的抽取行 status 未变为 unknown（只查证不重放）")
	if n := scalarInt(t, db,
		`SELECT COUNT(*) FROM memory_extractions WHERE extraction_id = $1 AND verified_at IS NOT NULL`, extractionID); n != 1 {
		t.Fatalf("恢复后未写查证时间（verified_at 为空）")
	}
	cursor := us22Int64(t, db, `SELECT committed_message_id FROM memory_extraction_cursors`)
	if cursor != windowTo {
		t.Fatalf("恢复后游标 = %d，期望冻结窗口末尾 %d", cursor, windowTo)
	}
	if n := scalarInt(t, db,
		`SELECT COUNT(*) FROM llm_calls WHERE run_id = $1 AND purpose = 'memory_extract'`, runID); n != 1 {
		t.Fatalf("恢复后抽取调用数 = %d，期望 1（不得重放）", n)
	}
	if got := us22String(t, db,
		`SELECT status FROM llm_calls WHERE run_id = $1 AND purpose = 'memory_extract'`, runID); got != "unknown" {
		t.Fatalf("恢复后的抽取调用审计 status = %q，期望 unknown", got)
	}

	// 查证后不再阻断：新消息产生新窗口，第二次抽取正常成功。
	run2 := us22PostMessage(t, addr2, key, sessionID, us22CrashTurn2)
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM memory_extractions WHERE agent_name = $1`, us22Agent); n != 2 {
		t.Fatalf("抽取行总数 = %d，期望 2（恢复后允许新窗口）", n)
	}
	var secondStatus string
	var secondFrom int64
	err := db.QueryRow(`
		SELECT status, window_from_message_id FROM memory_extractions WHERE run_id = $1`,
		run2.RunID).Scan(&secondStatus, &secondFrom)
	if err != nil {
		t.Fatalf("读第二次抽取行失败: %v", err)
	}
	if secondStatus != "success" {
		t.Fatalf("第二次抽取 status = %q，期望 success", secondStatus)
	}
	if secondFrom <= windowTo {
		t.Fatalf("第二次窗口起点 = %d，期望大于已提交游标 %d", secondFrom, windowTo)
	}
}

// TestUS22_E2E_DisabledNoSideEffects 判出厂默认关闭：零抽取行、零抽取调用、零候选、
// 零草案目录，状态页如实报 enabled=false。
// TestUS22_E2E_DisabledNoSideEffects requires zero extraction rows, calls, candidates and
// drafts when the learning section is absent.
func TestUS22_E2E_DisabledNoSideEffects(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)

	model := newUS22Model(t, textResponse("默认关闭"))
	providers := "providers:\n" + v3ProviderBlock(fixtureProvider, model.URL(), fixtureModel)
	// 不写 learning 段：出厂默认关闭。
	h.writeConfig(t, v3ServeConfig(addr, providers, ""))
	writeWebAgent(t, h, us22Agent, nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)
	db := h.openDB(t)

	session := webCreateSession(t, addr, us22Agent, nil)
	resp := webPostMessage(t, addr, session.SessionID, "请记住：季度评审固定在每月第一个周四。")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "发消息")
	var run webRun
	decodeWebData(t, resp, &run, "发消息")
	if run.Status != "completed" {
		t.Fatalf("运行的 status = %q，期望 completed（reason=%q）", run.Status, run.Reason)
	}

	if n := scalarInt(t, db, `SELECT COUNT(*) FROM memory_extractions`); n != 0 {
		t.Fatalf("默认关闭时抽取行数 = %d，期望 0", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM llm_calls WHERE purpose = 'memory_extract'`); n != 0 {
		t.Fatalf("默认关闭时抽取调用数 = %d，期望 0", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM memory_entries WHERE source = 'auto_extract'`); n != 0 {
		t.Fatalf("默认关闭时自动抽取条目数 = %d，期望 0", n)
	}
	if n := model.extractionCount(); n != 0 {
		t.Fatalf("默认关闭时假模型收到抽取请求 %d 次，期望 0", n)
	}

	draftRoot := filepath.Join(h.Root, ".basalt", "learning", "drafts")
	entries, err := os.ReadDir(draftRoot)
	switch {
	case err == nil && len(entries) != 0:
		t.Fatalf("默认关闭时草案目录非空：%v", entries)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		t.Fatalf("读草案目录 %s 失败: %v", draftRoot, err)
	}

	resp = webRequest(t, addr, http.MethodGet, "/api/v1/learning/status", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读学习状态")
	var status struct {
		Enabled bool     `json:"enabled"`
		Agents  []string `json:"agents"`
	}
	decodeWebData(t, resp, &status, "读学习状态")
	if status.Enabled {
		t.Fatalf("默认关闭时状态页 enabled = true，期望 false")
	}
	if len(status.Agents) != 0 {
		t.Fatalf("默认关闭时状态页 agents = %v，期望空列表", status.Agents)
	}
}
