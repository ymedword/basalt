package knowledge

// 本文件是工作区读取器（`RootReader`）的单元用例：图 3「前置」段的四项判定
// ——允许根、真实路径投影、大小上限、UTF-8 解码——每条都有拒绝路径用例。
//
// 判别力（逆改点）：
//   - 把 `withinRoot` 的前缀比较改成 `strings.HasPrefix` → `TestRootReader_RejectsLinkEscape`
//     的根外链接用例变绿（错误的绿），该用例红；
//   - 去掉 `rejectLinks` → 根内链接用例红（设计明确要求拒绝链接，而不只是拒绝逃逸）；
//   - 把 `readBounded` 的上限判断挪到读完以后 → 大小上限用例仍可红，但 `LimitReader`
//     的字节上界不再成立；用例断言错误文案里的上限值，防止悄悄改成"截断后继续"；
//   - 去掉 `utf8.Valid` → 非法字节用例红。
//
// The cases pin each precondition with a rejection path; the header lists the mutation
// each one is designed to catch.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// mustRootReader 建一个已投影的读取器；构造失败直接终止用例。
func mustRootReader(t *testing.T, root string, maxBytes int64) *RootReader {
	t.Helper()
	reader, err := NewRootReader(root, maxBytes)
	require.NoError(t, err)
	return reader
}

// writeWorkspaceFile 在工作区根内写一个文件（自动建目录），返回原始字节。
func writeWorkspaceFile(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o644))
}

// TestRootReader_LoadsValidDocument 守正常路径：正文、大小、内容哈希与相对路径
// 四项一一对应，且 `ContentHash == sha256(Text)`（ports.go 的显式规则）。
func TestRootReader_LoadsValidDocument(t *testing.T) {
	root := t.TempDir()
	text := "标题\n正文第一段\r\n第二段：世界\n"
	writeWorkspaceFile(t, root, "docs/a.md", []byte(text))

	src, err := mustRootReader(t, root, 1<<20).Load(t.Context(), "docs/a.md")
	require.NoError(t, err)

	require.Equal(t, "docs/a.md", src.Path)
	require.Equal(t, text, src.Text)
	require.Equal(t, int64(len(text)), src.SizeBytes)

	sum := sha256.Sum256([]byte(text))
	require.Equal(t, hex.EncodeToString(sum[:]), src.ContentHash)
	require.Equal(t, sha256Hex(src.Text), src.ContentHash, "ContentHash 必须等于 sha256(Text)")
}

// TestRootReader_NormalizesRelativePath 守文档行唯一性：`./docs/../docs/a.md` 与
// `docs/a.md` 必须落到同一个规范化相对路径，否则同一文件会裂成两条文档行。
func TestRootReader_NormalizesRelativePath(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/a.md", []byte("x"))

	reader := mustRootReader(t, root, 1<<20)
	// 反斜杠只在 Windows 上等同于分隔符；在 Linux 它只是普通的文件名字符，
	// 要求 `docs\a.md` 指到 `docs/a.md` 等于要求跨平台改写文件系统语义。
	// The backslash spelling is a separator only on Windows; on Linux it is a
	// literal filename byte, so the assertion is scoped to the platform it describes.
	spellings := []string{"docs/a.md", "./docs/a.md", "docs/../docs/a.md"}
	if runtime.GOOS == "windows" {
		spellings = append(spellings, `docs\a.md`)
	}
	for _, spelling := range spellings {
		src, err := reader.Load(t.Context(), spelling)
		require.NoErrorf(t, err, "写法 %q 应当可用", spelling)
		require.Equalf(t, "docs/a.md", src.Path, "写法 %q 必须规范化", spelling)
	}
}

// TestRootReader_RejectsNonLocalPaths 守词法闸门：空路径、绝对路径、`..` 越界、
// Windows 盘符 / UNC 一律在打开文件之前拒绝。
func TestRootReader_RejectsNonLocalPaths(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/a.md", []byte("x"))
	reader := mustRootReader(t, root, 1<<20)

	cases := map[string]string{
		"空路径":  "   ",
		"越界":   ".." + string(filepath.Separator) + "outside.md",
		"根自身":  ".",
		"绝对路径": filepath.Join(root, "docs", "a.md"),
	}
	if runtime.GOOS == "windows" {
		cases["盘符"] = `C:\Windows\win.ini`
		cases["UNC"] = `\\server\share\a.md`
		cases["根路径"] = `\docs\a.md`
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := reader.Load(t.Context(), path)
			require.ErrorIs(t, err, ErrPathRejected)
		})
	}
}

// TestRootReader_RejectsMissingAndNonFile 守"读不到就拒绝"：不存在的文件与目录
// 都不是可摄取的文档，错误类别同为准入拒绝（契约 400）。
func TestRootReader_RejectsMissingAndNonFile(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/a.md", []byte("x"))
	reader := mustRootReader(t, root, 1<<20)

	_, err := reader.Load(t.Context(), "docs/missing.md")
	require.ErrorIs(t, err, ErrPathRejected)

	_, err = reader.Load(t.Context(), "docs")
	require.ErrorIs(t, err, ErrPathRejected)
}

// TestRootReader_RejectsInRootSymlink 守"拒绝符号链接"本身：链接指向根**内**也必须
// 拒绝——设计的前置写的是拒绝链接，而不是只拒绝逃逸。
// 逆改点：删掉 rejectLinks → 本用例变绿（错误方向），用例红。
func TestRootReader_RejectsInRootSymlink(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/a.md", []byte("x"))
	link := filepath.Join(root, "docs", "link.md")
	if err := os.Symlink(filepath.Join(root, "docs", "a.md"), link); err != nil {
		t.Skipf("本机无法创建符号链接（%v）——需要启用开发者模式或管理员权限", err)
	}

	_, err := mustRootReader(t, root, 1<<20).Load(t.Context(), "docs/link.md")
	require.ErrorIs(t, err, ErrPathRejected)
}

// TestRootReader_RejectsLinkEscape 守真实路径投影：链接指向根外时，前缀比较必须
// 打在投影结果上。即使链接被 Lstat 拦住，本用例仍要求"逃逸"这一判定独立成立——
// 投影失败 / 根外都返回同一个准入拒绝。
// 逆改点：把真实路径投影换成词法前缀比较 → 本用例变绿（错误的绿）。
func TestRootReader_RejectsLinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.md"), []byte("secret"), 0o644))
	link := filepath.Join(root, "docs", "escape.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	if err := os.Symlink(filepath.Join(outside, "secret.md"), link); err != nil {
		t.Skipf("本机无法创建符号链接（%v）——需要启用开发者模式或管理员权限", err)
	}

	_, err := mustRootReader(t, root, 1<<20).Load(t.Context(), "docs/escape.md")
	require.ErrorIs(t, err, ErrPathRejected)
}

// TestRootReader_EnforcesSizeCap 守大小上限：恰好等于上限可读，超出 1 字节即拒绝，
// 且拒绝文案点明上限（防止实现悄悄改成"截断后继续索引"）。
func TestRootReader_EnforcesSizeCap(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/at.md", []byte("12345678"))
	writeWorkspaceFile(t, root, "docs/over.md", []byte("123456789"))

	reader := mustRootReader(t, root, 8)
	_, err := reader.Load(t.Context(), "docs/at.md")
	require.NoError(t, err)

	_, err = reader.Load(t.Context(), "docs/over.md")
	require.ErrorIs(t, err, ErrPathRejected)
	require.Contains(t, err.Error(), "上限")
}

// TestRootReader_RejectsInvalidUTF8 守"UTF-8 解码成功"这一前置：非法字节必须拒绝，
// 不得以替换字符兜底（契约把解码失败映射为 400，零写入）。
func TestRootReader_RejectsInvalidUTF8(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "docs/bad.md", []byte{0xff, 0xfe, 'a'})

	_, err := mustRootReader(t, root, 1<<20).Load(t.Context(), "docs/bad.md")
	require.ErrorIs(t, err, ErrPathRejected)
	require.Contains(t, err.Error(), "UTF-8")
}

// TestNewRootReader_RejectsBadAssembly 守装配期失败：根不存在或上限非正时构造即
// 报错，不得拖到第一次摄取才失败。
func TestNewRootReader_RejectsBadAssembly(t *testing.T) {
	_, err := NewRootReader(filepath.Join(t.TempDir(), "missing"), 1024)
	require.ErrorIs(t, err, ErrPathRejected)

	_, err = NewRootReader(t.TempDir(), 0)
	require.ErrorIs(t, err, ErrPathRejected)
}
