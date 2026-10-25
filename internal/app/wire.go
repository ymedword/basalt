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

	// 别名：本文件里 `catalog` 是运行期模型目录的局部变量（providerCatalog），
	// 只读 MCP 目录用 `mcpcat` 以免两件事共用一个标识符。
	// Aliased: `catalog` below is the runtime provider catalog, a different thing.
	mcpcat "github.com/ymedword/basalt/internal/catalog"

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

	// stepLock / stepOpen / stepMigrate / stepRecover / stepBaseline / stepRecovery /
	// stepReconcile / stepCatalog / stepRegistry 是重路径**九个**次序读数（图 A 的判据）：
	// 日志里 `step` 键的取值必须**按此序**出现——顺序错了就是接线错了。基线排在崩溃恢复
	// 之后、管理面恢复之前（注 5）：先修账本与工作区残留，再补资源指针与生命周期状态文件，
	// 最后才查证上次留下的 pending 管理操作。US-12 追加后三步（约束 18 / 图 A ⑤⑥⑦）：
	// 定义对账先于运行目录与注册表——目录只由已发布版本构建，手工改文件不可能绕过对账直接
	// 生效。
	// The nine ordering readings: the `step` values must appear in exactly this order.
	stepLock      = "lock"
	stepOpen      = "open"
	stepMigrate   = "migrate"
	stepRecover   = "recover"
	stepBaseline  = "baseline"
	stepRecovery  = "recovery"
	stepReconcile = "reconcile"
	stepCatalog   = "catalog"
	stepRegistry  = "registry"
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

	// 存储实例在迁移**之前**构造：v3 迁移的中间步骤要写操作台账、状态文件与绑定投影，
	// 它们必须与之后所有消费面是**同一批**实例——迁移写下的行与运行期读到的行因此来自
	// 同一个连接池。构造只持句柄、不读表，早于迁移不违反"先迁移后使用"。
	// The stores are built before the migration because its middle steps write the
	// operation ledger, state files and binding projection; every consumer must share
	// these very instances. Constructing them only holds handles and reads nothing.
	versions := storage.NewResourceStore(db)
	credentials := storage.NewCredentialStore(db)
	// identities 是资源身份墓碑表：结构化配置读面按绑定 UID 现场查名称与删除事实。
	// identities is the uid tombstone table read by the config view.
	identities := storage.NewIdentityStore(db)
	notifyAdminStore := storage.NewNotifyAdminStore(db)
	bindingStore := storage.NewBindingStore(db)
	archives := storage.NewAgentAdminStore(db)
	ops := storage.NewManagementStore(db)
	lifecycle := workspace.NewLifecycleStore(root, a.nowOr())
	channels := storage.NewNotifyChannelRepo(db)
	// connectionTests 是「最后一次连接测试」投影（R7 范围修订、schema v4）：模型连接与
	// 通知渠道的测试写同一张表，管理读面与两个测试端点共用这一实例——页面读到的就是
	// 上一次真实测试留下的行；历史明细仍在管理操作台账。
	// The last-test projection shared by the two test endpoints and the admin read faces.
	connectionTests := storage.NewConnectionTestStore(db)
	// networkPolicy 是出网域名白名单的唯一权威行（schema v5）：启动种子、运行期条件写、
	// 启动恢复查证共用同一实例（R8 裁决 2）。
	// networkPolicy is the single authoritative allowlist row shared by seeding, the
	// conditional write and startup recovery.
	networkPolicy := storage.NewNetworkPolicyStore(db)

	// ③ 显式迁移：升级是持锁重命令的**显式动作**，不做任何"顺手改库"。六步编排是唯一
	// 的迁移路径（准入 → 状态文件 → SQL 单事务 → 身份回填 → 绑定回填 → 投影重建）：
	// 库已是目标版本时返回零报告，不写操作行。
	// The six-step orchestration is the only migration path; an up-to-date database is
	// left untouched with no operation row.
	migrator := NewMigrator(MigrateV3Deps{
		DB:         db,
		Operations: ops,
		Lifecycle:  lifecycle,
		Resources:  versions,
		Notify:     notifyAdminStore,
		Bindings:   bindingStore,
		Root:       root,
		Logger:     logger,
		Now:        a.nowOr(),
		Target:     a.supportedVersion,
	})
	if _, err = migrator.MigrateV3(ctx); err != nil {
		return nil, abortRuntime(fmt.Errorf("迁移数据库: %w", err), db, lock)
	}
	logger.Info("重路径：schema 迁移完成", "step", stepMigrate, "schema_version", a.supportedVersion)

	// 凭据五件（解析器 / 守卫 / Run 检查点 / 写面 / secrets 服务）在迁移之后、工具注册
	// 之前一次装配：通知工具、运行目录构建、准入固定与 CLI 的 secrets 子命令消费的都是
	// 这一组实例。解析器只拿主密钥**路径**，密钥本身每次解析时重新读取（不常驻内存）。
	// The five credential components are wired once after migration and before the tools;
	// the resolver holds only the key path — the key is re-read on every resolution.
	masterKeyFile := cfg.Secrets.MasterKeyFile
	resolver, err := newCredentialResolver(CredentialResolverDeps{
		Credentials:   credentials,
		Resources:     versions,
		Notify:        notifyAdminStore,
		Bindings:      bindingStore,
		MasterKeyFile: masterKeyFile,
		WorkspaceRoot: root,
		Logger:        logger,
	})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	guard, err := NewCredentialGuard(CredentialGuardDeps{Credentials: credentials, Logger: logger})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	checkpoint, err := NewRunCredentialCheckpoint(RunCredentialCheckpointDeps{
		Pins:  storage.NewRunCredentialStore(db),
		Guard: guard,
	})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	credentialOps, err := NewCredentialOps(CredentialOpsDeps{
		Operations:    ops,
		Credentials:   credentials,
		Resources:     versions,
		Notify:        notifyAdminStore,
		MasterKeyFile: masterKeyFile,
		WorkspaceRoot: root,
		Logger:        logger,
		Now:           a.nowOr(),
	})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	secrets, err := NewSecretsService(SecretsServiceDeps{
		Credentials:   credentials,
		Channels:      channels,
		Ops:           credentialOps,
		Resolver:      resolver,
		MasterKeyFile: masterKeyFile,
		WorkspaceRoot: root,
		Logger:        logger,
	})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}

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
	// 工具来源表（US-14 D-06）与注册池**同生共死**：注册池只存 `core.Tool`，来源与跳过
	// 原因是装配期事实，因此在这里建表、在内置注册与 `OnConnected` 两处登记，最后挂到
	// `mcpRT` 上供视图读取。表只写装配期，运行期只读——不改注册池的重名语义。
	// The disposition table is built beside the registry and filled at both registration
	// sites; it records assembly facts only and never changes duplicate semantics.
	dispositions := newMCPDispositionTable()
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
		// 托管凭据解析面在此接线：通知按**逐次发送**解析当前版本（不像模型 Run 那样在
		// 准入固定），解析失败即失败关闭，不回退明文；env 占位渠道不经此路。
		// Managed notify credentials resolve per send; a failure is fail-closed.
		newNotifyTool(channels, policy, resolver,
			notifyDeliveryRecorder{store: storage.NewNotifyDeliveryStore(db), logger: logger}),
		tool.NewManageScheduleTool(scheduleSlot),
	} {
		if err := registry.Register(t); err != nil {
			return nil, abortRuntime(fmt.Errorf("注册工具: %w", err), db, lock)
		}
		// 内置先注册即先占名（D-06）：同名 MCP 工具随后注册必然失败，赢家来源一处可查，
		// 不必从注册池反推。
		// Builtins register first and therefore own their names; the table records the win.
		dispositions.recordBuiltin(t.Name())
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

	// MCP 配置解析在等值门禁**之后**（约束 9），但**不再在这里连接**：连接生命周期自
	// US-12 起归 `MCPSupervisor`（图 A ⑧），而运行对象只由已发布版本构建（FR17 / 约束
	// 18）——supervisor 因此在基线、对账与运行目录之后才构造。本段只取文件名集，供启动
	// 基线之前的第一遍装载使用。零 server 时解析结果为空，后续整段 supervisor 跳过。
	// MCP parsing stays after the equality gate, but connecting moved to the supervisor,
	// which runs only after the baseline and reconciliation publish the runtime set.
	mcpSpecs, err := workspace.ParseMCPServers(root)
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("解析 MCP 配置: %w", err), db, lock)
	}
	fileServers := mcpConfigNames(mcpSpecs)

	// fail（§3.5 改造点 ⑥）：supervisor 一经建立就可能持有真实子进程——自这一刻起的每一条
	// 装配失败路径都**先收连接、再走 abortRuntime**（库 → 锁），半开会话不得留在失败路径上。
	// 尚未构造时闭包退化为原 abortRuntime（零 MCP 配置时行为逐字节不变）。
	// From the moment the supervisor exists, every failure path stops it before releasing
	// the database and the lock.
	var mcpSupervisor *MCPSupervisor
	var mcpConns []tool.MCPConnection
	// networkAllowlist 保留在本作用域：它在 supervisor 之前装载权威白名单（MCP http
	// 连接用的是**已装载**的规则），随后投影给 web 管理面。
	// The allowlist service seeds before the supervisor connects, then reaches the web
	// management surface.
	var networkAllowlist *NetworkAllowlist
	fail := func(err error) (*heavyRuntime, error) {
		if mcpSupervisor != nil {
			err = errors.Join(err, mcpSupervisor.Close())
		}
		return nil, abortRuntime(err, db, lock)
	}

	// MCP 目录是**编译期内嵌快照**（D-01）：一次解析、全进程共享——请求期不重读文件、
	// 不联网、不启动进程。解析失败是发布缺陷，按装配失败关闭：**不得**用空目录充数，
	// 那会让页面显示"一条配方都没有"而不是"这版制品坏了"。
	// The catalog is a build-time snapshot parsed once here; a parse failure is a release
	// defect and aborts startup instead of silently serving an empty catalog.
	mcpCatalogSnapshot, err := mcpcat.Builtin()
	if err != nil {
		return fail(fmt.Errorf("解析 MCP 目录快照: %w", err))
	}

	// ⑥ 注册池名字集喂装载器：workspace **不得**反向 import tool，名字在装配期一次导出
	// （§2.4 注 4）。声明了池外工具名的 Agent 整只不注册（宪法 IV）。第一遍装载只服务启动
	// 基线的名字集，此刻 MCP 工具尚未连接、发布版本尚不存在（首启）：把"已连接"置为文件
	// 集是**乐观占位**，避免把每个引用 MCP 的 Agent 记成第三态；注册结论不采信这一遍，
	// 目录之后按 supervisor 的实际连接结果重判（约束 21）。
	// The first pass only feeds the baseline's name set; the connected set is an optimistic
	// stand-in because no connection verdict exists yet, and the registry pass re-judges.
	baselineServers := fileServers
	// 存储实例已在 ② 与 ③ 之间一次建好（迁移也要用它们），本段起的基线、对账、目录与
	// 注册表读的都是**同一个**实例（另建一份会让"读到的指针"来自不同的连接池快照）。
	// The stores were created once before migration; the baseline, reconciliation, catalog
	// and registry below all read those same instances.
	//
	// 第一遍装载只服务启动基线的**名字集**（基线按目录逐个对齐生命周期状态文件）；
	// 它的 Provider 名集取"配置文件 ∪ 未删除发布指针"的并集：首启尚无发布版本（并集 =
	// 文件），后续启动时"文件里删掉但指针保留"的 Provider 也不会在这里把 Agent 误判成
	// "provider 未注册"——那只会污染日志。注册结论不采信这一遍：目录之后按**已发布**
	// 名集重判（约束 18 / T032）。
	// The first pass only feeds the baseline's agent name set; the registry re-judges
	// against the published provider set once the catalog exists.
	baselineProviders, err := providerNameUnion(ctx, versions, cfg)
	if err != nil {
		return fail(err)
	}
	baselineLoader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers:    baselineProviders,
		Tools:        registry.Names(),
		MCPServers:   baselineServers,
		MCPConnected: baselineServers, // 乐观占位：连接判定由目录之后的第二遍给出
		Logger:       logger,
	})
	entries, err := baselineLoader.LoadAll(root)
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
	mcpDefs, err := loadMCPServerDefinitions(root, fileServers)
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

		// US-12 动作组的查证面：工作区根与实例配置用于读定义文件，`skills` / `agentGate` /
		// 两个管理仓储与写路径是**同一批**共享实例——恢复只能看到写路径真实的留痕。
		// The US-12 verification surfaces are the very instances the write paths use.
		Root:       root,
		ConfigPath: filepath.Join(a.workDir, configFileName),
		Skills:     skills,
		Gate:       agentGate,
		Archives:   archives,
		Notify:     notifyAdminStore,
		// 凭据四证（FR-081 / 契约 §9.4）与库版本读数：与写路径同一个凭据仓储，读到的
		// 就是写路径留下的提交事实；`db` 回答 v3 迁移是否真的完成。
		// The credential store is the same instance the write paths use; db answers the
		// schema version for the v3 baseline-v3 verdict.
		Credentials: credentials,
		Schema:      db,
		// 出网白名单的三态查证读同一张权威表（R8.2.4 第 6 条）。
		NetworkPolicy: networkPolicy,
	}); err != nil {
		return fail(err)
	}
	logger.Info("重路径：管理面恢复完成", "step", stepRecovery)

	// 定义对账（约束 18 / 约束 34 / 图 A ⑤）：读取定义文件与已发布版本，按四向表处理差异
	// ——一致跳过；文件新且候选合法则建新版本并发布；文件无效应保留已发布版本并 WARN；
	// 条目被删则忽略并保留。对账**必须**在运行目录与注册表之前：运行对象只由已发布版本
	// 构建，手工编辑因此不可能绕过发布闸门直接生效。发布写库失败 → 启动失败，不带病运行。
	// Startup reconciliation runs before the catalog and the registry, so a hand edit can
	// never bypass the publish gate; a failed publish aborts startup.
	reconcileResult, err := ReconcileDefinitions(ctx, ReconcileDeps{
		ConfigPath:     filepath.Join(a.workDir, configFileName),
		MCPServersPath: filepath.Join(a.wsRoot(), mcpServersFileName),
		Store:          versions,
		Operations:     ops,
		Skills:         skills,
		Gate:           resourceGate,
		Logger:         logger,
		Now:            a.nowOr(),
		// 托管条目的候选核对与准入期同一判据（只读、不解密）；没有它，手改托管
		// 模型连接会被误判成缺占位而永远对不上账。
		Credentials: resolver,
	})
	if err != nil {
		return fail(fmt.Errorf("启动定义对账: %w", err))
	}
	// 全量重建绑定投影（约束 38 / 图 A ⑤）：投影是 `AGENT.md` 的可重建派生视图，对账把
	// 文件与已发布版本对齐之后必须从**文件**重算，不能沿用上一进程的残行。它与对账同属
	// 一个启动阶段（五阶段判据不变）：扫描失败按工作区级问题失败关闭，单 Agent 失败记
	// WARN、留给准入闸门按"可能引用"处理。
	// The full projection rebuild belongs to the same startup stage as reconciliation; a
	// scan failure aborts startup while per-agent failures stay fail-closed at the gate.
	projection := NewBindingProjection(bindingStore, versions, archives, root, logger, a.nowOr())
	projectionReport, err := rebuildStartupBindings(ctx, projection, logger)
	if err != nil {
		return fail(err)
	}
	logger.Info("重路径：启动定义对账完成", "step", stepReconcile,
		"published", reconcileResult.Published, "retained", reconcileResult.Retained,
		"unchanged", reconcileResult.Unchanged, "ignored", reconcileResult.Ignored,
		"bindings_rebuilt", projectionReport.Rebuilt,
		"bindings_cleared", projectionReport.Cleared,
		"bindings_failed", len(projectionReport.Failures))

	// 凭据启动校验（R4.8「启动顺序」第四步）：投影已从文件重算，此刻按**已注册** Agent 的
	// 绑定查验每一份托管凭据——密文可解、key_id 对得上、模型连接的绑定目标与当前发布版本的
	// base_url 一致。判定不了的 Agent（未注册 / 不可读）不参与校验：它们本就进不了运行面。
	// 校验失败即拒绝启动：带着解不开的密文开跑，第一次模型调用才炸，那时已经晚了。
	// Startup credential verification runs after the projection rebuild and before the
	// catalog: an unusable managed credential must stop the start, not the first run.
	if err := resolver.VerifyStartup(ctx, registeredAgentNames(entries)); err != nil {
		return fail(fmt.Errorf("凭据启动校验: %w", err))
	}

	// ⑥ 运行目录：模型调用唯一入口 + 四个仓储 + 自实现循环 + 显式时钟。provider 映射表在
	// 这一次装配里**一次构建**（宪法 III：运行时只查表），且**只来自已发布版本**（约束 3 /
	// 约束 16）：运行准入拿它做"发布的 Provider 是否可用"判定，模型调用拿它做路由——两个
	// 消费面共享同一份目录，表不可能分叉；构建失败在这里失败关闭，不退回配置文件解析结果。
	// The one explicit provider catalog is built here from published versions only and
	// shared by admission and the model client; a build failure aborts the assembly.
	// 托管凭据来源就是上面那份解析器：目录构建期解密 `credential_mode: managed` 的条目，
	// 与 Run 准入时的固定走同一条解析路径（同一把主密钥、同一套失败关闭）。
	// The catalog decrypts managed published entries through the very same resolver
	// admission later pins from.
	catalog, err := newProviderCatalogFromPublished(ctx, versions, a.providerOverride, logger, resolver)
	if err != nil {
		return fail(err)
	}
	logger.Info("重路径：运行目录构建完成", "step", stepCatalog, "providers", len(catalog.clients))
	// 运行期目录的**唯一原子持有者**（约束 16）：资源写面在发布 / 撤回 / 删除的提交段
	// `Store` 一次，准入与模型调用从 `Load` 读同一张表。装配期以首次构建的目录初始化——
	// 构建失败已在上一行失败关闭，持有者不会带着空目录启动。持有者同时是注册表的构建
	// 来源（下面第二遍装载）与运行期路由的读取点（`catalogProviderRouter`）。
	// The one atomic holder of the runtime catalog: the write surface swaps it under the
	// gate, and both the registry pass below and the live router read it.
	catalogStore, err := newProviderCatalogStore(catalog)
	if err != nil {
		return fail(err)
	}

	// ⑧ MCP supervisor（图 A ⑧ / 约束 17 / 21 / 42）：运行对象只由**已发布版本**构建
	// （FR17 / 约束 18），按配置顺序逐个过策略并连接；`applied` 只在连接成功后于
	// ResourceGate 写锁内落库，连接失败保留旧 applied 并标记未连接。它必须在注册表之前
	// 完成——装载器要用一份**已知**的已连接集判定引用 MCP 的 Agent 是否注册（约束 21：
	// 载入期必须已应用且已连接）。图 A 标题写「注册表 → supervisor」，图体把 supervisor
	// 放在注册表之前；按图体与约束 21 执行（T049 D 项已登记）。
	// The supervisor applies published versions before the registry pass, as constraint 21
	// requires; figure A's body order wins over its title (recorded for T049).
	publishedMCP, err := publishedMCPServers(ctx, versions, fileServers, os.LookupEnv)
	if err != nil {
		return fail(fmt.Errorf("解析已发布 MCP server: %w", err))
	}
	mcpDeps := MCPSupervisorDeps{
		Policy: policy,
		Gate:   resourceGate,
		Logger: logger,
		Now:    a.nowOr(),
		// applied 的落库只在发布指针仍指向该版本时发生：期间若有更新的发布，写入旧版本
		// 会把指针倒退——直接跳过，`pending_restart` 自然为真。
		MarkApplied: func(markCtx context.Context, name string, versionID int64) error {
			return markMCPApplied(markCtx, versions, name, versionID, a.nowOr())
		},
		// 工具适配器随连接一起进入注册池与视图；注册序 = 配置顺序 → 工具列表序，重名
		// keep-first（内置优先，约束 8），其余工具照常注册。
		OnConnected: func(conns []tool.MCPConnection) {
			for _, conn := range conns {
				for _, t := range conn.Tools {
					regErr := registry.Register(t)
					// 每条 offered 工具都进来源表：注册成功记赢家，重名记跳过并写明赢家
					// 来源（D-06）。注册池自身的行为一个字都不改——失败照旧跳过。
					// Every offered tool is recorded; the registry's own semantics stay.
					dispositions.recordMCP(conn.Server, t.Name(), regErr)
					if regErr != nil {
						continue
					}
				}
			}
			mcpConns = append(mcpConns, conns...)
			logger.Debug("MCP 工具进入注册池", "tools", mcpToolNames(conns))
		},
		Root:           root,
		EnvPassthrough: cfg.Shell.EnvPassthrough,
	}
	// 测试注入替身时，supervisor 的每次连接都走 `App.mcpConnect`（生产为 nil，用真实
	// 连接池）。只在有替身时设缝：nil 会让 `newConnector` 回退到零值并空指针。
	// Tests inject the connector through App.mcpConnect; production leaves it nil.
	if a.mcpConnect != nil {
		fake := a.mcpConnect
		mcpDeps.NewConnector = func() mcpConnector { return fake }
	}
	mcpSupervisor = NewMCPSupervisor(mcpDeps)
	// 出网白名单在 supervisor 连接之前装载（R8.2.1）：表内已有行即应用它并**忽略**
	// `basalt.yaml`；无行才用配置作首次种子。`policy` 与工具、通知、supervisor 是同一
	// 实例，装载完成后的第一次判定就按权威值——MCP http 连接也包含在内。
	// The allowlist loads before the supervisor connects, so the very first policy
	// decision (including MCP http connects) uses the authoritative value.
	networkAllowlist = NewNetworkAllowlist(NetworkAllowlistDeps{
		Store:      networkPolicy,
		Operations: ops,
		Policy:     policy,
		Supervisor: mcpSupervisor,
		Logger:     logger,
		Now:        a.nowOr(),
	})
	if err := networkAllowlist.EnsureSeeded(ctx, cfg.HTTP.AllowedDomains); err != nil {
		return fail(fmt.Errorf("装载网络域名白名单: %w", err))
	}
	if err := mcpSupervisor.ApplyPublished(ctx, publishedMCP); err != nil {
		return fail(fmt.Errorf("启动 MCP supervisor: %w", err))
	}
	mcpRT := newMCPRuntimeFromSupervisor(mcpSupervisor, mcpPublishedConfigs(publishedMCP))
	// 来源表挂在运行期句柄上（视图经 `mcpRT.ToolDisposition` 读）：supervisor 的
	// `OnConnected` 已在上面写完本表，此处只做一次引用交付，不复制。
	// The table is handed to the runtime handle once; the view reads it through there.
	mcpRT.dispositions = dispositions

	// 第二遍装载 = 注册表的构建来源（约束 18 / T032）：装载器在目录之后重建，Provider
	// 名集取**已发布版本**的键（`catalogProviderNames` 显式枚举、升序），不再读
	// `cli.Config.Providers`——文件里有、却没发布的条目不会让 Agent 注册；发布过、文件里
	// 已删但指针保留的条目仍可被引用。这一遍的 entries 同时是调度注册集、管理台列表与
	// watcher 热加载共用的那份。装载器保留句柄：`manage_schedule` 的编辑适配器与装载共用
	// **同一个**实例，`ApplySchedule` 的 writeMu 因此也覆盖未来的装载调用（约束 29 / 39），
	// 不让第二条写入路径从另一个实例进入。
	// The registry pass runs after the catalog and judges against the published provider
	// names only. The loader handle is retained so the schedule editor shares this very
	// instance and its write lock stays the single gate for workspace writes.
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		// 每次判定读**当前**运行期目录：模型连接在启动后发布 / 撤回 / 删除时，
		// 候选校验与热加载立即看到新名集（否则页面里新建的连接要重启才能绑定，
		// 2026-10-23 浏览器走查复现）。目录本身在 ResourceGate 写锁内原子替换，
		// 读到 "一半新一半旧" 不可能发生。
		// Every validation reads the current runtime catalog, which is replaced
		// atomically under the ResourceGate write lock.
		ProvidersFunc: func() []string { return catalogProviderNames(catalogStore.Load()) },
		Tools:         registry.Names(),
		MCPServers:    mcpRT.ConfiguredNames(),
		MCPConnected:  mcpRT.ConnectedNames(),
		Logger:        logger,
	})
	entries, err = loader.LoadAll(root)
	if err != nil {
		return fail(fmt.Errorf("装载 Agent 目录: %w", err))
	}

	// 注册装配的技能发布过滤（约束 36 / FR-042，F-03 A）：装载器只校验技能实体，绑定
	// 指针是否存在必须在这里补判——启动失败关闭（读不到指针即拒绝启动），与 watcher 的
	// 热加载判定共用同一个函数。
	// The skill-publication filter completes the registration invariant after the loader
	// and before the registry; a failed lookup aborts startup, exactly like the watcher.
	entries, err = registryEntriesWithSkillPublication(ctx, versions, entries, logger)
	if err != nil {
		return fail(fmt.Errorf("注册装配: %w", err))
	}

	// ⑦ 不可变注册表（§3.1.20 / 图 A）：装载结果在恢复、对账与目录之后冻结成首版快照。
	// 它是管理台列表、运行准入与调度器**共同**的读面——热加载之后以整份替换发布新版本，
	// 三处因此不可能各自看到不同的 Agent 集合。
	// The immutable registry is published after recovery, reconciliation and the catalog;
	// it is the single read face for the console, admission and the scheduler.
	agents := newAgentRegistry(root, entries, logger)
	logger.Info("重路径：Agent 注册表建立", "step", stepRegistry, "agents", len(entries))
	sessions := storage.NewSessionStore(db)
	invocations := storage.NewToolInvocationRepo(db)
	llmCalls := storage.NewLLMCallRepo(db)
	// 运行账本只建一次：AgentService 写、web.RunService 适配器读，同一实例保证
	// "读到的"就是"写下的"（两条连接池之间的影子账本会让排障无从对齐）。
	// One run store is shared by the writer (AgentService) and the reader (the web port).
	runs := storage.NewRunStore(db)
	// 模型调用的唯一入口按**当前**目录路由（约束 16 / 17）：发布在 ResourceGate 写锁内
	// 替换持有者后，下一个新 Run 与下一次作者调用立即读新表；在途 Run 拿的是准入时固定的
	// `Pinned` 视图，不受影响。路由仍只查表，查不到即拒绝发出（宪法 III）。
	// The one model entry point routes against the current catalog; admitted runs keep
	// the pinned view captured at admission.
	client := newCatalogProviderRouter(catalogStore, llmCalls, logger)
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
		Resources: NewRunResources(versions, catalog, client, mcpRT, skills, resourceGate, AdmissionDeps{
			Identities:  versions,
			NotifyAdmin: notifyAdminStore,
			Channels:    channels,
			Bindings:    bindingStore,
			// 凭据三件同进同出（R4.8 / 约束 44）：准入在资源闸门读锁内固定模型连接的凭据
			// 版本（`CredentialPins`），绑定检查按解析器的判定拒绝不可用连接（`Credentials`），
			// 每次模型调用前按 Run 固定的版本重查撤销状态（`Checkpoint`）。缺任何一件，
			// 托管连接要么无法固定、要么撤销后仍能调用——装配层不给"半套"。
			// The three credential pieces are supplied together; a partial wiring would let a
			// revoked credential keep serving an in-flight run.
			Credentials:    resolver,
			CredentialPins: resolver,
			Checkpoint:     checkpoint,
		}),
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
	// 系统代投（R7）：调度器只给目标与正文，预写 / 资格复核 / 三分结果 / 恢复都在
	// 投递服务里；发送实现与 `notify` 工具共用适配器表与凭据解析器。
	// System delivery shares the adapter table and credential resolver with the notify tool.
	// 发送面**只构造一次**：定时代投、`notify` 工具与渠道连通性测试共用这一个
	// `DeliverySender`——三条路径因此共用同一张适配器表、同一份凭据解析与同一道授权。
	// The one DeliverySender shared by scheduled delivery, the notify tool and the admin
	// connectivity test, so all three share the adapter table, resolver and policy.
	deliverySender := tool.NewDeliverySender(channels, policy, newNotifyAdapters(), resolver)
	// 注册表一起传入：运行前除范围资格外还要复核"目标在 Agent 声明的渠道内"
	//（R7 的权限边界），注册表缺失时投递服务失败关闭。
	delivery := newDeliveryService(
		storage.NewDeliveryStore(db),
		deliverySender,
		lifecycle,
		agents,
		logger,
	)
	scheduler := a.newScheduler(storage.NewTaskStore(db), service, agents.Entries(), delivery, cfg, logger)
	// 两段绑定的第二段（颗粒度文档 §3.1.17）：runtime 就绪后把生产编辑后端交给工具
	// 槽位。此后 `manage_schedule` 的写入门 = 归属预检 → 工作区写闸门 → 即时对账。
	// 适配器同时保留在运行期上：配置热加载的调度联动必须走**同一实例**（约束 39 的
	// 私有 claim 基线随之增减），另建一个只用裸 scheduler 的联动器会让基线分叉。
	// Second half of the two-stage binding: the production editor backend is installed
	// once the loader and scheduler exist, and the same instance is retained for hot reload.
	scheduleEditor := newScheduleEditorAdapter(loader, root, entries, scheduler, logger)
	scheduleSlot.Bind(scheduleEditor)

	// ⑧ US-12 写面（颗粒度文档 §3.1.5 / 图 A）：Agent 管理编排在这里**一次构造**。锁表、
	// 生命周期存储、资源仓储与管理操作仓储全部传上面那几份共享实例——另建一份会让写锁与
	// 准入互斥静默消失；三个回调依赖按**每次调用**的当前快照回答，不缓存列表。作者提示词的
	// 模板骨架取自 workspace 的同一个模板渲染器（与创建模板同源），装配层不另抄文本。
	// 归档路径的三个协作者（注册表 / 调度编辑后端 / 在途 Run 面）与归档行存储在这里**同时**
	// 提供（约束 32 的 all-or-nothing 装配面）：前两者传上面那几份共享实例，运行面经
	// `runCancellerAdapter` 读运行账本、送取消信号；CLI 写路径则整组省略（ports.go 的
	// `openCLIAdmin` 只装归档行存储，注销与等待在实例锁下没有消费者）。
	// The three archive collaborators are supplied together, as constraint 32 requires; the
	// CLI write path supplies none of them because it holds the instance lock.
	now := a.nowOr()
	authorTemplate, err := workspace.AgentTemplateSkeleton()
	if err != nil {
		return fail(err)
	}
	// 保存期绑定校验（内容条件写与多文件提交共用）：构造一次，闭包每次调用只查身份。
	// One save-time binding check shared by the content editor and submit paths.
	saveBindings := SaveBindingCheck(lifecycle, BindingCheckDeps{
		Resources:      versions,
		Notify:         notifyAdminStore,
		NotifyChannels: channels,
		Credentials:    resolver,
	})
	agentAdmin := NewAgentAdmin(AgentAdminDeps{
		Root:       root,
		Gate:       agentGate,
		Lifecycle:  lifecycle,
		Index:      versions,
		Operations: ops,
		Logger:     logger,
		Now:        now,
		PublishedProviders: func(ctx context.Context) ([]workspace.ProviderEntry, error) {
			return authorPublishedProviders(ctx, versions)
		},
		PublishedSkills: func(ctx context.Context) ([]string, error) {
			return authorPublishedSkills(ctx, versions)
		},
		AppliedMCPServers: func(ctx context.Context) ([]string, error) {
			return authorAppliedMCPServers(ctx, versions, mcpRT)
		},
		AuthorChat:      client,
		AuthorTemplate:  authorTemplate,
		MaxRequestBytes: cfg.HTTP.MaxRequestBytes,
		// 多文件提交的候选校验与内容条件写共用同一道装载期规则，外加同一条保存期
		// 绑定校验（卡 R4.9：正文提交必须显式给 `resource_bindings`，不自动补绑）。
		// Submit validation reuses the loader rules plus the same save-time binding
		// check as the content editor.
		ValidateDefinition: func(ctx context.Context, agent string, definition []byte) error {
			entry, _, _, validateErr := loader.ValidateCandidate(root, agents.Entries(), agent, definition, now())
			if validateErr != nil {
				return validateErr
			}
			return saveBindings(ctx, agent, entry.Profile)
		},
		// 归档路径的存储与三个协作者（约束 32）：HTTP 装配三件同时提供，归档行与操作终态
		// 同事务落库；`WaitRuns` 的有界窗口由编排层的 `BusyWait` 给（零值 = 缺省 30 秒）。
		// The archive store and its three collaborators are supplied together on the HTTP path.
		Archives:     storage.NewAgentAdminStore(db),
		Registry:     agents,
		Scheduler:    scheduleEditor,
		RunCanceller: runCancellerAdapter{runs: runs, service: service},
		// 复制前的源专属判定与映射目标校验读**同一批**资源存储；绑定投影重建面与启动
		// 全量重建共用同一个实例（失败集合因此只有一份真值）。
		// Copy-time checks and projection rebuilds share the assembly's stores and projector.
		Resources:   versions,
		NotifyAdmin: notifyAdminStore,
		Projection:  projection,
	})
	// 公共资源写面（颗粒度文档 §3.1.7 / 图 B）：版本仓储、管理操作仓储、发布 / 准入闸门
	// 与上文是**同一份**共享实例，运行期目录以原子持有者共享——发布在写锁内替换后，新
	// Run 与模型调用读同一张表（约束 16 / 17）。定义文件路径指实例配置本身：模型连接块
	// 就写在 basalt.yaml。删除闸门的 Agent 引用取**同一份**注册表快照（`Entries` 是只读
	// 副本），归档扫描以工作区根为界；评测注入的替身客户端与启动装配共用同一个值。
	// 使用范围编排（R4.4）：与资源写面共用同一个版本仓储——`versions` 同时满足 Agent
	// 目录读取面（身份索引与生命周期索引都是可重建镜像）；工作区根供转专属时的
	// 定义文件整词扫描（与删除闸门同一份扫描面）。本轮没有消息路由接入点，
	// `Routes` 留 nil（范围转换的取证端口保留，接入后再装）。
	// The scope orchestrator shares the same resource store and workspace root; message
	// routes do not exist in this revision, so that port stays nil.
	scopes := &ScopeService{
		Resources:     versions,
		Agents:        versions,
		WorkspaceRoot: root,
		Now:           now,
	}
	// The resource write orchestrator shares the assembly's stores, gate, catalog and
	// registry snapshot; the provider override is the very value used at startup.
	resourceAdmin := NewResourceAdmin(ResourceAdminDeps{
		ConfigPath:       filepath.Join(a.workDir, configFileName),
		Root:             root,
		Agents:           func() ([]core.AgentEntry, error) { return agents.Entries(), nil },
		ProviderOverride: a.providerOverride,
		Store:            versions,
		Operations:       ops,
		Gate:             resourceGate,
		Catalog:          catalogStore,
		Supervisor:       mcpSupervisor,
		// 诊断的命令定位面与启动**共用**构造器（C-04）：root 与 env passthrough 必须与
		// supervisor 连接时是同一份值，否则同一条配置在诊断与启动两条路上给出不同结论。
		// The diagnosis locator mirrors launch: same root, same env passthrough.
		DiagnoseMCP: func(spec core.MCPServerConfig) core.MCPCommandDiagnosis {
			return diagnoseMCPCommand(spec, root, cfg.Shell.EnvPassthrough)
		},
		Credentials: credentials,
		// 发布编排经同一份解析器做凭据写 / 绑定检查（保存前的"能不能解"、发布前的目标
		// 绑定比对），主密钥路径与启动校验因此只有一处来源。
		// Publishing reuses the assembly's resolver for credential checks.
		CredentialSource: resolver,
		// 保存路径上的凭据段与范围声明走同一套实例：托管写与退役不会另开第二条路径。
		// The save path reuses the same credential writer and scope orchestrator.
		CredentialWriter: credentialOps,
		Scopes:           scopes,
		// 引用视图的绑定投影与归档索引（R4.6）：与通知删除闸门共用同一份投影实例，
		// `bound_agents` 因此与删除判据同源；归档标志由归档索引的未恢复行给出。
		// The references view reads the same binding projection as the notify gate.
		Bindings: bindingStore,
		Archives: archives,
		// 最后一次连接测试投影：测试端点写完即被列表页读到（同一实例、零缓存）。
		// The last-test projection written by the test endpoint and read by the list.
		ConnectionTests: connectionTests,
		Logger:          logger,
		Now:             now,
	})
	// 通知渠道写面（颗粒度文档 §3.1.7 / 约束 38）：类型表取 `GET /notify-channels/types`
	// 的**同一装配来源**（`notifyTypeNames` 与发送面共用 `newNotifyAdapters`），不是另抄
	// 一份字面量；管理与发送共用同一张表、同一只读仓储，写后下一发即可见，零缓存。删除
	// 闸门以工作区根为界扫描活动与归档 `AGENT.md`，并按 UID 反查绑定投影与未释放的
	// 活动 Run 冻结；仓储 / 操作台账 / 绑定投影与上文是同一批实例（不另建第二套读数）。
	// The notify write surface shares the ordered type slice with the types endpoint and
	// the send path; the delete gate scans both active and archived AGENT.md documents and
	// reverse-looks-up the binding projection plus unreleased run freezes.
	notifyAdmin := NewNotifyAdmin(NotifyAdminDeps{
		Root:       root,
		Store:      notifyAdminStore,
		Bindings:   bindingStore,
		RunRefs:    storage.NewNotifyDeliveryStore(db),
		Operations: ops,
		Types:      notifyTypeNames(),
		Policy:     policy,
		Logger:     logger,
		Now:        now,
		// 托管地址与专属范围复用与模型连接**同一批**实例：凭据只经一个写面，范围只经
		// 一条转换路径，不按资源类别各造一套。
		// Managed addresses and exclusive scopes reuse the same instances as model
		// connections; there is exactly one credential writer and one scope path.
		CredentialWriter: credentialOps,
		Scopes:           scopes,
		// 连通性测试走**同一个**发送面（`deliverySender`），测试结果写同一张投影表；
		// 测试消息文案固定为 `core.NotifyConnectivityTestText`，接收方可与真实通知区分。
		// The connectivity test reuses the very sender and writes the same projection.
		TestSender:      deliverySender,
		ConnectionTests: connectionTests,
	})
	// 工作区只读浏览（颗粒度文档 §3.1.7 / 约束 40）：浏览根由服务自己拼成
	// `<工作区根>/agents/`，装配层只给工作区根；纯读、不持写锁、不写操作台账、
	// 不缓存条目，因此无需共享任何写面实例。
	// The read-only browser takes the workspace root and derives agents/ itself; it is
	// pure read with no shared write state, so it needs nothing else from the assembly.
	workspaceBrowse := NewWorkspaceBrowse(WorkspaceBrowseDeps{Root: root, Logger: logger})
	// 通知渠道管理读面（notify_read.go）：在**发送路径同一个**零缓存仓储之上补齐属主名、
	// 凭据状态与条件 ETag。列表端点与写后回显用同一实例，页面读到的与下一发会用的一致。
	// The channel admin read face wraps the same zero-cache repository the send path uses;
	// the list endpoint and the post-write echo share this one instance.
	channelRead := newNotifyChannelReadPort(storage.NewNotifyChannelRepo(db), credentials, versions,
		notifyReadDeps{ConnectionTests: connectionTests})
	// 资源状态动作编排（R4.4 / R4.8）：撤销与范围切换各缺一半——范围编排要求调用方
	// 给出操作标识却不写操作行，撤销编排不识别条件写——因此组合层在这里补齐
	// 「校验 → 预写 → 执行 → 回填 → 回读」。资源锁表、操作台账、渠道读面与注册表
	// 快照全部复用上面的同一批实例，不另建第二套（FR-041）。
	// The state orchestrator completes both endpoints: the scope service needs an
	// operation id without writing the row, and revoke ignores conditional writes.
	// It reuses the same lock table, operation store, channel read face and registry.
	resourceState, err := NewResourceStateService(ResourceStateDeps{
		Resources:     versions,
		Credentials:   credentials,
		Operations:    ops,
		Admin:         resourceAdmin,
		Scopes:        scopes,
		CredentialOps: credentialOps,
		Channels:      channelRead,
		Agents:        func() ([]core.AgentEntry, error) { return agents.Entries(), nil },
		Logger:        logger,
		Now:           now,
	})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}

	logger.Info("重路径装配完成", "agents", len(agents.Entries()), "tools", registry.Names(),
		"mcp_connected", len(mcpRT.ConnectedNames()))
	return &heavyRuntime{
		lock:             lock,
		db:               db,
		service:          service,
		runs:             runs,
		sessions:         sessions,
		invocations:      invocations,
		llmCalls:         llmCalls,
		registry:         registry,
		mcpSupervisor:    mcpSupervisor,
		networkAllowlist: networkAllowlist,
		scheduler:        scheduler,
		scheduleSlot:     scheduleSlot,
		scheduleEditor:   scheduleEditor,
		agents:           agents,
		agentGate:        agentGate,
		lifecycle:        lifecycle,
		resourceGate:     resourceGate,
		versions:         versions,
		ops:              ops,
		credentials:      credentials,
		connectionTests:  connectionTests,
		notifyAdminStore: notifyAdminStore,
		resolver:         resolver,
		secrets:          secrets,
		mcpRT:            mcpRT,
		skills:           skills,
		agentAdmin:       agentAdmin,
		projection:       projection,
		resourceAdmin:    resourceAdmin,
		notifyAdmin:      notifyAdmin,
		channelRead:      channelRead,
		catalogStore:     catalogStore,
		// 目录快照在重路径开头解析一次（见 `mcpcat.Builtin` 处）：保留端口实例让
		// `webOptions` 逐字段投影，管理台读到的就是这份快照，请求期零解析。
		// The parsed catalog snapshot travels as a port so webOptions can project it.
		mcpCatalog: &mcpCatalogPort{catalog: mcpCatalogSnapshot},
		// 写面的 web 端口在这里**一次装配**：Agent 端口 8 个方法已齐，作者配置取命令面
		// 已装载的那一份（命令层装载一次、装配层冻结）；资源端口 13 个方法全部转调本
		// 实例——MCP 与技能方法族先后由 T048 / T053 接上，本 US 不再有失败关闭桩；
		// 通知端口 3 个方法自 T058 起转调本实例。资源端口的只读投影与 `Options.Resources`
		// 同一构造、同一批存储（写后回读不做乐观更新）。
		// The write web ports are assembled once here; the resource port's thirteen
		// methods all forward to this instance now that T048 / T053 landed.
		writeServices: writeServices{
			agentAdmin: agentAdminPort{admin: agentAdmin, author: cfg.Author},
			resourceAdmin: resourceAdminPort{
				admin: resourceAdmin,
				read:  NewResourceViews(versions, lifecycle, mcpRT, credentials, ResourceViewDeps{ConnectionTests: connectionTests}),
			},
			notifyAdmin:   notifyAdminPort{admin: notifyAdmin, read: channelRead},
			workspace:     workspaceBrowsePort{browse: workspaceBrowse},
			resourceState: resourceStatePort{state: resourceState},
			agentConfig: NewAgentConfigReader(AgentConfigDeps{
				Root:        root,
				Resources:   versions,
				Identities:  identities,
				Notify:      notifyAdminStore,
				Credentials: credentials,
				Lifecycle:   lifecycle,
			}),
		},
		// US-9 追加的只读装配态：全部是本次装配里**已经算出来的值**的保留，不新增读取。
		// US-9's read-only assembly state: values already computed here, merely retained.
		cfg:             cfg,
		entries:         entries,
		mem:             memSvc,
		mcpConns:        mcpConns,
		root:            root,
		loader:          loader,
		workspaceBrowse: workspaceBrowse,
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

// 下面三个函数是作者草稿的**引用白名单快照**（US-12 约束 29 / FR20）：装配层按每次调用
// 的当前状态回答，AgentAdmin 不缓存结果。三者与运行准入读同一批表，但不构造客户端、不
// 解析环境变量——草稿只需要名字。
// The three helpers below answer the author draft's reference whitelists from the current
// snapshot; they never build clients or resolve credentials.

// authorPublishedProviders 列出已发布模型连接条目，顺序取 `ListPointers` 的固定排序
// （模板"取第一项"的判据因此稳定）。条目解析复用运行目录的同一个 `publishedProviderEntry`
// ——归属、形态与名字一致性校验只有一份。
// authorPublishedProviders lists published provider entries in the store's fixed order,
// reusing the very resolver the runtime catalog uses.
func authorPublishedProviders(ctx context.Context, versions *storage.ResourceStore) ([]workspace.ProviderEntry, error) {
	pointers, err := versions.ListPointers(ctx, core.ResourceModelConnection)
	if err != nil {
		return nil, fmt.Errorf("列出已发布模型连接: %w", err)
	}
	entries := make([]workspace.ProviderEntry, 0, len(pointers))
	seen := make(map[string]struct{}, len(pointers))
	for _, pointer := range pointers {
		entry, _, err := publishedProviderEntry(ctx, versions, pointer)
		if err != nil {
			return nil, err
		}
		// 已发布条目带上发布指针的稳定身份：模板创建据此写 `resource_bindings`，
		// 运行装配与提示词渲染只读名字，多出来的这一项不影响它们。
		// The published list carries the stable uid of the pointer for template bindings.
		entry.ResourceUID = pointer.ResourceUID
		if _, dup := seen[entry.Name]; dup {
			return nil, fmt.Errorf("已发布模型连接的条目名 %s 重复——按名定位必须唯一", entry.Name)
		}
		seen[entry.Name] = struct{}{}
		entries = append(entries, entry)
	}
	return entries, nil
}

// authorPublishedSkills 返回当前未撤回的技能发布指针名（升序）：提示词按传入顺序渲染，
// 顺序稳定才可判卷；判定复用注册装配的同一函数（`publishedSkillNames` 自己复核撤回位）。
// authorPublishedSkills returns the live skill names in ascending order.
func authorPublishedSkills(ctx context.Context, versions *storage.ResourceStore) ([]string, error) {
	published, err := publishedSkillNames(ctx, versions)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(published))
	for name := range published {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// authorAppliedMCPServers 返回**已应用且已连接**的 MCP server（FR20）：应用版本非空
// （存在 `applied_version_id`，可能是待重启前的旧版本——运行准入用的正是它）且出现在本次
// 装配实际连上的名单里。两个条件任一不成立都不进提示词，草稿里的引用随后由同一份名单判卷。
// authorAppliedMCPServers lists MCP servers that have an applied version and are connected
// in this process — exactly the set admission can resolve.
func authorAppliedMCPServers(ctx context.Context, versions *storage.ResourceStore, mcpRT *mcpRuntime) ([]string, error) {
	pointers, err := versions.ListPointers(ctx, core.ResourceMCPServer)
	if err != nil {
		return nil, fmt.Errorf("列出已发布 MCP server: %w", err)
	}
	connected := make(map[string]struct{}, len(mcpRT.ConnectedNames()))
	for _, name := range mcpRT.ConnectedNames() {
		connected[name] = struct{}{}
	}
	names := make([]string, 0, len(pointers))
	for _, pointer := range pointers {
		if pointer.DeletedAt != nil || pointer.AppliedVersionID == nil {
			continue
		}
		if _, ok := connected[pointer.ResourceID]; !ok {
			continue
		}
		names = append(names, pointer.ResourceID)
	}
	return names, nil
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

// providerNameUnion 返回"配置文件名字 ∪ 未删除发布指针名字"的并集（去重、升序），只服务
// 启动基线之前的第一遍装载：那一刻尚未跑基线（首启时一个 Provider 都没发布），而后续启动
// 又可能存在"文件已删、指针保留"的条目。它**不是**运行目录的构建来源——Provider 目录与
// 注册结论都在对账之后按已发布版本重算（约束 18）。
// providerNameUnion merges the configured and published provider names for the
// pre-baseline load only; it never builds the runtime catalog.
func providerNameUnion(ctx context.Context, versions *storage.ResourceStore, cfg *cli.Config) ([]string, error) {
	pointers, err := versions.ListPointers(ctx, core.ResourceModelConnection)
	if err != nil {
		return nil, fmt.Errorf("列出已发布模型连接: %w", err)
	}
	seen := make(map[string]struct{}, len(cfg.Providers)+len(pointers))
	names := make([]string, 0, len(cfg.Providers)+len(pointers))
	for _, entry := range cfg.Providers {
		if _, dup := seen[entry.Name]; dup {
			continue
		}
		seen[entry.Name] = struct{}{}
		names = append(names, entry.Name)
	}
	for _, pointer := range pointers {
		if _, dup := seen[pointer.ResourceID]; dup {
			continue
		}
		seen[pointer.ResourceID] = struct{}{}
		names = append(names, pointer.ResourceID)
	}
	// 排序固定：同一份输入两次启动必须给装载器同一个序（判卷与日志可复现）。
	slices.Sort(names)
	return names, nil
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

// registeredAgentNames 取出第一遍装载里**八条校验全过**的 Agent 名（排序固定）：凭据启动
// 校验只查这些 Agent 的绑定——未注册的 Agent 进不了运行面，替它们报"凭据不可用"只会把
// 一条本来就该有的注册错误盖掉。第一遍的 Provider / MCP 名集是并集与乐观占位，只会**多**
// 认出已注册者，不会漏——用它做上界是安全的一侧。
// registeredAgentNames lists the agents that passed every load check, sorted; the startup
// credential verification only covers these, and the first pass can only over-count them.
func registeredAgentNames(entries []core.AgentEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Registered {
			names = append(names, entry.Name)
		}
	}
	slices.Sort(names)
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

	// mcp 是**无 supervisor 时**的 MCP 连接池接缝（生产装配恒 nil；停机顺序用例用它观察
	// "连接收起先于库关闭"）。US-12 起生产连接归 mcpSupervisor，两者互斥。
	// mcp is the pool seam used by the shutdown-order fixture when no supervisor exists;
	// production sets mcpSupervisor instead.
	mcp mcpConnector

	// mcpSupervisor 持有全部 MCP server 连接（零配置时也已构造但无条目）。停机时必须先于
	// 库关闭：它收起子进程与会话，之后才是写池的最后一批使用者（约束 22：scheduler → MCP
	// → 库 → 锁）。
	// The supervisor owns every MCP connection and must stop before the database closes.
	mcpSupervisor *MCPSupervisor

	// networkAllowlist 是出网域名白名单的管理服务（`GET/PUT /sandbox` 与 MCP 撤销的
	// 共享实例）：它与运行期策略是同一个 `*tool.WhitelistPolicy`，所以"保存即生效"
	// 不需要第二次装配。
	// networkAllowlist is the managed allowlist shared with the live policy instance.
	networkAllowlist *NetworkAllowlist

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
	versions    *storage.ResourceStore
	ops         *storage.ManagementStore
	credentials *storage.CredentialStore

	// connectionTests 是资源「最后一次连接测试」投影：测试端点写入、管理读面读取，
	// 保留句柄让装配面可核"写的与读的是同一张表"（历史仍在管理操作台账）。
	// connectionTests is the last-test projection shared by the writers and readers.
	connectionTests *storage.ConnectionTestStore

	// notifyAdminStore 是通知渠道仓储：保存期绑定校验与通知写面读的是同一实例，
	// 保留句柄让装配面可核"写面与校验看到的是同一份渠道真值"。
	// notifyAdminStore is the one channel store shared by the write surface and the
	// save-time binding check.
	notifyAdminStore *storage.NotifyAdminStore

	// resolver / secrets 是凭据解析器与 secrets 子命令的装配产物：命令面经
	// `Runtime.Secrets()` 取 `cli.SecretsService` 端口，解析器本身不再外露。
	// The resolver and the secrets service are retained here; only the latter crosses
	// the package seam.
	resolver *credentialResolver
	secrets  SecretsService

	// mcpRT 是已发布 MCP 定义的只读运行期句柄（配置与连接两份名单）；skills 是技能库
	// 句柄——准入解析、基线发布与发布版本快照共用同一实例。
	// mcpRT and skills are the runtime handles shared by admission and the baseline.
	mcpRT  *mcpRuntime
	skills *workspace.SkillLibrary

	// agentAdmin 是 Agent 写面的编排实现（US-12）：构造一次、全进程共享。它持有与准入
	// 同一把 Agent 级锁表、同一份生命周期存储与资源仓储；web 端口适配器
	// （`writeServices.agentAdmin`）投影的正是这一个实例。保留句柄让装配面可核"只有这一个"。
	// agentAdmin is the one write-surface orchestrator; the web port projects this instance.
	agentAdmin *AgentAdmin

	// projection 是绑定投影的重建面（与启动全量重建、Agent 写面共用同一实例）：
	// 内容编辑路径（US-11 的 `core.AgentAdmin` 适配器）写成功之后也按文件重建一次。
	// projection is the one projector shared by startup, the write surface and content edits.
	projection *BindingProjection

	// resourceAdmin 是公共资源写面的编排实现（US-12）：构造一次、全进程共享，持有与
	// 准入同一道发布闸门、同一份版本 / 操作仓储与同一个运行期目录持有者；Agent 引用取
	// 同一份注册表快照，归档扫描以工作区根为界。它的 web 端口
	// （`writeServices.resourceAdmin`）投影的正是这一个实例。
	// resourceAdmin is the one resource write orchestrator; the web port projects this
	// very instance.
	resourceAdmin *ResourceAdmin

	// notifyAdmin 是通知渠道写面的编排实现（US-12）：构造一次、全进程共享，类型表与
	// 发送面 / 类型端点同源，操作台账与资源写面是同一实例。它的 web 端口
	// （`writeServices.notifyAdmin`）投影的正是这一个实例。
	// notifyAdmin is the one channel write orchestrator; the web port projects it.
	notifyAdmin *NotifyAdmin

	// channelRead 是通知渠道的**管理读面**（notify_read.go）：列表端点与写后回显共用
	// 这一个实例，发送路径的仓储与它是同一个零缓存读面。保留句柄让装配面可核
	// "页面读到的就是下一发会用的"。
	// channelRead is the admin read face shared by the list endpoint and the post-write echo.
	channelRead *notifyChannelReadPort

	// workspaceBrowse 是工作区只读浏览的唯一实现（US-12）：构造一次、全进程共享，
	// web 端口（`writeServices.workspace`）投影的正是这一个实例。浏览没有写路径，
	// 也不与写面共享锁或存储。
	// workspaceBrowse is the one read-only workspace browser; the web port projects it.
	workspaceBrowse *WorkspaceBrowse

	// mcpCatalog 是只读 MCP 目录端口（US-14 D-01）：包着装配期解析好的那份快照，
	// 请求期零解析、零文件读、零进程。nil 表示本次装配没有目录（轻路径），此时
	// `webOptions` **不**注入——目录端点按固定 500 装配缺失失败关闭。
	// mcpCatalog wraps the snapshot parsed at assembly; nil means "not assembled", and
	// webOptions leaves the port unset so the endpoint fails closed with a fixed 500.
	mcpCatalog *mcpCatalogPort

	// catalogStore 是运行期目录的原子持有者：资源发布在此替换，T032 之后准入与模型
	// 调用从这里读取——保留句柄让装配面可核"写面和读面确实是同一个持有者"。
	// catalogStore is the one atomic catalog holder shared by the write surface and,
	// from T032 on, by admission and routing.
	catalogStore *providerCatalogStore

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

	// writeServices 是 US-12 写面适配器的装配槽（类型定义在 web.go）：尚未装配的端口保持
	// nil，`webOptions` 逐字段投影到 `web.Options`，handler 对缺端口固定 500 失败关闭
	// ——**不**以半套实现冒充"可用"。保留在运行期句柄上是为了让装配面可核「写服务只构造
	// 一次，路由处理函数与恢复路径看到的是同一实例」——**不**在 webOptions 里临时构造。
	// writeServices is the US-12 write-adapter slot projected field-by-field in webOptions;
	// an unassembled port stays nil and its handlers fail closed with a fixed 500.
	writeServices writeServices
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
	if r.mcpSupervisor != nil {
		errs = append(errs, r.mcpSupervisor.Close())
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

// runCancellerAdapter 把运行账本与运行入口适配成 Agent 写面的在途 Run 端口（约束 32 ③）：
// 归档编排只认三个语义——快照在途 Run、尽力送达取消信号、在有界窗口内等它们退出。适配器
// 不复制任何循环逻辑：取消之后由谁把 Run 写成终态仍归 `ReActLoop`（宪法 2），`Cancel` 本身
// 从不写终态。
// runCancellerAdapter adapts the run ledger and the run service to the archive port. It
// signals only; the loop alone writes terminal states.
type runCancellerAdapter struct {
	runs    *storage.RunStore
	service *core.AgentService
	// poll 是等待窗口内的轮询间隔；≤0 取 runCancellerPoll（测试注入小值只为缩短用例）。
	// poll is the polling interval inside the bounded wait window.
	poll time.Duration
}

const (
	// runCancellerPoll 是等待窗口内的轮询间隔：100ms 足以让几十秒级的归档窗口及时看到退出，
	// 又不会把一次 30 秒窗口变成上千次查询。
	// runCancellerPoll is the polling interval inside the bounded wait window.
	runCancellerPoll = 100 * time.Millisecond

	// runCancellerListLimit 是在途快照的行数上限，与 `ListRuns` 的固定窗口上限同值（200）：
	// 归档不为在途判定另开一条全表扫描。
	// runCancellerListLimit mirrors the run list window cap (200).
	runCancellerListLimit = 200
)

// 编译期断言：端口形状一旦漂移在这里先红（与 web.go 的端口断言同一纪律）。
// Compile-time assertion: the port shape is fixed here.
var _ RunCanceller = runCancellerAdapter{}

// RunningRunIDs 从运行账本取该 Agent 仍处于 running 的行（一次快照，不等待）。过滤与排序
// 全部走 `ListRuns` 的常量 SQL，请求侧只给值谓词。
// RunningRunIDs snapshots the agent's running rows through the shared list query.
func (a runCancellerAdapter) RunningRunIDs(ctx context.Context, agent string) ([]string, error) {
	details, err := a.runs.ListRuns(ctx, core.RunFilter{
		Agent:  agent,
		Status: string(core.StatusRunning),
		Limit:  runCancellerListLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("列出 Agent %s 的在途 Run: %w", agent, err)
	}
	ids := make([]string, 0, len(details))
	for _, detail := range details {
		ids = append(ids, detail.RunID)
	}
	return ids, nil
}

// CancelRun 只负责把取消信号送达运行循环：`AgentService.Cancel` 在信号之后至多等**一个
// 轮询间隔**（而不是它自己的 30 秒上限）——整段归档等待由 `WaitRuns` 的窗口统一计时，
// 逐个 Run 各等 30 秒会把有界窗口撑成 N×30 秒。行已不存在（已退出并清理）视为已达成。
// CancelRun delivers the signal and returns; the bounded wait belongs to WaitRuns.
func (a runCancellerAdapter) CancelRun(ctx context.Context, runID string) error {
	signalCtx, cancel := context.WithTimeout(ctx, a.interval())
	defer cancel()
	if _, err := a.service.Cancel(signalCtx, runID); err != nil && !errors.Is(err, core.ErrRunNotFound) {
		return err
	}
	return nil
}

// WaitRuns 在 ctx 窗口内轮询运行账本，返回窗口结束时仍未退出的标识。**到期不是错误**：
// 剩余集合就是等待结果，调用方据此判 409 并回滚（约束 32 ③）；只有查询失败才上抛。
// WaitRuns polls within the window; an expired window is a result, not an error.
func (a runCancellerAdapter) WaitRuns(ctx context.Context, runIDs []string) ([]string, error) {
	pending := append([]string(nil), runIDs...)
	if len(pending) == 0 {
		return nil, nil
	}
	ticker := time.NewTicker(a.interval())
	defer ticker.Stop()
	for {
		remaining, err := a.aliveRuns(ctx, pending)
		if err != nil {
			return nil, err
		}
		if len(remaining) == 0 {
			return nil, nil
		}
		pending = remaining
		select {
		case <-ctx.Done():
			return pending, nil
		case <-ticker.C:
		}
	}
}

// aliveRuns 逐个读当前状态：终态或行已不存在算已退出，running 算在途。窗口到期与查询判定
// 竞争时**期限断言优先**（按未退出计），其余查询失败如实上抛。
// aliveRuns classifies each run as exited or alive; an expired window wins over a read.
func (a runCancellerAdapter) aliveRuns(ctx context.Context, runIDs []string) ([]string, error) {
	alive := make([]string, 0, len(runIDs))
	for _, runID := range runIDs {
		if ctx.Err() != nil {
			alive = append(alive, runID)
			continue
		}
		detail, err := a.runs.GetRun(ctx, runID)
		switch {
		case err == nil:
			if detail.Status == core.StatusRunning {
				alive = append(alive, runID)
			}
		case errors.Is(err, core.ErrRunNotFound):
			// 行已不存在：账本清理与退出同源，按已退出处理。
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// 窗口到期打断本次读取：按未退出计，由调用方判 409。
			alive = append(alive, runID)
		default:
			return nil, fmt.Errorf("读取 Run %s 的状态: %w", runID, err)
		}
	}
	return alive, nil
}

// interval 给未注入轮询间隔的装配一个缺省值。
// interval falls back to the default polling interval.
func (a runCancellerAdapter) interval() time.Duration {
	if a.poll <= 0 {
		return runCancellerPoll
	}
	return a.poll
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

// Secrets 返回 secrets 子命令的装配产物（init / status / verify / revoke-managed）。
// Secrets returns the assembly's secrets service for the command surface.
func (r *Runtime) Secrets() SecretsService { return r.inner.secrets }

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
