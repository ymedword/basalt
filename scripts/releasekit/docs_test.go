package releasekit

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestUS17_Docs_CommandsMatchCLI(t *testing.T) {
	known := map[string]bool{
		"preflight": true, "build": true, "package": true, "release": true, "verify-package": true,
		"assemble": true, "verify": true, "repro": true, "smoke": true,
		"upgrade": true, "sbom": true, "provenance": true, "record": true, "sign": true,
	}
	pattern := regexp.MustCompile(`releasekit ([a-z][a-z-]*)`)
	root := filepath.Join("..", "..")
	for _, name := range []string{"docs/Operations.md", "docs/us/us17-release-artifacts.md"} {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			// docs/us/ 是私有资产，干净检出里不存在；公开的 Operations.md 缺了才算失败。
			if os.IsNotExist(err) && name != "docs/Operations.md" {
				t.Logf("跳过私有文档 %s：干净检出不带它", name)
				continue
			}
			t.Fatalf("读取 %s：%v", name, err)
		}
		matches := pattern.FindAllStringSubmatch(string(body), -1)
		if name == "docs/Operations.md" && len(matches) == 0 {
			t.Fatalf("%s 未记录 releasekit 命令", name)
		}
		for _, match := range matches {
			if !known[match[1]] {
				t.Fatalf("%s 记录了 CLI 不存在的子命令 %q", name, match[1])
			}
		}
	}
	operations, err := os.ReadFile(filepath.Join(root, "docs", "Operations.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"basalt-verify", "privatebackup"} {
		if !regexp.MustCompile(want).Match(operations) {
			t.Fatalf("docs/Operations.md 未记录 %s", want)
		}
	}
}
