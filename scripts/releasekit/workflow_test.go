package releasekit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// errUnsupportedStageCommand 表示 stage 里出现了非 `go test` 的命令，无法列名。
var errUnsupportedStageCommand = errors.New("stage lint 只支持 go test 命令")

// stageRef 把模块卡 §4.5 的 check 绑到它所属的 stage 文件。
// stageRef binds a module-card check to the stage file that owns it.
type stageRef struct {
	Check string
	File  string
}

// us17StageRefs 是模块卡 §4.5 的 18 个 check 与 §4.5 末尾「stage 归属」段落地。
// us17StageRefs mirrors the 18 checks and their stage ownership in the module card.
var us17StageRefs = []stageRef{
	{Check: "us17-unit", File: "us17-base.json"},
	{Check: "us17-race", File: "us17-base.json"},
	{Check: "us17-sign-verify", File: "us17-base.json"},
	{Check: "us17-record", File: "us17-base.json"},
	{Check: "us17-package-safety", File: "us17-base.json"},
	{Check: "us17-sbom", File: "us17-base.json"},
	{Check: "us17-private-backup", File: "us17-base.json"},
	{Check: "us17-workflow", File: "us17-base.json"},
	{Check: "us17-docs", File: "us17-base.json"},
	{Check: "us17-stage-lint", File: "us17-base.json"},
	{Check: "us17-artifact-windows", File: "us17-windows.json"},
	{Check: "us17-upgrade-windows", File: "us17-windows.json"},
	{Check: "us17-artifact-linux", File: "us17-linux.json"},
	{Check: "us17-upgrade-linux", File: "us17-linux.json"},
	{Check: "us17-repro", File: "us17-linux.json"},
	{Check: "us17-artifact-linux-arm64", File: "us17-linux-arm64.json"},
	{Check: "us17-verify-download", File: "us17-aggregate.json"},
	{Check: "us17-clean-checkout", File: "us17-aggregate.json"},
}

type stageCheck struct {
	ID            string   `json:"id"`
	Args          []string `json:"args"`
	RequiredTests []string `json:"required_tests"`
}

// stageTask 把 tasks.md 的任务号绑到该 stage 实际运行的 check。
// stageTask binds a tasks.md task id to the checks that stage runs.
type stageTask struct {
	ID     string   `json:"id"`
	Checks []string `json:"checks"`
}

type stageFile struct {
	Checks []stageCheck `json:"checks"`
	Tasks  []stageTask  `json:"tasks"`
}

// taskLinePattern 匹配 tasks.md 的任务行（勾选状态不计）。
// taskLinePattern matches a tasks.md task line regardless of its checkbox state.
var taskLinePattern = regexp.MustCompile(`(?m)^\s*- \[[ xX]\] (T\d{3})\b`)

// TestUS17_Workflow_NoPublish 守住发布工作流的只读边界：只构建与上传，不 tag、
// 不 push、不创建 Release，且四个目标都在矩阵里。
// TestUS17_Workflow_NoPublish keeps the release workflow read-only.
func TestUS17_Workflow_NoPublish(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("读取发布工作流：%v", err)
	}
	text := string(body)
	for _, banned := range []string{"git push", "gh release create", "contents: write", "workflow_run"} {
		if strings.Contains(text, banned) {
			t.Fatalf("发布工作流不得包含 %q", banned)
		}
	}
	if !strings.Contains(text, "permissions:") || !strings.Contains(text, "contents: read") {
		t.Fatal("发布工作流必须显式声明只读权限")
	}
	for _, target := range []string{"windows/amd64", "linux/amd64", "linux/arm64", "darwin/arm64"} {
		if !strings.Contains(text, target) {
			t.Fatalf("发布工作流缺少目标 %s", target)
		}
	}
}

// TestUS17_StageLint 校验五个 stage 与模块卡逐一对上，且 required_tests 逐名存在于
// 对应构建标签的包里。它只对本 US 的 stage 生效：全仓 lint 会把历史 stage 的 -race
// 集成包也拉进编译，冷缓存下超过 go test 默认超时；列名与 -race 无关，这里按
// build tag 列名即可，真正的 -race 由 us17-race 检查执行。
// TestUS17_StageLint checks the five stages against the module card and that every
// required test is listed by the package built with the same tags. It is scoped to
// this US's stages: the repo-wide lint compiles historical -race integration
// packages too, which exceeds go test's default timeout on a cold cache.
func TestUS17_StageLint(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	stageDir := filepath.Join(root, "harness", "stages")
	paths, err := filepath.Glob(filepath.Join(stageDir, "us17-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 5 {
		t.Fatalf("us17 stage 文件应为 5 个，实际 %d 个：%v", len(paths), paths)
	}
	stages := make(map[string]stageFile, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path) // #nosec G304 -- 仓库内 stage 文件
		if err != nil {
			t.Fatalf("读取 %s：%v", path, err)
		}
		var stage stageFile
		if err := json.Unmarshal(body, &stage); err != nil {
			t.Fatalf("解析 %s：%v", path, err)
		}
		stages[filepath.Base(path)] = stage
	}
	byCheck := map[string]stageCheck{}
	for _, ref := range us17StageRefs {
		stage, ok := stages[ref.File]
		if !ok {
			t.Fatalf("缺少 stage 文件 %s", ref.File)
		}
		index := slices.IndexFunc(stage.Checks, func(c stageCheck) bool { return c.ID == ref.Check })
		if index < 0 {
			t.Fatalf("%s 缺少 check %s", ref.File, ref.Check)
		}
		byCheck[ref.Check] = stage.Checks[index]
	}
	for name, stage := range stages {
		for _, check := range stage.Checks {
			if !slices.ContainsFunc(us17StageRefs, func(ref stageRef) bool {
				return ref.Check == check.ID && ref.File == name
			}) {
				t.Fatalf("%s 出现模块卡未登记的 check %s", name, check.ID)
			}
		}
	}
	// tasks 数组把任务号绑到 check；任务号必须真实存在于 tasks.md，"跳过某个任务"
	// 会在这里暴露成不存在的编号或悬空的 check 引用。
	tasksBody, err := os.ReadFile(filepath.Join(root, "specs", "017-release-artifacts", "tasks.md")) // #nosec G304 -- 仓库内 fixed 路径
	if err != nil {
		t.Fatalf("读取 tasks.md：%v", err)
	}
	knownTasks := map[string]bool{}
	for _, match := range taskLinePattern.FindAllStringSubmatch(string(tasksBody), -1) {
		knownTasks[match[1]] = true
	}
	for name, stage := range stages {
		for _, task := range stage.Tasks {
			if !knownTasks[task.ID] {
				t.Fatalf("%s 引用了 tasks.md 不存在的任务 %s", name, task.ID)
			}
			for _, check := range task.Checks {
				if !slices.ContainsFunc(stage.Checks, func(c stageCheck) bool { return c.ID == check }) {
					t.Fatalf("%s 的任务 %s 引用了本 stage 不存在的 check %s", name, task.ID, check)
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	for _, ref := range us17StageRefs {
		check := byCheck[ref.Check]
		if len(check.RequiredTests) == 0 {
			continue
		}
		args, err := stageListArgs(check.Args, check.RequiredTests)
		if err != nil {
			t.Fatalf("%s 的命令不可列名：%v", check.ID, err)
		}
		cmd := exec.CommandContext(ctx, "go", args...) // #nosec G204 -- args 由 stageListArgs 白名单构造
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s 列名失败：%v\n%s", check.ID, err, out)
		}
		listed := parseStageList(string(out))
		for _, name := range check.RequiredTests {
			top, _, _ := strings.Cut(name, "/")
			if !listed[top] {
				t.Fatalf("%s 的 required_test %s 不在 %v 的列名结果里", check.ID, top, args)
			}
		}
	}
}

// stageListArgs 只保留 build tag 与包参数，拼出 `go test -list`；-json / -count /
// -timeout / -run / -race 与列名无关，一律不保留。
// stageListArgs keeps only build tags and packages to build a read-only listing.
func stageListArgs(args []string, required []string) ([]string, error) {
	if len(args) < 3 || args[0] != "go" || args[1] != "test" {
		return nil, errUnsupportedStageCommand
	}
	out := []string{"test"}
	for _, arg := range args[2:] {
		if strings.HasPrefix(arg, "-tags=") || strings.HasPrefix(arg, "./") {
			out = append(out, arg)
		}
	}
	names := make([]string, 0, len(required))
	for _, name := range required {
		top, _, _ := strings.Cut(name, "/")
		if !slices.Contains(names, top) {
			names = append(names, top)
		}
	}
	for i, name := range names {
		names[i] = regexp.QuoteMeta(name)
	}
	return append(out, "-list", "^(?:"+strings.Join(names, "|")+")$"), nil
}

var stageTestLine = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func parseStageList(output string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); stageTestLine.MatchString(line) {
			names[line] = true
		}
	}
	return names
}
