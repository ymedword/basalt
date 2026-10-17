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

// TestTemplates_AgentTemplateDeclaresMemoryTools：新建 Agent 的模板**默认工具**逐字
// = [save_memory, recall_memory]（颗粒度文档 §6.2 口径 26 / spec FR-026 / plan
// Constraints）——记忆工具随本 US 进注册池，出厂模板即声明它们，`profile create`
// 造出的 Agent 第一次运行就能记东西。
//
// 判卷装置刻意**不用装载器**：本条判的是**模板声明本身**（文件里写了什么），
// 装载器的注册判定另有 T032 承载；走真模板路径（`CreateAgent` 落盘 → 读回文件 →
// 按 YAML 解码 frontmatter），避免绕开 agentTemplate 直接读未导出函数。
// 中文验收点：`agents/<name>/AGENT.md` 的 frontmatter `tools:` 恰为两件记忆工具。
// The template's declared default tools are judged from the file the real path
// wrote — not through the loader, whose registration rule is T032's subject.
func TestTemplates_AgentTemplateDeclaresMemoryTools(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)

	created, err := workspace.CreateAgent(root, "memory-tools", []string{"deepseek"})
	require.NoError(t, err)
	require.Equal(t, []string{"agents/memory-tools/AGENT.md"}, created)

	raw, err := os.ReadFile(filepath.Join(root, "agents", "memory-tools", "AGENT.md"))
	require.NoError(t, err)

	front, _, found := strings.Cut(string(raw), "\n---\n")
	require.True(t, found, "模板必须是 frontmatter 围栏形态（首行 --- 起、--- 止）：\n%s", raw)

	var dto struct {
		Tools []string `yaml:"tools"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(front), &dto))
	require.Equal(t, []string{"save_memory", "recall_memory"}, dto.Tools,
		"模板默认工具逐字：两件记忆工具（顺序即声明序，多余与缺失都算装配漂移）")
}
