//go:build e2e

package e2e

// 本文件是八模板的质量闸门（模块卡 §2.4.6 / §4.3 的 `template-quality`）：
//
//  1. **可装载**——用生产同一份装载器（internal/workspace.AgentLoader）判，不在测试里
//     另写一份 frontmatter 解析或准入规则；
//  2. **内置工具可解析**——工具名单取自候选二进制的 `tool list`（装配期注册池的真值），
//     测试不冻结第二份名单：装配层增删内置工具会同时打红这里，而不是悄悄放行；
//  3. **MCP 工具 ⊆ 目标 server 的 `tools/list`**——`docs-readonly` 是仓内自研 server，
//     走真连接、按它**实测**报出的工具面判；其余四家（brave-search / fetch / dbhub /
//     playwright）按各模板 README 记录的**候选名**配夹具服务。真实服务未授权实连，
//     这一点与 README 的「候选」「未实测」逐字同口径，**不**冒充真实实跑；
//  4. **无凭据**——模板会被复制进企业工作区，凭据只能是 ${ENV_VAR} 占位；
//  5. **档位固定**（`TestTemplates_TierEvidence`）——A / A* / B 与 README 文案一致；
//     A 不通过只能修复或保持未完成，**不得**自动降为 B（§2.4.6）。
//
// This file is the e2e gate for the eight shipped templates: loadability through the real
// loader, builtin tool resolution against the real registry, the MCP tool subset against
// each server's tools/list, credential-free directories, and frozen tier evidence.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/testutil"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// 档位字面量逐字来自模块卡 §2.4.6（review ① 2026-10-25 冻结）：README 必须写同一串。
// The tier literals are frozen by the reviewed module card; the READMEs must quote them.
const (
	templateTierA     = "A（全量）"
	templateTierAStar = "A*（浏览器待就绪）"
	templateTierB     = "B（配方）"
)

// templateProvider 与夹具配置里的 provider 名一致（装载规则 2：必须已注册）。
const templateProvider = "local-model"

// templateDocsServer 是唯一能在仓内实连核对的 MCP server（自研只读素材）。
// templateDocsServer is the only MCP server whose tool face can be verified in-repo.
const templateDocsServer = "docs-readonly"

// templateHelperArg 让 MCP 规格 helper 以「测试二进制 + 单个用例」的子进程形态被拉起
// （与五 Demo 夹具同一入口；TestMain 见 BASALT_MCP_HELPER 即跳过候选二进制构建）。
const templateHelperArg = "-test.run=^TestMCPHelperProcess$"

// templateCase 是一个模板的固定判据面：档位 + 模块卡 §3.6 的文件表。
// files 必须存在且非空（空文件 = 空壳）；globs 至少命中一个文件。
// templateCase is one template's frozen gate: its tier plus the §3.6 file table.
type templateCase struct {
	name  string
	tier  string
	files []string
	globs []string
}

// templateCatalog 是八模板的固定集合（模块卡 §2.4.6 / §3.6，review ① 已确认）。
// 改这张表 = 判据面变更：A 不通过只能修复或保持未完成，**不得**在这里降档或删模板。
// templateCatalog is the frozen eight-template set; editing it changes the judged surface.
var templateCatalog = []templateCase{
	{
		name:  "arxiv-tracker",
		tier:  templateTierA,
		files: []string{"AGENT.md", "README.md", "fixtures/response.xml"},
	},
	{
		name:  "dependency-watch",
		tier:  templateTierA,
		files: []string{"AGENT.md", "README.md", "fixtures/dependencies.txt", "fixtures/osv.json"},
	},
	{
		name:  "funding-watch",
		tier:  templateTierB,
		files: []string{"AGENT.md", "README.md"},
	},
	{
		name:  "onboarding-buddy",
		tier:  templateTierA,
		files: []string{"AGENT.md", "README.md", "skills/onboarding/SKILL.md"},
		globs: []string{"fixtures/docs/*.md"},
	},
	{
		name:  "policy-watch",
		tier:  templateTierA,
		files: []string{"AGENT.md", "README.md", "skills/policy-watch/SKILL.md", "fixtures/announcements.json"},
	},
	{
		name:  "ticket-insights",
		tier:  templateTierA,
		files: []string{"AGENT.md", "README.md", "skills/ticket-insights/SKILL.md", "fixtures/schema.sql", "fixtures/rows.json"},
	},
	{
		name:  "weather-brief",
		tier:  templateTierA,
		files: []string{"AGENT.md", "README.md", "fixtures/city.json"},
	},
	{
		name:  "web-watch",
		tier:  templateTierAStar,
		files: []string{"AGENT.md", "README.md", "fixtures/pages.json"},
	},
}

// templateCandidateFaces 是第三方 server 的工具面：`dbhub` 一行是 2026-10-26 的实测结果
// （@bytebase/dbhub 1.5.0：只读配置下只暴露 execute_sql；见 examples 里该 Demo 的实测
// 记录），其余三家仍是模板 README 记录的**候选名**（README 已标「未实测」）。
// 这里判的是「模板声明 ⊆ README 记录 + 夹具复现的面」；再改这张表（例如真实连到 brave）
// 仍是判据面变更，按 EngineeringStandards §2 裁决。
// templateCandidateFaces holds the candidate faces quoted from the template READMEs;
// the real third-party servers are not authorized for a live connection here.
var templateCandidateFaces = map[string][]string{
	"brave-search": {"brave_web_search"},
	"fetch":        {"fetch"},
	"dbhub":        {"execute_sql"},
	"playwright":   {"browser_navigate", "browser_snapshot"},
}

// TestTemplates_LoadAndTools 判四件事：八模板能装进工作区被真装载器注册、内置工具能在
// 注册池里解析、MCP 工具落在**声明过的** server 的工具面内、目录里没有凭据字面量。
// TestTemplates_LoadAndTools registers all eight templates through the real loader and
// checks tool resolution, the per-server MCP tool subset, and credential-free dirs.
func TestTemplates_LoadAndTools(t *testing.T) {
	repo, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}
	h := newHarness(t)
	builtin := templateBuiltinTools(t, h)

	// 模板是「复制到 .basalt/agents 与 .basalt/skills」的交付物：判据必须落在仓库里
	// 发出去的那一份字节上，夹具不另写 AGENT.md。
	// The templates are copied verbatim into the workspace; the gate judges the shipped bytes.
	workspaceDir := filepath.Join(h.Root, ".basalt")
	for _, tc := range templateCatalog {
		templateStage(t, repo, workspaceDir, tc.name)
	}
	faces := templateMCPFaces(t, h.Root, repo)

	// docs-readonly 是仓内真 server：它报的就是实名单，不是候选名。它一变，模板与
	// README 必须同改——所以这里钉死，而不是「只要超集就算过」。
	// The in-repo server's face is measured, not candidate: pin it exactly.
	wantDocs := []string{"document_metadata", "list_documents", "read_document"}
	if got := templateSortedFace(faces[templateDocsServer]); !slices.Equal(got, wantDocs) {
		t.Fatalf("docs-readonly 的 tools/list = %v，期望 %v（server 工具面变了，模板与 README 要同改）", got, wantDocs)
	}

	// 工具池 = 注册池内置名 ∪ 已连接 server 报出的全部工具名：与装配层构造装载器时
	// 的池同形（内置与 MCP 工具共用一个注册池）。
	// The pool mirrors the assembly: builtin names plus every connected MCP tool.
	serverNames := make([]string, 0, len(faces))
	poolNames := make([]string, 0, len(builtin)+8)
	for name := range builtin {
		poolNames = append(poolNames, name)
	}
	for name, face := range faces {
		serverNames = append(serverNames, name)
		for toolName := range face {
			poolNames = append(poolNames, toolName)
		}
	}
	slices.Sort(serverNames)
	slices.Sort(poolNames)

	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers:    []string{templateProvider},
		Tools:        poolNames,
		MCPServers:   serverNames,
		MCPConnected: serverNames,
		Logger:       slog.New(slog.DiscardHandler),
	})
	entries, err := loader.LoadAll(workspaceDir)
	if err != nil {
		t.Fatalf("装载八模板失败: %v", err)
	}
	byName := make(map[string]core.AgentEntry, len(entries))
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	if len(entries) != len(templateCatalog) {
		t.Fatalf("装载得到 %d 个条目，期望 %d（多出的条目说明 agents/ 里混进了别的东西）", len(entries), len(templateCatalog))
	}

	for _, tc := range templateCatalog {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := byName[tc.name]
			if !ok {
				t.Fatalf("装载结果里没有 %s（目录未落到 agents/%s？）", tc.name, tc.name)
			}
			if entry.Profile == nil {
				t.Fatalf("%s 的 AGENT.md 未解析：%s", tc.name, entry.RejectReason)
			}
			if !entry.Registered {
				t.Fatalf("%s 未注册：%s（拒绝类别 %s；声明工具 %v / MCP %v）",
					tc.name, entry.RejectReason, entry.RejectKind, entry.Profile.Tools, entry.Profile.MCPServers)
			}

			// 声明里的非内置名就是 MCP 工具；每个都必须落在某个**声明过的** server
			// 的工具面内，且声明了 server 就必须至少用上一个 MCP 工具——两头都判，
			// 免得「声明了 server 却全用内置工具」或「用了没声明的 server 的工具」蒙混。
			var mcpTools []string
			for _, name := range entry.Profile.Tools {
				if _, isBuiltin := builtin[name]; isBuiltin {
					continue
				}
				mcpTools = append(mcpTools, name)
			}
			switch {
			case len(entry.Profile.MCPServers) == 0 && len(mcpTools) > 0:
				t.Fatalf("声明了 MCP 工具 %v 却没声明 mcp_servers", mcpTools)
			case len(entry.Profile.MCPServers) > 0 && len(mcpTools) == 0:
				t.Fatalf("声明了 mcp_servers %v 却没有任何 MCP 工具（工具面无法核对）", entry.Profile.MCPServers)
			}
			for _, name := range mcpTools {
				owners := templateToolOwners(name, entry.Profile.MCPServers, faces)
				if len(owners) == 0 {
					t.Fatalf("MCP 工具 %q 不在任何声明过的 server %v 的 tools/list 里（各面：%s）",
						name, entry.Profile.MCPServers, templateFaceSummary(entry.Profile.MCPServers, faces))
				}
			}

			// README 的「MCP 工具名」行是给使用者的核对依据：集合必须与声明逐项相等，
			// 差一个就说明文档与 AGENT.md 已经不同步。
			documented := templateReadmeMCPTools(t, repo, tc.name)
			want := slices.Clone(mcpTools)
			slices.Sort(want)
			if !slices.Equal(documented, want) {
				t.Fatalf("README 记录的 MCP 工具 %v 与 AGENT.md 声明 %v 不一致", documented, want)
			}

			if findings := templateScanSecrets(t, filepath.Join(repo, "examples", "templates", tc.name)); len(findings) > 0 {
				t.Fatalf("%s 含凭据字面量（模板只能写 ${ENV_VAR} 占位）：%s", tc.name, strings.Join(findings, "；"))
			}
		})
	}

	// 逆改点（正反两向）：子集判据用一个必然不存在的名字走一遍，确认它不是恒真。
	// Discrimination check: the subset predicate must reject a name no face offers.
	t.Run("subset_check_discriminates", func(t *testing.T) {
		const absent = "template_tool_absent_from_every_face"
		if owners := templateToolOwners(absent, serverNames, faces); len(owners) != 0 {
			t.Fatalf("子集判据把不存在的工具 %q 判成了命中：%v——判据失效", absent, owners)
		}
	})
	// 逆改点：凭据扫描器必须抓到植入的样本，否则「八模板无凭据」是空断言。
	// The credential scanner must catch a planted sample or the clean result means nothing.
	t.Run("secret_scanner_discriminates", func(t *testing.T) {
		dir := t.TempDir()
		// 运行时拼样本，避免把「像密钥」的字面量写进源码本身。
		sample := "token: " + "gh" + "p_" + strings.Repeat("a1", 15) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "leak.md"), []byte(sample), 0o644); err != nil {
			t.Fatalf("写植入样本失败: %v", err)
		}
		if findings := templateScanSecrets(t, dir); len(findings) == 0 {
			t.Fatal("凭据扫描器没抓到植入的 GitHub token 样本——判据失效")
		}
	})
}

// TestTemplates_TierEvidence 判档位证据：文件表逐项非空（不是空壳）、README 的档位声明
// 与冻结档位逐字一致且不夹带别的档位、固定章节齐全、不出现笼统「已验证」徽章。
// A 不通过只能修复或保持未完成——README 里同时写两个档位正是「悄悄降 B」的形态，判红。
// TestTemplates_TierEvidence checks the tier evidence: the §3.6 file table is populated,
// the README quotes exactly one frozen tier, and no blanket "verified" badge appears.
func TestTemplates_TierEvidence(t *testing.T) {
	repo, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}
	counts := make(map[string]int, 3)
	for _, tc := range templateCatalog {
		counts[tc.tier]++

		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(repo, "examples", "templates", tc.name)
			for _, rel := range tc.files {
				// 存在且非空：空文件是「先占位后填写」的空壳，不算交付。
				templateReadText(t, filepath.Join(dir, filepath.FromSlash(rel)))
			}
			for _, pattern := range tc.globs {
				matches, globErr := filepath.Glob(filepath.Join(dir, filepath.FromSlash(pattern)))
				if globErr != nil {
					t.Fatalf("通配 %q 无效: %v", pattern, globErr)
				}
				if len(matches) == 0 {
					t.Fatalf("通配 %q 没有命中任何文件（夹具是空壳）", pattern)
				}
				for _, match := range matches {
					templateReadText(t, match)
				}
			}

			readme := templateReadText(t, filepath.Join(dir, "README.md"))
			want := "档位：" + tc.tier
			if !strings.Contains(readme, want) {
				t.Fatalf("README 缺少冻结档位声明 %q", want)
			}
			for _, other := range []string{templateTierA, templateTierAStar, templateTierB} {
				if other == tc.tier {
					continue
				}
				if strings.Contains(readme, "档位："+other) {
					t.Fatalf("README 同时写了另一个档位 %q——档位固定（§2.4.6），A 不通过只能修复或保持未完成，不得降 B", other)
				}
			}
			for _, section := range []string{
				"## 领域", "## 能力", "## 工具与域名", "## 文件范围", "## 凭据",
				"## 依赖", "## 档位", "## 证据状态", "## 已知限制",
			} {
				if !strings.Contains(readme, "\n"+section) {
					t.Fatalf("README 缺少章节 %q（§3.6 的固定包含项）", section)
				}
			}
			// 连接成功不等于业务可用（C-23）：笼统「已验证」徽章一律判红；
			// 正文里的「未实测 / 候选」不在此列。
			for _, badge := range []string{"**已验证**", "| 已验证 |"} {
				if strings.Contains(readme, badge) {
					t.Fatalf("README 出现笼统「已验证」表述 %q——必须写明具体验了什么、哪些仍是候选", badge)
				}
			}
		})
	}

	// 档位分布是 review ① 冻结的：6×A + 1×A* + 1×B。改动它 = 判据面变更。
	wantCounts := map[string]int{templateTierA: 6, templateTierAStar: 1, templateTierB: 1}
	for tier, want := range wantCounts {
		if counts[tier] != want {
			t.Fatalf("档位 %q 有 %d 个模板，期望 %d（§2.4.6 冻结的分布）", tier, counts[tier], want)
		}
	}

	// B 档必须逐字明写「未验证，不要当可运行示例」——这是 B 与 A 的分界线（§2.4.6）。
	funding := templateReadText(t, filepath.Join(repo, "examples", "templates", "funding-watch", "README.md"))
	const unverified = "未验证，不要当可运行示例"
	if !strings.Contains(funding, unverified) {
		t.Fatalf("funding-watch（B 档）README 缺少逐字声明 %q", unverified)
	}
}

// templateBuiltinTools 取候选二进制 `tool list` 报出的内置工具名集合：注册池的真值
// 来自装配层，测试不另存一份常量表（抄一份就会与装配漂移）。
// templateBuiltinTools reads the real registry names from the candidate binary.
func templateBuiltinTools(t *testing.T, h *harness) map[string]struct{} {
	t.Helper()
	initWorkspace(t, h)
	// provider 只是装配前置（注册池按已注册 provider 构建）；本用例不发起任何模型调用。
	h.writeConfig(t, configWithProvider("http://127.0.0.1:1"))
	res := h.run(t, fixtureEnv(), "tool", "list")
	requireExit(t, res, 0, "tool list（内置工具注册池）")
	names := make(map[string]struct{})
	for _, line := range strings.Split(res.Stdout, "\n") {
		name, _, ok := strings.Cut(line, "：")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if !templateToolNamePattern.MatchString(name) {
			continue
		}
		names[name] = struct{}{}
	}
	if len(names) == 0 {
		t.Fatalf("`tool list` 没有解析出任何工具名：\n%s", res.Stdout)
	}
	return names
}

// templateToolNamePattern 只认小写标识符形态的工具名：`共 N 件工具` 之类汇总行不含冒号，
// 但保留这道过滤，避免把帮助文案里的「名称：说明」误当工具。
var templateToolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// templateStage 把模板整棵装进工作区：Agent 定义进 agents/<name>，技能进共享技能库
// skills/<skill>（技能绑定只由 frontmatter 声明，实体必须在技能库里——规则 5）。
// templateStage copies one template into the workspace agents dir plus its skills.
func templateStage(t *testing.T, repo, workspaceDir, name string) {
	t.Helper()
	src := filepath.Join(repo, "examples", "templates", name)
	copyTree(t, src, filepath.Join(workspaceDir, "agents", name))

	skillsDir := filepath.Join(src, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("读 %s 的技能目录失败: %v", name, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		copyTree(t, filepath.Join(skillsDir, entry.Name()), filepath.Join(workspaceDir, "skills", entry.Name()))
	}
}

// templateMCPFaces 连上五家 MCP server 并返回各自 `tools/list` 的工具名集合：
// docs-readonly 用真实构建的二进制（真 tools/list），其余四家用规格 helper 服务
// README 记录的候选名。连接失败即判红——工具面判不了，模板闸门就不成立。
// templateMCPFaces connects the five servers and returns each tools/list name set.
func templateMCPFaces(t *testing.T, root, repo string) map[string]map[string]struct{} {
	t.Helper()
	helperBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("解析测试二进制路径失败: %v", err)
	}
	specDir := t.TempDir()
	serverNames := make([]string, 0, len(templateCandidateFaces))
	for name := range templateCandidateFaces {
		serverNames = append(serverNames, name)
	}
	slices.Sort(serverNames)

	specs := make([]core.MCPServerConfig, 0, len(serverNames)+1)
	for _, name := range serverNames {
		specPath := filepath.Join(specDir, name+".json")
		spec := testutil.MCPSpec{Tools: make([]testutil.MCPSpecTool, 0, len(templateCandidateFaces[name]))}
		for _, toolName := range templateCandidateFaces[name] {
			spec.Tools = append(spec.Tools, testutil.MCPSpecTool{
				Name:        toolName,
				Description: "候选工具面夹具（真实 server 未授权实连）",
			})
		}
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatalf("编码 %s 的规格失败: %v", name, err)
		}
		if err := os.WriteFile(specPath, raw, 0o644); err != nil {
			t.Fatalf("写 %s 的规格文件失败: %v", name, err)
		}
		specs = append(specs, core.MCPServerConfig{
			Name:      name,
			Transport: "stdio",
			Command:   helperBinary,
			Args:      []string{templateHelperArg},
			Env: map[string]string{
				"BASALT_MCP_HELPER":      "1",
				"BASALT_MCP_HELPER_SPEC": specPath,
			},
		})
	}

	docsRoot := filepath.Join(t.TempDir(), "docs")
	copyTree(t, filepath.Join(repo, "examples", "shared", "fixtures", "docs"), docsRoot)
	specs = append(specs, core.MCPServerConfig{
		Name:      templateDocsServer,
		Transport: "stdio",
		Command:   buildDocsReadOnlyBinary(t, repo),
		Args:      []string{"-root", docsRoot},
	})

	pool := tool.NewMCPPool(tool.MCPPoolOptions{
		Logger:         slog.New(slog.DiscardHandler),
		Root:           root,
		ConnectTimeout: 30 * time.Second,
		EnvPassthrough: []string{"PATH", "SystemRoot", "TEMP", "TMP", "USERPROFILE", "LOCALAPPDATA"},
	})
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("关闭 MCP 夹具池失败: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conns, failures, err := pool.ConnectDetailed(ctx, specs)
	if err != nil {
		t.Fatalf("连接 MCP 夹具失败: %v", err)
	}
	if len(failures) > 0 {
		details := make([]string, 0, len(failures))
		for _, failure := range failures {
			details = append(details, fmt.Sprintf("%s（stage=%s class=%s: %v）", failure.Server, failure.Stage, failure.Class, failure.Err))
		}
		t.Fatalf("MCP 夹具未连上：%s", strings.Join(details, "；"))
	}
	faces := make(map[string]map[string]struct{}, len(conns))
	for _, conn := range conns {
		face := make(map[string]struct{}, len(conn.Tools))
		for _, entry := range conn.Tools {
			face[entry.Name()] = struct{}{}
		}
		faces[conn.Server] = face
	}
	return faces
}

// templateToolOwners 返回 tools/list 含该工具名的**已声明** server（升序）。
// templateToolOwners lists the declared servers whose tools/list offers the tool.
func templateToolOwners(name string, declared []string, faces map[string]map[string]struct{}) []string {
	owners := make([]string, 0, 1)
	for _, server := range declared {
		if _, ok := faces[server][name]; ok {
			owners = append(owners, server)
		}
	}
	slices.Sort(owners)
	return owners
}

// templateFaceSummary 把各 server 的工具面渲染成一行，失败时直接看出「面里到底有什么」。
func templateFaceSummary(servers []string, faces map[string]map[string]struct{}) string {
	parts := make([]string, 0, len(servers))
	for _, server := range servers {
		parts = append(parts, fmt.Sprintf("%s=%v", server, templateSortedFace(faces[server])))
	}
	return strings.Join(parts, " ")
}

// templateSortedFace 是工具面集合的稳定序视图（map 遍历序随机，判卷与报错都要确定）。
func templateSortedFace(face map[string]struct{}) []string {
	names := make([]string, 0, len(face))
	for name := range face {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// templateReadmeMCPToolRow 取「工具与域名」表里 `MCP 工具名` 那一行。只认单元格**恰好**
// 是「MCP 工具名」的行——证据状态表里的「MCP 工具名是 DBHub tools/list 的子集」是另一行，
// 不得混进来。
var templateReadmeMCPToolRow = regexp.MustCompile(`(?m)^\|\s*MCP 工具名\s*\|(.+)\|\s*$`)

// templateBacktickName 只认反引号里的工具名：单元格同时会写 `tools/list`（带斜杠，不是
// 工具名）与普通说明文字，两者都不能进集合。
var templateBacktickName = regexp.MustCompile("`([a-z][a-z0-9_]*)`")

// templateReadmeMCPTools 解析 README 记录的 MCP 工具名（升序去重）；未声明 MCP 的模板
// 没有这一行，返回 nil——调用方按「空集合」比对。
// templateReadmeMCPTools parses the documented MCP tool names from the README table.
func templateReadmeMCPTools(t *testing.T, repo, name string) []string {
	t.Helper()
	readme := templateReadText(t, filepath.Join(repo, "examples", "templates", name, "README.md"))
	match := templateReadmeMCPToolRow.FindStringSubmatch(readme)
	if match == nil {
		return nil
	}
	names := make([]string, 0, 2)
	for _, hit := range templateBacktickName.FindAllStringSubmatch(match[1], -1) {
		if !slices.Contains(names, hit[1]) {
			names = append(names, hit[1])
		}
	}
	slices.Sort(names)
	return names
}

// templateSecretPatterns 是凭据字面量的判据集：只认可证明的形态，不做熵值猜测
// （猜出来的命中会逼着模板作者删正常文字）。
// templateSecretPatterns matches provable credential shapes only, never entropy guesses.
var templateSecretPatterns = []struct {
	label   string
	pattern *regexp.Regexp
}{
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`)},
	{"OpenAI 风格 key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{16,}`)},
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"私钥块", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
}

// templateURLCredentialPattern 单独抓 URL 内嵌凭据，因为要按主机名过滤：RFC 2606 保留的
// `example.com/.net/.org` 是文档占位域，指向它们的 `user:password@` 是"必须拒绝带凭据
// 的 URL"这类反例夹具，不是可用的凭据。其余主机照旧判红，避免留下真正的泄漏盲区。
// The URL-credential probe is separate so documentation hosts (RFC 2606) can be excluded;
// those appear as rejection fixtures, while any other host still fails the scan.
var templateURLCredentialPattern = regexp.MustCompile(`https?://[^/\s:@]+:[^/\s@]+@([^/\s:?#]+)`)

// templateDocumentationHost 判断 URL 凭据是否指向文档占位域。
// templateDocumentationHost reports whether the host is an RFC 2606 placeholder domain.
func templateDocumentationHost(host string) bool {
	lower := strings.ToLower(host)
	for _, suffix := range []string{"example.com", "example.net", "example.org"} {
		if lower == suffix || strings.HasSuffix(lower, "."+suffix) {
			return true
		}
	}
	return false
}

// templateScanSecrets 逐文件扫一遍模板目录，返回「相对路径：命中类别」（升序）。
// 模板会被复制到企业工作区并可能随仓库公开，凭据只能以 ${ENV_VAR} 占位出现。
// templateScanSecrets returns "relpath: label" hits for provable credential shapes.
func templateScanSecrets(t *testing.T, dir string) []string {
	t.Helper()
	var findings []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		text := string(raw)
		for _, probe := range templateSecretPatterns {
			if probe.pattern.MatchString(text) {
				findings = append(findings, filepath.ToSlash(rel)+"："+probe.label)
			}
		}
		for _, match := range templateURLCredentialPattern.FindAllStringSubmatch(text, -1) {
			if templateDocumentationHost(match[1]) {
				continue
			}
			findings = append(findings, filepath.ToSlash(rel)+"：URL 内嵌凭据")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 %s 失败: %v", filepath.ToSlash(dir), err)
	}
	slices.Sort(findings)
	return findings
}

// templateReadText 读一个必须存在且非空的文件；空文件按空壳判红。
// templateReadText reads a file that must exist and carry content.
func templateReadText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", filepath.ToSlash(path), err)
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		t.Fatalf("%s 是空文件（空壳）", filepath.ToSlash(path))
	}
	return text
}
