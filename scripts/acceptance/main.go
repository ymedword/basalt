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
	ids, exists := cfg.Profiles[*profile]
	if !exists || !relative(*out) || filepath.Dir(filepath.Clean(*out)) == "." || !isReportOutput(*out) {
		fmt.Fprintln(stderr, "unknown profile or output outside .loop")
		return 2
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
	if s.ID != "" {
		scope += "/" + s.ID
	}
	if *taskID != "" {
		scope += "/" + *taskID
	}
	code, err := accept(ctx, root, filepath.Join(root, *out), scope, checks, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
	}
	return codeOrError(code, err)
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
