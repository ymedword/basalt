//go:build e2e && real

package e2e

// 本文件是 US-16 模块卡 §2.17「真实服务层」R1–R6 的判卷面（T104 →
// `harness/stages/us16-real.json` 的 `us16-external`）。七条用例与 stage 的
// `required_tests` 逐字对应，一条不多、一条不少。
//
// 三条纪律（模块卡 §4.6）：
//   - **缺凭证即失败，不跳过**：任何 `t.Skip` 都会让"整套外部验证变红"退化成
//     "整套外部验证没跑"，因此这里只出现 `t.Fatalf`，且文案只点名变量名。
//   - **凭据只经进程环境**：配置里永远只有 `${VAR}` 占位；值由父进程环境继承进
//     子进程，失败文案经 `h.redact` 脱敏。
//   - **不把假上游当真实连接**：外部服务必须是真实目标；只有模型允许脚本化替身，
//     而 R1–R4 是"产品链路对真实服务"，因此这几条用真模型跑。
//
// 判据取舍（逐条对号 §2.17 表）：
//   - R1 只判"连通 + 模型/维度与配置同源 + 审计有行"，**不**判召回质量与费用；
//   - R2 只判"非空文本 + `llm_calls` 终态 + 配额预留结算"，**不**判回答质量；
//   - R3 判"搜索解析成功 + 结果不带正文 + 至少一页正文落 `pages.json`"，正文抓取按
//     卡内「`fetch` MCP **或** `http_get` 另外抓取」用 `http_get`；
//   - R4 判"组件启动 + `tools/list` 非空 + 建连测试 + 一次只读调用 + 固定目标仓"，
//     目标仓 `ymedword/basalt`，实际 commit 记进日志；
//   - R5 判"HTTP 2xx + 投递记录有行"，成败只在 `tool_invocations` 上（契约的投递表
//     **没有** status 列）；
//   - R6 逐 Agent 一行给出七项盘点，业务项按"实际值 / 未验证 + 原因"写，**不**合并计数。
//
// This file is the real-service judging surface of module-card §2.17 R1–R6. Missing
// credentials fail (never skip), credentials only travel through the process
// environment, and every external service is real; R1–R4 additionally drive a real
// model because those rows judge the product chain rather than answer quality.

import (
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/knowledge"
)

// 外部服务层的环境变量名：与模块卡 §4.6 的占位名逐字一致。
// The external-tier variable names match module-card §4.6 verbatim.
const (
	us16ExtEmbedKeyEnv   = "DASHSCOPE_API_KEY"
	us16ExtEmbedBaseEnv  = "BASALT_DEV_EMBEDDING_BASE_URL"
	us16ExtEmbedModelEnv = "BASALT_DEV_EMBEDDING_MODEL"
	us16ExtEmbedDimEnv   = "BASALT_DEV_EMBEDDING_DIMENSIONS"

	us16ExtChatKeyEnv   = "DEEPSEEK_API_KEY"
	us16ExtChatBaseEnv  = "DEEPSEEK_BASE_URL"
	us16ExtChatModelEnv = "DEEPSEEK_MODEL"

	us16ExtGitHubTokenEnv   = "GITHUB_TOKEN"
	us16ExtGitHubTokenAlias = "GITHUB_PERSONAL_ACCESS_TOKEN"
	us16ExtGitHubCommandEnv = "GITHUB_MCP_COMMAND"

	us16ExtFeishuURLEnv = "FEISHU_WEBHOOK_URL"
)

// 缺省值只在**变量未给出**时生效：DeepSeek 的公开端点是稳定常量，
// 模型名仍可由 `DEEPSEEK_MODEL` 覆盖（模块卡："模型名以实际配置为准"）。
// Defaults apply only when the variable is absent; the model id stays overridable.
const (
	us16ExtChatDefaultBaseURL = "https://api.deepseek.com/v1"
	us16ExtChatDefaultModel   = "deepseek-chat"
	// us16ExtGitHubRepo 是 R4 冻结的只读目标：公开仓，报告里记录实际 owner/repo 与 commit。
	// us16ExtGitHubRepo is the frozen read-only target repository for R4.
	us16ExtGitHubRepo = "ymedword/basalt"
)

// provider 名与渠道名：配置、审计列与断言共用同一组字面量。
// Provider and channel names shared by config, audit columns and assertions.
const (
	us16ExtEmbedProvider = "bailian"
	us16ExtChatProvider  = "deepseek"
	us16ExtNotifyChannel = "ops"
)

// us16ExtModelCallCeiling 是**异常循环保护**，不是费用闸门：模块卡 §4.6 明确
// "不设冻结金额、不设每项请求次数硬闸门"。它只在真实模型累计调用数远超七条用例的
// 合理规模时判红——要抓的是"同一调用反复重试无进展"（§4.6 停止规则第一条）。
// A runaway-loop guard, not a cost gate: the module card sets no per-request ceiling.
const us16ExtModelCallCeiling = 80

// us16ExtModelCalls 跨七条用例累计（同一 `go test` 进程内）。
// Accumulated across the seven cases inside one `go test` process.
var us16ExtModelCalls atomic.Int64

// us16ExtLLM 是一次真实 Chat 端点的运行时读数；APIKey 只进子进程环境。
// us16ExtLLM carries one real chat endpoint reading; the key only reaches the child.
type us16ExtLLM struct {
	BaseURL string
	Model   string
	APIKey  string
}

// us16ExtEmbedding 是一次真实 embedding 端点的运行时读数。
// us16ExtEmbedding carries one real embedding endpoint reading.
type us16ExtEmbedding struct {
	BaseURL string
	Model   string
	APIKey  string
}

// requireUS16ExtEnv 读取一组必填变量；缺失即 FAIL，文案只点名变量。
// requireUS16ExtEnv fails (never skips) and names the variable only, never the value.
func requireUS16ExtEnv(t *testing.T, label string, keys ...string) {
	t.Helper()
	var missing []string
	for _, key := range keys {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s 缺少环境变量 %v——按模块卡 §4.6，缺凭据即 FAIL，不得 SKIP。"+
			"凭据只在进程环境里注入（DPAPI 加载器不入库）", label, missing)
	}
}

// us16ExtAbsURL 校验绝对 URL（scheme + host 都必须有），失败只点名变量。
// us16ExtAbsURL requires an absolute URL and names the variable on failure.
func us16ExtAbsURL(t *testing.T, key, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		t.Fatalf("%s 不是绝对 URL（需要 scheme://host）", key)
	}
	return raw
}

// requireUS16ExtDeepSeek 读 DeepSeek 三项读数；base URL 与模型有公开缺省，Key 必填。
// requireUS16ExtDeepSeek reads the three DeepSeek readings with public defaults.
func requireUS16ExtDeepSeek(t *testing.T) us16ExtLLM {
	t.Helper()
	forbidRealModelValidation(t)
	requireUS16ExtEnv(t, "真实 DeepSeek Chat", us16ExtChatKeyEnv)
	base := strings.TrimSpace(os.Getenv(us16ExtChatBaseEnv))
	if base == "" {
		base = us16ExtChatDefaultBaseURL
	}
	model := strings.TrimSpace(os.Getenv(us16ExtChatModelEnv))
	if model == "" {
		model = us16ExtChatDefaultModel
	}
	return us16ExtLLM{
		BaseURL: us16ExtAbsURL(t, us16ExtChatBaseEnv, base),
		Model:   model,
		APIKey:  os.Getenv(us16ExtChatKeyEnv),
	}
}

// requireUS16ExtEmbedding 读百炼三项读数。`BASALT_DEV_EMBEDDING_DIMENSIONS` 是**交接
// 变量而非配置键**（`Enter-DevEnv.ps1` 的提示原文）：交接值是 1024，而 D-05 把列宽
// 冻结成 512，两者本就不该相等。因此这里**只记录、不判等**——配置永远写冻结宽度；
// 若把交接值当判据，会把一条正确的运行判成红。真正的一致性由 `embedding_calls` /
// `kb_index_generations` 的维度列与冻结值比对来判。
// The handoff dimension is not a config key: it reads 1024 while D-05 froze 512, so it is
// recorded, never compared. Consistency is judged on the stored dimension columns instead.
func requireUS16ExtEmbedding(t *testing.T) us16ExtEmbedding {
	t.Helper()
	forbidRealModelValidation(t)
	requireUS16ExtEnv(t, "真实 embedding", us16ExtEmbedKeyEnv, us16ExtEmbedBaseEnv, us16ExtEmbedModelEnv)
	if handoff := strings.TrimSpace(os.Getenv(us16ExtEmbedDimEnv)); handoff != "" {
		t.Logf("交接维度 %s=%s（非配置键；本验证按 D-05 冻结宽度 %d 配置）",
			us16ExtEmbedDimEnv, handoff, knowledge.VectorDimensions)
	}
	return us16ExtEmbedding{
		BaseURL: us16ExtAbsURL(t, us16ExtEmbedBaseEnv, strings.TrimSpace(os.Getenv(us16ExtEmbedBaseEnv))),
		Model:   strings.TrimSpace(os.Getenv(us16ExtEmbedModelEnv)),
		APIKey:  os.Getenv(us16ExtEmbedKeyEnv),
	}
}

// requireUS16ExtGitHubToken 读 GitHub 只读 PAT；两个别名按模块卡的顺序取第一个非空。
// requireUS16ExtGitHubToken reads the read-only PAT from either documented alias.
func requireUS16ExtGitHubToken(t *testing.T) string {
	t.Helper()
	for _, key := range []string{us16ExtGitHubTokenEnv, us16ExtGitHubTokenAlias} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	t.Fatalf("真实 GitHub MCP 缺少环境变量 %s（或别名 %s）——按模块卡 §4.6，缺凭据即 FAIL，不得 SKIP",
		us16ExtGitHubTokenEnv, us16ExtGitHubTokenAlias)
	return ""
}

// requireUS16ExtFeishuURL 读飞书 webhook；缺失即 FAIL。失败文案只出 `scheme://host`。
// requireUS16ExtFeishuURL reads the webhook; diagnostics carry scheme://host only.
func requireUS16ExtFeishuURL(t *testing.T) (string, *url.URL) {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(us16ExtFeishuURLEnv))
	if raw == "" {
		t.Fatalf("真实飞书外发缺少环境变量 %s——按模块卡 §4.6，缺凭据即 FAIL，不得 SKIP", us16ExtFeishuURLEnv)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		t.Fatalf("%s 不是绝对 URL（需要 scheme://host）", us16ExtFeishuURLEnv)
	}
	if parsed.Path == "" && parsed.RawQuery == "" && parsed.User == nil {
		t.Fatalf("%s 缺少可判定的令牌段（path / query / userinfo 均为空）", us16ExtFeishuURLEnv)
	}
	return raw, parsed
}

// us16ExtEnv 返回子进程需要的外部凭据环境：管理面 token 是夹具哨兵，另加非空的真实 Key。
// 空值不登记——登记空串会让脱敏面把整段输出抹掉，掩盖真实读数。
// us16ExtEnv returns the child environment; empty keys are never added nor registered.
func us16ExtEnv(llm us16ExtLLM, embed us16ExtEmbedding) []string {
	env := []string{e2eAdminTokenEnv + "=" + e2eAdminToken}
	if llm.APIKey != "" {
		env = append(env, us16ExtChatKeyEnv+"="+llm.APIKey)
	}
	if embed.APIKey != "" {
		env = append(env, us16ExtEmbedKeyEnv+"="+embed.APIKey)
	}
	return env
}

// newUS16ExtHarness 建夹具并登记需要脱敏的运行时读数：base URL 与 Key 都按凭证对待。
// newUS16ExtHarness builds the fixture and registers every runtime value for redaction.
func newUS16ExtHarness(t *testing.T, llm us16ExtLLM, embed us16ExtEmbedding) *harness {
	t.Helper()
	h := newHarness(t)
	h.setRunTimeout(realRunTimeout)
	if llm.BaseURL != "" {
		h.registerSecret(us16ExtChatBaseEnv, llm.BaseURL)
	}
	if llm.APIKey != "" {
		h.registerSecret(us16ExtChatKeyEnv, llm.APIKey)
	}
	if embed.BaseURL != "" {
		h.registerSecret(us16ExtEmbedBaseEnv, embed.BaseURL)
	}
	if embed.APIKey != "" {
		h.registerSecret(us16ExtEmbedKeyEnv, embed.APIKey)
	}
	return h
}

// us16ExtProviderBlock 渲染一条 env 模式的 Provider 条目（相对 `providers:` 的两空格缩进）。
// us16ExtProviderBlock renders one env-mode provider entry inside `providers:`.
func us16ExtProviderBlock(name, baseURL, keyEnv, model string) string {
	return "  - name: " + name + "\n" +
		"    base_url: " + baseURL + "\n" +
		"    api_key: ${" + keyEnv + "}\n" +
		"    default_model: " + model + "\n"
}

// us16ExtProviders 渲染 `providers:` 段；给定哪个读数就写哪个。
// us16ExtProviders renders the `providers:` block from whichever readings are set.
func us16ExtProviders(llm us16ExtLLM, embed us16ExtEmbedding) string {
	out := "providers:\n"
	if embed.Model != "" {
		out += us16ExtProviderBlock(us16ExtEmbedProvider, embed.BaseURL, us16ExtEmbedKeyEnv, embed.Model)
	}
	if llm.Model != "" {
		out += us16ExtProviderBlock(us16ExtChatProvider, llm.BaseURL, us16ExtChatKeyEnv, llm.Model)
	}
	return out
}

// us16ExtEmbeddingSection 渲染 embedding 段：连接名、运行时模型与冻结维度。
// us16ExtEmbeddingSection renders the embedding section with the frozen width.
func us16ExtEmbeddingSection(embed us16ExtEmbedding) string {
	return "embedding:\n" +
		"  model_connection: " + us16ExtEmbedProvider + "\n" +
		"  model: " + embed.Model + "\n" +
		"  dimensions: " + strconv.Itoa(knowledge.VectorDimensions) + "\n"
}

// us16ExtFrontmatter 渲染指向真实 provider 的 AGENT.md frontmatter。模型名由运行时读数
// 填入，不在夹具里硬编码（模块卡：模型名以实际配置为准）。
// us16ExtFrontmatter renders frontmatter pointing at the real provider with the runtime
// model id; nothing here hardcodes a model name.
func us16ExtFrontmatter(name, model string, tools, servers []string) string {
	out := "name: " + name + "\n" +
		"description: US-16 真实服务层夹具\n" +
		"provider:\n  name: " + us16ExtChatProvider + "\n  model: \"" + model + "\"\n"
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

// us16ExtStartServe 起 serve 并等健康检查；返回进程句柄。
// us16ExtStartServe starts serve and waits for the health endpoint.
func us16ExtStartServe(t *testing.T, h *harness, env []string, addr string) *groupedProcess {
	t.Helper()
	p := startGrouped(t, h, env, "serve")
	waitHealthHTTP(t, p, addr, 60*time.Second)
	return p
}

// us16ExtToolRows 读某个工具的审计行（按写入顺序，正文已剥掉 `{content}` 外壳）；
// 无行即判红——工具调用必须落审计。剥壳是**尽力而为**：失败行可能没有正文，此时保留
// 原文，好让真正要判的失败分支给出准确文案，而不是在剥壳处先炸。
// us16ExtToolRows reads one tool's audit rows in insertion order, stripping the envelope
// best-effort so a failed row's own assertion reports the real problem.
// us16ExtDumpRunDiagnostics 把失败 Run 的落库事实原样打进测试日志：对外口径只有
// 固定文案「运行失败」，真实 error 只在 `runs.reason` / `llm_calls.error_message` /
// `tool_invocations.error_message` 里。只读、只在失败分支调用，不改变任何断言。
// us16ExtDumpRunDiagnostics dumps the stored failure facts (read-only, failure path only).
func us16ExtDumpRunDiagnostics(t *testing.T, db *sql.DB, runID string) {
	t.Helper()
	for _, row := range queryRows(t, db,
		`SELECT status, reason, iterations, tool_calls FROM runs WHERE run_id = $1`, runID) {
		t.Logf("诊断 runs：status=%q reason=%q iterations=%v tool_calls=%v",
			stringValue(t, row, "status"), stringValue(t, row, "reason"),
			row["iterations"], row["tool_calls"])
	}
	for _, row := range queryRows(t, db,
		`SELECT round, provider, model, status, error_kind, error_message, purpose
FROM llm_calls WHERE run_id = $1 ORDER BY id`, runID) {
		t.Logf("诊断 llm_calls：round=%v provider=%q model=%q status=%q kind=%q purpose=%q error=%q",
			row["round"], stringValue(t, row, "provider"), stringValue(t, row, "model"),
			stringValue(t, row, "status"), stringValue(t, row, "error_kind"),
			stringValue(t, row, "purpose"), truncate(stringValue(t, row, "error_message"), 400))
	}
	for _, row := range queryRows(t, db,
		`SELECT tool_name, status, error_kind, error_message
FROM tool_invocations WHERE run_id = $1 ORDER BY id`, runID) {
		t.Logf("诊断 tool_invocations：tool=%q status=%q kind=%q error=%q",
			stringValue(t, row, "tool_name"), stringValue(t, row, "status"),
			stringValue(t, row, "error_kind"), truncate(stringValue(t, row, "error_message"), 400))
	}
}

// us16ExtDumpTimeline 给"没有成功响应、拿不到 run_id"的失败（典型是 504 撞 60 秒
// 运行预算）打出全部审计行的耗时：没有时间线就只能猜是搜索慢、模型慢还是抓取慢。
// 只读、只在失败分支调用。
// us16ExtDumpTimeline prints per-call durations when the run id is unknown.
func us16ExtDumpTimeline(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, row := range queryRows(t, db,
		`SELECT round, provider, model, status, error_kind, purpose, duration_ms, created_at
FROM llm_calls ORDER BY id`) {
		t.Logf("时间线 llm_calls：round=%v provider=%q model=%q status=%q kind=%q purpose=%q duration_ms=%v at=%v",
			row["round"], stringValue(t, row, "provider"), stringValue(t, row, "model"),
			stringValue(t, row, "status"), stringValue(t, row, "error_kind"),
			stringValue(t, row, "purpose"), row["duration_ms"], row["created_at"])
	}
	for _, row := range queryRows(t, db,
		`SELECT tool_name, status, error_kind, error_message, duration_ms, started_at, finished_at
FROM tool_invocations ORDER BY id`) {
		t.Logf("时间线 tool_invocations：tool=%q status=%q kind=%q duration_ms=%v started_at=%v finished_at=%v error=%q",
			stringValue(t, row, "tool_name"), stringValue(t, row, "status"),
			stringValue(t, row, "error_kind"), row["duration_ms"], row["started_at"], row["finished_at"],
			truncate(stringValue(t, row, "error_message"), 300))
	}
}

func us16ExtToolRows(t *testing.T, db *sql.DB, tool string) []map[string]any {
	t.Helper()
	rows := queryRows(t, db,
		`SELECT tool_name, status, error_kind, error_message, input_json, result_json
FROM tool_invocations WHERE tool_name = $1 ORDER BY id`, tool)
	if len(rows) == 0 {
		t.Fatalf("工具 %s 没有留下审计行——工具调用必须落审计", tool)
	}
	for _, row := range rows {
		row["content"] = us16ExtUnwrap(stringValue(t, row, "result_json"))
	}
	return rows
}

// us16ExtUnwrap 剥掉工具结果的 `{content}` 外壳；不是该形态就原样返回。
// us16ExtUnwrap strips the tool-result envelope and returns the raw text otherwise.
func us16ExtUnwrap(raw string) string {
	var shape struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &shape); err != nil || shape.Content == "" {
		return raw
	}
	return shape.Content
}

// accountUS16ExtModelCalls 读全部 llm_calls 行做形状断言（provider / model / 终态），
// 再累计到异常循环保护计数上。
// accountUS16ExtModelCalls asserts every llm_calls row and accumulates the guard count.
func accountUS16ExtModelCalls(t *testing.T, db *sql.DB, provider, model, label string) int {
	t.Helper()
	rows := queryRows(t, db, `SELECT provider, model, status FROM llm_calls`)
	for _, row := range rows {
		if got := stringValue(t, row, "provider"); provider != "" && got != provider {
			t.Fatalf("%s：llm_calls.provider = %q，期望 %q", label, got, provider)
		}
		if got := stringValue(t, row, "model"); model != "" && got != model {
			t.Fatalf("%s：llm_calls.model = %q，期望运行时模型 %q", label, got, model)
		}
		if got := stringValue(t, row, "status"); got != "success" {
			t.Fatalf("%s：llm_calls 存在非 success 行（%q）——真实服务失败必须如实判红", label, got)
		}
	}
	total := us16ExtModelCalls.Add(int64(len(rows)))
	if total > us16ExtModelCallCeiling {
		t.Fatalf("%s：真实模型累计调用 %d 次，超过异常循环保护上限 %d——"+
			"按 §4.6 停止规则先排查异常循环，不得继续重试", label, total, us16ExtModelCallCeiling)
	}
	t.Logf("%s：llm_calls=%d 累计=%d", label, len(rows), total)
	return len(rows)
}

// ---------------------------------------------------------------------------
// R1 摄取侧真实 embedding
// ---------------------------------------------------------------------------

// TestUS16External_EmbeddingIngest 判 R1 的摄取半：一次真实跑通 Basalt 自己的入库链路，
// 并核对 `embedding_calls` 与 `kb_index_generations` 的模型 / 维度与配置同源。
// 逆改点：模型或维度写错、摄取跳过真实 embedding → 审计行缺失或列值不符。
func TestUS16External_EmbeddingIngest(t *testing.T) {
	embed := requireUS16ExtEmbedding(t)
	// 本用例只判 embedding：chat provider 不配置，也不需要 DeepSeek 凭据。
	// Embedding only: no chat provider, so no DeepSeek credential is required here.
	h := newUS16ExtHarness(t, us16ExtLLM{}, embed)
	initWorkspace(t, h)

	addr := freeLoopbackAddr(t)
	h.writeConfig(t, v3ServeConfig(addr, us16ExtProviders(us16ExtLLM{}, embed),
		us16ExtEmbeddingSection(embed)+us16CallersSection()))
	writeUS16Manual(t, h, us16ManualV1)

	p := us16ExtStartServe(t, h, us16ExtEnv(us16ExtLLM{}, embed), addr)
	defer p.stop(t)

	key := us16IssueCallerKey(t, addr)
	header := map[string]string{"Authorization": "Bearer " + key}

	collectionID := us16CreateCollection(t, addr, header)
	reading := us16Ingest(t, addr, header, collectionID)
	if reading.ChunkCount < 1 {
		t.Fatalf("真实摄取 chunk_count = %d，期望 ≥ 1", reading.ChunkCount)
	}
	if reading.Generation < 1 {
		t.Fatalf("真实摄取 generation = %d，期望 ≥ 1", reading.Generation)
	}

	db := h.openDB(t)
	rows := queryRows(t, db, `SELECT model, dimensions, input_count, status, error_kind
FROM embedding_calls WHERE ingest_job_id = $1 ORDER BY id`, reading.IngestJobID)
	if len(rows) == 0 {
		t.Fatalf("真实摄取没有留下 embedding_calls 行（ingest_job_id=%s）——"+
			"审计 fail-closed 要求先写 pending 再发调用", reading.IngestJobID)
	}
	for _, row := range rows {
		if got := stringValue(t, row, "model"); got != embed.Model {
			t.Fatalf("embedding_calls.model = %q，期望配置模型 %q", got, embed.Model)
		}
		if got := intValue(t, row, "dimensions"); got != int64(knowledge.VectorDimensions) {
			t.Fatalf("embedding_calls.dimensions = %d，期望冻结宽度 %d", got, knowledge.VectorDimensions)
		}
		if got := stringValue(t, row, "status"); got != "success" {
			t.Fatalf("embedding_calls 存在非 success 行（%q / error_kind=%q）",
				got, stringValue(t, row, "error_kind"))
		}
		if got := intValue(t, row, "input_count"); got < 1 {
			t.Fatalf("embedding_calls.input_count = %d，期望 ≥ 1", got)
		}
	}

	// 代际记录必须与配置同源（模型名 + 冻结宽度 + 距离），且摄取的代际已生效。
	// The active generation must match the configured model and frozen width.
	generations := queryRows(t, db, `SELECT generation, embedding_model, dimensions, distance, status
FROM kb_index_generations WHERE status = 'active'`)
	if len(generations) != 1 {
		t.Fatalf("active 代际行数 = %d，期望恰好 1", len(generations))
	}
	if got := stringValue(t, generations[0], "embedding_model"); got != embed.Model {
		t.Fatalf("kb_index_generations.embedding_model = %q，期望 %q", got, embed.Model)
	}
	if got := intValue(t, generations[0], "dimensions"); got != int64(knowledge.VectorDimensions) {
		t.Fatalf("kb_index_generations.dimensions = %d，期望 %d", got, knowledge.VectorDimensions)
	}
	if got := intValue(t, generations[0], "generation"); got != int64(reading.Generation) {
		t.Fatalf("摄取 generation = %d，active 代际 = %d，两者必须一致",
			reading.Generation, got)
	}
	t.Logf("r1_ingest：model=%s dimensions=%d distance=%s chunks=%d embedding_calls=%d",
		embed.Model, knowledge.VectorDimensions, stringValue(t, generations[0], "distance"),
		reading.ChunkCount, len(rows))
}

// ---------------------------------------------------------------------------
// R1 查询侧真实 embedding
// ---------------------------------------------------------------------------

// TestUS16External_EmbeddingQuery 判 R1 的查询半：同模型同维度的一次真实检索，
// 且向量路必须真的生效——`us16SearchTop` 内部断言 `generation ≥ 1`，该值只在查询
// 向量真算出来时才带。
// 逆改点：查询侧降级为纯关键词 → generation 归零，断言红。
func TestUS16External_EmbeddingQuery(t *testing.T) {
	embed := requireUS16ExtEmbedding(t)
	h := newUS16ExtHarness(t, us16ExtLLM{}, embed)
	initWorkspace(t, h)

	addr := freeLoopbackAddr(t)
	h.writeConfig(t, v3ServeConfig(addr, us16ExtProviders(us16ExtLLM{}, embed),
		us16ExtEmbeddingSection(embed)+us16CallersSection()))
	writeUS16Manual(t, h, us16ManualV1)

	p := us16ExtStartServe(t, h, us16ExtEnv(us16ExtLLM{}, embed), addr)
	defer p.stop(t)

	key := us16IssueCallerKey(t, addr)
	header := map[string]string{"Authorization": "Bearer " + key}

	collectionID := us16CreateCollection(t, addr, header)
	ingested := us16Ingest(t, addr, header, collectionID)

	hit := us16SearchTop(t, addr, header, collectionID, us16ManualPhrase+"多久")
	if hit.DocumentID != ingested.DocumentID {
		t.Fatalf("检索命中 %s，期望刚摄取的文档 %s", hit.DocumentID, ingested.DocumentID)
	}
	t.Logf("r1_query：document=%s version=%s", hit.DocumentID, hit.VersionID)
}

// ---------------------------------------------------------------------------
// R2 DeepSeek Chat
// ---------------------------------------------------------------------------

// TestUS16External_DeepSeekChat 判 R2：一次真实 Chat 调用必须返回非空文本、
// `llm_calls` 落到 success 终态、配额预留从 pending 结算为 committed。
// 逆改点：用量缺失导致预留停在 pending，或模型名与配置不符 → 对应断言红。
func TestUS16External_DeepSeekChat(t *testing.T) {
	llm := requireUS16ExtDeepSeek(t)
	h := newUS16ExtHarness(t, llm, us16ExtEmbedding{})
	initWorkspace(t, h)

	addr := freeLoopbackAddr(t)
	h.writeConfig(t, v3ServeConfig(addr, us16ExtProviders(llm, us16ExtEmbedding{}),
		us16CallersSection()))
	h.writeAgent(t, us16AgentName, us16ExtFrontmatter(us16AgentName, llm.Model, nil, nil),
		"真实 Chat 夹具。")

	p := us16ExtStartServe(t, h, us16ExtEnv(llm, us16ExtEmbedding{}), addr)
	defer p.stop(t)

	// 用调用方 Key 而不是管理 token：内置管理员走 NoReservation，配额面根本不会被触达。
	// Key 必须带 `agents:invoke:<name>`——会话入口本身只要 `sessions:write`，但记忆
	// 注入的读取复核逐次判 Agent scope，缺了就是 fail-closed 的运行失败。
	// A caller key, not the admin token: the builtin admin skips reservations entirely.
	key := us16IssueCallerKey(t, addr, us16AgentName)
	header := map[string]string{"Authorization": "Bearer " + key}

	session := us16CreateSession(t, addr, header)
	body := mustJSON(map[string]any{"message": "只回复两个字：收到"})
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/sessions/"+session+"/messages", body, header)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "真实 Chat 调用")
	var run webRun
	decodeWebData(t, resp, &run, "真实 Chat 调用")
	if run.Status != "completed" {
		t.Fatalf("真实 Chat 运行 status = %q（reason=%q），期望 completed", run.Status, run.Reason)
	}
	if strings.TrimSpace(run.FinalText) == "" {
		t.Fatalf("真实 Chat 返回空文本——R2 要求非空回答")
	}

	db := h.openDB(t)
	calls := queryRows(t, db, `SELECT provider, model, status FROM llm_calls WHERE run_id = $1`, run.RunID)
	if len(calls) == 0 {
		t.Fatalf("真实 Chat 没有留下 llm_calls 行（run_id=%s）", run.RunID)
	}
	for _, row := range calls {
		if got := stringValue(t, row, "provider"); got != us16ExtChatProvider {
			t.Fatalf("llm_calls.provider = %q，期望 %q", got, us16ExtChatProvider)
		}
		if got := stringValue(t, row, "model"); got != llm.Model {
			t.Fatalf("llm_calls.model = %q，期望运行时模型 %q", got, llm.Model)
		}
		if got := stringValue(t, row, "status"); got != "success" {
			t.Fatalf("llm_calls 存在非 success 行（%q）", got)
		}
	}

	// 配额结算分两段查，各自有契约依据：
	//  ① Run 级准入预留（class=`run`，键 `run:<run_id>:admit`）必须按 §3.4.2 的
	//     写入顺序回填 `quota_reservations.run_id`——这是"管理面能把 Run 与预留
	//     对上"的列，同时 `runs.reservation_id` 必须指向同一条；
	//  ② Chat 预留（class=`chat`）按 C-29 的幂等键格式
	//     `run:<run_id>:chat:<step_id>:<attempt_id>` 定位：`QuotaStore.Reserve`
	//     的签名（§3.1.5）没有 Run 入参，chat 行因此不是经 run_id 列归集的——
	//     测试按唯一键查，不去断言设计里没有的写入路径。
	// class 字面量取契约五值 `run` / `chat` / `compaction` / `ingest` / `rerank`
	// 中的短名；DDL 行尾注释里的 "run-chat" 是陈旧措辞、不构成约束
	// （`schema.pg.sql` 的 class 列无 CHECK，见 tasks.md 的 T069 行）。
	// The run-class reservation is looked up by run_id (§3.4.2 backfill); the chat
	// reservation by its C-29 idempotency key, because Reserve carries no run id.
	reservations := queryRows(t, db, `SELECT class, state, actual FROM quota_reservations
WHERE run_id = $1 ORDER BY reservation_id`, run.RunID)
	runRows := 0
	for _, row := range reservations {
		if stringValue(t, row, "class") != "run" {
			continue
		}
		runRows++
		if got := stringValue(t, row, "state"); got != "committed" {
			t.Fatalf("Run 级预留 state = %q，期望 committed", got)
		}
		if actual := intValue(t, row, "actual"); actual != 1 {
			t.Fatalf("Run 级预留 actual = %d，期望 1（一次逻辑准入记 1 个任务）", actual)
		}
	}
	if runRows == 0 {
		t.Fatalf("Run 级预留没有回填 run_id（run_id=%s）——§3.4.2 要求账本行与预留同事务挂接", run.RunID)
	}
	ledgerReservation := ""
	for _, row := range queryRows(t, db, `SELECT reservation_id FROM runs WHERE run_id = $1`, run.RunID) {
		ledgerReservation = stringValue(t, row, "reservation_id")
	}
	if ledgerReservation == "" {
		t.Fatalf("runs.reservation_id 为空（run_id=%s）——账本行必须记下准入预留", run.RunID)
	}

	chatRows := queryRows(t, db, `SELECT class, state FROM quota_reservations
WHERE idempotency_key LIKE 'run:' || $1 || ':chat:%' ORDER BY reservation_id`, run.RunID)
	if len(chatRows) == 0 {
		t.Fatalf("真实 Chat 没有 chat 预留行（run_id=%s）——R2 要求配额预留结算", run.RunID)
	}
	for _, row := range chatRows {
		if got := stringValue(t, row, "class"); got != "chat" {
			t.Fatalf("按 chat 幂等键找到的行 class = %q，期望 chat", got)
		}
		if got := stringValue(t, row, "state"); got != "committed" {
			t.Fatalf("chat 预留 state = %q，期望 committed（真实模型必须回报用量）", got)
		}
	}
	accountUS16ExtModelCalls(t, db, us16ExtChatProvider, llm.Model, "r2_chat")
	t.Logf("r2_chat：run=%s llm_calls=%d run_reservations=%d chat_reservations=%d reservation_id=%s",
		run.RunID, len(calls), len(reservations), len(chatRows), ledgerReservation)
}

// ---------------------------------------------------------------------------
// R3 DeepSeek Search + 正文抓取
// ---------------------------------------------------------------------------

// us16ExtSearchOutput 是 `web_search` 成功正文的判卷形状。模块卡要求
// `need_content=false`：结果只带摘要，**不得**带正文——正文由抓取另走一步，
// 因此这里把 `content` / `body` 两个可能的正文字段显式解出来做负向断言。
// us16ExtSearchOutput is the verdict shape of a web_search body; no body field may
// appear, which is the observable consequence of need_content=false.
type us16ExtSearchOutput struct {
	Query   string `json:"query"`
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
		Summary string `json:"summary"`
		Content string `json:"content"`
		Body    string `json:"body"`
	} `json:"results"`
}

// TestUS16External_IndustryBriefSearch 判 R3：真实 `web_search`（DeepSeek Search API，
// `need_content=false`）解析成功且结果不带正文；随后按卡内允许的 `http_get` 抓到至少
// 一页正文并落 `pages.json`。`industry-brief` 的完整业务简报属 R6 的逐 Agent 行，
// 不在本条内声称。
// 逆改点：搜索返回空结果却仍算通过、只做搜索不抓正文、或搜索结果带正文 → 断言红。
func TestUS16External_IndustryBriefSearch(t *testing.T) {
	llm := requireUS16ExtDeepSeek(t)
	h := newUS16ExtHarness(t, llm, us16ExtEmbedding{})
	initWorkspace(t, h)

	addr := freeLoopbackAddr(t)
	h.writeConfig(t, v3ServeConfig(addr, us16ExtProviders(llm, us16ExtEmbedding{}),
		us16CallersSection()))
	h.writeAgent(t, us16AgentName,
		us16ExtFrontmatter(us16AgentName, llm.Model,
			[]string{"web_search", "http_get", "write_file"}, nil),
		"真实检索 + 抓取夹具：先搜索，再抓正文，最后落 pages.json。")

	p := us16ExtStartServe(t, h, us16ExtEnv(llm, us16ExtEmbedding{}), addr)
	defer p.stop(t)

	// 会话入口跑这个 Agent，Key 必须带 `agents:invoke:<name>`（记忆读取复核逐次判）。
	key := us16IssueCallerKey(t, addr, us16AgentName)
	header := map[string]string{"Authorization": "Bearer " + key}

	// 抓取目标按环境可达性挑选：本机实测（2026-10-31）github.com / hub.docker.com /
	// docs.docker.com / en.wikipedia.org 均超时（000），而 example.com / api.github.com /
	// baidu.com / gitee.com 可达；搜索结果头部又多落在不可达主机上。会话入口的运行
	// 时限是 60 秒（`web.defaultRunTimeout`，产品默认、本测试不改），每一条不可达
	// 链接要耗掉十几秒，因此只允许试 **1 条**搜索链接，失败立即退到
	// `https://example.com/`——多试几条会把整个 Run 撑过 60 秒，得到 504 而不是
	// 一条关于抓取链的读数。抓取链本身（真实外呼 → 正文 → 落盘）不因环境放宽：
	// 仍然必须是真实 http_get 的返回正文。
	// One search link at most, then a known-reachable fallback, because the session
	// endpoint's 60s run budget is a product default this test must not raise.
	instr := "请严格按顺序执行三步，不要省略：\n" +
		"1) 调用 web_search，参数 {\"query\": \"Basalt agent os github\", \"count\": 5}；\n" +
		"2) 从返回结果里挑第一条 host 不是 github.com 的 http(s) 链接，调用 http_get 抓取该页面正文；" +
		"若抓取失败（或结果里没有可用的非 github.com 链接），**立即**改为抓取 https://example.com/ ，" +
		"不要再试其它结果链接；\n" +
		"3) 调用 write_file，path 写 pages.json，content 第一行写实际抓取成功的链接、第二行写正文前 200 字。\n" +
		"三步都完成后，只回复一行：SEARCH_OK=<结果条数> FETCH_OK=<正文字节数>"
	session := us16CreateSession(t, addr, header)
	body := mustJSON(map[string]any{"message": instr})
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/sessions/"+session+"/messages", body, header)
	if resp.Status != http.StatusOK {
		// 504 拿不到 run_id：先按整个工作区把每个调用的耗时打出来，再走标准失败断言。
		us16ExtDumpTimeline(t, h.openDB(t))
	}
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "真实搜索 + 抓取")
	var run webRun
	decodeWebData(t, resp, &run, "真实搜索 + 抓取")
	if run.Status != "completed" {
		// 公开面只给固定文案（`web.envelope.publicReasonFailed`），真实原因在库里；
		// 失败时把 runs / llm_calls / tool_invocations 的原始读数打出来，否则只剩
		// "运行失败"四个字，排障被迫重放真实外呼。
		// The public reason is a fixed literal; dump the stored rows so a failure does
		// not force an expensive replay against real services.
		us16ExtDumpRunDiagnostics(t, h.openDB(t), run.RunID)
		t.Fatalf("真实搜索 + 抓取 status = %q（reason=%q），期望 completed", run.Status, run.Reason)
	}

	db := h.openDB(t)
	searchRows := us16ExtToolRows(t, db, "web_search")
	lastSearch := searchRows[len(searchRows)-1]
	if got := stringValue(t, lastSearch, "status"); got != "success" {
		t.Fatalf("web_search 终态 = %q（error_kind=%q error=%q），期望 success",
			got, stringValue(t, lastSearch, "error_kind"), truncate(stringValue(t, lastSearch, "error_message"), 300))
	}
	rawSearch := stringValue(t, lastSearch, "content")
	if strings.TrimSpace(rawSearch) == "" {
		t.Fatalf("web_search 结果为空——R3 要求搜索结果非空且解析成功")
	}
	var searchPayload us16ExtSearchOutput
	if err := json.Unmarshal([]byte(rawSearch), &searchPayload); err != nil {
		t.Fatalf("web_search 结果不是预期 JSON: %v（原文 %q）", err, truncate(rawSearch, 400))
	}
	if len(searchPayload.Results) == 0 {
		t.Fatalf("真实 web_search 零结果——R3 要求搜索结果非空（原文：%q）", truncate(rawSearch, 400))
	}
	usable := 0
	for _, item := range searchPayload.Results {
		if strings.TrimSpace(item.Content) != "" || strings.TrimSpace(item.Body) != "" {
			t.Fatalf("web_search 结果带了正文（title=%q）——need_content=false 的可见后果被破坏", item.Title)
		}
		parsed, err := url.Parse(item.URL)
		if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") &&
			parsed.Host != "" && parsed.User == nil && strings.TrimSpace(item.Title) != "" {
			usable++
		}
	}
	if usable == 0 {
		t.Fatalf("web_search 没有任何可用结果（需 title + 绝对 http(s) URL 且无 userinfo）")
	}

	// 判据是"至少一页正文抓到并对号"，不是"最后一次尝试恰好可达"：同一台机器上
	// 不同主机的可达性不同（见上），模型按指令换链接重试是合法路径。逆改点不变——
	// 全部抓取失败、或成功行没有正文，仍然红。
	// The criterion is "at least one real page fetched", not "the last attempt succeeded".
	fetchRows := us16ExtToolRows(t, db, "http_get")
	var fetchRow map[string]any
	fetchBody := ""
	for _, row := range fetchRows {
		if stringValue(t, row, "status") != "success" {
			continue
		}
		body := stringValue(t, row, "content")
		if len(strings.TrimSpace(body)) < 100 {
			continue
		}
		fetchRow = row
		fetchBody = body
	}
	if fetchBody == "" {
		// 全失败时把每一次尝试的目标与原因都打出来：只报"末次"会逼人重放真实外呼
		// 才能看出模型换了哪些链接、哪些主机在本机不可达。
		// On total failure every attempt is reported, not just the last one.
		attempts := make([]string, 0, len(fetchRows))
		for i, row := range fetchRows {
			attempts = append(attempts, "#"+strconv.Itoa(i+1)+
				" status="+stringValue(t, row, "status")+
				" error_kind="+stringValue(t, row, "error_kind")+
				" error="+truncate(stringValue(t, row, "error_message"), 160)+
				" input="+truncate(stringValue(t, row, "input_json"), 160))
		}
		t.Fatalf("http_get 没有任何一次抓到 ≥100 字节正文（共 %d 次）——R3 要求抓到至少一页正文：\n%s",
			len(fetchRows), strings.Join(attempts, "\n"))
	}
	fetchTarget := stringValue(t, fetchRow, "input_json")
	if !strings.Contains(fetchTarget, "http") {
		t.Fatalf("http_get 成功行没有留下 http(s) 目标（input=%q）", truncate(fetchTarget, 200))
	}

	writeRows := us16ExtToolRows(t, db, "write_file")
	if got := stringValue(t, writeRows[len(writeRows)-1], "status"); got != "success" {
		t.Fatalf("write_file 终态 = %q，期望 success", got)
	}

	// pages.json 落盘：由 Agent 经 write_file 写出，不是夹具自己造的证据文件。
	// pages.json is written by the agent through write_file, never fabricated here.
	pagesPath := ""
	walkErr := filepath.WalkDir(filepath.Join(h.Root, ".basalt"),
		func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && entry.Name() == "pages.json" && pagesPath == "" {
				pagesPath = path
			}
			return nil
		})
	if walkErr != nil {
		t.Fatalf("遍历工作区找 pages.json 失败: %v", walkErr)
	}
	if pagesPath == "" {
		t.Fatalf("R3 要求抓到的正文落 pages.json——工作区里没有该文件")
	}
	pagesBody, readErr := os.ReadFile(pagesPath)
	if readErr != nil {
		t.Fatalf("读 pages.json 失败: %v", readErr)
	}
	if len(strings.TrimSpace(string(pagesBody))) < 1 {
		t.Fatalf("pages.json 为空——抓到的正文没有真正落盘")
	}
	accountUS16ExtModelCalls(t, db, us16ExtChatProvider, llm.Model, "r3_search")
	t.Logf("r3_search：results=%d usable=%d fetch_attempts=%d fetch_bytes=%d fetch_input=%s pages=%s",
		len(searchPayload.Results), usable, len(fetchRows), len(fetchBody),
		truncate(fetchTarget, 200), pagesPath)
}

// truncate 截断长正文，只用于失败文案（避免把整页塞进日志）。
// truncate shortens a payload for failure messages only.
func truncate(raw string, limit int) string {
	if len(raw) <= limit {
		return raw
	}
	return raw[:limit] + "…"
}

// ---------------------------------------------------------------------------
// R4 GitHub MCP（只读）
// ---------------------------------------------------------------------------

// us16ExtMCPServerView 是 `GET /api/v1/mcp-servers` 的单条投影（只取本条用到的字段）。
// us16ExtMCPServerView is the slice of the MCP list projection this row judges.
type us16ExtMCPServerView struct {
	Name               string   `json:"name"`
	Connected          bool     `json:"connected"`
	AppliedVersionID   int64    `json:"applied_version_id"`
	PublishedVersionID int64    `json:"published_version_id"`
	Tools              []string `json:"tools"`
}

// TestUS16External_GitHubMcpReadOnly 判 R4：内置 github-mcp-server 组件真的启动、
// `tools/list` 非空、`POST /test` 能建连并发现工具、一次只读 Agent 调用落到审计，
// 目标固定公开仓 `ymedword/basalt`，实际 commit 记录进日志。
// 逆改点：组件缺失 / 未启动 → connected=false；只读参数丢失 → 写工具出现在列表里。
func TestUS16External_GitHubMcpReadOnly(t *testing.T) {
	token := requireUS16ExtGitHubToken(t)
	llm := requireUS16ExtDeepSeek(t)
	h := newUS16ExtHarness(t, llm, us16ExtEmbedding{})
	h.registerSecret(us16ExtGitHubTokenEnv, token)
	initWorkspace(t, h)

	// 组件随发布布局交付：`<程序目录>/runtime/mcp/github-mcp-server/<ver>/…`。
	// 用临时构建的二进制时没有这棵树，因此这里 fail-closed 点名 BASALT_E2E_BINARY。
	// The component ships beside the binary; a temp-built binary has no runtime tree.
	componentDir := filepath.Join(filepath.Dir(h.Binary), "runtime", "mcp", "github-mcp-server", "2.0.3")
	command := strings.TrimSpace(os.Getenv(us16ExtGitHubCommandEnv))
	if command == "" {
		for _, candidate := range []string{"github-mcp-server.exe", "github-mcp-server"} {
			path := filepath.Join(componentDir, candidate)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				command = path
				break
			}
		}
	}
	if command == "" {
		t.Fatalf("找不到内置 github-mcp-server 组件（%s 或环境变量 %s）——"+
			"把 BASALT_E2E_BINARY 指向 bin/basalt.exe（其同级 runtime/ 带五个随包组件）；"+
			"缺组件即 FAIL，不得 SKIP", componentDir, us16ExtGitHubCommandEnv)
	}

	servers := "servers:\n" +
		"  - name: github\n" +
		"    transport: stdio\n" +
		"    command: '" + command + "'\n" +
		"    args:\n" +
		"      - stdio\n" +
		"      - --read-only\n" +
		"    env:\n" +
		"      GITHUB_PERSONAL_ACCESS_TOKEN: ${" + us16ExtGitHubTokenEnv + "}\n"
	h.writeMCPServers(t, servers)

	addr := freeLoopbackAddr(t)
	// MCP stdio 启动与 shell 工具共用同一授权面，命令按整串逐字比对。
	// MCP stdio startup shares the shell authorization surface; the command is matched
	// verbatim against the allowlist.
	h.writeConfig(t, v3ServeConfig(addr, us16ExtProviders(llm, us16ExtEmbedding{}),
		us16CallersSection()+mcpShellAllow(command)))
	h.writeAgent(t, us16AgentName,
		us16ExtFrontmatter(us16AgentName, llm.Model,
			[]string{"list_commits"}, []string{"github"}),
		"真实 GitHub MCP 只读夹具：目标公开仓 "+us16ExtGitHubRepo+"。")

	env := append(us16ExtEnv(llm, us16ExtEmbedding{}),
		us16ExtGitHubTokenEnv+"="+token, us16ExtGitHubTokenAlias+"="+token)
	p := us16ExtStartServe(t, h, env, addr)
	defer p.stop(t)

	// ① 组件启动与工具列表：管理面读数，不依赖模型。
	serversResp := webRequest(t, addr, http.MethodGet, "/api/v1/mcp-servers", "", nil)
	requireWebStatus(t, serversResp, http.StatusOK, "OK", "", "读 MCP 列表")
	var listed []us16ExtMCPServerView
	decodeWebData(t, serversResp, &listed, "读 MCP 列表")
	var githubEntry *us16ExtMCPServerView
	for i := range listed {
		if listed[i].Name == "github" {
			githubEntry = &listed[i]
		}
	}
	if githubEntry == nil {
		t.Fatalf("MCP 列表里没有 github 条目：组件没有登记进装配")
	}
	if !githubEntry.Connected {
		t.Fatalf("github MCP 未连接（component=%s）——组件启动失败", command)
	}
	if len(githubEntry.Tools) == 0 {
		t.Fatalf("github MCP tools/list 为空——R4 要求工具列表非空")
	}
	requireUS16ExtReadOnlyTools(t, githubEntry.Tools)

	// ② 真实建连测试：`POST /mcp-servers/github/test` 建一次临时连接并回工具发现结果。
	//    诊断端点只读、不建连，因此连通性只能由这一条给出。
	testResp := webRequest(t, addr, http.MethodPost, "/api/v1/mcp-servers/github/test", "", nil)
	requireWebStatus(t, testResp, http.StatusOK, "OK", "", "MCP 连通性测试")
	var tested struct {
		OK              bool     `json:"ok"`
		Tools           []string `json:"tools"`
		Stage           string   `json:"stage"`
		FailureClass    string   `json:"failure_class"`
		ErrorMessage    string   `json:"error_message"`
		TestedVersionID int64    `json:"tested_version_id"`
	}
	decodeWebData(t, testResp, &tested, "MCP 连通性测试")
	if !tested.OK {
		t.Fatalf("MCP 连通性测试失败（stage=%q failure_class=%q error=%q）",
			tested.Stage, tested.FailureClass, truncate(tested.ErrorMessage, 300))
	}
	if len(tested.Tools) == 0 {
		t.Fatalf("MCP 连通性测试返回空工具列表")
	}
	requireUS16ExtReadOnlyTools(t, tested.Tools)
	if tested.TestedVersionID != githubEntry.PublishedVersionID {
		t.Fatalf("测试版本 %d != 已发布版本 %d——测的不是当前发布版本",
			tested.TestedVersionID, githubEntry.PublishedVersionID)
	}

	// ③ 一次只读 Agent 调用：固定目标仓，只允许 list_commits 这一个读工具。
	// 会话入口跑这个 Agent，Key 必须带 `agents:invoke:<name>`（记忆读取复核逐次判）。
	key := us16IssueCallerKey(t, addr, us16AgentName)
	header := map[string]string{"Authorization": "Bearer " + key}
	session := us16CreateSession(t, addr, header)
	message := "请调用 list_commits，参数 {\"owner\": \"ymedword\", \"repo\": \"basalt\", \"perPage\": 5}，" +
		"取回该公开仓最近的提交，然后只回复一行：COMMITS=<条数> HEAD=<第一条的 sha>"
	body := mustJSON(map[string]any{"message": message})
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/sessions/"+session+"/messages", body, header)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "真实 GitHub MCP 调用")
	var run webRun
	decodeWebData(t, resp, &run, "真实 GitHub MCP 调用")
	if run.Status != "completed" {
		t.Fatalf("真实 GitHub MCP 调用 status = %q（reason=%q），期望 completed", run.Status, run.Reason)
	}

	db := h.openDB(t)
	rows := us16ExtToolRows(t, db, "list_commits")
	last := rows[len(rows)-1]
	if got := stringValue(t, last, "status"); got != "success" {
		t.Fatalf("list_commits 终态 = %q（error_kind=%q error=%q），期望 success",
			got, stringValue(t, last, "error_kind"), truncate(stringValue(t, last, "error_message"), 300))
	}
	inputJSON := stringValue(t, last, "input_json")
	requireContains(t, inputJSON, "ymedword", "只读调用必须指向冻结目标 owner")
	requireContains(t, inputJSON, "basalt", "只读调用必须指向冻结目标 repo")

	// 实际 commit：从审计结果里取 40 位 sha，逐字记录进日志（R4 要求记录）。
	sha := firstHexSHA(stringValue(t, last, "content"))
	if sha == "" {
		t.Fatalf("list_commits 结果里没有 40 位 commit sha——R4 要求记录实际 commit（结果：%q）",
			truncate(stringValue(t, last, "content"), 400))
	}
	accountUS16ExtModelCalls(t, db, us16ExtChatProvider, llm.Model, "r4_github")
	t.Logf("r4_github：component=%s tool=list_commits repo=%s head=%s tools=%d",
		filepath.Base(command), us16ExtGitHubRepo, sha, len(tested.Tools))
}

// requireUS16ExtReadOnlyTools 断言只读模式下不出现写工具，且读工具在列。
// requireUS16ExtReadOnlyTools rejects write tools and requires the read tool.
func requireUS16ExtReadOnlyTools(t *testing.T, tools []string) {
	t.Helper()
	for _, tool := range tools {
		switch tool {
		case "create_issue", "create_pull_request", "merge_pull_request", "push_files",
			"delete_file", "update_issue", "create_branch", "fork_repository":
			t.Fatalf("只读模式下出现了写工具 %q——--read-only 没有生效", tool)
		}
	}
	for _, tool := range tools {
		if tool == "list_commits" {
			return
		}
	}
	t.Fatalf("服务端工具列表里没有 list_commits（列表：%v）——R4 的只读调用没有可判定的落点", tools)
}

// firstHexSHA 从工具结果里取出第一个 40 位十六进制串（commit sha）。
// firstHexSHA returns the first 40-hex run in the payload, i.e. the commit sha.
func firstHexSHA(raw string) string {
	const width = 40
	for i := 0; i+width <= len(raw); i++ {
		// 前后不得紧邻十六进制字符，避免从更长的串里截一段。
		// Neighbours must not be hex so a longer run is never sliced mid-way.
		if i > 0 && isHexByte(raw[i-1]) {
			continue
		}
		if i+width < len(raw) && isHexByte(raw[i+width]) {
			continue
		}
		if isHexRun(raw[i : i+width]) {
			return raw[i : i+width]
		}
	}
	return ""
}

// isHexRun 判断整串是否都是十六进制字符。
// isHexRun reports whether the whole string is hexadecimal.
func isHexRun(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHexByte(s[i]) {
			return false
		}
	}
	return len(s) > 0
}

// isHexByte 判断单字节是否十六进制字符。
// isHexByte reports whether one byte is a hexadecimal digit.
func isHexByte(b byte) bool {
	switch {
	case b >= '0' && b <= '9':
		return true
	case b >= 'a' && b <= 'f':
		return true
	case b >= 'A' && b <= 'F':
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// R5 飞书通知
// ---------------------------------------------------------------------------

// TestUS16External_FeishuNotify 判 R5：一条测试消息真的发到用户配置的测试目标，
// 且 `notify_deliveries` 有投递记录、终态由 `tool_invocations` 权威给出
// （契约的投递表没有 status 列）。
// 逆改点：发送成功但没有投递行，或投递行指向别的渠道 → 对应断言红。
func TestUS16External_FeishuNotify(t *testing.T) {
	raw, parsed := requireUS16ExtFeishuURL(t)
	h := newHarness(t)
	h.setRunTimeout(realRunTimeout)
	h.registerSecret(us16ExtFeishuURLEnv, raw)

	const marker = "US-16 真实飞书验证（合成数据，非生产通知）"
	fake := newFakeModel(t,
		toolCallResponse("call-feishu-ext", "notify", mustJSON(map[string]string{
			"channel": us16ExtNotifyChannel, "content": marker,
		})),
		textResponse("真实外发完成"),
	)
	h.writeConfig(t, configWithProvider(fake.URL())+realHTTPBlock("", []string{parsed.Hostname()}))
	initWorkspace(t, h)
	// 渠道声明沿用 real_test 的既有写法（不写 notify_mode，按旧兼容规则推断）。
	// The channel binding follows the proven real test: no notify_mode, legacy inference.
	h.writeAgent(t, "feishu-ext", agentFrontmatter("feishu-ext", []string{"notify"}),
		"真实飞书外发夹具。")
	insertNotifyChannel(t, h, us16ExtNotifyChannel, "feishu",
		"${"+us16ExtFeishuURLEnv+"}", "US-16 外部验证渠道")

	res := h.run(t, append(fixtureEnv(), us16ExtFeishuURLEnv+"="+raw),
		"chat", "--agent", "feishu-ext", "--message", "发送一条 US-16 验证消息")
	requireRealExit(t, h, res, 0, "真实飞书外发")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	result := lastToolContent(reqs[1])
	requireContains(t, result, "已推送到通知渠道："+us16ExtNotifyChannel, "成功文案回给模型")

	// 令牌零回显：模型上下文、进程输出、工作区、审计都不得出现 webhook 的路径/查询段。
	// Token zero-echo: the path/query segment must appear nowhere.
	token := strings.TrimPrefix(raw, parsed.Scheme+"://"+parsed.Host)
	requireAbsent(t, result, token, "模型上下文不得回显 webhook 令牌")
	requireAbsent(t, res.Stdout+res.Stderr, token, "进程输出不得回显 webhook 令牌")
	requireAbsentInWorkspace(t, h.Root, token, "webhook 令牌不得落盘到工作区")

	db := h.openDB(t)
	requireToolAudit(t, db, "notify", "success", "", 1)
	audit := queryRows(t, db, `SELECT error_message FROM tool_invocations WHERE tool_name = 'notify'`)
	if message, ok := audit[0]["error_message"].(string); ok {
		requireAbsent(t, message, token, "审计不得回显 webhook 令牌")
	}
	// 投递记录：主键是 tool_invocations.id，状态只在 tool_invocations 上。
	deliveries := queryRows(t, db, `SELECT d.run_id, d.channel_name, d.target_host, i.status, i.error_kind
FROM notify_deliveries d JOIN tool_invocations i ON i.id = d.tool_invocation_id
ORDER BY d.tool_invocation_id`)
	if len(deliveries) != 1 {
		t.Fatalf("notify_deliveries 行数 = %d，期望 1（成功发送必须留一条投递记录）", len(deliveries))
	}
	if got := stringValue(t, deliveries[0], "channel_name"); got != us16ExtNotifyChannel {
		t.Fatalf("notify_deliveries.channel_name = %q，期望 %q", got, us16ExtNotifyChannel)
	}
	if got := stringValue(t, deliveries[0], "status"); got != "success" {
		t.Fatalf("投递终态 = %q（error_kind=%q），期望 success",
			got, stringValue(t, deliveries[0], "error_kind"))
	}
	if got := stringValue(t, deliveries[0], "target_host"); got != parsed.Hostname() {
		t.Fatalf("notify_deliveries.target_host = %q，期望裸主机名 %q", got, parsed.Hostname())
	}
	t.Logf("r5_feishu：target=%s://%s deliveries=%d", parsed.Scheme, parsed.Hostname(), len(deliveries))
}

// ---------------------------------------------------------------------------
// R6 五个内置 Agent 盘点
// ---------------------------------------------------------------------------

// us16ExtAgentInventory 是一只内置 Agent 的七项盘点结果。
// us16ExtAgentInventory is the seven-item inventory row of one built-in agent.
type us16ExtAgentInventory struct {
	Name     string
	Defined  bool
	State    bool
	Deps     bool
	Creds    bool
	Config   bool
	Assets   bool
	Business string
}

// TestUS16External_FiveAgentsInventory 判 R6：新实例首个 `serve` 必须装出**五只**
// 内置 Agent，且每只能给出七项独立盘点；条件不具备的 Agent 记"未验证 + 原因"，
// 不得用其他 Agent 的结果代替，也不得把两只并成一只。
// 逆改点：安装器少装一只、或合并计数 → 名称集合与逐行盘点断言红。
func TestUS16External_FiveAgentsInventory(t *testing.T) {
	// R6 不需要外部凭据：它判的是安装与盘点本身。缺凭据的项在盘点里如实记原因。
	// No external credential is required: this row judges installation and inventory.
	fake := newFakeModel(t)
	h := newHarness(t)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, v3ServeConfig(addr, configWithProvider(fake.URL()), ""))
	// 空工作目录 + 有 basalt.yaml：Fresh 判定只看 basalt.db / instance.json，两者都不存在，
	// 因此 serveInit 会补骨架，安装器随后装好五个 Agent 与免凭据 MCP 条目。
	// 这里**不得**调 initWorkspace：`basalt init` 会建出 basalt.db，实例就不再是 Fresh。
	p := us16ExtStartServe(t, h, fixtureEnv(), addr)
	defer p.stop(t)

	want := []string{"biz-daily", "docs-diff", "github-radar", "industry-brief", "repo-digest"}
	inventories := make([]us16ExtAgentInventory, 0, len(want))
	for _, name := range want {
		inventories = append(inventories, us16ExtInventoryOf(t, h, name))
	}

	// 逐项判：任何一项 false 都说明该 Agent 的盘点不成立。
	for _, item := range inventories {
		if !item.Defined {
			t.Fatalf("%s：定义缺失（.basalt/agents/%s/AGENT.md 不存在或 name 不符）", item.Name, item.Name)
		}
		if !item.State {
			t.Fatalf("%s：生命周期状态缺失或不是 disabled/1（首个 serve 的 Bootstrap 口径）", item.Name)
		}
		if !item.Deps {
			t.Fatalf("%s：依赖盘点不成立（声明的 MCP 名在 mcp_servers.yaml 里找不到）", item.Name)
		}
		if !item.Config {
			t.Fatalf("%s：配置盘点不成立（provider/model 行缺失）", item.Name)
		}
		if !item.Assets {
			t.Fatalf("%s：业务资产未随包安装", item.Name)
		}
	}

	// 计数纪律：逐行报告，分子只数"业务验证完成"的 Agent。
	completed := 0
	for _, item := range inventories {
		state := "未验证"
		if item.Business != "" {
			completed++
			state = item.Business
		}
		t.Logf("r6_inventory agent=%s defined=%v lifecycle=%v deps=%v creds=%v config=%v assets=%v business=%s",
			item.Name, item.Defined, item.State, item.Deps, item.Creds, item.Config, item.Assets, state)
	}
	t.Logf("r6_summary：五个 Agent 中 %d 个完成业务验证（本用例只做盘点；业务链路见 R1–R5 各自用例）", completed)

	// 安装标记：schema_version=1，五个 Agent 记为 installed，不是 skipped。
	markerPath := filepath.Join(h.Root, ".basalt", "control", "builtin-openbox.json")
	rawMarker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("安装标记不可读（%s）: %v", markerPath, err)
	}
	var marker struct {
		SchemaVersion   int      `json:"schema_version"`
		AgentsInstalled bool     `json:"agents_installed"`
		AgentsSkipped   bool     `json:"agents_skipped"`
		MCPEntries      []string `json:"mcp_entries"`
	}
	if err := json.Unmarshal(rawMarker, &marker); err != nil {
		t.Fatalf("安装标记不是预期 JSON: %v", err)
	}
	if marker.SchemaVersion != 1 {
		t.Fatalf("安装标记 schema_version = %d，期望 1", marker.SchemaVersion)
	}
	if !marker.AgentsInstalled || marker.AgentsSkipped {
		t.Fatalf("安装标记 agents_installed=%v agents_skipped=%v，期望 true / false",
			marker.AgentsInstalled, marker.AgentsSkipped)
	}
	if len(marker.MCPEntries) == 0 {
		t.Fatalf("安装标记没有记录任何 MCP 条目——免凭据的条目应当无条件写入")
	}
}

// us16ExtInventoryOf 读一只内置 Agent 的七项盘点。
// us16ExtInventoryOf reads the seven-item inventory of one built-in agent.
func us16ExtInventoryOf(t *testing.T, h *harness, name string) us16ExtAgentInventory {
	t.Helper()
	item := us16ExtAgentInventory{Name: name}

	agentMD := filepath.Join(h.Root, ".basalt", "agents", name, "AGENT.md")
	if body, err := os.ReadFile(agentMD); err == nil {
		text := string(body)
		item.Defined = strings.Contains(text, "name: "+name)
		item.Deps = us16ExtDepsResolved(h, text)
		item.Config = strings.Contains(text, "provider:") && strings.Contains(text, "model:")
	}

	// 状态：首个 serve 的 Bootstrap 只对缺失的状态文件写 disabled / generation=1。
	// 先 stat 再读：读取器在缺失时直接 Fatalf，而这里要的是逐项盘点。
	statePath := filepath.Join(h.Root, ".basalt", "control", "agents", name+".json")
	if _, err := os.Stat(statePath); err == nil {
		state := readLifecycleStateFile(t, h, name)
		item.State = state.State == "disabled" && state.Generation == 1
	}

	// 凭据与资产按 Agent 的真实依赖判：缺件记 false，由失败分支点名。
	item.Creds = us16ExtCredsResolved(name)
	item.Assets = us16ExtAssetsPresent(t, h, name)
	return item
}

// us16ExtDepsResolved 判声明的 mcp_servers 是否都能解析：名单里的每个名字必须在
// `mcp_servers.yaml` 有对应条目（拼写逐字）。没有 MCP 声明的 Agent 视为可解析。
// us16ExtDepsResolved checks every declared MCP server has a registry entry.
func us16ExtDepsResolved(h *harness, agentMD string) bool {
	declared := us16ExtMCPServers(agentMD)
	if len(declared) == 0 {
		return true
	}
	serversRaw, err := os.ReadFile(filepath.Join(h.Root, ".basalt", "mcp_servers.yaml"))
	if err != nil {
		return false
	}
	servers := string(serversRaw)
	for _, name := range declared {
		if !strings.Contains(servers, "name: "+name) {
			return false
		}
	}
	return true
}

// us16ExtMCPServers 从 frontmatter 里取出 `mcp_servers:` 段的条目名。
// us16ExtMCPServers extracts the declared mcp_servers entries from the frontmatter.
func us16ExtMCPServers(agentMD string) []string {
	var names []string
	inSection := false
	for _, line := range strings.Split(agentMD, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "mcp_servers:") {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			// 顶格出现的另一个键结束本节；空行与更深缩进的续行不结束。
			// A top-level key ends the section; blank and indented lines do not.
			if trimmed != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-") {
				break
			}
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
		if name != "" && !strings.ContainsAny(name, "{}") {
			names = append(names, name)
		}
	}
	return names
}

// us16ExtCredsResolved 判该 Agent 声明的外部凭据是否存在（只看存在性，不读值）。
// us16ExtCredsResolved checks whether the declared external credential exists.
func us16ExtCredsResolved(name string) bool {
	switch name {
	case "github-radar", "repo-digest":
		return strings.TrimSpace(os.Getenv(us16ExtGitHubTokenEnv)) != "" ||
			strings.TrimSpace(os.Getenv(us16ExtGitHubTokenAlias)) != ""
	default:
		// biz-daily / docs-diff / industry-brief 不依赖外部凭据 env。
		return true
	}
}

// us16ExtAssetsPresent 判该 Agent 的业务资产是否随包安装：样例库 / 文档库 / 技能。
// us16ExtAssetsPresent checks the per-agent business assets that ship with the pack.
func us16ExtAssetsPresent(t *testing.T, h *harness, name string) bool {
	t.Helper()
	switch name {
	case "biz-daily":
		_, err := os.Stat(filepath.Join(h.Root, ".basalt", "samples", "biz-sample.db"))
		return err == nil
	case "docs-diff":
		_, err := os.Stat(filepath.Join(h.Root, ".basalt", "samples", "docs"))
		return err == nil
	case "repo-digest":
		_, err := os.Stat(filepath.Join(h.Root, ".basalt", "skills", "repo-digest"))
		return err == nil
	default:
		// github-radar / industry-brief 的资产由运行期产物目录承载，首个 serve 尚未产生。
		return true
	}
}
