package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
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

// agentsPort 把 workspace 包适配成 cli.Agents（list / show 读，create / delete 写）。
// agentsPort adapts the workspace package to cli.Agents.
//
// 错误一律**原样**上抛——拒锁 / 已存在 / 归档撞名都是实现层的事实，端口不改写语义。
// 两个字段各有各的用途：装载器拿到**注入的**日志器（装载期的逐条点名 WARN 因此走配置
// 档位，而不是进程默认档）；写模式的两次调用各记一条 DEBUG——写的是持久状态，"哪个
// 工作目录上的哪个 Agent"排障时要能一句话答上来（prod 默认关闭，见工程规范 §4）。
// Errors travel through unchanged; the fix-up-free forwarding is the whole point of
// the adapter. The injected logger carries the configured profile into the loader's
// per-agent WARN lines, and the work directory gives the write-mode debug lines the
// one fact the workspace root does not carry: which instance the command ran in.
type agentsPort struct {
	// WorkDir 是命令跑在哪个目录（实例配置 basalt.yaml 所在层）——写模式日志的上下文。
	// WorkDir is the directory the command ran in, where basalt.yaml lives.
	WorkDir string

	// Logger 是装配层注入的日志器（带配置档位）；nil 时回落进程默认档。
	// Logger is the injected logger; a nil falls back to the process default.
	Logger *slog.Logger
}

// List 转调装载器：provider 名集由调用方显式传入（适配器**不**另读一次配置）。
// List forwards to the loader with the provider names passed in explicitly; the
// adapter never re-reads the configuration file.
func (p agentsPort) List(root string, providers []string) ([]core.AgentEntry, error) {
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: providers,
		Logger:    p.logger(),
	}).LoadAll(root)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Create 转调写模式创建（实现侧取实例锁）。
// Create forwards to the write-mode create, which takes the instance lock.
func (p agentsPort) Create(root, name string, providers []string) ([]string, error) {
	p.logger().Debug("Agent 工作区创建", "workdir", p.WorkDir, "workspace", root, "agent", name)

	created, err := workspace.CreateAgent(root, name, providers)
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Delete 转调写模式归档（实现侧取实例锁；now 由命令层显式传入）。
// Delete forwards to the write-mode archive with the caller's clock.
func (p agentsPort) Delete(root, name string, now time.Time) (string, error) {
	p.logger().Debug("Agent 工作区归档", "workdir", p.WorkDir, "workspace", root, "agent", name)

	archived, err := workspace.DeleteAgent(root, name, now)
	if err != nil {
		return "", err
	}
	return archived, nil
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

// ListSessions 转调会话清单（排序与去重是实现侧的事，端口保序）。
// ListSessions forwards the session list; ordering stays the implementation's call.
func (p sessionListerPort) ListSessions(ctx context.Context) ([]core.SessionRecord, error) {
	return p.store.ListSessions(ctx)
}

// Messages 转调一个会话的完整历史。
// Messages forwards one session's full history.
func (p sessionListerPort) Messages(ctx context.Context, sessionID string) ([]core.Message, error) {
	return p.store.Messages(ctx, sessionID)
}

// 编译期断言：端口形状与实现签名一旦对不上，在这里先红——不是在装配处报"缺方法"。
// Compile-time assertions: the ports and the implementations must match here first.
var (
	_ cli.RunService    = runServicePort{}
	_ cli.SessionLister = sessionListerPort{}
)
