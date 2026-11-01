package dbmigrate

// 本文件守颗粒度文档 §4.3 C-40 的第三层「外部依赖就绪」：运行时组件（DBHub / GitHub MCP /
// Fetch / docs-readonly）缺失时，复核报告**必须**按 Agent 点名"缺了它谁不完整"，不能只说
// "依赖缺失"——否则目标机缺组件也会被当成"迁移成功"。
// C-40's external-dependency layer: a missing runtime component must be attributed to the
// agents that need it, never reduced to a generic "dependency missing" line.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVerifyAssets_ReportsMissingComponentPerAgent 判 `VerifyAssets` 对运行时组件条目的
// 缺项报告：`Missing` 按路径点名、`MissingFor` 按 base 名索引并带回全部 RequiredFor。
//
// 逆改点：缺项分支不填 `MissingFor`（只留路径）→ 组件名断言红；把 RequiredFor 换成空表 →
// Agent 点名断言红；组件在位时也报缺项 → 首个 `report.OK()` 断言红。
// TestVerifyAssets_ReportsMissingComponentPerAgent pins per-agent attribution of a missing
// runtime component.
func TestVerifyAssets_ReportsMissingComponentPerAgent(t *testing.T) {
	const component = "bin/runtime/mcp/github-mcp-server"
	root := t.TempDir()
	assetWriteFile(t, root, "basalt.db", "SQLite format 3\x00")
	assetWriteFile(t, root, "agents/github-radar/AGENT.md", "agent\n")
	assetWriteFile(t, root, component, "#!/bin/sh\n")

	specs := []AssetSpec{
		{Path: "agents/github-radar", Kind: AssetKindAgentDef, RequiredFor: []string{"github-radar"}},
		{Path: component, Kind: AssetKindScript, RequiredFor: []string{"github-radar", "repo-digest"}},
	}
	manifest := assetBuild(t, root, specs)
	require.Len(t, manifest.Entries, 2, "业务定义与组件各一条")

	report, err := VerifyAssets(context.Background(), root, manifest)
	require.NoError(t, err)
	require.True(t, report.OK(), "组件在位时复核必须零差异：%v", report)

	require.NoError(t, os.Remove(filepath.Join(root, filepath.FromSlash(component))))
	report, err = VerifyAssets(context.Background(), root, manifest)
	require.NoError(t, err)
	require.False(t, report.OK(), "缺组件不得判成功")
	require.Contains(t, report.Missing, component)
	require.Equal(t, []string{"github-radar", "repo-digest"}, report.MissingFor["github-mcp-server"],
		"缺组件要按 Agent 点名，不能只说'依赖缺失'")
}
