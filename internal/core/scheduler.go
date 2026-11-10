package core

// 本文件是调度器的核心业务实现（US-10 起；US-24 把触发从进程内定时器改为数据库
// 到期扫描）。包含：启动对账、到期扫描与领取（§2.3.4 图 4）、唯一执行链
// `runClaimed`、管理面（List / Executions / SetEnabled / RunNow）与有界停机；
// 公开签名取自 docs/us/us24-state-distributed.md §3.1.5。
// This file implements the scheduler core: startup reconciliation, the database due-scan
// and claim path (§2.3.4), the single execution chain, the management surface and
// bounded shutdown. Signatures follow the module card §3.1.5.
//
// 三条纪律（US-24）：
//   - 触发权在数据库：领取是 `(task_id, planned_at)` 唯一约束 + `next_fire_at` 行条件
//     推进 + 执行租约的**单事务**；进程内不再有任务锁 / 代次 / cron 句柄。
//   - 执行权在租约：每条已领取执行由 `KeepLease` 续约，终态写带 token 做 fencing；
//     失权后本进程**不得**再提交任何执行结果。
//   - 停机先停止领取、再排空；排空期间续约不停，超时才取消在途。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 调度器的固定文案与常量（契约字面量，不得改写）。
// Fixed messages and constants of the scheduler (contract literals).
const (
	schedulerBusyMessage     = "同一 Agent 的另一任务仍在执行"
	schedulerArchivedMessage = "会话已归档，本轮跳过"
	schedulerLockMissMessage = "上一轮未结束，本轮跳过"

	schedulerTriggerLog = "调度触发"
	schedulerFailLog    = "调度运行失败"
	schedulerPanicLog   = "调度运行 panic"

	// schedulerMissedOccurrences 是错过触发点的枚举上限（约束 38）。
	schedulerMissedOccurrences = 1000
	// schedulerFinishTimeout 是终态写的清理时限（约束 11 / 图 C）。
	schedulerFinishTimeout = 5 * time.Second
	// schedulerDeliveryTimeout 是系统代投单次发送的时限：运行已结束，发送用独立 ctx，
	// 不继承可能已超时的运行 ctx；超时按「结果未知」记录，不自动重发（R7）。
	schedulerDeliveryTimeout = 15 * time.Second
	// schedulerCloseoutTimeout 覆盖代投 Run 的收尾末笔：投递最多 15s 加一次收尾事务，
	// 超过就放手，引用与租约留给回收器按「已过期」接手（B1）。
	// schedulerCloseoutTimeout bounds the caller-side final closeout step.
	schedulerCloseoutTimeout = 20 * time.Second
	// schedulerStopWait 是宽限耗尽、取消在途运行后的第二段有界等待（约束 23）。
	schedulerStopWait = 5 * time.Second

	// schedulerMissedReason 是「错过宽限」的固定原因字面量（模块卡 §2.3.4）。
	// schedulerMissedReason is the fixed reason literal of the missed-grace branch.
	schedulerMissedReason = "missed_grace_exceeded"

	// 到期扫描的缺省参数（模块卡 §3.3 的缺省值）。装配层按配置注入；零值回落这里，
	// 免得零值变成「每 0 秒轮询」或「一次都不扫」。
	// Defaults for the due scan; the assembly layer injects configured values.
	defaultSchedulerPollInterval  = 500 * time.Millisecond
	defaultSchedulerDispatchBatch = 32
	defaultSchedulerMissedGrace   = 300 * time.Second
	defaultSchedulerRunTimeout    = 300 * time.Second
	defaultSchedulerStopGrace     = 70 * time.Second

	// schedulerNotReadyWarnInterval 是就绪门 WARN 的限速窗口（卡 §2.3.5 步骤 ④：
	// 同一 Agent 的同一原因每 5 分钟最多一条）。
	// schedulerNotReadyWarnInterval throttles the not-ready WARN per agent and reason.
	schedulerNotReadyWarnInterval = 5 * time.Minute
)

// errSchedulerStarted 表示调度器已经成功启动或已被停止，不得再次启动。
// errSchedulerStarted reports a scheduler that was already started or stopped.
var errSchedulerStarted = errors.New("scheduler: already started")

// errSchedulerNoLeases 表示本次装配没有执行权凭证（租约仓储或 Worker 身份缺一），
// 到期扫描与手动补跑都必须 fail-closed：没有执行权就没有可续约、可 fencing 的执行。
// errSchedulerNoLeases is an assembly defect: without a lease store and a worker
// identity the scheduler cannot claim or fence anything.
var errSchedulerNoLeases = errors.New("scheduler: lease store and worker identity are required")

// errSchedulerNoPinnedClaim 表示就绪门给出了期望哈希，而任务仓储没有实现钉住领取
// （`core.AssetPinnedClaimer`）。这是装配缺陷：退回未钉住的领取会绕过 FR-25C-08 的
// 版本绑定，因此本拍不领取（零写入）并报错，由装配修复而不是降级。
// errSchedulerNoPinnedClaim is an assembly defect: a ready hash without a pinned-claim
// store must never fall back to the unpinned claim.
var errSchedulerNoPinnedClaim = errors.New(
	"scheduler: readiness provides a bundle hash but the task store has no pinned claim")

// errSchedulerNoDefinitionAuthority 表示本进程没有共享任务定义列的写权（Worker 角色），
// 运行期定义协调与退休必须失败关闭：悄悄跳过会让装配缺陷不可见，照写会让旧 Worker
// 把控制面已发布的定义盖回本地种子（US-25C FR-25C-13 / C18）。
// errSchedulerNoDefinitionAuthority is the worker-side fail-closed answer to any
// runtime shared-definition write.
var errSchedulerNoDefinitionAuthority = errors.New(
	"定义写权缺失：本进程无权写共享任务定义列，定义变更须由控制面发布")

// RunCloseout 是系统代投 Run 的收尾末笔端口（模块卡 §2.3.5 五步收尾 / P-01）：
// 投递结算发生在 Run 返回之后，所以第 4、5 步由调度器代做——先按 run_id 取回本次
// Run 的租约凭证（Run 返回时服务侧 keeper 只停不释放，此刻它仍由本进程合法持有），
// 结算完成后补 `CompleteRunCloseout`，租约在最后一个事务里交还。
// 接口定义在消费方（调度器）：只需要这两个方法，不把整个 RunStore 拖进调度装配。
// RunCloseout is the caller-side final step for system-delivered runs: look the lease
// token up and complete the closeout after the delivery has been settled.
type RunCloseout interface {
	LookupAdmittedRun(ctx context.Context, runID string) (LeaseToken, error)
	CompleteRunCloseout(ctx context.Context, runID string, token LeaseToken, now time.Time) error
}

// schedulerOwnerUnauthorized 是任务执行授权不可用时的**固定**失败原因（C-32）：
// 执行行的 error_message 与 Run 的 Reason 都写它——细节原因码（owner_reason）留给
// 审计字段，不改写这条对外的稳定字面量。
// schedulerOwnerUnauthorized is the fixed failure reason for an unusable execution
// authorization; the detailed reason code belongs to the audit fields.
const schedulerOwnerUnauthorized = "owner_unauthorized"

// schedulerAdminAuthUnavailable 是 admin-builtin 任务在管理认证姿态为 `unavailable`
// 时的**固定**失败原因（§2.14 判定表）。它与 owner_unauthorized 分开：前者是"这台
// 实例根本没有管理认证能力"，后者是"原授权已经失效"——两者的处置不同。
// schedulerAdminAuthUnavailable is the fixed failure reason for an admin-builtin task
// when the startup admin posture is unavailable.
const schedulerAdminAuthUnavailable = "admin_auth_unavailable"

// ScheduleGate 判定某个 Agent 当前是否允许被**定时**触发（14D / E5「启用才运行」）。
// 返回 false 表示本轮不排期：不写执行行、不预写投递、不记撞锁、不发起运行。
// 它只拦 cron 触发；管理面手动补跑不受影响——补跑是显式请求，准入失败按既有语义记
// 一条 failed 执行行（US-10 / US-11 口径保持不变）。
// ScheduleGate decides whether an agent may be triggered by cron right now. A false
// answer skips the tick entirely; manual RunNow deliberately bypasses it.
type ScheduleGate func(ctx context.Context, agent string) bool

// OwnerKeyVerifier 复核后台执行的原授权 Key 是否仍然成立（C-32 / 设计卡 §2.14）。
// 实现必须按 `(caller_id, key_id)` 读**那一把** Key 的现值：状态、过期时间、当前
// scope 与调用方状态；返回该 Key 当前的 scope 供 `OwnedPrincipal` 构造。失效时返回
// 带 `OwnerReason` 的 `*PrincipalError`——**不得**退回空 scope 当"没有权限"，
// 也**不得**换成同一调用方的其他 Key。
// OwnerKeyVerifier re-reads the exact authorizing key bound to a background run.
type OwnerKeyVerifier interface {
	VerifyOwnerKey(ctx context.Context, callerID, keyID, agent string) ([]string, error)
}

// SchedulerOptions 是调度器的全部装配输入（§3.1.2 逐字，US-24 追加执行权装配）。
// SchedulerOptions is the full assembly input of the scheduler.
type SchedulerOptions struct {
	Store   TaskStore
	Invoker RunInvoker
	Engine  CronEngine
	Entries []AgentEntry
	// OwnerKeys 是 caller-key 任务执行前的 Key 复核端口（C-32）。为 nil 时该来源一律
	// fail-closed——无法证明授权仍在就不得出网。
	// A nil OwnerKeys fails closed for caller-key tasks.
	OwnerKeys OwnerKeyVerifier
	// AdminPosture 迟绑定地读取启动期算出的管理认证姿态（§2.14）。web 面在监听成功后
	// 才发布姿态，而调度器先于此构造，所以这里收的是取值函数而不是值；为 nil 或返回
	// 未知值时按"尚未计算"处理，**只有** `unavailable` 拒绝 admin-builtin。
	// AdminPosture is read late because the web face publishes it after the scheduler is
	// built; only the unavailable posture rejects admin-builtin tasks.
	AdminPosture func() AdminPosture
	// ScheduleGate 为 nil 时不设门（全部按任务定义的 enabled 列判定）；非 nil 时每轮
	// cron 触发先问一次，false 即跳过。门在**每次触发**求值、不缓存，启停下一次触发
	// 即生效。
	// A nil gate disables the check; a non-nil gate is evaluated per tick, uncached.
	ScheduleGate ScheduleGate
	CronSpec     func(Schedule) (string, error) // workspace.CronSpec；唯一 CRON_TZ= 前缀出口
	// Delivery 是系统代投端口（R7）；任务配置了 deliver_to 而这里为 nil 时，
	// 该任务不发起运行并如实记失败——**不得**降级为"模型自己发通知"。
	Delivery ScheduleDelivery
	// Closeout 是系统代投 Run 的收尾末笔（五步收尾的第 5 步）。同一条件下
	// `SuppressNotify` 为真、服务侧跳过收尾，交还给调用方——所以这里缺端口时
	// 投递照常结算，但**不**假装收尾：租约过期后由回收器按终态残留清理。
	// A missing port never fakes the closeout; the reclaimer picks the residue up.
	Closeout   RunCloseout
	Budget     Budget
	RunTimeout time.Duration // 300s（§3.1.8）
	StopGrace  time.Duration // 70s（§3.1.8）
	Logger     *slog.Logger
	Now        func() time.Time

	// 以下四项是 US-24 的执行权装配（§3.1.5）。Leases 提供续约与释放，Worker 是
	// 领取事务写入的归属身份，LeaseTTL / RenewInterval 决定执行租约的生命期与续约节奏。
	// Lease carrier wiring: the lease store, the claiming identity and its cadence.
	Leases        LeaseStore
	Worker        WorkerIdentity
	LeaseTTL      time.Duration
	RenewInterval time.Duration

	// 以下三项是到期扫描的节奏（§3.3）：轮询间隔、单轮领取上限、错失宽限。
	// Cadence of the due scan: poll interval, per-pass batch and the missed grace.
	PollInterval  time.Duration
	DispatchBatch int
	MissedGrace   time.Duration

	// AssetReady 是到期触发的就绪门（US-25C FR-25C-11）：nil = 不设门（测试与轻装配）；
	// 非 nil 时每次领取前先问一次该 Agent 是否已就绪，未就绪**不领取、不推进
	// `next_fire_at`、不写执行行**。
	// AssetReady is the per-tick readiness gate; nil disables it.
	AssetReady func(ctx context.Context, agent string) error

	// AssetReadyHash 是就绪门的**带哈希**形态（US-25C ADJ-1）：非 nil 时以它为准，
	// 一次读到「是否就绪 + 就绪快照里的已安装哈希」，门与领取钉住因此来自同一份快照；
	// 返回 error = 未就绪（原因进限速 WARN）。nil 时退回 AssetReady 作门，领取走未钉住
	// 的 `ClaimDueTrigger`。
	// AssetReadyHash carries the ready hash so the gate and the claim pin share one
	// snapshot; nil falls back to AssetReady plus the unpinned claim.
	AssetReadyHash func(ctx context.Context, agent string) (string, error)

	// DefinitionAuthority 表示本进程是否有权写共享任务定义列（US-25C §2.3.1）：
	// 仅 control 为 true；为 false（Worker）时 `SetRegistered` / `ReconcileAgent`
	// 必须 fail-closed，只保留执行协调写。
	// DefinitionAuthority is true only on the control role; workers fail closed.
	DefinitionAuthority bool
}

// Scheduler 是定时任务调度器。任务运行期状态一律读库，进程内只保留**注册事实**
// （registry / definitions 只读快照）与到期扫描的骨架。
// Scheduler keeps only registration facts in memory; runtime task state is always read
// from the store.
type Scheduler struct {
	store        TaskStore
	invoker      RunInvoker
	engine       CronEngine
	scheduleGate ScheduleGate
	cronSpec     func(Schedule) (string, error)
	ownerKeys    OwnerKeyVerifier
	adminPosture func() AdminPosture
	delivery     ScheduleDelivery
	closeout     RunCloseout
	budget       Budget
	runTimeout   time.Duration
	stopGrace    time.Duration
	logger       *slog.Logger
	now          func() time.Time

	// 执行权装配（US-24）。
	leases        LeaseStore
	worker        WorkerIdentity
	leaseTTL      time.Duration
	renewInterval time.Duration

	// 到期扫描的节奏（US-24）。
	pollInterval  time.Duration
	dispatchBatch int
	missedGrace   time.Duration

	// US-25C：就绪门与定义权（装配期固化）。
	assetReady          func(ctx context.Context, agent string) error
	assetReadyHash      func(ctx context.Context, agent string) (string, error)
	definitionAuthority bool
	// scanCursor 是到期扫描的两段键集游标（US-25C §2.3.5）：stateMu 保护，锁内零 I/O。
	// **只有 Start 清空**——SetRegistered / ReplaceEntries 只换内存集合，不得复位。
	// scanCursor is cleared only by Start; registry refreshes never reset it.
	scanCursor DueScanCursor
	// gateWarnMu / gateWarnAt 是就绪门 WARN 的限速表：键为 Agent + 原因，同一原因
	// 每 5 分钟最多一条（卡 §2.3.5 步骤 ④）。未就绪的行每拍都会命中门，不限速会写满日志。
	// gateWarnMu throttles the not-ready WARN per agent and reason.
	gateWarnMu sync.Mutex
	gateWarnAt map[string]time.Time

	// 构造期固化的只读快照（P-05）：registry 只当 Agent 注册表，definitions 仅供
	// Start 对账；二者都不得回写，也不缓存运行期任务状态（约束 3）。
	// ReplaceEntries 在运行期整体替换这两个集合（US-25C C18）。
	registry    map[string]bool
	definitions map[string][]Schedule

	// stateMu 保护以下字段：启动 / 停止位、运行期 ctx 与注册集合的增删。
	// The state mutex guards the lifecycle flags, the runtime contexts and registry edits.
	stateMu        sync.RWMutex
	started        bool
	stopped        bool
	baseCtx        context.Context
	cancelBase     context.CancelFunc
	dispatchCtx    context.Context
	cancelDispatch context.CancelFunc
	dispatchDone   chan struct{}
	// executionWG 是本进程已领取执行（含 Run 与收尾）的等待组：停机时先停领取，
	// 再按宽限等待它自然归零（§2.3.11 [4]）。
	executionWG sync.WaitGroup

	stopOnce sync.Once
	// claimsOnce 独立于 stopOnce：StopClaims 可以先于 Stop 单独执行（图 11 [2] 与
	// [4] 之间有排空窗口），两者都必须幂等且不重复等待领取循环。
	// claimsOnce is separate so StopClaims can run first; both entry points stay idempotent.
	claimsOnce sync.Once
	done       chan struct{}
}

// NewScheduler 构造调度器：拷贝 Entries 为只读快照并补齐到期扫描的缺省节奏。
// NewScheduler copies Entries into read-only snapshots and fills the due-scan defaults.
func NewScheduler(opts SchedulerOptions) *Scheduler {
	if opts.Now == nil {
		opts.Now = time.Now // 唯一的时钟缺省点（约束 37）
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultSchedulerPollInterval
	}
	if opts.DispatchBatch <= 0 {
		opts.DispatchBatch = defaultSchedulerDispatchBatch
	}
	if opts.MissedGrace < 0 {
		opts.MissedGrace = 0
	}
	if opts.RunTimeout <= 0 {
		opts.RunTimeout = defaultSchedulerRunTimeout
	}
	if opts.StopGrace <= 0 {
		opts.StopGrace = defaultSchedulerStopGrace
	}
	registry, definitions := deriveEntries(opts.Entries)
	return &Scheduler{
		store:         opts.Store,
		invoker:       opts.Invoker,
		engine:        opts.Engine,
		scheduleGate:  opts.ScheduleGate,
		cronSpec:      opts.CronSpec,
		ownerKeys:     opts.OwnerKeys,
		adminPosture:  opts.AdminPosture,
		delivery:      opts.Delivery,
		closeout:      opts.Closeout,
		budget:        opts.Budget,
		runTimeout:    opts.RunTimeout,
		stopGrace:     opts.StopGrace,
		logger:        opts.Logger,
		now:           opts.Now,
		leases:        opts.Leases,
		worker:        opts.Worker,
		leaseTTL:      opts.LeaseTTL,
		renewInterval: opts.RenewInterval,
		pollInterval:  opts.PollInterval,
		dispatchBatch: opts.DispatchBatch,
		missedGrace:   opts.MissedGrace,

		assetReady:          opts.AssetReady,
		assetReadyHash:      opts.AssetReadyHash,
		definitionAuthority: opts.DefinitionAuthority,

		registry:    registry,
		definitions: definitions,
		done:        make(chan struct{}),
	}
}

// deriveEntries 把装载结果派生成调度器的两个只读集合：注册表只收 `Registered`，
// 定义按 Agent 名保存一份 `Profile.Schedules` 的副本（nil Profile = 空定义）。
// NewScheduler 与 ReplaceEntries **共用**本函数，派生规则因此逐字相同（US-25C C18）。
// deriveEntries is the single derivation shared by construction and runtime replacement.
func deriveEntries(entries []AgentEntry) (map[string]bool, map[string][]Schedule) {
	registry := make(map[string]bool, len(entries))
	definitions := make(map[string][]Schedule, len(entries))
	for _, entry := range entries {
		if entry.Registered {
			registry[entry.Name] = true
		}
		var schedules []Schedule
		if entry.Profile != nil {
			schedules = append(schedules, entry.Profile.Schedules...)
		}
		definitions[entry.Name] = schedules
	}
	return registry, definitions
}

// ReplaceEntries 以纯内存方式整体替换调度器的注册集合与定义快照（US-25C C18 /
// §2.3.4）：派生规则与 NewScheduler 逐字相同；`stateMu` 内整体替换、**锁内零 I/O**；
// Start 前后语义相同。它**不写库**、**不退休**共享库里未出现的 Agent（只从本进程
// 内存集合移除），也**不得**复位或清空扫描游标与本圈边界——扫描进度只由 Start 清空。
// ReplaceEntries swaps both in-memory sets without any I/O, never retires absent agents,
// and never touches the scan cursor.
func (s *Scheduler) ReplaceEntries(entries []AgentEntry) error {
	registry, definitions := deriveEntries(entries)
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.registry = registry
	s.definitions = definitions
	return nil
}

// leaseless 判定本次装配是否缺少执行权：租约仓储或 Worker 身份任一缺失即不可调度。
// leaseless reports an assembly without the ownership wiring.
func (s *Scheduler) leaseless() bool {
	return s.leases == nil || s.worker.IsZero() || s.leaseTTL <= 0 || s.renewInterval <= 0
}

// Start 执行图 B 的 US-24 形态：读库 → 对账（只写库）→ 错过告警 → 启动到期扫描。
// 启动**不再**做全局恢复：遗留执行行由回收器按过期 / 已释放租约领取（FR-24-02）。
// 只有**成功**过一次的 Start 会拒绝重入；失败不置启动位，可重试（约束 21 / 图 B ①）。
// Start runs the startup pipeline; a failed attempt does not mark the scheduler started,
// so a retry is allowed. There is deliberately no global recovery scan here.
func (s *Scheduler) Start(ctx context.Context) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if s.started || s.stopped {
		return errSchedulerStarted
	}
	if s.leaseless() {
		s.logger.Error("调度器缺少执行权装配，拒绝启动")
		return errSchedulerNoLeases
	}
	now := s.now()

	// ③ 读库。
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("启动读取任务失败", "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}

	// ④ 对账：逐已注册 Agent 单事务提交；未注册项不遍历（坏文件不写库）。
	// 本阶段**只提交**，不注册任何句柄——句柄已经不存在，到期扫描直接读库。
	// 定义写权缺失（Worker）时退化为**只读对账**：不提交任何定义行，只在下面按库行
	// 告警；协调写（领取 / 推进 / 执行行）不受影响（US-25C §2.3.1 / C18）。
	// A worker reconciles read-only: no definition-row commit, warnings only.
	if s.definitionAuthority {
		for _, agent := range s.registeredAgentNamesLocked() {
			fresh, _, err := s.commitAgentLocked(ctx, agent, s.definitions[agent], rows)
			if err != nil {
				s.logger.Error("启动对账失败", "agent", agent, "error", err)
				return fmt.Errorf("scheduler: reconcile agent %q: %w", agent, err)
			}
			rows = fresh
		}
	} else if owners := s.registeredAgentNamesLocked(); len(owners) > 0 {
		// 只读对账的可见性：本进程注册了定义，但共享行归控制面写——点名说明为何
		// 启动时没有任何 `ApplyAgentTasks`，避免运维把"没写"当成对账失败。
		s.logger.Info("definition authority absent; startup reconcile is read-only",
			"agents", len(owners))
	}

	// ⑥ 库里有、本进程工作区没有的 Agent：**不**退休，只报告（F7）。
	// 多 Worker 共用一张任务表时，另一个工作区的 Agent 对本进程同样是"缺席"；按缺席
	// 退休会静默停掉别的 Worker 的定时任务。可证明的移除只有运行期 SetRegistered(false)。
	s.warnForeignAgentsLocked(rows)

	// ⑦ 错过触发点只 WARN，不补跑（约束 18）。真正的错失落库在到期扫描里。
	s.warnMissedTriggersLocked(rows, now)

	// ⑧ 运行期 ctx：baseCtx 是每条执行与其续约的父 ctx；dispatchCtx 只包住到期扫描。
	// 停止时先取消 dispatchCtx（不再领取），在途执行继续用 baseCtx 跑完宽限。
	// baseCtx parents every execution and its keeper; dispatchCtx only wraps the scan,
	// so cancelling it stops claiming without killing in-flight runs.
	s.baseCtx, s.cancelBase = context.WithCancel(context.WithoutCancel(ctx))
	s.dispatchCtx, s.cancelDispatch = context.WithCancel(s.baseCtx)
	s.dispatchDone = make(chan struct{})
	// 扫描游标与冻结边界**只在这里**清空（US-25C §2.3.5）：运行期的集合替换不得把
	// 长扫描拉回第一页。
	s.scanCursor = DueScanCursor{}
	s.started = true

	go s.dispatchLoop(s.dispatchCtx)

	// ⑩ 信号 watcher：ctx 取消 → Stop 恰一次；Stop 完成后退出（无 goroutine 泄漏）。
	go s.watchStartContext(ctx)
	return nil
}

// Stop 执行图 F 的 US-24 形态：停止领取 → 有界 drain（期间续约不停）→ 宽限耗尽
// 才取消在途；幂等且并发安全。
// Stop stops claiming, drains within the grace budget while renewals keep running, and
// cancels in-flight work only after the grace expires. It is idempotent.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(s.stop)
}

// StopClaims 只停领取（US-24 §2.3.11 [2]）：取消到期扫描并等它退出，**不**等待
// 在途执行。调用方因此可以在 Drain 期间继续持有续约（各执行自己的 keeper），
// 再由 Stop 完成有界排空与取消后的 join。
//
// 幂等，且与 Stop 可任意顺序组合：先 StopClaims 再 Stop 时，Stop 复用同一次
// 领取停止（claimsOnce），不会重复等待；只调 Stop 时行为与从前逐字一致。
// StopClaims stops only the claim loop (figure 11 [2]); it never waits for in-flight
// executions, so renewals keep running during the drain. It is idempotent and composes
// with Stop in either order.
func (s *Scheduler) StopClaims() {
	s.claimsOnce.Do(s.stopClaims)
}

// CancelExecutions 取消全部在途执行，**不等待**它们退出（图 11 [4] 的"超时 →
// cancel()"）。它只负责发取消：join 仍由 Stop 在剩余预算内完成，终态写走各执行
// 自己的 WithoutCancel + 短超时。幂等；未 Start 时为空操作。
//
// 与 Stop 的分工：Stop 内部也有一条宽限计时，但那只是**兜底**——生产停机路径
// 由编排层在 drain 预算到点时显式调用本方法，取消时刻因此可判、可测，并且落在
// flows.shutdown_grace 而不是某个包级常量上。
// CancelExecutions cancels in-flight executions without waiting (figure 11 [4]); the
// join stays with Stop under the remaining budget. It is idempotent.
func (s *Scheduler) CancelExecutions() {
	s.stateMu.Lock()
	cancelBase := s.cancelBase
	s.stateMu.Unlock()
	if cancelBase != nil {
		cancelBase()
	}
}

// stopClaims 是领取停止的一次性实体。
// stopClaims is the once-only body of StopClaims.
func (s *Scheduler) stopClaims() {
	s.stateMu.Lock()
	s.stopped = true
	wasStarted := s.started
	cancelDispatch := s.cancelDispatch
	dispatchDone := s.dispatchDone
	s.stateMu.Unlock()
	if !wasStarted {
		return
	}
	// [2] 停止领取：取消到期扫描并等它退出。此后不会再有新的执行进入等待组。
	if cancelDispatch != nil {
		cancelDispatch()
	}
	if dispatchDone != nil {
		<-dispatchDone
	}
}

// stop 是 Stop 的一次性实体。
// stop is the once-only body of Stop.
func (s *Scheduler) stop() {
	s.StopClaims()
	s.stateMu.Lock()
	wasStarted := s.started
	cancelBase := s.cancelBase
	s.stateMu.Unlock()

	defer close(s.done)
	if !wasStarted {
		return
	}

	// [3] + [4] 有界排空：本阶段各执行的续约循环仍在运行——排空期间租约过期被他人
	// 接管正是要避免的缺陷（C-24）。
	drained := make(chan struct{})
	go func() {
		s.executionWG.Wait()
		close(drained)
	}()
	graceTimer := time.NewTimer(s.stopGrace)
	defer graceTimer.Stop()
	select {
	case <-drained:
		return
	case <-graceTimer.C:
	}

	// [5] 宽限耗尽：取消全部在途执行（其续约随执行 ctx 结束而停止）。
	if cancelBase != nil {
		cancelBase()
	}
	waitTimer := time.NewTimer(schedulerStopWait)
	defer waitTimer.Stop()
	select {
	case <-drained:
		return
	case <-waitTimer.C:
		// 安全优先于退出速度：记 ERROR 后继续等待（约束 23）。
		s.logger.Error("调度器停机超时，继续等待在途运行")
		<-drained
	}
}

// watchStartContext 监听启动 ctx 的取消；Stop 完成后退出。
// watchStartContext stops the scheduler when the start context is cancelled and exits
// once Stop completes.
func (s *Scheduler) watchStartContext(ctx context.Context) {
	select {
	case <-ctx.Done():
		s.Stop()
	case <-s.done:
	}
}

// dispatchLoop 按 poll_interval 驱动到期扫描，直到 dispatchCtx 被取消。
// dispatchLoop drives the due scan every poll interval until the dispatch context ends.
func (s *Scheduler) dispatchLoop(ctx context.Context) {
	defer close(s.dispatchDone)
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		if err := s.dispatchOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Error("调度到期扫描失败", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// dispatchOnce 完成一轮到期扫描与领取（模块卡 §2.3.4 图 4 / 伪代码）：
// 初始化 → 错失 → 生命周期门 → 领取。单任务失败只记日志、不中断本轮。
// dispatchOnce runs one due-scan pass: initialize, missed, lifecycle gate, claim.
func (s *Scheduler) dispatchOnce(ctx context.Context) error {
	// 本轮所有到期 / 宽限 / 下一次触发判定共用**数据库时钟**（§2.3.4 的 db_now）：
	// 线程内一致，且与领取事务里的 CAS 基准同源。读不到就放弃本轮（扫描下一拍重试），
	// 不用进程时钟顶替——时钟偏差正是这里要消除的变量（F8）。开圈那一拍的 dbNow
	// 直接取冻结边界里的数据库时间（同一次读）。
	// One database-clock reading per pass; a failure skips the pass rather than falling
	// back to a process clock that could disagree with the claim transaction.
	tasks, now, cursor, err := s.duePage(ctx)
	if err != nil {
		return err
	}
	registered := s.registeredSnapshot()
	complete := true
	for _, due := range tasks {
		// 停止领取：扫描 ctx 一取消就不再处理本轮剩余项（§2.3.11 [1]）。本页没走完
		// 就**不推进**扫描游标——下次重扫整页（领取有 CAS，不会重复）。
		if ctx.Err() != nil {
			complete = false
			break
		}
		// 防御性复查：仓储已按 enabled / retired 过滤，这里再判一次，避免可见性
		// 判据分叉（管理读面与调度路径共用同一规则）。
		if !due.Task.Enabled || !due.Task.RetiredAt.IsZero() {
			continue
		}
		// 本进程不含该 Agent 的工作区：不领取，next_fire_at 留给持有方（FR-24-25）。
		if !registered[due.Task.Agent] {
			continue
		}

		// 下一次触发从 max(next_fire_at, db_now) 起算（图 4）：既跳过停机窗口内的
		// 全部中间点（不补跑），也保证 next 严格大于 planned，CAS 推进必然前进。
		from := due.NextFireAt
		if from.IsZero() || from.Before(now) {
			from = now
		}
		next, spec, err := s.nextFire(due.Task, from)
		if err != nil {
			s.logger.Warn("schedule skipped: invalid cron",
				append(s.runAttrs(due.Task, "", TriggerCron), "error", err)...)
			continue
		}

		if due.NextFireAt.IsZero() {
			// 迁移后的遗留行 / 新建行：只初始化，不执行（图 4 第一个分支）。初始化是
			// **行条件写**：扫描到 NULL 只是快照，不是写权。若在读取与写入之间外部
			// （管理面重写 / 测试夹具武装）已把 next_fire_at 改成非 NULL，本次初始化
			// 必须放弃——ErrStaleNextFire 按既有口径无声跳过（与下方推进分支一致），
			// 否则会把外部刚写下的触发覆盖成 cron 计算的未来值，触发被静默吞掉。
			// The NULL-initialization is a conditional write; losing the race to an
			// external writer abandons this round with ErrStaleNextFire, silently.
			if err := s.store.SetNextFire(ctx, due.Task.TaskID, nil, next); err != nil &&
				!errors.Is(err, ErrStaleNextFire) {
				s.logger.Error("initialize next fire",
					append(s.runAttrs(due.Task, "", TriggerCron), "error", err)...)
			}
			continue
		}

		// 14D / E5「启用才运行」：生命周期门在**任何执行落库之前**求值——关闭只能拦住
		// 启动、拦不住真实执行时，观察窗口里会出现执行行与失败 Run。本轮不排期只推进
		// next_fire_at（不写执行行、不预写投递、不记撞锁、不发起运行），既保持 US-10
		// 语义，也让该任务离开到期集合、不占住单轮领取名额。
		// The lifecycle gate is evaluated before any execution write; a skipped tick
		// only advances next_fire_at so the task stops occupying the dispatch batch.
		if s.scheduleGate != nil && !s.scheduleGate(ctx, due.Task.Agent) {
			s.logger.Debug("Agent 未启用，本轮不排期", "task_id", due.Task.TaskID, "agent", due.Task.Agent)
			planned := due.NextFireAt
			if err := s.store.SetNextFire(ctx, due.Task.TaskID, &planned, next); err != nil &&
				!errors.Is(err, ErrStaleNextFire) {
				s.logger.Error("advance gated trigger",
					append(s.runAttrs(due.Task, "", TriggerCron), "error", err)...)
			}
			continue
		}

		if now.Sub(due.NextFireAt) > s.missedGrace {
			// 错过宽限：只记一条 skipped 并推进到未来，不逐点补跑（FR-24-12）。
			if err := s.store.RecordMissedTrigger(ctx, due.Task.TaskID, due.NextFireAt, next, 1,
				schedulerMissedReason); err != nil {
				s.logger.Error("record missed trigger",
					append(s.runAttrs(due.Task, "", TriggerCron), "error", err)...)
			}
			continue
		}

		// ④ 就绪门（US-25C FR-25C-11 / 卡 §2.3.5）：本进程还没有该 Agent 的**已安装**
		// 版本时不领取、不推进 `next_fire_at`、不写执行行——本次触发原样留在原地，
		// 下一拍重新求值。门在 ③ 之后：越过宽限的未就绪行必须**如实记 missed**，而不是
		// 被门悄悄跳过（那是"永不消失"的假条）。
		// The readiness gate runs after the missed branch: an overdue unready row still
		// records its missed skip instead of silently holding the trigger forever.
		expectHash, readyErr := s.readyHash(ctx, due.Task.Agent)
		if readyErr != nil {
			s.warnNotReady(due.Task, readyErr)
			continue
		}

		claimed, claimErr := s.claimDue(ctx, due, next, expectHash)
		switch {
		case errors.Is(claimErr, ErrTriggerAlreadyClaimed), errors.Is(claimErr, ErrStaleNextFire):
			// 另一 Worker 已领取 / 已推进：本轮无声跳过（FR-24-11）。
			continue
		case errors.Is(claimErr, ErrAssetNotReady), errors.Is(claimErr, ErrAssetMoved):
			// 指针缺失 / 扫描后、领取前被发布抢先：**零写入**结局，行留在到期集合，
			// 再次被考察时从 ④ 重新求值。不得引入"暂存触发、稍后补发"的第二套待办
			// （卡 §2.3.5「不得用重放补救」）。
			s.logger.Debug("pinned claim refused, trigger left in place",
				append(s.runAttrs(due.Task, "", TriggerCron), "reason", claimErr.Error())...)
			continue
		case errors.Is(claimErr, ErrTaskDisabled):
			// 扫描之后被停用：本轮无声跳过，不写执行行、不推进。重新启用后若
			// next_fire_at 仍在过去，按既有的错失宽限 / 领取路径处理。
			continue
		case errors.Is(claimErr, ErrTaskBusy):
			// 忙路径必须与错失路径一样在同一事务里推进 next_fire_at：该 planned_at
			// 一旦落下 skipped 行，后续领取会永远撞 (task_id, planned_at) 唯一约束。
			if err := s.store.RecordBusyTrigger(ctx, due.Task.TaskID, due.NextFireAt, next); err != nil {
				s.logger.Error("record busy trigger",
					append(s.runAttrs(due.Task, "", TriggerCron), "error", err)...)
				continue
			}
			s.logger.Warn(schedulerLockMissMessage, s.runAttrs(due.Task, "", TriggerCron)...)
			continue
		case claimErr != nil:
			s.logger.Error("claim due trigger",
				append(s.runAttrs(due.Task, "", TriggerCron), "error", claimErr)...)
			continue
		}

		// 领取成功：执行跑在 baseCtx 上——停止领取不打断在途执行，宽限耗尽才取消。
		s.executionWG.Go(func() {
			_ = s.executeClaimed(s.executionContext(), claimed, spec)
		})
	}
	if complete {
		// 前进不看领取结果：④ 未就绪、⑤ 零写入拒绝、③ missed 都照常前进（卡 §2.3.5）。
		s.commitScanCursor(cursor, tasks)
	}
	return nil
}

// duePage 取一拍到期扫描的一页，并在需要时开新圈（US-25C §2.3.5）：
//   - 存储实现 `AgentDueLister` 时按两段键集游标取一页；`Sweep == nil` 表示本拍开圈，
//     先以**一次**只读查询冻结边界（数据库时间 + NULL 段上界），并用同一次读到的
//     数据库时间充当本拍 dbNow；此后各拍照常各读一次 dbNow。
//   - 未实现扩展（轻装配 / 零值单测）时退回全局批次 + 装配时钟，语义与从前一致。
//
// 边界读不到就放弃本拍（**不推进**游标），下一拍重试；不得用进程时钟顶替。
// duePage returns one page of the due scan and opens a new sweep when needed; a failed
// bounds read abandons the pass without advancing the cursor.
func (s *Scheduler) duePage(ctx context.Context) ([]DueTask, time.Time, DueScanCursor, error) {
	lister, ok := s.store.(AgentDueLister)
	if !ok {
		tasks, err := s.store.ListDispatchable(ctx, s.dispatchBatch)
		if err != nil {
			return nil, time.Time{}, DueScanCursor{}, fmt.Errorf("scheduler: list dispatchable: %w", err)
		}
		now, err := s.dbNow(ctx)
		if err != nil {
			return nil, time.Time{}, DueScanCursor{}, fmt.Errorf("scheduler: db now: %w", err)
		}
		return tasks, now, DueScanCursor{}, nil
	}
	// 扫描不持 stateMu：先取注册集合快照，再在快照上排序——直接遍历 s.registry 会与
	// SetRegistered 的并发增删构成数据竞争（归档屏障在派发循环运行时就是这条路径）。
	// The scan holds no lock: copy the registry first, then sort the copy.
	agents := sortedRegisteredNames(s.registeredSnapshot())
	if len(agents) == 0 {
		return nil, time.Time{}, DueScanCursor{}, nil
	}
	cursor := s.scanCursorSnapshot()
	var now time.Time
	if cursor.Sweep == nil {
		bounds, err := lister.ReadSweepBounds(ctx, agents)
		if err != nil {
			return nil, time.Time{}, DueScanCursor{}, fmt.Errorf("scheduler: read sweep bounds: %w", err)
		}
		cursor = DueScanCursor{Sweep: &bounds, InNullSegment: true}
		now = bounds.Cutoff
	} else {
		dbNow, err := s.dbNow(ctx)
		if err != nil {
			return nil, time.Time{}, DueScanCursor{}, fmt.Errorf("scheduler: db now: %w", err)
		}
		now = dbNow
	}
	tasks, err := lister.ListDispatchableForAgents(ctx, agents, &cursor, s.dispatchBatch)
	if err != nil {
		return nil, time.Time{}, DueScanCursor{}, fmt.Errorf("scheduler: list dispatchable: %w", err)
	}
	return tasks, now, cursor, nil
}

// scanCursorSnapshot 复制当前扫描游标（stateMu 内取，锁内零 I/O）。
// scanCursorSnapshot copies the cursor under the state mutex.
func (s *Scheduler) scanCursorSnapshot() DueScanCursor {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.scanCursor
}

// commitScanCursor 按本页末行推进两段游标：整页取自 NULL 段时按 `task_id` 前进（整页
// NULL 也前进，不得复位），取自非 NULL 段时按 `(next_fire_at, task_id)` 前进。
//
// 短页是**段尾信号**——它表示本圈边界内的行已经走完，而不是"此刻恰好没有到期触发"：
// 持续到期时后者永远不成立（卡 §2.3.5）。因此短页（含空页）即结束本圈、清空边界；
// 下一拍开新圈并重新冻结 cutoff 与 NULL 上界。
// commitScanCursor advances the two-segment cursor; a short page ends the sweep.
func (s *Scheduler) commitScanCursor(cursor DueScanCursor, page []DueTask) {
	if cursor.Sweep == nil {
		return
	}
	if len(page) < s.dispatchBatch {
		s.setScanCursor(DueScanCursor{})
		return
	}
	last := page[len(page)-1]
	next := DueScanCursor{Sweep: cursor.Sweep}
	if last.NextFireAt.IsZero() {
		next.InNullSegment = true
		next.NullAfterTaskID = last.Task.TaskID
	} else {
		at := last.NextFireAt
		next.InNullSegment = false
		next.AfterNextFireAt = &at
		next.AfterTaskID = last.Task.TaskID
	}
	s.setScanCursor(next)
}

// setScanCursor 写回扫描游标（stateMu 内，锁内零 I/O）。
// setScanCursor stores the cursor under the state mutex.
func (s *Scheduler) setScanCursor(cursor DueScanCursor) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.scanCursor = cursor
}

// readyHash 求值领取前的就绪门（卡 §2.3.5 步骤 ④）：返回本次领取可用的期望哈希，
// error 表示未就绪。带哈希的端口优先——门与领取哈希来自同一份就绪快照，不会在两次
// 读之间撕裂；两个端口都没接线时视为不设门（轻装配）。
// readyHash evaluates the readiness gate and returns the claim's expected hash.
func (s *Scheduler) readyHash(ctx context.Context, agent string) (string, error) {
	if s.assetReadyHash != nil {
		return s.assetReadyHash(ctx, agent)
	}
	if s.assetReady != nil {
		return "", s.assetReady(ctx, agent)
	}
	return "", nil
}

// warnNotReady 按 `(Agent, 原因)` 限速地记一条 WARN：未就绪的行每拍都会命中 ④，
// 不限速会把日志写满（卡 §2.3.5：同一原因每 5 分钟最多一条）。
// warnNotReady throttles one WARN per agent and reason every five minutes.
func (s *Scheduler) warnNotReady(task TaskRecord, reason error) {
	key := task.Agent + "\x00" + reason.Error()
	now := s.now()
	s.gateWarnMu.Lock()
	if s.gateWarnAt == nil {
		s.gateWarnAt = make(map[string]time.Time)
	}
	if last, ok := s.gateWarnAt[key]; ok && now.Sub(last) < schedulerNotReadyWarnInterval {
		s.gateWarnMu.Unlock()
		return
	}
	s.gateWarnAt[key] = now
	s.gateWarnMu.Unlock()
	s.logger.Warn("agent not ready, trigger left in place",
		append(s.runAttrs(task, "", TriggerCron), "reason", reason.Error())...)
}

// claimDue 领取一次计划触发：有期望哈希时走钉住版本（`AssetPinnedClaimer`），
// 否则走既有未钉住的领取。哈希来源已就绪而存储不支持钉住属于装配缺陷——**不得**
// 退回未钉住的领取，那等于把 FR-25C-08 的版本绑定整条绕开。
// claimDue prefers the pinned claim whenever an expected hash is available.
func (s *Scheduler) claimDue(ctx context.Context, due DueTask, next time.Time,
	expectHash string,
) (DueExecution, error) {
	if expectHash == "" {
		return s.store.ClaimDueTrigger(ctx, due.Task.TaskID, due.NextFireAt, next, s.worker, s.leaseTTL)
	}
	claimer, ok := s.store.(AssetPinnedClaimer)
	if !ok {
		return DueExecution{}, errSchedulerNoPinnedClaim
	}
	return claimer.ClaimDueTriggerPinned(ctx, due.Task.TaskID, due.NextFireAt, next,
		s.worker, s.leaseTTL, expectHash)
}

// executionContext 返回在途执行与其续约使用的父 ctx：Start 建立的 baseCtx。
// 它**不是**到期扫描的 ctx——两者分开正是"停止领取但不打断在途"的实现。
// executionContext is the parent of in-flight executions, deliberately distinct from
// the dispatch context.
func (s *Scheduler) executionContext() context.Context {
	s.stateMu.RLock()
	base := s.baseCtx
	s.stateMu.RUnlock()
	if base == nil {
		return context.Background()
	}
	return base
}

// registeredSnapshot 复制一份注册集合，供一轮扫描使用：扫描期间不持锁，
// 也不会与 SetRegistered 的并发写构成数据竞争。
// registeredSnapshot copies the registry so a scan pass holds no lock.
func (s *Scheduler) registeredSnapshot() map[string]bool {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	out := make(map[string]bool, len(s.registry))
	for name, ok := range s.registry {
		out[name] = ok
	}
	return out
}

// executeClaimed 执行一次已领取的定时触发：派生运行时限后交给共用的执行链。
// 终态与失败类别已在 runClaimed 内落库并记 WARN/ERROR，这里只补一条 Debug 痕迹。
// executeClaimed runs one claimed trigger under the per-run deadline; the terminal row
// and its failure class are already persisted inside runClaimed.
func (s *Scheduler) executeClaimed(ctx context.Context, claimed DueExecution, spec string) error {
	runCtx, cancel := context.WithTimeout(ctx, s.runTimeout)
	defer cancel()
	if _, _, err := s.runClaimed(runCtx, claimed, spec, TriggerCron); err != nil {
		s.logger.Debug("cron 运行返回错误（终态已落账）",
			append(s.runAttrs(claimed.Task, claimed.RunID, TriggerCron), "error", err)...)
		return err
	}
	return nil
}

// runClaimed 是到点与手动补跑**共用**的唯一执行函数：执行行与租约已由调用方在
// 领取事务里写好，这里只做续约 → 授权 → 代投预写 → Run → fencing 收尾 → 投递结算。
// spec 在领取时算好传进来，执行侧不再重解析 cron；运行 ctx 的时限由调用方准备。
// runClaimed is the single execution chain shared by cron and manual triggers; the
// execution row and its lease were written by the caller's claim transaction.
func (s *Scheduler) runClaimed(ctx context.Context, claimed DueExecution, spec string, trigger Trigger,
) (req RunRequest, res RunResult, err error) {
	task := claimed.Task
	runID := claimed.RunID
	startedAt := s.now()
	_ = spec // 领取方已解析过 cron；这里保留入参只为不再解析一次
	s.logger.Info(schedulerTriggerLog, append(s.runAttrs(task, runID, trigger), "message_bytes", len(task.Message))...)

	// 执行租约的续约循环：失权 / 他处取消 / 续约超时都会取消 leaseCtx，执行侧据此
	// 停止发起新动作。abandon 只取消续约，交还租约由 FinishExecution 或 stop 负责。
	leaseCtx, stopKeeper, abandon := KeepLease(ctx, s.leases, claimed.Lease,
		s.renewInterval, s.leaseTTL, s.logger)
	defer abandon()

	// 终态写用 WithoutCancel 派生的 5s 清理 ctx：运行 ctx 已超时或被停机取消时，
	// 终态写仍必须成功（约束 11）。失败只记 ERROR，行留给回收器按租约状态收尾。
	//
	// 返回值 `fenced` 报告"终态是在仍持有执行权时写下的"：只有它为真才允许继续结算
	// 投递与收尾（F5）。失权（ErrLeaseLost）或终态没写进去都返回 false——此时本进程
	// 不再是该 Run 的合法写者，发投递或补收尾都会覆盖/顶替新属主的结果；残留交给
	// 回收器按「终态残留 / 前置投递」规则查证（FR-24-39），**不**自动重发。
	// The final write reports whether it landed while the lease was still ours; only then
	// may the delivery and the closeout proceed (F5). Anything else is left to the
	// reclaimer, which must never resend.
	finish := func(status ExecutionStatus, errMsg string) (fenced bool) {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), schedulerFinishTimeout)
		defer cancel()
		duration := s.now().Sub(startedAt)
		finishErr := s.store.FinishExecution(finishCtx, claimed.ExecutionID, claimed.Lease,
			status, duration, errMsg, s.now())
		if finishErr == nil {
			return true
		}
		s.logger.Error("执行行收尾失败", append(s.runAttrs(task, runID, trigger), "error", finishErr)...)
		if errors.Is(finishErr, ErrLeaseLost) {
			// 执行权已不属于本进程：**不得**释放别人的租约，也**不得**再以属主身份结算
			// 投递或补收尾——交回收器查证。
			return false
		}
		// 收尾没成功就主动交还执行租约：行仍是 started，回收器可按「已释放」领取清理。
		stopKeeper(finishCtx)
		return false
	}

	// 代投的运行期固定目标：在运行前确定，panic 收尾也要据此把预写行改判。
	var deliveryIntent DeliveryIntent
	deliveryPrepared := false

	// panic 兜底：运行 panic 必须吸收，并把执行行收尾为 failed（约束 11）；
	// 已预写的投递行同样收尾（运行未成功 → 跳过）。
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error(schedulerPanicLog, append(s.runAttrs(task, runID, trigger), "panic", fmt.Sprint(r))...)
			if finish(ExecutionFailed, fmt.Sprintf("运行 panic：%v", r)) {
				s.settleAndClose(ctx, task, deliveryIntent, deliveryPrepared, trigger,
					ExecutionFailed, RunResult{RunID: runID, Status: StatusFailed})
			}
			req = RunRequest{}
			res = RunResult{RunID: runID, Status: StatusFailed}
			err = fmt.Errorf("scheduler: run panic: %v", r)
		}
	}()

	// 尝试开始（准入失败也保留 last_run_at）；失败只记 WARN（图 C）。
	if markErr := s.store.MarkTaskRun(leaseCtx, task.TaskID, startedAt, ExecutionStarted); markErr != nil {
		s.logger.Warn("记录任务运行痕迹失败",
			append(s.runAttrs(task, runID, trigger), "error", markErr)...)
	}

	// 执行授权先于一切外发动作（C-32）：归属取自任务行、执行者为 scheduler、授权来源按
	// `authorizing_kind` 显式二分。任一不可满足即本轮不执行——执行行收尾 failed、
	// Reason 固定 `owner_unauthorized`，不预写投递、不发起运行。
	// Execution authorization is resolved before any outbound step; an unusable owner
	// leaves a failed execution row and never reaches delivery or the model.
	principal, authErr := s.taskPrincipal(leaseCtx, task)
	if authErr != nil {
		reason := schedulerOwnerUnauthorized
		if ownerReasonOf(authErr) == OwnerReasonAdminAuthUnavailable {
			// 管理认证不可用不是"归属失效"：单独的原因码让运维分辨该修哪一侧。
			// An unavailable admin posture is not a revoked owner; keep the codes apart.
			reason = schedulerAdminAuthUnavailable
		}
		s.logger.Error("任务执行授权不可用，本轮不发起运行",
			append(s.runAttrs(task, runID, trigger), "error", authErr, "owner_reason", ownerReasonOf(authErr))...)
		finish(ExecutionFailed, reason)
		return RunRequest{}, RunResult{RunID: runID, Status: StatusFailed, Reason: reason}, authErr
	}

	// R7：配置了系统代投的任务，运行前必须先落投递预写行并复核渠道资格；任一步失败
	// 本轮不发起运行——没有记录就不投递，也不允许"先跑完再补记录"。
	if task.DeliverTo != "" {
		intent, prepErr := s.prepareDelivery(leaseCtx, task, runID, claimed.ExecutionID)
		if prepErr != nil {
			s.logger.Error("代投前置检查失败，本轮不发起运行",
				append(s.runAttrs(task, runID, trigger), "error", prepErr)...)
			finish(ExecutionFailed, prepErr.Error())
			return RunRequest{}, RunResult{}, prepErr
		}
		deliveryIntent = intent
		deliveryPrepared = true
	}

	req = RunRequest{
		// 身份按任务归属重建（C-32）：归属方不会被"是谁在执行"改写。
		Principal: principal,
		RunID:     runID,
		Agent:     task.Agent,
		SessionID: DeriveSessionID("scheduler", "scheduler", task.Agent),
		Message:   task.Message,
		Channel:   "scheduler",
		UserID:    "scheduler",
		Trigger:   trigger,
		Budget:    s.budget,
		// 系统代投接管投递：本 Run 不向模型提供 `notify`，授权层同时固定为 none。
		SuppressNotify: deliveryPrepared,
	}
	if claimed.PinnedBundleHash != "" {
		// 领取事务已钉住版本（卡 §2.3.5.1 T1→T2 的传递）：准入按这枚凭证复核领取
		// 记录与执行租约，**不**重读当时的指针。手动补跑没有领取事务，哈希为空，
		// 因此保持既有当前指针判据。
		// The claim transaction pinned the bundle; manual reruns carry no pin.
		req.Pinned = &ClaimedBundlePin{
			ExecutionID: claimed.ExecutionID,
			BundleHash:  claimed.PinnedBundleHash,
			Lease:       claimed.Lease,
		}
	}
	res, err = s.invoker.Run(leaseCtx, req)

	status, errMsg := s.classifyRun(task, runID, trigger, res, err)
	if finish(status, errMsg) {
		s.settleAndClose(ctx, task, deliveryIntent, deliveryPrepared, trigger, status, res)
	} else {
		// 失权 / 终态未落：投递与收尾都不再以本进程名义进行，留给回收器查证。
		s.logger.Warn("执行权不在本进程，投递与收尾交给回收器",
			append(s.runAttrs(task, runID, trigger), "delivery_prepared", deliveryPrepared)...)
	}
	return req, res, err
}

// settleAndClose 结算系统代投并补齐收尾末笔（五步收尾的第 4、5 步）。只有代投的
// Run 走这条分支：同一条件下 `RunRequest.SuppressNotify` 为真，服务侧的 `finish`
// 跳过 `CompleteRunCloseout`——投递结果必须先落库，租约才能交还。Run 返回时服务侧
// keeper 只停不续（abandon），所以结算期间由这里续约：否则一次超过 TTL 的发送会让
// 租约自然过期，B1 就会把**在途**投递改判成 `unknown`，与「不重发」的结论混在一起。
// settleAndClose settles the delivery and completes the caller-side closeout. The
// scheduler renews the run lease while the send is in flight so it never expires
// mid-delivery and gets misread as an unknown outcome.
func (s *Scheduler) settleAndClose(ctx context.Context, task TaskRecord, intent DeliveryIntent,
	prepared bool, trigger Trigger, status ExecutionStatus, res RunResult,
) {
	// 没有代投：RunRequest 没设 SuppressNotify，五步收尾在服务侧已经走完。
	if !prepared || s.delivery == nil {
		return
	}
	if s.closeout == nil || s.leases == nil {
		// 装配没有收尾端口：投递照常结算，租约与引用留给回收器按终态残留清理。
		// 不在这里凭空「释放」——没有凭证的释放就是猜测。
		s.settleDelivery(ctx, task, intent, prepared, trigger, status, res)
		return
	}

	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), schedulerCloseoutTimeout)
	defer cancel()

	token, lookErr := s.closeout.LookupAdmittedRun(closeCtx, intent.RunID)
	keepCtx := closeCtx
	stopRenew := func() {}
	switch {
	case lookErr != nil:
		// Run 行不存在 = 准入被拒（没有任何行可收尾）：投递本轮自己结算；若仍有
		// pending 残留，B2 按「无 Run 的前置投递」收成 skipped。其余读取错误同样
		// 只影响收尾，不影响投递结果的如实落库。
		if !errors.Is(lookErr, ErrRunNotFound) {
			s.logger.Error("代投运行取租约失败，收尾留给回收器",
				append(s.runAttrs(task, intent.RunID, trigger), "error", lookErr)...)
		}
	case !token.IsZero() && !s.worker.IsZero() &&
		(token.OwnerWorkerID != s.worker.InstanceID || token.OwnershipEpoch != 1):
		// 回查到的 Run 租约**已不是本次准入创建的那条**：要么已属于另一个 Worker，
		// 要么被回收器以更高代次领回（可能是本进程自己的回收器——它与调度器共用同一
		// Worker 身份，`reclaimer_store.go` 的领取语句不排除自身）。准入创建的租约代次
		// 恒为 1（`run_store.go` 的 INSERT），续约不改代次，所以 `epoch != 1` 就是
		// "被领回过"的充分证据。此时发投递或补收尾都会顶替回收作用域的裁决，两项一律
		// 不做，残留交由回收器（F5 的第二道闸；`finish` 的返回值是第一道）。
		// The looked-up run lease is no longer the admission one — another worker took it
		// over, or a reclaimer (possibly this process's own) re-claimed it at a higher
		// epoch. Admission always stamps epoch 1, so anything else means "reclaimed":
		// skip both the send and the closeout and let the reclaimer decide.
		s.logger.Warn("代投运行已不归本次准入，投递与收尾留给回收器",
			append(s.runAttrs(task, intent.RunID, trigger),
				"owner", token.OwnerWorkerID, "epoch", token.OwnershipEpoch)...)
		return
	case !token.IsZero():
		var abandon func()
		keepCtx, _, abandon = KeepLease(closeCtx, s.leases, token,
			s.renewInterval, s.leaseTTL, s.logger)
		stopRenew = abandon
	}

	s.settleDelivery(keepCtx, task, intent, prepared, trigger, status, res)
	// 投递结果已落库：只停续约、不释放——最后一笔交给 CompleteRunCloseout。
	stopRenew()
	if lookErr != nil {
		return
	}
	if err := s.closeout.CompleteRunCloseout(closeCtx, intent.RunID, token, s.now()); err != nil {
		s.logger.Error("代投运行收尾末笔失败，租约与引用留给回收器",
			append(s.runAttrs(task, intent.RunID, trigger), "error", err)...)
	}
}

// nextFire 用任务的 cron 定义算出**严格晚于 from** 的下一次触发点，并返回解析后的
// spec 供执行侧复用。
// nextFire computes the next occurrence strictly after from and returns the parsed spec.
func (s *Scheduler) nextFire(task TaskRecord, from time.Time) (time.Time, string, error) {
	if s.engine == nil {
		return time.Time{}, "", errors.New("scheduler: CronEngine 未装配")
	}
	if s.cronSpec == nil {
		return time.Time{}, "", errors.New("scheduler: CronSpec is required")
	}
	spec, err := s.cronSpec(Schedule{ID: task.TaskID, Cron: task.Cron, Zone: task.Zone, Message: task.Message})
	if err != nil {
		return time.Time{}, "", err
	}
	next, err := s.engine.Next(spec, from)
	if err != nil {
		return time.Time{}, spec, err
	}
	return next, spec, nil
}

// dbNow 返回本轮扫描使用的时钟：存储提供 `DBNowReader` 时取数据库时钟（模块卡
// §2.3.4 的 `db_now`），否则退回装配时钟（轻装配与零值单测）。
// dbNow prefers the database clock so due, grace and next-fire share one time basis.
func (s *Scheduler) dbNow(ctx context.Context) (time.Time, error) {
	reader, ok := s.store.(DBNowReader)
	if !ok {
		return s.now(), nil
	}
	return reader.DBNow(ctx)
}

// taskPrincipal 按任务行重建后台执行身份（C-32）：归属恒为任务归属方，执行者固定
// `scheduler`，授权来源按 `authorizing_kind` 显式二分——**不得**按"有没有 key_id"
// 推断，**不得**把后台执行提升成管理员的"外部调用"身份。
//
//   - `admin-builtin`：走 `OwnedPrincipal(local-admin, admin-builtin, "", nil, scheduler)`；
//     直接调 `LocalAdminPrincipal()` 会把 Actor 固定成 caller，审计便分不清调度与外部请求。
//     启动期算出的管理认证姿态里 `token` 与 `local_trust` 都是**合法启用态**，只有
//     `unavailable` 拒绝——把 `local_trust` 当成"管理认证关闭"会让本机模式下的旧任务
//     集体停摆。
//   - `caller-key`：执行前**必须**按 `(caller_id, key_id)` 重读**那一把** Key 并核对
//     当前状态与 scope；端口未接线时无法证明授权仍在，一律 fail-closed——**不得**
//     借用管理员、**不得**用同一调用方的其他 Key 顶替、也**不得**跳过校验继续出网。
//
// taskPrincipal rebuilds the background principal from the task row: admin-builtin is
// refused only under an unavailable posture, and caller-key re-reads that exact key
// through the verifier port.
func (s *Scheduler) taskPrincipal(ctx context.Context, task TaskRecord) (Principal, error) {
	switch task.AuthorizingKind {
	case AuthorizingKindAdminBuiltin:
		if s.adminPostureUnavailable() {
			return Principal{}, &PrincipalError{
				Reason: OwnerReasonAdminAuthUnavailable,
				Detail: "startup admin posture is unavailable",
			}
		}
		return OwnedPrincipal(task.CallerID, AuthorizingKindAdminBuiltin, "", nil, ActorScheduler)
	case AuthorizingKindCallerKey:
		if task.AuthorizingKeyID == "" {
			return Principal{}, &PrincipalError{
				Reason: OwnerReasonKeyMissing,
				Detail: "caller-key task carries no authorizing key",
			}
		}
		if s.ownerKeys == nil {
			return Principal{}, &PrincipalError{
				Reason: OwnerReasonKeyMissing,
				Detail: "caller-key task requires the key verification port, which is not wired",
			}
		}
		scopes, verifyErr := s.ownerKeys.VerifyOwnerKey(ctx, task.CallerID, task.AuthorizingKeyID, task.Agent)
		if verifyErr != nil {
			return Principal{}, verifyErr
		}
		return OwnedPrincipal(task.CallerID, AuthorizingKindCallerKey, task.AuthorizingKeyID, scopes, ActorScheduler)
	case "":
		return Principal{}, &PrincipalError{
			Reason: OwnerReasonKindMismatch,
			Detail: "task has no authorizing source",
		}
	default:
		return Principal{}, &PrincipalError{
			Reason: OwnerReasonKindMismatch,
			Detail: "task carries an unknown authorizing kind",
		}
	}
}

// adminPostureUnavailable 只在启动期确实算出 `unavailable` 时为真。姿态源缺席或尚未
// 发布（web 面还没算出来）都按"尚未计算"处理，**不得**当成不可用：调度器先于 web 面
// 构造，把未知当不可用会让 gateway 与所有 admin-builtin 任务永久停摆。
// adminPostureUnavailable is true only for a posture that was actually published as
// unavailable; a missing or not-yet-published source is not treated as unavailable.
func (s *Scheduler) adminPostureUnavailable() bool {
	if s.adminPosture == nil {
		return false
	}
	return s.adminPosture() == AdminPostureUnavailable
}

// ownerReasonOf 取出错误携带的原因码；没有结构化的 `*PrincipalError` 时返回空串。
// ownerReasonOf extracts the structured owner reason, or "" when there is none.
func ownerReasonOf(err error) OwnerReason {
	var principalErr *PrincipalError
	if errors.As(err, &principalErr) {
		return principalErr.Reason
	}
	return ""
}

// prepareDelivery 校验代投目标并写 pending 行：投递服务未装配、UID 非法、渠道资格
// 复核失败都返回错误，由调用方终止本轮运行。
//
// executionID 是本次调度执行的 `task_executions.id`（领取事务里已写好）：它随 pending
// 行一起落库，回收器 B2 才能判"这条 Run 创建前的投递是否还有合法执行者"（FR-24-39）。
// prepareDelivery validates the target and writes the pending row together with the
// execution id that owns it; any failure aborts the run before the model is called.
func (s *Scheduler) prepareDelivery(ctx context.Context, task TaskRecord, runID string,
	executionID int64,
) (DeliveryIntent, error) {
	if s.delivery == nil {
		return DeliveryIntent{}, errors.New("任务配置了系统代投，但本实例未装配投递服务（装配错误）")
	}
	channelUID, uidErr := ParseResourceUID(task.DeliverTo)
	if uidErr != nil {
		return DeliveryIntent{}, fmt.Errorf("任务 %s 的代投目标不是合法的渠道资源 UID: %w",
			task.TaskID, uidErr)
	}
	intent := DeliveryIntent{
		RunID:       runID,
		TaskID:      task.TaskID,
		ExecutionID: executionID,
		Agent:       task.Agent,
		ChannelUID:  channelUID,
	}
	if err := s.delivery.Prepare(ctx, intent); err != nil {
		return DeliveryIntent{}, fmt.Errorf("系统代投预写失败: %w", err)
	}
	return intent, nil
}

// settleDelivery 在运行收尾后处理已预写的投递行：运行成功才发送，其余一律标跳过。
// 发送用独立的 15s ctx（不继承可能已超时的运行 ctx），结果三分如实落库与记日志；
// 未知**不**触发自动重发。
// settleDelivery delivers only after a successful run and marks everything else skipped.
// The send uses its own 15s context, and an unknown outcome never triggers a resend.
func (s *Scheduler) settleDelivery(ctx context.Context, task TaskRecord, intent DeliveryIntent,
	prepared bool, trigger Trigger, status ExecutionStatus, res RunResult,
) {
	if !prepared || s.delivery == nil {
		return
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), schedulerDeliveryTimeout)
	defer cancel()

	attrs := s.runAttrs(task, intent.RunID, trigger)
	if status != ExecutionCompleted {
		if err := s.delivery.Skip(finishCtx, intent, "运行未成功，未投递"); err != nil {
			s.logger.Error("投递跳过写库失败（记录保持 pending，启动恢复改判）",
				append(attrs, "error", err)...)
		}
		return
	}

	outcome := s.delivery.Deliver(finishCtx, intent, deliveryContent(task, res))
	attrs = append(attrs, "delivery_status", string(outcome.Status), "detail", outcome.Detail)
	switch outcome.Status {
	case DeliverySent:
		s.logger.Info("系统代投完成", attrs...)
	case DeliveryFailed:
		s.logger.Warn("系统代投失败", attrs...)
	default:
		// 未知需要人工核对：记录里保留可查证的事实，**不**盲目重发（R7）。
		s.logger.Error("系统代投结果未知（不自动重发）", attrs...)
	}
}

// deliveryContent 组装推送正文：点名 Agent 与任务，正文是模型本轮的最终回答。
// 无文本（例如达迭代上限）时给固定说明，不把空消息发出去。
// deliveryContent names the agent and task and carries the run's final text verbatim.
func deliveryContent(task TaskRecord, res RunResult) string {
	text := strings.TrimSpace(res.FinalText)
	if text == "" {
		text = "（本轮未产生文本回答）"
	}
	return fmt.Sprintf("【%s】定时任务 %s 执行完成\n\n%s", task.Agent, task.TaskID, text)
}

// classifyRun 把运行结果映射为执行行终态（约束 10 / D-18 / 约束 43）。
// classifyRun maps one run outcome onto the execution-row terminal state.
func (s *Scheduler) classifyRun(task TaskRecord, runID string, trigger Trigger, res RunResult, runErr error) (ExecutionStatus, string) {
	if runErr != nil {
		var busy *SessionBusyError
		var archived *SessionArchivedError
		switch {
		case errors.As(runErr, &busy):
			s.logger.Warn(schedulerBusyMessage, append(s.runAttrs(task, runID, trigger), "error", runErr)...)
			return ExecutionSkipped, schedulerBusyMessage
		case errors.As(runErr, &archived):
			// 不重建会话：归档互斥由 AgentService 判定（约束 40 / 43）。
			s.logger.Warn(schedulerArchivedMessage, append(s.runAttrs(task, runID, trigger), "error", runErr)...)
			return ExecutionSkipped, schedulerArchivedMessage
		default:
			s.logger.Error(schedulerFailLog, append(s.runAttrs(task, runID, trigger), "error", runErr)...)
			return ExecutionFailed, runErr.Error()
		}
	}

	switch res.Status {
	case StatusCompleted, StatusLimitReached:
		return ExecutionCompleted, ""
	case StatusFailed, StatusCancelled:
		s.logger.Error(schedulerFailLog,
			append(s.runAttrs(task, runID, trigger), "status", string(res.Status), "reason", res.Reason)...)
		return ExecutionFailed, res.Reason
	case StatusRunning:
		// 运行面不应在返回时仍是 running；按失败收尾，不掩盖异常（约束 10）。
		s.logger.Error(schedulerFailLog,
			append(s.runAttrs(task, runID, trigger), "status", string(res.Status))...)
		return ExecutionFailed, "调度运行未在时限内结束"
	default:
		s.logger.Error(schedulerFailLog,
			append(s.runAttrs(task, runID, trigger), "status", string(res.Status))...)
		return ExecutionFailed, fmt.Sprintf("未知运行状态 %q", res.Status)
	}
}

// List 返回管理读面的任务列表：每次读库 + 共用可见性过滤，不在进程内缓存任务状态
// （约束 3 / 17）；输出保持 SQL 的 task_id 升序。
// List reads the store on every call and returns the visible rows in task_id order.
func (s *Scheduler) List(ctx context.Context) ([]TaskRecord, error) {
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list tasks: %w", err)
	}
	visible := make([]TaskRecord, 0, len(rows))
	for _, row := range rows {
		if s.visible(row) {
			visible = append(visible, row)
		}
	}
	return visible, nil
}

// Executions 返回任务执行历史：先过共用可见性判定（未知 / retired / 未注册 Agent →
// fs.ErrNotExist），再把 limit 原样透传给仓储（约束 33 / 38）。
// Executions gates the history behind the shared visibility rule and forwards the limit.
func (s *Scheduler) Executions(ctx context.Context, taskID string, limit int) ([]ExecutionRecord, error) {
	if _, err := s.lookupVisible(ctx, taskID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListExecutions(ctx, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list executions of task %q: %w", taskID, err)
	}
	return rows, nil
}

// SetEnabled 翻转任务启停：按共用可见性判定 → 幂等短路 → 写库。
// US-24 起没有进程内句柄可动；到期扫描每轮读库，库值即生效（跨 Worker 一致）。
// SetEnabled flips the enabled flag after the shared visibility check; with the
// database-driven scan there is no in-process handle to swap.
func (s *Scheduler) SetEnabled(ctx context.Context, taskID string, enabled bool) error {
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("启停判定读取任务失败", "task_id", taskID, "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}
	task, ok := s.visibleRow(rows, taskID)
	if !ok {
		s.logger.Debug("任务不可见，启停请求拒绝", "task_id", taskID)
		return fmt.Errorf("scheduler: task %q: %w", taskID, fs.ErrNotExist)
	}

	// 幂等短路：目标值与库值相同即零写 SQL（不刷新 updated_at）。
	// Idempotent short-circuit: the stored value already matches the target.
	if task.Enabled == enabled {
		return nil
	}

	if err := s.store.SetTaskEnabled(ctx, taskID, enabled, s.now()); err != nil {
		// 并发下线由 SQL 的 retired_at 谓词兜底（RowsAffected()==0 → fs.ErrNotExist）。
		s.logger.Error("任务启停写库失败", "task_id", taskID, "enabled", enabled, "error", err)
		return fmt.Errorf("scheduler: set task %q enabled=%t: %w", taskID, enabled, err)
	}

	if enabled {
		s.logger.Info("定时任务已启用", "task_id", taskID, "agent", task.Agent)
	} else {
		s.logger.Info("定时任务已停用", "task_id", taskID, "agent", task.Agent)
	}
	return nil
}

// RunNow 执行手动补跑：先过共用可见性判定，再经 `StartExecution` 写执行行与租约
// （同任务已有 started 执行 → ErrTaskBusy，不排队），最后复用 runClaimed 的收尾。
// 运行 ctx 的时限由调用方准备（web handler 传 60s），调度器不另行派生。
// RunNow runs the shared execution chain under StartExecution; the caller owns the
// deadline. A started execution for the same task yields ErrTaskBusy without writing.
func (s *Scheduler) RunNow(ctx context.Context, taskID string) (RunRequest, RunResult, error) {
	task, err := s.lookupVisible(ctx, taskID)
	if err != nil {
		return RunRequest{}, RunResult{}, err
	}
	if s.leaseless() {
		s.logger.Error("手动补跑缺少执行权装配，拒绝执行", "task_id", taskID)
		return RunRequest{}, RunResult{}, fmt.Errorf("scheduler: task %q: %w", taskID, errSchedulerNoLeases)
	}

	now := s.now()
	rec := ExecutionRecord{
		TaskID:    task.TaskID,
		RunID:     NewRunID("scheduler", now, RunIDSuffix()),
		Status:    ExecutionStarted,
		Trigger:   TriggerManual,
		StartedAt: now,
	}
	execID, token, startErr := s.store.StartExecution(ctx, rec, s.worker, s.leaseTTL)
	if startErr != nil {
		if errors.Is(startErr, ErrTaskBusy) {
			return RunRequest{}, RunResult{}, fmt.Errorf("scheduler: task %q: %w", taskID, ErrTaskBusy)
		}
		s.logger.Error("执行行写入失败，本轮不发起运行",
			append(s.runAttrs(task, rec.RunID, TriggerManual), "error", startErr)...)
		return RunRequest{}, RunResult{}, startErr
	}
	claimed := DueExecution{
		Task:        task,
		ExecutionID: execID,
		Lease:       token,
		RunID:       rec.RunID,
	}
	// manual 没有计划时刻（`(task_id, planned_at)` 去重只服务 cron），PlannedAt 保持零值。
	return s.runClaimed(ctx, claimed, "", TriggerManual)
}

// visible 是 List / Executions / SetEnabled / RunNow / 到期扫描共用的可见性判定：
// retired 行与构造期未注册 Agent 的任务一律隐藏（§3.1.2）。
// visible is the shared visibility rule: retired rows and unregistered agents are hidden.
func (s *Scheduler) visible(row TaskRecord) bool {
	// 与管理面的 SetRegistered 并发时必须持锁读注册集合（同一张 map 的增删）。
	// The registry is mutated by SetRegistered, so the read must take the lock.
	s.stateMu.RLock()
	registered := s.registry[row.Agent]
	s.stateMu.RUnlock()
	return row.RetiredAt.IsZero() && registered
}

// visibleRow 在同一次读库结果里定位可见任务；不可见返回 false。
// visibleRow locates a visible task inside one ListTasks result.
func (s *Scheduler) visibleRow(rows []TaskRecord, taskID string) (TaskRecord, bool) {
	for _, row := range rows {
		if row.TaskID == taskID && s.visible(row) {
			return row, true
		}
	}
	return TaskRecord{}, false
}

// lookupVisible 读一次库并返回可见任务；不可见一律包装 fs.ErrNotExist（web 404）。
// lookupVisible reads the store once and wraps fs.ErrNotExist for hidden tasks.
func (s *Scheduler) lookupVisible(ctx context.Context, taskID string) (TaskRecord, error) {
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("读取任务失败", "task_id", taskID, "error", err)
		return TaskRecord{}, fmt.Errorf("scheduler: list tasks: %w", err)
	}
	if task, ok := s.visibleRow(rows, taskID); ok {
		return task, nil
	}
	s.logger.Debug("任务不可见，请求拒绝", "task_id", taskID)
	return TaskRecord{}, fmt.Errorf("scheduler: task %q: %w", taskID, fs.ErrNotExist)
}

// CheckOwnership 是写入门**落盘之前**的归属预检（约束 30 / 42）：读一次 ListTasks，
// 任一候选 id 在库中归属于**其他** agent_name（含 retired 行）→ ErrTaskOwnerConflict；
// 无行或同 owner 一律放行（同 Agent 的 retired 行由随后的 ReconcileAgent 复活）。
// 只读、不取 stateMu（锁序约束 39）、不写库、不动引擎——最终一致性由 ApplyAgentTasks
// 的归属守卫兜底。
// CheckOwnership runs the pre-write ownership check: any candidate id owned by another
// agent (retired or not) is a conflict. It only reads, takes no state lock, and leaves
// the transactional guard as the final backstop.
func (s *Scheduler) CheckOwnership(ctx context.Context, agent string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("归属预检读取任务失败", "agent", agent, "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}
	owners := make(map[string]string, len(rows))
	for _, row := range rows {
		owners[row.TaskID] = row.Agent
	}
	for _, id := range taskIDs {
		if owner, ok := owners[id]; ok && owner != agent {
			s.logger.Warn("任务 id 归属其他 Agent，拒绝写入", "task_id", id, "agent", agent, "owner", owner)
			return fmt.Errorf("scheduler: 任务 id %s 属于 Agent %s: %w", id, owner, ErrTaskOwnerConflict)
		}
	}
	return nil
}

// runAttrs 返回约束 36 的固定四字段。
// runAttrs renders the four fixed log fields of constraint 36.
func (s *Scheduler) runAttrs(task TaskRecord, runID string, trigger Trigger) []any {
	return []any{
		"task_id", task.TaskID,
		"agent", task.Agent,
		"run_id", runID,
		"trigger", string(trigger),
	}
}

// registeredAgentNamesLocked 返回已注册 Agent 名（排序保证对账顺序稳定）。
// **调用方必须持有 stateMu**（Start 在写锁内调用）；不持锁的路径一律走
// registeredSnapshot + sortedRegisteredNames，直接遍历 s.registry 会与 SetRegistered
// 的并发增删构成数据竞争。
// registeredAgentNamesLocked returns the registered agent names in sorted order; the
// caller must hold stateMu.
func (s *Scheduler) registeredAgentNamesLocked() []string {
	return sortedRegisteredNames(s.registry)
}

// sortedRegisteredNames 从注册集合（或其快照）提取已注册名字并排序，供持锁路径与
// 快照路径共用。
// sortedRegisteredNames extracts the registered names from a registry or its snapshot.
func sortedRegisteredNames(registry map[string]bool) []string {
	names := make([]string, 0, len(registry))
	for name, ok := range registry {
		if ok {
			names = append(names, name)
		}
	}
	sortStrings(names)
	return names
}

// sortStrings 是只依赖标准库的升序排序（避免为一行排序引入额外导入面）。
// sortStrings sorts in place with a minimal standard-library surface.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// commitAgentLocked 以单事务提交一个 Agent 的完整 upsert / retire 集合，返回提交后的
// 全量任务行与受影响任务；失败时库与旧定义完整保留（HIGH-2 / 约束 41）。
// US-24 起没有句柄可切换：新任务的 `next_fire_at` 为 NULL，由下一轮到期扫描初始化。
// commitAgentLocked applies one agent's full upsert/retire set in a single transaction;
// new rows start with a NULL next_fire_at and are initialized by the next scan.
func (s *Scheduler) commitAgentLocked(ctx context.Context, agent string, schedules []Schedule, rows []TaskRecord) ([]TaskRecord, map[string]bool, error) {
	now := s.now()
	derived := make(map[string]bool, len(schedules))
	upserts := make([]TaskRecord, 0, len(schedules))
	for _, schedule := range schedules {
		derived[schedule.ID] = true
		upserts = append(upserts, TaskRecord{
			TaskID: schedule.ID,
			Agent:  agent,
			// AGENT.md 声明的任务由部署者（内置管理员）创建：归属与授权来源在定义
			// 对齐时就固定为 `local-admin` + `admin-builtin`（C-32），执行期不得改写。
			// Definition-derived tasks are admin-owned; ownership is fixed at apply time.
			CallerID:        LocalAdminCallerID,
			AuthorizingKind: AuthorizingKindAdminBuiltin,
			Cron:            schedule.Cron,
			Zone:            schedule.Zone,
			Message:         schedule.Message,
			// 代投目标随定义落库：它是任务在 AGENT.md 里声明的一部分（R7），
			// 不随定义指纹参与变更提示，但**必须**写入库列——漏写会让每次对账
			// 把已登记的目标清空，任务照常运行却再也不投递。
			DeliverTo:      schedule.DeliverTo,
			DefinitionHash: definitionHash(agent, schedule),
		})
	}
	retireIDs := make([]string, 0)
	oldHash := make(map[string]string, len(rows))
	affected := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.Agent != agent {
			continue
		}
		affected[row.TaskID] = true
		oldHash[row.TaskID] = row.DefinitionHash
		if row.RetiredAt.IsZero() && !derived[row.TaskID] {
			retireIDs = append(retireIDs, row.TaskID)
		}
	}
	for _, upsert := range upserts {
		affected[upsert.TaskID] = true
	}

	if err := s.store.ApplyAgentTasks(ctx, agent, upserts, retireIDs, now); err != nil {
		return rows, nil, err
	}

	// 定义变化只记 INFO，不翻 enabled（注 14）。
	for _, upsert := range upserts {
		if old, ok := oldHash[upsert.TaskID]; ok && old != "" && old != upsert.DefinitionHash {
			s.logger.Info("任务定义已更新，enabled 保持库值",
				"task_id", upsert.TaskID, "agent", agent)
		}
	}

	fresh, err := s.store.ListTasks(ctx)
	if err != nil {
		return rows, nil, fmt.Errorf("scheduler: list tasks after reconcile: %w", err)
	}
	return fresh, affected, nil
}

// ReconcileAgent 在 chat / tool 落盘后把单个 Agent 的定义同步到库（图 E 尾段）。
// 未启动（chat / tool）→ DEBUG 后返回：文件已落盘，不落库，下次启动对账生效；
// 已启动 → 单事务提交。US-24 起提交即生效：到期扫描每轮读库，无需注册步骤。
// ReconcileAgent syncs one agent's definitions after a successful file write; it is a
// no-op before Start, and a committed transaction takes effect on the next scan.
func (s *Scheduler) ReconcileAgent(ctx context.Context, agent string, tasks []Schedule) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	// 定义写权缺失即失败关闭（US-25C FR-25C-13）：静默 no-op 会让装配缺陷不可见，
	// 照写则会把控制面已发布的共享定义盖回本机旧种子。放在启动判定**之前**——这条
	// 路径的存在本身就说明有人以为本进程能写定义列。
	// A worker refuses the call outright, before the started check.
	if !s.definitionAuthority {
		s.logger.Error("reconcile refused: no definition authority", "agent", agent)
		return errSchedulerNoDefinitionAuthority
	}

	if !s.started || s.stopped {
		s.logger.Debug("调度器未启动，定义不落库", "agent", agent)
		return nil
	}

	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("运行期对账读取任务失败", "agent", agent, "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}
	if _, _, err := s.commitAgentLocked(ctx, agent, tasks, rows); err != nil {
		s.logger.Error("运行期对账失败，保留旧定义", "agent", agent, "error", err)
		return fmt.Errorf("scheduler: reconcile agent %q: %w", agent, err)
	}
	return nil
}

// SetRegistered 在运行期增减调度器的 Agent 注册集合（颗粒度文档 §3.1.24 第 5 行 /
// 约束 39）：注册集合不再构造期封死。加入只放开准入——任务定义经 ReconcileAgent 落库；
// 移除则退休该 Agent 全部未退休任务（只置 retired_at、不删行，与启动对账同口径），
// 库读 / 写失败如实上抛，由装配层记 WARN 保留注册表撤销结果。
// SetRegistered adds or removes an agent at run time. Adding only opens admission—
// definitions still go through ReconcileAgent. Removing retires the agent's live tasks.
func (s *Scheduler) SetRegistered(agent string, registered bool) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if registered {
		s.registry[agent] = true
		return nil
	}
	if _, ok := s.registry[agent]; !ok {
		return nil
	}
	// 移出注册集合会退休该 Agent 在共享库里的全部未退休任务（下方 ApplyAgentTasks）
	// ——Worker 上必须失败关闭，且**不改**本进程内存集合，避免"注册已撤、库行还在"
	// 的半截状态；运行期的集合替换走 ReplaceEntries（纯内存、不退休），见 C18。
	// Removal retires shared rows, so a worker refuses it before touching memory.
	if !s.definitionAuthority {
		s.logger.Error("unregister refused: no definition authority", "agent", agent)
		return errSchedulerNoDefinitionAuthority
	}
	delete(s.registry, agent)
	if !s.started || s.stopped {
		return nil
	}

	ctx := s.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.store.ListTasks(ctx)
	if err != nil {
		s.logger.Error("移出注册集合时读取任务失败", "agent", agent, "error", err)
		return fmt.Errorf("scheduler: list tasks: %w", err)
	}
	var ids []string
	for _, row := range rows {
		if row.Agent == agent && row.RetiredAt.IsZero() {
			ids = append(ids, row.TaskID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if err := s.store.ApplyAgentTasks(ctx, agent, nil, ids, s.now()); err != nil {
		s.logger.Error("移出注册集合时退休任务失败", "agent", agent, "error", err)
		return fmt.Errorf("scheduler: retire agent %q: %w", agent, err)
	}
	s.logger.Info("Agent 已移出调度注册集合，任务退休", "agent", agent, "tasks", len(ids))
	return nil
}

// warnForeignAgentsLocked 报告"库里有未退休任务、但本进程工作区没有该 Agent"的读数。
//
// 为什么**不**在这里退休（F7）：多 Worker 共用一张 `scheduled_tasks` 时，另一个 Worker
// 工作区里的 Agent 对本进程而言就是"缺席"——按缺席退休会静默停掉别人的定时任务。
// 本进程能**证明**的移除只有运行期的 `SetRegistered(false)`（该 Agent 确实在本进程注册
// 过），那条路径仍然退休。目录被整只删除的单机场景同样是"不可证明"，交运维处理；
// 重新装回同名 Agent 时对账会把 `retired_at` 清空，两边都不会丢数据。
// Foreign agents are reported, never retired: absence from this workspace does not prove
// removal, and retiring them would silently stop another worker's schedules.
func (s *Scheduler) warnForeignAgentsLocked(rows []TaskRecord) {
	counts := make(map[string]int)
	for _, row := range rows {
		if _, known := s.definitions[row.Agent]; known || !row.RetiredAt.IsZero() {
			continue
		}
		counts[row.Agent]++
	}
	if len(counts) == 0 {
		return
	}
	agents := make([]string, 0, len(counts))
	for agent := range counts {
		agents = append(agents, agent)
	}
	sortStrings(agents)
	for _, agent := range agents {
		s.logger.Info("库中存在本进程工作区没有的 Agent 任务，保留给持有方",
			"agent", agent, "tasks", counts[agent])
	}
}

// warnMissedTriggersLocked 枚举停机窗口内错过的触发点：只 WARN、不补跑（约束 18）。
// 它与到期扫描的错失分支并存：这里服务可观测性，那里负责落 skipped 行并推进。
// warnMissedTriggersLocked warns about trigger points missed while the process was down;
// the scan's missed branch is what persists the skipped row and advances.
func (s *Scheduler) warnMissedTriggersLocked(rows []TaskRecord, now time.Time) {
	for _, row := range rows {
		if !row.Enabled || !row.RetiredAt.IsZero() || !s.registry[row.Agent] || row.LastRunAt.IsZero() {
			continue
		}
		schedule := Schedule{ID: row.TaskID, Cron: row.Cron, Zone: row.Zone, Message: row.Message}
		if s.cronSpec == nil {
			continue
		}
		spec, err := s.cronSpec(schedule)
		if err != nil {
			continue
		}
		missed := s.engine.Occurrences(spec, row.LastRunAt, now, schedulerMissedOccurrences)
		if len(missed) == 0 {
			continue
		}
		// 枚举达到上限即说明真实错过数至少为上限，日志必须明示「≥1000」（约束 38）。
		// Hitting the cap means the true count is at least the cap; say so explicitly.
		s.logger.Warn("错过调度触发点",
			"task_id", row.TaskID, "agent", row.Agent,
			"missed_count", len(missed),
			"missed_at_least", len(missed) >= schedulerMissedOccurrences,
			"last_missed_at", missed[len(missed)-1].UTC().Format(time.RFC3339))
	}
}

// definitionHash 按 agent、id、cron、zone、message 顺序以换行连接后取 sha256（注 14）。
// `deliver_to` **不在**这五字段指纹内：它是投放目标而不是任务定义，变更由库列与
// AGENT.md 承载，不触发"任务定义已更新"提示。
// definitionHash computes the documented five-field fingerprint; deliver_to stays out
// because it is a delivery target rather than part of the task definition.
func definitionHash(agent string, schedule Schedule) string {
	sum := sha256.Sum256([]byte(agent + "\n" + schedule.ID + "\n" + schedule.Cron + "\n" + schedule.Zone + "\n" + schedule.Message))
	return hex.EncodeToString(sum[:])
}
