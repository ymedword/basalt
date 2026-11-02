package releasekit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUS17_SBOM_CoversFrontendAndRuntime(t *testing.T) {
	repo := t.TempDir()
	writeTestFileAt(t, filepath.Join(repo, "go.mod"),
		"module example.com/basalt\n\ngo 1.27\n\nrequire github.com/example/lib v1.2.3\n")
	writeTestFileAt(t, filepath.Join(repo, "assets", "console", "package.json"),
		`{"name":"basalt-console","version":"0.4.0"}`)
	writeTestFileAt(t, filepath.Join(repo, "release", "runtime-manifest.json"),
		`{"components":[{"id":"python","version":"3.12.0","license":"PSF","source":"https://python.org"}]}`)
	in := Inputs{Repo: repo, Version: "1.2.3"}
	pkg := PackageResult{Target: Target{GOOS: "linux", GOARCH: "amd64", Tier: TierFull}}
	dir := t.TempDir()
	if err := BuildSBOM(in, []PackageResult{pkg}, dir); err != nil {
		t.Fatalf("生成 SBOM：%v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "basalt-1.2.3-linux-amd64.sbom.cdx.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document sbomDocument
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"github.com/example/lib", "basalt-console", `"python"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("SBOM 缺少 %q：%s", want, text)
		}
	}
	if len(document.Gaps) != 0 {
		t.Fatalf("输入齐全时不应有缺口：%v", document.Gaps)
	}
	if document.BOMFormat != "CycloneDX" || document.SpecVersion != "1.5" {
		t.Fatalf("SBOM 格式不符：%+v", document)
	}
}

// TestUS17_SBOM_BinaryTierExcludesRuntime：binary 档归档不带 runtime/，SBOM 也不得
// 列出归档里不存在的 runtime 组件。
func TestUS17_SBOM_BinaryTierExcludesRuntime(t *testing.T) {
	repo := t.TempDir()
	writeTestFileAt(t, filepath.Join(repo, "go.mod"), "module example.com/basalt\n\ngo 1.27\n")
	writeTestFileAt(t, filepath.Join(repo, "release", "runtime-manifest.json"),
		`{"components":[{"id":"python","version":"3.12.0","license":"PSF","source":"https://python.org"}]}`)
	in := Inputs{Repo: repo, Version: "1.2.3"}
	pkg := PackageResult{Target: Target{GOOS: "linux", GOARCH: "arm64", Tier: TierBinary}}
	dir := t.TempDir()
	if err := BuildSBOM(in, []PackageResult{pkg}, dir); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "basalt-1.2.3-linux-arm64.sbom.cdx.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"python"`) {
		t.Fatalf("binary 档 SBOM 不得列出 runtime 组件：%s", body)
	}
}

func TestUS17_SBOM_Deterministic(t *testing.T) {
	repo := t.TempDir()
	writeTestFileAt(t, filepath.Join(repo, "go.mod"), "module example.com/basalt\n\ngo 1.27\n")
	in := Inputs{Repo: repo, Version: "1.2.3"}
	pkg := PackageResult{Target: Target{GOOS: "linux", GOARCH: "arm64", Tier: TierBinary}}
	first, second := t.TempDir(), t.TempDir()
	if err := BuildSBOM(in, []PackageResult{pkg}, first); err != nil {
		t.Fatal(err)
	}
	if err := BuildSBOM(in, []PackageResult{pkg}, second); err != nil {
		t.Fatal(err)
	}
	bodyFirst, err := os.ReadFile(filepath.Join(first, "basalt-1.2.3-linux-arm64.sbom.cdx.json"))
	if err != nil {
		t.Fatal(err)
	}
	bodySecond, err := os.ReadFile(filepath.Join(second, "basalt-1.2.3-linux-arm64.sbom.cdx.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(bodyFirst) != string(bodySecond) {
		t.Fatal("同输入两次 SBOM 必须逐字节一致")
	}
}
