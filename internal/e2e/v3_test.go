//go:build e2e

package e2e

// 本文件是 US-12 范围修订（R4）的 v3 端到端判卷（模块卡 §4.4 的 `012-e2e-v3`，T153）：
// 真二进制 + 临时工作区 + 假模型，判三条只有 v3 才成立的产品流程——
//   ① 全新安装直接建出当前基线（v4）；`AGENT.md` 自带两类资源绑定时，通知调用按出网策略
//      拒绝，且拒绝落在工具审计上。
//   ② 托管凭据写入即密文落库 → 跨进程重启仍可调用 → 撤销 → 准入 409 → 重新录入失败
//      保持撤销 → 重新录入成功只追加新版本，旧版本永不复活。
//   ③ 共享转专属的引用闸门（409 点名 + 失败操作行）与副本的资源映射改写。
//
// 装载器每次候选校验读**当前**运行期目录（US-12 R4：发布 / 撤回 / 删除在 `ResourceGate`
// 写锁内原子替换目录），因此运行期发布的模型连接无需重启即可被 Agent 显式绑定——
// `TestV3E2E_RuntimePublishThenBind` 判这条；`TestV3E2E_CredentialLifecycle` 的两次重启
// 只为判「密文 + 工作区外主密钥」跨进程解密，不是为了刷新 Provider 名集。
// The loader reads the current runtime catalog on every validation, so a connection
// published at runtime binds without a restart; the credential test restarts only to
// prove cross-process decryption of the sealed value.
//
// 断言面与 admin_console_test.go 同口径：HTTP 信封、响应头 ETag、工作区字节与 SQLite；
// 负向调用走 adminInvoke / webRequest 以保留状态码。v3 专属字段（凭据面与使用范围）在
// 本文件各有一份判卷副本，不复用只读面的旧形状。
// This file is the v3 end-to-end verdict: fresh v3 installs, managed credentials across
// restarts, and scope conversion plus copy mapping.

import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// v3NotifyEnv 是通知渠道地址占位引用的变量名；真实值只在服务进程环境里。
	// v3NotifyEnv names the placeholder variable; the value lives only in the process env.
	v3NotifyEnv = "BASALT_E2E_NOTIFY"
	// v3NotifyURL 是一个**不是域名**的出网目标：策略拒绝先于外呼，用例不会发起真实外连。
	// v3NotifyURL is not a domain, so the outbound policy refuses before any dial.
	v3NotifyURL = "http://127.0.0.1:1"
)

// v3ModelConnectionView 是 ModelConnectionView 的判卷副本：在 US-12 只读副本上补齐
// 凭据面（模式 / 引用 / 状态 / 版本 / 尾号）与使用范围，撤销与转专属两条流程要用它们。
// v3ModelConnectionView adds the credential and scope fields to the read-only copy.
type v3ModelConnectionView struct {
	Name              string `json:"name"`
	ResourceUID       string `json:"resource_uid"`
	BaseURL           string `json:"base_url"`
	APIKeyEnv         string `json:"api_key_env"`
	DefaultModel      string `json:"default_model"`
	CredentialMode    string `json:"credential_mode"`
	CredentialRef     string `json:"credential_ref"`
	CredentialState   string `json:"credential_state"`
	CredentialVersion int    `json:"credential_version"`
	Last4             string `json:"last4"`
	Scope             string `json:"scope"`
	OwnerAgent        string `json:"owner_agent"`
	Revision          int    `json:"revision"`
	ETag              string `json:"etag"`
}

// v3NotifyChannelView 是 NotifyChannelView 的判卷副本：渠道读面必须带回资源 UID 与
// 凭据段——绑定要写 UID，按名解析是被禁止的；`url_configured` 与 `url_host` 分开，
// `${VAR}` 引用有地址但没有 host 投影。
// v3NotifyChannelView carries the uid, address and credential fields of the read view.
type v3NotifyChannelView struct {
	Name              string `json:"name"`
	ResourceUID       string `json:"resource_uid"`
	Type              string `json:"type"`
	Description       string `json:"description"`
	URLHost           string `json:"url_host"`
	URLConfigured     bool   `json:"url_configured"`
	AddressStatus     string `json:"address_status"`
	CredentialMode    string `json:"credential_mode"`
	CredentialState   string `json:"credential_state"`
	CredentialVersion int    `json:"credential_version"`
	Scope             string `json:"scope"`
	OwnerAgent        string `json:"owner_agent"`
	Revision          int    `json:"revision"`
	ETag              string `json:"etag"`
}

// v3CredentialRevokeView 是 CredentialRevokeView 的判卷副本：ETag 只走响应头。
// v3CredentialRevokeView mirrors the revoke view; its ETag is header-only.
type v3CredentialRevokeView struct {
	ResourceUID     string `json:"resource_uid"`
	CredentialRef   string `json:"credential_ref"`
	CredentialState string `json:"credential_state"`
	RevokedVersions int    `json:"revoked_versions"`
	RevocationEpoch int    `json:"revocation_epoch"`
}

// v3ScopeView 是 ResourceScopeView 的判卷副本（`etag` 同时在响应体与响应头）。
// v3ScopeView mirrors the scope view; the etag appears in body and header.
type v3ScopeView struct {
	ResourceUID            string   `json:"resource_uid"`
	Scope                  string   `json:"scope"`
	OwnerAgent             string   `json:"owner_agent"`
	Revision               int      `json:"revision"`
	ETag                   string   `json:"etag"`
	AffectedEligibleAgents []string `json:"affected_eligible_agents"`
}

// v3ErrorData 是 v3 失败信封 `data` 的判卷子集（白名单投影里的两列）。
// v3ErrorData reads the two whitelisted failure fields these flows assert.
type v3ErrorData struct {
	OperationID *string   `json:"operation_id,omitempty"`
	Problems    *[]string `json:"problems,omitempty"`
}

// v3BindingsBlock 渲染同时带模型连接与通知渠道的 `resource_bindings` 块：服务端不按名
// 补绑，两类绑定必须由提交正文自带（R4.4 严格一一对应）。
// v3BindingsBlock renders a two-kind bindings block; the server never infers bindings.
func v3BindingsBlock(modelName, modelUID, channelName, channelUID string) string {
	return "resource_bindings:\n" +
		"  model_connection:\n" +
		"    - name: " + modelName + "\n" +
		"      uid: " + modelUID + "\n" +
		"  notify_channel:\n" +
		"    - name: " + channelName + "\n" +
		"      uid: " + channelUID + "\n"
}

// v3ProviderBlock 渲染一条 env 模式的 Provider 定义（占位引用，值只来自进程环境）。
// v3ProviderBlock renders one env-mode provider definition.
func v3ProviderBlock(name, baseURL, model string) string {
	return "  - name: " + name + "\n" +
		"    base_url: " + baseURL + "\n" +
		"    api_key: ${" + fixtureKeyEnv + "}\n" +
		"    default_model: " + model + "\n"
}

// v3ServeConfig 组装 `providers:` + `http:` + 管理认证 + 可选 `secrets:` 段。
// v3ServeConfig assembles the service config used by the v3 tests.
func v3ServeConfig(addr, providers, extra string) string {
	return providers + "http:\n  addr: " + addr + "\n" + adminAuthSection() + extra
}

// v3SecretsSection 返回 `secrets:` 段：主密钥文件放在工作区之外，只以 key_id 进库。
// v3SecretsSection points the master key outside the workspace.
func v3SecretsSection(keyPath string) string {
	return "secrets:\n  master_key_file: '" + filepath.ToSlash(keyPath) + "'\n"
}

// v3RewriteAddr 只改配置文件里的监听地址：管理面写回的 Provider 块必须逐字保留，
// 否则重启就变成了"手工删块"，测不到真实的对账路径。
// v3RewriteAddr rewrites only the listen address, preserving the admin-written blocks.
func v3RewriteAddr(t *testing.T, h *harness, oldAddr, newAddr string) {
	t.Helper()
	path := filepath.Join(h.Root, "basalt.yaml")
	config := readText(t, path)
	updated := strings.Replace(config, "addr: "+oldAddr, "addr: "+newAddr, 1)
	if updated == config {
		t.Fatalf("重写配置：未找到 addr: %s", oldAddr)
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("重写配置失败: %v", err)
	}
}

// requireFreshSchema 断言全新安装**直接**建出当前基线（v6）库：schema_version 只有
// 一行且为 6、33 张用户表、且没有 `system.baseline_v3` 迁移操作行（迁移只属于真实的
// v2→v3 升级）。0004 追加两表、0005 追加一张、0006（14D）追加四张，总数与
// internal/storage/migrate_test.go 的 contractTables 同口径。
// requireFreshSchema pins a direct current-baseline install: one version row (6),
// 33 tables, and no baseline_v3 migration op.
func requireFreshSchema(t *testing.T, db *sql.DB, label string) {
	t.Helper()
	versions := queryRows(t, db, `SELECT version FROM schema_version ORDER BY version`)
	if len(versions) != 1 {
		t.Fatalf("%s：schema_version 应只有一行，实际 %d 行：%v", label, len(versions), versions)
	}
	if got, _ := versions[0]["version"].(int64); got != 6 {
		t.Fatalf("%s：schema_version = %v，期望 6（全新安装直接建 v6）", label, versions[0]["version"])
	}
	tables := scalarInt(t, db,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if tables != 33 {
		t.Fatalf("%s：用户表 %d 张，期望 33（v1 十一张 + v2 七张 + v3 八张 + v4 两张 + v5 一张 + v6 四张）", label, tables)
	}
	if got := scalarInt(t, db,
		`SELECT COUNT(*) FROM management_operations WHERE action = 'system.baseline_v3'`); got != 0 {
		t.Fatalf("%s：全新安装不得出现 system.baseline_v3 迁移操作行，实际 %d 行", label, got)
	}
}

// v3ReadModelConnection 读一个模型连接（含凭据面与范围），并断言 ETag 非空。
// v3ReadModelConnection reads one connection with its credential and scope fields.
func v3ReadModelConnection(t *testing.T, addr, name string) v3ModelConnectionView {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/model-connections/"+name, "", nil)
	var view v3ModelConnectionView
	requireWriteOK(t, resp, &view, "读取模型连接 "+name)
	if view.ResourceUID == "" || view.ETag == "" {
		t.Fatalf("读取模型连接 %s：resource_uid = %q、etag = %q，两者都必须非空",
			name, view.ResourceUID, view.ETag)
	}
	return view
}

// v3InsertNotifyChannel 按 v3 形态直接登记一条共享渠道行：主键是资源 UID，`scope`
// 参与资格判定，`operation_id` 是 v3 新增的非空审计引用。地址只存 `${VAR}` 引用。
// v3InsertNotifyChannel registers one shared channel row in the v3 shape, keyed by uid.
func v3InsertNotifyChannel(t *testing.T, db *sql.DB, uid, name, url, description string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(
		`INSERT INTO notify_channels(resource_uid, name, type, url, url_host,
		        credential_mode, scope, owner_agent_uid, revision, description,
		        created_at, updated_at, operation_id)
		 VALUES (?, ?, 'webhook', ?, '', 'env', 'shared', NULL, 1, ?, ?, ?, 'op-e2e-v3-notify')`,
		uid, name, url, description, now, now); err != nil {
		t.Fatalf("登记通知渠道 %s 失败: %v", name, err)
	}
}

// v3ReadNotifyChannel 从渠道列表里读出目标渠道：列表是 R3.6 起的唯一读面。
// v3ReadNotifyChannel reads one channel out of the list view.
func v3ReadNotifyChannel(t *testing.T, addr, name string) v3NotifyChannelView {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/notify-channels", "", nil)
	var channels []v3NotifyChannelView
	requireWriteOK(t, resp, &channels, "读取通知渠道列表")
	for _, channel := range channels {
		if channel.Name == name {
			return channel
		}
	}
	t.Fatalf("通知渠道列表里没有 %q：%+v", name, channels)
	return v3NotifyChannelView{}
}

// v3WaitLifecycleIndex 轮询生命周期索引，直到该 Agent 有可用身份：索引是状态文件的可
// 重建镜像，异步对账，范围转换的属主解析要求它已就绪。
// v3WaitLifecycleIndex waits until the async lifecycle index carries the agent identity.
func v3WaitLifecycleIndex(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last []map[string]any
	for time.Now().Before(deadline) {
		last = queryRows(t, db,
			`SELECT agent_uid FROM agent_lifecycle
			  WHERE agent_name = ? AND valid = 1 AND agent_uid <> ''`, name)
		if len(last) == 1 {
			uid, _ := last[0]["agent_uid"].(string)
			return uid
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 %s 的生命周期索引出现可用身份超时：最后一次读数 %v", name, last)
	return ""
}

// v3WaitBinding 轮询资源绑定投影，直到该 Agent 的指定绑定出现（投影由提交在锁内重建，
// 但仍按"可观察"判卷，不依赖时序）。
// v3WaitBinding waits for one projected binding row to appear.
func v3WaitBinding(t *testing.T, db *sql.DB, agent, kind, refName, uid string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if scalarInt(t, db,
			`SELECT COUNT(*) FROM agent_resource_bindings
			  WHERE agent_name = ? AND resource_kind = ? AND ref_name = ? AND resource_uid = ?`,
			agent, kind, refName, uid) >= 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 %s 绑定 %s/%s（%s）出现超时", agent, kind, refName, uid)
}

// v3WaitBindingGone 轮询直到该 Agent 不再绑定这个资源 UID（解绑后的投影读数）。
// v3WaitBindingGone waits until the projection no longer carries the binding.
func v3WaitBindingGone(t *testing.T, db *sql.DB, agent, kind, uid string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if scalarInt(t, db,
			`SELECT COUNT(*) FROM agent_resource_bindings
			  WHERE agent_name = ? AND resource_kind = ? AND resource_uid = ?`,
			agent, kind, uid) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 %s 解除绑定 %s（%s）超时", agent, kind, uid)
}

// requireContainsAny 断言文本至少包含候选之一：写面渲染 YAML 时按需加引号，判卷要
// 接受同值的两种合法写法，而不是钉死某一种。
// requireContainsAny accepts either render form of the same scalar.
func requireContainsAny(t *testing.T, text string, candidates []string, label string) {
	t.Helper()
	for _, candidate := range candidates {
		if strings.Contains(text, candidate) {
			return
		}
	}
	t.Fatalf("%s：输出缺少任何一种形态 %q\n%s", label, candidates, text)
}

// v3HasProblem 判断 problems 列表里是否有逐字相等的一项。
// v3HasProblem reports whether problems carries the exact entry.
func v3HasProblem(problems []string, want string) bool {
	for _, item := range problems {
		if item == want {
			return true
		}
	}
	return false
}

// TestV3E2E_MigrateAdmitNotify 覆盖全新安装的 v3 库与"绑定即准入权威"的通知路径：
// 手工提交的 AGENT.md 自带模型连接与通知渠道绑定，通知工具读到的渠道地址不是域名，
// 失败下沉到出网策略并留一条 failed + sandbox_denied 审计；运行本身继续到完成。
func TestV3E2E_MigrateAdmitNotify(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	db := openDB(t, h.dbPath())
	requireFreshSchema(t, db, "init 之后")

	model := newFakeModel(t,
		toolCallResponse("call-v3-notify", "notify", mustJSON(map[string]string{
			"channel": "ops", "content": "v3 通知演示",
		})),
		textResponse("v3 通知演示完成"),
	)
	addr := freeLoopbackAddr(t)
	providers := "providers:\n" + v3ProviderBlock("aux", model.URL(), fixtureModel)
	h.writeConfig(t, v3ServeConfig(addr, providers, ""))
	startServeWithEnv(t, h, addr, append(fixtureEnv(), v3NotifyEnv+"="+v3NotifyURL))
	requireFreshSchema(t, db, "serve 之后")

	// aux 先于 serve 出现在配置文件里：启动对账发布它，注册表的 Provider 名集因此包含它。
	aux := v3ReadModelConnection(t, addr, "aux")
	if aux.Scope != "shared" || aux.OwnerAgent != "" {
		t.Fatalf("启动发布的模型连接范围 = %q / 属主 %q，期望 shared / 空",
			aux.Scope, aux.OwnerAgent)
	}

	var created adminFilesView
	resp := adminWrite(t, addr, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":        "notice",
		"description": "v3 通知绑定演示",
		"model":       fixtureModel,
	}, nil)
	requireWriteOK(t, resp, &created, "创建 Agent")
	requireAdminState(t, created.Lifecycle, "disabled", "创建 Agent")
	if state := readLifecycleStateFile(t, h, "notice"); state.State != "disabled" || state.Generation != 1 {
		t.Fatalf("创建后状态文件：state=%q generation=%d，期望 disabled/1", state.State, state.Generation)
	}
	waitAgentRegistered(t, addr, "notice")
	waitAgentEnabled(t, addr, "notice", 1)

	// 渠道行按 v3 形态**直接登记**：管理写面在创建时预跑出网白名单，写后不可达的渠道
	// 根本登记不进去；而本用例要判的正是「发送时的策略拒绝」。因此这里落一条运维预先
	// 登记的遗留行（与 notify_test 的手工登记同源），管理写面的创建 / 更新 / 删除由
	// TestAdminConsoleE2E_WorkspaceAndNotify 覆盖。
	// The row is pre-registered on purpose: the admin write surface pre-runs the outbound
	// allowlist, so an unreachable channel can never be created through it. This test
	// judges the send-time denial instead.
	const channelUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3E"
	v3InsertNotifyChannel(t, db, channelUID, "ops", "${"+v3NotifyEnv+"}", "v3 通知渠道")
	channel := v3ReadNotifyChannel(t, addr, "ops")
	if channel.ResourceUID != channelUID || channel.CredentialMode != "env" {
		t.Fatalf("通知渠道读数：uid = %q、credential_mode = %q，期望 %q / env",
			channel.ResourceUID, channel.CredentialMode, channelUID)
	}
	if !channel.URLConfigured || channel.URLHost != "" || channel.Scope != "shared" {
		t.Fatalf("通知渠道读数：url_configured=%v、url_host=%q、scope=%q，"+
			"期望 true / 空串 / shared（占位有引用、没有 host 投影）",
			channel.URLConfigured, channel.URLHost, channel.Scope)
	}

	before := adminAgentFiles(t, addr, "notice")
	declared := agentDoc(
		"name: notice\n"+
			"description: v3 通知绑定演示\n"+
			"provider:\n  name: aux\n  model: "+fixtureModel+"\n"+
			"tools:\n  - notify\n"+
			"notify_mode: allowlist\n"+
			"notify_channels:\n  - ops\n"+
			v3BindingsBlock("aux", aux.ResourceUID, "ops", channel.ResourceUID),
		"v3 通知绑定演示")
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents/notice/files", map[string]any{
		"files": []map[string]string{{"path": "AGENT.md", "content": declared}},
	}, map[string]string{"If-Match": before.ETag})
	requireWriteOK(t, resp, nil, "提交 AGENT.md（两类绑定）")

	// 引用视图（注册表快照）看见新绑定时，提交后的重扫才真正生效——监听器有 300ms
	// 去抖，提前调用会读到旧定义。等待信号必须选**只有提交后才会出现**的那条：
	// 模板创建时已把唯一已发布 Provider 写进 provider 段，`model_connection/aux`
	// 的引用在提交前就成立，等它会把旧定义（工具集仍是模板默认值）当成生效结果；
	// 通知渠道又不进资源引用端点（只收 model_connection / mcp_server / skill）。
	// 因此按 `/profiles` 的声明投影等 `tools` 与 `notify_channels`——两者都只有新
	// 定义才写得出来。
	// The wait must key on a signal only the submitted file can produce: the template
	// already binds the sole published provider, and notify channels are outside the
	// resource-reference endpoint. Poll the `/profiles` declaration projection instead.
	waitProfile(t, addr, "notice", "定义重载后 tools 含 notify", func(row adminProfileRow, ok bool) bool {
		return ok && row.Registered && containsString(row.Tools, "notify") &&
			containsString(row.NotifyChannels, "ops")
	})
	modelRefs := waitResourceReferences(t, addr, "model_connection", "aux", "引用视图看见 notice",
		func(view adminReferencesView) bool {
			return len(view.Agents) == 1 && view.Agents[0] == "notice"
		})
	if modelRefs.ResourceUID != aux.ResourceUID {
		t.Fatalf("模型连接引用视图 uid = %q，期望 %q", modelRefs.ResourceUID, aux.ResourceUID)
	}

	reading := invokeAgent(t, addr, "notice", "请发通知")
	if reading.Status != "completed" {
		t.Fatalf("通知调用状态 = %q，期望 completed（原因 %q）", reading.Status, reading.Reason)
	}
	if reading.FinalText != "v3 通知演示完成" {
		t.Fatalf("通知调用回复 = %q", reading.FinalText)
	}
	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("一次通知调用应产生 2 次模型请求，实际 %d 次", len(reqs))
	}
	// 策略文案只带 scheme + host（端口被略去），与实际地址字面量不同。
	// The policy refusal renders scheme + host only, dropping the port.
	requireToolResultContains(t, reqs[1], "出网目标不是域名：http://127.0.0.1",
		"绑定解析出的渠道地址被出网策略拒绝")
	requireToolAudit(t, db, "notify", "failed", "sandbox_denied", 1)
}

// TestV3E2E_CredentialLifecycle 覆盖托管凭据的完整生命周期：写入即密文落库（明文零落盘）
// → 两次跨进程重启都能解密调用 → 撤销后准入 409 → 重新录入失败保持撤销 → 重新录入成功
// 只追加新版本；旧版本状态仍是 revoked 且密文为空，撤销纪元不回退。
func TestV3E2E_CredentialLifecycle(t *testing.T) {
	const (
		agentName = "cred-user"
		managed   = "aaa-managed"
		firstKey  = "sk-e2e-managed-first-0001"
		rotated   = "sk-e2e-managed-rotated-0002"
	)
	h := newHarness(t)
	initWorkspace(t, h)
	db := openDB(t, h.dbPath())
	keyPath := filepath.Join(t.TempDir(), "master.key")
	tightenMasterKeyDirACL(t, filepath.Dir(keyPath))
	model := newFakeModel(t,
		textResponse("托管凭据第一次调用"),
		textResponse("重启后仍能调用"),
		textResponse("重新录入后第三次调用"),
	)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, v3ServeConfig(addr, "", v3SecretsSection(keyPath)))

	res := h.run(t, fixtureEnv(), "secrets", "init")
	requireExit(t, res, 0, "生成主密钥")
	requireContains(t, res.Stdout, "主密钥已生成：key_id=", "生成主密钥")

	p := startServe(t, h, addr)

	// managed 创建：明文只进密文，响应只回模式 / 引用 / 状态 / 版本 / 尾号。
	var created v3ModelConnectionView
	resp := adminWrite(t, addr, http.MethodPost, "/api/v1/model-connections", map[string]any{
		"name":            managed,
		"base_url":        model.URL(),
		"api_key":         firstKey,
		"default_model":   fixtureModel,
		"credential_mode": "managed",
	}, nil)
	requireWriteOK(t, resp, &created, "创建托管模型连接")
	if created.CredentialMode != "managed" || created.CredentialState != "active" {
		t.Fatalf("创建托管模型连接：credential_mode = %q、credential_state = %q，期望 managed / active",
			created.CredentialMode, created.CredentialState)
	}
	if created.CredentialVersion != 1 || created.Last4 != "0001" {
		t.Fatalf("创建托管模型连接：credential_version = %d、last4 = %q，期望 1 / 0001",
			created.CredentialVersion, created.Last4)
	}
	if !strings.HasPrefix(created.CredentialRef, "cred_") || created.APIKeyEnv != "" {
		t.Fatalf("创建托管模型连接：credential_ref = %q、api_key_env = %q，期望 cred_ 前缀与空占位",
			created.CredentialRef, created.APIKeyEnv)
	}
	requireAbsent(t, resp.Raw, firstKey, "创建响应不得回显托管凭据明文")

	// 第一次重启：「密文 + 工作区外主密钥」跨进程解密的第一次判卷（运行期目录随发布
	// 立即生效，重启不是刷新 Provider 名集所必需——那条路径见 RuntimePublishThenBind）。
	// The first restart proves the sealed value survives a process boundary; refreshing
	// the provider name set no longer needs one.
	p.kill()
	p.waitExit(t, 30*time.Second)
	addr2 := freeLoopbackAddr(t)
	v3RewriteAddr(t, h, addr, addr2)
	p2 := startServe(t, h, addr2)

	var agentView adminFilesView
	resp = adminWrite(t, addr2, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":        agentName,
		"description": "托管凭据演示",
		"model":       fixtureModel,
	}, nil)
	requireWriteOK(t, resp, &agentView, "创建 Agent")
	requireContainsAny(t, readText(t, filepath.Join(h.Root, ".basalt", "agents", agentName, "AGENT.md")),
		[]string{"name: " + managed, "name: \"" + managed + "\""},
		"模板必须绑定唯一的已发布 Provider")
	waitAgentRegistered(t, addr2, agentName)
	waitAgentEnabled(t, addr2, agentName, 1)

	first := invokeAgent(t, addr2, agentName, "第一次")
	if first.Status != "completed" || first.FinalText != "托管凭据第一次调用" {
		t.Fatalf("首次调用 = %q / 回复 %q，期望 completed / 托管凭据第一次调用",
			first.Status, first.FinalText)
	}

	// 第二次重启：定义文件里只有 `credential_mode: managed` 与只读引用，启动对账照常通过。
	// The second restart re-reads the managed definition with no plaintext in the file.
	p2.kill()
	p2.waitExit(t, 30*time.Second)
	addr3 := freeLoopbackAddr(t)
	v3RewriteAddr(t, h, addr2, addr3)
	p3 := startServe(t, h, addr3)

	second := invokeAgent(t, addr3, agentName, "重启后")
	if second.Status != "completed" || second.FinalText != "重启后仍能调用" {
		t.Fatalf("重启后调用 = %q / 回复 %q，期望 completed / 重启后仍能调用",
			second.Status, second.FinalText)
	}

	before := v3ReadModelConnection(t, addr3, managed)
	if before.CredentialState != "active" || before.CredentialVersion != 1 {
		t.Fatalf("重启后凭据读数 = %q / v%d，期望 active / v1",
			before.CredentialState, before.CredentialVersion)
	}
	resp = adminWrite(t, addr3, http.MethodPost,
		"/api/v1/resources/model_connection/"+managed+"/credential/revoke",
		map[string]any{"resource_uid": before.ResourceUID, "confirm": true},
		map[string]string{"If-Match": before.ETag})
	var revoked v3CredentialRevokeView
	requireWriteOK(t, resp, &revoked, "撤销托管凭据")
	revokedETag := resp.Header.Get("ETag")
	if revokedETag == "" || revokedETag == before.ETag {
		t.Fatalf("撤销响应头 ETag = %q，期望非空且不同于撤销前 %q", revokedETag, before.ETag)
	}
	if revoked.CredentialState != "revoked" || revoked.RevokedVersions != 1 ||
		revoked.RevocationEpoch != 1 || revoked.CredentialRef != before.CredentialRef {
		t.Fatalf("撤销结果 = %+v，期望 revoked / 1 版 / 纪元 1 / 同一凭据身份", revoked)
	}

	// 准入失败关闭：绑定的凭据已撤销 → 409 + 固定文案，模型不会被调用。
	resp = adminInvoke(t, addr3, agentName, "撤销后")
	requireWebStatus(t, resp, http.StatusConflict, "CREDENTIAL_REVOKED",
		"绑定的凭据已撤销，请重新录入新值", "撤销后调用")

	// 重新录入失败（空值）：零写入，撤销状态与 ETag 都不变，调用继续被拒。
	resp = adminWrite(t, addr3, http.MethodPut, "/api/v1/model-connections/"+managed, map[string]any{
		"base_url":      model.URL(),
		"default_model": fixtureModel,
		"api_key":       "",
	}, map[string]string{"If-Match": revokedETag})
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST",
		"模型连接 "+managed+": 凭据值不得为空；保留请省略字段", "重新录入失败")
	stillRevoked := v3ReadModelConnection(t, addr3, managed)
	if stillRevoked.CredentialState != "revoked" || stillRevoked.CredentialVersion != 1 ||
		stillRevoked.ETag != revokedETag {
		t.Fatalf("失败的重新录入改动了状态：state = %q、version = %d、etag = %q，期望 revoked / 1 / %q",
			stillRevoked.CredentialState, stillRevoked.CredentialVersion, stillRevoked.ETag, revokedETag)
	}
	resp = adminInvoke(t, addr3, agentName, "重新录入失败后")
	requireWebStatus(t, resp, http.StatusConflict, "CREDENTIAL_REVOKED",
		"绑定的凭据已撤销，请重新录入新值", "重新录入失败后调用")

	// 重新录入成功：同一身份追加第 2 版并迁回 active，纪元与旧版本都不回退。
	// Re-entry appends version 2 on the same identity; the epoch never rolls back.
	var reentered v3ModelConnectionView
	resp = adminWrite(t, addr3, http.MethodPut, "/api/v1/model-connections/"+managed, map[string]any{
		"base_url":      model.URL(),
		"default_model": fixtureModel,
		"api_key":       rotated,
	}, map[string]string{"If-Match": revokedETag})
	requireWriteOK(t, resp, &reentered, "重新录入凭据")
	if reentered.CredentialState != "active" || reentered.CredentialVersion != 2 {
		t.Fatalf("重新录入：credential_state = %q、credential_version = %d，期望 active / 2",
			reentered.CredentialState, reentered.CredentialVersion)
	}
	if reentered.CredentialRef != before.CredentialRef || reentered.Last4 != "0002" {
		t.Fatalf("重新录入：credential_ref = %q、last4 = %q，期望同一身份与 0002",
			reentered.CredentialRef, reentered.Last4)
	}
	if reentered.ETag == revokedETag {
		t.Fatalf("重新录入后 ETag 未变化：%q", reentered.ETag)
	}
	requireAbsent(t, resp.Raw, rotated, "重新录入响应不得回显明文")

	third := invokeAgent(t, addr3, agentName, "重新录入后")
	if third.Status != "completed" || third.FinalText != "重新录入后第三次调用" {
		t.Fatalf("重新录入后调用 = %q / 回复 %q，期望 completed / 重新录入后第三次调用",
			third.Status, third.FinalText)
	}

	// 库面判卷：身份同一、版本逐条、旧版本永不复活、撤销纪元只增不减。
	identity := queryRows(t, db,
		`SELECT credential_id, state, current_version, revocation_epoch
		   FROM credentials WHERE owner_kind = 'model_connection' AND owner_uid = ?`,
		before.ResourceUID)
	if len(identity) != 1 {
		t.Fatalf("凭据身份应恰有 1 行，实际 %d 行：%v", len(identity), identity)
	}
	row := identity[0]
	if got, _ := row["credential_id"].(string); got != before.CredentialRef {
		t.Fatalf("凭据身份 = %q，期望 %q（重新录入不换身份）", got, before.CredentialRef)
	}
	if got, _ := row["state"].(string); got != "active" {
		t.Fatalf("凭据身份状态 = %q，期望 active", got)
	}
	if got, _ := row["current_version"].(int64); got != 2 {
		t.Fatalf("凭据当前版本 = %v，期望 2", row["current_version"])
	}
	if got, _ := row["revocation_epoch"].(int64); got != 1 {
		t.Fatalf("撤销纪元 = %v，期望 1（重新录入不重置、不回退）", row["revocation_epoch"])
	}

	versions := queryRows(t, db,
		`SELECT version_no, state, length(nonce) AS nonce_len,
		        (ciphertext IS NULL) AS ciphertext_absent, last4
		   FROM credential_versions WHERE credential_id = ? ORDER BY version_no`,
		before.CredentialRef)
	if len(versions) != 2 {
		t.Fatalf("凭据版本应有 2 行，实际 %d 行：%v", len(versions), versions)
	}
	old := versions[0]
	if got, _ := old["version_no"].(int64); got != 1 {
		t.Fatalf("第 1 行版本号 = %v，期望 1", old["version_no"])
	}
	if got, _ := old["state"].(string); got != "revoked" {
		t.Fatalf("被撤销版本状态 = %q，期望 revoked（旧版本永不复活）", got)
	}
	if got, _ := old["ciphertext_absent"].(int64); got != 1 {
		t.Fatalf("被撤销版本密文应为 NULL，实际 %v", old["ciphertext_absent"])
	}
	current := versions[1]
	if got, _ := current["version_no"].(int64); got != 2 {
		t.Fatalf("第 2 行版本号 = %v，期望 2", current["version_no"])
	}
	if got, _ := current["state"].(string); got != "active" {
		t.Fatalf("新版本状态 = %q，期望 active", got)
	}
	if got, _ := current["ciphertext_absent"].(int64); got != 0 {
		t.Fatalf("新版本必须带密文，实际 ciphertext_absent = %v", current["ciphertext_absent"])
	}
	if got, _ := current["nonce_len"].(int64); got != 12 {
		t.Fatalf("新版本 nonce 长度 = %v，期望 12 字节", current["nonce_len"])
	}
	if got, _ := current["last4"].(string); got != "0002" {
		t.Fatalf("新版本 last4 = %q，期望 0002", got)
	}

	// 明文零落盘：进程停稳后再扫工作区——运行期工作区锁文件被服务持有，读不了就不算证据。
	// The plaintext-on-disk check runs after the server stops so the workspace lock is free.
	p3.kill()
	p3.waitExit(t, 30*time.Second)
	requireAbsentInWorkspace(t, h.Root, firstKey, "托管凭据明文不得落盘")
	requireAbsentInWorkspace(t, h.Root, rotated, "重新录入的明文不得落盘")
}

// TestV3E2E_ScopeAndCopyMapping 覆盖共享资源转专属的引用闸门与副本的资源映射：
// 仍被其他 Agent 绑定时 409 点名并留一条失败操作行；解绑后转换成功；复制遇源专属
// 绑定必须显式映射，缺映射零副本零操作行，给映射后副本改写 provider 与绑定 UID。
func TestV3E2E_ScopeAndCopyMapping(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	model := newFakeModel(t, textResponse("不应被调用"))
	addr := freeLoopbackAddr(t)
	providers := "providers:\n" +
		v3ProviderBlock("aux", "https://api.example.com/v1", "m-aux") +
		v3ProviderBlock(fixtureProvider, model.URL(), fixtureModel)
	h.writeConfig(t, v3ServeConfig(addr, providers, ""))
	startServe(t, h, addr)

	db := openDB(t, h.dbPath())

	// aux 是已发布列表里字典序最靠前的一条：模板创建会绑定它（顺序固定，不遍历），
	// 所以两个 Agent 天然共享同一条连接，范围闸门才有占用者。
	aux := v3ReadModelConnection(t, addr, "aux")
	if aux.Scope != "shared" || aux.OwnerAgent != "" || aux.Revision < 1 {
		t.Fatalf("启动发布的模型连接 = %q / 属主 %q / 修订 %d，期望 shared / 空 / ≥1",
			aux.Scope, aux.OwnerAgent, aux.Revision)
	}

	// 创建响应的范围默认值单独判一次：新建资源必须是 shared，且没有属主。
	var extra v3ModelConnectionView
	resp := adminWrite(t, addr, http.MethodPost, "/api/v1/model-connections", map[string]any{
		"name":          "zzz-extra",
		"base_url":      "https://api.example.com/v1",
		"api_key":       "${" + fixtureKeyEnv + "}",
		"default_model": "m-extra",
	}, nil)
	requireWriteOK(t, resp, &extra, "创建共享模型连接 zzz-extra")
	if extra.Scope != "shared" || extra.OwnerAgent != "" {
		t.Fatalf("新建模型连接范围 = %q / 属主 %q，期望 shared / 空", extra.Scope, extra.OwnerAgent)
	}

	for _, name := range []string{"holder", "other"} {
		var created adminFilesView
		resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":        name,
			"description": "范围与复制演示",
			"model":       fixtureModel,
		}, nil)
		requireWriteOK(t, resp, &created, "创建 Agent "+name)
		waitAgentRegistered(t, addr, name)
	}
	holderUID := v3WaitLifecycleIndex(t, db, "holder")
	otherUID := v3WaitLifecycleIndex(t, db, "other")
	if state := readLifecycleStateFile(t, h, "other"); state.AgentUID != otherUID {
		t.Fatalf("生命周期索引 %q 与状态文件身份 %q 不一致", otherUID, state.AgentUID)
	}
	v3WaitBinding(t, db, "holder", "model_connection", "aux", aux.ResourceUID)
	v3WaitBinding(t, db, "other", "model_connection", "aux", aux.ResourceUID)

	// other 仍绑定 → 转专属被拒绝：409 点名占用者，并留一条 failed 操作行。
	before := v3ReadModelConnection(t, addr, "aux")
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/resources/model_connection/aux/scope",
		map[string]any{
			"resource_uid": before.ResourceUID,
			"scope":        "exclusive",
			"owner_agent":  "holder",
		}, map[string]string{"If-Match": before.ETag})
	requireWebStatus(t, resp, http.StatusConflict, "RESOURCE_SCOPE_IN_USE", "", "仍被 other 绑定时转专属")
	if !strings.Contains(resp.Env.Message, "model_connection/aux 转专属被拒绝") ||
		!strings.Contains(resp.Env.Message, otherUID) {
		t.Fatalf("范围闸门文案 = %q，期望点名 model_connection/aux 与占用者 %s",
			resp.Env.Message, otherUID)
	}
	var blocked v3ErrorData
	decodeWebData(t, resp, &blocked, "仍被 other 绑定时转专属")
	if blocked.OperationID == nil || *blocked.OperationID == "" {
		t.Fatalf("范围闸门失败必须带 operation_id，实际 %+v", blocked)
	}
	if got := scalarInt(t, db,
		`SELECT COUNT(*) FROM management_operations
		  WHERE operation_id = ? AND action = 'model_connection.scope' AND status = 'failed'`,
		*blocked.OperationID); got != 1 {
		t.Fatalf("被拒的范围转换应留 1 条 failed 操作行，实际 %d 行", got)
	}
	if after := v3ReadModelConnection(t, addr, "aux"); after.Scope != "shared" || after.ETag != before.ETag {
		t.Fatalf("被拒后资源读数 = %q / etag %q，期望仍为 shared / %q",
			after.Scope, after.ETag, before.ETag)
	}

	// 把 other 改绑到共享的 fake：UID 必须自带给定（服务端不按名补绑）。
	fake := adminModelConnection(t, addr, fixtureProvider)
	otherFiles := adminAgentFiles(t, addr, "other")
	rebound := agentDoc(
		agentFrontmatter("other", nil)+bindingsBlock("model_connection", fixtureProvider, fake.ResourceUID),
		"改绑共享 Provider")
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents/other/files", map[string]any{
		"files": []map[string]string{{"path": "AGENT.md", "content": rebound}},
	}, map[string]string{"If-Match": otherFiles.ETag})
	requireWriteOK(t, resp, nil, "把 other 改绑到 fake")
	v3WaitBindingGone(t, db, "other", "model_connection", aux.ResourceUID)
	waitResourceReferences(t, addr, "model_connection", "aux", "引用视图不再列出 other",
		func(view adminReferencesView) bool {
			for _, agent := range view.Agents {
				if agent == "other" {
					return false
				}
			}
			return true
		})

	// 引用清空后转专属成功：范围、属主与 ETag 同时更新。
	before = v3ReadModelConnection(t, addr, "aux")
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/resources/model_connection/aux/scope",
		map[string]any{
			"resource_uid": before.ResourceUID,
			"scope":        "exclusive",
			"owner_agent":  "holder",
		}, map[string]string{"If-Match": before.ETag})
	var converted v3ScopeView
	requireWriteOK(t, resp, &converted, "解绑后转专属")
	if converted.ResourceUID != before.ResourceUID || converted.Scope != "exclusive" ||
		converted.OwnerAgent != "holder" || converted.ETag == "" {
		t.Fatalf("转专属结果 = %+v，期望 exclusive / holder / 非空 ETag", converted)
	}
	if got := resp.Header.Get("ETag"); got != converted.ETag {
		t.Fatalf("转专属：响应头 ETag = %q，data.etag = %q", got, converted.ETag)
	}
	if converted.AffectedEligibleAgents == nil {
		t.Fatalf("转专属：affected_eligible_agents 必须是数组（空集合也要 []）")
	}
	if after := v3ReadModelConnection(t, addr, "aux"); after.Scope != "exclusive" ||
		after.OwnerAgent != "holder" {
		t.Fatalf("转专属后资源读数 = %q / %q，期望 exclusive / holder", after.Scope, after.OwnerAgent)
	}
	if holderUID == "" {
		t.Fatal("holder 的稳定身份不应为空")
	}

	// 复制 holder：源专属的 model_connection aux 没有映射 → 409 + problems，零副本零操作行。
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents/holder/copy", map[string]any{
		"new_name": "clone",
	}, nil)
	requireWebStatus(t, resp, http.StatusConflict, "AGENT_COPY_BLOCKED", "", "未给映射的复制")
	var copyBlocked v3ErrorData
	decodeWebData(t, resp, &copyBlocked, "未给映射的复制")
	if copyBlocked.Problems == nil || !v3HasProblem(*copyBlocked.Problems, "model_connection aux") {
		t.Fatalf("复制闸门的 problems = %v，期望包含 \"model_connection aux\"", copyBlocked.Problems)
	}
	requireNoPath(t, filepath.Join(h.Root, ".basalt", "agents", "clone"), "复制被拒")
	if got := scalarInt(t, db,
		`SELECT COUNT(*) FROM management_operations WHERE action = 'agent.copy' AND target_id = 'clone'`); got != 0 {
		t.Fatalf("复制被拒不得留操作行，实际 %d 行", got)
	}

	// 给映射后复制成功：副本 provider 与绑定都改写到目标 UID，不携带源专属 UID。
	var clone adminFilesView
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents/holder/copy", map[string]any{
		"new_name": "clone",
		"resource_mapping": []map[string]string{{
			"resource_kind":       "model_connection",
			"source_resource_uid": aux.ResourceUID,
			"target_resource_uid": fake.ResourceUID,
		}},
	}, nil)
	requireWriteOK(t, resp, &clone, "带映射的复制")
	requireAdminState(t, clone.Lifecycle, "disabled", "复制 Agent")
	if state := readLifecycleStateFile(t, h, "clone"); state.State != "disabled" || state.Generation != 1 {
		t.Fatalf("复制后状态文件：state=%q generation=%d，期望 disabled/1", state.State, state.Generation)
	}
	cloneDoc := readText(t, filepath.Join(h.Root, ".basalt", "agents", "clone", "AGENT.md"))
	requireContainsAny(t, cloneDoc, []string{
		"name: " + fixtureProvider, "name: \"" + fixtureProvider + "\"",
	}, "副本必须改写 provider.name")
	requireContainsAny(t, cloneDoc, []string{
		"uid: " + fake.ResourceUID, "uid: \"" + fake.ResourceUID + "\"",
	}, "副本绑定必须是目标 UID")
	requireAbsent(t, cloneDoc, aux.ResourceUID, "副本不得携带源专属资源 UID")
	waitAgentRegistered(t, addr, "clone")
}

// TestV3E2E_RuntimePublishThenBind 判「管理台三分钟上新」的关键闭环：运行期发布一条
// 托管凭据的模型连接后，已注册的 Agent 立即能显式绑定它（名称 + UID），保存即重载，
// 启用后 Run 走新连接。回归点：装载器的 Provider 名集曾按构造期快照冻结，提交绑定
// 直接 400 `provider not registered`——本用例在修复前必红（2026-10-23 浏览器走查复现）。
// The page flow "publish a connection, then bind it" must work without a restart.
func TestV3E2E_RuntimePublishThenBind(t *testing.T) {
	const (
		agentName  = "late-binder"
		connection = "zzz-late-conn"
		managedKey = "sk-e2e-late-conn-0001"
	)
	h := newHarness(t)
	initWorkspace(t, h)
	db := openDB(t, h.dbPath())
	keyPath := filepath.Join(t.TempDir(), "master.key")
	tightenMasterKeyDirACL(t, filepath.Dir(keyPath))
	model := newFakeModel(t, textResponse("运行期发布后绑定成功"))
	addr := freeLoopbackAddr(t)
	providers := "providers:\n" + v3ProviderBlock(fixtureProvider, model.URL(), fixtureModel)
	h.writeConfig(t, v3ServeConfig(addr, providers, v3SecretsSection(keyPath)))

	res := h.run(t, fixtureEnv(), "secrets", "init")
	requireExit(t, res, 0, "生成主密钥")
	startServe(t, h, addr)

	// Agent 先于新连接存在：模板绑定启动时唯一的已发布 Provider。
	var created adminFilesView
	resp := adminWrite(t, addr, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":        agentName,
		"description": "运行期发布后改绑",
		"model":       fixtureModel,
	}, nil)
	requireWriteOK(t, resp, &created, "创建 Agent "+agentName)
	waitAgentRegistered(t, addr, agentName)

	// 运行期发布一条托管凭据的模型连接：不重启，直接进已发布目录。
	var connectionView v3ModelConnectionView
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/model-connections", map[string]any{
		"name":            connection,
		"base_url":        model.URL(),
		"api_key":         managedKey,
		"default_model":   fixtureModel,
		"credential_mode": "managed",
	}, nil)
	requireWriteOK(t, resp, &connectionView, "运行期发布模型连接")
	if connectionView.CredentialMode != "managed" || connectionView.ResourceUID == "" {
		t.Fatalf("运行期发布模型连接 = %+v，期望 managed 与非空 UID", connectionView)
	}
	requireAbsent(t, resp.Raw, managedKey, "创建响应不得回显托管凭据明文")

	// 显式绑定：页面提交名称 + UID，服务端在写边界内重新校验。修复前这里回
	// 400 BAD_REQUEST「provider not registered: zzz-late-conn」（规则 2 读的是启动快照）。
	files := adminAgentFiles(t, addr, agentName)
	rebound := agentDoc(
		"name: "+agentName+"\n"+
			"description: 运行期发布后改绑\n"+
			"provider:\n  name: "+connection+"\n  model: "+fixtureModel+"\n"+
			"tools: []\n"+
			bindingsBlock("model_connection", connection, connectionView.ResourceUID),
		"绑定运行期发布的连接")
	resp = adminWrite(t, addr, http.MethodPost, "/api/v1/agents/"+agentName+"/files", map[string]any{
		"files": []map[string]string{{"path": "AGENT.md", "content": rebound}},
	}, map[string]string{"If-Match": files.ETag})
	requireWriteOK(t, resp, nil, "绑定运行期发布的连接")
	waitAgentRegistered(t, addr, agentName)
	v3WaitBinding(t, db, agentName, "model_connection", connection, connectionView.ResourceUID)

	// 引用视图来自注册表：新连接被点名为该 Agent 的绑定。
	waitResourceReferences(t, addr, "model_connection", connection, "引用视图列出新绑定",
		func(view adminReferencesView) bool {
			for _, agent := range view.Agents {
				if agent == agentName {
					return true
				}
			}
			return false
		})

	// 启用后跑一次：Run 走运行期发布的连接（托管凭据在进程内解密）。
	// 期望代际取状态文件（/files 视图的 lifecycle 投影不带 generation）。
	// The expected generation comes from the state file, not the /files projection.
	generation := readLifecycleStateFile(t, h, agentName).Generation
	waitAgentEnabled(t, addr, agentName, generation)
	reading := invokeAgent(t, addr, agentName, "报个到")
	if reading.Status != "completed" || reading.FinalText != "运行期发布后绑定成功" {
		t.Fatalf("运行期发布后调用 = %q / 回复 %q，期望 completed / 运行期发布后绑定成功",
			reading.Status, reading.FinalText)
	}
}
