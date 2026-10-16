package workspace_test

// 本文件是 US-3 写模式文件面的关键回归用例（颗粒度文档 §3.2 的 agent_files_test.go
// 九例；A8 = 模板可装载、A9 = 归档语义）。夹具沿用 agent_test.go 的家族（§2.4 注 8）。
// Regression tests for the write path: template creation and archive-delete.
// Fixtures are shared with agent_test.go.

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/workspace"
)

// archiveNow 是归档用例的统一固定时刻——DeleteAgent 的 now 由调用方显式传入
// （§2.2 约束 8：时间显式传入是可判卷前提，归档名含时间戳）。
// archiveNow is the fixed instant every archive test passes in.
func archiveNow() time.Time {
	return time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
}

// archiveStamp 是 archiveNow 对应的归档名时间戳（20060102-150405 格式）。
// archiveStamp is the timestamp component derived from archiveNow.
const archiveStamp = "20261016-093000"

// dirNames 列出目录内的名字（os.ReadDir 已按文件名排序——清单顺序与平台无关）。
// 零写入判据用：新建 / 覆盖文件都会改变这个清单。
// dirNames lists the sorted entry names of a directory, for zero-write assertions.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	items, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name())
	}
	return names
}

// initWorkspaceRoot 造一个**已初始化**的工作区根——写模式命令面只在初始化过的工作区里跑
// （agents/ 缺失是命令面要点名的错误，不是写模式函数的常规输入；archive/ 由 init 建）。
// initWorkspaceRoot returns a workspace root built through the init path.
func initWorkspaceRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)
	return root
}

// 中文验收点：create 产出**可装载**的模板——占位模型是普通文本、天然放行（技术方案
// §11.5），provider.name 取配置数组首项（明示"选中了谁"的结果面），tools 为空列表；
// created 报出本次落盘的相对路径（供命令输出）。
// Acceptance point: the created template loads and registers as is.
func TestCreateAgent_WritesLoadableTemplate(t *testing.T) {
	root := initWorkspaceRoot(t)

	created, err := workspace.CreateAgent(root, "demo-agent", []string{"deepseek", "openai"})
	require.NoError(t, err)
	require.Equal(t, []string{"agents/demo-agent/AGENT.md"}, created)

	// 模板必须经**同一条装载路径**注册成功——create 的判据是"可被加载"，不是"文件写出来了"。
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek", "openai"},
		Logger:    bufferedLogger(t),
	}).LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "模板应当可装载：%s", entries[0].RejectReason)
	require.Equal(t, "demo-agent", entries[0].Name)                // 目录名 = frontmatter name
	require.Equal(t, "deepseek", entries[0].Profile.Provider.Name) // 配置数组首项（§2.3 流程 C）
	require.NotEmpty(t, entries[0].Profile.Provider.Model)         // 占位模型名非空白 → 放行（规则 3）
	require.Empty(t, entries[0].Profile.Tools)                     // tools: []（记忆工具到 US-6 才在池）
}

// 中文验收点：已存在目录 → 拒绝且**零写入**——既有目录及其内容逐字节保留（不覆盖、
// 不追加、不动兄弟文件）。"先写后判"的实现会在这里改掉既有文件。
// Acceptance point: an existing agent directory is refused without a single byte written.
func TestCreateAgent_RefusesExistingDirectory(t *testing.T) {
	root := initWorkspaceRoot(t)
	original := "---\nname: demo-agent\nprovider: {name: deepseek, model: my-own-model}\n---\n\n既有正文\n"
	writeFileInRoot(t, root, filepath.Join("agents", "demo-agent", "AGENT.md"), original)

	created, err := workspace.CreateAgent(root, "demo-agent", []string{"deepseek"})
	require.Error(t, err)
	require.Empty(t, created)
	require.Contains(t, err.Error(), "demo-agent") // 点名被拒的目录

	got, readErr := os.ReadFile(filepath.Join(root, "agents", "demo-agent", "AGENT.md"))
	require.NoError(t, readErr)
	require.Equal(t, original, string(got), "既有定义文件被改写了")
	require.Equal(t, []string{"AGENT.md"}, dirNames(t, filepath.Join(root, "agents", "demo-agent")),
		"既有目录里出现了新文件（拒绝路径必须零写入）")
}

// 中文验收点：Agent 名非法 → 拒绝且零写入；create / delete **共用同一校验函数**
// （同一份判据，不得各写一份——§3.1 ValidateAgentName 注释）。合法但少见的名字
// （空格 / 中文 / 井号）必须放行，不得过度收紧。
// Acceptance point: invalid names are refused by the shared validator, odd ones pass.
func TestCreateAgent_RejectsInvalidName(t *testing.T) {
	cases := []struct {
		label string
		name  string
	}{
		{"empty", ""},
		{"slash", "a/b"},
		{"backslash", `a\b`},
		{"dot", "."},
		{"dotdot", ".."},
		{"nul", "a\x00b"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			require.Error(t, workspace.ValidateAgentName(tc.name), "校验函数必须拒绝：%q", tc.name)

			root := initWorkspaceRoot(t)
			created, err := workspace.CreateAgent(root, tc.name, []string{"deepseek"})
			require.Error(t, err)
			require.Empty(t, created)
			require.Empty(t, dirNames(t, filepath.Join(root, "agents")), "拒绝路径必须零写入")
		})
	}

	// 合法形态：单个目录名即可——空格 / 中文 / 井号都不是分隔符，不得被过度收紧。
	for _, name := range []string{"spacey agent", "演示 智能体", "agent#1"} {
		require.NoError(t, workspace.ValidateAgentName(name), "合法名字被误拒：%q", name)
	}
}

// 中文验收点：模板里的名字经 **YAML 编码**写入（不是字符串插值）——provider 名含
// 冒号 / 井号时插值会写坏 YAML（`name: prov: v2 #1`），编码写入必须原样往返。
// Acceptance point: names go through the YAML encoder, so odd names round-trip.
func TestCreateAgent_EncodesNamesThroughYAML(t *testing.T) {
	root := initWorkspaceRoot(t)
	name := "spacey agent"
	provider := "prov: v2 #1"

	created, err := workspace.CreateAgent(root, name, []string{provider})
	require.NoError(t, err)
	require.NotEmpty(t, created)

	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{provider},
		Logger:    bufferedLogger(t),
	}).LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, name, entries[0].Name)
	require.True(t, entries[0].Registered, "含冒号 / 井号的名字应当原样往返：%s", entries[0].RejectReason)
	require.Equal(t, provider, entries[0].Profile.Provider.Name)
}

// 中文验收点：写模式命令被活实例持锁 → 拒绝启动且**零业务写入**（agents/ 一个字节
// 都没建）；错误是 *LockBusyError 且能读到持有者诊断（诊断在锁定区之外——A10）。
// 锁文件本身是取锁的副产物，不算业务写入。
// Acceptance point: a held instance lock refuses creation before any business write.
func TestCreateAgent_RefusesWhenInstanceLockHeld(t *testing.T) {
	root := initWorkspaceRoot(t)

	lock, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })

	created, err := workspace.CreateAgent(root, "demo-agent", []string{"deepseek"})
	require.Error(t, err)
	require.Empty(t, created)

	var busy *workspace.LockBusyError
	require.ErrorAs(t, err, &busy)
	require.Equal(t, os.Getpid(), busy.PID, "拒锁文案必须点名持有者的 pid")
	require.Contains(t, err.Error(), "pid="+strconv.Itoa(os.Getpid()))
	require.Empty(t, dirNames(t, filepath.Join(root, "agents")), "拒锁路径必须零业务写入")
}

// 中文验收点：delete 的语义是**归档**——单次原子移动进 archive/<name>-<时间戳>/，
// 会话 / 记忆 / 审计数据一个字节都不动（DA §5.14）。
// Acceptance point: delete archives the agent directory and leaves every data file intact.
func TestDeleteAgent_ArchivesWithoutTouchingData(t *testing.T) {
	root := initWorkspaceRoot(t)
	writeFileInRoot(t, root, filepath.Join("agents", "compat-check", "AGENT.md"), compatSampleAgentMD)
	// 会话 / 记忆 / 审计三份数据（归档**不得**触碰）。
	sessionData := `{"session":"s1"}`
	memoryData := "# 长期记忆\n\n记忆占位。\n"
	auditData := `{"event":"llm.call"}`
	writeFileInRoot(t, root, filepath.Join("sessions", "s1.json"), sessionData)
	writeFileInRoot(t, root, filepath.Join("memory", "MEMORY.md"), memoryData)
	writeFileInRoot(t, root, filepath.Join("audit", "audit.jsonl"), auditData)

	archived, err := workspace.DeleteAgent(root, "compat-check", archiveNow())
	require.NoError(t, err)
	require.Equal(t, "archive/compat-check-"+archiveStamp, archived)

	// 源目录已移出、归档副本内容一致（单次 rename——不是复制 + 删除）。
	require.NoDirExists(t, filepath.Join(root, "agents", "compat-check"))
	archivedDef, readErr := os.ReadFile(filepath.Join(root, "archive", "compat-check-"+archiveStamp, "AGENT.md"))
	require.NoError(t, readErr)
	require.Equal(t, compatSampleAgentMD, string(archivedDef))

	// 会话 / 记忆 / 审计数据逐字节保持原样。
	for _, f := range []struct{ rel, want string }{
		{filepath.Join("sessions", "s1.json"), sessionData},
		{filepath.Join("memory", "MEMORY.md"), memoryData},
		{filepath.Join("audit", "audit.jsonl"), auditData},
	} {
		got, err := os.ReadFile(filepath.Join(root, f.rel))
		require.NoError(t, err, "%s 不应被动过", f.rel)
		require.Equal(t, f.want, string(got), "%s 内容被改动了", f.rel)
	}
}

// 中文验收点：目录不存在 → 拒绝并点名（不是静默成功）；归档目录不得凭空出现（零写入）。
// Acceptance point: deleting a missing agent fails loudly and writes nothing.
func TestDeleteAgent_RefusesMissingAgent(t *testing.T) {
	root := initWorkspaceRoot(t)

	archived, err := workspace.DeleteAgent(root, "ghost-agent", archiveNow())
	require.Error(t, err)
	require.Empty(t, archived)
	require.Contains(t, err.Error(), "ghost-agent")
	require.Empty(t, dirNames(t, filepath.Join(root, "archive")), "拒绝路径不得创建归档副本")
}

// 中文验收点：Agent 名非法 → 拒绝且零写入；与 create 同一校验函数（同一表逐条判同一结果）。
// Acceptance point: the delete path shares the validator with the create path.
func TestDeleteAgent_RejectsInvalidName(t *testing.T) {
	for _, name := range []string{"", "a/b", `a\b`, ".", "..", "a\x00b"} {
		root := initWorkspaceRoot(t)
		archived, err := workspace.DeleteAgent(root, name, archiveNow())
		require.Error(t, err, "非法名字必须被拒：%q", name)
		require.Empty(t, archived)
		require.Empty(t, dirNames(t, filepath.Join(root, "archive")), "拒绝路径不得创建归档副本")
	}
}

// 中文验收点（R2 评审修复）：delete 归档的是**目录**——agents/ 下的普通文件不是 Agent
// 目录（装载面同样忽略它），归档它等于把运维的手记挪进 archive/。判据：拒绝、源条目
// 逐字节保留、archive/ 一个条目都不许出现（零写入）。
// Acceptance point: a plain file under agents/ is refused and left untouched.
func TestDeleteAgent_RefusesNonDirectoryEntry(t *testing.T) {
	root := initWorkspaceRoot(t)
	notes := "运维随手记的说明\n"
	writeFileInRoot(t, root, filepath.Join("agents", "notes.txt"), notes)

	archived, err := workspace.DeleteAgent(root, "notes.txt", archiveNow())
	require.Error(t, err)
	require.Empty(t, archived)
	require.Contains(t, err.Error(), "notes.txt") // 点名被拒的条目
	require.Contains(t, err.Error(), "不是目录")
	require.Empty(t, dirNames(t, filepath.Join(root, "archive")), "拒绝路径不得创建归档副本")

	got, readErr := os.ReadFile(filepath.Join(root, "agents", "notes.txt"))
	require.NoError(t, readErr, "源条目必须原样保留")
	require.Equal(t, notes, string(got), "源条目内容被改动了")
}

// 中文验收点（R2 评审修复）：符号链接不是 Agent 目录——不得被归档，且判定**不跟随**
// 链接（跟随会拿链接指向的目标类型做判定，等于"指向哪就归档哪"；Lstat 的判定面）。
// 平台差异：Windows 建符号链接需要开发者模式 / 特权，建不出来时跳过（理由写明）。
// Acceptance point: a symlink is refused and never followed.
func TestDeleteAgent_RefusesSymlinkEntry(t *testing.T) {
	root := initWorkspaceRoot(t)
	target := t.TempDir()
	writeFileInRoot(t, target, filepath.Join("agent-dir", "AGENT.md"), "目标目录里的定义\n")
	link := filepath.Join(root, "agents", "link-agent")
	if err := os.Symlink(filepath.Join(target, "agent-dir"), link); err != nil {
		t.Skip("本平台不允许创建符号链接（Windows 需开发者模式或特权）：" + err.Error())
	}

	archived, err := workspace.DeleteAgent(root, "link-agent", archiveNow())
	require.Error(t, err)
	require.Empty(t, archived)
	require.Contains(t, err.Error(), "link-agent")
	require.Contains(t, err.Error(), "不是目录")
	require.Empty(t, dirNames(t, filepath.Join(root, "archive")), "拒绝路径不得创建归档副本")

	// 链接本身（仍是链接）与它的目标都原样保留。
	info, lstatErr := os.Lstat(link)
	require.NoError(t, lstatErr, "源条目必须原样保留")
	require.NotZero(t, info.Mode()&os.ModeSymlink, "源条目仍是符号链接")
	require.FileExists(t, filepath.Join(target, "agent-dir", "AGENT.md"), "链接目标不得被动过")
}

// 中文验收点：归档目标已存在 → 拒绝且**源目录原样保留**（"不得先删源后建目标"的可判据
// 形态——同一秒内的二次删除不得吃掉已归档的副本，§2.4 注 11）；既有归档副本也不得被覆盖。
// Acceptance point: a colliding archive target is refused and the source stays put.
func TestDeleteAgent_RefusesCollidingArchiveTarget(t *testing.T) {
	root := initWorkspaceRoot(t)
	writeFileInRoot(t, root, filepath.Join("agents", "compat-check", "AGENT.md"), compatSampleAgentMD)
	// 先放一份"上一秒的归档"占位（与本次删除同名的目标目录）。
	previous := "上一份归档副本占位\n"
	writeFileInRoot(t, root, filepath.Join("archive", "compat-check-"+archiveStamp, "AGENT.md"), previous)

	archived, err := workspace.DeleteAgent(root, "compat-check", archiveNow())
	require.Error(t, err)
	require.Empty(t, archived)
	require.Contains(t, err.Error(), "compat-check-"+archiveStamp) // 点名撞名目标

	// 源目录及其定义文件原样保留（"先删后移"的实现会在这里判红）。
	require.DirExists(t, filepath.Join(root, "agents", "compat-check"))
	got, readErr := os.ReadFile(filepath.Join(root, "agents", "compat-check", "AGENT.md"))
	require.NoError(t, readErr)
	require.Equal(t, compatSampleAgentMD, string(got))

	// 既有归档副本**未被覆盖**。
	kept, readErr := os.ReadFile(filepath.Join(root, "archive", "compat-check-"+archiveStamp, "AGENT.md"))
	require.NoError(t, readErr)
	require.Equal(t, previous, string(kept))
}
