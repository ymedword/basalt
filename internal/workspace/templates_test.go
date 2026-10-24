package workspace_test

// 本文件承载工作区记忆路径约定与模板默认工具两件事（颗粒度文档 §3.2 的
// workspace/templates_test.go 两例）——文件按文档点名的文件名新建（仓库实测它
// 先前不存在，见 plan.md「文档 ↔ 仓库事实差异」）。两例分属两个任务：
// TestMemoryFilePath_Convention 属 T005（纯路径约定，零文件系统访问）；
// TestTemplates_AgentTemplateDeclaresMemoryTools 属 T030（走真模板路径
// `CreateAgent` 落盘 → 读回文件 → 解析 frontmatter——**不用装载器**，判的是模板
// 声明本身）。两条各用各的判卷装置，夹具不依赖 agent_test.go 的家族。
// Two cases, two tasks: the path convention (T005) and the template's declared
// default tools (T030), the latter read back from the file the real path wrote.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// TestMemoryFilePath_Convention：两条路径逐字断言（相对工作区根、无前导斜杠），
// 并与 core.GlobalScope 侧形态成对——空 Agent 名 = 全局层是核心声明面的一等取值，
// 路径约定不得与它漂移。
// 中文验收点：agent == "" → memory/MEMORY.md；否则 → memory/agents/<agent>/MEMORY.md。
func TestMemoryFilePath_Convention(t *testing.T) {
	cases := []struct {
		name  string
		agent string
		want  string
	}{
		{"全局层_空Agent名", "", "memory/MEMORY.md"},
		{"Agent层", "ops-agent", "memory/agents/ops-agent/MEMORY.md"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workspace.MemoryFilePath(tc.agent)
			require.Equal(t, tc.want, got, "路径逐字——相对工作区根形态（正斜杠），不得多一字")
			require.False(t, strings.HasPrefix(got, "/"), "相对工作区根、无前导斜杠：%q", got)
		})
	}

	// 与 core.GlobalScope 侧形态成对：全局层作用域的空 Agent 名映射到全局路径
	// （core 的 Scope 零值语义与 workspace 的路径约定是同一件事的两个声明面）。
	require.Equal(t, "memory/MEMORY.md", workspace.MemoryFilePath(core.GlobalScope.Agent))
}

// templateFrontmatter 读回模板文件并解出 frontmatter 段（围栏之间）。
// templateFrontmatter reads a created template back and returns its frontmatter.
func templateFrontmatter(t *testing.T, root, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "agents", name, "AGENT.md"))
	require.NoError(t, err)
	front, _, found := strings.Cut(string(raw), "\n---\n")
	require.True(t, found, "模板必须是 frontmatter 围栏形态（首行 --- 起、--- 止）：\n%s", raw)
	return front
}

// templateDTO 是模板 frontmatter 的解码形态（只取本文件判的字段）。
// templateDTO is the decoded shape of the template frontmatter.
type templateDTO struct {
	Description string `yaml:"description"`
	Provider    struct {
		Name  string `yaml:"name"`
		Model string `yaml:"model"`
	} `yaml:"provider"`
	Tools []string `yaml:"tools"`
}

// TestTemplates_AgentTemplateDeclaresMemoryTools：新建 Agent 的模板**默认工具**逐字
// = [save_memory, recall_memory]（颗粒度文档 §6.2 口径 26 / spec FR-026 / plan
// Constraints）——记忆工具随本 US 进注册池，出厂模板即声明它们，`profile create`
// 造出的 Agent 第一次运行就能记东西。
//
// 判卷装置刻意**不用装载器**：本条判的是**模板声明本身**（文件里写了什么），
// 装载器的注册判定另有 T032 承载；走真模板路径（`CreateAgentStaged` 落盘 → 读回文件 →
// 按 YAML 解码 frontmatter），避免绕开 agentTemplate 直接读未导出函数。
// 中文验收点：`agents/<name>/AGENT.md` 的 frontmatter `tools:` 恰为两件记忆工具。
// The template's declared default tools are judged from the file the real path
// wrote — not through the loader, whose registration rule is T032's subject.
func TestTemplates_AgentTemplateDeclaresMemoryTools(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)

	created, err := workspace.CreateAgentStaged(root, core.AgentSpec{Name: "memory-tools"}, []string{"deepseek"})
	require.NoError(t, err)
	require.Len(t, created, 1)
	require.Equal(t, "agents/memory-tools/AGENT.md", created[0].Path)

	var dto templateDTO
	require.NoError(t, yaml.Unmarshal([]byte(templateFrontmatter(t, root, "memory-tools")), &dto))
	require.Equal(t, []string{"save_memory", "recall_memory"}, dto.Tools,
		"模板默认工具逐字：两件记忆工具（顺序即声明序，多余与缺失都算装配漂移）")
}

// 中文验收点（§3.2.2）：创建请求给了描述 → 描述**逐字**落进 frontmatter
// （管理台的"一句描述"就是从这里进模板的，编码写入、不插值）。
// Acceptance point: a supplied description lands in the frontmatter verbatim.
func TestTemplates_DescriptionLandsInFrontmatter(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)

	description := "日报汇总：每日 09:00 汇总昨日告警 # 只读"
	_, err = workspace.CreateAgentStaged(root, core.AgentSpec{Name: "digest", Description: description}, []string{"deepseek"})
	require.NoError(t, err)

	var dto templateDTO
	require.NoError(t, yaml.Unmarshal([]byte(templateFrontmatter(t, root, "digest")), &dto))
	require.Equal(t, description, dto.Description)
}

// 中文验收点（§3.2.2）：创建请求给了模型 → 模型**逐字**落进 provider.model
// （不给才落占位文案；占位是普通文本、装载天然放行）。
// Acceptance point: a supplied model lands in provider.model verbatim.
func TestTemplates_ModelLandsInFrontmatter(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)

	_, err = workspace.CreateAgentStaged(root, core.AgentSpec{Name: "digest", Model: "deepseek-chat"}, []string{"deepseek"})
	require.NoError(t, err)

	var dto templateDTO
	require.NoError(t, yaml.Unmarshal([]byte(templateFrontmatter(t, root, "digest")), &dto))
	require.Equal(t, "deepseek-chat", dto.Provider.Model)
	require.Equal(t, "deepseek", dto.Provider.Name, "provider.name 仍取候选列表首项")
}

// 中文验收点（FR-027 / 约束：不得遍历随机选择）：没有任何已发布 Provider 时，
// provider.name 写**可见占位符**——确定性产物，不是随机挑一个或留空。
// Acceptance point: with no providers the template writes the visible placeholder.
func TestTemplates_NoProvidersWritesPlaceholder(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)

	for i := range 2 { // 跑两次：产物逐字节一致（确定性判据）
		name := "digest" + strconv.Itoa(i)
		_, err := workspace.CreateAgentStaged(root, core.AgentSpec{Name: name}, nil)
		require.NoError(t, err)

		var dto templateDTO
		require.NoError(t, yaml.Unmarshal([]byte(templateFrontmatter(t, root, name)), &dto))
		require.Equal(t, "请在此填写 provider 名", dto.Provider.Name, "无候选时必须写可见占位符")
	}
}
