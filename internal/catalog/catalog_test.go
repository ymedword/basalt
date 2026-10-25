package catalog_test

// 本文件是 US-14 只读 MCP 目录的验收用例（颗粒度文档 §3.1.1 的四个用例名逐字保留）。
// 全部经**导出**入口 `catalog.Load` / `catalog.Builtin` 断言，不新增测试专用导出 API；
// `TestCatalog_RecipesParse` 把目录条目渲染成 `mcp_servers.yaml` 片段后交给既有
// `workspace.ParseMCPServerBlock`——目录自洽不等于配置可装载，这一条把两者钉在一起。
// Acceptance tests for the read-only MCP catalog. Everything goes through the exported
// Load / Builtin entry points; the recipes case renders each entry into a
// mcp_servers.yaml fragment and parses it with the existing workspace parser.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/catalog"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// alphaItemYAML 是测试用的合法条目（stdio，带一条 env 占位）。
// alphaItemYAML is one valid stdio entry with a single env placeholder.
const alphaItemYAML = `  - id: alpha-tool
    display_name: Alpha
    summary: 第一份测试配方。
    category: 测试 / 甲
    transport: stdio
    command: alpha-mcp
    args: ["--flag"]
    url: ""
    env:
      - key: ALPHA_KEY
        placeholder: "${ALPHA_KEY}"
        required: true
        description: Alpha 的 key。
    requires:
      - Alpha 包
    source_url: https://example.com/alpha
    verified: false
    verification_note: 待实测。
    verified_at: ""
    verified_platforms: []
    warnings:
      - package_entry_unverified
`

// zetaItemYAML 是第二条合法条目（id 排在 alpha 之后），用于断言输出按 id 升序。
// zetaItemYAML is the second valid entry, used to assert id-ascending output.
const zetaItemYAML = `  - id: zeta-tool
    display_name: Zeta
    summary: 第二份测试配方。
    category: 测试 / 乙
    transport: stdio
    command: zeta-mcp
    args: []
    url: ""
    env: []
    requires:
      - Zeta 包
    source_url: https://example.com/zeta
    verified: false
    verification_note: 待实测。
    verified_at: ""
    verified_platforms: []
    warnings: []
`

// httpItemYAML 是合法的 http 传输条目：command / args 必须为空，url 必须 https。
// httpItemYAML is the valid http entry: no command / args, https url only.
const httpItemYAML = `  - id: remote-docs
    display_name: Remote Docs
    summary: 远程文档服务。
    category: 测试 / 丙
    transport: http
    command: ""
    args: []
    url: "https://example.com/mcp"
    env: []
    requires:
      - 远程文档服务地址
    source_url: https://example.com/docs
    verified: false
    verification_note: 待实测。
    verified_at: ""
    verified_platforms: []
    warnings: []
`

// catalogDoc 拼接一份目录文档。
// catalogDoc assembles one catalog document from item fragments.
func catalogDoc(items ...string) string {
	var b strings.Builder
	b.WriteString("version: 1\nitems:\n")
	for _, item := range items {
		b.WriteString(item)
	}
	return b.String()
}

// TestCatalog_Load：合法文档产出按 id 升序的快照与逐字段映射，空数组规范化为 []；
// 每个校验拒绝分支各有一次红态（错误文案点名条目与字段）。
// TestCatalog_Load covers the happy path and every rejection branch.
func TestCatalog_Load(t *testing.T) {
	t.Run("合法文档按 id 升序", func(t *testing.T) {
		// 故意把 zeta 放在 alpha 前面：排序是解析层职责，视图层不再排序。
		loaded, err := catalog.Load([]byte(catalogDoc(zetaItemYAML, alphaItemYAML)))
		require.NoError(t, err)
		require.Equal(t, 1, loaded.Version)
		require.Len(t, loaded.Items, 2)
		require.Equal(t, "alpha-tool", loaded.Items[0].ID)
		require.Equal(t, "zeta-tool", loaded.Items[1].ID)

		alpha := loaded.Items[0]
		require.Equal(t, "Alpha", alpha.DisplayName)
		require.Equal(t, "第一份测试配方。", alpha.Summary)
		require.Equal(t, "测试 / 甲", alpha.Category)
		require.Equal(t, "stdio", alpha.Transport)
		require.Equal(t, "alpha-mcp", alpha.Command)
		require.Equal(t, []string{"--flag"}, alpha.Args)
		require.Empty(t, alpha.URL)
		require.Equal(t, []core.MCPCatalogEnv{{
			Key:         "ALPHA_KEY",
			Placeholder: "${ALPHA_KEY}",
			Required:    true,
			Description: "Alpha 的 key。",
		}}, alpha.Env)
		require.Equal(t, []string{"Alpha 包"}, alpha.Requires)
		require.Equal(t, "https://example.com/alpha", alpha.SourceURL)
		require.False(t, alpha.Verified)
		require.Equal(t, "待实测。", alpha.VerificationNote)
		require.Empty(t, alpha.VerifiedAt)
		require.Empty(t, alpha.VerifiedPlatforms)
		require.Equal(t, []string{"package_entry_unverified"}, alpha.Warnings)

		// 空数组必须是 []（非 nil），视图才能恒输出 `[]`。
		zeta := loaded.Items[1]
		require.NotNil(t, zeta.Args)
		require.Empty(t, zeta.Args)
		require.NotNil(t, zeta.Env)
		require.Empty(t, zeta.Env)
		require.NotNil(t, zeta.Warnings)
		require.Empty(t, zeta.Warnings)
	})

	t.Run("http 传输合法", func(t *testing.T) {
		loaded, err := catalog.Load([]byte(catalogDoc(httpItemYAML)))
		require.NoError(t, err)
		require.Len(t, loaded.Items, 1)
		entry := loaded.Items[0]
		require.Equal(t, "http", entry.Transport)
		require.Empty(t, entry.Command)
		require.Equal(t, "https://example.com/mcp", entry.URL)
	})

	rejects := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name:    "版本为零",
			doc:     strings.Replace(catalogDoc(alphaItemYAML), "version: 1", "version: 0", 1),
			wantErr: "version 必须为正整数",
		},
		{
			name:    "缺少版本",
			doc:     strings.Replace(catalogDoc(alphaItemYAML), "version: 1\n", "", 1),
			wantErr: "version 必须为正整数",
		},
		{
			name:    "YAML 语法错误",
			doc:     "version: 1\nitems: [\n",
			wantErr: "解析 MCP 目录",
		},
		{
			name: "未知顶层字段",
			doc:  "version: 1\nextra: true\n" + "items:\n" + alphaItemYAML,
			// 严格解码拒绝未知键：错误由解析层包装。
			wantErr: "解析 MCP 目录",
		},
		{
			name: "未知条目字段",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    summary: 第一份测试配方。\n", "    summary: 第一份测试配方。\n    typo_field: 1\n", 1)),
			wantErr: "解析 MCP 目录",
		},
		{
			name: "id 非 kebab-case",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "id: alpha-tool", "id: Alpha_Tool", 1)),
			wantErr: "kebab-case",
		},
		{
			name: "缺少 display_name",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    display_name: Alpha\n", "", 1)),
			wantErr: "缺少 display_name",
		},
		{
			name: "缺少 summary",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    summary: 第一份测试配方。\n", "", 1)),
			wantErr: "缺少 summary",
		},
		{
			name: "缺少 category",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    category: 测试 / 甲\n", "", 1)),
			wantErr: "缺少 category",
		},
		{
			name: "transport 不在闭集",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    transport: stdio\n", "    transport: sse\n", 1)),
			wantErr: "transport 不受支持",
		},
		{
			name: "stdio 缺少 command",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    command: alpha-mcp\n", "", 1)),
			wantErr: "必须给出 command",
		},
		{
			name: "stdio 给出 url",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    url: \"\"\n", "    url: \"https://example.com/alpha\"\n", 1)),
			wantErr: "不得给出 url",
		},
		{
			name: "http 给出 command",
			doc: catalogDoc(strings.Replace(
				httpItemYAML, "    command: \"\"\n", "    command: \"npx\"\n", 1)),
			wantErr: "不得给出 command",
		},
		{
			name: "http 给出 args",
			doc: catalogDoc(strings.Replace(
				httpItemYAML, "    args: []\n", "    args: [\"--transport\"]\n", 1)),
			wantErr: "不得给出 args",
		},
		{
			name: "http url 非 https",
			doc: catalogDoc(strings.Replace(
				httpItemYAML, "url: \"https://example.com/mcp\"", "url: \"http://example.com/mcp\"", 1)),
			wantErr: "https 绝对地址",
		},
		{
			name: "http url 不是绝对地址",
			doc: catalogDoc(strings.Replace(
				httpItemYAML, "url: \"https://example.com/mcp\"", "url: \"example.com/mcp\"", 1)),
			wantErr: "https 绝对地址",
		},
		{
			name: "requires 为空",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    requires:\n      - Alpha 包\n", "    requires: []\n", 1)),
			wantErr: "requires 至少要有一条",
		},
		{
			name: "source_url 非 https",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "source_url: https://example.com/alpha", "source_url: http://example.com/alpha", 1)),
			wantErr: "source_url 必须是带主机名的 https 绝对地址",
		},
		{
			name: "warnings 不在目录闭集",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "      - package_entry_unverified\n", "      - custom_code\n", 1)),
			wantErr: "不在目录固定集合内",
		},
		{
			name: "verified=false 缺少说明",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    verification_note: 待实测。\n", "    verification_note: \"\"\n", 1)),
			wantErr: "必须给出 verification_note",
		},
		{
			name: "verified=false 仍带时间",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    verified_at: \"\"\n", "    verified_at: \"2026-10-25T00:00:00Z\"\n", 1)),
			wantErr: "verified_at 必须为空",
		},
		{
			name: "verified=false 仍带平台",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    verified_platforms: []\n", "    verified_platforms: [\"linux\"]\n", 1)),
			wantErr: "verified_platforms 必须为空数组",
		},
		{
			name: "verified=true 缺少时间",
			doc: catalogDoc(strings.Replace(
				strings.Replace(alphaItemYAML, "    verified: false\n", "    verified: true\n", 1),
				"    verified_platforms: []\n", "    verified_platforms: [\"linux\"]\n", 1)),
			wantErr: "必须给出 verified_at",
		},
		{
			name: "verified=true 缺少平台",
			doc: catalogDoc(strings.Replace(
				strings.Replace(alphaItemYAML, "    verified: false\n", "    verified: true\n", 1),
				"    verified_at: \"\"\n", "    verified_at: \"2026-10-25T00:00:00Z\"\n", 1)),
			wantErr: "必须给出 verified_platforms",
		},
		{
			name: "平台不在闭集",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "    verified_platforms: []\n", "    verified_platforms: [\"solaris\"]\n", 1)),
			wantErr: "verified_platforms[0] 不受支持",
		},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			_, err := catalog.Load([]byte(tc.doc))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestCatalog_UniqueIDs：重复 id 整份拒绝；内置目录的 id 唯一且六个都在。
// TestCatalog_UniqueIDs rejects duplicate ids and pins the built-in id set.
func TestCatalog_UniqueIDs(t *testing.T) {
	duplicate := catalogDoc(
		alphaItemYAML,
		strings.Replace(alphaItemYAML, "display_name: Alpha", "display_name: Alpha 二号", 1),
	)
	_, err := catalog.Load([]byte(duplicate))
	require.Error(t, err)
	require.Contains(t, err.Error(), "id 重复")

	builtin, err := catalog.Builtin()
	require.NoError(t, err)
	ids := make([]string, 0, len(builtin.Items))
	for _, item := range builtin.Items {
		ids = append(ids, item.ID)
	}
	require.Equal(t, []string{
		"brave-search", "dbhub", "fetch", "filesystem-readonly", "github", "playwright",
	}, ids)
}

// TestCatalog_EnvPlaceholders：placeholder 必须恰为 `${key}` 且同一条目内键不重复；
// 内置目录的每条占位都自洽。目录只声明键名，**不携带任何值**。
// TestCatalog_EnvPlaceholders pins the ${KEY} equality and key uniqueness rules.
func TestCatalog_EnvPlaceholders(t *testing.T) {
	rejects := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{
			name: "占位符指向别的键",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "placeholder: \"${ALPHA_KEY}\"", "placeholder: \"${BETA_KEY}\"", 1)),
			wantErr: "placeholder 必须恰为 ${ALPHA_KEY}",
		},
		{
			name: "占位符不是 ${KEY} 形态",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML, "placeholder: \"${ALPHA_KEY}\"", "placeholder: \"ALPHA_KEY\"", 1)),
			wantErr: "placeholder 必须恰为 ${ALPHA_KEY}",
		},
		{
			name: "env 键重复",
			doc: catalogDoc(strings.Replace(
				alphaItemYAML,
				"    requires:\n",
				"      - key: ALPHA_KEY\n        placeholder: \"${ALPHA_KEY}\"\n        required: false\n        description: 重复。\n    requires:\n",
				1)),
			wantErr: "env 键 ALPHA_KEY 重复",
		},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			_, err := catalog.Load([]byte(tc.doc))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("内置目录占位自洽", func(t *testing.T) {
		builtin, err := catalog.Builtin()
		require.NoError(t, err)
		for _, item := range builtin.Items {
			seen := make(map[string]bool, len(item.Env))
			for _, env := range item.Env {
				require.Equal(t, "${"+env.Key+"}", env.Placeholder, "条目 %s", item.ID)
				require.False(t, seen[env.Key], "条目 %s 的 env 键 %s 重复", item.ID, env.Key)
				seen[env.Key] = true
			}
		}
	})
}

// TestCatalog_RecipesParse：每条内置配方都能渲染成 `mcp_servers.yaml` 片段并经既有
// `workspace.ParseMCPServerBlock` 装载——目录自洽与真实配置语法必须是同一套规则。
// TestCatalog_RecipesParse renders every built-in recipe into a config fragment and
// parses it with the existing parser, so catalog and config syntax cannot diverge.
func TestCatalog_RecipesParse(t *testing.T) {
	builtin, err := catalog.Builtin()
	require.NoError(t, err)
	require.NotEmpty(t, builtin.Items)

	for _, item := range builtin.Items {
		t.Run(item.ID, func(t *testing.T) {
			env := make(map[string]string, len(item.Env))
			for _, entry := range item.Env {
				env[entry.Key] = entry.Placeholder
			}
			config := core.MCPServerConfig{
				Name:      item.ID,
				Transport: item.Transport,
				Command:   item.Command,
				Args:      item.Args,
				URL:       item.URL,
				Env:       env,
			}
			block, err := workspace.RenderMCPServerEntry(config)
			require.NoError(t, err)

			parsed, err := workspace.ParseMCPServerBlock(block)
			require.NoError(t, err)
			require.Equal(t, config.Name, parsed.Name)
			require.Equal(t, config.Transport, parsed.Transport)
			require.Equal(t, config.Command, parsed.Command)
			require.Equal(t, config.Args, parsed.Args)
			require.Equal(t, config.URL, parsed.URL)
			// 无 env 的配方解析后是 nil 映射——空映射与 nil 在配置语义上等价。
			// A recipe without env parses to a nil map; both mean "no env".
			if len(config.Env) == 0 {
				require.Empty(t, parsed.Env)
			} else {
				require.Equal(t, config.Env, parsed.Env)
			}
		})
	}
}
