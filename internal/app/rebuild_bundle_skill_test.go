package app

// 本文件直接钉 `rebuildBundleWithSkill` 的输入输出（模块卡 §3.3 冻结签名）：
//
//   - `ConfigHash` 沿用旧清单，`ConfigFiles` 逐项相等——配置文件由 `assets.Build` 从
//     `<oldDir>/agent` **重新计算**（`copyBundleFile` 对配置文件传 `expected == nil`，
//     不校验），所以"重建后必须用返回清单与旧清单逐项比对"这条判据只能在这里看；
//   - 被替换技能的字节来自 `newRoot`（已发布版本目录），未变技能仍取 `<oldDir>/skills/<name>`；
//   - 结果必须**已装进资产根**并可由 `assets.Resolve` 按哈希复算——冻结签名没有 Dest
//     参数，重建函数自己持有暂存目录并完成安装，调用方按返回的 hash 重新解析。
//
// 工作副本与工作区 AGENT.md 在这里被故意分叉：这两份字节出现在结果里即证明有人绕过了
// "只装已发布内容"的约束。
// This file pins rebuildBundleWithSkill directly: config hashes are preserved, replaced
// skills come from the published version directory, and the result is installed and
// resolvable by hash (the frozen signature has no Dest parameter).

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
)

// TestSkillRepublish_RebuildsFromInstalledBundle 守"从已安装目录重建、不读工作副本、
// ConfigHash 沿用"三条判据。
func TestSkillRepublish_RebuildsFromInstalledBundle(t *testing.T) {
	t.Parallel()

	f := newSkillRepairFixture(t)
	f.seedBundle(repairAgentAlpha, 1)
	// 工作区侧两份内容都分叉：重建若读工作区，下面的逐字断言就会红。
	writeWorkspaceFile(t, f.root, "skills/"+repairSkillName+"/SKILL.md", repairWorkCopyBody)
	writeWorkspaceFile(t, f.root, "agents/"+repairAgentAlpha+"/AGENT.md", repairAgentDivergedBody)

	ctx := context.Background()
	oldManifest := f.publishedManifest(repairAgentAlpha)
	oldDir, err := assets.Resolve(ctx, f.assetRoot, repairAgentAlpha, oldManifest.BundleHash)
	require.NoError(t, err)
	verified, err := assets.Verify(ctx, oldDir)
	require.NoError(t, err, "重建来源必须自洽——哈希与记录不符的目录不得作为来源")

	source := f.skillSource(2)
	rebuilt, err := rebuildBundleWithSkill(ctx, oldDir, repairAgentAlpha, source.Ref, source.Root)
	require.NoError(t, err)

	require.Equal(t, oldManifest.ConfigHash, rebuilt.ConfigHash,
		"重建不得重算配置快照哈希：它描述的是已批准的那次发布")
	require.Equal(t, oldManifest.ConfigFiles, rebuilt.ConfigFiles,
		"配置文件清单必须与旧清单逐项相等（哈希 + 大小），否则等于顺带发布了工作区改动")
	require.Equal(t, oldManifest.Agent, rebuilt.Agent)
	require.Equal(t, oldManifest.AgentUID, rebuilt.AgentUID)
	require.NotEqual(t, oldManifest.BundleHash, rebuilt.BundleHash,
		"技能版本变了，bundle 哈希必须随之变化")
	require.Len(t, rebuilt.Skills, 1)
	require.Equal(t, source.Ref, rebuilt.Skills[0], "技能引用必须逐字等于锁内复核过的当前版本")

	dir, err := assets.Resolve(ctx, f.assetRoot, repairAgentAlpha, rebuilt.BundleHash)
	require.NoError(t, err, "重建结果必须已装进资产根，并能按哈希解析")
	installed, err := assets.Verify(ctx, dir)
	require.NoError(t, err, "安装目录必须可由清单复算")
	require.Equal(t, rebuilt.BundleHash, installed.BundleHash)

	skillBody, err := os.ReadFile(filepath.Join(dir, "skills", repairSkillName, "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, repairSkillV2Body, string(skillBody), "技能字节必须来自已发布版本目录")
	require.NotEqual(t, repairWorkCopyBody, string(skillBody), "不得使用工作副本")

	configBody, err := os.ReadFile(filepath.Join(dir, "agent", "agents", repairAgentAlpha, "AGENT.md"))
	require.NoError(t, err)
	require.Equal(t, repairAgentBody(repairAgentAlpha), string(configBody),
		"配置字节必须取自旧 bundle 的 agent/ 子树")
	require.NotEqual(t, repairAgentDivergedBody, string(configBody), "不得使用工作区未发布的定义")

	require.Equal(t, verified.BundleHash, oldManifest.BundleHash,
		"重建不得改动旧目录（资产不可变）")
}
