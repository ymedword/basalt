package app

// 本文件是公共资源写面的编排层（颗粒度文档 §3.1.7 / 约束 1a·3·7·9·10·24–26·35·36·50）：
// 模型连接与 MCP server 的写入（新建 / 更新）、发布（含显式撤回）、连接测试与删除；
// 技能工作副本的创建 / 导入 / 更新 / 校验与不可变版本发布。引用 / 版本详情同文承载。
//
// 技能与另两类资源的存证口径有一处**刻意不同**：同名创建与工作副本分歧是预写之前的
// 409，**不留操作行**（图 D 两处"不写操作行"、失败表技能行"零写入"）；模型连接 / MCP
// 的重名则走"预写 → failed → 409"（失败表通用行）。差异来自技能以工作副本为写入对象、
// 没有可编辑的定义文件条目，判据面以卡片技能行为准。
//
// 每次写都按「预写 pending → 动目标 → 回填终态」编排。纯请求校验（资源名、If-Match
// 匹配、存在性、候选块解析与整份候选校验、运行时客户端构造）一律在 `BeginOperation`
// **之前**完成：400 / 404 / 412 不留操作行、零写入（约束 1a）；一旦写入 pending，
// 后续任何失败都回填终态并在错误 `data` 里带回可查的 `operation_id`。
//
// 锁序固定（约束 35 / 36）：资源锁 → 定义文件锁 → 重读最新文件 → 目标块分歧闸门 →
// 块编辑 → 原子写 → `ResourceGate` 写锁 → 同一事务写版本 / 指针 / 终态 → 运行时目录
// 原子替换（仅模型连接）→ 逆序释放。gate **只罩提交段**：候选解析与客户端构造不在锁内。
// This file orchestrates the model-connection and MCP server write surfaces: upsert,
// publish (including explicit revert), the connection test and delete; it also owns
// the skill work-copy surface (create / import / update / validate) and skill version
// publication. Every write pre-writes a pending operation before touching the target;
// pure request validation runs before that pre-write, so a rejected request leaves no
// operation row and no bytes behind. Skill duplicate creation and work-copy divergence
// are deliberate exceptions: they are 409s decided before the pre-write, with no
// operation row (card figure D and the skill row of the failure table).
// The lock order is resource lock → definition-file lock → re-read → divergence gate →
// block edit → atomic write → ResourceGate write lock → one transaction for version,
// pointer and terminal state → runtime catalog swap (model connections only); the gate
// covers the commit segment only.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// providersBlockKey 是 `basalt.yaml` 中模型连接块的顶层键。workspace 内同名常量
	// 未导出（它只是解析器的内部字段），本层做块定位与分歧闸门需要同一个字面量；
	// 取值与 `workspace.providersKey` 逐字一致，**不**引入第二种写法。
	// providersBlockKey mirrors the workspace literal for the providers block.
	providersBlockKey = "providers"

	// defaultModelTestTimeout 是模型连接测试的固定超时（约束 50）：15 秒。
	// defaultModelTestTimeout is the fixed 15-second connection-test budget.
	defaultModelTestTimeout = 15 * time.Second

	// modelTestPing 是测试调用唯一的一条最小消息：不接收、也不携带任何业务上下文。
	// modelTestPing is the only message ever sent by the connection test.
	modelTestPing = "ping"

	// result_code 的三个新取值（错误码表 §9.5）；OK / INTERNAL / BAD_REQUEST 复用包内
	// 既有常量（agent_console.go / agent_admin.go），不重复定义同一字面量。
	// Three new result codes; OK / INTERNAL / BAD_REQUEST reuse the package constants.
	resourceResultConflict      = "CONFLICT"
	resourceResultNotConfigured = "NOT_CONFIGURED"
	resourceResultTimeout       = "TIMEOUT"

	// unparseableAgentMarker 是"解析失败、按可能引用计入"的点名后缀——逐字取自颗粒度
	// 文档图 G（`点名 <agent>（unparseable）`），全角括号是卡片字面量，**不**改写。
	// unparseableAgentMarker is the card's literal marker for an unparseable definition.
	unparseableAgentMarker = "（unparseable）"

	// maxArchivedAgentBytes 是归档 `AGENT.md` 的读取上限：超过它按"解析失败"计入可能
	// 引用（fail-closed），不把异常大的文件读进内存。
	// maxArchivedAgentBytes bounds the archived definition read; oversized files count as
	// possible references rather than being loaded.
	maxArchivedAgentBytes = 1 << 20
)

// errResourceInternal 是资源写面内部失败的分类哨兵：web 层据此映射 500 并把
// `data.operation_id` 带回（约束 1a：pending 之后的失败必须可追查）。消息面只给
// 固定短语，底层错误进日志、不进响应。
// errResourceInternal classifies internal write failures; the underlying error is
// logged, never returned.
var errResourceInternal = errors.New("resource write failed")

// ModelConnectionRequest 是一次模型连接写入请求（web 层解码后原样交给编排层）。
// `Create=true` 表示 `POST /resources/model-connections`（新建），否则是带 If-Match 的
// 更新。`IfMatch` 的**语法**由 web 层校验；本层在资源锁内复核它是否仍等于当前发布
// 版本的强 ETag（约束 3：中间被别人发布过就 412，零写入）。
//
// 凭据三字段按契约 R4.8 解释：`APIKeySet` 区分"字段省略"（保留）与"显式给值"；
// `CredentialMode` 是显式声明的模式（空 = 省略）；`Scope` / `OwnerAgent` 只在**创建**
// 时生效，后续切换走 `/scope` 端点。
// ModelConnectionRequest is one upsert request. APIKeySet distinguishes an omitted
// credential field (keep) from an explicit value; scope is create-only.
type ModelConnectionRequest struct {
	Name         string
	BaseURL      string
	APIKey       string
	APIKeySet    bool
	DefaultModel string
	Create       bool
	IfMatch      string
	// CredentialMode 是请求显式声明的模式（`env` / `managed`，空 = 省略）。
	CredentialMode string
	// Scope / OwnerAgent 是创建时的使用范围（空 = shared；exclusive 必须给属主）。
	Scope      string
	OwnerAgent string
}

// MCPServerRequest 是一次 MCP server 写入请求（web 层解码后原样交给编排层）。
// 与模型连接不同，这里**没有**字段继承：PUT 是整条条目的全量替换（transport /
// command / args / url / env 都以本次请求为准），候选按约束 8 的严格模式校验。
// `Create=true` 表示 `POST /mcp-servers`；`IfMatch` 的语法由 web 层校验，本层在
// 资源锁内复核它是否仍等于当前发布版本的强 ETag（约束 3）。
// MCPServerRequest is one MCP server upsert; PUT replaces the whole entry and the
// candidate is validated in the strict mode of constraint 8.
type MCPServerRequest struct {
	Name      string
	Transport string
	Command   string
	Args      []string
	URL       string
	Env       map[string]string
	Create    bool
	IfMatch   string
}

// SkillCreateRequest 是一次技能骨架创建（`POST /skills`）：只给名字与一句话描述，
// 内容由本层渲染成最小 SKILL.md（frontmatter 的 name + description），正文留空。
// SkillCreateRequest is the skeleton-creation body; the surface renders the minimal
// SKILL.md itself instead of trusting caller-supplied content.
type SkillCreateRequest struct {
	Name        string
	Description string
}

// SkillImportRequest 是一次目录导入（`POST /skills/import`）：全部文件由调用方给出，
// 边界（路径、UTF-8、大小、数量、大小写重复）由工作空间的同一校验函数裁决。
// SkillImportRequest carries the text files read by the caller; the workspace gate
// decides every boundary.
type SkillImportRequest struct {
	Name  string
	Files []core.AuthorFile
}

// SkillUpdateRequest 是一次全量替换（`PUT /skills/{name}`）：未列出的文件被删除。
// `Name` 与 `IfMatch` 由装配层从路径与请求头填入（app 层不解析 HTTP）；`IfMatch`
// 缺失按 412 处理（技能写的失败判据表把缺失归入 412，与发布路径不同）。
// SkillUpdateRequest replaces the work copy wholesale; `Name`/`IfMatch` are filled by
// the assembly layer from the path and headers.
type SkillUpdateRequest struct {
	Name    string
	Files   []core.AuthorFile
	IfMatch string
}

// SkillDetail 是一次技能写操作的结果视图（`SkillDetail` = `SkillView` + 文件清单）：
// web 层只做投影，不重复定义领域类型。`Description` 在写入面有唯一来源（本次请求 /
// 候选卡片），与只读面受 I-60 限制保持空串的口径不同。
// SkillDetail is the write surface's skill view: the write path knows the description,
// unlike the read-only projection which has no reachable source (I-60).
type SkillDetail struct {
	Name         string
	Description  string
	Path         string
	VersionID    int64
	VersionNo    int
	ContentHash  string
	ETag         string
	ReferencedBy []string
	Files        []workspace.SkillFile
}

// ResourceAdminDeps 是资源写面的全部依赖。构造后不再变化；除 Listed 的注入位外，
// 一律由装配层传入生产实现。
// ResourceAdminDeps carries every dependency of the resource write surface.
type ResourceAdminDeps struct {
	// ConfigPath 是 `basalt.yaml` 的路径：模型连接块就写在这份文件里。
	ConfigPath string
	// Root 是工作区根目录（`<workDir>/.basalt`）：删除闸门的归档扫描在
	// `<Root>/archive/*/AGENT.md` 上进行。空串表示没有可扫描的归档（测试夹具缺省为空
	// 目录，生产装配由 wire 传入）。
	Root string
	// Agents 返回当前 Agent 装载快照（含未注册与解析失败的条目）：删除闸门据此点名
	// 活动 Agent；解析失败按"可能引用"计入（约束 11，fail-closed）。nil 视为零 Agent。
	Agents func() ([]core.AgentEntry, error)
	// ProviderOverride 是评测注入的替身客户端：删除模型连接重建候选目录时每个已发布
	// 名各建一条替身，不解析环境变量、不构造真实客户端（与启动装配同一份注入值）。
	ProviderOverride core.ProviderClient
	// Store 是版本 / 指针仓储（US-11 的同一份实现，**不**另建资源存储）。
	Store *storage.ResourceStore
	// Operations 是管理操作仓储：预写 pending、回填终态。
	Operations core.OperationStore
	// Gate 必须与运行准入共用同一个实例（约束 16）：发布持写锁、准入持读锁。
	Gate *core.ResourceGate
	// Catalog 是运行时生效目录的原子持有者。nil 时退化为私有空目录——那只防本实例
	// 自身，**不**提供与准入的共享，生产装配必须传共享实例。
	Catalog *providerCatalogStore
	// Logger 记录发布事务失败、补偿失败等"变更可能已发生"的 ERROR。
	Logger *slog.Logger
	// Now 是操作标识与时间戳的唯一时钟来源。
	Now func() time.Time
	// PublishVersion 是「同事务写版本 / 指针 / 终态」的提交入口，缺省
	// `Store.PublishWithVersionState`。暴露为注入位只为失败路径判据（发布失败必须能在
	// 事务内失败），**不是**第二套实现：生产装配只传缺省值。`applied` 区分两类发布：
	// 模型连接发布后立即生效（true），MCP 发布要等重启（false，保留旧的实际生效指针，
	// 约束 9）。
	PublishVersion func(ctx context.Context, v core.ResourceVersion, applied bool, operationID string, now time.Time) (core.ResourceVersion, error)
	// Supervisor 是 MCP 连接生命周期（约束 18–20）：删除 MCP 时在 gate 写锁内断开连接。
	// nil 表示本进程没有 MCP 运行时（T049 装配前 / 测试夹具），删除跳过 `Stop`——
	// 此时也不存在由本进程建立的连接。
	Supervisor *MCPSupervisor
	// ModelTestTimeout 是模型连接测试的固定超时：缺省 15 秒（约束 50），测试可缩短。
	ModelTestTimeout time.Duration
	// Credentials 是托管凭据仓储：条件 ETag 的凭据段（FR-081）与后续凭据写路径的
	// 唯一读取点。nil 表示本次装配没有托管凭据（CLI 轻路径 / 半截夹具），此时 ETag
	// 只有内容与身份段——生产装配必须传共享实例。
	Credentials *storage.CredentialStore
	// CredentialSource 是删除模型连接后按剩余已发布集合重建目录时的托管凭据解析面：
	// 剩余条目里可能有 `credential_mode: managed`。nil 表示本次装配没有托管凭据能力，
	// 遇到 managed 条目时重建失败关闭（目录不换、定义文件按身份检查回滚），不猜值。
	CredentialSource managedCredentialSource
	// CredentialWriter 是保存路径上的托管凭据写入面（`*CredentialOps`）：managed 保存
	// 在发布定义**之后**写凭据（目标绑定要读新定义），env 切换用它退役身份。
	// nil 表示本次装配没有凭据写面：managed 保存一律拒绝（fail-closed），env 照常。
	CredentialWriter CredentialSaveWriter
	// Scopes 是使用范围转换编排：创建时声明 `scope: exclusive` 在发布之后按同一服务
	// 转换（指针默认 shared，收缩属主必须走同一条校验路径）。nil 时专属创建拒绝。
	Scopes *ScopeService
	// Bindings 是绑定投影（`agent_resource_bindings`）的反查面：引用视图的
	// `bound_agents` 只从这张投影取（R4.6 绑定权威），不按名称猜。nil 表示本次装配
	// 没有投影（CLI 轻路径 / 半截夹具），引用视图只给定义名引用。
	// Bindings reverse-looks-up the binding projection for the references view.
	Bindings NotifyBindingReferrer
	// Archives 是归档索引：绑定者是否已归档由此判定（归档 Agent 也参与投影重建，
	// 仍是合法引用者）。nil 时 `archived` 恒 false。
	// Archives tells whether a bound agent is archived; nil keeps every flag false.
	Archives ArchiveLister
	// ConnectionTests 是「最后一次连接测试」投影仓储（schema v4）：模型连接测试的
	// 结论与时间在这里落一份，列表页因此能区分"从未测试 / 测得怎样"。nil 表示本次
	// 装配不保留投影——历史仍在管理操作台账，测试照常执行。
	// ConnectionTests is the last-test projection; a nil port only skips the projection.
	ConnectionTests core.ResourceConnectionTestStore
	// DiagnoseMCP 是 MCP 只读依赖诊断的命令定位面（契约 C-04）：装配层绑定与启动共用的
	// `tool.NewMCPCommand` 构造器，诊断不自己另写一套路径解析。nil 时诊断把命令答成
	// `unconfirmed / unknown` 并留 WARN——装配缺失不得伪装成"已就绪"。
	// DiagnoseMCP is the stat-only command locator shared with launch; nil degrades to
	// unconfirmed with a warning rather than faking a positive verdict.
	DiagnoseMCP func(core.MCPServerConfig) core.MCPCommandDiagnosis
}

// ResourceAdmin 是公共资源写面的编排实现；字段全部私有，构造后不再变化。
// ResourceAdmin implements the resource write surface; its fields are fixed at construction.
type ResourceAdmin struct {
	configPath       string
	root             string
	agents           func() ([]core.AgentEntry, error)
	providerOverride core.ProviderClient
	store            *storage.ResourceStore
	operations       core.OperationStore
	gate             *core.ResourceGate
	catalog          *providerCatalogStore
	supervisor       *MCPSupervisor
	credentials      *storage.CredentialStore
	credentialSource managedCredentialSource
	credentialWriter CredentialSaveWriter
	scopes           *ScopeService
	bindings         NotifyBindingReferrer
	archives         ArchiveLister
	connectionTests  core.ResourceConnectionTestStore
	diagnoseMCP      func(core.MCPServerConfig) core.MCPCommandDiagnosis
	logger           *slog.Logger
	now              func() time.Time
	publishVersion   func(ctx context.Context, v core.ResourceVersion, applied bool, operationID string, now time.Time) (core.ResourceVersion, error)
	testTimeout      time.Duration

	// 两把按键互斥表：资源锁按 `(kind, id)`，文件锁按定义文件路径。锁序固定为
	// 资源锁 → 文件锁（约束 35）；两张表都是本实例私有的纯进程内互斥。
	// Two keyed mutex tables; the resource lock is always acquired before the file lock.
	resourceLocks *resourceLockTable
	fileLocks     *resourceLockTable
}

// resourceLockTable 是进程内的按键互斥表：同一个键串行、不同键并行。表项按见过的键
// 增长（键集 = 资源数 / 定义文件数，天然有界），取锁与解锁都在表自身的短锁内完成。
// resourceLockTable serializes callers per key and never across keys.
type resourceLockTable struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lock 取得键对应的互斥量并返回解锁函数；调用方 defer 它。
// lock acquires the key's mutex and returns the unlock function.
func (t *resourceLockTable) lock(key string) func() {
	t.mu.Lock()
	if t.locks == nil {
		t.locks = make(map[string]*sync.Mutex)
	}
	mu, ok := t.locks[key]
	if !ok {
		mu = &sync.Mutex{}
		t.locks[key] = mu
	}
	t.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// LockResource 把**同一张**按键互斥表交给相邻编排（撤销 / 范围切换）：键与写路径
// 同为 `resourceLockKey(kind, 名称)`，因此两条路径对同一资源串行。**不得**另建第二套
// 互斥（FR-041）；返回的解锁函数由调用方 defer。
// LockResource hands the very same keyed mutex table to adjacent orchestrations, so
// revoke/scope serializes against resource writes without a second mutex mechanism.
func (s *ResourceAdmin) LockResource(kind core.ResourceKind, id string) func() {
	return s.resourceLocks.lock(resourceLockKey(kind, id))
}

// NewResourceAdmin 组装资源写面；Gate / Catalog / Logger / Now / PublishVersion 为 nil
// 时回落安全缺省值（Gate 退化为独立实例——只防本实例自身并发，生产装配必须传共享
// 实例；Catalog 退化为空目录，发布后新 Run **不会**看到新映射）。
// NewResourceAdmin assembles the write surface; nil dependencies fall back to safe
// defaults, and production assembly must pass the shared gate and catalog.
func NewResourceAdmin(deps ResourceAdminDeps) *ResourceAdmin {
	gate := deps.Gate
	if gate == nil {
		gate = &core.ResourceGate{}
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	catalog := deps.Catalog
	if catalog == nil {
		catalog = &providerCatalogStore{}
		catalog.Store(&providerCatalog{
			clients: map[string]core.ProviderClient{},
			models:  map[string]string{},
		})
	}
	publishVersion := deps.PublishVersion
	if publishVersion == nil {
		publishVersion = deps.Store.PublishWithVersionState
	}
	timeout := deps.ModelTestTimeout
	if timeout <= 0 {
		timeout = defaultModelTestTimeout
	}
	return &ResourceAdmin{
		configPath:       deps.ConfigPath,
		root:             deps.Root,
		agents:           deps.Agents,
		providerOverride: deps.ProviderOverride,
		store:            deps.Store,
		operations:       deps.Operations,
		gate:             gate,
		catalog:          catalog,
		supervisor:       deps.Supervisor,
		credentials:      deps.Credentials,
		credentialSource: deps.CredentialSource,
		credentialWriter: deps.CredentialWriter,
		scopes:           deps.Scopes,
		bindings:         deps.Bindings,
		archives:         deps.Archives,
		connectionTests:  deps.ConnectionTests,
		diagnoseMCP:      deps.DiagnoseMCP,
		logger:           logger,
		now:              now,
		publishVersion:   publishVersion,
		testTimeout:      timeout,
		resourceLocks:    &resourceLockTable{},
		fileLocks:        &resourceLockTable{},
	}
}

// UpsertModelConnection 写入一条模型连接并**立即发布**（新建 / 更新同一语义）：
//
//	静态校验 → 资源锁 →（更新：指针 404 / If-Match 412 / 空 api_key 继承 / 范围只读）→
//	模式与凭据值解析 → 文件锁 → 重读 →（更新：分歧闸门 409 / 新建：块内重名）→ 块编辑 +
//	整份候选校验 + 环境占位解析与客户端构造（全部在预写之前）→（托管写：主密钥预检）→
//	预写 pending →（新建重名：409 + 操作 failed）→ 原子写 → gate 写锁内同事务写版本 /
//	指针 / 终态 → 目录原子替换 →（创建专属：范围转换）→（托管写：凭据写入）→ 逆序释放。
//
// 契约 7 的两处分层：`api_key` 只接受精确 `${VAR}` 占位（明文 / 前缀 / 嵌套一律 400），
// 候选校验**不**解析无关段的环境变量；只有本资源条目的占位在构造运行时客户端时解析，
// 缺失 / 空值 → 400 点名变量名、零写入。
//
// 托管凭据（R4.8）在同一保存动作里完成，**不**新开端点：先发布定义（凭据写面要读新
// 定义的 `base_url` 与 `credential_mode`），再写凭据；凭据写面自己预写
// `model_connection.credential.write` 操作行并推进资源 revision。
// UpsertModelConnection writes and immediately publishes one model connection. Managed
// credentials are written in the same save action, after the definition publish, so the
// target binding sees the new base_url; the credential half keeps its own action row.
func (s *ResourceAdmin) UpsertModelConnection(ctx context.Context, actor core.Actor, req ModelConnectionRequest) (core.ResourceVersion, error) {
	action := core.ActionModelConnectionUpdate
	if req.Create {
		action = core.ActionModelConnectionCreate
	}
	if err := validateResourceID(req.Name); err != nil {
		return core.ResourceVersion{}, &core.InvalidInputError{Tool: action, Reason: err.Error()}
	}

	unlockResource := s.resourceLocks.lock(resourceLockKey(core.ResourceModelConnection, req.Name))
	defer unlockResource()

	var (
		published   core.ResourceVersion
		publishedIn workspace.ProviderEntry
		current     string
		currentMode = core.CredentialModeEnv
		requested   = core.ScopeShared
		duplicate   bool
		uid         core.ResourceUID
	)
	if req.Create {
		// 专属范围在写行之前解析属主：创建失败零残余，不允许"先建共享再转换"。
		scope, _, scopeErr := s.requestedCreateScope(ctx, action, req.Name, req.Scope, req.OwnerAgent)
		if scopeErr != nil {
			return core.ResourceVersion{}, scopeErr
		}
		requested = scope

		exists, err := s.resourceExists(ctx, core.ResourceModelConnection, req.Name)
		if err != nil {
			return core.ResourceVersion{}, err
		}
		duplicate = exists
		// 新建（含删除后的同名重建）一律分配新身份：旧 UID 的墓碑与历史不再被接管。
		if !duplicate {
			uid, err = newResourceUID(core.ResourceModelConnection, req.Name)
			if err != nil {
				return core.ResourceVersion{}, err
			}
		}
	} else {
		pointer, err := s.activePointer(ctx, core.ResourceModelConnection, req.Name)
		if err != nil {
			return core.ResourceVersion{}, err
		}
		uid, err = resourceUIDOfPointer(core.ResourceModelConnection, req.Name, pointer)
		if err != nil {
			return core.ResourceVersion{}, err
		}
		published, err = s.store.VersionByID(ctx, pointer.PublishedVersionID)
		if err != nil {
			return core.ResourceVersion{}, fmt.Errorf("读取模型连接 %s 的发布版本: %w", req.Name, err)
		}
		// 条件写：缺失 / 弱 / `*` 在 web 层已 412；这里判值。空串在本层同样算缺失
		// （更新必须带 If-Match），不匹配 → 412 并给出当前值，零写入、零操作行。
		// 当前值按 FR-081 组成（含范围与凭据状态），不是纯内容哈希。
		current, err = publishedResourceETag(ctx, s.credentials, pointer, published)
		if err != nil {
			return core.ResourceVersion{}, err
		}
		if req.IfMatch != current {
			return core.ResourceVersion{}, preconditionError(current)
		}
		publishedIn, err = providerEntryFromVersion(req.Name, published)
		if err != nil {
			return core.ResourceVersion{}, fmt.Errorf("读取模型连接 %s 的已发布条目: %w", req.Name, err)
		}
		// 当前模式以**已发布定义**的 `credential_mode` 为权威（缺省 = env）：运行对象由
		// 定义构建，模式继承必须与运行对象同一来源，不能凭身份表推断。
		currentMode, err = credentialModeOfEntry(req.Name, publishedIn)
		if err != nil {
			return core.ResourceVersion{}, &core.InvalidInputError{Tool: action, Reason: err.Error()}
		}
		if err := s.checkScopeOnUpdate(ctx, action, req, pointer); err != nil {
			return core.ResourceVersion{}, err
		}
	}

	plan, err := planCredentialSave(action, "模型连接 "+req.Name, req.CredentialMode,
		req.APIKey, req.APIKeySet, currentMode, req.Create)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	// 改 base_url 必须录新 Key：目标绑定跟着地址走，旧密文在新地址上不可用（R4.8）。
	if !req.Create && plan.Mode == core.CredentialModeManaged && plan.Value == nil &&
		strings.TrimSpace(publishedIn.BaseURL) != strings.TrimSpace(req.BaseURL) {
		return core.ResourceVersion{}, &core.InvalidInputError{
			Tool:   action,
			Reason: "模型连接 " + req.Name + ": 变更 base_url 必须同时录入新 Key",
		}
	}

	entry := workspace.ProviderEntry{
		Name:           req.Name,
		BaseURL:        req.BaseURL,
		DefaultModel:   req.DefaultModel,
		CredentialMode: string(plan.Mode),
	}
	switch {
	case plan.Mode == core.CredentialModeManaged:
		// managed 的定义里不得有 api_key：明文只走凭据写面，绝不落配置文件。
	case req.APIKeySet:
		entry.APIKey = req.APIKey
	default:
		entry.APIKey = publishedIn.APIKey
	}
	rendered, err := workspace.RenderProviderEntry(entry)
	if err != nil {
		return core.ResourceVersion{}, &core.InvalidInputError{Tool: action, Reason: err.Error()}
	}

	unlockFile := s.fileLocks.lock(s.configPath)
	defer unlockFile()

	original, err := os.ReadFile(s.configPath)
	if err != nil {
		return core.ResourceVersion{}, fmt.Errorf("读取模型连接定义 %s: %w", s.configPath, err)
	}
	blocks, err := workspace.BlockEntries(original, providersBlockKey)
	if err != nil {
		s.logger.Warn("模型连接定义块不可安全编辑", "resource", req.Name, "action", action, "error", err)
		return core.ResourceVersion{}, editUnsupportedError()
	}

	var candidate, entryRaw []byte
	var client core.ProviderClient
	if req.Create {
		if !duplicate && blockEntryExists(blocks, req.Name) {
			duplicate = true
		}
		if !duplicate {
			candidate, err = workspace.AppendEntry(original, providersBlockKey, rendered)
			if err != nil {
				s.logger.Warn("模型连接块追加失败", "resource", req.Name, "error", err)
				return core.ResourceVersion{}, editUnsupportedError()
			}
		}
	} else {
		// 分歧闸门（约束 34）：文件里目标块的当前字节必须仍是已发布版本的字节。
		// 两种已落库形态都接受（基线写入的条目映射 / 管理写路径的块原文），哈希只对
		// 存储时的同一份字节求值，不做任何再序列化。
		if !definitionBlockMatches(original, req.Name, published) {
			s.logger.Warn("定义文件与已发布版本不一致，拒绝覆盖",
				"resource", req.Name, "expected_etag", current)
			return core.ResourceVersion{}, &core.AdminError{
				Kind:    core.ErrConflict,
				Message: "定义文件与已发布版本不一致，请先对账或重新读取后再保存",
			}
		}
		var replaceLost bool
		candidate, replaceLost, err = workspace.ReplaceEntry(original, providersBlockKey, req.Name, rendered)
		if err != nil {
			s.logger.Warn("模型连接块替换失败", "resource", req.Name, "error", err)
			return core.ResourceVersion{}, editUnsupportedError()
		}
		if replaceLost {
			// 条目内注释无法在整条替换中保留：**不写文件**、400 EDIT_UNSUPPORTED
			// （失败判据表要求文件逐字节不变）。因此成功路径的 detail_json 里
			// comments_lost 恒为 false——"会丢注释"的写入根本到不了提交段。
			s.logger.Warn("模型连接条目内含注释，整条替换会丢失它们", "resource", req.Name)
			return core.ResourceVersion{}, editUnsupportedError()
		}
	}

	if !duplicate {
		// 整份候选校验：覆盖全文件而不只是目标条目——块外内容若被破坏必须在这里被拒。
		if _, err := workspace.ValidateProviderEntries(candidate); err != nil {
			return core.ResourceVersion{}, &core.InvalidInputError{Tool: action, Reason: err.Error()}
		}
		raw, ok := entryBlockRaw(candidate, providersBlockKey, req.Name)
		if !ok {
			return core.ResourceVersion{}, fmt.Errorf("块编辑后未找到模型连接 %s 的条目", req.Name)
		}
		entryRaw = raw
		// 运行时客户端按凭据模式构建（R4.8）：env 在写盘前解析本条目占位——缺失 /
		// 空值 → 400 点名变量名、零写入（约束 7）；managed 的明文只交给凭据写面，新值
		// 要等凭据落库后再按**已发布版本**构建（`adoptManagedClient`），保留旧值时沿用
		// 目录里的同一客户端——地址变了必须录新 Key 已在上方拒绝，客户端与版本仍同源。
		// The runtime client follows the credential mode: env builds from the `${VAR}`
		// placeholder before the file write; managed builds after the credential lands.
		switch {
		case plan.Mode == core.CredentialModeManaged && plan.Value == nil:
			if catalog := s.catalog.Load(); catalog != nil {
				client = catalog.clients[req.Name]
			}
		case plan.Mode == core.CredentialModeManaged:
			// 推迟到凭据写入之后；本次不向目录换入任何客户端。
		default:
			client, err = buildModelConnectionClient(req.Name, entry)
			if err != nil {
				return core.ResourceVersion{}, &core.InvalidInputError{Tool: action, Reason: err.Error()}
			}
		}
	}

	// 凭据写面在发布之后（目标绑定要读新定义），但主密钥能否装载必须在写盘之前判：
	// 不可用 → 503，定义文件与发布指针零变化。退役不需要主密钥（只改状态）。
	if plan.needsWrite() {
		if s.credentialWriter == nil {
			return core.ResourceVersion{}, credentialUnavailableError(
				"模型连接 " + req.Name + ": 本次装配没有凭据写面，拒绝保存托管凭据")
		}
		if plan.Value != nil {
			if err := s.credentialWriter.Preflight(); err != nil {
				return core.ResourceVersion{}, err
			}
		}
	}

	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   core.TargetKindModelConnection,
		TargetID:     req.Name,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  modelConnectionRequestHash(action, req, plan.Mode),
		ExpectedETag: req.IfMatch,
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := s.operations.BeginOperation(ctx, record); err != nil {
		s.logger.Error("模型连接写预写操作失败，拒绝执行", "resource", req.Name, "action", action, "error", err)
		return core.ResourceVersion{}, fmt.Errorf("预写管理操作失败: %w", err)
	}

	if duplicate {
		// 失败判据表把「同名资源已存在」归为 409 + **操作 failed**：预写之前只做纯
		// 请求校验（1a 的 400/404/412 面不含 409），因此重名判定放在预写之后，
		// 结果是一条可查的失败行 + 零文件改动。
		s.finish(ctx, operationID, core.OperationFailed, resourceResultConflict, "同名模型连接已存在")
		return core.ResourceVersion{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: fmt.Sprintf("模型连接 %s 已存在", req.Name),
			Data:    map[string]any{"operation_id": operationID},
		}
	}

	if err := workspace.CommitProviderDefinition(s.configPath, candidate); err != nil {
		s.logger.Error("写入模型连接定义失败", "resource", req.Name, "operation_id", operationID, "error", err)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "写入定义文件失败")
		return core.ResourceVersion{}, operationFailedError(operationID, "写入模型连接定义失败")
	}

	version, err := s.commitModelConnection(ctx, operationID, now, s.configPath, candidate, original,
		core.ResourceVersion{
			Kind:           core.ResourceModelConnection,
			ResourceUID:    uid,
			ResourceID:     req.Name,
			ContentHash:    baselineResourceHash(string(core.ResourceModelConnection), req.Name, entryRaw),
			DefinitionYAML: string(entryRaw),
			OperationID:    operationID,
		}, req.Name, client, entry.DefaultModel)
	if err != nil {
		return core.ResourceVersion{}, err
	}

	// 创建即专属：发布指针的默认范围是 shared（`publishTx` 的 ON CONFLICT 也保留既有
	// 范围），收缩属主必须走范围转换的同一条校验路径，不在这里另写一次 UPDATE。
	if requested == core.ScopeExclusive {
		if _, err := s.scopes.Convert(ctx, ScopeRequest{
			Kind:        core.ResourceModelConnection,
			Name:        req.Name,
			ResourceUID: uid,
			Scope:       core.ScopeExclusive,
			OwnerAgent:  req.OwnerAgent,
			OperationID: operationID,
		}); err != nil {
			// 定义与版本已提交，范围仍是 shared：如实报告，由页面重新读取后经 /scope 补做。
			s.logger.Error("模型连接已发布但范围转换失败",
				"resource", req.Name, "operation_id", operationID, "error", err)
			return version, err
		}
	}

	// 凭据段最后执行：它推进资源 revision（换 Key 内容哈希不变也要换 ETag），
	// 失败时定义与凭据状态不一致，错误原样上抛并由页面重新读取。
	if _, err := writeSavedCredential(ctx, s.credentialWriter, actor,
		core.BindingModelConnection, req.Name, uid, plan); err != nil {
		s.logger.Error("模型连接定义已发布但凭据写入失败",
			"resource", req.Name, "operation_id", operationID, "error", err)
		return version, err
	}
	// 新 Key 落库之后才能构建运行时客户端：目录里的客户端必须同时对应已发布版本与
	// 已存的 active 凭据版本（改地址不换 Key 的路径已在前面拒绝）。
	if plan.Mode == core.CredentialModeManaged && plan.Value != nil {
		if err := s.adoptManagedClient(ctx, req.Name, version, entry.DefaultModel); err != nil {
			s.logger.Error("模型连接已保存但运行时客户端构建失败",
				"resource", req.Name, "operation_id", operationID, "error", err)
			return version, err
		}
	}
	return version, nil
}

// adoptManagedClient 在凭据落库之后按**已发布版本**构建运行时客户端并原子换入目录
// （约束 3 / 16）：Key 取自本次写入产生的 active 版本，地址取自版本定义——与启动 /
// 重建路径（`newProviderCatalogFromPublished` → `catalogAPIKey`）同源，不读请求参数。
// 构建失败时目录保持旧表：定义与凭据已经提交，错误原样上抛由页面重新读取；下次启动
// 按同一路径重建，目标地址与凭据不一致时失败关闭。
// adoptManagedClient builds the runtime client from the published version after the
// credential write; it mirrors the startup rebuild path and never reads the request.
func (s *ResourceAdmin) adoptManagedClient(ctx context.Context, name string,
	version core.ResourceVersion, model string,
) error {
	if s.credentialSource == nil {
		return credentialUnavailableError(
			"模型连接 " + name + "：本次装配没有托管凭据解析面，无法构建运行时客户端")
	}
	entry, err := providerEntryFromVersion(name, version)
	if err != nil {
		return fmt.Errorf("读取模型连接 %s 的已发布条目: %w", name, err)
	}
	key, err := s.credentialSource.ResolveModelCredential(ctx, core.ResourceVersionRef{
		Name:        name,
		ResourceUID: version.ResourceUID,
		VersionID:   version.ID,
		VersionNo:   version.VersionNo,
		ContentHash: version.ContentHash,
	})
	if err != nil {
		return fmt.Errorf("模型连接 %s 的托管凭据不可用: %w", name, err)
	}
	if key == "" {
		return fmt.Errorf("模型连接 %s 的托管凭据解析为空", name)
	}
	client, err := provider.NewOpenAIClient(provider.Options{
		Name:    name,
		BaseURL: entry.BaseURL,
		APIKey:  key,
	})
	if err != nil {
		return fmt.Errorf("模型连接 %s: %w", name, err)
	}
	s.gate.Lock()
	s.catalog.Store(s.catalog.Load().with(name, client, model))
	s.gate.Unlock()
	return nil
}

// requestedCreateScope 解析创建请求声明的使用范围：缺省 shared；exclusive 必须给
// 属主，属主按范围转换的同一口径解析（存在、可证明身份、未归档）。形状错误是 400，
// 属主解析失败沿用 404 / 409。写到行上的范围仍是发布指针的默认 shared，由发布之后的
// `ScopeService.Convert` 转换——**不**在这里另开一条 UPDATE。
// requestedCreateScope validates the create-time scope request; the actual conversion
// still runs through ScopeService after the publish.
func (s *ResourceAdmin) requestedCreateScope(ctx context.Context, action, name, rawScope,
	owner string,
) (core.ResourceScope, core.AgentUID, error) {
	raw := strings.TrimSpace(rawScope)
	owner = strings.TrimSpace(owner)
	if raw == "" && owner == "" {
		return core.ScopeShared, "", nil
	}
	if s.scopes == nil {
		return "", "", errors.New("资源写面缺少范围转换服务，拒绝创建专属资源")
	}
	return s.scopes.RequestedScope(ctx, action, name, raw, owner)
}

// checkScopeOnUpdate 拒绝在更新里切换使用范围（判定与提示在 `checkScopeUpdate`）。
// checkScopeOnUpdate delegates to the shared scope-update guard.
func (s *ResourceAdmin) checkScopeOnUpdate(ctx context.Context, action string,
	req ModelConnectionRequest, pointer core.ResourcePointer,
) error {
	return checkScopeUpdate(ctx, s.scopes, action, req.Scope, req.OwnerAgent,
		scopeOfPointer(pointer), pointer.OwnerAgentUID)
}

// credentialModeOfEntry 读一条已发布条目的凭据模式：空串 = env（v2 定义兼容），
// 其余只接受两个契约字面量。与清单侧 `credentialModeOf` 同一口径，只是入参是条目。
// credentialModeOfEntry reads the declared mode of a published provider entry.
func credentialModeOfEntry(name string, entry workspace.ProviderEntry) (core.CredentialMode, error) {
	raw := strings.TrimSpace(entry.CredentialMode)
	if raw == "" {
		return core.CredentialModeEnv, nil
	}
	mode, err := core.ParseCredentialMode(raw)
	if err != nil {
		return "", fmt.Errorf("模型连接 %s 的凭据模式非法: %w", name, err)
	}
	return mode, nil
}

// credentialUnavailableError 是保存期 503 `CREDENTIAL_UNAVAILABLE` 的固定载体：
// 文案只说明能力缺失，不含路径、密钥或任何值。
// credentialUnavailableError is the fixed 503 CREDENTIAL_UNAVAILABLE carrier.
func credentialUnavailableError(message string) error {
	return &core.AdminError{Kind: core.ErrCredentialUnavailable, Message: message}
}

// isCredentialSentinel 判断错误是否属于凭据与使用范围的固定类别（与 web 层
// `writeCredentialError` 同一集合）。命中时**不得**再包成 InvalidInputError：通用
// 400 分支会抢先映射，丢掉契约专用码（保存期 400 / 409 / 503）。
// isCredentialSentinel reports whether err carries one of the credential sentinels
// that the web layer maps to dedicated contract codes.
func isCredentialSentinel(err error) bool {
	return errors.Is(err, core.ErrCredentialModeRequired) ||
		errors.Is(err, core.ErrCredentialRevoked) ||
		errors.Is(err, core.ErrResourceScopeInUse) ||
		errors.Is(err, core.ErrCredentialOwnerMismatch) ||
		errors.Is(err, core.ErrCredentialUnavailable)
}

// UpsertMCPServer 写入一条 MCP server 并**立即发布**（新建 / 更新同一语义，图 C）：
//
//	严格候选校验（约束 8）→ 资源锁 →（更新：指针 404 / If-Match 412）→ 文件锁 →
//	重读（新建允许文件缺失）→（更新：分歧闸门 409 / 新建：块内重名）→ 块编辑 +
//	整份候选校验（全部在预写之前）→ 预写 pending →（新建重名：409 + 操作 failed）→
//	原子写 → gate 写锁内同事务写版本 / 指针 / 终态（applied 不前进）→ 200
//	`pending_restart=true`。supervisor 不在写路径上：生效发生在下一次成功连接。
//
// 候选严格性只作用于被编辑的条目；整份文件校验走兼容取径，未触碰的旧条目（含旧多词
// command）不会因旁邻被拒。反之，编辑一条旧多词 command 条目时，候选由严格规范化
// 渲染，会以 400 指向 args 字段（约束 47 的迁移要求）。
// UpsertMCPServer writes and immediately publishes one MCP server (figure C); the
// supervisor stays off the write path because applying happens on the next connect.
func (s *ResourceAdmin) UpsertMCPServer(ctx context.Context, actor core.Actor, req MCPServerRequest) (core.ResourceVersion, error) {
	action := core.ActionMCPServerUpdate
	if req.Create {
		action = core.ActionMCPServerCreate
	}
	if err := validateResourceID(req.Name); err != nil {
		return core.ResourceVersion{}, &core.InvalidInputError{Tool: action, Reason: err.Error()}
	}

	rendered, err := workspace.RenderMCPServerEntry(core.MCPServerConfig{
		Name:      req.Name,
		Transport: req.Transport,
		Command:   req.Command,
		Args:      req.Args,
		URL:       req.URL,
		Env:       req.Env,
	})
	if err != nil {
		// 候选非法 → 400 点名字段，预写之前返回：零写入、零操作行。
		return core.ResourceVersion{}, &core.InvalidInputError{Tool: action, Reason: err.Error()}
	}

	unlockResource := s.resourceLocks.lock(resourceLockKey(core.ResourceMCPServer, req.Name))
	defer unlockResource()

	var (
		published core.ResourceVersion
		current   string
		duplicate bool
		uid       core.ResourceUID
	)
	if req.Create {
		exists, err := s.resourceExists(ctx, core.ResourceMCPServer, req.Name)
		if err != nil {
			return core.ResourceVersion{}, err
		}
		duplicate = exists
		if !duplicate {
			uid, err = newResourceUID(core.ResourceMCPServer, req.Name)
			if err != nil {
				return core.ResourceVersion{}, err
			}
		}
	} else {
		pointer, err := s.activePointer(ctx, core.ResourceMCPServer, req.Name)
		if err != nil {
			return core.ResourceVersion{}, err
		}
		uid, err = resourceUIDOfPointer(core.ResourceMCPServer, req.Name, pointer)
		if err != nil {
			return core.ResourceVersion{}, err
		}
		published, err = s.store.VersionByID(ctx, pointer.PublishedVersionID)
		if err != nil {
			return core.ResourceVersion{}, fmt.Errorf("读取 MCP server %s 的发布版本: %w", req.Name, err)
		}
		current, err = publishedResourceETag(ctx, s.credentials, pointer, published)
		if err != nil {
			return core.ResourceVersion{}, err
		}
		if req.IfMatch != current {
			return core.ResourceVersion{}, preconditionError(current)
		}
	}

	definitionPath := s.mcpServersPath()
	unlockFile := s.fileLocks.lock(definitionPath)
	defer unlockFile()

	original, err := readFileUnder(filepath.Dir(definitionPath), filepath.Base(definitionPath))
	if errors.Is(err, fs.ErrNotExist) && req.Create {
		// 没有 mcp_servers.yaml 时新建 = 创建文件并写入块；AppendEntry 对 nil 文档
		// 走"创建块"路径（`servers:` 顶层键 + 单条目）。
		original, err = nil, nil
	}
	if err != nil {
		return core.ResourceVersion{}, fmt.Errorf("读取 MCP 定义 %s: %w", definitionPath, err)
	}
	blocks, err := workspace.BlockEntries(original, mcpServersDefinitionsKey)
	if err != nil {
		s.logger.Warn("MCP 定义块不可安全编辑", "resource", req.Name, "action", action, "error", err)
		return core.ResourceVersion{}, editUnsupportedError()
	}

	var candidate, entryRaw []byte
	if req.Create {
		if !duplicate && blockEntryExists(blocks, req.Name) {
			duplicate = true
		}
		if !duplicate {
			candidate, err = workspace.AppendEntry(original, mcpServersDefinitionsKey, rendered)
			if err != nil {
				s.logger.Warn("MCP server 块追加失败", "resource", req.Name, "error", err)
				return core.ResourceVersion{}, editUnsupportedError()
			}
		}
	} else {
		// 分歧闸门（约束 34）：文件里目标块的当前字节必须仍是已发布版本的字节。
		if !definitionBlockMatchesFor(original, mcpServersDefinitionsKey, req.Name, published) {
			s.logger.Warn("定义文件与已发布版本不一致，拒绝覆盖",
				"resource", req.Name, "expected_etag", current)
			return core.ResourceVersion{}, &core.AdminError{
				Kind:    core.ErrConflict,
				Message: "定义文件与已发布版本不一致，请先对账或重新读取后再保存",
			}
		}
		var replaceLost bool
		candidate, replaceLost, err = workspace.ReplaceEntry(original, mcpServersDefinitionsKey, req.Name, rendered)
		if err != nil {
			s.logger.Warn("MCP server 块替换失败", "resource", req.Name, "error", err)
			return core.ResourceVersion{}, editUnsupportedError()
		}
		if replaceLost {
			s.logger.Warn("MCP server 条目内含注释，整条替换会丢失它们", "resource", req.Name)
			return core.ResourceVersion{}, editUnsupportedError()
		}
	}

	if !duplicate {
		// 整份候选校验覆盖全文件（块外内容若被破坏必须在这里被拒），但走兼容取径——
		// 未触碰的旧条目不被本次写入连带拒绝。
		if err := workspace.ValidateMCPServersDocument(candidate); err != nil {
			return core.ResourceVersion{}, &core.InvalidInputError{Tool: action, Reason: err.Error()}
		}
		raw, ok := entryBlockRaw(candidate, mcpServersDefinitionsKey, req.Name)
		if !ok {
			return core.ResourceVersion{}, fmt.Errorf("块编辑后未找到 MCP server %s 的条目", req.Name)
		}
		entryRaw = raw
	}

	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   core.TargetKindMCPServer,
		TargetID:     req.Name,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  mcpServerRequestHash(action, req),
		ExpectedETag: req.IfMatch,
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := s.operations.BeginOperation(ctx, record); err != nil {
		s.logger.Error("MCP server 写预写操作失败，拒绝执行", "resource", req.Name, "action", action, "error", err)
		return core.ResourceVersion{}, fmt.Errorf("预写管理操作失败: %w", err)
	}

	if duplicate {
		// 与模型连接同口径：重名是 409 + 操作 failed（预写之前只做纯请求校验）。
		s.finish(ctx, operationID, core.OperationFailed, resourceResultConflict, "同名 MCP server 已存在")
		return core.ResourceVersion{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: fmt.Sprintf("MCP server %s 已存在", req.Name),
			Data:    map[string]any{"operation_id": operationID},
		}
	}

	// env 闸门（模块卡 §2.3 图 2-2）：预写之后、**第一次写文件之前**证明当前进程环境
	// 满足这条条目的 `${VAR}` 引用。失败只留一条 failed/NOT_CONFIGURED 行，文件 / 版本 /
	// 指针零变化；错误与 detail 只点名 server / 键 / 变量，不含值。
	if err := validateMCPEntryEnv(entryRaw, os.LookupEnv); err != nil {
		return core.ResourceVersion{}, s.rejectMCPEnvGate(ctx, operationID, "保存所需环境变量未配置", err)
	}

	if err := workspace.CommitMCPServersDefinition(definitionPath, candidate); err != nil {
		s.logger.Error("写入 MCP 定义失败", "resource", req.Name, "operation_id", operationID, "error", err)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "写入定义文件失败")
		return core.ResourceVersion{}, operationFailedError(operationID, "写入 MCP 定义失败")
	}

	version, err := s.commitMCPServer(ctx, operationID, now, definitionPath, candidate, original,
		core.ResourceVersion{
			Kind:           core.ResourceMCPServer,
			ResourceUID:    uid,
			ResourceID:     req.Name,
			ContentHash:    baselineResourceHash(string(core.ResourceMCPServer), req.Name, entryRaw),
			DefinitionYAML: string(entryRaw),
			OperationID:    operationID,
		}, req.Name)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	return version, nil
}

// skillDefinitionFileName 是技能卡片的固定文件名（与 `workspace` 的未导出常量同值；
// 本层渲染最小卡片需要同一个字面量，**不**引入第二种写法）。
// skillDefinitionFileName is the fixed card file name of a skill.
const skillDefinitionFileName = "SKILL.md"

// skillCardFrontmatter 是渲染最小 SKILL.md 的 frontmatter DTO：键序固定为
// name → description，与解析侧同名同义（正文留空——骨架创建只承诺一个可编辑的实体）。
// skillCardFrontmatter renders the minimal card header; the body stays empty.
type skillCardFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// skillWritePlan 是一条技能写路径的全部输入：创建 / 导入 / 更新只在动作字面量与
// "是否要求已发布版本"上不同，其余编排共用 `writeSkill`。
// skillWritePlan is the shared input of the create / import / update paths.
type skillWritePlan struct {
	action      string
	name        string
	description string
	files       []core.AuthorFile
	create      bool
	ifMatch     string
}

// CreateSkill 创建技能骨架并**立即发布**第一版（图 D）：
//
//	共享校验（名字 + 最小卡片）→ 资源锁 → 同名 409（零操作行）→ 预写 pending →
//	全量替换工作副本 → 快照 version 1 → gate 写锁内同事务写版本 / 指针 / 终态。
//
// 内容由本层渲染：frontmatter 只含 name 与 description，正文留空，与 `POST /skills`
// 契约"名字 + 一句话描述"一致（不采信调用方提供的卡片内容）。
// CreateSkill creates the skeleton skill and publishes version 1 immediately; the
// minimal card is rendered here rather than supplied by the caller.
func (s *ResourceAdmin) CreateSkill(ctx context.Context, actor core.Actor, req SkillCreateRequest) (SkillDetail, error) {
	card, err := renderSkillCard(req.Name, req.Description)
	if err != nil {
		return SkillDetail{}, err
	}
	return s.writeSkill(ctx, actor, skillWritePlan{
		action:      core.ActionSkillCreate,
		name:        req.Name,
		description: req.Description,
		files:       []core.AuthorFile{{Path: skillDefinitionFileName, Content: card}},
		create:      true,
	})
}

// ImportSkill 导入一整棵技能目录并立即发布（约束 24 的边界由共享校验统一裁决）。
// ImportSkill imports a whole skill tree and publishes it immediately.
func (s *ResourceAdmin) ImportSkill(ctx context.Context, actor core.Actor, req SkillImportRequest) (SkillDetail, error) {
	return s.writeSkill(ctx, actor, skillWritePlan{
		action: core.ActionSkillImport,
		name:   req.Name,
		files:  req.Files,
		create: true,
	})
}

// UpdateSkill 全量替换技能工作副本并立即发布新版本（约束 25）：未列出的文件随旧树
// 一起消失，staging 构建与两次 rename 由工作空间实现；提交段失败回滚旧副本与快照。
// UpdateSkill replaces the work copy wholesale and publishes a new version.
func (s *ResourceAdmin) UpdateSkill(ctx context.Context, actor core.Actor, req SkillUpdateRequest) (SkillDetail, error) {
	return s.writeSkill(ctx, actor, skillWritePlan{
		action:  core.ActionSkillUpdate,
		name:    req.Name,
		files:   req.Files,
		ifMatch: req.IfMatch,
	})
}

// ValidateSkill 是 `POST /skills/{name}/validate` 的实现（约束 26）：与创建 / 导入 /
// 更新**共用** `workspace.ValidateSkillDraft`，不落盘、不写操作行、不执行脚本。
// 校验不通过以 `valid=false` + `problems` 返回 200——端点回答的是"这份候选能不能
// 写"，不是"请求本身非法"；名字取路径值，`req.Name` 不参与判定。
// ValidateSkill runs the exact create/import/update gate without touching disk,
// operations or scripts; an invalid candidate is a 200 with valid=false.
func (s *ResourceAdmin) ValidateSkill(ctx context.Context, name string, req SkillUpdateRequest) (core.SkillValidateResult, error) {
	if _, err := workspace.ValidateSkillDraft(name, req.Files); err != nil {
		return core.SkillValidateResult{Valid: false, Problems: []string{err.Error()}}, nil
	}
	return core.SkillValidateResult{Valid: true, Problems: []string{}}, nil
}

// writeSkill 是技能创建 / 导入 / 更新的共用编排（图 D）：
//
//	候选校验（共享闸门，零写入）→ 资源锁（兼任工作副本文件锁，约束 36）→ 同名 409 /
//	指针 404 / If-Match 412 / 工作副本分歧 409（四者都不写操作行，见文件头差异说明）
//	→ 预写 pending → 全量替换工作副本 → 快照 → gate 写锁内同事务写版本 / 指针 / 终态。
//
// 预写之后的任一步失败都回填终态：工作副本恢复为写入前内容（创建 / 导入为删除）、
// 本次快照删除；补偿失败置 unknown，不谎报 failed（约束 36）。
func (s *ResourceAdmin) writeSkill(ctx context.Context, actor core.Actor, plan skillWritePlan) (SkillDetail, error) {
	cand, err := workspace.ValidateSkillDraft(plan.name, plan.files)
	if err != nil {
		// 导入边界违反：400（web 映射 IMPORT_REJECTED），零写入、零操作行。
		return SkillDetail{}, err
	}

	unlockResource := s.resourceLocks.lock(resourceLockKey(core.ResourceSkill, plan.name))
	defer unlockResource()

	pointer, err := s.store.Pointer(ctx, core.ResourceSkill, plan.name)
	if err != nil && !errors.Is(err, core.ErrResourceNotFound) {
		return SkillDetail{}, fmt.Errorf("读取技能 %s 的发布指针: %w", plan.name, err)
	}
	live := err == nil && pointer.DeletedAt == nil
	var published core.ResourceVersion
	if live {
		published, err = s.store.VersionByID(ctx, pointer.PublishedVersionID)
		if err != nil {
			return SkillDetail{}, fmt.Errorf("读取技能 %s 的发布版本: %w", plan.name, err)
		}
	}
	var uid core.ResourceUID
	if plan.create {
		if live {
			// 同名创建是 409 且**零操作行**（图 D「不写操作行」、失败表技能行"零写入"）。
			s.logger.Warn("同名技能已存在，拒绝创建", "skill", plan.name, "action", plan.action)
			return SkillDetail{}, &core.AdminError{
				Kind:    core.ErrConflict,
				Message: fmt.Sprintf("技能 %s 已存在", plan.name),
			}
		}
		// 创建（含删除后的同名重建）分配新身份：版本号从 1 起算，旧 UID 的历史不接管。
		uid, err = newResourceUID(core.ResourceSkill, plan.name)
		if err != nil {
			return SkillDetail{}, err
		}
	} else {
		if !live {
			return SkillDetail{}, fmt.Errorf("%w: 技能 %s", core.ErrResourceNotFound, plan.name)
		}
		uid, err = resourceUIDOfPointer(core.ResourceSkill, plan.name, pointer)
		if err != nil {
			return SkillDetail{}, err
		}
		current, etagErr := publishedResourceETag(ctx, s.credentials, pointer, published)
		if etagErr != nil {
			return SkillDetail{}, etagErr
		}
		// 技能的 PUT 把 If-Match 缺失归入 412（失败表），与发布路径的"空串 = 未声明"不同。
		if plan.ifMatch == "" || plan.ifMatch != current {
			return SkillDetail{}, preconditionError(current)
		}
	}

	var previous []workspace.SkillFile
	if !plan.create {
		work, diffErr := s.skillWorkCopyState(plan.name, published)
		if diffErr != nil {
			return SkillDetail{}, diffErr
		}
		workRel, relErr := workspace.SkillWorkCopyRelDir(plan.name)
		if relErr != nil {
			return SkillDetail{}, relErr
		}
		// 回滚材料在预写之前取：读失败不该留下一条 pending / failed 操作行。
		previous, err = readSkillFiles(s.root, workRel, work.Files)
		if err != nil {
			return SkillDetail{}, fmt.Errorf("读取技能 %s 的现有工作副本: %w", plan.name, err)
		}
	}

	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       plan.action,
		TargetKind:   core.TargetKindSkill,
		TargetID:     plan.name,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  skillWriteRequestHash(plan.action, cand),
		ExpectedETag: plan.ifMatch,
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := s.operations.BeginOperation(ctx, record); err != nil {
		s.logger.Error("技能写预写操作失败，拒绝执行", "skill", plan.name, "action", plan.action, "error", err)
		return SkillDetail{}, fmt.Errorf("预写管理操作失败: %w", err)
	}

	restore := func() error { return removeSkillWorkCopy(s.root, plan.name) }
	if !plan.create {
		restore = func() error { return restoreSkillWorkCopy(s.root, plan.name, previous) }
	}

	if err := workspace.WriteSkillWorkCopy(s.root, cand); err != nil {
		// 工作空间保证失败时旧副本原样保留，因此这一阶段没有文件要回滚；失败面按图 D
		// 分两种：导入边界违反 → 400（IMPORT_REJECTED），其余 → 500。
		s.logger.Error("写入技能工作副本失败", "skill", plan.name, "operation_id", operationID, "error", err)
		if errors.Is(err, core.ErrImportRejected) {
			s.finish(ctx, operationID, core.OperationFailed, agentResultBadRequest, "技能候选未通过边界校验")
			return SkillDetail{}, &core.AdminError{
				Kind:    core.ErrImportRejected,
				Message: err.Error(),
				Data:    map[string]any{"operation_id": operationID},
			}
		}
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "写入技能工作副本失败")
		return SkillDetail{}, operationFailedError(operationID, "写入技能工作副本失败")
	}

	versions, err := s.store.VersionsByUID(ctx, core.ResourceSkill, uid)
	if err != nil {
		return SkillDetail{}, s.abortSkillWrite(ctx, operationID, plan.name,
			"列出技能版本失败", err, uid, 0, restore)
	}
	nextNo := 1
	if len(versions) > 0 {
		nextNo = versions[0].VersionNo + 1
	}
	storagePath, err := workspace.SnapshotSkill(s.root, cand, uid, nextNo)
	if err != nil {
		return SkillDetail{}, s.abortSkillWrite(ctx, operationID, plan.name,
			"技能版本快照失败", err, uid, nextNo, restore)
	}

	s.gate.Lock()
	version, err := s.publishVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceUID:  uid,
		ResourceID:   plan.name,
		ContentHash:  cand.ContentHash,
		ManifestJSON: cand.ManifestJSON,
		StoragePath:  storagePath,
		OperationID:  operationID,
	}, true, operationID, now)
	if err != nil {
		s.gate.Unlock()
		s.logger.Error("技能版本提交失败", "skill", plan.name, "operation_id", operationID, "error", err)
		return SkillDetail{}, s.abortSkillWrite(ctx, operationID, plan.name,
			"技能版本提交失败", err, uid, nextNo, restore)
	}
	s.gate.Unlock()

	// 提交后重读指针：revision 已在发布事务里前进，结果视图的 ETag 必须按新指针组成。
	publishedPtr, err := s.store.Pointer(ctx, core.ResourceSkill, plan.name)
	if err != nil {
		return SkillDetail{}, fmt.Errorf("读取技能 %s 的提交后指针: %w", plan.name, err)
	}
	return s.skillDetail(ctx, plan.name, plan.description, cand.Files, publishedPtr, version)
}

// skillWorkCopyState 在锁内重读工作副本并做分歧闸门（约束 35）：工作副本不存在 / 缺
// SKILL.md 或内容哈希 ≠ 已发布 `content_hash` → 409，且**不写操作行**（图 D）。
// skillWorkCopyState re-reads the work copy under the lock and returns a row-free 409
// when it diverges from the published version.
func (s *ResourceAdmin) skillWorkCopyState(name string, published core.ResourceVersion) (workspace.SkillVersion, error) {
	work, err := workspace.NewSkillLibrary(s.root).HashWorkCopy(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.logger.Warn("技能工作副本缺失，拒绝写入", "skill", name)
			return workspace.SkillVersion{}, skillWorkCopyDivergedError(
				"技能工作副本不存在或缺少 SKILL.md，请重启完成对账后再写")
		}
		return workspace.SkillVersion{}, fmt.Errorf("读取技能 %s 的工作副本: %w", name, err)
	}
	if work.ContentHash != published.ContentHash {
		// 记的是已发布版本的内容哈希，不是条件 ETag（含范围与凭据段）——日志键名
		// 如实写清楚，避免排障时把内容哈希当 ETag 去对页面。
		s.logger.Warn("技能工作副本与已发布版本不一致，拒绝写入",
			"skill", name, "published_content_hash", published.ContentHash)
		return workspace.SkillVersion{}, skillWorkCopyDivergedError(
			"技能工作副本已在外部变化，请重启完成对账后再写")
	}
	return work, nil
}

// abortSkillWrite 是预写之后的统一回滚：恢复工作副本（创建 / 导入为删除，更新 / 发布
// 为重建旧文件）并删除本次快照；补偿失败一律置 unknown，不谎报 failed。返回的 500
// 带回可查的操作标识（约束 1a）。
// abortSkillWrite restores the work copy and drops this attempt's snapshot; a failed
// compensation is classified unknown rather than reported as a clean failure.
func (s *ResourceAdmin) abortSkillWrite(ctx context.Context, operationID, name, reason string,
	cause error, uid core.ResourceUID, versionNo int, restore func() error,
) error {
	s.logger.Error("技能写失败，开始回滚", "skill", name, "operation_id", operationID,
		"reason", reason, "error", cause)
	status := core.OperationFailed
	message := reason
	if err := restore(); err != nil {
		status = core.OperationUnknown
		message = reason + "；回滚工作副本失败，结果待查证"
		s.logger.Error("技能写回滚工作副本失败", "skill", name, "operation_id", operationID, "error", err)
	} else if err := removeSkillSnapshot(s.root, name, uid, versionNo); err != nil {
		status = core.OperationUnknown
		message = reason + "；清理快照失败，结果待查证"
		s.logger.Error("技能写回滚清理快照失败", "skill", name, "operation_id", operationID,
			"version_no", versionNo, "error", err)
	}
	s.finish(ctx, operationID, status, lifecycleResultInternal, message)
	return operationFailedError(operationID, message)
}

// skillDetail 组装写操作的结果视图：path 与只读面同一口径；引用列表读仓储，读失败
// 只记 WARN 并返回空列表（写入已成功，不因展示数据回滚）。`description` 在创建时有
// 唯一来源（本次请求），导入 / 更新 / 发布留空——只读面受 I-60 约束也没有可达来源。
// skillDetail builds the write result; a reference-read failure degrades the display
// only and never rolls the write back.
func (s *ResourceAdmin) skillDetail(ctx context.Context, name, description string,
	files []workspace.SkillFile, pointer core.ResourcePointer, version core.ResourceVersion,
) (SkillDetail, error) {
	referenced, err := s.store.ReferencedBy(ctx, core.ResourceSkill, name)
	if err != nil {
		s.logger.Warn("读取技能引用失败，返回空引用列表", "skill", name, "error", err)
		referenced = nil
	}
	// ETag 取**本次提交之后**的指针（revision 已前进），不能让页面拿旧修订组成的新值。
	etag, err := publishedResourceETag(ctx, s.credentials, pointer, version)
	if err != nil {
		return SkillDetail{}, err
	}
	return SkillDetail{
		Name:         name,
		Description:  description,
		Path:         skillWorkCopyPrefix + name,
		VersionID:    version.ID,
		VersionNo:    version.VersionNo,
		ContentHash:  version.ContentHash,
		ETag:         etag,
		ReferencedBy: nonNilStrings(referenced),
		Files:        files,
	}, nil
}

// renderSkillCard 渲染骨架创建的最小 SKILL.md：frontmatter 由 yaml.Marshal 生成
// （name 与 description 都经过 YAML 转义），正文留空。序列化失败不外泄半截卡片。
// renderSkillCard renders the minimal card, with YAML-escaped header fields.
func renderSkillCard(name, description string) (string, error) {
	raw, err := yaml.Marshal(skillCardFrontmatter{Name: name, Description: description})
	if err != nil {
		return "", fmt.Errorf("渲染技能 %s 的最小 %s: %w", name, skillDefinitionFileName, err)
	}
	return "---\n" + string(raw) + "---\n", nil
}

// readSkillFiles 按已登记的清单从目录读取文件并逐项复核大小与 sha256：清单是已发布
// 事实，字节只是它的存储副本，任一项不符即说明存储被外部改动——按内部错误失败关闭，
// 不把可疑字节当版本内容使用。
// readSkillFiles verifies size and sha256 of every listed file and fails closed on
// any mismatch.
func readSkillFiles(root, relDir string, files []core.FileHash) ([]workspace.SkillFile, error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	out := make([]workspace.SkillFile, 0, len(files))
	for _, file := range files {
		rel := path.Join(relDir, file.Path)
		data, readErr := base.ReadFile(filepath.FromSlash(rel))
		if readErr != nil {
			return nil, fmt.Errorf("读取技能文件 %s: %w", rel, readErr)
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != file.SHA256 || int64(len(data)) != file.Size {
			return nil, fmt.Errorf("技能文件 %s 与登记清单不符（sha256 %s/%s，大小 %d/%d）",
				rel, got, file.SHA256, len(data), file.Size)
		}
		out = append(out, workspace.SkillFile{
			Path:    file.Path,
			Content: data,
			SHA256:  got,
			Size:    int64(len(data)),
		})
	}
	return out, nil
}

// restoreSkillWorkCopy 把写入前的工作副本文件重建回去：候选经共享校验后走同一条
// 全量替换路径（哈希自洽），不手拼文件系统写。
// restoreSkillWorkCopy rebuilds the pre-write work copy through the same validated path.
func restoreSkillWorkCopy(root, name string, files []workspace.SkillFile) error {
	drafts := make([]core.AuthorFile, len(files))
	for i, file := range files {
		drafts[i] = core.AuthorFile{Path: file.Path, Content: string(file.Content)}
	}
	cand, err := workspace.ValidateSkillDraft(name, drafts)
	if err != nil {
		return fmt.Errorf("重建技能 %s 的回滚候选: %w", name, err)
	}
	return workspace.WriteSkillWorkCopy(root, cand)
}

// removeSkillWorkCopy 删除创建 / 导入失败时留下的工作副本（回滚到"没有这个技能"）。
// removeSkillWorkCopy removes a freshly written work copy during rollback.
func removeSkillWorkCopy(root, name string) error {
	relDir, err := workspace.SkillWorkCopyRelDir(name)
	if err != nil {
		return err
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()
	if err := base.RemoveAll(filepath.FromSlash(relDir)); err != nil {
		return fmt.Errorf("删除技能工作副本 %s: %w", relDir, err)
	}
	return nil
}

// removeSkillSnapshot 删除本次失败写入产生的版本快照：版本目录发布后是只读的
// （0o444 / 0o555，Windows 上映射为只读属性），必须先把权限放宽再 RemoveAll。
// 路径按本次写入的身份与版本号拼出——只删这一代（<name>/<uid>/<no>）的目录，同名的
// 上一代（另一 UID）文件不受影响。版本号 0 表示还没有快照，按幂等空操作处理。
// removeSkillSnapshot clears the read-only bits before removing this attempt's snapshot;
// it addresses exactly <name>/<uid>/<no>, so another generation's files are untouched.
func removeSkillSnapshot(root, name string, uid core.ResourceUID, versionNo int) error {
	if versionNo < 1 {
		return nil
	}
	relDir, err := workspace.SkillVersionRelDir(name, uid, versionNo)
	if err != nil {
		return err
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	walkErr := fs.WalkDir(base.FS(), relDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		perm := os.FileMode(0o644)
		if d.IsDir() {
			perm = 0o755
		}
		return base.Chmod(filepath.FromSlash(p), perm)
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return fmt.Errorf("放宽技能快照权限 %s: %w", relDir, walkErr)
	}
	if err := base.RemoveAll(filepath.FromSlash(relDir)); err != nil {
		return fmt.Errorf("删除技能快照 %s: %w", relDir, err)
	}
	return nil
}

// skillWorkCopyDivergedError 是技能工作副本分歧的统一载体：409 CONFLICT、零操作行。
// skillWorkCopyDivergedError is the row-free 409 carrier of a work-copy divergence.
func skillWorkCopyDivergedError(message string) error {
	return &core.AdminError{Kind: core.ErrConflict, Message: message}
}

// skillWriteRequestHash 摘要一次技能写请求：动作、技能名与候选清单哈希（材料不含
// 文件正文，符合"脱敏请求摘要"口径）。
// skillWriteRequestHash digests a skill write without any file content.
func skillWriteRequestHash(action string, cand workspace.SkillCandidate) string {
	return agentRequestHash(strings.Join([]string{action, cand.Name, cand.ContentHash}, "\n"))
}

// Publish 发布模型连接版本（约束 9 / 10）：
//
//   - 不带 `VersionID` → 发布当前最新版本；最新版本已是发布指针 → 幂等成功
//     （不新增版本、不写文件，但同样留一条成功操作行）。
//   - 带旧 `VersionID` → 用该版本的**存储字节**创建新版本（`version_no=N+1`，
//     `content_hash` 等于来源版本），并把定义文件改写为该版本字节——历史版本永不改写。
//
// 发布前先创建运行时客户端（已发布版本 → 一次性构造，在预写之前），再原子写文件，
// 最后在 gate 写锁内同事务提交版本 / 指针 / 终态并替换目录；任一步失败都保留旧文件
// 与旧指针（约束 10），补偿前做文件身份检查，身份不符 → 操作置 unknown。
// Publish publishes the latest version (idempotent when already current) or appends a
// new version from the bytes of an older one; failures keep the old file and pointer.
func (s *ResourceAdmin) Publish(ctx context.Context, actor core.Actor, req core.ResourcePublishRequest) (core.ResourcePublishResult, error) {
	if err := validateResourceID(req.ID); err != nil {
		return core.ResourcePublishResult{}, &core.InvalidInputError{Tool: publishAction(req.Kind), Reason: err.Error()}
	}
	// 三类资源共用同一签名：技能走自己的工作副本编排（没有可编辑的定义文件块），
	// 另两类共用下面的块替换编排；未知类型在这里拒绝（400），不静默走其它资源路径。
	if _, err := core.ParseResourceKind(string(req.Kind)); err != nil {
		return core.ResourcePublishResult{}, &core.InvalidInputError{Tool: publishAction(req.Kind), Reason: err.Error()}
	}
	action := publishAction(req.Kind)
	if req.Kind == core.ResourceSkill {
		return s.publishSkillVersion(ctx, actor, req, action)
	}
	definitionPath := s.configPath
	blockKey := providersBlockKey
	commitDefinition := workspace.CommitProviderDefinition
	validateDefinition := func(doc []byte) error {
		_, err := workspace.ValidateProviderEntries(doc)
		return err
	}
	if req.Kind == core.ResourceMCPServer {
		definitionPath = s.mcpServersPath()
		blockKey = mcpServersDefinitionsKey
		commitDefinition = workspace.CommitMCPServersDefinition
		validateDefinition = workspace.ValidateMCPServersDocument
	}

	unlockResource := s.resourceLocks.lock(resourceLockKey(req.Kind, req.ID))
	defer unlockResource()

	pointer, err := s.activePointer(ctx, req.Kind, req.ID)
	if err != nil {
		return core.ResourcePublishResult{}, err
	}
	uid, err := resourceUIDOfPointer(req.Kind, req.ID, pointer)
	if err != nil {
		return core.ResourcePublishResult{}, err
	}
	published, err := s.store.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return core.ResourcePublishResult{}, fmt.Errorf("读取 %s %s 的发布版本: %w", req.Kind, req.ID, err)
	}
	current, err := publishedResourceETag(ctx, s.credentials, pointer, published)
	if err != nil {
		return core.ResourcePublishResult{}, err
	}
	// 逐字回归用例 `TestResourceAdmin_PublishFailureKeepsOldPointer` 不带 ETag，
	// 因此空串在发布路径按"未声明期望"处理（web 层对所有写端点要求 If-Match）；
	// 非空值必须在锁内复核——中间被别人发布过就 412，零写入、零操作行。
	if req.ExpectedETag != "" && req.ExpectedETag != current {
		return core.ResourcePublishResult{}, preconditionError(current)
	}

	var target core.ResourceVersion
	if req.VersionID != nil {
		target, err = s.store.VersionByID(ctx, *req.VersionID)
		if errors.Is(err, core.ErrResourceNotFound) {
			return core.ResourcePublishResult{}, fmt.Errorf("%w: 版本 %d", core.ErrResourceNotFound, *req.VersionID)
		}
		if err != nil {
			return core.ResourcePublishResult{}, fmt.Errorf("读取发布目标版本 %d: %w", *req.VersionID, err)
		}
		// 版本必须属于**当前身份**：同名重建后旧 UID 的历史版本不能被当作本实例的来源。
		if target.Kind != req.Kind || target.ResourceID != req.ID || target.ResourceUID != uid {
			return core.ResourcePublishResult{}, &core.InvalidInputError{
				Tool:   action,
				Reason: fmt.Sprintf("版本 %d 不属于 %s %s", target.ID, req.Kind, req.ID),
			}
		}
	} else {
		versions, err := s.store.VersionsByUID(ctx, req.Kind, uid)
		if err != nil {
			return core.ResourcePublishResult{}, fmt.Errorf("列出 %s %s 的版本: %w", req.Kind, req.ID, err)
		}
		if len(versions) == 0 {
			return core.ResourcePublishResult{}, fmt.Errorf("%w: %s %s 没有可发布的版本", core.ErrResourceNotFound, req.Kind, req.ID)
		}
		target = versions[0]
	}
	idempotent := req.VersionID == nil && target.ID == pointer.PublishedVersionID

	unlockFile := s.fileLocks.lock(definitionPath)
	defer unlockFile()

	original, err := readFileUnder(filepath.Dir(definitionPath), filepath.Base(definitionPath))
	if err != nil {
		return core.ResourcePublishResult{}, fmt.Errorf("读取 %s 定义 %s: %w", req.Kind, definitionPath, err)
	}
	if _, err := workspace.BlockEntries(original, blockKey); err != nil {
		s.logger.Warn("资源定义块不可安全编辑", "kind", string(req.Kind), "resource", req.ID, "action", action, "error", err)
		return core.ResourcePublishResult{}, editUnsupportedError()
	}
	fileMatchesPublished := definitionBlockMatchesFor(original, blockKey, req.ID, published)
	// 崩溃窗口的重试（约束 13 的"重新发布"）：上次发布已把定义文件改写为目标版本、
	// 但版本 / 指针事务未提交时，文件已是目标字节。此时允许继续，并跳过重复改写；
	// 其它任何与发布版本和目标都不一致的形态仍按分歧闸门 409 拒绝。
	fileMatchesTarget := !idempotent && definitionBlockMatchesFor(original, blockKey, req.ID, target)
	if !fileMatchesPublished && !fileMatchesTarget {
		s.logger.Warn("定义文件与已发布版本不一致，拒绝发布", "resource", req.ID, "expected_etag", current)
		return core.ResourcePublishResult{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "定义文件与已发布版本不一致，请先对账或重新读取后再发布",
		}
	}

	var (
		candidate []byte
		client    core.ProviderClient
		model     string
	)
	if !idempotent {
		if req.Kind == core.ResourceModelConnection {
			entry, entryErr := providerEntryFromVersion(req.ID, target)
			if entryErr != nil {
				return core.ResourcePublishResult{}, fmt.Errorf("读取模型连接 %s 的目标版本 %d: %w", req.ID, target.ID, entryErr)
			}
			// 运行时客户端按条目声明的凭据模式取 Key：env 解析精确 `${VAR}`，managed
			// 经凭据解析器按**目标版本**解密——发布（含撤回 / 重发）不得把托管条目误判
			// 为缺占位。凭据哨兵原样上抛，由 web 层 `writeCredentialError` 映射
			// 400 / 409 / 503；env 解析错误（无哨兵）仍按请求非法 400。
			// The published client resolves the key by the entry's declared mode; managed
			// entries go through the credential resolver with the target version ref.
			apiKey, keyErr := catalogAPIKey(ctx, pointer, entry, target, s.credentialSource)
			if keyErr != nil {
				if isCredentialSentinel(keyErr) {
					return core.ResourcePublishResult{}, keyErr
				}
				return core.ResourcePublishResult{}, &core.InvalidInputError{Tool: action, Reason: keyErr.Error()}
			}
			client, err = newModelConnectionClient(req.ID, entry, apiKey)
			if err != nil {
				return core.ResourcePublishResult{}, &core.InvalidInputError{Tool: action, Reason: err.Error()}
			}
			model = entry.DefaultModel
		} else {
			cfg, parseErr := workspace.ParseMCPServerBlock([]byte(target.DefinitionYAML))
			if parseErr != nil {
				return core.ResourcePublishResult{}, fmt.Errorf("读取 MCP server %s 的目标版本 %d: %w", req.ID, target.ID, parseErr)
			}
			if cfg.Name != req.ID {
				return core.ResourcePublishResult{}, fmt.Errorf(
					"MCP server %s 的目标版本 %d 条目名 %q 与资源标识不一致", req.ID, target.ID, cfg.Name)
			}
		}
		if fileMatchesTarget {
			s.logger.Warn("发布重试：定义文件已是目标版本字节，跳过重复改写",
				"kind", string(req.Kind), "resource", req.ID, "version_id", target.ID)
			candidate = original
		} else {
			var lost bool
			candidate, lost, err = workspace.ReplaceEntry(original, blockKey, req.ID,
				definitionEntry(target.DefinitionYAML))
			if err != nil {
				s.logger.Warn("资源块替换失败", "kind", string(req.Kind), "resource", req.ID, "version_id", target.ID, "error", err)
				return core.ResourcePublishResult{}, editUnsupportedError()
			}
			if lost {
				s.logger.Warn("资源条目内含注释，撤回会丢失它们", "kind", string(req.Kind), "resource", req.ID, "version_id", target.ID)
				return core.ResourcePublishResult{}, editUnsupportedError()
			}
			if err := validateDefinition(candidate); err != nil {
				return core.ResourcePublishResult{}, &core.InvalidInputError{Tool: action, Reason: err.Error()}
			}
		}
	}

	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   resourceTargetKind(req.Kind),
		TargetID:     req.ID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  resourcePublishHash(req),
		ExpectedETag: req.ExpectedETag,
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if req.VersionID != nil {
		record.RequestedVersionID = *req.VersionID
	}
	if err := s.operations.BeginOperation(ctx, record); err != nil {
		s.logger.Error("发布资源预写操作失败，拒绝执行", "kind", string(req.Kind), "resource", req.ID, "error", err)
		return core.ResourcePublishResult{}, fmt.Errorf("预写管理操作失败: %w", err)
	}

	if idempotent {
		// 已发布 → 幂等返回：不新增版本、不写文件，仍留一条 success 操作行。
		// MCP 的待重启由 published ≠ applied 派生——幂等重发不改变任何指针，结论照旧。
		s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "")
		pendingRestart := req.Kind == core.ResourceMCPServer && mcpRestartPending(pointer)
		return core.ResourcePublishResult{Version: target, PendingRestart: pendingRestart}, nil
	}

	// env 闸门（模块卡 §2.3 图 2-2）：幂等分支已在上面短路，这里校验的是**目标版本**
	// 条目（回滚同样走这里）——当前进程环境不满足就拒绝，且不改文件 / 指针 / 版本。
	if req.Kind == core.ResourceMCPServer {
		if err := validateMCPEntryEnv([]byte(target.DefinitionYAML), os.LookupEnv); err != nil {
			return core.ResourcePublishResult{}, s.rejectMCPEnvGate(ctx, operationID, "发布所需环境变量未配置", err)
		}
	}

	if !bytes.Equal(candidate, original) {
		if err := commitDefinition(definitionPath, candidate); err != nil {
			s.logger.Error("写入资源定义失败", "kind", string(req.Kind), "resource", req.ID, "operation_id", operationID, "error", err)
			s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "写入定义文件失败")
			return core.ResourcePublishResult{}, operationFailedError(operationID, "写入定义文件失败")
		}
	}

	// 新版本按来源版本的**存储字节**追加（内容与哈希逐字保留），文件收到的是块形态；
	// 两者由分歧闸门的双形态判据统一——哈希永不重新计算。
	recordVersion := core.ResourceVersion{
		Kind:           req.Kind,
		ResourceUID:    uid,
		ResourceID:     req.ID,
		ContentHash:    target.ContentHash,
		DefinitionYAML: target.DefinitionYAML,
		ManifestJSON:   target.ManifestJSON,
		StoragePath:    target.StoragePath,
		OperationID:    operationID,
	}
	if req.Kind == core.ResourceMCPServer {
		version, err := s.commitMCPServer(ctx, operationID, now, definitionPath, candidate, original,
			recordVersion, req.ID)
		if err != nil {
			return core.ResourcePublishResult{}, err
		}
		// 发布只更新 published 指针；applied 在下一次成功连接后才追上（约束 9）。
		return core.ResourcePublishResult{Version: version, PendingRestart: true}, nil
	}
	version, err := s.commitModelConnection(ctx, operationID, now, definitionPath, candidate, original,
		recordVersion, req.ID, client, model)
	if err != nil {
		return core.ResourcePublishResult{}, err
	}
	return core.ResourcePublishResult{Version: version, PendingRestart: false}, nil
}

// publishSkillVersion 是技能的发布编排（约束 9 / 25，图 D 的撤回分支）：
//
//   - 不带 `VersionID` → 发布最新版本；最新即发布指针 → 幂等成功（不新建版本、
//     不改写工作副本，仍留一条 success 操作行）。
//   - 带旧 `VersionID` → 用该版本**存储目录的字节**重建候选（哈希与清单必须与登记
//     逐项相等），全量替换工作副本并追加新版本——历史快照永不改写。
//
// 技能没有定义文件块：分歧闸门比对的是工作副本的完整文件清单哈希，回滚材料是写入前
// 的工作副本文件。其余口径（预写、gate 内的同事务提交、补偿失败置 unknown）与
// 创建 / 导入 / 更新完全一致。
// publishSkillVersion publishes the latest version idempotently, or rebuilds an older
// version from its published bytes into a new version and the work copy.
func (s *ResourceAdmin) publishSkillVersion(ctx context.Context, actor core.Actor,
	req core.ResourcePublishRequest, action string,
) (core.ResourcePublishResult, error) {
	unlockResource := s.resourceLocks.lock(resourceLockKey(core.ResourceSkill, req.ID))
	defer unlockResource()

	pointer, err := s.activePointer(ctx, core.ResourceSkill, req.ID)
	if err != nil {
		return core.ResourcePublishResult{}, err
	}
	uid, err := resourceUIDOfPointer(core.ResourceSkill, req.ID, pointer)
	if err != nil {
		return core.ResourcePublishResult{}, err
	}
	published, err := s.store.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return core.ResourcePublishResult{}, fmt.Errorf("读取技能 %s 的发布版本: %w", req.ID, err)
	}
	current, err := publishedResourceETag(ctx, s.credentials, pointer, published)
	if err != nil {
		return core.ResourcePublishResult{}, err
	}
	// 与块替换编排同口径：空串在发布路径是"未声明期望"（web 层要求 If-Match）。
	if req.ExpectedETag != "" && req.ExpectedETag != current {
		return core.ResourcePublishResult{}, preconditionError(current)
	}

	work, err := s.skillWorkCopyState(req.ID, published)
	if err != nil {
		return core.ResourcePublishResult{}, err
	}
	workRel, err := workspace.SkillWorkCopyRelDir(req.ID)
	if err != nil {
		return core.ResourcePublishResult{}, err
	}

	versions, err := s.store.VersionsByUID(ctx, core.ResourceSkill, uid)
	if err != nil {
		return core.ResourcePublishResult{}, fmt.Errorf("列出技能 %s 的版本: %w", req.ID, err)
	}
	if len(versions) == 0 {
		return core.ResourcePublishResult{}, fmt.Errorf("%w: 技能 %s 没有可发布的版本", core.ErrResourceNotFound, req.ID)
	}
	target := versions[0]
	if req.VersionID != nil {
		target, err = s.store.VersionByID(ctx, *req.VersionID)
		if errors.Is(err, core.ErrResourceNotFound) {
			return core.ResourcePublishResult{}, fmt.Errorf("%w: 版本 %d", core.ErrResourceNotFound, *req.VersionID)
		}
		if err != nil {
			return core.ResourcePublishResult{}, fmt.Errorf("读取发布目标版本 %d: %w", *req.VersionID, err)
		}
		// 与块替换编排同口径：来源版本必须属于当前身份，同名重建前的历史不可混用。
		if target.Kind != core.ResourceSkill || target.ResourceID != req.ID || target.ResourceUID != uid {
			return core.ResourcePublishResult{}, &core.InvalidInputError{
				Tool:   action,
				Reason: fmt.Sprintf("版本 %d 不属于技能 %s", target.ID, req.ID),
			}
		}
	}
	idempotent := req.VersionID == nil && target.ID == pointer.PublishedVersionID

	var (
		cand     workspace.SkillCandidate
		previous []workspace.SkillFile
	)
	if !idempotent {
		lib := workspace.NewSkillLibrary(s.root)
		source, loadErr := lib.LoadPublished(ctx, req.ID, target.VersionNo, target.StoragePath)
		if loadErr != nil {
			return core.ResourcePublishResult{}, fmt.Errorf(
				"读取技能 %s 的目标版本 %d: %w", req.ID, target.ID, loadErr)
		}
		if source.ContentHash != target.ContentHash {
			return core.ResourcePublishResult{}, fmt.Errorf(
				"技能 %s 的版本 %d 存储哈希 %s 与登记 %s 不一致",
				req.ID, target.ID, source.ContentHash, target.ContentHash)
		}
		// 读取路径以**登记的 storage_path** 为准（新身份分段 / 旧扁平都认），
		// 不按当前身份重拼——来源版本可能属于上一代的目录形态。
		versionRel, relErr := workspace.PublishedVersionRelDir(target.StoragePath)
		if relErr != nil {
			return core.ResourcePublishResult{}, fmt.Errorf(
				"技能 %s 的版本 %d 发布目录不可用: %w", req.ID, target.ID, relErr)
		}
		rawFiles, readErr := readSkillFiles(s.root, versionRel, source.Files)
		if readErr != nil {
			return core.ResourcePublishResult{}, fmt.Errorf(
				"读取技能 %s 的版本 %d 文件: %w", req.ID, target.ID, readErr)
		}
		drafts := make([]core.AuthorFile, len(rawFiles))
		for i, file := range rawFiles {
			drafts[i] = core.AuthorFile{Path: file.Path, Content: string(file.Content)}
		}
		cand, err = workspace.ValidateSkillDraft(req.ID, drafts)
		if err != nil {
			return core.ResourcePublishResult{}, fmt.Errorf(
				"技能 %s 的版本 %d 未通过边界校验: %w", req.ID, target.ID, err)
		}
		// 重建候选必须与登记逐项相等：清单与哈希来自同一批字节，任何不等都说明存储
		// 与登记分叉，不能把可疑内容发布成新版本。
		if cand.ContentHash != target.ContentHash || cand.ManifestJSON != target.ManifestJSON {
			return core.ResourcePublishResult{}, fmt.Errorf(
				"技能 %s 的版本 %d 重建候选与登记清单不一致", req.ID, target.ID)
		}
		// 回滚材料：改写前的工作副本（分歧闸门已确认它等于已发布内容）。
		previous, err = readSkillFiles(s.root, workRel, work.Files)
		if err != nil {
			return core.ResourcePublishResult{}, fmt.Errorf("读取技能 %s 的现有工作副本: %w", req.ID, err)
		}
	}

	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   core.TargetKindSkill,
		TargetID:     req.ID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  resourcePublishHash(req),
		ExpectedETag: req.ExpectedETag,
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if req.VersionID != nil {
		record.RequestedVersionID = *req.VersionID
	}
	if err := s.operations.BeginOperation(ctx, record); err != nil {
		s.logger.Error("发布技能预写操作失败，拒绝执行", "skill", req.ID, "error", err)
		return core.ResourcePublishResult{}, fmt.Errorf("预写管理操作失败: %w", err)
	}

	if idempotent {
		// 已发布 → 幂等返回：不新建版本、不改写工作副本，仍留一条 success 操作行。
		s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "")
		return core.ResourcePublishResult{Version: target, PendingRestart: false}, nil
	}

	nextNo := versions[0].VersionNo + 1
	restore := func() error { return restoreSkillWorkCopy(s.root, req.ID, previous) }
	if err := workspace.WriteSkillWorkCopy(s.root, cand); err != nil {
		s.logger.Error("发布技能改写工作副本失败", "skill", req.ID, "operation_id", operationID, "error", err)
		if errors.Is(err, core.ErrImportRejected) {
			s.finish(ctx, operationID, core.OperationFailed, agentResultBadRequest, "技能候选未通过边界校验")
			return core.ResourcePublishResult{}, &core.AdminError{
				Kind:    core.ErrImportRejected,
				Message: err.Error(),
				Data:    map[string]any{"operation_id": operationID},
			}
		}
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "改写技能工作副本失败")
		return core.ResourcePublishResult{}, operationFailedError(operationID, "发布技能失败，旧版本已保留")
	}

	storagePath, err := workspace.SnapshotSkill(s.root, cand, uid, nextNo)
	if err != nil {
		return core.ResourcePublishResult{}, s.abortSkillWrite(ctx, operationID, req.ID,
			"技能版本快照失败", err, uid, nextNo, restore)
	}

	s.gate.Lock()
	version, err := s.publishVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceUID:  uid,
		ResourceID:   req.ID,
		ContentHash:  target.ContentHash,
		ManifestJSON: target.ManifestJSON,
		StoragePath:  storagePath,
		OperationID:  operationID,
	}, true, operationID, now)
	if err != nil {
		s.gate.Unlock()
		s.logger.Error("技能版本提交失败", "skill", req.ID, "operation_id", operationID, "error", err)
		return core.ResourcePublishResult{}, s.abortSkillWrite(ctx, operationID, req.ID,
			"技能版本提交失败", err, uid, nextNo, restore)
	}
	s.gate.Unlock()

	return core.ResourcePublishResult{Version: version, PendingRestart: false}, nil
}

// TestModelConnection 测试一条已发布模型连接（约束 50）：先写 pending，再按**已发布
// 版本**构造一次性客户端，用固定 15 秒超时发一次最小协议调用；不创建 Run、不写
// `llm_calls`、不动发布指针、不换运行时目录。结果脱敏后落在 `detail_json`：
//
//   - 调用成功 → 操作 success，`ok=true` + `latency_ms`。
//   - 连接失败 / 非超时错误 → 操作 success、`ok=false` + `error_code`（外部调用确实
//     发生了，操作记录的是"测试执行完毕"）；响应仍是 200，由 `data.ok` 表达结论。
//   - 超时（含请求被取消折成的超时）→ 操作 failed + `TIMEOUT`，错误映射 504。
//   - 凭证未配置 → 操作 failed + `NOT_CONFIGURED`，错误映射 503（点名变量名，不回显值）。
//
// TestModelConnection pre-writes a pending row, builds a one-shot client from the
// published version and issues exactly one minimal call. It never creates a run,
// never writes an LLM-call audit row and never swaps the runtime catalog.
func (s *ResourceAdmin) TestModelConnection(ctx context.Context, actor core.Actor, name string) (core.ResourceTestResult, error) {
	if err := validateResourceID(name); err != nil {
		return core.ResourceTestResult{}, &core.InvalidInputError{Tool: core.ActionModelConnectionTest, Reason: err.Error()}
	}

	unlockResource := s.resourceLocks.lock(resourceLockKey(core.ResourceModelConnection, name))
	defer unlockResource()

	pointer, err := s.activePointer(ctx, core.ResourceModelConnection, name)
	if err != nil {
		return core.ResourceTestResult{}, err
	}
	published, err := s.store.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return core.ResourceTestResult{}, fmt.Errorf("读取模型连接 %s 的发布版本: %w", name, err)
	}
	entry, err := providerEntryFromVersion(name, published)
	if err != nil {
		return core.ResourceTestResult{}, fmt.Errorf("读取模型连接 %s 的已发布条目: %w", name, err)
	}

	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionModelConnectionTest,
		TargetKind:   core.TargetKindModelConnection,
		TargetID:     name,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  modelConnectionTestHash(name),
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := s.operations.BeginOperation(ctx, record); err != nil {
		s.logger.Error("模型连接测试预写操作失败，拒绝执行", "resource", name, "error", err)
		return core.ResourceTestResult{}, fmt.Errorf("预写管理操作失败: %w", err)
	}

	client, err := buildModelConnectionTestClient(ctx, name, pointer, published, entry, s.credentialSource)
	if err != nil {
		detail, marshalErr := json.Marshal(modelTestOperationDetail{
			ErrorCode:    resourceResultNotConfigured,
			ErrorMessage: err.Error(),
		})
		if marshalErr != nil {
			detail = nil
		}
		s.finish(ctx, operationID, core.OperationFailed, resourceResultNotConfigured, "测试所需凭证未配置", detail)
		s.recordConnectionTest(ctx, published.ResourceUID, core.ConnectionTestFailed, err.Error(), operationID)
		return core.ResourceTestResult{}, &core.AdminError{
			Kind:    core.ErrProviderNotConfigured,
			Message: err.Error(),
			Data:    map[string]any{"operation_id": operationID},
		}
	}

	testCtx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	started := time.Now()
	_, callErr := client.Chat(testCtx, core.ChatRequest{
		Provider: name,
		Model:    entry.DefaultModel,
		Messages: []core.Message{{Role: "user", Content: modelTestPing}},
	})
	latency := time.Since(started).Milliseconds()

	result := core.ResourceTestResult{LatencyMS: latency, Model: entry.DefaultModel}
	detail := modelTestOperationDetail{LatencyMS: latency, Model: entry.DefaultModel}

	if callErr != nil {
		code, message := modelTestError(callErr)
		detail.ErrorCode = code
		detail.ErrorMessage = message
		payload, marshalErr := json.Marshal(detail)
		if marshalErr != nil {
			payload = nil
		}
		if modelTestTimedOut(callErr) {
			s.finish(ctx, operationID, core.OperationFailed, resourceResultTimeout, "模型连接测试超时", payload)
			s.recordConnectionTest(ctx, published.ResourceUID, core.ConnectionTestUnknown,
				"测试超时，结果未知", operationID)
			return result, &core.AdminError{
				Kind:    context.DeadlineExceeded,
				Message: fmt.Sprintf("模型连接测试超时（%s）", s.testTimeout),
				Data:    map[string]any{"operation_id": operationID},
			}
		}
		// 连接失败 / 外部错误：调用确实发生过，操作记 success，结论由 ok=false 表达。
		result.OK = false
		result.ErrorCode = code
		result.ErrorMessage = message
		s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", payload)
		s.recordConnectionTest(ctx, published.ResourceUID, core.ConnectionTestFailed, message, operationID)
		return result, nil
	}

	detail.OK = true
	result.OK = true
	payload, marshalErr := json.Marshal(detail)
	if marshalErr != nil {
		payload = nil
	}
	s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", payload)
	s.recordConnectionTest(ctx, published.ResourceUID, core.ConnectionTestSuccess,
		fmt.Sprintf("模型调用成功（%d ms）", latency), operationID)
	return result, nil
}

// recordConnectionTest 覆盖「最后一次连接测试」投影。写失败只记 ERROR：权威记录是
// 管理操作台账的终态 detail，把已完成的测试说成失败会让运维重复发起真实调用。
// recordConnectionTest upserts the projection; a failure is logged because the
// operation ledger is authoritative and a completed test is never reported as failed.
func (s *ResourceAdmin) recordConnectionTest(ctx context.Context, uid core.ResourceUID,
	status, detail, operationID string,
) {
	if s.connectionTests == nil || uid == "" {
		return
	}
	rec := core.ResourceConnectionTest{
		Kind:        core.ResourceModelConnection,
		ResourceUID: uid,
		TestedAt:    s.now().UTC(),
		Status:      status,
		Detail:      detail,
		OperationID: operationID,
	}
	if err := s.connectionTests.RecordConnectionTest(context.WithoutCancel(ctx), rec); err != nil {
		s.logger.Error("写入模型连接最后测试结果失败（操作台账仍为权威记录）",
			"resource_uid", string(uid), "operation_id", operationID, "error", err)
	}
}

// TestMCPServer 测试一条已发布 MCP server（约束 20–22）：先写 pending，再把**已发布
// 版本**解析回一条配置交给 `MCPSupervisor.Test`——策略判定与审计在 supervisor 内，
// 固定 15 秒预算，连接建立后立即关闭；不创建 Run、不写 `llm_calls`、不动 applied、
// 不改定义文件与发布指针。结果映射：
//
//   - 连接成功 → 操作 success，`ok=true` + 工具发现结果。
//   - 连接失败 / 策略拒绝 → 操作 success、`ok=false` + 固定短语（策略拒绝是 200，
//     失败判据表明确区分"被拒绝"与"测试失败"）。
//   - 超时 → 操作 failed + `TIMEOUT`，错误映射 504。
//
// supervisor 未装配（本进程没有 MCP 运行时）时 fail-closed：预写之前返回 503，
// 不伪造"连接失败"。测试所需的进程 / HTTP 侧效由 supervisor 的连接池负责回收。
// TestMCPServer pre-writes a pending row and delegates the one-off connection to the
// supervisor; it never creates a run, writes an LLM-call row or moves applied.
func (s *ResourceAdmin) TestMCPServer(ctx context.Context, actor core.Actor, name string) (core.ResourceTestResult, error) {
	if err := validateResourceID(name); err != nil {
		return core.ResourceTestResult{}, &core.InvalidInputError{Tool: core.ActionMCPServerTest, Reason: err.Error()}
	}
	if s.supervisor == nil {
		return core.ResourceTestResult{}, &core.AdminError{
			Kind:    core.ErrProviderNotConfigured,
			Message: "MCP 运行时尚未装配，无法执行连接测试",
		}
	}

	unlockResource := s.resourceLocks.lock(resourceLockKey(core.ResourceMCPServer, name))
	defer unlockResource()

	pointer, err := s.activePointer(ctx, core.ResourceMCPServer, name)
	if err != nil {
		return core.ResourceTestResult{}, err
	}
	published, err := s.store.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return core.ResourceTestResult{}, fmt.Errorf("读取 MCP server %s 的发布版本: %w", name, err)
	}
	cfg, err := workspace.ParseMCPServerBlock([]byte(published.DefinitionYAML))
	if err != nil {
		return core.ResourceTestResult{}, fmt.Errorf("读取 MCP server %s 的已发布定义: %w", name, err)
	}
	if cfg.Name != name {
		return core.ResourceTestResult{}, fmt.Errorf(
			"MCP server %s 的发布版本条目名 %q 与资源标识不一致", name, cfg.Name)
	}

	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionMCPServerTest,
		TargetKind:   core.TargetKindMCPServer,
		TargetID:     name,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  mcpServerTestHash(name),
		// 被测版本在**预写之前**固定：页面以 `requested_version_id` 为权威被测版本，
		// 只在响应里带版本的话，刷新后读操作详情就不知道测的是哪一版（D-03）。
		// The tested version is pinned before the prewrite; the operation row is the
		// authoritative record the page reads after a refresh.
		RequestedVersionID: pointer.PublishedVersionID,
		Status:             core.OperationPending,
		DetailJSON:         []byte("{}"),
		CreatedAt:          now,
	}
	if err := s.operations.BeginOperation(ctx, record); err != nil {
		s.logger.Error("MCP server 测试预写操作失败，拒绝执行", "resource", name, "error", err)
		return core.ResourceTestResult{}, fmt.Errorf("预写管理操作失败: %w", err)
	}

	// 已发布版本里的 env 值仍是 `${VAR}` 占位：连接前必须解析成终值，否则子进程会收到
	// 字面占位。解析失败与模型连接测试同口径——pending 已写，置 failed + 503 并返回
	// operation_id；错误里只有 server / 键 / 变量名，不含值（约束 7 / 22）。
	// Published env values are still ${VAR} placeholders; resolve them before connecting.
	resolved, resolveErr := resolveMCPEnv([]core.MCPServerConfig{cfg}, os.LookupEnv)
	if resolveErr != nil {
		detail, marshalErr := json.Marshal(mcpTestOperationDetail{
			Tools:        []string{},
			ErrorCode:    resourceResultNotConfigured,
			ErrorMessage: resolveErr.Error(),
			Stage:        mcpGateStagePreflight,
			FailureClass: mcpGateClassEnvMissing,
		})
		if marshalErr != nil {
			detail = nil
		}
		s.finish(ctx, operationID, core.OperationFailed, resourceResultNotConfigured, "测试所需环境变量未配置", detail)
		// 503 不返回测试结果对象：页面用 operation_id 拉操作详情读 stage / failure_class。
		return core.ResourceTestResult{}, &core.AdminError{
			Kind:    core.ErrProviderNotConfigured,
			Message: resolveErr.Error(),
			Data:    map[string]any{"operation_id": operationID},
		}
	}
	cfg = resolved[0]

	result := s.supervisor.Test(ctx, cfg)
	// 响应里的 operation_id / 被测版本与操作行同源：页面并发测试后按 operation_id 串行。
	result.OperationID = operationID
	result.TestedVersionID = pointer.PublishedVersionID

	payload, marshalErr := json.Marshal(mcpTestOperationDetailOf(result))
	if marshalErr != nil {
		payload = nil
	}
	if result.ErrorCode == resourceResultTimeout {
		s.finish(ctx, operationID, core.OperationFailed, resourceResultTimeout, "MCP server 连接测试超时", payload)
		// 504 同样不返回结果对象（D-03）：超时的 stage / class 只在操作详情里。
		return core.ResourceTestResult{}, &core.AdminError{
			Kind:    context.DeadlineExceeded,
			Message: fmt.Sprintf("MCP server 连接测试超时（%s）", defaultMCPTestTimeout),
			Data:    map[string]any{"operation_id": operationID},
		}
	}
	// 成功与被拒绝（含连接失败）都记 success：外部动作确实执行完毕，结论由 ok 表达。
	s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", payload)
	return result, nil
}

// References 返回资源的引用联合视图（约束 11 / 15）：`agents` 来自当前 Agent 装载
// 快照与归档目录扫描（解析失败者按"可能引用"点名），`active_runs` 来自未释放的
// `run_resource_refs`（**不**用"当前没有 Run"推断无引用）。已删除资源仍可查询——历史
// 版本与引用证据必须保留，因此这里用 `Pointer` 而不是 `activePointer`，只有指针行
// 不存在才 404。
// References unions the agent references (live snapshot plus archive scan) with the
// unreleased run references; deleted resources stay answerable from their pointer.
func (s *ResourceAdmin) References(ctx context.Context, kind core.ResourceKind, id string) (core.ResourceReferences, error) {
	if _, err := core.ParseResourceKind(string(kind)); err != nil {
		return core.ResourceReferences{}, &core.InvalidInputError{Tool: "references", Reason: err.Error()}
	}
	if err := validateResourceID(id); err != nil {
		return core.ResourceReferences{}, &core.InvalidInputError{Tool: "references", Reason: err.Error()}
	}

	pointer, err := s.store.Pointer(ctx, kind, id)
	if errors.Is(err, core.ErrResourceNotFound) {
		return core.ResourceReferences{}, fmt.Errorf("%w: %s %s", core.ErrResourceNotFound, kind, id)
	}
	if err != nil {
		return core.ResourceReferences{}, fmt.Errorf("读取 %s %s 的发布指针: %w", kind, id, err)
	}

	agents, err := s.agentReferences(kind, id)
	if err != nil {
		return core.ResourceReferences{}, err
	}
	boundAgents, err := s.boundAgentReferences(ctx, pointer.ResourceUID)
	if err != nil {
		return core.ResourceReferences{}, err
	}
	activeRuns, err := s.store.ReferencedBy(ctx, kind, id)
	if err != nil {
		return core.ResourceReferences{}, err
	}
	if activeRuns == nil {
		activeRuns = []string{}
	}
	return core.ResourceReferences{
		Kind:               kind,
		ID:                 id,
		ResourceUID:        pointer.ResourceUID,
		PublishedVersionID: pointer.PublishedVersionID,
		Agents:             agents,
		BoundAgents:        boundAgents,
		ActiveRuns:         activeRuns,
	}, nil
}

// boundAgentReferences 从绑定投影反查指向该 UID 的 Agent（R4.6 绑定权威）：按
// `agent_uid` 去重、保持仓储的名字顺序；归档标志取自未恢复的归档行。投影读取失败
// **不得**降级成"没有绑定"——引用视图是删除闸门之外的排障证据，缺证据要报错。
// boundAgentReferences reads the binding projection and marks each referrer archived;
// a projection read failure is an error, never an empty list.
func (s *ResourceAdmin) boundAgentReferences(ctx context.Context, uid core.ResourceUID) ([]core.ResourceBoundAgent, error) {
	if s.bindings == nil || uid == "" {
		return []core.ResourceBoundAgent{}, nil
	}
	rows, err := s.bindings.ReferencingAgents(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("反查资源 %s 的绑定投影: %w", uid, err)
	}
	archived, err := s.archivedAgentUIDs(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[core.AgentUID]bool, len(rows))
	out := make([]core.ResourceBoundAgent, 0, len(rows))
	for _, row := range rows {
		if seen[row.AgentUID] {
			continue
		}
		seen[row.AgentUID] = true
		out = append(out, core.ResourceBoundAgent{
			Agent:    row.AgentName,
			AgentUID: row.AgentUID,
			Archived: archived[row.AgentUID],
		})
	}
	return out, nil
}

// archivedAgentUIDs 汇总"有未恢复归档行"的 Agent 身份；装配未提供归档索引时返回空
// 集合（该装配无法回答归档问题，`archived` 保持 false）。
// archivedAgentUIDs collects the agent uids with an unrecovered archive row.
func (s *ResourceAdmin) archivedAgentUIDs(ctx context.Context) (map[core.AgentUID]bool, error) {
	out := map[core.AgentUID]bool{}
	if s.archives == nil {
		return out, nil
	}
	records, err := s.archives.ListArchives(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取归档索引以判定引用者状态: %w", err)
	}
	for _, rec := range records {
		if rec.RestoredAt == nil && rec.AgentUID != "" {
			out[rec.AgentUID] = true
		}
	}
	return out, nil
}

// Versions 返回某资源的全部版本行（新 → 旧，顺序由存储层保持）。只读：不写操作行、
// 不解析环境变量、不构造运行时对象。未知类型 / 非法标识 → `InvalidInputError`（400）；
// 指针行不存在（含"配置了但从未发布"）→ `core.ErrResourceNotFound`（404）；已删除资源
// 仍可查询——历史版本必须保留（约束 14）。
// Versions lists every version row of one resource, newest first; reads stay read-only
// and a deleted resource stays queryable from its pointer row.
func (s *ResourceAdmin) Versions(ctx context.Context, kind core.ResourceKind, id string) ([]core.ResourceVersion, error) {
	versions, _, err := s.versionsAndPointer(ctx, kind, id)
	return versions, err
}

// versionsAndPointer 一次读取版本行与发布指针：列表与详情共用同一入口，避免"资源
// 是否存在"在两处各判一套。指针行含已删除资源（`Pointer` 不过滤 `deleted_at`），
// 因此删除后的历史仍可读。
// versionsAndPointer reads the version rows and the pointer row in one place, including
// deleted resources, so history stays readable after deletion.
func (s *ResourceAdmin) versionsAndPointer(ctx context.Context, kind core.ResourceKind, id string) (
	[]core.ResourceVersion, *core.ResourcePointer, error,
) {
	if _, err := core.ParseResourceKind(string(kind)); err != nil {
		return nil, nil, &core.InvalidInputError{Tool: "versions", Reason: err.Error()}
	}
	if err := validateResourceID(id); err != nil {
		return nil, nil, &core.InvalidInputError{Tool: "versions", Reason: err.Error()}
	}

	pointer, err := s.store.Pointer(ctx, kind, id)
	if errors.Is(err, core.ErrResourceNotFound) {
		return nil, nil, fmt.Errorf("%w: %s %s", core.ErrResourceNotFound, kind, id)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("读取 %s %s 的发布指针: %w", kind, id, err)
	}

	// 按名读取只回**当前指针身份**的版本（R4.5）：同名重建前的旧 UID 历史不进当前
	// 实例的版本列表，需要旧历史时按 `resource_uid` 维度单独查。
	versions, err := s.versionsForPointer(ctx, kind, id, pointer)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 %s %s 的版本列表: %w", kind, id, err)
	}
	return versions, &pointer, nil
}

// versionsForPointer 按指针身份取版本行。指针行缺合法 UID 只可能来自迁移前的遗留
// 数据，此时退回名称快照，保证历史仍可读，不把只读面变成 500。
// versionsForPointer lists by pointer identity and falls back to the name snapshot for
// pre-migration rows that carry no uid.
func (s *ResourceAdmin) versionsForPointer(ctx context.Context, kind core.ResourceKind,
	id string, pointer core.ResourcePointer,
) ([]core.ResourceVersion, error) {
	if _, err := core.ParseResourceUID(string(pointer.ResourceUID)); err != nil {
		return s.store.Versions(ctx, kind, id)
	}
	return s.store.VersionsByUID(ctx, kind, pointer.ResourceUID)
}

// versionDetails 是 web 端口 `Versions` 的列表投影：版本行 + 指针状态 + 撤回来源。
// 定义原文与技能清单**不**在列表里返回（详情端点才有），因此这里不做脱敏扫描——
// 列表不携带任何凭证面。
// versionDetails is the web list projection; it never carries definitions, so no
// redaction scan is needed here.
func (s *ResourceAdmin) versionDetails(ctx context.Context, kind core.ResourceKind, id string) ([]core.ResourceVersionDetail, error) {
	versions, pointer, err := s.versionsAndPointer(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	details := make([]core.ResourceVersionDetail, len(versions))
	for i, version := range versions {
		published, applied, revertedFrom, err := s.versionFlags(ctx, version, pointer)
		if err != nil {
			return nil, err
		}
		details[i] = core.ResourceVersionDetail{
			Version:               version,
			Published:             published,
			Applied:               applied,
			RevertedFromVersionID: revertedFrom,
		}
	}
	return details, nil
}

// versionFlags 派生一个版本的两个指针状态位与撤回来源（约束 14）：`published` /
// `applied` 取自指针行（含已删除资源），撤回来源从创建该版本的操作行
// `requested_version_id` 派生；操作行缺失时保持 nil，不猜测。pointer 为 nil 表示
// 指针行缺失——详情仍可读，两个状态位保持 false。
// versionFlags derives the pointer flags and the revert source; a nil pointer keeps
// both flags false, and a missing operation row keeps the source nil.
func (s *ResourceAdmin) versionFlags(ctx context.Context, version core.ResourceVersion, pointer *core.ResourcePointer) (
	published, applied bool, revertedFrom *int64, err error,
) {
	if pointer != nil {
		published = pointer.PublishedVersionID == version.ID
		applied = pointer.AppliedVersionID != nil && *pointer.AppliedVersionID == version.ID
	}

	operation, err := s.operations.GetOperation(ctx, version.OperationID)
	switch {
	case errors.Is(err, core.ErrOperationNotFound):
		// 操作行缺失时撤回来源保持 nil——不把未知写成"非撤回"。
		return published, applied, nil, nil
	case err != nil:
		return false, false, nil, fmt.Errorf("读取版本 %d 的创建操作: %w", version.ID, err)
	case operation.RequestedVersionID > 0 && operation.RequestedVersionID != version.ID:
		source := operation.RequestedVersionID
		return published, applied, &source, nil
	default:
		return published, applied, nil, nil
	}
}

// VersionDetail 返回单个版本的只读详情（H2 / 约束 14 / 23）：版本行 + 定义原文
// （模型连接 / MCP 为块原文，技能恒为空串）+ 技能快照清单 + 指针状态 + 撤回来源。
// 只读——不写操作行、不解析环境变量、不构造运行时对象。`version_id` ≤ 0 与不存在
// 一律 `core.ErrResourceNotFound`（web 映射 404，不做 400 区分，避免按状态码枚举）。
//
// 三个派生字段的口径：
//   - `published` / `applied` 取自指针行（含已删除资源——历史版本仍可读）；
//   - `reverted_from_version_id` 从创建该版本的操作行 `requested_version_id` 派生
//     （约束 14：v2 表没有该列，不得为此加列）；操作行缺失时保持 nil，不猜测。
//
// 定义原文返回前做凭证扫描（`redactVersionDefinition`）：任何凭证值不是精确
// `${VAR}` 都 fail-closed 报错，不返回部分内容。
// VersionDetail is read-only: it never writes an operation row and derives the revert
// source from the creating operation. Credential-shaped content fails closed.
func (s *ResourceAdmin) VersionDetail(ctx context.Context, versionID int64) (core.ResourceVersionDetail, error) {
	if versionID <= 0 {
		return core.ResourceVersionDetail{}, fmt.Errorf("%w: 版本 %d", core.ErrResourceNotFound, versionID)
	}
	version, err := s.store.VersionByID(ctx, versionID)
	if err != nil {
		return core.ResourceVersionDetail{}, err
	}

	detail := core.ResourceVersionDetail{Version: version}
	switch version.Kind {
	case core.ResourceSkill:
		files, err := parseSkillManifest(version.ResourceID, version.ManifestJSON)
		if err != nil {
			return core.ResourceVersionDetail{}, err
		}
		detail.Manifest = make([]core.ResourceManifestFile, len(files))
		for i, file := range files {
			detail.Manifest[i] = core.ResourceManifestFile(file)
		}
	case core.ResourceModelConnection, core.ResourceMCPServer:
		if err := redactVersionDefinition(version); err != nil {
			// 日志只记资源标识与版本号，**不**记正文（H2）；错误文案同样不含正文。
			s.logger.Error("资源版本定义未脱敏，拒绝返回详情",
				"kind", version.Kind, "resource", version.ResourceID, "version_id", version.ID)
			return core.ResourceVersionDetail{}, err
		}
		detail.DefinitionYAML = version.DefinitionYAML
	default:
		return core.ResourceVersionDetail{}, fmt.Errorf("资源版本 %d 的类型未知: %q", version.ID, version.Kind)
	}

	var pointer *core.ResourcePointer
	current, err := s.store.Pointer(ctx, version.Kind, version.ResourceID)
	switch {
	case errors.Is(err, core.ErrResourceNotFound):
		// 版本行在而指针行缺失不是正常状态，但详情仍可读；两个状态位保持 false。
	case err != nil:
		return core.ResourceVersionDetail{}, fmt.Errorf(
			"读取 %s %s 的发布指针: %w", version.Kind, version.ResourceID, err)
	default:
		pointer = &current
	}

	published, applied, revertedFrom, err := s.versionFlags(ctx, version, pointer)
	if err != nil {
		return core.ResourceVersionDetail{}, err
	}
	detail.Published = published
	detail.Applied = applied
	detail.RevertedFromVersionID = revertedFrom
	return detail, nil
}

// credentialDefinitionKeys 是版本定义里必须保持精确 `${VAR}` 形态的凭证键（小写）。
// H2 的固定清单；比较前把键名折成小写，`API_KEY` 与 `api_key` 同档。
// credentialDefinitionKeys is H2's fixed list of credential keys, compared lowercase.
var credentialDefinitionKeys = map[string]struct{}{
	"api_key":  {},
	"token":    {},
	"secret":   {},
	"password": {},
}

// redactVersionDefinition 是版本详情的脱敏 fail-closed 闸门（H2）：把存储的定义原文
// 解析成通用树后递归扫描——
//   - 凭证键（`api_key` / `token` / `secret` / `password`）的值必须是精确 `${VAR}`；
//   - `env` 映射的每个值同样必须是精确 `${VAR}`（候选校验已如此要求，这里复核存储行）；
//   - 任何字符串值都不得携带 URL userinfo。
//
// 定义无法解析、节点类型无法识别或任一检查失败都返回错误；错误文案固定，不含发现的
// 值（不返回部分内容）。两种已落库形态（块式列表片段 / 顶层条目映射）都由同一棵
// 通用树覆盖，不需要分别处理。
// redactVersionDefinition walks the whole stored definition tree and fails closed; its
// fixed error text never carries the offending value.
func redactVersionDefinition(version core.ResourceVersion) error {
	if strings.TrimSpace(version.DefinitionYAML) == "" {
		return fmt.Errorf("资源版本 %d 缺少定义原文", version.ID)
	}
	var doc any
	if err := yaml.Unmarshal([]byte(version.DefinitionYAML), &doc); err != nil {
		return fmt.Errorf("资源版本 %d 的定义原文无法解析", version.ID)
	}
	if err := scanDefinitionCredentials(doc); err != nil {
		return fmt.Errorf("资源版本 %d 的定义含未脱敏凭证", version.ID)
	}
	return nil
}

// errDefinitionCredential 是扫描命中凭证风险的内部哨兵：调用方统一换成不含正文的
// 固定文案，避免把命中的值带进错误或日志。
// errDefinitionCredential marks a redaction hit; callers replace it with fixed text.
var errDefinitionCredential = errors.New("definition carries a non-placeholder credential")

// scanDefinitionCredentials 递归扫描定义树。未知节点类型按失败关闭处理——宁可拒绝
// 返回，也不把一个扫不到的结构当成"没有凭证"。
// scanDefinitionCredentials fails closed on any node shape it cannot classify.
func scanDefinitionCredentials(node any) error {
	switch value := node.(type) {
	case nil, bool, int, int64, uint64, float64:
		return nil
	case string:
		if definitionStringHasUserinfo(value) {
			return errDefinitionCredential
		}
		return nil
	case []any:
		for _, child := range value {
			if err := scanDefinitionCredentials(child); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for key, child := range value {
			lower := strings.ToLower(key)
			if _, credential := credentialDefinitionKeys[lower]; credential {
				text, ok := child.(string)
				if !ok {
					return errDefinitionCredential
				}
				if _, placeholder := core.EnvPlaceholderName(text); !placeholder {
					return errDefinitionCredential
				}
				continue
			}
			if lower == "env" {
				if err := checkDefinitionEnv(child); err != nil {
					return err
				}
				continue
			}
			if err := scanDefinitionCredentials(child); err != nil {
				return err
			}
		}
		return nil
	default:
		return errDefinitionCredential
	}
}

// checkDefinitionEnv 复核 `env` 映射：必须是映射，且每个值都是精确 `${VAR}`。
// checkDefinitionEnv re-checks that every env value is an exact placeholder.
func checkDefinitionEnv(node any) error {
	env, ok := node.(map[string]any)
	if !ok {
		return errDefinitionCredential
	}
	for _, raw := range env {
		text, ok := raw.(string)
		if !ok {
			return errDefinitionCredential
		}
		if _, placeholder := core.EnvPlaceholderName(text); !placeholder {
			return errDefinitionCredential
		}
	}
	return nil
}

// definitionStringHasUserinfo 判定字符串是否携带 URL userinfo（`scheme://user:pass@host`）。
// 只有像 URL 的字符串才解析；像 URL 却解析失败同样按失败关闭拒绝。
// definitionStringHasUserinfo parses URL-shaped strings only and fails closed on error.
func definitionStringHasUserinfo(text string) bool {
	if !strings.Contains(text, "://") {
		return false
	}
	parsed, err := url.Parse(text)
	if err != nil {
		return true
	}
	return parsed.User != nil
}

// Delete 删除一条公共资源（图 G / 约束 11 / 12）：
//
//	静态校验 → 资源锁 → 指针 404 → If-Match 412 → 文件锁 → 重读 → 分歧闸门 409 →
//	移除块 + 整份候选校验（全部在预写之前）→ 预写 pending → 引用闸门（活动 + 归档
//	Agent 与未释放运行引用；任一命中 400 点名、零写入）→ 原子写 → gate 外重建候选
//	Provider 映射表 → gate 写锁内原子替换目录 + `deleted_at`（同事务回填终态）→ 释放。
//
// `If-Match` 是**必填**：缺失按 412 处理（与发布路径的"空串 = 未声明"不同——失败判据
// 表把删除的缺失归入 412）。`resource_uid` 同样必填且必须与当前指针身份一致，
// 同名重建期间提交的旧页面因此无法删到新实例（409，不写操作行）。删除**不创建新版本**、
// 不删历史版本与引用行；同名重建换新 UID 并从 1 重新计数（R4.5 / SC-017）。
// Delete removes one resource: validation, conditional check and the block edit happen
// before the pending row; the reference gate then runs with a pre-written operation.
func (s *ResourceAdmin) Delete(ctx context.Context, actor core.Actor, kind core.ResourceKind, id, ifMatch,
	resourceUID string,
) error {
	action := deleteAction(kind)
	if _, err := core.ParseResourceKind(string(kind)); err != nil {
		return &core.InvalidInputError{Tool: action, Reason: err.Error()}
	}
	if err := validateResourceID(id); err != nil {
		return &core.InvalidInputError{Tool: action, Reason: err.Error()}
	}
	// 技能没有定义文件条目：走工作副本归档 + 指针删除的独立编排（图 G 技能分支），
	// 不进入下面的块移除路径。
	if kind == core.ResourceSkill {
		return s.deleteSkill(ctx, actor, id, ifMatch, resourceUID)
	}
	definitionPath := s.configPath
	blockKey := providersBlockKey
	commitDefinition := workspace.CommitProviderDefinition
	validateDefinition := func(doc []byte) error {
		_, err := workspace.ValidateProviderEntries(doc)
		return err
	}
	if kind == core.ResourceMCPServer {
		definitionPath = s.mcpServersPath()
		blockKey = mcpServersDefinitionsKey
		commitDefinition = workspace.CommitMCPServersDefinition
		validateDefinition = workspace.ValidateMCPServersDocument
	}

	unlockResource := s.resourceLocks.lock(resourceLockKey(kind, id))
	defer unlockResource()

	pointer, err := s.activePointer(ctx, kind, id)
	if err != nil {
		return err
	}
	uid, err := expectResourceUID(kind, id, resourceUID, pointer)
	if err != nil {
		return err
	}
	published, err := s.store.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return fmt.Errorf("读取 %s %s 的发布版本: %w", kind, id, err)
	}
	current, err := publishedResourceETag(ctx, s.credentials, pointer, published)
	if err != nil {
		return err
	}
	if ifMatch == "" || ifMatch != current {
		return preconditionError(current)
	}

	unlockFile := s.fileLocks.lock(definitionPath)
	defer unlockFile()

	original, err := readFileUnder(filepath.Dir(definitionPath), filepath.Base(definitionPath))
	if err != nil {
		return fmt.Errorf("读取 %s 定义 %s: %w", kind, definitionPath, err)
	}
	if _, err := workspace.BlockEntries(original, blockKey); err != nil {
		s.logger.Warn("资源定义块不可安全编辑", "kind", string(kind), "resource", id, "action", action, "error", err)
		return editUnsupportedError()
	}
	fileMatches := definitionBlockMatchesFor(original, blockKey, id, published)
	// 崩溃窗口的重试（约束 13）：上次删除已把目标块移出文件、但指针事务未提交时，
	// 文件侧已在终态。此时按幂等补齐继续走引用闸门与删除标记，**不**重建块——
	// 重建会把刚删掉的条目悄悄复活。解析不出块形态的文件不算"已无块"。
	blockGone := !fileMatches && definitionBlockAbsent(original, blockKey, id)
	if !fileMatches && !blockGone {
		s.logger.Warn("定义文件与已发布版本不一致，拒绝删除", "resource", id, "expected_etag", current)
		return &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "定义文件与已发布版本不一致，请先对账或重新读取后再删除",
		}
	}
	candidate := original
	if blockGone {
		s.logger.Warn("删除重试：定义文件已无目标条目，按幂等补齐处理",
			"kind", string(kind), "resource", id)
	} else {
		var commentsLost bool
		candidate, commentsLost, err = workspace.RemoveEntry(original, blockKey, id)
		if err != nil {
			s.logger.Warn("资源块移除失败", "kind", string(kind), "resource", id, "error", err)
			return editUnsupportedError()
		}
		if commentsLost {
			// 删除整条条目，条目内注释随条目一起消失是删除语义的一部分（与整条替换不同：
			// 后者会留下"看起来还在"的注释残骸）。记录 WARN，不拒绝。
			s.logger.Warn("删除资源条目会一并移除条目内注释", "kind", string(kind), "resource", id)
		}
		// 移除后的整份候选必须仍然合法：分歧闸门只比对目标块的哈希，块外若已被手工改坏
		// 会在这里失败关闭——不能把一份启动时会被判无效的文件留在磁盘上。
		if err := validateDefinition(candidate); err != nil {
			return &core.InvalidInputError{Tool: action, Reason: err.Error()}
		}
	}

	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   resourceTargetKind(kind),
		TargetID:     id,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  resourceDeleteHash(kind, id, ifMatch),
		ExpectedETag: ifMatch,
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := s.operations.BeginOperation(ctx, record); err != nil {
		s.logger.Error("删除资源预写操作失败，拒绝执行", "resource", id, "action", action, "error", err)
		return fmt.Errorf("预写管理操作失败: %w", err)
	}

	agents, activeRuns, err := s.referenceHits(ctx, kind, id)
	if err != nil {
		s.logger.Error("删除资源引用检查失败，按未验证拒绝执行",
			"resource", id, "operation_id", operationID, "error", err)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "引用检查失败")
		return operationFailedError(operationID, "删除前的引用检查失败，未做任何修改")
	}
	if len(agents) > 0 || len(activeRuns) > 0 {
		message := resourceReferencedMessage(kind, id, agents, activeRuns)
		s.logger.Warn("资源正在被引用，拒绝删除",
			"resource", id, "operation_id", operationID, "agents", agents, "active_runs", activeRuns)
		detail, marshalErr := json.Marshal(map[string]any{
			"agents":      agents,
			"active_runs": activeRuns,
		})
		if marshalErr != nil {
			detail = nil
		}
		s.finish(ctx, operationID, core.OperationFailed, agentResultBadRequest, message, detail)
		return &core.AdminError{
			Kind:    &core.InvalidInputError{Tool: action, Reason: message},
			Message: message,
			Data: map[string]any{
				"agents": agents, "active_runs": activeRuns, "operation_id": operationID,
			},
		}
	}

	if !blockGone {
		if err := commitDefinition(definitionPath, candidate); err != nil {
			s.logger.Error("写入资源定义失败", "kind", string(kind), "resource", id, "operation_id", operationID, "error", err)
			s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "写入定义文件失败")
			return operationFailedError(operationID, "写入资源定义失败")
		}
	}

	if kind == core.ResourceMCPServer {
		return s.finishMCPDelete(ctx, operationID, now, definitionPath, candidate, original, id, uid)
	}

	// 候选映射表在 gate 之外按"移除目标后的已发布集合"重建：构建失败不换运行时目录，
	// 按约束 36 做文件身份检查后恢复原字节（文件已写、指针未动）。目标在解析任何占位
	// 之前就被跳过——它自己的 `${VAR}` 可能已撤掉，删除不能被自己的凭证缺失挡住。
	candidateCatalog, err := newProviderCatalogFromPublishedExcept(ctx, s.store, s.providerOverride,
		s.logger, id, s.credentialSource)
	if err != nil {
		s.logger.Error("删除模型连接后重建 Provider 映射表失败，按身份检查恢复定义文件",
			"resource", id, "operation_id", operationID, "error", err)
		status := core.OperationFailed
		message := "重建 Provider 映射表失败"
		if !s.restoreDefinitionFile(definitionPath, candidate, original, commitDefinition) {
			status = core.OperationUnknown
			message = "重建 Provider 映射表失败且定义文件身份检查不通过，结果待查证"
		}
		s.finish(ctx, operationID, status, lifecycleResultInternal, message)
		return operationFailedError(operationID, "删除模型连接失败，定义文件与发布指针未变更")
	}

	s.gate.Lock()
	previous := s.catalog.Load()
	s.catalog.Store(candidateCatalog)
	if err := s.store.MarkDeletedByUID(ctx, kind, id, uid, operationID, now); err != nil {
		// MarkDeleted 与终态同事务：失败即整笔回滚（指针未动），先把运行时目录换回旧
		// 快照，再做文件身份检查恢复——补偿失败一律 unknown，不谎报 failed。
		if previous != nil {
			s.catalog.Store(previous)
		}
		s.gate.Unlock()
		s.logger.Error("删除资源事务失败，按身份检查恢复定义文件",
			"resource", id, "operation_id", operationID, "error", err)
		status := core.OperationFailed
		message := "删除事务失败"
		if !s.restoreDefinitionFile(definitionPath, candidate, original, commitDefinition) {
			status = core.OperationUnknown
			message = "删除事务失败且定义文件身份检查不通过，结果待查证"
		}
		s.finish(ctx, operationID, status, lifecycleResultInternal, message)
		return operationFailedError(operationID, "删除模型连接失败，发布指针未变更")
	}
	s.gate.Unlock()
	return nil
}

// skillWorkCopyAbsent 判定 `skills/<name>` 目录是否**整体不存在**：这是删除崩溃窗口
// 重试的幂等补齐条件（约束 13）。只有目录本身缺席才算"上次删除已把工作副本移入归档"；
// 目录仍在但缺 `SKILL.md`（或读不出来）属于外部改动，继续按分歧闸门拒绝。符号链接
// 按"存在"处理——不跟随、不当作已移走。
// skillWorkCopyAbsent reports that the whole work-copy directory is gone, which is the
// idempotent-retry condition for a delete interrupted after the archive move.
func (s *ResourceAdmin) skillWorkCopyAbsent(name string) (bool, error) {
	relDir, err := workspace.SkillWorkCopyRelDir(name)
	if err != nil {
		return false, err
	}
	base, err := os.OpenRoot(s.root)
	if err != nil {
		return false, fmt.Errorf("打开工作区目录 %s: %w", s.root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			s.logger.Warn("关闭工作区目录失败", "root", s.root, "error", closeErr)
		}
	}()
	if _, statErr := base.Lstat(filepath.FromSlash(relDir)); statErr == nil {
		return false, nil
	} else if errors.Is(statErr, fs.ErrNotExist) {
		return true, nil
	} else {
		return false, fmt.Errorf("检查技能工作副本 %s: %w", relDir, statErr)
	}
}

// deleteSkill 是 `DELETE /skills/{name}` 的删除编排（图 G 技能分支）：
//
//	资源锁（兼任工作副本文件锁）→ 指针 404 → If-Match 412 → 锁内重读分歧闸门 409
//	（以上都不写操作行）→ 预写 pending → 引用闸门（活动 / 归档 Agent 与未释放运行
//	引用；命中 400 点名 + 操作 failed）→ 工作副本移入 archive/skills/<name>-<stamp> →
//	gate 写锁内同事务置 `deleted_at`。**不新增版本**、不删历史版本与引用行。
//
// 归档后的提交失败先做身份检查（工作副本路径必须仍为空）再回移；回移失败置
// unknown，不谎报 failed。
// deleteSkill archives the work copy and marks the pointer deleted; a failed commit
// moves the archive back only when the work-copy path is still free.
func (s *ResourceAdmin) deleteSkill(ctx context.Context, actor core.Actor, name, ifMatch,
	resourceUID string,
) error {
	action := core.ActionSkillDelete
	unlockResource := s.resourceLocks.lock(resourceLockKey(core.ResourceSkill, name))
	defer unlockResource()

	pointer, err := s.activePointer(ctx, core.ResourceSkill, name)
	if err != nil {
		return err
	}
	uid, err := expectResourceUID(core.ResourceSkill, name, resourceUID, pointer)
	if err != nil {
		return err
	}
	published, err := s.store.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return fmt.Errorf("读取技能 %s 的发布版本: %w", name, err)
	}
	current, err := publishedResourceETag(ctx, s.credentials, pointer, published)
	if err != nil {
		return err
	}
	if ifMatch == "" || ifMatch != current {
		return preconditionError(current)
	}
	// 工作副本整体缺席 = 上次删除已把它移入归档、但指针事务未提交（约束 13 的幂等
	// 补齐）：文件侧已在终态，跳过分歧闸门，继续走引用检查与删除标记。目录仍在时
	// 仍按工作副本哈希对账，不一致照旧 409。
	workCopyAbsent, err := s.skillWorkCopyAbsent(name)
	if err != nil {
		return err
	}
	if !workCopyAbsent {
		if _, err := s.skillWorkCopyState(name, published); err != nil {
			return err
		}
	}

	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   core.TargetKindSkill,
		TargetID:     name,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  resourceDeleteHash(core.ResourceSkill, name, ifMatch),
		ExpectedETag: ifMatch,
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}
	if err := s.operations.BeginOperation(ctx, record); err != nil {
		s.logger.Error("删除技能预写操作失败，拒绝执行", "skill", name, "error", err)
		return fmt.Errorf("预写管理操作失败: %w", err)
	}

	agents, activeRuns, err := s.referenceHits(ctx, core.ResourceSkill, name)
	if err != nil {
		s.logger.Error("删除技能引用检查失败，按未验证拒绝执行",
			"skill", name, "operation_id", operationID, "error", err)
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "引用检查失败")
		return operationFailedError(operationID, "删除前的引用检查失败，未做任何修改")
	}
	if len(agents) > 0 || len(activeRuns) > 0 {
		message := resourceReferencedMessage(core.ResourceSkill, name, agents, activeRuns)
		s.logger.Warn("技能正在被引用，拒绝删除",
			"skill", name, "operation_id", operationID, "agents", agents, "active_runs", activeRuns)
		detail, marshalErr := json.Marshal(map[string]any{
			"agents":      agents,
			"active_runs": activeRuns,
		})
		if marshalErr != nil {
			detail = nil
		}
		s.finish(ctx, operationID, core.OperationFailed, agentResultBadRequest, message, detail)
		return &core.AdminError{
			Kind:    &core.InvalidInputError{Tool: action, Reason: message},
			Message: message,
			Data: map[string]any{
				"agents": agents, "active_runs": activeRuns, "operation_id": operationID,
			},
		}
	}

	archiveRel := ""
	if workCopyAbsent {
		s.logger.Warn("删除重试：技能工作副本已不存在，按幂等补齐处理", "skill", name)
	} else {
		archiveRel, err = workspace.ArchiveSkillWorkCopy(s.root, name, now)
		if err != nil {
			s.logger.Error("技能工作副本归档失败", "skill", name, "operation_id", operationID, "error", err)
			if errors.Is(err, core.ErrConflict) {
				s.finish(ctx, operationID, core.OperationFailed, resourceResultConflict, err.Error())
				return &core.AdminError{
					Kind:    core.ErrConflict,
					Message: err.Error(),
					Data:    map[string]any{"operation_id": operationID},
				}
			}
			s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal, "归档技能工作副本失败")
			return operationFailedError(operationID, "删除技能失败：工作副本归档失败")
		}
	}

	s.gate.Lock()
	err = s.store.MarkDeletedByUID(ctx, core.ResourceSkill, name, uid, operationID, now)
	s.gate.Unlock()
	if err != nil {
		s.logger.Error("删除技能事务失败，按身份检查回移工作副本",
			"skill", name, "operation_id", operationID, "error", err)
		status := core.OperationFailed
		message := "删除事务失败"
		// 幂等补齐路径本次没有移动工作副本：没有可回滚的文件动作。
		if archiveRel != "" {
			if restoreErr := restoreSkillArchive(s.root, archiveRel, name); restoreErr != nil {
				status = core.OperationUnknown
				message = "删除事务失败且工作副本回移失败，结果待查证"
				s.logger.Error("技能工作副本回移失败",
					"skill", name, "operation_id", operationID, "archive", archiveRel, "error", restoreErr)
			}
		}
		s.finish(ctx, operationID, status, lifecycleResultInternal, message)
		return operationFailedError(operationID, "删除技能失败，发布指针未变更")
	}
	return nil
}

// restoreSkillArchive 把刚移入归档区的工作副本移回 `skills/<name>`，是删除提交失败的
// 补偿步骤。身份检查：目标路径已被占用（其它写入已落下新副本）→ 拒绝覆盖并报错，由
// 调用方把操作置 unknown；只有目标仍为空时才 rename 回去。
// restoreSkillArchive moves the archive back only when the work-copy path is free.
func restoreSkillArchive(root, archiveRel, name string) error {
	relDir, err := workspace.SkillWorkCopyRelDir(name)
	if err != nil {
		return err
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	if _, statErr := base.Lstat(filepath.FromSlash(relDir)); statErr == nil {
		return fmt.Errorf("工作副本路径 %s 已被占用，拒绝覆盖归档", relDir)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("检查工作副本路径 %s: %w", relDir, statErr)
	}
	if err := base.Rename(filepath.FromSlash(archiveRel), filepath.FromSlash(relDir)); err != nil {
		return fmt.Errorf("回移归档 %s → %s: %w", archiveRel, relDir, err)
	}
	return nil
}

// finishMCPDelete 是 MCP 删除的运行时与数据库尾段（图 G）：gate 写锁内先断开连接
// （`Stop` 幂等，无连接即成功），再写 `deleted_at`。任一步失败都先释放 gate，再做
// 文件身份检查恢复定义——指针未动、补偿失败置 unknown。
// finishMCPDelete tears the connection down inside the gate write lock, then marks
// the pointer deleted; failures restore the file only after an identity check.
func (s *ResourceAdmin) finishMCPDelete(ctx context.Context, operationID string, now time.Time,
	path string, written, original []byte, name string, uid core.ResourceUID,
) error {
	s.gate.Lock()
	if s.supervisor != nil {
		if err := s.supervisor.Stop(ctx, name); err != nil {
			s.gate.Unlock()
			s.logger.Error("MCP server 断开连接失败，按身份检查恢复定义文件",
				"resource", name, "operation_id", operationID, "error", err)
			status := core.OperationFailed
			message := "断开 MCP 连接失败"
			if !s.restoreDefinitionFile(path, written, original, workspace.CommitMCPServersDefinition) {
				status = core.OperationUnknown
				message = "断开 MCP 连接失败且定义文件身份检查不通过，结果待查证"
			}
			s.finish(ctx, operationID, status, lifecycleResultInternal, message)
			return operationFailedError(operationID, "删除 MCP server 失败，定义文件与发布指针未变更")
		}
	}
	if err := s.store.MarkDeletedByUID(ctx, core.ResourceMCPServer, name, uid, operationID, now); err != nil {
		s.gate.Unlock()
		s.logger.Error("删除 MCP server 事务失败，按身份检查恢复定义文件",
			"resource", name, "operation_id", operationID, "error", err)
		status := core.OperationFailed
		message := "删除事务失败"
		if !s.restoreDefinitionFile(path, written, original, workspace.CommitMCPServersDefinition) {
			status = core.OperationUnknown
			message = "删除事务失败且定义文件身份检查不通过，结果待查证"
		}
		s.finish(ctx, operationID, status, lifecycleResultInternal, message)
		return operationFailedError(operationID, "删除 MCP server 失败，发布指针未变更")
	}
	s.gate.Unlock()
	return nil
}

// referenceHits 是删除闸门与引用端点共用的引用收集：活动 / 归档 Agent 与未释放的
// 运行引用两路**都要**查，任何一路失败都 fail-closed（返回错误，由调用方拒绝执行）。
// referenceHits collects both reference sources and fails closed on either error.
func (s *ResourceAdmin) referenceHits(ctx context.Context, kind core.ResourceKind, id string) ([]string, []string, error) {
	agents, err := s.agentReferences(kind, id)
	if err != nil {
		return nil, nil, err
	}
	activeRuns, err := s.store.ReferencedBy(ctx, kind, id)
	if err != nil {
		return nil, nil, err
	}
	if activeRuns == nil {
		activeRuns = []string{}
	}
	return agents, activeRuns, nil
}

// agentReferences 收集 Agent 侧引用：活动 Agent 走装载快照（含未注册条目——它们的
// AGENT.md 同样在引用资源，删掉资源会让下一次装载失败），归档 Agent 走归档目录扫描。
// 快照里 `Profile == nil` 的条目是解析失败，按"可能引用"点名；归档侧任何读取 / 解析
// 失败同样计入，点名为 `archive:<archive_id>`。结果去重并升序，同一份输入两次调用
// 得到同一顺序（错误文案与视图都不依赖 map 遍历）。
// agentReferences unions live and archived agent references, counting anything
// unparseable as a possible reference and returning a sorted, de-duplicated list.
func (s *ResourceAdmin) agentReferences(kind core.ResourceKind, id string) ([]string, error) {
	refs := make([]string, 0)
	if s.agents != nil {
		entries, err := s.agents()
		if err != nil {
			return nil, fmt.Errorf("读取 Agent 装载快照: %w", err)
		}
		for _, entry := range entries {
			if entry.Profile == nil {
				s.logger.Warn("Agent 定义不可解析，按可能引用计入删除闸门",
					"agent", entry.Name, "resource_kind", string(kind), "resource_id", id)
				refs = append(refs, entry.Name+unparseableAgentMarker)
				continue
			}
			if profileReferencesResource(entry.Profile, kind, id) {
				refs = append(refs, entry.Name)
			}
		}
	}

	archived, err := s.archivedAgentReferences(kind, id)
	if err != nil {
		return nil, err
	}
	refs = append(refs, archived...)
	slices.Sort(refs)
	return slices.Compact(refs), nil
}

// archivedAgentReferences 扫描工作区归档目录（`<root>/archive/<archive_id>/AGENT.md`）：
// 只读普通目录条目、不跟随符号链接；缺 `AGENT.md` 的归档目录不可能引用任何资源，跳过；
// 读取或解析失败按"可能引用"计入并点名 `archive:<archive_id>`（fail-closed）。
// 归档目录本身不存在（未归档过任何 Agent）是正常状态，返回空列表。
// archivedAgentReferences scans archived AGENT.md files and treats unreadable or
// unparseable archives as possible references.
func (s *ResourceAdmin) archivedAgentReferences(kind core.ResourceKind, id string) ([]string, error) {
	if s.root == "" {
		return nil, nil
	}
	base, err := os.OpenRoot(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("打开工作区根 %s: %w", s.root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	dir, err := base.Open(agentArchiveDirName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("打开 Agent 归档目录: %w", err)
	}
	defer func() {
		_ = dir.Close()
	}()

	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("读取 Agent 归档目录: %w", err)
	}

	refs := make([]string, 0)
	for _, entry := range entries {
		// 只有目录才是归档单元；符号链接与普通文件都不读（不跟随、不越界）。
		if !entry.IsDir() {
			continue
		}
		archiveID := entry.Name()
		name := "archive:" + archiveID
		rel := filepath.FromSlash(path.Join(agentArchiveDirName, archiveID, authorDraftDefinitionPath))
		raw, readErr := readArchivedAgentDefinition(base, rel)
		if errors.Is(readErr, fs.ErrNotExist) {
			continue
		}
		if readErr != nil {
			s.logger.Warn("归档 Agent 定义不可读，按可能引用计入删除闸门",
				"archive_id", archiveID, "error", readErr)
			refs = append(refs, name)
			continue
		}
		dto, parseErr := parseArchivedAgentFrontmatter(raw)
		if parseErr != nil {
			s.logger.Warn("归档 Agent 定义不可解析，按可能引用计入删除闸门",
				"archive_id", archiveID, "error", parseErr)
			refs = append(refs, name)
			continue
		}
		if draftReferencesResource(dto, kind, id) {
			refs = append(refs, name)
		}
	}
	return refs, nil
}

// readArchivedAgentDefinition 在 `os.Root` 内读取归档定义：只接受普通文件（符号链接
// 由 Root 约束在根内，越界打不开），超过上限按错误处理，由调用方计入可能引用。
// readArchivedAgentDefinition reads one archived definition inside the workspace root.
func readArchivedAgentDefinition(base *os.Root, rel string) ([]byte, error) {
	file, err := base.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("归档定义不是普通文件")
	}
	if info.Size() > maxArchivedAgentBytes {
		return nil, fmt.Errorf("归档定义超过 %d 字节", maxArchivedAgentBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxArchivedAgentBytes))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// parseArchivedAgentFrontmatter 用作者草稿的同一个窄解析器读归档定义：只取引用相关的
// 三个字段，未知键忽略（归档文件来自装载期校验过的 AGENT.md）。
// parseArchivedAgentFrontmatter reads only the reference-relevant frontmatter fields.
func parseArchivedAgentFrontmatter(raw []byte) (authorDraftFrontmatter, error) {
	front, err := splitDraftFrontmatter(string(raw))
	if err != nil {
		return authorDraftFrontmatter{}, err
	}
	var dto authorDraftFrontmatter
	if err := yaml.Unmarshal(front, &dto); err != nil {
		return authorDraftFrontmatter{}, fmt.Errorf("归档定义 frontmatter 不是合法 YAML: %w", err)
	}
	return dto, nil
}

// profileReferencesResource 判定一份已解析的 Agent 是否引用目标资源：三类资源各查
// 自己的声明字段，**不**做模糊匹配（整词边界只属于通知渠道约束 38）。
// profileReferencesResource checks the one declaration field of the resource kind.
func profileReferencesResource(profile *core.Profile, kind core.ResourceKind, id string) bool {
	if profile == nil {
		return false
	}
	switch kind {
	case core.ResourceModelConnection:
		return profile.Provider.Name == id
	case core.ResourceMCPServer:
		return slices.Contains(profile.MCPServers, id)
	case core.ResourceSkill:
		return slices.Contains(profile.Skills, id)
	}
	return false
}

// draftReferencesResource 是归档定义版本的同一判定。
// draftReferencesResource applies the same rule to an archived definition.
func draftReferencesResource(dto authorDraftFrontmatter, kind core.ResourceKind, id string) bool {
	switch kind {
	case core.ResourceModelConnection:
		return dto.Provider.Name == id
	case core.ResourceMCPServer:
		return slices.Contains(dto.MCP, id)
	case core.ResourceSkill:
		return slices.Contains(dto.Skills, id)
	}
	return false
}

// resourceReferencedMessage 是 400 的点名文案：固定短语 + 两边名单，不含定义正文。
// resourceReferencedMessage builds the fixed BAD_REQUEST message with both name lists.
func resourceReferencedMessage(kind core.ResourceKind, id string, agents, activeRuns []string) string {
	parts := make([]string, 0, 2)
	if len(agents) > 0 {
		parts = append(parts, "Agents: "+strings.Join(agents, ", "))
	}
	if len(activeRuns) > 0 {
		parts = append(parts, "活动运行: "+strings.Join(activeRuns, ", "))
	}
	return fmt.Sprintf("%s %s 正在被引用，解除引用后才能删除（%s）", kind, id, strings.Join(parts, "；"))
}

// deleteAction 给出删除动作字面量（MCP / 技能在各自任务接入同一签名）。
// deleteAction returns the delete action literal for the kind.
func deleteAction(kind core.ResourceKind) string {
	switch kind {
	case core.ResourceMCPServer:
		return core.ActionMCPServerDelete
	case core.ResourceSkill:
		return core.ActionSkillDelete
	default:
		return core.ActionModelConnectionDelete
	}
}

// resourceTargetKind 把资源种类映射为审计目标类型（与动作字面量同源）。
// resourceTargetKind maps the resource kind to its audit target kind.
func resourceTargetKind(kind core.ResourceKind) string {
	switch kind {
	case core.ResourceMCPServer:
		return core.TargetKindMCPServer
	case core.ResourceSkill:
		return core.TargetKindSkill
	default:
		return core.TargetKindModelConnection
	}
}

// resourceDeleteHash 摘要一次删除请求：资源身份与期望 ETag（材料不含定义正文）。
// resourceDeleteHash digests one delete request.
func resourceDeleteHash(kind core.ResourceKind, id, ifMatch string) string {
	return agentRequestHash(strings.Join([]string{string(kind), id, ifMatch}, "\n"))
}

// mcpServersPath 是工作区里 MCP 定义文件的路径（与 `workspace.mcpServersFile`
// 同一个字面量；本层做块编辑与文件锁需要路径）。
// mcpServersPath resolves the MCP definition file inside the workspace root.
func (s *ResourceAdmin) mcpServersPath() string {
	return filepath.Join(s.root, mcpServersFileName)
}

// mcpServerRequestHash 摘要一次 MCP 写请求：动作、标识、传输与公开字段；env 只含
// 键与占位（占位本身就是请求内容），排序后拼接以保证同一请求摘要稳定。
// mcpServerRequestHash digests one MCP write request with sorted env placeholders.
func mcpServerRequestHash(action string, req MCPServerRequest) string {
	keys := make([]string, 0, len(req.Env))
	for key := range req.Env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+req.Env[key])
	}
	return agentRequestHash(strings.Join([]string{
		action, req.Name, req.Transport, req.Command,
		strings.Join(req.Args, "\x00"), req.URL, strings.Join(env, "\x00"),
	}, "\n"))
}

// mcpServerTestHash 摘要一次 MCP 测试请求：测试没有请求体，材料只有资源标识。
// mcpServerTestHash digests the MCP test request.
func mcpServerTestHash(name string) string {
	return agentRequestHash(core.ActionMCPServerTest + "\n" + name)
}

// commitModelConnection 是两段写共用的提交段：在同一 gate 写锁内把版本 / 指针 / 终态
// 交给 `PublishVersion`（单事务），成功才原子替换运行时目录。失败时按约束 36 逆序
// 补偿：先做文件身份检查再恢复原字节；身份不符或恢复失败 → 操作置 unknown。
// commitModelConnection runs the shared commit segment: one transaction under the gate
// write lock, then the catalog swap; failures restore the file only after an identity
// check and otherwise leave the operation unknown.
func (s *ResourceAdmin) commitModelConnection(ctx context.Context, operationID string, now time.Time,
	path string, written, original []byte, record core.ResourceVersion, name string,
	client core.ProviderClient, model string,
) (core.ResourceVersion, error) {
	s.gate.Lock()
	// 模型连接发布即生效：applied 与 published 同步推进（MCP 走 commitMCPServer，传 false）。
	version, err := s.publishVersion(ctx, record, true, operationID, now)
	if err != nil {
		s.gate.Unlock()
		s.logger.Error("模型连接发布事务失败，按身份检查恢复定义文件",
			"resource", name, "operation_id", operationID, "error", err)
		status := core.OperationFailed
		message := "发布事务失败"
		if !s.restoreDefinitionFile(path, written, original, workspace.CommitProviderDefinition) {
			status = core.OperationUnknown
			message = "发布事务失败且定义文件身份检查不通过，结果待查证"
		}
		s.finish(ctx, operationID, status, lifecycleResultInternal, message)
		return core.ResourceVersion{}, operationFailedError(operationID, "发布模型连接失败，旧版本已保留")
	}
	if client != nil {
		// managed 新 Key 的客户端在凭据落库后才构建（`adoptManagedClient`）：此刻目录
		// 不换入半成品，也不把 nil 当客户端写进去。
		s.catalog.Store(s.catalog.Load().with(name, client, model))
	}
	s.gate.Unlock()
	return version, nil
}

// commitMCPServer 是 MCP 写路径的提交段：同一 gate 写锁内把版本 / 指针 / 终态交给
// `PublishVersion`，`applied=false` 保留上一次实际生效的版本（约束 9：发布 ≠ 生效，
// 下一次成功连接才推进 applied）。运行时不在这里换——supervisor 的 applied 由连接
// 成功路径回写（约束 20）。失败同样先做文件身份检查再恢复 MCP 定义文件。
// commitMCPServer publishes without advancing applied and never touches the runtime;
// failures restore the MCP definition only after an identity check.
func (s *ResourceAdmin) commitMCPServer(ctx context.Context, operationID string, now time.Time,
	path string, written, original []byte, record core.ResourceVersion, name string,
) (core.ResourceVersion, error) {
	s.gate.Lock()
	version, err := s.publishVersion(ctx, record, false, operationID, now)
	if err != nil {
		s.gate.Unlock()
		s.logger.Error("MCP server 发布事务失败，按身份检查恢复定义文件",
			"resource", name, "operation_id", operationID, "error", err)
		status := core.OperationFailed
		message := "发布事务失败"
		if !s.restoreDefinitionFile(path, written, original, workspace.CommitMCPServersDefinition) {
			status = core.OperationUnknown
			message = "发布事务失败且定义文件身份检查不通过，结果待查证"
		}
		s.finish(ctx, operationID, status, lifecycleResultInternal, message)
		return core.ResourceVersion{}, operationFailedError(operationID, "发布 MCP server 失败，旧版本已保留")
	}
	s.gate.Unlock()
	return version, nil
}

// restoreDefinitionFile 在提交段失败后把定义文件恢复为写入前的字节：先重读并比较
// 当前字节与"本次写入的候选"是否一致（身份检查），不一致说明文件已被其他写入改动，
// **不覆盖**并报告失败（调用方置 unknown）；一致才原子写回原字节。`commit` 是按定义
// 文件种类选择的校验 + 原子写入口（provider / MCP 各一份），恢复同样不接受非法内容。
// restoreDefinitionFile restores the pre-write bytes only after an identity check;
// `commit` is the kind-specific validate-and-write entry, so a restore can never
// leave an invalid document behind either.
func (s *ResourceAdmin) restoreDefinitionFile(path string, written, original []byte, commit func(string, []byte) error) bool {
	if bytes.Equal(written, original) {
		// 本次没有改写文件（崩溃窗口的幂等补齐）：文件已经在原字节上，没有可回滚的
		// 文件动作，也就没有"身份检查失败"可言。
		return true
	}
	current, err := readFileUnder(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		s.logger.Error("补偿读取定义文件失败，无法确认身份", "error", err)
		return false
	}
	if !bytes.Equal(current, written) {
		s.logger.Error("定义文件已被其他写入改动，不覆盖，留待查证")
		return false
	}
	if err := commit(path, original); err != nil {
		s.logger.Error("恢复定义文件失败，留待查证", "error", err)
		return false
	}
	return true
}

// finish 回填操作终态。ctx 可能已取消（超时 / 客户端断开），因此走 WithoutCancel：
// 终态是审计事实，不得随请求上下文一起消失；回填失败只记 ERROR，由启动恢复查证。
// finish writes the terminal state even when the request context is gone.
func (s *ResourceAdmin) finish(ctx context.Context, operationID string, status core.OperationStatus,
	code, message string, detail ...[]byte,
) {
	var payload []byte
	if len(detail) > 0 {
		payload = detail[0]
	}
	if err := s.operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		status, code, message, payload, s.now()); err != nil {
		s.logger.Error("资源写回填操作终态失败，留待启动恢复查证",
			"operation_id", operationID, "status", string(status), "error", err)
	}
}

// CurrentETag 是条件写"当前值"的对外读取点：与写路径的比较值同源（同一指针、同一
// 发布版本、同一凭据段），因此 web 层在形状校验失败时给出的 `current_etag` 不会被
// 服务层再判一次 412。资源已删除 / 没有发布版本 → `false`（调用方不给该字段）。
// CurrentETag is the single external read point for a resource's current conditional
// ETag; it matches exactly what the write paths compare against.
func (s *ResourceAdmin) CurrentETag(ctx context.Context, kind core.ResourceKind, id string) (string, bool) {
	if _, err := core.ParseResourceKind(string(kind)); err != nil || id == "" {
		return "", false
	}
	pointer, err := s.activePointer(ctx, kind, id)
	if err != nil {
		return "", false
	}
	version, err := s.store.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return "", false
	}
	etag, err := publishedResourceETag(ctx, s.credentials, pointer, version)
	if err != nil {
		return "", false
	}
	return etag, true
}

// activePointer 读未删除的发布指针；缺失或已删除 → `core.ErrResourceNotFound`。
// activePointer loads the live pointer, treating a tombstone as absent.
func (s *ResourceAdmin) activePointer(ctx context.Context, kind core.ResourceKind, id string) (core.ResourcePointer, error) {
	pointer, err := s.store.Pointer(ctx, kind, id)
	if errors.Is(err, core.ErrResourceNotFound) {
		return core.ResourcePointer{}, fmt.Errorf("%w: %s %s", core.ErrResourceNotFound, kind, id)
	}
	if err != nil {
		return core.ResourcePointer{}, fmt.Errorf("读取 %s %s 的发布指针: %w", kind, id, err)
	}
	if pointer.DeletedAt != nil {
		return core.ResourcePointer{}, fmt.Errorf("%w: %s %s", core.ErrResourceNotFound, kind, id)
	}
	return pointer, nil
}

// resourceExists 判定同名资源是否仍在（已删除不算存在——删除后同名重建允许，版本号
// 继续递增，约束 12）。
// resourceExists reports whether a live pointer exists; deleted rows do not count.
func (s *ResourceAdmin) resourceExists(ctx context.Context, kind core.ResourceKind, id string) (bool, error) {
	pointer, err := s.store.Pointer(ctx, kind, id)
	if errors.Is(err, core.ErrResourceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取 %s %s 的发布指针: %w", kind, id, err)
	}
	return pointer.DeletedAt == nil, nil
}

// providerEntryFromVersion 从一份资源版本解析出**恰一个**条目：归属必须一致，定义
// 原文（两种已落库形态都接受）必须恰含一个条目且条目名等于资源标识。任何一条不
// 成立都说明存储与定义分叉，调用方按内部错误处理（fail-closed，不构造错客户端）。
// providerEntryFromVersion resolves exactly one entry out of one version and fails
// closed on any identity or shape mismatch.
func providerEntryFromVersion(resourceID string, version core.ResourceVersion) (workspace.ProviderEntry, error) {
	if version.Kind != core.ResourceModelConnection || version.ResourceID != resourceID {
		return workspace.ProviderEntry{}, fmt.Errorf(
			"模型连接 %s 的版本 %d 归属不符（got %s/%s）", resourceID, version.ID, version.Kind, version.ResourceID)
	}
	entries, err := publishedProviderEntries(version.DefinitionYAML)
	if err != nil {
		return workspace.ProviderEntry{}, fmt.Errorf("模型连接 %s 的版本 %d 定义非法: %w", resourceID, version.ID, err)
	}
	if len(entries) != 1 {
		return workspace.ProviderEntry{}, fmt.Errorf(
			"模型连接 %s 的版本 %d 必须恰含一个条目（实际 %d）", resourceID, version.ID, len(entries))
	}
	if entries[0].Name != resourceID {
		return workspace.ProviderEntry{}, fmt.Errorf(
			"模型连接 %s 的版本 %d 条目名 %q 与资源标识不一致", resourceID, version.ID, entries[0].Name)
	}
	return entries[0], nil
}

// definitionBlockMatches 是分歧闸门的判据：文件里目标块的当前字节是否仍对应发布
// 版本的 `content_hash`。实现按顶层键参数化后放在 `definition_reconcile.go`
// （启动对账与写面共用同一判据），这里只固定 `providers:` 键。
// definitionBlockMatches fixes the providers key for the shared block predicate.
func definitionBlockMatches(doc []byte, name string, published core.ResourceVersion) bool {
	return definitionBlockMatchesFor(doc, providersBlockKey, name, published)
}

// definitionEntry 把已发布版本的 `definition_yaml` 变成可交给块替换的条目字节：
// 块式列表片段原样使用；基线写入的条目映射（顶层是映射）在内存里包成单条目列表——
// 存储字节与哈希**不变**，只影响替换输入。模型连接与 MCP 共用同一形态判定。
// definitionEntry re-wraps the baseline mapping form for block replacement without
// touching the stored bytes or their hash; both resource kinds share the shape test.
func definitionEntry(definition string) []byte {
	for _, line := range strings.Split(definition, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") {
			return []byte(definition)
		}
		break
	}
	return asProviderSequence(definition)
}

// entryBlockRaw 取候选文件里目标条目的块原文（写入版本行的 `definition_yaml`
// 就是这份字节：块怎么写就怎么存，**不**重新序列化）。
// entryBlockRaw returns the entry block exactly as written.
func entryBlockRaw(doc []byte, key, name string) ([]byte, bool) {
	blocks, err := workspace.BlockEntries(doc, key)
	if err != nil {
		return nil, false
	}
	for _, block := range blocks {
		if block.Name == name {
			return block.Raw, true
		}
	}
	return nil, false
}

// blockEntryExists 判定块里是否已有同名条目（创建路径的重名闸门；块本身已通过
// `BlockEntries` 的结构校验）。
// blockEntryExists reports whether the block already lists the name.
func blockEntryExists(blocks []workspace.BlockEntry, name string) bool {
	for _, block := range blocks {
		if block.Name == name {
			return true
		}
	}
	return false
}

// buildModelConnectionClient 在提交段之外构造一次性客户端：只解析本资源条目的
// `${VAR}` 占位，缺失 / 空值 → 点名资源与变量名（**不**回显值），错误文案不含凭证
// 与请求内容。约束 7 / 50 的"零外呼"由此保证——构造期不发任何请求。
// buildModelConnectionClient resolves only this resource's placeholder and never
// issues a request.
func buildModelConnectionClient(resourceID string, entry workspace.ProviderEntry) (core.ProviderClient, error) {
	apiKey, err := resolveProviderAPIKey(resourceID, entry.APIKey)
	if err != nil {
		return nil, err
	}
	return newModelConnectionClient(resourceID, entry, apiKey)
}

// buildModelConnectionTestClient 按已发布版本声明的凭据模式取 Key：env 解析精确
// `${VAR}`，managed 经凭据解析器按发布版本解密——与运行时目录构建共用同一条
// `catalogAPIKey` 判定，连接测试不得绕过托管凭据、也不得把托管条目误判为缺占位。
// buildModelConnectionTestClient resolves the key by the published entry's declared
// mode through the same catalogAPIKey path the runtime catalog uses.
func buildModelConnectionTestClient(ctx context.Context, resourceID string,
	pointer core.ResourcePointer, version core.ResourceVersion,
	entry workspace.ProviderEntry, credentials managedCredentialSource,
) (core.ProviderClient, error) {
	apiKey, err := catalogAPIKey(ctx, pointer, entry, version, credentials)
	if err != nil {
		return nil, err
	}
	return newModelConnectionClient(resourceID, entry, apiKey)
}

// newModelConnectionClient 用已解析的 Key 构造客户端：env 与 managed 只在 Key 的来源
// 上不同，构造段保持同一份。
// newModelConnectionClient builds the client once the key has been resolved.
func newModelConnectionClient(resourceID string, entry workspace.ProviderEntry, apiKey string) (core.ProviderClient, error) {
	client, err := provider.NewOpenAIClient(provider.Options{
		Name:    entry.Name,
		BaseURL: entry.BaseURL,
		APIKey:  apiKey,
	})
	if err != nil {
		return nil, fmt.Errorf("模型连接 %s: %w", resourceID, err)
	}
	return client, nil
}

// modelTestError 把调用错误收成脱敏的错误码与固定短语：`CallError` 携带分类与已脱敏
// 文案；其余错误只给 unknown 与固定短语，原始错误不回显（可能含地址或响应片段）。
// modelTestError reduces a call failure to a redacted code and message.
func modelTestError(err error) (code, message string) {
	var callErr *provider.CallError
	if errors.As(err, &callErr) {
		return string(callErr.Kind), callErr.Message
	}
	return string(core.ErrorKindUnknown), "模型调用失败"
}

// modelTestTimedOut 判定一次测试调用是否以超时收场：客户端的超时分类同时覆盖
// 截止时间与取消（请求方断开），两者都按 504 处理——调用没有完成，不能记成功。
// modelTestTimedOut folds both deadline and cancellation into the timeout result.
func modelTestTimedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var callErr *provider.CallError
	return errors.As(err, &callErr) && callErr.Kind == core.ErrorKindTimeout
}

// validateResourceID 校验资源标识的路径安全面：非空、首尾无空白、不是 `.` / `..`、
// 不含路径分隔符与控制字符。资源标识会进 URL 段、审计行与定义文件条目名，三处都
// 不接受这些字符（**不**在此推断业务命名规则——那是 web 层按契约判的）。
// validateResourceID rejects empty, padded and path-like resource identifiers.
func validateResourceID(name string) error {
	if name == "" {
		return errors.New("资源标识不能为空")
	}
	if name != strings.TrimSpace(name) {
		return errors.New("资源标识首尾不得有空白")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("资源标识 %q 非法", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return errors.New("资源标识不得包含路径分隔符")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("资源标识不得包含控制字符")
		}
	}
	return nil
}

// resourceLockKey 把资源身份拼成互斥表的键：不同种类同名资源互不阻塞。
// resourceLockKey joins the kind and id into one mutex key.
func resourceLockKey(kind core.ResourceKind, id string) string {
	return string(kind) + "\x00" + id
}

// publishAction 给出发布动作字面量（目前只有模型连接；MCP / 技能在各自任务接入）。
// publishAction returns the publish action literal for the kind.
func publishAction(kind core.ResourceKind) string {
	switch kind {
	case core.ResourceMCPServer:
		return core.ActionMCPServerPublish
	case core.ResourceSkill:
		return core.ActionSkillPublish
	default:
		return core.ActionModelConnectionPublish
	}
}

// preconditionError 是 412 的统一载体：`data.current_etag` 给出当前强 ETag，
// **不**回显定义正文（约束 3）。
// preconditionError carries the current strong ETag and never the definition body.
func preconditionError(currentETag string) error {
	return &core.AdminError{
		Kind:    core.ErrPreconditionFailed,
		Message: "预条件失败：If-Match 与当前发布版本不一致",
		Data:    map[string]any{"current_etag": currentETag},
	}
}

// editUnsupportedError 是 400 EDIT_UNSUPPORTED 的统一载体：块结构无法安全改写或
// 条目内注释无法保留时使用；调用方保证此时文件逐字节未动。
// editUnsupportedError is the fixed 400 EDIT_UNSUPPORTED carrier.
func editUnsupportedError() error {
	return &core.AdminError{
		Kind:    core.ErrEditUnsupported,
		Message: "定义文件块无法安全改写（格式不支持或条目内含注释），请先手工修正",
	}
}

// operationFailedError 是 pending 之后失败的统一载体：分类为内部失败，`data` 带回
// 可经 `GET /operations/{id}` 追查的操作标识（约束 1a）。
// operationFailedError carries the queryable operation id of a post-pending failure.
func operationFailedError(operationID, message string) error {
	return &core.AdminError{
		Kind:    errResourceInternal,
		Message: message,
		Data:    map[string]any{"operation_id": operationID},
	}
}

// modelConnectionRequestHash 与 Agent 写面共用同一个摘要形态（`sha256:` 前缀 +
// 小写十六进制）；材料只含占位符与公开字段，不含解析后的凭证值。托管模式下请求里的
// `api_key` 是**明文**：摘要只记「本次带值」这一位，永不把明文写进操作行。
// modelConnectionRequestHash reuses the agent-surface digest shape and never records a
// managed plaintext: the material only says whether a value came with the request.
func modelConnectionRequestHash(action string, req ModelConnectionRequest, mode core.CredentialMode) string {
	key := req.APIKey
	if mode == core.CredentialModeManaged {
		key = ""
		if req.APIKeySet {
			key = "<managed-value>"
		}
	}
	return agentRequestHash(strings.Join([]string{
		action, req.Name, req.BaseURL, key, req.DefaultModel, string(mode),
	}, "\n"))
}

// resourcePublishHash 摘要一次发布请求：资源、目标版本与期望 ETag（模型连接与 MCP 共用）。
// resourcePublishHash digests one publish request.
func resourcePublishHash(req core.ResourcePublishRequest) string {
	target := ""
	if req.VersionID != nil {
		target = fmt.Sprintf("%d", *req.VersionID)
	}
	return agentRequestHash(strings.Join([]string{
		string(req.Kind), req.ID, target, req.ExpectedETag,
	}, "\n"))
}

// modelConnectionTestHash 摘要一次测试请求：测试没有请求体，材料只有资源标识。
// modelConnectionTestHash digests the test request.
func modelConnectionTestHash(name string) string {
	return agentRequestHash(core.ActionModelConnectionTest + "\n" + name)
}

// modelTestOperationDetail 是测试操作 `detail_json` 的固定投影（脱敏：错误只留
// 固定短语与分类码，不含凭证、地址原文或响应片段）。
// modelTestOperationDetail is the redacted detail projection of a connection test.
type modelTestOperationDetail struct {
	OK           bool   `json:"ok"`
	LatencyMS    int64  `json:"latency_ms"`
	Model        string `json:"model,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// mcpTestOperationDetail 是 `mcp_server.test` 操作详情的**固定七字段**（契约 D-07）：
// 一律不用 omitempty——成功时 `error_code` / `error_message` / `failure_class` 必须是
// 空串而不是缺键，`tools` 必须存在（空列表写 `[]`）。页面按"字段是否齐全"区分
// 完整记录与历史记录；缺字段的旧行不得被推断成通过。
// mcpTestOperationDetail is the fixed seven-field detail of an MCP connection test;
// every key is always present so the page can tell a complete row from a legacy one.
type mcpTestOperationDetail struct {
	OK           bool     `json:"ok"`
	LatencyMS    int64    `json:"latency_ms"`
	Tools        []string `json:"tools"`
	ErrorCode    string   `json:"error_code"`
	ErrorMessage string   `json:"error_message"`
	Stage        string   `json:"stage"`
	FailureClass string   `json:"failure_class"`
}

// mcpTestOperationDetailOf 把测试结果投影成 D-07 固定字段；工具清单为空时写空切片
// （`null` 与"字段缺失"在页面上不可区分，两者都不允许）。
// mcpTestOperationDetailOf projects the result into the fixed D-07 shape, with an
// empty (never nil) tool list.
func mcpTestOperationDetailOf(result core.ResourceTestResult) mcpTestOperationDetail {
	tools := result.Tools
	if tools == nil {
		tools = []string{}
	}
	return mcpTestOperationDetail{
		OK:           result.OK,
		LatencyMS:    result.LatencyMS,
		Tools:        tools,
		ErrorCode:    result.ErrorCode,
		ErrorMessage: result.ErrorMessage,
		Stage:        result.Stage,
		FailureClass: result.FailureClass,
	}
}
