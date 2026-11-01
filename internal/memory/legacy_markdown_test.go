package memory_test

// 本文件是**旧 markdown 载体只读导入源**的验收面（US-16 V-2 / D-18 / D-22）：五条用例分别
// 钉住"全局层 + 各 Agent 层都读得到""缺失 / 空文件是合法空层""损坏层显式上报且不拖累其它
// 层""只读、一个字节都不写""取消即停"。判据全部建立在**文件的原始形状**上——导入源若改用
// 另一套路径或分区切分，这里逐字红。
// The acceptance surface of the read-only markdown import source.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// legacyLayer 是判卷用的紧凑读数：分层 / 分区 / 正文三元组，失败信息里一眼看出差在哪层。
// legacyLayer keeps the three judged fields in one comparable value.
type legacyLayer struct {
	Agent   string
	Section core.Section
	Content string
}

// writeLegacyLayer 按工作区布局落一层旧记忆文件（父目录按需建），返回文件路径。
// writeLegacyLayer writes one legacy layer file at the workspace-relative location.
func writeLegacyLayer(t *testing.T, root, agent, body string) string {
	t.Helper()

	rel := "memory/MEMORY.md"
	if agent != "" {
		rel = "memory/agents/" + agent + "/MEMORY.md"
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// legacyBody 与工作区模板同构（文件标题在前、两分区标题齐备、条目以 `- ` 开头）。
// legacyBody mirrors the workspace template's shape.
func legacyBody(coreEntry, archiveEntry string) string {
	return "# 记忆\n\n## 核心记忆\n\n- " + coreEntry + "\n\n## 归档记忆\n\n- " + archiveEntry + "\n"
}

// layersOf 把源返回的条目投影成紧凑读数；`Unreadable` 条目单独列出，避免与正文混判。
func layersOf(entries []memory.LegacyEntry) (layers []legacyLayer, unreadable []memory.LegacyEntry) {
	for _, entry := range entries {
		if entry.Unreadable != "" {
			unreadable = append(unreadable, entry)
			continue
		}
		layers = append(layers, legacyLayer{Agent: entry.Scope.Agent, Section: entry.Section, Content: entry.Content})
	}
	return layers, unreadable
}

// TestMarkdownLegacySource_ReadsGlobalAndAgentLayers 全局层恒查、Agent 层按目录逐个查且
// 顺序与目录遍历无关（排序后返回）：核心区条目在前、归档区在后，分层与分区逐条带出。
// 逆改点：漏读 Agent 层 / 只读核心区 / 不排序（目录顺序随机）→ 对应断言红。
func TestMarkdownLegacySource_ReadsGlobalAndAgentLayers(t *testing.T) {
	root := t.TempDir()
	writeLegacyLayer(t, root, "", legacyBody("全局核心", "全局归档"))
	writeLegacyLayer(t, root, "beta", legacyBody("Beta 核心", "Beta 归档"))
	writeLegacyLayer(t, root, "alpha", legacyBody("Alpha 核心", "Alpha 归档"))

	entries, err := memory.NewMarkdownLegacySource(root).Read(context.Background())
	require.NoError(t, err)

	layers, unreadable := layersOf(entries)
	require.Empty(t, unreadable, "结构完好的载体不得产出一条不可读条目")
	require.Equal(t, []legacyLayer{
		{Agent: "", Section: core.SectionCore, Content: "全局核心"},
		{Agent: "", Section: core.SectionArchive, Content: "全局归档"},
		{Agent: "alpha", Section: core.SectionCore, Content: "Alpha 核心"},
		{Agent: "alpha", Section: core.SectionArchive, Content: "Alpha 归档"},
		{Agent: "beta", Section: core.SectionCore, Content: "Beta 核心"},
		{Agent: "beta", Section: core.SectionArchive, Content: "Beta 归档"},
	}, layers, "全局层在前、Agent 层按名字升序；每层核心区在前、归档区在后")
}

// TestMarkdownLegacySource_MissingAndEmptyLayersAreLegal 缺失文件与空文件都表示"这一层
// 没有记忆"（与运行期合法空层同一档）：零条目、零不可读、零错误——报成不可读会让每次迁移
// 都出现假问题；报成错误会让"首次迁移一个空实例"根本无法完成。
// 逆改点：把缺失 / 空文件产出 Unreadable 或返回 error → 本用例红。
func TestMarkdownLegacySource_MissingAndEmptyLayersAreLegal(t *testing.T) {
	t.Run("整棵 memory 目录缺失", func(t *testing.T) {
		entries, err := memory.NewMarkdownLegacySource(t.TempDir()).Read(context.Background())
		require.NoError(t, err, "没有旧记忆不是错误")
		require.Empty(t, entries)
	})

	t.Run("全局层为空文件", func(t *testing.T) {
		root := t.TempDir()
		writeLegacyLayer(t, root, "", "")
		entries, err := memory.NewMarkdownLegacySource(root).Read(context.Background())
		require.NoError(t, err)
		require.Empty(t, entries, "零字节文件 = 合法空层")
	})

	t.Run("只有 agents 目录没有全局层", func(t *testing.T) {
		root := t.TempDir()
		writeLegacyLayer(t, root, "alpha", legacyBody("Alpha 核心", "Alpha 归档"))
		entries, err := memory.NewMarkdownLegacySource(root).Read(context.Background())
		require.NoError(t, err)
		layers, unreadable := layersOf(entries)
		require.Empty(t, unreadable)
		require.Equal(t, []legacyLayer{
			{Agent: "alpha", Section: core.SectionCore, Content: "Alpha 核心"},
			{Agent: "alpha", Section: core.SectionArchive, Content: "Alpha 归档"},
		}, layers)
	})
}

// TestMarkdownLegacySource_DamagedLayerIsExplicitlyUnreadable 结构损坏的层产出**一条**
// `Unreadable` 条目（原因含相对路径与损坏类别），并且**不**拖累其它层——一处损坏不该让
// 整次导入只剩一个错误（D-18 的"不可读条目显式报告、不静默丢弃"）。
// 逆改点：损坏时返回 error 或静默跳过该层 → 分别让"其余层仍在"或"恰一条不可读"断言红。
func TestMarkdownLegacySource_DamagedLayerIsExplicitlyUnreadable(t *testing.T) {
	root := t.TempDir()
	// 缺少归档分区标题 = 结构损坏（parseSections 的判据），再好的条目也不得被当成空层。
	writeLegacyLayer(t, root, "", "# 记忆\n\n## 核心记忆\n\n- 只有核心区\n")
	writeLegacyLayer(t, root, "alpha", legacyBody("Alpha 核心", "Alpha 归档"))

	entries, err := memory.NewMarkdownLegacySource(root).Read(context.Background())
	require.NoError(t, err, "单层损坏不是整次读取的失败")

	layers, unreadable := layersOf(entries)
	require.Len(t, unreadable, 1, "损坏层恰产出一条不可读条目")
	require.Empty(t, unreadable[0].Scope.Agent)
	require.Contains(t, unreadable[0].Unreadable, "memory/MEMORY.md", "原因必须点名哪一层坏了")
	require.Contains(t, unreadable[0].Unreadable, "结构损坏", "原因必须给出损坏类别")
	require.Equal(t, []legacyLayer{
		{Agent: "alpha", Section: core.SectionCore, Content: "Alpha 核心"},
		{Agent: "alpha", Section: core.SectionArchive, Content: "Alpha 归档"},
	}, layers, "损坏层之外的层必须照常读出——否则一处损坏会变成全都导不进去")
}

// TestMarkdownLegacySource_ReadsWithoutWriting 只读纪律（D-22）：读取前后旧载体逐字节
// 不变，且不得在 `memory/agents/` 外新建任何文件——导入源不是运行期载体，没有写句柄。
// 逆改点：读取路径顺手补齐目录 / 规范化文件 → 字节比较或"无新增文件"断言红。
func TestMarkdownLegacySource_ReadsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	path := writeLegacyLayer(t, root, "", legacyBody("全局核心", "全局归档"))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	_, err = memory.NewMarkdownLegacySource(root).Read(context.Background())
	require.NoError(t, err)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "旧载体一个字节都不动")

	require.NoDirExists(t, filepath.Join(root, "memory", "agents"),
		"只读源不得顺手补齐 Agent 目录")
}

// TestMarkdownLegacySource_StopsOnCanceledContext 取消即停：已取消的 context 在打开
// 根目录之前就返回 `context.Canceled`，不产生半份条目——导入要么完整要么不开始。
// 逆改点：删掉入口的 ctx 检查 → 返回 nil 错误，本用例红。
func TestMarkdownLegacySource_StopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	entries, err := memory.NewMarkdownLegacySource(t.TempDir()).Read(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, entries)
}
