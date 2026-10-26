package builtin

// 本文件是内置 Agent 开箱路径的单元测试：清单、安装 / 保留 / 强制三种结果、模型清空、
// 内置 MCP 条目的追加与零写入、样例库与 DBHub 配置的生成三态。
//
// 判据只读事实（目录与文件字节、返回的结构体），不 stub 嵌入素材：`examples.Assets`
// 是随二进制发布的同一份素材，测另一份复制品只会让两边慢慢分家。真实解释器路径
// （`EnsureSampleDB` 的 installed 分支）依赖本机 Python，不在这里判——它由
// `internal/cli` 的安装器测试与 `internal/e2e` 的制品测试覆盖。
// These unit tests read the embedded assets themselves; the interpreter-dependent branch
// of EnsureSampleDB is covered by the installer and artifact tests instead.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgents_ListsFiveBuiltinsInInstallOrder 判清单本身：五只、名字与示例目录逐字一致、
// 顺序即安装顺序，且返回的是副本——调用方排序改不动下一份读数。
func TestAgents_ListsFiveBuiltinsInInstallOrder(t *testing.T) {
	want := []string{"biz-daily", "docs-diff", "github-radar", "industry-brief", "repo-digest"}
	got := Agents()
	if len(got) != len(want) {
		t.Fatalf("内置 Agent 数量 = %d，期望 %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("第 %d 只 = %q，期望 %q", i, got[i].Name, name)
		}
		if got[i].Title == "" {
			t.Errorf("%s 缺少显示标题", name)
		}
	}

	// 2026-10-26 裁决：industry-brief 走内置 web_search，只声明 fetch 一条 MCP，
	// 且不再要求 BRAVE_API_KEY。
	brief := findAgent(t, got, "industry-brief")
	if len(brief.MCPServers) != 1 || brief.MCPServers[0] != "fetch" {
		t.Errorf("industry-brief 的 MCP 依赖 = %v，期望 [fetch]", brief.MCPServers)
	}
	if len(brief.EnvKeys) != 0 {
		t.Errorf("industry-brief 不应要求任何环境变量，实际 %v", brief.EnvKeys)
	}

	// 副本语义：改返回值不影响第二次读数。
	got[0].Name = "mutated"
	got[0].MCPServers = append(got[0].MCPServers, "mutated")
	again := Agents()
	if again[0].Name != "biz-daily" || len(again[0].MCPServers) != 1 {
		t.Fatalf("Agents() 返回的不是副本：%+v", again[0])
	}
}

// TestInstall_CreatesBlankModelTreesAndSkills 判新工作区的安装形态：五份 AGENT.md 都在、
// 模型字段一律留空、技能装到 `<ws>/skills` 而不是 Agent 目录里。
func TestInstall_CreatesBlankModelTreesAndSkills(t *testing.T) {
	ws := t.TempDir()
	results, err := Install(ws, false, "")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("安装结果 = %d 条，期望 5", len(results))
	}
	for _, result := range results {
		if result.State != StateInstalled {
			t.Errorf("%s 的目录状态 = %q，期望 %q", result.Name, result.State, StateInstalled)
		}
		agentFile := filepath.Join(ws, agentsDirName, result.Name, agentDefinitionFile)
		raw, err := os.ReadFile(agentFile)
		if err != nil {
			t.Fatalf("读 %s: %v", agentFile, err)
		}
		if !strings.Contains(string(raw), `  model: ""`) {
			t.Errorf("%s 的模型字段没有留空：\n%s", result.Name, raw)
		}
		// skills/ 由安装器单独装到 <ws>/skills：复制进 Agent 目录只会让加载器打 WARN。
		if _, err := os.Stat(filepath.Join(ws, agentsDirName, result.Name, skillsDirName)); err == nil {
			t.Errorf("%s 目录里不应出现 skills/", result.Name)
		}
	}

	digest := findResult(t, results, "repo-digest")
	if digest.Skill != "repo-digest" || digest.SkillState != StateInstalled {
		t.Errorf("repo-digest 技能 = %q/%q，期望 repo-digest/%s", digest.Skill, digest.SkillState, StateInstalled)
	}
	if _, err := os.Stat(filepath.Join(ws, skillsDirName, "repo-digest", "SKILL.md")); err != nil {
		t.Fatalf("技能没有装到 %s：%v", filepath.Join(ws, skillsDirName, "repo-digest"), err)
	}
}

// TestInstall_SubstitutesInterpreterForFreshAndKept 判解释器占位符的两条路径：新装时与
// 留空模型同一次写入替换；保留态只替换占位符、不动用户填好的模型绑定（旧版本装出来的
// 目录升级后也要对齐，否则示例脚本一直 `sandbox_denied`）。
func TestInstall_SubstitutesInterpreterForFreshAndKept(t *testing.T) {
	ws := t.TempDir()
	interp := filepath.Join(t.TempDir(), "python.exe")
	if _, err := Install(ws, false, interp); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, spec := range Agents() {
		agentFile := filepath.Join(ws, agentsDirName, spec.Name, agentDefinitionFile)
		raw, err := os.ReadFile(agentFile)
		if err != nil {
			t.Fatalf("读 %s: %v", agentFile, err)
		}
		if strings.Contains(string(raw), PythonPlaceholder) {
			t.Errorf("%s 仍含未替换的占位符", spec.Name)
		}
		if !strings.Contains(string(raw), interp) {
			t.Errorf("%s 没有写入解释器 %q", spec.Name, interp)
		}
	}

	// 模拟旧版本装出来的保留态：占位符还在，模型已由用户填好。
	agentFile := filepath.Join(ws, agentsDirName, "docs-diff", agentDefinitionFile)
	raw, err := os.ReadFile(agentFile)
	if err != nil {
		t.Fatalf("读 %s: %v", agentFile, err)
	}
	stale := strings.ReplaceAll(string(raw), interp, PythonPlaceholder)
	stale = strings.Replace(stale, `  model: ""`, "  model: 用户填的模型", 1)
	if err := os.WriteFile(agentFile, []byte(stale), 0o600); err != nil {
		t.Fatalf("写回保留态: %v", err)
	}
	results, err := Install(ws, false, interp)
	if err != nil {
		t.Fatalf("二次 Install: %v", err)
	}
	if state := findResult(t, results, "docs-diff").State; state != StateKept {
		t.Fatalf("已存在目录状态 = %q，期望 %q", state, StateKept)
	}
	raw, err = os.ReadFile(agentFile)
	if err != nil {
		t.Fatalf("读 %s: %v", agentFile, err)
	}
	if strings.Contains(string(raw), PythonPlaceholder) {
		t.Error("保留态的占位符没有被替换")
	}
	if !strings.Contains(string(raw), "  model: 用户填的模型") {
		t.Error("保留态的模型绑定被改动")
	}

	if _, err := Install(t.TempDir(), false, "python"); err == nil {
		t.Fatal("相对路径解释器应被拒绝")
	}
}

// TestResolveAgentPython_SkipsMissingFile 判文件不存在时静默跳过：保留态目录里没有
// AGENT.md（用户自己删了）不该让整轮安装失败。
func TestResolveAgentPython_SkipsMissingFile(t *testing.T) {
	interp := filepath.Join(t.TempDir(), "python.exe")
	if err := ResolveAgentPython(filepath.Join(t.TempDir(), "AGENT.md"), interp); err != nil {
		t.Fatalf("文件不存在时应跳过，实际 %v", err)
	}
	if err := ResolveAgentPython(filepath.Join(t.TempDir(), "AGENT.md"), ""); err != nil {
		t.Fatalf("空解释器应跳过，实际 %v", err)
	}
}

// TestInstall_KeepsExistingAndForceRewrites 判幂等口径：已存在的定义逐字节保留；只有
// force 才重写，且重写后的模型字段仍是空串。
func TestInstall_KeepsExistingAndForceRewrites(t *testing.T) {
	ws := t.TempDir()
	if _, err := Install(ws, false, ""); err != nil {
		t.Fatalf("首次 Install: %v", err)
	}
	agentFile := filepath.Join(ws, agentsDirName, "docs-diff", agentDefinitionFile)
	custom := "用户改过的正文\n"
	if err := os.WriteFile(agentFile, []byte(custom), 0o600); err != nil {
		t.Fatalf("写自定义定义: %v", err)
	}

	results, err := Install(ws, false, "")
	if err != nil {
		t.Fatalf("二次 Install: %v", err)
	}
	if state := findResult(t, results, "docs-diff").State; state != StateKept {
		t.Errorf("已存在的目录状态 = %q，期望 %q", state, StateKept)
	}
	raw, err := os.ReadFile(agentFile)
	if err != nil {
		t.Fatalf("读 %s: %v", agentFile, err)
	}
	if string(raw) != custom {
		t.Fatalf("非 force 安装改写了已存在的定义：%q", raw)
	}

	results, err = Install(ws, true, "")
	if err != nil {
		t.Fatalf("force Install: %v", err)
	}
	if state := findResult(t, results, "docs-diff").State; state != StateInstalled {
		t.Errorf("force 后状态 = %q，期望 %q", state, StateInstalled)
	}
	raw, err = os.ReadFile(agentFile)
	if err != nil {
		t.Fatalf("读 %s: %v", agentFile, err)
	}
	if string(raw) == custom {
		t.Fatal("force 安装没有重写已存在的定义")
	}
	if !strings.Contains(string(raw), `  model: ""`) {
		t.Errorf("force 重写后的模型字段没有留空：\n%s", raw)
	}
}

// TestCopyTree_SkipDropsFirstLevel 判子目录黑名单只作用于目标根下一级：同名目录在更深
// 层级时照常复制。
func TestCopyTree_SkipDropsFirstLevel(t *testing.T) {
	// 目标必须尚不存在：既存的目录一律走 kept（见下一个用例）。
	dst := filepath.Join(t.TempDir(), "repo-digest")
	state, err := CopyTree("daily/repo-digest", dst, false, map[string]bool{"skills": true})
	if err != nil {
		t.Fatalf("CopyTree: %v", err)
	}
	if state != StateInstalled {
		t.Fatalf("状态 = %q，期望 %q", state, StateInstalled)
	}
	if _, err := os.Stat(filepath.Join(dst, "AGENT.md")); err != nil {
		t.Fatalf("AGENT.md 没有被复制：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "skills")); err == nil {
		t.Fatal("skip 名单没有拦下第一级 skills/")
	}
	if _, err := os.Stat(filepath.Join(dst, "scripts")); err != nil {
		t.Fatalf("非黑名单目录应照常复制：%v", err)
	}
}

// TestCopyTree_KeepsExistingTarget 判目标已存在时的保留语义。
func TestCopyTree_KeepsExistingTarget(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "AGENT.md"), []byte("自定义"), 0o600); err != nil {
		t.Fatalf("准备目标: %v", err)
	}
	state, err := CopyTree("daily/docs-diff", dst, false, nil)
	if err != nil {
		t.Fatalf("CopyTree: %v", err)
	}
	if state != StateKept {
		t.Fatalf("状态 = %q，期望 %q", state, StateKept)
	}
	raw, err := os.ReadFile(filepath.Join(dst, "AGENT.md"))
	if err != nil {
		t.Fatalf("读 AGENT.md: %v", err)
	}
	if string(raw) != "自定义" {
		t.Fatalf("既有文件被覆盖：%q", raw)
	}
}

// TestBlankModel_TouchesOnlyProviderModel 判只改 provider 块里的那一行：其他键、缩进与
// 正文原样保留。
func TestBlankModel_TouchesOnlyProviderModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, agentDefinitionFile)
	source := "---\n" +
		"name: demo\n" +
		"provider:\n" +
		"  name: local-model\n" +
		"  model: 请在此填写模型名\n" +
		"  temperature: 0.2\n" +
		"tools:\n" +
		"  - read_file\n" +
		"---\n" +
		"正文提到 model: 也不该被改\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("准备 %s: %v", path, err)
	}
	if err := BlankModel(path); err != nil {
		t.Fatalf("BlankModel: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	want := strings.Replace(source, "  model: 请在此填写模型名", `  model: ""`, 1)
	if string(raw) != want {
		t.Fatalf("改写结果与期望不一致：\n--- 实际 ---\n%s\n--- 期望 ---\n%s", raw, want)
	}
}

// TestBlankModel_RejectsMissingProviderBlock provider 块不存在时必须报错，而不是写坏文件。
func TestBlankModel_RejectsMissingProviderBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, agentDefinitionFile)
	source := "---\nname: demo\ntools:\n  - read_file\n---\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("准备 %s: %v", path, err)
	}
	err := BlankModel(path)
	if err == nil {
		t.Fatal("缺少 provider.model 时应报错")
	}
	if !strings.Contains(err.Error(), "provider.model") {
		t.Errorf("错误信息未点名 provider.model：%v", err)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("读 %s: %v", path, readErr)
	}
	if string(raw) != source {
		t.Fatalf("报错路径不应改写文件：%q", raw)
	}
}

// TestBlankModel_MissingFile 目标不存在时报错且不新建。
func TestBlankModel_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, agentDefinitionFile)
	if err := BlankModel(path); err == nil {
		t.Fatal("目标文件不存在时应报错")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("报错路径不应创建文件")
	}
}

// TestEntries_CommandAndPlaceholderShapes 判条目构造：命令取解析后的路径、GitHub 凭据
// 只以 `${VAR}` 占位出现、白名单由条目派生且去重跳过空命令。
func TestEntries_CommandAndPlaceholderShapes(t *testing.T) {
	paths := Paths{
		DocsReadOnly: `C:\app\runtime\docs-readonly.exe`,
		DocsRoot:     `C:\ws\samples\docs`,
		Node:         `C:\app\runtime\node\node.exe`,
		DBHubEntry:   `C:\app\runtime\dbhub\dist\index.js`,
		DBHubConfig:  `C:\ws\demos\mcp\dbhub.toml`,
		GitHub:       `C:\app\runtime\github\github-mcp-server.exe`,
		Python:       `C:\app\runtime\python\python.exe`,
		Fetch:        `C:\app\runtime\fetch\run_fetch.py`,
	}
	free := CredentialFreeEntries(paths)
	if len(free) != 3 {
		t.Fatalf("免凭据条目 = %d 条，期望 3", len(free))
	}
	if free[0].Name != "docs-readonly" || free[0].Command != paths.DocsReadOnly || len(free[0].Env) != 0 {
		t.Errorf("docs-readonly 条目形态不符：%+v", free[0])
	}
	// 缺 `-root` 时 server 落在进程当前目录——真实实例上就是整个工作区，生成物会被
	// 当成文档（2026-10-27 docs-diff「迭代上限」的根因）。根目录必须显式写进参数。
	// Without -root the server lists the whole workspace; the root must be explicit.
	if want := []string{"-root", paths.DocsRoot}; strings.Join(free[0].Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("docs-readonly 参数 = %v，期望 %v", free[0].Args, want)
	}
	// 调用方没算出根目录时（老单元测试 / 半截装配）保持升级前的形态，不凭空吐一个裸 -root。
	// A caller without a root keeps the pre-upgrade shape instead of a bare -root.
	if legacy := CredentialFreeEntries(Paths{DocsReadOnly: paths.DocsReadOnly}); len(legacy[0].Args) != 0 {
		t.Errorf("没有根目录时不应写出 -root：%v", legacy[0].Args)
	}
	if free[1].Name != "dbhub" || free[1].Command != paths.Node {
		t.Errorf("dbhub 条目形态不符：%+v", free[1])
	}
	wantArgs := []string{paths.DBHubEntry, "--transport", "stdio", "--config", paths.DBHubConfig}
	if strings.Join(free[1].Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf("dbhub 参数 = %v，期望 %v", free[1].Args, wantArgs)
	}
	// fetch 由随包 Python 运行组件目录里的启动脚本：两个路径都必须来自发行布局，
	// 依赖轴才有版本段可比对。
	if free[2].Name != "fetch" || free[2].Command != paths.Python ||
		strings.Join(free[2].Args, "\x00") != paths.Fetch || len(free[2].Env) != 0 {
		t.Errorf("fetch 条目形态不符：%+v", free[2])
	}

	github := GitHubEntry(paths)
	if github.Name != "github" || github.Command != paths.GitHub {
		t.Errorf("github 条目形态不符：%+v", github)
	}
	if got := github.Env[GitHubTokenEnv]; got != "${"+GitHubTokenEnv+"}" {
		t.Errorf("github 凭据占位 = %q，期望 ${%s}", got, GitHubTokenEnv)
	}

	commands := AllowlistedCommands(paths)
	if len(commands) != 4 {
		t.Fatalf("白名单命令 = %d 条，期望 4：%v", len(commands), commands)
	}
	for _, command := range commands {
		if strings.Contains(command, "${") || strings.TrimSpace(command) == "" {
			t.Errorf("白名单里出现占位或空命令：%q", command)
		}
	}

	// 未解析的组件不进白名单（空命令既不登记也不写条目）。
	partial := Paths{DocsReadOnly: paths.DocsReadOnly, Node: paths.Node}
	if got := AllowlistedCommands(partial); len(got) != 2 {
		t.Errorf("部分路径的白名单 = %v，期望两条", got)
	}
}

// TestGitHubTokenSource_ExplicitAliasesOnly 判凭据来源只按显式别名表识别：规范名优先，
// gh CLI 的 `GITHUB_TOKEN` 次之，其它"看起来像凭据"的变量一概不认（猜错就是把别的
// 凭据递给第三方进程），空值 / 纯空白不算就绪。
func TestGitHubTokenSource_ExplicitAliasesOnly(t *testing.T) {
	lookupFrom := func(values map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			value, ok := values[name]
			return value, ok
		}
	}
	cases := []struct {
		name   string
		values map[string]string
		want   string
		wantOK bool
	}{
		{name: "仅规范名", values: map[string]string{GitHubTokenEnv: "x"}, want: GitHubTokenEnv, wantOK: true},
		{name: "仅 gh 别名", values: map[string]string{"GITHUB_TOKEN": "x"}, want: "GITHUB_TOKEN", wantOK: true},
		{
			name:   "两都在取规范名",
			values: map[string]string{GitHubTokenEnv: "x", "GITHUB_TOKEN": "y"},
			want:   GitHubTokenEnv,
			wantOK: true,
		},
		{name: "空白值不算就绪", values: map[string]string{"GITHUB_TOKEN": "  "}},
		{name: "近似名不猜", values: map[string]string{"GITHUB_PAT": "x", "GH_TOKEN": "y"}},
		{name: "都没有", values: map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := GitHubTokenSource(lookupFrom(tc.values))
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("GitHubTokenSource = (%q, %v)，期望 (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
	if _, ok := GitHubTokenSource(nil); ok {
		t.Error("nil 查找函数应报未就绪")
	}
}

// TestGitHubEntryWithToken_AliasKeepsCanonicalChildKey 判别名只影响占位符引用哪一支变量：
// 子进程的环境键恒为 `GITHUB_PERSONAL_ACCESS_TOKEN`，值永远是 `${VAR}` 占位。
func TestGitHubEntryWithToken_AliasKeepsCanonicalChildKey(t *testing.T) {
	entry := GitHubEntryWithToken(Paths{GitHub: "/app/github-mcp-server"}, "GITHUB_TOKEN")
	if entry.Name != "github" || entry.Command != "/app/github-mcp-server" {
		t.Fatalf("条目形态不符：%+v", entry)
	}
	if got := entry.Env[GitHubTokenEnv]; got != "${GITHUB_TOKEN}" {
		t.Errorf("别名来源的占位 = %q，期望 ${GITHUB_TOKEN}", got)
	}
	if len(entry.Env) != 1 {
		t.Errorf("github 条目只应有一个环境键：%v", entry.Env)
	}
	// 空来源名退回规范名：绝不写出 `${{}}` 之类的空占位。
	fallback := GitHubEntryWithToken(Paths{GitHub: "/app/github-mcp-server"}, " ")
	if got := fallback.Env[GitHubTokenEnv]; got != "${"+GitHubTokenEnv+"}" {
		t.Errorf("空来源名的占位 = %q，期望 ${%s}", got, GitHubTokenEnv)
	}
}

// TestEnsureMCP_AppendsMissingAndKeepsExisting 判追加语义：缺的写上、已有的一字节不动、
// 再次调用零写入。
func TestEnsureMCP_AppendsMissingAndKeepsExisting(t *testing.T) {
	ws := t.TempDir()
	configPath := filepath.Join(ws, mcpServersFileName)
	existing := "servers:\n" +
		"  - name: mine\n" +
		"    transport: stdio\n" +
		"    command: /opt/mine\n"
	if err := os.WriteFile(configPath, []byte(existing), 0o600); err != nil {
		t.Fatalf("准备 %s: %v", configPath, err)
	}

	paths := Paths{
		DocsReadOnly: "/app/docs-readonly",
		Node:         "/app/node",
		DBHubEntry:   "/app/dbhub.js",
		DBHubConfig:  "/ws/dbhub.toml",
		Python:       "/app/python",
		Fetch:        "/app/fetch/run_fetch.py",
	}
	entries := CredentialFreeEntries(paths)
	first, err := EnsureMCP(ws, entries)
	if err != nil {
		t.Fatalf("EnsureMCP: %v", err)
	}
	if want := []string{"docs-readonly", "dbhub", "fetch"}; strings.Join(first.Written, ",") != strings.Join(want, ",") {
		t.Errorf("写入名单 = %v，期望 %v", first.Written, want)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("读 %s: %v", configPath, err)
	}
	text := string(raw)
	for _, want := range []string{"- name: mine", "- name: docs-readonly", "- name: dbhub"} {
		if !strings.Contains(text, want) {
			t.Errorf("配置文件缺少 %q：\n%s", want, text)
		}
	}
	// 已有条目必须仍在最前面：追加不是重排。
	if strings.Index(text, "- name: mine") > strings.Index(text, "- name: docs-readonly") {
		t.Errorf("既有序被打乱：\n%s", text)
	}

	second, err := EnsureMCP(ws, entries)
	if err != nil {
		t.Fatalf("二次 EnsureMCP: %v", err)
	}
	if len(second.Written) != 0 {
		t.Errorf("第二次仍写入 %v，期望零写入", second.Written)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("读 %s: %v", configPath, err)
	}
	if string(after) != text {
		t.Fatalf("零写入路径改动了文件：\n%s", after)
	}
}

// TestEnsureMCP_CreatesFileWhenAbsent 目标文件不存在时新建并只含两条内置条目。
func TestEnsureMCP_CreatesFileWhenAbsent(t *testing.T) {
	ws := t.TempDir()
	paths := Paths{DocsReadOnly: "/app/docs-readonly"}
	result, err := EnsureMCP(ws, CredentialFreeEntries(paths)[:1])
	if err != nil {
		t.Fatalf("EnsureMCP: %v", err)
	}
	if len(result.Written) != 1 || result.Written[0] != "docs-readonly" {
		t.Fatalf("写入名单 = %v", result.Written)
	}
	raw, err := os.ReadFile(filepath.Join(ws, mcpServersFileName))
	if err != nil {
		t.Fatalf("读配置文件: %v", err)
	}
	if !strings.Contains(string(raw), "servers:") {
		t.Fatalf("新建文件缺少顶层键：\n%s", raw)
	}
}

// TestEnsureMCP_RefusesBlankCommand 组件路径没解析出来时拒绝写入空命令条目，并点名。
func TestEnsureMCP_RefusesBlankCommand(t *testing.T) {
	ws := t.TempDir()
	entries := []Entry{{Name: "docs-readonly", Command: ""}}
	result, err := EnsureMCP(ws, entries)
	if err == nil {
		t.Fatal("空命令条目应被拒绝")
	}
	if !strings.Contains(err.Error(), "docs-readonly") {
		t.Errorf("错误信息未点名条目：%v", err)
	}
	if len(result.Written) != 0 {
		t.Errorf("拒绝路径不应报告写入：%v", result.Written)
	}
	if _, statErr := os.Stat(filepath.Join(ws, mcpServersFileName)); statErr == nil {
		t.Fatal("拒绝路径不应创建配置文件")
	}
}

// TestEnsureMCP_EmptyEntriesNoop 空条目清单不创建文件。
func TestEnsureMCP_EmptyEntriesNoop(t *testing.T) {
	ws := t.TempDir()
	result, err := EnsureMCP(ws, nil)
	if err != nil {
		t.Fatalf("EnsureMCP: %v", err)
	}
	if len(result.Written) != 0 || len(result.Present) != 0 {
		t.Errorf("空清单结果 = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(ws, mcpServersFileName)); err == nil {
		t.Fatal("空清单不应创建配置文件")
	}
}

// TestEnsureMCP_RejectsBrokenDocument 既有文件不是合法 YAML 块时报错，且不改写文件。
func TestEnsureMCP_RejectsBrokenDocument(t *testing.T) {
	ws := t.TempDir()
	configPath := filepath.Join(ws, mcpServersFileName)
	broken := "servers: [unclosed\n"
	if err := os.WriteFile(configPath, []byte(broken), 0o600); err != nil {
		t.Fatalf("准备 %s: %v", configPath, err)
	}
	if _, err := EnsureMCP(ws, []Entry{{Name: "docs-readonly", Command: "/app/docs-readonly"}}); err == nil {
		t.Fatal("损坏的定义文件应报错")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("读 %s: %v", configPath, err)
	}
	if string(raw) != broken {
		t.Fatalf("报错路径改写了文件：%q", raw)
	}
}

// TestSampleAndDBHubPaths_AreStable 判两个固定落点与只读配置模板。
func TestSampleAndDBHubPaths_AreStable(t *testing.T) {
	ws := t.TempDir()
	if got, want := SampleDBPath(ws), filepath.Join(ws, "samples", "biz-sample.db"); got != want {
		t.Errorf("样例库落点 = %q，期望 %q", got, want)
	}
	if got, want := DBHubConfigPath(ws), filepath.Join(ws, "demos", "mcp", "dbhub.toml"); got != want {
		t.Errorf("DBHub 配置落点 = %q，期望 %q", got, want)
	}

	dbPath := filepath.Join(ws, "samples", "biz-sample.db")
	config := DBHubConfig(dbPath)
	for _, want := range []string{
		`id = "default"`,
		"sqlite:///",
		"readonly = true",
		"max_rows = 500",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("DBHub 配置缺少 %q：\n%s", want, config)
		}
	}
	if strings.Contains(config, `\`) {
		t.Errorf("DSN 里出现了反斜杠（应为正斜杠）：\n%s", config)
	}
}

// TestDBHubConfig_DSNShape 判两种平台的 DSN 形态：POSIX 绝对路径与 Windows 盘符
// 路径都必须恰好三个斜杠。多一个斜杠会被 DBHub 的 URL 解析当成 `//host` 样式，
// 去掉两个斜杠后变成相对路径，报 unable to open database file（WSL2 实测）。
func TestDBHubConfig_DSNShape(t *testing.T) {
	cases := []struct {
		name   string
		dbPath string
		want   string
	}{
		{"posix", "/ws/samples/biz-sample.db", `dsn = "sqlite:///ws/samples/biz-sample.db"`},
		{"windows", "C:/ws/samples/biz-sample.db", `dsn = "sqlite:///C:/ws/samples/biz-sample.db"`},
		{"relative", "samples/biz-sample.db", `dsn = "sqlite:///samples/biz-sample.db"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if config := DBHubConfig(tc.dbPath); !strings.Contains(config, tc.want) {
				t.Fatalf("DBHubConfig(%q) 缺少 %q：\n%s", tc.dbPath, tc.want, config)
			}
		})
	}
}

// TestEnsureDBHubConfig_WritesThenKeeps 判配置的三态：新建、保留、force 重写。
func TestEnsureDBHubConfig_WritesThenKeeps(t *testing.T) {
	ws := t.TempDir()
	dbPath := SampleDBPath(ws)

	path, state, err := EnsureDBHubConfig(ws, dbPath, false)
	if err != nil {
		t.Fatalf("EnsureDBHubConfig: %v", err)
	}
	if state != StateInstalled {
		t.Errorf("首次状态 = %q，期望 %q", state, StateInstalled)
	}
	want := DBHubConfig(dbPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	if string(raw) != want {
		t.Fatalf("配置正文与模板不一致：\n%s", raw)
	}

	if _, state, err = EnsureDBHubConfig(ws, dbPath, false); err != nil || state != StateKept {
		t.Fatalf("二次调用 = %q/%v，期望 kept/nil", state, err)
	}
	if err := os.WriteFile(path, []byte("自定义"), 0o600); err != nil {
		t.Fatalf("改配置: %v", err)
	}
	if _, state, err = EnsureDBHubConfig(ws, dbPath, true); err != nil || state != StateInstalled {
		t.Fatalf("force 调用 = %q/%v，期望 installed/nil", state, err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	if string(raw) != want {
		t.Fatalf("force 没有恢复模板：\n%s", raw)
	}
}

// TestDescribeSampleDBNote 判摘要压缩：合法 JSON 出事实行、非 JSON 原样保留第一行。
func TestDescribeSampleDBNote(t *testing.T) {
	summary := `{"table":"orders","rows":{"count":15},"db":{"sha256":"0123456789abcdef0123"}}`
	got := DescribeSampleDBNote(summary)
	want := "orders 表 15 行，库 sha256 0123456789ab…"
	if got != want {
		t.Errorf("摘要 = %q，期望 %q", got, want)
	}

	if got := DescribeSampleDBNote("done\n第二行"); got != "done" {
		t.Errorf("非 JSON 输出 = %q，期望首行 done", got)
	}
	if got := DescribeSampleDBNote(`{"table":"t","rows":{"count":1},"db":{"sha256":"short"}}`); !strings.Contains(got, "short") {
		t.Errorf("短哈希应原样保留，实际 %q", got)
	}
}

// TestFirstLine 判首行提取（含无换行与空串）。
func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"a\nb":   "a",
		"only":   "only",
		"":       "",
		"\nx":    "",
		"a\r\nb": "a\r",
	}
	for input, want := range cases {
		if got := FirstLine(input); got != want {
			t.Errorf("FirstLine(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// TestFindPython_ReportsMissingCandidates PATH 为空时必须报错并说明找过哪些名字，
// 而不是静默返回空串（那会把「没有解释器」误报成「脚本失败」）。
func TestFindPython_ReportsMissingCandidates(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	interpreter, err := FindPython(context.Background())
	if err == nil {
		t.Fatalf("PATH 为空时不应找到解释器，实际返回 %q", interpreter)
	}
	if !strings.Contains(err.Error(), "python3") || !strings.Contains(err.Error(), "py") {
		t.Errorf("错误信息应列出探测过的名字：%v", err)
	}
}

// TestEnsureSampleDB_PendingWithoutInterpreter 缺解释器时记 pending 而不让调用方失败。
func TestEnsureSampleDB_PendingWithoutInterpreter(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ws := t.TempDir()
	result, err := EnsureSampleDB(context.Background(), ws, false)
	if err != nil {
		t.Fatalf("缺解释器不应返回错误：%v", err)
	}
	if result.State != StatePending {
		t.Errorf("状态 = %q，期望 %q", result.State, StatePending)
	}
	if !strings.Contains(result.Note, "Python 3") {
		t.Errorf("待生成说明未点名 Python 3：%q", result.Note)
	}
	if _, statErr := os.Stat(result.Path); statErr == nil {
		t.Fatal("待生成路径不应产出文件")
	}
}

// TestEnsureSampleDB_KeepsExistingDatabase 样例库已存在时直接保留，不再探测解释器。
func TestEnsureSampleDB_KeepsExistingDatabase(t *testing.T) {
	ws := t.TempDir()
	dbPath := SampleDBPath(ws)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("建样例目录: %v", err)
	}
	if err := os.WriteFile(dbPath, []byte("existing"), 0o600); err != nil {
		t.Fatalf("准备样例库: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	result, err := EnsureSampleDB(context.Background(), ws, false)
	if err != nil {
		t.Fatalf("EnsureSampleDB: %v", err)
	}
	if result.State != StateKept || result.Path != dbPath {
		t.Errorf("结果 = %+v，期望 kept/%s", result, dbPath)
	}
}

// findAgent 按名字取清单项；测试清单本身时用。
func findAgent(t *testing.T, agents []Agent, name string) Agent {
	t.Helper()
	for _, agent := range agents {
		if agent.Name == name {
			return agent
		}
	}
	t.Fatalf("清单里没有 %s", name)
	return Agent{}
}

// findResult 按名字取安装结果；测试安装输出时用。
func findResult(t *testing.T, results []AgentResult, name string) AgentResult {
	t.Helper()
	for _, result := range results {
		if result.Name == name {
			return result
		}
	}
	t.Fatalf("安装结果里没有 %s", name)
	return AgentResult{}
}
