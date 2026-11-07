package cli

import (
	"log/slog"
	"os"

	"github.com/ymedword/basalt/internal/core"
)

// Deps 是命令面的显式类型化装配参数——工作目录、目标 schema 版本与日志器一律
// 经参数传递，不得塞进 context、不得用包级变量顶替（宪法第 7 条）。
// Deps carries the explicitly typed wiring parameters for the command surface:
// the working directory, the target schema version and the logger. None of them
// travel through the context or through package-level state.
//
// WorkDir 由装配层注入而非各命令自行 os.Getwd——测试因此免于 chdir，
// status 的三节也共享同一路径（一份报告只检查一个工作区）。
// WorkDir is injected by the assembly layer instead of each command calling
// os.Getwd, which keeps tests free of chdir and pins every status section to the
// same workspace.
type Deps struct {
	// IsolationProbe 是不触数据库的隔离运行时自检端口。
	// IsolationProbe checks isolation runtimes without database access.
	IsolationProbe IsolationProbe
	// WorkDir 是运行命令的进程工作目录；工作区 = <WorkDir>/.basalt。
	// WorkDir is the process working directory; the workspace is <WorkDir>/.basalt.
	WorkDir string

	// SupportedSchemaVersion 是**运行期谱系（PostgreSQL）**的目标 schema 版本；生产
	// 装配传 postgres.SupportedSchemaVersion，测试传合成值以覆盖单版本期不可达的分支。
	// SupportedSchemaVersion is the runtime (PostgreSQL) lineage's target version.
	SupportedSchemaVersion int

	// SourceSchemaVersion 是**SQLite 源谱系**的目标版本，只由 `init` 的引导建库使用。
	// 两个谱系版本是不同常数（源 v6 / 运行期 v1）：混用会让新建工作区得到一份
	// "v6 schema、v1 版本行"的库，`db export` 随即以"源库是 v1"拒绝（T078 演练实测）。
	// 生产装配传 storage.SupportedSchemaVersion。
	// SourceSchemaVersion is the SQLite source lineage's target version, used only by
	// init; swapping it with the runtime lineage's version stamps v1 onto a v6 schema.
	SourceSchemaVersion int

	// Logger 是结构化日志器；测试注入带缓冲 handler 的实例以断言凭证不泄露与
	// 失败路径留痕。
	// Logger is the structured logger; tests inject a buffered handler to assert
	// that credentials never leak and that failure paths leave a trace.
	Logger *slog.Logger

	// Store 与 Workspace 是命令面的两个端口（接口定义在消费方，见 ports.go）：
	// 存储器与工作区的**实现**由装配层 internal/app 注入，命令面只认接口——
	// cli 因此不 import storage / workspace，依赖方向是 app → cli → core。
	// The two ports the commands consume; internal/app injects the adapters, so
	// cli never imports the implementation packages.
	Store     Store
	Workspace Workspace

	// AcquireWorkspaceLock 为离线素材安装取原实例锁；缺失时拒绝写入。
	// AcquireWorkspaceLock injects the original lock without importing its implementation.
	AcquireWorkspaceLock func(root string) (release func() error, err error)

	// Agents 是 Agent 目录端口：list / show 只读（轻路径——不取实例锁、不触数据库），
	// create / delete 是**写模式**（实现侧先取实例锁：被活实例持有时拒绝启动且零写入）。
	// Agents is the agent-directory port: list / show stay read-only, while create /
	// delete write through the instance lock taken inside the implementation.
	Agents Agents

	// RunService 与 SessionLister 是运行面端口（见 ports.go）：`chat` 走前者跑一次运行、
	// 经后者读回本轮新增的消息，`session list` 只用后者。
	// The run-face ports: chat runs through RunService and reads history through
	// SessionLister; session list consumes SessionLister only.
	RunService    RunService
	SessionLister SessionLister

	// Server 是 HTTP 服务面端口（见 ports.go）：只有 `serve` 消费，且只由重路径注入——
	// `gateway` / `chat` 拿到 nil 是正确的形态（前者不启 HTTP，后者不占端口）。
	// The HTTP service port: only serve consumes it, and only the heavy path injects it.
	Server Server

	// Secrets 是主密钥与托管凭据运维的端口（见 ports.go）：只有重路径装配才产出该服务
	// （它要库、撤销编排与解析器），轻路径拿到 nil 即报缺件，不自造第二套读面。
	// Secrets is only produced by the heavy-path assembly; a nil here is a wiring gap.
	Secrets Secrets

	// DatabaseMigrator 是 PostgreSQL 迁移端口（见 ports.go）：`db migrate` 经它建库 /
	// 前向迁移。装配层常驻注入（它与库、锁、运行期资源都无关，轻路径也能安全带上）；
	// 拿到 nil 即报缺件——命令面**不得**自己直连数据库补出第二条迁移路径。
	// The PostgreSQL migration port, injected on every path; a nil is a wiring gap and
	// never a reason for the command surface to open a database by itself.
	DatabaseMigrator DatabaseMigrator

	// Migration 是 `db export / import / verify / switch` 的端口（见 ports.go）。它与
	// DatabaseMigrator 分开：迁移只碰 PG，导出还要读 SQLite 快照与资产树，两者的依赖面
	// 不同；合成一个接口会让"只想跑 db migrate"的装配被迫带上整个迁移链路。
	// 装配层**总是**注入（与 DatabaseMigrator 同理：导出不需要库，导入才需要）。
	// Migration is the port behind export/import/verify/switch, kept apart from the
	// migrator because the two have different dependency surfaces.
	Migration DatabaseMigration

	// InstanceAdopt 是 `instance adopt` 的端口（见 ports.go）。它与 Migration 分开：
	// 采纳只碰旧 SQLite 工作区与名册，零外呼、不装配运行期；合成一个接口会让"只想采纳
	// 身份"的路径被迫带上 PG 迁移链路。装配层总常驻注入（它不依赖库与运行期资源）。
	// InstanceAdopt sits beside Migration: adoption touches only the legacy SQLite
	// workspace and the roster, so it must not drag the PG migration chain along.
	InstanceAdopt InstanceAdoption

	// SecretsRecovery 与 LegacySecretsAdopt 是两条**离线**手工流程的端口（见 ports.go）：
	// `secrets recover-record`（D-40 记录恢复）与 `secrets adopt-legacy`（schema 3～5 受控
	// 升级）。它们与 Secrets 分开是正确的形态——离线流程不许经过重路径装配（那会迁移库、
	// 自动 adopt 并启动全部已发布 MCP）；装配层常驻注入，拿到 nil 即报缺件。
	// The two offline manual procedures: share no code path with the heavy runtime.
	SecretsRecovery    SecretsRecovery
	LegacySecretsAdopt LegacySecretsAdopt

	// Handoff 是 `db verify --handoff` 的端口（见 ports.go）：只读核对迁移准备副本的
	// 交接就绪，离线可用、不要求 DSN。它与 Migration 分开——交接核对只碰副本的
	// SQLite 库、名册、密钥文件与同级配置，零外呼、不装配运行期；装配层常驻注入。
	// The offline handoff gate sits beside Migration so a read-only roster/key/ciphertext
	// check never drags the PG migration chain along.
	Handoff HandoffVerification

	// Flow is a stateless service client and offline loader, available on the
	// light path; none of its commands assemble a second runtime or acquire a lock.
	Flow FlowClient

	// Tools 是工具注册池端口（见 ports.go）：`tool list` 经它取「注册了什么」这一份数据，
	// 清单的顺序由实现侧给出、命令面零重排。只有重路径装配才构建过注册池，因此该端口
	// **只在重路径上非空**——命令面拿到 nil 即报缺件，不自造第二份清单。
	// The tool-registry port: only the heavy path ever builds the pool, so a nil here
	// means a wiring gap and is reported as such.
	Tools ToolLister

	// Admission 是学习准入端口（见 ports.go）：propose / evaluate 等写命令经它冻结
	// 候选并预写管理审计；list / show 只读。命令面拿到 nil 即报缺件——**不得**自己
	// 打开数据库补出第二条准入读写路径。
	// The admission port; a nil here is a wiring gap, never a reason to open a second
	// database path from the command surface.
	Admission AdmissionPort

	// Learning 是受控自我进化端口（见 ports.go）：status / extractions / drafts 只读，
	// handoff 是唯一写适配（实例锁归实现侧持有，命令面不重复取锁）。拿到 nil 即报
	// 缺件——**不得**自行拼装存储或另起一条抽取 / 审计路径。
	// The learning port; its handoff adapter owns the instance lock, so the command
	// surface never takes a second one. A nil here is a wiring gap.
	Learning LearningPort

	// Workers 是 Worker 登记行的**只读**端口（见 ports.go）：`workers list` 经它取
	// 实例 / 名称 / 启动 / 心跳 / 活跃租约数，不取实例锁、不写任何行。拿到 nil 即报
	// 缺件——命令面**不得**自己打开数据库补出第二条读面。
	// The read-only worker listing port; a nil here is a wiring gap.
	Workers WorkerLister

	// Assets 是资产 bundle 命令面的装配（见 ports.go）：资产根由装配层解析，
	// `assets install` 的共享发布记录写入由 Publish 提供。nil 即报缺件；
	// install 缺 Publish 时失败关闭（**不得**退化成只装本地）。
	// The asset command surface; a nil port is a wiring gap and install fails closed.
	Assets AssetsPort

	// CLIWriteActor 给出**本机 CLI 写动作**的操作主体（`cli:<OS user>`）。它是七个
	// 准入写动作的唯一身份来源：命令面没有 `--approver` 之类旁路，缺失即报缺件；
	// `cli:unknown` 在取锁与开库之前被拒绝（颗粒度文档 §3.1.10）。
	// CLIWriteActor is the sole identity source for the admission write commands; there
	// is no --approver-style bypass, and cli:unknown is rejected before the lock or any
	// database is opened.
	CLIWriteActor func() core.Actor

	// In / Out 是命令面应当读写的**介质**（真控制台或管道——分流由渠道层按句柄判定，
	// 不看环境变量）；Signals 是中断来源。三者为空即用进程标准流与渠道自建的中断源。
	// 注入是为了可判卷：真控制台与真 Ctrl-C 只能在人工项里按，自动化这一侧拿得住的
	// 只有一对管道和一条信号通道（颗粒度文档 §6.1 第 4 项）。
	// In/Out/Signals are the media and the interrupt source; nil falls back to the
	// process standard streams and a channel-owned signal source. Injecting them is
	// what makes the interactive path testable at all.
	In      *os.File
	Out     *os.File
	Signals <-chan os.Signal
}
