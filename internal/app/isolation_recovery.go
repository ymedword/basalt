package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

func isolationSegment(s string) bool {
	return s != "" && s != "." && s != ".." && len(s) <= 200 && filepath.IsLocal(s) && !strings.ContainsAny(s, "/\\:\x00\r\n")
}

func cleanupIsolationOrphans(ctx context.Context, runners map[string]*tool.ContainerRunner, deps isolationDeps) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var primary *tool.ContainerRunner
	for _, name := range []string{"oci", "gvisor"} {
		if runners[name] != nil {
			primary = runners[name]
			break
		}
	}
	if primary == nil {
		return errors.New("恢复缺少容器执行器")
	}
	rows, err := deps.Reader.PendingSandboxExecutions(ctx, deps.InstanceID)
	if err != nil {
		return err
	}
	if len(rows) > 1024 {
		return errors.New("遗留执行超过有界恢复批次，请运维先处置")
	}
	handled := map[string]bool{}
	protected := map[string]bool{}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		taskName := tool.SandboxContainerName(deps.InstanceID, row.ToolInvocationID)
		cleanupName := tool.CleanupContainerName(deps.InstanceID, row.ToolInvocationID)
		handled[taskName] = true
		handled[cleanupName] = true
		rel := filepath.Join(row.RunID, strconv.FormatInt(row.ToolInvocationID, 10))
		protected[rel] = true
		var rowErr error
		if row.ContainerName != taskName || row.ToolInvocationID <= 0 || !isolationSegment(row.RunID) {
			rowErr = errors.New("账本执行身份不合法")
		} else {
			_, taskErr := primary.RemoveByName(ctx, taskName)
			_, cleanupErr := primary.RemoveByName(ctx, cleanupName)
			rowErr = errors.Join(taskErr, cleanupErr)
			if rowErr == nil {
				rowErr = removeRecoveryOutput(ctx, deps, runners, row, rel)
				if stageErr := removeIsolationPath(deps.Root, filepath.Join("sandbox", rel)); stageErr != nil {
					rowErr = errors.Join(rowErr, stageErr)
				}
			}
		}
		cleanup := core.CleanupCleaned
		reason := "orphan_cleaned"
		if rowErr != nil {
			cleanup = core.CleanupFailed
			reason = "cleanup_failed"
		}
		state := row.State
		switch state {
		case core.SandboxFinished, core.SandboxCanceled, core.SandboxTimeout, core.SandboxFailed, core.SandboxUnknown:
		default:
			state = core.SandboxUnknown
		}
		if err := deps.Store.MarkSandboxCleaned(ctx, row.ToolInvocationID, state, cleanup, reason, time.Now().UTC()); err != nil {
			rowErr = errors.Join(rowErr, err)
		}
		if rowErr != nil {
			logger.Warn("遗留隔离执行未完成清理，继续占用容量", "tool_invocation_id", row.ToolInvocationID, "error", rowErr)
		}
	}
	// 已处理的账本行即使失败也不在孤儿扫描中二次判死；基线会继续扣除它们。
	// Handled ledger rows are never retried as untracked orphans; baseline occupancy retains failures.
	names, err := primary.ListByLabel(ctx, "basalt.instance_id="+deps.InstanceID)
	if err != nil {
		return fmt.Errorf("枚举孤儿容器失败: %w", err)
	}
	if len(names) > 2048 {
		return errors.New("孤儿容器超过恢复上限")
	}
	for _, name := range names {
		if handled[name] {
			continue
		}
		if _, err := primary.RemoveByName(ctx, name); err != nil {
			return fmt.Errorf("无账本孤儿容器清理失败: %w", err)
		}
	}
	// 覆盖已建目录、尚未写账本的崩溃窗口；有账本的失败目录由上面的分支保留。
	// Sweep the directory-before-ledger crash window while retaining tracked failed directories.
	if err := sweepIsolationDirectories(ctx, filepath.Join(deps.Root, "sandbox"), protected); err != nil {
		return err
	}
	return sweepIsolationDirectories(ctx, deps.Config.OutputRoot, protected)
}

func removeRecoveryOutput(ctx context.Context, deps isolationDeps, runners map[string]*tool.ContainerRunner, row core.SandboxExecution, rel string) error {
	err := removeIsolationPath(deps.Config.OutputRoot, rel)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrPermission) {
		return err
	}
	runner := runners[row.Profile]
	profile, ok := deps.Config.Profiles[row.Profile]
	if runner == nil || !ok || profile.Runtime != row.Runtime || isolationImageDigest(profile.Image) != row.ImageDigest {
		return errors.New("旧执行档或镜像已变，不能用另一档清理遗留输出")
	}
	return runner.RemoveOutputDir(ctx, filepath.Join(deps.Config.OutputRoot, rel))
}

func removeIsolationPath(rootPath, rel string) error {
	root, err := os.OpenRoot(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(rel)
}

func limitedDirectory(root *os.Root, path string) ([]os.DirEntry, error) {
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	entries, readErr := f.ReadDir(1025)
	closeErr := f.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > 1024 {
		return nil, errors.New("孤儿目录超过恢复上限")
	}
	return entries, nil
}

func sweepIsolationDirectories(ctx context.Context, path string, protected map[string]bool) error {
	root, err := os.OpenRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	nonempty := 0
	for {
		runs, readErr := dir.ReadDir(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, run := range runs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !run.IsDir() {
				nonempty++
				if nonempty > 1024 {
					return errors.New("孤儿目录超过恢复上限")
				}
				if err := root.RemoveAll(run.Name()); err != nil {
					return err
				}
				continue
			}
			children, err := limitedDirectory(root, run.Name())
			if err != nil {
				return err
			}
			if len(children) > 0 {
				nonempty++
				if nonempty > 1024 {
					return errors.New("孤儿目录超过恢复上限")
				}
			}
			for _, child := range children {
				rel := filepath.Join(run.Name(), child.Name())
				if protected[rel] {
					continue
				}
				if err := root.RemoveAll(rel); err != nil {
					return fmt.Errorf("孤儿目录清理失败: %w", err)
				}
			}
			remaining, err := limitedDirectory(root, run.Name())
			if err != nil {
				return err
			}
			if len(remaining) == 0 {
				if err := root.Remove(run.Name()); err != nil {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}
