package cli

// 本文件是 review ② D-6 / C-55 的输出面判据：`--dry-run` 必须点名「模板 vs 已安装」
// 的三态，且 `--force` 的破坏性由此获得可见依据——它删掉的是整个目录，不只是正文。
// This file is the review ② D-6 regression: the dry-run must name the three-way state
// of each bundled template against its installed copy.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// demoPlanAgentFile 返回某个 Demo 装出来的 AGENT.md 路径（三态用例共用）。
// demoPlanAgentFile is the installed AGENT.md path for one demo.
func demoPlanAgentFile(t *testing.T, dir, name string) string {
	t.Helper()
	return filepath.Join(dir, workspaceDirName, agentsDirName, name, "AGENT.md")
}

// TestDemoPlan_ReportsTemplateDrift 守 dry-run 的三态（C-55）：
//
//	新建             —— 目录不存在，安装会新建；
//	正文与模板一致   —— 已存在且归一化后逐文件相同，安装会保留；
//	正文已修改       —— 已存在但正文与模板不同，安装**仍会保留**并点名差异文件。
//
// 归一化只抹安装器自己造成的差异：`provider.model` / `schedule` 等 frontmatter 是运维
// 填的实例数据，填了不得被判成"改过模板"；`{{BASALT_PYTHON}}` 的替换同理。
// 逆改点：把 `CompareTree` 换成"目标存在即 modified"→ 第二条用例红；把 `agentBody`
// 去掉、整份 AGENT.md 逐字比 → 第四条用例红。
// 验收点：C-55；review ② D-6。
func TestDemoPlan_ReportsTemplateDrift(t *testing.T) {
	dir := initDemoWorkspace(t)

	// —— 一、全新工作区：新建 ——
	stdout, err := runDemoInstallCmd(t, dir, "--dry-run", "--demo", "github-radar")
	require.NoError(t, err)
	require.Contains(t, stdout, demoPlanStateNew, "全新工作区的 dry-run 应判「新建」")

	// —— 二、原样装一份：正文与模板一致 ——
	_, err = runDemoInstallCmd(t, dir, "--demo", "github-radar")
	require.NoError(t, err)
	stdout, err = runDemoInstallCmd(t, dir, "--dry-run", "--demo", "github-radar")
	require.NoError(t, err)
	require.Contains(t, stdout, demoPlanStateIdentical, "刚装好的目录应与模板一致")
	require.NotContains(t, stdout, demoPlanStateModified)

	// —— 三、改正文：正文已修改，并点名 AGENT.md ——
	agentFile := demoPlanAgentFile(t, dir, "github-radar")
	raw, readErr := os.ReadFile(agentFile)
	require.NoError(t, readErr)
	require.NoError(t, os.WriteFile(agentFile, append(raw, []byte("\n本实例的补充：只在盘后跑一次。\n")...), 0o600))
	stdout, err = runDemoInstallCmd(t, dir, "--dry-run", "--demo", "github-radar")
	require.NoError(t, err)
	require.Contains(t, stdout, demoPlanStateModified)
	require.Contains(t, stdout, "AGENT.md", "差异必须点名文件")

	// —— 四、只把 frontmatter 的模型填上：不算改过模板正文 ——
	body := strings.Replace(string(raw), `  model: ""`, `  model: "deepseek-chat"`, 1)
	require.NotEqual(t, string(raw), body, "夹具前提：模板的 provider.model 应为空串")
	require.NoError(t, os.WriteFile(agentFile, []byte(body), 0o600))
	stdout, err = runDemoInstallCmd(t, dir, "--dry-run", "--demo", "github-radar")
	require.NoError(t, err)
	require.Contains(t, stdout, demoPlanStateIdentical,
		"运维填模型属于实例数据，不得被判成改过模板正文")
	require.NotContains(t, stdout, demoPlanStateModified)
}

// TestDemoInstall_KeptStateDoesNotRewriteAgentBody 守「无 --force 时不动用户正文」：
// 重复安装只做占位符对齐，正文逐字保留——这是升级路径不得依赖 `--force` 的直接依据。
// 逆改点：把 `if state == demoStateInstalled` 的守卫去掉（保留态也 BlankModel / 也
// 重写正文）→ 用户那行消失 → 本用例红。
// 验收点：C-55；review ② D-6。
func TestDemoInstall_KeptStateDoesNotRewriteAgentBody(t *testing.T) {
	dir := initDemoWorkspace(t)
	_, err := runDemoInstallCmd(t, dir, "--demo", "github-radar")
	require.NoError(t, err)

	agentFile := demoPlanAgentFile(t, dir, "github-radar")
	if raw, readErr := os.ReadFile(agentFile); readErr == nil {
		require.NotEmpty(t, raw)
	}
	edited := "用户改过的提示词：只在盘后跑一次。\n"
	require.NoError(t, os.WriteFile(agentFile, []byte(edited), 0o600))

	stdout, err := runDemoInstallCmd(t, dir, "--demo", "github-radar")
	require.NoError(t, err)
	require.Contains(t, stdout, "已存在，保留")

	got, readErr := os.ReadFile(agentFile)
	require.NoError(t, readErr)
	require.Equal(t, edited, string(got),
		"无 --force 时安装器不得改写已存在目录里的正文")
}

// TestDemoInstall_ForceRemovesWholeAgentDir 把 `--force` 的真实破坏面钉死：它删的是
// **整个** Agent 目录，用户放进同目录的其他文件一并消失。这正是运维文档不得把
// `--force` 写成"更新模板"的原因（D-6 的处理方式是备份 → dry-run 判漂移 → 人工合并）。
// 逆改点：把 `CopyTree` 的 force 分支改成"逐文件覆盖"→ runs.txt 仍在 → 本用例红。
// 验收点：C-55；review ② D-6。
func TestDemoInstall_ForceRemovesWholeAgentDir(t *testing.T) {
	dir := initDemoWorkspace(t)
	_, err := runDemoInstallCmd(t, dir, "--demo", "repo-digest")
	require.NoError(t, err)

	agentDir := filepath.Join(dir, workspaceDirName, agentsDirName, "repo-digest")
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("用户改过的正文\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "runs.txt"), []byte("历史运行\n"), 0o600))

	_, err = runDemoInstallCmd(t, dir, "--demo", "repo-digest", "--force")
	require.NoError(t, err)

	rebuilt, readErr := os.ReadFile(filepath.Join(agentDir, "AGENT.md"))
	require.NoError(t, readErr)
	require.NotContains(t, string(rebuilt), "用户改过的正文", "--force 会重建整份 AGENT.md")
	require.Contains(t, string(rebuilt), "name: repo-digest", "重建出的是模板内容")

	_, statErr := os.Stat(filepath.Join(agentDir, "runs.txt"))
	require.ErrorIs(t, statErr, fs.ErrNotExist,
		"--force 删掉整个目录：同目录的用户文件一并消失")
}
