package workspace_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/workspace"
)

// 工作区骨架的固定清单（需求文档 §3.1 产物树；Compatibility §2.1）：顺序与报告一致。
// The fixed skeleton inventory, in report order.
var (
	skeletonDirs  = []string{"agents", "skills", "memory", "logs", "archive", "sessions"}
	skeletonFiles = []string{"AGENTS.md", "SOUL.md", "USER.md", "mcp_servers.yaml", "memory/MEMORY.md"}
)

// newWorkspaceRoot 给每个用例一个独立的 .basalt 路径（测试隔离纪律：不污染工作区）。
// newWorkspaceRoot hands every test its own .basalt path under t.TempDir().
func newWorkspaceRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".basalt")
}

// TestInit_CreatesFullSkeleton：目录 6 项 + 文件 5 项齐备；memory/agents/ 不预建。
func TestInit_CreatesFullSkeleton(t *testing.T) {
	ws := newWorkspaceRoot(t)

	report, err := workspace.Init(ws)
	require.NoError(t, err)

	require.Len(t, report.Created, len(skeletonDirs)+len(skeletonFiles))
	require.Empty(t, report.Kept, "空目录上首跑不应有 Kept 条目")

	for _, dir := range skeletonDirs {
		info, err := os.Stat(filepath.Join(ws, dir))
		require.NoError(t, err, "目录 %s 应当存在", dir)
		require.True(t, info.IsDir(), "%s 应当是目录", dir)
	}
	for _, file := range skeletonFiles {
		info, err := os.Stat(filepath.Join(ws, file))
		require.NoError(t, err, "文件 %s 应当存在", file)
		require.False(t, info.IsDir(), "%s 应当是文件", file)
	}

	// memory/agents/ 不预建——Agent 层记忆在 save_memory 时按需落盘（流程 A）
	_, err = os.Stat(filepath.Join(ws, "memory", "agents"))
	require.ErrorIs(t, err, fs.ErrNotExist, "memory/agents/ 不得预建")
}

// TestInit_TemplateContent：MEMORY.md 两分区齐备；mcp_servers.yaml 是空表且**下游
// 解析器读回**为空映射（生成物必须经解析器回读，不能只看文件存在）。
func TestInit_TemplateContent(t *testing.T) {
	ws := newWorkspaceRoot(t)
	require.NoError(t, initErr(ws))

	memory := readWorkspaceFile(t, ws, "memory/MEMORY.md")
	require.Contains(t, memory, "## 核心记忆")
	require.Contains(t, memory, "## 归档记忆")

	servers := map[string]any{}
	require.NoError(t, yaml.Unmarshal([]byte(readWorkspaceFile(t, ws, "mcp_servers.yaml")), &servers),
		"mcp_servers.yaml 必须可被 goccy/go-yaml 解析")
	require.Empty(t, servers, "mcp_servers.yaml 应当是空表")
}

// TestInit_Idempotent：二跑幂等——已有文件不覆盖（需求文档 §3.1；技术方案 §3.1.1）。
func TestInit_Idempotent(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, ".basalt")

	first, err := workspace.Init(ws)
	require.NoError(t, err)
	require.NotEmpty(t, first.Created)

	// 预置"被运维手工改过"的文件，验证已有内容不被覆盖
	custom := []byte("# 自定义 bootstrap，init 不得覆盖\n")
	require.NoError(t, os.WriteFile(filepath.Join(ws, "AGENTS.md"), custom, 0o644))

	second, err := workspace.Init(ws)
	require.NoError(t, err)          // 二跑不报错
	require.Empty(t, second.Created) // 二跑无新建

	got, err := os.ReadFile(filepath.Join(ws, "AGENTS.md"))
	require.NoError(t, err)
	require.Equal(t, custom, got) // 已有文件逐字节未变
}

// TestInit_DoesNotOverwriteExisting：已在的目录与其内容同样不被触碰——只预置文件
// 的用例测不出"已存在目录被重建"这类回归（目录级幂等）。
func TestInit_DoesNotOverwriteExisting(t *testing.T) {
	ws := newWorkspaceRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "skills"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "memory"), 0o750))

	customAgents := []byte("# 自定义 AGENTS.md\n")
	customMemory := []byte("# 自定义长期记忆\n")
	customSkill := []byte("---\nname: demo-skill\n---\n演示技能\n")
	require.NoError(t, os.WriteFile(filepath.Join(ws, "AGENTS.md"), customAgents, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "memory", "MEMORY.md"), customMemory, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "skills", "demo-skill.md"), customSkill, 0o644))

	report, err := workspace.Init(ws)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{"skills", "memory", "AGENTS.md", "memory/MEMORY.md"}, report.Kept)
	require.ElementsMatch(t, []string{"agents", "logs", "archive", "sessions", "SOUL.md", "USER.md", "mcp_servers.yaml"}, report.Created)

	require.Equal(t, customAgents, readWorkspaceFileBytes(t, ws, "AGENTS.md"))
	require.Equal(t, customMemory, readWorkspaceFileBytes(t, ws, "memory/MEMORY.md"))
	require.Equal(t, customSkill, readWorkspaceFileBytes(t, ws, "skills/demo-skill.md"))
}

// ExampleInit 演示完整调用序列：首跑建全骨架、二跑零新建（幂等即此语义）。
// ExampleInit walks the full call sequence: a first run creates the skeleton, a
// second run creates nothing at all.
func ExampleInit() {
	root, err := os.MkdirTemp("", "basalt-example")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)

	ws := filepath.Join(root, ".basalt")

	first, err := workspace.Init(ws)
	if err != nil {
		panic(err)
	}
	second, err := workspace.Init(ws)
	if err != nil {
		panic(err)
	}

	fmt.Println("first created:", len(first.Created))
	fmt.Println("second created:", len(second.Created))
	fmt.Println("second kept:", len(second.Kept))
	// Output:
	// first created: 11
	// second created: 0
	// second kept: 11
}

// initErr 是 Init 的薄包装：让"只关心错误"的用例少一行噪音。
func initErr(root string) error {
	_, err := workspace.Init(root)
	return err
}

// readWorkspaceFile / readWorkspaceFileBytes 经 os.Root 读工作区内的文件——与实现
// 同一口径（路径钉在根内），同时避开裸 os.ReadFile(变量) 在静态安全扫描里的判定。
func readWorkspaceFile(t *testing.T, ws, rel string) string {
	t.Helper()
	return string(readWorkspaceFileBytes(t, ws, rel))
}

func readWorkspaceFileBytes(t *testing.T, ws, rel string) []byte {
	t.Helper()
	root, err := os.OpenRoot(ws)
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()

	data, err := root.ReadFile(filepath.FromSlash(rel))
	require.NoError(t, err)
	return data
}
