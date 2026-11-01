package cli

import (
	"context"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是命令面的**端口**（依赖倒置的接口面）：接口定义在消费方，实现由装配层
// internal/app 注入。cli 因而不 import storage / workspace——依赖方向保持
// app → cli → core，而"没有编译环"**不等于**方向合规（环只是最粗的形态；实测：
// 直接引用实现包同样让 cli 依赖具体实现，装配层就失去了替换与测试注入的接缝）。
// The command surface's ports: interfaces are defined by their consumer and the
// adapters are injected by internal/app, so cli never imports storage or
// workspace. Acyclicity alone is not the criterion — cli must not depend on the
// concrete implementations either.

// Store 是命令面对持久化基座的端口（init 引导 / status 只读诊断）。
// Store is the command surface's persistence port.
type Store interface {
	// Bootstrap 引导创建库（幂等）：created 为 true 仅当**本进程实际创建**了
	// schema——已存在的当前版本库一律 Kept 且**零写**。
	// Bootstrap provisions the database; created is true only for the process
	// that actually created the schema, and Kept means zero bytes written.
	Bootstrap(ctx context.Context, path string, target int) (created bool, err error)

	// OpenReadOnly 只读打开运行期后端（PostgreSQL）：只校验、不建库、不迁移、不取锁、
	// 不恢复，会话由服务端固定为只读事务。连接失败按打开失败回报；版本矩阵由调用方
	// 用返回视图的版本对照 Deps.SupportedSchemaVersion 判定（未初始化 / 损坏经
	// core 哨兵错误回报）。
	// OpenReadOnly opens the runtime backend read-only; the server pins the session
	// read-only, and the caller judges the version matrix against the target.
	OpenReadOnly(ctx context.Context, database DatabaseConfig) (SchemaView, error)
}

// SchemaView 是只读打开的库视图——status 节 3 的只读消费面（版本 + 表清点）。
// SchemaView is the read-only database view section 3 of status consumes.
//
// 返回接口而非具体类型：接口的**返回类型也是依赖面**，返回 *storage.DB 等于把
// cli 钉回实现包，注入接缝随即消失。
// Returning an interface matters: a concrete return type would pin cli back to
// the implementation package and dissolve the seam.
type SchemaView interface {
	SchemaVersion(ctx context.Context) (int, error)
	TableNames(ctx context.Context) ([]string, error)
	Close() error
}

// Workspace 是命令面的工作区端口：骨架幂等创建（init）与骨架清点（status 节 1）。
// Workspace is the command surface's workspace port.
//
// 返回形态刻意收成**裸切片**而不用实现包的报告类型——报告类型同样是依赖面，
// 直接使用它等于绕开端口把 cli 绑回 workspace 包。
// Plain slices rather than the implementation's report structs: those types are
// part of the dependency surface too.
type Workspace interface {
	// Init 幂等创建骨架：created / kept 分别是"本进程新建"与"打开时已存在、
	// 逐字节未动"的条目清单。
	// Init creates the skeleton idempotently; a second run reports empty created.
	Init(root string) (created, kept []string, err error)

	// Inspect 清点骨架：missing 为空即齐备；未初始化（连工作区目录都没有）时
	// 返回 core.ErrWorkspaceNotInitialized。
	// Inspect inventories the skeleton; an empty missing means it is complete.
	Inspect(root string) (missing []string, err error)
}

// Agents 是命令面对 Agent 目录的端口：list / show 读（轻路径），create / delete 写
// （写模式，取实例锁）。
// Agents is the command surface's port onto the agent directories: list / show read
// (the light path) while create / delete write (holding the instance lock).
//
// providers 是注册判定的**显式输入**（命令面经 LoadConfig 取得——§2.4 注 2）；
// 适配器不另读配置——注册判定与装载因此共用同一条代码路径（FR-012）。
// providers is passed in explicitly rather than re-read inside the adapter, so the
// registration verdict and the loader share one code path.
//
// Delete 的 now 由命令层传入（归档名含时间戳——时间显式传入是可判卷前提，§2.4 注 11）。
// Delete receives the clock from the command layer because the archive name carries
// a timestamp and an injected clock is what makes the archive testable.
type Agents interface {
	// List 装载全部 Agent 目录：注册成功与失败**都有一条**（失败不静默消失）。
	// List loads every agent directory; rejects are reported, never dropped.
	List(root string, providers []string) ([]core.AgentEntry, error)

	// Create 写入最小 AGENT.md 模板并返回本次新落盘的相对路径（实现侧先取实例锁）。
	// Create writes a minimal template and returns the paths it created.
	Create(root, name string, providers []string) (created []string, err error)

	// Delete 把 Agent 目录归档进 archive/<name>-<时间戳>/，返回归档目标的相对路径
	// （实现侧先取实例锁）。
	// Delete archives the agent directory and returns the archive path.
	Delete(root, name string, now time.Time) (archived string, err error)
}

// RunService 是命令面对**唯一运行入口**的端口（`chat` 消费，适配器 ← `core.AgentService`）。
// RunService is the command surface's port onto the single run entry point.
//
// 返回语义沿用契约 §1.2 两层分离：**调用方问题**（请求非法 / Agent 不存在 / 同会话忙 /
// 实例数超限）走 error，**运行期失败**在 `RunResult.Status` 里——命令面据此分工：
// 前者终止会话并以非零退出码收场，后者按 §6.2 固定口径 4 渲染状态行。
// Caller-side problems travel as errors; run-time failures ride in the status.
type RunService interface {
	// Run 执行一次运行：准入、装配、循环、收尾全在这一层之内。
	// Run performs one run: admission, wiring, loop and finish.
	Run(ctx context.Context, req core.RunRequest) (core.RunResult, error)
}

// SessionLister 是命令面对会话仓储的**只读**端口（`session list` 与 `chat` 消费，
// 适配器 ← `storage.SessionStore`）。
// SessionLister is the command surface's read-only port onto the session store.
//
// 为什么 chat 也用它——端口是按**消费面**划的，不是按命令划的：`RunResult` 只有工具
// 调用的**计数**（`ToolCalls int`，没有名字），而渲染口径要求逐条打出
// `⚙ 调用工具 <name>…` 与"已得部分结果"（§6.2 固定口径 4）。同步运行模型下（调用方
// 阻塞等 `Run` 返回、全仓无进度回调），**消息历史是唯一能取到这两样的读面**：每次运行
// 返回后按位置增量确认本轮新增的消息，工具调用提示行取自其中的助手消息、部分结果取自
// 最后一条非空助手正文。**只读**——命令面从不经此端口写历史（追加唯一路径在 core）。
// chat reads the very same port: RunResult carries a tool-call count but no names, and
// under the synchronous run model the message history is the only read face that has
// them. The port stays read-only; appending remains core's single path.
type SessionLister interface {
	// ListSessions 列出全部会话（排序与去重由实现侧负责——命令面保序渲染、不重排）。
	// ListSessions lists every session; the command surface preserves the order.
	ListSessions(ctx context.Context) ([]core.SessionRecord, error)

	// Messages 读一个会话的完整历史（按 seq 升序；未知会话返回空切片而非错误）。
	// Messages reads one session's history in seq order; an unknown session is empty.
	Messages(ctx context.Context, sessionID string) ([]core.Message, error)
}

// Server 是命令面对 HTTP 服务面的端口（`serve` 消费，适配器 ← `web.Server`）。
// Server is the command surface's port onto the HTTP service face.
//
// 只声明一条 Run：监听地址、路由、中间件与五阶段停机全部属于实现侧——cli 因此
// **不 import internal/web**（约束 32），`gateway` / `chat` 也从来看不到这个端口。
// One method is enough: listening, routing, middleware and the shutdown protocol all
// belong to the implementation, so cli never imports internal/web.
type Server interface {
	// Run 监听并服务，直到 ctx 取消后按五阶段协议收尾。
	// Run serves until ctx is cancelled, then drains per the five-phase protocol.
	Run(ctx context.Context) error
}

// SecretsStatus 是 `secrets init` / `secrets status` 的只读结果：**不含**密钥、密文或
// 任何可用秘密。指纹是主密钥的公开比对值（供运维核对"当前生效的是哪把钥匙"），
// 与密钥材料不可互推。类型定义在消费方，装配层负责投影（见 internal/app/ports.go）。
// SecretsStatus is the read-only secrets result; the fingerprint is a public identifier,
// never key material.
type SecretsStatus struct {
	// Initialized 表示主密钥文件已就位并可装载。
	// Initialized reports whether the master key is present and loadable.
	Initialized bool

	// State / Reason 是实例加密三态（uninitialized / ready / recovery_required）与
	// 恢复细分原因（missing / corrupt / key_id_mismatch / unprovable_provenance /
	// unavailable）；空串表示"未判定"。命令面据此打印恢复态，不把它说成"未初始化"。
	// State / Reason carry the three-state verdict and its recovery reason.
	State  string
	Reason string

	// KeyID / Fingerprint 是主密钥的公开标识与比对值。
	// KeyID / Fingerprint are the key's public identifiers.
	KeyID       string
	Fingerprint string

	// ManagedModelConnections / ManagedNotifyChannels 是"当前托管依赖"计数——它们同时
	// 是 `secrets init` 的拒绝条件（新建密钥会让既有密文变成孤儿）。
	// The managed-dependency counts are also the refusal conditions of secrets init.
	ManagedModelConnections int
	ManagedNotifyChannels   int
}

// SecretsVerifyReport 是 `secrets verify` 的分类计数：两组计数对应"历史密文完整性"
// 与"当前可用性"两个报告对象，命令面按非零项打印明细；材料（明文 / 密文 / 密钥）
// 一律不出现，逐条原因只在结构化日志里（WARN 点名资源与凭据 ID）。
// SecretsVerifyReport counts the two report objects; the command surface prints the
// non-zero categories and the per-row reasons stay in the structured log.
type SecretsVerifyReport struct {
	// (a) 历史密文完整性（逐版本行）。
	Checked               int
	Failed                int
	HistoricalOK          int
	Revoked               int
	RevokedWithCiphertext int
	MissingCiphertext     int
	KeyIDMismatch         int
	AADMismatch           int

	// (b) 当前可用性（逐 active 身份）。
	CurrentChecked int
	CurrentFailed  int
	TargetMismatch int
	BindingMissing int
	KeyRevoked     int
	EnvMissing     int
}

// Secrets 是命令面对主密钥与托管凭据运维动作的端口（`secrets` 命令组消费，适配器 ←
// 装配层的 secrets 服务）。四个动作都不返回密钥材料；`revoke-managed` 也不需要解密。
// Secrets is the command surface's port onto the secrets maintenance actions.
type Secrets interface {
	// Init 生成主密钥并落盘；文件已存在或存在当前托管依赖时拒绝（不提供自动轮换）。
	// Init mints the master key; an existing file or a live managed dependency refuses.
	Init(ctx context.Context) (SecretsStatus, error)

	// Status 只读报告主密钥是否就绪与托管依赖计数。
	// Status reports key readiness and managed-dependency counts.
	Status(ctx context.Context) (SecretsStatus, error)

	// Verify 校验历史密文完整性与当前 active 凭据的可用性，只回分类计数。
	// Verify checks history integrity and current usability; counts only.
	Verify(ctx context.Context) (SecretsVerifyReport, error)

	// RevokeManaged 撤销全部托管凭据（服务停止时的丢失恢复）：清空密文、升纪元、
	// 写操作行。返回本次实际撤销的条数。
	// RevokeManaged revokes every managed credential and returns how many changed.
	RevokeManaged(ctx context.Context) (int, error)
}

// ToolLister 是命令面对**工具注册池**的端口（`tool list` 消费，适配器 ← `tool.Registry`）。
// ToolLister is the command surface's port onto the tool registry.
//
// 只给一个方法：命令面要的是「注册了什么」这一份**数据**（`core.ToolSpec` 是 core 的
// 类型，不是工具实现包的类型——这正是本端口不引入实现依赖的原因）。清单的**顺序由实现侧
// 给定**（注册池按名字排序导出），命令面保序渲染、**不得**重排（口径 13）；工具池是装配期
// 构建的内存对象，所以 `tool list` 属**重路径**——清单只在那次装配之后才存在。
// One method is enough: the command surface wants the data, and the order is the
// implementation's to give. The pool lives in memory, built at assembly time — hence
// the heavy path.
type ToolLister interface {
	// Tools 返回注册池的清单（顺序稳定、由实现侧负责——命令面逐行渲染、零重排）。
	// Tools returns the registry's specs; the implementation owns the order.
	Tools() []core.ToolSpec
}

// DatabaseMigrator 是命令面对 PostgreSQL 谱系的迁移端口（`db migrate` 消费，适配器 ←
// 装配层的 `internal/storage/postgres`）。cli 因此**不 import** postgres：命令面从已装载
// 配置里取出 `database` 段显式传入，连接池上限、三个超时与 DSN 的语义解释全部留在实现侧。
// The migration port onto the PostgreSQL lineage: the command surface passes the loaded
// database section in explicitly and never imports the implementation package.
//
// 端口只回"已就绪的版本号"：版本判定、advisory 锁与失败整体回滚属于实现（设计 §3.1.1），
// 命令面不重复判。**没有**"跳过迁移直接服务"的分支——迁移失败原样上抛，`db migrate`
// 以非零退出收场（服务启动路径同源：`serve` 经 `Open` 走同一条 `ensureSchema`）。
// Only the ready version comes back; a failed migration propagates and exits non-zero.
type DatabaseMigrator interface {
	// Migrate 连接配置的 PostgreSQL，校验扩展能力并前向迁移到当前支持版本；返回版本号。
	// Migrate opens the configured PostgreSQL, verifies extension capabilities, brings
	// the schema forward and returns the ready version.
	Migrate(ctx context.Context, database DatabaseConfig) (schemaVersion int, err error)
}

// ─────────────────────────── db export / seal / import / verify / switch ───────────────────────────
//
// 下面六组请求 / 报告与上一条 DatabaseMigrator 同一端口纪律：**返回类型也是依赖面**，
// 所以这里不出现 `dbmigrate.Manifest` / `AssetManifest` / `VerifyReport`——命令面只拿到
// 它要打印的那几项，实现侧的类型不出装配层。请求类型同理：cli 只说"从哪个目录导到
// 哪个目录"，快照、批次号、三元组绑定与逐表摘要全部留在 `internal/dbmigrate`。
// The six request/report families below obey the same rule as every other port: return
// types are part of the dependency surface, so no dbmigrate type appears here.

// MigrationExportRequest 是 `db export --out` 的输入：源工作区（SQLite v6 根）与输出目录。
// MigrationExportRequest names the SQLite v6 workspace and the package directory.
type MigrationExportRequest struct {
	// Workspace 是**工作区根**（`<WorkDir>/.basalt`），不是 WorkDir 本身：名册、库与
	// 资产树都在这层之下。
	// Workspace is the workspace root, not the working directory.
	Workspace string

	// Out 是要产出的迁移包目录；必须先不存在或为空的导出目录，且与 Workspace 互不包含。
	// Out is the package directory; it must not overlap the workspace.
	Out string
}

// MigrationExportReport 是导出结果：批次号、实例标识、源快照摘要（三元组的三个值）与
// 资产盘点摘要。`Deliverable=false` 表示本次只产出了受限核查包（`ReviewStatus=pending`），
// 命令面**必须**据此打印"不可交付"，不得让运维把核查包当成迁移包。
// MigrationExportReport carries the binding triple and the asset summary; Deliverable is
// false for a review-only package, which must never be presented as shippable.
type MigrationExportReport struct {
	OutDir                 string
	MigrationBatchID       string
	SourceInstanceID       string
	SourceInstanceIDOrigin string
	SourceSnapshotSHA256   string
	EntryCount             int
	UnclassifiedCount      int
	UnclassifiedDigest     string
	// ContentHints 是未归类文件的启发式扫描结果：规则 ID → 命中文件数（C-49）。
	// 它只提示，**不**构成"包内无秘密"的证明，也**不**参与任何放行 / 删除判断；
	// 空 map 表示没有可扫描的未归类文件，不表示安全。
	// ContentHints maps rule id to matching-file count for unclassified files only.
	ContentHints map[string]int
	ReviewStatus string
	Agents       []string
	Deliverable  bool
}

// MigrationSealRequest 是 `db export --seal` 的输入：待封包的导出目录与逐条处置文件。
// `ReviewFile` 可以留空——**仅当**清单没有未归类文件（`not_required`）时成立；有待核查
// 条目却没有处置文件会被拒绝，实现侧**不得**按"全 include"补默认值。
// MigrationSealRequest seals an export directory; an empty ReviewFile is only acceptable
// when the manifest has nothing to review.
type MigrationSealRequest struct {
	Dir        string
	ReviewFile string
}

// MigrationSealReport 是封包结果：最终核查状态与三类处置条数（进包 / 排除 / 移交）。
// MigrationSealReport reports the sealed status and the three disposition counts.
type MigrationSealReport struct {
	Dir          string
	ReviewStatus string
	Included     int
	Excluded     int
	Handoff      int
	ReviewDigest string
	Reviewer     string
}

// MigrationImportRequest 是 `db import` 的输入：一份已复核的迁移包目录。
// MigrationImportRequest names one verified package directory.
type MigrationImportRequest struct {
	Dump string
}

// MigrationTableRows 是导入报告的一行：表名与提交后确认的行数。
// MigrationTableRows is one row of the import report.
type MigrationTableRows struct {
	Table string
	Rows  int64
}

// MigrationImportReport 是导入结果：逐表行数（按表名升序，命令面保序打印）与合计。
// `Tables` 只在事务提交确认后才有内容。
// MigrationImportReport lists per-table counts in a stable order.
type MigrationImportReport struct {
	Tables    []MigrationTableRows
	TotalRows int64
	Duration  time.Duration
}

// MigrationVerifyRequest 是 `db verify` 的输入：迁移包目录 + 是否复核目标库。
// `Dump` **总是**必填——`--target` 也要它：目标库复核按清单的列元数据回读，没有清单
// 就没有比对基准（`--target` 因此是 `--dump` 的修饰符，不是与它并列的独立模式）。
// MigrationVerifyRequest always carries the package directory; --target merely adds the
// target-side check, because the manifest is its only comparison baseline.
type MigrationVerifyRequest struct {
	Dump   string
	Target bool
}

// MigrationTableCheck 是一张表的复核读数：两侧行数 + 行摘要是否相符。
// MigrationTableCheck is one table's readings from a verify pass.
type MigrationTableCheck struct {
	Table       string
	SourceRows  int64
	TargetRows  int64
	RowDigestOK bool
}

// MigrationVerifyReport 是复核结果。`Mode` 固定为 `dump` / `target`；差异项分开列，
// 命令面逐条打印、**不得**只报一句"复核失败"。
// MigrationVerifyReport keeps each kind of difference apart so the command surface can
// print them one by one.
type MigrationVerifyReport struct {
	Mode              string
	OK                bool
	Tables            []MigrationTableCheck
	MissingIDs        []string
	ExtraIDs          []string
	ReferenceFailures []string
	Notes             []string
	Duration          time.Duration
}

// MigrationSwitchRequest 是 `db switch` 的输入：迁移包目录与源工作区。
// `Workspace` 用于切换前重盘（源端自快照以来是否漂移）；**必填**——留空即拒绝
// （FR-092：跳过重盘会让 source_changed_since_snapshot fail-open），不存在"留空
// 表示不重盘"的调用形态。
// MigrationSwitchRequest carries the package and the mandatory source workspace used
// for the pre-switch re-inventory; an empty workspace is refused (FR-092).
type MigrationSwitchRequest struct {
	Dump      string
	Workspace string
}

// MigrationSwitchReport 是切换事实：cutover 行本身。
// `RollbackWindowOpen` 恒为 true（`first_write_at` 留 NULL）——切换刚写完时窗口一定是
// 开着的，它不是"可以回退"的承诺，只说明首次业务写入尚未登记。
// MigrationSwitchReport is the cutover row; an open rollback window is not a promise of
// reversibility, only the fact that the first business write is still unrecorded.
type MigrationSwitchReport struct {
	CutoverID           string
	CutoverAt           time.Time
	ManifestSHA256      string
	SourceSchemaVersion int
	RollbackWindowOpen  bool
	// Memory 是切换时旧记忆一次性只读导入的逐条对账（D-18 / D-22）。`Total == 0`
	// **不得**读作"迁移成功"：它只说明源里没有可导入的旧记忆条目，渲染层必须把这句话
	// 原样说出来。冲突 / 不可读非空时切换根本不成立（实现侧已拒绝），报告不会走到这里。
	// Memory is the legacy memory reconciliation; an empty source is stated, never
	// presented as a successful migration.
	Memory MigrationMemoryImport
	// MemoryBaseline 是导入完成后 `memory_entries` 的行数对账（S-F04）：导入前 +
	// 本次导入 = 导入后实测，三个数在写切换记录之前核对过，不成立时切换根本不发生。
	// 它同时是回退判据该取的基线——`Checked` 为假表示这次切换没有产出对账。
	// MemoryBaseline is the post-import row reconciliation for memory_entries (S-F04).
	MemoryBaseline MigrationMemoryBaseline
}

// MigrationMemoryBaseline 是旧记忆导入的行数对账：三个数相等才算成立（实现侧在写入
// `storage_cutover` 之前核对，不等即切换不发生）。
// MigrationMemoryBaseline must reconcile before the cutover row is written.
type MigrationMemoryBaseline struct {
	Checked      bool
	ImportRows   int64
	Imported     int64
	ObservedRows int64
}

// MigrationMemoryImport 是旧记忆导入的四类对账投影（条数互斥、加总等于源条目数）。
// 冲突与不可读以**渲染好的行**给出（分层 / 分区 / 状态 / 原因，不含条目正文）。
// MigrationMemoryImport is the four-bucket projection; bodies are never carried here.
type MigrationMemoryImport struct {
	Imported   int
	Skipped    int
	Total      int
	Conflicts  []string
	Unreadable []string
}

// DatabaseMigration 是 `db export / import / verify / switch` 的端口（适配器 ← 装配层的
// `internal/dbmigrate` + PostgreSQL 连接）。六个方法各自对应设计 §3.1.8 的一行命令，
// 命令面因此**不 import** dbmigrate，也不自己打开数据库。
//
// 三条判据由实现侧负责，命令面**不得**重复判：包与清单的三元组绑定、`pending` 核查包
// 的拒收、以及"复核不通过就不写任何东西"。`--handoff` 不在本端口内——它归
// `HandoffVerification`（T142），两条判据各有一份实现。
// DatabaseMigration is the port behind the four migration subcommands. Triple binding,
// pending-package refusal and "verify before write" all live in the implementation.
type DatabaseMigration interface {
	// Export 在静止窗口里导出 SQLite v6 工作区：A1 = A2、快照、逐表 dump、资产清单。
	// 有待核查文件时只产出 `review/` 镜像与 `pending` 清单，并如实回报不可交付。
	// Export walks the quiet window: A1 = A2, snapshot, table dumps and the asset manifest.
	Export(ctx context.Context, req MigrationExportRequest) (MigrationExportReport, error)

	// Seal 用逐条处置把受限核查包封成可交付包（`package/` + 最终 `assets.json`）。
	// Seal turns a reviewed package into the shipped one.
	Seal(ctx context.Context, req MigrationSealRequest) (MigrationSealReport, error)

	// Import 把已复核的迁移包导入**空的**目标库（单事务、不覆盖、不合并）。
	// Import loads the package into an empty target inside one transaction.
	Import(ctx context.Context, database DatabaseConfig, req MigrationImportRequest) (MigrationImportReport, error)

	// VerifyDump 离线复核迁移包：先绑三元组，再逐项重算 dump 与包内资产。
	// VerifyDump checks the package offline: binding first, then every digest.
	VerifyDump(ctx context.Context, req MigrationVerifyRequest) (MigrationVerifyReport, error)

	// VerifyTarget 复核目标库：按清单列元数据回读并比对行数 / 主键集合 / 行摘要 / 引用。
	// VerifyTarget re-reads the target through the manifest's own column metadata.
	VerifyTarget(ctx context.Context, database DatabaseConfig, req MigrationVerifyRequest) (MigrationVerifyReport, error)

	// Switch 在机器可判的三道门禁（包完整、dump 一致、目标库一致）都过后写入 cutover 行。
	// handoff 门禁**不在本方法内**——它归 `HandoffVerification`（T142），实现侧**不得**
	// 声称它已核对。
	// Switch writes the cutover row after the three machine-checkable gates pass. The
	// handoff gate is not part of this method and must never be reported as checked.
	Switch(ctx context.Context, database DatabaseConfig, req MigrationSwitchRequest) (MigrationSwitchReport, error)
}

// InstanceAdoptRequest 是 `instance adopt` 的输入：旧 SQLite 工作区根、与它**同级**的
// `basalt.yaml`，以及操作者显式给出的主密钥文件路径（可空——空表示"按配置里的路径找"）。
// `ConfigPath` 由命令面从工作区根推导（§3.1.8c：`basalt.yaml` 不是工作区资产），
// 实现侧不再自己拼第二份路径口径。
// InstanceAdoptRequest names the legacy workspace root, its sibling basalt.yaml and the
// master-key file the operator named explicitly (empty means "use the configured path").
type InstanceAdoptRequest struct {
	// Workspace 是旧 SQLite 工作区根（默认 `<WorkDir>/.basalt`），必须是绝对路径或
	// 可解析的相对路径——实现侧负责解析，报告里回报解析后的绝对路径。
	Workspace string
	// ConfigPath 是与工作区**同级**的配置文件路径。
	ConfigPath string
	// MasterKeyFile 是 `--master-key-file` 的原值；与配置里的路径不一致时拒绝。
	MasterKeyFile string
	// ConfigChangeRef 是 `--config-change-ref` 的原值：迁移准备副本的配置调整过时，
	// 变更单引用是受控调整的凭据；未调整时留空。
	// ConfigChangeRef carries the change-ticket reference for a controlled config
	// adjustment on a migration-preparation copy.
	ConfigChangeRef string
}

// InstanceAdoptReport 是一次采纳的事实清单。`Case` 是四判定命中编号（1 有记录有密文 /
// 2 有记录无密文 / 3 无记录有密文（拒绝）/ 4 无记录无密文）；**拒绝与失败也填**——
// 命令面据此打印"停在哪一步"，不能只给一条错误字符串。
//
// `WroteSecretsRecord` 永远为 false：采纳只写名册，`instance_secrets` 记录与密文
// 一律不动。`RecoveredPending` 表示本次回填了上次崩溃留下的 pending 行。
// `RestartVerified` 恒为 false——重启复验不在离线命令内，由后续启动流程判定。
// InstanceAdoptReport is the fact sheet of one adoption; a rejection still fills in the
// case and the state so the command surface can name the step it stopped at.
type InstanceAdoptReport struct {
	Workspace             string
	ConfigPath            string
	RosterPath            string
	AdoptedAt             time.Time
	Case                  int
	InstanceID            string
	KeyFile               string
	KeyPresent            bool
	KeyID                 string
	Fingerprint           string
	RecordKeyID           string
	RecordFingerprint     string
	ManagedCiphertexts    int
	CiphertextsVerified   bool
	CiphertextsUnreadable int
	FirstFailure          string
	State                 string
	Action                string
	Reason                string
	// 迁移准备模式的配置核对事实：`ConfigChecked` 为 true 表示记录来自 adopt-legacy，
	// 本次核对了副本配置，后三个字段写进审计终态；普通采纳四者均为零值。
	// Migration-mode config attestation; the three hashes/ref land in the audit.
	ConfigChecked        bool
	TargetConfigSHA256   string
	ConfigChangeRef      string
	ConfigAdjusted       bool
	WroteSecretsRecord   bool
	RecoveredPending     bool
	AuditOperationID     string
	NewPrivateDir        string
	NewPrivateDirCreated bool
	RestartVerified      bool
}

// InstanceAdoption 是 `instance adopt` 的端口：离线采纳旧 SQLite 工作区的实例身份。
// 实现方（装配层）负责全部拒绝判据与审计两阶段写；命令面只做参数解析与报告打印。
// InstanceAdoption is the port behind `instance adopt`; the adapter owns every refusal
// rule and the two-phase audit write, the command surface only parses and prints.
type InstanceAdoption interface {
	// Adopt 为存量工作区建立实例名册：只写 `instance.json`，零外呼、不装配运行期、
	// 不生成 / 搬移 / 重加密密钥。任何拒绝都在写名册之前返回，且零写入。
	Adopt(ctx context.Context, req InstanceAdoptRequest) (InstanceAdoptReport, error)
}

// ─────────────────────────── secrets recover-record / adopt-legacy ───────────────────────────
//
// 两条命令的手工恢复 / 受控升级面（颗粒度文档 §3.1.8b / §3.1.8c）。与上面的 `Secrets`
// 不同：它们**不消费装配层的 secrets 服务**，因为那需要库、撤销编排与解析器——正是
// 「会在旧实例上自动补写记录、启动 MCP」的那条路。恢复与采纳只能离线发生，所以端口由
// 装配层用**独立适配器**实现（直接开库 / 迁移 / 写记录），不经过运行期装配。
// The two manual procedures do not consume the heavy-path secrets service: that path is
// exactly what auto-adopts and starts MCP. They stay offline behind their own adapters.

// BackupKind 是备份目录的内容形态（按**内容**判定，不按路径名或扩展名）。
// BackupKind is the backup's shape, judged by content rather than by name.
type BackupKind string

const (
	// BackupKindExportPackage 是迁移导出包 A：目录里含 `manifest.json`。
	// BackupKindExportPackage is route A: the directory holds manifest.json.
	BackupKindExportPackage BackupKind = "export_package"

	// BackupKindNativeSet 是原生备份集 B：目录里含 `basalt.db` 与 `instance.json`。
	// BackupKindNativeSet is route B: the directory holds basalt.db and instance.json.
	BackupKindNativeSet BackupKind = "native_backup_set"
)

// RecoverRecordRequest 是 `secrets recover-record` 交给端口的**已判定**事实：备份形态与
// 外部锚点已在命令面核对完毕（形态二义、锚点缺失 / 不符都在调用端口之前拒绝），端口
// 据此重算清单、逐项校验并在通过后写记录。请求里没有密钥材料，也没有布尔开关能绕过校验。
// RecoverRecordRequest carries facts the command surface has already decided (shape and
// external anchor); no key material and no bypass switch travels with it.
type RecoverRecordRequest struct {
	// Workspace 是**工作区根** `<WorkDir>/.basalt`，不是 WorkDir 本身。
	// Workspace is the workspace root, not the working directory.
	Workspace string

	// Backup 是备份目录（形态已由 Kind 判明）。
	// Backup is the backup directory whose shape Kind names.
	Backup string

	// Kind 是命令面按内容判定的备份形态。
	// Kind is the shape the command surface decided by content.
	Kind BackupKind

	// ManifestSHA256 是**重算并已与外部锚点核对通过**的备份清单哈希（小写十六进制）。
	// ManifestSHA256 is the recomputed manifest hash that already matched the anchor.
	ManifestSHA256 string

	// Confirm 是操作者的显式确认（命令面已拒绝缺省形态，端口收到的恒为 true）。
	// Confirm is the operator's explicit acknowledgement; the surface rejects its absence.
	Confirm bool
}

// RecoverRecordResult 报告恢复结局：只含公开摘要与计数，不含密钥材料、密文或明文。
// RecoverRecordResult reports public digests and counts only.
type RecoverRecordResult struct {
	// Workspace / Kind / BackupPath 回显本次操作涉及的工作区、备份形态与路径。
	Workspace  string
	Kind       BackupKind
	BackupPath string

	// ExpectedSHA256 / ComputedSHA256 是锚点核对的两侧取值（输出里并排打印）。
	ExpectedSHA256 string
	ComputedSHA256 string

	// RosterPresentBefore 表示恢复前工作区是否已有名册；RosterRestored 表示本次是否
	// 逐字复制了备份名册；RosterSHA256 是名册哈希（有则非空）。
	RosterPresentBefore bool
	RosterRestored      bool
	RosterSHA256        string

	// RecordKeyID / RecordFingerprint 是写入记录里的公开摘要。
	RecordKeyID       string
	RecordFingerprint string

	// Ciphertexts 是本次校验过的托管密文条数；CiphertextsUnreadable 必须为 0 才会写。
	Ciphertexts           int
	CiphertextsUnreadable int

	// InstanceID 是名册（或备份清单）里的实例标识；空表示备份未携带。
	InstanceID string

	// OperationID 是审计操作号（成功与预写后失败都带，供查证）。
	OperationID string

	// FinalState 是命令结束状态：completed / not_applied / failed。
	FinalState string
}

// SecretsRecovery 是 `secrets recover-record` 的端口（§3.1.8b）：离线校验可信备份并在
// 通过后写 1 条 `master_key` 记录（及缺失时的名册），全程零外呼。
// SecretsRecovery is the offline recover-record port.
type SecretsRecovery interface {
	// RecoverSecretsRecord 校验备份与当前工作区后写入记录；任何校验失败都零写入并返回
	// 带退出码的错误（`1` 校验失败 / `2` 前置不满足 / `3` 预写后失败）。
	// RecoverSecretsRecord verifies then writes; every refusal is a zero-write,
	// status-bearing error.
	RecoverSecretsRecord(ctx context.Context, req RecoverRecordRequest) (RecoverRecordResult, error)
}

// AdoptLegacyRequest 是 `secrets adopt-legacy` 交给端口的已判定事实：备份外部锚点与
// 形态已在命令面核对（`backup_unverified` / `backup_invalid` 在调用前拒绝），端口从
// **副本**的旧 schema 出发完成「校验 → 迁移 3～5 → 6 → 写 1 条 attested_legacy 记录 →
// 审计回填」。请求不含密钥材料。
// AdoptLegacyRequest carries the already-verified anchor and paths; the adapter runs
// verify → migrate → record → audit on the isolated copy only.
type AdoptLegacyRequest struct {
	// Workspace 是隔离副本的**工作区根** `<copy-root>/.basalt`；`basalt.yaml` 在其父目录。
	// Workspace is the copy's workspace root; basalt.yaml sits in its parent directory.
	Workspace string

	// Backup 是备份目录（含 `backup.json` 与清单列出的全部文件）。
	// Backup is the backup directory holding backup.json and the listed files.
	Backup string

	// ManifestSHA256 是重算并已与外部锚点核对通过的 `backup.json` 字节哈希。
	// ManifestSHA256 is the recomputed, anchor-matched hash of backup.json bytes.
	ManifestSHA256 string

	// Source 是原实例的 `.basalt` 根（本地可读时的**首选**见证方式）；与
	// AttestSourceUnchanged 二选一。
	// Source is the original instance's workspace root; exactly one witness is used.
	Source string

	// AttestSourceUnchanged 是带外见证编号（源端不可读时使用）。
	// AttestSourceUnchanged is the out-of-band witness reference.
	AttestSourceUnchanged string

	// MasterKeyFile 是受控取钥的覆盖路径（K1 / K2）；空表示按副本配置里的路径读取。
	// MasterKeyFile overrides the configured key path for controlled retrieval.
	MasterKeyFile string

	// ExpectKeySHA256 是覆盖取钥的外部锚点（K2 必填，K1 可省）。
	// ExpectKeySHA256 is the external key-file anchor; mandatory for K2.
	ExpectKeySHA256 string

	// Confirm 是操作者的显式确认（命令面已拒绝缺省形态）。
	// Confirm is the operator's explicit acknowledgement.
	Confirm bool
}

// AdoptLegacyResult 报告采纳结局：只含公开摘要与计数，不含密钥材料、密文或明文。
// AdoptLegacyResult reports public digests and counts only.
type AdoptLegacyResult struct {
	// Workspace / BackupPath 回显本次操作的两侧路径。
	Workspace  string
	BackupPath string

	// ExpectedSHA256 / ComputedSHA256 是备份清单锚点的两侧取值。
	ExpectedSHA256 string
	ComputedSHA256 string

	// 配置哈希四值：源端（唯一漂移基线）/ 备份自证 / 副本 / 规范化摘要。
	SourceConfigSHA256         string
	BackupConfigSHA256         string
	CopyConfigSHA256           string
	ConfigWithoutKeyPathSHA256 string

	// SchemaBefore / SchemaAfter 是副本 schema 的前后版本。
	SchemaBefore int
	SchemaAfter  int

	// KeyID / KeyFingerprint 是写入记录的公开摘要；KeySource 取 backup_copy /
	// external_anchor / config_path；KeyFileSHA256 是取钥文件的字节哈希。
	KeyID          string
	KeyFingerprint string
	KeySource      string
	KeyFileSHA256  string

	// Ciphertexts 是迁移后复核通过的密文条数。
	Ciphertexts int

	// RosterPresent 表示副本是否已带名册（采纳命令**不**写名册）。
	RosterPresent bool

	// IdentityEvidence 是身份证据档位（§2.12.0b 可信备份判定）：roster 表示清单带着
	// 名册且已按哈希 + 实例标识核对；attested_no_roster 表示无名册存量——身份来自部署者
	// 见证，**不是**密码学证明。
	IdentityEvidence string

	// SourceWitness 是本次采用的源端见证方式：source / attestation。
	SourceWitness string

	// InstanceID 是备份清单里的实例标识（空表示备份未携带）。
	InstanceID string

	// OperationID 是审计操作号（成功与预写后失败都带）。
	OperationID string

	// FinalState 是命令结束状态：completed / not_applied / failed。
	FinalState string
}

// LegacySecretsAdopt 是 `secrets adopt-legacy` 的端口（§3.1.8c）：只对隔离副本操作，
// 不装配 runtime / MCP / 调度 / HTTP / notifier / provider，不写名册、不改配置、不生成
// 或复制密钥、不改写密文。
// LegacySecretsAdopt is the offline adopt-legacy port, confined to the isolated copy.
type LegacySecretsAdopt interface {
	// AdoptLegacySecrets 在副本上完成校验、迁移与记录写入；任何校验失败都零写入并返回
	// 带退出码的错误（`1` 校验失败 / `2` 前置不满足 / `3` 预写后失败）。
	// AdoptLegacySecrets verifies, migrates and records on the copy only; every
	// refusal is a zero-write, status-bearing error.
	AdoptLegacySecrets(ctx context.Context, req AdoptLegacyRequest) (AdoptLegacyResult, error)
}

// ─────────────────────────── db verify --handoff ───────────────────────────

// HandoffVerifyRequest 是 `db verify --handoff` 的输入。
//
// `Dir` 的形态**只有一个**：迁移准备副本的**工作区根**（`<copy-root>/.basalt`）。
// `basalt.yaml` 取它的**同级** `<copy-root>/basalt.yaml`——与 `instance adopt` 的
// `report.ConfigPath` 同一路径口径。导出包（`manifest.json` + JSONL + `package/`）
// 不是本命令的输入：包内不含名册、密钥与配置，卡片要求的核对在包上无法执行；
// 包自身用 `db verify --dump` 复核。
// Dir is the migration-preparation copy's workspace root; its sibling basalt.yaml is
// the configuration under check. An export package is refused by construction.
type HandoffVerifyRequest struct {
	Dir string
}

// HandoffVerifyReport 是交接核对的事实清单：**拒绝与失败也填**——命令面据此打印
// "停在哪一项"，不能只给一条错误字符串。报告只含公开摘要与计数，不含密钥材料、
// 密文或明文。
// HandoffVerifyReport is the fact sheet of one handoff verification; a rejection still
// fills in every reading gathered before the stop.
type HandoffVerifyReport struct {
	// Dir 是命令面收到的原值；Workspace / ConfigPath / RosterPath 是解析后的绝对路径。
	Dir        string
	Workspace  string
	ConfigPath string
	RosterPath string

	// 身份门：名册是否在、实例标识是什么。
	InstanceID    string
	RosterPresent bool

	// 记录：`instance_secrets` 的公开摘要（无记录时三者为零值）。
	RecordPresent     bool
	RecordKeyID       string
	RecordFingerprint string

	// 密钥：文件、路径是否已持久化为绝对路径、key_id 与指纹。
	KeyFile          string
	KeyPresent       bool
	KeyPathPersisted bool
	KeyID            string
	Fingerprint      string

	// 目标配置核对：`ConfigRequired` 为 true 表示记录来自 `adopt-legacy`（迁移准备
	// 副本），`target_config_sha256` 是硬门禁；普通存量实例为 false，`ConfigNote`
	// 必须显式说明"未核对"及其原因——缺省读法不得把"没核"当成"核过"。
	ConfigRequired      bool
	ConfigChecked       bool
	TargetConfigSHA256  string
	CurrentConfigSHA256 string
	ConfigNote          string

	// 凭据核验（与 `secrets verify` 同一实现）：两组计数逐项对应。
	Credentials SecretsVerifyReport

	// Ready 是总判据；不 ready 时 Code / Detail 说明停在哪一项，Recovery 给恢复指引。
	Ready    bool
	Code     string
	Detail   string
	Recovery string
	Duration time.Duration
}

// HandoffVerification 是 `db verify --handoff` 的端口：只读核对迁移准备副本的交接
// 就绪（名册 / 记录 / 密钥 / 全部密文 / 目标配置），不装载配置、不要求 DSN、不装配
// runtime、零外呼。解密与绑定校验复用 `secrets verify` 的同一实现，**不得**各写一份。
// HandoffVerification is the offline handoff gate: read-only, no DSN, no runtime, and
// the decryption/binding pass is the very same implementation secrets verify uses.
type HandoffVerification interface {
	// VerifyHandoff 核对交接就绪；不就绪时返回带稳定码与退出码 `1` 的错误，报告照常填。
	// 参数 / 装配前置不满足属退出码 `2`（命令面已判）。
	// VerifyHandoff returns a status-bearing error for every not-ready verdict; the
	// report is filled either way.
	VerifyHandoff(ctx context.Context, req HandoffVerifyRequest) (HandoffVerifyReport, error)
}
