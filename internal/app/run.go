package app

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

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

// heavyCommands 是需要完整运行期的命令名清单（重路径）：**涉及注册表或数据库的读写**。
// 轻命令（init / status / provider / profile / session 等只读面）走零装配路径——不取实例
// 锁、不开数据库写入，打开库前自行校验 `schema_version`（口径 23 / 25）。
// heavyCommands lists the commands that need the full runtime: those that read or write
// the agent registry or the database. Everything else takes the zero-assembly path.
//
// 清单只有这一份，判据落在命令名上——装配层不猜意图，命令面也不会因为注册顺序不同
// 而走到另一条路上（`serve` 落位时在此追加一名即可）。
// One list, keyed by command name; serving a new heavy command means adding one entry.
var heavyCommands = []string{"chat"}

// readStoreCommands 是需要**只读库句柄**的命令组：`session list` 要读会话数据，但它
// **不是**重命令——不取锁、不做恢复、不写库（口径 23：别的实例持锁时也该能看清单）。
// 它拿到的是一条只读打开，版本裁决在打开之前完成。
// readStoreCommands names the command groups that need a read-only database handle.
var readStoreCommands = []string{"session"}

// Run 是装配骨架与进程退出码的唯一出口：成功 0 / 失败 1，具体原因由文案区分。
// Run is the assembly skeleton and the single outlet for the process exit code:
// 0 on success, 1 on failure, with the wording carrying the reason.
//
// 装配顺序：工作目录 → 配置装载（只读一次）→ 日志档位 → 按命令分流（见下）→ 显式 Deps → 根命令。
// 配置装载现在**保留结果**：重路径要拿同一份配置去建策略与 provider 映射表，读第二遍会让
// 注册判定与运行配置各凭一次读取（同一文件两次读取正是缺陷 39 的根因形态）。
// Assembly order: working directory, one configuration load, log profile, heavy/light split,
// explicit Deps, root command.
//
// 分流只有两条：重命令先经 `App.newRuntime` 把锁、库、恢复与运行入口接起来，端口**注入**命令面；
// 轻命令零装配——不取锁、不开数据库写入（FR-025）。重路径失败即 1，不进命令面：半开的实例
// 不如不开。
// The heavy path assembles before the command surface runs and injects the run ports; the
// light path assembles nothing. A failed assembly exits 1 without reaching the command.
func Run(ctx context.Context, args []string) int {
	workDir, err := os.Getwd()
	if err != nil {
		// 配置尚未装载，用缺省档把失败记下来——错误绝不静默吞掉。
		NewLogger(nil).Error("无法确定当前工作目录", "error", err)
		return 1
	}

	configPath := filepath.Join(workDir, configFileName)
	cfg, cfgErr := cli.LoadConfig(configPath, os.LookupEnv)
	logger := bootstrapLogger(cfg, cfgErr, configPath)

	// Store / Workspace / Agents 是装配层注入的适配器（ports.go）：命令面只认自己
	// 声明的端口，storage / workspace 的实现细节不越过这道接缝。
	// The adapters are injected here; the command surface only sees its own ports.
	deps := cli.Deps{
		WorkDir:                workDir,
		SupportedSchemaVersion: storage.SupportedSchemaVersion,
		Logger:                 logger,
		Store:                  storageStore{},
		Workspace:              workspacePort{},
		// Agents 是写模式端口（create / delete 由实现侧取实例锁）——构造参数显式传递。
		Agents: agentsPort{WorkDir: workDir, Logger: logger},
	}

	// 装配面在这里构造一次，两条路径共用（重路径接管句柄，只读路径只开一次只读库）。
	// The assembly face is built once and shared by both paths.
	a := &App{
		workDir:          workDir,
		supportedVersion: storage.SupportedSchemaVersion,
		logger:           logger,
		cfg:              cfg,
		cfgErr:           cfgErr,
	}

	if needsRuntime(args) {
		// 重路径：固定次序（取锁 → 打开 → 迁移 → 恢复）在 newRuntime 内一次走完，四个次序
		// 读数以 `step` 键落日志（T050 T1 的判据）。
		// The heavy path walks its fixed order inside newRuntime and logs the four steps.
		rt, rtErr := a.newRuntime(ctx)
		if rtErr != nil {
			logger.Error("重路径装配失败", "error", rtErr)
			return 1
		}
		// 释放挂在唯一出口上：命令跑完（正常 / 报错 / 取消）都走同一条收尾路径。
		// Release hangs off the single exit point, so every ending takes the same path.
		defer func() {
			if closeErr := rt.Close(); closeErr != nil {
				logger.Error("重路径资源释放失败", "error", closeErr)
			}
		}()
		deps.RunService = rt.runService()
		deps.SessionLister = rt.sessionLister()
	} else if needsReadStore(args) {
		// 轻路径里需要数据的命令：**只读**打开一次（不取锁、不做恢复、不开写池）。
		// 打不开就如实报——库里没有可列的东西时，"列不出来"比"列出空清单"更接近事实。
		// A light command that needs data gets a read-only open, nothing else.
		read, readErr := a.readStore(ctx)
		if readErr != nil {
			logger.Error("只读打开数据库失败", "error", readErr)
			return 1
		}
		defer func() {
			if closeErr := read.Close(); closeErr != nil {
				logger.Error("只读库句柄释放失败", "error", closeErr)
			}
		}()
		deps.SessionLister = sessionListerPort{store: storage.NewSessionStore(read)}
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

// needsRuntime 判定这次调用是否走重路径：取**第一个非选项参数**当命令名，与重命令清单比对。
// needsRuntime reports whether the call takes the heavy path, judged by the first argument
// that is not an option.
//
// 只看命令名、不看其余参数：`basalt chat --message x` 与 `basalt --version` 两种形态都判得准。
// 前置全局选项若带值（本项目当前没有这样的选项）会落进首个非选项位——那种选项落位时要
// 一并改判据，注释留在这里就是为了那时候能看见。
// Only the command name matters; a value-taking global flag would land here first, which is
// the one case that would need this judgement revisited.
func needsRuntime(args []string) bool {
	name, ok := commandName(args)
	return ok && slices.Contains(heavyCommands, name)
}

// needsReadStore 判定这次调用是否需要一条只读库句柄（轻路径里的 `session list`）。
// needsReadStore reports whether the call needs a read-only database handle.
func needsReadStore(args []string) bool {
	name, ok := commandName(args)
	return ok && slices.Contains(readStoreCommands, name)
}

// commandName 取第一个非选项参数当命令名；`basalt --version` 这类全是选项的调用没有命令名。
// commandName returns the first non-option argument, if any.
func commandName(args []string) (string, bool) {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg, true
	}
	return "", false
}

// bootstrapLogger 按已装载的配置选日志档位；配置缺失取缺省档，存在但无效记 WARN
// 后同样取缺省档——日志器永远可用，轻命令的退出码不受这里影响。
// bootstrapLogger picks the log profile from the already-loaded configuration. It always
// returns a usable logger and never changes a light command's exit code.
//
// 配置的**裁决**分两层：轻命令由各自消费点自行判卷（status 节 2 计失败）；重命令的
// fail-closed 在 `App.newRuntime` 里（配置无效即拒绝装配）。本函数只管日志档位。
// Judgement stays with each consumer; this function only chooses a log profile.
func bootstrapLogger(cfg *cli.Config, cfgErr error, path string) *slog.Logger {
	switch {
	case cfgErr == nil:
		return NewLogger(cfg)
	case errors.Is(cfgErr, fs.ErrNotExist):
		// init 本来就不生成实例配置，status 更是专为"无配置"设计——缺文件不是失败。
		return NewLogger(nil)
	default:
		logger := NewLogger(nil)
		logger.Warn("配置无效，按缺省日志档继续", "path", path, "error", cfgErr)
		return logger
	}
}

// configFileName 是实例配置的文件名（与工作区同级，不在 .basalt/ 内）。
const configFileName = "basalt.yaml"
