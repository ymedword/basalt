package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPublicDocs_UseAgentFlag 保护公开命令、归档语义与私有文档链接边界。
// TestPublicDocs_UseAgentFlag protects public commands and documentation boundaries.
func TestPublicDocs_UseAgentFlag(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate public documentation test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	read := func(t *testing.T, name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(data)
	}
	t.Run("public", func(t *testing.T) {
		const name = "README.md"
		text := read(t, name)
		if strings.Contains(text, "--profile") {
			t.Fatalf("%s still documents --profile", name)
		}
		if !strings.Contains(text, "basalt chat --agent") {
			t.Fatalf("%s does not document basalt chat --agent", name)
		}
	})
	t.Run("requirements-local", func(t *testing.T) {
		name := filepath.Join("docs", "Requirements.md")
		if _, err := os.Stat(filepath.Join(root, name)); errors.Is(err, os.ErrNotExist) {
			t.Skip("docs/Requirements.md is local-private; absent in clean checkouts, run locally for drift protection")
		} else if err != nil {
			t.Fatalf("stat Requirements: %v", err)
		}
		text := read(t, name)
		_, after, found := strings.Cut(text, "### 5.8 命令入口清单")
		if !found {
			t.Fatal("Requirements command section is missing")
		}
		block, _, found := strings.Cut(after, "> 版本查询用")
		if !found {
			t.Fatal("Requirements command section boundary is missing")
		}
		if strings.Contains(block, "--profile") || !strings.Contains(block, "basalt chat --agent <name>") {
			t.Fatal("Requirements command section must document --agent")
		}
		if !strings.Contains(block, "归档") {
			t.Fatal("Requirements command section must describe profile delete as archive")
		}
	})
	t.Run("operations-links", func(t *testing.T) {
		text := read(t, filepath.Join("docs", "Operations.md"))
		for _, name := range []string{"Compatibility.md", "IndustrySurvey.md", "Requirements.md", "TechnicalDesign.md"} {
			for _, prefix := range []string{"](" + name, "](./" + name} {
				if strings.Contains(text, prefix) {
					t.Fatalf("docs/Operations.md still links to private document %s", name)
				}
			}
		}
	})
}
