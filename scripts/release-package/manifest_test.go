package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建 %s 的父目录失败: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 %s 失败: %v", path, err)
	}
}

const testManifest = `{
  "schema_version": 1,
  "components": [
    {
      "id": "tool",
      "kind": "executable",
      "version": "1.0.0",
      "license": "MIT",
      "source": "https://example.invalid/tool",
      "paths": {"windows": "tool/tool.exe", "linux": "tool/tool"},
      "repo_licenses": ["LICENSE"]
    }
  ]
}`

func TestLoadManifest_Valid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	writeTestFile(t, path, testManifest)
	loaded, err := loadManifest(path, "windows")
	if err != nil {
		t.Fatalf("读取合法清单失败: %v", err)
	}
	if len(loaded.Components) != 1 || loaded.Components[0].ID != "tool" {
		t.Fatalf("清单内容不符合预期: %+v", loaded)
	}
}

func TestLoadManifest_RejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	writeTestFile(t, path, strings.Replace(testManifest, `"license": "MIT",`, `"license": "MIT", "licences": [],`, 1))
	if _, err := loadManifest(path, "windows"); err == nil {
		t.Fatal("未知字段应被拒绝（清单拼写错误不能静默忽略）")
	}
}

func TestLoadManifest_RejectsDuplicateID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	duplicate := strings.Replace(testManifest, `"components": [`, `"components": [
    {
      "id": "tool",
      "kind": "executable",
      "version": "2.0.0",
      "license": "MIT",
      "source": "https://example.invalid/tool2",
      "paths": {"windows": "tool2/tool2.exe", "linux": "tool2/tool2"},
      "repo_licenses": ["LICENSE"]
    },`, 1)
	writeTestFile(t, path, duplicate)
	if _, err := loadManifest(path, "windows"); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 id 应被拒绝，实际 %v", err)
	}
}

func TestLoadManifest_RejectsMissingPlatform(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	writeTestFile(t, path, strings.Replace(testManifest, `"linux": "tool/tool"`, `"plan9": "tool/tool"`, 1))
	if _, err := loadManifest(path, "linux"); err == nil || !strings.Contains(err.Error(), "linux") {
		t.Fatalf("缺少当前平台路径应被拒绝，实际 %v", err)
	}
}

func TestLoadManifest_RejectsTrailingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	writeTestFile(t, path, testManifest+"\n{\n}\n")
	if _, err := loadManifest(path, "windows"); err == nil || !strings.Contains(err.Error(), "多余内容") {
		t.Fatalf("多余内容应被拒绝，实际 %v", err)
	}
}

func TestVerifyComponents_ChecksEntryAndMetadata(t *testing.T) {
	runtimeRoot := t.TempDir()
	writeTestFile(t, filepath.Join(runtimeRoot, "app", "app.exe"), "binary")
	writeTestFile(t, filepath.Join(runtimeRoot, "pkg", "index.js"), "module")
	writeTestFile(t, filepath.Join(runtimeRoot, "pkg", "package.json"),
		`{"name":"pkg","version":"2.3.4","license":"MIT"}`)
	writeTestFile(t, filepath.Join(runtimeRoot, "site", "pkg-2.3.4.dist-info", "METADATA"),
		"Metadata-Version: 2.1\nName: pkg\nVersion: 2.3.4\nLicense: MIT\n\nbody\n")

	loaded := manifest{SchemaVersion: 1, Components: []component{
		{
			ID: "app", Kind: kindExecutable, Version: "1.0.0", License: "MIT", Source: "s",
			Paths: map[string]string{"windows": "app/app.exe"}, RepoLicenses: []string{"LICENSE"},
		},
		{
			ID: "pkg", Kind: kindNodePackage, Version: "2.3.4", License: "MIT", Source: "s",
			Paths: map[string]string{"windows": "pkg/index.js"}, PackageJSON: "pkg/package.json",
		},
		{
			ID: "py", Kind: kindPythonPackage, Version: "2.3.4", License: "MIT", Source: "s",
			PackageName: "pkg", Paths: map[string]string{"windows": "site/pkg-2.3.4.dist-info/METADATA"},
			Scan: &scanSpec{Kind: scanKindDistInfo, Root: "site"},
		},
	}}
	reports, err := verifyComponents(loaded, runtimeRoot, "windows")
	if err != nil {
		t.Fatalf("核对组件失败: %v", err)
	}
	if len(reports) != 3 || reports[0].File != "runtime/app/app.exe" {
		t.Fatalf("核对读数不符合预期: %+v", reports)
	}
}

func TestVerifyComponents_RejectsVersionMismatch(t *testing.T) {
	runtimeRoot := t.TempDir()
	writeTestFile(t, filepath.Join(runtimeRoot, "pkg", "index.js"), "module")
	writeTestFile(t, filepath.Join(runtimeRoot, "pkg", "package.json"),
		`{"name":"pkg","version":"2.3.4"}`)
	loaded := manifest{SchemaVersion: 1, Components: []component{{
		ID: "pkg", Kind: kindNodePackage, Version: "9.9.9", License: "MIT", Source: "s",
		Paths: map[string]string{"windows": "pkg/index.js"}, PackageJSON: "pkg/package.json",
	}}}
	_, err := verifyComponents(loaded, runtimeRoot, "windows")
	if err == nil || !strings.Contains(err.Error(), "9.9.9") {
		t.Fatalf("版本不一致应被拒绝，实际 %v", err)
	}
}

func TestVerifyComponents_RejectsMissingEntry(t *testing.T) {
	loaded := manifest{SchemaVersion: 1, Components: []component{{
		ID: "app", Kind: kindExecutable, Version: "1.0.0", License: "MIT", Source: "s",
		Paths: map[string]string{"windows": "app/app.exe"},
	}}}
	if _, err := verifyComponents(loaded, t.TempDir(), "windows"); err == nil ||
		!strings.Contains(err.Error(), "缺少入口") {
		t.Fatalf("缺少入口应被拒绝，实际 %v", err)
	}
}

func TestReadDistInfoMetadata(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pkg-1.0.dist-info")
	writeTestFile(t, filepath.Join(dir, "METADATA"),
		"Metadata-Version: 2.1\nName: demo\nVersion: 1.2.3\nLicense-Expression: Apache-2.0\n\nsummary\n")
	meta, err := readDistInfoMetadata(dir)
	if err != nil {
		t.Fatalf("读取 METADATA 失败: %v", err)
	}
	if meta.Name != "demo" || meta.Version != "1.2.3" || meta.License != "Apache-2.0" {
		t.Fatalf("元数据读数不符合预期: %+v", meta)
	}
}
