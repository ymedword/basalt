package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// checkTests 要求具名 PASS 事件且该名及其子测试都不出现 SKIP；空匹配、父测试放行都算失败。
// checkTests requires a named pass event and rejects skips of that name or its subtests,
// so an empty -run match or an all-skipped parent test cannot pass. It also returns the
// observed pass/skip event names for the published report.
func checkTests(log string, required []string) (passed, skipped []string, err error) {
	dir, err := os.OpenRoot(filepath.Dir(log))
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	f, err := dir.Open(filepath.Base(log))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	passedSet, skippedSet := map[string]bool{}, map[string]bool{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		var event struct{ Action, Test string }
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Test == "" {
			continue
		}
		switch event.Action {
		case "pass":
			if !passedSet[event.Test] {
				passedSet[event.Test] = true
				passed = append(passed, event.Test)
			}
		case "skip":
			if !skippedSet[event.Test] {
				skippedSet[event.Test] = true
				skipped = append(skipped, event.Test)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	for _, name := range required {
		if !passedSet[name] {
			return passed, skipped, fmt.Errorf("required test did not execute and pass: %s", name)
		}
		if skippedSet[name] {
			return passed, skipped, fmt.Errorf("required test was skipped: %s", name)
		}
		for sub := range skippedSet {
			if strings.HasPrefix(sub, name+"/") {
				return passed, skipped, fmt.Errorf("required test skipped a subtest: %s", sub)
			}
		}
	}
	return passed, skipped, nil
}

type result struct {
	ID           string   `json:"id"`
	Level        string   `json:"level"`
	Status       string   `json:"status"`
	ExitCode     int      `json:"exit_code"`
	Log          string   `json:"log"`
	Detail       string   `json:"detail,omitempty"`
	Millis       int64    `json:"duration_ms"`
	PassedTests  []string `json:"passed_tests"`
	SkippedTests []string `json:"skipped_tests"`
}

type acceptanceReport struct {
	Version            int            `json:"version"`
	RunID              string         `json:"run_id"`
	Scope              string         `json:"scope"`
	Fingerprint        string         `json:"fingerprint"`
	BaseCommit         string         `json:"base_commit"`
	Dirty              bool           `json:"dirty"`
	Environment        environment    `json:"environment"`
	Authorization      *authorization `json:"authorization,omitempty"`
	TestedBinarySHA256 *string        `json:"tested_binary_sha256,omitempty"`
	StartedAt          string         `json:"started_at"`
	Status             string         `json:"status"`
	ExitCode           int            `json:"exit_code"`
	Checks             []result       `json:"checks"`
}

var binaryMarker = regexp.MustCompile(`binary_sha256=([0-9a-f]{64})`)

// projectReport 把日志路径投影为仓库相对路径并统一 `/` 分隔；报告落盘后即可跨机复跑核对。
// projectReport rewrites log paths to repo-relative slash-separated form before writing.
func projectReport(root string, rpt acceptanceReport) acceptanceReport {
	projected := rpt
	projected.Checks = slices.Clone(rpt.Checks)
	for i := range projected.Checks {
		if projected.Checks[i].Log == "" {
			continue
		}
		rel, err := filepath.Rel(root, projected.Checks[i].Log)
		if err == nil && relative(rel) {
			projected.Checks[i].Log = filepath.ToSlash(rel)
		}
	}
	return projected
}

// resolveBinaryHash 从 process / external check 的日志中提取唯一 binary_sha256 标记。
// 缺失、多个或跨 check 不一致都按基础设施错误处理，不得降级为 source。
// resolveBinaryHash extracts exactly one binary_sha256 marker from process/external logs;
// missing, duplicated or disagreeing markers are infrastructure errors, never downgraded.
func resolveBinaryHash(rpt *acceptanceReport) error {
	hashes := map[string]bool{}
	needed := false
	for _, r := range rpt.Checks {
		if r.Level != "process" && r.Level != "external" {
			continue
		}
		needed = true
		raw, err := os.ReadFile(r.Log)
		if err != nil {
			return fmt.Errorf("%s: level %s requires a binary_sha256 marker: %w", r.ID, r.Level, err)
		}
		matches := binaryMarker.FindAllSubmatch(raw, -1)
		if len(matches) != 1 {
			return fmt.Errorf("%s: level %s requires exactly one binary_sha256 marker, found %d", r.ID, r.Level, len(matches))
		}
		hashes[string(matches[0][1])] = true
	}
	if !needed {
		return nil
	}
	if len(hashes) != 1 {
		return fmt.Errorf("process/external checks report %d distinct binary_sha256 values", len(hashes))
	}
	for hash := range hashes {
		value := hash
		rpt.TestedBinarySHA256 = &value
	}
	return nil
}

// execute 直执受保护配置中的 argv，显式限制超时并清理进程树；日志不记录环境变量。
// execute uses argv directly, with an explicit timeout and process-tree cleanup.
func execute(ctx context.Context, root, out string, c command, log string) (int, error) {
	args := slices.Clone(c.Args)
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "{out}", out)
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return 2, errors.New("executable unavailable: " + args[0])
	}
	dir, err := os.OpenRoot(filepath.Dir(log))
	if err != nil {
		return 2, err
	}
	defer dir.Close()
	f, err := dir.OpenFile(filepath.Base(log), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 2, err
	}
	defer f.Close()
	budget, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := &exec.Cmd{Path: path, Args: args, Dir: root, Stdout: f, Stderr: f}
	prepareProcess(cmd)
	if err := cmd.Start(); err != nil {
		return 2, errors.New("process could not start; see log")
	}
	stop, err := trackProcess(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 2, fmt.Errorf("process containment failed: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// Also stop descendants left behind by an exited command.
		if cleanupErr := stop(); cleanupErr != nil {
			return 2, cleanupErr
		}
		if err == nil {
			return 0, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 2, errors.New("process wait failed; see log")
	case <-budget.Done():
		cleanupErr := stop()
		<-done
		return 2, errors.Join(fmt.Errorf("command interrupted or timed out: %w", budget.Err()), cleanupErr)
	}
}

// checkEval 拒绝缺失必跑用例、未登记跳过与计数失配，SKIP 永不算 PASS。
// checkEval rejects missing mandatory cases, undeclared skips and inconsistent totals.
func checkEval(dir string, c check) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		return errors.New("expected exactly one fresh eval report")
	}
	var rpt struct {
		Provider string                                       `json:"provider_gate"`
		Totals   struct{ Total, Passed, Failed, Skipped int } `json:"totals"`
		Cases    []struct {
			ID      string              `json:"id"`
			Backend string              `json:"backend"`
			Passed  bool                `json:"passed"`
			Skipped bool                `json:"skipped"`
			Reason  string              `json:"skip_reason"`
			Checks  []struct{ OK bool } `json:"checks"`
		} `json:"cases"`
	}
	raw, err := readLocal(files[0])
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &rpt); err != nil {
		return err
	}
	if rpt.Provider != "" {
		return errors.New("automated acceptance cannot enable real-model providers")
	}
	seen := map[string]bool{}
	passed, skipped := 0, 0
	for _, r := range rpt.Cases {
		if r.ID == "" || seen[r.ID] || (r.Passed && r.Skipped) {
			return errors.New("duplicate/invalid case or contradictory status")
		}
		seen[r.ID] = true
		if r.Skipped {
			if !slices.Contains(c.AllowedSkips, r.ID) || r.Reason == "" || !strings.HasPrefix(r.Backend, "provider:") {
				return fmt.Errorf("unauthorized skip: %s", r.ID)
			}
			skipped++
			continue
		}
		if !r.Passed || len(r.Checks) == 0 || slices.ContainsFunc(r.Checks, func(v struct{ OK bool }) bool { return !v.OK }) || r.Backend != "fake" {
			return fmt.Errorf("case must pass with fake backend and effective assertions: %s", r.ID)
		}
		passed++
	}
	for _, id := range c.Required {
		if !seen[id] {
			return fmt.Errorf("mandatory case missing: %s", id)
		}
	}
	if passed == 0 || rpt.Totals.Total != len(rpt.Cases) || rpt.Totals.Passed != passed || rpt.Totals.Skipped != skipped || rpt.Totals.Failed != 0 {
		return errors.New("empty pass set or inconsistent eval totals")
	}
	return nil
}

func atomicJSON(path string, data any) error {
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	f, err := os.CreateTemp(filepath.Dir(path), ".report-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	encodeErr := json.NewEncoder(f).Encode(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(encodeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := dir.Rename(filepath.Base(f.Name()), filepath.Base(path)); err != nil {
		return err
	}
	return syncDir(dir)
}

func accept(ctx context.Context, root, out, scope string, checks []check, auth *authorization, stdout io.Writer) (int, acceptanceReport, error) {
	hasExternal := slices.ContainsFunc(checks, func(c check) bool { return checkLevel(c) == "external" })
	if hasExternal != (auth != nil) || (auth != nil && (strings.TrimSpace(auth.Ref) == "" || strings.TrimSpace(auth.Scope) == "")) {
		return 2, acceptanceReport{}, errors.New("external checks require matching authorization ref and scope")
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return 2, acceptanceReport{}, err
	}
	defer handle.Close()
	rel, err := filepath.Rel(root, out)
	if err != nil || !relative(rel) {
		return 2, acceptanceReport{}, errors.New("output must stay within repository")
	}
	if err := handle.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
		return 2, acceptanceReport{}, err
	}
	// Fresh directories prevent stale reports from authorizing a new code state.
	if err := handle.Mkdir(rel, 0o700); err != nil {
		return 2, acceptanceReport{}, fmt.Errorf("output must be a fresh directory: %w", err)
	}
	fingerprint, err := sourceFingerprint(root)
	if err != nil {
		return 2, acceptanceReport{}, err
	}
	commit, dirty, err := gitState(root)
	if err != nil {
		return 2, acceptanceReport{}, err
	}
	runID := filepath.Base(filepath.Clean(out))
	rpt := acceptanceReport{
		Version:       1,
		RunID:         runID,
		Scope:         scope,
		Fingerprint:   fingerprint,
		BaseCommit:    commit,
		Dirty:         dirty,
		Environment:   currentEnvironment(),
		Authorization: auth,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		Status:        "running",
		Checks:        []result{},
	}
	writeProgress := func() error {
		partial := projectReport(root, rpt)
		partial.Status, partial.ExitCode = "running", 2
		return atomicJSON(filepath.Join(out, "report.json"), &partial)
	}
	for _, c := range checks {
		rpt.Checks = append(rpt.Checks, result{ID: c.ID, Level: checkLevel(c), Status: "not_run", ExitCode: -1, PassedTests: []string{}, SkippedTests: []string{}})
	}
	if err := writeProgress(); err != nil {
		return 2, acceptanceReport{}, err
	}
	for i, c := range checks {
		// Cancellation between two checks must not leave an earlier pass authorizing the run.
		if ctx.Err() != nil {
			rpt.Status, rpt.ExitCode = "error", 2
			break
		}
		r := result{ID: c.ID, Level: checkLevel(c), Status: "passed", Log: filepath.Join(out, c.ID+".log"), PassedTests: []string{}, SkippedTests: []string{}}
		start := time.Now()
		code, runErr := execute(ctx, root, out, c.command, r.Log)
		r.ExitCode = code
		if runErr != nil {
			r.Detail, r.Status, rpt.ExitCode = runErr.Error(), "error", 2
		} else if code != 0 {
			r.Status = "failed"
			if rpt.ExitCode == 0 {
				rpt.ExitCode = 1
			}
			// make 也用 2 表示 recipe 失败，因此只有评测二进制按基础设施错误处理。
			// make also exits 2 on recipe failure; only the eval binary means infrastructure.
			if c.EvalReport != "" && code == 2 {
				r.Status, rpt.ExitCode = "error", 2
			}
		} else if c.EvalReport != "" {
			if err := checkEval(filepath.Join(out, c.EvalReport), c); err != nil {
				r.Status, r.Detail = "failed", err.Error()
				if rpt.ExitCode == 0 {
					rpt.ExitCode = 1
				}
			}
		}
		if len(c.RequiredTests) > 0 {
			passed, skipped, err := checkTests(r.Log, c.RequiredTests)
			r.PassedTests, r.SkippedTests = passed, skipped
			if err != nil && r.Status == "passed" {
				r.Status, r.Detail = "failed", err.Error()
				if rpt.ExitCode == 0 {
					rpt.ExitCode = 1
				}
			}
		}
		r.Millis = time.Since(start).Milliseconds()
		rpt.Checks[i] = r
		if rpt.ExitCode != 0 {
			rpt.Status = "failed"
		}
		if rpt.ExitCode == 2 {
			rpt.Status = "error"
		}
		fmt.Fprintf(stdout, "%s %s (exit=%d) %s\n", r.Status, r.ID, r.ExitCode, r.Detail)
		if err := writeProgress(); err != nil {
			return 2, acceptanceReport{}, err
		}
		if ctx.Err() != nil || r.Status == "error" {
			break
		}
	}
	if rpt.Status != "error" && ctx.Err() == nil {
		if err := resolveBinaryHash(&rpt); err != nil {
			fmt.Fprintf(stdout, "error: %s\n", err)
			rpt.Status, rpt.ExitCode = "error", 2
		}
	}
	after, err := sourceFingerprint(root)
	if err != nil {
		return 2, acceptanceReport{}, err
	}
	if after != fingerprint {
		rpt.Status, rpt.ExitCode = "source_changed", 2
	}
	// A cancel that arrives after the last check still means the run did not complete.
	if ctx.Err() != nil {
		rpt.Status, rpt.ExitCode = "error", 2
	}
	if rpt.ExitCode == 0 {
		rpt.Status = "passed"
	}
	final := projectReport(root, rpt)
	if err := atomicJSON(filepath.Join(out, "report.json"), &final); err != nil {
		return 2, acceptanceReport{}, err
	}
	fmt.Fprintf(stdout, "acceptance %s: %s exit=%d report=%s\n", scope, rpt.Status, rpt.ExitCode, filepath.Join(out, "report.json"))
	return rpt.ExitCode, final, nil
}
