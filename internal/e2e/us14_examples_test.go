//go:build e2e

package e2e

// 本文件补两条声明式复核（模块卡 C-13 / C-19 / C-20）：
//
//   - `TestExamples_PortableBaseline`——示例目录是便携基线：无实例 UID（大小写不敏感）、
//     无凭据；基线 `notify_mode: none`、无 `deliver_to` / `agent_uid`，`resource_bindings`
//     与 `notify_channels` 为空（按 YAML 结构判，块式写法同样覆盖）；SETUP 逐项写清
//     导入后的编辑项；没有任何自动导入入口或改写实例配置的脚本。每条判据都先用
//     植入样本证明自己能红，避免"干净"是空断言。
//   - `TestMCPEgress_Documentation`——出网边界按通道分开写：操作手册 §2.2 的六行边界表
//     逐行判极性（受白名单约束的写"是"，独立进程 / 下游写"否"），每份示例 README 的
//     MCP 出网段写明该通道不受白名单约束（没有 MCP 的模板写明内置工具受约束）。
//     不允许把 stdio 说成受白名单约束，也不允许把内置工具说成不受约束。
//
// These two tests are the named checks the reviewed module card lists for C-13 / C-19
// and C-20. Both prove their own discrimination with planted samples.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// exampleUIDPattern 只认可证明的实例 UID 形态：`rs_` / `ag_` 加 26 位 Crockford Base32。
// 解析入口 `ParseAgentUID` / `ParseResourceUID` 接受小写输入并规范化，所以扫描必须
// 大小写不敏感——否则小写 UID 能绕过基线判据。
// The pattern matches both real UID prefixes with a 26-char Crockford Base32 body;
// matching is case-insensitive because the parsers accept lowercase input.
var exampleUIDPattern = regexp.MustCompile(`(?i)\b(?:rs|ag)_[0-9A-HJKMNP-TV-Z]{26}\b`)

// exampleSetupServicePattern 抓 SETUP 里 `mcp_servers.yaml` 片段中的具体服务名。
// It matches the concrete server name written in a SETUP's mcp_servers.yaml snippet.
var exampleSetupServicePattern = regexp.MustCompile(`(?m)^\s*-\s+name:\s*[A-Za-z0-9][A-Za-z0-9_-]*\s*$`)

// exampleScriptExtensions 是"可执行入口"的扩展名集合；这些文件里出现实例配置键
// 说明脚本可能在自动导入或放宽白名单。
var exampleScriptExtensions = map[string]bool{
	".py": true, ".sh": true, ".ps1": true, ".mjs": true, ".js": true, ".bat": true, ".cmd": true,
}

// exampleImportForbiddenTokens 是脚本里不得出现的实例配置改写目标（C-13）。
var exampleImportForbiddenTokens = []string{
	"mcp_servers.yaml", "allowed_commands", "allowed_domains", "shell.interpreters",
}

// exampleDailyDemos 是五个每日 Demo 的目录名（与 §1.5 一致）。
var exampleDailyDemos = []string{"github-radar", "repo-digest", "biz-daily", "docs-diff", "industry-brief"}

// exampleTemplates 是八个模板的目录名（与 §1.6 一致）。
var exampleTemplates = []string{
	"weather-brief", "arxiv-tracker", "policy-watch", "ticket-insights",
	"onboarding-buddy", "dependency-watch", "web-watch", "funding-watch",
}

func TestExamples_PortableBaseline(t *testing.T) {
	repo, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}
	examplesDir := filepath.Join(repo, "examples")

	// 1) 凭据与 UID：整个 examples/ 都是公开可复制的基线，扫描面不挑目录。
	if findings := templateScanSecrets(t, examplesDir); len(findings) > 0 {
		t.Fatalf("examples/ 含凭据字面量（只能用 ${ENV_VAR} 占位）：%s", strings.Join(findings, "；"))
	}
	if hits := exampleUIDHits(t, examplesDir); len(hits) > 0 {
		t.Fatalf("examples/ 含实例 UID 字面量（基线必须可移植）：%s", strings.Join(hits, "；"))
	}

	// 2) 十三个 AGENT.md 的基线字段（按 YAML 结构判，块式绑定同样覆盖）。
	for _, name := range exampleDailyDemos {
		checkExampleAgentBaseline(t, filepath.Join(examplesDir, "daily", name, "AGENT.md"), name)
	}
	for _, name := range exampleTemplates {
		checkExampleAgentBaseline(t, filepath.Join(examplesDir, "templates", name, "AGENT.md"), name)
	}

	// 3) 五个 SETUP 的编辑项：模型绑定、服务名、渠道 UID、deliver_to、解释器路径。
	for _, name := range exampleDailyDemos {
		path := filepath.Join(examplesDir, "daily", name, "SETUP.md")
		if findings := setupFindings(name, exampleReadText(t, path)); len(findings) > 0 {
			t.Fatalf("%s", strings.Join(findings, "；"))
		}
	}

	// 4) 不自动导入、不改写实例配置：示例目录里不允许出现导入 / 安装入口，
	//    README 要写明这一点，脚本不得碰 mcp_servers.yaml 或白名单键。
	if findings := exampleImportFindings(t, examplesDir); len(findings) > 0 {
		t.Fatalf("%s", strings.Join(findings, "；"))
	}
	if overview := exampleReadText(t, filepath.Join(examplesDir, "README.md")); len(readmeImportClaimFindings(overview)) > 0 {
		t.Fatalf("%s", strings.Join(readmeImportClaimFindings(overview), "；"))
	}

	// 逆改点：植入样本，判据必须命中——否则上面的"干净"什么也没证明。
	planted := t.TempDir()
	uidSample := "agent_uid: " + "rs_" + strings.Repeat("a", 26) + "\n"
	if err := os.WriteFile(filepath.Join(planted, "uid.md"), []byte(uidSample), 0o644); err != nil {
		t.Fatalf("写植入样本失败: %v", err)
	}
	if hits := exampleUIDHits(t, planted); len(hits) == 0 {
		t.Fatal("UID 判据没抓到小写植入样本——判据失效")
	}
	tokenSample := "token: " + "gh" + "p_" + strings.Repeat("b2", 15) + "\n"
	if err := os.WriteFile(filepath.Join(planted, "leak.md"), []byte(tokenSample), 0o644); err != nil {
		t.Fatalf("写植入样本失败: %v", err)
	}
	if findings := templateScanSecrets(t, planted); len(findings) == 0 {
		t.Fatal("凭据判据没抓到植入样本——判据失效")
	}

	// URL 内嵌凭据按主机名判：文档占位域（RFC 2606）豁免，其余主机必须命中。
	placeholderURL := filepath.Join(planted, "url-placeholder.md")
	if err := os.WriteFile(placeholderURL, []byte("https://user:secret@example.com/a\n"), 0o644); err != nil {
		t.Fatalf("写植入样本失败: %v", err)
	}
	for _, finding := range templateScanSecrets(t, planted) {
		if strings.Contains(finding, "URL 内嵌凭据") {
			t.Fatalf("文档占位域的反例夹具被误报成凭据：%s", finding)
		}
	}
	corporateURL := filepath.Join(planted, "url-corporate.md")
	if err := os.WriteFile(corporateURL, []byte("https://user:secret@intranet.corp/a\n"), 0o644); err != nil {
		t.Fatalf("写植入样本失败: %v", err)
	}
	hit := false
	for _, finding := range templateScanSecrets(t, planted) {
		if strings.Contains(finding, "URL 内嵌凭据") {
			hit = true
		}
	}
	if !hit {
		t.Fatal("URL 内嵌凭据判据没抓到非占位域的植入样本——判据失效")
	}

	// 逆改点：基线字段与大块绑定必须被结构判据抓住（内联空列表的旧判据抓不到块式）。
	badFront := "notify_mode: allowlist\nresource_bindings:\n  model_connection:\n    - name: demo\n      uid: rs_" + strings.Repeat("A", 26) + "\n"
	if findings := agentBaselineFindings("planted", badFront); len(findings) == 0 {
		t.Fatal("AGENT.md 基线判据没抓到块式绑定 / notify_mode 违规——判据失效")
	}

	// 逆改点：SETUP 缺一个编辑项必须被点名。
	if findings := setupFindings("planted", "不会自动导入。模型连接在这里选。mcp_servers.yaml 里写 `- name: demo`。UID 在这里。deliver_to 在这里。"); len(findings) == 0 {
		t.Fatal("SETUP 判据没抓到缺失的编辑项——判据失效")
	}

	// 逆改点：嵌套目录里的导入脚本与 README 缺声明都必须被抓住。
	plantedDemo := t.TempDir()
	nested := filepath.Join(plantedDemo, "daily", "demo", "scripts")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("建植入目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "import-demo.sh"), []byte("#!/bin/sh\ncp -r . ~/.basalt/agents/demo\n"), 0o644); err != nil {
		t.Fatalf("写植入脚本失败: %v", err)
	}
	if findings := exampleImportFindings(t, plantedDemo); len(findings) == 0 {
		t.Fatal("自动导入判据没抓到嵌套脚本——判据失效")
	}
	if findings := readmeImportClaimFindings("# examples\n\n复制即可。\n"); len(findings) == 0 {
		t.Fatal("README 判据没抓到缺失的「不会自动导入」声明——判据失效")
	}
}

// checkExampleAgentBaseline 判一个 AGENT.md 的便携基线字段。
// checkExampleAgentBaseline asserts the portable baseline fields of one AGENT.md.
func checkExampleAgentBaseline(t *testing.T, path, name string) {
	t.Helper()
	front := exampleFrontmatter(t, exampleReadText(t, path), name)
	if findings := agentBaselineFindings(name, front); len(findings) > 0 {
		t.Fatalf("%s", strings.Join(findings, "；"))
	}
}

// agentBaselineFindings 按 YAML 结构判基线：块式 `resource_bindings` 与内联空列表
// 走同一条判据，不依赖"值必须写成 `[...]`"的文本形态。
// agentBaselineFindings judges the baseline structurally, so block-style bindings and
// inline empty lists are handled by the same predicate.
func agentBaselineFindings(name, front string) []string {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(front), &doc); err != nil {
		return []string{name + " 的 frontmatter 不是合法 YAML"}
	}
	var findings []string
	if mode, ok := doc["notify_mode"].(string); !ok || mode != "none" {
		findings = append(findings, name+" 的基线不是 notify_mode: none")
	}
	if _, ok := doc["deliver_to"]; ok {
		findings = append(findings, name+" 的 AGENT.md 含 deliver_to（代投只能由 SETUP 在导入后配置）")
	}
	if _, ok := doc["agent_uid"]; ok {
		findings = append(findings, name+" 的 AGENT.md 含 agent_uid")
	}
	if entries := nestedEntryCount(doc["resource_bindings"]); entries > 0 {
		findings = append(findings, fmt.Sprintf("%s 的 resource_bindings 已绑定 %d 条（基线必须为空）", name, entries))
	}
	if entries := nestedEntryCount(doc["notify_channels"]); entries > 0 {
		findings = append(findings, fmt.Sprintf("%s 的 notify_channels 已声明 %d 个（基线必须为空）", name, entries))
	}
	slices.Sort(findings)
	return findings
}

// nestedEntryCount 数一个 YAML 值里的实际条目：nil / 空列表 = 0，块式列表按元素数，
// 映射递归求和；结构异常（既不是列表也不是空值）按 1 条违规处理。
// nestedEntryCount counts real entries in a decoded YAML value.
func nestedEntryCount(value any) int {
	switch typed := value.(type) {
	case nil:
		return 0
	case []any:
		return len(typed)
	case map[string]any:
		total := 0
		for _, entry := range typed {
			total += nestedEntryCount(entry)
		}
		return total
	default:
		return 1
	}
}

// setupFindings 逐项判 SETUP 的导入编辑项：模型绑定、服务名、渠道 UID、deliver_to、
// 解释器路径，以及"不会自动导入"的声明本身。每项独立成条，缺哪项点名哪项。
// setupFindings checks each SETUP edit item independently.
func setupFindings(name, text string) []string {
	checks := []struct {
		item    string
		missing bool
	}{
		{"不自动导入声明", !strings.Contains(text, "不会自动导入")},
		{"模型绑定", !strings.Contains(text, "模型连接")},
		{"MCP 服务名", !strings.Contains(text, "mcp_servers.yaml") || !exampleSetupServicePattern.MatchString(text)},
		{"渠道 UID", !strings.Contains(text, "UID")},
		{"deliver_to", !strings.Contains(text, "deliver_to")},
		{"解释器路径", !strings.Contains(text, "解释器") || !strings.Contains(text, "路径")},
	}
	var findings []string
	for _, check := range checks {
		if check.missing {
			findings = append(findings, name+"/SETUP.md 缺少导入编辑项："+check.item)
		}
	}
	return findings
}

// exampleImportFindings 扫示例目录里的自动导入入口与改写实例配置的脚本：
// 顶层任何 import*/install* 名称、任意深度的 import*/install* 脚本，以及脚本里
// 出现 `mcp_servers.yaml` / 白名单键（C-13 的"不自动导入、不放宽白名单"）。
// exampleImportFindings scans for auto-import entry points and scripts that rewrite
// instance configuration.
func exampleImportFindings(t *testing.T, dir string) []string {
	t.Helper()
	var findings []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		lowerName := strings.ToLower(entry.Name())
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		looksLikeImport := strings.HasPrefix(lowerName, "import") || strings.HasPrefix(lowerName, "install")
		switch {
		case looksLikeImport && exampleScriptExtensions[extension]:
			findings = append(findings, "examples/ 下出现疑似自动导入脚本："+relative)
		case looksLikeImport && !strings.Contains(relative, "/"):
			findings = append(findings, "examples/ 下出现疑似自动导入入口："+relative)
		}
		if !exampleScriptExtensions[extension] {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, token := range exampleImportForbiddenTokens {
			if strings.Contains(string(raw), token) {
				findings = append(findings, relative+" 的脚本改写了实例配置："+token)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 %s 失败: %v", filepath.ToSlash(dir), err)
	}
	slices.Sort(findings)
	return findings
}

// readmeImportClaimFindings 判 examples/README.md 是否写明示例不会被自动导入。
// readmeImportClaimFindings requires the examples overview to say imports are manual.
func readmeImportClaimFindings(text string) []string {
	if strings.Contains(text, "imported automatically") || strings.Contains(text, "不会自动导入") {
		return nil
	}
	return []string{"examples/README.md 没有写明示例不会被自动导入"}
}

// exampleUIDHits 扫目录下所有文件里的实例 UID 字面量，返回「相对路径：UID」（升序）。
// exampleUIDHits returns "relpath: uid" hits for literal instance UIDs.
func exampleUIDHits(t *testing.T, dir string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		for _, match := range exampleUIDPattern.FindAllString(string(raw), -1) {
			hits = append(hits, filepath.ToSlash(rel)+"："+match)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 %s 失败: %v", filepath.ToSlash(dir), err)
	}
	slices.Sort(hits)
	return hits
}

// exampleFrontmatter 取出首个 `---` 块并统一换行；缺失即判红。
// exampleFrontmatter extracts the leading front matter, normalizing CRLF.
func exampleFrontmatter(t *testing.T, text, name string) string {
	t.Helper()
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		t.Fatalf("%s 的 AGENT.md 没有 frontmatter", name)
	}
	rest := normalized[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		t.Fatalf("%s 的 frontmatter 没有结束行", name)
	}
	return rest[:end]
}

// exampleReadOptional 读一个可缺失的文本文件；缺失由调用方记成 finding，
// 这样植入样本的逆改点也能复用同一套判据（而不是换一套宽松逻辑）。
// exampleReadOptional reads a possibly missing file so the planted-sample branch
// exercises the same findings path as the real tree.
func exampleReadOptional(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// exampleReadText 读一个必须存在的文本文件。
// exampleReadText reads a file that must exist.
func exampleReadText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", filepath.ToSlash(path), err)
	}
	return string(raw)
}

func TestMCPEgress_Documentation(t *testing.T) {
	repo, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}

	if findings := egressDocFindings(t, repo); len(findings) > 0 {
		t.Fatalf("出网边界文档不一致：%s", strings.Join(findings, "；"))
	}

	// 逆改点 1：缺出网段的 README 必须被**点名**，不能靠"其它文件读不到"凑出失败。
	missing := readmeEgressFindings("planted", "# demo\n\n没有出网段。\n")
	if !slices.Contains(missing, "planted/README.md 缺出网边界段（MCP 自身出网范围 / 出网边界）") {
		t.Fatalf("缺段 README 没有被点名：%v", missing)
	}

	// 逆改点 2：没有 MCP 的模板必须写"受白名单约束"；写反成"不受"必须红，写对必须绿。
	wrongPolarity := "| MCP | 无 |\n\n## 出网边界\n\nhttp_get 不受白名单约束。\n"
	if findings := readmeEgressFindings("planted", wrongPolarity); len(findings) == 0 {
		t.Fatal("无 MCP 模板把极性写反（不受白名单约束）时判据没红——判据失效")
	}
	rightPolarity := "| MCP | 无 |\n\n## 出网边界\n\nhttp_get 受实例的域名白名单约束。\n"
	if findings := readmeEgressFindings("planted", rightPolarity); len(findings) != 0 {
		t.Fatalf("无 MCP 模板写对极性时被误报：%v", findings)
	}
	// 正例：有 MCP 的 README 写明该通道不受白名单约束时不该红。
	withMCP := "| MCP | github |\n\n## MCP 自身出网范围\n\n它是独立进程，不受 Basalt 白名单约束。\n"
	if findings := readmeEgressFindings("planted", withMCP); len(findings) != 0 {
		t.Fatalf("有 MCP 的 README 写对边界时被误报：%v", findings)
	}

	// 逆改点 3：Operations 边界表逐行判极性——"远程 MCP 下游"写成受约束必须红，写否必须绿。
	if findings := operationsEgressFindings(plantedEgressSection(true)); len(findings) == 0 {
		t.Fatal("Operations 边界表把远程下游写成受白名单约束时判据没红——判据失效")
	}
	if findings := operationsEgressFindings(plantedEgressSection(false)); len(findings) != 0 {
		t.Fatalf("Operations 边界表写对极性时被误报：%v", findings)
	}
}

// operationsEgressHeading 是操作手册 §2.2 的标题；改标题必须同步改本判据。
const operationsEgressHeading = "### 2.2 出网：按通道分开看"

// egressCorridorRows 是 §2.2 边界表必须逐行出现的走廊与期望极性：
// constrained=true 表示该行必须写"受白名单约束"（是），false 表示必须写"否"。
var egressCorridorRows = []struct {
	tag         string
	constrained bool
}{
	{"工具出网", true},
	{"MCP http 传输", true},
	{"供应商模型调用", false},
	{"MCP server 自己的网络行为", false},
	{"远程 MCP 下游", false},
	{"stdio 启动程序", true},
}

// egressDocFindings 检查操作手册 §2.2 的边界表与每份示例 README 的 MCP 出网段。
// egressDocFindings checks the Operations §2.2 table and every example README.
func egressDocFindings(t *testing.T, root string) []string {
	t.Helper()
	var findings []string
	for _, name := range exampleDailyDemos {
		text, ok := exampleReadOptional(filepath.Join(root, "examples", "daily", name, "README.md"))
		if !ok {
			findings = append(findings, name+"/README.md 读不到")
			continue
		}
		findings = append(findings, readmeEgressFindings(name, text)...)
	}
	for _, name := range exampleTemplates {
		text, ok := exampleReadOptional(filepath.Join(root, "examples", "templates", name, "README.md"))
		if !ok {
			findings = append(findings, name+"/README.md 读不到")
			continue
		}
		findings = append(findings, readmeEgressFindings(name, text)...)
	}

	operations, ok := exampleReadOptional(filepath.Join(root, "docs", "Operations.md"))
	if !ok {
		return append(findings, "docs/Operations.md 读不到")
	}
	section := sectionText(operations, operationsEgressHeading)
	if section == "" {
		return append(findings, "Operations.md 缺 §2.2 出网边界表")
	}
	return append(findings, operationsEgressFindings(section)...)
}

// operationsEgressFindings 逐行判 §2.2 边界表的走廊与极性：走廊缺行、极性写反、
// stdio 启动程序没写 `shell.allowed_commands` 都各自成条。
// operationsEgressFindings checks each corridor row and its whitelist polarity.
func operationsEgressFindings(section string) []string {
	var findings []string
	if !strings.Contains(section, "白名单") {
		findings = append(findings, "Operations.md §2.2 未提白名单")
	}
	for _, corridor := range egressCorridorRows {
		row := egressTableRow(section, corridor.tag)
		if row == "" {
			findings = append(findings, "Operations.md §2.2 缺走廊："+corridor.tag)
			continue
		}
		cell := strings.TrimSpace(egressWhitelistCell(row))
		constrained := strings.HasPrefix(cell, "**是**") || strings.HasPrefix(cell, "是")
		unconstrained := strings.HasPrefix(cell, "**否**") || strings.HasPrefix(cell, "否")
		if corridor.constrained && !constrained {
			findings = append(findings, "Operations.md §2.2 的走廊极性写反（应为「是」）："+corridor.tag)
		}
		if !corridor.constrained && !unconstrained {
			findings = append(findings, "Operations.md §2.2 的走廊极性写反（应为「否」）："+corridor.tag)
		}
		if corridor.tag == "stdio 启动程序" && !strings.Contains(cell, "shell.allowed_commands") {
			findings = append(findings, "Operations.md §2.2 的 stdio 启动程序未写 shell.allowed_commands")
		}
	}
	return findings
}

// egressTableRow 取边界表里包含 tag 的第一行；找不到返回空串。
// egressTableRow returns the first table row containing the corridor tag.
func egressTableRow(section, tag string) string {
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "|") && strings.Contains(trimmed, tag) {
			return trimmed
		}
	}
	return ""
}

// egressWhitelistCell 取表格行的第三列（受不受白名单）。
// egressWhitelistCell returns the third cell of a boundary-table row.
func egressWhitelistCell(row string) string {
	cells := strings.Split(row, "|")
	if len(cells) < 4 {
		return ""
	}
	return cells[3]
}

// plantedEgressSection 造一张边界表；remoteConstrained 用于把"远程 MCP 下游"的
// 极性故意写反，证明逐行判据能红。
func plantedEgressSection(remoteConstrained bool) string {
	remote := "否——上游连接受白名单约束，下游不受"
	if remoteConstrained {
		remote = "**是**——上游连接受白名单约束"
	}
	return strings.Join([]string{
		"| 流量 | 谁控制 | 受不受白名单 |",
		"| --- | --- | --- |",
		"| 工具出网（`http_get` / `http_post`）+ 通知投递 | **模型**可控 | **是**——网络域名白名单 |",
		"| MCP http 传输的连接 | Basalt 发起 | **是**——连接前按白名单授权 |",
		"| 供应商模型调用 | 运维配置 | 否——模型连接的独立配置 |",
		"| MCP server 自己的网络行为（stdio 进程内） | server 自己 | 否——白名单管不到 |",
		"| 远程 MCP 下游 | server 自己 | " + remote + " |",
		"| stdio 启动程序 | Basalt 发起 | **是**（`shell.allowed_commands`） |",
	}, "\n")
}

// readmeEgressFindings 检查一份示例 README 有没有出网边界段，且按通道写对极性。
// 有 MCP 的示例用「MCP 自身出网范围」，只用内置工具的模板用「出网边界」——两种标题都
// 接受，判的是内容：有 MCP 就必须写明该通道不受白名单约束，没有 MCP 就必须写明内置
// 工具受白名单约束（或写明不加白名单会得到 `sandbox_denied`）。标题只是入口，不是判据。
// readmeEgressFindings accepts either egress heading and judges the polarity per
// channel, not the heading text.
func readmeEgressFindings(name, text string) []string {
	heading := "MCP 自身出网范围"
	index := strings.Index(text, heading)
	if index < 0 {
		heading = "出网边界"
		index = strings.Index(text, heading)
	}
	if index < 0 {
		return []string{name + "/README.md 缺出网边界段（MCP 自身出网范围 / 出网边界）"}
	}
	window := text[index:]
	if cut := strings.Index(window, "\n## "); cut > 0 {
		window = window[:cut]
	}
	var findings []string
	if !strings.Contains(window, "白名单") {
		findings = append(findings, name+"/README.md 的"+heading+"段未提白名单")
	}
	// 表格行 `| MCP | 无` 是模板声明"没有 MCP"的固定写法；其余按有 MCP 判。
	hasMCP := !strings.Contains(text, "| MCP | 无")
	compact := strings.Join(strings.Fields(window), "")
	bypass := strings.Contains(compact, "不受") || strings.Contains(compact, "管不到") || strings.Contains(compact, "不代管")
	if hasMCP {
		if !bypass {
			findings = append(findings, name+"/README.md 的出网段没写清白名单管不到该通道")
		}
		return findings
	}
	// 无 MCP：必须正向写清"受…白名单…约束"；"不受白名单约束"是写反，不能算通过。
	constrained := strings.Contains(compact, "受") && !strings.Contains(compact, "不受") &&
		strings.Contains(compact, "白名单") && strings.Contains(compact, "约束")
	if !constrained && !strings.Contains(compact, "sandbox_denied") {
		findings = append(findings, name+"/README.md 的出网段没写清内置工具受白名单约束")
	}
	return findings
}

// sectionText 取指定标题到下一个同级标题之间的正文；标题不存在返回空串。
// sectionText returns the body between a heading and the next same-or-higher heading.
func sectionText(text, heading string) string {
	index := strings.Index(text, heading)
	if index < 0 {
		return ""
	}
	rest := text[index+len(heading):]
	if cut := strings.Index(rest, "\n### "); cut >= 0 {
		rest = rest[:cut]
	}
	return rest
}
