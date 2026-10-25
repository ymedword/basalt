//go:build e2e

package e2e

// 本文件是 US-14 安装器的**制品级**验收（2026-10-26 用户裁决 §5.3 第 3~5 条）：
// 只用构建后的 Basalt 二进制和已声明的 Go 工具链，在临时工作区走完整条链路——
// `basalt init` → `basalt demo install` → 执行报告里打印的构建命令 → 用真实 MCP
// 客户端连上导出的 docs-readonly，列出工具并读回一篇文档。
//
// 判据来源（模块卡 §5.3）：
//   - 安装器必须**实际导出源码**到报告指向的目录，不能只打印一个不存在的路径；
//   - 导出物自带最小 go.mod，可在独立目录构建，且仓库里不维护第二份源码副本；
//   - 源码完整性、构建命令可用性、真实连接 / 工具发现 / 一次文档读取都必须实测。
//
// The artifact-level check: only the built binary plus the declared Go toolchain, an
// install, the printed build command, a real MCP connect, tool discovery and one read.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// demoBuildCommandPattern 抓安装报告里 docs-readonly 的构建命令；两个路径都必须是
// 真实存在的绝对路径，测试随后按抓到的词元原样执行（不是拿常量重拼一遍）。
// The pattern captures the printed build command so the test runs those very tokens.
var demoBuildCommandPattern = regexp.MustCompile(`go -C (\S+) build -o (\S+) \.`)

// TestUS14DemoInstall_Artifact 只用构建后的二进制与 Go 工具链完成安装与首连。
// TestUS14DemoInstall_Artifact drives the install with the built binary only.
func TestUS14DemoInstall_Artifact(t *testing.T) {
	h := newHarness(t)

	if res := h.run(t, nil, "init"); res.ExitCode != 0 {
		t.Fatalf("basalt init 失败（exit %d）：%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	wsRoot := filepath.Join(h.Root, ".basalt")

	// 1) 只装 docs-diff：本用例的判据是源码导出与连接，不依赖 Python 样例库或 npm。
	install := h.run(t, nil, "demo", "install", "--demo", "docs-diff")
	if install.ExitCode != 0 {
		t.Fatalf("basalt demo install 失败（exit %d）：%s%s", install.ExitCode, install.Stdout, install.Stderr)
	}

	// 2) 源码完整性：报告指向的目录必须真的存在，且带可独立构建的最小 go.mod。
	srcDir := filepath.Join(wsRoot, "demos", "src", "docs-readonly")
	for _, name := range []string{"main.go", "main_test.go", "go.mod"} {
		info, err := os.Stat(filepath.Join(srcDir, name))
		if err != nil {
			t.Fatalf("导出目录缺少 %s：%v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("导出文件 %s 为空", name)
		}
	}
	gomod, err := os.ReadFile(filepath.Join(srcDir, "go.mod"))
	if err != nil {
		t.Fatalf("读取导出的 go.mod 失败：%v", err)
	}
	for _, want := range []string{"module basalt-docs-readonly", "go 1.27"} {
		if !strings.Contains(string(gomod), want) {
			t.Fatalf("go.mod 缺少 %q：\n%s", want, gomod)
		}
	}

	// 3) 构建命令可用：按报告里打印的命令逐字执行，产物必须非空。
	command := demoBuildCommandPattern.FindStringSubmatch(install.Stdout)
	if command == nil {
		t.Fatalf("安装报告里没有可执行的构建命令：\n%s", install.Stdout)
	}
	buildSrc, buildOut := command[1], command[2]
	if filepath.Clean(buildSrc) != filepath.Clean(srcDir) {
		t.Fatalf("构建命令的来源目录与实际导出目录不一致：%s ≠ %s", buildSrc, srcDir)
	}
	build := exec.CommandContext(t.Context(), "go", "-C", buildSrc, "build", "-o", buildOut, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("执行报告里的构建命令失败：%v\n%s", err, out)
	}
	binary, err := os.Stat(buildOut)
	if err != nil {
		t.Fatalf("构建产物不存在：%v", err)
	}
	if binary.Size() == 0 {
		t.Fatal("构建产物为空")
	}
	// 产物必须落在工作区的依赖目录：报告、文档与 mcp_servers.yaml 片段都把那里当作
	// MCP 入口的固定位置，落到别处会让用户按文档配出一个找不到的命令。
	// The artefact must land in the workspace dependency directory the docs point at.
	if depsDir := filepath.Join(wsRoot, "demos", "deps"); filepath.Clean(filepath.Dir(buildOut)) != filepath.Clean(depsDir) {
		t.Fatalf("构建产物不在依赖目录：%s ≠ %s", filepath.Dir(buildOut), depsDir)
	}

	// 4) 真实连接：工具发现三件套齐全，且一次 read_document 读回正文。
	docsRoot := filepath.Join(h.Root, "docs")
	if err := os.MkdirAll(docsRoot, 0o755); err != nil {
		t.Fatalf("建文档根失败：%v", err)
	}
	const documentBody = "# e2e 文档\n\nartifact-level read ok\n"
	if err := os.WriteFile(filepath.Join(docsRoot, "hello.md"), []byte(documentBody), 0o644); err != nil {
		t.Fatalf("写测试文档失败：%v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	pool := tool.NewMCPPool(tool.MCPPoolOptions{Root: wsRoot, Version: "us14-e2e"})
	defer func() {
		if cerr := pool.Close(); cerr != nil {
			t.Errorf("关闭 MCP 池失败：%v", cerr)
		}
	}()

	conns, failures, err := pool.ConnectDetailed(ctx, []core.MCPServerConfig{{
		Name:      "docs-readonly",
		Transport: "stdio",
		Command:   buildOut,
		Args:      []string{"-root", docsRoot},
	}})
	if err != nil {
		t.Fatalf("连接导出的 docs-readonly 失败：%v", err)
	}
	if len(failures) != 0 {
		for _, f := range failures {
			t.Errorf("连接失败项：server=%s stage=%s class=%s (%v)", f.Server, f.Stage, f.Class, f.Err)
		}
		t.Fatalf("期望零连接失败，实际 %d 项", len(failures))
	}
	if len(conns) != 1 {
		t.Fatalf("期望 1 条连接，实际 %d 条", len(conns))
	}

	tools := make(map[string]core.Tool, len(conns[0].Tools))
	for _, item := range conns[0].Tools {
		tools[item.Name()] = item
	}
	for _, want := range []string{"list_documents", "read_document", "document_metadata"} {
		if _, ok := tools[want]; !ok {
			t.Fatalf("工具发现缺少 %s；实际：%v", want, toolNamesOf(conns[0].Tools))
		}
	}

	read, ok := tools["read_document"]
	if !ok {
		t.Fatal("没有 read_document，无法验证真实读取")
	}
	result, err := read.Execute(ctx, core.ToolEnv{RunID: "e2e-artifact-read"}, json.RawMessage(`{"path":"hello.md"}`))
	if err != nil {
		t.Fatalf("read_document 执行失败：%v", err)
	}
	if !strings.Contains(result.Content, "artifact-level read ok") {
		t.Fatalf("读回的正文不含预期内容：%q", result.Content)
	}
}

// toolNamesOf 列出连接返回的工具名，用于失败信息可读。
// toolNamesOf lists the discovered tools for failure messages.
func toolNamesOf(items []core.Tool) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name())
	}
	return names
}
