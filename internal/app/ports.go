package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// 本文件是装配层的**适配器**：把实现包（storage / workspace）接到命令面自己声明的
// 端口上（见 internal/cli/ports.go）。适配器只能放在装配层——端口的一个返回类型
// 是 cli 定义的接口类型，storage 无法命名它（那会成为反向依赖）。
// The assembly layer is the only place these adapters can live: a port returns a
// cli-defined interface type, which storage cannot name without depending back
// on cli.

// storageStore 把 storage 包适配成 cli.Store。
// storageStore adapts the storage package to cli.Store.
type storageStore struct{}

// Bootstrap 转调引导入口。
// Bootstrap forwards to the storage bootstrap entry.
func (storageStore) Bootstrap(ctx context.Context, path string, target int) (bool, error) {
	return storage.Bootstrap(ctx, path, target)
}

// OpenReadOnly 转调只读打开，并把库句柄接到只读视图端口上。
// OpenReadOnly forwards the read-only open and returns it as the view port.
func (storageStore) OpenReadOnly(ctx context.Context, path string, target int) (cli.SchemaView, error) {
	db, err := storage.OpenReadOnly(ctx, path, target)
	if err != nil {
		// typed-nil 陷阱：*storage.DB 装箱进接口时**必须**在这一步显式判空——
		// 否则得到"非空接口 + 空指针"，上层的 err != nil 判据全部失效，nil 检查
		// 形同虚设（实测形态：`if db == nil` 为 false 而方法调用 panic）。
		// A typed nil boxed into the interface is non-nil, so the error path must
		// return early here.
		return nil, err
	}
	return db, nil
}

// workspacePort 把 workspace 包适配成 cli.Workspace。
// workspacePort adapts the workspace package to cli.Workspace.
type workspacePort struct{}

// Init 转调骨架创建，报告类型收成裸切片（端口形态）。
// Init forwards the skeleton creation, flattening the report struct.
func (workspacePort) Init(root string) (created, kept []string, err error) {
	report, err := workspace.Init(root)
	if err != nil {
		return nil, nil, err
	}
	return report.Created, report.Kept, nil
}

// Inspect 转调骨架清点，只把缺失清单交回端口。
// Inspect forwards the skeleton inventory, returning the missing list only.
func (workspacePort) Inspect(root string) ([]string, error) {
	report, err := workspace.Inspect(root)
	if err != nil {
		return nil, err
	}
	return report.Missing, nil
}

// cliOperationSource 是 CLI 写路径操作行的 `source` 字面量（契约 §9.3 的合法取值，
// 与启动对账的 reconcileSource 同值同义）。
// cliOperationSource is the literal written to the source column for CLI writes.
const cliOperationSource = "cli"

// agentsPort 把 workspace / storage 包适配成 cli.Agents（list / show 读，create / delete 写）。
// agentsPort adapts the workspace and storage packages to cli.Agents.
//
// 读面（List）走轻路径：不取实例锁、不打开库——装载器只读文件与调用方显式传入的
// provider 名集。写面（Create / Delete）是 **CLI 写路径的完整生命周期所有者**
// （颗粒度文档 §3.1.5 H4）：取一次实例锁 → 打开并迁移库 → 装配最小管理面 →
// `AgentAdmin` 编排（预写操作行、生命周期状态文件、归档记录）→ 关闭库 → 释放锁；
// 成功与失败同序。错误一律**原样**上抛——拒锁 / 已存在 / 归档撞名都是实现层的事实，
// 端口不改写语义。
// The read path takes no lock and opens no database; the write path owns the whole CLI
// lifecycle (lock, open + migrate, minimal management assembly, AgentAdmin, close,
// release) on both outcomes.
type agentsPort struct {
	// WorkDir 是命令跑在哪个目录（实例配置 basalt.yaml 所在层）——写模式日志的上下文。
	// WorkDir is the directory the command ran in, where basalt.yaml lives.
	WorkDir string

	// SupportedSchemaVersion 是写路径打开 / 迁移数据库的目标版本（与装配层同源）。
	// SupportedSchemaVersion is the target schema version for the write path.
	SupportedSchemaVersion int

	// Logger 是装配层注入的日志器（带配置档位）；nil 时回落进程默认档。
	// Logger is the injected logger; a nil falls back to the process default.
	Logger *slog.Logger
}

// List 转调装载器：provider 名集由调用方显式传入（适配器**不**另读一次配置）；工具池用
// 装配层的**声明名集**（`declaredToolNames`，wire.go）——轻路径不建注册池，但装载判定必须
// 与重路径同一份名字，否则同一份 AGENT.md 会因走了哪条命令而得到相反的结论。传零值等于
// "空池"，那会把所有声明了工具的 Agent 判成未注册（装载器的空池语义，US-4 已判卷）。
// List forwards to the loader with the provider names passed in explicitly and the declared
// tool pool: the light path builds no registry, so the declaration is what keeps both paths
// in agreement. Passing nothing would mean an empty pool, which would reject every agent
// that declares tools.
func (p agentsPort) List(root string, providers []string) ([]core.AgentEntry, error) {
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: providers,
		Tools:     declaredToolNames,
		Logger:    p.logger(),
	}).LoadAll(root)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Create 经 `AgentAdmin.Create` 写真实操作行（约束 27）：名字 / 目标空闲校验、预写
// `agent.create`、`Bootstrap` 状态文件（disabled / generation=1）、模板目录两次 rename、
// 生命周期索引全部由管理面编排——CLI 不走第二套实现。provider 名集取命令层装载的
// 配置列表（与 `profile create` 打印的「已选择 provider」同源），适配器把它交给模板
// 的确定性选择（约束 28），签名里的参数因此不是死参数。
// Create goes through AgentAdmin.Create so the CLI writes a real operation row; the
// provider list is the command layer's configured list, the same source it prints.
func (p agentsPort) Create(root, name string, providers []string) ([]string, error) {
	p.logger().Debug("Agent 创建（CLI 写路径）",
		"workdir", p.WorkDir, "workspace", root, "agent", name)

	var created []string
	err := p.withCLIAdmin(root, time.Now, providers, func(ctx context.Context, admin *AgentAdmin) error {
		view, err := admin.Create(ctx, p.actor(), core.AgentSpec{Name: name})
		if err != nil {
			return err
		}
		created = make([]string, 0, len(view.Files))
		for _, file := range view.Files {
			// 视图路径相对 Agent 目录；命令面打印相对工作区根的路径（与归档输出同口径）。
			// The view is agent-relative; the command surface echoes workspace-relative paths.
			created = append(created, path.Join(agentsDirName, name, file.Path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Delete 经 `AgentAdmin.Archive` 走约束 32 的归档编排并写归档记录：CLI 持有实例锁，
// 注册表注销 / 调度取消 / 在途 Run 等待三项活动面依赖以 nil 表达，适配器记 Debug 后
// 直接进入状态迁移与目录搬迁（归档行与操作终态同事务）。注入的 now 同时进入归档标识
// 与操作行时间戳，`profile delete` 的可复现输出因此仍然成立。
// Delete goes through AgentAdmin.Archive so the CLI writes an archive row; registry,
// scheduler and run-drain are nil on this path, and the injected instant drives both
// the archive id and the operation timestamps.
func (p agentsPort) Delete(root, name string, now time.Time) (string, error) {
	p.logger().Debug("Agent 归档（CLI 写路径）",
		"workdir", p.WorkDir, "workspace", root, "agent", name)

	clock := func() time.Time { return now }
	var archivePath string
	err := p.withCLIAdmin(root, clock, nil, func(ctx context.Context, admin *AgentAdmin) error {
		record, err := admin.Archive(ctx, p.actor(), name)
		if err != nil {
			return err
		}
		// ArchiveID 就是 `archive/` 下的目录名（§3.1.5 / core.AgentArchiveRecord 注释）；
		// 相对工作区根的展示路径由适配器拼出，命令面不认识工作区布局。
		archivePath = path.Join("archive", record.ArchiveID)
		return nil
	})
	if err != nil {
		return "", err
	}
	return archivePath, nil
}

// withCLIAdmin 在一次实例锁内装配最小管理面并执行 fn：库句柄在成功与失败路径都关闭，
// 实例锁由 withInstanceLock 在关闭之后释放（次序与获取相反）。业务写入已生效时关闭
// 失败只记 ERROR——不得把落地的写说成失败。
// withCLIAdmin runs fn against the minimal management assembly under one instance lock;
// the database is closed on both outcomes and the lock is released last.
func (p agentsPort) withCLIAdmin(root string, now func() time.Time, providers []string,
	fn func(ctx context.Context, admin *AgentAdmin) error,
) error {
	return withInstanceLockErr(root, func() error {
		ctx := context.Background()
		session, err := p.openCLIAdmin(ctx, root, now, providers)
		if err != nil {
			return err
		}
		runErr := fn(ctx, session.admin)
		closeErr := session.db.Close()
		if runErr != nil {
			return errors.Join(runErr, closeErr)
		}
		if closeErr != nil {
			p.logger().Error("CLI 写路径关闭数据库失败（业务写入已生效）",
				"workspace", root, "error", closeErr)
		}
		return nil
	})
}

// cliAdminSession 是一次 CLI 写路径装配出来的库句柄与管理面。
// cliAdminSession pairs the database handle with the assembled management surface.
type cliAdminSession struct {
	db    *storage.DB
	admin *AgentAdmin
}

// openCLIAdmin 在已持有的实例锁内打开并迁移库，装配约束 27 / 32 需要的最小组件：
// 管理操作仓储、生命周期状态文件、资源存储（生命周期索引）、Agent 写锁与归档仓储。
// HTTP / MCP / 调度器 / 文件监视器**不**装配——CLI 写命令不服务请求也不跑定时任务，
// 归档的注销 / 取消 / 等待因此以三项 nil 依赖表达（约束 32 的 CLI 分支）。
// openCLIAdmin opens and migrates the database under the held lock and wires only the
// components the CLI write path needs; the heavy runtime stays unbuilt.
func (p agentsPort) openCLIAdmin(ctx context.Context, root string, now func() time.Time,
	providers []string,
) (*cliAdminSession, error) {
	version := p.SupportedSchemaVersion
	if version <= 0 {
		return nil, errors.New("CLI 写路径缺少 schema 目标版本（装配错误）")
	}
	db, err := storage.OpenWritable(ctx, filepath.Join(root, databaseFileName), version)
	if err != nil {
		return nil, err
	}
	// CLI 写路径与重路径用**同一条**迁移编排：v3 的六步（准入 → 状态文件 → SQL 单事务 →
	// 身份回填 → 绑定回填 → 投影重建）在两条路上逐字节相同，另开一条 `db.Migrate` 捷径
	// 会让"升级过的库"依命令不同而不同。库已是目标版本时返回零报告、不写操作行。
	// The CLI write path runs the very same migration orchestration as the heavy path.
	index := storage.NewResourceStore(db)
	operations := storage.NewManagementStore(db)
	lifecycle := workspace.NewLifecycleStore(root, now)
	migrator := NewMigrator(MigrateV3Deps{
		DB:         db,
		Operations: operations,
		Lifecycle:  lifecycle,
		Resources:  index,
		Notify:     storage.NewNotifyAdminStore(db),
		Bindings:   storage.NewBindingStore(db),
		Root:       root,
		Logger:     p.logger(),
		Now:        now,
		Target:     version,
	})
	if _, migrateErr := migrator.MigrateV3(ctx); migrateErr != nil {
		return nil, errors.Join(fmt.Errorf("迁移数据库到版本 %d: %w", version, migrateErr), db.Close())
	}

	admin := NewAgentAdmin(AgentAdminDeps{
		Root:       root,
		Gate:       core.NewAgentGate(),
		Lifecycle:  lifecycle,
		Index:      index,
		Operations: operations,
		Archives:   storage.NewAgentAdminStore(db),
		Logger:     p.logger(),
		Now:        now,
		// CLI 没有已发布目录快照：模板的 provider 顺序以命令层装载的配置列表为准。
		// The CLI has no published snapshot; the configured list is the template source.
		PublishedProviders: func(context.Context) ([]workspace.ProviderEntry, error) {
			entries := make([]workspace.ProviderEntry, 0, len(providers))
			for _, name := range providers {
				entries = append(entries, workspace.ProviderEntry{Name: name})
			}
			return entries, nil
		},
	})
	return &cliAdminSession{db: db, admin: admin}, nil
}

// actor 是 CLI 写路径的操作主体：`cli:<本机用户>` / auth_method=cli / source=cli
// （契约 §9.3 的固定字面量；与启动对账同一归因口径）。
// actor is the CLI write path's operation identity, matching the contract literals.
func (p agentsPort) actor() core.Actor { return cliWriteActor(p.Logger) }

// cliWriteActor 构造 CLI 写路径的操作主体（契约 §9.3 的固定字面量）。
// 多个 CLI 写面（Agent 管理 / 凭据撤销）共用这一处——主体口径分叉就等于审计归因分叉。
// cliWriteActor is the single place the CLI operation identity is built; both CLI write
// surfaces share it so audit attribution cannot drift.
func cliWriteActor(logger *slog.Logger) core.Actor {
	return core.Actor{
		Subject: core.ActorSubjectCLIPrefix + cliUserName(logger),
		Method:  core.AuthMethodCLI,
		Source:  cliOperationSource,
	}
}

// cliUserName 取本机用户名；取不到时记 WARN 并以 `unknown` 收场（操作行仍可归因到 CLI）。
// cliUserName falls back to "unknown" with a WARN when the OS does not name the user.
func cliUserName(logger *slog.Logger) string {
	if current, err := user.Current(); err == nil {
		if name := strings.TrimSpace(current.Username); name != "" {
			return name
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("CLI 写路径：无法确定本机用户名，操作主体记为 cli:unknown")
	return "unknown"
}

// withInstanceLock 取一次工作区实例锁并执行 fn，返回前释放（失败也释放）：
// 被活实例持有时原样上抛 *workspace.LockBusyError——拒绝启动、零业务写入。
// withInstanceLock runs fn while holding the instance lock and always releases it.
func withInstanceLock[T any](root string, fn func() (T, error)) (T, error) {
	var zero T
	lock, err := workspace.AcquireInstanceLock(root)
	if err != nil {
		return zero, err
	}
	value, err := fn()
	if relErr := lock.Release(); relErr != nil {
		err = errors.Join(err, relErr)
	}
	return value, err
}

// withInstanceLockErr 是 withInstanceLock 的 error-only 形态。
// withInstanceLockErr is the error-only form of withInstanceLock.
func withInstanceLockErr(root string, fn func() error) error {
	_, err := withInstanceLock(root, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

// logger 保证日志出口永远可用：适配器不因为缺日志器而 panic（缺省档与 cli 侧同口径）。
// logger keeps the log outlet usable even when no logger was injected.
func (p agentsPort) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

// runServicePort 把 core.AgentService（唯一运行入口）适配成 cli.RunService。
// runServicePort adapts the run entry point to the command surface's port.
//
// 这一层**不做任何事**——不改写请求、不翻译错误、不加超时：`core` 的返回语义本就与端口
// 同源（调用方问题走 error、运行期失败走 `RunResult.Status`，契约 §1.2），适配器一旦
// "顺手"加工，两层语义立刻开始分叉。本类型是**接缝**而不是工厂：装配（wire.go）给出
// 实例，命令面只认接口。
// The adapter is a seam, not a factory: it forwards verbatim, because the return
// semantics of core and the port are the same by design.
type runServicePort struct {
	service *core.AgentService
}

// Run 转调唯一运行入口：准入、装配、循环、收尾全在 core 之内。
// Run forwards to the single run entry point.
func (p runServicePort) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	return p.service.Run(ctx, req)
}

// sessionListerPort 把 storage.SessionStore 适配成 cli.SessionLister（**只读**端口：
// `session list` 与 `chat` 的历史读取，写路径不在这里——追加的唯一出口在 core）。
// sessionListerPort adapts the session store to the read-only port.
type sessionListerPort struct {
	store *storage.SessionStore
}

// ListSessions 转调会话清单（排序与去重是实现侧的事，端口保序）。命令面没有过滤参数，
// 这里**固定传空过滤器**——既有的全量口径逐字不变；过滤与归档状态派生只服务
// `GET /sessions`（web 端口直连仓储，不走本适配器）。
// ListSessions forwards the unfiltered list: the command surface has no filter flags,
// so an empty filter keeps its previous behaviour exactly.
func (p sessionListerPort) ListSessions(ctx context.Context) ([]core.SessionRecord, error) {
	return p.store.ListSessions(ctx, core.SessionFilter{})
}

// Messages 转调一个会话的完整历史。
// Messages forwards one session's full history.
func (p sessionListerPort) Messages(ctx context.Context, sessionID string) ([]core.Message, error) {
	return p.store.Messages(ctx, sessionID)
}

// toolListerPort 把 tool.Registry（装配期构建的内存注册池）适配成 cli.ToolLister。
// toolListerPort adapts the assembly-time tool registry to the command surface's port.
//
// 这一层做的**唯一一件事**是选顺序：`Specs(Names())` 的名字排序是清单顺序的唯一定义处
// （命令面保序渲染、零重排），适配器不排序、不过滤、不补默认值。接口给的是**数据**
// （`core.ToolSpec` 三件字段），所以本类型不把工具实现包带进 cli——依赖方向保持
// app → 两侧，cli 与 tool 互不相识。
// The adapter's only job is to pick the order: Specs(Names()) is the single definition
// of the listing order. It passes data, not the implementation package, so cli and tool
// stay strangers to each other.
type toolListerPort struct {
	registry *tool.Registry
}

// Tools 转调注册池的清单（顺序 = 注册池的名字排序，由 Names() 定义）。
// Tools forwards the registry's specs in the registry's own order.
func (p toolListerPort) Tools() []core.ToolSpec {
	return p.registry.Specs(p.registry.Names())
}

// secretsPort 把装配层的 SecretsService 适配成 `cli.Secrets`：投影结果类型，并补上
// 命令面不携带的操作主体（`cli:<本机用户>` / auth_method=cli / source=cli）——身份
// 归因属于装配层，命令面只发起动作，不自己编造主体。
// secretsPort adapts the secrets service to the command surface's port and supplies the
// CLI operation identity, which belongs to the assembly layer.
type secretsPort struct {
	service SecretsService
	Logger  *slog.Logger
}

// Init 转调并投影结果；主体归因与 RevokeManaged 同源（命令面不自己编造身份）。
// Init forwards and projects the result with the same CLI attribution as RevokeManaged.
func (p secretsPort) Init(ctx context.Context) (cli.SecretsStatus, error) {
	status, err := p.service.Init(ctx, cliWriteActor(p.Logger))
	return cliSecretsStatus(status), err
}

// Status 转调并投影结果。
// Status forwards and projects the result.
func (p secretsPort) Status(ctx context.Context) (cli.SecretsStatus, error) {
	status, err := p.service.Status(ctx)
	return cliSecretsStatus(status), err
}

// Verify 转调并投影结果。
// Verify forwards and projects the result.
func (p secretsPort) Verify(ctx context.Context) (cli.SecretsVerifyReport, error) {
	report, err := p.service.Verify(ctx)
	return cli.SecretsVerifyReport{Checked: report.Checked, Failed: report.Failed}, err
}

// RevokeManaged 转调并补上 CLI 操作主体（撤销要写管理操作行，必须可归因）。
// RevokeManaged forwards with the CLI operation identity, since the revoke writes
// management operation rows.
func (p secretsPort) RevokeManaged(ctx context.Context) (int, error) {
	return p.service.RevokeManaged(ctx, cliWriteActor(p.Logger))
}

// cliSecretsStatus 把装配层的状态投影成命令面类型（两个包的同名字段一一对应）。
// cliSecretsStatus projects the assembly status onto the command-surface type.
func cliSecretsStatus(status SecretsStatus) cli.SecretsStatus {
	return cli.SecretsStatus{
		Initialized:             status.Initialized,
		State:                   string(status.State),
		Reason:                  string(status.Reason),
		KeyID:                   status.KeyID,
		Fingerprint:             status.Fingerprint,
		ManagedModelConnections: status.ManagedModelConnections,
		ManagedNotifyChannels:   status.ManagedNotifyChannels,
	}
}

// 编译期断言：端口形状与实现签名一旦对不上，在这里先红——不是在装配处报"缺方法"。
// Compile-time assertions: the ports and the implementations must match here first.
var (
	_ cli.RunService    = runServicePort{}
	_ cli.SessionLister = sessionListerPort{}
	_ cli.ToolLister    = toolListerPort{}
	_ cli.Secrets       = secretsPort{}
)

// ============================================================
// R4 / v3 资源身份与凭据端口（T126）
// ============================================================
//
// 这八组端口是**消费方声明**：装配层（wire.go）与编排层按接口取用，具体实现留在
// storage / app 的具体类型里。端口在这里集中的理由与 `cli.*` 端口相同——装配层是唯
// 一允许依赖两侧的地方；接口与实现的签名一旦漂移，本文件的编译期断言先红。
// These eight consumer-side ports (T126) keep the assembly layer as the only place that
// names both sides; the assertions below fail the build the moment an implementation
// drifts away from its port.

// CredentialStore 是凭据身份 / 版本 / 资源版本关联的读写面（`storage.CredentialStore`
// 实现）：写入新身份、追加版本、撤销与退役、按身份或属主读取、记录资源版本解析到的
// 凭据版本。读面**不返回**解密后的明文——解密归 `CredentialResolver`。
// CredentialStore reads and writes credential identities and versions; it never returns
// plaintext, because decryption belongs to CredentialResolver.
type CredentialStore interface {
	Create(ctx context.Context, identity core.CredentialRecord, version core.CredentialVersionRecord) error
	AppendVersion(ctx context.Context, credentialID, keyID, operationID string,
		version core.CredentialVersionRecord, at time.Time) error
	Revoke(ctx context.Context, credentialID, operationID string, at time.Time) error
	Retire(ctx context.Context, credentialID, operationID string, at time.Time) error
	Credential(ctx context.Context, credentialID string) (core.CredentialRecord, error)
	CredentialByOwner(ctx context.Context, kind core.BindingKind, ownerUID core.ResourceUID) (core.CredentialRecord, error)
	Version(ctx context.Context, credentialID string, versionNo int) (core.CredentialVersionRecord, error)
	Versions(ctx context.Context, credentialID string) ([]core.CredentialVersionRecord, error)
	RecordVersionCredential(ctx context.Context, versionID int64, credentialID string, resolvedVersion int, at time.Time) error
	VersionCredential(ctx context.Context, versionID int64, credentialID string) (int, bool, error)
}

// CredentialResolver 按资源解析**当前** active 版本的凭据（T130 实现）：模型连接给
// 明文 Key，通知渠道给投递地址。两个方法都每次重新读取主密钥与权威行，**不缓存**；
// 失败按固定错误返回，不回落 env、不自动生成新密钥。
// CredentialResolver resolves the current active credential of a resource on every call;
// it never caches a key and never falls back to env.
type CredentialResolver interface {
	// NotifyCredentialResolver 是工具层已声明的通知解析面（同一形状，不另造一版）。
	// The embedded face is the tool's own notify resolver.
	tool.NotifyCredentialResolver
	// ResolveModelCredential 返回模型连接当前 active 版本的明文 Key。
	// ResolveModelCredential returns the current plaintext key of one model connection.
	ResolveModelCredential(ctx context.Context, connection core.ResourceVersionRef) (string, error)
}

// CredentialGuard 是模型调用前的检查点（T131 实现，FR-078）：每次 `Chat` 前按 Run
// 准入固定的引用查权威版本状态与撤销纪元；`revoked` / 身份 `revoked` / 纪元变化 /
// 版本不可解密 → 固定错误并终止本次 Run。
// CredentialGuard is the pre-call checkpoint: the run's pinned credential must still be
// active with an unchanged revocation epoch, or the run stops with a fixed error.
type CredentialGuard interface {
	Check(ctx context.Context, ref core.RunCredentialRef) error
}

// BindingStore 是 `agent_resource_bindings` 投影的读写面（`storage.BindingStore` 实现）：
// 权威永远是 `AGENT.md`，这里只重建 / 清空 / 读取投影与按资源 UID 反查引用者。
// BindingStore rebuilds and reads the binding projection; AGENT.md stays authoritative.
type BindingStore interface {
	Rebuild(ctx context.Context, agentUID core.AgentUID, agentName, contentHash string,
		bindings core.ResourceBindings, at time.Time) error
	Clear(ctx context.Context, agentUID core.AgentUID) error
	Bindings(ctx context.Context, agentUID core.AgentUID) ([]core.AgentResourceBinding, error)
	ReferencingAgents(ctx context.Context, uid core.ResourceUID) ([]core.AgentResourceBinding, error)
}

// IdentityStore 是 `resource_identities` 的读写面（`storage.IdentityStore` 实现）：
// UID 一次性分配、永不复用；删除只写墓碑，同名重建拿新 UID。
// IdentityStore allocates immutable uids and tombstones identities; uids never return.
type IdentityStore interface {
	Create(ctx context.Context, rec core.ResourceIdentity) error
	Tombstone(ctx context.Context, uid core.ResourceUID, operationID string, at time.Time) error
	IdentityByUID(ctx context.Context, uid core.ResourceUID) (core.ResourceIdentity, error)
	IdentitiesByName(ctx context.Context, kind core.ResourceKind, name string) ([]core.ResourceIdentity, error)
	AllocateVersionNo(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) (int, error)
}

// ScopeConversion 是范围转换端口（T121 的 `ScopeService` 的消费方接口形态）：
// 共享 ⇄ 专属的检查与原子生效。端口名不用 `ScopeService`——那个名字已被具体服务占用；
// 两者形状一致由下方编译期断言钉住。
// ScopeConversion is the consumer-side face of the ScopeService concrete type; the port
// uses a distinct name because the concrete already owns ScopeService in this package.
type ScopeConversion interface {
	Convert(ctx context.Context, req ScopeRequest) (ScopeResult, error)
}

// Migrator 是 v3 迁移编排端口（T134 装配）：读当前 schema 版本、必要时执行六步编排并
// 返回报告。库已是目标版本时返回零报告且不写操作行。
// Migrator is the v3 migration orchestration port; a database already at the target
// returns a zero report and no operation row.
type Migrator interface {
	MigrateV3(ctx context.Context) (MigrateV3Report, error)
}

// SecretsStatus 是 `secrets status` / `init` 的只读结果：**不含**密钥、密文或指纹以外的
// 任何凭据材料（指纹是主密钥的公开标识，供运维比对，不是密钥派生出的可用秘密）。
// SecretsStatus is the read-only secrets result; it never carries key material.
type SecretsStatus struct {
	Initialized bool
	// State / Reason 是实例加密三态判定（T085）与恢复细分原因：`Initialized` 只说
	// "密钥文件可读"，"这个实例的加密身份是不是可用的"由这两个字段回答。
	// State and Reason carry the three-state verdict; Initialized only says the file reads.
	State                   core.SecretState
	Reason                  core.SecretRecoveryReason
	KeyID                   string
	Fingerprint             string
	ManagedModelConnections int
	ManagedNotifyChannels   int
}

// SecretsVerifyReport 是 `secrets verify` 的计数结果：只回检查与失败条数，不回密钥。
// SecretsVerifyReport counts checks and failures only.
type SecretsVerifyReport struct {
	Checked int
	Failed  int
}

// SecretsService 是 CLI `secrets` 子命令的服务面（T135 装配）：`init` 生成主密钥并设
// 权限、`status` / `verify` 只读报告、`revoke-managed` 在服务停止时撤销全部托管凭据
// （不需解密，写审计）。**不**提供自动轮换。
// SecretsService backs the secrets subcommands; it never prints key material and offers
// no automatic rotation.
type SecretsService interface {
	// Init 的 actor 是初始化审计的归因：命令面传入 CLI 操作主体，页面传入管理员身份。
	Init(ctx context.Context, actor core.Actor) (SecretsStatus, error)
	Status(ctx context.Context) (SecretsStatus, error)
	Verify(ctx context.Context) (SecretsVerifyReport, error)
	RevokeManaged(ctx context.Context, actor core.Actor) (int, error)
}

// 编译期断言（T126）：存储侧四个实现今天就必须满足端口；范围转换端口由既有具体服务
// 满足。凭据解析 / 守卫 / 迁移 / secrets 四个实现随 B5 落地，届时在实现文件里补断言。
// Compile-time assertions: the four storage implementations must satisfy their ports
// today; the scope port is satisfied by the existing concrete service.
var (
	_ CredentialStore = (*storage.CredentialStore)(nil)
	_ IdentityStore   = (*storage.IdentityStore)(nil)
	_ BindingStore    = (*storage.BindingStore)(nil)
	_ ScopeConversion = (*ScopeService)(nil)
)
