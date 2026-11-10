package app

// 本文件是**唯一装配层**的重路径接线（技术方案 §6.1.2 轻 / 重两条路；颗粒度文档 §2.3 流程 D /
// 口径 25）：重命令（本 US 是 `chat`——`serve` 归后续 US）在开跑之前，按**固定次序**把长生命
// 周期资源接起来——取实例锁 → 写模式打开 → 显式迁移 → 崩溃恢复（账本六步 + 工作区残留第 ⑤ 步）
// → 构建记忆门面（载体恒为 PostgreSQL，D-18）→ 建工具注册池 → 注册池名字集喂 Agent
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

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/observability"
	"github.com/ymedword/basalt/internal/storage/postgres"
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

	// supportedVersion 是目标 schema 版本——生产传 postgres.SupportedSchemaVersion，测试传
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

	// freshInstance 表示本次启动前库与实例名册都不存在（T089 的唯一口径）。它只由
	// Run 在 `cli.LoadConfig` **之前**判定一次并带下来：名册一旦写盘，下一次启动就不再
	// 是新实例——主密钥的自动初始化因此**不会**为存量实例补做。
	//
	// 访问密码是例外的一半：首次初始化中断后（名册已写、密码未发放），重试靠实例私有
	// 目录里的进度记录继续把密码做完（instance_provisioning.go）。它只续做**同一件**
	// 未完成的事，不改写任何既有文件，也不补做已 minted 的密码。
	// The admin password is the one thing a retry may still complete, and only while the
	// provisioning record says pending.
	freshInstance bool

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

	// builtinRuntime 是内置组件运行时根目录的**进程内接缝**（生产恒空串 = 按程序目录现算）。
	// 用例注入临时目录即可走完整个开箱安装路径，不必先造一份真实的发行布局。
	// builtinRuntime overrides the runtime root; empty means the program directory's.
	builtinRuntime string
}

// wsRoot 返回工作区根的绝对路径。
// wsRoot returns the absolute workspace root.
func (a *App) wsRoot() string { return filepath.Join(a.workDir, workspaceDirName) }

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

// builtinRuntimeRootOr 返回本次装配用的运行时根：注入值优先，否则按程序目录现算。
// builtinRuntimeRootOr prefers the injected runtime root and falls back to the program dir.
func (a *App) builtinRuntimeRootOr() string {
	if a.builtinRuntime != "" {
		return a.builtinRuntime
	}
	return programRuntimeRoot()
}

// newRuntime 按固定次序装配一次重路径运行期；返回的句柄归调用方所有，**必须** Close（释放
// 次序与取得次序相反）。任何一步失败都不留半开的资源：已到手的锁与库句柄在这一层释放并
// 并入错误链。
//
// `role` 是本进程的启动角色（US-25C §2.3.1 / D-25C-06）：`serve` 走 control，`gateway`
// 走 Worker；共享初始化写**逐项**经 `role.allowSharedInit(...)` 判定，Worker 只做只读
// 装配与合法的执行协调写（FR-25C-12 / FR-25C-13）。Worker 打不开需要迁移的库——
// `postgres.OpenForWorker` 只校验版本，不建库、不迁移。
// newRuntime assembles the heavy-path runtime in the fixed order. The returned handle owns
// the instance lock and the database and must be closed by the caller; every failure path
// below releases whatever it already holds. The role decides, item by item, which
// shared-initialization writes run; workers also open the database in verify-only mode.
func (a *App) newRuntime(ctx context.Context, role runtimeRole) (*heavyRuntime, error) {
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
	observationConfig := cfg.Observability.Effective()
	if err := cli.ValidateObservabilityConfig(observationConfig); err != nil {
		return nil, err
	}
	if err := observationConfig.ValidateHistoryWindow(cfg.Diagnostics.RetentionDays); err != nil {
		return nil, err
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

	// ② 打开 PostgreSQL：连接、扩展能力校验与建库 / 前向迁移都在 `postgres.Open` 内完成
	// （advisory 锁保护）。失败即拒绝启动——**不回退** SQLite（D-01）；DSN 不进日志。
	// Worker 走 `postgres.OpenForWorker`：只校验 schema 版本，不建库、不迁移——库结构是
	// 控制面的共享初始化写，Worker 一项不做（FR-25C-12 / §2.3.1）。
	// The PostgreSQL handle is opened here; capability checks and the forward
	// migration happen inside postgres.Open, and a failure aborts startup instead of
	// falling back to SQLite. The DSN never reaches the log. Workers open in
	// verify-only mode: creating or migrating the schema is a control-plane write.
	openDatabase := postgres.Open
	if role != roleControl {
		// Worker 走版本校验路径（`OpenForWorker` 只接受精确的受支持版本）。
		// Workers verify the schema version and never migrate it.
		openDatabase = postgres.OpenForWorker
	}
	db, err := openDatabase(ctx, runtimePostgresConfig(cfg))
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("打开 PostgreSQL: %w", err), nil, lock)
	}
	logger.Info("重路径：PostgreSQL 已打开", "step", stepOpen,
		"schema_version", postgres.SupportedSchemaVersion)

	// 存储实例在打开之后一次构造：运行期所有消费面共享**同一批**实例，读到的行因此
	// 来自同一个连接池。构造只持句柄、不读表。
	// The stores are constructed once after the open so every consumer shares these
	// very instances. Constructing them only holds handles and reads nothing.
	versions := postgres.NewResourceStore(db)
	credentials := postgres.NewCredentialStore(db)
	// identities 是资源身份墓碑表：结构化配置读面按绑定 UID 现场查名称与删除事实。
	// identities is the uid tombstone table read by the config view.
	identities := postgres.NewIdentityStore(db)
	notifyAdminStore := postgres.NewNotifyAdminStore(db)
	bindingStore := postgres.NewBindingStore(db)
	archives := postgres.NewAgentAdminStore(db)
	ops := postgres.NewManagementStore(db)
	lifecycle := workspace.NewLifecycleStore(root, a.nowOr())
	// 生命周期读面的索引优先装配（US-25C FR-25C-14 / §2.6 判据表第 3 行）：准入端口与
	// 调度门共用同一份读取实现——本地状态文件在各机是旧副本，共享判定以 `agent_lifecycle`
	// 索引为准；索引无行才回退状态文件，索引行不可用或读失败一律失败关闭。写仍只走
	// `lifecycle`（状态文件），索引由管理面在落盘成功后就地回写。
	// The index-first reader is shared by admission and the schedule gate; writes keep
	// going through the state-file store, and the management path owns the index write.
	lifecyclePort := newLifecycleIndexPort(lifecycle, versions)
	channels := postgres.NewNotifyChannelRepo(db)
	// connectionTests 是「最后一次连接测试」投影（R7 范围修订、schema v4）：模型连接与
	// 通知渠道的测试写同一张表，管理读面与两个测试端点共用这一实例——页面读到的就是
	// 上一次真实测试留下的行；历史明细仍在管理操作台账。
	// The last-test projection shared by the two test endpoints and the admin read faces.
	connectionTests := postgres.NewConnectionTestStore(db)
	// networkPolicy 是出网域名白名单的唯一权威行（schema v5）：启动种子、运行期条件写、
	// 启动恢复查证共用同一实例（R8 裁决 2）。
	// networkPolicy is the single authoritative allowlist row shared by seeding, the
	// conditional write and startup recovery.
	networkPolicy := postgres.NewNetworkPolicyStore(db)
	// networkExceptionStore 是网络访问例外的唯一读写面（schema v6）：启动装载、管理写
	// 与恢复查证读的是同一批行。
	// One instance backs the startup load, the admin writes and recovery.
	networkExceptionStore := postgres.NewNetworkExceptionStore(db)
	// executionPolicyStore 是执行档位单行表（schema v6）的同一实例：启动装载 / 种子、
	// 运行期条件写、启动恢复查证读的都是它——恢复判"生效 / 未生效"读的必须是写路径
	// 写进去的那一行。
	// One instance backs seeding, the conditional write and the startup verification.
	executionPolicyStore := postgres.NewExecutionPolicyStore(db)

	// ③ 迁移已在 `postgres.Open` 内完成（advisory 锁 + `schema_version` 前向推进）。
	// 运行期**不再**调用 SQLite 谱系的 v3 六步编排——那条路径只保留给旧 SQLite 实例的
	// 离线升级 / 导出（`basalt db` 子命令），不从 serve 触碰。
	// The forward migration already ran inside postgres.Open; the SQLite-lineage v3
	// orchestration is kept only for the offline upgrade/export path.
	logger.Info("重路径：schema 版本已校验", "step", stepMigrate,
		"schema_version", postgres.SupportedSchemaVersion)

	// 网络访问例外在迁移之后、工具装配之前装载进进程级快照（14D §2.2.6）：守门器在
	// `Run` 里已经装好，这里喂给它"哪些受保护地址经显式授权放行"。读失败保留空快照
	// （受保护地址一律拒），不阻断启动——启动失败比少放行代价大，且失败是安全方向。
	// The exception snapshot is fed to the guard before any tool client is built.
	refreshNetworkExceptions(ctx, networkExceptionStore, logger)

	// 凭据五件（解析器 / 守卫 / Run 检查点 / 写面 / secrets 服务）在迁移之后、工具注册
	// 之前一次装配：通知工具、运行目录构建、准入固定与 CLI 的 secrets 子命令消费的都是
	// 这一组实例。解析器只拿主密钥**路径**，密钥本身每次解析时重新读取（不常驻内存）。
	// The five credential components are wired once after migration and before the tools;
	// the resolver holds only the key path — the key is re-read on every resolution.
	// 主密钥路径按"显式配置 → 实例名册推导的缺省路径 → 不配置"三级解析（T089 / §2.4.3）：
	// 没有名册的存量实例不猜旧路径，`auto_init: false` 连缺省路径也不推导。
	// The key path is explicit, or derived from the roster, or absent — a roster-less
	// instance never gets a guessed path.
	masterKeyFile, err := resolveMasterKeyFile(cfg, a.workDir)
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("解析主密钥路径: %w", err), db, lock)
	}
	// 路径用**一个共享持有者**贯穿五个凭据组件：用户在工作区管理台显式初始化存量
	// 实例后，模型连接 / MCP 凭据 / 通知渠道在同一进程内看到新路径，不必重启。
	// One shared holder threads the path through all five credential components.
	masterKeyPath := NewMasterKeyLocator(masterKeyFile)
	instanceSecretsStore := postgres.NewInstanceSecretsStore(db)
	instanceSecrets, err := NewInstanceSecretsService(InstanceSecretsDeps{
		Store:         instanceSecretsStore,
		Operations:    ops,
		MasterKeyFile: masterKeyFile,
		KeyPath:       masterKeyPath,
		WorkDir:       a.workDir,
		WorkspaceRoot: root,
		AutoInit:      cfg.Secrets.AutoInitEnabled(),
		FreshInstance: a.freshInstance,
		Logger:        logger,
		Now:           a.nowOr(),
	})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	// 加密三态判定与首次初始化在**同一把实例锁内**先走一遍（T085 / T087）：可证明的新实例
	// 在这里生成主密钥；恢复态只记录、不生成替代密钥，也**不**阻断启动（管理面显示
	// 「恢复中」，托管凭据按失败关闭）。
	// Worker 跳过这一步：生成 / 轮换主密钥是共享初始化写（`sharedInitSecretsEnsure`，
	// FR-25C-12）。Worker 只使用控制面已经写下的那份密钥；读不到时托管凭据按失败关闭。
	// The three-state verdict and the first-run mint happen inside the instance lock;
	// the recovery state is reported but never blocks startup. Workers skip the mint:
	// the master key is a control-plane shared write.
	if role.allowSharedInit(sharedInitSecretsEnsure) {
		secretsStatus, err := instanceSecrets.Ensure(ctx)
		if err != nil {
			return nil, abortRuntime(fmt.Errorf("实例加密初始化: %w", err), db, lock)
		}
		logger.Info("实例加密：三态判定完成", "step", "secrets",
			"state", secretsStatus.Decision.State, "reason", secretsStatus.Decision.Reason,
			"action", secretsStatus.Decision.Action, "key_id", secretsStatus.KeyID)
		if secretsStatus.Decision.State == core.SecretStateRecoveryRequired {
			logger.Warn("实例加密：主密钥处于恢复态——不生成替代密钥、不回落 env，托管凭据将失败关闭",
				"reason", secretsStatus.Decision.Reason)
		}
	} else {
		logger.Info("Worker 跳过主密钥初始化（共享初始化写归控制面）",
			"item", sharedInitItemName(sharedInitSecretsEnsure))
	}

	resolver, err := newCredentialResolver(CredentialResolverDeps{
		Credentials:   credentials,
		Resources:     versions,
		Notify:        notifyAdminStore,
		Bindings:      bindingStore,
		MasterKeyFile: masterKeyFile,
		KeyPath:       masterKeyPath,
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
		Pins:  postgres.NewRunCredentialStore(db),
		Guard: guard,
	})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	// MCP 逐槽位凭据的来源与状态（14D / 设计 §2.3.2）：写面、解析面与视图投影共用
	// 同一个实例，主密钥路径与启动校验因此只有一处来源。
	// The per-slot MCP credential surface is built once and shared by the admin, the
	// resolver and the console projection.
	mcpCredentials, err := NewMCPCredentialService(MCPCredentialServiceDeps{
		Credentials:   credentials,
		Sources:       postgres.NewMCPCredentialSourceStore(db),
		MasterKeyFile: masterKeyFile,
		KeyPath:       masterKeyPath,
		WorkspaceRoot: root,
		Logger:        logger,
	})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	credentialOps, err := NewCredentialOps(CredentialOpsDeps{
		Operations:    ops,
		Credentials:   credentials,
		Resources:     versions,
		Notify:        notifyAdminStore,
		Instance:      instanceSecrets,
		MasterKeyFile: masterKeyFile,
		KeyPath:       masterKeyPath,
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
		Instance:      instanceSecrets,
		MasterKeyFile: masterKeyFile,
		KeyPath:       masterKeyPath,
		WorkspaceRoot: root,
		Resources:     versions,
		Logger:        logger,
	})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}

	// ④ 启动只做**工作区侧**残留判定（目录交换残留）。账本侧**不再**在这里全局扫描：
	// 多 Worker 下启动时可能有别的活实例在写库，按"跑得久 / 心跳旧"推断死亡会改写别人的
	// 在跑行（FR-24-02 / C-06）。账本恢复改为只作用于**合法领取的过期租约**，由
	// `startWorkerRuntime` 的首轮 `Reclaimer.ReclaimOnce` 完成（图 1）。
	// Startup only settles workspace residue here. Ledger recovery is no longer a global
	// scan: with multiple workers it would rewrite live rows. Recovery is scoped to
	// legitimately claimed expired leases and runs as the first ReclaimOnce of the worker.
	residue, err := workspace.RecoverWorkspaceResidue(root, logger)
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("工作区残留判定: %w", err), db, lock)
	}
	logger.Info("重路径：工作区残留判定完成", "step", stepRecover,
		"rolled_back", residue.RolledBack, "rolled_forward", residue.RolledForward)

	// 记忆门面（US-6，位置在 ④ 与 ⑤ 之间——颗粒度文档 §3.1）：载体恒为 PostgreSQL
	// （D-18），门面**一个实例喂两个消费面**——工具侧（写路径）与运行入口的段 2
	// 注入（读路径）拿到的是同一个对象，作用域回退、per-scope 写锁与注入期 WARN 因此
	// 只有一份实现（口径 25）。
	// The memory facade is built once over PostgreSQL and handed to both consumers, so the
	// fallback, the per-scope lock and the injection-time warning stay single-sourced.
	memBackend, err := memoryCarrier(db, a.nowOr())
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	memSvc, err := memory.NewService(memory.Options{Backend: memBackend, Logger: logger})
	if err != nil {
		return nil, abortRuntime(fmt.Errorf("构建记忆门面: %w", err), db, lock)
	}
	// 记忆管理面（US-16 裁决 A / C-21）与门面**共用同一个载体实例**：管理面读到的
	// 行就是 `save_memory` 写进去的那行，不存在两份数据。载体不满足窄端口时启动即失败
	// ——管理面是核心能力，缺它不能用"空列表"顶替。
	// The management surface shares the very carrier the facade writes through; a
	// carrier without the narrow port aborts startup instead of degrading to an empty list.
	memStore, ok := memBackend.(memory.MemoryStore)
	if !ok {
		return nil, abortRuntime(errors.New(
			"记忆管理面：载体未实现窄持久化端口——拒绝启动，不降级为只读门面"), db, lock)
	}
	// 索引槽位先于管理面构造：确认晋升成功后的 best-effort 补索引（US-22 C-28）要与
	// `save_memory` 共用同一个槽位。槽位是两段绑定，晚绑定（下方知识库装配处 Bind）
	// 不影响此处先建后传。
	// The index slot is built before the admin surface so a confirmed promotion can
	// re-index through the very slot save_memory uses; binding still happens later.
	memoryIndexSlot := newMemoryIndexSlot(logger)
	memAdmin := NewMemoryAdmin(MemoryAdminDeps{
		Store:      memStore,
		Operations: ops,
		Index:      memoryIndexSlot,
		Logger:     logger,
		Now:        a.nowOr(),
	})

	// ⑤ 授权判定与工具注册池：策略先建（工具持有的是**接口**，具体实现在装配处一处选定）；
	// 池是**全局**的——同一实例的注册池服务所有 Agent，声明的名字与之比对（宪法 IV）。
	// 程序目录三项条件（14D §2.2.2）的清单**产者**是安装器（US-17 拟）：本增量只落
	// 判定面与装配槽位，**不**自行发明清单文件格式。清单为空时自动放行面恒不成立——
	// 程序目录里的可执行件一律要显式登记（`shell.allowed_commands`），fail-closed
	// 而不是"先放了再说"。
	// The installation manifest is produced by the installer in a later increment; an
	// empty manifest means no auto-admission, so runtime executables need an explicit
	// registration. This assembly intentionally invents no manifest file format.
	policy := tool.NewWhitelistPolicy(tool.PolicyOptions{
		AllowRunCode:    cfg.Execution.AllowRunCode,
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
		// ProgramDir / ProgramManifest 与上面的策略清单同源，同样等安装器交付；
		// 留空即"没有自动放行面"（三项条件恒不成立）。
		// ProgramDir and ProgramManifest come from the same installer-produced source;
		// left empty, the three-condition admission never fires.
	})
	shellTool := tool.NewShellTool(policy, runner)
	runCodeTool := tool.NewRunCodeTool(tool.RunCodeOptions{Policy: policy})
	var isolationRT *isolationRuntime
	if cfg.Isolation.Backend != "" {
		marker, present, err := readInstanceMarkerAt(root)
		if err != nil || !present {
			return nil, abortRuntime(errors.New("隔离执行需要有效实例名册"), db, lock)
		}
		isolationRT, err = buildIsolation(ctx, isolationDeps{Config: cfg.Isolation, ShellConfig: cfg.Shell, Policy: policy, Root: root, InstanceID: marker.InstanceID, Store: db, Reader: db, Logger: logger})
		if err != nil {
			return nil, abortRuntime(fmt.Errorf("装配隔离执行: %w", err), db, lock)
		}
		if isolationRT.Shell != nil {
			shellTool = isolationRT.Shell
		}
		runCodeTool = isolationRT.RunCode
	}
	// 十二件工具：三件文件工具 + US-5 的 shell / http_get / http_post + 两件记忆工具 +
	// US-8 的 notify + US-10 的 manage_schedule + 14D 的 web_search（技能调用工具永久禁；
	// US-7 的 MCP 各件在等值门禁之后注册——颗粒度文档 §3.5 后续占位 1）。`manage_schedule`
	// 的编辑后端与 `web_search` 的凭据解析面都在 runtime / 注册表之后才存在，这里先建
	// **两段绑定**的槽位，装配完成后由下方 `Bind` 接入生产适配器（颗粒度文档 §3.1.8 图 E）。
	// write_file 的构造期 Agent 目录留空：每次调用的 `ToolEnv.AgentDir` 才是写入根，
	// "判定的那个 Agent"与"写入的那棵树"同源由运行环境保证。
	// Eleven tools: the file trio, the shell/HTTP trio, the memory pair, notify,
	// manage_schedule, and web_search — the last two bound in two stages.
	scheduleSlot := tool.NewScheduleEditorSlot()
	// web_search 的搜索键取该 Agent 绑定的模型连接（14D §2.5.4）：解析面需要 Agent
	// 注册表按名查 provider，而注册表在工具注册之后才构建——同样走两段绑定，调用前未
	// 绑定即失败关闭（不猜默认连接）。
	// The search key resolves through the agent's bound model connection; the registry
	// arrives after tool registration, so the slot is bound below.
	webSearchSlot := newWebSearchCredentialSlot(versions, resolver)
	// recall_memory 的语义路同样两段绑定（P-07）：embedding 客户端与语义载体都在知识库
	// 装配之后才存在，槽位先建、`knowledgeWire` 就绪后 Bind；未绑定即失败关闭。
	// The semantic recall face is two-stage bound for the same reason as web_search.
	memoryRecallSlot := newMemoryRecallSlot()
	// 记忆向量的写后补索引同样两段绑定（V-5）：槽位早在记忆管理面构造前已建（US-22 的
	// 确认晋升也向它补索引），此处沿用同一个槽位；知识库装配完成后 Bind 并做一次启动期
	// 回填。未绑定（embedding 未配置）时补索引是 no-op，保存路径行为不变。
	// The write-side index slot was created before the admin surface; the knowledge
	// wiring binds it later and unbound means no-op.
	for _, t := range []core.Tool{
		tool.NewReadFileTool(policy, root),
		tool.NewWriteFileTool(policy, root, ""),
		tool.NewListDirTool(policy, root),
		shellTool,
		runCodeTool,
		tool.NewHTTPGetTool(policy),
		tool.NewHTTPPostTool(policy),
		// save_memory 的工具侧拿到**带写后补索引**的门面（V-5）：Append 返回时 per-scope
		// 写锁已释放，嵌入调用不在临界区内；保存失败不触发嵌入。
		// The save tool sees the indexed facade; embedding runs after the write lock is out.
		tool.NewSaveMemoryTool(&indexingCarrier{LongTermMemoryStore: memSvc, slot: memoryIndexSlot}),
		tool.NewRecallMemoryTool(memSvc, memoryRecallSlot),
		// 托管凭据解析面在此接线：通知按**逐次发送**解析当前版本（不像模型 Run 那样在
		// 准入固定），解析失败即失败关闭，不回退明文；env 占位渠道不经此路。
		// Managed notify credentials resolve per send; a failure is fail-closed.
		newNotifyTool(channels, policy, resolver,
			notifyDeliveryRecorder{store: postgres.NewNotifyDeliveryStore(db), logger: logger}),
		tool.NewManageScheduleTool(scheduleSlot),
		tool.NewWebSearchTool(policy, webSearchSlot),
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

	// 开箱即用安装（14D T111–T113）：新实例首次启动时把五个内置 Agent、样例库与免凭据的
	// MCP 条目就位；凭据就绪时补写需要凭据的条目。**必须排在下面第一次解析文件之前**——
	// 写下的条目正是由紧随其后的启动基线发布，本次启动即生效（不需要用户重启）。
	// 存量实例与安装标记损坏时本段直接返回（见 builtin_install.go 的纪律）。
	// 装 Agent / 样例库是共享初始化写（`sharedInitBuiltinOpenBox`）：Worker 不装，
	// 但走同一条首次装载——控制面装好的定义经资产包分发到 Worker。
	// The open-box installer runs before the first parse so the baseline publishes what it
	// wrote in this very start. Workers skip the install; the control plane's bundles
	// carry those definitions to them.
	if role.allowSharedInit(sharedInitBuiltinOpenBox) {
		openBoxReport, err := prepareBuiltinOpenBox(ctx, builtinOpenBoxDeps{
			WorkspaceRoot: root,
			RuntimeRoot:   a.builtinRuntimeRootOr(),
			Fresh:         a.freshInstance,
			WorkflowKnown: func(name string) (bool, error) {
				record, valid, reason, readErr := versions.ReadLifecycleIndex(ctx, name)
				return valid || record.Generation != 0 || reason != "", readErr
			},
			LookupEnv: os.LookupEnv,
			Now:       a.nowOr(),
			Logger:    logger,
		})
		if err != nil {
			return nil, abortRuntime(fmt.Errorf("准备内置 Agent 与 MCP 条目: %w", err), db, lock)
		}
		if openBoxReport.Skipped != "" {
			logger.Info("内置安装器跳过", "reason", openBoxReport.Skipped)
		}
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
	// networkDenylist 是同一张表的第二行（14D），与白名单服务共用条件写实现；
	// networkExceptions 是访问例外的管理服务，三处消费面由它的 apply 一次替换。
	// The denylist shares the conditional-write implementation; the exception service
	// swaps all three faces of the exception set at once.
	var networkDenylist *NetworkAllowlist
	var networkExceptions *NetworkExceptionService
	// executionPolicy 同样保留在本作用域：档位要在 supervisor 连接**之前**装载，否则
	// MCP http 连接会按构造函数的缺省档位判定，而不是库里的权威值。
	// The profile must be loaded before the supervisor connects for the same reason.
	var executionPolicy *ExecutionPolicyService
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
	// 资产根（US-24 §2.3.9）：`assets.bundle_root` 的相对路径按**进程工作目录**解析，
	// 与工作区根同一基准——缺省值因此落在 `<workDir>/.basalt/assets`，安装目录是
	// `<assetRoot>/bundles/<agent>/<hash>`。空值不在这里兜底：配置校验已拒绝空串
	// （`validateDistributedConfig`），这里的缺省只覆盖"没有配置文件"的零值装配。
	// The asset root resolves against the process working directory, the same base as
	// the workspace; an empty value only appears in a zero-value test assembly.
	bundleRoot := resolveAssetBundleRoot(cfg, a.workDir)
	// 启动基线发布共享记录（`sharedInitBaseline`）：Worker 不发布，只读控制面已经发布的
	// 版本；它的运行集合由资产同步按 bundle 重建（§2.3.4）。跳过基线不改变装载语义——
	// 上面那一遍装载只读工作区种子。
	// The baseline publishes shared records; workers never do — their run sets come from
	// the asset sync. Skipping it leaves the read-only seed load untouched.
	if role.allowSharedInit(sharedInitBaseline) {
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
	} else {
		logger.Info("Worker 跳过启动基线（共享初始化写归控制面）",
			"item", sharedInitItemName(sharedInitBaseline), "agents", len(entries))
	}

	// 资产自举（US-24 §2.3.9 启动自举）：只在"共享发布记录在、本地 hash 目录缺"且本地
	// 代次与共享索引**相等**时按记录重建；不发布、不改索引、不覆盖既有目录，单个 Agent
	// 失败只 WARN 不阻塞启动。这里**不带 `step` 键**：启动步骤清单是已定判据，自举不是
	// 新步骤，也不是一次独立的恢复扫描。
	// The asset bootstrap never publishes, never touches the index and never blocks
	// startup; it deliberately carries no "step" key. It is a control-plane shared
	// write (`sharedInitAssetBootstrap`): workers receive bundles through the sync.
	if role.allowSharedInit(sharedInitAssetBootstrap) {
		bootstrapAgentBundles(ctx, assetBootstrapDeps{
			Root:      root,
			AssetRoot: bundleRoot,
			Versions:  versions,
			Lifecycle: lifecycle,
			Agents:    entries,
			Logger:    logger,
		})
	}

	// 存量载荷回填（US-25C §2.3.3）：为 US-25 及以前发布的指针补载荷，幂等、可重复。
	// 它**只**在控制面启动链里执行（US-25C §2.3.1 树图 / `sharedInitPayloadBackfill`）；
	// Worker 不补写载荷——缺口由 `assets status` 如实标注，Worker 以 ASSET_UNAVAILABLE
	// 失败关闭并留下可见原因，重导种子不在恢复手段之列。失败不阻塞启动。
	// The backfill stays control-only per §2.3.1; workers report the gap through asset
	// status instead of writing payloads.
	if role.allowSharedInit(sharedInitPayloadBackfill) {
		if _, err := BackfillBundlePayloads(ctx, BackfillDeps{
			Distribution:  postgres.NewAssetDistributionStore(db),
			PayloadStore:  postgres.NewBundlePayloadStore(db),
			Versions:      versions,
			Lifecycle:     lifecycle,
			WorkspaceRoot: root,
			AssetRoot:     bundleRoot,
			MaxBytes:      resolveMaxBundleBytes(cfg),
			Logger:        logger,
		}); err != nil {
			logger.Warn("bundle payload backfill did not finish; missing payloads stay visible in asset status",
				"error", err)
		}
	}

	// 技能启动兜底（US-25C §2.3.7 / `sharedInitSkillRepair`）：控制面重启后不等下一轮
	// 就先把 `failed` / `missing` / `superseded` 的联动重发全量补齐。Worker 不发起重发
	// （FR-25C-17）——它的运行集合由同步轮从已发布内容重建，修复写归控制面。
	// The control-startup drain repairs stale skill linkage once per boot; workers never
	// originate republishes.
	if role.allowSharedInit(sharedInitSkillRepair) {
		if _, err := repairStaleBundleSkills(ctx, RepairDeps{
			Distribution:  postgres.NewAssetDistributionStore(db),
			AssetRoot:     bundleRoot,
			WorkspaceRoot: root,
			Versions:      versions,
			Operations:    ops,
			Payloads:      postgres.NewAssetDistributionStore(db),
			PayloadStore:  postgres.NewBundlePayloadStore(db),
			AgentGate:     agentGate,
			MaxBytes:      resolveMaxBundleBytes(cfg),
			Logger:        logger,
			Now:           a.nowOr(),
			Batch:         skillRepairDefaultBatch,
			Interval:      cfg.Assets.SyncInterval,
		}); err != nil {
			logger.Warn("skill repair startup drain did not finish; stale skills stay visible for the next round",
				"error", err)
		}
	}

	// 管理面恢复（图 A ④ / 约束 38）：基线自己若留下 pending 行，它已在上一段收口；
	// 其余按动作组查证，判定不了的标 unknown 并**保留原样**——重放一次停用会让 generation
	// 跳两格，重放一次内容写会覆盖第三方的修改。查证本身会推进操作行状态，属共享初始化写
	// （`sharedInitOperationRecovery`）：Worker 不做，控制面下一次启动会收口。
	// Management recovery verifies each pending row and never replays a change; the
	// verdict writes the operation row, so it is a control-plane shared write.
	if role.allowSharedInit(sharedInitOperationRecovery) {
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
			// 访问例外的三态查证读同一张 `network_exceptions` 表（契约 §9.4）。
			NetworkExceptions: networkExceptionStore,
			// 执行档位的三态查证读同一行表（14D）：档位与修订号都要对上才算 applied。
			ExecutionPolicy: executionPolicyStore,
			// 实例初始化记录与主密钥路径：`system.secrets_init` 的恢复查证按记录 + key_id
			// + 完整读回判定 applied / not_applied / indeterminate（契约 §9.4）。
			InstanceSecrets: instanceSecretsStore,
			MasterKeyFile:   masterKeyFile,
			// 记忆条目的只读查证面：与记忆门面 / 管理面共用同一个载体，`memory.*` 的
			// pending 行按条目行状态与正文摘要判定三态（契约 §9.4）。
			// The memory verdicts read the very carrier the write path uses.
			Memory: memStore,
			// 准入操作的只读查证面（US-21 / 颗粒度文档 §2.3.7）：准入写面走独立 CLI 端口，
			// 崩溃留下的 pending 行由下一次服务启动查证。这里只装配查证需要的工作区根与
			// 准入仓储——`LockResource` / `References` 归写会话，恢复只读行与候选目录。
			// The admission verifier needs only the workspace root and the admission store;
			// the write-side lock table stays with the CLI session.
			Admission: NewAdmissionAdmin(AdmissionAdminDeps{
				Root:   root,
				Store:  postgres.NewAdmissionStore(db),
				Logger: logger,
				Now:    a.nowOr(),
			}),
		}); err != nil {
			return fail(err)
		}
	}
	logger.Info("重路径：管理面恢复完成", "step", stepRecovery)

	// 定义对账（约束 18 / 约束 34 / 图 A ⑤）：读取定义文件与已发布版本，按四向表处理差异
	// ——一致跳过；文件新且候选合法则建新版本并发布；文件无效应保留已发布版本并 WARN；
	// 条目被删则忽略并保留。对账**必须**在运行目录与注册表之前：运行对象只由已发布版本
	// 构建，手工编辑因此不可能绕过发布闸门直接生效。发布写库失败 → 启动失败，不带病运行。
	// Startup reconciliation runs before the catalog and the registry, so a hand edit can
	// never bypass the publish gate; a failed publish aborts startup. Publishing is a
	// control-plane shared write (`sharedInitDefinitionReconcile`): the worker skips it
	// and reads published versions only.
	var reconcileResult ReconcileResult
	if role.allowSharedInit(sharedInitDefinitionReconcile) {
		reconcileResult, err = ReconcileDefinitions(ctx, ReconcileDeps{
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
	}
	// 全量重建绑定投影（约束 38 / 图 A ⑤）：投影是 `AGENT.md` 的可重建派生视图，对账把
	// 文件与已发布版本对齐之后必须从**文件**重算，不能沿用上一进程的残行。它与对账同属
	// 一个启动阶段（五阶段判据不变）：扫描失败按工作区级问题失败关闭，单 Agent 失败记
	// WARN、留给准入闸门按"可能引用"处理。
	// The full projection rebuild belongs to the same startup stage as reconciliation; a
	// scan failure aborts startup while per-agent failures stay fail-closed at the gate.
	// 知识集合目录只建一次（不带 I/O 的存储句柄），既供投影按名解析集合、又供注册闸门
	// 与热加载判"名字有没有实体"、再供知识库装配复用——四处同一个实例，判定口径只有
	// 一份。放在投影之前是因为投影重建是它的第一个消费者。
	// The catalog store is created once, before the projection that resolves names through
	// it, and reused by the registration gate, the watcher and the knowledge wiring.
	knowledgeCatalogStore := postgres.NewKnowledgeCatalogStore(db)
	projection := NewBindingProjection(bindingStore, versions, archives, knowledgeCatalogStore, root, logger, a.nowOr())
	// 绑定投影重建会整表替换派生行（`sharedInitStartupBindings`）：Worker 跳过，
	// 控制面在下一次启动重建；Worker 的准入判定不依赖这份派生视图。
	// The projection rebuild rewrites derived rows; workers skip it.
	var projectionReport ProjectionReport
	if role.allowSharedInit(sharedInitStartupBindings) {
		projectionReport, err = rebuildStartupBindings(ctx, projection, logger)
		if err != nil {
			return fail(err)
		}
		logger.Info("重路径：启动定义对账完成", "step", stepReconcile,
			"published", reconcileResult.Published, "retained", reconcileResult.Retained,
			"unchanged", reconcileResult.Unchanged, "ignored", reconcileResult.Ignored,
			"bindings_rebuilt", projectionReport.Rebuilt,
			"bindings_cleared", projectionReport.Cleared,
			"bindings_failed", len(projectionReport.Failures))
	} else {
		logger.Info("Worker 跳过定义对账与绑定投影重建（共享初始化写归控制面）",
			"item", sharedInitItemName(sharedInitDefinitionReconcile))
	}

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
	// MCP 凭据按**声明来源**解析（14D / 设计 §2.3.3）：装配期先把文件播种 / 升级而来的
	// 槽位事实补记成 `env`，再逐槽位取值——`managed` 取不到即失败，**不**回落进程环境。
	// Slots are resolved by declared source; file-seeded ones get their env fact recorded
	// first, and a managed slot that cannot be read fails closed.
	// 来源补记（`BackfillEnvSources`）是共享初始化写（`sharedInitMCPCredentialSource`）：
	// 只有 control 走 `publishedMCPServers`；Worker 走只读解析——applied 指针存在时读
	// 那一版并按声明来源只读解析凭据（M4），未 applied 的只列名字（D-12）。
	// The source backfill is a control-plane shared write; workers resolve published
	// servers read-only and never touch the source table.
	var publishedMCP []core.PublishedMCPServer
	var workerMCPConfigs []core.MCPServerConfig
	if role.allowSharedInit(sharedInitMCPCredentialSource) {
		publishedMCP, err = publishedMCPServers(ctx, versions, fileServers, mcpCredentials, a.nowOr(), logger)
		if err != nil {
			return fail(fmt.Errorf("解析已发布 MCP server: %w", err))
		}
	} else {
		// 启动期只取配置名单；准备输入（targets）由同步轮按**当轮**快照重新解析，
		// 不在此处留存（§2.3.8 M5：名单可增长，准备要跟发布走）。
		// Startup keeps only the configured names; each sync round resolves its own
		// targets afresh.
		_, workerMCPConfigs, err = publishedMCPPrepareTargets(ctx, versions, fileServers, mcpCredentials, logger)
		if err != nil {
			return fail(fmt.Errorf("只读解析已发布 MCP server: %w", err))
		}
	}
	mcpDeps := MCPSupervisorDeps{
		Policy: policy,
		Gate:   resourceGate,
		Logger: logger,
		Now:    a.nowOr(),
		// 同步准备路径的换入前只读复核（§2.3.8 M2 / 提交复核分支表）：指针仍存在、
		// applied 仍是目标版本、目标版本的原文定义仍解出同一凭据代次且未撤销。缺这条
		// 接线时 `replace` 在 Commit / Verify 都为空时失败关闭——Worker 的 MCP 永远
		// 准备失败，依赖 MCP 的 Agent 无法就绪。
		// The sync path's pre-swap recheck: pointer alive, applied still the target, and
		// the target's credential generation still current; without it the prepare path
		// fails closed and workers never connect MCP.
		PrepareVerify: newMCPPrepareVerify(versions, mcpCredentials),
		// applied 的落库只在发布指针仍指向该版本时发生：期间若有更新的发布，写入旧版本
		// 会把指针倒退——直接跳过，`pending_restart` 自然为真。
		MarkApplied: func(markCtx context.Context, name string, versionID int64) error {
			return markMCPApplied(markCtx, versions, name, versionID, a.nowOr())
		},
		// 工具适配器随连接一起进入注册池与视图；注册序 = 配置顺序 → 工具列表序，重名
		// keep-first（内置优先，约束 8），其余工具照常注册。登记一律**带归属**：受控重建
		// 要靠归属把整组工具换掉（`ReplaceTools`），不带归属就会在换组时留下孤儿名字。
		OnConnected: func(conns []tool.MCPConnection) {
			mcpRegisterTools(registry, dispositions, conns, logger)
			mcpConns = append(mcpConns, conns...)
			logger.Debug("MCP 工具进入注册池", "tools", mcpToolNames(conns))
		},
		// 受控重建的提交段（在 supervisor 的 gate 写锁 + entry.mu 内执行）：注册池与来源
		// 表同时换组。旧 Run 的快照不被改写——它钉的是工具实例，连接由旧代排空态负责
		// 明确失败（C4）；这里只保证"下一次准入看到的是新的一整组"。
		// ReplaceTools swaps one server's whole tool set inside the commit section; a live
		// run's pinned instances are untouched and settle on the draining link.
		ReplaceTools: func(server string, tools []core.Tool) {
			mcpReplaceTools(registry, dispositions, server, tools)
			logger.Info("MCP 工具集已整组替换", "server", server, "tools", len(tools))
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
	// 策略种子写（`sharedInitPolicySeed`）只归控制面：Worker 按只读重读装载，缺行即
	// 失败关闭并点名先由控制面初始化——凭空播种会让两个角色对"权威行"各写一次。
	// Seeding the policy rows is a control-plane shared write; workers re-read and fail
	// closed on a missing row.
	if role.allowSharedInit(sharedInitPolicySeed) {
		if err := networkAllowlist.EnsureSeeded(ctx, cfg.HTTP.AllowedDomains); err != nil {
			return fail(fmt.Errorf("装载网络域名白名单: %w", err))
		}
	} else if _, _, err := networkAllowlist.RefreshFromStore(ctx); err != nil {
		return fail(fmt.Errorf("装载网络域名白名单（只读）: %w", err))
	}
	// 禁止域名表是同一张 `network_domain_policy` 的第二行（14D §2.2.5）：同一个策略
	// 实例、同一套 ETag / CAS，只在审计动作名与生效字段上分开。配置键 `http.denied_domains`
	// 同样只作首次种子；装载顺序同样先于 supervisor 连接——第一批判定就要按禁止表拒。
	// The denylist is the second row of the same table, loaded before any judgement.
	networkDenylist = NewNetworkAllowlist(NetworkAllowlistDeps{
		Scope:      core.PolicyScopeHTTPDeniedDomains,
		Store:      networkPolicy,
		Operations: ops,
		Policy:     policy,
		Supervisor: mcpSupervisor,
		Logger:     logger,
		Now:        a.nowOr(),
	})
	if role.allowSharedInit(sharedInitPolicySeed) {
		if err := networkDenylist.EnsureSeeded(ctx, cfg.HTTP.DeniedDomains); err != nil {
			return fail(fmt.Errorf("装载网络禁止域名表: %w", err))
		}
	} else if _, _, err := networkDenylist.RefreshFromStore(ctx); err != nil {
		return fail(fmt.Errorf("装载网络禁止域名表（只读）: %w", err))
	}
	// 访问例外与运行期策略、守门器快照同源（14D §2.2.6）：服务在这里把库里已装载的
	// 集合复制到策略实例与视图快照上（守门器快照在迁移之后已由 `refreshNetworkExceptions`
	// 装过一份）。读失败不阻断启动：保留守门器已有快照并只记 WARN（失败是安全方向）。
	// The exception service mirrors the authoritative set onto the policy instance and
	// the view; a failed read keeps the guard snapshot and only warns.
	networkExceptions = NewNetworkExceptionService(NetworkExceptionDeps{
		Store:      networkExceptionStore,
		Operations: ops,
		Policy:     policy,
		Logger:     logger,
		Now:        a.nowOr(),
	})
	if err := networkExceptions.EnsureLoaded(ctx); err != nil {
		logger.Warn("装载网络访问例外失败——保留守门器已有快照（受保护地址仍按原集合判定）",
			"error", err)
	}
	// 执行档位与**同一个**策略实例绑定（14D）：装配顺序上排在 supervisor 连接之前，
	// 让第一批判定（含 MCP http 连接）就按库里的权威档位；`execution.profile` 只在
	// 表内无行时作首次种子，落库之后修改配置不再覆盖管理台的写入。
	// The profile binds to the very policy instance and loads before the supervisor
	// connects; the config key only seeds a row-less database.
	executionPolicy = NewExecutionPolicyService(ExecutionPolicyDeps{
		Store:         executionPolicyStore,
		Operations:    ops,
		Policy:        policy,
		ShellCommands: cfg.Shell.AllowedCommands,
		Logger:        logger,
		Now:           a.nowOr(),
	})
	if role.allowSharedInit(sharedInitPolicySeed) {
		if err := executionPolicy.EnsureSeeded(ctx, cfg.Execution.Profile); err != nil {
			return fail(fmt.Errorf("装载执行档位: %w", err))
		}
	} else if _, _, err := executionPolicy.RefreshFromStore(ctx); err != nil {
		return fail(fmt.Errorf("装载执行档位（只读）: %w", err))
	}
	// applied 指针写（`sharedInitMCPApplied`）只在 control：Worker 的连接由同步轮的
	// `prepareMCPInstances` 按 applied 指针准备（`Commit: nil`，不写指针）。
	// The applied write is control-only; workers connect through the sync round.
	if role.allowSharedInit(sharedInitMCPApplied) {
		if err := mcpSupervisor.ApplyPublished(ctx, publishedMCP); err != nil {
			return fail(fmt.Errorf("启动 MCP supervisor: %w", err))
		}
	}
	mcpConfigs := mcpPublishedConfigs(publishedMCP)
	if role != roleControl {
		// Worker 的配置名单来自同一次只读解析；运行期由同步轮 `ReplaceSpecs` 增长。
		// The worker's configured list comes from the same read-only resolution.
		mcpConfigs = workerMCPConfigs
	}
	mcpRT := newMCPRuntimeFromSupervisor(mcpSupervisor, mcpConfigs)
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
	// 注册装配的知识集合过滤（D-19 / 契约 agent-md §2.1 规则 13）：声明的集合名必须
	// 解析到**唯一活动集合**才注册。与技能分支同一纪律——读不到目录即拒绝启动，热加载
	// 用同一个函数（`watcher.go`）。
	// The knowledge-collection filter completes D-19 registration after the skill gate and
	// before the registry; a failed lookup aborts startup exactly like the watcher.
	entries, err = registryEntriesWithKnowledgeCollections(ctx, knowledgeCatalogStore, entries, logger)
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
	// web_search 的两段绑定在此收口：注册表就位后才能按 Agent 名查 provider（14D §2.5.4）。
	// The second binding stage lands once the registry exists.
	webSearchSlot.Bind(agents)
	sessions := postgres.NewSessionStore(db)
	invocations := postgres.NewToolInvocationRepo(db)
	llmCalls := postgres.NewLLMCallRepo(db)
	// Worker 身份在装配期生成一次（US-24 T007）：运行入口按它写租约归属，`serve` /
	// `gateway` 的登记与心跳复用同一个实例。装配期生成而不是等登记时再生成，是因为
	// 运行入口先于登记拿到服务对象——两处各生成一次会让租约指向一个不在
	// `worker_nodes` 里的标识。熵源失败按启动失败处理（没有身份就不参与协调）。
	// The identity is minted once here so admission-time lease ownership and the
	// serve-time registration always name the same process.
	workerIdentity, idErr := mintWorkerIdentity(cfg)
	if idErr != nil {
		return nil, abortRuntime(idErr, db, lock)
	}
	workerCfg := workerSettings(cfg)
	// 运行账本只建一次：AgentService 写、web.RunService 适配器读，同一实例保证
	// "读到的"就是"写下的"（两条连接池之间的影子账本会让排障无从对齐）。
	// 租约 TTL 与续约循环取**同一份** `worker.lease_ttl_seconds`：仓储按它判过期、
	// keeper 按它续约，两边不同源会让本进程在自己仍执行时被判过期。
	// One run store is shared by the writer (AgentService) and the reader (the web port);
	// its lease TTL is the same value the keeper renews against.
	runs := postgres.NewRunStoreWithLeaseTTL(db, workerCfg.LeaseTTL())
	// 执行租约仓储只建一次（US-24）：运行入口写 run 租约、调度器写 schedule_execution
	// 租约，两者共用同一个实例（类型无状态，共用保证的是"装配面只有一处构造"与同一份
	// TTL / 续约节奏，见下面两处接线）。
	// One lease store serves both run leases and schedule_execution leases.
	leases := postgres.NewLeaseStore(db)
	// 受控自我进化（US-22 §3.1.8）：**同一个**仓储实例同时喂抽取服务（写路径）与
	// 只读状态面（读路径）——列表读到的行就是钩子刚提交的那行，不存在两套账。
	// 未启用时钩子在首行返回：零写入、零模型调用；管理面仍可读出「已关闭」的状态。
	// 名单内未注册的 Agent 只 WARN + 状态页列出（Q7），不抽取、不阻止启动。
	// One repository feeds both the extraction service and the read-only status face;
	// disabled deployments do nothing on the hook while the console still sees "off".
	learningRepo := postgres.NewLearningRepo(db)
	// 启动恢复（§2.3.6 / C-15）：扫描上次崩溃留下的 pending 抽取行，只查证、不重放。
	// 查证结论（有候选 / 草案 → success，否则 unknown）在同一事务推进来源游标并写
	// `verified_at`，此后该行不再阻断同来源新窗口；恢复失败拒绝启动——带着结果未知的
	// 抽取行开始服务，同来源的下一窗口要么永久阻断，要么被重复抽取。
	// Startup recovery verifies pending extractions before serving; it never replays,
	// and a storage failure aborts startup.
	// 角色门：学习回填会推进来源游标与 `verified_at`，属共享初始化写，Worker 一项不做
	// （FR-25C-12）；Worker 侧未查证的行保持 pending，由控制面下次启动处理。
	// The learning recovery advances source cursors and `verified_at`; it stays on the
	// control plane per FR-25C-12.
	if role.allowSharedInit(sharedInitLearningRecovery) {
		if err := recoverLearning(ctx, LearningRecoveryDeps{
			Store:  learningRepo,
			Logger: logger,
			Now:    a.nowOr(),
		}); err != nil {
			return fail(fmt.Errorf("启动恢复：学习抽取查证: %w", err))
		}
	} else {
		logger.Info("Worker 跳过学习抽取回填（共享初始化写归控制面）",
			"item", sharedInitItemName(sharedInitLearningRecovery))
	}
	learningDraftRoot := filepath.Join(root, "learning", "drafts")
	learningSvc := memory.NewLearningService(memory.LearningDeps{
		Store:     learningRepo,
		Messages:  sessions,
		Enabled:   cfg.Learning.AutoExtract,
		Agents:    cfg.Learning.Agents,
		DraftRoot: learningDraftRoot,
		Limits:    memory.DefaultLearningLimits(),
		Logger:    logger,
		Now:       a.nowOr(),
	})
	unknownLearning := reportUnknownLearningAgents(logger, cfg.Learning.AutoExtract,
		cfg.Learning.Agents, registeredAgentNames(entries))
	learningAdmin := NewLearningAdmin(LearningAdminDeps{
		Store:         learningRepo,
		DraftRoot:     learningDraftRoot,
		Enabled:       cfg.Learning.AutoExtract,
		Agents:        cfg.Learning.Agents,
		UnknownAgents: unknownLearning,
		Limits:        memory.DefaultLearningLimits(),
		Logger:        logger,
		Now:           a.nowOr(),
	})
	// 模型调用的唯一入口按**当前**目录路由（约束 16 / 17）：发布在 ResourceGate 写锁内
	// 替换持有者后，下一个新 Run 与下一次作者调用立即读新表；在途 Run 拿的是准入时固定的
	// `Pinned` 视图，不受影响。路由仍只查表，查不到即拒绝发出（宪法 III）。
	// 抽取预写面（US-22 §3.1.6）必须在**装配期**装进路由器：运行期 `env.ProviderClient()`
	// 拿到的是 `Pinned` 交出的冻结视图，路由器本身不实现 `MemoryExtractor`；不带仓储的
	// 冻结视图会在预写处 fail-closed，抽取在生产链路永不可达（本缺陷由 T031 E2E 捕获）。
	// 装配只是能力挂载、不产生写入；未启用抽取时钩子在首行返回，仍是零写入零调用。
	// The extraction pre-write surface must ride on the router from assembly time: the run
	// path only ever extracts through the pinned view, and a view without the surface
	// fails closed, leaving extraction unreachable in production. Attaching it is a pure
	// capability mount — a disabled deployment still performs no writes or model calls.
	client := newCatalogProviderRouter(catalogStore, llmCalls, logger).
		WithMemoryExtractStore(llmCalls)
	// 诊断两件（US-16 T095 / T097）：写入器进运行路径，读取器进管理面。配置非法在这里
	// 拒绝启动；两个实例共用同一个存储适配器，读到的就是刚写下的那批事件。
	// The diagnostics pair: the recorder joins the run path, the reader the console.
	// An invalid configuration refuses startup instead of running with a reinterpretation.
	observationStore := postgres.NewObservabilityStore(db).WithClock(a.nowOr())
	var observationMetrics *observability.MetricsCollector
	if *observationConfig.Enabled {
		observationMetrics = observability.NewMetricsCollector(observationStore, a.nowOr())
		db.ObserveAuditWriteFailures(func() { observationMetrics.IncCounter("obs_audit_write_failures_total", nil) })
	}
	diagRecorder, diagReader, err := newDiagnosticsPortsWithMetrics(cfg.Diagnostics, db, workerIdentity, a.nowOr(), logger, observationMetrics)
	if err != nil {
		return fail(err)
	}
	sampleRate := cfg.Diagnostics.SampleRateOrDefault()
	observation, err := NewObservability(ctx, observationConfig, Deps{Store: observationStore, Diagnostics: diagRecorder, Clock: a.nowOr(), Metrics: observationMetrics, SampleRate: &sampleRate, Logger: logger})
	if err != nil {
		return fail(err)
	}
	// 图 6 [3] / [5] 的**唯一**限制器实例（C-35）：运行入口（web 的 RunService）、
	// 调度器与摘要调用共用同一份名额表与配额端口。分头 `callers.NewLimiter` 会让同一
	// 调用方在 web 与后台各拿一份 `max_concurrent` 计数，上限形同虚设。
	// 限额来源见 `concurrency_limits.go`（无行 / 0 → 实例级上限兜底，读失败失败关闭）。
	// One limiter instance serves the run entry, the scheduler and compaction; two
	// instances would hand the same caller two independent slot tables.
	quotaStore := postgres.NewQuotaStore(db)
	concurrencyLimit := newConcurrencyLimitCache(
		quotaStore.MaxConcurrent, cfg.Server.MaxConcurrentRuns, a.nowOr())
	concurrency, err := callers.NewMemoryConcurrency(concurrencyLimit.limit)
	if err != nil {
		return fail(fmt.Errorf("装配调用方并发名额表: %w", err))
	}
	runLimiter := callers.NewLimiter(callers.LimiterDeps{
		Concurrency:         concurrency,
		Quotas:              quotaStore,
		LocalAdminUnlimited: cfg.Callers.LocalAdmin.UnlimitedEnabled(),
	})
	// 运行准入（取名额 / 归还 / panic 收口）只装配这一份，web 入口与调度器共用。
	// The one admission object shared by the web entry and the scheduler.
	// inflight 是 `worker.max_inflight` 的池化名额（US-24 T026）：三条执行路径共用
	// 这一份，配置校验保证 dispatch_batch + flows.max_active 不超过它。
	// One pooled budget for the three execution paths; config keeps it large enough.
	// 准入解析器只建一次（US-24）：入口的 [4] 预检与 `core.AgentService` 的 [4] /
	// `StartRun` 事务复查必须读**同一份**限额投影，另建一份会让同一次准入在两个判据点
	// 看到不同的 max_concurrent。`reserved`（交互预留）由下面的 `flowConfig` 补填——
	// 它在本段之后才派生，两处都写在同一个变量上。
	// One resolver serves the entry precheck and the core precheck / in-tx recheck.
	admissionResolver := &callerAdmissionResolver{
		limits:              concurrencyLimit,
		localAdminUnlimited: cfg.Callers.LocalAdmin.UnlimitedEnabled(),
	}
	admission := &runAdmission{
		limiter:   runLimiter,
		inflight:  newInflightLimiter(workerCfg.MaxInflight),
		runs:      runs,
		admission: admissionResolver,
		logger:    logger,
		diag:      diagRecorder,
		// 策略新鲜度门（FR-25C-16）：阈值取 `policy.max_stale`；新鲜度面本身由 run.go 在
		// 建好 `PolicyRefresher` 后接到这个同一对象上——chat / tool 不设刷新器，保持 nil 即
		// 不设门（单进程轻路径不得因没刷新被判过期）。
		// The staleness bound comes from policy.max_stale; the freshness face is attached by
		// run.go once the refresher exists, so chat/tool keep the gate disabled.
		policyMaxStale: policySettings(cfg).MaxStale,
	}
	// 知识库与历史回查接线（US-16 T099 前置）：领域对象必须在压缩接线**之前**落地——
	// 压缩的来源闸门要用知识库的有效性判定，而读面（来源视图 / 引用有效性）用的必须是
	// **同一个** `SourceViewer`，两处判定才不可能分叉。embedding 未配置时装配照常成功，
	// 语义路径按 §3.3 降级为关键词（摄取端回"未配置"而**不是**装配缺失）。
	// 托管凭据来源传 `resolver`（与运行目录同一条解密路径），不是凭据存储。
	// The knowledge and history wiring lands before the compaction gate so the very
	// same SourceViewer serves both the read faces and the injection-time check.
	knowledgeWire, err := newKnowledgeWiring(ctx, knowledgeWiringDeps{
		Events:        diagRecorder,
		Config:        cfg,
		DB:            db,
		WorkspaceRoot: root,
		Resources:     versions,
		Credentials:   resolver,
		// 知识面走**同一个**限制器与诊断写入器，经包级适配器翻译（C-06：knowledge
		// 不 import callers / diagnostics，值类型与哨兵由装配层桥接）。
		// The knowledge faces share the one limiter and recorder through the adapters.
		Quota:            newKnowledgeQuotaAdapter(runLimiter),
		Diagnostics:      newKnowledgeDiagAdapter(diagRecorder, logger),
		QuotaDiagnostics: newKnowledgeIngestQuotaAdapter(diagRecorder, logger),
		// 检索授权按注册表快照读声明、按投影行读稳定 ID（D-19）。
		// Retrieval authz reads declarations off this very registry snapshot.
		Agents:  agents,
		Catalog: knowledgeCatalogStore,
		// 角色门（D-25C-06）：透传子命令判定的真实角色（`serve` = control，`gateway` =
		// worker）；知识代际的 Ensure 属共享初始化写，装配层按 `sharedInitKnowledgeGeneration`
		// 逐项问门（wire_knowledge.go），Worker 只走只读 `Pin`。
		// The subcommand role flows through; the knowledge assembly consults the
		// knowledge-generation gate itself and workers only pin an existing generation.
		Role:   role,
		Logger: logger,
		Now:    a.nowOr(),
	})
	if err != nil {
		return fail(err)
	}
	// recall_memory 语义路绑定（P-07）：embedding 客户端复用知识库栈的那一个；记忆载体
	// 不满足窄持久化端口（markdown 档）或 embedding 未配置时**不绑定**——工具侧按
	// `tool.ErrRecallUnavailable` 失败关闭，给出"改用 mode=keyword"的可执行文案，而不是
	// 静默降级成关键词检索。绑定发生在运行期开跑之前，所以不存在"半绑"的窗口。
	// Bind the semantic recall face; an unsupported carrier or missing embedder leaves it
	// unbound so the tool fails closed with an actionable refusal.
	if recaller := newSemanticRecaller(memBackend, memoryEmbedderOf(knowledgeWire.Embedder)); recaller != nil {
		memoryRecallSlot.Bind(recaller)
	}
	// 记忆向量索引（V-5）：与语义召回共用同一个带审计的 embedding 客户端和同一个固定
	// 代际；写后补索引由上面的工具侧出口触发，这里补一次**启动期回填**——旧记忆导入
	// （`db switch` 只导行）与历史上写失败的缺口都在下一次正常启动时补齐。
	// 回填失败**不阻断启动**：缺口保留（召回遇到空结果会显式报"索引未就绪"），下次
	// 启动重试；日志两档都在（INFO 记读数、WARN 记失败）。
	// The startup backfill closes gaps left by the read-only legacy import or earlier
	// failures; it never blocks startup and is retried next time.
	if indexer := newMemoryIndexer(memBackend, knowledgeWire, logger); indexer != nil {
		memoryIndexSlot.Bind(indexer)
		backfill, err := indexer.Backfill(ctx)
		switch {
		case err != nil:
			logger.WarnContext(ctx, "记忆向量：启动期回填未完成，缺口保留到下次启动",
				"generation", knowledgeWire.Generation, "indexed", backfill.Indexed,
				"batches", backfill.Batches, "error", err)
		case backfill.Indexed > 0:
			logger.InfoContext(ctx, "记忆向量：启动期回填完成",
				"generation", knowledgeWire.Generation, "indexed", backfill.Indexed,
				"batches", backfill.Batches)
		}
	}
	historyWire, err := newHistoryWiring(db, logger)
	if err != nil {
		return fail(err)
	}
	// 会话压缩接线（US-16 T090 / C-25）：来源闸门交给提示词构建器，压缩器按 Run 装配。
	// 未启用时 `Assemble` 返回 nil、闸门为 nil，行为与接线前逐字一致；**启用却缺端口**
	// （含来源校验器尚未接线）在这里拒绝启动，而不是运行期静默降级成"没有过滤 /
	// 没有审计 / 没有配额"的压缩（C-26）。
	// The compaction wiring: the injection-time gate goes to the builder, the per-run
	// compactor to the loop. A half-wired switch refuses startup.
	compactionStore := postgres.NewCompactionStore(db, logger)
	compactionWire, err := newCompactionWiring(compactionWiringDeps{
		Enabled:       cfg.Compaction.Enabled,
		Policy:        compactionPolicyOf(cfg.Compaction),
		OutputReserve: cfg.Compaction.OutputReserveTokens,
		Messages:      compactionStore,
		Store:         compactionStore,
		// 来源校验器接的是知识库的**同一个** `SourceViewer`（经形状适配器）：压缩注入
		// 前逐条判定引用是否仍可用，撤权 / 删除 / 版本变更都按冻结四值拒绝注入。
		// 与知识面读面共用一个实例，判定口径不可能分叉；它**无论压缩开关与否**都要接上
		// （`newCompactionWiring` 的规则），否则开关一开就是半套接线。R104 缺口由此关闭。
		// The validity checker is the very SourceViewer the knowledge read faces use;
		// it is wired regardless of the switch so a switch-on never runs half-wired.
		Validity: knowledgeWire.Validity,
		Audit:    llmCalls,
		// 摘要调用走**同一个**限制器实例：入口限流器（web 面）只装 Buckets，压缩不借它，
		// 但配额端口必须与对话调用同源——另建一个 Limiter 会让同一个 `Quotas` 端口出现
		// 两个实例，审计与排障时说不清"哪一次预留是谁记的"。
		// 幂等键末位是每次尝试的 `attempt_id`（C-29），与对话调用的键空间不重叠。
		// The summary gate shares the one limiter so every reservation has a single owner.
		Quota:       runLimiter,
		Logger:      logger,
		Now:         a.nowOr(),
		Diagnostics: diagRecorder,
	})
	if err != nil {
		return fail(err)
	}
	loop := core.NewReActLoop(
		core.NewPromptBuilder(registry).WithSourceFilter(compactionWire.SourceFilter()),
		// 工具诊断（V-8）：与模型调用装饰器共用同一个写入器——同一 Run 的
		// `llm_call` / `tool_call` / `permission_decision` 因此落在同一条时间线上。
		// 适配器在写入器为 nil 时返回 nil，执行器据此逐字保持未接诊断的行为。
		// The tool diagnostics share the recorder with the model-call decorator.
		core.NewToolExecutor(registry, invocations, logger).
			WithDiagnostics(newToolDiagAdapter(diagRecorder, logger)),
		logger,
	).WithCompaction(compactionWire.OutputReserve(), compactionWire.Assemble).
		// 图 6 [5]：对话调用的配额预留。与入口限流、摘要网关共用**同一个** `runLimiter`
		// （C-35 的单实例裁决）——另建一个 Limiter 会让同一调用方出现两套账。
		// Stage [5] shares the one limiter with the run entry and the summary gate.
		WithCallQuota(&chatQuota{limiter: runLimiter, logger: logger, diag: diagRecorder}).
		// 对话装配记录（V-7）：与检索装配共用知识库装配出来的那一个写入器。
		// 写失败只 WARN（记录可丢弃），不改变 Run 的结局。
		// The chat assembly records share the knowledge wiring's writer; a failed
		// write only warns and never changes the run's outcome.
		WithAssemblyRecorder(knowledgeWire.Assembly)
	flowConfig, err := runtimeFlowConfig(cfg.Flows, cfg.Server.MaxConcurrentRuns)
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}
	admission.flowReservedPerCaller = flowConfig.InteractiveReservedPerCaller
	// 入口预检与 core 预检 / 事务复查共用同一个交互预留（见上面 resolver 的说明）。
	admissionResolver.reserved = flowConfig.InteractiveReservedPerCaller
	// Exactly the same resolver and snapshot builder feed ordinary Run and Flow
	// pin validation; all credential/lifecycle/resource gates remain shared.
	// 调度领取的执行行回读口（US-25C §2.3.5.1 T2）：准入按领取记录复核钉住哈希，
	// 与调度器**共用同一个** TaskStore 实例——两处各建一份会让"领取写了、准入读不到"。
	// The claimed-execution reader shares one TaskStore with the scheduler.
	taskStore := postgres.NewTaskStore(db)
	runResources := NewRunResources(versions, catalog, client, mcpRT, skills, resourceGate, AdmissionDeps{Identities: versions, NotifyAdmin: notifyAdminStore, Channels: channels, Bindings: bindingStore, Credentials: resolver, CredentialPins: resolver, Checkpoint: checkpoint, Diagnostics: diagRecorder, Assets: newAssetVerifier(versions, bundleRoot), Rebuild: newLocalRebuilder(versions, catalogStore, resolver, mcpRT, mcpCredentials)})
	runSnapshots := newRunSnapshotBuilder(workspace.NewContextLoader(logger), root, a.nowOr())
	service := core.NewAgentService(core.ServiceOptions{
		FlowRunsReady:               flowConfig.Enabled,
		FlowWritesReady:             true, // G-FT design prerequisites and independent behavior probes passed.
		FlowInteractiveReservedRuns: flowConfig.InteractiveReservedRuns,
		ToolPins:                    registry,
		Phases:                      newPhaseAdapter(diagRecorder),
		Agents:                      agents,
		// 生命周期准入（图 F ③ / 约束 15）：端口与 Agent 级闸门传的是上面那一份共享实例
		// ——准入在读锁内现场重读，启停迁移持写锁，两者因此互斥。nil 会让准入门禁与启停
		// 互斥同时静默消失，生产装配**不得**省略。US-25C 起读面索引优先（FR-25C-14）：
		// 端口写仍落状态文件，读以数据库索引为准，Worker 不再因本地没有状态文件而失败关闭。
		// Admission shares the gate and store, and reads index-first since US-25C.
		Lifecycle: lifecyclePort,
		Gate:      agentGate,
		// 发布资源的两件必须同进同出（图 F ③ / N1）：解析端口在资源闸门读锁内固定版本，
		// 快照构建器按固定的 SkillDirs 读发布目录；缺一件由 core 在准入前失败关闭。
		// Resources and RunSnapshots are wired together; a partial wiring fails closed.
		Resources:    runResources,
		RunSnapshots: runSnapshots,
		Client:       client,
		Messages:     sessions,
		// Sessions 是准入处的会话门禁端口（US-10 约束 40）：调度运行与手动补跑都经
		// `AgentService.Run`，已归档会话必须在唯一准入点被拒——门禁只能挂在 core，
		// web / app 都不得另写一份判定。nil 会让门禁静默消失，因此这里显式接上。
		// The session gate lives at the single admission point; a nil port would silently
		// disable it, so the store is wired explicitly.
		Sessions:  sessions,
		Runs:      runs,
		Snapshots: postgres.NewSnapshotStore(db),
		Audit:     invocations,
		Memory:    memSvc,
		// 唯一的抽取触发点（US-22 C-05）：钩子只在 `FinishRun` 真正落库之后、引用释放
		// 之前同步运行；关闭时 `AfterRun` 首行返回，与未接线前逐字节相同。
		// The one extraction trigger; disabled means the hook returns on its first line.
		Learning: learningSvc,
		// Run 侧知识链路（V-6）：装配时检索并注入、每轮复核、答案后引用复核。
		// 与 web 的检索端口共用同一个检索器 / 来源视图 / 索引代际。
		// The run-side knowledge port shares the retriever, viewer and generation with
		// the management search surface.
		Knowledge: knowledgeWire.Run,
		Loop:      loop,
		Logger:    logger,
		Now:       a.nowOr(),
		// 并发上限接线（US-9 约束 31）：0 / 未配置由 core 落缺省 32，负数已在装载期拒绝。
		// The cap is wired explicitly; zero falls back to core's single default.
		MaxConcurrent: cfg.Server.MaxConcurrentRuns,
		// 调用方并发判定输入（US-24 T013）：与上面的名额表共用**同一份**限额投影，
		// 预检与 `StartRun` 事务内权威复查因此不会各读一套上限值；内置不限量管理员
		// 与限制器持同一开关（`Enabled=false` 只表示不占桶）。
		// The resolver shares the very limit projection the slot table reads, so the
		// precheck and the in-transaction recheck can never disagree about the limit.
		Admission: admissionResolver,
		// Run 级任务次数（N-01 / 图 6 [5] 的 ClassRun 一支）：与 [3] 并发名额、[5] 对话
		// 记账共用**同一个**限制器实例（C-35）——另建一个会让同一调用方在准入与记账
		// 各拿一份限额视图，超额放行且诊断面对不上账。US-24 P-12 起准入路径的记账已
		// 迁进 `StartRun` 事务（`chargeRunTaskQuotaTx`），这里的端口保留为 Flow 准入的
		// 前置与 `core.RunQuota` 的公开形状；两条记账路径不得同时生效（见 run_quota.go）。
		// The run-level task count shares the one limiter; since P-12 the admission charge
		// commits inside StartRun's transaction and this port stays for Flow readiness.
		RunQuota: newRunQuota(runLimiter, diagRecorder, logger),
		// 执行权（US-24 T007）：每次 Run 的账本事务写租约归属，运行期由 keeper 按
		// `worker.*` 的周期续约；失权后执行上下文被取消，受保护写入再由仓储逐笔拒绝。
		// 时长与 `runs` 的租约 TTL 同源（上面同一个 `workerCfg`）。
		// The lease owner, renewal port and durations: the TTL is the same value the run
		// store uses, so admission and renewal cannot disagree about expiry.
		Worker:        workerIdentity,
		Leases:        leases,
		RenewInterval: workerCfg.RenewInterval(),
		LeaseTTL:      workerCfg.LeaseTTL(),
		// 领取记录回读（US-25C §2.3.5.1 T2）：`RunRequest.Pinned` 非 nil 时，准入按
		// 执行行复核归属与哈希；未接则该类请求直接按无效领取拒绝（失败关闭）。
		// The claimed-execution reader; a pinned request is refused when it is nil.
		Claims: taskStore,
	})

	observation.History = postgres.NewHistoryRetentionStore(db, observationConfig.Export.Enabled)
	observation.SessionGate = service

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
		postgres.NewDeliveryStore(db),
		deliverySender,
		lifecycle,
		agents,
		logger,
	)
	// 管理认证姿态由 web 面在监听成功后发布（T027 / §2.14），调度器先于此构造，故经
	// 这个盒子迟绑定读取；未发布时按"尚未计算"处理，只有 `unavailable` 拒绝后台任务。
	// caller-key 任务的 Key 复核端口（C-32）接 PostgreSQL `caller_keys`（T014）：按
	// 任务行上的 `(caller_id, key_id)` 重读**那一把** Key 的状态、过期与 scope，
	// 任何不成立都带原因码 fail-closed——不得改用同一调用方的其他 Key。
	// The posture box is published later by the web face. The owner-key verifier reads
	// the PostgreSQL key store; any failure keeps the task from running.
	// 调度器同样占名额（图 6 [3]）：它没有 HTTP 入口、因此不经入口速率，但"后台任务
	// 不占调用方并发"会让定时任务成为绕过上限的旁路。包装器只补取名额与归还，其余
	// 语义（归属、生命周期闸门、Key 复核）仍由 core 与调度器负责。
	// The scheduler takes a slot too: without the wrapper, scheduled tasks would bypass
	// the per-caller concurrency limit entirely.
	adminPosture := &postureBox{}
	ownerKeys := core.OwnerKeyVerifier(callers.NewKeyVerifier(postgres.NewKeyStore(db)))
	flowStore := postgres.NewFlowStore(db)
	flowSchemas := tool.NewSchemaCompiler()
	flowAuthority := &flowAuthority{agents: agents, lifecycle: lifecycle, gate: agentGate, resources: runResources, snapshots: runSnapshots, definitions: flowStore, keys: ownerKeys, store: flowStore, tools: registry, schemas: flowSchemas, collections: knowledgeWire.Collections, posture: adminPosture.Get, writeReady: true}
	flowAuthority.newTaskCapacity = func(ctx context.Context, p core.Principal) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		limit, ok := concurrencyLimit.limit(p.CallerID)
		if !ok {
			return core.ErrFlowDispatch
		}
		reserved := 1
		if flowConfig.InteractiveReservedPerCaller != nil {
			reserved = *flowConfig.InteractiveReservedPerCaller
		}
		if limit <= reserved {
			return flow.ErrInteractiveReserve
		}
		return nil
	}
	flowSources := flowSourceValidity{viewer: knowledgeWire.Source, store: flowStore}
	flowRunner := &flowNodeRunner{service: service, admission: admission, store: flowStore, authority: flowAuthority, sources: flowSources, gate: postgres.NewSubtreeGate(db), collections: postgres.NewKnowledgeAuthz(db, agents), schemas: flowSchemas, writeReady: true}
	// Identity / Leases / LeaseTTL 是管理面"零 token 自取根租约"的装配面（US-24 §3.1.6）：
	// resume / revise 的请求里没有租约来源，写 Queued 时由 withRootLease 用这三项自取。
	// The management path self-acquires the flow-root lease through these three fields.
	flowService := &flow.Service{Repository: flowStore, Definitions: flowStore, Schemas: flowSchemas, Authority: flowAuthority, Runner: flowRunner, Config: flowConfig, Now: a.nowOr(), Logger: logger, Outputs: flowOutputChecker{store: flowStore, sources: flowSources}, Verification: &flow.VerificationService{Schemas: flowSchemas, Sources: flowSources, Support: flowSourceValueSupport{viewer: knowledgeWire.Source, sources: flowSources, check: flowAuthority.checkSourceProof}}, Operations: ops, Identity: workerIdentity, Leases: leases, LeaseTTL: workerCfg.LeaseTTL()}
	flowRuntime := &flowRuntime{service: flowService, store: flowStore, runner: flowRunner, logger: logger, workerGrace: workerCfg.ShutdownGrace(), authReady: make(chan struct{}), publisher: &flow.Publisher{Definitions: flowStore, Operations: ops, Validator: &flow.ContractValidator{Schemas: flowSchemas, Resolver: flowAuthority, WriteGate: true}}}
	// 根推进的执行权（US-24 §3.1.7）：Identity 是领取者，Leases 是领取 / 续约 / 交还
	// 的端口，Gate 是跨 Worker 三判据，ReleaseCtx 给出交还时的剩余停机预算。
	// 构造仍不启动；首轮与循环由 startWorkerRuntime 在 serve / gateway 上启动。
	// The root-advancement authority: identity, lease port, durable subtree gate, and
	// the remaining shutdown budget used when a lease is handed back.
	flowRuntime.worker = &flow.Worker{Service: flowService, Repository: flowStore, Identity: workerIdentity, Leases: leases, Gate: postgres.NewSubtreeGate(db), LeaseTTL: workerCfg.LeaseTTL(), RenewInterval: workerCfg.RenewInterval(), ReleaseCtx: flowRuntime.releaseCtx, Logger: logger, Startup: func(ctx context.Context) error {
		select {
		case <-flowRuntime.authReady:
		case <-ctx.Done():
			return ctx.Err()
		}
		flowRunner.mu.Lock()
		defer flowRunner.mu.Unlock()
		if !flowRunner.startupComplete {
			return core.ErrFlowDispatch
		}
		return nil
	}}
	// 停机编排（US-24 T026 / 图 11）：三段预算全部来自配置——`worker.shutdown_grace`
	// 是总上界，`flows.shutdown_grace` 是排空上界（含它自己的 cancel_wait），
	// `flows.cancel_wait` 是取消后 join 的基值。配置校验保证 35+5 ≤ 45 ≤ 70−5。
	// The figure-11 budgets come from config; validation keeps 35+5 ≤ 45 ≤ 70−5.
	shutdownCoord := newShutdownCoordinator(workerCfg.ShutdownGrace(),
		time.Duration(flowConfig.ShutdownGraceSeconds)*time.Second,
		time.Duration(flowConfig.CancelWaitSeconds)*time.Second, logger)
	// 回收器（US-24 T017 / §2.3.5 图 5）：装配在这里是**固定位置**——它同时需要 Flow
	// 服务（A3 里根状态的唯一判定者）与租约端口；两样都在上面就绪后立即构造。构造本身
	// 不启动任何循环，首轮与循环由 `startWorkerRuntime` 在 serve / gateway 上启动。
	// 装配失败即整条重路径失败：半套装配会让"领到了却交还不了"变成静默租约泄漏。
	// The reclaimer is assembled once here, after the flow service and the lease port;
	// construction starts nothing, and a partial assembly aborts the heavy path.
	reclaimer, reclaimErr := newLeaseReclaimer(reclaimerDeps{
		DB:       db,
		Flows:    flowService,
		Leases:   leases,
		Worker:   workerIdentity,
		Logger:   logger,
		Now:      a.nowOr(),
		LeaseTTL: workerCfg.LeaseTTL(),
		Learning: learningRepo,
		// A1 的未重放动作要挂回 Run 时间线（§2.3.5 A1）；同一个诊断写入器与运行路径
		// 共用实例，读到的就是刚写下的那批事件。
		Diagnostics: diagRecorder,
	})
	if reclaimErr != nil {
		return nil, abortRuntime(reclaimErr, db, lock)
	}
	// 资产就绪门的发布盒（US-25C ADJ-1）：调度器在这里构造，同步器在 run.go 才建，
	// 由后者把实例写进盒子；未写入前到期行一律读作未就绪（不领取、不推进）。
	// The readiness box bridges the sync loop (built later in run.go) to the scheduler.
	readyHashBox := &assetReadyHashBox{}
	scheduler := a.newScheduler(taskStore,
		&admittedRunInvoker{inner: service, admission: admission},
		agents.Entries(), delivery, runs,
		lifecycleScheduleGateFor(lifecycle, versions), cfg, logger, ownerKeys, adminPosture,
		leases, workerIdentity, role, readyHashBox)
	// 两段绑定的第二段（颗粒度文档 §3.1.17）：runtime 就绪后把生产编辑后端交给工具
	// 槽位。此后 `manage_schedule` 的写入门 = 归属预检 → 工作区写闸门 → 即时对账。
	// 适配器同时保留在运行期上：配置热加载的调度联动必须走**同一实例**（约束 39 的
	// 私有 claim 基线随之增减），另建一个只用裸 scheduler 的联动器会让基线分叉。
	// Second half of the two-stage binding: the production editor backend is installed
	// once the loader and scheduler exist, and the same instance is retained for hot reload.
	scheduleEditor := newScheduleEditorAdapter(loader, root, entries, scheduler, logger)
	scheduleSlot.Bind(scheduleEditor)

	// ⑦ US-25C 两个后台同步器的**依赖包**（§2.3.1 / §3.1.4）：这里只构造，不启动。
	// 实例与首轮同步归 run.go（需要 serve / gateway 的运行上下文），后台循环随 ctx 退出、
	// 停机 join 在关库之前（§2.3.4）。角色差异按 §2.3.1 表：Worker 拿注册表 / 调度集合 /
	// Entry 构建器，控制面拿技能修复入口；资源准备闭包两侧相同（都持 ResourceGate 写锁、
	// 只读本轮快照，版本与凭据未变时零副作用复用）。
	// The two sync dependency bundles are assembled here and started nowhere; run.go owns
	// the instances, the first round and the shutdown join.
	syncAssets := assetsSettings(cfg)
	syncPolicy := policySettings(cfg)
	syncNow := a.nowOr()
	// 控制面的运行期技能修复器（§2.3.7）：与启动兜底共用同一套重发实现与同一把
	// AgentGate；Worker 不建实例——`assetSyncRepair` 也按角色再挡一次。
	// The control-only runtime skill repairer shares the startup drain's republisher.
	var runtimeRepairer *SkillRepairer
	if role.allowSharedInit(sharedInitSkillRepair) {
		runtimeRepairer = NewSkillRepairer(SkillRepairerDeps{
			Distribution: postgres.NewAssetDistributionStore(db),
			Versions:     versions,
			Admin: newSkillRepublisher(skillRepublishDeps{
				AssetRoot:     bundleRoot,
				WorkspaceRoot: root,
				Versions:      versions,
				Operations:    ops,
				Payloads:      postgres.NewAssetDistributionStore(db),
				PayloadStore:  postgres.NewBundlePayloadStore(db),
				AgentGate:     agentGate,
				MaxBytes:      resolveMaxBundleBytes(cfg),
				Logger:        logger,
				Now:           syncNow,
			}),
			Logger:   logger,
			Now:      syncNow,
			Batch:    skillRepairDefaultBatch,
			Interval: syncAssets.SyncInterval,
		})
	}
	assetSyncDeps := &AssetSyncDeps{
		WorkspaceRoot: root,
		AssetRoot:     bundleRoot,
		Distribution:  postgres.NewAssetDistributionStore(db),
		Versions:      versions,
		Providers: func(ctx context.Context) (CatalogReport, error) {
			resourceGate.Lock()
			defer resourceGate.Unlock()
			return prepareProviderCatalog(ctx, providerPrepareDeps{
				catalog:     catalogStore,
				store:       versions,
				override:    a.providerOverride,
				credentials: resolver,
				logger:      logger,
			})
		},
		MCP: func(ctx context.Context) (MCPReport, error) {
			// **不得**在此持 `ResourceGate`（§2.3.8 M3）：受控重建的提交段在
			// `commitReplace` 里按 `ResourceGate → entry.mu` 自己取同一把写锁，而
			// `core.ResourceGate` 是不可重入的 `sync.RWMutex`——外层再持会在第一次
			// 真正重建时自锁，准备从"失败关闭"变成"永久挂住"（2026-11-10 复核 P1）。
			// 本轮一致性由目标快照 + 换入前复核保证：指针在窗口内移动 / 凭据换代都由
			// `PrepareVerify` 判 superseded，候选不换入。
			// The commit section takes the same (non-reentrant) gate itself; consistency
			// comes from the round snapshot plus the pre-swap recheck.
			// 准备输入按**当轮**快照重新解析（§2.3.8 M5）：发布名单可在两轮之间增长，
			// 沿用启动期名单会让新增 server 永远等不到准备。
			// Targets are re-resolved from the round's own snapshot.
			targets, configs, err := publishedMCPPrepareTargets(ctx, versions, fileServers, mcpCredentials, logger)
			if err != nil {
				return MCPReport{}, err
			}
			mcpRT.ReplaceSpecs(configs)
			return prepareMCPInstances(ctx, mcpPrepareDeps{
				Targets: func(context.Context) ([]mcpPrepareTarget, error) { return targets, nil },
				Prepare: mcpSupervisor.PrepareAtApplied,
				Status:  mcpSupervisor.Status,
			})
		},
		Repair:        assetSyncRepair(role, runtimeRepairer),
		Logger:        logger,
		Now:           syncNow,
		Interval:      syncAssets.SyncInterval,
		Timeout:       syncAssets.SyncTimeout,
		Batch:         syncAssets.SyncBatch,
		MaxBytes:      syncAssets.MaxBundleBytes,
		FullScanEvery: syncAssets.FullScanEvery,
	}
	// 资源准备快照的交接槽（§3.1.4 C14 / §2.3.5.1 T2）：同步器每轮在 prepare() 里写入
	// 本轮实际准备出的 Providers / MCP / Tools，钉住装载面读的必须是**同一份**快照——
	// 准备闭包要持 ResourceGate 写锁，准入期重跑一次会与发布互锁。
	// The prepared-resources slot hands the round's snapshot to the pinned entry source.
	preparedBox := &preparedResourcesBox{}
	assetSyncDeps.prepared = preparedBox
	if role != roleControl {
		// Worker 才有的三件写面（§2.3.1）：注册表替换、调度集合替换与 Entry 构建。
		// The three worker-only faces: registry, schedule set and entry building.
		assetSyncDeps.Registry = agents
		assetSyncDeps.Scheduler = scheduler
		assetSyncDeps.Loader = loader
	}
	// 钉住装载面（US-25C §2.3.5.1 T2）：把「按领取时固定的 bundle 哈希构建 Entry」
	// 绑进注册表。两种角色都要接——调度器在 serve 与 gateway 上都会领取到期行，
	// 控制面的同步项同样走 buildEntry（技能在这之前已物化），只是不做注册表替换。
	// Both roles claim due rows, so both bind the pinned entry source.
	agents.BindPinned(newPinnedEntries(bundleRoot, root, postgres.NewAssetDistributionStore(db), loader, preparedBox))
	policyRefresherDeps := &PolicyRefresherDeps{
		Allowlist:  networkAllowlist,
		Denylist:   networkDenylist,
		Exceptions: networkExceptions,
		Execution:  executionPolicy,
		// 任一步内容变化即收紧已建立的 MCP http 连接（撤销是安全方向）。
		// A changed round tightens every live MCP http link, which is the safe side.
		RevokeHTTP: func() int {
			return len(mcpSupervisor.RevokeDisallowedHTTP(policy.AllowsHost))
		},
		Logger:   logger,
		Now:      syncNow,
		Interval: syncPolicy.SyncInterval,
		MaxStale: syncPolicy.MaxStale,
	}

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
	// 知识集合与注册闸门共用 `knowledgeWire.Collections` 这一个存储实例——两处判据
	// 若各接一份，"保存时通过、注册时不注册"就会再次出现。
	// One save-time binding check shared by the content editor and submit paths; the
	// collection reader is the very store the registration gate consults.
	saveBindings := SaveBindingCheck(lifecycle, BindingCheckDeps{
		Resources:      versions,
		Notify:         notifyAdminStore,
		NotifyChannels: channels,
		Credentials:    resolver,
		Collections:    knowledgeWire.Collections,
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
		Archives:     postgres.NewAgentAdminStore(db),
		Registry:     agents,
		Scheduler:    scheduleEditor,
		RunCanceller: runCancellerAdapter{runs: runs, service: service},
		// 复制前的源专属判定与映射目标校验读**同一批**资源存储；绑定投影重建面与启动
		// 全量重建共用同一个实例（失败集合因此只有一份真值）。
		// Copy-time checks and projection rebuilds share the assembly's stores and projector.
		Resources:   versions,
		NotifyAdmin: notifyAdminStore,
		Projection:  projection,
		// 资产发布面（US-24 §2.3.9 / FR-24-25）：创建 / 提交 / 复制 / 恢复成功之后把
		// **落盘后的定义**构建成内容寻址 bundle 并推进共享指针——准入因此总能读到与本机
		// 定义一致的资产；运行中的 Run 仍固定在开始时那个哈希目录（FR-24-42）。构建与
		// 发布与 `basalt assets install --agent` 是同一份实现，不新增协议。
		// The console write path publishes the committed definition through the same chain
		// as `assets install --agent`.
		PublishBundle: func(ctx context.Context, agent string, actor core.Actor) (int64, error) {
			return publishWorkspaceAgentBundle(ctx, workspacePublishDeps{
				Root:         root,
				BundleRoot:   bundleRoot,
				Versions:     versions,
				Operations:   ops,
				Payloads:     postgres.NewAssetDistributionStore(db),
				PayloadStore: postgres.NewBundlePayloadStore(db),
				MaxBytes:     resolveMaxBundleBytes(cfg),
				Actor:        actor,
				Logger:       logger,
				Now:          now,
			}, agent)
		},
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
		// 依赖轴按**程序目录**下的 `runtime/` 与版本清单判定（14D / 设计 §2.3.4）：
		// 只 stat、不启动进程；未登记组件输出 null。
		MCPDependencies: newProgramMCPDependencyProbe().Diagnose,
		Credentials:     credentials,
		// MCP 逐槽位托管凭据的声明 / 可用性 / 写入面（14D）。
		MCPCredentials: mcpCredentials,
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
		// 技能发布联动重发（US-25C §2.3.7）：Agent 锁必须是进程里**那一把**（与内容编辑 /
		// 准入同一实例），否则联动与编辑互不互斥；载荷面与 `PublishBundle` 用同一组生产
		// 实现，重建来源只有已安装目录与已发布技能目录。
		// Linkage publishing shares the process-wide agent gate and the same payload stores
		// as the console publish path.
		AgentGate:      agentGate,
		AssetRoot:      bundleRoot,
		Payloads:       postgres.NewAssetDistributionStore(db),
		PayloadStore:   postgres.NewBundlePayloadStore(db),
		MaxBundleBytes: resolveMaxBundleBytes(cfg),
		Logger:         logger,
		Now:            now,
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
		RunRefs:    postgres.NewNotifyDeliveryStore(db),
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
	channelRead := newNotifyChannelReadPort(postgres.NewNotifyChannelRepo(db), credentials, versions,
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
		// MCP 槽位凭据一旦撤销，连接必须**立即**回收（14D C8）：撤销不等一次 apply，
		// 也不靠下一次重启。supervisor 缺席时保持 nil——撤销照常记账，只是没有连接可回收。
		// A revoked MCP slot tears its connection down now, not at the next apply.
		MCPRevoker: mcpRevokerOf(mcpRT),
		Logger:     logger,
		Now:        now,
	})
	if err != nil {
		return nil, abortRuntime(err, db, lock)
	}

	logger.Info("重路径装配完成", "agents", len(agents.Entries()), "tools", registry.Names(),
		"mcp_connected", len(mcpRT.ConnectedNames()))
	return &heavyRuntime{
		isolation:     isolationRT,
		flows:         flowRuntime,
		observability: observation,
		// 角色由子命令透传（T032）：共享初始化写的地面已在各点上按
		// `role.allowSharedInit(...)` 逐项包裹，这里只记录本进程的真实角色供装配产物
		// 与停机路径共用。
		// The real subcommand role travels here for downstream assembly products.
		role:              role,
		lock:              lock,
		db:                db,
		service:           service,
		admission:         admission,
		runs:              runs,
		sessions:          sessions,
		invocations:       invocations,
		llmCalls:          llmCalls,
		diagnostics:       diagReader,
		diagRecorder:      diagRecorder,
		registry:          registry,
		mcpSupervisor:     mcpSupervisor,
		networkAllowlist:  networkAllowlist,
		networkDenylist:   networkDenylist,
		networkExceptions: networkExceptions,
		execPolicy:        executionPolicy,
		scheduler:         scheduler,
		shutdown:          shutdownCoord,
		workerIdentity:    workerIdentity,
		reclaimer:         reclaimer,
		adminPosture:      adminPosture,
		scheduleSlot:      scheduleSlot,
		scheduleEditor:    scheduleEditor,
		// 两个后台同步器的**依赖包**在装配期构造（构造器不启动，见 §3.1.4）；实例
		// 由 run.go 在调度启动前建、启动后跑，停机时 join 在关库之前（§2.3.4）。
		// The sync dependency structs are assembled here; run.go builds, starts and
		// joins the instances around scheduler startup and before the DB closes.
		assetSyncDeps:       assetSyncDeps,
		assetReadyHash:      readyHashBox,
		policyRefresherDeps: policyRefresherDeps,
		agents:              agents,
		agentGate:           agentGate,
		lifecycle:           lifecycle,
		resourceGate:        resourceGate,
		versions:            versions,
		ops:                 ops,
		credentials:         credentials,
		connectionTests:     connectionTests,
		notifyAdminStore:    notifyAdminStore,
		resolver:            resolver,
		secrets:             secrets,
		instanceSecrets:     instanceSecrets,
		mcpRT:               mcpRT,
		mcpCredentials:      mcpCredentials,
		skills:              skills,
		agentAdmin:          agentAdmin,
		projection:          projection,
		resourceAdmin:       resourceAdmin,
		notifyAdmin:         notifyAdmin,
		channelRead:         channelRead,
		catalogStore:        catalogStore,
		bundleRoot:          bundleRoot,
		// US-16 知识与历史两条链路的装配产物：web 的八个消费方端口由 `webOptions` 里的
		// `wireKnowledgePorts` 逐槽位投影；压缩读面的链目录也在这里（与压缩写面同一个
		// `CompactionStore` 实例，读到的就是刚写下的链）。
		// The US-16 knowledge and history assembly products, projected in webOptions.
		knowledge:       knowledgeWire,
		collections:     knowledgeWire.Collections,
		history:         historyWire,
		compactionStore: compactionStore,
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
				read: NewResourceViews(versions, lifecycle, mcpRT, credentials, ResourceViewDeps{
					ConnectionTests: connectionTests,
					MCPCredentials:  mcpCredentials,
				}),
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
		memoryAdmin:     memAdmin,
		learningAdmin:   learningAdmin,
		mcpConns:        mcpConns,
		root:            root,
		loader:          loader,
		workspaceBrowse: workspaceBrowse,
	}, nil
}

// readStore 按**只读路径**打开 PostgreSQL 句柄：不取实例锁、不做崩溃恢复、不清理锁文件，
// 也**不建库 / 不迁移**——轻路径里需要数据的命令（`session list`）用它。缺 DSN 即报错，
// 不回退 SQLite（D-01）。
// readStore is the read-only path: no instance lock, no recovery, no lock-file cleanup
// and no migration. A missing DSN is an error, never a silent SQLite fallback.
func (a *App) readStore(ctx context.Context) (*postgres.DB, error) {
	if a.cfg == nil {
		return nil, errors.New("实例配置缺失：没有 database.dsn，只读命令无法连接 PostgreSQL")
	}
	db, err := postgres.OpenReadOnly(ctx, postgresConfig(a.cfg.Database))
	if err != nil {
		return nil, err
	}
	// 只读打开不迁移，所以初始化状态要自己探：版本表缺失 → ErrDatabaseMissing，与重路径
	// "库不存在"同一语义（口径 23）。少了这一步，列表命令会对空 schema 返回空清单，把
	// "还没初始化"伪装成"没有数据"。
	// The read-only open never migrates, so the version probe is what distinguishes an
	// uninitialised database from an empty one.
	if _, err := db.SchemaVersion(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w（释放只读句柄失败: %v）", err, closeErr)
		}
		return nil, err
	}
	return db, nil
}

// abortRuntime 把失败与已取得资源的释放并成一条错误链——半开的句柄不回给调用方。`db` 为
// nil 表示库还没打开（第二步之前失败），此时只放锁；锁非 nil 是本函数的调用前提。
// abortRuntime folds a failure together with the release of whatever was already acquired.
func abortRuntime(err error, db *postgres.DB, lock *workspace.InstanceLock) error {
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
func authorPublishedProviders(ctx context.Context, versions *postgres.ResourceStore) ([]workspace.ProviderEntry, error) {
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
func authorPublishedSkills(ctx context.Context, versions *postgres.ResourceStore) ([]string, error) {
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
func authorAppliedMCPServers(ctx context.Context, versions *postgres.ResourceStore, mcpRT *mcpRuntime) ([]string, error) {
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
// 同一份：① 重路径在注册完十二件工具之后拿它校验注册池（不一致即拒绝启动）；② 轻路径
// （`profile list` / `profile show` 经 `agentsPort`）不建注册池，直接拿它当池做装载判定
// （池外工具名 → 整只不注册，宪法 IV）。两条路因此对同一份 AGENT.md 给出同一个结论。
// 顺序 = `tool.Registry.Names()` 的排序序（比较逐名且保序）——**不得**按主题分组重排。
// declaredToolNames is the pool declared once at assembly time and read by both paths: the
// heavy path verifies its registry against it, the light path judges admission with it. The
// order matches Registry.Names(), which is sorted.
var declaredToolNames = []string{
	"http_get", "http_post", "list_dir", "manage_schedule", "notify", "read_file",
	"recall_memory", "run_code", "save_memory", "shell", "web_search", "write_file",
}

// providerNameUnion 返回"配置文件名字 ∪ 未删除发布指针名字"的并集（去重、升序），只服务
// 启动基线之前的第一遍装载：那一刻尚未跑基线（首启时一个 Provider 都没发布），而后续启动
// 又可能存在"文件已删、指针保留"的条目。它**不是**运行目录的构建来源——Provider 目录与
// 注册结论都在对账之后按已发布版本重算（约束 18）。
// providerNameUnion merges the configured and published provider names for the
// pre-baseline load only; it never builds the runtime catalog.
func providerNameUnion(ctx context.Context, versions *postgres.ResourceStore, cfg *cli.Config) ([]string, error) {
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

// assetsSettings 取出 `assets` 段；配置文件整体缺失时（`cfg == nil` 或未解码的零值）
// 退回 §3.3 的冻结缺省。段存在时解析入口已填入缺省并通过装载期校验，这里不再判——
// 与 `schedulerSettings` / `workerSettings` 同一套取数口径。
// assetsSettings falls back to the frozen defaults only when the config file itself is
// absent; a present section was defaulted and validated at load time.
func assetsSettings(cfg *cli.Config) cli.AssetsConfig {
	if cfg == nil || cfg.Assets.SyncTimeout == 0 {
		return cli.DefaultAssetsConfig()
	}
	return cfg.Assets
}

// policySettings 取出 `policy` 段，缺省口径同上（`max_stale` 是段存在与否的哨兵）。
// policySettings falls back to the frozen defaults when the policy section is absent.
func policySettings(cfg *cli.Config) cli.PolicyConfig {
	if cfg == nil || cfg.Policy.MaxStale == 0 {
		return cli.DefaultPolicyConfig()
	}
	return cfg.Policy
}

// memoryCarrier 返回运行期唯一的长期记忆载体：PostgreSQL（D-18 / D-22）。
//
// 没有载体开关可读：`memory.backend` 已退役（装载期出现即拒绝启动），markdown 文件
// 载体只剩一次性只读导入这一个用途（`db switch` 走 `memory.NewMarkdownLegacySource`），
// 运行期**不得**回读工作区文件，也不得与 PG 双读（D-22）。
//
// 缺库句柄即拒绝启动：装配层不假设调用方一定走过装载期那道门（fail-closed 双保险，
// 颗粒度文档 §3.5 第 9 项）——退回文件载体会让"库没接上"表现成"记忆还在，只是换了个
// 地方"，而那个地方此后不再同步。
// memoryCarrier returns the single runtime carrier: PostgreSQL. There is no switch to
// read, and a missing handle is a refusal rather than a silent fall back to files.
func memoryCarrier(db *postgres.DB, now func() time.Time) (core.LongTermMemoryStore, error) {
	if db == nil {
		return nil, fmt.Errorf(
			"记忆载体：运行时唯一载体为 PostgreSQL（D-18），装配时缺库句柄——拒绝启动，不回退 markdown 文件")
	}
	return postgres.NewMemoryStore(db, now), nil
}

// heavyRuntime 是重路径运行期的资源句柄：实例锁、数据库、运行入口与三个只读消费面。
// 生命周期 = 一次重命令——`chat` 在整场交互期间持有它，退出时释放。
// heavyRuntime owns the heavy path's resources for the lifetime of one heavy command.
//
// 名字带 `heavy` 前缀是为了与导出面 `Runtime` 明确分开：包内这两个名字只差大小写，
// 读代码时一眼能看出"这是内部接线"还是"这是包外拿到的句柄"。
// The heavy prefix keeps the internal handle distinguishable from the exported Runtime.
type heavyRuntime struct {
	isolation     *isolationRuntime
	flows         *flowRuntime
	observability *Observability
	// role 是本进程的启动角色（US-25C D-25C-06：`serve` = control，`gateway` =
	// worker）。零值是 `roleWorker`（最小权限）——**不得**依赖零值表达控制面：
	// 装配必须显式赋值；T032 把子命令判定的角色经 `newRuntime(ctx, role)` 透传进来。
	// role is the startup role; the zero value is the least-privileged worker, so the
	// assembly always assigns it explicitly.
	role runtimeRole
	// lock 是实例锁：进程存续期间持有（写模式的语义就是"这一进程是唯一写者"）。
	lock *workspace.InstanceLock

	// db 是写模式打开的库句柄（读写两条池）。
	db *postgres.DB

	// service 是唯一运行入口。
	service *core.AgentService

	// admission 是图 6 [3] 的运行准入对象：调度器已用它包装运行入口，web 的
	// RunService 适配器在 `webOptions` 里拿**同一份**——另建一份会让同一调用方在
	// web 与后台各拿一份 `max_concurrent` 计数，上限形同虚设（C-35）。
	// The same admission object the scheduler uses is handed to the web run entry.
	admission *runAdmission

	// runs 是运行账本的唯一实例：AgentService 写入、web.RunService 适配器读取，
	// 保留句柄是为了让装配面可核"读面与写面确实是同一个仓储"。
	// runs is the one run store shared by the writer and the web read port.
	runs *postgres.RunStore

	// sessions 是会话仓储；命令面经只读端口消费它（`chat` 读回本轮消息、`session list`
	// 列清单）——写路径只有 core 一条。
	sessions *postgres.SessionStore

	// invocations / llmCalls 是两个审计仓储：装配期建好之后**同时**喂给循环（写路径）与
	// 导出面（只读路径）。一份实现、两个消费面——读面另起一个实例会让"读到的"与"写下的"
	// 隔着两条连接池，排障时说不清是不是同一个库。
	// One implementation feeds both the write path (the loop) and the read face.
	invocations *postgres.ToolInvocationRepo
	llmCalls    *postgres.LLMCallRepo

	// diagnostics 是诊断事件的读面（`GET /api/v1/diagnostics`）。它与运行路径上的
	// 写入器由同一次 `newDiagnosticsPorts` 构造、共用同一个存储适配器——读不到就说明
	// 生产端没发，而不是"两套实例各写各的"。
	// diagnostics is the read face of the very store the run path writes to.
	diagnostics *diagnostics.Reader

	// diagRecorder 是同一个写入器的**公开面**（V-8）：入口速率中间件经 web 的写端口
	// 用它记 `quota_decision`。与运行路径共用同一个实例——两条来源的事件因此落进
	// 同一个存储、可被同一个读端点按关联标识查回。
	// The same recorder, exposed to the ingress middleware through the web write port.
	diagRecorder diagnosticRecorder

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

	// networkDenylist 是禁止域名表的管理服务（14D；同一张表的第二行）：`GET /sandbox`
	// 的 `denied_domains` 与 `PUT /sandbox/denied-domains` 读写的就是这一行。
	// networkDenylist is the second row of the same policy table.
	networkDenylist *NetworkAllowlist

	// networkExceptions 是访问例外的管理服务：`/network-exceptions` 的读写与守门器
	// 快照共用同一份集合。
	// networkExceptions is the managed exception set shared with the dial guard.
	networkExceptions *NetworkExceptionService

	// execPolicy 是执行档位的管理服务（`/execution-policy`）：与运行期策略同一实例，
	// 切换成功即对**下一次**授权判定生效；在途 Run 与已建立连接不追溯撤回。
	// execPolicy is the managed profile service bound to the very live policy.
	execPolicy *ExecutionPolicyService

	// scheduler 是运行期调度器（重路径始终构造，chat / tool 不 Start）。停机时**最先**
	// 停它：Stop 可耗满 70s 宽限，必须在库关闭之前收尾在途运行（约束 22）。
	// The scheduler is always constructed; Stop runs first so in-flight runs finish
	// before the database closes.
	scheduler *core.Scheduler

	// worker 是 `serve` / `gateway` 的 Worker 身份与诊断心跳（US-24 T006）：只有进入
	// 调度分支才登记；chat / tool / secrets 不登记、不参与多 Worker 协调。停机时在
	// 调度器停止之后、库关闭之前标记下线（图 1 的次序）。
	// worker is the serve/gateway identity and diagnostic heartbeat; nil for chat/tool.
	// It stops after the scheduler and before the database closes.
	worker *workerRuntime

	// shutdown 是图 11 的停机预算与退出点（US-24 T026）。只有 `worker != nil` 的
	// serve / gateway 走编排路径；chat / tool 与各测试夹具继续用原来的释放顺序。
	// shutdown carries the figure-11 budgets; only serve/gateway (worker != nil) use it.
	shutdown *shutdownCoordinator

	// workerIdentity 是装配期生成一次的进程身份（US-24 T007）：运行入口按它写租约
	// 归属，`startWorkerRuntime` 注册与心跳复用同一个值。chat / tool 也持有它——运行
	// 租约照样写上，崩溃后的回收因此不区分触发源；只是不登记 `worker_nodes`。
	// workerIdentity is the assembly-time identity shared by run leases and the serve
	// registration; chat/tool hold it too but never register.
	workerIdentity core.WorkerIdentity

	// reclaimer 是重路径上**唯一**的回收器实例（US-24 T017 / §2.3.5 图 5）：首轮与
	// 循环都由 `startWorkerRuntime` 取得它后启动。构造不启动；chat / tool 不启动任何
	// 循环——它们没有协调身份，跑回收只会替别的 Worker 做没有归属依据的收尾。
	// reclaimer is the one heavy-path reclaim implementation; only serve/gateway start it.
	reclaimer core.Reclaimer

	// adminPosture 是启动期管理认证姿态的发布盒（T027 / §2.14）：web 面在监听成功后
	// 写入，调度器经它迟绑定读取——`unavailable` 时 admin-builtin 后台任务不再执行。
	// adminPosture carries the startup posture from the web face to the scheduler.
	adminPosture *postureBox

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

	// assetSyncDeps 是自动分发同步器的依赖包（US-25C §3.1.4）：装配期构造、构造不启动；
	// run.go 用它在调度启动前建实例、跑首轮同步，启动后跑后台循环。Worker 与控制面
	// 各按 §2.3.1 表取不同字段（Registry / Scheduler / Loader 只在 Worker 侧装配）。
	// assetSyncDeps is the asset-sync dependency bundle; construction never starts it.
	assetSyncDeps *AssetSyncDeps

	// assetReadyHash 是到期触发就绪门的迟绑定盒（US-25C ADJ-1）：调度器构造时经它
	// 取门，run.go 在建好同步器后把实例发布进来。构造后、发布前，到期行读作未就绪。
	// assetReadyHash bridges the sync loop into the already-built scheduler gate.
	assetReadyHash *assetReadyHashBox

	// assetSync 是 run.go 建好的同步器实例（构造后先跑 SyncOnce）；只由 serve / gateway
	// 路径赋值，其余重命令保持 nil。停机 join 在关库之前（§2.3.4）。
	// assetSync is built by run.go for serve/gateway only, started after the first sync.
	assetSync *AssetSync

	// assetSyncDone 在后台同步循环退出时关闭：停机 join 用它证明循环已收尾，
	// 库才是最后关闭的那一个（§2.3.4 的停机次序）。
	// assetSyncDone closes when the sync loop returns, so shutdown joins it first.
	assetSyncDone chan struct{}

	// policyRefresherDeps / policyRefresher / policyRefreshDone 与上面三件同构，
	// 只是对象换成四类运行期策略的刷新器（§3.1.9）。
	// The policy refresher mirrors the asset sync trio.
	policyRefresherDeps *PolicyRefresherDeps
	policyRefresher     *PolicyRefresher
	policyRefreshDone   chan struct{}

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
	versions    *postgres.ResourceStore
	ops         *postgres.ManagementStore
	credentials *postgres.CredentialStore

	// connectionTests 是资源「最后一次连接测试」投影：测试端点写入、管理读面读取，
	// 保留句柄让装配面可核"写的与读的是同一张表"（历史仍在管理操作台账）。
	// connectionTests is the last-test projection shared by the writers and readers.
	connectionTests *postgres.ConnectionTestStore

	// notifyAdminStore 是通知渠道仓储：保存期绑定校验与通知写面读的是同一实例，
	// 保留句柄让装配面可核"写面与校验看到的是同一份渠道真值"。
	// notifyAdminStore is the one channel store shared by the write surface and the
	// save-time binding check.
	notifyAdminStore *postgres.NotifyAdminStore

	// resolver / secrets 是凭据解析器与 secrets 子命令的装配产物：命令面经
	// `Runtime.Secrets()` 取 `cli.SecretsService` 端口，解析器本身不再外露。
	// The resolver and the secrets service are retained here; only the latter crosses
	// the package seam.
	resolver *credentialResolver
	secrets  SecretsService

	// instanceSecrets 是实例主密钥的判定表与显式初始化服务（14D）：启动时的
	// `Ensure` 与命令面 `secrets` 子命令、web 面 `GET/POST /instance/secrets` 读的都是
	// **同一判定**，页面报出的状态就是下一次启动会采取的动作。保留句柄让 `webOptions`
	// 投影这个实例本身——命令面的 Status 在未配置主密钥路径时会短路成错误，web 面
	// 需要的是状态，不是错误。
	// instanceSecrets is the same decision table Ensure and the secrets subcommands use;
	// the web port projects this instance itself because the CLI Status short-circuits.
	instanceSecrets *InstanceSecretsService

	// mcpRT 是已发布 MCP 定义的只读运行期句柄（配置与连接两份名单）；skills 是技能库
	// 句柄——准入解析、基线发布与发布版本快照共用同一实例。
	// mcpRT 之外的 mcpCredentials 是逐槽位凭据状态服务：启动回填、写面与**管理读面**
	// 必须读同一实例，否则列表页的 `credential_keys` 恒为空、声明过的槽位被误判成
	// 「未声明来源」。此前只在写端口注入，读端口漏传，正是这个症状的成因。
	// mcpRT and skills are the runtime handles shared by admission and the baseline;
	// mcpCredentials is the one per-slot credential status service both the write and
	// the read ports must share.
	mcpRT          *mcpRuntime
	mcpCredentials *MCPCredentialService
	skills         *workspace.SkillLibrary

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

	// bundleRoot 是 `assets.bundle_root` 解析后的内容寻址资产根（US-24 §2.3.9）：装配
	// 期算一次，准入校验与包外发布入口（评测基座）读的是**同一个**绝对路径——两处各
	// 拼一次会让"装在哪"与"读哪"分叉（FR-24-44）。
	// bundleRoot is the resolved content-addressed asset root shared by admission and
	// the out-of-package publish entry.
	bundleRoot string

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

	// memoryAdmin 是记忆管理面（结构化读模型 + 四个写动作）：与门面共用同一个存储
	// 载体，启动恢复也用它查证 `memory.*` 的 pending 行。
	// memoryAdmin shares the facade's carrier and backs the memory recovery verdicts.
	memoryAdmin *MemoryAdmin

	// learningAdmin 是 US-22 的只读学习查询面：状态 / 抽取 / 草案投影，供 web 与
	// CLI 读取；抽取服务本身经 `core.ServiceOptions.Learning` 进入 Run 收尾路径。
	// learningAdmin is the read-only learning surface; the extraction service joins
	// the run finish path through core.ServiceOptions.Learning.
	learningAdmin *LearningAdmin

	// mcpConns 是已连接的 MCP server 与工具（`/info` 的连接数与 `/tools` 的 mcp 来源）。
	// mcpConns carries the connected MCP servers and their tools.
	mcpConns []tool.MCPConnection

	// knowledge / history 是 US-16 两条链路的装配产物（`wire_knowledge.go`）：
	// `webOptions` 经 `wireKnowledgePorts` 逐槽位投影到 `web.Options`，缺件的槽位保持
	// nil、对应路由按固定 500 失败关闭。knowledge 里同时带着压缩来源闸门要的校验器
	// ——它与来源视图读面是**同一个** `SourceViewer`，所以判定口径只有一份。
	// The US-16 assembly products projected field-by-field in webOptions; the same
	// SourceViewer backs both the read faces and the compaction validity gate.
	knowledge *knowledgeWiring
	// collections 是知识集合目录的"名字 → 稳定 ID"读面：`serve` 的配置监听用它
	// 判定声明过的集合是否还存在（D-19），与注册装配共用同一个存储实例。
	// collections is the name-resolution face handed to the serve-time watcher.
	collections KnowledgeCollectionSource
	history     historyWiring

	// compactionStore 是压缩链的读面（`GET /sessions/{id}/compactions`）：与压缩写面
	// 共用同一个实例，页面读到的链就是刚追加的那条。
	// compactionStore is the read face of the very chain the compactor appends to.
	compactionStore *postgres.CompactionStore

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
//
// US-24 T026：`serve` / `gateway`（`worker != nil`）走图 11 的编排路径
// （`closeCoordinated`，含预算、取消后 join 与超界直接退出）；chat / tool 与测试
// 夹具保持原来的释放顺序，行为不变。
// Serve/gateway run the coordinated figure-11 shutdown; everything else keeps the
// historical release order.
func (r *heavyRuntime) Close() error {
	if r.shutdown != nil && r.worker != nil {
		return r.closeCoordinated()
	}
	return r.closeLegacy()
}

// closeLegacy 是 chat / tool 与测试夹具的释放顺序（US-24 之前的行为，逐字保留）。
// closeLegacy is the historical release order, unchanged.
func (r *heavyRuntime) closeLegacy() error {
	var errs []error
	if r.flows != nil {
		errs = append(errs, r.flows.Close())
	}
	if r.observability != nil {
		errs = append(errs, r.observability.Close())
	}
	if r.watch != nil {
		errs = append(errs, r.watch.Close())
	}
	if r.scheduler != nil {
		r.scheduler.Stop()
	}
	// Worker 下线标记在调度器停止之后、库关闭之前：先不再领取，再写下线，最后关连接
	// （模块卡图 1 的停机次序；心跳只为诊断，失败并入错误链但不回滚停机）。
	// Mark the worker stopped after the scheduler and before the database closes.
	if r.worker != nil {
		errs = append(errs, r.worker.Stop(context.Background()))
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

// runCancellerAdapter 把运行账本与运行入口适配成 Agent 写面的归档端口（约束 32 ③ / 图 8
// 的 [3][4]）：快照四条完成判据、尽力送达取消信号。适配器不复制任何循环逻辑：取消之后由谁
// 把 Run 写成终态仍归 `ReActLoop`（宪法 2），`Cancel` 本身从不写终态；有界等待的轮询节奏
// 归 `AgentAdmin`（同一个窗口对四条判据统一计时）。
// runCancellerAdapter adapts the run ledger and the run service to the archive port. It
// signals only; the loop alone writes terminal states, and the bounded wait is AgentAdmin's.
type runCancellerAdapter struct {
	// runs 是共享运行账本：残量快照与跨 Worker 取消标志都从这里读写（每条路径必填）。
	// runs is the shared ledger; every assembly must supply it.
	runs *postgres.RunStore
	// service 是本进程的运行入口；nil = CLI 装配（不跑循环），CancelRun 直接写共享标志。
	// A nil service means the CLI assembly, where cancel is a flag write.
	service *core.AgentService
	// poll 是取消信号送达后的等待上限；≤0 取 runCancellerPoll（测试注入小值只为缩短用例）。
	// poll caps how long one cancel signal waits before returning to the caller.
	poll time.Duration
}

const (
	// runCancellerPoll 是单个取消信号送达后的等待上限：100ms 足以让本地循环看到标志，
	// 又不会让逐个 Run 的信号把有界窗口撑成 N×30 秒。
	// runCancellerPoll caps one cancel signal so N runs never spend N×30s of the window.
	runCancellerPoll = 100 * time.Millisecond
)

// 编译期断言：端口形状一旦漂移在这里先红（与 web.go 的端口断言同一纪律）。
// Compile-time assertion: the port shape is fixed here.
var _ RunCanceller = runCancellerAdapter{}

// Residuals 取一次归档完成判据快照（图 8 [4] 的 ①②③④）。判定全部在存储层用常量 SQL
// 完成：本层只做类型透传，不拼查询、不缓存（缓存会把"已达成"读成"仍残留"）。
// Residuals snapshots the four completion predicates; the storage layer owns the SQL.
func (a runCancellerAdapter) Residuals(ctx context.Context, agent string) (core.ArchiveResiduals, error) {
	residuals, err := a.runs.AgentResiduals(ctx, agent)
	if err != nil {
		return core.ArchiveResiduals{}, fmt.Errorf("读取 Agent %s 的归档完成判据: %w", agent, err)
	}
	return residuals, nil
}

// CancelRun 只负责把取消信号送达运行循环：`AgentService.Cancel` 在信号之后至多等**一个
// 轮询间隔**（而不是它自己的 30 秒上限）——整段归档等待由 `AgentAdmin` 的窗口统一计时。
// 行已不存在（已退出并清理）视为已达成。**非属主也可以调用**：远端 Run 的取消由属主在
// 续约 / 检查点观察 `cancel_requested`（FR-24-22）。
//
// `service == nil` 是 CLI 装配：进程不跑 ReAct 循环，任何在跑 Run 都在别的 Worker 上，
// 跨 Worker 取消的唯一合法动作就是把标志写进共享库——与 `AgentService.Cancel` 的远端
// 分支逐字相同。取消仍**不**代替属主提交终态（宪法 2）。
// A nil service is the CLI assembly: with no local loop every live run belongs to
// another worker, so raising the shared flag is the only legal action.
// CancelRun delivers the signal and returns; the bounded wait belongs to AgentAdmin.
func (a runCancellerAdapter) CancelRun(ctx context.Context, runID string) error {
	signalCtx, cancel := context.WithTimeout(ctx, a.interval())
	defer cancel()
	if a.service == nil {
		if _, err := a.runs.RequestCancel(signalCtx, runID); err != nil && !errors.Is(err, core.ErrRunNotFound) {
			return err
		}
		return nil
	}
	if _, err := a.service.Cancel(signalCtx, runID); err != nil && !errors.Is(err, core.ErrRunNotFound) {
		return err
	}
	return nil
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

	// SupportedSchemaVersion 是目标 schema 版本；生产传 postgres.SupportedSchemaVersion。
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

	// DatabaseDSN 覆盖配置文件里的 `database.dsn`（可选；评测基座的**存储隔离**）。评测
	// 逐用例建一次性 schema，连接串只在内存里交给装配——不落盘、不进结果文件；真模型档
	// 原样复制运维配置时也靠它把写入挡在实例库之外。空 = 用配置里的值。
	// An in-memory DSN override for the eval base: each case runs in a throwaway schema
	// and the DSN never reaches a file. Empty means "use the configured value".
	DatabaseDSN string
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
		return nil, errors.New("OpenRuntime: SupportedSchemaVersion 必须为正（生产传 postgres.SupportedSchemaVersion）")
	}
	configPath := filepath.Join(opts.WorkDir, configFileName)
	cfg, cfgErr := cli.LoadConfig(configPath, os.LookupEnv)
	// 存储隔离在装载之后、装配之前生效：配置里的 `database.dsn` 缺省时**不**替它填默认
	// （§3.1.9 按子命令分流），评测因此仍会拿到"未配置"这条 fail-closed 错误。
	// The isolation override applies after loading and before assembly; a config without
	// a DSN still fails closed when no override was supplied.
	if cfgErr == nil && opts.DatabaseDSN != "" {
		cfg.Database.DSN = opts.DatabaseDSN
	}
	a := &App{
		workDir:          opts.WorkDir,
		supportedVersion: opts.SupportedSchemaVersion,
		logger:           opts.Logger,
		now:              opts.Now,
		cfg:              cfg,
		cfgErr:           cfgErr,
		providerOverride: opts.ProviderClient,
	}
	// 评测基座按控制面装配：它就是"这条生产路径"的单进程形态（迁移、共享初始化写入
	// 全部执行），与 `serve` 逐字同义，角色显式写死为 control。
	// The evaluation base is the single-process control-plane shape by definition.
	rt, err := a.newRuntime(ctx, roleControl)
	if err != nil {
		return nil, err
	}
	return &Runtime{inner: rt}, nil
}
