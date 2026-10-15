package app

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/storage"
)

// Version 是构建期注入的版本标识的交接点。
// Version is the hand-off point for the build-time version stamp.
//
// 入口包在调用 Run 之前把 main.version（经 -X main.version 注入）赋给它，Run 再
// 交给根命令的 --version。这样版本只经显式赋值传递，不给 cli 的类型面加包级状态。
// The entry package assigns main.version to it before calling Run, and Run hands
// it to the root command. A build identity is not per-request business state, so
// this is deliberately not routed through context values.
var Version = "dev"

// Run 是装配骨架与进程退出码的唯一出口：成功 0 / 失败 1，具体原因由文案区分。
// Run is the assembly skeleton and the single outlet for the process exit code:
// 0 on success, 1 on failure, with the wording carrying the reason.
//
// 装配顺序：工作目录 → 日志档位（读配置，缺省档兜底）→ 显式 Deps → 根命令。
// 配置缺失不是失败——init 本来就不生成实例配置，status 更是专为"无配置"设计；
// 配置存在但无效则记 WARN，由消费该配置的命令自行判卷（status 节 2 计失败）。
// Assembly order: working directory, log profile, explicit Deps, root command.
// A missing configuration is not a failure; an invalid one is logged and left
// for the consuming command to judge.
func Run(ctx context.Context, args []string) int {
	workDir, err := os.Getwd()
	if err != nil {
		// 配置尚未装载，用缺省档把失败记下来——错误绝不静默吞掉。
		NewLogger(nil).Error("无法确定当前工作目录", "error", err)
		return 1
	}

	logger := bootstrapLogger(workDir)

	// Store / Workspace 是装配层注入的适配器（ports.go）：命令面只认自己声明的
	// 端口，storage / workspace 的实现细节不越过这道接缝。
	// The adapters are injected here; the command surface only sees its own ports.
	deps := cli.Deps{
		WorkDir:                workDir,
		SupportedSchemaVersion: storage.SupportedSchemaVersion,
		Logger:                 logger,
		Store:                  storageStore{},
		Workspace:              workspacePort{},
	}

	root := cli.NewRootCommand(deps)
	root.Version = Version
	root.SetArgs(args)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	if err := root.ExecuteContext(ctx); err != nil {
		// cobra 已把可读文案写到标准错误；这里补结构化日志，失败路径两轨都在。
		logger.Error("命令执行失败", "error", err)
		return 1
	}
	return 0
}

// bootstrapLogger 装载配置以选择日志档位；配置缺失取缺省档，存在但无效记 WARN
// 后同样取缺省档——日志器永远可用，命令的退出码不受这里影响。
// bootstrapLogger loads the configuration to pick the log profile. It always
// returns a usable logger and never changes a command's exit code.
func bootstrapLogger(workDir string) *slog.Logger {
	path := filepath.Join(workDir, configFileName)
	cfg, err := cli.LoadConfig(path, os.LookupEnv)
	switch {
	case err == nil:
		return NewLogger(cfg)
	case errors.Is(err, fs.ErrNotExist):
		return NewLogger(nil)
	default:
		logger := NewLogger(nil)
		logger.Warn("配置无效，按缺省日志档继续", "path", path, "error", err)
		return logger
	}
}

// configFileName 是实例配置的文件名（与工作区同级，不在 .basalt/ 内）。
const configFileName = "basalt.yaml"
