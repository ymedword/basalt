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

	// OpenReadOnly 只读打开并做状态矩阵校验：只校验、不迁移、不取锁、不恢复；
	// 异常态经 core 的哨兵错误回报（ErrDatabaseMissing / ErrSchemaOutdated /
	// ErrSchemaTooNew / ErrSchemaCorrupt）。
	// OpenReadOnly validates and opens read-only; failure states travel as the
	// core sentinels.
	OpenReadOnly(ctx context.Context, path string, target int) (SchemaView, error)
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

// SecretsVerifyReport 是 `secrets verify` 的计数结果：只回检查与失败条数，明细只在
// 结构化日志里（WARN 点名资源与凭据 ID，仍不含材料）。
// SecretsVerifyReport counts checks and failures only.
type SecretsVerifyReport struct {
	Checked int
	Failed  int
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

	// Verify 逐条校验当前 active 托管凭据，只回计数。
	// Verify checks every active managed credential and reports counts only.
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
