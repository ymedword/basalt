package workspace

// 本文件是技能不可变版本的验收测试（颗粒度文档 §3.2.1：workspace/skill_version_test.go
// 十四例；族 TestSkillVersion_* / TestSkillLibrary_*）。采用**同包白盒**：四个哈希按
// 约束 32 的字节定义对着固定向量判卷——测试自己用 crypto/sha256 与字面量拼出期望值，
// 不调用被测的哈希函数反推。
// This file covers immutable skill versions. White-box on purpose: the four hashes are
// judged against fixed vectors built from crypto/sha256 and literals.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// vectorSkillMD 是固定向量的 SKILL.md：frontmatter 有 name / description，正文带
// 前导空行与结尾换行——正好区分"原始字节"与 splitFrontmatter 的 Trim 行为。
// vectorSkillMD is the fixed-vector SKILL.md with a leading blank line after the fence.
const vectorSkillMD = "---\nname: writer\ndescription: 写作助手\n---\n\n正文第一行\n\n第二段\n"

// vectorBody 是 frontmatter 之后的正文原始字节（闭合围栏换行符之后，含前导空行）。
const vectorBody = "\n正文第一行\n\n第二段\n"

// vectorGuide 是 L3 文件的固定内容。
const vectorGuide = "guide line\n"

// testSkillUID / testSkillUIDOther 是两个固定的合法资源身份（rs_ + 26 位 Crockford 载荷）：
// 发布目录按身份分段，同名重建（新 UID）必须与上一代的版本 1 落在不同目录。
const (
	testSkillUID      = core.ResourceUID("rs_01J9ZQ7K3W8V2N5R6T4YHBCDEF")
	testSkillUIDOther = core.ResourceUID("rs_01J9ZQ7K3W8V2N5R6T4YHBCDE9")
)

// testVersionDir 用字面量拼出固定身份下的发布目录逻辑路径（不调用被测的映射函数）。
func testVersionDir(name string, uid core.ResourceUID, versionNo int) string {
	return fmt.Sprintf(".basalt/versions/skills/%s/%s/%d", name, uid, versionNo)
}

// testVersionRel 拼出同一目录相对工作区根的形态。
func testVersionRel(name string, uid core.ResourceUID, versionNo int) string {
	return fmt.Sprintf("versions/skills/%s/%s/%d", name, uid, versionNo)
}

// versionDirPath 给出发布目录在磁盘上的绝对路径。
func versionDirPath(root, name string, uid core.ResourceUID, versionNo int) string {
	return filepath.Join(root, filepath.FromSlash(testVersionRel(name, uid, versionNo)))
}

// newSkillRoot 建一个临时工作区根（.basalt/ 本身）。
func newSkillRoot(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// writeWorkSkillFile 写工作副本文件（自动创建父目录）。
func writeWorkSkillFile(t *testing.T, root, name, rel string, raw []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(skillWorkDir(name)), filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), dirPerm))
	require.NoError(t, os.WriteFile(full, raw, filePerm))
}

// sha256Literal 独立计算期望哈希（不经过被测实现）。
func sha256Literal(t *testing.T, raw []byte) string {
	t.Helper()
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// fileEntryJSON 按契约字段序拼一条清单条；测试路径都是 ASCII，%q 与 JSON 引号同形。
func fileEntryJSON(filePath, hash string, size int) string {
	return fmt.Sprintf(`{"path":%q,"sha256":%q,"size":%d}`, filePath, hash, size)
}

// TestSkillVersion_HashWorkCopyFixedVector：工作副本的四个哈希、文件清单与卡片命名
// 逐项对固定向量；非法技能名（含路径分隔符）在读取前即拒绝。
func TestSkillVersion_HashWorkCopyFixedVector(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))
	writeWorkSkillFile(t, root, "writer", "references/guide.md", []byte(vectorGuide))

	version, err := NewSkillLibrary(root).HashWorkCopy("writer")
	require.NoError(t, err)

	skillMDHash := sha256Literal(t, []byte(vectorSkillMD))
	guideHash := sha256Literal(t, []byte(vectorGuide))
	contentJSON := "[" + fileEntryJSON("SKILL.md", skillMDHash, len(vectorSkillMD)) + "," +
		fileEntryJSON("references/guide.md", guideHash, len(vectorGuide)) + "]"
	l3JSON := "[" + fileEntryJSON("references/guide.md", guideHash, len(vectorGuide)) + "]"
	cardLine := "- writer：写作助手（.basalt/skills/writer/SKILL.md）\n"

	require.Equal(t, 0, version.VersionNo)
	require.Empty(t, version.Dir)
	require.Equal(t, skillMDHash, version.SkillMDHash)
	require.Equal(t, sha256Literal(t, []byte(cardLine)), version.L1Hash)
	require.Equal(t, sha256Literal(t, []byte(vectorBody)), version.L2Hash)
	require.Equal(t, sha256Literal(t, []byte(l3JSON)), version.L3Hash)
	require.Equal(t, sha256Literal(t, []byte(contentJSON)), version.ContentHash)
	require.Equal(t, []core.FileHash{
		{Path: "SKILL.md", SHA256: skillMDHash, Size: int64(len(vectorSkillMD))},
		{Path: "references/guide.md", SHA256: guideHash, Size: int64(len(vectorGuide))},
	}, version.Files)

	_, err = NewSkillLibrary(root).HashWorkCopy("../agents/writer")
	require.ErrorContains(t, err, "单个目录名")
}

// TestSkillVersion_L1CardLineUsesWorkCopyPath：工作副本的 L1 是工作副本卡片行；
// 发布版本的 L1 换用发布真路径（约束 31 的 D-02 口径），两者路径不同、命名相同。
func TestSkillVersion_L1CardLineUsesWorkCopyPath(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))

	library := NewSkillLibrary(root)
	work, err := library.HashWorkCopy("writer")
	require.NoError(t, err)
	require.Equal(t, ".basalt/skills/writer/SKILL.md", library.CardPath(work))

	published, err := library.Materialize(context.Background(), "writer", testSkillUID, 1, work.ContentHash)
	require.NoError(t, err)
	require.Equal(t, testVersionDir("writer", testSkillUID, 1)+"/SKILL.md", library.CardPath(published))

	publishedLine := "- writer：写作助手（" + testVersionDir("writer", testSkillUID, 1) + "/SKILL.md）\n"
	require.Equal(t, sha256Literal(t, []byte(publishedLine)), published.L1Hash)
	require.NotEqual(t, work.L1Hash, published.L1Hash)
}

// TestSkillVersion_L2KeepsRawBodyBytes：L2 覆盖闭合围栏之后的**全部**原始字节，
// 含前导空行与结尾换行——不是 Trim 后的正文。
func TestSkillVersion_L2KeepsRawBodyBytes(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))

	version, err := NewSkillLibrary(root).HashWorkCopy("writer")
	require.NoError(t, err)
	require.Equal(t, sha256Literal(t, []byte(vectorBody)), version.L2Hash)

	trimmed := sha256Literal(t, []byte(strings.Trim(vectorBody, "\r\n")))
	require.NotEqual(t, trimmed, version.L2Hash, "L2 不得对正文做 Trim")
}

// TestSkillVersion_CRLFBodyIsNotNormalized：CRLF 正文按原字节哈希，与 LF 版本不同
// （约束 32 明确不做换行归一）。
func TestSkillVersion_CRLFBodyIsNotNormalized(t *testing.T) {
	crlfMD := "---\r\nname: writer\r\ndescription: 写作助手\r\n---\r\n\r\n正文第一行\r\n"
	crlfBody := "\r\n正文第一行\r\n"

	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(crlfMD))
	crlfVersion, err := NewSkillLibrary(root).HashWorkCopy("writer")
	require.NoError(t, err)
	require.Equal(t, sha256Literal(t, []byte(crlfBody)), crlfVersion.L2Hash)

	lfRoot := newSkillRoot(t)
	writeWorkSkillFile(t, lfRoot, "writer", "SKILL.md", []byte("---\nname: writer\ndescription: 写作助手\n---\n\n正文第一行\n"))
	lfVersion, err := NewSkillLibrary(lfRoot).HashWorkCopy("writer")
	require.NoError(t, err)

	require.NotEqual(t, lfVersion.L2Hash, crlfVersion.L2Hash, "换行风格必须进哈希")
	require.NotEqual(t, lfVersion.SkillMDHash, crlfVersion.SkillMDHash)
}

// TestSkillVersion_L2WithoutFrontmatterUsesWholeFile：没有 frontmatter 时 L2 = 整份字节。
func TestSkillVersion_L2WithoutFrontmatterUsesWholeFile(t *testing.T) {
	raw := "整份都是正文\n没有围栏\n"
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(raw))

	version, err := NewSkillLibrary(root).HashWorkCopy("writer")
	require.NoError(t, err)
	require.Equal(t, version.SkillMDHash, version.L2Hash)
	require.Equal(t, sha256Literal(t, []byte(raw)), version.L2Hash)
}

// TestSkillVersion_EmptyL3HashesEmptyArray：没有非 SKILL.md 文件时 L3 = sha256("[]")，
// 不是 sha256("null") 或空串。
func TestSkillVersion_EmptyL3HashesEmptyArray(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))

	version, err := NewSkillLibrary(root).HashWorkCopy("writer")
	require.NoError(t, err)
	require.Equal(t, sha256Literal(t, []byte("[]")), version.L3Hash)
	require.Len(t, version.Files, 1)
}

// TestSkillVersion_RejectsNonUTF8SkillMD：SKILL.md 不是合法 UTF-8 → 拒绝计算哈希。
func TestSkillVersion_RejectsNonUTF8SkillMD(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte{'#', ' ', 0xff, 0xfe, '\n'})

	_, err := NewSkillLibrary(root).HashWorkCopy("writer")
	require.ErrorContains(t, err, "UTF-8")
}

// TestSkillVersion_RejectsMissingSKILLMD：只有别的文件、没有 SKILL.md → 不是技能实体，
// 错误必须可被 errors.Is(fs.ErrNotExist) 识别（基线据此跳过建版本）。
func TestSkillVersion_RejectsMissingSKILLMD(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "notes.md", []byte("no skill here\n"))

	_, err := NewSkillLibrary(root).HashWorkCopy("writer")
	require.Error(t, err)
	require.True(t, errors.Is(err, fs.ErrNotExist), "缺 SKILL.md 必须包装 fs.ErrNotExist：%v", err)
}

// TestSkillVersion_FilesAreSortedByPath：文件清单按 path 升序，与创建顺序无关。
func TestSkillVersion_FilesAreSortedByPath(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "zeta.md", []byte("z\n"))
	writeWorkSkillFile(t, root, "writer", "references/b.md", []byte("b\n"))
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))
	writeWorkSkillFile(t, root, "writer", "references/a.md", []byte("a\n"))

	version, err := NewSkillLibrary(root).HashWorkCopy("writer")
	require.NoError(t, err)

	paths := make([]string, 0, len(version.Files))
	for _, file := range version.Files {
		paths = append(paths, file.Path)
	}
	require.Equal(t, []string{"SKILL.md", "references/a.md", "references/b.md", "zeta.md"}, paths)
}

// TestSkillLibrary_MaterializeFreezesPublishedCopy：物化后发布目录冻结在版本目录；
// 工作副本随后改动不影响发布版本，发布版本的 L1 指向发布真路径。
func TestSkillLibrary_MaterializeFreezesPublishedCopy(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))

	library := NewSkillLibrary(root)
	work, err := library.HashWorkCopy("writer")
	require.NoError(t, err)

	published, err := library.Materialize(context.Background(), "writer", testSkillUID, 1, work.ContentHash)
	require.NoError(t, err)
	require.Equal(t, testVersionDir("writer", testSkillUID, 1), published.Dir)
	require.Equal(t, 1, published.VersionNo)
	require.Equal(t, work.Files, published.Files)
	require.Equal(t, work.ContentHash, published.ContentHash)
	require.Equal(t, work.SkillMDHash, published.SkillMDHash)
	require.Equal(t, work.L2Hash, published.L2Hash)
	require.Equal(t, work.L3Hash, published.L3Hash)

	// 改动工作副本：发布版本逐字节不变。
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte("---\nname: writer\ndescription: 已改动\n---\n\n新正文\n"))
	reloaded, err := library.LoadPublished(context.Background(), "writer", 1, published.Dir)
	require.NoError(t, err)
	require.Equal(t, published.ContentHash, reloaded.ContentHash)
	require.Equal(t, published.SkillMDHash, reloaded.SkillMDHash)

	raw, err := os.ReadFile(filepath.Join(versionDirPath(root, "writer", testSkillUID, 1), "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, vectorSkillMD, string(raw))
}

// TestSkillLibrary_MaterializeIsIdempotent：同版本号同内容重复物化 → 直接复用，
// 不重写文件、不报错。
func TestSkillLibrary_MaterializeIsIdempotent(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))
	writeWorkSkillFile(t, root, "writer", "references/guide.md", []byte(vectorGuide))

	library := NewSkillLibrary(root)
	work, err := library.HashWorkCopy("writer")
	require.NoError(t, err)

	first, err := library.Materialize(context.Background(), "writer", testSkillUID, 1, work.ContentHash)
	require.NoError(t, err)
	second, err := library.Materialize(context.Background(), "writer", testSkillUID, 1, work.ContentHash)
	require.NoError(t, err)
	require.Equal(t, first.ContentHash, second.ContentHash)
	require.Equal(t, first.Files, second.Files)

	entries, err := os.ReadDir(versionDirPath(root, "writer", testSkillUID, 1))
	require.NoError(t, err)
	require.Len(t, entries, 2)
}

// TestSkillLibrary_MaterializeRejectsHashMismatch：期望哈希与工作副本不符 → 拒绝冻结；
// 已存在的发布目录内容不同 → 拒绝覆盖且原地不动（不可变目录）。
func TestSkillLibrary_MaterializeRejectsHashMismatch(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))

	library := NewSkillLibrary(root)
	_, err := library.Materialize(context.Background(), "writer", testSkillUID, 1, strings.Repeat("a", 64))
	require.ErrorContains(t, err, "拒绝冻结")
	_, statErr := os.Stat(versionDirPath(root, "writer", testSkillUID, 1))
	require.True(t, errors.Is(statErr, fs.ErrNotExist), "失败不得留下发布目录")

	work, err := library.HashWorkCopy("writer")
	require.NoError(t, err)
	published, err := library.Materialize(context.Background(), "writer", testSkillUID, 1, work.ContentHash)
	require.NoError(t, err)

	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte("---\nname: writer\ndescription: 已改动\n---\n\n新正文\n"))
	changed, err := library.HashWorkCopy("writer")
	require.NoError(t, err)
	require.NotEqual(t, published.ContentHash, changed.ContentHash)

	_, err = library.Materialize(context.Background(), "writer", testSkillUID, 1, changed.ContentHash)
	require.ErrorContains(t, err, "拒绝覆盖")

	raw, err := os.ReadFile(filepath.Join(versionDirPath(root, "writer", testSkillUID, 1), "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, vectorSkillMD, string(raw), "已发布目录必须原地不动")
}

// TestSkillLibrary_LoadPublishedRecomputesHashes：发布目录被改动后，重算的哈希随之
// 变化——调用方据此发现与 manifest_json 不符（不静默降级）。
func TestSkillLibrary_LoadPublishedRecomputesHashes(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))
	writeWorkSkillFile(t, root, "writer", "references/guide.md", []byte(vectorGuide))

	library := NewSkillLibrary(root)
	work, err := library.HashWorkCopy("writer")
	require.NoError(t, err)
	published, err := library.Materialize(context.Background(), "writer", testSkillUID, 1, work.ContentHash)
	require.NoError(t, err)

	require.Equal(t, published.ContentHash, mustLoad(t, library, published.Dir).ContentHash)

	require.NoError(t, os.WriteFile(filepath.Join(versionDirPath(root, "writer", testSkillUID, 1), "references", "guide.md"), []byte("tampered\n"), filePerm))
	reloaded := mustLoad(t, library, published.Dir)
	require.NotEqual(t, published.ContentHash, reloaded.ContentHash)
	require.NotEqual(t, published.L3Hash, reloaded.L3Hash)
	require.Equal(t, published.SkillMDHash, reloaded.SkillMDHash)
}

// mustLoad 读发布版本并断言无错误。
func mustLoad(t *testing.T, library *SkillLibrary, dir string) SkillVersion {
	t.Helper()
	version, err := library.LoadPublished(context.Background(), "writer", 1, dir)
	require.NoError(t, err)
	return version
}

// TestSkillLibrary_LoadPublishedRejectsForeignPath：storage_path 只接受两种契约形态——
// 工作副本路径、缺前缀、相对逃逸、非规范 UID、版本号不符、空串都必须拒绝，不得把它们
// 当真路径去读。
func TestSkillLibrary_LoadPublishedRejectsForeignPath(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))

	library := NewSkillLibrary(root)
	work, err := library.HashWorkCopy("writer")
	require.NoError(t, err)
	_, err = library.Materialize(context.Background(), "writer", testSkillUID, 1, work.ContentHash)
	require.NoError(t, err)

	cases := []struct {
		label string
		dir   string
		want  string
	}{
		{"工作副本路径", ".basalt/skills/writer", "不符合契约形态"},
		{"缺 .basalt 前缀", "versions/skills/writer/1", "不符合契约形态"},
		{"层级过多", ".basalt/versions/skills/writer/x/y/1", "不符合契约形态"},
		{"相对逃逸", ".basalt/versions/skills/writer/1/../2", "不是规范形态"},
		{"尾斜杠", ".basalt/versions/skills/writer/" + string(testSkillUID) + "/1/", "不是规范形态"},
		{"身份非法", ".basalt/versions/skills/writer/not-a-uid/1", "资源身份非法"},
		{"身份非规范形态", ".basalt/versions/skills/writer/" + strings.ToLower(string(testSkillUID)) + "/1", "不是规范形态"},
		{"版本号不符登记", ".basalt/versions/skills/writer/2", "不一致"},
		{"空串", "", "storage_path 为空"},
	}
	for _, tc := range cases {
		_, err := library.LoadPublished(context.Background(), "writer", 1, tc.dir)
		require.ErrorContains(t, err, tc.want, "路径 %q 必须被拒绝", tc.dir)
	}

	// 另一只技能的目录不得被当作本技能的发布目录读取。
	foreignName := ".basalt/versions/skills/other/" + string(testSkillUID) + "/1"
	_, err = library.LoadPublished(context.Background(), "writer", 1, foreignName)
	require.ErrorContains(t, err, "属于技能 other")
}

// TestSkillLibrary_LegacyFlatDirStaysReadable：旧扁平目录不被搬迁、不被改写——按记录
// 的 storage_path 原样读取；RESTORE 也按记录路径重建，不迁到身份分段形态。
func TestSkillLibrary_LegacyFlatDirStaysReadable(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))

	legacyRel := "versions/skills/writer/1"
	legacyDir := ".basalt/versions/skills/writer/1"
	writeSnapshotFile(t, root, legacyRel+"/SKILL.md", []byte(vectorSkillMD))

	library := NewSkillLibrary(root)
	work, err := library.HashWorkCopy("writer")
	require.NoError(t, err)

	legacy, err := library.LoadPublished(context.Background(), "writer", 1, legacyDir)
	require.NoError(t, err)
	require.Equal(t, work.ContentHash, legacy.ContentHash)
	require.Equal(t, legacyDir, legacy.Dir)
	require.Equal(t, legacyDir+"/SKILL.md", library.CardPath(legacy))

	// 新发布用身份分段：同名同版本的旧目录不受影响，新目录另起一支。
	published, err := library.Materialize(context.Background(), "writer", testSkillUID, 1, work.ContentHash)
	require.NoError(t, err)
	require.NotEqual(t, legacyDir, published.Dir)
	relative, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(legacyRel), "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, vectorSkillMD, string(relative), "旧扁平目录必须原地不动")

	// 记录路径被删后按记录原位重建（不迁移）。
	require.NoError(t, os.RemoveAll(filepath.Join(root, filepath.FromSlash(legacyRel))))
	restored, err := library.RestorePublished(context.Background(), "writer", 1, work.ContentHash, legacyDir)
	require.NoError(t, err)
	require.Equal(t, legacyDir, restored.Dir)
	require.Equal(t, work.ContentHash, restored.ContentHash)
	_, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(legacyRel), "SKILL.md"))
	require.NoError(t, statErr, "重建必须落在记录的旧扁平目录，而不是身份分段目录")
}

// TestSkillLibrary_SameNameTwoGenerationsShareVersionOne：同名重建的两个身份各自拥有
// 版本 1，内容不同也不冲突——物化、读取与卡片路径互不覆盖。
func TestSkillLibrary_SameNameTwoGenerationsShareVersionOne(t *testing.T) {
	root := newSkillRoot(t)
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))

	library := NewSkillLibrary(root)
	first, err := library.HashWorkCopy("writer")
	require.NoError(t, err)
	firstVersion, err := library.Materialize(context.Background(), "writer", testSkillUID, 1, first.ContentHash)
	require.NoError(t, err)

	secondMD := "---\nname: writer\ndescription: 重建后的说明\n---\n\n重建后的正文\n"
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(secondMD))
	second, err := library.HashWorkCopy("writer")
	require.NoError(t, err)
	require.NotEqual(t, first.ContentHash, second.ContentHash)
	secondVersion, err := library.Materialize(context.Background(), "writer", testSkillUIDOther, 1, second.ContentHash)
	require.NoError(t, err)
	require.Equal(t, 1, secondVersion.VersionNo, "新身份的第一版仍然是版本 1")

	require.NotEqual(t, firstVersion.Dir, secondVersion.Dir)
	require.Equal(t, testVersionDir("writer", testSkillUID, 1), firstVersion.Dir)
	require.Equal(t, testVersionDir("writer", testSkillUIDOther, 1), secondVersion.Dir)

	// 两代都可读，且各自读到自己的字节。
	againFirst, err := library.LoadPublished(context.Background(), "writer", 1, firstVersion.Dir)
	require.NoError(t, err)
	require.Equal(t, first.ContentHash, againFirst.ContentHash)
	againSecond, err := library.LoadPublished(context.Background(), "writer", 1, secondVersion.Dir)
	require.NoError(t, err)
	require.Equal(t, second.ContentHash, againSecond.ContentHash)

	raw, err := os.ReadFile(filepath.Join(versionDirPath(root, "writer", testSkillUID, 1), "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, vectorSkillMD, string(raw), "上一代的文件不得被同名重建覆盖")
}

// snapshotAgentMD 是声明了 writer 技能的 AGENT.md 夹具：段一冻结只读这一份正文。
const snapshotAgentMD = "---\nname: writer\ndescription: 快照夹具\nskills:\n  - writer\n---\n\n你是「writer」。\n"

// snapshotEntry 返回与夹具同名的已注册条目（绑定声明只有一个技能）。
func snapshotEntry() core.AgentEntry {
	return core.AgentEntry{
		Name:       "writer",
		Registered: true,
		Profile:    &core.Profile{Name: "writer", Skills: []string{"writer"}},
	}
}

// snapshotNow 是快照夹具的固定时钟（段一末尾只影响正文，不进哈希）。
func snapshotNow() time.Time {
	return time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
}

// writeSnapshotFile 在工作区根下写任意相对路径文件（自动创建父目录）。
func writeSnapshotFile(t *testing.T, root, rel string, raw []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), dirPerm))
	require.NoError(t, os.WriteFile(full, raw, filePerm))
}

// manifestMap 把清单摊平成 path → sha256（清单顺序另由 core 用例判卷）。
func manifestMap(snapshot *core.ConfigSnapshot) map[string]string {
	m := make(map[string]string, len(snapshot.Manifest))
	for _, entry := range snapshot.Manifest {
		m[entry.Path] = entry.SHA256
	}
	return m
}

// TestSkillVersion_SnapshotPublishedUsesPublishedCard：发布版本快照的卡片行展示发布
// 真路径，清单仍记逻辑路径；发布之后改写工作副本，快照读到的仍是发布版本的字节
// ——"读即所记"与"运行中的运行不受工作副本影响"（约束 31）。
func TestSkillVersion_SnapshotPublishedUsesPublishedCard(t *testing.T) {
	root := newSkillRoot(t)
	writeSnapshotFile(t, root, "agents/writer/AGENT.md", []byte(snapshotAgentMD))
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))

	library := NewSkillLibrary(root)
	work, err := library.HashWorkCopy("writer")
	require.NoError(t, err)
	published, err := library.Materialize(context.Background(), "writer", testSkillUID, 1, work.ContentHash)
	require.NoError(t, err)
	require.Equal(t, testVersionDir("writer", testSkillUID, 1), published.Dir)

	// 发布之后改写工作副本：发布版本快照**不得**读到它（不回落到工作副本）。
	changedMD := "---\nname: writer\ndescription: 改过的说明\n---\n\n改过的正文\n"
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(changedMD))

	snapshot, err := NewContextLoader(nil).SnapshotPublished(root, snapshotEntry(),
		PublishedSkills{"writer": published.Dir}, snapshotNow())
	require.NoError(t, err)

	// 卡片行展示发布真路径（约束 31 的字面量）。
	require.Contains(t, snapshot.SectionOne,
		"- writer：写作助手（"+published.Dir+"/SKILL.md）")
	require.NotContains(t, snapshot.SectionOne, "改过的说明")
	require.NotContains(t, snapshot.SectionOne, ".basalt/skills/writer/SKILL.md")

	// 清单记逻辑路径，哈希是发布版本的字节；发布真路径**不进**清单。
	manifest := manifestMap(snapshot)
	require.Equal(t, sha256Literal(t, []byte(vectorSkillMD)), manifest[".basalt/skills/writer/SKILL.md"])
	require.Equal(t, sha256Literal(t, []byte(snapshotAgentMD)), manifest["agents/writer/AGENT.md"])
	require.NotContains(t, manifest, published.Dir+"/SKILL.md")
}

// TestSkillVersion_SnapshotPublishedFailsClosed：声明技能缺目录映射、目录形态不符、
// 版本号非法、发布目录或 SKILL.md 读不到，一律失败关闭——工作副本明明存在也不得
// 回落（约束 31）。
func TestSkillVersion_SnapshotPublishedFailsClosed(t *testing.T) {
	root := newSkillRoot(t)
	writeSnapshotFile(t, root, "agents/writer/AGENT.md", []byte(snapshotAgentMD))
	// 工作副本存在：任何回落都会让下面的用例"看起来成功"。
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))
	// 版本 2 的目录存在但没有 SKILL.md（只有 L3 文件）。
	writeSnapshotFile(t, root, "versions/skills/writer/2/references/guide.md", []byte(vectorGuide))

	cases := []struct {
		label  string
		skills PublishedSkills
		want   string
	}{
		{"缺映射", nil, "缺少发布目录"},
		{"空映射", PublishedSkills{}, "缺少发布目录"},
		{"映射值为空串", PublishedSkills{"writer": "  "}, "缺少发布目录"},
		{"目录形态不符", PublishedSkills{"writer": ".basalt/skills/writer"}, "不符合契约形态"},
		{"版本号为零", PublishedSkills{"writer": ".basalt/versions/skills/writer/0"}, "版本号非法"},
		{"版本号带前导零", PublishedSkills{"writer": ".basalt/versions/skills/writer/01"}, "版本号非法"},
		{"发布目录不存在", PublishedSkills{"writer": ".basalt/versions/skills/writer/3"}, "读取技能定义"},
		{"SKILL.md 缺失", PublishedSkills{"writer": ".basalt/versions/skills/writer/2"}, "读取技能定义"},
	}
	for _, tc := range cases {
		_, err := NewContextLoader(nil).SnapshotPublished(root, snapshotEntry(), tc.skills, snapshotNow())
		require.ErrorContains(t, err, tc.want, "场景「%s」必须失败关闭", tc.label)
	}
}

// TestSkillVersion_SnapshotWorkCopyKeepsLegacyPaths：工作副本快照的卡片行与清单路径
// 保持既有口径（卡片展示 `.basalt/skills/<name>/SKILL.md`、清单记 `skills/<name>/SKILL.md`），
// 即使同名发布版本存在也不改读发布目录——「工作副本口径不变」。
func TestSkillVersion_SnapshotWorkCopyKeepsLegacyPaths(t *testing.T) {
	root := newSkillRoot(t)
	writeSnapshotFile(t, root, "agents/writer/AGENT.md", []byte(snapshotAgentMD))
	writeWorkSkillFile(t, root, "writer", "SKILL.md", []byte(vectorSkillMD))

	library := NewSkillLibrary(root)
	work, err := library.HashWorkCopy("writer")
	require.NoError(t, err)
	_, err = library.Materialize(context.Background(), "writer", testSkillUID, 1, work.ContentHash)
	require.NoError(t, err)

	snapshot, err := NewContextLoader(nil).Snapshot(root, snapshotEntry(), snapshotNow())
	require.NoError(t, err)

	require.Contains(t, snapshot.SectionOne, "- writer：写作助手（.basalt/skills/writer/SKILL.md）")
	require.NotContains(t, snapshot.SectionOne, ".basalt/versions/skills/")

	manifest := manifestMap(snapshot)
	require.Equal(t, sha256Literal(t, []byte(vectorSkillMD)), manifest["skills/writer/SKILL.md"])
	require.NotContains(t, manifest, testVersionDir("writer", testSkillUID, 1)+"/SKILL.md")
}
