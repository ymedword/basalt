//go:build e2e

package e2e

// 本文件回填 US-1（工程地基）/ US-2（Provider）/ US-3（Agent 与 Profile）的人工项
// （模块卡 §2.6.1~§2.6.3）。全部场景驱动真实二进制与临时工作区，断言只读进程输出、
// 退出码与工作区里的 SQLite 库；不调用包内函数。
// This file backfills the manual items of US-1/US-2/US-3 by driving the real binary in
// temporary workspaces and reading only process output, exit codes and the workspace DB.

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBackfill_Bootstrap 覆盖地基三件：装配、凭证、init 幂等、坏配置、不可达端点审计。
// 子测试名与模块卡 §2.6 的目录项逐字对应（`backfillCatalog` 以 `Parent/sub` 全名登记）。
// TestBackfill_Bootstrap covers the foundation trio; subtest names are the catalog keys.
func TestBackfill_Bootstrap(t *testing.T) {
	t.Run("init_idempotent", testBootstrapInitIdempotent)
	t.Run("status_no_config", testBootstrapStatusNoConfig)
	t.Run("version_injection", testBootstrapVersionInjection)
	t.Run("negative_three", testBootstrapNegativeThree)
	t.Run("credential_scan", testBootstrapCredentialScan)
	t.Run("assembly", testBootstrapAssembly)
	t.Run("provider_unreachable_audit", testBootstrapProviderUnreachableAudit)
	t.Run("provider_negative", testBootstrapProviderNegative)
	t.Run("profile", testBootstrapProfile)
	t.Run("bad_agents", testBootstrapBadAgents)
	t.Run("cli_import_graph", testBootstrapCLIImportGraph)
	t.Run("tool_list", testBootstrapToolList)
	t.Run("credential_template", testBootstrapCredentialTemplate)
}

// testBootstrapInitIdempotent：`basalt init` 二跑幂等——工作区条目不覆盖、库零写。
// 判据（US-1 §6 Demo ②）：第二次输出「新建：（无）」「保留（N）」「已存在，零写」，
// 且五个引导文件的内容哈希逐字节不变。
func testBootstrapInitIdempotent(t *testing.T) {
	h := newHarness(t)

	first := h.run(t, nil, "init")
	requireExit(t, first, 0, "init 首次")

	files := []string{"AGENTS.md", "SOUL.md", "USER.md", "mcp_servers.yaml", "memory/MEMORY.md"}
	before := make(map[string]string, len(files))
	for _, rel := range files {
		path := filepath.Join(h.Root, ".basalt", filepath.FromSlash(rel))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("init 首次未生成 %s: %v", rel, err)
		}
		before[rel] = fileDigest(t, path)
	}
	if _, err := os.Stat(h.dbPath()); err != nil {
		t.Fatalf("init 首次未生成库 %s: %v", h.dbPath(), err)
	}

	second := h.run(t, nil, "init")
	requireExit(t, second, 0, "init 二次")
	requireContains(t, second.Stdout, "新建：（无）", "init 二次")
	requireContains(t, second.Stdout, "保留（", "init 二次")
	requireContains(t, second.Stdout, "已存在，零写（schema v5）", "init 二次")

	for _, rel := range files {
		path := filepath.Join(h.Root, ".basalt", filepath.FromSlash(rel))
		if got := fileDigest(t, path); got != before[rel] {
			t.Fatalf("%s 在二次 init 后被改写（%s → %s）", rel, before[rel], got)
		}
	}
}

// testBootstrapStatusNoConfig：无配置文件时 `status` 仍 exit 0，且明示「未发现配置」。
// 判据（US-1 §6 Demo ③）：库、schema 版本、29 张表就绪，汇总全部通过。
func testBootstrapStatusNoConfig(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)

	res := h.run(t, nil, "status")
	requireExit(t, res, 0, "status 无配置")
	for _, want := range []string{"未发现配置", "schema v5", "29 张表", "汇总：全部通过"} {
		requireContains(t, res.Stdout, want, "status 无配置")
	}
}

// testBootstrapVersionInjection：构建期注入的版本必须由 `--version` 回显
// （US-1 §6 Demo ④；TestMain 以 `-X main.version=us15-e2e` 构建）。
func testBootstrapVersionInjection(t *testing.T) {
	h := newHarness(t)

	res := h.run(t, nil, "--version")
	requireExit(t, res, 0, "--version")
	combined := res.Stdout + res.Stderr
	requireContains(t, combined, buildVersion, "--version")
	requireContains(t, combined, "basalt", "--version")
}

// testBootstrapNegativeThree：US-1 §6 负向三连。
//
//  1. 未格式化文件 → 格式门禁非零并点名文件；
//  2. 语法错误文件 → 非零（脚本按「先查退出码、再判输出」的口径报异常退出）；
//  3. `${UNRESOLVED}` 残留 → 真实二进制拒绝启动并点名配置键。
//
// 前两项把 `scripts/check-format.sh` 复制进临时工作区后执行——脚本按自身路径推导
// 仓库根，直接调用仓库内脚本会扫描整个仓库而不是本用例造的坏文件。
func testBootstrapNegativeThree(t *testing.T) {
	root := mustRepoRoot(t)
	h := newHarness(t)

	script, err := os.ReadFile(filepath.Join(root, "scripts", "check-format.sh"))
	if err != nil {
		t.Fatalf("读取 scripts/check-format.sh 失败: %v", err)
	}
	scriptDir := filepath.Join(h.Root, "scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("建临时 scripts/ 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "check-format.sh"), script, 0o755); err != nil {
		t.Fatalf("复制 check-format.sh 失败: %v", err)
	}
	badGo := filepath.Join(h.Root, "bad.go")

	// ① 未格式化：缩进用空格，gofmt 会把它列进 -l 输出（退出码仍为 0）。
	if err := os.WriteFile(badGo, []byte("package bad\n\nfunc F() {\n    _ = 1\n}\n"), 0o644); err != nil {
		t.Fatalf("写未格式化文件失败: %v", err)
	}
	unformatted := runShellScript(t, h, "scripts/check-format.sh")
	requireExit(t, unformatted, 1, "未格式化")
	requireContains(t, unformatted.Stderr, "未格式化（gofmt）", "未格式化")
	requireContains(t, unformatted.Stderr, "bad.go", "未格式化")

	// ② 语法错误：gofmt 退出码非 0、stdout 为空——脚本必须先查退出码才判得出红态。
	if err := os.WriteFile(badGo, []byte("package bad\n\nfunc Broken( {\n"), 0o644); err != nil {
		t.Fatalf("写语法错误文件失败: %v", err)
	}
	syntax := runShellScript(t, h, "scripts/check-format.sh")
	requireExit(t, syntax, 1, "语法错误")
	requireContains(t, syntax.Stderr, "gofmt 异常退出（退出码", "语法错误")

	// ③ 未解析占位符：凭证键引用的环境变量不存在 → 拒绝启动并点名键路径。
	// 先 init 一次，把失败面收窄到配置本身。
	broken := newHarness(t)
	initWorkspace(t, broken)
	broken.writeConfig(t, "providers:\n"+
		"  - name: "+fixtureProvider+"\n"+
		"    base_url: http://127.0.0.1:1/v1\n"+
		"    api_key: ${BASALT_E2E_UNRESOLVED_9F3A}\n"+
		"    default_model: "+fixtureModel+"\n")
	res := broken.run(t, nil, "status")
	requireExit(t, res, 1, "未解析占位符")
	combined := res.Stdout + res.Stderr
	requireContains(t, combined, "providers[0].api_key", "未解析占位符")
	requireContains(t, combined, "BASALT_E2E_UNRESOLVED_9F3A", "未解析占位符")
}

// credentialSentinel 是夹具凭证的已知值（与 fixtureEnv 注入的同一串）：它出现在任何
// 输出或落盘文件里都算泄漏。失败文案只报位置，不重复打印该值。
// credentialSentinel is the fixture credential's known value: any appearance in
// output or on disk is a leak; failure messages name the location only.
const credentialSentinel = "e2e-sentinel-not-a-secret"

// testBootstrapCredentialScan：运行对象的凭证面——三路输出（provider list / 失败文案 /
// status）与工作区落盘均不得出现凭证值（US-1 §6 / US-2 §6 的凭证复核）。
func testBootstrapCredentialScan(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeConfig(t, configWithProvider("http://127.0.0.1:1/v1"))
	h.writeAgent(t, "scan", agentFrontmatter("scan", nil), "凭证面输出测试")

	// 三路输出之一：provider list（正常路径）。
	list := h.run(t, fixtureEnv(), "provider", "list")
	requireExit(t, list, 0, "provider list")
	requireContains(t, list.Stdout, "汇总：1 家 Provider", "provider list")
	if leakedIn(list.Stdout + list.Stderr) {
		t.Fatalf("provider list 的输出回显了凭证值（值不打印）")
	}

	// 三路输出之二：失败文案（不可达 endpoint 的 chat 运行）。
	chat := h.run(t, fixtureEnv(), "chat", "--agent", "scan", "--message", "你好")
	if chat.ExitCode == 0 {
		t.Fatalf("不可达 provider 下的 chat 应非零退出\n%s%s", chat.Stdout, chat.Stderr)
	}
	if leakedIn(chat.Stdout + chat.Stderr) {
		t.Fatalf("chat 失败文案回显了凭证值（值不打印）")
	}

	// 三路输出之三：status 诊断。
	status := h.run(t, fixtureEnv(), "status")
	requireExit(t, status, 0, "status")
	if leakedIn(status.Stdout + status.Stderr) {
		t.Fatalf("status 输出回显了凭证值（值不打印）")
	}

	// 落盘面：工作区任何文件（库 / 日志 / 配置 / 快照）都不得含凭证值。
	for _, path := range walkFiles(t, h.Root) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取工作区文件 %s 失败: %v", path, err)
		}
		if bytes.Contains(data, []byte(credentialSentinel)) {
			rel, relErr := filepath.Rel(h.Root, path)
			if relErr != nil {
				rel = path
			}
			t.Fatalf("%s 落盘了凭证值（值不打印）", rel)
		}
	}
}

// leakedIn 判输出里是否出现夹具凭证哨兵（失败文案不重复打印哨兵本身）。
// leakedIn reports whether the fixture credential sentinel appears in the output.
func leakedIn(text string) bool {
	return strings.Contains(text, credentialSentinel)
}

// testBootstrapAssembly：US-1 / US-2 的装配确认（人工走查改为机器判据）。
//
//  1. 审计 SQL 只出现在 `internal/storage/`（全局不变量 3）；
//  2. `internal/core` 不依赖任何非标准库包（`go list -deps`）；
//  3. provider 映射由配置数组逐条构建，无「取第一个 / 遍历映射表挑一个」形态；
//  4. `--help` 可用（US-4 §6.1 第 7 项的装配确认面）。
func testBootstrapAssembly(t *testing.T) {
	root := mustRepoRoot(t)

	// ① 审计 SQL 不外溢。先确认判定面本身有效（至少命中一处），再核对归属。
	// 谓词按「动词 + 表名」两段拼接：本文件自身也躺在 internal/ 下，若源码里出现
	// 连续字面量，扫描会命中自己（自匹配假红）。
	// The needles are assembled from two parts so this file cannot match itself.
	auditSQL := []struct{ verb, table string }{
		{"INTO ", "llm_calls"},
		{"UPDATE ", "llm_calls"},
		{"INTO ", "tool_invocations"},
		{"UPDATE ", "tool_invocations"},
	}
	hits := 0
	walkErr := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(data)
		for _, pattern := range auditSQL {
			needle := pattern.verb + pattern.table
			if !strings.Contains(text, needle) {
				continue
			}
			hits++
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			if !strings.HasPrefix(filepath.ToSlash(rel), "internal/storage/") {
				t.Errorf("审计 SQL 外溢：%s 含 %q（只允许出现在 internal/storage）", filepath.ToSlash(rel), needle)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("遍历 internal/ 失败: %v", walkErr)
	}
	if hits == 0 {
		t.Fatalf("审计 SQL 判定面失效：internal/ 下未找到任何审计语句")
	}

	// ② core 只依赖标准库（`go list -deps` 的模板对标准库输出空行）。
	// `-deps` 的结果含被查包自身，先取它的 ImportPath 以便跳过——自身不是依赖。
	// -deps includes the queried package itself; skip it, it is not a dependency.
	self := strings.TrimSpace(runGo(t, root, "list", "-f", "{{.ImportPath}}", "./internal/core"))
	raw := runGo(t, root, "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./internal/core")
	var external []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == self {
			continue
		}
		external = append(external, line)
	}
	if len(external) > 0 {
		t.Fatalf("internal/core 依赖了非标准库包：\n%s", strings.Join(external, "\n"))
	}

	// ③ 映射构建：逐条遍历有序配置数组；不得取首项、不得遍历映射表挑一个。
	providers, err := os.ReadFile(filepath.Join(root, "internal", "app", "providers.go"))
	if err != nil {
		t.Fatalf("读取 internal/app/providers.go 失败: %v", err)
	}
	src := string(providers)
	requireContains(t, src, "range cfg.Providers", "映射构建")
	for _, forbidden := range []string{"Providers[0]", "range clients"} {
		if strings.Contains(src, forbidden) {
			t.Fatalf("映射构建含禁用形态 %q（宪法 III：不得从表里挑一个）", forbidden)
		}
	}

	// ④ 命令面 --help 可用（重 / 轻路径都能走到命令树）。
	h := newHarness(t)
	help := h.run(t, nil, "--help")
	requireExit(t, help, 0, "--help")
	for _, want := range []string{"chat", "profile", "serve", "tool"} {
		requireContains(t, help.Stdout, want, "--help")
	}
}

// testBootstrapProviderUnreachableAudit：不可达端点也必须落审计（US-2 §6 Demo ③）：
// 运行期失败 → `llm_calls` 有终态行，`status=failed` 且 `error_kind` 非空。
func testBootstrapProviderUnreachableAudit(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeConfig(t, configWithProvider("http://127.0.0.1:1/v1"))
	h.writeAgent(t, "offline", agentFrontmatter("offline", nil), "不可达 provider 的审计回填")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "offline", "--message", "你好")
	if res.ExitCode == 0 {
		t.Fatalf("不可达端点应使 chat 非零退出\n%s%s", res.Stdout, res.Stderr)
	}

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT status, error_kind FROM llm_calls ORDER BY id DESC LIMIT 1`)
	if len(rows) != 1 {
		t.Fatalf("llm_calls 应有 1 行终态审计，实际 %d 行", len(rows))
	}
	status, _ := rows[0]["status"].(string)
	if status != "failed" {
		t.Fatalf("llm_calls.status = %v，期望 failed", rows[0]["status"])
	}
	kind, _ := rows[0]["error_kind"].(string)
	if strings.TrimSpace(kind) == "" {
		t.Fatalf("llm_calls.error_kind 为空（%v）——失败必须可解释", rows[0]["error_kind"])
	}
}

// testBootstrapProviderNegative：装载期拒绝 userinfo / query 成分，文案点名键与成分、
// 不回显 URL（US-2 §6 Demo ④/⑤）。
func testBootstrapProviderNegative(t *testing.T) {
	cases := []struct {
		label     string
		baseURL   string
		component string
	}{
		{"userinfo", "http://user:secret@127.0.0.1:1/v1", "userinfo"},
		{"query", "http://127.0.0.1:1/v1?token=secret", "query"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			h := newHarness(t)
			h.writeConfig(t, configWithProvider(tc.baseURL))

			res := h.run(t, fixtureEnv(), "status")
			requireExit(t, res, 1, "坏 base_url")
			combined := res.Stdout + res.Stderr
			requireContains(t, combined, "providers[0].base_url", "坏 base_url")
			requireContains(t, combined, tc.component, "坏 base_url")
			for _, leak := range []string{"user:secret", "token=secret"} {
				if strings.Contains(combined, leak) {
					t.Fatalf("错误文案回显了 base_url 的成分 %q（只允许点名键与成分名）", leak)
				}
			}
		})
	}
}

// testBootstrapProfile：`profile create` / `list` / `show` 三命令（US-3 §6 Demo ①）。
// create 明示选中配置首项；list 渲染固定口径的注册行与汇总行；show 复用同一份装载结果。
func testBootstrapProfile(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeConfig(t, configWithProvider("http://127.0.0.1:1/v1"))

	create := h.run(t, fixtureEnv(), "profile", "create", "demo-agent")
	requireExit(t, create, 0, "profile create")
	requireContains(t, create.Stdout, "已选择 provider："+fixtureProvider+"（配置首项）", "profile create")
	requireContains(t, create.Stdout, "创建：agents/demo-agent/AGENT.md", "profile create")

	list := h.run(t, fixtureEnv(), "profile", "list")
	requireExit(t, list, 0, "profile list")
	requireContains(t, list.Stdout,
		"demo-agent：registered=true（provider "+fixtureProvider+" / 模型 请在此填写模型名）", "profile list")
	requireContains(t, list.Stdout, "汇总：1 个 Agent 目录（已注册 1）", "profile list")

	show := h.run(t, fixtureEnv(), "profile", "show", "demo-agent")
	requireExit(t, show, 0, "profile show")
	requireContains(t, show.Stdout, "身份：agent_name=demo-agent", "profile show")
	requireContains(t, show.Stdout, "技能：无", "profile show")
	requireContains(t, show.Stdout, "工具：save_memory、recall_memory", "profile show")
}

// testBootstrapBadAgents：六例坏配置逐只 `registered=false` + WARN 点名
// （US-3 §6 Demo ②；WARN 固定键 agent / reason / detail）。
func testBootstrapBadAgents(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeConfig(t, configWithProvider("http://127.0.0.1:1/v1"))

	bad := []struct {
		name        string
		frontmatter string
		reason      string
	}{
		{
			name: "bad-provider",
			frontmatter: "name: bad-provider\n" +
				"description: 未注册 provider\n" +
				"provider:\n  name: nosuch\n  model: " + fixtureModel + "\n" +
				"tools: []",
			reason: "provider not registered: nosuch",
		},
		{
			name: "bad-skill",
			frontmatter: "name: bad-skill\n" +
				"description: 技能缺失\n" +
				"provider:\n  name: " + fixtureProvider + "\n  model: " + fixtureModel + "\n" +
				"skills:\n  - missing-skill\n" +
				"tools: []",
			reason: "skill not found: missing-skill",
		},
		{
			name: "bad-cron5",
			frontmatter: "name: bad-cron5\n" +
				"description: 五段 cron\n" +
				"provider:\n  name: " + fixtureProvider + "\n  model: " + fixtureModel + "\n" +
				"schedules:\n  - id: five\n    cron: \"0 8 * * *\"\n    message: 五段\n" +
				"tools: []",
			reason: "invalid schedule",
		},
		{
			name: "bad-cron-dw",
			frontmatter: "name: bad-cron-dw\n" +
				"description: 日周双限定\n" +
				"provider:\n  name: " + fixtureProvider + "\n  model: " + fixtureModel + "\n" +
				"schedules:\n  - id: both\n    cron: \"0 0 8 1 * 1\"\n    message: 双限定\n" +
				"tools: []",
			reason: "日与周不得同时受限",
		},
		{
			name: "bad-zone",
			frontmatter: "name: bad-zone\n" +
				"description: 本地时区\n" +
				"provider:\n  name: " + fixtureProvider + "\n  model: " + fixtureModel + "\n" +
				"schedules:\n  - id: local\n    cron: \"0 0 8 * * *\"\n    zone: Local\n    message: 本地时区\n" +
				"tools: []",
			reason: "zone Local 被禁用",
		},
		{
			name: "bad-model",
			frontmatter: "name: bad-model\n" +
				"description: 空白模型\n" +
				"provider:\n  name: " + fixtureProvider + "\n  model: \"\"\n" +
				"tools: []",
			reason: "blank model",
		},
	}
	for _, tc := range bad {
		h.writeAgent(t, tc.name, tc.frontmatter, "坏配置回填")
	}

	res := h.run(t, fixtureEnv(), "profile", "list")
	requireExit(t, res, 0, "profile list")
	for _, tc := range bad {
		requireContains(t, res.Stdout, tc.name+"：registered=false——", "坏配置 "+tc.name)
		requireContains(t, res.Stdout, tc.reason, "坏配置原因 "+tc.name)
		requireContains(t, res.Stderr, "agent="+tc.name, "WARN 点名 "+tc.name)
	}
	requireContains(t, res.Stdout, "汇总：6 个 Agent 目录（已注册 0）", "profile list")
	if got := strings.Count(res.Stderr, `msg="Agent 未注册"`); got != len(bad) {
		t.Fatalf("WARN「Agent 未注册」条数 = %d，期望 %d\n%s", got, len(bad), res.Stderr)
	}
	requireContains(t, res.Stderr, "reason=", "WARN 固定键")
	requireContains(t, res.Stderr, "detail=", "WARN 固定键")
}

// testBootstrapCLIImportGraph：US-3 §6 第 7 项——cli 不 import 实现包（workspace）与
// 装配包（app）；app 侧以端口注入的方式向 cli 提供 workspace 能力。
func testBootstrapCLIImportGraph(t *testing.T) {
	root := mustRepoRoot(t)

	imports := runGo(t, root, "list", "-f", `{{join .Imports "\n"}}`, "./internal/cli")
	for _, line := range strings.Split(imports, "\n") {
		line = strings.TrimSpace(line)
		for _, forbidden := range []string{
			"github.com/ymedword/basalt/internal/workspace",
			"github.com/ymedword/basalt/internal/app",
		} {
			if line == forbidden {
				t.Fatalf("internal/cli import 了 %s——命令面只能依赖 core 与自身声明的端口", forbidden)
			}
		}
	}
	requireContains(t, imports, "github.com/ymedword/basalt/internal/core", "cli import")

	run, err := os.ReadFile(filepath.Join(root, "internal", "app", "run.go"))
	if err != nil {
		t.Fatalf("读取 internal/app/run.go 失败: %v", err)
	}
	requireContains(t, string(run), "Agents: agentsPort{", "agentsPort 注入")
}

// testBootstrapToolList：`tool list` 冒烟——十件内置工具（US-5 §6.1 第 4 项；US-6 §6.1
// 第 11 项记录历史读数八件的差异，以现行件数为判据）。
func testBootstrapToolList(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeConfig(t, configWithProvider("http://127.0.0.1:1/v1"))

	res := h.run(t, fixtureEnv(), "tool", "list")
	requireExit(t, res, 0, "tool list")
	requireContains(t, res.Stdout, "共 10 件工具", "tool list")
	for _, name := range []string{
		"http_get", "http_post", "list_dir", "manage_schedule", "notify",
		"read_file", "recall_memory", "save_memory", "shell", "write_file",
	} {
		requireContains(t, res.Stdout, name+"：", "tool list "+name)
	}
}

// testBootstrapCredentialTemplate：`basalt.yaml.example` 的凭证面——所有 api_key 一律
// `${VAR}` 占位，并在文件里明示凭证必须由环境变量提供（US-1 §2.6 第 16b 项）。
func testBootstrapCredentialTemplate(t *testing.T) {
	root := mustRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "basalt.yaml.example"))
	if err != nil {
		t.Fatalf("读取 basalt.yaml.example 失败: %v", err)
	}
	text := string(raw)

	requireContains(t, text, "凭证红线", "配置模板")
	requireContains(t, text, "${ENV_VAR}", "配置模板")
	requireContains(t, text, "按环境改", "配置模板")

	apiKeys := 0
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "api_key:") {
			continue
		}
		apiKeys++
		if !strings.Contains(line, "${") || !strings.Contains(line, "}") {
			t.Fatalf("api_key 行不是 ${VAR} 占位：%q", strings.TrimSpace(line))
		}
	}
	if apiKeys == 0 {
		t.Fatalf("配置模板里未找到任何 api_key 行——判定面失效")
	}
	if regexp.MustCompile(`sk-[A-Za-z0-9]{16,}`).MatchString(text) {
		t.Fatalf("配置模板含形似明文凭证的字符串")
	}
}

// initWorkspace 跑一次 init 并断言成功；后续场景都从已初始化工作区起步。
// initWorkspace runs init once and requires success.
func initWorkspace(t *testing.T, h *harness) {
	t.Helper()
	res := h.run(t, nil, "init")
	requireExit(t, res, 0, "init")
}

// requireExit 断言退出码；失败时带上完整输出（只用于不含凭证的场景）。
// requireExit asserts the exit code and reports both streams on failure.
func requireExit(t *testing.T, res runResult, want int, label string) {
	t.Helper()
	if res.ExitCode != want {
		t.Fatalf("%s：退出码 = %d，期望 %d\nstdout:\n%s\nstderr:\n%s",
			label, res.ExitCode, want, res.Stdout, res.Stderr)
	}
}

// requireContains 断言文本包含片段。
// requireContains asserts that text contains want.
func requireContains(t *testing.T, text, want, label string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%s：输出缺少 %q\n%s", label, want, text)
	}
}

// runShellScript 在临时工作目录里执行仓库脚本（用于复跑真实的格式门禁）。
// runShellScript runs a repository script inside the temporary work directory.
func runShellScript(t *testing.T, h *harness, script string) runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", script)
	cmd.Dir = h.Root
	cmd.Env = h.environ(nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := runResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: -1}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		t.Fatalf("脚本 %s 超时（%s）", script, h.timeout())
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("运行脚本 %s 失败: %v\n%s", script, err, res.Stderr)
	}
	return res
}

// runGo 在仓库根执行一条 `go` 子命令并返回标准输出（装配判据用）。
// runGo runs one go subcommand at the repository root and returns its stdout.
func runGo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go %s 失败: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// mustRepoRoot 取仓库根；定位失败即判红（装配类断言需要读源码）。
// mustRepoRoot returns the repository root or fails the test.
func mustRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}
	return root
}

// fileDigest 返回文件内容的十六进制 sha256（幂等判据用）。
// fileDigest returns the hex sha256 of a file's content.
func fileDigest(t *testing.T, path string) string {
	t.Helper()
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("计算 %s 的哈希失败: %v", path, err)
	}
	return sum
}

// walkFiles 列出目录下的全部普通文件（凭证落盘扫描用）。
// walkFiles lists every regular file under root.
func walkFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	return files
}
