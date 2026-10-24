package app

// 本文件是 Agent 写面的编排层（颗粒度文档 §3.1.7 / 约束 27—33）：创建、作者模型草稿、
// 受管文件读取与多文件条件提交，以及复制、归档、恢复与归档列表。每次写都按
// 「预写 pending → 动目标 → 回填终态」编排；纯请求校验（名字、集合 ETag、候选路径、
// 归档标识与恢复目标前置）一律在预写**之前**完成，失败不留操作行（约束 1）。Agent 级
// 写锁与运行准入、内容写共用同一个实例（约束 25）。
//
// 归档按图 F 的九步落地；回滚矩阵的三档（状态 / 目录 / 归档行）各自把补偿失败收敛到
// `unknown`，**不得**谎报成功。注册表 / 调度 / Run 取消器三项依赖可选：CLI 持有实例锁
// 时不存在其它服务进程与在途 Run，归档跳过注销、取消与等待三步并记 Debug；HTTP 路径
// 三项必填，缺一即装配错误。
// This file orchestrates the Agent write surface: create, author draft, managed-file
// read, the conditional multi-file submit, and copy / archive / restore / archive list.
// Every write pre-writes a pending operation before touching its target, and every
// compensation failure ends in `unknown` rather than a false success. The registry,
// scheduler and run canceller are optional together: the CLI path holds the instance
// lock, so it has no other process and no in-flight runs to wait for.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// 三个入参错误的点名标识直接复用契约动作字面量（run-and-errors.md §9.3），
	// 不另造工具名：同一个写面在审计与错误里必须叫同一个名字。
	// The invalid-input surface names reuse the contract action literals verbatim.
	agentCreateTool = core.ActionAgentCreate
	agentFilesTool  = core.ActionAgentFilesUpdate
	agentDraftTool  = core.ActionAgentGenerateFiles

	// agentIndexWriteDeferred 是 `detail_json.index_write` 的固定取值（约束 27）：
	// 状态文件已生效、索引写失败，下次启动基线修复。
	// agentIndexWriteDeferred is the fixed index_write marker for constraint 27.
	agentIndexWriteDeferred = "deferred"

	// 失败终态的 result_code 取自错误码表（schema.sql：`OK` 或错误码表中的 code）。
	// Failure result codes reuse the error-code table; success is always OK.
	agentResultBadRequest         = "BAD_REQUEST"
	agentResultPreconditionFailed = "PRECONDITION_FAILED"
	agentResultConflict           = "CONFLICT"
	agentResultAgentBusy          = "AGENT_BUSY"
	agentResultNotFound           = "NOT_FOUND"

	// agentDefinitionFileName 是 Agent 定义文件名（契约 §1 与 workspace 的同名字面量
	// 一致）；写面用它判定候选集合是否仍有定义文件，**不再发明第二种写法**。
	// agentDefinitionFileName mirrors the workspace literal for the definition file.
	agentDefinitionFileName = "AGENT.md"

	// agentArchiveDirName 是工作区归档区目录名（与 workspace 的私有同名字面量逐字一致）：
	// 恢复入口必须按 `archive/<archive_id>` 形态调用 workspace，本层不推导时间戳格式。
	// agentArchiveDirName is the archive-area directory literal; the stamp format stays
	// private to workspace and is never recomputed here.
	agentArchiveDirName = "archive"

	// agentBusyWait 是归档等待在途 Run 退出缺省上限（约束 32 ③：默认 30 秒）。
	// agentBusyWait is the default bounded wait for in-flight runs (constraint 32).
	agentBusyWait = 30 * time.Second
)

// 复制 / 归档 / 恢复三个动作在入参错误里同样复用契约动作字面量作工具名。
// The three new write actions reuse the contract action literals as tool names.
const (
	agentCopyTool    = core.ActionAgentCopy
	agentArchiveTool = core.ActionAgentArchive
	agentRestoreTool = core.ActionAgentRestore
)

// RunCanceller 是归档路径等待在途 Run 的消费面（§3.1.7）：一次快照、发出取消、在有界
// 窗口内等待退出。实现由装配层提供（内部存储的 running 行 + `AgentService.Cancel`）；
// 归档编排只依赖这三个动作，不依赖任何具体运行实现。
// RunCanceller is the consumer-side surface the archive path needs: snapshot, signal,
// then a bounded wait. The assembly layer supplies the implementation.
type RunCanceller interface {
	// RunningRunIDs 返回该 Agent 当前仍在运行的 Run 标识（一次快照，不等待）。
	// RunningRunIDs snapshots the runs still running for one agent.
	RunningRunIDs(ctx context.Context, agent string) ([]string, error)
	// CancelRun 发出取消信号；返回不代表 Run 已退出。
	// CancelRun signals cancellation; a nil error does not mean the run has exited.
	CancelRun(ctx context.Context, runID string) error
	// WaitRuns 在 ctx 窗口内等待给定 Run 退出，返回窗口结束时仍未退出的标识；
	// 窗口到期不是错误——剩余集合就是等待结果。
	// WaitRuns waits within ctx and returns the ids still alive when the window closes;
	// an expired window is a result, not an error.
	WaitRuns(ctx context.Context, runIDs []string) ([]string, error)
}

// AgentRegistry 是归档路径需要的注册表最小面（US-11 的 `*agentRegistry` 直接满足）：
// 查旧条目、移出、回滚时放回。
// AgentRegistry is the minimal registry surface the archive path needs.
type AgentRegistry interface {
	Lookup(name string) (core.AgentEntry, bool)
	Replace(name string, entry core.AgentEntry)
	Remove(name string)
}

// AgentArchiveStore 是 `agent_archives` 的消费方端口（实现在 internal/storage）：
// 归档行与操作终态同事务，恢复标记同样与终态同事务。
// AgentArchiveStore is the consumer-side port of agent_archives; the row and the
// operation terminal state share one transaction on both insert and restore mark.
type AgentArchiveStore interface {
	InsertArchive(ctx context.Context, rec core.AgentArchiveRecord, opID string) error
	MarkRestored(ctx context.Context, archiveID, newName, opID string, at time.Time) error
	ArchiveByID(ctx context.Context, archiveID string) (core.AgentArchiveRecord, error)
	ListArchives(ctx context.Context) ([]core.AgentArchiveRecord, error)
}

// AgentFilesView 是 Agent 写面的响应视图（契约 `AgentFilesView`）：Agent、生命周期
// 投影、受管文件集合 ETag 与文件清单。它是 `internal/app` 的返回类型，web 只做字段
// 投影，不重复定义领域类型（§3.1.7 冻结节）。
// AgentFilesView is the write-surface response view; the web layer only projects it.
type AgentFilesView struct {
	Agent     string
	Lifecycle core.AgentLifecycleView
	ETag      string
	Files     []core.FileEntry
}

// AgentDraft 是作者草稿的响应视图（契约 `AgentDraftView`）：草稿只在内存里，磁盘上
// 没有对应的基础版本，因此没有 ETag 与大小；保存必须另走带 `If-Match` 的多文件提交。
// 文件与问题互斥（R6.3）：有问题时 Files 为空，问题也只存在于本次响应里。
// AgentDraft is the in-memory draft view; it carries no ETag because no disk base exists.
type AgentDraft struct {
	Agent     string
	Files     []core.AuthorFile
	Questions []core.AuthorQuestion
}

// AgentAdminDeps 是 Agent 写面的全部依赖。回调形态的依赖（已发布资源快照、加载期
// 校验、作者调用）由装配层按**每次调用**的当前快照提供，本类型不缓存过期列表。
// 复制 / 归档 / 恢复所需的注册表、调度与运行依赖由 T033 追加。
// AgentAdminDeps carries every dependency of the Agent write surface. Callback-shaped
// dependencies are answered from the current snapshot on every call.
type AgentAdminDeps struct {
	// Root 是工作区根（`.basalt/`）；生命周期存储与它同根。
	Root string
	// Gate 必须与运行准入、内容写共用同一个实例（生产装配传共享闸门）。
	Gate *core.AgentGate
	// Lifecycle 是状态文件的唯一写者（约束 49）；Bootstrap / Discard 要求调用方已持锁。
	Lifecycle *workspace.LifecycleStore
	// Index 是可重建的生命周期索引写端口；写失败不回滚（约束 37）。
	Index core.LifecycleIndex
	// Operations 是管理操作仓储：先写 pending、成功 / 失败后回填终态。
	Operations core.OperationStore
	// Logger 记录索引同步失败、补偿失败等"变更已生效但外围失败"的 ERROR。
	Logger *slog.Logger
	// Now 是操作标识与时间戳的唯一时钟来源。
	Now func() time.Time

	// PublishedProviders 返回已发布模型连接条目，顺序即模板取「第一项」的固定顺序
	// （约束 28：**不得**遍历 map 挑一个）。
	PublishedProviders func(ctx context.Context) ([]workspace.ProviderEntry, error)
	// PublishedSkills / AppliedMCPServers 供作者草稿的提示词与引用校验使用。
	PublishedSkills   func(ctx context.Context) ([]string, error)
	AppliedMCPServers func(ctx context.Context) ([]string, error)
	// AuthorChat 是作者调用的唯一模型依赖（生产传 `*provider.ProviderService`，审计不可绕过）。
	AuthorChat authorChatter
	// AuthorTemplate 是作者提示词里的模板原文，与 `Create` 使用的模板同源。
	AuthorTemplate string
	// MaxRequestBytes 是 `http.max_request_bytes`；≤0 表示不施加额外上限。
	MaxRequestBytes int64
	// ValidateDefinition 是提交前的加载期校验（生产传 `workspace.ValidateCandidate`
	// 的适配）：候选 `AGENT.md` 必须整份过装载期规则，含 `schedules` 全局 id 不与
	// 其它 Agent 撞车（约束 30）。
	ValidateDefinition func(ctx context.Context, agent string, definition []byte) error
	// CreateStaged 是"模板目录落地"这一步的注入位：生产缺省传
	// `workspace.CreateAgentStaged`。暴露为依赖只为约束 27 要求的回滚判据——测试必须在
	// **`Bootstrap` 之后**注入一次失败，验证状态文件经 `Discard` 收回；否则该分支只能
	// 靠制造真实磁盘故障才可触发。**不是**第二套实现入口：生产装配只传缺省函数。
	// CreateStaged is the injectable "materialize the template directory" step, exposed
	// solely so the mandated rollback test can fail after Bootstrap. Production passes
	// the workspace implementation and nothing else.
	CreateStaged func(root string, spec core.AgentSpec, providers []string, bindings core.ResourceBindings) ([]core.AgentFileInfo, error)

	// Archives 是归档行的存储端口：归档 / 恢复两个动作都必须装配它。
	// Archives stores archive rows; archive and restore both require it.
	Archives AgentArchiveStore
	// Registry / Scheduler / RunCanceller 是归档路径的注销、调度与在途 Run 面；
	// 三者**同时提供或同时省略**（CLI 持有实例锁时省略并记 Debug，约束 32）。
	// Registry, Scheduler and RunCanceller are all-or-nothing: HTTP supplies all three,
	// the CLI path supplies none and logs at Debug.
	Registry     AgentRegistry
	Scheduler    ScheduleRegistry
	RunCanceller RunCanceller
	// BusyWait 是归档等待在途 Run 的上限；≤0 时取缺省的 30 秒（约束 32 ③）。
	// BusyWait overrides the 30-second default bounded wait; tests inject a tiny value.
	BusyWait time.Duration

	// Resources / NotifyAdmin 是复制前判定"源专属绑定"与校验映射目标的只读来源，
	// 与准入检查用**同一批**存储：按绑定 UID 判范围，**不得**按名称猜。
	// Resources and NotifyAdmin answer the copy-time exclusivity question from the same
	// stores admission reads; the decision is made by binding UID, never by name.
	Resources   *storage.ResourceStore
	NotifyAdmin *storage.NotifyAdminStore
	// Projection 是绑定投影的重建面（R4.6）：创建 / 复制 / 恢复 / 归档成功之后按
	// 定义文件重建该 Agent 的投影行。投影可重建，重建失败只记 ERROR——不把已生效的
	// 写改写成失败（与索引写同一纪律）。
	// Projection rebuilds the binding projection after each lifecycle write; a rebuild
	// failure is logged, never turned into a failed write.
	Projection *BindingProjection
}

// AgentAdmin 是 Agent 写面的编排实现；字段全部私有，构造后不再变化。
// AgentAdmin implements the write surface; its fields are fixed at construction.
type AgentAdmin struct {
	root            string
	gate            *core.AgentGate
	lifecycle       *workspace.LifecycleStore
	index           core.LifecycleIndex
	operations      core.OperationStore
	logger          *slog.Logger
	now             func() time.Time
	published       func(ctx context.Context) ([]workspace.ProviderEntry, error)
	publishedSkills func(ctx context.Context) ([]string, error)
	appliedMCP      func(ctx context.Context) ([]string, error)
	authorChat      authorChatter
	authorTemplate  string
	maxRequestBytes int64
	validateDef     func(ctx context.Context, agent string, definition []byte) error
	createStaged    func(root string, spec core.AgentSpec, providers []string, bindings core.ResourceBindings) ([]core.AgentFileInfo, error)
	archives        AgentArchiveStore
	registry        AgentRegistry
	scheduler       ScheduleRegistry
	canceller       RunCanceller
	busyWait        time.Duration
	resources       *storage.ResourceStore
	notify          *storage.NotifyAdminStore
	projection      *BindingProjection
}

// NewAgentAdmin 组装 Agent 写面；Gate 为 nil 时退化为私有锁表（只防本实例自身并发，
// **不**提供与准入的互斥——生产装配必须传共享实例），Logger / Now 为 nil 时回落默认值。
// NewAgentAdmin assembles the write surface. A nil gate falls back to a private lock table
// that never serializes against admission; production assembly must pass the shared gate.
func NewAgentAdmin(deps AgentAdminDeps) *AgentAdmin {
	gate := deps.Gate
	if gate == nil {
		gate = core.NewAgentGate()
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	createStaged := deps.CreateStaged
	if createStaged == nil {
		createStaged = workspace.CreateAgentStagedWithBindings
	}
	busyWait := deps.BusyWait
	if busyWait <= 0 {
		busyWait = agentBusyWait
	}
	return &AgentAdmin{
		root:            deps.Root,
		gate:            gate,
		lifecycle:       deps.Lifecycle,
		index:           deps.Index,
		operations:      deps.Operations,
		logger:          logger,
		now:             now,
		published:       deps.PublishedProviders,
		publishedSkills: deps.PublishedSkills,
		appliedMCP:      deps.AppliedMCPServers,
		authorChat:      deps.AuthorChat,
		authorTemplate:  deps.AuthorTemplate,
		maxRequestBytes: deps.MaxRequestBytes,
		validateDef:     deps.ValidateDefinition,
		createStaged:    createStaged,
		archives:        deps.Archives,
		registry:        deps.Registry,
		scheduler:       deps.Scheduler,
		canceller:       deps.RunCanceller,
		busyWait:        busyWait,
		resources:       deps.Resources,
		notify:          deps.NotifyAdmin,
		projection:      deps.Projection,
	}
}

// Create 按约束 27 的顺序创建一个默认停用的 Agent：名字与目标空闲校验 → 读已发布
// Provider → 预写 `agent.create` → 写锁内复核 → `Lifecycle.Bootstrap`（disabled /
// generation=1，operation_id 为本次操作）→ `CreateAgentStaged`（staging + rename）→
// 可重建索引（失败不回滚，记 `index_write=deferred`）→ 回填 success → 重新读取受管
// 文件作为响应真值。
//
// 创建**不做**加载期校验：模板在没有已发布 Provider 时写的是可见占位符（约束 28），
// 占位符必然违反装载期规则 2；因此"模板可用"由运维在提交文件后由提交路径校验。
// Create orchestrates constraint 27. It deliberately skips load-time validation: the
// template may legitimately carry the visible provider placeholder.
func (s *AgentAdmin) Create(ctx context.Context, actor core.Actor, spec core.AgentSpec) (AgentFilesView, error) {
	if err := workspace.ValidateAgentName(spec.Name); err != nil {
		return AgentFilesView{}, &core.InvalidInputError{Tool: agentCreateTool, Reason: err.Error()}
	}
	if err := s.ensureCreateTargetFree(ctx, spec.Name); err != nil {
		return AgentFilesView{}, err
	}

	entries, err := s.publishedEntries(ctx)
	if err != nil {
		return AgentFilesView{}, fmt.Errorf("读取已发布 Provider 列表: %w", err)
	}
	providers := authorProviderNames(entries)
	// 模板创建是产生绑定的三类入口之一（R4.6）：服务端为选定的首项已发布 Provider
	// 写一条 `resource_bindings.model_connection`，UID 取自发布指针——**不**按名解析。
	// 无已发布 Provider 时模板写占位符，此时不写绑定（也没有可写的身份）。
	// Template creation binds the provider it selected; the uid comes from the pointer.
	var bindings core.ResourceBindings
	if len(entries) > 0 && entries[0].ResourceUID != "" {
		bindings.ModelConnections = []core.ResourceBinding{{
			Name: entries[0].Name,
			UID:  entries[0].ResourceUID,
		}}
	}

	detail, err := json.Marshal(agentCreateOperationDetail{})
	if err != nil {
		return AgentFilesView{}, fmt.Errorf("组装创建操作详情: %w", err)
	}
	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionAgentCreate,
		TargetKind:   targetKindAgent,
		TargetID:     spec.Name,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  agentCreateRequestHash(spec),
		Status:       core.OperationPending,
		DetailJSON:   detail,
		CreatedAt:    now,
	}
	if beginErr := s.operations.BeginOperation(ctx, record); beginErr != nil {
		// 预写失败 = 拒绝执行（约束 1）：目录与状态文件一个字节都没动。
		s.logger.Error("创建 Agent 预写操作记录失败，拒绝执行",
			"agent", spec.Name, "error", beginErr)
		return AgentFilesView{}, fmt.Errorf("创建 Agent 预写操作记录失败，拒绝执行: %w", beginErr)
	}

	// 写锁与运行准入、内容写共用同一把（约束 25）：创建期间的 staging / rename 不能被
	// 同名的准入或其它写看见半成品。
	unlock := s.gate.Lock(spec.Name)
	defer unlock()

	if err := s.ensureCreateTargetFree(ctx, spec.Name); err != nil {
		// 预写与取锁之间的窗口里别人建好了同名 Agent：变更尚未开始，但 pending 行已经
		// 落下，必须回填 failed（约束 1 的后半句），且目标零写入。
		s.finish(ctx, operationID, core.OperationFailed, agentResultBadRequest,
			"目标已存在，未执行创建", detail)
		return AgentFilesView{}, postPendingError(err, operationID)
	}

	rec, bootErr := s.lifecycle.Bootstrap(ctx, spec.Name, operationID)
	if bootErr != nil {
		// 状态文件一个字节都没写成：只回填终态，不碰目录。
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"生命周期状态写入失败", detail)
		return AgentFilesView{}, postPendingError(bootErr, operationID)
	}

	infos, createErr := s.createStaged(s.root, spec, providers, bindings)
	if createErr != nil {
		if discardErr := s.lifecycle.Discard(ctx, spec.Name, operationID); discardErr != nil {
			// 补偿失败：状态文件可能残留，操作标 unknown 留给启动恢复查证（约束 27）。
			s.logger.Error("创建失败后撤销生命周期状态文件失败，操作标记 unknown",
				"agent", spec.Name, "operation_id", operationID, "error", discardErr)
			s.finish(ctx, operationID, core.OperationUnknown, lifecycleResultInternal,
				"创建失败且状态文件撤销失败，需查证", detail)
			return AgentFilesView{}, postPendingError(errors.Join(createErr, discardErr), operationID)
		}
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"创建 Agent 目录失败", detail)
		return AgentFilesView{}, postPendingError(createErr, operationID)
	}

	if s.index != nil {
		finalDetail := detail
		if idxErr := s.index.PutLifecycleIndex(ctx, rec, true, ""); idxErr != nil {
			// 索引可从状态文件重建：状态文件与目录已生效，不回滚、不把成功说成失败；
			// 记 ERROR 并在 detail_json 标 index_write=deferred（约束 27 / 约束 37）。
			s.logger.Error("创建 Agent：生命周期索引同步失败（状态文件为准，下次启动基线修复）",
				"agent", spec.Name, "operation_id", operationID, "error", idxErr)
			if deferred, marshalErr := json.Marshal(agentCreateOperationDetail{
				IndexWrite: agentIndexWriteDeferred,
			}); marshalErr == nil {
				finalDetail = deferred
			}
		}
		s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", finalDetail)
	} else {
		s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", detail)
	}

	// 创建成功即按落盘的定义建立投影：删除闸门与准入从这一刻起就能按投影反查引用。
	s.rebuildActiveProjection(ctx, spec.Name, rec.AgentUID)

	if view, ok := s.readFilesView(ctx, spec.Name); ok {
		return view, nil
	}
	// 创建已经生效：重新读取失败不得改写成创建失败（与 withLifecycle 同一纪律）。
	// 模板创建只写 Agent 根目录下的 AGENT.md，`CreateAgentStaged` 返回的工作区相对
	// 路径因此可按 basename 降级投影；ETag 只能留空，由客户端下一次 GET 补齐。
	return AgentFilesView{
		Agent:     spec.Name,
		Lifecycle: s.lifecycleView(ctx, spec.Name),
		Files:     baseNameEntries(infos),
	}, nil
}

// Files 返回受管文件集合与集合 ETag（约束 30）：只读，不写管理操作行。
// Files returns the managed set and its ETag; it is read-only.
func (s *AgentAdmin) Files(ctx context.Context, name string) (AgentFilesView, error) {
	if err := workspace.ValidateAgentName(name); err != nil {
		return AgentFilesView{}, &core.InvalidInputError{Tool: agentFilesTool, Reason: err.Error()}
	}

	files, etag, err := workspace.ManagedAgentFiles(s.root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return AgentFilesView{}, &core.AgentNotFoundError{Name: name}
		}
		return AgentFilesView{}, err
	}
	return AgentFilesView{
		Agent:     name,
		Lifecycle: s.lifecycleView(ctx, name),
		ETag:      etag,
		Files:     fileEntries(files),
	}, nil
}

// SubmitFiles 执行一次有条件多文件提交（约束 30）：集合 ETag 的比较、候选树校验与
// 预写全部在 Agent 写锁内完成。顺序固定为「锁内重读并比较集合 ETag → 纯请求校验
// （路径 / UTF-8 / 大小写重复）→ 候选树必须仍有 AGENT.md → 整份配置过加载期校验 →
// 预写 agent.files.update → 完整副本 + 两次 rename → 回填终态」。
//
// 前四步失败**不写操作行、不落盘**（412 / 400）；预写成功之后的失败一律回填 failed
// 并返回原分类，`SubmitAgentFiles` 自身保证失败零残余。
// SubmitFiles compares the set ETag and validates the candidate tree under the agent
// write lock before pre-writing; a stale ETag or an invalid candidate writes nothing.
func (s *AgentAdmin) SubmitFiles(ctx context.Context, actor core.Actor, name, ifMatch string,
	files []core.AuthorFile,
) (AgentFilesView, error) {
	if err := workspace.ValidateAgentName(name); err != nil {
		return AgentFilesView{}, &core.InvalidInputError{Tool: agentFilesTool, Reason: err.Error()}
	}

	// 写锁与运行准入共用（约束 25 / 约束 30）：ETag 比较与候选构建必须在同一临界区，
	// 否则两个并发提交会各自读到同一基础版本。
	unlock := s.gate.Lock(name)
	defer unlock()

	current, currentETag, err := workspace.ManagedAgentFiles(s.root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return AgentFilesView{}, &core.AgentNotFoundError{Name: name}
		}
		return AgentFilesView{}, err
	}
	// 缺失、弱 ETag、`*`、形状非法与不匹配都落在这里：字面比较不可能等于强 ETag。
	if ifMatch != currentETag {
		return AgentFilesView{}, fmt.Errorf("%w: 当前集合 ETag 为 %s", core.ErrPreconditionFailed, currentETag)
	}

	normalized, err := workspace.NormalizeAgentFiles(files)
	if err != nil {
		return AgentFilesView{}, &core.InvalidInputError{Tool: agentFilesTool, Reason: err.Error()}
	}
	candidate := mergeAgentFiles(current, normalized)
	definition, ok := candidate[agentDefinitionFileName]
	if !ok {
		return AgentFilesView{}, &core.InvalidInputError{
			Tool:   agentFilesTool,
			Reason: "候选文件集合缺少 " + agentDefinitionFileName + "（提交后每个 Agent 必须仍有定义文件）",
		}
	}
	if s.validateDef == nil {
		return AgentFilesView{}, errors.New("多文件提交缺少加载期校验依赖（装配错误）")
	}
	if err := s.validateDef(ctx, name, []byte(definition)); err != nil {
		// 装载期拒绝保留类型（管理台按分类出中文）；其余装配 / IO 错误仍按入参
		// 错误上抛，文案由产生方负责。
		var rejected *core.AgentDefinitionRejectedError
		if errors.As(err, &rejected) {
			return AgentFilesView{}, rejected
		}
		return AgentFilesView{}, &core.InvalidInputError{Tool: agentFilesTool, Reason: err.Error()}
	}

	candidateETag := workspace.FileSetETag(mapToFiles(candidate))
	detail, err := json.Marshal(agentFilesOperationDetail{
		BaseETag:      currentETag,
		CandidateETag: candidateETag,
	})
	if err != nil {
		return AgentFilesView{}, fmt.Errorf("组装提交操作详情: %w", err)
	}
	operationID, beginErr := s.beginAgentFilesOperation(ctx, actor, name, currentETag, ifMatch, candidateETag, detail)
	if beginErr != nil {
		return AgentFilesView{}, beginErr
	}

	infos, newETag, submitErr := workspace.SubmitAgentFiles(s.root, name, ifMatch, normalized)
	if submitErr != nil {
		code := lifecycleResultInternal
		switch {
		case errors.Is(submitErr, core.ErrPreconditionFailed):
			code = agentResultPreconditionFailed
		case core.IsInvalidInput(submitErr):
			code = agentResultBadRequest
		}
		s.finish(ctx, operationID, core.OperationFailed, code, "多文件提交失败", detail)
		return AgentFilesView{}, postPendingError(submitErr, operationID)
	}
	s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", detail)

	// 提交成功即按新定义重建投影（`resource_bindings` 可能被整块替换）：身份取生命周期
	// 状态文件；取不到就跳过并记 ERROR——投影可由下次启动重建，提交已经生效。
	if rec, exists, readErr := s.lifecycle.Read(context.WithoutCancel(ctx), name); readErr != nil || !exists {
		s.logger.Error("重建绑定投影跳过：读取生命周期状态失败（提交已生效，投影待下次重建）",
			"agent", name, "exists", exists, "error", readErr)
	} else {
		s.rebuildActiveProjection(ctx, name, rec.AgentUID)
	}

	return AgentFilesView{
		Agent:     name,
		Lifecycle: s.lifecycleView(ctx, name),
		ETag:      newETag,
		Files:     infoEntries(infos),
	}, nil
}

// beginAgentFilesOperation 预写一条 `agent.files.update` pending 行并返回操作标识。
// 预写失败返回包装错误，调用方必须放弃本次提交（目标一个字节都没动）。操作标识只经
// 返回值在同一次调用内传递——**不**落到实例字段或包级变量（并发请求会互相覆盖）。
// beginAgentFilesOperation pre-writes the pending row and returns the operation id by
// value; the id never lives on the instance, so concurrent requests cannot clobber it.
func (s *AgentAdmin) beginAgentFilesOperation(ctx context.Context, actor core.Actor, name, currentETag, ifMatch, candidateETag string, detail []byte) (string, error) {
	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionAgentFilesUpdate,
		TargetKind:   targetKindAgent,
		TargetID:     name,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  agentFilesRequestHash(name, ifMatch, candidateETag),
		ExpectedETag: currentETag,
		Status:       core.OperationPending,
		DetailJSON:   detail,
		CreatedAt:    now,
	}
	if beginErr := s.operations.BeginOperation(ctx, record); beginErr != nil {
		s.logger.Error("多文件提交预写操作记录失败，拒绝执行",
			"agent", name, "error", beginErr)
		return "", fmt.Errorf("多文件提交预写操作记录失败，拒绝执行: %w", beginErr)
	}
	return operationID, nil
}

// GenerateFiles 生成一份只在内存里的文件草稿或澄清问题（约束 29 / R6.3）：**不写管理
// 操作行**、不建 Run、不调用工具、不落盘、不注册；唯一持久副作用是模型调用审计（两阶段
// `llm_calls`，由注入的 `ProviderService` 完成）。失败分类：配置不可用 → 503；上游调用
// 失败 → 原样上抛（500）；答案非法 / 输出不可解析 / 缺 AGENT.md / 引用未发布资源 /
// 超限 → 400 点名。
// GenerateFiles produces an in-memory draft (or questions): no operation row, no run,
// no tools, no disk. Its only persistent side effect is the two-phase model-call audit.
func (s *AgentAdmin) GenerateFiles(ctx context.Context, author core.AuthorConfig, name, description string,
	answers []core.AuthorAnswer,
) (AgentDraft, error) {
	if err := workspace.ValidateAgentName(name); err != nil {
		return AgentDraft{}, &core.InvalidInputError{Tool: agentDraftTool, Reason: err.Error()}
	}

	// 作者功能整体关闭时不读任何已发布快照：Draft 会把空配置判为 503，快照读取失败
	// 不得把一个"功能未开启"掩盖成 500。
	var providers []workspace.ProviderEntry
	var skills, mcp []string
	if author.Provider != "" && author.Model != "" {
		var err error
		if providers, err = s.publishedEntries(ctx); err != nil {
			return AgentDraft{}, fmt.Errorf("读取已发布 Provider 列表: %w", err)
		}
		if skills, err = s.callSkillNames(ctx); err != nil {
			return AgentDraft{}, fmt.Errorf("读取已发布技能列表: %w", err)
		}
		if mcp, err = s.callMCPServerNames(ctx); err != nil {
			return AgentDraft{}, fmt.Errorf("读取已应用 MCP 列表: %w", err)
		}
	}

	service := newAuthorService(authorServiceDeps{
		Config:    author,
		Chat:      s.authorChat,
		Providers: providers,
		Skills:    skills,
		MCP:       mcp,
		Template:  s.authorTemplate,
		MaxBytes:  s.maxRequestBytes,
		Now:       s.now,
		Logger:    s.logger,
	})
	result, err := service.DraftWithAnswers(ctx, name, description, answers)
	if err != nil {
		return AgentDraft{}, mapDraftError(err)
	}
	return AgentDraft{Agent: name, Files: result.Files, Questions: result.Questions}, nil
}

// ScheduleDraft 生成一次定时候选（R6.4）：**不写管理操作行**、不建 Run、不落盘、不
// 注册、不改调度器；唯一持久副作用仍是 `llm_calls` 的两阶段审计。提示词只含 Agent 名
// 与一句话指令，不引用任何资源——但作者 Provider 仍必须命中已发布列表，因此与草稿
// 一样在配置成对时才读一次 Provider 快照（技能 / MCP 名单不用，不读）。
// ScheduleDraft produces one candidate; its only persistent side effect is the audit row.
// The provider snapshot is still read because the author provider must be published.
func (s *AgentAdmin) ScheduleDraft(ctx context.Context, author core.AuthorConfig, name, instruction string) (ScheduleDraftResult, error) {
	if err := workspace.ValidateAgentName(name); err != nil {
		return ScheduleDraftResult{}, &core.InvalidInputError{Tool: agentDraftTool, Reason: err.Error()}
	}

	var providers []workspace.ProviderEntry
	if author.Provider != "" && author.Model != "" {
		var err error
		if providers, err = s.publishedEntries(ctx); err != nil {
			return ScheduleDraftResult{}, fmt.Errorf("读取已发布 Provider 列表: %w", err)
		}
	}

	service := newAuthorService(authorServiceDeps{
		Config:    author,
		Chat:      s.authorChat,
		Providers: providers,
		Now:       s.now,
		Logger:    s.logger,
	})
	draft, err := service.DraftSchedule(ctx, name, instruction)
	if err != nil {
		return ScheduleDraftResult{}, mapDraftError(err)
	}
	return draft, nil
}

// mapDraftError 把作者服务的失败映射到写面分类（失败分类表）：配置不可用与上游调用
// 失败、请求取消都不是入参问题，原样上抛；其余一律 400 点名。
// mapDraftError maps author-service failures onto the write-surface classification.
func mapDraftError(err error) error {
	if errors.Is(err, core.ErrProviderNotConfigured) || errors.Is(err, core.ErrAuthorModelUnavailable) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var callErr *provider.CallError
	if errors.As(err, &callErr) {
		return err
	}
	return &core.InvalidInputError{Tool: agentDraftTool, Reason: err.Error()}
}

// ensureCreateTargetFree 判定创建目标是否空闲（约束 27）：目标目录与状态文件都必须
// 不存在。目录存在 → 400（契约该端点只列 400，D-05）；状态文件存在或不可读同样拒绝——
// 那意味着有孤立状态记录，创建会把它覆盖成半套事实。
// ensureCreateTargetFree requires both the target directory and the state file to be
// absent; an existing either one refuses the create with 400.
func (s *AgentAdmin) ensureCreateTargetFree(ctx context.Context, name string) error {
	if _, _, err := workspace.ManagedAgentFiles(s.root, name); err == nil {
		return &core.InvalidInputError{
			Tool:   agentCreateTool,
			Reason: "同名 Agent 目录已存在，拒绝覆盖：" + name,
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	_, exists, err := s.lifecycle.Read(ctx, name)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return &core.InvalidInputError{
			Tool:   agentCreateTool,
			Reason: "Agent 状态文件不可读或损坏，拒绝创建：" + name,
		}
	case exists:
		return &core.InvalidInputError{
			Tool:   agentCreateTool,
			Reason: "Agent 状态文件已存在（疑似残留），拒绝创建：" + name,
		}
	}
	return nil
}

// readFilesView 重新读取受管文件并组装响应视图（契约：返回落盘后重新读取的真值）。
// 读取失败只记 ERROR 并返回 false——调用方**不得**把已生效的写说成失败。
// readFilesView re-reads the managed set for the response; a read failure is logged
// and reported as "unavailable" so callers never turn a landed write into an error.
func (s *AgentAdmin) readFilesView(ctx context.Context, agent string) (AgentFilesView, bool) {
	files, etag, err := workspace.ManagedAgentFiles(s.root, agent)
	if err != nil {
		s.logger.Error("重新读取受管文件失败（写入已生效）", "agent", agent, "error", err)
		return AgentFilesView{}, false
	}
	return AgentFilesView{
		Agent:     agent,
		Lifecycle: s.lifecycleView(ctx, agent),
		ETag:      etag,
		Files:     fileEntries(files),
	}, true
}

// lifecycleView 投影状态文件真值：与启停前置校验同一分类口径——缺失 / 损坏记
// valid=false 并给不自带路径的固定原因（约束 45），**不得**把缺失读成 active；读取
// 失败也**不得**把已生效的写入改写成错误。
//
// 与 agent_console.go 的 withLifecycle 是同一投影的两处落地（那里作用于 core.AgentFiles，
// 这里作用于 AgentFilesView）；两处必须同步修改。
// lifecycleView projects the state file with the same fail-closed classification as the
// transition precondition. It mirrors agent_console.go's withLifecycle; keep both in sync.
func (s *AgentAdmin) lifecycleView(ctx context.Context, agent string) core.AgentLifecycleView {
	// 写入可能已经生效：客户端断开也要投影真实状态，因此不随请求取消而放弃。
	rec, exists, err := s.lifecycle.Read(context.WithoutCancel(ctx), agent)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return core.AgentLifecycleView{LifecycleError: lifecycleMissingNotice}
	case err != nil:
		return core.AgentLifecycleView{LifecycleError: lifecycleCorruptNotice}
	case !exists:
		return core.AgentLifecycleView{LifecycleError: lifecycleMissingNotice}
	default:
		state := string(rec.State)
		return core.AgentLifecycleView{State: &state, Valid: true}
	}
}

// finish 回填终态；终态回填不随请求取消而放弃（客户端断开不能让审计停在 pending）。
// 回填失败只记 ERROR：目标是否已变由启动恢复按 operation_id 查证。
// finish writes the terminal state detached from request cancellation; a failure is
// logged only and left to startup recovery.
func (s *AgentAdmin) finish(ctx context.Context, operationID string, status core.OperationStatus,
	code, message string, detail []byte,
) {
	if err := s.operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		status, code, message, detail, s.now()); err != nil {
		s.logger.Error("Agent 管理写回填操作终态失败，留待启动恢复查证",
			"operation_id", operationID, "status", string(status), "error", err)
	}
}

// publishedEntries 取已发布 Provider 条目；装配层未提供回调时按空列表处理（创建退回
// 占位符，作者配置判定为未发布）。
// publishedEntries returns the published provider entries; a nil callback means none.
func (s *AgentAdmin) publishedEntries(ctx context.Context) ([]workspace.ProviderEntry, error) {
	if s.published == nil {
		return nil, nil
	}
	return s.published(ctx)
}

// callSkillNames / callMCPServerNames 读取作者草稿的引用白名单；未装配时返回空。
// callSkillNames and callMCPServerNames read the author draft reference whitelists.
func (s *AgentAdmin) callSkillNames(ctx context.Context) ([]string, error) {
	if s.publishedSkills == nil {
		return nil, nil
	}
	return s.publishedSkills(ctx)
}

func (s *AgentAdmin) callMCPServerNames(ctx context.Context) ([]string, error) {
	if s.appliedMCP == nil {
		return nil, nil
	}
	return s.appliedMCP(ctx)
}

// mergeAgentFiles 把提交文件覆盖到当前受管集合上，得到候选受管集合（未列出的现有
// 文件原样保留，约束 30）。只收录 Agent 根目录下的文件——子目录文件不参与受管集合与
// 集合 ETag（managedFilesIn 的同一口径），因此带 `/` 的路径被跳过。
// mergeAgentFiles overlays the submission on the current managed set; sub-directory
// paths are excluded because they are not part of the managed set or its ETag.
func mergeAgentFiles(current, drafts []core.AuthorFile) map[string]string {
	merged := make(map[string]string, len(current)+len(drafts))
	for _, file := range current {
		merged[file.Path] = file.Content
	}
	for _, file := range drafts {
		if strings.Contains(file.Path, "/") {
			continue
		}
		merged[file.Path] = file.Content
	}
	return merged
}

// mapToFiles 把路径→正文映射还原为文件切片；`FileSetETag` 内部按 path 升序排序，
// 因此 map 的遍历顺序不影响结果。
// mapToFiles rebuilds a file slice; FileSetETag sorts internally, so map order is inert.
func mapToFiles(merged map[string]string) []core.AuthorFile {
	files := make([]core.AuthorFile, 0, len(merged))
	for p, content := range merged {
		files = append(files, core.AuthorFile{Path: p, Content: content})
	}
	return files
}

// fileEntries 把受管文件映射为视图行（只带路径与字节数，不携带正文）。
// fileEntries maps managed files onto view rows without content.
func fileEntries(files []core.AuthorFile) []core.FileEntry {
	out := make([]core.FileEntry, len(files))
	for i, file := range files {
		out[i] = core.FileEntry{Path: file.Path, Size: int64(len(file.Content))}
	}
	return out
}

// infoEntries 把 workspace 的相对文件信息映射为视图行（提交路径返回的已是 Agent
// 相对路径，直接同构转换）。
// infoEntries maps workspace file info onto view rows.
func infoEntries(infos []core.AgentFileInfo) []core.FileEntry {
	out := make([]core.FileEntry, len(infos))
	for i, info := range infos {
		// 两个类型的字段与底层类型逐一相同，直接转换（staticcheck S1016 要求）。
		// Both structs have identical field types, so a direct conversion suffices.
		out[i] = core.FileEntry(info)
	}
	return out
}

// baseNameEntries 是创建成功后重新读取失败时的降级投影：`CreateAgentStaged` 返回的是
// 工作区相对路径（`agents/<name>/AGENT.md`），模板创建只写 Agent 根目录下的文件，
// 因此按 basename 还原仍与受管集合同构。
// baseNameEntries degrades workspace-relative paths to base names for the response.
func baseNameEntries(infos []core.AgentFileInfo) []core.FileEntry {
	out := make([]core.FileEntry, len(infos))
	for i, info := range infos {
		out[i] = core.FileEntry{Path: path.Base(info.Path), Size: info.Size}
	}
	return out
}

// agentCreateRequestHash 是创建请求的脱敏摘要：动作、名字、描述与模型按固定顺序拼接
// 取 sha256；不含正文之外任何内容（描述本身也只以摘要出现）。
// agentCreateRequestHash digests the sanitized create request shape.
func agentCreateRequestHash(spec core.AgentSpec) string {
	return agentRequestHash(strings.Join([]string{
		core.ActionAgentCreate, spec.Name, spec.Description, spec.Model,
	}, "|"))
}

// agentFilesRequestHash 是提交请求的脱敏摘要：动作、Agent、请求头 ETag 与候选集合
// ETag（候选集合已由路径与内容哈希决定，不含正文原文）。
// agentFilesRequestHash digests the sanitized submit request shape.
func agentFilesRequestHash(name, ifMatch, candidateETag string) string {
	return agentRequestHash(strings.Join([]string{
		core.ActionAgentFilesUpdate, name, ifMatch, candidateETag,
	}, "|"))
}

// agentRequestHash 取 sha256 并渲染为与其他审计摘要同形的 `sha256:<hex>`。
// agentRequestHash renders a sha256 digest in the shared `sha256:<hex>` shape.
func agentRequestHash(material string) string {
	sum := sha256.Sum256([]byte(material))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// agentCreateOperationDetail 是创建操作 `detail_json` 的固定投影：索引写失败时记
// `index_write=deferred`（约束 27），成功且索引已同步时该键省略。
// agentCreateOperationDetail is the fixed create detail projection.
type agentCreateOperationDetail struct {
	IndexWrite string `json:"index_write,omitempty"`
}

// agentFilesOperationDetail 是提交操作 `detail_json` 的固定投影：基础集合 ETag 与候选
// 集合 ETag，供审计对账"这次提交基于哪一版、产出哪一版"。
// agentFilesOperationDetail is the fixed submit detail projection.
type agentFilesOperationDetail struct {
	BaseETag      string `json:"base_etag"`
	CandidateETag string `json:"candidate_etag"`
}

// —— 复制 / 归档 / 恢复（约束 31—33）——
// Copy, archive and restore orchestration (constraints 31–33).

// agentCopyOperationDetail 是复制操作 `detail_json` 的固定投影：源 / 目标、本次采用的
// 资源映射（`<类别>:<源 UID>→<目标 UID>`，按请求顺序）与索引写结果；索引从状态文件可
// 重建，写失败只记 `index_write=deferred`，**不**把已生效的复制说成失败。
// agentCopyOperationDetail is the fixed copy detail projection.
type agentCopyOperationDetail struct {
	Source     string   `json:"source"`
	Target     string   `json:"target"`
	Mapped     []string `json:"mapped,omitempty"`
	IndexWrite string   `json:"index_write,omitempty"`
}

// describeCopyRewrites 把映射计划渲染成审计用的固定短串（不含凭证、不含定义正文）。
// describeCopyRewrites renders the mapping plan for the operation detail.
func describeCopyRewrites(rewrites []workspace.AgentCopyRewrite) []string {
	if len(rewrites) == 0 {
		return nil
	}
	out := make([]string, len(rewrites))
	for i, rewrite := range rewrites {
		out[i] = fmt.Sprintf("%s:%s→%s", rewrite.Kind, rewrite.FromUID, rewrite.ToUID)
	}
	return out
}

// agentArchiveOperationDetail 是归档操作 `detail_json` 的固定投影：归档前的生命周期状态
// （回滚矩阵的判据与审计对账都从它出发）。
// agentArchiveOperationDetail records the lifecycle state the archive started from.
type agentArchiveOperationDetail struct {
	BeforeState string `json:"before_state"`
}

// agentRestoreOperationDetail 是恢复操作 `detail_json` 的固定投影：归档标识、目标名与
// 两条路径中的哪一条（原名保留 generation、异名 Bootstrap）。
// agentRestoreOperationDetail records the archive id, target and which restore path ran.
type agentRestoreOperationDetail struct {
	ArchiveID string `json:"archive_id"`
	Target    string `json:"target"`
	Renamed   bool   `json:"renamed"`
}

// archiveRegistration 是归档步骤 ①②③ 的回滚快照：注册表旧条目、旧定时任务与"是否曾
// 注册"。零值表示 CLI 路径（三项依赖未装配）或该 Agent 本就不在注册表内。
// archiveRegistration snapshots the registry entry and schedule set for rollback.
type archiveRegistration struct {
	entry      core.AgentEntry
	registered bool
	schedules  []core.Schedule
}

// Copy 把一个活动区 Agent 复制为目标名（约束 31 / FR-070）：源必须处于 `active` /
// `disabled`，目标目录与状态文件都必须不存在；复制在 staging 构建、改写身份字段、清空
// `schedules`，并按 `mapping` 改写源专属资源引用，单次 rename 落位；目标一律以
// `disabled` / `generation=1` / 本次 operation_id 引导。
//
// **专属资源闸门在预写之前**：源定义里指向源 Agent 专属模型连接 / 通知渠道的绑定，
// 必须在 `mapping` 里逐条给出目标（共享、已发布、存在且不沿用源 UID），否则整次复制
// 以 `ErrAgentCopyBlocked` 拒绝——零副本、零操作行、列明待处理资源名与类型。映射本身
// 非法的请求是 `*core.InvalidInputError`（400）；两者都不落 pending 行（约束 1 的
// 「纯请求校验在写操作行之前」）。共享绑定随目录携带（UID 不变）。
//
// 失败 `Discard` 收回状态文件并删除 staging，源 Agent 逐字节不变（不复制会话 / 记忆 /
// 审计，也不带走任何凭据）。
// Copy copies an active-area agent into a new name. Exclusive bindings must be mapped
// explicitly before the pending row is written; the target is bootstrapped as disabled
// and a failure leaves both the source and any target residues untouched.
func (s *AgentAdmin) Copy(ctx context.Context, actor core.Actor, source, target string,
	mapping []core.ResourceMapping,
) (AgentFilesView, error) {
	if err := workspace.ValidateAgentName(source); err != nil {
		return AgentFilesView{}, &core.InvalidInputError{Tool: agentCopyTool, Reason: err.Error()}
	}
	if err := workspace.ValidateAgentName(target); err != nil {
		return AgentFilesView{}, &core.InvalidInputError{Tool: agentCopyTool, Reason: err.Error()}
	}
	if source == target {
		return AgentFilesView{}, &core.InvalidInputError{
			Tool:   agentCopyTool,
			Reason: "复制源与目标同名，拒绝原地复制：" + source,
		}
	}

	sourceRecord, err := s.checkCopySource(ctx, source)
	if err != nil {
		return AgentFilesView{}, err
	}
	if err := s.checkTargetFree(ctx, target, "复制"); err != nil {
		return AgentFilesView{}, err
	}
	rewrites, err := s.resolveCopyMappings(ctx, source, sourceRecord.AgentUID, mapping)
	if err != nil {
		return AgentFilesView{}, err
	}

	detail, err := json.Marshal(agentCopyOperationDetail{
		Source: source,
		Target: target,
		Mapped: describeCopyRewrites(rewrites),
	})
	if err != nil {
		return AgentFilesView{}, fmt.Errorf("组装复制操作详情: %w", err)
	}
	operationID, beginErr := s.beginAgentOperation(ctx, actor, core.ActionAgentCopy, target,
		agentRequestHash(strings.Join([]string{core.ActionAgentCopy, source, target}, "|")), detail)
	if beginErr != nil {
		return AgentFilesView{}, beginErr
	}

	unlock := s.gate.Lock(target)
	defer unlock()

	// 预写与取锁之间的窗口里目标可能已被别人占用：变更尚未开始，但 pending 行已落下，
	// 必须回填 failed（约束 1 的后半句），目标零写入。
	if err := s.checkTargetFree(ctx, target, "复制"); err != nil {
		s.finish(ctx, operationID, core.OperationFailed, agentResultCode(err),
			"复制目标在锁内复核失败", detail)
		return AgentFilesView{}, postPendingError(err, operationID)
	}

	rec, bootErr := s.lifecycle.Bootstrap(ctx, target, operationID)
	if bootErr != nil {
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"复制目标的生命周期状态写入失败", detail)
		return AgentFilesView{}, postPendingError(bootErr, operationID)
	}

	infos, copyErr := workspace.CopyAgentDir(s.root, source, target, rewrites)
	if copyErr != nil {
		if discardErr := s.lifecycle.Discard(ctx, target, operationID); discardErr != nil {
			s.logger.Error("复制失败后撤销目标状态文件失败，操作标记 unknown",
				"source", source, "target", target, "operation_id", operationID, "error", discardErr)
			s.finish(ctx, operationID, core.OperationUnknown, lifecycleResultInternal,
				"复制失败且状态文件撤销失败，需查证", detail)
			return AgentFilesView{}, postPendingError(errors.Join(copyErr, discardErr), operationID)
		}
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"复制 Agent 目录失败", detail)
		return AgentFilesView{}, postPendingError(copyErr, operationID)
	}

	finalDetail := detail
	if s.index != nil {
		if idxErr := s.index.PutLifecycleIndex(ctx, rec, true, ""); idxErr != nil {
			s.logger.Error("复制 Agent：生命周期索引同步失败（状态文件为准，下次启动基线修复）",
				"target", target, "operation_id", operationID, "error", idxErr)
			if deferred, marshalErr := json.Marshal(agentCopyOperationDetail{
				Source:     source,
				Target:     target,
				IndexWrite: agentIndexWriteDeferred,
			}); marshalErr == nil {
				finalDetail = deferred
			}
		}
	}
	s.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", finalDetail)

	// 复制成功即按副本的定义重建投影：副本的新身份与改写后的绑定清单都已落盘，
	// 不能沿用源 Agent 的投影行（那属于源身份）。
	s.rebuildActiveProjection(ctx, target, rec.AgentUID)

	if view, ok := s.readFilesView(ctx, target); ok {
		return view, nil
	}
	return AgentFilesView{
		Agent:     target,
		Lifecycle: s.lifecycleView(ctx, target),
		Files:     infoEntries(infos),
	}, nil
}

// Archive 按图 F 的九步归档一个 Agent（约束 32 / 33）：锁内注销定时 → 移出注册表 →
// 有界等待在途 Run 退出 → 状态迁移为 archived（generation+1）→ 目录单次 rename 到
// `archive/<agent>-<UTC 时间戳>/` → 清单与 `content_hash` → 归档行与操作终态同事务。
//
// 回滚矩阵逐档收敛：注销失败 / 等待超时恢复 ①② 并置 failed；状态迁移或目录搬迁失败
// 回退状态（generation 再 +1）并恢复 ①②；归档行失败反向 rename + 状态回退 + 恢复 ①②；
// **任一补偿失败一律 `unknown`，不得谎报 success**。状态文件保留为 `archived` 供原名恢复
// 校验，索引写失败只记 ERROR（索引可重建，不进权威链）。
// Archive runs the nine-step figure-F sequence. Every compensation failure converges to
// an unknown operation; the state file is kept at archived for the original-name restore.
func (s *AgentAdmin) Archive(ctx context.Context, actor core.Actor, name string) (core.AgentArchiveRecord, error) {
	if err := workspace.ValidateAgentName(name); err != nil {
		return core.AgentArchiveRecord{}, &core.InvalidInputError{Tool: agentArchiveTool, Reason: err.Error()}
	}
	if s.archives == nil {
		return core.AgentArchiveRecord{}, errors.New("归档管理面缺少归档存储（装配错误）")
	}
	live, depsErr := s.archiveLiveDeps()
	if depsErr != nil {
		return core.AgentArchiveRecord{}, depsErr
	}

	initial, err := s.checkArchiveSource(ctx, name)
	if err != nil {
		return core.AgentArchiveRecord{}, err
	}
	detail, err := json.Marshal(agentArchiveOperationDetail{BeforeState: string(initial.State)})
	if err != nil {
		return core.AgentArchiveRecord{}, fmt.Errorf("组装归档操作详情: %w", err)
	}
	operationID, beginErr := s.beginAgentOperation(ctx, actor, core.ActionAgentArchive, name,
		agentRequestHash(strings.Join([]string{core.ActionAgentArchive, name}, "|")), detail)
	if beginErr != nil {
		return core.AgentArchiveRecord{}, beginErr
	}

	unlock := s.gate.Lock(name)
	defer unlock()

	current, err := s.checkArchiveSource(ctx, name)
	if err != nil {
		s.finish(ctx, operationID, core.OperationFailed, agentResultCode(err),
			"归档前置条件在锁内复核失败", detail)
		return core.AgentArchiveRecord{}, postPendingError(err, operationID)
	}
	beforeState := current.State

	registration := archiveRegistration{}
	if live {
		registration = s.captureArchiveRegistration(name)
		if unregErr := s.unregisterForArchive(ctx, name); unregErr != nil {
			if regErr := s.restoreArchiveRegistration(ctx, name, registration); regErr != nil {
				s.logger.Error("归档注销失败且注册 / 定时恢复失败，操作标记 unknown",
					"agent", name, "operation_id", operationID, "error", errors.Join(unregErr, regErr))
				s.finish(ctx, operationID, core.OperationUnknown, lifecycleResultInternal,
					"归档注销失败且回滚失败，需查证", detail)
				return core.AgentArchiveRecord{}, postPendingError(errors.Join(unregErr, regErr), operationID)
			}
			s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
				"归档注销定时任务失败", detail)
			return core.AgentArchiveRecord{}, postPendingError(unregErr, operationID)
		}
		s.registry.Remove(name)

		remaining, runErr := s.awaitArchiveRuns(ctx, name)
		if runErr != nil {
			if regErr := s.restoreArchiveRegistration(ctx, name, registration); regErr != nil {
				s.logger.Error("列出 / 等待在途 Run 失败且注册恢复失败，操作标记 unknown",
					"agent", name, "operation_id", operationID, "error", errors.Join(runErr, regErr))
				s.finish(ctx, operationID, core.OperationUnknown, lifecycleResultInternal,
					"归档等待在途 Run 失败且回滚失败，需查证", detail)
				return core.AgentArchiveRecord{}, postPendingError(errors.Join(runErr, regErr), operationID)
			}
			s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
				"归档等待在途 Run 失败", detail)
			return core.AgentArchiveRecord{}, postPendingError(runErr, operationID)
		}
		if len(remaining) > 0 {
			// 超时不是"稍后再说"：已发出的取消信号保留（不回滚取消），只回滚 ①②。
			busy := &core.AgentBusyError{RunIDs: remaining, OperationID: operationID}
			if regErr := s.restoreArchiveRegistration(ctx, name, registration); regErr != nil {
				s.logger.Error("归档等待超时且注册恢复失败，操作标记 unknown",
					"agent", name, "operation_id", operationID,
					"run_ids", remaining, "error", regErr)
				s.finish(ctx, operationID, core.OperationUnknown, agentResultAgentBusy,
					"归档等待超时且回滚失败，需查证", detail)
				return core.AgentArchiveRecord{}, postPendingError(errors.Join(busy, regErr), operationID)
			}
			s.finish(ctx, operationID, core.OperationFailed, agentResultAgentBusy,
				"归档等待在途 Run 超时", detail)
			return core.AgentArchiveRecord{}, postPendingError(busy, operationID)
		}
	} else {
		s.logger.Debug("CLI 持有实例锁：归档跳过注册表注销 / 调度取消 / Run 等待",
			"agent", name, "operation_id", operationID)
	}

	archivedRec, transErr := s.lifecycle.Transition(ctx, name, beforeState, current.Generation,
		core.AgentArchived, operationID)
	if transErr != nil {
		code := lifecycleResultInternal
		if errors.Is(transErr, core.ErrLifecycleConflict) {
			code = agentResultConflict
		}
		var regErr error
		if live {
			regErr = s.restoreArchiveRegistration(ctx, name, registration)
		}
		if regErr != nil {
			s.logger.Error("归档状态迁移失败且注册恢复失败，操作标记 unknown",
				"agent", name, "operation_id", operationID, "error", errors.Join(transErr, regErr))
			s.finish(ctx, operationID, core.OperationUnknown, code,
				"归档状态迁移失败且回滚失败，需查证", detail)
			return core.AgentArchiveRecord{}, postPendingError(errors.Join(transErr, regErr), operationID)
		}
		s.finish(ctx, operationID, core.OperationFailed, code, "归档状态迁移失败", detail)
		return core.AgentArchiveRecord{}, postPendingError(transErr, operationID)
	}

	archiveAt := s.now()
	result, archiveErr := workspace.ArchiveAgentDir(s.root, name, archiveAt)
	if archiveErr != nil {
		// 反向 rename 在 ArchiveAgentDir 内部完成；能走到这里仍报错而源目录已不在活动区，
		// 说明目录归属不确定——**不**回退状态、**不**恢复注册（那会让注册表指向不存在的目录）。
		_, _, sourceErr := workspace.ManagedAgentFiles(s.root, name)
		if sourceErr != nil && !errors.Is(sourceErr, fs.ErrNotExist) {
			s.logger.Error("归档目录搬迁失败且源目录状态不可判定，操作标记 unknown",
				"agent", name, "operation_id", operationID, "error", archiveErr, "source_error", sourceErr)
			s.finish(ctx, operationID, core.OperationUnknown, lifecycleResultInternal,
				"归档目录状态不确定，需查证", detail)
			return core.AgentArchiveRecord{}, postPendingError(archiveTargetConflictError(archiveErr), operationID)
		}
		if errors.Is(sourceErr, fs.ErrNotExist) {
			s.logger.Error("归档目录搬迁失败且源目录已不在活动区（反向 rename 未完成），操作标记 unknown",
				"agent", name, "operation_id", operationID, "error", archiveErr)
			s.finish(ctx, operationID, core.OperationUnknown, lifecycleResultInternal,
				"归档目录已移出活动区但目录搬迁失败，需查证", detail)
			return core.AgentArchiveRecord{}, postPendingError(archiveTargetConflictError(archiveErr), operationID)
		}

		code := agentResultCode(archiveErr)
		message := "归档目录搬迁失败"
		if errors.Is(archiveErr, workspace.ErrArchiveTargetExists) {
			code = agentResultConflict
			message = "归档目标已存在，未执行归档"
		}
		var compensations []error
		if rbErr := s.rollbackArchiveState(ctx, name, beforeState, archivedRec.Generation, operationID); rbErr != nil {
			compensations = append(compensations, rbErr)
		}
		if live {
			if regErr := s.restoreArchiveRegistration(ctx, name, registration); regErr != nil {
				compensations = append(compensations, regErr)
			}
		}
		if joined := errors.Join(compensations...); joined != nil {
			s.logger.Error("归档目录搬迁失败且回滚未完成，操作标记 unknown",
				"agent", name, "operation_id", operationID, "error", errors.Join(archiveErr, joined))
			s.finish(ctx, operationID, core.OperationUnknown, code, message+"且回滚失败，需查证", detail)
			return core.AgentArchiveRecord{}, postPendingError(
				errors.Join(archiveTargetConflictError(archiveErr), joined), operationID)
		}
		s.finish(ctx, operationID, core.OperationFailed, code, message, detail)
		return core.AgentArchiveRecord{}, postPendingError(archiveTargetConflictError(archiveErr), operationID)
	}

	// 身份必须随归档行落库：`uid_source` 由 `AgentUID` 派生，缺了会把 v3 归档误记成
	// `legacy_unrecorded`，启动全量重建随之无法把归档副本挂回原身份（只能当作无身份的
	// 旧归档跳过）。归档不换身份——目录只是 rename，字节不变。
	// The identity rides with the archive row: uid_source is derived from it, and the
	// startup rebuild needs it to attach an archived copy to its owner.
	record := core.AgentArchiveRecord{
		ArchiveID:    result.ArchiveID,
		AgentName:    name,
		AgentUID:     archivedRec.AgentUID,
		ArchivedAt:   archiveAt.UTC().UnixMilli(),
		ContentHash:  result.ContentHash,
		ManifestJSON: result.ManifestJSON,
		OperationID:  operationID,
	}
	// 归档行与操作终态同事务（约束 37）；终态写不随请求取消而放弃。
	if insertErr := s.archives.InsertArchive(context.WithoutCancel(ctx), record, operationID); insertErr != nil {
		s.logger.Error("归档记录写入失败，开始回滚", "agent", name,
			"archive_id", result.ArchiveID, "operation_id", operationID, "error", insertErr)
		var compensations []error
		if rbErr := workspace.UnarchiveAgentDir(s.root, result.ArchiveID, name); rbErr != nil {
			// 目录归属不确定：不再回退状态、不恢复注册，整体标 unknown 交人工查证。
			compensations = append(compensations, rbErr)
		} else {
			if rbErr := s.rollbackArchiveState(ctx, name, beforeState, archivedRec.Generation, operationID); rbErr != nil {
				compensations = append(compensations, rbErr)
			}
			if live {
				if regErr := s.restoreArchiveRegistration(ctx, name, registration); regErr != nil {
					compensations = append(compensations, regErr)
				}
			}
		}
		if joined := errors.Join(compensations...); joined != nil {
			s.logger.Error("归档记录写入失败且回滚未完成，操作标记 unknown",
				"agent", name, "archive_id", result.ArchiveID,
				"operation_id", operationID, "error", errors.Join(insertErr, joined))
			s.finish(ctx, operationID, core.OperationUnknown, lifecycleResultInternal,
				"归档记录写入失败且回滚未完成，需查证", detail)
			return core.AgentArchiveRecord{}, postPendingError(errors.Join(insertErr, joined), operationID)
		}
		s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"归档记录写入失败（目录已回退）", detail)
		return core.AgentArchiveRecord{}, postPendingError(insertErr, operationID)
	}

	// ⑨ 索引是派生视图：写失败只记 ERROR，状态文件仍是权威（约束 32 / 36）。
	s.syncLifecycleIndex(context.WithoutCancel(ctx), archivedRec)
	// 归档只是把目录整体 rename 走、字节不变：投影按归档副本重建（身份仍是状态文件里的
	// agent_uid），失败只记 ERROR——投影可重建，归档已经生效。
	s.rebuildArchivedProjection(ctx, name, archivedRec.AgentUID, result.ArchiveID)
	return record, nil
}

// Restore 按图 F 的恢复流程把一条归档记录恢复为 Agent（约束 31）：三项前置校验
// （唯一未恢复归档行、归档清单 `content_hash`、目标名与目标占用）全部在预写**之前**
// 完成，失败零写入零操作行；原名恢复以状态文件的 generation 做条件
// `Transition(archived → disabled)`（**不** Bootstrap、**不**重置 generation、**不**删
// 状态文件），异名恢复才 `Bootstrap(disabled, generation=1)`。
//
// 目录落位失败或归档行标记失败 → 按路径回退：删目标目录，原名路径 `Transition(disabled →
// archived, 当前 generation)`（generation 再 +1），异名路径 `Discard`；任一补偿失败 →
// `unknown`。归档目录**从不移动**（恢复是复制），归档行保持未恢复、可重试。
// Restore validates the archive identity, manifest hash and target before pre-writing;
// the original-name path keeps the generation through a conditional transition, and the
// renamed path bootstraps a fresh disabled agent. The archive directory is never moved.
func (s *AgentAdmin) Restore(ctx context.Context, actor core.Actor, archiveID, newName string) (AgentFilesView, error) {
	if s.archives == nil {
		return AgentFilesView{}, errors.New("恢复管理面缺少归档存储（装配错误）")
	}

	row, err := s.archives.ArchiveByID(ctx, archiveID)
	if err != nil {
		// 未命中由存储层包 `core.ErrResourceNotFound`（web 映射 404）。
		return AgentFilesView{}, err
	}
	if row.RestoredAt != nil {
		return AgentFilesView{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "归档已恢复，拒绝重复恢复：" + archiveID,
		}
	}

	target := newName
	if target == "" {
		target = row.AgentName
	}
	if err := workspace.ValidateAgentName(target); err != nil {
		return AgentFilesView{}, &core.InvalidInputError{Tool: agentRestoreTool, Reason: err.Error()}
	}
	renamed := target != row.AgentName

	// ③ 目标占用：异名要求目录与状态文件都不存在；原名要求目录不存在、状态文件存在且
	// 为 archived（读取在取锁后再复核一次，生成条件迁移的 expectedGeneration）。
	if renamed {
		if err := s.checkTargetFree(ctx, target, "恢复"); err != nil {
			return AgentFilesView{}, err
		}
	} else {
		if _, _, err := workspace.ManagedAgentFiles(s.root, target); err == nil {
			return AgentFilesView{}, &core.AdminError{
				Kind:    core.ErrConflict,
				Message: "原名恢复的目标目录已存在：" + target,
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return AgentFilesView{}, err
		}
		if _, err := s.readArchivedStateFile(ctx, target); err != nil {
			return AgentFilesView{}, err
		}
	}

	// ② 归档清单必须可逐字节复算且与归档行一致；不匹配 → 409，归档不动、不写操作行。
	archiveDir := path.Join(agentArchiveDirName, archiveID)
	contentHash, hashErr := workspace.ArchiveDirContentHash(s.root, archiveDir)
	if hashErr != nil {
		return AgentFilesView{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "归档目录清单无法复算，拒绝恢复：" + archiveID + "（" + hashErr.Error() + "）",
		}
	}
	if contentHash != row.ContentHash {
		return AgentFilesView{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "归档目录 content_hash 与归档记录不一致，拒绝恢复：" + archiveID,
		}
	}

	detail, err := json.Marshal(agentRestoreOperationDetail{
		ArchiveID: archiveID,
		Target:    target,
		Renamed:   renamed,
	})
	if err != nil {
		return AgentFilesView{}, fmt.Errorf("组装恢复操作详情: %w", err)
	}
	operationID, beginErr := s.beginAgentOperation(ctx, actor, core.ActionAgentRestore, target,
		agentRequestHash(strings.Join([]string{core.ActionAgentRestore, archiveID, target}, "|")), detail)
	if beginErr != nil {
		return AgentFilesView{}, beginErr
	}

	unlock := s.gate.Lock(target)
	defer unlock()

	var expectedGeneration int64
	if renamed {
		if err := s.checkTargetFree(ctx, target, "恢复"); err != nil {
			s.finish(ctx, operationID, core.OperationFailed, agentResultCode(err),
				"恢复目标在锁内复核失败", detail)
			return AgentFilesView{}, postPendingError(err, operationID)
		}
	} else {
		stateRec, stateErr := s.readArchivedStateFile(ctx, target)
		if stateErr != nil {
			s.finish(ctx, operationID, core.OperationFailed, agentResultConflict,
				"原名恢复的状态前提在锁内复核失败", detail)
			return AgentFilesView{}, postPendingError(stateErr, operationID)
		}
		expectedGeneration = stateRec.Generation
	}

	var rec core.LifecycleRecord
	if renamed {
		// 异名才 Bootstrap：目标是一个全新的 disabled Agent，generation 从 1 起。
		rec, err = s.lifecycle.Bootstrap(ctx, target, operationID)
		if err != nil {
			s.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
				"恢复目标的生命周期状态写入失败", detail)
			return AgentFilesView{}, postPendingError(err, operationID)
		}
	} else {
		rec, err = s.lifecycle.Transition(ctx, target, core.AgentArchived, expectedGeneration,
			core.AgentDisabled, operationID)
		if err != nil {
			code := lifecycleResultInternal
			if errors.Is(err, core.ErrLifecycleConflict) {
				code = agentResultConflict
			}
			s.finish(ctx, operationID, core.OperationFailed, code, "原名恢复的状态迁移失败", detail)
			return AgentFilesView{}, postPendingError(err, operationID)
		}
	}

	infos, restoreErr := workspace.RestoreAgentDir(s.root, archiveDir, target)
	if restoreErr != nil {
		code := agentResultCode(restoreErr)
		return AgentFilesView{}, postPendingError(s.rollbackRestore(ctx, target, renamed, rec.Generation,
			operationID, restoreErr, detail, code, "恢复 Agent 目录失败"), operationID)
	}

	// 索引是派生视图：写失败只记 ERROR（`MarkRestored` 拥有终态写，detail 不再改写）。
	if s.index != nil {
		if idxErr := s.index.PutLifecycleIndex(ctx, rec, true, ""); idxErr != nil {
			s.logger.Error("恢复 Agent：生命周期索引同步失败（状态文件为准，下次启动基线修复）",
				"agent", target, "operation_id", operationID, "error", idxErr)
		}
	}

	// 归档行标记与操作终态同事务（约束 37）：标记失败必须回退目录与状态，归档行保持未恢复。
	if markErr := s.archives.MarkRestored(context.WithoutCancel(ctx), archiveID, target,
		operationID, s.now()); markErr != nil {
		s.logger.Error("归档行标记恢复失败，开始回滚目录与状态",
			"archive_id", archiveID, "agent", target, "operation_id", operationID, "error", markErr)
		return AgentFilesView{}, postPendingError(s.rollbackRestore(ctx, target, renamed, rec.Generation,
			operationID, markErr, detail, lifecycleResultInternal, "归档行标记恢复失败"), operationID)
	}

	// 恢复把归档副本写回活动区：投影必须按**落盘后的定义**重算，不能沿用归档前的残行
	// （期间资源的当前指针可能已经变化）。失败只记 ERROR——恢复已经生效。
	//
	// 异名恢复换成了新身份，归档身份的旧行再也没有定义文件能重建它们（归档目录已经
	// 搬回活动区）：留着会让删除闸门在 `ReferencingAgents` 里看到一个已经不在归档区、
	// 也不在活动区的 Agent，把删除永久挡住。原名恢复两边是同一个身份，重建已覆盖。
	// A renamed restore mints a new identity; the archived identity's rows can no longer
	// be rebuilt by any definition file, so they are cleared instead of blocking deletes.
	s.rebuildActiveProjection(ctx, target, rec.AgentUID)
	if row.AgentUID != "" && row.AgentUID != rec.AgentUID {
		s.clearProjectionIdentity(ctx, row.AgentUID)
	}

	if view, ok := s.readFilesView(ctx, target); ok {
		return view, nil
	}
	return AgentFilesView{
		Agent:     target,
		Lifecycle: s.lifecycleView(ctx, target),
		Files:     infoEntries(infos),
	}, nil
}

// Archived 列出全部归档记录（新的在前），供 `/archived-agents` 与引用扫描共用。
// Archived lists every archive row, newest first.
func (s *AgentAdmin) Archived(ctx context.Context) ([]core.AgentArchiveRecord, error) {
	if s.archives == nil {
		return nil, errors.New("归档列表缺少归档存储（装配错误）")
	}
	return s.archives.ListArchives(ctx)
}

// beginAgentOperation 预写一条 pending 管理操作行并返回操作标识；失败即拒绝执行，
// 目标一个字节都没动（约束 1）。标识只经返回值传递，不落实例字段。
// beginAgentOperation pre-writes the pending row and returns the id by value.
func (s *AgentAdmin) beginAgentOperation(ctx context.Context, actor core.Actor, action, targetID, requestHash string, detail []byte) (string, error) {
	now := s.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   targetKindAgent,
		TargetID:     targetID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  requestHash,
		Status:       core.OperationPending,
		DetailJSON:   detail,
		CreatedAt:    now,
	}
	if beginErr := s.operations.BeginOperation(ctx, record); beginErr != nil {
		s.logger.Error("管理写预写操作记录失败，拒绝执行",
			"action", action, "target", targetID, "error", beginErr)
		return "", fmt.Errorf("%s 预写操作记录失败，拒绝执行: %w", action, beginErr)
	}
	return operationID, nil
}

// postPendingError 给 `pending` 之后的失败补上可追查的操作标识（约束 1a / 约束 43）：
// 响应必须带 `operation_id`，调用方据此走 `GET /operations/{id}` 查证终态。分类哨兵
// 原样保留为 `AdminError.Kind`，`errors.Is` / `errors.As` 穿透不变；已带操作标识的错误
// （引用闸门 400、`AgentBusyError`、`operationFailedError`）原样返回，不重复包裹。
// 对外文案只复用既有 `AdminError.Message`——内部错误原文仍只进日志。
// postPendingError attaches the queryable operation id to a failure that happened after
// the pending row landed. The classification sentinel stays the unwrap target, so
// errors.Is/As keep working, and already-tagged errors pass through unchanged.
func postPendingError(err error, operationID string) error {
	if err == nil || operationID == "" {
		return err
	}
	existing := core.AdminErrorData(err)
	if _, ok := existing["operation_id"]; ok {
		return err
	}
	data := make(map[string]any, len(existing)+1)
	for key, value := range existing {
		data[key] = value
	}
	data["operation_id"] = operationID
	message := ""
	var adminErr *core.AdminError
	if errors.As(err, &adminErr) {
		message = adminErr.Message
	}
	return &core.AdminError{Kind: err, Message: message, Data: data}
}

// archiveTargetConflictError 把"归档目标已存在"提升为 409 分类（约束 33），同时保留
// `workspace.ErrArchiveTargetExists` 的哨兵身份（测试与恢复查证都按 errors.Is 判定），
// 因此 Kind 取两个哨兵的 Join——两个身份都判得出来。
// archiveTargetConflictError promotes an occupied archive target to the conflict class
// while keeping the workspace sentinel reachable through errors.Is.
func archiveTargetConflictError(err error) error {
	if !errors.Is(err, workspace.ErrArchiveTargetExists) {
		return err
	}
	return &core.AdminError{
		Kind:    errors.Join(core.ErrConflict, err),
		Message: "归档目标已存在，未执行归档",
	}
}

// archiveLiveDeps 判定归档的三项活动面依赖是否可用：全有（HTTP 路径）或全无（CLI 路径）；
// 部分装配是装配错误，**不得**静默降级成"跳过注销"。
// archiveLiveDeps requires the registry, scheduler and run canceller all-or-nothing.
func (s *AgentAdmin) archiveLiveDeps() (bool, error) {
	provided := 0
	for _, present := range []bool{s.registry != nil, s.scheduler != nil, s.canceller != nil} {
		if present {
			provided++
		}
	}
	switch provided {
	case 0:
		return false, nil
	case 3:
		return true, nil
	default:
		return false, errors.New(
			"归档管理面依赖不完整：注册表 / 调度 / Run 取消器必须同时提供或同时省略（装配错误）")
	}
}

// checkArchiveSource 判定归档源可归档：状态文件存在且为 active / disabled，目录存在。
// 缺失 / 已归档 → 404 或 409；读取失败与损坏 → 409（拒绝在不确定的状态上归档）。
// checkArchiveSource requires an active-area agent: an existing state file in active or
// disabled and an existing directory.
func (s *AgentAdmin) checkArchiveSource(ctx context.Context, name string) (core.LifecycleRecord, error) {
	rec, exists, err := s.lifecycle.Read(ctx, name)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return core.LifecycleRecord{}, &core.AgentNotFoundError{Name: name}
	case err != nil:
		return core.LifecycleRecord{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "Agent 生命周期状态文件不可读或校验失败，拒绝归档：" + name,
		}
	case !exists:
		return core.LifecycleRecord{}, &core.AgentNotFoundError{Name: name}
	}
	if rec.State != core.AgentActive && rec.State != core.AgentDisabled {
		return core.LifecycleRecord{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "Agent 已不在活动区（状态 " + string(rec.State) + "），拒绝重复归档：" + name,
		}
	}
	if _, _, dirErr := workspace.ManagedAgentFiles(s.root, name); dirErr != nil {
		if errors.Is(dirErr, fs.ErrNotExist) {
			return core.LifecycleRecord{}, &core.AgentNotFoundError{Name: name}
		}
		return core.LifecycleRecord{}, dirErr
	}
	return rec, nil
}

// checkCopySource 判定复制源在活动区：状态文件存在且为 active / disabled，目录存在；
// 返回状态记录——复制要拿源 `agent_uid` 判定哪些绑定是源专属（不按名称猜）。
// checkCopySource requires an existing active-area source agent and returns its record.
func (s *AgentAdmin) checkCopySource(ctx context.Context, source string) (core.LifecycleRecord, error) {
	rec, exists, err := s.lifecycle.Read(ctx, source)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return core.LifecycleRecord{}, &core.AgentNotFoundError{Name: source}
	case err != nil:
		return core.LifecycleRecord{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "复制源 Agent 的生命周期状态文件不可读或校验失败：" + source,
		}
	case !exists:
		return core.LifecycleRecord{}, &core.AgentNotFoundError{Name: source}
	}
	if rec.State != core.AgentActive && rec.State != core.AgentDisabled {
		return core.LifecycleRecord{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "复制源不在活动区（状态 " + string(rec.State) + "）：" + source,
		}
	}
	if _, _, dirErr := workspace.ManagedAgentFiles(s.root, source); dirErr != nil {
		if errors.Is(dirErr, fs.ErrNotExist) {
			return core.LifecycleRecord{}, &core.AgentNotFoundError{Name: source}
		}
		return core.LifecycleRecord{}, dirErr
	}
	return rec, nil
}

// resolveCopyMappings 在预写**之前**解析复制请求的资源映射（FR-070 / 约束 1）：以源定义
// 文件的 `resource_bindings` 为准逐条判定绑定是否**源专属**（`scope=exclusive` 且属主 =
// 源 `agent_uid`）。缺映射的专属绑定 → `core.ErrAgentCopyBlocked`（409，`data.problems`
// 列明资源名与类型；零副本、零操作行）；映射本身非法（来源不属于该 Agent、同源重复、
// 目标不存在 / 已删除 / 未发布 / 不是共享 / 沿用源 UID）→ 400。
//
// 目标必须是**工作区共享**资源：副本的 `agent_uid` 在 `Bootstrap` 时才生成，复制时刻
// 任何专属资源都不可能以副本为属主——"为副本新建专属资源"是复制之后的两步流程，不能
// 塞进同一个请求。共享绑定随目录携带（UID 不变），不进返回的改写计划。
//
// 本函数只读：不写盘、不写操作行；返回的计划交给 workspace 在 staging 里一次性落盘。
// 检查与复制之间源定义可能被并发修改——那时的计划与文件对不上，workspace 的改写前置
// 校验会 fail-closed 拒绝，候选树随之丢弃（不产生半改写的副本）。
// resolveCopyMappings decides which bindings are exclusive to the source, rejects the
// copy when any lacks a target, and returns the validated rewrite plan.
func (s *AgentAdmin) resolveCopyMappings(ctx context.Context, source string, sourceUID core.AgentUID,
	mapping []core.ResourceMapping,
) ([]workspace.AgentCopyRewrite, error) {
	invalid := func(reason string) error {
		return &core.InvalidInputError{Tool: agentCopyTool, Reason: reason}
	}

	definition, readErr := workspace.ReadAgentDefinition(s.root, source)
	if readErr != nil {
		return nil, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "复制源缺少 " + agentDefinitionFileName + "，拒绝复制：" + source,
		}
	}
	bindings, parseErr := workspace.ParseResourceBindings(definition)
	if parseErr != nil {
		// 定义不可解析时无法判定哪些绑定是专属：按「可能引用」拒绝，不猜、不部分放行。
		return nil, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "复制源的 resource_bindings 不可解析，拒绝复制：" + source,
		}
	}
	if len(bindings.ModelConnections) == 0 && len(bindings.NotifyChannels) == 0 {
		if len(mapping) > 0 {
			return nil, invalid("复制源的 Agent 没有可映射的资源绑定")
		}
		return nil, nil
	}
	if len(bindings.ModelConnections) > 0 && s.resources == nil {
		return nil, errors.New("复制管理面缺少资源存储（装配错误）")
	}
	if len(bindings.NotifyChannels) > 0 && s.notify == nil {
		return nil, errors.New("复制管理面缺少通知渠道存储（装配错误）")
	}

	type sourceKey struct {
		kind core.BindingKind
		uid  core.ResourceUID
	}
	byUID := make(map[sourceKey]core.ResourceBinding,
		len(bindings.ModelConnections)+len(bindings.NotifyChannels))
	for _, entry := range bindings.ModelConnections {
		byUID[sourceKey{core.BindingModelConnection, entry.UID}] = entry
	}
	for _, entry := range bindings.NotifyChannels {
		byUID[sourceKey{core.BindingNotifyChannel, entry.UID}] = entry
	}

	covered := make(map[sourceKey]struct{}, len(mapping))
	rewrites := make([]workspace.AgentCopyRewrite, 0, len(mapping))
	for _, entry := range mapping {
		kind, kindErr := core.ParseBindingKind(string(entry.Kind))
		if kindErr != nil {
			return nil, invalid("resource_mapping 的 resource_kind 非法：" + string(entry.Kind))
		}
		key := sourceKey{kind, entry.SourceUID}
		binding, ok := byUID[key]
		if !ok {
			return nil, invalid("resource_mapping 的 source_resource_uid 不属于该 Agent 的" +
				string(kind) + "绑定：" + string(entry.SourceUID))
		}
		if _, dup := covered[key]; dup {
			return nil, invalid("resource_mapping 对同一来源重复映射：" + binding.Name)
		}
		covered[key] = struct{}{}
		targetName, targetErr := s.checkCopyMappingTarget(ctx, entry)
		if targetErr != nil {
			return nil, targetErr
		}
		rewrites = append(rewrites, workspace.AgentCopyRewrite{
			Kind:     kind,
			FromName: binding.Name,
			FromUID:  binding.UID,
			ToName:   targetName,
			ToUID:    entry.TargetUID,
		})
	}

	// 专属扫描：缺映射的专属绑定汇总成一条 409，`problems` 按固定写法点名（类别 + 名称）。
	problems := make([]string, 0, 2)
	exclusiveModels, exclusiveChannels := 0, 0
	for _, entry := range bindings.ModelConnections {
		exclusive, err := s.modelBindingExclusiveTo(ctx, sourceUID, entry)
		if err != nil {
			return nil, err
		}
		if !exclusive {
			continue
		}
		exclusiveModels++
		if _, mapped := covered[sourceKey{core.BindingModelConnection, entry.UID}]; !mapped {
			problems = append(problems, string(core.BindingModelConnection)+" "+entry.Name)
		}
	}
	for _, entry := range bindings.NotifyChannels {
		exclusive, err := s.notifyBindingExclusiveTo(ctx, sourceUID, entry)
		if err != nil {
			return nil, err
		}
		if !exclusive {
			continue
		}
		exclusiveChannels++
		if _, mapped := covered[sourceKey{core.BindingNotifyChannel, entry.UID}]; !mapped {
			problems = append(problems, string(core.BindingNotifyChannel)+" "+entry.Name)
		}
	}
	if len(problems) > 0 {
		return nil, &core.AdminError{
			Kind: core.ErrAgentCopyBlocked,
			Message: fmt.Sprintf(
				"此 Agent 使用了 %d 个专属模型连接和 %d 个专属通知渠道。复制不会带走这些资源或它们的凭据。"+
					"请用 resource_mapping 为副本显式选择替代的共享资源，或先在源 Agent 上把资源改为共享。",
				exclusiveModels, exclusiveChannels),
			Data: map[string]any{"problems": problems},
		}
	}
	return rewrites, nil
}

// modelBindingExclusiveTo 判定一条模型连接绑定是否指向**源 Agent 专属**的当前资源：
// 绑定 UID 必须仍是该名称的当前指针（同名重建后的旧 UID 不算），且指针为 exclusive、
// 属主是源 Agent。绑定已经不指向当前指针时不给"专属"结论——那本来就过不了准入，
// 复制按原样携带（复制不是修复）。
// modelBindingExclusiveTo reports whether the binding still points at a live resource
// exclusive to the source agent.
func (s *AgentAdmin) modelBindingExclusiveTo(ctx context.Context, sourceUID core.AgentUID,
	entry core.ResourceBinding,
) (bool, error) {
	ptr, err := s.resources.Pointer(ctx, core.ResourceModelConnection, entry.Name)
	if err != nil {
		if errors.Is(err, core.ErrResourceNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("读取模型连接 %q 的发布指针: %w", entry.Name, err)
	}
	if ptr.DeletedAt != nil || ptr.ResourceUID != entry.UID {
		return false, nil
	}
	return ptr.Scope == core.ScopeExclusive && ptr.OwnerAgentUID == sourceUID, nil
}

// notifyBindingExclusiveTo 判定一条通知渠道绑定是否指向源 Agent 专属的当前渠道。
// notifyBindingExclusiveTo reports whether the binding still points at a channel
// exclusive to the source agent.
func (s *AgentAdmin) notifyBindingExclusiveTo(ctx context.Context, sourceUID core.AgentUID,
	entry core.ResourceBinding,
) (bool, error) {
	channel, err := s.notify.Get(ctx, entry.Name)
	if err != nil {
		if errors.Is(err, core.ErrResourceNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("读取通知渠道 %q: %w", entry.Name, err)
	}
	if channel.ResourceUID != entry.UID {
		return false, nil
	}
	return channel.Scope == core.ScopeExclusive && channel.OwnerAgentUID == sourceUID, nil
}

// checkCopyMappingTarget 校验一条映射目标对副本可用，返回目标的当前名称。规则与准入
// 同一档：存在、未删除、已发布、工作区共享、不沿用源 UID；任一不成立 → 400 点名。
// checkCopyMappingTarget validates one mapping target and returns its current name.
func (s *AgentAdmin) checkCopyMappingTarget(ctx context.Context,
	entry core.ResourceMapping,
) (string, error) {
	invalid := func(reason string) error {
		return &core.InvalidInputError{Tool: agentCopyTool, Reason: reason}
	}
	if entry.TargetUID == entry.SourceUID {
		return "", invalid("resource_mapping 的目标不得沿用源 UID：" + string(entry.TargetUID))
	}
	if _, uidErr := core.ParseResourceUID(string(entry.TargetUID)); uidErr != nil {
		return "", invalid("resource_mapping 的 target_resource_uid 非法：" + string(entry.TargetUID))
	}

	switch entry.Kind {
	case core.BindingModelConnection:
		ptr, err := s.resources.PointerByUID(ctx, core.ResourceModelConnection, entry.TargetUID)
		if err != nil {
			if errors.Is(err, core.ErrResourceNotFound) {
				return "", invalid("resource_mapping 的目标模型连接不存在：" + string(entry.TargetUID))
			}
			return "", fmt.Errorf("读取映射目标模型连接: %w", err)
		}
		if ptr.DeletedAt != nil || ptr.PublishedVersionID == 0 {
			return "", invalid("resource_mapping 的目标模型连接已删除或从未发布：" + ptr.ResourceID)
		}
		if ptr.Scope != core.ScopeShared {
			return "", invalid("resource_mapping 的目标模型连接必须是工作区共享" +
				"（副本的 agent_uid 在复制完成后才生成，专属资源对它没有资格）：" + ptr.ResourceID)
		}
		return ptr.ResourceID, nil
	case core.BindingNotifyChannel:
		channel, err := s.notify.GetByUID(ctx, entry.TargetUID)
		if err != nil {
			if errors.Is(err, core.ErrResourceNotFound) {
				return "", invalid("resource_mapping 的目标通知渠道不存在：" + string(entry.TargetUID))
			}
			return "", fmt.Errorf("读取映射目标通知渠道: %w", err)
		}
		if channel.DeletedAt != nil {
			return "", invalid("resource_mapping 的目标通知渠道已删除：" + channel.Name)
		}
		if channel.Scope != core.ScopeShared {
			return "", invalid("resource_mapping 的目标通知渠道必须是工作区共享" +
				"（副本的 agent_uid 在复制完成后才生成，专属资源对它没有资格）：" + channel.Name)
		}
		return channel.Name, nil
	default:
		return "", invalid("resource_mapping 的 resource_kind 非法：" + string(entry.Kind))
	}
}

// checkTargetFree 判定复制 / 异名恢复的目标空闲：目标目录与状态文件都必须不存在。
// 任一存在 → 409 CONFLICT（目标与请求不可共存，重试不会自行消失）。
// checkTargetFree requires both the target directory and state file to be absent.
func (s *AgentAdmin) checkTargetFree(ctx context.Context, target, operation string) error {
	if _, _, err := workspace.ManagedAgentFiles(s.root, target); err == nil {
		return &core.AdminError{
			Kind:    core.ErrConflict,
			Message: operation + "目标目录已存在：" + target,
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	_, exists, err := s.lifecycle.Read(ctx, target)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return &core.AdminError{
			Kind:    core.ErrConflict,
			Message: operation + "目标状态文件不可读或损坏：" + target,
		}
	case exists:
		return &core.AdminError{
			Kind:    core.ErrConflict,
			Message: operation + "目标状态文件已存在：" + target,
		}
	}
	return nil
}

// readArchivedStateFile 读取原名恢复要求的状态文件：必须存在、可校验、state=archived。
// `Lifecycle.Read` 已保证记录里的 agent 与目录名一致，而调用方只在 target == 归档行的
// `agent_name` 时走这条路径，因此"与归档身份不符"由这两层共同保证。缺失 / 非 archived /
// 损坏一律 409（不写操作行）。
// readArchivedStateFile requires the target state file to exist and be archived.
func (s *AgentAdmin) readArchivedStateFile(ctx context.Context, target string) (core.LifecycleRecord, error) {
	conflict := func(reason string) error {
		return &core.AdminError{Kind: core.ErrConflict, Message: reason + "：" + target}
	}

	rec, exists, err := s.lifecycle.Read(ctx, target)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return core.LifecycleRecord{}, conflict("原名恢复要求目标状态文件存在（归档后状态文件应保留）")
	case err != nil:
		return core.LifecycleRecord{}, conflict("原名恢复的状态文件不可读或校验失败")
	case !exists:
		return core.LifecycleRecord{}, conflict("原名恢复要求目标状态文件存在")
	}
	if rec.State != core.AgentArchived {
		return core.LifecycleRecord{}, conflict("原名恢复要求目标状态为 archived（当前 " + string(rec.State) + "）")
	}
	return rec, nil
}

// captureArchiveRegistration 在注销前取注册表条目与定时任务快照；未注册时返回零值
// （仍会注销调度可见性，但回滚时无法凭空恢复不存在的条目）。
// captureArchiveRegistration snapshots the registry entry and its schedules.
func (s *AgentAdmin) captureArchiveRegistration(name string) archiveRegistration {
	entry, ok := s.registry.Lookup(name)
	if !ok {
		return archiveRegistration{}
	}
	snapshot := archiveRegistration{entry: entry, registered: true}
	if entry.Profile != nil {
		snapshot.schedules = append([]core.Schedule(nil), entry.Profile.Schedules...)
	}
	return snapshot
}

// unregisterForArchive 先移出调度可见 / 准入集合，再退休任务行（写 `retired_at`，保留行）。
// unregisterForArchive hides the agent from scheduling and retires its task rows.
func (s *AgentAdmin) unregisterForArchive(ctx context.Context, name string) error {
	if err := s.scheduler.SetRegistered(name, false); err != nil {
		return fmt.Errorf("注销 Agent %s 的调度注册: %w", name, err)
	}
	if err := s.scheduler.ReconcileAgent(ctx, name, nil); err != nil {
		return fmt.Errorf("退休 Agent %s 的定时任务: %w", name, err)
	}
	return nil
}

// restoreArchiveRegistration 恢复 ①②：清 `retired_at` 的定时任务、重新放回注册表。
// 用与请求取消解耦的上下文——补偿是变更的一部分，客户端断开不能把它半途丢下。
// restoreArchiveRegistration rolls back steps ①② with a cancellation-detached context.
func (s *AgentAdmin) restoreArchiveRegistration(ctx context.Context, name string, snapshot archiveRegistration) error {
	rollbackCtx := context.WithoutCancel(ctx)
	var errs []error
	if err := s.scheduler.ReconcileAgent(rollbackCtx, name, snapshot.schedules); err != nil {
		errs = append(errs, fmt.Errorf("恢复 Agent %s 的定时任务: %w", name, err))
	}
	if snapshot.registered {
		if err := s.scheduler.SetRegistered(name, true); err != nil {
			errs = append(errs, fmt.Errorf("恢复 Agent %s 的调度注册: %w", name, err))
		}
		s.registry.Replace(name, snapshot.entry)
	}
	return errors.Join(errs...)
}

// awaitArchiveRuns 取消该 Agent 的在途 Run 并在有界窗口内等待退出，返回窗口结束时仍在
// 运行的标识。取消信号是尽力而为（单个取消失败只记 WARN，等待结果才是判据）。
// awaitArchiveRuns signals cancellation and waits up to the bounded window.
func (s *AgentAdmin) awaitArchiveRuns(ctx context.Context, name string) ([]string, error) {
	runIDs, err := s.canceller.RunningRunIDs(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("列出 Agent %s 的在途 Run: %w", name, err)
	}
	if len(runIDs) == 0 {
		return nil, nil
	}
	for _, runID := range runIDs {
		if cancelErr := s.canceller.CancelRun(ctx, runID); cancelErr != nil {
			s.logger.Warn("归档：取消在途 Run 失败，继续等待",
				"agent", name, "run_id", runID, "error", cancelErr)
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.busyWait)
	defer cancel()
	remaining, waitErr := s.canceller.WaitRuns(waitCtx, runIDs)
	if waitErr != nil {
		return nil, fmt.Errorf("等待 Agent %s 的在途 Run 退出: %w", name, waitErr)
	}
	return remaining, nil
}

// rollbackArchiveState 把状态文件从 archived 回退到归档前状态（generation 再 +1，
// 不回退），并同步派生索引。
// rollbackArchiveState transitions archived back to the pre-archive state and bumps the
// generation again; a failed index sync is logged only.
func (s *AgentAdmin) rollbackArchiveState(ctx context.Context, name string, beforeState core.AgentState, archivedGeneration int64, operationID string) error {
	rollbackCtx := context.WithoutCancel(ctx)
	rec, err := s.lifecycle.Transition(rollbackCtx, name, core.AgentArchived, archivedGeneration,
		beforeState, operationID)
	if err != nil {
		return fmt.Errorf("回退 Agent %s 的生命周期状态: %w", name, err)
	}
	s.syncLifecycleIndex(rollbackCtx, rec)
	return nil
}

// rollbackRestore 是恢复失败的统一回退：删目标目录（恢复是复制，归档副本仍在），
// 原名路径把状态 `disabled → archived`（generation 再 +1），异名路径 `Discard`。
// 任一补偿失败 → 操作 unknown 并返回 joined 错误（**不**谎报 failed / success）。
// rollbackRestore removes the restored directory and rolls the lifecycle state back.
func (s *AgentAdmin) rollbackRestore(ctx context.Context, target string, renamed bool, generation int64,
	operationID string, cause error, detail []byte, code, message string,
) error {
	rollbackCtx := context.WithoutCancel(ctx)
	var compensations []error
	if rmErr := workspace.RemoveAgentDir(s.root, target); rmErr != nil {
		compensations = append(compensations, rmErr)
	}
	if renamed {
		if discardErr := s.lifecycle.Discard(rollbackCtx, target, operationID); discardErr != nil {
			compensations = append(compensations, fmt.Errorf("撤销恢复目标 %s 的状态文件: %w", target, discardErr))
		}
	} else {
		rec, transErr := s.lifecycle.Transition(rollbackCtx, target, core.AgentDisabled, generation,
			core.AgentArchived, operationID)
		if transErr != nil {
			compensations = append(compensations, fmt.Errorf("回退 %s 的生命周期状态: %w", target, transErr))
		} else {
			s.syncLifecycleIndex(rollbackCtx, rec)
		}
	}
	if joined := errors.Join(compensations...); joined != nil {
		s.logger.Error("恢复失败且回滚未完成，操作标记 unknown",
			"agent", target, "operation_id", operationID, "error", errors.Join(cause, joined))
		s.finish(ctx, operationID, core.OperationUnknown, code, message+"且回滚失败，需查证", detail)
		return errors.Join(cause, joined)
	}
	s.finish(ctx, operationID, core.OperationFailed, code, message, detail)
	return cause
}

// syncLifecycleIndex 把一次生命周期变更同步到派生索引；失败只记 ERROR——状态文件是权威，
// 索引在下次启动基线修复（约束 32 / 36）。
// syncLifecycleIndex writes the derived index; a failure is logged, never rolled back.
func (s *AgentAdmin) syncLifecycleIndex(ctx context.Context, rec core.LifecycleRecord) {
	if s.index == nil {
		return
	}
	if err := s.index.PutLifecycleIndex(ctx, rec, true, ""); err != nil {
		s.logger.Error("生命周期索引同步失败（状态文件为准，下次启动基线修复）",
			"agent", rec.Agent, "state", string(rec.State), "error", err)
	}
}

// rebuildActiveProjection 按活动区的定义文件重建一个 Agent 的绑定投影（创建 / 复制 /
// 多文件提交 / 恢复成功之后调用）。投影是派生视图：定义读不到或重建失败只记 ERROR——
// 写操作本身已经生效，**不得**把投影失败改写成写失败（与索引同步同一纪律）。
// rebuildActiveProjection rebuilds one agent's projection from its definition file after a
// successful write; a failure is logged and never turned into a write failure.
func (s *AgentAdmin) rebuildActiveProjection(ctx context.Context, agent string, uid core.AgentUID) {
	RebuildAgentFromDisk(ctx, s.projection, s.root, agent, uid, s.logger)
}

// rebuildArchivedProjection 按归档副本的定义文件重建投影（归档成功之后调用）：归档
// 只做目录 rename、字节不变，因此身份仍取归档记录里的 `agent_uid`。
// rebuildArchivedProjection rebuilds the projection from the archived copy, which keeps
// the recorded identity because archiving only renames the directory.
func (s *AgentAdmin) rebuildArchivedProjection(ctx context.Context, agent string, uid core.AgentUID,
	archiveID string,
) {
	if s.projection == nil {
		return
	}
	definition, err := workspace.ReadArchivedAgentDefinition(s.root, archiveID)
	if err != nil {
		s.logger.Error("重建绑定投影失败：读取归档定义文件失败（归档已生效，投影待下次重建）",
			"agent", agent, "archive_id", archiveID, "error", err)
		return
	}
	s.applyProjection(ctx, agent, uid, definition)
}

// applyProjection 用**给定字节**重建投影（归档路径：字节来自归档副本而不是活动区）。
// applyProjection rebuilds from the given bytes (the archive path reads the copy).
func (s *AgentAdmin) applyProjection(ctx context.Context, agent string, uid core.AgentUID,
	definition []byte,
) {
	if s.projection == nil {
		return
	}
	rebuild(ctx, s.projection, agent, uid, definition, s.logger)
}

// clearProjectionIdentity 清掉一个**不再对应任何定义文件**的身份的历史投影行（异名恢复
// 之后残留的归档身份）。投影是派生视图：清理失败只记 ERROR——恢复本身已经生效，行仍可
// 由下一次全量重建之外的显式清理处置；不清则会留下假的"仍被引用"。
// clearProjectionIdentity drops the projection rows of an identity no definition file can
// reach any more; a failure is logged, never turned into a failed restore.
func (s *AgentAdmin) clearProjectionIdentity(ctx context.Context, uid core.AgentUID) {
	if s.projection == nil {
		return
	}
	if err := s.projection.Bindings.Clear(context.WithoutCancel(ctx), uid); err != nil {
		s.logger.Error("清理归档身份的绑定投影失败（恢复已生效，投影待下次重建）",
			"agent_uid", uid, "error", err)
	}
}

// agentResultCode 把写面错误映射到 `management_operations.result_code`（契约错误码表）；
// 未分类的失败一律 INTERNAL。
// agentResultCode maps a write-surface error onto the contract result-code table.
func agentResultCode(err error) string {
	var notFound *core.AgentNotFoundError
	switch {
	case errors.As(err, &notFound) || errors.Is(err, core.ErrResourceNotFound):
		return agentResultNotFound
	case errors.Is(err, core.ErrBusy):
		return agentResultAgentBusy
	case errors.Is(err, core.ErrConflict):
		return agentResultConflict
	case errors.Is(err, core.ErrPreconditionFailed):
		return agentResultPreconditionFailed
	case core.IsInvalidInput(err):
		return agentResultBadRequest
	default:
		return lifecycleResultInternal
	}
}
