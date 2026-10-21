package app

// 本文件是**唯一装配层**的重路径接线（技术方案 §6.1.2 轻 / 重两条路；颗粒度文档 §2.3 流程 D /
// 口径 25）：重命令（本 US 是 `chat`——`serve` 归后续 US）在开跑之前，按**固定次序**把长生命
// 周期资源接起来——取实例锁 → 写模式打开 → 显式迁移 → 崩溃恢复（账本六步 + 工作区残留第 ⑤ 步）
// → 构建记忆门面（载体按 `memory.backend` 选，US-6）→ 建工具注册池 → 注册池名字集喂 Agent
// 装载器 → 构造运行入口。
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
	"slices"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
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

	// stepLock / stepOpen / stepMigrate / stepRecover / stepBaseline / stepRecovery 是重路径
	// 六个次序读数（图 A 的判据）：日志里 `step` 键的取值必须**按此序**出现——顺序错了就是
	// 接线错了。基线排在崩溃恢复之后、管理面恢复之前（注 5）：先修账本与工作区残留，再补
	// 资源指针与生命周期状态文件，最后才查证上次留下的 pending 管理操作。
	// The six ordering readings: the `step` values must appear in exactly this order.
	stepLock     = "lock"
	stepOpen     = "open"
	stepMigrate  = "migrate"
	stepRecover  = "recover"
	stepBaseline = "baseline"
	stepRecovery = "recovery"
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

	// mcpConnect 是 MCP 连接池的**进程内替身**（沿 providerOverride 先例；生产路径恒 nil）。
	// 非 nil 时装配面不再建真实连接池——app 层用例因此不起真实子进程，也不新增导出测试 API。
	// A non-nil connector replaces the real pool; production leaves it nil.
	mcpConnect mcpConnector

	// notifyContext 是信号接线的**进程内接缝**（约束 24 的单一信号源；生产路径恒 nil）。
	// nil 时 `signalContext` 取标准库 `signal.NotifyContext`；用例注入可取消 ctx 的同形
	// 函数即可驱动取消传播，**不向进程发真实信号**。
	// notifyContext is the signal-wiring seam; nil falls back to signal.NotifyContext.
	notifyContext func(context.Context, ...os.Signal) (context.Context, context.CancelFunc)
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

	// 记忆门面（US-6，位置在 ④ 与 ⑤ 之间——颗粒度文档 §3.1）：载体按 `memory.backend` 在工作区
	// 根 / 库句柄之上选一次，门面**一个实例喂两个消费面**——工具侧（写路径）与运行入口的段 2
	// 注入（读路径）拿到的是同一个对象，作用域回退、per-scope 写锁与注入期 WARN 因此只有一份
	// 实现（口径 25）。构建期**零落盘**：markdown 档的 MEMORY.md 在首次写入时才创建。
	// The memory facade is built once and handed to both consumers, so the fallback, the
	// per-scope lock and the injection-time warning live in exactly one implementation.
	memBackend, err := memoryCarrier(cfg, db, root, a.nowOr())
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	memSvc, err := memory.NewService(memory.Options{Backend: memBackend, Logger: logger})
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("构建记忆门面: %w", err), db, lock)
	}

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
	// 十件工具：三件文件工具 + US-5 的 shell / http_get / http_post + 两件记忆工具 +
	// US-8 的 notify + US-10 的 manage_schedule（技能调用工具永久禁；US-7 的 MCP 各件在
	// 等值门禁之后注册——颗粒度文档 §3.5 后续占位 1）。`manage_schedule` 的编辑后端在
	// runtime（loader / scheduler）之后才存在，这里先建**两段绑定**的槽位，装配完成后
	// 由下方 `Bind` 接入生产适配器（颗粒度文档 §3.1.8 图 E）。write_file 的构造期 Agent
	// 目录留空：每次调用的 `ToolEnv.AgentDir` 才是写入根，"判定的那个 Agent"与"写入的
	// 那棵树"同源由运行环境保证。
	// Ten tools: the file trio, US-5's shell/HTTP trio, the memory pair, notify, and
	// manage_schedule — whose editor is bound in two stages once the runtime exists.
	scheduleSlot := tool.NewScheduleEditorSlot()
	for _, t := range []core.Tool{
		tool.NewReadFileTool(policy, root),
		tool.NewWriteFileTool(policy, root, ""),
		tool.NewListDirTool(policy, root),
		tool.NewShellTool(policy, runner),
		tool.NewHTTPGetTool(policy),
		tool.NewHTTPPostTool(policy),
		tool.NewSaveMemoryTool(memSvc),
		tool.NewRecallMemoryTool(memSvc),
		tool.NewNotifyTool(storage.NewNotifyChannelRepo(db), policy, newNotifyAdapters()),
		tool.NewManageScheduleTool(scheduleSlot),
	} {
		if err := registry.Register(t); err != nil {
			return nil, abortRuntime(fmt.Errorf("注册工具: %w", err), db, lock)
		}
	}
	// 注册池必须与**声明名集**（`declaredToolNames`）逐名一致：轻路径（`profile list` /
	// `profile show`）不建注册池，Agent 的装载判定拿的就是那份声明——两者漂移会让同一份
	// AGENT.md 在重 / 轻两条路上得到相反的注册结论。不一致即拒绝启动：两套判定的分歧比
	// "哪一套对"更值得先停下。
	// The registry must match the declared name set, or the same AGENT.md would be judged
	// differently depending on which command read it.
	if got := registry.Names(); !slices.Equal(got, declaredToolNames) {
		return nil, abortRuntime(fmt.Errorf(
			"注册池与声明名集不一致——声明 %v、实际注册 %v（重 / 轻两条路会给出相反的注册判定）",
			declaredToolNames, got), db, lock)
	}

	// MCP 接入在等值门禁**之后**（约束 9）：门禁比对的是十件内置声明名集，MCP 工具随后
	// 注册、池随之变大；把注册挪到门禁之前，门禁会因池变大而拒绝启动。零 server（缺失 /
	// 空 / {} / 无 servers）整段跳过——不建池、不连接、不发日志，零 MCP 配置时行为与 US-6
	// 逐字节相同（约束 1）。解析失败是工作区结构问题，与「配置写错的实例配置」同档拒绝启动。
	// MCP wiring happens after the equality gate: the gate compares the ten builtin names,
	// and MCP tools join the pool afterwards. Zero servers skip the whole block.
	mcpSpecs, err := workspace.ParseMCPServers(root)
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("解析 MCP 配置: %w", err), db, lock)
	}
	var mcpConns []tool.MCPConnection
	var mcpPool mcpConnector
	if len(mcpSpecs) > 0 {
		mcpPool = a.mcpConnect
		if mcpPool == nil {
			mcpPool = tool.NewMCPPool(tool.MCPPoolOptions{
				Logger:         logger,
				EnvPassthrough: cfg.Shell.EnvPassthrough,
				Root:           root,
			})
		}
	}

	// fail（§3.5 改造点 ⑥）：连接池一经建立就可能持有真实子进程——自这一刻起的每一条
	// 装配失败路径都**先收池、再走 abortRuntime**（库 → 锁），半开会话不得留在失败路径上。
	// 零 server 时 mcpPool 为 nil，闭包退化为原 abortRuntime（零配置行为逐字节不变）。
	// From the moment the pool exists, every failure path closes it before releasing the
	// database and the lock; with zero servers the closure degenerates to abortRuntime.
	fail := func(err error) (*heavyRuntime, error) {
		if mcpPool != nil {
			err = errors.Join(err, mcpPool.Close())
		}
		return nil, abortRuntime(err, db, lock)
	}

	if mcpPool != nil {
		// 占位解析在连接**之前**（技术方案 §3.7）：解析不出来就不连接——fail-closed 发生在
		// 任何子进程被拉起之前，错误里只有 server / 键 / 变量名，没有值。
		mcpSpecs, err = resolveMCPEnv(mcpSpecs, os.LookupEnv)
		if err != nil {
			return fail(fmt.Errorf("解析 MCP server 环境变量: %w", err))
		}
		mcpConns, err = mcpPool.Connect(ctx, mcpSpecs)
		if err != nil {
			return fail(fmt.Errorf("连接 MCP server: %w", err))
		}
		// 注册序 = server 文件序 → 工具列表序（连接结果已按文件序返回）。重名 keep-first：
		// 注册池保留先注册者（内置优先）并记 WARN `拒绝重复注册的工具`——重复不是装配失败
		// （约束 8），其余 MCP 工具照常注册。
		for _, conn := range mcpConns {
			for _, t := range conn.Tools {
				if regErr := registry.Register(t); regErr != nil {
					continue
				}
			}
		}
		// 诊断旁路（Debug，缺省级别不输出）：连接序 → 注册序的工具名清单。
		logger.Debug("MCP 工具进入注册池", "tools", mcpToolNames(mcpConns))
	}

	// ⑥ 注册池名字集喂装载器：workspace **不得**反向 import tool，名字在装配期一次导出
	// （§2.4 注 4）。声明了池外工具名的 Agent 整只不注册（宪法 IV）。MCP 两名单都是**显式
	// 值**：已配置（文件序）与已连接（实际连上，文件序）——非 nil 的"已连接"集是重路径
	// "知道连接结果"的判据，装载器据此对失败 server 的 Agent 直接拒载，而不是退化成轻
	// 路径的第三态。轻路径两者皆 nil，由装载器自读配置并走第三态。
	// Both MCP name sets are explicit on the heavy path; a non-nil connected set tells the
	// loader the connection verdict is known, so failed servers are rejected outright.
	mcpRT := newMCPRuntime(mcpSpecs, mcpConns)
	configuredServers := mcpRT.ConfiguredNames()
	connectedServers := mcpRT.ConnectedNames()
	// loader 保留句柄：`manage_schedule` 的编辑适配器与装载共用**同一个**实例——
	// `ApplySchedule` 的 writeMu 因此也覆盖未来的装载调用（约束 29 / 39），
	// 不让第二条写入路径从另一个实例进入。
	// Keep the loader: the schedule editor shares this very instance so its write lock
	// stays the single gate for workspace writes.
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers:    providerNames(cfg),
		Tools:        registry.Names(),
		MCPServers:   configuredServers,
		MCPConnected: connectedServers,
		Logger:       logger,
	})
	entries, err := loader.LoadAll(root)
	if err != nil {
		return fail(fmt.Errorf("装载 Agent 目录: %w", err))
	}

	// 启动基线（颗粒度文档 §3.1.19 / 图 A ③）：迁移之后、开放服务之前，把资源发布指针与
	// 磁盘上的工作副本、生命周期状态文件对齐。Provider / MCP 的定义块按**原文逐行**取自实例
	// 配置与 mcp_servers.yaml——发布版本记录的是运维写下的那一段，不是重新序列化的结果。
	// 无活可做时不写操作行、不改状态文件（注 13），重复启动因此不在库里留新行。
	// The baseline aligns pointers with the workspace before serving; it takes provider and
	// MCP definitions verbatim from the files instead of re-serialising them.
	providerDefs, err := loadProviderDefinitions(filepath.Join(a.workDir, configFileName), providerNames(cfg))
	if err != nil {
		return fail(err)
	}
	mcpDefs, err := loadMCPServerDefinitions(root, mcpRT.ConfiguredNames())
	if err != nil {
		return fail(err)
	}
	// 进程级共享闸门（约束 25 / 46，图 F）：**只有这一份** Agent 级读写锁表与发布 / 准入
	// 闸门。基线此刻写状态文件用的锁，就是准入稍后重读用的锁，也是启停迁移与内容条件写
	// 用的锁；T045 的运行资源解析将在这同一个 ResourceGate 的读锁内进行。两个实例各建
	// 一份会让互斥形同虚设——装配层因此在第一次写之前建好、向下只传递。
	// One per-agent lock table and one publish/admission gate for the whole process; the
	// baseline writes under the very locks admission will read under.
	agentGate := core.NewAgentGate()
	resourceGate := &core.ResourceGate{}
	ops := storage.NewManagementStore(db)
	versions := storage.NewResourceStore(db)
	lifecycle := workspace.NewLifecycleStore(root, a.nowOr())
	// 技能库**一次构建**（T056）：基线发布、准入解析与发布版本快照读的是同一实例，
	// 版本目录与工作副本的形态因此只有一个来源。
	// One skill library shared by the baseline, admission and the published snapshot.
	skills := workspace.NewSkillLibrary(root)
	if err := runBaseline(ctx, BaselineDeps{
		Operations: ops,
		Resources:  versions,
		Skills:     skills,
		Lifecycle:  lifecycle,
		Providers:  providerDefs,
		MCPServers: mcpDefs,
		Agents:     entries,
		Logger:     logger,
		Now:        a.nowOr(),
	}); err != nil {
		return fail(err)
	}
	logger.Info("重路径：启动基线完成", "step", stepBaseline,
		"providers", len(providerDefs), "mcp", len(mcpDefs), "agents", len(entries))

	// 管理面恢复（图 A ④ / 约束 38）：基线自己若留下 pending 行，它已在上一段收口；
	// 其余按动作组查证，判定不了的标 unknown 并**保留原样**——重放一次停用会让 generation
	// 跳两格，重放一次内容写会覆盖第三方的修改。
	// Management recovery verifies each pending row and never replays a change.
	if err := recoverPendingOperations(ctx, RecoveryDeps{
		Operations: ops,
		Resources:  versions,
		Lifecycle:  lifecycle,
		Editor:     workspace.NewAgentEditor(workspace.AgentEditorDeps{Root: root}),
		Logger:     logger,
		Now:        a.nowOr(),
	}); err != nil {
		return fail(err)
	}
	logger.Info("重路径：管理面恢复完成", "step", stepRecovery)

	// 注册装配的技能发布过滤（约束 36 / FR-042，F-03 A）：装载器只校验技能实体，绑定
	// 指针是否存在必须在这里补判——启动失败关闭（读不到指针即拒绝启动），与 watcher 的
	// 热加载判定共用同一个函数。
	// The skill-publication filter completes the registration invariant after the loader
	// and before the registry; a failed lookup aborts startup, exactly like the watcher.
	entries, err = registryEntriesWithSkillPublication(ctx, versions, entries, logger)
	if err != nil {
		return fail(fmt.Errorf("注册装配: %w", err))
	}

	// 不可变注册表（§3.1.20 / 图 A）：装载结果在恢复之后冻结成首版快照。它是管理台列表、
	// 运行准入与调度器**共同**的读面——热加载之后以整份替换发布新版本，三处因此不可能
	// 各自看到不同的 Agent 集合。
	// The immutable registry is published after recovery and is the single read face for
	// the console, admission and the scheduler.
	agents := newAgentRegistry(root, entries, logger)

	// ⑦ 运行入口：模型调用唯一入口 + 四个仓储 + 自实现循环 + 显式时钟。provider 映射表在
	// 这一次装配里**一次构建**（宪法 III：运行时只查表）：运行准入拿它做"发布的 Provider
	// 是否可用"判定，模型调用拿它做路由——两个消费面共享同一份目录，表不可能分叉。
	// The one explicit provider catalog is built here and shared by admission and the
	// model client; the two faces can never see different tables.
	catalog, err := newProviderCatalogWithOverride(cfg, a.providerOverride)
	if err != nil {
		return fail(err)
	}
	sessions := storage.NewSessionStore(db)
	invocations := storage.NewToolInvocationRepo(db)
	llmCalls := storage.NewLLMCallRepo(db)
	// 运行账本只建一次：AgentService 写、web.RunService 适配器读，同一实例保证
	// "读到的"就是"写下的"（两条连接池之间的影子账本会让排障无从对齐）。
	// One run store is shared by the writer (AgentService) and the reader (the web port).
	runs := storage.NewRunStore(db)
	client := modelClient(catalog, llmCalls, logger)
	loop := core.NewReActLoop(
		core.NewPromptBuilder(registry),
		core.NewToolExecutor(registry, invocations, logger),
		logger,
	)
	service := core.NewAgentService(core.ServiceOptions{
		Agents: agents,
		// 生命周期准入（图 F ③ / 约束 15）：状态文件端口与 Agent 级闸门传的是上面那一份
		// 共享实例——准入在读锁内现场重读，启停迁移持写锁，两者因此互斥。nil 会让准入
		// 门禁与启停互斥同时静默消失，生产装配**不得**省略。
		// Admission shares the very gate and store the lifecycle transitions use; a nil
		// would silently disable the fail-closed gate.
		Lifecycle: lifecycle,
		Gate:      agentGate,
		// 发布资源的两件必须同进同出（图 F ③ / N1）：解析端口在资源闸门读锁内固定版本，
		// 快照构建器按固定的 SkillDirs 读发布目录；缺一件由 core 在准入前失败关闭。
		// Resources and RunSnapshots are wired together; a partial wiring fails closed.
		Resources:    NewRunResources(versions, catalog, client, mcpRT, skills, resourceGate),
		RunSnapshots: newRunSnapshotBuilder(workspace.NewContextLoader(logger), root, a.nowOr()),
		Client:       client,
		Messages:     sessions,
		// Sessions 是准入处的会话门禁端口（US-10 约束 40）：调度运行与手动补跑都经
		// `AgentService.Run`，已归档会话必须在唯一准入点被拒——门禁只能挂在 core，
		// web / app 都不得另写一份判定。nil 会让门禁静默消失，因此这里显式接上。
		// The session gate lives at the single admission point; a nil port would silently
		// disable it, so the store is wired explicitly.
		Sessions:  sessions,
		Runs:      runs,
		Snapshots: storage.NewSnapshotStore(db),
		Audit:     invocations,
		Memory:    memSvc,
		Loop:      loop,
		Logger:    logger,
		Now:       a.nowOr(),
		// 并发上限接线（US-9 约束 31）：0 / 未配置由 core 落缺省 32，负数已在装载期拒绝。
		// The cap is wired explicitly; zero falls back to core's single default.
		MaxConcurrent: cfg.Server.MaxConcurrentRuns,
	})

	// 调度器在重路径中**始终构造**（约束 21 / 图 A）：chat / tool 只是不 Start，
	// `webOptions.Schedules` 因此恒非 nil；启动接缝只认 serve / gateway（约束 20）。
	// 定义快照在 core.NewScheduler 构造时一次冻结，此后调用方改 entries 不影响对账。
	// The scheduler is always constructed on the heavy path; only serve/gateway Start it.
	// 条目取自同一份注册表快照（FR-22）：调度注册集与准入集不会分叉。
	// The scheduler reads the same registry snapshot, so its set can never diverge.
	scheduler := a.newScheduler(storage.NewTaskStore(db), service, agents.Entries(), cfg, logger)
	// 两段绑定的第二段（颗粒度文档 §3.1.17）：runtime 就绪后把生产编辑后端交给工具
	// 槽位。此后 `manage_schedule` 的写入门 = 归属预检 → 工作区写闸门 → 即时对账。
	// 适配器同时保留在运行期上：配置热加载的调度联动必须走**同一实例**（约束 39 的
	// 私有 claim 基线随之增减），另建一个只用裸 scheduler 的联动器会让基线分叉。
	// Second half of the two-stage binding: the production editor backend is installed
	// once the loader and scheduler exist, and the same instance is retained for hot reload.
	scheduleEditor := newScheduleEditorAdapter(loader, root, entries, scheduler, logger)
	scheduleSlot.Bind(scheduleEditor)

	logger.Info("重路径装配完成", "agents", len(agents.Entries()), "tools", registry.Names(),
		"mcp_connected", len(mcpConns))
	return &heavyRuntime{
		lock:           lock,
		db:             db,
		service:        service,
		runs:           runs,
		sessions:       sessions,
		invocations:    invocations,
		llmCalls:       llmCalls,
		registry:       registry,
		mcp:            mcpPool,
		scheduler:      scheduler,
		scheduleSlot:   scheduleSlot,
		scheduleEditor: scheduleEditor,
		agents:         agents,
		agentGate:      agentGate,
		lifecycle:      lifecycle,
		resourceGate:   resourceGate,
		versions:       versions,
		ops:            ops,
		mcpRT:          mcpRT,
		skills:         skills,
		// US-9 追加的只读装配态：全部是本次装配里**已经算出来的值**的保留，不新增读取。
		// US-9's read-only assembly state: values already computed here, merely retained.
		cfg:      cfg,
		entries:  entries,
		mem:      memSvc,
		mcpConns: mcpConns,
		root:     root,
		loader:   loader,
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

// declaredToolNames 是装配层注册池的**声明名集**（颗粒度文档 US-4 §2.4 注 4：名字在装配期
// 一次导出——`workspace` 不反向 import `tool`，装载器只认这份字符串清单）。两个消费面读的是
// 同一份：① 重路径在注册完十件工具之后拿它校验注册池（不一致即拒绝启动）；② 轻路径
// （`profile list` / `profile show` 经 `agentsPort`）不建注册池，直接拿它当池做装载判定
// （池外工具名 → 整只不注册，宪法 IV）。两条路因此对同一份 AGENT.md 给出同一个结论。
// 顺序 = `tool.Registry.Names()` 的排序序（比较逐名且保序）——**不得**按主题分组重排。
// declaredToolNames is the pool declared once at assembly time and read by both paths: the
// heavy path verifies its registry against it, the light path judges admission with it. The
// order matches Registry.Names(), which is sorted.
var declaredToolNames = []string{
	"http_get", "http_post", "list_dir", "manage_schedule", "notify", "read_file",
	"recall_memory", "save_memory", "shell", "write_file",
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

// memoryCarrier 按 `memory.backend` 选定记忆载体（颗粒度文档 §3.1 的装配行）：markdown 档建在
// 工作区根之上（全部读写经根句柄，Agent 层目录与文件按需创建），sqlite 档复用重路径已打开的
// 库句柄与**同一个时钟**（时间显式传入，载体内部不取 time.Now）。
//
// **空串归 markdown**：它只有两条来路——配置文件缺失 / 没有 memory 段的实例配置，以及测试夹具
// 直接构造 `cli.Config`（不经过装载期校验）——两者都是"没配"而不是"配错"，缺省与装载期取同一档
// （`cli.validateConfig` 已把空串就地写回该缺省，这里是同一条口径的第二处落点）。取值域外的取值
// **再拒一次**：装配层不假设调用方一定走过装载期那道门（fail-closed 双保险，颗粒度文档 §3.5
// 第 9 项）。
// memoryCarrier picks the carrier named by memory.backend; an empty string means "not
// configured" and lands on the documented default, while any value outside the two backends
// is refused again here rather than silently degraded.
func memoryCarrier(cfg *cli.Config, db *storage.DB, root string, now func() time.Time) (core.LongTermMemoryStore, error) {
	switch cfg.Memory.Backend {
	case "", "markdown":
		return memory.NewMarkdownStore(root), nil
	case "sqlite":
		return storage.NewMemoryStore(db, now), nil
	default:
		return nil, fmt.Errorf(
			"配置项 memory.backend 取值非法（%s）——只允许 markdown（缺省）或 sqlite；mem0 载体尚未实现，未知取值一律拒绝启动，不静默降级",
			cfg.Memory.Backend)
	}
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

	// runs 是运行账本的唯一实例：AgentService 写入、web.RunService 适配器读取，
	// 保留句柄是为了让装配面可核"读面与写面确实是同一个仓储"。
	// runs is the one run store shared by the writer and the web read port.
	runs *storage.RunStore

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

	// mcp 是 MCP 连接池（零配置时为 nil）。停机时必须先于库关闭：池收起子进程与会话，
	// 之后才是写池的最后一批使用者（约束 22：scheduler → MCP → 库 → 锁）。
	// The MCP pool (nil without configuration); it must close before the database.
	mcp mcpConnector

	// scheduler 是运行期调度器（重路径始终构造，chat / tool 不 Start）。停机时**最先**
	// 停它：Stop 可耗满 70s 宽限，必须在库关闭之前收尾在途运行（约束 22）。
	// The scheduler is always constructed; Stop runs first so in-flight runs finish
	// before the database closes.
	scheduler *core.Scheduler

	// scheduleSlot 是 `manage_schedule` 的两段绑定槽位（颗粒度文档 §3.1.17）：注册池
	// 先于 runtime 构建，工具持槽、runtime 就绪后 Bind 生产适配器。保留句柄只为让装配
	// 面可见"第 10 件工具确实完成绑定"，不把适配器暴露给任何消费方。
	// The two-stage slot for manage_schedule; kept only to make the binding visible.
	scheduleSlot *tool.ScheduleEditorSlot

	// scheduleEditor 是绑定在槽位上的生产适配器本身（**同一实例**）。配置热加载让
	// 调度器增减 Agent / 对账任务时必须走它：适配器维护的私有 claim 基线随热加载
	// 同步增减（约束 39），换一个只用裸 scheduler 的联动器会让基线分叉。
	// scheduleEditor is the very adapter bound to the slot, shared with hot reload so
	// the private claim baseline stays in step with the scheduler.
	scheduleEditor *scheduleEditorAdapter

	// watch 是 serve 打开的配置监听（只有 serve 赋值；其余重命令为 nil）。停机时
	// **最先**关闭它：它会在回调里推进注册表并联动调度器，必须赶在调度器停止与库
	// 关闭之前把最后一个防抖窗口收完（约束 22 的次序在此向前延长一格）。
	// watch is the serve-time config watcher (nil for other heavy commands); it closes
	// first so no reload callback can touch the scheduler after it stops.
	watch *workspace.Watcher

	// agents 是进程内唯一的 Agent 注册表（§3.1.20 / FR-22）：运行准入、管理台列表与
	// 调度器读的是同一份不可变快照，热加载以整份替换发布新版本。
	// The one in-process agent registry shared by admission, the console and the scheduler.
	agents *agentRegistry

	// agentGate 是 Agent 级读写锁表（约束 25）：准入持读锁、启停迁移与内容条件写持写锁，
	// 三者必须是同一实例——保留句柄让装配面可核"确实只有这一份"。
	// The per-agent gate shared by admission, lifecycle transitions and content edits.
	agentGate *core.AgentGate

	// lifecycle 是生命周期状态文件的读写面：准入经它现场重读（失败关闭），管理面经它
	// 迁移；与 `agentGate` 同属一份共享组合。
	// lifecycle is the state-file surface admission re-reads and the console transitions.
	lifecycle *workspace.LifecycleStore

	// resourceGate 是发布 / 准入共用的进程级闸门（约束 46）：T045 的运行资源解析持读锁、
	// 资源发布持写锁。在此保留装配产物，确保全进程只有这一份实例。
	// The process-wide publish/admission gate later tasks resolve resources under.
	resourceGate *core.ResourceGate

	// versions / ops 是两个管理侧仓储：基线、管理恢复与准入解析共用同一实例，保留句柄
	// 让装配面可核"只有这一份"（与 runs / sessions 的保留口径相同）。
	// versions and ops are the resource and management stores shared across the assembly.
	versions *storage.ResourceStore
	ops      *storage.ManagementStore

	// mcpRT 是已发布 MCP 定义的只读运行期句柄（配置与连接两份名单）；skills 是技能库
	// 句柄——准入解析、基线发布与发布版本快照共用同一实例。
	// mcpRT and skills are the runtime handles shared by admission and the baseline.
	mcpRT  *mcpRuntime
	skills *workspace.SkillLibrary

	// root 是工作区根（`.basalt`）：内容条件写的编辑器与候选校验器都按它打开根约束；
	// loader 是装配期那个**唯一**的装载器实例——候选校验必须与热加载共用同一实例，
	// 两者的 writeMu 才是同一把锁（内容写与全量 rescan 不得交错）。
	// root is the workspace root; loader is the single loader instance shared with hot
	// reload, so candidate validation and rescans serialize on the same write lock.
	root   string
	loader *workspace.AgentLoader

	// 以下为 US-9 追加的只读装配态（均为既有局部值的保留，不新增读取）：
	// US-9 retains the read-only assembly state computed above.
	//
	// cfg 是**生效配置**：配置文件缺失时装配用的是 `&cli.Config{}`，此处保存的是同一个值，
	// 绝不可能是 nil——HTTP 装配面因此不必再判一次"配置在不在"。
	// cfg is the effective configuration, never nil.
	cfg *cli.Config

	// entries 是**全部**装载结果（含未注册项）——`/profiles` 要把未注册原因作为 problems
	// 呈现出来，只保留注册成功项会让拒绝原因静默消失。
	// entries keeps every loader result, rejects included, for /profiles' problems.
	entries []core.AgentEntry

	// mem 是记忆门面（`/memory` 两分区读面）；与工具侧消费的是同一个实例。
	// mem is the one memory facade shared with the tool side.
	mem *memory.Service

	// mcpConns 是已连接的 MCP server 与工具（`/info` 的连接数与 `/tools` 的 mcp 来源）。
	// mcpConns carries the connected MCP servers and their tools.
	mcpConns []tool.MCPConnection
}

// Close 按**相反次序**释放：配置监听 → 调度器 → MCP 连接池 → 数据库 → 实例锁
// （约束 22）。监听最先关：它的回调会推进注册表并联动调度器，必须在调度器停止与库
// 关闭之前把最后一个防抖窗口收完（关闭会等回调退出，幂等，与 ctx 取消并发安全）。
// 调度器随后停：Stop 会在 70s 宽限内等待在途运行，库必须活到它收尾之后；随后池收
// 子进程与会话，最后才关写池的使用者（库），锁等所有写入收尾之后才松开。每处失败
// 并入同一条错误链，释放失败不得静默。
// Close releases in reverse order — watcher, scheduler, MCP pool, database, lock.
func (r *heavyRuntime) Close() error {
	var errs []error
	if r.watch != nil {
		errs = append(errs, r.watch.Close())
	}
	if r.scheduler != nil {
		r.scheduler.Stop()
	}
	if r.mcp != nil {
		errs = append(errs, r.mcp.Close())
	}
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
