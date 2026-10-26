package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyTree_PreservesFilesAndDirs(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeTestFile(t, filepath.Join(src, "top.txt"), "top")
	writeTestFile(t, filepath.Join(src, "nested", "inner.txt"), "inner")

	if err := copyTree(src, filepath.Join(dst, "copied")); err != nil {
		t.Fatalf("复制文件树失败: %v", err)
	}
	for _, rel := range []string{"top.txt", filepath.Join("nested", "inner.txt")} {
		raw, err := os.ReadFile(filepath.Join(dst, "copied", rel)) // #nosec G304 -- 测试内路径
		if err != nil {
			t.Fatalf("复制后读取 %s 失败: %v", rel, err)
		}
		want := "top"
		if strings.Contains(rel, "inner") {
			want = "inner"
		}
		if string(raw) != want {
			t.Fatalf("%s 内容 = %q，期望 %q", rel, raw, want)
		}
	}
}

func TestCopyTree_PreservesSymlink(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeTestFile(t, filepath.Join(src, "bin", "real-node"), "binary")
	if err := os.Symlink("real-node", filepath.Join(src, "bin", "node")); err != nil {
		t.Skipf("当前环境不允许创建符号链接（%v），按平台限制跳过", err)
	}

	if err := copyTree(src, filepath.Join(dst, "copied")); err != nil {
		t.Fatalf("复制文件树失败: %v", err)
	}
	linkPath := filepath.Join(dst, "copied", "bin", "node")
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("读取复制后的链接失败: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("复制后应保持符号链接，而不是展开成普通文件")
	}
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("读取链接目标失败: %v", err)
	}
	if target != "real-node" {
		t.Fatalf("链接目标 = %q，期望 real-node", target)
	}
}

func TestZipTree_StoresSymlinkTargetAndFixedTime(t *testing.T) {
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "payload.txt"), "hello")
	hasLink := os.Symlink("payload.txt", filepath.Join(src, "link")) == nil

	archivePath := filepath.Join(t.TempDir(), "out.zip")
	stats, err := zipTree(src, archivePath)
	if err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	wantFiles := 1
	if hasLink {
		wantFiles = 2
	}
	if stats.files != wantFiles || stats.bytes != int64(len("hello")) {
		t.Fatalf("压缩读数 = %+v，期望 files=%d bytes=5", stats, wantFiles)
	}

	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatalf("打开压缩包失败: %v", err)
	}
	defer func() { _ = reader.Close() }()
	entries := make(map[string]*zip.File, len(reader.File))
	for _, file := range reader.File {
		entries[file.Name] = file
		if !file.Modified.Equal(zipFixedTime) {
			t.Fatalf("%s 的时间戳 = %v，期望固定 %v", file.Name, file.Modified, zipFixedTime)
		}
	}
	if _, ok := entries["payload.txt"]; !ok {
		t.Fatal("压缩包缺少 payload.txt")
	}
	if hasLink {
		link, ok := entries["link"]
		if !ok {
			t.Fatal("压缩包缺少 link")
		}
		if link.Mode()&os.ModeSymlink == 0 {
			t.Fatal("link 条目应保留符号链接模式")
		}
		reader, err := link.Open()
		if err != nil {
			t.Fatalf("读取 link 条目失败: %v", err)
		}
		buf := make([]byte, 64)
		n, _ := reader.Read(buf)
		_ = reader.Close()
		if string(buf[:n]) != "payload.txt" {
			t.Fatalf("link 条目内容 = %q，期望链接目标字符串 payload.txt", buf[:n])
		}
	}
}

func TestSha256File_MatchesKnownDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "digest.txt")
	writeTestFile(t, path, "abc")
	sum, err := sha256File(path)
	if err != nil {
		t.Fatalf("计算摘要失败: %v", err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if sum != want {
		t.Fatalf("摘要 = %s，期望 %s", sum, want)
	}
}

func TestWriteReadme_RendersPlaceholders(t *testing.T) {
	templatePath := filepath.Join(t.TempDir(), "README.tmpl")
	writeTestFile(t, templatePath, "Basalt {{VERSION}} for {{PLATFORM}}\n")
	programDir := t.TempDir()

	if err := writeReadme(templatePath, programDir, "0.1.0", platformLabel("windows", "amd64")); err != nil {
		t.Fatalf("渲染 README 失败: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(programDir, "README.txt")) // #nosec G304 -- 测试内路径
	if err != nil {
		t.Fatalf("读取渲染结果失败: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "0.1.0") || !strings.Contains(text, "Windows amd64") {
		t.Fatalf("渲染结果缺少版本或平台: %q", text)
	}
	if strings.Contains(text, "{{") {
		t.Fatalf("渲染结果仍有未替换占位符: %q", text)
	}
}
