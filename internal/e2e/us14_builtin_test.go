//go:build e2e

package e2e

// 本文件回填 14D（开箱即用增强）冻结测试里的十个（T116）。判据与模块卡 §4.3 的
// `builtin-agent` 检查项逐条对应：
//
//	E1  AgentsDisabledAfterInstall   装好但等配置：五只内置 Agent 一律未注册、生命周期 disabled / 1
//	E2  MCPAutoPublishedOnCredential 凭据就绪才补写 github 条目，本次启动即发布；已有定义零改写
//	E3  StartupIsolation             坏 MCP 条目只拒载引用它的那只 Agent，不拖垮启动
//	E5  DisabledNotScheduled         停用的 Agent 不排期（零执行行、零 Run）；启用后同进程到点即跑
//	E6  EnableBlockedWithoutConfig   缺模型 / 缺 MCP 时启用 409 并点名；就绪项与跳转与 read面同源
//	E7  FreshInstanceBudgetPreset    新实例生成最小配置与名册；已有配置一字节不改
//	E8  BudgetCompatNotices          预算 / 并发只提示不改配置（点明两边数字）
//	E9  ConfigEffectiveThisStart     本次启动生成的配置当次即生效（不需要重启）
//	E10 ExistingNoConfigUntouched    存量实例缺配置：拒启动、零写入、不猜旧路径
//
// 全部用例都跑真实二进制（TestMain 构建一次），不 import internal 包，判据只用管理面
// 契约与库内读数。**没有** `t.Parallel`：E7 / E9 / E10 判的是新实例配置模板里写死的
// `http.addr: 127.0.0.1:8080`（该键没有环境变量覆盖），并发会互相抢端口。
//
// This file backfills all ten of 14D's frozen tests; E4 reuses the demo scenarios.
// Every case runs the real binary and never runs in parallel, because three of them
// need 127.0.0.1:8080 — the address the new-instance template hard-codes.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// us14dBuiltinAgentNames 是安装器写入的五只内置 Agent（顺序即发行目录顺序）。
// us14dBuiltinAgentNames lists the five built-in agents in distribution order.
var us14dBuiltinAgentNames = []string{"biz-daily", "docs-diff", "github-radar", "industry-brief", "repo-digest"}

const (
	// us14dDefaultAddr 是新实例配置模板里的监听地址：`http.addr` 没有环境变量覆盖，
	// 判"模板生成的东西当次生效"只能落在它身上。
	// us14dDefaultAddr is the address the new-instance template hard-codes.
	us14dDefaultAddr = "127.0.0.1:8080"

	// us14dInstanceIDPattern 是名册里实例标识的合法形态（契约：inst_ + 16 位小写十六进制）。
	// us14dInstanceIDPattern is the contract shape of the roster's instance id.
	us14dInstanceIDPattern = `^inst_[0-9a-f]{16}$`
)

// us14dProfile 是 `/profiles` 元素的判卷副本：在公共夹具读数之外补 provider / model /
// problems —— E1 判的正是"装好了、但还没轮到配置"的那三项。
// us14dProfile adds the provider / model / problems fields E1 judges.
type us14dProfile struct {
	Name       string             `json:"name"`
	Provider   string             `json:"provider"`
	Model      string             `json:"model"`
	Registered bool               `json:"registered"`
	Problems   []string           `json:"problems"`
	Lifecycle  adminLifecycleView `json:"lifecycle"`
}

// us14dReadinessItem / us14dReadiness 是就绪视图的判卷副本（契约 AgentReadinessView
// 同形；本套件不 import internal 包）。
// us14dReadiness mirrors the readiness contract view.
type us14dReadinessItem struct {
	Kind   string `json:"kind"`
	Item   string `json:"item"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

type us14dReadiness struct {
	Agent   string               `json:"agent"`
	Ready   bool                 `json:"ready"`
	Pending []us14dReadinessItem `json:"pending"`
	Notices []us14dReadinessItem `json:"notices"`
}

// us14dMCPServer 是 `/mcp-servers` 元素的判卷副本（只取本文件判的字段）。
// us14dMCPServer keeps only the fields this file judges.
type us14dMCPServer struct {
	Name               string `json:"name"`
	Connected          bool   `json:"connected"`
	PublishedVersionID int64  `json:"published_version_id"`
	PendingRestart     bool   `json:"pending_restart"`
}

// us14dPrivateEnv 把实例私有数据根钉进用例自己的临时目录：`LOCALAPPDATA`（Windows）/
// `HOME`（macOS）/ `XDG_DATA_HOME`（Linux）一起改，三平台的数据根都落在工作区之外——
// 否则用例会读写真实用户目录下的 admin-token，互相污染，也会把"本机已有一把钥匙"
// 当成产品行为。GitHub 凭据默认清空：只有 E2 经 extra 显式给值。
// us14dPrivateEnv pins the instance data root into a per-test temp directory on all
// three platforms and blanks the GitHub credential unless a case supplies it.
func us14dPrivateEnv(dataRoot string, extra ...string) []string {
	env := append(fixtureEnv(),
		"LOCALAPPDATA="+dataRoot,
		"APPDATA="+dataRoot,
		"HOME="+dataRoot,
		"XDG_DATA_HOME="+dataRoot,
		"GITHUB_PERSONAL_ACCESS_TOKEN=",
	)
	return append(env, extra...)
}

// us14dAuthHeader 返回一条只带管理面令牌的头（E9 用的是新实例自动生成的密码，
// 不是夹具的固定值）。
// us14dAuthHeader builds the bearer header for an explicitly supplied token.
func us14dAuthHeader(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// us14dProfiles 读一次 `/profiles`（令牌由调用方给）。
// us14dProfiles reads /profiles with the given token.
func us14dProfiles(t *testing.T, addr, token string) []us14dProfile {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/profiles", "", us14dAuthHeader(token))
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读取 profiles")
	var rows []us14dProfile
	decodeWebData(t, resp, &rows, "读取 profiles")
	return rows
}

// us14dFindProfile 在读数里按名字取一行。
// us14dFindProfile looks one profile row up by name.
func us14dFindProfile(rows []us14dProfile, name string) (us14dProfile, bool) {
	for _, row := range rows {
		if row.Name == name {
			return row, true
		}
	}
	return us14dProfile{}, false
}

// us14dWaitRegisteredOrLog 轮询注册状态；超时把**被测进程的输出**贴进失败信息——
// 注册失败的原因（provider / MCP / 技能 / 预算）只出现在进程日志里，profiles 读数本身
// 不带拒因，只报读数会让这条用例没法定位。
// us14dWaitRegisteredOrLog polls until the agent registers and attaches the process
// output on timeout, because the rejection reason only lives in the process log.
func us14dWaitRegisteredOrLog(t *testing.T, p *groupedProcess, addr, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last us14dProfile
	for time.Now().Before(deadline) {
		last, _ = us14dFindProfile(us14dProfiles(t, addr, e2eAdminToken), name)
		if last.Registered {
			return
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待 Agent %s 注册超时（%s），最后一次读数 %+v\n进程输出：\n%s", name, timeout, last, p.output())
}

// us14dWaitProfile 轮询 `/profiles` 直到 ready 成立；超时报告最后一次读数。
// us14dWaitProfile polls /profiles until ready holds.
func us14dWaitProfile(t *testing.T, addr, token, name string, timeout time.Duration,
	ready func(us14dProfile, bool) bool,
) us14dProfile {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var (
		last us14dProfile
		ok   bool
	)
	for time.Now().Before(deadline) {
		last, ok = us14dFindProfile(us14dProfiles(t, addr, token), name)
		if ready(last, ok) {
			return last
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待 Agent %s 的 profiles 条件超时（%s），最后一次读数 %+v（存在=%v）", name, timeout, last, ok)
	return us14dProfile{}
}

// us14dReadinessOf 读一次就绪视图。
// us14dReadinessOf reads the readiness view once.
func us14dReadinessOf(t *testing.T, addr, token, name string) us14dReadiness {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/agents/"+name+"/readiness", "", us14dAuthHeader(token))
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读取 Agent "+name+" 的就绪视图")
	var view us14dReadiness
	decodeWebData(t, resp, &view, "读取 Agent "+name+" 的就绪视图")
	return view
}

// us14dWaitReadiness 轮询就绪视图直到 ready 成立。
// us14dWaitReadiness polls the readiness view until ready holds.
func us14dWaitReadiness(t *testing.T, addr, token, name string, timeout time.Duration,
	ready func(us14dReadiness) bool,
) us14dReadiness {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last us14dReadiness
	for time.Now().Before(deadline) {
		last = us14dReadinessOf(t, addr, token, name)
		if ready(last) {
			return last
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待 Agent %s 的就绪条件超时（%s），最后一次读数 %+v", name, timeout, last)
	return us14dReadiness{}
}

// us14dHasNotice 报告 notices 里是否有一项的 detail 含片段。
// us14dHasNotice reports whether any notice detail carries the fragment.
func us14dHasNotice(view us14dReadiness, fragment string) bool {
	for _, notice := range view.Notices {
		if strings.Contains(notice.Detail, fragment) {
			return true
		}
	}
	return false
}

// us14dMCPServers 读一次 MCP 列表。
// us14dMCPServers reads the MCP server list once.
func us14dMCPServers(t *testing.T, addr, token string) []us14dMCPServer {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/mcp-servers", "", us14dAuthHeader(token))
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读取 MCP 列表")
	var servers []us14dMCPServer
	decodeWebData(t, resp, &servers, "读取 MCP 列表")
	return servers
}

// us14dFindMCPServer 在列表里按名字取一条。
// us14dFindMCPServer looks one MCP entry up by name.
func us14dFindMCPServer(servers []us14dMCPServer, name string) (us14dMCPServer, bool) {
	for _, server := range servers {
		if server.Name == name {
			return server, true
		}
	}
	return us14dMCPServer{}, false
}

// us14dWaitScheduleRow 轮询调度列表直到该任务行出现：停用的 Agent 也先把任务行落库，
// 才谈得上"到点不执行"——先等到它，随后的零执行才是判据。
// us14dWaitScheduleRow polls the schedule list until the task row appears.
func us14dWaitScheduleRow(t *testing.T, addr, id string, timeout time.Duration) scheduleSummary {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []scheduleSummary
	for time.Now().Before(deadline) {
		last = listSchedules(t, addr)
		for _, row := range last {
			if row.ID == id {
				return row
			}
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待调度任务 %s 落库超时（%s），最后一次读数 %+v", id, timeout, last)
	return scheduleSummary{}
}

// us14dBuiltinFiles 返回五只内置 Agent 的定义与生命周期状态文件的相对路径（另加 extra）：
// "安装器与基线不得改写已就位的东西"一类判据按这些文件的字节等值成立。
// us14dBuiltinFiles lists the built-in agents' definitions and lifecycle state files.
func us14dBuiltinFiles(extra ...string) []string {
	rels := make([]string, 0, len(us14dBuiltinAgentNames)*2+len(extra))
	for _, name := range us14dBuiltinAgentNames {
		rels = append(rels, ".basalt/agents/"+name+"/AGENT.md")
		rels = append(rels, ".basalt/control/agents/"+name+".json")
	}
	return append(rels, extra...)
}

// us14dSnapshot 记录一组工作区相对路径的内容摘要。
// us14dSnapshot records the digests of workspace-relative files.
func us14dSnapshot(t *testing.T, h *harness, rels []string) map[string]string {
	t.Helper()
	digests := make(map[string]string, len(rels))
	for _, rel := range rels {
		path := filepath.Join(h.Root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("快照：读 %s 失败: %v", rel, err)
		}
		sum := sha256.Sum256(raw)
		digests[rel] = hex.EncodeToString(sum[:])
	}
	return digests
}

// us14dRequireSameBytes 逐文件比对快照：任何一处字节变化都判红（并点出前后摘要前 12 位）。
// us14dRequireSameBytes fails on the first file whose bytes changed.
func us14dRequireSameBytes(t *testing.T, h *harness, before map[string]string, label string) {
	t.Helper()
	for rel, want := range before {
		path := filepath.Join(h.Root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s：读 %s 失败: %v", label, rel, err)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s：%s 被改写（之前 sha256 %s，现在 %s）——安装器与启动基线只追加，缺口按 kept 保留",
				label, rel, want[:12], got[:12])
		}
	}
}

// us14dRequireFreePort 等到 addr 可被监听；占用超时判红并给出可执行的处置。E7 / E9 /
// E10 判的是新实例模板里的固定地址，改端口就等于换掉被测对象。
// us14dRequireFreePort waits until addr is bindable; the fixed address is the subject
// under test, so substituting a port would test something else.
func us14dRequireFreePort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		listener, err := net.Listen("tcp", addr)
		if err == nil {
			_ = listener.Close()
			return
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("监听地址 %s 在 20 秒内始终不可用（最后一次错误 %v）——新实例配置模板写死该地址，"+
		"`http.addr` 没有环境变量覆盖；必须先停掉占用它的进程（本机演示实例 / 后台 serve）再跑本用例",
		addr, lastErr)
}

// us14dInstanceID 读 `.basalt/instance.json` 的实例标识并校验形态。
// us14dInstanceID reads and validates the roster's instance id.
func us14dInstanceID(t *testing.T, h *harness) string {
	t.Helper()
	path := filepath.Join(h.Root, ".basalt", "instance.json")
	var marker struct {
		Version    int    `json:"version"`
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal([]byte(readText(t, path)), &marker); err != nil {
		t.Fatalf("实例名册 %s 不是合法 JSON: %v", path, err)
	}
	if marker.Version != 1 {
		t.Fatalf("实例名册 %s 的 version = %d，期望 1", path, marker.Version)
	}
	if !regexp.MustCompile(us14dInstanceIDPattern).MatchString(marker.InstanceID) {
		t.Fatalf("实例名册 %s 的 instance_id = %q，期望 inst_ 加 16 位小写十六进制", path, marker.InstanceID)
	}
	return marker.InstanceID
}

// us14dGeneratedAdminToken 读新实例自动生成的访问密码文件（实例私有目录，工作区之外）。
// us14dGeneratedAdminToken reads the admin token the first start generated.
func us14dGeneratedAdminToken(t *testing.T, dataRoot, instanceID string) string {
	t.Helper()
	path := filepath.Join(dataRoot, "basalt", "instances", instanceID, "admin-token")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读自动生成的访问密码 %s 失败: %v", path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		t.Fatalf("自动生成的访问密码 %s 为空", path)
	}
	return token
}

// TestUS14D_AgentsDisabledAfterInstall（E1）判"装好但等配置"：全新实例首次启动后，
// 五只内置 Agent 已经落在工作区里，但都被装载规则拒载（内置定义里的模型名留空，等运维
// 填写）、生命周期一律 disabled / 代数 1；免凭据的两条 MCP 条目本次启动即发布，需要
// 凭据的 github 条目**不在**文件里。
// E1: a fresh instance installs the five agents (all rejected, disabled/1) plus the two
// credential-free MCP entries, which are published in this very start.
func TestUS14D_AgentsDisabledAfterInstall(t *testing.T) {
	h := newHarness(t)
	addr := freeLoopbackAddr(t)
	// 配置里声明内置 Agent 引用的 provider：规则 2（provider 已发布）因此通过，
	// 拒载落在规则 3（模型名为空）——这正是"等运维填模型"的唯一缺口。
	h.writeConfig(t, v3ServeConfig(addr,
		"providers:\n"+v3ProviderBlock("local-model", "http://127.0.0.1:1", "e2e-model"), ""))

	p := startGrouped(t, h, us14dPrivateEnv(t.TempDir()), "serve")
	waitHealthHTTP(t, p, addr, 60*time.Second)

	rows := us14dProfiles(t, addr, e2eAdminToken)
	for _, name := range us14dBuiltinAgentNames {
		row, ok := us14dFindProfile(rows, name)
		if !ok {
			t.Fatalf("内置 Agent %s 未出现在 /profiles（首次启动应安装五只）", name)
		}
		if row.Registered {
			t.Fatalf("内置 Agent %s 不得在首次启动即注册：模型名留空，运维还没填", name)
		}
		if row.Provider != "local-model" {
			t.Fatalf("内置 Agent %s 的 provider = %q，期望 local-model", name, row.Provider)
		}
		if row.Model != "" {
			t.Fatalf("内置 Agent %s 的 model = %q，期望空串（内置定义留空待填）", name, row.Model)
		}
		if len(row.Problems) != 1 || row.Problems[0] != "未在 AGENT.md 中声明默认模型" {
			t.Fatalf("内置 Agent %s 的装载问题 = %v，期望恰好一条「未在 AGENT.md 中声明默认模型」", name, row.Problems)
		}
		if row.Lifecycle.State == nil || *row.Lifecycle.State != "disabled" {
			t.Fatalf("内置 Agent %s 的生命周期 = %v，期望 disabled（启动基线为缺失状态建 disabled）", name, row.Lifecycle.State)
		}
		if row.Lifecycle.Generation != 1 {
			t.Fatalf("内置 Agent %s 的 generation = %d，期望 1", name, row.Lifecycle.Generation)
		}
	}

	// 免凭据的两条条目在文件里、github 不在：凭据没就绪时写出来的是"注定发布失败"
	// （env 闸门同一判据），安装器因此连写都不写。
	mcpText := readText(t, filepath.Join(h.Root, ".basalt", "mcp_servers.yaml"))
	for _, want := range []string{"docs-readonly", "dbhub"} {
		if !strings.Contains(mcpText, "- name: "+want) {
			t.Fatalf("mcp_servers.yaml 缺少免凭据条目 %s：\n%s", want, mcpText)
		}
	}
	if strings.Contains(mcpText, "- name: github") {
		t.Fatalf("凭据未就绪时不得写入 github 条目：\n%s", mcpText)
	}

	// 发布面：安装器写在首次解析之前，两条免凭据条目本次启动即已发布（不需要重启）。
	servers := us14dMCPServers(t, addr, e2eAdminToken)
	for _, want := range []string{"docs-readonly", "dbhub"} {
		server, ok := us14dFindMCPServer(servers, want)
		if !ok {
			t.Fatalf("MCP 列表缺少 %s", want)
		}
		if server.PublishedVersionID <= 0 {
			t.Fatalf("MCP %s 尚未发布（published_version_id = %d）——安装器必须排在首次解析与启动基线之前",
				want, server.PublishedVersionID)
		}
	}

	p.stop(t)
}

// TestUS14D_MCPAutoPublishedOnCredential（E2）判凭据闸门的两面：没有凭据时不写 github
// 条目（写出来也发布不了）；凭据就绪后的下一次启动补写该条目并当次发布，而已就位的
// 五只 Agent 定义与生命周期状态**逐字节不变**（安装器只追加，不是第二条覆盖路径）。
// 补写走安装路径，因此不得留下 `mcp_server.*` 管理操作行。
// E2: the credential-gated entry appears only once the credential exists, is published
// in the same start, and nothing that already existed is rewritten.
func TestUS14D_MCPAutoPublishedOnCredential(t *testing.T) {
	h := newHarness(t)
	dataRoot := t.TempDir()
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, v3ServeConfig(addr,
		"providers:\n"+v3ProviderBlock("local-model", "http://127.0.0.1:1", "e2e-model"), ""))

	mcpPath := filepath.Join(h.Root, ".basalt", "mcp_servers.yaml")

	// 第一次启动：环境里没有 GitHub 凭据。
	first := startGrouped(t, h, us14dPrivateEnv(dataRoot), "serve")
	waitHealthHTTP(t, first, addr, 60*time.Second)
	if text := readText(t, mcpPath); strings.Contains(text, "- name: github") {
		t.Fatalf("凭据缺失时不得写入 github 条目：\n%s", text)
	}
	// 快照取在健康检查之后：夹具在启动前补写的绑定已落盘，此后任何字节变化都来自被测进程。
	before := us14dSnapshot(t, h, us14dBuiltinFiles())
	first.stop(t)

	newAddr := freeLoopbackAddr(t)
	v3RewriteAddr(t, h, addr, newAddr)

	// 第二次启动：凭据就绪 → 补写 github 条目；dbhub / docs-readonly 已在，零写入。
	const token = "e2e-github-token-not-a-secret"
	second := startGrouped(t, h, us14dPrivateEnv(dataRoot, "GITHUB_PERSONAL_ACCESS_TOKEN="+token), "serve")
	waitHealthHTTP(t, second, newAddr, 60*time.Second)

	text := readText(t, mcpPath)
	if !strings.Contains(text, "- name: github") {
		t.Fatalf("凭据就绪后应补写 github 条目：\n%s", text)
	}
	if strings.Contains(text, token) {
		t.Fatalf("凭据明文进了 mcp_servers.yaml——占位纪律（${ENV_VAR}）被破坏：\n%s", text)
	}
	if !strings.Contains(text, "${GITHUB_PERSONAL_ACCESS_TOKEN}") {
		t.Fatalf("github 条目必须以 ${GITHUB_PERSONAL_ACCESS_TOKEN} 占位：\n%s", text)
	}

	servers := us14dMCPServers(t, newAddr, e2eAdminToken)
	github, ok := us14dFindMCPServer(servers, "github")
	if !ok {
		t.Fatalf("MCP 列表缺少 github 条目")
	}
	if github.PublishedVersionID <= 0 {
		t.Fatalf("补写的 github 条目本次启动即应发布（published_version_id = %d）", github.PublishedVersionID)
	}

	us14dRequireSameBytes(t, h, before, "凭据补写不得改写已就位的定义")
	second.stop(t)

	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM management_operations WHERE action LIKE 'mcp_server.%'`); n != 0 {
		t.Fatalf("安装器的补写不得留下 mcp_server.* 管理操作行，实际 %d 条（补写不是管理面保存路径）", n)
	}
}

// TestUS14D_StartupIsolation（E3）判启动隔离：`mcp_servers.yaml` 里一条连不上的条目
// 只拒载引用它的那只 Agent，不拖垮进程；引用可用条目的 Agent 照常注册。
// E3: one broken MCP entry refuses only the agent that references it.
func TestUS14D_StartupIsolation(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr("http://127.0.0.1:1", addr)+mcpShellAllow(os.Args[0]))
	// helper 是自举的测试可执行件（能连上）；broken 的命令不在盘上。
	h.writeMCPServers(t, mcpServersYAML("helper")+
		"  - name: broken\n    command: basalt-e2e-missing-mcp-server\n")
	h.writeAgent(t, "good", mcpAgentFrontmatter("good", []string{"echo"}, []string{"helper"}), "依赖可用 MCP")
	h.writeAgent(t, "bad", mcpAgentFrontmatter("bad", nil, []string{"broken"}), "依赖不可用 MCP")

	p := startGrouped(t, h, mcpHelperEnv(), "serve")
	waitHealthHTTP(t, p, addr, 60*time.Second)

	// 引用可用条目的 Agent 正常注册：进程活着且它的两条 Entry 都成立。
	us14dWaitRegisteredOrLog(t, p, addr, "good", 30*time.Second)

	// 引用坏条目的 Agent 只被拒载，拒因点名该条目。
	bad := us14dWaitProfile(t, addr, e2eAdminToken, "bad", 30*time.Second,
		func(row us14dProfile, ok bool) bool { return ok && !row.Registered && len(row.Problems) > 0 })
	want := "MCP 服务不可用（未配置或未连接）：broken"
	found := false
	for _, problem := range bad.Problems {
		if problem == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("Agent bad 的装载问题 = %v，期望包含 %q", bad.Problems, want)
	}

	servers := us14dMCPServers(t, addr, e2eAdminToken)
	helperServer, ok := us14dFindMCPServer(servers, "helper")
	if !ok || !helperServer.Connected {
		t.Fatalf("helper 条目应已连接：%+v（存在=%v）", helperServer, ok)
	}
	brokenServer, ok := us14dFindMCPServer(servers, "broken")
	if !ok {
		t.Fatalf("broken 条目应出现在列表里（文件里写着，只是连不上）")
	}
	if brokenServer.Connected {
		t.Fatalf("broken 条目的命令不在盘上，不得报已连接")
	}

	p.stop(t)
}

// TestUS14D_DisabledNotScheduled（E5）判"启用才运行"：停用的 Agent 到点不产生任何执行
// 行与 Run（调度门在**任何写之前**判定）；在同一进程内启用后，下一次到点即跑出一次
// completed 的 cron Run——门每次触发都重读状态文件，不需要重启。
// E5: a disabled agent never writes an execution row or a run; enabling it in the same
// process makes the next tick run.
func TestUS14D_DisabledNotScheduled(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t,
		textResponse("定时任务完成"),
		textResponse("定时任务完成"),
		textResponse("定时任务完成"),
	)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	h.writeAgent(t, "demo",
		agentFrontmatter("demo", nil)+scheduleBlock("*/5 * * * * *", "", "定时演示"),
		"定时演示")
	// 覆盖夹具默认的 active / 2：本用例的起点是"新建即停用"。
	h.writeLifecycleState(t, "demo", "disabled", 1)

	p := startGrouped(t, h, us14dPrivateEnv(t.TempDir()), "serve")
	waitHealthHTTP(t, p, addr, 60*time.Second)

	// 任务行先落库（证明文件定义已被接受），但停用态不得触发。
	us14dWaitScheduleRow(t, addr, "demo-1", 30*time.Second)
	// 跨过至少两个 5 秒触发点：门若在写之后判定，这里就会留下执行行。
	time.Sleep(11 * time.Second)

	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs WHERE agent = 'demo'`); n != 0 {
		t.Fatalf("停用的 Agent 不得产生 Run，实际 %d 条", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM task_executions WHERE task_id = 'demo-1'`); n != 0 {
		t.Fatalf("停用的 Agent 不得产生执行行，实际 %d 条", n)
	}
	if n := scalarInt(t, db,
		`SELECT COUNT(*) FROM scheduled_tasks WHERE task_id = 'demo-1' AND last_run_at IS NULL`); n != 1 {
		t.Fatalf("停用期间 last_run_at 必须保持为空（门在写之前判定），匹配行数 = %d", n)
	}

	// 同一进程内启用 → 下一次到点即跑。
	waitAgentEnabled(t, addr, "demo", 1)
	waitScheduleStatus(t, addr, "demo-1", "completed", 90*time.Second)
	p.stop(t)

	if n := scalarInt(t, db,
		`SELECT COUNT(*) FROM runs WHERE agent = 'demo' AND trigger = 'cron'
		   AND channel = 'scheduler' AND status = 'completed'`); n != 1 {
		t.Fatalf("启用后应恰好产生一次 completed 的 cron Run，实际 %d 条", n)
	}
}

// TestUS14D_EnableBlockedWithoutConfig（E6）判启用前置校验与就绪视图同源：缺模型连接 /
// 缺 MCP 时启用返回 409 AGENT_UNAVAILABLE 并逐项点名，就绪视图给出同一句话与可点的
// 跳转；只有"通知渠道未配置"这类**可选**项进 notices，不阻止启用。
// E6: enable is refused with a named reason when a required resource is missing, while
// the readiness view reports the same item and a working jump path.
func TestUS14D_EnableBlockedWithoutConfig(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr("http://127.0.0.1:1", addr))
	// ghostly：provider 不在已发布集合里（规则 2 拒载）。
	h.writeAgent(t, "ghostly",
		"name: ghostly\ndescription: 14D 缺模型连接演示\nprovider:\n  name: ghost\n  model: demo-model\ntools: []\n",
		"缺模型连接")
	// mcp-bound：声明了一个没有配置、也连不上的 MCP server（规则 6 拒载）。
	h.writeAgent(t, "mcp-bound", mcpAgentFrontmatter("mcp-bound", nil, []string{"missing-mcp"}), "缺 MCP")
	// plain：引用全部就绪，只差（可选的）通知渠道。
	h.writeAgent(t, "plain", agentFrontmatter("plain", nil), "就绪")
	for _, name := range []string{"ghostly", "mcp-bound", "plain"} {
		h.writeLifecycleState(t, name, "disabled", 1)
	}

	p := startGrouped(t, h, us14dPrivateEnv(t.TempDir()), "serve")
	waitHealthHTTP(t, p, addr, 60*time.Second)

	// (a) 缺模型连接：409 点名 provider，就绪项归 model_connection 且跳到模型连接页。
	us14dWaitProfile(t, addr, e2eAdminToken, "ghostly", 30*time.Second,
		func(row us14dProfile, ok bool) bool { return ok && !row.Registered })
	resp := enableAgentRequest(t, addr, "ghostly", 1)
	requireWebStatus(t, resp, http.StatusConflict, "AGENT_UNAVAILABLE",
		"Agent「ghostly」当前不可启用：引用的模型连接未发布或不存在：ghost", "启用缺模型连接的 Agent")

	view := us14dReadinessOf(t, addr, e2eAdminToken, "ghostly")
	if view.Ready {
		t.Fatalf("缺模型连接时 ready 不得为真：%+v", view)
	}
	if len(view.Pending) != 1 || view.Pending[0].Kind != "model_connection" ||
		view.Pending[0].Fix != "/console/resources/model-connections" {
		t.Fatalf("ghostly 的就绪待补项 = %+v，期望恰一条 model_connection 且跳转 /console/resources/model-connections", view.Pending)
	}

	// (b) 缺 MCP：409 点名 server，就绪项归 resource 且跳到 MCP 页。
	us14dWaitProfile(t, addr, e2eAdminToken, "mcp-bound", 30*time.Second,
		func(row us14dProfile, ok bool) bool { return ok && !row.Registered })
	resp = enableAgentRequest(t, addr, "mcp-bound", 1)
	requireWebStatus(t, resp, http.StatusConflict, "AGENT_UNAVAILABLE",
		"Agent「mcp-bound」当前不可启用：MCP 服务不可用（未配置或未连接）：missing-mcp", "启用缺 MCP 的 Agent")

	view = us14dReadinessOf(t, addr, e2eAdminToken, "mcp-bound")
	if view.Ready {
		t.Fatalf("缺 MCP 时 ready 不得为真：%+v", view)
	}
	if len(view.Pending) != 1 || view.Pending[0].Kind != "resource" ||
		view.Pending[0].Fix != "/console/resources/mcp-servers" {
		t.Fatalf("mcp-bound 的就绪待补项 = %+v，期望恰一条 resource 且跳转 /console/resources/mcp-servers", view.Pending)
	}

	// (c) 就绪的 Agent：通知渠道缺失只进 notices，不阻止启用。
	view = us14dReadinessOf(t, addr, e2eAdminToken, "plain")
	if !view.Ready || len(view.Pending) != 0 {
		t.Fatalf("plain 的引用全部就绪，ready 应为真且待补项为空：%+v", view)
	}
	if !us14dHasNotice(view, "尚未配置通知渠道（可选，不阻止启用）") {
		t.Fatalf("plain 的 notices 应点名未配置的通知渠道：%+v", view.Notices)
	}
	waitAgentEnabled(t, addr, "plain", 1)

	p.stop(t)
}

// TestUS14D_FreshInstanceBudgetPreset（E7）判两件事：全新实例首次启动生成最小配置
// （8080 / 并发 32 / 预算 30 / token 模式且**不落明文**）与实例名册；已有配置的实例
// 启动前后逐字节不变，名册仍然生成（"配置在、名册不在"继续按新实例补写）。
// E7: a fresh start generates the minimal config plus the roster; an existing config is
// never rewritten.
func TestUS14D_FreshInstanceBudgetPreset(t *testing.T) {
	t.Run("新实例生成配置与名册", func(t *testing.T) {
		h := newHarness(t)
		dataRoot := t.TempDir()
		// 模板把监听地址写死成 127.0.0.1:8080，且该键没有环境变量覆盖。
		us14dRequireFreePort(t, us14dDefaultAddr)

		p := startGrouped(t, h, us14dPrivateEnv(dataRoot), "serve")
		waitHealthHTTP(t, p, us14dDefaultAddr, 60*time.Second)

		text := readText(t, filepath.Join(h.Root, "basalt.yaml"))
		for _, want := range []string{
			"addr: 127.0.0.1:8080",
			"mode: token",
			"max_concurrent_runs: 32",
			"max_iterations: 30",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("生成的配置缺少 %q：\n%s", want, text)
			}
		}
		// token 模式但**不写** token：密码由实例私有目录提供，明文禁止落盘。
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "token:") {
				t.Fatalf("生成的配置不得带 token 字面量（明文凭据禁止落盘）：%q", line)
			}
		}
		// 命令白名单与随包条目同源（T117 现场缺陷的回归面）：安装器写入的条目 command
		// 是绝对路径，白名单整串逐字比对——不登记时内置 MCP 会在 spawn 前被策略拒绝。
		runtimeRoot := filepath.Join(filepath.Dir(h.Binary), "runtime")
		for _, want := range []string{
			filepath.Join(runtimeRoot, "mcp", "docs-readonly", us14dDocsReadOnlyVersion,
				"docs-readonly"+us14dExeSuffix()),
			filepath.Join(runtimeRoot, "node", "node"+us14dExeSuffix()),
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("生成的配置未登记随包运行时命令 %q，内置 MCP 条目会在 spawn 前被策略拒绝：\n%s",
					want, text)
			}
		}

		us14dInstanceID(t, h)

		combined := p.combined()
		if !strings.Contains(combined, "新实例：最小配置与实例名册已就绪") {
			t.Fatalf("首次启动应记一条新实例初始化日志：\n%s", combined)
		}
		if !strings.Contains(combined, "config_created=true") {
			t.Fatalf("日志应如实记录本次创建了配置（config_created=true）：\n%s", combined)
		}

		p.stop(t)
	})

	t.Run("已有配置一字节不改", func(t *testing.T) {
		h := newHarness(t)
		dataRoot := t.TempDir()
		addr := freeLoopbackAddr(t)
		// 已有配置且预算被运维压到 7：启动不得"顺手"改回模板值。
		h.writeConfig(t, configWithAddr("http://127.0.0.1:1", addr)+"budget:\n  max_iterations: 7\n")
		configPath := filepath.Join(h.Root, "basalt.yaml")
		before := readText(t, configPath)

		p := startGrouped(t, h, us14dPrivateEnv(dataRoot), "serve")
		waitHealthHTTP(t, p, addr, 60*time.Second)
		p.stop(t)

		if after := readText(t, configPath); after != before {
			t.Fatalf("已有配置文件被改写：\n之前：\n%s\n之后：\n%s", before, after)
		}
		// 名册仍然生成：配置在、名册不在继续按新实例补写（次序本身就是判据）。
		us14dInstanceID(t, h)
	})
}

// TestUS14D_BudgetCompatNotices（E8）判"只提示不改配置"：Agent 声明的预算超过实例
// 上限、启用计划的 Agent 数超过并发上限时，就绪视图只出两条 notices（点名两边数字），
// 不改 `basalt.yaml`、也不改 Agent 定义。
// E8: over-limit budget and schedule concurrency produce notices only; no file changes.
func TestUS14D_BudgetCompatNotices(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, v3ServeConfig(addr,
		"providers:\n"+v3ProviderBlock("local-model", "http://127.0.0.1:1", "e2e-model"),
		"budget:\n  max_iterations: 10\nserver:\n  max_concurrent_runs: 1\n"))
	frontmatter := func(name string, extra string) string {
		return "name: " + name + "\ndescription: 14D 预算与并发提示演示\n" +
			"provider:\n  name: local-model\n  model: e2e-model\ntools: []\n" + extra
	}
	// alpha 声明 40 轮（实例上限 10）；两只 Agent 都挂一个几小时后才触发的计划。
	h.writeAgent(t, "alpha",
		frontmatter("alpha", "settings:\n  max_iterations: 40\n")+scheduleBlock(farFutureCron(), "", "预算演示"),
		"预算演示")
	h.writeAgent(t, "beta",
		frontmatter("beta", "")+scheduleBlock(farFutureCron(), "", "并发演示"),
		"并发演示")

	p := startGrouped(t, h, us14dPrivateEnv(t.TempDir()), "serve")
	waitHealthHTTP(t, p, addr, 60*time.Second)

	waitAgentRegistered(t, addr, "alpha")
	waitAgentRegistered(t, addr, "beta")

	view := us14dWaitReadiness(t, addr, e2eAdminToken, "alpha", 30*time.Second,
		func(view us14dReadiness) bool { return us14dHasNotice(view, "声明 40 超过实例上限 10") })
	if !us14dHasNotice(view, "超出部分不会生效（实例配置键 budget.max_iterations）") {
		t.Fatalf("预算提示应说明超出部分不会生效并点名配置键：%+v", view.Notices)
	}
	if !us14dHasNotice(view, "有 2 只 Agent 启用了计划，超过并发上限 1") {
		t.Fatalf("并发提示应点名两边数字：%+v", view.Notices)
	}
	if !us14dHasNotice(view, "到点可能被拒绝（429 TOO_MANY_RUNS）") {
		t.Fatalf("并发提示应说明后果（429）：%+v", view.Notices)
	}
	if !view.Ready {
		t.Fatalf("提示项不得阻止启用（ready 应为真）：%+v", view)
	}

	// 两条提示都**只提示**：配置与两只 Agent 的定义一字节不改。
	us14dRequireSameBytes(t, h, us14dSnapshot(t, h, []string{"basalt.yaml"}), "预算 / 并发提示不得改配置")
	us14dRequireSameBytes(t, h, us14dSnapshot(t, h, []string{
		".basalt/agents/alpha/AGENT.md",
		".basalt/agents/beta/AGENT.md",
	}), "预算 / 并发提示不得改 Agent 定义")

	p.stop(t)
}

// TestUS14D_ConfigEffectiveThisStart（E9）判"生成的配置当次即生效"：全新实例第一次
// 启动写出 30 轮预算、名册与访问密码，同一个进程里用那把自动生成的密码发布模型连接、
// 运行期写入声明 40 轮的 Agent，就绪视图立刻点名"声明 40 超过实例上限 30"——不需要
// 重启来让配置生效。
// E9: the generated configuration takes effect in the very process that wrote it.
func TestUS14D_ConfigEffectiveThisStart(t *testing.T) {
	h := newHarness(t)
	dataRoot := t.TempDir()
	us14dRequireFreePort(t, us14dDefaultAddr)

	p := startGrouped(t, h, us14dPrivateEnv(dataRoot), "serve")
	waitHealthHTTP(t, p, us14dDefaultAddr, 60*time.Second)

	configPath := filepath.Join(h.Root, "basalt.yaml")
	if text := readText(t, configPath); !strings.Contains(text, "max_iterations: 30") {
		t.Fatalf("本次启动生成的配置应带 30 轮预算：\n%s", text)
	}

	// 第三来源的访问密码：配置只写 mode: token，密码在实例私有目录。
	instanceID := us14dInstanceID(t, h)
	token := us14dGeneratedAdminToken(t, dataRoot, instanceID)

	// 用自动生成的密码发布模型连接：新实例第一次启动即可被管理，不必手工补 token。
	var created struct {
		Name string `json:"name"`
	}
	resp := adminWrite(t, us14dDefaultAddr, http.MethodPost, "/api/v1/model-connections", map[string]any{
		"name":          "local-model",
		"base_url":      "http://127.0.0.1:1",
		"api_key":       "${" + fixtureKeyEnv + "}",
		"default_model": "demo-model",
	}, us14dAuthHeader(token))
	requireWriteOK(t, resp, &created, "用自动生成的访问密码发布模型连接")

	// 运行期写入一只声明 40 轮的 Agent：装载器接受，就绪视图按**本次生效的**上限提示。
	h.writeAgent(t, "tuned",
		"name: tuned\ndescription: 14D 预算兼容演示\nprovider:\n  name: local-model\n  model: demo-model\n"+
			"tools: []\nsettings:\n  max_iterations: 40\n",
		"预算兼容演示")
	us14dWaitProfile(t, us14dDefaultAddr, token, "tuned", 30*time.Second,
		func(row us14dProfile, ok bool) bool { return ok && row.Registered })

	view := us14dWaitReadiness(t, us14dDefaultAddr, token, "tuned", 30*time.Second,
		func(view us14dReadiness) bool { return us14dHasNotice(view, "声明 40 超过实例上限 30") })
	if !view.Ready {
		t.Fatalf("预算提示不得阻止启用（ready 应为真）：%+v", view)
	}

	// "当次生效"是读出来的，不是写出来的：提示不改配置。
	if text := readText(t, configPath); !strings.Contains(text, "max_iterations: 30") {
		t.Fatalf("就绪提示不得改写实例配置：\n%s", text)
	}

	p.stop(t)
}

// TestUS14D_ExistingNoConfigUntouched（E10）判存量实例的 fail-closed：`init` 建过库但
// 没有配置文件的工作区**不得**被当成新实例补生成——没有管理认证配置就拒绝启动，
// 且不写配置、不写名册、不建实例私有目录（不猜旧路径）。
// E10: an established instance without a config refuses to start and writes nothing.
func TestUS14D_ExistingNoConfigUntouched(t *testing.T) {
	h := newHarness(t)
	dataRoot := t.TempDir()
	us14dRequireFreePort(t, us14dDefaultAddr)

	// `init` 只建骨架与库、不写名册：之后的工作区是"存量实例"，不是新实例。
	initWorkspace(t, h)

	res := h.run(t, us14dPrivateEnv(dataRoot), "serve")
	if res.ExitCode == 0 {
		t.Fatalf("存量实例缺管理认证配置时必须拒绝启动，实际退出码 0\nstdout:\n%s\nstderr:\n%s",
			res.Stdout, res.Stderr)
	}
	output := res.Stdout + res.Stderr
	for _, want := range []string{
		"管理认证配置校验失败",
		"http.admin_auth.mode 缺失或非法",
		"存量实例",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("启动失败输出缺少 %q：\n%s", want, output)
		}
	}

	if _, err := os.Stat(filepath.Join(h.Root, "basalt.yaml")); err == nil {
		t.Fatalf("存量实例不得被补生成配置文件")
	}
	if _, err := os.Stat(filepath.Join(h.Root, ".basalt", "instance.json")); err == nil {
		t.Fatalf("存量实例不得被补写实例名册——没有名册就不猜私有目录")
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "basalt", "instances")); err == nil {
		t.Fatalf("存量实例不得创建实例私有目录（%s）", filepath.Join(dataRoot, "basalt", "instances"))
	}
}

// TestUS14D_FixtureEndToEnd（E4）判的是**装出来的那一份**：五只内置 Demo 由真实
// `basalt demo install` 落盘，之后跑的是与 `TestUS14Demos_Fixture` 完全同一份剧本与
// 断言（`runDemoScenarios`）——两处唯一的变量就是 Demo 的来源。安装器的三件制品
// （安装清单、五份留空模型的 AGENT.md、docs-readonly 源码导出）由夹具在任何字段改写
// 之前先读一遍（`newInstalledDemoFixture` → `requireInstallArtifacts`），所以本用例
// 通过即说明「装出来的那一份」确实能跑，而不是测试自己复制的那一份。
//
// 检索（D5）走内置 `web_search`，端点是编译期常量，本机没有可用替身：夹具把该域名
// 钉进禁止域名表，D5 由此判「检索不可达时来源表留空、不编造」。成功一侧由
// `TestWebSearch_*` 与 industry-brief 的脚本单测覆盖，本用例不声称外网可达。
//
// E4 judges the installed copy: the five demos come from the real installer and then run
// the exact same scenarios as TestUS14Demos_Fixture. The only variable is their source.
func TestUS14D_FixtureEndToEnd(t *testing.T) {
	runDemoScenarios(t, newInstalledDemoFixture)
}
