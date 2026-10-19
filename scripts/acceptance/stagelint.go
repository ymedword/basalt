package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// listFunc 执行 `go test -list` 并返回标准输出；测试可注入假实现。
// listFunc runs `go test -list` and returns stdout; tests may inject a fake.
type listFunc func(ctx context.Context, root string, args []string) (string, error)

type lintFinding struct {
	Stage    string
	Check    string
	Test     string
	Severity string
}

// transformListArgs 把执行型 `go test` 参数改成只读列名模式：命令名固定为 go test，
// 去掉执行与计数参数，保留 `-C`、build tag 与包参数，拒绝可替换执行体的旗标
// （-exec / -toolexec / -overlay），并把 -list 的模式限定为 required 的顶层测试名。
// transformListArgs turns a running `go test` command into a read-only listing command:
// the command is pinned to `go test`, execution/count flags are dropped, -C/tags/packages
// stay, execution-replacing flags are refused, and -list is pinned to top-level names.
func transformListArgs(args []string, required []string) ([]string, error) {
	if len(args) == 0 || args[0] != "go" {
		return nil, errors.New("stage lint only supports go test commands")
	}
	rest := args[1:]
	out := []string{"go"}
	for len(rest) >= 2 && rest[0] == "-C" {
		out = append(out, rest[0], rest[1])
		rest = rest[2:]
	}
	if len(rest) == 0 || rest[0] != "test" {
		return nil, errors.New("stage lint only supports go test commands")
	}
	out = append(out, "test")
	rest = rest[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "-json" || arg == "-v":
			continue
		case arg == "-count" || arg == "-run" || arg == "-list":
			i++
			continue
		case strings.HasPrefix(arg, "-count=") || strings.HasPrefix(arg, "-run=") || strings.HasPrefix(arg, "-list="):
			continue
		case arg == "-exec" || strings.HasPrefix(arg, "-exec=") ||
			arg == "-toolexec" || strings.HasPrefix(arg, "-toolexec=") ||
			arg == "-overlay" || strings.HasPrefix(arg, "-overlay="):
			return nil, fmt.Errorf("stage lint refuses execution-replacing flag %q", arg)
		default:
			out = append(out, arg)
		}
	}
	names := topNames(required)
	pattern := ".*"
	switch len(names) {
	case 1:
		pattern = "^" + regexp.QuoteMeta(names[0]) + "$"
	case 0:
	default:
		quoted := make([]string, 0, len(names))
		for _, name := range names {
			quoted = append(quoted, regexp.QuoteMeta(name))
		}
		pattern = "^(?:" + strings.Join(quoted, "|") + ")$"
	}
	return append(out, "-list", pattern), nil
}

func topName(test string) string {
	name, _, _ := strings.Cut(test, "/")
	return name
}

func topNames(required []string) []string {
	var names []string
	for _, test := range required {
		name := topName(test)
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

var testNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func parseListOutput(output string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if testNamePattern.MatchString(line) {
			names[line] = true
		}
	}
	return names
}

func runGoList(ctx context.Context, root string, args []string) (string, error) {
	// #nosec G204 -- 豁免理由：① 命令名由 transformListArgs 校验为常量 "go"，
	// 子命令必须是 test；参数经变换后仅保留 -C / build tag / 包路径并追加 -list，
	// -exec / -toolexec / -overlay 一律拒绝，不经 shell、逐项直执；② 输入是仓库
	// 受版本控制、随本 US 评审的 harness/stages/*.json（lint 逐个读取），与代码
	// 同信任级，不是外部数据；③ 只执行只读的 `go test -list`，不运行测试、不写
	// 文件；④ G204 只接受常量实参，数据驱动的 -tags / 包路径静态不可见，无法用
	// 代码消除；改用 os.StartProcess 或掩盖调用只会削弱审计面
	// （用户 2026-10-20 裁决，与 internal/tool runner / mcp 的 G204 豁免同口径）。
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go test -list failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// lintStages 对每个 stage 的 required_tests 做只读存在性预检：failStage 失配即错误，
// 历史 stage 失配只告警。它不执行任何测试，也不改写 stage 文件。
// lintStages is a read-only existence preflight for stage required_tests; the named
// failStage is fail-closed, other stages only warn. No test is executed and no stage
// file is modified.
func lintStages(ctx context.Context, root, failStage string, list listFunc) ([]lintFinding, error) {
	if strings.TrimSpace(failStage) == "" {
		return nil, errors.New("stage lint requires the current stage to fail closed")
	}
	fail := filepath.ToSlash(filepath.Clean(failStage))
	if filepath.IsAbs(failStage) {
		rel, err := filepath.Rel(root, failStage)
		if err != nil {
			return nil, err
		}
		fail = filepath.ToSlash(rel)
	}
	if list == nil {
		list = runGoList
	}
	entries, err := os.ReadDir(filepath.Join(root, "harness", "stages"))
	if err != nil {
		return nil, err
	}
	var findings []lintFinding
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		rel := "harness/stages/" + entry.Name()
		var s stage
		if err := readJSON(filepath.Join(root, rel), &s); err != nil {
			return nil, err
		}
		for _, c := range s.Checks {
			if len(c.RequiredTests) == 0 || !isGoTest(c.Args) {
				continue
			}
			args, err := transformListArgs(c.Args, c.RequiredTests)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", rel, c.ID, err)
			}
			output, err := list(ctx, root, args)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", rel, c.ID, err)
			}
			available := parseListOutput(output)
			for _, test := range c.RequiredTests {
				if available[topName(test)] {
					continue
				}
				severity := "warn"
				if rel == fail {
					severity = "error"
				}
				findings = append(findings, lintFinding{Stage: rel, Check: c.ID, Test: test, Severity: severity})
			}
		}
	}
	return findings, nil
}

// runStageLint 打印预检结果：错误返回 1（fail-closed），基础设施问题返回 2。
// runStageLint prints findings: missing current-stage tests exit 1, infrastructure 2.
func runStageLint(ctx context.Context, root, failStage string, stdout, stderr io.Writer) int {
	findings, err := lintStages(ctx, root, failStage, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	code := 0
	for _, finding := range findings {
		if finding.Severity == "error" {
			fmt.Fprintf(stderr, "ERROR %s %s missing %s\n", finding.Stage, finding.Check, finding.Test)
			code = 1
			continue
		}
		fmt.Fprintf(stdout, "WARN %s %s missing %s\n", finding.Stage, finding.Check, finding.Test)
	}
	if code == 0 {
		fmt.Fprintln(stdout, "stage lint: ok")
	}
	return code
}
