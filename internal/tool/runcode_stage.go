package tool

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

import "github.com/ymedword/basalt/internal/core"

type runCodeStage struct{ StageDir, TaskDir, OutputDir string }

func (t *RunCodeTool) stageRunCode(ctx context.Context, env core.ToolEnv, in runCodeInput) (runCodeStage, error) {
	var stage runCodeStage
	if len(in.Code) > t.opts.MaxCodeBytes || len(in.Inputs) > t.opts.MaxInputFiles || int64(len(in.Code)) > t.opts.MaxInputBytes {
		return stage, runCodeInputError("暂存代码或输入预算超限")
	}
	if err := ctx.Err(); err != nil {
		return stage, err
	}
	root, err := os.OpenRoot(t.opts.Root)
	if err != nil {
		return stage, err
	}
	defer root.Close()
	rel := filepath.Join("sandbox", env.RunID, strconv.FormatInt(env.ToolInvocationID, 10))
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return stage, err
	}
	if err := root.Mkdir(rel, 0o755); err != nil {
		return stage, err
	}
	stage.StageDir = filepath.Join(t.opts.Root, rel)
	stage.TaskDir = filepath.Join(stage.StageDir, "task")
	if err := root.MkdirAll(filepath.Join(rel, "task", "input"), 0o755); err != nil {
		return stage, err
	}
	for _, p := range []string{"sandbox", filepath.Join("sandbox", env.RunID), rel, filepath.Join(rel, "task"), filepath.Join(rel, "task", "input")} {
		if err := root.Chmod(p, 0o755); err != nil {
			return stage, err
		}
	}
	if err := writeStageFile(root, filepath.Join(rel, "task", "main.sh"), []byte(in.Code)); err != nil {
		return stage, err
	}
	used := int64(len(in.Code))
	names := map[string]bool{}
	for _, input := range in.Inputs {
		if err := ctx.Err(); err != nil {
			return stage, err
		}
		target := resolveToolPath(t.opts.Root, env.AgentDir, input)
		if err := t.opts.Policy.Check(ctx, core.Action{Type: core.ActionFileRead, Target: actionTarget(t.opts.Root, target), Agent: env.AgentName, RunBound: env.RunBound, SkillDirs: env.SkillDirs}); err != nil {
			return stage, runCodeInputError("输入读取未获授权")
		}
		real, err := resolveRealPath(t.opts.Root, target)
		if err != nil {
			return stage, runCodeInputError("输入路径无法解析")
		}
		inputRel, err := filepath.Rel(t.opts.Root, real)
		if err != nil {
			return stage, runCodeInputError("输入越出工作区")
		}
		name := filepath.Base(real)
		if !safeExecutionSegment(name) || names[name] {
			return stage, runCodeInputError("输入文件名重复或无效")
		}
		names[name] = true
		body, err := readStageInput(root, inputRel, t.opts.MaxInputBytes-used)
		if err != nil {
			return stage, runCodeInputError("输入不是可读普通文件或累计字节超限")
		}
		used += int64(len(body))
		if err := writeStageFile(root, filepath.Join(rel, "task", "input", name), body); err != nil {
			return stage, err
		}
	}
	output, err := os.OpenRoot(t.opts.OutputRoot)
	if err != nil {
		return stage, err
	}
	defer output.Close()
	if err := output.MkdirAll(env.RunID, 0o755); err != nil {
		return stage, err
	}
	if err := output.Chmod(env.RunID, 0o755); err != nil {
		return stage, err
	}
	outRel := filepath.Join(env.RunID, strconv.FormatInt(env.ToolInvocationID, 10))
	if err := output.Mkdir(outRel, 0o755); err != nil {
		return stage, err
	}
	stage.OutputDir = filepath.Join(t.opts.OutputRoot, outRel)
	// 显式 chmod 抵消 umask，只放宽本次输出叶目录。
	// Explicit chmod counters umask on this execution's output leaf only.
	if err := os.Chmod(stage.OutputDir, 0o777); err != nil {
		return stage, err
	} // #nosec G302 -- 单次输出叶目录需要跨 UID 写入
	return stage, nil
}

func writeStageFile(root *os.Root, name string, body []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(body)
	chmodErr := f.Chmod(0o644)
	return errors.Join(writeErr, chmodErr, f.Close())
}

func readStageInput(root *os.Root, name string, remaining int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > remaining {
		return nil, errors.New("输入类型或大小不合法")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("输入在读取前改变类型")
	}
	body, err := io.ReadAll(io.LimitReader(f, remaining+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > remaining {
		return nil, errors.New("累计暂存超限")
	}
	return body, nil
}

func releaseStage(env core.ToolEnv, runner core.CommandRunner, stage runCodeStage, execution *core.CommandExecution) core.CleanupState {
	if execution == nil || execution.CleanupState != core.CleanupCleaned {
		return core.CleanupFailed
	}
	var errs []error
	if stage.OutputDir != "" {
		if cleaner, ok := runner.(interface {
			RemoveOutputDir(context.Context, string) error
		}); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			errs = append(errs, cleaner.RemoveOutputDir(ctx, stage.OutputDir))
			cancel()
		} else {
			errs = append(errs, os.RemoveAll(stage.OutputDir))
		}
	}
	if stage.StageDir != "" {
		errs = append(errs, os.RemoveAll(stage.StageDir))
	}
	if errors.Join(errs...) != nil {
		return core.CleanupFailed
	}
	// 空 Run 父目录不携带执行资源，尽力回收；同 Run 其他调用仍占用时 Remove 不会递归。
	// Remove only empty run parents; a sibling invocation is never recursively removed.
	if safeExecutionSegment(env.RunID) {
		for _, leaf := range []string{stage.OutputDir, stage.StageDir} {
			if leaf != "" && filepath.Base(filepath.Dir(leaf)) == env.RunID {
				_ = os.Remove(filepath.Dir(leaf))
			}
		}
	}
	return core.CleanupCleaned
}
