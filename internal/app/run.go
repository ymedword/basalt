package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/ymedword/basalt/internal/historyretention"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/storage/postgres"
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
// `tool` 属重路径的理由是**答案的来源**而不是命令的动作：`tool list` 列的是装配期构建的
// 内存注册池，池之外不存在这份清单（它不写任何东西——"只读"的准确含义见颗粒度文档注 9）。
// tool is heavy because of where its answer lives, not because of what it does: the pool
// only exists after assembly.
//
// 清单只有这一份，判据落在命令名上——装配层不猜意图，命令面也不会因为注册顺序不同
// 而走到另一条路上。`serve` / `gateway` 自 US-9 起同属重路径（技术方案 §6.5）：两者都
// 要全量装配，差别只在 HTTP 面与常驻形态。
//
// `secrets` 自 US-12（T135）起重路径：在线子命令都建立在真库与真主密钥之上——`init`
// 要统计当前托管依赖、`revoke-managed` 要写撤销行与操作行，`status` / `verify` 读的
// 也是同一批凭据表。它因此与 `chat` / `tool` 同口径：取实例锁、走迁移，再执行命令；
// 部署类命令（`revoke-managed` 明确要求服务已停止）与运行实例不并存，锁冲突即是该语义。
// secrets is heavy since US-12: the online subcommands sit on the real database and key.
//
// **例外**是 US-16 的两条离线命令（`recover-record` / `adopt-legacy`，见下方
// offlineSecretsCommands）：它们与 `Secrets` 端口分开，正是为了不经这里的装配。
// The two offline secrets commands are the exception; see offlineSecretsCommands below.
var heavyCommands = []string{"chat", "tool", "serve", "gateway", "secrets"}

// offlineSecretsCommands 是 `secrets` 组里**必须**绕开重路径的子命令：`recover-record`
// 与 `adopt-legacy` 各自持自己的工作区锁、自己打开备份 / 隔离副本，属于离线手工流程
// （§2.12.0a / §2.12.0b / §3.1.8b / §3.1.8c）。走重路径会把库迁移、`Ensure` 自动 adopt
// 与 `ApplyPublished` 的 MCP 启动带进来——那正是这两条命令存在要避免的行为；而且重路径
// 已持实例锁，命令自己的取锁会直接 `workspace_busy`。
//
// `check-key`（US-25 P-18）是第三条：Worker 启动预检时控制面可能正在迁移或持锁，
// 预检只读一次密钥文件与直接父目录，**不得**连库、取锁或迁移——否则预检会把
// “密钥不可读”与“库没就绪”混成一个失败，Worker 也就无法执行 78 的部署语义。
// check-key (US-25 P-18) joins them: the preflight reads one file and its directory
// only, never the database, the lock or the migration chain.
// offlineSecretsCommands must never assemble the runtime: each holds its own workspace
// lock and opens its own backup / isolated copy. The heavy path would migrate, adopt and
// start MCP servers — exactly what these offline procedures exist to avoid — and its
// lock would make the command's own acquisition fail as workspace_busy.
var offlineSecretsCommands = []string{"recover-record", "adopt-legacy", "check-key"}

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

	// 启动验证窗口（T143 / §2.12.0b 步骤 7）在**任何装配之前**分流：它不跑 serve 的
	// 兜底初始化（会写工作区）、不取锁、不迁移、不启 MCP / 调度 / 配置监听。把它排在
	// 后面再加开关，等于让"窗口内禁写入"依赖一串 flag 判断——这里让它成为结构性事实。
	// The startup verification window branches off before any assembly, so "no writes,
	// no MCP, no scheduler" is structural rather than a set of flags.
	if windowOpts, matched, parseErr := parseVerificationWindowArgs(args); matched {
		if parseErr != nil {
			NewLogger(nil).Error("验证窗口参数无效", "error", parseErr)
			return cli.ExitStatusOf(parseErr)
		}
		windowOpts.Workspace = filepath.Join(workDir, workspaceDirName)
		return runVerificationWindowMode(ctx, workDir, windowOpts)
	}

	// 新实例判定与首次配置生成**必须**排在 `cli.LoadConfig` 之前（T089）：排在后面会
	// 出现"文件写对了、首个进程仍用缺省预算"，用户必须重启一次才生效。判定只看库与
	// 名册，二者都不在才是新实例——存量无配置实例不得被当成新实例放宽策略。
	// The new-instance check and first-run config generation must precede the load, or the
	// first process would still run on defaults until restarted.
	serveInit := needsServeInit(args)
	bootstrap := instanceBootstrapReport{}
	if serveInit {
		// 配置尚未装载，日志出口还没档位可选：失败用缺省档记一次，错误绝不静默吞掉。
		report, bootErr := ensureNewInstanceConfig(workDir, programRuntimeRoot())
		if bootErr != nil {
			NewLogger(nil).Error("新实例首次初始化失败", "error", bootErr)
			return 1
		}
		bootstrap = report
	}

	configPath := filepath.Join(workDir, configFileName)
	cfg, cfgErr := cli.LoadConfig(configPath, os.LookupEnv)
	logger := bootstrapLogger(cfg, cfgErr, configPath)
	var retentionLog *retentionLogWriter
	if serveInit && cfgErr == nil && cfg.Observability.Retention.HistoryEnabled {
		retentionLog = &retentionLogWriter{}
		logger = loggerWithWriter(cfg, retentionLog)
	}
	if bootstrap.Fresh {
		// 只报**本次真的做了什么**：配置已存在时不得声称"写了 30 轮预算"。
		// Report only what actually happened; an existing config is never rewritten.
		fields := []any{
			"config_created", bootstrap.ConfigCreated,
			"provisioning_recorded", bootstrap.ProvisioningRecorded,
			"marker_created", bootstrap.MarkerCreated, "instance_id", bootstrap.InstanceID,
		}
		if bootstrap.ConfigCreated {
			fields = append(fields, "budget_max_iterations", 30, "max_concurrent_runs", 32)
		}
		logger.Info("新实例：最小配置与实例名册已就绪", fields...)
	} else if serveInit {
		if _, statErr := os.Stat(configPath); errors.Is(statErr, fs.ErrNotExist) {
			// 存量实例缺配置：**不**补生成（没有名册就不猜私有目录），也不替它放宽策略；
			// 必填项缺失会在装配时拒绝启动。这里的告警是"为什么拒"的那半句——只点名事实，
			// 不让运维从"配置校验失败"倒推本工作区其实是一个存量实例。
			// An existing workspace without a config is never bootstrapped; say so before the
			// mandatory-item refusal, so the refusal cannot be mistaken for a broken new instance.
			logger.Warn("存量实例：工作区已有数据库或实例名册，但没有配置文件——不补生成，缺失的必填项会让启动失败")
		}
		// 上次首次启动中断（配置与名册已在盘上、密码还没发放）：如实说明本次是**续做**，
		// 而不是新实例——数据库连不上、DSN 没修好时这句话就是"为什么还会失败"的那半句。
		// A previous first start was interrupted; say that this run resumes it.
		if pending, instanceID, _ := adminTokenProvisioningPending(workDir); pending {
			logger.Warn("首次初始化未完成：配置与实例名册已在盘上，管理面访问密码尚未生成——本次启动继续完成；"+
				"数据库仍不可用时失败不会改写任何既有文件", "instance_id", instanceID)
		}
	}

	if serveInit && (cfgErr == nil || errors.Is(cfgErr, fs.ErrNotExist)) {
		if ingressErr := cli.ValidateServeIngress(cfg); ingressErr != nil {
			logger.Error("服务启动前的入口限流配置不完整，请在 basalt.yaml 中设置后重试", "error", ingressErr)
			return 1
		}
	}

	// 出网受控拨号在**任何客户端构造之前**装配（14D §2.2.2 / D12）：此后本进程内造的
	// 出站客户端都经它拨号——先解析、再按受保护地址段分类、只拨已校验的 IP；代理只认
	// 显式 `http.proxy`，环境变量代理一律不读。装配失败（代理形态非法）拒绝启动：
	// 静默直连等于"以为走了代理、实际换了一个出口"。
	// The outbound guard is installed before any client exists; a bad proxy refuses startup.
	if guardErr := installOutboundGuard(cfg, logger); guardErr != nil {
		logger.Error("出网受控拨号装配失败", "error", guardErr)
		return 1
	}

	// Demo 准备端口（US-25 §2.3.7）：只持配置、工作目录与一个 HTTP 客户端——不连库、
	// 不装配运行期、不取实例锁，因此两条路径都安全带上。构造失败只记一条装配错误并
	// 把端口留空：命令面把 nil 当装配错误报出来，**不**静默跳过。
	// The demo-prepare port holds a config, a work directory and an HTTP client only;
	// a nil port surfaces as a wiring error on the command face.
	// 构造失败时端口必须保持**真正的 nil 接口**：`*DemoPrepareService` 的 nil 指针装进
	// 接口后接口本身不为 nil，命令面的"端口缺失即装配错误"判据会被绕过，随后在 Plan
	// 里以空指针崩溃。只在成功时赋值。
	// Assign only on success: a typed-nil pointer in the interface would defeat the
	// command face's missing-port check.
	var demoPrepare cli.DemoPrepare
	if service, demoPrepareErr := newDemoPrepareService(cfg, cfgErr, workDir, filepath.Join(workDir, workspaceDirName), logger); demoPrepareErr != nil {
		logger.Error("Demo 准备端口装配失败", "error", demoPrepareErr)
	} else {
		demoPrepare = service
	}

	// Store / Workspace / Agents 是装配层注入的适配器（ports.go）：命令面只认自己
	// 声明的端口，storage / workspace 的实现细节不越过这道接缝。
	// The adapters are injected here; the command surface only sees its own ports.
	deps := cli.Deps{
		IsolationProbe:         isolationProbePort{Config: cfg, ConfigErr: cfgErr, Root: filepath.Join(workDir, workspaceDirName), Logger: logger},
		WorkDir:                workDir,
		SupportedSchemaVersion: postgres.SupportedSchemaVersion,
		// init 引导的是 SQLite 源谱系（v6）：与运行期 PG 谱系（v1）分开传，混用会写出
		// "v6 schema + v1 版本行"的库，db export 随即拒绝（T078 实测）。
		// init provisions the SQLite source lineage; it is a different constant.
		SourceSchemaVersion:  storage.SupportedSchemaVersion,
		Logger:               logger,
		Store:                storageStore{},
		Workspace:            workspacePort{},
		AcquireWorkspaceLock: workspacePort{}.AcquireLock,
		// 首次安装准备（US-25 §3.1.1）：`init` 在骨架之后、建库之前调用。无状态、
		// 不取实例锁；`serve` / `gateway` 不走这条端口（serve 在装配前直接调用
		// ensureNewInstanceConfig，gateway 不得写名册或铸钥）。
		// The stateless first-install port for init; serve keeps its own pre-assembly
		// call and gateway never prepares or mints anything.
		FirstInstall: firstInstallAdapter{},
		// Agents 是写模式端口（create / delete 在实现侧取实例锁、开关库、写操作行）——
		// schema 目标版本与命令面同源，构造参数显式传递。
		// The agents port owns the CLI write path; the schema target matches the command face.
		Agents: agentsPort{
			WorkDir:                workDir,
			SupportedSchemaVersion: postgres.SupportedSchemaVersion,
			Database:               databaseConfig(cfg),
			Logger:                 logger,
			// 创建成功后即发布资产（US-24 §2.3.9 / FR-24-25）：CLI 写路径与
			// `assets install --agent` 用同一个资产根，装在哪与读哪不分叉（FR-24-44）；
			// 载荷上限与 serve 同源（US-25C §2.3.2）。
			BundleRoot:     resolveAssetBundleRoot(cfg, workDir),
			MaxBundleBytes: resolveMaxBundleBytes(cfg),
		},
		// PostgreSQL 迁移端口与库、锁、运行期资源都无关（每次调用自建并释放连接池），
		// 因此两条路径都安全带上：`db migrate` 是建库前置，不能要求先有运行期装配。
		// The migration port holds no runtime resources, so both paths carry it: the
		// database must be creatable before any runtime assembly exists.
		DatabaseMigrator: postgresMigratorPort{},
		// 迁移链路端口（export / seal / import / verify / switch）：与迁移端口同理无状态，
		// 每次调用自建句柄。导出只读 SQLite 快照，导入 / 复核 / 切换才打开 PostgreSQL。
		// The migration-chain port, stateless like the migrator: export reads the SQLite
		// snapshot offline, while import/verify/switch open PostgreSQL.
		Migration: migrationPort{Logger: logger},
		// 实例采纳端口（`instance adopt`）：只碰旧 SQLite 工作区与名册，零外呼、不装配
		// 运行期——与迁移链路同理无状态，两条路径都安全带上。
		// The adoption port touches only the legacy SQLite workspace and the roster; it
		// is stateless and therefore safe on both paths.
		InstanceAdopt: instanceAdoptPort{Logger: logger},
		// 两条**离线手工流程**的端口（`secrets recover-record` / `secrets adopt-legacy`）：
		// 它们与 `Secrets` 分开正是为了不经重路径装配——恢复与受控升级只许离线发生，
		// 任何"顺手装配一下运行期"的路径都会把库迁移、自动 adopt 与 MCP 启动带进来。
		// The two offline ports stay off the heavy path on purpose: assembling the
		// runtime here is exactly what these procedures must never do.
		SecretsRecovery:    secretsRecoverPort{Logger: logger},
		LegacySecretsAdopt: legacyAdoptPort{Logger: logger},
		// 交接核对端口（`db verify --handoff`）：只读副本的 SQLite 库、名册、密钥文件与
		// 同级配置，零外呼、不装配运行期——与上面两个离线端口同一条理由，两条路径都安全带上。
		// The handoff gate is read-only and offline, so it is wired on every path.
		Handoff: handoffVerifyPort{Logger: logger},
		Flow:    flowCLIPort{},
		// `demo workflow prepare` 的计划 / 应用两段（US-25 §3.1.1）：同一个端口对象，
		// 计划只发 GET，应用才写。
		// The demo-prepare port backs both halves; only Apply writes.
		DemoPrepare: demoPrepare,
		// 学习准入端口：写命令的实例锁由命令面持有（本适配器不再取锁），读命令走只读
		// 会话；行为与身份口径见 admission_port.go。CLIWriteActor 是七个写动作唯一的
		// 身份来源——命令面没有 --approver 之类旁路。
		// The admission port; the command surface owns the instance lock, and the actor
		// function is the only identity source for the seven write actions.
		Admission: admissionPort{
			WorkDir:                workDir,
			SupportedSchemaVersion: postgres.SupportedSchemaVersion,
			Database:               databaseConfig(cfg),
			Logger:                 logger,
		},
		// 受控自我进化端口（US-22 §3.1.10）：查询只读、不取实例锁；handoff 先锁后库，
		// 与准入端口同一理由在两条路径都安全带上（每次调用自建句柄，不装配运行期）。
		// The learning port: read-only queries without the instance lock, and a handoff
		// that takes the lock before opening the database. Stateless, hence both paths.
		Learning: learningPort{
			WorkDir:                workDir,
			SupportedSchemaVersion: postgres.SupportedSchemaVersion,
			Database:               databaseConfig(cfg),
			AutoExtract:            learningPortConfig(cfg).AutoExtract,
			Agents:                 learningPortConfig(cfg).Agents,
			Logger:                 logger,
		},
		// Worker 登记行的**只读**列表端口（US-24 §3.1.12）：只开只读会话、不取锁、
		// 不刷新心跳——诊断命令不得替任何人续命。
		// The read-only worker listing: read-only session, no lock, no heartbeat write.
		Workers: workersPort{Database: databaseConfig(cfg)},
		// 资产 bundle 命令面（US-24 §3.1.11）：资产根在这里按 `assets.bundle_root`
		// 解析（相对路径以进程工作目录为基准），install 的共享发布写入在实例锁内完成；
		// 每次调用自建句柄，两条路径都安全带上。
		// The asset surface resolves the bundle root here and publishes under the lock.
		Assets: assetsPort{
			WorkDir:                workDir,
			SupportedSchemaVersion: postgres.SupportedSchemaVersion,
			Database:               databaseConfig(cfg),
			Logger:                 logger,
			Root:                   resolveAssetBundleRoot(cfg, workDir),
			MaxBundleBytes:         resolveMaxBundleBytes(cfg),
		},
		// 分发只读诊断（US-25C §3.1.5）：只开只读会话、不取实例锁、不触发同步。
		// 资产根与 `assets` 命令组同源——诊断看到的树就是安装写的那棵树。
		// The read-only distribution diagnostic shares the asset root with the assets
		// command group; it never takes a lock or triggers a sync.
		AssetStatus: assetStatusPort{
			WorkDir:  workDir,
			Database: databaseConfig(cfg),
			Logger:   logger,
			Root:     resolveAssetBundleRoot(cfg, workDir),
		},
		CLIWriteActor: func() core.Actor { return cliWriteActor(logger) },
	}

	// 装配面在这里构造一次，两条路径共用（重路径接管句柄，只读路径只开一次只读库）。
	// The assembly face is built once and shared by both paths.
	a := &App{
		workDir:          workDir,
		supportedVersion: postgres.SupportedSchemaVersion,
		logger:           logger,
		cfg:              cfg,
		cfgErr:           cfgErr,
		freshInstance:    bootstrap.Fresh,
	}

	// serve 的启动前兜底：工作区骨架先建齐再装配（技术方案 §3.1.1）。幂等——二次启动
	// 逐字节不动既有文件；初始化失败即 1，半建的工作区不进入装配。
	// serve initializes the workspace skeleton before assembly; the call is idempotent.
	if needsServeInit(args) {
		if initErr := ensureWorkspaceForServe(a.wsRoot(), logger); initErr != nil {
			logger.Error("serve 兜底初始化失败", "error", initErr)
			return 1
		}
		// 工作区可写性 fail-closed 检查（US-18 A11）：容器部署时卷属主错误、骨架目录
		// 却已存在，`workspace.Init` 的 mkdir 全部成功——真正的失败会推迟到写配置、
		// 迁移或上传，运维看到的是一个"启动成功、随后以无关形态失败"的实例。这里在
		// 装配前实际写一次再删掉，失败立即退出并点名路径。
		// Probe the workspace before assembly: with a wrong volume owner the skeleton
		// already exists, so init succeeds and the first real write fails much later.
		if probeErr := probeWorkspaceWritable(a.wsRoot()); probeErr != nil {
			logger.Error("serve 启动前工作区不可写，拒绝启动", "error", probeErr)
			return 1
		}
	}

	if needsRuntime(args) {
		// 重路径：固定次序（取锁 → 打开 → 迁移 → 恢复）在 newRuntime 内一次走完，四个次序
		// 读数以 `step` 键落日志（T050 T1 的判据）。运行期角色由子命令判定（D-25C-06）：
		// `serve` = control，`gateway` = worker；**不新增** role 配置键。
		// The heavy path walks its fixed order inside newRuntime and logs the four steps;
		// the role comes from the subcommand alone.
		heavyCommand, _ := commandName(args)
		rt, rtErr := a.newRuntime(ctx, roleForCommand(heavyCommand))
		if rtErr != nil {
			logger.Error("重路径装配失败", "error", rtErr)
			return 1
		}
		if retentionLog != nil {
			files, fileErr := historyretention.OpenFiles(a.wsRoot(), cfg.Observability.Retention.HistoryDays)
			if fileErr != nil {
				logger.Error("history file retention unavailable")
				_ = rt.Close()
				return 1
			}
			defer files.Close()
			retentionLog.bind(files)
			previousDefault := slog.Default()
			slog.SetDefault(logger)
			defer slog.SetDefault(previousDefault)
			rt.observability.Files = files
		}
		deps.RunService = rt.runService()
		deps.SessionLister = rt.sessionLister()
		// 工具清单端口也只在这一次装配之后才有内容（`tool list` 走的就是这条路）。
		// The tool-listing port also only has content after this assembly.
		deps.Tools = rt.tools()
		// secrets 服务面同样只在重路径装配中产出：命令面拿到的是投影端口，不直接
		// 看见存储或解析器。缺件（理论上的装配缺口）交给命令面按「未装配」报错。
		// The secrets service is likewise heavy-path-only; the command face gets the
		// projected port, never the stores or the resolver.
		if rt.secrets != nil {
			deps.Secrets = secretsPort{service: rt.secrets, Logger: logger}
		}
		// serve / gateway 的信号上下文在这里**唯一一次**派生（约束 24）：`cmd.Context()`
		// 即 runCtx，cli 不再自建 NotifyContext——两个信号源并存会让调度器的 watcher
		// 只监听其中一个，停机宽限静默失效。装配阶段仍用原始 ctx（Ctrl-C 保持默认处置）；
		// chat / tool 完全跳过（信号处置与 US-9 一致）。
		// The signal context is derived exactly once here for serve/gateway.
		runCtx := ctx
		if needsSchedules(args) {
			var stopSignals context.CancelFunc
			runCtx, stopSignals = a.signalContext(ctx)
			defer stopSignals()
			// 图 11 的三段预算都从**信号**起算，不是从 Close 起算：HTTP 优雅关闭
			// 花掉的秒数必须计入，否则进程会比配置承诺的 45s 多活一段。
			// All figure-11 budgets count from the signal instant, not from Close.
			rt.shutdown.watchSignal(runCtx)

			// Worker 身份先于一切领取（US-24 图 1）：Register 成功才参与协调，失败即启动
			// 失败。放在调度器 Start 之前——调度器一旦开始领取，进程就必须已经有身份。
			// The worker registers before anything claims work; a failed registration
			// aborts startup (figure 1).
			workerRT, workerErr := a.startWorkerRuntime(runCtx, rt.db, rt.workerIdentity, rt.reclaimer)
			if workerErr != nil {
				code := scheduleStartExitCode(runCtx, workerErr)
				if code == 0 {
					logger.Warn("启动被信号中断", "error", workerErr)
				} else {
					logger.Error("Worker 注册失败", "error", workerErr)
				}
				if closeErr := rt.Close(); closeErr != nil {
					logger.Error("重路径资源释放失败", "error", closeErr)
				}
				return code
			}
			rt.worker = workerRT

			// US-25C §2.3.1：两个后台同步器在调度启动**之前**建好并各跑首轮——领取第一次
			// 触发时，注册集合与四类运行期策略都已是本进程核对过的读数。构造失败是装配
			// 接线缺陷（依赖包由 wire.go 固定），按启动失败处理；首轮运行失败**不**阻断
			// 启动：失败轮不推进策略新鲜度，准入按 POLICY_STALE 失败关闭、下一拍重试即
			// 恢复（§2.3.6），同步的失败项留在工作集里由后台轮次有界自愈（§2.3.4）。
			// The two loopers are built and run their first round before the scheduler
			// starts; a failed first round degrades to the stale-policy gate instead of
			// aborting startup.
			syncLoop, syncBuildErr := NewAssetSync(*rt.assetSyncDeps)
			if syncBuildErr != nil {
				logger.Error("asset sync wiring is invalid", "error", syncBuildErr)
				if closeErr := rt.Close(); closeErr != nil {
					logger.Error("重路径资源释放失败", "error", closeErr)
				}
				return 1
			}
			rt.assetSync = syncLoop
			// 就绪门在这里接上同步器（US-25C ADJ-1）：调度器先于本段构造，盒子是两者
			// 之间唯一的交汇点。必须在 `startSchedules` 之前发布——调度一旦开始领取，
			// 每一拍都要按同一份同步快照既判门又取钉住哈希。
			// Bind the readiness gate before the scheduler starts claiming.
			rt.assetReadyHash.bind(syncLoop)
			rt.policyRefresher = NewPolicyRefresher(*rt.policyRefresherDeps)
			if report, syncRunErr := rt.assetSync.SyncOnce(runCtx); syncRunErr != nil {
				logger.Warn("asset sync first round did not finish",
					"error", syncRunErr, "round", report.Round)
			}
			if _, refreshErr := rt.policyRefresher.RefreshOnce(runCtx); refreshErr != nil {
				logger.Error("runtime policy first refresh failed", "error", refreshErr)
			}
			// 新鲜度门接的是**这一份**刷新器：准入与后台刷新读同一个 `LastVerified`，
			// 两处各建一份会让准入永远看到"从未核对"。
			// Admission reads the very same refresher that keeps refreshing it.
			rt.admission.policyFreshness = rt.policyRefresher

			// 启动失败两层边界（注 16）：存储层致命 → fail closed；runCtx 已被信号取消
			// → 属于"启动被信号打断"，退出码 0（WARN），其余 → 1（ERROR）。判定只调一次。
			// A cancelled run context means the signal interrupted startup (0); anything
			// else is a real failure (1).
			if startErr := startSchedules(runCtx, args, rt.scheduler); startErr != nil {
				code := scheduleStartExitCode(runCtx, startErr)
				if code == 0 {
					logger.Warn("启动被信号中断", "error", startErr)
				} else {
					logger.Error("调度器启动失败", "error", startErr)
				}
				if closeErr := rt.Close(); closeErr != nil {
					logger.Error("重路径资源释放失败", "error", closeErr)
				}
				return code
			}
			// 调度起来之后两个循环才进后台：它们与调度共享 runCtx，停机时先随取消返回，
			// joinWorkers 在关库之前等它们（§2.3.4）。循环另派一层子 ctx：信号只覆盖
			// 信号停机，**启动失败**路径（端口占用、监听启动失败）没有信号，第 [2] 步
			// 「停止领取」经 `stopSyncLoops` 取消它们，否则 join 会等满 worker 宽限。
			// The loops get their own child context so a startup-failure shutdown can stop
			// them without a signal (step [2] cancels it through stopSyncLoops).
			loopCtx, stopLoops := context.WithCancel(runCtx)
			rt.stopSyncLoops = stopLoops
			rt.startSyncLoops(loopCtx, logger)
		}
		if needsServer(args) {
			if flowErr := rt.flows.Start(runCtx); flowErr != nil {
				logger.Error("Flow 启动核对失败", "reason", "flow_recovery_unavailable")
				_ = rt.Close()
				return 1
			}
			// 配置监听在调度器就绪之后、HTTP 开放之前接入（图 A ⑧–⑩）：先接监听再交
			// 端口，反过来的顺序会让"启动后头几秒的修改不生效"成为可复现的缺陷。调度联动
			// 走运行期保留的**同一份**编辑适配器（私有 claim 基线随热加载增减，约束 39）。
			// The config watcher starts after the scheduler and before the HTTP face opens;
			// the scheduler seam is the very adapter retained on the runtime.
			watcher, watchErr := startServeWatcher(runCtx, rt.root, rt.agents, rt.loader, rt.skills, rt.versions, rt.collections, rt.scheduleEditor, logger)
			if watchErr != nil {
				logger.Error("配置监听启动失败", "error", watchErr)
				if closeErr := rt.Close(); closeErr != nil {
					logger.Error("重路径资源释放失败", "error", closeErr)
				}
				return 1
			}
			rt.watch = watcher

			// HTTP 面只在 serve 上装配：端口背后是同一份重路径装配态（技术方案 §6.5）。
			// The HTTP face is assembled only for serve, over the same runtime state.
			srv, srvErr := a.newWebServer(rt)
			if srvErr != nil {
				logger.Error("HTTP 面装配失败", "error", srvErr)
				// 服务没交出去也要释放：此刻锁已取、库已开（约束 20 ⑤）。
				// Release even on this early exit: the lock and database are already held.
				if closeErr := rt.Close(); closeErr != nil {
					logger.Error("重路径资源释放失败", "error", closeErr)
				}
				return 1
			}
			deps.Server = srv
		}
		// 释放挂在唯一出口上且**在 exec 之后**：Serve 阻塞期间锁与库必须活着。
		// Release hangs off this one exit point and runs after exec returns.
		if execErr := runWithRuntime(rt, logger, func() error {
			return executeRoot(runCtx, args, deps)
		}); execErr != nil {
			logger.Error("命令执行失败", "error", execErr)
			// 四档退出码只对显式声明者生效（§3.1.8b / §3.1.8c）；其余命令仍是 1。
			// Only errors that declare a status deviate from the historical 1.
			return cli.ExitStatusOf(execErr)
		}
		return 0
	} else {
		// 轻路径才注入主密钥预检端口（US-25 §3.1.4）：`secrets check-key` 只读文件与
		// 直接父目录，重路径上没有任何调用者——把它放进重路径装配会诱导后来者"顺手"
		// 依赖已开好的库或锁，而那正是预检要避免的耦合。
		// The key preflight is wired on the light path only; the heavy path has no caller.
		deps.MasterKeyCheck = masterKeyCheckPort{}
		if needsReadStore(args) {
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
			deps.SessionLister = sessionListerPort{store: postgres.NewSessionStore(read)}
		}
	}

	if err := executeRoot(ctx, args, deps); err != nil {
		// cobra 已把可读文案写到标准错误；这里补结构化日志，失败路径两轨都在。
		logger.Error("命令执行失败", "error", err)
		return cli.ExitStatusOf(err)
	}
	return 0
}

// executeRoot 组装根命令并执行一次——重 / 轻两条分流共用这一处执行面。
// executeRoot builds the root command and executes it once; both splits share it.
func executeRoot(ctx context.Context, args []string, deps cli.Deps) error {
	root := cli.NewRootCommand(deps)
	root.Version = Version
	root.SetArgs(args)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	return root.ExecuteContext(ctx)
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
	if !ok || !slices.Contains(heavyCommands, name) {
		return false
	}
	// `secrets` 组整体重，但两条离线命令必须绕开装配（见 offlineSecretsCommands）：
	// 判据落在 `secrets` 之后的**下一个非选项参数**上；没有子命令时仍是重命令。
	// The group is heavy, but its two offline commands opt out by their subcommand name.
	if name == "secrets" {
		if sub, found := nextPositional(args, name); found && slices.Contains(offlineSecretsCommands, sub) {
			return false
		}
	}
	return true
}

// nextPositional 取 `command` 之后的下一个非选项参数（子命令名）。`command` 之前的选项与
// 它们的值不参与判定：本项目没有带值的全局选项（needsRuntime 的注释同口径）。
// nextPositional returns the first non-option argument after the given command name.
func nextPositional(args []string, command string) (string, bool) {
	seen := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if !seen {
			seen = arg == command
			continue
		}
		return arg, true
	}
	return "", false
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

// needsServer 判定这次调用是否需要 HTTP 面：目前只有 `serve`；`gateway` 只常驻等信号，
// 不启 HTTP（技术方案 §6.5）。
// needsServer reports whether the call needs the HTTP face; only serve does today.
func needsServer(args []string) bool {
	name, ok := commandName(args)
	return ok && name == "serve"
}

// signalContext 派生 serve / gateway 的**唯一信号上下文**（约束 24）：`App.notifyContext`
// 非 nil 时用它（用例注入可取消 ctx 的同形函数，不发真实信号），nil 时取标准库
// `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`。取消只表示"收到停机信号"，
// 停止宽限由调度器内部按 70s 有界等待实现（约束 24），不在这里做另一套计时。
// signalContext derives the single signal context for serve/gateway; the notifier seam
// lets tests drive cancellation without sending real process signals.
func (a *App) signalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	notify := a.notifyContext
	if notify == nil {
		notify = signal.NotifyContext
	}
	return notify(ctx, os.Interrupt, syscall.SIGTERM)
}

// needsServeInit 判定这次调用是否要在装配之前做幂等工作区初始化：只有 `serve`——它要
// 建库建工作区后才装配；`gateway` 不碰 HTTP 面，也不在此初始化。
// needsServeInit reports whether the workspace is initialized before assembly; only
// serve does, gateway merely parks until a signal arrives.
func needsServeInit(args []string) bool {
	name, ok := commandName(args)
	return ok && name == "serve"
}

// probeWorkspaceWritable 是 serve 的写能力探针：在工作区根目录实际创建并删除一个
// 临时文件。目录存在不等于可写（属主 / 只读挂载），路径从错误里原样带出。
// probeWorkspaceWritable writes and removes one temp file: existing directories are
// not necessarily writable, and the path travels back in the error.
func probeWorkspaceWritable(root string) error {
	file, err := os.CreateTemp(root, ".basalt-write-probe-*")
	if err != nil {
		return fmt.Errorf("工作区不可写 %s: %w", root, err)
	}
	name := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(name)
	return errors.Join(closeErr, removeErr)
}

// runtimeCloser 是 runWithRuntime 需要的最小释放面（生产传 *heavyRuntime）。
// runtimeCloser is the minimal release surface runWithRuntime needs.
type runtimeCloser interface {
	Close() error
}

// runWithRuntime 在 exec 返回之后释放重路径资源（约束 20 ⑤ / 回归 R17）：Serve 阻塞
// 期间锁、库与 MCP 连接必须活着，释放**不得**早于 exec；正常返回与报错都恰好释放一次。
// runWithRuntime releases the runtime after exec returns — never before, exactly once,
// on both the success and error paths.
func runWithRuntime(rt runtimeCloser, logger *slog.Logger, exec func() error) error {
	defer func() {
		if closeErr := rt.Close(); closeErr != nil {
			logger.Error("重路径资源释放失败", "error", closeErr)
		}
	}()
	return exec()
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
