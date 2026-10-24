package workspace_test

// 本文件是 Agent 目录复制 / 恢复与定义文件改写的关键回归用例（颗粒度文档 §3.2 的
// agent_admin_test.go 家族 / 约束 31）：复制与恢复都要把 frontmatter 的 `name` 与
// `identity.agent_name` 改成目标名、清空 `schedules`，失败时只删本次 staging；
// 生命周期状态文件与归档行**不在**本层写入（约束 49——它们是调用方的事）。
// Regression tests for copying/restoring agent directories and rewriting their
// definition files. Lifecycle state and archive rows belong to the caller.

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

// copySourceDefinition 是复制 / 恢复用例的源定义：含 identity 与块式 schedules，
// 用于断言目标上的「改名 + 清空定时任务」两处改写与其余内容逐字保留。
// copySourceDefinition carries identity and a block-style schedules list.
const copySourceDefinition = `---
name: source-agent
description: 复制与恢复用例
identity:
  agent_name: 源助手
  prompt: 保持人格设定。
provider:
  name: deepseek
  model: deepseek-chat
schedules:
  - id: daily-report
    cron: "0 0 9 * * *"
    zone: Asia/Shanghai
    message: "生成日报"
---

正文必须逐字保留。
`

// agentInfoPaths 取返回清单的 path 序列，断言「受管文件 = 根目录普通文件、升序」。
// agentInfoPaths projects the returned managed-file paths.
func agentInfoPaths(infos []core.AgentFileInfo) []string {
	paths := make([]string, len(infos))
	for i, info := range infos {
		paths[i] = info.Path
	}
	return paths
}

// requireNoWorkspaceEntry 断言 root/rel 不存在——失败路径的「零残留」判据。
// requireNoWorkspaceEntry asserts that a workspace path does not exist.
func requireNoWorkspaceEntry(t *testing.T, root, rel string) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	require.ErrorIs(t, err, os.ErrNotExist, "路径必须不存在（零残留）：%s", rel)
}

// requireNoLifecycleFiles 断言生命周期目录里没有状态文件：目录本身由 basalt init 建好，
// 复制 / 恢复**不得**写任何 <agent>.json（约束 49 的唯一写者是 lifecycle.go）。
// requireNoLifecycleFiles asserts that copy/restore wrote no lifecycle state file.
func requireNoLifecycleFiles(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "control", "agents"))
	if os.IsNotExist(err) {
		return
	}
	require.NoError(t, err)
	require.Empty(t, entries, "复制 / 恢复不得写生命周期状态文件")
}

// rewrittenFront 取改写结果里的 frontmatter 段（围栏之间），供 YAML 解码。
// rewrittenFront cuts the frontmatter out of a rewritten document.
func rewrittenFront(t *testing.T, doc []byte) string {
	t.Helper()
	front, _, found := strings.Cut(string(doc), "\n---\n")
	require.True(t, found, "改写结果必须仍是围栏形态：\n%s", doc)
	return front
}

// 中文验收点（约束 31）：改名同时改 `name` 与 `identity.agent_name`，其余字段与正文
// 一个字节都不动——漏改 name 会让目标 Agent 整只不注册（装载器判目录名 ≠ frontmatter
// name），漏改 identity 会让身份文本与目录名对不上。
// Acceptance point: rename touches exactly the two name fields.
func TestRewriteAgentName_UpdatesNameAndIdentity(t *testing.T) {
	in := `---
name: old-agent
description: 演示用
identity:
  agent_name: 旧小助手
  prompt: 你是一个用于演示的助手。
provider:
  name: deepseek
  model: deepseek-chat
---

正文必须逐字保留。
`
	out, err := workspace.RewriteAgentName([]byte(in), "new-agent")
	require.NoError(t, err)

	require.NotContains(t, string(out), "old-agent")
	require.NotContains(t, string(out), "旧小助手")
	require.Contains(t, string(out), "正文必须逐字保留。")
	require.Contains(t, string(out), "prompt: 你是一个用于演示的助手。")

	var dto struct {
		Name     string `yaml:"name"`
		Identity struct {
			AgentName string `yaml:"agent_name"`
			Prompt    string `yaml:"prompt"`
		} `yaml:"identity"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(rewrittenFront(t, out)), &dto))
	require.Equal(t, "new-agent", dto.Name)
	require.Equal(t, "new-agent", dto.Identity.AgentName)
	require.Equal(t, "你是一个用于演示的助手。", dto.Identity.Prompt)
}

// 中文验收点：没有 identity 段 → **不凭空添加**（只改已存在的字段，不造结构）。
// Acceptance point: an absent identity block stays absent.
func TestRewriteAgentName_LeavesAbsentIdentityAbsent(t *testing.T) {
	in := "---\nname: old-agent\nprovider:\n  name: deepseek\n  model: deepseek-chat\n---\n\n正文\n"
	out, err := workspace.RewriteAgentName([]byte(in), "new-agent")
	require.NoError(t, err)
	require.NotContains(t, string(out), "identity", "不得凭空补出 identity 段")
	require.Equal(t, "---\nname: new-agent\nprovider:\n  name: deepseek\n  model: deepseek-chat\n---\n\n正文\n", string(out))
}

// 中文验收点：行尾注释保留、需要加引号的名字按 YAML 规则编码写入（不插值）——
// `digest: v2` 这类名字裸写会变成新键值结构。
// Acceptance point: trailing comments survive and odd names are encoded, not interpolated.
func TestRewriteAgentName_PreservesCommentsAndEncodesValue(t *testing.T) {
	in := "---\nname: \"old\" # 作者备注\nprovider:\n  name: deepseek\n  model: deepseek-chat\n---\n\n正文\n"
	out, err := workspace.RewriteAgentName([]byte(in), "digest: v2")
	require.NoError(t, err)
	require.Contains(t, string(out), "name: \"digest: v2\" # 作者备注")

	var dto struct {
		Name string `yaml:"name"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(rewrittenFront(t, out)), &dto))
	require.Equal(t, "digest: v2", dto.Name, "编码后的名字必须原样往返")
}

// 中文验收点：identity 是**流式映射**（`{...}`）时无法安全定位 agent_name 行 →
// 拒绝并说明（不猜、不改坏）。
// Acceptance point: a flow-style identity is refused instead of guessed at.
func TestRewriteAgentName_RejectsFlowIdentity(t *testing.T) {
	in := "---\nname: old-agent\nidentity: {agent_name: 旧小助手}\n---\n\n正文\n"
	_, err := workspace.RewriteAgentName([]byte(in), "new-agent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "流式映射")
}

// 中文验收点：frontmatter 缺少 name → 拒绝（没有改写目标，不能凭目录名补一条）。
// Acceptance point: a document without a name key is refused.
func TestRewriteAgentName_RequiresName(t *testing.T) {
	in := "---\ndescription: 没有名字\n---\n\n正文\n"
	_, err := workspace.RewriteAgentName([]byte(in), "new-agent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "name")
}

// 中文验收点：重复的顶层 name 键 → 拒绝（改哪一个都是猜）。
// Acceptance point: duplicate top-level name keys are refused.
func TestRewriteAgentName_RejectsDuplicateName(t *testing.T) {
	in := "---\nname: a\nname: b\n---\n\n正文\n"
	_, err := workspace.RewriteAgentName([]byte(in), "new-agent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "重复")
}

// 中文验收点：CRLF 文档逐行保留行尾（改写不把文件统一成 LF）。
// Acceptance point: CRLF line endings survive the rewrite.
func TestRewriteAgentName_PreservesCRLF(t *testing.T) {
	in := "---\r\nname: old-agent\r\nprovider:\r\n  name: deepseek\r\n  model: deepseek-chat\r\n---\r\n\r\n正文\r\n"
	out, err := workspace.RewriteAgentName([]byte(in), "new-agent")
	require.NoError(t, err)
	outStr := string(out)
	require.Contains(t, outStr, "name: new-agent\r\n")
	require.NotContains(t, strings.ReplaceAll(outStr, "\r\n", ""), "\n", "改写后不得残留裸 LF")
}

// 中文验收点：目标名先过**同一份**名称校验（不合法就不进入改写）。
// Acceptance point: the target name goes through the shared validator first.
func TestRewriteAgentName_RejectsInvalidName(t *testing.T) {
	in := "---\nname: old-agent\n---\n\n正文\n"
	for _, name := range []string{"", "a/b", ".hidden", "a.staging"} {
		_, err := workspace.RewriteAgentName([]byte(in), name)
		require.Error(t, err, "非法目标名必须被拒：%q", name)
	}
}

// 中文验收点（约束 31）：复制目标只在 `name` 与 `identity.agent_name` 上等于目标名，
// 其余字段、行尾注释与围栏外正文逐字保留；源目录一个字节都不动；返回的受管文件清单
// 是落盘后重读的根目录普通文件（升序）。
// Acceptance point: a copy only rewrites the two identity fields and leaves the
// source untouched.
func TestAgentCopy_RewritesNameAndIdentity(t *testing.T) {
	root := initWorkspaceRoot(t)
	writeFileInRoot(t, root, filepath.Join("agents", "source-agent", "AGENT.md"), copySourceDefinition)
	writeFileInRoot(t, root, filepath.Join("agents", "source-agent", "REFERENCE.md"), "参考资料\n")

	infos, err := workspace.CopyAgentDir(root, "source-agent", "target-agent", nil)
	require.NoError(t, err)

	target := readWorkspaceFile(t, root, "agents/target-agent/AGENT.md")
	require.Contains(t, target, "name: target-agent")
	require.Contains(t, target, "agent_name: target-agent")
	require.Contains(t, target, "保持人格设定。")
	require.Contains(t, target, "正文必须逐字保留。")
	require.NotContains(t, target, "source-agent")

	var dto struct {
		Name     string `yaml:"name"`
		Identity struct {
			AgentName string `yaml:"agent_name"`
		} `yaml:"identity"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(rewrittenFront(t, []byte(target))), &dto))
	require.Equal(t, "target-agent", dto.Name)
	require.Equal(t, "target-agent", dto.Identity.AgentName)

	require.Equal(t, copySourceDefinition, readWorkspaceFile(t, root, "agents/source-agent/AGENT.md"),
		"复制不得改动源 Agent 的定义文件")
	require.Equal(t, "参考资料\n", readWorkspaceFile(t, root, "agents/target-agent/REFERENCE.md"),
		"未参与改写的文件逐字复制")

	require.Equal(t, []string{"AGENT.md", "REFERENCE.md"}, agentInfoPaths(infos),
		"返回落盘后重读的受管文件（根目录普通文件、升序）")
	require.Equal(t, int64(len("参考资料\n")), infos[1].Size)
	requireNoWorkspaceEntry(t, root, "agents/target-agent.staging")
}

// 中文验收点（约束 31）：复制不继承 `schedules`——块式列表整体写回空列表，源保留；
// 流式列表同样改写。全局 `id` 冲突正靠这一步避免。
// Acceptance point: copy clears block-style and flow-style schedules while the
// source keeps its own.
func TestAgentCopy_ClearsSchedules(t *testing.T) {
	root := initWorkspaceRoot(t)
	writeFileInRoot(t, root, filepath.Join("agents", "source-agent", "AGENT.md"), copySourceDefinition)

	_, err := workspace.CopyAgentDir(root, "source-agent", "target-agent", nil)
	require.NoError(t, err)

	target := readWorkspaceFile(t, root, "agents/target-agent/AGENT.md")
	require.Contains(t, target, "schedules: []")
	require.NotContains(t, target, "daily-report")
	require.NotContains(t, target, "0 0 9 * * *")
	require.Contains(t, readWorkspaceFile(t, root, "agents/source-agent/AGENT.md"), "daily-report",
		"源 Agent 的定时任务不受复制影响")

	var dto struct {
		Schedules []map[string]any `yaml:"schedules"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(rewrittenFront(t, []byte(target))), &dto))
	require.Empty(t, dto.Schedules, "目标 frontmatter 的 schedules 必须为空列表")

	// 流式列表的同一处改写。
	writeFileInRoot(t, root, filepath.Join("agents", "flow-agent", "AGENT.md"),
		"---\nname: flow-agent\nprovider:\n  name: deepseek\n  model: deepseek-chat\n"+
			"schedules: [{id: a, cron: \"0 0 9 * * *\"}]\n---\n\n正文\n")
	_, err = workspace.CopyAgentDir(root, "flow-agent", "flow-target", nil)
	require.NoError(t, err)
	require.Contains(t, readWorkspaceFile(t, root, "agents/flow-target/AGENT.md"), "schedules: []")
}

// 中文验收点（契约 agent-md.md §1）：Agent 目录内的子目录（references/、scripts/）
// 一并复制；返回清单只含根目录普通文件——与 `GET /agents/{name}/files` 同一集合。
// Acceptance point: nested directories are copied while the returned managed set
// stays the root-level regular files.
func TestAgentCopy_PreservesNestedFilesAndReturnsManagedSet(t *testing.T) {
	root := initWorkspaceRoot(t)
	writeFileInRoot(t, root, filepath.Join("agents", "source-agent", "AGENT.md"), copySourceDefinition)
	writeFileInRoot(t, root, filepath.Join("agents", "source-agent", "REFERENCE.md"), "参考资料\n")
	writeFileInRoot(t, root, filepath.Join("agents", "source-agent", "references", "guide.md"), "深层资料\n")
	writeFileInRoot(t, root, filepath.Join("agents", "source-agent", "scripts", "run.sh"), "#!/bin/sh\necho ok\n")

	infos, err := workspace.CopyAgentDir(root, "source-agent", "target-agent", nil)
	require.NoError(t, err)

	require.Equal(t, []string{"AGENT.md", "REFERENCE.md"}, agentInfoPaths(infos))
	require.Equal(t, "深层资料\n", readWorkspaceFile(t, root, "agents/target-agent/references/guide.md"))
	require.Equal(t, "#!/bin/sh\necho ok\n", readWorkspaceFile(t, root, "agents/target-agent/scripts/run.sh"))
}

// 中文验收点（约束 31）：复制源必须在活动区且是目录；目标名先过同一份名称校验；
// 源与目标同名拒绝原地复制。全部失败都不产生目标目录与 staging。
// Acceptance point: missing/non-directory sources and same-name copies are refused
// before anything is written.
func TestAgentCopy_RejectsMissingOrNonDirectorySource(t *testing.T) {
	root := initWorkspaceRoot(t)

	_, err := workspace.CopyAgentDir(root, "missing-agent", "target-agent", nil)
	require.ErrorContains(t, err, "不存在")

	writeFileInRoot(t, root, filepath.Join("agents", "file-agent"), "不是目录\n")
	_, err = workspace.CopyAgentDir(root, "file-agent", "target-agent", nil)
	require.ErrorContains(t, err, "不是目录")

	_, err = workspace.CopyAgentDir(root, "same-agent", "same-agent", nil)
	require.ErrorContains(t, err, "同名")

	_, err = workspace.CopyAgentDir(root, "source-agent", "bad/name", nil)
	require.Error(t, err, "目标名必须过 ValidateAgentName")

	requireNoWorkspaceEntry(t, root, "agents/target-agent")
	requireNoWorkspaceEntry(t, root, "agents/target-agent.staging")
}

// 中文验收点（约束 31）：目标目录已存在 → 拒绝且既有目录逐字保留，staging 零残留。
// Acceptance point: an existing target is refused without touching either tree.
func TestAgentCopy_TargetExistsRefused(t *testing.T) {
	root := initWorkspaceRoot(t)
	writeFileInRoot(t, root, filepath.Join("agents", "source-agent", "AGENT.md"), copySourceDefinition)
	writeFileInRoot(t, root, filepath.Join("agents", "target-agent", "AGENT.md"), "既有目标\n")

	_, err := workspace.CopyAgentDir(root, "source-agent", "target-agent", nil)
	require.ErrorContains(t, err, "已存在")

	require.Equal(t, "既有目标\n", readWorkspaceFile(t, root, "agents/target-agent/AGENT.md"))
	require.Equal(t, copySourceDefinition, readWorkspaceFile(t, root, "agents/source-agent/AGENT.md"))
	requireNoWorkspaceEntry(t, root, "agents/target-agent.staging")
}

// 中文验收点（约束 31）：候选树缺少 AGENT.md → 拒绝（复制不产出半只 Agent），
// staging 已建但必须被删净；源目录原样保留。
// Acceptance point: a source without AGENT.md fails after staging and leaves no residue.
func TestAgentCopy_DefinitionFailureLeavesNoStaging(t *testing.T) {
	root := initWorkspaceRoot(t)
	writeFileInRoot(t, root, filepath.Join("agents", "source-agent", "REFERENCE.md"), "只有资料\n")

	_, err := workspace.CopyAgentDir(root, "source-agent", "target-agent", nil)
	require.ErrorContains(t, err, "缺少 AGENT.md")

	requireNoWorkspaceEntry(t, root, "agents/target-agent")
	requireNoWorkspaceEntry(t, root, "agents/target-agent.staging")
	require.Equal(t, "只有资料\n", readWorkspaceFile(t, root, "agents/source-agent/REFERENCE.md"))
}

// 中文验收点（约束 31）：恢复同样改写 `name` / `identity.agent_name` 并清空 `schedules`；
// 恢复是**复制不是搬移**——归档目录原样保留（`restored_at` 由存储层标记）。
// Acceptance point: restore rewrites identity and clears schedules while the archive
// directory stays in place.
func TestAgentRestore_RewritesNameIdentityAndClearsSchedules(t *testing.T) {
	root := initWorkspaceRoot(t)
	const archiveRel = "archive/source-agent-20261022-080000"
	writeFileInRoot(t, root, filepath.Join(archiveRel, "AGENT.md"), copySourceDefinition)
	writeFileInRoot(t, root, filepath.Join(archiveRel, "REFERENCE.md"), "归档资料\n")

	infos, err := workspace.RestoreAgentDir(root, archiveRel, "restored-agent")
	require.NoError(t, err)

	target := readWorkspaceFile(t, root, "agents/restored-agent/AGENT.md")
	require.Contains(t, target, "name: restored-agent")
	require.Contains(t, target, "agent_name: restored-agent")
	require.Contains(t, target, "schedules: []")
	require.NotContains(t, target, "daily-report")
	require.Equal(t, "归档资料\n", readWorkspaceFile(t, root, "agents/restored-agent/REFERENCE.md"))
	require.Equal(t, []string{"AGENT.md", "REFERENCE.md"}, agentInfoPaths(infos))

	require.Equal(t, copySourceDefinition, readWorkspaceFile(t, root, filepath.Join(archiveRel, "AGENT.md")),
		"恢复不得改动归档目录（复制不是搬移）")
	requireNoWorkspaceEntry(t, root, "agents/restored-agent.staging")
}

// 中文验收点（约束 31 / 32）：恢复源必须是 `archive/<归档标识>` 形态且清单可复算；
// 不存在 / 指向活动区 / 缺定义文件都在写入前或 staging 构建段失败，目标与 staging 零残留。
// 符号链接是否可建因平台而异：建不了时如实跳过该子断言（与 TestDeleteAgent_RefusesSymlinkEntry
// 同一处置），其余断言仍执行。
// Acceptance point: invalid or unrestorable archive sources are refused with zero
// residue; the symlink sub-check runs only where the platform permits symlinks.
func TestAgentRestore_RejectsUnrestorableArchive(t *testing.T) {
	root := initWorkspaceRoot(t)

	_, err := workspace.RestoreAgentDir(root, "archive/missing-20261022-080000", "target-a")
	require.ErrorContains(t, err, "不存在")

	writeFileInRoot(t, root, filepath.Join("agents", "live-agent", "AGENT.md"), copySourceDefinition)
	_, err = workspace.RestoreAgentDir(root, "agents/live-agent", "target-b")
	require.ErrorContains(t, err, "恢复源必须")

	writeFileInRoot(t, root, filepath.Join("archive", "no-def-20261022-080000", "REFERENCE.md"), "无定义\n")
	_, err = workspace.RestoreAgentDir(root, "archive/no-def-20261022-080000", "target-c")
	require.ErrorContains(t, err, "缺少 AGENT.md")

	for _, rel := range []string{
		"agents/target-a", "agents/target-a.staging",
		"agents/target-b", "agents/target-b.staging",
		"agents/target-c", "agents/target-c.staging",
	} {
		requireNoWorkspaceEntry(t, root, rel)
	}

	const linkArchive = "archive/links-20261022-080000"
	writeFileInRoot(t, root, filepath.Join(linkArchive, "AGENT.md"), copySourceDefinition)
	linkPath := filepath.Join(root, filepath.FromSlash(linkArchive), "link.md")
	if err := os.Symlink(filepath.Join(root, filepath.FromSlash(linkArchive), "AGENT.md"), linkPath); err != nil {
		t.Logf("本平台不允许创建符号链接，跳过归档清单不可复算的断言：%v", err)
	} else {
		_, restoreErr := workspace.RestoreAgentDir(root, linkArchive, "target-d")
		require.ErrorContains(t, restoreErr, "清单无法复算")
		requireNoWorkspaceEntry(t, root, "agents/target-d")
		requireNoWorkspaceEntry(t, root, "agents/target-d.staging")
	}
}

// 中文验收点（约束 49）：复制 / 恢复只碰目录——生命周期状态文件一个都不写
// （目录由 basalt init 建好，内容必须仍为空），归档目录仍在原位。
// Acceptance point: copy and restore write no lifecycle state file and leave the
// archive in place.
func TestAgentRestore_LeavesArchiveAndLifecycleUntouched(t *testing.T) {
	root := initWorkspaceRoot(t)
	const archiveRel = "archive/source-agent-20261022-080000"
	writeFileInRoot(t, root, filepath.Join(archiveRel, "AGENT.md"), copySourceDefinition)

	writeFileInRoot(t, root, filepath.Join("agents", "source-agent", "AGENT.md"), copySourceDefinition)
	_, err := workspace.CopyAgentDir(root, "source-agent", "copied-agent", nil)
	require.NoError(t, err)
	_, err = workspace.RestoreAgentDir(root, archiveRel, "restored-agent")
	require.NoError(t, err)

	requireNoLifecycleFiles(t, root)
	require.Equal(t, copySourceDefinition, readWorkspaceFile(t, root, filepath.Join(archiveRel, "AGENT.md")),
		"恢复后归档目录保持原样，等待存储层标记 restored_at")
	requireNoWorkspaceEntry(t, root, "agents/copied-agent.staging")
	requireNoWorkspaceEntry(t, root, "agents/restored-agent.staging")
}
