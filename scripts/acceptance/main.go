package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(mainRun(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func mainRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("acceptance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "full", "acceptance profile")
	stagePath := fs.String("stage", "", "approved stage JSON inside repository")
	taskID := fs.String("task", "", "task ID; without task runs every stage assertion")
	out := fs.String("out", ".loop/runs/accept-"+time.Now().UTC().Format("20060102T150405.000000000"), "fresh report directory below .loop")
	stageOnly := fs.Bool("stage-only", false, "run only stage checks without the profile")
	lintStagesFlag := fs.Bool("lint-stages", false, "check that stage required tests exist; read-only")
	executor := fs.String("executor", "", "record executor: ai | user | ci")
	runKind := fs.String("run-kind", "", "record run kind: original | rerun | backfill")
	authorizationRef := fs.String("authorization-ref", "", "runtime authorization reference for external checks")
	authorizationScope := fs.String("authorization-scope", "", "runtime authorization scope for external checks")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	explicitProfile := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "profile" {
			explicitProfile = true
		}
	})
	if *taskID != "" && !explicitProfile {
		*profile = "task"
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	// stage lint 只读：在配置加载与独占锁之前返回，不产生报告或记录。
	// Stage lint is read-only and returns before config loading and the exclusive lock.
	if *lintStagesFlag {
		return runStageLint(ctx, root, *stagePath, stdout, stderr)
	}
	if *stageOnly && explicitProfile {
		fmt.Fprintln(stderr, "--stage-only cannot be combined with --profile")
		return 2
	}
	if *stageOnly && *stagePath == "" {
		fmt.Fprintln(stderr, "--stage-only requires --stage")
		return 2
	}
	cfgPath := "harness/acceptance.json"
	cfg, err := loadConfiguration(root, cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var s stage
	if *stagePath != "" {
		s, err = loadStage(root, *stagePath, cfg)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ".loop"), 0o700); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	lockPath := filepath.Join(root, ".loop", "lock")
	lockRoot, err := os.OpenRoot(filepath.Dir(lockPath))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	defer lockRoot.Close()
	lock, err := lockRoot.OpenFile("lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "exclusive harness lock unavailable; ensure no other run is active. After a crash, inspect .loop/lock before removing it")
		return 2
	}
	defer os.Remove(lockPath)
	_, writeErr := fmt.Fprintf(lock, "pid=%d started=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := errors.Join(writeErr, lock.Close()); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if !relative(*out) || filepath.Dir(filepath.Clean(*out)) == "." || !isReportOutput(*out) {
		fmt.Fprintln(stderr, "output outside .loop")
		return 2
	}
	var ids []string
	if !*stageOnly {
		var exists bool
		ids, exists = cfg.Profiles[*profile]
		if !exists {
			fmt.Fprintln(stderr, "unknown profile")
			return 2
		}
	}
	if *taskID != "" && *stagePath == "" {
		fmt.Fprintln(stderr, "--task requires --stage")
		return 2
	}
	if *stagePath != "" {
		// 完整验收含未绑定任务的阶段断言，避免漏掉跨任务回归。
		// Full acceptance includes unbound stage checks as cross-task regressions.
		if *taskID == "" {
			for _, c := range s.Checks {
				ids = append(ids, c.ID)
			}
		}
		found := false
		for _, t := range s.Tasks {
			if *taskID == "" || t.ID == *taskID {
				ids = append(ids, t.Checks...)
				found = true
			}
		}
		if !found {
			fmt.Fprintln(stderr, "unknown stage task")
			return 2
		}
		cfg.Checks = append(cfg.Checks, s.Checks...)
	}
	checks, err := selectChecks(cfg.Checks, ids)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	scope := *profile
	if *stageOnly {
		scope = "stage-only"
	}
	if s.ID != "" {
		scope += "/" + s.ID
	}
	if *taskID != "" {
		scope += "/" + *taskID
	}
	auth, err := resolveAuthorization(authorizationFlags{Ref: *authorizationRef, Scope: *authorizationScope}, checks)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	code, rep, err := accept(ctx, root, filepath.Join(root, *out), scope, checks, auth, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return codeOrError(code, err)
	}
	// 基础设施错误 / 取消只留 report.json 与日志，不生成 record.json（C-04）。
	// Infrastructure errors and cancellation leave only report.json and logs (C-04).
	if rep.Status == "error" || ctx.Err() != nil {
		return 2
	}
	rec, err := buildRecord(root, *out, scope, rep, cfg, &s, recordOptions{
		Executor:      *executor,
		RunKind:       *runKind,
		Authorization: auth,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := validateRecord(root, rec); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := writeRecord(filepath.Join(root, *out), rec); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	return code
}

func codeOrError(code int, err error) int {
	if err != nil {
		return 2
	}
	return code
}

func isReportOutput(path string) bool {
	rel, err := filepath.Rel(".loop", filepath.Clean(path))
	return err == nil && relative(rel) && filepath.Dir(rel) == "runs"
}
