package main

// 仓库级不变量的机器判据（模块卡 §4.2 的 `TestRepoInvariants_*`；`015-invariants` check 的承载面）。
// 与 `internal/e2e` 的行为断言不同，这里读的是仓库工件（go.mod / 生产源码 / workflow 文本），
// 判的是「发布面没有越界」，不构建也不启动二进制。
// Repository invariants scan released artifacts (go.mod, production sources, workflows);
// they never build or start the binary.

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// invariantHit 是一次命中。失败文案只给仓库相对路径与行号，**不回显命中内容**——
// 凭证类命中若被打印，等于把密钥再写进日志与 CI 工件。
// A hit reports repo-relative path:line only; matched text is never echoed.
type invariantHit struct {
	Path string
	Line int
	Text string
}

func (h invariantHit) String() string { return fmt.Sprintf("%s:%d", h.Path, h.Line) }

// scanSurfaces 遍历若干仓库相对路径（文件或目录），跳过 skip 认可的路径，
// 对文本文件逐行跑 match，返回文件序排列的命中。
// scanSurfaces walks repo-relative files/dirs, skipping paths accepted by skip,
// and matches line by line.
func scanSurfaces(
	t *testing.T,
	root string,
	rels []string,
	skip func(rel string) bool,
	match func(line string) bool,
) []invariantHit {
	t.Helper()
	var hits []invariantHit
	for _, rel := range rels {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		require.NoError(t, err, "不变量扫描面缺少 %s——判据面被删除或改名即红", rel)
		if !info.IsDir() {
			hits = append(hits, scanOneFile(t, root, abs, match)...)
			continue
		}
		walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			r, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return relErr
			}
			r = filepath.ToSlash(r)
			if skip != nil && skip(r) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			hits = append(hits, scanOneFile(t, root, p, match)...)
			return nil
		})
		require.NoError(t, walkErr, "遍历扫描面 %s 失败", rel)
	}
	return hits
}

func scanOneFile(t *testing.T, root, abs string, match func(line string) bool) []invariantHit {
	t.Helper()
	data, err := os.ReadFile(abs)
	require.NoError(t, err, "读取扫描面文件失败: %s", abs)
	rel, err := filepath.Rel(root, abs)
	require.NoError(t, err)
	rel = filepath.ToSlash(rel)
	var hits []invariantHit
	for i, line := range strings.Split(string(data), "\n") {
		if match(line) {
			hits = append(hits, invariantHit{Path: rel, Line: i + 1, Text: line})
		}
	}
	return hits
}

func requireNoHits(t *testing.T, hits []invariantHit, format string, args ...any) {
	t.Helper()
	if len(hits) == 0 {
		return
	}
	lines := make([]string, len(hits))
	for i, h := range hits {
		lines[i] = h.String()
	}
	t.Fatalf(format+"\n%s", append(args, strings.Join(lines, "\n"))...)
}

// runGoOutput 在仓库根执行 go 子命令。判据本身依赖 Go 工具链——
// 与 `internal/memory/import_test.go` 同口径：工具链不可用即判红，不得静默跳过。
func runGoOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go %s 失败（依赖图判据需要 Go 工具链）:\n%s", strings.Join(args, " "), out)
	return string(out)
}

// TestRepoInvariants_NoForbiddenAgentFramework 守宪法 I：循环体是本仓库代码，
// 不得引入 Agent 框架坐标（框架会接管循环或自动执行工具）。
// 逆改点：在 go.mod 加入 eino / langchaingo / adk-go / trpc-agent-go 任一坐标即红。
// Inverse probe: adding any forbidden agent-framework coordinate to go.mod turns this red.
func TestRepoInvariants_NoForbiddenAgentFramework(t *testing.T) {
	root := stageTestRoot(t)
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err, "读取 go.mod 失败")
	for _, coord := range []string{"eino", "langchaingo", "adk-go", "trpc-agent-go"} {
		require.NotContains(t, string(gomod), coord,
			"go.mod 出现 Agent 框架坐标 %q（宪法 I：ReAct 循环必须自实现，协议 SDK 不得接管循环）", coord)
	}
}

// TestRepoInvariants_NoProcessLocalTimezone 守时区纪律：生产代码不得依赖进程本地时区，
// 调度 zone 必须显式（缺省 UTC、Agent 配置显式 zone）。
// 扫描面 = `cmd/` / `internal/` 的非测试代码；测试文件不在卡片的判定面内。
// 逆改点：在任一生产文件写 time.Local 或 .Local() 即红。
// Inverse probe: any process-local timezone reference in non-test code turns this red.
func TestRepoInvariants_NoProcessLocalTimezone(t *testing.T) {
	root := stageTestRoot(t)
	pattern := regexp.MustCompile(`time\.Local\b|\.Local\(\)|time\.Now\(\)\.Location\(\)`)
	hits := scanSurfaces(t, root, []string{"cmd", "internal"}, func(rel string) bool {
		return strings.HasSuffix(path.Base(rel), "_test.go")
	}, pattern.MatchString)
	requireNoHits(t, hits,
		"生产代码使用了进程本地时区（时区必须显式：调度 zone 缺省 UTC、Agent 配置显式 zone）")
}

// TestRepoInvariants_NoPlaintextCredential 守凭证纪律：发布面零明文凭证，一律 `${ENV_VAR}` 占位。
// 扫描面按模块卡 §4.2：cmd/、internal/、scripts/、harness/、evals/、.github/、Makefile、
// basalt.yaml.example；排除 docs/ 示例（不在扫描面内）、.loop/、specs/（不在扫描面内）
// 与已发布记录（`harness/acceptance/records/`，其凭证面由 TestRecords_NoSecretsOrAbsolutePaths 承担）。
//
// `*_test.go` 不属于发布面：既有已批准测试用合成探测值（如 redaction 用例）按设计匹配宽松
// 模式，它们验证的是「输出面零回显」，不是被提交的凭证；发布二进制也不含测试文件。
// Test files are not part of the release surface; approved redaction tests use synthetic
// probe values by design, and the shipped binary contains no test files.
//
// 逆改点：在扫描面的生产文件 / 配置 / workflow 写入 sk-… 形态值即红。
// Inverse probe: writing an sk-… shaped value into any scanned release file turns this red.
func TestRepoInvariants_NoPlaintextCredential(t *testing.T) {
	root := stageTestRoot(t)
	// 含连字符形态（`sk-proj-` / `sk-ant-`）一并覆盖；另含 Slack / GitHub / AWS 的固定前缀。
	// The hyphen-inclusive form covers sk-proj- / sk-ant-; fixed prefixes cover Slack/GitHub/AWS.
	pattern := regexp.MustCompile(
		`sk-[A-Za-z0-9_-]{16,}|xox[baprs]-|ghp_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}`)
	surfaces := []string{
		"cmd", "internal", "scripts", "harness", "evals",
		".github", "Makefile", "basalt.yaml.example",
	}
	skip := func(rel string) bool {
		return strings.HasSuffix(path.Base(rel), "_test.go") ||
			strings.HasPrefix(rel, "harness/acceptance/records/")
	}
	hits := scanSurfaces(t, root, surfaces, skip, pattern.MatchString)
	requireNoHits(t, hits,
		"发布面出现明文凭证形态（一律 ${ENV_VAR} 占位，运行时注入；扫描面见模块卡 §4.2）")
}

// TestRepoInvariants_NoORM 守宪法 IX：不用 ORM，手写 SQL + schema.sql 幂等迁移。
// 逆改点：在 go.mod 加入 gorm / ent / xorm 任一坐标即红。
// Inverse probe: adding an ORM coordinate to go.mod turns this red.
func TestRepoInvariants_NoORM(t *testing.T) {
	root := stageTestRoot(t)
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err, "读取 go.mod 失败")
	for _, coord := range []string{"gorm", "entgo", "ent.io", "xorm"} {
		require.NotContains(t, string(gomod), coord,
			"go.mod 出现 ORM 坐标 %q（宪法 IX：手写 SQL，迁移只经 schema.sql + schema_version）", coord)
	}
}

// TestRepoInvariants_TestOnlyDependencyNotLinked 守单二进制纪律的依赖最小化：
// 测试专用坐标（技术栈句：santhosh-tekuri/jsonschema，仅 *_test.go 导入）不得进入发布二进制。
// 双重判据：① 生产源码零导入；② `go list -deps ./cmd/basalt` 的依赖图零命中（传递引入也算）。
// 逆改点：在生产代码 import santhosh-tekuri/jsonschema 即红（两层任一命中）。
// Inverse probe: importing the test-only module from production code turns this red.
func TestRepoInvariants_TestOnlyDependencyNotLinked(t *testing.T) {
	root := stageTestRoot(t)
	const coord = "santhosh-tekuri/jsonschema"
	hits := scanSurfaces(t, root, []string{"cmd", "internal"}, func(rel string) bool {
		return strings.HasSuffix(path.Base(rel), "_test.go")
	}, func(line string) bool { return strings.Contains(line, coord) })
	requireNoHits(t, hits,
		"生产代码导入了测试专用依赖 %s（发布二进制只带必要依赖）", coord)

	deps := runGoOutput(t, root, "list", "-deps", "-f", "{{.ImportPath}}", "./cmd/basalt")
	require.NotContains(t, deps, coord,
		"发布二进制依赖图包含测试专用依赖 %s（发布面只允许构建所需依赖）", coord)
}

// TestRepoInvariants_CIRunDirsUnique 守 CI 运行目录跨运行 / 重试唯一：
// 任一 `--out` 必须同时带 `GITHUB_RUN_ID` 与 `GITHUB_RUN_ATTEMPT`——只带 run id 时，
// 同一 run 的重试（run_attempt 递增）会撞上已存在的目录，`accept` 拒绝且 `record_id` 冲突；
// 固定名（含既有 `ci`）还会让不同运行互相覆盖证据。
// 逆改点：把任一 `--out` 改回固定名，或去掉 `GITHUB_RUN_ATTEMPT`。
// Inverse probe: fixing any --out or dropping GITHUB_RUN_ATTEMPT turns this red.
func TestRepoInvariants_CIRunDirsUnique(t *testing.T) {
	root := stageTestRoot(t)
	var workflows []string
	for _, glob := range []string{"*.yml", "*.yaml"} {
		found, err := filepath.Glob(filepath.Join(root, ".github", "workflows", glob))
		require.NoError(t, err, "glob workflow 失败")
		workflows = append(workflows, found...)
	}
	require.NotEmpty(t, workflows, "未找到 workflow 文件——CI 运行目录唯一性判据失效")

	// 只截 `--out` 后的参数做判据：同一行可能还有 `tee ...`（控制台日志），
	// 整行含两个变量不代表 `--out` 的值合规——把 `--out` 退回固定名时整行仍会命中。
	// Judge the argument after --out, not the whole line: a tee log on the same line
	// would otherwise mask a fixed --out value.
	argPattern := regexp.MustCompile(`--out\s+("[^"]*"|'[^']*'|\S+)`)
	var outArgs []invariantHit
	for _, wf := range workflows {
		data, err := os.ReadFile(wf)
		require.NoError(t, err, "读取 %s 失败", wf)
		rel, err := filepath.Rel(root, wf)
		require.NoError(t, err)
		rel = filepath.ToSlash(rel)
		for i, line := range strings.Split(string(data), "\n") {
			// 注释行里的 --out 不构成运行目录（例如说明文字），不计入判据。
			if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "--out") {
				continue
			}
			found := argPattern.FindAllStringSubmatch(line, -1)
			if len(found) == 0 {
				// fail-closed：解析不到参数就判红，不静默跳过生成位置的运行目录。
				t.Fatalf("%s:%d 的 --out 参数无法解析（续行？请把 --out 与其值写在同一行）", rel, i+1)
			}
			for _, m := range found {
				arg := strings.Trim(m[1], `"'`)
				outArgs = append(outArgs, invariantHit{Path: rel, Line: i + 1, Text: arg})
			}
		}
	}
	require.NotEmpty(t, outArgs, "workflow 中没有 --out——判据面失效，验收运行无处落证")

	for _, h := range outArgs {
		require.Contains(t, h.Text, "GITHUB_RUN_ID",
			"%s 的 --out %q 缺少 GITHUB_RUN_ID（跨运行会复用目录，证据互相覆盖）", h, h.Text)
		require.Contains(t, h.Text, "GITHUB_RUN_ATTEMPT",
			"%s 的 --out %q 缺少 GITHUB_RUN_ATTEMPT（同一 run 的重试会撞上已存在目录）", h, h.Text)
	}
}
