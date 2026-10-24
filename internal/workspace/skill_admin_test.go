package workspace_test

// 本文件对应颗粒度文档 §3.2.1（16 例计划，按实测补足）与约束 24 / 25 / 26：
// 技能导入边界、工作副本全量替换、不可变版本快照、删除归档、校验零写入。
// 文档写出的关键回归测试名逐字保留：TestSkillImport_TextOnly、TestSkillUpdate_FullReplacement、
// TestSkillVersion_Immutable、TestSkillUpdate_CrashResidue、TestSkillValidate_SameGateAsImport。
//
// This file covers the skill-admin file surface: import boundary, full-replacement
// work copies, immutable snapshots, delete archives and zero-write validation.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// workspaceTestUID 是一个固定的合法资源身份（rs_ + 26 位 Crockford 载荷）：新发布的
// 技能版本目录按身份分段，不再与同名重建的上一代共用扁平目录。
const workspaceTestUID = core.ResourceUID("rs_01J9ZQ7K3W8V2N5R6T4YHBCDEF")

// publishedRel 给出固定身份下发布目录相对工作区根的形态。
func publishedRel(name string, versionNo int) string {
	return fmt.Sprintf("versions/skills/%s/%s/%d", name, workspaceTestUID, versionNo)
}

// skillMD 造一份带 name / description 的合法 SKILL.md。
// skillMD builds a valid SKILL.md with frontmatter.
func skillMD(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
}

// authorFiles 把成对的 path/content 字面量整理成 []core.AuthorFile——显式顺序用于
// 验证排序与首错定位。
// authorFiles pairs path/content literals into []core.AuthorFile in input order.
func authorFiles(t *testing.T, pairs ...string) []core.AuthorFile {
	t.Helper()
	require.Zero(t, len(pairs)%2, "authorFiles 需要成对的 path/content")
	files := make([]core.AuthorFile, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		files = append(files, core.AuthorFile{Path: pairs[i], Content: pairs[i+1]})
	}
	return files
}

// candidatePaths 取候选文件清单的路径序列（升序）。
func candidatePaths(cand workspace.SkillCandidate) []string {
	paths := make([]string, len(cand.Files))
	for i, file := range cand.Files {
		paths[i] = file.Path
	}
	return paths
}

// requireImportRejected 断言错误归入 IMPORT_REJECTED 且文案含全部片段。
// requireImportRejected asserts the ErrImportRejected classification and message.
func requireImportRejected(t *testing.T, err error, fragments ...string) {
	t.Helper()
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrImportRejected, "导入边界失败必须归入 IMPORT_REJECTED：%v", err)
	for _, fragment := range fragments {
		require.Contains(t, err.Error(), fragment)
	}
}

// workFile 读回技能工作副本里的一个文件。
func workFile(t *testing.T, root, name, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "skills", name, filepath.FromSlash(rel)))
	require.NoError(t, err)
	return string(raw)
}

// mustWriteWorkCopy 校验并落工作副本，失败即终止。
func mustWriteWorkCopy(t *testing.T, root, name string, files []core.AuthorFile) workspace.SkillCandidate {
	t.Helper()
	cand, err := workspace.ValidateSkillDraft(name, files)
	require.NoError(t, err)
	require.NoError(t, workspace.WriteSkillWorkCopy(root, cand))
	return cand
}

// unlockTreeOnCleanup 在清理阶段解除只读（Windows 下 RemoveAll 删不掉只读文件）。
// unlockTreeOnCleanup clears read-only bits so t.TempDir cleanup can remove the tree.
func unlockTreeOnCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(p, 0o755)
			} else {
				_ = os.Chmod(p, 0o644)
			}
			return nil
		})
	})
}

// treeDigest 把落盘状态压成「相对路径 + 内容哈希」的有序清单——"校验零写入"的判据。
// treeDigest projects the on-disk tree into an ordered path=hash list.
func treeDigest(t *testing.T, root string) []string {
	t.Helper()
	out := make([]string, 0, 16)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out = append(out, rel+"/")
			return nil
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(raw)
		out = append(out, rel+"="+hex.EncodeToString(sum[:]))
		return nil
	})
	require.NoError(t, err)
	return out
}

// TestSkillImport_TextOnly：只接受合法 UTF-8 且不含 NUL 的文本；二进制拒绝并点名文件。
// 文档原文用例名：文本闸门（约束 24）。
func TestSkillImport_TextOnly(t *testing.T) {
	cand, err := workspace.ValidateSkillDraft("demo", authorFiles(t,
		"SKILL.md", skillMD("demo", "文本技能", "# 用法"),
		"notes/readme.md", "这是 UTF-8 正文。\n",
	))
	require.NoError(t, err)
	require.Equal(t, []string{"SKILL.md", "notes/readme.md"}, candidatePaths(cand))

	_, err = workspace.ValidateSkillDraft("demo", authorFiles(t,
		"SKILL.md", skillMD("demo", "二进制", "body"),
		"assets/logo.md", string([]byte{0xff, 0xfe, 0xfd}),
	))
	requireImportRejected(t, err, "assets/logo.md", "UTF-8")

	_, err = workspace.ValidateSkillDraft("demo", authorFiles(t,
		"SKILL.md", skillMD("demo", "NUL", "body"),
		"assets/nul.md", "abc\x00def",
	))
	requireImportRejected(t, err, "assets/nul.md", "NUL")
}

// TestSkillImport_BoundaryMatrix：越界路径逐类拒绝（约束 24）；反斜杠归一后放行。
// 文档原文用例名：导入边界矩阵。
func TestSkillImport_BoundaryMatrix(t *testing.T) {
	cases := []struct {
		name     string
		badPath  string
		fragment string
	}{
		{"absolute", "/etc/passwd", "绝对路径"},
		{"drive", `C:\Windows\evil.md`, "绝对路径"},
		{"unc", `\\server\share\evil.md`, "绝对路径"},
		{"parent", "docs/../evil.md", ".. 段"},
		{"dot", "docs/./evil.md", ".. 段"},
		{"empty-segment", "docs//evil.md", "空段"},
		{"control", "docs/e\x01vil.md", "控制字符"},
		{"hidden-dir", ".git/config", "以点开头"},
		{"hidden-file", ".env", "以点开头"},
		{"nested-hidden", "docs/.keep/x.md", "以点开头"},
		{"staging-suffix", "docs/evil.md.staging", "结尾"},
		{"backup-suffix", "docs/evil.md.backup", "结尾"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := workspace.ValidateSkillDraft("demo", authorFiles(t,
				"SKILL.md", skillMD("demo", "边界", "body"),
				tc.badPath, "x",
			))
			requireImportRejected(t, err, tc.fragment)
		})
	}

	// 反斜杠与正斜杠等价：归一后存正斜杠路径。
	cand, err := workspace.ValidateSkillDraft("demo", authorFiles(t,
		"SKILL.md", skillMD("demo", "反斜杠", "body"),
		`docs\guide.md`, "正文",
	))
	require.NoError(t, err)
	require.Equal(t, []string{"SKILL.md", "docs/guide.md"}, candidatePaths(cand))
}

// TestSkillImport_CaseFoldDuplicate：大小写折叠后重名拒绝——Windows / macOS 上两个
// 路径会落到同一文件，留着就是"发布清单与实际落盘不一致"的伏笔。
func TestSkillImport_CaseFoldDuplicate(t *testing.T) {
	_, err := workspace.ValidateSkillDraft("demo", authorFiles(t,
		"SKILL.md", skillMD("demo", "重复", "body"),
		"Docs/A.md", "x",
		"docs/a.md", "y",
	))
	requireImportRejected(t, err, "大小写折叠后重复")
}

// TestSkillImport_SizeLimits：单文件 ≤ 256 KiB、总量 ≤ 2 MiB、文件数 ≤ 200；上限是
// "超过才拒绝"（恰好等于放行）。
func TestSkillImport_SizeLimits(t *testing.T) {
	big := strings.Repeat("a", 256*1024)

	_, err := workspace.ValidateSkillDraft("demo", authorFiles(t,
		"SKILL.md", skillMD("demo", "单文件上限", "body"),
		"docs/big.md", big,
	))
	require.NoError(t, err, "恰好 256 KiB 必须放行")

	_, err = workspace.ValidateSkillDraft("demo", authorFiles(t,
		"SKILL.md", skillMD("demo", "单文件上限", "body"),
		"docs/big.md", big+"a",
	))
	requireImportRejected(t, err, "docs/big.md", "单文件上限")

	totalPairs := []string{"SKILL.md", skillMD("demo", "总量上限", "body")}
	for i := 0; i < 9; i++ {
		totalPairs = append(totalPairs, fmt.Sprintf("docs/%02d.md", i), big)
	}
	_, err = workspace.ValidateSkillDraft("demo", authorFiles(t, totalPairs...))
	requireImportRejected(t, err, "文件总量超过上限")

	files := []core.AuthorFile{{Path: "SKILL.md", Content: skillMD("demo", "数量上限", "body")}}
	for i := 0; i < 199; i++ {
		files = append(files, core.AuthorFile{Path: fmt.Sprintf("docs/%03d.md", i), Content: "x"})
	}
	_, err = workspace.ValidateSkillDraft("demo", files)
	require.NoError(t, err, "200 个文件必须放行")

	files = append(files, core.AuthorFile{Path: "docs/extra.md", Content: "x"})
	_, err = workspace.ValidateSkillDraft("demo", files)
	requireImportRejected(t, err, "文件数量", "200")
}

// TestSkillImport_MissingSKILLMD：根级 SKILL.md 是硬前提；子目录里的同名文件不算。
func TestSkillImport_MissingSKILLMD(t *testing.T) {
	_, err := workspace.ValidateSkillDraft("demo", authorFiles(t, "docs/a.md", "a"))
	requireImportRejected(t, err, "缺少根级 SKILL.md")

	_, err = workspace.ValidateSkillDraft("demo", authorFiles(t, "sub/SKILL.md", skillMD("demo", "嵌套", "body")))
	requireImportRejected(t, err, "缺少根级 SKILL.md")

	_, err = workspace.ValidateSkillDraft("demo", nil)
	requireImportRejected(t, err, "为空")
}

// TestSkillImport_InvalidFrontmatter：SKILL.md 的 frontmatter 必须能解析成映射。
func TestSkillImport_InvalidFrontmatter(t *testing.T) {
	_, err := workspace.ValidateSkillDraft("demo", authorFiles(t,
		"SKILL.md", "---\n- 顶层是列表\n- 不是映射\n---\n\nbody\n",
	))
	requireImportRejected(t, err, "解析技能定义 SKILL.md")
}

// TestSkillImport_InvalidNames：技能名与 Agent 目录同名规则（M9 裁决）：拒绝空名、
// 路径分隔符、前导点与 staging / backup 后缀。
func TestSkillImport_InvalidNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, ".hidden", "demo.staging", "demo.backup"} {
		_, err := workspace.ValidateSkillDraft(name, authorFiles(t,
			"SKILL.md", skillMD("demo", "名字", "body"),
		))
		requireImportRejected(t, err, "技能名非法")
	}
}

// TestSkillImport_NoExecution：脚本字节只被存储、从不执行——导入后工作区里没有
// 任何脚本副产物（"不执行"的结构性证据）。
func TestSkillImport_NoExecution(t *testing.T) {
	const markerName = "SKILL_IMPORT_EXECUTED"
	root := initWorkspaceRoot(t)
	script := "#!/bin/sh\ntouch " + markerName + "\necho done\n"

	cand := mustWriteWorkCopy(t, root, "demo", authorFiles(t,
		"SKILL.md", skillMD("demo", "脚本", "body"),
		"scripts/hook.sh", script,
	))
	require.NotEmpty(t, cand.ContentHash)
	require.Equal(t, script, workFile(t, root, "demo", "scripts/hook.sh"), "脚本按字节原样存储")

	executed := false
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == markerName {
			executed = true
		}
		return nil
	}))
	require.False(t, executed, "导入不得执行脚本")
}

// TestSkillCandidate_ManifestHashRecomputable：候选的 SHA256 / Size / 排序 / 清单哈希
// 全部可由内容复算——ETag 与版本清单共用同一字节定义（约束 4）。
func TestSkillCandidate_ManifestHashRecomputable(t *testing.T) {
	cand, err := workspace.ValidateSkillDraft("demo", authorFiles(t,
		"docs/z.md", "z",
		"SKILL.md", skillMD("demo", "清单", "body"),
		"docs/a.md", "a",
	))
	require.NoError(t, err)
	require.Equal(t, []string{"SKILL.md", "docs/a.md", "docs/z.md"}, candidatePaths(cand))

	for _, file := range cand.Files {
		sum := sha256.Sum256(file.Content)
		require.Equal(t, hex.EncodeToString(sum[:]), file.SHA256)
		require.Equal(t, int64(len(file.Content)), file.Size)
	}

	var manifest []core.FileHash
	require.NoError(t, json.Unmarshal([]byte(cand.ManifestJSON), &manifest))
	require.Len(t, manifest, 3)
	require.Equal(t, "SKILL.md", manifest[0].Path)
	require.Equal(t, "docs/a.md", manifest[1].Path)

	sum := sha256.Sum256([]byte(cand.ManifestJSON))
	require.Equal(t, hex.EncodeToString(sum[:]), cand.ContentHash,
		"ContentHash 必须等于规整清单 JSON 的哈希")
}

// TestSkillValidate_SameGateAsImport：校验入口与写入口共用同一判据（约束 26）——
// 同一违规输入在两端得到逐字相同的错误。
func TestSkillValidate_SameGateAsImport(t *testing.T) {
	root := initWorkspaceRoot(t)
	badDraft := []core.AuthorFile{
		{Path: "SKILL.md", Content: skillMD("demo", "共同闸门", "body")},
		{Path: "../escape.md", Content: "x"},
	}

	_, draftErr := workspace.ValidateSkillDraft("demo", badDraft)
	requireImportRejected(t, draftErr, ".. 段")

	writeErr := workspace.WriteSkillWorkCopy(root, workspace.SkillCandidate{
		Name: "demo",
		Files: []workspace.SkillFile{
			{Path: "SKILL.md", Content: []byte(skillMD("demo", "共同闸门", "body"))},
			{Path: "../escape.md", Content: []byte("x")},
		},
	})
	requireImportRejected(t, writeErr, ".. 段")
	require.Equal(t, draftErr.Error(), writeErr.Error(), "校验端与写入端必须共用同一判据")
	requireNoWorkspaceEntry(t, root, "skills/demo")
}

// TestSkillValidate_NoWrites：校验端点零写入——不落工作副本、不建版本、不留操作痕迹
// （约束 26 的"不落盘"）。
func TestSkillValidate_NoWrites(t *testing.T) {
	root := initWorkspaceRoot(t)
	before := treeDigest(t, root)

	cand, err := workspace.ValidateSkillDraft("demo", authorFiles(t,
		"SKILL.md", skillMD("demo", "零写入", "body"),
		"docs/a.md", "a",
	))
	require.NoError(t, err)
	require.NotEmpty(t, cand.ContentHash)

	require.Equal(t, before, treeDigest(t, root), "校验端点必须零写入")
	requireNoWorkspaceEntry(t, root, "skills/demo")
}

// TestSkillUpdate_FullReplacement：PUT 是全量替换——未列出的文件随旧树一起消失，
// staging / backup 不留残余（约束 25）。
func TestSkillUpdate_FullReplacement(t *testing.T) {
	root := initWorkspaceRoot(t)
	mustWriteWorkCopy(t, root, "logger", authorFiles(t,
		"SKILL.md", skillMD("logger", "首版", "v1"),
		"docs/a.md", "a1",
		"docs/b.md", "b1",
	))
	require.Equal(t, "b1", workFile(t, root, "logger", "docs/b.md"))

	mustWriteWorkCopy(t, root, "logger", authorFiles(t,
		"SKILL.md", skillMD("logger", "次版", "v2"),
		"docs/a.md", "a2",
	))
	require.Equal(t, "a2", workFile(t, root, "logger", "docs/a.md"))
	requireNoWorkspaceEntry(t, root, "skills/logger/docs/b.md")
	requireNoWorkspaceEntry(t, root, "skills/logger.staging")
	requireNoWorkspaceEntry(t, root, "skills/logger.backup")
}

// TestSkillUpdate_CrashResidue：staging / backup 残留时拒绝写入并点名路径——残留的
// 裁决属于运维 / 恢复路径，写入口不替调用方删（约束 25 的崩溃窗口判据）。
func TestSkillUpdate_CrashResidue(t *testing.T) {
	root := initWorkspaceRoot(t)
	mustWriteWorkCopy(t, root, "logger", authorFiles(t,
		"SKILL.md", skillMD("logger", "首版", "v1"),
	))
	next := authorFiles(t,
		"SKILL.md", skillMD("logger", "次版", "v2"),
	)
	nextCand, err := workspace.ValidateSkillDraft("logger", next)
	require.NoError(t, err)

	writeFileInRoot(t, root, "skills/logger.staging/leftover.md", "x")
	err = workspace.WriteSkillWorkCopy(root, nextCand)
	require.Error(t, err)
	require.Contains(t, err.Error(), "skills/logger.staging")
	require.Equal(t, skillMD("logger", "首版", "v1"), workFile(t, root, "logger", "SKILL.md"),
		"拒绝写入时旧工作副本必须原样保留")
	require.NoError(t, os.RemoveAll(filepath.Join(root, "skills", "logger.staging")))

	writeFileInRoot(t, root, "skills/logger.backup/leftover.md", "x")
	err = workspace.WriteSkillWorkCopy(root, nextCand)
	require.Error(t, err)
	require.Contains(t, err.Error(), "skills/logger.backup")
	require.Equal(t, skillMD("logger", "首版", "v1"), workFile(t, root, "logger", "SKILL.md"))
}

// TestSkillWrite_RejectsTamperedCandidate：候选在校验后被改动（哈希与清单不自洽）→
// 拒绝写入，零残留。
func TestSkillWrite_RejectsTamperedCandidate(t *testing.T) {
	root := initWorkspaceRoot(t)
	cand, err := workspace.ValidateSkillDraft("logger", authorFiles(t,
		"SKILL.md", skillMD("logger", "篡改", "v1"),
	))
	require.NoError(t, err)

	tampered := cand
	tampered.ContentHash = strings.Repeat("0", 64)
	err = workspace.WriteSkillWorkCopy(root, tampered)
	require.Error(t, err)
	require.Contains(t, err.Error(), "不一致")
	requireNoWorkspaceEntry(t, root, "skills/logger")
}

// TestSkillVersion_Immutable：发布版本不可变——同内容重复冻结幂等复用；同版本号换
// 内容拒绝覆盖；已发布文件只读（约束 25；文档原文用例名）。
func TestSkillVersion_Immutable(t *testing.T) {
	root := initWorkspaceRoot(t)
	unlockTreeOnCleanup(t, root)

	md := skillMD("logger", "不可变", "v1")
	cand1 := mustWriteWorkCopy(t, root, "logger", authorFiles(t,
		"SKILL.md", md,
		"docs/a.md", "a1",
	))
	storagePath, err := workspace.SnapshotSkill(root, cand1, workspaceTestUID, 1)
	require.NoError(t, err)
	require.Equal(t, ".basalt/"+publishedRel("logger", 1), storagePath)

	published := filepath.Join(root, filepath.FromSlash(publishedRel("logger", 1)))
	raw, err := os.ReadFile(filepath.Join(published, "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, md, string(raw))

	again, err := workspace.SnapshotSkill(root, cand1, workspaceTestUID, 1)
	require.NoError(t, err, "同内容重复冻结必须幂等")
	require.Equal(t, storagePath, again)

	if os.Geteuid() != 0 {
		writeErr := os.WriteFile(filepath.Join(published, "SKILL.md"), []byte("tampered"), 0o644)
		require.Error(t, writeErr, "已发布版本文件必须只读")
	}

	md2 := skillMD("logger", "不可变", "v2")
	cand2 := mustWriteWorkCopy(t, root, "logger", authorFiles(t, "SKILL.md", md2))
	_, err = workspace.SnapshotSkill(root, cand2, workspaceTestUID, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "拒绝覆盖")

	raw, err = os.ReadFile(filepath.Join(published, "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, md, string(raw), "拒绝覆盖后已发布字节不得改变")
}

// TestSkillSnapshot_WorkCopyDrift：校验后工作副本被改动 → 冻结拒绝，不复制不一致
// 的字节，也不留半截版本目录（约束 25 的"发布的就是校验过的"）。
func TestSkillSnapshot_WorkCopyDrift(t *testing.T) {
	root := initWorkspaceRoot(t)
	cand := mustWriteWorkCopy(t, root, "logger", authorFiles(t,
		"SKILL.md", skillMD("logger", "漂移", "v1"),
	))

	tampered := filepath.Join(root, "skills", "logger", "SKILL.md")
	require.NoError(t, os.WriteFile(tampered, []byte(skillMD("logger", "漂移", "v2")), 0o644))

	_, err := workspace.SnapshotSkill(root, cand, workspaceTestUID, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "不一致")
	requireNoWorkspaceEntry(t, root, publishedRel("logger", 1))
}

// TestSkillSnapshot_RejectsSymlink：工作副本里出现软连接 → 冻结拒绝（不跟随、不复制
// 目录外字节）。无符号链接权限的平台按约定跳过并写明理由。
func TestSkillSnapshot_RejectsSymlink(t *testing.T) {
	root := initWorkspaceRoot(t)
	cand := mustWriteWorkCopy(t, root, "logger", authorFiles(t,
		"SKILL.md", skillMD("logger", "软连接", "v1"),
	))

	target := filepath.Join(root, "skills", "logger", "docs", "link.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
	if err := os.Symlink(filepath.Join(root, "skills", "logger", "SKILL.md"), target); err != nil {
		t.Logf("本平台不允许创建符号链接，跳过软连接拒绝用例：%v", err)
		return
	}

	_, err := workspace.SnapshotSkill(root, cand, workspaceTestUID, 1)
	require.Error(t, err)
	requireNoWorkspaceEntry(t, root, publishedRel("logger", 1))
}

// TestSkillDelete_WorkCopyArchived：删除 = 工作副本移入 workspace 归档区，时间戳按
// UTC（约束 12 / D-10）；内容逐字节保留，目录本体不再出现。
func TestSkillDelete_WorkCopyArchived(t *testing.T) {
	root := initWorkspaceRoot(t)
	mustWriteWorkCopy(t, root, "logger", authorFiles(t,
		"SKILL.md", skillMD("logger", "归档", "v1"),
		"docs/a.md", "a",
	))

	at := time.Date(2026, 10, 22, 3, 4, 5, 0, time.FixedZone("CST", 8*3600))
	archivePath, err := workspace.ArchiveSkillWorkCopy(root, "logger", at)
	require.NoError(t, err)
	require.Equal(t, "archive/skills/logger-20261021-190405", archivePath, "时间戳必须按 UTC")

	requireNoWorkspaceEntry(t, root, "skills/logger")
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(archivePath), "docs", "a.md"))
	require.NoError(t, err)
	require.Equal(t, "a", string(raw))
}

// TestSkillArchive_ConflictKeepsSource：归档目标已存在 → ErrConflict（409 语义），
// 不自动换名、源工作副本原样保留（约束 32 的时间戳冲突口径）。
func TestSkillArchive_ConflictKeepsSource(t *testing.T) {
	root := initWorkspaceRoot(t)
	md := skillMD("logger", "冲突", "v1")
	mustWriteWorkCopy(t, root, "logger", authorFiles(t, "SKILL.md", md))

	at := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	_, err := workspace.ArchiveSkillWorkCopy(root, "logger", at)
	require.NoError(t, err)

	mustWriteWorkCopy(t, root, "logger", authorFiles(t, "SKILL.md", md))
	_, err = workspace.ArchiveSkillWorkCopy(root, "logger", at)
	require.Error(t, err)
	var adminErr *core.AdminError
	require.ErrorAs(t, err, &adminErr)
	require.ErrorIs(t, err, core.ErrConflict)
	require.Equal(t, md, workFile(t, root, "logger", "SKILL.md"), "冲突时源目录必须原样保留")
}

// TestSkillArchive_MissingSourceAndBadName：误操作可读——不存在的技能与非法名字都
// 报错点名，不做任何移动。
func TestSkillArchive_MissingSourceAndBadName(t *testing.T) {
	root := initWorkspaceRoot(t)

	_, err := workspace.ArchiveSkillWorkCopy(root, "ghost", time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "不存在")

	_, err = workspace.ArchiveSkillWorkCopy(root, "a/b", time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "技能名非法")
}
