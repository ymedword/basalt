package app

// 本文件覆盖基线定义原文的**切分形态**（颗粒度文档 §3.1 基线资源版本 / 契约
// `definition_yaml = 该资源块的原文`）：`providers` / `servers` 是块式序列，切出来的
// 每个条目必须是**映射**原文，发布出去才能被 `/system/resources` 解析成资源视图。
// 逆改点：保留序列指示符 `-` → `parseProviderDefinition` / `parseMCPDefinition`
// 得到 `sequence was used where mapping is expected`，可运行性视图退化为"不可确认"。
// The extracted definition block must be a mapping: keeping the sequence dash makes the
// published text unparsable as a resource definition.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadProviderDefinitions_ExtractedBlockIsMapping：从实例配置切出的 provider 定义
// 必须能按映射解析出全部字段；`${ENV_VAR}` 保持原样（原文不含解析后的值）。
func TestLoadProviderDefinitions_ExtractedBlockIsMapping(t *testing.T) {
	t.Setenv("BASALT_DEFINITION_KEY", "definition-sentinel-not-a-secret")

	cases := map[string]string{
		"LF": "providers:\n" +
			"  - name: demo\n" +
			"    base_url: http://127.0.0.1:18090/v1\n" +
			"    api_key: ${BASALT_DEFINITION_KEY}\n" +
			"    default_model: demo-model\n",
		"CRLF": "providers:\r\n" +
			"  - name: demo\r\n" +
			"    base_url: http://127.0.0.1:18090/v1\r\n" +
			"    api_key: ${BASALT_DEFINITION_KEY}\r\n" +
			"    default_model: demo-model\r\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, configFileName)
			require.NoError(t, os.WriteFile(configPath, []byte(raw), 0o644))

			definitions, err := loadProviderDefinitions(configPath, []string{"demo"})
			require.NoError(t, err)
			require.Len(t, definitions, 1)
			require.Equal(t, "demo", definitions[0].Name)

			content := string(definitions[0].Content)
			require.False(t, strings.HasPrefix(content, "-"),
				"条目指示符不属于资源块本身：%q", content)
			require.Contains(t, content, "\n", "定义块必须逐行保留")

			parsed, err := parseProviderDefinition(definitions[0].Name, content)
			require.NoError(t, err, "发布定义必须是可解析的映射原文: %q", content)
			require.Equal(t, "demo", parsed.Name)
			require.Equal(t, "http://127.0.0.1:18090/v1", parsed.BaseURL)
			require.Equal(t, "${BASALT_DEFINITION_KEY}", parsed.APIKey)
			require.Equal(t, "demo-model", parsed.DefaultModel)
		})
	}
}

// TestLoadMCPServerDefinitions_ExtractedBlockIsMapping：MCP 定义同理——切出的原文必须
// 按映射解析出 name / transport / command / env 键值，且 env 值保留 `${VAR}` 形态。
func TestLoadMCPServerDefinitions_ExtractedBlockIsMapping(t *testing.T) {
	t.Setenv("BASALT_DEFINITION_KEY", "definition-sentinel-not-a-secret")

	root := t.TempDir()
	raw := "servers:\n" +
		"  - name: web\n" +
		"    transport: stdio\n" +
		"    command: npx -y web-search\n" +
		"    env:\n" +
		"      SEARCH_KEY: ${BASALT_DEFINITION_KEY}\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, mcpServersFileName), []byte(raw), 0o644))

	definitions, err := loadMCPServerDefinitions(root, []string{"web"})
	require.NoError(t, err)
	require.Len(t, definitions, 1)
	require.Equal(t, "web", definitions[0].Name)

	content := string(definitions[0].Content)
	parsed, err := parseMCPDefinition(definitions[0].Name, content)
	require.NoError(t, err, "发布定义必须是可解析的映射原文: %q", content)
	require.Equal(t, "web", parsed.Name)
	require.Equal(t, "stdio", parsed.Transport)
	require.Equal(t, "npx -y web-search", parsed.Command)
	require.Equal(t, "${BASALT_DEFINITION_KEY}", parsed.Env["SEARCH_KEY"])
}

// TestLoadProviderDefinitions_DashOnOwnLine：`-` 单独成行的写法（字段另起缩进行）
// 同样属于合法块式序列——切分后去掉指示符必须得到合法映射，而不是空文档。
func TestLoadProviderDefinitions_DashOnOwnLine(t *testing.T) {
	t.Setenv("BASALT_DEFINITION_KEY", "definition-sentinel-not-a-secret")

	root := t.TempDir()
	configPath := filepath.Join(root, configFileName)
	raw := "providers:\n" +
		"  -\n" +
		"    name: demo\n" +
		"    base_url: http://127.0.0.1:18090/v1\n" +
		"    api_key: ${BASALT_DEFINITION_KEY}\n" +
		"    default_model: demo-model\n"
	require.NoError(t, os.WriteFile(configPath, []byte(raw), 0o644))

	definitions, err := loadProviderDefinitions(configPath, []string{"demo"})
	require.NoError(t, err)
	require.Len(t, definitions, 1)

	parsed, err := parseProviderDefinition(definitions[0].Name, string(definitions[0].Content))
	require.NoError(t, err)
	require.Equal(t, "demo", parsed.Name)
	require.Equal(t, "demo-model", parsed.DefaultModel)
}
