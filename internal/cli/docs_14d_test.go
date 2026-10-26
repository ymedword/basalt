package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDocs14D_FactsMatchImplementation 校验 14D 的文档事实与实现一致：运维手册写下的
// 路径 / 轴名必须能在契约、路由注册与控制台文案里找到对应字面量，避免文档单方面漂移。
// TestDocs14D_FactsMatchImplementation keeps the 14D documentation facts aligned with the
// implementation: every path, axis name and label promised in docs/Operations.md must exist
// literally in the contract, the route table and the console view.
func TestDocs14D_FactsMatchImplementation(t *testing.T) {
	root := repoRoot(t)
	read := func(t *testing.T, name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(data)
	}

	// upgrade-impact-list：升级影响清单必须点明两轴、两个显式动作与「不放行」语义，
	// 并且这些字面量在契约、路由与控制台里都能对上。
	// upgrade-impact-list: the impact list must name both axes and both explicit actions,
	// with every literal traceable to the contract, the route table and the console.
	t.Run("upgrade-impact-list", func(t *testing.T) {
		const heading = "#### 升级影响清单（受保护地址与 shell 名称）"
		operations := read(t, filepath.Join("docs", "Operations.md"))
		_, after, found := strings.Cut(operations, heading)
		if !found {
			t.Fatalf("docs/Operations.md is missing %q", heading)
		}
		block, _, found := strings.Cut(after, "### 5.5")
		if !found {
			t.Fatal("docs/Operations.md impact list has no §5.5 boundary")
		}
		for _, want := range []string{
			"`schema_version` 5 → 6",
			"`network_allowlist` 轴",
			"`shell_command` 轴",
			"POST /api/v1/network-exceptions",
			"POST /api/v1/execution-policy/confirmations",
			"不改授权结论",
			"**不得**为了让清单变短",
		} {
			if !strings.Contains(block, want) {
				t.Errorf("impact list is missing %q", want)
			}
		}

		contract := read(t, filepath.Join("docs", "contracts", "openapi.yaml"))
		for _, path := range []string{
			"\n  /execution-policy:\n",
			"\n  /execution-policy/confirmations:\n",
			"\n  /network-exceptions:\n",
		} {
			if !strings.Contains(contract, path) {
				t.Errorf("docs/contracts/openapi.yaml is missing path %q", strings.TrimSpace(path))
			}
		}

		routes := read(t, filepath.Join("internal", "web", "admin_routes.go"))
		for _, route := range []string{
			`"/api/v1/execution-policy"`,
			`"/api/v1/execution-policy/confirmations"`,
			`"/api/v1/network-exceptions"`,
		} {
			if !strings.Contains(routes, route) {
				t.Errorf("internal/web/admin_routes.go does not register %s", route)
			}
		}

		console := read(t, filepath.Join("assets", "console", "src", "views", "workspace.ts"))
		for _, label := range []string{"待确认项", "加例外（放行）", "保持拒绝", "确认加例外", "未确认不放行", "不改变结论"} {
			if !strings.Contains(console, label) {
				t.Errorf("assets/console/src/views/workspace.ts is missing %q", label)
			}
		}
	})

	// operations-sync：14D 事实块（执行档位 / 禁止域名 / 访问例外 / 密码文件 / 实例加密 /
	// 逐槽凭据）写进运维手册后，必须与实现字面量对得上。
	// operations-sync: the 14D fact blocks in the operations manual must match the code.
	t.Run("operations-sync", func(t *testing.T) {
		operations := read(t, filepath.Join("docs", "Operations.md"))
		_, after, found := strings.Cut(operations, "**执行档位与出网基础防护（14D）**")
		if !found {
			t.Fatal("docs/Operations.md is missing the 14D execution-profile block")
		}
		block, _, found := strings.Cut(after, "**必须记住的生效时间差**")
		if !found {
			t.Fatal("docs/Operations.md execution-profile block has no hot-effective-list boundary")
		}
		for _, want := range []string{
			"`strict`", "`standard`", "存量实例升级一律",
			"禁止域名", "受保护地址段", "云元数据",
			"`profile_denied`", "`denied_domain`", "`protected_address`", "`exception_missing`",
		} {
			if !strings.Contains(block, want) {
				t.Errorf("execution-profile block is missing %q", want)
			}
		}

		for _, want := range []string{
			"admin-token", "三处都没有",
			"GET /api/v1/instance/secrets", "POST /api/v1/instance/secrets/init",
			"`uninitialized`", "`ready`", "`recovery_required`", "`unprovable_provenance`",
			"`credential_summary`", "binding_key",
			"`basalt secrets init`", "`basalt secrets status`",
			"`basalt secrets verify`", "`basalt secrets revoke-managed --confirm`",
		} {
			if !strings.Contains(operations, want) {
				t.Errorf("docs/Operations.md is missing %q", want)
			}
		}

		contract := read(t, filepath.Join("docs", "contracts", "openapi.yaml"))
		for _, path := range []string{"\n  /instance/secrets:\n", "\n  /instance/secrets/init:\n"} {
			if !strings.Contains(contract, path) {
				t.Errorf("docs/contracts/openapi.yaml is missing path %q", strings.TrimSpace(path))
			}
		}

		routes := read(t, filepath.Join("internal", "web", "admin_routes.go"))
		for _, route := range []string{
			`"/api/v1/instance/secrets"`,
			`"/api/v1/instance/secrets/init"`,
		} {
			if !strings.Contains(routes, route) {
				t.Errorf("internal/web/admin_routes.go does not register %s", route)
			}
		}

		policy := read(t, filepath.Join("internal", "core", "execution_policy.go"))
		for _, reason := range []string{"profile_denied", "denied_domain", "protected_address", "exception_missing"} {
			if !strings.Contains(policy, `"`+reason+`"`) {
				t.Errorf("internal/core/execution_policy.go is missing denial reason %q", reason)
			}
		}
		secrets := read(t, filepath.Join("internal", "core", "instance_secrets.go"))
		for _, state := range []string{"uninitialized", "ready", "recovery_required", "unprovable_provenance"} {
			if !strings.Contains(secrets, `"`+state+`"`) {
				t.Errorf("internal/core/instance_secrets.go is missing state or reason %q", state)
			}
		}

		cli := read(t, filepath.Join("internal", "cli", "secrets.go"))
		for _, sub := range []string{`"secrets"`, `"init"`, `"status"`, `"verify"`, `"revoke-managed"`} {
			if !strings.Contains(cli, sub) {
				t.Errorf("internal/cli/secrets.go is missing %s", sub)
			}
		}
	})

	// web-search-sync：14D 的 D5 检索口径（内置 web_search + DeepSeek Search API）在契约、
	// 运维手册、实现与示例目录之间必须对得上：端点常量逐字一致；示例不得再声明 MCP 搜索
	// 工具；契约也不得自行写下未经核实的限流数值。
	// web-search-sync: the D5 search channel (built-in web_search over the DeepSeek Search API)
	// must line up across the contract, the operations manual, the implementation and the
	// example directory; the contract promises no rate the official page has not confirmed.
	t.Run("web-search-sync", func(t *testing.T) {
		contract := read(t, filepath.Join("docs", "contracts", "agent-md.md"))
		if !strings.Contains(contract, "## 5. 内置工具契约（11 个）") {
			t.Error("docs/contracts/agent-md.md no longer declares eleven built-in tools")
		}
		const sectionHeading = "### 5.7 SearchTools"
		_, after, found := strings.Cut(contract, sectionHeading)
		if !found {
			t.Fatalf("docs/contracts/agent-md.md is missing %q", sectionHeading)
		}
		section, _, found := strings.Cut(after, "## 6. 拒绝信息格式")
		if !found {
			t.Fatal("docs/contracts/agent-md.md §5.7 has no §6 boundary")
		}
		for _, want := range []string{
			"https://api.deepseek.com/v1/tools/search",
			"need_content: false",
			"每次外呼各判一次",
			"`count` | int | 否",
			"不声明 QPS 数值",
		} {
			if !strings.Contains(section, want) {
				t.Errorf("§5.7 is missing %q", want)
			}
		}

		impl := read(t, filepath.Join("internal", "tool", "websearch.go"))
		for _, want := range []string{
			`"https://api.deepseek.com/v1/tools/search"`,
			`json:"need_content"`,
			`json:"rerank_score"`,
			`json:"authority"`,
		} {
			if !strings.Contains(impl, want) {
				t.Errorf("internal/tool/websearch.go is missing %s", want)
			}
		}

		operations := read(t, filepath.Join("docs", "Operations.md"))
		for _, want := range []string{
			"搜索出网（`web_search` → `https://api.deepseek.com/v1/tools/search`）",
			"内置 `web_search`、Fetch MCP",
			"内置 `web_search` 的搜索凭据",
		} {
			if !strings.Contains(operations, want) {
				t.Errorf("docs/Operations.md is missing %q", want)
			}
		}

		agent := read(t, filepath.Join("examples", "daily", "industry-brief", "AGENT.md"))
		for _, want := range []string{"  - web_search", "mcp_servers:"} {
			if !strings.Contains(agent, want) {
				t.Errorf("industry-brief AGENT.md is missing %q", want)
			}
		}
		if strings.Contains(agent, "brave_web_search") {
			t.Error("industry-brief AGENT.md still declares the retired MCP search tool brave_web_search")
		}
	})

	// authoritative-sync：三份权威正文是本地私有件（干净检出里不存在），存在时逐份核对
	// 14D 关键事实；缺失时按 requirements-local 的同一口径跳过并写明理由。
	// authoritative-sync: the three authoritative documents are local-private; when present,
	// each must carry the 14D facts, otherwise skip with the same reason as requirements-local.
	t.Run("authoritative-sync", func(t *testing.T) {
		cases := []struct {
			file  string
			wants []string
		}{
			{
				file: filepath.Join("docs", "Requirements.md"),
				wants: []string{
					"执行档位（14D，两档）", "访问例外（仅网络）",
					"`exception_missing`", "admin-token", "没有**默认密码",
					"basalt secrets init", "basalt demo install",
					"`execution_policy`", "`instance_secrets`", "`credentials`（v6 重建）",
					"**内置十一工具**", "`web_search`", "搜索 + Fetch 双通道",
				},
			},
			{
				file: filepath.Join("docs", "TechnicalDesign.md"),
				wants: []string{
					"#### 4.3.4 执行档位与出网边界（14D）",
					"迁移一律落 `strict`", "`execution_policy`", "`network_exceptions`",
					"`instance_secrets`", "`credentials`（v6 重建）", "admin-token",
					"basalt secrets init", "basalt demo install",
					"`GET /instance/secrets`", "`POST /instance/secrets/init`",
					"SearchTools", "`web_search`（US-14 14D 增量", "搜索 + Fetch 双通道",
				},
			},
			{
				file: filepath.Join("docs", "Compatibility.md"),
				wants: []string{
					"14D", "`execution_policy`", "`instance_secrets`", "admin-token",
					"`binding_key`", "v5 → v6", "第 11 个内置工具",
				},
			},
		}
		for _, tc := range cases {
			t.Run(filepath.Base(tc.file), func(t *testing.T) {
				if _, err := os.Stat(filepath.Join(root, tc.file)); errors.Is(err, os.ErrNotExist) {
					t.Skipf("%s is local-private; absent in clean checkouts, run locally for drift protection", tc.file)
				} else if err != nil {
					t.Fatalf("stat %s: %v", tc.file, err)
				}
				text := read(t, tc.file)
				for _, want := range tc.wants {
					if !strings.Contains(text, want) {
						t.Errorf("%s is missing %q", tc.file, want)
					}
				}
			})
		}
	})
}

// repoRoot 从测试文件位置回溯到仓库根目录。
// repoRoot walks up from this test file to the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate documentation consistency test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}
