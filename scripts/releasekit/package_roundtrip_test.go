package releasekit

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRoundTripTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	program := filepath.Join(root, "basalt-1.2.3")
	if err := os.MkdirAll(filepath.Join(program, "runtime", "fetch"), 0o755); err != nil {
		t.Fatalf("建目录：%v", err)
	}
	files := map[string]string{
		"basalt": "binary",
		filepath.Join("runtime", "fetch", "tool.txt"): "tool",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(program, name), []byte(body), 0o644); err != nil {
			t.Fatalf("写 %s：%v", name, err)
		}
	}
	return root
}

// 中文验收点：tar.gz 与 zip 都必须能原样解回，顶层目录与嵌套 runtime 不变。
// Both tar.gz and zip must round-trip, preserving the top-level directory and the
// nested runtime tree.
func TestUS17_Package_TarGzRoundTrip(t *testing.T) {
	root := writeRoundTripTree(t)
	archive := filepath.Join(t.TempDir(), "basalt-1.2.3-linux-amd64.tar.gz")
	if err := writeTarGzTree(root, archive); err != nil {
		t.Fatalf("打包 tar.gz：%v", err)
	}
	dest := t.TempDir()
	if err := extractArchive(archive, dest); err != nil {
		t.Fatalf("解包 tar.gz：%v", err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "basalt-1.2.3", "basalt"))
	if err != nil || string(body) != "binary" {
		t.Fatalf("解包后的二进制 = %q, %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "basalt-1.2.3", "runtime", "fetch", "tool.txt")); err != nil {
		t.Fatalf("嵌套 runtime 未解出：%v", err)
	}
}

func TestUS17_Package_ZipRoundTrip(t *testing.T) {
	root := writeRoundTripTree(t)
	archive := filepath.Join(t.TempDir(), "basalt-1.2.3-windows-amd64.zip")
	if err := writeZipTree(root, archive); err != nil {
		t.Fatalf("打包 zip：%v", err)
	}
	dest := t.TempDir()
	if err := extractArchive(archive, dest); err != nil {
		t.Fatalf("解包 zip：%v", err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "basalt-1.2.3", "basalt"))
	if err != nil || string(body) != "binary" {
		t.Fatalf("解包后的二进制 = %q, %v", body, err)
	}
}

// 中文验收点：归档里的 `..` 路径必须被拒绝，不得写到目标目录之外。
// Archive entries containing `..` must be rejected instead of escaping dest.
func TestUS17_Package_RejectsTraversalEntries(t *testing.T) {
	dest := t.TempDir()
	// Windows 的 IsAbs 要求带卷名，用当前平台的绝对路径形态构造用例。
	// Windows IsAbs requires a volume name, so build the absolute form per platform.
	absolute := filepath.VolumeName(dest) + string(os.PathSeparator) + "abs"
	for _, name := range []string{"../evil", absolute, ".."} {
		if _, err := secureJoin(dest, name); err == nil || !strings.Contains(err.Error(), "越界") {
			t.Fatalf("secureJoin(%q) 应报越界，实际 %v", name, err)
		}
	}
	if _, err := secureJoin(dest, "ok/file.txt"); err != nil {
		t.Fatalf("正常路径不应报错：%v", err)
	}

	tarPath := filepath.Join(t.TempDir(), "evil.tar.gz")
	tarFile, err := os.Create(tarPath)
	if err != nil {
		t.Fatalf("建 tar：%v", err)
	}
	gzWriter := gzip.NewWriter(tarFile)
	tarWriter := tar.NewWriter(gzWriter)
	body := []byte("x")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("写 tar 头：%v", err)
	}
	if _, err := tarWriter.Write(body); err != nil {
		t.Fatalf("写 tar 体：%v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("关闭 tar：%v", err)
	}
	if err := gzWriter.Close(); err != nil {
		t.Fatalf("关闭 gzip：%v", err)
	}
	if err := tarFile.Close(); err != nil {
		t.Fatalf("关闭 tar 文件：%v", err)
	}
	if err := extractArchive(tarPath, dest); err == nil || !strings.Contains(err.Error(), "越界") {
		t.Fatalf("tar 越界条目应拒绝，实际 %v", err)
	}

	zipPath := filepath.Join(t.TempDir(), "evil.zip")
	zipFile, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("建 zip：%v", err)
	}
	zipWriter := zip.NewWriter(zipFile)
	entry, err := zipWriter.CreateHeader(&zip.FileHeader{Name: "../evil.txt", Method: zip.Store})
	if err != nil {
		t.Fatalf("写 zip 头：%v", err)
	}
	if _, err := entry.Write(body); err != nil {
		t.Fatalf("写 zip 体：%v", err)
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("关闭 zip：%v", err)
	}
	if err := zipFile.Close(); err != nil {
		t.Fatalf("关闭 zip 文件：%v", err)
	}
	if err := extractArchive(zipPath, dest); err == nil || !strings.Contains(err.Error(), "越界") {
		t.Fatalf("zip 越界条目应拒绝，实际 %v", err)
	}
}

func TestUS17_Package_VerifyPackageStaticRejects(t *testing.T) {
	in := Inputs{Version: "1.2.3"}
	if err := VerifyPackageStatic(filepath.Join(t.TempDir(), "missing.zip"), in); err == nil {
		t.Fatal("不存在的归档应报错")
	}

	archiveFrom := func(t *testing.T, entries map[string]string) string {
		t.Helper()
		root := t.TempDir()
		for name, body := range entries {
			path := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("建目录：%v", err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatalf("写 %s：%v", name, err)
			}
		}
		archive := filepath.Join(t.TempDir(), "pkg.zip")
		if err := writeZipTree(root, archive); err != nil {
			t.Fatalf("打包：%v", err)
		}
		return archive
	}

	cases := []struct {
		name    string
		entries map[string]string
		want    string
	}{
		{"两个顶层目录", map[string]string{"a/x.txt": "x", "b/y.txt": "y"}, "只有一个顶层目录"},
		{"顶层名不符", map[string]string{"other-1.2.3/x.txt": "x"}, "顶层目录"},
		{"版本不符", map[string]string{
			"basalt-1.2.3/MANIFEST.json": `{"schema":1,"version":"9.9.9","target":{"goos":"windows","goarch":"amd64","tier":"binary"},"files":[]}`,
		}, "版本"},
		{"目标非法", map[string]string{
			"basalt-1.2.3/MANIFEST.json": `{"schema":1,"version":"1.2.3","target":{"goos":"plan9","goarch":"amd64","tier":"binary"},"files":[]}`,
		}, "非法"},
		{"清单缺失", map[string]string{"basalt-1.2.3/x.txt": "x"}, "MANIFEST.json"},
	}
	for _, testCase := range cases {
		err := VerifyPackageStatic(archiveFrom(t, testCase.entries), in)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: 期望错误含 %q，实际 %v", testCase.name, testCase.want, err)
		}
	}
}

// 中文验收点：包内目标与期望目标不符时必须拒绝——逐文件哈希只证明文件没被改过，
// 把 arm64 归档改名成 amd64 冒充别的平台，只有目标核对拦得住。
// The packaged target must equal the expected target; hashes alone cannot tell that a
// renamed archive belongs to another platform.
func TestUS17_Package_VerifyPackageForTargetRejectsMismatch(t *testing.T) {
	root := t.TempDir()
	program := filepath.Join(root, "basalt-1.2.3")
	if err := os.MkdirAll(program, 0o755); err != nil {
		t.Fatalf("建目录：%v", err)
	}
	manifest := `{"schema":1,"version":"1.2.3","target":{"goos":"windows","goarch":"amd64","tier":"full"},"files":[]}`
	if err := os.WriteFile(filepath.Join(program, "MANIFEST.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写清单：%v", err)
	}
	archive := filepath.Join(t.TempDir(), "basalt-1.2.3-linux-amd64.zip")
	if err := writeZipTree(root, archive); err != nil {
		t.Fatalf("打包：%v", err)
	}
	in := Inputs{Version: "1.2.3"}
	mismatch := Target{GOOS: "linux", GOARCH: "amd64", Tier: TierFull}
	if err := VerifyPackageForTarget(archive, in, mismatch); err == nil ||
		!strings.Contains(err.Error(), "期望目标") {
		t.Fatalf("目标不符应拒绝，实际 %v", err)
	}
	// 目标相符时越过该分支：本用例没有二进制，随后会在 buildinfo 处失败——只要不再是
	// 目标不一致的错误，就证明核对按期望目标判卷而不是恒失败。
	matched := Target{GOOS: "windows", GOARCH: "amd64", Tier: TierFull}
	if err := VerifyPackageForTarget(archive, in, matched); err == nil ||
		strings.Contains(err.Error(), "期望目标") {
		t.Fatalf("目标相符不应报目标不一致，实际 %v", err)
	}
}

func TestUS17_Package_LoadPackagesRequiresAllTargets(t *testing.T) {
	if _, err := LoadPackages(t.TempDir(), Inputs{Version: "1.2.3"}); err == nil ||
		!strings.Contains(err.Error(), "缺少") {
		t.Fatalf("空目录应报缺少归档，实际 %v", err)
	}
}
