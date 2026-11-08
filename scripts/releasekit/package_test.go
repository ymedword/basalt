package releasekit

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUS17_Package_Deterministic(t *testing.T) {
	repo := t.TempDir()
	binary := writeTestFile(t, "basalt.exe", "fake-binary")
	build := testBuild(binary, Target{GOOS: "windows", GOARCH: "amd64", Tier: TierBinary})
	in := Inputs{Repo: repo, Version: "1.2.3"}

	first, err := PackageTarget(context.Background(), in, build.Target, build,
		ReleaseOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("第一次打包：%v", err)
	}
	second, err := PackageTarget(context.Background(), in, build.Target, build,
		ReleaseOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("第二次打包：%v", err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatalf("同输入两次打包哈希不同：%s / %s", first.SHA256, second.SHA256)
	}
	if first.FileCount != second.FileCount {
		t.Fatalf("文件数不同：%d / %d", first.FileCount, second.FileCount)
	}
}

func TestUS17_Package_Manifest(t *testing.T) {
	repo := t.TempDir()
	binary := writeTestFile(t, "basalt.exe", "fake-binary")
	build := testBuild(binary, Target{GOOS: "windows", GOARCH: "amd64", Tier: TierBinary})
	pkg, err := PackageTarget(context.Background(), Inputs{Repo: repo, Version: "1.2.3"},
		build.Target, build, ReleaseOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("打包：%v", err)
	}
	entries := readZipEntries(t, pkg.Archive)
	raw, ok := entries["basalt-1.2.3/MANIFEST.json"]
	if !ok {
		t.Fatalf("归档缺少 MANIFEST.json，实际条目：%v", keys(entries))
	}
	var manifest packageManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("解析 MANIFEST.json：%v", err)
	}
	if manifest.Version != "1.2.3" || manifest.Target != build.Target {
		t.Fatalf("清单版本/目标不符：%+v", manifest)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("清单没有文件条目")
	}
	for _, file := range manifest.Files {
		body, ok := entries["basalt-1.2.3/"+file.Path]
		if !ok {
			t.Fatalf("清单列出 %s，归档中没有", file.Path)
		}
		if int64(len(body)) != file.Size {
			t.Fatalf("%s 尺寸不符：清单 %d，实际 %d", file.Path, file.Size, len(body))
		}
		sum := sha256Hex(body)
		if sum != file.SHA256 {
			t.Fatalf("%s 哈希不符：清单 %s，实际 %s", file.Path, file.SHA256, sum)
		}
		if file.Mode == "" {
			t.Fatalf("%s 缺少模式", file.Path)
		}
	}
}

func TestUS17_Package_BinaryTierExcludesRuntime(t *testing.T) {
	repo := t.TempDir()
	binary := writeTestFile(t, "basalt.exe", "fake-binary")
	runtimeDir := t.TempDir()
	writeTestFileAt(t, filepath.Join(runtimeDir, "python.exe"), "runtime")
	build := testBuild(binary, Target{GOOS: "windows", GOARCH: "amd64", Tier: TierBinary})
	pkg, err := PackageTarget(context.Background(), Inputs{Repo: repo, Version: "1.2.3"},
		build.Target, build, ReleaseOptions{OutDir: t.TempDir(), RuntimeDir: runtimeDir})
	if err != nil {
		t.Fatalf("打包：%v", err)
	}
	for name := range readZipEntries(t, pkg.Archive) {
		if strings.Contains(name, "runtime/") {
			t.Fatalf("binary 档不应包含 runtime/：%s", name)
		}
	}
}

func TestUS17_Package_NoSecretsOrPrivatePaths(t *testing.T) {
	t.Run("私有路径", func(t *testing.T) {
		repo := t.TempDir()
		binary := writeTestFile(t, "basalt.exe", "fake-binary")
		runtimeDir := t.TempDir()
		writeRuntimeManifest(t, repo, "windows", "fetch/run_fetch.py")
		writeTestFileAt(t, filepath.Join(runtimeDir, "fetch", "run_fetch.py"), "PY")
		writeTestFileAt(t, filepath.Join(runtimeDir, "career", "plan.md"), "PRIVATE")
		build := testBuild(binary, Target{GOOS: "windows", GOARCH: "amd64", Tier: TierFull})
		_, err := PackageTarget(context.Background(), Inputs{Repo: repo, Version: "1.2.3"},
			build.Target, build, ReleaseOptions{OutDir: t.TempDir(), RuntimeDir: runtimeDir})
		if err == nil || !strings.Contains(err.Error(), "拒绝出包") {
			t.Fatalf("私有路径必须拒绝，实际错误：%v", err)
		}
	})
	t.Run("真实凭据", func(t *testing.T) {
		repo := t.TempDir()
		binary := writeTestFile(t, "basalt.exe",
			"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQ\n-----END PRIVATE KEY-----\n")
		build := testBuild(binary, Target{GOOS: "windows", GOARCH: "amd64", Tier: TierBinary})
		_, err := PackageTarget(context.Background(), Inputs{Repo: repo, Version: "1.2.3"},
			build.Target, build, ReleaseOptions{OutDir: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "凭据") {
			t.Fatalf("完整 PEM 私钥块必须拒绝，实际错误：%v", err)
		}
	})
	// 上游运行时（oracledb / ssh2 的 keyParser）把 PEM 头当解析常量写进源码，
	// 只有头部不是密钥；按头部判卷会把随包依赖整批拒掉（2026-11-09 首发演练实测）。
	t.Run("PEM 解析常量", func(t *testing.T) {
		repo := t.TempDir()
		binary := writeTestFile(t, "basalt.exe",
			"const pemHeaders = ['-----BEGIN PRIVATE KEY-----', '-----END PRIVATE KEY-----'];\n")
		build := testBuild(binary, Target{GOOS: "windows", GOARCH: "amd64", Tier: TierBinary})
		if _, err := PackageTarget(context.Background(), Inputs{Repo: repo, Version: "1.2.3"},
			build.Target, build, ReleaseOptions{OutDir: t.TempDir()}); err != nil {
			t.Fatalf("PEM 头部常量不得被误判为凭据：%v", err)
		}
	})
}

// TestFindSecret_Precision 固化判卷口径：完整私钥块与令牌格式命中，解析常量不命中。
func TestFindSecret_Precision(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"完整 RSA 块", "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("MIIE", 16) +
			"\n-----END RSA PRIVATE KEY-----", "PEM 私钥"},
		{"仅头部常量", "const header = '-----BEGIN PRIVATE KEY-----';", ""},
		{"头尾分离的拼接字面量", "['-----BEGIN PRIVATE KEY-----', '-----END PRIVATE KEY-----']", ""},
		{"GitHub token", "token=ghp_" + strings.Repeat("a", 36), "GitHub token"},
		{"AWS access key", "key=AKIAIOSFODNN7EXAMPLE", "AWS access key"},
		{"OpenAI token", "sk-" + strings.Repeat("b", 32), "OpenAI token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findSecret(tc.text); got != tc.want {
				t.Fatalf("findSecret=%q，want %q", got, tc.want)
			}
		})
	}
}

func TestUS17_Package_AllowsEnvMention(t *testing.T) {
	repo := t.TempDir()
	binary := writeTestFile(t, "basalt.exe", "fake-binary")
	runtimeDir := t.TempDir()
	writeRuntimeManifest(t, repo, "windows", "fetch/run_fetch.py")
	writeTestFileAt(t, filepath.Join(runtimeDir, "fetch", "run_fetch.py"), "PY")
	writeTestFileAt(t, filepath.Join(runtimeDir, "config", "app.env"),
		"BASALT_API_KEY=${BASALT_API_KEY}\nOPENAI_BASE_URL=${OPENAI_BASE_URL}\n")
	build := testBuild(binary, Target{GOOS: "windows", GOARCH: "amd64", Tier: TierFull})
	if _, err := PackageTarget(context.Background(), Inputs{Repo: repo, Version: "1.2.3"},
		build.Target, build, ReleaseOptions{OutDir: t.TempDir(), RuntimeDir: runtimeDir}); err != nil {
		t.Fatalf("普通环境变量占位不得被误判为凭据：%v", err)
	}
}

// TestUS17_Package_FullTierRefusesIncompleteRuntime 守「半成品不得标成 full」：
// 清单缺失或任一组件缺失都必须拒绝出包，并提示 binary 档；binary 档不受影响。
func TestUS17_Package_FullTierRefusesIncompleteRuntime(t *testing.T) {
	target := Target{GOOS: "windows", GOARCH: "amd64", Tier: TierFull}

	t.Run("缺清单", func(t *testing.T) {
		repo := t.TempDir()
		binary := writeTestFile(t, "basalt.exe", "fake-binary")
		build := testBuild(binary, target)
		_, err := PackageTarget(context.Background(), Inputs{Repo: repo, Version: "1.2.3"},
			target, build, ReleaseOptions{OutDir: t.TempDir(), RuntimeDir: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "runtime 清单") {
			t.Fatalf("缺 runtime 清单必须拒绝，实际错误：%v", err)
		}
	})

	t.Run("缺组件", func(t *testing.T) {
		repo := t.TempDir()
		binary := writeTestFile(t, "basalt.exe", "fake-binary")
		runtimeDir := t.TempDir()
		writeRuntimeManifest(t, repo, "windows", "python/python.exe", "node/node.exe")
		writeTestFileAt(t, filepath.Join(runtimeDir, "python", "python.exe"), "PY")
		build := testBuild(binary, target)
		_, err := PackageTarget(context.Background(), Inputs{Repo: repo, Version: "1.2.3"},
			target, build, ReleaseOptions{OutDir: t.TempDir(), RuntimeDir: runtimeDir})
		if err == nil || !strings.Contains(err.Error(), "node") {
			t.Fatalf("缺 node 组件必须拒绝并指名，实际错误：%v", err)
		}
		if !strings.Contains(err.Error(), "--tier binary") {
			t.Fatalf("拒绝文案应给出 binary 档出路，实际错误：%v", err)
		}
	})

	t.Run("组件齐全", func(t *testing.T) {
		repo := t.TempDir()
		binary := writeTestFile(t, "basalt.exe", "fake-binary")
		runtimeDir := t.TempDir()
		writeRuntimeManifest(t, repo, "windows", "python/python.exe", "node/node.exe")
		writeTestFileAt(t, filepath.Join(runtimeDir, "python", "python.exe"), "PY")
		writeTestFileAt(t, filepath.Join(runtimeDir, "node", "node.exe"), "NODE")
		build := testBuild(binary, target)
		if _, err := PackageTarget(context.Background(), Inputs{Repo: repo, Version: "1.2.3"},
			target, build, ReleaseOptions{OutDir: t.TempDir(), RuntimeDir: runtimeDir}); err != nil {
			t.Fatalf("组件齐全时不应拒绝：%v", err)
		}
	})
}

func writeRuntimeManifest(t *testing.T, repo, goos string, components ...string) {
	t.Helper()
	type component struct {
		ID    string            `json:"id"`
		Paths map[string]string `json:"paths"`
	}
	var manifest struct {
		Schema     int         `json:"schema_version"`
		Components []component `json:"components"`
	}
	manifest.Schema = 1
	for _, name := range components {
		manifest.Components = append(manifest.Components, component{
			ID: name, Paths: map[string]string{goos: name},
		})
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("序列化 runtime 清单：%v", err)
	}
	writeTestFileAt(t, filepath.Join(repo, "release", "runtime-manifest.json"), string(body))
}

func TestUS17_Package_CanonicalMode(t *testing.T) {
	cases := []struct {
		in   os.FileMode
		want os.FileMode
	}{
		{0o644, 0o644},
		{0o640, 0o644},
		{0o666, 0o644},
		{0o755, 0o755},
		{0o700, 0o755},
		{0o775, 0o755},
	}
	for _, c := range cases {
		if got := canonicalMode(c.in); got != c.want {
			t.Fatalf("canonicalMode(%04o) = %04o，期望 %04o", c.in, got, c.want)
		}
	}
}

// TestUS17_Package_UnixArchiveKeepsExecBit 守 F-01：Linux/macOS 归档必须保留可执行位，
// 否则解包后的 basalt 无法直接运行。NTFS 不表达 Unix 权限位，Windows 宿主跳过。
func TestUS17_Package_UnixArchiveKeepsExecBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("NTFS 不保留 Unix 可执行位，该断言在 Linux/macOS 上执行")
	}
	root := t.TempDir()
	programDir := filepath.Join(root, "basalt-1.2.3")
	if err := os.MkdirAll(programDir, 0o755); err != nil {
		t.Fatalf("建目录：%v", err)
	}
	if err := os.WriteFile(filepath.Join(programDir, "basalt"), []byte("fake"), 0o755); err != nil {
		t.Fatalf("写二进制：%v", err)
	}
	if err := os.WriteFile(filepath.Join(programDir, "VERIFY.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写说明：%v", err)
	}
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := writeTarGzTree(root, archive); err != nil {
		t.Fatalf("打包：%v", err)
	}
	file, err := os.Open(archive)
	if err != nil {
		t.Fatalf("打开归档：%v", err)
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("解压：%v", err)
	}
	defer func() { _ = gz.Close() }()
	modes := map[string]int64{}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("读归档：%v", err)
		}
		modes[header.Name] = header.Mode
	}
	if modes["basalt-1.2.3/basalt"] != 0o755 {
		t.Fatalf("二进制在归档内权限 %04o，期望 0755", modes["basalt-1.2.3/basalt"])
	}
	if modes["basalt-1.2.3/VERIFY.txt"] != 0o644 {
		t.Fatalf("说明文件在归档内权限 %04o，期望 0644", modes["basalt-1.2.3/VERIFY.txt"])
	}
}

func testBuild(binary string, target Target) BuildResult {
	sum, _ := FileSHA256(binary)
	return BuildResult{Target: target, Version: "1.2.3", Binary: binary, BinarySHA: sum}
}

func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeTestFileAt(t, path, content)
	return path
}

func writeTestFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录：%v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件 %s：%v", path, err)
	}
}

func readZipEntries(t *testing.T, archive string) map[string][]byte {
	t.Helper()
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatalf("打开 zip：%v", err)
	}
	defer func() { _ = reader.Close() }()
	entries := map[string][]byte{}
	for _, file := range reader.File {
		body, err := readZipFile(file)
		if err != nil {
			t.Fatalf("读取 %s：%v", file.Name, err)
		}
		entries[file.Name] = body
	}
	return entries
}

func readZipFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

func keys(values map[string][]byte) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	return out
}
