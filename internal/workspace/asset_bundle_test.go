package workspace_test

// 本文件是 bundle 形态注册条目（US-25C §3.1.4 `EntryFromBundle`）的定向回归：
// 判定只读本轮 `prepared` 快照（C14），技能只认**已物化的发布目录**、不回落工作副本
// （FR-25C-07）。夹具沿用本包既有的 `t.TempDir()` 家族，不新增导出测试 API。
// Regression tests for the bundle entry builder: prepared-snapshot-only validation
// and publication-only skill resolution.

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// bundleProviderOnlyMD 只声明 Provider 的 bundle 定义（技能判定不参与本用例）。
const bundleProviderOnlyMD = `---
name: bundle-agent
description: bundle 装载用例
provider:
  name: deepseek
  model: deepseek-chat
---

bundle 正文占位。
`

// bundleWithSkillMD 在上一份定义之上声明一个技能（published 判定面用例）。
const bundleWithSkillMD = `---
name: bundle-agent
description: bundle 装载用例
provider:
  name: deepseek
  model: deepseek-chat
skills:
  - digest-format
---

bundle 正文占位。
`

// writeBundleFixture 砌出「工作区根 + 已安装 bundle 目录」两件套：定义落在
// `<bundle>/agent/agents/bundle-agent/AGENT.md`（bundle 布局是工作区根的投影）。
// writeBundleFixture materializes a workspace root and an installed bundle dir.
func writeBundleFixture(t *testing.T, agentMD string) (string, assets.Installed) {
	t.Helper()
	root := t.TempDir()
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	writeFileInRoot(t, bundleDir, filepath.Join("agent", "agents", "bundle-agent", "AGENT.md"), agentMD)
	return root, assets.Installed{
		Manifest: assets.Manifest{Agent: "bundle-agent", BundleHash: "bundle-hash-fixture"},
		Dir:      bundleDir,
	}
}

// rejectKindOf 取出类型化拒绝的类别（非拒绝错误直接失败）。
func rejectKindOf(t *testing.T, err error) core.AgentRejectKind {
	t.Helper()
	var rejected *core.AgentDefinitionRejectedError
	require.ErrorAs(t, err, &rejected)
	return rejected.Kind
}

// 中文验收点：Entry 判定只读 `prepared` 快照——装载器自己的 Provider 名集（实时目录）
// 不参与：装载器注册了 deepseek 而 prepared 为空仍拒载；装载器空名集而 prepared 给出
// deepseek 则注册成功（US-25C C14 / T030 `TestAssetSync_EntryUsesPreparedSnapshot` 的
// workspace 侧定向判据）。
// Acceptance point: the entry judgement reads the prepared snapshot only, never the
// loader's live provider set.
func TestEntryFromBundle_UsesPreparedSnapshot(t *testing.T) {
	root, installed := writeBundleFixture(t, bundleProviderOnlyMD)

	// 实时名集有 deepseek、prepared 为空 → 按"本轮未准备"拒载并点名。
	liveLoader := newTestLoaderWithTools(t, nil, nil, nil, "deepseek")
	_, err := liveLoader.EntryFromBundle(root, installed, workspace.PreparedResources{}, nil)
	require.Equal(t, core.AgentRejectProviderNotRegistered, rejectKindOf(t, err))
	var rejected *core.AgentDefinitionRejectedError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, "deepseek", rejected.Detail)

	// 实时名集为空、prepared 给出 deepseek → 必须注册成功（判定面只有 prepared）。
	emptyLoader := newTestLoaderWithTools(t, nil, nil, nil)
	entry, err := emptyLoader.EntryFromBundle(root, installed,
		workspace.PreparedResources{Providers: []string{"deepseek"}}, nil)
	require.NoError(t, err)
	require.True(t, entry.Registered)
	require.Equal(t, "bundle-agent", entry.Name)
	require.Equal(t, bundleProviderOnlyMD, string(entry.Definition))
}

// 中文验收点：技能判定只认 `published` 给出的**已物化发布目录**——工作副本存在也不得
// 回落（FR-25C-07）；发布目录缺 SKILL.md 与发布目录列表缺项同判未注册；补齐后注册成功。
// Acceptance point: skills resolve against materialized publications only; the work
// copy never satisfies the declaration.
func TestEntryFromBundle_SkillsResolveAgainstPublishedDirsOnly(t *testing.T) {
	root, installed := writeBundleFixture(t, bundleWithSkillMD)
	loader := newTestLoader(t, nil, nil)

	// 工作副本 skills/digest-format/SKILL.md 存在，但 published 未列出该技能。
	writeFileInRoot(t, root, filepath.Join("skills", "digest-format", "SKILL.md"), digestFormatSkillMD)
	_, err := loader.EntryFromBundle(root, installed,
		workspace.PreparedResources{Providers: []string{"deepseek"}}, nil)
	require.Equal(t, core.AgentRejectSkillNotFound, rejectKindOf(t, err))

	// 发布目录已登记但 SKILL.md 未物化 → 同样未注册。
	published := map[string]string{"digest-format": "versions/skills/digest-format/1/1"}
	_, err = loader.EntryFromBundle(root, installed,
		workspace.PreparedResources{Providers: []string{"deepseek"}}, published)
	require.Equal(t, core.AgentRejectSkillNotFound, rejectKindOf(t, err))

	// 发布目录补齐 SKILL.md → 注册成功。
	writeFileInRoot(t, root, filepath.Join("versions", "skills", "digest-format", "1", "1", "SKILL.md"),
		digestFormatSkillMD)
	entry, err := loader.EntryFromBundle(root, installed,
		workspace.PreparedResources{Providers: []string{"deepseek"}}, published)
	require.NoError(t, err)
	require.True(t, entry.Registered)
	require.Equal(t, []string{"digest-format"}, entry.Profile.Skills)
}
