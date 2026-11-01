package dbmigrate

// 本文件是**内置 Agent 资产白名单**的纯单元判卷（不依赖 PostgreSQL，也不跑 Python）：
// 五只 Agent 一只都不能从清单消失、路径与类别符合产品布局、真实安装出来的工作区不产生
// 未归类文件、凭据值不进清单。需要真实库与真实名册的取证在 CLI 层完成（T070）。
// Unit checks for BuiltinAssetSpecs: five agents are covered, paths and kinds follow the
// product layout, a really installed workspace yields no unclassified files, and no
// credential value can reach the manifest.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/builtin"
)

// builtinAssetExpectedBusinessFiles 是颗粒度文档 §2.13 图 11 的 FS 行：每只 Agent 在自己
// 目录根下的业务输入 / 状态文件。**单独抄一份**（不从实现里取）——两份不一致才说明实现
// 漏了文件，照抄实现等于把测试写成自证。
// The per-agent root-level business files from the design document, copied here so that a
// missing entry in the implementation turns this test red.
var builtinAssetExpectedBusinessFiles = map[string][]string{
	"biz-daily":      {"orders.json"},
	"docs-diff":      {"documents.json", "metadata.json"},
	"github-radar":   {"github_search.json"},
	"industry-brief": {"search.json", "pages.json"},
	"repo-digest":    {"pull_requests.json", "issues.json", "releases.json"},
}

// findBuiltinAssetSpec 按路径取一条白名单声明；不存在返回零值与 false。
func findBuiltinAssetSpec(specs []AssetSpec, rel string) (AssetSpec, bool) {
	for _, spec := range specs {
		if spec.Path == rel {
			return spec, true
		}
	}
	return AssetSpec{}, false
}

// TestBuiltinAssetSpecs_CoversFiveAgents 守住 R27 的第一条：五只内置 Agent 一只都不能从
// 清单消失，且每只都有定义 / 脚本 / 夹具 / 状态 / 报表五类声明。
// 逆改点：删掉任意一只 Agent 的声明 → 对应 RequiredFor 缺失，本用例红。
func TestBuiltinAssetSpecs_CoversFiveAgents(t *testing.T) {
	specs := BuiltinAssetSpecs()
	agents := builtin.Agents()
	require.Len(t, agents, 5, "内置 Agent 清单本身变了，先核对设计")

	covered := make(map[string]struct{})
	for _, spec := range specs {
		for _, agent := range spec.RequiredFor {
			covered[agent] = struct{}{}
		}
	}
	require.Len(t, covered, len(agents), "RequiredFor 里的 Agent 名数量必须与内置清单一致")

	for _, agent := range agents {
		name := agent.Name
		require.Contains(t, covered, name, "五个内置 Agent 一个都不能从清单消失：%s", name)
		base := "agents/" + name
		for rel, wantKind := range map[string]string{
			base:                               AssetKindAgentDef,
			base + "/scripts":                  AssetKindScript,
			base + "/fixtures":                 AssetKindFixture,
			base + "/state":                    AssetKindState,
			base + "/runs":                     AssetKindReport,
			"control/agents/" + name + ".json": AssetKindState,
		} {
			spec, ok := findBuiltinAssetSpec(specs, rel)
			require.True(t, ok, "%s 缺少声明：%s", name, rel)
			require.Equal(t, wantKind, spec.Kind, "%s 的类别不符：%s", rel, name)
			require.Contains(t, spec.RequiredFor, name, "%s 的 RequiredFor 必须点名 %s", rel, name)
		}
	}
}

// TestBuiltinAssetSpecs_BusinessStatePerAgent 守住七项清单里的"业务状态"：每只 Agent 的
// 根级业务文件都在白名单里，且按 `state` 归类（**不得**落进目录默认的 agent-def）。
// 逆改点：删掉某只 Agent 的一条业务文件声明 → 本用例按设计文档清单报红。
func TestBuiltinAssetSpecs_BusinessStatePerAgent(t *testing.T) {
	specs := BuiltinAssetSpecs()
	for _, agent := range builtin.Agents() {
		expected, ok := builtinAssetExpectedBusinessFiles[agent.Name]
		require.True(t, ok, "设计文档里有 %s 的业务文件清单，测试表不能缺", agent.Name)
		for _, name := range expected {
			rel := "agents/" + agent.Name + "/" + name
			spec, found := findBuiltinAssetSpec(specs, rel)
			require.True(t, found, "业务状态文件缺声明：%s", rel)
			require.Equal(t, AssetKindState, spec.Kind, "业务状态文件的类别必须是 state：%s", rel)
			require.Contains(t, spec.RequiredFor, agent.Name, "业务状态文件必须点名归属 Agent：%s", rel)
		}
	}
}

// TestBuiltinAssetSpecs_KindsMatchLayout 逐类核对产品布局：引导文件与技能是定义、
// 样例库与文档输入分开、Demo 生成物各有归属。表里刻意混入"不该出现"的组合
// （如把样例库写成 agent-def）——顺序写错、类别抄错都会在这里红。
func TestBuiltinAssetSpecs_KindsMatchLayout(t *testing.T) {
	specs := BuiltinAssetSpecs()
	allAgents := make([]string, 0, 5)
	for _, agent := range builtin.Agents() {
		allAgents = append(allAgents, agent.Name)
	}

	cases := []struct {
		rel         string
		kind        string
		requiredFor []string // nil = 不校验（实例级资产不归属单只 Agent）
	}{
		{"AGENTS.md", AssetKindAgentDef, allAgents},
		{"SOUL.md", AssetKindAgentDef, allAgents},
		{"USER.md", AssetKindAgentDef, allAgents},
		{"mcp_servers.yaml", AssetKindAgentDef, allAgents},
		{"memory", AssetKindState, nil},
		{"archive", AssetKindState, nil},
		{"versions", AssetKindState, nil},
		{"control", AssetKindState, nil},
		{"agents", AssetKindAgentDef, nil},
		{"skills", AssetKindAgentDef, nil},
		{"skills/repo-digest", AssetKindAgentDef, []string{"repo-digest"}},
		{"samples/biz-sample.db", AssetKindSampleDB, []string{"biz-daily"}},
		{"samples/docs", AssetKindDocInput, []string{"docs-diff"}},
		{"demos/install.json", AssetKindState, nil},
		{"demos/scripts", AssetKindScript, []string{"biz-daily"}},
		{"demos/mcp/dbhub.toml", AssetKindState, []string{"biz-daily"}},
		{"demos/src", AssetKindScript, nil},
		// 最长前缀优先：`agents/biz-daily` 是定义，它的 runs/ 是报表而不是定义。
		{"agents/biz-daily", AssetKindAgentDef, []string{"biz-daily"}},
		{"agents/biz-daily/runs", AssetKindReport, []string{"biz-daily"}},
	}
	for _, tc := range cases {
		spec, ok := findBuiltinAssetSpec(specs, tc.rel)
		require.True(t, ok, "白名单缺声明：%s", tc.rel)
		require.Equal(t, tc.kind, spec.Kind, "类别不符：%s", tc.rel)
		if tc.requiredFor != nil {
			require.ElementsMatch(t, tc.requiredFor, spec.RequiredFor, "RequiredFor 不符：%s", tc.rel)
		}
	}
}

// TestBuiltinAssetSpecs_PathSafety 守住 T068 的路径纪律：整份白名单必须能被
// `normalizeSpecs` 接受（相对路径、正斜杠、无 `..` / 盘符 / UNC / 重复 / 排除项命中），
// 且**不得**指向仓库模板 `examples/**`。
// 逆改点：把任一路径改成绝对路径 / 反斜杠 / 重复条目 / `logs/...` → 整份规格被拒，本用例红。
func TestBuiltinAssetSpecs_PathSafety(t *testing.T) {
	specs := BuiltinAssetSpecs()
	require.NotEmpty(t, specs, "空白名单会把整棵工作区都当未归类")
	_, err := normalizeSpecs(specs)
	require.NoError(t, err, "白名单必须整体合法")

	for _, spec := range specs {
		require.NotEmpty(t, spec.Path)
		require.NotContains(t, spec.Path, `\`, "路径必须用正斜杠：%s", spec.Path)
		require.NotContains(t, spec.Path, "..", "路径不得包含 ..：%s", spec.Path)
		require.False(t, filepath.IsAbs(spec.Path), "路径必须相对工作区根：%s", spec.Path)
		require.NotEqual(t, "examples", spec.Path, "仓库模板不是实例资产")
		require.NotContains(t, spec.Path+"/", "examples/", "仓库模板不是实例资产：%s", spec.Path)
		require.NotContains(t, "/sessions", spec.Path, "sessions/ 是骨架目录，留给未归类核查")
	}
}

// TestBuiltinAssetSpecs_InstalledWorkspaceHasNoUnclassified 是判别力最强的一条：按
// `internal/builtin` 的**真实安装布局**铺一个工作区（五份定义 + 技能 + 样例 + Demo 生成物），
// 用 `BuiltinAssetSpecs()` 采集后不得出现任何未归类文件——白名单漏一个真实存在的文件，
// 这里就红（`ReviewStatus` 变 pending、`UnclassifiedDigest` 非空）。
// 逆改点：把 fixtures 拼成 fixture、把 samples/docs 写成 samples/doc → 真实文件掉进未归类。
func TestBuiltinAssetSpecs_InstalledWorkspaceHasNoUnclassified(t *testing.T) {
	root := t.TempDir()
	results, err := builtin.Install(root, false, "")
	require.NoError(t, err)
	require.Len(t, results, 5, "安装器必须铺出五只 Agent")

	// 样例库与样例文档库：生成脚本需要 Python，这里直接铺等价落点（采集只看文件本身）。
	assetWriteFile(t, root, "samples/biz-sample.db", "SQLite format 3\x00")
	_, err = builtin.EnsureDocsLibrary(root, false)
	require.NoError(t, err)

	// Demo 生成物：DBHub 只读配置、样例库脚本与安装清单。
	assetWriteFile(t, root, "demos/mcp/dbhub.toml", "[[sources]]\ndsn = \"placeholder\"\n")
	assetWriteFile(t, root, "demos/scripts/make_sample_db.py", "print('placeholder')\n")
	assetWriteFile(t, root, "demos/install.json", `{"demos":[]}`)
	assetWriteFile(t, root, "demos/src/docs-readonly/main.go", "package main\n")

	// 每只 Agent 的业务状态：根级输入、state/ 与 runs/ 报告。
	for _, agent := range builtin.Agents() {
		base := "agents/" + agent.Name + "/"
		for _, name := range builtinAssetExpectedBusinessFiles[agent.Name] {
			assetWriteFile(t, root, base+name, "{}\n")
		}
		assetWriteFile(t, root, base+"state/seen.json", "{}\n")
		assetWriteFile(t, root, base+"runs/2026-10-26/report.json", "{}\n")
		assetWriteFile(t, root, "control/agents/"+agent.Name+".json", `{"state":"disabled"}`+"\n")
	}
	assetWriteFile(t, root, "memory/MEMORY.md", "memory\n")

	manifest, err := BuildAssetManifest(context.Background(), root, BuiltinAssetSpecs())
	require.NoError(t, err)
	require.Empty(t, manifest.UnclassifiedDigest, "白名单漏了真实存在的文件时这里非空")
	require.Equal(t, AssetReviewNotRequired, manifest.ReviewStatus)
	require.NotEmpty(t, manifest.Entries, "清单为空即未真正盘点")

	agents := manifest.Agents()
	require.Len(t, agents, 5)
	for _, agent := range builtin.Agents() {
		require.Contains(t, agents, agent.Name, "五个内置 Agent 一个都不能从清单消失：%s", agent.Name)
	}
}

// TestBuiltinAssetSpecs_NoCredentialValues 守住"env 只核对变量名"：采集结果里的任何字段
// 都**不得**出现凭据值。用一个哨兵值（不是真凭据）走一遍完整采集与 JSON 序列化。
// 逆改点：把 env 变量值写进 RequiredFor / 条目字段 → 哨兵值出现在清单 JSON 里，本用例红。
func TestBuiltinAssetSpecs_NoCredentialValues(t *testing.T) {
	const sentinel = "builtin-assets-test-sentinel-value"
	t.Setenv("GITHUB_PERSONAL_ACCESS_TOKEN", sentinel)

	root := t.TempDir()
	_, err := builtin.Install(root, false, "")
	require.NoError(t, err)
	assetWriteFile(t, root, "agents/github-radar/github_search.json", "{}\n")

	manifest, err := BuildAssetManifest(context.Background(), root, BuiltinAssetSpecs())
	require.NoError(t, err)

	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), sentinel, "凭据值不得进入资产清单")
}

// TestAssets_BusinessStateHashesMatch 守颗粒度文档 §4.3 C-40 的"业务状态逐文件可核对"：
// 清单里每一条业务状态条目（根级输入、`state/seen.json`、`runs/` 报告）记的尺寸与
// SHA-256 必须等于**盘上文件的实测值**。只声明路径不够——哈希若没算或算错，恢复后
// "资产齐全"就是一句没有取证的自述。
//
// 逆改点：采集时跳过业务状态文件的哈希（写空串 / 用尺寸代替摘要）→ 逐条断言红并点名文件。
func TestAssets_BusinessStateHashesMatch(t *testing.T) {
	root := t.TempDir()
	_, err := builtin.Install(root, false, "")
	require.NoError(t, err)

	for _, agent := range builtin.Agents() {
		base := "agents/" + agent.Name + "/"
		for _, name := range builtinAssetExpectedBusinessFiles[agent.Name] {
			assetWriteFile(t, root, base+name, `{"agent":"`+agent.Name+`","file":"`+name+`"}`+"\n")
		}
		assetWriteFile(t, root, base+"state/seen.json", `{"seen":["`+agent.Name+`"]}`+"\n")
		assetWriteFile(t, root, base+"runs/2026-10-26/report.json", `{"report":"`+agent.Name+`"}`+"\n")
	}

	manifest, err := BuildAssetManifest(context.Background(), root, BuiltinAssetSpecs())
	require.NoError(t, err, "内置安装 + 业务状态文件必须能完整盘点")

	checked := 0
	for _, entry := range manifest.Entries {
		if entry.Kind != AssetKindState {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.RelPath)))
		require.NoError(t, err, "清单条目必须对应盘上文件：%s", entry.RelPath)
		sum := sha256.Sum256(raw)
		require.Equal(t, hex.EncodeToString(sum[:]), entry.SHA256,
			"业务状态文件的 SHA-256 必须是实测值：%s", entry.RelPath)
		require.Equal(t, int64(len(raw)), entry.Size,
			"业务状态文件的尺寸必须是实测值：%s", entry.RelPath)
		checked++
	}
	require.Greater(t, checked, 5, "五只 Agent 的业务状态文件都应进清单（实际核对 %d 条）", checked)
}

// TestAssets_DisabledAgentStillListed 守颗粒度文档 §4.3 C-40 的「停用的 Agent 仍须在
// 清单里」：控制面把 biz-daily 标成 disabled 之后，资产盘点不得按启用状态过滤——迁移
// 是把实例**原样**搬过去，停用只说明它不跑，不说明它没数据；漏采就等于迁移后再也回答
// 不了"这只 Agent 曾经存在过、有过哪些业务状态"。
//
// 逆改点：采集时按 control/agents 的 state 跳过条目 → 该 Agent 的控制面与业务文件从
// 清单消失，本用例按路径点名红。
// TestAssets_DisabledAgentStillListed: a disabled agent must still be inventoried.
func TestAssets_DisabledAgentStillListed(t *testing.T) {
	const agent = "biz-daily"
	root := t.TempDir()
	_, err := builtin.Install(root, false, "")
	require.NoError(t, err)
	assetWriteFile(t, root, "control/agents/"+agent+".json", `{"state":"disabled"}`+"\n")
	assetWriteFile(t, root, "agents/"+agent+"/orders.json", `{"orders":[]}`+"\n")

	manifest, err := BuildAssetManifest(context.Background(), root, BuiltinAssetSpecs())
	require.NoError(t, err, "停用的 Agent 不得让整份盘点失败")

	require.Contains(t, manifest.Agents(), agent, "停用的 Agent 不得从清单消失")
	control := assetEntry(t, manifest, "control/agents/"+agent+".json")
	require.Equal(t, AssetKindState, control.Kind)
	require.NotEmpty(t, control.SHA256, "停用状态文件也要留下哈希，供恢复后对账")

	business := assetEntry(t, manifest, "agents/"+agent+"/orders.json")
	require.NotEmpty(t, business.SHA256, "停用 Agent 的业务状态同样要盘点")
}
