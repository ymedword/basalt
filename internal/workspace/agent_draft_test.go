package workspace_test

// 本文件是 Agent 受管文件集合与多文件条件提交的验收面（颗粒度文档 §3.2 的
// workspace/agent_draft_test.go 家族 / 约束 30 / 约束 41）：
//
//	T1  TestAgentFiles_CollectionETag——集合 ETag 的字节定义（path 升序、原字节哈希、
//	   强 ETag 形状、与单文件 ETag 不可互换）
//	T2  TestAgentFiles_ManagedFilesOnlyRegularDirect——只收录根目录普通文件
//	T3  TestAgentFiles_StaleETag412NoWrite——过期集合 ETag → 412 且零写入
//	T4  TestAgentFiles_IfMatchMalformed412——缺失 / 弱 / `*` / 形状非法 → 412
//	T5  TestAgentFiles_FullCopyAndTwoRenames——完整副本 + 两次 rename，无残留
//	T6  TestAgentFiles_UnlistedFilesKept——未列出的现有文件逐字节保留
//	T7  TestAgentFiles_CrashResidue——staging / backup 残留拒绝且正式目录不动
//	T8  TestAgentFiles_PathMatrix——约束 41 路径矩阵与 UTF-8
//	T9  TestAgentFiles_CaseCollision——大小写重复拒绝
//	T10 TestAgentFiles_MissingAgentMDRejected——候选树必须有 AGENT.md
//	T11 TestAgentFiles_NewFileSubdirectory——新文件自动建父目录、返回落盘后真值
//
// 采用**同包测试**（workspace_test）：期望 ETag 由测试自己用 crypto/sha256 拼出，
// 不调用被测实现；工作区夹具沿用 agent_test.go / agent_files_test.go 的家族。
// Expected ETags are computed from crypto/sha256 on the test side; the fixture
// family is shared with the other workspace tests.

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// draftAgentName / draftAgentMD 是提交用例的 Agent 名与最小合法定义。
// draftAgentName and draftAgentMD are the fixture agent's name and minimal definition.
const (
	draftAgentName = "draft-agent"
	draftAgentMD   = "---\nname: draft-agent\ndescription: 提交测试\nprovider:\n  name: deepseek\n  model: deepseek-chat\n---\n\n正文。\n"
)

// agentDirPath 给出 Agent 目录在工作区内的相对路径。
// agentDirPath returns the agent directory's workspace-relative path.
func agentDirPath(name string) string {
	return filepath.Join("agents", name)
}

// draftRoot 造一个已初始化工作区，并落一个带 AGENT.md 的 Agent 目录。
// draftRoot builds an initialised workspace holding one agent directory.
func draftRoot(t *testing.T) string {
	t.Helper()
	root := initWorkspaceRoot(t)
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "AGENT.md"), draftAgentMD)
	return root
}

// expectedFileSetETag 按约束 30 的字节定义独立重算集合 ETag：path 升序的
// `path\0sha256(内容)\0` 串接后求 sha256，渲染为带引号的强 ETag。
// expectedFileSetETag recomputes the set ETag from the fixed byte definition.
func expectedFileSetETag(files []core.AuthorFile) string {
	sorted := append([]core.AuthorFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var canonical strings.Builder
	for _, file := range sorted {
		sum := sha256.Sum256([]byte(file.Content))
		canonical.WriteString(file.Path)
		canonical.WriteByte(0)
		canonical.WriteString(hex.EncodeToString(sum[:]))
		canonical.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return `"sha256:` + hex.EncodeToString(sum[:]) + `"`
}

// snapshotAgentTree 读取 Agent 目录的全部普通文件（相对路径 → 字节），用于零写入判据。
// snapshotAgentTree snapshots every regular file of the agent directory.
func snapshotAgentTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string, 8)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	require.NoError(t, err)
	return out
}

// requireNoSwapResidue 断言两次 rename 的 staging / backup 残留都不存在。
// requireNoSwapResidue asserts both swap residue directories are absent.
func requireNoSwapResidue(t *testing.T, root, name string) {
	t.Helper()
	require.NoDirExists(t, filepath.Join(root, "agents", name+".staging"), "staging 不得残留")
	require.NoDirExists(t, filepath.Join(root, "agents", name+".backup"), "backup 不得残留")
}

// currentSetETag 读取磁盘上的集合 ETag（提交前置条件）。
// currentSetETag reads the current set ETag from disk.
func currentSetETag(t *testing.T, root string) string {
	t.Helper()
	_, etag, err := workspace.ManagedAgentFiles(root, draftAgentName)
	require.NoError(t, err)
	return etag
}

// TestAgentFiles_CollectionETag —— 约束 30 的字节定义：集合 ETag 由 path 升序的
// `(path, sha256(内容))` 规整序列化决定，与入参顺序无关；形态是带引号的强 ETag；
// 内容或路径变化都会改变它；它与 AGENT.md 的单文件 ETag **不可互换**。
func TestAgentFiles_CollectionETag(t *testing.T) {
	files := []core.AuthorFile{
		{Path: "AGENT.md", Content: draftAgentMD},
		{Path: "notes.md", Content: "笔记一\n"},
		{Path: "docs/guide.md", Content: "指南\n"},
	}
	want := expectedFileSetETag(files)
	require.Equal(t, want, workspace.FileSetETag(files))

	reversed := []core.AuthorFile{files[2], files[0], files[1]}
	require.Equal(t, want, workspace.FileSetETag(reversed), "集合 ETag 必须与入参顺序无关")

	require.Regexp(t, `^"sha256:[0-9a-f]{64}"$`, want, "形态必须是带引号的强 ETag")
	require.NotEqual(t, want, workspace.FileSetETag([]core.AuthorFile{
		{Path: "AGENT.md", Content: draftAgentMD},
		{Path: "notes.md", Content: "笔记二\n"},
		{Path: "docs/guide.md", Content: "指南\n"},
	}), "内容任何变化都必须改变集合 ETag")
	require.NotEqual(t, want, workspace.FileSetETag([]core.AuthorFile{
		{Path: "AGENT.md", Content: draftAgentMD},
		{Path: "notes.md", Content: "笔记一\n"},
		{Path: "docs/GUIDE.md", Content: "指南\n"},
	}), "路径任何变化都必须改变集合 ETag")
	require.NotEqual(t, workspace.ContentETag([]byte(draftAgentMD)), want,
		"单文件 ETag 与集合 ETag 不可互换")

	// 读面返回的 ETag 与测试独立重算一致（只收录根目录普通文件后按同一字节定义求值）。
	root := draftRoot(t)
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "notes.md"), "笔记一\n")
	got, etag, err := workspace.ManagedAgentFiles(root, draftAgentName)
	require.NoError(t, err)
	require.Equal(t, []core.AuthorFile{
		{Path: "AGENT.md", Content: draftAgentMD},
		{Path: "notes.md", Content: "笔记一\n"},
	}, got)
	require.Equal(t, expectedFileSetETag(got), etag)
}

// TestAgentFiles_ManagedFilesOnlyRegularDirect —— 读面只收录 Agent **根目录**下的
// 普通文件：子目录内容、`.staging` / `.backup` 残留名与软连接都不是受管文件。
func TestAgentFiles_ManagedFilesOnlyRegularDirect(t *testing.T) {
	root := draftRoot(t)
	dir := filepath.Join(root, agentDirPath(draftAgentName))
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "notes.md"), "笔记\n")
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "docs", "guide.md"), "子目录内容\n")
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "residue.staging"), "残留\n")
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "residue.backup"), "残留\n")

	linkCreated := true
	if err := os.Symlink(filepath.Join(dir, "notes.md"), filepath.Join(dir, "link.md")); err != nil {
		linkCreated = false
		t.Logf("本平台不允许创建符号链接，跳过软连接断言：%v", err)
	}

	files, _, err := workspace.ManagedAgentFiles(root, draftAgentName)
	require.NoError(t, err)
	require.Equal(t, []core.AuthorFile{
		{Path: "AGENT.md", Content: draftAgentMD},
		{Path: "notes.md", Content: "笔记\n"},
	}, files, "只收录根目录普通文件，按 path 升序；子目录 / 残留名 / 软连接都不收录")
	if linkCreated {
		for _, file := range files {
			require.NotEqual(t, "link.md", file.Path, "软连接不是受管文件（不跟随）")
		}
	}
}

// TestAgentFiles_StaleETag412NoWrite —— 约束 30 的核心回归：页面拿到的集合 ETag
// 过期后再次提交必须 412，磁盘逐字节不变、不留 staging / backup、不产生操作行
// （操作行归 app 层；本层判据是零磁盘写入）。
func TestAgentFiles_StaleETag412NoWrite(t *testing.T) {
	root := draftRoot(t)
	dir := filepath.Join(root, agentDirPath(draftAgentName))
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "notes.md"), "旧笔记\n")
	stale := currentSetETag(t, root)

	// 另一个写者先保存：内容变了，旧 ETag 随之过期。
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "notes.md"), "别人的新笔记\n")
	before := snapshotAgentTree(t, dir)

	infos, etag, err := workspace.SubmitAgentFiles(root, draftAgentName, stale, []core.AuthorFile{
		{Path: "AGENT.md", Content: draftAgentMD + "\n我的改动\n"},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrPreconditionFailed)
	require.Empty(t, infos)
	require.Empty(t, etag)
	require.Contains(t, err.Error(), "ETag", "错误要点名前置条件")
	require.Equal(t, before, snapshotAgentTree(t, dir), "412 必须零写入")
	requireNoSwapResidue(t, root, draftAgentName)
}

// TestAgentFiles_IfMatchMalformed412 —— 缺失、弱 ETag、`*`、裸哈希、逗号列表与
// 大小写变体都不是合法的强 ETag：一律 412，且磁盘零写入。
func TestAgentFiles_IfMatchMalformed412(t *testing.T) {
	root := draftRoot(t)
	dir := filepath.Join(root, agentDirPath(draftAgentName))
	valid := currentSetETag(t, root)
	before := snapshotAgentTree(t, dir)

	malformed := []struct {
		name    string
		ifMatch string
	}{
		{"missing", ""},
		{"weak", `W/` + valid},
		{"star", "*"},
		{"bare-hash", strings.Trim(valid, `"`)},
		{"uppercase-hex", strings.ToUpper(valid)},
		{"comma-list", valid + `, ` + valid},
		{"wrong-length", `"sha256:abc"`},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := workspace.SubmitAgentFiles(root, draftAgentName, tc.ifMatch, []core.AuthorFile{
				{Path: "AGENT.md", Content: draftAgentMD + "\n改动\n"},
			})
			require.ErrorIs(t, err, core.ErrPreconditionFailed, "If-Match=%q", tc.ifMatch)
			require.Equal(t, before, snapshotAgentTree(t, dir), "412 必须零写入（If-Match=%q）", tc.ifMatch)
			requireNoSwapResidue(t, root, draftAgentName)
		})
	}
}

// TestAgentFiles_FullCopyAndTwoRenames —— 提交按"完整副本 + 两次 rename"落地：
// 嵌套目录与未提及的文件都进入新树，提交的文件是新内容，交换后不留 staging / backup，
// 返回值是落盘后重新读取的真值（含新集合 ETag）。
func TestAgentFiles_FullCopyAndTwoRenames(t *testing.T) {
	root := draftRoot(t)
	dir := filepath.Join(root, agentDirPath(draftAgentName))
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "notes.md"), "旧笔记\n")
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "docs", "guide.md"), "指南\n")
	etag := currentSetETag(t, root)

	infos, newETag, err := workspace.SubmitAgentFiles(root, draftAgentName, etag, []core.AuthorFile{
		{Path: "AGENT.md", Content: draftAgentMD + "\n第二版\n"},
		{Path: "notes.md", Content: "新笔记\n"},
	})
	require.NoError(t, err)

	disk := snapshotAgentTree(t, dir)
	require.Contains(t, disk["AGENT.md"], "第二版")
	require.Equal(t, "新笔记\n", disk["notes.md"])
	require.Equal(t, "指南\n", disk["docs/guide.md"], "完整副本必须保留嵌套目录与未提及的文件")
	requireNoSwapResidue(t, root, draftAgentName)

	require.Equal(t, []core.AgentFileInfo{
		{Path: "AGENT.md", Size: int64(len(draftAgentMD + "\n第二版\n"))},
		{Path: "notes.md", Size: int64(len("新笔记\n"))},
	}, infos, "返回值是落盘后重新读取的**受管文件**清单（根目录普通文件，按 path 升序）")

	_, readETag, err := workspace.ManagedAgentFiles(root, draftAgentName)
	require.NoError(t, err)
	require.Equal(t, readETag, newETag, "返回的新 ETag 必须等于重新读取的集合 ETag")
}

// TestAgentFiles_UnlistedFilesKept —— 提交只列 AGENT.md 时，现有 `notes.md` 原样保留
// （完整副本语义；"未列出 = 删除"是技能全量替换的规则，不适用于 Agent 多文件提交）。
func TestAgentFiles_UnlistedFilesKept(t *testing.T) {
	root := draftRoot(t)
	dir := filepath.Join(root, agentDirPath(draftAgentName))
	const notes = "不应被动过的笔记\n"
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "notes.md"), notes)
	etag := currentSetETag(t, root)

	infos, _, err := workspace.SubmitAgentFiles(root, draftAgentName, etag, []core.AuthorFile{
		{Path: "AGENT.md", Content: draftAgentMD + "\n只改定义\n"},
	})
	require.NoError(t, err)
	raw, readErr := os.ReadFile(filepath.Join(dir, "notes.md"))
	require.NoError(t, readErr)
	require.Equal(t, notes, string(raw), "未列出的现有文件必须逐字节保留")
	require.Contains(t, infos, core.AgentFileInfo{Path: "notes.md", Size: int64(len(notes))},
		"保留的文件出现在返回清单里")
}

// TestAgentFiles_CrashResidue —— 两次 rename 的残留目录由启动恢复裁决：提交路径发现
// `.staging` / `.backup` 残留时拒绝覆盖并点名，正式目录逐字节不变。
func TestAgentFiles_CrashResidue(t *testing.T) {
	cases := []struct {
		name    string
		residue string
	}{
		{"staging", draftAgentName + ".staging"},
		{"backup", draftAgentName + ".backup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := draftRoot(t)
			dir := filepath.Join(root, agentDirPath(draftAgentName))
			writeFileInRoot(t, root, filepath.Join("agents", tc.residue, "AGENT.md"), "残留副本\n")
			etag := currentSetETag(t, root)
			before := snapshotAgentTree(t, dir)

			_, _, err := workspace.SubmitAgentFiles(root, draftAgentName, etag, []core.AuthorFile{
				{Path: "AGENT.md", Content: draftAgentMD + "\n改动\n"},
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), "残留", "错误必须点名残留目录")
			require.Equal(t, before, snapshotAgentTree(t, dir), "拒绝时正式目录逐字节不变")
			require.FileExists(t, filepath.Join(root, "agents", tc.residue, "AGENT.md"),
				"残留目录不得被本函数删除（裁决属于恢复路径）")
		})
	}
}

// TestAgentFiles_PathMatrix —— 约束 41：绝对路径（含盘符）、越界段、空段、控制字符、
// 保留后缀与非法 UTF-8 正文一律拒绝并点名；拒绝发生在任何磁盘写入之前。
func TestAgentFiles_PathMatrix(t *testing.T) {
	badPaths := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"absolute-slash", "/etc/passwd"},
		{"absolute-drive", `C:\Windows\system.ini`},
		{"parent", "../outside.md"},
		{"embedded-parent", "docs/../AGENT.md"},
		{"empty-segment", "docs//guide.md"},
		{"control-char", "bad\x01name.md"},
		{"nul", "bad\x00name.md"},
		{"staging-suffix", "notes.staging"},
		{"backup-suffix", "notes.backup"},
	}
	root := draftRoot(t)
	dir := filepath.Join(root, agentDirPath(draftAgentName))
	before := snapshotAgentTree(t, dir)

	for _, tc := range badPaths {
		t.Run(tc.name, func(t *testing.T) {
			etag := currentSetETag(t, root)
			_, _, err := workspace.SubmitAgentFiles(root, draftAgentName, etag, []core.AuthorFile{
				{Path: tc.path, Content: "内容\n"},
			})
			require.Error(t, err, "路径 %q 必须拒绝", tc.path)
			require.NotErrorIs(t, err, core.ErrPreconditionFailed, "这是路径拒绝，不是条件写冲突")
			require.Equal(t, before, snapshotAgentTree(t, dir), "拒绝时磁盘零写入（路径 %q）", tc.path)
			requireNoSwapResidue(t, root, draftAgentName)
		})
	}

	t.Run("invalid-utf8-content", func(t *testing.T) {
		etag := currentSetETag(t, root)
		_, _, err := workspace.SubmitAgentFiles(root, draftAgentName, etag, []core.AuthorFile{
			{Path: "notes.md", Content: string([]byte{0xff, 0xfe, 0xfd})},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "UTF-8")
		require.Equal(t, before, snapshotAgentTree(t, dir), "拒绝时磁盘零写入")
	})
}

// TestAgentFiles_CaseCollision —— Windows 文件系统大小写不敏感：提交内部仅大小写不同、
// 或与现有文件仅大小写不同 → 拒绝（否则两个文件互相覆盖）。
func TestAgentFiles_CaseCollision(t *testing.T) {
	root := draftRoot(t)
	dir := filepath.Join(root, agentDirPath(draftAgentName))
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "Notes.md"), "现有\n")
	before := snapshotAgentTree(t, dir)

	t.Run("within-drafts", func(t *testing.T) {
		etag := currentSetETag(t, root)
		_, _, err := workspace.SubmitAgentFiles(root, draftAgentName, etag, []core.AuthorFile{
			{Path: "notes.md", Content: "一\n"},
			{Path: "Notes.md", Content: "二\n"},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "大小写")
		require.Equal(t, before, snapshotAgentTree(t, dir), "拒绝时磁盘零写入")
	})

	t.Run("against-existing", func(t *testing.T) {
		etag := currentSetETag(t, root)
		_, _, err := workspace.SubmitAgentFiles(root, draftAgentName, etag, []core.AuthorFile{
			{Path: "notes.md", Content: "覆盖尝试\n"},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "大小写")
		require.Equal(t, before, snapshotAgentTree(t, dir), "拒绝时磁盘零写入")
	})
}

// TestAgentFiles_MissingAgentMDRejected —— 候选树必须仍有 AGENT.md：提交不能把
// 定义文件"改丢"（未列出时它由完整副本保留；磁盘本就没有则拒绝）。
func TestAgentFiles_MissingAgentMDRejected(t *testing.T) {
	root := initWorkspaceRoot(t)
	dir := filepath.Join(root, agentDirPath(draftAgentName))
	writeFileInRoot(t, root, filepath.Join(agentDirPath(draftAgentName), "notes.md"), "笔记\n")
	etag := currentSetETag(t, root)
	before := snapshotAgentTree(t, dir)

	_, _, err := workspace.SubmitAgentFiles(root, draftAgentName, etag, []core.AuthorFile{
		{Path: "notes.md", Content: "新笔记\n"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "AGENT.md")
	require.Equal(t, before, snapshotAgentTree(t, dir), "拒绝时磁盘零写入")
	requireNoSwapResidue(t, root, draftAgentName)
}

// TestAgentFiles_NewFileSubdirectory —— 新文件路径中的 `\` 归一为 `/`，父目录在候选树
// 中创建；成功后返回的清单与 ETag 都来自落盘后的重新读取。
func TestAgentFiles_NewFileSubdirectory(t *testing.T) {
	root := draftRoot(t)
	dir := filepath.Join(root, agentDirPath(draftAgentName))
	etag := currentSetETag(t, root)
	const guide = "新的指南\n"

	infos, newETag, err := workspace.SubmitAgentFiles(root, draftAgentName, etag, []core.AuthorFile{
		{Path: `docs\guide.md`, Content: guide},
	})
	require.NoError(t, err)

	raw, readErr := os.ReadFile(filepath.Join(dir, "docs", "guide.md"))
	require.NoError(t, readErr, "`\\` 必须归一为 `/` 并创建父目录")
	require.Equal(t, guide, string(raw))
	require.Equal(t, []core.AgentFileInfo{{Path: "AGENT.md", Size: int64(len(draftAgentMD))}},
		infos, "子目录文件不是受管文件（视图只列根目录普通文件），但已落盘")

	_, readETag, err := workspace.ManagedAgentFiles(root, draftAgentName)
	require.NoError(t, err)
	require.Equal(t, readETag, newETag)
	require.Equal(t, etag, newETag, "只改动子目录文件时受管文件集合不变，集合 ETag 随之不变")
	require.Regexp(t, `^"sha256:[0-9a-f]{64}"$`, newETag)
	requireNoSwapResidue(t, root, draftAgentName)
}
