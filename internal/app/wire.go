package app

// 本文件是**唯一装配层**的重路径接线（技术方案 §8.7 轻 / 重两条路；颗粒度文档 §2.3 流程 D /
// 口径 25）：重命令（本 US 是 `chat`——`serve` 归后续 US）在开跑之前，按**固定次序**把长生命
// 周期资源接起来——取实例锁 → 写模式打开 → 显式迁移 → 崩溃恢复（账本六步 + 工作区残留第 ⑤ 步）
// → 建工具注册池 → 注册池名字集喂 Agent 装载器 → 构造运行入口。
// This file wires the heavy path, the one place where an instance's long-lived resources
// are assembled. The order is a correctness constraint rather than a style: the lock
// precedes every write, the recovery scan precedes the first run, and the agent registry
// precedes admission.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// workspaceDirName / databaseFileName 与命令面的布局常量同源（cli 的对应常量未导出，
	// 装配层引用不到）：工作区 = <工作目录>/.basalt，库文件在工作区根下。
	// The workspace layout literals, mirroring the command surface's own constants.
	workspaceDirName = ".basalt"
	databaseFileName = "basalt.db"

	// agentsDirName 是 Agent 目录名（与 workspace / tool 两包内的同名常量同一份约定）：
	// 重路径用它把 Agent 名还原成绝对目录，填进 `ToolEnv.AgentDir`。
	// The agents directory name, shared with the workspace and tool packages.
	agentsDirName = "agents"

	// stepLock / stepOpen / stepMigrate / stepRecover 是重路径四个次序读数（T050 T1 的判据）：
	// 日志里 `step` 键的取值必须**按此序**出现——顺序错了就是接线错了。
	// The four ordering readings: the `step` values must appear in exactly this order.
	stepLock    = "lock"
	stepOpen    = "open"
	stepMigrate = "migrate"
	stepRecover = "recover"
)

// App 是重路径装配的显式依赖面（宪法 VII：不塞 context、不用包级变量）——与命令面的
// `cli.Deps` 同一套纪律，只是面向长生命周期资源。
// App carries the heavy path's explicit dependencies; nothing is taken from the context
// or from package state.
type App struct {
	// workDir 是进程工作目录；工作区根 = <workDir>/.basalt。
	// workDir is the process working directory; the workspace is <workDir>/.basalt.
	workDir string

	// supportedVersion 是目标 schema 版本——生产传 storage.SupportedSchemaVersion，测试传
	// 合成值以覆盖单版本期不可达的分支（与 cli.Deps 的同名字段同口径）。
	// supportedVersion is the schema version this binary targets.
	supportedVersion int

	// logger 是装配期留痕的出口；nil 回落进程默认档。
	// logger is the assembly-time log outlet; a nil falls back to the process default.
	logger *slog.Logger

	// now 是时钟（恢复扫描的判定时刻与运行时间戳同源）；nil 回落 time.Now。
	// now is the clock shared by the recovery scan and the run timestamps.
	now func() time.Time

	// cfg 是**只读一次**装载出来的实例配置（重路径不另读一遍——同一文件两次读取正是让
	// 注册判定与运行配置分叉的那种缺陷）。
	// cfg is loaded exactly once, so the registration verdict and the run configuration
	// can never diverge.
	cfg *cli.Config

	// cfgErr 是那次装载的错误。文件缺失按空配置继续（与 init / status 对"无配置"的宽容同
	// 口径）；**其余错误 fail-closed 拒绝启动**——预算与凭证读不出来时静默跑一套缺省值，
	// 等于替运维改了上限。
	// cfgErr: a missing file falls back to the empty configuration; anything else refuses
	// to start.
	cfgErr error

	// providerOverride 是模型客户端的**进程内替身**（评测基座的默认后端；生产路径恒 nil）。
	// 非 nil 时它替掉每个配置名的真实客户端，但**仍套在路由 + 两阶段审计之内**——评测跑的是
	// 与生产同一条审计链，"用量读数"因此不是另一套统计口径。
	// A non-nil override replaces the real clients behind every configured name while
	// staying inside the routing and audit service, so evals exercise the very same chain.
	providerOverride core.ProviderClient
}

// wsRoot 返回工作区根的绝对路径。
// wsRoot returns the absolute workspace root.
func (a *App) wsRoot() string { return filepath.Join(a.workDir, workspaceDirName) }

// dbPath 返回库文件路径。
// dbPath returns the database file path.
func (a *App) dbPath() string { return filepath.Join(a.wsRoot(), databaseFileName) }

// loggerOr 保证日志出口永远可用（缺省档与 cli 侧同口径）。
// loggerOr keeps the log outlet usable even when no logger was injected.
func (a *App) loggerOr() *slog.Logger {
	if a.logger == nil {
		return slog.Default()
	}
	return a.logger
}

// nowOr 保证时钟可用：恢复扫描与运行入口取的是同一个来源。
// nowOr keeps one clock behind both the recovery scan and the run entry point.
func (a *App) nowOr() func() time.Time {
	if a.now == nil {
		return time.Now
	}
	return a.now
}

// newRuntime 按固定次序装配一次重路径运行期；返回的句柄归调用方所有，**必须** Close（释放
// 次序与取得次序相反）。任何一步失败都不留半开的资源：已到手的锁与库句柄在这一层释放并
// 并入错误链。
// newRuntime assembles the heavy-path runtime in the fixed order. The returned handle owns
// the instance lock and the database and must be closed by the caller; every failure path
// below releases whatever it already holds.
func (a *App) newRuntime(ctx context.Context) (*heavyRuntime, error) {
	logger := a.loggerOr()
	root := a.wsRoot()

	if a.cfgErr != nil && !errors.Is(a.cfgErr, fs.ErrNotExist) {
		// 配置损坏即拒绝：这一层宁可不开跑，也不拿一份读不出来的配置去改库与放行工具。
		// A broken configuration refuses the whole heavy path — nothing is opened at all.
		return nil, fmt.Errorf("实例配置无效，重路径拒绝启动: %w", a.cfgErr)
	}
	cfg := a.cfg
	if cfg == nil {
		// 配置文件缺失：空配置（策略全拒、provider 表为空）。重路径不代造配置，也不因此
		// 拒绝启动——命令面自己还会就"无配置"提醒一次。
		// A missing file yields the empty configuration: deny-by-default policy, no providers.
		cfg = &cli.Config{}
	}

	// ① 实例锁：**先于一切写入**。取不到（被活实例持有）即拒绝启动，且零写入。
	lock, err := workspace.AcquireInstanceLock(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("工作区骨架缺失（%s）——先跑 `basalt init` 建骨架: %w", root, err)
		}
		return nil, fmt.Errorf("取得实例锁: %w", err)
	}
	logger.Info("重路径：实例锁已取得", "step", stepLock, "root", root)

	// ② 写模式打开：库文件缺失即建（全量建表与版本行在同一事务内完成）。
	db, err := storage.OpenWritable(ctx, a.dbPath(), a.supportedVersion)
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("写模式打开数据库: %w", err), nil, lock)
	}
	logger.Info("重路径：数据库已写模式打开", "step", stepOpen, "path", a.dbPath())

	// ③ 显式迁移：升级是持锁重命令的**显式动作**，不做任何"顺手改库"。
	if err = db.Migrate(ctx, a.supportedVersion); err != nil {
		return nil, abortRuntime(fmt.Errorf("迁移数据库: %w", err), db, lock)
	}
	logger.Info("重路径：schema 迁移完成", "step", stepMigrate, "schema_version", a.supportedVersion)

	// ④ 崩溃恢复两段，都在开跑之前、都在同一把锁之下：账本侧（半截工具组 + 陈旧运行）与
	// 工作区侧（目录交换残留）。第 6 步的启动摘要——"这次启动修了什么"——由下面这条 INFO
	// 一句话答出（契约 §4.3.2 第 6 步）。
	report, err := db.Recover(ctx, logger, a.nowOr()())
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("崩溃恢复扫描: %w", err), db, lock)
	}
	residue, err := workspace.RecoverWorkspaceResidue(root, logger)
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("工作区残留判定: %w", err), db, lock)
	}
	logger.Info("重路径：崩溃恢复完成", "step", stepRecover,
		"unknowns", report.Unknowns, "repaired_groups", report.RepairedGroups,
		"closed_runs", report.ClosedRuns, "rolled_back", residue.RolledBack,
		"rolled_forward", residue.RolledForward)

	// ⑤ 授权判定与工具注册池：策略先建（工具持有的是**接口**，具体实现在装配处一处选定）；
	// 池是**全局**的——同一实例的注册池服务所有 Agent，声明的名字与之比对（宪法 IV）。
	policy := tool.NewWhitelistPolicy(tool.PolicyOptions{
		Root:            root,
		CrossAgentRead:  cfg.Sandbox.CrossAgentRead,
		AllowedCommands: cfg.Shell.AllowedCommands,
		Interpreters:    cfg.Shell.Interpreters,
		AllowedDomains:  cfg.HTTP.AllowedDomains,
	})
	registry := tool.NewRegistry(logger)
	// 受控本地执行器在**本 US 首次接线**（US-4 交付了实现与用例、零生产调用点）：shell 工具
	// 持有的是 `core.CommandRunner` 接口，具体实现在这里一处选定（宪法 VI）。装配上限接
	// `budget.max_output_bytes` 而**不是** 0——传 0 会让读取侧恒用 256 KB 缺省，配置收紧的
	// 预算就到不了读取端（子进程仍把 256 KB 读完才算完）。
	// The controlled executor is wired for the first time here; the assembly-time cap comes
	// from the existing budget key rather than from the runner's own default.
	runner := tool.NewLocalRunner(tool.RunnerOptions{
		Root:           root,
		EnvPassthrough: cfg.Shell.EnvPassthrough,
		Interpreters:   cfg.Shell.Interpreters,
		MaxOutputBytes: outputBudget(cfg),
	})
	// 六件工具：三件文件工具 + 本 US 的 shell / http_get / http_post（技能调用工具永久禁；
	// `notify` / `manage_schedule` 与 US-6 / US-7 / US-10 各件仍为占位）。write_file 的构造期
	// Agent 目录留空：每次调用的 `ToolEnv.AgentDir` 才是写入根，"判定的那个 Agent"与"写入的
	// 那棵树"同源由运行环境保证。
	// Six tools: the three file tools plus this US's shell and HTTP pair.
	for _, t := range []core.Tool{
		tool.NewReadFileTool(policy, root),
		tool.NewWriteFileTool(policy, root, ""),
		tool.NewListDirTool(policy, root),
		tool.NewShellTool(policy, runner),
		tool.NewHTTPGetTool(policy),
		tool.NewHTTPPostTool(policy),
	} {
		if err := registry.Register(t); err != nil {
			return nil, abortRuntime(fmt.Errorf("注册工具: %w", err), db, lock)
		}
	}

	// ⑥ 注册池名字集喂装载器：workspace **不得**反向 import tool，名字在装配期一次导出
	// （§2.4 注 4）。声明了池外工具名的 Agent 整只不注册（宪法 IV）。
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: providerNames(cfg),
		Tools:     registry.Names(),
		Logger:    logger,
	}).LoadAll(root)
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("装载 Agent 目录: %w", err), db, lock)
	}
	agents := newAgentSource(root, entries, logger)

	// ⑦ 运行入口：模型调用唯一入口 + 四个仓储 + 自实现循环 + 显式时钟。provider 映射表由
	// `newProviderService` 在这一次装配里**一次构建**（宪法 III：运行时只查表）。
	sessions := storage.NewSessionStore(db)
	invocations := storage.NewToolInvocationRepo(db)
	llmCalls := storage.NewLLMCallRepo(db)
	client, err := a.modelClient(cfg, llmCalls, logger)
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	loop := core.NewReActLoop(
		core.NewPromptBuilder(registry),
		core.NewToolExecutor(registry, invocations, logger),
		logger,
	)
	service := core.NewAgentService(core.ServiceOptions{
		Agents:    agents,
		Client:    client,
		Messages:  sessions,
		Runs:      storage.NewRunStore(db),
		Snapshots: storage.NewSnapshotStore(db),
		Audit:     invocations,
		Loop:      loop,
		Logger:    logger,
		Now:       a.nowOr(),
	})

	logger.Info("重路径装配完成", "agents", len(agents.entries), "tools", registry.Names())
	return &heavyRuntime{
		lock:        lock,
		db:          db,
		service:     service,
		sessions:    sessions,
		invocations: invocations,
		llmCalls:    llmCalls,
		registry:    registry,
	}, nil
}

// readStore 按**只读路径**打开库句柄：不取实例锁、不做崩溃恢复、不清理锁文件，`schema_version`
// 的裁决发生在打开之前（`OpenReadOnly` 内）——轻路径里需要数据的命令（`session list`）用它。
// readStore is the read-only path: no lock, no recovery, no lock-file cleanup, and the
// schema version verdict happens before the database is opened.
//
// 与重路径的区别只有一句：**不打开写池**。因此它可以在别的实例持锁时使用（那正是只读命令
// 存在的意义），也永远不可能顺手改库。
// The only difference from the heavy path is that no write pool is opened.
func (a *App) readStore(ctx context.Context) (*storage.DB, error) {
	return storage.OpenReadOnly(ctx, a.dbPath(), a.supportedVersion)
}

// abortRuntime 把失败与已取得资源的释放并成一条错误链——半开的句柄不回给调用方。`db` 为
// nil 表示库还没打开（第二步之前失败），此时只放锁；锁非 nil 是本函数的调用前提。
// abortRuntime folds a failure together with the release of whatever was already acquired.
func abortRuntime(err error, db *storage.DB, lock *workspace.InstanceLock) error {
	if db != nil {
		err = errors.Join(err, db.Close())
	}
	return errors.Join(err, lock.Release())
}

// providerNames 按配置顺序取出 provider 名集（显式遍历有序数组，**不**遍历映射表——Go 的
// map 遍历顺序随机，任何"从表里挑"的写法都会让注册判定在两次启动之间漂移）。
// providerNames lists the configured provider names in configuration order.
func providerNames(cfg *cli.Config) []string {
	names := make([]string, 0, len(cfg.Providers))
	for _, entry := range cfg.Providers {
		names = append(names, entry.Name)
	}
	return names
}

// outputBudget 把配置面的输出预算（int64）收成执行侧装配值（int）；超出 int 表示范围的取值
// **不搬运**——留在零值由 runner 补缺省，比截断成一个无声改写的上限更接近配置者的意图
// （与 `cli/chat.go` 的运行预算同一套转换口径）。
// outputBudget narrows the configured cap to the executor's int, dropping out-of-range
// values rather than truncating them into a cap nobody wrote.
func outputBudget(cfg *cli.Config) int {
	if v := cfg.Budget.MaxOutputBytes; v > 0 && v <= math.MaxInt {
		return int(v)
	}
	return 0
}

// heavyRuntime 是重路径运行期的资源句柄：实例锁、数据库、运行入口与三个只读消费面。
// 生命周期 = 一次重命令——`chat` 在整场交互期间持有它，退出时释放。
// heavyRuntime owns the heavy path's resources for the lifetime of one heavy command.
//
// 名字带 `heavy` 前缀是为了与导出面 `Runtime` 明确分开：包内这两个名字只差大小写，
// 读代码时一眼能看出"这是内部接线"还是"这是包外拿到的句柄"。
// The heavy prefix keeps the internal handle distinguishable from the exported Runtime.
type heavyRuntime struct {
	// lock 是实例锁：进程存续期间持有（写模式的语义就是"这一进程是唯一写者"）。
	lock *workspace.InstanceLock

	// db 是写模式打开的库句柄（读写两条池）。
	db *storage.DB

	// service 是唯一运行入口。
	service *core.AgentService

	// sessions 是会话仓储；命令面经只读端口消费它（`chat` 读回本轮消息、`session list`
	// 列清单）——写路径只有 core 一条。
	sessions *storage.SessionStore

	// invocations / llmCalls 是两个审计仓储：装配期建好之后**同时**喂给循环（写路径）与
	// 导出面（只读路径）。一份实现、两个消费面——读面另起一个实例会让"读到的"与"写下的"
	// 隔着两条连接池，排障时说不清是不是同一个库。
	// One implementation feeds both the write path (the loop) and the read face.
	invocations *storage.ToolInvocationRepo
	llmCalls    *storage.LLMCallRepo

	// registry 是装配期构建的工具注册池（只读面）：`tool list` 要的"注册了什么"只存在于
	// 这一次装配之后——池是内存对象，命令面经 `cli.ToolLister` 取它的**数据**。
	// The assembly-time tool registry, kept for the command surface's listing.
	registry *tool.Registry
}

// Close 按**相反次序**释放：先关库（写池的最后一批使用者就是运行入口），再放实例锁——
// 锁必须等所有写入收尾之后才松开。两处失败并入一条错误链，释放失败不得静默。
// Close releases in reverse order and never hides a failure.
func (r *heavyRuntime) Close() error {
	var errs []error
	if r.db != nil {
		errs = append(errs, r.db.Close())
	}
	if r.lock != nil {
		errs = append(errs, r.lock.Release())
	}
	return errors.Join(errs...)
}

// runService 把运行入口交给命令面的端口（适配器见 ports.go）：返回的正是 `cli.Deps` 要注入
// 的那个接口类型——装配层是唯一知道"接口背后是哪个实例"的地方。
// runService hands the run entry point to the command surface's port.
func (r *heavyRuntime) runService() cli.RunService { return runServicePort{service: r.service} }

// sessionLister 把会话仓储交给命令面的**只读**端口；写路径不在这里（追加的唯一出口在 core）。
// sessionLister hands the session store to the read-only port.
func (r *heavyRuntime) sessionLister() cli.SessionLister { return sessionListerPort{store: r.sessions} }

// tools 把装配期注册池交给命令面的工具清单端口（`tool list` 消费）。
// tools hands the assembly-time registry to the command surface's tool-listing port.
func (r *heavyRuntime) tools() cli.ToolLister { return toolListerPort{registry: r.registry} }

// agentSourcePort 是 `core.AgentSource` 的适配器（端口定义在消费方 core，实现落在装配层）：
// 装配期装载出来的注册结果 + 工作区根 + 段一 / 快照装配器，回答运行入口的三个问题——
// 在不在、目录在哪、这一跑的配置快照是什么。
// agentSourcePort answers the run entry point's three questions: is it registered, where
// does it live, and what is this run's frozen configuration snapshot.
type agentSourcePort struct {
	// root 是工作区根（`.basalt`）。
	root string

	// entries 只含**注册成功**的 Agent——未注册的目录不进运行面（准入判据只有这一份）。
	// Only registered agents make it into the run face.
	entries map[string]core.AgentEntry

	// contexts 冻结运行级配置快照（读即所记：Profile 与段一正文出自同一次读取）。
	contexts *workspace.ContextLoader
}

// 编译期断言：端口形状与适配器一旦对不上，在这里先红。
// Compile-time assertion that the adapter still matches the consumer's port.
var _ core.AgentSource = agentSourcePort{}

// newAgentSource 从装载结果建运行面：未注册的条目在这里被挡在外面，运行入口因此不必再判
// 一次注册态——判定只发生一次，读的是同一份登记结果。
// newAgentSource keeps the registered entries only, so registration is judged exactly once.
func newAgentSource(root string, entries []core.AgentEntry, logger *slog.Logger) agentSourcePort {
	registered := make(map[string]core.AgentEntry, len(entries))
	for _, entry := range entries {
		if entry.Registered {
			registered[entry.Name] = entry
		}
	}
	return agentSourcePort{root: root, entries: registered, contexts: workspace.NewContextLoader(logger)}
}

// Exists 报告 Agent 是否已注册。
// Exists reports whether the agent is registered.
func (p agentSourcePort) Exists(name string) bool {
	_, ok := p.entries[name]
	return ok
}

// Dir 返回 Agent 目录的绝对路径；未知 Agent 返回空串——`ToolEnv.AgentDir` 的空值语义是
// "无 Agent 上下文"，工具据此不放宽任何一格。
// Dir returns the agent directory; an empty string means "no agent context", which tools
// never widen access on.
func (p agentSourcePort) Dir(name string) string {
	if _, ok := p.entries[name]; !ok {
		return ""
	}
	return filepath.Join(p.root, agentsDirName, name)
}

// Snapshot 冻结一次运行的配置快照（Profile / 段一正文 / 清单哈希出自同一次读取，读即所记）。
// 签名里的 ctx 是端口形状；这一段是短读、没有取消点——取消留给模型调用与工具执行。
// Snapshot freezes one run's configuration. The context is part of the port's shape; this
// is a short read with no cancellation point of its own.
func (p agentSourcePort) Snapshot(_ context.Context, name string, now time.Time) (*core.ConfigSnapshot, error) {
	entry, ok := p.entries[name]
	if !ok {
		return nil, fmt.Errorf("agent %s 未注册——无配置可冻结（先看 profile list 的拒绝原因）", name)
	}
	return p.contexts.Snapshot(p.root, entry, now)
}

// Options 是包外取得重路径运行期的显式入参（评测基座 `evals` 的装配点，颗粒度文档 §3.1）。
// 字段与 `App` 的私有面一一对应：包外只说清"从哪来、什么版本、什么时钟、谁来回答"——
// 依赖全部走类型化参数（宪法 VII：不塞 context、不用包级变量）。
// Options carries the heavy path's inputs from outside the package.
type Options struct {
	// WorkDir 是进程工作目录；工作区根 = <WorkDir>/.basalt。**必填**——空值会让工作区
	// 落到相对路径上，那是"在另一个目录又建了一个库"的经典形态。
	// WorkDir is required: an empty value silently resolves the workspace relatively.
	WorkDir string

	// SupportedSchemaVersion 是目标 schema 版本；生产传 storage.SupportedSchemaVersion。
	// **必填且为正**——零值建出来的库没有任何版本行，后续每次打开都是版本裁决失败。
	SupportedSchemaVersion int

	// Logger 是日志出口；nil 回落进程默认档。
	Logger *slog.Logger

	// Now 是时钟（恢复扫描的判定时刻与运行时间戳同源）；nil 回落 time.Now。
	Now func() time.Time

	// ProviderClient 是模型客户端的**进程内替身**（可选；评测基座的默认后端）。非 nil 时
	// 它替掉每个配置名的真实客户端，但**仍套在路由 + 两阶段审计之内**——评测跑的是与生产
	// 同一条审计链，"用量读数"因此不是另一套统计口径。nil = 用配置里的真实客户端。
	// A non-nil override replaces the real clients but stays inside the routing and audit
	// service, so eval usage readings come from the production chain.
	ProviderClient core.ProviderClient
}

// Runtime 是包外拿到的重路径运行期句柄：三个消费面 + 一个释放口。
// Runtime is the heavy-path handle visible outside the package.
//
// 每个访问器返回的都是**消费方声明的接口**：包外拿到的是能力而不是实现——`storage` 的
// 具体类型不越过这道接缝，读面也就无从顺手取到写方法。
// Every accessor returns the consumer-declared interface, so no storage type crosses it.
type Runtime struct{ inner *heavyRuntime }

// RunService 返回唯一运行入口（命令面与评测基座共用同一条入口）。
// RunService returns the one run entry point.
func (r *Runtime) RunService() cli.RunService { return r.inner.runService() }

// Sessions 返回会话的只读面（列出会话 / 读回消息）。
// Sessions returns the read-only session surface.
func (r *Runtime) Sessions() cli.SessionLister { return r.inner.sessionLister() }

// Invocations 返回工具审计的只读面（按运行标识取调用行——与循环消费的是同一个接口）。
// Invocations returns the tool audit's read-only surface, typed by the very interface the
// loop itself consumes.
func (r *Runtime) Invocations() core.ToolInvocationReader { return r.inner.invocations }

// LLMCalls 返回模型调用审计的只读面（按运行标识取用量行——「计量完整性」判定的唯一来源）。
// LLMCalls returns the model-call audit's read-only surface, the single source of the
// usage readings and the completeness verdict.
func (r *Runtime) LLMCalls() core.LLMCallReader { return r.inner.llmCalls }

// Close 释放运行期（次序与取得相反）；重复调用不得再放一次锁。
// Close releases the runtime in reverse acquisition order.
func (r *Runtime) Close() error { return r.inner.Close() }

// OpenRuntime 按固定次序装配一次重路径运行期并交给包外调用方（评测基座走这条路）。
// OpenRuntime assembles the heavy-path runtime for callers outside the package.
//
// 与 `Run` 的差别只有**谁来驱动**：命令面走 cobra，评测基座直接调运行入口。配置装载一次、
// 锁 / 迁移 / 恢复的次序、审计链——一条不少，评测跑的因此是生产的那条路。
// The only difference from Run is who drives it; nothing about the assembly or the audit
// chain differs, so evals exercise the production path.
func OpenRuntime(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.WorkDir == "" {
		return nil, errors.New("OpenRuntime: WorkDir 必填（工作区根 = <WorkDir>/.basalt）")
	}
	if opts.SupportedSchemaVersion <= 0 {
		return nil, errors.New("OpenRuntime: SupportedSchemaVersion 必须为正（生产传 storage.SupportedSchemaVersion）")
	}
	configPath := filepath.Join(opts.WorkDir, configFileName)
	cfg, cfgErr := cli.LoadConfig(configPath, os.LookupEnv)
	a := &App{
		workDir:          opts.WorkDir,
		supportedVersion: opts.SupportedSchemaVersion,
		logger:           opts.Logger,
		now:              opts.Now,
		cfg:              cfg,
		cfgErr:           cfgErr,
		providerOverride: opts.ProviderClient,
	}
	rt, err := a.newRuntime(ctx)
	if err != nil {
		return nil, err
	}
	return &Runtime{inner: rt}, nil
}
