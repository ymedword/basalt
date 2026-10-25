package cli

// 本文件是 `basalt demo install` 的关键回归用例：安装只写素材与清单、不写模型 /
// 凭据 / 权限，重复安装保留用户改动，`--dry-run` 不碰磁盘，缺工作区时报错指向
// `basalt init`。包内测试（`package cli`）以便直接对 `demoSpecs` 与内嵌素材断言。
// In-package tests pin the installer's three boundaries: assets only, idempotent,
// and dry-run writes nothing.

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/examples"
)

// runDemoInstallCmd 在给定工作目录上执行一次 `basalt demo install`，返回合并输出与错误。
// runDemoInstallCmd runs one install and returns the merged output plus the error.
func runDemoInstallCmd(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()

	root := NewRootCommand(Deps{WorkDir: dir})
	root.SetArgs(append([]string{"demo", "install"}, args...))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// initDemoWorkspace 只铺 `basalt init` 的最小形态：`<ws>/agents` 存在即可——安装器
// 的判据是「工作区已初始化」，不重复校验 init 自己负责的文件。
// initDemoWorkspace lays down the minimum an initialized workspace has.
func initDemoWorkspace(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, workspaceDirName, agentsDirName), 0o755))
	return dir
}

// readDemoManifest 读回安装清单。
// readDemoManifest reads the manifest written by the installer.
func readDemoManifest(t *testing.T, dir string) demoManifest {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, workspaceDirName, "demos", "install.json"))
	require.NoError(t, err)
	var manifest demoManifest
	require.NoError(t, json.Unmarshal(data, &manifest))
	return manifest
}

// TestDemoInstall_InstallsAllDemosAndManifest 断言五份 Demo、技能与清单全部就位，
// 且每条 AGENT.md 的模型被清空（装好但拒绝注册，等用户配置）。
// TestDemoInstall_InstallsAllDemosAndManifest checks the five demos, the skill and
// the manifest, and that every installed AGENT.md carries a blank model.
func TestDemoInstall_InstallsAllDemosAndManifest(t *testing.T) {
	dir := initDemoWorkspace(t)

	stdout, err := runDemoInstallCmd(t, dir)
	require.NoError(t, err)

	for _, spec := range demoSpecs {
		agentFile := filepath.Join(dir, workspaceDirName, agentsDirName, spec.Name, "AGENT.md")
		data, readErr := os.ReadFile(agentFile)
		require.NoError(t, readErr, "%s 未安装", spec.Name)
		text := string(data)
		require.Contains(t, text, `model: ""`, "%s 的模型未被清空", spec.Name)
		require.Contains(t, text, "name: "+spec.Name)
		require.NotContains(t, text, "请在此填写模型名")
	}

	manifest := readDemoManifest(t, dir)
	require.Equal(t, 1, manifest.SchemaVersion)
	require.Len(t, manifest.Demos, len(demoSpecs))
	for _, item := range manifest.Demos {
		require.Equal(t, demoStateInstalled, item.State, item.Name)
		require.NotEmpty(t, item.Pending, "%s 的待配置清单为空", item.Name)
	}

	skillDir := filepath.Join(dir, workspaceDirName, demoSkillsDirName, "repo-digest", "SKILL.md")
	_, err = os.Stat(skillDir)
	require.NoError(t, err, "repo-digest 技能未安装到工作区 skills/")

	require.Contains(t, stdout, "待配置")
	require.Contains(t, stdout, filepath.Join(dir, workspaceDirName, "demos", "install.json"))
}

// TestDemoInstall_BlankModelOnlyTouchesOneLine 断言安装器只改 provider.model 一行，
// 其余正文逐字保真——素材是已审交付物，安装不得顺手改写。
// TestDemoInstall_BlankModelOnlyTouchesOneLine pins the edit to exactly one line.
func TestDemoInstall_BlankModelOnlyTouchesOneLine(t *testing.T) {
	dir := initDemoWorkspace(t)

	_, err := runDemoInstallCmd(t, dir, "--demo", "biz-daily")
	require.NoError(t, err)

	source, err := fs.ReadFile(examples.Assets, "daily/biz-daily/AGENT.md")
	require.NoError(t, err)
	want := strings.Replace(string(source), "  model: 请在此填写模型名", `  model: ""`, 1)
	require.NotEqual(t, string(source), want, "素材里没有待清空的模型行——本用例的前提已变")

	got, err := os.ReadFile(filepath.Join(dir, workspaceDirName, agentsDirName, "biz-daily", "AGENT.md"))
	require.NoError(t, err)
	require.Equal(t, want, string(got))
}

// TestDemoInstall_KeepsExistingWithoutForce 断言重复安装保留用户改动：没有 --force
// 时不覆盖已有 Agent 目录，并如实报告「保留」。
// TestDemoInstall_KeepsExistingWithoutForce keeps user edits unless forced.
func TestDemoInstall_KeepsExistingWithoutForce(t *testing.T) {
	dir := initDemoWorkspace(t)
	_, err := runDemoInstallCmd(t, dir)
	require.NoError(t, err)

	marker := filepath.Join(dir, workspaceDirName, agentsDirName, "biz-daily", "user-note.txt")
	require.NoError(t, os.WriteFile(marker, []byte("用户自己的改动"), 0o644))

	stdout, err := runDemoInstallCmd(t, dir)
	require.NoError(t, err)
	require.Contains(t, stdout, "已存在，保留")

	_, statErr := os.Stat(marker)
	require.NoError(t, statErr, "无 --force 时不得覆盖已有 Agent 目录")
	manifest := readDemoManifest(t, dir)
	for _, item := range manifest.Demos {
		require.Equal(t, demoStateKept, item.State, item.Name)
	}
}

// TestDemoInstall_ForceReplacesAgent 断言 --force 真的重建目录。
// TestDemoInstall_ForceReplacesAgent checks that --force rebuilds the directory.
func TestDemoInstall_ForceReplacesAgent(t *testing.T) {
	dir := initDemoWorkspace(t)
	_, err := runDemoInstallCmd(t, dir, "--demo", "docs-diff")
	require.NoError(t, err)

	marker := filepath.Join(dir, workspaceDirName, agentsDirName, "docs-diff", "user-note.txt")
	require.NoError(t, os.WriteFile(marker, []byte("旧改动"), 0o644))

	_, err = runDemoInstallCmd(t, dir, "--demo", "docs-diff", "--force")
	require.NoError(t, err)

	_, statErr := os.Stat(marker)
	require.ErrorIs(t, statErr, fs.ErrNotExist, "--force 应清掉旧目录里的残留")
}

// TestDemoInstall_DryRunWritesNothing 断言试运行零写入。
// TestDemoInstall_DryRunWritesNothing asserts a dry run touches nothing.
func TestDemoInstall_DryRunWritesNothing(t *testing.T) {
	dir := initDemoWorkspace(t)

	stdout, err := runDemoInstallCmd(t, dir, "--dry-run")
	require.NoError(t, err)
	require.Contains(t, stdout, "试运行")

	entries, readErr := os.ReadDir(filepath.Join(dir, workspaceDirName, agentsDirName))
	require.NoError(t, readErr)
	require.Empty(t, entries, "--dry-run 不得写出任何 Agent")
	_, statErr := os.Stat(filepath.Join(dir, workspaceDirName, "demos"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "--dry-run 不得写安装清单")
	// 计划里要能看到会生成的两份文件，否则「不写」与「不打算写」分不清。
	require.Contains(t, stdout, "dbhub.toml")
	require.Contains(t, stdout, filepath.Join("samples", "biz-sample.db"))
}

// TestDemoInstall_RequiresInitializedWorkspace 断言未初始化工作区时报错并指向
// `basalt init`——安装器不替 init 建骨架。
// TestDemoInstall_RequiresInitializedWorkspace points the operator at basalt init.
func TestDemoInstall_RequiresInitializedWorkspace(t *testing.T) {
	dir := t.TempDir()

	_, err := runDemoInstallCmd(t, dir)
	require.Error(t, err)
	require.ErrorContains(t, err, "basalt init")
}

// TestDemoInstall_RejectsUnknownDemo 断言未知名字报错并列出可选项（不静默忽略）。
// TestDemoInstall_RejectsUnknownDemo fails loudly on a typo.
func TestDemoInstall_RejectsUnknownDemo(t *testing.T) {
	dir := initDemoWorkspace(t)

	_, err := runDemoInstallCmd(t, dir, "--demo", "biz-daliy")
	require.Error(t, err)
	require.ErrorContains(t, err, "biz-daliy")
	require.ErrorContains(t, err, "biz-daily")
}

// TestDemoInstall_SelectionInstallsOnlyChosenDemo 断言 --demo 只装选中的那份，
// 且报告里的固定版本依赖表也只列用得上的条目。
// TestDemoInstall_SelectionInstallsOnlyChosenDemo scopes both files and the pinned
// dependency table to the selection.
func TestDemoInstall_SelectionInstallsOnlyChosenDemo(t *testing.T) {
	dir := initDemoWorkspace(t)

	stdout, err := runDemoInstallCmd(t, dir, "--demo", "docs-diff")
	require.NoError(t, err)

	manifest := readDemoManifest(t, dir)
	require.Len(t, manifest.Demos, 1)
	require.Equal(t, "docs-diff", manifest.Demos[0].Name)
	require.Nil(t, manifest.SampleDB, "未选 biz-daily 时不生成样例库")

	_, statErr := os.Stat(filepath.Join(dir, workspaceDirName, agentsDirName, "biz-daily"))
	require.ErrorIs(t, statErr, fs.ErrNotExist)
	require.Contains(t, stdout, "docs-readonly")
	require.NotContains(t, stdout, "@bytebase/dbhub@")
}

// TestDemoInstall_WritesDBHubReadOnlyConfig 断言安装器生成的 DBHub 配置是 1.5.0 的
// 只读形状：一个 source 指向样例库、只暴露 execute_sql、只读与行数上限写在配置里
// （该版本已移除 `--readonly` 参数），并在报告里给出 --config 片段。
// TestDemoInstall_WritesDBHubReadOnlyConfig pins the dbhub 1.5.0 read-only shape.
func TestDemoInstall_WritesDBHubReadOnlyConfig(t *testing.T) {
	dir := initDemoWorkspace(t)

	stdout, err := runDemoInstallCmd(t, dir, "--demo", "biz-daily")
	require.NoError(t, err)

	manifest := readDemoManifest(t, dir)
	require.NotEmpty(t, manifest.DBHubConfig)
	data, readErr := os.ReadFile(manifest.DBHubConfig)
	require.NoError(t, readErr)
	text := string(data)
	for _, want := range []string{
		"[[sources]]",
		`id = "default"`,
		`dsn = "sqlite:///`,
		"[[tools]]",
		`name = "execute_sql"`,
		"readonly = true",
		"max_rows = 500",
	} {
		require.Contains(t, text, want)
	}
	require.NotContains(t, text, "--readonly", "1.5.0 已移除该启动参数")
	require.Contains(t, text,
		filepath.ToSlash(demoDBHubSamplePath(filepath.Join(dir, workspaceDirName))),
		"DSN 必须指向本次生成的样例库")
	// 报告给出可粘贴片段；接入仍由用户确认后自己写进 mcp_servers.yaml。
	require.Contains(t, stdout, "--config")
	require.Contains(t, stdout, filepath.ToSlash(manifest.DBHubConfig))
	_, statErr := os.Stat(filepath.Join(dir, workspaceDirName, "mcp_servers.yaml"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "安装器不得写 mcp_servers.yaml")
}

// TestDemoInstall_SkipsDBHubConfigWithoutSampleDemo 断言没选 biz-daily 时既不生成
// 样例库，也不留下指向不存在文件的 DBHub 配置。
// TestDemoInstall_SkipsDBHubConfigWithoutSampleDemo keeps dbhub.toml out of the run.
func TestDemoInstall_SkipsDBHubConfigWithoutSampleDemo(t *testing.T) {
	dir := initDemoWorkspace(t)

	_, err := runDemoInstallCmd(t, dir, "--demo", "github-radar")
	require.NoError(t, err)

	manifest := readDemoManifest(t, dir)
	require.Empty(t, manifest.DBHubConfig)
	_, statErr := os.Stat(filepath.Join(dir, workspaceDirName, "demos", "mcp", "dbhub.toml"))
	require.ErrorIs(t, statErr, fs.ErrNotExist)
}

// TestDemoInstall_NeverWritesCredentials 断言安装产物里不出现任何凭据值，只出现
// 环境变量名——凭据一律由用户在进程环境里给。
// TestDemoInstall_NeverWritesCredentials asserts only env-var names are written.
func TestDemoInstall_NeverWritesCredentials(t *testing.T) {
	dir := initDemoWorkspace(t)

	_, err := runDemoInstallCmd(t, dir)
	require.NoError(t, err)

	// 判据是「有没有真值」，不是「有没有出现变量名」——SETUP 本来就要教用户
	// 设置这些变量，占位写法（空值 / <...> / $VAR）是合法内容。
	// The criterion is a real-looking value, not the mere mention of a key name.
	credentialShapes := []*regexp.Regexp{
		regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`BSA[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`),
	}
	var offenders []string
	walkErr := filepath.WalkDir(filepath.Join(dir, workspaceDirName), func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		for _, shape := range credentialShapes {
			if hit := shape.Find(data); hit != nil {
				offenders = append(offenders, p+" 含疑似凭据 "+string(hit[:min(len(hit), 8)])+"…")
			}
		}
		return nil
	})
	require.NoError(t, walkErr)
	require.Empty(t, offenders, "安装产物里出现了凭据赋值形态：%v", offenders)
}

// TestDemoInstall_GeneratesSampleDBWhenInterpreterAvailable 在装了 Python 3 的机器上
// 断言样例库真的生成；缺解释器时记跳过（理由写进测试日志），不代表通过。
// TestDemoInstall_GeneratesSampleDBWhenInterpreterAvailable skips with a reason when
// no Python 3 is installed; a skip is not a pass.
func TestDemoInstall_GeneratesSampleDBWhenInterpreterAvailable(t *testing.T) {
	if _, err := findDemoPython(context.Background()); err != nil {
		t.Skipf("本机没有可用的 Python 3（%v）：样例库生成未验证（缺解释器 = 未验证，不是通过）", err)
	}
	dir := initDemoWorkspace(t)

	_, err := runDemoInstallCmd(t, dir, "--demo", "biz-daily")
	require.NoError(t, err)

	manifest := readDemoManifest(t, dir)
	require.NotNil(t, manifest.SampleDB)
	require.Equal(t, demoStateInstalled, manifest.SampleDB.State, manifest.SampleDB.Note)
	info, statErr := os.Stat(manifest.SampleDB.Path)
	require.NoError(t, statErr)
	require.Positive(t, info.Size())
}

// TestDemoInstall_ExportsDocsReadOnlySource 断言自研 MCP 源码真的落到工作区、带上
// 可独立构建的最小 go.mod，且报告里的构建命令指向实际路径（不是打印一个不存在的目录）。
// TestDemoInstall_ExportsDocsReadOnlySource proves the source export and its go.mod.
func TestDemoInstall_ExportsDocsReadOnlySource(t *testing.T) {
	dir := initDemoWorkspace(t)

	stdout, err := runDemoInstallCmd(t, dir, "--demo", "docs-diff")
	require.NoError(t, err)

	wsRoot := filepath.Join(dir, workspaceDirName)
	srcDir := filepath.Join(wsRoot, "demos", "src", "docs-readonly")
	for _, name := range []string{"main.go", "main_test.go", "go.mod"} {
		_, statErr := os.Stat(filepath.Join(srcDir, name))
		require.NoError(t, statErr, "导出目录缺少 %s", name)
	}
	gomod, readErr := os.ReadFile(filepath.Join(srcDir, "go.mod"))
	require.NoError(t, readErr)
	require.Contains(t, string(gomod), "module basalt-docs-readonly")
	require.Contains(t, string(gomod), "go 1.27")
	// 最小 go.mod：不得夹带 require —— 一旦导出物引入第三方坐标，独立构建就不再
	// 是"纯标准库、可离线"，这条断言防止将来悄悄加依赖。
	// Minimal means no requires: the exported build must stay stdlib-only and offline.
	require.NotContains(t, string(gomod), "require", "导出目录的 go.mod 必须零第三方依赖")

	// 逐字节比对：导出物必须与仓库里那一份源码完全相同（单一事实源，不是再写一遍）。
	for _, name := range []string{"main.go", "main_test.go"} {
		want, readErr := os.ReadFile(filepath.Join("..", "..", "examples", "shared", "mcp", "docs-readonly", name))
		require.NoError(t, readErr, "读取仓库源码 %s 失败", name)
		got, readErr := os.ReadFile(filepath.Join(srcDir, name))
		require.NoError(t, readErr)
		require.Equal(t, want, got, "导出物 %s 与仓库源码不一致（出现了第二份副本？）", name)
	}

	depsDir := filepath.Join(wsRoot, "demos", "deps")
	info, statErr := os.Stat(depsDir)
	require.NoError(t, statErr, "依赖目录未创建")
	require.True(t, info.IsDir())

	manifest := readDemoManifest(t, dir)
	require.NotNil(t, manifest.DocsReadOnly)
	require.Equal(t, demoStateInstalled, manifest.DocsReadOnly.State)
	require.Equal(t, srcDir, manifest.DocsReadOnly.Path)

	require.Contains(t, stdout, srcDir)
	buildCmd := "go -C " + srcDir + " build -o " + filepath.Join(depsDir, "docs-readonly.exe") + " ."
	require.Contains(t, stdout, buildCmd, "报告里的构建命令必须按实际路径渲染")
}

// TestDemoInstall_ReportsKeptSourceWithoutGoMod 断言保留态的报告不说谎：没有 --force 时
// 不重写既有目录；若那里原本没有 go.mod，报告必须点名"未写"，不能宣称"已生成"——
// 否则用户会按一行注定失败的构建命令操作。
// TestDemoInstall_ReportsKeptSourceWithoutGoMod makes the kept-state report truthful.
func TestDemoInstall_ReportsKeptSourceWithoutGoMod(t *testing.T) {
	dir := initDemoWorkspace(t)
	_, err := runDemoInstallCmd(t, dir, "--demo", "docs-diff")
	require.NoError(t, err)

	wsRoot := filepath.Join(dir, workspaceDirName)
	srcDir := filepath.Join(wsRoot, "demos", "src", "docs-readonly")
	require.NoError(t, os.Remove(filepath.Join(srcDir, "go.mod")), "前置：删掉上一次导出的 go.mod")

	stdout, err := runDemoInstallCmd(t, dir, "--demo", "docs-diff")
	require.NoError(t, err)

	manifest := readDemoManifest(t, dir)
	require.NotNil(t, manifest.DocsReadOnly)
	require.Equal(t, demoStateKept, manifest.DocsReadOnly.State)

	_, statErr := os.Stat(filepath.Join(srcDir, "go.mod"))
	require.True(t, os.IsNotExist(statErr), "无 --force 时不得替用户补写 go.mod")

	require.Contains(t, stdout, "保留原目录，未写 go.mod")
	require.NotContains(t, stdout, "docs-readonly 源码 "+srcDir+"（已存在，保留，已生成 go.mod）",
		"保留态不得声称已生成 go.mod")
}

// TestDemoInstall_KeepsSkillsOutOfAgentDirs 断言 Agent 目录不含 skills/ 副本：
// 技能只装到 <workspace>/skills，否则加载器会打 WARN 并忽略 Agent 内的那份。
// TestDemoInstall_KeepsSkillsOutOfAgentDirs keeps a single skill copy in the workspace.
func TestDemoInstall_KeepsSkillsOutOfAgentDirs(t *testing.T) {
	dir := initDemoWorkspace(t)

	_, err := runDemoInstallCmd(t, dir, "--demo", "repo-digest")
	require.NoError(t, err)

	wsRoot := filepath.Join(dir, workspaceDirName)
	spec := findDemoSpec(t, "repo-digest")
	require.NotEmpty(t, spec.Skill, "repo-digest 应声明技能")

	_, statErr := os.Stat(filepath.Join(wsRoot, agentsDirName, spec.Name, "skills"))
	require.True(t, os.IsNotExist(statErr), "%s 的 Agent 目录里仍有 skills/ 副本", spec.Name)
	_, skillErr := os.Stat(filepath.Join(wsRoot, demoSkillsDirName, spec.Skill))
	require.NoError(t, skillErr, "技能 %s 未装到 %s", spec.Skill, demoSkillsDirName)
}

// TestDemoInstall_DoesNotTouchExistingConfig 断言已有的 basalt.yaml / mcp_servers.yaml
// 逐字节不变：安装器不改配置、不改权限，接入由用户显式确认后自行完成。
// TestDemoInstall_DoesNotTouchExistingConfig pins byte equality of operator config.
func TestDemoInstall_DoesNotTouchExistingConfig(t *testing.T) {
	dir := initDemoWorkspace(t)
	wsRoot := filepath.Join(dir, workspaceDirName)

	configs := map[string]string{
		"basalt.yaml":      "shell:\n  allowed_commands:\n    - node\n",
		"mcp_servers.yaml": "servers:\n  - name: keep-me\n    transport: stdio\n    command: node\n",
	}
	for name, body := range configs {
		require.NoError(t, os.WriteFile(filepath.Join(wsRoot, name), []byte(body), 0o644))
	}

	_, err := runDemoInstallCmd(t, dir)
	require.NoError(t, err)

	for name, body := range configs {
		data, readErr := os.ReadFile(filepath.Join(wsRoot, name))
		require.NoError(t, readErr)
		require.Equal(t, body, string(data), "%s 被安装器改写了", name)
	}
}

// TestDemoInstall_CredentialHintUsesSelectedDemo 断言凭据提示指向本次真正安装的
// Demo 的 SETUP，而不是写死某个未选中的 Demo。
// TestDemoInstall_CredentialHintUsesSelectedDemo points the hint at the chosen demo.
func TestDemoInstall_CredentialHintUsesSelectedDemo(t *testing.T) {
	dir := initDemoWorkspace(t)

	stdout, err := runDemoInstallCmd(t, dir, "--demo", "repo-digest")
	require.NoError(t, err)

	wsRoot := filepath.Join(dir, workspaceDirName)
	require.Contains(t, stdout, filepath.Join(wsRoot, agentsDirName, "repo-digest", "SETUP.md"))
	require.NotContains(t, stdout, filepath.Join(wsRoot, agentsDirName, "github-radar", "SETUP.md"))
}

// findDemoSpec 按名字取安装描述，取不到直接判失败。
// findDemoSpec returns the demo spec by name or fails the test.
func findDemoSpec(t *testing.T, name string) demoSpec {
	t.Helper()
	for _, spec := range demoSpecs {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("未找到 Demo %s", name)
	return demoSpec{}
}
