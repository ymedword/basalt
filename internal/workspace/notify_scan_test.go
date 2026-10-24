package workspace_test

// 本文件是通知渠道删除闸门扫描面的回归（颗粒度文档 §3.1.4 / 约束 38，测试映射
// §3.2.1 的 workspace 行：整词、大小写、归档目录、解析失败四类，外加读取面的
// fail-closed 边界）。
// Regression tests for the document scan behind the notify-channel delete gate.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/workspace"
)

// 中文验收点：整词匹配的边界是 Unicode 字母 / 数字 / `_` 之外的字符——空白、连字符、
// 全角括号都算边界；被字母、数字或下划线黏住的出现不算命中；空词一律不命中。
// Acceptance point: whole-word boundaries are anything but letters, digits and
// underscore; the empty word never matches.
func TestNotifyScan_WordBoundary(t *testing.T) {
	cases := []struct {
		name string
		text string
		word string
		want bool
	}{
		{name: "空白边界", text: "请把结果发到 ops 渠道", word: "ops", want: true},
		{name: "整份文本就是渠道名", text: "ops", word: "ops", want: true},
		{name: "连字符边界", text: "ops-prod 与 prod-ops", word: "ops", want: true},
		{name: "全角括号边界", text: "见（ops）一节", word: "ops", want: true},
		{name: "反引号边界", text: "`ops` 是渠道名", word: "ops", want: true},
		{name: "字母前缀不算命中", text: "myops 不是渠道", word: "ops", want: false},
		{name: "字母后缀不算命中", text: "opschannel 不是渠道", word: "ops", want: false},
		{name: "数字后缀不算命中", text: "ops2 不是渠道", word: "ops", want: false},
		{name: "下划线算词内字符", text: "ops_team 不是渠道", word: "ops", want: false},
		{name: "汉字算词内字符", text: "团队ops组", word: "ops", want: false},
		{name: "空词不匹配任何文本", text: "ops", word: "", want: false},
		{name: "文本短于词", text: "op", word: "ops", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, workspace.WholeWordMatch(tc.text, tc.word))
		})
	}
}

// 中文验收点：大小写敏感——渠道名是冻结字面量，`OPS` 不得命中 `ops`。
// Acceptance point: matching is case-sensitive.
func TestNotifyScan_CaseSensitive(t *testing.T) {
	require.False(t, workspace.WholeWordMatch("发到 OPS 渠道", "ops"))
	require.False(t, workspace.WholeWordMatch("发到 Ops 渠道", "ops"))
	require.True(t, workspace.WholeWordMatch("发到 ops 渠道", "ops"))
	require.False(t, workspace.WholeWordMatch("发到 ops 渠道", "OPS"))
}

// 中文验收点：活动区文档按 `agents/<name>/AGENT.md` 收集，Name 取目录名、Archived
// 为假、ArchiveID 为空；Text 是**全文**（含 frontmatter），按名字节序返回。
// Acceptance point: active documents are collected by directory name with full text.
func TestNotifyScan_ActiveDocuments(t *testing.T) {
	root := initWorkspaceRoot(t)
	writeAgentFrontmatter(t, root, "beta-agent", `
name: beta-agent
provider: {name: deepseek, model: deepseek-chat}
`, "值班告警发到 ops 渠道")
	writeAgentFrontmatter(t, root, "alpha-agent", `
name: alpha-agent
provider: {name: deepseek, model: deepseek-chat}
`, "不引用任何渠道")

	docs, err := workspace.ScanAgentDocuments(root)
	require.NoError(t, err)
	require.Len(t, docs, 2)
	require.Equal(t, "alpha-agent", docs[0].Name, "活动文档按目录名字节序返回")
	require.False(t, docs[0].Archived)
	require.Empty(t, docs[0].ArchiveID)
	require.Contains(t, docs[0].Text, "不引用任何渠道")
	require.True(t, strings.HasPrefix(docs[0].Text, "---\n"), "Text 必须是含 frontmatter 的全文")
	require.Equal(t, "beta-agent", docs[1].Name)
	require.True(t, workspace.WholeWordMatch(docs[1].Text, "ops"))
}

// 中文验收点：归档区按 `archive/<archive_id>/AGENT.md` 收集，Archived 为真、
// ArchiveID 取归档标识、Name 取 frontmatter 的 name；正文同样可整词命中。
// Acceptance point: archived documents carry the archive id and frontmatter name.
func TestNotifyScan_ArchiveDocuments(t *testing.T) {
	root := initWorkspaceRoot(t)
	const archiveID = "ops-agent-20261022-080000"
	writeFileInRoot(t, root, filepath.Join("archive", archiveID, "AGENT.md"),
		"---\nname: ops-agent\nprovider: {name: deepseek, model: deepseek-chat}\n---\n\n把告警发到 ops 渠道\n")

	docs, err := workspace.ScanAgentDocuments(root)
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.True(t, docs[0].Archived)
	require.Equal(t, archiveID, docs[0].ArchiveID)
	require.Equal(t, "ops-agent", docs[0].Name)
	require.True(t, workspace.WholeWordMatch(docs[0].Text, "ops"))
}

// 中文验收点（fail-closed）：归档定义解析失败**不得丢文档**——围栏缺失或 frontmatter
// 不是合法 YAML 时，文档照样返回（正文就是判定面），名字回退归档标识。
// Acceptance point: an unparseable archive is still returned; the text scan must
// not miss a reference.
func TestNotifyScan_ParseFailureKeepsDocument(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "缺围栏", raw: "没有围栏的归档正文，仍提到 ops 渠道\n"},
		{name: "frontmatter 非法 YAML", raw: "---\nname: [unclosed\n---\n\n正文仍提到 ops\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := initWorkspaceRoot(t)
			const archiveID = "broken-agent-20261022-090000"
			writeFileInRoot(t, root, filepath.Join("archive", archiveID, "AGENT.md"), tc.raw)

			docs, err := workspace.ScanAgentDocuments(root)
			require.NoError(t, err, "解析失败不是扫描错误")
			require.Len(t, docs, 1)
			require.True(t, docs[0].Archived)
			require.Equal(t, archiveID, docs[0].ArchiveID)
			require.Equal(t, archiveID, docs[0].Name, "取不到 name 时回退归档标识")
			require.Equal(t, tc.raw, docs[0].Text, "正文必须原样返回")
			require.True(t, workspace.WholeWordMatch(docs[0].Text, "ops"))
		})
	}
}

// 中文验收点：缺 AGENT.md 的目录、非目录条目都跳过；`.staging` 残留里有 AGENT.md
// 时**照样扫描**（残留树可能仍引用渠道，跳过等于假阴性；残骸清理由恢复路径负责）。
// Acceptance point: directories without a definition and non-directory entries are
// skipped, while staging residue is still scanned (fail-closed).
func TestNotifyScan_SkipsNonDocuments(t *testing.T) {
	root := initWorkspaceRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents", "no-definition"), 0o750))
	writeFileInRoot(t, root, filepath.Join("agents", "stray.txt"), "不是目录\n")
	writeFileInRoot(t, root, filepath.Join("archive", "stray.txt"), "不是目录\n")
	writeFileInRoot(t, root, filepath.Join("agents", "half.staging", "AGENT.md"),
		"---\nname: half\n---\n\nops\n")

	docs, err := workspace.ScanAgentDocuments(root)
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, "half.staging", docs[0].Name)
	require.False(t, docs[0].Archived)
}

// 中文验收点（不跟随软链接）：软链接目录不是 Agent 单元，不得被扫描；真实 Agent
// 目录里的 AGENT.md 若是软链接，扫描整体报错——读不到定义就证明不了"没人引用"，
// 必须 fail-closed 挡住删除，而不是跳过。平台不允许建链接时跳过（理由写明）。
// Acceptance point: linked directories are skipped and a linked definition fails the
// whole scan (fail-closed); the test is skipped where symlinks cannot be created.
func TestNotifyScan_DoesNotFollowSymlinks(t *testing.T) {
	root := initWorkspaceRoot(t)
	target := t.TempDir()
	writeFileInRoot(t, target, filepath.Join("agent-dir", "AGENT.md"),
		"---\nname: outer\n---\n\nops\n")
	if err := os.Symlink(filepath.Join(target, "agent-dir"), filepath.Join(root, "agents", "linked")); err != nil {
		t.Skip("本平台不允许创建符号链接（Windows 需开发者模式或特权）：" + err.Error())
	}

	docs, err := workspace.ScanAgentDocuments(root)
	require.NoError(t, err)
	require.Empty(t, docs, "软链接目录不是 Agent 单元，不得被扫描")

	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents", "real"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(target, "agent-dir", "AGENT.md"),
		filepath.Join(root, "agents", "real", "AGENT.md")))
	_, err = workspace.ScanAgentDocuments(root)
	require.ErrorContains(t, err, "普通文件")
}

// 中文验收点（fail-closed 的平台无关形态）：`AGENT.md` 是目录（非普通文件）→ 整体
// 报错。与软链接用例走同一个 Lstat 类型判定——本机建不出符号链接时，这条仍能判卷。
// Acceptance point: a non-regular definition (a directory named AGENT.md) fails the
// scan on every platform, covering the same type check the symlink test exercises.
func TestNotifyScan_RejectsNonRegularDefinition(t *testing.T) {
	root := initWorkspaceRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents", "weird", "AGENT.md"), 0o750))

	_, err := workspace.ScanAgentDocuments(root)
	require.ErrorContains(t, err, "普通文件")
}

// 中文验收点：单份定义超过 1 MiB 上限 → 整体报错（不截断——截断可能切掉后半段的
// 整词命中，变成假阴性）。
// Acceptance point: an oversized definition fails the whole scan instead of being
// truncated.
func TestNotifyScan_RejectsOversizeDocument(t *testing.T) {
	root := initWorkspaceRoot(t)
	writeFileInRoot(t, root, filepath.Join("agents", "big", "AGENT.md"),
		strings.Repeat("a", (1<<20)+1))

	_, err := workspace.ScanAgentDocuments(root)
	require.ErrorContains(t, err, "字节上限")
}

// 中文验收点：`agents/` 缺失是工作区未初始化 → 报错（不得当成"零只 Agent"放行删除）；
// `archive/` 缺失是正常状态（从未归档）→ 空集无错；根不存在 → 报错。
// Acceptance point: a missing agents/ directory is an error, a missing archive/ is
// an empty scan, and a missing root is an error.
func TestNotifyScan_RequiresInitializedWorkspace(t *testing.T) {
	t.Run("缺 agents 目录", func(t *testing.T) {
		root := t.TempDir()
		_, err := workspace.ScanAgentDocuments(root)
		require.ErrorContains(t, err, "agents")
	})
	t.Run("缺 archive 目录按空集", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "agents"), 0o750))
		docs, err := workspace.ScanAgentDocuments(root)
		require.NoError(t, err)
		require.Empty(t, docs)
	})
	t.Run("根不存在", func(t *testing.T) {
		_, err := workspace.ScanAgentDocuments(filepath.Join(t.TempDir(), "no-such-root"))
		require.Error(t, err)
	})
}
