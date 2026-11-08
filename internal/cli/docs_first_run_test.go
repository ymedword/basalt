package cli_test

// US-25 文档契约（FR-25-28～31）：四处文档面的首配固定顺序、容器隔离开关、多 Worker
// 口径与首发支持矩阵。断言读的是**真实文档文件**，不是夹具——文档改坏就红，改对才绿。
//
// 四条判据对应模块卡 §4.4 的四行：
//   - 九个首配锚点按固定顺序出现（中文三面共用一套词，README 用等价英文）；
//   - 容器隔离是 opt-in、不在首发支持矩阵、5 个 daily 脚本受影响、4 个 Flow 角色只读
//     不受影响、可回退 local；
//   - 四个面都写清多 Worker 的角色、DSN 复制、主密钥共享、种子与启动顺序；
//   - 发布说明与 Operations 的支持矩阵只写 Compose 多 Worker + LocalRunner，K8s /
//     数据库 HA / 真实双宿主只能以「未验证」出现，`release/support-matrix.json` 的
//     schema（键集）不变。
//
// The assertions read the real documentation surfaces; editing a document is the only
// way to turn them green, and breaking the contract turns them red.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 文档路径（相对仓库根）。
const (
	docsOperationsPath    = "docs/Operations.md"
	docsReadmePath        = "README.md"
	docsPackageReadmePath = "release/package-README.txt"
	docsYamlExamplePath   = "basalt.yaml.example"
	docsReleaseNotesPath  = "release/notes/TEMPLATE.md"
	docsSupportMatrixPath = "release/support-matrix.json"
)

// docsFirstRunAnchorsZh 是中文文档面共用的九个首配锚点；顺序即契约（FR-25-28）。
// The nine first-run anchors for the Chinese surfaces; order is the contract.
var docsFirstRunAnchorsZh = []string{
	"模型连接",
	"Agent 绑定与启用",
	"先建集合",
	"摄取",
	"只读绑定",
	"记忆确认（4 条）",
	"flows.enabled",
	"模板发布",
	"手动建任务",
}

// docsFirstRunAnchorsEn 是 README（英文）的等价九步。
// The equivalent English anchors used by the README.
var docsFirstRunAnchorsEn = []string{
	"model connection",
	"bind and enable the agent",
	"create collections",
	"ingest",
	"read-only knowledge bindings",
	"confirm memory (4 entries)",
	"flows.enabled",
	"publish the workflow template",
	"create a task manually",
}

// TestDocs_FirstRunChecklistOrder 判 FR-25-28：四条文档面的九步顺序、init/serve 都可
// 作为第一步、重试补发主密钥，以及旧口径「不得在全新目录先执行 init」已经消失。
// TestDocs_FirstRunChecklistOrder judges FR-25-28.
func TestDocs_FirstRunChecklistOrder(t *testing.T) {
	operations := docsRead(t, docsOperationsPath)
	section := docsBlockBetween(t, "Operations §1.5", operations, "### 1.5 首次配置清单", "\n---")
	docsRequireOrdered(t, "Operations §1.5", section, docsFirstRunAnchorsZh)
	for _, want := range []string{"`init`", "`serve`", "补发主密钥"} {
		if !strings.Contains(section, want) {
			t.Fatalf("Operations §1.5 缺少 %q：init 与首次 serve 都必须能作为第一步，"+
				"重试必须续做并补发主密钥（FR-25-28 / T023）", want)
		}
	}
	if strings.Contains(operations, "不得在全新目录先执行") {
		t.Fatalf("Operations 仍保留「不得在全新目录先执行 basalt init」的旧口径（FR-25-28 / T023）")
	}

	readme := docsRead(t, docsReadmePath)
	readmeBlock := docsBlockBetween(t, "README 首配段", readme,
		"### First-run configuration order", "\n### ")
	docsRequireOrdered(t, "README 首配段", readmeBlock, docsFirstRunAnchorsEn)

	packaged := docsRead(t, docsPackageReadmePath)
	packageBlock := docsBlockBetween(t, "package README 步骤 6", packaged,
		"6. 首次配置按固定顺序", "\n7. ")
	docsRequireOrdered(t, "package README 步骤 6", packageBlock, docsFirstRunAnchorsZh)
	for _, want := range []string{"`init`", "补发主密钥"} {
		if !strings.Contains(packaged, want) {
			t.Fatalf("package README 缺少 %q：安装说明必须与 Operations §1.5 同口径"+
				"（init 可作为第一步；重试补发主密钥）（FR-25-28 / T024）", want)
		}
	}
	if strings.Contains(packaged, "不会自动生成") {
		t.Fatalf("package README 仍写「不会自动生成主密钥」：重试现在会补发（FR-25-28 / T024）")
	}

	yaml := docsRead(t, docsYamlExamplePath)
	yamlBlock := docsBlockBetween(t, "basalt.yaml.example 首配注释", yaml,
		"# 首次配置顺序（固定）", "\n\n")
	docsRequireOrdered(t, "basalt.yaml.example 首配注释", yamlBlock, docsFirstRunAnchorsZh)
}

// TestDocs_IsolationOptInWarning 判 FR-25-29：容器隔离是 opt-in、不在首发支持矩阵、
// 5 个 daily 脚本受影响、4 个 Flow 角色只读不受影响、可以回退 local。
// TestDocs_IsolationOptInWarning judges FR-25-29.
func TestDocs_IsolationOptInWarning(t *testing.T) {
	zhFaces := []string{docsOperationsPath, docsPackageReadmePath, docsYamlExamplePath}
	for _, path := range zhFaces {
		body := docsRead(t, path)
		for _, want := range []string{
			"opt-in",
			"不在首发支持矩阵",
			"5 个 daily 脚本",
			"4 个 Flow 角色",
			"回退 local",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s 缺少容器隔离声明 %q：容器隔离是 opt-in，不在首发支持矩阵，"+
					"5 个 daily 脚本受影响，4 个 Flow 角色只读不受影响，可回退 local（FR-25-29）",
					path, want)
			}
		}
		if strings.Contains(body, "容器隔离已验证") {
			t.Fatalf("%s 宣称「容器隔离已验证」：本 US 不验证容器隔离（FR-25-29）", path)
		}
	}

	readme := docsRead(t, docsReadmePath)
	for _, want := range []string{
		"opt-in",
		"not in the first-release support matrix",
		"5 daily scripts",
		"4 Flow roles",
		"fall back to local",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("%s 缺少容器隔离声明 %q（FR-25-29）", docsReadmePath, want)
		}
	}
	if strings.Contains(readme, "container isolation is verified") {
		t.Fatalf("%s 宣称容器隔离已验证：本 US 不验证容器隔离（FR-25-29）", docsReadmePath)
	}
}

// TestDocs_MultiworkerQuickStartAndSecrets 判 FR-25-30：四个面都写清控制面 / Worker
// 角色、Worker 用 gateway、DSN 由控制面复制（不得自行生成）、主密钥共享、种子与启动
// 顺序。
// TestDocs_MultiworkerQuickStartAndSecrets judges FR-25-30.
func TestDocs_MultiworkerQuickStartAndSecrets(t *testing.T) {
	zhFaces := []string{docsOperationsPath, docsPackageReadmePath, docsYamlExamplePath}
	zhAnchors := []string{
		"控制面",
		"gateway",
		"DSN",
		"主密钥",
		"种子",
		"启动顺序",
		"复制",
		"不得自行生成",
	}
	for _, path := range zhFaces {
		body := docsRead(t, path)
		for _, want := range zhAnchors {
			if !strings.Contains(body, want) {
				t.Fatalf("%s 缺少多 Worker 声明 %q：角色、DSN 复制、主密钥共享、种子、启动顺序"+
					"都要写清（FR-25-30）", path, want)
			}
		}
	}

	readme := docsRead(t, docsReadmePath)
	for _, want := range []string{
		"control plane",
		"gateway",
		"DSN",
		"master key",
		"seed",
		"startup order",
		"copy",
		"must not generate",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("%s 缺少多 Worker 声明 %q（FR-25-30）", docsReadmePath, want)
		}
	}
}

// TestDocs_SupportMatrixOnlyComposeMultiworker 判 FR-25-31：发布说明与 Operations 的
// 支持矩阵只写 Compose 多 Worker + LocalRunner；K8s / 数据库 HA / 真实双宿主只能以
// 「未验证」出现；`release/support-matrix.json` 的键集不变。
// TestDocs_SupportMatrixOnlyComposeMultiworker judges FR-25-31.
func TestDocs_SupportMatrixOnlyComposeMultiworker(t *testing.T) {
	template := docsRead(t, docsReleaseNotesPath)
	templateBlock := docsBlockBetween(t, "发布说明支持矩阵", template, "## 部署支持矩阵", "\n## ")
	docsRequireSupportMatrixScope(t, "发布说明支持矩阵", templateBlock)

	operations := docsRead(t, docsOperationsPath)
	// 支持矩阵是 §8.3 的最后一小节：用二级标题作结束标记，区块才只含矩阵本身
	// （若用 "\n### "，区块会一直延伸到文件末尾，把后面无关章节里的 K8s 提法也卷进来）。
	// The matrix is the last subsection of §8.3; a level-2 end marker keeps the block to
	// the matrix itself.
	operationsBlock := docsBlockBetween(t, "Operations 首发支持矩阵", operations,
		"#### 首发支持矩阵", "\n## ")
	docsRequireSupportMatrixScope(t, "Operations 首发支持矩阵", operationsBlock)

	matrix := docsRead(t, docsSupportMatrixPath)
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(matrix), &parsed); err != nil {
		t.Fatalf("%s 不是合法 JSON：%v", docsSupportMatrixPath, err)
	}
	docsRequireKeySet(t, docsSupportMatrixPath, parsed, "schema", "note", "rows")

	// 键集不变还不够：`schema` 的**值**也不得变（FR-25-31：schema 与判卷逻辑不改）。
	// 改 schema 号或新增拓扑字段都会在这里红；逆改点：把 schema 改成 2。
	// The schema value itself is pinned too, not just the key set.
	var schema int
	if err := json.Unmarshal(parsed["schema"], &schema); err != nil || schema != 1 {
		t.Fatalf("%s 的 schema=%s 必须是 1（FR-25-31：schema 与判卷逻辑不改）", docsSupportMatrixPath, parsed["schema"])
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(parsed["rows"], &rows); err != nil {
		t.Fatalf("%s 的 rows 不是对象数组：%v", docsSupportMatrixPath, err)
	}
	for _, row := range rows {
		docsRequireKeySet(t, docsSupportMatrixPath+" rows", row,
			"goos", "goarch", "tier", "status", "reason")
	}
}

// docsRequireSupportMatrixScope 判一个支持矩阵区块：只承诺 Compose 多 Worker 与
// LocalRunner，K8s / 数据库 HA / 双宿主的每一行都必须带「未验证」。
// docsRequireSupportMatrixScope judges one support-matrix block.
func docsRequireSupportMatrixScope(t *testing.T, face, block string) {
	t.Helper()
	for _, want := range []string{"Compose 多 Worker", "LocalRunner"} {
		if !strings.Contains(block, want) {
			t.Fatalf("%s 缺少 %q：首发支持矩阵只写 Compose 多 Worker + LocalRunner（FR-25-31）", face, want)
		}
	}
	for _, line := range strings.Split(block, "\n") {
		forbidden := false
		for _, term := range []string{"K8s", "数据库 HA", "DB HA", "双宿主"} {
			if strings.Contains(line, term) {
				forbidden = true
			}
		}
		if forbidden && !strings.Contains(line, "未验证") {
			t.Fatalf("%s 的行 %q 提到 K8s / 数据库 HA / 双宿主却没标「未验证」"+
				"（FR-25-31：不得把未交付的部署形态写进支持矩阵）", face, line)
		}
	}
}

// docsRequireKeySet 判 JSON 对象的键集恰好是期望值——新增拓扑字段会立刻变红。
// docsRequireKeySet judges an exact key set so a new topology field fails immediately.
func docsRequireKeySet(t *testing.T, face string, object map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(object) != len(want) {
		t.Fatalf("%s 的键数是 %d，要求 %d（键集：%v）", face, len(object), len(want), want)
	}
	for _, key := range want {
		if _, ok := object[key]; !ok {
			t.Fatalf("%s 缺少键 %q（FR-25-31：support-matrix.json 的 schema 不得新增拓扑字段）", face, key)
		}
	}
}

// docsRequireOrdered 判一组锚点在文档块里按给定顺序出现，且每项都存在。
// docsRequireOrdered judges that every anchor appears, in the given order.
func docsRequireOrdered(t *testing.T, face, block string, anchors []string) {
	t.Helper()
	previous := -1
	for _, anchor := range anchors {
		index := strings.Index(block, anchor)
		if index < 0 {
			t.Fatalf("%s 缺少首配锚点 %q（FR-25-28 的九步顺序）", face, anchor)
		}
		if index <= previous {
			t.Fatalf("%s 的锚点 %q 出现在第 %d 字节，不大于前一项的 %d——顺序被调换（FR-25-28）",
				face, anchor, index, previous)
		}
		previous = index
	}
}

// docsBlockBetween 取从 start 标记（含标记之后的正文）到 end 标记之间的文档块；
// 找不到 start 直接判红，找不到 end 取到文件末尾。
// docsBlockBetween slices a document block between two markers.
func docsBlockBetween(t *testing.T, face, body, start, end string) string {
	t.Helper()
	from := strings.Index(body, start)
	if from < 0 {
		t.Fatalf("%s 缺少起始标记 %q（该文档面还没有对应的章节）", face, start)
	}
	rest := body[from+len(start):]
	if to := strings.Index(rest, end); to >= 0 {
		rest = rest[:to]
	}
	return rest
}

// docsRead 读仓库根下的文档文件。测试进程的工作目录是包目录，向上找到 go.mod 即根。
// docsRead reads a repository-root document.
func docsRead(t *testing.T, rel string) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败：%v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatalf("从 %q 向上找不到仓库根（go.mod）", root)
		}
		root = parent
	}
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("读取文档 %s 失败：%v", rel, err)
	}
	return string(body)
}
