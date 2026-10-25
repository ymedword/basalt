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
	// readme-examples：README 的四个公开增量区块必须存在，且 examples/ 链接指向真实文件。
	// readme-examples: the four public additions exist and the examples link resolves.
	t.Run("readme-examples", func(t *testing.T) {
		const name = "README.md"
		text := read(t, name)
		for _, heading := range []string{"## Templates & demos", "## MCP catalog", "## Run a demo", "## What is verified"} {
			if !strings.Contains(text, heading) {
				t.Fatalf("%s is missing section %q", name, heading)
			}
		}
		if !strings.Contains(text, "](examples/README.md)") {
			t.Fatalf("%s does not link examples/README.md", name)
		}
		if _, err := os.Stat(filepath.Join(root, "examples", "README.md")); err != nil {
			t.Fatalf("examples/README.md is not readable: %v", err)
		}
	})
	// readme-private-links：README 是公开面，不得链接私有正文、过程目录与本地约定文件。
	// readme-private-links: the public README must not link private documents or local-only files.
	t.Run("readme-private-links", func(t *testing.T) {
		const name = "README.md"
		text := read(t, name)
		for _, target := range []string{
			"career/", "specs/", "harness/", "docs/us/", "docs/interview/", ".specify/", ".claude/",
			"CLAUDE.md", "CLAUDE.local.md", ".gitmessage",
		} {
			for _, prefix := range []string{"](" + target, "](./" + target} {
				if strings.Contains(text, prefix) {
					t.Fatalf("%s links to private path %s", name, target)
				}
			}
		}
		for _, doc := range []string{
			"Requirements.md", "TechnicalDesign.md", "DevelopmentLoop.md",
			"IndustrySurvey.md", "Roadmap.md", "Compatibility.md",
		} {
			for _, prefix := range []string{"](" + doc, "](./" + doc, "](docs/" + doc, "](./docs/" + doc} {
				if strings.Contains(text, prefix) {
					t.Fatalf("%s links to private document %s", name, doc)
				}
			}
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
