package workspace_test

// 本文件是 Agent 目录写路径的关键回归用例（颗粒度文档 §3.2 的 agent_files_test.go
// 家族）：创建走 staging + 单次 rename、归档走单次 rename 并产清单、名称判据共用
// 同一函数、生命周期状态文件只有唯一写者。夹具沿用 agent_test.go 的家族（§2.4 注 8）。
// Regression tests for the agent directory write path: staged creation, archive with
// manifest, the shared name rules, and the single-writer lifecycle assertion.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// archiveNow 是归档用例的统一固定时刻——归档操作的 now 由调用方显式传入
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

// initWorkspaceRoot 造一个**已初始化**的工作区根——写路径只在初始化过的工作区里跑
// （agents/ 缺失是命令面要点名的错误，不是写路径函数的常规输入；archive/ 由 init 建）。
// initWorkspaceRoot returns a workspace root built through the init path.
func initWorkspaceRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)
	return root
}

// agentMDOf 读回一个 Agent 的定义文件原文。
// agentMDOf reads back an agent's definition file.
func agentMDOf(t *testing.T, root, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "agents", name, "AGENT.md"))
	require.NoError(t, err)
	return string(raw)
}

// 中文验收点：create 产出**可装载**的模板——占位模型是普通文本、天然放行（技术方案
// §11.5），provider.name 取配置数组首项（明示"选中了谁"的结果面），tools 为两件记忆
// 工具（US-6 起模板默认声明 `save_memory` / `recall_memory`——口径 26 / FR-026）；
// 返回的文件行报出本次落盘的相对路径与字节数。
// Acceptance point: the created template loads and registers as is.
func TestCreateAgent_WritesLoadableTemplate(t *testing.T) {
	root := initWorkspaceRoot(t)

	created, err := workspace.CreateAgentStaged(root, core.AgentSpec{Name: "demo-agent"}, []string{"deepseek", "openai"})
	require.NoError(t, err)
	require.Len(t, created, 1)
	require.Equal(t, "agents/demo-agent/AGENT.md", created[0].Path)
	require.Equal(t, int64(len(agentMDOf(t, root, "demo-agent"))), created[0].Size)

	// 模板必须经**同一条装载路径**注册成功——create 的判据是"可被加载"，不是"文件写出来了"。
	// 池按模板声明的两件记忆工具注入：零值（空池）会让模板这类"声明了 tools: 的 Agent"
	// **整只不注册**，注册断言随之落红——池与模板必须成对演进（T032 判的就是这条成对关系）。
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek", "openai"},
		Tools:     []string{"save_memory", "recall_memory"},
		Logger:    bufferedLogger(t),
	}).LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "模板应当可装载：%s", entries[0].RejectReason)
	require.Equal(t, "demo-agent", entries[0].Name)                // 目录名 = frontmatter name
	require.Equal(t, "deepseek", entries[0].Profile.Provider.Name) // 配置数组首项（§2.3 流程 C）
	require.NotEmpty(t, entries[0].Profile.Provider.Model)         // 占位模型名非空白 → 放行（规则 3）
	// tools 逐字（判据面随 US-6 变更：原为"空列表"——模板默认工具落位后语义改为两件记忆工具）
	require.Equal(t, []string{"save_memory", "recall_memory"}, entries[0].Profile.Tools)
}

// 中文验收点（文档 §3.2.2 §6.2 口径 21）：**默认值不变**——创建请求不带描述与模型时，
// 落盘内容与历史模板逐字一致（默认描述 / 占位模型是可见的"没配完"标记，不是新语义）。
// 判卷装置：同一时刻造两份文件，一份走默认、一份显式给出历史文案，两文件除名字外
// 逐字节相同——默认文案一旦漂移，这条断言立刻落红。
// Acceptance point: an empty description/model falls back to the historical literals
// byte for byte.
func TestCreateAgent_DefaultsUnchanged(t *testing.T) {
	root := initWorkspaceRoot(t)

	_, err := workspace.CreateAgentStaged(root, core.AgentSpec{Name: "bare-agent"}, []string{"deepseek"})
	require.NoError(t, err)
	_, err = workspace.CreateAgentStaged(root, core.AgentSpec{
		Name:        "explicit-agent",
		Description: "一句话说明这个 Agent 的职责（可留空）",
		Model:       "请在此填写模型名",
	}, []string{"deepseek"})
	require.NoError(t, err)

	bare := agentMDOf(t, root, "bare-agent")
	explicit := agentMDOf(t, root, "explicit-agent")
	require.Equal(t, strings.ReplaceAll(bare, "bare-agent", "explicit-agent"), explicit,
		"空描述 / 空模型的产物必须与显式给出历史默认文案的产物逐字节一致")

	// 再从文件里读回两个默认值（不经装载器：判的是模板声明的字面量本身）。
	var dto struct {
		Description string `yaml:"description"`
		Provider    struct {
			Model string `yaml:"model"`
		} `yaml:"provider"`
	}
	front, _, found := strings.Cut(bare, "\n---\n")
	require.True(t, found, "模板必须是 frontmatter 围栏形态")
	require.NoError(t, yaml.Unmarshal([]byte(front), &dto))
	require.Equal(t, "一句话说明这个 Agent 的职责（可留空）", dto.Description)
	require.Equal(t, "请在此填写模型名", dto.Provider.Model)
}

// 中文验收点：已存在目录 → 拒绝且**零写入**——既有目录及其内容逐字节保留（不覆盖、
// 不追加、不动兄弟文件）。"先写后判"的实现会在这里改掉既有文件。
// Acceptance point: an existing agent directory is refused without a single byte written.
func TestCreateAgent_RefusesExistingDirectory(t *testing.T) {
	root := initWorkspaceRoot(t)
	original := "---\nname: demo-agent\nprovider: {name: deepseek, model: my-own-model}\n---\n\n既有正文\n"
	writeFileInRoot(t, root, filepath.Join("agents", "demo-agent", "AGENT.md"), original)

	created, err := workspace.CreateAgentStaged(root, core.AgentSpec{Name: "demo-agent"}, []string{"deepseek"})
	require.Error(t, err)
	require.Empty(t, created)
	require.Contains(t, err.Error(), "demo-agent") // 点名被拒的目录

	require.Equal(t, original, agentMDOf(t, root, "demo-agent"), "既有定义文件被改写了")
	require.Equal(t, []string{"AGENT.md"}, dirNames(t, filepath.Join(root, "agents", "demo-agent")),
		"既有目录里出现了新文件（拒绝路径必须零写入）")
}

// 中文验收点：Agent 名非法 → 拒绝且零写入；create / delete **共用同一校验函数**
// （同一份判据，不得各写一份——§3.1 ValidateAgentName 注释）。点前缀与 `.staging` /
// `.backup` 后缀属写路径的残骸保留面（M9），必须拒绝；合法但少见的名字（空格 /
// 中文 / 井号）必须放行，不得过度收紧。
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
		{"leading_dot", ".hidden"},
		{"staging_suffix", "a.staging"},
		{"backup_suffix", "a.backup"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			require.Error(t, workspace.ValidateAgentName(tc.name), "校验函数必须拒绝：%q", tc.name)

			root := initWorkspaceRoot(t)
			created, err := workspace.CreateAgentStaged(root, core.AgentSpec{Name: tc.name}, []string{"deepseek"})
			require.Error(t, err)
			require.Empty(t, created)
			require.Empty(t, dirNames(t, filepath.Join(root, "agents")), "拒绝路径必须零写入")
		})
	}

	// 合法形态：单个目录名即可——空格 / 中文 / 井号都不是分隔符，不得被过度收紧。
	for _, name := range []string{"spacey agent", "演示 智能体", "agent#1", "agent.staging2"} {
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

	created, err := workspace.CreateAgentStaged(root, core.AgentSpec{Name: name}, []string{provider})
	require.NoError(t, err)
	require.NotEmpty(t, created)

	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{provider},
		Tools:     []string{"save_memory", "recall_memory"}, // 模板默认声明的两件（US-6）
		Logger:    bufferedLogger(t),
	}).LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, name, entries[0].Name)
	require.True(t, entries[0].Registered, "含冒号 / 井号的名字应当原样往返：%s", entries[0].RejectReason)
	require.Equal(t, provider, entries[0].Profile.Provider.Name)
}

// 中文验收点（H4：唯一取锁者）：目录操作**不取实例锁**——锁的归属是调用方
// （HTTP 管理请求复用服务已持有的锁；CLI 写路径在端口层取一次）。判据：活实例持锁时
// 创建照常成功，且锁**仍在本调用方手里**（第二次取锁必须被拒）。
// Acceptance point: directory creation never touches the instance lock.
func TestCreateAgent_DoesNotTakeInstanceLock(t *testing.T) {
	root := initWorkspaceRoot(t)

	lock, err := workspace.AcquireInstanceLock(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })

	created, err := workspace.CreateAgentStaged(root, core.AgentSpec{Name: "demo-agent"}, []string{"deepseek"})
	require.NoError(t, err, "目录操作不得因外层持锁而失败：锁归调用方")
	require.NotEmpty(t, created)
	require.FileExists(t, filepath.Join(root, "agents", "demo-agent", "AGENT.md"))

	// 锁仍在调用方手里：再次取锁必须被拒（若目录操作擅自取 / 放锁，这里会漏判）。
	second, err := workspace.AcquireInstanceLock(root)
	require.Error(t, err, "实例锁必须仍被本调用方持有")
	require.Nil(t, second)
}

// 中文验收点（约束 27 / 恢复路径第三态）：`agents/<name>.staging` 已存在 → 拒绝并点名，
// **不**替运维决定删除；正式目录不得出现，残留一字不动。
// Acceptance point: a stale staging tree is reported by name, never silently removed.
func TestCreateAgent_RefusesStagingResidue(t *testing.T) {
	root := initWorkspaceRoot(t)
	residue := "半成品占位\n"
	writeFileInRoot(t, root, filepath.Join("agents", "demo-agent.staging", "AGENT.md"), residue)

	created, err := workspace.CreateAgentStaged(root, core.AgentSpec{Name: "demo-agent"}, []string{"deepseek"})
	require.Error(t, err)
	require.Empty(t, created)
	require.Contains(t, err.Error(), "demo-agent.staging", "必须点名残留目录")

	require.NoDirExists(t, filepath.Join(root, "agents", "demo-agent"), "拒绝路径不得落地正式目录")
	got, readErr := os.ReadFile(filepath.Join(root, "agents", "demo-agent.staging", "AGENT.md"))
	require.NoError(t, readErr, "残留目录必须原样保留（是否删除归运维 / 恢复路径判定）")
	require.Equal(t, residue, string(got))
}

// 中文验收点：delete 的语义是**归档**——单次原子移动进 archive/<name>-<UTC 时间戳>/，
// 会话 / 记忆 / 审计数据一个字节都不动（DA §5.14）；返回值携带归档标识与清单。
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

	result, err := workspace.ArchiveAgentDir(root, "compat-check", archiveNow())
	require.NoError(t, err)
	require.Equal(t, "compat-check-"+archiveStamp, result.ArchiveID)
	require.NotEmpty(t, result.ManifestJSON)
	require.Len(t, result.ContentHash, 64, "content_hash 是裸的 64 位十六进制")

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

// 中文验收点（约束 32）：归档标识用 **UTC**——注入时刻的时区不影响归档名
// （+08:00 的 17:30 落成同一 UTC 时刻的 09:30）。
// Acceptance point: the archive id is formatted in UTC regardless of the injected zone.
func TestDeleteAgent_UsesUTCStamp(t *testing.T) {
	root := initWorkspaceRoot(t)
	writeFileInRoot(t, root, filepath.Join("agents", "compat-check", "AGENT.md"), compatSampleAgentMD)

	local := time.Date(2026, 10, 16, 17, 30, 0, 0, time.FixedZone("UTC+8", 8*3600))
	result, err := workspace.ArchiveAgentDir(root, "compat-check", local)
	require.NoError(t, err)
	require.Equal(t, "compat-check-"+archiveStamp, result.ArchiveID,
		"归档名必须按 UTC 格式化（+08:00 的 17:30 = UTC 09:30）")
}

// 中文验收点：归档清单逐字 `[{path,sha256,size}]`、按 path 升序；content_hash 是
// `path\0sha256\0` 串的 sha256（与技能清单同一规则）——同一内容在两个工作区里必须算出
// 同一个哈希（路径相对 Agent 根，不带工作区前缀）。
// Acceptance point: the manifest is path-sorted with per-file hashes and the content
// hash is recomputable from path+hash alone.
func TestDeleteAgent_ComputesManifestAndHash(t *testing.T) {
	build := func(t *testing.T) (root string, files map[string]string) {
		t.Helper()
		root = initWorkspaceRoot(t)
		files = map[string]string{
			"AGENT.md":               compatSampleAgentMD,
			"notes/b.md":             "# B\n",
			"notes/a.txt":            "甲\n",
			"memory/agents/x/MD.md":  "记忆\n",
			"skills/digest/SKILL.md": "# skill\n",
		}
		for rel, body := range files {
			writeFileInRoot(t, root, filepath.Join("agents", "compat-check", filepath.FromSlash(rel)), body)
		}
		return root, files
	}

	root, files := build(t)
	result, err := workspace.ArchiveAgentDir(root, "compat-check", archiveNow())
	require.NoError(t, err)

	var manifest []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.ManifestJSON), &manifest))
	require.Len(t, manifest, len(files))

	// 升序且相对 Agent 根（正斜杠）。
	require.Equal(t, []string{"AGENT.md", "memory/agents/x/MD.md", "notes/a.txt", "notes/b.md", "skills/digest/SKILL.md"},
		[]string{manifest[0].Path, manifest[1].Path, manifest[2].Path, manifest[3].Path, manifest[4].Path})

	// 逐文件哈希与大小对得上磁盘内容。
	for _, entry := range manifest {
		want := files[entry.Path]
		sum := sha256.Sum256([]byte(want))
		require.Equal(t, hex.EncodeToString(sum[:]), entry.SHA256, "%s 的哈希", entry.Path)
		require.Equal(t, int64(len(want)), entry.Size, "%s 的字节数", entry.Path)
	}

	// 规整哈希可由清单复算（同一规则的可验证形态）。
	var canonical strings.Builder
	for _, entry := range manifest {
		canonical.WriteString(entry.Path)
		canonical.WriteByte(0)
		canonical.WriteString(entry.SHA256)
		canonical.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	require.Equal(t, hex.EncodeToString(sum[:]), result.ContentHash)

	// 同一内容在另一个工作区里算出同一个哈希（路径相对 Agent 根）。
	otherRoot, _ := build(t)
	other, err := workspace.ArchiveAgentDir(otherRoot, "compat-check", archiveNow())
	require.NoError(t, err)
	require.Equal(t, result.ContentHash, other.ContentHash, "content_hash 不得随工作区路径漂移")
}

// 中文验收点：目录不存在 → 拒绝并点名（不是静默成功）；归档目录不得凭空出现（零写入）。
// Acceptance point: deleting a missing agent fails loudly and writes nothing.
func TestDeleteAgent_RefusesMissingAgent(t *testing.T) {
	root := initWorkspaceRoot(t)

	result, err := workspace.ArchiveAgentDir(root, "ghost-agent", archiveNow())
	require.Error(t, err)
	require.Empty(t, result)
	require.Contains(t, err.Error(), "ghost-agent")
	require.Empty(t, dirNames(t, filepath.Join(root, "archive")), "拒绝路径不得创建归档副本")
}

// 中文验收点：Agent 名非法 → 拒绝且零写入；与 create 同一校验函数（同一表逐条判同一结果）。
// Acceptance point: the delete path shares the validator with the create path.
func TestDeleteAgent_RejectsInvalidName(t *testing.T) {
	for _, name := range []string{"", "a/b", `a\b`, ".", "..", "a\x00b", ".hidden", "a.staging", "a.backup"} {
		root := initWorkspaceRoot(t)
		result, err := workspace.ArchiveAgentDir(root, name, archiveNow())
		require.Error(t, err, "非法名字必须被拒：%q", name)
		require.Empty(t, result)
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

	result, err := workspace.ArchiveAgentDir(root, "notes.txt", archiveNow())
	require.Error(t, err)
	require.Empty(t, result)
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

	result, err := workspace.ArchiveAgentDir(root, "link-agent", archiveNow())
	require.Error(t, err)
	require.Empty(t, result)
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

	result, err := workspace.ArchiveAgentDir(root, "compat-check", archiveNow())
	require.Error(t, err)
	require.Empty(t, result)
	require.Contains(t, err.Error(), "compat-check-"+archiveStamp) // 点名撞名目标

	// 源目录及其定义文件原样保留（"先删后移"的实现会在这里判红）。
	require.DirExists(t, filepath.Join(root, "agents", "compat-check"))
	require.Equal(t, compatSampleAgentMD, agentMDOf(t, root, "compat-check"))

	// 既有归档副本**未被覆盖**。
	kept, readErr := os.ReadFile(filepath.Join(root, "archive", "compat-check-"+archiveStamp, "AGENT.md"))
	require.NoError(t, readErr)
	require.Equal(t, previous, string(kept))
}

// 中文验收点（约束 49，机器可判）：生命周期状态文件 `control/agents/*.json` 只有
// **唯一写者** `lifecycle.go`——本包其余生产文件一律不得构造该路径。判卷装置是源码
// grep：包目录下所有非测试 .go 文件里出现 `control/agents` 或 path 段常量的只有
// lifecycle.go。测试文件可以出现（夹具建目录），所以只看非测试文件。
// Acceptance point: lifecycle.go is the only production file that builds the
// control/agents path; the assertion is a source grep over the package directory.
func TestLifecycle_SingleWriter(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	// 两个路径段常量与字面路径都算"构造该路径"——换个写法绕开判据同样算第二写者。
	markers := []string{"controlDirName", "lifecycleAgentsDirName", "control/agents"}
	var writers []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		require.NoError(t, err)
		source := string(raw)
		for _, marker := range markers {
			if strings.Contains(source, marker) {
				writers = append(writers, name)
				break
			}
		}
	}

	require.Equal(t, []string{"lifecycle.go"}, writers,
		"control/agents 的路径构造只允许出现在 lifecycle.go（唯一写者，约束 49）；命中文件：%v", writers)
}
