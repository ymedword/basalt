package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCommandHelper is a real child-process adapter, not an external model.
func TestCommandHelper(t *testing.T) {
	idx := -1
	for i, arg := range os.Args {
		if arg == "--harness-helper" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	mode := os.Args[idx+1]
	switch mode {
	case "pass":
		os.Exit(0)
	case "fail":
		os.Exit(1)
	case "infra":
		os.Exit(2)
	case "sleep":
		time.Sleep(30 * time.Second)
	case "heartbeat":
		for n := 0; n < 600; n++ {
			require.NoError(t, os.WriteFile("heartbeat.txt", []byte(strconv.Itoa(n)), 0o600))
			time.Sleep(25 * time.Millisecond)
		}
	case "tree", "tree-exit":
		child := exec.Command(os.Args[0], "-test.run=^TestCommandHelper$", "--", "--harness-helper", "heartbeat")
		require.NoError(t, child.Start())
		for n := 0; n < 100; n++ {
			if _, err := os.Stat("heartbeat.txt"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if mode == "tree" {
			time.Sleep(30 * time.Second)
		}
	case "touch":
		require.NoError(t, os.WriteFile("changed.txt", []byte("changed"), 0o600))
	case "check":
		raw, _ := os.ReadFile("artifact.txt")
		if string(raw) != "good" {
			os.Exit(1)
		}
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func helperCommand(t *testing.T, mode string) command {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	return command{Args: []string{exe, "-test.run=^TestCommandHelper$", "--", "--harness-helper", mode}, TimeoutSeconds: 10}
}

func fixture(t *testing.T) (string, configuration, stage) {
	t.Helper()
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	root := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", root)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".loop/\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "input.md"), []byte("approved input"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".loop/runs"), 0o700))
	cfg := configuration{Version: 1, Profiles: map[string][]string{"full": {"base"}, "task": {"base"}}, Checks: []check{{ID: "base", command: helperCommand(t, "pass")}}}
	s := stage{Version: 1, ID: "us-999-r1", Approval: "fixture approval", Inputs: []string{"input.md"}, Checks: []check{{ID: "contract", command: helperCommand(t, "check")}}, Tasks: []task{{ID: "T1", Checks: []string{"contract"}}}}
	require.NoError(t, atomicJSON(filepath.Join(root, "config.json"), cfg))
	require.NoError(t, atomicJSON(filepath.Join(root, "stage.json"), s))
	return root, cfg, s
}

func TestAccept_FailureInfrastructureAndFreshEvidence(t *testing.T) {
	root, _, _ := fixture(t)
	out := filepath.Join(root, ".loop/accept")
	checks := []check{{ID: "failed", command: helperCommand(t, "fail")}, {ID: "passed", command: helperCommand(t, "pass")}}
	code, err := accept(context.Background(), root, out, "fixture", checks, io.Discard)
	require.NoError(t, err)
	require.Equal(t, 1, code)
	var rpt acceptanceReport
	require.NoError(t, readJSON(filepath.Join(out, "report.json"), &rpt))
	require.Len(t, rpt.Checks, 2)
	require.Equal(t, "failed", rpt.Checks[0].Status)
	_, err = accept(context.Background(), root, out, "fixture", checks, io.Discard)
	require.ErrorContains(t, err, "fresh")
	checks[0].Args[0] = "this-executable-must-not-exist-basalt"
	code, err = accept(context.Background(), root, out+"-infra", "fixture", checks, io.Discard)
	require.NoError(t, err)
	require.Equal(t, 2, code)
}

func TestExecute_CancellationStopsChild(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	code, err := execute(ctx, root, root, helperCommand(t, "sleep"), filepath.Join(root, "cancel.log"))
	require.Error(t, err)
	require.Equal(t, 2, code)
	require.Less(t, time.Since(start), 5*time.Second)
}

func TestExecute_CleansDescendantsOnCancelAndParentExit(t *testing.T) {
	for _, mode := range []string{"tree", "tree-exit"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GORACE", "atexit_sleep_ms=0")
			root := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan int, 1)
			go func() {
				code, _ := execute(ctx, root, root, helperCommand(t, mode), filepath.Join(root, "tree.log"))
				done <- code
			}()
			require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(root, "heartbeat.txt")); return err == nil }, 5*time.Second, 20*time.Millisecond)
			if mode == "tree" {
				cancel()
			}
			select {
			case code := <-done:
				if mode == "tree" {
					require.Equal(t, 2, code)
				} else {
					require.Zero(t, code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("process tree cleanup timed out")
			}
			first, err := os.ReadFile(filepath.Join(root, "heartbeat.txt"))
			require.NoError(t, err)
			time.Sleep(100 * time.Millisecond)
			second, err := os.ReadFile(filepath.Join(root, "heartbeat.txt"))
			require.NoError(t, err)
			require.Equal(t, string(first), string(second), "descendant is still executing")
		})
	}
}

func TestHashPaths_NormalizesTaskCheckboxesAndIgnoresRunState(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "specs/001")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	tasks := filepath.Join(dir, "tasks.md")
	require.NoError(t, os.WriteFile(tasks, []byte("- [ ] T001 do work\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "run-state.md"), []byte("ledger v1\n"), 0o600))
	first, err := hashPaths(root, []string{"specs/001"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tasks, []byte("- [x] T001 do work\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "run-state.md"), []byte("ledger v2\n"), 0o600))
	second, err := hashPaths(root, []string{"specs/001"})
	require.NoError(t, err)
	require.Equal(t, first, second, "checkbox state and run-state are process state")
	require.NoError(t, os.WriteFile(tasks, []byte("- [x] T001 changed scope\n"), 0o600))
	third, err := hashPaths(root, []string{"specs/001"})
	require.NoError(t, err)
	require.NotEqual(t, first, third, "task text changes still count")
}

func TestAccept_CanceledContextNeverPasses(t *testing.T) {
	root, _, _ := fixture(t)
	out := filepath.Join(root, ".loop/runs/canceled")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, err := accept(ctx, root, out, "fixture", []check{{ID: "late", command: helperCommand(t, "pass")}}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, 2, code)
	var rpt acceptanceReport
	require.NoError(t, readJSON(filepath.Join(out, "report.json"), &rpt))
	require.Equal(t, "error", rpt.Status)
	require.Len(t, rpt.Checks, 1)
	require.Equal(t, "not_run", rpt.Checks[0].Status)
}

func TestAccept_PlainExitTwoStaysCheckFailure(t *testing.T) {
	root, _, _ := fixture(t)
	out := filepath.Join(root, ".loop/runs/cmd-exit2")
	// make 对 recipe 失败也返回 2；普通检查的 2 不得被当成基础设施错误而中止整轮。
	checks := []check{{ID: "make-like", command: helperCommand(t, "infra")}, {ID: "later", command: helperCommand(t, "pass")}}
	code, err := accept(context.Background(), root, out, "fixture", checks, io.Discard)
	require.NoError(t, err)
	require.Equal(t, 1, code)
	var rpt acceptanceReport
	require.NoError(t, readJSON(filepath.Join(out, "report.json"), &rpt))
	require.Equal(t, "failed", rpt.Status)
	require.Equal(t, "failed", rpt.Checks[0].Status)
	require.Equal(t, "passed", rpt.Checks[1].Status)
}

func TestAccept_RejectsSourceChangedDuringChecks(t *testing.T) {
	root, _, _ := fixture(t)
	out := filepath.Join(root, ".loop/runs/changed")
	code, err := accept(context.Background(), root, out, "fixture", []check{{ID: "mutation", command: helperCommand(t, "touch")}}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, 2, code)
	var rpt acceptanceReport
	require.NoError(t, readJSON(filepath.Join(out, "report.json"), &rpt))
	require.Equal(t, "source_changed", rpt.Status)
}

func TestAccept_InProgressReportCannotAuthorizeAndInterruptedChecksAreListed(t *testing.T) {
	root, _, _ := fixture(t)
	out := filepath.Join(root, ".loop/runs/interrupt")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks := []check{{ID: "slow", command: helperCommand(t, "sleep")}, {ID: "later", command: helperCommand(t, "pass")}}
	done := make(chan int, 1)
	go func() { code, _ := accept(ctx, root, out, "fixture", checks, io.Discard); done <- code }()
	require.Eventually(t, func() bool {
		var rpt acceptanceReport
		return readJSON(filepath.Join(out, "report.json"), &rpt) == nil && rpt.Status == "running" && rpt.ExitCode == 2
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case code := <-done:
		require.Equal(t, 2, code)
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop checks")
	}
	var rpt acceptanceReport
	require.NoError(t, readJSON(filepath.Join(out, "report.json"), &rpt))
	require.Equal(t, "error", rpt.Status)
	require.Len(t, rpt.Checks, 2)
	require.Equal(t, "not_run", rpt.Checks[1].Status)
}

func TestRequiredGoTests_EmptyMatchAndSkipCannotPass(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		ok            bool
	}{
		{"pass", `{"Action":"pass","Test":"TestContract"}`, true},
		{"empty", `{"Action":"pass","Package":"example"}`, false},
		{"skip", `{"Action":"skip","Test":"TestContract"}`, false},
		{"wrong-name", `{"Action":"pass","Test":"TestOther"}`, false},
		// 子测试全 SKIP 时父测试仍报 pass：必须按跳过拒绝。
		{"parent-all-skipped", `{"Action":"skip","Test":"TestContract/case"}` + "\n" + `{"Action":"pass","Test":"TestContract"}`, false},
		{"unrelated-skip", `{"Action":"skip","Test":"TestOther/case"}` + "\n" + `{"Action":"pass","Test":"TestContract"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "test.log")
			require.NoError(t, os.WriteFile(log, []byte(tc.content+"\n"), 0o600))
			err := checkTests(log, []string{"TestContract"})
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestEvalPolicy_RejectsMissingSkippedAndFalseGreen(t *testing.T) {
	valid := `{"provider_gate":"","totals":{"total":2,"passed":1,"failed":0,"skipped":1},"cases":[{"id":"mandatory","backend":"fake","passed":true,"checks":[{"ok":true}]},{"id":"real","backend":"provider:deepseek","skipped":true,"skip_reason":"not enabled"}]}`
	for _, tc := range []struct {
		name, raw string
		ok        bool
	}{
		{"valid", valid, true},
		{"missing", strings.Replace(valid, `"mandatory"`, `"renamed"`, 1), false},
		{"mandatory-skipped", strings.Replace(valid, `"passed":true`, `"skipped":true,"skip_reason":"opt out"`, 1), false},
		{"false-green", strings.Replace(valid, `"ok":true`, `"ok":false`, 1), false},
		{"duplicate", strings.Replace(valid, `"id":"real"`, `"id":"mandatory"`, 1), false},
		{"wrong-total", strings.Replace(valid, `"total":2`, `"total":3`, 1), false},
		{"real-enabled", strings.Replace(valid, `"provider_gate":""`, `"provider_gate":"deepseek"`, 1), false},
		{"unapproved-skip", strings.Replace(valid, `"id":"real"`, `"id":"unknown"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "report.json"), []byte(tc.raw), 0o600))
			err := checkEval(dir, check{Required: []string{"mandatory"}, AllowedSkips: []string{"real"}})
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	// A successful process without a fresh report must fail closed.
	require.Error(t, checkEval(t.TempDir(), check{Required: []string{"mandatory"}}))
}

func TestConfiguration_ValidatesFutureStageAndRejectsGlobalOnlyTask(t *testing.T) {
	root, cfg, s := fixture(t)
	_, err := loadConfiguration(root, "config.json")
	require.NoError(t, err)
	loaded, err := loadStage(root, "stage.json", cfg)
	require.NoError(t, err)
	require.Equal(t, "us-999-r1", loaded.ID) // No upper bound for future US stages.
	s.Tasks[0].Checks = []string{"base"}
	require.NoError(t, atomicJSON(filepath.Join(root, "stage.json"), s))
	_, err = loadStage(root, "stage.json", cfg)
	require.ErrorContains(t, err, "specific assertion")
	require.Error(t, readJSON(filepath.Join(root, "absent"), new(any)))
	require.NoError(t, os.WriteFile(filepath.Join(root, "unknown.json"), []byte(`{"unknown":1}`), 0o600))
	require.Error(t, readJSON(filepath.Join(root, "unknown.json"), &s))
	for _, p := range []string{"../escape", "/absolute", "D:/other", "."} {
		require.False(t, relative(p), fmt.Sprint(p))
	}
	s.Approval = "REPLACE: template placeholder"
	require.NoError(t, atomicJSON(filepath.Join(root, "stage.json"), s))
	_, err = loadStage(root, "stage.json", cfg)
	require.ErrorContains(t, err, "placeholder")
	s.Approval = "fixture approval"
	s.Checks[0].command = command{Args: []string{"go", "-C", ".", "test", "./..."}, TimeoutSeconds: 10}
	require.NoError(t, atomicJSON(filepath.Join(root, "stage.json"), s))
	_, err = loadStage(root, "stage.json", cfg)
	require.ErrorContains(t, err, "named required_tests")
	s.Checks[0].command = helperCommand(t, "check")
	s.Inputs = append(s.Inputs, "./.loop/state/escape")
	require.NoError(t, atomicJSON(filepath.Join(root, "stage.json"), s))
	_, err = loadStage(root, "stage.json", cfg)
	require.ErrorContains(t, err, "invalid input/protected path")
}

func TestMainRun_ArgumentErrorsAndLock(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"accept"}, {"--unknown"}, {"extra"}} {
		require.Equal(t, 2, mainRun(context.Background(), args, io.Discard, io.Discard))
	}
	root, _, _ := fixture(t)
	t.Chdir(root)
	require.NoError(t, os.MkdirAll("harness", 0o700))
	var cfg configuration
	require.NoError(t, readJSON("config.json", &cfg))
	require.NoError(t, atomicJSON("harness/acceptance.json", cfg))
	require.NoError(t, os.MkdirAll(".loop", 0o700))
	require.NoError(t, os.WriteFile(".loop/lock", []byte("owned by another run"), 0o600))
	require.Equal(t, 2, mainRun(context.Background(), nil, io.Discard, io.Discard))
	require.NoError(t, os.Remove(".loop/lock"))
	require.Zero(t, mainRun(context.Background(), []string{"--out", ".loop/runs/test"}, io.Discard, io.Discard))
	require.Equal(t, 2, mainRun(context.Background(), []string{"--out", "../escape"}, io.Discard, io.Discard))
	var rpt acceptanceReport
	require.NoError(t, readJSON(filepath.Join(root, ".loop/runs/test/report.json"), &rpt))
	raw, err := json.Marshal(rpt)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "environment")
}

// TestMainRun_FullIncludesUnboundStageCheck 防止移除自动执行器后漏掉跨任务验收。
// TestMainRun_FullIncludesUnboundStageCheck keeps cross-task checks in full acceptance.
func TestMainRun_FullIncludesUnboundStageCheck(t *testing.T) {
	root, cfg, s := fixture(t)
	t.Chdir(root)
	require.NoError(t, os.MkdirAll("harness", 0o700))
	require.NoError(t, atomicJSON("harness/acceptance.json", cfg))
	require.NoError(t, os.WriteFile("artifact.txt", []byte("good"), 0o600))
	s.Checks = append(s.Checks, check{ID: "cross-task", command: helperCommand(t, "fail")})
	require.NoError(t, atomicJSON("stage.json", s))
	require.Zero(t, mainRun(context.Background(), []string{"--stage", "stage.json", "--task", "T1", "--out", ".loop/runs/task"}, io.Discard, io.Discard))
	require.Equal(t, 1, mainRun(context.Background(), []string{"--stage", "stage.json", "--out", ".loop/runs/full"}, io.Discard, io.Discard))
	var rpt acceptanceReport
	require.NoError(t, readJSON(".loop/runs/full/report.json", &rpt))
	require.Equal(t, "cross-task", rpt.Checks[2].ID)
	require.Equal(t, "failed", rpt.Checks[2].Status)
}

// TestStage_RejectsRemovedAgentDriver 避免旧配置被静默当成仍可驱动开发。
// TestStage_RejectsRemovedAgentDriver rejects obsolete automation fields explicitly.
func TestStage_RejectsRemovedAgentDriver(t *testing.T) {
	root, cfg, _ := fixture(t)
	raw, err := os.ReadFile(filepath.Join(root, "stage.json"))
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	for _, name := range []string{"driver", "max_attempts"} {
		t.Run(name, func(t *testing.T) {
			fields[name] = 1
			require.NoError(t, atomicJSON(filepath.Join(root, "legacy.json"), fields))
			_, err := loadStage(root, "legacy.json", cfg)
			require.ErrorContains(t, err, "unknown field")
			delete(fields, name)
		})
	}
}
